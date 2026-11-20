package tui

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"sync"
	"testing"

	tea "github.com/charmbracelet/bubbletea"
	"github.com/charmbracelet/x/ansi"

	"github.com/zealtv/easyhog/internal/core"
	"github.com/zealtv/easyhog/internal/groundhog"
)

// Synthetic instances only: nothing here runs a script.

func supported(name string) core.InstanceView {
	return iv(name, core.ClassSupported, core.HealthOK, core.Inspection{Last: obs(okStatus())})
}

func pausedInstance(name string) core.InstanceView {
	v := supported(name)
	v.Inspection.Last.Status.Paused = true
	return v
}

// ticks is a TickLookup over the keys this session ticks.
func ticks(keys ...string) TickLookup {
	return func(key string) (core.TickInfo, bool) {
		for _, k := range keys {
			if k == key {
				return core.TickInfo{Ticking: true, Interval: core.DefaultTickInterval}, true
			}
		}
		return core.TickInfo{}, false
	}
}

func actionsOf(t *testing.T, v core.InstanceView, look TickLookup) string {
	t.Helper()
	d := FromViewWith(core.View{Root: "/r", Instances: []core.InstanceView{v}}, look)
	var out []string
	for _, a := range d.Nodes[0].Actions {
		if a != ActionOpenFolder {
			out = append(out, string(a))
		}
	}
	return strings.Join(out, ",")
}

