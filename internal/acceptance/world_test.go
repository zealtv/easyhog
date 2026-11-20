//go:build unix

package acceptance

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"runtime"
	"strings"
	"testing"
	"time"

	tea "github.com/charmbracelet/bubbletea"
	"github.com/charmbracelet/x/ansi"

	"github.com/zealtv/easyhog/internal/core"
	"github.com/zealtv/easyhog/internal/discovery"
	"github.com/zealtv/easyhog/internal/groundhog"
	"github.com/zealtv/easyhog/internal/payload"
	"github.com/zealtv/easyhog/internal/runner"
	"github.com/zealtv/easyhog/internal/tui"
)

const (
	pinnedRel    = "../groundhog/testdata/upstream-9317d4e/groundhog.sh"
	pinnedSHA256 = "5a961d101ffc6d9186b836bd1f140c4c78e0b92770d682858a5a799d0cc562c2"
	overdue      = "once/2020-01-01" // a date long gone: fires at the next enabled tick
	future       = "once/2099-01-01"
)

// gate skips the test unless the reader asked for the acceptance run and the
// host can run it. It returns the pinned script's bytes.
func gate(t *testing.T) []byte {
	t.Helper()
	if os.Getenv("EASYHOG_ACCEPTANCE") != "1" {
		t.Skip("acceptance run not requested: set EASYHOG_ACCEPTANCE=1 (make acceptance); it takes about 40 seconds")
	}
	if _, err := exec.LookPath("bash"); err != nil {
		t.Skip("bash is not installed; the pinned Groundhog script needs it")
	}
	script, err := os.ReadFile(pinnedRel)
	if err != nil {
		t.Fatal(err)
	}
	if sum := sha256.Sum256(script); hex.EncodeToString(sum[:]) != pinnedSHA256 {
		t.Fatalf("%s no longer matches the pinned digest; update the pin deliberately", pinnedRel)
	}
	return script
}

// repoFile returns an absolute path inside this repository (examples, say).
func repoFile(t *testing.T, rel ...string) string {
	t.Helper()
	_, file, _, _ := runtime.Caller(0)
	p := filepath.Join(append([]string{filepath.Dir(file), "..", ".."}, rel...)...)
	abs, err := filepath.Abs(p)
	if err != nil {
		t.Fatal(err)
	}
	return abs
}

// world is one temporary root of synthetic repositories and an Easyhog session
// (ticker, refresher, model).
type world struct {
	t      *testing.T
	script []byte
	tmp    string // short, canonical; holds everything the test writes
	root   string // the directory Easyhog is pointed at

	tk  *core.Ticker
	ref *core.Refresher

	m             tui.Model
	ctx           context.Context
	stopRefresher func()
}

func newWorld(t *testing.T, script []byte) *world {
	t.Helper()
	tmp, err := os.MkdirTemp("", "ehacc")
	if err != nil {
		t.Fatal(err)
	}
	if tmp, err = filepath.EvalSymlinks(tmp); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = os.RemoveAll(tmp) })
	w := &world{t: t, script: script, tmp: tmp, root: filepath.Join(tmp, "repos"), ctx: context.Background()}
	w.must(os.MkdirAll(w.root, 0o755))
	return w
}

// start opens the Easyhog session once the repositories exist.
func (w *world) start() {
	w.openSession()
	w.t.Cleanup(func() {
		w.tk.Quit(context.Background())
		w.closeSession()
	})
}

// openSession is what cmd/easyhog builds at launch: a ticker that ticks only
// what the reader runs, a refresher that runs scripts only for the selected
// instance or on Refresh, and the shell reading one through the other. The
// ticker ticks every second here, not every minute, so the reader's waits are
// seconds; nothing else differs from cmd/easyhog. It can be called again after
// Quit to stand for reopening Easyhog.
func (w *world) openSession() {
	t := w.t
	var err error
	clients := core.NewClients(runner.Process{}) // shared, as in cmd/easyhog
	if w.tk, err = core.NewTicker(core.TickerConfig{Clients: clients, Interval: time.Second}); err != nil {
		t.Fatal(err)
	}
	// The interval is far above one `status` call (the pinned script takes
	// well under a second), or the selected instance would read as stale.
	if w.ref, err = core.NewRefresher(core.Config{Root: w.root, FS: discovery.OS{}, Exec: runner.Process{}, Clients: clients,
		Interval: 2 * time.Second, SelectSettle: 20 * time.Millisecond, DiscoverEvery: time.Hour}); err != nil {
		t.Fatal(err)
	}
	if err := w.ref.Rescan(w.ctx); err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithCancel(w.ctx)
	done := make(chan struct{})
	go func() { defer close(done); _ = w.ref.Run(ctx) }()
	w.stopRefresher = func() { cancel(); <-done }
	w.m = tui.NewWithData(w.data(), tui.WithSelectionObserver(w.ref.Select), tui.WithRefresh(w.ref.Refresh),
		tui.WithFolderOpener(func(string) error { return nil }), tui.WithPayloads(readPayload),
		tui.WithControl(w.tk, w.ref.Reinspect),
		// send runs each command to completion, so a self-re-arming clock tick
		// would never let one finish; the world feeds time through sync instead.
		tui.WithoutClockTicks())
	w.send(tea.WindowSizeMsg{Width: 150, Height: 36})
	// Nothing is selected, and so nothing is read, until the reader's first key
	// press that selects or moves.
}

