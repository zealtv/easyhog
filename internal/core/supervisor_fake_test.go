//go:build unix

package core

import (
	"context"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/zealtv/easyhog/internal/runner"
)

// A stateful stand-in for the Groundhog script and for the process launcher, so
// the supervisor's sequences run without any process. It follows the contract's
// rules where the supervisor depends on them: enroll needs no owner and writes a
// token file with a secret and nonsecret metadata; the control commands check
// the token against the owner (exit 3); release needs a stopped runner (exit 4)
// and, without --leave-unresolved, no unresolved attempt (exit 5); stop waits
// for a consumer call in flight to end (the test ends it, by clearing inflight)
// and then ends the runner, as Groundhog f189ad2 does; only SIGTERM to the
// runner (fakeChild.Terminate) detaches from a call, leaving the delivery
// uncertain.

const fakeSecret = "SECRET-TOKEN-0123456789abcdef"

type ghOwner struct {
	id    string
	gen   int64
	label string
}

type ghInst struct {
	owner        *ghOwner
	lastGen      int64
	paused       bool
	runner       string   // "stopped", "running" or "stopping" (a stop requested, the runner not yet ended)
	inflight     bool     // a consumer call is in flight; stop waits until a test clears it
	uncertain    bool     // SIGTERM detached a call in flight
	oldStop      bool     // the stop of Groundhog 0034706 and earlier: detaches from a call in flight at once
	settled      int      // accepted deliveries a status --active read counts instead of listing
	caps         []string // nil: every capability, the optional status-active-v1 and release-unresolved-v1 too
	inconsistent int      // the next n status reads report consistent:false
	// dels are further deliveries beyond the single in-flight or uncertain "d-1";
	// retry and recover act on them as the script does (a live runner: exit 4).
	dels []*fakeDel
	// journalOdd makes status say consistent:false with journal_write_in_progress,
	// as an interrupted journal write does, until a command takes the mutation
	// lock (enroll here). journalSticky keeps it odd past that.
	journalOdd, journalSticky bool
}

// alive reports a runner that has not ended, whether or not a stop was asked of it.
func (s *ghInst) alive() bool { return s.runner == "running" || s.runner == "stopping" }

type fakeDel struct {
	id, state, item string // item is the schedule path, "" for none
	decision        string // the recover decision recorded, once recovered
}

type fakeGH struct {
	mu     sync.Mutex
	insts  map[string]*ghInst
	calls  []string // "<instance> <command> <args…>", in order
	counts map[string]int
	nextID int
	// hook runs before the default behavior of a command, outside the lock.
	hook func(ctx context.Context, inst, cmd string, n int) (done bool, stdout string, err error)
	// launcher is told when stop or release must end a child.
	launcher *fakeLauncher
}

func newFakeGH() *fakeGH {
	return &fakeGH{insts: map[string]*ghInst{}, counts: map[string]int{}}
}

func (g *fakeGH) add(inst string) *ghInst {
	g.mu.Lock()
	defer g.mu.Unlock()
	s := &ghInst{runner: "stopped"}
	g.insts[inst] = s
	return s
}

func (g *fakeGH) update(inst string, f func(*ghInst)) {
	g.mu.Lock()
	defer g.mu.Unlock()
	f(g.insts[inst])
}

func (g *fakeGH) state(inst string) ghInst {
	g.mu.Lock()
	defer g.mu.Unlock()
	return *g.insts[inst]
}

func (g *fakeGH) count(inst, cmd string) int {
	g.mu.Lock()
	defer g.mu.Unlock()
	return g.counts[inst+" "+cmd]
}

// commands lists the command names run against inst, in order.
func (g *fakeGH) commands(inst string) []string {
	g.mu.Lock()
	defer g.mu.Unlock()
	var out []string
	for _, c := range g.calls {
		if rest, ok := strings.CutPrefix(c, inst+" "); ok {
			out = append(out, strings.Fields(rest)[0])
		}
	}
	return out
}

func (g *fakeGH) allCalls() []string {
	g.mu.Lock()
	defer g.mu.Unlock()
	return append([]string(nil), g.calls...)
}

func tokenArg(args []string) string {
	for i, a := range args {
		if a == "--token-file" && i+1 < len(args) {
			return args[i+1]
		}
	}
	return ""
}

type fakeToken struct {
	Schema     int    `json:"schema_version"`
	Token      string `json:"token"`
	Instance   string `json:"instance"`
	OwnerID    string `json:"owner_id"`
	Generation int64  `json:"generation"`
}

