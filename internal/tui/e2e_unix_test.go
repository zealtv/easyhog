//go:build unix

package tui

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	tea "github.com/charmbracelet/bubbletea"

	"github.com/zealtv/easyhog/internal/core"
	"github.com/zealtv/easyhog/internal/discovery"
	"github.com/zealtv/easyhog/internal/groundhog"
	"github.com/zealtv/easyhog/internal/runner"
)

// The shell, the supervisor and the refresher against the pinned upstream
// script, on a temporary instance only: the reader runs an instance from the
// palette, the view reconciles with protocol status (running, ours), pause
// leaves pending deliveries pending, and Quit releases. Nothing here touches
// this repository's own .groundhog/ or any real schedule.

const (
	pinnedScript = "../groundhog/testdata/upstream-1c68c9d/groundhog.sh"
	pinnedSHA256 = "298c9d5d11465607796043d44c5e8dc42a20912aa52c00988505aaf8be09dbb9"
)

// reaper starts real runners and makes sure none outlives its test.
type reaper struct {
	mu       sync.Mutex
	children []runner.Child
}

func (l *reaper) Start(ln runner.Launch) (runner.Child, error) {
	c, err := runner.Spawner{}.Start(ln)
	if err == nil {
		l.mu.Lock()
		l.children = append(l.children, c)
		l.mu.Unlock()
	}
	return c, err
}

func (l *reaper) reapAll(t *testing.T) {
	l.mu.Lock()
	children := append([]runner.Child(nil), l.children...)
	l.mu.Unlock()
	for _, c := range children {
		_ = c.Terminate()
		select {
		case <-c.Done():
		case <-time.After(10 * time.Second):
			t.Errorf("runner %d outlived its test", c.PID())
		}
	}
}

// countingLauncher counts runner starts, to show that a double Run enrolls and
// starts once.
type countingLauncher struct {
	*reaper
	mu     sync.Mutex
	starts int
}

func (l *countingLauncher) Start(ln runner.Launch) (runner.Child, error) {
	l.mu.Lock()
	l.starts++
	l.mu.Unlock()
	return l.reaper.Start(ln)
}

func (l *countingLauncher) count() int { l.mu.Lock(); defer l.mu.Unlock(); return l.starts }

// countingExec counts calls per protocol command.
type countingExec struct {
	runner.Executor
	mu     sync.Mutex
	counts map[string]int
}

func (e *countingExec) Run(ctx context.Context, inv runner.Invocation) error {
	e.mu.Lock()
	if e.counts == nil {
		e.counts = map[string]int{}
	}
	e.counts[inv.Args[0]]++
	e.mu.Unlock()
	return e.Executor.Run(ctx, inv)
}

func (e *countingExec) count(cmd string) int { e.mu.Lock(); defer e.mu.Unlock(); return e.counts[cmd] }

type e2e struct {
	t        *testing.T
	key      string
	sup      *core.Supervisor
	ref      *core.Refresher
	launcher *countingLauncher
	exec     *countingExec
	m        Model
}

