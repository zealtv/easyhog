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

// armed waits for the loop to have re-armed its poll and rescan timers.
func (g *rig) armed() {
	g.t.Helper()
	eventually(g.t, "the loop to re-arm its timers", func() bool { return g.clk.pending() >= 2 })
}

// tick moves one poll interval and waits for the loop to re-arm.
func (g *rig) tick() {
	g.t.Helper()
	g.clk.Advance(g.r.Config().Interval)
	g.armed()
}

func status(g *rig, inst string) int { return g.exec.count(inst, "status") }

func TestOnlyTheSelectedInstanceRuns(t *testing.T) {
	g := newRig(t, healthy(), nil)
	a, b, c := mkInstance(t, g.root, "a"), mkInstance(t, g.root, "b"), mkInstance(t, g.root, "c")
	g.start()
	if g.exec.total() != 0 {
		t.Fatalf("starting and rescanning ran %d script calls", g.exec.total())
	}

	g.r.Select(a)
	eventually(t, "the settle timer", func() bool { return g.clk.pending() >= 3 })
	g.clk.Advance(g.r.Config().SelectSettle)
	eventually(t, "the first read of a", func() bool { return status(g, a) == 1 })
	for i := 2; i <= 4; i++ {
		g.tick()
		want := i
		eventually(t, "a polled again", func() bool { return status(g, a) >= want })
	}

	if got := g.exec.ran(); len(got) != 1 || got[0] != a {
		t.Fatalf("background refresh reached %q; want only %q", got, a)
	}
	v := g.view()
	for _, key := range []string{b, c} {
		iv, _ := v.Find(key)
		if iv.Class != ClassNotInspected || iv.Failing() || iv.Inspection.Last != nil {
			t.Errorf("%s: class %v health %v, want not inspected", key, iv.Class, iv.Health)
		}
	}
	if iv, _ := v.Find(a); !iv.Selected || iv.Class != ClassSupported || iv.Health != HealthOK {
		t.Errorf("a: selected=%v class=%v health=%v", iv.Selected, iv.Class, iv.Health)
	}
	// Capabilities were negotiated once, not on every poll.
	if n := g.exec.count(a, "capabilities"); n != 1 {
		t.Errorf("capabilities ran %d times for a; want once", n)
	}
}

func TestSelectionRunsNothingUntilItSettles(t *testing.T) {
	g := newRig(t, healthy(), nil)
	a, b, c := mkInstance(t, g.root, "a"), mkInstance(t, g.root, "b"), mkInstance(t, g.root, "c")
	g.start()

	// Arrowing down the list: each stop is shorter than the settle delay.
	g.r.Select(a)
	eventually(t, "settle timer 1", func() bool { return g.clk.pending() >= 3 })
	g.clk.Advance(200 * time.Millisecond)
	g.r.Select(b)
	eventually(t, "settle timer 2", func() bool { return g.clk.pending() >= 4 })
	g.clk.Advance(200 * time.Millisecond)
	g.r.Select(c)
	eventually(t, "settle timer 3", func() bool { return g.clk.pending() >= 5 })
	g.clk.Advance(500 * time.Millisecond)

	eventually(t, "c to be read", func() bool { return status(g, c) == 1 })
	never(t, "an instance passed on the way was run", func() bool { return status(g, a) > 0 || status(g, b) > 0 })
}

func TestExplicitRefreshRunsEveryWellFormedInstance(t *testing.T) {
	g := newRig(t, healthy(), nil)
	a, b, c := mkInstance(t, g.root, "a"), mkInstance(t, g.root, "b"), mkInstance(t, g.root, "c")
	bad := filepath.Join(g.root, "bad", ".groundhog") // no groundhog.sh: malformed
	for _, d := range []string{"schedule", "out", "fired"} {
		if err := os.MkdirAll(filepath.Join(bad, d), 0o755); err != nil {
			t.Fatal(err)
		}
	}
	g.start()
	g.r.Refresh()
	eventually(t, "every instance to be read", func() bool {
		v := g.view()
		for _, k := range []string{a, b, c} {
			if iv, ok := v.Find(k); !ok || iv.Inspection.Last == nil {
				return false
			}
		}
		return true
	})
	if got := g.exec.ran(); len(got) != 3 {
		t.Fatalf("refresh ran %q; want a, b and c and not the malformed instance", got)
	}
	iv, ok := g.view().Find(bad)
	if !ok || iv.Class != ClassMalformed {
		t.Fatalf("malformed instance: found=%v class=%v", ok, iv.Class)
	}
}