func (g *fakeGH) Run(ctx context.Context, inv runner.Invocation) error {
	inst, cmd := inv.Dir, inv.Args[0]
	g.mu.Lock()
	g.calls = append(g.calls, inst+" "+strings.Join(inv.Args, " "))
	g.counts[inst+" "+cmd]++
	n := g.counts[inst+" "+cmd]
	hook := g.hook
	g.mu.Unlock()
	if hook != nil {
		if done, out, err := hook(ctx, inst, cmd, n); done {
			if out != "" && inv.Stdout != nil {
				_, _ = inv.Stdout.Write([]byte(out))
			}
			return err
		}
	}
	if cmd == "enroll" {
		label := ""
		for i, a := range inv.Args {
			if a == "--owner-label" {
				label = inv.Args[i+1]
			}
		}
		return g.enroll(inst, tokenArg(inv.Args), label)
	}
	g.mu.Lock()
	defer g.mu.Unlock()
	s := g.insts[inst]
	if s == nil {
		return exitStatus(1)
	}
	write := func(v string) {
		if inv.Stdout != nil {
			_, _ = inv.Stdout.Write([]byte(v))
		}
	}
	if cmd == "capabilities" {
		if s.caps != nil {
			write(capsJSON(s.caps...))
		} else {
			write(capsJSON(append(controlCaps(), "status-active-v1", "release-unresolved-v1")...))
		}
		return nil
	}
	if cmd == "status" {
		write(g.statusDoc(inst, s, slices.Contains(inv.Args, "--active")))
		if s.inconsistent > 0 {
			s.inconsistent--
		}
		return nil
	}
	tok := tokenArg(inv.Args)
	// Token commands.
	if s.owner == nil {
		return exitStatus(3)
	}
	data, err := os.ReadFile(tok)
	if err != nil {
		return exitStatus(2)
	}
	var ft fakeToken
	if json.Unmarshal(data, &ft) != nil {
		return exitStatus(2)
	}
	if ft.OwnerID != s.owner.id || ft.Generation != s.owner.gen {
		return exitStatus(3)
	}
	switch cmd {
	case "pause":
		s.paused = true
	case "resume":
		s.paused = false
	case "stop":
		// Close admission, wait for the admitted call, then end the runner. The
		// wait ends when a test clears inflight, or when the caller gives up on
		// `stop` (its context ends): the runner then lives on, still waiting.
		if s.oldStop && s.inflight {
			s.inflight, s.uncertain = false, true // detached, never killed
		}
		if s.runner == "running" {
			s.runner = "stopping" // the request is recorded: it stays so if the caller gives up
		}
		for s.inflight && s.runner == "stopping" {
			g.mu.Unlock()
			select {
			case <-ctx.Done():
				g.mu.Lock()
				return ctx.Err()
			case <-time.After(2 * time.Millisecond):
			}
			g.mu.Lock()
		}
		s.runner = "stopped"
		if g.launcher != nil {
			g.mu.Unlock()
			g.launcher.endChild(inst, runner.Exit{})
			g.mu.Lock()
		}
	case "retry", "recover":
		if s.alive() {
			return exitStatus(4) // "a runner is still alive; stop it first"
		}
		return g.settle(s, cmd, inv.Args)
	case "release":
		switch {
		case s.alive():
			return exitStatus(4)
		case (s.uncertain || s.inflight) && !slices.Contains(inv.Args, "--leave-unresolved"):
			return exitStatus(5)
		}
		s.owner = nil
	default:
		return exitStatus(1)
	}
	return nil
}

// settle is retry and recover on a stopped runner. The single d-1 of the
// in-flight and uncertain flags is a delivery too.
func (g *fakeGH) settle(s *ghInst, cmd string, args []string) error {
	arg := func(name string) string {
		for i, a := range args {
			if a == name && i+1 < len(args) {
				return args[i+1]
			}
		}
		return ""
	}
	id, decision := arg("--delivery"), arg("--decision")
	if id == "d-1" && cmd == "recover" && s.uncertain {
		s.uncertain = false
		return nil
	}
	for _, d := range s.dels {
		if d.id != id {
			continue
		}
		switch {
		case cmd == "retry" && d.state == "failed":
			d.state = "pending"
			return nil
		case cmd == "retry" && d.state == "pending":
			return nil
		case cmd == "recover" && d.state == "uncertain":
			d.decision = decision
			d.state = "pending"
			if decision == "accept" {
				d.state = "accepted"
			}
			return nil
		}
		return exitStatus(5)
	}
	return exitStatus(5)
}

