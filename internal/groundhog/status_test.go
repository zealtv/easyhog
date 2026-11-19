package groundhog

import (
	"context"
	"encoding/json"
	"errors"
	"strings"
	"testing"
)

func statusWith(t *testing.T, doc string) (Status, error) {
	t.Helper()
	f := newFake(t).on("status", ok(doc))
	return newClient(t, f).Status(context.Background())
}

func TestStatusParsesFullSnapshot(t *testing.T) {
	st, err := statusWith(t, jsonOf(t, snapshot()))
	if err != nil {
		t.Fatal(err)
	}
	if st.Revision != 12 || st.JournalGeneration == nil || *st.JournalGeneration != 4 || !st.Consistent || !st.Paused {
		t.Errorf("tokens or flags wrong: %+v", st)
	}
	if st.Owner == nil || st.Owner.ID != "o-demo" || st.Owner.Generation != 1 || st.Owner.Label != "demo-session" {
		t.Errorf("owner: %+v", st.Owner)
	}
	if st.Runner.State != RunnerStopped || st.Runner.LastError != nil || !st.Runner.State.Known() {
		t.Errorf("runner: %+v", st.Runner)
	}
	if !st.Consumer.Configured || !st.Consumer.Valid || st.Consumer.ConfigDigest == nil || *st.Consumer.ConfigDigest != "sha256:example" {
		t.Errorf("consumer: %+v", st.Consumer)
	}
	if st.ObservedAt.UTC().Format("2006-01-02T15:04:05Z") != "2026-11-15T00:01:00Z" {
		t.Errorf("observed_at: %v", st.ObservedAt)
	}
	if len(st.Schedule) != 2 || len(st.Deliveries) != 2 || len(st.Diagnostics) != 1 {
		t.Fatalf("lengths: %d %d %d", len(st.Schedule), len(st.Deliveries), len(st.Diagnostics))
	}
	nudge, later := st.Schedule[0], st.Schedule[1]
	if nudge.Path != "schedule/daily/nudge" || nudge.ItemID != "nudge" || !nudge.Fired || nudge.EligibleNow || nudge.FiringAllowed {
		t.Errorf("nudge: %+v", nudge)
	}
	if nudge.Cadence == nil || nudge.Cadence.Axis != "daily" || nudge.Cadence.Time == nil || *nudge.Cadence.Time != "09:00" || nudge.Cadence.Day != nil {
		t.Errorf("cadence: %+v", nudge.Cadence)
	}
	if nudge.FiredMarker == nil || strings.Join(nudge.BlockedBy, ",") != "already_fired,instance_paused,owned" {
		t.Errorf("nudge markers: %+v", nudge)
	}
	if later.Cadence != nil || later.FiredMarker != nil || len(later.PausedBy) != 1 {
		t.Errorf("later: %+v", later)
	}
	var routed, loose *Delivery
	for i := range st.Deliveries { // do not rely on order
		if st.Deliveries[i].ID != nil && *st.Deliveries[i].ID == "d-demo-1" {
			routed = &st.Deliveries[i]
		} else {
			loose = &st.Deliveries[i]
		}
	}
	if routed == nil || loose == nil {
		t.Fatalf("deliveries: %+v", st.Deliveries)
	}
	if routed.State != DeliveryFailed || routed.Reason != nil || routed.Occurrence == nil || routed.Occurrence.Date != "2026-11-15" || routed.Occurrence.Slot != nil {
		t.Errorf("routed: %+v", routed)
	}
	if len(routed.Attempts) != 2 {
		t.Fatalf("attempts: %+v", routed.Attempts)
	}
	a1, a2 := routed.Attempts[0], routed.Attempts[1]
	if a1.ExitCode == nil || *a1.ExitCode != 7 || a1.FinishedAt == nil || a1.Error == nil || a1.Error.Code != "consumer_failed" || a1.Error.Path != nil {
		t.Errorf("attempt 1: %+v", a1)
	}
	if a2.FinishedAt != nil || a2.Signal == nil || *a2.Signal != "SIGPIPE" || a2.Decision == nil || *a2.Decision != "retry" || a2.DecidedAt == nil {
		t.Errorf("attempt 2: %+v", a2)
	}
	if loose.ID != nil || loose.SchedulePath != nil || loose.Occurrence != nil || loose.Origin != "legacy_out" || loose.Reason == nil || *loose.Reason != "unregistered" {
		t.Errorf("loose: %+v", loose)
	}
	if d := st.Diagnostics[0]; d.Code != "out_collision" || d.Path == nil || *d.Path != "out/loose" {
		t.Errorf("diagnostic: %+v", d)
	}
}

