package tui

import (
	"errors"
	"slices"
	"strings"
	"sync"
	"testing"

	tea "github.com/charmbracelet/bubbletea"
	"github.com/charmbracelet/x/ansi"

	"github.com/zealtv/easyhog/internal/core"
	"github.com/zealtv/easyhog/internal/discovery"
	"github.com/zealtv/easyhog/internal/groundhog"
)

func sp(s string) *string { return &s }

// inspected builds one inspected instance with three items: one due now with a
// failed delivery, one paused by an inherited folder, and one quiet.
func inspected() core.InstanceView {
	st := okStatus()
	st.Owner = &groundhog.Owner{ID: "o-9", Label: "other-supervisor"}
	st.Schedule = []groundhog.ScheduleItem{
		{Path: "schedule/daily/09/report", ItemID: "report", Cadence: &groundhog.Cadence{Axis: "daily", Time: sp("09:00")},
			EligibleNow: true, FiringAllowed: true},
		{Path: "schedule/weekly.paused/mon/review", ItemID: "review", Cadence: &groundhog.Cadence{Axis: "weekly", Day: sp("mon"), Defaulted: true},
			PausedBy: []string{"schedule/weekly.paused"}, BlockedBy: []string{"subtree_paused", "not_due"}},
		{Path: "schedule/every/15m/beat", ItemID: "beat", Cadence: &groundhog.Cadence{Axis: "every", Interval: sp("15m")}, BlockedBy: []string{"owned", "mystery"}},
	}
	code := 3
	st.Deliveries = []groundhog.Delivery{
		{Path: "out/report-2026-11-15", SchedulePath: sp("schedule/daily/09/report"), State: groundhog.DeliveryFailed, Reason: sp("consumer exited"),
			Attempts: []groundhog.Attempt{{Outcome: "failed", ExitCode: &code, Error: &groundhog.Problem{Code: "exit", Message: "status 3"}}}},
		{Path: "out/old", State: groundhog.DeliveryAccepted},
		{Path: "out/waiting", State: groundhog.DeliveryPending},
	}
	v := iv("alpha", core.ClassExternal, core.HealthOK, core.Inspection{Last: obs(st)})
	return v
}

func TestInspectedInstanceShowsOwnershipScheduleAndDeliveries(t *testing.T) {
	n := instanceNode(inspected(), nil, false)
	for _, want := range []string{"Ownership: another supervisor, other-supervisor (o-9)", "Pause: not paused", "Runner: stopped",
		"Schedule: 3 items (1 due now, 0 already fired, 1 paused)", "Deliveries: 1 pending, 1 failed, 1 accepted",
		"out/report-2026-11-15: failed (consumer exited); attempt 1 of 1 failed, exit status 3, exit: status 3", "out/waiting: pending"} {
		if !strings.Contains(n.Preview, want) {
			t.Errorf("preview lacks %q:\n%s", want, n.Preview)
		}
	}
	if strings.Contains(n.Preview, "out/old") {
		t.Error("an accepted delivery should be counted, not listed")
	}
	// Its one failed delivery makes the row read failed, though the instance is
	// another supervisor's.
	if n.Badge != "3 items · 1 pending · 1 failed" || n.State != StateFailed {
		t.Errorf("badge %q state %v", n.Badge, n.State)
	}
}

