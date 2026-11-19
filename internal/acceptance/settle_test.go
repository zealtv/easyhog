//go:build unix

package acceptance

import (
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	tea "github.com/charmbracelet/bubbletea"

	"github.com/zealtv/easyhog/internal/core"
	"github.com/zealtv/easyhog/internal/groundhog"
	"github.com/zealtv/easyhog/internal/tui"
)

// A failed and an uncertain delivery settled from the palette, the uncertain one
// left by a Quit that gave up waiting.
//
// Groundhog's `retry` and `recover` need a stopped runner, so each settling
// action stops this session's runner, settles, and starts it again; this test
// shows it.
//
// The reader's path after a Quit that timed out: the instance is unowned, so
// nothing is offered to settle until they run it again; Run enrolls it and
// starts a runner, which with the barrier up admits no call; then Recover as
// done or Recover and retry settles it.
func TestSettlingFromThePalette(t *testing.T) {
	script := gate(t)
	w := newWorld(t, script)
	w.quitWait = 2 * time.Second // a Quit gives up on a call that outlasts this
	failing := repoFile(t, "examples", "consumers", "fail-until-fixed.sh")

	// A consumer that never answers until it is released, and logs each call.
	release := filepath.Join(w.tmp, "release")
	t.Cleanup(func() { _ = os.WriteFile(release, nil, 0o644) }) // detached calls end with the test
	slow := filepath.Join(w.tmp, "slow-consumer.sh")
	w.must(os.WriteFile(slow, []byte("#!/bin/sh\necho \"$GROUNDHOG_DELIVERY_ID\" >> calls.log\n"+
		"while [ ! -e \""+release+"\" ]; do sleep 0.2; done\n"), 0o755))

	flaky := w.instance("flaky")
	w.consumer(flaky, nil, "/bin/sh", failing)
	w.item(flaky, overdue, "f1", "flaky note")
	slowA := w.instance("slow-a")
	w.consumer(slowA, nil, "/bin/sh", slow)
	w.item(slowA, overdue, "a1", "slow a first")
	w.item(slowA, "every/1h", "a2", "slow a second")
	slowB := w.instance("slow-b")
	w.consumer(slowB, nil, "/bin/sh", slow)
	w.item(slowB, overdue, "b1", "slow b")
	w.start()

	// lists reports whether the palette, searched for query, lists that command's
	// row (glyph and name). `w.offers` would also count Open folder, whose aliases
	// fuzzy-match "retry" and "recover".
	lists := func(query, row string) bool {
		t.Helper()
		w.send(typed(":" + query)...)
		found := strings.Contains(w.screen(), row)
		w.send(press(tea.KeyEsc))
		return found
	}
	const retryRow, doneRow, andRetryRow = "↺ Retry failed", "✓ Recover as done", "↺ Recover and retry"

	calls := func(inst string) []string {
		b, _ := os.ReadFile(filepath.Join(filepath.Dir(inst), "calls.log"))
		return strings.Fields(string(b))
	}

	t.Run("a failed delivery is retried from the palette", func(t *testing.T) {
		w.selectInstance("flaky")
		w.palette("run")
		w.await("the failing consumer to be tried", 30*time.Second, func() bool {
			return countState(w.status(flaky), groundhog.DeliveryFailed) == 1
		})
		w.reread("flaky")
		if !lists("retry failed", retryRow) {
			t.Fatalf("a failed delivery of ours does not offer Retry failed\n%s", w.screen())
		}
		if lists("recover", doneRow) || lists("recover", andRetryRow) {
			t.Errorf("Recover is offered with no uncertain delivery\n%s", w.screen())
		}
		w.show("flaky, failed")
		// The reader mends the consumer, then retries.
		w.must(os.WriteFile(filepath.Join(filepath.Dir(flaky), "fixed"), nil, 0o644))
		starts := w.easy.startsFor(flaky)
		w.palette("retry failed")
		w.show("flaky after Retry failed")
		said := w.flat()
		for _, want := range []string{"Last action: Retry failed: flaky: 1 failed delivery set pending", "the runner was stopped and restarted"} {
			if !strings.Contains(said, want) {
				t.Errorf("the preview does not say %q\n%s", want, w.screen())
			}
		}
		if got := w.easy.startsFor(flaky); got != starts+1 {
			t.Errorf("the runner was started %d times after Retry failed, want 1", got-starts)
		}
		w.await("the retried delivery to be accepted", 30*time.Second, func() bool {
			return countState(w.status(flaky), groundhog.DeliveryAccepted) == 1
		})
		st := w.status(flaky)
		if d := st.Deliveries[0]; len(d.Attempts) != 2 || d.Attempts[1].ExitCode == nil || *d.Attempts[1].ExitCode != 0 {
			t.Errorf("the delivery has %d attempts (want a second one that exited 0)", len(d.Attempts))
		}
		if st.Owner == nil || st.Runner.State != groundhog.RunnerRunning || runnerProcesses(t, flaky) != 1 {
			t.Errorf("flaky after Retry failed: owner %+v, runner %s, %d processes", st.Owner, st.Runner.State, runnerProcesses(t, flaky))
		}
		w.reread("flaky")
		if lists("retry", retryRow) {
			t.Errorf("Retry failed is still offered with nothing failed\n%s", w.screen())
		}
	})

	t.Run("a Quit that gives up waiting leaves uncertain deliveries", func(t *testing.T) {
		for _, rel := range []string{"slow-a", "slow-b"} {
			w.selectInstance(rel)
			w.palette("run")
		}
		w.await("a call in flight on each", 30*time.Second, func() bool {
			return countState(w.status(slowA), groundhog.DeliveryInFlight) == 1 && countState(w.status(slowB), groundhog.DeliveryInFlight) == 1
		})
		if !w.palette("quit") {
			t.Fatal("the palette's Quit did not quit")
		}
		ctx, cancel := context.WithTimeout(w.ctx, 120*time.Second)
		defer cancel()
		rep := w.sup.Quit(ctx)
		for _, in := range rep.Instances {
			if (in.Instance == slowA || in.Instance == slowB) && (!in.Terminated || in.Outcome != core.QuitReleased) {
				t.Errorf("%s: terminated %v, %v (%s); want the runner sent SIGTERM and the instance released", in.Instance, in.Terminated, in.Outcome, in.Notice)
			}
		}
		for _, inst := range []string{slowA, slowB} {
			st := w.status(inst)
			if st.Owner != nil || countState(st, groundhog.DeliveryUncertain) != 1 {
				t.Fatalf("%s after the Quit: owner %+v, %d uncertain", inst, st.Owner, countState(st, groundhog.DeliveryUncertain))
			}
			for _, d := range st.Deliveries {
				if d.State == groundhog.DeliveryUncertain {
					if l := d.Attempts[len(d.Attempts)-1].OwnerLabel; l == nil || *l != "easyhog-acceptance" {
						t.Errorf("%s: the uncertain attempt's owner label is %v", inst, l)
					}
				}
			}
		}
	})

	t.Run("reopened, nothing is offered to settle until Run", func(t *testing.T) {
		w.closeSession()
		w.openSession()
		w.selectInstance("slow-a")
		w.show("slow-a after the Quit that gave up")
		n := w.node("slow-a")
		if !hasAction(n, tui.ActionRun) || hasAction(n, tui.ActionRecoverAccept) || hasAction(n, tui.ActionRecoverRetry) || hasAction(n, tui.ActionRetryFailed) {
			t.Fatalf("an unowned instance holding an uncertain delivery offers %v", n.Actions)
		}
		if lists("recover", doneRow) || lists("recover", andRetryRow) || lists("retry", retryRow) {
			t.Errorf("the palette offers Recover or Retry on an unowned instance\n%s", w.screen())
		}
		said := w.flat()
		for _, want := range []string{
			"Holds 1 uncertain delivery (every handoff waits until they are recovered)",
			"Run enrolls it for this session, and then Recover as done and Recover and retry are offered",
			"admitted by easyhog-acceptance",
		} {
			if !strings.Contains(said, want) {
				t.Errorf("the preview does not say %q\n%s", want, w.screen())
			}
		}
	})

	t.Run("Run, then Recover as done", func(t *testing.T) {
		w.palette("run")
		if !strings.Contains(w.flat(), "Run: slow-a: enrolled for this session, runner started") {
			t.Fatalf("Run did not enroll and start the runner\n%s", w.screen())
		}
		w.reread("slow-a")
		before := len(calls(slowA))
		// The runner runs, and with the barrier up admits no call: Run alone
		// settles nothing, and the verdict says handoffs wait.
		time.Sleep(3 * time.Second)
		if got := len(calls(slowA)); got != before {
			t.Errorf("a runner admitted a call with an uncertain delivery on record (%d calls, was %d)", got, before)
		}
		if st := w.status(slowA); countState(st, groundhog.DeliveryUncertain) != 1 || countState(st, groundhog.DeliveryPending) == 0 {
			t.Fatalf("after Run: %d uncertain, %d pending", countState(st, groundhog.DeliveryUncertain), countState(st, groundhog.DeliveryPending))
		}
		w.sync()
		if said := w.flat(); !strings.Contains(said, "items fire, but handoffs wait until 1 uncertain delivery is recovered") || strings.Contains(said, "Pending work: yes") {
			t.Errorf("the verdict is not qualified\n%s", w.screen())
		}
		if !lists("recover as", doneRow) || !lists("recover and", andRetryRow) || lists("retry", retryRow) {
			t.Fatalf("an owned instance with an uncertain delivery should offer both Recovers and no Retry failed\n%s", w.screen())
		}
		starts := w.easy.startsFor(slowA)
		w.palette("recover as")
		w.show("slow-a after Recover as done")
		said := w.flat()
		for _, want := range []string{"Last action: Recover as done: slow-a: 1 uncertain delivery recorded as done on your say-so; nothing was invoked", "the runner was stopped and restarted"} {
			if !strings.Contains(said, want) {
				t.Errorf("the preview does not say %q\n%s", want, w.screen())
			}
		}
		if got := w.easy.startsFor(slowA); got != starts+1 {
			t.Errorf("the runner was started %d times after Recover, want 1", got-starts)
		}
		st := w.status(slowA)
		if countState(st, groundhog.DeliveryUncertain) != 0 || countState(st, groundhog.DeliveryAccepted) != 1 {
			t.Errorf("after Recover as done: %d uncertain, %d accepted", countState(st, groundhog.DeliveryUncertain), countState(st, groundhog.DeliveryAccepted))
		}
		for _, d := range st.Deliveries {
			if d.State == groundhog.DeliveryAccepted {
				a := d.Attempts[len(d.Attempts)-1]
				if a.Decision == nil || *a.Decision != "accept" || a.Outcome != "unknown" {
					t.Errorf("the accepted delivery's attempt: decision %v, outcome %q (the outcome stays unknown)", a.Decision, a.Outcome)
				}
			}
		}
		// The barrier is down: the runner, running again, takes the next call.
		w.await("the waiting delivery to be handed off", 30*time.Second, func() bool { return len(calls(slowA)) > before })
		if st := w.status(slowA); st.Owner == nil || st.Runner.State != groundhog.RunnerRunning || st.Paused {
			t.Errorf("slow-a after Recover: owner %+v, runner %s, paused %v", st.Owner, st.Runner.State, st.Paused)
		}
	})

	t.Run("Run, then Recover and retry", func(t *testing.T) {
		w.selectInstance("slow-b")
		w.palette("run")
		w.reread("slow-b")
		if !lists("recover and", andRetryRow) {
			t.Fatalf("Recover and retry is not offered on slow-b after Run\n%s", w.screen())
		}
		w.must(os.WriteFile(release, nil, 0o644)) // the new attempt finishes at once
		w.palette("recover and")
		w.show("slow-b after Recover and retry")
		said := w.flat()
		for _, want := range []string{"Last action: Recover and retry: slow-b: 1 uncertain delivery set pending to be tried again; nothing was invoked", "the runner was stopped and restarted"} {
			if !strings.Contains(said, want) {
				t.Errorf("the preview does not say %q\n%s", want, w.screen())
			}
		}
		w.await("the retried delivery to be accepted by a new attempt", 30*time.Second, func() bool {
			return countState(w.status(slowB), groundhog.DeliveryAccepted) == 1
		})
		d := w.status(slowB).Deliveries[0]
		if len(d.Attempts) != 2 || d.Attempts[0].Decision == nil || *d.Attempts[0].Decision != "retry" || d.Attempts[1].ExitCode == nil || *d.Attempts[1].ExitCode != 0 {
			t.Errorf("slow-b's delivery: %d attempts, first decision %v (want retry, then a second attempt that exited 0)", len(d.Attempts), d.Attempts[0].Decision)
		}
	})

	t.Run("Quit still releases everything", func(t *testing.T) {
		if !w.palette("quit") {
			t.Fatal("the palette's Quit did not quit")
		}
		ctx, cancel := context.WithTimeout(w.ctx, 120*time.Second)
		defer cancel()
		rep := w.sup.Quit(ctx)
		for _, in := range rep.Instances {
			if in.Outcome == core.QuitLeftOwned {
				t.Errorf("%s was left owned: %s", in.Instance, in.Notice)
			}
		}
		for _, inst := range []string{flaky, slowA, slowB} {
			if n := runnerProcesses(t, inst); n != 0 {
				t.Errorf("%s: %d runner processes after Quit", inst, n)
			}
		}
	})
}
