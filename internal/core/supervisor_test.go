//go:build unix

package core

import (
	"context"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/zealtv/easyhog/internal/groundhog"
	"github.com/zealtv/easyhog/internal/runner"
)

func wantRefused(t *testing.T, err error, reason Refusal) *RefusedError {
	t.Helper()
	var re *RefusedError
	if !errors.As(err, &re) || !errors.Is(err, ErrRefused) {
		t.Fatalf("want a refusal, got %T %v", err, err)
	}
	if re.Reason != reason {
		t.Fatalf("refusal %q (%s), want %q", re.Reason, re.Detail, reason)
	}
	return re
}

func filesUnder(t *testing.T, dir string) []string {
	t.Helper()
	var out []string
	_ = filepath.WalkDir(dir, func(p string, d os.DirEntry, err error) error {
		if err == nil && !d.IsDir() && d.Name() != lockName {
			out = append(out, p)
		}
		return nil
	})
	return out
}

// ---- Run ----

func TestRunEnrollsAndStartsARunnerWhoseOutputIsAFile(t *testing.T) {
	r := newSRig(t)
	key := r.inst("proj")
	res := r.run(key)
	if res.Outcome != RunStarted || !res.Enrolled || !res.Seen || res.PID == 0 || res.ClearedPause {
		t.Fatalf("result %+v", res)
	}
	if got := r.gh.commands(key)[:4]; strings.Join(got, ",") != "capabilities,status,enroll,status" {
		t.Errorf("first calls %v", got)
	}
	if r.l.launchCount() != 1 {
		t.Fatalf("%d launches", r.l.launchCount())
	}
	ln := r.l.launches[0]
	if ln.Executable != filepath.Join(key, "groundhog.sh") || ln.Dir != key {
		t.Errorf("launch %+v", ln)
	}
	if len(ln.Args) != 3 || ln.Args[0] != "run" || ln.Args[1] != "--token-file" {
		t.Fatalf("args %v", ln.Args)
	}
	// The runner's output is a regular file, never a pipe a dying Easyhog could close.
	info, err := ln.Log.Stat()
	if err != nil || !info.Mode().IsRegular() {
		t.Errorf("runner log is %v, %v", info, err)
	}
	// The token sits in a private session directory under the state directory,
	// outside the instance.
	token := ln.Args[2]
	if !strings.HasPrefix(token, r.state+string(filepath.Separator)) && !strings.HasPrefix(token, mustReal(t, r.state)+string(filepath.Separator)) {
		t.Errorf("token %s is not under the state directory %s", token, r.state)
	}
	if strings.HasPrefix(token, filepath.Dir(key)) {
		t.Errorf("token %s lies inside the instance's project", token)
	}
	if di, err := os.Stat(filepath.Dir(token)); err != nil || di.Mode().Perm() != 0o700 {
		t.Errorf("session directory: %v %v", di, err)
	}
	if fi, err := os.Stat(token); err != nil || fi.Mode().Perm() != 0o600 {
		t.Errorf("token file: %v %v", fi, err)
	}
	// The ownership is the session's, and only its.
	st := r.gh.state(key)
	if !r.sup.IsOurs(groundhog.Owner{ID: st.owner.id, Generation: st.owner.gen}) {
		t.Error("the enrolled owner is not recognised as ours")
	}
	if r.sup.IsOurs(groundhog.Owner{ID: "o-else", Generation: 1}) || r.sup.IsOurs(groundhog.Owner{ID: st.owner.id, Generation: st.owner.gen + 1}) {
		t.Error("a stranger's owner is recognised as ours")
	}
	if st.owner.label != "test-session" {
		t.Errorf("owner label %q", st.owner.label)
	}
	info2, ok := r.sup.Runner(key)
	if !ok || info2.Phase != PhaseRunning || info2.PID != res.PID || info2.TokenFile != token {
		t.Errorf("runner info %+v", info2)
	}
	noSecret(t, "a call", r.gh.allCalls()...)
	noSecret(t, "the runner info", info2.Notice, info2.Log, info2.TokenFile)
}

func mustReal(t *testing.T, p string) string {
	t.Helper()
	real, err := filepath.EvalSymlinks(p)
	if err != nil {
		t.Fatal(err)
	}
	return real
}

func TestRunTwiceStartsOneRunner(t *testing.T) {
	r := newSRig(t)
	key := r.inst("proj")
	r.run(key)
	again := r.run(key)
	if again.Outcome != RunAlreadyRunning || again.Enrolled || again.ClearedPause {
		t.Fatalf("second Run %+v", again)
	}
	if r.gh.count(key, "enroll") != 1 || r.l.launchCount() != 1 || r.gh.count(key, "resume") != 0 {
		t.Errorf("enrolls %d launches %d resumes %d", r.gh.count(key, "enroll"), r.l.launchCount(), r.gh.count(key, "resume"))
	}
}

func TestConcurrentRunsStartOneRunner(t *testing.T) {
	r := newSRig(t)
	key := r.inst("proj")
	var wg sync.WaitGroup
	var started, already atomic.Int32
	for i := 0; i < 6; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			res, err := r.sup.Run(context.Background(), key)
			if err != nil {
				t.Error(err)
				return
			}
			if res.Outcome == RunStarted {
				started.Add(1)
			} else if res.Outcome == RunAlreadyRunning {
				already.Add(1)
			}
		}()
	}
	wg.Wait()
	if started.Load() != 1 || already.Load() != 5 || r.gh.count(key, "enroll") != 1 || r.l.launchCount() != 1 {
		t.Fatalf("started %d already %d enrolls %d launches %d", started.Load(), already.Load(), r.gh.count(key, "enroll"), r.l.launchCount())
	}
}

func TestRunClearsAPersistedPause(t *testing.T) {
	r := newSRig(t)
	key := r.inst("proj")
	r.gh.update(key, func(s *ghInst) { s.paused = true }) // paused by an earlier session
	res := r.run(key)
	if !res.ClearedPause || res.Outcome != RunStarted {
		t.Fatalf("result %+v", res)
	}
	if r.gh.state(key).paused {
		t.Error("the pause was not cleared")
	}
}

// Easyhog no longer sets the instance-wide pause, but something else can (an
// earlier Easyhog, or Groundhog's own pause command): Run on a runner that
// already ticks clears it and starts nothing second.
func TestRunClearsAPauseSetByAnotherToolOnARunningRunner(t *testing.T) {
	r := newSRig(t)
	key := r.inst("proj")
	r.run(key)
	r.gh.update(key, func(s *ghInst) { s.paused = true })
	res := r.run(key)
	if res.Outcome != RunResumed || !res.ClearedPause || r.l.launchCount() != 1 {
		t.Fatalf("Run on a paused, running instance: %+v, %d launches", res, r.l.launchCount())
	}
	if r.gh.state(key).paused {
		t.Error("still paused")
	}
}

