//go:build unix

package acceptance

import (
	"bufio"
	"context"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"syscall"
	"testing"
	"time"

	"github.com/zealtv/easyhog/internal/core"
	"github.com/zealtv/easyhog/internal/groundhog"
	"github.com/zealtv/easyhog/internal/runner"
	"github.com/zealtv/easyhog/internal/tui"
)

// A session killed (SIGKILL) while its runner runs, and a second session that
// adopts its token, stops the instance, runs it again and quits it.
//
// The first session is a process of its own: this test binary, run again with
// the helper test below, which runs one instance through a real Supervisor and
// then waits. SIGKILL gives it no chance to quit, so the runner (which has its
// own process group) keeps ticking and the token stays in the dead session's
// directory, whose lock the OS has dropped.

const helperEnv = "EASYHOG_ACCEPTANCE_HELPER"

// TestHelperSessionToKill is not a test. It is the first Easyhog session of
// TestKilledSessionIsAdoptedStoppedRunAgainAndQuitBySecond, and runs only when that test
// starts it.
func TestHelperSessionToKill(t *testing.T) {
	if os.Getenv(helperEnv) != "session" {
		t.Skip("helper process for TestKilledSessionIsAdoptedStoppedRunAgainAndQuitBySecond")
	}
	sup, err := core.NewSupervisor(core.SupervisorConfig{
		Exec: runner.Process{}, Launcher: runner.Spawner{}, StateDir: os.Getenv(helperEnv + "_STATE"), Label: "easyhog-acceptance",
		RunInterval: time.Second, PollEvery: 100 * time.Millisecond, BusyDelay: 150 * time.Millisecond, StartWait: 15 * time.Second,
	})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := sup.Run(context.Background(), os.Getenv(helperEnv+"_INSTANCE")); err != nil {
		t.Fatal(err)
	}
	os.Stdout.WriteString("HELPER-READY\n")
	select {} // until the SIGKILL
}

