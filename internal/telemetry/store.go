package telemetry

import (
	"cmp"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
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

// maxLiveSessions bounds how many sessions the store keeps in memory. A new
// session beyond it evicts the least recently written session, with its
// agents, spans and tools, from memory; SQLite keeps them.
const maxLiveSessions = 16

// Rows hold the full record as JSON so the schema can evolve without
// migrations; the extra columns exist for ad-hoc querying.
const schemaDDL = `
CREATE TABLE IF NOT EXISTS sessions (id TEXT PRIMARY KEY, data TEXT NOT NULL);
CREATE TABLE IF NOT EXISTS agents (id TEXT PRIMARY KEY, session_id TEXT NOT NULL, data TEXT NOT NULL);
CREATE TABLE IF NOT EXISTS spans (id TEXT PRIMARY KEY, session_id TEXT NOT NULL, agent_id TEXT NOT NULL, data TEXT NOT NULL);
CREATE TABLE IF NOT EXISTS tools (tool_use_id TEXT PRIMARY KEY, span_id TEXT NOT NULL, data TEXT NOT NULL);
`

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
	planUsage *PlanUsage              // latest only; never persisted
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
	s := &Store{capacity: capacity, live: make(map[string]*liveSession)}
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
	return s.persist(`INSERT INTO sessions (data, id) VALUES (?, ?)
		ON CONFLICT(id) DO UPDATE SET data = excluded.data`, sess, sess.ID)
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
	return s.persist(`INSERT INTO agents (data, id, session_id) VALUES (?, ?, ?)
		ON CONFLICT(id) DO UPDATE SET session_id = excluded.session_id, data = excluded.data`,
		a, a.ID, a.SessionID)
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
	return s.persist(`INSERT INTO spans (data, id, session_id, agent_id) VALUES (?, ?, ?, ?)
		ON CONFLICT(id) DO UPDATE SET session_id = excluded.session_id, agent_id = excluded.agent_id, data = excluded.data`,
		sp, sp.ID, sp.SessionID, sp.AgentID)
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
	return s.persist(`INSERT INTO tools (data, tool_use_id, span_id) VALUES (?, ?, ?)
		ON CONFLICT(tool_use_id) DO UPDATE SET span_id = excluded.span_id, data = excluded.data`,
		t, t.ToolUseID, t.SpanID)
}

// SetPlanUsage replaces the account-level plan usage. Only the latest value is
// kept, in memory: every response carrying usage headers refreshes it, so
// there is nothing to persist.
func (s *Store) SetPlanUsage(u PlanUsage) {
	u.Windows = slices.Clone(u.Windows)
	for i := range u.Windows {
		u.Windows[i].ResetsAt = cloneTime(u.Windows[i].ResetsAt)
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	s.planUsage = &u
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
	if len(s.live) >= maxLiveSessions {
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
