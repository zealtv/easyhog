package core

import (
	"errors"
	"slices"
	"strings"
	"testing"

	"github.com/zealtv/easyhog/internal/groundhog"
)

// The table below pins what the palette offered before Offers moved into core
// (it was tui.controls): the rows come from that function's doc
// comment and its branches, and a sweep over every reachable combination was
// compared against the old function's answers when the rule moved. Synthetic
// views only; nothing here runs a script.

type offerCase struct {
	name   string
	class  Class
	trust  Health // current, stale or inconsistent: trust in the status shown
	failed bool   // the newest attempt to read failed
	noLast bool   // no status was ever read
	state  groundhog.RunnerState
	paused bool
	phase  Phase // PhaseNone: the supervisor holds nothing for the instance
	want   []Action
	diags  []string // diagnostic codes in the status
	why    string   // a fragment of the reason, empty when something is offered
}

func (c offerCase) view() InstanceView {
	iv := InstanceView{Class: c.class, Health: c.trust}
	if !c.noLast {
		st := groundhog.Status{Consistent: c.trust != HealthInconsistent}
		for _, code := range c.diags {
			st.Diagnostics = append(st.Diagnostics, groundhog.Problem{Code: code})
		}
		st.Runner.State, st.Paused = c.state, c.paused
		iv.Inspection.Last = &Observation{Status: st}
	}
	if c.failed {
		iv.Inspection.Err = errors.New("timed out")
	}
	return iv
}

func (c offerCase) runner() *RunnerInfo {
	if c.phase == PhaseNone {
		return nil
	}
	return &RunnerInfo{Phase: c.phase}
}

