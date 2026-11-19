package tui

import (
	"errors"
	"strings"
	"testing"
	"time"

	tea "github.com/charmbracelet/bubbletea"

	"github.com/zealtv/easyhog/internal/core"
	"github.com/zealtv/easyhog/internal/groundhog"
)

// Synthetic instances only: nothing here runs a script.

func strp(s string) *string { return &s }

func delivery(id string, state groundhog.DeliveryState, item, owner string) groundhog.Delivery {
	d := groundhog.Delivery{ID: strp(id), Path: "out/" + id, State: state, Attempts: []groundhog.Attempt{{ID: "a-" + id, Outcome: "unknown"}}}
	if item != "" {
		d.SchedulePath = strp(item)
	}
	if owner != "" {
		d.Attempts[0].OwnerLabel = strp(owner)
	}
	return d
}

// withDeliveries gives an instance's status these deliveries and the schedule
// items they came from.
func withDeliveries(v core.InstanceView, dels ...groundhog.Delivery) core.InstanceView {
	st := v.Inspection.Last.Status
	st.Deliveries = dels
	seen := map[string]bool{}
	for _, d := range dels {
		if d.SchedulePath != nil && !seen[*d.SchedulePath] {
			seen[*d.SchedulePath] = true
			st.Schedule = append(st.Schedule, groundhog.ScheduleItem{Path: *d.SchedulePath, ItemID: strings.TrimPrefix(*d.SchedulePath, "schedule/daily/")})
		}
	}
	v.Inspection.Last = obs(st)
	return v
}

var (
	failedA    = delivery("d-fa", groundhog.DeliveryFailed, "schedule/daily/a", "easyhog-1")
	failedB    = delivery("d-fb", groundhog.DeliveryFailed, "schedule/daily/b", "easyhog-1")
	uncertainA = delivery("d-ua", groundhog.DeliveryUncertain, "schedule/daily/a", "easyhog-1")
)

func TestSettlingActionsAreOfferedOnlyWhereThisSessionHoldsTheToken(t *testing.T) {
	all := []groundhog.Delivery{failedA, uncertainA}
	foreign := withDeliveries(iv("e", core.ClassExternal, core.HealthOK, core.Inspection{Last: obs(okStatus())}), all...)
	cases := []struct {
		name string
		v    core.InstanceView
		want string
	}{
		{"ours, running, failed", withDeliveries(ours("a", groundhog.RunnerRunning, false), failedA), "stop,retry-failed"},
		{"ours, running, uncertain", withDeliveries(ours("a", groundhog.RunnerRunning, false), uncertainA), "stop,recover-accept,recover-retry"},
		{"ours, stopped, both", withDeliveries(ours("a", groundhog.RunnerStopped, false), all...), "run,stop,retry-failed,recover-accept,recover-retry"},
		{"ours, nothing to decide", withDeliveries(ours("a", groundhog.RunnerRunning, false), delivery("d-p", groundhog.DeliveryPending, "", "")), "stop"},
		{"unowned: Run first", withDeliveries(unowned("a"), all...), "run"},
		{"external", foreign, ""},
	}
	for _, c := range cases {
		if got := actionsOf(t, c.v, nil); got != c.want {
			t.Errorf("%s: offers %q, want %q", c.name, got, c.want)
		}
	}
}