func (w *world) closeSession() {
	if w.stopRefresher != nil {
		w.stopRefresher()
		w.stopRefresher = nil
	}
}

func (w *world) data() tui.Data { return tui.FromViewWith(w.ref.View(), w.tk.Info) }

// --- the synthetic repositories ------------------------------------------------------------

// instance creates <root>/<rel>/.groundhog holding a copy of the pinned script
// and returns the .groundhog directory.
func (w *world) instance(rel string) string {
	w.t.Helper()
	inst := filepath.Join(w.root, rel, ".groundhog")
	for _, d := range []string{"schedule", "out", "fired", "runtime"} {
		w.must(os.MkdirAll(filepath.Join(inst, d), 0o755))
	}
	w.must(os.WriteFile(filepath.Join(inst, "groundhog.sh"), w.script, 0o755))
	return inst
}

func (w *world) must(err error) {
	w.t.Helper()
	if err != nil {
		w.t.Fatal(err)
	}
}

// hog runs the instance's own script for setup on a temporary instance.
func (w *world) hog(inst string, args ...string) string {
	w.t.Helper()
	cmd := exec.Command(filepath.Join(inst, "groundhog.sh"), args...)
	cmd.Dir = filepath.Dir(inst)
	out, err := cmd.CombinedOutput()
	if err != nil {
		w.t.Fatalf("groundhog.sh %s: %v\n%s", strings.Join(args, " "), err, out)
	}
	return string(out)
}

// item schedules a one-shot or recurring item and gives it a note.txt payload.
func (w *world) item(inst, when, id, note string) {
	w.t.Helper()
	w.hog(inst, "add", when, id)
	w.must(os.WriteFile(filepath.Join(inst, "schedule", when, id, "note.txt"), []byte(note+"\n"), 0o644))
}

// run gives the item an executable run with this text: what firing it does.
func (w *world) run(inst, when, id, text string) {
	w.t.Helper()
	w.must(os.WriteFile(filepath.Join(inst, "schedule", when, id, "run"), []byte(text), 0o755))
}

// example is one of the repository's examples/runs scripts, as text.
func (w *world) example(name string) string {
	w.t.Helper()
	b, err := os.ReadFile(repoFile(w.t, "examples", "runs", name))
	w.must(err)
	return string(b)
}

func (w *world) client(inst string) *groundhog.Client {
	w.t.Helper()
	c, err := groundhog.New(runner.Process{}, inst)
	if err != nil {
		w.t.Fatal(err)
	}
	return c
}

func (w *world) status(inst string) groundhog.Status {
	w.t.Helper()
	st, err := w.client(inst).Status(w.ctx)
	if err != nil {
		w.t.Fatalf("status %s: %v", inst, err)
	}
	return st
}

// deliveries returns the deliveries fired from the schedule item with this id.
func deliveriesOf(st groundhog.Status, id string) []groundhog.Delivery {
	var out []groundhog.Delivery
	for _, d := range st.Deliveries {
		if d.SchedulePath != nil && strings.HasSuffix(*d.SchedulePath, "/"+id) {
			out = append(out, d)
		}
	}
	return out
}

func countState(st groundhog.Status, s groundhog.DeliveryState) int {
	n := 0
	for _, d := range st.Deliveries {
		if d.State == s {
			n++
		}
	}
	return n
}

// --- the reader: keys and the palette through the model ------------------------------------

// cmds runs a command and everything it batches to completion, like a Bubble
// Tea program would, and returns the messages.
func cmds(c tea.Cmd) []tea.Msg {
	if c == nil {
		return nil
	}
	msg := c()
	if b, ok := msg.(tea.BatchMsg); ok {
		var out []tea.Msg
		for _, x := range b {
			out = append(out, cmds(x)...)
		}
		return out
	}
	return []tea.Msg{msg}
}

