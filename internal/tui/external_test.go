package tui

import (
	"strings"
	"testing"
	"time"

	tea "github.com/charmbracelet/bubbletea"
	"github.com/charmbracelet/x/ansi"

	"github.com/zealtv/easyhog/internal/core"
)

// "External" means something outside this session ticks the instance recently:
// cron, a service, a terminal's `groundhog.sh run`. It is shown, not refused:
// two tickers are normal, so Run is offered on it and asks nothing.

func firstNode(t *testing.T, v core.InstanceView, look TickLookup) Node {
	t.Helper()
	return FromViewWith(core.View{Root: "/r", Instances: []core.InstanceView{v}}, look).Nodes[0]
}

func TestExternalIsAFreshTickThatIsNotThisSessions(t *testing.T) {
	paused := withTick(supported("p"), time.Second, "standalone")
	paused.Inspection.Last.Status.Paused = true
	// This session stopped ticking "a" 5 s before the read: that tick is its own.
	ownTick := at.Add(-5 * time.Second)
	stoppedHere := func(string) (core.TickInfo, bool) {
		return core.TickInfo{Interval: time.Minute, LastBegan: ownTick, LastEnded: ownTick}, true
	}
	cases := []struct {
		name   string
		v      core.InstanceView
		look   TickLookup
		state  State
		detail string
	}{
		{"ticked from outside", withTick(supported("a"), 45*time.Second, "standalone"), ticks(), StateExternal, "ticked from outside Easyhog"},
		{"by a run loop", withTick(supported("a"), 5*time.Second, "runner"), ticks(), StateExternal, "ticked from outside Easyhog"},
		{"exactly at the window", withTick(supported("a"), core.TickFreshness, "standalone"), ticks(), StateExternal, "ticked from outside Easyhog"},
		{"one second past the window", withTick(supported("a"), core.TickFreshness+time.Second, "standalone"), ticks(), StateStopped, "not ticked"},
		{"never ticked", supported("a"), ticks(), StateStopped, "not ticked"},
		{"this session's own last tick, after Stop", withTick(supported("a"), 5*time.Second, "standalone"), stoppedHere, StateStopped, "not ticked"},
		{"ticked here too", withTick(supported("a"), 5*time.Second, "standalone"), ticks("/r/a/.groundhog"), StateRunning, ""},
		{"paused, so nothing ticks it", paused, ticks(), StatePaused, "nothing fires or starts"},
	}
	for _, c := range cases {
		n := firstNode(t, c.v, c.look)
		if n.State != c.state || !strings.HasPrefix(n.Detail, c.detail) {
			t.Errorf("%s: state %s, detail %q; want %s, %q", c.name, n.State.label(), n.Detail, c.state.label(), c.detail)
		}
		if c.state == StateExternal && !hasAction(n, ActionRun) {
			t.Errorf("%s: Run is not offered", c.name)
		}
	}
}

// External has its own hue, apart from every other state's, and with colour off
// its word and glyph (◇, "external") still tell it from the rest.
func TestExternalHasItsOwnHueAndReadsPlainWithColourOff(t *testing.T) {
	s := newStyles()
	for st := StateStopped; st <= StateScheduled; st++ {
		if st != StateExternal && sameColour(s.state[StateExternal], s.state[st]) {
			t.Errorf("external shares a hue with %s", st.label())
		}
	}
	m := NewWithData(FromViewWith(core.View{Root: "/r", Instances: []core.InstanceView{withTick(supported("alpha"), 30*time.Second, "standalone")}}, ticks()))
	next, _ := m.Update(tea.WindowSizeMsg{Width: 100, Height: 24})
	row := ansi.Strip(next.(Model).treeRow(0, 80))
	// A ticked row also carries its heartbeat mark after the word.
	if !strings.Contains(row, "◇ alpha") || !strings.HasSuffix(strings.TrimSuffix(strings.TrimRight(row, " "), " •"), "external") {
		t.Errorf("row %q", row)
	}
}

// Run on an instance ticked from outside goes straight to the ticker: there is
// no question to answer, since ticking it here as well breaks nothing.
func TestRunOnAnInstanceTickedFromOutsideAsksNothing(t *testing.T) {
	f := &fakeControl{}
	r := &rig{t: t, f: f}
	r.m = NewWithData(r.data(withTick(supported("alpha"), 45*time.Second, "standalone")), WithControl(f, nil))
	r.m, _ = feed(r.m, tea.WindowSizeMsg{Width: 100, Height: 30})
	if got := r.do("run"); !strings.HasPrefix(got, "Run: alpha: ticked") || f.list() != "run alpha" {
		t.Fatalf("notice %q calls %q", got, f.list())
	}
}