func TestOffers(t *testing.T) {
	offerRun, offerPause, offerBoth := []Action{ActionRun}, []Action{ActionStop}, []Action{ActionRun, ActionStop}
	run, stop := groundhog.RunnerRunning, groundhog.RunnerStopped
	cases := []offerCase{
		// Unowned and controllable: Run, whatever the runner field says.
		{name: "unowned", class: ClassUnowned, state: stop, want: offerRun},
		{name: "unowned, paused", class: ClassUnowned, state: stop, paused: true, want: offerRun},
		{name: "unowned, a stray runner state", class: ClassUnowned, state: groundhog.RunnerUnknown, want: offerRun},

		// This session's own instance follows the runner state and the pause.
		{name: "ours, running", class: ClassOwnedBySession, state: run, want: offerPause},
		{name: "ours, running and paused: Run clears the pause, Stop lets it go", class: ClassOwnedBySession, state: run, paused: true, want: offerBoth},
		{name: "ours, stopped", class: ClassOwnedBySession, state: stop, want: offerBoth},
		{name: "ours, stopped and paused", class: ClassOwnedBySession, state: stop, paused: true, want: offerBoth},
		{name: "ours, runner failed", class: ClassOwnedBySession, state: groundhog.RunnerFailed, want: offerBoth},
		{name: "ours, runner stopping", class: ClassOwnedBySession, state: groundhog.RunnerStopping, why: "its runner is stopping."},
		{name: "ours, runner state unknown", class: ClassOwnedBySession, state: groundhog.RunnerUnknown, why: "its runner is unknown."},
		{name: "ours, no runner state at all", class: ClassOwnedBySession, state: "", why: "its runner is ."},

		// What the supervisor knows overrides the status for this session's own.
		{name: "ours, runner ended, status says unknown", class: ClassOwnedBySession, state: groundhog.RunnerUnknown, phase: PhaseExited, want: offerBoth},
		{name: "ours, runner ended, status says running", class: ClassOwnedBySession, state: run, phase: PhaseExited, want: offerBoth},
		{name: "ours, runner ended, status says stopping", class: ClassOwnedBySession, state: groundhog.RunnerStopping, phase: PhaseExited, want: offerBoth},
		{name: "ours, running per the supervisor", class: ClassOwnedBySession, state: run, phase: PhaseRunning, want: offerPause},
		{name: "ours, enrolled with no runner yet", class: ClassOwnedBySession, state: stop, phase: PhaseEnrolled, want: offerBoth},
		// An adopted instance whose runner died with its session reads "unknown"
		// and the session has no live child: Run starts one.
		{name: "ours, enrolled (adopted), runner state unknown", class: ClassOwnedBySession, state: groundhog.RunnerUnknown, phase: PhaseEnrolled, want: offerBoth},
		{name: "ours, enrolled, runner state unknown and paused", class: ClassOwnedBySession, state: groundhog.RunnerUnknown, phase: PhaseEnrolled, paused: true, want: offerBoth},
		{name: "ours, live child, runner state unknown", class: ClassOwnedBySession, state: groundhog.RunnerUnknown, phase: PhaseRunning, why: "its runner is unknown."},
		{name: "ours, released at quit", class: ClassOwnedBySession, state: stop, phase: PhaseReleased, want: offerBoth},
		{name: "ours, retained at quit, running", class: ClassOwnedBySession, state: run, phase: PhaseRetained, want: offerPause},

		{name: "external, phase none", class: ClassExternal, state: run, why: "another supervisor owns it, running or not"},
		{name: "external, stopped", class: ClassExternal, state: stop, why: "another supervisor owns it, running or not"},
		{name: "external, runner ended is not ours", class: ClassExternal, state: stop, phase: PhaseExited, why: "another supervisor owns it, running or not"},
		{name: "external, released", class: ClassExternal, state: stop, phase: PhaseReleased, why: "another supervisor owns it, running or not"},

		// Classes that offer nothing.
		{name: "unsupported", class: ClassUnsupported, state: stop, why: "the script lacks the capabilities supervised control needs."},
		{name: "legacy", class: ClassLegacy, noLast: true, why: "the script lacks the runtime commands (read-only)."},
		{name: "legacy, with an old status", class: ClassLegacy, state: stop, why: "the script lacks the runtime commands (read-only)."},
		{name: "malformed", class: ClassMalformed, noLast: true, why: "the directory is malformed, so its script is never run."},
		{name: "malformed, failed", class: ClassMalformed, noLast: true, failed: true, why: "the directory is malformed, so its script is never run."},
		{name: "not inspected", class: ClassNotInspected, noLast: true, why: "not until its first status read shows it unowned and controllable (select it, or choose Refresh)."},
		{name: "failing, never read", class: ClassFailing, noLast: true, failed: true, why: "not until a status can be read again."},

		// A status that cannot be acted on stops everything.
		{name: "unowned, newest read failed", class: ClassUnowned, state: stop, failed: true, why: "not until a status can be read again."},
		{name: "ours, newest read failed", class: ClassOwnedBySession, state: run, failed: true, why: "not until a status can be read again."},
		{name: "ours, runner ended, newest read failed", class: ClassOwnedBySession, state: stop, failed: true, phase: PhaseExited, why: "not until a status can be read again."},
		{name: "unowned, inconsistent", class: ClassUnowned, trust: HealthInconsistent, state: stop, why: "its status is not coherent; it will be offered once a coherent read arrives."},
		{name: "ours, inconsistent", class: ClassOwnedBySession, trust: HealthInconsistent, state: run, why: "its status is not coherent"},
		// An interrupted journal write on an unowned instance is settled by
		// Run's enroll; any other incoherence, or an owner, keeps the refusal.
		{name: "unowned, interrupted journal write", class: ClassUnowned, trust: HealthInconsistent, state: stop, diags: []string{"journal_write_in_progress"}, want: offerRun},
		{name: "unowned, interrupted journal write and harmless diagnostics", class: ClassUnowned, trust: HealthInconsistent, state: stop, diags: []string{"path_not_utf8", "journal_write_in_progress", "out_collision"}, want: offerRun},
		{name: "unowned, journal write and a malformed delivery record", class: ClassUnowned, trust: HealthInconsistent, state: stop, diags: []string{"journal_write_in_progress", "delivery_record_malformed"}, why: "its status is not coherent"},
		{name: "unowned, journal write and a changed state.json", class: ClassUnowned, trust: HealthInconsistent, state: stop, diags: []string{"state_changed_during_read", "journal_write_in_progress"}, why: "its status is not coherent"},
		{name: "unowned, journal write and an unknown code", class: ClassUnowned, trust: HealthInconsistent, state: stop, diags: []string{"journal_write_in_progress", "something_new"}, why: "its status is not coherent"},
		{name: "unowned, journal changed during read", class: ClassUnowned, trust: HealthInconsistent, state: stop, diags: []string{"journal_changed_during_read"}, why: "its status is not coherent"},
		{name: "unowned, inconsistent with no diagnostic", class: ClassUnowned, trust: HealthInconsistent, state: stop, why: "its status is not coherent"},
		{name: "ours, interrupted journal write", class: ClassOwnedBySession, trust: HealthInconsistent, state: stop, diags: []string{"journal_write_in_progress"}, why: "its status is not coherent"},
		{name: "external, interrupted journal write", class: ClassExternal, trust: HealthInconsistent, state: stop, diags: []string{"journal_write_in_progress"}, why: "its status is not coherent"},
		{name: "unsupported, interrupted journal write", class: ClassUnsupported, trust: HealthInconsistent, state: stop, diags: []string{"journal_write_in_progress"}, why: "its status is not coherent"},
		{name: "unowned, stale", class: ClassUnowned, trust: HealthStale, state: stop, why: "the status shown is stale; it will be offered once it is read again."},
		{name: "ours, stale", class: ClassOwnedBySession, trust: HealthStale, state: run, why: "the status shown is stale"},
		{name: "ours, runner ended, stale", class: ClassOwnedBySession, trust: HealthStale, state: stop, phase: PhaseExited, why: "the status shown is stale"},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			got, why := Offers(c.view(), c.runner())
			if !slices.Equal(got, c.want) {
				t.Errorf("offers %v, want %v (reason %q)", got, c.want, why)
			}
			switch {
			case len(c.want) > 0 && why != "":
				t.Errorf("offers %v and also says %q", got, why)
			case len(c.want) == 0 && (c.why == "" || !strings.Contains(why, c.why)):
				t.Errorf("reason %q, want it to contain %q", why, c.why)
			}
		})
	}
}

