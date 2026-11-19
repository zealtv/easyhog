package tui

import (
	"errors"
	"strings"
	"testing"

	tea "github.com/charmbracelet/bubbletea"
	"github.com/charmbracelet/x/ansi"

	"github.com/zealtv/easyhog/internal/core"
	"github.com/zealtv/easyhog/internal/discovery"
	"github.com/zealtv/easyhog/internal/groundhog"
)

// Pause item and Unpause item: rename one item's folder; the
// row's identity ignores `.paused`. Synthetic instances only.

const (
	foo       = "schedule/daily/foo"
	fooPaused = "schedule/daily/foo.paused"
)

func plainItem(path string) groundhog.ScheduleItem {
	return groundhog.ScheduleItem{Path: path, ItemID: path[strings.LastIndex(path, "/")+1:],
		Cadence: &groundhog.Cadence{Axis: "daily", Time: strp("09:00")}}
}

// ownPause marks an item as paused by its own folder name, as the script reports it.
func ownPause(it groundhog.ScheduleItem) groundhog.ScheduleItem {
	it.PausedBy, it.BlockedBy = []string{it.Path}, []string{"subtree_paused"}
	return it
}

func withItems(v core.InstanceView, items ...groundhog.ScheduleItem) core.InstanceView {
	st := v.Inspection.Last.Status
	st.Schedule = items
	v.Inspection.Last = obs(st)
	return v
}

func itemNodes(t *testing.T, v core.InstanceView) []Node {
	t.Helper()
	return FromViewWith(core.View{Root: "/r", Instances: []core.InstanceView{v}}, lookup(nil)).Nodes[0].Children
}

func TestAnItemKeepsItsKeyLabelAndRowAcrossAToggle(t *testing.T) {
	plain := itemNodes(t, withItems(unowned("a"), plainItem(foo)))[0]
	paused := itemNodes(t, withItems(unowned("a"), ownPause(plainItem(fooPaused))))[0]
	if plain.Key != paused.Key || plain.Key != "/r/a/.groundhog#schedule/daily/foo" {
		t.Errorf("keys %q and %q", plain.Key, paused.Key)
	}
	if plain.Label != "foo" || paused.Label != "foo" {
		t.Errorf("labels %q and %q: the suffix is the state word's to say", plain.Label, paused.Label)
	}
	if plain.State != StateScheduled || paused.State != StatePaused || plain.State.label() != "scheduled" || paused.State.label() != "paused" {
		t.Errorf("states %v and %v", plain.State, paused.State)
	}
	// The folder the preview and the payload read stays the real one.
	if paused.Payload.Rel != fooPaused || plain.Payload.Rel != foo {
		t.Errorf("payload %+v and %+v", plain.Payload, paused.Payload)
	}
	if !strings.Contains(paused.Preview, "Item "+fooPaused) || !strings.Contains(paused.Preview, "Item id: foo.paused") {
		t.Errorf("preview lost the real folder or item_id:\n%s", paused.Preview)
	}
}

func TestSelectionStaysOnTheItemWhenItsFolderIsRenamed(t *testing.T) {
	f := &fakeRunners{}
	r := newRig(t, f)
	r.m = NewWithData(r.data(withItems(unowned("alpha"), plainItem("schedule/daily/a"), plainItem(foo), plainItem("schedule/daily/z"))),
		WithRunners(f, nil))
	r.send(tea.WindowSizeMsg{Width: 110, Height: 34}, key(tea.KeyRight), key(tea.KeyDown), key(tea.KeyDown))
	const want = "/r/alpha/.groundhog#schedule/daily/foo"
	if r.m.selectedKey() != want {
		t.Fatalf("selected %q", r.m.selectedKey())
	}
	for _, v := range []core.InstanceView{
		withItems(unowned("alpha"), plainItem("schedule/daily/a"), ownPause(plainItem(fooPaused)), plainItem("schedule/daily/z")),
		withItems(unowned("alpha"), plainItem("schedule/daily/a"), plainItem(foo), plainItem("schedule/daily/z")),
	} {
		r.send(SnapshotMsg{Data: r.data(v)})
		if r.m.selectedKey() != want {
			t.Fatalf("the selection moved to %q", r.m.selectedKey())
		}
	}
}

