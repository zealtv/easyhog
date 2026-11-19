package tui

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"sync"
	"testing"
	"time"

	tea "github.com/charmbracelet/bubbletea"
	"github.com/charmbracelet/x/ansi"

	"github.com/zealtv/easyhog/internal/core"
	"github.com/zealtv/easyhog/internal/groundhog"
	"github.com/zealtv/easyhog/internal/runner"
)

// Synthetic instances only: nothing here runs a script.

func ours(name string, runnerState groundhog.RunnerState, paused bool) core.InstanceView {
	st := okStatus()
	st.Owner = &groundhog.Owner{ID: "easyhog-1", Label: "easyhog"}
	st.Runner.State, st.Paused = runnerState, paused
	return iv(name, core.ClassOwnedBySession, core.HealthOK, core.Inspection{Last: obs(st)})
}

func unowned(name string) core.InstanceView {
	return iv(name, core.ClassUnowned, core.HealthOK, core.Inspection{Last: obs(okStatus())})
}

func lookup(infos map[string]core.RunnerInfo) RunnerLookup {
	return func(key string) (core.RunnerInfo, bool) { i, ok := infos[key]; return i, ok }
}

func actionsOf(t *testing.T, v core.InstanceView, infos map[string]core.RunnerInfo) string {
	t.Helper()
	d := FromViewWith(core.View{Root: "/r", Instances: []core.InstanceView{v}}, lookup(infos))
	var out []string
	for _, a := range d.Nodes[0].Actions {
		if a == ActionOpenFolder {
			continue
		}
		out = append(out, string(a))
	}
	return strings.Join(out, ",")
}

// Run and Stop appear only where they can work: the offered controls follow
// ownership and protocol status, and nothing misleading is offered otherwise.
func TestOfferedControlsPerClass(t *testing.T) {
	good := obs(okStatus())
	foreign := okStatus()
	foreign.Owner = &groundhog.Owner{ID: "o-1", Label: "team-supervisor"}
	foreign.Runner.State = groundhog.RunnerRunning
	foreignStopped := foreign
	foreignStopped.Runner.State = groundhog.RunnerStopped
	incoherent := okStatus()
	incoherent.Consistent = false
	malformed := iv("m", core.ClassMalformed, core.HealthOK, core.Inspection{})
	malformed.Instance.Malformed = []string{"groundhog.sh is missing"}
	failingNeverRead := iv("f", core.ClassFailing, core.HealthOK, core.Inspection{Err: errors.New("timed out")})
	failingWithLast := unowned("fl")
	failingWithLast.Inspection.Err = errors.New("timed out")
	stale := unowned("s")
	stale.Health = core.HealthStale
	ourStale := ours("os", groundhog.RunnerRunning, false)
	ourStale.Health = core.HealthStale

	cases := []struct {
		name string
		v    core.InstanceView
		want string
	}{
		{"unowned and controllable", unowned("a"), "run"},
		{"ours, running", ours("b", groundhog.RunnerRunning, false), "stop"},
		{"ours, running but paused", ours("c", groundhog.RunnerRunning, true), "run,stop"},
		{"ours, runner stopped", ours("d", groundhog.RunnerStopped, false), "run,stop"},
		{"ours, stopped and paused", ours("e", groundhog.RunnerStopped, true), "run,stop"},
		{"ours, runner failed", ours("f", groundhog.RunnerFailed, false), "run,stop"},
		{"ours, runner stopping", ours("g", groundhog.RunnerStopping, false), ""},
		{"ours, runner state unknown", ours("h", groundhog.RunnerUnknown, false), ""},
		{"external, runner alive", iv("i", core.ClassExternal, core.HealthOK, core.Inspection{Last: obs(foreign)}), ""},
		{"external, runner stopped", iv("j", core.ClassExternal, core.HealthOK, core.Inspection{Last: obs(foreignStopped)}), ""},
		{"unsupported", iv("k", core.ClassUnsupported, core.HealthOK, core.Inspection{Last: good}), ""},
		{"legacy", iv("l", core.ClassLegacy, core.HealthOK, core.Inspection{Support: &groundhog.Support{Reason: "old"}}), ""},
		{"malformed", malformed, ""},
		{"not inspected yet", iv("n", core.ClassNotInspected, core.HealthOK, core.Inspection{}), ""},
		{"failing, never read", failingNeverRead, ""},
		{"failing, with an old status", failingWithLast, ""},
		{"inconsistent", iv("o", core.ClassUnowned, core.HealthInconsistent, core.Inspection{Last: obs(incoherent)}), ""},
		{"stale", stale, ""},
		{"ours but stale", ourStale, ""},
	}
	for _, c := range cases {
		if got := actionsOf(t, c.v, nil); got != c.want {
			t.Errorf("%s: offers %q, want %q", c.name, got, c.want)
		}
	}

	// Without a supervisor nothing is offered, whatever the class.
	d := FromViewWith(core.View{Instances: []core.InstanceView{unowned("a")}}, nil)
	if got := d.Nodes[0].Actions; len(got) != 1 || got[0] != ActionOpenFolder {
		t.Errorf("no supervisor: %v", got)
	}
	d = FromView(core.View{Instances: []core.InstanceView{unowned("a")}})
	if got := d.Nodes[0].Actions; len(got) != 1 || got[0] != ActionOpenFolder {
		t.Errorf("FromView: %v", got)
	}
}

