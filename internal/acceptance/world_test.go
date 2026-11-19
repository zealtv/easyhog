//go:build unix

package acceptance

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"runtime"
	"strings"
	"sync"
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
	pinnedRel    = "../groundhog/testdata/upstream-1c68c9d/groundhog.sh"
	pinnedSHA256 = "298c9d5d11465607796043d44c5e8dc42a20912aa52c00988505aaf8be09dbb9"
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

// tracker starts real runners and records every start, so the test can count
// runner processes per instance and make sure none outlives it.
type tracker struct {
	mu     sync.Mutex
	starts map[string]int // instance directory -> runner starts
	kids   []runner.Child
}

func (l *tracker) Start(ln runner.Launch) (runner.Child, error) {
	c, err := runner.Spawner{}.Start(ln)
	if err != nil {
		return nil, err
	}
	l.mu.Lock()
	if l.starts == nil {
		l.starts = map[string]int{}
	}
	l.starts[filepath.Dir(ln.Executable)]++
	l.kids = append(l.kids, c)
	l.mu.Unlock()
	return c, nil
}

func (l *tracker) startsFor(inst string) int {
	l.mu.Lock()
	defer l.mu.Unlock()
	return l.starts[inst]
}

func (l *tracker) reapAll(t *testing.T) {
	l.mu.Lock()
	kids := append([]runner.Child(nil), l.kids...)
	l.mu.Unlock()
	for _, c := range kids {
		_ = c.Terminate()
		select {
		case <-c.Done():
		case <-time.After(15 * time.Second):
			t.Errorf("runner %d outlived the test", c.PID())
		}
	}
}

// world is one temporary root of synthetic repositories, an Easyhog session
// (supervisor, refresher, model) and a second supervisor standing for another
// owner of some instances.
type world struct {
	t      *testing.T
	script []byte
	tmp    string // short, canonical; holds everything the test writes
	root   string // the directory Easyhog is pointed at
	state  string // Easyhog's session directory base

	easy, other *tracker
	sup, osup   *core.Supervisor
	ref         *core.Refresher

	m             tui.Model
	ctx           context.Context
	stopRefresher func()
	backlog       int // pending deliveries on bravo when it was paused
	// quitWait, when set, replaces the supervisor's 30 s wait for a runner's
	// call at Quit, so a test can have a Quit that gives up waiting.
	quitWait time.Duration
}

func newWorld(t *testing.T, script []byte) *world {
	t.Helper()
	// A short base keeps any unix socket path under the platform limit.
	tmp, err := os.MkdirTemp("", "ehacc")
	if err != nil {
		t.Fatal(err)
	}
	if tmp, err = filepath.EvalSymlinks(tmp); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = os.RemoveAll(tmp) })
	w := &world{t: t, script: script, tmp: tmp, root: filepath.Join(tmp, "repos"), state: filepath.Join(tmp, "state"),
		easy: &tracker{}, other: &tracker{}, ctx: context.Background()}
	for _, d := range []string{w.root, w.state} {
		if err := os.MkdirAll(d, 0o755); err != nil {
			t.Fatal(err)
		}
	}
	return w
}

// start opens the first Easyhog session and the second supervisor, once the
// repositories exist. Intervals are short so the reader's waits are seconds,
// not minutes; nothing else differs from cmd/easyhog.
func (w *world) start() {
	t := w.t
	var err error
	if w.osup, err = core.NewSupervisor(w.supervisorConfig(w.other, "other-state", "another-supervisor")); err != nil {
		t.Fatal(err)
	}
	w.openSession()
	t.Cleanup(func() {
		qctx, qcancel := context.WithTimeout(context.Background(), 90*time.Second)
		defer qcancel()
		w.sup.Quit(qctx)
		w.osup.Quit(qctx)
		w.closeSession()
		w.easy.reapAll(t)
		w.other.reapAll(t)
	})
}

func (w *world) supervisorConfig(l runner.Launcher, dir, label string) core.SupervisorConfig {
	return core.SupervisorConfig{Exec: runner.Process{}, Launcher: l, StateDir: filepath.Join(w.tmp, dir), Label: label,
		RunInterval: time.Second, PollEvery: 100 * time.Millisecond, BusyDelay: 150 * time.Millisecond, StartWait: 15 * time.Second,
		QuitWait: w.quitWait}
}