func TestDeliveriesAndSettlingFollowTheItemAcrossAToggle(t *testing.T) {
	// A delivery fired under the unpaused path still belongs to the item once its
	// folder reads foo.paused, and a settling action asks for that path.
	v := withDeliveries(ours("a", groundhog.RunnerRunning, false), failedA)
	st := v.Inspection.Last.Status
	st.Schedule = []groundhog.ScheduleItem{ownPause(plainItem("schedule/daily/a.paused"))}
	v.Inspection.Last = obs(st)
	n := itemNodes(t, v)[0]
	if n.State != StateFailed || !hasAction(n, ActionRetryFailed) || n.ActionNotes[ActionRetryFailed] != "1 failed" || !strings.Contains(n.Preview, "Delivery:") {
		t.Fatalf("state %v, actions %v %v\n%s", n.State, n.Actions, n.ActionNotes, n.Preview)
	}

	f := &fakeRunners{}
	r := settleRig(t, f, v)
	r.send(key(tea.KeyRight), key(tea.KeyDown))
	msgs := r.send(append(typeText(":retry"), key(tea.KeyEnter))...)
	r.send(msgs...)
	if len(f.settles) != 1 || f.settles[0].req.Item != "schedule/daily/a" {
		t.Fatalf("settle calls %+v", f.settles)
	}
}

func TestOnlyTheItemsOwnFolderNameOffersAToggle(t *testing.T) {
	const above = "schedule/weekly.paused/mon/review"
	review := plainItem(above)
	review.PausedBy = []string{"schedule/weekly.paused"}
	for _, c := range []struct {
		name  string
		v     core.InstanceView
		want  []Action
		label string
		in    string // the preview says this
	}{
		{"scheduled, unowned", withItems(unowned("a"), plainItem(foo)), []Action{ActionPauseItem}, "foo", "Palette offers: Pause item (renames its folder to foo.paused)"},
		{"scheduled, ours", withItems(ours("a", groundhog.RunnerRunning, false), plainItem(foo)), []Action{ActionPauseItem}, "foo", ""},
		{"paused by name", withItems(unowned("a"), ownPause(plainItem(fooPaused))), []Action{ActionUnpauseItem}, "foo", "Palette offers: Unpause item (renames its folder to foo)"},
		{"paused through a folder above", withItems(ours("a", groundhog.RunnerStopped, false), review), nil, "review",
			"Pausing is not offered: paused through schedule/weekly.paused, not by its own name"},
		{"external", withItems(iv("a", core.ClassExternal, core.HealthOK, core.Inspection{Last: obs(okStatus())}), plainItem(foo)), nil, "foo", "not offered: another supervisor owns it"},
		{"not inspected: listed only", unlisted("a", "daily/foo"), nil, "foo", ""},
	} {
		kids := itemNodes(t, c.v)
		if len(kids) != 1 {
			t.Fatalf("%s: %d items", c.name, len(kids))
		}
		k := kids[0]
		var got []Action
		for _, a := range k.Actions {
			if a.togglesItem() {
				got = append(got, a)
			}
		}
		if len(got) != len(c.want) || (len(got) == 1 && got[0] != c.want[0]) {
			t.Errorf("%s: offers %v, want %v", c.name, got, c.want)
		}
		if k.Label != c.label {
			t.Errorf("%s: label %q", c.name, k.Label)
		}
		if !strings.Contains(k.Preview, c.in) {
			t.Errorf("%s: preview lacks %q:\n%s", c.name, c.in, k.Preview)
		}
	}
	// Nothing is supervised: no offer.
	for _, k := range FromView(core.View{Instances: []core.InstanceView{withItems(unowned("a"), plainItem(foo))}}).Nodes[0].Children {
		if len(k.Actions) != 0 {
			t.Errorf("without a supervisor an item offers %v", k.Actions)
		}
	}
}

