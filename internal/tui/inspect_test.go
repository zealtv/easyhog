package tui

import (
	"errors"
	"slices"
	"strings"
	"sync"
	"testing"
	"time"

	tea "github.com/charmbracelet/bubbletea"
	"github.com/charmbracelet/lipgloss"
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
	st.Schedule = []groundhog.ScheduleItem{
		{Path: "schedule/daily/09/report", ItemID: "report", Cadence: &groundhog.Cadence{Axis: "daily", Time: sp("09:00")},
			EligibleNow: true, FiringAllowed: true},
		{Path: "schedule/weekly.paused/mon/review", ItemID: "review", Cadence: &groundhog.Cadence{Axis: "weekly", Day: sp("mon"), Defaulted: true},
			PausedBy: []string{"schedule/weekly.paused"}, BlockedBy: []string{"subtree_paused", "not_due"}},
		{Path: "schedule/every/15m/beat", ItemID: "beat", Cadence: &groundhog.Cadence{Axis: "every", Interval: sp("15m")}, BlockedBy: []string{"instance_paused", "mystery"}},
	}
	code := 3
	started := at.Add(-time.Minute)
	st.Deliveries = []groundhog.Delivery{
		{Path: "out/report-2026-11-15", SchedulePath: sp("schedule/daily/09/report"), State: groundhog.DeliveryFailed, Run: true, ExitStatus: &code,
			FiredAt: &started, Log: sp("out/report-2026-11-15/.groundhog-log"),
			Attempts: []groundhog.Attempt{{ID: sp("a-1"), StartedAt: &started, By: "tick", FinishedAt: &started, ExitStatus: &code}}},
		{Path: "out/old", State: groundhog.DeliveryAccepted, Run: true},
		{Path: "out/waiting", State: groundhog.DeliveryPending, Run: true},
		{Path: "out/by hand", State: groundhog.DeliveryUnrecorded},
	}
	return iv("alpha", core.ClassSupported, core.HealthOK, core.Inspection{Last: obs(st)})
}

func TestInspectedInstanceShowsPauseScheduleAndDeliveries(t *testing.T) {
	n := instanceNode(inspected(), nil, false)
	for _, want := range []string{"Pause: not paused", "Schedule: 3 items (1 due now, 0 already fired, 1 paused)",
		"Deliveries: 1 failed, 1 pending, 1 accepted, 1 unrecorded",
		"out/report-2026-11-15: ✗ failed; attempt 1 by tick, started ", ", exit status 3; log out/report-2026-11-15/.groundhog-log",
		"out/waiting: ◌ pending", "out/by hand: ? unrecorded"} {
		if !strings.Contains(n.Preview, want) {
			t.Errorf("preview lacks %q:\n%s", want, n.Preview)
		}
	}
	for _, gone := range []string{"out/old", "Ownership", "Runner:", "Consumer", "Revision"} {
		if strings.Contains(n.Preview, gone) {
			t.Errorf("preview shows %q:\n%s", gone, n.Preview)
		}
	}
	if n.Badge != "3 items · 1 pending · 1 failed" || n.State != StateFailed {
		t.Errorf("badge %q state %v", n.Badge, n.State)
	}
}

// Every delivery state has its own word and glyph in the preview, one cell
// wide, and none is the badge separator.
func TestEveryDeliveryStateHasItsOwnWordAndGlyph(t *testing.T) {
	states := []groundhog.DeliveryState{groundhog.DeliveryDelivered, groundhog.DeliveryPending, groundhog.DeliveryRunning,
		groundhog.DeliveryInterrupted, groundhog.DeliveryAccepted, groundhog.DeliveryFailed, groundhog.DeliveryUnrecorded}
	seen := map[string]groundhog.DeliveryState{}
	for _, s := range states {
		g := deliveryGlyph(s)
		if other, dup := seen[g]; dup || g == "·" || lipgloss.Width(g) != 1 {
			t.Errorf("%s: glyph %q (shared with %q, or not one cell)", s, g, other)
		}
		seen[g] = s
		if line := deliveryLine(groundhog.Delivery{Path: "out/x", State: s}); line != "out/x: "+g+" "+string(s) {
			t.Errorf("%s: line %q", s, line)
		}
	}
	if g := deliveryGlyph("teleported"); seen[g] != "" {
		t.Errorf("an unknown state borrows %s's glyph", seen[g])
	}
}

