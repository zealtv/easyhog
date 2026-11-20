package main

import (
	"errors"
	"os"
	"path/filepath"
	"strings"
	"time"

	"github.com/zealtv/easyhog/internal/core"
	"github.com/zealtv/easyhog/internal/discovery"
	"github.com/zealtv/easyhog/internal/groundhog"
)

// Synthetic instances live at /work/projects/<name>/.groundhog. Their payload
// files are written under a temporary directory and found by mapPath, so no
// real path shows in a capture.
const synthRoot = "/work/projects"

var taken = time.Date(2026, 11, 15, 9, 30, 15, 0, time.Local)

func mapPath(tmp, instance string) string {
	return filepath.Join(tmp, strings.TrimPrefix(instance, synthRoot))
}

func writeFiles(tmp string, files map[string][]byte) error {
	for rel, data := range files {
		p := filepath.Join(tmp, filepath.FromSlash(rel))
		if err := os.MkdirAll(filepath.Dir(p), 0o755); err != nil {
			return err
		}
		if err := os.WriteFile(p, data, 0o644); err != nil {
			return err
		}
	}
	return nil
}

func sp(s string) *string { return &s }

func inst(name string) discovery.Instance {
	return discovery.Instance{Path: synthRoot + "/" + name + "/.groundhog", Display: name + "/.groundhog"}
}

func glance(items ...string) discovery.Glance {
	var g discovery.Glance
	for _, it := range items {
		g.Schedule = append(g.Schedule, discovery.ScheduleDir{Path: it, Leaf: true, Paused: strings.Contains(it, ".paused")})
	}
	g.Out = []string{"report-2026-11-14", "report-2026-11-15"}
	g.Runtime = true
	return g
}

func item(path string, cad *groundhog.Cadence) groundhog.ScheduleItem {
	return groundhog.ScheduleItem{Path: "schedule/" + path, ItemID: filepath.Base(path), Cadence: cad}
}

func status(items ...groundhog.ScheduleItem) groundhog.Status {
	return groundhog.Status{Protocol: groundhog.Protocol, ProtocolVersion: groundhog.ProtocolVersion, ObservedAt: taken, Schedule: items}
}

// ticked names the synthetic instances this session ticks, and when it last did.
var ticked = map[string]time.Duration{
	"team/worker": 12 * time.Second, "charlie": 2 * time.Second, "team/bravo/nightly-exports/regional": 20 * time.Second,
	"paused": 20 * time.Second, "running": 12 * time.Second, "running-fresh": 12 * time.Second,
	"running-late": 2 * time.Minute, "running-quiet": 4 * time.Minute,
}

// tickLookup is the ticker's side for the captures: the instances in ticked.
func tickLookup(key string) (core.TickInfo, bool) {
	name := strings.TrimSuffix(strings.TrimPrefix(key, synthRoot+"/"), "/.groundhog")
	age, ok := ticked[name]
	if !ok {
		return core.TickInfo{}, false
	}
	at := taken.Add(-age)
	return core.TickInfo{Ticking: true, Interval: core.DefaultTickInterval, LastBegan: at, LastEnded: at}, true
}

// tickedHere marks a status as ticked by this session age before the read.
func tickedHere(age time.Duration) func(*groundhog.Status) {
	return func(st *groundhog.Status) {
		st.LastTick = &groundhog.LastTick{At: taken.Add(-age), Source: "standalone"}
	}
}

func read(name string, class core.Class, health core.Health, st groundhog.Status, files discovery.Glance) core.InstanceView {
	o := &core.Observation{Status: st, TakenAt: taken}
	return core.InstanceView{Instance: inst(name), Files: files, Class: class, Health: health,
		Inspection: core.Inspection{Last: o}}
}

func listed(name string, files discovery.Glance) core.InstanceView {
	return core.InstanceView{Instance: inst(name), Files: files, Class: core.ClassNotInspected}
}

