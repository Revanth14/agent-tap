package telemetry

import (
	"cmp"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"math"
	"os"
	"path/filepath"
	"slices"
	"sync"
	"time"

	_ "modernc.org/sqlite"
)

// DefaultRingCapacity bounds how many spans, and separately how many tool
// invocations, the store keeps in memory per session.
const DefaultRingCapacity = 1000

// MaxLiveSessions bounds how many sessions the store keeps in memory. A new
// session beyond it evicts the least recently written session, with its
// agents, spans and tools, from memory; SQLite keeps them.
const MaxLiveSessions = 16

// Rows hold the full record as JSON so the schema can evolve without
// migrations; the extra columns exist for ad-hoc querying, and started_at
// (Unix milliseconds) for retention.
const schemaDDL = `
CREATE TABLE IF NOT EXISTS sessions (id TEXT PRIMARY KEY, started_at INTEGER NOT NULL, data TEXT NOT NULL);
CREATE TABLE IF NOT EXISTS agents (id TEXT PRIMARY KEY, session_id TEXT NOT NULL, started_at INTEGER NOT NULL, data TEXT NOT NULL);
CREATE TABLE IF NOT EXISTS spans (id TEXT PRIMARY KEY, session_id TEXT NOT NULL, agent_id TEXT NOT NULL, started_at INTEGER NOT NULL, data TEXT NOT NULL);
CREATE TABLE IF NOT EXISTS tools (tool_use_id TEXT PRIMARY KEY, span_id TEXT NOT NULL, started_at INTEGER NOT NULL, data TEXT NOT NULL);
CREATE TABLE IF NOT EXISTS usage_observations (plan_window TEXT NOT NULL, started_at INTEGER NOT NULL, data TEXT NOT NULL);
`

var retainedTables = []string{"sessions", "agents", "spans", "tools", "usage_observations"}

var (
	errEmptyID       = errors.New("telemetry: empty id")
	errToolNoSession = errors.New("telemetry: tool references no known span or agent")
)

// Store is the live state, grouped by session: each live session holds its
// agents and its most recent spans and tool invocations in fixed-size rings.
// Every upsert is written through to SQLite when a database is open.
//
// A record's session is taken from its SessionID ("" groups records not yet
// tied to a session), which must not change between upserts of the same
// record. A tool invocation joins the session of its span, or failing that
// of its agent, so that span or agent must be upserted first.
//
// Upserts copy the slices and pointers of their argument, so callers may keep
// mutating a span they are building. Callers that also broadcast must upsert
// before calling Hub.Broadcast; the hub relies on that order to avoid gaps
// between a client's snapshot and its live events.
type Store struct {
	// mu is held across the memory update and the SQLite write so the
	// database sees upserts of the same record in the same order as memory.
	mu        sync.RWMutex
	db        *sql.DB // nil when persistence is disabled
	capacity  int
	live      map[string]*liveSession // by session ID
	writes    uint64                  // write counter, for least-recently-written eviction
	planUsage *PlanUsage              // Latest response's values, in memory
	usage     map[string]*windowUsage // By plan window name; changes are persisted
}

type liveSession struct {
	session   *Session // nil until UpsertSession
	agents    map[string]AgentNode
	spans     *ring[InferenceSpan]
	tools     *ring[ToolInvocation]
	writtenAt uint64
}

// OpenStore creates a store that keeps up to capacity spans and tool
// invocations per session in memory (DefaultRingCapacity if capacity <= 0).
// An empty path disables persistence; otherwise the SQLite database at path
// is created if needed.
func OpenStore(path string, capacity int) (*Store, error) {
	if capacity <= 0 {
		capacity = DefaultRingCapacity
	}
	s := &Store{capacity: capacity, live: make(map[string]*liveSession), usage: make(map[string]*windowUsage)}
	if path == "" {
		return s, nil
	}

	// The database holds prompts and tool arguments: keep its directory private.
	if err := os.MkdirAll(filepath.Dir(path), 0o700); err != nil {
		return nil, fmt.Errorf("telemetry: create db dir: %w", err)
	}
	db, err := sql.Open("sqlite", path+"?_pragma=busy_timeout(5000)&_pragma=journal_mode(WAL)")
	if err != nil {
		return nil, fmt.Errorf("telemetry: open db: %w", err)
	}
	db.SetMaxOpenConns(1)
	if _, err := db.Exec(schemaDDL); err != nil {
		db.Close()
		return nil, fmt.Errorf("telemetry: create schema: %w", err)
	}
	s.db = db
	return s, nil
}

