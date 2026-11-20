//go:build unix

package main

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"testing"
	"time"

	"github.com/zealtv/easyhog/internal/core"
	"github.com/zealtv/easyhog/internal/groundhog"
	"github.com/zealtv/easyhog/internal/runner"
)

const (
	pinnedScript = "../../internal/groundhog/testdata/upstream-9317d4e/groundhog.sh"
	pinnedSHA256 = "5a961d101ffc6d9186b836bd1f140c4c78e0b92770d682858a5a799d0cc562c2"
)

// Whatever ends the terminal, main runs shutdown, which stops ticking at once:
// a run still going is not waited for, signalled or released, and finishes on
// its own afterwards. A temporary instance and the pinned script only.
func TestShutdownStopsTickingAndLeavesARunThatIsStillGoingToFinish(t *testing.T) {
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
	gate := filepath.Join(root, "gate")
	item := filepath.Join(inst, "schedule", "daily", "slow")
	if err := os.MkdirAll(item, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(item, "run"), []byte("#!/bin/sh\nwhile [ ! -e \""+gate+"\" ]; do sleep 0.05; done\n"), 0o755); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = os.WriteFile(gate, nil, 0o644) }) // the run ends with the test, whatever happens

	client, err := groundhog.New(runner.Process{}, inst)
	if err != nil {
		t.Fatal(err)
	}
	if support, err := client.Negotiate(context.Background()); err != nil || !support.Controllable {
		t.Skipf("this host offers no runs: %+v %v", support, err)
	}
	// A tick a second, so a loop that kept going would show within the test.
	tk, err := core.NewTicker(core.TickerConfig{Clients: core.NewClients(runner.Process{}), Interval: time.Second})
	if err != nil {
		t.Fatal(err)
	}
	if res, err := tk.Run(context.Background(), inst); err != nil || res.TickErr != nil {
		t.Fatalf("%+v %v", res, err)
	}
	state := func() groundhog.DeliveryState {
		st, err := client.Status(context.Background())
		if err != nil || len(st.Deliveries) != 1 {
			t.Fatalf("%+v %v", st, err)
		}
		return st.Deliveries[0].State
	}
	for deadline := time.Now().Add(10 * time.Second); state() != groundhog.DeliveryRunning; time.Sleep(50 * time.Millisecond) {
		if time.Now().After(deadline) {
			t.Fatalf("the run never started: %s", state())
		}
	}

	began := time.Now()
	shutdown(tk, nil, io.Discard)
	if d := time.Since(began); d > 5*time.Second {
		t.Errorf("shutdown took %s", d)
	}
	if s := state(); s != groundhog.DeliveryRunning {
		t.Fatalf("after shutdown the run reads %s, want still running", s)
	}
	// Ticking stopped: the loop is done, no later tick is recorded, and the
	// ticker takes no new Run.
	if info, _ := tk.Info(inst); info.Ticking {
		t.Error("still ticking after shutdown")
	}
	lastTick := func() time.Time {
		st, err := client.Status(context.Background())
		if err != nil || st.LastTick == nil {
			t.Fatalf("%+v %v", st.LastTick, err)
		}
		return st.LastTick.At
	}
	before := lastTick()
	time.Sleep(2500 * time.Millisecond)
	if after := lastTick(); !after.Equal(before) {
		t.Errorf("a tick at %s after shutdown (the last was %s)", after, before)
	}
	if _, err := tk.Run(context.Background(), inst); err == nil {
		t.Error("Run worked after shutdown")
	}
	if err := os.WriteFile(gate, nil, 0o644); err != nil {
		t.Fatal(err)
	}
	for deadline := time.Now().Add(10 * time.Second); state() != groundhog.DeliveryAccepted; time.Sleep(50 * time.Millisecond) {
		if time.Now().After(deadline) {
			t.Fatalf("the run never finished: %s", state())
		}
	}
}
