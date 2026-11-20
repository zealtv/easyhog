// Package tui presents headless state in a terminal shell: an instance tree, a
// preview pane, a status line and a searchable command palette. Rendering
// never accesses the filesystem or starts processes; slow work arrives as
// messages.
//
// Key safety is the core rule. Arrows, Tab and scroll keys only inspect, and
// Enter may inspect. Bare letters, Space, Enter and mouse clicks never start,
// stop or quit anything. ":" opens the palette; Quit and every action live
// only there. ctrl+c is the one chord that always exits.
package tui

import (
	"context"
	"errors"
	"io"
	"sort"
	"strings"
	"time"

	tea "github.com/charmbracelet/bubbletea"
)

type pane int

const (
	paneTree pane = iota
	panePreview
)

// Model is the Bubble Tea model for the shell.
type Model struct {
	data    Data
	handler ActionHandler
	styles  styles
	// colour is whether the terminal gets colour at all (see colourOn). With it
	// off the preview shows text exactly as it did before styling existed.
	colour   bool
	width    int
	height   int
	expanded map[string]bool
	rows     []row
	sel      int // index into rows, -1 when empty
	treeTop  int // first visible row of the tree
	scroll   int // first visible line of the preview
	focus    pane
	// showHidden is the reader's view preference: list the instances marked
	// Node.Hidden too. It starts false, and nothing but the palette changes it.
	showHidden bool
	palette    *palette
	onSelect   func(instanceKey string)
	// engaged becomes true at the reader's first choosing key (see navigates and
	// readerMoves) and false again when something else moves the highlight. While
	// false the observer holds no selection: no script runs for the instance
	// highlighted at startup or left there by a rescan.
	engaged bool
	refresh func()
	notice  string
	failure bool // notice reports an error

	readPayload func(PayloadRef) string
	openFolder  func(dir string) error
	control     Control
	reread      func(instanceKey string)
	payload     payloadState
	// busy holds the action under way for each instance key (Run, Stop, Retry, a pause). It is
	// replaced, never edited, since a Model is copied by value.
	busy map[string]working
	// last is the newest such action's outcome, kept whole for the preview of
	// its instance: the status line holds one line and can cut a long reason.
	last *outcome
	// clock is where "now" comes from (WithClock); now is its reading at the
	// start of the latest update, which is all the drawing uses. clockEvery is the
	// interval of the live clock tick chain (see countdown.go).
	clock      func() time.Time
	now        time.Time
	clockEvery time.Duration
	// noTicks stops the shell scheduling its own clock ticks (WithoutClockTicks).
	noTicks bool
}

// outcome is a finished action on an instance: whose, and the notice that was shown.
type outcome struct {
	instance string
	text     string
	failure  bool
}

// working is an action on an instance the ticker has not yet answered.
type working struct {
	action Action
	label  string
}

// payloadState is the payload text for the selected node, once read.
type payloadState struct {
	key     string
	text    string
	loaded  bool
	loading bool
}

// Option customises a Model.
type Option func(*Model)

// WithActions sets the handler that palette actions are sent to. Without one,
// every action other than Quit reports "not available yet".
func WithActions(h ActionHandler) Option { return func(m *Model) { m.handler = h } }

// WithSelectionObserver calls f with the key of the selected instance, empty
// when none, each time that changes. The call runs as a command, off the update
// loop, and only reports: the shell stays the owner of selection, by key.
//
// No script runs before a keypress. The instance highlighted at startup is not
// reported. The first key press that selects or moves (an arrow, page, home or
// end key, or a click on a row) reports the selection then in force, even when
// it does not change; after that, each change the reader makes in the tree is
// reported. A highlight moved by anything else (a rescan that removed its
// instance, the hidden filter) reports "" and waits for the reader's next
// choosing key, so a script runs only after the reader chooses an instance, or on an explicit Refresh.
func WithSelectionObserver(f func(instanceKey string)) Option {
	return func(m *Model) { m.onSelect = f }
}