// A status read with --active lists no settled delivery and counts them in
// Settled; the preview and the badge give the same counts as the full read.
func TestActiveReadShowsTheSameDeliveryCountsAsTheFullRead(t *testing.T) {
	full := instanceNode(inspected(), nil, false)

	active := inspected()
	last := *active.Inspection.Last
	last.Status.Deliveries = slices.DeleteFunc(slices.Clone(last.Status.Deliveries), func(d groundhog.Delivery) bool {
		return d.State == groundhog.DeliveryAccepted
	})
	last.Status.Settled = &groundhog.Settled{Accepted: 1}
	active.Inspection.Last = &last
	n := instanceNode(active, nil, false)
	if n.Badge != full.Badge || !strings.Contains(n.Preview, "Deliveries: 1 pending, 1 failed, 1 accepted") {
		t.Errorf("badge %q (want %q), preview:\n%s", n.Badge, full.Badge, n.Preview)
	}

	// The reader is told what an --active read left out, in the instance preview
	// and in each item's preview; a full read and an empty settled count say nothing.
	const itemLine = "Settled deliveries (accepted or missing) are counted, not listed"
	if !strings.Contains(n.Preview, "Settled: 1 accepted, 0 missing (not listed)") {
		t.Errorf("the instance preview does not say what is not listed:\n%s", n.Preview)
	}
	for _, kid := range instanceNode(active, nil, false).Children {
		if !strings.Contains(kid.Preview, itemLine) {
			t.Errorf("item %s: no note that settled deliveries are not listed:\n%s", kid.Label, kid.Preview)
		}
	}
	if strings.Contains(full.Preview, "Settled:") {
		t.Errorf("a full read has no settled line:\n%s", full.Preview)
	}
	for _, kid := range full.Children {
		if strings.Contains(kid.Preview, itemLine) {
			t.Errorf("a full read's item %s carries the settled note", kid.Label)
		}
	}
	none := inspected()
	l0 := *none.Inspection.Last
	l0.Status.Settled = &groundhog.Settled{}
	none.Inspection.Last = &l0
	if z := instanceNode(none, nil, false); strings.Contains(z.Preview, "Settled:") {
		t.Errorf("a zero settled count should say nothing:\n%s", z.Preview)
	}

	// Settled deliveries alone, nothing listed: "none" must not be shown.
	only := inspected()
	l2 := *only.Inspection.Last
	l2.Status.Deliveries = nil
	l2.Status.Settled = &groundhog.Settled{Accepted: 4501, Missing: 2}
	only.Inspection.Last = &l2
	m := instanceNode(only, nil, false)
	if !strings.Contains(m.Preview, "Deliveries: 2 missing, 4501 accepted") || strings.Contains(m.Preview, "Deliveries: none") {
		t.Errorf("preview:\n%s", m.Preview)
	}
	if strings.Contains(m.Badge, "failed") {
		t.Errorf("badge %q: settled missing deliveries are not failed ones", m.Badge)
	}
}

// An --active read counts a settled `missing` delivery and lists none, so no
// item row could turn failed for it while the instance badge counted it as
// failed. A missing delivery is settled for good: no action offers anything for
// it (Retry failed and Recover take failed and uncertain ones). The tree
// therefore calls only failed and uncertain deliveries failed, in the row and in
// the badge alike, and a full read and an --active read of the same instance
// show the same thing. The preview still counts it as missing.
func TestAMissingDeliveryReadsTheSameInTheRowAndTheBadgeWhicheverReadShowedIt(t *testing.T) {
	const beat = "beat"
	build := func(active bool) core.InstanceView {
		v := inspected()
		last := *v.Inspection.Last
		last.Status.Deliveries = []groundhog.Delivery{
			{Path: "out/beat-gone", SchedulePath: sp("schedule/every/15m/beat"), State: groundhog.DeliveryMissing},
			{Path: "out/waiting", State: groundhog.DeliveryPending},
		}
		if active {
			last.Status.Deliveries = last.Status.Deliveries[1:]
			last.Status.Settled = &groundhog.Settled{Missing: 1}
		}
		v.Inspection.Last = &last
		return v
	}
	var badges []string
	for _, active := range []bool{false, true} {
		name := map[bool]string{false: "full read", true: "--active read"}[active]
		n := instanceNode(build(active), nil, false)
		var row *Node
		for i := range n.Children {
			if n.Children[i].Label == beat {
				row = &n.Children[i]
			}
		}
		if row == nil {
			t.Fatalf("%s: no row %q", name, beat)
		}
		if row.State == StateFailed {
			t.Errorf("%s: the row turned failed for a missing delivery", name)
		}
		if strings.Contains(n.Badge, "failed") {
			t.Errorf("%s: the badge %q counts a missing delivery as failed", name, n.Badge)
		}
		if !strings.Contains(n.Preview, "1 missing") {
			t.Errorf("%s: the preview does not count the missing delivery:\n%s", name, n.Preview)
		}
		badges = append(badges, n.Badge)
	}
	if badges[0] != badges[1] {
		t.Errorf("the two reads of one instance badge differently: %q and %q", badges[0], badges[1])
	}

	// A failed and an uncertain delivery still turn the row and the badge, in both reads.
	v := inspected()
	last := *v.Inspection.Last
	last.Status.Deliveries = []groundhog.Delivery{
		{Path: "out/a", SchedulePath: sp("schedule/every/15m/beat"), State: groundhog.DeliveryUncertain},
		{Path: "out/b", State: groundhog.DeliveryFailed},
	}
	last.Status.Settled = &groundhog.Settled{Missing: 3}
	v.Inspection.Last = &last
	n := instanceNode(v, nil, false)
	if !strings.Contains(n.Badge, "2 failed") {
		t.Errorf("badge %q: want 2 failed (failed and uncertain, not the 3 missing)", n.Badge)
	}
	for _, kid := range n.Children {
		if kid.Label == beat && kid.State != StateFailed {
			t.Errorf("the row of an item with an uncertain delivery is %v", kid.State)
		}
	}
}

