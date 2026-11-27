package tui

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"strings"
	"testing"

	tea "github.com/charmbracelet/bubbletea"
	"github.com/charmbracelet/lipgloss"
	"github.com/charmbracelet/x/ansi"

	"github.com/zealtv/easyhog/internal/core"
)

// spy records every action the shell hands to its handler.
type spy struct{ calls []ActionRequest }

func (s *spy) handle(req ActionRequest) tea.Cmd {
	s.calls = append(s.calls, req)
	return nil
}

func newSpied(t *testing.T, scenario string) (Model, *spy) {
	t.Helper()
	s := &spy{}
	m := NewWithData(SampleData(scenario), WithActions(s.handle))
	next, _ := m.Update(tea.WindowSizeMsg{Width: 100, Height: 30})
	return next.(Model), s
}

// quits reports whether running cmd produces a quit, expanding batches.
func quits(cmd tea.Cmd) bool {
	if cmd == nil {
		return false
	}
	switch msg := cmd().(type) {
	case tea.QuitMsg:
		return true
	case tea.BatchMsg:
		for _, c := range msg {
			if quits(c) {
				return true
			}
		}
	}
	return false
}

func feed(m Model, msgs ...tea.Msg) (Model, tea.Cmd) {
	var cmd tea.Cmd
	for _, msg := range msgs {
		var next tea.Model
		next, cmd = m.Update(msg)
		m = next.(Model)
	}
	return m, cmd
}

func typeText(s string) []tea.Msg {
	var out []tea.Msg
	for _, r := range s {
		out = append(out, tea.KeyMsg{Type: tea.KeyRunes, Runes: []rune{r}})
	}
	return out
}

func key(t tea.KeyType) tea.Msg { return tea.KeyMsg{Type: t} }

// TestKeyRoutingNeverQuitsOrActs is the key-safety table. Outside the palette,
// no key of any type — every named key, every control code, every printable
// ASCII rune with and without alt — quits or reaches the action handler,
// except ctrl+c, which quits. A regression such as "q quits" fails here.
func TestKeyRoutingNeverQuitsOrActs(t *testing.T) {
	var keys []tea.KeyMsg
	for kt := tea.KeyType(-300); kt <= 127; kt++ {
		keys = append(keys, tea.KeyMsg{Type: kt})
	}
	for r := rune(0x20); r < 0x7f; r++ {
		keys = append(keys, tea.KeyMsg{Type: tea.KeyRunes, Runes: []rune{r}})
		keys = append(keys, tea.KeyMsg{Type: tea.KeyRunes, Runes: []rune{r}, Alt: true})
	}
	keys = append(keys,
		tea.KeyMsg{Type: tea.KeyRunes, Runes: []rune("quit"), Paste: true},
		tea.KeyMsg{Type: tea.KeyRunes, Runes: []rune(":"), Paste: true},
		tea.KeyMsg{Type: tea.KeyRunes, Runes: []rune(">"), Paste: true},
		tea.KeyMsg{Type: tea.KeyEnter, Alt: true}, tea.KeyMsg{Type: tea.KeyUp, Alt: true})

	for _, focus := range []pane{paneTree, panePreview} {
		for _, expanded := range []bool{false, true} {
			base, sp := newSpied(t, "full")
			base.focus = focus
			if expanded {
				base = base.setExpanded("alpha", true)
			}
			for _, k := range keys {
				if k.Type == tea.KeyCtrlC && !k.Alt {
					continue
				}
				if k.String() == paletteKey {
					continue // opens the palette; covered below
				}
				next, cmd := base.Update(k)
				if cmd != nil {
					t.Fatalf("key %q (type %d) scheduled work", k.String(), k.Type)
				}
				if next.(Model).palette != nil {
					t.Fatalf("key %q opened the palette", k.String())
				}
			}
			if len(sp.calls) != 0 {
				t.Fatalf("handler reached by bare keys: %v", sp.calls)
			}
		}
	}

	base, _ := newSpied(t, "full")
	_, cmd := base.Update(key(tea.KeyCtrlC))
	if !quits(cmd) {
		t.Fatal("ctrl+c did not quit")
	}
	for _, withPalette := range []bool{false, true} {
		m := base
		if withPalette {
			m = m.openPalette()
		}
		if _, cmd := m.Update(key(tea.KeyCtrlC)); !quits(cmd) {
			t.Fatalf("ctrl+c did not quit (palette open: %v)", withPalette)
		}
	}
}

