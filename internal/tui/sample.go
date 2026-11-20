package tui

import "strings"

// Synthetic data for tests and terminal captures. Nothing here reads a real
// instance, and no product code path calls it.

// SampleData returns a synthetic snapshot: "full", "empty", "error" or
// "stale" (an error with the last good tree). Unknown names give "full".
func SampleData(name string) Data {
	switch name {
	case "empty":
		return Data{Root: "/work/projects", Note: "Nothing under this directory contains a Groundhog instance."}
	case "error":
		return Data{Root: "/work/projects", Err: "scan /work/projects: permission denied"}
	case "stale":
		d := SampleData("full")
		d.Err = "scan /work/projects: permission denied"
		return d
	}
	control := []Action{ActionRun, ActionOpenFolder}
	running := []Action{ActionStop, ActionOpenFolder}
	return Data{Root: "/work/projects", Nodes: []Node{
		{Key: "alpha", Label: "alpha", State: StateRunning, Detail: "2 schedule items", Actions: running,
			Preview: "Instance alpha\nThis session ticks it every 1m00s\n\nNext firing: tomorrow 09:00\n\n" + strings.Repeat("Payload preview line. ", 12),
			Children: []Node{
				{Key: "alpha/daily", Label: "daily-report", State: StatePending, Detail: "every day 09:00", Preview: "# Daily report\n\nSummarise yesterday's changes.\n\n- collect\n- summarise\n- deliver"},
				{Key: "alpha/weekly", Label: "weekly-review", State: StatePaused, Detail: "Mondays 10:00", Preview: "# Weekly review\n\nPaused item."},
			}},
		{Key: "beta", Label: "tools/beta", State: StateStopped, Detail: "not ticked", Actions: control,
			Preview: "Instance tools/beta\nNot ticked. Run ticks it from this session.",
			Children: []Node{
				{Key: "beta/once", Label: "one-shot-2026-12-01", State: StatePending, Detail: "once, 2026-12-01 08:00", Preview: "One-shot payload."},
			}},
		{Key: "gamma", Label: "legacy/gamma", State: StateUnsupported, Detail: "older Groundhog",
			Preview: "Instance legacy/gamma\nThis copy speaks an older protocol. It remains inspectable."},
		{Key: "delta", Label: "shared/delta", State: StateExternal, Detail: "ticked from outside Easyhog", Actions: control,
			Preview: "Instance shared/delta\nSomething outside Easyhog ticks it. Run ticks it from here too."},
		{Key: "epsilon", Label: "ops/epsilon", State: StateFailed, Detail: "1 delivery failed", Actions: control,
			Preview: "Instance ops/epsilon\nA run failed: it exited with status 3.\n\nControl characters are inert: \x1b[2J\tdone.",
			Children: []Node{
				{Key: "epsilon/sync", Label: "sync-notes", State: StateFailed, Detail: "exit status 3", Preview: "Delivery /work/projects/ops/epsilon/out/sync-notes.md\nexit status 3"},
			}},
	}}
}

// HiddenSampleData is SampleData("full") with the unsupported instance marked
// hidden, plus a malformed one, as the adapter marks them by class.
func HiddenSampleData() Data {
	d := SampleData("full")
	for i := range d.Nodes {
		if d.Nodes[i].Key == "gamma" {
			d.Nodes[i].Hidden = true
		}
	}
	d.Nodes = append(d.Nodes, Node{Key: "zeta", Label: "host", State: StateUnsupported, Detail: "malformed, not run", Hidden: true,
		Preview: "Instance host\nMalformed: script missing."})
	return d
}
