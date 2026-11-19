package core

import (
	"context"
	"errors"
	"fmt"
	"sync"
	"time"

	"github.com/zealtv/easyhog/internal/discovery"
	"github.com/zealtv/easyhog/internal/groundhog"
	"github.com/zealtv/easyhog/internal/runner"
)

// Refresh model defaults. See Config.
const (
	// DefaultInterval is how often the selected instance's status is re-read.
	// Five seconds keeps the terminal current enough for a viewer whose state
	// changes by minutes (schedules, handoffs, pauses), while one `status` run
	// costs a few milliseconds on a small journal and, as measured,
	// about 8 ms per delivery record on a large one: the interval
	// stretches to four times the last call's duration so that even a slow
	// instance spends at most a fifth of its time answering us.
	DefaultInterval = 5 * time.Second
	// DefaultSelectSettle delays the first read of a newly selected instance, so
	// holding an arrow key down a long list runs no script on the way past.
	DefaultSelectSettle = 500 * time.Millisecond
	// DefaultDiscoverEvery is the rescan period. A rescan lists directories and
	// runs nothing.
	DefaultDiscoverEvery = 30 * time.Second
	// DefaultCallTimeout bounds each script call the refresher makes, passed per
	// call (the shared client's own default, 30 s, stays for the supervisor's
	// calls). Half the client's default: a status that takes this long is itself
	// the finding to show.
	DefaultCallTimeout = 15 * time.Second
	// DefaultConcurrency bounds simultaneous script calls, which only an
	// explicit Refresh over many instances approaches.
	DefaultConcurrency = 4
	// DefaultRetries is how many times a `consistent:false` status is re-read
	// before it is kept and shown as inconsistent (three reads in all).
	DefaultRetries    = 2
	DefaultRetryDelay = 300 * time.Millisecond

	// staleAfterIntervals: the selected instance counts as stale once its last
	// read is older than this many intervals.
	staleAfterIntervals = 3
)

// Config holds the refresher's dependencies and bounds. Zero values select the
// defaults above; Retries below zero means none.
type Config struct {
	// Root is the canonical search root (Snapshot.Root from Open).
	Root string
	// FS performs discovery and glances: listings and metadata only.
	FS        discovery.FS
	Discovery discovery.Options
	// Exec runs the instances' own scripts, by absolute path.
	Exec  runner.Executor
	Clock Clock
	// Clients is the pool of one client per instance. Pass the supervisor's
	// (SupervisorConfig.Clients) so both share each instance's capability
	// cache; nil makes a pool of its own over Exec.
	Clients *Clients

	Interval      time.Duration
	SelectSettle  time.Duration
	DiscoverEvery time.Duration
	CallTimeout   time.Duration
	Concurrency   int
	Retries       int
	RetryDelay    time.Duration

	// IsOurs says whether an owner record belongs to this session: whether a
	// token in the session's directory matches it (Supervisor.IsOurs). Nil means
	// never: every owner is external.
	IsOurs func(groundhog.Owner) bool
	// OnCoherent, if set, is called with each coherent status the refresher has
	// just recorded, outside its locks, so the supervisor can apply the ownership
	// rule to a token it holds (Supervisor.Observe). It runs on the read's own
	// goroutine and may take as long as one more status call.
	OnCoherent func(ctx context.Context, instance string, st groundhog.Status)
}

func (c *Config) fill() {
	if c.Clock == nil {
		c.Clock = SystemClock{}
	}
	if c.Interval <= 0 {
		c.Interval = DefaultInterval
	}
	if c.SelectSettle <= 0 {
		c.SelectSettle = DefaultSelectSettle
	}
	if c.DiscoverEvery <= 0 {
		c.DiscoverEvery = DefaultDiscoverEvery
	}
	if c.CallTimeout <= 0 {
		c.CallTimeout = DefaultCallTimeout
	}
	if c.Concurrency <= 0 {
		c.Concurrency = DefaultConcurrency
	}
	switch {
	case c.Retries == 0:
		c.Retries = DefaultRetries
	case c.Retries < 0:
		c.Retries = 0
	}
	if c.RetryDelay <= 0 {
		c.RetryDelay = DefaultRetryDelay
	}
}

