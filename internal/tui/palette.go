package tui

import (
	"fmt"
	"sort"
	"strings"
	"unicode"

	tea "github.com/charmbracelet/bubbletea"
	"github.com/charmbracelet/lipgloss"
	"github.com/charmbracelet/x/ansi"

	"github.com/zealtv/easyhog/internal/core"
)

// The command palette follows EasyLoom's: every action, found by typing, run
// by Enter. Quit and everything that changes anything live here and nowhere
// else. Every action that changes anything, Quit and Refresh are strict: they match only a prefix of
// their name, with no aliases and no scattered letters, so a sentence typed
// into the wrong tmux pane reaches them only by ending, after a colon, in
// their exact name or a prefix of it ("q: quit"). Open folder keeps its
// aliases: at worst a sentence opens a file manager. The palette selects
// nothing until the reader has typed or moved, which makes ":" then Enter do
// nothing at all.

// paletteKey opens the palette.
const paletteKey = ":"

// paletteRows is how many commands the palette shows at most.
const paletteRows = 10

type palette struct {
	query string
	// chosen is the action the reader has typed toward or moved to, or "" before
	// they have typed or moved: nothing is selected, so Enter runs nothing. It
	// names an action, not a list position, so a refresh that reshuffles or
	// shrinks the list never moves it onto a different action.
	chosen Action
	// instance is the key of the instance that was selected when the palette
	// opened, "" when none was. Run, Stop and Open folder run on it or not at
	// all, however the selection has moved since.
	instance string
	// label is that instance's relative path, cleaned, which the prompt line
	// shows so the reader sees what Run or Stop will act on.
	label string
	// node is the key of the node that was selected when the palette opened (the
	// instance or one of its items). Retry and the item toggles run on that node
	// or not at all: on an item Retry retries that item's deliveries only.
	node string
}

type command struct {
	action Action
	name   string
	words  string // other names it can be found by
	glyph  string
	key    string // bare key shown beside it, if any
	// strict commands change what runs (or end it, or run scripts), so they
	// match only a prefix of their name: no aliases and no scattered letters.
	strict bool
	// wordy commands match when the query starts the name, or starts any word of
	// the name or its other names ("hidden", "show", "unsup"), never as scattered
	// letters: typing "run" finds nothing here.
	wordy bool
}

// hue colours the command's glyph the way EasyLoom does: an action that moves
// something takes the hue of the state it leads to (Run the running green,
// Stop the stopped grey), Retry the warning orange as drop does there, and the
// rest (open, refresh, show, quit) stay the quiet grey.
// The glyph and the name carry the meaning with colour off.
func (c command) hue(s styles) lipgloss.Style {
	switch {
	case c.action == ActionRun:
		return s.state[StateRunning]
	case c.action == ActionStop:
		return s.state[StateStopped]
	case c.action == ActionPauseItem, c.action == ActionPause:
		return s.state[StatePaused]
	case c.action == ActionUnpauseItem:
		return s.state[StateScheduled]
	case c.action == ActionUnpause:
		return s.state[StateRunning]
	case c.action == ActionRetry:
		return s.warn
	}
	return s.soft
}

// onInstance reports whether the command acts on the instance the palette
// was opened on.
func (c command) onInstance() bool {
	return c.action.acts() || c.action == ActionOpenFolder
}

func commandName(a Action) string {
	if a == ActionToggleHidden {
		return "Show or hide instances"
	}
	for _, c := range allCommands {
		if c.action == a {
			return c.name
		}
	}
	return string(a)
}

