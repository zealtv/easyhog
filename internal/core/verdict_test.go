package core

import (
	"testing"
	"time"

	"github.com/zealtv/easyhog/internal/groundhog"
)

func TestVerdictFor(t *testing.T) {
	obs := time.Date(2026, 11, 17, 10, 0, 0, 0, time.UTC)
	at := func(age time.Duration) time.Time { return obs.Add(-age) }
	tick := func(age time.Duration, src string) *groundhog.LastTick {
		return &groundhog.LastTick{At: at(age), Source: src}
	}
	// This session ticked the instance 30 s before the read, and has stopped.
	stopped := &TickInfo{LastBegan: at(31 * time.Second), LastEnded: at(30 * time.Second)}
	ticking := &TickInfo{Ticking: true, LastBegan: at(31 * time.Second), LastEnded: at(30 * time.Second)}
	cases := []struct {
		name    string
		paused  bool
		mine    *TickInfo
		tick    *groundhog.LastTick
		want    Outlook
		ours    bool
		hasTick bool
		age     time.Duration
	}{
		{"paused beats everything", true, ticking, tick(30*time.Second, "standalone"), OutlookNo, true, true, 30 * time.Second},
		{"ticked here", false, ticking, tick(30*time.Second, "standalone"), OutlookYes, true, true, 30 * time.Second},
		{"ticked here, the first tick not recorded yet", false, &TickInfo{Ticking: true}, nil, OutlookYes, false, false, 0},
		{"stopped here, the last tick ours", false, stopped, tick(30*time.Second, "standalone"), OutlookNotSeen, true, true, 30 * time.Second},
		{"stopped here, a newer tick from outside", false, stopped, tick(5*time.Second, "standalone"), OutlookProbably, false, true, 5 * time.Second},
		{"never ticked here, a fresh standalone tick", false, nil, tick(90*time.Second, "standalone"), OutlookProbably, false, true, 90 * time.Second},
		{"a fresh tick from a run loop", false, nil, tick(5*time.Second, "runner"), OutlookProbably, false, true, 5 * time.Second},
		{"exactly at the threshold", false, nil, tick(TickFreshness, "standalone"), OutlookProbably, false, true, TickFreshness},
		{"just past the threshold", false, nil, tick(TickFreshness+time.Second, "standalone"), OutlookNotSeen, false, true, TickFreshness + time.Second},
		{"never recorded", false, nil, nil, OutlookNotSeen, false, false, 0},
		{"tick later than the read (skew)", false, nil, tick(-time.Minute, "standalone"), OutlookProbably, false, true, 0},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			st := groundhog.Status{ObservedAt: obs, Paused: c.paused, LastTick: c.tick}
			v := verdictFor(st, c.mine)
			if v.Outlook != c.want || v.HasTick != c.hasTick || v.Age != c.age || v.Ours != c.ours {
				t.Errorf("got %+v, want outlook %v ours %v hasTick %v age %v", v, c.want, c.ours, c.hasTick, c.age)
			}
		})
	}
}

func TestPendingVerdictNeedsAStatusAndARunnableScript(t *testing.T) {
	if _, ok := PendingVerdict(InstanceView{Class: ClassNotInspected}, nil); ok {
		t.Error("no status: no verdict")
	}
	last := &Observation{Status: groundhog.Status{}}
	for _, c := range []Class{ClassMalformed, ClassLegacy} {
		if _, ok := PendingVerdict(InstanceView{Class: c, Inspection: Inspection{Last: last}}, nil); ok {
			t.Errorf("class %v: no verdict", c)
		}
	}
	if v, ok := PendingVerdict(InstanceView{Class: ClassSupported, Inspection: Inspection{Last: last}}, nil); !ok || v.Outlook != OutlookNotSeen {
		t.Errorf("got %+v %v", v, ok)
	}
}

func TestTickInfoOursAllowsTheSecondThatLastTickIsRoundedTo(t *testing.T) {
	began := time.Date(2026, 11, 20, 10, 0, 0, 400e6, time.UTC)
	ti := TickInfo{LastBegan: began, LastEnded: began.Add(700 * time.Millisecond)}
	for _, c := range []struct {
		at   time.Time
		want bool
	}{
		{began.Truncate(time.Second), true},
		{began.Add(time.Second).Truncate(time.Second), true},
		{began.Add(-2 * time.Second), false},
		{began.Add(5 * time.Second), false},
	} {
		if got := ti.Ours(c.at); got != c.want {
			t.Errorf("Ours(%v) = %v, want %v", c.at, got, c.want)
		}
	}
	if (TickInfo{}).Ours(began) {
		t.Error("a session that never ticked owns no tick")
	}
}
