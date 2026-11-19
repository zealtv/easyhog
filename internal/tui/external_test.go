package tui

import (
	"strings"
	"testing"
	"time"

	tea "github.com/charmbracelet/bubbletea"
	"github.com/charmbracelet/x/ansi"

	"github.com/zealtv/easyhog/internal/core"
	"github.com/zealtv/easyhog/internal/groundhog"
)

// Tests for external-means-driven-from-outside: "external" covers two kinds, an
// instance enrolled elsewhere and an unowned one something outside Easyhog ticks,
// and Run on the second kind asks first.

func ownedElsewhere(name, label string) core.InstanceView {
	st := okStatus()
	st.Owner = &groundhog.Owner{ID: "o-1", Generation: 2, Label: label}
	return iv(name, core.ClassExternal, core.HealthOK, core.Inspection{Last: obs(st)})
}

func firstNode(t *testing.T, v core.InstanceView) Node {
	t.Helper()
	return FromViewWith(core.View{Root: "/r", Instances: []core.InstanceView{v}}, lookup(nil)).Nodes[0]
}

func TestExternalCoversTwoKindsAndReadsThemFromTheVerdict(t *testing.T) {
	paused := withTick(unowned("p"), time.Second, "standalone")
	st := paused.Inspection.Last.Status
	st.Paused = true
	paused.Inspection.Last = obs(st)
	cases := []struct {
		name   string
		v      core.InstanceView
		state  State
		detail string
	}{
		{"ticked from outside", withTick(unowned("a"), 45*time.Second, "standalone"), StateExternal, "ticked from outside Easyhog"},
		{"exactly at the window", withTick(unowned("a"), core.TickFreshness, "standalone"), StateExternal, "ticked from outside Easyhog"},
		{"one second past the window", withTick(unowned("a"), core.TickFreshness+time.Second, "standalone"), StateStopped, "not enrolled"},
		{"quiet for ten minutes", withTick(unowned("a"), 10*time.Minute, "standalone"), StateStopped, "not enrolled"},
		{"never ticked", unowned("a"), StateStopped, "not enrolled"},
		// A runner's own tick lingers for the window after it stops and releases.
		{"a released runner's tick", withTick(unowned("a"), 5*time.Second, "runner"), StateStopped, "not enrolled"},
		{"paused, so nothing ticks it", paused, StateStopped, "not enrolled"},
		{"enrolled elsewhere", ownedElsewhere("a", "team-supervisor"), StateExternal, "owned by team-supervisor"},
		{"enrolled elsewhere, ticked too", withTick(ownedElsewhere("a", "team-supervisor"), time.Second, "runner"), StateExternal, "owned by team-supervisor"},
		{"enrolled elsewhere, no label", ownedElsewhere("a", ""), StateExternal, "owned by another supervisor"},
	}
	for _, c := range cases {
		n := firstNode(t, c.v)
		if n.State != c.state || !strings.HasPrefix(n.Detail, c.detail) {
			t.Errorf("%s: state %s, detail %q; want %s, %q", c.name, n.State.label(), n.Detail, c.state.label(), c.detail)
		}
	}
}

func TestAnOwnersLabelIsShortenedAndMadeSafeInTheDetail(t *testing.T) {
	n := firstNode(t, ownedElsewhere("a", "evil\x1b[2J\tlabel "+strings.Repeat("long", 40)))
	if strings.ContainsAny(n.Detail, "\x1b\t") {
		t.Errorf("detail keeps a control character: %q", n.Detail)
	}
	if r := []rune(n.Detail); len(r) > len("owned by ")+40+len(", inspected 09:30:15")+1 {
		t.Errorf("detail not shortened (%d runes): %q", len(r), n.Detail)
	}
}

// External has its own hue, apart from every other state's, and with colour off
// its word and glyph (◇, "external") still tell it from the rest.
func TestExternalHasItsOwnHueAndReadsPlainWithColourOff(t *testing.T) {
	s := newStyles()
	for st := StateStopped; st <= StateInconsistent; st++ {
		if st != StateExternal && sameColour(s.state[StateExternal], s.state[st]) {
			t.Errorf("external shares a hue with %s", st.label())
		}
	}
	for _, v := range []core.InstanceView{withTick(unowned("alpha"), 30*time.Second, "standalone"), ownedElsewhere("alpha", "x")} {
		m := NewWithData(FromViewWith(core.View{Root: "/r", Instances: []core.InstanceView{v}}, lookup(nil)))
		next, _ := m.Update(tea.WindowSizeMsg{Width: 100, Height: 24})
		row := ansi.Strip(next.(Model).treeRow(0, 80))
		// A ticked row also carries its heartbeat mark after the word.
		if !strings.Contains(row, "◇ alpha") || !strings.HasSuffix(strings.TrimSuffix(strings.TrimRight(row, " "), " •"), "external") {
			t.Errorf("row %q", row)
		}
		if ansi.Strip(row) != row {
			t.Errorf("styled output where none is expected: %q", row)
		}
	}
}