var payloads = map[string][]byte{
	"alpha/.groundhog/schedule/daily/09/report/prompt.md":                 []byte("# Daily report\n\nSummarise **yesterday's** changes in three bullets, as `plain text`.\n\n- what moved\n- what stalled\n- what to do next\n\n> Keep it short.\n\n```sh\ngit log --since=yesterday\n```\n"),
	"alpha/.groundhog/schedule/daily/09/report/context.json":              []byte("{\n  \"repo\": \"projects/alpha\",\n  \"since\": \"yesterday\",\n  \"limit\": 20,\n  \"labels\": [\"moved\", \"stalled\"]\n}\n"),
	"alpha/.groundhog/schedule/daily/09/report/notes.txt":                 []byte("Keep it short.\nAsk before sending anything outside the team.\n"),
	"alpha/.groundhog/schedule/every/15m/beat/ping":                       {0x7f, 'E', 'L', 'F', 0, 1, 2, 3},
	"alpha/.groundhog/schedule/weekly.paused/mon/review/agenda.md":        []byte("# Weekly review\n\n1. Last week\n2. This week\n"),
	"alpha/.groundhog/schedule/once/2026-12-01/08-00/launch/checklist.md": []byte(strings.Repeat("- [ ] a long checklist line that goes on and on to show wrapping in a narrow pane\n", 60)),
	"alpha/.groundhog/schedule/monthly/1/rent/.keep":                      []byte(""),
	"external/.groundhog/schedule/daily/sync/run":                         []byte("#!/bin/sh\n# Groundhog starts this for each delivery; Easyhog only shows it.\necho syncing \"$1\"\n"),
	"legacy/.groundhog/schedule/daily/old/note.md":                        []byte("An item in a copy of the script that predates the runtime commands.\n"),
	"failing/.groundhog/schedule/daily/export/job.md":                     []byte("Export the weekly figures.\n"),
}

func view(name string) (core.View, map[string][]byte, bool) {
	v := core.View{Root: synthRoot, DiscoveredAt: taken}
	switch name {
	case "empty-view":
		return v, nil, true
	case "crowded":
		v.Instances = crowded()
	case "legacy":
		v.Instances = []core.InstanceView{legacy(), listed("fresh", glance("daily/morning-nudge", "weekly/mon/standup")), malformed()}
	case "failing":
		v.Instances = failing()
	case "stale-read":
		v.Instances = stale()
	case "external":
		v.Instances = external()
	case "actions":
		v.Instances = actions()
	case "verdicts":
		v.Instances = verdicts()
	case "rows":
		v.Instances = rows()
	case "items":
		v.Instances = items()
	case "heartbeat":
		v.Instances = heartbeat()
	case "countdown":
		v.Instances = countdown()
	default:
		return v, nil, false
	}
	return v, payloads, true
}

func alpha() core.InstanceView {
	report := item("daily/09/report", &groundhog.Cadence{Axis: "daily", Time: sp("09:00")})
	report.EligibleNow, report.FiringAllowed = true, true
	review := item("weekly.paused/mon/review", &groundhog.Cadence{Axis: "weekly", Day: sp("mon"), Defaulted: true})
	review.PausedBy, review.BlockedBy = []string{"schedule/weekly.paused"}, []string{"subtree_paused", "not_due"}
	beat := item("every/15m/beat", &groundhog.Cadence{Axis: "every", Interval: sp("15m")})
	beat.BlockedBy = []string{"not_due"}
	launch := item("once/2026-12-01/08-00/launch", &groundhog.Cadence{Axis: "once", Date: sp("2026-12-01"), Time: sp("08:00")})
	launch.BlockedBy = []string{"not_due"}
	// Two items named "report" (daily/09 and weekly/mon) show the clash rule.
	weeklyReport := item("weekly/mon/report", &groundhog.Cadence{Axis: "weekly", Day: sp("mon"), Defaulted: true})
	weeklyReport.BlockedBy = []string{"not_due"}
	st := status(report, review, beat, launch, item("monthly/1/rent", &groundhog.Cadence{Axis: "monthly", Day: sp("1"), Defaulted: true}), weeklyReport)
	st.Deliveries = []groundhog.Delivery{
		{Path: "out/report-2026-11-14", SchedulePath: sp(report.Path), State: groundhog.DeliveryAccepted},
		{Path: "out/report-2026-11-15", SchedulePath: sp(report.Path), State: groundhog.DeliveryPending},
	}
	return read("alpha", core.ClassSupported, core.HealthOK, st,
		glance("daily/09/report", "weekly.paused/mon/review", "every/15m/beat", "once/2026-12-01/08-00/launch", "monthly/1/rent", "weekly/mon/report"))
}

