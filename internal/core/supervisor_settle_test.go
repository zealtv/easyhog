//go:build unix

package core

import (
	"context"
	"errors"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/zealtv/easyhog/internal/groundhog"
	"github.com/zealtv/easyhog/internal/runner"
)

func (r *srig) withDeliveries(key string, dels ...*fakeDel) {
	r.gh.update(key, func(s *ghInst) { s.dels = dels })
}

func (r *srig) settle(key string, req SettleRequest) (SettleResult, error) {
	return r.sup.Settle(context.Background(), key, req)
}

// admitACallBeforeStop makes a consumer call start between Settle's read and its
// stop, which is the one way a stop can now wait: a read that lists an in-flight
// delivery is refused before any stop.
func (r *srig) admitACallBeforeStop(key string) {
	r.gh.hook = func(_ context.Context, _, cmd string, _ int) (bool, string, error) {
		if cmd == "stop" {
			r.gh.update(key, func(s *ghInst) { s.inflight = true })
		}
		return false, "", nil
	}
}

func delState(r *srig, key, id string) string {
	r.gh.mu.Lock()
	defer r.gh.mu.Unlock()
	for _, d := range r.gh.insts[key].dels {
		if d.id == id {
			return d.state
		}
	}
	return ""
}

// after returns the commands run against key after the first n calls.
func after(cmds []string, first string) []string {
	if i := slices.Index(cmds, first); i >= 0 {
		return cmds[i:]
	}
	return nil
}

func TestSettleRetryStopsTheRunnerRetriesEveryFailedDeliveryAndStartsItAgain(t *testing.T) {
	r := newSRig(t)
	key := r.inst("proj")
	r.run(key)
	r.withDeliveries(key,
		&fakeDel{id: "d-a", state: "failed", item: "schedule/daily/a"},
		&fakeDel{id: "d-b", state: "failed", item: "schedule/daily/b"},
		&fakeDel{id: "d-c", state: "accepted"},
		&fakeDel{id: "d-u", state: "uncertain"})
	first := r.l.child(key)
	res, err := r.settle(key, SettleRequest{Action: ActionRetryFailed})
	if err != nil {
		t.Fatal(err)
	}
	if !slices.Equal(res.Settled, []string{"d-a", "d-b"}) || !res.StoppedRunner || !res.Restarted || res.PID == 0 || !res.Seen {
		t.Fatalf("result %+v", res)
	}
	cmds := after(r.gh.commands(key), "stop")
	if len(cmds) < 4 || strings.Join(cmds[:3], ",") != "stop,retry,retry" {
		t.Fatalf("calls after Run: %v", cmds)
	}
	if delState(r, key, "d-a") != "pending" || delState(r, key, "d-b") != "pending" {
		t.Error("the failed deliveries are not pending")
	}
	if delState(r, key, "d-u") != "uncertain" || delState(r, key, "d-c") != "accepted" {
		t.Error("a delivery Retry failed does not settle was changed")
	}
	if r.l.launchCount() != 2 || r.l.child(key) == first || r.gh.state(key).runner != "running" {
		t.Errorf("the runner was not started again: %d launches", r.l.launchCount())
	}
	if first.termCount() != 0 {
		t.Error("the runner was sent SIGTERM")
	}
	if info, _ := r.sup.Runner(key); info.Phase != PhaseRunning || info.PID != res.PID {
		t.Errorf("session %+v", info)
	}
}

func TestSettleOnAnItemTouchesOnlyThatItemsDeliveries(t *testing.T) {
	r := newSRig(t)
	key := r.inst("proj")
	r.run(key)
	r.withDeliveries(key,
		&fakeDel{id: "d-a", state: "failed", item: "schedule/daily/a"},
		&fakeDel{id: "d-b", state: "failed", item: "schedule/daily/b"})
	res, err := r.settle(key, SettleRequest{Action: ActionRetryFailed, Item: "schedule/daily/b"})
	if err != nil || !slices.Equal(res.Settled, []string{"d-b"}) {
		t.Fatalf("%+v %v", res, err)
	}
	if delState(r, key, "d-a") != "failed" || delState(r, key, "d-b") != "pending" {
		t.Error("the wrong deliveries were settled")
	}
	// Nothing failed on the item any more: refused, and the runner left alone.
	launches := r.l.launchCount()
	_, err = r.settle(key, SettleRequest{Action: ActionRetryFailed, Item: "schedule/daily/b"})
	wantRefused(t, err, RefusedNotOffered)
	if r.l.launchCount() != launches || r.gh.count(key, "stop") != 1 {
		t.Error("a refused settle stopped or started a runner")
	}
}

