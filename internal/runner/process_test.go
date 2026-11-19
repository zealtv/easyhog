package runner

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

func TestProcessHelper(t *testing.T) {
	for i, arg := range os.Args {
		if arg != "easyhog-test-helper" {
			continue
		}
		switch os.Args[i+1] {
		case "inherited-pipe":
			child := exec.Command(os.Args[0], "-test.run=^TestProcessHelper$", "--", "easyhog-test-helper", "hold-pipe", os.Args[i+2])
			child.Stdout = os.Stdout
			if err := child.Start(); err != nil {
				os.Exit(2)
			}
			fmt.Println("ready")
			for {
				time.Sleep(time.Hour)
			}
		case "hold-pipe":
			deadline := time.Now().Add(10 * time.Second)
			for time.Now().Before(deadline) {
				if _, err := os.Stat(os.Args[i+2]); err == nil {
					_ = os.WriteFile(os.Args[i+2]+".done", nil, 0600)
					os.Exit(0)
				}
				time.Sleep(10 * time.Millisecond)
			}
		case "wait":
			fmt.Println("ready")
			for {
				time.Sleep(time.Hour)
			}
		case "echo":
			cwd, _ := os.Getwd()
			fmt.Printf("%s\n%s", cwd, os.Args[i+2])
		case "mark":
			if err := os.WriteFile(os.Args[i+2], []byte("started"), 0600); err != nil {
				os.Exit(2)
			}
		}
		os.Exit(0)
	}
}

func helper(t *testing.T, mode string, args ...string) Invocation {
	t.Helper()
	executable, err := os.Executable()
	if err != nil {
		t.Fatal(err)
	}
	return Invocation{Executable: executable, Dir: t.TempDir(), Args: append([]string{"-test.run=^TestProcessHelper$", "--", "easyhog-test-helper", mode}, args...)}
}

type readyWriter struct{ ready chan struct{} }

func (w readyWriter) Write(p []byte) (int, error) {
	select {
	case w.ready <- struct{}{}:
	default:
	}
	return len(p), nil
}

func TestCancellationWaitsForStartedChild(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	call := helper(t, "wait")
	ready := make(chan struct{}, 1)
	call.Stdout = readyWriter{ready}
	done := make(chan error, 1)
	go func() { done <- (Process{}).Run(ctx, call) }()
	select {
	case <-ready:
	case err := <-done:
		t.Fatalf("child exited before ready: %v", err)
	case <-time.After(10 * time.Second):
		t.Fatal("child did not start")
	}
	cancel()
	select {
	case err := <-done:
		if !errors.Is(err, context.Canceled) {
			t.Fatalf("got %v", err)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("canceled child was not reaped")
	}
}

func TestCanceledRequestNeverStartsChild(t *testing.T) {
	marker := filepath.Join(t.TempDir(), "started")
	call := helper(t, "mark", marker)
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	if err := (Process{}).Run(ctx, call); !errors.Is(err, context.Canceled) {
		t.Fatal(err)
	}
	if _, err := os.Stat(marker); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("child started: %v", err)
	}
}

func TestExplicitProcessArgumentsAndDirectory(t *testing.T) {
	literal := "$(echo unexpected); & | spaced argument"
	call := helper(t, "echo", literal)
	var output bytes.Buffer
	call.Stdout = &output
	if err := (Process{}).Run(context.Background(), call); err != nil {
		t.Fatal(err)
	}
	// Temp directories may have a canonical alias on desktop platforms.
	cwd := strings.SplitN(output.String(), "\n", 2)
	want, _ := filepath.EvalSymlinks(call.Dir)
	got, _ := filepath.EvalSymlinks(cwd[0])
	if len(cwd) != 2 || got != want || cwd[1] != literal {
		t.Fatalf("unexpected output %q", output.String())
	}
	for _, bad := range []Invocation{{Executable: "groundhog.sh", Dir: call.Dir}, {Executable: call.Executable, Dir: "."}} {
		if err := (Process{}).Run(context.Background(), bad); err == nil {
			t.Fatal("accepted relative process path")
		}
	}
}

// The descendant simulates an independent handoff retaining stdout. Closing
// the gate lets it exit on its own; cancellation must not wait for that gate.
func TestCancellationBoundsInheritedPipeDrain(t *testing.T) {
	gate := filepath.Join(t.TempDir(), "release")
	defer func() {
		if err := os.WriteFile(gate, nil, 0600); err != nil {
			t.Error(err)
			return
		}
		deadline := time.Now().Add(2 * time.Second)
		for time.Now().Before(deadline) {
			if _, err := os.Stat(gate + ".done"); err == nil {
				return
			}
			time.Sleep(10 * time.Millisecond)
		}
		t.Error("descendant did not acknowledge cleanup")
	}()
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	call := helper(t, "inherited-pipe", gate)
	ready := make(chan struct{}, 1)
	call.Stdout = readyWriter{ready}
	done := make(chan error, 1)
	go func() { done <- (Process{}).Run(ctx, call) }()
	select {
	case <-ready:
	case err := <-done:
		t.Fatalf("child exited before ready: %v", err)
	case <-time.After(10 * time.Second):
		t.Fatal("child did not start")
	}
	cancel()
	select {
	case err := <-done:
		if !errors.Is(err, context.Canceled) {
			t.Fatal(err)
		}
	case <-time.After(3 * time.Second):
		t.Fatal("inherited pipe blocked cancellation")
	}
}