func TestKilledSessionIsAdoptedStoppedRunAgainAndQuitBySecond(t *testing.T) {
	script := gate(t)
	w := newWorld(t, script)
	inst := w.instance("solo")
	w.start() // the reader's session; it holds nothing yet
	state := filepath.Join(w.tmp, "easy-state")

	// The first session, in its own process.
	cmd := exec.Command(os.Args[0], "-test.run=^TestHelperSessionToKill$", "-test.count=1")
	cmd.Env = append(os.Environ(), helperEnv+"=session", helperEnv+"_STATE="+state, helperEnv+"_INSTANCE="+inst)
	out, err := cmd.StdoutPipe()
	if err != nil {
		t.Fatal(err)
	}
	cmd.Stderr = os.Stderr
	if err := cmd.Start(); err != nil {
		t.Fatal(err)
	}
	reaped := false
	t.Cleanup(func() {
		if !reaped {
			_ = cmd.Process.Kill()
			_ = cmd.Wait()
		}
	})
	ready := make(chan bool, 1)
	go func() {
		sc := bufio.NewScanner(out)
		for sc.Scan() {
			if strings.Contains(sc.Text(), "HELPER-READY") {
				ready <- true
				return
			}
		}
		ready <- false
	}()
	select {
	case ok := <-ready:
		if !ok {
			t.Fatal("the first session ended before it ran the instance")
		}
	case <-time.After(90 * time.Second):
		t.Fatal("the first session never ran the instance")
	}
	w.await("the first session's runner to run", 30*time.Second, func() bool { return w.status(inst).Runner.State == groundhog.RunnerRunning })
	if st := w.status(inst); st.Owner == nil || st.Owner.Label != "easyhog-acceptance" {
		t.Fatalf("owner %+v", st.Owner)
	}

	// While it lives, its directory is off limits: nothing is adopted.
	if got := w.sup.Adopt(w.inRoot); len(got) != 0 {
		t.Fatalf("adopted %v from a session that was alive", got)
	}

	// SIGKILL. The runner is not its child in any sense that matters: it goes on.
	if err := syscall.Kill(cmd.Process.Pid, syscall.SIGKILL); err != nil {
		t.Fatal(err)
	}
	_ = cmd.Wait()
	reaped = true
	time.Sleep(1500 * time.Millisecond) // longer than a runner cycle
	if st := w.status(inst); st.Runner.State != groundhog.RunnerRunning || runnerProcesses(t, inst) != 1 {
		t.Fatalf("after SIGKILL: runner %s, %d processes; the runner should have lived on", st.Runner.State, runnerProcesses(t, inst))
	}

	// The reader's session notices (cmd/easyhog does this at launch and every
	// rescan) and takes the token.
	if got := w.sup.Adopt(w.inRoot); len(got) != 1 || got[0] != inst {
		t.Fatalf("adopted %v", got)
	}
	info, ok := w.sup.Runner(inst)
	if !ok || !info.Adopted || info.PID != 0 {
		t.Fatalf("runner info %+v %v", info, ok)
	}
	// Nothing was restarted, and the row reads as it is: running, ours.
	w.selectInstance("solo")
	n := w.node("solo")
	if n.State != tui.StateRunning || !hasAction(n, tui.ActionStop) || hasAction(n, tui.ActionRun) {
		t.Fatalf("adopted row: state %v actions %v\n%s", n.State, n.Actions, n.Preview)
	}
	if !strings.Contains(n.Preview, "Adopted from a session that ended at") {
		t.Errorf("the preview does not say where the token came from:\n%s", n.Preview)
	}
	if w.easy.startsFor(inst) != 0 || runnerProcesses(t, inst) != 1 {
		t.Fatalf("adoption started a runner: %d launches, %d processes", w.easy.startsFor(inst), runnerProcesses(t, inst))
	}

	// Stop through the palette works through the adopted token: the runner, which
	// is not this session's child, ends through `stop` alone, and the instance is released.
	if w.palette("stop") {
		t.Fatal("Stop quit the program")
	}
	w.await("the instance to be released", 30*time.Second, func() bool { return w.status(inst).Owner == nil })
	w.reread("solo")
	if st := w.status(inst); st.Runner.State == groundhog.RunnerRunning || st.Paused {
		t.Errorf("after Stop: runner %s, paused %v", st.Runner.State, st.Paused)
	}
	if n := runnerProcesses(t, inst); n != 0 {
		t.Errorf("%d runner processes after Stop", n)
	}
	if left, _ := filepath.Glob(filepath.Join(state, "sessions", "*", "*.token")); len(left) != 0 {
		t.Errorf("tokens left behind after Stop: %v", left)
	}
	if n := w.node("solo"); n.State != tui.StateStopped || !hasAction(n, tui.ActionRun) || hasAction(n, tui.ActionStop) {
		t.Errorf("after Stop: state %v actions %v", n.State, n.Actions)
	}

	// Run enrols it afresh and starts a runner of this session's own.
	w.palette("run")
	w.await("the runner to run again", 30*time.Second, func() bool { return w.status(inst).Runner.State == groundhog.RunnerRunning })
	if w.easy.startsFor(inst) != 1 {
		t.Errorf("Run after Stop started %d runners, want one", w.easy.startsFor(inst))
	}

	// Quit stops and releases it.
	if !w.palette("quit") {
		t.Fatal("the palette's Quit did not quit")
	}
	ctx, cancel := context.WithTimeout(w.ctx, 120*time.Second)
	defer cancel()
	rep := w.sup.Quit(ctx)
	if !rep.Clean() || len(rep.Notices) != 0 || len(rep.Instances) != 1 {
		t.Fatalf("quit report: %+v", rep)
	}
	if in := rep.Instances[0]; in.Outcome != core.QuitReleased || in.Terminated || !in.RunnerGone {
		t.Errorf("%+v", in)
	}
	st := w.status(inst)
	if st.Owner != nil || st.Runner.State == groundhog.RunnerRunning || st.Paused {
		t.Errorf("after Quit: owner %+v, runner %s, paused %v (want released, ended, not paused)", st.Owner, st.Runner.State, st.Paused)
	}
	if n := runnerProcesses(t, inst); n != 0 {
		t.Errorf("%d runner processes after Quit", n)
	}
	if left, _ := filepath.Glob(filepath.Join(state, "sessions", "*")); len(left) != 0 {
		t.Errorf("tokens, logs or directories left behind: %v", left)
	}
}