// Refresher keeps a cache of what is known about each discovered instance and
// refreshes it off the caller's goroutine.
//
// Which scripts run is the reader's decision:
//   - Background and periodic refresh run `capabilities --json` and
//     `status --json` only for the selected instance.
//   - An explicit Refresh, the reader's deliberate act, runs them for every
//     discovered, well-formed instance.
//   - Every other instance is shown from directory listings (discovery.Glance),
//     marked not inspected, or with the time it was last inspected.
//
// A Refresher is safe for concurrent use. Run must be called once.
type Refresher struct {
	cfg Config

	wake    chan struct{} // requests for the loop; capacity 1
	changed chan struct{} // something visible changed; capacity 1
	sem     chan struct{} // bounds simultaneous script calls
	scanMu  sync.Mutex    // one rescan at a time

	mu         sync.Mutex
	root       string
	entries    map[string]*entry
	order      []string
	diags      []discovery.Diagnostic
	scannedAt  time.Time
	scanErr    error
	selected   string
	selectedAt time.Time // when it became selected, by the refresher's clock
	selSeq     int
	refreshQ   bool
	ectx       context.Context // Run's context; cancelled when Run ends
	wg         sync.WaitGroup
}

type entry struct {
	inst   discovery.Instance
	glance discovery.Glance
	client *groundhog.Client
	insp   Inspection

	needNegotiate bool
	busy          bool
	rerun         bool // an explicit refresh arrived while busy
	attempted     bool
	fails         int
	lastDur       time.Duration
	nextAfter     time.Time
}

// NewRefresher builds a refresher. It starts nothing.
func NewRefresher(cfg Config) (*Refresher, error) {
	if cfg.FS == nil || cfg.Exec == nil || cfg.Root == "" {
		return nil, errors.New("core: refresher needs a root, a file system and an executor")
	}
	cfg.fill()
	if cfg.Clients == nil {
		cfg.Clients = NewClients(cfg.Exec)
	}
	return &Refresher{
		cfg:     cfg,
		wake:    make(chan struct{}, 1),
		changed: make(chan struct{}, 1),
		sem:     make(chan struct{}, cfg.Concurrency),
		root:    cfg.Root,
		entries: map[string]*entry{},
	}, nil
}

// Config returns the configuration with defaults filled in.
func (r *Refresher) Config() Config { return r.cfg }

// Changed signals, conflated to at most one pending signal, that View would
// now return something different. Read the view after receiving.
func (r *Refresher) Changed() <-chan struct{} { return r.changed }

func (r *Refresher) signal() {
	select {
	case r.changed <- struct{}{}:
	default:
	}
}

func (r *Refresher) poke() {
	select {
	case r.wake <- struct{}{}:
	default:
	}
}

// Select names the instance the refresher polls: an Instance.Path, or empty for
// none. Selection is by instance key, never by position. It reads nothing by
// itself; the loop reads the instance after SelectSettle if nothing newer was
// selected, and not at all when it was read recently.
func (r *Refresher) Select(key string) {
	r.mu.Lock()
	if key == r.selected {
		r.mu.Unlock()
		return
	}
	r.selected = key
	r.selectedAt = r.cfg.Clock.Now()
	r.selSeq++
	r.mu.Unlock()
	r.signal() // the view marks the selected instance
	r.poke()
}

// Refresh is the reader's explicit request: rescan, then inspect every
// discovered instance. It returns at once.
func (r *Refresher) Refresh() {
	r.mu.Lock()
	r.refreshQ = true
	r.mu.Unlock()
	r.poke()
}

// Reinspect reads one instance's capabilities and status now, without waiting
// for the interval: the reader has just acted on it (Run, Stop), so what the
// terminal shows should catch up with protocol status. It returns at once, runs
// only that instance's script (the one the reader just acted on) and, if a read
// is already in flight, queues one more behind it, since the read in flight may
// have begun before the action. An unknown or malformed key does nothing.
func (r *Refresher) Reinspect(key string) {
	r.mu.Lock()
	e := r.entries[key]
	ok := e != nil && len(e.inst.Malformed) == 0
	r.mu.Unlock()
	if ok {
		r.start(e, true)
	}
}

// spawn runs f on a tracked goroutine under Run's context. Once that context
// is cancelled it starts nothing, which is what lets Run wait for the goroutines it started.
func (r *Refresher) spawn(f func(ctx context.Context)) bool {
	r.mu.Lock()
	ctx := r.ectx
	if ctx == nil || ctx.Err() != nil {
		r.mu.Unlock()
		return false
	}
	r.wg.Add(1)
	r.mu.Unlock()
	go func() {
		defer r.wg.Done()
		f(ctx)
	}()
	return true
}