func TestItemsOfferTheirOwnSettlingActionsOnly(t *testing.T) {
	v := withDeliveries(ours("a", groundhog.RunnerRunning, false), failedA, failedB, uncertainA)
	d := FromViewWith(core.View{Root: "/r", Instances: []core.InstanceView{v}}, lookup(nil))
	n := d.Nodes[0]
	if !hasAction(n, ActionRetryFailed) || !hasAction(n, ActionRecoverAccept) || n.ActionNotes[ActionRetryFailed] != "2 failed" || n.ActionNotes[ActionRecoverAccept] != "1 uncertain" {
		t.Fatalf("instance: %v %v", n.Actions, n.ActionNotes)
	}
	byLabel := map[string]Node{}
	for _, c := range n.Children {
		byLabel[c.Label] = c
	}
	a, b := byLabel["a"], byLabel["b"]
	// Pause item is the item's own offer beside its settling actions.
	if len(a.Actions) != 4 || !hasAction(a, ActionPauseItem) || !hasAction(a, ActionRetryFailed) || !hasAction(a, ActionRecoverRetry) || a.ActionNotes[ActionRetryFailed] != "1 failed" {
		t.Errorf("item a: %v %v", a.Actions, a.ActionNotes)
	}
	if len(b.Actions) != 2 || !hasAction(b, ActionRetryFailed) || hasAction(b, ActionRecoverAccept) {
		t.Errorf("item b: %v", b.Actions)
	}
	if !strings.Contains(b.Preview, "Palette offers: Retry failed (this item's deliveries only)") {
		t.Errorf("item b preview:\n%s", b.Preview)
	}
	// Without the token, an item offers no settling action; an unowned instance's
	// item can still be paused, and an external one's offers nothing.
	for _, v := range []core.InstanceView{
		withDeliveries(unowned("u"), failedA),
		withDeliveries(iv("e", core.ClassExternal, core.HealthOK, core.Inspection{Last: obs(okStatus())}), failedA),
	} {
		for _, c := range FromViewWith(core.View{Instances: []core.InstanceView{v}}, lookup(nil)).Nodes[0].Children {
			for _, a := range c.Actions {
				if a.settles() {
					t.Errorf("%s: item offers %v", v.Class, c.Actions)
				}
			}
			if hasAction(c, ActionPauseItem) != (v.Class == core.ClassUnowned) {
				t.Errorf("%s: item offers %v", v.Class, c.Actions)
			}
		}
	}
	for _, c := range FromView(core.View{Instances: []core.InstanceView{v}}).Nodes[0].Children {
		if len(c.Actions) != 0 {
			t.Errorf("no supervisor: item offers %v", c.Actions)
		}
	}
}

func settleRig(t *testing.T, f *fakeRunners, v core.InstanceView) *rig {
	t.Helper()
	r := newRig(t, f)
	r.m = NewWithData(r.data(v), WithRunners(f, func(k string) { r.mu.Lock(); r.reread = append(r.reread, k); r.mu.Unlock() }))
	r.send(tea.WindowSizeMsg{Width: 110, Height: 34})
	return r
}

func TestPaletteListsAndRunsTheSettlingActions(t *testing.T) {
	f := &fakeRunners{release: make(chan struct{})}
	r := settleRig(t, f, withDeliveries(ours("alpha", groundhog.RunnerRunning, false), failedA, failedB, uncertainA))
	if r.names() != "Stop,Open folder,Refresh,Retry failed,Recover as done,Recover and retry,Quit" {
		t.Fatalf("offers %s", r.names())
	}
	r.send(typeText(":")...)
	for _, want := range []string{"Retry failed", "2 failed", "Recover as done", "1 uncertain"} {
		if !strings.Contains(r.screen(), want) {
			t.Errorf("palette lacks %q:\n%s", want, r.screen())
		}
	}
	r.send(key(tea.KeyEsc))

	r.send(typeText(":retry")...)
	next, cmd := r.m.Update(key(tea.KeyEnter))
	r.m = next.(Model)
	if cmd == nil {
		t.Fatal("Retry failed scheduled nothing")
	}
	if len(f.settles) != 0 {
		t.Fatal("Retry failed ran on the update loop")
	}
	if !strings.Contains(r.screen(), "Retrying alpha…") {
		t.Fatalf("no transitional state:\n%s", r.screen())
	}
	if r.names() != "Open folder,Refresh,Quit" {
		t.Fatalf("while it runs the palette offers %s", r.names())
	}
	done := make(chan tea.Msg, 1)
	go func() { done <- cmd() }()
	close(f.release)
	r.send(<-done)
	if len(f.settles) != 1 || f.settles[0].key != "/r/alpha/.groundhog" || f.settles[0].req.Action != core.ActionRetryFailed || f.settles[0].req.Item != "" {
		t.Fatalf("calls %+v", f.settles)
	}
	want := "Retry failed: alpha: 1 failed delivery set pending; the next attempt is made by the runner; the runner was stopped and restarted (pid 4343), status shows it running"
	if r.m.notice != want || r.m.failure {
		t.Fatalf("notice %q", r.m.notice)
	}
	if len(r.reread) != 1 {
		t.Errorf("reread %v", r.reread)
	}
	// "Last action" in the preview keeps the whole outcome.
	if !strings.Contains(r.screen(), "Last action: Retry failed: alpha: 1 failed delivery") {
		t.Errorf("no Last action line:\n%s", r.screen())
	}
}