// --- the confirmation ---

// A rig with alpha ticked from outside 45 s before the read, bravo unowned and
// quiet, charlie enrolled elsewhere.
func outsideRig(t *testing.T, f *fakeRunners) *rig {
	t.Helper()
	r := &rig{t: t, f: f}
	r.m = NewWithData(r.data(withTick(unowned("alpha"), 45*time.Second, "standalone"), unowned("bravo"), ownedElsewhere("charlie", "ci")),
		WithRunners(f, func(string) {}))
	r.send(tea.WindowSizeMsg{Width: 100, Height: 30})
	return r
}

func (r *rig) chooseRun() {
	r.t.Helper()
	r.send(typeText(":run")...)
	r.send(key(tea.KeyEnter))
}

func (r *rig) runs() int { n, _ := r.f.counts(); return n }

func TestRunOnAnInstanceTickedFromOutsideAsksFirst(t *testing.T) {
	f := &fakeRunners{}
	r := outsideRig(t, f)
	if row := ansi.Strip(r.m.treeRow(0, 80)); !strings.HasSuffix(strings.TrimRight(row, " "), "external •") {
		t.Fatalf("alpha's row: %q", row)
	}
	r.chooseRun()
	s := r.screen()
	// The question wraps inside the box: compare it with the wrapping undone.
	flat := strings.Join(strings.Fields(strings.ReplaceAll(s, "│", " ")), " ")
	for _, want := range []string{"Something outside Easyhog ticked alpha 45s before the last read", "those ticks will fail until you Stop it here", "Run anyway?", "enter or y run", "esc cancel", "on alpha"} {
		if !strings.Contains(flat, want) {
			t.Errorf("the confirmation lacks %q:\n%s", want, s)
		}
	}
	if r.runs() != 0 || len(r.m.busy) != 0 {
		t.Fatal("Run started before the reader answered")
	}
	// Keys that do not answer change nothing: no typing, no run, no close.
	for _, k := range append(typeText("xRUN :q"), key(tea.KeyDown), key(tea.KeyTab), key(tea.KeySpace)) {
		if msgs := r.send(k); len(msgs) != 0 {
			t.Fatalf("key %v scheduled %v", k, msgs)
		}
	}
	if r.m.palette == nil || !r.m.palette.confirming || r.runs() != 0 {
		t.Fatal("the question went away without an answer")
	}
}

func TestEnterOrYConfirmsRun(t *testing.T) {
	for _, answer := range []tea.Msg{key(tea.KeyEnter), typeText("y")[0]} {
		f := &fakeRunners{}
		r := outsideRig(t, f)
		r.chooseRun()
		msgs := r.send(answer)
		if len(msgs) == 0 {
			t.Fatalf("%v scheduled nothing", answer)
		}
		if r.runs() != 1 || r.m.palette != nil {
			t.Fatalf("after %v: %d runs, palette %v", answer, r.runs(), r.m.palette)
		}
		if !strings.Contains(r.screen(), "Running alpha…") {
			t.Errorf("no transitional state:\n%s", r.screen())
		}
	}
}

func TestEscOrNCancelsRunWithoutRunningAnything(t *testing.T) {
	for _, answer := range []tea.Msg{key(tea.KeyEsc), typeText("n")[0], key(tea.KeyCtrlG)} {
		f := &fakeRunners{}
		r := outsideRig(t, f)
		r.chooseRun()
		if msgs := r.send(answer); len(msgs) != 0 {
			t.Fatalf("%v scheduled %v", answer, msgs)
		}
		if r.runs() != 0 || r.m.palette != nil || len(r.m.busy) != 0 {
			t.Fatalf("after %v: %d runs, palette %v", answer, r.runs(), r.m.palette)
		}
		if s := r.screen(); !strings.Contains(s, "Nothing ran: Run was cancelled for alpha") {
			t.Errorf("no notice:\n%s", s)
		}
	}
	// After a cancel, choosing Run again asks again.
	r := outsideRig(t, &fakeRunners{})
	r.chooseRun()
	r.send(key(tea.KeyEsc))
	r.chooseRun()
	if r.m.palette == nil || !r.m.palette.confirming {
		t.Error("the second Run did not ask")
	}
}