// Run drives periodic refresh until ctx ends, then cancels every call in flight,
// waits for them and returns ctx's error. Call Rescan first to fill the view.
func (r *Refresher) Run(ctx context.Context) error {
	runCtx, cancel := context.WithCancel(ctx)
	r.mu.Lock()
	r.ectx = runCtx
	r.mu.Unlock()
	clk := r.cfg.Clock
	poll, rescan := clk.After(r.cfg.Interval), clk.After(r.cfg.DiscoverEvery)
	var settle <-chan time.Time
	seenSeq := 0
	for {
		select {
		case <-ctx.Done():
			cancel()
			r.wg.Wait()
			return ctx.Err()
		case <-r.wake:
			r.mu.Lock()
			refreshQ, sel := r.refreshQ, r.selSeq != seenSeq
			r.refreshQ, seenSeq = false, r.selSeq
			r.mu.Unlock()
			if sel {
				settle = clk.After(r.cfg.SelectSettle)
			}
			if refreshQ {
				r.spawn(r.explicit)
			}
		case <-settle:
			settle = nil
			r.pollSelected()
		case <-poll:
			poll = clk.After(r.cfg.Interval)
			r.pollSelected()
			r.signalSelectedAge()
		case <-rescan:
			rescan = clk.After(r.cfg.DiscoverEvery)
			r.spawn(func(c context.Context) {
				if r.scanMu.TryLock() {
					r.scanMu.Unlock()
					_ = r.Rescan(c)
				}
			})
		}
	}
}

// explicit is the reader's Refresh: rescan, then run every well-formed
// instance, the selected one first, at most Concurrency at a time.
func (r *Refresher) explicit(ctx context.Context) {
	_ = r.Rescan(ctx)
	r.mu.Lock()
	var list []*entry
	for _, k := range r.order {
		if e := r.entries[k]; e != nil && len(e.inst.Malformed) == 0 {
			if k == r.selected {
				list = append([]*entry{e}, list...)
			} else {
				list = append(list, e)
			}
		}
	}
	r.mu.Unlock()
	for _, e := range list {
		r.start(e, true)
	}
}

// pollSelected starts a background read of the selected instance when one is due.
func (r *Refresher) pollSelected() {
	now := r.cfg.Clock.Now()
	r.mu.Lock()
	e := r.entries[r.selected]
	due := e != nil && len(e.inst.Malformed) == 0 && !e.busy && (!e.attempted || !now.Before(e.nextAfter))
	r.mu.Unlock()
	if due {
		r.start(e, false)
	}
}

// signalSelectedAge tells the view, once per interval, to recompute what
// depends on the clock alone: how long ago the selected instance was read and
// whether that has become stale. Nothing else announces this when reads stop
// arriving.
func (r *Refresher) signalSelectedAge() {
	r.mu.Lock()
	e := r.entries[r.selected]
	show := e != nil && e.insp.Last != nil
	r.mu.Unlock()
	if show {
		r.signal()
	}
}

// start runs one inspection of e on its own goroutine, unless one is already in
// flight; an explicit request then reruns once when it ends.
func (r *Refresher) start(e *entry, explicit bool) {
	r.mu.Lock()
	if e.busy {
		if explicit {
			e.rerun = true
		}
		r.mu.Unlock()
		return
	}
	e.busy = true
	r.mu.Unlock()
	if !r.spawn(func(ctx context.Context) { r.inspect(ctx, e, explicit) }) {
		r.mu.Lock()
		e.busy = false
		r.mu.Unlock()
	}
}

// Rescan rediscovers instances beneath the root and glances at each. It runs no
// script. Instances that persist keep their cache; new ones start not
// inspected; vanished ones are dropped. On a failed scan the previous list
// stays and the error is shown. It is safe to call before Run.
func (r *Refresher) Rescan(ctx context.Context) error {
	r.scanMu.Lock()
	defer r.scanMu.Unlock()

	res, err := discovery.Scan(ctx, r.cfg.FS, r.root, r.cfg.Discovery)
	if err != nil {
		if ctx.Err() == nil {
			r.mu.Lock()
			r.scanErr = err
			r.mu.Unlock()
			r.signal()
		}
		return err
	}
	glances := make([]discovery.Glance, len(res.Instances))
	for i, in := range res.Instances {
		g, gerr := discovery.GlanceInstance(ctx, r.cfg.FS, in.Path)
		if gerr != nil {
			return gerr
		}
		glances[i] = g
	}
	now := r.cfg.Clock.Now()

	r.mu.Lock()
	defer r.mu.Unlock()
	changed := r.scanErr != nil || !sameDiags(r.diags, res.Diagnostics)
	next := make(map[string]*entry, len(res.Instances))
	order := make([]string, 0, len(res.Instances))
	for i, in := range res.Instances {
		e := r.entries[in.Path]
		if e == nil {
			e = &entry{}
			if c, cerr := r.cfg.Clients.For(in.Path); cerr == nil {
				e.client = c
			} else {
				e.insp.Err, e.insp.ErrAt = cerr, now
			}
			e.inst, e.glance = in, glances[i]
			changed = true
		} else {
			if !e.glance.Script.Equal(glances[i].Script) {
				e.needNegotiate = true // a different script may answer differently
			}
			if !e.glance.Equal(glances[i]) || !sameInstance(e.inst, in) {
				changed = true
			}
			e.inst, e.glance = in, glances[i]
		}
		next[in.Path] = e
		order = append(order, in.Path)
	}
	if len(next) != len(r.entries) {
		changed = true
	}
	r.entries, r.order, r.diags, r.scanErr, r.scannedAt = next, order, res.Diagnostics, nil, now
	if changed {
		r.signal()
	}
	return nil
}

