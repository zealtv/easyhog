package core

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"time"

	"github.com/zealtv/easyhog/internal/groundhog"
)

// SettleRequest says which settling action to run and where.
type SettleRequest struct {
	// Action is ActionRetryFailed, ActionRecoverAccept or ActionRecoverRetry.
	Action Action
	// Item is the schedule item's path ("schedule/daily/x") to settle only that
	// item's deliveries, or "" for every delivery of the instance.
	Item string
}

// SettleResult is what a Settle that went through did.
type SettleResult struct {
	Instance string
	Action   Action
	// Settled lists the delivery ids that were settled.
	Settled []string
	// StoppedRunner is true when this call stopped a running runner to settle.
	StoppedRunner bool
	// Restarted is true when it then started the runner again (it was running
	// before). PID and Seen describe that new runner, as RunResult's do. A
	// paused instance restarts paused: the persistent pause is never touched.
	Restarted bool
	PID       int
	Seen      bool
	Paused    bool // the instance was paused, so a restarted runner ticks nothing
}

// SettleStage says how far a Settle got when it failed.
type SettleStage int

const (
	// SettleStopping: the runner would not stop in time. Nothing was settled.
	SettleStopping SettleStage = iota + 1
	// SettleSettling: a retry or recover call failed. Some earlier ones may have
	// been settled.
	SettleSettling
	// SettleRestarting: the deliveries were settled and the runner would not start again.
	SettleRestarting
)

// SettleError reports a Settle that did not finish. Done of Total deliveries
// were settled; the runner's state is in Runner, a sentence for the reader.
type SettleError struct {
	Instance string
	Action   Action
	Stage    SettleStage
	Done     int
	Total    int
	// Waited is how long the stop waited, for SettleStopping.
	Waited time.Duration
	// Runner says what became of the runner.
	Runner string
	Cause  error
}

func (e *SettleError) Error() string {
	var b strings.Builder
	switch e.Stage {
	case SettleStopping:
		fmt.Fprintf(&b, "settle %s: nothing was settled: the runner had not stopped after %s", e.Instance, e.Waited.Round(time.Second))
	case SettleSettling:
		fmt.Fprintf(&b, "settle %s: settled %d of %d deliveries, then failed", e.Instance, e.Done, e.Total)
	default:
		fmt.Fprintf(&b, "settle %s: settled %d of %d deliveries", e.Instance, e.Done, e.Total)
	}
	if e.Cause != nil {
		b.WriteString(" (" + e.Cause.Error() + ")")
	}
	if e.Runner != "" {
		b.WriteString("; " + e.Runner)
	}
	return b.String()
}

func (e *SettleError) Unwrap() error { return e.Cause }