// The palette's actions appear only where they can work, by core.Offers, and
// nothing misleading is offered otherwise.
func TestOfferedControlsPerClass(t *testing.T) {
	failed := supported("fd")
	failed.Inspection.Last.Status.Deliveries = []groundhog.Delivery{{Path: "out/x", State: groundhog.DeliveryFailed}}
	malformed := iv("m", core.ClassMalformed, core.HealthOK, core.Inspection{})
	malformed.Instance.Malformed = []string{"groundhog.sh is missing"}
	failingWithLast := supported("fl")
	failingWithLast.Inspection.Err = errors.New("timed out")
	stale := supported("s")
	stale.Health = core.HealthStale
	cases := []struct {
		name string
		v    core.InstanceView
		look TickLookup
		want string
	}{
		{"supported, not ticked here", supported("a"), nil, "run,pause"},
		{"ticked here", supported("b"), ticks("/r/b/.groundhog"), "stop,pause"},
		{"paused", pausedInstance("c"), nil, "run,unpause"},
		{"paused and ticked here", pausedInstance("d"), ticks("/r/d/.groundhog"), "stop,unpause"},
		{"a failed delivery", failed, nil, "run,pause,retry"},
		{"unsupported", iv("k", core.ClassUnsupported, core.HealthOK, core.Inspection{Last: obs(okStatus())}), nil, ""},
		{"legacy", iv("l", core.ClassLegacy, core.HealthOK, core.Inspection{Support: &groundhog.Support{Reason: "old"}}), nil, ""},
		{"malformed", malformed, nil, ""},
		{"not inspected yet", iv("n", core.ClassNotInspected, core.HealthOK, core.Inspection{}), nil, ""},
		{"failing, never read", iv("f", core.ClassFailing, core.HealthOK, core.Inspection{Err: errors.New("timed out")}), nil, ""},
		{"failing, with an old status", failingWithLast, nil, ""},
		{"stale", stale, nil, ""},
	}
	for _, c := range cases {
		look := c.look
		if look == nil {
			look = ticks()
		}
		if got := actionsOf(t, c.v, look); got != c.want {
			t.Errorf("%s: offers %q, want %q", c.name, got, c.want)
		}
	}
	// Without a ticker nothing is offered, whatever the class.
	for _, d := range []Data{FromViewWith(core.View{Instances: []core.InstanceView{supported("a")}}, nil),
		FromView(core.View{Instances: []core.InstanceView{supported("a")}})} {
		if got := d.Nodes[0].Actions; len(got) != 1 || got[0] != ActionOpenFolder {
			t.Errorf("no ticker: %v", got)
		}
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

// The preview says why nothing is offered, and how a not-yet-inspected instance
// becomes runnable.
func TestPreviewSaysWhyNoControlIsOffered(t *testing.T) {
	cases := map[string]struct {
		v    core.InstanceView
		want string
	}{
		"fresh":     {iv("n", core.ClassNotInspected, core.HealthOK, core.Inspection{}), "not until its first status read (select it, or choose Refresh)"},
		"legacy":    {iv("l", core.ClassLegacy, core.HealthOK, core.Inspection{}), "an older Groundhog or lacks the runtime commands"},
		"supported": {supported("u"), "Palette offers: Run, Pause instance"},
	}
	for name, c := range cases {
		d := FromViewWith(core.View{Instances: []core.InstanceView{c.v}}, ticks())
		if !strings.Contains(d.Nodes[0].Preview, c.want) {
			t.Errorf("%s: preview lacks %q:\n%s", name, c.want, d.Nodes[0].Preview)
		}
	}
	d := FromViewWith(core.View{Instances: []core.InstanceView{supported("r")}}, ticks("/r/r/.groundhog"))
	if p := d.Nodes[0].Preview; !strings.Contains(p, "Palette offers: Stop") || !strings.Contains(p, "This session ticks it every 1m00s") {
		t.Errorf("ticked here:\n%s", p)
	}
}

// fakeControl stands in for the ticker; the tests that use it assert on the
// calls and on what the terminal then shows.
type fakeControl struct {
	mu      sync.Mutex
	calls   []string // "run alpha", "pause alpha", "retry alpha schedule/x", …
	release chan struct{}
	run     func(key string) (core.RunResult, error)
	retry   func(key, item string) (core.RetryResult, error)
	pause   func(key string, pause bool) error
	toggle  func(key, item string, pause bool) (core.ItemResult, error)
}

func (f *fakeControl) called(what string) {
	f.mu.Lock()
	f.calls = append(f.calls, what)
	f.mu.Unlock()
	if f.release != nil {
		<-f.release
	}
}

func short2(key string) string {
	return strings.TrimSuffix(strings.TrimPrefix(key, "/r/"), "/.groundhog")
}

func (f *fakeControl) Run(_ context.Context, key string) (core.RunResult, error) {
	f.called("run " + short2(key))
	if f.run != nil {
		return f.run(key)
	}
	return core.RunResult{Instance: key, Ticked: true}, nil
}

func (f *fakeControl) Stop(key string) core.StopResult {
	f.called("stop " + short2(key))
	return core.StopResult{Instance: key, WasTicking: true}
}

func (f *fakeControl) Retry(_ context.Context, key, item string) (core.RetryResult, error) {
	f.called(strings.TrimSpace("retry " + short2(key) + " " + item))
	if f.retry != nil {
		return f.retry(key, item)
	}
	return core.RetryResult{Instance: key, Started: []string{"out/x-1"}}, nil
}

func (f *fakeControl) SetPaused(_ context.Context, key string, pause bool) error {
	verb := "unpause"
	if pause {
		verb = "pause"
	}
	f.called(verb + " " + short2(key))
	if f.pause != nil {
		return f.pause(key, pause)
	}
	return nil
}

func (f *fakeControl) SetItemPaused(_ context.Context, key, item string, pause bool) (core.ItemResult, error) {
	f.called(fmt.Sprintf("item %s %s %v", short2(key), item, pause))
	if f.toggle != nil {
		return f.toggle(key, item, pause)
	}
	return core.ItemResult{Instance: key, Item: core.ItemIdentity(item), From: item, To: core.ItemTarget(item, pause), Paused: pause, Locked: true}, nil
}

func (f *fakeControl) list() string {
	f.mu.Lock()
	defer f.mu.Unlock()
	return strings.Join(f.calls, "; ")
}

type rig struct {
	t      *testing.T
	m      Model
	f      *fakeControl
	reread []string
	mu     sync.Mutex
	ticked []string
}

// newRig shows alpha (Run), bravo (ticked here: Stop) and charlie (paused:
// Unpause instance), at 100x30.
func newRig(t *testing.T, f *fakeControl) *rig {
	t.Helper()
	r := &rig{t: t, f: f, ticked: []string{"/r/bravo/.groundhog"}}
	r.m = NewWithData(r.data(supported("alpha"), supported("bravo"), pausedInstance("charlie")),
		WithControl(f, func(k string) { r.mu.Lock(); r.reread = append(r.reread, k); r.mu.Unlock() }))
	next, _ := r.m.Update(tea.WindowSizeMsg{Width: 100, Height: 30})
	r.m = next.(Model)
	return r
}

func (r *rig) data(vs ...core.InstanceView) Data {
	return FromViewWith(core.View{Root: "/r", Instances: vs}, ticks(r.ticked...))
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

// do runs the palette command typed as query on the selected row, feeding its
// result back, and returns the notice.
func (r *rig) do(query string) string {
	r.t.Helper()
	r.send(typeText(":" + query)...)
	r.send(r.send(key(tea.KeyEnter))...)
	return r.m.notice
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
	r := newRig(t, &fakeControl{})
	r.send(typeText(":")...)
	line := ""
	for _, l := range strings.Split(r.screen(), "\n") {
		if strings.Contains(l, "type a command") {
			line = l
		}
	}
	if !strings.Contains(line, "on alpha") {
		t.Fatalf("the name is not on the prompt line: %q", line)
	}
	r.send(key(tea.KeyEsc), key(tea.KeyDown), typeText(":")[0])
	if !strings.Contains(r.screen(), "on bravo") || strings.Contains(r.screen(), "on alpha") {
		t.Fatalf("prompt after moving:\n%s", r.screen())
	}
	empty := NewWithData(Data{Root: "/r"})
	empty, _ = feed(empty, tea.WindowSizeMsg{Width: 100, Height: 30}, typeText(":")[0])
	if strings.Contains(ansi.Strip(empty.View()), " on ") {
		t.Errorf("an empty list names an instance:\n%s", ansi.Strip(empty.View()))
	}
}

func TestPalettePromptKeepsTheEndOfALongPathAndTheQuery(t *testing.T) {
	v := supported("x")
	v.Instance.Display = strings.Repeat("deep/", 12) + "project/.groundhog"
	m := NewWithData(FromViewWith(core.View{Instances: []core.InstanceView{v}}, ticks()), WithControl(&fakeControl{}, nil))
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
	v := supported("x")
	v.Instance.Display = "evil\x1b[2J\x07dir/.groundhog"
	m := NewWithData(FromViewWith(core.View{Instances: []core.InstanceView{v}}, ticks()))
	m, _ = feed(m, tea.WindowSizeMsg{Width: 100, Height: 30}, typeText(":")[0])
	if out := m.View(); strings.Contains(out, "\x1b[2J") || strings.Contains(out, "\x07") {
		t.Fatal("control sequence reached the frame")
	}
}

// Run goes to the ticker off the update loop, shows "Running …" while it
// waits, offers no action on that instance again (the palette does not queue a
// second one), and then shows the exact outcome.
func TestRunShowsTransitionThenOutcomeAndDoesNotQueueASecond(t *testing.T) {
	f := &fakeControl{release: make(chan struct{})}
	r := newRig(t, f)
	if r.names() != "Run,Pause instance,Open folder,Refresh,Quit" {
		t.Fatalf("alpha offers %s", r.names())
	}
	r.send(typeText(":run")...)
	next, cmd := r.m.Update(key(tea.KeyEnter))
	r.m = next.(Model)
	if cmd == nil || f.list() != "" {
		t.Fatal("Run ran on the update loop, or scheduled nothing")
	}
	if !strings.Contains(r.screen(), "Running alpha…") {
		t.Fatalf("no transitional state:\n%s", r.screen())
	}
	if r.names() != "Open folder,Refresh,Quit" {
		t.Fatalf("while Run runs the palette offers %s", r.names())
	}
	r.send(typeText(":run")...)
	if msgs := r.send(key(tea.KeyEnter)); len(msgs) != 0 {
		t.Fatalf("a second Run scheduled %v", msgs)
	}
	r.send(key(tea.KeyEsc))

	done := make(chan tea.Msg, 1)
	go func() { done <- cmd() }()
	close(f.release)
	r.send(<-done)
	if f.list() != "run alpha" {
		t.Fatalf("calls %q", f.list())
	}
	if want := "Run: alpha: ticked, and ticking every minute while Easyhog runs"; r.m.notice != want || r.m.failure {
		t.Fatalf("notice %q (failure %v)", r.m.notice, r.m.failure)
	}
	if len(r.reread) != 1 || r.reread[0] != "/r/alpha/.groundhog" {
		t.Fatalf("reread %v", r.reread)
	}
	if len(r.m.busy) != 0 || r.names() != "Run,Pause instance,Open folder,Refresh,Quit" {
		t.Fatalf("not offered again after the answer: busy %v, %s", r.m.busy, r.names())
	}
}

// After Run, the next view (this session ticks it) shows the instance running
// with Stop offered instead of Run.
func TestRunThenRefreshShowsRunning(t *testing.T) {
	r := newRig(t, &fakeControl{})
	r.do("run")
	if n := r.m.rows[0].node; n.State != StateStopped {
		t.Fatalf("before the re-read the row is %v", n.State)
	}
	r.ticked = append(r.ticked, "/r/alpha/.groundhog")
	r.send(SnapshotMsg{Data: r.data(supported("alpha"), supported("bravo"), pausedInstance("charlie"))})
	if n := r.m.rows[0].node; n.State != StateRunning || r.m.selectedInstance() != "/r/alpha/.groundhog" {
		t.Fatalf("row %v, selected %q", n.State, r.m.selectedInstance())
	}
	if r.names() != "Stop,Pause instance,Open folder,Refresh,Quit" {
		t.Fatalf("after Run the palette offers %s", r.names())
	}
}

func TestStopPauseAndUnpauseGoToTheTickerAndSaySoPlainly(t *testing.T) {
	f := &fakeControl{}
	r := newRig(t, f)
	r.send(key(tea.KeyDown)) // bravo: ticked here
	if got := r.do("stop"); got != "Stop: bravo: stopped ticking. Runs already started go on in their own sessions, and anything else that ticks it still does" {
		t.Errorf("stop: %q", got)
	}
	if got := r.do("pause in"); got != "Pause instance: bravo: paused. No tick, from here or anywhere else, fires an item or starts a run until Unpause instance; runs already going carry on" {
		t.Errorf("pause: %q", got)
	}
	r.send(key(tea.KeyDown)) // charlie: paused
	if r.names() != "Run,Unpause instance,Open folder,Refresh,Quit" {
		t.Fatalf("charlie offers %s", r.names())
	}
	if got := r.do("unpause"); got != "Unpause instance: charlie: unpaused. The next tick fires what is due and starts what waits" {
		t.Errorf("unpause: %q", got)
	}
	if f.list() != "stop bravo; pause bravo; unpause charlie" {
		t.Errorf("calls %q", f.list())
	}
	if len(r.reread) != 3 {
		t.Errorf("reread %v", r.reread)
	}
}

// Retry acts on the node the palette opened on: an item's own deliveries, or
// all of the instance's, and the notice says what started and what was busy.
func TestRetryOnAnItemRetriesThatItemsDeliveriesAndSaysWhatWasBusy(t *testing.T) {
	v := supported("alpha")
	st := &v.Inspection.Last.Status
	st.Schedule = []groundhog.ScheduleItem{{Path: "schedule/daily/a", ItemID: "a"}, {Path: "schedule/daily/b", ItemID: "b"}}
	a, b := "schedule/daily/a", "schedule/daily/b"
	st.Deliveries = []groundhog.Delivery{
		{Path: "out/a-1", State: groundhog.DeliveryFailed, SchedulePath: &a},
		{Path: "out/b-1", State: groundhog.DeliveryInterrupted, SchedulePath: &b},
	}
	f := &fakeControl{retry: func(key, item string) (core.RetryResult, error) {
		if item == "" {
			return core.RetryResult{Started: []string{"out/a-1"}, Busy: []string{"out/b-1"}}, nil
		}
		return core.RetryResult{Started: []string{"out/a-1"}}, nil
	}}
	r := &rig{t: t, f: f}
	r.m = NewWithData(r.data(v), WithControl(f, nil))
	r.m, _ = feed(r.m, tea.WindowSizeMsg{Width: 100, Height: 30})
	if n := r.m.rows[0].node; n.State != StateFailed || n.ActionNotes[ActionRetry] != "2 to retry" ||
		!strings.Contains(n.Preview, "An interrupted run lost its wrapper") {
		t.Fatalf("instance row %+v", n)
	}
	if got := r.do("ret"); got != "Retry: alpha: 1 run started again (out/a-1); busy: nothing was started for out/b-1 (its run is going, or the instance's lock was held); try again shortly" {
		t.Errorf("instance: %q", got)
	}
	r.send(key(tea.KeyRight), key(tea.KeyDown)) // the first item
	if r.m.rows[r.m.sel].node.ActionNotes[ActionRetry] != "1 to retry" {
		t.Fatalf("item notes %v", r.m.rows[r.m.sel].node.ActionNotes)
	}
	if got := r.do("retry"); got != "Retry: alpha: 1 run started again (out/a-1)" {
		t.Errorf("item: %q", got)
	}
	if f.list() != "retry alpha; retry alpha schedule/daily/a" {
		t.Errorf("calls %q", f.list())
	}
	// A paused instance offers no Retry, and its preview says what waits.
	p := pausedInstance("p")
	p.Inspection.Last.Status.Deliveries = st.Deliveries
	n := FromViewWith(core.View{Instances: []core.InstanceView{p}}, ticks()).Nodes[0]
	if hasAction(n, ActionRetry) || !strings.Contains(n.Preview, "Retry waits for Unpause instance") {
		t.Errorf("paused: %v\n%s", n.Actions, n.Preview)
	}
}

// The words for each failure, exactly. A refusal names the instance by its
// relative path and gives the ticker's reason and detail as they are.
func TestOutcomeTextIsExact(t *testing.T) {
	const l = "alpha/.groundhog"
	cases := []struct {
		name string
		err  error
		want string
	}{
		{"unsupported", &core.RefusedError{Op: "run", Reason: core.RefusedUnsupported, Detail: "lacks required capabilities: handlers-v1"},
			"alpha/.groundhog: refused: not supported: lacks required capabilities: handlers-v1"},
		{"paused", &core.RefusedError{Op: "retry", Reason: core.RefusedPaused, Detail: "Unpause instance first"},
			"alpha/.groundhog: refused: the instance is paused: Unpause instance first"},
		{"quitting", &core.RefusedError{Op: "run", Reason: core.RefusedClosing}, "alpha/.groundhog: refused: quitting"},
		{"busy", &groundhog.ExitError{Command: "pause", Code: 4, Kind: groundhog.KindBusy},
			"alpha/.groundhog: busy (another writer holds the instance's lock); try again shortly"},
		{"other failure", errors.New("status: exit status 6"), "alpha/.groundhog: failed: status: exit status 6"},
		{"wrapped refusal", fmt.Errorf("outer: %w", &core.RefusedError{Op: "retry", Reason: core.RefusedNotOffered, Detail: "nothing here"}),
			"alpha/.groundhog: refused: not offered: nothing here"},
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
		RunText(l, core.RunResult{Already: true}):                          "alpha/.groundhog: already ticked by this session; nothing changed",
		RunText(l, core.RunResult{}):                                       "alpha/.groundhog: ticking every minute; the first tick has not finished yet",
		RunText(l, core.RunResult{Ticked: true, TickErr: errors.New("x")}): "alpha/.groundhog: ticking every minute; the first tick reported a problem: x",
		StopText(l, core.StopResult{}):                                     "alpha/.groundhog: this session was not ticking it; nothing changed",
		RetryText(l, core.RetryResult{Gone: []string{"out/x"}}):            "alpha/.groundhog: nothing to run for out/x",
		RetryText(l, core.RetryResult{Busy: []string{"out/x", "out/y"}}):   "alpha/.groundhog: busy: nothing was started for out/x, out/y (its run is going, or the instance's lock was held); try again shortly",
		RetryText(l, core.RetryResult{Started: []string{"out/y"}, Failed: []core.RetryFailure{{Delivery: "out/x", Err: errors.New("exit 6")}}}): "alpha/.groundhog: 1 run started again (out/y); could not start out/x (exit 6)",
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
	f := &fakeControl{run: func(key string) (core.RunResult, error) {
		return core.RunResult{}, &core.RefusedError{Op: "run", Instance: key, Reason: core.RefusedUnsupported, Detail: "lacks handlers-v1"}
	}}
	r := newRig(t, f)
	want := "Run: alpha: refused: not supported: lacks handlers-v1"
	if got := r.do("run"); got != want || !r.m.failure {
		t.Fatalf("notice %q failure %v", got, r.m.failure)
	}
	if !strings.Contains(r.screen(), "! "+want) {
		t.Fatalf("not in the status line:\n%s", r.screen())
	}
	if len(r.reread) != 1 || len(r.m.busy) != 0 {
		t.Fatalf("reread %v busy %v", r.reread, r.m.busy)
	}
}

// The selection holds while a Run is under way and across refreshes, and a
// result for an instance that is no longer selected does not move it.
func TestSelectionHoldsDuringARefreshAndAnActionInFlight(t *testing.T) {
	f := &fakeControl{release: make(chan struct{})}
	r := newRig(t, f)
	r.send(typeText(":run")...)
	next, cmd := r.m.Update(key(tea.KeyEnter))
	r.m = next.(Model)
	alpha, bravo := "/r/alpha/.groundhog", "/r/bravo/.groundhog"
	r.send(key(tea.KeyDown)) // bravo, while alpha's Run is under way
	if r.m.selectedInstance() != bravo || r.names() != "Stop,Pause instance,Open folder,Refresh,Quit" {
		t.Fatalf("selected %q offers %s", r.m.selectedInstance(), r.names())
	}
	r.send(SnapshotMsg{Data: r.data(supported("aaa"), supported("alpha"), supported("bravo"))})
	if r.m.selectedInstance() != bravo {
		t.Fatalf("selection moved to %q", r.m.selectedInstance())
	}
	if _, busy := r.m.busy[alpha]; !busy || !strings.Contains(r.screen(), "Running alpha…") {
		t.Fatalf("alpha's Run is not shown as under way:\n%s", r.screen())
	}
	close(f.release)
	r.send(cmd())
	if r.m.selectedInstance() != bravo || len(r.m.busy) != 0 {
		t.Fatalf("selection %q busy %v after the result", r.m.selectedInstance(), r.m.busy)
	}
}

// A palette opened on one instance acts on it or not at all, even if a refresh
// makes the selection land elsewhere.
func TestBoundInstanceSurvivesWhileSomethingIsInFlight(t *testing.T) {
	f := &fakeControl{}
	r := newRig(t, f)
	r.send(typeText(":ru")...)
	r.send(SnapshotMsg{Data: r.data(supported("bravo"))}) // alpha vanishes behind the open palette
	r.send(key(tea.KeyEnter))
	if f.list() != "" || !strings.Contains(r.m.notice, "Nothing ran:") {
		t.Fatalf("calls %q notice %q", f.list(), r.m.notice)
	}
}

// Quit never goes through the control handler: the shell quits, and main ends
// in core.Ticker.Quit for every way out.
func TestQuitDoesNotTouchTheControl(t *testing.T) {
	f := &fakeControl{}
	r := newRig(t, f)
	r.send(typeText(":quit")...)
	if _, cmd := r.m.Update(key(tea.KeyEnter)); !quits(cmd) {
		t.Fatal("palette Quit did not quit")
	}
	if _, cmd := r.m.Update(key(tea.KeyCtrlC)); !quits(cmd) {
		t.Fatal("ctrl+c did not quit")
	}
	if f.list() != "" {
		t.Fatalf("calls %q", f.list())
	}
}

// Bare keys still start nothing with the real handler installed.
func TestBareKeysNeverReachTheControl(t *testing.T) {
	f := &fakeControl{}
	r := newRig(t, f)
	for _, k := range []tea.Msg{key(tea.KeyEnter), key(tea.KeySpace), key(tea.KeyRight), tea.KeyMsg{Type: tea.KeyRunes, Runes: []rune("p")},
		tea.KeyMsg{Type: tea.KeyRunes, Runes: []rune("y")}, key(tea.KeyTab)} {
		if msgs := r.send(k); len(msgs) != 0 {
			t.Fatalf("key %v produced %v", k, msgs)
		}
	}
	if f.list() != "" {
		t.Fatalf("calls %q", f.list())
	}
}

// The status line holds one line and cuts a long reason; the whole outcome stays
// in the preview of the instance it was about, until that instance is acted on again.
func TestLongRefusalStaysWholeInThePreview(t *testing.T) {
	detail := strings.Repeat("a long reason that goes on ", 6)
	f := &fakeControl{run: func(key string) (core.RunResult, error) {
		return core.RunResult{}, &core.RefusedError{Op: "run", Instance: key, Reason: core.RefusedUnsupported, Detail: detail}
	}}
	r := newRig(t, f)
	r.m, _ = feed(r.m, tea.WindowSizeMsg{Width: 80, Height: 24})
	r.do("run")
	whole := func() string {
		return strings.Join(strings.Fields(ansi.Strip(strings.Join(r.m.previewLines(40), "\n"))), " ")
	}
	if want := "Last action: Run: alpha: refused: not supported: " + strings.TrimSpace(detail); !strings.Contains(whole(), want) {
		t.Fatalf("preview lacks the whole refusal:\n%s", whole())
	}
	r.send(key(tea.KeyDown))
	if strings.Contains(whole(), "Last action") {
		t.Fatalf("bravo's preview shows alpha's outcome:\n%s", whole())
	}
	r.send(key(tea.KeyUp))
	r.send(typeText(":run")...)
	r.send(key(tea.KeyEnter)) // its answer has not been fed back: the action is under way
	if strings.Contains(whole(), "Last action") {
		t.Fatalf("an old outcome stays while a new action is under way:\n%s", whole())
	}
}

// A tick that failed keeps the row running, says so in the detail, and gives
// the error in the preview; the loop goes on.
func TestATickThatFailedIsShownAndTheRowStaysRunning(t *testing.T) {
	v := supported("x")
	look := func(string) (core.TickInfo, bool) {
		return core.TickInfo{Ticking: true, Interval: core.DefaultTickInterval, LastBegan: at, LastEnded: at,
			LastErr: errors.New("groundhog tick: exit 1 (unexpected exit): firing failed")}, true
	}
	n := FromViewWith(core.View{Instances: []core.InstanceView{v}}, look).Nodes[0]
	if n.State != StateRunning || !strings.Contains(n.Detail, "the last tick failed") ||
		!strings.Contains(n.Preview, "its last tick began at 09:30:15 and failed: groundhog tick: exit 1") {
		t.Fatalf("%+v", n)
	}
}

func strp(s string) *string { return &s }

// delivery is a delivery at path in state, fired from item ("" for none).
func delivery(path string, state groundhog.DeliveryState, item string) groundhog.Delivery {
	d := groundhog.Delivery{Path: path, State: state, Run: true}
	if item != "" {
		d.SchedulePath = strp(item)
	}
	return d
}

func withDeliveries(v core.InstanceView, dels ...groundhog.Delivery) core.InstanceView {
	st := v.Inspection.Last.Status
	st.Deliveries = append(append([]groundhog.Delivery(nil), st.Deliveries...), dels...)
	v.Inspection.Last = obs(st)
	return v
}

// toggleCalls lists the item toggles asked of f, as "item <instance> <path> <pause>".
func (f *fakeControl) toggleCalls() []string {
	f.mu.Lock()
	defer f.mu.Unlock()
	var out []string
	for _, c := range f.calls {
		if strings.HasPrefix(c, "item ") {
			out = append(out, c)
		}
	}
	return out
}