func TestSpecificKeysDoNothingDangerous(t *testing.T) {
	m, sp := newSpied(t, "full")
	for _, msg := range []tea.Msg{
		tea.KeyMsg{Type: tea.KeyRunes, Runes: []rune("q")}, tea.KeyMsg{Type: tea.KeyRunes, Runes: []rune("Q")},
		tea.KeyMsg{Type: tea.KeyRunes, Runes: []rune("p")}, tea.KeyMsg{Type: tea.KeyRunes, Runes: []rune("r")},
		tea.KeyMsg{Type: tea.KeySpace, Runes: []rune(" ")}, key(tea.KeyEnter), key(tea.KeyEsc),
	} {
		next, cmd := m.Update(msg)
		if cmd != nil {
			t.Fatalf("%v scheduled a command", msg)
		}
		_ = next
	}
	if len(sp.calls) != 0 {
		t.Fatalf("unexpected calls %v", sp.calls)
	}
}

func TestMouseOnlyInspects(t *testing.T) {
	base, sp := newSpied(t, "full")
	buttons := []tea.MouseButton{tea.MouseButtonNone, tea.MouseButtonLeft, tea.MouseButtonMiddle, tea.MouseButtonRight,
		tea.MouseButtonWheelUp, tea.MouseButtonWheelDown, tea.MouseButtonWheelLeft, tea.MouseButtonWheelRight}
	actions := []tea.MouseAction{tea.MouseActionPress, tea.MouseActionRelease, tea.MouseActionMotion}
	for _, withPalette := range []bool{false, true} {
		m := base
		if withPalette {
			m = m.openPalette()
		}
		for _, b := range buttons {
			for _, a := range actions {
				for _, pos := range [][2]int{{0, 0}, {5, 3}, {5, 5}, {70, 5}, {99, 29}, {500, 500}, {-1, -1}} {
					for clicks := 0; clicks < 2; clicks++ { // a double-click is two presses
						next, cmd := m.Update(tea.MouseMsg{X: pos[0], Y: pos[1], Button: b, Action: a})
						if cmd != nil {
							t.Fatalf("mouse %v %v at %v scheduled work", b, a, pos)
						}
						m = next.(Model)
					}
				}
			}
		}
	}
	if len(sp.calls) != 0 {
		t.Fatalf("mouse reached the handler: %v", sp.calls)
	}
}

func TestPaletteOpensEmptyAndEnterAloneDoesNothing(t *testing.T) {
	m, sp := newSpied(t, "full")
	m, _ = feed(m, typeText(">")...)
	if m.palette == nil || m.palette.chosen != "" {
		t.Fatalf("palette = %#v", m.palette)
	}
	next, cmd := m.Update(key(tea.KeyEnter))
	if cmd != nil || next.(Model).palette == nil || len(sp.calls) != 0 {
		t.Fatal("enter on an untouched palette acted")
	}
	// Esc closes it.
	next, _ = m.Update(key(tea.KeyEsc))
	if next.(Model).palette != nil {
		t.Fatal("esc did not close the palette")
	}
}

func TestPaletteQuitOnlyThroughPalette(t *testing.T) {
	m, sp := newSpied(t, "full")
	m, _ = feed(m, typeText(">qui")...)
	_, cmd := m.Update(key(tea.KeyEnter))
	if !quits(cmd) {
		t.Fatal("palette Quit did not quit")
	}
	if len(sp.calls) != 0 {
		t.Fatal("quit went through the handler")
	}
}

func TestPaletteActionsGoThroughHandlerAndValidOnly(t *testing.T) {
	m, sp := newSpied(t, "full") // alpha (running): Stop + Open folder, no Run
	names := func(m Model) string {
		var out []string
		for _, c := range m.commands() {
			out = append(out, c.name)
		}
		return strings.Join(out, ",")
	}
	if got := names(m); got != "Stop,Open folder,Refresh,Quit" {
		t.Fatalf("alpha commands = %s", got)
	}
	m, _ = feed(m, key(tea.KeyDown)) // beta: not ticked
	if got := names(m); got != "Run,Open folder,Refresh,Quit" {
		t.Fatalf("beta commands = %s", got)
	}
	m, _ = feed(m, key(tea.KeyDown)) // gamma: unsupported, nothing valid
	if got := names(m); got != "Refresh,Quit" {
		t.Fatalf("gamma commands = %s", got)
	}
	m, _ = feed(m, key(tea.KeyUp))
	m, _ = feed(m, typeText(">run")...)
	m, _ = feed(m, key(tea.KeyEnter))
	if len(sp.calls) != 1 || sp.calls[0].Action != ActionRun || sp.calls[0].InstanceKey != "beta" {
		t.Fatalf("calls = %#v", sp.calls)
	}
	if m.palette != nil {
		t.Fatal("palette stayed open after running")
	}
	// Typing a hidden action finds nothing to run.
	m, _ = feed(m, typeText(">stop")...)
	for _, c := range m.filtered() {
		if c.action == ActionStop {
			t.Fatal("invalid action offered")
		}
	}
}

