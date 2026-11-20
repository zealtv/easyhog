#!/usr/bin/env bash
# Drives the real easyhog binary through a walkthrough of setup.sh's root in a PRIVATE tmux
# server and saves what the screen showed at each step, at 160x45 and at 80x24.
#
#   examples/walkthrough/capture.sh [OUTDIR]       OUTDIR defaults to ./walkthrough-captures
#
# Environment:
#   EASYHOG_BIN   the binary (default: bin/easyhog; run `make build` first)
#   WALK_ROOT     the synthetic root (default /tmp/easyhog-walk: short, so tmux socket paths
#                 and the screens stay short; it must not exist, and is removed at the end)
#   ANSI2PNG      a tool taking IN.ansi.txt OUT.png (any ANSI-to-PNG converter); without
#                 it only the .ansi.txt (with colour) and .txt (plain) captures are written
#   RUN_DELAY     seconds each of bravo's runs takes (default 6)
#
# Safety: every tmux call goes through tm(), which names a server with -L, points TMUX_TMPDIR
# at a private directory, unsets TMUX and TMUX_PANE, and the script stops unless that server's
# socket lies inside the private directory. The default tmux server is never called. Runs
# belong to the temporary instances only; the cleanup ends any still going.
#
# It takes a minute or two: Easyhog ticks an instance once a minute, and Run ticks at once.
set -uo pipefail

here=$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd -P)
repo=$(cd "$here/../.." && pwd -P)
BIN=${EASYHOG_BIN:-$repo/bin/easyhog}
ROOT=${WALK_ROOT:-/tmp/easyhog-walk}
OUT=${1:-./walkthrough-captures}
ANSI2PNG=${ANSI2PNG:-}
export RUN_DELAY=${RUN_DELAY:-6}
[ -x "$BIN" ] || { echo "capture.sh: no binary at $BIN (run make build)" >&2; exit 1; }
command -v tmux >/dev/null || { echo "capture.sh: tmux is not installed" >&2; exit 1; }
[ ! -e "$ROOT" ] || { echo "capture.sh: $ROOT exists; remove it or set WALK_ROOT" >&2; exit 1; }
mkdir -p "$OUT"; OUT=$(cd "$OUT" && pwd -P)

PRIV=$(mktemp -d /tmp/ehwt.XXXXXX) || exit 1
NAME=ehw$$
tm() { env -u TMUX -u TMUX_PANE TMUX_TMPDIR="$PRIV" tmux -L "$NAME" "$@"; }
cleanup() {
  tm kill-server 2>/dev/null
  pkill -f -- "$ROOT/" 2>/dev/null; sleep 0.3; pkill -9 -f -- "$ROOT/" 2>/dev/null
  rm -rf "$PRIV" "$ROOT"
}
trap cleanup EXIT
trap 'exit 130' INT TERM HUP

"$here/setup.sh" "$ROOT" >/dev/null 2>&1 || { echo "capture.sh: setup failed" >&2; exit 1; }
ROOT=$(cd "$ROOT" && pwd -P) # the canonical spelling is what the header shows
W=160 H=45