// A running delivery makes its item's row read running, an interrupted one
// failed, and the badge counts them.
func TestRunningAndInterruptedDeliveriesReachTheRowsAndTheBadge(t *testing.T) {
	v := inspected()
	st := &v.Inspection.Last.Status
	st.Deliveries = []groundhog.Delivery{
		{Path: "out/beat-1", SchedulePath: sp("schedule/every/15m/beat"), State: groundhog.DeliveryRunning},
		{Path: "out/review-1", SchedulePath: sp("schedule/weekly.paused/mon/review"), State: groundhog.DeliveryInterrupted},
	}
	n := instanceNode(v, nil, false)
	if n.Badge != "3 items · 1 running · 1 failed" || n.State != StateFailed {
		t.Errorf("badge %q state %v", n.Badge, n.State)
	}
	if beat := n.Children[2]; beat.State != StateRunning {
		t.Errorf("beat %v", beat.State)
	}
	if review := n.Children[1]; review.State != StateFailed {
		t.Errorf("review %v", review.State)
	}
}

// The item's payload reference carries its deliveries' logs, newest first.
func TestAnItemsPayloadCarriesItsRunLogsNewestFirst(t *testing.T) {
	v := inspected()
	st := &v.Inspection.Last.Status
	older, newer := at.Add(-2*time.Hour), at.Add(-time.Hour)
	st.Deliveries = append(st.Deliveries,
		groundhog.Delivery{Path: "out/report-new", SchedulePath: sp("schedule/daily/09/report"), State: groundhog.DeliveryAccepted, FiredAt: &newer, Log: sp("out/report-new/.groundhog-log")},
		groundhog.Delivery{Path: "out/report-old", SchedulePath: sp("schedule/daily/09/report"), State: groundhog.DeliveryAccepted, FiredAt: &older, Log: sp("out/report-old/.groundhog-log")})
	ref := instanceNode(v, nil, false).Children[0].Payload
	if want := []string{"out/report-2026-11-15/.groundhog-log", "out/report-new/.groundhog-log", "out/report-old/.groundhog-log"}; !slices.Equal(ref.Logs, want) {
		t.Errorf("logs %q, want %q", ref.Logs, want)
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
	for _, want := range []string{"Cadence: daily at 09:00", "Due: due now", "Delivery: out/report-2026-11-15: ✗ failed"} {
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
	if !strings.Contains(beat.Preview, "Cadence: every 15m") || !strings.Contains(beat.Preview, "the instance is paused; mystery") {
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
		strings.Contains(pn.Badge, "paused") { // the state word says paused; the badge leaves the verdict out
		t.Errorf("instance pause:\n%s\n%s", pn.Preview, pn.Children[2].Preview)
	}
}

func TestAnUninspectedInstanceShowsOnlyWhatTheListingKnows(t *testing.T) {
	v := iv("fresh", core.ClassNotInspected, core.HealthOK, core.Inspection{})
	v.Files = discovery.Glance{Schedule: []discovery.ScheduleDir{{Path: "weekly.paused", Paused: true}, {Path: "weekly.paused/review", Leaf: true, Paused: true}},
		Out: []string{"a", "b"}, Runtime: true}
	n := instanceNode(v, nil, false)
	for _, banned := range []string{"Pause:", "Deliveries", "Last inspected"} {
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

func TestLegacyFailingAndStaleKeepTheirWordsInThePreview(t *testing.T) {
	good := obs(okStatus())
	cases := map[string]struct {
		v    core.InstanceView
		want []string
	}{
		"failing": {iv("f", core.ClassSupported, core.HealthOK, core.Inspection{Last: good, Err: errors.New("deadline exceeded"), ErrAt: at}),
			[]string{"Error at 09:30:15: deadline exceeded", "(the last status that could be read)"}},
		"stale": {iv("s", core.ClassSupported, core.HealthStale, core.Inspection{Last: good}),
			[]string{"Health: stale", "stale: not re-read although it should have been"}},
		"legacy": {iv("l", core.ClassLegacy, core.HealthOK, core.Inspection{Support: &groundhog.Support{Reason: "the script is an older Groundhog (runtime protocol 1)"}}),
			[]string{"Kind: legacy", "Support: unsupported, read-only: the script is an older Groundhog"}},
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
	v.Inspection.Last.Status.Deliveries[0].Path = "out/\x1b]0;pwned\x07boom"
	v.Inspection.Last.Status.Diagnostics = []groundhog.Problem{{Code: "x", Message: "evil\x1b[2J\x07text"}}
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