func TestItemsShowCadenceDueStateAndInheritedPause(t *testing.T) {
	kids := instanceNode(inspected(), nil, false).Children
	if len(kids) != 3 {
		t.Fatalf("%d items", len(kids))
	}
	report, review, beat := kids[0], kids[1], kids[2]
	if report.State != StateFailed || report.Label != "report" || report.Badge != "daily at 09:00 · due now" {
		t.Errorf("report: %+v", report)
	}
	for _, want := range []string{"Cadence: daily at 09:00", "Due: due now", "Delivery: out/report-2026-11-15: failed"} {
		if !strings.Contains(report.Preview, want) {
			t.Errorf("report lacks %q:\n%s", want, report.Preview)
		}
	}
	if review.State != StatePaused {
		t.Errorf("paused item state %v", review.State)
	}
	for _, want := range []string{"Cadence: weekly mon (default slot)", "Due: paused", "Pause: inherited from schedule/weekly.paused",
		"Firing held: a folder or item on its path is paused; not due yet"} {
		if !strings.Contains(review.Preview, want) {
			t.Errorf("review lacks %q:\n%s", want, review.Preview)
		}
	}
	if !strings.Contains(beat.Preview, "Cadence: every 15m") || !strings.Contains(beat.Preview, "an owner is recorded; mystery") {
		t.Errorf("beat:\n%s", beat.Preview)
	}
	own := inspected()
	own.Inspection.Last.Status.Schedule[1].PausedBy = []string{own.Inspection.Last.Status.Schedule[1].Path}
	if got := instanceNode(own, nil, false).Children[1].Preview; !strings.Contains(got, "this item's own name is paused") {
		t.Errorf("own pause:\n%s", got)
	}
	pausedInst := inspected()
	pausedInst.Inspection.Last.Status.Paused = true
	pn := instanceNode(pausedInst, nil, false)
	if !strings.Contains(pn.Preview, "Pause: the instance is paused") || !strings.Contains(pn.Children[2].Preview, "Pause: inherited from the instance") ||
		!strings.Contains(pn.Badge, "paused") {
		t.Errorf("instance pause:\n%s\n%s", pn.Preview, pn.Children[2].Preview)
	}
}

func TestAnUninspectedInstanceShowsOnlyWhatTheListingKnows(t *testing.T) {
	v := iv("fresh", core.ClassNotInspected, core.HealthOK, core.Inspection{})
	v.Files = discovery.Glance{Schedule: []discovery.ScheduleDir{{Path: "weekly.paused", Paused: true}, {Path: "weekly.paused/review", Leaf: true, Paused: true}},
		Out: []string{"a", "b"}, Runtime: true}
	n := instanceNode(v, nil, false)
	for _, banned := range []string{"Ownership", "Pause:", "Runner", "Consumer", "Deliveries", "Revision", "Last inspected"} {
		if strings.Contains(n.Preview, banned) {
			t.Errorf("an uninspected instance shows %q:\n%s", banned, n.Preview)
		}
	}
	for _, want := range []string{"Not inspected: nothing was run", "1 schedule items, 2 entries in out/, runtime/ present"} {
		if !strings.Contains(n.Preview, want) {
			t.Errorf("lacks %q:\n%s", want, n.Preview)
		}
	}
	if n.Badge != "1 item listed" || n.State != StateStopped || len(n.Children) != 1 {
		t.Errorf("%+v", n)
	}
	it := n.Children[0]
	if it.Badge != "listed only" || !strings.Contains(it.Detail, "paused by name") || strings.Contains(it.Preview, "Due:") || strings.Contains(it.Preview, "Cadence:") {
		t.Errorf("listed item %+v", it)
	}
}

