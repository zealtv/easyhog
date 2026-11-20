package groundhog

import (
	"errors"
	"fmt"
	"strings"
)

// Sentinel errors for errors.Is.
var (
	// ErrInvalidArgument marks a call rejected before any process started.
	ErrInvalidArgument = errors.New("groundhog: invalid argument")
	// ErrUnsupported marks control refused because negotiation found the
	// script unsupported (read-only). Nothing was run beyond `capabilities`.
	ErrUnsupported = errors.New("groundhog: script does not support control")
	// ErrMalformed marks output that could not be read as the documented JSON.
	ErrMalformed = errors.New("groundhog: malformed output")
	// ErrOutputLimit marks a command that wrote more than the allowed stdout.
	ErrOutputLimit = errors.New("groundhog: output limit exceeded")
	// ErrInstanceMismatch marks a status naming a different instance than the
	// script path this client invoked.
	ErrInstanceMismatch = errors.New("groundhog: status names a different instance")
)

// Kind classifies a non-zero exit by the runtime contract's exit-code table.
type Kind int

const (
	// KindUnexpected is any exit outside the table: 1 from a command other than
	// `tick` (an older script's unknown command, say), a signal death, or a code
	// the contract does not list.
	KindUnexpected Kind = iota
	// KindInvalid is exit 2: invalid arguments, or for `retry` not a delivery of
	// this instance.
	KindInvalid
	// KindPaused is exit 3: refused by the instance pause.
	KindPaused
	// KindBusy is exit 4: a lock stayed busy (for `retry`, the run is going or
	// being started). Try again later.
	KindBusy
	// KindNothing is exit 5: nothing to do, or not possible on this host (for
	// `retry`, a delivery with no record, no run, or no executable `run`).
	KindNothing
	// KindIO is exit 6: an I/O failure.
	KindIO
	// KindFiringFailed is exit 1 from `tick`: an item could not be fired, after
	// the rest of the tick ran. Every tick tries it again.
	KindFiringFailed
)

func (k Kind) String() string {
	switch k {
	case KindInvalid:
		return "invalid arguments"
	case KindPaused:
		return "the instance is paused"
	case KindBusy:
		return "busy"
	case KindNothing:
		return "nothing to do"
	case KindIO:
		return "I/O failure"
	case KindFiringFailed:
		return "a firing failed"
	default:
		return "unexpected exit"
	}
}

// kindFor maps one command's exit status. `tick` gives 1 its own meaning
// (KindFiringFailed); 3 and 5 are not in its table.
func kindFor(command string, code int) Kind {
	switch {
	case code == 1 && command == "tick":
		return KindFiringFailed
	case code == 2:
		return KindInvalid
	case code == 3 && command != "tick":
		return KindPaused
	case code == 4:
		return KindBusy
	case code == 5 && command != "tick":
		return KindNothing
	case code == 6:
		return KindIO
	}
	return KindUnexpected
}

// ExitError reports a script that ran and exited non-zero. Code is -1 when a
// signal ended it. Stderr holds what the script wrote, bounded.
type ExitError struct {
	Command         string
	Code            int
	Kind            Kind
	Stderr          string
	StderrTruncated bool
}

func (e *ExitError) Error() string {
	msg := fmt.Sprintf("groundhog %s: exit %d (%s)", e.Command, e.Code, e.Kind)
	if s := oneLine(e.Stderr, 400); s != "" {
		msg += ": " + s
	}
	return msg
}

// Retryable reports whether the same call may succeed shortly: exit 4.
func (e *ExitError) Retryable() bool { return e.Kind == KindBusy }

// CallError reports a command that produced no exit status: the call timed out,
// its context was cancelled, or the process could not be run. For a command that
// mutates state the outcome is unknown; refresh with Status before acting.
type CallError struct {
	Command string
	// Err is context.DeadlineExceeded for the call's time bound,
	// context.Canceled for cancellation, or the executor's error.
	Err             error
	Stderr          string
	StderrTruncated bool
}

func (e *CallError) Error() string {
	msg := fmt.Sprintf("groundhog %s: %v", e.Command, e.Err)
	if s := oneLine(e.Stderr, 400); s != "" {
		msg += ": " + s
	}
	return msg
}

func (e *CallError) Unwrap() error { return e.Err }

// MalformedError reports output that is not the documented JSON object. It
// lists every problem found, so a half-filled value is never returned.
type MalformedError struct {
	Command  string
	Problems []string
}

func (e *MalformedError) Error() string {
	return fmt.Sprintf("groundhog %s: malformed output: %s", e.Command, strings.Join(e.Problems, "; "))
}

func (e *MalformedError) Is(target error) bool { return target == ErrMalformed }

// ProtocolError reports well-formed output from a protocol or version this
// client does not read.
type ProtocolError struct {
	Command  string
	Protocol string
	Version  int
}

func (e *ProtocolError) Error() string {
	return fmt.Sprintf("groundhog %s: unsupported protocol %q version %d", e.Command, e.Protocol, e.Version)
}

// OutputLimitError reports stdout beyond the configured bound. The process was
// stopped.
type OutputLimitError struct {
	Command string
	Limit   int
}

func (e *OutputLimitError) Error() string {
	return fmt.Sprintf("groundhog %s: stdout exceeded %d bytes", e.Command, e.Limit)
}

func (e *OutputLimitError) Is(target error) bool { return target == ErrOutputLimit }

// InstanceMismatchError reports a snapshot that names another instance. Want is
// the path this client was built with, Got what the script reported.
type InstanceMismatchError struct {
	Want, Got string
}

func (e *InstanceMismatchError) Error() string {
	return fmt.Sprintf("groundhog status: script reports instance %q, client expects %q", e.Got, e.Want)
}

func (e *InstanceMismatchError) Is(target error) bool { return target == ErrInstanceMismatch }

// UnsupportedError reports control refused for a script that negotiation found
// unsupported.
type UnsupportedError struct {
	Command string
	Reason  string
}

func (e *UnsupportedError) Error() string {
	return fmt.Sprintf("groundhog %s: unsupported, read-only: %s", e.Command, e.Reason)
}

func (e *UnsupportedError) Is(target error) bool { return target == ErrUnsupported }

// IsRetryable reports whether err is an ExitError with a busy exit.
func IsRetryable(err error) bool {
	var ee *ExitError
	return errors.As(err, &ee) && ee.Retryable()
}

// oneLine collapses whitespace and drops control characters so stderr can sit
// in a one-line message, then bounds it.
func oneLine(s string, max int) string {
	s = strings.Map(func(r rune) rune {
		switch {
		case r == '\n' || r == '\r' || r == '\t':
			return ' '
		case r < 0x20 || r == 0x7f:
			return -1
		}
		return r
	}, s)
	s = strings.Join(strings.Fields(s), " ")
	if len(s) > max {
		s = strings.ToValidUTF8(s[:max], "") + "…"
	}
	return s
}
