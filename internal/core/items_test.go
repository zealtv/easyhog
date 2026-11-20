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

// Synthetic instances in temporary directories only.

func TestItemIdentityIgnoresTheItemsOwnPausedSuffix(t *testing.T) {
	for in, want := range map[string]string{
		"schedule/daily/foo":                       "schedule/daily/foo",
		"schedule/daily/foo.paused":                "schedule/daily/foo",
		"schedule/daily/09/report.paused":          "schedule/daily/09/report",
		"schedule/weekly.paused/mon/review":        "schedule/weekly.paused/mon/review", // a folder above keeps its name
		"schedule/weekly.paused/mon/review.paused": "schedule/weekly.paused/mon/review",
		"schedule/daily/foo.paused.paused":         "schedule/daily/foo.paused",
		"schedule/daily/.paused":                   "schedule/daily/.paused", // nothing left of the name: kept
		"foo.paused":                               "foo",
		"":                                         "",
	} {
		if got := ItemIdentity(in); got != want {
			t.Errorf("ItemIdentity(%q) = %q, want %q", in, got, want)
		}
	}
	if !ItemPausedByName("schedule/daily/foo.paused") || ItemPausedByName("schedule/weekly.paused/mon/review") || ItemPausedByName("schedule/daily/.paused") {
		t.Error("ItemPausedByName reads more than the item's own final component")
	}
}

func itemsView(class Class, st groundhog.Status, items ...groundhog.ScheduleItem) InstanceView {
	st.Schedule = items
	return InstanceView{Class: class, Inspection: Inspection{Last: &Observation{Status: st}}}
}

func scheduled(path string) groundhog.ScheduleItem {
	return groundhog.ScheduleItem{Path: path, ItemID: filepath.Base(path)}
}

func pausedItem(path string, by ...string) groundhog.ScheduleItem {
	it := scheduled(path)
	it.PausedBy = by
	return it
}

func TestItemOffersFollowTheItemsFolder(t *testing.T) {
	const a, ap = "schedule/daily/a", "schedule/daily/a.paused"
	plain := groundhog.Status{}
	pausedInstance := groundhog.Status{Paused: true}
	tickedFromOutside := groundhog.Status{ObservedAt: time.Date(2026, 11, 18, 12, 0, 0, 0, time.UTC)}
	tickedFromOutside.LastTick = &groundhog.LastTick{At: tickedFromOutside.ObservedAt.Add(-20 * time.Second), Source: "standalone"}
	withHealth := func(iv InstanceView, h Health) InstanceView { iv.Health = h; return iv }

	for _, c := range []struct {
		name  string
		iv    InstanceView
		rel   string
		want  []Action
		inWhy string // the reason contains this, for a refusal
	}{
		{name: "a scheduled item", iv: itemsView(ClassSupported, plain, scheduled(a)), rel: a, want: []Action{ActionPauseItem}},
		{name: "a paused item", iv: itemsView(ClassSupported, plain, pausedItem(ap, ap)), rel: ap, want: []Action{ActionUnpauseItem}},
		{name: "an instance-wide pause is not the item's", iv: itemsView(ClassSupported, pausedInstance, scheduled(a)), rel: a, want: []Action{ActionPauseItem}},
		{name: "ticked from outside Easyhog: the rename takes the same lock", iv: itemsView(ClassSupported, tickedFromOutside, scheduled(a)), rel: a, want: []Action{ActionPauseItem}},
		{name: "own name and a folder above both paused: unpausing the item is still its own act",
			iv:  itemsView(ClassSupported, plain, pausedItem("schedule/weekly.paused/mon/r.paused", "schedule/weekly.paused", "schedule/weekly.paused/mon/r.paused")),
			rel: "schedule/weekly.paused/mon/r.paused", want: []Action{ActionUnpauseItem}},

		{name: "paused through a folder above: no Unpause, and it says why",
			iv:  itemsView(ClassSupported, plain, pausedItem("schedule/weekly.paused/mon/review", "schedule/weekly.paused")),
			rel: "schedule/weekly.paused/mon/review", inWhy: "paused through schedule/weekly.paused, not by its own name"},
		{name: "foo beside foo.paused: neither toggles (the plain one)", iv: itemsView(ClassSupported, plain, scheduled(a), pausedItem(ap, ap)), rel: a, inWhy: "both a and a.paused exist"},
		{name: "foo beside foo.paused: neither toggles (the paused one)", iv: itemsView(ClassSupported, plain, scheduled(a), pausedItem(ap, ap)), rel: ap, inWhy: "both a.paused and a exist"},
		{name: "an item the status does not list", iv: itemsView(ClassSupported, plain, scheduled(a)), rel: "schedule/daily/zzz", inWhy: "does not list this item"},

		{name: "not inspected", iv: InstanceView{Class: ClassNotInspected}, rel: a, inWhy: "first status read"},
		{name: "unsupported", iv: itemsView(ClassUnsupported, plain, scheduled(a)), rel: a, inWhy: "lacks what ticking"},
		{name: "legacy", iv: itemsView(ClassLegacy, plain, scheduled(a)), rel: a, inWhy: "read-only"},
		{name: "malformed", iv: itemsView(ClassMalformed, plain, scheduled(a)), rel: a, inWhy: "malformed"},
		{name: "stale", iv: withHealth(itemsView(ClassSupported, plain, scheduled(a)), HealthStale), rel: a, inWhy: "stale"},
		{name: "failing read", iv: func() InstanceView {
			v := itemsView(ClassSupported, plain, scheduled(a))
			v.Inspection.Err = errors.New("x")
			return v
		}(), rel: a, inWhy: "status can be read again"},
	} {
		got, why := ItemOffers(c.iv, c.rel)
		if len(got) != len(c.want) || (len(got) == 1 && got[0] != c.want[0]) {
			t.Errorf("%s: offers %v (%q), want %v", c.name, got, why, c.want)
			continue
		}
		if len(c.want) == 0 && !strings.Contains(why, c.inWhy) {
			t.Errorf("%s: reason %q lacks %q", c.name, why, c.inWhy)
		}
		if len(c.want) > 0 && why != "" {
			t.Errorf("%s: offered with a reason %q", c.name, why)
		}
	}
}