func TestLegacyFailingInconsistentAndStaleKeepTheirWordsInThePreview(t *testing.T) {
	good := obs(okStatus())
	bad := okStatus()
	bad.Consistent = false
	cases := map[string]struct {
		v    core.InstanceView
		want []string
	}{
		"failing": {iv("f", core.ClassUnowned, core.HealthOK, core.Inspection{Last: good, Err: errors.New("deadline exceeded"), ErrAt: at}),
			[]string{"Error at 09:30:15: deadline exceeded", "(the last status that could be read)"}},
		"stale": {iv("s", core.ClassUnowned, core.HealthStale, core.Inspection{Last: good}),
			[]string{"Health: stale", "stale: not re-read although it should have been"}},
		"inconsistent": {iv("i", core.ClassUnowned, core.HealthInconsistent, core.Inspection{Last: obs(bad), Attempts: 3}),
			[]string{"inconsistent status after 3 reads", "should not be acted on"}},
		"legacy": {iv("l", core.ClassLegacy, core.HealthOK, core.Inspection{Support: &groundhog.Support{Reason: "no capabilities"}}),
			[]string{"Kind: legacy", "Support: unsupported, read-only: no capabilities"}},
	}
	for name, c := range cases {
		got := instanceNode(c.v, nil, false).Preview
		for _, w := range c.want {
			if !strings.Contains(got, w) {
				t.Errorf("%s lacks %q:\n%s", name, w, got)
			}
		}
	}
}

func TestHostileStatusTextStaysInertInThePreview(t *testing.T) {
	v := inspected()
	v.Inspection.Last.Status.Owner.Label = "evil\x1b[2J\x07label"
	v.Inspection.Last.Status.Deliveries[0].Reason = sp("\x1b]0;pwned\x07boom")
	m := NewWithData(FromView(core.View{Instances: []core.InstanceView{v}}))
	next, _ := m.Update(tea.WindowSizeMsg{Width: 100, Height: 40})
	out := next.(Model).View()
	if strings.Contains(out, "\x1b]0;") || strings.Contains(out, "\x1b[2J") || strings.Contains(out, "\x07") {
		t.Errorf("control sequence reached the frame")
	}
}

// --- payload preview -------------------------------------------------------

type reads struct {
	mu   sync.Mutex
	refs []PayloadRef
	text map[string]string
}

func (r *reads) read(ref PayloadRef) string {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.refs = append(r.refs, ref)
	return r.text[ref.Rel]
}

func payloadModel(t *testing.T, r *reads, w, h int) Model {
	t.Helper()
	m := NewWithData(FromView(core.View{Instances: []core.InstanceView{inspected()}}), WithPayloads(r.read))
	next, _ := m.Update(tea.WindowSizeMsg{Width: w, Height: h})
	return next.(Model)
}

// pump feeds the messages commands produce back into the model.
func pump(m Model, msgs ...tea.Msg) Model {
	queue := msgs
	for len(queue) > 0 {
		msg := queue[0]
		queue = queue[1:]
		next, cmd := m.Update(msg)
		m = next.(Model)
		queue = append(queue, run(cmd)...)
	}
	return m
}

func TestSelectingAnItemReadsItsPayloadAsTextOffTheRenderLoop(t *testing.T) {
	r := &reads{text: map[string]string{"schedule/daily/09/report": "report.md  12 bytes\n    # Daily report\n    hello\x1b[31m"}}
	m := payloadModel(t, r, 100, 40)
	before := m.View()
	_ = m.View()
	if len(r.refs) != 0 || strings.Contains(before, "Payload") {
		t.Fatalf("an instance read payloads: %v", r.refs)
	}
	m = pump(m, key(tea.KeyRight), key(tea.KeyDown)) // open alpha, select its first item
	if len(r.refs) != 1 || r.refs[0].Instance != "/r/alpha/.groundhog" || r.refs[0].Rel != "schedule/daily/09/report" {
		t.Fatalf("reads: %+v", r.refs)
	}
	out := m.View()
	for _, want := range []string{"Payload, read as text (nothing is run)", "# Daily report", "hello·[31m"} {
		if !strings.Contains(out, want) {
			t.Errorf("frame lacks %q:\n%s", want, out)
		}
	}
	if strings.Contains(out, "\x1b[31m") {
		t.Error("payload escape reached the frame")
	}
	_ = m.View()
	if len(r.refs) != 1 {
		t.Errorf("View read the payload again: %d reads", len(r.refs))
	}
}