func TestExplicitRefreshBoundsConcurrency(t *testing.T) {
	var cur, peak atomic.Int32
	fn := func(ctx context.Context, inst, cmd string, n int) (string, error) {
		if cmd == "status" {
			c := cur.Add(1)
			for {
				p := peak.Load()
				if c <= p || peak.CompareAndSwap(p, c) {
					break
				}
			}
			time.Sleep(15 * time.Millisecond)
			cur.Add(-1)
		}
		return healthy()(ctx, inst, cmd, n)
	}
	g := newRig(t, fn, func(c *Config) { c.Concurrency = 3 })
	for i := 0; i < 12; i++ {
		mkInstance(t, g.root, fmt.Sprintf("i%02d", i))
	}
	g.start()
	g.r.Refresh()
	eventually(t, "all twelve to be read", func() bool {
		n := 0
		for _, iv := range g.view().Instances {
			if iv.Inspection.Last != nil {
				n++
			}
		}
		return n == 12
	})
	if p := peak.Load(); p > 3 || p < 2 {
		t.Fatalf("peak concurrent status calls %d; want 2 or 3 (limit 3)", p)
	}
}

func TestOneHungInstanceDoesNotStallTheOthers(t *testing.T) {
	var hung string
	fn := func(ctx context.Context, inst, cmd string, n int) (string, error) {
		if inst == hung && cmd == "status" {
			<-ctx.Done()
			return "", ctx.Err()
		}
		return healthy()(ctx, inst, cmd, n)
	}
	g := newRig(t, fn, func(c *Config) { c.CallTimeout = 300 * time.Millisecond })
	hung = mkInstance(t, g.root, "a-hung")
	b, c := mkInstance(t, g.root, "b"), mkInstance(t, g.root, "c")
	g.start()
	g.r.Refresh()

	eventually(t, "the others to be read while a hangs", func() bool {
		v := g.view()
		ib, _ := v.Find(b)
		ic, _ := v.Find(c)
		return ib.Inspection.Last != nil && ic.Inspection.Last != nil
	})
	if iv, _ := g.view().Find(hung); iv.Inspection.Err != nil || iv.Inspection.Last != nil {
		t.Fatalf("the hung instance should still be waiting, got %+v", iv.Inspection)
	}

	eventually(t, "the hung call to time out", func() bool {
		iv, _ := g.view().Find(hung)
		return iv.Failing()
	})
	iv, _ := g.view().Find(hung)
	if !errors.Is(iv.Inspection.Err, context.DeadlineExceeded) {
		t.Fatalf("hung instance error = %v; want a deadline", iv.Inspection.Err)
	}
	if iv.Class != ClassFailing {
		t.Errorf("class %v; want failing (nothing was ever read)", iv.Class)
	}
	for _, k := range []string{b, c} {
		if iv, _ := g.view().Find(k); iv.Health != HealthOK {
			t.Errorf("%s: health %v", k, iv.Health)
		}
	}
}