func TestRecoverActionsSendTheChosenDecision(t *testing.T) {
	for _, tc := range []struct {
		typed  string
		action core.Action
		text   string
	}{
		{":recover as", core.ActionRecoverAccept, "1 uncertain delivery recorded as done on your say-so; nothing was invoked"},
		{":recover and", core.ActionRecoverRetry, "1 uncertain delivery set pending to be tried again; nothing was invoked"},
	} {
		f := &fakeRunners{}
		r := settleRig(t, f, withDeliveries(ours("alpha", groundhog.RunnerRunning, false), uncertainA))
		msgs := r.send(append(typeText(tc.typed), key(tea.KeyEnter))...)
		r.send(msgs...)
		if len(f.settles) != 1 || f.settles[0].req.Action != tc.action {
			t.Fatalf("%q: calls %+v", tc.typed, f.settles)
		}
		if !strings.Contains(r.m.notice, tc.text) || !strings.Contains(r.m.notice, "the runner was stopped and restarted") {
			t.Errorf("%q: notice %q", tc.typed, r.m.notice)
		}
	}
}

func TestSettlingOnAnItemSettlesThatItemsDeliveriesOnly(t *testing.T) {
	f := &fakeRunners{}
	v := withDeliveries(ours("alpha", groundhog.RunnerRunning, false), failedA, failedB)
	r := settleRig(t, f, v)
	r.send(key(tea.KeyRight)) // expand
	r.send(key(tea.KeyDown), key(tea.KeyDown))
	if sel := r.m.selectedKey(); !strings.HasSuffix(sel, "schedule/daily/b") {
		t.Fatalf("selected %q", sel)
	}
	if r.names() != "Stop,Pause item,Open folder,Refresh,Retry failed,Quit" {
		t.Fatalf("item offers %s", r.names())
	}
	msgs := r.send(append(typeText(":retry"), key(tea.KeyEnter))...)
	r.send(msgs...)
	if len(f.settles) != 1 || f.settles[0].req.Item != "schedule/daily/b" || f.settles[0].key != "/r/alpha/.groundhog" {
		t.Fatalf("calls %+v", f.settles)
	}
}

// The palette binds to the row it opened on: a settling action chosen on one item
// does not run on another because the selection moved behind it.
func TestSettlingActionRunsOnlyOnTheRowThePaletteOpenedOn(t *testing.T) {
	f := &fakeRunners{}
	r := settleRig(t, f, withDeliveries(ours("alpha", groundhog.RunnerRunning, false), failedA, failedB))
	r.send(key(tea.KeyRight), key(tea.KeyDown), key(tea.KeyDown))
	r.send(typeText(":retry")...)
	// Behind the open palette a snapshot changes which row is selected.
	r.send(SnapshotMsg{Data: r.data(withDeliveries(ours("alpha", groundhog.RunnerRunning, false), failedA))})
	r.send(key(tea.KeyEnter))
	if len(f.settles) != 0 {
		t.Fatalf("ran on another row: %+v", f.settles)
	}
	if !r.m.failure || !strings.Contains(r.m.notice, "Nothing ran") {
		t.Errorf("no visible refusal: %q", r.m.notice)
	}
}

