//go:build unix

package acceptance

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"syscall"
	"testing"
	"time"

	tea "github.com/charmbracelet/bubbletea"

	"github.com/zealtv/easyhog/internal/core"
	"github.com/zealtv/easyhog/internal/groundhog"
	"github.com/zealtv/easyhog/internal/tui"
)

// TestMVPAcceptance is one reader's session over a temporary directory of
// synthetic repositories, through the headless core and the Bubble Tea model,
// with real Groundhog runners on the pinned script. The steps run in order and
// share the world; a step that fails stops the rest, since later steps build
// on earlier ones.
//
// The claims it checks are Easyhog's product rules and the pinned Groundhog
// script's observed behavior. A newer upstream that disagrees means the pin
// moves deliberately, with this test.
func TestMVPAcceptance(t *testing.T) {
	script := gate(t)
	w := newWorld(t, script)
	f := w.build(t, w.newWake(t))
	w.start()
	// Another supervisor (its own session, token and label) runs delta before
	// Easyhog looks: that runner is alive and not Easyhog's to control.
	if res, err := w.osup.Run(w.ctx, f.delta); err != nil || res.Outcome != core.RunStarted {
		t.Fatalf("setup: another supervisor could not run delta: %+v %v", res, err)
	}

	step := func(name string, fn func(t *testing.T)) {
		t.Helper()
		if !t.Run(name, fn) {
			t.Fatalf("stopping: step %q failed, and the steps after it depend on it", name)
		}
	}

	step("discover without running anything", func(t *testing.T) { w.discover(t, f) })
	step("refresh classifies every instance", func(t *testing.T) { w.classify(t, f) })
	step("expand and inspect without running payloads", func(t *testing.T) { w.expandAndInspect(t, f) })
	step("run a repository with no consumer: delivery only", func(t *testing.T) { w.runNoConsumer(t, f) })
	step("run, deliver and consume with a plain script", func(t *testing.T) { w.runBravo(t, f) })
	step("stop with a backlog", func(t *testing.T) { w.stopWithBacklog(t, f) })
	step("a failed consumer is visible and stops no one", func(t *testing.T) { w.failedConsumer(t, f) })
	step("run again finishes the backlog", func(t *testing.T) { w.runAgain(t, f) })
	step("externally owned instances are inspected, never controlled", func(t *testing.T) { w.externals(t, f) })
	step("no duplicate owners", func(t *testing.T) { w.noDuplicateOwners(t, f) })
	step("errors stay visible beside healthy instances", func(t *testing.T) { w.visibleErrors(t, f) })
	step("hiding unsupported instances runs nothing and keeps the highlight sensible", func(t *testing.T) { w.hideUnsupported(t, f) })
	if f.wake != nil {
		step("in-place wake through the adapter", func(t *testing.T) { w.wakeInPlace(t, f) })
	} else {
		t.Run("in-place wake through the adapter", func(t *testing.T) {
			t.Skip("EASYHOG_WAKE_CONSUMER is not set, or tmux is missing: the wake adapter part did not run")
		})
	}
	step("shutdown releases what Easyhog owns and leaves the rest", func(t *testing.T) { w.shutdown(t, f) })
	step("reopening restarts nothing until Run", func(t *testing.T) { w.reopen(t, f) })
	if _, err := os.Stat(filepath.Join(w.tmp, "canary")); err == nil {
		t.Errorf("a payload file was executed during the session (the canary exists)")
	}
}

// show logs the current screen when EASYHOG_ACCEPTANCE_SCREENS=1, for captures.
func (w *world) show(title string) {
	if os.Getenv("EASYHOG_ACCEPTANCE_SCREENS") == "1" {
		w.t.Logf("--- %s\n%s", title, w.screen())
	}
}

func (w *world) discover(t *testing.T, f *fixture) {
	w.show("first screen")
	screen := w.screen()
	want := []string{"alpha", "team/bravo", "charlie", "delta",
		"echo", "foxtrot", "golf"}
	if f.wake != nil {
		want = append(want, "wake")
	}
	for _, rel := range want {
		if !strings.Contains(screen, rel) {
			t.Errorf("the first screen does not list %s\n%s", rel, screen)
		}
	}
	// Malformed is known from listings alone, so it starts hidden and counted;
	// the legacy script is not known until it is negotiated, so it is listed.
	if strings.Contains(screen, "hotel") {
		t.Errorf("the malformed instance is listed by default\n%s", screen)
	}
	if !strings.Contains(screen, "1 hidden") {
		t.Errorf("the hidden count is not shown\n%s", screen)
	}
	// An unreadable directory is reported without hiding the healthy instances.
	if os.Geteuid() != 0 && !strings.Contains(screen, "private") {
		t.Errorf("the unreadable directory is not reported\n%s", screen)
	}
	// Nothing has run: no owner, no runner, no token, no runtime file but the config.
	if n := len(w.sup.Runners()); n != 0 {
		t.Errorf("%d runners before any Run", n)
	}
	for _, inst := range []string{f.alpha, f.bravo, f.charlie} {
		if n := runnerProcesses(t, inst); n != 0 {
			t.Errorf("%s: %d runner processes before any Run", inst, n)
		}
	}
	// Easyhog's own session wrote nothing: no state directory, no token. (The
	// one runner in the tree is the other supervisor's, on delta.)
	if _, err := os.Stat(filepath.Join(w.tmp, "easy-state")); err == nil {
		t.Errorf("Easyhog created its state directory before any Run")
	}
	if n := runnerProcesses(t, f.delta); n != 1 {
		t.Errorf("delta: %d runner processes, want the other supervisor's one", n)
	}
	// "Wait for a keypress before running any script": the first instance is
	// highlighted, and after several settle delays idling has read none of them.
	time.Sleep(400 * time.Millisecond)
	w.sync()
	for _, iv := range w.ref.View().Instances {
		wantClass := core.ClassNotInspected
		if len(iv.Instance.Malformed) > 0 {
			wantClass = core.ClassMalformed
		}
		if iv.Class != wantClass || iv.Inspection.Last != nil || iv.Inspection.Support != nil {
			t.Errorf("%s was inspected (%s) before any key press or Refresh", iv.Instance.Display, iv.Class)
		}
	}
	if _, err := os.Stat(filepath.Join(filepath.Dir(f.foxtrot), "calls.log")); err == nil {
		t.Errorf("the legacy script was run before any Refresh")
	}
	// The palette shows what is hidden; showing is a view action and runs nothing.
	w.palette("show hidden")
	if !strings.Contains(w.screen(), "hotel") || w.view("hotel").Class != core.ClassMalformed {
		t.Errorf("show hidden did not list the malformed instance\n%s", w.screen())
	}
	if _, err := os.Stat(filepath.Join(filepath.Dir(f.foxtrot), "calls.log")); err == nil {
		t.Errorf("showing hidden instances ran the legacy script")
	}
	if n := len(w.sup.Runners()); n != 0 {
		t.Errorf("showing hidden instances started %d runners", n)
	}
}