// An adopted instance reads as this session's own and says where it came from:
// its runner, if it still runs, belongs to the session that ended.
func TestAdoptedInstanceSaysWhereItCameFrom(t *testing.T) {
	v := ours("x", groundhog.RunnerRunning, false)
	key := v.Instance.Path
	ended := time.Date(2026, 11, 17, 14, 3, 0, 0, time.Local)
	info := core.RunnerInfo{Instance: key, Phase: core.PhaseEnrolled, Adopted: true, AdoptedFrom: ended}
	if got := actionsOf(t, v, map[string]core.RunnerInfo{key: info}); got != "stop" {
		t.Fatalf("an adopted, running instance offers %q, want stop", got)
	}
	d := FromViewWith(core.View{Instances: []core.InstanceView{v}}, lookup(map[string]core.RunnerInfo{key: info}))
	if !strings.Contains(d.Nodes[0].Preview, "Adopted from a session that ended at 14:03") {
		t.Errorf("preview lacks the adoption note:\n%s", d.Nodes[0].Preview)
	}
	info.Adopted = false // a runner of this session's own has been started since
	d = FromViewWith(core.View{Instances: []core.InstanceView{v}}, lookup(map[string]core.RunnerInfo{key: info}))
	if strings.Contains(d.Nodes[0].Preview, "Adopted") {
		t.Errorf("preview still says adopted:\n%s", d.Nodes[0].Preview)
	}
}

// A runner that ended on its own is shown failed, with the notice (and its log
// path) in the preview, and Run offers a start again.
func TestRunnerThatEndedOnItsOwnShowsItsNoticeAndOffersRun(t *testing.T) {
	v := ours("x", groundhog.RunnerStopped, false)
	key := v.Instance.Path
	info := core.RunnerInfo{Instance: key, Phase: core.PhaseExited, Notice: "runner ended on its own (exit status 1) and was not restarted; log /state/i1.log"}
	d := FromViewWith(core.View{Instances: []core.InstanceView{v}}, lookup(map[string]core.RunnerInfo{key: info}))
	n := d.Nodes[0]
	if n.State != StateFailed || !strings.Contains(n.Detail, "runner ended on its own") ||
		!strings.Contains(n.Preview, "log /state/i1.log") || !hasAction(n, ActionRun) {
		t.Fatalf("%+v", n)
	}
}

func hasAction(n Node, a Action) bool {
	for _, x := range n.Actions {
		if x == a {
			return true
		}
	}
	return false
}

// The preview says why Run and Stop are missing, and how a not-yet-inspected
// instance becomes runnable.
func TestPreviewSaysWhyNoControlIsOffered(t *testing.T) {
	cases := map[string]struct {
		v    core.InstanceView
		want string
	}{
		"fresh":    {iv("n", core.ClassNotInspected, core.HealthOK, core.Inspection{}), "not until its first status read shows it unowned and controllable (select it, or choose Refresh)"},
		"external": {iv("e", core.ClassExternal, core.HealthOK, core.Inspection{Last: obs(okStatus())}), "another supervisor owns it, running or not"},
		"legacy":   {iv("l", core.ClassLegacy, core.HealthOK, core.Inspection{}), "the script lacks the runtime commands"},
		"unowned":  {unowned("u"), "Palette offers: Run"},
		"running":  {ours("r", groundhog.RunnerRunning, false), "Palette offers: Stop"},
	}
	for name, c := range cases {
		d := FromViewWith(core.View{Instances: []core.InstanceView{c.v}}, lookup(nil))
		if !strings.Contains(d.Nodes[0].Preview, c.want) {
			t.Errorf("%s: preview lacks %q:\n%s", name, c.want, d.Nodes[0].Preview)
		}
	}
}

