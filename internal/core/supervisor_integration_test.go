//go:build unix

package core

import (
	"bufio"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"strings"
	"sync"
	"syscall"
	"testing"
	"time"

	"github.com/zealtv/easyhog/internal/groundhog"
	"github.com/zealtv/easyhog/internal/runner"
)

// The supervisor against the pinned upstream script and real runner processes,
// on temporary instances only. The consumers are plain /bin/sh scripts. Nothing
// here touches this repository's own .groundhog/ or any real schedule.

// trackingLauncher starts real runners and, at test end, makes sure none
// outlives its test (SIGTERM, which a runner handles by closing admission).
type trackingLauncher struct {
	mu       sync.Mutex
	children []runner.Child
}

func (l *trackingLauncher) Start(ln runner.Launch) (runner.Child, error) {
	c, err := runner.Spawner{}.Start(ln)
	if err == nil {
		l.mu.Lock()
		l.children = append(l.children, c)
		l.mu.Unlock()
	}
	return c, err
}

func (l *trackingLauncher) count() int {
	l.mu.Lock()
	defer l.mu.Unlock()
	return len(l.children)
}

func (l *trackingLauncher) last() runner.Child {
	l.mu.Lock()
	defer l.mu.Unlock()
	return l.children[len(l.children)-1]
}

func (l *trackingLauncher) reapAll(t *testing.T) {
	l.mu.Lock()
	children := append([]runner.Child(nil), l.children...)
	l.mu.Unlock()
	for _, c := range children {
		_ = c.Terminate()
		select {
		case <-c.Done():
		case <-time.After(10 * time.Second):
			t.Errorf("runner %d outlived its test", c.PID())
		}
	}
}

// countingExec counts calls per command and can alter one command's result.
type countingExec struct {
	runner.Executor
	mu     sync.Mutex
	counts map[string]int
	// after, when set, replaces a command's result once it has really run.
	after func(cmd string, n int, err error) error
	// instead, when set, replaces the command without running it.
	instead func(cmd string, n int) (error, bool)
}

func (e *countingExec) Run(ctx context.Context, inv runner.Invocation) error {
	cmd := inv.Args[0]
	e.mu.Lock()
	if e.counts == nil {
		e.counts = map[string]int{}
	}
	e.counts[cmd]++
	n := e.counts[cmd]
	e.mu.Unlock()
	if e.instead != nil {
		if err, ok := e.instead(cmd, n); ok {
			return err
		}
	}
	err := e.Executor.Run(ctx, inv)
	if e.after != nil {
		return e.after(cmd, n, err)
	}
	return err
}

func (e *countingExec) count(cmd string) int {
	e.mu.Lock()
	defer e.mu.Unlock()
	return e.counts[cmd]
}

type realRig struct {
	t     *testing.T
	root  string // canonical temp dir holding the project
	key   string // the instance
	state string
	sup   *Supervisor
	l     *trackingLauncher
	exec  *countingExec
}

// pinnedScript is read from the groundhog package's fixture (integration_test.go).
func pinnedBytes(t *testing.T) []byte {
	t.Helper()
	script, err := os.ReadFile(pinnedScript)
	if err != nil {
		t.Fatal(err)
	}
	if sum := sha256.Sum256(script); hex.EncodeToString(sum[:]) != pinnedSHA256 {
		t.Fatalf("%s no longer matches the pinned digest; update the pin deliberately", pinnedScript)
	}
	return script
}

