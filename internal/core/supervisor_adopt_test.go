//go:build unix

package core

import (
	"context"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/zealtv/easyhog/internal/groundhog"
)

// The ownership rule (a token in this session's directory that matches
// status.owner), adoption of an ended session's tokens, and the runner-slot
// reservation, against the fake script. The pinned script has its own cases in
// supervisor_adopt_integration_test.go.

// killSession makes a session look like a process that died: the OS drops its
// lock, and nothing else changes. The session must not be used afterwards.
func killSession(t *testing.T, s *Supervisor) {
	t.Helper()
	s.mu.Lock()
	l := s.lock
	s.lock = nil
	s.mu.Unlock()
	if l == nil {
		t.Fatal("the session holds no lock: it never made its directory")
	}
	if err := l.Close(); err != nil {
		t.Fatal(err)
	}
}

// another makes a second session over the same fakes and state directory.
func (r *srig) another() *Supervisor {
	r.t.Helper()
	sup, err := NewSupervisor(SupervisorConfig{
		Exec: r.gh, Launcher: r.l, StateDir: r.state, Label: "second-session",
		StartWait: 300 * time.Millisecond, QuitWait: 400 * time.Millisecond, ReleaseWait: 2 * time.Second,
		ReapWait: 200 * time.Millisecond, PollEvery: 5 * time.Millisecond, BusyDelay: 2 * time.Millisecond,
	})
	if err != nil {
		r.t.Fatal(err)
	}
	r.t.Cleanup(func() {
		ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
		defer cancel()
		sup.Quit(ctx)
	})
	return sup
}

func ownerOf(s ghInst) *groundhog.Owner {
	if s.owner == nil {
		return nil
	}
	return &groundhog.Owner{ID: s.owner.id, Generation: s.owner.gen, Label: s.owner.label}
}

func tokenExists(t *testing.T, r *srig, key string) bool {
	t.Helper()
	info, ok := r.sup.Runner(key)
	return ok && info.TokenFile != "" && fileExists(info.TokenFile)
}

// ---- The rule, as a table ----

// Each case changes what the script says about the instance whose token this
// session holds, then applies the rule twice: as the refresher's coherent status
// reaches Observe, and as a Run reads for itself. Whether the token stays, and
// whether the instance still reads as ours, must agree with the rule.
func TestOwnershipRule(t *testing.T) {
	type outcome struct {
		keeps   bool // the token stays, and the instance is still ours
		refusal Refusal
		run     RunOutcome // for a kept token, what Run does
	}
	cases := []struct {
		name   string
		change func(*ghInst)
		want   outcome
	}{
		{"matching token", func(*ghInst) {}, outcome{keeps: true, run: RunAlreadyRunning}},
		{"mismatched generation", func(s *ghInst) { s.owner.gen++ }, outcome{refusal: RefusedExternal}},
		{"no owner", func(s *ghInst) { s.owner = nil }, outcome{refusal: RefusedNotOurs}},
		{"another owner", func(s *ghInst) { s.owner = &ghOwner{id: "o-other", gen: 1, label: "other"} }, outcome{refusal: RefusedExternal}},
		{"incoherent read, no owner", func(s *ghInst) { s.owner = nil; s.inconsistent = 100 }, outcome{keeps: true, refusal: RefusedInconsistent}},
		{"incoherent read, another owner", func(s *ghInst) { s.owner = &ghOwner{id: "o-other", gen: 1}; s.inconsistent = 100 }, outcome{keeps: true, refusal: RefusedInconsistent}},
	}
	for _, via := range []string{"observe", "run"} {
		for _, c := range cases {
			t.Run(via+"/"+c.name, func(t *testing.T) {
				r := newSRig(t)
				key := r.inst("proj")
				r.run(key)
				was := mustRunner(t, r, key).TokenFile
				r.gh.update(key, c.change)
				now := r.gh.state(key)

				switch via {
				case "observe":
					// The status the refresher just read: coherent unless the script
					// says otherwise.
					r.sup.Observe(context.Background(), key, groundhog.Status{Consistent: now.inconsistent == 0, Owner: ownerOf(now)})
				case "run":
					res, err := r.sup.Run(context.Background(), key)
					switch {
					case c.want.keeps && c.want.refusal == 0:
						if err != nil || res.Outcome != c.want.run {
							t.Fatalf("Run %+v %v, want %v", res, err, c.want.run)
						}
					default:
						wantRefused(t, err, c.want.refusal)
					}
				}
				kept := fileExists(was)
				_, held := r.sup.Runner(key)
				if kept != c.want.keeps || held != c.want.keeps {
					t.Errorf("token kept %v, supervisor holds the instance %v; want both %v", kept, held, c.want.keeps)
				}
				if o := ownerOf(now); o != nil && now.inconsistent == 0 && r.sup.IsOurs(*o) != c.want.keeps {
					t.Errorf("instance ours: %v, want %v", !c.want.keeps, c.want.keeps)
				}
			})
		}
	}
}

