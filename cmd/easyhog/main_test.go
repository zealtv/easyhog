package main

import (
	"bytes"
	"context"
	"errors"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
	"time"
	"unicode"

	"github.com/zealtv/easyhog/internal/core"
	"github.com/zealtv/easyhog/internal/runner"
)

// Exercise main's real stderr/exit boundary in a separate process.
func TestCommandHelper(t *testing.T) {
	for i, arg := range os.Args {
		if arg == "easyhog-command-helper" {
			os.Args = append([]string{"easyhog"}, os.Args[i+1:]...)
			main()
			os.Exit(0)
		}
	}
}

func TestInvalidRootDiagnosticsEscapeControls(t *testing.T) {
	executable, err := os.Executable()
	if err != nil {
		t.Fatal(err)
	}
	null, err := os.OpenFile(os.DevNull, os.O_RDWR, 0)
	if err != nil {
		t.Fatal(err)
	}
	defer null.Close()

	for _, name := range []string{"missing\x1b[2J", "missing\nforged diagnostic", "missing\x1b[2J\nforged diagnostic"} {
		t.Run(name, func(t *testing.T) {
			root := filepath.Join(t.TempDir(), name)
			// Keep the platform's actual filesystem cause for the comparison below.
			_, statErr := os.Stat(root)
			var cause *os.PathError
			if !errors.As(statErr, &cause) {
				t.Fatalf("expected stat failure: %v", statErr)
			}
			// The headless error must retain its cause and original path.
			var diagnostics bytes.Buffer
			err := run(context.Background(), nil, []string{root}, null, null, &diagnostics)
			var pathErr *os.PathError
			if !errors.Is(err, cause.Err) || !errors.As(err, &pathErr) || pathErr.Path != root {
				t.Fatalf("filesystem error identity lost: %v", err)
			}

			command := exec.Command(executable, "-test.run=^TestCommandHelper$", "--", "easyhog-command-helper", root)
			var stderr, stdout bytes.Buffer
			command.Stderr, command.Stdout = &stderr, &stdout
			err = command.Run()
			var exit *exec.ExitError
			if !errors.As(err, &exit) || exit.ExitCode() != 1 {
				t.Fatalf("expected exit 1, got %v", err)
			}
			text := stderr.String()
			if stdout.Len() != 0 || !strings.HasPrefix(text, "easyhog: ") || !strings.HasSuffix(text, "\n") {
				t.Fatalf("unexpected command output: stdout=%q stderr=%q", stdout.String(), text)
			}
			for _, r := range strings.TrimSuffix(text, "\n") {
				if unicode.IsControl(r) {
					t.Fatalf("raw control in diagnostic: %q", text)
				}
			}
			if strings.Contains(name, "\x1b") && !strings.Contains(text, `\x1b[2J`) {
				t.Fatalf("missing escaped ESC: %q", text)
			}
			if strings.Contains(name, "\n") && !strings.Contains(text, `\nforged diagnostic`) {
				t.Fatalf("missing escaped newline: %q", text)
			}
		})
	}
}

func TestUnknownOptionDiagnosticsEscapeControls(t *testing.T) {
	executable, err := os.Executable()
	if err != nil {
		t.Fatal(err)
	}
	for _, option := range []string{"-bad\x1b[2J", "-bad\nforged diagnostic", "-bad\x1b[2J\nforged diagnostic"} {
		t.Run(option, func(t *testing.T) {
			command := exec.Command(executable, "-test.run=^TestCommandHelper$", "--", "easyhog-command-helper", option)
			var stderr, stdout bytes.Buffer
			command.Stderr, command.Stdout = &stderr, &stdout
			err := command.Run()
			var exit *exec.ExitError
			if !errors.As(err, &exit) || exit.ExitCode() != 1 {
				t.Fatalf("expected exit 1, got %v", err)
			}
			text := stderr.String()
			if stdout.Len() != 0 || !strings.Contains(text, "easyhog: ") {
				t.Fatalf("unexpected output: stdout=%q stderr=%q", stdout.String(), text)
			}
			for _, r := range text {
				if unicode.IsControl(r) && r != '\n' {
					t.Fatalf("raw control in diagnostic: %q", text)
				}
			}
			if strings.Contains(option, "\x1b") && !strings.Contains(text, `\x1b[2J`) {
				t.Fatalf("missing escaped ESC: %q", text)
			}
			if strings.Contains(option, "\n") && (!strings.Contains(text, `\nforged diagnostic`) || strings.Contains(text, "\nforged diagnostic")) {
				t.Fatalf("unsafe newline: %q", text)
			}
		})
	}
}