// WithRefresh sets what the palette's Refresh does. f must return at once and
// do its work elsewhere: Refresh is the reader's deliberate request to inspect
// every instance, and the new snapshots arrive as SnapshotMsg.
func WithRefresh(f func()) Option { return func(m *Model) { m.refresh = f } }

// WithFolderOpener sets what the palette's Open folder does: f shows the
// instance directory at the given path in the file manager. It runs as a
// command, off the update loop, and only for the instance the palette was
// opened on. It must run nothing that lives inside that directory.
func WithFolderOpener(f func(dir string) error) Option { return func(m *Model) { m.openFolder = f } }

// WithPayloads sets how an item's files are read for the preview. f runs as a
// command, off the update loop and the render loop, each time an item is
// selected or its snapshot changes; it must only read, and return inert text.
func WithPayloads(f func(PayloadRef) string) Option { return func(m *Model) { m.readPayload = f } }

// NewWithData shows the given data.
func NewWithData(data Data, opts ...Option) Model {
	m := Model{styles: newStyles(), colour: colourOn(), expanded: map[string]bool{}, sel: -1}
	for _, o := range opts {
		o(&m)
	}
	if m.handler == nil {
		m.handler = unavailable
	}
	if m.refresh != nil {
		inner, refresh := m.handler, m.refresh
		m.handler = func(req ActionRequest) tea.Cmd {
			if req.Action != ActionRefresh {
				return inner(req)
			}
			return func() tea.Msg {
				refresh()
				return ResultMsg{Action: req.Action, Message: "rescanning and inspecting every instance"}
			}
		}
	}
	if m.openFolder != nil {
		inner, open := m.handler, m.openFolder
		m.handler = func(req ActionRequest) tea.Cmd {
			if req.Action != ActionOpenFolder {
				return inner(req)
			}
			dir := req.InstanceKey
			return func() tea.Msg {
				if dir == "" {
					return ResultMsg{Action: req.Action, Err: errors.New("no instance is selected")}
				}
				if err := open(dir); err != nil {
					return ResultMsg{Action: req.Action, Err: err}
				}
				return ResultMsg{Action: req.Action, Message: "showing the folder in the file manager"}
			}
		}
	}
	if m.control != nil {
		m.handler = controlHandler(m.handler, m.control, m.reread)
	}
	m.now = m.clockNow()
	m = m.load(data)
	m.clockEvery = m.clockInterval()
	return m
}

// unavailable is the default handler: nothing implements the action yet.
func unavailable(req ActionRequest) tea.Cmd {
	return func() tea.Msg { return ResultMsg{Action: req.Action, Err: ErrNotAvailable} }
}

// Init starts the clock tick that keeps the countdowns in item rows moving.
func (m Model) Init() tea.Cmd {
	if m.clockEvery <= 0 || m.noTicks {
		return nil
	}
	return tickClock(m.clockEvery)
}

// size is the window size, or the default before one is known.
func (m Model) size() (int, int) {
	w, h := m.width, m.height
	if w <= 0 {
		w = defaultWidth
	}
	if h <= 0 {
		h = defaultHeight
	}
	return w, h
}

func (m Model) layout() layout {
	w, h := m.size()
	return computeLayout(w, h, m.focus == panePreview)
}

// load installs a snapshot, keeping selection and expansion by key.
func (m Model) load(data Data) Model {
	oldRows, oldSel, oldKey := m.rows, m.sel, m.selectedKey()
	m.data = data
	m.rows = flatten(data.Nodes, m.expanded, m.showHidden)
	m.sel = reselect(oldRows, oldSel, m.rows)
	if m.selectedKey() != oldKey {
		m.scroll = 0
	}
	return m.settle()
}

func (m Model) selectedRow() *row {
	if m.sel < 0 || m.sel >= len(m.rows) {
		return nil
	}
	return &m.rows[m.sel]
}

func (m Model) selectedKey() string {
	if r := m.selectedRow(); r != nil {
		return r.node.Key
	}
	return ""
}