// send feeds messages to the model and then every message its commands make.
// It reports whether any command asked the program to quit.
func (w *world) send(msgs ...tea.Msg) (quit bool) {
	w.t.Helper()
	for len(msgs) > 0 {
		msg := msgs[0]
		msgs = msgs[1:]
		if msg == nil {
			continue
		}
		if _, ok := msg.(tea.QuitMsg); ok {
			quit = true
			continue
		}
		next, cmd := w.m.Update(msg)
		w.m = next.(tui.Model)
		msgs = append(msgs, cmds(cmd)...)
	}
	return quit
}

// sync gives the model what RunLive's feed would: the refresher's current view.
func (w *world) sync() { w.send(tui.SnapshotMsg{Data: w.data()}) }

func (w *world) screen() string { return ansi.Strip(w.m.View()) }

// flat is the preview pane's text on one line, so a phrase that wraps at the
// pane's edge still matches.
func (w *world) flat() string {
	var parts []string
	for _, line := range strings.Split(w.screen(), "\n") {
		if i := strings.Index(line, "│"); i >= 0 {
			if text := strings.TrimSpace(strings.Trim(line[i+len("│"):], "│ ")); text != "" {
				parts = append(parts, text)
			}
		}
	}
	return strings.Join(parts, " ")
}

func typed(s string) []tea.Msg {
	var out []tea.Msg
	for _, r := range s {
		out = append(out, tea.KeyMsg{Type: tea.KeyRunes, Runes: []rune{r}})
	}
	return out
}

func press(t tea.KeyType) tea.Msg { return tea.KeyMsg{Type: t} }

// until polls cond, feeding the model fresh views, until it holds or time runs out.
func (w *world) until(what string, limit time.Duration, cond func() bool) {
	w.t.Helper()
	deadline := time.Now().Add(limit)
	for {
		w.sync()
		if cond() {
			return
		}
		if time.Now().After(deadline) {
			w.t.Fatalf("timed out after %s waiting for %s\n%s", limit, what, w.screen())
		}
		time.Sleep(50 * time.Millisecond)
	}
}

// node finds an instance's node in the model's data.
func (w *world) node(rel string) tui.Node {
	w.t.Helper()
	for _, n := range w.data().Nodes {
		if n.Label == rel {
			return n
		}
	}
	w.t.Fatalf("no node %q in the tree", rel)
	return tui.Node{}
}

func (w *world) view(rel string) core.InstanceView {
	w.t.Helper()
	iv, ok := w.ref.View().Find(filepath.Join(w.root, rel, ".groundhog"))
	if !ok {
		w.t.Fatalf("no instance %q in the refresher's view", rel)
	}
	return iv
}

// selectRow moves the selection with the arrow keys until the row marked ❯
// shows label, the way a reader would.
func (w *world) selectRow(label string) {
	w.t.Helper()
	w.send(press(tea.KeyHome))
	for i := 0; i < 60; i++ {
		if w.selected() == label {
			return
		}
		w.send(press(tea.KeyDown))
	}
	w.t.Fatalf("could not select %q\n%s", label, w.screen())
}

// selected returns the label on the row marked ❯ in the tree pane. A row that was
// short of room shows its label cut with "…" (the state word and badge come first);
// it is read back as the one instance or item label that starts that way (the
// same label under two instances is still one answer).
func (w *world) selected() string {
	got := w.selectedOnScreen()
	prefix, cut := strings.CutSuffix(got, "…")
	if !cut {
		return got
	}
	match := ""
	var walk func(ns []tui.Node)
	walk = func(ns []tui.Node) {
		for _, n := range ns {
			if strings.HasPrefix(n.Label, prefix) {
				if match != "" && match != n.Label {
					w.t.Fatalf("the cut label %q fits more than one row (%q, %q)", got, match, n.Label)
				}
				match = n.Label
			}
			walk(n.Children)
		}
	}
	walk(w.data().Nodes)
	if match == "" {
		return got
	}
	return match
}

func (w *world) selectedOnScreen() string {
	for _, line := range strings.Split(w.screen(), "\n") {
		tree := strings.SplitN(line, "│", 2)[0]
		i := strings.Index(tree, "❯")
		if i < 0 {
			continue
		}
		for _, f := range strings.Fields(tree[i+len("❯"):]) {
			if len([]rune(f)) > 1 { // glyphs (▸ ○ ◇ …) are one cell each
				return f
			}
		}
	}
	return ""
}