// classes is what each repository must read as once Refresh has run every
// well-formed script, and whether the palette may offer Run for it.
func (w *world) classes(f *fixture) map[string]core.Class {
	c := map[string]core.Class{
		"alpha": core.ClassUnowned, "team/bravo": core.ClassUnowned, "charlie": core.ClassUnowned,
		"delta": core.ClassExternal, "echo": core.ClassExternal, "foxtrot": core.ClassLegacy,
		"golf": core.ClassFailing, "hotel": core.ClassMalformed,
	}
	if f.wake != nil {
		c["wake"] = core.ClassUnowned
	}
	return c
}

func (w *world) classify(t *testing.T, f *fixture) {
	w.palette("refresh")
	want := w.classes(f)
	w.until("every instance to be classified by Refresh", 60*time.Second, func() bool {
		for rel, class := range want {
			if w.view(rel).Class != class {
				return false
			}
		}
		return true
	})
	for rel, class := range want {
		if got := w.view(rel).Class; got != class {
			t.Errorf("%s reads as %s, want %s", rel, got, class)
		}
	}

	// Control eligibility follows capabilities and ownership: Run is offered
	// for the unowned, controllable instances and for nothing else, and Stop
	// for none (nothing of ours is running yet). Ownership is not liveness: the
	// stopped external owner (echo) and the running one (delta) read alike.
	for rel, class := range want {
		w.selectInstance(rel)
		if got, wantRun := w.offers("run"), class == core.ClassUnowned; got != wantRun {
			t.Errorf("%s (%s): palette offers Run = %v, want %v\n%s", rel, class, got, wantRun, w.screen())
		}
		if w.offers("stop") {
			t.Errorf("%s (%s): the palette offers Stop before anything is ours", rel, class)
		}
	}

	// Facts behind the two external owners, read from the protocol itself.
	d, e := w.status(f.delta), w.status(f.echo)
	if d.Owner == nil || d.Owner.Label != "another-supervisor" || d.Runner.State != groundhog.RunnerRunning {
		t.Errorf("delta: want a running runner owned by another-supervisor, got owner %+v runner %s", d.Owner, d.Runner.State)
	}
	if e.Owner == nil || e.Owner.Label != "stopped-supervisor" || e.Runner.State != groundhog.RunnerStopped {
		t.Errorf("echo: want an owner and a stopped runner, got owner %+v runner %s", e.Owner, e.Runner.State)
	}

	// The legacy script was asked only `capabilities --json`; the malformed
	// one was never run at all.
	calls, err := os.ReadFile(filepath.Join(filepath.Dir(f.foxtrot), "calls.log"))
	if err != nil {
		t.Fatalf("the legacy script was never asked anything: %v", err)
	}
	for _, line := range strings.Split(strings.TrimSpace(string(calls)), "\n") {
		if line != "capabilities --json" {
			t.Errorf("the legacy script was run with %q", line)
		}
	}
	if _, err := os.Stat(filepath.Join(filepath.Dir(f.hotel), "calls.log")); err == nil {
		t.Errorf("the malformed instance's script was run")
	}

	// Errors are visible where the reader looks, in words.
	for rel, text := range map[string][]string{
		"foxtrot": {"legacy", "read-only"},
		"golf":    {"failing", "status is unavailable"},
		"hotel":   {"malformed"},
	} {
		w.selectInstance(rel)
		w.show(rel)
		for _, s := range text {
			if !strings.Contains(w.flat(), s) {
				t.Errorf("%s: the preview does not say %q\n%s", rel, s, w.screen())
			}
		}
	}
}

func (w *world) expandAndInspect(t *testing.T, f *fixture) {
	w.selectInstance("alpha")
	w.send(press(tea.KeyRight)) // expand
	for label, want := range map[string][]string{
		"old-note":    {"Cadence: once", "due now", "alpha overdue note", "run-me.sh"},
		"future-note": {"Cadence: once", "not due", "alpha future note"},
		"hourly":      {"Cadence: every 1h", "alpha hourly note"},
	} {
		w.selectRow(label)
		time.Sleep(60 * time.Millisecond)
		w.sync()
		w.show(label)
		for _, s := range want {
			if !strings.Contains(w.screen(), s) {
				t.Errorf("%s: the preview does not show %q\n%s", label, s, w.screen())
			}
		}
	}
	// The instance row shows what the status says.
	w.selectRow("alpha")
	w.send(press(tea.KeyLeft)) // collapse again
	for _, s := range []string{"Kind: unowned", "Palette offers: Run", "Consumer: not configured", "Deliveries: none"} {
		if !strings.Contains(w.screen(), s) {
			t.Errorf("alpha: the preview does not show %q\n%s", s, w.screen())
		}
	}
	if _, err := os.Stat(filepath.Join(w.tmp, "canary")); err == nil {
		t.Errorf("inspecting alpha's payload ran it")
	}
}