func unlisted(name string, paths ...string) core.InstanceView {
	v := iv(name, core.ClassNotInspected, core.HealthOK, core.Inspection{})
	for _, p := range paths {
		v.Files.Schedule = append(v.Files.Schedule, discovery.ScheduleDir{Path: p, Leaf: true, Paused: strings.HasSuffix(p, ".paused")})
	}
	return v
}

func TestFooBesideFooPausedShowsBothRowsAndOffersNeither(t *testing.T) {
	v := withItems(ours("a", groundhog.RunnerStopped, false), plainItem(foo), ownPause(plainItem(fooPaused)))
	kids := itemNodes(t, v)
	if len(kids) != 2 {
		t.Fatalf("%d rows", len(kids))
	}
	if kids[0].Key == kids[1].Key {
		t.Fatalf("both rows share the key %q", kids[0].Key)
	}
	if kids[0].Label != "foo" || kids[1].Label != "foo.paused" {
		t.Errorf("labels %q and %q: the pair needs two names", kids[0].Label, kids[1].Label)
	}
	if kids[0].State != StateScheduled || kids[1].State != StatePaused {
		t.Errorf("states %v and %v", kids[0].State, kids[1].State)
	}
	for _, k := range kids {
		for _, a := range k.Actions {
			if a.togglesItem() {
				t.Errorf("%s offers %s", k.Label, a)
			}
		}
		if !strings.Contains(k.Preview, "Pausing is not offered: both") {
			t.Errorf("%s: the preview does not say why:\n%s", k.Label, k.Preview)
		}
	}
	// The same pair in the listing form.
	listed := itemNodes(t, unlisted("a", "daily/foo", "daily/foo.paused"))
	if len(listed) != 2 || listed[0].Key == listed[1].Key || listed[1].Label != "foo.paused" {
		t.Errorf("listed rows %+v", listed)
	}
	// Selecting the paused row of a pair works: its key is its own.
	m := NewWithData(FromViewWith(core.View{Root: "/r", Instances: []core.InstanceView{v}}, lookup(nil)), WithRunners(&fakeRunners{}, nil))
	m, _ = feed(m, tea.WindowSizeMsg{Width: 100, Height: 30}, key(tea.KeyRight), key(tea.KeyDown), key(tea.KeyDown))
	if m.selectedKey() != kids[1].Key {
		t.Errorf("selected %q, want %q", m.selectedKey(), kids[1].Key)
	}
}

func TestListedItemsKeyTheSameWayBeforeAndAfterTheStatusArrives(t *testing.T) {
	before := itemNodes(t, unlisted("a", "daily/foo.paused"))[0]
	after := itemNodes(t, withItems(unowned("a"), ownPause(plainItem(fooPaused))))[0]
	if before.Key != after.Key || before.Label != "foo" {
		t.Errorf("listed %q %q, read %q", before.Key, before.Label, after.Key)
	}
}

// toggleRig shows one instance with the item `daily/foo` (and `daily/b`) selected.
func toggleRig(t *testing.T, f *fakeRunners, v core.InstanceView) *rig {
	t.Helper()
	r := newRig(t, f)
	r.m = NewWithData(r.data(v), WithRunners(f, func(k string) { r.mu.Lock(); r.reread = append(r.reread, k); r.mu.Unlock() }))
	r.send(tea.WindowSizeMsg{Width: 110, Height: 34}, key(tea.KeyRight), key(tea.KeyDown))
	return r
}

