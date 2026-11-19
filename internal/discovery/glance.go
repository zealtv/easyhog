package discovery

import (
	"context"
	"io/fs"
	"path"
	"path/filepath"
	"reflect"
	"sort"
	"strings"
	"time"
)

// Bounds on one glance, so a huge or hostile tray cannot make it slow or large.
const (
	glanceMaxDepth    = 6
	glanceMaxDirReads = 400
	glanceMaxSchedule = 300
	glanceMaxOut      = 300
)

// ScheduleDir is one directory beneath an instance's schedule/ tree. Leaf means
// it holds no directory of its own, which is how an item usually looks; this is
// a reading of the listing, not of Groundhog's grammar, and the status JSON
// stays the authority once an instance has been inspected.
type ScheduleDir struct {
	Path   string // slash-separated, relative to schedule/
	Leaf   bool
	Paused bool // some path component ends in ".paused"
}

// Script is the metadata of the instance's groundhog.sh, taken without reading
// or running it. A change in any field means the script may be a different one.
type Script struct {
	Present bool
	Size    int64
	ModTime time.Time
	Mode    fs.FileMode
}

// Equal reports whether two scripts have the same metadata.
func (s Script) Equal(o Script) bool {
	return s.Present == o.Present && s.Size == o.Size && s.Mode == o.Mode && s.ModTime.Equal(o.ModTime)
}

// Glance is what directory listings and metadata alone say about one instance.
// It never reads a file, never follows a symlink and never runs anything, so it
// is safe for instances nobody has chosen to inspect. It does not parse
// Groundhog's private runtime files: whether runtime/ exists is all it knows
// about them.
type Glance struct {
	Script Script
	// Schedule lists directories under schedule/, sorted by path.
	Schedule          []ScheduleDir
	ScheduleTruncated bool
	// Out lists the entry names directly under out/, sorted.
	Out          []string
	OutTruncated bool
	// Runtime says whether runtime/ exists as a directory.
	Runtime bool
	// Problems lists listings that failed, in words, without failing the rest.
	Problems []string
}

// Equal reports whether two glances describe the same listings.
func (g Glance) Equal(o Glance) bool { return reflect.DeepEqual(g, o) }

// Items returns the leaf schedule directories.
func (g Glance) Items() []ScheduleDir {
	var out []ScheduleDir
	for _, d := range g.Schedule {
		if d.Leaf {
			out = append(out, d)
		}
	}
	return out
}

// GlanceInstance lists the instance directory at inst (an Instance.Path). It
// honours ctx between directory reads and returns what it has on cancellation.
func GlanceInstance(ctx context.Context, fsys FS, inst string) (Glance, error) {
	var g Glance
	if info, err := fsys.Lstat(filepath.Join(inst, "groundhog.sh")); err == nil {
		g.Script = Script{Present: true, Size: info.Size(), ModTime: info.ModTime(), Mode: info.Mode()}
	}
	if info, err := fsys.Lstat(filepath.Join(inst, "runtime")); err == nil && info.IsDir() {
		g.Runtime = true
	}
	reads := 0
	var walk func(dir, rel string, depth int, paused bool)
	walk = func(dir, rel string, depth int, paused bool) {
		if ctx.Err() != nil {
			return
		}
		if reads >= glanceMaxDirReads || depth > glanceMaxDepth {
			g.ScheduleTruncated = true
			return
		}
		reads++
		entries, err := fsys.ReadDir(dir)
		if err != nil {
			g.Problems = append(g.Problems, "schedule/"+rel+": "+oneLine(err))
			return
		}
		for _, e := range entries {
			if !e.IsDir() { // a symlink to a directory reports false, so none is followed
				continue
			}
			if len(g.Schedule) >= glanceMaxSchedule {
				g.ScheduleTruncated = true
				return
			}
			p := path.Join(rel, e.Name())
			pz := paused || strings.HasSuffix(e.Name(), ".paused")
			at := len(g.Schedule)
			g.Schedule = append(g.Schedule, ScheduleDir{Path: p, Leaf: true, Paused: pz})
			before := len(g.Schedule)
			walk(filepath.Join(dir, e.Name()), p, depth+1, pz)
			if len(g.Schedule) > before {
				g.Schedule[at].Leaf = false
			}
		}
	}
	walk(filepath.Join(inst, "schedule"), "", 1, false)
	sort.SliceStable(g.Schedule, func(i, j int) bool { return g.Schedule[i].Path < g.Schedule[j].Path })

	if ctx.Err() == nil {
		entries, err := fsys.ReadDir(filepath.Join(inst, "out"))
		if err != nil {
			g.Problems = append(g.Problems, "out/: "+oneLine(err))
		}
		for _, e := range entries {
			if len(g.Out) >= glanceMaxOut {
				g.OutTruncated = true
				break
			}
			g.Out = append(g.Out, e.Name())
		}
		sort.Strings(g.Out)
	}
	sort.Strings(g.Problems)
	return g, ctx.Err()
}

func oneLine(err error) string {
	return strings.Join(strings.Fields(err.Error()), " ")
}