func TestDefaultHandlerReportsNotAvailable(t *testing.T) {
	m := NewWithData(SampleData("full"))
	m, _ = feed(m, typeText(">stop")...)
	m, cmd := feed(m, key(tea.KeyEnter))
	if cmd == nil {
		t.Fatal("no result command")
	}
	m, _ = feed(m, cmd())
	if !m.failure || !strings.Contains(m.notice, "not available yet") {
		t.Fatalf("notice = %q failure=%v", m.notice, m.failure)
	}
	if out := m.View(); !strings.Contains(out, "Stop: not available yet") {
		t.Fatalf("status missing from view:\n%s", out)
	}
}

func TestTreeNavigationAndFolding(t *testing.T) {
	m, _ := newSpied(t, "full")
	m, _ = feed(m, key(tea.KeyRight))
	if len(m.rows) != 7 || !m.expanded["alpha"] {
		t.Fatalf("rows=%d expanded=%v", len(m.rows), m.expanded)
	}
	m, _ = feed(m, key(tea.KeyDown), key(tea.KeyLeft)) // child → parent
	if m.selectedKey() != "alpha" {
		t.Fatalf("selected %q", m.selectedKey())
	}
	m, _ = feed(m, key(tea.KeyEnter)) // enter inspects: folds
	if len(m.rows) != 5 {
		t.Fatalf("rows=%d", len(m.rows))
	}
	m, _ = feed(m, key(tea.KeyEnd))
	if m.selectedKey() != "epsilon" {
		t.Fatalf("end → %q", m.selectedKey())
	}
	m, _ = feed(m, key(tea.KeyHome))
	if m.sel != 0 {
		t.Fatal("home")
	}
}

func TestTabMovesFocusAndScrollsPreview(t *testing.T) {
	m, _ := newSpied(t, "full")
	m.height = 12 // short preview so it overflows
	m = m.settle()
	m, _ = feed(m, key(tea.KeyTab))
	if m.focus != panePreview {
		t.Fatal("tab did not focus the preview")
	}
	m, _ = feed(m, key(tea.KeyDown), key(tea.KeyDown))
	if m.scroll == 0 {
		t.Fatal("preview did not scroll")
	}
	if m.selectedKey() != "alpha" {
		t.Fatal("scrolling moved the selection")
	}
	m, _ = feed(m, key(tea.KeyPgDown), key(tea.KeyPgDown), key(tea.KeyPgDown))
	if m.scroll > m.maxScroll() {
		t.Fatal("scroll past the end")
	}
	m, _ = feed(m, key(tea.KeyTab), key(tea.KeyDown))
	if m.selectedKey() != "beta" || m.scroll != 0 {
		t.Fatalf("selected %q scroll %d", m.selectedKey(), m.scroll)
	}
}

func TestClickSelects(t *testing.T) {
	m, sp := newSpied(t, "full")
	l := m.layout()
	m, _ = feed(m, tea.MouseMsg{X: 3, Y: l.tree.y + 3, Button: tea.MouseButtonLeft, Action: tea.MouseActionPress})
	if m.selectedKey() != "gamma" || len(sp.calls) != 0 {
		t.Fatalf("selected %q", m.selectedKey())
	}
}

func TestSnapshotKeepsSelectionByKey(t *testing.T) {
	m, _ := newSpied(t, "full")
	m, _ = feed(m, key(tea.KeyDown), key(tea.KeyDown), key(tea.KeyDown)) // delta
	if m.selectedKey() != "delta" {
		t.Fatalf("setup: %q", m.selectedKey())
	}
	// Reordered, with a new instance inserted before it.
	d := SampleData("full")
	d.Nodes = append([]Node{{Key: "new", Label: "new", State: StatePending}}, d.Nodes...)
	d.Nodes[1], d.Nodes[4] = d.Nodes[4], d.Nodes[1]
	m, _ = feed(m, SnapshotMsg{d})
	if m.selectedKey() != "delta" {
		t.Fatalf("selection moved to %q", m.selectedKey())
	}
	if m.rows[m.sel].node.Label != "shared/delta" {
		t.Fatal("selection points at a stale row")
	}
}