func TestFailureKeepsTheLastGoodSnapshotAndBacksOff(t *testing.T) {
	fn := func(ctx context.Context, inst, cmd string, n int) (string, error) {
		if cmd == "status" && (n == 2 || n == 3) {
			return "", exitStatus(5)
		}
		return healthy()(ctx, inst, cmd, n)
	}
	g := newRig(t, fn, nil)
	a := mkInstance(t, g.root, "a")
	g.start()
	g.r.Select(a)
	eventually(t, "settle timer", func() bool { return g.clk.pending() >= 3 })
	g.clk.Advance(g.r.Config().SelectSettle)
	eventually(t, "first read", func() bool { iv, _ := g.view().Find(a); return readOK(iv) })
	good, _ := g.view().Find(a)
	firstAt := good.Inspection.Last.TakenAt

	g.tick() // the read that fails
	eventually(t, "the failure", func() bool { iv, _ := g.view().Find(a); return iv.Failing() })
	iv, _ := g.view().Find(a)
	var ee *groundhog.ExitError
	if !errors.As(iv.Inspection.Err, &ee) || ee.Code != 5 {
		t.Fatalf("error = %v; want exit 5", iv.Inspection.Err)
	}
	if iv.Inspection.Last == nil || !iv.Inspection.Last.TakenAt.Equal(firstAt) {
		t.Fatalf("the last good snapshot was not kept: %+v", iv.Inspection)
	}
	if iv.Class != ClassSupported {
		t.Errorf("class %v; a failure should not erase what the instance is", iv.Class)
	}
	if !iv.Inspection.ErrAt.After(firstAt) {
		t.Errorf("ErrAt %v should come after the good read at %v", iv.Inspection.ErrAt, firstAt)
	}

	// One failure doubles the wait: the next interval passes without a read.
	g.tick()
	never(t, "a read inside the backoff", func() bool { return status(g, a) > 2 })
	g.tick()
	eventually(t, "the second failing read", func() bool { return status(g, a) == 3 })
	// Two failures: the wait is four intervals from the second failure.
	for i := 0; i < 3; i++ {
		g.tick()
	}
	never(t, "a read inside the longer backoff", func() bool { return status(g, a) > 3 })
	g.tick()
	eventually(t, "recovery", func() bool { iv, _ := g.view().Find(a); return readOK(iv) })
	iv, _ = g.view().Find(a)
	if iv.Inspection.Err != nil || !iv.Inspection.Last.TakenAt.After(firstAt) {
		t.Fatalf("recovery should clear the error and replace the snapshot: %+v", iv.Inspection)
	}
}

func TestCancellationStopsCallsInFlight(t *testing.T) {
	started := make(chan string, 8)
	cancelled := make(chan string, 8)
	fn := func(ctx context.Context, inst, cmd string, n int) (string, error) {
		if cmd == "status" {
			started <- inst
			<-ctx.Done()
			cancelled <- inst
			return "", ctx.Err()
		}
		return healthy()(ctx, inst, cmd, n)
	}
	g := newRig(t, fn, func(c *Config) { c.CallTimeout = time.Minute })
	a, b := mkInstance(t, g.root, "a"), mkInstance(t, g.root, "b")
	g.start()
	g.r.Refresh()
	for i := 0; i < 2; i++ {
		select {
		case <-started:
		case <-time.After(5 * time.Second):
			t.Fatal("calls did not start")
		}
	}
	start := time.Now()
	g.stop() // cancels and waits for Run
	if d := time.Since(start); d > 2*time.Second {
		t.Fatalf("Run took %v to stop", d)
	}
	for i := 0; i < 2; i++ {
		select {
		case <-cancelled:
		default:
			t.Fatal("a call in flight was not cancelled")
		}
	}
	for _, k := range []string{a, b} {
		if iv, _ := g.view().Find(k); iv.Inspection.Err != nil {
			t.Errorf("%s: cancellation recorded as a failure: %v", k, iv.Inspection.Err)
		}
	}
}

