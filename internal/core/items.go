package core

import (
	"context"
	"errors"
	"fmt"
	"os"
	"path"
	"path/filepath"
	"slices"
	"strings"
	"syscall"
	"time"

	"github.com/zealtv/easyhog/internal/groundhog"
)

// Pausing and unpausing one schedule item is Groundhog's own toggle: rename the
// item's folder to add or remove a `.paused` suffix (its README, Rule 6). It is
// the only write Easyhog makes to an instance's schedule/, and it goes through
// no Groundhog command, because the protocol has none.

const (
	// PausedSuffix is what Groundhog reads as "this path component is paused".
	PausedSuffix = ".paused"

	// ActionPauseItem and ActionUnpauseItem act on one schedule item, not on the
	// instance's runner. They are the item's own offers, as the settling actions
	// are (OffersOn), and ItemOffers is their rule.
	ActionPauseItem   Action = "pause-item"
	ActionUnpauseItem Action = "unpause-item"
)

// Items are Easyhog's to rename only within one instance's schedule/, and never
// onto a name that exists. Each refusal names itself so the reader's notice can.
var (
	// ErrItemOutside: the path is not an item folder inside the instance's own
	// schedule/ (it climbs out, goes through a symlink out, or is not a directory).
	ErrItemOutside = errors.New("core: not an item folder inside the instance's schedule/")
	// ErrItemExists: the name the rename would produce is already taken.
	ErrItemExists = errors.New("core: the target name already exists")
	// ErrItemState: the item is not in the state the action needs (already
	// paused, not paused, or gone).
	ErrItemState = errors.New("core: the item is not in the state the action needs")
	// ErrItemBusy: another writer held Groundhog's mutation lock for the whole wait.
	ErrItemBusy = errors.New("core: another writer holds the instance's mutation lock")
)

// ItemIdentity is rel, an item's path ("schedule/daily/foo.paused"), with a
// trailing `.paused` removed from the item's own final component only:
// "schedule/daily/foo". It is what an item is called across a pause and an
// unpause, so the row keeps its place when its folder is renamed. Groundhog's
// `item_id` keeps the suffix; Easyhog drops it here. A component that is only
// ".paused" keeps its name. Folders above the item are left as they are.
func ItemIdentity(rel string) string {
	i := strings.LastIndexByte(rel, '/')
	base := rel[i+1:]
	if len(base) > len(PausedSuffix) {
		base = strings.TrimSuffix(base, PausedSuffix)
	}
	return rel[:i+1] + base
}

// ItemPausedByName reports whether the item's own folder name carries `.paused`.
func ItemPausedByName(rel string) bool {
	base := rel[strings.LastIndexByte(rel, '/')+1:]
	return len(base) > len(PausedSuffix) && strings.HasSuffix(base, PausedSuffix)
}

// ItemClash reports another item of st with the same identity as rel, which is
// to say `foo` beside `foo.paused`: either toggle would collide with it.
func ItemClash(st groundhog.Status, rel string) (other string, clash bool) {
	id := ItemIdentity(rel)
	for _, it := range st.Schedule {
		if it.Path != rel && ItemIdentity(it.Path) == id {
			return it.Path, true
		}
	}
	return "", false
}

// ItemOffers says whether the item at rel (a status schedule path) offers Pause
// item or Unpause item, and when it offers neither, why. It is the one rule: the
// palette lists what it returns, and the supervisor applies it to its own fresh
// read before renaming anything.
//
//   - The instance must be one Easyhog may act on: unowned (stopped), or one of
//     this session's; the same test as Run's (baseOffers), plus a coherent, current
//     status. An instance that something outside Easyhog drives is refused, as the
//     rest of the palette refuses it: its owner, or the loop ticking it, keeps its
//     own schedule. This is the narrowest rule that works; it could widen later.
//   - `foo` and `foo.paused` side by side: neither toggles, since either rename
//     would land on the other.
//   - A paused item (its own folder name says so) offers Unpause; an item not
//     paused by its own name but through a folder above it offers nothing, since
//     the pause is that folder's to lift; any other item offers Pause.
func ItemOffers(iv InstanceView, runner *RunnerInfo, rel string) (actions []Action, why string) {
	if base, reason := baseOffers(iv, runner); len(base) == 0 {
		return nil, reason
	}
	switch {
	case TickedFromOutside(iv):
		return nil, "something outside Easyhog ticked this instance recently, so Easyhog leaves its schedule alone."
	case iv.Health != HealthOK:
		return nil, "its status is not coherent; it will be offered once a coherent read arrives."
	}
	st := iv.Inspection.Last.Status
	var item *groundhog.ScheduleItem
	for i := range st.Schedule {
		if st.Schedule[i].Path == rel {
			item = &st.Schedule[i]
		}
	}
	if item == nil {
		return nil, "the last status read does not list this item."
	}
	if other, clash := ItemClash(st, rel); clash {
		return nil, fmt.Sprintf("both %s and %s exist, and either rename would land on the other; remove or rename one of them first.", path.Base(rel), path.Base(other))
	}
	if ItemPausedByName(rel) {
		return []Action{ActionUnpauseItem}, ""
	}
	for _, by := range item.PausedBy {
		if by != rel {
			return nil, fmt.Sprintf("paused through %s, not by its own name. Easyhog pauses and unpauses items only, so lift that pause where it was set.", by)
		}
	}
	return []Action{ActionPauseItem}, ""
}

