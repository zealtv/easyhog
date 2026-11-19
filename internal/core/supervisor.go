package core

import (
	"context"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"runtime"
	"slices"
	"sort"
	"sync"
	"time"

	"github.com/zealtv/easyhog/internal/groundhog"
	"github.com/zealtv/easyhog/internal/runner"
)

// Supervisor defaults. See SupervisorConfig.
const (
	// DefaultStartWait is how long Run watches a freshly started runner for
	// `status` to show it running, or for the process to end, before reporting
	// that it was started and not yet seen.
	DefaultStartWait = 5 * time.Second
	// DefaultQuitWait is how long Quit waits for each instance to finish before
	// it stops and releases it. It bounds each instance's `stop`, which waits for
	// the consumer call the runner admitted; at the bound the runner gets SIGTERM.
	DefaultQuitWait = 30 * time.Second
	// DefaultReapWait is how long to wait for a runner process to end after
	// `stop` (it exits as soon as it has released its lock), and again after
	// SIGTERM. Past the second wait the runner is reported, never killed.
	DefaultReapWait = 5 * time.Second
	// DefaultReleaseWait bounds Quit's `release` (with its busy retries) once the
	// runner has ended. It runs in real time, outside the quit's context, so a
	// second interrupt still releases.
	DefaultReleaseWait = 10 * time.Second
	// DefaultPollEvery is the pace of the watches on `status`.
	DefaultPollEvery = 250 * time.Millisecond
	// DefaultBusyRetries and DefaultBusyDelay bound the retry of exit 4. Each
	// attempt already waits about three seconds for the mutation lock inside
	// the script, so a few more tries ride out a long
	// tick and no more.
	DefaultBusyRetries = 4
	DefaultBusyDelay   = 250 * time.Millisecond
	// DefaultMaxRunners bounds the runners one session supervises.
	DefaultMaxRunners = 16
)

// SupervisorConfig holds the supervisor's dependencies and bounds. Zero values
// select the defaults above.
type SupervisorConfig struct {
	// Exec runs the instances' own scripts for the short protocol calls
	// (`status`, `enroll`, `resume`, `stop`, `release`).
	Exec runner.Executor
	// Launcher starts `run`. It is separate from Exec on purpose: a runner
	// needs file output and must never be killed (see runner.Spawner).
	Launcher runner.Launcher
	// StateDir is the user's private state directory. Tokens and runner logs
	// live in a 0700 directory beneath it, outside every instance and every
	// repository. Empty selects DefaultStateDir.
	StateDir string
	// Label is the owner label written at enrollment: display text that grants
	// nothing. Empty selects "easyhog-<pid>".
	Label string
	// RunInterval is passed to `run --interval-seconds`; zero leaves the
	// script's own default (60 s).
	RunInterval time.Duration

	StartWait time.Duration
	// QuitWait bounds each instance's `stop` in Quit; every instance's stop runs
	// at once, so it is also what Quit's wait comes to however many there are.
	QuitWait time.Duration
	// ReapWait bounds the wait for a runner process to end, twice at most.
	ReapWait time.Duration
	// ReleaseWait bounds Quit's `release` after the runner has ended.
	ReleaseWait time.Duration
	PollEvery   time.Duration
	BusyRetries int // below zero means none
	BusyDelay   time.Duration
	MaxRunners  int

	// Clients is the pool of one client per instance. Pass it to the refresher
	// too (Config.Clients) so both share each instance's capability cache; nil
	// makes a pool of its own over Exec.
	Clients *Clients

	// Sleep waits d or until ctx ends; tests replace it.
	Sleep func(ctx context.Context, d time.Duration) error
}

