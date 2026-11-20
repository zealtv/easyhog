// Package payload reads a schedule item's files as inert, bounded text for the
// preview. It lists and reads; it never executes, opens with a program, follows
// a symlink or walks outside the item. Everything it returns is plain text
// that the caller still cleans before drawing.
package payload

import (
	"fmt"
	"io"
	"io/fs"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"unicode/utf8"
)

// Bounds on one preview, so a huge or hostile item cannot make it slow or large.
const (
	MaxFiles       = 12      // files shown from one item
	MaxEntries     = 200     // directory entries listed before giving up
	MaxDepth       = 3       // directories descended inside an item
	MaxFileBytes   = 4 << 10 // bytes read from one file
	MaxTotalBytes  = 16 << 10
	MaxLinesInFile = 40
	MaxLineRunes   = 200
)

// StartOnlyNote begins the indented line that ends a file shown only in part.
// The preview's styling reads it to keep that notice out of a file's own text.
const StartOnlyNote = "… (shows the start only;"

// Files is the whole file surface the reader may use: metadata, listings and a
// bounded read. Paths are absolute operating-system paths.
type Files interface {
	ReadDir(name string) ([]fs.DirEntry, error)
	Lstat(name string) (fs.FileInfo, error)
	// ReadPrefix returns at most n bytes from the start of the regular file
	// name. It refuses anything that is not a regular file when opened.
	ReadPrefix(name string, n int) ([]byte, error)
}

// OS is the real file system.
type OS struct{}

func (OS) ReadDir(name string) ([]fs.DirEntry, error) { return os.ReadDir(name) }
func (OS) Lstat(name string) (fs.FileInfo, error)     { return os.Lstat(name) }

// ReadPrefix opens name and checks that what it opened is the regular file
// Lstat saw, so a symlink swapped in between is refused.
func (OS) ReadPrefix(name string, n int) ([]byte, error) {
	before, err := os.Lstat(name)
	if err != nil {
		return nil, err
	}
	if !before.Mode().IsRegular() {
		return nil, fmt.Errorf("not a regular file")
	}
	f, err := os.Open(name)
	if err != nil {
		return nil, err
	}
	defer f.Close()
	after, err := f.Stat()
	if err != nil {
		return nil, err
	}
	if !after.Mode().IsRegular() || !os.SameFile(before, after) {
		return nil, fmt.Errorf("changed while opening")
	}
	return io.ReadAll(io.LimitReader(f, int64(n)))
}

// Preview describes the item at rel (slash-separated, relative to the instance
// directory inst, for example "schedule/daily/morning-nudge") as text. It
// returns a sentence, never an error: a vanished, unreadable or oversized item
// is described as such.
func Preview(fsys Files, inst, rel string) string {
	dir, why := resolve(fsys, inst, rel)
	if why != "" {
		return why
	}
	var b strings.Builder
	total := 0
	shown, skipped := 0, 0
	entries := 0
	var walk func(dir, prefix string, depth int)
	walk = func(dir, prefix string, depth int) {
		list, err := fsys.ReadDir(dir)
		if err != nil {
			fmt.Fprintf(&b, "%s(cannot list: %s)\n", prefix, reason(err))
			return
		}
		sort.Slice(list, func(i, j int) bool {
			// An item's own run comes first: it says what firing the item does.
			if a, b := depth == 0 && list[i].Name() == "run", depth == 0 && list[j].Name() == "run"; a != b {
				return a
			}
			return list[i].Name() < list[j].Name()
		})
		for _, e := range list {
			entries++
			if entries > MaxEntries {
				skipped++
				continue
			}
			name := prefix + e.Name()
			full := filepath.Join(dir, e.Name())
			info, err := fsys.Lstat(full)
			switch {
			case err != nil:
				fmt.Fprintf(&b, "%s: cannot be read (%s)\n", name, reason(err))
			case info.IsDir():
				if depth >= MaxDepth {
					fmt.Fprintf(&b, "%s/ (directory, not opened: too deep)\n", name)
					continue
				}
				fmt.Fprintf(&b, "%s/\n", name)
				walk(full, name+"/", depth+1)
			case depth == 0 && name == "run" && info.Mode()&fs.ModeSymlink != 0:
				// Groundhog follows it: the delivery's copy keeps the link, and the
				// wrapper starts what it points to. Easyhog never follows a link.
				b.WriteString("run: a link named run. Groundhog starts what it points to when the item fires, if that is an executable file; the link is not followed here\n")
			case !info.Mode().IsRegular():
				fmt.Fprintf(&b, "%s: not a regular file (%s), not read\n", name, kind(info.Mode()))
			case shown >= MaxFiles || total >= MaxTotalBytes:
				skipped++
			default:
				shown++
				if depth == 0 && name == "run" {
					// Groundhog reserves the name: an executable run starts for each
					// delivery of the item. Its text is shown like any file's.
					if info.Mode().Perm()&0o111 != 0 {
						b.WriteString("Runs when the item fires (Groundhog starts this executable for each delivery):\n")
					} else {
						b.WriteString("A file named run that is not executable, so it does not run:\n")
					}
				}
				total += describeFile(&b, fsys, name, full, info.Size())
			}
		}
	}
	walk(dir, "", 0)
	if shown == 0 && skipped == 0 && b.Len() == 0 {
		return "(the item is empty: no files)\n"
	}
	if skipped > 0 {
		fmt.Fprintf(&b, "… %d more not shown\n", skipped)
	}
	return b.String()
}

// MaxLogs is how many delivery logs one item's preview shows.
const MaxLogs = 2

