package groundhog

import (
	"context"
	"os"
	"os/exec"
	"path/filepath"
	"syscall"
	"testing"
	"time"
)

// Retry and Recover against the pinned script need real deliveries in the states
// they settle, which only a runner and a consumer make. This file is the one
// place in the package that starts `run` (and a consumer), in a temporary
// instance, and ends it before the test does.

type realRunner struct {
	t    *testing.T
	cmd  *exec.Cmd
	done chan struct{}
}

func startRunner(t *testing.T, inst, token string) *realRunner {
	t.Helper()
	log, err := os.Create(filepath.Join(t.TempDir(), "runner.log"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = log.Close() })
	cmd := exec.Command(filepath.Join(inst, "groundhog.sh"), "run", "--token-file", token, "--interval-seconds", "1")
	cmd.Dir, cmd.Stdout, cmd.Stderr = inst, log, log
	cmd.SysProcAttr = &syscall.SysProcAttr{Setpgid: true}
	if err := cmd.Start(); err != nil {
		t.Fatal(err)
	}
	r := &realRunner{t: t, cmd: cmd, done: make(chan struct{})}
	go func() { _ = cmd.Wait(); close(r.done) }()
	t.Cleanup(func() { _ = syscall.Kill(-cmd.Process.Pid, syscall.SIGTERM); <-r.done })
	return r
}

func waitFor(t *testing.T, what string, ok func() bool) {
	t.Helper()
	for end := time.Now().Add(30 * time.Second); time.Now().Before(end); time.Sleep(200 * time.Millisecond) {
		if ok() {
			return
		}
	}
	t.Fatalf("timed out waiting for %s", what)
}

func writeConsumer(t *testing.T, inst, body string) {
	t.Helper()
	script := filepath.Join(filepath.Dir(inst), "consumer.sh")
	if err := os.WriteFile(script, []byte("#!/bin/sh\n"+body+"\n"), 0o755); err != nil {
		t.Fatal(err)
	}
	cfg := `{"schema_version":1,"consumer":{"argv":["/bin/sh",` + quote(script) + `],"cwd":"..","env":{}}}` + "\n"
	if err := os.MkdirAll(filepath.Join(inst, "runtime"), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(inst, "runtime", "config.json"), []byte(cfg), 0o644); err != nil {
		t.Fatal(err)
	}
}

func quote(s string) string { return `"` + s + `"` }

func deliveryState(t *testing.T, c *Client) (id string, state DeliveryState) {
	t.Helper()
	st, err := c.Status(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	for _, d := range st.Deliveries {
		if d.ID != nil {
			return *d.ID, d.State
		}
	}
	return "", ""
}

func TestRealScriptRetryAndRecoverSettleDeliveries(t *testing.T) {
	inst := upstreamInstance(t)
	c := controllableClient(t, inst)
	ctx := context.Background()
	dir := privateDir(t)
	tok := filepath.Join(dir, "owner token")

	// One overdue once item, and a consumer that fails.
	if out, err := exec.Command(filepath.Join(inst, "groundhog.sh"), "add", "once/2020-01-01", "n1").CombinedOutput(); err != nil {
		t.Fatalf("add: %v: %s", err, out)
	}
	writeConsumer(t, inst, "exit 1")
	if err := c.Enroll(ctx, "test session", tok); err != nil {
		t.Fatal(err)
	}
	run := startRunner(t, inst, tok)
	var id string
	waitFor(t, "a failed delivery", func() bool {
		var s DeliveryState
		id, s = deliveryState(t, c)
		return s == DeliveryFailed
	})

	// A live runner refuses both, with the retryable busy exit; nothing changes.
	assertKind(t, c.Retry(ctx, tok, id), KindBusy, 4)
	assertKind(t, c.Recover(ctx, tok, id, DecisionAccept), KindBusy, 4)
	if _, s := deliveryState(t, c); s != DeliveryFailed {
		t.Fatalf("a refused retry changed the delivery to %s", s)
	}

	// Stopped: retry makes it pending, again and again without harm.
	if err := c.Stop(ctx, tok, WithTimeout(30*time.Second)); err != nil {
		t.Fatal(err)
	}
	<-run.done
	if err := c.Retry(ctx, tok, id); err != nil {
		t.Fatal(err)
	}
	if _, s := deliveryState(t, c); s != DeliveryPending {
		t.Fatalf("after retry the delivery is %s", s)
	}
	if err := c.Retry(ctx, tok, id); err != nil {
		t.Errorf("retrying a pending delivery: %v", err)
	}
	// Recover is for unresolved deliveries only; an unknown id has no record.
	assertKind(t, c.Recover(ctx, tok, id, DecisionAccept), KindState, 5)
	assertKind(t, c.Retry(ctx, tok, "no-such-delivery"), KindState, 5)
	// A token that does not match is an ownership conflict, certain.
	other := filepath.Join(dir, "stale token")
	if err := os.WriteFile(other, []byte(`{"schema_version":1,"instance":"x","owner_id":"o-x","generation":1,"token":"x"}`), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := c.Retry(ctx, other, id); err == nil {
		t.Error("a foreign token retried a delivery")
	}

	// A consumer that waits, and a runner that is SIGTERMed during its call, leave
	// the delivery uncertain with the attempt's owner recorded.
	release := filepath.Join(filepath.Dir(inst), "release")
	writeConsumer(t, inst, `while [ ! -e "`+release+`" ]; do sleep 0.2; done`)
	run = startRunner(t, inst, tok)
	waitFor(t, "an in-flight delivery", func() bool { _, s := deliveryState(t, c); return s == DeliveryInFlight })
	if err := syscall.Kill(-run.cmd.Process.Pid, syscall.SIGTERM); err != nil {
		t.Fatal(err)
	}
	<-run.done
	t.Cleanup(func() { _ = os.WriteFile(release, nil, 0o644) }) // let the detached consumer end
	if _, s := deliveryState(t, c); s != DeliveryUncertain {
		t.Fatalf("after SIGTERM the delivery is %s", mustState(t, c))
	}
	st, _ := c.Status(ctx)
	atts := st.Deliveries[0].Attempts
	if got := atts[len(atts)-1].OwnerLabel; got == nil || *got != "test session" {
		t.Errorf("attempt owner_label = %v, want the enrolling label", got)
	}
	assertKind(t, c.Retry(ctx, tok, id), KindState, 5) // retry is for failed ones
	if err := c.Recover(ctx, tok, id, DecisionRetry); err != nil {
		t.Fatal(err)
	}
	if _, s := deliveryState(t, c); s != DeliveryPending {
		t.Errorf("recover retry left the delivery %s", s)
	}
	st, _ = c.Status(ctx)
	atts = st.Deliveries[0].Attempts
	if d := atts[len(atts)-1].Decision; d == nil || *d != "retry" || atts[len(atts)-1].DecidedAt == nil {
		t.Errorf("the decision was not recorded on the attempt: %+v", atts[len(atts)-1])
	}
	if err := c.Release(ctx, tok); err != nil {
		t.Fatal(err)
	}
}

func mustState(t *testing.T, c *Client) DeliveryState {
	_, s := deliveryState(t, c)
	return s
}
