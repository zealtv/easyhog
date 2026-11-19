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

// ownedWithDeliveries is an instance this session owns, its runner running or paused,
// holding deliveries in the given states.
func ownedWithDeliveries(name string, paused bool, states ...groundhog.DeliveryState) core.InstanceView {
	v := ours(name, groundhog.RunnerRunning, paused)
	st := v.Inspection.Last.Status
	for i, s := range states {
		st.Deliveries = append(st.Deliveries, groundhog.Delivery{Path: "out/d" + string(rune('a'+i)), State: s})
	}
	v.Inspection.Last = obs(st)
	return v
}

// A schedule item with nothing waiting reads scheduled, in its own
// glyph; pending is left to an item with a delivery waiting for hand-over.
func TestScheduledItemsReadScheduledAndPendingMeansADeliveryWaiting(t *testing.T) {
	v := ours("a", groundhog.RunnerRunning, false)
	st := v.Inspection.Last.Status
	st.Schedule = []groundhog.ScheduleItem{
		{Path: "schedule/daily/quiet", ItemID: "quiet"},
		{Path: "schedule/daily/waiting", ItemID: "waiting"},
		{Path: "schedule/daily/busy", ItemID: "busy"},
		{Path: "schedule/daily/broken", ItemID: "broken"},
		{Path: "schedule/daily/held.paused/x", ItemID: "x", PausedBy: []string{"schedule/daily/held.paused"}},
	}
	for path, s := range map[string]groundhog.DeliveryState{"waiting": groundhog.DeliveryPending, "busy": groundhog.DeliveryInFlight,
		"broken": groundhog.DeliveryFailed} {
		st.Deliveries = append(st.Deliveries, groundhog.Delivery{Path: "out/" + path, SchedulePath: sp("schedule/daily/" + path), State: s})
	}
	st.Deliveries = append(st.Deliveries, groundhog.Delivery{Path: "out/quiet-old", SchedulePath: sp("schedule/daily/quiet"), State: groundhog.DeliveryAccepted})
	v.Inspection.Last = obs(st)

	got := map[string]State{}
	for _, c := range FromViewWith(core.View{Instances: []core.InstanceView{v}}, lookup(nil)).Nodes[0].Children {
		got[c.Label] = c.State
	}
	want := map[string]State{"quiet": StateScheduled, "waiting": StatePending, "busy": StatePending, "broken": StateFailed, "x": StatePaused}
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
	for s := StateStopped; s <= StateInconsistent; s++ {
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
	v := ours("a", groundhog.RunnerRunning, false)
	st := v.Inspection.Last.Status
	st.Schedule = []groundhog.ScheduleItem{{Path: "schedule/daily/quiet", ItemID: "quiet", Cadence: &groundhog.Cadence{Axis: "daily"}}}
	v.Inspection.Last = obs(st)
	m, _ := feed(NewWithData(FromViewWith(core.View{Instances: []core.InstanceView{v}}, lookup(nil))), tea.WindowSizeMsg{Width: 80, Height: 24},
		key(tea.KeyRight))
	row := ansi.Strip(m.treeRow(1, 80))
	if !strings.Contains(row, "◷ quiet") || !strings.HasSuffix(strings.TrimRight(row, " "), "scheduled") || strings.Contains(row, "pending") {
		t.Errorf("row %q", row)
	}
}

// A failed or uncertain delivery makes the instance row read failed, as
// it makes an item's row; missing and accepted ones do not.
func TestInstanceRowReadsFailedForAFailedOrUncertainDelivery(t *testing.T) {
	cases := []struct {
		name   string
		v      core.InstanceView
		state  State
		detail string
	}{
		{"running, failed", ownedWithDeliveries("a", false, groundhog.DeliveryFailed, groundhog.DeliveryAccepted), StateFailed, "1 delivery failed, running"},
		{"paused, uncertain", ownedWithDeliveries("b", true, groundhog.DeliveryUncertain, groundhog.DeliveryFailed), StateFailed, "2 deliveries failed, paused"},
		{"running, pending only", ownedWithDeliveries("c", false, groundhog.DeliveryPending, groundhog.DeliveryInFlight), StateRunning, "running"},
		{"running, missing and accepted", ownedWithDeliveries("d", false, groundhog.DeliveryMissing, groundhog.DeliveryAccepted), StateRunning, "running"},
	}
	for _, c := range cases {
		n := FromViewWith(core.View{Instances: []core.InstanceView{c.v}}, lookup(nil)).Nodes[0]
		if n.State != c.state || !strings.HasPrefix(n.Detail, c.detail) {
			t.Errorf("%s: state %q detail %q, want %q starting %q", c.name, n.State.label(), n.Detail, c.state.label(), c.detail)
		}
	}
	// An unowned instance that holds an uncertain delivery (a Quit that gave up
	// waiting leaves one) reads failed too; so does one whose failed delivery sits
	// beside a runner that ended: both are already failed or become so.
	un := unowned("u")
	st := un.Inspection.Last.Status
	st.Deliveries = []groundhog.Delivery{{Path: "out/x", State: groundhog.DeliveryUncertain}}
	un.Inspection.Last = obs(st)
	if n := FromView(core.View{Instances: []core.InstanceView{un}}).Nodes[0]; n.State != StateFailed || !strings.Contains(n.Detail, "not enrolled") {
		t.Errorf("unowned with an uncertain delivery: %q %q", n.State.label(), n.Detail)
	}
	// An inconsistent read is not acted on, so it keeps its own word (not
	// pending, which is left to a delivery waiting); an unsupported instance keeps its word.
	inc := ownedWithDeliveries("i", false, groundhog.DeliveryFailed)
	inc.Health = core.HealthInconsistent
	if n := FromViewWith(core.View{Instances: []core.InstanceView{inc}}, lookup(nil)).Nodes[0]; n.State != StateInconsistent || n.State.label() != "inconsistent" {
		t.Errorf("inconsistent read reads %q", n.State.label())
	}
	uns := ownedWithDeliveries("s", false, groundhog.DeliveryFailed)
	uns.Class = core.ClassUnsupported
	if n := FromViewWith(core.View{Instances: []core.InstanceView{uns}}, lookup(nil)).Nodes[0]; n.State != StateUnsupported {
		t.Errorf("unsupported instance reads %q", n.State.label())
	}
}

// The badge and the row use one rule (failedState): the count in the badge is
// the count the row's detail gives.
func TestInstanceRowAndBadgeCountTheSameDeliveries(t *testing.T) {
	v := ownedWithDeliveries("a", false, groundhog.DeliveryFailed, groundhog.DeliveryUncertain, groundhog.DeliveryMissing, groundhog.DeliveryPending)
	n := FromViewWith(core.View{Instances: []core.InstanceView{v}}, lookup(nil)).Nodes[0]
	if !strings.Contains(n.Badge, "2 failed") || !strings.HasPrefix(n.Detail, "2 deliveries failed") {
		t.Errorf("badge %q detail %q", n.Badge, n.Detail)
	}
}

// The header's failed count includes an instance whose runner is healthy but
// whose delivery failed, and drops it from running or paused.
func TestHeaderCountsInstancesWithAFailedDelivery(t *testing.T) {
	v := core.View{Root: "/r", Instances: []core.InstanceView{
		ours("ok", groundhog.RunnerRunning, false),
		ownedWithDeliveries("bad", false, groundhog.DeliveryFailed),
		ownedWithDeliveries("held", true, groundhog.DeliveryUncertain),
		ours("idle", groundhog.RunnerRunning, true),
	}}
	m, _ := feed(NewWithData(FromViewWith(v, lookup(nil))), tea.WindowSizeMsg{Width: 120, Height: 30})
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
	v := ownedWithDeliveries("team/bravo/nightly-exports/regional", true, groundhog.DeliveryPending)
	m, _ := feed(NewWithData(FromViewWith(core.View{Instances: []core.InstanceView{v}}, lookup(nil))), tea.WindowSizeMsg{Width: 200, Height: 30})
	row := func(w int) string { return ansi.Strip(m.treeRow(0, w)) }

	for _, w := range []int{64, 80, 100} {
		r := row(w)
		if !strings.HasSuffix(strings.TrimRight(r, " "), "paused") || !strings.Contains(r, "paused: won't run") {
			t.Errorf("width %d: row %q lost the state word or the verdict", w, r)
		}
		if lipgloss.Width(r) > w {
			t.Errorf("width %d: row is %d cells", w, lipgloss.Width(r))
		}
	}
	// The label is cut with an ellipsis, never touching the badge.
	if r := row(64); !strings.Contains(r, "…  ") {
		t.Errorf("a cut label should leave two spaces before the badge: %q", r)
	}
	// Room for everything: the label stays whole.
	if r := row(120); !strings.Contains(r, "team/bravo/nightly-exports/regional") || strings.Contains(r, "…") {
		t.Errorf("room enough, yet %q", r)
	}
	// Narrower still: the badge goes before the label drops below its minimum,
	// and the state word is the last thing to go.
	r := row(34)
	if !strings.Contains(r, "paused") || strings.Contains(r, "won't") || !strings.Contains(r, "team/bravo/nightl") {
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
// state word and its verdict in the real frame; at 80x24 too.
func TestWideAndSmallFramesShowTheStateWordAndBadge(t *testing.T) {
	v := core.View{Root: "/r", Instances: []core.InstanceView{
		ownedWithDeliveries("team/bravo/nightly-exports/regional", true, groundhog.DeliveryPending, groundhog.DeliveryPending),
		ownedWithDeliveries("charlie", false, groundhog.DeliveryFailed),
	}}
	for _, size := range [][2]int{{160, 45}, {80, 24}} {
		m, _ := feed(NewWithData(FromViewWith(v, lookup(nil))), tea.WindowSizeMsg{Width: size[0], Height: size[1]})
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
		if !strings.Contains(paused, "paused: won't run") || !strings.HasSuffix(strings.TrimRight(paused, " "), "paused") {
			t.Errorf("%dx%d: paused row %q", size[0], size[1], paused)
		}
		if !strings.HasSuffix(strings.TrimRight(failed, " "), "failed") {
			t.Errorf("%dx%d: failed row %q", size[0], size[1], failed)
		}
	}
}
