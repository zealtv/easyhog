// Package discovery owns filesystem discovery. Recursive scanning is deferred.
package discovery

import (
	"context"
	"fmt"
	"io/fs"
	"path/filepath"
)

// Stat is injected so root validation can run against a controlled filesystem.
type Stat func(string) (fs.FileInfo, error)

// Root validates the requested directory without enumerating or changing it.
// This is a search root, not a canonical Groundhog instance identity. Future
// scanning must resolve instance identities and enforce the symlink policy.
func Root(ctx context.Context, path string, stat Stat) (string, error) {
	if err := ctx.Err(); err != nil {
		return "", err
	}
	root, err := filepath.Abs(path)
	if err != nil {
		return "", err
	}
	info, err := stat(root)
	if err != nil {
		return "", fmt.Errorf("root: %w", err)
	}
	if err := ctx.Err(); err != nil {
		return "", err
	}
	if !info.IsDir() {
		return "", fmt.Errorf("root %q is not a directory", root)
	}
	return root, nil
}