// fakeRunners stands in for the supervisor; the tests that use it assert on the
// calls and on what the terminal then shows.
type fakeRunners struct {
	mu      sync.Mutex
	runs    []string
	stops   []string
	release chan struct{} // when set, a call waits for it
	run     func(key string) (core.RunResult, error)
	stop    func(key string) (core.StopResult, error)
	settles []settleCall
	settle  func(key string, req core.SettleRequest) (core.SettleResult, error)
	toggles []toggleCall
	toggle  func(key, item string, pause bool) (core.ItemResult, error)
}

type toggleCall struct {
	key, item string
	pause     bool
}

func (f *fakeRunners) SetItemPaused(ctx context.Context, key, item string, pause bool) (core.ItemResult, error) {
	f.mu.Lock()
	f.toggles = append(f.toggles, toggleCall{key, item, pause})
	f.mu.Unlock()
	if f.release != nil {
		<-f.release
	}
	if f.toggle != nil {
		return f.toggle(key, item, pause)
	}
	return core.ItemResult{Instance: key, Item: core.ItemIdentity(item), From: item, To: core.ItemTarget(item, pause), Paused: pause, Locked: true}, nil
}

type settleCall struct {
	key string
	req core.SettleRequest
}

func (f *fakeRunners) Settle(ctx context.Context, key string, req core.SettleRequest) (core.SettleResult, error) {
	f.mu.Lock()
	f.settles = append(f.settles, settleCall{key, req})
	f.mu.Unlock()
	if f.release != nil {
		<-f.release
	}
	if f.settle != nil {
		return f.settle(key, req)
	}
	return core.SettleResult{Instance: key, Action: req.Action, Settled: []string{"d-1"}, StoppedRunner: true, Restarted: true, PID: 4343, Seen: true}, nil
}

func (f *fakeRunners) Run(ctx context.Context, key string) (core.RunResult, error) {
	f.mu.Lock()
	f.runs = append(f.runs, key)
	f.mu.Unlock()
	if f.release != nil {
		<-f.release
	}
	if f.run != nil {
		return f.run(key)
	}
	return core.RunResult{Instance: key, Outcome: core.RunStarted, Enrolled: true, PID: 4242, Seen: true}, nil
}

func (f *fakeRunners) Stop(ctx context.Context, key string) (core.StopResult, error) {
	f.mu.Lock()
	f.stops = append(f.stops, key)
	f.mu.Unlock()
	if f.release != nil {
		<-f.release
	}
	if f.stop != nil {
		return f.stop(key)
	}
	return core.StopResult{Instance: key}, nil
}

func (f *fakeRunners) counts() (int, int) {
	f.mu.Lock()
	defer f.mu.Unlock()
	return len(f.runs), len(f.stops)
}

type rig struct {
	t      *testing.T
	m      Model
	f      *fakeRunners
	reread []string
	mu     sync.Mutex
	infos  map[string]core.RunnerInfo
}

// newRig shows alpha (unowned: Run), bravo (ours, running: Pause) and charlie
// (external: neither), at 100x30.
func newRig(t *testing.T, f *fakeRunners) *rig {
	t.Helper()
	r := &rig{t: t, f: f}
	r.m = NewWithData(r.data(unowned("alpha"), ours("bravo", groundhog.RunnerRunning, false),
		iv("charlie", core.ClassExternal, core.HealthOK, core.Inspection{Last: obs(okStatus())})),
		WithRunners(f, func(k string) { r.mu.Lock(); r.reread = append(r.reread, k); r.mu.Unlock() }))
	next, _ := r.m.Update(tea.WindowSizeMsg{Width: 100, Height: 30})
	r.m = next.(Model)
	return r
}

func (r *rig) data(vs ...core.InstanceView) Data {
	return FromViewWith(core.View{Root: "/r", Instances: vs}, lookup(r.infos))
}

func (r *rig) send(msgs ...tea.Msg) []tea.Msg {
	r.t.Helper()
	var out []tea.Msg
	for _, msg := range msgs {
		next, cmd := r.m.Update(msg)
		r.m = next.(Model)
		out = append(out, run(cmd)...)
	}
	return out
}

func (r *rig) screen() string { return ansi.Strip(r.m.View()) }

func (r *rig) names() string {
	var out []string
	for _, c := range r.m.commands() {
		out = append(out, c.name)
	}
	return strings.Join(out, ",")
}