func TestCommandHelpRemainsStatic(t *testing.T) {
	executable, err := os.Executable()
	if err != nil {
		t.Fatal(err)
	}
	command := exec.Command(executable, "-test.run=^TestCommandHelper$", "--", "easyhog-command-helper", "-h")
	var stderr, stdout bytes.Buffer
	command.Stderr, command.Stdout = &stderr, &stdout
	if err := command.Run(); err != nil {
		t.Fatal(err)
	}
	const help = "Usage: easyhog [-include dir] [-exclude dir] [directory]\n       easyhog -version\n\nList Groundhog instances beneath a directory (default: the current directory).\n-include and -exclude may repeat; an exclude is a directory name or a root-relative path.\n"
	if stdout.Len() != 0 || stderr.String() != help {
		t.Fatalf("unexpected help: stdout=%q stderr=%q", stdout.String(), stderr.String())
	}
}

func TestVersionPrintsTheStampedVersionAndRunsNothing(t *testing.T) {
	saved := version
	version = "1.2.3"
	t.Cleanup(func() { version = saved })
	out, err := os.CreateTemp(t.TempDir(), "stdout")
	if err != nil {
		t.Fatal(err)
	}
	defer out.Close()
	var stderr bytes.Buffer
	// No root is read: -version returns before discovery.
	if err := run(context.Background(), nil, []string{"-version", "/nowhere/at/all"}, nil, out, &stderr); err != nil {
		t.Fatal(err)
	}
	got, err := os.ReadFile(out.Name())
	if err != nil {
		t.Fatal(err)
	}
	if string(got) != "1.2.3\n" || stderr.Len() != 0 {
		t.Fatalf("stdout=%q stderr=%q", got, stderr.String())
	}
}

func TestShutdownWithNothingSupervisedSaysNothingAndDoesNotWait(t *testing.T) {
	sup, err := core.NewSupervisor(core.SupervisorConfig{Exec: runner.Process{}, Launcher: runner.Spawner{}, StateDir: t.TempDir()})
	if err != nil {
		t.Fatal(err)
	}
	var diagnostics bytes.Buffer
	done := make(chan struct{})
	go func() { shutdown(sup, nil, &diagnostics); close(done) }()
	select {
	case <-done:
	case <-time.After(5 * time.Second):
		t.Fatal("shutdown waited with nothing to stop")
	}
	if diagnostics.Len() != 0 {
		t.Errorf("stderr: %q", diagnostics.String())
	}
	if _, err := sup.Run(context.Background(), "/nowhere/.groundhog"); err == nil {
		t.Error("Run worked after shutdown")
	}
}

// Adoption's scope follows what discovery lists: beneath the root, or beneath an
// included directory (relative ones taken from the root). Nothing else.
func TestAdoptScopeCoversTheRootAndIncludedDirectoriesOnly(t *testing.T) {
	tmp := t.TempDir()
	root := filepath.Join(tmp, "repos")
	extra := filepath.Join(tmp, "extra")
	inScope := adoptScope(root, []string{extra, "vendored"})
	for path, want := range map[string]bool{
		filepath.Join(root, "a", ".groundhog"):             true,
		filepath.Join(root, "vendored", "b", ".groundhog"): true,
		filepath.Join(extra, "c", ".groundhog"):            true,
		filepath.Join(tmp, "elsewhere", "d", ".groundhog"): false,
		filepath.Join(tmp, "repos-two", "e", ".groundhog"): false, // a sibling sharing a name prefix
	} {
		if got := inScope(path); got != want {
			t.Errorf("inScope(%s) = %v, want %v", path, got, want)
		}
	}
}
