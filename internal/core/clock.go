package core

import "time"

// Clock is the refresher's time, injected so tests set the pace. Per-call
// timeouts still use the context's real deadline: they bound a process, not a
// schedule.
type Clock interface {
	Now() time.Time
	// After delivers once, no sooner than d from now.
	After(d time.Duration) <-chan time.Time
}

// SystemClock is the wall clock.
type SystemClock struct{}

func (SystemClock) Now() time.Time                         { return time.Now() }
func (SystemClock) After(d time.Duration) <-chan time.Time { return time.After(d) }
