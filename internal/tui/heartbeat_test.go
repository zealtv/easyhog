package tui

import (
	"strings"
	"testing"
	"time"

	"github.com/charmbracelet/lipgloss"
	"github.com/charmbracelet/x/ansi"

	"github.com/zealtv/easyhog/internal/core"
	"github.com/zealtv/easyhog/internal/groundhog"
)

// Tests for the heartbeat: a small mark after the state word of a row something
// drives, coloured by how recent the tick looks, and no tick age in the badge.

// here is what the ticker says of an instance this session ticks, on interval
// (the default when zero).
func here(interval time.Duration) *core.TickInfo {
	if interval == 0 {
		interval = core.DefaultTickInterval
	}
	return &core.TickInfo{Ticking: true, Interval: interval}
}

func lookupOf(v core.InstanceView, info *core.TickInfo) TickLookup {
	return func(key string) (core.TickInfo, bool) {
		if info == nil || key != v.Instance.Path {
			return core.TickInfo{}, false
		}
		return *info, true
	}
}

func heartbeatNode(v core.InstanceView, info *core.TickInfo) Node {
	return FromViewWith(core.View{Root: "/r", Instances: []core.InstanceView{v}}, lookupOf(v, info)).Nodes[0]
}

func heartbeatRow(t *testing.T, v core.InstanceView, info *core.TickInfo, width int) string {
	t.Helper()
	m := NewWithData(FromViewWith(core.View{Root: "/r", Instances: []core.InstanceView{v}}, lookupOf(v, info)))
	return ansi.Strip(m.treeRow(0, width))
}

// Only rows something drives get a mark: external and running.
func TestOnlyDrivenRowsGetAHeartbeat(t *testing.T) {
	const s = time.Second
	failedRun := withDeliveries(withTick(supported("a"), 10*s, "standalone"), delivery("out/f", groundhog.DeliveryFailed, ""))
	unsupported := withTick(supported("a"), 10*s, "standalone")
	unsupported.Class = core.ClassUnsupported
	cases := []struct {
		name  string
		v     core.InstanceView
		info  *core.TickInfo
		state State
		want  core.Heartbeat
	}{
		{"running here, ticked", withTick(supported("a"), 10*s, "standalone"), here(0), StateRunning, core.HeartbeatFresh},
		{"running here, no tick recorded yet", supported("a"), here(0), StateRunning, core.HeartbeatNone},
		{"external, a run loop", withTick(supported("a"), 10*s, "runner"), nil, StateExternal, core.HeartbeatFresh},
		{"external, ticked from outside", withTick(supported("a"), 10*s, "standalone"), nil, StateExternal, core.HeartbeatFresh},
		{"paused", withTick(pausedInstance("a"), 10*s, "standalone"), here(0), StatePaused, core.HeartbeatNone},
		{"not ticked, old tick", withTick(supported("a"), 10*time.Minute, "standalone"), nil, StateStopped, core.HeartbeatNone},
		{"failed", failedRun, here(0), StateFailed, core.HeartbeatNone},
		{"unsupported", unsupported, nil, StateUnsupported, core.HeartbeatNone},
		{"not inspected", iv("a", core.ClassNotInspected, core.HealthOK, core.Inspection{}), nil, StateStopped, core.HeartbeatNone},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			n := heartbeatNode(c.v, c.info)
			if n.State != c.state || n.Heartbeat != c.want {
				t.Errorf("state %s heartbeat %v, want %s and %v", n.State.label(), n.Heartbeat, c.state.label(), c.want)
			}
			dot := heartbeatGlyph(c.want)
			if !n.Hidden { // an unsupported instance's row is hidden by default
				row := strings.TrimRight(heartbeatRow(t, c.v, c.info, 90), " ")
				if (dot != "") != strings.HasSuffix(row, " "+dot) {
					t.Errorf("row %q, want a trailing %q: %v", row, dot, dot != "")
				}
			}
			if c.want == core.HeartbeatNone && strings.Contains(n.Preview, "Heartbeat:") {
				t.Errorf("preview names a heartbeat for a row with none:\n%s", n.Preview)
			}
		})
	}
}