func TestSnapshotWhenSelectedNodeDisappears(t *testing.T) {
	without := func(keys ...string) Data {
		d := SampleData("full")
		var kept []Node
		for _, n := range d.Nodes {
			drop := false
			for _, k := range keys {
				drop = drop || n.Key == k
			}
			if !drop {
				kept = append(kept, n)
			}
		}
		d.Nodes = kept
		return d
	}
	sel := func(downs int, d Data) (Model, string) {
		m, _ := newSpied(t, "full")
		for i := 0; i < downs; i++ {
			m, _ = feed(m, key(tea.KeyDown))
		}
		m, _ = feed(m, SnapshotMsg{d})
		return m, m.selectedKey()
	}
	// Middle node gone: the next sibling takes its place.
	if _, got := sel(2, without("gamma")); got != "delta" {
		t.Fatalf("middle: %q", got)
	}
	// Last node gone: the previous sibling.
	if _, got := sel(4, without("epsilon")); got != "delta" {
		t.Fatalf("last: %q", got)
	}
	// First gone.
	if _, got := sel(0, without("alpha")); got != "beta" {
		t.Fatalf("first: %q", got)
	}
	// Everything gone: nothing selected, no panic, and it recovers.
	m, got := sel(2, Data{Root: "/x"})
	if got != "" || m.sel != -1 {
		t.Fatalf("empty: %q %d", got, m.sel)
	}
	m, _ = feed(m, key(tea.KeyDown), key(tea.KeyEnter), key(tea.KeyRight), SnapshotMsg{SampleData("full")})
	if m.selectedKey() != "alpha" {
		t.Fatalf("recover: %q", m.selectedKey())
	}
	// A selected child whose parent loses it falls back to a sibling, then the parent.
	m, _ = newSpied(t, "full")
	m, _ = feed(m, key(tea.KeyRight), key(tea.KeyDown), key(tea.KeyDown)) // alpha/weekly
	d := SampleData("full")
	d.Nodes[0].Children = d.Nodes[0].Children[:1]
	m, _ = feed(m, SnapshotMsg{d})
	if m.selectedKey() != "alpha/daily" {
		t.Fatalf("child: %q", m.selectedKey())
	}
	d.Nodes[0].Children = nil
	m, _ = feed(m, SnapshotMsg{d})
	if m.selectedKey() != "alpha" {
		t.Fatalf("parent: %q", m.selectedKey())
	}
}

func TestSnapshotKeepsExpansionAndPreviewScroll(t *testing.T) {
	m, _ := newSpied(t, "full")
	m.height = 12
	m, _ = feed(m, key(tea.KeyRight), key(tea.KeyTab), key(tea.KeyDown))
	scroll := m.scroll
	m, _ = feed(m, SnapshotMsg{SampleData("full")})
	if !m.expanded["alpha"] || len(m.rows) != 7 || m.scroll != scroll {
		t.Fatalf("expanded=%v rows=%d scroll=%d/%d", m.expanded, len(m.rows), m.scroll, scroll)
	}
}

func TestEmptyAndErrorStates(t *testing.T) {
	empty := NewWithData(SampleData("empty")).View()
	if !strings.Contains(empty, "No Groundhog instances to show.") || !strings.Contains(empty, "Nothing under this directory") {
		t.Fatalf("empty state:\n%s", empty)
	}
	errv := NewWithData(SampleData("error")).View()
	if !strings.Contains(errv, "Could not load instances") || !strings.Contains(errv, "permission denied") {
		t.Fatalf("error state:\n%s", errv)
	}
	stale := NewWithData(SampleData("stale")).View()
	if !strings.Contains(stale, "last good snapshot") || !strings.Contains(stale, "alpha") {
		t.Fatalf("stale state:\n%s", stale)
	}
	// With nothing selected the palette still offers Refresh and Quit.
	m := NewWithData(SampleData("empty"))
	if len(m.commands()) != 2 {
		t.Fatalf("commands = %v", m.commands())
	}
	m, _ = feed(m, key(tea.KeyDown), key(tea.KeyEnter), key(tea.KeyRight), key(tea.KeyEnd), key(tea.KeyTab), key(tea.KeyPgDown))
	_ = m.View()
}

// TestViewFitsEverySize resizes through a wide range and checks the frame
// never panics, never exceeds the window, and never wraps.
func TestViewFitsEverySize(t *testing.T) {
	for _, scenario := range []string{"full", "empty", "error", "stale"} {
		for _, expand := range []bool{false, true} {
			for _, withPalette := range []bool{false, true} {
				for _, focus := range []pane{paneTree, panePreview} {
					for w := 1; w <= 130; w += 3 {
						for h := 1; h <= 40; h += 3 {
							m := NewWithData(SampleData(scenario))
							if expand {
								m = m.setExpanded("alpha", true).setExpanded("epsilon", true)
							}
							m.focus = focus
							next, _ := m.Update(tea.WindowSizeMsg{Width: w, Height: h})
							m = next.(Model)
							if withPalette {
								m = m.openPalette()
								m, _ = feed(m, key(tea.KeyDown))
							}
							out := m.View()
							lines := strings.Split(out, "\n")
							if len(lines) > h {
								t.Fatalf("%s %dx%d: %d lines", scenario, w, h, len(lines))
							}
							for i, line := range lines {
								if lipgloss.Width(line) > w {
									t.Fatalf("%s %dx%d line %d is %d wide: %q", scenario, w, h, i, lipgloss.Width(line), ansi.Strip(line))
								}
							}
							// Selection stays visible in the tree window.
							if m.sel >= 0 && m.layout().showTree && (m.sel < m.treeTop || m.sel >= m.treeTop+max(1, m.treeRows())) {
								t.Fatalf("%dx%d selection %d outside window top=%d rows=%d", w, h, m.sel, m.treeTop, m.treeRows())
							}
						}
					}
				}
			}
		}
	}
}

