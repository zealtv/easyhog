package tui

import (
	"context"
	"errors"
	"fmt"
	"path"
	"strings"
	"time"

	tea "github.com/charmbracelet/bubbletea"

	"github.com/zealtv/easyhog/internal/core"
	"github.com/zealtv/easyhog/internal/groundhog"
)

// Runners is what the palette's Run, Stop and settling actions need: core.Supervisor satisfies
// it. Both calls block until the supervisor has an outcome (Run can take
// several seconds), so they run only inside a command, never in Update or View.
//
// Settle is the settling actions' one call: a runner has to be stopped for
// `retry` and `recover` to run, so it takes as long as the runner's last call.
type Runners interface {
	Run(ctx context.Context, instanceKey string) (core.RunResult, error)
	Stop(ctx context.Context, instanceKey string) (core.StopResult, error)
	Settle(ctx context.Context, instanceKey string, req core.SettleRequest) (core.SettleResult, error)
	// SetItemPaused renames one schedule item's folder to pause or unpause it.
	SetItemPaused(ctx context.Context, instanceKey, item string, pause bool) (core.ItemResult, error)
}

// WithRunners sends the palette's Run, Stop and settling actions to r, on the
// instance the palette was bound to. While one runs, the status line shows
// "Running …", "Stopping …", "Retrying …" or "Recovering …" and the palette offers
// none of them for that instance again;
// then it shows the exact outcome. reread, if set, is told which instance was
// acted on once the call has returned, whatever its outcome, so the view can
// catch up with protocol status; it must return at once (Refresher.Reinspect).
func WithRunners(r Runners, reread func(instanceKey string)) Option {
	return func(m *Model) { m.runners, m.reread = r, reread }
}

// runnersHandler puts Run, Stop and the settling actions in front of inner.
func runnersHandler(inner ActionHandler, r Runners, reread func(string)) ActionHandler {
	return func(req ActionRequest) tea.Cmd {
		if !req.Action.acts() {
			return inner(req)
		}
		return func() tea.Msg {
			res := ResultMsg{Action: req.Action, InstanceKey: req.InstanceKey}
			if req.InstanceKey == "" {
				res.Err = errors.New("no instance is selected")
				return res
			}
			label := req.Label
			if label == "" {
				label = req.InstanceKey
			}
			// The supervisor ends this call itself when Quit begins.
			ctx := context.Background()
			switch {
			case req.Action == ActionRun:
				out, err := r.Run(ctx, req.InstanceKey)
				res.Message, res.Err = RunText(label, out), ActionFailure(label, err)
			case req.Action == ActionStop:
				out, err := r.Stop(ctx, req.InstanceKey)
				res.Message, res.Err = StopText(label, out), ActionFailure(label, err)
			case req.Action.togglesItem():
				out, err := r.SetItemPaused(ctx, req.InstanceKey, req.ItemPath, req.Action == ActionPauseItem)
				res.Message, res.Err = ItemText(label, out), ActionFailure(label, err)
			default:
				out, err := r.Settle(ctx, req.InstanceKey, core.SettleRequest{Action: core.Action(req.Action), Item: req.ItemPath})
				res.Message, res.Err = SettleText(label, out), ActionFailure(label, err)
			}
			if res.Err != nil {
				res.Message = ""
			}
			if reread != nil {
				reread(req.InstanceKey)
			}
			return res
		}
	}
}

// RunText is the notice for a Run that returned without an error. It says
// what happened and nothing it did not see.
func RunText(label string, r core.RunResult) string {
	enrolled, cleared := "", ""
	if r.Enrolled {
		enrolled = "enrolled for this session, "
	}
	if r.ClearedPause {
		cleared = "; a pause was cleared"
	}
	switch r.Outcome {
	case core.RunStarted:
		seen := "status has not shown it running yet; checking again"
		if r.Seen {
			seen = "status shows it running"
		}
		return fmt.Sprintf("%s: %srunner started (pid %d), %s%s", label, enrolled, r.PID, seen, cleared)
	case core.RunResumed:
		return fmt.Sprintf("%s: resumed; the runner was already running%s", label, cleared)
	case core.RunAlreadyRunning:
		return label + ": already running; nothing changed"
	}
	return label + ": done"
}

// StopText is the notice for a Stop that returned without an error: the runner
// has ended and the instance is released, as Quit leaves each one. A Stop that
// needed SIGTERM, or whose `stop` reported a problem, says so, in the words of
// core.StopResult.Why (which never carries an absolute path).
func StopText(label string, r core.StopResult) string {
	text := label + ": stopped and released. The runner has ended and nothing owns the instance now, so another supervisor may take it, and Run enrolls it for this session again; deliveries already pending stay pending"
	switch {
	case r.Terminated:
		why := r.Why
		if why == "" {
			why = "the runner lingering after stop"
		}
		text += fmt.Sprintf(". It needed SIGTERM (%s): a consumer call the runner was waiting on, if any, stays uncertain until it is recovered", why)
	case r.Why != "":
		text += ". Stop reported a problem on the way (" + r.Why + ")"
	}
	return text
}

