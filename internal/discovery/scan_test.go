package discovery

import (
	"context"
	"crypto/sha256"
	"errors"
	"io/fs"
	"os"
	"path/filepath"
	"reflect"
	"sort"
	"strings"
	"testing"
	"testing/fstest"
)

// mkInstance builds a well-formed synthetic instance whose script would leave
// a marker file if anything ever ran it.
func mkInstance(t *testing.T, parent string) string {
	t.Helper()
	dir := filepath.Join(parent, InstanceDir)
	for _, sub := range []string{"schedule", "out", "fired"} {
		if err := os.MkdirAll(filepath.Join(dir, sub), 0o755); err != nil {
			t.Fatal(err)
		}
	}
	script := "#!/bin/sh\ntouch \"$(dirname \"$0\")/EXECUTED\"\n"
	if err := os.WriteFile(filepath.Join(dir, "groundhog.sh"), []byte(script), 0o755); err != nil {
		t.Fatal(err)
	}
	return dir
}

func canon(t *testing.T, p string) string {
	t.Helper()
	r, err := filepath.EvalSymlinks(p)
	if err != nil {
		t.Fatal(err)
	}
	return r
}

func displays(r Result) []string {
	var out []string
	for _, i := range r.Instances {
		out = append(out, filepath.ToSlash(i.Display))
	}
	return out
}

func scan(t *testing.T, root string, opts Options) Result {
	t.Helper()
	r, err := Scan(context.Background(), OS{}, root, opts)
	if err != nil {
		t.Fatal(err)
	}
	return r
}

func TestNestedInstancesRootAndSpaces(t *testing.T) {
	root := canon(t, t.TempDir())
	mkInstance(t, root)
	mkInstance(t, filepath.Join(root, "my repo"))
	mkInstance(t, filepath.Join(root, "a", "b c", "deep"))
	r := scan(t, root, Options{})
	want := []string{".groundhog", "a/b c/deep/.groundhog", "my repo/.groundhog"}
	if got := displays(r); !reflect.DeepEqual(got, want) {
		t.Fatalf("got %q want %q", got, want)
	}
	for _, i := range r.Instances {
		if len(i.Malformed) != 0 || !filepath.IsAbs(i.Path) {
			t.Fatalf("%+v", i)
		}
	}
}

func TestRootThatIsAnInstance(t *testing.T) {
	parent := canon(t, t.TempDir())
	inst := mkInstance(t, parent)
	r := scan(t, inst, Options{})
	if len(r.Instances) != 1 || r.Instances[0].Path != inst || r.Instances[0].Display != "." {
		t.Fatalf("%+v", r)
	}
}

func TestSameNameDifferentParentsAreDistinct(t *testing.T) {
	root := canon(t, t.TempDir())
	mkInstance(t, filepath.Join(root, "x", "app"))
	mkInstance(t, filepath.Join(root, "y", "app"))
	r := scan(t, root, Options{})
	if len(r.Instances) != 2 || r.Instances[0].Path == r.Instances[1].Path {
		t.Fatalf("%+v", r.Instances)
	}
}

func TestSymlinkedRootAndOverlappingIncludeAppearOnce(t *testing.T) {
	base := canon(t, t.TempDir())
	real := filepath.Join(base, "real")
	inst := mkInstance(t, filepath.Join(real, "proj"))
	link := filepath.Join(base, "link")
	if err := os.Symlink(real, link); err != nil {
		t.Fatal(err)
	}
	r := scan(t, link, Options{Include: []string{"proj", filepath.Join(real, "proj"), "proj/.groundhog"}})
	if r.Root != real || len(r.Instances) != 1 || r.Instances[0].Path != inst {
		t.Fatalf("%+v", r)
	}
}

