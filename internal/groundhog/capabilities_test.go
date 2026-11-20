package groundhog

import (
	"context"
	"errors"
	"strings"
	"testing"
)

func TestCapabilitiesParses(t *testing.T) {
	doc := fullCaps()
	doc["future"] = "ignored"
	doc["capabilities"] = append(doc["capabilities"].([]string), "telepathy-v9")
	f := newFake(t).on("capabilities", ok(jsonOf(t, doc)))
	got, err := newClient(t, f).Capabilities(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	if got.Protocol != Protocol || got.ProtocolVersion != 2 || got.ImplementationVersion != "example-build" || !got.Has("handlers-v1") || !got.Has("telepathy-v9") || got.Has("nope") {
		t.Errorf("%+v", got)
	}
	if inv := f.calls[0]; strings.Join(inv.Args, " ") != "capabilities --json" || inv.Executable != fakeInstance+"/groundhog.sh" {
		t.Errorf("invocation: %+v", inv)
	}
}

func TestCapabilitiesStrict(t *testing.T) {
	good := jsonOf(t, fullCaps())
	for _, key := range []string{"protocol", "protocol_version", "capabilities"} {
		doc := fullCaps()
		delete(doc, key)
		f := newFake(t).on("capabilities", ok(jsonOf(t, doc)))
		_, err := newClient(t, f).Capabilities(context.Background())
		if !errors.Is(err, ErrMalformed) || !strings.Contains(err.Error(), key) {
			t.Errorf("without %s: %v", key, err)
		}
	}
	for _, doc := range []string{"", "nope", "[]", good[:len(good)/2], good + good, `{"protocol":"groundhog-runtime","protocol_version":"2","capabilities":[]}`} {
		f := newFake(t).on("capabilities", ok(doc))
		if _, err := newClient(t, f).Capabilities(context.Background()); !errors.Is(err, ErrMalformed) {
			t.Errorf("%.30q: %v", doc, err)
		}
	}
}

func TestNegotiate(t *testing.T) {
	without := func(drop string) map[string]any {
		doc := fullCaps()
		var keep []string
		for _, c := range doc["capabilities"].([]string) {
			if c != drop {
				keep = append(keep, c)
			}
		}
		doc["capabilities"] = keep
		return doc
	}
	protocol1 := map[string]any{
		"protocol": "groundhog-runtime", "protocol_version": 1,
		"capabilities": []string{"status-json-v1", "ownership-v1", "runner-v1", "pause-v1", "consumer-v1", "delivery-v1"},
	}
	version3 := fullCaps()
	version3["protocol_version"] = 3
	otherProtocol := fullCaps()
	otherProtocol["protocol"] = "something-else"
	noLockHelper := fullCaps()
	noLockHelper["capabilities"] = []string{"status-json-v2", "next-due-v1", "add-from-v1", "pause-v2"}

	tests := []struct {
		name         string
		reply        reply
		controllable bool
		inspectable  bool
		reason       string // substring
		missing      string
	}{
		{"everything", ok(jsonOf(t, fullCaps())), true, true, "", ""},
		{"no pause", ok(jsonOf(t, without("pause-v2"))), false, true, "pause-v2", "pause-v2"},
		{"host without a lock helper", ok(jsonOf(t, noLockHelper)), false, true, "handlers-v1", "handlers-v1"},
		{"no status", ok(jsonOf(t, without("status-json-v2"))), false, false, "status-json-v2", "status-json-v2"},
		{"protocol 1, an older Groundhog", ok(jsonOf(t, protocol1)), false, false, "older Groundhog (runtime protocol 1", ""},
		{"unknown protocol version", ok(jsonOf(t, version3)), false, false, "version 3", ""},
		{"other protocol", ok(jsonOf(t, otherProtocol)), false, false, "something-else", ""},
		{"legacy script", exits(1, "error: unknown command 'capabilities'\n"), false, false, "legacy", ""},
		{"legacy script, other exit", exits(2, "usage: groundhog.sh <command>\n"), false, false, "exit 2", ""},
		{"malformed answer", ok("{not json"), false, false, "malformed", ""},
		{"empty answer", ok(""), false, false, "malformed", ""},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			f := newFake(t).on("capabilities", tc.reply)
			sup, err := newClient(t, f).Negotiate(context.Background())
			if err != nil {
				t.Fatal(err)
			}
			if sup.Controllable != tc.controllable || sup.Inspectable != tc.inspectable || sup.ReadOnly() == tc.controllable {
				t.Errorf("%+v", sup)
			}
			if !tc.controllable && !strings.Contains(sup.Reason, tc.reason) {
				t.Errorf("reason %q lacks %q", sup.Reason, tc.reason)
			}
			if tc.controllable && sup.Reason != "" {
				t.Errorf("reason on a controllable script: %q", sup.Reason)
			}
			if tc.missing != "" && !strings.Contains(strings.Join(sup.Missing, ","), strings.Split(tc.missing, ",")[0]) {
				t.Errorf("missing %v", sup.Missing)
			}
			if !tc.controllable && !strings.HasPrefix(sup.String(), "unsupported, read-only: ") {
				t.Errorf("String: %q", sup.String())
			}
		})
	}
}