func TestSettleRecoverDecisionsAndTheDeliveryLeftByATerminatedRunner(t *testing.T) {
	for _, tc := range []struct {
		action   Action
		decision string
		state    string
	}{{ActionRecoverAccept, "accept", "accepted"}, {ActionRecoverRetry, "retry", "pending"}} {
		t.Run(string(tc.action), func(t *testing.T) {
			r := newSRig(t)
			key := r.inst("proj")
			r.run(key)
			r.withDeliveries(key, &fakeDel{id: "d-u", state: "uncertain", item: "schedule/daily/a"}, &fakeDel{id: "d-f", state: "failed"})
			res, err := r.settle(key, SettleRequest{Action: tc.action})
			if err != nil || !slices.Equal(res.Settled, []string{"d-u"}) {
				t.Fatalf("%+v %v", res, err)
			}
			if delState(r, key, "d-u") != tc.state || delState(r, key, "d-f") != "failed" {
				t.Errorf("states %q %q", delState(r, key, "d-u"), delState(r, key, "d-f"))
			}
			r.gh.mu.Lock()
			got := r.gh.insts[key].dels[0].decision
			r.gh.mu.Unlock()
			if got != tc.decision {
				t.Errorf("decision %q, want %q", got, tc.decision)
			}
		})
	}
}

// After a Quit that timed out, the instance is unowned with an uncertain
// delivery (the barrier up). Nothing is offered until Run enrolls it; then
// Recover stops the runner Run started, settles, and starts it again.
func TestSettleAfterAQuitThatTimedOutNeedsRunFirst(t *testing.T) {
	r := newSRig(t)
	key := r.inst("proj")
	r.gh.update(key, func(s *ghInst) { s.uncertain = true }) // what the earlier Quit left, unowned
	_, err := r.settle(key, SettleRequest{Action: ActionRecoverAccept})
	wantRefused(t, err, RefusedNotOurs)
	if len(r.gh.commands(key)) != 0 {
		t.Errorf("an unowned instance was touched: %v", r.gh.commands(key))
	}
	r.run(key) // enroll and run work with the barrier up
	res, err := r.settle(key, SettleRequest{Action: ActionRecoverAccept})
	if err != nil || !slices.Equal(res.Settled, []string{"d-1"}) || !res.Restarted {
		t.Fatalf("%+v %v", res, err)
	}
	if r.gh.state(key).uncertain {
		t.Error("the delivery is still uncertain")
	}
}

func TestSettleLeavesAPausedInstancePausedAndAnEndedRunnerEnded(t *testing.T) {
	t.Run("paused restarts paused", func(t *testing.T) {
		r := newSRig(t)
		key := r.inst("proj")
		r.run(key)
		r.gh.update(key, func(s *ghInst) { s.paused = true }) // set by something else
		r.withDeliveries(key, &fakeDel{id: "d-a", state: "failed"})
		res, err := r.settle(key, SettleRequest{Action: ActionRetryFailed})
		if err != nil || !res.Restarted || !res.Paused {
			t.Fatalf("%+v %v", res, err)
		}
		if !r.gh.state(key).paused || r.gh.count(key, "resume") != 0 {
			t.Error("the pause was touched")
		}
	})
	t.Run("a runner that had ended is not started", func(t *testing.T) {
		r := newSRig(t)
		key := r.inst("proj")
		r.run(key)
		r.l.child(key).crash(runner.Exit{Code: 1})
		eventuallyTrue(t, "the session to see the runner end", func() bool {
			info, _ := r.sup.Runner(key)
			return info.Phase == PhaseExited
		})
		r.withDeliveries(key, &fakeDel{id: "d-a", state: "failed"})
		res, err := r.settle(key, SettleRequest{Action: ActionRetryFailed})
		if err != nil || res.StoppedRunner || res.Restarted {
			t.Fatalf("%+v %v", res, err)
		}
		if r.gh.count(key, "stop") != 0 || r.l.launchCount() != 1 {
			t.Errorf("a stopped runner was stopped or started: %d stops, %d launches", r.gh.count(key, "stop"), r.l.launchCount())
		}
		if delState(r, key, "d-a") != "pending" {
			t.Error("not settled")
		}
		if info, _ := r.sup.Runner(key); info.Phase != PhaseExited {
			t.Errorf("phase %v", info.Phase)
		}
	})
}

