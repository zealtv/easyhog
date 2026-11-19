//go:build unix

package core

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"syscall"
	"testing"
	"time"

	"github.com/zealtv/easyhog/internal/groundhog"
)

// Pause item and Unpause item against the pinned upstream script, on temporary
// instances only: the rename is the toggle the script's own `status` reads back.

func (r *realRig) item() (groundhog.ScheduleItem, bool) {
	r.t.Helper()
	st := r.status()
	if len(st.Schedule) != 1 {
		return groundhog.ScheduleItem{}, false
	}
	return st.Schedule[0], true
}

func (r *realRig) outEntries() int {
	entries, _ := os.ReadDir(filepath.Join(r.key, "out"))
	return len(entries)
}

func TestRealPauseAndUnpauseAnItemOnAnUnownedThenARunningInstance(t *testing.T) {
	r := newRealRig(t, "", true, func(c *SupervisorConfig) { c.RunInterval = time.Second })
	const rel = "schedule/once/job"
	ctx := context.Background()

	// Unowned and stopped: Pause item renames the folder, and the script's status
	// reads it back as paused by its own name, with the suffix in item_id.
	res, err := r.sup.SetItemPaused(ctx, r.key, rel, true)
	if err != nil {
		t.Fatal(err)
	}
	if res.From != rel || res.To != rel+".paused" || res.Item != rel {
		t.Errorf("result %+v", res)
	}
	it, ok := r.item()
	if !ok || it.Path != rel+".paused" || it.ItemID != "job.paused" || len(it.PausedBy) != 1 || it.PausedBy[0] != rel+".paused" || it.FiringAllowed {
		t.Fatalf("status shows %+v", it)
	}
	if ItemIdentity(it.Path) != rel {
		t.Errorf("identity %q", ItemIdentity(it.Path))
	}

	// A runner ticks while it is paused: nothing fires.
	r.run()
	waitFor(t, "a tick by the runner", 15*time.Second, func() bool {
		st := r.status()
		return st.LastTick != nil && st.LastTick.Source == "runner"
	})
	time.Sleep(2500 * time.Millisecond) // two more cycles
	if n := r.outEntries(); n != 0 {
		t.Fatalf("%d entries in out/ while the item was paused", n)
	}

	// Unpause on the running instance: the runner keeps running, and the item
	// fires on a later tick.
	if _, err := r.sup.SetItemPaused(ctx, r.key, rel+".paused", false); err != nil {
		t.Fatal(err)
	}
	if it, ok := r.item(); !ok || it.Path != rel || len(it.PausedBy) != 0 {
		t.Fatalf("status shows %+v", it)
	}
	waitFor(t, "the unpaused item to fire", 20*time.Second, func() bool { return r.outEntries() > 0 })
	if st := r.status(); st.Runner.State != groundhog.RunnerRunning || st.Runner.LastError != nil {
		t.Errorf("runner %+v after the rename", st.Runner)
	}
}

func TestRealPauseIsRefusedOnAnExternalInstanceAndNothingMoves(t *testing.T) {
	r := newRealRig(t, "", true)
	other := mustReal(t, t.TempDir())
	if err := os.Chmod(other, 0o700); err != nil {
		t.Fatal(err)
	}
	if err := r.client().Enroll(context.Background(), "their-supervisor", filepath.Join(other, "their.token")); err != nil {
		t.Fatal(err)
	}
	for _, pause := range []bool{true, false} {
		rel := "schedule/once/job"
		if !pause {
			rel += ".paused"
		}
		_, err := r.sup.SetItemPaused(context.Background(), r.key, rel, pause)
		wantRefused(t, err, RefusedExternal)
	}
	if it, ok := r.item(); !ok || it.Path != "schedule/once/job" {
		t.Fatalf("status shows %+v", it)
	}
}

func TestRealPauseIsRefusedFromAStaleView(t *testing.T) {
	r := newRealRig(t, "", true)
	const rel = "schedule/once/job"
	ctx := context.Background()
	// Unpause an item that is not paused, pause one that already is, and name one
	// the script does not list: each is refused by the fresh read, whatever the
	// view that offered it said.
	_, err := r.sup.SetItemPaused(ctx, r.key, rel+".paused", false)
	wantRefused(t, err, RefusedNotOffered)
	_, err = r.sup.SetItemPaused(ctx, r.key, "schedule/once/nothing", true)
	wantRefused(t, err, RefusedNotOffered)
	if _, err := r.sup.SetItemPaused(ctx, r.key, rel, true); err != nil {
		t.Fatal(err)
	}
	_, err = r.sup.SetItemPaused(ctx, r.key, rel, true) // the view still showed the old name
	wantRefused(t, err, RefusedNotOffered)
	// Both names present: neither is offered, and the refusal says why.
	if err := os.MkdirAll(filepath.Join(r.key, "schedule", "once", "job"), 0o755); err != nil {
		t.Fatal(err)
	}
	for _, c := range []struct {
		rel   string
		pause bool
	}{{rel, true}, {rel + ".paused", false}} {
		_, err = r.sup.SetItemPaused(ctx, r.key, c.rel, c.pause)
		if re := wantRefused(t, err, RefusedNotOffered); !strings.Contains(re.Detail, "both job and job.paused exist") && !strings.Contains(re.Detail, "both job.paused and job exist") {
			t.Errorf("%s: detail %q", c.rel, re.Detail)
		}
	}
	if !isDir(filepath.Join(r.key, "schedule/once/job")) || !isDir(filepath.Join(r.key, "schedule/once/job.paused")) {
		t.Error("a folder moved")
	}
}

// The mutation lock is the script's own: while another writer holds it (as the
// runner's tick does while it copies a due item), the rename waits and then
// gives up with nothing renamed.
func TestRealPauseWaitsForTheScriptsMutationLock(t *testing.T) {
	r := newRealRig(t, "", false, func(c *SupervisorConfig) {
		c.Sleep = func(ctx context.Context, d time.Duration) error { return sleepCtx(ctx, min(d, 5*time.Millisecond)) }
	})
	// A daily item stays after it fires, unlike a once item.
	if err := os.MkdirAll(filepath.Join(r.key, "schedule", "daily", "job"), 0o755); err != nil {
		t.Fatal(err)
	}
	r.run() // creates runtime/locks/mutation.lock and holds the runner lock
	lock := filepath.Join(r.key, "runtime", "locks", "mutation.lock")
	f, err := os.OpenFile(lock, os.O_WRONLY|os.O_APPEND, 0)
	if err != nil {
		t.Fatal(err)
	}
	defer f.Close()
	// The runner takes the lock for a moment each cycle, so hold it until we have it.
	waitFor(t, "the mutation lock", 10*time.Second, func() bool { return syscall.Flock(int(f.Fd()), syscall.LOCK_EX|syscall.LOCK_NB) == nil })
	_, err = r.sup.SetItemPaused(context.Background(), r.key, "schedule/daily/job", true)
	if !errors.Is(err, ErrItemBusy) {
		t.Fatalf("err %v", err)
	}
	if !isDir(filepath.Join(r.key, "schedule/daily/job")) {
		t.Fatal("renamed without the lock")
	}
	_ = syscall.Flock(int(f.Fd()), syscall.LOCK_UN)
	res, err := r.sup.SetItemPaused(context.Background(), r.key, "schedule/daily/job", true)
	if err != nil || !res.Locked {
		t.Fatalf("after the lock was free: %v %+v", err, res)
	}
}
