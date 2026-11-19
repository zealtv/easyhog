// Package groundhog is the typed client for one deployed Groundhog instance.
//
// It invokes the instance's own script, <instance>/groundhog.sh, by absolute
// path through an injected runner.Executor: no shell, no PATH lookup, and never
// a substitute or upgraded script. The instance is the canonical `.groundhog`
// directory that discovery reports, equal to `status.instance`.
//
// What it wraps, per the accepted runtime contract and the upstream readings
// recorded for revision d5e3c1f:
//
//   - Capabilities and Negotiate: `capabilities --json` and the version and
//     capability check. A script without the runtime commands (the legacy
//     script) or with an unknown protocol version is reported as unsupported,
//     read-only, with the reason. Control methods refuse it.
//   - Status: `status --json` as a typed snapshot (Revision and the optional
//     JournalGeneration are for display), strict about required fields.
//   - Enroll, Pause, Resume, Stop, Release: the control commands Easyhog's
//     Run and Stop need. The token travels only as `--token-file PATH`, and
//     the client never reads that file.
//
// It deliberately has no `run`. Starting and supervising the runner belongs to
// the supervisor, which must give `run` file output, not a pipe it may close.
//
// Every call is bounded: wall time (WithTimeout), captured stdout
// (WithMaxStdout) and kept stderr (WithMaxStderr). Failures are typed:
// *ExitError (exit mapped to a Kind), *CallError (timeout, cancel, start
// failure), *MalformedError, *ProtocolError, *OutputLimitError,
// *InstanceMismatchError, *UnsupportedError and *UncertainEnrollError.
// Discovery alone never grants control; nothing here branches on a consumer,
// host or application identity.
package groundhog
