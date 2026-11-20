package tui

import (
	"strings"
	"testing"

	tea "github.com/charmbracelet/bubbletea"
	"github.com/charmbracelet/lipgloss"
	"github.com/charmbracelet/x/ansi"

	"github.com/zealtv/easyhog/internal/core"
	"github.com/zealtv/easyhog/internal/groundhog"
)

// Tests for the wording and layout of rows.

// tickAll is the ticker's word that this session ticks every instance.
func tickAll(string) (core.TickInfo, bool) {
	return core.TickInfo{Ticking: true, Interval: core.DefaultTickInterval}, true
}

// tickedWithDeliveries is an instance, paused or not, holding deliveries in the
// given states; the tests show it with tickAll, so it reads ticked here.
func tickedWithDeliveries(name string, paused bool, states ...groundhog.DeliveryState) core.InstanceView {
	v := supported(name)
	if paused {
		v = pausedInstance(name)
	}
	st := v.Inspection.Last.Status
	for i, s := range states {
		st.Deliveries = append(st.Deliveries, groundhog.Delivery{Path: "out/d" + string(rune('a'+i)), State: s})
	}
	v.Inspection.Last = obs(st)
	return v
}

// A schedule item with nothing waiting reads scheduled, in its own glyph;
// pending is left to an item with a run waiting to start, and running to one
// whose run is going.
func TestScheduledItemsReadScheduledAndPendingMeansADeliveryWaiting(t *testing.T) {
	v := supported("a")
	st := v.Inspection.Last.Status
	st.Schedule = []groundhog.ScheduleItem{
		{Path: "schedule/daily/quiet", ItemID: "quiet"},
		{Path: "schedule/daily/waiting", ItemID: "waiting"},
		{Path: "schedule/daily/busy", ItemID: "busy"},
		{Path: "schedule/daily/broken", ItemID: "broken"},
		{Path: "schedule/daily/held.paused/x", ItemID: "x", PausedBy: []string{"schedule/daily/held.paused"}},
	}
	for path, s := range map[string]groundhog.DeliveryState{"waiting": groundhog.DeliveryPending, "busy": groundhog.DeliveryRunning,
		"broken": groundhog.DeliveryFailed} {
		st.Deliveries = append(st.Deliveries, groundhog.Delivery{Path: "out/" + path, SchedulePath: sp("schedule/daily/" + path), State: s})
	}
	st.Deliveries = append(st.Deliveries, groundhog.Delivery{Path: "out/quiet-old", SchedulePath: sp("schedule/daily/quiet"), State: groundhog.DeliveryAccepted})
	v.Inspection.Last = obs(st)

	got := map[string]State{}
	for _, c := range FromViewWith(core.View{Instances: []core.InstanceView{v}}, tickAll).Nodes[0].Children {
		got[c.Label] = c.State
	}
	want := map[string]State{"quiet": StateScheduled, "waiting": StatePending, "busy": StateRunning, "broken": StateFailed, "x": StatePaused}
	for label, state := range want {
		if got[label] != state {
			t.Errorf("item %s reads %q (%s), want %q (%s)", label, got[label].label(), got[label].glyph(), state.label(), state.glyph())
		}
	}
}

// Every state has its own word and its own glyph, so none reads as another with
// colour off.
func TestEveryStateHasItsOwnWordAndGlyph(t *testing.T) {
	words, glyphs := map[string]State{}, map[string]State{}
	for s := StateStopped; s <= StateScheduled; s++ {
		if other, dup := words[s.label()]; dup {
			t.Errorf("states %d and %d both read %q", other, s, s.label())
		}
		if other, dup := glyphs[s.glyph()]; dup {
			t.Errorf("states %d and %d both draw %q", other, s, s.glyph())
		}
		words[s.label()], glyphs[s.glyph()] = s, s
		if w := lipgloss.Width(s.glyph()); w != 1 {
			t.Errorf("%s's glyph %q is %d cells wide, not 1", s.label(), s.glyph(), w)
		}
	}
	if StateScheduled.label() != "scheduled" || StateScheduled.glyph() == StatePending.glyph() {
		t.Errorf("scheduled reads %q %q", StateScheduled.label(), StateScheduled.glyph())
	}
}

// A scheduled item's row says scheduled, and the plain row reads the same with
// colour off.
func TestAScheduledItemsRowSaysScheduled(t *testing.T) {
	v := supported("a")
	st := v.Inspection.Last.Status
	st.Schedule = []groundhog.ScheduleItem{{Path: "schedule/daily/quiet", ItemID: "quiet", Cadence: &groundhog.Cadence{Axis: "daily"}}}
	v.Inspection.Last = obs(st)
	m, _ := feed(NewWithData(FromViewWith(core.View{Instances: []core.InstanceView{v}}, ticks())), tea.WindowSizeMsg{Width: 80, Height: 24},
		key(tea.KeyRight))
	row := ansi.Strip(m.treeRow(1, 80))
	if !strings.Contains(row, "◷ quiet") || !strings.HasSuffix(strings.TrimRight(row, " "), "scheduled") || strings.Contains(row, "pending") {
		t.Errorf("row %q", row)
	}
}