// instanceDir makes <tmp>/inst with schedule/<paths...>, one folder per path.
func instanceDir(t *testing.T, folders ...string) string {
	t.Helper()
	inst := filepath.Join(mustReal(t, t.TempDir()), "inst")
	if err := os.MkdirAll(filepath.Join(inst, "schedule"), 0o755); err != nil {
		t.Fatal(err)
	}
	for _, f := range folders {
		if err := os.MkdirAll(filepath.Join(inst, filepath.FromSlash(f)), 0o755); err != nil {
			t.Fatal(err)
		}
	}
	return inst
}

func isDir(p string) bool { fi, err := os.Lstat(p); return err == nil && fi.IsDir() }

func TestRenameItemGoesBothWaysAndKeepsTheFolderWhole(t *testing.T) {
	inst := instanceDir(t, "schedule/daily/09/foo")
	if err := os.WriteFile(filepath.Join(inst, "schedule/daily/09/foo/prompt.md"), []byte("hi"), 0o644); err != nil {
		t.Fatal(err)
	}
	res, err := RenameItem(context.Background(), inst, "schedule/daily/09/foo", true, noWait)
	if err != nil {
		t.Fatal(err)
	}
	if res.From != "schedule/daily/09/foo" || res.To != "schedule/daily/09/foo.paused" || res.Item != "schedule/daily/09/foo" || !res.Paused || res.Locked {
		t.Errorf("result %+v", res)
	}
	if isDir(filepath.Join(inst, "schedule/daily/09/foo")) || !isDir(filepath.Join(inst, "schedule/daily/09/foo.paused")) {
		t.Fatal("not renamed to foo.paused")
	}
	if b, err := os.ReadFile(filepath.Join(inst, "schedule/daily/09/foo.paused/prompt.md")); err != nil || string(b) != "hi" {
		t.Errorf("payload lost: %q %v", b, err)
	}
	res, err = RenameItem(context.Background(), inst, "schedule/daily/09/foo.paused", false, noWait)
	if err != nil {
		t.Fatal(err)
	}
	if res.To != "schedule/daily/09/foo" || res.Item != "schedule/daily/09/foo" || res.Paused {
		t.Errorf("result %+v", res)
	}
	if !isDir(filepath.Join(inst, "schedule/daily/09/foo")) || isDir(filepath.Join(inst, "schedule/daily/09/foo.paused")) {
		t.Fatal("not renamed back")
	}
}