func TestSelectionSurvivesAdditionsRemovalsAndReordering(t *testing.T) {
	g := newRig(t, healthy(), nil)
	mkInstance(t, g.root, "m")
	b := mkInstance(t, g.root, "m2")
	g.start()
	g.r.Select(b)
	eventually(t, "settle timer", func() bool { return g.clk.pending() >= 3 })
	g.clk.Advance(g.r.Config().SelectSettle)
	eventually(t, "b read", func() bool { iv, _ := g.view().Find(b); return iv.Inspection.Last != nil })
	before, _ := g.view().Find(b)

	// A new instance sorts before it, another after; b's position moves.
	first := mkInstance(t, g.root, "0-new")
	mkInstance(t, g.root, "z-new")
	if err := g.r.Rescan(context.Background()); err != nil {
		t.Fatal(err)
	}
	v := g.view()
	if v.Selected != b || len(v.Instances) != 4 || v.Instances[0].Instance.Path != first {
		t.Fatalf("after additions: selected %q, instances %d", v.Selected, len(v.Instances))
	}
	after, _ := v.Find(b)
	if !after.Selected || after.Inspection.Last != before.Inspection.Last {
		t.Fatal("the selected instance lost its mark or its cache when others were added")
	}
	for _, iv := range v.Instances {
		if iv.Instance.Path != b && (iv.Selected || iv.Inspection.Last != nil) {
			t.Errorf("%s was marked or inspected", iv.Instance.Path)
		}
	}

	// The selected instance disappears: selection keeps its key, nothing is run
	// in its place, and it is selected again when it returns.
	calls := g.exec.total()
	if err := os.RemoveAll(filepath.Dir(b)); err != nil {
		t.Fatal(err)
	}
	if err := g.r.Rescan(context.Background()); err != nil {
		t.Fatal(err)
	}
	v = g.view()
	if _, ok := v.Find(b); ok || v.Selected != b || len(v.Instances) != 3 {
		t.Fatalf("after removal: found=%v selected=%q n=%d", ok, v.Selected, len(v.Instances))
	}
	g.tick()
	never(t, "a script ran after the selected instance vanished", func() bool { return g.exec.total() > calls })
	mkInstance(t, g.root, "m2")
	if err := g.r.Rescan(context.Background()); err != nil {
		t.Fatal(err)
	}
	if iv, ok := g.view().Find(b); !ok || !iv.Selected || iv.Class != ClassNotInspected {
		t.Fatalf("returned instance: found=%v selected=%v class=%v", ok, iv.Selected, iv.Class)
	}
}

// What status shows can move while the listings hold still (a run ends; the
// pause is set by another tool; eligibility follows the clock). The view must
// follow the newest read, and a poll must still skip the capabilities call.
// Each step waits on what the view holds, never on a signal: the poll that
// starts a read also signals the selected instance's age at once, before that
// read is recorded.
func TestChangedReadsReachTheView(t *testing.T) {
	var eligible, paused atomic.Bool
	fn := func(ctx context.Context, inst, cmd string, n int) (string, error) {
		if cmd == "status" {
			return statusJSON(inst, statusOpts{itemPath: "schedule/daily/x", eligible: eligible.Load(), paused: paused.Load()}), nil
		}
		return healthy()(ctx, inst, cmd, n)
	}
	g := newRig(t, fn, nil)
	a := mkInstance(t, g.root, "a")
	g.start()
	g.r.Select(a)
	eventually(t, "settle timer", func() bool { return g.clk.pending() >= 3 })
	g.clk.Advance(g.r.Config().SelectSettle)
	eventually(t, "the first read", func() bool {
		iv, _ := g.view().Find(a)
		return iv.Inspection.Last != nil && !iv.Inspection.Last.Status.Paused
	})

	// Only the pause moved.
	paused.Store(true)
	g.tick()
	eventually(t, "the view to show the pause", func() bool {
		iv, _ := g.view().Find(a)
		return iv.Inspection.Last.Status.Paused
	})

	// Only clock-dependent eligibility moved.
	eligible.Store(true)
	g.tick()
	eventually(t, "the view to show the item eligible", func() bool {
		iv, _ := g.view().Find(a)
		return iv.Inspection.Last.Status.Schedule[0].EligibleNow
	})

	if n := g.exec.count(a, "capabilities"); n != 1 {
		t.Fatalf("capabilities ran %d times; unchanged polls should not renegotiate", n)
	}
}