// settle keeps the selection in the tree window and the preview scroll in range.
func (m Model) settle() Model {
	rows := m.treeRows()
	if rows < 1 {
		rows = 1
	}
	if m.sel >= 0 {
		if m.sel < m.treeTop {
			m.treeTop = m.sel
		}
		if m.sel >= m.treeTop+rows {
			m.treeTop = m.sel - rows + 1
		}
	}
	m.treeTop = max(0, min(m.treeTop, max(0, len(m.rows)-rows)))
	m.scroll = max(0, min(m.scroll, m.maxScroll()))
	return m
}

// treeRows is how many tree rows fit in the tree pane, below its title.
func (m Model) treeRows() int {
	l := m.layout()
	if !l.showTree {
		// Hidden: size it as the single-pane layout would so the selection
		// is in range when Tab brings the tree back.
		l = computeLayout(m.sizeW(), m.sizeH(), false)
	}
	return l.tree.h - 1
}

func (m Model) sizeW() int { w, _ := m.size(); return w }
func (m Model) sizeH() int { _, h := m.size(); return h }

// selectedInstance is the key of the selected row's instance.
func (m Model) selectedInstance() string {
	if r := m.selectedRow(); r != nil && r.instance != nil {
		return r.instance.Key
	}
	return ""
}

func (m Model) Update(msg tea.Msg) (tea.Model, tea.Cmd) {
	m.now = m.clockNow()
	before, beforeKey := m.selectedInstance(), m.selectedKey()
	reader := m.readerMoves(msg)
	firstMove := !m.engaged && reader && navigates(msg)
	next, cmd := m.update(msg)
	nm, ok := next.(Model)
	if !ok {
		return next, cmd
	}
	if firstMove {
		nm.engaged = true
	}
	if nm.onSelect != nil {
		// Only the reader choosing in the tree reports an instance. A highlight
		// moved by anything else (a rescan that dropped the instance, the hidden
		// filter) keeps a neighbour highlighted but reports no selection, as at
		// startup, until the reader's next choosing key.
		after := nm.selectedInstance()
		report, key := false, after
		switch {
		case nm.engaged && reader && (after != before || firstMove):
			report = true
		case nm.engaged && !reader && after != before:
			nm.engaged, report, key = false, true, ""
		}
		if report {
			f := nm.onSelect
			cmd = tea.Batch(cmd, func() tea.Msg { f(key); return nil })
		}
	}
	nm, cmd = nm.speedUpClock(cmd)
	_, isSnapshot := msg.(SnapshotMsg)
	if key := nm.selectedKey(); key != beforeKey || isSnapshot {
		var pc tea.Cmd
		nm, pc = nm.wantPayload(key != beforeKey)
		cmd = tea.Batch(cmd, pc)
	}
	return nm, cmd
}

// readerMoves reports whether msg is the reader acting on the tree: a key press
// with tree focus (the palette closed), or a click on a tree row. A key with
// preview focus only scrolls the preview, and the palette's keys act on actions.
func (m Model) readerMoves(msg tea.Msg) bool {
	if m.palette != nil {
		return false
	}
	switch msg := msg.(type) {
	case tea.KeyMsg:
		return !msg.Paste && m.focus == paneTree
	case tea.MouseMsg:
		if msg.Button != tea.MouseButtonLeft || msg.Action != tea.MouseActionPress {
			return false
		}
		l := m.layout()
		if l.mode == layoutTiny || !l.showTree || !l.tree.contains(msg.X, msg.Y) {
			return false
		}
		i := m.treeTop + msg.Y - l.tree.y - 1
		return msg.Y > l.tree.y && i >= 0 && i < len(m.rows)
	}
	return false
}

// hiddenCount is how many instances the filter leaves out of the tree now.
func (m Model) hiddenCount() int {
	if m.showHidden {
		return 0
	}
	k := 0
	for _, n := range m.data.Nodes {
		if n.Hidden {
			k++
		}
	}
	return k
}

