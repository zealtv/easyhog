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

// TestMVPAcceptance is one reader's session over a temporary directory of
// synthetic repositories, through the headless core and the Bubble Tea model,
// with real ticks of the pinned Groundhog script. The steps run in order and
// share the world; a step that fails stops the rest, since later steps build
// on earlier ones.
//
// The claims it checks are Easyhog's product rules and the pinned Groundhog
// script's observed behavior. A newer upstream that disagrees means the pin
// moves deliberately, with this test.
func TestMVPAcceptance(t *testing.T) {
	w := newWorld(t, gate(t))
	f := w.build(t)
	w.start()

	step := func(name string, fn func(t *testing.T)) {
		t.Helper()
		if !t.Run(name, fn) {
			t.Fatalf("stopping: step %q failed, and the steps after it depend on it", name)
		}
	}
	step("discover without running anything", func(t *testing.T) { w.discover(t, f) })
	step("refresh classifies every instance", func(t *testing.T) { w.classify(t, f) })
	step("expand and inspect without running payloads", func(t *testing.T) { w.expandAndInspect(t, f) })
	step("run an instance whose items have no run: delivery only", func(t *testing.T) { w.runAlpha(t, f) })
	step("run an instance whose items carry their run: each accepted once", func(t *testing.T) { w.runBravo(t, f) })
	step("pause and unpause the instance from the palette", func(t *testing.T) { w.pauseBravo(t, f) })
	step("a failed run is visible, and Retry runs it again", func(t *testing.T) { w.failAndRetry(t, f) })
	step("a retry of a run still going says busy", func(t *testing.T) { w.busyRetry(t, f) })
	step("an instance ticked from outside is shown, and Run asks nothing", func(t *testing.T) { w.outside(t, f) })
	step("errors stay visible beside healthy instances", func(t *testing.T) { w.visibleErrors(t, f) })
	step("hiding unsupported instances runs nothing and keeps the highlight sensible", func(t *testing.T) { w.hideUnsupported(t, f) })
	step("stop ends this session's ticking", func(t *testing.T) { w.stopAlpha(t, f) })
	step("quit stops ticking and leaves a run still going to finish", func(t *testing.T) { w.quit(t, f) })
	step("reopening ticks nothing until Run", func(t *testing.T) { w.reopen(t, f) })
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

func (w *world) key(rel string) string { return filepath.Join(w.root, rel, ".groundhog") }

func (w *world) ticking(inst string) bool {
	info, _ := w.tk.Info(inst)
	return info.Ticking
}

func (w *world) discover(t *testing.T, f *fixture) {
	w.show("first screen")
	screen := w.screen()
	for _, rel := range []string{"alpha", "team/bravo", "charlie", "delta", "foxtrot", "india", "golf"} {
		if !strings.Contains(screen, rel) {
			t.Errorf("the first screen does not list %s\n%s", rel, screen)
		}
	}
	// Malformed is known from listings alone, so it starts hidden and counted;
	// the legacy scripts are not known until they are negotiated, so they are listed.
	if strings.Contains(screen, "hotel") || !strings.Contains(screen, "1 hidden") {
		t.Errorf("the malformed instance is listed by default, or the hidden count is missing\n%s", screen)
	}
	if os.Geteuid() != 0 && !strings.Contains(screen, "private") {
		t.Errorf("the unreadable directory is not reported\n%s", screen)
	}
	// Nothing has ticked: no delivery anywhere but delta's, ticked from outside.
	for _, inst := range []string{f.alpha, f.bravo, f.charlie} {
		if entries, _ := os.ReadDir(filepath.Join(inst, "out")); len(entries) != 0 || w.ticking(inst) {
			t.Errorf("%s: %d deliveries, or ticking, before any Run", inst, len(entries))
		}
	}
	// "Wait for a keypress before running any script": after several settle
	// delays idling has read none of them.
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
	for _, inst := range []string{f.foxtrot, f.india} {
		if _, err := os.Stat(filepath.Join(filepath.Dir(inst), "calls.log")); err == nil {
			t.Errorf("%s: a legacy script was run before any Refresh", inst)
		}
	}
	// The palette shows what is hidden; showing is a view action and runs nothing.
	w.palette("show hidden")
	if !strings.Contains(w.screen(), "hotel") || w.view("hotel").Class != core.ClassMalformed {
		t.Errorf("show hidden did not list the malformed instance\n%s", w.screen())
	}
	if _, err := os.Stat(filepath.Join(filepath.Dir(f.foxtrot), "calls.log")); err == nil {
		t.Errorf("showing hidden instances ran the legacy script")
	}
}

// classes is what each repository must read as once Refresh has run every
// well-formed script.
var classes = map[string]core.Class{
	"alpha": core.ClassSupported, "team/bravo": core.ClassSupported, "charlie": core.ClassSupported,
	"delta": core.ClassSupported, "foxtrot": core.ClassLegacy, "india": core.ClassLegacy,
	"golf": core.ClassFailing, "hotel": core.ClassMalformed,
}

func (w *world) classify(t *testing.T, f *fixture) {
	w.palette("refresh")
	w.until("every instance to be classified by Refresh", 60*time.Second, func() bool {
		for rel, class := range classes {
			if w.view(rel).Class != class {
				return false
			}
		}
		return true
	})
	// Run is offered for every supported instance, the one ticked from outside
	// included, and for nothing else; Stop for none, since this session ticks nothing yet.
	for rel, class := range classes {
		w.selectInstance(rel)
		if got, want := w.offers("run"), class == core.ClassSupported; got != want {
			t.Errorf("%s (%s): palette offers Run = %v, want %v\n%s", rel, class, got, want, w.screen())
		}
		if w.offers("stop") {
			t.Errorf("%s (%s): the palette offers Stop before anything is ticked", rel, class)
		}
	}
	// The legacy scripts were asked only `capabilities --json`; the malformed
	// one was never run at all.
	for _, inst := range []string{f.foxtrot, f.india} {
		calls, err := os.ReadFile(filepath.Join(filepath.Dir(inst), "calls.log"))
		if err != nil {
			t.Fatalf("%s was never asked anything: %v", inst, err)
		}
		for _, line := range strings.Split(strings.TrimSpace(string(calls)), "\n") {
			if line != "capabilities --json" {
				t.Errorf("%s was run with %q", inst, line)
			}
		}
	}
	if _, err := os.Stat(filepath.Join(filepath.Dir(f.hotel), "calls.log")); err == nil {
		t.Errorf("the malformed instance's script was run")
	}
	// Errors and read-only instances say why, in words.
	for rel, text := range map[string][]string{
		"foxtrot": {"legacy", "read-only"},
		"india":   {"legacy script, read-only", "an older Groundhog (runtime protocol 1"},
		"golf":    {"failing", "status is unavailable"},
		"hotel":   {"malformed"},
	} {
		w.selectInstance(rel)
		w.show(rel)
		for _, s := range text {
			if !w.said(s) {
				t.Errorf("%s: the screen does not say %q\n%s", rel, s, w.screen())
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
		"day-note":    {"Cadence: daily", "alpha daily note"},
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
	w.selectRow("alpha")
	w.send(press(tea.KeyLeft))
	for _, s := range []string{"Kind: supported", "Palette offers: Run, Pause instance", "Deliveries: none", "Pause: not paused"} {
		if !strings.Contains(w.screen(), s) {
			t.Errorf("alpha: the preview does not show %q\n%s", s, w.screen())
		}
	}
	// What an item does is its run, shown first, as text: at the top of the
	// payload, before the item's other files (note.txt falls below the fold).
	w.selectInstance("team/bravo")
	w.send(press(tea.KeyRight))
	w.selectRow("b1")
	time.Sleep(60 * time.Millisecond)
	w.sync()
	for _, s := range []string{"Runs when the item fires", "#!/bin/sh", "A synchronous, non-AI Groundhog run"} {
		if !w.said(s) {
			t.Errorf("b1: the preview does not show %q\n%s", s, w.screen())
		}
	}
	w.selectRow("team/bravo")
	w.send(press(tea.KeyLeft))
	if _, err := os.Stat(filepath.Join(w.tmp, "canary")); err == nil {
		t.Errorf("inspecting a payload ran it")
	}
	if entries, _ := os.ReadDir(filepath.Join(f.bravo, "out")); len(entries) != 0 {
		t.Errorf("inspecting bravo fired %d items", len(entries))
	}
}

func (w *world) runAlpha(t *testing.T, f *fixture) {
	w.selectInstance("alpha")
	w.palette("run")
	if !w.said("Run: alpha: ticked, and ticking every minute") {
		t.Fatalf("Run's notice\n%s", w.screen())
	}
	w.await("alpha's overdue item to be delivered", 20*time.Second, func() bool {
		return len(deliveriesOf(w.status(f.alpha), "old-note")) == 1
	})
	st := w.status(f.alpha)
	d := deliveriesOf(st, "old-note")[0]
	if d.State != groundhog.DeliveryDelivered || d.Run || len(d.Attempts) != 0 {
		t.Errorf("an item with no run: %+v", d)
	}
	if n := len(deliveriesOf(st, "future-note")); n != 0 {
		t.Errorf("the future item fired (%d)", n)
	}
	if st.LastTick == nil || st.LastTick.Source != "standalone" {
		t.Errorf("last_tick %+v", st.LastTick)
	}
	w.reread("alpha")
	if n := w.node("alpha"); n.State != tui.StateRunning || !w.offers("stop") || w.offers("run") {
		t.Errorf("alpha after Run: %v %q\n%s", n.State, n.Detail, w.screen())
	}
	// Run again is not offered, and asked anyway it changes nothing.
	if res, err := w.tk.Run(w.ctx, f.alpha); err != nil || !res.Already {
		t.Errorf("a second Run: %+v %v", res, err)
	}
}

func (w *world) bravoNotes() string {
	b, _ := os.ReadFile(filepath.Join(w.root, "team", "bravo", "notes.log"))
	return string(b)
}

func (w *world) runBravo(t *testing.T, f *fixture) {
	w.selectInstance("team/bravo")
	w.palette("run")
	w.await("bravo's four runs to be accepted", 30*time.Second, func() bool {
		return countState(w.status(f.bravo), groundhog.DeliveryAccepted) == 4
	})
	notes := w.bravoNotes()
	for _, n := range []string{"bravo note b1", "bravo note b2", "bravo note b3", "bravo daily note"} {
		if strings.Count(notes, n) != 1 {
			t.Errorf("%q appears %d times in notes.log:\n%s", n, strings.Count(notes, n), notes)
		}
	}
	// More ticks start nothing again: each run started once.
	time.Sleep(2500 * time.Millisecond)
	for _, d := range w.status(f.bravo).Deliveries {
		if len(d.Attempts) != 1 || d.Attempts[0].By != "tick" {
			t.Errorf("%s: %d attempts", d.Path, len(d.Attempts))
		}
	}
	w.reread("team/bravo")
	if n := w.node("team/bravo"); n.State != tui.StateRunning || n.Heartbeat != core.HeartbeatFresh {
		t.Errorf("bravo: state %v heartbeat %v", n.State, n.Heartbeat)
	}
	if !strings.Contains(w.flat(), "accepted") {
		t.Errorf("the preview does not count the accepted runs\n%s", w.screen())
	}
}

func (w *world) pauseBravo(t *testing.T, f *fixture) {
	w.selectInstance("team/bravo")
	w.palette("pause in")
	if !w.said("Pause instance: team/bravo: paused") {
		t.Fatalf("the pause's notice\n%s", w.screen())
	}
	if !w.status(f.bravo).Paused {
		t.Fatal("bravo is not paused")
	}
	w.reread("team/bravo")
	if n := w.node("team/bravo"); n.State != tui.StatePaused || !strings.Contains(n.Detail, "this session still ticks it") || !w.offers("unpause") {
		t.Errorf("paused: %v %q\n%s", n.State, n.Detail, w.screen())
	}
	// A new item comes due while paused: ticks go on, and fire nothing.
	w.item(f.bravo, overdue, "b4", "bravo note b4")
	w.run(f.bravo, overdue, "b4", w.example("collect-note.sh"))
	time.Sleep(2500 * time.Millisecond)
	if n := len(deliveriesOf(w.status(f.bravo), "b4")); n != 0 {
		t.Fatalf("b4 fired while bravo was paused (%d)", n)
	}
	w.palette("unpause")
	if !w.said("Unpause instance: team/bravo: unpaused") {
		t.Fatalf("the unpause's notice\n%s", w.screen())
	}
	w.await("b4 to run once the pause is lifted", 20*time.Second, func() bool {
		d := deliveriesOf(w.status(f.bravo), "b4")
		return len(d) == 1 && d[0].State == groundhog.DeliveryAccepted
	})
	if !strings.Contains(w.bravoNotes(), "bravo note b4") {
		t.Errorf("b4's note did not arrive:\n%s", w.bravoNotes())
	}
}

func (w *world) charlieDelivery(id string) groundhog.Delivery {
	w.t.Helper()
	ds := deliveriesOf(w.status(w.key("charlie")), id)
	if len(ds) != 1 {
		w.t.Fatalf("charlie %s: %d deliveries", id, len(ds))
	}
	return ds[0]
}

func (w *world) failAndRetry(t *testing.T, f *fixture) {
	w.selectInstance("charlie")
	w.palette("run")
	w.await("both of charlie's runs to fail", 20*time.Second, func() bool {
		return countState(w.status(f.charlie), groundhog.DeliveryFailed) == 2
	})
	w.reread("charlie")
	n := w.node("charlie")
	if n.State != tui.StateFailed || !strings.Contains(n.Badge, "2 failed") || !strings.Contains(n.Detail, "2 deliveries failed, running") {
		t.Errorf("charlie: %v %q %q", n.State, n.Detail, n.Badge)
	}
	if !strings.Contains(w.flat(), "✗ failed") || !strings.Contains(w.flat(), "Palette offers: Stop, Pause instance, Retry") {
		t.Errorf("the preview does not show the failure or offer Retry\n%s", w.screen())
	}
	// Groundhog does not retry by itself: more ticks leave c1 at one attempt.
	time.Sleep(1500 * time.Millisecond)
	if d := w.charlieDelivery("c1"); len(d.Attempts) != 1 {
		t.Fatalf("c1 was started again without Retry: %+v", d.Attempts)
	}
	// Its run's output is in the item's preview, read as text, below the run
	// itself: the reader scrolls the preview (Tab, then a page down) to it.
	w.send(press(tea.KeyRight))
	w.selectRow("c1")
	time.Sleep(60 * time.Millisecond)
	w.sync()
	w.send(press(tea.KeyTab), press(tea.KeyPgDown), press(tea.KeyPgDown))
	if !w.said("Output of a run") || !w.said("failing on purpose") {
		t.Errorf("c1's preview does not show its run's output\n%s", w.screen())
	}
	w.send(press(tea.KeyTab)) // back to the tree
	// Fixed: Retry on the item starts that item's run again, as a new attempt.
	w.must(os.WriteFile(filepath.Join(filepath.Dir(f.charlie), "fixed"), nil, 0o644))
	w.palette("retry")
	if !w.said("Retry: charlie: 1 run started again") {
		t.Fatalf("Retry's notice\n%s", w.screen())
	}
	w.await("c1 to be accepted on its second attempt", 20*time.Second, func() bool {
		return w.charlieDelivery("c1").State == groundhog.DeliveryAccepted
	})
	if d := w.charlieDelivery("c1"); len(d.Attempts) != 2 || d.Attempts[1].By != "retry" {
		t.Errorf("c1: %+v", d.Attempts)
	}
	if d := w.charlieDelivery("c2"); d.State != groundhog.DeliveryFailed || len(d.Attempts) != 1 {
		t.Errorf("Retry on c1 touched c2: %+v", d)
	}
	attempts, _ := os.ReadFile(filepath.Join(filepath.Dir(f.charlie), "attempts.log"))
	if lines := strings.Split(strings.TrimSpace(string(attempts)), "\n"); len(lines) != 2 ||
		strings.Fields(lines[0])[0] != strings.Fields(lines[1])[0] || strings.Fields(lines[0])[1] == strings.Fields(lines[1])[1] {
		t.Errorf("want two attempts of one delivery, with two attempt ids:\n%s", attempts)
	}
	w.selectRow("charlie")
	w.send(press(tea.KeyLeft))
}

func (w *world) busyRetry(t *testing.T, f *fixture) {
	w.selectInstance("charlie")
	w.send(press(tea.KeyRight))
	w.selectRow("c2")
	w.sync()
	w.palette("retry")
	w.await("c2's second attempt to be running", 20*time.Second, func() bool {
		return w.charlieDelivery("c2").State == groundhog.DeliveryRunning
	})
	// The screen has not caught up yet: it still shows c2 failed, so the reader
	// can ask for Retry again. The ticker reads afresh and says busy.
	w.paletteAsSeen("retry")
	if !w.said("busy: nothing was started for") || !w.said("its run is going, or the instance's lock was held") {
		t.Fatalf("a retry of a run still going did not say busy\n%s", w.screen())
	}
	if d := w.charlieDelivery("c2"); len(d.Attempts) != 2 {
		t.Errorf("the busy retry started an attempt: %+v", d.Attempts)
	}
	w.reread("charlie")
	if n := w.node("charlie"); !strings.Contains(n.Badge, "1 running") {
		t.Errorf("charlie's badge %q does not count the running run", n.Badge)
	}
	w.selectRow("charlie")
	w.send(press(tea.KeyLeft))
}

func (w *world) outside(t *testing.T, f *fixture) {
	w.selectInstance("delta")
	n := w.node("delta")
	if n.State != tui.StateExternal || !strings.HasPrefix(n.Detail, "ticked from outside Easyhog") || n.Heartbeat == core.HeartbeatNone {
		t.Errorf("delta: %v %q heartbeat %v", n.State, n.Detail, n.Heartbeat)
	}
	if !strings.Contains(w.flat(), "Pending work: probably, something outside Easyhog ticked it") || !w.offers("run") {
		t.Errorf("delta's preview or palette\n%s", w.screen())
	}
	// Run asks nothing: ticking it here as well is harmless. Stop again at once.
	w.palette("run")
	if !w.said("Run: delta: ticked") || strings.Contains(w.screen(), "Run anyway?") {
		t.Fatalf("Run on delta\n%s", w.screen())
	}
	w.palette("stop")
	if w.ticking(f.delta) {
		t.Error("delta still ticked here after Stop")
	}
}

func (w *world) visibleErrors(t *testing.T, f *fixture) {
	began := time.Now()
	w.palette("refresh")
	w.until("Refresh to read every well-formed instance again", 60*time.Second, func() bool {
		for _, rel := range []string{"alpha", "team/bravo", "charlie", "delta"} {
			if l := w.view(rel).Inspection.Last; l == nil || !l.TakenAt.After(began) {
				return false
			}
		}
		return w.view("golf").Inspection.ErrAt.After(began)
	})
	for rel, want := range classes {
		if got := w.view(rel).Class; got != want {
			t.Errorf("%s reads as %s after a second Refresh, want %s", rel, got, want)
		}
	}
	w.selectRow("golf")
	if !strings.Contains(w.flat(), "status is unavailable") || !strings.Contains(w.flat(), "Kind: failing") {
		t.Errorf("golf's failure is not shown\n%s", w.screen())
	}
	if os.Geteuid() != 0 {
		w.selectRow("private")
		for _, s := range []string{"Could not inspect", "permission denied"} {
			if !strings.Contains(w.screen(), s) {
				t.Errorf("the unreadable directory's preview does not say %q\n%s", s, w.screen())
			}
		}
	}
	if !w.ticking(f.alpha) || !w.ticking(f.bravo) || !w.ticking(f.charlie) {
		t.Error("an error elsewhere stopped this session's ticking")
	}
}

func (w *world) hideUnsupported(t *testing.T, f *fixture) {
	w.selectInstance("foxtrot")
	logPath := filepath.Join(filepath.Dir(f.foxtrot), "calls.log")
	before, err := os.ReadFile(logPath)
	if err != nil {
		t.Fatal(err)
	}
	w.palette("hide unsupported")
	time.Sleep(500 * time.Millisecond) // several settle delays: a reported selection would read now
	w.sync()
	screen := w.screen()
	for _, rel := range []string{"foxtrot", "india", "hotel"} {
		if strings.Contains(screen, rel) {
			t.Errorf("%s is still listed after hiding\n%s", rel, screen)
		}
	}
	if !strings.Contains(screen, "3 hidden") {
		t.Errorf("the hidden count is not 3\n%s", screen)
	}
	if got := w.selected(); got == "" || got == "foxtrot" {
		t.Errorf("the highlight is on %q after its instance hid\n%s", got, screen)
	}
	if after, _ := os.ReadFile(logPath); string(after) != string(before) {
		t.Errorf("hiding ran the legacy script again:\n%s", after)
	}
	w.palette("show hidden")
}

func (w *world) stopAlpha(t *testing.T, f *fixture) {
	w.selectInstance("alpha")
	w.palette("stop")
	if !w.said("Stop: alpha: stopped ticking") || w.ticking(f.alpha) {
		t.Fatalf("Stop\n%s", w.screen())
	}
	time.Sleep(1500 * time.Millisecond) // a tick under way has finished
	last := w.status(f.alpha).LastTick
	time.Sleep(2500 * time.Millisecond)
	if now := w.status(f.alpha).LastTick; now == nil || last == nil || !now.At.Equal(last.At) {
		t.Errorf("alpha was ticked after Stop: %+v then %+v", last, now)
	}
	// Its last tick was this session's own: the row reads stopped, not external.
	w.reread("alpha")
	if n := w.node("alpha"); n.State != tui.StateStopped || !w.offers("run") {
		t.Errorf("alpha after Stop: %v %q", n.State, n.Detail)
	}
}

func (w *world) quit(t *testing.T, f *fixture) {
	if d := w.charlieDelivery("c2"); d.State != groundhog.DeliveryRunning {
		t.Fatalf("setup: c2 is %s, not running", d.State)
	}
	if !w.palette("quit") {
		t.Fatal("the palette's Quit did not end the shell")
	}
	// main then runs shutdown, which is the ticker's Quit.
	began := time.Now()
	ctx, cancel := context.WithTimeout(w.ctx, 30*time.Second)
	defer cancel()
	if n := w.tk.Quit(ctx); n != 2 { // bravo and charlie; alpha and delta were stopped
		t.Errorf("Quit stopped %d instances, want 2", n)
	}
	if d := time.Since(began); d > 5*time.Second {
		t.Errorf("Quit took %s", d)
	}
	w.closeSession()
	if d := w.charlieDelivery("c2"); d.State != groundhog.DeliveryRunning {
		t.Errorf("after Quit c2 is %s; its run should still be going", d.State)
	}
	if _, err := w.tk.Run(w.ctx, f.alpha); err == nil {
		t.Error("Run worked after Quit")
	}
	w.must(os.WriteFile(f.gate, nil, 0o644))
	w.await("c2 to finish on its own after Quit", 20*time.Second, func() bool {
		return w.charlieDelivery("c2").State == groundhog.DeliveryAccepted
	})
	// No tick after Quit: last_tick stays where Quit left it.
	last := w.status(f.bravo).LastTick
	time.Sleep(2500 * time.Millisecond)
	if now := w.status(f.bravo).LastTick; now == nil || last == nil || !now.At.Equal(last.At) {
		t.Errorf("bravo was ticked after Quit: %+v then %+v", last, now)
	}
}

func (w *world) reopen(t *testing.T, f *fixture) {
	w.openSession()
	w.palette("refresh")
	w.until("the reopened session to read bravo", 30*time.Second, func() bool { return w.view("team/bravo").Inspection.Last != nil })
	if w.ticking(f.bravo) {
		t.Fatal("a reopened session ticks bravo by itself")
	}
	w.selectInstance("team/bravo")
	// The ended session's ticks are not this session's: within three minutes of
	// the last one, bravo reads as ticked from outside, and Run is offered.
	if n := w.node("team/bravo"); n.State != tui.StateExternal || !w.offers("run") {
		t.Errorf("bravo on reopening: %v %q", n.State, n.Detail)
	}
	w.palette("run")
	if !w.said("Run: team/bravo: ticked") || !w.ticking(f.bravo) {
		t.Errorf("Run after reopening\n%s", w.screen())
	}
}
