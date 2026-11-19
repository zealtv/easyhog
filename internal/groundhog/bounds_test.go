package groundhog

import (
	"context"
	"errors"
	"strings"
	"testing"
	"time"
)

func TestStatusTimeout(t *testing.T) {
	f := newFake(t).on("status", hangs())
	start := time.Now()
	_, err := newClient(t, f).Status(context.Background(), WithTimeout(30*time.Millisecond))
	var ce *CallError
	if !errors.As(err, &ce) || !errors.Is(err, context.DeadlineExceeded) || ce.Command != "status" {
		t.Fatalf("%v", err)
	}
	if time.Since(start) > 5*time.Second {
		t.Error("timeout not honoured")
	}
}

func TestParentCancellationIsNotATimeout(t *testing.T) {
	f := newFake(t).on("status", hangs())
	ctx, cancel := context.WithCancel(context.Background())
	go func() { time.Sleep(20 * time.Millisecond); cancel() }()
	_, err := newClient(t, f).Status(ctx)
	if !errors.Is(err, context.Canceled) || errors.Is(err, context.DeadlineExceeded) {
		t.Fatalf("%v", err)
	}
}

func TestClientLevelTimeoutApplies(t *testing.T) {
	f := newFake(t).on("status", hangs())
	_, err := newClient(t, f, WithTimeout(20*time.Millisecond)).Status(context.Background())
	if !errors.Is(err, context.DeadlineExceeded) {
		t.Fatalf("%v", err)
	}
}

func TestOversizedStdoutIsRefusedNotTruncated(t *testing.T) {
	doc := jsonOf(t, snapshot())
	f := newFake(t).on("status", ok(doc))
	st, err := newClient(t, f).Status(context.Background(), WithMaxStdout(len(doc)-1))
	var oe *OutputLimitError
	if !errors.As(err, &oe) || !errors.Is(err, ErrOutputLimit) || oe.Limit != len(doc)-1 || st.Instance != "" {
		t.Fatalf("%v %+v", err, st)
	}
	// Exactly at the limit is allowed.
	f = newFake(t).on("status", ok(doc))
	if _, err := newClient(t, f).Status(context.Background(), WithMaxStdout(len(doc))); err != nil {
		t.Fatal(err)
	}
}

func TestOversizedStdoutStopsAScriptThatKeepsWriting(t *testing.T) {
	f := newFake(t).on("status", reply{block: false, stdout: strings.Repeat("x", 1<<20)})
	_, err := newClient(t, f).Status(context.Background(), WithMaxStdout(1024))
	if !errors.Is(err, ErrOutputLimit) {
		t.Fatalf("%v", err)
	}
}

func TestStderrIsKeptBoundedAndTidy(t *testing.T) {
	noisy := "error: pause: the instance is busy\n" + strings.Repeat("noise line\n", 50000) + "\x1b[31mred\x1b[0m"
	f := newFake(t).on("pause", exits(4, noisy))
	err := controlClient(t, f).Pause(context.Background(), tokenPath, WithMaxStderr(2048))
	var ee *ExitError
	if !errors.As(err, &ee) {
		t.Fatal(err)
	}
	if len(ee.Stderr) != 2048 || !ee.StderrTruncated || !strings.HasPrefix(ee.Stderr, "error: pause: the instance is busy") {
		t.Errorf("stderr kept: %d bytes, truncated=%v", len(ee.Stderr), ee.StderrTruncated)
	}
	msg := err.Error()
	if len(msg) > 600 || strings.ContainsAny(msg, "\n\x1b") || !strings.Contains(msg, "the instance is busy") {
		t.Errorf("message %q", msg)
	}
	// Short stderr is kept whole and not flagged.
	f = newFake(t).on("pause", exits(3, "error: stale\n"))
	err = controlClient(t, f).Pause(context.Background(), tokenPath)
	if !errors.As(err, &ee) || ee.Stderr != "error: stale\n" || ee.StderrTruncated {
		t.Errorf("%+v", ee)
	}
}

func TestStderrSurvivesATimeout(t *testing.T) {
	f := newFake(t).on("stop", reply{block: false, stderr: "groundhog: waiting for the runner\n", err: context.DeadlineExceeded})
	err := controlClient(t, f).Stop(context.Background(), tokenPath, WithTimeout(time.Minute))
	var ce *CallError
	if !errors.As(err, &ce) || !strings.Contains(ce.Stderr, "waiting for the runner") {
		t.Fatalf("%v", err)
	}
}

func TestExecutorFailureIsACallError(t *testing.T) {
	f := newFake(t).on("status", reply{err: errors.New("fork/exec /x: permission denied")})
	_, err := newClient(t, f).Status(context.Background())
	var ce *CallError
	var ee *ExitError
	if !errors.As(err, &ce) || errors.As(err, &ee) || !strings.Contains(err.Error(), "permission denied") {
		t.Fatalf("%v", err)
	}
}
