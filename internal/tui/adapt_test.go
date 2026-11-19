package tui

import (
	"errors"
	"strings"
	"testing"
	"time"

	tea "github.com/charmbracelet/bubbletea"

	"github.com/zealtv/easyhog/internal/core"
	"github.com/zealtv/easyhog/internal/discovery"
	"github.com/zealtv/easyhog/internal/groundhog"
)

var at = time.Date(2026, 11, 15, 9, 30, 15, 0, time.UTC)

func obs(st groundhog.Status) *core.Observation { return &core.Observation{Status: st, TakenAt: at} }

func okStatus() groundhog.Status {
	return groundhog.Status{Consistent: true, Revision: 3, Runner: groundhog.Runner{State: groundhog.RunnerStopped}}
}

func iv(name string, class core.Class, health core.Health, in core.Inspection) core.InstanceView {
	return core.InstanceView{Instance: discovery.Instance{Path: "/r/" + name + "/.groundhog", Display: name + "/.groundhog"},
		Class: class, Health: health, Inspection: in}
}

func TestFromViewDistinguishesEveryKindOfInstance(t *testing.T) {
	good := obs(okStatus())
	owned := okStatus()
	owned.Owner = &groundhog.Owner{ID: "o-1", Label: "other-supervisor"}
	bad := okStatus()
	bad.Consistent = false
	malformed := iv("malformed", core.ClassMalformed, core.HealthOK, core.Inspection{})
	malformed.Instance.Malformed = []string{"groundhog.sh is missing"}
	view := core.View{Root: "/r", Instances: []core.InstanceView{
		iv("fresh", core.ClassNotInspected, core.HealthOK, core.Inspection{}),
		iv("unowned", core.ClassUnowned, core.HealthOK, core.Inspection{Last: good}),
		iv("external", core.ClassExternal, core.HealthOK, core.Inspection{Last: obs(owned)}),
		iv("legacy", core.ClassLegacy, core.HealthOK, core.Inspection{Support: &groundhog.Support{Reason: "lacks required capabilities: x"}}),
		iv("partial", core.ClassUnsupported, core.HealthOK, core.Inspection{Last: good}),
		iv("failing", core.ClassUnowned, core.HealthOK, core.Inspection{Last: good, Err: errors.New("deadline"), ErrAt: at}),
		iv("neverread", core.ClassFailing, core.HealthOK, core.Inspection{Err: errors.New("exit 6"), ErrAt: at}),
		iv("stale", core.ClassUnowned, core.HealthStale, core.Inspection{Last: good}),
		iv("inconsistent", core.ClassUnowned, core.HealthInconsistent, core.Inspection{Last: obs(bad), Attempts: 3}),
		malformed,
	}}
	d := FromView(view)
	want := map[string]struct {
		state State
		in    string
	}{
		"fresh":        {StateStopped, "not inspected"},
		"unowned":      {StateStopped, "inspected 09:30:15"},
		"external":     {StateExternal, "owned by other-supervisor"},
		"legacy":       {StateUnsupported, "legacy"},
		"partial":      {StateUnsupported, "read-only"},
		"failing":      {StateFailed, "last good 09:30:15"},
		"neverread":    {StateFailed, "failed"},
		"stale":        {StateStopped, "(stale)"},
		"inconsistent": {StateInconsistent, "inconsistent"},
		"malformed":    {StateUnsupported, "not run"},
	}
	if len(d.Nodes) != len(want) {
		t.Fatalf("%d nodes", len(d.Nodes))
	}
	for _, n := range d.Nodes {
		w := want[n.Label]
		if n.State != w.state || !strings.Contains(n.Detail, w.in) {
			t.Errorf("%s: state %v detail %q; want %v containing %q", n.Label, n.State, n.Detail, w.state, w.in)
		}
		if len(n.Actions) != 1 || n.Actions[0] != ActionOpenFolder {
			t.Errorf("%s offers actions %v; only Open folder belongs here", n.Label, n.Actions)
		}
	}
	byLabel := func(l string) Node {
		for _, n := range d.Nodes {
			if n.Label == l {
				return n
			}
		}
		t.Fatalf("no node %s", l)
		return Node{}
	}
	if p := byLabel("failing").Preview; !strings.Contains(p, "Error at 09:30:15: deadline") || !strings.Contains(p, "the last status that could be read") {
		t.Errorf("failing preview hides the error or the kept snapshot:\n%s", p)
	}
	if p := byLabel("inconsistent").Preview; !strings.Contains(p, "inconsistent status after 3 reads") {
		t.Errorf("inconsistent preview:\n%s", p)
	}
	if p := byLabel("fresh").Preview; !strings.Contains(p, "nothing was run") {
		t.Errorf("fresh preview:\n%s", p)
	}
	if p := byLabel("external").Preview; !strings.Contains(p, "other-supervisor") {
		t.Errorf("external preview:\n%s", p)
	}
}