// enroll commits an owner and writes the token file first, as the contract's
// ordering has it. A hook can call it and then return its own error, to model a
// lost result.
func (g *fakeGH) enroll(inst, tok, label string) error {
	g.mu.Lock()
	defer g.mu.Unlock()
	s := g.insts[inst]
	if s == nil {
		return exitStatus(1)
	}
	if s.owner != nil {
		return exitStatus(3)
	}
	if _, err := os.Stat(tok); err == nil {
		return exitStatus(2)
	}
	g.nextID++
	s.lastGen++
	own := &ghOwner{id: fmt.Sprintf("o-%d", g.nextID), gen: s.lastGen, label: label}
	b, _ := json.Marshal(fakeToken{1, fakeSecret, inst, own.id, own.gen})
	if err := os.WriteFile(tok, b, 0o600); err != nil {
		return exitStatus(6)
	}
	s.owner = own
	s.journalOdd = s.journalSticky // taking the mutation lock settles a journal write
	return nil
}

func (g *fakeGH) statusDoc(inst string, s *ghInst, active bool) string {
	var owner any
	if s.owner != nil {
		owner = map[string]any{"id": s.owner.id, "generation": s.owner.gen, "label": s.owner.label}
	}
	dels := []any{}
	if s.inflight || s.uncertain {
		state, outcome := "in_flight", "in_flight"
		if s.uncertain {
			state, outcome = "uncertain", "unknown"
		}
		dels = append(dels, map[string]any{
			"id": "d-1", "path": "out/job-2026-11-15", "origin": "runtime", "schedule_path": nil, "occurrence": nil,
			"state": state, "reason": nil, "execution": "unknown",
			"attempts": []any{map[string]any{
				"id": "a-1", "started_at": "2026-11-15T09:00:00Z", "finished_at": nil,
				"outcome": outcome, "exit_code": nil, "signal": nil, "error": nil,
			}},
		})
	}
	for _, d := range s.dels {
		var item any
		if d.item != "" {
			item = d.item
		}
		att := map[string]any{"id": "a-" + d.id, "started_at": "2026-11-15T09:00:00Z", "finished_at": nil,
			"outcome": "failed", "exit_code": 1, "signal": nil, "error": nil, "owner_label": "test-session"}
		if d.state == "uncertain" {
			att["outcome"] = "unknown"
		}
		dels = append(dels, map[string]any{
			"id": d.id, "path": "out/" + d.id, "origin": "runtime", "schedule_path": item, "occurrence": nil,
			"state": d.state, "reason": nil, "execution": "unknown", "attempts": []any{att},
		})
	}
	diags := []any{}
	if s.inconsistent > 0 { // the script always says why, whatever the reason
		diags = append(diags, map[string]any{"code": "delivery_record_malformed", "message": "a delivery record is not valid", "path": "runtime/deliveries/x.json"})
	}
	if s.journalOdd {
		diags = append(diags, map[string]any{"code": "journal_write_in_progress", "message": "a delivery journal write is in progress, or one did not finish", "path": "runtime/journal.gen"})
	}
	doc := map[string]any{
		"protocol": "groundhog-runtime", "protocol_version": 1, "instance": inst,
		"observed_at": "2026-11-15T09:00:00Z", "revision": 1, "journal_generation": 0,
		"consistent": s.inconsistent == 0 && !s.journalOdd, "paused": s.paused, "owner": owner,
		"runner":     map[string]any{"state": s.runner, "last_error": nil},
		"consumer":   map[string]any{"configured": true, "valid": true},
		"schedule":   []any{},
		"deliveries": dels, "diagnostics": diags,
	}
	if active {
		doc["settled"] = map[string]any{"accepted": s.settled, "missing": 0}
	}
	b, _ := json.Marshal(doc)
	return string(b)
}

// fakeLauncher starts fake runners. A started runner marks the instance running
// when its token matches the owner, as the real one would, and otherwise ends
// at once with exit 3.
type fakeLauncher struct {
	mu          sync.Mutex
	gh          *fakeGH
	launches    []runner.Launch
	children    map[string]*fakeChild
	all         []*fakeChild
	startErr    error
	exitAtStart *runner.Exit // every child ends at once with this
	ignoreTerm  bool
	pid         int
}

func newFakeLauncher(g *fakeGH) *fakeLauncher {
	l := &fakeLauncher{gh: g, children: map[string]*fakeChild{}, pid: 40000}
	g.launcher = l
	return l
}

type fakeChild struct {
	l     *fakeLauncher
	inst  string
	pid   int
	done  chan struct{}
	once  sync.Once
	exit  runner.Exit
	terms int
}

func (l *fakeLauncher) Start(ln runner.Launch) (runner.Child, error) {
	l.mu.Lock()
	if l.startErr != nil {
		l.mu.Unlock()
		return nil, l.startErr
	}
	l.launches = append(l.launches, ln)
	l.pid++
	c := &fakeChild{l: l, inst: ln.Dir, pid: l.pid, done: make(chan struct{})}
	l.children[ln.Dir] = c
	l.all = append(l.all, c)
	early := l.exitAtStart
	l.mu.Unlock()
	if early != nil {
		c.end(*early)
		return c, nil
	}
	l.gh.update(ln.Dir, func(s *ghInst) { s.runner = "running" }) // never under l.mu: stop takes gh.mu then l.mu
	return c, nil
}

