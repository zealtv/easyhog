package tui

import (
	"bytes"
	"context"
	"io"
	"os"
	"path/filepath"
	"sync"
	"testing"
	"time"

	tea "github.com/charmbracelet/bubbletea"

	"github.com/zealtv/easyhog/internal/core"
	"github.com/zealtv/easyhog/internal/discovery"
	"github.com/zealtv/easyhog/internal/runner"
)

// callLog is an executor that runs nothing and records every script call.
type callLog struct {
	mu    sync.Mutex
	calls []string
}

func (c *callLog) Run(_ context.Context, inv runner.Invocation) error {
	c.mu.Lock()
	c.calls = append(c.calls, filepath.Base(filepath.Dir(inv.Dir))+" "+inv.Args[0])
	c.mu.Unlock()
	return os.ErrNotExist // not a protocol answer; the test only counts calls
}

func (c *callLog) count() int { c.mu.Lock(); defer c.mu.Unlock(); return len(c.calls) }

// idleRig is the real refresher over temp instances with a call-recording
// executor, behind the model wired as cmd/easyhog wires it.
type idleRig struct {
	t    *testing.T
	log  *callLog
	ref  *core.Refresher
	root string
	m    Model
	stop func()
}

func newIdleRig(t *testing.T, names ...string) *idleRig {
	t.Helper()
	root, err := filepath.EvalSymlinks(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	for _, n := range names {
		inst := filepath.Join(root, n, ".groundhog")
		for _, d := range []string{"schedule", "out", "fired"} {
			if err := os.MkdirAll(filepath.Join(inst, d), 0o755); err != nil {
				t.Fatal(err)
			}
		}
		if err := os.WriteFile(filepath.Join(inst, "groundhog.sh"), []byte("#!/bin/sh\nexit 0\n"), 0o755); err != nil {
			t.Fatal(err)
		}
	}
	log := &callLog{}
	ref, err := core.NewRefresher(core.Config{Root: root, FS: discovery.OS{}, Exec: log,
		Interval: 20 * time.Millisecond, SelectSettle: 10 * time.Millisecond, Retries: -1})
	if err != nil {
		t.Fatal(err)
	}
	if err := ref.Rescan(context.Background()); err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan struct{})
	go func() { defer close(done); _ = ref.Run(ctx) }()
	r := &idleRig{t: t, log: log, ref: ref, root: root, stop: func() { cancel(); <-done }}
	t.Cleanup(r.stop)
	r.m = NewWithData(FromView(ref.View()), WithSelectionObserver(ref.Select), WithRefresh(ref.Refresh))
	r.send(tea.WindowSizeMsg{Width: 100, Height: 30})
	return r
}

func (r *idleRig) send(msgs ...tea.Msg) {
	r.t.Helper()
	for len(msgs) > 0 {
		next, cmd := r.m.Update(msgs[0])
		msgs = append(msgs[1:], run(cmd)...)
		r.m = next.(Model)
	}
}

func (r *idleRig) idle(d time.Duration) {
	// Snapshots keep arriving while the reader does nothing, as RunLive's feed does.
	for end := time.Now().Add(d); time.Now().Before(end); time.Sleep(10 * time.Millisecond) {
		r.send(SnapshotMsg{Data: FromView(r.ref.View())})
	}
}

func (r *idleRig) eventually(what string, cond func() bool) {
	r.t.Helper()
	for end := time.Now().Add(3 * time.Second); time.Now().Before(end); time.Sleep(5 * time.Millisecond) {
		if cond() {
			return
		}
	}
	r.t.Fatalf("timed out waiting for %s (calls: %v)", what, r.log.calls)
}

// No script runs before a keypress. Starting the model highlights the first
// instance; idling, with snapshots arriving and the palette opened and closed,
// runs no script. The first arrow key does, and only for the instance it lands
// on.
func TestIdleStartRunsNoScriptAndTheFirstArrowKeyDoes(t *testing.T) {
	r := newIdleRig(t, "a", "b")
	r.idle(300 * time.Millisecond) // many times the settle delay and the interval
	r.send(typeText(":")...)
	r.send(key(tea.KeyEsc))
	r.send(key(tea.KeyTab), key(tea.KeyTab), key(tea.KeyRight), key(tea.KeyLeft)) // not selecting keys
	r.idle(100 * time.Millisecond)
	if n := r.log.count(); n != 0 {
		t.Fatalf("%d script calls before any key that selects or moves: %v", n, r.log.calls)
	}

	r.send(key(tea.KeyDown))
	r.eventually("the first arrow key to read the instance it landed on", func() bool { return r.log.count() > 0 })
	for _, c := range r.log.calls {
		if c != "b capabilities" && c != "b status" {
			t.Errorf("call %q; only b was selected", c)
		}
	}
}

// With one instance the first arrow key cannot change the selection, but it is
// still the reader's first key that selects or moves, so the instance is read.
func TestFirstArrowKeyReadsEvenWhenTheSelectionCannotMove(t *testing.T) {
	r := newIdleRig(t, "only")
	r.idle(100 * time.Millisecond)
	if n := r.log.count(); n != 0 {
		t.Fatalf("%d script calls at startup", n)
	}
	r.send(key(tea.KeyUp))
	r.eventually("the first key to read the only instance", func() bool { return r.log.count() > 0 })
}