func sameInstance(a, b discovery.Instance) bool {
	if a.Path != b.Path || a.Display != b.Display || len(a.Malformed) != len(b.Malformed) {
		return false
	}
	for i := range a.Malformed {
		if a.Malformed[i] != b.Malformed[i] {
			return false
		}
	}
	return true
}

func sameDiags(a, b []discovery.Diagnostic) bool {
	if len(a) != len(b) {
		return false
	}
	for i := range a {
		if a[i].Path != b[i].Path || fmt.Sprint(a[i].Err) != fmt.Sprint(b[i].Err) {
			return false
		}
	}
	return true
}

// inspect reads one instance: a fresh glance, a capability negotiation when
// needed, then `status --json`, re-read a bounded number of times while it
// reports itself inconsistent.
func (r *Refresher) inspect(ctx context.Context, e *entry, explicit bool) {
	defer func() {
		r.mu.Lock()
		e.busy = false
		again := e.rerun && ctx.Err() == nil
		e.rerun = false
		r.mu.Unlock()
		if again {
			r.start(e, true)
		}
	}()
	select {
	case r.sem <- struct{}{}:
		defer func() { <-r.sem }()
	case <-ctx.Done():
		return
	}
	clk := r.cfg.Clock
	began := clk.Now()

	r.mu.Lock()
	client, path := e.client, e.inst.Path
	needNeg := explicit || e.insp.Support == nil || e.needNegotiate
	r.mu.Unlock()
	if client == nil {
		return
	}

	glance, _ := discovery.GlanceInstance(ctx, r.cfg.FS, path)
	r.mu.Lock()
	if !e.glance.Equal(glance) {
		if !e.glance.Script.Equal(glance.Script) {
			needNeg = true
		}
		e.glance = glance
		r.signal()
	}
	r.mu.Unlock()

	if needNeg {
		sup, err := client.Negotiate(ctx, r.callTimeout())
		if err != nil {
			r.fail(ctx, e, err, began)
			return
		}
		r.mu.Lock()
		if old := e.insp.Support; old == nil || old.Controllable != sup.Controllable || old.Inspectable != sup.Inspectable || old.Reason != sup.Reason {
			r.signal()
		}
		e.insp.Support, e.needNegotiate = &sup, false
		r.mu.Unlock()
	}
	r.mu.Lock()
	sup := e.insp.Support
	r.mu.Unlock()
	if !sup.Inspectable {
		// Legacy or unknown protocol: nothing further runs. Shown read-only.
		r.mu.Lock()
		e.insp.Err, e.attempted, e.fails = nil, true, 0
		e.setNext(r.cfg, clk.Now(), began)
		r.mu.Unlock()
		r.signal()
		return
	}

	st, attempts, ok, err := readCoherent(ctx, client, readPolicy{
		Tries: r.cfg.Retries + 1, Delay: r.cfg.RetryDelay, Opts: []groundhog.Option{r.callTimeout()},
		Wait: func(ctx context.Context, d time.Duration) error {
			select {
			case <-clk.After(d):
				return nil
			case <-ctx.Done():
				return ctx.Err()
			}
		},
	})
	switch {
	case err != nil:
		r.fail(ctx, e, err, began)
		return
	case !ok && ctx.Err() != nil:
		return // cut short while waiting to re-read: nothing worth keeping
	}
	r.record(e, st, attempts, began)
	if ok && r.cfg.OnCoherent != nil {
		r.cfg.OnCoherent(ctx, path, st)
	}
}