func external() []core.InstanceView {
	run := item("daily/sync", &groundhog.Cadence{Axis: "daily", Defaulted: true})
	run.BlockedBy = []string{"already_fired"}
	run.Fired = true
	st := status(run)
	// Ticked 40 s before the read by a `tick` from outside Easyhog (cron, say).
	st.LastTick = &groundhog.LastTick{At: taken.Add(-40 * time.Second), Source: "standalone"}
	loop := status(item("weekly/fri/summary", &groundhog.Cadence{Axis: "weekly", Day: sp("fri"), Defaulted: false}))
	loop.LastTick = &groundhog.LastTick{At: taken.Add(-25 * time.Second), Source: "runner"}
	return []core.InstanceView{
		read("external", core.ClassSupported, core.HealthOK, st, glance("daily/sync")),
		read("shared/docs", core.ClassSupported, core.HealthOK, loop, glance("weekly/fri/summary")),
		read("quiet", core.ClassSupported, core.HealthOK, status(item("daily/09/report", &groundhog.Cadence{Axis: "daily", Time: sp("09:00")})), glance("daily/09/report")),
	}
}

func legacy() core.InstanceView {
	v := listed("legacy", glance("daily/old"))
	v.Class = core.ClassLegacy
	v.Inspection.Support = &groundhog.Support{Reason: "the script is an older Groundhog (runtime protocol 1, before items carried their own run); Easyhog reads protocol 2, so it only shows this instance"}
	return v
}

func malformed() core.InstanceView {
	v := listed("broken", discovery.Glance{})
	v.Class = core.ClassMalformed
	v.Instance.Malformed = []string{"groundhog.sh is missing", "schedule/ is not a directory"}
	return v
}

func failing() []core.InstanceView {
	export := item("daily/export", &groundhog.Cadence{Axis: "daily", Defaulted: true})
	export.EligibleNow, export.FiringAllowed = true, true
	code := 3
	st := status(export)
	started := taken.Add(-time.Minute)
	st.Deliveries = []groundhog.Delivery{{Path: "out/export-2026-11-15", SchedulePath: sp(export.Path), State: groundhog.DeliveryFailed, Run: true, ExitStatus: &code,
		Attempts: []groundhog.Attempt{{ID: sp("a-1"), StartedAt: &started, By: "tick", FinishedAt: &started, ExitStatus: &code}},
		Log:      sp("out/export-2026-11-15/.groundhog-log")}}
	f := read("failing", core.ClassSupported, core.HealthOK, st, glance("daily/export"))
	f.Inspection.Err, f.Inspection.ErrAt = errors.New("status: timed out after 15s"), taken.Add(40*time.Second)
	never := listed("never-read", glance("daily/x"))
	never.Class = core.ClassFailing
	never.Inspection.Err, never.Inspection.ErrAt = errors.New("status: exit status 6: state is locked"), taken
	return []core.InstanceView{f, never, alpha()}
}

func stale() []core.InstanceView {
	s := read("stale", core.ClassSupported, core.HealthStale, status(item("daily/report", nil)), glance("daily/report"))
	return []core.InstanceView{s, listed("fresh", glance("daily/nudge"))}
}