func TestExternalInstanceIsRefusedWhateverItsRunnerDoes(t *testing.T) {
	for _, runnerState := range []string{"stopped", "running"} {
		t.Run(runnerState, func(t *testing.T) {
			r := newSRig(t)
			key := r.inst("proj")
			r.gh.update(key, func(s *ghInst) {
				s.owner = &ghOwner{id: "o-other", gen: 3, label: "some-other-supervisor"}
				s.runner = runnerState
			})
			_, err := r.sup.Run(context.Background(), key)
			wantRefused(t, err, RefusedExternal)
			if r.gh.count(key, "enroll") != 0 || r.l.launchCount() != 0 {
				t.Errorf("enrolls %d launches %d", r.gh.count(key, "enroll"), r.l.launchCount())
			}
			if _, err := r.sup.Stop(context.Background(), key); err == nil {
				t.Error("Stop reached an instance this session never enrolled")
			} else {
				wantRefused(t, err, RefusedNotOurs)
			}
			for _, c := range r.gh.commands(key) {
				if c != "capabilities" && c != "status" {
					t.Errorf("ran %q against an external instance", c)
				}
			}
			if _, ok := r.sup.Runner(key); ok || len(r.sup.Runners()) != 0 {
				t.Error("an external instance is listed as supervised")
			}
			if len(filesUnder(t, r.state)) != 0 {
				t.Errorf("a refused Run left files: %v", filesUnder(t, r.state))
			}
		})
	}
}

func TestUnsupportedScriptIsRefusedBeforeAnythingRuns(t *testing.T) {
	r := newSRig(t)
	key := r.inst("proj")
	r.gh.update(key, func(s *ghInst) { s.caps = []string{"status-json-v1"} })
	_, err := r.sup.Run(context.Background(), key)
	wantRefused(t, err, RefusedUnsupported)
	if got := r.gh.commands(key); len(got) != 1 || got[0] != "capabilities" {
		t.Errorf("commands %v", got)
	}
}

func TestInconsistentStatusIsRefused(t *testing.T) {
	r := newSRig(t)
	key := r.inst("proj")
	r.gh.update(key, func(s *ghInst) { s.inconsistent = 100 })
	_, err := r.sup.Run(context.Background(), key)
	wantRefused(t, err, RefusedInconsistent)
	if r.gh.count(key, "enroll") != 0 {
		t.Error("enrolled on an inconsistent status")
	}
}

// An unowned instance whose status is incoherent only for an interrupted
// journal write: Run enrolls (which settles it) and starts the runner.
func TestRunSettlesAnInterruptedJournalWrite(t *testing.T) {
	r := newSRig(t)
	key := r.inst("proj")
	r.gh.update(key, func(s *ghInst) { s.journalOdd = true })
	res, err := r.sup.Run(context.Background(), key)
	if err != nil || res.Outcome != RunStarted || !res.Enrolled {
		t.Fatalf("Run %+v %v", res, err)
	}
	if r.gh.count(key, "enroll") != 1 {
		t.Errorf("%d enrolls", r.gh.count(key, "enroll"))
	}
	if s := r.gh.state(key); s.journalOdd {
		t.Error("the journal write was not settled")
	}
}

// If the status is still incoherent after enroll, Run says so and stops: it
// does not enroll again or retry in a loop, and the next Run does not enroll
// a second time either.
func TestRunFailsVisiblyWhenTheJournalStaysUnsettledAfterEnroll(t *testing.T) {
	r := newSRig(t)
	key := r.inst("proj")
	r.gh.update(key, func(s *ghInst) { s.journalOdd, s.journalSticky = true, true })
	res, err := r.sup.Run(context.Background(), key)
	re := wantRefused(t, err, RefusedInconsistent)
	if !strings.Contains(re.Detail, "stayed incoherent") || !res.Enrolled {
		t.Errorf("result %+v, detail %q", res, re.Detail)
	}
	if r.gh.count(key, "enroll") != 1 || r.gh.count(key, "run") != 0 {
		t.Errorf("%d enrolls, %d runs", r.gh.count(key, "enroll"), r.gh.count(key, "run"))
	}
	if n := r.gh.count(key, "status"); n > 8 {
		t.Errorf("%d status reads: Run is looping", n)
	}
}

// Anything besides the journal write that makes the status incoherent keeps the
// refusal, with nothing enrolled.
func TestRunRefusesAJournalWriteWithAnotherIncoherence(t *testing.T) {
	r := newSRig(t)
	key := r.inst("proj")
	r.gh.update(key, func(s *ghInst) { s.journalOdd, s.inconsistent = true, 100 })
	_, err := r.sup.Run(context.Background(), key)
	wantRefused(t, err, RefusedInconsistent)
	if r.gh.count(key, "enroll") != 0 {
		t.Error("enrolled on a status incoherent for another reason")
	}
}

func TestStopOfAnUnenrolledInstanceIsRefused(t *testing.T) {
	r := newSRig(t)
	key := r.inst("proj")
	_, err := r.sup.Stop(context.Background(), key)
	wantRefused(t, err, RefusedNotOurs)
	if len(r.gh.allCalls()) != 0 {
		t.Errorf("calls %v", r.gh.allCalls())
	}
}

func TestBusyIsRetriedWithABound(t *testing.T) {
	t.Run("stop succeeds on a later try", func(t *testing.T) {
		r := newSRig(t)
		key := r.inst("proj")
		r.run(key)
		r.gh.hook = func(_ context.Context, _, cmd string, n int) (bool, string, error) {
			return cmd == "stop" && n <= 2, "", exitStatus(4)
		}
		if _, err := r.sup.Stop(context.Background(), key); err != nil {
			t.Fatal(err)
		}
		if got := r.gh.count(key, "stop"); got != 3 {
			t.Errorf("%d stop calls", got)
		}
	})
	t.Run("a script that stays busy is given up on", func(t *testing.T) {
		r := newSRig(t)
		key := r.inst("proj")
		r.run(key)
		r.gh.hook = func(_ context.Context, _, cmd string, _ int) (bool, string, error) {
			return cmd == "resume", "", exitStatus(4)
		}
		r.gh.update(key, func(s *ghInst) { s.paused = true })
		_, err := r.sup.Run(context.Background(), key)
		if !groundhog.IsRetryable(err) {
			t.Fatalf("err %v", err)
		}
		if got := r.gh.count(key, "resume"); got != DefaultBusyRetries+1 {
			t.Errorf("%d resume calls, want %d", got, DefaultBusyRetries+1)
		}
	})
	t.Run("enroll", func(t *testing.T) {
		r := newSRig(t)
		key := r.inst("proj")
		r.gh.hook = func(_ context.Context, _, cmd string, n int) (bool, string, error) {
			return cmd == "enroll" && n == 1, "", exitStatus(4)
		}
		r.run(key)
		if r.gh.count(key, "enroll") != 2 {
			t.Errorf("%d enroll calls", r.gh.count(key, "enroll"))
		}
	})
}