func (c *SupervisorConfig) fill() {
	if c.Label == "" {
		c.Label = fmt.Sprintf("easyhog-%d", os.Getpid())
	}
	for _, p := range []struct {
		v *time.Duration
		d time.Duration
	}{
		{&c.StartWait, DefaultStartWait}, {&c.QuitWait, DefaultQuitWait},
		{&c.ReapWait, DefaultReapWait}, {&c.ReleaseWait, DefaultReleaseWait}, {&c.PollEvery, DefaultPollEvery}, {&c.BusyDelay, DefaultBusyDelay},
	} {
		if *p.v <= 0 {
			*p.v = p.d
		}
	}
	switch {
	case c.BusyRetries == 0:
		c.BusyRetries = DefaultBusyRetries
	case c.BusyRetries < 0:
		c.BusyRetries = 0
	}
	if c.MaxRunners <= 0 {
		c.MaxRunners = DefaultMaxRunners
	}
	if c.Sleep == nil {
		c.Sleep = sleepCtx
	}
}

func sleepCtx(ctx context.Context, d time.Duration) error {
	t := time.NewTimer(d)
	defer t.Stop()
	select {
	case <-t.C:
		return nil
	case <-ctx.Done():
		return ctx.Err()
	}
}

// DefaultStateDir is where a session keeps its private directory: the user's
// state directory (XDG_STATE_HOME, or ~/.local/state on Linux; the application
// support directory on macOS), then "easyhog". It is
// never beneath a repository.
func DefaultStateDir() (string, error) {
	if v := os.Getenv("XDG_STATE_HOME"); filepath.IsAbs(v) {
		return filepath.Join(v, "easyhog"), nil
	}
	if runtime.GOOS == "darwin" {
		d, err := os.UserConfigDir()
		if err != nil {
			return "", err
		}
		return filepath.Join(d, "easyhog"), nil
	}
	home, err := os.UserHomeDir()
	if err != nil {
		return "", err
	}
	return filepath.Join(home, ".local", "state", "easyhog"), nil
}

// Phase says where a supervised instance stands in this session.
type Phase int

const (
	// PhaseNone: nothing enrolled in this session (never listed).
	PhaseNone Phase = iota
	// PhaseEnrolled: this session holds the instance's token (it enrolled the
	// instance, or adopted the token from an ended session) and has no runner
	// process of its own running: not yet started, one this session stopped, or
	// an adopted instance whose runner, if it still runs, belongs to the session
	// that ended.
	PhaseEnrolled
	// PhaseRunning: this session's runner process is alive.
	PhaseRunning
	// PhaseExited: the runner process ended without being asked to. It is
	// reported and not restarted; ownership and pause are as Groundhog keeps
	// them, and an explicit Run starts it again.
	PhaseExited
	// PhaseReleased: Quit released the instance and removed its token.
	PhaseReleased
	// PhaseRetained: Quit could not release; the token stays.
	PhaseRetained
)

func (p Phase) String() string {
	switch p {
	case PhaseEnrolled:
		return "enrolled, no runner"
	case PhaseRunning:
		return "running"
	case PhaseExited:
		return "runner ended"
	case PhaseReleased:
		return "released"
	case PhaseRetained:
		return "left owned"
	}
	return "none"
}

// RunnerInfo is a copy of what the supervisor knows about one instance it
// enrolled. It never carries a token's contents; TokenFile is a path.
type RunnerInfo struct {
	Instance string
	Phase    Phase
	// PID is the runner's process id while Phase is PhaseRunning.
	PID int
	// Exit says how the runner ended, for PhaseExited.
	Exit *runner.Exit
	// Log is the runner's output file; the token is TokenFile. Both lie in the
	// session's private directory.
	Log       string
	TokenFile string
	// Notice is a short human line when something wants attention.
	Notice string
	// Adopted is true while this session holds the token by adoption and has not
	// started a runner of its own: a runner still ticking belongs to the session
	// that ended, so Quit can stop and release it but cannot SIGTERM it.
	// AdoptedFrom is when that session was last seen alive (its lock file's
	// modification time, which a live session refreshes), the "ended at" shown.
	Adopted     bool
	AdoptedFrom time.Time
	// Interval is the gap this session's runner keeps between ticks: the
	// supervisor's RunInterval, or DefaultTickInterval when that is zero (the
	// script's own default). For an adopted runner it is the same guess: the
	// session that started it is gone.
	Interval time.Duration
}

