package core

import (
	"context"
	"errors"
	"slices"
	"sync/atomic"
	"testing"
	"time"
)

// A ticker over a fake executor and the fake clock. Nothing here starts a
// process.

type trig struct {
	t    *testing.T
	exec *fakeExec
	clk  *fakeClock
	tk   *Ticker
}

func newTRig(t *testing.T, fn script) *trig {
	t.Helper()
	if fn == nil {
		fn = tickable(nil)
	}
	r := &trig{t: t, exec: newExec(fn), clk: newClock()}
	tk, err := NewTicker(TickerConfig{Clients: NewClients(r.exec), Clock: r.clk, Interval: time.Minute})
	if err != nil {
		t.Fatal(err)
	}
	r.tk = tk
	t.Cleanup(func() { tk.Quit(context.Background()) })
	return r
}

// tickable answers capabilities, status (with o) and every command that
// changes an instance with exit 0.
func tickable(o *statusOpts) script {
	return func(_ context.Context, inst, cmd string, _ int) (string, error) {
		switch cmd {
		case "capabilities":
			return capsJSON(), nil
		case "status":
			opts := statusOpts{}
			if o != nil {
				opts = *o
			}
			return statusJSON(inst, opts), nil
		}
		return "", nil
	}
}

func (r *trig) run(key string) RunResult {
	r.t.Helper()
	res, err := r.tk.Run(context.Background(), key)
	if err != nil {
		r.t.Fatal(err)
	}
	return res
}

const key = "/synthetic/proj/.groundhog"

func TestRunTicksNowThenEveryInterval(t *testing.T) {
	r := newTRig(t, nil)
	res := r.run(key)
	if !res.Ticked || res.TickErr != nil || res.Already {
		t.Fatalf("%+v", res)
	}
	if n := r.exec.count(key, "tick"); n != 1 {
		t.Fatalf("%d ticks after Run", n)
	}
	info, ok := r.tk.Info(key)
	if !ok || !info.Ticking || info.Interval != time.Minute || info.LastBegan.IsZero() || info.LastErr != nil {
		t.Fatalf("info %+v %v", info, ok)
	}
	for want := 2; want <= 4; want++ {
		eventually(t, "the loop to wait for the next tick", func() bool { return r.clk.pending() == 1 })
		r.clk.Advance(time.Minute)
		eventually(t, "the next tick", func() bool { return r.exec.count(key, "tick") == want })
	}
	if got := r.exec.args("tick"); !slices.Equal(got, []string{"tick", "tick", "tick", "tick"}) {
		t.Errorf("tick argv %q", got)
	}
}

func TestRunTwiceKeepsOneLoop(t *testing.T) {
	r := newTRig(t, nil)
	r.run(key)
	if res := r.run(key); !res.Already || res.Ticked {
		t.Fatalf("second Run: %+v", res)
	}
	never(t, "a second tick without the clock moving", func() bool { return r.exec.count(key, "tick") > 1 })
}

func TestRunRefusesAScriptThatCannotBeTickedAndTicksNothing(t *testing.T) {
	for name, caps := range map[string]string{
		"protocol 1":       `{"protocol":"groundhog-runtime","protocol_version":1,"capabilities":["status-json-v1","ownership-v1"]}`,
		"no lock helper":   capsJSON("status-json-v2", "pause-v2", "next-due-v1"),
		"no runtime calls": "",
	} {
		t.Run(name, func(t *testing.T) {
			r := newTRig(t, func(_ context.Context, inst, cmd string, _ int) (string, error) {
				if cmd == "capabilities" && caps != "" {
					return caps, nil
				}
				return "", exitStatus(1)
			})
			_, err := r.tk.Run(context.Background(), key)
			var re *RefusedError
			if !errors.As(err, &re) || re.Reason != RefusedUnsupported || !errors.Is(err, ErrRefused) {
				t.Fatalf("%v", err)
			}
			if r.exec.count(key, "tick") != 0 {
				t.Error("an unsupported script was ticked")
			}
			if _, ok := r.tk.Info(key); ok {
				t.Error("a refused Run left a loop")
			}
		})
	}
}