func TestStatusUnownedAndEmpty(t *testing.T) {
	doc := snapshot()
	doc["owner"] = nil
	doc["paused"] = false
	doc["schedule"] = []any{}
	doc["deliveries"] = []any{}
	doc["diagnostics"] = []any{}
	doc["runner"] = map[string]any{"state": "failed", "last_error": map[string]any{"code": "tick_failed", "message": "the tick failed", "path": nil}}
	st, err := statusWith(t, jsonOf(t, doc))
	if err != nil {
		t.Fatal(err)
	}
	if st.Owner != nil || st.Paused || len(st.Schedule)+len(st.Deliveries)+len(st.Diagnostics) != 0 {
		t.Errorf("%+v", st)
	}
	if st.Runner.State != RunnerFailed || st.Runner.LastError == nil || st.Runner.LastError.Code != "tick_failed" {
		t.Errorf("runner: %+v", st.Runner)
	}
}

func TestStatusIgnoresUnknownFields(t *testing.T) {
	doc := snapshot()
	doc["future"] = map[string]any{"anything": []int{1, 2}}
	doc["owner"].(map[string]any)["extra"] = "x"
	doc["runner"].(map[string]any)["pid"] = 42
	doc["consumer"].(map[string]any)["note"] = true
	doc["schedule"].([]any)[0].(map[string]any)["new_field"] = 1
	d := doc["deliveries"].([]any)[0].(map[string]any)
	d["out_collisions"] = []any{}
	d["attempts"].([]any)[0].(map[string]any)["host"] = "h"
	doc["diagnostics"].([]any)[0].(map[string]any)["hint"] = "h"
	if _, err := statusWith(t, jsonOf(t, doc)); err != nil {
		t.Fatal(err)
	}
}

func TestStatusKeepsUnknownEnumsForTheCallerToDecline(t *testing.T) {
	doc := snapshot()
	doc["runner"] = map[string]any{"state": "hibernating", "last_error": nil}
	doc["deliveries"].([]any)[0].(map[string]any)["state"] = "teleported"
	st, err := statusWith(t, jsonOf(t, doc))
	if err != nil {
		t.Fatal(err)
	}
	if st.Runner.State.Known() || st.Deliveries[0].State.Known() {
		t.Errorf("unknown enum values reported as known: %+v", st)
	}
}

func TestStatusMissingRequiredFields(t *testing.T) {
	paths := []string{
		"protocol_version", "instance", "observed_at", "revision",
		"consistent", "paused", "owner", "runner", "consumer", "schedule", "deliveries", "diagnostics",
		"runner.state", "consumer.configured", "consumer.valid", "owner.id", "owner.generation",
		"schedule.0.path", "schedule.0.item_id", "schedule.0.paused_by", "schedule.0.eligible_now",
		"schedule.0.fired", "schedule.0.firing_allowed", "schedule.0.blocked_by", "schedule.0.cadence.axis",
		"deliveries.0.path", "deliveries.0.origin", "deliveries.0.state", "deliveries.0.attempts", "deliveries.0.execution",
		"deliveries.0.occurrence.date",
		"deliveries.0.attempts.0.id", "deliveries.0.attempts.0.started_at", "deliveries.0.attempts.0.outcome",
		"deliveries.0.attempts.0.error.code", "deliveries.0.attempts.0.error.message",
		"diagnostics.0.code", "diagnostics.0.message",
	}
	for _, path := range paths {
		t.Run(path, func(t *testing.T) {
			doc := mapOf(t, jsonOf(t, snapshot()))
			deleteAt(t, doc, strings.Split(path, "."))
			_, err := statusWith(t, jsonOf(t, doc))
			var me *MalformedError
			if !errors.As(err, &me) || !errors.Is(err, ErrMalformed) {
				t.Fatalf("want MalformedError, got %v", err)
			}
			last := path[strings.LastIndex(path, ".")+1:]
			if !strings.Contains(strings.Join(me.Problems, ";"), last) {
				t.Errorf("problems %q do not name %q", me.Problems, last)
			}
		})
	}
}