// newInstanceWith makes <root>/proj/.groundhog with the pinned script, a
// consumer script (the empty string means no consumer) and, when item is set,
// one `once` item that fires on the first cycle.
func newInstanceWith(t *testing.T, root, consumer string, item bool) string {
	t.Helper()
	proj := filepath.Join(root, "proj")
	inst := filepath.Join(proj, ".groundhog")
	for _, d := range []string{"schedule", "out", "fired", "runtime"} {
		if err := os.MkdirAll(filepath.Join(inst, d), 0o755); err != nil {
			t.Fatal(err)
		}
	}
	if err := os.WriteFile(filepath.Join(inst, "groundhog.sh"), pinnedBytes(t), 0o755); err != nil {
		t.Fatal(err)
	}
	if consumer != "" {
		path := filepath.Join(root, "consumer.sh")
		if err := os.WriteFile(path, []byte("#!/bin/sh\n"+consumer+"\n"), 0o755); err != nil {
			t.Fatal(err)
		}
		cfg := fmt.Sprintf(`{"schema_version":1,"consumer":{"argv":["/bin/sh",%q],"cwd":".."}}`, path)
		if err := os.WriteFile(filepath.Join(inst, "runtime", "config.json"), []byte(cfg), 0o644); err != nil {
			t.Fatal(err)
		}
	}
	if item {
		dir := filepath.Join(inst, "schedule", "once", "job")
		if err := os.MkdirAll(dir, 0o755); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(filepath.Join(dir, "note.md"), []byte("hello\n"), 0o644); err != nil {
			t.Fatal(err)
		}
	}
	return inst
}

// newRealRig builds the instance and a supervisor over real processes. consumer
// is the body of a /bin/sh script; $MARKER is a path in the project directory.
func newRealRig(t *testing.T, consumer string, item bool, tweak ...func(*SupervisorConfig)) *realRig {
	t.Helper()
	root := mustReal(t, t.TempDir())
	consumer = strings.ReplaceAll(consumer, "$MARKER", filepath.Join(root, "proj", "marker"))
	key := newInstanceWith(t, root, consumer, item)
	l := &trackingLauncher{}
	ex := &countingExec{Executor: runner.Process{}}
	cfg := SupervisorConfig{
		Exec: ex, Launcher: l, StateDir: mustReal(t, t.TempDir()), Label: "it-session",
		PollEvery: 50 * time.Millisecond, BusyDelay: 100 * time.Millisecond, StartWait: 10 * time.Second,
	}
	for _, f := range tweak {
		f(&cfg)
	}
	sup, err := NewSupervisor(cfg)
	if err != nil {
		t.Fatal(err)
	}
	rg := &realRig{t: t, root: root, key: key, state: cfg.StateDir, sup: sup, l: l, exec: ex}
	c := rg.client()
	if support, err := c.Negotiate(context.Background()); err != nil || !support.Controllable {
		t.Skipf("this host cannot run supervised control: %v %v", support, err)
	}
	t.Cleanup(func() {
		ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
		defer cancel()
		sup.Quit(ctx)
		l.reapAll(t)
		noStrays(t, root)
	})
	return rg
}

// noStrays fails the test if any process whose command line names the rig's
// temporary directory (a runner, or a script a call started) is still alive a
// few seconds after Quit and reaping, and sends what it finds SIGTERM so it
// does not outlive the test run. `make test` once left two such `bash`
// processes behind; this is how the next one
// gets caught by the test that started it. It skips where pgrep is missing.
func noStrays(t *testing.T, dir string) {
	t.Helper()
	pgrep, err := exec.LookPath("pgrep")
	if err != nil {
		return
	}
	list := func() string {
		out, _ := exec.Command(pgrep, "-fl", dir).Output() // exit 1: none
		return strings.TrimSpace(string(out))
	}
	var left string
	for deadline := time.Now().Add(5 * time.Second); time.Now().Before(deadline); time.Sleep(50 * time.Millisecond) {
		if left = list(); left == "" {
			return
		}
	}
	_ = exec.Command(pgrep, "-f", dir).Run()
	if pkill, err := exec.LookPath("pkill"); err == nil {
		_ = exec.Command(pkill, "-TERM", "-f", dir).Run()
	}
	t.Errorf("processes started by this test were still alive after Quit:\n%s", left)
}

