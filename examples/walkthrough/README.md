# Walkthrough scripts

Two scripts for trying Easyhog without touching anything real. Both use only synthetic,
temporary data: the pinned Groundhog script (`internal/groundhog/testdata/upstream-9317d4e/groundhog.sh`), the
example runs in [`../runs/`](../runs/README.md), and made-up repository names.

- `setup.sh [ROOT]` builds the root (alpha, team/bravo, charlie, delta, legacy), prints its path and ticks nothing
  after its own setup. Open it with `bin/easyhog ROOT` and look around.
- `capture.sh [OUTDIR]` replays a short session against the real binary in a **private tmux server** and saves each
  screen at 160×45 and 80×24 (`.ansi.txt`, `.txt`, and `.png` when `ANSI2PNG` names an ANSI-to-PNG
  converter). It needs `bin/easyhog` (`make build`), tmux, bash and a minute or two. It stops unless the tmux socket
  lies inside its own private directory, never touches the default tmux server, and removes everything it made.

A one-shot item (`once/…`) leaves the schedule when it fires, so it has no row in the tree afterwards: its delivery
shows in the instance's preview, with the path of its run's log (`out/<delivery>/.groundhog-log`), which Open folder
gets you to. Retry on the instance covers it.

Neither script touches this repository's own `.groundhog/` or a real schedule.
