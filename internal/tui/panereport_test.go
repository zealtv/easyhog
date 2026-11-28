package tui

import (
	"errors"
	"strconv"
	"strings"
	"sync"
	"testing"
	"time"

	tea "github.com/charmbracelet/bubbletea"

	"github.com/zealtv/easyhog/internal/core"
	"github.com/zealtv/easyhog/internal/groundhog"
	"github.com/zealtv/easyhog/internal/twpane"
)

func dueItem(path string, due time.Time) groundhog.ScheduleItem {
	it := plainItem(path)
	it.NextDue = &due
	return it
}

func failedDelivery(item string) groundhog.Delivery {
	sp := item
	return groundhog.Delivery{Path: "out/" + item[strings.LastIndex(item, "/")+1:] + "-1", State: groundhog.DeliveryFailed, SchedulePath: &sp}
}

// The next wake is the soonest next_due among the unpaused items of the
// unpaused instances this session ticks; instances ticked from outside, or not
// at all, say nothing.
func TestPaneReportNextIsTheSoonestWakeThisSessionTicks(t *testing.T) {
	t0 := time.Date(2026, 11, 27, 10, 0, 0, 0, time.Local)
	paused := ownPause(dueItem("schedule/daily/08/early", t0.Add(time.Minute)))
	paused.NextDue = ptrTime(t0.Add(time.Minute))
	a := withItems(supported("a"), dueItem("schedule/daily/11/later", t0.Add(time.Hour)), dueItem("schedule/daily/10/soon", t0.Add(30*time.Minute)), paused)
	b := withItems(supported("b"), dueItem("schedule/daily/10/sooner", t0.Add(5*time.Minute))) // not ticked here
	c := withItems(pausedInstance("c"), dueItem("schedule/daily/10/held", t0.Add(time.Minute)))
	v := core.View{Instances: []core.InstanceView{a, b, c}}
	r := paneReport(v, ticks("/r/a/.groundhog", "/r/c/.groundhog"))
	if r.Next == nil || r.Next.Item != "soon" || r.Next.Instance != "a" || !r.Next.Due.Equal(t0.Add(30*time.Minute)) {
		t.Fatalf("next = %+v", r.Next)
	}
	if got := nextLine(r.Next, t0); got != "next: soon at 10:30 · a" {
		t.Errorf("line = %q", got)
	}
	if got := nextLine(r.Next, t0.Add(31*time.Minute)); got != "next: soon due · a" {
		t.Errorf("line once due = %q", got)
	}
	if got := nextLine(&PaneWake{Item: "weekly", Instance: "a", Due: t0.Add(72 * time.Hour)}, t0); got != "next: weekly at Mon 10:00 · a" {
		t.Errorf("line on another day = %q", got)
	}
	if r := paneReport(v, ticks()); r.Next != nil || r.Fail != "" {
		t.Errorf("nothing ticked here, report = %+v", r)
	}
	if r := paneReport(v, nil); r.Next != nil || r.Fail != "" {
		t.Errorf("no ticker, report = %+v", r)
	}
	if nextLine(nil, t0) != "" {
		t.Error("no wake should unset the line")
	}
}

func ptrTime(t time.Time) *time.Time { return &t }

// A failed wake is a failed tick this session ran, or a delivery the tree
// counts failed, in an instance this session ticks.
func TestPaneReportFailNamesTheFailedWake(t *testing.T) {
	one := withDeliveries(supported("a"), failedDelivery("schedule/daily/09/report"))
	two := withDeliveries(supported("b"), failedDelivery("schedule/daily/09/x"), failedDelivery("schedule/daily/09/y"))
	outside := withDeliveries(supported("c"), failedDelivery("schedule/daily/09/z"))
	cases := []struct {
		name string
		v    []core.InstanceView
		look TickLookup
		want string
	}{
		{"one delivery", []core.InstanceView{one}, ticks("/r/a/.groundhog"), "a: report failed"},
		{"two deliveries", []core.InstanceView{two}, ticks("/r/b/.groundhog"), "b: 2 wakes failed"},
		{"two instances", []core.InstanceView{one, two}, ticks("/r/a/.groundhog", "/r/b/.groundhog"), "a: report failed (+1 more)"},
		{"ticked from outside", []core.InstanceView{outside}, ticks(), ""},
		{"tick failed", []core.InstanceView{supported("d")}, func(key string) (core.TickInfo, bool) {
			return core.TickInfo{Ticking: true, LastErr: errors.New("exit 1")}, key == "/r/d/.groundhog"
		}, "d: tick failed"},
	}
	for _, c := range cases {
		if got := paneReport(core.View{Instances: c.v}, c.look).Fail; got != c.want {
			t.Errorf("%s: fail = %q, want %q", c.name, got, c.want)
		}
	}
}