func TestLayoutModes(t *testing.T) {
	cases := []struct {
		w, h int
		want layoutMode
	}{{160, 45, layoutSplit}, {100, 30, layoutSplit}, {89, 30, layoutStacked}, {60, 25, layoutStacked}, {60, 15, layoutStacked},
		{60, 11, layoutStacked}, {60, 10, layoutSingle}, {40, 7, layoutSingle}, {19, 30, layoutTiny}, {80, 2, layoutTiny}}
	for _, c := range cases {
		if got := computeLayout(c.w, c.h, false).mode; got != c.want {
			t.Errorf("%dx%d: mode %v, want %v", c.w, c.h, got, c.want)
		}
	}
	// In single-pane mode tab swaps which pane is shown.
	m := NewWithData(SampleData("full"))
	m, _ = feed(m, tea.WindowSizeMsg{Width: 60, Height: 10})
	if v := m.View(); !strings.Contains(v, "Instances") || strings.Contains(v, "Preview") {
		t.Fatalf("tree only expected:\n%s", v)
	}
	m, _ = feed(m, key(tea.KeyTab))
	if v := m.View(); !strings.Contains(v, "Preview") || strings.Contains(v, "Instances") {
		t.Fatalf("preview only expected:\n%s", v)
	}
}

func TestStatesAreDistinguishableWithoutColour(t *testing.T) {
	out := ansi.Strip(newSpiedView(t, 100, 30))
	for _, want := range []string{"●", "○", "◇", "⊘", "✗", "running", "stopped", "external", "unsupported", "failed"} {
		if !strings.Contains(out, want) {
			t.Errorf("view lacks %q:\n%s", want, out)
		}
	}
}

func newSpiedView(t *testing.T, w, h int) string {
	m, _ := newSpied(t, "full")
	m, _ = feed(m, tea.WindowSizeMsg{Width: w, Height: h})
	return m.View()
}

func TestUntrustedTextIsInert(t *testing.T) {
	d := Data{Root: "/r\x1b[2Jroot", Nodes: []Node{{Key: "k", Label: "evil\x1b]0;title\x07\nname", Preview: "a\x1b[31mred\x00\tb", Detail: "x\x1b[2J"}}}
	m := NewWithData(d)
	for _, v := range []string{m.View(), m.openPalette().View()} {
		if strings.ContainsAny(strings.ReplaceAll(ansi.Strip(v), "\n", ""), "\x1b\x07\x00") {
			t.Fatalf("control character leaked: %q", v)
		}
		if strings.Contains(v, "\x1b[2J") || strings.Contains(v, "\x1b]0;") {
			t.Fatalf("escape sequence leaked: %q", v)
		}
	}
}

// Init starts the clock tick (a timer, which reads nothing) and nothing else:
// countdown_test.go covers what a tick does.
func TestInitAndViewPerformNoWork(t *testing.T) {
	m := NewWithData(SampleData("full"))
	if m.Init() == nil {
		t.Fatal("Init does not start the clock tick the countdowns need")
	}
	if m.View() != m.View() {
		t.Fatal("View is not pure")
	}
}

func TestRedirectedOutputAndCancellation(t *testing.T) {
	var out bytes.Buffer
	model := NewWithData(FromView(core.View{Root: "/synthetic\x1b[2J"}))
	if err := RunModel(context.Background(), model, nil, &out, false); err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(strings.Join(strings.Fields(out.String()), " "), "No scheduled work has started") || strings.Contains(out.String(), "\x1b[2J") {
		t.Fatalf("unsafe view %q", out.String())
	}
	if n := strings.Count(out.String(), "\n"); n != defaultHeight-1 {
		t.Fatalf("single frame has %d newlines", n)
	}
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	out.Reset()
	if err := RunModel(ctx, model, nil, &out, false); !errors.Is(err, context.Canceled) || out.Len() != 0 {
		t.Fatalf("cancellation: %v, %q", err, out.String())
	}
}