func TestPaletteListsAndRunsPauseItemAndUnpauseItem(t *testing.T) {
	f := &fakeRunners{release: make(chan struct{})}
	r := toggleRig(t, f, withItems(unowned("alpha"), plainItem(foo)))
	if r.names() != "Run,Pause item,Open folder,Refresh,Quit" {
		t.Fatalf("a scheduled item offers %s", r.names())
	}
	r.send(typeText(":pause")...)
	if !strings.Contains(r.screen(), "Pause item") || !strings.Contains(r.screen(), "→ foo.paused") {
		t.Fatalf("the palette row lacks its note:\n%s", r.screen())
	}
	next, cmd := r.m.Update(key(tea.KeyEnter))
	r.m = next.(Model)
	if cmd == nil {
		t.Fatal("nothing scheduled")
	}
	if len(f.toggles) != 0 {
		t.Fatal("it ran on the update loop")
	}
	if !strings.Contains(r.screen(), "Pausing alpha…") {
		t.Fatalf("no transitional state:\n%s", r.screen())
	}
	if r.names() != "Open folder,Refresh,Quit" {
		t.Fatalf("while it runs the palette offers %s", r.names())
	}
	done := make(chan tea.Msg, 1)
	go func() { done <- cmd() }()
	close(f.release)
	r.send(<-done)
	if len(f.toggles) != 1 || f.toggles[0] != (toggleCall{"/r/alpha/.groundhog", foo, true}) {
		t.Fatalf("calls %+v", f.toggles)
	}
	want := "Pause item: alpha: schedule/daily/foo paused: its folder foo was renamed to foo.paused, so no tick fires it; Unpause item renames it back"
	if r.m.notice != want || r.m.failure {
		t.Fatalf("notice %q (failure %v)", r.m.notice, r.m.failure)
	}
	if len(r.reread) != 1 || r.reread[0] != "/r/alpha/.groundhog" {
		t.Fatalf("reread %v", r.reread)
	}

	// The re-read arrives: the same row, now paused, offers Unpause item and not Pause item.
	r.send(SnapshotMsg{Data: r.data(withItems(unowned("alpha"), ownPause(plainItem(fooPaused))))})
	if r.m.selectedKey() != "/r/alpha/.groundhog#schedule/daily/foo" || r.names() != "Run,Unpause item,Open folder,Refresh,Quit" {
		t.Fatalf("selected %q, offers %s", r.m.selectedKey(), r.names())
	}
	f2 := &fakeRunners{}
	r.m.runners = f2
	r.m.handler = runnersHandler(func(ActionRequest) tea.Cmd { return nil }, f2, nil)
	msgs := r.send(append(typeText(":unpause"), key(tea.KeyEnter))...)
	r.send(msgs...)
	if len(f2.toggles) != 1 || f2.toggles[0] != (toggleCall{"/r/alpha/.groundhog", fooPaused, false}) {
		t.Fatalf("calls %+v", f2.toggles)
	}
	want = "Unpause item: alpha: schedule/daily/foo unpaused: its folder foo.paused was renamed to foo; it fires again at its next time unless a folder above it, or the instance, is still paused"
	if r.m.notice != want {
		t.Fatalf("notice %q", r.m.notice)
	}
}

func TestToggleActionsMatchOnlyTheNamePrefix(t *testing.T) {
	cases := []struct {
		v     core.InstanceView
		query string
		want  Action // "" finds neither
	}{
		{withItems(unowned("a"), plainItem(foo)), "p", ActionPauseItem},
		{withItems(unowned("a"), plainItem(foo)), "pa", ActionPauseItem},
		{withItems(unowned("a"), plainItem(foo)), "pause", ActionPauseItem},
		{withItems(unowned("a"), plainItem(foo)), "PAUSE ITEM", ActionPauseItem},
		{withItems(unowned("a"), plainItem(foo)), "pause i", ActionPauseItem},
		{withItems(unowned("a"), plainItem(foo)), "u", ""},
		{withItems(unowned("a"), plainItem(foo)), "unpause", ""},
		{withItems(unowned("a"), ownPause(plainItem(fooPaused))), "u", ActionUnpauseItem},
		{withItems(unowned("a"), ownPause(plainItem(fooPaused))), "un", ActionUnpauseItem},
		{withItems(unowned("a"), ownPause(plainItem(fooPaused))), "unpause item", ActionUnpauseItem},
		{withItems(unowned("a"), ownPause(plainItem(fooPaused))), "pause", ""},
		{withItems(unowned("a"), ownPause(plainItem(fooPaused))), "p", ""},
		// No aliases, no scattered letters, no other words.
		{withItems(unowned("a"), plainItem(foo)), "hold", ""},
		{withItems(unowned("a"), plainItem(foo)), "suspend", ""},
		{withItems(unowned("a"), plainItem(foo)), "pse", ""},
		{withItems(unowned("a"), plainItem(foo)), "item", ""},
		{withItems(unowned("a"), plainItem(foo)), "disable", ""},
		{withItems(unowned("a"), ownPause(plainItem(fooPaused))), "resume", ""},
		{withItems(unowned("a"), ownPause(plainItem(fooPaused))), "enable", ""},
		{withItems(unowned("a"), ownPause(plainItem(fooPaused))), "upi", ""},
	}
	for _, c := range cases {
		r := toggleRig(t, &fakeRunners{}, c.v)
		mm, _ := feed(r.m, typeText(":"+c.query)...)
		var got Action
		for _, m := range mm.filtered() {
			if m.action.togglesItem() {
				got = m.action
			}
		}
		if got != c.want {
			t.Errorf("query %q finds %q, want %q", c.query, got, c.want)
		}
	}
	// ":r" and ":re" stay Refresh and ":p" never reaches Run.
	r := toggleRig(t, &fakeRunners{}, withItems(unowned("a"), plainItem(foo)))
	r.send(append(typeText(":re"), key(tea.KeyEnter))...)
	if len(r.f.toggles) != 0 {
		t.Errorf("a toggle ran: %+v", r.f.toggles)
	}
}