// A read that began before an enroll finished can show no owner after the enroll
// has committed. Observe confirms with a read of its own and leaves a current
// token alone.
func TestObserveConfirmsBeforeDeletingAToken(t *testing.T) {
	r := newSRig(t)
	key := r.inst("proj")
	r.run(key)
	was := mustRunner(t, r, key).TokenFile
	r.sup.Observe(context.Background(), key, groundhog.Status{Consistent: true}) // a stale snapshot: no owner
	if !fileExists(was) {
		t.Fatal("a stale snapshot deleted a token that is current")
	}
	if got := r.gh.count(key, "status"); got < 3 {
		t.Errorf("%d status reads: Observe did not confirm with one of its own", got)
	}
}

// The rule deletes only on a coherent status: a coherent snapshot is confirmed by
// a read of Observe's own, and when that one is incoherent the token stays.
func TestObserveDoesNotDeleteWhenItsOwnConfirmingReadIsIncoherent(t *testing.T) {
	r := newSRig(t)
	key := r.inst("proj")
	r.run(key)
	was := mustRunner(t, r, key).TokenFile
	r.gh.update(key, func(s *ghInst) { s.owner = nil; s.inconsistent = 100 })
	r.sup.Observe(context.Background(), key, groundhog.Status{Consistent: true})
	if !fileExists(was) {
		t.Fatal("deleted on a confirming read that was not coherent")
	}
	r.gh.update(key, func(s *ghInst) { s.inconsistent = 0 })
	r.sup.Observe(context.Background(), key, groundhog.Status{Consistent: true})
	if fileExists(was) {
		t.Error("kept although a coherent read shows no owner")
	}
}

func TestObserveLeavesAnInstanceWithAnOperationInFlightToThatOperation(t *testing.T) {
	r := newSRig(t)
	key := r.inst("proj")
	r.run(key)
	sess := r.sup.sessions[key]
	sess.op <- struct{}{} // an operation holds the instance
	defer sess.release()
	before := r.gh.count(key, "status")
	r.gh.update(key, func(s *ghInst) { s.owner = nil })
	r.sup.Observe(context.Background(), key, groundhog.Status{Consistent: true})
	if r.gh.count(key, "status") != before || !tokenExists(t, r, key) {
		t.Error("Observe acted on an instance whose operation slot was taken")
	}
}

func TestObserveIgnoresInstancesItHoldsNothingFor(t *testing.T) {
	r := newSRig(t)
	key := r.inst("proj")
	r.sup.Observe(context.Background(), key, groundhog.Status{Consistent: true, Owner: &groundhog.Owner{ID: "o-x", Generation: 1}})
	if r.gh.count(key, "status") != 0 {
		t.Error("Observe read an instance this session holds no token for")
	}
}

// ---- Adoption ----

