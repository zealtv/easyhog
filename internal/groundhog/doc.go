// Package groundhog is the typed client for one deployed Groundhog instance.
//
// It invokes the instance's own script, <instance>/groundhog.sh, by absolute
// path through an injected runner.Executor: no shell, no PATH lookup, and never
// a substitute or upgraded script. The instance is the canonical `.groundhog`
// directory that discovery reports, equal to `status.instance`.
//
// What it wraps, per the runtime contract's protocol 2 (Groundhog 9317d4e):
//
//   - Capabilities and Negotiate: `capabilities --json` and the version and
//     capability check. An older script (protocol 1, or one without the
//     runtime commands) or an unknown protocol version is reported as
//     unsupported, read-only, with the reason. The commands below refuse it.
//   - Status: `status --json` as a typed snapshot, strict about required fields.
//   - Tick, Retry, Pause and Resume: what Easyhog's Run, Retry and the instance
//     pause need. None takes a token.
//
// It deliberately has no `run`: Easyhog ticks an instance itself, one bounded
// `tick` at a time.
//
// Every call is bounded: wall time (WithTimeout), captured stdout
// (WithMaxStdout) and kept stderr (WithMaxStderr). Failures are typed:
// *ExitError (exit mapped to a Kind), *CallError (timeout, cancel, start
// failure), *MalformedError, *ProtocolError, *OutputLimitError,
// *InstanceMismatchError and *UnsupportedError. Discovery alone never grants
// control; nothing here branches on a host or application identity.
package groundhog
