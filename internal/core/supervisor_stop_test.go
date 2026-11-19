//go:build unix

package core

import (
	"context"
	"errors"
	"os"
	"slices"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/zealtv/easyhog/internal/runner"
)

// Stop does for one instance what Quit does for each: stop, release, delete the
// token. These tests pin that against the fake script; the real script's is
// TestRealStopReleasesAndRunEnrollsAgain. They fail on the old Pause, which set
// the instance-wide pause and left the runner alive and the instance owned.

func TestStopStopsThenReleasesAndDeletesTheToken(t *testing.T) {
	r := newSRig(t)
	key := r.inst("proj")
	r.run(key)
	child := r.l.child(key)
	info := mustRunner(t, r, key)
	owner := *ownerOf(r.gh.state(key))
	r.gh.update(key, func(s *ghInst) { s.paused = true }) // something else's pause stays as it was
	before := len(r.gh.allCalls())

	res, err := r.sup.Stop(context.Background(), key)
	if err != nil || res.Terminated || res.Why != "" || res.Instance != key {
		t.Fatalf("Stop %+v %v", res, err)
	}
	var names []string
	for _, c := range r.gh.allCalls()[before:] {
		names = append(names, strings.Fields(c)[1])
		if f := strings.Fields(c)[1]; f == "release" && !strings.Contains(c, "--leave-unresolved") {
			t.Errorf("release %q lacks --leave-unresolved", c)
		}
	}
	if got := strings.Join(names, ","); got != "stop,release" {
		t.Errorf("Stop ran %v, want Quit's stop then release and nothing else (no pause, no status)", names)
	}
	if st := r.gh.state(key); st.owner != nil || st.runner != "stopped" || !st.paused {
		t.Errorf("state %+v: want released, stopped, the pause as it was", st)
	}
	if !isDone(child) || child.termCount() != 0 {
		t.Errorf("runner done=%v sigterms=%d: stop should have been enough", isDone(child), child.termCount())
	}
	if fileExists(info.TokenFile) || len(filesUnder(t, r.state)) != 0 {
		t.Errorf("token or log left behind: %v", filesUnder(t, r.state))
	}
	if _, ok := r.sup.Runner(key); ok || len(r.sup.Runners()) != 0 || r.sup.Stopping() != 0 {
		t.Error("the session still reports an instance it let go")
	}
	if r.sup.IsOurs(owner) {
		t.Error("still claims an owner it released")
	}
	noSecret(t, "a result", res.Why)
}

func TestStopThenRunEnrollsAfresh(t *testing.T) {
	r := newSRig(t)
	key := r.inst("proj")
	first := r.run(key)
	firstOwner := r.gh.state(key).owner
	if _, err := r.sup.Stop(context.Background(), key); err != nil {
		t.Fatal(err)
	}
	second := r.run(key)
	if second.Outcome != RunStarted || !second.Enrolled || first.PID == second.PID && r.l.launchCount() != 2 {
		t.Fatalf("Run after Stop: %+v, %d launches", second, r.l.launchCount())
	}
	if r.gh.count(key, "enroll") != 2 || r.l.launchCount() != 2 {
		t.Errorf("%d enrolls, %d launches", r.gh.count(key, "enroll"), r.l.launchCount())
	}
	if now := r.gh.state(key).owner; now == nil || now.id == firstOwner.id {
		t.Errorf("owner %+v after Run, was %+v: want a new one", now, firstOwner)
	}
	if info := mustRunner(t, r, key); info.Phase != PhaseRunning || info.Adopted {
		t.Errorf("info %+v", info)
	}
	if rep := r.sup.Quit(context.Background()); !rep.Clean() || rep.Retained != "" {
		t.Errorf("Quit after Stop and Run: %+v", rep)
	}
}