// A completed read is announced whether or not anyone is watching its
// instance: the read of an unselected instance that matches the last one in
// everything still signals. The signal is sent in the same step as the store,
// so once the view holds the new read the signal is already pending.
func TestEveryCompletedReadIsAnnounced(t *testing.T) {
	hold := make(chan struct{})
	var reads atomic.Int64
	fn := func(ctx context.Context, inst, cmd string, n int) (string, error) {
		if cmd == "status" && strings.Contains(inst, "/a/") && reads.Add(1) == 2 {
			select {
			case <-hold:
			case <-ctx.Done():
			}
		}
		return healthy()(ctx, inst, cmd, n)
	}
	g := newRig(t, fn, nil)
	a := mkInstance(t, g.root, "a")
	b := mkInstance(t, g.root, "b")
	g.start()
	g.r.Select(a)
	eventually(t, "settle timer", func() bool { return g.clk.pending() >= 3 })
	g.settle()
	eventually(t, "first read", func() bool { return status(g, a) == 1 && g.view().Instances[0].Inspection.Last != nil })
	first, _ := g.view().Find(a)

	g.tick()
	eventually(t, "second read to start", func() bool { return reads.Load() == 2 })
	g.r.Select(b) // a is no longer selected; no Refresh asked for the read
	drained(g.r)
	close(hold)
	eventually(t, "second read to be stored", func() bool {
		iv, _ := g.view().Find(a)
		return iv.Inspection.Last != first.Inspection.Last
	})
	if !drained(g.r) {
		t.Fatal("an identical read of an unselected instance was stored without an announcement")
	}
}

// With nothing read, a selected instance's staleness still reaches the view:
// the refresher announces once per interval so the clock-dependent health is
// recomputed.
func TestSelectedAgeIsAnnouncedEachInterval(t *testing.T) {
	var fail atomic.Bool
	fn := func(ctx context.Context, inst, cmd string, n int) (string, error) {
		if cmd == "status" && fail.Load() {
			<-ctx.Done() // a status read that never returns
			return "", ctx.Err()
		}
		return healthy()(ctx, inst, cmd, n)
	}
	g := newRig(t, fn, nil)
	a := mkInstance(t, g.root, "a")
	g.start()
	g.r.Select(a)
	eventually(t, "settle timer", func() bool { return g.clk.pending() >= 3 })
	g.settle()
	eventually(t, "first read", func() bool { return status(g, a) == 1 && g.view().Instances[0].Inspection.Last != nil })
	drained(g.r)
	fail.Store(true)
	for i := 0; i < staleAfterIntervals+1; i++ {
		g.tick()
		eventually(t, "an announcement for the interval", func() bool { return drained(g.r) })
	}
	if iv, _ := g.view().Find(a); iv.Health != HealthStale {
		t.Fatalf("health %v after %d intervals with no new read; want stale", iv.Health, staleAfterIntervals+1)
	}
}

func TestChangedListingsReachTheView(t *testing.T) {
	fn := func(ctx context.Context, inst, cmd string, n int) (string, error) {
		if cmd == "status" {
			return statusJSON(inst, statusOpts{}), nil
		}
		return healthy()(ctx, inst, cmd, n)
	}
	g := newRig(t, fn, nil)
	a := mkInstance(t, g.root, "a")
	g.start()
	g.r.Select(a)
	eventually(t, "settle timer", func() bool { return g.clk.pending() >= 3 })
	g.clk.Advance(g.r.Config().SelectSettle)
	eventually(t, "first read", func() bool { return g.view().Instances[0].Inspection.Last != nil })
	// A schedule item appears by hand, which no status field covers.
	if err := os.MkdirAll(filepath.Join(a, "schedule", "daily", "new"), 0o755); err != nil {
		t.Fatal(err)
	}
	g.tick()
	eventually(t, "the view to list the new item", func() bool {
		items := g.view().Instances[0].Files.Items()
		return len(items) == 1 && items[0].Path == "daily/new"
	})
}