func TestSettlingActionNoLongerOfferedRunsNothing(t *testing.T) {
	f := &fakeRunners{}
	r := settleRig(t, f, withDeliveries(ours("alpha", groundhog.RunnerRunning, false), failedA))
	r.send(typeText(":retry")...)
	r.send(SnapshotMsg{Data: r.data(ours("alpha", groundhog.RunnerRunning, false))}) // settled elsewhere
	r.send(key(tea.KeyEnter))
	if len(f.settles) != 0 || !r.m.failure || !strings.Contains(r.m.notice, "Nothing ran: Retry failed is no longer offered") {
		t.Fatalf("calls %+v, notice %q", f.settles, r.m.notice)
	}
}

// A settling action is as strict as Run and Stop: prefix of its name only, no
// aliases and no scattered letters, and "re" still finds Refresh first.
func TestSettlingActionsMatchOnlyTheNamePrefix(t *testing.T) {
	f := &fakeRunners{}
	r := settleRig(t, f, withDeliveries(ours("alpha", groundhog.RunnerStopped, false), failedA, uncertainA))
	for query, want := range map[string][]Action{
		"settle": nil, "fix": nil, "accept": nil, "done": nil, "again": nil, "rtry": nil, "rf": nil, "failed": nil, "ecover": nil,
		"ret": {ActionRetryFailed}, "retry failed": {ActionRetryFailed}, "RETRY": {ActionRetryFailed},
		"rec": {ActionRecoverAccept, ActionRecoverRetry}, "recover as": {ActionRecoverAccept}, "recover and": {ActionRecoverRetry},
	} {
		mm, _ := feed(r.m, typeText(":"+query)...)
		var got []Action
		for _, c := range mm.filtered() {
			if c.action.settles() {
				got = append(got, c.action)
			}
		}
		if len(got) != len(want) || (len(got) > 0 && got[0] != want[0]) || (len(got) > 1 && got[1] != want[1]) {
			t.Errorf("query %q finds %v, want %v", query, got, want)
		}
	}
	// "re" then Enter is still Refresh, never a settling action.
	r.send(append(typeText(":re"), key(tea.KeyEnter))...)
	if len(f.settles) != 0 {
		t.Errorf("a settling action ran: %+v", f.settles)
	}
}

func TestSentencesNeverReachSettlingActions(t *testing.T) {
	for _, sentence := range []string{
		"so: settle it", "ok: fix them", "see: accept all", "x: mark done", "note: try again", "hey: redo",
		"re: recovery plan", "so: recovered fine", "ok: retries", "x: retried", "y: failing", "z: uncertain",
	} {
		f := &fakeRunners{}
		r := settleRig(t, f, withDeliveries(ours("alpha", groundhog.RunnerStopped, false), failedA, uncertainA))
		msgs := r.send(append(typeText(sentence), key(tea.KeyEnter))...)
		r.send(msgs...)
		if len(f.settles) != 0 {
			t.Fatalf("%q reached a settling action: %+v", sentence, f.settles)
		}
	}
}

