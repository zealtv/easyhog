//go:build unix

package acceptance

import (
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	tea "github.com/charmbracelet/bubbletea"

	"github.com/zealtv/easyhog/internal/core"
	"github.com/zealtv/easyhog/internal/groundhog"
	"github.com/zealtv/easyhog/internal/tui"
)

// Pause item and Unpause item from the palette: the item's
// folder is renamed, the row keeps its place, the real script reads the pause
// back, and a runner that is running keeps running and stops, then resumes,
// firing the item. The renames are on temporary instances only.
func TestPausingAnItemFromThePalette(t *testing.T) {
	script := gate(t)
	w := newWorld(t, script)

	mine := w.instance("pausing")
	w.item(mine, "every/1h", "beat", "heartbeat note")
	w.item(mine, "every/1h", "other", "other note")
	theirs := w.instance("theirs")
	w.item(theirs, "every/1h", "beat", "their note")
	w.start()
	if res, err := w.osup.Run(w.ctx, theirs); err != nil || res.Outcome != core.RunStarted {
		t.Fatalf("setup: another supervisor could not run theirs: %+v %v", res, err)
	}

	const rel = "schedule/every/1h/beat"
	beat := filepath.Join(mine, filepath.FromSlash(rel))
	paused := beat + ".paused"
	itemKey := mine + "#" + rel

	// lists reports whether the palette, searched for query, lists that command's row.
	lists := func(query, row string) bool {
		t.Helper()
		w.send(typed(":" + query)...)
		found := strings.Contains(w.screen(), row)
		w.send(press(tea.KeyEsc))
		return found
	}
	const pauseRow, unpauseRow = "◐ Pause item", "◷ Unpause item"
	child := func(label string) tui.Node {
		t.Helper()
		for _, c := range w.node("pausing").Children {
			if c.Label == label {
				return c
			}
		}
		t.Fatalf("no item row %q under pausing", label)
		return tui.Node{}
	}
	selectItem := func(label string) {
		t.Helper()
		w.selectInstance("pausing")
		w.send(press(tea.KeyRight)) // expand
		w.selectRow(label)
	}

	t.Run("an unowned instance's item is paused, and its row keeps its place", func(t *testing.T) {
		selectItem("beat")
		if !lists("pause item", pauseRow) || lists("unpause", unpauseRow) {
			t.Fatalf("a scheduled item should offer Pause item only\n%s", w.screen())
		}
		w.palette("pause item")
		if _, err := os.Stat(paused); err != nil {
			t.Fatalf("the folder was not renamed: %v", err)
		}
		if _, err := os.Stat(beat); err == nil {
			t.Fatal("the plain folder is still there")
		}
		w.reread("pausing")
		if got := w.selected(); got != "beat" {
			t.Fatalf("the selection moved to %q\n%s", got, w.screen())
		}
		if n := child("beat"); n.Key != itemKey || n.State != tui.StatePaused {
			t.Errorf("row key %q state %v", n.Key, n.State)
		}
		st := w.status(mine)
		var found bool
		for _, it := range st.Schedule {
			if it.Path == rel+".paused" {
				found = len(it.PausedBy) == 1 && it.PausedBy[0] == rel+".paused" && !it.FiringAllowed
			}
		}
		if !found {
			t.Errorf("the script's status does not read the item as paused by its own name: %+v", st.Schedule)
		}
		if !strings.Contains(w.screen(), "paused") || strings.Contains(w.screen(), "beat.paused") && !strings.Contains(w.flat(), "Item "+rel+".paused") {
			t.Errorf("the row or preview misses the pause\n%s", w.screen())
		}
		if !lists("unpause item", unpauseRow) || lists("pause item", pauseRow) {
			t.Errorf("a paused item should offer Unpause item only\n%s", w.screen())
		}
	})

	t.Run("a running runner skips the paused item and fires it once it is unpaused", func(t *testing.T) {
		w.selectInstance("pausing")
		w.palette("run")
		w.await("the runner to fire the other item", 30*time.Second, func() bool {
			st := w.status(mine)
			return st.Runner.State == groundhog.RunnerRunning && len(deliveriesOf(st, "other")) > 0
		})
		if n := len(deliveriesOf(w.status(mine), "beat")); n != 0 {
			t.Fatalf("the paused item fired (%d deliveries)", n)
		}
		w.reread("pausing")
		selectItem("beat")
		if got := w.selected(); got != "beat" {
			t.Fatalf("selected %q", got)
		}
		w.palette("unpause item")
		if _, err := os.Stat(beat); err != nil {
			t.Fatalf("the folder was not renamed back: %v", err)
		}
		w.await("the unpaused item to fire under the running runner", 30*time.Second, func() bool {
			return len(deliveriesOf(w.status(mine), "beat")) > 0
		})
		if st := w.status(mine); st.Runner.State != groundhog.RunnerRunning || st.Owner == nil || runnerProcesses(t, mine) != 1 {
			t.Errorf("after the rename: runner %s, owner %+v, %d processes", st.Runner.State, st.Owner, runnerProcesses(t, mine))
		}
		w.reread("pausing")
		if n := child("beat"); n.Key != itemKey || n.State == tui.StatePaused {
			t.Errorf("row key %q state %v", n.Key, n.State)
		}
	})

	t.Run("foo beside foo.paused shows both rows and offers neither", func(t *testing.T) {
		w.must(os.Mkdir(paused, 0o755))
		w.reread("pausing")
		selectItem("beat")
		if lists("pause item", pauseRow) || lists("unpause", unpauseRow) {
			t.Fatalf("a toggle is offered beside its twin\n%s", w.screen())
		}
		if !strings.Contains(w.flat(), "Pausing is not offered: both beat and beat.paused exist") {
			t.Errorf("the preview does not say why\n%s", w.screen())
		}
		var labels []string
		for _, c := range w.node("pausing").Children {
			labels = append(labels, c.Label)
		}
		if got := strings.Join(labels, ","); got != "beat,beat.paused,other" {
			t.Errorf("rows %s", got)
		}
		_, err := w.sup.SetItemPaused(w.ctx, mine, rel, true)
		var re *core.RefusedError
		if !errors.As(err, &re) || re.Reason != core.RefusedNotOffered {
			t.Errorf("a stale request was not refused: %v", err)
		}
		w.must(os.Remove(paused))
		w.reread("pausing")
	})

	t.Run("an item of an instance another supervisor runs is left alone", func(t *testing.T) {
		w.selectInstance("theirs")
		w.send(press(tea.KeyRight)) // expand
		// "pausing" is still expanded above, with an item of the same name, so
		// selecting by label would land there: step onto theirs' own item instead.
		w.send(press(tea.KeyDown))
		if !strings.Contains(w.flat(), "in theirs") || !strings.Contains(w.flat(), "Item "+rel) {
			t.Fatalf("not on theirs' item\n%s", w.screen())
		}
		if lists("pause item", pauseRow) {
			t.Fatalf("Pause item is offered on an externally owned instance\n%s", w.screen())
		}
		_, err := w.sup.SetItemPaused(w.ctx, theirs, rel, true)
		var re *core.RefusedError
		if !errors.As(err, &re) || re.Reason != core.RefusedExternal {
			t.Fatalf("not refused as external: %v", err)
		}
		if _, err := os.Stat(filepath.Join(theirs, filepath.FromSlash(rel))); err != nil {
			t.Errorf("their item moved: %v", err)
		}
	})
}