func TestQuitAfterAStopHasNothingLeftToWindDown(t *testing.T) {
	r := newSRig(t)
	key := r.inst("proj")
	r.run(key)
	if _, err := r.sup.Stop(context.Background(), key); err != nil {
		t.Fatal(err)
	}
	before := len(r.gh.allCalls())
	rep := r.sup.Quit(context.Background())
	if len(rep.Instances) != 1 || rep.Instances[0].Outcome != QuitNotOwned || !rep.Clean() || rep.Retained != "" || len(rep.Notices) != 0 {
		t.Fatalf("report %+v", rep)
	}
	if calls := r.gh.allCalls()[before:]; len(calls) != 0 {
		t.Errorf("Quit ran %v against an instance Stop had released", calls)
	}
	if files := filesUnder(t, r.state); len(files) != 0 {
		t.Errorf("files left: %v", files)
	}
	if _, err := r.sup.Stop(context.Background(), key); err == nil {
		t.Error("Stop worked after Quit")
	} else {
		wantRefused(t, err, RefusedClosing)
	}
}

// A second Stop of the same instance finds nothing to stop, and releases nothing twice.
func TestStopTwiceReleasesOnce(t *testing.T) {
	r := newSRig(t)
	key := r.inst("proj")
	r.run(key)
	var wg sync.WaitGroup
	errs := make([]error, 2)
	for i := range errs {
		wg.Add(1)
		go func() { defer wg.Done(); _, errs[i] = r.sup.Stop(context.Background(), key) }()
	}
	wg.Wait()
	ok := 0
	for _, e := range errs {
		if e == nil {
			ok++
		} else {
			wantRefused(t, e, RefusedNotOurs)
		}
	}
	if ok != 1 || r.gh.count(key, "release") != 1 || r.gh.count(key, "stop") != 1 {
		t.Errorf("%d went through, %d stops, %d releases", ok, r.gh.count(key, "stop"), r.gh.count(key, "release"))
	}
}

func TestStopWaitsForAConsumerCallThatFinishes(t *testing.T) {
	r := newSRig(t, func(c *SupervisorConfig) { c.QuitWait = 2 * time.Second })
	key := r.inst("proj")
	r.run(key)
	r.gh.update(key, func(s *ghInst) { s.inflight = true })
	go func() {
		time.Sleep(120 * time.Millisecond)
		r.gh.update(key, func(s *ghInst) { s.inflight = false })
	}()
	res, err := r.sup.Stop(context.Background(), key)
	if err != nil || res.Terminated || res.Waited < 100*time.Millisecond {
		t.Fatalf("Stop %+v %v", res, err)
	}
	if r.l.child(key).termCount() != 0 {
		t.Error("SIGTERM sent although the call finished within the wait")
	}
	if st := r.gh.state(key); st.owner != nil || st.uncertain {
		t.Errorf("state %+v", st)
	}
}

// Past QuitWait Stop does what Quit does: SIGTERM, then release leaving the
// detached delivery uncertain.
func TestStopTerminatesARunnerWhoseCallOutlastsTheWait(t *testing.T) {
	r := newSRig(t, func(c *SupervisorConfig) { c.QuitWait = 150 * time.Millisecond })
	key := r.inst("proj")
	r.run(key)
	r.gh.update(key, func(s *ghInst) { s.inflight = true })
	res, err := r.sup.Stop(context.Background(), key)
	if err != nil || !res.Terminated || res.Waited < 140*time.Millisecond || res.Why == "" {
		t.Fatalf("Stop %+v %v", res, err)
	}
	if n := r.l.child(key).termCount(); n != 1 || !isDone(r.l.child(key)) {
		t.Errorf("%d SIGTERMs, runner done %v", n, isDone(r.l.child(key)))
	}
	if st := r.gh.state(key); st.owner != nil || !st.uncertain || st.runner != "stopped" {
		t.Errorf("state %+v: want released with the delivery uncertain", st)
	}
	if strings.Contains(res.Why, key) {
		t.Errorf("the result names the absolute path: %q", res.Why)
	}
}