func TestSettleFailureTextSaysWhatHappenedToDeliveriesAndRunner(t *testing.T) {
	const l = "alpha/.groundhog"
	cases := []struct {
		name string
		err  error
		want string
	}{
		{"stop timed out", &core.SettleError{Instance: "/r/alpha/.groundhog", Action: core.ActionRetryFailed, Stage: core.SettleStopping, Total: 2,
			Waited: 30 * time.Second, Runner: "the runner is still finishing a consumer call it admitted and was not signalled; its stop is requested, so it exits when the call ends. Settle again then"},
			l + ": could not be settled yet: the runner had not stopped after 30s, so nothing was settled; the runner is still finishing a consumer call it admitted and was not signalled; its stop is requested, so it exits when the call ends. Settle again then"},
		{"a call failed", &core.SettleError{Stage: core.SettleSettling, Done: 1, Total: 3, Runner: "the runner was stopped and restarted",
			Cause: &groundhog.ExitError{Command: "retry", Code: 6, Kind: groundhog.KindIO, Stderr: "disk full"}},
			l + ": settled 1 of 3, then failed (groundhog retry: exit 6 (I/O failure): disk full); the runner was stopped and restarted"},
		{"restart failed", &core.SettleError{Stage: core.SettleRestarting, Done: 2, Total: 2, Runner: "the runner was stopped and was not restarted: boom", Cause: errors.New("boom")},
			l + ": settled 2 of 2 (boom); the runner was stopped and was not restarted: boom"},
		{"not offered", &core.RefusedError{Op: "settle", Reason: core.RefusedNotOffered, Detail: "nothing to settle: the fresh read lists no failed deliveries here"},
			l + ": refused: not offered: nothing to settle: the fresh read lists no failed deliveries here"},
		{"not ours", &core.RefusedError{Op: "settle", Reason: core.RefusedNotOurs, Detail: "this session holds no token for it; Run it first, and then it can be settled"},
			l + ": refused: no token of this session's controls it: this session holds no token for it; Run it first, and then it can be settled"},
	}
	for _, c := range cases {
		got := ActionFailure(l, c.err)
		if got == nil || got.Error() != c.want {
			t.Errorf("%s:\n got %v\nwant %s", c.name, got, c.want)
		}
		var ae *ActionError
		if !errors.As(got, &ae) || ae.Err != c.err {
			t.Errorf("%s: the typed cause is not kept", c.name)
		}
	}
}

func TestSettleTextWhenNoRunnerWasRunningOrTheInstanceIsPaused(t *testing.T) {
	base := core.SettleResult{Action: core.ActionRetryFailed, Settled: []string{"a", "b"}}
	if got, want := SettleText("x", base), "x: 2 failed deliveries set pending; the next attempt is made by the runner; no runner was running, so none was started"; got != want {
		t.Errorf("got %q", got)
	}
	paused := base
	paused.StoppedRunner, paused.Restarted, paused.PID, paused.Paused = true, true, 77, true
	if got := SettleText("x", paused); !strings.Contains(got, "restarted (pid 77), status has not shown it running yet; checking again. The instance is still paused") {
		t.Errorf("got %q", got)
	}
}

// The preview says what each settling action does, for an unowned instance that
// Run comes first (a Quit that gave up waiting leaves one), and whose call each
// unresolved attempt was.
func TestPreviewExplainsSettlingAndTheQuitThatGaveUpWaiting(t *testing.T) {
	left := delivery("d-q", groundhog.DeliveryUncertain, "schedule/daily/a", "easyhog-4242")
	v := withDeliveries(unowned("alpha"), left)
	n := FromViewWith(core.View{Instances: []core.InstanceView{v}}, lookup(nil)).Nodes[0]
	for _, want := range []string{
		"Palette offers: Run",
		"Holds 1 uncertain delivery (every handoff waits until they are recovered).",
		"Run enrolls it for this session, and then Recover as done and Recover and retry are offered.",
		"admitted by easyhog-4242",
	} {
		if !strings.Contains(n.Preview, want) {
			t.Errorf("preview lacks %q:\n%s", want, n.Preview)
		}
	}
	// After Run it is this session's: what each action does, once.
	n = FromViewWith(core.View{Instances: []core.InstanceView{withDeliveries(ours("alpha", groundhog.RunnerRunning, false), left, failedB)}}, lookup(nil)).Nodes[0]
	for _, want := range []string{
		"Palette offers: Stop, Retry failed, Recover as done, Recover and retry",
		"Retry failed makes the failed deliveries pending",
		"Recover as done records the uncertain ones as accepted on your say-so",
		"Easyhog cannot know what the consumer did, so check first",
		"stops this session's runner for a moment and starts it again if it was running (the pause stays as it is)",
	} {
		if !strings.Contains(n.Preview, want) {
			t.Errorf("preview lacks %q:\n%s", want, n.Preview)
		}
	}
	if strings.Contains(n.Preview, "Run enrolls it") {
		t.Errorf("an instance that is ours is told to Run first:\n%s", n.Preview)
	}
	// An unowned instance with nothing to settle says nothing about it.
	if p := FromViewWith(core.View{Instances: []core.InstanceView{unowned("b")}}, lookup(nil)).Nodes[0].Preview; strings.Contains(p, "Holds") {
		t.Errorf("preview:\n%s", p)
	}
	// An external instance's deliveries show whose call they were, and offer nothing.
	ext := withDeliveries(iv("e", core.ClassExternal, core.HealthOK, core.Inspection{Last: obs(okStatus())}), left)
	if p := FromViewWith(core.View{Instances: []core.InstanceView{ext}}, lookup(nil)).Nodes[0].Preview; !strings.Contains(p, "admitted by easyhog-4242") || strings.Contains(p, "Recover") {
		t.Errorf("external preview:\n%s", p)
	}
}

