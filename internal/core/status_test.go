package core

import (
	"context"
	"testing"

	"github.com/zealtv/easyhog/internal/discovery"
)

const testInst = "/synthetic/proj/.groundhog"

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

// The refresher and the ticker given one pool use one client per instance,
// and so one capability cache.
func TestRefresherAndTickerShareTheirClients(t *testing.T) {
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
	tk, err := NewTicker(TickerConfig{Clients: clients})
	if err != nil {
		t.Fatal(err)
	}
	key := r.View().Instances[0].Instance.Path
	r.mu.Lock()
	rc := r.entries[key].client
	r.mu.Unlock()
	want, _ := clients.For(key)
	tc, _ := tk.cfg.Clients.For(key)
	if rc != want || tc != want {
		t.Errorf("refresher's client %p, ticker's %p, pool's %p", rc, tc, want)
	}
}