// Over every combination the refresher can produce, Offers offers Run, Stop or
// both (Stop only for an instance of this session's), always says why when it
// offers none, and never offers anything the reader should not act on.
func TestOffersSweep(t *testing.T) {
	offerRunOnly := []Action{ActionRun}
	classes := []Class{ClassNotInspected, ClassMalformed, ClassLegacy, ClassUnsupported, ClassUnowned, ClassOwnedBySession, ClassExternal, ClassFailing}
	states := []groundhog.RunnerState{"", groundhog.RunnerRunning, groundhog.RunnerStopped, groundhog.RunnerStopping, groundhog.RunnerUnknown, groundhog.RunnerFailed}
	n := 0
	for _, class := range classes {
		for _, noLast := range []bool{false, true} {
			for _, failed := range []bool{false, true} {
				for _, trust := range []Health{HealthOK, HealthStale, HealthInconsistent} {
					for phase := PhaseNone; phase <= PhaseRetained; phase++ {
						for _, state := range states {
							for _, paused := range []bool{false, true} {
								c := offerCase{class: class, trust: trust, failed: failed, noLast: noLast, state: state, paused: paused, phase: phase}
								got, why := Offers(c.view(), c.runner())
								n++
								if len(got) > 2 || (len(got) == 0) == (why == "") {
									t.Fatalf("%+v: offers %v, reason %q", c, got, why)
								}
								if len(got) == 0 {
									continue
								}
								switch {
								case class == ClassMalformed || class == ClassLegacy || class == ClassNotInspected || class == ClassFailing:
									t.Fatalf("%+v: offers %v for a class that is never controlled", c, got)
								case failed || trust != HealthOK:
									t.Fatalf("%+v: offers %v on a status that cannot be acted on", c, got)
								case slices.Contains(got, ActionStop) && class != ClassOwnedBySession:
									t.Fatalf("%+v: offers Stop for an instance this session holds no token for", c)
								case len(got) == 2 && !(got[0] == ActionRun && got[1] == ActionStop):
									t.Fatalf("%+v: offers %v, want Run then Stop", c, got)
								case slices.Equal(got, offerRunOnly) && class != ClassUnowned:
									t.Fatalf("%+v: an instance of this session's offers Run without Stop", c)
								}
							}
						}
					}
				}
			}
		}
	}
	if n == 0 {
		t.Fatal("empty sweep")
	}
}

