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

func heartbeatNode(v core.InstanceView, info *core.RunnerInfo) Node {
	infos := map[string]core.RunnerInfo{}
	if info != nil {
		infos[v.Instance.Path] = *info
	}
	return FromViewWith(core.View{Root: "/r", Instances: []core.InstanceView{v}}, lookup(infos)).Nodes[0]
}

func heartbeatRow(t *testing.T, v core.InstanceView, info *core.RunnerInfo, width int) string {
	t.Helper()
	infos := map[string]core.RunnerInfo{}
	if info != nil {
		infos[v.Instance.Path] = *info
	}
	m := NewWithData(FromViewWith(core.View{Root: "/r", Instances: []core.InstanceView{v}}, lookup(infos)))
	return ansi.Strip(m.treeRow(0, width))
}

// Only rows something drives get a mark: external (both kinds) and running.
func TestOnlyDrivenRowsGetAHeartbeat(t *testing.T) {
	const s = time.Second
	failedRun := withDeliveries(withTick(ours("a", groundhog.RunnerRunning, false), 10*s, "runner"), delivery("d-f", groundhog.DeliveryFailed, "", ""))
	inconsistent := withTick(ours("a", groundhog.RunnerRunning, false), 10*s, "runner")
	inconsistent.Health = core.HealthInconsistent
	unsupported := withTick(unowned("a"), 10*s, "standalone")
	unsupported.Class = core.ClassUnsupported
	cases := []struct {
		name  string
		v     core.InstanceView
		state State
		want  core.Heartbeat
	}{
		{"running here, ticked", withTick(ours("a", groundhog.RunnerRunning, false), 10*s, "runner"), StateRunning, core.HeartbeatFresh},
		{"running here, no tick recorded yet", ours("a", groundhog.RunnerRunning, false), StateRunning, core.HeartbeatNone},
		{"external, owned elsewhere, ticked", withTick(ownedElsewhere("a", "ci"), 10*s, "runner"), StateExternal, core.HeartbeatFresh},
		{"external, owned elsewhere, no tick", ownedElsewhere("a", "ci"), StateExternal, core.HeartbeatNone},
		{"external, ticked from outside", withTick(unowned("a"), 10*s, "standalone"), StateExternal, core.HeartbeatFresh},
		{"paused runner", withTick(ours("a", groundhog.RunnerRunning, true), 10*s, "runner"), StatePaused, core.HeartbeatNone},
		{"enrolled, no runner", withTick(ours("a", groundhog.RunnerStopped, false), 10*s, "runner"), StateStopped, core.HeartbeatNone},
		{"not enrolled, old tick", withTick(unowned("a"), 10*time.Minute, "standalone"), StateStopped, core.HeartbeatNone},
		{"failed", failedRun, StateFailed, core.HeartbeatNone},
		{"inconsistent", inconsistent, StateInconsistent, core.HeartbeatNone},
		{"unsupported", unsupported, StateUnsupported, core.HeartbeatNone},
		{"not inspected", iv("a", core.ClassNotInspected, core.HealthOK, core.Inspection{}), StateStopped, core.HeartbeatNone},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			n := heartbeatNode(c.v, nil)
			if n.State != c.state || n.Heartbeat != c.want {
				t.Errorf("state %s heartbeat %v, want %s and %v", n.State.label(), n.Heartbeat, c.state.label(), c.want)
			}
			dot := heartbeatGlyph(c.want)
			if !n.Hidden { // an unsupported instance's row is hidden by default
				row := strings.TrimRight(heartbeatRow(t, c.v, nil, 90), " ")
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
	cases := []struct {
		name string
		v    core.InstanceView
		info *core.RunnerInfo
		want core.Heartbeat
	}{
		{"our runner, 90 s on the default", withTick(ours("a", groundhog.RunnerRunning, false), 90*s, "runner"), nil, core.HeartbeatFresh},
		{"our runner, 91 s on the default", withTick(ours("a", groundhog.RunnerRunning, false), 91*s, "runner"), nil, core.HeartbeatLate},
		{"our runner, 3 min", withTick(ours("a", groundhog.RunnerRunning, false), core.TickFreshness, "runner"), nil, core.HeartbeatLate},
		{"our runner, stuck in a long consumer call", withTick(ours("a", groundhog.RunnerRunning, false), core.TickFreshness+s, "runner"), nil, core.HeartbeatQuiet},
		{"our runner on a 20 s interval, 30 s", withTick(ours("a", groundhog.RunnerRunning, false), 30*s, "runner"), &core.RunnerInfo{Interval: 20 * s}, core.HeartbeatFresh},
		{"our runner on a 20 s interval, 31 s", withTick(ours("a", groundhog.RunnerRunning, false), 31*s, "runner"), &core.RunnerInfo{Interval: 20 * s}, core.HeartbeatLate},
		{"our runner on a 5 min interval, 4 min", withTick(ours("a", groundhog.RunnerRunning, false), 4*time.Minute, "runner"), &core.RunnerInfo{Interval: 5 * time.Minute}, core.HeartbeatFresh},
		{"a runner interval of zero means the default", withTick(ours("a", groundhog.RunnerRunning, false), 91*s, "runner"), &core.RunnerInfo{}, core.HeartbeatLate},
		{"outside, 40 s", withTick(unowned("a"), 40*s, "standalone"), nil, core.HeartbeatFresh},
		{"outside, 2 min", withTick(unowned("a"), 2*time.Minute, "standalone"), nil, core.HeartbeatLate},
		{"outside, at the window's edge", withTick(unowned("a"), core.TickFreshness, "standalone"), nil, core.HeartbeatLate},
		{"enrolled elsewhere, a dead leftover owner", withTick(ownedElsewhere("a", "gone"), 20*time.Minute, "runner"), nil, core.HeartbeatQuiet},
		{"enrolled elsewhere, a live one", withTick(ownedElsewhere("a", "live"), 20*s, "runner"), nil, core.HeartbeatFresh},
		// An external row never reads the runner's interval, even if a lookup has one.
		{"owned elsewhere ignores a runner's interval", withTick(ownedElsewhere("a", "ci"), 31*s, "runner"), &core.RunnerInfo{Interval: 10 * s}, core.HeartbeatFresh},
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
	for st := StateStopped; st <= StateInconsistent; st++ {
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
	for _, v := range []core.InstanceView{withTick(unowned("a"), 10*time.Second, "standalone"), withTick(unowned("a"), 100*time.Second, "standalone")} {
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
		want string
	}{
		{"fresh", withTick(ours("alpha", groundhog.RunnerRunning, false), 10*s, "runner"), "running • "},
		{"late", withTick(ours("alpha", groundhog.RunnerRunning, false), 100*s, "runner"), "running ◦ "},
		{"quiet", withTick(ours("alpha", groundhog.RunnerRunning, false), 10*time.Minute, "runner"), "running ∙ "},
		{"external, late", withTick(unowned("alpha"), 100*s, "standalone"), "external ◦ "},
	} {
		t.Run(c.name, func(t *testing.T) {
			row := heartbeatRow(t, c.v, nil, 80)
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
	for name, v := range map[string]core.InstanceView{
		"will run":        withTick(ours("a", groundhog.RunnerRunning, false), 12*time.Second, "runner"),
		"probably":        withTick(unowned("a"), 45*time.Second, "standalone"),
		"handoffs wait":   withTick(withDeliveries(ours("a", groundhog.RunnerRunning, false), uncertainA), 12*time.Second, "runner"),
		"probably, waits": withTick(withDeliveries(unowned("a"), uncertainA), 45*time.Second, "standalone"),
		"quiet":           withTick(unowned("a"), 25*time.Minute, "standalone"),
	} {
		b := heartbeatNode(v, nil).Badge
		if strings.Contains(b, "12s") || strings.Contains(b, "45s") || strings.Contains(b, "25m") || strings.Contains(b, "tick 0") || strings.Contains(b, ", tick") {
			t.Errorf("%s: badge %q carries an age", name, b)
		}
	}
	if b := heartbeatNode(withTick(ours("a", groundhog.RunnerRunning, false), 12*time.Second, "runner"), nil).Badge; b != "will run" { // no items: the badge says nothing about them
		t.Errorf("badge %q", b)
	}
}

// At narrow widths the label gives way first, then the badge, then the state
// word; the mark goes with the word and is never left alone or cut in half.
func TestTheMarkStaysWithTheStateWordInANarrowRow(t *testing.T) {
	v := withTick(ours("a-long-instance-name", groundhog.RunnerRunning, false), 10*time.Second, "runner")
	sawBoth, sawNeither := false, false
	for w := 14; w <= 100; w++ {
		row := heartbeatRow(t, v, nil, w)
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
		if strings.Contains(row, "will run") && !word {
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
	own := withTick(ours("a", groundhog.RunnerRunning, false), 100*s, "runner")
	n := heartbeatNode(own, &core.RunnerInfo{Interval: 20 * s})
	for _, want := range []string{
		"Heartbeat: late, judged against a tick expected every 20s (this session's runner's interval): fresh up to 30s, late up to 3m00s.",
		"Pending work: yes, this session's runner ticked 1m40s before the 09:30:15 read.",
	} {
		if !strings.Contains(n.Preview, want) {
			t.Errorf("preview lacks %q:\n%s", want, n.Preview)
		}
	}
	out := heartbeatNode(withTick(unowned("a"), 40*s, "standalone"), nil).Preview
	if want := "Heartbeat: fresh, judged against a tick expected every 1m00s (assumed for something outside Easyhog, since Groundhog reports no interval): fresh up to 1m30s, late up to 3m00s."; !strings.Contains(out, want) {
		t.Errorf("preview lacks %q:\n%s", want, out)
	}
	if strings.Index(out, "Heartbeat:") < strings.Index(out, "Pending work:") {
		t.Errorf("the heartbeat line comes before the verdict:\n%s", out)
	}

	// The header: state word, then the mark, then the rest.
	m := NewWithData(FromViewWith(core.View{Root: "/r", Instances: []core.InstanceView{own}}, lookup(nil)))
	lines := m.previewLines(100)
	if len(lines) < 2 || !strings.HasPrefix(ansi.Strip(lines[1]), "running ◦ · running") {
		t.Errorf("preview header %q", lines)
	}
}

// A row with no mark keeps its preview header as it was.
func TestAStoppedRowsPreviewHeaderHasNoMark(t *testing.T) {
	m := NewWithData(FromViewWith(core.View{Root: "/r", Instances: []core.InstanceView{unowned("a")}}, lookup(nil)))
	lines := m.previewLines(100)
	if len(lines) < 2 || !strings.HasPrefix(ansi.Strip(lines[1]), "stopped · not enrolled") {
		t.Errorf("preview header %q", lines)
	}
}

// Run's confirmation question keeps the tick age.
func TestRunsQuestionKeepsItsAge(t *testing.T) {
	r := outsideRig(t, &fakeRunners{})
	if q := r.m.rows[0].node.RunConfirm; !strings.Contains(q, "ticked alpha 45s before the last read") {
		t.Errorf("question %q", q)
	}
}
