package core

import "github.com/zealtv/easyhog/internal/groundhog"

// Action is something the reader can ask Easyhog to do to one instance's
// runner or deliveries. Open folder and Refresh are not here: they touch no
// runner and are always available.
type Action string

const (
	ActionRun  Action = "run"
	ActionStop Action = "stop"
	// The three settling actions are the reader's explicit choice about
	// deliveries Groundhog cannot decide for itself (`retry`, `recover`). Each
	// needs a stopped runner, so Supervisor.Settle stops this session's runner,
	// settles, and starts it again if it was running.
	ActionRetryFailed   Action = "retry-failed"   // failed deliveries become pending
	ActionRecoverAccept Action = "recover-accept" // uncertain deliveries are recorded accepted
	ActionRecoverRetry  Action = "recover-retry"  // uncertain deliveries become pending
)

// Settles reports whether a is one of the three settling actions.
func (a Action) Settles() bool {
	return a == ActionRetryFailed || a == ActionRecoverAccept || a == ActionRecoverRetry
}

// SettleTargets lists the delivery ids a settling action would act on in st:
// `failed` ones for Retry failed, `uncertain` ones for either Recover. item is a
// schedule item's path ("schedule/daily/x") to limit it to that item's
// deliveries, or "" for every delivery of the instance. A delivery with no id (an
// unregistered legacy out/ entry) cannot be settled and is left out. It needs a
// status that lists failed and uncertain deliveries in full, which every read
// does (an --active read leaves out only accepted and missing ones).
func SettleTargets(st groundhog.Status, a Action, item string) []string {
	var want groundhog.DeliveryState
	switch a {
	case ActionRetryFailed:
		want = groundhog.DeliveryFailed
	case ActionRecoverAccept, ActionRecoverRetry:
		want = groundhog.DeliveryUncertain
	default:
		return nil
	}
	var ids []string
	for _, d := range st.Deliveries {
		if d.State != want || d.ID == nil {
			continue
		}
		if item != "" && (d.SchedulePath == nil || *d.SchedulePath != item) {
			continue
		}
		ids = append(ids, *d.ID)
	}
	return ids
}

// Offers says which of Run and Stop an instance offers (Run first, then Stop), and when it offers
// neither, why. It is the one rule: the palette lists what it returns, and the
// supervisor checks the same rule when Run or a settling action runs. Stop reads no
// status first: it needs only a token this session holds, and a `stop` the script
// refuses as an ownership conflict applies the ownership rule (Supervisor.Stop). Control follows
// ownership and the protocol's status, never the host application:
//
//   - Run: an unowned, controllable instance; or one of this session's (a token
//     of this session's matches its owner: the ownership rule) whose runner is
//     not running, or is paused (Run clears the pause).
//     Also one of this session's whose runner this session saw end
//     (PhaseExited), whatever status says the runner state is: a runner killed
//     by a signal reads as "unknown". And one whose runner reads "unknown"
//     while this session holds the token and has no live child (an adopted
//     instance whose runner died with its session).
//   - Stop: any instance of this session's whose runner is not stopping and is not
//     "unknown" under a live child of this session's: beside Run where the runner is
//     not running or is paused, alone where it runs. It stops the runner and releases
//     the instance, as Quit does for each (Supervisor.Stop), so it is also the way to let
//     go of an instance whose runner has ended.
//   - Also Run: an unowned instance whose status is incoherent only because a
//     delivery journal write was interrupted (SettlesOnRun). `enroll` takes the
//     mutation lock, which settles the journal generation, and the read after it
//     must then be coherent.
//   - Retry failed, Recover as done and Recover and retry, after whichever of
//     Run and Stop is offered, for an instance of this session's (a token of
//     this session's matches its owner; never for an unowned or externally owned
//     one) whose status lists failed deliveries (Retry failed) or uncertain ones
//     (both Recovers). Retry and recover need a stopped runner, so the supervisor
//     stops this session's runner, settles, and starts it again if it was running.
//     An unowned instance with such deliveries offers Run first: after
//     Run it is this session's, and the settling actions follow.
//   - Neither: not inspected yet (its first status read, which selecting the
//     instance or Refresh triggers, decides), malformed, legacy, unsupported,
//     externally owned, failing, stale or inconsistent. A stale or inconsistent
//     status is not something to act on.
//
// runner is what the supervisor holds for the instance, nil when this session
// never enrolled it. why is a sentence fragment for the reader, empty when
// something is offered. Offers is pure: it reads only its arguments.
func Offers(iv InstanceView, runner *RunnerInfo) (actions []Action, why string) {
	return OffersOn(iv, runner, "")
}

// OffersOn is Offers for the instance, or for one of its schedule items when
// item is that item's path ("schedule/daily/x"): the settling actions then count
// only that item's deliveries. Run and Stop act on the instance either way.
func OffersOn(iv InstanceView, runner *RunnerInfo, item string) (actions []Action, why string) {
	actions, why = baseOffers(iv, runner)
	if len(actions) == 0 || iv.Class != ClassOwnedBySession || iv.Inspection.Last == nil {
		return actions, why
	}
	st := iv.Inspection.Last.Status
	for _, a := range []Action{ActionRetryFailed, ActionRecoverAccept, ActionRecoverRetry} {
		if len(SettleTargets(st, a, item)) > 0 {
			actions = append(actions, a)
		}
	}
	return actions, why
}

