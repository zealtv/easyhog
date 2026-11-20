package payload

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func item(t *testing.T, files map[string][]byte) (inst string) {
	t.Helper()
	inst = t.TempDir()
	dir := filepath.Join(inst, "schedule", "daily", "nudge")
	if err := os.MkdirAll(dir, 0o755); err != nil {
		t.Fatal(err)
	}
	for name, data := range files {
		p := filepath.Join(dir, name)
		if err := os.MkdirAll(filepath.Dir(p), 0o755); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(p, data, 0o644); err != nil {
			t.Fatal(err)
		}
	}
	return inst
}

const rel = "schedule/daily/nudge"

func TestTextIsShownAndNothingRuns(t *testing.T) {
	// A file that would leave a mark if anything executed it.
	inst := item(t, map[string][]byte{
		"note.md":   []byte("# Nudge\n\nSay hello.\n"),
		"run.sh":    []byte("#!/bin/sh\ntouch " + filepath.Join(os.TempDir(), "payload-ran-marker") + "\n"),
		"sub/a.txt": []byte("inner\n"),
	})
	_ = os.Remove(filepath.Join(os.TempDir(), "payload-ran-marker"))
	if err := os.Chmod(filepath.Join(inst, rel, "run.sh"), 0o755); err != nil {
		t.Fatal(err)
	}
	got := Preview(OS{}, inst, rel)
	for _, want := range []string{"note.md", "    # Nudge", "    Say hello.", "run.sh", "    touch ", "sub/", "sub/a.txt", "    inner"} {
		if !strings.Contains(got, want) {
			t.Errorf("missing %q in:\n%s", want, got)
		}
	}
	if _, err := os.Stat(filepath.Join(os.TempDir(), "payload-ran-marker")); err == nil {
		t.Error("a payload script ran")
	}
}

func TestBinaryLargeAndEmptyFilesDegrade(t *testing.T) {
	inst := item(t, map[string][]byte{
		"a.bin":   {0x7f, 'E', 'L', 'F', 0, 1, 2},
		"b.big":   []byte(strings.Repeat("0123456789\n", 5000)),
		"c.empty": {},
		"d.latin": {'c', 'a', 'f', 0xe9, '\n'},
	})
	got := Preview(OS{}, inst, rel)
	for _, want := range []string{"a.bin  7 bytes", "(binary file, not shown)", "b.big  53.7 KiB", "shows the start only", "c.empty  0 bytes", "(empty)", "d.latin", "binary"} {
		if !strings.Contains(got, want) {
			t.Errorf("missing %q in:\n%s", want, got)
		}
	}
	if len(got) > MaxTotalBytes+4096 {
		t.Errorf("preview is %d bytes", len(got))
	}
	if strings.Count(got, "0123456789") > MaxLinesInFile {
		t.Error("a big file showed more than the line bound")
	}
}

func TestAMultiByteRuneCutAtTheBoundIsStillText(t *testing.T) {
	text := strings.Repeat("é", MaxFileBytes) // two bytes each: the bound falls mid-rune or between
	inst := item(t, map[string][]byte{"u.txt": []byte(text + "x")})
	got := Preview(OS{}, inst, rel)
	if strings.Contains(got, "binary") {
		t.Errorf("text with a cut rune read as binary:\n%.200s", got)
	}
}

func TestManyFilesAreBounded(t *testing.T) {
	files := map[string][]byte{}
	for i := 0; i < 40; i++ {
		files[strings.Repeat("f", 1)+string(rune('a'+i%26))+string(rune('a'+i/26))+".txt"] = []byte("x\n")
	}
	got := Preview(OS{}, item(t, files), rel)
	if n := strings.Count(got, ".txt  "); n != MaxFiles {
		t.Errorf("%d files shown, want %d", n, MaxFiles)
	}
	if !strings.Contains(got, "more not shown") {
		t.Errorf("no note about the rest:\n%s", got)
	}
}