// callTimeout is the refresher's bound on one script call, shorter than the
// shared client's default.
func (r *Refresher) callTimeout() groundhog.Option { return groundhog.WithTimeout(r.cfg.CallTimeout) }

// setNext schedules the earliest next background read: the interval, doubled
// for each consecutive failure (up to eight times), and never sooner than four
// times the last call's duration.
func (e *entry) setNext(cfg Config, now, began time.Time) {
	e.lastDur = now.Sub(began)
	wait := cfg.Interval << min(e.fails, 3)
	wait = max(wait, 4*e.lastDur)
	e.nextAfter = now.Add(wait)
}

func (r *Refresher) fail(ctx context.Context, e *entry, err error, began time.Time) {
	if ctx.Err() != nil {
		return // cancelled by Quit or a new root: not the instance's failure
	}
	now := r.cfg.Clock.Now()
	r.mu.Lock()
	defer r.mu.Unlock()
	prev := e.insp.Err
	e.insp.Err, e.insp.ErrAt = err, now
	e.attempted = true
	e.fails++
	e.setNext(r.cfg, now, began)
	if prev == nil || prev.Error() != err.Error() {
		r.signal()
	}
}

// record stores a read and announces it. Every completed read signals: the
// signal channel holds one pending signal, so a burst of reads costs the
// terminal one rebuild, and a rebuild that was not needed costs less than a view
// that should have changed and did not (a runner killed by a signal reads as
// unknown without any counter moving).
func (r *Refresher) record(e *entry, st groundhog.Status, attempts int, began time.Time) {
	now := r.cfg.Clock.Now()
	r.mu.Lock()
	defer r.mu.Unlock()
	e.insp.Last = &Observation{Status: st, TakenAt: now}
	e.insp.Err, e.insp.Attempts = nil, attempts
	e.attempted, e.fails = true, 0
	e.setNext(r.cfg, now, began)
	r.signal()
}

// View returns an immutable copy of the current state.
func (r *Refresher) View() View {
	now := r.cfg.Clock.Now()
	r.mu.Lock()
	defer r.mu.Unlock()
	v := View{Root: r.root, Diagnostics: append([]discovery.Diagnostic(nil), r.diags...),
		DiscoveredAt: r.scannedAt, DiscoverErr: r.scanErr, Selected: r.selected}
	for _, k := range r.order {
		e := r.entries[k]
		iv := InstanceView{Instance: e.inst, Files: e.glance, Inspection: e.insp, Selected: k == r.selected}
		iv.Class = r.classify(e)
		iv.Health = r.health(e, iv.Selected, now)
		v.Instances = append(v.Instances, iv)
	}
	return v
}

func (r *Refresher) classify(e *entry) Class {
	in := e.insp
	switch {
	case len(e.inst.Malformed) > 0:
		return ClassMalformed
	case in.Support != nil && !in.Support.Inspectable:
		return ClassLegacy
	case in.Last == nil && in.Err != nil:
		return ClassFailing
	case in.Last == nil:
		return ClassNotInspected
	}
	owned := in.Last.Status.Owner != nil && r.cfg.IsOurs != nil && r.cfg.IsOurs(*in.Last.Status.Owner)
	return classifyRead(in.Last.Status, in.Support, owned)
}

// classifyRead is the class of an instance whose script answered: who owns it,
// as the session sees it, and whether it supports supervised control. ours says
// the recorded owner, if any, is this session's. The supervisor applies it to
// its own fresh read, so that the rule it checks at Run is the palette's.
func classifyRead(st groundhog.Status, sup *groundhog.Support, ours bool) Class {
	switch {
	case st.Owner != nil && ours:
		return ClassOwnedBySession
	case st.Owner != nil:
		return ClassExternal
	case sup != nil && !sup.Controllable:
		return ClassUnsupported
	}
	return ClassUnowned
}

func (r *Refresher) health(e *entry, selected bool, now time.Time) Health {
	in := e.insp
	switch {
	case in.Last == nil:
		return HealthOK // no status is shown, so there is nothing to trust or doubt
	case !in.Last.Status.Consistent:
		return HealthInconsistent
	case selected && now.Sub(since(in.Last.TakenAt, r.selectedAt)) > staleAfterIntervals*r.cfg.Interval:
		// Stale counts from the later of the last read and the moment the
		// instance became selected: one read long ago is not "should have been
		// re-read" until it has been selected for several intervals, and the
		// read the selection starts takes a moment to arrive.
		return HealthStale
	}
	return HealthOK
}

// since returns the later of two times.
func since(a, b time.Time) time.Time {
	if b.After(a) {
		return b
	}
	return a
}
