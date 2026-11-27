package tui

import (
	"fmt"

	"github.com/zealtv/easyhog/internal/core"
	"github.com/zealtv/easyhog/internal/twglance"
)

// What Easyhog draws of itself inside its own pane's box on tireless-watcher's
// map (twglance): a badge line, "hog" and how many instances this session
// ticks, how many something outside ticks, and how many of all those have
// failed (a delivery, its inspection or this session's tick of it), and a list
// of the ticked instances only, one row each. Every other instance
// stays in the tree: the map's box is for what is running.

// ticker says who ticks an instance, as far as this session can tell.
type ticker int

const (
	tickedByNone  ticker = iota
	tickedHere           // this session ticks it
	tickedOutside        // a fresh tick that was not this session's
)

// tickerOf is who ticks iv. Outside means a recorded tick within
// core.TickFreshness that wasn't this session's, the rule the tree's
// "external" uses, here read whether or not the instance is paused, since a
// paused instance can still be ticked. It is known only for a supported
// instance this session has read; one whose inspection now fails keeps the
// tick its last good read saw, and its row reads failed.
func tickerOf(iv core.InstanceView, tick *core.TickInfo) ticker {
	if tick != nil && tick.Ticking {
		return tickedHere
	}
	if iv.Class != core.ClassSupported || iv.Inspection.Last == nil {
		return tickedByNone
	}
	v, ok := core.PendingVerdict(iv, tick)
	if ok && v.HasTick && !v.Ours && v.Age <= core.TickFreshness {
		return tickedOutside
	}
	return tickedByNone
}

// glanceRow is one ticked instance: who ticks it, whether this session's
// last tick of it failed, and its row in the tree.
type glanceRow struct {
	by         ticker
	tickFailed bool
	node       Node
}

// glanceOf is the glance for the ticked instances among rows, in tree order,
// those this session ticks first. A failed or paused instance takes the tree's
// glyph and the map's role for it, and a failed tick here reads failed, as the
// pane's @tw_status does; otherwise "●" (ok) says ticked here and "◇"
// (other) ticked from outside.
func glanceOf(rows []glanceRow) twglance.Glance {
	g := twglance.Glance{Badges: []twglance.Entry{{Role: twglance.Label, Text: "hog"}}}
	var here, outside, failed int
	var list, later []twglance.Entry
	for _, r := range rows {
		var e twglance.Entry
		switch {
		case r.node.State == StateFailed || r.tickFailed:
			failed++
			e = twglance.Entry{Role: twglance.Bad, Text: StateFailed.glyph()}
		case r.node.State == StatePaused:
			e = twglance.Entry{Role: twglance.Hold, Text: StatePaused.glyph()}
		case r.by == tickedHere:
			e = twglance.Entry{Role: twglance.OK, Text: StateRunning.glyph()}
		default:
			e = twglance.Entry{Role: twglance.Other, Text: StateExternal.glyph()}
		}
		e.Text += " " + r.node.Label
		if r.by == tickedHere {
			here++
			list = append(list, e)
		} else {
			outside++
			later = append(later, e)
		}
	}
	// Zeros are left out: a blank says it.
	for _, b := range []struct {
		n          int
		role, mark string
	}{
		{here, twglance.OK, StateRunning.glyph()},
		{outside, twglance.Other, StateExternal.glyph()},
		{failed, twglance.Bad, StateFailed.glyph()},
	} {
		if b.n > 0 {
			g.Badges = append(g.Badges, twglance.Entry{Role: b.role, Text: fmt.Sprintf("%s%d", b.mark, b.n)})
		}
	}
	g.Rows = append(list, later...)
	return g
}