func TestStatusParsesWithoutJournalGeneration(t *testing.T) {
	// The contract's field list omits journal_generation, so a script written
	// from the contract alone must still parse; the value is for display only.
	doc := mapOf(t, jsonOf(t, snapshot()))
	deleteAt(t, doc, []string{"journal_generation"})
	st, err := statusWith(t, jsonOf(t, doc))
	if err != nil {
		t.Fatal(err)
	}
	if st.JournalGeneration != nil || st.Revision != 12 || !st.Consistent {
		t.Errorf("got %+v", st)
	}
	doc["journal_generation"] = -1
	if _, err := statusWith(t, jsonOf(t, doc)); !errors.Is(err, ErrMalformed) {
		t.Errorf("a negative journal_generation should still be malformed, got %v", err)
	}
}

func TestStatusAcceptsMissingOptionalFields(t *testing.T) {
	// fired_marker, cadence, last_error, label, reason, slot, schedule_path,
	// occurrence and an attempt's finished_at, exit_code, signal and error may
	// be absent or null.
	doc := mapOf(t, jsonOf(t, snapshot()))
	deleteAt(t, doc, []string{"runner", "last_error"})
	deleteAt(t, doc, []string{"owner", "label"})
	deleteAt(t, doc, []string{"schedule", "0", "fired_marker"})
	deleteAt(t, doc, []string{"schedule", "0", "cadence"})
	deleteAt(t, doc, []string{"deliveries", "0", "reason"})
	deleteAt(t, doc, []string{"deliveries", "0", "occurrence", "slot"})
	deleteAt(t, doc, []string{"deliveries", "1", "schedule_path"})
	deleteAt(t, doc, []string{"deliveries", "1", "occurrence"})
	deleteAt(t, doc, []string{"deliveries", "0", "attempts", "0", "finished_at"})
	deleteAt(t, doc, []string{"deliveries", "0", "attempts", "0", "signal"})
	deleteAt(t, doc, []string{"deliveries", "0", "attempts", "1", "exit_code"})
	deleteAt(t, doc, []string{"deliveries", "0", "attempts", "1", "error"})
	if _, err := statusWith(t, jsonOf(t, doc)); err != nil {
		t.Fatal(err)
	}
}

func TestStatusWrongTypes(t *testing.T) {
	cases := map[string]func(map[string]any){
		"revision string":      func(d map[string]any) { d["revision"] = "12" },
		"revision float":       func(d map[string]any) { d["revision"] = 1.5 },
		"revision negative":    func(d map[string]any) { d["revision"] = -1 },
		"consistent string":    func(d map[string]any) { d["consistent"] = "true" },
		"owner array":          func(d map[string]any) { d["owner"] = []any{} },
		"owner generation str": func(d map[string]any) { d["owner"].(map[string]any)["generation"] = "1" },
		"schedule object":      func(d map[string]any) { d["schedule"] = map[string]any{} },
		"schedule null":        func(d map[string]any) { d["schedule"] = nil },
		"deliveries null":      func(d map[string]any) { d["deliveries"] = nil },
		"observed_at junk":     func(d map[string]any) { d["observed_at"] = "yesterday" },
		"attempt time junk": func(d map[string]any) {
			d["deliveries"].([]any)[0].(map[string]any)["attempts"].([]any)[0].(map[string]any)["started_at"] = "soon"
		},
		"paused_by string": func(d map[string]any) { d["schedule"].([]any)[0].(map[string]any)["paused_by"] = "x" },
		"exit_code string": func(d map[string]any) {
			d["deliveries"].([]any)[0].(map[string]any)["attempts"].([]any)[0].(map[string]any)["exit_code"] = "7"
		},
	}
	for name, mutate := range cases {
		t.Run(name, func(t *testing.T) {
			doc := snapshot()
			mutate(doc)
			st, err := statusWith(t, jsonOf(t, doc))
			if !errors.Is(err, ErrMalformed) {
				t.Fatalf("want ErrMalformed, got %v", err)
			}
			if st.Instance != "" || st.Schedule != nil {
				t.Errorf("half-filled snapshot returned: %+v", st)
			}
		})
	}
}

