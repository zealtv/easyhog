package core

import (
	"context"
	"errors"
	"testing"
	"time"

	"github.com/zealtv/easyhog/internal/discovery"
	"github.com/zealtv/easyhog/internal/groundhog"
)

const testInst = "/synthetic/proj/.groundhog"

func noWait(context.Context, time.Duration) error { return nil }

// readCoherent is the one coherent-read loop: these rows pin what the refresher's
// and the supervisor's hand-written loops did before they shared it.
func TestReadCoherent(t *testing.T) {
	busy := exitStatus(4)
	bad := statusOpts{rev: 3, inconsist: true}
	odd := statusOpts{rev: 3, gen: 5}
	good := statusOpts{rev: 3, gen: 4}
	cases := []struct {
		name      string
		answers   []any // statusOpts, or an error for a failed call
		tries     int
		retryBusy bool
		wantOK    bool
		wantReads int
		wantErr   func(error) bool // nil: no error
		wantCalls int
	}{
		{name: "coherent at once", answers: []any{good}, tries: 3, wantOK: true, wantReads: 1, wantCalls: 1},
		{name: "settles on the second read", answers: []any{bad, good}, tries: 3, wantOK: true, wantReads: 2, wantCalls: 2},
		{name: "an odd journal generation alone is trusted: the script says consistent", answers: []any{odd, good}, tries: 3, wantOK: true, wantReads: 1, wantCalls: 1},
		{name: "never settles: the last status comes back, marked", answers: []any{bad, bad, bad, good}, tries: 3, wantReads: 3, wantCalls: 3},
		{name: "one try reads once", answers: []any{bad, good}, tries: 1, wantReads: 1, wantCalls: 1},
		{name: "an error ends the read at once", answers: []any{bad, errors.New("boom"), good}, tries: 3, wantReads: 1, wantCalls: 2,
			wantErr: func(err error) bool { return err != nil && err.Error() != "" }},
		{name: "busy ends the read at once without RetryBusy", answers: []any{busy, good}, tries: 3, wantCalls: 1,
			wantErr: func(err error) bool { return groundhog.IsRetryable(err) }},
		{name: "busy costs a try with RetryBusy", answers: []any{busy, good}, tries: 3, retryBusy: true, wantOK: true, wantReads: 1, wantCalls: 2},
		{name: "busy to the end is the error", answers: []any{busy, busy, busy, good}, tries: 3, retryBusy: true, wantCalls: 3,
			wantErr: func(err error) bool { return groundhog.IsRetryable(err) }},
		{name: "inconsistent then busy to the end is the busy error", answers: []any{bad, bad, busy}, tries: 3, retryBusy: true, wantReads: 2, wantCalls: 3,
			wantErr: func(err error) bool { return groundhog.IsRetryable(err) }},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			ex := newExec(func(_ context.Context, in, cmd string, n int) (string, error) {
				if cmd != "status" {
					return "", exitStatus(1)
				}
				switch a := c.answers[min(n, len(c.answers))-1].(type) {
				case statusOpts:
					return statusJSON(in, a), nil
				case error:
					return "", a
				}
				return "", nil
			})
			cl, err := NewClients(ex).For(testInst)
			if err != nil {
				t.Fatal(err)
			}
			st, reads, ok, err := readCoherent(context.Background(), cl, readPolicy{Tries: c.tries, Delay: time.Millisecond, Wait: noWait, RetryBusy: c.retryBusy})
			if (c.wantErr == nil) != (err == nil) || (err != nil && !c.wantErr(err)) {
				t.Fatalf("error %v", err)
			}
			if ok != c.wantOK || reads != c.wantReads || ex.count(testInst, "status") != c.wantCalls {
				t.Errorf("ok %v reads %d calls %d; want %v/%d/%d", ok, reads, ex.count(testInst, "status"), c.wantOK, c.wantReads, c.wantCalls)
			}
			if err == nil && st.Revision != 3 {
				t.Errorf("status %+v is not the one read", st)
			}
		})
	}
}

// A context that ends while waiting to re-read stops the loop, keeping what was
// last read for a caller that shows it, and says so when nothing was read.
func TestReadCoherentStopsWhenTheContextEnds(t *testing.T) {
	ex := newExec(func(_ context.Context, in, _ string, _ int) (string, error) {
		return statusJSON(in, statusOpts{rev: 3, inconsist: true}), nil
	})
	cl, _ := NewClients(ex).For(testInst)
	ctx, cancel := context.WithCancel(context.Background())
	wait := func(c context.Context, _ time.Duration) error { cancel(); <-c.Done(); return c.Err() }
	_, reads, ok, err := readCoherent(ctx, cl, readPolicy{Tries: 3, Wait: wait})
	if err != nil || ok || reads != 1 || ex.count(testInst, "status") != 1 {
		t.Errorf("err %v ok %v reads %d calls %d", err, ok, reads, ex.count(testInst, "status"))
	}
	done, stop := context.WithCancel(context.Background())
	stop()
	if _, _, _, err := readCoherent(done, cl, readPolicy{Tries: 3, Wait: noWait}); err == nil {
		t.Error("a context that ended before any read: want an error")
	}
}

func TestOneClientPerInstance(t *testing.T) {
	cs := NewClients(newExec(func(context.Context, string, string, int) (string, error) { return "", nil }))
	a1, err := cs.For(testInst)
	if err != nil {
		t.Fatal(err)
	}
	a2, _ := cs.For(testInst)
	b, _ := cs.For("/synthetic/other/.groundhog")
	if a1 != a2 || a1 == b {
		t.Errorf("same instance: %v; different instance: %v", a1 == a2, a1 != b)
	}
	if _, err := cs.For("relative/path"); err == nil {
		t.Error("a relative path is no instance")
	}
}

// The refresher and the supervisor given one pool use one client per instance,
// and so one capability cache.
func TestRefresherAndSupervisorShareTheirClients(t *testing.T) {
	root := t.TempDir()
	mkInstance(t, root, "proj")
	ex := newExec(func(_ context.Context, in, cmd string, _ int) (string, error) { return "", exitStatus(1) })
	clients := NewClients(ex)
	r, err := NewRefresher(Config{Root: root, FS: discovery.OS{}, Exec: ex, Clients: clients})
	if err != nil {
		t.Fatal(err)
	}
	if err := r.Rescan(context.Background()); err != nil {
		t.Fatal(err)
	}
	s, err := NewSupervisor(SupervisorConfig{Exec: ex, Launcher: newFakeLauncher(newFakeGH()), Clients: clients, StateDir: t.TempDir()})
	if err != nil {
		t.Fatal(err)
	}
	key := r.View().Instances[0].Instance.Path
	sess, err := s.sessionFor("run", key)
	if err != nil {
		t.Fatal(err)
	}
	r.mu.Lock()
	rc := r.entries[key].client
	r.mu.Unlock()
	want, _ := clients.For(key)
	if rc != want || sess.client != want {
		t.Errorf("refresher's client %p, supervisor's %p, pool's %p", rc, sess.client, want)
	}
}