// A failed or interrupted delivery makes the instance row read failed, as it
// makes an item's row; running, pending, unrecorded and accepted ones do not.
func TestInstanceRowReadsFailedForAFailedOrInterruptedDelivery(t *testing.T) {
	cases := []struct {
		name   string
		v      core.InstanceView
		state  State
		detail string
	}{
		{"running, failed", tickedWithDeliveries("a", false, groundhog.DeliveryFailed, groundhog.DeliveryAccepted), StateFailed, "1 delivery failed, running"},
		{"paused, interrupted", tickedWithDeliveries("b", true, groundhog.DeliveryInterrupted, groundhog.DeliveryFailed), StateFailed, "2 deliveries failed, nothing fires or starts"},
		{"running, pending only", tickedWithDeliveries("c", false, groundhog.DeliveryPending, groundhog.DeliveryRunning), StateRunning, "inspected "},
		{"running, unrecorded and accepted", tickedWithDeliveries("d", false, groundhog.DeliveryUnrecorded, groundhog.DeliveryAccepted), StateRunning, "inspected "},
	}
	for _, c := range cases {
		n := FromViewWith(core.View{Instances: []core.InstanceView{c.v}}, tickAll).Nodes[0]
		if n.State != c.state || !strings.HasPrefix(n.Detail, c.detail) {
			t.Errorf("%s: state %q detail %q, want %q starting %q", c.name, n.State.label(), n.Detail, c.state.label(), c.detail)
		}
	}
	// An instance not ticked here reads failed for an interrupted delivery too.
	un := supported("u")
	st := un.Inspection.Last.Status
	st.Deliveries = []groundhog.Delivery{{Path: "out/x", State: groundhog.DeliveryInterrupted}}
	un.Inspection.Last = obs(st)
	if n := FromView(core.View{Instances: []core.InstanceView{un}}).Nodes[0]; n.State != StateFailed || !strings.Contains(n.Detail, "not ticked") {
		t.Errorf("not ticked, with an interrupted delivery: %q %q", n.State.label(), n.Detail)
	}
	// An unsupported instance keeps its word.
	uns := tickedWithDeliveries("s", false, groundhog.DeliveryFailed)
	uns.Class = core.ClassUnsupported
	if n := FromViewWith(core.View{Instances: []core.InstanceView{uns}}, tickAll).Nodes[0]; n.State != StateUnsupported {
		t.Errorf("unsupported instance reads %q", n.State.label())
	}
}

// The badge and the row use one rule (failedState): the count in the badge is
// the count the row's detail gives.
func TestInstanceRowAndBadgeCountTheSameDeliveries(t *testing.T) {
	v := tickedWithDeliveries("a", false, groundhog.DeliveryFailed, groundhog.DeliveryInterrupted, groundhog.DeliveryUnrecorded, groundhog.DeliveryPending)
	n := FromViewWith(core.View{Instances: []core.InstanceView{v}}, tickAll).Nodes[0]
	if !strings.Contains(n.Badge, "2 failed") || !strings.HasPrefix(n.Detail, "2 deliveries failed") {
		t.Errorf("badge %q detail %q", n.Badge, n.Detail)
	}
}

// The header's failed count includes an instance whose runner is healthy but
// whose delivery failed, and drops it from running or paused.
func TestHeaderCountsInstancesWithAFailedDelivery(t *testing.T) {
	v := core.View{Root: "/r", Instances: []core.InstanceView{
		supported("ok"),
		tickedWithDeliveries("bad", false, groundhog.DeliveryFailed),
		tickedWithDeliveries("held", true, groundhog.DeliveryInterrupted),
		pausedInstance("idle"),
	}}
	m, _ := feed(NewWithData(FromViewWith(v, tickAll)), tea.WindowSizeMsg{Width: 120, Height: 30})
	if h := ansi.Strip(m.header(120)); !strings.Contains(h, "4 instances · 1 running · 1 paused · 2 failed") {
		t.Errorf("header %q", h)
	}
	// With colour off the failed row is still told apart by glyph and word.
	row := ansi.Strip(m.treeRow(1, 80))
	if !strings.Contains(row, "✗ bad") || !strings.HasSuffix(strings.TrimRight(row, " "), "failed") {
		t.Errorf("row %q", row)
	}
}

// The tree pane grows past 60 columns when the window is wide, and
// stays as it was where it was not capped.
func TestTreePaneGrowsPastSixtyColumnsWhenWide(t *testing.T) {
	for _, c := range []struct{ w, want int }{{90, 36}, {120, 48}, {150, 60}, {160, 64}, {200, 80}, {300, maxTreeWidth}} {
		if got := computeLayout(c.w, 45, false).tree.w; got != c.want {
			t.Errorf("window %d: tree pane %d columns, want %d", c.w, got, c.want)
		}
	}
	l := computeLayout(160, 45, false)
	if l.tree.w+1+l.preview.w != 160 {
		t.Errorf("panes %d + 1 + %d do not fill 160", l.tree.w, l.preview.w)
	}
}

