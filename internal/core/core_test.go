package core

import (
	"context"
	"errors"
	"io/fs"
	"os"
	"testing"
)

func TestCanceledOpenDoesNotTouchFilesystem(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	_, err := Open(ctx, ".", func(string) (fs.FileInfo, error) { t.Fatal("filesystem touched"); return nil, nil })
	if !errors.Is(err, context.Canceled) {
		t.Fatal(err)
	}
}

func TestOpenOnlyStatsRequestedRoot(t *testing.T) {
	root := t.TempDir()
	calls := 0
	snapshot, err := Open(context.Background(), root, func(path string) (fs.FileInfo, error) {
		calls++
		if path != root {
			t.Fatalf("unexpected filesystem access: %s", path)
		}
		return os.Stat(path)
	})
	if err != nil || snapshot.Root != root || calls != 1 {
		t.Fatalf("snapshot=%+v calls=%d err=%v", snapshot, calls, err)
	}
}

func TestOpenRejectsFileAndPreservesFilesystemErrors(t *testing.T) {
	file, err := os.CreateTemp(t.TempDir(), "file")
	if err != nil {
		t.Fatal(err)
	}
	file.Close()
	if _, err := Open(context.Background(), file.Name(), os.Stat); err == nil {
		t.Fatal("accepted file as root")
	}
	_, err = Open(context.Background(), ".", func(string) (fs.FileInfo, error) { return nil, fs.ErrPermission })
	if !errors.Is(err, fs.ErrPermission) {
		t.Fatal(err)
	}
}
