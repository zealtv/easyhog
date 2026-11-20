package groundhog

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"io/fs"
	"os"
	"os/exec"
	"path/filepath"
	"sort"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/zealtv/easyhog/internal/runner"
)

// Integration tests against a pinned copy of the real upstream script, in
// temporary instances only. They never run the script's `run` loop and never
// touch this repository's own .groundhog/.

const (
	pinnedScript = "testdata/upstream-9317d4e/groundhog.sh"
	pinnedSHA256 = "5a961d101ffc6d9186b836bd1f140c4c78e0b92770d682858a5a799d0cc562c2"
)

func needShell(t *testing.T) {
	t.Helper()
	if _, err := exec.LookPath("bash"); err != nil {
		t.Skip("bash not found")
	}
}

// newInstance writes script into <tmp>/my proj/.groundhog (a path with a space,
// canonical) with the three trays, and returns the instance directory.
func newInstance(t *testing.T, script []byte) string {
	t.Helper()
	root, err := filepath.EvalSymlinks(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	inst := filepath.Join(root, "my proj", ".groundhog")
	for _, d := range []string{"schedule", "out", "fired"} {
		if err := os.MkdirAll(filepath.Join(inst, d), 0o755); err != nil {
			t.Fatal(err)
		}
	}
	if err := os.WriteFile(filepath.Join(inst, "groundhog.sh"), script, 0o755); err != nil {
		t.Fatal(err)
	}
	return inst
}

func upstreamInstance(t *testing.T) string {
	t.Helper()
	needShell(t)
	script, err := os.ReadFile(pinnedScript)
	if err != nil {
		t.Fatal(err)
	}
	sum := sha256.Sum256(script)
	if hex.EncodeToString(sum[:]) != pinnedSHA256 {
		t.Fatalf("%s no longer matches the pinned digest; update the pin deliberately", pinnedScript)
	}
	return newInstance(t, script)
}

func realClient(t *testing.T, inst string, opts ...Option) *Client {
	t.Helper()
	c, err := New(runner.Process{}, inst, opts...)
	if err != nil {
		t.Fatal(err)
	}
	return c
}

// controllableClient skips the test when the host lacks what the script needs
// (a lock helper, a hash tool, perl or sync), as Groundhog's capabilities say.
func controllableClient(t *testing.T, inst string) *Client {
	t.Helper()
	c := realClient(t, inst)
	sup, err := c.Negotiate(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	if !sup.Controllable {
		t.Skipf("this host offers no runs (no lock helper): %s", sup)
	}
	return c
}

// tree lists every file under dir with size, mode and mtime, to prove a call
// changed nothing.
func tree(t *testing.T, dir string) string {
	t.Helper()
	var lines []string
	err := filepath.WalkDir(dir, func(p string, d fs.DirEntry, err error) error {
		if err != nil {
			return err
		}
		info, err := d.Info()
		if err != nil {
			return err
		}
		rel, _ := filepath.Rel(dir, p)
		lines = append(lines, strings.Join([]string{rel, info.Mode().String(), info.ModTime().String(), strconv.FormatInt(info.Size(), 10)}, "|"))
		return nil
	})
	if err != nil {
		t.Fatal(err)
	}
	sort.Strings(lines)
	return strings.Join(lines, "\n")
}

func TestRealScriptNegotiatesAndInspectsReadOnly(t *testing.T) {
	inst := upstreamInstance(t)
	c := realClient(t, inst)
	ctx := context.Background()

	// Something to show: one schedule item and one loose out/ entry.
	if err := os.MkdirAll(filepath.Join(inst, "schedule", "daily", "nudge"), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.MkdirAll(filepath.Join(inst, "out", "loose entry"), 0o755); err != nil {
		t.Fatal(err)
	}
	before := tree(t, inst)

	caps, err := c.Capabilities(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if caps.Protocol != Protocol || caps.ProtocolVersion != 2 || !caps.Has(CapStatusJSON) || !caps.Has(CapNextDue) {
		t.Errorf("%+v", caps)
	}
	sup, err := c.Negotiate(ctx)
	if err != nil || !sup.Inspectable {
		t.Fatalf("%+v %v", sup, err)
	}
	st, err := c.Status(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if st.Instance != inst || st.ProtocolVersion != 2 || st.Paused || st.LastTick != nil {
		t.Errorf("%+v", st)
	}
	if len(st.Schedule) != 1 || st.Schedule[0].Path != "schedule/daily/nudge" || st.Schedule[0].ItemID != "nudge" || st.Schedule[0].Cadence == nil || st.Schedule[0].Cadence.Axis != "daily" {
		t.Errorf("schedule: %+v", st.Schedule)
	}
	// Put in out/ by hand: no record Groundhog made, so it never runs.
	if len(st.Deliveries) != 1 || st.Deliveries[0].ID != nil || st.Deliveries[0].State != DeliveryUnrecorded || st.Deliveries[0].Run ||
		st.Deliveries[0].Path != "out/loose entry" {
		t.Errorf("deliveries: %+v", st.Deliveries)
	}
	if after := tree(t, inst); after != before {
		t.Errorf("read-only calls changed the instance:\n%s\n--- became ---\n%s", before, after)
	}
	if _, err := os.Stat(filepath.Join(inst, "runtime")); err == nil {
		t.Error("inspection created runtime/")
	}
}

func TestRealScriptIdentityMismatchThroughSymlink(t *testing.T) {
	inst := upstreamInstance(t)
	link := filepath.Join(filepath.Dir(filepath.Dir(inst)), "alias")
	if err := os.Symlink(filepath.Dir(inst), link); err != nil {
		t.Skipf("no symlinks: %v", err)
	}
	c := realClient(t, filepath.Join(link, ".groundhog"))
	_, err := c.Status(context.Background())
	var im *InstanceMismatchError
	if !errors.As(err, &im) || im.Got != inst {
		t.Fatalf("a non-canonical path must not pass as the instance: %v", err)
	}
}

// writeItem makes a schedule item at rel (under schedule/) whose executable run
// has the given shell body.
func writeItem(t *testing.T, inst, rel, body string) {
	t.Helper()
	dir := filepath.Join(inst, "schedule", filepath.FromSlash(rel))
	if err := os.MkdirAll(dir, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(dir, "run"), []byte("#!/bin/sh\n"+body+"\n"), 0o755); err != nil {
		t.Fatal(err)
	}
}

// deliveryOf waits for the delivery fired from schedule path sp to reach a
// state for which done holds, and returns it.
func deliveryOf(t *testing.T, c *Client, sp string, done func(DeliveryState) bool) Delivery {
	t.Helper()
	deadline := time.Now().Add(20 * time.Second)
	for {
		st, err := c.Status(context.Background())
		if err != nil {
			t.Fatal(err)
		}
		for _, d := range st.Deliveries {
			if d.SchedulePath != nil && *d.SchedulePath == sp && done(d.State) {
				return d
			}
		}
		if time.Now().After(deadline) {
			t.Fatalf("no delivery of %s reached the state wanted: %+v", sp, st.Deliveries)
		}
		time.Sleep(50 * time.Millisecond)
	}
}

func is(s DeliveryState) func(DeliveryState) bool {
	return func(x DeliveryState) bool { return x == s }
}

// The whole of protocol 2 that Easyhog drives, on the real script: a tick fires
// due items and starts their runs, a failing run reads failed, retry runs it
// again (and says busy while it runs), and the instance pause holds firings and
// retries back.
func TestRealScriptTicksRetriesAndPauses(t *testing.T) {
	inst := upstreamInstance(t)
	c := controllableClient(t, inst)
	ctx := context.Background()
	if err := os.MkdirAll(filepath.Join(inst, "runtime"), 0o755); err != nil {
		t.Fatal(err)
	}
	gate := filepath.Join(filepath.Dir(inst), "gate")
	writeItem(t, inst, "daily/ok", `echo "ran $1"`)
	// Fails the first time; later attempts wait for the gate file, then succeed.
	writeItem(t, inst, "daily/flaky", `if [ -e "$1/tried" ]; then while [ ! -e "`+gate+`" ]; do sleep 0.05; done; exit 0; fi; : > "$1/tried"; exit 3`)

	if err := c.Tick(ctx); err != nil {
		t.Fatal(err)
	}
	okDel := deliveryOf(t, c, "schedule/daily/ok", is(DeliveryAccepted))
	if !okDel.Run || len(okDel.Attempts) != 1 || okDel.Attempts[0].By != "tick" || okDel.Log == nil {
		t.Errorf("accepted delivery: %+v", okDel)
	}
	flaky := deliveryOf(t, c, "schedule/daily/flaky", is(DeliveryFailed))
	if flaky.ExitStatus == nil || *flaky.ExitStatus != 3 {
		t.Errorf("failed delivery: %+v", flaky)
	}
	st, _ := c.Status(ctx)
	if st.LastTick == nil || st.LastTick.Source != "standalone" {
		t.Errorf("last_tick after a tick: %+v", st.LastTick)
	}

	// Retry runs it again; while that attempt runs, a second retry is busy.
	if err := c.Retry(ctx, flaky.Path); err != nil {
		t.Fatal(err)
	}
	running := deliveryOf(t, c, "schedule/daily/flaky", is(DeliveryRunning))
	if len(running.Attempts) != 2 || running.Attempts[1].By != "retry" {
		t.Errorf("running delivery: %+v", running)
	}
	assertKind(t, c.Retry(ctx, flaky.Path), KindBusy, 4)
	if err := os.WriteFile(gate, nil, 0o644); err != nil {
		t.Fatal(err)
	}
	deliveryOf(t, c, "schedule/daily/flaky", is(DeliveryAccepted))

	// A delivery without a run, and a path that is no delivery here.
	if err := os.MkdirAll(filepath.Join(inst, "out", "by hand"), 0o755); err != nil {
		t.Fatal(err)
	}
	assertKind(t, c.Retry(ctx, "out/by hand"), KindNothing, 5)
	assertKind(t, c.Retry(ctx, "out/nothing here"), KindInvalid, 2)

	// The instance pause: idempotent, shown in status, and retry is refused.
	for range 2 {
		if err := c.Pause(ctx); err != nil {
			t.Fatal(err)
		}
	}
	if st, _ := c.Status(ctx); !st.Paused {
		t.Error("pause did not show in status")
	}
	assertKind(t, c.Retry(ctx, okDel.Path), KindPaused, 3)
	if err := c.Tick(ctx); err != nil { // a paused tick exits 0 and fires nothing
		t.Fatalf("a paused tick: %v", err)
	}
	if err := c.Resume(ctx); err != nil {
		t.Fatal(err)
	}
	if st, _ := c.Status(ctx); st.Paused {
		t.Error("resume did not show in status")
	}
}

func assertKind(t *testing.T, err error, kind Kind, code int) {
	t.Helper()
	var ee *ExitError
	if !errors.As(err, &ee) || ee.Kind != kind || ee.Code != code {
		t.Errorf("want exit %d (%v), got %v", code, kind, err)
		return
	}
	if ee.Stderr == "" {
		t.Errorf("exit %d kept no stderr", code)
	}
}

// An older Groundhog (protocol 1) is shown read-only with a plain reason, and
// nothing that changes an instance is ever asked of it.
func TestProtocol1ScriptIsReadOnlyAndUntouched(t *testing.T) {
	needShell(t)
	caps := `{"protocol":"groundhog-runtime","protocol_version":1,"capabilities":["status-json-v1","ownership-v1","runner-v1","pause-v1","consumer-v1","delivery-v1"]}`
	script := []byte("#!/bin/sh\ncase \"$1\" in\ncapabilities) echo '" + caps + "';;\n*) echo ran >> \"$(dirname \"$0\")/ran\"; exit 9;;\nesac\n")
	inst := newInstance(t, script)
	c := realClient(t, inst)
	ctx := context.Background()
	sup, err := c.Negotiate(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if sup.Controllable || sup.Inspectable || !strings.Contains(sup.Reason, "older Groundhog") {
		t.Errorf("%+v", sup)
	}
	for name, err := range map[string]error{
		"tick": c.Tick(ctx), "retry": c.Retry(ctx, "out/x"), "pause": c.Pause(ctx), "resume": c.Resume(ctx),
	} {
		if !errors.Is(err, ErrUnsupported) {
			t.Errorf("%s: %v", name, err)
		}
	}
	if _, err := os.Stat(filepath.Join(inst, "ran")); err == nil {
		t.Error("a command other than capabilities ran")
	}
}

func TestLegacyScriptIsReadOnlyAndUntouched(t *testing.T) {
	needShell(t)
	script := []byte("#!/bin/sh\necho \"error: unknown command '$1'\" >&2\nexit 1\n")
	inst := newInstance(t, script)
	before := tree(t, inst)
	c := realClient(t, inst)
	ctx := context.Background()

	sup, err := c.Negotiate(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if sup.Controllable || sup.Inspectable || !sup.ReadOnly() || !strings.Contains(sup.Reason, "legacy") || !strings.Contains(sup.Reason, "unknown command") {
		t.Errorf("%+v", sup)
	}
	for name, err := range map[string]error{
		"tick": c.Tick(ctx), "retry": c.Retry(ctx, "out/x"), "pause": c.Pause(ctx), "resume": c.Resume(ctx),
	} {
		if !errors.Is(err, ErrUnsupported) {
			t.Errorf("%s: %v", name, err)
		}
	}
	if after := tree(t, inst); after != before {
		t.Errorf("the legacy instance changed:\n%s\n--- became ---\n%s", before, after)
	}
	if got, _ := os.ReadFile(filepath.Join(inst, "groundhog.sh")); string(got) != string(script) {
		t.Error("the legacy script was replaced or upgraded")
	}
}

func TestRealProcessTimeoutAndOutputBounds(t *testing.T) {
	needShell(t)
	ctx := context.Background()

	hang := newInstance(t, []byte("#!/bin/sh\ncase \"$1\" in\ncapabilities) echo '"+strings.TrimSpace(jsonOf(t, fullCaps()))+"';;\n*) exec sleep 60;;\nesac\n"))
	c := realClient(t, hang)
	// Negotiate with the default bound: the first run of a fresh script can be
	// slow, and the short bounds below are meant for the commands under test.
	if sup, err := c.Negotiate(ctx); err != nil || !sup.Controllable {
		t.Fatalf("%+v %v", sup, err)
	}
	start := time.Now()
	_, err := c.Status(ctx, WithTimeout(200*time.Millisecond))
	if !errors.Is(err, context.DeadlineExceeded) || time.Since(start) > 10*time.Second {
		t.Fatalf("status did not time out promptly: %v after %v", err, time.Since(start))
	}
	// A hung tick is cut off at its bound, its outcome unknown.
	start = time.Now()
	err = c.Tick(ctx, WithTimeout(300*time.Millisecond))
	var ce *CallError
	if !errors.As(err, &ce) || !errors.Is(err, context.DeadlineExceeded) || time.Since(start) > 10*time.Second {
		t.Fatalf("tick timeout: %v after %v", err, time.Since(start))
	}

	flood := newInstance(t, []byte("#!/bin/sh\nexec yes\n"))
	start = time.Now()
	_, err = realClient(t, flood).Status(ctx, WithMaxStdout(4096))
	if !errors.Is(err, ErrOutputLimit) || time.Since(start) > 10*time.Second {
		t.Fatalf("oversized output: %v after %v", err, time.Since(start))
	}

	noisy := newInstance(t, []byte("#!/bin/sh\ncase \"$1\" in\ncapabilities) echo '"+strings.TrimSpace(jsonOf(t, fullCaps()))+"';;\n*) echo 'error: first line' >&2; yes 'error: noise' | head -c 2000000 >&2; exit 5;;\nesac\n"))
	nc := realClient(t, noisy)
	if sup, err := nc.Negotiate(ctx); err != nil || !sup.Controllable {
		t.Fatalf("%+v %v", sup, err)
	}
	err = nc.Pause(ctx)
	var ee *ExitError
	if !errors.As(err, &ee) || ee.Kind != KindNothing || ee.Code != 5 {
		t.Fatalf("%v", err)
	}
	if len(ee.Stderr) > DefaultMaxStderr || !ee.StderrTruncated || !strings.HasPrefix(ee.Stderr, "error: first line") || len(err.Error()) > 600 {
		t.Errorf("stderr kept %d bytes, truncated=%v, message %d bytes", len(ee.Stderr), ee.StderrTruncated, len(err.Error()))
	}
}

// next-due-v1, against the real script: a negotiated read carries next_due for
// each recognized item (the observation time for one that is due now, a later
// time for one that is not, none for a paused item), and a read without the
// negotiation leaves the field out.
func TestRealScriptReportsNextDueOnlyWhenNegotiated(t *testing.T) {
	inst := upstreamInstance(t)
	for _, d := range []string{"daily/nudge", "weekly/tue/later", "once.paused/held"} {
		if err := os.MkdirAll(filepath.Join(inst, "schedule", filepath.FromSlash(d)), 0o755); err != nil {
			t.Fatal(err)
		}
	}
	c := realClient(t, inst)
	ctx := context.Background()

	st, err := c.Status(ctx) // not negotiated: the plain read
	if err != nil {
		t.Fatal(err)
	}
	for _, it := range st.Schedule {
		if it.NextDue != nil {
			t.Errorf("%s: NextDue %v on a read that did not ask for it", it.Path, it.NextDue)
		}
	}

	if sup, err := c.Negotiate(ctx); err != nil || !sup.Inspectable {
		t.Fatalf("%+v %v", sup, err)
	}
	st, err = c.Status(ctx)
	if err != nil {
		t.Fatal(err)
	}
	byPath := map[string]ScheduleItem{}
	for _, it := range st.Schedule {
		byPath[it.Path] = it
	}
	if len(byPath) != 3 {
		t.Fatalf("schedule: %+v", st.Schedule)
	}
	if due := byPath["schedule/daily/nudge"]; !due.EligibleNow || due.NextDue == nil || !due.NextDue.Equal(st.ObservedAt) {
		t.Errorf("an item due now reads next_due = observed_at (%v), got %+v", st.ObservedAt, due)
	}
	if later := byPath["schedule/weekly/tue/later"]; later.EligibleNow || later.NextDue == nil || !later.NextDue.After(st.ObservedAt) {
		t.Errorf("a weekly item not yet due reads a later time than %v, got %+v", st.ObservedAt, later)
	}
	if held := byPath["schedule/once.paused/held"]; held.NextDue != nil {
		t.Errorf("a paused item reads null: %+v", held)
	}
}
