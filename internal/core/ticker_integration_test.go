//go:build unix

package core

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"os"
	"os/exec"
	"path/filepath"
	"testing"
	"time"

	"github.com/zealtv/easyhog/internal/groundhog"
	"github.com/zealtv/easyhog/internal/runner"
)

// The ticker against the pinned upstream script, on temporary instances only.

type realRig struct {
	t   *testing.T
	key string
	tk  *Ticker
}

func pinnedBytes(t *testing.T) []byte {
	t.Helper()
	if _, err := exec.LookPath("bash"); err != nil {
		t.Skip("bash not found")
	}
	script, err := os.ReadFile(pinnedScript)
	if err != nil {
		t.Fatal(err)
	}
	if sum := sha256.Sum256(script); hex.EncodeToString(sum[:]) != pinnedSHA256 {
		t.Fatalf("%s no longer matches the pinned digest; update the pin deliberately", pinnedScript)
	}
	return script
}

// newRealRig makes <tmp>/proj/.groundhog with the pinned script, initialised,
// and a ticker over it. tweak adjusts the ticker's config.
func newRealRig(t *testing.T, tweak ...func(*TickerConfig)) *realRig {
	t.Helper()
	script := pinnedBytes(t)
	inst := mkInstance(t, tempRoot(t), "proj")
	if err := os.WriteFile(filepath.Join(inst, "groundhog.sh"), script, 0o755); err != nil {
		t.Fatal(err)
	}
	if out, err := exec.Command(filepath.Join(inst, "groundhog.sh"), "init").CombinedOutput(); err != nil {
		t.Fatalf("init: %v %s", err, out)
	}
	cfg := TickerConfig{Clients: NewClients(runner.Process{})}
	for _, f := range tweak {
		f(&cfg)
	}
	tk, err := NewTicker(cfg)
	if err != nil {
		t.Fatal(err)
	}
	r := &realRig{t: t, key: inst, tk: tk}
	sup, err := r.client().Negotiate(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	if !sup.Controllable {
		t.Skipf("this host offers no runs (%s); the ticker has nothing to do here", sup)
	}
	t.Cleanup(func() { tk.Quit(context.Background()) })
	return r
}

func (r *realRig) client() *groundhog.Client {
	r.t.Helper()
	c, err := r.tk.cfg.Clients.For(r.key)
	if err != nil {
		r.t.Fatal(err)
	}
	return c
}

func (r *realRig) status() groundhog.Status {
	r.t.Helper()
	st, err := r.client().Status(context.Background())
	if err != nil {
		r.t.Fatal(err)
	}
	return st
}

func (r *realRig) run() RunResult {
	r.t.Helper()
	res, err := r.tk.Run(context.Background(), r.key)
	if err != nil {
		r.t.Fatal(err)
	}
	return res
}

// item writes schedule/<rel> with an executable run of the given shell body.
func (r *realRig) item(rel, body string) {
	r.t.Helper()
	dir := filepath.Join(r.key, "schedule", filepath.FromSlash(rel))
	if err := os.MkdirAll(dir, 0o755); err != nil {
		r.t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(dir, "run"), []byte("#!/bin/sh\n"+body+"\n"), 0o755); err != nil {
		r.t.Fatal(err)
	}
}

// delivery waits for the delivery fired from schedule/<rel> to read state.
func (r *realRig) delivery(rel string, state groundhog.DeliveryState) groundhog.Delivery {
	r.t.Helper()
	var last []groundhog.Delivery
	deadline := time.Now().Add(20 * time.Second)
	for time.Now().Before(deadline) {
		last = r.status().Deliveries
		for _, d := range last {
			if d.SchedulePath != nil && *d.SchedulePath == "schedule/"+rel && d.State == state {
				return d
			}
		}
		time.Sleep(50 * time.Millisecond)
	}
	r.t.Fatalf("no delivery of %s reads %s: %+v", rel, state, last)
	return groundhog.Delivery{}
}

func waitFor(t *testing.T, what string, timeout time.Duration, f func() bool) {
	t.Helper()
	deadline := time.Now().Add(timeout)
	for !f() {
		if time.Now().After(deadline) {
			t.Fatalf("timed out after %s waiting for %s", timeout, what)
		}
		time.Sleep(20 * time.Millisecond)
	}
}

func TestRealRunTicksAnItemWhoseRunIsAcceptedAndStopEndsTicking(t *testing.T) {
	r := newRealRig(t)
	marker := filepath.Join(filepath.Dir(r.key), "ran")
	r.item("daily/hello", `echo "$GROUNDHOG_DELIVERY_ID" > "`+marker+`"`)
	if res := r.run(); !res.Ticked || res.TickErr != nil {
		t.Fatalf("%+v", res)
	}
	d := r.delivery("daily/hello", groundhog.DeliveryAccepted)
	if got, _ := os.ReadFile(marker); d.ID == nil || string(got) != *d.ID+"\n" {
		t.Errorf("the run saw delivery id %q, the record says %v", got, d.ID)
	}
	st := r.status()
	info, _ := r.tk.Info(r.key)
	if st.LastTick == nil || st.LastTick.Source != "standalone" || !info.Ours(st.LastTick.At) {
		t.Errorf("last_tick %+v not this session's (%+v)", st.LastTick, info)
	}
	if res := r.tk.Stop(r.key); !res.WasTicking {
		t.Error("Stop found nothing ticking")
	}
	if v := verdictFor(st, &info); v.Outlook != OutlookYes {
		t.Errorf("verdict while ticking: %+v", v)
	}
	info, _ = r.tk.Info(r.key)
	if v := verdictFor(st, &info); v.Outlook != OutlookNotSeen || !v.Ours {
		t.Errorf("after Stop, this session's own last tick is no outside ticker: %+v", v)
	}
}

// Quit just stops ticking: a run still going is left to go on in its own
// session, and its exit is recorded after Easyhog has let go.
func TestRealQuitLeavesARunThatIsStillGoingRunning(t *testing.T) {
	r := newRealRig(t)
	gate := filepath.Join(filepath.Dir(r.key), "gate")
	r.item("daily/slow", `while [ ! -e "`+gate+`" ]; do sleep 0.05; done`)
	r.run()
	r.delivery("daily/slow", groundhog.DeliveryRunning)
	began := time.Now()
	if n := r.tk.Quit(context.Background()); n != 1 {
		t.Fatalf("Quit stopped %d", n)
	}
	if d := time.Since(began); d > 5*time.Second {
		t.Errorf("Quit took %s", d)
	}
	r.delivery("daily/slow", groundhog.DeliveryRunning) // still going after Quit
	if err := os.WriteFile(gate, nil, 0o644); err != nil {
		t.Fatal(err)
	}
	r.delivery("daily/slow", groundhog.DeliveryAccepted)
}

func TestRealRetryRunsAFailedDeliveryAgain(t *testing.T) {
	r := newRealRig(t)
	r.item("daily/flaky", `[ -e "$1/tried" ] && exit 0; : > "$1/tried"; exit 7`)
	r.run()
	r.tk.Stop(r.key)
	d := r.delivery("daily/flaky", groundhog.DeliveryFailed)
	res, err := r.tk.Retry(context.Background(), r.key, "schedule/daily/flaky")
	if err != nil || len(res.Started) != 1 || res.Started[0] != d.Path {
		t.Fatalf("%+v %v", res, err)
	}
	if d := r.delivery("daily/flaky", groundhog.DeliveryAccepted); len(d.Attempts) != 2 || d.Attempts[1].By != "retry" {
		t.Errorf("%+v", d)
	}
	// Pause instance holds Retry back, and Unpause lifts it.
	if err := r.tk.SetPaused(context.Background(), r.key, true); err != nil {
		t.Fatal(err)
	}
	if !r.status().Paused {
		t.Error("not paused")
	}
	if err := r.tk.SetPaused(context.Background(), r.key, false); err != nil || r.status().Paused {
		t.Errorf("unpause: %v", err)
	}
}