func TestFromViewKeepsItemKeysWhenStatusArrivesAfterTheListing(t *testing.T) {
	listed := iv("a", core.ClassNotInspected, core.HealthOK, core.Inspection{})
	listed.Files = discovery.Glance{Schedule: []discovery.ScheduleDir{{Path: "daily", Leaf: false}, {Path: "daily/x", Leaf: true}}}
	st := okStatus()
	st.Schedule = []groundhog.ScheduleItem{{Path: "schedule/daily/x", ItemID: "x"}}
	read := iv("a", core.ClassUnowned, core.HealthOK, core.Inspection{Last: obs(st)})
	read.Files = listed.Files

	before := FromView(core.View{Instances: []core.InstanceView{listed}}).Nodes[0].Children
	after := FromView(core.View{Instances: []core.InstanceView{read}}).Nodes[0].Children
	if len(before) != 1 || len(after) != 1 || before[0].Key != after[0].Key {
		t.Fatalf("item keys differ: %+v vs %+v", before, after)
	}
	if before[0].Detail != "not inspected" || after[0].State != StateScheduled {
		t.Errorf("listing item %+v, status item %+v", before[0], after[0])
	}
}

func TestFromViewShowsAFailedRescanBesideTheLastList(t *testing.T) {
	d := FromView(core.View{Root: "/r", DiscoverErr: errors.New("permission denied"),
		Instances: []core.InstanceView{iv("a", core.ClassNotInspected, core.HealthOK, core.Inspection{})}})
	if !strings.Contains(d.Err, "permission denied") || len(d.Nodes) != 1 {
		t.Fatalf("%+v", d)
	}
}

// run executes a command and any commands batched in it.
func run(cmd tea.Cmd) []tea.Msg {
	if cmd == nil {
		return nil
	}
	msg := cmd()
	if b, ok := msg.(tea.BatchMsg); ok {
		var out []tea.Msg
		for _, c := range b {
			out = append(out, run(c)...)
		}
		return out
	}
	return []tea.Msg{msg}
}

func viewOf(names ...string) core.View {
	var v core.View
	for _, n := range names {
		x := iv(n, core.ClassNotInspected, core.HealthOK, core.Inspection{})
		x.Files = discovery.Glance{Schedule: []discovery.ScheduleDir{{Path: "daily/x", Leaf: true}}}
		v.Instances = append(v.Instances, x)
	}
	return v
}

func update(m Model, msgs ...tea.Msg) (Model, []tea.Msg) {
	var all []tea.Msg
	for _, msg := range msgs {
		next, cmd := m.Update(msg)
		m = next.(Model)
		all = append(all, run(cmd)...)
	}
	return m, all
}

