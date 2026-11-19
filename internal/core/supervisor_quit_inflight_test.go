//go:build unix

package core

import (
	"context"
	"os"
	"strings"
	"testing"
	"time"
)

// Quit that begins while an instance's first Run is still enrolling. The fake `enroll` blocks inside the call until the
// test lets it go, and ignores its context: a real `enroll` is a script that runs
// to its end unless the executor kills it, which is what the old Quit did.

// enrollGate holds the first `enroll` of an instance until release is closed.
type enrollGate struct {
	entered chan struct{}
	release chan struct{}
}

func holdEnroll(r *srig, then func(inst string) error) *enrollGate {
	g := &enrollGate{entered: make(chan struct{}), release: make(chan struct{})}
	r.gh.hook = func(ctx context.Context, inst, cmd string, n int) (bool, string, error) {
		switch {
		case cmd == "enroll" && n == 1:
			close(g.entered)
			<-g.release
			if then != nil {
				return true, "", then(inst)
			}
			return false, "", nil // the default behavior: commit
		case cmd == "status" && ctx.Err() != nil:
			return true, "", ctx.Err() // a real executor ends a call whose context ended
		}
		return false, "", nil
	}
	return g
}

// quitMidEnroll starts Run, begins Quit while its enroll is held, then lets the
// enroll go and returns what Run and Quit returned.
func quitMidEnroll(t *testing.T, r *srig, key string, g *enrollGate) (RunResult, error, QuitReport) {
	t.Helper()
	type ran struct {
		res RunResult
		err error
	}
	run := make(chan ran, 1)
	go func() {
		res, err := r.sup.Run(context.Background(), key)
		run <- ran{res, err}
	}()
	select {
	case <-g.entered:
	case <-time.After(5 * time.Second):
		t.Fatal("the enroll never began")
	}
	quit := make(chan QuitReport, 1)
	go func() { quit <- r.sup.Quit(context.Background()) }()
	eventuallyTrue(t, "Quit to begin", func() bool {
		r.sup.mu.Lock()
		defer r.sup.mu.Unlock()
		return r.sup.closing
	})
	time.Sleep(20 * time.Millisecond) // let a cancelled enroll show itself
	close(g.release)
	var p ran
	var rep QuitReport
	for i := 0; i < 2; i++ {
		select {
		case p = <-run:
		case rep = <-quit:
		case <-time.After(10 * time.Second):
			t.Fatal("Run and Quit did not both return")
		}
	}
	return p.res, p.err, rep
}

func TestQuitDuringAnEnrollThatCommitsReleasesTheInstanceAndLeavesNoToken(t *testing.T) {
	r := newSRig(t)
	key := r.inst("proj")
	g := holdEnroll(r, nil)
	res, err, rep := quitMidEnroll(t, r, key, g)

	if len(rep.Instances) != 1 || rep.Instances[0].Outcome != QuitReleased || !rep.Clean() {
		t.Fatalf("report %+v", rep)
	}
	if r.gh.state(key).owner != nil {
		t.Errorf("the instance is still owned: %+v", r.gh.state(key))
	}
	if files := filesUnder(t, r.state); len(files) != 0 {
		t.Errorf("files left: %v", files)
	}
	if r.l.launchCount() != 0 {
		t.Error("a runner was started by a Run that Quit had cut short")
	}
	if err == nil || res.Outcome == RunStarted {
		t.Errorf("Run %+v %v: a Run cut short by Quit should say so", res, err)
	}
	if got := r.gh.count(key, "enroll"); got != 1 {
		t.Errorf("%d enrolls", got)
	}
	if info, _ := r.sup.Runner(key); info.Phase != PhaseReleased {
		t.Errorf("phase %v", info.Phase)
	}
}

func TestQuitDuringAnEnrollThatDidNotCommitEndsNotOwnedAfterTheRuleRemovesItsToken(t *testing.T) {
	r := newSRig(t)
	key := r.inst("proj")
	// The script wrote the token and then lost its result before committing an
	// owner: exit 7, uncertain.
	g := holdEnroll(r, func(inst string) error {
		b := `{"schema_version":1,"token":"` + fakeSecret + `","instance":"` + inst + `","owner_id":"o-ghost","generation":9}`
		_ = os.WriteFile(r.lastTokenArg(), []byte(b), 0o600)
		return exitStatus(7)
	})
	_, _, rep := quitMidEnroll(t, r, key, g)

	if len(rep.Instances) != 1 || rep.Instances[0].Outcome != QuitNotOwned {
		t.Fatalf("report %+v", rep)
	}
	if r.gh.state(key).owner != nil {
		t.Errorf("the instance is owned: %+v", r.gh.state(key))
	}
	if files := filesUnder(t, r.state); len(files) != 0 {
		t.Errorf("an orphan token or log was left: %v", files)
	}
	for _, n := range rep.Notices {
		noSecret(t, "a notice", n)
		if strings.Contains(n, "left owned") {
			t.Errorf("notice %q", n)
		}
	}
}

// Quit needs no status to wind an enroll down: the token it holds names the owner
// the script recorded, and stop then release work through it.
func TestQuitDuringAnEnrollWhoseStatusNeverSettlesStillStopsAndReleasesIt(t *testing.T) {
	r := newSRig(t)
	key := r.inst("proj")
	g := holdEnroll(r, func(inst string) error {
		_ = r.gh.enroll(inst, r.lastTokenArg(), "test-session") // committed, result lost
		r.gh.update(inst, func(s *ghInst) { s.inconsistent = 1000 })
		return exitStatus(7)
	})
	_, _, rep := quitMidEnroll(t, r, key, g)

	if len(rep.Instances) != 1 || rep.Instances[0].Outcome != QuitReleased || !rep.Clean() || len(rep.Notices) != 0 {
		t.Fatalf("report %+v", rep)
	}
	if r.gh.state(key).owner != nil {
		t.Errorf("the instance is still owned: %+v", r.gh.state(key))
	}
	if files := filesUnder(t, r.state); len(files) != 0 {
		t.Errorf("files left: %v", files)
	}
}

func TestStoppingCountsARunStillEnrolling(t *testing.T) {
	r := newSRig(t)
	key := r.inst("proj")
	if n := r.sup.Stopping(); n != 0 {
		t.Fatalf("%d before any Run", n)
	}
	g := holdEnroll(r, nil)
	done := make(chan struct{})
	go func() { defer close(done); _, _ = r.sup.Run(context.Background(), key) }()
	<-g.entered
	if n := r.sup.Stopping(); n != 1 {
		t.Errorf("Stopping() = %d with a Run enrolling, want 1", n)
	}
	close(g.release)
	<-done
	if n := r.sup.Stopping(); n != 1 {
		t.Errorf("Stopping() = %d with the instance enrolled, want 1", n)
	}
}
