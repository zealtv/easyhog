package groundhog

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

const tokenPath = "/example/session dir/owner-token"

func controlClient(t *testing.T, f *fakeExec, opts ...Option) *Client {
	t.Helper()
	f.on("capabilities", ok(jsonOf(t, fullCaps())))
	return newClient(t, f, opts...)
}

type controlCall struct {
	name string
	run  func(*Client, context.Context, ...Option) error
}

func controlCalls() []controlCall {
	return []controlCall{
		{"enroll", func(c *Client, ctx context.Context, o ...Option) error {
			return c.Enroll(ctx, "demo session", tokenPath, o...)
		}},
		{"pause", func(c *Client, ctx context.Context, o ...Option) error { return c.Pause(ctx, tokenPath, o...) }},
		{"resume", func(c *Client, ctx context.Context, o ...Option) error { return c.Resume(ctx, tokenPath, o...) }},
		{"stop", func(c *Client, ctx context.Context, o ...Option) error { return c.Stop(ctx, tokenPath, o...) }},
		{"release", func(c *Client, ctx context.Context, o ...Option) error { return c.Release(ctx, tokenPath, o...) }},
	}
}

func TestControlArgvIsExact(t *testing.T) {
	for _, cc := range controlCalls() {
		t.Run(cc.name, func(t *testing.T) {
			f := newFake(t).on(cc.name, ok("done\n"))
			c := controlClient(t, f)
			if err := cc.run(c, context.Background()); err != nil {
				t.Fatal(err)
			}
			inv := f.calls[len(f.calls)-1]
			if inv.Executable != fakeInstance+"/groundhog.sh" || inv.Dir != fakeInstance {
				t.Errorf("executable %q dir %q", inv.Executable, inv.Dir)
			}
			if cc.name == "enroll" {
				want := []string{"enroll", "--owner-label", "demo session", "--token-file", tokenPath}
				if strings.Join(inv.Args, "|") != strings.Join(want, "|") {
					t.Errorf("argv %q, want %q", inv.Args, want)
				}
			} else if strings.Join(inv.Args, "|") != cc.name+"|--token-file|"+tokenPath {
				t.Errorf("argv %q", inv.Args)
			}
		})
	}
}

// Exit codes: the contract's table plus the recorded readings.
// Each command is exercised with each code.
func TestControlExitCodeMapping(t *testing.T) {
	table := []struct {
		code      int
		kind      Kind
		retryable bool
	}{
		{1, KindUnexpected, false},
		{2, KindInvalid, false},
		{3, KindOwnership, false},
		{4, KindBusy, true},
		{5, KindState, false},
		{6, KindIO, false},
		{7, KindUncertain, false}, // enroll only; elsewhere unexpected
		{8, KindUnexpected, false},
		{126, KindUnexpected, false},
		{127, KindUnexpected, false},
		{141, KindUnexpected, false},
		{-1, KindUnexpected, false},
	}
	for _, cc := range controlCalls() {
		for _, row := range table {
			want := row.kind
			if row.code == 7 && cc.name != "enroll" {
				want = KindUnexpected
			}
			f := newFake(t).on(cc.name, exits(row.code, "error: "+cc.name+" failed\n"))
			err := cc.run(controlClient(t, f), context.Background())
			if err == nil {
				t.Errorf("%s exit %d: no error", cc.name, row.code)
				continue
			}
			var ee *ExitError
			if cc.name == "enroll" {
				switch row.code {
				case 2, 3, 4, 5, 6: // certain: no owner was committed
					if errors.Is(err, ErrEnrollUncertain) {
						t.Errorf("enroll exit %d reported uncertain", row.code)
					}
				default:
					var ue *UncertainEnrollError
					if !errors.As(err, &ue) || !errors.Is(err, ErrEnrollUncertain) || ue.TokenFile != tokenPath {
						t.Errorf("enroll exit %d: want uncertain, got %v", row.code, err)
					}
				}
			}
			if !errors.As(err, &ee) {
				t.Errorf("%s exit %d: not an ExitError: %v", cc.name, row.code, err)
				continue
			}
			if ee.Code != row.code || ee.Kind != want || ee.Retryable() != (want == KindBusy) || IsRetryable(err) != (want == KindBusy) {
				t.Errorf("%s exit %d: %+v, want kind %v", cc.name, row.code, ee, want)
			}
			if !strings.Contains(err.Error(), cc.name+" failed") || !strings.Contains(err.Error(), "exit") {
				t.Errorf("%s exit %d: stderr missing from %q", cc.name, row.code, err.Error())
			}
		}
	}
}