func TestSelectionObserverHearsOnlyInstanceChanges(t *testing.T) {
	var heard []string
	m := NewWithData(FromView(viewOf("a", "b", "c")), WithSelectionObserver(func(k string) { heard = append(heard, k) }))
	b, c := "/r/b/.groundhog", "/r/c/.groundhog"

	m, _ = update(m, key(tea.KeyDown))
	m, _ = update(m, key(tea.KeyRight)) // open b: its item is not an instance change
	m, _ = update(m, key(tea.KeyDown))  // onto b's item
	m, _ = update(m, key(tea.KeyDown))  // c
	if strings.Join(heard, ",") != b+","+c {
		t.Fatalf("heard %q; want b then c only", heard)
	}

	// Additions, removals and reordering around the selection: by key, never index.
	m, _ = update(m, SnapshotMsg{FromView(viewOf("0", "a", "b", "c", "z"))})
	if m.selectedKey() != c || len(heard) != 2 {
		t.Fatalf("selection %q, heard %q after additions", m.selectedKey(), heard)
	}
	m, _ = update(m, SnapshotMsg{FromView(viewOf("a", "b"))}) // c is gone
	if got := m.selectedInstance(); got != b {
		t.Fatalf("selection %q after removing c; want b", got)
	}
	if heard[len(heard)-1] != "" {
		t.Fatalf("heard %q; a highlight moved by a removal reports no selection", heard)
	}
	m, _ = update(m, key(tea.KeyUp)) // the reader's next key reports again
	if heard[len(heard)-1] == "" {
		t.Fatalf("heard %q; the next arrow key should report an instance", heard)
	}
	m, _ = update(m, SnapshotMsg{FromView(viewOf())})
	if heard[len(heard)-1] != "" {
		t.Fatalf("heard %q; an empty list means no selection", heard)
	}
}

func TestRefreshRunsOnlyFromThePalette(t *testing.T) {
	calls := 0
	m := NewWithData(FromView(viewOf("a")), WithRefresh(func() { calls++ }))
	var msgs []tea.Msg
	for _, k := range []tea.Msg{key(tea.KeyEnter), key(tea.KeySpace), key(tea.KeyRight), key(tea.KeyTab), tea.KeyMsg{Type: tea.KeyRunes, Runes: []rune("r")}} {
		m, msgs = update(m, k)
		if len(msgs) != 0 || calls != 0 {
			t.Fatalf("a bare key refreshed: %v", k)
		}
	}
	m, _ = update(m, typeText(":refresh")...)
	m, msgs = update(m, key(tea.KeyEnter))
	if calls != 1 || len(msgs) != 1 {
		t.Fatalf("calls %d msgs %v", calls, msgs)
	}
	if r, ok := msgs[0].(ResultMsg); !ok || r.Action != ActionRefresh || r.Err != nil {
		t.Fatalf("result %+v", msgs[0])
	}
	// Other actions still report that they are not available.
	m2 := NewWithData(SampleData("full"), WithRefresh(func() {}))
	if r, ok := m2.handler(ActionRequest{Action: ActionRun})().(ResultMsg); !ok || !errors.Is(r.Err, ErrNotAvailable) {
		t.Fatal("run should still answer through the default handler")
	}
}

// A runner kept alive by a pause reads paused: true with runner.state: running.
// This session's instance then shows "paused", not "running"; an external or
// unowned instance keeps the generic word for its class.
func TestFromViewShowsThisSessionsPausedRunnerAsPaused(t *testing.T) {
	ext := okStatus()
	ext.Owner = &groundhog.Owner{ID: "other", Label: "other"}
	ext.Runner.State, ext.Paused = groundhog.RunnerRunning, true
	unow := okStatus()
	unow.Paused = true
	cases := []struct {
		name  string
		v     core.InstanceView
		state State
		word  string
	}{
		{"ours-running", ours("a", groundhog.RunnerRunning, false), StateRunning, "running"},
		{"ours-paused", ours("b", groundhog.RunnerRunning, true), StatePaused, "paused"},
		{"external-paused", iv("c", core.ClassExternal, core.HealthOK, core.Inspection{Last: obs(ext)}), StateExternal, "owned by other"},
		{"unowned-paused", iv("d", core.ClassUnowned, core.HealthOK, core.Inspection{Last: obs(unow)}), StateStopped, "not enrolled"},
	}
	for _, c := range cases {
		d := FromViewWith(core.View{Instances: []core.InstanceView{c.v}}, lookup(nil))
		n := d.Nodes[0]
		if n.State != c.state || !strings.Contains(n.Detail, c.word) {
			t.Errorf("%s: state %v detail %q, want %v containing %q", c.name, n.State, n.Detail, c.state, c.word)
		}
	}
	if StatePaused.label() != "paused" || StatePaused.glyph() == StateRunning.glyph() || StatePaused.glyph() == StatePending.glyph() {
		t.Errorf("paused needs its own word and glyph: %q %q", StatePaused.label(), StatePaused.glyph())
	}
}

