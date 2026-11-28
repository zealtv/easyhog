# Changelog

## v0.3.1 (2026-11-28)

- Easyhog has a glance for [Tireless Watcher](https://github.com/zealtv/tireless-watcher); the README says so in one line.
- Inside tmux, the `@tw-idle` option replaces `@tw-next` for the map of windows.
- The README drops its detail on the glance.

## v0.3.0 (2026-11-27)

Easyhog reports what it sees to a map of tmux windows, and its palette moves to `>`.

- `>` opens the command palette, as in the other easy tools; `:` no longer does.
- Inside tmux, Easyhog keeps options on its own pane for a map of windows, and unsets them on Quit:
  - `@tw-focus`: where the instance list is, so a small live preview of the pane shows the list;
  - `@tw-next`: the next wake among the instances it ticks (`report at 10:30 · alpha`);
  - `@tw_status` `fail`, with the reason in `@tw_status_text`, while one of their wakes has failed;
  - a glance: `@tw-glance-far`, a badge line of instances it ticks, ticked from outside and failed,
    and `@tw-glance-mid`, one row per ticked instance.
- Instances you have already opened this session are re-read about once a minute, so one ticked from
  outside, or one this session ticks but isn't showing, stays current. An instance never opened is
  still never run.

## v0.2.0 (2026-11-20)

Easyhog moves to Groundhog's runtime protocol 2, in which a schedule item can carry its own executable
`run`. **v0.2.0 needs Groundhog 9317d4e or later**; an older Groundhog shows read-only. **v0.1.0 needs the
older Groundhog** (runtime protocol 1): keep v0.1.0 if you can't upgrade Groundhog yet.

- Run now means this Easyhog ticks the instance once a minute while it is open; Stop means it stops.
  Nothing is enrolled, owned, adopted or released any more. Quit just stops ticking: runs a tick started
  carry on in their own sessions.
- New palette actions: Pause instance and Unpause instance (Groundhog's instance pause, which holds back
  every ticker of the instance), and one Retry for failed and interrupted deliveries, in place of Retry
  failed, Recover as done and Recover and retry.
- An instance something else ticks (cron, `groundhog.sh run`) reads `external`, and Run no longer asks
  before ticking it too.
- The preview shows each delivery's state (delivered, pending, running, accepted, failed, interrupted,
  unrecorded), an item's `run` as text, and its newest runs' output. Nothing is run to show it.
- Easyhog keeps no state of its own any more. v0.1.0's session folders can be deleted: `easyhog/` under
  `$XDG_STATE_HOME`, or else `~/Library/Application Support/easyhog` on macOS and `~/.local/state/easyhog`
  on Linux.
- `examples/consumers/` is now `examples/runs/`, and `make fixtures` is gone.

## v0.1.0 (2026-11-19)

The first public release.

- Lists the Groundhog instances beneath a directory (with `-include` and `-exclude`) and shows each one's
  schedule, deliveries and payloads.
- Runs and stops an instance's Groundhog runner while Easyhog stays open. Quit winds down every runner
  it ran.
- Pauses and unpauses items, and counts each item row down to its next firing.
- Retries failed deliveries and recovers uncertain ones, from the command palette.
- `-version` prints the version.
- macOS and Linux.