var allCommands = []command{
	{ActionRun, "Run", "", "▶", "", true, false},
	{ActionStop, "Stop", "", "■", "", true, false},
	// Pause item and Unpause item rename one schedule item's folder, so they are
	// strict like Run and Stop; "p" and "u" find them only on an item that offers one.
	{ActionPauseItem, "Pause item", "", "◐", "", true, false},
	{ActionUnpauseItem, "Unpause item", "", "◷", "", true, false},
	// The instance pause holds every ticker of the instance back, so it is
	// strict too; "pause in" and "pause it" tell it from Pause item.
	{ActionPause, "Pause instance", "", "◑", "", true, false},
	{ActionUnpause, "Unpause instance", "", "▷", "", true, false},
	{ActionOpenFolder, "Open folder", "finder reveal files directory explorer show", "▤", "", false, false},
	{ActionRefresh, "Refresh", "", "↻", "", true, false},
	// Retry starts runs again, so it is strict like Run and Stop. It follows
	// Refresh, so that "re" still finds Refresh first.
	{ActionRetry, "Retry", "", "↺", "", true, false},
	// ToggleHidden's name changes with the preference; see commands(). It is a view
	// action that starts nothing, so it keeps aliases and fuzzy matching.
	{ActionToggleHidden, "Show hidden instances", "show hidden hide unsupported malformed legacy filter", "◌", "", false, true},
	{ActionQuit, "Quit", "", "×", "ctrl+c", true, false},
}

// commands lists the valid commands. Run, Stop and Open folder appear only
// when the selected instance lists them in its Actions, so the capability
// state flows through the data and nothing here changes.
func (m Model) commands() []command {
	allowed := map[Action]bool{ActionRefresh: true, ActionQuit: true}
	if r := m.selectedRow(); r != nil {
		for _, a := range r.instance.Actions {
			if !a.onNode() {
				allowed[a] = true
			}
		}
		// Retry, Pause item and Unpause item are the selected node's own: an item
		// lists the Retry its deliveries earn, and the toggle its folder's name allows.
		for _, a := range r.node.Actions {
			if a.onNode() {
				allowed[a] = true
			}
		}
		if _, working := m.busy[r.instance.Key]; working {
			// One action at a time per instance; the answer is awaited.
			for a := range allowed {
				if a.acts() {
					delete(allowed, a)
				}
			}
		}
	}
	if m.hiddenTotal() > 0 {
		allowed[ActionToggleHidden] = true
	}
	var out []command
	for _, c := range allCommands {
		if allowed[c.action] {
			if c.action == ActionToggleHidden && m.showHidden {
				c.name = "Hide unsupported instances"
			}
			out = append(out, c)
		}
	}
	return out
}

type match struct {
	command
	score int
	hits  []int // rune indexes of the name the query matched
}

// filtered is the commands the query matches, best first.
func (m Model) filtered() []match {
	query := ""
	if m.palette != nil {
		query = m.palette.query
	}
	var out []match
	for _, c := range m.commands() {
		if c.wordy {
			if score, ok := wordPrefix(query, c.name+" "+c.words); ok {
				out = append(out, match{c, score, nil})
			}
			continue
		}
		if c.strict {
			if score, hits, ok := prefix(query, c.name); ok {
				if c.action == ActionRefresh {
					// ":r" then Enter has always refreshed; Run, which also starts with
					// "r", must be asked for with "ru" and so must not win the tie.
					score++
				}
				out = append(out, match{c, score, hits})
			}
			continue
		}
		score, hits, ok := fuzzy(query, c.name)
		if !ok {
			if score, _, ok = fuzzy(query, c.words); !ok {
				continue
			}
			score -= 20
			hits = nil
		}
		out = append(out, match{c, score, hits})
	}
	if query != "" {
		sort.SliceStable(out, func(i, j int) bool { return out[i].score > out[j].score })
	}
	return out
}

// prefix matches query as the start of text, ignoring case and the spaces of
// both, so "retry f" finds "Retry failed". hits are the rune indexes of text the
// query matched. An empty query matches.
func prefix(query, text string) (score int, hits []int, ok bool) {
	q := []rune(strings.ToLower(strings.ReplaceAll(query, " ", "")))
	t := []rune(strings.ToLower(text))
	ti := 0
	for _, r := range q {
		for ti < len(t) && t[ti] == ' ' {
			ti++
		}
		if ti >= len(t) || t[ti] != r {
			return 0, nil, false
		}
		hits = append(hits, ti)
		ti++
	}
	return 100 + len(q), hits, true
}

