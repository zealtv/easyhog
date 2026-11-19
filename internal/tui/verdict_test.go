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

func TestVerdictShowsInTheBadgeAndThePreview(t *testing.T) {
	cases := []struct {
		name    string
		v       core.InstanceView
		badge   string
		preview string
	}{
		{"running here", withTick(ours("a", groundhog.RunnerRunning, false), 12*time.Second, "runner"),
			"will run", "Pending work: yes, this session's runner ticked 12s before the"},
		{"paused", withTick(ours("a", groundhog.RunnerRunning, true), 12*time.Second, "runner"),
			"paused: won't run", "Pending work: no, the instance is paused."},
		{"something else ticked", withTick(unowned("a"), 45*time.Second, "standalone"),
			"probably", "Pending work: probably, something outside Easyhog ticked it 45s before the"},
		{"old tick", withTick(unowned("a"), 25*time.Minute, "standalone"),
			"", "Pending work: nothing seen ticking it; last tick 25m00s before the"},
		{"never recorded", unowned("a"),
			"", "Pending work: nothing seen ticking it; no tick recorded."},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			n := instanceNode(c.v, nil, false)
			if n.Badge != c.badge { // these instances have no items, so the verdict is the whole badge
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

func TestVerdictOutlastsTheItemCountsInANarrowTree(t *testing.T) {
	d := FromView(core.View{Instances: []core.InstanceView{withTick(unowned("alpha"), 45*time.Second, "standalone")}})
	m := NewWithData(d)
	next, _ := m.Update(tea.WindowSizeMsg{Width: 200, Height: 30})
	row := ansi.Strip(next.(Model).treeRow(0, 56))
	if !strings.Contains(row, "probably") || strings.Contains(row, "45s") {
		t.Errorf("row %q", row)
	}
	if strings.Contains(row, "pending") {
		t.Errorf("the counts should go first: %q", row)
	}
}

// An unowned instance whose status is incoherent only for an unfinished journal
// write offers Run, and the palette says Run will settle it.
func TestRunNoteForAnInterruptedJournalWrite(t *testing.T) {
	st := okStatus()
	st.Consistent = false
	st.Diagnostics = []groundhog.Problem{{Code: "journal_write_in_progress"}}
	v := iv("alpha", core.ClassUnowned, core.HealthInconsistent, core.Inspection{Last: obs(st), Attempts: 3})
	if got := actionsOf(t, v, nil); got != "run" {
		t.Fatalf("actions %q", got)
	}
	r := newRig(t, &fakeRunners{})
	r.m = NewWithData(r.data(v), WithRunners(&fakeRunners{}, func(string) {}))
	r.send(tea.WindowSizeMsg{Width: 110, Height: 30})
	r.send(typeText(":")...)
	if s := r.screen(); !strings.Contains(s, "will settle an interrupted journal write") {
		t.Fatalf("no hint beside Run:\n%s", s)
	}
	// Another incoherence keeps today's refusal.
	st.Diagnostics = append(st.Diagnostics, groundhog.Problem{Code: "delivery_record_malformed"})
	v = iv("alpha", core.ClassUnowned, core.HealthInconsistent, core.Inspection{Last: obs(st), Attempts: 3})
	if got := actionsOf(t, v, nil); got != "" {
		t.Errorf("actions %q", got)
	}
}
