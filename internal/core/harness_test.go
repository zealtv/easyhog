package core

import (
	"context"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"sync"
	"testing"
	"time"

	"github.com/zealtv/easyhog/internal/discovery"
	"github.com/zealtv/easyhog/internal/runner"
)

// fakeClock is time the test moves by hand.
type fakeClock struct {
	mu      sync.Mutex
	now     time.Time
	waiters []*waiter
}

type waiter struct {
	at time.Time
	ch chan time.Time
}

func newClock() *fakeClock {
	return &fakeClock{now: time.Date(2026, 11, 15, 9, 0, 0, 0, time.UTC)}
}

func (c *fakeClock) Now() time.Time {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.now
}

func (c *fakeClock) After(d time.Duration) <-chan time.Time {
	c.mu.Lock()
	defer c.mu.Unlock()
	w := &waiter{at: c.now.Add(d), ch: make(chan time.Time, 1)}
	c.waiters = append(c.waiters, w)
	return w.ch
}

// Advance moves time forward and fires every waiter now due.
func (c *fakeClock) Advance(d time.Duration) {
	c.mu.Lock()
	c.now = c.now.Add(d)
	now := c.now
	var keep []*waiter
	var due []*waiter
	for _, w := range c.waiters {
		if w.at.After(now) {
			keep = append(keep, w)
		} else {
			due = append(due, w)
		}
	}
	c.waiters = keep
	c.mu.Unlock()
	for _, w := range due {
		w.ch <- now
	}
}

func (c *fakeClock) pending() int {
	c.mu.Lock()
	defer c.mu.Unlock()
	return len(c.waiters)
}

// exitStatus stands in for *exec.ExitError.
type exitStatus int

func (e exitStatus) Error() string { return fmt.Sprintf("exit status %d", int(e)) }
func (e exitStatus) ExitCode() int { return int(e) }

// script decides what one fake script call does. n counts calls of that
// command on that instance, from 1. Block by waiting on ctx.
type script func(ctx context.Context, inst, cmd string, n int) (stdout string, err error)

type call struct{ inst, cmd string }

// fakeExec is a runner.Executor that never starts a process.
type fakeExec struct {
	fn     script
	mu     sync.Mutex
	calls  []call
	counts map[call]int
}

func newExec(fn script) *fakeExec {
	return &fakeExec{fn: fn, counts: map[call]int{}}
}

func (f *fakeExec) Run(ctx context.Context, inv runner.Invocation) error {
	c := call{inv.Dir, inv.Args[0]}
	f.mu.Lock()
	f.calls = append(f.calls, c)
	f.counts[c]++
	n := f.counts[c]
	f.mu.Unlock()
	out, err := f.fn(ctx, c.inst, c.cmd, n)
	if out != "" && inv.Stdout != nil {
		if _, werr := inv.Stdout.Write([]byte(out)); werr != nil {
			return werr
		}
	}
	return err
}

func (f *fakeExec) count(inst, cmd string) int {
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.counts[call{inst, cmd}]
}

// ran lists the instances any script call reached, sorted.
func (f *fakeExec) ran() []string {
	f.mu.Lock()
	defer f.mu.Unlock()
	seen := map[string]bool{}
	for _, c := range f.calls {
		seen[c.inst] = true
	}
	var out []string
	for k := range seen {
		out = append(out, k)
	}
	sort.Strings(out)
	return out
}

func (f *fakeExec) total() int {
	f.mu.Lock()
	defer f.mu.Unlock()
	return len(f.calls)
}

// controlCaps lists the six capabilities a supervised script has to offer.
func controlCaps() []string {
	return []string{"status-json-v1", "ownership-v1", "runner-v1", "pause-v1", "consumer-v1", "delivery-v1"}
}

func capsJSON(caps ...string) string {
	if caps == nil {
		caps = controlCaps()
	}
	b, _ := json.Marshal(map[string]any{"protocol": "groundhog-runtime", "protocol_version": 1, "capabilities": caps})
	return string(b)
}

// statusOpts shapes a synthetic status document.
type statusOpts struct {
	rev, gen   int64
	inconsist  bool
	owner      string
	eligible   bool
	itemPath   string
	observedAt string
	runner     string // runner state; "stopped" when empty
}

func statusJSON(inst string, o statusOpts) string {
	var owner any
	if o.owner != "" {
		owner = map[string]any{"id": o.owner, "generation": 1, "label": o.owner}
	}
	if o.observedAt == "" {
		o.observedAt = "2026-11-15T09:00:00Z"
	}
	if o.runner == "" {
		o.runner = "stopped"
	}
	sched := []any{}
	if o.itemPath != "" {
		sched = append(sched, map[string]any{
			"path": o.itemPath, "item_id": filepath.Base(o.itemPath), "cadence": nil,
			"paused_by": []string{}, "eligible_now": o.eligible, "fired_marker": nil, "fired": false,
			"firing_allowed": false, "blocked_by": []string{},
		})
	}
	b, _ := json.Marshal(map[string]any{
		"protocol": "groundhog-runtime", "protocol_version": 1, "instance": inst,
		"observed_at": o.observedAt, "revision": o.rev, "journal_generation": o.gen,
		"consistent": !o.inconsist, "paused": false, "owner": owner,
		"runner":     map[string]any{"state": o.runner, "last_error": nil},
		"consumer":   map[string]any{"configured": false, "valid": false},
		"schedule":   sched,
		"deliveries": []any{}, "diagnostics": []any{},
	})
	return string(b)
}