// openSession is what cmd/easyhog builds at launch: a supervisor that touches
// nothing until Run, a refresher that runs scripts only for the selected
// instance or on Refresh, and the shell reading one through the other. It can
// be called again after Quit to stand for reopening Easyhog.
func (w *world) openSession() {
	t := w.t
	var err error
	clients := core.NewClients(runner.Process{}) // shared, as in cmd/easyhog
	cfg := w.supervisorConfig(w.easy, "easy-state", "easyhog-acceptance")
	cfg.Clients = clients
	if w.sup, err = core.NewSupervisor(cfg); err != nil {
		t.Fatal(err)
	}
	// The interval is far above one `status` call (the pinned script takes
	// well under a second), or the selected instance would read as stale.
	if w.ref, err = core.NewRefresher(core.Config{Root: w.root, FS: discovery.OS{}, Exec: runner.Process{}, Clients: clients,
		IsOurs: w.sup.IsOurs, OnCoherent: w.sup.Observe,
		Interval: 2 * time.Second, SelectSettle: 20 * time.Millisecond, DiscoverEvery: time.Hour}); err != nil {
		t.Fatal(err)
	}
	if err := w.ref.Rescan(w.ctx); err != nil {
		t.Fatal(err)
	}
	w.sup.Adopt(w.inRoot) // cmd/easyhog adopts the tokens of ended sessions before the first frame
	ctx, cancel := context.WithCancel(w.ctx)
	done := make(chan struct{})
	go func() { defer close(done); _ = w.ref.Run(ctx) }()
	w.stopRefresher = func() { cancel(); <-done }
	w.m = tui.NewWithData(w.data(), tui.WithSelectionObserver(w.ref.Select), tui.WithRefresh(w.ref.Refresh),
		tui.WithFolderOpener(func(string) error { return nil }), tui.WithPayloads(readPayload),
		tui.WithRunners(w.sup, w.ref.Reinspect),
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

func (w *world) data() tui.Data { return tui.FromViewWith(w.ref.View(), w.sup.Runner) }

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

// consumer writes runtime/config.json for an ordinary executable. cwd is the
// repository holding .groundhog/.
func (w *world) consumer(inst string, env map[string]string, argv ...string) {
	w.t.Helper()
	var quoted, pairs []string
	for _, a := range argv {
		quoted = append(quoted, fmt.Sprintf("%q", a))
	}
	for k, v := range env {
		pairs = append(pairs, fmt.Sprintf("%q:%q", k, v))
	}
	cfg := fmt.Sprintf(`{"schema_version":1,"consumer":{"argv":[%s],"cwd":"..","env":{%s}}}`+"\n",
		strings.Join(quoted, ","), strings.Join(pairs, ","))
	w.must(os.WriteFile(filepath.Join(inst, "runtime", "config.json"), []byte(cfg), 0o644))
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

// runnerProcesses counts live `run` processes of one instance by its script
// path. A bash subshell (a command substitution inside a tick) shows the same
// command line as its parent for a moment, so a match whose parent also
// matches is not counted again.
func runnerProcesses(t *testing.T, inst string) int {
	t.Helper()
	out, err := exec.Command("ps", "-axo", "pid=,ppid=,command=").Output()
	if err != nil {
		t.Skipf("ps is not available: %v", err)
	}
	needle := filepath.Join(inst, "groundhog.sh") + " run "
	matched := map[string]bool{}
	var parents []string
	for _, line := range strings.Split(string(out), "\n") {
		f := strings.Fields(line)
		if len(f) < 3 || !strings.Contains(line, needle) {
			continue
		}
		matched[f[0]] = true
		parents = append(parents, f[1])
	}
	n := 0
	for _, ppid := range parents {
		if !matched[ppid] {
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
// in the status line.
func (w *world) palette(command string) (quit bool) {
	w.t.Helper()
	w.send(typed(":" + command)...)
	return w.send(press(tea.KeyEnter))
}

// reread asks for a fresh status of one instance and waits for one the script
// observed after the ask, as the reader's Run and Stop do. It makes the view
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
// searched for name ("run" or "stop"), lists that command. It closes the palette
// again. It looks for the command's own row: Open folder keeps its aliases and
// fuzzy matching, so a query like "stop" finds it too (scattered letters of
// "files directory explorer"), and "nothing matches" would never show.
func (w *world) offers(name string) bool {
	w.t.Helper()
	w.send(typed(":" + name)...)
	row := regexp.MustCompile(`│\s+(?:❯\s+)?\S\s+` + regexp.QuoteMeta(strings.ToUpper(name[:1])+name[1:]) + `\s+(?:.*)│`)
	found := row.MatchString(w.screen())
	w.send(press(tea.KeyEsc))
	return found
}

// readPayload is cmd/easyhog's: the item's files as bounded text, nothing run.
func readPayload(ref tui.PayloadRef) string {
	return payload.Preview(payload.OS{}, ref.Instance, ref.Rel)
}

// inRoot is the scope cmd/easyhog gives Adopt: instances beneath the root.
func (w *world) inRoot(instance string) bool { return core.Within(w.root, instance) }