func TestNegotiateMissingIsSorted(t *testing.T) {
	doc := fullCaps()
	doc["capabilities"] = []string{"next-due-v1"}
	f := newFake(t).on("capabilities", ok(jsonOf(t, doc)))
	sup, _ := newClient(t, f).Negotiate(context.Background())
	want := "handlers-v1,pause-v2,status-json-v2"
	if got := strings.Join(sup.Missing, ","); got != want {
		t.Errorf("missing %q, want %q", got, want)
	}
}

func TestNegotiateErrorsWhenNothingIsKnown(t *testing.T) {
	// Busy, a start failure and a hang say nothing about the script.
	f := newFake(t).on("capabilities", exits(4, "busy"))
	if _, err := newClient(t, f).Negotiate(context.Background()); !IsRetryable(err) {
		t.Errorf("busy: %v", err)
	}
	f = newFake(t).on("capabilities", reply{err: errors.New("fork/exec: permission denied")})
	if _, err := newClient(t, f).Negotiate(context.Background()); err == nil {
		t.Error("start failure reported as unsupported")
	}
	f = newFake(t).on("capabilities", hangs())
	_, err := newClient(t, f).Negotiate(context.Background(), WithTimeout(20*1e6))
	if !errors.Is(err, context.DeadlineExceeded) {
		t.Errorf("hang: %v", err)
	}
}

func TestUnsupportedScriptNeverReceivesControl(t *testing.T) {
	legacy := exits(1, "error: unknown command 'capabilities'")
	f := newFake(t).on("capabilities", legacy)
	c := newClient(t, f)
	calls := []func() error{
		func() error { return c.Tick(context.Background()) },
		func() error { return c.Retry(context.Background(), "out/report") },
		func() error { return c.Pause(context.Background()) },
		func() error { return c.Resume(context.Background()) },
	}
	for i, call := range calls {
		err := call()
		var ue *UnsupportedError
		if !errors.Is(err, ErrUnsupported) || !errors.As(err, &ue) || !strings.Contains(ue.Reason, "legacy") {
			t.Errorf("call %d: %v", i, err)
		}
	}
	for _, cmd := range f.commands() {
		if cmd != "capabilities" {
			t.Errorf("an unsupported script was asked to %q", cmd)
		}
	}
}

func TestNegotiationIsRememberedUntilRenegotiated(t *testing.T) {
	f := newFake(t).
		on("capabilities", ok(jsonOf(t, fullCaps()))).
		on("pause", ok("paused\n"))
	c := newClient(t, f)
	for i := 0; i < 3; i++ {
		if err := c.Pause(context.Background()); err != nil {
			t.Fatal(err)
		}
	}
	if got := strings.Join(f.commands(), " "); got != "capabilities pause pause pause" {
		t.Errorf("commands: %s", got)
	}
	// The script is replaced by a legacy one; a refresh makes control stop.
	f.replies["capabilities"] = []reply{exits(1, "unknown command")}
	if sup, err := c.Negotiate(context.Background()); err != nil || sup.Controllable {
		t.Fatalf("%+v %v", sup, err)
	}
	if err := c.Pause(context.Background()); !errors.Is(err, ErrUnsupported) {
		t.Errorf("after renegotiation: %v", err)
	}
}