// A runner that ignores SIGTERM stays alive and the instance stays owned. Unlike
// Quit's, the session lives on: it keeps the token and can try again.
func TestStopThatCannotEndTheRunnerLeavesTheSessionHoldingItAndCanBeRetried(t *testing.T) {
	r := newSRig(t)
	key := r.inst("proj")
	r.run(key)
	child := r.l.child(key)
	token := mustRunner(t, r, key).TokenFile
	owner := *ownerOf(r.gh.state(key))
	r.l.mu.Lock()
	r.l.ignoreTerm = true
	r.l.mu.Unlock()
	r.gh.hook = func(_ context.Context, _, cmd string, _ int) (bool, string, error) {
		return cmd == "stop", "", exitStatus(6)
	}
	_, err := r.sup.Stop(context.Background(), key)
	var se *StopError
	if !errors.As(err, &se) || se.RunnerGone || !strings.Contains(se.Why, "left running") {
		t.Fatalf("err %v", err)
	}
	if r.gh.count(key, "release") != 0 {
		t.Error("released an instance whose runner is still alive")
	}
	info := mustRunner(t, r, key)
	if info.Phase != PhaseRunning || info.TokenFile != token || info.Notice != "" || !fileExists(token) {
		t.Fatalf("info %+v: the session should still hold the instance, its runner running", info)
	}
	if !r.sup.IsOurs(owner) {
		t.Error("the session no longer claims an instance it still holds")
	}

	// The runner ends; Stop again finishes the job.
	r.clearHook()
	r.l.mu.Lock()
	r.l.ignoreTerm = false
	r.l.mu.Unlock()
	child.end(runner.Exit{})
	if _, err := r.sup.Stop(context.Background(), key); err != nil {
		t.Fatalf("second Stop: %v", err)
	}
	if st := r.gh.state(key); st.owner != nil {
		t.Errorf("state %+v", st)
	}
	if fileExists(token) {
		t.Error("the token stays")
	}
}

// A script without release-unresolved-v1 refuses a plain release while a handoff
// is unresolved. Stop reports that, keeps the token, and a Recover then a second
// Stop finish it.
func TestStopRefusedByAnUnresolvedHandoffOnAnOldScriptKeepsTheTokenForRecover(t *testing.T) {
	r := newSRig(t, func(c *SupervisorConfig) { c.QuitWait = 2 * time.Second })
	key := r.inst("proj")
	r.gh.update(key, func(s *ghInst) { s.caps = controlCaps(); s.oldStop = true })
	r.run(key)
	token := mustRunner(t, r, key).TokenFile
	r.gh.update(key, func(s *ghInst) { s.inflight = true })
	_, err := r.sup.Stop(context.Background(), key)
	var se *StopError
	if !errors.As(err, &se) || !se.RunnerGone {
		t.Fatalf("err %v", err)
	}
	for _, want := range []string{"unresolved handoff", "release-unresolved-v1"} {
		if !strings.Contains(se.Why, want) {
			t.Errorf("Why lacks %q: %q", want, se.Why)
		}
	}
	if strings.Contains(se.Error(), token) || strings.Contains(se.Why, "Reopen") {
		t.Errorf("a Quit-only sentence or the token path reached Stop's error: %q", se.Error())
	}
	info := mustRunner(t, r, key)
	if info.Phase != PhaseEnrolled || info.TokenFile != token || !fileExists(token) {
		t.Fatalf("info %+v: the session should keep holding it, runner stopped", info)
	}
	if st := r.gh.state(key); st.owner == nil || !st.uncertain {
		t.Fatalf("state %+v", st)
	}
	// Recover settles it (through the supervisor, as the palette does); then Stop releases.
	if _, err := r.settle(key, SettleRequest{Action: ActionRecoverAccept}); err != nil {
		t.Fatalf("Recover: %v", err)
	}
	if _, err := r.sup.Stop(context.Background(), key); err != nil {
		t.Fatalf("Stop after Recover: %v", err)
	}
	if st := r.gh.state(key); st.owner != nil {
		t.Errorf("state %+v", st)
	}
}

// A token the script no longer recognises controls nothing: Stop deletes it and
// says so, and refuses; it never calls release.
func TestStopOfATokenThatControlsNothingRemovesItAndRefuses(t *testing.T) {
	r := newSRig(t)
	key := r.inst("proj")
	r.run(key)
	token := mustRunner(t, r, key).TokenFile
	r.gh.update(key, func(s *ghInst) { s.owner = &ghOwner{id: "o-other", gen: 9, label: "someone-else"} })
	_, err := r.sup.Stop(context.Background(), key)
	re := wantRefused(t, err, RefusedNotOurs)
	if !strings.Contains(re.Detail, "controlled nothing") || strings.Contains(re.Detail, key) {
		t.Errorf("detail %q", re.Detail)
	}
	if r.gh.count(key, "release") != 0 || fileExists(token) {
		t.Errorf("%d releases, token present %v", r.gh.count(key, "release"), fileExists(token))
	}
	if st := r.gh.state(key); st.owner == nil || st.owner.id != "o-other" {
		t.Errorf("the other owner was touched: %+v", st)
	}
	if _, ok := r.sup.Runner(key); ok {
		t.Error("the session still holds the instance")
	}
}

