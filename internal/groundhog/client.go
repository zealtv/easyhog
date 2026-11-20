package groundhog

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"os/exec"
	"path/filepath"
	"strings"
	"sync"
	"time"

	"github.com/zealtv/easyhog/internal/runner"
)

// Defaults for one call. Callers override them per client or per call.
const (
	DefaultTimeout   = 30 * time.Second
	DefaultMaxStdout = 8 << 20
	DefaultMaxStderr = 64 << 10
)

// Option adjusts the bounds of a client (New) or of one call.
type Option func(*settings)

type settings struct {
	timeout   time.Duration // 0 selects the command's default
	maxStdout int
	maxStderr int
	bad       error
}

// WithTimeout bounds the subprocess's wall time. On expiry the executor kills
// the script's direct process and the call returns a *CallError wrapping
// context.DeadlineExceeded.
func WithTimeout(d time.Duration) Option {
	return func(s *settings) {
		if d <= 0 {
			s.bad = fmt.Errorf("%w: timeout must be positive", ErrInvalidArgument)
			return
		}
		s.timeout = d
	}
}

// WithMaxStdout bounds captured stdout. More than n bytes stop the process and
// return an *OutputLimitError.
func WithMaxStdout(n int) Option {
	return func(s *settings) {
		if n <= 0 {
			s.bad = fmt.Errorf("%w: stdout limit must be positive", ErrInvalidArgument)
			return
		}
		s.maxStdout = n
	}
}

// WithMaxStderr bounds the stderr kept for errors. Further output is read and
// dropped, so the script never blocks on a full pipe.
func WithMaxStderr(n int) Option {
	return func(s *settings) {
		if n <= 0 {
			s.bad = fmt.Errorf("%w: stderr limit must be positive", ErrInvalidArgument)
			return
		}
		s.maxStderr = n
	}
}

// Client talks to one deployed Groundhog instance through that instance's own
// script, by absolute path, with no shell and no PATH lookup. It never starts
// the script's `run` loop and never writes, replaces or upgrades a script. A
// Client is safe for concurrent use.
type Client struct {
	exec     runner.Executor
	instance string
	script   string
	defaults settings

	mu      sync.Mutex
	support *Support
}

// New builds a client for the canonical `.groundhog` directory instance (the
// value of discovery's Instance.Path, which equals `status.instance`). It does
// not touch the file system or start a process.
func New(exec runner.Executor, instance string, opts ...Option) (*Client, error) {
	if exec == nil {
		return nil, fmt.Errorf("%w: executor is nil", ErrInvalidArgument)
	}
	if err := checkPath("instance", instance); err != nil {
		return nil, err
	}
	d := settings{maxStdout: DefaultMaxStdout, maxStderr: DefaultMaxStderr}
	for _, o := range opts {
		o(&d)
	}
	if d.bad != nil {
		return nil, d.bad
	}
	instance = filepath.Clean(instance)
	return &Client{
		exec:     exec,
		instance: instance,
		script:   filepath.Join(instance, "groundhog.sh"),
		defaults: d,
	}, nil
}

// offers reports whether the script this client negotiated with advertises the
// optional capability name. A client that has not negotiated yet offers nothing,
// so it asks in the older way.
func (c *Client) offers(name string) bool {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.support != nil && c.support.Capabilities != nil && c.support.Capabilities.Has(name)
}

func checkPath(what, p string) error {
	switch {
	case p == "":
		return fmt.Errorf("%w: %s is empty", ErrInvalidArgument, what)
	case strings.ContainsRune(p, 0):
		return fmt.Errorf("%w: %s contains a NUL byte", ErrInvalidArgument, what)
	case !filepath.IsAbs(p):
		return fmt.Errorf("%w: %s must be an absolute path", ErrInvalidArgument, what)
	}
	return nil
}

