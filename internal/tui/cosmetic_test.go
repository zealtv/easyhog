package tui

import (
	"strings"
	"testing"

	tea "github.com/charmbracelet/bubbletea"
)

// The palette box is drawn over a faded frame. On the rows it covers, nothing
// of that frame may remain beside it: a cut through a line left fragments such
// as "ired, 0 paused)".
func TestPaletteOverlayLeavesNoFragmentsBesideTheBox(t *testing.T) {
	for _, size := range [][2]int{{100, 30}, {120, 40}, {80, 24}, {64, 20}} {
		r := newRig(t, &fakeControl{})
		r.send(tea.WindowSizeMsg{Width: size[0], Height: size[1]})
		r.send(typeText(":pl")...)
		lines := strings.Split(r.screen(), "\n")
		top, bottom := -1, -1
		for i, l := range lines {
			if top < 0 && strings.ContainsRune(l, '╭') {
				top = i
			}
			if strings.ContainsRune(l, '╰') {
				bottom = i
			}
		}
		if top < 0 || bottom < top {
			t.Fatalf("%dx%d: no palette box in\n%s", size[0], size[1], r.screen())
		}
		left := len([]rune(lines[top][:strings.IndexRune(lines[top], '╭')]))
		right := left + len([]rune(strings.TrimRight(lines[top][strings.IndexRune(lines[top], '╭'):], " ")))
		for i := top; i <= bottom; i++ {
			rs := []rune(lines[i])
			if left > len(rs) || right > len(rs) {
				t.Fatalf("%dx%d: row %d is %d cells, the box spans %d..%d", size[0], size[1], i, len(rs), left, right)
			}
			if beside := strings.TrimSpace(string(rs[:left]) + string(rs[right:])); beside != "" {
				t.Errorf("%dx%d: row %d has %q beside the box:\n%s", size[0], size[1], i, beside, r.screen())
			}
		}
	}
}

// With no notice, no error and nothing running, the status line used to say
// "inspect only" although Run and Stop exist. It now says what is true
// whatever the instance: nothing runs until the reader chooses it.
func TestQuietStatusLineNoLongerSaysInspectOnly(t *testing.T) {
	r := newRig(t, &fakeControl{})
	screen := r.screen()
	if strings.Contains(screen, "inspect only") {
		t.Fatalf("stale footer text:\n%s", screen)
	}
	if !strings.Contains(screen, "nothing runs until you choose it") {
		t.Fatalf("quiet status line missing:\n%s", screen)
	}
}