// wordPrefix matches query, ignoring case and spaces, as the start of text or of
// any of its words. An empty query matches.
func wordPrefix(query, text string) (int, bool) {
	q := strings.ToLower(strings.ReplaceAll(query, " ", ""))
	if q == "" {
		return 0, true
	}
	t := strings.ToLower(text)
	if strings.HasPrefix(t, q) {
		return 100, true
	}
	for _, w := range strings.Fields(t) {
		if strings.HasPrefix(w, q) {
			return 50, true
		}
	}
	// A query with spaces ("show hidden") may span words of the name.
	if strings.HasPrefix(strings.ReplaceAll(t, " ", ""), q) {
		return 80, true
	}
	return 0, false
}

// fuzzy matches query against text as a subsequence, ignoring case and the
// query's spaces, scoring word starts and runs above scattered letters.
func fuzzy(query, text string) (score int, hits []int, ok bool) {
	q := []rune(strings.ToLower(strings.ReplaceAll(query, " ", "")))
	if len(q) == 0 {
		return 0, nil, true
	}
	t := []rune(strings.ToLower(text))
	qi, last := 0, -2
	for ti := 0; ti < len(t) && qi < len(q); ti++ {
		if t[ti] != q[qi] {
			continue
		}
		score++
		if ti == 0 || !unicode.IsLetter(t[ti-1]) {
			score += 8
		}
		if ti == last+1 {
			score += 5
		}
		hits = append(hits, ti)
		last = ti
		qi++
	}
	if qi < len(q) {
		return 0, nil, false
	}
	score -= hits[0]
	if len(q) == len(t) {
		score += 20
	}
	return score, hits, true
}

// hiddenNotice is the status line after the toggle ran.
func (m Model) hiddenNotice() string {
	k := m.hiddenTotal()
	if m.showHidden {
		return fmt.Sprintf("showing %s that were hidden (malformed, legacy or unsupported)", plural(k, "instance", "instances"))
	}
	return fmt.Sprintf("hiding %s (malformed, legacy or unsupported); nothing was started or stopped", plural(k, "instance", "instances"))
}

func (m Model) openPalette() Model {
	m.palette = &palette{}
	if r := m.selectedRow(); r != nil {
		m.palette.instance, m.palette.label, m.palette.node = r.instance.Key, cleanLine(r.instance.Label), r.node.Key
	}
	m.notice, m.failure = "", false
	return m
}

// updatePalette takes every key while the palette is open.
func (m Model) updatePalette(k tea.KeyMsg) (tea.Model, tea.Cmd) {
	p := *m.palette
	matches := m.filtered()
	at := -1 // where the chosen action sits in the list now, -1 if absent
	for i, c := range matches {
		if c.action == p.chosen && p.chosen != "" {
			at = i
		}
	}
	pick := func(i int) {
		if i >= 0 && i < len(matches) {
			p.chosen = matches[i].action
		}
	}
	switch k.String() {
	case "esc", "ctrl+g":
		m.palette = nil
		return m, nil
	case "enter":
		if p.chosen == "" {
			return m, nil
		}
		return m.run(p)
	case "up", "ctrl+p", "shift+tab":
		pick(step(at, -1, len(matches)))
	case "down", "ctrl+n", "tab":
		pick(step(at, 1, len(matches)))
	case "pgup":
		pick(max(0, at-paletteRows))
	case "pgdown":
		pick(min(len(matches)-1, max(0, at)+paletteRows))
	case "backspace", "ctrl+h":
		if r := []rune(p.query); len(r) > 0 {
			p.query = string(r[:len(r)-1])
		}
		p.chosen = ""
	case "ctrl+u", "ctrl+w":
		p.query, p.chosen = "", ""
	default:
		if k.Type == tea.KeyRunes || k.Type == tea.KeySpace {
			p.query += cleanLine(string(k.Runes))
			if k.Type == tea.KeySpace {
				p.query += " "
			}
			p.chosen = ""
		}
	}
	m.palette = &p
	if (k.String() == "backspace" || k.String() == "ctrl+h" || k.Type == tea.KeyRunes || k.Type == tea.KeySpace) &&
		strings.TrimSpace(p.query) != "" {
		// Typing selects the best match, by action.
		if top := m.filtered(); len(top) > 0 {
			m.palette.chosen = top[0].action
		}
	}
	return m, nil
}