func crowded() []core.InstanceView {
	runner := status(item("every/5m/tick", &groundhog.Cadence{Axis: "every", Interval: sp("5m")}))
	tickedHere(12 * time.Second)(&runner)
	out := []core.InstanceView{alpha(), read("team/worker", core.ClassSupported, core.HealthOK, runner, glance("every/5m/tick"))}
	out = append(out, external()...)
	out = append(out, failing()[:2]...)
	out = append(out, stale()...)
	out = append(out, legacy(), malformed())
	for _, n := range []string{"notes/a", "notes/b", "tools/c", "tools/d", "tools/e", "archive/f"} {
		out = append(out, listed(n, glance("daily/x", "weekly/y")))
	}
	return out
}

// actions is a small tree for the Run and Stop captures: three instances Run
// is offered for (one of which will be refused, one never answers), one this
// session ticks (Stop), and one ticked from outside (Run too).
func actions() []core.InstanceView {
	ready := func(name string) core.InstanceView {
		return read(name, core.ClassSupported, core.HealthOK,
			status(item("daily/09/report", &groundhog.Cadence{Axis: "daily", Time: sp("09:00")})), glance("daily/09/report"))
	}
	runningStatus := status(item("every/5m/tick", &groundhog.Cadence{Axis: "every", Interval: sp("5m")}))
	tickedHere(12 * time.Second)(&runningStatus)
	runningStatus.Deliveries = []groundhog.Delivery{{Path: "out/tick-2026-11-15-0925", SchedulePath: sp("schedule/every/5m/tick"), State: groundhog.DeliveryPending}}
	ext := external()[0]
	return []core.InstanceView{
		ready("alpha"), ready("nolock"),
		read("team/worker", core.ClassSupported, core.HealthOK, runningStatus, glance("every/5m/tick")),
		ready("slow"), ext,
	}
}

// rows is the tree's rows as the reader scans them: a running instance whose
// delivery failed, a paused one with a backlog under a long path, an
// interrupted run, and one with scheduled items, one of which has a run waiting.
func rows() []core.InstanceView {
	delivery := func(path, sched string, state groundhog.DeliveryState) groundhog.Delivery {
		return groundhog.Delivery{Path: "out/" + path, SchedulePath: sp("schedule/" + sched), State: state}
	}
	beat := item("every/5m/beat", &groundhog.Cadence{Axis: "every", Interval: sp("5m")})

	failedRun := status(beat)
	tickedHere(2 * time.Second)(&failedRun)
	failedRun.Deliveries = []groundhog.Delivery{delivery("beat-1", "every/5m/beat", groundhog.DeliveryFailed)}

	backlog := status(beat)
	tickedHere(20 * time.Second)(&backlog)
	backlog.Paused = true
	for i := 1; i <= 4; i++ {
		backlog.Deliveries = append(backlog.Deliveries, delivery("beat-"+string(rune('0'+i)), "every/5m/beat", groundhog.DeliveryPending))
	}

	unsure := status(beat)
	unsure.Deliveries = []groundhog.Delivery{delivery("beat-1", "every/5m/beat", groundhog.DeliveryInterrupted)}

	daily := item("daily/09/report", &groundhog.Cadence{Axis: "daily", Time: sp("09:00")})
	once := item("once/2026-12-01/08-00/launch", &groundhog.Cadence{Axis: "once", Date: sp("2026-12-01"), Time: sp("08:00")})
	once.BlockedBy = []string{"not_due"}
	tidy := status(daily, once, beat)
	tidy.Deliveries = []groundhog.Delivery{delivery("report-1", "daily/09/report", groundhog.DeliveryPending)}

	return []core.InstanceView{
		read("alpha", core.ClassSupported, core.HealthOK, tidy, glance("daily/09/report", "once/2026-12-01/08-00/launch", "every/5m/beat")),
		read("charlie", core.ClassSupported, core.HealthOK, failedRun, glance("every/5m/beat")),
		read("team/bravo/nightly-exports/regional", core.ClassSupported, core.HealthOK, backlog, glance("every/5m/beat")),
		read("delta", core.ClassSupported, core.HealthOK, unsure, glance("every/5m/beat")),
	}
}

