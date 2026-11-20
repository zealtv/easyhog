package core

import (
	"time"

	"github.com/zealtv/easyhog/internal/discovery"
	"github.com/zealtv/easyhog/internal/groundhog"
)

// Class says what kind of instance this is, as far as it has been seen: about
// support, never about control. No class grants an action by itself.
type Class int

const (
	// ClassNotInspected: shown from its files only. No script has run for it,
	// or none has answered yet.
	ClassNotInspected Class = iota
	// ClassMalformed: discovery found the directory unusable. Its script is
	// never run.
	ClassMalformed
	// ClassLegacy: the script does not offer `status --json` at protocol
	// version 2 (an older Groundhog, an unknown version, malformed
	// capabilities). Read-only; it stays unexecuted beyond `capabilities`.
	ClassLegacy
	// ClassUnsupported: the script reports status but lacks what ticking,
	// Retry and the pause need (a host without a lock helper, say). Read-only.
	ClassUnsupported
	// ClassSupported: inspectable, and Easyhog may tick it, retry its
	// deliveries and pause it.
	ClassSupported
	// ClassFailing: no usable status could be read and none is kept.
	ClassFailing
)

func (c Class) String() string {
	switch c {
	case ClassMalformed:
		return "malformed"
	case ClassLegacy:
		return "legacy"
	case ClassUnsupported:
		return "unsupported"
	case ClassSupported:
		return "supported"
	case ClassFailing:
		return "failing"
	}
	return "not inspected"
}

// Health says how far to trust the status shown for an instance, and nothing
// else. Whether a status exists at all ("not inspected") and whether the newest
// read failed ("failing") are facts about the instance, not about the status:
// they live in Class and Inspection.Err (see InstanceView.Failing). With no
// status shown Health holds its zero value, HealthOK, which then means nothing.
type Health int

const (
	// HealthOK: the newest read that succeeded is recent enough.
	HealthOK Health = iota
	// HealthStale: the selected instance has not been re-read for several
	// poll intervals although it should have been.
	HealthStale
)

func (h Health) String() string {
	if h == HealthStale {
		return "stale"
	}
	return "current"
}

// Observation is one typed status with when it was taken.
type Observation struct {
	Status groundhog.Status
	// TakenAt is when the read finished, by the refresher's clock. Status's own
	// ObservedAt comes from the script's clock.
	TakenAt time.Time
}

// Inspection is everything known from running scripts for one instance.
type Inspection struct {
	// Support is the last capability negotiation; nil until one has run.
	Support *groundhog.Support
	// Last is the newest status that could be read; nil if none ever was.
	Last *Observation
	// Err is the newest attempt's failure, nil when it succeeded. A failure
	// never discards Last.
	Err error
	// ErrAt is when Err happened.
	ErrAt time.Time
}

// InstanceView is one instance as the terminal should show it.
type InstanceView struct {
	Instance discovery.Instance
	// Files is what listings say, always present; Inspection is what scripts said.
	Files      discovery.Glance
	Inspection Inspection
	Class      Class
	// Health is the trust in Inspection.Last, when there is one.
	Health Health
	// Selected marks the instance the refresher polls.
	Selected bool
}

// Failing reports that the newest attempt to read the instance failed: either
// no status was ever read (ClassFailing), or the last one read stays visible
// beside Inspection.Err.
func (iv InstanceView) Failing() bool {
	return iv.Class == ClassFailing || iv.Inspection.Err != nil
}

// View is an immutable copy of what the refresher holds.
type View struct {
	Root        string
	Instances   []InstanceView
	Diagnostics []discovery.Diagnostic
	// DiscoveredAt is when the instance list was last rebuilt; DiscoverErr is
	// the failure of the newest rescan (the previous list stays shown).
	DiscoveredAt time.Time
	DiscoverErr  error
	// Selected is the instance key the refresher polls; it may name an
	// instance that has since gone.
	Selected string
}

// Find returns the instance with the given key.
func (v View) Find(key string) (InstanceView, bool) {
	for _, in := range v.Instances {
		if in.Instance.Path == key {
			return in, true
		}
	}
	return InstanceView{}, false
}