func TestCertainEnrollFailureLeavesNothingBehind(t *testing.T) {
	r := newSRig(t)
	key := r.inst("proj")
	r.gh.hook = func(_ context.Context, _, cmd string, _ int) (bool, string, error) {
		return cmd == "enroll", "", exitStatus(6)
	}
	_, err := r.sup.Run(context.Background(), key)
	var ee *groundhog.ExitError
	if !errors.As(err, &ee) || ee.Kind != groundhog.KindIO {
		t.Fatalf("err %v", err)
	}
	if _, ok := r.sup.Runner(key); ok {
		t.Error("a failed enroll is listed")
	}
	if files := filesUnder(t, r.state); len(files) != 0 {
		t.Errorf("files left: %v", files)
	}
}

// ---- Uncertain enrollment ----

// lostResult makes enroll commit and then report exit 7, as a lost result would.
func lostResult(g *fakeGH, after func(inst, tok string)) {
	g.hook = func(_ context.Context, inst, cmd string, n int) (bool, string, error) {
		if cmd != "enroll" || n != 1 {
			return false, "", nil
		}
		return true, "", exitStatus(7)
	}
}

// An enroll that ends without a certain outcome keeps its token and starts
// nothing; the ownership rule settles it, by the next Run or the next coherent
// status. Nothing is retried, and no special state is kept.

func uncertainEnroll(t *testing.T, err error) {
	t.Helper()
	var unc *groundhog.UncertainEnrollError
	if !errors.As(err, &unc) {
		t.Fatalf("want an uncertain enroll, got %v", err)
	}
}

func TestUncertainEnrollThatTookKeepsItsTokenAndTheNextRunStartsIt(t *testing.T) {
	r := newSRig(t)
	key := r.inst("proj")
	r.gh.hook = func(_ context.Context, inst, cmd string, n int) (bool, string, error) {
		if cmd == "enroll" && n == 1 {
			// Commit, then lose the result.
			_ = r.gh.enroll(inst, r.lastTokenArg(), "test-session")
			return true, "", exitStatus(7)
		}
		return false, "", nil
	}
	_, err := r.sup.Run(context.Background(), key)
	uncertainEnroll(t, err)
	if r.l.launchCount() != 0 {
		t.Fatal("an uncertain enrollment started a runner by itself")
	}
	if got := r.gh.count(key, "enroll"); got != 1 {
		t.Fatalf("%d enrolls: the uncertain enroll was retried", got)
	}
	info, _ := r.sup.Runner(key)
	if info.Phase != PhaseEnrolled || info.TokenFile == "" {
		t.Fatalf("info %+v", info)
	}
	if _, err := os.Stat(info.TokenFile); err != nil {
		t.Errorf("token not kept: %v", err)
	}
	// The token matches the owner status shows, so by the one rule the instance is ours.
	if st := r.gh.state(key); !r.sup.IsOurs(groundhog.Owner{ID: st.owner.id, Generation: st.owner.gen}) {
		t.Error("the instance is not ours although its token matches")
	}
	// The explicit Run after it applies the rule, then runs, with the same token.
	res, err := r.sup.Run(context.Background(), key)
	if err != nil || res.Outcome != RunStarted || res.Enrolled {
		t.Fatalf("second Run %+v %v", res, err)
	}
	if r.gh.count(key, "enroll") != 1 {
		t.Error("enrolled again")
	}
}

// lastTokenArg is the token path of the enroll call in progress.
func (r *srig) lastTokenArg() string {
	calls := r.gh.allCalls()
	for i := len(calls) - 1; i >= 0; i-- {
		if strings.Contains(calls[i], " enroll ") {
			return tokenArg(strings.Fields(calls[i]))
		}
	}
	r.t.Fatal("no enroll call yet")
	return ""
}

func TestUncertainEnrollThatDidNotTakeIsSettledByTheNextRun(t *testing.T) {
	r := newSRig(t)
	key := r.inst("proj")
	r.gh.hook = func(_ context.Context, inst, cmd string, n int) (bool, string, error) {
		if cmd == "enroll" && n == 1 {
			// A token was written, the owner never committed.
			b := `{"schema_version":1,"token":"` + fakeSecret + `","instance":"` + inst + `","owner_id":"o-ghost","generation":9}`
			_ = os.WriteFile(r.lastTokenArg(), []byte(b), 0o600)
			return true, "", exitStatus(7)
		}
		return false, "", nil
	}
	_, err := r.sup.Run(context.Background(), key)
	uncertainEnroll(t, err)
	if files := filesUnder(t, r.state); len(files) != 1 {
		t.Fatalf("the token must be kept until a coherent status shows it controls nothing: %v", files)
	}
	// The next Run reads a coherent status: no owner, so the token controls
	// nothing and goes, and nothing starts.
	_, err = r.sup.Run(context.Background(), key)
	wantRefused(t, err, RefusedNotOurs)
	if files := filesUnder(t, r.state); len(files) != 0 {
		t.Errorf("an unused token was kept: %v", files)
	}
	if r.l.launchCount() != 0 {
		t.Error("started a runner")
	}
	// Run may enroll afresh now that the ghost is gone.
	if res := r.run(key); res.Outcome != RunStarted || r.gh.count(key, "enroll") != 2 {
		t.Fatalf("fresh Run %+v after %d enrolls", res, r.gh.count(key, "enroll"))
	}
}

func TestUncertainEnrollOfAnotherOwnerIsExternal(t *testing.T) {
	r := newSRig(t)
	key := r.inst("proj")
	r.gh.hook = func(_ context.Context, inst, cmd string, n int) (bool, string, error) {
		if cmd == "enroll" && n == 1 {
			b := `{"schema_version":1,"token":"` + fakeSecret + `","instance":"` + inst + `","owner_id":"o-mine","generation":1}`
			_ = os.WriteFile(r.lastTokenArg(), []byte(b), 0o600)
			r.gh.update(inst, func(s *ghInst) { s.owner = &ghOwner{id: "o-someone-else", gen: 2, label: "else"} })
			return true, "", exitStatus(7)
		}
		return false, "", nil
	}
	_, err := r.sup.Run(context.Background(), key)
	uncertainEnroll(t, err)
	if r.sup.IsOurs(groundhog.Owner{ID: "o-someone-else", Generation: 2}) {
		t.Error("claimed an owner no token matches")
	}
	_, err = r.sup.Run(context.Background(), key)
	wantRefused(t, err, RefusedExternal)
	if files := filesUnder(t, r.state); len(files) != 0 {
		t.Errorf("a stale token was kept: %v", files)
	}
	if r.sup.IsOurs(groundhog.Owner{ID: "o-mine", Generation: 1}) {
		t.Error("claimed an owner it does not hold")
	}
}

