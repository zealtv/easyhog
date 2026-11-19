package groundhog

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
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
// temporary instances only. They never run `tick` or `run` and never touch this
// repository's own .groundhog/.

const (
	pinnedScript = "testdata/upstream-1c68c9d/groundhog.sh"
	pinnedSHA256 = "298c9d5d11465607796043d44c5e8dc42a20912aa52c00988505aaf8be09dbb9"
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
		t.Skipf("this host cannot run supervised control: %s", sup)
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

func privateDir(t *testing.T) string {
	t.Helper()
	dir := filepath.Join(t.TempDir(), "session dir")
	if err := os.Mkdir(dir, 0o700); err != nil {
		t.Fatal(err)
	}
	return dir
}

type tokenMeta struct {
	Instance   string `json:"instance"`
	OwnerID    string `json:"owner_id"`
	Generation int64  `json:"generation"`
}

func readTokenMeta(t *testing.T, path string) tokenMeta {
	t.Helper()
	b, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	var m tokenMeta
	if err := json.Unmarshal(b, &m); err != nil {
		t.Fatal(err)
	}
	return m
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
	if caps.Protocol != Protocol || caps.ProtocolVersion != 1 || !caps.Has(CapStatusJSON) {
		t.Errorf("%+v", caps)
	}
	// The pinned script offers both optional capabilities Quit and the status
	// reads negotiate (since Groundhog f189ad2).
	if !caps.Has(CapStatusActive) || !caps.Has(CapReleaseUnresolved) {
		t.Errorf("optional capabilities missing from %v", caps.Capabilities)
	}
	sup, err := c.Negotiate(ctx)
	if err != nil || !sup.Inspectable {
		t.Fatalf("%+v %v", sup, err)
	}
	st, err := c.Status(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if st.Instance != inst || st.Revision != 0 || st.JournalGeneration == nil || *st.JournalGeneration != 0 || !st.Consistent || st.Paused || st.Owner != nil || st.Runner.State != RunnerStopped {
		t.Errorf("%+v", st)
	}
	if st.Consumer.Configured || !st.Consumer.Valid {
		t.Errorf("consumer: %+v", st.Consumer)
	}
	// Negotiated, so the read asked for --active: the settled counts are there.
	if st.Settled == nil || st.Settled.Accepted != 0 || st.Settled.Missing != 0 {
		t.Errorf("settled %+v: the client should have passed --active to a script that offers it", st.Settled)
	}
	if len(st.Schedule) != 1 || st.Schedule[0].Path != "schedule/daily/nudge" || st.Schedule[0].ItemID != "nudge" || st.Schedule[0].Cadence == nil || st.Schedule[0].Cadence.Axis != "daily" {
		t.Errorf("schedule: %+v", st.Schedule)
	}
	if len(st.Deliveries) != 1 || st.Deliveries[0].ID != nil || st.Deliveries[0].Origin != "legacy_out" || st.Deliveries[0].State != DeliveryPending ||
		st.Deliveries[0].Reason == nil || *st.Deliveries[0].Reason != "unregistered" || st.Deliveries[0].Path != "out/loose entry" {
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

func TestRealScriptControlLifecycle(t *testing.T) {
	inst := upstreamInstance(t)
	c := controllableClient(t, inst)
	ctx := context.Background()
	dir := privateDir(t)
	tok := filepath.Join(dir, "owner token")

	status := func() Status {
		t.Helper()
		st, err := c.Status(ctx)
		if err != nil {
			t.Fatal(err)
		}
		return st
	}

	if err := c.Enroll(ctx, "test session", tok); err != nil {
		t.Fatal(err)
	}
	meta := readTokenMeta(t, tok)
	st := status()
	if st.Owner == nil || st.Owner.ID != meta.OwnerID || st.Owner.Generation != meta.Generation || meta.Instance != inst || st.Owner.Label != "test session" {
		t.Fatalf("owner %+v does not match the token's metadata %+v", st.Owner, meta)
	}
	if info, err := os.Stat(tok); err != nil || info.Mode().Perm() != 0o600 {
		t.Errorf("token file: %v %v", info, err)
	}
	rev := st.Revision
	if rev == 0 {
		t.Error("enroll left the revision at 0")
	}

	if err := c.Pause(ctx, tok); err != nil {
		t.Fatal(err)
	}
	st = status()
	if !st.Paused || st.Revision <= rev {
		t.Errorf("after pause: paused=%v revision %d (was %d)", st.Paused, st.Revision, rev)
	}
	rev = st.Revision
	if err := c.Pause(ctx, tok); err != nil { // idempotent
		t.Fatal(err)
	}
	if err := c.Resume(ctx, tok); err != nil {
		t.Fatal(err)
	}
	if st = status(); st.Paused || st.Revision <= rev {
		t.Errorf("after resume: paused=%v revision %d", st.Paused, st.Revision)
	}
	if err := c.Pause(ctx, tok); err != nil {
		t.Fatal(err)
	}

	// No runner is live, so stop returns at once and keeps ownership.
	if err := c.Stop(ctx, tok, WithTimeout(20*time.Second)); err != nil {
		t.Fatal(err)
	}
	if st = status(); st.Owner == nil || st.Runner.State != RunnerStopped {
		t.Errorf("after stop: %+v", st)
	}

	// A second enroll on an owned instance is an ownership conflict, certain.
	err := c.Enroll(ctx, "rival", filepath.Join(dir, "rival token"))
	assertKind(t, err, KindOwnership, 3)
	if errors.Is(err, ErrEnrollUncertain) {
		t.Error("a refused enroll was reported uncertain")
	}

	if err := c.Release(ctx, tok); err != nil {
		t.Fatal(err)
	}
	st = status()
	if st.Owner != nil || !st.Paused {
		t.Errorf("release must end ownership and keep the pause: %+v", st)
	}

	// The released token is stale now.
	assertKind(t, c.Pause(ctx, tok), KindOwnership, 3)
	assertKind(t, c.Stop(ctx, tok), KindOwnership, 3)
	assertKind(t, c.Release(ctx, tok), KindOwnership, 3)

	// Re-enrollment advances the generation.
	tok2 := filepath.Join(dir, "second token")
	if err := c.Enroll(ctx, "again", tok2); err != nil {
		t.Fatal(err)
	}
	if m := readTokenMeta(t, tok2); m.Generation != meta.Generation+1 || status().Owner.Generation != m.Generation {
		t.Errorf("generation did not advance: %+v after %+v", m, meta)
	}
	if err := c.Release(ctx, tok2); err != nil {
		t.Fatal(err)
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

func TestRealScriptTokenFileErrors(t *testing.T) {
	inst := upstreamInstance(t)
	c := controllableClient(t, inst)
	ctx := context.Background()
	dir := privateDir(t)

	// A missing and a malformed token file are exit 2.
	assertKind(t, c.Pause(ctx, filepath.Join(dir, "missing")), KindInvalid, 2)
	bad := filepath.Join(dir, "bad")
	if err := os.WriteFile(bad, []byte("{not json"), 0o600); err != nil {
		t.Fatal(err)
	}
	assertKind(t, c.Resume(ctx, bad), KindInvalid, 2)

	// Enroll onto an existing file: exit 2, no ownership.
	err := c.Enroll(ctx, "demo", bad)
	assertKind(t, err, KindInvalid, 2)
	if errors.Is(err, ErrEnrollUncertain) {
		t.Error("exit 2 is a certain failure")
	}
	if st, err := c.Status(context.Background()); err != nil || st.Owner != nil {
		t.Errorf("a failed enroll left an owner: %+v %v", st.Owner, err)
	}

	// An unwritable token directory fails before any owner commit: exit 6.
	if os.Geteuid() != 0 {
		ro := filepath.Join(dir, "ro")
		if err := os.Mkdir(ro, 0o500); err != nil {
			t.Fatal(err)
		}
		err := c.Enroll(ctx, "demo", filepath.Join(ro, "token"))
		assertKind(t, err, KindIO, 6)
		if errors.Is(err, ErrEnrollUncertain) {
			t.Error("exit 6 guarantees failure before the owner commit")
		}
		if st, _ := c.Status(ctx); st.Owner != nil {
			t.Error("exit 6 left an owner")
		}
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
	tok := filepath.Join(privateDir(t), "token")
	for name, err := range map[string]error{
		"enroll":  c.Enroll(ctx, "demo", tok),
		"pause":   c.Pause(ctx, tok),
		"resume":  c.Resume(ctx, tok),
		"stop":    c.Stop(ctx, tok),
		"release": c.Release(ctx, tok),
	} {
		if !errors.Is(err, ErrUnsupported) {
			t.Errorf("%s: %v", name, err)
		}
	}
	if _, err := os.Stat(tok); err == nil {
		t.Error("a token file appeared")
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
	// A timed-out enroll is uncertain, never failed.
	tok := filepath.Join(privateDir(t), "token")
	start = time.Now()
	err = c.Enroll(ctx, "demo", tok, WithTimeout(300*time.Millisecond))
	var ue *UncertainEnrollError
	if !errors.As(err, &ue) || !errors.Is(err, ErrEnrollUncertain) || ue.TokenFile != tok || time.Since(start) > 10*time.Second {
		t.Fatalf("enroll timeout: %v after %v", err, time.Since(start))
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
	err = nc.Pause(ctx, tok)
	var ee *ExitError
	if !errors.As(err, &ee) || ee.Kind != KindState || ee.Code != 5 {
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