// hiddenTotal is how many instances are marked hidden, shown or not.
func (m Model) hiddenTotal() int {
	k := 0
	for _, n := range m.data.Nodes {
		if n.Hidden {
			k++
		}
	}
	return k
}

// toggleHidden flips the view preference and rebuilds the rows. The selection
// follows its key; when its instance goes, reselect picks a visible neighbour.
func (m Model) toggleHidden() Model {
	m.showHidden = !m.showHidden
	old, oldSel, oldKey := m.rows, m.sel, m.selectedKey()
	m.rows = flatten(m.data.Nodes, m.expanded, m.showHidden)
	m.sel = reselect(old, oldSel, m.rows)
	if m.selectedKey() != oldKey {
		m.scroll = 0
	}
	return m.settle()
}

// navigates reports whether msg is a key press that selects or moves (or a
// click that selects), the reader's first sign of attention. The palette and
// quit keys, Tab and plain scrolling of the preview do not count.
func navigates(msg tea.Msg) bool {
	switch msg := msg.(type) {
	case tea.KeyMsg:
		if msg.Paste {
			return false
		}
		switch msg.String() {
		case "up", "down", "pgup", "pgdown", "home", "end", "ctrl+u", "ctrl+d":
			return true
		}
	case tea.MouseMsg:
		return msg.Button == tea.MouseButtonLeft && msg.Action == tea.MouseActionPress
	}
	return false
}

// wantPayload starts reading the selected item's files, when it has any. A
// changed selection forgets the previous text at once; a new snapshot for the
// same item keeps showing the old text until the new arrives.
func (m Model) wantPayload(changed bool) (Model, tea.Cmd) {
	r := m.selectedRow()
	if changed || r == nil || r.node.Payload == nil || m.payload.key != r.node.Key {
		m.payload = payloadState{}
	}
	if r == nil || r.node.Payload == nil || m.readPayload == nil {
		return m, nil
	}
	ref, key, read := *r.node.Payload, r.node.Key, m.readPayload
	m.payload.key, m.payload.loading = key, true
	return m, func() tea.Msg { return PayloadMsg{Key: key, Text: read(ref)} }
}

func (m Model) update(msg tea.Msg) (tea.Model, tea.Cmd) {
	switch msg := msg.(type) {
	case tea.WindowSizeMsg:
		// Keep the last good size if a terminal reports zeros.
		if msg.Width > 0 {
			m.width = msg.Width
		}
		if msg.Height > 0 {
			m.height = msg.Height
		}
		return m.settle(), nil
	case tea.KeyMsg:
		return m.updateKey(msg)
	case tea.MouseMsg:
		return m.updateMouse(msg), nil
	case clockMsg:
		return m.onClock(msg)
	case SnapshotMsg:
		return m.load(msg.Data), nil
	case PayloadMsg:
		if msg.Key == m.selectedKey() && m.payload.key == msg.Key {
			m.payload.text, m.payload.loaded, m.payload.loading = msg.Text, true, false
		}
		return m.settle(), nil
	case ResultMsg:
		if _, ok := m.busy[msg.InstanceKey]; ok && msg.InstanceKey != "" {
			m.busy = without(m.busy, msg.InstanceKey)
		}
		m.notice, m.failure = resultText(msg), msg.Err != nil
		if msg.InstanceKey != "" && msg.Action.acts() {
			m.last = &outcome{instance: msg.InstanceKey, text: m.notice, failure: m.failure}
		}
		return m, nil
	}
	return m, nil
}

// with and without return a copy of busy with one instance added or removed.
func with(busy map[string]working, key string, w working) map[string]working {
	out := make(map[string]working, len(busy)+1)
	for k, v := range busy {
		out[k] = v
	}
	out[key] = w
	return out
}

func without(busy map[string]working, key string) map[string]working {
	out := make(map[string]working, len(busy))
	for k, v := range busy {
		if k != key {
			out[k] = v
		}
	}
	return out
}