func TestSnapshotRendersManyRowsScrolling(t *testing.T) {
	var nodes []Node
	for i := 0; i < 200; i++ {
		nodes = append(nodes, Node{Key: fmt.Sprint("n", i), Label: fmt.Sprint("instance-", i)})
	}
	m := NewWithData(Data{Root: "/r", Nodes: nodes})
	m, _ = feed(m, tea.WindowSizeMsg{Width: 100, Height: 20}, key(tea.KeyEnd))
	if v := m.View(); !strings.Contains(v, "instance-199") {
		t.Fatalf("end not visible:\n%s", v)
	}
	m, _ = feed(m, key(tea.KeyPgUp), key(tea.KeyPgUp))
	if m.selectedKey() == "n199" || m.sel < m.treeTop {
		t.Fatal("page up lost the selection")
	}
}

// snapshotWith returns the sample data with the given instances' actions
// replaced, and any instance named in drop removed.
func snapshotWith(actions map[string][]Action, drop ...string) SnapshotMsg {
	d := SampleData("full")
	var nodes []Node
	for _, n := range d.Nodes {
		gone := false
		for _, k := range drop {
			gone = gone || n.Key == k
		}
		if gone {
			continue
		}
		if a, ok := actions[n.Key]; ok {
			n.Actions = a
		}
		nodes = append(nodes, n)
	}
	d.Nodes = nodes
	return SnapshotMsg{Data: d}
}

// The reader typed toward Stop on alpha, a refresh
// reports alpha stopped, and Run is now offered. Enter must not send Stop or Run.
func TestPaletteRefreshFlippingStopToRunRunsNothing(t *testing.T) {
	m, sp := newSpied(t, "full")
	m, _ = feed(m, typeText(">st")...)
	m, _ = feed(m, snapshotWith(map[string][]Action{"alpha": {ActionRun, ActionOpenFolder}}))
	m, cmd := feed(m, key(tea.KeyEnter))
	if len(sp.calls) != 0 || cmd != nil {
		t.Fatalf("handler reached after the offered action changed: %v", sp.calls)
	}
	if m.palette != nil || !m.failure || !strings.Contains(m.notice, "Nothing ran") {
		t.Fatalf("no visible refusal: palette=%v notice=%q", m.palette, m.notice)
	}
}

// Case (b): alpha is selected, the reader typed toward an action, and a
// refresh removes alpha so the selection moves to a sibling offering the same
// action. Enter must not run it on the sibling.
func TestPaletteSelectionVanishingRunsNothing(t *testing.T) {
	m, sp := newSpied(t, "full")
	m, _ = feed(m, key(tea.KeyDown)) // beta, offers Run
	m, _ = feed(m, typeText(">p")...)
	// beta vanishes; selection moves to a sibling that also offers Run.
	m, _ = feed(m, snapshotWith(map[string][]Action{"epsilon": {ActionRun, ActionOpenFolder}}, "beta", "gamma", "delta"))
	if r := m.selectedRow(); r == nil || r.instance.Key == "beta" {
		t.Fatalf("setup: selection = %#v", r)
	}
	m, _ = feed(m, key(tea.KeyEnter))
	if len(sp.calls) != 0 {
		t.Fatalf("action ran on a different instance: %v", sp.calls)
	}
	if m.palette != nil || !m.failure || !strings.Contains(m.notice, "Nothing ran") {
		t.Fatalf("no visible refusal: palette=%v notice=%q", m.palette, m.notice)
	}
}

// A refresh that leaves the reader's instance and action alone changes nothing:
// the action still runs, on the instance they saw.
func TestPaletteSurvivesUnrelatedRefresh(t *testing.T) {
	m, sp := newSpied(t, "full")
	m, _ = feed(m, key(tea.KeyDown)) // beta
	m, _ = feed(m, typeText(">run")...)
	m, _ = feed(m, snapshotWith(nil, "epsilon"))
	m, _ = feed(m, key(tea.KeyEnter))
	if len(sp.calls) != 1 || sp.calls[0].Action != ActionRun || sp.calls[0].InstanceKey != "beta" {
		t.Fatalf("calls = %#v", sp.calls)
	}
}