// pendingReasons returns the reasons of the pending deliveries.
func pendingReasons(st groundhog.Status) []string {
	var out []string
	for _, d := range st.Deliveries {
		if d.State == groundhog.DeliveryPending {
			r := "(none)"
			if d.Reason != nil {
				r = *d.Reason
			}
			out = append(out, r)
		}
	}
	return out
}

func hasAction(n tui.Node, a tui.Action) bool {
	for _, x := range n.Actions {
		if x == a {
			return true
		}
	}
	return false
}

func (w *world) key(rel string) string { return filepath.Join(w.root, rel, ".groundhog") }

func (w *world) runNoConsumer(t *testing.T, f *fixture) {
	w.selectInstance("alpha")
	// The overdue item has waited: nothing fires until an enabled tick.
	if st := w.status(f.alpha); len(st.Deliveries) != 0 {
		t.Fatalf("alpha delivered before Run: %+v", st.Deliveries)
	}
	// An interrupted journal write : the status stays
	// incoherent and unowned, the palette still offers Run, and Run settles it.
	w.must(os.WriteFile(filepath.Join(f.alpha, "runtime", "journal.gen"), []byte("000000000000000005\n"), 0o644))
	w.reread("alpha")
	if st := w.status(f.alpha); st.Consistent || st.Owner != nil || !w.offers("run") {
		t.Fatalf("an odd journal.gen: consistent %v, owner %+v, Run offered %v", st.Consistent, st.Owner, w.offers("run"))
	}
	if w.palette("run") {
		t.Fatal("Run quit the program")
	}
	w.show("alpha after Run")
	if want := "Run: alpha: enrolled for this session, runner started"; !strings.Contains(w.screen(), want) {
		t.Fatalf("the status line does not say %q\n%s", want, w.screen())
	}
	w.await("the overdue once item and the every/ item to be delivered", 30*time.Second, func() bool {
		return countState(w.status(f.alpha), groundhog.DeliveryPending) == 2
	})
	st := w.status(f.alpha)
	for _, r := range pendingReasons(st) {
		if r != "no_consumer" {
			t.Errorf("a delivery is pending for %q, want no_consumer", r)
		}
	}
	if st.Owner == nil || st.Owner.Label != "easyhog-acceptance" || st.Runner.State != groundhog.RunnerRunning || st.Paused || !st.Consistent {
		t.Errorf("alpha after Run: owner %+v, runner %s, paused %v, consistent %v", st.Owner, st.Runner.State, st.Paused, st.Consistent)
	}
	if _, err := os.Stat(filepath.Join(f.alpha, "schedule", overdue, "old-note")); err == nil {
		t.Errorf("the one-shot item stayed in the schedule after firing")
	}
	// The future item is untouched, and nothing consumed anything.
	if got := deliveriesOf(st, "future-note"); len(got) != 0 {
		t.Errorf("the future item fired: %+v", got)
	}
	time.Sleep(2500 * time.Millisecond) // a couple more cycles
	st = w.status(f.alpha)
	for _, d := range st.Deliveries {
		if d.State != groundhog.DeliveryPending || len(d.Attempts) != 0 {
			t.Errorf("without a consumer a delivery is %s with %d attempts", d.State, len(d.Attempts))
		}
	}
	// The reader's view follows the protocol, and the palette follows the view.
	w.reread("alpha")
	n := w.node("alpha")
	if n.State != tui.StateRunning || !strings.Contains(n.Badge, "2 pending") || !hasAction(n, tui.ActionStop) || hasAction(n, tui.ActionRun) {
		t.Errorf("alpha's row: state %v, badge %q, actions %v", n.State, n.Badge, n.Actions)
	}
	if !w.offers("stop") || w.offers("run") {
		t.Errorf("alpha is running and ours: the palette should offer Stop and not Run")
	}
	// Run again is idempotent: one enrollment, one runner.
	res, err := w.sup.Run(w.ctx, f.alpha)
	if err != nil || res.Outcome != core.RunAlreadyRunning {
		t.Errorf("second Run: %+v %v", res, err)
	}
	if got := w.easy.startsFor(f.alpha); got != 1 {
		t.Errorf("alpha's runner started %d times", got)
	}
}

func (w *world) bravoNotes() []string {
	b, err := os.ReadFile(filepath.Join(filepath.Dir(w.key("team/bravo")), "notes.log"))
	if err != nil {
		return nil
	}
	return strings.Split(strings.TrimSpace(string(b)), "\n")
}

func (w *world) runBravo(t *testing.T, f *fixture) {
	w.selectInstance("team/bravo")
	w.palette("run")
	if want := "Run: team/bravo: enrolled for this session, runner started"; !strings.Contains(w.screen(), want) {
		t.Fatalf("the status line does not say %q\n%s", want, w.screen())
	}
	// Four deliveries are registered; the consumer takes two seconds for each.
	w.await("the first handoff to be accepted", 30*time.Second, func() bool {
		return countState(w.status(f.bravo), groundhog.DeliveryAccepted) >= 1
	})
	st := w.status(f.bravo)
	if len(st.Deliveries) != 4 {
		t.Errorf("bravo has %d deliveries, want 4 (three overdue items and one every/ slot)", len(st.Deliveries))
	}
	for _, d := range st.Deliveries {
		if d.State == groundhog.DeliveryAccepted {
			if len(d.Attempts) != 1 || d.Attempts[0].ExitCode == nil || *d.Attempts[0].ExitCode != 0 || d.Execution != "unknown" {
				t.Errorf("an accepted delivery: %d attempts, exit %v, execution %q", len(d.Attempts), d.Attempts[0].ExitCode, d.Execution)
			}
		}
	}
}

