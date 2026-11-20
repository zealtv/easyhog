package core

import (
	"context"
	"errors"
	"fmt"
	"sync"
	"time"

	"github.com/zealtv/easyhog/internal/groundhog"
)

// DefaultTickInterval is how often this session ticks an instance it runs, and
// what Easyhog assumes for anything outside it, since Groundhog reports no
// interval: the gap the script's own `run` loop keeps by default.
const DefaultTickInterval = 60 * time.Second

// TickerConfig holds the ticker's dependencies. Zero values select defaults.
type TickerConfig struct {
	// Clients is the pool of one client per instance, shared with the
	// refresher (Config.Clients) so both share each instance's capabilities.
	Clients *Clients
	// Interval is the gap from one tick's start to the next; DefaultTickInterval
	// when zero.
	Interval time.Duration
	Clock    Clock
	// Sleep waits d or until ctx ends: the wait between tries of the mutation
	// lock when an item is renamed. Tests replace it.
	Sleep func(ctx context.Context, d time.Duration) error
}

// Ticker is this session's hand on the instances it runs: Run means this
// session ticks the instance, one bounded `groundhog.sh tick` every Interval,
// and Stop means it stops. A tick fires what is due and starts each fired run in
// a session of its own, so it returns quickly, and runs outlive Easyhog. The
// ticker owns nothing: something else may tick the same instance, and
// Groundhog fires each occurrence once and starts each run once. Ticks for one
// instance never overlap. It is safe for concurrent use.
type Ticker struct {
	cfg     TickerConfig
	changed chan struct{}

	mu     sync.Mutex
	closed bool
	loops  map[string]*loop
}

// loop is one instance's ticking. Fields after done are guarded by Ticker.mu.
type loop struct {
	stop  chan struct{} // closed by Stop or Quit
	done  chan struct{} // closed when the goroutine has ended
	first chan error    // the first tick's outcome, for Run

	info TickInfo
}

// TickInfo is what this session's ticker knows about one instance.
type TickInfo struct {
	// Ticking says this session ticks it now.
	Ticking bool
	// Interval is the gap this session keeps between ticks.
	Interval time.Duration
	// LastBegan and LastEnded bracket this session's newest tick of it, by this
	// machine's clock; zero before the first.
	LastBegan, LastEnded time.Time
	// LastErr is the newest tick's failure, nil when it exited 0.
	LastErr error
}

// Ours reports whether a tick Groundhog recorded at at (`last_tick.at`, whole
// seconds when the tick completed) was this session's newest.
func (t TickInfo) Ours(at time.Time) bool {
	return !t.LastBegan.IsZero() && !at.Before(t.LastBegan.Add(-time.Second)) && !at.After(t.LastEnded.Add(time.Second))
}

// NewTicker builds a ticker. It starts nothing.
func NewTicker(cfg TickerConfig) (*Ticker, error) {
	if cfg.Clients == nil {
		return nil, errors.New("core: ticker needs a client pool")
	}
	if cfg.Interval <= 0 {
		cfg.Interval = DefaultTickInterval
	}
	if cfg.Clock == nil {
		cfg.Clock = SystemClock{}
	}
	if cfg.Sleep == nil {
		cfg.Sleep = sleepCtx
	}
	return &Ticker{cfg: cfg, changed: make(chan struct{}, 1), loops: map[string]*loop{}}, nil
}

// Changed signals, conflated, that Info would now return something different.
func (t *Ticker) Changed() <-chan struct{} { return t.changed }

func (t *Ticker) signal() {
	select {
	case t.changed <- struct{}{}:
	default:
	}
}

// Info returns what the ticker knows about one instance, if this session ever
// ticked it.
func (t *Ticker) Info(key string) (TickInfo, bool) {
	t.mu.Lock()
	defer t.mu.Unlock()
	if l := t.loops[key]; l != nil {
		return l.info, true
	}
	return TickInfo{}, false
}

// Refusal says why an action was declined before it changed anything.
type Refusal int

const (
	// RefusedUnsupported: the script lacks what ticking, Retry and the pause need.
	RefusedUnsupported Refusal = iota + 1
	// RefusedClosing: the session is quitting.
	RefusedClosing
	// RefusedPaused: the instance is paused, so `retry` would refuse.
	RefusedPaused
	// RefusedNotOffered: the fresh read offers nothing of the kind (Detail says why).
	RefusedNotOffered
)

func (r Refusal) String() string {
	switch r {
	case RefusedUnsupported:
		return "not supported"
	case RefusedClosing:
		return "quitting"
	case RefusedPaused:
		return "the instance is paused"
	case RefusedNotOffered:
		return "not offered"
	}
	return "refused"
}

