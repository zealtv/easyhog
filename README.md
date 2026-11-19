# Easyhog

A terminal viewer and controller for [Groundhog](https://github.com/zealtv/groundhog),
the file-based scheduler. Point Easyhog at a directory: it lists every Groundhog instance
(`.groundhog/` folder) beneath it, shows their schedules, deliveries and payloads, and
runs and stops their runners while it stays open.

```text
 Easyhog  5 instances · 1 running                                              "/work/projects"
━━ Instances 5 ━━━━━━━━━━━━━━━━━━━━━━━┬── Preview 1–17 of 25 ───────────────────────────────────
  ▸ ○ alpha           1 item  stopped │ ● ours/runner
  ▸ ○ contested       1 item  stopped │ running · running, inspected 09:30:15
❯ ▾ ● ours/runner   will run  running │
      ◌ tick        every 5m  pending │ /work/projects/ours/runner/.groundhog
  ▸ ○ slow            1 item  stopped │
  ▸ ◇ external       1 item  external │ Kind: owned by this session · Health: current
                                      │ Palette offers: Stop
                                      │
                                      │ Pending work: yes, this session's runner is running (no
                                      │ tick recorded yet).
                                      │ Ownership: this session (easyhog)
                                      │ Pause: not paused
                                      │ Runner: running
                                      │ Consumer: configured
                                      │
                                      │ Schedule: 1 item (0 due now, 0 already fired, 0 paused)
                                      │ Deliveries: 1 pending
 nothing runs until you choose it · : opens commands
 : commands   ↑↓ select   tab focus   ←→ fold   ⇧↑↓ scroll   ctrl+c quit
```

A 96×21 capture over made-up instances.

## Install

**With Go** (1.24 or later):

```sh
go install github.com/zealtv/easyhog/cmd/easyhog@latest
```

**From a clone.** `./install.sh` builds `bin/easyhog` and links it into `GOBIN`, or into
the directory you give it: `./install.sh ~/.local/bin`. Or use `make build`, which leaves
the binary in `bin/easyhog`, and `make install`, which copies it into `~/.local/bin`
(`PREFIX` changes that).

You also need [Groundhog](https://github.com/zealtv/groundhog) in the projects you want
to see. Easyhog runs on macOS and Linux only.

## Use

```sh
easyhog                               # instances beneath the current directory
easyhog ~/projects                    # instances beneath that directory
easyhog -exclude archive ~/projects   # skip a directory; -include adds one back
easyhog -version
```

Move with the arrow keys; `→` opens an instance to show its schedule items. `:` opens the
command palette, which holds every action: Run, Stop, Pause item, Retry failed, Refresh,
Quit and the rest. Type to narrow the list, `Enter` to run a command, `Esc` to close.
`ctrl+c` also quits. A stray letter never runs, stops or quits anything.

Easyhog is not a daemon. The runners you run live only while it stays open, and when you
quit it winds them down. To keep schedules running, leave it open in a spare terminal.

## Licence and contributing

GPL-3.0-or-later; see [LICENSE](LICENSE).

Issues are welcome. Pull requests aren't accepted for now.