func TestAdoptTakesATokenFromAnEndedSessionAndLeavesItsRunnerAlone(t *testing.T) {
	r := newSRig(t)
	key := r.inst("proj")
	r.run(key)
	st := r.gh.state(key)
	old := mustRunner(t, r, key)
	killSession(t, r.sup)

	b := r.another()
	callsBefore := len(r.gh.allCalls())
	if got := b.Adopt(everywhere); len(got) != 1 || got[0] != key {
		t.Fatalf("adopted %v", got)
	}
	info, ok := b.Runner(key)
	if !ok || !info.Adopted || info.TokenFile == "" || info.TokenFile == old.TokenFile || info.Phase != PhaseEnrolled || info.PID != 0 {
		t.Fatalf("adopted info %+v", info)
	}
	if fileExists(old.TokenFile) || !fileExists(info.TokenFile) || filepath.Dir(info.TokenFile) == filepath.Dir(old.TokenFile) {
		t.Errorf("the token did not move: old %v new %v", fileExists(old.TokenFile), fileExists(info.TokenFile))
	}
	if !b.IsOurs(groundhog.Owner{ID: st.owner.id, Generation: st.owner.gen}) {
		t.Error("an adopted token does not make its instance ours")
	}
	if info.AdoptedFrom.IsZero() || time.Since(info.AdoptedFrom) > time.Minute {
		t.Errorf("adopted from %v", info.AdoptedFrom)
	}
	// Nothing ran: adoption is not a restart, and it calls no script.
	if r.l.launchCount() != 1 || len(r.gh.allCalls()) != callsBefore {
		t.Errorf("calls %v, launches %d", r.gh.commands(key), r.l.launchCount())
	}
	// The ended session's directory, left with its lock file alone, went.
	if _, err := os.Stat(filepath.Dir(old.TokenFile)); !os.IsNotExist(err) {
		t.Errorf("the ended session's directory remains: %v", err)
	}
	// A second Adopt finds nothing more.
	if got := b.Adopt(everywhere); len(got) != 0 {
		t.Errorf("adopted %v twice", got)
	}
}

// "Ended at" is the dead session's last sign of life: its lock file's modification
// time, which a live session refreshes at each Adopt.
func TestAnAdoptedInstanceReportsWhenItsSessionWasLastSeenAlive(t *testing.T) {
	r := newSRig(t)
	key := r.inst("proj")
	r.run(key)
	lockPath := filepath.Join(filepath.Dir(mustRunner(t, r, key).TokenFile), lockName)
	before := time.Now().Add(-time.Hour).Truncate(time.Second)
	if err := os.Chtimes(lockPath, before, before); err != nil {
		t.Fatal(err)
	}
	r.sup.Adopt(everywhere) // a live session's heartbeat
	fi, err := os.Stat(lockPath)
	if err != nil || time.Since(fi.ModTime()) > time.Minute {
		t.Fatalf("the heartbeat did not refresh the lock file's time: %v %v", fi, err)
	}
	killed := time.Now().Add(-90 * time.Minute).Truncate(time.Second)
	if err := os.Chtimes(lockPath, killed, killed); err != nil { // as if it died then
		t.Fatal(err)
	}
	killSession(t, r.sup)
	b := r.another()
	b.Adopt(everywhere)
	if got := mustRunnerOf(t, b, key).AdoptedFrom; !got.Equal(killed) {
		t.Errorf("adopted from %v, want the lock file's time %v", got, killed)
	}
}

