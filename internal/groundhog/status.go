package groundhog

import (
	"context"
	"encoding/json"
	"fmt"
	"time"
)

// RunnerState is the runner's state as reported; unknown values pass through
// and decline control (see Known).
type RunnerState string

const (
	RunnerStopped  RunnerState = "stopped"
	RunnerRunning  RunnerState = "running"
	RunnerStopping RunnerState = "stopping"
	RunnerUnknown  RunnerState = "unknown"
	RunnerFailed   RunnerState = "failed"
)

// Known reports whether the state is one the contract lists.
func (s RunnerState) Known() bool {
	switch s {
	case RunnerStopped, RunnerRunning, RunnerStopping, RunnerUnknown, RunnerFailed:
		return true
	}
	return false
}

// DeliveryState is a delivery's handoff state as reported.
type DeliveryState string

const (
	DeliveryPending   DeliveryState = "pending"
	DeliveryInFlight  DeliveryState = "in_flight"
	DeliveryAccepted  DeliveryState = "accepted"
	DeliveryFailed    DeliveryState = "failed"
	DeliveryUncertain DeliveryState = "uncertain"
	DeliveryMissing   DeliveryState = "missing"
)

// Known reports whether the state is one the contract lists.
func (s DeliveryState) Known() bool {
	switch s {
	case DeliveryPending, DeliveryInFlight, DeliveryAccepted, DeliveryFailed, DeliveryUncertain, DeliveryMissing:
		return true
	}
	return false
}

// Problem is a structured error or diagnostic: a stable code, a human message
// and an optional instance-relative path.
type Problem struct {
	Code    string
	Message string
	Path    *string
}

// Owner is the public ownership record. It never carries a secret.
type Owner struct {
	ID         string
	Generation int64
	Label      string
}

// Runner is the runner's reported state. Activity and ownership are separate
// facts; a state here grants no control.
type Runner struct {
	State     RunnerState
	LastError *Problem
}

// Consumer reports whether a consumer is configured and valid, never its argv.
type Consumer struct {
	Configured   bool
	Valid        bool
	ConfigDigest *string
}

// Cadence describes when a schedule item recurs. It is additive; a nil pointer
// on ScheduleItem means the script gave none.
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
	// script's own due calculation, probed at later moments. The item fires on
	// the first tick at or after it, not at it. Nil when the read did not ask
	// for it (the script lacks next-due-v1), when the script gave null (a paused
	// item, a fired once item, no occurrence ahead) or a value that is not a
	// time. Nothing here distinguishes those cases, and a reader need not.
	NextDue *time.Time
}

// Occurrence is the schedule occurrence a delivery came from.
type Occurrence struct {
	Date string
	Slot *string
}

// Attempt is one consumer handoff attempt.
type Attempt struct {
	ID         string
	StartedAt  time.Time
	FinishedAt *time.Time
	Outcome    string // in_flight, accepted, failed, unknown
	ExitCode   *int
	Signal     *string
	Error      *Problem
	Decision   *string // retry or accept, once recovered
	DecidedAt  *time.Time
	// OwnerLabel is the label of the owner that admitted the call (display text,
	// not an identity). Nil when the record predates the field or the script does
	// not report it.
	OwnerLabel *string
}

// Delivery is one materialized out/ directory and its handoff record. Do not
// rely on the order of Status.Deliveries.
type Delivery struct {
	ID           *string // nil for an unregistered legacy out/ entry
	Path         string
	Origin       string
	SchedulePath *string
	Occurrence   *Occurrence
	State        DeliveryState
	Reason       *string
	Attempts     []Attempt
	Execution    string
}

// LastTick is the instance's record of its last completed tick (liveness only:
// it names no caller). Source is "runner" for the owned runner's tick and
// "standalone" for a direct `tick` call.
type LastTick struct {
	At     time.Time
	Source string
}

// Status is one observation of an instance. Revision and JournalGeneration are
// for display: Easyhog compares neither across reads and trusts the script's
// Consistent to say whether a writer was caught mid-update. A snapshot with
// Consistent false must be refreshed before any control.
type Status struct {
	Protocol        string
	ProtocolVersion int
	Instance        string
	ObservedAt      time.Time
	Revision        int64
	// JournalGeneration is nil when the script omits journal_generation, which
	// the contract's field list does not name.
	JournalGeneration *int64
	Consistent        bool
	Paused            bool
	Owner             *Owner // nil when unowned
	// LastTick is nil when the script predates the field (absent), when no tick
	// was ever recorded (null), or when the value is unreadable. Nothing here
	// distinguishes those cases, and a reader need not.
	LastTick   *LastTick
	Runner     Runner
	Consumer   Consumer
	Schedule   []ScheduleItem
	Deliveries []Delivery
	// Settled counts the deliveries a `status --json --active` read left out of
	// Deliveries: accepted and missing ones never change again. It is nil when
	// the read was not asked for it (the script lacks status-active-v1, or the
	// client had not negotiated), and Deliveries then lists every delivery.
	Settled     *Settled
	Diagnostics []Problem
}