// ItemText is the notice for a Pause item or Unpause item that went through: which
// folder was renamed to what, and what that does and does not change.
func ItemText(label string, r core.ItemResult) string {
	from, to := path.Base(r.From), path.Base(r.To)
	if r.Paused {
		return fmt.Sprintf("%s: %s paused: its folder %s was renamed to %s, so no tick fires it; Unpause item renames it back", label, r.Item, from, to)
	}
	return fmt.Sprintf("%s: %s unpaused: its folder %s was renamed to %s; it fires again at its next time unless a folder above it, or the instance, is still paused", label, r.Item, from, to)
}

// SettleText is the notice for a settling action that returned without an
// error: what was settled, and what became of the runner.
func SettleText(label string, r core.SettleResult) string {
	n := len(r.Settled)
	var what string
	switch r.Action {
	case core.ActionRetryFailed:
		what = fmt.Sprintf("%s set pending; the next attempt is made by the runner", plural(n, "failed delivery", "failed deliveries"))
	case core.ActionRecoverAccept:
		what = fmt.Sprintf("%s recorded as done on your say-so; nothing was invoked", plural(n, "uncertain delivery", "uncertain deliveries"))
	default:
		what = fmt.Sprintf("%s set pending to be tried again; nothing was invoked", plural(n, "uncertain delivery", "uncertain deliveries"))
	}
	runner := ""
	switch {
	case r.Restarted:
		seen := "status has not shown it running yet; checking again"
		if r.Seen {
			seen = "status shows it running"
		}
		runner = fmt.Sprintf("; the runner was stopped and restarted (pid %d), %s", r.PID, seen)
		if r.Paused {
			runner += ". The instance is still paused"
		}
	case r.StoppedRunner:
		runner = "; the runner was stopped and not restarted"
	default:
		runner = "; no runner was running, so none was started"
	}
	return label + ": " + what + runner
}

// ActionFailure words an error from Run, Stop or a settling action for the status line, naming
// the instance by its relative path (never the absolute one the error carries)
// and the reason as the supervisor gave it. A nil error gives nil.
func ActionFailure(label string, err error) error {
	if err == nil {
		return nil
	}
	var text string
	var refused *core.RefusedError
	var exited *core.RunnerExitedError
	var unc *groundhog.UncertainEnrollError
	var exit *groundhog.ExitError
	var settle *core.SettleError
	var stop *core.StopError
	switch {
	case errors.Is(err, core.ErrItemBusy):
		text = label + ": nothing was renamed: another operation holds the instance's lock; try again shortly"
	case errors.Is(err, core.ErrItemExists), errors.Is(err, core.ErrItemOutside), errors.Is(err, core.ErrItemState):
		// The detail after the sentinel's own words is the sentence for the reader.
		detail := err.Error()
		for _, e := range []error{core.ErrItemExists, core.ErrItemOutside, core.ErrItemState} {
			detail = strings.TrimPrefix(detail, e.Error()+": ")
		}
		text = label + ": nothing was renamed: " + detail
	case errors.As(err, &stop):
		text = label + ": not released: " + stop.Why + ". This session still holds the instance, so Stop can be tried again"
	case errors.As(err, &settle):
		text = label + ": " + settleFailure(settle)
	case errors.As(err, &refused):
		text = label + ": refused: " + refused.Reason.String()
		if refused.Detail != "" {
			text += ": " + refused.Detail
		}
	case errors.As(err, &exited):
		text = fmt.Sprintf("%s: the runner ended at startup (%s) and was not started again; log: %s", label, exited.Exit, exited.Log)
	case errors.As(err, &unc):
		text = label + ": the enroll ended without a certain outcome; the token is kept and nothing was started. The next Run, or the next status read, settles it"
	case errors.As(err, &exit) && exit.Kind == groundhog.KindBusy:
		text = label + ": still busy after retries (another operation holds the instance's lock); try again shortly"
	case errors.Is(err, context.Canceled), errors.Is(err, context.DeadlineExceeded):
		text = label + ": interrupted before it finished: " + err.Error()
	default:
		text = label + ": failed: " + err.Error()
	}
	return &ActionError{Text: text, Err: err}
}

// settleFailure words a settling action that did not finish, with what became
// of the deliveries and of the runner.
func settleFailure(e *core.SettleError) string {
	var b strings.Builder
	switch e.Stage {
	case core.SettleStopping:
		fmt.Fprintf(&b, "could not be settled yet: the runner had not stopped after %s, so nothing was settled", e.Waited.Round(time.Second))
	case core.SettleSettling:
		fmt.Fprintf(&b, "settled %d of %d, then failed", e.Done, e.Total)
	default:
		fmt.Fprintf(&b, "settled %d of %d", e.Done, e.Total)
	}
	if e.Cause != nil {
		b.WriteString(" (" + short(e.Cause.Error(), 200) + ")")
	}
	if e.Runner != "" {
		b.WriteString("; " + e.Runner)
	}
	return b.String()
}
