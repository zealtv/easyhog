// Command tuicapture runs the shell on synthetic data so its screens can be
// captured in a terminal (for example under tmux). It is a test aid, not part
// of the product CLI, and touches no real instance: the instances are made up,
// and the payload files they show are written to a temporary directory that
// is removed on exit.
package main

import (
	"context"
	"flag"
	"fmt"
	"os"
	"strings"
	"time"

	"github.com/zealtv/easyhog/internal/core"
	"github.com/zealtv/easyhog/internal/payload"
	"github.com/zealtv/easyhog/internal/tui"
)

func main() {
	scenario := flag.String("scenario", "full",
		"synthetic data: full, empty, error, stale (older samples), or empty-view, crowded, legacy, failing, stale-read, external (ticked from outside, with Run answered by a made-up ticker), actions (Run and Stop answered by a made-up ticker), verdicts (one instance per pending-work verdict), rows (failed, paused with a backlog, interrupted, scheduled), items (Pause item and Unpause item: scheduled, paused, paused above, a clash, ticked from outside), countdown (item rows at each countdown distance, paused, fired, due now, held, no next_due), heartbeat (the mark at each level, and rows that get none)")
	flag.Parse()

	var model tui.Model
	if v, files, ok := view(*scenario); ok {
		dir, err := os.MkdirTemp("", "tuicapture-")
		if err != nil {
			fmt.Fprintf(os.Stderr, "tuicapture: %q\n", err.Error())
			os.Exit(1)
		}
		defer os.RemoveAll(dir)
		if err := writeFiles(dir, files); err != nil {
			fmt.Fprintf(os.Stderr, "tuicapture: %q\n", err.Error())
			os.Exit(1)
		}
		opts := []tui.Option{
			tui.WithPayloads(func(ref tui.PayloadRef) string {
				return payload.Preview(payload.OS{}, mapPath(dir, ref.Instance), ref.Rel)
			}),
			tui.WithFolderOpener(func(string) error { return nil }),
		}
		// The actions are offered, and answered by a made-up ticker.
		data := tui.FromViewWith(v, tickLookup)
		opts = append(opts, tui.WithControl(syntheticControl{}, nil))
		model = tui.NewWithData(data, opts...)
	} else {
		model = tui.NewWithData(tui.SampleData(*scenario))
	}
	if err := tui.RunModel(context.Background(), model, os.Stdin, os.Stdout, true); err != nil {
		fmt.Fprintf(os.Stderr, "tuicapture: %q\n", err.Error())
		os.Exit(1)
	}
}

// syntheticControl answers the actions without starting anything: "nolock"
// is refused as if its script had lost what ticking needs a moment ago, "slow"
// never answers (to capture the transitional state), and the rest succeed.
type syntheticControl struct{}

func (syntheticControl) Run(_ context.Context, key string) (core.RunResult, error) {
	switch {
	case strings.Contains(key, "/nolock/"):
		return core.RunResult{}, &core.RefusedError{Op: "run", Instance: key, Reason: core.RefusedUnsupported,
			Detail: "lacks required capabilities: handlers-v1"}
	case strings.Contains(key, "/slow/"):
		time.Sleep(time.Hour)
	}
	return core.RunResult{Instance: key, Ticked: true}, nil
}

func (syntheticControl) Stop(key string) core.StopResult {
	return core.StopResult{Instance: key, WasTicking: true}
}

func (syntheticControl) Retry(_ context.Context, key, _ string) (core.RetryResult, error) {
	return core.RetryResult{Instance: key, Started: []string{"out/beat-1"}}, nil
}

func (syntheticControl) SetPaused(context.Context, string, bool) error { return nil }

func (syntheticControl) SetItemPaused(_ context.Context, key, item string, pause bool) (core.ItemResult, error) {
	return core.ItemResult{Instance: key, Item: core.ItemIdentity(item), From: item, To: core.ItemTarget(item, pause), Paused: pause, Locked: true}, nil
}
