package core

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"io/fs"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/zealtv/easyhog/internal/discovery"
	"github.com/zealtv/easyhog/internal/runner"
)

// The refresher against the pinned upstream script, on temporary instances,
// with read-only commands only (`capabilities --json` and `status --json`).
// It never runs `tick`, `run` or any control command.
const (
	pinnedScript = "../groundhog/testdata/upstream-9317d4e/groundhog.sh"
	pinnedSHA256 = "5a961d101ffc6d9186b836bd1f140c4c78e0b92770d682858a5a799d0cc562c2"
)

func listTree(t *testing.T, dir string) string {
	t.Helper()
	var lines []string
	err := filepath.WalkDir(dir, func(p string, d fs.DirEntry, err error) error {
		if err != nil {
			return err
		}
		info, err := d.Info()
		if err != nil {
			return err
		}
		lines = append(lines, fmt.Sprintf("%s %s %s %d", p, info.Mode(), info.ModTime(), info.Size()))
		return nil
	})
	if err != nil {
		t.Fatal(err)
	}
	return strings.Join(lines, "\n")
}

func TestRefresherAgainstThePinnedScript(t *testing.T) {
	if _, err := exec.LookPath("bash"); err != nil {
		t.Skip("bash not found")
	}
	script, err := os.ReadFile(pinnedScript)
	if err != nil {
		t.Fatal(err)
	}
	if sum := sha256.Sum256(script); hex.EncodeToString(sum[:]) != pinnedSHA256 {
		t.Fatalf("%s no longer matches the pinned digest; update the pin deliberately", pinnedScript)
	}
	root := tempRoot(t)
	var insts []string
	for _, rel := range []string{"one", "two"} {
		inst := mkInstance(t, root, rel)
		if err := os.WriteFile(filepath.Join(inst, "groundhog.sh"), script, 0o755); err != nil {
			t.Fatal(err)
		}
		if err := os.MkdirAll(filepath.Join(inst, "schedule", "daily", "standup"), 0o755); err != nil {
			t.Fatal(err)
		}
		insts = append(insts, inst)
	}
	before := listTree(t, root)

	r, err := NewRefresher(Config{Root: root, FS: discovery.OS{}, Exec: runner.Process{},
		SelectSettle: 20 * time.Millisecond, Interval: time.Hour, DiscoverEvery: time.Hour})
	if err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan error, 1)
	if err := r.Rescan(ctx); err != nil {
		t.Fatal(err)
	}
	go func() { done <- r.Run(ctx) }()
	defer func() {
		cancel()
		<-done
	}()
	v := r.View()
	for _, iv := range v.Instances {
		if iv.Class != ClassNotInspected || len(iv.Files.Items()) != 1 {
			t.Fatalf("%s before any selection: class %v, %d items", iv.Instance.Display, iv.Class, len(iv.Files.Items()))
		}
	}

	// Selecting one instance reads that one only.
	r.Select(insts[0])
	eventually(t, "the selected instance to be read", func() bool {
		iv, _ := r.View().Find(insts[0])
		return iv.Inspection.Last != nil
	})
	iv, _ := r.View().Find(insts[0])
	if iv.Inspection.Last.Status.Instance != insts[0] || iv.Inspection.Support == nil || !iv.Inspection.Support.Inspectable {
		t.Fatalf("typed status/support wrong: %+v", iv.Inspection)
	}
	if len(iv.Inspection.Last.Status.Schedule) != 1 || iv.Inspection.Last.Status.Schedule[0].Path != "schedule/daily/standup" {
		t.Fatalf("schedule %+v", iv.Inspection.Last.Status.Schedule)
	}
	if iv.Health != HealthOK || (iv.Class != ClassSupported && iv.Class != ClassUnsupported) {
		t.Errorf("class %v health %v", iv.Class, iv.Health)
	}
	never(t, "the other instance was read without being selected", func() bool {
		o, _ := r.View().Find(insts[1])
		return o.Inspection.Support != nil || o.Inspection.Last != nil
	})

	// An explicit Refresh reads it too.
	r.Refresh()
	eventually(t, "the other instance to be read by an explicit refresh", func() bool {
		o, _ := r.View().Find(insts[1])
		return o.Inspection.Last != nil
	})
	if after := listTree(t, root); after != before {
		t.Fatalf("reading changed the tree:\nbefore:\n%s\nafter:\n%s", before, after)
	}
}