// Stop does what Quit does for the one instance: the runner finishes the call
// it admitted and ends, the instance is released, and what is still waiting
// stays waiting. Nothing is paused.
func (w *world) stopWithBacklog(t *testing.T, f *fixture) {
	w.selectInstance("team/bravo")
	w.reread("team/bravo")
	if !w.offers("stop") {
		t.Fatalf("bravo is running and ours but the palette does not offer Stop\n%s", w.screen())
	}
	w.palette("stop")
	w.show("bravo after Stop")
	if want := "Stop: team/bravo: stopped and released."; !strings.Contains(w.screen(), want) {
		t.Fatalf("the status line does not say %q\n%s", want, w.screen())
	}
	// The call already admitted finished before the runner ended; nothing new is dispatched.
	st := w.status(f.bravo)
	accepted, pending := countState(st, groundhog.DeliveryAccepted), countState(st, groundhog.DeliveryPending)
	if st.Owner != nil || st.Paused || st.Runner.State == groundhog.RunnerRunning || countState(st, groundhog.DeliveryInFlight) != 0 {
		t.Fatalf("after Stop: owner %+v, paused %v, runner %s, %d in flight (want released, not paused, ended, none in flight)",
			st.Owner, st.Paused, st.Runner.State, countState(st, groundhog.DeliveryInFlight))
	}
	if pending == 0 || accepted == 0 {
		t.Fatalf("after Stop: %d accepted, %d pending (want a backlog)", accepted, pending)
	}
	if n := runnerProcesses(t, f.bravo); n != 0 {
		t.Errorf("%d runner processes after Stop", n)
	}
	if _, held := w.sup.Runner(f.bravo); held {
		t.Errorf("the session still holds bravo's token after Stop")
	}
	if sessions, _ := filepath.Glob(filepath.Join(w.tmp, "easy-state", "sessions", "*", "*.token")); len(sessions) != 1 {
		t.Errorf("after Stop the session should hold only alpha's token: %v", sessions)
	}
	time.Sleep(3500 * time.Millisecond) // several runner cycles
	st = w.status(f.bravo)
	if got := countState(st, groundhog.DeliveryAccepted); got != accepted {
		t.Errorf("a stopped instance dispatched: %d accepted, then %d", accepted, got)
	}
	if got := len(w.bravoNotes()); got != accepted {
		t.Errorf("the consumer ran %d times for %d accepted deliveries", got, accepted)
	}
	// The view says so, and the offer flips to Run.
	w.reread("team/bravo")
	n := w.node("team/bravo")
	if n.State != tui.StateStopped || !strings.Contains(n.Badge, "pending") || strings.Contains(n.Badge, "paused") {
		t.Errorf("bravo's row: state %v, badge %q: want stopped, with the backlog and no pause", n.State, n.Badge)
	}
	if !w.offers("run") || w.offers("stop") {
		t.Errorf("a stopped instance should offer Run and not Stop")
	}
	w.backlog = pending
}

func (w *world) failedConsumer(t *testing.T, f *fixture) {
	w.selectInstance("charlie")
	w.palette("run")
	w.await("the failing consumer to be tried", 30*time.Second, func() bool {
		return countState(w.status(f.charlie), groundhog.DeliveryFailed) == 1
	})
	time.Sleep(3 * time.Second)
	st := w.status(f.charlie)
	d := st.Deliveries[0]
	if len(d.Attempts) != 1 || d.Attempts[0].ExitCode == nil || *d.Attempts[0].ExitCode != 1 {
		t.Errorf("charlie's delivery: %d attempts, exit %v; want one attempt, exit 1, and no automatic retry", len(d.Attempts), d.Attempts[0].ExitCode)
	}
	b, _ := os.ReadFile(filepath.Join(filepath.Dir(f.charlie), "attempts.log"))
	if n := len(strings.Split(strings.TrimSpace(string(b)), "\n")); n != 1 {
		t.Errorf("the failing consumer ran %d times, want 1", n)
	}
	// The failure is on screen, on the row and in the preview, in words.
	w.reread("charlie")
	w.sync()
	w.show("charlie after a failed handoff")
	n := w.node("charlie")
	// The runner is healthy, yet the row reads failed ; the badge counts it and the
	// preview says so in words.
	if n.State != tui.StateFailed || !strings.Contains(n.Badge, "1 failed") {
		t.Errorf("charlie's row: state %v, badge %q", n.State, n.Badge)
	}
	if screen := w.screen(); !strings.Contains(screen, "Deliveries: 1 failed") {
		t.Errorf("the preview does not report the failed delivery\n%s", screen)
	}
	// Meanwhile the stopped instance is just as it was, and alpha still runs.
	if st := w.status(f.bravo); st.Owner != nil || st.Runner.State == groundhog.RunnerRunning {
		t.Errorf("charlie's failure disturbed bravo: owner %+v, runner %s", st.Owner, st.Runner.State)
	}
	if st := w.status(f.alpha); st.Runner.State != groundhog.RunnerRunning {
		t.Errorf("charlie's failure disturbed alpha: runner %s", st.Runner.State)
	}
}

