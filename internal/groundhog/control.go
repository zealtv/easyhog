package groundhog

import (
	"context"
	"fmt"
	"path"
	"strings"
)

// The commands that change an instance: tick, retry, pause and resume. None
// takes a token. Each refuses a script that negotiation found unsupported (it
// runs `capabilities --json` once if the client has not yet), so an older
// script is never asked to tick or retry.
//
// A call that returns a *CallError (timeout, cancellation) leaves the outcome
// unknown: refresh with Status and decide from the snapshot. Exit 4 (*ExitError
// with KindBusy) means try again later; the client does not retry on its own.

// Tick runs one `tick`: it fires what is due, then starts the run of every
// delivery whose record says fired and never started, each in a session of its
// own, and returns without waiting for them. Its human output is read and
// dropped, within the client's stdout bound. Exit 1 (*ExitError, KindFiringFailed)
// means an item could not be fired after the rest of the tick ran; the status's
// `firing_failed` diagnostic names it. A paused instance ticks to exit 0 and
// fires nothing.
func (c *Client) Tick(ctx context.Context, opts ...Option) error {
	if err := c.requireControl(ctx, "tick", opts); err != nil {
		return err
	}
	_, err := c.run(ctx, "tick", nil, false, opts)
	return err
}

// Retry runs `retry <delivery>`, which starts the delivery's run again now as a
// new attempt and returns once its wrapper has started. delivery is the
// delivery's path as status lists it ("out/<name>"). Exit 3 (KindPaused): the
// instance is paused. Exit 4 (KindBusy): the run is going, or being started.
// Exit 5 (KindNothing): no record Groundhog made, a delivery without a run, or no
// executable `run`.
func (c *Client) Retry(ctx context.Context, delivery string, opts ...Option) error {
	if !validDelivery(delivery) {
		return fmt.Errorf("%w: %q is not a delivery path under out/", ErrInvalidArgument, delivery)
	}
	if err := c.requireControl(ctx, "retry", opts); err != nil {
		return err
	}
	_, err := c.run(ctx, "retry", []string{delivery}, false, opts)
	return err
}

// Pause creates the instance pause (`runtime/paused`): every tick, from any
// ticker, then fires and starts nothing, and `retry` exits 3. Runs already
// going are left alone. It is idempotent, and when it returns no tick is in
// progress.
func (c *Client) Pause(ctx context.Context, opts ...Option) error {
	if err := c.requireControl(ctx, "pause", opts); err != nil {
		return err
	}
	_, err := c.run(ctx, "pause", nil, false, opts)
	return err
}

// Resume removes the instance pause. It is idempotent and starts nothing.
func (c *Client) Resume(ctx context.Context, opts ...Option) error {
	if err := c.requireControl(ctx, "resume", opts); err != nil {
		return err
	}
	_, err := c.run(ctx, "resume", nil, false, opts)
	return err
}

// validDelivery reports whether d is "out/<name>": one clean name directly
// under out/, not hidden, not a landing copy. The script checks again.
func validDelivery(d string) bool {
	name, ok := strings.CutPrefix(d, "out/")
	return ok && name != "" && path.Clean(d) == d && !strings.ContainsAny(name, "/\x00") &&
		!strings.HasPrefix(name, ".") && !strings.HasSuffix(name, ".landing")
}