func TestClassesAreDistinguished(t *testing.T) {
	fn := func(ctx context.Context, inst, cmd string, n int) (string, error) {
		name := filepath.Base(filepath.Dir(inst))
		switch {
		case name == "legacy" && cmd == "capabilities":
			return "", exitStatus(1)
		case name == "older" && cmd == "capabilities":
			return `{"protocol":"groundhog-runtime","protocol_version":1,"capabilities":["status-json-v1","ownership-v1"]}`, nil
		case name == "partial" && cmd == "capabilities":
			return capsJSON("status-json-v2", "pause-v2"), nil
		case name == "broken" && cmd == "status":
			return "", exitStatus(6)
		}
		return healthy()(ctx, inst, cmd, n)
	}
	g := newRig(t, fn, nil)
	for _, n := range []string{"legacy", "older", "partial", "broken", "plain"} {
		mkInstance(t, g.root, n)
	}
	g.start()
	g.r.Refresh()
	eventually(t, "every instance to settle", func() bool {
		for _, iv := range g.view().Instances {
			if iv.Class == ClassNotInspected {
				return false
			}
		}
		return len(g.view().Instances) == 5
	})
	want := map[string]Class{"legacy": ClassLegacy, "older": ClassLegacy, "partial": ClassUnsupported, "broken": ClassFailing,
		"plain": ClassSupported}
	for name, class := range want {
		if iv := g.inst(g.view(), name); iv.Class != class {
			t.Errorf("%s: class %v, want %v", name, iv.Class, class)
		}
	}
	for _, name := range []string{"legacy", "older"} {
		if iv := g.inst(g.view(), name); status(g, iv.Instance.Path) != 0 {
			t.Errorf("%s: a script that is not protocol 2 was asked for status", name)
		}
	}
	if r := g.inst(g.view(), "older").Inspection.Support.Reason; !strings.Contains(r, "older Groundhog") {
		t.Errorf("older: reason %q", r)
	}
	if !g.inst(g.view(), "broken").Failing() {
		t.Error("broken should be failing")
	}
	// Distinct strings, so the terminal can say which.
	seen := map[string]bool{}
	for _, c := range want {
		seen[c.String()] = true
	}
	if len(seen) != len(want)-1 { // legacy twice
		t.Errorf("class names collide: %v", seen)
	}
}

func TestAChangedScriptIsNegotiatedAgain(t *testing.T) {
	g := newRig(t, healthy(), nil)
	a := mkInstance(t, g.root, "a")
	g.start()
	g.r.Select(a)
	eventually(t, "settle timer", func() bool { return g.clk.pending() >= 3 })
	g.clk.Advance(g.r.Config().SelectSettle)
	eventually(t, "first read", func() bool { return status(g, a) == 1 })
	g.tick()
	eventually(t, "second read", func() bool { return status(g, a) == 2 })
	if n := g.exec.count(a, "capabilities"); n != 1 {
		t.Fatalf("capabilities ran %d times before the script changed", n)
	}
	if err := os.WriteFile(filepath.Join(a, "groundhog.sh"), []byte("#!/bin/sh\n# a newer copy\nexit 0\n"), 0o755); err != nil {
		t.Fatal(err)
	}
	g.tick()
	eventually(t, "renegotiation after the script changed", func() bool { return g.exec.count(a, "capabilities") == 2 })
}

func TestPeriodicRescanFindsAdditionsWithoutRunningThem(t *testing.T) {
	g := newRig(t, healthy(), func(c *Config) { c.DiscoverEvery = time.Minute })
	mkInstance(t, g.root, "a")
	g.start()
	added := mkInstance(t, g.root, "b")
	drained(g.r)
	g.clk.Advance(time.Minute)
	eventually(t, "the addition to appear", func() bool { _, ok := g.view().Find(added); return ok })
	if !drained(g.r) {
		t.Error("an addition should be announced")
	}
	if g.exec.total() != 0 {
		t.Errorf("rescanning ran %d script calls", g.exec.total())
	}
}

func TestFailedRescanKeepsThePreviousListAndSaysSo(t *testing.T) {
	g := newRig(t, healthy(), nil)
	a := mkInstance(t, g.root, "a")
	g.start()
	if err := os.RemoveAll(g.root); err != nil {
		t.Fatal(err)
	}
	if err := g.r.Rescan(context.Background()); err == nil {
		t.Fatal("a vanished root should fail the rescan")
	}
	v := g.view()
	if _, ok := v.Find(a); !ok || v.DiscoverErr == nil {
		t.Fatalf("found=%v err=%v; want the old list and the error", ok, v.DiscoverErr)
	}
	mkInstance(t, filepath.Dir(g.root), filepath.Base(g.root)+"/a")
	if err := g.r.Rescan(context.Background()); err != nil {
		t.Fatal(err)
	}
	if g.view().DiscoverErr != nil {
		t.Error("the error should clear after a good rescan")
	}
}

