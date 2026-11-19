//go:build unix

package acceptance

import (
	"strings"
	"sync"
	"syscall"
	"testing"
	"time"

	"github.com/zealtv/easyhog/internal/core"
	"github.com/zealtv/easyhog/internal/groundhog"
	"github.com/zealtv/easyhog/internal/tui"
)

// A runner killed by a signal skips its exit trap. The view must follow the
// protocol's status (runner "unknown", once the recorded process is gone) within
// one refresh interval, and Run must be offered again, because this session
// saw its child end. The test feeds the model the way cmd/easyhog does: it
// rebuilds the data only when the refresher or the supervisor signals a change,
// so a read that is stored but not announced would leave the old screen.
//
// Before the fix, the fed data kept "Runner: running" and offered Stop while
// the real status said unknown.
func TestKilledRunnerShowsNotRunningWithinOneIntervalAndOffersRun(t *testing.T) {
	script := gate(t)
	w := newWorld(t, script)
	inst := w.instance("solo")
	w.start()

	// What cmd/easyhog's feed goroutine does: rebuild on either signal.
	var mu sync.Mutex
	var fed tui.Data
	stop := make(chan struct{})
	t.Cleanup(func() { close(stop) })
	push := func() {
		d := w.data()
		mu.Lock()
		fed = d
		mu.Unlock()
	}
	push()
	go func() {
		for {
			select {
			case <-w.ref.Changed():
			case <-w.sup.Changed():
			case <-stop:
				return
			}
			push()
		}
	}()
	soloNode := func() tui.Node {
		mu.Lock()
		defer mu.Unlock()
		for _, n := range fed.Nodes {
			if n.Label == "solo" {
				return n
			}
		}
		t.Fatal("no solo node in the fed data")
		return tui.Node{}
	}

	w.selectInstance("solo")
	if w.palette("run") {
		t.Fatal("Run quit the program")
	}
	w.await("the runner to run", 30*time.Second, func() bool { return w.status(inst).Runner.State == groundhog.RunnerRunning })
	w.until("the view to show the runner running", 20*time.Second, func() bool {
		return strings.Contains(soloNode().Preview, "Runner: running")
	})
	if !hasAction(soloNode(), tui.ActionStop) {
		t.Fatalf("a running runner of ours should offer Stop: %v", soloNode().Actions)
	}

	// The test runs the crash.
	w.easy.mu.Lock()
	pid := w.easy.kids[len(w.easy.kids)-1].PID()
	w.easy.mu.Unlock()
	killed := time.Now()
	if err := syscall.Kill(pid, syscall.SIGKILL); err != nil {
		t.Fatal(err)
	}

	// One interval to the next background read, plus the read itself and a
	// margin for a loaded machine.
	limit := w.ref.Config().Interval + 3*time.Second
	deadline := killed.Add(limit)
	for {
		n := soloNode()
		if !strings.Contains(n.Preview, "Runner: running") && strings.Contains(n.Preview, "Runner: ") {
			if !hasAction(n, tui.ActionRun) || !hasAction(n, tui.ActionStop) {
				t.Fatalf("after the crash the palette should offer Run and Stop (Stop lets the instance go): %v\n%s", n.Actions, n.Preview)
			}
			break
		}
		if time.Now().After(deadline) {
			t.Fatalf("%s after SIGKILL the fed view still shows the runner running (real status: %s)\n%s",
				limit, w.status(inst).Runner.State, n.Preview)
		}
		time.Sleep(50 * time.Millisecond)
	}
	if got := w.status(inst).Runner.State; got == groundhog.RunnerRunning {
		t.Errorf("the protocol still says running (%s): the test saw the wrong thing", got)
	}
	if info, ok := w.sup.Runner(inst); !ok || info.Phase != core.PhaseExited {
		t.Errorf("the supervisor's word: %+v %v, want PhaseExited", info, ok)
	}

	// Run brings it back: the supervisor starts a runner when its child is not live.
	w.sync()
	if w.palette("run") {
		t.Fatal("Run quit the program")
	}
	w.await("the runner to run again", 30*time.Second, func() bool { return w.status(inst).Runner.State == groundhog.RunnerRunning })
	if got := w.easy.startsFor(inst); got != 2 {
		t.Errorf("the runner started %d times, want 2 (once, and again by Run)", got)
	}
}