func TestRenameItemRefusesAnythingOutsideTheInstancesSchedule(t *testing.T) {
	inst := instanceDir(t, "schedule/daily/foo", "runtime/state", "elsewhere/foo")
	outside := mustReal(t, t.TempDir())
	if err := os.MkdirAll(filepath.Join(outside, "victim"), 0o755); err != nil {
		t.Fatal(err)
	}
	// A symlinked item, a symlinked folder above an item, and a symlinked schedule/.
	must(t, os.Symlink(filepath.Join(outside, "victim"), filepath.Join(inst, "schedule/daily/link")))
	must(t, os.Symlink(outside, filepath.Join(inst, "schedule/weekly")))
	other := instanceDir(t, "schedule/daily/foo")
	must(t, os.RemoveAll(filepath.Join(other, "schedule")))
	must(t, os.Symlink(filepath.Join(inst, "schedule"), filepath.Join(other, "schedule")))
	must(t, os.WriteFile(filepath.Join(inst, "schedule/daily/afile"), nil, 0o644))

	for _, c := range []struct{ name, inst, rel string }{
		{"climbing out", inst, "schedule/../runtime/state"},
		{"climbing out in the middle", inst, "schedule/daily/../../elsewhere/foo"},
		{"not under schedule/", inst, "runtime/state"},
		{"another folder of the instance", inst, "elsewhere/foo"},
		{"an absolute path", inst, filepath.Join(inst, "schedule/daily/foo")},
		{"schedule itself", inst, "schedule"},
		{"schedule with a slash", inst, "schedule/"},
		{"an unclean path", inst, "schedule//daily/foo"},
		{"a trailing slash", inst, "schedule/daily/foo/"},
		{"an empty path", inst, ""},
		{"a symlinked item", inst, "schedule/daily/link"},
		{"a file", inst, "schedule/daily/afile"},
		{"a folder above the item that links out", inst, "schedule/weekly/victim"},
		{"a schedule/ that is a link", other, "schedule/daily/foo"},
	} {
		for _, pause := range []bool{true, false} {
			rel := c.rel
			if !pause {
				rel += ".paused"
			}
			_, err := RenameItem(context.Background(), c.inst, rel, pause, noWait)
			// Pausing the path as given must be refused as outside; the unpause
			// spelling of a path that has no such folder may also be "gone".
			if pause && !errors.Is(err, ErrItemOutside) || !pause && err == nil {
				t.Errorf("%s (pause=%v): err %v, want a refusal", c.name, pause, err)
			}
		}
	}
	// Nothing outside moved, and nothing inside changed.
	for _, p := range []string{filepath.Join(outside, "victim"), filepath.Join(inst, "schedule/daily/foo"), filepath.Join(inst, "runtime/state"), filepath.Join(inst, "elsewhere/foo")} {
		if !isDir(p) {
			t.Errorf("%s moved", p)
		}
	}
	if _, err := os.Lstat(filepath.Join(outside, "victim.paused")); err == nil {
		t.Error("a folder outside the instance was renamed")
	}
	entries, _ := os.ReadDir(filepath.Join(inst, "schedule/daily"))
	for _, e := range entries {
		if strings.HasSuffix(e.Name(), ".paused") {
			t.Errorf("%s appeared in schedule/daily", e.Name())
		}
	}
}

func must(t *testing.T, err error) {
	t.Helper()
	if err != nil {
		t.Fatal(err)
	}
}

func TestRenameItemRefusesWhenTheTargetExistsOrTheItemIsInTheWrongState(t *testing.T) {
	inst := instanceDir(t, "schedule/daily/foo", "schedule/daily/foo.paused", "schedule/daily/solo", "schedule/daily/slept.paused")
	for _, c := range []struct {
		rel   string
		pause bool
		want  error
	}{
		{"schedule/daily/foo", true, ErrItemExists},
		{"schedule/daily/foo.paused", false, ErrItemExists},
		{"schedule/daily/solo.paused", false, ErrItemState}, // gone
		{"schedule/daily/solo", false, ErrItemState},        // not paused by name
		{"schedule/daily/slept.paused", true, ErrItemState}, // already paused
		{"schedule/daily/missing", true, ErrItemState},
	} {
		_, err := RenameItem(context.Background(), inst, c.rel, c.pause, noWait)
		if !errors.Is(err, c.want) {
			t.Errorf("%s pause=%v: %v, want %v", c.rel, c.pause, err, c.want)
		}
	}
	for _, p := range []string{"foo", "foo.paused", "solo", "slept.paused"} {
		if !isDir(filepath.Join(inst, "schedule/daily", p)) {
			t.Errorf("%s changed", p)
		}
	}
	// The reader's notice names the folder, never an absolute path.
	_, err := RenameItem(context.Background(), inst, "schedule/daily/foo", true, noWait)
	if strings.Contains(err.Error(), inst) {
		t.Errorf("error carries an absolute path: %v", err)
	}
}

