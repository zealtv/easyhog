package tui

import (
	"fmt"
	"strconv"
	"strings"

	"github.com/charmbracelet/lipgloss"
	"github.com/charmbracelet/x/ansi"
)

// View draws the whole frame as exactly height lines of at most width cells.
// It reads the model only: no I/O, and no clock (the model's own "now", set by
// its updates, is what the countdowns use).
func (m Model) View() string {
	w, h := m.size()
	l := m.layout()
	if l.mode == layoutTiny {
		return m.tinyView(w, h)
	}
	var frame []string
	if l.header {
		frame = append(frame, m.header(w))
	}
	frame = append(frame, m.body(l)...)
	frame = append(frame, m.footer(l, w)...)
	for len(frame) < h {
		frame = append(frame, strings.Repeat(" ", w))
	}
	frame = frame[:h]
	if m.palette != nil {
		frame = m.overlay(frame, w)
	}
	for i := range frame {
		frame[i] = clip(frame[i], w)
	}
	return strings.Join(frame, "\n")
}

// clip truncates a rendered line to width cells; it never wraps.
func clip(line string, width int) string {
	if lipgloss.Width(line) > width {
		return ansi.Truncate(line, width, "")
	}
	return line
}

// fit truncates with an ellipsis and pads to exactly width cells.
func fit(line string, width int) string {
	if width <= 0 {
		return ""
	}
	if lipgloss.Width(line) > width {
		line = ansi.Truncate(line, width, "…")
	}
	return padTo(line, width)
}

func (m Model) tinyView(w, h int) string {
	lines := []string{"easyhog", "window too small", "ctrl+c exits"}
	for i := range lines {
		lines[i] = fit(lines[i], w)
	}
	if h < len(lines) {
		lines = lines[:max(h, 1)]
	}
	if m.palette != nil || h < 1 {
		return strings.Join(lines[:1], "\n")
	}
	return strings.Join(lines, "\n")
}

func (m Model) header(w int) string {
	s := m.styles
	total, running, paused, failed := 0, 0, 0, 0
	for _, n := range m.data.Nodes {
		if n.Hidden && !m.showHidden {
			continue
		}
		total++
		switch n.State {
		case StateRunning:
			running++
		case StatePaused:
			paused++
		case StateFailed:
			failed++
		}
	}
	// Each count has its number in the hue of what it counts, as EasyLoom's
	// header does, and the word beside it stays quiet. The hues are the rows'
	// own (running green, paused yellow, failed red), so a count and the rows it
	// counts read as one thing. A zero recedes. The text is the same with colour
	// off.
	instances := "instances"
	if total == 1 {
		instances = "instance"
	}
	parts := []string{m.tally(total, instances, s.soft)}
	if running > 0 {
		parts = append(parts, m.tally(running, "running", s.state[StateRunning]))
	}
	if paused > 0 {
		parts = append(parts, m.tally(paused, "paused", s.state[StatePaused]))
	}
	if failed > 0 {
		parts = append(parts, m.tally(failed, "failed", s.state[StateFailed]))
	}
	if k := m.hiddenCount(); k > 0 {
		parts = append(parts, m.tally(k, "hidden", s.warn))
	} else if m.showHidden && m.hiddenTotal() > 0 {
		parts = append(parts, m.tally(m.hiddenTotal(), "hidden, shown", s.soft))
	}
	left := " " + s.brand.Render("Easyhog") + "  " + strings.Join(parts, s.faint.Render(" · "))
	// The root is quoted so path names cannot inject terminal escapes.
	root := strconv.Quote(m.data.Root)
	room := w - lipgloss.Width(left) - 3
	if room < 8 {
		return fit(left, w)
	}
	if lipgloss.Width(root) > room {
		root = "…" + ansi.TruncateLeft(root, lipgloss.Width(root)-room+1, "")
	}
	pad := w - lipgloss.Width(left) - lipgloss.Width(root) - 1
	return left + strings.Repeat(" ", max(1, pad)) + s.faint.Render(root) + " "
}

// tally is one header count: the number in the hue of what it counts, the
// word after it in the quiet grey. A count of zero recedes to faint.
func (m Model) tally(n int, word string, hue lipgloss.Style) string {
	if n == 0 {
		hue = m.styles.faint
	}
	return hue.Render(strconv.Itoa(n)) + m.styles.soft.Render(" "+word)
}