// Log describes a delivery's run log at rel (slash-separated, relative to the
// instance directory inst, "out/<name>/.groundhog-log", as status names it) as
// bounded text, the way Preview shows a file: the start of it, cleaned by the
// caller. A missing or unreadable log, or one reached through a symlink, is
// described as such.
func Log(fsys Files, inst, rel string) string {
	dir, why := resolve(fsys, inst, filepath.ToSlash(filepath.Dir(filepath.FromSlash(rel))))
	if why != "" || !strings.HasPrefix(rel, "out/") || filepath.Base(rel) != ".groundhog-log" {
		return rel + ": not read (not a delivery log inside the instance)\n"
	}
	full := filepath.Join(dir, ".groundhog-log")
	info, err := fsys.Lstat(full)
	switch {
	case err != nil:
		return rel + ": cannot be read (" + reason(err) + ")\n"
	case !info.Mode().IsRegular():
		return rel + ": not a regular file, not read\n"
	}
	var b strings.Builder
	describeFile(&b, fsys, rel, full, info.Size())
	return b.String()
}

// ItemPreview is Preview for the item at rel, followed by the logs of its
// newest deliveries (at most MaxLogs, as status names them).
func ItemPreview(fsys Files, inst, rel string, logs []string) string {
	text := Preview(fsys, inst, rel)
	for i, l := range logs {
		if i == MaxLogs {
			break
		}
		text += "\nOutput of a run (read as text):\n" + Log(fsys, inst, l)
	}
	return text
}

// resolve checks that rel names a real directory beneath inst without passing
// through a symlink, and returns its path; otherwise it returns the sentence
// that says why not.
func resolve(fsys Files, inst, rel string) (string, string) {
	rel = filepath.FromSlash(rel)
	if rel == "" || filepath.IsAbs(rel) || filepath.VolumeName(rel) != "" {
		return "", "(the item's path is not inside the instance, so nothing was read)\n"
	}
	cur := inst
	for _, part := range strings.Split(rel, string(filepath.Separator)) {
		if part == "" || part == "." || part == ".." {
			return "", "(the item's path is not inside the instance, so nothing was read)\n"
		}
		cur = filepath.Join(cur, part)
		info, err := fsys.Lstat(cur)
		switch {
		case os.IsNotExist(err):
			return "", "(the item is gone: it is no longer on disk. The next refresh will drop it.)\n"
		case err != nil:
			return "", "(the item cannot be read: " + reason(err) + ")\n"
		case info.Mode()&fs.ModeSymlink != 0:
			return "", "(a symbolic link lies on the item's path, so nothing was read)\n"
		case !info.IsDir():
			return "", "(the item is no longer a directory, so nothing was read)\n"
		}
	}
	return cur, ""
}

// describeFile writes one file's header and, when it is text, its first lines.
// It returns the bytes it read.
func describeFile(b *strings.Builder, fsys Files, name, full string, size int64) int {
	fmt.Fprintf(b, "%s  %s\n", name, human(size))
	if size == 0 {
		b.WriteString("    (empty)\n")
		return 0
	}
	data, err := fsys.ReadPrefix(full, MaxFileBytes)
	if err != nil {
		fmt.Fprintf(b, "    (cannot be read: %s)\n", reason(err))
		return 0
	}
	if isBinary(data) {
		b.WriteString("    (binary file, not shown)\n")
		return len(data)
	}
	text := string(data)
	truncated := size > int64(len(data))
	if truncated {
		// Do not end in the middle of a rune.
		for !utf8.ValidString(text) && len(text) > 0 {
			text = text[:len(text)-1]
		}
	}
	text = strings.ReplaceAll(text, "\r\n", "\n")
	lines := strings.Split(strings.TrimRight(text, "\n"), "\n")
	cut := false
	if len(lines) > MaxLinesInFile {
		lines, cut = lines[:MaxLinesInFile], true
	}
	for _, line := range lines {
		if r := []rune(line); len(r) > MaxLineRunes {
			line = string(r[:MaxLineRunes]) + "…"
		}
		b.WriteString("    " + line + "\n")
	}
	if cut || truncated {
		fmt.Fprintf(b, "    %s the file has %s)\n", StartOnlyNote, human(size))
	}
	return len(data)
}

// isBinary says the bytes are not text: a NUL, or invalid UTF-8 other than a
// rune cut off at the end of the prefix.
func isBinary(data []byte) bool {
	for _, c := range data {
		if c == 0 {
			return true
		}
	}
	if utf8.Valid(data) {
		return false
	}
	// A prefix may end inside a multi-byte rune; allow up to 3 such bytes.
	for cut := 1; cut <= 3 && cut < len(data); cut++ {
		if utf8.Valid(data[:len(data)-cut]) {
			return false
		}
	}
	return true
}

func human(n int64) string {
	switch {
	case n < 1<<10:
		return fmt.Sprintf("%d bytes", n)
	case n < 1<<20:
		return fmt.Sprintf("%.1f KiB", float64(n)/(1<<10))
	}
	return fmt.Sprintf("%.1f MiB", float64(n)/(1<<20))
}

func kind(m fs.FileMode) string {
	switch {
	case m&fs.ModeSymlink != 0:
		return "symbolic link"
	case m&fs.ModeNamedPipe != 0:
		return "named pipe"
	case m&fs.ModeSocket != 0:
		return "socket"
	case m&fs.ModeDevice != 0:
		return "device"
	}
	return "special file"
}

// reason shortens a file error to words without the path.
func reason(err error) string {
	switch {
	case os.IsNotExist(err):
		return "it is gone"
	case os.IsPermission(err):
		return "permission denied"
	}
	if pe, ok := err.(*fs.PathError); ok {
		return pe.Err.Error()
	}
	return strings.Join(strings.Fields(err.Error()), " ")
}