// holdMutationLock takes the instance's mutation lock the way Groundhog does (a
// flock(2) on runtime/locks/mutation.lock) and returns its release.
func holdMutationLock(t *testing.T, inst string) (release func()) {
	t.Helper()
	dir := filepath.Join(inst, "runtime", "locks")
	must(t, os.MkdirAll(dir, 0o755))
	f, err := os.OpenFile(filepath.Join(dir, "mutation.lock"), os.O_CREATE|os.O_APPEND|os.O_WRONLY, 0o644)
	must(t, err)
	must(t, syscall.Flock(int(f.Fd()), syscall.LOCK_EX|syscall.LOCK_NB))
	var once bool
	release = func() {
		if !once {
			once = true
			_ = syscall.Flock(int(f.Fd()), syscall.LOCK_UN)
			_ = f.Close()
		}
	}
	t.Cleanup(release)
	return release
}

func TestRenameItemWaitsForTheMutationLockAndGivesUpWhenItStaysHeld(t *testing.T) {
	inst := instanceDir(t, "schedule/daily/foo")
	release := holdMutationLock(t, inst)

	// Held for the whole wait: nothing is renamed.
	waits := 0
	_, err := RenameItem(context.Background(), inst, "schedule/daily/foo", true, func(context.Context, time.Duration) error { waits++; return nil })
	if !errors.Is(err, ErrItemBusy) || waits != lockTries-1 {
		t.Fatalf("err %v after %d waits, want ErrItemBusy after %d", err, waits, lockTries-1)
	}
	if !isDir(filepath.Join(inst, "schedule/daily/foo")) {
		t.Fatal("renamed while another writer held the lock")
	}

	// Released during the wait: the rename goes ahead, under the lock.
	res, err := RenameItem(context.Background(), inst, "schedule/daily/foo", true, func(context.Context, time.Duration) error { release(); return nil })
	if err != nil || !res.Locked {
		t.Fatalf("err %v, result %+v", err, res)
	}
	if !isDir(filepath.Join(inst, "schedule/daily/foo.paused")) {
		t.Fatal("not renamed once the lock was free")
	}
	// The lock is let go again afterwards: another writer can take it.
	again := holdMutationLock(t, inst)
	again()
}

func TestRenameItemStopsWaitingWhenTheContextEnds(t *testing.T) {
	inst := instanceDir(t, "schedule/daily/foo")
	holdMutationLock(t, inst)
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	if _, err := RenameItem(ctx, inst, "schedule/daily/foo", true, sleepCtx); !errors.Is(err, context.Canceled) {
		t.Fatalf("err %v", err)
	}
	if !isDir(filepath.Join(inst, "schedule/daily/foo")) {
		t.Fatal("renamed")
	}
}

func TestRenameItemRechecksTheTargetUnderTheLock(t *testing.T) {
	inst := instanceDir(t, "schedule/daily/foo")
	release := holdMutationLock(t, inst)
	// While it waits, someone makes the target name.
	_, err := RenameItem(context.Background(), inst, "schedule/daily/foo", true, func(context.Context, time.Duration) error {
		release()
		return os.Mkdir(filepath.Join(inst, "schedule/daily/foo.paused"), 0o755)
	})
	if !errors.Is(err, ErrItemExists) {
		t.Fatalf("err %v", err)
	}
	if !isDir(filepath.Join(inst, "schedule/daily/foo")) {
		t.Fatal("the original moved")
	}
}

func noWait(context.Context, time.Duration) error { return nil }

func mustReal(t *testing.T, p string) string {
	t.Helper()
	real, err := filepath.EvalSymlinks(p)
	if err != nil {
		t.Fatal(err)
	}
	return real
}
