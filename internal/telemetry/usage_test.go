package telemetry

import (
	"database/sql"
	"io"
	"net/http"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

var u0 = time.Date(2026, 10, 15, 10, 0, 0, 0, time.UTC)

func minute(m float64) time.Time { return u0.Add(time.Duration(m * float64(time.Minute))) }

const inFlight = -1000.0

// span is session's span from minute start to minute end, or still in flight.
func span(id, session string, start, end float64) InferenceSpan {
	sp := InferenceSpan{ID: id, SessionID: session, AgentID: session + "/main", StartedAt: minute(start)}
	if end != inFlight {
		done := minute(end)
		sp.CompletedAt = &done
	}
	return sp
}

func TestAttribute(t *testing.T) {
	since := minute(0)
	tests := []struct {
		name         string
		since        *time.Time
		until        float64
		spans        []InferenceSpan
		want         Bucket
		wantSessions string
	}{
		{"previous value's time unknown", nil, 10, []InferenceSpan{span("a1", "A", -1, inFlight)}, BucketUnseen, ""},
		{"one session throughout", &since, 10, []InferenceSpan{span("a1", "A", -1, 4), span("a2", "A", 4, inFlight)}, BucketAttributed, "A"},
		{"two sessions", &since, 10, []InferenceSpan{span("a1", "A", -1, inFlight), span("b1", "B", 3, 6)}, BucketShared, "A,B"},
		{"idle before the first span", &since, 10, []InferenceSpan{span("a1", "A", 6, inFlight)}, BucketUnseen, "A"},
		// The previous value was confirmed mid-turn, then the person stepped
		// away (and used claude.ai) before the next turn revealed the rise.
		{"idle mid-interval", &since, 20, []InferenceSpan{span("a1", "A", -1, 1), span("a2", "A", 16, inFlight)}, BucketUnseen, "A"},
		{"a pause within IdleGap", &since, 10, []InferenceSpan{span("a1", "A", -1, 1), span("a2", "A", 5, inFlight)}, BucketAttributed, "A"},
		{"spans outside the interval are ignored", &since, 10, []InferenceSpan{
			span("b0", "B", -10, -1), span("a1", "A", -1, inFlight), span("b2", "B", 11, inFlight),
		}, BucketAttributed, "A"},
		{"nothing in flight", &since, 10, nil, BucketUnseen, ""},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got, sessions := attribute(tt.spans, tt.since, minute(tt.until))
			if got != tt.want || strings.Join(sessions, ",") != tt.wantSessions {
				t.Errorf("attribute = %s %v, want %s [%s]", got, sessions, tt.want, tt.wantSessions)
			}
		})
	}
}

// obs is a change to util at minute m, rise from the previous value.
func obs(m, util, rise float64) UsageObservation {
	return UsageObservation{Window: "5h", Utilization: util, Rise: rise, ObservedAt: minute(m)}
}

func TestBurnRate(t *testing.T) {
	tests := []struct {
		name    string
		history []UsageObservation
		now     float64
		want    float64 // 0 means no rate
	}{
		{"no history", nil, 30, 0},
		{"one rise", []UsageObservation{obs(0, 0.40, 0), obs(10, 0.41, 0.01)}, 30, 0},
		{"two rises over the lookback", []UsageObservation{obs(0, 0.40, 0), obs(10, 0.41, 0.01), obs(20, 0.42, 0.01)}, 30, 0.04},
		{"baseline set before the lookback", []UsageObservation{obs(0, 0.40, 0), obs(40, 0.41, 0.01), obs(50, 0.42, 0.01)}, 60, 0.04},
		{"rises before the lookback do not count", []UsageObservation{
			obs(0, 0.40, 0), obs(5, 0.41, 0.01), obs(10, 0.42, 0.01), obs(50, 0.43, 0.01),
		}, 60, 0},
		{"history starts inside the lookback", []UsageObservation{obs(50, 0.10, 0.10), obs(55, 0.11, 0.01), obs(58, 0.12, 0.01)}, 60, 0.12},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got := burnRate(tt.history, minute(tt.now))
			switch {
			case tt.want == 0 && got != nil:
				t.Errorf("burnRate = %v, want none", *got)
			case tt.want != 0 && got == nil:
				t.Errorf("burnRate = none, want %v", tt.want)
			case tt.want != 0 && (*got < tt.want-1e-9 || *got > tt.want+1e-9):
				t.Errorf("burnRate = %v, want %v", *got, tt.want)
			}
		})
	}
}