func TestAFailingTickIsReportedAndTickingGoesOn(t *testing.T) {
	var fails atomic.Int32
	fails.Store(1)
	r := newTRig(t, func(ctx context.Context, inst, cmd string, n int) (string, error) {
		if cmd == "tick" && fails.Load() == 1 {
			return "", exitStatus(1) // an item could not be fired
		}
		return tickable(nil)(ctx, inst, cmd, n)
	})
	res := r.run(key)
	if !res.Ticked || res.TickErr == nil {
		t.Fatalf("%+v", res)
	}
	if info, _ := r.tk.Info(key); !info.Ticking || info.LastErr == nil {
		t.Fatalf("info %+v", info)
	}
	fails.Store(0)
	eventually(t, "the wait", func() bool { return r.clk.pending() == 1 })
	r.clk.Advance(time.Minute)
	eventually(t, "the next tick to clear the error", func() bool {
		info, _ := r.tk.Info(key)
		return r.exec.count(key, "tick") == 2 && info.LastErr == nil
	})
}

// gatedTicks makes every tick of instance only ("" for all) wait on its gate
// until released (or ctx ends).
type gatedTicks struct {
	only     string
	inFlight atomic.Int32
	gate     chan struct{}
}

func (g *gatedTicks) script(ctx context.Context, inst, cmd string, n int) (string, error) {
	if cmd != "tick" || (g.only != "" && inst != g.only) {
		return tickable(nil)(ctx, inst, cmd, n)
	}
	if g.inFlight.Add(1) > 1 {
		panic("two ticks of one instance overlap")
	}
	defer g.inFlight.Add(-1)
	select {
	case <-g.gate:
	case <-ctx.Done():
		return "", ctx.Err()
	}
	return "", nil
}

func TestStopReturnsAtOnceAndTheTickUnderWayFinishes(t *testing.T) {
	g := &gatedTicks{gate: make(chan struct{})}
	r := newTRig(t, g.script)
	runs := make(chan error, 2)
	run := func() { _, err := r.tk.Run(context.Background(), key); runs <- err }
	go run() // returns once its first tick has finished
	eventually(t, "the first tick to start", func() bool { return g.inFlight.Load() == 1 })
	if res := r.tk.Stop(key); !res.WasTicking {
		t.Fatalf("%+v", res)
	}
	if info, _ := r.tk.Info(key); info.Ticking {
		t.Error("still ticking after Stop")
	}
	// Run again while the old tick is under way: the new loop waits for it, so
	// the ticks never overlap (gatedTicks panics if they do).
	go run()
	never(t, "a second tick began beside the first", func() bool { return r.exec.count(key, "tick") > 1 })
	close(g.gate)
	for range 2 {
		if err := <-runs; err != nil {
			t.Fatal(err)
		}
	}
	if n := r.exec.count(key, "tick"); n != 2 {
		t.Fatalf("%d ticks, want the old one and the new loop's first", n)
	}
	if res := r.tk.Stop(key); !res.WasTicking {
		t.Fatal("second Stop found nothing ticking")
	}
	if res := r.tk.Stop(key); res.WasTicking {
		t.Error("Stop of a stopped instance reported ticking")
	}
	r.clk.Advance(time.Hour)
	never(t, "a tick after Stop", func() bool { return r.exec.count(key, "tick") > 2 })
}