// The palette's prompt line names the instance it is bound to, by relative
// path, and keeps naming it however the selection moves afterwards.
func TestPalettePromptNamesTheBoundInstance(t *testing.T) {
	r := newRig(t, &fakeRunners{})
	r.send(typeText(":")...)
	if !strings.Contains(r.screen(), "on alpha") {
		t.Fatalf("prompt does not name the instance:\n%s", r.screen())
	}
	line := ""
	for _, l := range strings.Split(r.screen(), "\n") {
		if strings.Contains(l, "type a command") {
			line = l
		}
	}
	if !strings.Contains(line, "on alpha") {
		t.Fatalf("the name is not on the prompt line: %q", line)
	}
	// Moving the selection behind the open palette does not rebind it.
	r.send(key(tea.KeyEsc), key(tea.KeyDown), typeText(":")[0])
	if !strings.Contains(r.screen(), "on bravo") || strings.Contains(r.screen(), "on alpha") {
		t.Fatalf("prompt after moving:\n%s", r.screen())
	}
	// Nothing selected: no name.
	empty := NewWithData(Data{Root: "/r"})
	empty, _ = feed(empty, tea.WindowSizeMsg{Width: 100, Height: 30}, typeText(":")[0])
	if strings.Contains(ansi.Strip(empty.View()), " on ") {
		t.Errorf("an empty list names an instance:\n%s", ansi.Strip(empty.View()))
	}
}

func TestPalettePromptKeepsTheEndOfALongPathAndTheQuery(t *testing.T) {
	long := strings.Repeat("deep/", 12) + "project/.groundhog"
	v := unowned("x")
	v.Instance.Display = long
	m := NewWithData(FromViewWith(core.View{Instances: []core.InstanceView{v}}, lookup(nil)), WithRunners(&fakeRunners{}, nil))
	m, _ = feed(m, tea.WindowSizeMsg{Width: 60, Height: 24}, typeText(":pla")[0], typeText("pla")[0], typeText("pla")[1], typeText("pla")[2])
	out := ansi.Strip(m.View())
	if !strings.Contains(out, "project") || !strings.Contains(out, "pla▌") {
		t.Fatalf("long path or query lost:\n%s", out)
	}
	for _, l := range strings.Split(out, "\n") {
		if w := len([]rune(l)); w > 60 {
			t.Fatalf("line wider than the terminal (%d): %q", w, l)
		}
	}
}

// Hostile directory names stay inert in the prompt.
func TestPalettePromptNameIsInert(t *testing.T) {
	v := unowned("x")
	v.Instance.Display = "evil\x1b[2J\x07dir/.groundhog"
	m := NewWithData(FromViewWith(core.View{Instances: []core.InstanceView{v}}, lookup(nil)))
	m, _ = feed(m, tea.WindowSizeMsg{Width: 100, Height: 30}, typeText(":")[0])
	if out := m.View(); strings.Contains(out, "\x1b[2J") || strings.Contains(out, "\x07") {
		t.Fatal("control sequence reached the frame")
	}
}

// Run goes to the supervisor off the update loop, shows "Running …" while it
// waits, offers neither Run nor Pause again for that instance (the palette does
// not queue a second one), and then shows the exact outcome.
func TestRunShowsTransitionThenOutcomeAndDoesNotQueueASecond(t *testing.T) {
	f := &fakeRunners{release: make(chan struct{})}
	r := newRig(t, f)
	if r.names() != "Run,Open folder,Refresh,Quit" {
		t.Fatalf("alpha offers %s", r.names())
	}
	r.send(typeText(":run")...)
	next, cmd := r.m.Update(key(tea.KeyEnter))
	r.m = next.(Model)
	if cmd == nil {
		t.Fatal("Run scheduled nothing")
	}
	if n, _ := f.counts(); n != 0 {
		t.Fatal("Run ran on the update loop")
	}
	if !strings.Contains(r.screen(), "Running alpha…") {
		t.Fatalf("no transitional state:\n%s", r.screen())
	}
	if r.names() != "Open folder,Refresh,Quit" {
		t.Fatalf("while Run runs the palette offers %s", r.names())
	}

	// A second invocation while the first is under way finds nothing to run:
	// the palette does not offer Run, so Enter has nothing chosen.
	r.send(typeText(":run")...)
	if got := r.m.filtered(); len(got) != 0 {
		t.Fatalf("the palette still finds %d commands for \"run\"", len(got))
	}
	if msgs := r.send(key(tea.KeyEnter)); len(msgs) != 0 {
		t.Fatalf("a second Run scheduled %v", msgs)
	}
	r.send(key(tea.KeyEsc))

	done := make(chan tea.Msg, 1)
	go func() { done <- cmd() }()
	close(f.release)
	msg := <-done
	r.send(msg)
	if p, _ := f.counts(); p != 1 {
		t.Fatalf("Run reached the supervisor %d times", p)
	}
	want := "Run: alpha: enrolled for this session, runner started (pid 4242), status shows it running"
	if r.m.notice != want || r.m.failure {
		t.Fatalf("notice %q (failure %v)", r.m.notice, r.m.failure)
	}
	if len(r.reread) != 1 || r.reread[0] != "/r/alpha/.groundhog" {
		t.Fatalf("reread %v", r.reread)
	}
	if len(r.m.busy) != 0 || r.names() != "Run,Open folder,Refresh,Quit" {
		t.Fatalf("not offered again after the answer: busy %v, %s", r.m.busy, r.names())
	}
}

