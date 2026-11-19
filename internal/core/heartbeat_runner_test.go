//go:build unix

package core

import (
	"testing"
	"time"
)

func TestRunnerInfoCarriesTheIntervalTheRunnerKeeps(t *testing.T) {
	for _, c := range []struct {
		name string
		set  time.Duration
		want time.Duration
	}{
		{"unset: the script's default", 0, DefaultTickInterval},
		{"set: passed to --interval-seconds, so reported", 20 * time.Second, 20 * time.Second},
	} {
		t.Run(c.name, func(t *testing.T) {
			r := newSRig(t, func(cfg *SupervisorConfig) { cfg.RunInterval = c.set })
			key := r.inst("proj")
			r.run(key)
			info, ok := r.sup.Runner(key)
			if !ok || info.Interval != c.want {
				t.Errorf("Runner interval %v (ok %v), want %v", info.Interval, ok, c.want)
			}
			for _, in := range r.sup.Runners() {
				if in.Interval != c.want {
					t.Errorf("Runners interval %v, want %v", in.Interval, c.want)
				}
			}
		})
	}
}