// An instance this session owns whose runner is not
// running read "not enrolled", though Quit will stop and release it. The row says
// what holds, and an adopted instance says so before it has been inspected.
func TestFromViewNamesAnOwnedInstanceWithoutARunner(t *testing.T) {
	const key = "/r/a/.groundhog"
	adopted := core.RunnerInfo{Instance: key, Phase: core.PhaseEnrolled, Adopted: true}
	cases := []struct {
		name  string
		v     core.InstanceView
		infos map[string]core.RunnerInfo
		word  string
	}{
		{"stopped", ours("a", groundhog.RunnerStopped, false), nil, "enrolled, no runner"},
		{"runner failed", ours("a", groundhog.RunnerFailed, false), nil, "enrolled, no runner"},
		{"unknown (adopted, its runner died)", ours("a", groundhog.RunnerUnknown, false), map[string]core.RunnerInfo{key: adopted}, "enrolled, no runner"},
		{"stopping", ours("a", groundhog.RunnerStopping, false), nil, "runner stopping"},
		{"adopted, not inspected", iv("a", core.ClassNotInspected, core.HealthOK, core.Inspection{}), map[string]core.RunnerInfo{key: adopted}, "adopted, not inspected"},
		{"not adopted, not inspected", iv("a", core.ClassNotInspected, core.HealthOK, core.Inspection{}), nil, "not inspected"},
	}
	for _, c := range cases {
		n := FromViewWith(core.View{Instances: []core.InstanceView{c.v}}, lookup(c.infos)).Nodes[0]
		if n.State != StateStopped || !strings.HasPrefix(n.Detail, c.word) {
			t.Errorf("%s: state %v detail %q, want it to start %q", c.name, n.State, n.Detail, c.word)
		}
		if strings.Contains(n.Detail, "not enrolled") {
			t.Errorf("%s: detail %q says not enrolled for an instance this session holds", c.name, n.Detail)
		}
	}
	// An unowned instance still reads "not enrolled".
	if n := FromViewWith(core.View{Instances: []core.InstanceView{unowned("u")}}, lookup(nil)).Nodes[0]; !strings.HasPrefix(n.Detail, "not enrolled") {
		t.Errorf("unowned detail %q", n.Detail)
	}
}

// The header counts paused instances beside running ones, only when there are any.
func TestHeaderCountsPausedInstances(t *testing.T) {
	v := core.View{Root: "/r", Instances: []core.InstanceView{ours("a", groundhog.RunnerRunning, false), ours("b", groundhog.RunnerRunning, true)}}
	m, _ := feed(NewWithData(FromViewWith(v, lookup(nil))), tea.WindowSizeMsg{Width: 100, Height: 30})
	if h := m.header(100); !strings.Contains(h, "2 instances · 1 running · 1 paused") {
		t.Errorf("header %q", h)
	}
	v.Instances = v.Instances[:1]
	m, _ = feed(NewWithData(FromViewWith(v, lookup(nil))), tea.WindowSizeMsg{Width: 100, Height: 30})
	if h := m.header(100); strings.Contains(h, "paused") {
		t.Errorf("header %q mentions paused", h)
	}
}