// After Run, the refresher's next view (running, ours) shows the instance
// running with Pause offered instead of Run.
func TestRunThenRefreshShowsRunning(t *testing.T) {
	r := newRig(t, &fakeRunners{})
	r.send(typeText(":run")...)
	msgs := r.send(key(tea.KeyEnter))
	r.send(msgs...)
	if p, _ := r.f.counts(); p != 1 {
		t.Fatalf("runs: %d", p)
	}
	if n := r.m.rows[0].node; n.State != StateStopped {
		t.Fatalf("before the re-read the row is %v", n.State)
	}
	// The re-read arrives: alpha is now ours and its runner runs.
	r.send(SnapshotMsg{Data: r.data(ours("alpha", groundhog.RunnerRunning, false), ours("bravo", groundhog.RunnerRunning, false),
		iv("charlie", core.ClassExternal, core.HealthOK, core.Inspection{Last: obs(okStatus())}))})
	if n := r.m.rows[0].node; n.State != StateRunning || r.m.selectedInstance() != "/r/alpha/.groundhog" {
		t.Fatalf("row %v, selected %q", n.State, r.m.selectedInstance())
	}
	if r.names() != "Stop,Open folder,Refresh,Quit" {
		t.Fatalf("after Run the palette offers %s", r.names())
	}
}

func TestStopShowsTransitionThenOutcome(t *testing.T) {
	f := &fakeRunners{release: make(chan struct{})}
	r := newRig(t, f)
	r.send(key(tea.KeyDown)) // bravo: ours, running
	r.send(typeText(":stop")...)
	next, cmd := r.m.Update(key(tea.KeyEnter))
	r.m = next.(Model)
	if !strings.Contains(r.screen(), "Stopping bravo…") {
		t.Fatalf("no transitional state:\n%s", r.screen())
	}
	close(f.release)
	r.send(cmd())
	want := "Stop: bravo: stopped and released. The runner has ended and nothing owns the instance now, so another supervisor may take it, and Run enrolls it for this session again; deliveries already pending stay pending"
	if r.m.notice != want || r.m.failure {
		t.Fatalf("notice %q", r.m.notice)
	}
	if _, n := f.counts(); n != 1 {
		t.Fatalf("stops %d", n)
	}
}