// An explicit Refresh is the other way in: it reads every instance, with no
// key press before it, and nothing else does.
func TestRefreshRunsScriptsWithoutAKeyPress(t *testing.T) {
	r := newIdleRig(t, "a", "b")
	r.idle(100 * time.Millisecond)
	if n := r.log.count(); n != 0 {
		t.Fatalf("%d script calls at startup", n)
	}
	r.send(typeText(":refresh")...)
	r.send(key(tea.KeyEnter))
	r.eventually("Refresh to read both instances", func() bool {
		seen := map[string]bool{}
		r.log.mu.Lock()
		defer r.log.mu.Unlock()
		for _, c := range r.log.calls {
			seen[c[:1]] = true
		}
		return seen["a"] && seen["b"]
	})
}

// A snapshot that changes the selected instance before the first key press
// (the highlighted one vanished) is not a key press either.
func TestASnapshotBeforeTheFirstKeyReportsNothing(t *testing.T) {
	var heard []string
	m := NewWithData(FromView(viewOf("a", "b")), WithSelectionObserver(func(k string) { heard = append(heard, k) }))
	m, _ = update(m, SnapshotMsg{FromView(viewOf("b"))})
	if len(heard) != 0 {
		t.Fatalf("heard %q before any key", heard)
	}
	m, _ = update(m, key(tea.KeyDown))
	if len(heard) != 1 {
		t.Fatalf("heard %q after the first arrow key; want the selection then in force", heard)
	}
	_ = m
}

// RunLive used to tell the observer the starting selection before the program
// ran. Running it for real (output to a buffer, no keys) must report nothing.
func TestRunLiveReportsNoSelectionBeforeAKey(t *testing.T) {
	var mu sync.Mutex
	var heard []string
	m := NewWithData(FromView(viewOf("a", "b")), WithSelectionObserver(func(k string) { mu.Lock(); heard = append(heard, k); mu.Unlock() }))
	ctx, cancel := context.WithTimeout(context.Background(), 400*time.Millisecond)
	defer cancel()
	in, w := io.Pipe()
	defer w.Close()
	var out bytes.Buffer
	_ = RunLive(ctx, m, in, &out, true, nil)
	mu.Lock()
	defer mu.Unlock()
	if len(heard) != 0 {
		t.Fatalf("the observer heard %q before any key", heard)
	}
	if out.Len() == 0 {
		t.Fatal("the program drew nothing; the test did not run the shell")
	}
}

// A rescan that removes the selected instance moves the highlight to a
// neighbour, but the reader chose nothing: the refresher is told "no
// selection" and runs no script until the next choosing key.
func TestARescanRemovingTheSelectedInstanceRunsNoScriptUntilAKey(t *testing.T) {
	r := newIdleRig(t, "a", "b", "c")
	r.send(key(tea.KeyDown))
	r.eventually("the arrow key to read b", func() bool { return r.log.count() > 0 })
	if err := os.RemoveAll(filepath.Join(r.root, "b")); err != nil {
		t.Fatal(err)
	}
	if err := r.ref.Rescan(context.Background()); err != nil {
		t.Fatal(err)
	}
	r.send(SnapshotMsg{Data: FromView(r.ref.View())})
	if k := r.m.selectedKey(); k == "" || filepath.Base(k) == "b" {
		t.Fatalf("highlight %q; want a neighbour", k)
	}
	r.idle(80 * time.Millisecond) // lets a read already in flight finish
	n := r.log.count()
	r.idle(300 * time.Millisecond) // many times the settle delay and the interval
	if got := r.log.count(); got != n {
		t.Fatalf("%d script calls after the rescan with no key: %v", got-n, r.log.calls)
	}
	r.send(key(tea.KeyDown))
	r.eventually("the next arrow key to read the new highlight", func() bool { return r.log.count() > n })
}

// An arrow that scrolls the preview is not the reader choosing an instance:
// nothing runs until an arrow in the tree.
func TestAPreviewFocusArrowBeforeAnyTreeKeyRunsNothing(t *testing.T) {
	r := newIdleRig(t, "a", "b")
	r.send(key(tea.KeyTab), key(tea.KeyDown), key(tea.KeyUp), key(tea.KeyPgDown))
	if r.m.focus != panePreview {
		t.Skip("Tab did not focus the preview at this size")
	}
	r.idle(300 * time.Millisecond)
	if n := r.log.count(); n != 0 {
		t.Fatalf("%d script calls after preview-only keys: %v", n, r.log.calls)
	}
	r.send(key(tea.KeyTab), key(tea.KeyDown))
	r.eventually("a tree arrow to read the instance", func() bool { return r.log.count() > 0 })
}

// The observer hears "" when a snapshot moves the highlight, then the instance
// again at the next choosing key, even if the highlight does not move.
func TestAMovedHighlightReportsNoSelectionThenTheNextKeyReports(t *testing.T) {
	var heard []string
	m := NewWithData(FromView(viewOf("a", "b", "c")), WithSelectionObserver(func(k string) { heard = append(heard, k) }))
	m, _ = update(m, tea.WindowSizeMsg{Width: 100, Height: 30}, key(tea.KeyDown))
	if len(heard) != 1 || heard[0] == "" {
		t.Fatalf("heard %q after the first arrow", heard)
	}
	m, _ = update(m, SnapshotMsg{FromView(viewOf("a", "c"))})
	if len(heard) != 2 || heard[1] != "" {
		t.Fatalf("heard %q after the rescan; want a final \"\"", heard)
	}
	m, _ = update(m, SnapshotMsg{FromView(viewOf("a", "c", "d"))})
	if len(heard) != 2 {
		t.Fatalf("heard %q; a second snapshot reports nothing more", heard)
	}
	_, _ = update(m, key(tea.KeyUp))
	if len(heard) != 3 || heard[2] == "" {
		t.Fatalf("heard %q after the next arrow", heard)
	}
}