func (w *world) runAgain(t *testing.T, f *fixture) {
	w.selectInstance("team/bravo")
	w.reread("team/bravo")
	w.palette("run")
	w.show("bravo after Run on a stopped instance")
	if want := "Run: team/bravo: enrolled for this session, runner started"; !strings.Contains(w.screen(), want) {
		t.Fatalf("the status line does not say %q\n%s", want, w.screen())
	}
	if got := w.easy.startsFor(f.bravo); got != 2 {
		t.Errorf("Run after Stop started %d runners in all, want a second one", got)
	}
	w.await("the whole backlog to be consumed", 40*time.Second, func() bool {
		return countState(w.status(f.bravo), groundhog.DeliveryAccepted) == 4
	})
	st := w.status(f.bravo)
	if st.Paused || countState(st, groundhog.DeliveryPending) != 0 {
		t.Errorf("after Run again: paused %v, %d pending", st.Paused, countState(st, groundhog.DeliveryPending))
	}
	// The effect happened once per delivery, and each delivery's own note arrived.
	notes := w.bravoNotes()
	seen := map[string]bool{}
	for _, line := range notes {
		id := strings.SplitN(line, "\t", 2)[0]
		if seen[id] {
			t.Errorf("delivery %s was collected twice", id)
		}
		seen[id] = true
	}
	if len(notes) != 4 {
		t.Errorf("notes.log has %d lines, want 4:\n%s", len(notes), strings.Join(notes, "\n"))
	}
	for _, want := range []string{"bravo note b1", "bravo note b2", "bravo note b3", "bravo hourly note"} {
		found := false
		for _, line := range notes {
			found = found || strings.HasSuffix(line, "\t"+want)
		}
		if !found {
			t.Errorf("no note %q was collected", want)
		}
	}
	// The every/ item fired for its slot once (a second slot, if the hour
	// turned during the run, would be a second delivery with another slot).
	slots := map[string]bool{}
	for _, d := range deliveriesOf(st, "hourly") {
		if d.Occurrence != nil && d.Occurrence.Slot != nil {
			if slots[*d.Occurrence.Slot] {
				t.Errorf("the every/ slot %s fired twice", *d.Occurrence.Slot)
			}
			slots[*d.Occurrence.Slot] = true
		}
	}
	if len(slots) == 0 {
		t.Errorf("the every/ item's delivery names no slot")
	}
	w.reread("team/bravo")
	if n := w.node("team/bravo"); n.State != tui.StateRunning || strings.Contains(n.Badge, "pending") || strings.Contains(n.Badge, "paused") {
		t.Errorf("bravo's row after resume: state %v, badge %q", n.State, n.Badge)
	}
}

func (w *world) externals(t *testing.T, f *fixture) {
	// The other supervisor's runner goes on with its own work, untouched by
	// anything Easyhog does: it consumes delta's overdue item.
	w.await("the other supervisor's runner to consume delta's item", 30*time.Second, func() bool {
		return countState(w.status(f.delta), groundhog.DeliveryAccepted) == 1
	})
	for _, c := range []struct {
		rel, inst, label string
		runner           groundhog.RunnerState
	}{
		{"delta", f.delta, "another-supervisor", groundhog.RunnerRunning}, // owned, runner alive
		{"echo", f.echo, "stopped-supervisor", groundhog.RunnerStopped},   // owned, runner not alive
	} {
		before := w.status(c.inst)
		if before.Owner == nil || before.Owner.Label != c.label || before.Runner.State != c.runner {
			t.Fatalf("%s: setup is not as described: owner %+v, runner %s", c.rel, before.Owner, before.Runner.State)
		}
		w.selectInstance(c.rel)
		w.reread(c.rel)
		n := w.node(c.rel)
		if n.State != tui.StateExternal || hasAction(n, tui.ActionRun) || hasAction(n, tui.ActionStop) {
			t.Errorf("%s: row state %v with actions %v; want external and no Run or Stop", c.rel, n.State, n.Actions)
		}
		// Liveness does not matter: a stopped owner is no more Easyhog's to take.
		if w.offers("run") || w.offers("stop") {
			t.Errorf("%s: the palette offers control of an instance another supervisor owns (runner %s)", c.rel, c.runner)
		}
		w.palette("run") // typed anyway: nothing matches, nothing runs, the palette stays open
		if !strings.Contains(w.screen(), "nothing matches") {
			t.Errorf("%s: typing run and Enter found something to run\n%s", c.rel, w.screen())
		}
		w.send(press(tea.KeyEsc))
		w.show(c.rel + " after typing run")
		for _, s := range []string{"Kind: externally owned", "another supervisor", "Easyhog only inspects it"} {
			if !strings.Contains(w.flat(), s) {
				t.Errorf("%s: the preview does not say %q\n%s", c.rel, s, w.screen())
			}
		}
		// Asked directly, the supervisor refuses, with the reason in words.
		_, err := w.sup.Run(w.ctx, c.inst)
		var refused *core.RefusedError
		if !errors.As(err, &refused) || refused.Reason != core.RefusedExternal {
			t.Errorf("%s: Run returned %v, want a refusal because another supervisor owns it", c.rel, err)
		}
		if text := tui.ActionFailure(c.rel+"/.groundhog", err); text == nil || !strings.Contains(text.Error(), "owned by another supervisor") {
			t.Errorf("%s: the refusal reads %v", c.rel, text)
		}
		if _, err := w.sup.Stop(w.ctx, c.inst); !errors.Is(err, core.ErrRefused) {
			t.Errorf("%s: Stop returned %v, want a refusal", c.rel, err)
		}
		if _, ok := w.sup.Runner(c.inst); ok {
			t.Errorf("%s: Easyhog's supervisor holds a runner for it", c.rel)
		}
		if w.easy.startsFor(c.inst) != 0 {
			t.Errorf("%s: Easyhog started a runner there", c.rel)
		}
		after := w.status(c.inst)
		if after.Owner == nil || after.Owner.ID != before.Owner.ID || after.Owner.Generation != before.Owner.Generation ||
			after.Runner.State != before.Runner.State || after.Paused != before.Paused {
			t.Errorf("%s changed: owner %+v -> %+v, runner %s -> %s, paused %v -> %v", c.rel,
				before.Owner, after.Owner, before.Runner.State, after.Runner.State, before.Paused, after.Paused)
		}
	}
	if n := runnerProcesses(t, f.delta); n != 1 {
		t.Errorf("delta has %d runner processes, want the other supervisor's one", n)
	}
	if n := runnerProcesses(t, f.echo); n != 0 {
		t.Errorf("echo has %d runner processes, want none", n)
	}
}

