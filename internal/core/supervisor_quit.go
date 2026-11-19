package core

import (
	"context"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"sync"
	"time"

	"github.com/zealtv/easyhog/internal/groundhog"
	"github.com/zealtv/easyhog/internal/runner"
)

// QuitOutcome says how one instance ended up when the session quit.
type QuitOutcome int

const (
	// QuitReleased: the runner is stopped, ownership released and the token
	// deleted. A persisted pause stays as it was (Quit never touches it).
	QuitReleased QuitOutcome = iota
	// QuitLeftOwned: something stopped the clean sequence. The instance is
	// left owned, which every later session sees as an external owner, and the
	// token stays in TokenFile with a Notice saying why and what to do.
	QuitLeftOwned
	// QuitNotOwned: this session holds nothing for the instance, or the token it
	// held controlled nothing (the ownership rule showed no owner or another
	// owner, and the token was deleted); nothing is left owned by this session.
	QuitNotOwned
)

func (o QuitOutcome) String() string {
	switch o {
	case QuitReleased:
		return "released"
	case QuitLeftOwned:
		return "left owned"
	}
	return "not owned"
}

// QuitInstance is one instance's part of a QuitReport.
type QuitInstance struct {
	Instance string
	Outcome  QuitOutcome
	// Waited is how long `stop` took: the time the runner needed to finish the
	// consumer call it had admitted, if any, and exit.
	Waited time.Duration
	// Terminated is true only when SIGTERM was actually sent to the runner: `stop`
	// did not finish (it outlasted QuitWait, was hurried, or failed) while the
	// process lived, or the process lingered ReapWait after a good `stop`. A
	// consumer call it was still waiting on was then detached, never killed, and
	// its delivery stays `uncertain` until someone records `recover`.
	Terminated bool
	// RunnerGone is true when the runner process has ended (or never ran).
	RunnerGone bool
	// TokenFile is the retained token's path when Outcome is QuitLeftOwned.
	TokenFile string
	// Notice is a one-line, secret-free explanation for a person, set when
	// the instance was left owned or anything went wrong.
	Notice string
	// Why is the part of Notice that says what went wrong, without the instance's
	// path in front or the left-owned advice behind it: the problems that stopped
	// or marred the sequence, joined. Empty when nothing went wrong. Stop words
	// its own notice from it.
	Why string
}

// QuitReport is the result of Quit.
type QuitReport struct {
	Instances []QuitInstance
	// Retained is the session's private directory when it still holds tokens
	// or logs worth keeping; empty when Quit removed it.
	Retained string
	// Notices are the lines to show the reader, in order.
	Notices []string
}

// Clean reports whether every instance was released or was never owned.
func (r QuitReport) Clean() bool {
	for _, in := range r.Instances {
		if in.Outcome == QuitLeftOwned {
			return false
		}
	}
	return true
}

// Quit shuts down everything this session owns, once; a second call waits for
// the first and returns the same report. It is the one path for ctrl+c, the
// palette's Quit and SIGINT, SIGTERM and SIGHUP: the terminal ends first, then
// main calls Quit.
//
// Quit is stop, then release. For every instance at once, none waiting for
// another:
//
//  1. `stop`, bounded by QuitWait. Since Groundhog f189ad2 it closes admission,
//     waits for the consumer call the runner already admitted, records its
//     result and lets the runner exit;
//  2. if `stop` did not finish (the bound, a failure, a second interrupt), send
//     a live runner SIGTERM and wait ReapWait for it to end. SIGTERM detaches
//     from a consumer call at once and leaves its delivery `uncertain`; never
//     SIGKILL, which would leave the handoff unrecorded. A runner that does not
//     end is left running and its instance left owned;
//  3. once the runner has ended, `release --leave-unresolved` (a plain
//     `release` for a script without that capability), under its own
//     real-time bound, ReleaseWait, which a second interrupt does not cut; then
//     delete the token.
//
// Quit never touches the pause and reads `status` only when `stop` was refused
// as a stale token, to apply the ownership rule. An unresolved delivery no longer
// stops the release: the instance goes back unowned, the delivery stays
// `uncertain`, and the barrier it raises stays until the next owner records
// `recover`. The instance stays owned only for a real failure: the runner will
// not end, `release` fails, or the token is refused for a reason the rule cannot
// settle.
//
// An adopted instance is wound down the same way, except that its runner is not
// this session's child: a good `stop` has waited for it to exit, but when `stop`
// fails or runs out of time there is no process here to SIGTERM, so the instance
// is left owned and the notice says so.
//
// The instances run in parallel, so Quit takes about QuitWait plus ReapWait and
// ReleaseWait however many runners there are. A second interrupt (ctx ending)
// hurries it: `stop` fails at once, every live runner is still sent SIGTERM,
// and `release` still runs.
func (s *Supervisor) Quit(ctx context.Context) QuitReport {
	s.quitOnce.Do(func() {
		defer close(s.quitDone)
		s.quitReport = s.quit(ctx)
	})
	<-s.quitDone
	return s.quitReport
}

