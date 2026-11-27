package tui

import (
	"errors"
	"strings"
	"testing"
	"time"

	"github.com/zealtv/easyhog/internal/core"
	"github.com/zealtv/easyhog/internal/twglance"
)

func glanceValues(v core.View, look TickLookup) (far, mid string) {
	g := FromViewWith(v, look).Glance
	return twglance.FarValue(g.Badges), twglance.MidValue(g.Rows)
}

// The list holds the ticked instances only, those ticked here first, each in
// tree order; a failed or paused one takes the tree's glyph and the map's role
// for it. The badges count who ticks them, and the failed, leaving zeros out.
func TestGlanceListsTheTickedInstancesOnly(t *testing.T) {
	outside := func(name string) core.InstanceView { return withTick(supported(name), 20*time.Second, "runner") }
	pausedOut := outside("held")
	pausedOut.Inspection.Last.Status.Paused = true
	v := core.View{Root: "/r", Instances: []core.InstanceView{
		outside("ai-kit"),
		supported("idle"), // ticked by nobody
		supported("here"),
		withDeliveries(supported("broken"), failedDelivery("schedule/daily/09/report")),
		pausedOut,
		withTick(supported("old"), core.TickFreshness+time.Second, "runner"), // a stale outside tick
		{Instance: supported("unread").Instance, Class: core.ClassNotInspected},
	}}
	far, mid := glanceValues(v, ticks("/r/here/.groundhog", "/r/broken/.groundhog"))
	if want := ":hog;ok:●2;other:◇2;bad:✗1"; far != want {
		t.Errorf("far = %q, want %q", far, want)
	}
	if want := "ok:● here;bad:✗ broken;other:◇ ai-kit;hold:◐ held"; mid != want {
		t.Errorf("mid = %q, want %q", mid, want)
	}
}

// With nothing ticked the badge line is the label alone and the list unset.
func TestGlanceWithNothingTickedIsTheLabelAlone(t *testing.T) {
	far, mid := glanceValues(core.View{Root: "/r", Instances: []core.InstanceView{supported("a")}}, ticks())
	if far != ":hog" || mid != "" {
		t.Errorf("far, mid = %q, %q", far, mid)
	}
	if far, _ := glanceValues(core.View{Root: "/r"}, nil); far != ":hog" {
		t.Errorf("no instances: far = %q", far)
	}
}

// An instance ticked here that has not been read yet still counts as ticked
// here: this session knows it ticks it.
func TestGlanceCountsAnUnreadInstanceTickedHere(t *testing.T) {
	v := core.View{Root: "/r", Instances: []core.InstanceView{{Instance: supported("a").Instance, Class: core.ClassNotInspected}}}
	far, mid := glanceValues(v, ticks("/r/a/.groundhog"))
	if far != ":hog;ok:●1" || mid != "ok:● a" {
		t.Errorf("far, mid = %q, %q", far, mid)
	}
}

// The model writes the glance to its own pane when it changes, and Clear unsets
// it.
func TestModelWritesItsGlanceOnItsOwnPane(t *testing.T) {
	m := NewWithData(Data{}, WithoutClockTicks())
	rec := &paneCalls{}
	m.glance = twglance.New("%7")
	m.glance.Run = rec.run
	v := core.View{Root: "/r", Instances: []core.InstanceView{supported("a")}}
	m, cmd := feed(m, SnapshotMsg{Data: FromViewWith(v, ticks("/r/a/.groundhog"))})
	runAll(cmd)
	if got := rec.all(); !strings.Contains(got, "set-option -p -t %7 @tw-glance-far :hog;ok:●1") || !strings.Contains(got, "set-option -p -t %7 @tw-glance-mid ok:● a") {
		t.Fatalf("first write = %q", got)
	}
	rec.calls = nil
	m, cmd = feed(m, SnapshotMsg{Data: FromViewWith(v, ticks("/r/a/.groundhog"))})
	runAll(cmd)
	if got := rec.all(); got != "" {
		t.Errorf("an unchanged snapshot wrote %q", got)
	}
	m, cmd = feed(m, SnapshotMsg{Data: FromViewWith(v, ticks())})
	runAll(cmd)
	if got := rec.all(); !strings.Contains(got, "@tw-glance-far :hog") || !strings.Contains(got, "set-option -pu -t %7 @tw-glance-mid") {
		t.Errorf("stopping ticking wrote %q", got)
	}
	rec.calls = nil
	if err := m.glance.Clear(); err != nil {
		t.Fatal(err)
	}
	if got := rec.all(); !strings.Contains(got, "-pu -t %7 @tw-glance-far") || !strings.Contains(got, "-pu -t %7 @tw-glance-mid") {
		t.Errorf("clear = %q", got)
	}
}

// Outside tmux the model has no glance pane and writes nothing.
func TestModelWithoutAGlancePaneWritesNothing(t *testing.T) {
	m := NewWithData(Data{}, WithoutClockTicks())
	if m.glance.Sync(twglance.Glance{Badges: []twglance.Entry{{Text: "hog"}}}) != nil {
		t.Error("a zero glance pane made a command")
	}
}

// The edge states: an unsupported instance never counts, whatever ticked it;
// a failed tick here reads failed, as @tw_status does; a failing inspection
// reads failed; paused while ticked here holds; and this session's own fresh
// tick after Stop is not an outside one.
func TestGlanceEdgeStates(t *testing.T) {
	unsupported := withTick(supported("odd"), 10*time.Second, "runner")
	unsupported.Class = core.ClassUnsupported
	failing := withTick(supported("sick"), 10*time.Second, "runner")
	failing.Inspection.Err = errors.New("exit 2")
	ownTick := at.Add(-5 * time.Second)
	cases := []struct {
		name     string
		v        core.InstanceView
		look     TickLookup
		far, mid string
	}{
		{"unsupported, ticked from outside", unsupported, ticks(), ":hog", ""},
		{"a failed tick here", supported("a"), func(key string) (core.TickInfo, bool) {
			return core.TickInfo{Ticking: true, LastErr: errors.New("exit 1")}, key == "/r/a/.groundhog"
		}, ":hog;ok:●1;bad:✗1", "bad:✗ a"},
		{"inspection failing, ticked from outside", failing, ticks(), ":hog;other:◇1;bad:✗1", "bad:✗ sick"},
		{"paused, ticked here", pausedInstance("p"), ticks("/r/p/.groundhog"), ":hog;ok:●1", "hold:◐ p"},
		{"stopped here, its own tick fresh", withTick(supported("a"), 5*time.Second, "standalone"), func(string) (core.TickInfo, bool) {
			return core.TickInfo{Interval: time.Minute, LastBegan: ownTick, LastEnded: ownTick}, true
		}, ":hog", ""},
	}
	for _, c := range cases {
		far, mid := glanceValues(core.View{Root: "/r", Instances: []core.InstanceView{c.v}}, c.look)
		if far != c.far || mid != c.mid {
			t.Errorf("%s: far, mid = %q, %q; want %q, %q", c.name, far, mid, c.far, c.mid)
		}
	}
}