func TestSymlinksAreNeverFollowed(t *testing.T) {
	inst := item(t, map[string][]byte{"real.txt": []byte("ok\n")})
	secret := filepath.Join(t.TempDir(), "secret.txt")
	if err := os.WriteFile(secret, []byte("TOP SECRET\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	dir := filepath.Join(inst, rel)
	if err := os.Symlink(secret, filepath.Join(dir, "link.txt")); err != nil {
		t.Skip("symlinks unavailable:", err)
	}
	if err := os.Symlink(filepath.Dir(secret), filepath.Join(dir, "linkdir")); err != nil {
		t.Fatal(err)
	}
	got := Preview(OS{}, inst, rel)
	if strings.Contains(got, "TOP SECRET") || strings.Contains(got, "secret.txt\n") {
		t.Errorf("followed a link:\n%s", got)
	}
	if !strings.Contains(got, "link.txt: not a regular file (symbolic link)") || !strings.Contains(got, "linkdir: not a regular file") {
		t.Errorf("links not reported:\n%s", got)
	}

	// A link on the item's own path stops the read.
	outside := t.TempDir()
	if err := os.MkdirAll(filepath.Join(outside, "nudge"), 0o755); err != nil {
		t.Fatal(err)
	}
	_ = os.WriteFile(filepath.Join(outside, "nudge", "x.txt"), []byte("outside\n"), 0o644)
	if err := os.Symlink(outside, filepath.Join(inst, "schedule", "weekly")); err != nil {
		t.Fatal(err)
	}
	got = Preview(OS{}, inst, "schedule/weekly/nudge")
	if strings.Contains(got, "outside") || !strings.Contains(got, "symbolic link lies on the item's path") {
		t.Errorf("%s", got)
	}
}

func TestPathsOutsideTheInstanceAreRefused(t *testing.T) {
	inst := item(t, map[string][]byte{"a.txt": []byte("a\n")})
	for _, bad := range []string{"", "../x", "schedule/../../x", "/etc", "schedule//daily", "."} {
		if got := Preview(OS{}, inst, bad); !strings.Contains(got, "not inside the instance") {
			t.Errorf("%q: %s", bad, got)
		}
	}
}

func TestVanishedUnreadableAndNonDirectoryItems(t *testing.T) {
	inst := item(t, map[string][]byte{"a.txt": []byte("a\n")})
	if got := Preview(OS{}, inst, "schedule/daily/missing"); !strings.Contains(got, "item is gone") {
		t.Errorf("vanished: %s", got)
	}
	if err := os.WriteFile(filepath.Join(inst, "schedule", "daily", "file"), []byte("x"), 0o644); err != nil {
		t.Fatal(err)
	}
	if got := Preview(OS{}, inst, "schedule/daily/file"); !strings.Contains(got, "no longer a directory") {
		t.Errorf("file: %s", got)
	}
	if os.Geteuid() != 0 {
		p := filepath.Join(inst, rel, "locked.txt")
		_ = os.WriteFile(p, []byte("hidden\n"), 0o000)
		got := Preview(OS{}, inst, rel)
		if strings.Contains(got, "hidden") || !strings.Contains(got, "locked.txt") || !strings.Contains(got, "permission denied") {
			t.Errorf("unreadable: %s", got)
		}
	}
	empty := filepath.Join(inst, "schedule", "daily", "void")
	_ = os.MkdirAll(empty, 0o755)
	if got := Preview(OS{}, inst, "schedule/daily/void"); !strings.Contains(got, "empty") {
		t.Errorf("empty: %s", got)
	}
}

// An item's own run is what firing it does: it comes first, says whether it
// runs, and is shown as text like any file. Nothing runs it.
func TestTheItemsRunComesFirstAndIsReadAsText(t *testing.T) {
	marker := filepath.Join(t.TempDir(), "ran")
	inst := item(t, map[string][]byte{
		"a-note.md": []byte("first by name\n"),
		"run":       []byte("#!/bin/sh\ntouch " + marker + "\n"),
	})
	if err := os.Chmod(filepath.Join(inst, rel, "run"), 0o755); err != nil {
		t.Fatal(err)
	}
	got := Preview(OS{}, inst, rel)
	if !strings.HasPrefix(got, "Runs when the item fires") || strings.Index(got, "run  ") > strings.Index(got, "a-note.md") ||
		!strings.Contains(got, "    touch "+marker) {
		t.Errorf("preview:\n%s", got)
	}
	if err := os.Chmod(filepath.Join(inst, rel, "run"), 0o644); err != nil {
		t.Fatal(err)
	}
	if got := Preview(OS{}, inst, rel); !strings.HasPrefix(got, "A file named run that is not executable") {
		t.Errorf("preview:\n%s", got)
	}
	if _, err := os.Stat(marker); err == nil {
		t.Error("the item's run ran")
	}
}

func TestADeliveryLogIsReadAsBoundedTextAndNothingElse(t *testing.T) {
	inst := t.TempDir()
	dir := filepath.Join(inst, "out", "report-1")
	if err := os.MkdirAll(dir, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(dir, ".groundhog-log"), []byte("hello\n"+strings.Repeat("x\n", 100)), 0o644); err != nil {
		t.Fatal(err)
	}
	got := Log(OS{}, inst, "out/report-1/.groundhog-log")
	if !strings.Contains(got, "    hello") || !strings.Contains(got, StartOnlyNote) {
		t.Errorf("log:\n%s", got)
	}
	for _, bad := range []string{"out/report-1/run", "schedule/x/.groundhog-log", "out/../../etc/.groundhog-log", "out/missing/.groundhog-log"} {
		if got := Log(OS{}, inst, bad); strings.Contains(got, "hello") || !strings.Contains(got, "not") {
			t.Errorf("%s: %q", bad, got)
		}
	}
	if err := os.Symlink(dir, filepath.Join(inst, "out", "alias")); err != nil {
		t.Fatal(err)
	}
	if got := Log(OS{}, inst, "out/alias/.groundhog-log"); strings.Contains(got, "hello") {
		t.Errorf("read through a symlink: %q", got)
	}
	both := ItemPreview(OS{}, inst, "out/report-1", []string{"out/report-1/.groundhog-log", "out/report-1/.groundhog-log", "out/report-1/.groundhog-log"})
	if strings.Count(both, "Output of a run") != MaxLogs {
		t.Errorf("logs shown: %d", strings.Count(both, "Output of a run"))
	}
}

// A run that is a symbolic link runs all the same (Groundhog follows it), so
// the preview says so, and still does not follow it.
func TestALinkedRunIsNamedAsRunningAndNotFollowed(t *testing.T) {
	inst := item(t, map[string][]byte{"note.md": []byte("hello\n")})
	target := filepath.Join(t.TempDir(), "real-run.sh")
	if err := os.WriteFile(target, []byte("#!/bin/sh\necho SECRET-BODY\n"), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink(target, filepath.Join(inst, rel, "run")); err != nil {
		t.Fatal(err)
	}
	got := Preview(OS{}, inst, rel)
	if !strings.HasPrefix(got, "run: a link named run. Groundhog starts what it points to when the item fires") ||
		strings.Contains(got, "SECRET-BODY") || !strings.Contains(got, "note.md") {
		t.Errorf("preview:\n%s", got)
	}
}
