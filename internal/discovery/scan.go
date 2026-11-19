package discovery

import (
	"context"
	"errors"
	"fmt"
	"io/fs"
	"os"
	"path/filepath"
	"sort"
	"strings"
)

// InstanceDir is the directory name that marks a Groundhog instance.
const InstanceDir = ".groundhog"

// FS is the whole filesystem surface discovery may touch. It offers metadata
// and directory listings only: no file reads, writes, creation or execution,
// so a scan cannot read payload contents or change the tree by construction.
// Paths are absolute operating-system paths.
type FS interface {
	ReadDir(name string) ([]fs.DirEntry, error)
	Lstat(name string) (fs.FileInfo, error)
	Stat(name string) (fs.FileInfo, error)
	// Canonical returns the absolute path of the existing directory or file
	// name, with symlinks resolved and every component spelled the way the
	// file system lists it (see Spell). Two spellings of one directory give one
	// result, whatever case or symlinked prefix the caller typed.
	Canonical(name string) (string, error)
}

// OS is the real filesystem.
type OS struct{}

func (OS) ReadDir(name string) ([]fs.DirEntry, error) { return os.ReadDir(name) }
func (OS) Lstat(name string) (fs.FileInfo, error)     { return os.Lstat(name) }
func (OS) Stat(name string) (fs.FileInfo, error)      { return os.Stat(name) }

// Canonical resolves symlinks, then takes each component's name from a
// listing of its parent, so a case-insensitive file system reports one spelling.
func (o OS) Canonical(name string) (string, error) {
	real, err := filepath.EvalSymlinks(name)
	if err != nil {
		return "", err
	}
	return Spell(o, real), nil
}

// Spell rewrites each component of the absolute, symlink-free path p to the
// name its parent directory lists, comparing exactly first and then without
// regard to case. Where the parent cannot be listed, or no single listed name
// matches, the component stays as given. It never follows symlinks and reads
// only directory listings and metadata. A case-sensitive file system always
// has the exact name, so Spell leaves its paths unchanged.
func Spell(fsys FS, p string) string {
	vol := filepath.VolumeName(p)
	rest := strings.TrimPrefix(p[len(vol):], string(filepath.Separator))
	if rest == "" {
		return p
	}
	cur := vol + string(filepath.Separator)
	for _, name := range strings.Split(rest, string(filepath.Separator)) {
		if name == "" {
			continue
		}
		cur = filepath.Join(cur, spellChild(fsys, cur, name))
	}
	return cur
}

// spellChild returns the listed spelling of name inside dir.
func spellChild(fsys FS, dir, name string) string {
	entries, _ := fsys.ReadDir(dir) // partial listings still help
	var folded []string
	for _, e := range entries {
		if e.Name() == name {
			return name
		}
		if strings.EqualFold(e.Name(), name) {
			folded = append(folded, e.Name())
		}
	}
	switch len(folded) {
	case 0:
		return name
	case 1:
		return folded[0]
	}
	// Several names differ only in case: pick the one that is the same file.
	want, err := fsys.Lstat(filepath.Join(dir, name))
	if err != nil {
		return name
	}
	for _, f := range folded {
		if got, err := fsys.Lstat(filepath.Join(dir, f)); err == nil && os.SameFile(want, got) {
			return f
		}
	}
	return name
}

// Options are the caller-supplied controls. The same rules apply to every
// host project; nothing here recognizes an application by name.
type Options struct {
	// Include lists extra directories to search, absolute or relative to the
	// root. An included directory is searched even when it lies under a
	// pruned tree or outside the root, and even when an Exclude entry would
	// otherwise cover it. Pruning and Exclude still apply beneath it.
	Include []string
	// Exclude lists directories to skip. An entry without a path separator
	// matches a directory by base name anywhere; an entry with a separator
	// matches one root-relative (or absolute) path exactly.
	Exclude []string
	// NoDefaultPrune disables the built-in pruned names.
	NoDefaultPrune bool
}

