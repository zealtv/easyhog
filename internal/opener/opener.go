// Package opener shows a directory in the platform's file manager. It runs the
// file manager, never anything inside the directory: the program is fixed per
// platform and the directory is only its argument.
package opener

import (
	"errors"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
)

// Command returns the file-manager program and arguments that show dir on the
// given GOOS. dir must be absolute, which keeps it from reading as an option.
func Command(goos, dir string) (string, []string, error) {
	if !filepath.IsAbs(dir) {
		return "", nil, errors.New("the folder path is not absolute")
	}
	switch goos {
	case "darwin":
		return "open", []string{"--", dir}, nil
	case "linux", "freebsd", "openbsd", "netbsd", "dragonfly":
		return "xdg-open", []string{dir}, nil
	}
	return "", nil, fmt.Errorf("no file manager is known for %s", goos)
}

// Open asks the file manager to show dir. It checks that dir is a real
// directory (not a symlink, not a file) first, starts the program without
// waiting, and reaps it in the background.
func Open(dir string) error {
	info, err := os.Lstat(dir)
	switch {
	case os.IsNotExist(err):
		return errors.New("the folder is gone")
	case err != nil:
		return fmt.Errorf("the folder cannot be read: %w", err)
	case !info.IsDir():
		return errors.New("the path is no longer a folder")
	}
	name, args, err := Command(runtime.GOOS, dir)
	if err != nil {
		return err
	}
	cmd := exec.Command(name, args...)
	if err := cmd.Start(); err != nil {
		return fmt.Errorf("could not start %s: %w", name, err)
	}
	go func() { _ = cmd.Wait() }()
	return nil
}