func TestSettlesOnRun(t *testing.T) {
	d := func(codes ...string) []groundhog.Problem {
		var out []groundhog.Problem
		for _, c := range codes {
			out = append(out, groundhog.Problem{Code: c})
		}
		return out
	}
	owner := &groundhog.Owner{ID: "o-1"}
	for _, c := range []struct {
		name string
		st   groundhog.Status
		want bool
	}{
		{"an interrupted journal write", groundhog.Status{Diagnostics: d("journal_write_in_progress")}, true},
		{"with a harmless diagnostic", groundhog.Status{Diagnostics: d("out_collision", "journal_write_in_progress")}, true},
		{"consistent", groundhog.Status{Consistent: true, Diagnostics: d("journal_write_in_progress")}, false},
		{"owned", groundhog.Status{Owner: owner, Diagnostics: d("journal_write_in_progress")}, false},
		{"no diagnostics", groundhog.Status{}, false},
		{"another journal diagnostic", groundhog.Status{Diagnostics: d("journal_generation_unreadable")}, false},
		{"another incoherence too", groundhog.Status{Diagnostics: d("journal_write_in_progress", "state_malformed")}, false},
	} {
		if got := SettlesOnRun(c.st); got != c.want {
			t.Errorf("%s: %v, want %v", c.name, got, c.want)
		}
	}
}

// The settling actions follow the same gates as Run and Pause, plus the token:
// only an instance this session controls offers them, and only for deliveries
// the status lists in the state they act on.
func TestOffersSettling(t *testing.T) {
	str := func(s string) *string { return &s }
	del := func(id, state, item string) groundhog.Delivery {
		d := groundhog.Delivery{ID: str(id), State: groundhog.DeliveryState(state)}
		if item != "" {
			d.SchedulePath = str(item)
		}
		return d
	}
	failed := del("d-f", "failed", "schedule/daily/a")
	failedB := del("d-g", "failed", "schedule/daily/b")
	uncertain := del("d-u", "uncertain", "schedule/daily/a")
	legacy := groundhog.Delivery{State: groundhog.DeliveryFailed} // an unregistered out/ entry: no id
	run, stop := groundhog.RunnerRunning, groundhog.RunnerStopped
	retryF, recA, recR := ActionRetryFailed, ActionRecoverAccept, ActionRecoverRetry

	cases := []struct {
		name string
		offerCase
		dels []groundhog.Delivery
		item string
		want []Action
	}{
		{"ours, running, a failed delivery", offerCase{class: ClassOwnedBySession, state: run}, []groundhog.Delivery{failed}, "", []Action{ActionStop, retryF}},
		{"ours, running, an uncertain delivery", offerCase{class: ClassOwnedBySession, state: run}, []groundhog.Delivery{uncertain}, "", []Action{ActionStop, recA, recR}},
		{"ours, both kinds", offerCase{class: ClassOwnedBySession, state: run}, []groundhog.Delivery{failed, uncertain}, "", []Action{ActionStop, retryF, recA, recR}},
		{"ours, paused", offerCase{class: ClassOwnedBySession, state: run, paused: true}, []groundhog.Delivery{failed}, "", []Action{ActionRun, ActionStop, retryF}},
		{"ours, stopped", offerCase{class: ClassOwnedBySession, state: stop}, []groundhog.Delivery{uncertain}, "", []Action{ActionRun, ActionStop, recA, recR}},
		{"ours, runner ended", offerCase{class: ClassOwnedBySession, state: groundhog.RunnerUnknown, phase: PhaseExited}, []groundhog.Delivery{failed}, "", []Action{ActionRun, ActionStop, retryF}},
		{"ours, adopted token, running", offerCase{class: ClassOwnedBySession, state: run, phase: PhaseEnrolled}, []groundhog.Delivery{failed}, "", []Action{ActionStop, retryF}},
		{"ours, no deliveries needing a decision", offerCase{class: ClassOwnedBySession, state: run}, []groundhog.Delivery{del("d-p", "pending", ""), del("d-a", "accepted", ""), del("d-m", "missing", "")}, "", []Action{ActionStop}},
		{"ours, an in-flight call is the runner's, not ours to settle", offerCase{class: ClassOwnedBySession, state: run}, []groundhog.Delivery{del("d-i", "in_flight", "")}, "", []Action{ActionStop}},
		{"ours, a failed entry with no id cannot be retried", offerCase{class: ClassOwnedBySession, state: run}, []groundhog.Delivery{legacy}, "", []Action{ActionStop}},
		{"on an item: only its failed deliveries count", offerCase{class: ClassOwnedBySession, state: run}, []groundhog.Delivery{failed, failedB}, "schedule/daily/b", []Action{ActionStop, retryF}},
		{"on an item with none of its own", offerCase{class: ClassOwnedBySession, state: run}, []groundhog.Delivery{failed}, "schedule/daily/b", []Action{ActionStop}},
		{"on an item: an uncertain delivery elsewhere is not offered", offerCase{class: ClassOwnedBySession, state: run}, []groundhog.Delivery{uncertain, failedB}, "schedule/daily/b", []Action{ActionStop, retryF}},

		// Without the token, nothing is offered to settle, whatever the status lists.
		{"unowned: Run first", offerCase{class: ClassUnowned, state: stop}, []groundhog.Delivery{failed, uncertain}, "", []Action{ActionRun}},
		{"external", offerCase{class: ClassExternal, state: stop}, []groundhog.Delivery{failed, uncertain}, "", nil},
		{"unsupported", offerCase{class: ClassUnsupported, state: stop}, []groundhog.Delivery{failed}, "", nil},
		{"ours, the newest read failed", offerCase{class: ClassOwnedBySession, state: run, failed: true}, []groundhog.Delivery{failed}, "", nil},
		{"ours, stale", offerCase{class: ClassOwnedBySession, state: run, trust: HealthStale}, []groundhog.Delivery{failed}, "", nil},
		{"ours, inconsistent", offerCase{class: ClassOwnedBySession, state: run, trust: HealthInconsistent}, []groundhog.Delivery{failed}, "", nil},
		{"ours, the runner is stopping", offerCase{class: ClassOwnedBySession, state: groundhog.RunnerStopping}, []groundhog.Delivery{failed}, "", nil},
		{"ours, the runner state is unknown and nothing saw it end", offerCase{class: ClassOwnedBySession, state: groundhog.RunnerUnknown}, []groundhog.Delivery{uncertain}, "", nil},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			iv := c.view()
			if iv.Inspection.Last != nil {
				iv.Inspection.Last.Status.Deliveries = c.dels
			}
			got, why := OffersOn(iv, c.runner(), c.item)
			if !slices.Equal(got, c.want) {
				t.Errorf("offers %v, want %v (reason %q)", got, c.want, why)
			}
			if (len(got) == 0) == (why == "") {
				t.Errorf("offers %v with reason %q", got, why)
			}
			if c.item == "" {
				if plain, _ := Offers(iv, c.runner()); !slices.Equal(plain, got) {
					t.Errorf("Offers %v differs from OffersOn with no item %v", plain, got)
				}
			}
		})
	}
}