func TestAnAdoptedInstanceTakesRunAndClearsAPauseButStartsNoSecondRunner(t *testing.T) {
	r := newSRig(t)
	key := r.inst("proj")
	r.run(key)
	killSession(t, r.sup)
	b := r.another()
	b.Adopt(everywhere)

	r.gh.update(key, func(s *ghInst) { s.paused = true }) // set by something else
	res, err := b.Run(context.Background(), key)
	if err != nil || res.Outcome != RunResumed || !res.ClearedPause || res.Enrolled {
		t.Fatalf("Run %+v %v", res, err)
	}
	if r.l.launchCount() != 1 {
		t.Fatalf("%d runners: Run started a second one beside the adopted runner", r.l.launchCount())
	}
	if info, _ := b.Runner(key); !info.Adopted {
		t.Error("the instance stopped reading as adopted though no runner of this session's own exists")
	}
	// The runner ends; Run now starts one of this session's own.
	r.gh.update(key, func(s *ghInst) { s.runner = "stopped" })
	res, err = b.Run(context.Background(), key)
	if err != nil || res.Outcome != RunStarted || r.l.launchCount() != 2 {
		t.Fatalf("Run %+v %v, %d launches", res, err, r.l.launchCount())
	}
	if info, _ := b.Runner(key); info.Adopted || info.Phase != PhaseRunning {
		t.Errorf("info %+v", info)
	}
}

func TestQuitStopsAndReleasesAnAdoptedInstanceWithoutSignallingItsRunner(t *testing.T) {
	r := newSRig(t)
	key := r.inst("proj")
	r.run(key)
	child := r.l.child(key)
	killSession(t, r.sup)
	b := r.another()
	b.Adopt(everywhere)

	rep := b.Quit(context.Background())
	if len(rep.Instances) != 1 || rep.Instances[0].Outcome != QuitReleased || !rep.Clean() || len(rep.Notices) != 0 || rep.Instances[0].Terminated {
		t.Fatalf("report %+v", rep)
	}
	if cmds := r.gh.commands(key); strings.Join(cmds[len(cmds)-2:], ",") != "stop,release" || r.gh.count(key, "pause") != 0 {
		t.Errorf("calls %v, want Quit's stop then release and nothing else of its own", cmds)
	}
	if st := r.gh.state(key); st.owner != nil || st.runner != "stopped" {
		t.Errorf("state %+v", st)
	}
	if child.termCount() != 0 {
		t.Error("the adopted runner was sent SIGTERM")
	}
	if files := filesUnder(t, r.state); len(files) != 0 {
		t.Errorf("files left: %v", files)
	}
}

func TestQuitLeavesAnAdoptedInstanceOwnedWhenItsStopTimesOut(t *testing.T) {
	r := newSRig(t)
	key := r.inst("proj")
	r.run(key)
	child := r.l.child(key)
	r.gh.update(key, func(s *ghInst) { s.inflight = true }) // a consumer call that outlasts QuitWait
	killSession(t, r.sup)
	b := r.another()
	b.Adopt(everywhere)

	rep := b.Quit(context.Background())
	in := rep.Instances[0]
	if in.Outcome != QuitLeftOwned || in.Terminated || rep.Clean() || in.TokenFile == "" {
		t.Fatalf("%+v", in)
	}
	for _, want := range []string{"stop had not finished", "no process to SIGTERM", "left owned"} {
		if !strings.Contains(in.Notice, want) {
			t.Errorf("notice %q lacks %q", in.Notice, want)
		}
	}
	if child.termCount() != 0 {
		t.Error("an adopted runner was sent SIGTERM")
	}
	if r.gh.count(key, "release") != 0 || r.gh.state(key).owner == nil {
		t.Error("released an instance whose runner may still run")
	}
	if !fileExists(in.TokenFile) {
		t.Error("the token was removed with the instance still owned")
	}
	noSecret(t, "a notice", rep.Notices...)
	wantRepairLeadsWithReopening(t, in.Notice)
}

// ---- Stale tokens ----