// items is the Pause item and Unpause item capture: an instance with a scheduled
// item, an item paused by its own folder name, an item paused through a folder
// above it, and a pair (`twin` and `twin.paused`) that blocks both toggles; and
// an instance ticked from outside, whose items toggle all the same.
func items() []core.InstanceView {
	daily := &groundhog.Cadence{Axis: "daily", Time: sp("09:00")}
	report := item("daily/09/report", daily)
	report.EligibleNow, report.FiringAllowed = true, true
	digest := item("daily/09/digest.paused", daily)
	digest.PausedBy, digest.BlockedBy = []string{"schedule/daily/09/digest.paused"}, []string{"subtree_paused"}
	review := item("weekly.paused/mon/review", &groundhog.Cadence{Axis: "weekly", Day: sp("mon"), Defaulted: true})
	review.PausedBy, review.BlockedBy = []string{"schedule/weekly.paused"}, []string{"subtree_paused", "not_due"}
	twin := item("daily/09/twin", daily)
	twinPaused := item("daily/09/twin.paused", daily)
	twinPaused.PausedBy = []string{twinPaused.Path}
	st := status(report, digest, review, twin, twinPaused)
	st.Deliveries = []groundhog.Delivery{{Path: "out/digest-2026-11-17", SchedulePath: sp("schedule/daily/09/digest"), State: groundhog.DeliveryPending}}
	ext := external()[0]
	return []core.InstanceView{
		read("alpha", core.ClassSupported, core.HealthOK, st,
			glance("daily/09/report", "daily/09/digest.paused", "weekly.paused/mon/review", "daily/09/twin", "daily/09/twin.paused")),
		ext,
	}
}

// countdown is an instance with one item at each distance the badge words
// (a half minute to months), a paused one, a fired one-shot, one due now, one
// held, and one with no next_due (what an older script reports). The times are
// worked out from the clock when the program starts.
func countdown() []core.InstanceView {
	now := time.Now()
	daily := &groundhog.Cadence{Axis: "daily", Time: sp("09:00")}
	var its []groundhog.ScheduleItem
	var paths []string
	add := func(name string, in time.Duration) groundhog.ScheduleItem {
		it := item("every/1h/"+name, &groundhog.Cadence{Axis: "every", Interval: sp("1h")})
		it.BlockedBy = []string{"not_due"}
		if in != 0 {
			due := now.Add(in)
			it.NextDue = &due
		}
		its = append(its, it)
		paths = append(paths, "every/1h/"+name)
		return it
	}
	add("a-half-minute", 31*time.Second)
	add("b-fifteen-minutes", 15*time.Minute)
	add("c-three-hours", 3*time.Hour+20*time.Minute)
	add("d-a-day-and-a-bit", 26*time.Hour)
	add("e-nine-days", 9*24*time.Hour)
	add("f-a-week", 7*24*time.Hour)
	add("g-three-weeks", 21*24*time.Hour)
	add("h-two-months", 62*24*time.Hour)
	due := item("daily/09/due-now", daily)
	due.EligibleNow, due.FiringAllowed, due.NextDue = true, true, &now
	held := item("daily/09/due-held", daily)
	held.EligibleNow, held.BlockedBy, held.NextDue = true, []string{"instance_paused"}, &now
	paused := item("daily/09/paused.paused", daily)
	paused.PausedBy, paused.BlockedBy = []string{paused.Path}, []string{"subtree_paused"}
	fired := item("once/2026-11-18/07-00/launch", &groundhog.Cadence{Axis: "once", Date: sp("2026-11-18"), Time: sp("07:00")})
	fired.Fired, fired.BlockedBy = true, []string{"already_fired"}
	older := item("daily/09/older-script", daily)
	older.BlockedBy = []string{"not_due"}
	its = append(its, due, held, paused, fired, older)
	paths = append(paths, "daily/09/due-now", "daily/09/due-held", "daily/09/paused.paused", "once/2026-11-18/07-00/launch", "daily/09/older-script")
	return []core.InstanceView{read("alpha", core.ClassSupported, core.HealthOK, status(its...), glance(paths...))}
}

