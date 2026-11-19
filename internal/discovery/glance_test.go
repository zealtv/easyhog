package discovery

import (
	"context"
	"os"
	"path/filepath"
	"reflect"
	"testing"
)

func writeTree(t *testing.T, inst string, dirs []string, files map[string]string) {
	t.Helper()
	for _, d := range dirs {
		if err := os.MkdirAll(filepath.Join(inst, d), 0o755); err != nil {
			t.Fatal(err)
		}
	}
	for name, body := range files {
		if err := os.WriteFile(filepath.Join(inst, name), []byte(body), 0o755); err != nil {
			t.Fatal(err)
		}
	}
}

func TestGlanceListsWithoutReadingOrFollowing(t *testing.T) {
	root, err := filepath.EvalSymlinks(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	inst := filepath.Join(root, ".groundhog")
	writeTree(t, inst, []string{
		"schedule/daily/standup", "schedule/daily/review.paused", "schedule/once/2026-12-01/ship",
		"schedule/weekly.paused/mon/plan", "out", "fired", "runtime",
	}, map[string]string{
		"groundhog.sh":                  "#!/bin/sh\n",
		"schedule/daily/standup/prompt": "payload text the glance must not read",
		"out/2026-11-15-standup":        "x",
		"runtime/state.json":            "{not json",
	})
	outside := t.TempDir()
	if err := os.Symlink(outside, filepath.Join(inst, "schedule", "linked")); err != nil {
		t.Fatal(err)
	}

	g, err := GlanceInstance(context.Background(), OS{}, inst)
	if err != nil {
		t.Fatal(err)
	}
	if !g.Script.Present || !g.Runtime || len(g.Problems) != 0 {
		t.Fatalf("script/runtime/problems: %+v", g)
	}
	var got []string
	for _, d := range g.Schedule {
		got = append(got, d.Path)
	}
	want := []string{"daily", "daily/review.paused", "daily/standup", "once", "once/2026-12-01", "once/2026-12-01/ship",
		"weekly.paused", "weekly.paused/mon", "weekly.paused/mon/plan"}
	if !reflect.DeepEqual(got, want) {
		t.Fatalf("schedule %q\nwant %q", got, want)
	}
	byPath := map[string]ScheduleDir{}
	for _, d := range g.Schedule {
		byPath[d.Path] = d
	}
	if !byPath["daily/standup"].Leaf || byPath["daily"].Leaf || !byPath["daily/review.paused"].Paused ||
		!byPath["weekly.paused/mon/plan"].Paused || byPath["daily/standup"].Paused {
		t.Fatalf("leaf/paused flags wrong: %+v", byPath)
	}
	if !reflect.DeepEqual(g.Out, []string{"2026-11-15-standup"}) {
		t.Fatalf("out %q", g.Out)
	}
	if n := len(g.Items()); n != 4 {
		t.Fatalf("%d items; want 4 leaves", n)
	}
	again, _ := GlanceInstance(context.Background(), OS{}, inst)
	if !g.Equal(again) {
		t.Fatal("two glances at an unchanged instance should be equal")
	}
	writeTree(t, inst, []string{"schedule/daily/extra"}, nil)
	changed, _ := GlanceInstance(context.Background(), OS{}, inst)
	if g.Equal(changed) {
		t.Fatal("a new item should change the glance")
	}
}

func TestGlanceOfAMissingInstanceReportsProblemsNotPanics(t *testing.T) {
	g, err := GlanceInstance(context.Background(), OS{}, filepath.Join(t.TempDir(), ".groundhog"))
	if err != nil {
		t.Fatal(err)
	}
	if g.Script.Present || g.Runtime || len(g.Problems) != 2 {
		t.Fatalf("%+v", g)
	}
}

func TestGlanceIsBoundedAndCancellable(t *testing.T) {
	inst := filepath.Join(t.TempDir(), ".groundhog")
	deep := "schedule"
	for i := 0; i < glanceMaxDepth+3; i++ {
		deep = filepath.Join(deep, "d")
	}
	writeTree(t, inst, []string{deep, "out"}, nil)
	g, _ := GlanceInstance(context.Background(), OS{}, inst)
	if !g.ScheduleTruncated || len(g.Schedule) != glanceMaxDepth {
		t.Fatalf("depth bound: truncated=%v n=%d", g.ScheduleTruncated, len(g.Schedule))
	}
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	if _, err := GlanceInstance(ctx, OS{}, inst); err == nil {
		t.Fatal("a cancelled context should be reported")
	}
}