// busyText is the transitional state: what Run or Stop has been asked of
// which instances and not yet answered, in tree order.
func (m Model) busyText() string {
	var parts []string
	for _, r := range m.rows {
		if r.parent != "" {
			continue
		}
		if w, ok := m.busy[r.node.Key]; ok {
			parts = append(parts, w.verb()+" "+w.label+"…")
		}
	}
	if len(parts) == 0 { // the instance is not in view: say so rather than hide the work
		for _, w := range m.busy {
			parts = append(parts, w.verb()+" "+w.label+"…")
		}
		sort.Strings(parts)
	}
	return strings.Join(parts, "  ")
}

func (w working) verb() string {
	switch w.action {
	case ActionStop:
		return "Stopping"
	case ActionRetry:
		return "Retrying"
	case ActionPause:
		return "Pausing"
	case ActionUnpause:
		return "Unpausing"
	case ActionPauseItem:
		return "Pausing"
	case ActionUnpauseItem:
		return "Unpausing"
	}
	return "Running"
}

func resultText(r ResultMsg) string {
	name := commandName(r.Action)
	switch {
	case r.Err != nil:
		return name + ": " + cleanLine(r.Err.Error())
	case r.Message != "":
		return name + ": " + cleanLine(r.Message)
	}
	return name + ": done"
}

// updateKey is the whole key routing table. Everything that is not listed here
// does nothing, so adding a binding means adding it here, and the routing
// tests fail if it quits or starts anything outside the palette.
func (m Model) updateKey(k tea.KeyMsg) (tea.Model, tea.Cmd) {
	if k.Type == tea.KeyCtrlC {
		return m, tea.Quit
	}
	if m.palette != nil {
		return m.updatePalette(k)
	}
	if k.Paste {
		return m, nil
	}
	switch k.String() {
	case paletteKey:
		return m.openPalette(), nil
	case "tab", "shift+tab":
		if m.focus == paneTree {
			m.focus = panePreview
		} else {
			m.focus = paneTree
		}
		return m.settle(), nil
	case "up":
		return m.arrow(-1), nil
	case "down":
		return m.arrow(1), nil
	case "pgup":
		return m.arrow(-m.page()), nil
	case "pgdown":
		return m.arrow(m.page()), nil
	case "ctrl+u":
		return m.arrow(-max(1, m.page()/2)), nil
	case "ctrl+d":
		return m.arrow(max(1, m.page()/2)), nil
	case "shift+up":
		return m.scrollBy(-1), nil
	case "shift+down":
		return m.scrollBy(1), nil
	case "home":
		return m.jump(0), nil
	case "end":
		return m.jump(len(m.rows) - 1), nil
	case "right":
		return m.fold(true), nil
	case "left":
		return m.fold(false), nil
	case "enter":
		// Inspect: open or close the selected node. Enter never acts on it.
		return m.toggle(), nil
	}
	return m, nil
}

func (m Model) page() int {
	l := m.layout()
	if m.focus == panePreview && l.show {
		return max(1, l.preview.h-2)
	}
	return max(1, m.treeRows()-1)
}

// arrow moves the selection with tree focus and scrolls the preview with
// preview focus.
func (m Model) arrow(delta int) Model {
	if m.focus == panePreview {
		return m.scrollBy(delta)
	}
	return m.jump(m.sel + delta)
}

func (m Model) jump(i int) Model {
	if len(m.rows) == 0 {
		return m
	}
	i = max(0, min(i, len(m.rows)-1))
	if i != m.sel {
		m.sel, m.scroll = i, 0
	}
	return m.settle()
}

func (m Model) scrollBy(delta int) Model {
	m.scroll += delta
	return m.settle()
}

// fold opens (right) or closes (left) the selected node. Left on a closed node
// or an item moves to its parent.
func (m Model) fold(open bool) Model {
	r := m.selectedRow()
	if r == nil || m.focus == panePreview {
		return m
	}
	n := r.node
	switch {
	case open && len(n.Children) > 0 && !m.expanded[n.Key]:
		return m.setExpanded(n.Key, true)
	case open && len(n.Children) > 0:
		return m.jump(m.sel + 1)
	case !open && len(n.Children) > 0 && m.expanded[n.Key]:
		return m.setExpanded(n.Key, false)
	case !open && r.parent != "":
		if i, ok := indexByKey(m.rows)[r.parent]; ok {
			return m.jump(i)
		}
	}
	return m
}

