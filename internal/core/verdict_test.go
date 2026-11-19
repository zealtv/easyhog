package core

import (
	"testing"
	"time"

	"github.com/zealtv/easyhog/internal/groundhog"
)

func TestVerdictFor(t *testing.T) {
	obs := time.Date(2026, 11, 17, 10, 0, 0, 0, time.UTC)
	tick := func(age time.Duration, src string) *groundhog.LastTick {
		return &groundhog.LastTick{At: obs.Add(-age), Source: src}
	}
	cases := []struct {
		name    string
		class   Class
		paused  bool
		runner  groundhog.RunnerState
		tick    *groundhog.LastTick
		want    Outlook
		hasTick bool
		age     time.Duration
	}{
		{"paused beats everything", ClassOwnedBySession, true, groundhog.RunnerRunning, tick(time.Second, "runner"), OutlookNo, true, time.Second},
		{"paused external with fresh tick", ClassExternal, true, groundhog.RunnerRunning, tick(time.Second, "runner"), OutlookNo, true, time.Second},
		{"ours running", ClassOwnedBySession, false, groundhog.RunnerRunning, tick(12*time.Second, "runner"), OutlookYes, true, 12 * time.Second},
		{"ours running, tick never recorded", ClassOwnedBySession, false, groundhog.RunnerRunning, nil, OutlookYes, false, 0},
		{"ours stopped, fresh tick", ClassOwnedBySession, false, groundhog.RunnerStopped, tick(time.Minute, "runner"), OutlookProbably, true, time.Minute},
		{"ours stopped, old tick", ClassOwnedBySession, false, groundhog.RunnerStopped, tick(time.Hour, "runner"), OutlookNotSeen, true, time.Hour},
		{"unowned, standalone tick inside window", ClassUnowned, false, groundhog.RunnerStopped, tick(90*time.Second, "standalone"), OutlookProbably, true, 90 * time.Second},
		{"unowned, exactly at the threshold", ClassUnowned, false, groundhog.RunnerStopped, tick(TickFreshness, "standalone"), OutlookProbably, true, TickFreshness},
		{"unowned, just past the threshold", ClassUnowned, false, groundhog.RunnerStopped, tick(TickFreshness+time.Second, "standalone"), OutlookNotSeen, true, TickFreshness + time.Second},
		{"external running, fresh tick", ClassExternal, false, groundhog.RunnerRunning, tick(5*time.Second, "runner"), OutlookProbably, true, 5 * time.Second},
		{"external, old tick", ClassExternal, false, groundhog.RunnerRunning, tick(10*time.Minute, "runner"), OutlookNotSeen, true, 10 * time.Minute},
		{"never recorded or older script", ClassUnowned, false, groundhog.RunnerStopped, nil, OutlookNotSeen, false, 0},
		{"tick later than the read (skew)", ClassUnowned, false, groundhog.RunnerStopped, tick(-time.Minute, "standalone"), OutlookProbably, true, 0},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			st := groundhog.Status{ObservedAt: obs, Paused: c.paused, LastTick: c.tick, Runner: groundhog.Runner{State: c.runner}}
			v := verdictFor(c.class, st)
			if v.Outlook != c.want || v.HasTick != c.hasTick || v.Age != c.age {
				t.Errorf("got %+v, want outlook %v hasTick %v age %v", v, c.want, c.hasTick, c.age)
			}
		})
	}
}

func TestPendingVerdictNeedsAStatusAndARunnableScript(t *testing.T) {
	if _, ok := PendingVerdict(InstanceView{Class: ClassNotInspected}); ok {
		t.Error("no status: no verdict")
	}
	last := &Observation{Status: groundhog.Status{}}
	for _, c := range []Class{ClassMalformed, ClassLegacy} {
		if _, ok := PendingVerdict(InstanceView{Class: c, Inspection: Inspection{Last: last}}); ok {
			t.Errorf("class %v: no verdict", c)
		}
	}
	if v, ok := PendingVerdict(InstanceView{Class: ClassUnowned, Inspection: Inspection{Last: last}}); !ok || v.Outlook != OutlookNotSeen {
		t.Errorf("got %+v %v", v, ok)
	}
}

// An uncertain delivery raises Groundhog's
// barrier, so items fire and their handoffs wait; the verdict is not a plain yes.
func TestVerdictCountsUncertainDeliveriesAndQualifiesYesAndProbably(t *testing.T) {
	obs := time.Date(2026, 11, 17, 10, 0, 0, 0, time.UTC)
	uncertain := groundhog.Delivery{State: groundhog.DeliveryUncertain}
	pending := groundhog.Delivery{State: groundhog.DeliveryPending}
	for _, c := range []struct {
		name   string
		class  Class
		paused bool
		runner groundhog.RunnerState
		tick   time.Duration
		dels   []groundhog.Delivery
		want   Outlook
		count  int
		waits  bool
	}{
		{"ours running, one uncertain and some pending", ClassOwnedBySession, false, groundhog.RunnerRunning, time.Second, []groundhog.Delivery{uncertain, pending, pending}, OutlookYes, 1, true},
		{"ours running, two uncertain", ClassOwnedBySession, false, groundhog.RunnerRunning, time.Second, []groundhog.Delivery{uncertain, uncertain}, OutlookYes, 2, true},
		{"ours running, none uncertain", ClassOwnedBySession, false, groundhog.RunnerRunning, time.Second, []groundhog.Delivery{pending}, OutlookYes, 0, false},
		{"external with a fresh tick and an uncertain delivery", ClassExternal, false, groundhog.RunnerRunning, time.Second, []groundhog.Delivery{uncertain}, OutlookProbably, 1, true},
		{"paused: nothing fires, so handoffs are not the question", ClassOwnedBySession, true, groundhog.RunnerRunning, time.Second, []groundhog.Delivery{uncertain}, OutlookNo, 1, false},
		{"nothing seen ticking", ClassUnowned, false, groundhog.RunnerStopped, time.Hour, []groundhog.Delivery{uncertain}, OutlookNotSeen, 1, false},
	} {
		t.Run(c.name, func(t *testing.T) {
			st := groundhog.Status{ObservedAt: obs, Paused: c.paused, Runner: groundhog.Runner{State: c.runner}, Deliveries: c.dels,
				LastTick: &groundhog.LastTick{At: obs.Add(-c.tick), Source: "runner"}}
			v := verdictFor(c.class, st)
			if v.Outlook != c.want || v.Uncertain != c.count || v.HandoffsWait() != c.waits {
				t.Errorf("got %+v (waits %v), want outlook %v uncertain %d waits %v", v, v.HandoffsWait(), c.want, c.count, c.waits)
			}
		})
	}
	// An --active read counts settled deliveries, never uncertain ones, so the
	// count holds on either kind of read.
	st := groundhog.Status{Settled: &groundhog.Settled{Accepted: 3}, Deliveries: []groundhog.Delivery{uncertain}, Runner: groundhog.Runner{State: groundhog.RunnerRunning}}
	if v := verdictFor(ClassOwnedBySession, st); v.Uncertain != 1 {
		t.Errorf("uncertain %d on an --active read", v.Uncertain)
	}
}
