package core

import (
	"context"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"time"

	"github.com/zealtv/easyhog/internal/groundhog"
	"github.com/zealtv/easyhog/internal/runner"
)

// RunOutcome says what an explicit Run did.
type RunOutcome int

const (
	// RunStarted: a runner was started (after enrolling and clearing a pause,
	// if those were needed).
	RunStarted RunOutcome = iota + 1
	// RunResumed: the runner was already running and a pause was cleared.
	RunResumed
	// RunAlreadyRunning: nothing to do. Run is idempotent.
	RunAlreadyRunning
)

func (o RunOutcome) String() string {
	switch o {
	case RunStarted:
		return "started"
	case RunResumed:
		return "resumed"
	case RunAlreadyRunning:
		return "already running"
	}
	return "nothing"
}

// RunResult is what Run did, for a notice. It carries no token contents.
type RunResult struct {
	Instance string
	Outcome  RunOutcome
	// Enrolled is true when this call enrolled the instance.
	Enrolled bool
	// ClearedPause is true when the instance was paused and this call resumed it.
	ClearedPause bool
	// PID is the runner's process id, for RunStarted.
	PID int
	// Log is the runner's output file, for RunStarted.
	Log string
	// Seen is true when `status` showed the runner running before Run returned.
	Seen bool
}

// Run is the reader's explicit act on one instance: enroll it for this session
// if it is unowned and capable, clear a persisted pause, and start its runner.
// It is idempotent: running a running instance changes nothing. It refuses an
// instance whose recorded owner no token of this session's matches (*RefusedError
// with RefusedExternal), however stopped that owner's runner looks, and one whose
// script lacks supervised control. For an instance whose token this session
// already holds (an earlier Run's, or one adopted from an ended session) it
// applies the ownership rule to a fresh read first: a token that no longer
// matches is deleted and the Run refused.
//
// An enroll that ends without a certain outcome is never retried blindly: the
// token stays, nothing starts, and the next Run, or the next coherent status,
// settles it by the ownership rule. Exit 4 (busy) is retried a few times. A
// runner that ends on its own is reported (RunnerExitedError at startup,
// PhaseExited later) and not restarted. An adopted instance whose runner still
// ticks is not started a second time.
func (s *Supervisor) Run(ctx context.Context, key string) (RunResult, error) {
	res := RunResult{Instance: key}
	callerCtx := ctx // an enroll already running outlives Quit; see runEnroll
	ctx, done := s.opContext(ctx)
	defer done()
	sess, err := s.sessionFor("run", key)
	if err != nil {
		return res, err
	}
	if err := sess.acquire(ctx); err != nil {
		return res, err
	}
	defer sess.release()
	if err := s.closingErr("run", key); err != nil {
		return res, err
	}

	s.mu.Lock()
	held := sess.holds()
	s.mu.Unlock()
	if !held {
		return s.runEnroll(ctx, callerCtx, sess, res)
	}
	return s.bringUp(ctx, sess, res)
}

func (s *Supervisor) closingErr(op, key string) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.closing {
		return &RefusedError{Op: op, Instance: key, Reason: RefusedClosing}
	}
	return nil
}

