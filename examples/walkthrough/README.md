# Walkthrough scripts

Two scripts for trying Easyhog without touching anything real. Both use only synthetic,
temporary data: the pinned Groundhog script (`internal/groundhog/testdata/upstream-1c68c9d/groundhog.sh`), the
plain consumers in [`../consumers/`](../consumers/README.md), and made-up repository names.

- `setup.sh [ROOT]` builds the root (alpha, team/bravo, charlie, contested, delta, legacy), prints its path and
  starts nothing. Open it with `bin/easyhog ROOT` and look around.
- `capture.sh [OUTDIR]` replays a short session against the real binary in a **private tmux server** and saves each
  screen at 160×45 and 80×24 (`.ansi.txt`, `.txt`, and `.png` when `ANSI2PNG` names an ANSI-to-PNG
  converter). It needs `bin/easyhog` (`make build`), tmux, bash and a few minutes, because Groundhog's runner
  cycles once a minute. It stops unless the tmux socket lies inside its own private directory, never touches the
  default tmux server, keeps tokens under a private `XDG_STATE_HOME`, and removes everything it made.

Neither script touches this repository's own `.groundhog/`, a real schedule or a real token.
