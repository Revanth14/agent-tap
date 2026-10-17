package telemetry

import (
	"cmp"
	"math"
	"slices"
	"time"
)

// PlanView answers "what is eating my plan" for the current plan window of
// the first (shortest) window: its burn, and every session active in it
// ranked by the rise it can be held to.
type PlanView struct {
	Window             string        `json:"window"` // "" before any plan usage
	Utilization        float64       `json:"utilization"`
	StartsAt           *time.Time    `json:"starts_at,omitempty"`
	ResetsAt           *time.Time    `json:"resets_at,omitempty"`
	RatePerHour        *float64      `json:"rate_per_hour"`
	TimeToLimitSeconds *float64      `json:"time_to_limit_seconds"`
	Others             []UsageWindow `json:"others"` // The later windows, e.g. 7d
	Unseen             float64       `json:"unseen"` // Rise atap could not attribute
	Sessions           []PlanSession `json:"sessions"`
}

// PlanSession is one session's share of a plan window. Its token tiers count
// only spans that started in the window.
type PlanSession struct {
	ID                    string     `json:"id"`
	Session               *Session   `json:"session"` // nil once the session has left memory
	Attributed            float64    `json:"attributed"`
	Shared                float64    `json:"shared"`
	SharedWith            []string   `json:"shared_with"`
	Spans                 int        `json:"spans"`
	InputTokensUncached   int        `json:"input_tokens_uncached"`
	InputTokensCacheWrite int        `json:"input_tokens_cache_write"`
	InputTokensCacheRead  int        `json:"input_tokens_cache_read"`
	OutputTokens          int        `json:"output_tokens"`
	LastActive            *time.Time `json:"last_active,omitempty"`
}

// planView builds the view of window w from its changes in the current plan
// window, the later windows, the live spans and sessions.
func planView(w UsageWindow, others []UsageWindow, history []UsageObservation, spans []InferenceSpan, sessions map[string]*Session, now time.Time) PlanView {
	b := burn(w, history, now)
	v := PlanView{
		Window: w.Name, Utilization: b.Utilization, ResetsAt: b.ResetsAt, RatePerHour: b.RatePerHour,
		Others: slices.Clone(others), Sessions: []PlanSession{},
	}
	if v.Others == nil {
		v.Others = []UsageWindow{}
	}
	if b.TimeToLimit != nil {
		secs := b.TimeToLimit.Seconds()
		v.TimeToLimitSeconds = &secs
	}
	switch {
	case w.ResetsAt != nil && !now.Before(*w.ResetsAt):
		// The window has reset since the last response: a new one began.
		start := *w.ResetsAt
		v.StartsAt, history = &start, nil
	case w.ResetsAt != nil && w.Length > 0:
		start := w.ResetsAt.Add(-w.Length)
		v.StartsAt = &start
	}

	rows := map[string]*PlanSession{}
	row := func(id string) *PlanSession {
		if r := rows[id]; r != nil {
			return r
		}
		r := &PlanSession{ID: id, Session: sessions[id], SharedWith: []string{}}
		rows[id] = r
		return r
	}
	for _, o := range history {
		if o.Rise <= 0 {
			continue
		}
		switch o.Bucket {
		case BucketAttributed:
			for _, id := range o.Sessions {
				row(id).Attributed += o.Rise
			}
		case BucketShared:
			for _, id := range o.Sessions {
				r := row(id)
				r.Shared += o.Rise
				for _, other := range o.Sessions {
					if other != id && !slices.Contains(r.SharedWith, other) {
						r.SharedWith = append(r.SharedWith, other)
					}
				}
			}
		default:
			v.Unseen += o.Rise
		}
	}
	for _, sp := range spans {
		if (v.StartsAt != nil && sp.StartedAt.Before(*v.StartsAt)) || sp.StartedAt.After(now) {
			continue
		}
		r := row(sp.SessionID)
		r.Spans++
		r.InputTokensUncached += sp.InputTokensUncached
		r.InputTokensCacheWrite += sp.InputTokensCacheWrite
		r.InputTokensCacheRead += sp.InputTokensCacheRead
		r.OutputTokens += sp.OutputTokens
		if r.LastActive == nil || sp.StartedAt.After(*r.LastActive) {
			at := sp.StartedAt
			r.LastActive = &at
		}
	}

	v.Unseen = round4(v.Unseen)
	for _, r := range rows {
		r.Attributed, r.Shared = round4(r.Attributed), round4(r.Shared)
		slices.Sort(r.SharedWith)
		v.Sessions = append(v.Sessions, *r)
	}
	slices.SortFunc(v.Sessions, func(a, b PlanSession) int {
		return cmp.Or(
			cmp.Compare(b.Attributed+b.Shared, a.Attributed+a.Shared),
			cmp.Compare(b.tokens(), a.tokens()),
			cmp.Compare(a.ID, b.ID),
		)
	})
	return v
}

func (r PlanSession) tokens() int {
	return r.InputTokensUncached + r.InputTokensCacheWrite + r.InputTokensCacheRead + r.OutputTokens
}

func round4(f float64) float64 { return math.Round(f*1e4) / 1e4 }
