//go:build unix

package core

import (
	"context"
	"testing"
	"time"

	"github.com/zealtv/easyhog/internal/groundhog"
)

// A session killed while its runner runs leaves the runner ticking and its token
// in its directory. A second session adopts the token, and Stop, Run and Quit
// work through it against the pinned script. The runner is the first session's
// child, never the second's: Quit reaches it through `stop`, never a signal.

func TestRealAKilledSessionsTokenIsAdoptedThenStoppedRunAgainAndQuit(t *testing.T) {
	r := newRealRig(t, "", false)
	r.run()
	first := r.l.last() // the killed session's child
	was := r.runnerInfo().TokenFile
	st := r.status()
	if st.Owner == nil || st.Runner.State != groundhog.RunnerRunning {
		t.Fatalf("status %+v", st)
	}
	killSession(t, r.sup) // SIGKILL as far as the lock is concerned: the OS drops it, nothing else changes

	l2 := &trackingLauncher{}
	b, err := NewSupervisor(SupervisorConfig{
		Exec: r.exec, Launcher: l2, StateDir: r.state, Label: "second",
		PollEvery: 50 * time.Millisecond, BusyDelay: 100 * time.Millisecond, StartWait: 10 * time.Second,
	})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
		defer cancel()
		b.Quit(ctx)
		l2.reapAll(t)
	})
	if got := b.Adopt(everywhere); len(got) != 1 || got[0] != r.key {
		t.Fatalf("adopted %v", got)
	}
	if fileExists(was) {
		t.Error("the token is still in the ended session's directory")
	}
	info, _ := b.Runner(r.key)
	if !info.Adopted || info.PID != 0 || !fileExists(info.TokenFile) {
		t.Fatalf("info %+v", info)
	}
	// By the one rule the instance is the second session's: its token matches.
	if !b.IsOurs(*st.Owner) {
		t.Fatal("the adopted token does not match the owner status shows")
	}
	b.Observe(context.Background(), r.key, st) // a coherent status that agrees changes nothing
	if !fileExists(info.TokenFile) {
		t.Fatal("the rule deleted a token that matches")
	}

	// Stop works through the token: the adopted runner is not this session's child,
	// so it ends through `stop` alone, and the instance is released.
	sres, err := b.Stop(context.Background(), r.key)
	if err != nil || sres.Terminated {
		t.Fatalf("Stop %+v %v", sres, err)
	}
	if st := r.status(); st.Owner != nil || st.Runner.State != groundhog.RunnerStopped || st.Paused {
		t.Fatalf("after Stop: owner %v runner %v paused %v (want released, stopped, not paused)", st.Owner, st.Runner.State, st.Paused)
	}
	if _, ok := b.Runner(r.key); ok || fileExists(info.TokenFile) {
		t.Fatalf("the session still holds a token for a released instance")
	}
	select {
	case <-first.Done():
		if x := first.Exit(); !x.OK() {
			t.Errorf("runner exit %+v, want a clean exit", x)
		}
	case <-time.After(5 * time.Second):
		t.Error("the adopted runner is still alive after Stop")
	}
	// Run enrols it afresh, and starts a runner of this session's own.
	res, err := b.Run(context.Background(), r.key)
	if err != nil || res.Outcome != RunStarted || !res.Enrolled || l2.count() != 1 {
		t.Fatalf("Run %+v %v, %d runners started", res, err, l2.count())
	}
	if info, _ := b.Runner(r.key); info.Adopted {
		t.Error("a runner of this session's own still reads as adopted")
	}

	rep := b.Quit(context.Background())
	if len(rep.Instances) != 1 || rep.Instances[0].Outcome != QuitReleased || rep.Instances[0].Terminated || !rep.Clean() || len(rep.Notices) != 0 {
		t.Fatalf("report %+v", rep)
	}
	st = r.status()
	if st.Owner != nil || st.Runner.State != groundhog.RunnerStopped {
		t.Errorf("after Quit: owner %v runner %v (want released, stopped)", st.Owner, st.Runner.State)
	}
	if left := filesUnder(t, r.state); len(left) != 0 {
		t.Errorf("tokens or logs left: %v", left)
	}
	if r.exec.count("release") != 2 {
		t.Errorf("%d releases, want one for Stop and one for Quit", r.exec.count("release"))
	}
}

// A token the instance no longer recognises, adopted and never read, is settled
// at Quit by the one rule, against the real script's refusal.
func TestRealAnAdoptedTokenThatControlsNothingIsSettledAtQuit(t *testing.T) {
	r := newRealRig(t, "", false)
	r.run()
	token := r.runnerInfo().TokenFile
	// Quit by hand: stop the runner and release, as the person would, leaving the
	// first session's token behind.
	c := r.client()
	ctx := context.Background()
	if err := c.Stop(ctx, token); err != nil {
		t.Fatal(err)
	}
	if err := c.Release(ctx, token); err != nil {
		t.Fatal(err)
	}
	killSession(t, r.sup)

	l2 := &trackingLauncher{}
	b, err := NewSupervisor(SupervisorConfig{Exec: r.exec, Launcher: l2, StateDir: r.state, Label: "second", BusyDelay: 100 * time.Millisecond})
	if err != nil {
		t.Fatal(err)
	}
	if got := b.Adopt(everywhere); len(got) != 1 {
		t.Fatalf("adopted %v", got)
	}
	adopted := mustRunnerOf(t, b, r.key).TokenFile
	rep := b.Quit(ctx)
	in := rep.Instances[0]
	if in.Outcome != QuitNotOwned || !rep.Clean() {
		t.Fatalf("%+v", in)
	}
	if fileExists(adopted) || len(filesUnder(t, r.state)) != 0 {
		t.Errorf("tokens left: %v", filesUnder(t, r.state))
	}
	if r.exec.count("release") != 0 {
		t.Errorf("%d releases: Quit released an instance that was not this session's", r.exec.count("release"))
	}
}