// body draws the panes into l.body.h lines.
func (m Model) body(l layout) []string {
	switch l.mode {
	case layoutSplit:
		left := m.treePane(l.tree.w, l.tree.h)
		right := m.previewPane(l.preview.w, l.preview.h)
		out := make([]string, l.body.h)
		for i := range out {
			sep := m.styles.faint.Render("│")
			if i == 0 {
				sep = m.styles.faint.Render("┬")
			}
			out[i] = left[i] + sep + right[i]
		}
		return out
	case layoutStacked:
		return append(m.treePane(l.tree.w, l.tree.h), m.previewPane(l.preview.w, l.preview.h)...)
	}
	if l.showTree {
		return m.treePane(l.tree.w, l.tree.h)
	}
	return m.previewPane(l.preview.w, l.preview.h)
}

// title draws a pane's top line as a rule with a label. The focused pane's
// label is lit and its rule is the heavy one, so which pane the arrows and
// the scroll keys act on still shows with colour off (EasyLoom has one list,
// and lights the current tab only; two panes need a cue that survives NO_COLOR).
func (m Model) title(label, extra string, focused bool, w int) string {
	s := m.styles
	st, rule := s.faint, "─"
	if focused {
		st, rule = s.brand, "━"
	}
	left := s.faint.Render(strings.Repeat(rule, 2)+" ") + st.Render(label)
	if extra != "" {
		left += " " + s.soft.Render(extra)
	}
	left += " "
	return left + s.faint.Render(strings.Repeat(rule, max(0, w-lipgloss.Width(left))))
}

func (m Model) treePane(w, h int) []string {
	s := m.styles
	out := make([]string, 0, h)
	out = append(out, clip(m.title("Instances", strconv.Itoa(len(m.data.Nodes)-m.hiddenCount()), m.focus == paneTree, w), w))
	rows := h - 1
	if len(m.rows) == 0 {
		for _, line := range m.emptyLines(w - 2) {
			out = append(out, fit(" "+line, w))
		}
	} else {
		for i := 0; i < rows && m.treeTop+i < len(m.rows); i++ {
			out = append(out, m.treeRow(m.treeTop+i, w))
		}
	}
	for len(out) < h {
		out = append(out, strings.Repeat(" ", w))
	}
	_ = s
	return out[:h]
}

// emptyLines is the tree pane's content when there is nothing to list:
// an error state when the load failed, otherwise an empty state.
func (m Model) emptyLines(w int) []string {
	s := m.styles
	var lines []string
	if m.data.Err != "" {
		lines = append(lines, s.state[StateFailed].Render("✗ Could not load instances"))
		lines = append(lines, wrapPlain(cleanLine(m.data.Err), w)...)
		lines = append(lines, "", s.hint.Render("Nothing was changed. : opens commands."))
		return lines
	}
	if k := m.hiddenCount(); k > 0 {
		lines = append(lines, s.soft.Render("No instances to show."))
		lines = append(lines, wrapPlain(fmt.Sprintf("%s hidden: malformed, legacy or unsupported. Open the palette with : and choose \"Show hidden instances\" (type \"hidden\" or \"show\").", plural(k, "instance is", "instances are")), w)...)
		return lines
	}
	lines = append(lines, s.soft.Render("No Groundhog instances to show."))
	if m.data.Note != "" {
		lines = append(lines, wrapPlain(cleanLine(m.data.Note), w)...)
	}
	return lines
}

func wrapPlain(text string, w int) []string {
	if w < 1 {
		return nil
	}
	return strings.Split(ansi.Wrap(text, w, ""), "\n")
}

// minLabelWidth is how much of a row's label the state word and the badge may
// not take; treeGap is the least blank space between the label and what follows.
const (
	minLabelWidth = 14
	treeGap       = 2
)

type seg struct {
	text  string
	style lipgloss.Style
}

