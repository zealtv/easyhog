//go:build unix

package acceptance

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/zealtv/easyhog/internal/core"
	"github.com/zealtv/easyhog/internal/tui"
)

// TestPendingWorkVerdict follows the one-line answer to "will due items fire?"
// through real scripts on the pinned copy. Instances stand for the ways one gets
// ticked: this session, a standalone `tick` from something outside, and
// nothing. An old tick is made by writing the script's own record file with an
// earlier time, which stands in for waiting out the 3-minute threshold.
func TestPendingWorkVerdict(t *testing.T) {
	w := newWorld(t, gate(t))
	ours, outside, aged, quiet := w.instance("ours"), w.instance("outside"), w.instance("aged"), w.instance("quiet")
	for _, inst := range []string{ours, outside, aged, quiet} {
		w.item(inst, future, "later", "synthetic note") // nothing due at once
	}
	w.hog(outside, "tick")
	w.hog(aged, "tick")
	old := time.Now().UTC().Add(-10 * time.Minute).Format("2006-01-02T15:04:05Z")
	w.must(os.WriteFile(filepath.Join(aged, "runtime", "last_tick"), []byte(old+" standalone\n"), 0o644))
	w.start()

	badge := func(rel string) string {
		w.selectInstance(rel)
		return w.node(rel).Badge
	}
	// The row's badge leaves the verdict out (the state word and the heartbeat
	// mark say it; Bob, 2026-11-19), and the preview keeps the whole verdict.
	noVerdict := func(b string) bool {
		return !strings.Contains(b, "tick") && !strings.Contains(b, "probably") && !strings.Contains(b, "paused")
	}
	if b := badge("outside"); !noVerdict(b) {
		t.Errorf("ticked standalone just now: badge %q, want no verdict and no tick age in it", b)
	}
	if n := w.node("outside"); n.Heartbeat != core.HeartbeatFresh || n.State != tui.StateExternal || !strings.HasPrefix(n.Detail, "ticked from outside Easyhog") {
		t.Errorf("ticked standalone just now: state %v, detail %q, heartbeat %v", n.State, n.Detail, n.Heartbeat)
	}
	if !strings.Contains(w.screen(), "Pending work: probably, something outside Easyhog ticked it") {
		t.Errorf("preview lacks the probable verdict\n%s", w.screen())
	}
	if b := badge("aged"); !noVerdict(b) || w.node("aged").State != tui.StateStopped {
		t.Errorf("a tick 10 minutes old: badge %q, state %v", b, w.node("aged").State)
	}
	if b := badge("quiet"); !noVerdict(b) || !strings.Contains(w.screen(), "nothing seen ticking it; no tick recorded.") {
		t.Errorf("never ticked: badge %q\n%s", b, w.screen())
	}

	// Run: this session ticks it, so the verdict is yes; Pause instance makes it no.
	w.selectInstance("ours")
	w.palette("run")
	w.await("the first tick to be recorded", 20*time.Second, func() bool { return w.status(ours).LastTick != nil })
	w.reread("ours")
	if n := w.node("ours"); !noVerdict(n.Badge) || n.Heartbeat != core.HeartbeatFresh || n.State != tui.StateRunning {
		t.Errorf("ticked here: badge %q, heartbeat %v, state %v", n.Badge, n.Heartbeat, n.State)
	}
	if !strings.Contains(w.flat(), "Pending work: yes, this session ticks it") {
		t.Errorf("preview lacks the yes verdict\n%s", w.screen())
	}
	w.palette("pause in")
	w.reread("ours")
	if n := w.node("ours"); !noVerdict(n.Badge) || n.State != tui.StatePaused || !strings.Contains(w.flat(), "Pending work: no, the instance is paused.") {
		t.Errorf("paused: badge %q, state %v\n%s", n.Badge, n.State, w.screen())
	}

	// Stop: this session's own fresh tick does not make the row external.
	w.palette("unpause")
	w.palette("stop")
	w.reread("ours")
	if n := w.node("ours"); n.State != tui.StateStopped || !strings.Contains(w.flat(), "since this session stopped") {
		t.Errorf("stopped here: state %v %q\n%s", n.State, n.Detail, w.screen())
	}

	// Run on the instance ticked from outside asks nothing, and its outside
	// ticks still work beside this session's.
	w.selectInstance("outside")
	w.palette("run")
	if !w.said("Run: outside: ticked") {
		t.Fatalf("Run on an instance ticked from outside\n%s", w.screen())
	}
	w.hog(outside, "tick")
}