// ErrRefused matches every *RefusedError under errors.Is.
var ErrRefused = errors.New("core: action refused")

// RefusedError reports an action declined, with nothing changed.
type RefusedError struct {
	Op       string
	Instance string
	Reason   Refusal
	Detail   string
}

func (e *RefusedError) Error() string {
	msg := fmt.Sprintf("%s %s: %s", e.Op, e.Instance, e.Reason)
	if e.Detail != "" {
		msg += ": " + e.Detail
	}
	return msg
}

func (e *RefusedError) Is(target error) bool { return target == ErrRefused }

// RunResult is what Run did, for a notice.
type RunResult struct {
	Instance string
	// Already says this session was ticking it already; nothing changed.
	Already bool
	// Ticked says the first tick finished before Run returned; TickErr is its
	// failure, nil when it exited 0. Ticking goes on either way.
	Ticked  bool
	TickErr error
}

// Run starts this session ticking the instance at key: a tick now, then one
// every Interval, until Stop or Quit. It refuses a script that lacks what
// ticking needs, after asking it afresh (`capabilities`), and does nothing for
// an instance it ticks already. It returns once the first tick has finished,
// when the loop is stopped before that tick, or when ctx ends first (ticking
// goes on).
func (t *Ticker) Run(ctx context.Context, key string) (RunResult, error) {
	res := RunResult{Instance: key}
	c, err := t.cfg.Clients.For(key)
	if err != nil {
		return res, err
	}
	sup, err := c.Negotiate(ctx)
	if err != nil {
		return res, err
	}
	if !sup.Controllable {
		return res, &RefusedError{Op: "run", Instance: key, Reason: RefusedUnsupported, Detail: sup.Reason}
	}
	t.mu.Lock()
	if t.closed {
		t.mu.Unlock()
		return res, &RefusedError{Op: "run", Instance: key, Reason: RefusedClosing}
	}
	prev := t.loops[key]
	if prev != nil && prev.info.Ticking {
		t.mu.Unlock()
		res.Already = true
		return res, nil
	}
	l := &loop{stop: make(chan struct{}), done: make(chan struct{}), first: make(chan error, 1),
		info: TickInfo{Ticking: true, Interval: t.cfg.Interval}}
	var after <-chan struct{}
	if prev != nil {
		// A tick of the stopped loop may still be under way: this one starts
		// after it, so ticks never overlap. The last tick's times carry over.
		after = prev.done
		l.info.LastBegan, l.info.LastEnded, l.info.LastErr = prev.info.LastBegan, prev.info.LastEnded, prev.info.LastErr
	}
	t.loops[key] = l
	t.mu.Unlock()
	t.signal()
	go t.tickLoop(c, l, after)
	select {
	case res.TickErr = <-l.first:
		res.Ticked = true
	case <-l.done: // stopped before its first tick
	case <-ctx.Done():
	}
	return res, nil
}

// tickLoop ticks until l.stop closes. A tick is never cut short by Stop or
// Quit: it runs under the client's own time bound, and the loop ends after it.
func (t *Ticker) tickLoop(c *groundhog.Client, l *loop, after <-chan struct{}) {
	defer close(l.done)
	if after != nil {
		<-after
	}
	clk := t.cfg.Clock
	for first := true; ; first = false {
		select {
		case <-l.stop:
			return
		default:
		}
		began := clk.Now()
		err := c.Tick(context.Background())
		ended := clk.Now()
		t.mu.Lock()
		l.info.LastBegan, l.info.LastEnded, l.info.LastErr = began, ended, err
		t.mu.Unlock()
		t.signal()
		if first {
			l.first <- err
		}
		select {
		case <-l.stop:
			return
		case <-clk.After(max(0, t.cfg.Interval-ended.Sub(began))):
		}
	}
}

// StopResult is what Stop did.
type StopResult struct {
	Instance string
	// WasTicking says this session was ticking it; false means nothing changed.
	WasTicking bool
}

// Stop ends this session's ticking of the instance at key and returns at once.
// A tick already under way finishes; runs it started go on in their own sessions.
func (t *Ticker) Stop(key string) StopResult {
	res := StopResult{Instance: key}
	t.mu.Lock()
	if l := t.loops[key]; l != nil && l.info.Ticking {
		l.info.Ticking = false
		close(l.stop)
		res.WasTicking = true
	}
	t.mu.Unlock()
	if res.WasTicking {
		t.signal()
	}
	return res
}

