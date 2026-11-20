package core

import (
	"errors"
	"slices"
	"strings"
	"testing"

	"github.com/zealtv/easyhog/internal/groundhog"
)

// Synthetic views only; nothing here runs a script.

type offerCase struct {
	name    string
	class   Class
	trust   Health // current or stale: trust in the status shown
	failed  bool   // the newest attempt to read failed
	noLast  bool   // no status was ever read
	paused  bool
	ticking bool
	dels    []groundhog.Delivery
	item    string
	want    []Action
	why     string // a fragment of the reason, empty when something is offered
}

func (c offerCase) view() InstanceView {
	iv := InstanceView{Class: c.class, Health: c.trust}
	if !c.noLast {
		iv.Inspection.Last = &Observation{Status: groundhog.Status{Paused: c.paused, Deliveries: c.dels}}
	}
	if c.failed {
		iv.Inspection.Err = errors.New("timed out")
	}
	return iv
}

func (c offerCase) tick() *TickInfo {
	if !c.ticking {
		return nil
	}
	return &TickInfo{Ticking: true}
}

func del(path, state, item string) groundhog.Delivery {
	d := groundhog.Delivery{Path: path, State: groundhog.DeliveryState(state)}
	if item != "" {
		d.SchedulePath = &item
	}
	return d
}

func TestOffers(t *testing.T) {
	run, stop, pause, unpause, retry := ActionRun, ActionStop, ActionPause, ActionUnpause, ActionRetry
	failedA := del("out/a-1", "failed", "schedule/daily/a")
	interruptedB := del("out/b-1", "interrupted", "schedule/daily/b")
	cases := []offerCase{
		{name: "supported, not ticked here", class: ClassSupported, want: []Action{run, pause}},
		{name: "ticked here", class: ClassSupported, ticking: true, want: []Action{stop, pause}},
		{name: "paused: Unpause, and Run still ticks it", class: ClassSupported, paused: true, want: []Action{run, unpause}},
		{name: "paused and ticked here", class: ClassSupported, paused: true, ticking: true, want: []Action{stop, unpause}},
		{name: "a failed delivery", class: ClassSupported, dels: []groundhog.Delivery{failedA}, want: []Action{run, pause, retry}},
		{name: "an interrupted delivery", class: ClassSupported, ticking: true, dels: []groundhog.Delivery{interruptedB}, want: []Action{stop, pause, retry}},
		{name: "paused with a failed delivery: retry would refuse", class: ClassSupported, paused: true, dels: []groundhog.Delivery{failedA}, want: []Action{run, unpause}},
		{name: "nothing to retry in the other states", class: ClassSupported, dels: []groundhog.Delivery{
			del("out/p", "pending", ""), del("out/r", "running", ""), del("out/a", "accepted", ""),
			del("out/d", "delivered", ""), del("out/u", "unrecorded", "")}, want: []Action{run, pause}},
		{name: "on an item: its own failed delivery", class: ClassSupported, dels: []groundhog.Delivery{failedA, interruptedB}, item: "schedule/daily/b", want: []Action{run, pause, retry}},
		{name: "on an item with none of its own", class: ClassSupported, dels: []groundhog.Delivery{failedA}, item: "schedule/daily/b", want: []Action{run, pause}},

		{name: "unsupported", class: ClassUnsupported, why: "lacks what ticking, Retry and the pause need"},
		{name: "legacy", class: ClassLegacy, noLast: true, why: "older Groundhog"},
		{name: "malformed", class: ClassMalformed, noLast: true, why: "the directory is malformed"},
		{name: "not inspected", class: ClassNotInspected, noLast: true, why: "not until its first status read"},
		{name: "failing, never read", class: ClassFailing, noLast: true, failed: true, why: "not until a status can be read again."},
		{name: "the newest read failed", class: ClassSupported, failed: true, why: "not until a status can be read again."},
		{name: "stale", class: ClassSupported, trust: HealthStale, why: "the status shown is stale"},
		// Stop runs no script, so it stays wherever this session ticks the instance.
		{name: "ticked here, the newest read failed", class: ClassSupported, failed: true, ticking: true, want: []Action{stop}},
		{name: "ticked here, stale", class: ClassSupported, trust: HealthStale, ticking: true, want: []Action{stop}},
		{name: "ticked here, now unsupported", class: ClassUnsupported, ticking: true, want: []Action{stop}},
		{name: "ticked here, now legacy", class: ClassLegacy, noLast: true, ticking: true, want: []Action{stop}},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			got, why := OffersOn(c.view(), c.tick(), c.item)
			if !slices.Equal(got, c.want) {
				t.Errorf("offers %v, want %v (reason %q)", got, c.want, why)
			}
			switch {
			case len(c.want) > 0 && why != "":
				t.Errorf("offers %v and also says %q", got, why)
			case len(c.want) == 0 && (c.why == "" || !strings.Contains(why, c.why)):
				t.Errorf("reason %q, want it to contain %q", why, c.why)
			}
			if c.item == "" {
				if plain, _ := Offers(c.view(), c.tick()); !slices.Equal(plain, got) {
					t.Errorf("Offers %v differs from OffersOn with no item %v", plain, got)
				}
			}
		})
	}
}