func TestSettleThatCannotStopTheRunnerInTimeSignalsNothingAndSettlesNothing(t *testing.T) {
	r := newSRig(t, func(c *SupervisorConfig) { c.QuitWait = 60 * time.Millisecond })
	key := r.inst("proj")
	r.run(key)
	r.withDeliveries(key, &fakeDel{id: "d-a", state: "failed"})
	r.admitACallBeforeStop(key) // a consumer call the runner admits after Settle's read
	child := r.l.child(key)
	res, err := r.settle(key, SettleRequest{Action: ActionRetryFailed})
	var se *SettleError
	if !errors.As(err, &se) || se.Stage != SettleStopping || se.Done != 0 || len(res.Settled) != 0 || res.StoppedRunner {
		t.Fatalf("%+v %v", res, err)
	}
	if !strings.Contains(err.Error(), "nothing was settled") || !strings.Contains(err.Error(), "was not signalled") {
		t.Errorf("text %q", err)
	}
	if child.termCount() != 0 || isDone(child) {
		t.Error("the runner was signalled or ended")
	}
	if st := r.gh.state(key).runner; st != "stopping" {
		t.Errorf("the script shows the runner %q, want \"stopping\": the stop request stays recorded", st)
	}
	if r.gh.count(key, "retry") != 0 || delState(r, key, "d-a") != "failed" {
		t.Error("a delivery was settled with a runner alive")
	}
	// The call ends, the runner (asked to stop) exits, and Settle then works
	// without stopping or starting anything.
	r.gh.update(key, func(s *ghInst) { s.inflight = false })
	r.l.endChild(key, runner.Exit{})
	r.gh.update(key, func(s *ghInst) { s.runner = "stopped" })
	eventuallyTrue(t, "the session to see the runner end", func() bool {
		info, _ := r.sup.Runner(key)
		return info.Phase == PhaseEnrolled
	})
	if info, _ := r.sup.Runner(key); info.Notice != "" {
		t.Errorf("a runner that was asked to stop was reported as a crash: %q", info.Notice)
	}
	res, err = r.settle(key, SettleRequest{Action: ActionRetryFailed})
	if err != nil || res.StoppedRunner || res.Restarted || !slices.Equal(res.Settled, []string{"d-a"}) {
		t.Fatalf("%+v %v", res, err)
	}
}

func TestSettleFailureMidwayStillStartsTheRunnerAgain(t *testing.T) {
	r := newSRig(t)
	key := r.inst("proj")
	r.run(key)
	r.withDeliveries(key, &fakeDel{id: "d-a", state: "failed"}, &fakeDel{id: "d-b", state: "failed"}, &fakeDel{id: "d-c", state: "failed"})
	r.gh.hook = func(_ context.Context, _, cmd string, n int) (bool, string, error) {
		if cmd == "retry" && n == 2 {
			return true, "", exitStatus(6)
		}
		return false, "", nil
	}
	res, err := r.settle(key, SettleRequest{Action: ActionRetryFailed})
	var se *SettleError
	if !errors.As(err, &se) || se.Stage != SettleSettling || se.Done != 1 || se.Total != 3 || !slices.Equal(res.Settled, []string{"d-a"}) {
		t.Fatalf("%+v %v", res, err)
	}
	var ee *groundhog.ExitError
	if !errors.As(err, &ee) || ee.Kind != groundhog.KindIO {
		t.Errorf("the cause is not kept: %v", err)
	}
	if !res.Restarted || !strings.Contains(err.Error(), "stopped and restarted") || r.gh.state(key).runner != "running" {
		t.Errorf("the runner was not started again: %v", err)
	}
	if r.gh.count(key, "retry") != 2 {
		t.Errorf("%d retries; it should stop at the first failure", r.gh.count(key, "retry"))
	}
}