func TestBurn(t *testing.T) {
	history := []UsageObservation{obs(0, 0.88, 0), obs(10, 0.89, 0.01), obs(20, 0.90, 0.01)} // 4%/h at minute 30
	later, soon, past := minute(30+180), minute(30+60), minute(20)

	b := burn(UsageWindow{Name: "5h", Utilization: 0.90, ResetsAt: &later}, history, minute(30))
	if b.TimeToLimit == nil || b.TimeToLimit.Round(time.Minute) != 150*time.Minute {
		t.Errorf("reset in 3h: TimeToLimit = %v, want 2h30m (10%% left at 4%%/h)", b.TimeToLimit)
	}
	b = burn(UsageWindow{Name: "5h", Utilization: 0.90, ResetsAt: &soon}, history, minute(30))
	if b.RatePerHour == nil || b.TimeToLimit != nil || b.ResetsAt == nil {
		t.Errorf("reset in 1h: %+v, want a rate, no TimeToLimit, the reset time", b)
	}
	b = burn(UsageWindow{Name: "5h", Utilization: 0.90, ResetsAt: &past}, history, minute(30))
	if b.Utilization != 0 || b.RatePerHour != nil || b.ResetsAt != nil {
		t.Errorf("reset already passed: %+v, want an empty window", b)
	}
}

func TestStatusLine(t *testing.T) {
	zone := time.FixedZone("UTC+2", 2*60*60)
	now := u0.In(zone)
	resets := time.Date(2026, 10, 15, 12, 20, 0, 0, time.UTC) // 14:20 in zone
	rate, ttl := 0.06, 70*time.Minute
	ctx := &Session{CurrentContextSize: 530_000, MaxContextWindow: 1_000_000}
	tests := []struct {
		name  string
		burns []Burn
		sess  *Session
		want  string
	}{
		{"full before reset", []Burn{
			{Window: "5h", Utilization: 0.91, ResetsAt: &resets, RatePerHour: &rate, TimeToLimit: &ttl},
			{Window: "7d", Utilization: 0.34},
		}, ctx, "5h 91% · +6%/h · full ~1h10m · ctx 530K/1M"},
		{"reset first", []Burn{{Window: "5h", Utilization: 0.91, ResetsAt: &resets, RatePerHour: &rate}}, nil, "5h 91% · +6%/h · resets 14:20"},
		{"no rate yet", []Burn{{Window: "5h", Utilization: 0.22, ResetsAt: &resets}}, nil, "5h 22% · — · resets 14:20"},
		{"later window from 50%", []Burn{{Window: "5h", Utilization: 0.40}, {Window: "7d", Utilization: 0.55}}, nil, "5h 40% · — · 7d 55%"},
		{"no plan usage", nil, &Session{CurrentContextSize: 950, MaxContextWindow: 200_000}, "no plan usage yet · ctx 950/200K"},
		{"no plan usage, unknown session", nil, nil, "no plan usage yet"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if got := statusLine(tt.burns, tt.sess, now); got != tt.want {
				t.Errorf("statusLine =\n  %q\nwant\n  %q", got, tt.want)
			}
		})
	}
}

// usageAt is a 5h plan usage observed at minute m.
func usageAt(m, util float64, resets time.Time) PlanUsage {
	return PlanUsage{Provider: "claude_code", ObservedAt: minute(m), Windows: []UsageWindow{
		{Name: "5h", Utilization: util, ResetsAt: &resets, Status: "allowed"},
	}}
}