func TestAnAdoptedTokenThatControlsNothingGoesOnTheFirstCoherentStatus(t *testing.T) {
	r := newSRig(t)
	key := r.inst("proj")
	r.run(key)
	killSession(t, r.sup)
	// Someone released the instance by hand while no session lived.
	r.gh.update(key, func(s *ghInst) { s.owner, s.runner = nil, "stopped" })
	b := r.another()
	b.Adopt(everywhere)
	token := mustRunnerOf(t, b, key).TokenFile
	b.Observe(context.Background(), key, groundhog.Status{Consistent: false}) // an incoherent read changes nothing
	if !fileExists(token) {
		t.Fatal("deleted on an incoherent status")
	}
	b.Observe(context.Background(), key, groundhog.Status{Consistent: true})
	if fileExists(token) {
		t.Error("the token that controls nothing was kept")
	}
	if _, ok := b.Runner(key); ok {
		t.Error("the supervisor still holds the instance")
	}
}

func TestQuitSettlesAnAdoptedTokenThatControlsNothing(t *testing.T) {
	r := newSRig(t)
	key := r.inst("proj")
	r.run(key)
	killSession(t, r.sup)
	r.gh.update(key, func(s *ghInst) { s.owner, s.runner = &ghOwner{id: "o-new", gen: 5, label: "someone"}, "stopped" })
	b := r.another()
	b.Adopt(everywhere)
	token := mustRunnerOf(t, b, key).TokenFile

	rep := b.Quit(context.Background())
	in := rep.Instances[0]
	if in.Outcome != QuitNotOwned || !rep.Clean() || !strings.Contains(in.Notice, "controlled nothing") || !strings.Contains(in.Notice, "someone") {
		t.Fatalf("%+v", in)
	}
	if fileExists(token) {
		t.Error("the stale token was kept")
	}
	if r.gh.state(key).owner == nil || r.gh.count(key, "release") != 0 {
		t.Error("Quit released an owner that is not this session's")
	}
}

func mustRunnerOf(t *testing.T, s *Supervisor, key string) RunnerInfo {
	t.Helper()
	info, ok := s.Runner(key)
	if !ok {
		t.Fatalf("%s is not supervised", key)
	}
	return info
}

// ---- Several windows ----

// Two sessions (or twenty) that find the same ended session's token at once:
// the rename lets exactly one win.
func TestRacingSupervisorsAdoptOneTokenExactlyOnce(t *testing.T) {
	for round := 0; round < 20; round++ {
		r := newSRig(t)
		key := r.inst("proj")
		r.run(key)
		killSession(t, r.sup)

		const n = 6
		sups := make([]*Supervisor, n)
		for i := range sups {
			sups[i] = r.another()
		}
		var wins atomic.Int32
		start := make(chan struct{})
		var wg sync.WaitGroup
		for _, s := range sups {
			wg.Add(1)
			go func() {
				defer wg.Done()
				<-start
				wins.Add(int32(len(s.Adopt(everywhere))))
			}()
		}
		close(start)
		wg.Wait()

		holders := 0
		for _, s := range sups {
			if info, ok := s.Runner(key); ok && info.Adopted && fileExists(info.TokenFile) {
				holders++
			}
		}
		if wins.Load() != 1 || holders != 1 {
			t.Fatalf("round %d: %d adoptions reported, %d sessions hold the token; want exactly one of each", round, wins.Load(), holders)
		}
		tokens := 0
		for _, f := range filesUnder(t, r.state) {
			if strings.HasSuffix(f, ".token") {
				tokens++
			}
		}
		if tokens != 1 {
			t.Fatalf("round %d: %d token files, want the one the winner holds", round, tokens)
		}
	}
}

