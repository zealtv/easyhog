package groundhog

import (
	"context"
	"encoding/json"
	"fmt"
	"time"
)

// DeliveryState is a delivery's state as `status` derives it from the
// delivery's record and its lock. Unknown values pass through; see Known.
type DeliveryState string

const (
	// DeliveryDelivered: fired with no run; there is nothing to start.
	DeliveryDelivered DeliveryState = "delivered"
	// DeliveryPending: fired with a run that has not started yet; the next tick
	// that is not paused starts it.
	DeliveryPending DeliveryState = "pending"
	// DeliveryRunning: the latest attempt has started and its wrapper holds the lock.
	DeliveryRunning DeliveryState = "running"
	// DeliveryInterrupted: the latest attempt started and never exited, and the
	// lock is free. Groundhog never starts it again by itself.
	DeliveryInterrupted DeliveryState = "interrupted"
	// DeliveryAccepted: the latest attempt exited 0.
	DeliveryAccepted DeliveryState = "accepted"
	// DeliveryFailed: the latest attempt exited non-zero.
	DeliveryFailed DeliveryState = "failed"
	// DeliveryUnrecorded: no record Groundhog made (put in out/ by hand, say); it
	// never runs.
	DeliveryUnrecorded DeliveryState = "unrecorded"
)

// Known reports whether the state is one the contract lists.
func (s DeliveryState) Known() bool {
	switch s {
	case DeliveryDelivered, DeliveryPending, DeliveryRunning, DeliveryInterrupted,
		DeliveryAccepted, DeliveryFailed, DeliveryUnrecorded:
		return true
	}
	return false
}

// Problem is a diagnostic: a stable code, a human message and an optional
// instance-relative path.
type Problem struct {
	Code    string
	Message string
	Path    *string
}

// Cadence describes when a schedule item recurs. A nil pointer on ScheduleItem
// means the script gave none.
type Cadence struct {
	Axis      string
	Day       *string
	Date      *string
	Time      *string
	Interval  *string
	Defaulted bool
}

// ScheduleItem is one recognized schedule entry.
type ScheduleItem struct {
	Path          string // relative to the instance; the item's identity
	ItemID        string
	Cadence       *Cadence
	PausedBy      []string
	EligibleNow   bool
	FiredMarker   *string
	Fired         bool
	FiringAllowed bool
	BlockedBy     []string
	// NextDue is the time the item's next unfired occurrence begins: the
	// script's own due calculation. The item fires on the first tick at or after
	// it, not at it. Nil when the read did not ask for it, when the script gave
	// null (a paused item, a fired once item, no occurrence ahead) or a value
	// that is not a time.
	NextDue *time.Time
}

// Occurrence is the schedule occurrence a delivery came from. Slot is nil
// outside every/.
type Occurrence struct {
	Date string
	Slot *string
}

// Attempt is one start of a delivery's run. ID and StartedAt are nil for a
// start line whose fields did not parse. FinishedAt and ExitStatus are nil while
// it has not exited.
type Attempt struct {
	ID         *string
	StartedAt  *time.Time
	By         string // "tick" or "retry"
	FinishedAt *time.Time
	ExitStatus *int
}

// Delivery is one directory in out/ and what its record says. Do not rely on
// the order of Status.Deliveries.
type Delivery struct {
	Path         string // "out/<name>", relative to the instance
	ID           *string
	State        DeliveryState
	FiredAt      *time.Time
	SchedulePath *string
	Occurrence   *Occurrence
	// Run is the fired line's handler: true when the item held an executable run.
	Run bool
	// ExitStatus is the latest attempt's, nil while none finished.
	ExitStatus *int
	Attempts   []Attempt
	// Log is the run's output file relative to the instance, nil until a run
	// has written one.
	Log *string
}

// LastTick is the instance's record of its newest completed tick (liveness
// only: it names no caller). Source is "standalone" for `tick` (Easyhog's
// ticks among them) and "runner" for the script's own `run` loop.
type LastTick struct {
	At     time.Time
	Source string
}

// Status is one observation of an instance.
type Status struct {
	Protocol        string
	ProtocolVersion int
	Instance        string
	ObservedAt      time.Time
	// Paused is the instance pause: no ticker fires or starts anything.
	Paused bool
	// LastTick is nil when no tick was recorded (null) or the value is
	// unreadable.
	LastTick    *LastTick
	Schedule    []ScheduleItem
	Deliveries  []Delivery
	Diagnostics []Problem
}

// DeliveryCounts counts the deliveries by state.
func (s Status) DeliveryCounts() map[DeliveryState]int {
	counts := map[DeliveryState]int{}
	for _, d := range s.Deliveries {
		counts[d.State]++
	}
	return counts
}

type wireProblem struct {
	Code    *string `json:"code"`
	Message *string `json:"message"`
	Path    *string `json:"path"`
}

