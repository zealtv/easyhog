package tui

import (
	"reflect"
	"strings"
	"testing"

	"github.com/charmbracelet/x/ansi"
	"github.com/zealtv/easyhog/internal/core"
	"github.com/zealtv/easyhog/internal/discovery"
	"github.com/zealtv/easyhog/internal/groundhog"

	tea "github.com/charmbracelet/bubbletea"
)

// An item row once showed its whole path, which said the cadence and the date
// again beside the badge. The row names the item once.
func TestItemLabelsNameTheItemByItsFolder(t *testing.T) {
	for _, c := range []struct {
		name string
		rels []string
		want []string
	}{
		{"a daily item", []string{"daily/09/report"}, []string{"report"}},
		{"a once item with date and time folders", []string{"once/2026-11-18/19-45/nightly-review-wake"}, []string{"nightly-review-wake"}},
		{"distinct names stay bare", []string{"daily/a", "weekly/mon/b", "every/15m/c"}, []string{"a", "b", "c"}},
		{"one parent tells two apart", []string{"daily/report", "weekly/mon/report"}, []string{"daily/report", "mon/report"}},
		{"only the clashing rows grow", []string{"daily/report", "weekly/mon/report", "daily/other"}, []string{"daily/report", "mon/report", "other"}},
		{"the same parent needs two levels", []string{"daily/09/report", "weekly/mon/09/report"}, []string{"daily/09/report", "mon/09/report"}},
		{"a path that ends another's", []string{"a/x", "b/a/x"}, []string{"a/x", "b/a/x"}},
		{"three that clash", []string{"daily/r", "weekly/r", "monthly/r"}, []string{"daily/r", "weekly/r", "monthly/r"}},
		{"a paused folder keeps its own name", []string{"daily/held.paused"}, []string{"held.paused"}},
	} {
		if got := itemLabels(c.rels); !reflect.DeepEqual(got, c.want) {
			t.Errorf("%s: %q, want %q", c.name, got, c.want)
		}
	}
}

func itemRows(t *testing.T, v core.InstanceView) []Node {
	t.Helper()
	return FromViewWith(core.View{Instances: []core.InstanceView{v}}, lookup(nil)).Nodes[0].Children
}

func TestAnItemRowSaysTheNameOnceAndTheBadgeKeepsTheTiming(t *testing.T) {
	const path = "schedule/once/2026-11-18/19-45/nightly-review-wake"
	v := ours("a", groundhog.RunnerRunning, false)
	st := v.Inspection.Last.Status
	st.Schedule = []groundhog.ScheduleItem{{Path: path, ItemID: "nightly-review-wake",
		Cadence: &groundhog.Cadence{Axis: "once", Date: sp("2026-11-18"), Time: sp("19:45")}}}
	v.Inspection.Last = obs(st)

	kids := itemRows(t, v)
	if len(kids) != 1 {
		t.Fatalf("%d items", len(kids))
	}
	k := kids[0]
	if k.Label != "nightly-review-wake" {
		t.Errorf("label %q", k.Label)
	}
	if k.Key != v.Instance.Path+"#"+path {
		t.Errorf("the row key changed: %q", k.Key)
	}
	if !strings.Contains(k.Preview, "Item "+path+"\n") {
		t.Errorf("the preview lost the full path:\n%s", k.Preview)
	}
	if !strings.HasPrefix(k.Badge, "once") {
		t.Errorf("badge %q lost the cadence", k.Badge)
	}

	m, _ := feed(NewWithData(FromViewWith(core.View{Instances: []core.InstanceView{v}}, lookup(nil))), tea.WindowSizeMsg{Width: 100, Height: 24},
		key(tea.KeyRight))
	row := ansi.Strip(m.treeRow(1, 100))
	if !strings.Contains(row, "nightly-review-wake") || strings.Contains(row, "2026-11-18/") || strings.Contains(row, "once/") {
		t.Errorf("row %q", row)
	}
}

func TestItemsWithOneNameStayApartInTheTree(t *testing.T) {
	v := ours("a", groundhog.RunnerRunning, false)
	st := v.Inspection.Last.Status
	st.Schedule = []groundhog.ScheduleItem{
		{Path: "schedule/daily/report", ItemID: "report"},
		{Path: "schedule/weekly/mon/report", ItemID: "report"},
		{Path: "schedule/daily/other", ItemID: "other"},
	}
	v.Inspection.Last = obs(st)
	var got []string
	keys := map[string]bool{}
	for _, k := range itemRows(t, v) {
		got = append(got, k.Label)
		keys[k.Key] = true
	}
	if want := []string{"daily/report", "mon/report", "other"}; !reflect.DeepEqual(got, want) {
		t.Errorf("labels %q, want %q", got, want)
	}
	if len(keys) != 3 {
		t.Errorf("keys collide: %v", keys)
	}
}

func TestListedOnlyItemRowsNameTheItemOnceToo(t *testing.T) {
	v := iv("a", core.ClassNotInspected, core.HealthOK, core.Inspection{})
	v.Files = discovery.Glance{Schedule: []discovery.ScheduleDir{
		{Path: "once", Leaf: false},
		{Path: "once/2026-11-18/19-45/wake", Leaf: true},
		{Path: "daily/report", Leaf: true},
		{Path: "weekly/mon/report", Leaf: true},
	}}
	var got []string
	for _, k := range itemRows(t, v) {
		got = append(got, k.Label)
		if !strings.Contains(k.Key, "#schedule/") || !strings.Contains(k.Preview, "Item schedule/") {
			t.Errorf("the key or preview lost the full path: %q %q", k.Key, k.Preview)
		}
	}
	if want := []string{"wake", "daily/report", "mon/report"}; !reflect.DeepEqual(got, want) {
		t.Errorf("labels %q, want %q", got, want)
	}
}
