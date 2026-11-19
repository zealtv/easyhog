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
		"synthetic data: full, empty, error, stale (older samples), or empty-view, crowded, legacy, failing, inconsistent, external (both kinds of external, with Run answered by made-up runners), actions (Run and Stop answered by made-up runners), verdicts (one instance per pending-work verdict), rows (failed, paused with a backlog, uncertain, scheduled), items (Pause item and Unpause item: scheduled, paused, paused above, a clash, external), countdown (item rows at each countdown distance, paused, fired, due now, held, no next_due), heartbeat (the mark at each level, and rows that get none)")
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
		data := tui.FromView(v)
		if *scenario == "actions" || *scenario == "verdicts" || *scenario == "rows" || *scenario == "items" || *scenario == "external" {
			// Run and Stop are offered, and answered by made-up runners (also for verdicts).
			data = tui.FromViewWith(v, func(string) (core.RunnerInfo, bool) { return core.RunnerInfo{}, false })
			opts = append(opts, tui.WithRunners(syntheticRunners{}, nil))
		}
		model = tui.NewWithData(data, opts...)
	} else {
		model = tui.NewWithData(tui.SampleData(*scenario))
	}
	if err := tui.RunModel(context.Background(), model, os.Stdin, os.Stdout, true); err != nil {
		fmt.Fprintf(os.Stderr, "tuicapture: %q\n", err.Error())
		os.Exit(1)
	}
}

// syntheticRunners answers Run, Stop and the settling actions without starting anything: "contested"
// is refused as if another supervisor had enrolled it a moment ago, "slow" never
// answers (to capture the transitional state), and the rest succeed.
type syntheticRunners struct{}

func (syntheticRunners) Run(_ context.Context, key string) (core.RunResult, error) {
	switch {
	case strings.Contains(key, "/contested/"):
		return core.RunResult{}, &core.RefusedError{Op: "run", Instance: key, Reason: core.RefusedExternal,
			Detail: `owner "team-supervisor" is recorded; ownership and runner liveness are separate, and a stopped owner is not available for takeover`}
	case strings.Contains(key, "/slow/"):
		time.Sleep(time.Hour)
	}
	return core.RunResult{Instance: key, Outcome: core.RunStarted, Enrolled: true, PID: 40213, Seen: true}, nil
}

func (syntheticRunners) Stop(_ context.Context, key string) (core.StopResult, error) {
	return core.StopResult{Instance: key}, nil
}

func (syntheticRunners) SetItemPaused(_ context.Context, key, item string, pause bool) (core.ItemResult, error) {
	return core.ItemResult{Instance: key, Item: core.ItemIdentity(item), From: item, To: core.ItemTarget(item, pause), Paused: pause, Locked: true}, nil
}

func (syntheticRunners) Settle(_ context.Context, key string, req core.SettleRequest) (core.SettleResult, error) {
	return core.SettleResult{Instance: key, Action: req.Action, Settled: []string{"d-1"}, StoppedRunner: true, Restarted: true, PID: 40214, Seen: true}, nil
}