func TestStoreRecordPlanUsage(t *testing.T) {
	s, err := OpenStore("", 0)
	mustNoErr(t, err)
	reset := minute(300)
	record := func(m, util float64, resets time.Time, session, spanID string) {
		t.Helper()
		if _, err := s.RecordPlanUsage(usageAt(m, util, resets), session, spanID); err != nil {
			t.Fatal(err)
		}
	}

	// Session A works for half an hour; every response confirms 45%.
	for k := range 7 {
		start := float64(5 * k)
		end := start + 4
		if k == 6 {
			end = inFlight // In flight: its response reveals the rise
		}
		mustNoErr(t, s.UpsertSpan(span("a"+string(rune('0'+k)), "A", start, end)))
		if k < 6 {
			record(start+1, 0.45, reset, "A", "")
		}
	}
	if h := s.UsageHistory("5h"); len(h) != 1 || h[0].Bucket != BucketUnseen || h[0].Rise != 0 || h[0].Since != nil {
		t.Fatalf("history after confirmations = %+v, want only the first value, Unseen", h)
	}

	record(31, 0.46, reset, "A", "a6")
	h := s.UsageHistory("5h")
	if len(h) != 2 {
		t.Fatalf("history = %+v, want 2 changes", h)
	}
	// The interval starts at the last confirmation (minute 26), not at the
	// stored change half an hour earlier.
	if got := h[1]; got.Bucket != BucketAttributed || got.Rise != 0.01 || got.Since == nil || !got.Since.Equal(minute(26)) ||
		got.SpanID != "a6" || strings.Join(got.Sessions, ",") != "A" {
		t.Errorf("rise = %+v, want Attributed to A since minute 26, Rise 0.01", got)
	}

	// A response processed after a newer one changes nothing.
	latest, err := s.RecordPlanUsage(usageAt(29, 0.45, reset), "A", "")
	mustNoErr(t, err)
	if len(s.UsageHistory("5h")) != 2 || latest.Windows[0].Utilization != 0.46 {
		t.Errorf("late response: history %d, latest %v; want 2 and 0.46 kept", len(s.UsageHistory("5h")), latest.Windows[0].Utilization)
	}

	mustNoErr(t, s.UpsertSpan(span("b1", "B", 32, inFlight)))
	record(33, 0.47, reset, "B", "b1")
	if got := s.UsageHistory("5h")[2]; got.Bucket != BucketShared || strings.Join(got.Sessions, ",") != "A,B" {
		t.Errorf("rise with two sessions = %+v, want Shared by A,B", got)
	}

	// A new plan window: the rise counts from 0 and the history restarts.
	record(34, 0.02, minute(600), "B", "b1")
	if h := s.UsageHistory("5h"); len(h) != 1 || h[0].Rise != 0.02 {
		t.Errorf("history after reset = %+v, want one change rising 0.02 from 0", h)
	}
}

func TestStoreUsageSurvivesRestart(t *testing.T) {
	path := filepath.Join(t.TempDir(), "state.db")
	s, err := OpenStore(path, 0)
	mustNoErr(t, err)
	oldReset, reset := minute(0), minute(300)
	for _, u := range []PlanUsage{usageAt(-10, 0.90, oldReset), usageAt(1, 0.45, reset), usageAt(2, 0.46, reset)} {
		_, err := s.RecordPlanUsage(u, "A", "")
		mustNoErr(t, err)
	}
	mustNoErr(t, s.Close())

	s, err = OpenStore(path, 0)
	mustNoErr(t, err)
	defer s.Close()
	_, err = s.Load()
	mustNoErr(t, err)
	if h := s.UsageHistory("5h"); len(h) != 2 || h[1].Utilization != 0.46 {
		t.Fatalf("restored history = %+v, want the 2 changes of the current plan window", h)
	}
	// The last value is known, its confirmation time is not.
	mustNoErr(t, s.UpsertSpan(span("a1", "A", 3, inFlight)))
	_, err = s.RecordPlanUsage(usageAt(4, 0.47, reset), "A", "a1")
	mustNoErr(t, err)
	if got := s.UsageHistory("5h")[2]; got.Rise != 0.01 || got.Bucket != BucketUnseen || got.Since != nil {
		t.Errorf("first rise after restart = %+v, want Rise 0.01, Unseen", got)
	}
}

func TestStorePrunesUsageObservations(t *testing.T) {
	path := filepath.Join(t.TempDir(), "state.db")
	s, err := OpenStore(path, 0)
	mustNoErr(t, err)
	now := time.Now()
	for i, observed := range []time.Time{now.Add(-31 * 24 * time.Hour), now.Add(-29 * 24 * time.Hour)} {
		reset := observed.Add(time.Hour)
		_, err := s.RecordPlanUsage(PlanUsage{ObservedAt: observed, Windows: []UsageWindow{{Name: "5h", Utilization: float64(i+1) / 10, ResetsAt: &reset}}}, "", "")
		mustNoErr(t, err)
	}
	n, err := s.Prune(now.Add(-30 * 24 * time.Hour))
	mustNoErr(t, err)
	mustNoErr(t, s.Close())

	db, err := sql.Open("sqlite", path)
	mustNoErr(t, err)
	defer db.Close()
	var left int
	mustNoErr(t, db.QueryRow(`SELECT count(*) FROM usage_observations`).Scan(&left))
	if n != 1 || left != 1 {
		t.Errorf("pruned %d, %d left; want 1 and 1", n, left)
	}
}

