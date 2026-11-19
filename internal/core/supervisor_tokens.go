package core

import (
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"syscall"
	"time"

	"github.com/zealtv/easyhog/internal/groundhog"
)

// Where tokens live, and what happens to one a crash leaves behind.
//
// Each Easyhog session makes one private directory (mode 0700) beneath the
// user's state directory: <state>/sessions/<pid>-<random>/. It holds a lock file,
// one token file and one runner log per instance the session holds a token for,
// the last two named by a counter so a path never carries a repository's name.
// It sits outside every instance and every repository, so no token can be
// committed or listed by a scan.
//
// A live session holds an advisory lock (flock) on the directory's lock file for
// as long as it runs; the OS drops it however the process dies, so a lock that
// can be taken means the session has ended (no pids, nothing to reuse). A clean
// Quit releases each instance, deletes its token, and removes the directory if
// nothing is left in it. After a crash, or when a release was refused, the
// tokens stay where they are, mode 0600: Groundhog keeps the ownership (nothing
// expires or transfers), and a later session moves them into its own directory
// by atomic rename (Adopt), after which the ownership rule applies to them like
// any other token. A token no session can match to an instance stays where it
// is and is listed (Leftovers).

const (
	sessionsDirName = "sessions"
	lockName        = "session.lock"
	// stagePrefix names a session directory while it is being made. Making it
	// has to be one step as far as other sessions see: a directory without a held
	// lock reads as an ended session, so the lock is taken before the directory
	// gets its real name, and a scan skips the staging names.
	stagePrefix = ".new-"
)

// stateBase returns the absolute state directory.
func (s *Supervisor) stateBase() (string, error) {
	base := s.cfg.StateDir
	if base == "" {
		var err error
		if base, err = DefaultStateDir(); err != nil {
			return "", fmt.Errorf("no state directory for tokens: %w", err)
		}
	}
	if !filepath.IsAbs(base) {
		return "", fmt.Errorf("state directory %q is not absolute", base)
	}
	return base, nil
}

// sessionDir returns the session's private directory, making it on first use.
// It refuses a state directory that lies inside the instance being enrolled:
// tokens belong outside every instance.
func (s *Supervisor) sessionDir(instance string) (string, error) {
	base, err := s.stateBase()
	if err != nil {
		return "", err
	}
	if Within(instance, base) {
		return "", fmt.Errorf("%w: the state directory %s lies inside the instance", groundhog.ErrInvalidArgument, base)
	}
	return s.ensureDir(base)
}

// ensureDir makes the session's directory and takes its lock, once.
func (s *Supervisor) ensureDir(base string) (string, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.dir != "" {
		return s.dir, nil
	}
	root := filepath.Join(base, sessionsDirName)
	if err := os.MkdirAll(root, 0o700); err != nil {
		return "", fmt.Errorf("make state directory: %w", err)
	}
	dir, lock, err := newSessionDir(root)
	if err != nil {
		return "", err
	}
	if real, err := filepath.EvalSymlinks(dir); err == nil {
		dir = real
	}
	s.dir, s.lock = dir, lock
	return dir, nil
}

// newSessionDir makes a private directory under root with its lock file held,
// as a single step for every other session: it is made under a staging name, its
// lock taken, and only then renamed.
func newSessionDir(root string) (dir string, lock *os.File, err error) {
	stage, err := os.MkdirTemp(root, stagePrefix) // created 0700
	if err != nil {
		return "", nil, fmt.Errorf("make session directory: %w", err)
	}
	lockPath := filepath.Join(stage, lockName)
	fail := func(f *os.File, err error) (string, *os.File, error) {
		if f != nil {
			_ = f.Close()
		}
		_ = os.Remove(lockPath)
		_ = os.Remove(stage)
		return "", nil, err
	}
	if err := os.Chmod(stage, 0o700); err != nil {
		return fail(nil, fmt.Errorf("make session directory private: %w", err))
	}
	f, err := os.OpenFile(lockPath, os.O_RDWR|os.O_CREATE|os.O_EXCL, 0o600)
	if err != nil {
		return fail(nil, fmt.Errorf("make session lock: %w", err))
	}
	if busy, err := tryLock(f); err != nil || busy {
		return fail(f, fmt.Errorf("lock the session directory: %v", errOrBusy(err)))
	}
	dir = filepath.Join(root, fmt.Sprintf("%d-%s", os.Getpid(), strings.TrimPrefix(filepath.Base(stage), stagePrefix)))
	if err := os.Rename(stage, dir); err != nil {
		return fail(f, fmt.Errorf("name the session directory: %w", err))
	}
	return dir, f, nil
}

func errOrBusy(err error) error {
	if err == nil {
		return errors.New("already locked")
	}
	return err
}