// Case (c): a sentence typed into the wrong pane, with the palette key (">") in it, then
// Enter. Nothing may reach the handler, whatever words follow the ">".
func TestSentencesNeverReachStateChangingActions(t *testing.T) {
	for _, sentence := range []string{
		"next> run it", "ok> start", "see> resume", "x> enroll", "note> stopping",
		"hey> hold on", "so> run it now", "ok> pausing now", "re> replay",
		"ok> exit", "re> close", "re> reload", "so> rescan",
	} {
		m, sp := newSpied(t, "full")
		for _, row := range []int{0, 1} { // alpha offers Stop, beta offers Run
			m2 := m
			for i := 0; i < row; i++ {
				m2, _ = feed(m2, key(tea.KeyDown))
			}
			var cmd tea.Cmd
			m2, cmd = feed(m2, append(typeText(sentence), key(tea.KeyEnter))...)
			for _, c := range sp.calls {
				// Open folder keeps its aliases: it changes no run, and a sentence
				// that fuzzily finds it opens a file manager at worst.
				if c.Action == ActionRun || c.Action == ActionStop || c.Action == ActionRefresh {
					t.Fatalf("%q on row %d reached the handler: %v", sentence, row, sp.calls)
				}
			}
			if quits(cmd) {
				t.Fatalf("%q on row %d quit", sentence, row)
			}
		}
	}
}

// What a sentence can still reach is an exact name prefix after a ">", and
// the short forms stay usable: ">q", ">quit" and ">r" then Enter.
func TestExactNamePrefixesStillQuitAndRefresh(t *testing.T) {
	for query, want := range map[string]Action{
		"q": ActionQuit, "quit": ActionQuit, "r": ActionRefresh, "refresh": ActionRefresh,
		"q> quit": ActionQuit, // a sentence ending in the exact name still gets there
	} {
		m, sp := newSpied(t, "full")
		text := ">" + query
		if strings.Contains(query, ">") {
			text = query
		}
		m, cmd := feed(m, append(typeText(text), key(tea.KeyEnter))...)
		switch want {
		case ActionQuit:
			if !quits(cmd) {
				t.Errorf("%q then Enter did not quit", query)
			}
		default:
			if len(sp.calls) != 1 || sp.calls[0].Action != want {
				t.Errorf("%q then Enter: calls = %v", query, sp.calls)
			}
		}
	}
}

// Stop, like Run, is found by the prefix of its name and nothing else; the old
// names Play and Pause find neither.
func TestStopIsFoundByItsNamePrefixAndNoAlias(t *testing.T) {
	m, _ := newSpied(t, "full") // alpha (running) offers Stop, no Run
	names := func(m Model) string {
		var out []string
		for _, c := range m.commands() {
			out = append(out, c.name)
		}
		return strings.Join(out, ",")
	}
	for query, want := range map[string]bool{
		"s": true, "st": true, "sto": true, "stop": true, "STOP": true,
		"pause": false, "halt": false, "end": false, "release": false, "top": false, "sp": false, "pa": false, "play": false,
	} {
		mm, _ := feed(m, append([]tea.Msg{}, typeText(">"+query)...)...)
		got := false
		for _, c := range mm.filtered() {
			got = got || c.action == ActionStop
		}
		if got != want {
			t.Errorf("query %q finds Stop = %v, want %v", query, got, want)
		}
	}
	// Run is not offered where the runner runs, and Stop not where it does not.
	if got := names(m); got != "Stop,Open folder,Refresh,Quit" {
		t.Errorf("alpha offers %s", got)
	}
	m, _ = feed(m, key(tea.KeyDown))
	if got := names(m); got != "Run,Open folder,Refresh,Quit" {
		t.Errorf("beta offers %s", got)
	}
	for _, q := range []string{"play", "pause"} {
		mm, _ := feed(m, append([]tea.Msg{}, typeText(">"+q)...)...)
		for _, c := range mm.filtered() {
			if c.action == ActionRun || c.action == ActionStop {
				t.Errorf("%q still finds %s", q, c.name)
			}
		}
	}
}

// The glyph of Run and of Stop is its own: no row state draws it, so a command's
// glyph never reads as a state (Stop is ■; the state stopped is ○).
func TestRunAndStopGlyphsDifferFromEveryStateGlyph(t *testing.T) {
	for s := StateStopped; s <= StateScheduled; s++ {
		for _, a := range []Action{ActionRun, ActionStop} {
			for _, c := range allCommands {
				if c.action == a && c.glyph == s.glyph() {
					t.Errorf("%s's glyph %q is also the state %q's", c.name, c.glyph, s.label())
				}
			}
		}
	}
}