// verdicts has one instance for each answer to "will its pending work run?":
// paused, running here, ticked recently by something else, ticked long ago,
// and never ticked.
func verdicts() []core.InstanceView {
	build := func(name string, class core.Class, mutate func(*groundhog.Status)) core.InstanceView {
		st := status(item("every/5m/beat", &groundhog.Cadence{Axis: "every", Interval: sp("5m")}))
		st.Deliveries = []groundhog.Delivery{{Path: "out/beat-2026-11-15-0925", SchedulePath: sp("schedule/every/5m/beat"), State: groundhog.DeliveryPending}}
		mutate(&st)
		return read(name, class, core.HealthOK, st, glance("every/5m/beat"))
	}
	tick := func(age time.Duration, source string) *groundhog.LastTick {
		return &groundhog.LastTick{At: taken.Add(-age), Source: source}
	}
	return []core.InstanceView{
		build("paused", core.ClassSupported, func(st *groundhog.Status) { tickedHere(20 * time.Second)(st); st.Paused = true }),
		build("running", core.ClassSupported, tickedHere(12*time.Second)),
		build("probably", core.ClassSupported, func(st *groundhog.Status) { st.LastTick = tick(45*time.Second, "standalone") }),
		build("old", core.ClassSupported, func(st *groundhog.Status) { st.LastTick = tick(25*time.Minute, "standalone") }),
		build("never", core.ClassSupported, func(st *groundhog.Status) {}),
	}
}

// heartbeat has the rows something drives at each level (this session's ticking
// and two kinds of outside ticker, fresh, late and quiet), then rows that get no
// mark: a stopped instance with an old tick, a paused one, and one never ticked.
func heartbeat() []core.InstanceView {
	build := func(name string, class core.Class, mutate func(*groundhog.Status)) core.InstanceView {
		st := status(item("every/1m/beat", &groundhog.Cadence{Axis: "every", Interval: sp("1m")}))
		mutate(&st)
		return read(name, class, core.HealthOK, st, glance("every/1m/beat"))
	}
	tick := func(age time.Duration, source string) *groundhog.LastTick {
		return &groundhog.LastTick{At: taken.Add(-age), Source: source}
	}
	elsewhere := func(age time.Duration) func(*groundhog.Status) {
		return func(st *groundhog.Status) { st.LastTick = tick(age, "runner") }
	}
	outside := func(age time.Duration) func(*groundhog.Status) {
		return func(st *groundhog.Status) { st.LastTick = tick(age, "standalone") }
	}
	return []core.InstanceView{
		build("running-fresh", core.ClassSupported, tickedHere(12*time.Second)),
		build("running-late", core.ClassSupported, tickedHere(2*time.Minute)),
		build("running-quiet", core.ClassSupported, tickedHere(4*time.Minute)),
		build("outside-fresh", core.ClassSupported, outside(40*time.Second)),
		build("outside-late", core.ClassSupported, outside(2*time.Minute+30*time.Second)),
		build("elsewhere-quiet", core.ClassSupported, elsewhere(20*time.Minute)),
		build("stopped-old-tick", core.ClassSupported, outside(25*time.Minute)),
		build("paused", core.ClassSupported, func(st *groundhog.Status) { tickedHere(20 * time.Second)(st); st.Paused = true }),
		build("never-ticked", core.ClassSupported, func(st *groundhog.Status) {}),
	}
}
