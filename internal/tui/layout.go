package tui

// Default size for the single frame printed to redirected output, and for a
// window that has not reported its size yet.
const (
	defaultWidth  = 80
	defaultHeight = 24
)

// maxTreeWidth bounds the tree pane in the side-by-side layout. It takes two
// fifths of the window, so it grows with a wide terminal (64 columns at 160, where
// it once stopped at 60) and stops where a longer row would only add blank space.
const maxTreeWidth = 100

type rect struct{ x, y, w, h int }

func (r rect) contains(x, y int) bool { return x >= r.x && x < r.x+r.w && y >= r.y && y < r.y+r.h }

type layoutMode int

const (
	layoutSplit   layoutMode = iota // tree left, preview right
	layoutStacked                   // tree above preview
	layoutSingle                    // one pane at a time; tab swaps
	layoutTiny                      // too small for panes
)

// layout is the screen divided up. Both panes' first line is their title.
type layout struct {
	mode           layoutMode
	w, h           int
	header         bool // one header line on top
	statusRow      bool // a status line above the key hints
	body           rect
	tree, preview  rect // zero when hidden
	showTree, show bool
}

// computeLayout adapts to the window: side by side when wide, stacked when
// narrow but tall enough, one pane at a time (tab swaps) when small, and a
// plain notice when there is no room for anything.
func computeLayout(w, h int, focusPreview bool) layout {
	l := layout{w: w, h: h}
	if w < 20 || h < 3 {
		l.mode = layoutTiny
		return l
	}
	switch {
	case h >= 10:
		l.header, l.statusRow = true, true
		l.body = rect{0, 1, w, h - 3}
	case h >= 6:
		l.header = true
		l.body = rect{0, 1, w, h - 2}
	default:
		l.body = rect{0, 0, w, h - 1}
	}
	b := l.body
	switch {
	case w >= 90:
		l.mode = layoutSplit
		tw := min(maxTreeWidth, max(34, w*2/5))
		l.tree = rect{b.x, b.y, tw, b.h}
		l.preview = rect{b.x + tw + 1, b.y, w - tw - 1, b.h}
		l.showTree, l.show = true, true
	case b.h >= 8:
		l.mode = layoutStacked
		th := b.h / 2
		l.tree = rect{b.x, b.y, w, th}
		l.preview = rect{b.x, b.y + th, w, b.h - th}
		l.showTree, l.show = true, true
	default:
		l.mode = layoutSingle
		if focusPreview {
			l.preview, l.show = b, true
		} else {
			l.tree, l.showTree = b, true
		}
	}
	return l
}