// healthy answers every instance with full capabilities and an unowned status.
func healthy(rev int64) script {
	return func(_ context.Context, inst, cmd string, _ int) (string, error) {
		switch cmd {
		case "capabilities":
			return capsJSON(), nil
		case "status":
			return statusJSON(inst, statusOpts{rev: rev}), nil
		}
		return "", exitStatus(1)
	}
}

// mkInstance creates <root>/<rel>/.groundhog with the three trays and an
// executable stub script, and returns its canonical path.
func mkInstance(t *testing.T, root, rel string) string {
	t.Helper()
	inst := filepath.Join(root, rel, ".groundhog")
	for _, d := range []string{"schedule", "out", "fired"} {
		if err := os.MkdirAll(filepath.Join(inst, d), 0o755); err != nil {
			t.Fatal(err)
		}
	}
	if err := os.WriteFile(filepath.Join(inst, "groundhog.sh"), []byte("#!/bin/sh\nexit 0\n"), 0o755); err != nil {
		t.Fatal(err)
	}
	return inst
}

func tempRoot(t *testing.T) string {
	t.Helper()
	root, err := filepath.EvalSymlinks(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	return root
}

// rig is a refresher over a temporary root with a fake clock and executor.
type rig struct {
	t      *testing.T
	root   string
	exec   *fakeExec
	clk    *fakeClock
	r      *Refresher
	cancel context.CancelFunc
	done   chan error
}

func newRig(t *testing.T, fn script, tune func(*Config)) *rig {
	t.Helper()
	g := &rig{t: t, root: tempRoot(t), exec: newExec(fn), clk: newClock()}
	cfg := Config{Root: g.root, FS: discovery.OS{}, Exec: g.exec, Clock: g.clk,
		Interval: 5 * time.Second, SelectSettle: 500 * time.Millisecond, DiscoverEvery: time.Hour,
		CallTimeout: 5 * time.Second, Concurrency: 4, RetryDelay: 300 * time.Millisecond}
	if tune != nil {
		tune(&cfg)
	}
	g.t = t
	r, err := NewRefresher(cfg)
	if err != nil {
		t.Fatal(err)
	}
	g.r = r
	return g
}

// start rescans and runs the loop, and waits until it is waiting on its timers.
func (g *rig) start() {
	g.t.Helper()
	ctx, cancel := context.WithCancel(context.Background())
	g.cancel = cancel
	if err := g.r.Rescan(ctx); err != nil {
		g.t.Fatal(err)
	}
	g.done = make(chan error, 1)
	go func() { g.done <- g.r.Run(ctx) }()
	g.t.Cleanup(g.stop)
	eventually(g.t, "the loop to arm its timers", func() bool { return g.clk.pending() >= 2 })
}

func (g *rig) stop() {
	if g.cancel == nil {
		return
	}
	g.cancel()
	select {
	case <-g.done:
	case <-time.After(5 * time.Second):
		g.t.Error("Run did not return after cancellation")
	}
	g.cancel = nil
}

// settle moves the clock past the selection delay.
func (g *rig) settle() { g.clk.Advance(g.r.Config().SelectSettle) }

func (g *rig) view() View { return g.r.View() }

func (g *rig) inst(v View, rel string) InstanceView {
	g.t.Helper()
	key := filepath.Join(g.root, rel, ".groundhog")
	iv, ok := v.Find(key)
	if !ok {
		g.t.Fatalf("no instance %s in view", rel)
	}
	return iv
}

func eventually(t *testing.T, what string, cond func() bool) {
	t.Helper()
	deadline := time.Now().Add(5 * time.Second)
	for time.Now().Before(deadline) {
		if cond() {
			return
		}
		time.Sleep(2 * time.Millisecond)
	}
	t.Fatalf("timed out waiting for %s", what)
}

// never asserts cond stays false for a short real interval.
func never(t *testing.T, what string, cond func() bool) {
	t.Helper()
	deadline := time.Now().Add(60 * time.Millisecond)
	for time.Now().Before(deadline) {
		if cond() {
			t.Fatalf("%s", what)
		}
		time.Sleep(2 * time.Millisecond)
	}
}

// drained reports whether a change signal was pending, consuming it.
func drained(r *Refresher) bool {
	select {
	case <-r.Changed():
		return true
	default:
		return false
	}
}
