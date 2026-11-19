package groundhog

import (
	"context"
	"errors"
	"fmt"
	"path/filepath"
	"strings"
)

// Control commands, token rules and outcomes.
//
// Every wrapper passes the token only as `--token-file PATH`. The client never
// opens that file, so a token's contents cannot reach argv, a log or an error.
// Each wrapper refuses a script that negotiation found unsupported (it runs
// `capabilities --json` once if the client has not yet), so a legacy script is
// never asked to enroll, pause, resume, stop or release.
//
// A control call that returns a *CallError (timeout, cancellation) leaves the
// outcome unknown: the command may or may not have committed. Refresh with
// Status and decide from the snapshot. Exit 4 (*ExitError with KindBusy) is
// retryable; the client does not retry on its own.
//
// Retry and Recover settle one delivery each. Both need the token and a stopped
// runner (exit 4 while one lives; the caller stops it first, and starts it
// again if it wants), and both are the reader's explicit choice: Groundhog
// cannot know what a consumer did.

// Enroll assigns ownership of the instance to a new session and writes its
// private token to tokenFile, which must be an absolute path that does not yet
// exist, outside the instance, in an existing private directory. The label is
// display text and grants nothing.
//
// A nil error means the script exited 0. Exits 2, 3, 4, 5 and 6 are an
// *ExitError and, by the contract, grant no ownership (an unused token file may
// remain; remove it before another attempt). Anything else is an
// *UncertainEnrollError: exit 7, a timeout or cancellation, a signal death or an
// exit the contract does not list. Then the token file may or may not exist and
// ownership may or may not have been committed. Keep the token, start nothing,
// and reconcile with Status against the token's nonsecret metadata.
func (c *Client) Enroll(ctx context.Context, ownerLabel, tokenFile string, opts ...Option) error {
	if err := validateLabel(ownerLabel); err != nil {
		return err
	}
	if err := checkPath("token file", tokenFile); err != nil {
		return err
	}
	if inside(c.instance, tokenFile) {
		return fmt.Errorf("%w: token file must be outside the instance", ErrInvalidArgument)
	}
	if err := c.requireControl(ctx, "enroll", opts); err != nil {
		return err
	}
	_, err := c.run(ctx, "enroll", []string{"--owner-label", ownerLabel, "--token-file", tokenFile}, false, opts)
	if err == nil {
		return nil
	}
	var ee *ExitError
	if errors.As(err, &ee) {
		switch ee.Code {
		case 2, 3, 4, 5, 6:
			return err // the contract guarantees no owner was committed
		}
	}
	// Everything else, including a script stopped for flooding stdout, may have
	// committed before it ended.
	return &UncertainEnrollError{TokenFile: tokenFile, Cause: err}
}

// Pause persists the instance-wide pause. It is idempotent. Easyhog's supervisor
// no longer calls it (Run clears a pause, Stop releases the instance); the tests
// use it to stand in for a pause set outside Easyhog.
func (c *Client) Pause(ctx context.Context, tokenFile string, opts ...Option) error {
	return c.tokenCommand(ctx, "pause", tokenFile, opts)
}

// Resume clears the instance-wide pause. It starts no runner and enrolls
// nothing. It is idempotent.
func (c *Client) Resume(ctx context.Context, tokenFile string, opts ...Option) error {
	return c.tokenCommand(ctx, "resume", tokenFile, opts)
}

// Stop asks the runner to close admission and waits until it has released its
// lock. Ownership and pause stay. It is idempotent, and with no live runner it
// returns at once. Since Groundhog f189ad2 the runner finishes the consumer call
// it already admitted and records the result before it exits, so a stop during a
// long call takes as long as the call. Upstream puts no time limit on the wait,
// so the call is bounded by DefaultStopTimeout unless the caller sets one (a
// supervisor that gives up sends the runner SIGTERM, which detaches at once and
// leaves the delivery uncertain; the bound only ends the `stop` process).
func (c *Client) Stop(ctx context.Context, tokenFile string, opts ...Option) error {
	return c.tokenCommand(ctx, "stop", tokenFile, opts)
}

// Release ends ownership and invalidates the token. It needs a stopped runner
// (exit 4 while one is live, which is retryable after Stop) and no unresolved
// delivery attempt (exit 5). Pause and delivery records stay. After success the
// caller deletes the token file.
func (c *Client) Release(ctx context.Context, tokenFile string, opts ...Option) error {
	return c.tokenCommand(ctx, "release", tokenFile, opts)
}

