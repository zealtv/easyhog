package groundhog

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"sort"
	"strings"
)

// Protocol identity and the one version this client reads.
const (
	Protocol        = "groundhog-runtime"
	ProtocolVersion = 1
)

// Capability names of protocol version 1.
const (
	CapStatusJSON = "status-json-v1"
	CapOwnership  = "ownership-v1"
	CapRunner     = "runner-v1"
	CapPause      = "pause-v1"
	CapConsumer   = "consumer-v1"
	CapDelivery   = "delivery-v1"
)

// Optional capabilities, found in `capabilities --json` before the client uses
// what they name. A script without one stays fully supported; the client keeps
// the older behaviour.
const (
	// CapStatusActive offers `status --json --active`: settled deliveries leave
	// `deliveries` and a top-level `settled` object counts them.
	CapStatusActive = "status-active-v1"
	// CapNextDue offers `status --json --next-due`: each schedule record gains
	// `next_due`, the UTC time its next unfired occurrence begins (or null).
	CapNextDue = "next-due-v1"
	// CapReleaseUnresolved offers `release --leave-unresolved`: release succeeds
	// although an attempt is unresolved, and the barrier stays until `recover`.
	CapReleaseUnresolved = "release-unresolved-v1"
)

// ControlCapabilities are the six capabilities the contract requires, with
// version 1, for supervised control.
var ControlCapabilities = []string{CapStatusJSON, CapOwnership, CapRunner, CapPause, CapConsumer, CapDelivery}

// Capabilities is the parsed answer of `capabilities --json`.
type Capabilities struct {
	Protocol              string
	ProtocolVersion       int
	ImplementationVersion string // informational; empty when absent
	Capabilities          []string
}

// Has reports whether name is advertised.
func (c Capabilities) Has(name string) bool {
	for _, have := range c.Capabilities {
		if have == name {
			return true
		}
	}
	return false
}

type wireCapabilities struct {
	Protocol              *string  `json:"protocol"`
	ProtocolVersion       *int     `json:"protocol_version"`
	ImplementationVersion *string  `json:"implementation_version"`
	Capabilities          []string `json:"capabilities"`
}

// Capabilities runs `capabilities --json`. It parses strictly: malformed JSON or
// a missing protocol, protocol_version or capabilities is a *MalformedError;
// another protocol or version is a *ProtocolError. Unknown fields are ignored.
func (c *Client) Capabilities(ctx context.Context, opts ...Option) (Capabilities, error) {
	out, err := c.run(ctx, "capabilities", []string{"--json"}, true, opts)
	if err != nil {
		return Capabilities{}, err
	}
	var w wireCapabilities
	if problems := decodeObject(out, &w); len(problems) > 0 {
		return Capabilities{}, &MalformedError{Command: "capabilities", Problems: problems}
	}
	var problems []string
	need := func(ok bool, name string) {
		if !ok {
			problems = append(problems, "missing required field "+name)
		}
	}
	need(w.Protocol != nil, `"protocol"`)
	need(w.ProtocolVersion != nil, `"protocol_version"`)
	need(w.Capabilities != nil, `"capabilities"`)
	if len(problems) > 0 {
		return Capabilities{}, &MalformedError{Command: "capabilities", Problems: problems}
	}
	if *w.Protocol != Protocol || *w.ProtocolVersion != ProtocolVersion {
		return Capabilities{}, &ProtocolError{Command: "capabilities", Protocol: *w.Protocol, Version: *w.ProtocolVersion}
	}
	res := Capabilities{
		Protocol:        *w.Protocol,
		ProtocolVersion: *w.ProtocolVersion,
		Capabilities:    w.Capabilities,
	}
	if w.ImplementationVersion != nil {
		res.ImplementationVersion = *w.ImplementationVersion
	}
	return res, nil
}

// Support is the outcome of negotiation with one script.
type Support struct {
	// Controllable is true only for protocol version 1 with all six capabilities.
	Controllable bool
	// Inspectable is true when `status --json` is advertised (version 1).
	Inspectable bool
	// Reason says why the script is not controllable; empty when it is.
	Reason string
	// Missing lists required capabilities the script lacks, sorted.
	Missing []string
	// Capabilities is what the script reported; nil for a script that gave no
	// usable answer (the legacy script, malformed output, an unknown version).
	Capabilities *Capabilities
}

// ReadOnly reports an unsupported script: Easyhog may show it but never control
// it, upgrade it or replace it.
func (s Support) ReadOnly() bool { return !s.Controllable }