// The words for each outcome, exactly. A refusal names the instance by its
// relative path and gives the supervisor's reason and detail as they are.
func TestOutcomeTextIsExact(t *testing.T) {
	const l = "alpha/.groundhog"
	cases := []struct {
		name string
		err  error
		want string
	}{
		{"external owner", &core.RefusedError{Op: "run", Instance: "/r/alpha/.groundhog", Reason: core.RefusedExternal,
			Detail: `owner "team-supervisor" is recorded; ownership and runner liveness are separate, and a stopped owner is not available for takeover`},
			`alpha/.groundhog: refused: owned by another supervisor: owner "team-supervisor" is recorded; ownership and runner liveness are separate, and a stopped owner is not available for takeover`},
		{"unsupported", &core.RefusedError{Op: "run", Reason: core.RefusedUnsupported, Detail: "missing consumer-v1"},
			"alpha/.groundhog: refused: does not support supervised control: missing consumer-v1"},
		{"limit", &core.RefusedError{Op: "run", Reason: core.RefusedLimit, Detail: "at most 16 runners"},
			"alpha/.groundhog: refused: too many runners: at most 16 runners"},
		{"not ours", &core.RefusedError{Op: "stop", Reason: core.RefusedNotOurs, Detail: "this session holds no token for it, so there is nothing to stop"},
			"alpha/.groundhog: refused: no token of this session's controls it: this session holds no token for it, so there is nothing to stop"},
		{"stop left it held", &core.StopError{Instance: "/r/alpha/.groundhog", Why: "stop had not finished after 30s; the runner (pid 7) did not end after stop and SIGTERM and was left running"},
			"alpha/.groundhog: not released: stop had not finished after 30s; the runner (pid 7) did not end after stop and SIGTERM and was left running. This session still holds the instance, so Stop can be tried again"},
		{"quitting", &core.RefusedError{Op: "run", Reason: core.RefusedClosing}, "alpha/.groundhog: refused: quitting"},
		{"busy after retries", &groundhog.ExitError{Command: "stop", Code: 4, Kind: groundhog.KindBusy},
			"alpha/.groundhog: still busy after retries (another operation holds the instance's lock); try again shortly"},
		{"runner ended at startup", &core.RunnerExitedError{Instance: "/r/alpha/.groundhog", Exit: runner.Exit{Code: 2}, Log: "/state/sessions/1-x/i1.log"},
			"alpha/.groundhog: the runner ended at startup (exit status 2) and was not started again; log: /state/sessions/1-x/i1.log"},
		{"uncertain enroll", &groundhog.UncertainEnrollError{},
			"alpha/.groundhog: the enroll ended without a certain outcome; the token is kept and nothing was started. The next Run, or the next status read, settles it"},
		{"other failure", errors.New("start runner: fork/exec: denied"), "alpha/.groundhog: failed: start runner: fork/exec: denied"},
		{"wrapped refusal", fmt.Errorf("outer: %w", &core.RefusedError{Op: "run", Reason: core.RefusedInconsistent, Detail: "refresh and try again"}),
			"alpha/.groundhog: refused: status is not coherent: refresh and try again"},
	}
	for _, c := range cases {
		err := ActionFailure(l, c.err)
		if err == nil || err.Error() != c.want {
			t.Errorf("%s:\n got  %v\n want %s", c.name, err, c.want)
			continue
		}
		if !errors.Is(err, c.err) {
			t.Errorf("%s: the typed cause is lost", c.name)
		}
	}
	if ActionFailure(l, nil) != nil {
		t.Error("nil error gave a failure")
	}

	texts := map[string]string{
		RunText(l, core.RunResult{Outcome: core.RunStarted, PID: 7}):                                 "alpha/.groundhog: runner started (pid 7), status has not shown it running yet; checking again",
		RunText(l, core.RunResult{Outcome: core.RunStarted, PID: 7, Seen: true, ClearedPause: true}): "alpha/.groundhog: runner started (pid 7), status shows it running; a pause was cleared",
		RunText(l, core.RunResult{Outcome: core.RunResumed, ClearedPause: true}):                     "alpha/.groundhog: resumed; the runner was already running; a pause was cleared",
		RunText(l, core.RunResult{Outcome: core.RunAlreadyRunning}):                                  "alpha/.groundhog: already running; nothing changed",
		StopText(l, core.StopResult{}):                                                               "alpha/.groundhog: stopped and released. The runner has ended and nothing owns the instance now, so another supervisor may take it, and Run enrolls it for this session again; deliveries already pending stay pending",
		StopText(l, core.StopResult{Terminated: true, Why: "stop had not finished after 30s"}):       "alpha/.groundhog: stopped and released. The runner has ended and nothing owns the instance now, so another supervisor may take it, and Run enrolls it for this session again; deliveries already pending stay pending. It needed SIGTERM (stop had not finished after 30s): a consumer call the runner was waiting on, if any, stays uncertain until it is recovered",
		StopText(l, core.StopResult{Why: "stop failed: exit status 6"}):                              "alpha/.groundhog: stopped and released. The runner has ended and nothing owns the instance now, so another supervisor may take it, and Run enrolls it for this session again; deliveries already pending stay pending. Stop reported a problem on the way (stop failed: exit status 6)",
	}
	for got, want := range texts {
		if got != want {
			t.Errorf("\n got  %s\n want %s", got, want)
		}
	}
}

// A refusal arrives as a failure notice in the status line, and the reread still
// happens so the view reconciles with what the protocol says.
func TestRefusalShowsAsAFailureAndRereads(t *testing.T) {
	f := &fakeRunners{run: func(key string) (core.RunResult, error) {
		return core.RunResult{}, &core.RefusedError{Op: "run", Instance: key, Reason: core.RefusedExternal, Detail: `owner "x" is recorded`}
	}}
	r := newRig(t, f)
	r.send(typeText(":run")...)
	msgs := r.send(key(tea.KeyEnter))
	r.send(msgs...)
	want := `Run: alpha: refused: owned by another supervisor: owner "x" is recorded`
	if r.m.notice != want || !r.m.failure {
		t.Fatalf("notice %q failure %v", r.m.notice, r.m.failure)
	}
	if !strings.Contains(r.screen(), "! "+want[:60]) {
		t.Fatalf("not in the status line:\n%s", r.screen())
	}
	if len(r.reread) != 1 {
		t.Fatalf("reread %v", r.reread)
	}
	if len(r.m.busy) != 0 {
		t.Fatal("still busy after a refusal")
	}
}

