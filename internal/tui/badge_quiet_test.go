package tui

import (
	"fmt"
	"testing"

	"strings"

	"github.com/zealtv/easyhog/internal/core"
	"github.com/zealtv/easyhog/internal/groundhog"
)

// A row with nothing to count says nothing about it: "0 items" leaves the
// badge, non-zero counts stay, and the preview keeps its full "Schedule:" line.
func TestTheBadgeLeavesOutAnEmptyItemCount(t *testing.T) {
	withItems := func(v core.InstanceView, n int) core.InstanceView {
		st := v.Inspection.Last.Status
		st.Schedule = nil
		for i := 0; i < n; i++ {
			st.Schedule = append(st.Schedule, groundhog.ScheduleItem{Path: fmt.Sprintf("schedule/daily/x%d", i), ItemID: fmt.Sprintf("x%d", i)})
		}
		v.Inspection.Last = obs(st)
		return v
	}
	failed := groundhog.Delivery{Path: "out/a", State: groundhog.DeliveryFailed} // no schedule path: adds no item
	running := withTick(ours("a", groundhog.RunnerRunning, false), 0, "runner")
	cases := []struct {
		name    string
		v       core.InstanceView
		badge   string
		preview string
	}{
		{"none, nothing seen ticking it", withItems(unowned("a"), 0), "", "Schedule: 0 items"},
		{"one, nothing seen ticking it", withItems(unowned("a"), 1), "1 item", "Schedule: 1 item"},
		{"none, will run", withItems(running, 0), "will run", "Schedule: 0 items"},
		{"one", withItems(running, 1), "will run · 1 item", "Schedule: 1 item"},
		{"two", withItems(running, 2), "will run · 2 items", "Schedule: 2 items"},
		{"none, but a failure", withDeliveries(withItems(running, 0), failed), "will run · 1 failed", "Schedule: 0 items"},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			n := instanceNode(c.v, nil, false)
			if n.Badge != c.badge {
				t.Errorf("badge %q, want %q", n.Badge, c.badge)
			}
			if !strings.Contains(n.Preview, c.preview) {
				t.Errorf("preview lacks %q:\n%s", c.preview, n.Preview)
			}
		})
	}
}