// resolve layers per-call options over the client's defaults.
func (c *Client) resolve(command string, opts []Option) (settings, error) {
	s := c.defaults
	for _, o := range opts {
		o(&s)
	}
	if s.bad != nil {
		return s, s.bad
	}
	if s.timeout == 0 {
		s.timeout = DefaultTimeout
	}
	return s, nil
}

// capture is a bounded writer. A strict one (stdout) cancels the call and fails
// when it would pass its limit; a lenient one (stderr) keeps the head and
// silently drops the rest.
type capture struct {
	mu        sync.Mutex
	buf       bytes.Buffer
	limit     int
	strict    bool
	keep      bool // false: count and discard
	total     int
	over      bool
	truncated bool
	cancel    context.CancelFunc
}

var errOverLimit = errors.New("output limit exceeded")

// snapshot returns a copy of what was kept; a descendant that outlives the
// script may still write after the executor returns.
func (w *capture) snapshot() (data []byte, truncated bool) {
	w.mu.Lock()
	defer w.mu.Unlock()
	return append([]byte(nil), w.buf.Bytes()...), w.truncated
}

func (w *capture) Write(p []byte) (int, error) {
	w.mu.Lock()
	defer w.mu.Unlock()
	if w.strict {
		if w.over || w.total+len(p) > w.limit {
			w.over = true
			if w.cancel != nil {
				w.cancel()
			}
			return 0, errOverLimit
		}
		w.total += len(p)
		if w.keep {
			w.buf.Write(p)
		}
		return len(p), nil
	}
	if room := w.limit - w.buf.Len(); room > 0 {
		if len(p) > room {
			w.buf.Write(p[:room])
			w.truncated = true
		} else {
			w.buf.Write(p)
		}
	} else if len(p) > 0 {
		w.truncated = true
	}
	return len(p), nil
}

// run executes one command and returns its stdout when keepStdout is set.
func (c *Client) run(ctx context.Context, command string, args []string, keepStdout bool, opts []Option) ([]byte, error) {
	s, err := c.resolve(command, opts)
	if err != nil {
		return nil, err
	}
	if err := ctx.Err(); err != nil {
		return nil, &CallError{Command: command, Err: err}
	}
	callCtx, cancel := context.WithTimeout(ctx, s.timeout)
	defer cancel()

	out := &capture{limit: s.maxStdout, strict: true, keep: keepStdout, cancel: cancel}
	errw := &capture{limit: s.maxStderr, keep: true}
	runErr := c.exec.Run(callCtx, runner.Invocation{
		Executable: c.script,
		Args:       append([]string{command}, args...),
		Dir:        c.instance,
		Stdout:     out,
		Stderr:     errw,
	})
	errBytes, truncated := errw.snapshot()
	stderr := string(errBytes)

	out.mu.Lock()
	over := out.over
	out.mu.Unlock()
	if over {
		return nil, &OutputLimitError{Command: command, Limit: s.maxStdout}
	}
	// A successful exit whose descendants still hold the pipes is a success:
	// the status is known, and output that was cut short fails to parse.
	if runErr == nil || errors.Is(runErr, exec.ErrWaitDelay) {
		data, _ := out.snapshot()
		return data, nil
	}
	if cerr := ctx.Err(); cerr != nil {
		return nil, &CallError{Command: command, Err: cerr, Stderr: stderr, StderrTruncated: truncated}
	}
	if cerr := callCtx.Err(); cerr != nil {
		return nil, &CallError{Command: command, Err: cerr, Stderr: stderr, StderrTruncated: truncated}
	}
	var ec interface{ ExitCode() int }
	if errors.As(runErr, &ec) {
		code := ec.ExitCode()
		return nil, &ExitError{
			Command: command, Code: code, Kind: kindFor(command, code),
			Stderr: stderr, StderrTruncated: truncated,
		}
	}
	return nil, &CallError{Command: command, Err: runErr, Stderr: stderr, StderrTruncated: truncated}
}