func TestRunDoesNotAskForAnInstanceNoOneTicks(t *testing.T) {
	cases := map[string]core.InstanceView{
		"plain":            unowned("alpha"),
		"quiet 4 minutes":  withTick(unowned("alpha"), 4*time.Minute, "standalone"),
		"a runner's tick":  withTick(unowned("alpha"), 3*time.Second, "runner"),
		"inside the limit": withTick(unowned("alpha"), core.TickFreshness+time.Second, "standalone"),
	}
	for name, v := range cases {
		f := &fakeRunners{}
		r := &rig{t: t, f: f}
		r.m = NewWithData(r.data(v), WithRunners(f, func(string) {}))
		r.send(tea.WindowSizeMsg{Width: 100, Height: 30})
		r.send(typeText(":run")...)
		if msgs := r.send(key(tea.KeyEnter)); len(msgs) == 0 || r.m.palette != nil {
			t.Errorf("%s: Run asked, or scheduled nothing", name)
		}
		if strings.Contains(r.screen(), "Run anyway?") {
			t.Errorf("%s: asked", name)
		}
	}
}

func TestRunIsStillNotOfferedForAnInstanceEnrolledElsewhere(t *testing.T) {
	f := &fakeRunners{}
	r := outsideRig(t, f)
	r.send(key(tea.KeyDown), key(tea.KeyDown))
	r.send(typeText(":")...)
	if r.names() != "Open folder,Refresh,Quit" {
		t.Errorf("charlie offers %s", r.names())
	}
	// Even one that also shows a recent tick has no Run to ask about.
	v := withTick(ownedElsewhere("alpha", "ci"), time.Second, "standalone")
	if n := firstNode(t, v); n.RunConfirm != "" || len(n.Actions) != 1 {
		t.Errorf("owned elsewhere: confirm %q, actions %v", n.RunConfirm, n.Actions)
	}
}

// The yes runs only what the palette still offers, on the instance it was opened
// on: a refresh between the question and the answer can take Run away or move the
// selection, and then nothing runs.
func TestConfirmingRunsNothingThePaletteNoLongerOffers(t *testing.T) {
	// Run is taken away: another supervisor enrolled it meanwhile.
	f := &fakeRunners{}
	r := outsideRig(t, f)
	r.chooseRun()
	r.send(SnapshotMsg{Data: r.data(ownedElsewhere("alpha", "ci"), unowned("bravo"), ownedElsewhere("charlie", "ci"))})
	if msgs := r.send(key(tea.KeyEnter)); len(msgs) != 0 {
		t.Fatalf("Run scheduled %v", msgs)
	}
	if r.runs() != 0 || !strings.Contains(r.screen(), "Nothing ran: Run is no longer offered") {
		t.Errorf("%d runs:\n%s", r.runs(), r.screen())
	}

	// The instance vanishes.
	f = &fakeRunners{}
	r = outsideRig(t, f)
	r.chooseRun()
	r.send(SnapshotMsg{Data: r.data(unowned("bravo"))})
	if msgs := r.send(typeText("y")[0]); len(msgs) != 0 || r.runs() != 0 {
		t.Fatalf("a vanished instance ran: %v, %d runs", msgs, r.runs())
	}
	if !strings.Contains(r.screen(), "Nothing ran") {
		t.Errorf("no notice:\n%s", r.screen())
	}

	// The tick goes quiet and the reason to ask is gone: the reader's yes stands.
	f = &fakeRunners{}
	r = outsideRig(t, f)
	r.chooseRun()
	r.send(SnapshotMsg{Data: r.data(withTick(unowned("alpha"), 10*time.Minute, "standalone"), unowned("bravo"), ownedElsewhere("charlie", "ci"))})
	if !strings.Contains(r.screen(), "Run alpha?") {
		t.Errorf("no plain question:\n%s", r.screen())
	}
	r.send(key(tea.KeyEnter))
	if r.runs() != 1 {
		t.Errorf("%d runs", r.runs())
	}
}

// Only Enter, y, Esc, n and ctrl+g mean anything while the question stands (ctrl+c
// still quits, as everywhere). Every other key type is inert, so text typed into
// the pane cannot answer it.
func TestEveryOtherKeyIsInertWhileAsking(t *testing.T) {
	for kt := tea.KeyType(-300); kt <= 127; kt++ {
		switch kt {
		case tea.KeyEnter, tea.KeyEsc, tea.KeyCtrlG, tea.KeyCtrlC:
			continue
		}
		f := &fakeRunners{}
		r := outsideRig(t, f)
		r.chooseRun()
		if msgs := r.send(tea.KeyMsg{Type: kt}); len(msgs) != 0 {
			t.Fatalf("key type %d scheduled %v", kt, msgs)
		}
		if r.runs() != 0 {
			t.Fatalf("key type %d started Run", kt)
		}
	}
}