func TestUncertainEnrollNeverDeletesItsTokenOnAnIncoherentRead(t *testing.T) {
	r := newSRig(t)
	key := r.inst("proj")
	r.gh.hook = func(_ context.Context, inst, cmd string, n int) (bool, string, error) {
		if cmd == "enroll" && n == 1 {
			_ = r.gh.enroll(inst, r.lastTokenArg(), "test-session")
			r.gh.update(inst, func(s *ghInst) { s.inconsistent = 6 }) // status will not settle
			return true, "", exitStatus(7)
		}
		return false, "", nil
	}
	_, err := r.sup.Run(context.Background(), key)
	uncertainEnroll(t, err)
	info, ok := r.sup.Runner(key)
	if !ok || info.Phase != PhaseEnrolled {
		t.Fatalf("info %+v", info)
	}
	// Still unsettled: Run does not enroll again, start anything or delete the token.
	_, err = r.sup.Run(context.Background(), key)
	wantRefused(t, err, RefusedInconsistent)
	if _, err := os.Stat(info.TokenFile); err != nil {
		t.Fatalf("the token must be kept on an incoherent read: %v", err)
	}
	if r.gh.count(key, "enroll") != 1 || r.l.launchCount() != 0 {
		t.Errorf("enrolls %d launches %d", r.gh.count(key, "enroll"), r.l.launchCount())
	}
	// Once status settles, the next Run runs it.
	r.gh.update(key, func(s *ghInst) { s.inconsistent = 0 })
	res, err := r.sup.Run(context.Background(), key)
	if err != nil || res.Outcome != RunStarted {
		t.Fatalf("Run %+v %v", res, err)
	}
	noSecret(t, "a call", r.gh.allCalls()...)
}

func TestEnrollThatTimesOutIsUncertainAndNotRetried(t *testing.T) {
	r := newSRig(t)
	key := r.inst("proj")
	r.gh.hook = func(ctx context.Context, inst, cmd string, n int) (bool, string, error) {
		if cmd == "enroll" && n == 1 {
			_ = r.gh.enroll(inst, r.lastTokenArg(), "test-session")
			<-ctx.Done() // the result never arrives
			return true, "", ctx.Err()
		}
		return false, "", nil
	}
	ctx, cancel := context.WithTimeout(context.Background(), 60*time.Millisecond)
	defer cancel()
	_, err := r.sup.Run(ctx, key)
	uncertainEnroll(t, err)
	res, err := r.sup.Run(context.Background(), key)
	if err != nil || res.Outcome != RunStarted || r.gh.count(key, "enroll") != 1 {
		t.Fatalf("Run %+v %v, %d enrolls", res, err, r.gh.count(key, "enroll"))
	}
}

// ---- Crashes ----

func TestRunnerThatEndsOnItsOwnIsReportedNotRestarted(t *testing.T) {
	r := newSRig(t)
	key := r.inst("proj")
	r.run(key)
	r.gh.update(key, func(s *ghInst) { s.paused = true })
	select { // drain earlier signals
	case <-r.sup.Changed():
	default:
	}
	r.l.child(key).crash(runner.Exit{Code: 6})
	eventuallyTrue(t, "the crash to be reported", func() bool {
		info, _ := r.sup.Runner(key)
		return info.Phase == PhaseExited
	})
	select {
	case <-r.sup.Changed():
	case <-time.After(time.Second):
		t.Error("no change signal for the crash")
	}
	info, _ := r.sup.Runner(key)
	if info.Exit == nil || info.Exit.Code != 6 || info.PID != 0 || !strings.Contains(info.Notice, "not restarted") || !strings.Contains(info.Notice, info.Log) {
		t.Errorf("info %+v", info)
	}
	time.Sleep(80 * time.Millisecond) // a restart storm would show here
	if r.l.launchCount() != 1 {
		t.Fatalf("%d launches: the runner was restarted", r.l.launchCount())
	}
	// Ownership and the persisted pause are as they were.
	st := r.gh.state(key)
	if st.owner == nil || !st.paused || !r.sup.IsOurs(groundhog.Owner{ID: st.owner.id, Generation: st.owner.gen}) {
		t.Errorf("state %+v", st)
	}
	// An explicit Run starts it again, once, and clears the pause.
	res := r.run(key)
	if res.Outcome != RunStarted || !res.ClearedPause || r.l.launchCount() != 2 || res.Enrolled {
		t.Fatalf("Run after the crash: %+v, %d launches", res, r.l.launchCount())
	}
}

func TestRunnerThatEndsAtStartupIsReportedWithItsLog(t *testing.T) {
	r := newSRig(t)
	key := r.inst("proj")
	r.l.exitAtStart = &runner.Exit{Code: 2}
	_, err := r.sup.Run(context.Background(), key)
	var re *RunnerExitedError
	if !errors.As(err, &re) || re.Exit.Code != 2 || re.Log == "" || !strings.Contains(err.Error(), re.Log) {
		t.Fatalf("err %v", err)
	}
	time.Sleep(50 * time.Millisecond)
	if r.l.launchCount() != 1 {
		t.Errorf("%d launches", r.l.launchCount())
	}
	if info, _ := r.sup.Runner(key); info.Phase != PhaseExited {
		t.Errorf("phase %v", info.Phase)
	}
}

func TestLauncherFailureLeavesTheInstanceEnrolled(t *testing.T) {
	r := newSRig(t)
	key := r.inst("proj")
	r.l.startErr = errors.New("exec failed")
	if _, err := r.sup.Run(context.Background(), key); err == nil || !strings.Contains(err.Error(), "exec failed") {
		t.Fatalf("err %v", err)
	}
	info, _ := r.sup.Runner(key)
	if info.Phase != PhaseEnrolled {
		t.Fatalf("phase %v", info.Phase)
	}
	r.l.startErr = nil
	if res := r.run(key); res.Outcome != RunStarted || r.gh.count(key, "enroll") != 1 {
		t.Fatalf("retry %+v", res)
	}
}

// ---- Quit ----

func TestQuitStopsThenReleasesAndNeverTouchesThePause(t *testing.T) {
	for _, tc := range []struct {
		name   string
		paused bool // something else paused the instance before quitting
	}{{"unpaused", false}, {"paused by something else", true}} {
		t.Run(tc.name, func(t *testing.T) {
			r := newSRig(t)
			key := r.inst("proj")
			r.run(key)
			if tc.paused {
				r.gh.update(key, func(s *ghInst) { s.paused = true }) // set by something else
			}
			child := r.l.child(key)
			before := len(r.gh.allCalls())
			rep := r.sup.Quit(context.Background())
			if len(rep.Instances) != 1 || rep.Instances[0].Outcome != QuitReleased || !rep.Clean() || rep.Retained != "" || len(rep.Notices) != 0 {
				t.Fatalf("report %+v", rep)
			}
			quitCalls := r.gh.allCalls()[before:]
			var names []string
			for _, c := range quitCalls {
				names = append(names, strings.Fields(c)[1])
			}
			if got := strings.Join(names, ","); got != "stop,release" {
				t.Errorf("Quit ran %v, want stop then release and nothing else (no pause, no status)", quitCalls)
			}
			if rel := quitCalls[len(quitCalls)-1]; !strings.Contains(rel, "--leave-unresolved") {
				t.Errorf("release call %q lacks --leave-unresolved", rel)
			}
			st := r.gh.state(key)
			if st.owner != nil || st.paused != tc.paused || st.runner != "stopped" {
				t.Errorf("state %+v: want released, pause as it was (%v), stopped", st, tc.paused)
			}
			if !isDone(child) || child.termCount() != 0 || rep.Instances[0].Terminated {
				t.Errorf("runner done=%v sigterms=%d: stop should have been enough", isDone(child), child.termCount())
			}
			if files := filesUnder(t, r.state); len(files) != 0 {
				t.Errorf("tokens or logs left behind: %v", files)
			}
			if info, _ := r.sup.Runner(key); info.Phase != PhaseReleased {
				t.Errorf("phase %v", info.Phase)
			}
			if r.sup.IsOurs(groundhog.Owner{ID: "o-1", Generation: 1}) {
				t.Error("still claims an owner it released")
			}
			// The same report on a second Quit.
			if again := r.sup.Quit(context.Background()); len(again.Instances) != 1 {
				t.Errorf("second Quit %+v", again)
			}
		})
	}
}