// Over every combination, Offers offers exactly one of Run and Stop, exactly
// one of Pause and Unpause, says why when it offers nothing, and offers only
// Stop (where this session ticks it) on a status that cannot be acted on.
func TestOffersSweep(t *testing.T) {
	classes := []Class{ClassNotInspected, ClassMalformed, ClassLegacy, ClassUnsupported, ClassSupported, ClassFailing}
	for _, class := range classes {
		for _, noLast := range []bool{false, true} {
			for _, failed := range []bool{false, true} {
				for _, trust := range []Health{HealthOK, HealthStale} {
					for _, paused := range []bool{false, true} {
						for _, ticking := range []bool{false, true} {
							c := offerCase{class: class, trust: trust, failed: failed, noLast: noLast, paused: paused, ticking: ticking,
								dels: []groundhog.Delivery{del("out/x", "failed", "")}}
							got, why := Offers(c.view(), c.tick())
							if (len(got) == 0) == (why == "") {
								t.Fatalf("%+v: offers %v, reason %q", c, got, why)
							}
							if len(got) == 0 {
								continue
							}
							if class != ClassSupported || failed || trust != HealthOK || noLast {
								// Only Stop, and only where this session ticks it.
								if !ticking || !slices.Equal(got, []Action{ActionStop}) {
									t.Fatalf("%+v: offers %v on what cannot be acted on", c, got)
								}
								continue
							}
							if slices.Contains(got, ActionRun) == slices.Contains(got, ActionStop) ||
								slices.Contains(got, ActionPause) == slices.Contains(got, ActionUnpause) ||
								slices.Contains(got, ActionStop) != ticking || slices.Contains(got, ActionUnpause) != paused ||
								slices.Contains(got, ActionRetry) == paused {
								t.Fatalf("%+v: offers %v", c, got)
							}
						}
					}
				}
			}
		}
	}
}

func TestRetryTargets(t *testing.T) {
	st := groundhog.Status{Deliveries: []groundhog.Delivery{
		del("out/f1", "failed", "schedule/x"),
		del("out/f2", "failed", ""),
		del("out/i1", "interrupted", "schedule/x"),
		del("out/p1", "pending", "schedule/x"),
		del("out/r1", "running", "schedule/x"),
		del("out/a1", "accepted", "schedule/x"),
	}}
	for _, c := range []struct {
		item string
		want []string
	}{
		{"", []string{"out/f1", "out/f2", "out/i1"}},
		{"schedule/x", []string{"out/f1", "out/i1"}},
		{"schedule/y", nil},
	} {
		if got := RetryTargets(st, c.item); !slices.Equal(got, c.want) {
			t.Errorf("%q: %v, want %v", c.item, got, c.want)
		}
	}
}
