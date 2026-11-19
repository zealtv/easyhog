package groundhog

import (
	"context"
	"errors"
	"strings"
	"testing"
)

// capsWith is fullCaps plus the optional capabilities named.
func capsWith(extra ...string) map[string]any {
	doc := fullCaps()
	doc["capabilities"] = append(doc["capabilities"].([]string), extra...)
	return doc
}

func lastArgs(f *fakeExec) string { return strings.Join(f.calls[len(f.calls)-1].Args, " ") }

// `status --json --active` is passed when, and only when, the script advertised
// status-active-v1 to this client's negotiation. One place (Client.Status)
// decides; no caller does.
func TestStatusPassesActiveOnlyWhenTheScriptOffersIt(t *testing.T) {
	for _, tc := range []struct {
		name      string
		caps      map[string]any // nil: the client never negotiates
		want      string
		negotiate bool
	}{
		{"offered", capsWith(CapStatusActive), "status --json --active", true},
		{"offered with others", capsWith(CapReleaseUnresolved, CapStatusActive), "status --json --active", true},
		{"not offered (the older pin)", fullCaps(), "status --json", true},
		{"only the other optional one", capsWith(CapReleaseUnresolved), "status --json", true},
		{"not negotiated yet", nil, "status --json", false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			doc := snapshot()
			doc["settled"] = map[string]any{"accepted": 0, "missing": 0} // an --active read carries it
			f := newFake(t).on("status", ok(jsonOf(t, doc)))
			if tc.negotiate {
				f.on("capabilities", ok(jsonOf(t, tc.caps)))
			}
			c := newClient(t, f)
			if tc.negotiate {
				if _, err := c.Negotiate(context.Background()); err != nil {
					t.Fatal(err)
				}
			}
			if _, err := c.Status(context.Background()); err != nil {
				t.Fatal(err)
			}
			if got := lastArgs(f); got != tc.want {
				t.Errorf("argv %q, want %q", got, tc.want)
			}
		})
	}
}

func TestStatusReadsSettledCountsAndDeliveryCountsAddThemToTheListed(t *testing.T) {
	doc := snapshot()
	doc["settled"] = map[string]any{"accepted": 4501, "missing": 500}
	st, err := statusWith(t, jsonOf(t, doc))
	if err != nil {
		t.Fatal(err)
	}
	if st.Settled == nil || st.Settled.Accepted != 4501 || st.Settled.Missing != 500 {
		t.Fatalf("settled %+v", st.Settled)
	}
	counts := st.DeliveryCounts()
	// The snapshot lists one failed and one pending delivery.
	if counts[DeliveryFailed] != 1 || counts[DeliveryPending] != 1 || counts[DeliveryAccepted] != 4501 || counts[DeliveryMissing] != 500 {
		t.Errorf("counts %v", counts)
	}
}

func TestStatusWithoutSettledCountsOnlyWhatItLists(t *testing.T) {
	st, err := statusWith(t, jsonOf(t, snapshot()))
	if err != nil {
		t.Fatal(err)
	}
	if st.Settled != nil {
		t.Errorf("settled %+v for a read without the field", st.Settled)
	}
	if counts := st.DeliveryCounts(); counts[DeliveryAccepted] != 0 || counts[DeliveryFailed] != 1 || counts[DeliveryPending] != 1 {
		t.Errorf("counts %v", counts)
	}
}

func TestStatusSettledMustBeWellFormedWhenPresent(t *testing.T) {
	for name, settled := range map[string]any{
		"accepted missing": map[string]any{"missing": 1},
		"missing missing":  map[string]any{"accepted": 1},
		"negative":         map[string]any{"accepted": -1, "missing": 0},
		"a string":         map[string]any{"accepted": "many", "missing": 0},
	} {
		t.Run(name, func(t *testing.T) {
			doc := snapshot()
			doc["settled"] = settled
			_, err := statusWith(t, jsonOf(t, doc))
			var me *MalformedError
			if !errors.As(err, &me) {
				t.Fatalf("got %v, want a *MalformedError", err)
			}
		})
	}
}

// ReleaseLeavingUnresolved passes --leave-unresolved only to a script that
// advertised release-unresolved-v1; any other script gets a plain release.
func TestReleaseLeavingUnresolvedIsNegotiated(t *testing.T) {
	for _, tc := range []struct {
		name string
		caps map[string]any
		want string
	}{
		{"offered", capsWith(CapReleaseUnresolved), "release --token-file " + tokenPath + " --leave-unresolved"},
		{"offered with status-active", capsWith(CapStatusActive, CapReleaseUnresolved), "release --token-file " + tokenPath + " --leave-unresolved"},
		{"not offered (the older pin)", fullCaps(), "release --token-file " + tokenPath},
		{"only the other optional one", capsWith(CapStatusActive), "release --token-file " + tokenPath},
	} {
		t.Run(tc.name, func(t *testing.T) {
			f := newFake(t).on("capabilities", ok(jsonOf(t, tc.caps))).on("release", ok(""))
			if err := newClient(t, f).ReleaseLeavingUnresolved(context.Background(), tokenPath); err != nil {
				t.Fatal(err)
			}
			if got := lastArgs(f); got != tc.want {
				t.Errorf("argv %q, want %q", got, tc.want)
			}
		})
	}
}