// tryLock takes f's advisory lock without waiting. busy is true when another
// open file description holds it (another process, or another supervisor in this
// one). The lock lasts until f is closed or the process ends.
func tryLock(f *os.File) (busy bool, err error) {
	for {
		err = syscall.Flock(int(f.Fd()), syscall.LOCK_EX|syscall.LOCK_NB)
		switch {
		case err == nil:
			return false, nil
		case errors.Is(err, syscall.EINTR):
			continue
		case errors.Is(err, syscall.EWOULDBLOCK):
			return true, nil
		}
		return false, err
	}
}

// newPaths returns fresh, absent token and log paths in the session directory.
func (s *Supervisor) newPaths(dir string) (token, log string) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.seq++
	return filepath.Join(dir, fmt.Sprintf("i%d.token", s.seq)), filepath.Join(dir, fmt.Sprintf("i%d.log", s.seq))
}

// tokenMeta is the token file's nonsecret metadata (contract, "Ownership and
// enrollment"): enough to tell, from `status`, whether the token controls the
// current owner. The secret field is absent from this type on purpose.
type tokenMeta struct {
	Schema     int    `json:"schema_version"`
	Instance   string `json:"instance"`
	OwnerID    string `json:"owner_id"`
	Generation int64  `json:"generation"`
}

var errTokenMeta = errors.New("token metadata unreadable")

// readTokenMeta reads the token file and keeps only the nonsecret fields. The
// bytes hold the secret while they are in memory, so they are cleared at once
// and no error quotes them. A missing file is returned as os.ErrNotExist.
func readTokenMeta(path string) (tokenMeta, error) {
	data, err := os.ReadFile(path)
	if err != nil {
		if errors.Is(err, os.ErrNotExist) {
			return tokenMeta{}, os.ErrNotExist
		}
		return tokenMeta{}, errTokenMeta
	}
	defer clear(data)
	var m tokenMeta
	if json.Unmarshal(data, &m) != nil || m.Schema != 1 || m.OwnerID == "" || m.Generation < 1 || !filepath.IsAbs(m.Instance) {
		return tokenMeta{}, errTokenMeta
	}
	return m, nil
}

// readTokenMetaFor is readTokenMeta for a token that must name the given instance.
func readTokenMetaFor(path, instance string) (tokenMeta, error) {
	m, err := readTokenMeta(path)
	if err == nil && m.Instance != instance {
		return tokenMeta{}, fmt.Errorf("%w: it names another instance", errTokenMeta)
	}
	return m, err
}

// Leftover is a token an Adopt pass left where it found it, with the reason.
type Leftover struct {
	// Instance is the instance the token names, empty when it could not be read.
	Instance  string
	TokenFile string
	Why       string
}

// Leftovers returns the tokens the latest Adopt could not take up. They stay in
// the ended session's directory, which may be the only credential for an
// instance still owned (a repository that moved, say).
func (s *Supervisor) Leftovers() []Leftover {
	s.mu.Lock()
	defer s.mu.Unlock()
	return append([]Leftover(nil), s.leftovers...)
}

// Adopt takes up the tokens that ended sessions left. Nothing runs: it lists the
// sessions' directories, and for each one whose lock can be taken (that session
// has ended; one that cannot is alive and untouched) moves every token that names
// an instance still in place into this session's directory by rename, with its
// runner log. A rename is atomic, so with several Easyhog windows open exactly
// one of them wins a token; the others find it gone. Adopted tokens then come
// under the ownership rule like any other: the first coherent status that
// shows another owner, or none, deletes one. Adoption never restarts anything:
// a runner the ended session left running keeps running, and Run and Stop
// work through the token. A directory left with nothing in it goes.
//
// inScope says whether an instance (the path its token names) lies where this
// session looks: beneath its root. A token for any other instance is left
// exactly where it is, unread by any script, uncounted, never listed as a
// leftover and never stopped at Quit, so a session on that instance's own root
// can adopt it later. inScope must not be nil.
//
// Call it at the start of a session and now and then after; it also refreshes
// this session's own lock file, whose modification time is the "last seen alive"
// a later session reports. It returns the instances adopted.
func (s *Supervisor) Adopt(inScope func(instance string) bool) []string {
	s.mu.Lock()
	closing, dir, held := s.closing, s.dir, s.lock != nil
	s.mu.Unlock()
	if closing {
		return nil
	}
	if held { // the lock file's path, not its descriptor's: the directory was renamed after the file was made
		now := time.Now()
		_ = os.Chtimes(filepath.Join(dir, lockName), now, now)
	}
	base, err := s.stateBase()
	if err != nil {
		return nil
	}
	root := filepath.Join(base, sessionsDirName)
	entries, err := os.ReadDir(root)
	if err != nil {
		return nil
	}
	var adopted []string
	var left []Leftover
	for _, e := range entries {
		if !e.IsDir() || strings.HasPrefix(e.Name(), ".") {
			continue
		}
		a, l := s.adoptFrom(filepath.Join(root, e.Name()), base, inScope)
		adopted, left = append(adopted, a...), append(left, l...)
	}
	s.mu.Lock()
	s.leftovers = left
	s.mu.Unlock()
	if len(adopted) > 0 {
		s.signal()
	}
	return adopted
}