// The row's level follows the tick age: this session's runner against its own
// interval, anything else against 60 s.
func TestHeartbeatLevelsFollowTheTickAgeAndTheExpectedInterval(t *testing.T) {
	const s = time.Second
	ours := func(age time.Duration) core.InstanceView { return withTick(supported("a"), age, "standalone") }
	cases := []struct {
		name string
		v    core.InstanceView
		info *core.TickInfo
		want core.Heartbeat
	}{
		{"ticked here, 90 s on the default", ours(90 * s), here(0), core.HeartbeatFresh},
		{"ticked here, 91 s on the default", ours(91 * s), here(0), core.HeartbeatLate},
		{"ticked here, 3 min", ours(core.TickFreshness), here(0), core.HeartbeatLate},
		{"ticked here, quiet: the tick is stuck", ours(core.TickFreshness + s), here(0), core.HeartbeatQuiet},
		{"ticked here every 20 s, 30 s", ours(30 * s), here(20 * s), core.HeartbeatFresh},
		{"ticked here every 20 s, 31 s", ours(31 * s), here(20 * s), core.HeartbeatLate},
		{"ticked here every 5 min, 4 min", ours(4 * time.Minute), here(5 * time.Minute), core.HeartbeatFresh},
		{"an interval of zero means the default", ours(91 * s), &core.TickInfo{Ticking: true}, core.HeartbeatLate},
		{"outside, 40 s", withTick(supported("a"), 40*s, "standalone"), nil, core.HeartbeatFresh},
		{"outside, 2 min", withTick(supported("a"), 2*time.Minute, "standalone"), nil, core.HeartbeatLate},
		{"outside, at the window's edge", withTick(supported("a"), core.TickFreshness, "standalone"), nil, core.HeartbeatLate},
		{"a run loop, 20 s", withTick(supported("a"), 20*s, "runner"), nil, core.HeartbeatFresh},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			if got := heartbeatNode(c.v, c.info).Heartbeat; got != c.want {
				t.Errorf("heartbeat %v, want %v", got, c.want)
			}
		})
	}
}

// Each level has its own glyph, unlike every state glyph, the palette's ▶ and ■,
// the badge separator and the others, and one cell wide, so colour off still tells them apart.
func TestHeartbeatGlyphsAreDistinctAndOneCellWide(t *testing.T) {
	taken := map[string]string{"▶": "palette Run", "■": "palette Stop", "·": "badge separator", "●": "state"}
	for st := StateStopped; st <= StateScheduled; st++ {
		taken[st.glyph()] = "state " + st.label()
	}
	seen := map[string]core.Heartbeat{}
	for _, h := range []core.Heartbeat{core.HeartbeatFresh, core.HeartbeatLate, core.HeartbeatQuiet} {
		g := heartbeatGlyph(h)
		if g == "" || lipgloss.Width(g) != 1 {
			t.Errorf("%v glyph %q is not one cell", h, g)
		}
		if what, dup := taken[g]; dup {
			t.Errorf("%v glyph %q is already the %s", h, g, what)
		}
		if other, dup := seen[g]; dup {
			t.Errorf("%v and %v both draw %q", other, h, g)
		}
		seen[g] = h
	}
	if heartbeatGlyph(core.HeartbeatNone) != "" {
		t.Error("no level draws nothing")
	}
}

// The mark is coloured, never the word: each level has its own hue, and none
// changes the state's own.
func TestTheHeartbeatColoursTheMarkNotTheWord(t *testing.T) {
	s := newStyles()
	fresh, late, quiet := s.beat[core.HeartbeatFresh], s.beat[core.HeartbeatLate], s.beat[core.HeartbeatQuiet]
	if sameColour(fresh, late) || sameColour(late, quiet) || sameColour(fresh, quiet) {
		t.Error("two levels share a hue")
	}
	if _, ok := s.beat[core.HeartbeatNone]; ok {
		t.Error("none has a style")
	}
	// Both an external row's word and a running row's keep their state colour whatever the level.
	for _, v := range []core.InstanceView{withTick(supported("a"), 10*time.Second, "standalone"), withTick(supported("a"), 100*time.Second, "standalone")} {
		n := heartbeatNode(v, nil)
		if n.State != StateExternal || !sameColour(s.state[n.State], s.state[StateExternal]) {
			t.Errorf("state %s", n.State.label())
		}
	}
}

// The row reads state word, then the mark, with the badge before them; with
// colour off the plain text carries the mark.
func TestTheMarkSitsRightAfterTheStateWordInTheRow(t *testing.T) {
	const s = time.Second
	for _, c := range []struct {
		name string
		v    core.InstanceView
		info *core.TickInfo
		want string
	}{
		{"fresh", withTick(supported("alpha"), 10*s, "standalone"), here(0), "running • "},
		{"late", withTick(supported("alpha"), 100*s, "standalone"), here(0), "running ◦ "},
		{"quiet", withTick(supported("alpha"), 10*time.Minute, "standalone"), here(0), "running ∙ "},
		{"external, late", withTick(supported("alpha"), 100*s, "standalone"), nil, "external ◦ "},
	} {
		t.Run(c.name, func(t *testing.T) {
			row := heartbeatRow(t, c.v, c.info, 80)
			if !strings.HasSuffix(row, c.want) || ansi.Strip(row) != row {
				t.Errorf("row %q, want it to end %q", row, c.want)
			}
			if strings.Contains(row, "tick ") {
				t.Errorf("the row still says tick: %q", row)
			}
		})
	}
}