// cursor is the list position of the chosen action, -1 when there is none or
// the list no longer offers it.
func (m Model) cursor() int {
	if m.palette == nil || m.palette.chosen == "" {
		return -1
	}
	for i, c := range m.filtered() {
		if c.action == m.palette.chosen {
			return i
		}
	}
	return -1
}

func step(cursor, delta, n int) int {
	if n == 0 {
		return -1
	}
	if cursor < 0 {
		return 0
	}
	return (cursor + delta + n) % n
}

// refuse closes the palette and says, in the status line, why nothing ran.
func (m Model) refuse(why string) (tea.Model, tea.Cmd) {
	m.palette = nil
	m.notice, m.failure = "Nothing ran: "+why, true
	return m, nil
}

// run performs the chosen command, but only if the palette still offers
// exactly what the reader chose, on the instance it was opened on. Otherwise
// nothing runs and a notice says why. Quit goes straight to the shell; anything
// else goes through the handler.
func (m Model) run(p palette) (tea.Model, tea.Cmd) {
	var chosen *command
	for _, c := range m.filtered() {
		if c.action == p.chosen {
			c := c
			chosen = &c.command
		}
	}
	if chosen == nil {
		return m.refuse(commandName(p.chosen) + " is no longer offered or no longer matches")
	}
	a := p.chosen
	if a == ActionQuit {
		m.palette = nil
		return m, tea.Quit
	}
	if a == ActionToggleHidden {
		// A view preference the shell keeps itself: no handler, no work.
		m.palette = nil
		m = m.toggleHidden()
		m.notice, m.failure = m.hiddenNotice(), false
		return m, nil
	}
	req := ActionRequest{Action: a}
	if chosen.onInstance() {
		r := m.selectedRow()
		if p.instance == "" || r == nil || r.instance.Key != p.instance {
			return m.refuse("the instance selected for " + commandName(a) + " changed or is gone")
		}
		if a.onNode() && r.node.Key != p.node {
			return m.refuse("the row selected for " + commandName(a) + " changed or is gone")
		}
		req.NodeKey, req.InstanceKey = r.node.Key, r.instance.Key
		if a.onNode() && r.node != r.instance && r.node.Payload != nil {
			req.ItemPath = r.node.Payload.Rel
			if a == ActionRetry {
				// Delivery records carry the path an item fired under, which has no
				// `.paused` on its own folder; the toggles need the folder as it is.
				req.ItemPath = core.ItemIdentity(req.ItemPath)
			}
		}
	}
	m.palette = nil
	m.notice, m.failure = commandName(a)+"…", false
	if a.acts() {
		// The transitional state ("Running …") stands in for the notice.
		req.Label = p.label
		m.notice = ""
		if m.last != nil && m.last.instance == req.InstanceKey {
			m.last = nil
		}
		cmd := m.handler(req)
		if cmd != nil {
			m.busy = with(m.busy, req.InstanceKey, working{action: a, label: p.label})
		}
		return m, cmd
	}
	return m, m.handler(req)
}

// overlay draws the palette box over the frame, fading the frame behind it.
// On the rows the box covers, the faded frame is cleared to either side: a cut
// through the middle of the text there would leave a stray fragment ("ired, 0
// paused)") beside the box.
func (m Model) overlay(frame []string, width int) []string {
	box := strings.Split(m.renderPalette(width), "\n")
	bw := lipgloss.Width(box[0])
	x := max(0, (width-bw)/2)
	y := max(0, (len(frame)-len(box))/3)
	out := make([]string, len(frame))
	for i, line := range frame {
		if i < y || i >= y+len(box) {
			out[i] = m.styles.faint.Render(ansi.Strip(line))
			continue
		}
		out[i] = strings.Repeat(" ", x) + box[i-y] + strings.Repeat(" ", max(0, width-x-bw))
	}
	return out
}

