package tui

import (
	"fmt"
	"strings"
	"testing"
	"time"

	tea "github.com/charmbracelet/bubbletea"
	"github.com/charmbracelet/x/ansi"

	"github.com/zealtv/easyhog/internal/groundhog"
)

func tp(t time.Time) *time.Time { return &t }

// The one wording function, as a table: the words lose precision as the time
// grows. Small units round up, large ones to the nearest.
func TestCountdownWording(t *testing.T) {
	const (
		s = time.Second
		m = time.Minute
		h = time.Hour
		d = 24 * h
	)
	for _, c := range []struct {
		r    time.Duration
		want string
	}{
		{0, ""},
		{-5 * s, ""},
		{1 * s, "in 10s"},
		{9 * s, "in 10s"},
		{10 * s, "in 10s"},
		{11 * s, "in 20s"},
		{30 * s, "in 30s"},
		{50 * s, "in 50s"},
		{51 * s, "in 1 min"},
		{60 * s, "in 1 min"},
		{61 * s, "in 2 min"},
		{15 * m, "in 15 min"},
		{14*m + 1*s, "in 15 min"},
		{59 * m, "in 59 min"},
		{59*m + 1*s, "in 1 h"},
		{1 * h, "in 1 h"},
		{1*h + 20*m, "in 1 h 20 min"},
		{3*h + 20*m - 1*s, "in 3 h 20 min"},
		{5*h + 59*m, "in 5 h 59 min"},
		{5*h + 59*m + 1*s, "in 6 h"},
		{6 * h, "in 6 h"},
		{6*h + 1*m, "in 7 h"},
		{23 * h, "in 23 h"},
		{23*h + 30*m, "in 24 h"},
		{24 * h, "in 1 day"},
		{35 * h, "in 1 day"},
		{36 * h, "in 2 days"},
		{9 * d, "in 9 days"},
		{7 * d, "in a week"},
		{6*d + 15*h, "in a week"},
		{13 * d, "in 13 days"},
		{13*d + 15*h, "in 2 weeks"},
		{14 * d, "in 2 weeks"},
		{21 * d, "in 3 weeks"},
		{59 * d, "in 8 weeks"},
		{60 * d, "in 2 months"},
		{90 * d, "in 3 months"},
		{400 * d, "in 13 months"},
		{548 * d, "in 2 years"},
		{9 * 365 * d, "in 9 years"},
	} {
		if got := countdownWording(c.r); got != c.want {
			t.Errorf("countdownWording(%v) = %q, want %q", c.r, got, c.want)
		}
	}
}

// What an item's row says, from the script's due state: paused and fired items
// say so, an item with a next_due counts down to it and reads its due word from
// then on, and one without falls back to the cadence it always showed.
func TestItemBadge(t *testing.T) {
	daily := &groundhog.Cadence{Axis: "daily", Time: sp("09:00")}
	due := tp(at.Add(time.Hour))
	for _, c := range []struct {
		name      string
		it        groundhog.ScheduleItem
		word      string
		wantBadge string
		wantDue   bool
	}{
		{"paused", groundhog.ScheduleItem{Cadence: daily, PausedBy: []string{"schedule/daily/x"}}, "paused", "paused", false},
		{"paused, with a stale next_due", groundhog.ScheduleItem{Cadence: daily, PausedBy: []string{"p"}, NextDue: due}, "paused", "paused", false},
		{"fired once", groundhog.ScheduleItem{Cadence: &groundhog.Cadence{Axis: "once"}, Fired: true}, "fired", "fired", false},
		{"fired, recurring, next occurrence known", groundhog.ScheduleItem{Cadence: daily, Fired: true, NextDue: due, BlockedBy: []string{"already_fired"}}, "fired", "due now", true},
		{"next due ahead", groundhog.ScheduleItem{Cadence: daily, NextDue: due, BlockedBy: []string{"not_due"}}, "not due", "due now", true},
		{"next due ahead, something else holds it", groundhog.ScheduleItem{Cadence: daily, NextDue: due, BlockedBy: []string{"not_due", "owned"}}, "not due", "due, held", true},
		{"due now", groundhog.ScheduleItem{Cadence: daily, NextDue: tp(at), EligibleNow: true, FiringAllowed: true}, "due now", "due now", true},
		{"due, held", groundhog.ScheduleItem{Cadence: daily, NextDue: tp(at), EligibleNow: true, BlockedBy: []string{"owned"}}, "due, held", "due, held", true},
		{"no next_due, not due", groundhog.ScheduleItem{Cadence: daily}, "not due", "daily at 09:00", false},
		{"no next_due, due now", groundhog.ScheduleItem{Cadence: daily, EligibleNow: true, FiringAllowed: true}, "due now", "daily at 09:00 · due now", false},
	} {
		t.Run(c.name, func(t *testing.T) {
			badge, got := itemBadge(c.it, c.word)
			if badge != c.wantBadge || (got != nil) != c.wantDue {
				t.Errorf("itemBadge = %q, due %v; want %q, due %v", badge, got, c.wantBadge, c.wantDue)
			}
		})
	}
}