func TestEnrollOutcomeCertainty(t *testing.T) {
	t.Run("success", func(t *testing.T) {
		f := newFake(t).on("enroll", ok("enrolled: owner o-demo, generation 1\n"))
		if err := controlClient(t, f).Enroll(context.Background(), "demo", tokenPath); err != nil {
			t.Fatal(err)
		}
	})
	t.Run("timeout is uncertain, not failed", func(t *testing.T) {
		f := newFake(t).on("enroll", hangs())
		err := controlClient(t, f).Enroll(context.Background(), "demo", tokenPath, WithTimeout(20*time.Millisecond))
		var ue *UncertainEnrollError
		if !errors.As(err, &ue) || !errors.Is(err, ErrEnrollUncertain) || !errors.Is(err, context.DeadlineExceeded) || ue.TokenFile != tokenPath {
			t.Fatalf("%v", err)
		}
		var ee *ExitError
		if errors.As(err, &ee) {
			t.Error("a timeout was presented as an exit")
		}
	})
	t.Run("cancellation is uncertain, not failed", func(t *testing.T) {
		f := newFake(t).on("enroll", hangs())
		ctx, cancel := context.WithCancel(context.Background())
		go func() { time.Sleep(20 * time.Millisecond); cancel() }()
		err := controlClient(t, f).Enroll(ctx, "demo", tokenPath)
		if !errors.Is(err, ErrEnrollUncertain) || !errors.Is(err, context.Canceled) {
			t.Fatalf("%v", err)
		}
	})
	t.Run("an executor failure is uncertain", func(t *testing.T) {
		f := newFake(t).on("enroll", reply{err: errors.New("wait: pipe broken")})
		if err := controlClient(t, f).Enroll(context.Background(), "demo", tokenPath); !errors.Is(err, ErrEnrollUncertain) {
			t.Fatalf("%v", err)
		}
	})
	t.Run("a flood of stdout is uncertain", func(t *testing.T) {
		f := newFake(t).on("enroll", ok(strings.Repeat("x", 5000)))
		err := controlClient(t, f).Enroll(context.Background(), "demo", tokenPath, WithMaxStdout(1000))
		if !errors.Is(err, ErrEnrollUncertain) || !errors.Is(err, ErrOutputLimit) {
			t.Fatalf("%v", err)
		}
	})
	t.Run("cancelled before it starts leaves nothing uncertain", func(t *testing.T) {
		f := newFake(t)
		c := controlClient(t, f)
		ctx, cancel := context.WithCancel(context.Background())
		cancel()
		// Negotiation is cancelled first, so no enroll ran.
		err := c.Enroll(ctx, "demo", tokenPath)
		if err == nil || errors.Is(err, ErrEnrollUncertain) {
			t.Fatalf("%v", err)
		}
		for _, cmd := range f.commands() {
			if cmd == "enroll" {
				t.Error("enroll ran with a cancelled context")
			}
		}
	})
}

func TestControlTimeoutsLeaveAnUnknownOutcome(t *testing.T) {
	for _, cc := range controlCalls()[1:] {
		f := newFake(t).on(cc.name, hangs())
		err := cc.run(controlClient(t, f), context.Background(), WithTimeout(20*time.Millisecond))
		var ce *CallError
		if !errors.As(err, &ce) || !errors.Is(err, context.DeadlineExceeded) {
			t.Errorf("%s: %v", cc.name, err)
		}
		var ee *ExitError
		if errors.As(err, &ee) {
			t.Errorf("%s: timeout presented as exit %d", cc.name, ee.Code)
		}
	}
}

func TestControlArgumentValidation(t *testing.T) {
	f := newFake(t)
	c := controlClient(t, f)
	ctx := context.Background()
	inside := fakeInstance + "/runtime/token"
	bad := map[string]error{
		"empty label":    c.Enroll(ctx, "  ", tokenPath),
		"newline label":  c.Enroll(ctx, "a\nb", tokenPath),
		"relative token": c.Enroll(ctx, "demo", "token"),
		"empty token":    c.Enroll(ctx, "demo", ""),
		"token inside":   c.Enroll(ctx, "demo", inside),
		"token is dir":   c.Enroll(ctx, "demo", fakeInstance),
		"nul token":      c.Pause(ctx, "/a\x00b"),
		"relative pause": c.Pause(ctx, "token"),
		"empty release":  c.Release(ctx, ""),
	}
	for name, err := range bad {
		if !errors.Is(err, ErrInvalidArgument) {
			t.Errorf("%s: %v", name, err)
		}
	}
	if len(f.calls) != 0 {
		t.Errorf("invalid calls started %d processes", len(f.calls))
	}
	// A sibling that merely shares the prefix is outside the instance.
	f.on("enroll", ok(""))
	if err := c.Enroll(ctx, "demo", fakeInstance+"-private/token"); err != nil {
		t.Errorf("sibling directory rejected: %v", err)
	}
}