func TestAdoptSkipsWhatIsNotItsToTake(t *testing.T) {
	r := newSRig(t)
	key := r.inst("proj")
	r.run(key)
	live := mustRunner(t, r, key).TokenFile

	root := filepath.Join(r.state, sessionsDirName)
	// A directory with no lock file: made by something else, or an older Easyhog.
	// Whether its session lives is unknown, so it is left alone.
	legacy := filepath.Join(root, "123-legacy")
	if err := os.Mkdir(legacy, 0o700); err != nil {
		t.Fatal(err)
	}
	legacyToken := filepath.Join(legacy, "i1.token")
	if err := os.WriteFile(legacyToken, []byte(`{"schema_version":1,"token":"`+fakeSecret+`","instance":"`+key+`","owner_id":"o-1","generation":1}`), 0o600); err != nil {
		t.Fatal(err)
	}
	// A directory still being made.
	staging := filepath.Join(root, stagePrefix+"x")
	if err := os.Mkdir(staging, 0o700); err != nil {
		t.Fatal(err)
	}

	b := r.another()
	if got := b.Adopt(everywhere); len(got) != 0 {
		t.Fatalf("adopted %v", got)
	}
	for _, p := range []string{live, legacyToken, staging} {
		if _, err := os.Stat(p); err != nil {
			t.Errorf("%s was touched: %v", p, err)
		}
	}
	if len(b.Leftovers()) != 0 {
		t.Errorf("leftovers %v", b.Leftovers())
	}
}

// A token whose instance cannot be found (a moved repository) may be the only
// credential for an instance still owned: it stays, and it is listed.
func TestATokenWhoseInstanceIsGoneStaysAndIsListed(t *testing.T) {
	r := newSRig(t)
	key := r.inst("proj")
	r.run(key)
	old := mustRunner(t, r, key)
	killSession(t, r.sup)
	if err := os.WriteFile(filepath.Join(filepath.Dir(old.TokenFile), "i9.token"), []byte("not json"), 0o600); err != nil {
		t.Fatal(err)
	}
	// The repository moves: nothing is at the path the token names now.
	moved := filepath.Join(r.dir, "proj-moved")
	if err := os.Rename(filepath.Join(r.dir, "proj"), moved); err != nil {
		t.Fatal(err)
	}
	b := r.another()
	if got := b.Adopt(everywhere); len(got) != 0 {
		t.Fatalf("adopted %v although no instance is at the token's path", got)
	}
	left := b.Leftovers()
	if len(left) != 2 {
		t.Fatalf("leftovers %+v, want the moved repository's token and the unreadable one", left)
	}
	for _, l := range left {
		if !fileExists(l.TokenFile) || l.Why == "" {
			t.Errorf("%+v", l)
		}
	}
	if !fileExists(old.TokenFile) || !fileExists(filepath.Join(filepath.Dir(old.TokenFile), lockName)) {
		t.Error("the ended session's directory lost what it still needs")
	}
	rep := b.Quit(context.Background())
	var listed int
	for _, n := range rep.Notices {
		if strings.Contains(n, "left where it is") {
			listed++
		}
		noSecret(t, "a notice", n)
	}
	if listed != 2 {
		t.Errorf("notices %q: want both tokens listed", rep.Notices)
	}
}

func TestAdoptRemovesEmptyEndedDirectoriesAndTheirOrphanLogs(t *testing.T) {
	r := newSRig(t)
	key := r.inst("proj")
	r.run(key)
	dir := filepath.Dir(mustRunner(t, r, key).TokenFile)
	r.sup.Quit(context.Background())
	// A crash after a release leaves a log and a lock file and nothing else.
	if err := os.MkdirAll(dir, 0o700); err != nil {
		t.Fatal(err)
	}
	for _, n := range []string{lockName, "i1.log"} {
		if err := os.WriteFile(filepath.Join(dir, n), nil, 0o600); err != nil {
			t.Fatal(err)
		}
	}
	b := r.another()
	b.Adopt(everywhere)
	if _, err := os.Stat(dir); !os.IsNotExist(err) {
		t.Errorf("the empty ended directory remains: %v", err)
	}
}