func (w *world) noDuplicateOwners(t *testing.T, f *fixture) {
	// Every instance Easyhog runs has exactly one runner process and one owner.
	for rel, inst := range map[string]string{"alpha": f.alpha, "team/bravo": f.bravo, "charlie": f.charlie} {
		if n := runnerProcesses(t, inst); n != 1 {
			t.Errorf("%s has %d runner processes, want 1", rel, n)
		}
		// One runner per Run: bravo was stopped and run again, so it had two in all.
		want := 1
		if inst == f.bravo {
			want = 2
		}
		if got := w.easy.startsFor(inst); got != want {
			t.Errorf("%s: Easyhog started %d runners, want %d", rel, got, want)
		}
	}
	// A second supervisor cannot take an instance Easyhog owns, however its runner is.
	for _, inst := range []string{f.alpha, f.bravo} {
		before := w.status(inst)
		_, err := w.osup.Run(w.ctx, inst)
		var refused *core.RefusedError
		if !errors.As(err, &refused) || refused.Reason != core.RefusedExternal {
			t.Errorf("another supervisor's Run on %s returned %v, want a refusal", inst, err)
		}
		after := w.status(inst)
		if before.Owner == nil || after.Owner == nil || before.Owner.ID != after.Owner.ID {
			t.Errorf("%s: ownership changed: %+v -> %+v", inst, before.Owner, after.Owner)
		}
		if n := runnerProcesses(t, inst); n != 1 {
			t.Errorf("%s has %d runner processes after the other supervisor tried", inst, n)
		}
	}
	// The protocol itself refuses a second enroll on an owned instance.
	err := w.client(f.alpha).Enroll(w.ctx, "intruder", filepath.Join(w.tmp, "other-tokens", "intruder.token"))
	if err == nil {
		t.Errorf("a second enroll on an owned instance succeeded")
	}
	_ = os.Remove(filepath.Join(w.tmp, "other-tokens", "intruder.token"))
}

func (w *world) visibleErrors(t *testing.T, f *fixture) {
	// Refresh again with failures all around: each is reported where the reader
	// looks, and the healthy instances are exactly as healthy as before.
	began := time.Now()
	w.palette("refresh")
	w.until("Refresh to read every well-formed instance again", 60*time.Second, func() bool {
		for _, rel := range []string{"alpha", "team/bravo", "charlie", "delta", "echo"} {
			if l := w.view(rel).Inspection.Last; l == nil || !l.TakenAt.After(began) {
				return false
			}
		}
		return w.view("golf").Inspection.ErrAt.After(began)
	})
	for rel, want := range map[string]core.Class{"alpha": core.ClassOwnedBySession, "team/bravo": core.ClassOwnedBySession,
		"charlie": core.ClassOwnedBySession, "delta": core.ClassExternal, "echo": core.ClassExternal,
		"foxtrot": core.ClassLegacy, "golf": core.ClassFailing, "hotel": core.ClassMalformed} {
		if got := w.view(rel).Class; got != want {
			t.Errorf("%s reads as %s after a second Refresh, want %s", rel, got, want)
		}
	}
	for _, inst := range []string{f.alpha, f.bravo, f.charlie} {
		if st := w.status(inst); st.Runner.State != groundhog.RunnerRunning {
			t.Errorf("%s: runner %s", inst, st.Runner.State)
		}
	}
	w.selectRow("golf")
	if !strings.Contains(w.flat(), "status is unavailable") || !strings.Contains(w.flat(), "Kind: failing") {
		t.Errorf("golf's failure is not shown\n%s", w.screen())
	}
	if os.Geteuid() != 0 {
		w.selectRow("private")
		w.show("the unreadable directory")
		for _, s := range []string{"Could not inspect", "permission denied"} {
			if !strings.Contains(w.screen(), s) {
				t.Errorf("the unreadable directory's preview does not say %q\n%s", s, w.screen())
			}
		}
	}
	w.selectRow("charlie")
	if !strings.Contains(w.screen(), "1 failed") {
		t.Errorf("charlie's failed handoff is no longer shown\n%s", w.screen())
	}
}

// agentPID reads the pid from the fake agent's marker file name.
func (x *wakeFixture) agentPID(t *testing.T) int {
	t.Helper()
	marks, _ := filepath.Glob(filepath.Join(x.marks, "agent.*"))
	if len(marks) != 1 {
		t.Fatalf("want one agent marker, got %v", marks)
	}
	pid, err := strconv.Atoi(strings.TrimPrefix(filepath.Base(marks[0]), "agent."))
	if err != nil {
		t.Fatal(err)
	}
	return pid
}