func (m Model) renderPalette(width int) string {
	s := m.styles
	_, h := m.size()
	w := min(64, max(26, width-6))
	inner := w - 2
	p := m.palette
	matches := m.filtered()

	count := s.faint.Render(fmt.Sprintf("%d/%d ", len(matches), len(m.commands())))
	// The bound instance sits beside the count, so the typed query keeps the
	// left of the line. A long path loses its start: its end names the instance.
	if p.label != "" {
		room := inner - lipgloss.Width(count) - 16 // keep room for the query
		label := "on " + p.label
		if w := lipgloss.Width(label); w > room {
			label = "on …" + ansi.TruncateLeft(label, w-max(8, room)+4, "")
		}
		count = s.soft.Render(label) + s.faint.Render("  ") + count
	}
	query := s.selected.Render(p.query) + s.thumb.Render("▌")
	if p.query == "" {
		query = s.thumb.Render("▌") + s.faint.Render("type a command")
	}
	prompt := padTo(" "+s.brand.Render(paletteKey)+" "+query, inner-lipgloss.Width(count))
	prompt = ansi.Truncate(prompt, inner-lipgloss.Width(count), "") + count

	rows := max(1, min(paletteRows, min(len(m.commands()), h-6)))
	start := 0
	if c := m.cursor(); c >= rows {
		start = c - rows + 1
	}
	lines := []string{prompt, s.faint.Render(strings.Repeat("─", inner))}
	for i := 0; i < rows; i++ {
		at := start + i
		switch {
		case at < len(matches):
			lines = append(lines, m.paletteRow(matches[at], at == m.cursor(), inner, m.rowNote(matches[at].action)))
		case i == 0:
			lines = append(lines, padTo(s.faint.Render("   nothing matches"), inner))
		default:
			lines = append(lines, strings.Repeat(" ", inner))
		}
	}
	keys := s.hintKey.Render("↑↓") + s.hint.Render(" select   ") +
		s.hintKey.Render("enter") + s.hint.Render(" run   ") +
		s.hintKey.Render("esc") + s.hint.Render(" close")
	lines = append(lines, s.faint.Render(strings.Repeat("─", inner)), padTo(ansi.Truncate(" "+keys, inner, ""), inner))
	return s.border.Render(strings.Join(lines, "\n"))
}

// rowNote is the hint beside Retry or an item toggle, for the node the palette
// is bound to.
func (m Model) rowNote(a Action) string {
	if r := m.selectedRow(); r != nil && a.onNode() {
		return r.node.ActionNotes[a]
	}
	return ""
}

func (m Model) paletteRow(c match, selected bool, width int, note string) string {
	s := m.styles
	on := func(st lipgloss.Style) lipgloss.Style {
		if selected {
			return st.Background(s.bar.GetBackground())
		}
		return st
	}
	plain := lipgloss.NewStyle()
	lit := map[int]bool{}
	for _, h := range c.hits {
		lit[h] = true
	}
	var b strings.Builder
	for i, r := range []rune(c.name) {
		st := plain
		if selected {
			st = st.Bold(true)
		}
		if lit[i] {
			st = s.match.Inherit(st)
		}
		b.WriteString(on(st).Render(string(r)))
	}
	marker := on(plain).Render("   ")
	if selected {
		marker = on(s.selected).Render(" ❯ ")
	}
	left := marker + on(c.hue(s)).Render(c.glyph) + on(plain).Render(" ") + b.String()
	right := ""
	if c.key != "" {
		right = on(s.hintKey).Render(c.key) + on(plain).Render(" ")
	}
	if note != "" && c.key == "" && width-lipgloss.Width(left)-lipgloss.Width(note)-3 >= 2 {
		right = on(s.faint).Render(note) + on(plain).Render(" ")
	}
	if room := width - lipgloss.Width(right); lipgloss.Width(left) > room {
		left = ansi.Truncate(left, max(0, room-1), "…")
	}
	gap := max(0, width-lipgloss.Width(left)-lipgloss.Width(right))
	return left + on(plain).Render(strings.Repeat(" ", gap)) + right
}

// padTo pads a rendered line with spaces to width cells.
func padTo(line string, width int) string {
	if gap := width - lipgloss.Width(line); gap > 0 {
		return line + strings.Repeat(" ", gap)
	}
	return line
}
