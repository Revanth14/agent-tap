package telemetry

import (
	"fmt"
	"math"
	"slices"
	"strings"
	"time"
)

// IdleGap is the longest stretch with no span in flight that an interval
// between two usage observations may hold and still be attributed: a rise
// after a longer gap may come from usage atap cannot see (claude.ai, the
// mobile app, another machine, fast mode), so it is Unseen.
const IdleGap = 5 * time.Minute

// BurnLookback is how far back the burn rate looks.
const BurnLookback = 30 * time.Minute

// Bucket says whom a usage observation's rise can be put down to. A rise is
// never split between sessions.
type Bucket string

const (
	BucketAttributed Bucket = "attributed" // Exactly one session active, no idle gap
	BucketShared     Bucket = "shared"     // Several sessions active, no idle gap
	BucketUnseen     Bucket = "unseen"     // An idle gap, no activity, or no known previous value
)

// UsageObservation is a recorded change in a plan window's utilization. Only
// changes are stored: a response that repeats the last value confirms it,
// which moves the next observation's Since.
type UsageObservation struct {
	Window      string     `json:"window"` // e.g. "5h", "7d"
	Utilization float64    `json:"utilization"`
	Rise        float64    `json:"rise"` // Change from the previous value; from 0 when the plan window reset; 0 when nothing came before
	ResetsAt    *time.Time `json:"resets_at,omitempty"`
	Status      string     `json:"status"`
	ObservedAt  time.Time  `json:"observed_at"`
	Since       *time.Time `json:"since,omitempty"` // When the previous value was last confirmed; nil when unknown, e.g. after a daemon restart
	Bucket      Bucket     `json:"bucket"`
	Sessions    []string   `json:"sessions"`             // Sessions with a span in flight between Since and ObservedAt
	SessionID   string     `json:"session_id,omitempty"` // Session of the response that revealed the change
	SpanID      string     `json:"span_id,omitempty"`    // That response's span
}

// Burn is a plan window's current utilization and how fast it is rising.
type Burn struct {
	Window      string
	Utilization float64
	ResetsAt    *time.Time
	RatePerHour *float64       // nil until BurnLookback holds at least two rises
	TimeToLimit *time.Duration // nil without a rate, or when the window resets first
}

// windowUsage is the store's running state for one plan window.
type windowUsage struct {
	last        UsageWindow // Latest value; zero until known
	known       bool
	confirmedAt *time.Time         // When last was last seen; nil after a restart
	history     []UsageObservation // Changes in the current plan window, oldest first
}

// attribute buckets the rise between since and until from the spans in
// flight during that interval. A nil since means the previous value's time is
// unknown, so nothing can be attributed.
func attribute(spans []InferenceSpan, since *time.Time, until time.Time) (Bucket, []string) {
	if since == nil {
		return BucketUnseen, []string{}
	}
	type interval struct{ start, end time.Time }
	var active []interval
	sessions := []string{}
	for _, sp := range spans {
		end := until
		if sp.CompletedAt != nil && sp.CompletedAt.Before(until) {
			end = *sp.CompletedAt
		}
		if end.Before(*since) || sp.StartedAt.After(until) {
			continue
		}
		active = append(active, interval{start: maxTime(sp.StartedAt, *since), end: end})
		if !slices.Contains(sessions, sp.SessionID) {
			sessions = append(sessions, sp.SessionID)
		}
	}
	slices.Sort(sessions)
	if len(active) == 0 {
		return BucketUnseen, sessions
	}
	slices.SortFunc(active, func(a, b interval) int { return a.start.Compare(b.start) })
	covered := *since
	for _, iv := range active {
		if iv.start.Sub(covered) > IdleGap {
			return BucketUnseen, sessions
		}
		covered = maxTime(covered, iv.end)
	}
	if until.Sub(covered) > IdleGap {
		return BucketUnseen, sessions
	}
	if len(sessions) == 1 {
		return BucketAttributed, sessions
	}
	return BucketShared, sessions
}