func newE2E(t *testing.T) *e2e {
	t.Helper()
	script, err := os.ReadFile(pinnedScript)
	if err != nil {
		t.Fatal(err)
	}
	if sum := sha256.Sum256(script); hex.EncodeToString(sum[:]) != pinnedSHA256 {
		t.Fatalf("%s no longer matches the pinned digest; update the pin deliberately", pinnedScript)
	}
	root, err := filepath.EvalSymlinks(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	inst := filepath.Join(root, "proj", ".groundhog")
	for _, d := range []string{"schedule", "out", "fired", "runtime"} {
		if err := os.MkdirAll(filepath.Join(inst, d), 0o755); err != nil {
			t.Fatal(err)
		}
	}
	if err := os.WriteFile(filepath.Join(inst, "groundhog.sh"), script, 0o755); err != nil {
		t.Fatal(err)
	}
	// A loose directory in out/ reads as a pending delivery (legacy_out,
	// unregistered) and nothing will consume it.
	if err := os.MkdirAll(filepath.Join(inst, "out", "waiting"), 0o755); err != nil {
		t.Fatal(err)
	}

	exec := &countingExec{Executor: runner.Process{}}
	launcher := &countingLauncher{reaper: &reaper{}}
	stateDir, err := filepath.EvalSymlinks(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	sup, err := core.NewSupervisor(core.SupervisorConfig{Exec: exec, Launcher: launcher, StateDir: stateDir, Label: "e2e-session",
		PollEvery: 50 * time.Millisecond, BusyDelay: 100 * time.Millisecond, StartWait: 10 * time.Second})
	if err != nil {
		t.Fatal(err)
	}
	ref, err := core.NewRefresher(core.Config{Root: root, FS: discovery.OS{}, Exec: runner.Process{}, IsOurs: sup.IsOurs,
		Interval: 200 * time.Millisecond, SelectSettle: 10 * time.Millisecond})
	if err != nil {
		t.Fatal(err)
	}
	if err := ref.Rescan(context.Background()); err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan struct{})
	go func() { defer close(done); _ = ref.Run(ctx) }()
	t.Cleanup(func() {
		qctx, qcancel := context.WithTimeout(context.Background(), 30*time.Second)
		defer qcancel()
		sup.Quit(qctx)
		cancel()
		<-done
		launcher.reapAll(t)
	})

	e := &e2e{t: t, key: inst, sup: sup, ref: ref, launcher: launcher, exec: exec}
	e.m = NewWithData(e.data(), WithSelectionObserver(ref.Select), WithRefresh(ref.Refresh),
		WithRunners(sup, ref.Reinspect))
	e.send(tea.WindowSizeMsg{Width: 100, Height: 30})
	ref.Select(e.m.selectedInstance()) // stands for the reader's first key press, which reports the selection
	return e
}

func (e *e2e) data() Data { return FromViewWith(e.ref.View(), e.sup.Runner) }

// send feeds messages to the model and then every message its commands
// produce, running the commands to completion (Run takes a moment).
func (e *e2e) send(msgs ...tea.Msg) {
	e.t.Helper()
	for len(msgs) > 0 {
		msg := msgs[0]
		msgs = msgs[1:]
		if msg == nil {
			continue
		}
		next, cmd := e.m.Update(msg)
		e.m = next.(Model)
		msgs = append(msgs, run(cmd)...)
	}
}

// settle feeds the refresher's current view into the model until cond holds.
func (e *e2e) settle(what string, cond func(core.InstanceView) bool) {
	e.t.Helper()
	for deadline := time.Now().Add(20 * time.Second); time.Now().Before(deadline); time.Sleep(25 * time.Millisecond) {
		v := e.ref.View()
		if iv, ok := v.Find(e.key); ok && cond(iv) {
			e.send(SnapshotMsg{Data: e.data()})
			return
		}
	}
	e.t.Fatalf("timed out waiting for %s", what)
}

func (e *e2e) palette(command string) {
	e.t.Helper()
	e.send(typeText(":" + command)...)
	e.send(key(tea.KeyEnter))
}

func (e *e2e) status() groundhog.Status {
	e.t.Helper()
	c, err := groundhog.New(runner.Process{}, e.key)
	if err != nil {
		e.t.Fatal(err)
	}
	st, err := c.Status(context.Background())
	if err != nil {
		e.t.Fatal(err)
	}
	return st
}

// pendingDeliveries counts pending deliveries; the fixture has no schedule
// item, so the runner can add none, and the one it starts with is out/waiting.
func pendingDeliveries(st groundhog.Status) int {
	n := 0
	for _, d := range st.Deliveries {
		if d.State == groundhog.DeliveryPending && d.Path == "out/waiting" {
			n++
		}
	}
	return n
}

func TestEndToEndRunStopRunQuitOnATempInstance(t *testing.T) {
	e := newE2E(t)
	if sup, err := (func() (groundhog.Support, error) {
		c, err := groundhog.New(runner.Process{}, e.key)
		if err != nil {
			return groundhog.Support{}, err
		}
		return c.Negotiate(context.Background())
	})(); err != nil || !sup.Controllable {
		t.Skipf("this host cannot run supervised control: %+v %v", sup, err)
	}

	// Not inspected yet: no Run. Its first status read (selection settling)
	// makes it unowned and controllable, and Run appears.
	if names := (&rig{m: e.m}).names(); names != "Open folder,Refresh,Quit" {
		t.Fatalf("an uninspected instance offers %s", names)
	}
	e.settle("the first status read", func(iv core.InstanceView) bool { return iv.Class == core.ClassUnowned })
	if e.m.selectedInstance() != e.key {
		t.Fatalf("selected %q", e.m.selectedInstance())
	}
	if names := (&rig{m: e.m}).names(); names != "Run,Open folder,Refresh,Quit" {
		t.Fatalf("after the first read the palette offers %s", names)
	}
	if n := pendingDeliveries(e.status()); n != 1 {
		t.Fatalf("setup: %d pending deliveries", n)
	}

	// Run, from the palette: the runner starts, the notice says so, and the
	// view reconciles with protocol status: running, ours, Pause offered.
	e.palette("run")
	if want := "Run: proj: enrolled for this session, runner started"; !strings.HasPrefix(e.m.notice, want) || e.m.failure {
		t.Fatalf("notice %q (failure %v)", e.m.notice, e.m.failure)
	}
	e.settle("the view to show the runner running and ours", func(iv core.InstanceView) bool {
		return iv.Class == core.ClassOwnedBySession && iv.Inspection.Last != nil && iv.Inspection.Last.Status.Runner.State == groundhog.RunnerRunning
	})
	st := e.status()
	if st.Owner == nil || st.Owner.Label != "e2e-session" || st.Runner.State != groundhog.RunnerRunning || st.Paused {
		t.Fatalf("status after Run: %+v", st)
	}
	if n := e.m.rows[0].node; n.State != StateRunning || !strings.Contains(n.Detail, "running") {
		t.Fatalf("row %v %q", n.State, n.Detail)
	}
	if names := (&rig{m: e.m}).names(); names != "Stop,Open folder,Refresh,Quit" {
		t.Fatalf("running and ours: the palette offers %s", names)
	}

	// Double invocation: a second Run (the palette no longer offers it; the
	// supervisor, asked directly, is idempotent) enrolls and starts nothing more.
	if res, err := e.sup.Run(context.Background(), e.key); err != nil || res.Outcome != core.RunAlreadyRunning {
		t.Fatalf("second Run: %+v %v", res, err)
	}
	if e.exec.count("enroll") != 1 || e.launcher.count() != 1 {
		t.Fatalf("enrolls %d, runner starts %d", e.exec.count("enroll"), e.launcher.count())
	}

	// Stop does what Quit does for one instance: the runner ends, the instance is
	// released and the token goes, the pending delivery stays pending, nothing is
	// paused, and the offer flips to Run.
	e.palette("stop")
	if !strings.HasPrefix(e.m.notice, "Stop: proj: stopped and released.") || e.m.failure {
		t.Fatalf("notice %q", e.m.notice)
	}
	e.settle("the view to show the instance released", func(iv core.InstanceView) bool {
		return iv.Class == core.ClassUnowned && iv.Inspection.Last != nil
	})
	st = e.status()
	if st.Owner != nil || st.Paused || st.Runner.State != groundhog.RunnerStopped || pendingDeliveries(st) != 1 {
		t.Fatalf("status after Stop: owner %v paused %v runner %v, %d pending", st.Owner, st.Paused, st.Runner.State, pendingDeliveries(st))
	}
	if n := e.m.rows[0].node; n.State != StateStopped || n.State.label() != "stopped" {
		t.Fatalf("after Stop the row is %v %q, want stopped", n.State, n.Detail)
	}
	if names := (&rig{m: e.m}).names(); names != "Run,Open folder,Refresh,Quit" {
		t.Fatalf("stopped: the palette offers %s", names)
	}

	// Run again enrols it afresh and starts a second runner.
	e.palette("run")
	if want := "Run: proj: enrolled for this session, runner started"; !strings.HasPrefix(e.m.notice, want) || e.m.failure {
		t.Fatalf("notice %q (failure %v)", e.m.notice, e.m.failure)
	}
	if e.launcher.count() != 2 || e.exec.count("enroll") != 2 {
		t.Fatalf("runner starts %d, enrolls %d", e.launcher.count(), e.exec.count("enroll"))
	}
	e.settle("the view to show it running and ours again", func(iv core.InstanceView) bool {
		return iv.Class == core.ClassOwnedBySession && iv.Inspection.Last != nil && iv.Inspection.Last.Status.Runner.State == groundhog.RunnerRunning
	})

	// Quit: the palette ends the shell, and the one Quit that main always runs
	// next stops and releases.
	e.send(typeText(":quit")...)
	_, cmd := e.m.Update(key(tea.KeyEnter))
	if !quits(cmd) {
		t.Fatal("palette Quit did not quit")
	}
	ctx, cancel := context.WithTimeout(context.Background(), 60*time.Second)
	defer cancel()
	rep := e.sup.Quit(ctx)
	if !rep.Clean() || len(rep.Instances) != 1 || rep.Instances[0].Outcome != core.QuitReleased {
		t.Fatalf("quit report %+v", rep)
	}
	st = e.status()
	if st.Owner != nil || st.Runner.State == groundhog.RunnerRunning {
		t.Fatalf("status after Quit: owner %+v runner %s", st.Owner, st.Runner.State)
	}
	if pendingDeliveries(st) != 1 {
		t.Fatalf("pending deliveries after Quit: %d", pendingDeliveries(st))
	}
}

// An instance owned by someone else gets no Run, and Run typed anyway finds
// nothing: the real script, with an owner enrolled by another client.
func TestEndToEndAnExternalOwnerIsNeverOffered(t *testing.T) {
	e := newE2E(t)
	other, err := core.NewSupervisor(core.SupervisorConfig{Exec: runner.Process{}, Launcher: e.launcher, StateDir: t.TempDir(), Label: "someone-else"})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { other.Quit(context.Background()) })
	if res, err := other.Run(context.Background(), e.key); err != nil {
		if strings.Contains(err.Error(), "controllable") {
			t.Skip(err)
		}
		t.Fatalf("setup: %+v %v", res, err)
	}
	e.settle("the instance to read as externally owned", func(iv core.InstanceView) bool { return iv.Class == core.ClassExternal })
	if names := (&rig{m: e.m}).names(); names != "Open folder,Refresh,Quit" {
		t.Fatalf("an external owner's instance offers %s", names)
	}
	e.send(typeText(":run")...)
	if got := e.m.filtered(); len(got) != 0 {
		t.Fatalf("Run found: %v", got)
	}
	// Asked directly, the supervisor refuses too, and says why.
	_, err = e.sup.Run(context.Background(), e.key)
	if text := ActionFailure("proj/.groundhog", err); text == nil || !strings.HasPrefix(text.Error(), "proj/.groundhog: refused: owned by another supervisor: owner \"someone-else\" is recorded") {
		t.Fatalf("refusal %v", text)
	}
}
