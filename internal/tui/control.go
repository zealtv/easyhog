package tui

import (
	"context"
	"errors"
	"fmt"
	"path"
	"strings"

	tea "github.com/charmbracelet/bubbletea"

	"github.com/zealtv/easyhog/internal/core"
	"github.com/zealtv/easyhog/internal/groundhog"
)

// Control is what the palette's actions on an instance need: core.Ticker
// satisfies it. The calls run a script and wait for it, so they run only inside
// a command, never in Update or View.
type Control interface {
	// Run starts this session ticking the instance; it returns after the first tick.
	Run(ctx context.Context, instanceKey string) (core.RunResult, error)
	// Stop ends this session's ticking at once.
	Stop(instanceKey string) core.StopResult
	// Retry starts the failed and interrupted deliveries' runs again, of one
	// schedule item when item is its path.
	Retry(ctx context.Context, instanceKey, item string) (core.RetryResult, error)
	// SetPaused sets or lifts the instance pause.
	SetPaused(ctx context.Context, instanceKey string, pause bool) error
	// SetItemPaused renames one schedule item's folder to pause or unpause it.
	SetItemPaused(ctx context.Context, instanceKey, item string, pause bool) (core.ItemResult, error)
}

// WithControl sends the palette's actions on an instance to c, on the instance
// the palette was bound to. While one runs, the status line shows "Running …",
// "Stopping …", "Retrying …" and so on, and the palette offers none of them for
// that instance again; then it shows the exact outcome. reread, if set, is told
// which instance was acted on once the call has returned, whatever its outcome,
// so the view can catch up with protocol status; it must return at once
// (Refresher.Reinspect).
func WithControl(c Control, reread func(instanceKey string)) Option {
	return func(m *Model) { m.control, m.reread = c, reread }
}

// controlHandler puts the actions on an instance in front of inner.
func controlHandler(inner ActionHandler, c Control, reread func(string)) ActionHandler {
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
			// The ticker refuses a new action itself once Quit has begun.
			ctx := context.Background()
			var err error
			switch {
			case req.Action == ActionRun:
				var out core.RunResult
				out, err = c.Run(ctx, req.InstanceKey)
				res.Message = RunText(label, out)
			case req.Action == ActionStop:
				res.Message = StopText(label, c.Stop(req.InstanceKey))
			case req.Action == ActionPause || req.Action == ActionUnpause:
				pause := req.Action == ActionPause
				err = c.SetPaused(ctx, req.InstanceKey, pause)
				res.Message = PauseText(label, pause)
			case req.Action == ActionRetry:
				var out core.RetryResult
				out, err = c.Retry(ctx, req.InstanceKey, req.ItemPath)
				res.Message = RetryText(label, out)
			default:
				var out core.ItemResult
				out, err = c.SetItemPaused(ctx, req.InstanceKey, req.ItemPath, req.Action == ActionPauseItem)
				res.Message = ItemText(label, out)
			}
			if res.Err = ActionFailure(label, err); res.Err != nil {
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
	switch {
	case r.Already:
		return label + ": already ticked by this session; nothing changed"
	case !r.Ticked:
		return label + ": ticking every minute; the first tick has not finished yet"
	case r.TickErr != nil:
		return label + ": ticking every minute; the first tick reported a problem: " + short(r.TickErr.Error(), 200)
	}
	return label + ": ticked, and ticking every minute while Easyhog runs"
}

// StopText is the notice for a Stop.
func StopText(label string, r core.StopResult) string {
	if !r.WasTicking {
		return label + ": this session was not ticking it; nothing changed"
	}
	return label + ": stopped ticking. Runs already started go on in their own sessions, and anything else that ticks it still does"
}

// PauseText is the notice for Pause instance or Unpause instance that went through.
func PauseText(label string, pause bool) string {
	if pause {
		return label + ": paused. No tick, from here or anywhere else, fires an item or starts a run until Unpause instance; runs already going carry on"
	}
	return label + ": unpaused. The next tick fires what is due and starts what waits"
}

// RetryText is the notice for a Retry that returned without an error: which
// runs started again, and which were busy or had nothing to run.
func RetryText(label string, r core.RetryResult) string {
	var parts []string
	if n := len(r.Started); n > 0 {
		parts = append(parts, plural(n, "run", "runs")+" started again ("+short(strings.Join(r.Started, ", "), 120)+")")
	}
	if n := len(r.Busy); n > 0 {
		parts = append(parts, "busy: nothing was started for "+short(strings.Join(r.Busy, ", "), 120)+" (its run is going, or the instance's lock was held); try again shortly")
	}
	if n := len(r.Gone); n > 0 {
		parts = append(parts, "nothing to run for "+short(strings.Join(r.Gone, ", "), 120))
	}
	for _, f := range r.Failed {
		parts = append(parts, "could not start "+short(f.Delivery, 60)+" ("+short(f.Err.Error(), 120)+")")
	}
	if len(parts) == 0 {
		return label + ": nothing to retry"
	}
	return label + ": " + strings.Join(parts, "; ")
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

// ActionFailure words an error from an action for the status line, naming the
// instance by its relative path (never the absolute one the error carries) and
// the reason as the ticker or the script gave it. A nil error gives nil.
func ActionFailure(label string, err error) error {
	if err == nil {
		return nil
	}
	var text string
	var refused *core.RefusedError
	var exit *groundhog.ExitError
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
	case errors.As(err, &refused):
		text = label + ": refused: " + refused.Reason.String()
		if refused.Detail != "" {
			text += ": " + refused.Detail
		}
	case errors.As(err, &exit) && exit.Kind == groundhog.KindBusy:
		text = label + ": busy (another writer holds the instance's lock); try again shortly"
	case errors.Is(err, context.Canceled), errors.Is(err, context.DeadlineExceeded):
		text = label + ": interrupted before it finished: " + err.Error()
	default:
		text = label + ": failed: " + err.Error()
	}
	return &ActionError{Text: text, Err: err}
}
