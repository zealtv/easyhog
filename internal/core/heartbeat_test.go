package core

import (
	"testing"
	"time"
)

func TestHeartbeatOf(t *testing.T) {
	s := time.Second
	cases := []struct {
		name     string
		hasTick  bool
		age      time.Duration
		interval time.Duration
		want     Heartbeat
	}{
		{"no tick recorded", false, 0, time.Minute, HeartbeatNone},
		{"no tick, whatever the age field holds", false, time.Hour, time.Minute, HeartbeatNone},
		{"just ticked", true, 0, time.Minute, HeartbeatFresh},
		{"one interval", true, 60 * s, time.Minute, HeartbeatFresh},
		{"exactly 1.5 intervals", true, 90 * s, time.Minute, HeartbeatFresh},
		{"just past 1.5 intervals", true, 91 * s, time.Minute, HeartbeatLate},
		{"exactly TickFreshness", true, TickFreshness, time.Minute, HeartbeatLate},
		{"just past TickFreshness", true, TickFreshness + s, time.Minute, HeartbeatQuiet},
		{"an hour", true, time.Hour, time.Minute, HeartbeatQuiet},
		{"zero interval means the default", true, 90 * s, 0, HeartbeatFresh},
		{"zero interval, past the default's bound", true, 91 * s, 0, HeartbeatLate},
		{"negative interval means the default", true, 91 * s, -time.Second, HeartbeatLate},
		{"a 10 s runner is fresh to 15 s", true, 15 * s, 10 * s, HeartbeatFresh},
		{"a 10 s runner is late just past 15 s", true, 16 * s, 10 * s, HeartbeatLate},
		{"a 2 minute interval: fresh to 3 min, the late window is empty", true, TickFreshness, 2 * time.Minute, HeartbeatFresh},
		{"a 5 minute interval stays fresh to 7.5 min", true, 450 * s, 5 * time.Minute, HeartbeatFresh},
		{"a 5 minute interval, quiet after 7.5 min", true, 451 * s, 5 * time.Minute, HeartbeatQuiet},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			got := HeartbeatOf(Verdict{HasTick: c.hasTick, Age: c.age}, c.interval)
			if got != c.want {
				t.Errorf("HeartbeatOf(age %v, interval %v) = %v, want %v", c.age, c.interval, got, c.want)
			}
		})
	}
}

func TestHeartbeatWords(t *testing.T) {
	for h, want := range map[Heartbeat]string{HeartbeatNone: "none", HeartbeatFresh: "fresh", HeartbeatLate: "late", HeartbeatQuiet: "quiet"} {
		if h.String() != want {
			t.Errorf("%d reads %q, want %q", h, h.String(), want)
		}
	}
}