// TickedFromOutside reports whether iv is an unowned instance that something
// outside Easyhog ticked recently, which the shell shows as external.
func TickedFromOutside(iv InstanceView) bool {
	if iv.Class != ClassUnowned {
		return false
	}
	v, ok := PendingVerdict(iv)
	return ok && v.Outlook == OutlookProbably && v.Source == "standalone"
}

// ItemResult is what a toggle that went through did.
type ItemResult struct {
	Instance string
	// Item is the item's identity, the same before and after.
	Item string
	// From and To are the folder's path before and after, relative to the instance.
	From, To string
	// Paused is whether the item is paused by name now.
	Paused bool
	// Locked is true when the rename ran under Groundhog's mutation lock. It is
	// false only for an instance whose runtime/locks/mutation.lock does not exist,
	// which no Groundhog writer has ever used.
	Locked bool
}

// lockWait is how long a toggle waits for the mutation lock, as Groundhog's own
// writers wait (about three seconds, in tenth-of-a-second tries).
const (
	lockTries = 30
	lockEvery = 100 * time.Millisecond
)

// RenameItem renames the item folder rel ("schedule/daily/foo") to add (pause)
// or remove (not pause) its `.paused` suffix, inside the instance directory
// inst and nowhere else:
//
//   - rel must be a clean slash path under schedule/ (no "..", not absolute),
//     naming a real directory (not a symlink), whose parent, after resolving
//     symlinks, lies within inst's schedule/, itself within inst. Anything else
//     is ErrItemOutside.
//   - The folder must be in the other state (ErrItemState), and the new name
//     must not exist (ErrItemExists). The check is repeated under the lock.
//   - It takes Groundhog's mutation lock around the rename, the lock every
//     Groundhog writer holds (README, Locks; a flock(2) on runtime/locks/
//     mutation.lock). The runner's tick holds it while it walks schedule/ and
//     copies a due item (`cp -R`), and a rename in the middle of that fails the
//     copy and, for the runner, the tick: it stops with tick_failed. So the
//     rename waits its turn. A lock held for the whole wait is ErrItemBusy; a
//     lock file that does not exist means no writer has run here, and the rename
//     goes ahead without one (ItemResult.Locked says which).
//
// Only the lock can order the rename against a tick; it cannot keep a person at
// a shell from renaming the same folder, which the "target exists" check
// narrows and rename(2) itself cannot close (it replaces an empty directory).
func RenameItem(ctx context.Context, inst, rel string, pause bool, wait func(context.Context, time.Duration) error) (ItemResult, error) {
	res := ItemResult{Item: ItemIdentity(rel), Paused: pause}
	if wait == nil {
		wait = sleepCtx
	}
	src, dst, err := itemPaths(inst, rel, pause)
	if err != nil {
		return res, err
	}
	res.From, res.To = rel, ItemTarget(rel, pause)
	unlock, locked, err := lockMutation(ctx, inst, wait)
	if err != nil {
		return res, err
	}
	defer unlock()
	res.Locked = locked
	// Under the lock, the same checks again on what is there now.
	if src, dst, err = itemPaths(inst, rel, pause); err != nil {
		return res, err
	}
	if err := os.Rename(src, dst); err != nil {
		// A *LinkError carries both absolute paths; the reader's notice names
		// the item by its relative path and the reason alone.
		var le *os.LinkError
		if errors.As(err, &le) {
			err = le.Err
		}
		return res, fmt.Errorf("core: rename %s: %w", rel, err)
	}
	return res, nil
}

// ItemTarget is the path rel becomes when the item is paused or unpaused.
func ItemTarget(rel string, pause bool) string {
	if pause {
		return rel + PausedSuffix
	}
	return strings.TrimSuffix(rel, PausedSuffix)
}

// itemPaths checks rel and returns the folder's absolute path and the one it
// would be renamed to.
func itemPaths(inst, rel string, pause bool) (src, dst string, err error) {
	outside := func(why string) error { return fmt.Errorf("%w: %s", ErrItemOutside, why) }
	clean := path.Clean(rel)
	switch {
	case rel == "" || clean != rel || path.IsAbs(rel) || strings.ContainsRune(rel, 0):
		return "", "", outside("not a clean relative path")
	case !strings.HasPrefix(rel, "schedule/") || strings.Contains("/"+rel+"/", "/../"):
		return "", "", outside("not beneath schedule/")
	}
	sched := filepath.Join(inst, "schedule")
	src = filepath.Join(inst, filepath.FromSlash(rel))
	if !Within(inst, sched) || !Within(sched, filepath.Dir(src)) {
		return "", "", outside("its folder resolves outside the instance's schedule/")
	}
	info, lerr := os.Lstat(src)
	switch {
	case errors.Is(lerr, os.ErrNotExist):
		return "", "", fmt.Errorf("%w: %s is gone", ErrItemState, rel)
	case lerr != nil:
		return "", "", fmt.Errorf("core: look at %s: %w", rel, lerr)
	case !info.IsDir():
		return "", "", outside(rel + " is not a directory (a link or a file)")
	case ItemPausedByName(rel) == pause:
		if pause {
			return "", "", fmt.Errorf("%w: %s is already paused", ErrItemState, rel)
		}
		return "", "", fmt.Errorf("%w: %s is not paused by its own name", ErrItemState, rel)
	}
	dst = filepath.Join(inst, filepath.FromSlash(ItemTarget(rel, pause)))
	if _, lerr := os.Lstat(dst); lerr == nil {
		return "", "", fmt.Errorf("%w: %s already exists beside it", ErrItemExists, path.Base(ItemTarget(rel, pause)))
	} else if !errors.Is(lerr, os.ErrNotExist) {
		return "", "", fmt.Errorf("core: look at the target of %s: %w", rel, lerr)
	}
	return src, dst, nil
}