func TestSettleReportsARunnerThatWillNotStartAgain(t *testing.T) {
	r := newSRig(t)
	key := r.inst("proj")
	r.run(key)
	r.withDeliveries(key, &fakeDel{id: "d-a", state: "failed"})
	r.l.mu.Lock()
	r.l.exitAtStart = &runner.Exit{Code: 2}
	r.l.mu.Unlock()
	res, err := r.settle(key, SettleRequest{Action: ActionRetryFailed})
	var se *SettleError
	if !errors.As(err, &se) || se.Stage != SettleRestarting || se.Done != 1 || res.Restarted || !slices.Equal(res.Settled, []string{"d-a"}) {
		t.Fatalf("%+v %v", res, err)
	}
	if !strings.Contains(err.Error(), "not restarted") {
		t.Errorf("text %q", err)
	}
}

func TestSettleRefusals(t *testing.T) {
	t.Run("external owner", func(t *testing.T) {
		r := newSRig(t)
		key := r.inst("proj")
		r.gh.update(key, func(s *ghInst) {
			s.owner = &ghOwner{id: "o-other", gen: 1, label: "someone else"}
			s.dels = []*fakeDel{{id: "d-a", state: "failed"}}
		})
		_, err := r.settle(key, SettleRequest{Action: ActionRetryFailed})
		wantRefused(t, err, RefusedNotOurs)
		if cmds := r.gh.commands(key); slices.Contains(cmds, "stop") || slices.Contains(cmds, "retry") {
			t.Errorf("another owner's instance was touched: %v", cmds)
		}
	})
	t.Run("not a settling action", func(t *testing.T) {
		r := newSRig(t)
		key := r.inst("proj")
		r.run(key)
		_, err := r.settle(key, SettleRequest{Action: ActionRun})
		wantRefused(t, err, RefusedNotOffered)
	})
	t.Run("nothing matches", func(t *testing.T) {
		r := newSRig(t)
		key := r.inst("proj")
		r.run(key)
		r.withDeliveries(key, &fakeDel{id: "d-a", state: "failed"})
		_, err := r.settle(key, SettleRequest{Action: ActionRecoverAccept})
		wantRefused(t, err, RefusedNotOffered)
		if r.gh.count(key, "stop") != 0 {
			t.Error("the runner was stopped for nothing")
		}
	})
	// The refusal also applies the ownership rule: a token that controls nothing
	// is removed and the session holds nothing, as after any other read that shows it.
	for _, c := range []struct {
		name   string
		owner  *ghOwner
		reason Refusal
	}{
		{"a token that no longer matches", &ghOwner{id: "o-other", gen: 9, label: "other"}, RefusedExternal},
		{"no owner recorded any more", nil, RefusedNotOurs},
	} {
		t.Run(c.name, func(t *testing.T) {
			r := newSRig(t)
			key := r.inst("proj")
			r.run(key)
			was := mustRunner(t, r, key).TokenFile
			r.gh.update(key, func(s *ghInst) {
				s.owner = c.owner
				s.dels = []*fakeDel{{id: "d-a", state: "failed"}}
			})
			_, err := r.settle(key, SettleRequest{Action: ActionRetryFailed})
			wantRefused(t, err, c.reason)
			if r.gh.count(key, "stop") != 0 {
				t.Error("stopped an instance this session does not control")
			}
			if _, held := r.sup.Runner(key); held || fileExists(was) {
				t.Errorf("the token that controls nothing stayed: held %v, file %v", held, fileExists(was))
			}
		})
	}
	t.Run("after quit began", func(t *testing.T) {
		r := newSRig(t)
		key := r.inst("proj")
		r.run(key)
		r.withDeliveries(key, &fakeDel{id: "d-a", state: "failed"})
		r.sup.Quit(context.Background())
		_, err := r.settle(key, SettleRequest{Action: ActionRetryFailed})
		wantRefused(t, err, RefusedClosing)
	})
}