// Settled is the count of settled deliveries that an --active read omits.
type Settled struct {
	Accepted int
	Missing  int
}

// DeliveryCounts counts the deliveries by state, the listed ones plus, when the
// read was --active, the settled ones it only counted. It is the one place a
// caller gets counts from, so it does not care which kind of read it holds.
func (s Status) DeliveryCounts() map[DeliveryState]int {
	counts := map[DeliveryState]int{}
	for _, d := range s.Deliveries {
		counts[d.State]++
	}
	if s.Settled != nil {
		counts[DeliveryAccepted] += s.Settled.Accepted
		counts[DeliveryMissing] += s.Settled.Missing
	}
	return counts
}

type wireProblem struct {
	Code    *string `json:"code"`
	Message *string `json:"message"`
	Path    *string `json:"path"`
}

type wireOwner struct {
	ID         *string `json:"id"`
	Generation *int64  `json:"generation"`
	Label      *string `json:"label"`
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
	ID         *string      `json:"id"`
	StartedAt  *string      `json:"started_at"`
	FinishedAt *string      `json:"finished_at"`
	Outcome    *string      `json:"outcome"`
	ExitCode   *int         `json:"exit_code"`
	Signal     *string      `json:"signal"`
	Error      *wireProblem `json:"error"`
	Decision   *string      `json:"decision"`
	DecidedAt  *string      `json:"decided_at"`
	OwnerLabel *string      `json:"owner_label"`
}

type wireDelivery struct {
	ID           *string `json:"id"`
	Path         *string `json:"path"`
	Origin       *string `json:"origin"`
	SchedulePath *string `json:"schedule_path"`
	Occurrence   *struct {
		Date *string `json:"date"`
		Slot *string `json:"slot"`
	} `json:"occurrence"`
	State     *string       `json:"state"`
	Reason    *string       `json:"reason"`
	Attempts  []wireAttempt `json:"attempts"`
	Execution *string       `json:"execution"`
}