// Refusal says why an operation was declined before it changed anything.
type Refusal int

const (
	// RefusedExternal: an owner this session did not enroll holds the instance,
	// whether or not its runner is alive.
	RefusedExternal Refusal = iota + 1
	// RefusedUnsupported: the script lacks the supervised-control capabilities.
	RefusedUnsupported
	// RefusedNotOurs: this session holds no token that controls the instance.
	RefusedNotOurs
	// RefusedInconsistent: status would not settle; refresh and try again.
	RefusedInconsistent
	// RefusedClosing: the session is quitting.
	RefusedClosing
	// RefusedLimit: too many runners already.
	RefusedLimit
	// RefusedNotOffered: Offers, the rule the palette follows, offers nothing
	// for the instance as the fresh read shows it (Detail says why).
	RefusedNotOffered
)

func (r Refusal) String() string {
	switch r {
	case RefusedExternal:
		return "owned by another supervisor"
	case RefusedUnsupported:
		return "does not support supervised control"
	case RefusedNotOurs:
		return "no token of this session's controls it"
	case RefusedInconsistent:
		return "status is not coherent"
	case RefusedClosing:
		return "quitting"
	case RefusedLimit:
		return "too many runners"
	case RefusedNotOffered:
		return "not offered"
	}
	return "refused"
}

// ErrRefused matches every *RefusedError under errors.Is.
var ErrRefused = errors.New("core: operation refused")

// ErrInconsistent marks a status that stayed `consistent:false` (or caught a
// writer mid-update) through every re-read. No control acts on such a status.
var ErrInconsistent = errors.New("core: status stayed inconsistent")

