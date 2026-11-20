package tui

import (
	"strings"
	"testing"
	"time"

	tea "github.com/charmbracelet/bubbletea"
	"github.com/charmbracelet/x/ansi"

	"github.com/zealtv/easyhog/internal/core"
	"github.com/zealtv/easyhog/internal/groundhog"
)

// withTick gives an instance's status a last tick the given age before the read.
func withTick(v core.InstanceView, age time.Duration, source string) core.InstanceView {
	st := v.Inspection.Last.Status
	st.ObservedAt = at
	st.LastTick = &groundhog.LastTick{At: at.Add(-age), Source: source}
	v.Inspection.Last = obs(st)
	return v
}

// The row's badge leaves the verdict out (the state word and its glyph say
// it; Bob, 2026-11-19) and the preview keeps it whole, with its reasons.
func TestVerdictStaysOutOfTheBadgeAndInThePreview(t *testing.T) {
	cases := []struct {
		name    string
		v       core.InstanceView
		info    *core.TickInfo
		badge   string
		preview string
	}{
		{"ticked here", withTick(supported("a"), 12*time.Second, "standalone"), here(0),
			"", "Pending work: yes, this session ticks it; last tick 12s before the"},
		{"paused", withTick(pausedInstance("a"), 12*time.Second, "standalone"), here(0),
			"", "Pending work: no, the instance is paused."},
		{"something else ticked", withTick(supported("a"), 45*time.Second, "standalone"), nil,
			"", "Pending work: probably, something outside Easyhog ticked it 45s before the"},
		{"old tick", withTick(supported("a"), 25*time.Minute, "standalone"), nil,
			"", "Pending work: nothing seen ticking it; last tick 25m00s before the"},
		{"never recorded", supported("a"), nil,
			"", "Pending work: nothing seen ticking it; no tick recorded."},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			n := instanceNode(c.v, c.info, false)
			if n.Badge != c.badge { // these instances have no items or deliveries, so nothing else is left in the badge
				t.Errorf("badge %q, want %q", n.Badge, c.badge)
			}
			if !strings.Contains(n.Preview, c.preview) {
				t.Errorf("preview lacks %q:\n%s", c.preview, n.Preview)
			}
		})
	}
}

func TestNoVerdictWithoutAStatus(t *testing.T) {
	n := instanceNode(iv("fresh", core.ClassNotInspected, core.HealthOK, core.Inspection{}), nil, false)
	if strings.Contains(n.Preview, "Pending work") || strings.Contains(n.Badge, "tick") {
		t.Errorf("an uninspected instance has no verdict: %q\n%s", n.Badge, n.Preview)
	}
}

// The counts give way before the state word in a narrow tree.
func TestCountsGiveWayInANarrowTree(t *testing.T) {
	v := withTick(withDeliveries(supported("alpha"), delivery("out/f", groundhog.DeliveryFailed, ""), delivery("out/p", groundhog.DeliveryPending, "")), 45*time.Second, "standalone")
	m := NewWithData(FromView(core.View{Instances: []core.InstanceView{v}}))
	next, _ := m.Update(tea.WindowSizeMsg{Width: 200, Height: 30})
	if row := ansi.Strip(next.(Model).treeRow(0, 80)); !strings.Contains(row, "1 pending · 1 failed") {
		t.Errorf("wide row %q", row)
	}
	row := ansi.Strip(next.(Model).treeRow(0, 30))
	if !strings.Contains(row, "failed") || strings.Contains(row, "pending") || strings.Contains(row, "45s") {
		t.Errorf("narrow row %q", row)
	}
}

// The script reports UTC; the verdict line gives the read's time in the zone
// the preview's other times use, so it sits beside "inspected" in one clock.
func TestVerdictTimeIsInThePreviewsZone(t *testing.T) {
	zone := time.FixedZone("UTC+11", 11*3600)
	v := withTick(supported("a"), 12*time.Second, "standalone")
	v.Inspection.Last.TakenAt = at.In(zone)
	n := instanceNode(v, nil, false)
	if !strings.Contains(n.Preview, "12s before the 20:30:15 read") || !strings.Contains(n.Detail, "inspected 20:30:15") {
		t.Errorf("detail %q\n%s", n.Detail, n.Preview)
	}
}