// runEnroll takes an instance this session has not enrolled through
// eligibility checks and `enroll`, then brings it up.
//
// ctx ends when Quit begins; callerCtx does not. Everything before `enroll` and
// everything after it stops with ctx. The `enroll` call itself runs on
// callerCtx, so a Quit that begins while it runs does not kill the script half
// way and leave an uncertain outcome: the call finishes within its own timeout,
// and Quit then finds an instance it can settle.
func (s *Supervisor) runEnroll(ctx, callerCtx context.Context, sess *session, res RunResult) (RunResult, error) {
	key, c := sess.key, sess.client
	sup, err := c.Negotiate(ctx)
	if err != nil {
		return res, err
	}
	if !sup.Controllable {
		return res, &RefusedError{Op: "run", Instance: key, Reason: RefusedUnsupported, Detail: sup.Reason}
	}
	st, err := s.readStatusForRun(ctx, c)
	if err != nil {
		return res, refuseIfInconsistent("run", key, err)
	}
	if err := s.checkOffered("run", sess, st, &sup); err != nil {
		return res, err
	}
	// Reserve the runner slot under the lock before
	// enrolling, so two first Runs cannot both pass the limit.
	if err := s.reserve(key); err != nil {
		return res, err
	}
	reserved := true
	defer func() {
		if reserved {
			s.mu.Lock()
			s.pending--
			s.mu.Unlock()
		}
	}()
	dir, err := s.sessionDir(key)
	if err != nil {
		return res, err
	}
	token, logPath := s.newPaths(dir)
	err = s.retryBusy(callerCtx, func() error {
		removeUnused(token) // exit 4 grants nothing and may leave an unused file
		return c.Enroll(callerCtx, s.cfg.Label, token)
	})
	var unc *groundhog.UncertainEnrollError
	switch {
	case err == nil:
	case errors.As(err, &unc):
		// The token may exist and the owner may be committed. Keep the token and
		// start nothing: the ownership rule settles it, on the next Run or the
		// next coherent status. A token that cannot be read is kept in the
		// session directory, where Quit lists it.
		if meta, merr := readTokenMetaFor(token, key); merr == nil {
			s.mu.Lock()
			s.pending--
			reserved = false
			s.hold(sess, token, logPath, meta)
			sess.notice = "enrollment outcome unknown; the token is kept, and the next Run or coherent status settles it"
			s.mu.Unlock()
			s.signal()
		} else if errors.Is(merr, os.ErrNotExist) {
			removeUnused(token) // a committed owner always has its token written first
		}
		return res, err
	default:
		removeUnused(token)
		return res, err
	}
	meta, merr := readTokenMetaFor(token, key)
	if merr != nil {
		// Ownership committed and we cannot say which owner. Keep the token (Quit
		// lists it); the instance reads as external until someone releases it.
		return res, &RefusedError{Op: "run", Instance: key, Reason: RefusedNotOurs,
			Detail: "enrolled, but the token's metadata is unreadable (" + merr.Error() + "); the token is kept in " + dir}
	}
	s.mu.Lock()
	s.pending--
	reserved = false
	s.hold(sess, token, logPath, meta)
	s.mu.Unlock()
	s.signal()
	res.Enrolled = true
	// Quit may have begun during the enroll: the instance is now ours, and Quit
	// (waiting for this operation to let go) releases it. Start nothing.
	if err := s.closingErr("run", key); err != nil {
		return res, err
	}
	return s.bringUp(ctx, sess, res)
}

// hold records a token as this session's. The caller holds s.mu.
func (s *Supervisor) hold(sess *session, token, logPath string, meta tokenMeta) {
	sess.phase, sess.token, sess.log = PhaseEnrolled, token, logPath
	sess.ownerID, sess.gen, sess.notice = meta.OwnerID, meta.Generation, ""
	sess.adopted = false
}

// reserve counts one more runner against the limit, for a first Run about to
// enroll. The count and the reservation share one critical section.
func (s *Supervisor) reserve(key string) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	n := s.pending
	for _, sess := range s.sessions {
		if sess.holds() {
			n++
		}
	}
	if n >= s.cfg.MaxRunners {
		return &RefusedError{Op: "run", Instance: key, Reason: RefusedLimit, Detail: fmt.Sprintf("at most %d runners", s.cfg.MaxRunners)}
	}
	s.pending++
	return nil
}

// applyRule is the ownership rule on a coherent status of an instance whose
// token this session holds: it reports whether the token controls the instance,
// and deletes a token whose instance shows no owner or another owner. Call it
// only with a coherent status and with the instance's operation slot held.
func (s *Supervisor) applyRule(sess *session, st groundhog.Status) (controls bool) {
	s.mu.Lock()
	ok := sess.holds() && sess.matches(st.Owner)
	s.mu.Unlock()
	if !ok {
		s.forget(sess)
	}
	return ok
}

