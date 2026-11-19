// Command easyhog lists Groundhog instances beneath a directory and keeps their
// snapshots fresh. It starts no scheduled work. No script runs until the
// reader's first key press that selects or moves; after that only the selected
// instance's own script is run in the background, and every instance's on an
// explicit Refresh; redirected output runs no script at all.
//
// Whatever ends the terminal (ctrl+c, the palette's Quit, SIGINT, SIGTERM or
// SIGHUP), the same shutdown follows: the runners this session started are all
// stopped at once, each given up to 30 seconds to finish the consumer call it
// admitted before it gets SIGTERM, and then released (core.Supervisor.Quit);
// that includes runners adopted from a session that ended, which are stopped and
// released but never signalled. A persisted pause stays as it was. A
// further signal hurries it.
package main

import (
	"context"
	"errors"
	"flag"
	"fmt"
	"io"
	"os"
	"os/signal"
	"path/filepath"
	"runtime/debug"
	"strings"
	"sync"
	"syscall"
	"time"

	"github.com/charmbracelet/x/term"
	"github.com/zealtv/easyhog/internal/core"
	"github.com/zealtv/easyhog/internal/discovery"
	"github.com/zealtv/easyhog/internal/opener"
	"github.com/zealtv/easyhog/internal/payload"
	"github.com/zealtv/easyhog/internal/runner"
	"github.com/zealtv/easyhog/internal/tui"
)

// version is set at link time from VERSION (make build); otherwise it comes from
// the build info the toolchain stamps in, as EasyLoom's and EasyMoot's do.
var version = ""

func main() {
	// Not NotifyContext: the first signal ends the terminal, and a later one
	// hurries the shutdown that follows, so run needs to see each of them.
	sigs := make(chan os.Signal, 4)
	signal.Notify(sigs, os.Interrupt, syscall.SIGTERM, syscall.SIGHUP)
	err := run(context.Background(), sigs, os.Args[1:], os.Stdin, os.Stdout, os.Stderr)
	signal.Stop(sigs)
	if err != nil && !errors.Is(err, flag.ErrHelp) && !errors.Is(err, context.Canceled) {
		// Quote only at the terminal boundary; callers retain the wrapped error.
		fmt.Fprintf(os.Stderr, "easyhog: %q\n", err.Error())
		os.Exit(1)
	}
}

// run does the whole command. The first value on sigs ends the terminal like
// ctrl+c does; each value after that, while runners are being wound down,
// hurries that. A nil sigs never fires.
func run(parent context.Context, sigs <-chan os.Signal, args []string, input *os.File, output *os.File, stderr io.Writer) error {
	flags := flag.NewFlagSet("easyhog", flag.ContinueOnError)
	// Parse errors may contain untrusted argv. Main prints them safely; the
	// custom usage below writes only static text to stderr.
	flags.SetOutput(io.Discard)
	flags.Usage = func() {
		fmt.Fprintln(stderr, "Usage: easyhog [-include dir] [-exclude dir] [directory]\n       easyhog -version\n\nList Groundhog instances beneath a directory (default: the current directory).\n-include and -exclude may repeat; an exclude is a directory name or a root-relative path.")
	}
	var include, exclude stringList
	flags.Var(&include, "include", "extra directory to search, even under a pruned tree")
	flags.Var(&exclude, "exclude", "directory name or root-relative path to skip")
	showVersion := flags.Bool("version", false, "print the version and exit")
	if err := flags.Parse(args); err != nil {
		return err
	}
	if *showVersion {
		fmt.Fprintln(output, buildVersion())
		return nil
	}
	if flags.NArg() > 1 {
		return fmt.Errorf("expected at most one directory")
	}
	root := "."
	if flags.NArg() == 1 {
		root = flags.Arg(0)
	}
	ctx, endTerminal := context.WithCancel(parent)
	defer endTerminal()
	terminalOver := make(chan struct{})
	go func() {
		select {
		case <-sigs:
			endTerminal()
		case <-terminalOver:
		}
	}()
	var over sync.Once
	closeTerminal := func() { over.Do(func() { close(terminalOver) }) }
	defer closeTerminal()

	snapshot, err := core.Open(ctx, root, os.Stat)
	if err != nil {
		return err
	}
	// The supervisor enrolls only on an explicit Run, and adopts only the tokens
	// that ended sessions left (below, interactive sessions only). Its IsOurs is
	// how the refresher tells an owner this session holds a token for from
	// another's, and its Observe applies the ownership rule to each coherent
	// status the refresher reads. One client per instance, shared by both, so
	// they share each one's capability answer.
	clients := core.NewClients(runner.Process{})
	sup, err := core.NewSupervisor(core.SupervisorConfig{Exec: runner.Process{}, Launcher: runner.Spawner{}, Clients: clients})
	if err != nil {
		return err
	}
	defer func() { closeTerminal(); shutdown(sup, sigs, stderr) }()
	refresher, err := core.NewRefresher(core.Config{
		Root: snapshot.Root, FS: discovery.OS{}, Exec: runner.Process{}, Clients: clients,
		Discovery: discovery.Options{Include: include, Exclude: exclude},
		IsOurs:    sup.IsOurs, OnCoherent: sup.Observe,
	})
	if err != nil {
		return err
	}
	if err := refresher.Rescan(ctx); err != nil {
		return err
	}
	// Adoption takes only the tokens of instances this session looks at: beneath
	// its root, or beneath a directory it was told to include (discovery lists
	// both). A token for any other instance stays where it is, for a session on
	// that instance's own root.
	inScope := adoptScope(snapshot.Root, include)
	interactive := term.IsTerminal(input.Fd()) && term.IsTerminal(output.Fd())
	if interactive {
		// Tokens that ended sessions left come under this session before the first
		// frame, so their instances read as ours (and Quit winds them down). A
		// redirected, one-frame run adopts nothing: it runs no script and would
		// release what it took the moment it ended. Adopting runs no script.
		sup.Adopt(inScope)
	}
	// Run and Stop run through the supervisor off the render loop. When one
	// returns, the refresher re-reads that instance so the view reconciles with
	// protocol status; Quit, wherever it starts, ends in shutdown below.
	model := tui.NewWithData(tui.FromViewWith(refresher.View(), sup.Runner),
		tui.WithSelectionObserver(refresher.Select), tui.WithRefresh(refresher.Refresh),
		tui.WithFolderOpener(opener.Open), tui.WithPayloads(readPayload),
		tui.WithRunners(sup, refresher.Reinspect))
	if !interactive {
		// One frame from directory listings: no script runs.
		return tui.RunModel(ctx, model, input, output, false)
	}

	// The refresher runs until the terminal ends; Quit cancels its calls in flight.
	rctx, cancel := context.WithCancel(ctx)
	done := make(chan struct{})
	go func() { defer close(done); _ = refresher.Run(rctx) }()
	go func() { // a session that ended while this one runs leaves tokens too
		tick := time.NewTicker(core.DefaultDiscoverEvery)
		defer tick.Stop()
		for {
			select {
			case <-tick.C:
				sup.Adopt(inScope)
			case <-rctx.Done():
				return
			}
		}
	}()
	feed := make(chan tui.Data)
	go func() {
		for {
			select {
			case <-refresher.Changed():
			case <-sup.Changed(): // a runner ended on its own, or an enrollment settled
			case <-rctx.Done():
				return
			}
			select {
			case feed <- tui.FromViewWith(refresher.View(), sup.Runner):
			case <-rctx.Done():
				return
			}
		}
	}()
	err = tui.RunLive(ctx, model, input, output, true, feed)
	cancel()
	<-done
	return err
}