func TestTokenContentsNeverLeave(t *testing.T) {
	dir := t.TempDir()
	tok := filepath.Join(dir, "token")
	const secret = "SECRET-0123456789abcdef"
	if err := os.WriteFile(tok, []byte(`{"schema_version":1,"token":"`+secret+`"}`), 0o600); err != nil {
		t.Fatal(err)
	}
	for _, cc := range []string{"pause", "resume", "stop", "release"} {
		f := newFake(t).on(cc, exits(3, "error: the token does not match the current owner\n"))
		c := controlClient(t, f)
		var err error
		switch cc {
		case "pause":
			err = c.Pause(context.Background(), tok)
		case "resume":
			err = c.Resume(context.Background(), tok)
		case "stop":
			err = c.Stop(context.Background(), tok)
		case "release":
			err = c.Release(context.Background(), tok)
		}
		if err == nil || strings.Contains(err.Error(), secret) {
			t.Errorf("%s: %v", cc, err)
		}
		for _, inv := range f.calls {
			if strings.Contains(strings.Join(inv.Args, " "), secret) {
				t.Errorf("%s: token in argv", cc)
			}
			if inv.Stdout == nil || inv.Stderr == nil {
				t.Errorf("%s: output not captured", cc)
			}
		}
	}
}

func TestStopHasItsOwnDefaultDeadline(t *testing.T) {
	// The fake records only whether a deadline exists; check the resolved
	// settings directly for the values.
	c := newClient(t, newFake(t))
	for command, want := range map[string]time.Duration{"stop": DefaultStopTimeout, "status": DefaultTimeout, "pause": DefaultTimeout} {
		s, err := c.resolve(command, nil)
		if err != nil || s.timeout != want {
			t.Errorf("%s: %v %v", command, s.timeout, err)
		}
	}
	s, _ := c.resolve("stop", []Option{WithTimeout(time.Second)})
	if s.timeout != time.Second {
		t.Errorf("per-call override ignored: %v", s.timeout)
	}
	cc := newClient(t, newFake(t), WithTimeout(5*time.Second))
	if s, _ := cc.resolve("stop", nil); s.timeout != 5*time.Second {
		t.Errorf("client override ignored for stop: %v", s.timeout)
	}
	if s, _ := cc.resolve("stop", []Option{WithTimeout(7 * time.Second)}); s.timeout != 7*time.Second {
		t.Errorf("call option must beat client option: %v", s.timeout)
	}
}

