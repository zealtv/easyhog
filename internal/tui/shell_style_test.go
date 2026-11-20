package tui

import (
	"strconv"
	"strings"
	"testing"

	tea "github.com/charmbracelet/bubbletea"
	"github.com/charmbracelet/lipgloss"
	"github.com/charmbracelet/x/ansi"
)

// Tests for shell-styling-pass: the header, hints, palette and pane rules take
// EasyLoom's emphasis and colour roles, and what colour says stays readable
// with colour off. `go test` has no colour profile, so as elsewhere these
// assert text and style definitions, not escape bytes.

func sameColour(a, b lipgloss.Style) bool {
	return a.GetForeground() == b.GetForeground()
}

// A header count takes the hue of what it counts, the rows' own; the plain
// text is what it was.
func TestHeaderCountsTakeTheirRowsHuesAndReadTheSameWithColourOff(t *testing.T) {
	r := newRig(t, &fakeControl{})
	m := r.m
	s := m.styles
	if sameColour(s.state[StateRunning], s.state[StateFailed]) || sameColour(s.state[StatePaused], s.state[StateFailed]) {
		t.Fatalf("running, paused and failed share a hue")
	}
	for _, c := range []struct {
		n    int
		word string
	}{{3, "running"}, {1, "paused"}, {0, "failed"}, {1, "hidden, shown"}} {
		got := ansi.Strip(m.tally(c.n, c.word, s.state[StateRunning]))
		if want := strconv.Itoa(c.n) + " " + c.word; got != want {
			t.Errorf("tally = %q, want %q", got, want)
		}
	}
	// With colour off the header keeps its plain text: the name, then the counts.
	head := ansi.Strip(m.header(120))
	if !strings.Contains(head, "Easyhog  ") || !strings.Contains(head, " instance") {
		t.Errorf("header text changed: %q", head)
	}
}

// Which pane has focus shows in the rule, not only in the label's colour.
func TestFocusedPaneRuleIsHeavyAndTheOtherLight(t *testing.T) {
	r := newRig(t, &fakeControl{})
	r.send(tea.WindowSizeMsg{Width: 120, Height: 30})
	screen := r.screen()
	if !strings.Contains(screen, "━━ Instances") || !strings.Contains(screen, "── Preview") {
		t.Fatalf("tree focused: want a heavy Instances rule and a light Preview rule:\n%s", screen)
	}
	r.send(key(tea.KeyTab))
	screen = r.screen()
	if !strings.Contains(screen, "── Instances") || !strings.Contains(screen, "━━ Preview") {
		t.Fatalf("preview focused: want a light Instances rule and a heavy Preview rule:\n%s", screen)
	}
	// Either rule fills its pane's width exactly, so nothing shifts.
	for w := 20; w <= 140; w += 7 {
		for _, focused := range []bool{true, false} {
			if got := lipgloss.Width(r.m.title("Instances", "4", focused, w)); got != w {
				t.Errorf("title at %d (focused %v) is %d cells", w, focused, got)
			}
		}
	}
}

// The hints sit three spaces apart, EasyLoom's way, with no dot between them,
// and the line still shows as many pairs as fit.
func TestHintLinePairsSitThreeSpacesApart(t *testing.T) {
	r := newRig(t, &fakeControl{})
	line := ansi.Strip(r.m.hintLine(100))
	if strings.Contains(line, "·") {
		t.Errorf("hint line still has dots: %q", line)
	}
	if !strings.Contains(line, ": commands   ↑↓ select   tab focus") {
		t.Errorf("hint pairs are not three spaces apart: %q", line)
	}
	for w := 1; w <= 100; w++ {
		if got := lipgloss.Width(r.m.hintLine(w)); got != w {
			t.Fatalf("hint line at %d is %d cells", w, got)
		}
	}
}

// Run and Stop take the hue of what they lead to, a settling action the
// warning, and the rest stay quiet; a command's name and glyph carry the same
// meaning with colour off.
func TestPaletteGlyphsTakeTheHueOfWhatTheyLeadTo(t *testing.T) {
	s := newStyles()
	hue := func(a Action) lipgloss.Style {
		for _, c := range allCommands {
			if c.action == a {
				return c.hue(s)
			}
		}
		t.Fatalf("no command for %v", a)
		return lipgloss.Style{}
	}
	if !sameColour(hue(ActionRun), s.state[StateRunning]) {
		t.Error("Run is not the running hue")
	}
	if !sameColour(hue(ActionStop), s.state[StateStopped]) {
		t.Error("Stop is not the stopped hue")
	}
	for _, a := range []Action{ActionRetry} {
		if !sameColour(hue(a), s.warn) {
			t.Errorf("%v is not the warning hue", a)
		}
	}
	for _, a := range []Action{ActionOpenFolder, ActionRefresh, ActionToggleHidden, ActionQuit} {
		if !sameColour(hue(a), s.soft) {
			t.Errorf("%v is not the quiet grey", a)
		}
	}
	if sameColour(hue(ActionRun), hue(ActionStop)) {
		t.Error("Run and Stop share a hue")
	}
}

// The palette's own key hints use the same three-space gap as the footer's.
func TestPaletteKeyHintsSitThreeSpacesApart(t *testing.T) {
	r := newRig(t, &fakeControl{})
	r.send(typeText(":")...)
	if screen := r.screen(); !strings.Contains(screen, "↑↓ select   enter run   esc close") {
		t.Fatalf("palette key hints:\n%s", screen)
	}
}