// Quit stops every loop and refuses further work. It waits only for ticks
// already under way to finish (each bounded by the client's own timeout), or
// until ctx ends. Nothing is signalled, released or waited for beyond that: runs
// started by a tick live on in their own sessions. It returns how many
// instances this session was ticking.
func (t *Ticker) Quit(ctx context.Context) int {
	t.mu.Lock()
	t.closed = true
	var waits []chan struct{}
	n := 0
	for _, l := range t.loops {
		if l.info.Ticking {
			l.info.Ticking = false
			close(l.stop)
			n++
		}
		waits = append(waits, l.done)
	}
	t.mu.Unlock()
	for _, done := range waits {
		select {
		case <-done:
		case <-ctx.Done():
			return n
		}
	}
	return n
}

// RetryResult is what Retry did.
type RetryResult struct {
	Instance string
	// Started are the deliveries whose run started again.
	Started []string
	// Busy are those whose run is going or being started (exit 4, or a fresh
	// read that shows them pending or running): nothing was started for them.
	Busy []string
	// Gone are those `retry` found nothing to run for (exit 5).
	Gone []string
	// Failed are those `retry` refused for another reason (paused since the
	// read, swept since the read, an I/O failure), each with its error. Retry
	// goes on to the next delivery after one.
	Failed []RetryFailure
}

// RetryFailure is one delivery `retry` could not start, and why.
type RetryFailure struct {
	Delivery string
	Err      error
}

// Retry starts the run of the instance's failed and interrupted deliveries
// again, or of one schedule item's when item is its path. It reads a fresh
// status first and retries only what that read shows failed or interrupted, so
// a delivery that has since been retried elsewhere and accepted is never run
// again. Where the read shows none but some are pending or running, they are
// reported Busy. A paused instance is refused (`retry` exits 3 then). Once it
// has begun, a delivery `retry` refuses is reported in Failed and Retry goes on
// to the next.
func (t *Ticker) Retry(ctx context.Context, key, item string) (RetryResult, error) {
	res := RetryResult{Instance: key}
	if err := t.open("retry", key); err != nil {
		return res, err
	}
	c, err := t.cfg.Clients.For(key)
	if err != nil {
		return res, err
	}
	st, err := c.Status(ctx)
	if err != nil {
		return res, err
	}
	if st.Paused {
		return res, &RefusedError{Op: "retry", Instance: key, Reason: RefusedPaused, Detail: "Unpause instance first"}
	}
	targets := RetryTargets(st, item)
	if len(targets) == 0 {
		for _, d := range ofItem(st, item) {
			if d.State == groundhog.DeliveryRunning || d.State == groundhog.DeliveryPending {
				res.Busy = append(res.Busy, d.Path)
			}
		}
		if len(res.Busy) > 0 {
			return res, nil
		}
		return res, &RefusedError{Op: "retry", Instance: key, Reason: RefusedNotOffered,
			Detail: "the fresh read lists no failed or interrupted delivery here"}
	}
	for _, p := range targets {
		err := c.Retry(ctx, p)
		var ee *groundhog.ExitError
		switch {
		case err == nil:
			res.Started = append(res.Started, p)
		case errors.As(err, &ee) && ee.Kind == groundhog.KindBusy:
			res.Busy = append(res.Busy, p)
		case errors.As(err, &ee) && ee.Kind == groundhog.KindNothing:
			res.Gone = append(res.Gone, p)
		default:
			res.Failed = append(res.Failed, RetryFailure{Delivery: p, Err: err})
		}
	}
	return res, nil
}

// SetPaused sets (pause) or lifts the instance pause with the tokenless
// `pause` or `resume`. Every ticker of the instance, this session's included,
// then fires and starts nothing, and `retry` refuses, until it is lifted. Runs
// already going are left alone. Both are idempotent.
func (t *Ticker) SetPaused(ctx context.Context, key string, pause bool) error {
	op := "unpause"
	if pause {
		op = "pause"
	}
	if err := t.open(op, key); err != nil {
		return err
	}
	c, err := t.cfg.Clients.For(key)
	if err != nil {
		return err
	}
	if pause {
		return c.Pause(ctx)
	}
	return c.Resume(ctx)
}

// open refuses an action once Quit has begun.
func (t *Ticker) open(op, key string) error {
	t.mu.Lock()
	defer t.mu.Unlock()
	if t.closed {
		return &RefusedError{Op: op, Instance: key, Reason: RefusedClosing}
	}
	return nil
}

func sleepCtx(ctx context.Context, d time.Duration) error {
	timer := time.NewTimer(d)
	defer timer.Stop()
	select {
	case <-timer.C:
		return nil
	case <-ctx.Done():
		return ctx.Err()
	}
}