// baseOffers is the rule for Run and Stop alone.
func baseOffers(iv InstanceView, runner *RunnerInfo) (actions []Action, why string) {
	switch {
	case iv.Class == ClassMalformed:
		return nil, "the directory is malformed, so its script is never run."
	case iv.Class == ClassLegacy:
		return nil, "the script lacks the runtime commands (read-only)."
	case iv.Class == ClassNotInspected:
		return nil, "not until its first status read shows it unowned and controllable (select it, or choose Refresh)."
	case iv.Failing():
		return nil, "not until a status can be read again."
	case iv.Health == HealthInconsistent && !runSettles(iv):
		return nil, "its status is not coherent; it will be offered once a coherent read arrives."
	case iv.Health == HealthStale:
		return nil, "the status shown is stale; it will be offered once it is read again."
	case iv.Class == ClassExternal:
		return nil, "another supervisor owns it, running or not; Easyhog only inspects it."
	case iv.Class == ClassUnsupported:
		return nil, "the script lacks the capabilities supervised control needs."
	case iv.Class == ClassUnowned:
		return []Action{ActionRun}, ""
	case iv.Class != ClassOwnedBySession || iv.Inspection.Last == nil:
		return nil, "it is not known to be controllable."
	}
	// This session holds the instance's token, so Stop (let it go) is always there
	// to offer, beside Run wherever the runner is not running: Stop on an instance
	// with no runner releases it, which is the way out after a Stop that ended the
	// runner and could not release, or after a runner that crashed.
	both := []Action{ActionRun, ActionStop}
	if runner != nil && runner.Phase == PhaseExited {
		// This session saw its runner end. A runner killed by a signal skips
		// its exit trap, so status reads "unknown" rather than "stopped"; the
		// supervisor starts a runner whenever its own child is not live.
		return both, ""
	}
	st := iv.Inspection.Last.Status
	switch st.Runner.State {
	case groundhog.RunnerRunning:
		if st.Paused {
			return both, "" // Run clears the pause; Stop releases the instance, pause and all
		}
		return []Action{ActionStop}, ""
	case groundhog.RunnerStopped, groundhog.RunnerFailed:
		return both, ""
	case groundhog.RunnerUnknown:
		// The runner of a session that died without its exit trap (a power cut,
		// a kill -9 on both) reads "unknown", and an interrupted journal write
		// can too. This session holds the token and has no child of its own
		// alive, so Run starts one, as bringUp does for a stopped runner; `run`
		// exits 4 if a runner does live, and Run then reports that.
		if runner != nil && runner.Phase != PhaseRunning {
			return both, ""
		}
	}
	return nil, "its runner is " + string(st.Runner.State) + "."
}

// runSettles reports whether iv is an unowned instance whose newest status is
// incoherent only for a journal write Run settles.
func runSettles(iv InstanceView) bool {
	return iv.Class == ClassUnowned && iv.Inspection.Last != nil && SettlesOnRun(iv.Inspection.Last.Status)
}

// harmlessDiagnostics lists the diagnostic codes of the pinned Groundhog script
// (`groundhog.sh`, status) that never make a status `consistent:false` by
// themselves. Every other code, including one this list has not met, may be the
// reason a status is incoherent. The list errs towards refusing.
var harmlessDiagnostics = map[string]bool{
	"out_collision":               true,
	"runner_not_observed":         true,
	"delivery_path_missing":       true,
	"out_entry_not_directory":     true,
	"out_entry_name_not_utf8":     true,
	"firing_reconciliation_error": true,
	"schedule_unrecognized_path":  true,
	"schedule_name_not_utf8":      true,
	"path_not_utf8":               true,
	"consumer_config_unreadable":  true,
	"consumer_config_invalid":     true,
}

// journalWriteInProgress is the diagnostic code for an odd journal generation:
// a delivery journal write is in progress, or one did not finish.
const journalWriteInProgress = "journal_write_in_progress"

// SettlesOnRun reports whether a status is incoherent only because a delivery
// journal write did not finish, which the next control command or runner start
// settles (as of Groundhog f189ad2, `status` never does). It holds when the
// status is unowned, says `consistent:false`, carries `journal_write_in_progress`,
// and carries no other diagnostic that could make it incoherent. Run's `enroll`
// takes the mutation lock, which settles it; a coherent read must follow.
func SettlesOnRun(st groundhog.Status) bool {
	if st.Owner != nil || st.Consistent {
		return false
	}
	found := false
	for _, d := range st.Diagnostics {
		switch {
		case d.Code == journalWriteInProgress:
			found = true
		case !harmlessDiagnostics[d.Code]:
			return false
		}
	}
	return found
}
