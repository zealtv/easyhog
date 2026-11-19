// Package core owns headless application state, independent of the terminal.
package core

import (
	"context"

	"github.com/zealtv/easyhog/internal/discovery"
)

// Snapshot holds the validated search root. Opening it acquires no ownership
// and starts no scheduled work; the Refresher discovers what lies beneath it.
type Snapshot struct {
	Root string
}

// Open validates root and returns it as a Snapshot.
func Open(ctx context.Context, root string, stat discovery.Stat) (Snapshot, error) {
	path, err := discovery.Root(ctx, root, stat)
	return Snapshot{Root: path}, err
}