// ReleaseLeavingUnresolved is Release for a supervisor that is leaving: it
// passes `--leave-unresolved` when the script advertises release-unresolved-v1,
// so an `uncertain` or `in_flight` attempt no longer blocks the release (the
// delivery keeps its record, and the instance-wide barrier stays until someone
// records `recover` under a later owner). A script without the capability gets a
// plain release and its older refusal (exit 5). The caller needs to have
// negotiated, which the control wrappers do on first use.
func (c *Client) ReleaseLeavingUnresolved(ctx context.Context, tokenFile string, opts ...Option) error {
	if err := checkPath("token file", tokenFile); err != nil {
		return err
	}
	if err := c.requireControl(ctx, "release", opts); err != nil {
		return err
	}
	args := []string{"--token-file", tokenFile}
	if c.offers(CapReleaseUnresolved) {
		args = append(args, "--leave-unresolved")
	}
	_, err := c.run(ctx, "release", args, false, opts)
	return err
}

// Decision is the reader's choice for an `uncertain` delivery.
type Decision string

const (
	// DecisionAccept records the delivery as `accepted` on the reader's say-so.
	// Nothing is invoked.
	DecisionAccept Decision = "accept"
	// DecisionRetry makes the delivery `pending`, so the next admission makes a
	// new attempt.
	DecisionRetry Decision = "retry"
)

// validDeliveryID is the script's own rule for `--delivery` (a letter or digit,
// then letters, digits, dot, underscore or dash); a wrong shape is refused here,
// before a process starts.
func validDeliveryID(id string) bool {
	if id == "" {
		return false
	}
	for i, r := range id {
		switch {
		case r >= 'a' && r <= 'z', r >= 'A' && r <= 'Z', r >= '0' && r <= '9':
		case i > 0 && (r == '.' || r == '_' || r == '-'):
		default:
			return false
		}
	}
	return true
}

// Retry turns one `failed` delivery `pending` (same delivery ID, history kept;
// the next admission makes a new attempt). A `pending` delivery stays as it is.
// Exit 4 is a live runner or a busy lock (retryable), exit 5 a delivery that is
// in neither state or has no record. If the failed handoff might have had side
// effects, that is for the reader to have reconciled first.
func (c *Client) Retry(ctx context.Context, tokenFile, deliveryID string, opts ...Option) error {
	return c.deliveryCommand(ctx, "retry", tokenFile, deliveryID, "", opts)
}

// Recover settles one `uncertain` delivery, on the reader's decision: accept it
// (recorded `accepted`) or retry it (`pending`). It invokes no consumer. The
// reader first establishes by their own means that the original consumer call
// has finished. Exit codes as Retry; exit 5 also for a delivery that is not
// unresolved.
func (c *Client) Recover(ctx context.Context, tokenFile, deliveryID string, decision Decision, opts ...Option) error {
	if decision != DecisionAccept && decision != DecisionRetry {
		return fmt.Errorf("%w: decision must be accept or retry", ErrInvalidArgument)
	}
	return c.deliveryCommand(ctx, "recover", tokenFile, deliveryID, decision, opts)
}

func (c *Client) deliveryCommand(ctx context.Context, command, tokenFile, deliveryID string, decision Decision, opts []Option) error {
	if err := checkPath("token file", tokenFile); err != nil {
		return err
	}
	if !validDeliveryID(deliveryID) {
		return fmt.Errorf("%w: delivery id %q is not a delivery id", ErrInvalidArgument, deliveryID)
	}
	if err := c.requireControl(ctx, command, opts); err != nil {
		return err
	}
	args := []string{"--token-file", tokenFile, "--delivery", deliveryID}
	if decision != "" {
		args = append(args, "--decision", string(decision))
	}
	_, err := c.run(ctx, command, args, false, opts)
	return err
}

func (c *Client) tokenCommand(ctx context.Context, command, tokenFile string, opts []Option) error {
	if err := checkPath("token file", tokenFile); err != nil {
		return err
	}
	if err := c.requireControl(ctx, command, opts); err != nil {
		return err
	}
	_, err := c.run(ctx, command, []string{"--token-file", tokenFile}, false, opts)
	return err
}

func validateLabel(label string) error {
	if strings.TrimSpace(label) == "" {
		return fmt.Errorf("%w: owner label is empty", ErrInvalidArgument)
	}
	for _, r := range label {
		if r < 0x20 || r == 0x7f {
			return fmt.Errorf("%w: owner label contains a control character", ErrInvalidArgument)
		}
	}
	return nil
}

// inside reports whether path is dir or lies below it, lexically.
func inside(dir, path string) bool {
	rel, err := filepath.Rel(dir, filepath.Clean(path))
	return err == nil && rel != ".." && !strings.HasPrefix(rel, ".."+string(filepath.Separator))
}