func TestAnAdoptedInstanceAlreadyHeldUnderAnotherTokenIsLeftForLater(t *testing.T) {
	r := newSRig(t)
	key := r.inst("proj")
	r.run(key)
	first := mustRunner(t, r, key)
	// Two ended sessions each left a token for the one instance.
	second := filepath.Join(filepath.Dir(first.TokenFile), "i2.token")
	data, err := os.ReadFile(first.TokenFile)
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(second, data, 0o600); err != nil {
		t.Fatal(err)
	}
	killSession(t, r.sup)
	b := r.another()
	if got := b.Adopt(everywhere); len(got) != 1 {
		t.Fatalf("adopted %v", got)
	}
	if left := b.Leftovers(); len(left) != 1 || !strings.Contains(left[0].Why, "already holds another token") {
		t.Fatalf("leftovers %+v", left)
	}
}

// ---- the runner limit ----

func TestTwoFirstRunsAtTheRunnerLimitCannotBothEnroll(t *testing.T) {
	r := newSRig(t, func(c *SupervisorConfig) { c.MaxRunners = 1 })
	a, b := r.inst("a"), r.inst("b")
	entered, release := make(chan struct{}), make(chan struct{})
	r.gh.hook = func(_ context.Context, inst, cmd string, _ int) (bool, string, error) {
		if cmd == "enroll" {
			close(entered) // a second enroll would close it twice and panic
			<-release
		}
		return false, "", nil
	}
	first := make(chan error, 1)
	go func() { _, err := r.sup.Run(context.Background(), a); first <- err }()
	<-entered // the first Run is inside its enroll: its slot is reserved, not yet held
	_, err := r.sup.Run(context.Background(), b)
	wantRefused(t, err, RefusedLimit)
	close(release)
	if err := <-first; err != nil {
		t.Fatal(err)
	}
	if r.gh.count(b, "enroll") != 0 || r.gh.count(a, "enroll") != 1 {
		t.Errorf("enrolls: a %d, b %d", r.gh.count(a, "enroll"), r.gh.count(b, "enroll"))
	}
}

func TestManyFirstRunsAtOnceStayWithinTheRunnerLimit(t *testing.T) {
	const limit, tries = 3, 12
	r := newSRig(t, func(c *SupervisorConfig) { c.MaxRunners = limit })
	keys := make([]string, tries)
	for i := range keys {
		keys[i] = r.inst(string(rune('a' + i)))
	}
	var ok, refused atomic.Int32
	var wg sync.WaitGroup
	start := make(chan struct{})
	for _, k := range keys {
		wg.Add(1)
		go func() {
			defer wg.Done()
			<-start
			if _, err := r.sup.Run(context.Background(), k); err == nil {
				ok.Add(1)
			} else if re, isRef := err.(*RefusedError); isRef && re.Reason == RefusedLimit {
				refused.Add(1)
			} else {
				t.Errorf("Run %s: %v", k, err)
			}
		}()
	}
	close(start)
	wg.Wait()
	if ok.Load() != limit || refused.Load() != tries-limit {
		t.Fatalf("%d Runs went through and %d were refused, want %d and %d", ok.Load(), refused.Load(), limit, tries-limit)
	}
}

// A refused or failed first Run gives its reserved slot back.
func TestAFailedEnrollReleasesItsReservedSlot(t *testing.T) {
	r := newSRig(t, func(c *SupervisorConfig) { c.MaxRunners = 1 })
	a, b := r.inst("a"), r.inst("b")
	r.gh.hook = func(_ context.Context, inst, cmd string, n int) (bool, string, error) {
		if cmd == "enroll" && inst == a {
			return true, "", exitStatus(3)
		}
		return false, "", nil
	}
	if _, err := r.sup.Run(context.Background(), a); err == nil {
		t.Fatal("the enroll was meant to fail")
	}
	if res := r.run(b); res.Outcome != RunStarted {
		t.Fatalf("Run %+v: a failed enroll kept its slot", res)
	}
}