// defaultPruned names directories that rarely hold instances of their own:
// version-control metadata, dependency trees and template trees. They apply
// to every project alike. Options.Include reaches into them when a layout
// needs it.
var defaultPruned = map[string]bool{
	".git": true, ".hg": true, ".svn": true,
	"node_modules": true, "vendor": true, ".venv": true, "venv": true,
	"__pycache__": true, ".tox": true,
	"template": true, "templates": true, ".template": true, ".templates": true,
}

// Instance is one Groundhog instance directory found by inspection.
type Instance struct {
	// Path is the canonical (symlink-resolved, absolute, spelled as the file
	// system lists it) instance directory, and is the instance's identity.
	Path string
	// Display is Path relative to the canonical search root.
	Display string
	// Malformed lists what is missing or unusable. Empty means well-formed.
	// Entries are inspection facts about metadata, not runtime capabilities.
	Malformed []string
}

// Diagnostic records a path that could not be inspected.
type Diagnostic struct {
	Path    string // absolute path as attempted
	Display string // relative to the canonical root when possible
	Err     error
}

// Result is what a scan found. On cancellation it holds the partial result.
type Result struct {
	Root        string // canonical search root
	Instances   []Instance
	Diagnostics []Diagnostic
}

// Scan walks root, honouring ctx between directory reads. It resolves the
// root to its canonical path, reports each instance once, and turns
// unreadable directories and malformed instances into per-path findings.
// Results sort by root-relative path. A returned error means the root could
// not be resolved or ctx ended; in the latter case the partial Result is
// still returned.
//
// An instance is a real (not symlinked) directory named ".groundhog". It is
// well-formed when "groundhog.sh" in it is a regular file (after following a
// symlink) that has an executable bit set, and
// "schedule", "out" and "fired" are directories. Scan checks only metadata
// and never executes the script or lists the instance directory, so it does
// not descend into an instance's own schedule/out/fired trees.
func Scan(ctx context.Context, fsys FS, root string, opts Options) (Result, error) {
	if err := ctx.Err(); err != nil {
		return Result{}, err
	}
	abs, err := filepath.Abs(root)
	if err != nil {
		return Result{}, err
	}
	canon, err := fsys.Canonical(abs)
	if err != nil {
		return Result{}, fmt.Errorf("root: %w", err)
	}
	info, err := fsys.Stat(canon)
	if err != nil {
		return Result{}, fmt.Errorf("root: %w", err)
	}
	if !info.IsDir() {
		return Result{}, fmt.Errorf("root %q is not a directory", canon)
	}
	s := &scanner{
		ctx: ctx, fs: fsys, opts: opts, root: canon,
		visited: map[string]bool{}, found: map[string]bool{},
		excludeNames: map[string]bool{}, excludePaths: map[string]bool{},
	}
	s.res.Root = canon
	for _, e := range opts.Exclude {
		if e = strings.TrimSpace(e); e == "" {
			continue
		}
		if strings.ContainsAny(e, `/\`) {
			if !filepath.IsAbs(e) {
				e = filepath.Join(canon, e)
			}
			e = filepath.Clean(e)
			// Spell it as the walk does, so a symlinked prefix or another
			// case still matches. Only the parent is resolved: the last
			// component keeps naming what was typed (a symlink stays a
			// link, which the walk never follows). A parent that does not
			// exist leaves the path as given, and it matches nothing.
			if parent, err := fsys.Canonical(filepath.Dir(e)); err == nil {
				e = filepath.Join(parent, spellChild(fsys, parent, filepath.Base(e)))
			}
			s.excludePaths[e] = true
		} else {
			s.excludeNames[e] = true
		}
	}
	// A root that is itself an instance directory is an instance.
	if filepath.Base(canon) == InstanceDir {
		s.visited[canon] = true
		s.instance(canon)
	} else {
		s.walk(canon)
	}
	for _, inc := range opts.Include {
		if s.ctx.Err() != nil {
			break
		}
		p := inc
		if !filepath.IsAbs(p) {
			p = filepath.Join(canon, p)
		}
		real, err := fsys.Canonical(p)
		if err != nil {
			s.diag(p, err)
			continue
		}
		if filepath.Base(real) == InstanceDir {
			if !s.visited[real] {
				s.visited[real] = true
				s.instance(real)
			}
			continue
		}
		s.walk(real)
	}
	sort.SliceStable(s.res.Instances, func(i, j int) bool {
		a, b := s.res.Instances[i], s.res.Instances[j]
		if a.Display != b.Display {
			return a.Display < b.Display
		}
		return a.Path < b.Path
	})
	sort.SliceStable(s.res.Diagnostics, func(i, j int) bool {
		a, b := s.res.Diagnostics[i], s.res.Diagnostics[j]
		if a.Display != b.Display {
			return a.Display < b.Display
		}
		return a.Path < b.Path
	})
	return s.res, s.ctx.Err()
}

type scanner struct {
	ctx          context.Context
	fs           FS
	opts         Options
	root         string
	res          Result
	visited      map[string]bool
	found        map[string]bool
	excludeNames map[string]bool
	excludePaths map[string]bool
}

func (s *scanner) display(p string) string {
	if rel, err := filepath.Rel(s.root, p); err == nil {
		return rel
	}
	return p
}

func (s *scanner) diag(p string, err error) {
	s.res.Diagnostics = append(s.res.Diagnostics, Diagnostic{Path: p, Display: s.display(p), Err: err})
}

func (s *scanner) excluded(p string) bool {
	return s.excludeNames[filepath.Base(p)] || s.excludePaths[p]
}

// walk lists dir (a canonical path) and recurses into real subdirectories.
// Symlinks are never followed, so child paths stay canonical.
func (s *scanner) walk(dir string) {
	if s.ctx.Err() != nil || s.visited[dir] {
		return
	}
	s.visited[dir] = true
	entries, err := s.fs.ReadDir(dir)
	if err != nil {
		s.diag(dir, err)
		if len(entries) == 0 {
			return
		}
	}
	sort.Slice(entries, func(i, j int) bool { return entries[i].Name() < entries[j].Name() })
	for _, e := range entries {
		if s.ctx.Err() != nil {
			return
		}
		if !e.IsDir() { // symlinks and files: not followed, not descended
			continue
		}
		child := filepath.Join(dir, e.Name())
		if s.excluded(child) {
			continue
		}
		if e.Name() == InstanceDir {
			if !s.visited[child] {
				s.visited[child] = true
				s.instance(child)
			}
			continue
		}
		if !s.opts.NoDefaultPrune && defaultPruned[e.Name()] {
			continue
		}
		s.walk(child)
	}
}

// instance records the instance at canonical path p from metadata alone.
func (s *scanner) instance(p string) {
	if s.found[p] {
		return
	}
	s.found[p] = true
	inst := Instance{Path: p, Display: s.display(p)}
	script := filepath.Join(p, "groundhog.sh")
	switch info, err := s.fs.Stat(script); {
	case errors.Is(err, fs.ErrNotExist):
		inst.Malformed = append(inst.Malformed, "groundhog.sh missing")
	case err != nil:
		inst.Malformed = append(inst.Malformed, fmt.Sprintf("groundhog.sh unreadable: %v", err))
	case !info.Mode().IsRegular():
		inst.Malformed = append(inst.Malformed, "groundhog.sh is not a regular file")
	case info.Mode().Perm()&0o111 == 0:
		inst.Malformed = append(inst.Malformed, "groundhog.sh is not executable")
	}
	for _, name := range []string{"schedule", "out", "fired"} {
		switch info, err := s.fs.Stat(filepath.Join(p, name)); {
		case errors.Is(err, fs.ErrNotExist):
			inst.Malformed = append(inst.Malformed, name+"/ missing")
		case err != nil:
			inst.Malformed = append(inst.Malformed, fmt.Sprintf("%s/ unreadable: %v", name, err))
		case !info.IsDir():
			inst.Malformed = append(inst.Malformed, name+" is not a directory")
		}
	}
	s.res.Instances = append(s.res.Instances, inst)
}