func TestExplicitRefreshDuringAReadRerunsOnce(t *testing.T) {
	var mu sync.Mutex
	release := make(chan struct{})
	fn := func(ctx context.Context, inst, cmd string, n int) (string, error) {
		if cmd == "status" && n == 1 {
			mu.Lock()
			ch := release
			mu.Unlock()
			select {
			case <-ch:
			case <-ctx.Done():
				return "", ctx.Err()
			}
		}
		return healthy()(ctx, inst, cmd, n)
	}
	g := newRig(t, fn, nil)
	a := mkInstance(t, g.root, "a")
	g.start()
	g.r.Select(a)
	eventually(t, "settle timer", func() bool { return g.clk.pending() >= 3 })
	g.clk.Advance(g.r.Config().SelectSettle)
	eventually(t, "the first read to block", func() bool { return status(g, a) == 1 })
	g.r.Refresh()
	g.r.Refresh()
	time.Sleep(30 * time.Millisecond)
	close(release)
	eventually(t, "the rerun", func() bool { return status(g, a) == 2 })
	never(t, "more than one rerun", func() bool { return status(g, a) > 2 })
}

// After an action the terminal asks for one instance to be read at once. It
// reaches that instance's script only, not the others, and not on any timer.
func TestReinspectReadsOnlyThatInstanceNow(t *testing.T) {
	g := newRig(t, healthy(), nil)
	a, b := mkInstance(t, g.root, "a"), mkInstance(t, g.root, "b")
	g.start()

	g.r.Reinspect(b)
	eventually(t, "b read without waiting for the interval", func() bool { return status(g, b) == 1 })
	never(t, "any read of a", func() bool { return status(g, a) != 0 })
	if iv, _ := g.view().Find(b); iv.Class != ClassSupported || iv.Inspection.Last == nil {
		t.Errorf("b after Reinspect: class %v last %v", iv.Class, iv.Inspection.Last)
	}

	// Unknown and malformed keys do nothing.
	before := g.exec.total()
	g.r.Reinspect(g.root + "/nowhere/.groundhog")
	g.r.Reinspect("")
	never(t, "a read for an unknown key", func() bool { return g.exec.total() != before })
}

// A Reinspect that lands while a read is in flight queues one more behind it:
// the read in flight may have started before the action.
func TestReinspectDuringAReadRerunsOnce(t *testing.T) {
	var mu sync.Mutex
	release := make(chan struct{})
	fn := func(ctx context.Context, inst, cmd string, n int) (string, error) {
		if cmd == "status" && n == 1 {
			mu.Lock()
			ch := release
			mu.Unlock()
			select {
			case <-ch:
			case <-ctx.Done():
				return "", ctx.Err()
			}
		}
		return healthy()(ctx, inst, cmd, n)
	}
	g := newRig(t, fn, nil)
	a := mkInstance(t, g.root, "a")
	g.start()
	g.r.Reinspect(a)
	eventually(t, "the first read to block", func() bool { return status(g, a) == 1 })
	g.r.Reinspect(a)
	g.r.Reinspect(a)
	time.Sleep(30 * time.Millisecond)
	close(release)
	eventually(t, "the rerun", func() bool { return status(g, a) == 2 })
	never(t, "more than one rerun", func() bool { return status(g, a) > 2 })
}