func (m Model) treeRow(i, w int) string {
	s := m.styles
	r := m.rows[i]
	n := r.node
	selected := i == m.sel
	bar := s.barDim
	if m.focus == paneTree {
		bar = s.bar
	}
	disc := " "
	if len(n.Children) > 0 {
		disc = "▸"
		if m.expanded[n.Key] {
			disc = "▾"
		}
	}
	marker := "  "
	if selected {
		marker = "❯ "
	}
	label := cleanLine(n.Label)
	left := []seg{
		{marker, s.selected},
		{strings.Repeat("  ", r.depth), lipgloss.NewStyle()},
		{disc + " ", s.faint},
		{n.State.glyph(), s.state[n.State]},
		{" ", lipgloss.NewStyle()},
	}
	name := lipgloss.NewStyle()
	if selected {
		name = s.selected
	} else if n.State == StateStopped {
		name = s.soft
	}
	// The heartbeat mark sits right after the state word and goes with it: the
	// word gives way last, and then the mark goes too.
	word, dot := n.State.label(), heartbeatGlyph(n.Heartbeat)
	right := word + " "
	if dot != "" {
		right = word + " " + dot + " "
	}
	badge := ""
	fixed := 0
	for _, sg := range left {
		fixed += lipgloss.Width(sg.text)
	}
	// The state word and the badge come before the rest of the label: when the
	// pane is short of room the label is cut with an ellipsis, down to a minimum
	// of its own, and only then does the badge lose its last parts (a badge puts
	// what matters most first, so the pending-work verdict outlasts the item
	// counts), and last of all the state word goes (the glyph still says it).
	keep := min(lipgloss.Width(label), minLabelWidth)
	if fixed+keep+treeGap+lipgloss.Width(right) > w {
		right = ""
	}
	if text := m.badgeOf(n); text != "" {
		parts := strings.Split(cleanLine(text), " · ")
		for len(parts) > 0 && fixed+keep+treeGap+lipgloss.Width(strings.Join(parts, " · "))+treeGap+lipgloss.Width(right) > w {
			parts = parts[:len(parts)-1]
		}
		if len(parts) > 0 {
			badge = strings.Join(parts, " · ") + "  "
		}
	}
	room := w - fixed - lipgloss.Width(badge) - lipgloss.Width(right)
	if badge != "" || right != "" {
		room -= treeGap // never let the label touch what follows it
	}
	room = max(room, 0)
	if lipgloss.Width(label) > room {
		label = ansi.Truncate(label, room, "…")
	}
	left = append(left, seg{label, name})
	used := fixed + lipgloss.Width(label)
	gap := max(0, w-used-lipgloss.Width(badge)-lipgloss.Width(right))
	segs := append(left, seg{strings.Repeat(" ", gap), lipgloss.NewStyle()}, seg{badge, s.faint})
	switch {
	case right == "":
	case dot == "":
		segs = append(segs, seg{right, s.state[n.State]})
	default:
		segs = append(segs, seg{word, s.state[n.State]}, seg{" ", lipgloss.NewStyle()}, seg{dot, s.beat[n.Heartbeat]}, seg{" ", lipgloss.NewStyle()})
	}
	var b strings.Builder
	for _, sg := range segs {
		st := sg.style
		if selected {
			st = st.Background(bar.GetBackground())
		}
		b.WriteString(st.Render(sg.text))
	}
	return clip(b.String(), w)
}

// previewLines is the preview text wrapped to the pane's text width.
func (m Model) previewLines(w int) []string {
	r := m.selectedRow()
	if r == nil {
		return nil
	}
	n := r.node
	var lines []string
	s := m.styles
	head := s.state[n.State].Render(n.State.glyph()) + " " + lipgloss.NewStyle().Bold(true).Render(cleanLine(n.Label))
	lines = append(lines, ansi.Truncate(head, w, "…"))
	// The state word, then the heartbeat mark in its own colour when the row has
	// one, then the rest in the quiet style.
	sub := s.soft.Render(n.State.label())
	if dot := heartbeatGlyph(n.Heartbeat); dot != "" {
		sub += s.soft.Render(" ") + s.beat[n.Heartbeat].Render(dot)
	}
	rest := ""
	if r.depth > 0 {
		rest += " · in " + cleanLine(r.instance.Label)
	}
	if n.Detail != "" {
		rest += " · " + cleanLine(n.Detail)
	}
	if rest != "" {
		sub += s.soft.Render(rest)
	}
	lines = append(lines, ansi.Truncate(sub, w, "…"), "")
	if l := m.last; l != nil && l.instance == r.instance.Key {
		style := s.soft
		if l.failure {
			style = s.warn
		}
		for _, line := range wrapPlain("Last action: "+cleanLine(l.text), w) {
			lines = append(lines, style.Render(line))
		}
		lines = append(lines, "")
	}
	if strings.TrimSpace(n.Preview) == "" {
		return append(lines, s.faint.Render("Nothing to preview."))
	}
	body := clean(n.Preview)
	if n.Payload != nil && m.readPayload != nil {
		body = strings.TrimRight(body, "\n")
	}
	for i, line := range strings.Split(body, "\n") {
		lines = append(lines, m.previewLine(line, i == 0, w)...)
	}
	if n.Payload != nil && m.readPayload != nil {
		lines = append(lines, "", s.section.Render("Payload, read as text (nothing is run)"))
		text := "reading…"
		if m.payload.key == n.Key && m.payload.loaded {
			text = m.payload.text
		}
		text = strings.TrimRight(clean(text), "\n")
		if m.colour {
			return append(lines, stylePayload(text, w, s)...)
		}
		for _, line := range strings.Split(text, "\n") {
			lines = append(lines, wrapPlain(line, w)...)
		}
	}
	return lines
}