// adoptFrom handles one session directory.
func (s *Supervisor) adoptFrom(dir, base string, inScope func(instance string) bool) (adopted []string, left []Leftover) {
	// No lock file: not a directory this version made (or one being removed).
	// Whether its session lives is unknown, so it is left alone.
	lf, err := os.OpenFile(filepath.Join(dir, lockName), os.O_RDWR, 0)
	if err != nil {
		return nil, nil
	}
	defer lf.Close()
	if busy, err := tryLock(lf); err != nil || busy {
		return nil, nil // alive, or not ours to judge
	}
	endedAt := time.Now()
	if fi, err := lf.Stat(); err == nil {
		endedAt = fi.ModTime()
	}
	names, err := os.ReadDir(dir)
	if err != nil {
		return nil, nil
	}
	for _, n := range names {
		name := n.Name()
		stem, ok := strings.CutSuffix(name, ".token")
		if !ok || n.IsDir() {
			continue
		}
		tokenPath, logPath := filepath.Join(dir, name), filepath.Join(dir, stem+".log")
		meta, err := readTokenMeta(tokenPath)
		switch {
		case errors.Is(err, os.ErrNotExist):
			continue // another window took it
		case err != nil:
			left = append(left, Leftover{TokenFile: tokenPath, Why: "its metadata cannot be read"})
			continue
		}
		if !inScope(meta.Instance) {
			// Another root's instance: not this session's to adopt, count or stop at
			// Quit. The token stays, 0600 where it is, for a session on that root.
			continue
		}
		if _, err := os.Stat(filepath.Join(meta.Instance, "groundhog.sh")); err != nil {
			left = append(left, Leftover{Instance: meta.Instance, TokenFile: tokenPath, Why: "no Groundhog instance is at that path now (moved or deleted?)"})
			continue
		}
		switch why := s.adoptToken(meta, tokenPath, logPath, endedAt, base); why {
		case "":
			adopted = append(adopted, meta.Instance)
		case "taken":
		default:
			left = append(left, Leftover{Instance: meta.Instance, TokenFile: tokenPath, Why: why})
		}
	}
	s.tidyEnded(dir)
	return adopted, left
}

// adoptToken moves one token (and its log) into this session's directory and
// registers it. It returns "" when it adopted the token, "taken" when another
// window renamed it first, and otherwise why the token stays.
func (s *Supervisor) adoptToken(meta tokenMeta, tokenPath, logPath string, endedAt time.Time, base string) string {
	sess, err := s.sessionFor("adopt", meta.Instance)
	if err != nil {
		return "this session is quitting or cannot reach the instance: " + err.Error()
	}
	// An operation in flight on the instance (a first Run, say) settles first;
	// the next Adopt tries again.
	select {
	case sess.op <- struct{}{}:
		defer sess.release()
	default:
		return "an operation on the instance is in flight; it is tried again later"
	}
	s.mu.Lock()
	held := sess.holds()
	s.mu.Unlock()
	if held {
		return "this session already holds another token for the instance; it is tried again once that one is settled"
	}
	dir, err := s.ensureDir(base)
	if err != nil {
		return err.Error()
	}
	token, log := s.newPaths(dir)
	if err := os.Rename(tokenPath, token); err != nil {
		if errors.Is(err, os.ErrNotExist) {
			return "taken"
		}
		return "it could not be moved: " + err.Error()
	}
	_ = os.Rename(logPath, log) // the runner, if it lives, keeps writing to the same file
	s.mu.Lock()
	sess.phase, sess.token, sess.log = PhaseEnrolled, token, log
	sess.ownerID, sess.gen = meta.OwnerID, meta.Generation
	sess.child, sess.exit, sess.asked, sess.notice = nil, nil, false, ""
	sess.adopted, sess.adoptedFrom = true, endedAt
	s.mu.Unlock()
	return ""
}

// tidyEnded removes what an ended session's directory no longer needs: logs
// whose token is gone, then the directory itself when only its lock file is
// left. The caller holds the directory's lock.
func (s *Supervisor) tidyEnded(dir string) {
	names, err := os.ReadDir(dir)
	if err != nil {
		return
	}
	tokens := map[string]bool{}
	for _, n := range names {
		if stem, ok := strings.CutSuffix(n.Name(), ".token"); ok {
			tokens[stem] = true
		}
	}
	rest := 0
	for _, n := range names {
		name := n.Name()
		if stem, ok := strings.CutSuffix(name, ".log"); ok && !tokens[stem] {
			_ = os.Remove(filepath.Join(dir, name))
			continue
		}
		if name != lockName {
			rest++
		}
	}
	if rest == 0 {
		_ = os.Remove(filepath.Join(dir, lockName))
		_ = os.Remove(dir)
	}
}