// Settle is one more operation on the instance: it holds the slot, so a Run or a
// second Settle on the same instance waits for it, and Quit waits too.
func TestSettleOneAtATimePerInstance(t *testing.T) {
	r := newSRig(t, func(c *SupervisorConfig) { c.QuitWait = 2 * time.Second })
	key := r.inst("proj")
	r.run(key)
	r.withDeliveries(key, &fakeDel{id: "d-a", state: "failed"}, &fakeDel{id: "d-b", state: "failed"})
	r.admitACallBeforeStop(key)
	done := make(chan error, 1)
	go func() { _, err := r.settle(key, SettleRequest{Action: ActionRetryFailed}); done <- err }()
	eventuallyTrue(t, "the settle to be waiting on stop", func() bool { return r.gh.count(key, "stop") == 1 })
	ctx, cancel := context.WithTimeout(context.Background(), 50*time.Millisecond)
	defer cancel()
	if _, err := r.sup.Settle(ctx, key, SettleRequest{Action: ActionRetryFailed}); !errors.Is(err, context.DeadlineExceeded) {
		t.Errorf("a second settle on the instance did not wait its turn: %v", err)
	}
	r.gh.update(key, func(s *ghInst) { s.inflight = false })
	if err := <-done; err != nil {
		t.Fatal(err)
	}
}

// Retry failed while the runner sits in a consumer call
// used to ask `stop`, wait out QuitWait and leave the stop requested, so the
// runner exited when the call ended and nothing started it again. Now the fresh
// read that lists an in-flight delivery is refused before `stop`: nothing is
// requested, the runner keeps running, and the reader is told why.
func TestSettleRefusesBeforeStopWhenTheFreshReadListsACallInFlight(t *testing.T) {
	r := newSRig(t, func(c *SupervisorConfig) { c.QuitWait = 60 * time.Millisecond })
	key := r.inst("proj")
	r.run(key)
	r.withDeliveries(key, &fakeDel{id: "d-a", state: "failed"})
	r.gh.update(key, func(s *ghInst) { s.inflight = true }) // a call the runner admitted, listed in the read
	child := r.l.child(key)

	res, err := r.settle(key, SettleRequest{Action: ActionRetryFailed})
	wantRefused(t, err, RefusedNotOffered)
	for _, want := range []string{"consumer call", "settle once it ends", "keeps running"} {
		if !strings.Contains(err.Error(), want) {
			t.Errorf("refusal %q lacks %q", err, want)
		}
	}
	if len(res.Settled) != 0 || res.StoppedRunner {
		t.Errorf("result %+v", res)
	}
	if r.gh.count(key, "stop") != 0 || r.gh.count(key, "retry") != 0 {
		t.Errorf("calls %v: nothing may be stopped or retried", r.gh.commands(key))
	}
	if isDone(child) || child.termCount() != 0 || r.gh.state(key).runner != "running" {
		t.Error("the runner did not keep running")
	}
	// Its exit later would be a crash, not a requested stop: nothing was asked.
	r.sup.mu.Lock()
	asked := r.sup.sessions[key].asked
	r.sup.mu.Unlock()
	if asked {
		t.Error("the session believes a stop was requested")
	}

	// Once the call has ended, the same request goes through.
	r.gh.update(key, func(s *ghInst) { s.inflight = false })
	res, err = r.settle(key, SettleRequest{Action: ActionRetryFailed})
	if err != nil || !res.StoppedRunner || !res.Restarted || !slices.Equal(res.Settled, []string{"d-a"}) {
		t.Fatalf("%+v %v", res, err)
	}
}

func (r *srig) clearHook() {
	r.gh.mu.Lock()
	r.gh.hook = nil
	r.gh.mu.Unlock()
}