// selectInstance selects an instance by its path relative to the root and waits
// for the read the selection starts (or a read within the last interval, which
// the refresher accepts instead), so the preview shows the instance as it is.
func (w *world) selectInstance(rel string) {
	w.t.Helper()
	began := time.Now()
	w.selectRow(rel)
	w.until("the selection's status read for "+rel, 20*time.Second, func() bool {
		iv := w.view(rel)
		switch {
		case iv.Class == core.ClassMalformed || iv.Class == core.ClassLegacy || iv.Inspection.Err != nil:
			return true // nothing more will be read
		case iv.Inspection.Last == nil:
			return false
		}
		return iv.Inspection.Last.TakenAt.After(began.Add(-w.ref.Config().Interval))
	})
}

// palette types a command into the palette and presses Enter. The result shows
// in the status line. It feeds the model the refresher's current view first, as
// RunLive's feed has done by the time a reader's keys arrive: the palette offers
// what the model last saw, so a read that landed after the last sync (a step
// checks the row through w.node, which reads the refresher) would otherwise
// leave Run unoffered, Enter on "nothing matches" would do nothing, and the
// status line would stay blank.
func (w *world) palette(command string) (quit bool) {
	w.t.Helper()
	w.sync()
	return w.paletteAsSeen(command)
}

// paletteAsSeen is palette on what the model shows now, without feeding it the
// refresher's newer view first: a reader acting before the screen caught up.
func (w *world) paletteAsSeen(command string) (quit bool) {
	w.t.Helper()
	w.send(typed(":" + command)...)
	return w.send(press(tea.KeyEnter))
}

// said reports whether the screen says text: on one line (the status line, a
// row), or in the preview pane, where a long phrase may wrap (an action's whole
// outcome stays there as "Last action: …").
func (w *world) said(text string) bool {
	w.t.Helper()
	for _, line := range strings.Split(w.screen(), "\n") {
		if strings.Contains(strings.Join(strings.Fields(line), " "), text) {
			return true
		}
	}
	return strings.Contains(w.flat(), text)
}

// reread asks for a fresh status of one instance and waits for one the script
// observed after the ask, as the reader's actions do. It makes the view
// catch up with the protocol without waiting for the interval. A read that was
// already under way when reread was called can finish after it and still show
// the state from before, so such a read doesn't count: reread asks again. The
// script's observed_at has whole-second precision, so that can take a second.
func (w *world) reread(rel string) {
	w.t.Helper()
	key := filepath.Join(w.root, rel, ".groundhog")
	asked := time.Now()
	var seen time.Time
	if l := w.view(rel).Inspection.Last; l != nil {
		seen = l.TakenAt
	}
	w.ref.Reinspect(key)
	w.until("a fresh read of "+rel, 20*time.Second, func() bool {
		l := w.view(rel).Inspection.Last
		if l == nil || !l.TakenAt.After(seen) {
			return false
		}
		if l.Status.ObservedAt.IsZero() || l.Status.ObservedAt.After(asked) {
			return true
		}
		seen = l.TakenAt
		w.ref.Reinspect(key)
		return false
	})
}

// await polls a condition on the protocol's own status (not the view).
func (w *world) await(what string, limit time.Duration, cond func() bool) {
	w.t.Helper()
	deadline := time.Now().Add(limit)
	for !cond() {
		if time.Now().After(deadline) {
			w.t.Fatalf("timed out after %s waiting for %s", limit, what)
		}
		time.Sleep(100 * time.Millisecond)
	}
}

// offers reports whether the palette, opened on the current selection and
// searched for name ("run", "stop", "retry"), lists that command. It closes the palette
// again. It looks for the command's own row: Open folder keeps its aliases and
// fuzzy matching, so a query like "stop" finds it too (scattered letters of
// "files directory explorer"), and "nothing matches" would never show.
func (w *world) offers(name string) bool {
	w.t.Helper()
	w.sync() // as palette does
	w.send(typed(":" + name)...)
	row := regexp.MustCompile(`│\s+(?:❯\s+)?\S\s+` + regexp.QuoteMeta(strings.ToUpper(name[:1])+name[1:]) + `\s+(?:.*)│`)
	found := row.MatchString(w.screen())
	w.send(press(tea.KeyEsc))
	return found
}

// readPayload is cmd/easyhog's: the item's files and its newest runs' logs as
// bounded text, nothing run.
func readPayload(ref tui.PayloadRef) string {
	return payload.ItemPreview(payload.OS{}, ref.Instance, ref.Rel, ref.Logs)
}