tm new-session -d -s w -x $W -y $H "sleep 3600" || exit 1
sock=$(tm display-message -p '#{socket_path}')
case $sock in "$PRIV"/*|"$(cd "$PRIV" && pwd -P)"/*) ;; *) echo "capture.sh: tmux socket $sock is outside $PRIV; stopping" >&2; exit 1 ;; esac

start() { tm respawn-pane -k -t w "'$BIN' $ROOT; echo '[easyhog exited]'; sleep 3600"; sleep 1; }
screen() { tm capture-pane -p -t w; }
# key K...: one key at a time, as a reader types (several at once arrive as one paste-like event).
key() { local k; for k in "$@"; do tm send-keys -t w "$k"; sleep 0.2; done; }
typed() { local i; for ((i = 0; i < ${#1}; i++)); do key "${1:i:1}"; done; }
palette() { key :; typed "$1"; }
run_palette() { palette "$1"; key Enter; }
# wait_for TEXT [SECONDS]: poll the screen until it shows TEXT.
wait_for() {
  local t=${2:-20} n=0
  while ! screen | grep -qF -- "$1"; do
    sleep 0.25; n=$((n + 1))
    [ $n -lt $((t * 4)) ] || { echo "capture.sh: gave up waiting for: $1" >&2; screen >&2; return 1; }
  done
}
# trim: drop blank lines at the end of a capture (a shell screen after Quit is mostly empty).
trim() { awk '{ a[NR] = $0 } END { n = NR; while (n > 0 && a[n] ~ /^[ ]*$/) n--; for (i = 1; i <= n; i++) print a[i] }'; }
# snap NAME [both]: save the screen as NAME-160x45 and, for "both", NAME-80x24 as well.
snap() {
  local name=$1 sizes=${2:-wide} s w h
  [ "$sizes" = both ] && sizes="wide narrow" || sizes=wide
  for s in $sizes; do
    if [ $s = narrow ]; then w=80 h=24; tm resize-window -t w -x $w -y $h; sleep 0.6; else w=$W h=$H; fi
    tm capture-pane -p -e -t w | trim > "$OUT/$name-${w}x${h}.ansi.txt"
    tm capture-pane -p -t w | trim > "$OUT/$name-${w}x${h}.txt"
    [ -z "$ANSI2PNG" ] || "$ANSI2PNG" "$OUT/$name-${w}x${h}.ansi.txt" "$OUT/$name-${w}x${h}.png" $w $h >/dev/null 2>&1
    if [ $s = narrow ]; then tm resize-window -t w -x $W -y $H; sleep 0.6; fi
  done
  echo "captured $name" >&2
}

# 1. Open: nothing runs until a key press that selects or moves.
start; wait_for "5 instances"
sleep 2; snap 01-open both
# 2. Select (the first arrow key reads the instance under it), then expand, then an item.
key Up; wait_for "Palette offers: Run" && snap 02-select-inspect both
key Right; wait_for "every/1h/heartbeat" && snap 03-expand both
key Down; wait_for "Payload, read as text" && snap 04-item-preview both
key Left; key Left   # back to alpha, collapsed
# 3. The palette, then Run on alpha (no run in its items: they are delivered only).
key :; snap 05-palette-open both
typed ru; snap 06-palette-run both
key Enter; wait_for "ticking every minute" 20 && snap 07-run both
# 4. Run team/bravo: each item's run (collect-note.sh) starts in its own session.
key End; wait_for "team/bravo/.groundhog" && sleep 1; snap 08-bravo-selected both
key Right; key Down; wait_for "Runs when the item fires" 10 && snap 09-bravo-run-text both
key Left; key Left
run_palette ru; wait_for "running" 20; snap 10-bravo-running both
wait_for "4 accepted" 60; snap 11-bravo-accepted both
# 5. Pause instance holds every tick back; Unpause lets it go.
run_palette "pause in"; wait_for "paused. No tick" 10; snap 12-paused both
run_palette unpause; wait_for "unpaused" 10
# 6. charlie's run fails on purpose; the failure shows in the row, the badge and the preview.
key Home; key Down; wait_for "charlie/.groundhog" && sleep 2
run_palette ru; wait_for "1 failed" 30; snap 13-failed both
touch "$ROOT/charlie/fixed"; run_palette ret; wait_for "started again" 10; snap 14-retry both
# 7. delta was ticked from outside: it reads external, and Run asks nothing.
key Down; wait_for "ticked from outside" 15; snap 15-external both
# 8. A legacy script: inspect only, nothing to Run.
key Down; wait_for "Support:" 15; snap 16-legacy both
# 9. Quit from the palette: Easyhog stops ticking; runs already going carry on.
palette quit; snap 17-palette-quit
key Enter; wait_for "[easyhog exited]" 30; snap 18-quit
# 10. Reopen: nothing ticks until Run.
start; sleep 2; snap 19-reopen both
key End; wait_for "Palette offers: Run" 15; snap 20-reopen-run-offered both
key C-c; wait_for "[easyhog exited]" 30
echo "done: captures are in $OUT" >&2
