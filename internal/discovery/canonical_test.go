package discovery

import (
	"context"
	"encoding/json"
	"io/fs"
	"os"
	"os/exec"
	"path/filepath"
	"reflect"
	"strings"
	"testing"
)

// foldFS is a case-insensitive, case-preserving view of a memFS, with
// optional prefix aliases standing in for symlinked directories. Every lookup
// accepts any spelling; ReadDir reports the stored one.
type foldFS struct {
	*memFS
	alias map[string]string // typed prefix -> stored prefix
}

// resolve maps a typed path to the stored spelling, following aliases.
func (f *foldFS) resolve(name string) string {
	for from, to := range f.alias {
		if name == from || strings.HasPrefix(name, from+"/") {
			name = to + strings.TrimPrefix(name, from)
			break
		}
	}
	cur := "/"
	for _, part := range strings.Split(strings.Trim(name, "/"), "/") {
		if part == "" {
			continue
		}
		next := part
		dir := f.key(cur)
		if dir == "" {
			dir = "."
		}
		if es, err := f.memFS.m.ReadDir(dir); err == nil {
			for _, e := range es {
				if strings.EqualFold(e.Name(), part) {
					next = e.Name()
				}
			}
		}
		cur = filepath.ToSlash(filepath.Join(cur, next))
	}
	return cur
}

func (f *foldFS) ReadDir(name string) ([]fs.DirEntry, error) { return f.memFS.ReadDir(f.resolve(name)) }
func (f *foldFS) Lstat(name string) (fs.FileInfo, error)     { return f.memFS.Lstat(f.resolve(name)) }
func (f *foldFS) Stat(name string) (fs.FileInfo, error)      { return f.memFS.Stat(f.resolve(name)) }

// Canonical resolves aliases (symlinks), then spells like the real OS does.
func (f *foldFS) Canonical(name string) (string, error) {
	r := f.resolve(name)
	if _, err := f.memFS.Lstat(r); err != nil {
		return "", err
	}
	return Spell(f, r), nil
}

func TestCaseInsensitiveFSGivesOneInstancePerDirectory(t *testing.T) {
	f := &foldFS{memFS: memTree()}
	for _, tc := range []struct {
		root    string
		include []string
	}{
		{"/r", nil},
		{"/R", nil},
		{"/R", []string{"/r/B/C", "A", "/R/b/c/.GROUNDHOG"}},
		{"/r", []string{"/R/B/C"}},
	} {
		r, err := Scan(context.Background(), f, tc.root, Options{Include: tc.include})
		if err != nil {
			t.Fatal(err)
		}
		var paths []string
		for _, i := range r.Instances {
			paths = append(paths, i.Path)
		}
		if want := []string{"/r/a/.groundhog", "/r/b/c/.groundhog"}; !reflect.DeepEqual(paths, want) || r.Root != "/r" {
			t.Fatalf("root %q include %q: root %q paths %q", tc.root, tc.include, r.Root, paths)
		}
	}
}

func TestExcludesMatchWhateverCaseOrPrefixIsTyped(t *testing.T) {
	f := &foldFS{memFS: memTree(), alias: map[string]string{"/link": "/r"}}
	for _, ex := range []string{"/link/a", "/R/A", "/link/B/c", "/r/a/"} {
		r, err := Scan(context.Background(), f, "/r", Options{Exclude: []string{ex}})
		if err != nil {
			t.Fatal(err)
		}
		if len(r.Instances) != 1 {
			t.Fatalf("exclude %q: %+v", ex, r.Instances)
		}
	}
	// Exclusion is by directory, not by lookalike.
	r, _ := Scan(context.Background(), f, "/r", Options{Exclude: []string{"/link/z"}})
	if len(r.Instances) != 2 {
		t.Fatalf("%+v", r.Instances)
	}
}

func TestSpellKeepsExactNamesOnCaseSensitiveListing(t *testing.T) {
	m := memTree()
	// "R" and "r" exist side by side, so exact names must win.
	m.m["R/q/file"] = m.m["r/z/file"]
	if got := Spell(m, "/R/q"); got != "/R/q" {
		t.Fatalf("got %q", got)
	}
	if got := Spell(m, "/r/z"); got != "/r/z" {
		t.Fatalf("got %q", got)
	}
	// Unlistable or absent components stay as typed.
	m.failRead = map[string]error{"/r": fs.ErrPermission}
	if got := Spell(m, "/r/Z"); got != "/r/Z" {
		t.Fatalf("got %q", got)
	}
}

func TestRealFSExcludeThroughSymlinkedPrefix(t *testing.T) {
	base := canon(t, t.TempDir())
	real := filepath.Join(base, "real")
	mkInstance(t, filepath.Join(real, "keep"))
	mkInstance(t, filepath.Join(real, "skip"))
	link := filepath.Join(base, "link")
	if err := os.Symlink(real, link); err != nil {
		t.Fatal(err)
	}
	r := scan(t, real, Options{Exclude: []string{filepath.Join(link, "skip")}})
	if got := displays(r); !reflect.DeepEqual(got, []string{"keep/.groundhog"}) {
		t.Fatalf("%q", got)
	}
}

