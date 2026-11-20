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
	return groundhog.Status{ProtocolVersion: groundhog.ProtocolVersion, ObservedAt: at}
}

func iv(name string, class core.Class, health core.Health, in core.Inspection) core.InstanceView {
	return core.InstanceView{Instance: discovery.Instance{Path: "/r/" + name + "/.groundhog", Display: name + "/.groundhog"},
		Class: class, Health: health, Inspection: in}
}

func TestFromViewDistinguishesEveryKindOfInstance(t *testing.T) {
	good := obs(okStatus())
	outside := okStatus()
	outside.LastTick = &groundhog.LastTick{At: at.Add(-20 * time.Second), Source: "standalone"}
	malformed := iv("malformed", core.ClassMalformed, core.HealthOK, core.Inspection{})
	malformed.Instance.Malformed = []string{"groundhog.sh is missing"}
	view := core.View{Root: "/r", Instances: []core.InstanceView{
		iv("fresh", core.ClassNotInspected, core.HealthOK, core.Inspection{}),
		iv("supported", core.ClassSupported, core.HealthOK, core.Inspection{Last: good}),
		iv("external", core.ClassSupported, core.HealthOK, core.Inspection{Last: obs(outside)}),
		iv("legacy", core.ClassLegacy, core.HealthOK, core.Inspection{Support: &groundhog.Support{Reason: "lacks required capabilities: x"}}),
		iv("partial", core.ClassUnsupported, core.HealthOK, core.Inspection{Last: good}),
		iv("failing", core.ClassSupported, core.HealthOK, core.Inspection{Last: good, Err: errors.New("deadline"), ErrAt: at}),
		iv("neverread", core.ClassFailing, core.HealthOK, core.Inspection{Err: errors.New("exit 6"), ErrAt: at}),
		iv("stale", core.ClassSupported, core.HealthStale, core.Inspection{Last: good}),
		malformed,
	}}
	d := FromView(view)
	want := map[string]struct {
		state State
		in    string
	}{
		"fresh":     {StateStopped, "not inspected"},
		"supported": {StateStopped, "not ticked, inspected 09:30:15"},
		"external":  {StateExternal, "ticked from outside Easyhog"},
		"legacy":    {StateUnsupported, "legacy"},
		"partial":   {StateUnsupported, "read-only"},
		"failing":   {StateFailed, "last good 09:30:15"},
		"neverread": {StateFailed, "failed"},
		"stale":     {StateStopped, "stale"},
		"malformed": {StateUnsupported, "not run"},
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
	if p := byLabel("fresh").Preview; !strings.Contains(p, "nothing was run") {
		t.Errorf("fresh preview:\n%s", p)
	}
	if p := byLabel("external").Preview; !strings.Contains(p, "Pending work: probably, something outside Easyhog ticked it 20s before") {
		t.Errorf("external preview:\n%s", p)
	}
}

func TestFromViewKeepsItemKeysWhenStatusArrivesAfterTheListing(t *testing.T) {
	listed := iv("a", core.ClassNotInspected, core.HealthOK, core.Inspection{})
	listed.Files = discovery.Glance{Schedule: []discovery.ScheduleDir{{Path: "daily", Leaf: false}, {Path: "daily/x", Leaf: true}}}
	st := okStatus()
	st.Schedule = []groundhog.ScheduleItem{{Path: "schedule/daily/x", ItemID: "x"}}
	read := iv("a", core.ClassSupported, core.HealthOK, core.Inspection{Last: obs(st)})
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

// The instance pause outranks who ticks it: a paused instance reads paused,
// ticked here or not, and says whether this session still ticks it.
func TestFromViewShowsAPausedInstanceAsPaused(t *testing.T) {
	cases := []struct {
		name  string
		v     core.InstanceView
		look  TickLookup
		state State
		word  string
	}{
		{"ticked here", supported("a"), ticks("/r/a/.groundhog"), StateRunning, "inspected 09:30:15"},
		{"ticked here, paused", pausedInstance("b"), ticks("/r/b/.groundhog"), StatePaused, "nothing fires or starts; this session still ticks it"},
		{"not ticked here, paused", pausedInstance("c"), ticks(), StatePaused, "nothing fires or starts, inspected"},
		{"not ticked here", supported("d"), ticks(), StateStopped, "not ticked"},
	}
	for _, c := range cases {
		n := FromViewWith(core.View{Instances: []core.InstanceView{c.v}}, c.look).Nodes[0]
		if n.State != c.state || !strings.Contains(n.Detail, c.word) {
			t.Errorf("%s: state %v detail %q, want %v containing %q", c.name, n.State, n.Detail, c.state, c.word)
		}
	}
	if StatePaused.label() != "paused" || StatePaused.glyph() == StateRunning.glyph() || StatePaused.glyph() == StatePending.glyph() {
		t.Errorf("paused needs its own word and glyph: %q %q", StatePaused.label(), StatePaused.glyph())
	}
	if p := FromViewWith(core.View{Instances: []core.InstanceView{pausedInstance("p")}}, ticks()).Nodes[0].Preview; !strings.Contains(p, "Pause: the instance is paused") {
		t.Errorf("preview:\n%s", p)
	}
}

// The header counts paused instances beside running ones, only when there are any.
func TestHeaderCountsPausedInstances(t *testing.T) {
	v := core.View{Root: "/r", Instances: []core.InstanceView{supported("a"), pausedInstance("b")}}
	look := ticks("/r/a/.groundhog", "/r/b/.groundhog")
	m, _ := feed(NewWithData(FromViewWith(v, look)), tea.WindowSizeMsg{Width: 100, Height: 30})
	if h := m.header(100); !strings.Contains(h, "2 instances · 1 running · 1 paused") {
		t.Errorf("header %q", h)
	}
	v.Instances = v.Instances[:1]
	m, _ = feed(NewWithData(FromViewWith(v, look)), tea.WindowSizeMsg{Width: 100, Height: 30})
	if h := m.header(100); strings.Contains(h, "paused") {
		t.Errorf("header %q mentions paused", h)
	}
}