func TestQuitStopsEveryLoopWaitsOnlyForTicksUnderWayAndRefusesMore(t *testing.T) {
	other := "/synthetic/other/.groundhog"
	g := &gatedTicks{only: other, gate: make(chan struct{})} // the other instance's first tick hangs
	r := newTRig(t, g.script)
	r.run(key)
	go func() { _, _ = r.tk.Run(context.Background(), other) }()
	eventually(t, "the other tick to start", func() bool { return g.inFlight.Load() == 1 })

	// Hurried: Quit returns when its context ends, the tick still under way.
	hurry, stop := context.WithTimeout(context.Background(), 30*time.Millisecond)
	defer stop()
	if n := r.tk.Quit(hurry); n != 2 {
		t.Fatalf("Quit stopped %d, want 2", n)
	}
	for _, k := range []string{key, other} {
		if info, _ := r.tk.Info(k); info.Ticking {
			t.Errorf("%s still ticking", k)
		}
	}
	close(g.gate)
	if n := r.tk.Quit(context.Background()); n != 0 { // nothing left; waits for the tick to end
		t.Errorf("second Quit stopped %d", n)
	}
	_, err := r.tk.Run(context.Background(), key)
	var re *RefusedError
	if !errors.As(err, &re) || re.Reason != RefusedClosing {
		t.Errorf("Run after Quit: %v", err)
	}
	if err := r.tk.SetPaused(context.Background(), key, true); !errors.As(err, &re) || re.Reason != RefusedClosing {
		t.Errorf("pause after Quit: %v", err)
	}
	r.clk.Advance(time.Hour)
	never(t, "a tick after Quit", func() bool { return r.exec.count(key, "tick") > 1 })
}

func TestRetryStartsOnlyWhatTheFreshReadShowsFailedOrInterrupted(t *testing.T) {
	o := &statusOpts{deliveries: []any{
		fakeDelivery("a-1", "schedule/daily/a", "failed"),
		fakeDelivery("a-2", "schedule/daily/a", "interrupted"),
		fakeDelivery("a-3", "schedule/daily/a", "accepted"),
		fakeDelivery("b-1", "schedule/daily/b", "failed"),
	}}
	r := newTRig(t, tickable(o))
	res, err := r.tk.Retry(context.Background(), key, "schedule/daily/a")
	if err != nil {
		t.Fatal(err)
	}
	if !slices.Equal(res.Started, []string{"out/a-1", "out/a-2"}) || len(res.Busy)+len(res.Gone) != 0 {
		t.Fatalf("%+v", res)
	}
	if got := r.exec.args("retry"); !slices.Equal(got, []string{"retry out/a-1", "retry out/a-2"}) {
		t.Errorf("retry argv %q", got)
	}
	res, err = r.tk.Retry(context.Background(), key, "")
	if err != nil || len(res.Started) != 3 {
		t.Fatalf("instance-wide: %+v %v", res, err)
	}
	if r.exec.count(key, "tick") != 0 {
		t.Error("Retry ticked")
	}
}

