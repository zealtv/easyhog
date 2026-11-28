# Easyhog

A terminal viewer and controller for [Groundhog](https://github.com/zealtv/groundhog),
the file-based scheduler. Point Easyhog at a directory: it lists every Groundhog instance
(`.groundhog/` folder) beneath it, shows their schedules, deliveries and payloads, and
ticks the ones you run while it stays open.

```text
 Easyhog  5 instances · 1 running                                              "/work/projects"
━━ Instances 5 ━━━━━━━━━━━━━━━━━━━━━━━┬── Preview 1–17 of 27 ───────────────────────────────────
  ▸ ○ alpha           1 item  stopped │ ● team/worker
  ▸ ○ nolock          1 item  stopped │ running • · inspected 09:30:15
❯ ▾ ● team/worker   1 item  running • │
      ◌ tick        every 5m  pending │ /work/projects/team/worker/.groundhog
  ▸ ○ slow            1 item  stopped │
  ▸ ◇ external     1 item  external • │ Kind: supported · Health: current
                                      │ Palette offers: Stop, Pause instance
                                      │ This session ticks it every 1m00s; its last tick began
                                      │ at 09:30:03. Runs start in sessions of their own and
                                      │ outlive Easyhog.
                                      │
                                      │ Pending work: yes, this session ticks it; last tick 12s
                                      │ before the 09:30:15 read.
                                      │ Heartbeat: fresh, judged against a tick expected every
                                      │ 1m00s (the interval this session ticks it at): fresh up
                                      │ to 1m30s, late up to 3m00s.
                                      │ Pause: not paused
 nothing runs until you choose it · > opens commands
 > commands   ↑↓ select   tab focus   ←→ fold   ⇧↑↓ scroll   ctrl+c quit
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
to see: a copy with runtime protocol 2 (Groundhog 9317d4e or later), in which an item can
carry its own `run`. Easyhog shows an older copy read-only. Easyhog runs on macOS and Linux
only.

## Use

```sh
easyhog                               # instances beneath the current directory
easyhog ~/projects                    # instances beneath that directory
easyhog -exclude archive ~/projects   # skip a directory; -include adds one back
easyhog -version
```

Move with the arrow keys; `→` opens an instance to show its schedule items. `>` opens the
command palette, which holds every action: Run, Stop, Pause instance, Pause item, Retry,
Refresh, Quit and the rest. Type to narrow the list, `Enter` to run a command, `Esc` to close.
`ctrl+c` also quits. A stray letter never runs, stops or quits anything.

Run means this Easyhog ticks the instance, once a minute, while it stays open; Stop and
Quit stop ticking. A tick fires what is due and starts each item's `run` in a session of its
own, so runs carry on after Easyhog quits. Easyhog is not a daemon: to keep schedules
firing, leave it open in a spare terminal, or tick from cron or `groundhog.sh run`, which
Easyhog shows beside its own.

Easyhog has a glance for [Tireless Watcher](https://github.com/zealtv/tireless-watcher).

## Licence and contributing

GPL-3.0-or-later; see [LICENSE](LICENSE).

Issues are welcome. Pull requests aren't accepted for now.