// burnRate is the utilization rise per hour over the BurnLookback before now,
// from a plan window's changes (oldest first). It is nil with fewer than two
// rises in that time: one step of 0.01 says too little about a rate.
func burnRate(history []UsageObservation, now time.Time) *float64 {
	if len(history) == 0 {
		return nil
	}
	start := now.Add(-BurnLookback)
	base := 0 // The value in force at start, or the first one known after it
	for i, o := range history {
		if o.ObservedAt.After(start) {
			break
		}
		base = i
	}
	rises := 0
	for _, o := range history[base+1:] {
		if o.Rise > 0 && !o.ObservedAt.After(now) {
			rises++
		}
	}
	if rises < 2 {
		return nil
	}
	from := maxTime(start, history[base].ObservedAt)
	hours := now.Sub(from).Hours()
	if hours <= 0 {
		return nil
	}
	rate := (history[len(history)-1].Utilization - history[base].Utilization) / hours
	if rate <= 0 {
		return nil
	}
	return &rate
}

// burn describes window w as of now, given its changes in the current plan
// window.
func burn(w UsageWindow, history []UsageObservation, now time.Time) Burn {
	if w.ResetsAt != nil && !now.Before(*w.ResetsAt) {
		// The window has reset since the last response: nothing is used yet.
		return Burn{Window: w.Name}
	}
	b := Burn{Window: w.Name, Utilization: w.Utilization, ResetsAt: w.ResetsAt, RatePerHour: burnRate(history, now)}
	if b.RatePerHour != nil {
		left := time.Duration(max(1-w.Utilization, 0) / *b.RatePerHour * float64(time.Hour))
		if w.ResetsAt == nil || now.Add(left).Before(*w.ResetsAt) {
			b.TimeToLimit = &left
		}
	}
	return b
}

// statusLine is the one-line summary for a session's status bar, e.g.
// "5h 91% · +6%/h · full ~1h10m · 7d 55% · ctx 530K/1M". The first window
// always shows; later ones only from 50%. sess may be nil.
func statusLine(burns []Burn, sess *Session, now time.Time) string {
	var parts []string
	if len(burns) == 0 {
		parts = append(parts, "no plan usage yet")
	}
	for i, b := range burns {
		if i > 0 {
			if b.Utilization >= 0.5 {
				parts = append(parts, fmt.Sprintf("%s %d%%", b.Window, percent(b.Utilization)))
			}
			continue
		}
		parts = append(parts, fmt.Sprintf("%s %d%%", b.Window, percent(b.Utilization)))
		if b.RatePerHour != nil {
			parts = append(parts, fmt.Sprintf("+%d%%/h", percent(*b.RatePerHour)))
		} else {
			parts = append(parts, "—")
		}
		switch {
		case b.TimeToLimit != nil:
			parts = append(parts, "full ~"+shortDuration(*b.TimeToLimit))
		case b.ResetsAt != nil:
			parts = append(parts, "resets "+b.ResetsAt.In(now.Location()).Format("15:04"))
		}
	}
	if sess != nil && sess.CurrentContextSize > 0 {
		ctx := "ctx " + shortTokens(sess.CurrentContextSize)
		if sess.MaxContextWindow > 0 {
			ctx += "/" + shortTokens(sess.MaxContextWindow)
		}
		parts = append(parts, ctx)
	}
	return strings.Join(parts, " · ")
}

func percent(f float64) int { return int(math.Round(f * 100)) }

// shortDuration renders d to the minute: "45m", "1h10m".
func shortDuration(d time.Duration) string {
	m := int(d.Round(time.Minute) / time.Minute)
	if m < 60 {
		return fmt.Sprintf("%dm", m)
	}
	return fmt.Sprintf("%dh%02dm", m/60, m%60)
}

// shortTokens renders n as "950", "530K" or "1.2M".
func shortTokens(n int) string {
	switch {
	case n >= 1_000_000:
		return strings.TrimSuffix(fmt.Sprintf("%.1f", float64(n)/1e6), ".0") + "M"
	case n >= 1_000:
		return fmt.Sprintf("%dK", int(math.Round(float64(n)/1e3)))
	}
	return fmt.Sprint(n)
}

func maxTime(a, b time.Time) time.Time {
	if a.After(b) {
		return a
	}
	return b
}

func sameTime(a, b *time.Time) bool {
	if a == nil || b == nil {
		return a == b
	}
	return a.Equal(*b)
}
