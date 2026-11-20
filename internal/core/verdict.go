package core

import (
	"time"

	"github.com/zealtv/easyhog/internal/groundhog"
)

// TickFreshness is how recent a recorded tick must be for Easyhog to say
// something outside it probably keeps ticking an instance.
const TickFreshness = 3 * time.Minute

// Outlook answers one question: will the instance's due items fire and their
// runs start?
type Outlook int

const (
	// OutlookNo: the instance is paused, so no tick fires or starts anything.
	OutlookNo Outlook = iota
	// OutlookYes: this session ticks it.
	OutlookYes
	// OutlookProbably: not ticked here, but a tick that was not this session's
	// was recorded within TickFreshness, so something outside Easyhog probably
	// keeps ticking it.
	OutlookProbably
	// OutlookNotSeen: no tick recorded, the last one is older than
	// TickFreshness, or it was this session's and this session has stopped.
	OutlookNotSeen
)

// Verdict is the one-line answer plus what it rests on.
type Verdict struct {
	Outlook Outlook
	// Age is how long before the status was observed the last tick happened.
	// Both times come from the instance script's host clock, so the clocks
	// are never mixed; a snapshot that has not been refreshed keeps its age.
	// Valid only when HasTick.
	Age time.Duration
	// HasTick says a last tick was recorded.
	HasTick bool
	// Ours says the recorded tick was one of this session's.
	Ours bool
	// ObservedAt is when the status was taken, by the script's clock.
	ObservedAt time.Time
}

// PendingVerdict builds the verdict from the status and what this session's
// ticker knows (tick, nil when it never ticked the instance). It looks for no
// host or application. It reports false when there is no status to judge from,
// or the instance is one whose script is not run.
func PendingVerdict(iv InstanceView, tick *TickInfo) (Verdict, bool) {
	if iv.Inspection.Last == nil || iv.Class == ClassMalformed || iv.Class == ClassLegacy {
		return Verdict{}, false
	}
	return verdictFor(iv.Inspection.Last.Status, tick), true
}

// verdictFor is the pure rule, separate so a table can drive it.
func verdictFor(st groundhog.Status, tick *TickInfo) Verdict {
	v := Verdict{ObservedAt: st.ObservedAt}
	if lt := st.LastTick; lt != nil {
		v.HasTick = true
		v.Age = max(0, st.ObservedAt.Sub(lt.At))
		v.Ours = tick != nil && tick.Ours(lt.At)
	}
	switch {
	case st.Paused:
		v.Outlook = OutlookNo
	case tick != nil && tick.Ticking:
		v.Outlook = OutlookYes
	case v.HasTick && !v.Ours && v.Age <= TickFreshness:
		v.Outlook = OutlookProbably
	default:
		v.Outlook = OutlookNotSeen
	}
	return v
}

// TickedFromOutside reports whether something outside this session ticked iv
// recently while this session does not: the shell shows it as external. It is
// information only; Run, Retry and the pause are offered on it all the same.
func TickedFromOutside(iv InstanceView, tick *TickInfo) bool {
	v, ok := PendingVerdict(iv, tick)
	return ok && v.Outlook == OutlookProbably
}