// A stop that fails without running out of time may never have been recorded, so
// the runner was not asked to end: when it ends later, that is a crash and is
// reported as one. (Only a stop that timed out stays "asked": its request is
// recorded and the runner exits when its call ends.)
func TestSettleWhoseStopFailsWithoutTimingOutDoesNotTreatALaterExitAsRequested(t *testing.T) {
	r := newSRig(t)
	key := r.inst("proj")
	r.run(key)
	r.withDeliveries(key, &fakeDel{id: "d-a", state: "failed"})
	r.gh.hook = func(_ context.Context, _, cmd string, _ int) (bool, string, error) {
		if cmd == "stop" {
			return true, "", exitStatus(6)
		}
		return false, "", nil
	}
	child := r.l.child(key)
	res, err := r.settle(key, SettleRequest{Action: ActionRetryFailed})
	var se *SettleError
	if !errors.As(err, &se) || se.Stage != SettleStopping || se.Cause == nil || res.StoppedRunner {
		t.Fatalf("%+v %v", res, err)
	}
	if r.gh.count(key, "retry") != 0 || child.termCount() != 0 || isDone(child) {
		t.Errorf("a failed stop went on: calls %v, signalled %d", r.gh.commands(key), child.termCount())
	}
	r.clearHook()

	child.crash(runner.Exit{Code: 1})
	eventuallyTrue(t, "the session to see the runner end", func() bool {
		info, _ := r.sup.Runner(key)
		return info.Phase == PhaseExited
	})
	if info, _ := r.sup.Runner(key); !strings.Contains(info.Notice, "ended on its own") {
		t.Errorf("an exit nobody asked for was not reported as a crash: %q", info.Notice)
	}
}

// `stop` returns without error but the runner process is still alive after
// ReapWait: Settle settles nothing and signals nothing.
func TestSettleWhoseStopReturnsWhileTheRunnerProcessLivesSettlesNothingAndSignalsNothing(t *testing.T) {
	r := newSRig(t)
	key := r.inst("proj")
	r.run(key)
	r.withDeliveries(key, &fakeDel{id: "d-a", state: "failed"})
	r.gh.hook = func(_ context.Context, _, cmd string, _ int) (bool, string, error) {
		return cmd == "stop", "", nil // `stop` says yes and the runner carries on
	}
	child := r.l.child(key)
	res, err := r.settle(key, SettleRequest{Action: ActionRetryFailed})
	var se *SettleError
	if !errors.As(err, &se) || se.Stage != SettleStopping || se.Done != 0 || res.StoppedRunner || len(res.Settled) != 0 {
		t.Fatalf("%+v %v", res, err)
	}
	for _, want := range []string{"nothing was settled", "has not ended", "still alive and was not signalled"} {
		if !strings.Contains(err.Error(), want) {
			t.Errorf("text %q lacks %q", err, want)
		}
	}
	if r.gh.count(key, "retry") != 0 || delState(r, key, "d-a") != "failed" {
		t.Errorf("a delivery was settled with a runner alive: %v", r.gh.commands(key))
	}
	if child.termCount() != 0 || isDone(child) {
		t.Error("the runner was signalled or ended")
	}
	r.clearHook()
}

// Once Quit has begun, Settle that has stopped the runner and settled its
// deliveries does not start it again: Quit releases the instance. Each guard
// alone must hold, since Quit sets the closing flag a moment before it ends the
// operations' context.
func TestSettleDoesNotRestartTheRunnerOnceQuitHasBegun(t *testing.T) {
	setup := func(t *testing.T, during func(r *srig, ctx context.Context, cancel context.CancelFunc)) (*srig, SettleResult, error) {
		r := newSRig(t)
		key := r.inst("proj")
		r.run(key)
		r.withDeliveries(key, &fakeDel{id: "d-a", state: "failed"})
		ctx, cancel := context.WithCancel(context.Background())
		defer cancel()
		r.gh.hook = func(hctx context.Context, _, cmd string, n int) (bool, string, error) {
			if cmd == "retry" && n == 1 {
				during(r, hctx, cancel)
			}
			return false, "", nil
		}
		res, err := r.sup.Settle(ctx, key, SettleRequest{Action: ActionRetryFailed})
		r.clearHook()
		return r, res, err
	}
	check := func(t *testing.T, r *srig, res SettleResult, err error) {
		t.Helper()
		if err != nil || !res.StoppedRunner || res.Restarted || !slices.Equal(res.Settled, []string{"d-a"}) {
			t.Fatalf("%+v %v", res, err)
		}
		if r.l.launchCount() != 1 {
			t.Errorf("%d launches: a runner was started while the session quits", r.l.launchCount())
		}
	}
	t.Run("the closing flag alone", func(t *testing.T) {
		r, res, err := setup(t, func(r *srig, _ context.Context, _ context.CancelFunc) {
			r.sup.mu.Lock()
			r.sup.closing = true
			r.sup.mu.Unlock()
		})
		check(t, r, res, err)
	})
	t.Run("the caller's context ending alone", func(t *testing.T) {
		r, res, err := setup(t, func(_ *srig, _ context.Context, cancel context.CancelFunc) { cancel() })
		check(t, r, res, err)
	})
	t.Run("a real Quit", func(t *testing.T) {
		var quit chan QuitReport
		r, res, err := setup(t, func(r *srig, ctx context.Context, _ context.CancelFunc) {
			quit = make(chan QuitReport, 1)
			go func() { quit <- r.sup.Quit(context.Background()) }()
			<-ctx.Done() // Quit has ended the operations' context
		})
		check(t, r, res, err)
		select {
		case rep := <-quit:
			if len(rep.Instances) != 1 || rep.Instances[0].Outcome != QuitReleased || !rep.Clean() {
				t.Errorf("report %+v", rep)
			}
		case <-time.After(5 * time.Second):
			t.Fatal("Quit did not finish")
		}
	})
}