// An enroll that ended without a certain outcome shows what happened and what
// settles it.
func TestUncertainEnrollIsShown(t *testing.T) {
	f := &fakeRunners{run: func(key string) (core.RunResult, error) {
		return core.RunResult{Instance: key}, &groundhog.UncertainEnrollError{}
	}}
	r := newRig(t, f)
	r.send(typeText(":run")...)
	r.send(r.send(key(tea.KeyEnter))...)
	want := "Run: alpha: the enroll ended without a certain outcome; the token is kept and nothing was started. The next Run, or the next status read, settles it"
	if r.m.notice != want || !r.m.failure {
		t.Fatalf("notice %q failure %v", r.m.notice, r.m.failure)
	}
}

// The selection holds while a Run is under way and across refreshes, and a
// result for an instance that is no longer selected does not move it.
func TestSelectionHoldsDuringARefreshAndAnActionInFlight(t *testing.T) {
	f := &fakeRunners{release: make(chan struct{})}
	r := newRig(t, f)
	r.send(typeText(":run")...)
	next, cmd := r.m.Update(key(tea.KeyEnter))
	r.m = next.(Model)
	alpha, bravo := "/r/alpha/.groundhog", "/r/bravo/.groundhog"

	r.send(key(tea.KeyDown)) // bravo, while alpha's Run is under way
	if r.m.selectedInstance() != bravo || r.names() != "Stop,Open folder,Refresh,Quit" {
		t.Fatalf("selected %q offers %s", r.m.selectedInstance(), r.names())
	}
	// A refresh reorders and adds instances; bravo stays selected, alpha stays busy.
	r.send(SnapshotMsg{Data: r.data(unowned("aaa"), unowned("alpha"), ours("bravo", groundhog.RunnerRunning, false))})
	if r.m.selectedInstance() != bravo {
		t.Fatalf("selection moved to %q", r.m.selectedInstance())
	}
	if _, busy := r.m.busy[alpha]; !busy || !strings.Contains(r.screen(), "Running alpha…") {
		t.Fatalf("alpha's Run is not shown as under way:\n%s", r.screen())
	}
	// Bravo can still be paused meanwhile: only the instance under way is held.
	if r.names() != "Stop,Open folder,Refresh,Quit" {
		t.Fatalf("bravo offers %s", r.names())
	}
	close(f.release)
	r.send(cmd())
	if r.m.selectedInstance() != bravo || len(r.m.busy) != 0 {
		t.Fatalf("selection %q busy %v after the result", r.m.selectedInstance(), r.m.busy)
	}
}

// A palette opened on one instance runs Run on it even if a refresh makes the
// selection land elsewhere; that rule (palette-target-binding) still holds with
// the real handler, and an action in flight does not rebind it.
func TestBoundInstanceSurvivesWhileSomethingIsInFlight(t *testing.T) {
	f := &fakeRunners{}
	r := newRig(t, f)
	r.send(typeText(":pl")...)
	// alpha vanishes behind the open palette; bravo (running) takes the selection.
	r.send(SnapshotMsg{Data: r.data(ours("bravo", groundhog.RunnerRunning, false))})
	r.send(key(tea.KeyEnter))
	if p, n := f.counts(); p != 0 || n != 0 {
		t.Fatalf("an action ran on the wrong instance: runs %d stops %d", p, n)
	}
	if !strings.Contains(r.m.notice, "Nothing ran:") {
		t.Fatalf("notice %q", r.m.notice)
	}
}

// Quit never goes through the runners handler: the shell quits, and main ends
// in core.Supervisor.Quit for every way out.
func TestQuitDoesNotTouchTheRunners(t *testing.T) {
	f := &fakeRunners{}
	r := newRig(t, f)
	r.send(typeText(":quit")...)
	_, cmd := r.m.Update(key(tea.KeyEnter))
	if !quits(cmd) {
		t.Fatal("palette Quit did not quit")
	}
	_, cmd = r.m.Update(key(tea.KeyCtrlC))
	if !quits(cmd) {
		t.Fatal("ctrl+c did not quit")
	}
	if p, n := f.counts(); p != 0 || n != 0 {
		t.Fatalf("runs %d stops %d", p, n)
	}
}

