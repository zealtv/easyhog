//go:build unix

package acceptance

import (
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
)

// wakeFixture is the optional in-place wake: an ordinary executable (a
// wake adapter, named by EASYHOG_WAKE_CONSUMER) configured as
// one instance's consumer exactly as examples/consumers/fixtures.sh does, a fake
// agent that calls no model, and a pane on a private tmux server.
//
// Safety: every tmux call names the private server with -L and runs with a
// private TMUX_TMPDIR and with TMUX and TMUX_PANE removed, and the fixture
// aborts unless that server's socket lies under the test's temporary
// directory. A bare `tmux` (the default server) is never called.
type wakeFixture struct {
	consumer string
	name     string
	tmpdir   string
	socket   string
	pane     string
	marks    string
	state    string
}

// newWake returns nil, with the reason logged, when the wake part cannot run.
func (w *world) newWake(t *testing.T) *wakeFixture {
	t.Helper()
	consumer := os.Getenv("EASYHOG_WAKE_CONSUMER")
	switch {
	case consumer == "":
		t.Log("SKIP wake adapter: EASYHOG_WAKE_CONSUMER is not set (point it at a wake adapter); the rest runs")
		return nil
	case !filepath.IsAbs(consumer):
		t.Logf("SKIP wake adapter: EASYHOG_WAKE_CONSUMER must be an absolute path, got %q", consumer)
		return nil
	}
	if info, err := os.Stat(consumer); err != nil || info.IsDir() || info.Mode()&0o111 == 0 {
		t.Logf("SKIP wake adapter: EASYHOG_WAKE_CONSUMER %q is not an executable file", consumer)
		return nil
	}
	if _, err := exec.LookPath("tmux"); err != nil {
		t.Log("SKIP wake adapter: tmux is not installed")
		return nil
	}
	x := &wakeFixture{consumer: consumer, name: fmt.Sprintf("ehacc%d", os.Getpid()),
		tmpdir: filepath.Join(w.tmp, "tmux"), marks: filepath.Join(w.tmp, "marks"), state: filepath.Join(w.tmp, "wake-state")}
	for _, d := range []string{x.tmpdir, x.marks} {
		w.must(os.MkdirAll(d, 0o700))
	}
	// The runners inherit this environment, so the adapter finds the same
	// private server directory and never an enclosing tmux session.
	t.Setenv("TMUX_TMPDIR", x.tmpdir)
	t.Setenv("TMUX", "")
	t.Setenv("TMUX_PANE", "")
	os.Unsetenv("TMUX")
	os.Unsetenv("TMUX_PANE")
	// Registered before the server exists, so a failed start still cleans up.
	t.Cleanup(func() { _, _ = x.tm("kill-server") })
	if out, err := x.tm("new-session", "-d", "-s", "main", "-x", "100", "-y", "30", "sleep 600"); err != nil {
		t.Fatalf("cannot start the scratch tmux server: %v\n%s", err, out)
	}
	sock, err := x.tm("display", "-p", "#{socket_path}")
	if err != nil {
		t.Fatalf("tmux display: %v\n%s", err, sock)
	}
	x.socket = strings.TrimSpace(sock)
	if !strings.HasPrefix(x.socket, w.tmp+string(filepath.Separator)) {
		_, _ = x.tm("kill-server")
		t.Fatalf("ABORT: the scratch tmux socket %q lies outside %q; refusing to continue", x.socket, w.tmp)
	}
	pane, err := x.tm("new-window", "-d", "-P", "-F", "#{pane_id}", "sleep 601")
	if err != nil {
		t.Fatalf("tmux new-window: %v\n%s", err, pane)
	}
	x.pane = strings.TrimSpace(pane)
	return x
}

// tm runs tmux against the private server only.
func (x *wakeFixture) tm(args ...string) (string, error) {
	cmd := exec.Command("tmux", append([]string{"-L", x.name, "-f", "/dev/null"}, args...)...)
	var env []string
	for _, e := range os.Environ() {
		if !strings.HasPrefix(e, "TMUX=") && !strings.HasPrefix(e, "TMUX_PANE=") {
			env = append(env, e)
		}
	}
	cmd.Env = env
	out, err := cmd.CombinedOutput()
	return string(out), err
}

func (x *wakeFixture) panes() []string {
	out, err := x.tm("list-panes", "-a", "-F", "#{pane_id}")
	if err != nil {
		return nil
	}
	return strings.Fields(out)
}

func (x *wakeFixture) hasPane() bool {
	for _, p := range x.panes() {
		if p == x.pane {
			return true
		}
	}
	return false
}

// wakeRepo adds the repository whose consumer is the adapter. The payload
// (prompt.md, agent, pane, tmux-socket) is the adapter's own convention;
// Groundhog and Easyhog never read it.
func (w *world) wakeRepo(t *testing.T, f *fixture, x *wakeFixture) {
	t.Helper()
	agent := filepath.Join(w.tmp, "wake-agent.sh")
	src, err := os.ReadFile(repoFile(t, "examples", "consumers", "wake-agent.sh"))
	w.must(err)
	w.must(os.WriteFile(agent, src, 0o755))
	inst := w.instance("wake")
	w.consumer(inst, map[string]string{
		"GROUNDHOG_WAKE_STATE_DIR": x.state, "GROUNDHOG_WAKE_QUIET": "2", "GROUNDHOG_WAKE_PATIENCE": "60", "GROUNDHOG_WAKE_POLL": "1",
	}, x.consumer)
	w.item(inst, overdue, "wake-demo", "wake note")
	dir := filepath.Join(inst, "schedule", overdue, "wake-demo")
	for name, body := range map[string]string{
		"prompt.md":   "# synthetic resume prompt\n",
		"agent":       "/bin/sh " + agent + " " + x.marks + "\n",
		"pane":        x.pane + "\n",
		"tmux-socket": x.socket + "\n",
	} {
		w.must(os.WriteFile(filepath.Join(dir, name), []byte(body), 0o644))
	}
	f.wake = x
	f.wakeInst = inst
}