// ---- Settle on an adopted instance ----

// An adopted instance's runner is no child of this session: Settle stops it
// through the token, settles, and starts a runner of this session's own.
func TestSettleOnAnAdoptedInstanceStopsItsRunnerThroughTheTokenAndStartsOneOfItsOwn(t *testing.T) {
	r := newSRig(t)
	key := r.inst("proj")
	r.run(key)
	old := r.l.child(key)
	killSession(t, r.sup)
	b := r.another()
	if got := b.Adopt(everywhere); len(got) != 1 {
		t.Fatalf("adopted %v", got)
	}
	r.withDeliveries(key, &fakeDel{id: "d-a", state: "failed"}, &fakeDel{id: "d-b", state: "failed"})

	res, err := b.Settle(context.Background(), key, SettleRequest{Action: ActionRetryFailed})
	if err != nil || !res.StoppedRunner || !res.Restarted || !slices.Equal(res.Settled, []string{"d-a", "d-b"}) {
		t.Fatalf("%+v %v", res, err)
	}
	if cmds := after(r.gh.commands(key), "stop"); len(cmds) < 3 || !slices.Equal(cmds[:3], []string{"stop", "retry", "retry"}) {
		t.Errorf("calls %v, want stop, retry, retry first", cmds)
	}
	if old.termCount() != 0 {
		t.Error("the ended session's runner was sent SIGTERM")
	}
	if r.l.launchCount() != 2 || r.gh.state(key).runner != "running" {
		t.Errorf("%d launches, runner %q: want a runner of this session's own", r.l.launchCount(), r.gh.state(key).runner)
	}
	if info, _ := b.Runner(key); info.Adopted || info.Phase != PhaseRunning || info.PID == 0 {
		t.Errorf("info %+v", info)
	}
	if delState(r, key, "d-a") != "pending" || delState(r, key, "d-b") != "pending" {
		t.Error("not settled")
	}
}

// A stop that runs out of time has no process of this session's to SIGTERM:
// nothing is settled, nothing is signalled, and the instance stays as it was.
func TestSettleOnAnAdoptedInstanceWhoseStopTimesOutSignalsNothing(t *testing.T) {
	r := newSRig(t, func(c *SupervisorConfig) { c.QuitWait = 60 * time.Millisecond })
	key := r.inst("proj")
	r.run(key)
	old := r.l.child(key)
	killSession(t, r.sup)
	b := r.another()
	b.Adopt(everywhere)
	r.withDeliveries(key, &fakeDel{id: "d-a", state: "failed"})
	r.admitACallBeforeStop(key)

	res, err := b.Settle(context.Background(), key, SettleRequest{Action: ActionRetryFailed})
	var se *SettleError
	if !errors.As(err, &se) || se.Stage != SettleStopping || res.StoppedRunner || len(res.Settled) != 0 {
		t.Fatalf("%+v %v", res, err)
	}
	if old.termCount() != 0 || isDone(old) || r.gh.count(key, "retry") != 0 || r.l.launchCount() != 1 {
		t.Errorf("signalled %d, ended %v, calls %v, launches %d", old.termCount(), isDone(old), r.gh.commands(key), r.l.launchCount())
	}
	if info, _ := b.Runner(key); !info.Adopted || info.TokenFile == "" || !fileExists(info.TokenFile) {
		t.Errorf("info %+v: the instance should still be held as adopted", info)
	}
	r.clearHook()
}