// The supervisor reads status through the same client, so its Run read asks
// for --active exactly when the script offers it, and gets the settled counts.
func TestRunReadsStatusWithActiveWhenTheScriptOffersIt(t *testing.T) {
	for _, tc := range []struct {
		name string
		caps []string // nil: every capability
		want string
	}{{"offered", nil, "status --json --active"}, {"not offered", controlCaps(), "status --json"}} {
		t.Run(tc.name, func(t *testing.T) {
			r := newSRig(t)
			key := r.inst("proj")
			r.gh.update(key, func(s *ghInst) { s.caps = tc.caps })
			r.run(key)
			var seen int
			for _, c := range r.gh.allCalls() {
				if _, rest, _ := strings.Cut(c, " "); strings.HasPrefix(rest, "status") {
					seen++
					if rest != tc.want {
						t.Errorf("read %q, want %q", rest, tc.want)
					}
				}
			}
			if seen == 0 {
				t.Error("Run read no status")
			}
		})
	}
}

func TestQuitWaitsForAConsumerCallThatFinishes(t *testing.T) {
	r := newSRig(t, func(c *SupervisorConfig) { c.QuitWait = 2 * time.Second })
	key := r.inst("proj")
	r.run(key)
	r.gh.update(key, func(s *ghInst) { s.inflight = true })
	go func() {
		time.Sleep(120 * time.Millisecond)
		r.gh.update(key, func(s *ghInst) { s.inflight = false }) // the call is recorded accepted
	}()
	rep := r.sup.Quit(context.Background())
	in := rep.Instances[0]
	if in.Outcome != QuitReleased || in.Terminated || in.Waited < 100*time.Millisecond || !rep.Clean() || len(rep.Notices) != 0 {
		t.Fatalf("%+v", in)
	}
	if r.l.child(key).termCount() != 0 {
		t.Error("SIGTERM sent although the call finished within the wait")
	}
	if st := r.gh.state(key); st.owner != nil || st.uncertain {
		t.Errorf("state %+v", st)
	}
}

// Quit during a call that outlasts QuitWait: SIGTERM at the bound (the call is
// detached, the delivery uncertain), then release --leave-unresolved.
func TestQuitTerminatesARunnerWhoseCallOutlastsTheWaitAndReleasesIt(t *testing.T) {
	r := newSRig(t, func(c *SupervisorConfig) { c.QuitWait = 150 * time.Millisecond })
	key := r.inst("proj")
	r.run(key)
	token := mustRunner(t, r, key).TokenFile
	r.gh.update(key, func(s *ghInst) { s.inflight = true })
	began := time.Now()
	rep := r.sup.Quit(context.Background())
	in := rep.Instances[0]
	if in.Outcome != QuitReleased || !in.Terminated || !in.RunnerGone || in.Waited < 140*time.Millisecond || !rep.Clean() {
		t.Fatalf("%+v", in)
	}
	if time.Since(began) > 3*time.Second {
		t.Errorf("quit took %v", time.Since(began))
	}
	if n := r.l.child(key).termCount(); n != 1 || !isDone(r.l.child(key)) {
		t.Errorf("%d SIGTERMs, runner done %v", n, isDone(r.l.child(key)))
	}
	// The call was detached, never killed: the delivery stays uncertain, and the
	// instance is nonetheless released, the token gone.
	st := r.gh.state(key)
	if st.owner != nil || !st.uncertain || st.runner != "stopped" {
		t.Errorf("state %+v", st)
	}
	if r.gh.count(key, "release") != 1 {
		t.Errorf("%d release calls", r.gh.count(key, "release"))
	}
	if fileExists(token) || len(filesUnder(t, r.state)) != 0 {
		t.Errorf("tokens or logs left: %v", filesUnder(t, r.state))
	}
	if len(rep.Notices) != 1 || rep.Retained != "" || !strings.Contains(in.Notice, "SIGTERM") || !strings.Contains(in.Notice, "uncertain") || !strings.Contains(in.Notice, key) {
		t.Errorf("notices %q retained %q", rep.Notices, rep.Retained)
	}
	noSecret(t, "a notice", rep.Notices...)
	if info, _ := r.sup.Runner(key); info.Phase != PhaseReleased {
		t.Errorf("phase %v", info.Phase)
	}
	// Another session finds it unowned, and so may enroll it.
	other, err := NewSupervisor(SupervisorConfig{Exec: r.gh, Launcher: r.l, StateDir: t.TempDir()})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { other.Quit(context.Background()) })
	if _, err := other.Run(context.Background(), key); err != nil {
		t.Errorf("another session's Run after the release: %v", err)
	}
}