func TestSentencesAndBareKeysNeverReachPauseItem(t *testing.T) {
	for _, sentence := range []string{
		"so: hold that", "see: unpause all", "x: resume", "hey: please pause this",
		"p", "u", "pause item", "unpause item", " pause", "so: suspend", "y: paused", "z: unpaused", "ok: pausing", "so: pause the report",
	} {
		for _, v := range []core.InstanceView{
			withItems(unowned("alpha"), plainItem(foo)),
			withItems(unowned("alpha"), ownPause(plainItem(fooPaused))),
		} {
			f := &fakeRunners{}
			r := toggleRig(t, f, v)
			// Outside the palette no key, Space, Enter or click starts anything.
			msgs := r.send(append(typeText(sentence), key(tea.KeyEnter), key(tea.KeySpace))...)
			r.send(msgs...)
			r.send(tea.MouseMsg{X: 10, Y: 5, Button: tea.MouseButtonLeft, Action: tea.MouseActionPress})
			if len(f.toggles) != 0 {
				t.Fatalf("%q reached a toggle: %+v", sentence, f.toggles)
			}
		}
	}
	// What a sentence can still reach is what it can reach for Run and Quit: an
	// exact name prefix after a colon, which opens the palette there and runs on
	// Enter.
	f := &fakeRunners{}
	r := toggleRig(t, f, withItems(unowned("alpha"), plainItem(foo)))
	msgs := r.send(append(typeText("note: pause"), key(tea.KeyEnter))...)
	r.send(msgs...)
	if len(f.toggles) != 1 {
		t.Fatalf("an exact name after a colon: %+v", f.toggles)
	}
}

func TestToggleRunsOnlyOnTheRowAndOfferThePaletteOpenedOn(t *testing.T) {
	two := withItems(unowned("alpha"), plainItem("schedule/daily/a"), plainItem(foo))
	f := &fakeRunners{}
	r := toggleRig(t, f, two) // selected: a
	r.send(typeText(":pause")...)
	// Behind the open palette a snapshot moves the selection (a is gone).
	r.send(SnapshotMsg{Data: r.data(withItems(unowned("alpha"), plainItem(foo)))})
	r.send(key(tea.KeyEnter))
	if len(f.toggles) != 0 {
		t.Fatalf("it ran on another row: %+v", f.toggles)
	}
	if !r.m.failure || !strings.Contains(r.m.notice, "Nothing ran") {
		t.Errorf("no visible refusal: %q", r.m.notice)
	}

	// The item was paused elsewhere meanwhile: Pause item is no longer offered.
	f = &fakeRunners{}
	r = toggleRig(t, f, withItems(unowned("alpha"), plainItem(foo)))
	r.send(typeText(":pause")...)
	r.send(SnapshotMsg{Data: r.data(withItems(unowned("alpha"), ownPause(plainItem(fooPaused))))})
	r.send(key(tea.KeyEnter))
	if len(f.toggles) != 0 || !r.m.failure || !strings.Contains(r.m.notice, "Nothing ran: Pause item is no longer offered") {
		t.Fatalf("calls %+v, notice %q", f.toggles, r.m.notice)
	}

	// Pause item is an offer of the selected item, not of its instance: the
	// instance row does not list it.
	r = toggleRig(t, &fakeRunners{}, withItems(unowned("alpha"), plainItem(foo)))
	r.send(key(tea.KeyUp))
	if strings.Contains(r.names(), "Pause item") {
		t.Errorf("the instance row offers %s", r.names())
	}
}

