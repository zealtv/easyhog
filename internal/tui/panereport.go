package tui

import (
	"fmt"
	"strconv"
	"strings"
	"time"

	"github.com/zealtv/easyhog/internal/core"
	"github.com/zealtv/easyhog/internal/groundhog"
	"github.com/zealtv/easyhog/internal/twpane"
)

// What Easyhog tells tireless-watcher's map about itself through its own pane's
// options (twpane): the next wake among the instances this session ticks, and,
// while one of them has a failed wake, that it failed and why. Instances ticked
// from outside are left out of both: their wakes belong to whatever ticks them,
// and this session reads them only when the reader asks.

// PaneReport is the part of a snapshot the pane's options are made from. Its
// zero value says nothing, which unsets them.
type PaneReport struct {
	// Next is the soonest next_due among the unpaused items of the unpaused
	// instances this session ticks; nil when none has one.
	Next *PaneWake
	// Fail is why a wake in an instance this session ticks failed ("alpha:
	// tick failed", "alpha: report failed"), empty when none did.
	Fail string
}

// PaneWake is one item's next wake: the item's name, its instance's label and
// when it is due.
type PaneWake struct {
	Item, Instance string
	Due            time.Time
}

// paneReport works out the report from the instances this session ticks.
func paneReport(v core.View, ticks TickLookup) PaneReport {
	var r PaneReport
	var fails []string
	if ticks == nil {
		return r
	}
	for _, iv := range v.Instances {
		info, ok := ticks(iv.Instance.Path)
		if !ok || !info.Ticking {
			continue
		}
		label := instanceLabel(iv.Instance)
		if info.LastErr != nil {
			fails = append(fails, label+": tick failed")
		}
		last := iv.Inspection.Last
		if last == nil {
			continue
		}
		st := last.Status
		if f := failedWakes(st); f != "" {
			fails = append(fails, label+": "+f)
		}
		if st.Paused {
			continue
		}
		for _, it := range st.Schedule {
			if it.NextDue == nil || len(it.PausedBy) > 0 {
				continue
			}
			if r.Next == nil || it.NextDue.Before(r.Next.Due) {
				r.Next = &PaneWake{Item: shortItemName(it.ItemID, it.Path), Instance: label, Due: *it.NextDue}
			}
		}
	}
	switch len(fails) {
	case 0:
	case 1:
		r.Fail = fails[0]
	default:
		r.Fail = fmt.Sprintf("%s (+%d more)", fails[0], len(fails)-1)
	}
	return r
}

// failedWakes names what failed among st's deliveries, as the tree counts
// failed (failedState): the one item's name, or how many failed.
func failedWakes(st groundhog.Status) string {
	var names []string
	for _, d := range st.Deliveries {
		if !failedState(d.State) {
			continue
		}
		name := strings.TrimPrefix(d.Path, "out/")
		if d.SchedulePath != nil {
			name = shortItemName("", *d.SchedulePath)
		}
		names = append(names, name)
	}
	switch len(names) {
	case 0:
		return ""
	case 1:
		return names[0] + " failed"
	}
	return fmt.Sprintf("%d wakes failed", len(names))
}

// shortItemName is an item's name for a one-line report: its item id, or
// else the last folder of its path without a ".paused".
func shortItemName(id, p string) string {
	if id != "" {
		return id
	}
	p = core.ItemIdentity(p)
	return p[strings.LastIndex(p, "/")+1:]
}

// nextLine is the map's ready line for w, read against now: "next: report at
// 10:52 · alpha" (with the weekday, "Mon 10:52", when not today), or
// "report due · alpha" once its time has come and the
// next tick will fire it. The map adds no label, so "next: " is written here.
func nextLine(w *PaneWake, now time.Time) string {
	if w == nil {
		return ""
	}
	if !w.Due.After(now) {
		return "next: " + w.Item + " due · " + w.Instance
	}
	due, today := w.Due.Local(), now.Local()
	when := due.Format("15:04")
	if y, m, d := due.Date(); y != today.Year() || m != today.Month() || d != today.Day() {
		when = due.Format("Mon 15:04")
	}
	return "next: " + w.Item + " at " + when + " · " + w.Instance
}

// paneValues is what the pane's options should say now.
func (m Model) paneValues() twpane.Values {
	v := twpane.Values{twpane.Idle: nextLine(m.data.Pane.Next, m.now)}
	if m.paneFail.text != "" {
		v[twpane.Status] = "fail"
		v[twpane.StatusText] = m.paneFail.text
		v[twpane.StatusAt] = strconv.FormatInt(m.paneFail.at, 10)
	}
	return v
}

// paneFailure is the failure the pane reports and when it began, in Unix
// seconds, so the map's age for it doesn't restart on every snapshot.
type paneFailure struct {
	text string
	at   int64
}

// updatePaneFail brings the reported failure up to date, stamping a new one
// with now.
func (m *Model) updatePaneFail() {
	text := m.data.Pane.Fail
	switch {
	case text == "":
		m.paneFail = paneFailure{}
	case text != m.paneFail.text:
		m.paneFail = paneFailure{text: text, at: m.now.Unix()}
	}
}