// When room is short the label gives way first, down to a minimum, then
// the badge loses its last parts, and the state word goes last; every row stays
// within its width.
func TestRowsKeepTheStateWordAndBadgeBeforeTheRestOfTheLabel(t *testing.T) {
	v := tickedWithDeliveries("team/bravo/nightly-exports/regional", true, groundhog.DeliveryPending)
	m, _ := feed(NewWithData(FromViewWith(core.View{Instances: []core.InstanceView{v}}, ticks())), tea.WindowSizeMsg{Width: 200, Height: 30})
	row := func(w int) string { return ansi.Strip(m.treeRow(0, w)) }

	for _, w := range []int{64, 80, 100} {
		r := row(w)
		if !strings.HasSuffix(strings.TrimRight(r, " "), "paused") || !strings.Contains(r, "1 pending") || strings.Contains(r, "won't run") {
			t.Errorf("width %d: row %q lost the state word or the count, or kept the verdict", w, r)
		}
		if lipgloss.Width(r) > w {
			t.Errorf("width %d: row is %d cells", w, lipgloss.Width(r))
		}
	}
	// The label is cut with an ellipsis, never touching the badge.
	if r := row(48); !strings.Contains(r, "…  ") {
		t.Errorf("a cut label should leave two spaces before the badge: %q", r)
	}
	// Room for everything: the label stays whole.
	if r := row(120); !strings.Contains(r, "team/bravo/nightly-exports/regional") || strings.Contains(r, "…") {
		t.Errorf("room enough, yet %q", r)
	}
	// Narrower still: the badge goes before the label drops below its minimum,
	// and the state word is the last thing to go.
	r := row(34)
	if !strings.Contains(r, "paused") || strings.Contains(r, "pending") || !strings.Contains(r, "team/bravo/nightl") {
		t.Errorf("width 34: %q", r)
	}
	if r := row(24); strings.Contains(r, "paused ") || !strings.Contains(r, "team/bravo/") {
		t.Errorf("width 24: %q", r)
	}
	for w := 1; w <= 130; w++ {
		if got := lipgloss.Width(row(w)); got > w {
			t.Errorf("width %d: row is %d cells", w, got)
		}
	}
}

// At 160x45 the paused instance with a backlog, under a long path, shows its
// state word and its counts in the real frame; at 80x24 too.
func TestWideAndSmallFramesShowTheStateWordAndBadge(t *testing.T) {
	v := core.View{Root: "/r", Instances: []core.InstanceView{
		tickedWithDeliveries("team/bravo/nightly-exports/regional", true, groundhog.DeliveryPending, groundhog.DeliveryPending),
		tickedWithDeliveries("charlie", false, groundhog.DeliveryFailed),
	}}
	for _, size := range [][2]int{{160, 45}, {80, 24}} {
		m, _ := feed(NewWithData(FromViewWith(v, tickAll)), tea.WindowSizeMsg{Width: size[0], Height: size[1]})
		var paused, failed string
		for _, line := range strings.Split(ansi.Strip(m.View()), "\n") {
			line, _, _ = strings.Cut(line, "│") // the tree pane only
			switch {
			case paused == "" && strings.Contains(line, "◐ team/bravo"): // the tree's row comes before the preview's heading
				paused = line
			case failed == "" && strings.Contains(line, "✗ charlie"):
				failed = line
			}
		}
		if !strings.Contains(paused, "2 pending") || strings.Contains(paused, "won't run") || !strings.HasSuffix(strings.TrimRight(paused, " "), "paused") {
			t.Errorf("%dx%d: paused row %q", size[0], size[1], paused)
		}
		if !strings.HasSuffix(strings.TrimRight(failed, " "), "failed") {
			t.Errorf("%dx%d: failed row %q", size[0], size[1], failed)
		}
	}
}

// The state word already says "running", so a running runner's detail leaves
// it out (Bob, 2026-11-19: the preview read "running · running, inspected"),
// and no stray comma is left; a failed delivery moves the word into the detail.
func TestRunningRowDetailDoesNotEchoTheStateWord(t *testing.T) {
	run := tickedWithDeliveries("a", false)
	n := FromViewWith(core.View{Instances: []core.InstanceView{run}}, tickAll).Nodes[0]
	if n.State != StateRunning || n.Detail != "inspected 09:30:15" {
		t.Errorf("running: state %q detail %q, want running and %q", n.State.label(), n.Detail, "inspected 09:30:15")
	}
	stale := tickedWithDeliveries("b", false)
	stale.Health = core.HealthStale
	n = FromViewWith(core.View{Instances: []core.InstanceView{stale}}, tickAll).Nodes[0]
	if n.State != StateRunning || n.Detail != "stale, inspected 09:30:15" {
		t.Errorf("running, stale: state %q detail %q", n.State.label(), n.Detail)
	}
	failed := tickedWithDeliveries("c", false, groundhog.DeliveryFailed)
	n = FromViewWith(core.View{Instances: []core.InstanceView{failed}}, tickAll).Nodes[0]
	if n.State != StateFailed || n.Detail != "1 delivery failed, running, inspected 09:30:15" {
		t.Errorf("running, failed: state %q detail %q", n.State.label(), n.Detail)
	}
}