// Observe applies the ownership rule to a coherent status the refresher has just
// read (pass it as Config.OnCoherent). Most reads change nothing: it acts only
// when this session holds a token for the instance and the status shows no owner
// or another owner. It then confirms with a read of its own, under the
// instance's operation slot (a read that began before an enroll finished could
// otherwise delete a token that is current), and deletes the token only if that
// read is coherent and still disagrees. An operation in flight on the instance
// applies the rule to its own reads, so Observe leaves it alone.
func (s *Supervisor) Observe(ctx context.Context, key string, st groundhog.Status) {
	if !st.Consistent {
		return
	}
	s.mu.Lock()
	sess := s.sessions[key]
	stale := !s.closing && sess != nil && sess.holds() && !sess.matches(st.Owner)
	s.mu.Unlock()
	if !stale {
		return
	}
	select {
	case sess.op <- struct{}{}:
		defer sess.release()
	default:
		return
	}
	fresh, err := s.readStatus(ctx, sess.client)
	if err != nil {
		return // not coherent: the rule waits for a coherent read
	}
	s.applyRule(sess, fresh)
}

// forget drops a token that the ownership rule shows controls nothing: the file
// and the runner log go, and the session holds nothing.
func (s *Supervisor) forget(sess *session) {
	s.mu.Lock()
	token, logPath := sess.token, sess.log
	sess.phase, sess.token, sess.ownerID, sess.gen, sess.notice = PhaseNone, "", "", 0, ""
	sess.log, sess.adopted = "", false
	s.mu.Unlock()
	if token != "" {
		removeUnused(token)
	}
	if logPath != "" {
		_ = os.Remove(logPath)
	}
	s.signal()
}

// Within reports whether path is dir or lies below it, after resolving symlinks
// where the paths exist (the client's own check is lexical). It is the one
// "beneath" test core has: the state-directory check uses it, and so does the
// scope Adopt is given (cmd/easyhog).
func Within(dir, path string) bool {
	if real, err := filepath.EvalSymlinks(dir); err == nil {
		dir = real
	}
	if real, err := filepath.EvalSymlinks(path); err == nil {
		path = real
	}
	rel, err := filepath.Rel(dir, path)
	return err == nil && rel != ".." && !strings.HasPrefix(rel, ".."+string(filepath.Separator))
}

func removeUnused(path string) { _ = os.Remove(path) }

func refuseIfInconsistent(op, key string, err error) error {
	if errors.Is(err, ErrInconsistent) {
		return &RefusedError{Op: op, Instance: key, Reason: RefusedInconsistent, Detail: "refresh and try again"}
	}
	return err
}

// bringUp makes an instance whose token this session holds run: apply the
// ownership rule to a fresh read, clear a pause, and start a runner unless one is
// alive (this session's own child, or an adopted instance's, which status shows).
func (s *Supervisor) bringUp(ctx context.Context, sess *session, res RunResult) (RunResult, error) {
	key, c := sess.key, sess.client
	st, err := s.readStatus(ctx, c)
	if errors.Is(err, ErrInconsistent) && res.Enrolled {
		// `enroll` takes the mutation lock, which settles an interrupted
		// journal write, so a status still incoherent here is something else.
		// The instance is ours and stays enrolled. Being ours, it no longer
		// qualifies as an unowned journal case, so it offers nothing until a
		// coherent read arrives; Quit releases it.
		return res, &RefusedError{Op: "run", Instance: key, Reason: RefusedInconsistent,
			Detail: "enrolled, but the status stayed incoherent afterwards; it is offered again once a coherent read arrives, and Quit releases it"}
	}
	if err != nil {
		return res, refuseIfInconsistent("run", key, err)
	}
	if !s.applyRule(sess, st) {
		// The token controls nothing now, and it is gone.
		if st.Owner == nil {
			return res, &RefusedError{Op: "run", Instance: key, Reason: RefusedNotOurs,
				Detail: "no owner is recorded, so this session's token controlled nothing and was removed; Run again to enroll afresh"}
		}
		return res, &RefusedError{Op: "run", Instance: key, Reason: RefusedExternal, Detail: "ownership changed; this session's token no longer controls it and was removed"}
	}
	if err := s.checkOffered("run", sess, st, nil); err != nil {
		return res, err
	}
	s.mu.Lock()
	token := sess.token
	s.mu.Unlock()
	if st.Paused {
		if err := s.retryBusy(ctx, func() error { return c.Resume(ctx, token) }); err != nil {
			return res, err
		}
		res.ClearedPause = true
	}
	if s.runnerLive(sess) || s.adoptedRunnerLives(sess, st) {
		res.Outcome = RunAlreadyRunning
		if res.ClearedPause {
			res.Outcome = RunResumed
		}
		return res, nil
	}
	return s.startRunner(ctx, sess, res)
}

