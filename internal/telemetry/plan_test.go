package telemetry

import (
	"encoding/json"
	"io"
	"net/http"
	"strings"
	"testing"
	"time"
)

// change is a rise at minute m, bucketed among sessions.
func change(m, rise float64, bucket Bucket, sessions ...string) UsageObservation {
	return UsageObservation{Window: "5h", Rise: rise, ObservedAt: minute(m), Bucket: bucket, Sessions: sessions}
}

// tokenSpan is a completed span of session at minute m with each tier set to n.
func tokenSpan(id, session string, m float64, n int) InferenceSpan {
	sp := span(id, session, m, m+1)
	sp.InputTokensUncached, sp.InputTokensCacheWrite, sp.InputTokensCacheRead, sp.OutputTokens = n, n, n, n
	return sp
}

func TestPlanView(t *testing.T) {
	reset := minute(240) // The window started at minute -60
	w := UsageWindow{Name: "5h", Utilization: 0.5, ResetsAt: &reset, Length: 5 * time.Hour}
	history := []UsageObservation{
		change(-50, 0, BucketUnseen), // The first value: no rise
		change(-40, 0.01, BucketAttributed, "A"),
		change(-30, 0.01, BucketAttributed, "A"),
		change(-20, 0.01, BucketShared, "A", "B"),
		change(-10, 0.03, BucketUnseen),
	}
	spans := []InferenceSpan{
		tokenSpan("a1", "A", -45, 100), tokenSpan("a2", "A", -25, 100),
		tokenSpan("b1", "B", -22, 10),
		tokenSpan("c0", "C", -70, 999), // Before the window
		tokenSpan("d1", "D", -5, 50),   // Active, no rise yet
	}
	sessions := map[string]*Session{"A": {ID: "A", CurrentContextSize: 530_000}, "B": {ID: "B"}}
	v := planView(w, []UsageWindow{{Name: "7d", Utilization: 0.34}}, history, spans, sessions, minute(0))

	if v.Window != "5h" || v.StartsAt == nil || !v.StartsAt.Equal(minute(-60)) || v.Unseen != 0.03 || len(v.Others) != 1 {
		t.Errorf("view = %+v, want 5h from minute -60, Unseen 0.03, 7d among the others", v)
	}
	var order []string
	for _, r := range v.Sessions {
		order = append(order, r.ID)
	}
	if strings.Join(order, ",") != "A,B,D" {
		t.Fatalf("sessions = %v, want A (3%%), B (1%%), then D by tokens; C started before the window", order)
	}
	a, b, d := v.Sessions[0], v.Sessions[1], v.Sessions[2]
	if a.Attributed != 0.02 || a.Shared != 0.01 || strings.Join(a.SharedWith, ",") != "B" || a.Spans != 2 ||
		a.InputTokensCacheRead != 200 || a.Session == nil || a.Session.CurrentContextSize != 530_000 || !a.LastActive.Equal(minute(-25)) {
		t.Errorf("A = %+v", a)
	}
	if b.Attributed != 0 || b.Shared != 0.01 || strings.Join(b.SharedWith, ",") != "A" || b.OutputTokens != 10 {
		t.Errorf("B = %+v", b)
	}
	if d.Attributed+d.Shared != 0 || d.Spans != 1 || d.Session != nil {
		t.Errorf("D = %+v, want its tokens, no rise, no live session", d)
	}
}

func TestPlanViewAfterReset(t *testing.T) {
	reset := minute(-10)
	w := UsageWindow{Name: "5h", Utilization: 0.9, ResetsAt: &reset, Length: 5 * time.Hour}
	history := []UsageObservation{change(-20, 0.05, BucketAttributed, "A")}
	spans := []InferenceSpan{tokenSpan("a1", "A", -20, 100), tokenSpan("a2", "A", -5, 7)}
	v := planView(w, nil, history, spans, nil, minute(0))
	if v.Utilization != 0 || v.ResetsAt != nil || len(v.Sessions) != 1 || v.Sessions[0].Attributed != 0 || v.Sessions[0].OutputTokens != 7 {
		t.Errorf("view = %+v, want an empty new window counting only the span after the reset", v)
	}
}

func TestHubServePlan(t *testing.T) {
	s, _, srv, _ := newTestHub(t)
	get := func() string {
		t.Helper()
		resp, err := http.Get(srv.URL + "/api/plan")
		mustNoErr(t, err)
		defer resp.Body.Close()
		body, _ := io.ReadAll(resp.Body)
		return strings.TrimSpace(string(body))
	}
	if got, want := get(), `{"window":"","utilization":0,"rate_per_hour":null,"time_to_limit_seconds":null,"others":[],"unseen":0,"sessions":[]}`; got != want {
		t.Errorf("before any plan usage:\n  %s\nwant\n  %s", got, want)
	}

	now := time.Now()
	reset := now.Add(time.Hour)
	mustNoErr(t, s.UpsertSession(Session{ID: "s1"}))
	mustNoErr(t, s.UpsertSpan(InferenceSpan{ID: "sp1", SessionID: "s1", AgentID: "s1/main", StartedAt: now.Add(-time.Minute), OutputTokens: 5}))
	_, err := s.RecordPlanUsage(PlanUsage{ObservedAt: now, Windows: []UsageWindow{
		{Name: "5h", Utilization: 0.2, ResetsAt: &reset, Length: 5 * time.Hour}, {Name: "7d", Utilization: 0.3},
	}}, "s1", "sp1")
	mustNoErr(t, err)
	var v PlanView
	mustNoErr(t, json.Unmarshal([]byte(get()), &v))
	if v.Window != "5h" || len(v.Sessions) != 1 || v.Sessions[0].ID != "s1" || v.Sessions[0].OutputTokens != 5 || len(v.Others) != 1 {
		t.Errorf("plan = %+v", v)
	}
}
