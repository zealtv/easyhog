package tui

import (
	"strings"
	"testing"

	tea "github.com/charmbracelet/bubbletea"
	"github.com/charmbracelet/x/ansi"
)

// The focus is the instance tree, from its title row, in each layout that shows
// it, and nothing while one-pane-at-a-time shows the preview.
func TestFocusRectIsTheInstanceTree(t *testing.T) {
	for _, size := range [][2]int{{100, 30}, {57, 53}, {178, 53}} {
		m, _ := newSpied(t, "full")
		next, _ := m.Update(tea.WindowSizeMsg{Width: size[0], Height: size[1]})
		m = next.(Model)
		r := m.focusRect()
		rows := strings.Split(ansi.Strip(m.View()), "\n")
		if r.W < 1 || r.Bottom || r.Y+r.H > len(rows) {
			t.Fatalf("%v: focus %+v on %d rows", size, r, len(rows))
		}
		if title := ansi.Cut(rows[r.Y], r.X, r.X+r.W); !strings.Contains(title, "Instances") {
			t.Errorf("%v: focus starts at %q, want the tree's title", size, title)
		}
		if strings.Contains(ansi.Cut(rows[r.Y+r.H-1], r.X, r.X+r.W), "Preview") {
			t.Errorf("%v: the focus reaches the preview", size)
		}
	}
	m, _ := newSpied(t, "full")
	next, _ := m.Update(tea.WindowSizeMsg{Width: 60, Height: 9})
	m = next.(Model)
	m.focus = panePreview
	if r := m.focusRect(); r.W != 0 {
		t.Errorf("preview's turn: focus %+v, want none", r)
	}
}