func TestToggleFailuresAreWordedWithoutAbsolutePaths(t *testing.T) {
	const l = "alpha/.groundhog"
	for _, c := range []struct {
		err  error
		want string
	}{
		{errors.Join(core.ErrItemBusy), "alpha/.groundhog: nothing was renamed: another operation holds the instance's lock; try again shortly"},
		{errorsWrap(core.ErrItemExists, "foo.paused already exists beside it"), "alpha/.groundhog: nothing was renamed: foo.paused already exists beside it"},
		{errorsWrap(core.ErrItemOutside, "not beneath schedule/"), "alpha/.groundhog: nothing was renamed: not beneath schedule/"},
		{errorsWrap(core.ErrItemState, "schedule/daily/foo is already paused"), "alpha/.groundhog: nothing was renamed: schedule/daily/foo is already paused"},
		{&core.RefusedError{Op: "pause-item", Instance: "/r/alpha/.groundhog", Reason: core.RefusedExternal, Detail: "another supervisor owns it"},
			"alpha/.groundhog: refused: owned by another supervisor: another supervisor owns it"},
	} {
		got := ActionFailure(l, c.err)
		if got == nil || got.Error() != c.want {
			t.Errorf("%v -> %v, want %q", c.err, got, c.want)
		}
	}
}

func errorsWrap(sentinel error, detail string) error { return &wrapped{sentinel, detail} }

type wrapped struct {
	sentinel error
	detail   string
}

func (w *wrapped) Error() string { return w.sentinel.Error() + ": " + w.detail }
func (w *wrapped) Unwrap() error { return w.sentinel }

func TestToggleRefusalShowsAsAFailureAndRereads(t *testing.T) {
	f := &fakeRunners{toggle: func(key, item string, pause bool) (core.ItemResult, error) {
		return core.ItemResult{}, &core.RefusedError{Op: "pause-item", Instance: key, Reason: core.RefusedNotOffered, Detail: "paused through schedule/weekly.paused, not by its own name"}
	}}
	r := toggleRig(t, f, withItems(unowned("alpha"), plainItem(foo)))
	msgs := r.send(append(typeText(":pause"), key(tea.KeyEnter))...)
	r.send(msgs...)
	if !r.m.failure || !strings.Contains(r.m.notice, "refused: not offered: paused through") || len(r.reread) != 1 || len(r.m.busy) != 0 {
		t.Fatalf("notice %q failure %v reread %v busy %v", r.m.notice, r.m.failure, r.reread, r.m.busy)
	}
}

// The rows the reader scans: the glyph and word for each item state stay unique,
// and the paused item row shows its own state.
func TestPausedAndScheduledItemRowsShowTheirOwnWordAndGlyph(t *testing.T) {
	v := withItems(unowned("alpha"), plainItem("schedule/daily/a"), ownPause(plainItem("schedule/daily/b.paused")))
	m := NewWithData(FromViewWith(core.View{Root: "/r", Instances: []core.InstanceView{v}}, lookup(nil)), WithRunners(&fakeRunners{}, nil))
	m, _ = feed(m, tea.WindowSizeMsg{Width: 100, Height: 30}, key(tea.KeyRight))
	out := ansi.Strip(m.View())
	for _, want := range []string{"◷ a", "scheduled", "◐ b", "paused"} {
		if !strings.Contains(out, want) {
			t.Errorf("tree lacks %q:\n%s", want, out)
		}
	}
	if strings.Contains(out, "b.paused") {
		t.Errorf("the label repeats the suffix:\n%s", out)
	}
}