func TestPayloadShowsReadingUntilItArrivesAndIgnoresAStaleAnswer(t *testing.T) {
	r := &reads{text: map[string]string{"schedule/daily/09/report": "FIRST", "schedule/weekly.paused/mon/review": "SECOND"}}
	m := payloadModel(t, r, 100, 40)
	m, _ = feed(m, key(tea.KeyRight), key(tea.KeyDown))
	if out := m.View(); !strings.Contains(out, "reading…") {
		t.Fatalf("no reading note:\n%s", out)
	}
	// Move on before the first answer comes back.
	m, _ = feed(m, key(tea.KeyDown))
	m = pump(m, PayloadMsg{Key: "/r/alpha/.groundhog#schedule/daily/09/report", Text: "FIRST"})
	if out := m.View(); strings.Contains(out, "FIRST") || !strings.Contains(out, "reading…") {
		t.Fatalf("a stale answer showed:\n%s", out)
	}
	m = pump(m, PayloadMsg{Key: m.selectedKey(), Text: "SECOND"})
	if out := m.View(); !strings.Contains(out, "SECOND") {
		t.Fatalf("the answer for the selection did not show:\n%s", out)
	}
	// Back on the instance: no payload section at all.
	m, _ = feed(m, key(tea.KeyUp), key(tea.KeyUp), key(tea.KeyUp))
	if out := m.View(); strings.Contains(out, "Payload") || strings.Contains(out, "SECOND") {
		t.Fatalf("an instance showed a payload:\n%s", out)
	}
}

func TestASnapshotRereadsTheSelectedItemAndKeepsTheOldTextMeanwhile(t *testing.T) {
	r := &reads{text: map[string]string{"schedule/daily/09/report": "v1"}}
	m := payloadModel(t, r, 100, 40)
	m = pump(m, key(tea.KeyRight), key(tea.KeyDown))
	key0 := m.selectedKey()
	m, cmd := feed(m, SnapshotMsg{FromView(core.View{Instances: []core.InstanceView{inspected()}})})
	if m.selectedKey() != key0 || cmd == nil {
		t.Fatalf("selection %q, cmd %v", m.selectedKey(), cmd)
	}
	if !strings.Contains(m.View(), "v1") {
		t.Error("the old text vanished while the new was being read")
	}
	r.text["schedule/daily/09/report"] = "v2"
	m = pump(m, run(cmd)...)
	if out := m.View(); !strings.Contains(out, "v2") || strings.Contains(out, "v1") || len(r.refs) != 2 {
		t.Fatalf("%d reads:\n%s", len(r.refs), out)
	}
}

func TestAVanishedItemSelectionFallsBackWithoutReading(t *testing.T) {
	r := &reads{text: map[string]string{}}
	m := payloadModel(t, r, 100, 40)
	m = pump(m, key(tea.KeyRight), key(tea.KeyDown), key(tea.KeyDown)) // the second item
	gone := inspected()
	gone.Inspection.Last.Status.Schedule = gone.Inspection.Last.Status.Schedule[:1]
	m = pump(m, SnapshotMsg{FromView(core.View{Instances: []core.InstanceView{gone}})})
	if m.selectedRow() == nil || !strings.HasSuffix(m.selectedKey(), "report") {
		t.Fatalf("selection %q", m.selectedKey())
	}
	if out := m.View(); strings.Contains(out, "weekly") {
		t.Errorf("a vanished item still shows:\n%s", out)
	}
	m = pump(m, SnapshotMsg{FromView(core.View{})})
	if m.selectedRow() != nil || strings.Contains(m.View(), "Payload") {
		t.Errorf("empty list: %q", m.selectedKey())
	}
}

func TestLongPayloadScrollsInThePreview(t *testing.T) {
	var lines []string
	for i := 0; i < 60; i++ {
		lines = append(lines, "    line "+string(rune('A'+i%26))+strings.Repeat("x", i))
	}
	r := &reads{text: map[string]string{"schedule/daily/09/report": strings.Join(lines, "\n")}}
	m := payloadModel(t, r, 80, 24)
	m = pump(m, key(tea.KeyRight), key(tea.KeyDown))
	if m.maxScroll() == 0 {
		t.Fatal("a long payload should scroll")
	}
	m, _ = feed(m, key(tea.KeyTab), key(tea.KeyPgDown), key(tea.KeyPgDown), key(tea.KeyPgDown))
	if m.scroll == 0 || m.scroll > m.maxScroll() {
		t.Errorf("scroll %d of %d", m.scroll, m.maxScroll())
	}
}

// --- tree badges -----------------------------------------------------------