// A model whose clock the test moves, with one instance of items due after the
// given distances, expanded.
func countdownModel(t *testing.T, now *time.Time, calls *int, dues ...time.Duration) Model {
	t.Helper()
	var kids []Node
	for i, d := range dues {
		kids = append(kids, Node{Key: fmt.Sprintf("a#i%d", i), Label: fmt.Sprintf("item%d", i), State: StateScheduled,
			Badge: "due now", Due: tp(now.Add(d))})
	}
	data := Data{Root: "/r", Nodes: []Node{{Key: "a", Label: "a", State: StateStopped, Children: kids}}}
	opts := []Option{WithClock(func() time.Time { return *now })}
	if calls != nil {
		opts = append(opts, WithActions(func(ActionRequest) tea.Cmd { *calls++; return nil }), WithSelectionObserver(func(string) { *calls++ }),
			WithRefresh(func() { *calls++ }), WithPayloads(func(PayloadRef) string { *calls++; return "" }))
	}
	m := NewWithData(data, opts...)
	m, _ = feed(m, tea.WindowSizeMsg{Width: 80, Height: 24}, key(tea.KeyRight))
	if len(m.rows) != 1+len(dues) {
		t.Fatalf("rows = %d", len(m.rows))
	}
	return m
}

func itemRow(m Model, i int) string { return ansi.Strip(m.treeRow(1+i, 80)) }

// The countdown is worked out when the row is drawn, against the model's own
// clock: it moves with the clock, and at the due time the row reads its due word.
func TestTheRowCountsDownAgainstTheClock(t *testing.T) {
	now := at
	m := countdownModel(t, &now, nil, 30*time.Second, 15*time.Minute, 9*24*time.Hour)
	for _, want := range []string{"in 30s", "in 15 min", "in 9 days"} {
		if row := itemRow(m, indexOf(want, []string{"in 30s", "in 15 min", "in 9 days"})); !strings.Contains(row, want) || !strings.Contains(row, "scheduled") {
			t.Errorf("row %q lacks %q and the state word", row, want)
		}
	}
	now = at.Add(20 * time.Second)
	m, _ = feed(m, clockMsg{every: m.clockEvery})
	if row := itemRow(m, 0); !strings.Contains(row, "in 10s") {
		t.Errorf("20 s on: row %q", row)
	}
	now = at.Add(30 * time.Second)
	m, _ = feed(m, clockMsg{every: m.clockEvery})
	if row := itemRow(m, 0); !strings.Contains(row, "due now") || strings.Contains(row, "in ") {
		t.Errorf("at the due time: row %q", row)
	}
	if row := itemRow(m, 1); !strings.Contains(row, "in 15 min") {
		t.Errorf("the later item moved too soon: row %q", row)
	}
}

func indexOf(s string, in []string) int {
	for i, v := range in {
		if v == s {
			return i
		}
	}
	return -1
}

// A tick is a bare timer: if running its command returned at once it would be
// a batch or a message, and could be carrying more than the next tick.
func isBareTimer(cmd tea.Cmd) bool {
	if cmd == nil {
		return false
	}
	done := make(chan struct{})
	go func() { cmd(); close(done) }()
	select {
	case <-done:
		return false
	case <-time.After(30 * time.Millisecond):
		return true // still waiting for its time; the goroutine ends with the test binary
	}
}

// The clock only moves "now" and re-arms itself: it never reads a status, runs a
// script, reports a selection, refreshes or loads a payload.
func TestTheClockTickReadsNothing(t *testing.T) {
	now := at
	calls := 0
	m := countdownModel(t, &now, &calls, 30*time.Second)
	sel, engaged, payload := m.selectedKey(), m.engaged, m.payload
	for i := 0; i < 3; i++ {
		now = now.Add(time.Second)
		var cmd tea.Cmd
		m, cmd = feed(m, clockMsg{every: m.clockEvery})
		if !isBareTimer(cmd) {
			t.Fatal("a tick must return exactly the next tick, and nothing else")
		}
	}
	if calls != 0 {
		t.Errorf("a clock tick reached the handler, the observer, a refresh or a payload read %d times", calls)
	}
	if m.selectedKey() != sel || m.engaged != engaged || m.payload != payload {
		t.Errorf("a clock tick changed the selection or the payload state")
	}
}

