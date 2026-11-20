package groundhog

import (
	"context"
	"encoding/json"
	"fmt"
	"sync"
	"testing"

	"github.com/zealtv/easyhog/internal/runner"
)

// Synthetic instance used by the fixture tests. Nothing here exists on disk;
// the path has a space on purpose.
const fakeInstance = "/example/my project/.groundhog"

// exitStatus stands in for *exec.ExitError: the client reads only ExitCode.
type exitStatus int

func (e exitStatus) Error() string { return fmt.Sprintf("exit status %d", int(e)) }
func (e exitStatus) ExitCode() int { return int(e) }

// reply is what one fake process does.
type reply struct {
	stdout, stderr string
	err            error
	// block makes the process wait for cancellation, like a hung script.
	block bool
}

func ok(stdout string) reply              { return reply{stdout: stdout} }
func exits(code int, stderr string) reply { return reply{stderr: stderr, err: exitStatus(code)} }
func hangs() reply                        { return reply{block: true} }
func jsonOf(t testing.TB, v any) string {
	t.Helper()
	b, err := json.Marshal(v)
	if err != nil {
		t.Fatal(err)
	}
	return string(b) + "\n"
}

// fakeExec is a scripted runner.Executor. Replies are matched by the first
// argument (the command); a command without a reply fails the test.
type fakeExec struct {
	t       *testing.T
	mu      sync.Mutex
	replies map[string][]reply
	calls   []runner.Invocation
	// deadlines records the context deadline each call saw.
	deadlines []bool
}

func newFake(t *testing.T) *fakeExec {
	return &fakeExec{t: t, replies: map[string][]reply{}}
}

// on queues replies for a command; the last one repeats.
func (f *fakeExec) on(command string, rs ...reply) *fakeExec {
	f.replies[command] = append(f.replies[command], rs...)
	return f
}

func (f *fakeExec) Run(ctx context.Context, inv runner.Invocation) error {
	f.mu.Lock()
	f.calls = append(f.calls, inv)
	_, hasDeadline := ctx.Deadline()
	f.deadlines = append(f.deadlines, hasDeadline)
	var r reply
	if len(inv.Args) == 0 {
		f.mu.Unlock()
		f.t.Fatalf("call without a command: %+v", inv)
	}
	q := f.replies[inv.Args[0]]
	switch {
	case len(q) == 0:
		f.mu.Unlock()
		f.t.Fatalf("unexpected command %q (args %q)", inv.Args[0], inv.Args)
		return nil
	case len(q) > 1:
		r, f.replies[inv.Args[0]] = q[0], q[1:]
	default:
		r = q[0]
	}
	f.mu.Unlock()
	if r.block {
		<-ctx.Done()
		return ctx.Err()
	}
	if inv.Stdout != nil && r.stdout != "" {
		if _, err := inv.Stdout.Write([]byte(r.stdout)); err != nil {
			return err
		}
	}
	if inv.Stderr != nil && r.stderr != "" {
		_, _ = inv.Stderr.Write([]byte(r.stderr))
	}
	return r.err
}

func (f *fakeExec) commands() []string {
	f.mu.Lock()
	defer f.mu.Unlock()
	var out []string
	for _, c := range f.calls {
		out = append(out, c.Args[0])
	}
	return out
}

func newClient(t *testing.T, f *fakeExec, opts ...Option) *Client {
	t.Helper()
	c, err := New(f, fakeInstance, opts...)
	if err != nil {
		t.Fatal(err)
	}
	return c
}

// fullCaps is a capabilities answer with everything protocol 2 offers.
func fullCaps() map[string]any {
	return map[string]any{
		"protocol": "groundhog-runtime", "protocol_version": 2, "implementation_version": "example-build",
		"capabilities": []string{"status-json-v2", "next-due-v1", "add-from-v1", "pause-v2", "handlers-v1"},
	}
}

// snapshot returns a synthetic, fully populated status document.
func snapshot() map[string]any {
	return map[string]any{
		"protocol": "groundhog-runtime", "protocol_version": 2,
		"instance":    fakeInstance,
		"observed_at": "2026-11-15T00:01:00Z",
		"paused":      true,
		"last_tick":   map[string]any{"at": "2026-11-15T00:00:04Z", "source": "standalone"},
		"schedule": []any{
			map[string]any{
				"path": "schedule/daily/nudge", "item_id": "nudge",
				"cadence":   map[string]any{"axis": "daily", "day": nil, "date": nil, "time": "09:00", "interval": nil, "defaulted": false},
				"paused_by": []string{}, "eligible_now": false,
				"fired_marker": "fired/2026-11-15/daily/nudge", "fired": true,
				"firing_allowed": false, "blocked_by": []string{"already_fired", "instance_paused"},
			},
			map[string]any{
				"path": "schedule/once/later", "item_id": "later", "cadence": nil,
				"paused_by": []string{"schedule/once.paused"}, "eligible_now": false,
				"fired_marker": nil, "fired": false, "firing_allowed": false, "blocked_by": []string{"subtree_paused"},
			},
		},
		"deliveries": []any{
			map[string]any{
				"path": "out/nudge-2026-11-15", "id": "d-3f9c0a1b2c3d4e5f", "state": "failed",
				"fired_at": "2026-11-15T00:00:04Z", "schedule_path": "schedule/daily/nudge",
				"occurrence": map[string]any{"date": "2026-11-15", "slot": nil},
				"run":        true, "exit_status": 7,
				"attempts": []any{
					map[string]any{"id": "a-77aa01b2c3d4e5f6", "started_at": "2026-11-15T00:00:04Z", "by": "tick",
						"finished_at": "2026-11-15T00:00:31Z", "exit_status": 0},
					map[string]any{"id": "a-88bb01b2c3d4e5f6", "started_at": "2026-11-15T00:00:40Z", "by": "retry",
						"finished_at": "2026-11-15T00:00:41Z", "exit_status": 7},
				},
				"log": "out/nudge-2026-11-15/.groundhog-log",
			},
			map[string]any{
				"path": "out/loose", "id": nil, "state": "unrecorded", "fired_at": nil, "schedule_path": nil,
				"occurrence": nil, "run": false, "exit_status": nil, "attempts": []any{}, "log": nil,
			},
		},
		"diagnostics": []any{
			map[string]any{"code": "out_collision", "message": "an out/ name already existed", "path": "out/loose"},
		},
	}
}
