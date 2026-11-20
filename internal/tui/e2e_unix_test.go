//go:build unix

package tui

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
	"time"

	tea "github.com/charmbracelet/bubbletea"

	"github.com/zealtv/easyhog/internal/core"
	"github.com/zealtv/easyhog/internal/discovery"
	"github.com/zealtv/easyhog/internal/groundhog"
	"github.com/zealtv/easyhog/internal/runner"
)

// The shell, the ticker and the refresher against the pinned upstream script,
// on a temporary instance only: the reader runs an instance from the palette,
// a due item's run is accepted, the view reconciles with protocol status, the
// instance pause holds it back, and Quit just stops ticking. Nothing here
// touches this repository's own .groundhog/ or any real schedule.

const (
	pinnedScript = "../groundhog/testdata/upstream-9317d4e/groundhog.sh"
	pinnedSHA256 = "5a961d101ffc6d9186b836bd1f140c4c78e0b92770d682858a5a799d0cc562c2"
)

type e2e struct {
	t   *testing.T
	key string
	tk  *core.Ticker
	ref *core.Refresher
	m   Model
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
	if err := os.MkdirAll(inst, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(inst, "groundhog.sh"), script, 0o755); err != nil {
		t.Fatal(err)
	}
	if out, err := exec.Command(filepath.Join(inst, "groundhog.sh"), "init").CombinedOutput(); err != nil {
		t.Fatalf("init: %v %s", err, out)
	}
	// A daily item due now, whose run leaves a mark beside the instance.
	item := filepath.Join(inst, "schedule", "daily", "hello")
	if err := os.MkdirAll(item, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(item, "run"), []byte("#!/bin/sh\necho hello > \""+filepath.Join(root, "proj", "ran")+"\"\n"), 0o755); err != nil {
		t.Fatal(err)
	}

	clients := core.NewClients(runner.Process{})
	tk, err := core.NewTicker(core.TickerConfig{Clients: clients})
	if err != nil {
		t.Fatal(err)
	}
	ref, err := core.NewRefresher(core.Config{Root: root, FS: discovery.OS{}, Exec: runner.Process{}, Clients: clients,
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
		tk.Quit(context.Background())
		cancel()
		<-done
	})
	e := &e2e{t: t, key: inst, tk: tk, ref: ref}
	if sup, err := e.client().Negotiate(context.Background()); err != nil || !sup.Controllable {
		t.Skipf("this host offers no runs: %+v %v", sup, err)
	}
	e.m = NewWithData(e.data(), WithSelectionObserver(ref.Select), WithRefresh(ref.Refresh), WithControl(tk, ref.Reinspect))
	e.send(tea.WindowSizeMsg{Width: 100, Height: 30})
	ref.Select(e.m.selectedInstance()) // stands for the reader's first key press, which reports the selection
	return e
}

func (e *e2e) data() Data { return FromViewWith(e.ref.View(), e.tk.Info) }

func (e *e2e) client() *groundhog.Client {
	c, err := groundhog.New(runner.Process{}, e.key)
	if err != nil {
		e.t.Fatal(err)
	}
	return c
}

// send feeds messages to the model and then every message its commands
// produce, running the commands to completion (Run takes a tick).
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
		if iv, ok := e.ref.View().Find(e.key); ok && cond(iv) {
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

func (e *e2e) names() string { return (&rig{m: e.m}).names() }

func delivered(state groundhog.DeliveryState) func(core.InstanceView) bool {
	return func(iv core.InstanceView) bool {
		if iv.Inspection.Last == nil {
			return false
		}
		for _, d := range iv.Inspection.Last.Status.Deliveries {
			if d.State == state {
				return true
			}
		}
		return false
	}
}

func TestEndToEndRunPauseStopRunQuitOnATempInstance(t *testing.T) {
	e := newE2E(t)

	// Not inspected yet: nothing offered. Its first status read makes it
	// supported, and Run appears.
	if names := e.names(); names != "Open folder,Refresh,Quit" {
		t.Fatalf("an uninspected instance offers %s", names)
	}
	e.settle("the first status read", func(iv core.InstanceView) bool { return iv.Class == core.ClassSupported })
	if names := e.names(); names != "Run,Pause instance,Open folder,Refresh,Quit" {
		t.Fatalf("after the first read the palette offers %s", names)
	}

	// Run ticks it: the item fires and its run is accepted.
	e.palette("run")
	if want := "Run: proj: ticked, and ticking every minute"; !strings.HasPrefix(e.m.notice, want) || e.m.failure {
		t.Fatalf("notice %q (failure %v)", e.m.notice, e.m.failure)
	}
	e.settle("the run to be accepted", delivered(groundhog.DeliveryAccepted))
	if got, _ := os.ReadFile(filepath.Join(filepath.Dir(filepath.Dir(e.key)), "proj", "ran")); string(got) != "hello\n" {
		t.Errorf("the item's run left %q", got)
	}
	if n := e.m.rows[0].node; n.State != StateRunning {
		t.Fatalf("row %v %q", n.State, n.Detail)
	}
	if names := e.names(); names != "Stop,Pause instance,Open folder,Refresh,Quit" {
		t.Fatalf("ticked here: the palette offers %s", names)
	}

	// Pause instance, then Unpause instance, through the palette.
	e.palette("pause in")
	if !strings.HasPrefix(e.m.notice, "Pause instance: proj: paused.") || e.m.failure {
		t.Fatalf("notice %q", e.m.notice)
	}
	e.settle("the view to show the pause", func(iv core.InstanceView) bool {
		return iv.Inspection.Last != nil && iv.Inspection.Last.Status.Paused
	})
	if n := e.m.rows[0].node; n.State != StatePaused || !strings.Contains(n.Detail, "this session still ticks it") {
		t.Fatalf("paused row %v %q", n.State, n.Detail)
	}
	e.palette("unpause")
	e.settle("the view to show it unpaused", func(iv core.InstanceView) bool {
		return iv.Inspection.Last != nil && !iv.Inspection.Last.Status.Paused
	})

	// Stop ends this session's ticking; Run starts it again.
	e.palette("stop")
	if !strings.HasPrefix(e.m.notice, "Stop: proj: stopped ticking.") || e.m.failure {
		t.Fatalf("notice %q", e.m.notice)
	}
	e.send(SnapshotMsg{Data: e.data()})
	if n := e.m.rows[0].node; n.State != StateStopped {
		t.Fatalf("after Stop the row is %v %q (this session's own last tick must not read external)", n.State, n.Detail)
	}
	e.palette("run")
	if !strings.HasPrefix(e.m.notice, "Run: proj: ticked") {
		t.Fatalf("notice %q", e.m.notice)
	}

	// Quit: the palette ends the shell, and main then stops every loop.
	e.send(typeText(":quit")...)
	if _, cmd := e.m.Update(key(tea.KeyEnter)); !quits(cmd) {
		t.Fatal("palette Quit did not quit")
	}
	began := time.Now()
	if n := e.tk.Quit(context.Background()); n != 1 || time.Since(began) > 5*time.Second {
		t.Fatalf("Quit stopped %d in %s", n, time.Since(began))
	}
	if info, _ := e.tk.Info(e.key); info.Ticking {
		t.Error("still ticking after Quit")
	}
}
