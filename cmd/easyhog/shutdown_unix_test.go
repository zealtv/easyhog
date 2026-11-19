//go:build unix

package main

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/zealtv/easyhog/internal/core"
	"github.com/zealtv/easyhog/internal/groundhog"
	"github.com/zealtv/easyhog/internal/runner"
)

const (
	pinnedScript = "../../internal/groundhog/testdata/upstream-1c68c9d/groundhog.sh"
	pinnedSHA256 = "298c9d5d11465607796043d44c5e8dc42a20912aa52c00988505aaf8be09dbb9"
)

// Whatever ends the terminal, main runs shutdown, which says what it is doing
// while it waits and then releases what this session enrolled. A temporary
// instance and the pinned script only.
func TestShutdownTellsTheReaderItIsStoppingRunnersAndReleasesThem(t *testing.T) {
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
	state, err := filepath.EvalSymlinks(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	sup, err := core.NewSupervisor(core.SupervisorConfig{Exec: runner.Process{}, Launcher: runner.Spawner{}, StateDir: state,
		PollEvery: 50 * time.Millisecond, StartWait: 10 * time.Second})
	if err != nil {
		t.Fatal(err)
	}
	client, err := groundhog.New(runner.Process{}, inst)
	if err != nil {
		t.Fatal(err)
	}
	if support, err := client.Negotiate(context.Background()); err != nil || !support.Controllable {
		t.Skipf("this host cannot run supervised control: %+v %v", support, err)
	}
	t.Cleanup(func() { sup.Quit(context.Background()) })

	if _, err := sup.Run(context.Background(), inst); err != nil {
		t.Fatal(err)
	}
	var stderr bytes.Buffer
	shutdown(sup, nil, &stderr)
	if !strings.HasPrefix(stderr.String(), "easyhog: Stopping 1 runner(s)") || !strings.Contains(stderr.String(), "30s") {
		t.Fatalf("stderr %q", stderr.String())
	}
	st, err := client.Status(context.Background())
	if err != nil || st.Owner != nil || st.Runner.State == groundhog.RunnerRunning {
		t.Fatalf("after shutdown: %+v %v", st, err)
	}
	if info, ok := sup.Runner(inst); ok && info.Phase != core.PhaseReleased {
		t.Fatalf("phase %v", info.Phase)
	}
}

// lockedBuffer lets a test read what shutdown has written while it still runs.
type lockedBuffer struct {
	mu sync.Mutex
	b  bytes.Buffer
}

func (l *lockedBuffer) Write(p []byte) (int, error) {
	l.mu.Lock()
	defer l.mu.Unlock()
	return l.b.Write(p)
}

func (l *lockedBuffer) String() string {
	l.mu.Lock()
	defer l.mu.Unlock()
	return l.b.String()
}

// gateExec holds the first `enroll` until release is closed, then runs it for
// real, and refuses every `release`, so that Quit has something to report.
type gateExec struct {
	runner.Executor
	once    sync.Once
	entered chan struct{}
	release chan struct{}
}

func (g *gateExec) Run(ctx context.Context, inv runner.Invocation) error {
	switch inv.Args[0] {
	case "enroll":
		g.once.Do(func() { close(g.entered); <-g.release })
	case "release":
		return errors.New("release refused for this test")
	}
	return g.Executor.Run(ctx, inv)
}

// A Quit that begins while the first Run of an instance is still enrolling has
// a runner count of zero. It must still say it is waiting, wait for the enroll,
// and print what it had to leave behind.
func TestShutdownWithARunStillEnrollingCountsItAndPrintsItsNotice(t *testing.T) {
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
	state, err := filepath.EvalSymlinks(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	gate := &gateExec{Executor: runner.Process{}, entered: make(chan struct{}), release: make(chan struct{})}
	sup, err := core.NewSupervisor(core.SupervisorConfig{Exec: gate, Launcher: runner.Spawner{}, StateDir: state,
		PollEvery: 50 * time.Millisecond, StartWait: 10 * time.Second})
	if err != nil {
		t.Fatal(err)
	}
	client, err := groundhog.New(runner.Process{}, inst)
	if err != nil {
		t.Fatal(err)
	}
	if support, err := client.Negotiate(context.Background()); err != nil || !support.Controllable {
		t.Skipf("this host cannot run supervised control: %+v %v", support, err)
	}
	ran := make(chan struct{})
	go func() { defer close(ran); _, _ = sup.Run(context.Background(), inst) }()
	select {
	case <-gate.entered:
	case <-time.After(30 * time.Second):
		t.Fatal("the enroll never began")
	}
	if n := len(sup.Runners()); n != 0 {
		t.Fatalf("%d runners listed while the first Run is still enrolling", n)
	}

	var stderr lockedBuffer
	finished := make(chan struct{})
	go func() { defer close(finished); shutdown(sup, nil, &stderr) }()
	deadline := time.Now().Add(30 * time.Second)
	for !strings.Contains(stderr.String(), "Stopping 1 runner(s)") && time.Now().Before(deadline) {
		select {
		case <-finished:
			deadline = time.Now() // shutdown ended without saying it was waiting
		case <-time.After(10 * time.Millisecond):
		}
	}
	if !strings.HasPrefix(stderr.String(), "easyhog: Stopping 1 runner(s)") {
		t.Fatalf("stderr before the enroll finished: %q", stderr.String())
	}
	close(gate.release)
	select {
	case <-finished:
	case <-time.After(60 * time.Second):
		t.Fatal("shutdown did not return")
	}
	<-ran
	out := stderr.String()
	if !strings.Contains(out, "left owned") {
		t.Fatalf("shutdown printed no notice for the instance it left owned: %q", out)
	}
	if strings.Contains(out, "SECRET") || strings.Contains(out, `"token"`) {
		t.Errorf("a token's contents reached stderr: %q", out)
	}
}
