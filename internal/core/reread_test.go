package core

import (
	"context"
	"sync"
	"testing"
	"time"
)

func withReread(c *Config) { c.RereadEvery = time.Minute }

// read waits until inst has had n status calls and none is still going: a
// read counted but in flight would otherwise race the next Advance.
func read(t *testing.T, g *rig, inst string, n int) {
	t.Helper()
	eventually(t, "a finished read", func() bool {
		if status(g, inst) < n {
			return false
		}
		g.r.mu.Lock()
		defer g.r.mu.Unlock()
		e := g.r.entries[inst]
		return e != nil && !e.busy && e.insp.Last != nil
	})
}

// step moves the fake clock on one poll interval at a time, for d, letting
// each poll's reads start.
func step(g *rig, d time.Duration) {
	for gone := time.Duration(0); gone < d; gone += g.r.Config().Interval {
		g.clk.Advance(g.r.Config().Interval)
		time.Sleep(2 * time.Millisecond)
	}
}

// Only instances the reader has had inspected this session are re-read.
// Nothing chosen, nothing runs, however long the session stays open.
func TestRereadRunsNothingTheReaderHasNotChosen(t *testing.T) {
	g := newRig(t, healthy(), withReread)
	mkInstance(t, g.root, "a")
	mkInstance(t, g.root, "b")
	g.start()
	step(g, 5*time.Minute)
	if n := g.exec.total(); n != 0 {
		t.Fatalf("five minutes with nothing chosen ran %d script calls (%v)", n, g.exec.ran())
	}
}

// An instance the reader chose and then left is read again once its last
// read is a minute old, status only; one never chosen stays unread.
func TestRereadReadsAChosenInstanceAfterTheReaderMovesOn(t *testing.T) {
	g := newRig(t, healthy(), withReread)
	a, b, c := mkInstance(t, g.root, "a"), mkInstance(t, g.root, "b"), mkInstance(t, g.root, "c")
	g.start()
	g.r.Select(a)
	eventually(t, "the settle timer", func() bool { return g.clk.pending() >= 3 })
	g.settle()
	read(t, g, a, 1)
	g.r.Select(b)
	eventually(t, "the settle timer for b", func() bool { return g.clk.pending() >= 3 })
	g.settle()
	read(t, g, b, 1)

	step(g, 50*time.Second)
	if n := status(g, a); n != 1 {
		t.Fatalf("a was read again %d times within a minute", n-1)
	}
	step(g, 15*time.Second)
	read(t, g, a, 2)
	step(g, time.Minute+5*time.Second)
	read(t, g, a, 3)
	if n := g.exec.count(a, "capabilities"); n != 1 {
		t.Errorf("capabilities ran %d times for a; want once", n)
	}
	if n := g.exec.count(c, "status") + g.exec.count(c, "capabilities"); n != 0 {
		t.Errorf("c was never chosen but ran %d calls", n)
	}
	if iv, _ := g.view().Find(a); iv.Class != ClassSupported || iv.Selected {
		t.Errorf("a: class %v selected %v", iv.Class, iv.Selected)
	}
}

// A Refresh reaches every instance, so each counts as inspected and is
// re-read from then on, one at a time rather than all together.
func TestRereadReadsWhatARefreshReachedOneAtATime(t *testing.T) {
	var mu sync.Mutex
	inflight, most := 0, 0
	slow := func(ctx context.Context, inst, cmd string, n int) (string, error) {
		if cmd == "status" {
			mu.Lock()
			inflight++
			most = max(most, inflight)
			mu.Unlock()
			time.Sleep(5 * time.Millisecond)
			mu.Lock()
			inflight--
			mu.Unlock()
		}
		return healthy()(ctx, inst, cmd, n)
	}
	g := newRig(t, slow, withReread)
	insts := []string{mkInstance(t, g.root, "a"), mkInstance(t, g.root, "b"), mkInstance(t, g.root, "c")}
	g.start()
	g.r.Refresh()
	for _, in := range insts {
		read(t, g, in, 1)
	}
	mu.Lock()
	most = 0
	mu.Unlock()
	step(g, 2*time.Minute)
	for _, in := range insts {
		eventually(t, "each read again", func() bool { return status(g, in) >= 2 })
	}
	mu.Lock()
	defer mu.Unlock()
	if most != 1 {
		t.Errorf("%d re-reads ran at once; want one at a time", most)
	}
}

// What has nothing to re-read is left alone: a legacy instance (only
// capabilities ever runs) and one whose capabilities never answered (a status
// read would first have to negotiate).
func TestRereadSkipsLegacyAndUnansweredInstances(t *testing.T) {
	var legacy, broken string
	fn := func(ctx context.Context, inst, cmd string, n int) (string, error) {
		switch {
		case inst == legacy && cmd == "capabilities":
			return capsJSON("pause-v2"), nil
		case inst == broken && cmd == "capabilities":
			return "", exitStatus(2)
		}
		return healthy()(ctx, inst, cmd, n)
	}
	g := newRig(t, fn, withReread)
	legacy, broken = mkInstance(t, g.root, "legacy"), mkInstance(t, g.root, "broken")
	g.start()
	g.r.Refresh()
	eventually(t, "the refresh", func() bool {
		return g.exec.count(legacy, "capabilities") == 1 && g.exec.count(broken, "capabilities") == 1
	})
	time.Sleep(10 * time.Millisecond)
	step(g, 5*time.Minute)
	for _, in := range []string{legacy, broken} {
		if n := g.exec.count(in, "capabilities") + g.exec.count(in, "status"); n != 1 {
			t.Errorf("%s ran %d calls; want only the refresh's one", in, n)
		}
	}
}

// Off means off: a negative period reads nothing in the background.
func TestRereadOff(t *testing.T) {
	g := newRig(t, healthy(), func(c *Config) { c.RereadEvery = -1 })
	a := mkInstance(t, g.root, "a")
	g.start()
	g.r.Refresh()
	read(t, g, a, 1)
	step(g, 3*time.Minute)
	if n := status(g, a); n != 1 {
		t.Errorf("status ran %d times with the re-read off", n)
	}
}