// The tick age has left the badge for every verdict.
func TestTheBadgeCarriesNoTickAge(t *testing.T) {
	for name, c := range map[string]struct {
		v    core.InstanceView
		info *core.TickInfo
	}{
		"running here": {withTick(supported("a"), 12*time.Second, "standalone"), here(0)},
		"outside":      {withTick(supported("a"), 45*time.Second, "standalone"), nil},
		"quiet":        {withTick(supported("a"), 25*time.Minute, "standalone"), nil},
	} {
		b := heartbeatNode(c.v, c.info).Badge
		if strings.Contains(b, "12s") || strings.Contains(b, "45s") || strings.Contains(b, "25m") || strings.Contains(b, ", tick") {
			t.Errorf("%s: badge %q carries an age", name, b)
		}
	}
	if b := heartbeatNode(withTick(supported("a"), 12*time.Second, "standalone"), here(0)).Badge; b != "" { // no items, and the verdict stays out of the badge
		t.Errorf("badge %q", b)
	}
}

// At narrow widths the label gives way first, then the badge, then the state
// word; the mark goes with the word and is never left alone or cut in half.
func TestTheMarkStaysWithTheStateWordInANarrowRow(t *testing.T) {
	v := withTick(supported("a-long-instance-name"), 10*time.Second, "standalone")
	sawBoth, sawNeither := false, false
	for w := 14; w <= 100; w++ {
		row := heartbeatRow(t, v, here(0), w)
		if lipgloss.Width(row) > w {
			t.Fatalf("width %d: row is %d cells: %q", w, lipgloss.Width(row), row)
		}
		word, dot := strings.Contains(row, "running"), strings.Contains(row, "•")
		if word != dot {
			t.Errorf("width %d: word %v but mark %v: %q", w, word, dot, row)
		}
		sawBoth, sawNeither = sawBoth || word, sawNeither || !word
		if dot {
			if !strings.HasSuffix(strings.TrimRight(row, " "), "running •") {
				t.Errorf("width %d: the mark is not right after the word: %q", w, row)
			}
		}
		// The label is cut before the badge, and the badge before the word (the old rule, kept).
		if strings.Contains(row, "1 pending") && !word {
			t.Errorf("width %d: badge outlasted the state word: %q", w, row)
		}
	}
	if !sawBoth || !sawNeither {
		t.Errorf("the widths never showed both the word with its mark (%v) and neither (%v)", sawBoth, sawNeither)
	}
}

// The preview's header carries the mark after the state word, and one preview
// line names the level and the interval it was judged against, with the exact
// age still in the verdict line.
func TestThePreviewNamesTheHeartbeatAndKeepsTheExactAge(t *testing.T) {
	const s = time.Second
	own := withTick(supported("a"), 100*s, "standalone")
	n := heartbeatNode(own, here(20*s))
	for _, want := range []string{
		"Heartbeat: late, judged against a tick expected every 20s (the interval this session ticks it at): fresh up to 30s, late up to 3m00s.",
		"Pending work: yes, this session ticks it; last tick 1m40s before the 09:30:15 read.",
	} {
		if !strings.Contains(n.Preview, want) {
			t.Errorf("preview lacks %q:\n%s", want, n.Preview)
		}
	}
	out := heartbeatNode(withTick(supported("a"), 40*s, "standalone"), nil).Preview
	if want := "Heartbeat: fresh, judged against a tick expected every 1m00s (assumed for something outside Easyhog, since Groundhog reports no interval): fresh up to 1m30s, late up to 3m00s."; !strings.Contains(out, want) {
		t.Errorf("preview lacks %q:\n%s", want, out)
	}
	if strings.Index(out, "Heartbeat:") < strings.Index(out, "Pending work:") {
		t.Errorf("the heartbeat line comes before the verdict:\n%s", out)
	}

	// The header: state word, then the mark, then the rest.
	m := NewWithData(FromViewWith(core.View{Root: "/r", Instances: []core.InstanceView{own}}, lookupOf(own, here(20*s))))
	lines := m.previewLines(100)
	if len(lines) < 2 || !strings.HasPrefix(ansi.Strip(lines[1]), "running ◦ · inspected 09:30:15") {
		t.Errorf("preview header %q", lines)
	}
}

// A row with no mark keeps its preview header as it was.
func TestAStoppedRowsPreviewHeaderHasNoMark(t *testing.T) {
	m := NewWithData(FromViewWith(core.View{Root: "/r", Instances: []core.InstanceView{supported("a")}}, ticks()))
	lines := m.previewLines(100)
	if len(lines) < 2 || !strings.HasPrefix(ansi.Strip(lines[1]), "stopped · not ticked") {
		t.Errorf("preview header %q", lines)
	}
}
