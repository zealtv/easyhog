package groundhog

import (
	"context"
	"errors"
	"strings"
	"testing"
	"time"
)

func controlClient(t *testing.T, f *fakeExec, opts ...Option) *Client {
	t.Helper()
	f.on("capabilities", ok(jsonOf(t, fullCaps())))
	return newClient(t, f, opts...)
}

type controlCall struct {
	name string
	argv string
	run  func(*Client, context.Context, ...Option) error
}

func controlCalls() []controlCall {
	return []controlCall{
		{"tick", "tick", func(c *Client, ctx context.Context, o ...Option) error { return c.Tick(ctx, o...) }},
		{"retry", "retry|out/report 2026-05-01", func(c *Client, ctx context.Context, o ...Option) error {
			return c.Retry(ctx, "out/report 2026-05-01", o...)
		}},
		{"pause", "pause", func(c *Client, ctx context.Context, o ...Option) error { return c.Pause(ctx, o...) }},
		{"resume", "resume", func(c *Client, ctx context.Context, o ...Option) error { return c.Resume(ctx, o...) }},
	}
}

func TestControlArgvIsExact(t *testing.T) {
	for _, cc := range controlCalls() {
		t.Run(cc.name, func(t *testing.T) {
			f := newFake(t).on(cc.name, ok("done\n"))
			if err := cc.run(controlClient(t, f), context.Background()); err != nil {
				t.Fatal(err)
			}
			inv := f.calls[len(f.calls)-1]
			if inv.Executable != fakeInstance+"/groundhog.sh" || inv.Dir != fakeInstance {
				t.Errorf("executable %q dir %q", inv.Executable, inv.Dir)
			}
			if got := strings.Join(inv.Args, "|"); got != cc.argv {
				t.Errorf("argv %q, want %q", got, cc.argv)
			}
			if inv.Stdout == nil || inv.Stderr == nil {
				t.Error("output not bounded: a stream was left to the executor")
			}
		})
	}
}

// Exit codes by the contract's table: `retry`, `pause` and `resume` give 3 and
// 5 a meaning; `tick` does not, and keeps 1 for a firing that failed.
func TestControlExitCodeMapping(t *testing.T) {
	table := []struct {
		code int
		kind Kind
	}{
		{1, KindUnexpected}, {2, KindInvalid}, {3, KindPaused}, {4, KindBusy}, {5, KindNothing}, {6, KindIO},
		{7, KindUnexpected}, {126, KindUnexpected}, {127, KindUnexpected}, {141, KindUnexpected}, {-1, KindUnexpected},
	}
	for _, cc := range controlCalls() {
		for _, row := range table {
			want := row.kind
			switch {
			case cc.name == "tick" && (row.code == 3 || row.code == 5):
				want = KindUnexpected
			case cc.name == "tick" && row.code == 1:
				want = KindFiringFailed
			}
			f := newFake(t).on(cc.name, exits(row.code, "error: "+cc.name+" failed\n"))
			err := cc.run(controlClient(t, f), context.Background())
			var ee *ExitError
			if !errors.As(err, &ee) {
				t.Errorf("%s exit %d: not an ExitError: %v", cc.name, row.code, err)
				continue
			}
			if ee.Code != row.code || ee.Kind != want || ee.Retryable() != (want == KindBusy) || IsRetryable(err) != (want == KindBusy) {
				t.Errorf("%s exit %d: %+v, want kind %v", cc.name, row.code, ee, want)
			}
			if !strings.Contains(err.Error(), cc.name+" failed") {
				t.Errorf("%s exit %d: stderr missing from %q", cc.name, row.code, err.Error())
			}
		}
	}
}

func TestControlTimeoutsLeaveAnUnknownOutcome(t *testing.T) {
	for _, cc := range controlCalls() {
		f := newFake(t).on(cc.name, hangs())
		err := cc.run(controlClient(t, f), context.Background(), WithTimeout(20*time.Millisecond))
		var ce *CallError
		var ee *ExitError
		if !errors.As(err, &ce) || !errors.Is(err, context.DeadlineExceeded) || errors.As(err, &ee) {
			t.Errorf("%s: %v", cc.name, err)
		}
	}
}

func TestRetryTakesOnlyADeliveryUnderOut(t *testing.T) {
	f := newFake(t)
	c := controlClient(t, f)
	for _, bad := range []string{"", "out/", "report", "out/a/b", "out/../x", "/abs/out/x", "out/.landing-x", "out/x.landing",
		"out/./x", "out/a\x00b", "schedule/daily/x", "out/.hidden"} {
		if err := c.Retry(context.Background(), bad); !errors.Is(err, ErrInvalidArgument) {
			t.Errorf("%q: %v", bad, err)
		}
	}
	if len(f.calls) != 0 {
		t.Errorf("invalid calls started %d processes", len(f.calls))
	}
}

func TestEveryCallCarriesADeadline(t *testing.T) {
	f := newFake(t).on("status", ok(jsonOf(t, snapshot()))).on("tick", ok(""))
	c := controlClient(t, f)
	if _, err := c.Status(context.Background()); err != nil {
		t.Fatal(err)
	}
	if err := c.Tick(context.Background()); err != nil {
		t.Fatal(err)
	}
	for i, has := range f.deadlines {
		if !has {
			t.Errorf("call %d (%s) ran without a deadline", i, f.calls[i].Args[0])
		}
	}
	s, _ := c.resolve("tick", nil)
	if s.timeout != DefaultTimeout {
		t.Errorf("tick's default bound: %v", s.timeout)
	}
	if s, _ := c.resolve("tick", []Option{WithTimeout(time.Second)}); s.timeout != time.Second {
		t.Errorf("per-call override ignored: %v", s.timeout)
	}
}

func TestInvalidOptions(t *testing.T) {
	if _, err := New(newFake(t), fakeInstance, WithTimeout(0)); !errors.Is(err, ErrInvalidArgument) {
		t.Errorf("zero timeout: %v", err)
	}
	if _, err := New(nil, fakeInstance); !errors.Is(err, ErrInvalidArgument) {
		t.Errorf("nil executor: %v", err)
	}
	for _, in := range []string{"", "relative/.groundhog", "/a\x00b"} {
		if _, err := New(newFake(t), in); !errors.Is(err, ErrInvalidArgument) {
			t.Errorf("instance %q: %v", in, err)
		}
	}
	f := newFake(t)
	if _, err := newClient(t, f).Status(context.Background(), WithMaxStdout(-1)); !errors.Is(err, ErrInvalidArgument) {
		t.Errorf("negative limit: %v", err)
	}
	if len(f.calls) != 0 {
		t.Error("a process started despite a bad option")
	}
}
