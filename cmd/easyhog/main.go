// Command easyhog lists Groundhog instances beneath a directory and keeps their
// snapshots fresh. It starts no scheduled work. No script runs until the
// reader's first key press that selects or moves; after that only the selected
// instance's own script is run in the background, and every instance's on an
// explicit Refresh; redirected output runs no script at all.
//
// Run ticks an instance from this session, one bounded `groundhog.sh tick`
// every minute, until Stop or Quit. Whatever ends the terminal (ctrl+c, the
// palette's Quit, SIGINT, SIGTERM or SIGHUP), the same shutdown follows: this
// session stops ticking (core.Ticker.Quit), letting a tick already under way
// finish. Runs the ticks started go on in their own sessions, and nothing is
// signalled, released or waited for beyond that. A further signal stops the
// wait for a tick under way.
package main

import (
	"context"
	"errors"
	"flag"
	"fmt"
	"io"
	"os"
	"os/signal"
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
// ctrl+c does; each value after that, while Quit waits for a tick under way,
// ends that wait. A nil sigs never fires.
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
	// The ticker ticks only what the reader runs. One client per instance,
	// shared with the refresher, so they share each one's capability answer.
	clients := core.NewClients(runner.Process{})
	tk, err := core.NewTicker(core.TickerConfig{Clients: clients})
	if err != nil {
		return err
	}
	defer func() { closeTerminal(); shutdown(tk, sigs, stderr) }()
	refresher, err := core.NewRefresher(core.Config{
		Root: snapshot.Root, FS: discovery.OS{}, Exec: runner.Process{}, Clients: clients,
		Discovery: discovery.Options{Include: include, Exclude: exclude},
	})
	if err != nil {
		return err
	}
	if err := refresher.Rescan(ctx); err != nil {
		return err
	}
	interactive := term.IsTerminal(input.Fd()) && term.IsTerminal(output.Fd())
	// The palette's actions run through the ticker off the render loop. When one
	// returns, the refresher re-reads that instance so the view reconciles with
	// protocol status; Quit, wherever it starts, ends in shutdown below.
	model := tui.NewWithData(tui.FromViewWith(refresher.View(), tk.Info),
		tui.WithSelectionObserver(refresher.Select), tui.WithRefresh(refresher.Refresh),
		tui.WithFolderOpener(opener.Open), tui.WithPayloads(readPayload),
		tui.WithControl(tk, refresher.Reinspect))
	if !interactive {
		// One frame from directory listings: no script runs.
		return tui.RunModel(ctx, model, input, output, false)
	}

	// The refresher runs until the terminal ends; Quit cancels its calls in flight.
	rctx, cancel := context.WithCancel(ctx)
	done := make(chan struct{})
	go func() { defer close(done); _ = refresher.Run(rctx) }()
	feed := make(chan tui.Data)
	go func() {
		for {
			select {
			case <-refresher.Changed():
			case <-tk.Changed(): // a tick finished, or ticking started or stopped
			case <-rctx.Done():
				return
			}
			select {
			case feed <- tui.FromViewWith(refresher.View(), tk.Info):
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

// shutdown stops this session's ticking, after the terminal has been restored.
// It waits only for a tick already under way, which a further signal cuts
// short. It says nothing unless that wait passes about a second, since nothing
// is left half done: runs go on in their own sessions.
func shutdown(tk *core.Ticker, sigs <-chan os.Signal, stderr io.Writer) {
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
	quiet := time.AfterFunc(quitNotice, func() {
		fmt.Fprintln(stderr, "easyhog: waiting for a tick under way; ctrl+c leaves now")
	})
	tk.Quit(ctx) // also refuses further work
	quiet.Stop()
	close(finished)
}

// quitNotice is how long Quit waits for a tick under way before it says so.
const quitNotice = time.Second

// readPayload reads a selected item's files, and its newest runs' logs, as
// text; nothing in them is run.
func readPayload(ref tui.PayloadRef) string {
	return payload.ItemPreview(payload.OS{}, ref.Instance, ref.Rel, ref.Logs)
}

type stringList []string

func (l *stringList) String() string     { return strings.Join(*l, ",") }
func (l *stringList) Set(v string) error { *l = append(*l, v); return nil }

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