// An adopted instance whose runner died with its session
// (no exit trap, so status reads "unknown") offered nothing. This session holds
// the token and has no child, so Run starts a runner, and Offers and the fresh
// read agree.
func TestRunStartsARunnerForAnAdoptedInstanceWhoseRunnerDiedWithItsSession(t *testing.T) {
	r := newSRig(t)
	key := r.inst("proj")
	r.run(key)
	killSession(t, r.sup)
	r.gh.update(key, func(s *ghInst) { s.runner = "unknown" }) // the runner died without its exit trap
	b := r.another()
	if got := b.Adopt(everywhere); len(got) != 1 {
		t.Fatalf("adopted %v", got)
	}
	launches := r.l.launchCount()

	res, err := b.Run(context.Background(), key)
	if err != nil {
		t.Fatalf("Run: %v", err)
	}
	if res.Outcome != RunStarted || r.l.launchCount() != launches+1 {
		t.Errorf("Run %+v, launches %d -> %d; want a runner started", res, launches, r.l.launchCount())
	}
	if info, _ := b.Runner(key); info.Phase != PhaseRunning {
		t.Errorf("info %+v", info)
	}
}

// Adopt used to take every ended session's tokens, so a
// window on one root swallowed the runners of another root's crashed window,
// counted them toward the runner limit and stopped them at Quit. Now a token for
// an instance outside this session's root stays exactly where it is, and a later
// session on that instance's own root adopts it.
func TestAdoptLeavesATokenForAnInstanceOutsideTheRootWhereItIs(t *testing.T) {
	r := newSRig(t)
	mine := r.inst("rootA/proj")
	theirs := r.inst("rootB/proj")
	r.run(mine)
	r.run(theirs)
	theirToken := mustRunner(t, r, theirs).TokenFile
	theirChild := r.l.child(theirs)
	killSession(t, r.sup)

	rootA, rootB := filepath.Join(r.dir, "rootA"), filepath.Join(r.dir, "rootB")
	b := r.another()
	callsBefore := r.gh.commands(theirs)
	if got := b.Adopt(func(inst string) bool { return Within(rootA, inst) }); len(got) != 1 || got[0] != mine {
		t.Fatalf("adopted %v, want only %s", got, mine)
	}
	if _, held := b.Runner(theirs); held {
		t.Error("the session holds a token for an instance outside its root")
	}
	if !fileExists(theirToken) {
		t.Fatal("the token for the other root's instance was moved or deleted")
	}
	if left := b.Leftovers(); len(left) != 0 {
		t.Errorf("an out-of-root token is listed as a leftover: %+v", left)
	}

	// It is not counted and not stopped at Quit: one instance, one stop, one release.
	rep := b.Quit(context.Background())
	if len(rep.Instances) != 1 || rep.Instances[0].Instance != mine || len(rep.Notices) != 0 {
		t.Errorf("quit report %+v", rep)
	}
	if got := r.gh.commands(theirs); len(got) != len(callsBefore) {
		t.Errorf("the other root's instance saw %v after adoption; no script may run for it", got[len(callsBefore):])
	}
	if st := r.gh.state(theirs); st.owner == nil || st.runner != "running" || theirChild.termCount() != 0 || isDone(theirChild) {
		t.Errorf("its runner or ownership was touched: %+v", st)
	}
	if !fileExists(theirToken) {
		t.Fatal("Quit removed the other root's token")
	}

	// The token stays valid: a session on its own root adopts it, and controls it.
	c := r.another()
	if got := c.Adopt(func(inst string) bool { return Within(rootB, inst) }); len(got) != 1 || got[0] != theirs {
		t.Fatalf("a session on the other root adopted %v", got)
	}
	if _, err := c.Stop(context.Background(), theirs); err != nil {
		t.Fatalf("the left-in-place token no longer controls the instance: %v", err)
	}
	if st := r.gh.state(theirs); st.owner != nil || st.runner != "stopped" {
		t.Errorf("Stop did not reach the instance: %+v", st)
	}
}