func TestHubServeStatusLine(t *testing.T) {
	s, _, srv, _ := newTestHub(t)
	mustNoErr(t, s.UpsertSession(Session{ID: "s1", CurrentContextSize: 120_000, MaxContextWindow: 200_000}))
	reset := time.Now().Add(3 * time.Hour)
	_, err := s.RecordPlanUsage(PlanUsage{ObservedAt: time.Now(), Windows: []UsageWindow{{Name: "5h", Utilization: 0.42, ResetsAt: &reset}}}, "s1", "")
	mustNoErr(t, err)

	resp, err := http.Get(srv.URL + "/api/statusline?session=s1")
	mustNoErr(t, err)
	defer resp.Body.Close()
	body, _ := io.ReadAll(resp.Body)
	want := "5h 42% · — · resets " + reset.Format("15:04") + " · ctx 120K/200K"
	if ct := resp.Header.Get("Content-Type"); ct != "text/plain; charset=utf-8" || string(body) != want {
		t.Errorf("GET /api/statusline = %q (%s), want %q", body, ct, want)
	}
}

func TestAlerts(t *testing.T) {
	zone := time.FixedZone("UTC+2", 2*60*60)
	now := u0.In(zone)
	reset := time.Date(2026, 10, 15, 12, 20, 0, 0, time.UTC) // 14:20 in zone
	past := u0.Add(-time.Minute)
	tests := []struct {
		name    string
		windows []UsageWindow
		want    string // id|text per alert, joined by ";"
	}{
		{"below every threshold", []UsageWindow{{Name: "5h", Utilization: 0.79, ResetsAt: &reset}, {Name: "7d", Utilization: 0.89}}, ""},
		{"5h past 80%", []UsageWindow{{Name: "5h", Utilization: 0.83, ResetsAt: &reset}},
			"5h@1792066800/80|5h plan window at 83% (past 80%) · resets 14:20"},
		{"only the highest threshold", []UsageWindow{{Name: "5h", Utilization: 0.9, ResetsAt: &reset}},
			"5h@1792066800/90|5h plan window at 90% (past 90%) · resets 14:20"},
		{"7d warns at 90% only", []UsageWindow{{Name: "7d", Utilization: 0.85}, {Name: "7d", Utilization: 0.91}},
			"7d@0/90|7d plan window at 91% (past 90%)"},
		{"a reset window is quiet", []UsageWindow{{Name: "5h", Utilization: 0.95, ResetsAt: &past}}, ""},
		{"windows without thresholds are quiet", []UsageWindow{{Name: "1h", Utilization: 1}}, ""},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			var got []string
			for _, a := range alerts(tt.windows, now) {
				got = append(got, a.ID+"|"+a.Text)
			}
			if strings.Join(got, ";") != tt.want {
				t.Errorf("alerts =\n  %q\nwant\n  %q", strings.Join(got, ";"), tt.want)
			}
		})
	}
}

func TestHubServeAlerts(t *testing.T) {
	s, _, srv, _ := newTestHub(t)
	get := func() string {
		t.Helper()
		resp, err := http.Get(srv.URL + "/api/alerts")
		mustNoErr(t, err)
		defer resp.Body.Close()
		if ct := resp.Header.Get("Content-Type"); ct != "application/json" {
			t.Errorf("Content-Type = %q", ct)
		}
		body, _ := io.ReadAll(resp.Body)
		return strings.TrimSpace(string(body))
	}
	if got := get(); got != "[]" {
		t.Errorf("before any plan usage = %s, want []", got)
	}
	reset := time.Now().Add(time.Hour)
	_, err := s.RecordPlanUsage(PlanUsage{ObservedAt: time.Now(), Windows: []UsageWindow{{Name: "5h", Utilization: 0.81, ResetsAt: &reset}}}, "", "")
	mustNoErr(t, err)
	if got := get(); !strings.Contains(got, `"id":"5h@`) || !strings.Contains(got, `"threshold":0.8`) {
		t.Errorf("past 80%% = %s, want the 5h alert", got)
	}
}
