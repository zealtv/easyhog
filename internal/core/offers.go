package core

import "github.com/zealtv/easyhog/internal/groundhog"

// Action is something the reader can ask Easyhog to do to one instance or one
// of its schedule items. Open folder and Refresh are not here: they change
// nothing and are always available.
type Action string

const (
	// ActionRun starts this session ticking the instance; ActionStop stops it.
	ActionRun  Action = "run"
	ActionStop Action = "stop"
	// ActionPause and ActionUnpause set and lift the instance pause, which holds
	// back every ticker's firings and run starts, and `retry`.
	ActionPause   Action = "pause"
	ActionUnpause Action = "unpause"
	// ActionRetry starts the run of the failed and interrupted deliveries again
	// (`retry`), of the instance or of one schedule item.
	ActionRetry Action = "retry"
)

// RetryTargets lists the deliveries Retry would start again in st: the failed
// and interrupted ones, by path ("out/<name>"). item is a schedule item's path
// ("schedule/daily/x") to limit it to that item's deliveries, or "" for every
// delivery of the instance. Those are the two states the contract offers retry
// for: a failed run exited non-zero, and an interrupted one has no exit and no
// wrapper (its `run` may still be going; the reader checks first).
func RetryTargets(st groundhog.Status, item string) []string {
	var out []string
	for _, d := range ofItem(st, item) {
		if d.State == groundhog.DeliveryFailed || d.State == groundhog.DeliveryInterrupted {
			out = append(out, d.Path)
		}
	}
	return out
}

// ofItem is st's deliveries, or those fired from item when it is not "".
func ofItem(st groundhog.Status, item string) []groundhog.Delivery {
	if item == "" {
		return st.Deliveries
	}
	var out []groundhog.Delivery
	for _, d := range st.Deliveries {
		if d.SchedulePath != nil && *d.SchedulePath == item {
			out = append(out, d)
		}
	}
	return out
}

// Offers says what an instance offers, in palette order, and when it offers
// nothing, why. It is the one rule: the palette lists what it returns, and the
// ticker's actions read fresh state themselves (Retry reads status, the script
// refuses what it must). tick is what this session's ticker knows of the
// instance, nil when it never ticked it.
//
//   - Run where this session does not tick it, Stop where it does. Something
//     else ticking it too (cron, a service, a terminal's `groundhog.sh run`) is
//     normal: Groundhog fires each occurrence once and starts each run once.
//   - Pause instance where it is not paused, Unpause instance where it is.
//   - Retry where the status lists failed or interrupted deliveries and the
//     instance is not paused (`retry` refuses then).
//   - Nothing for an instance not inspected yet, malformed, legacy (an older
//     Groundhog among them), unsupported, failing or stale, except Stop where
//     this session ticks it: Stop runs no script.
//
// Offers is pure: it reads only its arguments.
func Offers(iv InstanceView, tick *TickInfo) (actions []Action, why string) {
	return OffersOn(iv, tick, "")
}

// OffersOn is Offers for the instance, or for one of its schedule items when
// item is that item's path ("schedule/daily/x"): Retry then counts only that
// item's deliveries. Run, Stop and the pause act on the instance either way.
func OffersOn(iv InstanceView, tick *TickInfo, item string) (actions []Action, why string) {
	ticking := tick != nil && tick.Ticking
	if why := notActionable(iv); why != "" {
		if ticking {
			// Stop runs no script, so a status that cannot be read, or a script
			// that changed, never keeps the reader from ending this session's ticks.
			return []Action{ActionStop}, ""
		}
		return nil, why
	}
	if ticking {
		actions = append(actions, ActionStop)
	} else {
		actions = append(actions, ActionRun)
	}
	st := iv.Inspection.Last.Status
	if st.Paused {
		actions = append(actions, ActionUnpause)
	} else {
		actions = append(actions, ActionPause)
		if len(RetryTargets(st, item)) > 0 {
			actions = append(actions, ActionRetry)
		}
	}
	return actions, ""
}

// notActionable says why nothing may act on iv, or "" when something may: a
// supported instance with a current status.
func notActionable(iv InstanceView) string {
	switch {
	case iv.Class == ClassMalformed:
		return "the directory is malformed, so its script is never run."
	case iv.Class == ClassLegacy:
		return "the script is an older Groundhog or lacks the runtime commands (read-only)."
	case iv.Class == ClassNotInspected:
		return "not until its first status read (select it, or choose Refresh)."
	case iv.Failing():
		return "not until a status can be read again."
	case iv.Health == HealthStale:
		return "the status shown is stale; it will be offered once it is read again."
	case iv.Class == ClassUnsupported:
		return "the script lacks what ticking, Retry and the pause need (read-only)."
	case iv.Class != ClassSupported || iv.Inspection.Last == nil:
		return "it is not known to be supported."
	}
	return ""
}