// A script without release-unresolved-v1 (Groundhog 0034706 and earlier) gets
// the spec's plain release. Its `stop` also detaches from a call at once, so the
// delivery goes uncertain, its plain release refuses (exit 5), and the instance
// is left owned with its token and a notice that says what to do. Nothing waits
// for the call and nothing pauses.
func TestQuitOnAnOldScriptLeavesTheInstanceOwnedWithAnAccurateNotice(t *testing.T) {
	r := newSRig(t, func(c *SupervisorConfig) { c.QuitWait = 2 * time.Second })
	key := r.inst("proj")
	r.gh.update(key, func(s *ghInst) { s.caps = controlCaps(); s.oldStop = true })
	r.run(key)
	token := mustRunner(t, r, key).TokenFile
	child := r.l.child(key)
	r.gh.update(key, func(s *ghInst) { s.inflight = true })
	began := time.Now()
	rep := r.sup.Quit(context.Background())
	in := rep.Instances[0]
	if in.Outcome != QuitLeftOwned || in.TokenFile != token || rep.Clean() {
		t.Fatalf("%+v", in)
	}
	if time.Since(began) > time.Second || in.Terminated || child.termCount() != 0 || !in.RunnerGone {
		t.Errorf("an old stop detaches at once: quit took %v, terminated %v, sigterms %d, runner gone %v",
			time.Since(began), in.Terminated, child.termCount(), in.RunnerGone)
	}
	var plain int
	for _, c := range r.gh.allCalls() {
		if strings.Contains(c, "--leave-unresolved") {
			t.Errorf("passed the flag to a script that does not offer it: %q", c)
		}
		if _, rest, _ := strings.Cut(c, " "); strings.HasPrefix(rest, "release") {
			plain++
		}
		if _, rest, _ := strings.Cut(c, " "); strings.HasPrefix(rest, "pause") && !strings.Contains(c, "enroll") {
			t.Errorf("Quit paused: %q", c)
		}
	}
	if plain != 1 {
		t.Errorf("%d release calls, want one plain release", plain)
	}
	for _, want := range []string{"unresolved handoff", "release-unresolved-v1", token, key, "recover"} {
		if !strings.Contains(in.Notice, want) {
			t.Errorf("notice lacks %q: %q", want, in.Notice)
		}
	}
	if strings.Contains(in.Notice, "SIGTERM") {
		t.Errorf("notice mentions SIGTERM although none was sent: %q", in.Notice)
	}
	wantRepairLeadsWithReopening(t, in.Notice)
	if _, err := os.Stat(token); err != nil {
		t.Errorf("the token must stay: %v", err)
	}
	if st := r.gh.state(key); st.owner == nil || !st.uncertain || st.runner != "stopped" {
		t.Errorf("state %+v: want owned, the delivery uncertain, the runner stopped", st)
	}
	if info, _ := r.sup.Runner(key); info.Phase != PhaseRetained {
		t.Errorf("phase %v", info.Phase)
	}
}

// Terminated says SIGTERM was sent. A runner that outlives a good `stop` by
// ReapWait is sent one (and the notice says why); one that ended already is not.
func TestTerminatedMeansSIGTERMWasActuallySent(t *testing.T) {
	t.Run("lingering after a good stop", func(t *testing.T) {
		r := newSRig(t)
		key := r.inst("proj")
		r.run(key)
		child := r.l.child(key)
		// stop succeeds but the runner process does not end by itself.
		r.gh.hook = func(_ context.Context, _, cmd string, _ int) (bool, string, error) { return cmd == "stop", "", nil }
		rep := r.sup.Quit(context.Background())
		in := rep.Instances[0]
		if in.Outcome != QuitReleased || !in.Terminated || child.termCount() != 1 || !strings.Contains(in.Notice, "lingering") {
			t.Fatalf("%+v, sigterms %d", in, child.termCount())
		}
	})
	t.Run("stop failed but the runner had ended", func(t *testing.T) {
		r := newSRig(t)
		key := r.inst("proj")
		r.run(key)
		child := r.l.child(key)
		r.gh.hook = func(_ context.Context, _, cmd string, _ int) (bool, string, error) {
			if cmd == "stop" {
				child.crash(runner.Exit{Code: 6}) // it ended while stop was failing
				return true, "", exitStatus(6)
			}
			return false, "", nil
		}
		rep := r.sup.Quit(context.Background())
		in := rep.Instances[0]
		if in.Outcome != QuitReleased || in.Terminated || child.termCount() != 0 {
			t.Fatalf("%+v, sigterms %d", in, child.termCount())
		}
	})
}

func mustRunner(t *testing.T, r *srig, key string) RunnerInfo {
	t.Helper()
	info, ok := r.sup.Runner(key)
	if !ok {
		t.Fatalf("%s is not supervised", key)
	}
	return info
}

func TestQuitSendsSIGTERMWhenStopFails(t *testing.T) {
	r := newSRig(t)
	key := r.inst("proj")
	r.run(key)
	child := r.l.child(key)
	r.gh.hook = func(_ context.Context, _, cmd string, _ int) (bool, string, error) {
		return cmd == "stop", "", exitStatus(6)
	}
	rep := r.sup.Quit(context.Background())
	in := rep.Instances[0]
	if child.termCount() != 1 || !isDone(child) {
		t.Fatalf("sigterms %d, done %v", child.termCount(), isDone(child))
	}
	if in.Outcome != QuitReleased || !in.RunnerGone || !strings.Contains(in.Notice, "stop failed") {
		t.Fatalf("%+v", in)
	}
}

func TestQuitNeverKillsARunnerThatIgnoresSIGTERM(t *testing.T) {
	r := newSRig(t)
	key := r.inst("proj")
	r.run(key)
	child := r.l.child(key)
	r.l.mu.Lock()
	r.l.ignoreTerm = true
	r.l.mu.Unlock()
	r.gh.hook = func(_ context.Context, _, cmd string, _ int) (bool, string, error) {
		return cmd == "stop", "", exitStatus(6)
	}
	rep := r.sup.Quit(context.Background())
	in := rep.Instances[0]
	if in.Outcome != QuitLeftOwned || in.RunnerGone || child.termCount() != 1 || isDone(child) {
		t.Fatalf("%+v, sigterms %d", in, child.termCount())
	}
	if r.gh.count(key, "release") != 0 {
		t.Error("released an instance whose runner is still alive")
	}
	if !strings.Contains(in.Notice, "left running") {
		t.Errorf("notice %q", in.Notice)
	}
	child.end(runner.Exit{}) // the test's own cleanup of the fake
}

// Each stop is bounded by QuitWait, whatever the script does: a stop that never
// returns is given up on, the runner gets SIGTERM, and the instance is released.
func TestQuitBoundsEachStopByQuitWait(t *testing.T) {
	r := newSRig(t, func(c *SupervisorConfig) { c.QuitWait = 200 * time.Millisecond })
	key := r.inst("proj")
	r.run(key)
	child := r.l.child(key)
	r.gh.hook = func(ctx context.Context, _, cmd string, _ int) (bool, string, error) {
		if cmd == "stop" { // a stop that never returns
			<-ctx.Done()
			return true, "", ctx.Err()
		}
		return false, "", nil
	}
	began := time.Now()
	rep := r.sup.Quit(context.Background())
	if took := time.Since(began); took > 2*time.Second {
		t.Fatalf("quit took %v against a 200ms wait", took)
	}
	in := rep.Instances[0]
	if child.termCount() != 1 || !isDone(child) {
		t.Errorf("the runner got %d SIGTERMs, done %v", child.termCount(), isDone(child))
	}
	if in.Outcome != QuitReleased || !in.Terminated {
		t.Errorf("%+v", in)
	}
}