func (s *Supervisor) quit(ctx context.Context) QuitReport {
	s.mu.Lock()
	s.closing = true
	// Every session, including PhaseNone ones: a first Run may still be
	// enrolling, with no phase yet. quitOne waits for the instance's operation
	// slot and then reads the phase the Run left, so one that never enrolled
	// ends "not owned" and one that did is wound down.
	list := make([]*session, 0, len(s.sessions))
	for _, sess := range s.sessions {
		list = append(list, sess)
	}
	s.mu.Unlock()
	s.opCancel() // cuts short any Run or Stop still in flight, except an enroll already running

	sort.Slice(list, func(i, j int) bool { return list[i].key < list[j].key })
	out := make([]QuitInstance, len(list))
	var wg sync.WaitGroup
	for i, sess := range list {
		wg.Add(1)
		go func() {
			defer wg.Done()
			out[i] = s.quitOne(ctx, sess)
		}()
	}
	wg.Wait()

	rep := QuitReport{Instances: out}
	for _, in := range out {
		if in.Notice != "" {
			rep.Notices = append(rep.Notices, in.Notice)
		}
	}
	rep.Retained = s.tidy()
	if rep.Retained != "" {
		rep.Notices = append(rep.Notices, "tokens and runner logs kept in "+rep.Retained)
	}
	for _, l := range s.Leftovers() {
		what := "a token that cannot be read"
		if l.Instance != "" {
			what = "a token for " + l.Instance
		}
		rep.Notices = append(rep.Notices, fmt.Sprintf("%s was left where it is, in %s: %s", what, l.TokenFile, l.Why))
	}
	s.signal()
	return rep
}

// tidy removes what a clean quit leaves behind: the logs of instances no longer
// owned, then the session directory (its lock file with it) if nothing else is in
// it. A token is never removed here, so a directory that still holds one stays,
// with its lock file, which is what tells a later session this one has ended: the
// lock is released here, as Quit is the end of the session. It returns the
// directory's path when it stays.
func (s *Supervisor) tidy() string {
	s.mu.Lock()
	dir, lock := s.dir, s.lock
	var done []string
	for _, sess := range s.sessions {
		if sess.token == "" && sess.log != "" {
			done = append(done, sess.log)
		}
	}
	s.mu.Unlock()
	if dir == "" {
		return ""
	}
	for _, l := range done {
		_ = os.Remove(l)
	}
	left := dir
	if names, err := os.ReadDir(dir); err == nil && len(names) == 1 && names[0].Name() == lockName {
		_ = os.Remove(filepath.Join(dir, lockName))
		if err := os.Remove(dir); err == nil || errors.Is(err, os.ErrNotExist) {
			left = ""
		}
	}
	if lock != nil {
		_ = lock.Close()
	}
	return left
}

