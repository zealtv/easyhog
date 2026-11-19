package tui

import (
	"fmt"
	"math"
	"time"

	tea "github.com/charmbracelet/bubbletea"
)

// The countdown in an item row's badge says when the item next becomes due, in
// plain words that lose precision as the time grows: "in 30s", "in 15 min",
// "in 3 h 20 min", "in 9 days", "in a week", "in 3 weeks", "in 2 months". The
// exact local date and time stay one step away, in the preview.
//
// The time comes from the script's own next_due (Easyhog never works it out
// from the cadence) and is set against the local clock when the row is drawn,
// so it moves between status reads. Nothing here reads a status: the clock tick
// below only moves the model's idea of "now".

// countdownWording is the words for an item that becomes due after r. r must be
// positive: at or past the due time the row shows its due word instead. It
// rounds up in the small units, so a countdown never promises more time than
// there is, and to the nearest in the large ones.
func countdownWording(r time.Duration) string {
	const day = 24 * time.Hour
	switch {
	case r <= 0:
		return ""
	case r <= 50*time.Second:
		return fmt.Sprintf("in %ds", ceilDiv(r, 10*time.Second)*10)
	}
	if mins := ceilDiv(r, time.Minute); mins < 60 {
		return fmt.Sprintf("in %d min", mins)
	} else if mins < 360 {
		if mins%60 == 0 {
			return fmt.Sprintf("in %d h", mins/60)
		}
		return fmt.Sprintf("in %d h %d min", mins/60, mins%60)
	}
	if r < day {
		return fmt.Sprintf("in %d h", ceilDiv(r, time.Hour))
	}
	days := int(math.Round(float64(r) / float64(day)))
	switch {
	case days == 7:
		return "in a week"
	case days < 14:
		return "in " + unitCount(days, "day", "days")
	case days < 60:
		return "in " + unitCount(int(math.Round(float64(days)/7)), "week", "weeks")
	case days < 548: // about 18 months
		return "in " + unitCount(int(math.Round(float64(days)/30.44)), "month", "months")
	}
	return "in " + unitCount(int(math.Round(float64(days)/365.25)), "year", "years")
}

func unitCount(n int, one, many string) string {
	if n == 1 {
		return "1 " + one
	}
	return fmt.Sprintf("%d %s", n, many)
}

func ceilDiv(d, unit time.Duration) int {
	return int((d + unit - 1) / unit)
}

// secondsAhead is how close to due a countdown must be for the shell to redraw
// once a second. The words show seconds only in the last 50 s; starting two
// minutes early keeps the minute words and the switch to seconds from lagging.
const secondsAhead = 2 * time.Minute

// WithClock sets where the shell reads the local time from. It is read on every
// update and on each clock tick, never while drawing. The default is time.Now.
func WithClock(now func() time.Time) Option { return func(m *Model) { m.clock = now } }

// WithoutClockTicks stops the shell scheduling its own clock ticks. The
// countdowns still move with the clock on every update. It is for a harness that
// runs the model's commands synchronously and feeds it time itself: there, a tick
// that re-arms itself would never let a command finish.
func WithoutClockTicks() Option { return func(m *Model) { m.noTicks = true } }

// clockMsg is the clock tick. It carries the interval of the chain that sent
// it, so a chain that has been replaced (a row with seconds scrolled into view
// while a one-minute tick was pending) ends when its message arrives.
type clockMsg struct{ every time.Duration }

func tickClock(every time.Duration) tea.Cmd {
	return tea.Tick(every, func(time.Time) tea.Msg { return clockMsg{every} })
}

func (m Model) clockNow() time.Time {
	if m.clock != nil {
		return m.clock()
	}
	return time.Now()
}

// badgeOf is what a row's badge says now: the countdown for an item that is not
// yet due, otherwise the badge text the adapter built.
func (m Model) badgeOf(n *Node) string {
	if n.Due != nil {
		if r := n.Due.Sub(m.now); r > 0 {
			return countdownWording(r)
		}
	}
	return n.Badge
}

// wantsSeconds reports whether a tree row on screen is counting down close
// enough to due that the clock should tick once a second.
func (m Model) wantsSeconds() bool {
	for i := m.treeTop; i < len(m.rows) && i < m.treeTop+max(m.treeRows(), 0); i++ {
		if due := m.rows[i].node.Due; due != nil {
			if r := due.Sub(m.now); r > 0 && r <= secondsAhead {
				return true
			}
		}
	}
	return false
}

// clockInterval is the tick the shell wants now: a second while a visible row
// shows seconds, otherwise a minute.
func (m Model) clockInterval() time.Duration {
	if m.wantsSeconds() {
		return time.Second
	}
	return time.Minute
}

// onClock handles a tick: it keeps the chain going at the interval the screen
// now needs, and drops a tick from a chain that was replaced. The new "now" was
// already taken when the update began.
func (m Model) onClock(msg clockMsg) (Model, tea.Cmd) {
	if m.noTicks || msg.every != m.clockEvery {
		return m, nil
	}
	m.clockEvery = m.clockInterval()
	return m, tickClock(m.clockEvery)
}

// speedUpClock starts a one-second chain when something on screen has come to
// need one (a scroll or a snapshot brought a near-due row into view) while a
// one-minute tick is still pending.
func (m Model) speedUpClock(cmd tea.Cmd) (Model, tea.Cmd) {
	if !m.noTicks && m.clockEvery > time.Second && m.wantsSeconds() {
		m.clockEvery = time.Second
		return m, tea.Batch(cmd, tickClock(time.Second))
	}
	return m, cmd
}