// A second interrupt: the quit's context ends while a stop waits on a call.
// `stop` is given up on, the runner gets SIGTERM, and release still runs, in
// real time, outside that context.
func TestQuitHurriesWhenItsContextEndsAndStillReleases(t *testing.T) {
	r := newSRig(t, func(c *SupervisorConfig) { c.QuitWait = time.Minute })
	key := r.inst("proj")
	r.run(key)
	child := r.l.child(key)
	r.gh.update(key, func(s *ghInst) { s.inflight = true })
	ctx, cancel := context.WithCancel(context.Background())
	go func() { time.Sleep(80 * time.Millisecond); cancel() }() // a second ctrl+c
	began := time.Now()
	rep := r.sup.Quit(ctx)
	if time.Since(began) > 2*time.Second {
		t.Fatalf("quit took %v after its context ended", time.Since(began))
	}
	in := rep.Instances[0]
	if !isDone(child) || in.Outcome != QuitReleased || !in.Terminated {
		t.Errorf("%+v, runner done %v", in, isDone(child))
	}
	if st := r.gh.state(key); st.owner != nil || !st.uncertain {
		t.Errorf("state %+v: want released with the delivery uncertain", st)
	}
}

// A double interrupt after the runner ended: stop cannot run on an ended
// context, there is no live runner to SIGTERM, and the instance is released
// anyway, because release has a bound of its own.
func TestQuitAfterTheRunnerEndedStillReleasesOnADoubleInterrupt(t *testing.T) {
	r := newSRig(t)
	key := r.inst("proj")
	r.run(key)
	r.l.child(key).crash(runner.Exit{Code: 6})
	eventuallyTrue(t, "the crash", func() bool { info, _ := r.sup.Runner(key); return info.Phase == PhaseExited })
	ctx, cancel := context.WithCancel(context.Background())
	cancel() // the reader interrupted twice
	rep := r.sup.Quit(ctx)
	in := rep.Instances[0]
	if in.Outcome != QuitReleased || !in.RunnerGone || in.Terminated || !rep.Clean() {
		t.Fatalf("%+v", in)
	}
	if st := r.gh.state(key); st.owner != nil {
		t.Errorf("state %+v: still owned", st)
	}
	if files := filesUnder(t, r.state); len(files) != 0 {
		t.Errorf("files left: %v", files)
	}
}

// An ended context with an idle instance and a live runner: the slot is free,
// so acquire takes it, the runner gets SIGTERM and the release goes through.
func TestQuitOnAnEndedContextDoesNotClaimAnEarlierOperationIsStillRunning(t *testing.T) {
	r := newSRig(t)
	key := r.inst("proj")
	r.run(key)
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	for i := 0; i < 20; i++ { // select picked at random before the fix
		sess := r.sup.sessions[key]
		if err := sess.acquire(ctx); err != nil {
			t.Fatalf("acquire %d on an idle instance: %v", i, err)
		}
		sess.release()
	}
	rep := r.sup.Quit(ctx)
	if in := rep.Instances[0]; in.Outcome != QuitReleased || strings.Contains(in.Notice, "earlier operation") {
		t.Fatalf("%+v", in)
	}
}

// With the slot busy, acquire reports the context's end.
func TestAcquireOnABusySlotReportsTheContextEnd(t *testing.T) {
	sess := &session{op: make(chan struct{}, 1)}
	if err := sess.acquire(context.Background()); err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	if err := sess.acquire(ctx); !errors.Is(err, context.Canceled) {
		t.Errorf("acquire on a busy slot with an ended context: %v", err)
	}
}

func TestQuitWithNothingEnrolledDoesNothing(t *testing.T) {
	r := newSRig(t)
	r.inst("proj") // discovered, never run
	rep := r.sup.Quit(context.Background())
	if len(rep.Instances) != 0 || !rep.Clean() || rep.Retained != "" {
		t.Fatalf("%+v", rep)
	}
	if calls := r.gh.allCalls(); len(calls) != 0 {
		t.Errorf("calls %v", calls)
	}
	if entries, _ := os.ReadDir(r.state); len(entries) != 0 {
		t.Errorf("the state directory was touched: %v", entries)
	}
}

func TestNothingStartsOnceQuitHasBegun(t *testing.T) {
	r := newSRig(t)
	key := r.inst("proj")
	r.sup.Quit(context.Background())
	_, err := r.sup.Run(context.Background(), key)
	wantRefused(t, err, RefusedClosing)
	_, err = r.sup.Stop(context.Background(), key)
	wantRefused(t, err, RefusedClosing)
	if r.l.launchCount() != 0 || len(r.gh.allCalls()) != 0 {
		t.Error("work started after Quit")
	}
}

// More owned instances than Quit's old limit of four at a time, each with a
// consumer call still running: every `stop` starts before any has finished, so
// Quit takes about the longest call, not one batch after another. The barrier
// in the hook passes only when all of them are waiting at once.
func TestQuitStopsEveryInstanceAtOnceEachWithASlowConsumer(t *testing.T) {
	const n = 8 // twice the old limit of four at a time
	r := newSRig(t, func(c *SupervisorConfig) { c.QuitWait = 5 * time.Second })
	var keys []string
	for i := 0; i < n; i++ {
		k := r.inst(fmt.Sprintf("p%d", i))
		keys = append(keys, k)
		r.run(k)
		r.gh.update(k, func(s *ghInst) { s.inflight = true })
	}
	var started atomic.Int32
	var allAtOnce atomic.Bool
	r.gh.hook = func(_ context.Context, inst, cmd string, _ int) (bool, string, error) {
		if cmd != "stop" {
			return false, "", nil
		}
		started.Add(1)
		for deadline := time.Now().Add(3 * time.Second); time.Now().Before(deadline); time.Sleep(time.Millisecond) {
			if started.Load() == n {
				allAtOnce.Store(true)
				break
			}
		}
		// This instance's consumer finishes only after every stop is under way.
		time.AfterFunc(100*time.Millisecond, func() { r.gh.update(inst, func(s *ghInst) { s.inflight = false }) })
		return false, "", nil
	}
	began := time.Now()
	rep := r.sup.Quit(context.Background())
	if len(rep.Instances) != n || !rep.Clean() || len(rep.Notices) != 0 {
		t.Fatalf("%+v", rep)
	}
	if !allAtOnce.Load() {
		t.Errorf("only %d of %d stops were under way together", started.Load(), n)
	}
	if took := time.Since(began); took > 3*time.Second {
		t.Errorf("quit took %v", took)
	}
	for _, in := range rep.Instances {
		if in.Outcome != QuitReleased || in.Terminated {
			t.Errorf("%+v", in)
		}
	}
	for _, k := range keys {
		if st := r.gh.state(k); st.owner != nil || st.uncertain {
			t.Errorf("%s: %+v", k, st)
		}
	}
}

func TestQuitStopsAndReleasesAnEnrollThatEndedUncertainly(t *testing.T) {
	r := newSRig(t)
	key := r.inst("proj")
	r.gh.hook = func(_ context.Context, inst, cmd string, n int) (bool, string, error) {
		if cmd == "enroll" && n == 1 {
			_ = r.gh.enroll(inst, r.lastTokenArg(), "test-session")
			r.gh.update(inst, func(s *ghInst) { s.inconsistent = 3 })
			return true, "", exitStatus(7)
		}
		return false, "", nil
	}
	_, err := r.sup.Run(context.Background(), key)
	uncertainEnroll(t, err)
	rep := r.sup.Quit(context.Background())
	if rep.Instances[0].Outcome != QuitReleased || r.gh.state(key).owner != nil {
		t.Fatalf("%+v, state %+v", rep.Instances[0], r.gh.state(key))
	}
	if files := filesUnder(t, r.state); len(files) != 0 {
		t.Errorf("files left: %v", files)
	}
}

