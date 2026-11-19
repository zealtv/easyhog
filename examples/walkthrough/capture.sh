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
#   COLLECT_DELAY seconds bravo's consumer takes per delivery (default 6)
#
# Safety: every tmux call goes through tm(), which names a server with -L, points TMUX_TMPDIR
# at a private directory, unsets TMUX and TMUX_PANE, and the script stops unless that server's
# socket lies inside the private directory. The default tmux server is never called. Token and
# log files go to a private XDG_STATE_HOME next to the root, so nothing lands in your home.
# Runners belong to the temporary instances only and end with the session's Quit.
#
# It takes a few minutes: Groundhog's runner ticks once a minute, so running a stopped
# instance again waits for its first cycle.
set -uo pipefail

here=$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd -P)
repo=$(cd "$here/../.." && pwd -P)
BIN=${EASYHOG_BIN:-$repo/bin/easyhog}
ROOT=${WALK_ROOT:-/tmp/easyhog-walk}
OUT=${1:-./walkthrough-captures}
ANSI2PNG=${ANSI2PNG:-}
export COLLECT_DELAY=${COLLECT_DELAY:-6}
[ -x "$BIN" ] || { echo "capture.sh: no binary at $BIN (run make build)" >&2; exit 1; }
command -v tmux >/dev/null || { echo "capture.sh: tmux is not installed" >&2; exit 1; }
[ ! -e "$ROOT" ] || { echo "capture.sh: $ROOT exists; remove it or set WALK_ROOT" >&2; exit 1; }
mkdir -p "$OUT"; OUT=$(cd "$OUT" && pwd -P)

PRIV=$(mktemp -d /tmp/ehwt.XXXXXX) || exit 1
NAME=ehw$$
STATE=$ROOT-state
tm() { env -u TMUX -u TMUX_PANE TMUX_TMPDIR="$PRIV" tmux -L "$NAME" "$@"; }
cleanup() {
  tm kill-server 2>/dev/null
  pkill -f -- "$ROOT/" 2>/dev/null; sleep 0.3; pkill -9 -f -- "$ROOT/" 2>/dev/null
  rm -rf "$PRIV" "$ROOT" "$ROOT-aux" "$STATE"
}
trap cleanup EXIT
trap 'exit 130' INT TERM HUP

"$here/setup.sh" "$ROOT" >/dev/null 2>&1 || { echo "capture.sh: setup failed" >&2; exit 1; }
ROOT=$(cd "$ROOT" && pwd -P) # the canonical spelling is what the header shows
AUX=$ROOT-aux; STATE=$ROOT-state
mkdir -m 700 "$STATE"
W=160 H=45

tm new-session -d -s w -x $W -y $H "sleep 3600" || exit 1
sock=$(tm display-message -p '#{socket_path}')
case $sock in "$PRIV"/*|"$(cd "$PRIV" && pwd -P)"/*) ;; *) echo "capture.sh: tmux socket $sock is outside $PRIV; stopping" >&2; exit 1 ;; esac

start() { tm respawn-pane -k -t w "env XDG_STATE_HOME=$STATE '$BIN' $ROOT; echo '[easyhog exited]'; sleep 3600"; sleep 1; }
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
gh() { local d=$1; shift; bash "$ROOT/$d/.groundhog/groundhog.sh" "$@"; }

# 1. Open: nothing runs until a key press that selects or moves.
start; wait_for "6 instances"
sleep 2; snap 01-open both
# 2. Select (the first arrow key reads the instance under it), then expand, then an item.
key Up; wait_for "Palette offers: Run" && snap 02-select-inspect both
key Right; wait_for "every/1h/heartbeat" && snap 03-expand both
key Down; wait_for "Payload, read as text" && snap 04-item-preview both
key Left; key Left   # back to alpha, collapsed
# 3. The palette, then Run on alpha (no consumer: deliveries wait, pending).
key :; snap 05-palette-open both
typed ru; snap 06-palette-run both
key Enter; wait_for "Running" 3 && snap 07-running
wait_for "status shows it running" 20 && snap 08-run both
# 4. Another instance: Run team/bravo, then Stop while its first delivery is in flight (Stop waits for the call, then releases).
key End; wait_for "Consumer: configured" && snap 09-bravo-selected both
run_palette ru; wait_for "status shows it running" 20
wait_for "in flight" 20; run_palette st; wait_for "stopped and released" 40
snap 10-stop-backlog both
wait_for "3 pending, 1 accepted" 30; snap 11-stop-backlog-settled
# 5. Run again: the instance enrolls afresh and its first cycle hands the backlog over.
run_palette ru; wait_for "enrolled for this session" 10; snap 12-run-again both
wait_for "4 accepted" 150; snap 13-delivered both
# 6. charlie's consumer fails on purpose; the failure shows in the badge and the preview.
key Home; key Down; wait_for "charlie/.groundhog" && sleep 2
run_palette ru; wait_for "1 failed" 30; snap 14-failed-delivery both
# 7. A refused Run: contested looked free, then another tool enrolled it.
key Down; wait_for "Palette offers: Run" 15; sleep 0.5
mkdir -m 700 -p "$AUX"; gh contested enroll --owner-label other-tool --token-file "$AUX/contested.token" >/dev/null
run_palette ru; wait_for "refused" 10; snap 15-refusal both
# 8. Owned elsewhere (delta) and a legacy script: inspect only, nothing to Run.
key Down; wait_for "Ownership: another supervisor" 15; key :; typed run; snap 16-external-no-run both
key Escape; key Down; wait_for "Support:" 15; snap 17-legacy both
# 9. Migrating delta: its other supervisor lets go (by hand here), Refresh, then Run.
key Up; sleep 0.5
gh delta release --token-file "$AUX/delta.token" >/dev/null 2>&1
run_palette ref; wait_for "rescanning" 5 && snap 18-refresh both
wait_for "Palette offers: Run" 20; run_palette ru; wait_for "status shows it running" 20; snap 19-delta-migrated both
# 10. Quit from the palette: stop (each waits up to 30 s for its call, then SIGTERM), release, for every runner this session started.
palette quit; snap 20-palette-quit
key Enter; wait_for "[easyhog exited]" 90; snap 21-quit-shutdown
# 11. Reopen: nothing restarts. Easyhog sets no pause, so none is left, and explicit Run enrolls again.
start; sleep 2; snap 22-reopen both
key End; wait_for "Palette offers: Run" 15; snap 23-reopen-unowned both
run_palette ru; wait_for "status shows it running" 20; snap 24-reopen-run both
key C-c; wait_for "[easyhog exited]" 90
echo "done: captures are in $OUT" >&2