// previewLine wraps one line of an adapter preview. With colour on, a line of a
// shape preview_style.go knows is styled; any other line, and every line with
// colour off, is plain text exactly as the adapter wrote it.
func (m Model) previewLine(line string, first bool, w int) []string {
	if m.colour {
		if styled := stylePreviewLine(line, first, w, m.styles); styled != nil {
			return styled
		}
	}
	return wrapPlain(line, w)
}

func (m Model) previewWidth() int {
	l := m.layout()
	if !l.show {
		l = computeLayout(m.sizeW(), m.sizeH(), true)
	}
	return max(1, l.preview.w-2)
}

func (m Model) previewHeight() int {
	l := m.layout()
	if !l.show {
		l = computeLayout(m.sizeW(), m.sizeH(), true)
	}
	return max(1, l.preview.h-1)
}

func (m Model) maxScroll() int {
	return max(0, len(m.previewLines(m.previewWidth()))-m.previewHeight())
}

func (m Model) previewPane(w, h int) []string {
	lines := m.previewLines(max(1, w-2))
	rows := h - 1
	top := max(0, min(m.scroll, max(0, len(lines)-rows)))
	extra := ""
	if len(lines) > rows {
		extra = fmt.Sprintf("%d–%d of %d", top+1, min(len(lines), top+rows), len(lines))
	}
	out := []string{clip(m.title("Preview", extra, m.focus == panePreview, w), w)}
	if lines == nil {
		out = append(out, fit(" "+m.styles.faint.Render("Nothing selected."), w))
	}
	for i := 0; i < rows && top+i < len(lines); i++ {
		out = append(out, fit(" "+lines[top+i], w))
	}
	for len(out) < h {
		out = append(out, strings.Repeat(" ", w))
	}
	return out[:h]
}

type hint struct{ key, label string }

// footer is the status line (when there is room) and the key hints.
func (m Model) footer(l layout, w int) []string {
	status := m.status(w)
	hints := m.hintLine(w)
	if l.statusRow {
		return []string{status, hints}
	}
	if m.notice != "" || m.data.Err != "" || len(m.busy) > 0 {
		return []string{status}
	}
	return []string{hints}
}

// status shows the latest action result, else a load error, else a quiet line.
func (m Model) status(w int) string {
	s := m.styles
	var text string
	switch {
	case m.notice != "" && m.failure:
		text = s.warn.Render(" ! " + m.notice)
	case m.notice != "":
		text = s.soft.Render(" " + m.notice)
	case len(m.busy) > 0:
		text = s.soft.Render(" " + m.busyText())
	case m.data.Err != "" && len(m.rows) > 0:
		text = s.warn.Render(" ! showing the last good snapshot: " + cleanLine(m.data.Err))
	case m.data.Err != "":
		text = s.warn.Render(" ! load failed")
	case m.hiddenCount() > 0:
		text = s.faint.Render(fmt.Sprintf(" %d hidden (malformed, legacy or unsupported) · : then \"show\" lists them", m.hiddenCount()))
	default:
		text = s.faint.Render(" nothing runs until you choose it · : opens commands")
	}
	return fit(text, w)
}

// hintLine fits as many hints as the width allows, most important first. As in
// EasyLoom the key is brighter than its label and pairs sit three spaces
// apart, so the line scans by key rather than by punctuation.
func (m Model) hintLine(w int) string {
	s := m.styles
	all := []hint{{":", "commands"}, {"↑↓", "select"}, {"tab", "focus"}, {"←→", "fold"}, {"⇧↑↓", "scroll"}, {"ctrl+c", "quit"}}
	var b strings.Builder
	b.WriteString(" ")
	used := 1
	for i, h := range all {
		piece := s.hintKey.Render(h.key) + s.hint.Render(" "+h.label)
		pw := lipgloss.Width(h.key) + 1 + lipgloss.Width(h.label)
		if i > 0 {
			pw += 3
		}
		if used+pw > w {
			break
		}
		if i > 0 {
			b.WriteString("   ")
		}
		b.WriteString(piece)
		used += pw
	}
	return fit(b.String(), w)
}