// An instance read long ago is not "stale" the moment it is selected: the read
// the selection starts has not had its turn yet. Found by the MVP acceptance
// session, where selecting such an instance briefly read "(stale)" and offered
// neither Run nor Pause. It is stale once it has been selected for more than
// three intervals without a read arriving.
func TestSelectingAnOldReadIsNotStaleUntilItHasHadItsTurn(t *testing.T) {
	g := newRig(t, func(ctx context.Context, inst, cmd string, n int) (string, error) {
		if cmd == "status" && filepath.Base(filepath.Dir(inst)) == "a" && n >= 2 {
			<-ctx.Done() // a's later reads never answer
			return "", ctx.Err()
		}
		return healthy()(ctx, inst, cmd, n)
	}, nil)
	a, b := mkInstance(t, g.root, "a"), mkInstance(t, g.root, "b")
	g.start()

	g.r.Select(a)
	eventually(t, "the settle timer", func() bool { return g.clk.pending() >= 3 })
	g.settle()
	eventually(t, "the first read of a", func() bool { return status(g, a) == 1 })
	g.r.Select(b)
	eventually(t, "the settle timer", func() bool { return g.clk.pending() >= 3 })
	g.settle()
	eventually(t, "the first read of b", func() bool { return status(g, b) >= 1 })

	g.clk.Advance(time.Minute) // a's read is now far older than three intervals
	g.armed()
	if iv, _ := g.view().Find(a); iv.Health != HealthOK || iv.Selected {
		t.Fatalf("a, unselected: health %v selected %v", iv.Health, iv.Selected)
	}

	g.r.Select(a)
	if iv, _ := g.view().Find(a); !iv.Selected || iv.Health != HealthOK {
		t.Errorf("a, just selected after an old read: health %v, want current", iv.Health)
	}

	// Its read starts and never answers; after more than three intervals
	// selected, the old read does read as stale.
	eventually(t, "the settle timer", func() bool { return g.clk.pending() >= 3 })
	g.settle()
	eventually(t, "a's second read to start", func() bool { return status(g, a) == 2 })
	for i := 0; i < 4; i++ {
		g.tick()
	}
	if iv, _ := g.view().Find(a); iv.Health != HealthStale {
		t.Errorf("a, selected for four intervals with no read: health %v, want stale", iv.Health)
	}
}

// readOK says a status is shown, the newest read succeeded and it is current.
// Health alone cannot say so: it holds HealthOK when nothing was read, and
// keeps judging the last status while the newest read fails.
func readOK(iv InstanceView) bool {
	return iv.Inspection.Last != nil && !iv.Failing() && iv.Health == HealthOK
}

// argsExec records each call's arguments and passes it on.
type argsExec struct {
	inner runner.Executor
	mu    sync.Mutex
	argv  []string
}

func (a *argsExec) Run(ctx context.Context, inv runner.Invocation) error {
	a.mu.Lock()
	a.argv = append(a.argv, strings.Join(inv.Args, " "))
	a.mu.Unlock()
	return a.inner.Run(ctx, inv)
}

func (a *argsExec) statusArgs() []string {
	a.mu.Lock()
	defer a.mu.Unlock()
	var out []string
	for _, s := range a.argv {
		if strings.HasPrefix(s, "status") {
			out = append(out, s)
		}
	}
	return out
}

// The refresher reads status through the shared client, which passes
// --next-due to a script that offers next-due-v1 and nothing to one that does
// not: one place decides, and the refresher does not.
func TestTheRefresherPassesNextDueOnlyToAScriptThatOffersIt(t *testing.T) {
	for _, tc := range []struct {
		name string
		caps []string
		want string
	}{
		{"offered", controlCaps(), "status --json --next-due"},
		{"not offered", []string{"status-json-v2", "pause-v2", "handlers-v1"}, "status --json"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			fn := func(_ context.Context, inst, cmd string, _ int) (string, error) {
				switch cmd {
				case "capabilities":
					return capsJSON(tc.caps...), nil
				case "status":
					return statusJSON(inst, statusOpts{}), nil
				}
				return "", exitStatus(1)
			}
			rec := &argsExec{inner: newExec(fn)}
			g := newRig(t, fn, func(c *Config) { c.Exec = rec })
			a := mkInstance(t, g.root, "a")
			g.start()
			g.r.Select(a)
			eventually(t, "the settle timer", func() bool { return g.clk.pending() >= 3 })
			g.settle()
			eventually(t, "the first read", func() bool { return len(rec.statusArgs()) >= 1 })
			for _, got := range rec.statusArgs() {
				if got != tc.want {
					t.Errorf("status argv %q, want %q", got, tc.want)
				}
			}
		})
	}
}