// quitOne winds down one instance for Quit. It never returns before the runner
// process has either ended or been sent SIGTERM and given ReapWait to end.
func (s *Supervisor) quitOne(ctx context.Context, sess *session) QuitInstance {
	out := QuitInstance{Instance: sess.key, RunnerGone: true}
	// A Run or Stop in flight was cut short by opCancel (a Run) or runs on until it
	// ends (a Stop, on its caller's context); wait for it to let go.
	if err := sess.acquire(ctx); err != nil {
		// Quit was hurried while an operation still holds the slot. SIGTERM the runner
		// (a Stop waiting on a consumer call then finishes at once), and wait for the
		// slot in real time: the Stop's `release` runs under a bound of its own,
		// outside any context, so it can finish, and Quit then finds the instance
		// released or winds it down. Only past that bound is the instance reported left
		// owned.
		s.mu.Lock()
		child, token := sess.child, sess.token
		s.mu.Unlock()
		_, _ = s.reap(child, true)
		wctx, cancel := context.WithTimeout(context.WithoutCancel(ctx), s.cfg.ReapWait+s.cfg.ReleaseWait)
		werr := sess.acquire(wctx)
		cancel()
		if werr != nil {
			return s.leftOwned(sess, out, token, "an earlier operation had not finished when the quit was interrupted ("+err.Error()+")")
		}
	}
	defer sess.release()
	return s.windDown(ctx, sess, out)
}

// windDown is the one stop-then-release sequence, for one instance, with the
// instance's operation slot held: Quit runs it for every instance, and Stop runs
// it for the one the reader chose. Reuse it; a second stop-then-release would
// drift from the first.
func (s *Supervisor) windDown(ctx context.Context, sess *session, out QuitInstance) QuitInstance {
	s.mu.Lock()
	holds, token, child := sess.holds(), sess.token, sess.child
	foreign := sess.adopted && child == nil // its runner, if alive, is not this session's child
	s.mu.Unlock()

	if !holds {
		out.Outcome = QuitNotOwned
		return out
	}
	c := sess.client
	out.RunnerGone = child == nil || isDone(child)

	// 1. Stop: close admission, let the runner finish the call it admitted and
	// record its result, and exit. Bounded by QuitWait (this also bounds the busy
	// retries); the pause is left as the reader set it.
	s.mu.Lock()
	sess.asked = true
	s.mu.Unlock()
	var problems []string
	began := time.Now()
	sctx, scancel := context.WithTimeout(ctx, s.cfg.QuitWait)
	stopErr := s.retryBusy(sctx, func() error { return c.Stop(sctx, token) })
	scancel()
	out.Waited = time.Since(began)
	stopFailed := stopErr != nil
	if stopFailed && isOwnershipError(stopErr) {
		// The script refused the token: it may control nothing. The ownership rule
		// decides, on a coherent status of its own.
		if res, done := s.quitStaleToken(ctx, sess, out, token); done {
			return res
		}
	}
	if stopFailed {
		switch {
		case errors.Is(stopErr, context.DeadlineExceeded):
			problems = append(problems, fmt.Sprintf("stop had not finished after %s", out.Waited.Round(time.Second)))
		default:
			problems = append(problems, "stop failed: "+stopErr.Error())
		}
	}

	// 2. Make sure the process is gone: after a good stop, give it ReapWait to
	// exit; otherwise SIGTERM at once. Never SIGKILL. An adopted runner is not our
	// child: a good `stop` waited for it to exit, and a failed one leaves nothing
	// here to signal.
	if foreign && stopFailed {
		out.RunnerGone = false
		problems = append(problems, "the runner was started by an earlier session, so this session has no process to SIGTERM and cannot tell whether it still runs")
		return s.leftOwned(sess, out, token, joinProblems(problems))
	}
	out.RunnerGone, out.Terminated = s.reap(child, stopFailed)
	if !out.RunnerGone {
		problems = append(problems, fmt.Sprintf("the runner (pid %d) did not end after stop and SIGTERM and was left running", child.PID()))
		return s.leftOwned(sess, out, token, joinProblems(problems))
	}

	// 3. The runner has ended: release under a bound of its own, in real time, so
	// a second interrupt does not strand an instance whose release takes well
	// under a second. Then delete the token.
	rctx, rcancel := context.WithTimeout(context.WithoutCancel(ctx), s.cfg.ReleaseWait)
	err := s.retryBusy(rctx, func() error { return c.ReleaseLeavingUnresolved(rctx, token) })
	rcancel()
	if err != nil {
		var ee *groundhog.ExitError
		switch {
		case errors.As(err, &ee) && ee.Kind == groundhog.KindState:
			problems = append(problems, "an unresolved handoff attempt blocks release ("+err.Error()+"); a script without release-unresolved-v1 refuses a plain release then")
		default:
			problems = append(problems, "release failed: "+err.Error())
		}
		return s.leftOwned(sess, out, token, joinProblems(problems))
	}
	_ = os.Remove(token)
	s.mu.Lock()
	sess.phase, sess.token, sess.notice = PhaseReleased, "", ""
	s.mu.Unlock()
	out.Outcome = QuitReleased
	out.Why = joinProblems(problems)
	switch {
	case out.Terminated:
		why := "the runner lingering after stop"
		if len(problems) > 0 {
			why = joinProblems(problems)
		}
		out.Notice = fmt.Sprintf("%s: released, after %s and SIGTERM to the runner; a consumer call it was waiting on, if any, stays uncertain until it is recovered", sess.key, why)
	case len(problems) > 0: // stop failed with no live runner, but release went through
		out.Notice = fmt.Sprintf("%s: released, after %s", sess.key, joinProblems(problems))
	}
	return out
}