// Plain Release never passes the flag, even to a script that offers it: a plain
// release keeps its refusal while an attempt is unresolved.
func TestPlainReleaseNeverPassesTheFlag(t *testing.T) {
	f := newFake(t).on("capabilities", ok(jsonOf(t, capsWith(CapReleaseUnresolved)))).on("release", ok(""))
	if err := newClient(t, f).Release(context.Background(), tokenPath); err != nil {
		t.Fatal(err)
	}
	if got := lastArgs(f); got != "release --token-file "+tokenPath {
		t.Errorf("argv %q", got)
	}
}

func TestReleaseLeavingUnresolvedRefusesAnUnsupportedScript(t *testing.T) {
	f := newFake(t).on("capabilities", ok(jsonOf(t, map[string]any{
		"protocol": "groundhog-runtime", "protocol_version": 1, "capabilities": []string{"status-json-v1", CapReleaseUnresolved},
	})))
	err := newClient(t, f).ReleaseLeavingUnresolved(context.Background(), tokenPath)
	var ue *UnsupportedError
	if !errors.As(err, &ue) {
		t.Fatalf("got %v, want an *UnsupportedError", err)
	}
	for _, c := range f.commands() {
		if c == "release" {
			t.Error("release ran on a script that is not controllable")
		}
	}
}

// A read made with --active promises `settled`; without it the settled
// deliveries would be neither listed nor counted, so the document is malformed.
// A read made without the flag is not expected to carry it.
func TestStatusAfterAnActiveReadRequiresSettled(t *testing.T) {
	for name, settled := range map[string]any{"absent": nil, "null": "null"} {
		t.Run(name, func(t *testing.T) {
			doc := snapshot()
			if settled != nil {
				doc["settled"] = nil // JSON null
			}
			f := newFake(t).on("capabilities", ok(jsonOf(t, capsWith(CapStatusActive)))).on("status", ok(jsonOf(t, doc)))
			c := newClient(t, f)
			if _, err := c.Negotiate(context.Background()); err != nil {
				t.Fatal(err)
			}
			_, err := c.Status(context.Background())
			var me *MalformedError
			if !errors.As(err, &me) || !strings.Contains(strings.Join(me.Problems, ";"), "settled") {
				t.Fatalf("got %v, want a *MalformedError naming settled", err)
			}
		})
	}
	// Without the capability the same document is fine.
	f := newFake(t).on("capabilities", ok(jsonOf(t, fullCaps()))).on("status", ok(jsonOf(t, snapshot())))
	c := newClient(t, f)
	if _, err := c.Negotiate(context.Background()); err != nil {
		t.Fatal(err)
	}
	if _, err := c.Status(context.Background()); err != nil {
		t.Errorf("a plain read without settled: %v", err)
	}
}

// `status --json --next-due` is passed when, and only when, the script
// advertised next-due-v1, alone or together with --active, in one place
// (Client.Status).
func TestStatusPassesNextDueOnlyWhenTheScriptOffersIt(t *testing.T) {
	for _, tc := range []struct {
		name      string
		caps      map[string]any
		want      string
		negotiate bool
	}{
		{"offered", capsWith(CapNextDue), "status --json --next-due", true},
		{"offered with active", capsWith(CapStatusActive, CapNextDue), "status --json --active --next-due", true},
		{"only active", capsWith(CapStatusActive), "status --json --active", true},
		{"not offered (the older pin)", fullCaps(), "status --json", true},
		{"not negotiated yet", nil, "status --json", false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			doc := snapshot()
			doc["settled"] = map[string]any{"accepted": 0, "missing": 0}
			f := newFake(t).on("status", ok(jsonOf(t, doc)))
			if tc.negotiate {
				f.on("capabilities", ok(jsonOf(t, tc.caps)))
			}
			c := newClient(t, f)
			if tc.negotiate {
				if _, err := c.Negotiate(context.Background()); err != nil {
					t.Fatal(err)
				}
			}
			if _, err := c.Status(context.Background()); err != nil {
				t.Fatal(err)
			}
			if got := lastArgs(f); got != tc.want {
				t.Errorf("argv %q, want %q", got, tc.want)
			}
		})
	}
}
