package core

import (
	"time"

	"github.com/zealtv/easyhog/internal/groundhog"
)

// TickFreshness is how recent a recorded tick must be for Easyhog to say
// something outside it probably keeps ticking an instance.
const TickFreshness = 3 * time.Minute

// Outlook answers one question: will the instance's pending items be actioned?
type Outlook int

const (
	// OutlookNo: the instance is paused, so no tick fires anything.
	OutlookNo Outlook = iota
	// OutlookYes: this session's runner is running and not paused.
	OutlookYes
	// OutlookProbably: not running here, but a tick was recorded within
	// TickFreshness, so something outside Easyhog probably keeps ticking it.
	OutlookProbably
	// OutlookNotSeen: no tick recorded, or the last one is older than
	// TickFreshness. This includes scripts that do not report last_tick.
	OutlookNotSeen
)

// Verdict is the one-line answer plus what it rests on.
type Verdict struct {
	Outlook Outlook
	// Age is how long before the status was observed the last tick happened.
	// Both times come from the instance script's host clock, so the clocks
	// are never mixed; it is not "ago" from now, and a snapshot that has not
	// been refreshed keeps its age. Valid only when HasTick.
	Age time.Duration
	// HasTick says a last tick was recorded.
	HasTick bool
	// Source is the recorded tick's source ("runner" or "standalone").
	Source string
	// ObservedAt is when the status was taken, by the script's clock.
	ObservedAt time.Time
	// Uncertain counts the deliveries the status holds as `uncertain`. While any
	// exists, Groundhog's barrier stops every consumer call: items still fire and
	// register deliveries, and their handoffs wait until each uncertain delivery
	// is recovered. It qualifies OutlookYes
	// and OutlookProbably; a paused instance fires nothing either way.
	Uncertain int
}

// HandoffsWait reports that items fire but no handoff will be made until the
// uncertain deliveries are recovered: the verdict is not a plain yes.
func (v Verdict) HandoffsWait() bool {
	return v.Uncertain > 0 && (v.Outlook == OutlookYes || v.Outlook == OutlookProbably)
}

// PendingVerdict builds the verdict from generic status alone. It looks for no
// host or application and uses only paused, the runner's state, ownership as
// the session classified it, and last_tick. It reports false when there is no
// status to judge from, or the instance is one whose script is not run.
func PendingVerdict(iv InstanceView) (Verdict, bool) {
	if iv.Inspection.Last == nil || iv.Class == ClassMalformed || iv.Class == ClassLegacy {
		return Verdict{}, false
	}
	return verdictFor(iv.Class, iv.Inspection.Last.Status), true
}

// verdictFor is the pure rule, separate so a table can drive it.
func verdictFor(class Class, st groundhog.Status) Verdict {
	v := Verdict{ObservedAt: st.ObservedAt, Uncertain: st.DeliveryCounts()[groundhog.DeliveryUncertain]}
	if lt := st.LastTick; lt != nil {
		v.HasTick, v.Source = true, lt.Source
		v.Age = max(0, st.ObservedAt.Sub(lt.At))
	}
	switch {
	case st.Paused:
		v.Outlook = OutlookNo
	case class == ClassOwnedBySession && st.Runner.State == groundhog.RunnerRunning:
		v.Outlook = OutlookYes
	case v.HasTick && v.Age <= TickFreshness:
		v.Outlook = OutlookProbably
	default:
		v.Outlook = OutlookNotSeen
	}
	return v
}