func TestEveryCallCarriesADeadline(t *testing.T) {
	f := newFake(t).on("status", ok(jsonOf(t, snapshot()))).on("pause", ok(""))
	c := controlClient(t, f)
	if _, err := c.Status(context.Background()); err != nil {
		t.Fatal(err)
	}
	if err := c.Pause(context.Background(), tokenPath); err != nil {
		t.Fatal(err)
	}
	for i, has := range f.deadlines {
		if !has {
			t.Errorf("call %d (%s) ran without a deadline", i, f.calls[i].Args[0])
		}
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

// Retry and Recover take a delivery (and a decision) as well as the token.
func TestRetryAndRecoverArgvIsExact(t *testing.T) {
	cases := []struct {
		name string
		run  func(*Client) error
		want string
	}{
		{"retry", func(c *Client) error { return c.Retry(context.Background(), tokenPath, "d-demo-1") },
			"retry|--token-file|" + tokenPath + "|--delivery|d-demo-1"},
		{"recover", func(c *Client) error {
			return c.Recover(context.Background(), tokenPath, "d-demo-1", DecisionAccept)
		}, "recover|--token-file|" + tokenPath + "|--delivery|d-demo-1|--decision|accept"},
		{"recover", func(c *Client) error {
			return c.Recover(context.Background(), tokenPath, "d.demo_2-x", DecisionRetry)
		}, "recover|--token-file|" + tokenPath + "|--delivery|d.demo_2-x|--decision|retry"},
	}
	for _, tc := range cases {
		f := newFake(t).on(tc.name, ok("done\n"))
		if err := tc.run(controlClient(t, f)); err != nil {
			t.Fatal(err)
		}
		inv := f.calls[len(f.calls)-1]
		if got := strings.Join(inv.Args, "|"); got != tc.want {
			t.Errorf("argv %q, want %q", got, tc.want)
		}
		if inv.Executable != fakeInstance+"/groundhog.sh" || inv.Dir != fakeInstance {
			t.Errorf("executable %q dir %q", inv.Executable, inv.Dir)
		}
	}
}

func TestRetryAndRecoverRejectBadArgumentsBeforeAnyProcess(t *testing.T) {
	f := newFake(t)
	c := controlClient(t, f)
	ctx := context.Background()
	bad := map[string]error{
		"empty id":         c.Retry(ctx, tokenPath, ""),
		"id with a slash":  c.Retry(ctx, tokenPath, "a/b"),
		"id starts a dash": c.Retry(ctx, tokenPath, "-x"),
		"id has a space":   c.Recover(ctx, tokenPath, "a b", DecisionAccept),
		"id has a newline": c.Recover(ctx, tokenPath, "a\nb", DecisionRetry),
		"id not ascii":     c.Retry(ctx, tokenPath, "dé"),
		"relative token":   c.Retry(ctx, "token", "d1"),
		"empty token":      c.Recover(ctx, "", "d1", DecisionAccept),
		"no decision":      c.Recover(ctx, tokenPath, "d1", ""),
		"unknown decision": c.Recover(ctx, tokenPath, "d1", Decision("abandon")),
	}
	for name, err := range bad {
		if !errors.Is(err, ErrInvalidArgument) {
			t.Errorf("%s: %v", name, err)
		}
	}
	if len(f.calls) != 0 {
		t.Errorf("invalid calls started %d processes", len(f.calls))
	}
}

func TestRetryAndRecoverExitCodesAndTimeouts(t *testing.T) {
	run := map[string]func(*Client, ...Option) error{
		"retry": func(c *Client, o ...Option) error { return c.Retry(context.Background(), tokenPath, "d1", o...) },
		"recover": func(c *Client, o ...Option) error {
			return c.Recover(context.Background(), tokenPath, "d1", DecisionAccept, o...)
		},
	}
	for name, call := range run {
		for _, row := range []struct {
			code int
			kind Kind
		}{{2, KindInvalid}, {3, KindOwnership}, {4, KindBusy}, {5, KindState}, {6, KindIO}, {7, KindUnexpected}, {1, KindUnexpected}} {
			f := newFake(t).on(name, exits(row.code, "error: "+name+" refused\n"))
			err := call(controlClient(t, f))
			var ee *ExitError
			if !errors.As(err, &ee) || ee.Code != row.code || ee.Kind != row.kind || IsRetryable(err) != (row.kind == KindBusy) {
				t.Errorf("%s exit %d: %v", name, row.code, err)
			}
			if err != nil && !strings.Contains(err.Error(), name+" refused") {
				t.Errorf("%s exit %d: stderr missing from %q", name, row.code, err)
			}
		}
		f := newFake(t).on(name, hangs())
		err := call(controlClient(t, f), WithTimeout(20*time.Millisecond))
		var ce *CallError
		if !errors.As(err, &ce) || !errors.Is(err, context.DeadlineExceeded) {
			t.Errorf("%s timeout: %v", name, err)
		}
	}
}

func TestRetryAndRecoverRefuseALegacyScript(t *testing.T) {
	f := newFake(t).on("capabilities", ok(jsonOf(t, map[string]any{
		"protocol": "groundhog-runtime", "protocol_version": 1, "implementation_version": "x",
		"capabilities": []string{"status-json-v1"},
	})))
	c := newClient(t, f)
	if err := c.Retry(context.Background(), tokenPath, "d1"); !errors.Is(err, ErrUnsupported) {
		t.Errorf("retry: %v", err)
	}
	if err := c.Recover(context.Background(), tokenPath, "d1", DecisionRetry); !errors.Is(err, ErrUnsupported) {
		t.Errorf("recover: %v", err)
	}
	for _, cmd := range f.commands() {
		if cmd == "retry" || cmd == "recover" {
			t.Errorf("%s ran against a script that is not controllable", cmd)
		}
	}
}
