package opener

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestCommandIsAFixedFileManagerWithTheFolderAsArgument(t *testing.T) {
	dir := filepath.Join(string(filepath.Separator), "work", "-rf", "x y", ".groundhog")
	for goos, want := range map[string]string{"darwin": "open", "linux": "xdg-open"} {
		name, args, err := Command(goos, dir)
		if err != nil || name != want || args[len(args)-1] != dir {
			t.Errorf("%s: %q %q %v", goos, name, args, err)
		}
	}
	if _, args, _ := Command("darwin", dir); args[0] != "--" {
		t.Errorf("darwin options end: %q", args)
	}
	if _, _, err := Command("plan9", dir); err == nil {
		t.Error("unknown platform accepted")
	}
	if _, _, err := Command("linux", "-relative"); err == nil {
		t.Error("relative path accepted")
	}
}

func TestOpenRefusesWhatIsNotARealFolder(t *testing.T) {
	d := t.TempDir()
	if err := Open(filepath.Join(d, "missing")); err == nil || !strings.Contains(err.Error(), "gone") {
		t.Errorf("%v", err)
	}
	f := filepath.Join(d, "f")
	if err := writeFile(f); err != nil {
		t.Fatal(err)
	}
	if err := Open(f); err == nil || !strings.Contains(err.Error(), "no longer a folder") {
		t.Errorf("%v", err)
	}
}

func writeFile(p string) error { return os.WriteFile(p, []byte("x"), 0o644) }
