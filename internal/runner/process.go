// Package runner provides the subprocess boundary: Process runs the short,
// bounded protocol calls (status, tick, retry, pause) and kills the direct
// child on cancellation. Nothing here starts a long-lived process.
package runner

import (
	"context"
	"fmt"
	"io"
	"os/exec"
	"path/filepath"
	"time"
)

// Invocation names a concrete executable without a shell or PATH lookup.
// Callers own output limits and parsing; nil streams discard output.
type Invocation struct {
	Executable     string
	Args           []string
	Dir            string
	Stdout, Stderr io.Writer
}

// Executor permits a fake process boundary in headless integration tests.
type Executor interface {
	Run(context.Context, Invocation) error
}

// Process runs only an explicitly requested direct child. Cancellation kills
// and waits for that child, not its descendants: a run that a tick started
// lives in a session of its own and is never reached.
type Process struct{}

func (Process) Run(ctx context.Context, call Invocation) error {
	if err := ctx.Err(); err != nil {
		return err
	}
	if !filepath.IsAbs(call.Executable) || !filepath.IsAbs(call.Dir) {
		return fmt.Errorf("executable and working directory must be absolute")
	}
	cmd := exec.CommandContext(ctx, call.Executable, call.Args...)
	cmd.Dir, cmd.Stdout, cmd.Stderr = call.Dir, call.Stdout, call.Stderr
	// Descendants may retain inherited pipes after the direct child exits.
	// Bound pipe draining without signaling those independent descendants.
	cmd.WaitDelay = time.Second
	err := cmd.Run()
	if ctx.Err() != nil {
		return ctx.Err()
	}
	return err
}