type wireStatus struct {
	Protocol          *string         `json:"protocol"`
	ProtocolVersion   *int            `json:"protocol_version"`
	Instance          *string         `json:"instance"`
	ObservedAt        *string         `json:"observed_at"`
	Revision          *int64          `json:"revision"`
	JournalGeneration *int64          `json:"journal_generation"`
	Consistent        *bool           `json:"consistent"`
	Paused            *bool           `json:"paused"`
	Owner             json.RawMessage `json:"owner"` // absent is an error; null is unowned
	Runner            *struct {
		State     *string      `json:"state"`
		LastError *wireProblem `json:"last_error"`
	} `json:"runner"`
	Consumer *struct {
		Configured   *bool   `json:"configured"`
		Valid        *bool   `json:"valid"`
		ConfigDigest *string `json:"config_digest"`
	} `json:"consumer"`
	LastTick   json.RawMessage `json:"last_tick"` // optional: absent on older scripts, null when never recorded
	Schedule   []wireSchedule  `json:"schedule"`
	Deliveries []wireDelivery  `json:"deliveries"`
	Settled    *struct {
		Accepted *int64 `json:"accepted"`
		Missing  *int64 `json:"missing"`
	} `json:"settled"` // optional: present only on an --active read
	Diagnostics []wireProblem `json:"diagnostics"`
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

func (p *problems) number(where, name string, v *int64) int64 {
	if v == nil {
		p.addf("missing required field %q", where+name)
		return 0
	}
	if *v < 0 {
		p.addf("field %q is negative", where+name)
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

func (p *problems) problem(where string, v *wireProblem) *Problem {
	if v == nil {
		return nil
	}
	return &Problem{
		Code:    p.str(where, ".code", v.Code),
		Message: p.str(where, ".message", v.Message),
		Path:    v.Path,
	}
}

// Status runs `status --json` (with `--active` when the negotiated script
// advertises status-active-v1, so settled deliveries come back as counts in
// Settled, and with `--next-due` when it advertises next-due-v1, so each
// schedule item carries NextDue) and returns the snapshot. The script performs a
// strictly read-only inspection. Unknown fields are ignored. Malformed or
// truncated JSON, or any missing required field, is a *MalformedError that lists
// what was wrong; no half-filled snapshot is returned. A snapshot that names
// another instance than this client's is an *InstanceMismatchError.
//
// journal_generation is optional: the contract's field list omits it, and
// Easyhog only displays it (Consistent already says a write was in progress).
func (c *Client) Status(ctx context.Context, opts ...Option) (Status, error) {
	args := []string{"--json"}
	active := c.offers(CapStatusActive)
	if active {
		args = append(args, "--active") // settled deliveries are counted, not listed
	}
	if c.offers(CapNextDue) {
		args = append(args, "--next-due") // each schedule item says when it is next due
	}
	out, err := c.run(ctx, "status", args, true, opts)
	if err != nil {
		return Status{}, err
	}
	st, err := parseStatus(out, active)
	if err != nil {
		return Status{}, err
	}
	if st.Instance != c.instance {
		return Status{}, &InstanceMismatchError{Want: c.instance, Got: st.Instance}
	}
	return st, nil
}

// parseStatus reads one status document. active says the read was made with
// --active, which promises a `settled` object: its absence (or null) would
// leave the settled deliveries neither listed nor counted, so it is malformed.
func parseStatus(data []byte, active bool) (Status, error) {
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
	st.Revision = p.number("", "revision", w.Revision)
	if w.JournalGeneration != nil {
		st.JournalGeneration = new(int64)
		*st.JournalGeneration = p.number("", "journal_generation", w.JournalGeneration)
	}
	st.Consistent = p.boolean("", "consistent", w.Consistent)
	st.Paused = p.boolean("", "paused", w.Paused)

	switch {
	case w.Owner == nil:
		p.addf("missing required field %q", "owner")
	case string(w.Owner) == "null":
	default:
		var wo wireOwner
		if ps := decodeObject(w.Owner, &wo); len(ps) > 0 {
			p.addf("field %q: %s", "owner", ps[0])
			break
		}
		o := Owner{
			ID:         p.str("owner.", "id", wo.ID),
			Generation: p.number("owner.", "generation", wo.Generation),
		}
		if wo.Label != nil {
			o.Label = *wo.Label
		}
		st.Owner = &o
	}

	if w.Runner == nil {
		p.addf("missing required field %q", "runner")
	} else {
		st.Runner.State = RunnerState(p.str("runner.", "state", w.Runner.State))
		st.Runner.LastError = p.problem("runner.last_error", w.Runner.LastError)
	}
	if w.Consumer == nil {
		p.addf("missing required field %q", "consumer")
	} else {
		st.Consumer.Configured = p.boolean("consumer.", "configured", w.Consumer.Configured)
		st.Consumer.Valid = p.boolean("consumer.", "valid", w.Consumer.Valid)
		st.Consumer.ConfigDigest = w.Consumer.ConfigDigest
	}

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
			ID:           d.ID,
			Path:         p.str(where, "path", d.Path),
			Origin:       p.str(where, "origin", d.Origin),
			SchedulePath: d.SchedulePath,
			State:        DeliveryState(p.str(where, "state", d.State)),
			Reason:       d.Reason,
			Execution:    p.str(where, "execution", d.Execution),
		}
		if d.Occurrence != nil {
			del.Occurrence = &Occurrence{Date: p.str(where+"occurrence.", "date", d.Occurrence.Date), Slot: d.Occurrence.Slot}
		}
		if d.Attempts == nil {
			p.addf("missing required field %q", where+"attempts")
		}
		for j, a := range d.Attempts {
			aw := fmt.Sprintf("%sattempts[%d].", where, j)
			att := Attempt{
				ID:         p.str(aw, "id", a.ID),
				Outcome:    p.str(aw, "outcome", a.Outcome),
				ExitCode:   a.ExitCode,
				Signal:     a.Signal,
				Error:      p.problem(aw+"error", a.Error),
				Decision:   a.Decision,
				OwnerLabel: a.OwnerLabel,
			}
			if t := p.when(aw, "started_at", a.StartedAt, true); t != nil {
				att.StartedAt = *t
			}
			att.FinishedAt = p.when(aw, "finished_at", a.FinishedAt, false)
			att.DecidedAt = p.when(aw, "decided_at", a.DecidedAt, false)
			del.Attempts = append(del.Attempts, att)
		}
		st.Deliveries = append(st.Deliveries, del)
	}

	if active && w.Settled == nil {
		p.addf("missing required field %q (the read asked for --active)", "settled")
	}
	if w.Settled != nil {
		st.Settled = &Settled{
			Accepted: int(p.number("settled.", "accepted", w.Settled.Accepted)),
			Missing:  int(p.number("settled.", "missing", w.Settled.Missing)),
		}
	}

	if w.Diagnostics == nil {
		p.addf("missing required field %q", "diagnostics")
	}
	for i := range w.Diagnostics {
		if pr := p.problem(fmt.Sprintf("diagnostics[%d]", i), &w.Diagnostics[i]); pr != nil {
			st.Diagnostics = append(st.Diagnostics, *pr)
		}
	}

	if len(p) > 0 {
		return Status{}, &MalformedError{Command: "status", Problems: p}
	}
	return st, nil
}

// parseLastTick reads the optional last_tick field. Absent, null and anything
// unreadable all yield nil: the field is liveness only, so a bad value must not
// hide an otherwise good snapshot.
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