func isOwnershipError(err error) bool {
	var ee *groundhog.ExitError
	return errors.As(err, &ee) && ee.Kind == groundhog.KindOwnership
}

// quitStaleToken settles a token whose `stop` was refused as an ownership
// conflict: it reads a coherent status (in real time, under ReleaseWait, so a
// second interrupt does not cut it) and applies the ownership rule. It reports
// done when the rule showed the token controls nothing; the token is then
// deleted and the instance counts as not owned. Otherwise Quit goes on and the
// refusal is reported as it would have been.
func (s *Supervisor) quitStaleToken(ctx context.Context, sess *session, out QuitInstance, token string) (QuitInstance, bool) {
	rctx, cancel := context.WithTimeout(context.WithoutCancel(ctx), s.cfg.ReleaseWait)
	defer cancel()
	st, err := s.readStatus(rctx, sess.client)
	if err != nil || s.applyRule(sess, st) {
		return out, false
	}
	out.Outcome, out.RunnerGone = QuitNotOwned, true
	who := "no owner is recorded"
	if st.Owner != nil {
		who = fmt.Sprintf("owner %q is recorded", st.Owner.Label)
	}
	out.Why = fmt.Sprintf("this session's token controlled nothing (%s) and was removed", who)
	out.Notice = sess.key + ": " + out.Why
	return out, true
}

func joinProblems(p []string) string {
	s := ""
	for i, x := range p {
		if i > 0 {
			s += "; "
		}
		s += x
	}
	return s
}

// leftOwned records an instance left owned and builds the notice. The notice
// holds the token's path, never its contents.
func (s *Supervisor) leftOwned(sess *session, out QuitInstance, token, why string) QuitInstance {
	out.Outcome, out.TokenFile, out.Why = QuitLeftOwned, token, why
	out.Notice = fmt.Sprintf("%s was left owned by this session's token (%s): %s. Other supervisors will see an external owner. "+
		"Reopen Easyhog beneath the same root: it adopts this token (a session on another root leaves it where it is); Recover there, then Quit. "+
		"By hand instead, once the runner has ended and before a session adopts the token (adopting moves its file): groundhog.sh recover for any uncertain delivery, then release --token-file; a script with release-unresolved-v1 can also release --leave-unresolved", sess.key, token, why)
	s.mu.Lock()
	sess.phase, sess.notice = PhaseRetained, out.Notice
	s.mu.Unlock()
	return out
}

func isDone(c runner.Child) bool {
	select {
	case <-c.Done():
		return true
	default:
		return false
	}
}

// reap waits for the runner process to end. Unless force is set it first gives
// it ReapWait to end on its own after `stop`; then it sends SIGTERM and waits
// ReapWait again. It never sends anything stronger. It reports whether the
// process is gone and whether SIGTERM was actually sent (not when the process
// had already ended or ended by itself in time). These waits use real time, not
// the quit's context: a runner still alive when the context ended must still
// get its SIGTERM.
func (s *Supervisor) reap(child runner.Child, force bool) (gone, terminated bool) {
	if child == nil || isDone(child) {
		return true, false
	}
	if !force {
		select {
		case <-child.Done():
			return true, false
		case <-time.After(s.cfg.ReapWait):
		}
	}
	terminated = child.Terminate() == nil
	select {
	case <-child.Done():
		return true, terminated
	case <-time.After(s.cfg.ReapWait):
		return false, terminated
	}
}