// lockMutation takes the instance's mutation lock the way Groundhog's writers do
// (a non-blocking flock(2) retried for about three seconds), or reports that no
// lock file exists. unlock is always safe to call.
func lockMutation(ctx context.Context, inst string, wait func(context.Context, time.Duration) error) (unlock func(), locked bool, err error) {
	f, err := os.Open(filepath.Join(inst, "runtime", "locks", "mutation.lock"))
	switch {
	case errors.Is(err, os.ErrNotExist):
		return func() {}, false, nil
	case err != nil:
		return func() {}, false, fmt.Errorf("core: open the mutation lock: %w", err)
	}
	for try := 0; ; try++ {
		err = syscall.Flock(int(f.Fd()), syscall.LOCK_EX|syscall.LOCK_NB)
		switch {
		case err == nil:
			return func() { _ = syscall.Flock(int(f.Fd()), syscall.LOCK_UN); _ = f.Close() }, true, nil
		case errors.Is(err, syscall.EINTR):
			continue
		case !errors.Is(err, syscall.EWOULDBLOCK):
			_ = f.Close()
			return func() {}, false, fmt.Errorf("core: take the mutation lock: %w", err)
		case try+1 >= lockTries:
			_ = f.Close()
			return func() {}, false, ErrItemBusy
		}
		if werr := wait(ctx, lockEvery); werr != nil {
			_ = f.Close()
			return func() {}, false, werr
		}
	}
}

// itemActionOf reports the offer a toggle asks for.
func itemActionOf(pause bool) Action {
	if pause {
		return ActionPauseItem
	}
	return ActionUnpauseItem
}

// SetItemPaused pauses (pause) or unpauses one schedule item of the instance at
// key by renaming its folder (RenameItem), on the instance's operation slot, so
// it never overlaps a Run, Stop or settling action on the same instance. It
// reads a fresh coherent status and applies ItemOffers to it, so the rule the
// palette showed is checked at the protocol; refused, it renames nothing. The
// instance's runner, if this session runs one, keeps running: the rename holds
// the mutation lock for a moment, as any control command does, and the next
// tick sees the new name. The caller re-reads the instance's status afterwards
// (the refresher's Reinspect) so the row shows the change.
func (s *Supervisor) SetItemPaused(ctx context.Context, key, item string, pause bool) (ItemResult, error) {
	op := "unpause-item"
	if pause {
		op = "pause-item"
	}
	res := ItemResult{Instance: key, Item: ItemIdentity(item), Paused: pause}
	ctx, done := s.opContext(ctx)
	defer done()
	sess, err := s.sessionFor(op, key)
	if err != nil {
		return res, err
	}
	if err := sess.acquire(ctx); err != nil {
		return res, err
	}
	defer sess.release()
	if err := s.closingErr(op, key); err != nil {
		return res, err
	}
	st, err := s.readStatus(ctx, sess.client)
	if err != nil {
		return res, refuseIfInconsistent(op, key, err)
	}
	s.mu.Lock()
	held := sess.holds()
	s.mu.Unlock()
	if held {
		s.applyRule(sess, st) // drops a token the status shows no longer controls the instance
	}
	iv, runner := s.freshView(sess, st, nil)
	offered, why := ItemOffers(iv, runner, item)
	switch {
	case slices.Contains(offered, itemActionOf(pause)):
	case iv.Class == ClassExternal || TickedFromOutside(iv):
		return res, &RefusedError{Op: op, Instance: key, Reason: RefusedExternal,
			Detail: "another supervisor owns it, or something outside Easyhog ticks it, so Easyhog leaves its schedule alone"}
	default:
		if why == "" {
			why = "the fresh read offers " + actionList(offered) + " for this item, not " + string(itemActionOf(pause))
		}
		return res, &RefusedError{Op: op, Instance: key, Reason: RefusedNotOffered, Detail: why}
	}
	r, err := RenameItem(ctx, key, item, pause, s.cfg.Sleep)
	r.Instance = key
	return r, err
}

// actionList names actions for a refusal.
func actionList(as []Action) string {
	names := make([]string, len(as))
	for i, a := range as {
		names[i] = string(a)
	}
	return strings.Join(names, ", ")
}