func (s Support) String() string {
	if s.Controllable {
		return "supported"
	}
	return "unsupported, read-only: " + s.Reason
}

// Negotiate asks the script what it supports. It runs only `capabilities
// --json`, never `run`, `tick` or `init`. A script that cannot answer (the legacy
// script exits 1 with an unknown-command error), answers in malformed JSON, or
// names another protocol or version is reported as unsupported with the reason,
// not as an error. Busy (exit 4), timeouts, cancellation, process failures and
// output beyond the limit are errors: nothing is known yet. The result is kept
// for the control methods, which refuse an unsupported script.
func (c *Client) Negotiate(ctx context.Context, opts ...Option) (Support, error) {
	caps, err := c.Capabilities(ctx, opts...)
	var sup Support
	switch {
	case err == nil:
		sup = assess(caps)
	default:
		var ee *ExitError
		var me *MalformedError
		var pe *ProtocolError
		switch {
		case errors.As(err, &ee) && ee.Kind != KindBusy:
			sup = Support{Reason: fmt.Sprintf("the script does not answer `capabilities --json` (exit %d); it looks like a legacy script without the runtime commands%s",
				ee.Code, stderrSuffix(ee.Stderr)), Missing: append([]string(nil), ControlCapabilities...)}
		case errors.As(err, &me):
			sup = Support{Reason: "`capabilities --json` output is malformed: " + strings.Join(me.Problems, "; "),
				Missing: append([]string(nil), ControlCapabilities...)}
		case errors.As(err, &pe):
			sup = Support{Reason: fmt.Sprintf("the script speaks protocol %q version %d; this client reads %q version %d",
				pe.Protocol, pe.Version, Protocol, ProtocolVersion),
				Missing: append([]string(nil), ControlCapabilities...)}
		default:
			return Support{}, err
		}
	}
	c.mu.Lock()
	cp := sup
	c.support = &cp
	c.mu.Unlock()
	return sup, nil
}

func stderrSuffix(s string) string {
	if s = oneLine(s, 200); s != "" {
		return ": " + s
	}
	return ""
}

func assess(caps Capabilities) Support {
	sup := Support{Capabilities: &caps, Inspectable: caps.Has(CapStatusJSON)}
	for _, need := range ControlCapabilities {
		if !caps.Has(need) {
			sup.Missing = append(sup.Missing, need)
		}
	}
	sort.Strings(sup.Missing)
	if len(sup.Missing) == 0 {
		sup.Controllable = true
		return sup
	}
	sup.Reason = "lacks required capabilities: " + strings.Join(sup.Missing, ", ")
	return sup
}

// requireControl returns nil only for a script negotiated as controllable. It
// negotiates once when the client has not yet, and reuses that answer; call
// Negotiate again to refresh it.
func (c *Client) requireControl(ctx context.Context, command string, opts []Option) error {
	c.mu.Lock()
	sup := c.support
	c.mu.Unlock()
	if sup == nil {
		s, err := c.Negotiate(ctx, opts...)
		if err != nil {
			return err
		}
		sup = &s
	}
	if !sup.Controllable {
		return &UnsupportedError{Command: command, Reason: sup.Reason}
	}
	return nil
}

// decodeObject reads exactly one JSON object (and trailing whitespace) into dst.
// It returns the problems found; none means dst holds the whole object.
func decodeObject(data []byte, dst any) []string {
	data = bytes.TrimSpace(data)
	if len(data) == 0 {
		return []string{"empty output"}
	}
	dec := json.NewDecoder(bytes.NewReader(data))
	if err := dec.Decode(dst); err != nil {
		return []string{describeJSONError(err)}
	}
	if _, err := dec.Token(); err != io.EOF {
		return []string{"unexpected data after the JSON object"}
	}
	return nil
}

func describeJSONError(err error) string {
	var ute *json.UnmarshalTypeError
	var se *json.SyntaxError
	switch {
	case errors.Is(err, io.ErrUnexpectedEOF):
		return "truncated JSON"
	case errors.As(err, &ute):
		f := ute.Field
		if f == "" {
			return fmt.Sprintf("expected a JSON object, found a %s", ute.Value)
		}
		return fmt.Sprintf("field %q has the wrong type (found %s, want %s)", f, ute.Value, ute.Type)
	case errors.As(err, &se):
		return fmt.Sprintf("invalid JSON at byte %d", se.Offset)
	}
	return "invalid JSON: " + err.Error()
}