// Settle is the reader's explicit act on deliveries that Groundhog will not
// decide for itself: Retry failed turns failed deliveries pending (`retry`),
// Recover as done records uncertain ones accepted, and Recover and retry makes
// them pending (`recover --decision accept|retry`). Easyhog cannot know what a
// consumer did; the reader chose.
//
// Both calls need a stopped runner (exit 4 while one lives), so Settle, on an
// instance whose token this session holds (the ownership rule, as Offers
// applies it to the fresh read):
//
//  1. stops this session's runner, if it is running, with `stop` bounded by
//     QuitWait. `stop` waits for the consumer call the runner admitted, so a
//     long call makes this wait. Past the bound Settle SIGTERMs nothing: it
//     returns a *SettleError (SettleStopping) saying nothing was settled yet.
//     The stop request stays recorded, so the runner exits when its call has
//     ended; then settle again, or Run;
//  2. settles every matching delivery of the fresh read, one `retry` or
//     `recover` each (exit 4 retried a few times), and stops at the first failure;
//  3. starts the runner again if it was running before. The persistent pause is
//     never touched, so a paused instance restarts paused. Nothing is started if
//     the session is quitting (Quit then releases the instance), or if the
//     runner was not running (an exited or never started runner stays so: Run
//     is the reader's act).
//
// An adopted instance's runner is stopped through the token like any other and
// restarted as this session's own. A runner started again is not watched for
// longer than Run watches one.
func (s *Supervisor) Settle(ctx context.Context, key string, req SettleRequest) (SettleResult, error) {
	res := SettleResult{Instance: key, Action: req.Action}
	if !req.Action.Settles() {
		return res, &RefusedError{Op: "settle", Instance: key, Reason: RefusedNotOffered, Detail: fmt.Sprintf("%q is not a settling action", req.Action)}
	}
	ctx, done := s.opContext(ctx)
	defer done()
	sess, err := s.sessionFor("settle", key)
	if err != nil {
		return res, err
	}
	if err := sess.acquire(ctx); err != nil {
		return res, err
	}
	defer sess.release()
	if err := s.closingErr("settle", key); err != nil {
		return res, err
	}
	s.mu.Lock()
	held := sess.holds()
	s.mu.Unlock()
	if !held {
		return res, &RefusedError{Op: "settle", Instance: key, Reason: RefusedNotOurs,
			Detail: "this session holds no token for it; Run it first, and then it can be settled"}
	}

	c := sess.client
	st, err := s.readStatus(ctx, c)
	if err != nil {
		return res, refuseIfInconsistent("settle", key, err)
	}
	if !s.applyRule(sess, st) {
		if st.Owner == nil {
			return res, &RefusedError{Op: "settle", Instance: key, Reason: RefusedNotOurs,
				Detail: "no owner is recorded, so this session's token controlled nothing and was removed; Run it first"}
		}
		return res, &RefusedError{Op: "settle", Instance: key, Reason: RefusedExternal, Detail: "ownership changed; this session's token no longer controls it and was removed"}
	}
	if err := s.checkOfferedAction("settle", sess, st, nil, req.Action, req.Item); err != nil {
		return res, err
	}
	// checkOfferedAction passed, so OffersOn listed req.Action for this very read
	// and item, and it lists a settling action only when SettleTargets finds
	// something: targets is never empty.
	targets := SettleTargets(st, req.Action, req.Item)
	res.Paused = st.Paused

	s.mu.Lock()
	token, child := sess.token, sess.child
	s.mu.Unlock()
	wasRunning := s.runnerLive(sess) || s.adoptedRunnerLives(sess, st)
	fail := func(stage SettleStage, cause error, runner string) error {
		return &SettleError{Instance: key, Action: req.Action, Stage: stage, Done: len(res.Settled), Total: len(targets), Runner: runner, Cause: cause}
	}

	// A runner in a consumer call would make `stop` wait out the bound, and the
	// script has no command to withdraw a stop request: the runner would exit when
	// the call ended and nothing would start it again. So refuse before `stop`,
	// and the runner keeps running. A call admitted
	// between this read and the stop takes the path below.
	if wasRunning && hasInFlight(st) {
		return res, &RefusedError{Op: "settle", Instance: key, Reason: RefusedNotOffered,
			Detail: "the runner is in a consumer call; settle once it ends (nothing was stopped, and the runner keeps running)"}
	}

	// 1. Stop the runner, never SIGTERM it: a call it admitted is not ours to detach.
	if wasRunning {
		s.mu.Lock()
		sess.asked = true // its exit is no crash
		s.mu.Unlock()
		began := time.Now()
		sctx, cancel := context.WithTimeout(ctx, s.cfg.QuitWait)
		stopErr := s.retryBusy(sctx, func() error { return c.Stop(sctx, token) })
		cancel()
		if stopErr != nil {
			timedOut := errors.Is(stopErr, context.DeadlineExceeded)
			if !timedOut {
				s.mu.Lock()
				sess.asked = false // the stop may never have been recorded
				s.mu.Unlock()
			}
			e := &SettleError{Instance: key, Action: req.Action, Stage: SettleStopping, Total: len(targets), Waited: time.Since(began),
				Runner: "the runner is still running and was not signalled; settle again once it has ended", Cause: stopErr}
			if timedOut {
				e.Cause = nil
				e.Runner = "the runner is still finishing a consumer call it admitted and was not signalled; its stop is requested, so it exits when the call ends. Settle again then"
			}
			return res, e
		}
		if child != nil {
			select {
			case <-child.Done():
			case <-time.After(s.cfg.ReapWait):
				return res, fail(SettleStopping, errors.New("stop returned but the runner process has not ended"), "the runner process is still alive and was not signalled")
			}
		}
		res.StoppedRunner = true
	}

	// 2. Settle, one delivery at a time, stopping at the first failure.
	var settleErr error
	for _, id := range targets {
		call := func() error {
			switch req.Action {
			case ActionRetryFailed:
				return c.Retry(ctx, token, id)
			case ActionRecoverAccept:
				return c.Recover(ctx, token, id, groundhog.DecisionAccept)
			}
			return c.Recover(ctx, token, id, groundhog.DecisionRetry)
		}
		if settleErr = s.retryBusy(ctx, call); settleErr != nil {
			break
		}
		res.Settled = append(res.Settled, id)
	}

	// 3. Start the runner again if it was running, whatever the settling did: the
	// reader asked for deliveries to be settled, not for the instance to stop.
	var restartErr error
	if res.StoppedRunner && s.closingErr("settle", key) == nil && ctx.Err() == nil {
		var pr RunResult
		pr, restartErr = s.startRunner(ctx, sess, RunResult{Instance: key})
		res.Restarted, res.PID, res.Seen = restartErr == nil, pr.PID, pr.Seen
	}
	runnerNote := ""
	switch {
	case res.StoppedRunner && res.Restarted:
		runnerNote = "the runner was stopped and restarted"
	case res.StoppedRunner:
		runnerNote = "the runner was stopped and was not restarted"
		if restartErr != nil {
			runnerNote += ": " + restartErr.Error()
		}
	}
	switch {
	case settleErr != nil:
		return res, fail(SettleSettling, settleErr, runnerNote)
	case restartErr != nil:
		return res, fail(SettleRestarting, restartErr, runnerNote)
	}
	return res, nil
}

// hasInFlight reports whether st lists a delivery in a consumer call.
func hasInFlight(st groundhog.Status) bool {
	for _, d := range st.Deliveries {
		if d.State == groundhog.DeliveryInFlight {
			return true
		}
	}
	return false
}