func TestQuitAfterACrashStillStopsAndReleases(t *testing.T) {
	r := newSRig(t)
	key := r.inst("proj")
	r.run(key)
	r.l.child(key).crash(runner.Exit{Code: 6})
	eventuallyTrue(t, "the crash", func() bool { info, _ := r.sup.Runner(key); return info.Phase == PhaseExited })
	rep := r.sup.Quit(context.Background())
	if in := rep.Instances[0]; in.Outcome != QuitReleased || in.Terminated || in.Waited >= time.Second {
		t.Fatalf("%+v", in)
	}
}

// ---- Tokens ----

func TestTheTokensSecretNeverReachesArgvLogsOrNotices(t *testing.T) {
	r := newSRig(t, func(c *SupervisorConfig) { c.QuitWait = 100 * time.Millisecond })
	key := r.inst("proj")
	res := r.run(key)
	r.gh.update(key, func(s *ghInst) { s.inflight = true })
	rep := r.sup.Quit(context.Background())
	noSecret(t, "a call", r.gh.allCalls()...)
	noSecret(t, "the notices", rep.Notices...)
	noSecret(t, "the launch", res.Log, strings.Join(r.l.launches[0].Args, " "))
	for _, in := range r.sup.Runners() {
		noSecret(t, "runner info", in.Notice, in.Log, in.TokenFile, in.Instance)
	}
	if data, err := os.ReadFile(res.Log); err == nil {
		noSecret(t, "the runner log", string(data))
	}
}

// A live session's directory is never touched: it holds the lock, so a second
// session cannot adopt its tokens, and its instances are external to the second.
func TestATokenOfALiveSessionIsNeverAdopted(t *testing.T) {
	r := newSRig(t)
	key := r.inst("proj")
	r.run(key)
	token := mustRunner(t, r, key).TokenFile
	st := r.gh.state(key)

	second, err := NewSupervisor(SupervisorConfig{Exec: r.gh, Launcher: r.l, StateDir: r.state})
	if err != nil {
		t.Fatal(err)
	}
	if got := second.Adopt(everywhere); len(got) != 0 {
		t.Fatalf("adopted %v from a session that lives", got)
	}
	if second.IsOurs(groundhog.Owner{ID: st.owner.id, Generation: st.owner.gen}) {
		t.Fatal("a second session claims a live session's owner")
	}
	_, err = second.Run(context.Background(), key)
	wantRefused(t, err, RefusedExternal)
	_, err = second.Stop(context.Background(), key)
	wantRefused(t, err, RefusedNotOurs)
	if _, err := os.Stat(token); err != nil {
		t.Errorf("the live session's token must be left alone: %v", err)
	}
	// Quit lists every instance the session touched, including one a refused Run
	// left behind, but it must do nothing to an instance it does not control.
	rep := second.Quit(context.Background())
	if len(rep.Instances) != 1 || rep.Instances[0].Outcome != QuitNotOwned || rep.Instances[0].Notice != "" || len(rep.Notices) != 0 {
		t.Errorf("the second session reported more than \"not owned\": %+v", rep)
	}
	for _, cmd := range r.gh.commands(key) {
		switch cmd {
		case "pause", "resume", "stop", "release":
			t.Errorf("the second session ran %q against the live session's instance", cmd)
		}
	}
	if r.gh.count(key, "enroll") != 1 || r.gh.state(key).owner == nil {
		t.Error("the second session enrolled, or released the live session's owner")
	}
	if _, err := os.Stat(token); err != nil {
		t.Errorf("the second session removed the live token: %v", err)
	}
}

func TestTokensCannotBeKeptInsideAnInstance(t *testing.T) {
	g := newFakeGH()
	l := newFakeLauncher(g)
	key := filepath.Join(mustReal(t, t.TempDir()), "proj", ".groundhog")
	g.add(key)
	sup, err := NewSupervisor(SupervisorConfig{Exec: g, Launcher: l, StateDir: filepath.Join(key, "state")})
	if err != nil {
		t.Fatal(err)
	}
	_, err = sup.Run(context.Background(), key)
	if !errors.Is(err, groundhog.ErrInvalidArgument) {
		t.Fatalf("err %v", err)
	}
	if g.count(key, "enroll") != 0 || g.state(key).owner != nil {
		t.Error("enrolled with a token inside the instance")
	}
	if _, err := os.Stat(filepath.Join(key, "state")); err == nil {
		t.Error("made a directory inside the instance")
	}
}

func TestSupervisorNeedsItsDependencies(t *testing.T) {
	if _, err := NewSupervisor(SupervisorConfig{}); err == nil {
		t.Error("a supervisor without an executor or launcher")
	}
}

func TestDefaultStateDirIsPrivateToTheUserAndOutsideRepos(t *testing.T) {
	t.Setenv("XDG_STATE_HOME", "/xdg/state")
	if d, err := DefaultStateDir(); err != nil || d != "/xdg/state/easyhog" {
		t.Errorf("%q %v", d, err)
	}
	t.Setenv("XDG_STATE_HOME", "relative/is/ignored")
	d, err := DefaultStateDir()
	if err != nil || !filepath.IsAbs(d) || filepath.Base(d) != "easyhog" {
		t.Errorf("%q %v", d, err)
	}
}

// Run checks the palette's rule (Offers) on the fresh read it takes, so an
// instance whose runner is mid-stop is refused at the protocol too, not only
// left out of the palette.
func TestRunRefusesWhatTheRuleDoesNotOffer(t *testing.T) {
	r := newSRig(t)
	key := r.inst("proj")
	r.run(key)
	r.gh.update(key, func(s *ghInst) { s.runner = "stopping" })
	_, err := r.sup.Run(context.Background(), key)
	wantRefused(t, err, RefusedNotOffered)
	if !strings.Contains(err.Error(), "its runner is stopping") || r.l.launchCount() != 1 {
		t.Errorf("error %v, %d launches", err, r.l.launchCount())
	}
}

// A token left owned is adopted by the next session beneath the same root, which
// moves its file, so the notice leads with "reopen Easyhog" and gives the by-hand
// steps second, with the condition they depend on. The
// wording must also fit a token kept for another root, which adoption leaves.
func wantRepairLeadsWithReopening(t *testing.T, notice string) {
	t.Helper()
	at := -1
	for _, part := range []string{"Reopen Easyhog beneath the same root", "it adopts this token", "a session on another root leaves it where it is",
		"Recover there, then Quit", "By hand instead", "before a session adopts the token", "groundhog.sh recover", "release --token-file"} {
		i := strings.Index(notice, part)
		if i < 0 || i < at {
			t.Fatalf("notice %q: %q missing or out of order", notice, part)
		}
		at = i
	}
}