// paneCalls records the tmux calls a Pane makes.
type paneCalls struct {
	mu    sync.Mutex
	calls []string
}

func (p *paneCalls) run(args ...string) error {
	p.mu.Lock()
	defer p.mu.Unlock()
	p.calls = append(p.calls, strings.Join(args, " "))
	return nil
}

func (p *paneCalls) all() string {
	p.mu.Lock()
	defer p.mu.Unlock()
	return strings.Join(p.calls, "\n")
}

// runAll runs cmd and every command a batch holds, as Bubble Tea would.
func runAll(cmd tea.Cmd) {
	if cmd == nil {
		return
	}
	if b, ok := cmd().(tea.BatchMsg); ok {
		for _, c := range b {
			runAll(c)
		}
	}
}

// The model writes @tw-idle and the failure to its own pane only when they
// change, keeps the failure's start time across snapshots, and clears them when
// they clear.
func TestModelReportsNextAndFailOnItsOwnPane(t *testing.T) {
	t0 := time.Date(2026, 11, 27, 10, 0, 0, 0, time.Local)
	now := t0
	m := NewWithData(Data{}, WithClock(func() time.Time { return now }), WithoutClockTicks())
	rec := &paneCalls{}
	m.pane = twpane.New("%7")
	m.pane.Run = rec.run

	good := withItems(supported("a"), dueItem("schedule/daily/10/soon", t0.Add(30*time.Minute)))
	look := ticks("/r/a/.groundhog")
	m, cmd := feed(m, SnapshotMsg{Data: FromViewWith(core.View{Instances: []core.InstanceView{good}}, look)})
	runAll(cmd)
	if got := rec.all(); !strings.Contains(got, "set-option -p -t %7 @tw-idle next: soon at 10:30 · a") || !strings.Contains(got, "set-option -pu -t %7 @tw_status") {
		t.Fatalf("first write = %q", got)
	}
	rec.calls = nil
	m, cmd = feed(m, SnapshotMsg{Data: FromViewWith(core.View{Instances: []core.InstanceView{good}}, look)})
	runAll(cmd)
	if got := rec.all(); got != "" {
		t.Errorf("an unchanged snapshot wrote %q", got)
	}

	bad := withDeliveries(good, failedDelivery("schedule/daily/10/soon"))
	m, cmd = feed(m, SnapshotMsg{Data: FromViewWith(core.View{Instances: []core.InstanceView{bad}}, look)})
	runAll(cmd)
	got := rec.all()
	if !strings.Contains(got, "@tw_status fail") || !strings.Contains(got, "@tw_status_text a: soon failed") || !strings.Contains(got, "@tw_status_at "+strconv.FormatInt(t0.Unix(), 10)) {
		t.Fatalf("failure write = %q", got)
	}
	rec.calls = nil
	now = t0.Add(time.Minute)
	m, cmd = feed(m, SnapshotMsg{Data: FromViewWith(core.View{Instances: []core.InstanceView{bad}}, look)})
	runAll(cmd)
	if got := rec.all(); got != "" {
		t.Errorf("the same failure a minute later wrote %q", got)
	}

	m, cmd = feed(m, SnapshotMsg{Data: FromViewWith(core.View{Instances: []core.InstanceView{good}}, ticks())})
	runAll(cmd)
	got = rec.all()
	for _, opt := range []string{"@tw-idle", "@tw_status", "@tw_status_text", "@tw_status_at"} {
		if !strings.Contains(got, "set-option -pu -t %7 "+opt) {
			t.Errorf("stopping ticking did not unset %s: %q", opt, got)
		}
	}
	rec.calls = nil
	if err := m.pane.Clear(); err != nil {
		t.Fatal(err)
	}
	if got := rec.all(); !strings.Contains(got, "set-option -pu -t %7 @tw-idle") {
		t.Errorf("clear = %q", got)
	}
}

// Outside tmux, or before RunLive gives it a pane, the model writes nothing.
func TestModelWithoutAPaneWritesNothing(t *testing.T) {
	m := NewWithData(Data{}, WithoutClockTicks())
	good := withItems(supported("a"), dueItem("schedule/daily/10/soon", time.Now().Add(time.Hour)))
	_, cmd := feed(m, SnapshotMsg{Data: FromViewWith(core.View{Instances: []core.InstanceView{good}}, ticks("/r/a/.groundhog"))})
	if m.pane.Sync(twpane.Values{twpane.Idle: "x"}) != nil {
		t.Error("a zero pane made a command")
	}
	runAll(cmd)
}