func (w *world) wakeInPlace(t *testing.T, f *fixture) {
	x := f.wake
	w.selectInstance("wake")
	w.palette("run")
	w.show("wake after Run")
	if want := "Run: wake: enrolled for this session, runner started"; !strings.Contains(w.screen(), want) {
		t.Fatalf("the status line does not say %q\n%s", want, w.screen())
	}
	// The adapter waits for the pane to go quiet (2 s here), then respawns it.
	w.await("the wake delivery to be accepted", 90*time.Second, func() bool {
		return countState(w.status(f.wakeInst), groundhog.DeliveryAccepted) == 1
	})
	st := w.status(f.wakeInst)
	d := st.Deliveries[0]
	if len(d.Attempts) != 1 || d.Attempts[0].ExitCode == nil || *d.Attempts[0].ExitCode != 0 || d.Execution != "unknown" {
		t.Errorf("the wake delivery: %d attempts, exit %v, execution %q", len(d.Attempts), d.Attempts[0].ExitCode, d.Execution)
	}
	w.await("the fake agent to start in the pane", 20*time.Second, func() bool {
		m, _ := filepath.Glob(filepath.Join(x.marks, "agent.*"))
		return len(m) == 1
	})
	marker, _ := filepath.Glob(filepath.Join(x.marks, "agent.*"))
	body, _ := os.ReadFile(marker[0])
	lines := strings.Split(string(body), "\n")
	if lines[0] != filepath.Dir(f.wakeInst) {
		t.Errorf("the agent ran in %q, want the repository %q", lines[0], filepath.Dir(f.wakeInst))
	}
	if len(lines) < 2 || !strings.Contains(lines[1], "resume prompt in ") {
		t.Errorf("the agent was not given a prompt path: %q", lines)
	}
	if !x.hasPane() {
		t.Errorf("the pane %s did not survive; panes: %v", x.pane, x.panes())
	}
	// Nothing about the payload's agent, pane or tmux fields reached Groundhog's
	// own state: the adapter keeps its record under its own directory.
	if m, _ := filepath.Glob(filepath.Join(x.state, "*", *d.ID, "launched")); len(m) != 1 {
		t.Errorf("the adapter's launch record for %s is missing", *d.ID)
	}
	if _, err := os.Stat(filepath.Join(f.wakeInst, "runtime", "wake-consumer")); err == nil {
		t.Errorf("the adapter wrote under Groundhog's runtime/")
	}
	w.reread("wake")
	if n := w.node("wake"); n.State != tui.StateRunning || !hasAction(n, tui.ActionStop) {
		t.Errorf("wake's row: state %v, actions %v", n.State, n.Actions)
	}
}

func (w *world) shutdown(t *testing.T, f *fixture) {
	ours := map[string]string{"alpha": f.alpha, "team/bravo": f.bravo, "charlie": f.charlie}
	if f.wake != nil {
		ours["wake"] = f.wakeInst
	}
	var agent int
	if f.wake != nil {
		agent = f.wake.agentPID(t)
	}
	// Something outside Easyhog pauses bravo before the quit (Easyhog sets no
	// pause itself): Quit must leave that pause as it is, and must not add one to
	// the instances that were running.
	info, ok := w.sup.Runner(f.bravo)
	if !ok {
		t.Fatal("the supervisor holds nothing for bravo, which Run enrolled again")
	}
	if err := w.client(f.bravo).Pause(w.ctx, info.TokenFile); err != nil {
		t.Fatal(err)
	}
	if !w.status(f.bravo).Paused {
		t.Fatal("bravo is not paused before Quit")
	}
	before := map[string]groundhog.Status{}
	for rel, inst := range ours {
		before[rel] = w.status(inst)
	}

	// The palette's Quit ends the shell; main then runs the one Supervisor.Quit.
	w.selectInstance("alpha")
	if !w.palette("quit") {
		t.Fatal("the palette's Quit did not quit")
	}
	ctx, cancel := context.WithTimeout(w.ctx, 120*time.Second)
	defer cancel()
	rep := w.sup.Quit(ctx)
	if !rep.Clean() || len(rep.Notices) != 0 {
		t.Fatalf("quit report: %+v", rep)
	}
	// Quit lists every instance a Run touched. Those it enrolled are released;
	// one a refused Run touched (delta and echo: another owner's) is "not owned"
	// and nothing was done to it, as the checks below confirm.
	ownedHere := map[string]bool{}
	for _, inst := range ours {
		ownedHere[inst] = true
	}
	released := 0
	for _, in := range rep.Instances {
		switch {
		case ownedHere[in.Instance]:
			released++
			if in.Outcome != core.QuitReleased || !in.RunnerGone {
				t.Errorf("%s: %v, runner gone %v: %s", in.Instance, in.Outcome, in.RunnerGone, in.Notice)
			}
		case in.Outcome != core.QuitNotOwned || in.Notice != "":
			t.Errorf("%s was not enrolled here, yet the report says %v %q", in.Instance, in.Outcome, in.Notice)
		}
	}
	if released != len(ours) {
		t.Fatalf("quit report released %d of %d instances: %+v", released, len(ours), rep)
	}
	for rel, inst := range ours {
		st := w.status(inst)
		if st.Owner != nil || st.Runner.State == groundhog.RunnerRunning {
			t.Errorf("%s after Quit: owner %+v, runner %s", rel, st.Owner, st.Runner.State)
		}
		// Quit never touches the pause: it stays as the reader left it, so an
		// instance paused by something else before Quit stays paused and the others stay unpaused.
		if st.Paused != before[rel].Paused {
			t.Errorf("%s: paused %v before Quit, %v after; Quit must leave the pause alone", rel, before[rel].Paused, st.Paused)
		}
		if n := runnerProcesses(t, inst); n != 0 {
			t.Errorf("%s: %d runner processes after Quit", rel, n)
		}
		// Work handed off or still waiting is not undone.
		if a, b := countState(st, groundhog.DeliveryAccepted), countState(before[rel], groundhog.DeliveryAccepted); a < b {
			t.Errorf("%s: accepted deliveries fell from %d to %d", rel, b, a)
		}
		if a, b := countState(st, groundhog.DeliveryPending), countState(before[rel], groundhog.DeliveryPending); a != b {
			t.Errorf("%s: pending deliveries changed from %d to %d", rel, b, a)
		}
	}
	if sessions, _ := filepath.Glob(filepath.Join(w.tmp, "easy-state", "sessions", "*")); len(sessions) != 0 {
		t.Errorf("tokens or logs left behind: %v", sessions)
	}
	// What another supervisor owns is not Easyhog's to release.
	if st := w.status(f.delta); st.Owner == nil || st.Owner.Label != "another-supervisor" || st.Runner.State != groundhog.RunnerRunning {
		t.Errorf("delta after Easyhog quit: owner %+v, runner %s", st.Owner, st.Runner.State)
	}
	if st := w.status(f.echo); st.Owner == nil || st.Owner.Label != "stopped-supervisor" {
		t.Errorf("echo after Easyhog quit: owner %+v", st.Owner)
	}
	if n := runnerProcesses(t, f.delta); n != 1 {
		t.Errorf("delta has %d runner processes after Easyhog quit", n)
	}
	// The agent the adapter launched is not a child of the runner or of Easyhog.
	if f.wake != nil {
		if err := syscall.Kill(agent, 0); err != nil {
			t.Errorf("the launched agent (pid %d) did not survive Quit: %v", agent, err)
		}
		if !f.wake.hasPane() {
			t.Errorf("the agent's pane is gone after Quit")
		}
	}
	// Anything further is refused.
	if _, err := w.sup.Run(w.ctx, f.alpha); !errors.Is(err, core.ErrRefused) {
		t.Errorf("Run after Quit returned %v, want a refusal", err)
	}
}