func TestRetryReportsBusyNothingAndPausedPlainly(t *testing.T) {
	t.Run("exit 4 and exit 5", func(t *testing.T) {
		o := &statusOpts{deliveries: []any{fakeDelivery("a-1", "schedule/daily/a", "failed"), fakeDelivery("a-2", "schedule/daily/a", "failed")}}
		r := newTRig(t, func(ctx context.Context, inst, cmd string, n int) (string, error) {
			if cmd == "retry" {
				return "", exitStatus(3 + n) // 4 then 5
			}
			return tickable(o)(ctx, inst, cmd, n)
		})
		res, err := r.tk.Retry(context.Background(), key, "")
		if err != nil || !slices.Equal(res.Busy, []string{"out/a-1"}) || !slices.Equal(res.Gone, []string{"out/a-2"}) || len(res.Started) != 0 {
			t.Fatalf("%+v %v", res, err)
		}
	})
	t.Run("running since the palette showed it failed", func(t *testing.T) {
		r := newTRig(t, tickable(&statusOpts{deliveries: []any{fakeDelivery("a-1", "schedule/daily/a", "running")}}))
		res, err := r.tk.Retry(context.Background(), key, "schedule/daily/a")
		if err != nil || !slices.Equal(res.Busy, []string{"out/a-1"}) {
			t.Fatalf("%+v %v", res, err)
		}
		if r.exec.count(key, "retry") != 0 {
			t.Error("retry ran for a delivery the fresh read shows running")
		}
	})
	t.Run("accepted since: nothing to retry", func(t *testing.T) {
		r := newTRig(t, tickable(&statusOpts{deliveries: []any{fakeDelivery("a-1", "schedule/daily/a", "accepted")}}))
		_, err := r.tk.Retry(context.Background(), key, "schedule/daily/a")
		var re *RefusedError
		if !errors.As(err, &re) || re.Reason != RefusedNotOffered || r.exec.count(key, "retry") != 0 {
			t.Fatalf("%v", err)
		}
	})
	t.Run("paused", func(t *testing.T) {
		r := newTRig(t, tickable(&statusOpts{paused: true, deliveries: []any{fakeDelivery("a-1", "schedule/daily/a", "failed")}}))
		_, err := r.tk.Retry(context.Background(), key, "")
		var re *RefusedError
		if !errors.As(err, &re) || re.Reason != RefusedPaused || r.exec.count(key, "retry") != 0 {
			t.Fatalf("%v", err)
		}
	})
	t.Run("a hard error on one delivery does not stop the rest", func(t *testing.T) {
		o := &statusOpts{deliveries: []any{fakeDelivery("a-1", "schedule/daily/a", "failed"),
			fakeDelivery("a-2", "schedule/daily/a", "failed"), fakeDelivery("a-3", "schedule/daily/a", "failed")}}
		r := newTRig(t, func(ctx context.Context, inst, cmd string, n int) (string, error) {
			switch {
			case cmd == "retry" && n == 1:
				return "", exitStatus(3) // paused since the read
			case cmd == "retry" && n == 2:
				return "", exitStatus(6)
			}
			return tickable(o)(ctx, inst, cmd, n)
		})
		res, err := r.tk.Retry(context.Background(), key, "")
		if err != nil || !slices.Equal(res.Started, []string{"out/a-3"}) || len(res.Failed) != 2 ||
			res.Failed[0].Delivery != "out/a-1" || res.Failed[1].Delivery != "out/a-2" {
			t.Fatalf("%+v %v", res, err)
		}
	})
}

func TestSetPausedIsTheTokenlessPauseAndResume(t *testing.T) {
	r := newTRig(t, nil)
	for _, pause := range []bool{true, false, true} {
		if err := r.tk.SetPaused(context.Background(), key, pause); err != nil {
			t.Fatal(err)
		}
	}
	got := append(r.exec.args("pause"), r.exec.args("resume")...)
	if !slices.Equal(got, []string{"pause", "pause", "resume"}) {
		t.Errorf("argv %q", got)
	}
}

// A Run whose loop is stopped while it waits behind the previous loop's tick
// returns: it does not wait for a first tick that will never come.
func TestRunReturnsWhenItsLoopIsStoppedBeforeTheFirstTick(t *testing.T) {
	g := &gatedTicks{gate: make(chan struct{})}
	r := newTRig(t, g.script)
	first := make(chan error, 1)
	go func() { _, err := r.tk.Run(context.Background(), key); first <- err }()
	eventually(t, "the first tick to start", func() bool { return g.inFlight.Load() == 1 })
	r.tk.Stop(key)
	second := make(chan RunResult, 1)
	go func() { res, _ := r.tk.Run(context.Background(), key); second <- res }()
	eventually(t, "the second loop to be ticking", func() bool { info, _ := r.tk.Info(key); return info.Ticking })
	r.tk.Stop(key) // its loop still waits behind the first tick
	close(g.gate)
	select {
	case res := <-second:
		if res.Ticked {
			t.Errorf("the stopped loop ticked: %+v", res)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("Run did not return after its loop was stopped before the first tick")
	}
	if err := <-first; err != nil {
		t.Fatal(err)
	}
	never(t, "a tick from the stopped loop", func() bool { return r.exec.count(key, "tick") > 1 })
}