// With an uncertain delivery the verdict is not a plain "yes".
func TestVerdictIsNotAPlainYesWhileADeliveryIsUncertain(t *testing.T) {
	v := withTick(withDeliveries(ours("a", groundhog.RunnerRunning, false), uncertainA, delivery("d-p", groundhog.DeliveryPending, "", "")), 12*time.Second, "runner")
	n := instanceNode(v, nil, false)
	if !strings.HasPrefix(n.Badge, "items run, handoffs wait · ") {
		t.Errorf("badge %q", n.Badge)
	}
	want := "Pending work: items fire, but handoffs wait until 1 uncertain delivery is recovered (this session's runner ticked 12s before the"
	if !strings.Contains(n.Preview, want) {
		t.Errorf("preview lacks %q:\n%s", want, n.Preview)
	}
	if strings.Contains(n.Preview, "Pending work: yes") || strings.Contains(n.Badge, "will run") {
		t.Errorf("a plain yes:\n%s\n%s", n.Badge, n.Preview)
	}
	two := withTick(withDeliveries(ours("a", groundhog.RunnerRunning, false), uncertainA, delivery("d-u2", groundhog.DeliveryUncertain, "", "")), 0, "runner")
	if p := instanceNode(two, nil, false).Preview; !strings.Contains(p, "until 2 uncertain deliveries are recovered") {
		t.Errorf("preview:\n%s", p)
	}
	// Something outside Easyhog that ticked recently: the barrier holds for it too.
	ext := withTick(withDeliveries(unowned("b"), uncertainA), 45*time.Second, "standalone")
	n = instanceNode(ext, nil, false)
	if !strings.HasPrefix(n.Badge, "probably, handoffs wait · ") || !strings.Contains(n.Preview, "(something outside Easyhog ticked") {
		t.Errorf("%q\n%s", n.Badge, n.Preview)
	}
	// Paused: no. A plain yes is unchanged where nothing is uncertain.
	p := withTick(withDeliveries(ours("c", groundhog.RunnerRunning, true), uncertainA), time.Second, "runner")
	if got := instanceNode(p, nil, false).Preview; !strings.Contains(got, "Pending work: no, the instance is paused.") {
		t.Errorf("paused preview:\n%s", got)
	}
	y := withTick(withDeliveries(ours("d", groundhog.RunnerRunning, false), failedA), time.Second, "runner")
	if got := instanceNode(y, nil, false).Preview; !strings.Contains(got, "Pending work: yes, this session's runner ticked") {
		t.Errorf("a failed delivery alone changes the verdict:\n%s", got)
	}
}