func (l *fakeLauncher) launchCount() int {
	l.mu.Lock()
	defer l.mu.Unlock()
	return len(l.launches)
}

func (l *fakeLauncher) child(inst string) *fakeChild {
	l.mu.Lock()
	defer l.mu.Unlock()
	return l.children[inst]
}

func (l *fakeLauncher) endChild(inst string, x runner.Exit) {
	l.mu.Lock()
	c := l.children[inst]
	l.mu.Unlock()
	if c != nil {
		c.end(x)
	}
}

func (c *fakeChild) end(x runner.Exit) {
	c.once.Do(func() { c.exit = x; close(c.done) })
}

func (c *fakeChild) PID() int              { return c.pid }
func (c *fakeChild) Done() <-chan struct{} { return c.done }
func (c *fakeChild) Exit() runner.Exit     { return c.exit }

// crash ends the child as a runner dying on its own would.
func (c *fakeChild) crash(x runner.Exit) {
	c.l.gh.update(c.inst, func(s *ghInst) { s.runner = "stopped" })
	c.end(x)
}

func (c *fakeChild) Terminate() error {
	select {
	case <-c.done:
		return os.ErrProcessDone
	default:
	}
	c.l.mu.Lock()
	c.terms++
	ignore := c.l.ignoreTerm
	c.l.mu.Unlock()
	if ignore {
		return nil
	}
	// SIGTERM: the runner closes admission and exits 0; a consumer call it was
	// waiting on is detached, never killed, and its delivery is left uncertain.
	c.l.gh.update(c.inst, func(s *ghInst) {
		s.runner = "stopped"
		if s.inflight {
			s.inflight, s.uncertain = false, true
		}
	})
	c.end(runner.Exit{})
	return nil
}

func (c *fakeChild) termCount() int {
	c.l.mu.Lock()
	defer c.l.mu.Unlock()
	return c.terms
}

// srig wires a supervisor to the fakes, with every wait made short.
type srig struct {
	t     *testing.T
	gh    *fakeGH
	l     *fakeLauncher
	sup   *Supervisor
	state string
	dir   string // parent of the instances
}

func newSRig(t *testing.T, tweak ...func(*SupervisorConfig)) *srig {
	t.Helper()
	g := newFakeGH()
	l := newFakeLauncher(g)
	state := t.TempDir()
	cfg := SupervisorConfig{
		Exec: g, Launcher: l, StateDir: state, Label: "test-session",
		StartWait: 300 * time.Millisecond, QuitWait: 400 * time.Millisecond, ReleaseWait: 2 * time.Second,
		ReapWait: 200 * time.Millisecond, PollEvery: 5 * time.Millisecond, BusyDelay: 2 * time.Millisecond,
	}
	for _, f := range tweak {
		f(&cfg)
	}
	sup, err := NewSupervisor(cfg)
	if err != nil {
		t.Fatal(err)
	}
	r := &srig{t: t, gh: g, l: l, sup: sup, state: state, dir: t.TempDir()}
	t.Cleanup(func() {
		ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
		defer cancel()
		sup.Quit(ctx)
	})
	return r
}

// inst registers a fake instance and returns its key.
func (r *srig) inst(name string) string {
	key := filepath.Join(r.dir, name, ".groundhog")
	r.gh.add(key)
	// The fake script runs nowhere, but Adopt looks for the instance's script
	// before it takes a token for it.
	if err := os.MkdirAll(key, 0o755); err != nil {
		r.t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(key, "groundhog.sh"), nil, 0o755); err != nil {
		r.t.Fatal(err)
	}
	return key
}

func (r *srig) run(key string) RunResult {
	r.t.Helper()
	res, err := r.sup.Run(context.Background(), key)
	if err != nil {
		r.t.Fatalf("Run: %v", err)
	}
	return res
}

// eventuallyTrue polls f for up to two seconds.
func eventuallyTrue(t *testing.T, what string, f func() bool) {
	t.Helper()
	for deadline := time.Now().Add(2 * time.Second); time.Now().Before(deadline); time.Sleep(2 * time.Millisecond) {
		if f() {
			return
		}
	}
	t.Fatalf("timed out waiting for %s", what)
}

// noSecret fails if the secret appears in any of the given strings.
func noSecret(t *testing.T, where string, texts ...string) {
	t.Helper()
	for _, s := range texts {
		if strings.Contains(s, fakeSecret) {
			t.Errorf("the token's secret reached %s: %q", where, s)
		}
	}
}

// everywhere is an Adopt scope that takes in every instance, for the tests that
// are not about the root.
func everywhere(string) bool { return true }