func (m Model) toggle() Model {
	r := m.selectedRow()
	if r == nil || len(r.node.Children) == 0 || m.focus == panePreview {
		return m
	}
	return m.setExpanded(r.node.Key, !m.expanded[r.node.Key])
}

func (m Model) setExpanded(key string, open bool) Model {
	next := make(map[string]bool, len(m.expanded)+1)
	for k, v := range m.expanded {
		next[k] = v
	}
	next[key] = open
	m.expanded = next
	old, oldSel := m.rows, m.sel
	m.rows = flatten(m.data.Nodes, m.expanded, m.showHidden)
	m.sel = reselect(old, oldSel, m.rows)
	return m.settle()
}

// updateMouse lets the wheel scroll and a single click select. Clicks only
// inspect: a press selects the row under it and nothing more.
func (m Model) updateMouse(msg tea.MouseMsg) Model {
	if m.palette != nil {
		return m
	}
	l := m.layout()
	if l.mode == layoutTiny {
		return m
	}
	inTree := l.showTree && l.tree.contains(msg.X, msg.Y)
	inPreview := l.show && l.preview.contains(msg.X, msg.Y)
	switch {
	case msg.Button == tea.MouseButtonWheelUp || msg.Button == tea.MouseButtonWheelDown:
		d := 3
		if msg.Button == tea.MouseButtonWheelUp {
			d = -3
		}
		switch {
		case inTree:
			m.treeTop += d
			m.treeTop = max(0, min(m.treeTop, max(0, len(m.rows)-m.treeRows())))
			return m
		case inPreview:
			return m.scrollBy(d)
		}
	case msg.Button == tea.MouseButtonLeft && msg.Action == tea.MouseActionPress:
		switch {
		case inTree:
			m.focus = paneTree
			if i := m.treeTop + msg.Y - l.tree.y - 1; msg.Y > l.tree.y && i < len(m.rows) {
				return m.jump(i)
			}
		case inPreview:
			m.focus = panePreview
		}
	}
	return m
}

// RunModel renders a prepared model once for redirected output; interactive
// sessions restore the terminal through Bubble Tea and share the caller's
// cancellation context.
func RunModel(ctx context.Context, model Model, input io.Reader, output io.Writer, interactive bool) error {
	return RunLive(ctx, model, input, output, interactive, nil)
}

// RunLive is RunModel with a feed of snapshots built elsewhere, off the render
// loop. Each Data received replaces the shown tree, keeping selection by key.
// The feed is read until it closes or the program ends; nothing is read from it
// for redirected output, which shows the model once. The selection observer
// hears nothing until the reader's first key press that selects or moves.
func RunLive(ctx context.Context, model Model, input io.Reader, output io.Writer, interactive bool, feed <-chan Data) error {
	if err := ctx.Err(); err != nil {
		return err
	}
	if !interactive {
		_, err := io.WriteString(output, model.View())
		return err
	}
	// The starting selection is not reported: no script runs until the reader's
	// first key press that selects or moves (see WithSelectionObserver).
	p := tea.NewProgram(model, tea.WithContext(ctx), tea.WithInput(input), tea.WithOutput(output),
		tea.WithAltScreen(), tea.WithMouseCellMotion())
	stop := make(chan struct{})
	defer close(stop)
	if feed != nil {
		go func() {
			for {
				select {
				case d, ok := <-feed:
					if !ok {
						return
					}
					p.Send(SnapshotMsg{Data: d})
				case <-stop:
					return
				case <-ctx.Done():
					return
				}
			}
		}()
	}
	_, err := p.Run()
	if ctx.Err() != nil {
		return ctx.Err()
	}
	return err
}