// shutdown winds down what this session started, after the terminal has been
// restored: stop every runner (each waits up to 30 s for the consumer call it
// admitted, then gets SIGTERM), then release. Another signal hurries it. Notices (a runner left owned, a token kept) go to stderr,
// quoted because they carry paths.
func shutdown(sup *core.Supervisor, sigs <-chan os.Signal, stderr io.Writer) {
	// Count what Quit has to wait for, not only runners: a first Run still
	// enrolling has no runner yet, and Quit settles it once the enroll ends.
	if n := sup.Stopping(); n > 0 {
		// The terminal is already restored, so this is what the reader sees while Quit waits.
		fmt.Fprintf(stderr, "easyhog: Stopping %d runner(s): waiting up to %s for work in flight, then stopping the runner and releasing (interrupt again to hurry)\n", n, core.DefaultQuitWait)
	}
	ctx, hurry := context.WithCancel(context.Background())
	defer hurry()
	finished := make(chan struct{})
	go func() {
		select {
		case <-sigs:
			hurry()
		case <-finished:
		}
	}()
	rep := sup.Quit(ctx) // also refuses further work, and settles an operation in flight
	close(finished)
	for _, notice := range rep.Notices {
		fmt.Fprintf(stderr, "easyhog: %q\n", notice)
	}
}

// readPayload reads a selected item's files as text; nothing in them is run.
func readPayload(ref tui.PayloadRef) string {
	return payload.Preview(payload.OS{}, ref.Instance, ref.Rel)
}

type stringList []string

func (l *stringList) String() string     { return strings.Join(*l, ",") }
func (l *stringList) Set(v string) error { *l = append(*l, v); return nil }

// adoptScope returns the test Adopt applies to the instance a token names: it
// lies beneath the root, or beneath an included directory (a relative one is
// taken from the root, as discovery does). It uses core.Within, core's one
// "beneath" test.
func adoptScope(root string, include []string) func(instance string) bool {
	dirs := []string{root}
	for _, inc := range include {
		if !filepath.IsAbs(inc) {
			inc = filepath.Join(root, inc)
		}
		dirs = append(dirs, inc)
	}
	return func(instance string) bool {
		for _, d := range dirs {
			if core.Within(d, instance) {
				return true
			}
		}
		return false
	}
}

// buildVersion is the version -version prints: the one stamped in at link time,
// else the module version a `go install` records, else dev with the commit.
func buildVersion() string {
	if version != "" {
		return version
	}
	info, ok := debug.ReadBuildInfo()
	if !ok {
		return "dev"
	}
	if info.Main.Version != "" && info.Main.Version != "(devel)" {
		return info.Main.Version
	}
	for _, setting := range info.Settings {
		if setting.Key == "vcs.revision" {
			return "dev+" + setting.Value
		}
	}
	return "dev"
}