// ":r" then Enter has always refreshed, and must not start a runner because the
// selected instance happens to offer Run: "r" and "re" find Refresh first, and Run
// needs "ru" or "run". Checked on an instance that offers Run, and on one that does not.
func TestRAndReFindRefreshFirstAndRunNeedsRu(t *testing.T) {
	for _, row := range []int{0, 1} { // alpha offers Stop, beta offers Run
		for query, want := range map[string]Action{"r": ActionRefresh, "re": ActionRefresh, "ru": ActionRun, "run": ActionRun} {
			if row == 0 && want == ActionRun {
				continue // Run is not offered there
			}
			m, sp := newSpied(t, "full")
			for i := 0; i < row; i++ {
				m, _ = feed(m, key(tea.KeyDown))
			}
			m, _ = feed(m, append(typeText(">"+query), key(tea.KeyEnter))...)
			if len(sp.calls) != 1 || sp.calls[0].Action != want {
				t.Errorf("row %d, %q then Enter: calls = %v, want only %s", row, query, sp.calls, want)
			}
		}
	}
	m, _ := newSpied(t, "full")
	m, _ = feed(m, key(tea.KeyDown)) // beta offers Run
	m, _ = feed(m, typeText(">r")...)
	if top := m.filtered(); len(top) < 2 || top[0].action != ActionRefresh || top[1].action != ActionRun {
		t.Errorf(`">r" lists %v, want Refresh first, then Run`, top)
	}
}

// The name Run is reached the same way: a sentence that ends, after a ">", in
// "run" or a prefix of it runs the instance the palette was opened on.
func TestASentenceEndingInTheExactNameRunsTheInstance(t *testing.T) {
	m, sp := newSpied(t, "full")
	m, _ = feed(m, key(tea.KeyDown)) // beta offers Run
	m, _ = feed(m, append(typeText("next> run"), key(tea.KeyEnter))...)
	if len(sp.calls) != 1 || sp.calls[0].Action != ActionRun || sp.calls[0].InstanceKey != "beta" {
		t.Fatalf("calls = %v", sp.calls)
	}
}

// Aliases do not find Quit or Refresh either.
func TestQuitAndRefreshMatchOnlyTheNamePrefix(t *testing.T) {
	m, _ := newSpied(t, "full")
	for query, want := range map[string][]Action{
		"exit": nil, "close": nil, "reload": nil, "rescan": nil, "snapshot": nil,
		"qt": nil, "rf": nil, "q": {ActionQuit}, "re": {ActionRefresh}, "r": {ActionRefresh},
	} {
		mm, _ := feed(m, typeText(">"+query)...)
		var got []Action
		for _, c := range mm.filtered() {
			if c.action == ActionQuit || c.action == ActionRefresh {
				got = append(got, c.action)
			}
		}
		if fmt.Sprint(got) != fmt.Sprint(want) {
			t.Errorf("query %q finds %v, want %v", query, got, want)
		}
	}
}

// Aliases do not find Run or Pause; the names, by prefix, do.
func TestStateChangingActionsMatchOnlyTheNamePrefix(t *testing.T) {
	m, _ := newSpied(t, "full")
	m, _ = feed(m, key(tea.KeyDown)) // beta offers Run
	for query, want := range map[string]bool{
		"start": false, "enroll": false, "resume": false, "ay": false, "pa": false,
		"r": true, "ru": true, "run": true, "RUN": true,
	} {
		mm, _ := feed(m, append([]tea.Msg{}, typeText(">"+query)...)...)
		got := false
		for _, c := range mm.filtered() {
			got = got || c.action == ActionRun
		}
		if got != want {
			t.Errorf("query %q finds Run = %v, want %v", query, got, want)
		}
	}
}

// Moving the cursor follows the action, not the list position.
func TestPaletteCursorFollowsTheActionAcrossRefresh(t *testing.T) {
	m, sp := newSpied(t, "full") // alpha: Pause, Open folder
	m, _ = feed(m, typeText(">")...)
	m, _ = feed(m, key(tea.KeyDown), key(tea.KeyDown)) // Pause, Open folder
	// A refresh puts a new action first; position 1 would now be Pause's slot.
	m, _ = feed(m, snapshotWith(map[string][]Action{"alpha": {ActionRun, ActionStop, ActionOpenFolder}}))
	m, _ = feed(m, key(tea.KeyEnter))
	if len(sp.calls) != 1 || sp.calls[0].Action != ActionOpenFolder || sp.calls[0].InstanceKey != "alpha" {
		t.Fatalf("calls = %#v", sp.calls)
	}
}

// The palette opens on ">" alone (the same key as tireless-watcher's and the
// other easy apps'); ":" no longer opens it, and ">" typed into the open
// palette's query is text, not a second opening.
func TestPaletteOpensOnGreaterThanAndKeepsItAsText(t *testing.T) {
	m, _ := newSpied(t, "full")
	colon, _ := feed(m, typeText(":")...)
	if colon.palette != nil {
		t.Fatalf("\":\" opened the palette")
	}
	open, _ := feed(m, typeText(">")...)
	if open.palette == nil {
		t.Fatalf("\">\" did not open the palette")
	}
	typed, _ := feed(open, typeText("a>b")...)
	if typed.palette == nil || typed.palette.query != "a>b" {
		t.Fatalf("query after typing \"a>b\" = %+v", typed.palette)
	}
}