// Bare keys still start nothing with the real handler installed.
func TestBareKeysNeverReachTheRunners(t *testing.T) {
	f := &fakeRunners{}
	r := newRig(t, f)
	for _, k := range []tea.Msg{key(tea.KeyEnter), key(tea.KeySpace), key(tea.KeyRight), tea.KeyMsg{Type: tea.KeyRunes, Runes: []rune("p")},
		tea.KeyMsg{Type: tea.KeyRunes, Runes: []rune("y")}, key(tea.KeyTab)} {
		if msgs := r.send(k); len(msgs) != 0 {
			t.Fatalf("key %v produced %v", k, msgs)
		}
	}
	if p, n := f.counts(); p != 0 || n != 0 {
		t.Fatalf("runs %d stops %d", p, n)
	}
}

// The status line holds one line and cuts a long reason; the whole outcome stays
// in the preview of the instance it was about, until that instance is acted on again.
func TestLongRefusalStaysWholeInThePreview(t *testing.T) {
	detail := `owner "other" is recorded; ownership and runner liveness are separate, and a stopped owner is not available for takeover`
	f := &fakeRunners{run: func(key string) (core.RunResult, error) {
		return core.RunResult{}, &core.RefusedError{Op: "run", Instance: key, Reason: core.RefusedExternal, Detail: detail}
	}}
	r := newRig(t, f)
	r.m, _ = feed(r.m, tea.WindowSizeMsg{Width: 80, Height: 24})
	r.send(typeText(":run")...)
	r.send(r.send(key(tea.KeyEnter))...)
	whole := func() string {
		return strings.Join(strings.Fields(ansi.Strip(strings.Join(r.m.previewLines(40), "\n"))), " ")
	}
	if want := "Last action: Run: alpha: refused: owned by another supervisor: " + detail; !strings.Contains(whole(), want) {
		t.Fatalf("preview lacks the whole refusal:\n%s", whole())
	}
	// Another instance's preview does not carry it.
	r.send(key(tea.KeyDown))
	if strings.Contains(whole(), "Last action") {
		t.Fatalf("bravo's preview shows alpha's outcome:\n%s", whole())
	}
	r.send(key(tea.KeyUp))
	// Acting on the instance again clears the old outcome while the new one is under way.
	r.send(typeText(":run")...)
	r.send(key(tea.KeyEnter)) // its answer has not been fed back: the action is under way
	if strings.Contains(whole(), "Last action") {
		t.Fatalf("an old outcome stays while a new action is under way:\n%s", whole())
	}
}

// A runner killed by a signal skips its exit trap, so status reads "unknown"
// (or still "running", before the next read). When the supervisor saw its child
// end, Run is offered whatever status says the runner state is.
func TestRunIsOfferedAfterThisSessionSawItsRunnerEnd(t *testing.T) {
	for _, state := range []groundhog.RunnerState{groundhog.RunnerUnknown, groundhog.RunnerRunning, groundhog.RunnerStopped, groundhog.RunnerFailed} {
		v := ours("x", state, false)
		key := v.Instance.Path
		exited := map[string]core.RunnerInfo{key: {Instance: key, Phase: core.PhaseExited, Notice: "runner ended on its own (killed) and was not restarted"}}
		if got := actionsOf(t, v, exited); got != "run,stop" {
			t.Errorf("runner %s, exited: offered %q, want run,stop", state, got)
		}
	}
	// Without the supervisor's word, an unknown runner is not offered anything,
	// and a runner this session has not seen end keeps Stop.
	unknown := ours("x", groundhog.RunnerUnknown, false)
	if got := actionsOf(t, unknown, nil); got != "" {
		t.Errorf("unknown runner, no word from the supervisor: offered %q", got)
	}
	running := ours("x", groundhog.RunnerRunning, false)
	live := map[string]core.RunnerInfo{running.Instance.Path: {Instance: running.Instance.Path, Phase: core.PhaseRunning}}
	if got := actionsOf(t, running, live); got != "stop" {
		t.Errorf("live runner: offered %q, want stop", got)
	}
	// An exit does not override what makes any status untrustworthy, or an
	// instance that is not this session's.
	stale := ours("x", groundhog.RunnerUnknown, false)
	stale.Health = core.HealthStale
	if got := actionsOf(t, stale, map[string]core.RunnerInfo{stale.Instance.Path: {Phase: core.PhaseExited}}); got != "" {
		t.Errorf("stale status, exited runner: offered %q", got)
	}
	ext := ours("x", groundhog.RunnerUnknown, false)
	ext.Class = core.ClassExternal
	if got := actionsOf(t, ext, map[string]core.RunnerInfo{ext.Instance.Path: {Phase: core.PhaseExited}}); got != "" {
		t.Errorf("external instance, exited runner: offered %q", got)
	}
}