// One second while a visible row is within two minutes of due, a minute
// otherwise; a tick from a replaced chain does nothing.
func TestTheClockTicksOnceASecondOnlyWhileSecondsShow(t *testing.T) {
	now := at
	m := countdownModel(t, &now, nil, 3*time.Hour)
	if m.clockEvery != time.Minute {
		t.Fatalf("nothing near due: clock every %v", m.clockEvery)
	}
	if m.Init() == nil {
		t.Fatal("no tick started")
	}
	// Time passes to 90 s before due: the next minute tick moves to seconds.
	now = at.Add(3*time.Hour - 90*time.Second)
	m, cmd := feed(m, clockMsg{every: time.Minute})
	if m.clockEvery != time.Second || cmd == nil {
		t.Errorf("90 s before due: clock every %v, cmd %v", m.clockEvery, cmd != nil)
	}
	// A minute tick that was still pending from the old chain is ignored.
	if _, cmd := feed(m, clockMsg{every: time.Minute}); cmd != nil {
		t.Error("a tick from a replaced chain re-armed the clock")
	}
	// After it is due the row shows no seconds, so the next tick goes back to a minute.
	now = at.Add(3 * time.Hour)
	m, _ = feed(m, clockMsg{every: time.Second})
	if m.clockEvery != time.Minute {
		t.Errorf("past due: clock every %v", m.clockEvery)
	}
}

// A row with seconds that scrolls into view while a minute tick is pending
// starts the one-second chain at once, not a minute later.
func TestScrollingASecondsRowIntoViewSpeedsTheClockUp(t *testing.T) {
	now := at
	dues := []time.Duration{3 * time.Hour}
	for i := 0; i < 30; i++ {
		dues = append(dues, 4*time.Hour)
	}
	dues = append(dues, 30*time.Second) // far below the visible rows
	m := countdownModel(t, &now, nil, dues...)
	if m.clockEvery != time.Minute {
		t.Fatalf("the near-due row is off screen: clock every %v", m.clockEvery)
	}
	var cmd tea.Cmd
	m, cmd = feed(m, key(tea.KeyEnd))
	if m.clockEvery != time.Second || cmd == nil {
		t.Errorf("the near-due row came into view: clock every %v", m.clockEvery)
	}
}

// The preview keeps the exact local date and time, and says when the item fires.
func TestPreviewKeepsTheExactNextDue(t *testing.T) {
	v := withDeliveries(unowned("a"))
	st := v.Inspection.Last.Status
	due := time.Date(2026, 11, 19, 9, 0, 0, 0, time.UTC)
	st.Schedule = []groundhog.ScheduleItem{{Path: "schedule/daily/x", ItemID: "x", Cadence: &groundhog.Cadence{Axis: "daily", Time: sp("09:00")},
		PausedBy: []string{}, BlockedBy: []string{"not_due"}, NextDue: &due}}
	v.Inspection.Last = obs(st)
	n := instanceNode(v, nil, false).Children[0]
	want := "Next due: " + due.Local().Format("Mon 2 Jan 2006 15:04:05 MST")
	for _, w := range []string{want, "Cadence: daily at 09:00", "first tick at or after it"} {
		if !strings.Contains(n.Preview, w) {
			t.Errorf("preview lacks %q:\n%s", w, n.Preview)
		}
	}
	if n.Due == nil || !n.Due.Equal(due) {
		t.Errorf("Due = %v", n.Due)
	}
}

// Without clock ticks the shell schedules none: not at Init, not after a tick,
// and not when a near-due row scrolls into view. The rows still count down
// against the clock.
func TestWithoutClockTicksTheShellSchedulesNone(t *testing.T) {
	now := at
	dues := []time.Duration{3 * time.Hour}
	for i := 0; i < 30; i++ {
		dues = append(dues, 4*time.Hour)
	}
	dues = append(dues, 30*time.Second) // below the visible rows
	m := countdownModel(t, &now, nil, dues...)
	quiet := NewWithData(Data{}, WithoutClockTicks())
	if quiet.Init() != nil {
		t.Fatal("Init scheduled a tick")
	}
	WithoutClockTicks()(&m)
	if m.Init() != nil {
		t.Fatal("Init scheduled a tick")
	}
	if _, cmd := m.onClock(clockMsg{every: m.clockEvery}); cmd != nil {
		t.Fatal("a tick scheduled the next one")
	}
	every := m.clockEvery
	m, _ = feed(m, key(tea.KeyEnd))
	if m.clockEvery != every {
		t.Fatalf("scrolling a seconds row into view started a %v chain", m.clockEvery)
	}
	before := itemRow(m, len(dues)-1)
	now = now.Add(20 * time.Second)
	m, _ = feed(m, key(tea.KeyUp), key(tea.KeyDown))
	if after := itemRow(m, len(dues)-1); after == before {
		t.Errorf("the row stopped counting down: %q", after)
	}
}