func (r *realRig) client() *groundhog.Client {
	c, err := groundhog.New(runner.Process{}, r.key)
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

func (r *realRig) marker() string { return filepath.Join(r.root, "proj", "marker") }

func (r *realRig) run() RunResult {
	r.t.Helper()
	res, err := r.sup.Run(context.Background(), r.key)
	if err != nil {
		r.t.Fatalf("Run: %v", err)
	}
	return res
}

func waitFor(t *testing.T, what string, timeout time.Duration, f func() bool) {
	t.Helper()
	for deadline := time.Now().Add(timeout); time.Now().Before(deadline); time.Sleep(25 * time.Millisecond) {
		if f() {
			return
		}
	}
	t.Fatalf("timed out after %v waiting for %s", timeout, what)
}

func fileExists(p string) bool { _, err := os.Stat(p); return err == nil }

func (r *realRig) waitInFlight() {
	r.t.Helper()
	waitFor(r.t, "the consumer call to be in flight", 15*time.Second, func() bool { return hasInFlight(r.status()) })
}

func TestRealRunTwiceThenQuitWithNothingInFlight(t *testing.T) {
	r := newRealRig(t, "", false)
	res := r.run()
	if res.Outcome != RunStarted || !res.Enrolled || !res.Seen {
		t.Fatalf("Run %+v", res)
	}
	st := r.status()
	if st.Runner.State != groundhog.RunnerRunning || st.Owner == nil || st.Owner.Label != "it-session" {
		t.Fatalf("status %+v", st)
	}
	if again := r.run(); again.Outcome != RunAlreadyRunning {
		t.Fatalf("second Run %+v", again)
	}
	if r.l.count() != 1 || r.exec.count("enroll") != 1 {
		t.Fatalf("%d runners started, %d enrolls", r.l.count(), r.exec.count("enroll"))
	}
	child := r.l.last()
	token := r.runnerInfo().TokenFile
	// A pause set by something else (Easyhog sets none): Quit leaves it alone.
	if err := r.client().Pause(context.Background(), token); err != nil || !r.status().Paused {
		t.Fatalf("pause: %v", err)
	}

	rep := r.sup.Quit(context.Background())
	if len(rep.Instances) != 1 || rep.Instances[0].Outcome != QuitReleased || !rep.Clean() || len(rep.Notices) != 0 {
		t.Fatalf("report %+v", rep)
	}
	st = r.status()
	if st.Owner != nil || st.Runner.State != groundhog.RunnerStopped || !st.Paused {
		t.Errorf("after Quit: owner %v runner %v paused %v (want released, stopped, pause as it was)", st.Owner, st.Runner.State, st.Paused)
	}
	select {
	case <-child.Done():
		if x := child.Exit(); !x.OK() {
			t.Errorf("runner exit %+v", x)
		}
	default:
		t.Error("the runner process is still alive")
	}
	if fileExists(token) || len(filesUnder(t, r.state)) != 0 {
		t.Errorf("tokens or logs left: %v", filesUnder(t, r.state))
	}
}

// Stop on the pinned script: the runner ends through `stop`, the instance is
// released (no owner), the token and log go, and Run then enrolls it afresh and
// starts a second runner; Quit after that releases it again.
func TestRealStopReleasesAndRunEnrollsAgain(t *testing.T) {
	r := newRealRig(t, "", false)
	r.run()
	first := r.l.last()
	token := r.runnerInfo().TokenFile
	owner := r.status().Owner
	if owner == nil {
		t.Fatal("no owner after Run")
	}

	res, err := r.sup.Stop(context.Background(), r.key)
	if err != nil || res.Terminated || res.Why != "" {
		t.Fatalf("Stop %+v %v", res, err)
	}
	if st := r.status(); st.Owner != nil || st.Runner.State != groundhog.RunnerStopped || st.Paused {
		t.Fatalf("after Stop: owner %v runner %v paused %v (want released, stopped, not paused)", st.Owner, st.Runner.State, st.Paused)
	}
	select {
	case <-first.Done():
		if x := first.Exit(); !x.OK() {
			t.Errorf("runner exit %+v, want a clean exit", x)
		}
	default:
		t.Error("the runner process is still alive after Stop")
	}
	if fileExists(token) || len(filesUnder(t, r.state)) != 0 {
		t.Errorf("token or log left: %v", filesUnder(t, r.state))
	}
	if _, ok := r.sup.Runner(r.key); ok {
		t.Error("the session still holds the instance")
	}
	if r.exec.count("release") != 1 || r.exec.count("pause") != 0 {
		t.Errorf("%d releases, %d pauses", r.exec.count("release"), r.exec.count("pause"))
	}

	again := r.run()
	if again.Outcome != RunStarted || !again.Enrolled || r.l.count() != 2 || r.exec.count("enroll") != 2 {
		t.Fatalf("Run after Stop %+v, %d runners, %d enrolls", again, r.l.count(), r.exec.count("enroll"))
	}
	if now := r.status().Owner; now == nil || now.ID == owner.ID {
		t.Errorf("owner %+v after Run, was %+v: want a new one", now, owner)
	}
	rep := r.sup.Quit(context.Background())
	if len(rep.Instances) != 1 || rep.Instances[0].Outcome != QuitReleased || !rep.Clean() || len(rep.Notices) != 0 {
		t.Fatalf("report %+v", rep)
	}
	if st := r.status(); st.Owner != nil || st.Runner.State != groundhog.RunnerStopped {
		t.Errorf("after Quit: owner %v runner %v", st.Owner, st.Runner.State)
	}
}

func (r *realRig) runnerInfo() RunnerInfo {
	r.t.Helper()
	info, ok := r.sup.Runner(r.key)
	if !ok {
		r.t.Fatalf("%s is not supervised", r.key)
	}
	return info
}

func TestRealQuitWaitsForAConsumerCallThatFinishes(t *testing.T) {
	r := newRealRig(t, "sleep 2\necho done > $MARKER", true, func(c *SupervisorConfig) { c.QuitWait = 20 * time.Second })
	r.run()
	r.waitInFlight()
	if fileExists(r.marker()) {
		t.Fatal("the consumer finished before the test could quit")
	}
	began := time.Now()
	rep := r.sup.Quit(context.Background())
	in := rep.Instances[0]
	if in.Outcome != QuitReleased || in.Terminated || !rep.Clean() || len(rep.Notices) != 0 {
		t.Fatalf("%+v", in)
	}
	if in.Waited < time.Second || time.Since(began) > 15*time.Second {
		t.Errorf("waited %v, quit took %v", in.Waited, time.Since(began))
	}
	if !fileExists(r.marker()) {
		t.Error("the consumer's work did not finish")
	}
	st := r.status()
	if st.Owner != nil || st.DeliveryCounts()[groundhog.DeliveryAccepted] != 1 || st.DeliveryCounts()[groundhog.DeliveryUncertain] != 0 {
		t.Errorf("owner %v deliveries %v", st.Owner, st.DeliveryCounts())
	}
	if st.Paused {
		t.Error("Quit paused the instance")
	}
}

// A call longer than QuitWait: the stop gives up at the bound, the runner gets
// SIGTERM (the call is detached and the delivery uncertain), and the instance is
// released all the same with --leave-unresolved. The consumer is never killed.
func TestRealQuitDuringACallLongerThanQuitWaitTerminatesTheRunnerAndReleases(t *testing.T) {
	r := newRealRig(t, "sleep 4\necho done > $MARKER", true, func(c *SupervisorConfig) { c.QuitWait = 500 * time.Millisecond })
	r.run()
	r.waitInFlight()
	token := r.runnerInfo().TokenFile
	child := r.l.last()
	began := time.Now()
	rep := r.sup.Quit(context.Background())
	in := rep.Instances[0]
	if in.Outcome != QuitReleased || !in.Terminated || !in.RunnerGone || !rep.Clean() {
		t.Fatalf("%+v", in)
	}
	if took := time.Since(began); took > 3*time.Second {
		t.Errorf("quit took %v against a 500ms wait", took)
	}
	if !strings.Contains(in.Notice, "SIGTERM") || !strings.Contains(in.Notice, "uncertain") {
		t.Errorf("notice %q", in.Notice)
	}
	if fileExists(token) {
		t.Error("the token should be deleted after the release")
	}
	if x := child.Exit(); x.Signal != "" || x.Code != 0 {
		t.Errorf("runner exit %+v: SIGTERM should end it cleanly (a SIGKILL shows as a signal)", x)
	}
	st := r.status()
	if st.Owner != nil || st.Runner.State != groundhog.RunnerStopped {
		t.Errorf("owner %v runner %v", st.Owner, st.Runner.State)
	}
	if len(st.Deliveries) != 1 || st.Deliveries[0].State != groundhog.DeliveryUncertain {
		t.Errorf("deliveries %+v: want the one delivery uncertain", st.Deliveries)
	}
	// The consumer, detached into its own session, finishes its work: it
	// survived Easyhog's quit and the runner's SIGTERM.
	if fileExists(r.marker()) {
		t.Fatal("the consumer had already finished")
	}
	waitFor(t, "the detached consumer to finish", 15*time.Second, func() bool { return fileExists(r.marker()) })
}

// A second interrupt arrives (the quit's context is already ended) after Run
// and before any stop. `stop` cannot run, the runner is sent SIGTERM and ends,
// and release still goes through under its own bound.
func TestRealDoubleInterruptStillReleasesOnceTheRunnerEnded(t *testing.T) {
	r := newRealRig(t, "", false)
	r.run()
	token := r.runnerInfo().TokenFile
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	rep := r.sup.Quit(ctx)
	in := rep.Instances[0]
	if in.Outcome != QuitReleased || !in.RunnerGone || !rep.Clean() {
		t.Fatalf("%+v", in)
	}
	if st := r.status(); st.Owner != nil || st.Runner.State != groundhog.RunnerStopped {
		t.Errorf("owner %v runner %v", st.Owner, st.Runner.State)
	}
	if fileExists(token) {
		t.Error("the token should be deleted")
	}
}

func TestRealQuitSendsSIGTERMWhenStopFails(t *testing.T) {
	r := newRealRig(t, "", false)
	r.exec.instead = func(cmd string, _ int) (error, bool) {
		if cmd == "stop" {
			return exitStatus(6), true
		}
		return nil, false
	}
	r.run()
	child := r.l.last()
	rep := r.sup.Quit(context.Background())
	in := rep.Instances[0]
	if in.Outcome != QuitReleased || !in.RunnerGone || !strings.Contains(in.Notice, "stop failed") {
		t.Fatalf("%+v", in)
	}
	// SIGTERM, to which the runner answers by closing admission and exiting 0.
	// A SIGKILL would show here as a signal death.
	if x := child.Exit(); x.Code != 0 || x.Signal != "" {
		t.Errorf("runner exit %+v", x)
	}
	if st := r.status(); st.Owner != nil || st.Runner.State != groundhog.RunnerStopped {
		t.Errorf("owner %v runner %v", st.Owner, st.Runner.State)
	}
}

func TestRealUncertainEnrollIsSettledByTheOwnershipRule(t *testing.T) {
	r := newRealRig(t, "", false)
	r.exec.after = func(cmd string, n int, err error) error {
		if cmd == "enroll" && n == 1 && err == nil {
			return exitStatus(7) // the owner committed; the result was lost
		}
		return err
	}
	_, err := r.sup.Run(context.Background(), r.key)
	var unc *groundhog.UncertainEnrollError
	if !errors.As(err, &unc) {
		t.Fatalf("Run: %v, want an uncertain enroll", err)
	}
	if r.l.count() != 0 || r.status().Runner.State != groundhog.RunnerStopped {
		t.Fatal("a runner was started by an uncertain enrollment")
	}
	st := r.status()
	if st.Owner == nil || !r.sup.IsOurs(*st.Owner) {
		t.Fatalf("owner %+v: the token this session kept matches it, so it is ours", st.Owner)
	}
	if res := r.run(); res.Outcome != RunStarted || r.exec.count("enroll") != 1 {
		t.Fatalf("second Run %+v after %d enrolls", res, r.exec.count("enroll"))
	}
}

// An interrupted journal write
// leaves runtime/journal.gen odd, status stays consistent:false with no owner,
// and only a command that takes the mutation lock settles it. Run offers
// itself, enrolls, and the status is coherent afterwards.
func TestRealRunSettlesAnInterruptedJournalWrite(t *testing.T) {
	r := newRealRig(t, "", false)
	gen := filepath.Join(r.key, "runtime", "journal.gen")
	if err := os.WriteFile(gen, []byte("000000000000000005\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	for i := 0; i < 3; i++ { // status alone never settles it
		if st := r.status(); st.Consistent || st.Owner != nil || !SettlesOnRun(st) {
			t.Fatalf("read %d: consistent %v, owner %v, diagnostics %+v", i, st.Consistent, st.Owner, st.Diagnostics)
		}
	}
	st := r.status()
	iv := InstanceView{Class: ClassUnowned, Health: HealthInconsistent, Inspection: Inspection{Last: &Observation{Status: st}}}
	if got, why := Offers(iv, nil); len(got) != 1 || got[0] != ActionRun {
		t.Fatalf("offers %v (%q)", got, why)
	}
	res := r.run()
	if res.Outcome != RunStarted || !res.Enrolled {
		t.Fatalf("Run %+v", res)
	}
	if st := r.status(); !st.Consistent || st.Owner == nil || !r.sup.IsOurs(*st.Owner) || st.Runner.State != groundhog.RunnerRunning {
		t.Fatalf("after Run: consistent %v, owner %+v, runner %q, diagnostics %+v", st.Consistent, st.Owner, st.Runner.State, st.Diagnostics)
	}
}

func TestRealExternalOwnerIsRefusedEvenWithItsRunnerStopped(t *testing.T) {
	r := newRealRig(t, "", false)
	// Another supervisor enrolls the instance, then goes away (runner stopped).
	other := mustReal(t, t.TempDir())
	if err := os.Chmod(other, 0o700); err != nil {
		t.Fatal(err)
	}
	tok := filepath.Join(other, "their.token")
	if err := r.client().Enroll(context.Background(), "their-supervisor", tok); err != nil {
		t.Fatal(err)
	}
	if st := r.status(); st.Owner == nil || st.Runner.State != groundhog.RunnerStopped {
		t.Fatalf("status %+v", st)
	}
	_, err := r.sup.Run(context.Background(), r.key)
	wantRefused(t, err, RefusedExternal)
	if r.l.count() != 0 || r.exec.count("enroll") != 0 {
		t.Errorf("%d runners, %d enrolls", r.l.count(), r.exec.count("enroll"))
	}
	if _, err := r.sup.Stop(context.Background(), r.key); err == nil {
		t.Error("Stop reached an external instance")
	}
	if st := r.status(); st.Paused {
		t.Error("the external instance was paused")
	}
}

func TestRealCrashedRunnerIsReportedNotRestarted(t *testing.T) {
	r := newRealRig(t, "", false)
	r.run()
	child := r.l.last()
	if err := syscall.Kill(child.PID(), syscall.SIGKILL); err != nil { // the test runs the crash
		t.Fatal(err)
	}
	waitFor(t, "the crash to be reported", 10*time.Second, func() bool {
		info, _ := r.sup.Runner(r.key)
		return info.Phase == PhaseExited
	})
	info, _ := r.sup.Runner(r.key)
	if info.Exit == nil || info.Exit.Signal != "killed" || !strings.Contains(info.Notice, "not restarted") {
		t.Fatalf("info %+v", info)
	}
	time.Sleep(time.Second) // a restart storm would show here
	if r.l.count() != 1 {
		t.Fatalf("%d runners started: the crash was restarted", r.l.count())
	}
	if st := r.status(); st.Owner == nil || !r.sup.IsOurs(*st.Owner) {
		t.Errorf("ownership after the crash: %+v", st.Owner)
	}
	// Quit settles the dead runner and releases.
	rep := r.sup.Quit(context.Background())
	if rep.Instances[0].Outcome != QuitReleased {
		t.Fatalf("%+v", rep.Instances[0])
	}
	if st := r.status(); st.Owner != nil {
		t.Errorf("owner %+v", st.Owner)
	}
}

// ---- The runner's output is a file ----

// TestStandIn is Easyhog going away abruptly. It runs only as the child of
// TestRealRunnerOutlivesAnEasyhogThatVanishes: it runs an instance, reports
// where things are, and exits without quitting.
func TestStandIn(t *testing.T) {
	if os.Getenv("EASYHOG_STANDIN") != "1" {
		t.Skip("helper process for TestRealRunnerOutlivesAnEasyhogThatVanishes")
	}
	key, state := os.Getenv("EASYHOG_STANDIN_INSTANCE"), os.Getenv("EASYHOG_STANDIN_STATE")
	sup, err := NewSupervisor(SupervisorConfig{Exec: runner.Process{}, Launcher: runner.Spawner{}, StateDir: state, Label: "stand-in"})
	if err != nil {
		fmt.Println("ERROR", err)
		os.Exit(2)
	}
	res, err := sup.Run(context.Background(), key)
	if err != nil {
		fmt.Println("ERROR", err)
		os.Exit(2)
	}
	info, _ := sup.Runner(key)
	fmt.Printf("READY pid=%d token=%s log=%s\n", res.PID, info.TokenFile, res.Log)
	os.Exit(0) // no Quit, no deferred cleanup: as a crash or a kill would leave it
}

func TestRealRunnerOutlivesAnEasyhogThatVanishes(t *testing.T) {
	// The consumer writes to stderr after a pause. Had the runner's output been a
	// pipe whose reader (Easyhog) is gone, that write would raise SIGPIPE and
	// kill the consumer before the marker.
	r := newRealRig(t, "sleep 2\necho consumer-wrote-to-stderr >&2\necho done > $MARKER", true)
	cmd := exec.Command(os.Args[0], "-test.run=^TestStandIn$")
	cmd.Env = append(os.Environ(), "EASYHOG_STANDIN=1", "EASYHOG_STANDIN_INSTANCE="+r.key, "EASYHOG_STANDIN_STATE="+r.state)
	out, err := cmd.StdoutPipe()
	if err != nil {
		t.Fatal(err)
	}
	if err := cmd.Start(); err != nil {
		t.Fatal(err)
	}
	var ready string
	sc := bufio.NewScanner(out)
	for sc.Scan() {
		if strings.HasPrefix(sc.Text(), "READY ") || strings.HasPrefix(sc.Text(), "ERROR") {
			ready = sc.Text()
			break
		}
	}
	_ = cmd.Wait() // the stand-in is gone
	if !strings.HasPrefix(ready, "READY ") {
		t.Fatalf("stand-in said %q", ready)
	}
	var pid int
	var token, logPath string
	for _, f := range strings.Fields(ready)[1:] {
		k, v, _ := strings.Cut(f, "=")
		switch k {
		case "pid":
			pid, _ = strconv.Atoi(v)
		case "token":
			token = v
		case "log":
			logPath = v
		}
	}
	t.Cleanup(func() { // hand the instance back through its own token
		ctx := context.Background()
		c := r.client()
		_ = c.Pause(ctx, token)
		_ = c.Stop(ctx, token)
		_ = syscall.Kill(pid, syscall.SIGTERM)
		time.Sleep(200 * time.Millisecond)
		_ = c.Release(ctx, token)
	})
	if err := syscall.Kill(pid, 0); err != nil {
		t.Fatalf("the runner died with the supervisor: %v", err)
	}
	waitFor(t, "the consumer to finish its work after Easyhog vanished", 20*time.Second, func() bool { return fileExists(r.marker()) })
	waitFor(t, "the runner to record the handoff", 10*time.Second, func() bool {
		d := r.status().Deliveries
		return len(d) == 1 && d[0].State == groundhog.DeliveryAccepted
	})
	st := r.status()
	if st.Runner.State != groundhog.RunnerRunning || len(st.Deliveries) != 1 || st.Deliveries[0].State != groundhog.DeliveryAccepted {
		t.Errorf("runner %v deliveries %+v", st.Runner.State, st.Deliveries)
	}
	data, err := os.ReadFile(logPath)
	if err != nil || !strings.Contains(string(data), "consumer-wrote-to-stderr") || !strings.Contains(string(data), "accepted") {
		t.Errorf("runner log %q (%v)", data, err)
	}
	// What the vanished session left: the token, which a later Easyhog never
	// reuses, so the instance reads as owned by someone else.
	later, err := NewSupervisor(SupervisorConfig{Exec: runner.Process{}, Launcher: &trackingLauncher{}, StateDir: mustReal(t, t.TempDir())})
	if err != nil {
		t.Fatal(err)
	}
	_, err = later.Run(context.Background(), r.key)
	wantRefused(t, err, RefusedExternal)
	if !fileExists(token) {
		t.Error("the token vanished")
	}
}