// swapCase returns p with the case of its last letter-bearing components flipped.
func swapCase(p string) string {
	return strings.Map(func(r rune) rune {
		if r >= 'a' && r <= 'z' {
			return r - 32
		}
		if r >= 'A' && r <= 'Z' {
			return r + 32
		}
		return r
	}, p)
}

// caseInsensitiveHost reports whether dir's file system treats names that
// differ only in case as one.
func caseInsensitiveHost(t *testing.T, dir string) bool {
	t.Helper()
	probe := filepath.Join(dir, "CaseProbe")
	if err := os.Mkdir(probe, 0o755); err != nil {
		t.Fatal(err)
	}
	_, err := os.Lstat(filepath.Join(dir, "caseprobe"))
	return err == nil
}

func TestRealFSCaseTypedDifferentlyGivesTheListedSpelling(t *testing.T) {
	base := canon(t, t.TempDir())
	if !caseInsensitiveHost(t, base) {
		t.Skip("case-sensitive file system: no second spelling exists")
	}
	root := filepath.Join(base, "MixedCase")
	inst := mkInstance(t, filepath.Join(root, "Proj"))
	typed := filepath.Join(base, "mixedCASE")
	r := scan(t, typed, Options{Include: []string{filepath.Join(strings.ToLower(root), "PROJ")}})
	if r.Root != root || len(r.Instances) != 1 || r.Instances[0].Path != inst {
		t.Fatalf("root %q instances %+v want %q", r.Root, r.Instances, inst)
	}
	r = scan(t, root, Options{Exclude: []string{swapCase(filepath.Join(root, "Proj"))}})
	if len(r.Instances) != 0 {
		t.Fatalf("exclude in other case missed: %+v", r.Instances)
	}
}

// TestInstancePathEqualsGroundhogStatusInstance compares Instance.Path with
// the `instance` field of a real Groundhog's `status --json`. It needs
// EASYHOG_GROUNDHOG_SH to name a groundhog.sh and only runs `status --json`
// (read-only) on a copy inside a temporary instance.
func TestInstancePathEqualsGroundhogStatusInstance(t *testing.T) {
	script := os.Getenv("EASYHOG_GROUNDHOG_SH")
	if script == "" {
		t.Skip("EASYHOG_GROUNDHOG_SH not set")
	}
	src, err := os.ReadFile(script)
	if err != nil {
		t.Fatal(err)
	}
	base := canon(t, t.TempDir())
	host := filepath.Join(base, "MixedCase", "Host")
	inst := filepath.Join(host, InstanceDir)
	for _, sub := range []string{"schedule", "out", "fired"} {
		if err := os.MkdirAll(filepath.Join(inst, sub), 0o755); err != nil {
			t.Fatal(err)
		}
	}
	if err := os.WriteFile(filepath.Join(inst, "groundhog.sh"), src, 0o755); err != nil {
		t.Fatal(err)
	}
	spellings := map[string]string{"as listed": host}
	if err := os.Symlink(filepath.Join(base, "MixedCase"), filepath.Join(base, "link")); err == nil {
		spellings["symlinked prefix"] = filepath.Join(base, "link", "Host")
	}
	if caseInsensitiveHost(t, base) {
		spellings["other case"] = swapCase(host[len(base):])
		spellings["other case"] = base + spellings["other case"]
	}
	for name, typed := range spellings {
		r := scan(t, typed, Options{})
		if len(r.Instances) != 1 {
			t.Fatalf("%s: %+v", name, r)
		}
		// Groundhog's `instance` is the canonical .groundhog directory itself.
		cmd := exec.Command("bash", filepath.Join(typed, InstanceDir, "groundhog.sh"), "status", "--json")
		cmd.Dir = typed
		out, err := cmd.Output()
		if err != nil {
			t.Fatalf("%s: status --json: %v", name, err)
		}
		var st struct {
			Instance string `json:"instance"`
		}
		if err := json.Unmarshal(out, &st); err != nil {
			t.Fatalf("%s: %v\n%s", name, err, out)
		}
		if got := r.Instances[0].Path; got != st.Instance {
			t.Fatalf("%s: Instance.Path %q, status instance %q", name, got, st.Instance)
		}
	}
}

func TestExcludeNamingASymlinkDoesNotExcludeItsTarget(t *testing.T) {
	base := canon(t, t.TempDir())
	root := filepath.Join(base, "root")
	mkInstance(t, filepath.Join(root, "target"))
	if err := os.Symlink(filepath.Join(root, "target"), filepath.Join(root, "alias")); err != nil {
		t.Fatal(err)
	}
	r := scan(t, root, Options{Exclude: []string{filepath.Join(root, "alias")}})
	if got := displays(r); !reflect.DeepEqual(got, []string{"target/.groundhog"}) {
		t.Fatalf("%q", got)
	}
}
