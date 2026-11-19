package core

import (
	"context"
	"fmt"
	"time"
)

// StopResult is what a Stop that went through did: the runner has ended, the
// instance is released and its token deleted, as Quit leaves each instance.
type StopResult struct {
	Instance string
	// Waited is how long `stop` took: the time the runner needed to finish the
	// consumer call it had admitted, if any, and exit.
	Waited time.Duration
	// Terminated is true when SIGTERM was sent: `stop` did not finish within
	// QuitWait (or failed), or the process lingered. A consumer call the runner
	// was waiting on was then detached, and its delivery stays `uncertain` until
	// someone records `recover`.
	Terminated bool
	// Why says what marred a stop that still released, empty for a clean one.
	Why string
}

// StopError reports a Stop that could not finish the sequence. The runner may
// still run (RunnerGone says), the instance stays owned by this session's
// token, and the session still holds that token: Stop can be tried again, and so
// can Run, a settling action or Quit.
type StopError struct {
	Instance   string
	Why        string
	RunnerGone bool
}

func (e *StopError) Error() string {
	return fmt.Sprintf("stop %s: not released: %s", e.Instance, e.Why)
}

// Stop does for one instance what Quit does for each (it is Quit's own sequence,
// windDown): `stop`, bounded by QuitWait, which waits for the consumer call the
// runner admitted and lets it exit; SIGTERM, then a reap, if `stop` did not
// finish; then `release --leave-unresolved` and the token's deletion. The
// instance is let go, so an outside ticker can pick it up, and a later Run
// enrolls it afresh. The persistent pause is never touched.
//
// It refuses an instance this session holds no token for (*RefusedError with
// RefusedNotOurs), and one whose token the script refused as an ownership
// conflict: the ownership rule shows the token controls nothing, the token is
// deleted and the refusal says so. A sequence that stops short (the runner
// would not end, `release` failed) returns *StopError and leaves the session
// holding the token, unlike Quit, which leaves the token for a later session to
// adopt: the session lives on, so the reader can try again.
//
// Stop never cuts a Quit short, and a Quit that begins while Stop runs waits for
// it: Stop's wait runs on the caller's context, not the one Quit cancels, so
// the polite 30 s is not turned into an immediate SIGTERM. Quit then finds an
// instance that is released (nothing to do) or still held (wound down again).
// An adopted instance's runner is not this session's child: as in Quit, a
// failed or timed-out `stop` leaves nothing to SIGTERM and returns *StopError.
func (s *Supervisor) Stop(ctx context.Context, key string) (StopResult, error) {
	res := StopResult{Instance: key}
	s.mu.Lock()
	sess := s.sessions[key]
	closing := s.closing
	s.mu.Unlock()
	switch {
	case closing:
		return res, &RefusedError{Op: "stop", Instance: key, Reason: RefusedClosing}
	case sess == nil:
		return res, &RefusedError{Op: "stop", Instance: key, Reason: RefusedNotOurs, Detail: "this session holds no token for it, so there is nothing to stop"}
	}
	// Waiting for the slot ends when Quit begins (a Run in flight is cut short too).
	wctx, done := s.opContext(ctx)
	defer done()
	if err := sess.acquire(wctx); err != nil {
		return res, err
	}
	defer sess.release()
	if err := s.closingErr("stop", key); err != nil {
		return res, err
	}

	out := s.windDown(ctx, sess, QuitInstance{Instance: key, RunnerGone: true})
	switch out.Outcome {
	case QuitReleased:
		s.forget(sess) // the token is gone already; this drops the runner's log and the session's record
		res.Waited, res.Terminated, res.Why = out.Waited, out.Terminated, out.Why
		return res, nil
	case QuitLeftOwned:
		// Quit leaves a token for the next session to adopt. This session lives on:
		// it keeps the token and can try again.
		s.mu.Lock()
		sess.notice = ""
		if sess.child != nil && !isDone(sess.child) {
			sess.phase = PhaseRunning
		} else {
			sess.phase = PhaseEnrolled
		}
		s.mu.Unlock()
		s.signal()
		return res, &StopError{Instance: key, Why: out.Why, RunnerGone: out.RunnerGone}
	}
	why := out.Why
	if why == "" {
		why = "this session holds no token for it, so there is nothing to stop"
	}
	return res, &RefusedError{Op: "stop", Instance: key, Reason: RefusedNotOurs, Detail: why}
}