type wireCadence struct {
	Axis      *string `json:"axis"`
	Day       *string `json:"day"`
	Date      *string `json:"date"`
	Time      *string `json:"time"`
	Interval  *string `json:"interval"`
	Defaulted *bool   `json:"defaulted"`
}

type wireSchedule struct {
	Path          *string         `json:"path"`
	ItemID        *string         `json:"item_id"`
	Cadence       *wireCadence    `json:"cadence"`
	PausedBy      []string        `json:"paused_by"`
	EligibleNow   *bool           `json:"eligible_now"`
	FiredMarker   *string         `json:"fired_marker"`
	Fired         *bool           `json:"fired"`
	FiringAllowed *bool           `json:"firing_allowed"`
	BlockedBy     []string        `json:"blocked_by"`
	NextDue       json.RawMessage `json:"next_due"` // optional: absent unless asked for, null when none
}

type wireAttempt struct {
	ID         *string `json:"id"`
	StartedAt  *string `json:"started_at"`
	By         *string `json:"by"`
	FinishedAt *string `json:"finished_at"`
	ExitStatus *int    `json:"exit_status"`
}

type wireDelivery struct {
	Path         *string `json:"path"`
	ID           *string `json:"id"`
	State        *string `json:"state"`
	FiredAt      *string `json:"fired_at"`
	SchedulePath *string `json:"schedule_path"`
	Occurrence   *struct {
		Date *string `json:"date"`
		Slot *string `json:"slot"`
	} `json:"occurrence"`
	Run        *bool         `json:"run"`
	ExitStatus *int          `json:"exit_status"`
	Attempts   []wireAttempt `json:"attempts"`
	Log        *string       `json:"log"`
}

type wireStatus struct {
	Protocol        *string         `json:"protocol"`
	ProtocolVersion *int            `json:"protocol_version"`
	Instance        *string         `json:"instance"`
	ObservedAt      *string         `json:"observed_at"`
	Paused          *bool           `json:"paused"`
	LastTick        json.RawMessage `json:"last_tick"`
	Schedule        []wireSchedule  `json:"schedule"`
	Deliveries      []wireDelivery  `json:"deliveries"`
	Diagnostics     []wireProblem   `json:"diagnostics"`
}

// problems collects every defect of one document.
type problems []string

func (p *problems) addf(format string, args ...any) { *p = append(*p, fmt.Sprintf(format, args...)) }

func (p *problems) str(where, name string, v *string) string {
	if v == nil {
		p.addf("missing required field %q", where+name)
		return ""
	}
	return *v
}

func (p *problems) boolean(where, name string, v *bool) bool {
	if v == nil {
		p.addf("missing required field %q", where+name)
		return false
	}
	return *v
}

func (p *problems) when(where, name string, v *string, required bool) *time.Time {
	if v == nil {
		if required {
			p.addf("missing required field %q", where+name)
		}
		return nil
	}
	t, err := time.Parse(time.RFC3339, *v)
	if err != nil {
		p.addf("field %q is not an RFC 3339 time", where+name)
		return nil
	}
	return &t
}

// Status runs `status --json` (with `--next-due` when the negotiated script
// advertises next-due-v1) and returns the snapshot. The script inspects
// strictly read-only. Unknown fields are ignored. Malformed or truncated JSON,
// or a missing required field, is a *MalformedError listing what was wrong; no
// half-filled snapshot is returned. Another protocol or version is a
// *ProtocolError, and a snapshot naming another instance than this client's an
// *InstanceMismatchError.
func (c *Client) Status(ctx context.Context, opts ...Option) (Status, error) {
	args := []string{"--json"}
	if c.offers(CapNextDue) {
		args = append(args, "--next-due")
	}
	out, err := c.run(ctx, "status", args, true, opts)
	if err != nil {
		return Status{}, err
	}
	st, err := parseStatus(out)
	if err != nil {
		return Status{}, err
	}
	if st.Instance != c.instance {
		return Status{}, &InstanceMismatchError{Want: c.instance, Got: st.Instance}
	}
	return st, nil
}