// RefusedError reports an operation declined, with nothing changed beyond what
// Detail says.
type RefusedError struct {
	Op       string // "run", "stop" or "settle"
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

// RunnerExitedError reports a runner that ended while Run was watching it
// start. It is not started again; the log holds what it wrote.
type RunnerExitedError struct {
	Instance string
	Exit     runner.Exit
	Log      string
}

func (e *RunnerExitedError) Error() string {
	return fmt.Sprintf("run %s: the runner ended at startup (%s); see %s", e.Instance, e.Exit, e.Log)
}

// Supervisor starts, stops and shuts down the Groundhog runners whose tokens
// this session holds, and nothing else. Only an explicit Run enrolls;
// discovery and inspection never reach it.
//
// The one ownership rule: this session controls an
// instance when a token in its own session directory matches `status.owner`
// (`owner_id` and `generation`). Any other owner, recorded or not, is external,
// whether or not its runner is alive. A token whose instance shows no owner or
// another owner is deleted, on a coherent status only (Run, and Observe for
// each coherent status the refresher reads). A token in this session's
// directory got there by enrollment or by adoption (Adopt): a session that has
// ended leaves its tokens in its own directory, and a later session moves them
// into its own by atomic rename. It is safe for concurrent use.
//
// Easyhog's session policy, built on the generic primitives (contract, "Ownership,
// locks and enrollment lifecycle"): Run enrolls an unowned compatible instance,
// clears a pause and starts the runner; Stop and Quit stop a runner (each stop
// waits for the consumer call the runner admitted, up to QuitWait, then SIGTERM),
// release ownership and delete the token, and leave the pause alone: Stop for
// one instance the reader chose, Quit for every one at once. An
// adopted instance is never restarted by adoption: Run is still the reader's act.
type Supervisor struct {
	cfg     SupervisorConfig
	changed chan struct{}

	opCtx    context.Context // ends when Quit begins, to cut in-flight operations short
	opCancel context.CancelFunc

	mu       sync.Mutex
	closing  bool
	dir      string   // the session's private directory, made on the first enrollment or adoption
	lock     *os.File // holds the advisory lock that says this session lives; see sessionLock
	seq      int
	sessions map[string]*session
	pending  int // first Runs between their limit check and their enroll's outcome
	// leftovers are the tokens the last Adopt left where they were.
	leftovers []Leftover

	quitDone   chan struct{}
	quitReport QuitReport
	quitOnce   sync.Once
}

// session is one instance's supervision. Fields after op are guarded by
// Supervisor.mu; op serializes operations on the instance.
type session struct {
	key    string
	client *groundhog.Client
	op     chan struct{}

	phase   Phase
	token   string
	log     string
	ownerID string
	gen     int64
	child   runner.Child
	exit    *runner.Exit
	asked   bool // Quit asked the runner to end: its exit is no crash
	notice  string
	// adopted: the token came from an ended session and no runner of this
	// session's own has been started since; adoptedFrom is that session's last
	// sign of life.
	adopted     bool
	adoptedFrom time.Time
}

// NewSupervisor builds a supervisor. It touches no file and starts nothing; the
// private directory is made when the first instance is enrolled.
func NewSupervisor(cfg SupervisorConfig) (*Supervisor, error) {
	if cfg.Exec == nil || cfg.Launcher == nil {
		return nil, errors.New("core: supervisor needs an executor and a launcher")
	}
	cfg.fill()
	if cfg.Clients == nil {
		cfg.Clients = NewClients(cfg.Exec)
	}
	ctx, cancel := context.WithCancel(context.Background())
	return &Supervisor{
		cfg: cfg, changed: make(chan struct{}, 1),
		opCtx: ctx, opCancel: cancel,
		sessions: map[string]*session{},
		quitDone: make(chan struct{}),
	}, nil
}

// Changed signals, conflated, that Runners would now return something different.
func (s *Supervisor) Changed() <-chan struct{} { return s.changed }

func (s *Supervisor) signal() {
	select {
	case s.changed <- struct{}{}:
	default:
	}
}

// IsOurs says whether an owner record is the one a token in this session's
// directory names (the ownership rule). Pass it as Config.IsOurs so the
// refresher classifies those instances as owned by this session and every other
// owner as external.
func (s *Supervisor) IsOurs(o groundhog.Owner) bool {
	s.mu.Lock()
	defer s.mu.Unlock()
	for _, sess := range s.sessions {
		if sess.holds() && sess.matches(&o) {
			return true
		}
	}
	return false
}

// holds reports whether the session has a live token: enrolled or adopted, and
// not released. The caller holds Supervisor.mu.
func (sess *session) holds() bool {
	switch sess.phase {
	case PhaseEnrolled, PhaseRunning, PhaseExited:
		return sess.token != ""
	}
	return false
}

// matches is the ownership rule's comparison: the token's owner id and
// generation against `status.owner`. The caller holds Supervisor.mu.
func (sess *session) matches(o *groundhog.Owner) bool {
	return o != nil && sess.ownerID != "" && o.ID == sess.ownerID && o.Generation == sess.gen
}

// Runner returns one instance's supervision, if this session ever held a token for it.
func (s *Supervisor) Runner(key string) (RunnerInfo, bool) {
	s.mu.Lock()
	defer s.mu.Unlock()
	sess := s.sessions[key]
	if sess == nil || sess.phase == PhaseNone {
		return RunnerInfo{}, false
	}
	return s.infoOf(sess), true
}

// infoOf is sess.info with the runner's interval; the caller holds Supervisor.mu.
func (s *Supervisor) infoOf(sess *session) RunnerInfo {
	in := sess.info()
	in.Interval = s.cfg.RunInterval
	if in.Interval <= 0 {
		in.Interval = DefaultTickInterval
	}
	return in
}

// Runners returns every instance this session held a token for, in key order.
func (s *Supervisor) Runners() []RunnerInfo {
	s.mu.Lock()
	defer s.mu.Unlock()
	keys := make([]string, 0, len(s.sessions))
	for k, sess := range s.sessions {
		if sess.phase != PhaseNone {
			keys = append(keys, k)
		}
	}
	sort.Strings(keys)
	out := make([]RunnerInfo, 0, len(keys))
	for _, k := range keys {
		out = append(out, s.infoOf(s.sessions[k]))
	}
	return out
}

// Stopping counts the instances a Quit would have to wind down or wait for: those
// this session holds tokens for, and those with an operation still in flight (a first
// Run that is still enrolling has no runner yet, and may be about to own the
// instance).
func (s *Supervisor) Stopping() int {
	s.mu.Lock()
	defer s.mu.Unlock()
	n := 0
	for _, sess := range s.sessions {
		if sess.phase != PhaseNone || len(sess.op) > 0 {
			n++
		}
	}
	return n
}

// info copies the session; the caller holds Supervisor.mu.
func (sess *session) info() RunnerInfo {
	in := RunnerInfo{Instance: sess.key, Phase: sess.phase, Log: sess.log, TokenFile: sess.token, Notice: sess.notice,
		Adopted: sess.adopted && sess.child == nil, AdoptedFrom: sess.adoptedFrom}
	if sess.child != nil {
		in.PID = sess.child.PID()
	}
	if sess.exit != nil {
		x := *sess.exit
		in.Exit = &x
	}
	return in
}

// acquire takes the instance's operation slot, or gives up when ctx ends. A
// free slot is taken whatever ctx says: select alone picks at random between a
// free slot and an ended context, which would report "busy" for an idle
// instance.
func (sess *session) acquire(ctx context.Context) error {
	select {
	case sess.op <- struct{}{}:
		return nil
	default:
	}
	select {
	case sess.op <- struct{}{}:
		return nil
	case <-ctx.Done():
		return ctx.Err()
	}
}

func (sess *session) release() { <-sess.op }

// sessionFor returns the instance's session, making an empty one on first use.
func (s *Supervisor) sessionFor(op, key string) (*session, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.closing {
		return nil, &RefusedError{Op: op, Instance: key, Reason: RefusedClosing}
	}
	if sess := s.sessions[key]; sess != nil {
		return sess, nil
	}
	c, err := s.cfg.Clients.For(key)
	if err != nil {
		return nil, err
	}
	sess := &session{key: key, client: c, op: make(chan struct{}, 1)}
	s.sessions[key] = sess
	return sess, nil
}

// opContext joins ctx with the supervisor's quit signal.
func (s *Supervisor) opContext(ctx context.Context) (context.Context, context.CancelFunc) {
	c, cancel := context.WithCancel(ctx)
	stop := context.AfterFunc(s.opCtx, cancel)
	return c, func() { stop(); cancel() }
}

// retryBusy repeats f while it fails with exit 4 (busy), a bounded number of
// times with a doubling delay. Any other result returns at once.
func (s *Supervisor) retryBusy(ctx context.Context, f func() error) error {
	delay := s.cfg.BusyDelay
	for i := 0; ; i++ {
		err := f()
		if err == nil || !groundhog.IsRetryable(err) || i >= s.cfg.BusyRetries {
			return err
		}
		if s.cfg.Sleep(ctx, delay) != nil {
			return err
		}
		delay *= 2
	}
}

// readStatusForRun is readStatus for the read before `enroll`: a status that
// stays incoherent only for an interrupted journal write (SettlesOnRun) is
// returned too, for checkOffered to judge, since `enroll` settles it.
func (s *Supervisor) readStatusForRun(ctx context.Context, c *groundhog.Client) (groundhog.Status, error) {
	st, _, ok, err := readCoherent(ctx, c, readPolicy{Tries: 3, Delay: s.cfg.BusyDelay, Wait: s.cfg.Sleep, RetryBusy: true})
	switch {
	case err != nil:
		return groundhog.Status{}, err
	case !ok && !SettlesOnRun(st):
		return groundhog.Status{}, ErrInconsistent
	}
	return st, nil
}

// readStatus reads a coherent status: consistent, and not caught mid-write
// (an odd journal generation). It re-reads a few times for a busy script or an
// unsettled status (readCoherent), and otherwise returns the client's error.
func (s *Supervisor) readStatus(ctx context.Context, c *groundhog.Client) (groundhog.Status, error) {
	st, _, ok, err := readCoherent(ctx, c, readPolicy{Tries: 3, Delay: s.cfg.BusyDelay, Wait: s.cfg.Sleep, RetryBusy: true})
	switch {
	case err != nil:
		return groundhog.Status{}, err
	case !ok:
		return groundhog.Status{}, ErrInconsistent
	}
	return st, nil
}

// checkOffered applies Offers, the rule the palette follows, to the fresh
// coherent read Run or a settling action has just taken, so the palette's rule is
// checked at the protocol and not only on the snapshot the reader saw. (Stop takes
// no read: see Supervisor.Stop.) The action may run
// when the rule offers any action: Offers picks which of Run and Stop the
// palette lists, and Run is idempotent here (Run on a running
// instance changes nothing, Run on a paused one clears the pause). sup is the
// capability negotiation just made, if the caller made one.
//
// A recorded owner that no token of this session's matches is external, whether
// or not its runner is alive; the refusal says so as it always has.
func (s *Supervisor) checkOffered(op string, sess *session, st groundhog.Status, sup *groundhog.Support) error {
	return s.checkOfferedAction(op, sess, st, sup, "", "")
}

// checkOfferedAction is checkOffered for an action in particular: when want is
// not empty it must be among what OffersOn gives for item (the schedule item the
// action was asked on, "" for the instance), so Settle applies the very rule the
// palette showed, to its own fresh read.
func (s *Supervisor) checkOfferedAction(op string, sess *session, st groundhog.Status, sup *groundhog.Support, want Action, item string) error {
	iv, held := s.freshView(sess, st, sup)
	actions, why := OffersOn(iv, held, item)
	switch {
	case len(actions) > 0 && (want == "" || slices.Contains(actions, want)):
		return nil
	case len(actions) > 0:
		return &RefusedError{Op: op, Instance: sess.key, Reason: RefusedNotOffered,
			Detail: "nothing to settle: the fresh read lists no " + settleWhat(want, item) + " here"}
	case iv.Class == ClassExternal:
		return &RefusedError{Op: op, Instance: sess.key, Reason: RefusedExternal,
			Detail: fmt.Sprintf("owner %q is recorded; ownership and runner liveness are separate, and a stopped owner is not available for takeover", st.Owner.Label)}
	}
	return &RefusedError{Op: op, Instance: sess.key, Reason: RefusedNotOffered, Detail: why}
}

// freshView is the instance as the rules see it for a fresh read: its class by the
// ownership rule, its health (a status that is not coherent is only ever let
// through for a journal write Run settles), and what this session holds for it
// (nil when it never enrolled it).
func (s *Supervisor) freshView(sess *session, st groundhog.Status, sup *groundhog.Support) (InstanceView, *RunnerInfo) {
	s.mu.Lock()
	var held *RunnerInfo
	if sess.phase != PhaseNone {
		info := sess.info()
		held = &info
	}
	ours := sess.holds() && sess.matches(st.Owner)
	s.mu.Unlock()
	health := HealthOK
	if !st.Consistent {
		health = HealthInconsistent
	}
	return InstanceView{Class: classifyRead(st, sup, ours), Health: health,
		Inspection: Inspection{Support: sup, Last: &Observation{Status: st}}}, held
}

// settleWhat names the deliveries a settling action acts on, for a refusal.
func settleWhat(a Action, item string) string {
	what := "failed deliveries"
	if a == ActionRecoverAccept || a == ActionRecoverRetry {
		what = "uncertain deliveries"
	}
	if item != "" {
		what += " for " + item
	}
	return what
}
