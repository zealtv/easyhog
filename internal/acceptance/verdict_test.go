//go:build unix

package acceptance

import (
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
	"time"

	tea "github.com/charmbracelet/bubbletea"

	"github.com/zealtv/easyhog/internal/core"
	"github.com/zealtv/easyhog/internal/tui"
)

// TestPendingWorkVerdict follows the one-line answer to "will pending work be
// actioned?" through real scripts on the pinned copy. Three instances stand
// for the three ways an instance gets ticked: Easyhog's own runner, a
// standalone `tick` from something outside, and nothing. An old tick is made
// by writing the script's own record file with an earlier time, which stands
// in for waiting out the 3-minute threshold.
func TestPendingWorkVerdict(t *testing.T) {
	w := newWorld(t, gate(t))
	ours, outside, aged, quiet := w.instance("ours"), w.instance("outside"), w.instance("aged"), w.instance("quiet")
	for _, inst := range []string{ours, outside, aged, quiet} {
		w.item(inst, "every/1h", "hourly", "synthetic note") // gives a runner something to hold, nothing due at once
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
	if b := badge("outside"); !strings.HasPrefix(b, "probably · ") || strings.Contains(b, "tick") {
		t.Errorf("an unowned instance ticked standalone just now: badge %q, want probably, with no tick age", b)
	}
	// Its tick is seconds old: the mark after the state word reads fresh.
	if h := w.node("outside").Heartbeat; h != core.HeartbeatFresh {
		t.Errorf("an instance ticked standalone just now: heartbeat %v, want fresh", h)
	}
	if !strings.Contains(w.screen(), "Pending work: probably, something outside Easyhog ticked it") {
		t.Errorf("preview lacks the probable verdict\n%s", w.screen())
	}
	// The row reads external though no owner is recorded: something outside ticks it.
	if n := w.node("outside"); n.State != tui.StateExternal || !strings.HasPrefix(n.Detail, "ticked from outside Easyhog") {
		t.Errorf("an instance ticked standalone just now: state %v, detail %q, want external, ticked from outside Easyhog", n.State, n.Detail)
	}
	noVerdict := func(b string) bool {
		return !strings.Contains(b, "tick") && !strings.HasPrefix(b, "will run") && !strings.HasPrefix(b, "probably") && !strings.HasPrefix(b, "paused")
	}
	if b := badge("aged"); !noVerdict(b) || w.node("aged").State != tui.StateStopped {
		t.Errorf("a tick 10 minutes old: badge %q, want no verdict in it", b)
	}
	if b := badge("quiet"); !noVerdict(b) {
		t.Errorf("an instance never ticked: badge %q, want no verdict in it", b)
	}
	if !strings.Contains(w.screen(), "nothing seen ticking it; no tick recorded.") {
		t.Errorf("preview lacks the never-ticked wording\n%s", w.screen())
	}

	// Run: this session's runner ticks, so the verdict becomes yes; a pause set outside Easyhog makes it no
	// (Easyhog sets none itself).
	if res, err := w.sup.Run(w.ctx, ours); err != nil || res.Outcome != core.RunStarted {
		t.Fatalf("Run: %+v %v", res, err)
	}
	w.await("the runner's first recorded tick", 20*time.Second, func() bool { return w.status(ours).LastTick != nil })
	w.reread("ours")
	if b := w.node("ours").Badge; !strings.HasPrefix(b, "will run · ") || strings.Contains(b, "tick") {
		t.Errorf("our runner is running: badge %q, want will run, with no tick age", b)
	}
	if h := w.node("ours").Heartbeat; h != core.HeartbeatFresh {
		t.Errorf("our runner has just ticked: heartbeat %v, want fresh", h)
	}
	info, ok := w.sup.Runner(ours)
	if !ok {
		t.Fatal("the supervisor holds nothing for the instance it ran")
	}
	if err := w.client(ours).Pause(w.ctx, info.TokenFile); err != nil {
		t.Fatal(err)
	}
	w.reread("ours")
	if b := w.node("ours").Badge; !strings.HasPrefix(b, "paused: won't run") {
		t.Errorf("paused: badge %q, want paused: won't run", b)
	}

	confirmRunOnAnInstanceTickedFromOutside(t, w, outside, aged)
}

// Run on an instance something outside Easyhog ticks asks first, because
// enrolling it makes those ticks fail with exit 3 until Stop releases it. Esc
// leaves it untouched; an instance nothing ticks runs without a question; and
// yes enrols it, after which the outside tick fails, as the question warned.
func confirmRunOnAnInstanceTickedFromOutside(t *testing.T, w *world, outside, aged string) {
	w.selectInstance("outside")
	w.palette("run")
	if flat := w.flat(); !strings.Contains(flat, "Run anyway?") || !strings.Contains(flat, "those ticks will fail until you Stop it here") {
		t.Errorf("Run on an instance ticked from outside did not ask\n%s", w.screen())
	}
	w.send(press(tea.KeyEsc))
	if _, held := w.sup.Runner(outside); held || w.easy.startsFor(outside) != 0 || w.status(outside).Owner != nil {
		t.Errorf("declining Run still enrolled or started the instance")
	}

	// Its tick is 10 minutes old: stopped, and Run starts without a question.
	w.selectInstance("aged")
	w.palette("run")
	if strings.Contains(w.screen(), "Run anyway?") {
		t.Errorf("Run asked about an instance nothing ticks\n%s", w.screen())
	}
	w.await("Easyhog to own the aged instance", 30*time.Second, func() bool { return w.status(aged).Owner != nil })

	// Yes: Enter on the question.
	w.selectInstance("outside")
	w.palette("run")
	w.send(press(tea.KeyEnter))
	w.await("Easyhog to own the outside instance", 30*time.Second, func() bool { return w.status(outside).Owner != nil })
	if out, err := exec.Command(filepath.Join(outside, "groundhog.sh"), "tick").CombinedOutput(); err == nil {
		t.Errorf("a standalone tick on an instance Easyhog owns succeeded: %s", out)
	}
}
