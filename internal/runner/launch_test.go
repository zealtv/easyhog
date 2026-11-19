//go:build unix

package runner

import (
	"os"
	"path/filepath"
	"strings"
	"syscall"
	"testing"
	"time"
)

func logFile(t *testing.T) *os.File {
	t.Helper()
	f, err := os.OpenFile(filepath.Join(t.TempDir(), "run.log"), os.O_WRONLY|os.O_CREATE|os.O_APPEND, 0o600)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = f.Close() })
	return f
}

func start(t *testing.T, log *os.File, script string) Child {
	t.Helper()
	c, err := Spawner{}.Start(Launch{Executable: "/bin/sh", Args: []string{"-c", script}, Dir: t.TempDir(), Log: log})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		_ = c.Terminate()
		select {
		case <-c.Done():
		case <-time.After(5 * time.Second):
			t.Errorf("child %d outlived its test", c.PID())
		}
	})
	return c
}

func waitDone(t *testing.T, c Child) Exit {
	t.Helper()
	select {
	case <-c.Done():
		return c.Exit()
	case <-time.After(10 * time.Second):
		t.Fatal("child did not end")
		return Exit{}
	}
}

func TestSpawnerWritesStdoutAndStderrToTheFile(t *testing.T) {
	log := logFile(t)
	c := start(t, log, "echo out; echo err >&2; exit 3")
	x := waitDone(t, c)
	if x.Code != 3 || x.Signal != "" || x.Err != nil || x.OK() {
		t.Fatalf("exit %+v", x)
	}
	data, err := os.ReadFile(log.Name())
	if err != nil {
		t.Fatal(err)
	}
	if got := string(data); !strings.Contains(got, "out\n") || !strings.Contains(got, "err\n") {
		t.Fatalf("log %q", got)
	}
}

func TestSpawnerGivesTheChildAFileNotAPipe(t *testing.T) {
	log := logFile(t)
	// test -p is true for a FIFO or pipe.
	c := start(t, log, `[ ! -p /dev/stdout ] && echo regular-stdout; [ ! -p /dev/stderr ] && echo regular-stderr; [ ! -t 0 ] && echo stdin-not-a-tty`)
	waitDone(t, c)
	data, _ := os.ReadFile(log.Name())
	for _, want := range []string{"regular-stdout", "regular-stderr", "stdin-not-a-tty"} {
		if !strings.Contains(string(data), want) {
			t.Errorf("log %q lacks %q", data, want)
		}
	}
}

func TestSpawnerRefusesAPipeAndRelativePaths(t *testing.T) {
	r, w, err := os.Pipe()
	if err != nil {
		t.Fatal(err)
	}
	defer r.Close()
	defer w.Close()
	if _, err := (Spawner{}).Start(Launch{Executable: "/bin/sh", Dir: t.TempDir(), Log: w}); err == nil {
		t.Error("a pipe was accepted as the runner's output")
	}
	if _, err := (Spawner{}).Start(Launch{Executable: "/bin/sh", Dir: t.TempDir()}); err == nil {
		t.Error("a launch without a log was accepted")
	}
	if _, err := (Spawner{}).Start(Launch{Executable: "sh", Dir: t.TempDir(), Log: logFile(t)}); err == nil {
		t.Error("a relative executable was accepted")
	}
	if _, err := (Spawner{}).Start(Launch{Executable: "/bin/sh", Dir: "rel", Log: logFile(t)}); err == nil {
		t.Error("a relative directory was accepted")
	}
}

func TestChildOutlivesTheParentsHandleOnItsLog(t *testing.T) {
	// What a supervisor's exit looks like to the child: every descriptor the
	// parent held is gone. With a file the child goes on writing; with a pipe
	// the next write would be SIGPIPE.
	log := logFile(t)
	c := start(t, log, `sleep 0.3; echo still-writing; echo and-stderr >&2`)
	_ = log.Close()
	x := waitDone(t, c)
	if !x.OK() {
		t.Fatalf("child ended badly: %+v", x)
	}
	data, _ := os.ReadFile(log.Name())
	if !strings.Contains(string(data), "still-writing") || !strings.Contains(string(data), "and-stderr") {
		t.Fatalf("log %q", data)
	}
}

func TestTerminateSendsSIGTERMAndTheChildCanHandleIt(t *testing.T) {
	log := logFile(t)
	c := start(t, log, `trap 'echo got-term; exit 0' TERM; echo ready; while :; do sleep 0.05; done`)
	deadline := time.Now().Add(5 * time.Second)
	for {
		if data, _ := os.ReadFile(log.Name()); strings.Contains(string(data), "ready") {
			break
		}
		if time.Now().After(deadline) {
			t.Fatal("child never became ready")
		}
		time.Sleep(10 * time.Millisecond)
	}
	if err := c.Terminate(); err != nil {
		t.Fatal(err)
	}
	if x := waitDone(t, c); !x.OK() {
		t.Fatalf("exit %+v", x)
	}
	if data, _ := os.ReadFile(log.Name()); !strings.Contains(string(data), "got-term") {
		t.Fatalf("log %q", data)
	}
	if err := c.Terminate(); err == nil {
		t.Error("Terminate after the child ended reported success")
	}
}

func TestExitReportsASignalDeath(t *testing.T) {
	c := start(t, logFile(t), `kill -KILL $$`)
	x := waitDone(t, c)
	if x.Code != -1 || x.Signal != "killed" || x.OK() || !strings.Contains(x.String(), "killed") {
		t.Fatalf("exit %+v (%s)", x, x)
	}
}

func TestChildRunsInItsOwnProcessGroup(t *testing.T) {
	c := start(t, logFile(t), `sleep 5`)
	pgid, err := syscall.Getpgid(c.PID())
	if err != nil {
		t.Fatal(err)
	}
	if pgid != c.PID() {
		t.Fatalf("pgid %d, pid %d: a terminal's signals would reach the runner", pgid, c.PID())
	}
	if pgid == syscall.Getpgrp() {
		t.Fatal("the child shares the supervisor's process group")
	}
}