// adoptedRunnerLives reports whether an adopted instance's runner, which is not
// this session's child, still runs, as status shows it. A second runner would
// only exit at once, holding no lock.
func (s *Supervisor) adoptedRunnerLives(sess *session, st groundhog.Status) bool {
	s.mu.Lock()
	defer s.mu.Unlock()
	return sess.adopted && sess.child == nil && st.Runner.State == groundhog.RunnerRunning
}

func (s *Supervisor) runnerLive(sess *session) bool {
	s.mu.Lock()
	defer s.mu.Unlock()
	return sess.child != nil && sess.phase == PhaseRunning
}

// startRunner launches `run` with its output in a file in the private
// directory, never a pipe: a runner whose output pipe closes when Easyhog goes
// away would lose its consumer to SIGPIPE.
func (s *Supervisor) startRunner(ctx context.Context, sess *session, res RunResult) (RunResult, error) {
	key, c := sess.key, sess.client
	s.mu.Lock()
	token, logPath := sess.token, sess.log
	s.mu.Unlock()
	logf, err := os.OpenFile(logPath, os.O_WRONLY|os.O_CREATE|os.O_APPEND, 0o600)
	if err != nil {
		return res, fmt.Errorf("open runner log: %w", err)
	}
	args := []string{"run", "--token-file", token}
	if secs := int(s.cfg.RunInterval / time.Second); secs > 0 {
		args = append(args, "--interval-seconds", strconv.Itoa(secs))
	}
	child, err := s.cfg.Launcher.Start(runner.Launch{Executable: c.Script(), Args: args, Dir: c.Instance(), Log: logf})
	if err != nil {
		_ = logf.Close()
		return res, fmt.Errorf("start runner: %w", err)
	}
	s.mu.Lock()
	sess.phase, sess.child, sess.exit, sess.asked, sess.notice = PhaseRunning, child, nil, false, ""
	sess.adopted = false // a runner of this session's own now
	s.mu.Unlock()
	s.signal()
	go s.watch(sess, child, logf)
	res.Outcome, res.PID, res.Log = RunStarted, child.PID(), logPath

	// Watch the start: the runner validates its configuration before its first
	// tick and exits at once if that is wrong (exit 2), or if another runner
	// holds the lock (exit 4). Report that; never start it again.
	deadline := time.Now().Add(s.cfg.StartWait)
	for {
		select {
		case <-child.Done():
			return res, &RunnerExitedError{Instance: key, Exit: child.Exit(), Log: logPath}
		default:
		}
		if st, err := s.readStatus(ctx, c); err == nil && st.Runner.State == groundhog.RunnerRunning {
			res.Seen = true
			return res, nil
		}
		if time.Now().After(deadline) || s.cfg.Sleep(ctx, s.cfg.PollEvery) != nil {
			return res, nil
		}
	}
}

// watch reports the end of a runner process. One that ends without having been
// asked to is a crash as far as the reader is concerned: it is reported, the
// instance stays owned and (as persisted) paused or not, and it is not
// restarted.
func (s *Supervisor) watch(sess *session, child runner.Child, logf *os.File) {
	<-child.Done()
	_ = logf.Close()
	x := child.Exit()
	s.mu.Lock()
	if sess.child == child {
		sess.child = nil
		sess.exit = &x
		if sess.phase == PhaseRunning {
			if sess.asked {
				// Asked to end (Settle's stop; Quit sets its own phase afterwards):
				// the session holds the token and has no runner.
				sess.phase = PhaseEnrolled
			} else {
				sess.phase = PhaseExited
				sess.notice = fmt.Sprintf("runner ended on its own (%s) and was not restarted; log %s", x, sess.log)
			}
		}
	}
	s.mu.Unlock()
	s.signal()
}
