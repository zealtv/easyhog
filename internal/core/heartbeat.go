package core

import "time"

// DefaultTickInterval is the gap between ticks Easyhog expects when nothing says
// otherwise: the pinned script's own default for `run`, and what it assumes for
// anything outside Easyhog that ticks an instance, since Groundhog reports no
// interval for those.
const DefaultTickInterval = 60 * time.Second

// Heartbeat is how recent an instance's last tick looks next to the gap its
// ticker is expected to keep. It
// is read from the status's own tick age, never from the clock of this machine.
type Heartbeat int

const (
	// HeartbeatNone: no tick is recorded, so there is nothing to judge.
	HeartbeatNone Heartbeat = iota
	// HeartbeatFresh: within about 1.5 times the expected interval.
	HeartbeatFresh
	// HeartbeatLate: past that, up to TickFreshness.
	HeartbeatLate
	// HeartbeatQuiet: older than TickFreshness.
	HeartbeatQuiet
)

// String is the level's word, as the preview names it.
func (h Heartbeat) String() string {
	switch h {
	case HeartbeatFresh:
		return "fresh"
	case HeartbeatLate:
		return "late"
	case HeartbeatQuiet:
		return "quiet"
	}
	return "none"
}

// HeartbeatOf is the one rule that decides the level: fresh while the tick age
// is at most 1.5 times interval (DefaultTickInterval when interval is not
// positive), late while it is at most TickFreshness, quiet beyond, and none
// when the verdict records no tick. An interval so long that 1.5 times it passes
// TickFreshness has no late window: fresh then runs to that bound, and quiet
// starts after it.
func HeartbeatOf(v Verdict, interval time.Duration) Heartbeat {
	if !v.HasTick {
		return HeartbeatNone
	}
	if interval <= 0 {
		interval = DefaultTickInterval
	}
	fresh := interval * 3 / 2
	switch {
	case v.Age <= fresh:
		return HeartbeatFresh
	case v.Age <= max(TickFreshness, fresh):
		return HeartbeatLate
	}
	return HeartbeatQuiet
}