func TestBadgesYieldToTheLabelAsThePaneNarrows(t *testing.T) {
	m := NewWithData(FromView(core.View{Instances: []core.InstanceView{inspected()}}))
	row := func(w int) string {
		next, _ := m.Update(tea.WindowSizeMsg{Width: 200, Height: 30})
		return ansi.Strip(next.(Model).treeRow(0, w))
	}
	wide := row(90)
	if !strings.Contains(wide, "3 items · 1 pending · 1 failed") || !strings.Contains(wide, "failed") {
		t.Errorf("wide row %q", wide)
	}
	mid := row(34)
	if strings.Contains(mid, "pending") || !strings.Contains(mid, "alpha") {
		t.Errorf("mid row %q", mid)
	}
	if narrow := row(18); !strings.Contains(narrow, "alpha") {
		t.Errorf("narrow row %q", narrow)
	}
}

// --- Open folder -----------------------------------------------------------

func TestOpenFolderRunsOnlyThroughThePaletteOnTheBoundInstance(t *testing.T) {
	var opened []string
	open := func(dir string) error { opened = append(opened, dir); return nil }
	m := NewWithData(FromView(viewOf("a", "b")), WithFolderOpener(open))
	for _, k := range []tea.Msg{key(tea.KeyEnter), key(tea.KeySpace), key(tea.KeyRight), key(tea.KeyTab), tea.KeyMsg{Type: tea.KeyRunes, Runes: []rune("o")}} {
		m, _ = update(m, k)
	}
	if len(opened) != 0 {
		t.Fatalf("a bare key opened %v", opened)
	}
	m, _ = update(m, key(tea.KeyTab), key(tea.KeyDown)) // instance b
	m, _ = update(m, typeText(":open folder")...)
	var msgs []tea.Msg
	m, msgs = update(m, key(tea.KeyEnter))
	if len(opened) != 1 || opened[0] != "/r/b/.groundhog" {
		t.Fatalf("opened %v", opened)
	}
	if r, ok := msgs[0].(ResultMsg); !ok || r.Err != nil || r.Action != ActionOpenFolder {
		t.Fatalf("%+v", msgs)
	}
	m, _ = update(m, msgs...)
	if !strings.Contains(m.status(100), "showing the folder") {
		t.Errorf("status %q", m.status(100))
	}

	// The selection moving or vanishing while the palette is open opens nothing.
	m, _ = update(m, typeText(":open")...)
	m, _ = update(m, SnapshotMsg{FromView(viewOf("a"))})
	m, msgs = update(m, key(tea.KeyEnter))
	if len(opened) != 1 {
		t.Fatalf("opened %v after the bound instance vanished", opened)
	}
	for _, msg := range msgs {
		if r, ok := msg.(ResultMsg); ok && r.Err == nil && r.Action == ActionOpenFolder {
			t.Errorf("claimed success: %+v", r)
		}
	}
	if !strings.Contains(m.notice, "Nothing ran") {
		t.Errorf("notice %q", m.notice)
	}
}

func TestOpenFolderReportsAFailureAndIsOfferedForEveryKindOfInstance(t *testing.T) {
	m := NewWithData(FromView(viewOf("a")), WithFolderOpener(func(string) error { return errors.New("the folder is gone") }))
	m, _ = update(m, typeText(":open")...)
	m, msgs := update(m, key(tea.KeyEnter))
	if r, ok := msgs[0].(ResultMsg); !ok || r.Err == nil {
		t.Fatalf("%+v", msgs)
	}
	m, _ = update(m, msgs...)
	if !strings.Contains(m.status(100), "the folder is gone") || !m.failure {
		t.Errorf("status %q", m.status(100))
	}
	for _, n := range FromView(core.View{Instances: []core.InstanceView{inspected(),
		iv("fresh", core.ClassNotInspected, core.HealthOK, core.Inspection{}),
		iv("legacy", core.ClassLegacy, core.HealthOK, core.Inspection{})}}).Nodes {
		if len(n.Actions) != 1 || n.Actions[0] != ActionOpenFolder {
			t.Errorf("%s: %v", n.Label, n.Actions)
		}
	}
}

func TestWithoutAnOpenerOpenFolderStillReportsNotAvailable(t *testing.T) {
	m := NewWithData(FromView(viewOf("a")))
	m, _ = update(m, typeText(":open")...)
	_, msgs := update(m, key(tea.KeyEnter))
	if r, ok := msgs[0].(ResultMsg); !ok || !errors.Is(r.Err, ErrNotAvailable) {
		t.Fatalf("%+v", msgs)
	}
}