// An adopted instance's runner is not this session's child: a good stop ends
// it, and Stop releases; a failed stop leaves nothing to SIGTERM and the
// session keeps the token, still reading as adopted.
func TestStopOfAnAdoptedInstance(t *testing.T) {
	t.Run("releases", func(t *testing.T) {
		r := newSRig(t)
		key := r.inst("proj")
		r.run(key)
		child := r.l.child(key)
		killSession(t, r.sup)
		b := r.another()
		b.Adopt(everywhere)
		res, err := b.Stop(context.Background(), key)
		if err != nil || res.Terminated || child.termCount() != 0 {
			t.Fatalf("Stop %+v %v, sigterms %d", res, err, child.termCount())
		}
		if st := r.gh.state(key); st.owner != nil || st.runner != "stopped" {
			t.Errorf("state %+v", st)
		}
		if _, ok := b.Runner(key); ok {
			t.Error("still held")
		}
		if files := filesUnder(t, r.state); len(files) != 0 {
			t.Errorf("files left: %v", files)
		}
	})
	t.Run("a failed stop leaves it held and adopted", func(t *testing.T) {
		r := newSRig(t)
		key := r.inst("proj")
		r.run(key)
		child := r.l.child(key)
		killSession(t, r.sup)
		b := r.another()
		b.Adopt(everywhere)
		r.gh.hook = func(_ context.Context, _, cmd string, _ int) (bool, string, error) {
			return cmd == "stop", "", exitStatus(6)
		}
		_, err := b.Stop(context.Background(), key)
		var se *StopError
		if !errors.As(err, &se) || se.RunnerGone || !strings.Contains(se.Why, "earlier session") {
			t.Fatalf("err %v", err)
		}
		if child.termCount() != 0 {
			t.Error("the adopted runner was sent SIGTERM")
		}
		info, _ := b.Runner(key)
		if info.Phase != PhaseEnrolled || !info.Adopted || !fileExists(info.TokenFile) {
			t.Errorf("info %+v", info)
		}
		r.clearHook()
		if _, err := b.Stop(context.Background(), key); err != nil {
			t.Fatalf("Stop again: %v", err)
		}
	})
}

// A Quit that begins while a Stop waits on a consumer call lets Stop finish its
// polite wait instead of cutting it into an immediate SIGTERM; Quit then finds
// the instance released.
func TestQuitBeginningDuringAStopWaitsForItAndDoesNotTerminate(t *testing.T) {
	r := newSRig(t, func(c *SupervisorConfig) { c.QuitWait = 3 * time.Second })
	key := r.inst("proj")
	r.run(key)
	r.gh.update(key, func(s *ghInst) { s.inflight = true })
	stopped := make(chan error, 1)
	var res StopResult
	go func() {
		var err error
		res, err = r.sup.Stop(context.Background(), key)
		stopped <- err
	}()
	eventuallyTrue(t, "the stop to be waiting on the call", func() bool { return r.gh.state(key).runner == "stopping" })
	quitDone := make(chan QuitReport, 1)
	go func() { quitDone <- r.sup.Quit(context.Background()) }()
	time.Sleep(100 * time.Millisecond)
	select {
	case <-quitDone:
		t.Fatal("Quit returned while the instance's Stop still waited")
	default:
	}
	r.gh.update(key, func(s *ghInst) { s.inflight = false }) // the call is recorded accepted
	if err := <-stopped; err != nil || res.Terminated {
		t.Fatalf("Stop %+v %v", res, err)
	}
	rep := <-quitDone
	if len(rep.Instances) != 1 || rep.Instances[0].Outcome != QuitNotOwned || !rep.Clean() || rep.Retained != "" {
		t.Fatalf("report %+v", rep)
	}
	if r.l.child(key).termCount() != 0 || r.gh.count(key, "release") != 1 {
		t.Errorf("%d SIGTERMs, %d releases", r.l.child(key).termCount(), r.gh.count(key, "release"))
	}
}