func TestDirectorySymlinksAreNotFollowed(t *testing.T) {
	base := canon(t, t.TempDir())
	mkInstance(t, filepath.Join(base, "outside"))
	root := filepath.Join(base, "root")
	if err := os.MkdirAll(root, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink(filepath.Join(base, "outside"), filepath.Join(root, "linked")); err != nil {
		t.Fatal(err)
	}
	// A symlinked .groundhog is not an instance either.
	if err := os.MkdirAll(filepath.Join(root, "p"), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink(filepath.Join(base, "outside", InstanceDir), filepath.Join(root, "p", InstanceDir)); err != nil {
		t.Fatal(err)
	}
	if r := scan(t, root, Options{}); len(r.Instances) != 0 || len(r.Diagnostics) != 0 {
		t.Fatalf("%+v", r)
	}
}

func TestPruningAndExplicitControls(t *testing.T) {
	root := canon(t, t.TempDir())
	for _, d := range []string{"node_modules/pkg", "vendor/lib", ".git/hooks", ".venv/x", "templates/starter", "src/templates/s", "keep", "skipme/inner"} {
		mkInstance(t, filepath.Join(root, d))
	}
	if got, want := displays(scan(t, root, Options{Exclude: []string{"skipme"}})), []string{"keep/.groundhog"}; !reflect.DeepEqual(got, want) {
		t.Fatalf("got %q want %q", got, want)
	}
	// Path-form exclude is root-relative and exact.
	opts := Options{Exclude: []string{"keep"}, Include: []string{"vendor/lib", "templates/starter"}}
	got := displays(scan(t, root, opts))
	want := []string{"skipme/inner/.groundhog", "templates/starter/.groundhog", "vendor/lib/.groundhog"}
	if !reflect.DeepEqual(got, want) {
		t.Fatalf("got %q want %q", got, want)
	}
	// Path-form entries are cleaned, so "src/../keep" excludes root/keep.
	if got := displays(scan(t, root, Options{Exclude: []string{"src/../keep", "skipme/inner"}})); len(got) != 0 {
		t.Fatalf("got %q", got)
	}
	if got := displays(scan(t, root, Options{NoDefaultPrune: true, Exclude: []string{"skipme"}})); len(got) != 7 {
		t.Fatalf("no-prune got %q", got)
	}
}

func TestDoesNotDescendIntoInstance(t *testing.T) {
	root := canon(t, t.TempDir())
	inst := mkInstance(t, root)
	mkInstance(t, filepath.Join(inst, "out", "delivery"))
	mkInstance(t, filepath.Join(inst, "schedule", "item"))
	if r := scan(t, root, Options{}); len(r.Instances) != 1 {
		t.Fatalf("%q", displays(r))
	}
}

func TestMalformedInstancesAreReportedNotDropped(t *testing.T) {
	root := canon(t, t.TempDir())
	// No script, missing fired/.
	a := filepath.Join(root, "a", InstanceDir)
	for _, s := range []string{"schedule", "out"} {
		os.MkdirAll(filepath.Join(a, s), 0o755)
	}
	// Empty .groundhog.
	os.MkdirAll(filepath.Join(root, "b", InstanceDir), 0o755)
	// Script is a directory.
	c := mkInstance(t, filepath.Join(root, "c"))
	os.Remove(filepath.Join(c, "groundhog.sh"))
	os.Mkdir(filepath.Join(c, "groundhog.sh"), 0o755)
	// Non-executable script, and schedule is a file.
	d := mkInstance(t, filepath.Join(root, "d"))
	os.Chmod(filepath.Join(d, "groundhog.sh"), 0o644)
	os.Remove(filepath.Join(d, "schedule"))
	os.WriteFile(filepath.Join(d, "schedule"), nil, 0o644)
	mkInstance(t, filepath.Join(root, "ok"))

	r := scan(t, root, Options{})
	if len(r.Instances) != 5 || len(r.Diagnostics) != 0 {
		t.Fatalf("%+v", r)
	}
	by := map[string][]string{}
	for _, i := range r.Instances {
		by[filepath.ToSlash(filepath.Dir(i.Display))] = i.Malformed
	}
	join := func(k string) string { return strings.Join(by[k], "; ") }
	if !strings.Contains(join("a"), "groundhog.sh missing") || !strings.Contains(join("a"), "fired/ missing") || strings.Contains(join("a"), "out") {
		t.Fatalf("a: %q", join("a"))
	}
	if len(by["b"]) != 4 || !strings.Contains(join("c"), "not a regular file") || !strings.Contains(join("d"), "schedule is not a directory") || len(by["ok"]) != 0 {
		t.Fatalf("%v", by)
	}
	if !strings.Contains(join("d"), "not executable") {
		t.Fatalf("d: %q", join("d"))
	}
}

func TestUnreadableDirectoryIsADiagnosticAndHealthyOnesSurvive(t *testing.T) {
	if os.Geteuid() == 0 {
		t.Skip("permission bits not enforced")
	}
	root := canon(t, t.TempDir())
	mkInstance(t, filepath.Join(root, "good"))
	bad := filepath.Join(root, "locked dir")
	os.MkdirAll(bad, 0o755)
	if err := os.Chmod(bad, 0); err != nil {
		t.Fatal(err)
	}
	defer os.Chmod(bad, 0o755)
	r := scan(t, root, Options{})
	if len(r.Instances) != 1 || len(r.Diagnostics) != 1 || r.Diagnostics[0].Display != "locked dir" || !errors.Is(r.Diagnostics[0].Err, fs.ErrPermission) {
		t.Fatalf("%+v", r)
	}
}

func TestMissingIncludeIsADiagnostic(t *testing.T) {
	root := canon(t, t.TempDir())
	mkInstance(t, root)
	r := scan(t, root, Options{Include: []string{"nope"}})
	if len(r.Instances) != 1 || len(r.Diagnostics) != 1 || !errors.Is(r.Diagnostics[0].Err, fs.ErrNotExist) {
		t.Fatalf("%+v", r)
	}
}

func TestRootErrors(t *testing.T) {
	root := t.TempDir()
	file := filepath.Join(root, "f")
	os.WriteFile(file, nil, 0o644)
	if _, err := Scan(context.Background(), OS{}, file, Options{}); err == nil {
		t.Fatal("accepted file")
	}
	if _, err := Scan(context.Background(), OS{}, filepath.Join(root, "missing"), Options{}); !errors.Is(err, fs.ErrNotExist) {
		t.Fatal(err)
	}
}

func TestResultsSortStablyByRelativePath(t *testing.T) {
	root := canon(t, t.TempDir())
	for _, n := range []string{"z", "a b", "m", "a", "B"} {
		mkInstance(t, filepath.Join(root, n))
	}
	got := displays(scan(t, root, Options{}))
	if !sort.StringsAreSorted(got) || len(got) != 5 {
		t.Fatalf("%q", got)
	}
	if again := displays(scan(t, root, Options{})); !reflect.DeepEqual(got, again) {
		t.Fatal("unstable")
	}
}

// memFS is an in-memory FS over fstest.MapFS keyed by absolute slash paths
// (without the leading slash), with hooks for failures and observation.
type memFS struct {
	m          fstest.MapFS
	failRead   map[string]error
	onReadDir  func(string)
	readDirs   []string
	statCalled []string
}

func (f *memFS) key(p string) string { return strings.TrimPrefix(filepath.ToSlash(p), "/") }
func (f *memFS) ReadDir(name string) ([]fs.DirEntry, error) {
	f.readDirs = append(f.readDirs, name)
	if f.onReadDir != nil {
		f.onReadDir(name)
	}
	if err := f.failRead[name]; err != nil {
		return nil, err
	}
	return f.m.ReadDir(f.key(name))
}
func (f *memFS) Lstat(name string) (fs.FileInfo, error) { return fs.Stat(f.m, f.key(name)) }
func (f *memFS) Stat(name string) (fs.FileInfo, error) {
	f.statCalled = append(f.statCalled, name)
	return fs.Stat(f.m, f.key(name))
}
func (f *memFS) Canonical(name string) (string, error) {
	if _, err := f.Lstat(name); err != nil {
		return "", err
	}
	return name, nil
}

func memTree() *memFS {
	m := fstest.MapFS{}
	for _, p := range []string{"r/a", "r/b/c"} {
		m[p+"/.groundhog/groundhog.sh"] = &fstest.MapFile{Mode: 0o755}
		for _, s := range []string{"schedule", "out", "fired"} {
			m[p+"/.groundhog/"+s+"/x"] = &fstest.MapFile{}
		}
	}
	m["r/z/file"] = &fstest.MapFile{}
	return &memFS{m: m}
}

func TestInjectedFSWithFailureAndNoScriptReads(t *testing.T) {
	f := memTree()
	f.failRead = map[string]error{"/r/b": fs.ErrPermission}
	r, err := Scan(context.Background(), f, "/r", Options{})
	if err != nil {
		t.Fatal(err)
	}
	if got := displays(r); !reflect.DeepEqual(got, []string{"a/.groundhog"}) {
		t.Fatalf("%q", got)
	}
	if len(r.Diagnostics) != 1 || r.Diagnostics[0].Display != "b" {
		t.Fatalf("%+v", r.Diagnostics)
	}
	for _, d := range f.readDirs {
		if strings.Contains(d, InstanceDir) {
			t.Fatalf("listed inside an instance: %s", d)
		}
	}
}

func TestCancellationBetweenDirectoryReads(t *testing.T) {
	pre, cancelPre := context.WithCancel(context.Background())
	cancelPre()
	f := memTree()
	if _, err := Scan(pre, f, "/r", Options{}); !errors.Is(err, context.Canceled) || len(f.readDirs) != 0 {
		t.Fatalf("pre-cancelled scan read %v: %v", f.readDirs, err)
	}
	ctx, cancel := context.WithCancel(context.Background())
	f = memTree()
	f.onReadDir = func(string) { cancel() } // cancel during the first read
	r, err := Scan(ctx, f, "/r", Options{})
	if !errors.Is(err, context.Canceled) {
		t.Fatal(err)
	}
	if len(f.readDirs) != 1 {
		t.Fatalf("kept reading after cancel: %v (%d instances)", f.readDirs, len(r.Instances))
	}
}

// snapshotTree records every path with mode, size, mtime and content hash.
func snapshotTree(t *testing.T, root string) map[string]string {
	t.Helper()
	out := map[string]string{}
	err := filepath.WalkDir(root, func(p string, d fs.DirEntry, err error) error {
		if err != nil {
			return err
		}
		info, err := os.Lstat(p)
		if err != nil {
			return err
		}
		sum := ""
		if info.Mode().IsRegular() {
			b, err := os.ReadFile(p)
			if err != nil {
				return err
			}
			sum = string(sha256.New().Sum(b))
		}
		out[p] = info.Mode().String() + "|" + info.ModTime().String() + "|" + sum
		return nil
	})
	if err != nil {
		t.Fatal(err)
	}
	return out
}

func TestScanLeavesTreeUnchangedAndExecutesNothing(t *testing.T) {
	root := canon(t, t.TempDir())
	mkInstance(t, root)
	mkInstance(t, filepath.Join(root, "p"))
	broken := filepath.Join(root, "q", InstanceDir)
	os.MkdirAll(broken, 0o755) // malformed: scan must not repair it
	before := snapshotTree(t, root)
	scan(t, root, Options{Include: []string{"p"}})
	if after := snapshotTree(t, root); !reflect.DeepEqual(before, after) {
		t.Fatal("scan changed the tree")
	}
	for _, p := range []string{root, filepath.Join(root, "p")} {
		if _, err := os.Stat(filepath.Join(p, InstanceDir, "EXECUTED")); err == nil {
			t.Fatal("a groundhog.sh ran")
		}
	}
}