func (w *world) reopen(t *testing.T, f *fixture) {
	// A new session over the same root. Nothing starts by itself: the instance
	// is unowned and nothing runs until the reader runs again.
	w.closeSession()
	w.openSession()
	w.selectInstance("alpha")
	w.show("alpha after reopening")
	time.Sleep(2500 * time.Millisecond) // longer than a runner cycle
	if n := runnerProcesses(t, f.alpha); n != 0 {
		t.Errorf("reopening started %d runners on alpha", n)
	}
	st := w.status(f.alpha)
	if st.Owner != nil || st.Paused || countState(st, groundhog.DeliveryPending) != 2 {
		t.Errorf("alpha after reopening: owner %+v, paused %v, %d pending", st.Owner, st.Paused, countState(st, groundhog.DeliveryPending))
	}
	if n := w.node("alpha"); n.State != tui.StateStopped || !hasAction(n, tui.ActionRun) || strings.Contains(n.Badge, "paused") {
		t.Errorf("alpha's row after reopening: state %v, actions %v, badge %q", n.State, n.Actions, n.Badge)
	}
	// The reader's explicit Run enrolls again; Quit left no pause to clear.
	w.palette("run")
	if want := "enrolled for this session, runner started"; !strings.Contains(w.screen(), want) {
		t.Errorf("the status line does not say %q\n%s", want, w.screen())
	}
	if strings.Contains(w.screen(), "a pause was cleared") {
		t.Errorf("Run cleared a pause that Quit should not have set\n%s", w.screen())
	}
	if st := w.status(f.alpha); st.Paused || st.Owner == nil || st.Runner.State != groundhog.RunnerRunning {
		t.Errorf("alpha after Run: paused %v, owner %+v, runner %s", st.Paused, st.Owner, st.Runner.State)
	}
	// delta is still refused in the new session.
	if _, err := w.sup.Run(w.ctx, f.delta); !errors.Is(err, core.ErrRefused) {
		t.Errorf("the new session's Run on delta returned %v", err)
	}
}

// hideUnsupported hides the malformed and legacy instances again, with a legacy
// one highlighted. The highlight moves to a visible neighbour, the count says 2,
// and nothing is run for it: the legacy script's call log does not grow and the
// refresher reads no instance it had not read.
func (w *world) hideUnsupported(t *testing.T, f *fixture) {
	w.selectInstance("foxtrot")
	logPath := filepath.Join(filepath.Dir(f.foxtrot), "calls.log")
	before, err := os.ReadFile(logPath)
	if err != nil {
		t.Fatal(err)
	}
	last := map[string]time.Time{}
	for _, iv := range w.ref.View().Instances {
		if iv.Inspection.Last != nil {
			last[iv.Instance.Display] = iv.Inspection.Last.TakenAt
		}
	}
	w.palette("hide unsupported")
	time.Sleep(500 * time.Millisecond) // several settle delays: a reported selection would read now
	w.sync()
	screen := w.screen()
	for _, rel := range []string{"foxtrot", "hotel"} {
		if strings.Contains(screen, rel) {
			t.Errorf("%s is still listed after hiding\n%s", rel, screen)
		}
	}
	if !strings.Contains(screen, "2 hidden") {
		t.Errorf("the hidden count is not 2\n%s", screen)
	}
	if got := w.selected(); got == "" || got == "foxtrot" {
		t.Errorf("the highlight is on %q after its instance hid\n%s", got, screen)
	}
	if after, _ := os.ReadFile(logPath); string(after) != string(before) {
		t.Errorf("hiding ran the legacy script again:\n%s", after)
	}
	for _, iv := range w.ref.View().Instances {
		if iv.Inspection.Last != nil && !iv.Inspection.Last.TakenAt.Equal(last[iv.Instance.Display]) {
			// Only the selected instance's periodic read may move; a neighbour
			// that was highlighted by the move must not be the reason.
			if w.selected() != iv.Instance.Display {
				t.Errorf("%s was read after hiding", iv.Instance.Display)
			}
		}
	}
	w.palette("show hidden")
}