func TestSettleTargets(t *testing.T) {
	id := func(s string) *string { return &s }
	st := groundhog.Status{Deliveries: []groundhog.Delivery{
		{ID: id("f1"), State: groundhog.DeliveryFailed, SchedulePath: id("schedule/x")},
		{ID: id("f2"), State: groundhog.DeliveryFailed},
		{ID: id("u1"), State: groundhog.DeliveryUncertain, SchedulePath: id("schedule/x")},
		{ID: id("p1"), State: groundhog.DeliveryPending},
		{State: groundhog.DeliveryFailed},
	}}
	for _, c := range []struct {
		a    Action
		item string
		want []string
	}{
		{ActionRetryFailed, "", []string{"f1", "f2"}},
		{ActionRetryFailed, "schedule/x", []string{"f1"}},
		{ActionRecoverAccept, "", []string{"u1"}},
		{ActionRecoverRetry, "schedule/x", []string{"u1"}},
		{ActionRecoverRetry, "schedule/y", nil},
		{ActionRun, "", nil},
	} {
		if got := SettleTargets(st, c.a, c.item); !slices.Equal(got, c.want) {
			t.Errorf("%s on %q: %v, want %v", c.a, c.item, got, c.want)
		}
	}
	if !ActionRetryFailed.Settles() || !ActionRecoverAccept.Settles() || !ActionRecoverRetry.Settles() || ActionRun.Settles() || ActionStop.Settles() {
		t.Error("Settles is wrong")
	}
}