func TestStatusMalformedAndTruncatedJSON(t *testing.T) {
	good := jsonOf(t, snapshot())
	bad := []string{"", "\n", "not json", "[]", "null", `"text"`, "{", `{"protocol":`, good + good, good + "x", "<html>", good[:len(good)/2]}
	for _, doc := range bad {
		st, err := statusWith(t, doc)
		if !errors.Is(err, ErrMalformed) {
			t.Errorf("%.40q: want ErrMalformed, got %v", doc, err)
		}
		if st.Instance != "" {
			t.Errorf("%.40q: returned a snapshot", doc)
		}
	}
	// Every strict prefix of a good document fails; none yields a snapshot.
	trimmed := strings.TrimSpace(good)
	for n := 0; n < len(trimmed); n++ {
		st, err := statusWith(t, trimmed[:n])
		if err == nil || st.Instance != "" {
			t.Fatalf("prefix of %d bytes accepted", n)
		}
	}
	// The complete document, with or without its newline, is fine.
	for _, doc := range []string{good, trimmed, "\n" + good + "\n\n"} {
		if _, err := statusWith(t, doc); err != nil {
			t.Errorf("good document rejected: %v", err)
		}
	}
}

func TestStatusReportsEveryProblem(t *testing.T) {
	doc := snapshot()
	delete(doc, "revision")
	delete(doc, "paused")
	_, err := statusWith(t, jsonOf(t, doc))
	var me *MalformedError
	if !errors.As(err, &me) || len(me.Problems) < 2 {
		t.Fatalf("want two problems, got %v", err)
	}
	if !strings.Contains(err.Error(), "revision") || !strings.Contains(err.Error(), "paused") {
		t.Errorf("message hides problems: %v", err)
	}
}

func TestStatusProtocolAndInstanceChecks(t *testing.T) {
	doc := snapshot()
	doc["protocol_version"] = 2
	if _, err := statusWith(t, jsonOf(t, doc)); !asProtocol(err) {
		t.Errorf("version 2: %v", err)
	}
	doc = snapshot()
	doc["protocol"] = "other"
	if _, err := statusWith(t, jsonOf(t, doc)); !asProtocol(err) {
		t.Errorf("other protocol: %v", err)
	}
	doc = snapshot()
	doc["instance"] = "/example/elsewhere/.groundhog"
	_, err := statusWith(t, jsonOf(t, doc))
	var im *InstanceMismatchError
	if !errors.Is(err, ErrInstanceMismatch) || !errors.As(err, &im) || im.Got != "/example/elsewhere/.groundhog" || im.Want != fakeInstance {
		t.Errorf("instance mismatch: %v", err)
	}
}

func asProtocol(err error) bool {
	var pe *ProtocolError
	return errors.As(err, &pe)
}

func TestStatusArgv(t *testing.T) {
	f := newFake(t).on("status", ok(jsonOf(t, snapshot())))
	if _, err := newClient(t, f).Status(context.Background()); err != nil {
		t.Fatal(err)
	}
	inv := f.calls[0]
	if inv.Executable != fakeInstance+"/groundhog.sh" || strings.Join(inv.Args, " ") != "status --json" || inv.Dir != fakeInstance {
		t.Errorf("invocation: %+v", inv)
	}
}