// A Stop that failed leaves the instance held, so a later Quit winds it down.
func TestQuitAfterAStopThatFailedWindsTheInstanceDown(t *testing.T) {
	r := newSRig(t)
	key := r.inst("proj")
	r.run(key)
	child := r.l.child(key)
	r.l.mu.Lock()
	r.l.ignoreTerm = true
	r.l.mu.Unlock()
	r.gh.hook = func(_ context.Context, _, cmd string, _ int) (bool, string, error) {
		return cmd == "stop", "", exitStatus(6)
	}
	if _, err := r.sup.Stop(context.Background(), key); err == nil {
		t.Fatal("Stop succeeded")
	}
	r.clearHook()
	r.l.mu.Lock()
	r.l.ignoreTerm = false
	r.l.mu.Unlock()
	child.end(runner.Exit{})
	rep := r.sup.Quit(context.Background())
	if len(rep.Instances) != 1 || rep.Instances[0].Outcome != QuitReleased || !rep.Clean() {
		t.Fatalf("report %+v", rep)
	}
	if st := r.gh.state(key); st.owner != nil {
		t.Errorf("state %+v", st)
	}
}

// Settle stops this session's runner and starts it again; it never releases, so
// the session keeps its token. It must not be routed through Stop.
func TestSettleKeepsOwnershipWhereStopReleases(t *testing.T) {
	r := newSRig(t)
	key := r.inst("proj")
	r.run(key)
	token := mustRunner(t, r, key).TokenFile
	owner := r.gh.state(key).owner
	r.withDeliveries(key, &fakeDel{id: "d-a", state: "failed"})
	res, err := r.settle(key, SettleRequest{Action: ActionRetryFailed})
	if err != nil || !res.StoppedRunner || !res.Restarted {
		t.Fatalf("%+v %v", res, err)
	}
	if n := r.gh.count(key, "release"); n != 0 {
		t.Errorf("Settle released the instance (%d release calls)", n)
	}
	if st := r.gh.state(key); st.owner == nil || st.owner.id != owner.id || st.owner.gen != owner.gen {
		t.Errorf("owner %+v, was %+v", st.owner, owner)
	}
	if info := mustRunner(t, r, key); info.Phase != PhaseRunning || info.TokenFile != token || !fileExists(token) {
		t.Errorf("info %+v", info)
	}
	if r.gh.count(key, "enroll") != 1 {
		t.Errorf("%d enrolls", r.gh.count(key, "enroll"))
	}
	if _, err := os.Stat(token); err != nil {
		t.Error(err)
	}
}

// offersFor is what the palette would offer for the instance now: Offers on a
// fresh status read and what the supervisor holds, as checkOfferedAction builds it.
func (r *srig) offersFor(key string) []Action {
	r.t.Helper()
	c, err := r.sup.cfg.Clients.For(key)
	if err != nil {
		r.t.Fatal(err)
	}
	st, err := c.Status(context.Background())
	if err != nil {
		r.t.Fatal(err)
	}
	var held *RunnerInfo
	if info, ok := r.sup.Runner(key); ok {
		held = &info
	}
	ours := st.Owner != nil && r.sup.IsOurs(*st.Owner)
	iv := InstanceView{Class: classifyRead(st, nil, ours), Health: HealthOK, Inspection: Inspection{Last: &Observation{Status: st}}}
	got, _ := OffersOn(iv, held, "")
	return got
}

