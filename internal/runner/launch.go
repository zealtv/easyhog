package runner

import (
	"errors"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"sync"
)

// Launch describes one long-lived child, such as a Groundhog runner.
//
// Process.Run is the wrong tool for such a child: it kills the child when its
// context ends and gives it pipes. A supervisor that holds a runner's output in
// a pipe and then exits leaves the runner (and the consumers it starts, which
// inherit the streams) writing into a pipe nobody reads, and they die of
// SIGPIPE. A Launch therefore carries a file for the child's output, which
// stays valid after the supervisor is gone.
type Launch struct {
	Executable string
	Args       []string
	Dir        string
	// Log receives the child's stdout and stderr. It must be a regular file the
	// caller opened; the launcher never substitutes a pipe. Stdin is /dev/null.
	Log *os.File
}

// Exit says how a child ended.
type Exit struct {
	// Code is the exit status, or -1 when a signal ended the child.
	Code int
	// Signal names the ending signal ("terminated", "killed", …) when Code is -1
	// and the platform reports one.
	Signal string
	// Err is set when the child could not be waited for, or ended abnormally
	// for a reason Code and Signal do not carry.
	Err error
}

// OK reports a clean exit 0.
func (e Exit) OK() bool { return e.Code == 0 && e.Err == nil }

func (e Exit) String() string {
	switch {
	case e.Err != nil:
		return e.Err.Error()
	case e.Signal != "":
		return "ended by signal " + e.Signal
	}
	return fmt.Sprintf("exit status %d", e.Code)
}

// Child is a started long-lived process. Nothing here can kill it: the only
// signal a Child accepts is the polite one.
type Child interface {
	// PID is the child's process id.
	PID() int
	// Done is closed once the child has ended and been reaped.
	Done() <-chan struct{}
	// Exit is how it ended; call it only after Done is closed.
	Exit() Exit
	// Terminate asks the child to end with SIGTERM. It is a no-op error once the
	// child has ended. There is deliberately no Kill: a Groundhog runner that is
	// killed leaves its in-flight handoff unrecorded.
	Terminate() error
}

// Launcher starts long-lived children. A fake stands in for it in tests.
type Launcher interface {
	Start(Launch) (Child, error)
}

// Spawner is the real Launcher. It starts the child directly: no shell, no PATH
// lookup, no context that could kill it, output to the caller's file, and (on
// Unix) its own process group so a terminal's Ctrl-C or hangup does not reach it
// by the back door. Its end is the supervisor's business.
type Spawner struct{}

func (Spawner) Start(l Launch) (Child, error) {
	if !filepath.IsAbs(l.Executable) || !filepath.IsAbs(l.Dir) {
		return nil, errors.New("executable and working directory must be absolute")
	}
	if l.Log == nil {
		return nil, errors.New("a launch needs a log file for the child's output")
	}
	if info, err := l.Log.Stat(); err != nil || !info.Mode().IsRegular() {
		return nil, errors.New("the child's log must be a regular file, not a pipe")
	}
	cmd := exec.Command(l.Executable, l.Args...)
	cmd.Dir = l.Dir
	// *os.File values are handed to the child as they are: no pipe, no copying
	// goroutine, nothing that closes when this process ends.
	cmd.Stdout, cmd.Stderr = l.Log, l.Log
	cmd.SysProcAttr = ownProcessGroup()
	if err := cmd.Start(); err != nil {
		return nil, err
	}
	c := &spawned{cmd: cmd, done: make(chan struct{})}
	go c.wait()
	return c, nil
}

type spawned struct {
	cmd  *exec.Cmd
	done chan struct{}

	mu   sync.Mutex
	exit Exit
}

func (c *spawned) wait() {
	err := c.cmd.Wait()
	x := Exit{Code: c.cmd.ProcessState.ExitCode()}
	if sig := signalOf(c.cmd.ProcessState); sig != "" {
		x.Code, x.Signal = -1, sig
	}
	var ee *exec.ExitError
	if err != nil && !errors.As(err, &ee) {
		x.Err = err
	}
	c.mu.Lock()
	c.exit = x
	c.mu.Unlock()
	close(c.done)
}

func (c *spawned) PID() int              { return c.cmd.Process.Pid }
func (c *spawned) Done() <-chan struct{} { return c.done }

func (c *spawned) Exit() Exit {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.exit
}

func (c *spawned) Terminate() error {
	select {
	case <-c.done:
		return os.ErrProcessDone
	default:
	}
	return terminate(c.cmd.Process)
}