// mapOf decodes a JSON document into generic maps for surgery.
func mapOf(t *testing.T, doc string) map[string]any {
	t.Helper()
	var m map[string]any
	if err := json.Unmarshal([]byte(doc), &m); err != nil {
		t.Fatal(err)
	}
	return m
}

// deleteAt removes the key at a path of map keys and slice indexes.
func deleteAt(t *testing.T, root map[string]any, path []string) {
	t.Helper()
	var cur any = root
	for i, key := range path {
		last := i == len(path)-1
		switch node := cur.(type) {
		case map[string]any:
			if last {
				if _, ok := node[key]; !ok {
					t.Fatalf("no key %q on path %v", key, path)
				}
				delete(node, key)
				return
			}
			cur = node[key]
		case []any:
			idx := int(key[0] - '0')
			cur = node[idx]
			if last {
				t.Fatalf("path %v ends at an index", path)
			}
		default:
			t.Fatalf("path %v leaves the document at %q", path, key)
		}
	}
}

func TestStatusLastTick(t *testing.T) {
	cases := []struct {
		name   string
		set    func(doc map[string]any)
		want   bool
		source string
	}{
		{"present", func(d map[string]any) {
			d["last_tick"] = map[string]any{"at": "2026-11-17T10:00:00Z", "source": "standalone"}
		}, true, "standalone"},
		{"null", func(d map[string]any) { d["last_tick"] = nil }, false, ""},
		{"absent", func(d map[string]any) { delete(d, "last_tick") }, false, ""},
		{"unreadable time", func(d map[string]any) {
			d["last_tick"] = map[string]any{"at": "yesterday", "source": "runner"}
		}, false, ""},
		{"wrong type", func(d map[string]any) { d["last_tick"] = "now" }, false, ""},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			doc := snapshot()
			c.set(doc)
			st, err := statusWith(t, jsonOf(t, doc))
			if err != nil {
				t.Fatalf("an optional liveness field must not fail the snapshot: %v", err)
			}
			if (st.LastTick != nil) != c.want {
				t.Fatalf("LastTick = %+v, want present=%v", st.LastTick, c.want)
			}
			if c.want && (st.LastTick.Source != c.source || st.LastTick.At.Format("2006-01-02T15:04:05Z") != "2026-11-17T10:00:00Z") {
				t.Errorf("LastTick = %+v", st.LastTick)
			}
		})
	}
}

// next_due feeds a countdown only: absent, null and unreadable values all read
// as no time, and none of them fails the snapshot.
func TestStatusNextDue(t *testing.T) {
	cases := []struct {
		name string
		set  func(item map[string]any)
		want string // "" for nil
	}{
		{"present", func(i map[string]any) { i["next_due"] = "2026-11-19T01:30:00Z" }, "2026-11-19T01:30:00Z"},
		{"with an offset", func(i map[string]any) { i["next_due"] = "2026-11-19T12:30:00+11:00" }, "2026-11-19T01:30:00Z"},
		{"null", func(i map[string]any) { i["next_due"] = nil }, ""},
		{"absent", func(i map[string]any) { delete(i, "next_due") }, ""},
		{"unreadable time", func(i map[string]any) { i["next_due"] = "soon" }, ""},
		{"wrong type", func(i map[string]any) { i["next_due"] = 42 }, ""},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			doc := snapshot()
			c.set(doc["schedule"].([]any)[0].(map[string]any))
			st, err := statusWith(t, jsonOf(t, doc))
			if err != nil {
				t.Fatalf("an optional countdown field must not fail the snapshot: %v", err)
			}
			got := st.Schedule[0].NextDue
			switch {
			case c.want == "" && got != nil:
				t.Fatalf("NextDue = %v, want nil", got)
			case c.want != "" && (got == nil || got.UTC().Format("2006-01-02T15:04:05Z") != c.want):
				t.Fatalf("NextDue = %v, want %s", got, c.want)
			}
			if st.Schedule[1].NextDue != nil {
				t.Errorf("the other item never had the field: %v", st.Schedule[1].NextDue)
			}
		})
	}
}