// Prune deletes persisted rows that started before cutoff, in every table,
// and returns how many it deleted. It leaves memory alone: the live rings
// hold only recent records anyway.
func (s *Store) Prune(cutoff time.Time) (int64, error) {
	if s.db == nil {
		return 0, nil
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	var total int64
	for _, table := range retainedTables {
		res, err := s.db.Exec(`DELETE FROM `+table+` WHERE started_at < ?`, cutoff.UnixMilli())
		if err != nil {
			return total, fmt.Errorf("telemetry: prune %s: %w", table, err)
		}
		n, _ := res.RowsAffected()
		total += n
	}
	return total, nil
}

// Load reads the most recently updated sessions (up to MaxLiveSessions)
// back into memory with their agents and their most recent spans and tools,
// and returns the resulting snapshot. Spans that were still in flight when
// the daemon stopped become errors ("daemon_restarted"), agents that were
// streaming become idle, and tools whose arguments those spans were still
// streaming fail, all in memory and in the database. Plan windows get their
// last value and current changes back (see loadUsage).
func (s *Store) Load() (StateSnapshot, error) {
	if s.db == nil {
		return s.Snapshot(), nil
	}
	s.mu.Lock()
	err := s.load()
	s.mu.Unlock()
	if err != nil {
		return StateSnapshot{}, fmt.Errorf("telemetry: load: %w", err)
	}
	return s.Snapshot(), nil
}

// load does Load's work. Callers hold s.mu.
func (s *Store) load() error {
	sessions, err := queryRows[Session](s.db,
		`SELECT data FROM sessions ORDER BY json_extract(data, '$.updated_at') DESC LIMIT ?`, MaxLiveSessions)
	if err != nil {
		return err
	}
	now := time.Now()
	for i := len(sessions) - 1; i >= 0; i-- { // Oldest first, so the newest is most recently written.
		sess := sessions[i]
		s.liveSession(sess.ID).session = &sess

		agents, err := queryRows[AgentNode](s.db, `SELECT data FROM agents WHERE session_id = ?`, sess.ID)
		if err != nil {
			return err
		}
		for _, a := range agents {
			if a.Status == "streaming" || a.Status == "running_tool" {
				a.Status, a.ActiveTool = "idle", ""
				if err := s.persist(`UPDATE agents SET data = ? WHERE id = ?`, a, a.ID); err != nil {
					return err
				}
			}
			s.liveSession(sess.ID).agents[a.ID] = a
		}

		spans, err := queryRows[InferenceSpan](s.db, `SELECT data FROM (SELECT data, started_at FROM spans
			WHERE session_id = ? ORDER BY started_at DESC LIMIT ?) ORDER BY started_at`, sess.ID, s.capacity)
		if err != nil {
			return err
		}
		interrupted := map[string]bool{}
		for _, sp := range spans {
			if sp.CompletedAt == nil {
				sp.Status, sp.StopReason, sp.CompletedAt = "error", "daemon_restarted", &now
				if err := s.persist(`UPDATE spans SET data = ? WHERE id = ?`, sp, sp.ID); err != nil {
					return err
				}
				interrupted[sp.ID] = true
			}
			s.liveSession(sess.ID).spans.put(sp.ID, sp)
		}

		tools, err := queryRows[ToolInvocation](s.db, `SELECT data FROM (SELECT t.data, t.started_at FROM tools t
			JOIN spans sp ON sp.id = t.span_id WHERE sp.session_id = ? ORDER BY t.started_at DESC LIMIT ?) ORDER BY started_at`,
			sess.ID, s.capacity)
		if err != nil {
			return err
		}
		for _, t := range tools {
			// Arguments cut off mid-stream never reach Claude Code as a
			// tool call, so the tool can never run.
			if interrupted[t.SpanID] && t.Status == "streaming_args" {
				t.Status = "failed"
				if err := s.persist(`UPDATE tools SET data = ? WHERE tool_use_id = ?`, t, t.ToolUseID); err != nil {
					return err
				}
			}
			s.liveSession(sess.ID).tools.put(t.ToolUseID, t)
		}
	}
	return s.loadUsage()
}

// loadUsage restores each plan window's last value and its changes in the
// current plan window, so the burn rate survives a restart. When the last
// value was confirmed is not known, so the next rise is Unseen. Callers hold
// s.mu.
func (s *Store) loadUsage() error {
	rows, err := s.db.Query(`SELECT DISTINCT plan_window FROM usage_observations`)
	if err != nil {
		return err
	}
	var names []string
	for rows.Next() {
		var name string
		if err := rows.Scan(&name); err != nil {
			rows.Close()
			return err
		}
		names = append(names, name)
	}
	rows.Close()
	if err := rows.Err(); err != nil {
		return err
	}
	for _, name := range names {
		history, err := queryRows[UsageObservation](s.db, `SELECT data FROM usage_observations
			WHERE plan_window = ?1 AND json_extract(data, '$.resets_at') IS (SELECT json_extract(data, '$.resets_at')
				FROM usage_observations WHERE plan_window = ?1 ORDER BY started_at DESC, rowid DESC LIMIT 1)
			ORDER BY started_at, rowid`, name)
		if err != nil {
			return err
		}
		if len(history) == 0 {
			continue
		}
		last := history[len(history)-1]
		s.usage[name] = &windowUsage{
			last:    UsageWindow{Name: name, Utilization: last.Utilization, ResetsAt: last.ResetsAt, Status: last.Status},
			known:   true,
			history: history,
		}
	}
	return nil
}

// queryRows decodes the JSON data column of every row the query returns.
func queryRows[T any](db *sql.DB, query string, args ...any) ([]T, error) {
	rows, err := db.Query(query, args...)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []T
	for rows.Next() {
		var data string
		if err := rows.Scan(&data); err != nil {
			return nil, err
		}
		var v T
		if err := json.Unmarshal([]byte(data), &v); err != nil {
			return nil, err
		}
		out = append(out, v)
	}
	return out, rows.Err()
}

// Close closes the database, if any.
func (s *Store) Close() error {
	if s.db == nil {
		return nil
	}
	return s.db.Close()
}

// UpsertSession inserts or replaces a session.
func (s *Store) UpsertSession(sess Session) error {
	if sess.ID == "" {
		return errEmptyID
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	s.liveSession(sess.ID).session = &sess
	return s.persist(`INSERT INTO sessions (data, id, started_at) VALUES (?, ?, ?)
		ON CONFLICT(id) DO UPDATE SET started_at = excluded.started_at, data = excluded.data`,
		sess, sess.ID, sess.StartedAt.UnixMilli())
}

// UpsertAgent inserts or replaces an agent.
func (s *Store) UpsertAgent(a AgentNode) error {
	if a.ID == "" {
		return errEmptyID
	}
	a.CompletedAt = cloneTime(a.CompletedAt)
	s.mu.Lock()
	defer s.mu.Unlock()
	s.liveSession(a.SessionID).agents[a.ID] = a
	return s.persist(`INSERT INTO agents (data, id, session_id, started_at) VALUES (?, ?, ?, ?)
		ON CONFLICT(id) DO UPDATE SET session_id = excluded.session_id, started_at = excluded.started_at, data = excluded.data`,
		a, a.ID, a.SessionID, a.StartedAt.UnixMilli())
}

// UpsertSpan inserts or replaces a span, evicting its session's oldest span
// when the session's ring is full.
func (s *Store) UpsertSpan(sp InferenceSpan) error {
	if sp.ID == "" {
		return errEmptyID
	}
	sp.FirstTokenAt = cloneTime(sp.FirstTokenAt)
	sp.CompletedAt = cloneTime(sp.CompletedAt)
	sp.ToolsOffered = slices.Clone(sp.ToolsOffered)
	sp.ToolsInvoked = slices.Clone(sp.ToolsInvoked)
	sp.InjectedIDEBlocks = slices.Clone(sp.InjectedIDEBlocks)
	s.mu.Lock()
	defer s.mu.Unlock()
	s.liveSession(sp.SessionID).spans.put(sp.ID, sp)
	return s.persist(`INSERT INTO spans (data, id, session_id, agent_id, started_at) VALUES (?, ?, ?, ?, ?)
		ON CONFLICT(id) DO UPDATE SET session_id = excluded.session_id, agent_id = excluded.agent_id, started_at = excluded.started_at, data = excluded.data`,
		sp, sp.ID, sp.SessionID, sp.AgentID, sp.StartedAt.UnixMilli())
}

// UpsertTool inserts or replaces a tool invocation in the session of its span
// or agent, evicting that session's oldest tool when its ring is full.
func (s *Store) UpsertTool(t ToolInvocation) error {
	if t.ToolUseID == "" {
		return errEmptyID
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	sessionID, ok := s.toolSessionID(t)
	if !ok {
		return fmt.Errorf("%w: tool %s, span %q, agent %q", errToolNoSession, t.ToolUseID, t.SpanID, t.AgentID)
	}
	s.liveSession(sessionID).tools.put(t.ToolUseID, t)
	return s.persist(`INSERT INTO tools (data, tool_use_id, span_id, started_at) VALUES (?, ?, ?, ?)
		ON CONFLICT(tool_use_id) DO UPDATE SET span_id = excluded.span_id, started_at = excluded.started_at, data = excluded.data`,
		t, t.ToolUseID, t.SpanID, t.StartedAt.UnixMilli())
}

// RecordPlanUsage applies the plan usage a response reported, from the span
// spanID of session sessionID (either may be empty), and returns the latest
// plan usage. A window whose value changed gets a UsageObservation, bucketed
// from the spans in flight since its previous value was last confirmed, so
// record the response's span before its usage. A response observed before one
// already applied changes nothing: responses can be processed out of order.
func (s *Store) RecordPlanUsage(u PlanUsage, sessionID, spanID string) (PlanUsage, error) {
	u.Windows = slices.Clone(u.Windows)
	for i := range u.Windows {
		u.Windows[i].ResetsAt = cloneTime(u.Windows[i].ResetsAt)
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.planUsage == nil || !u.ObservedAt.Before(s.planUsage.ObservedAt) {
		s.planUsage = &u
	}
	latest := *s.planUsage

	at := u.ObservedAt
	for _, w := range u.Windows {
		st := s.usage[w.Name]
		if st == nil {
			st = &windowUsage{}
			s.usage[w.Name] = st
		}
		if st.confirmedAt != nil && at.Before(*st.confirmedAt) {
			continue
		}
		if st.known && st.last.Utilization == w.Utilization && sameTime(st.last.ResetsAt, w.ResetsAt) && st.last.Status == w.Status {
			st.confirmedAt = &at
			continue
		}
		obs := UsageObservation{
			Window: w.Name, Utilization: w.Utilization, ResetsAt: w.ResetsAt, Status: w.Status,
			ObservedAt: at, Since: st.confirmedAt, SessionID: sessionID, SpanID: spanID,
		}
		newWindow := !sameTime(st.last.ResetsAt, w.ResetsAt)
		if st.known {
			prev := st.last.Utilization
			if newWindow {
				prev = 0
			}
			obs.Rise = math.Round((w.Utilization-prev)*1e4) / 1e4 // Drop float noise: 0.46-0.45 is 0.010000000000000009
		}
		obs.Bucket, obs.Sessions = attribute(s.allSpans(), obs.Since, at)
		if err := s.persist(`INSERT INTO usage_observations (data, plan_window, started_at) VALUES (?, ?, ?)`,
			obs, obs.Window, at.UnixMilli()); err != nil {
			return latest, err
		}
		if newWindow {
			st.history = nil
		}
		st.history = append(st.history, obs)
		st.last, st.known, st.confirmedAt = w, true, &at
	}
	return latest, nil
}

// UsageHistory returns the changes of plan window window (e.g. "5h") in its
// current plan window, oldest first.
func (s *Store) UsageHistory(window string) []UsageObservation {
	s.mu.RLock()
	defer s.mu.RUnlock()
	if st := s.usage[window]; st != nil {
		return slices.Clone(st.history)
	}
	return nil
}

// StatusLine is the one-line plan and context summary for sessionID's status
// bar (see statusLine). An unknown session leaves out the context part.
func (s *Store) StatusLine(sessionID string, now time.Time) string {
	s.mu.RLock()
	defer s.mu.RUnlock()
	var burns []Burn
	if s.planUsage != nil {
		for _, w := range s.planUsage.Windows {
			var history []UsageObservation
			if st := s.usage[w.Name]; st != nil {
				history = st.history
			}
			burns = append(burns, burn(w, history, now))
		}
	}
	var sess *Session
	if ls, ok := s.live[sessionID]; ok {
		sess = ls.session
	}
	return statusLine(burns, sess, now)
}

// allSpans returns every live span. Callers hold s.mu.
func (s *Store) allSpans() []InferenceSpan {
	var out []InferenceSpan
	for _, ls := range s.live {
		out = append(out, ls.spans.items()...)
	}
	return out
}

// toolSessionID finds the live session that already holds t, else the one
// holding its span, else its agent's. Callers hold s.mu.
func (s *Store) toolSessionID(t ToolInvocation) (string, bool) {
	for id, ls := range s.live {
		if ls.tools.has(t.ToolUseID) {
			return id, true
		}
	}
	for id, ls := range s.live {
		if ls.spans.has(t.SpanID) {
			return id, true
		}
	}
	for id, ls := range s.live {
		if _, ok := ls.agents[t.AgentID]; ok {
			return id, true
		}
	}
	return "", false
}

// liveSession returns the live state for sessionID, marking it most recently
// written. Creating it may evict the least recently written session. Callers
// hold s.mu.
func (s *Store) liveSession(sessionID string) *liveSession {
	s.writes++
	if ls, ok := s.live[sessionID]; ok {
		ls.writtenAt = s.writes
		return ls
	}
	if len(s.live) >= MaxLiveSessions {
		var oldestID string
		var oldest *liveSession
		for id, ls := range s.live {
			if oldest == nil || ls.writtenAt < oldest.writtenAt {
				oldestID, oldest = id, ls
			}
		}
		delete(s.live, oldestID)
	}
	ls := &liveSession{
		agents:    make(map[string]AgentNode),
		spans:     newRing[InferenceSpan](s.capacity),
		tools:     newRing[ToolInvocation](s.capacity),
		writtenAt: s.writes,
	}
	s.live[sessionID] = ls
	return ls
}

// persist writes v as JSON, bound as the first query argument, followed by
// keys. Callers hold s.mu.
func (s *Store) persist(query string, v any, keys ...any) error {
	if s.db == nil {
		return nil
	}
	data, err := json.Marshal(v)
	if err != nil {
		return fmt.Errorf("telemetry: encode: %w", err)
	}
	if _, err := s.db.Exec(query, append([]any{string(data)}, keys...)...); err != nil {
		return fmt.Errorf("telemetry: persist: %w", err)
	}
	return nil
}

// Snapshot returns the state of all live sessions, each slice ordered by
// start time, plus the latest plan usage (nil until first set). Slices are
// never nil, so they encode as JSON arrays.
func (s *Store) Snapshot() StateSnapshot {
	s.mu.RLock()
	defer s.mu.RUnlock()

	snap := StateSnapshot{
		Sessions: []Session{},
		Agents:   []AgentNode{},
		Spans:    []InferenceSpan{},
		Tools:    []ToolInvocation{},
	}
	for _, ls := range s.live {
		if ls.session != nil {
			snap.Sessions = append(snap.Sessions, *ls.session)
		}
		for _, a := range ls.agents {
			snap.Agents = append(snap.Agents, a)
		}
		snap.Spans = append(snap.Spans, ls.spans.items()...)
		snap.Tools = append(snap.Tools, ls.tools.items()...)
	}
	if s.planUsage != nil {
		u := *s.planUsage
		snap.PlanUsage = &u
	}
	sortByStart(snap.Sessions, func(v Session) (time.Time, string) { return v.StartedAt, v.ID })
	sortByStart(snap.Agents, func(v AgentNode) (time.Time, string) { return v.StartedAt, v.ID })
	sortByStart(snap.Spans, func(v InferenceSpan) (time.Time, string) { return v.StartedAt, v.ID })
	sortByStart(snap.Tools, func(v ToolInvocation) (time.Time, string) { return v.StartedAt, v.ToolUseID })
	return snap
}

// sortByStart sorts by start time, then ID, so ties order deterministically.
func sortByStart[T any](vs []T, key func(T) (time.Time, string)) {
	slices.SortFunc(vs, func(a, b T) int {
		startA, idA := key(a)
		startB, idB := key(b)
		return cmp.Or(startA.Compare(startB), cmp.Compare(idA, idB))
	})
}

func cloneTime(t *time.Time) *time.Time {
	if t == nil {
		return nil
	}
	c := *t
	return &c
}

// ring is a fixed-capacity insertion-ordered buffer keyed by ID. Putting an
// existing ID updates it in place; putting a new ID into a full ring evicts
// the oldest entry. It grows on demand, so a mostly idle session stays small.
type ring[T any] struct {
	buf      []T
	ids      []string
	head     int // slot of the oldest entry; stays 0 until the ring is full
	capacity int
	index    map[string]int // id -> slot
}

func newRing[T any](capacity int) *ring[T] {
	return &ring[T]{capacity: capacity, index: make(map[string]int)}
}

func (r *ring[T]) has(id string) bool {
	_, ok := r.index[id]
	return ok
}

func (r *ring[T]) put(id string, v T) {
	if slot, ok := r.index[id]; ok {
		r.buf[slot] = v
		return
	}
	if len(r.buf) < r.capacity {
		r.index[id] = len(r.buf)
		r.buf = append(r.buf, v)
		r.ids = append(r.ids, id)
		return
	}
	slot := r.head
	delete(r.index, r.ids[slot])
	r.head = (r.head + 1) % r.capacity
	r.buf[slot] = v
	r.ids[slot] = id
	r.index[id] = slot
}

func (r *ring[T]) items() []T {
	out := make([]T, 0, len(r.buf))
	for i := range r.buf {
		out = append(out, r.buf[(r.head+i)%len(r.buf)])
	}
	return out
}