// parseStatus reads one status document.
func parseStatus(data []byte) (Status, error) {
	var w wireStatus
	if ps := decodeObject(data, &w); len(ps) > 0 {
		return Status{}, &MalformedError{Command: "status", Problems: ps}
	}
	var p problems
	var st Status
	st.Protocol = p.str("", "protocol", w.Protocol)
	if w.ProtocolVersion == nil {
		p.addf("missing required field %q", "protocol_version")
	} else {
		st.ProtocolVersion = *w.ProtocolVersion
	}
	if w.Protocol != nil && w.ProtocolVersion != nil && (st.Protocol != Protocol || st.ProtocolVersion != ProtocolVersion) {
		return Status{}, &ProtocolError{Command: "status", Protocol: st.Protocol, Version: st.ProtocolVersion}
	}
	st.Instance = p.str("", "instance", w.Instance)
	if t := p.when("", "observed_at", w.ObservedAt, true); t != nil {
		st.ObservedAt = *t
	}
	st.Paused = p.boolean("", "paused", w.Paused)
	st.LastTick = parseLastTick(w.LastTick)

	if w.Schedule == nil {
		p.addf("missing required field %q", "schedule")
	}
	for i, it := range w.Schedule {
		where := fmt.Sprintf("schedule[%d].", i)
		item := ScheduleItem{
			Path:          p.str(where, "path", it.Path),
			ItemID:        p.str(where, "item_id", it.ItemID),
			EligibleNow:   p.boolean(where, "eligible_now", it.EligibleNow),
			FiredMarker:   it.FiredMarker,
			Fired:         p.boolean(where, "fired", it.Fired),
			FiringAllowed: p.boolean(where, "firing_allowed", it.FiringAllowed),
			PausedBy:      it.PausedBy,
			BlockedBy:     it.BlockedBy,
			NextDue:       parseNextDue(it.NextDue),
		}
		if it.PausedBy == nil {
			p.addf("missing required field %q", where+"paused_by")
		}
		if it.BlockedBy == nil {
			p.addf("missing required field %q", where+"blocked_by")
		}
		if cd := it.Cadence; cd != nil {
			item.Cadence = &Cadence{
				Axis: p.str(where+"cadence.", "axis", cd.Axis), Day: cd.Day, Date: cd.Date,
				Time: cd.Time, Interval: cd.Interval, Defaulted: cd.Defaulted != nil && *cd.Defaulted,
			}
		}
		st.Schedule = append(st.Schedule, item)
	}

	if w.Deliveries == nil {
		p.addf("missing required field %q", "deliveries")
	}
	for i, d := range w.Deliveries {
		where := fmt.Sprintf("deliveries[%d].", i)
		del := Delivery{
			Path:         p.str(where, "path", d.Path),
			ID:           d.ID,
			State:        DeliveryState(p.str(where, "state", d.State)),
			FiredAt:      p.when(where, "fired_at", d.FiredAt, false),
			SchedulePath: d.SchedulePath,
			Run:          p.boolean(where, "run", d.Run),
			ExitStatus:   d.ExitStatus,
			Log:          d.Log,
		}
		if d.Occurrence != nil {
			del.Occurrence = &Occurrence{Date: p.str(where+"occurrence.", "date", d.Occurrence.Date), Slot: d.Occurrence.Slot}
		}
		if d.Attempts == nil {
			p.addf("missing required field %q", where+"attempts")
		}
		for j, a := range d.Attempts {
			aw := fmt.Sprintf("%sattempts[%d].", where, j)
			att := Attempt{ID: a.ID, ExitStatus: a.ExitStatus}
			if a.By != nil {
				att.By = *a.By
			}
			att.StartedAt = p.when(aw, "started_at", a.StartedAt, false)
			att.FinishedAt = p.when(aw, "finished_at", a.FinishedAt, false)
			del.Attempts = append(del.Attempts, att)
		}
		st.Deliveries = append(st.Deliveries, del)
	}

	if w.Diagnostics == nil {
		p.addf("missing required field %q", "diagnostics")
	}
	for i, d := range w.Diagnostics {
		where := fmt.Sprintf("diagnostics[%d].", i)
		st.Diagnostics = append(st.Diagnostics, Problem{
			Code: p.str(where, "code", d.Code), Message: p.str(where, "message", d.Message), Path: d.Path,
		})
	}

	if len(p) > 0 {
		return Status{}, &MalformedError{Command: "status", Problems: p}
	}
	return st, nil
}

// parseLastTick reads last_tick. Absent, null and anything unreadable all yield
// nil: the field is liveness only, so a bad value must not hide an otherwise
// good snapshot.
func parseLastTick(raw json.RawMessage) *LastTick {
	if len(raw) == 0 || string(raw) == "null" {
		return nil
	}
	var w struct {
		At     *string `json:"at"`
		Source *string `json:"source"`
	}
	if err := json.Unmarshal(raw, &w); err != nil || w.At == nil || w.Source == nil {
		return nil
	}
	t, err := time.Parse(time.RFC3339, *w.At)
	if err != nil {
		return nil
	}
	return &LastTick{At: t, Source: *w.Source}
}

// parseNextDue reads the optional next_due field. Absent, null and anything
// that is not an RFC 3339 time string all yield nil: the field only feeds a
// countdown, so a bad value must not hide an otherwise good snapshot.
func parseNextDue(raw json.RawMessage) *time.Time {
	var v *string
	if len(raw) == 0 || json.Unmarshal(raw, &v) != nil || v == nil {
		return nil
	}
	t, err := time.Parse(time.RFC3339, *v)
	if err != nil {
		return nil
	}
	return &t
}