// A Stop that ended the runner and could not release says it can be tried again:
// the palette must offer Stop then, beside Run, and after Recover too (the notice
// promised it). Offering Stop only for a running runner left Quit the only way out.
func TestStopIsOfferedAfterAStopThatEndedTheRunnerButCouldNotRelease(t *testing.T) {
	r := newSRig(t, func(c *SupervisorConfig) { c.QuitWait = 2 * time.Second })
	key := r.inst("proj")
	r.gh.update(key, func(s *ghInst) { s.caps = controlCaps(); s.oldStop = true })
	r.run(key)
	if got := r.offersFor(key); !slices.Equal(got, []Action{ActionStop}) {
		t.Fatalf("running: offers %v", got)
	}
	r.gh.update(key, func(s *ghInst) { s.inflight = true })
	if _, err := r.sup.Stop(context.Background(), key); err == nil {
		t.Fatal("Stop released a handoff the old script says is unresolved")
	}
	if got := r.offersFor(key); !slices.Contains(got, ActionStop) || !slices.Contains(got, ActionRun) {
		t.Fatalf("after a Stop that stopped short: offers %v, want Run and Stop", got)
	}
	if _, err := r.settle(key, SettleRequest{Action: ActionRecoverAccept}); err != nil {
		t.Fatal(err)
	}
	got := r.offersFor(key)
	if !slices.Contains(got, ActionStop) {
		t.Fatalf("after Recover: offers %v, want Stop", got)
	}
	if _, err := r.sup.Stop(context.Background(), key); err != nil {
		t.Fatalf("Stop as the palette offered it: %v", err)
	}
	if st := r.gh.state(key); st.owner != nil {
		t.Errorf("state %+v", st)
	}
}

// A runner this session saw end on its own leaves the instance held; Stop lets it go.
func TestStopReleasesAnInstanceWhoseRunnerEndedOnItsOwn(t *testing.T) {
	r := newSRig(t)
	key := r.inst("proj")
	r.run(key)
	r.l.child(key).crash(runner.Exit{Code: 6})
	eventuallyTrue(t, "the crash", func() bool { info, _ := r.sup.Runner(key); return info.Phase == PhaseExited })
	if got := r.offersFor(key); !slices.Equal(got, []Action{ActionRun, ActionStop}) {
		t.Fatalf("offers %v", got)
	}
	if _, err := r.sup.Stop(context.Background(), key); err != nil {
		t.Fatal(err)
	}
	if st := r.gh.state(key); st.owner != nil || len(filesUnder(t, r.state)) != 0 {
		t.Errorf("state %+v, files %v", st, filesUnder(t, r.state))
	}
}

// A hurried Quit (a second ctrl+c) while a Stop still waits on a consumer call:
// Quit SIGTERMs the runner, which lets the Stop's release run, and then waits for
// it in real time and finds the instance released. It must not report "left owned"
// and skip the release of an instance that then gets released behind its back.
func TestHurriedQuitDuringAStopWaitsForItsReleaseAndReportsTheTruth(t *testing.T) {
	r := newSRig(t, func(c *SupervisorConfig) { c.QuitWait = time.Minute })
	key := r.inst("proj")
	r.run(key)
	child := r.l.child(key)
	r.gh.update(key, func(s *ghInst) { s.inflight = true })
	stopped := make(chan error, 1)
	go func() { _, err := r.sup.Stop(context.Background(), key); stopped <- err }()
	eventuallyTrue(t, "the stop to wait on the call", func() bool { return r.gh.state(key).runner == "stopping" })
	ctx, cancel := context.WithCancel(context.Background())
	quitDone := make(chan QuitReport, 1)
	go func() { quitDone <- r.sup.Quit(ctx) }()
	time.Sleep(80 * time.Millisecond) // Quit has begun and waits on the slot
	cancel()                          // a second interrupt
	var rep QuitReport
	select {
	case rep = <-quitDone:
	case <-time.After(10 * time.Second):
		t.Fatal("a hurried Quit did not finish")
	}
	if err := <-stopped; err != nil {
		t.Fatalf("Stop: %v", err)
	}
	if len(rep.Instances) != 1 || rep.Instances[0].Outcome == QuitLeftOwned || !rep.Clean() || rep.Retained != "" {
		t.Fatalf("report %+v: the instance was released, so it cannot be reported left owned", rep)
	}
	if st := r.gh.state(key); st.owner != nil || !st.uncertain {
		t.Errorf("state %+v: want released, the detached call uncertain", st)
	}
	if child.termCount() != 1 || r.gh.count(key, "release") != 1 {
		t.Errorf("%d SIGTERMs, %d releases", child.termCount(), r.gh.count(key, "release"))
	}
	if files := filesUnder(t, r.state); len(files) != 0 {
		t.Errorf("files left: %v", files)
	}
}
