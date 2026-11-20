#!/usr/bin/env bash
# Builds a synthetic multi-repository root to try Easyhog on.
#
#   examples/walkthrough/setup.sh [ROOT]        ROOT defaults to a fresh temporary directory
#
# It prints the root's path (and nothing else on stdout) and what it made on stderr.
# Run it again for a fresh world: it refuses to reuse a directory that already exists.
#
# What it makes, each a small "repository" with a .groundhog/ instance:
#
#   alpha        items without a run: delivery only. An overdue one-shot, a far-future one-shot
#                and an hourly item.
#   team/bravo   items whose run is collect-note.sh (examples/runs), which takes RUN_DELAY
#                seconds, and four items that fire on the first tick.
#   charlie      an item whose run fails on purpose (fail-until-fixed.sh): Retry it once you
#                create charlie/fixed.
#   delta        ticked once from outside just now (as cron would): reads external for three
#                minutes, and Run ticks it from Easyhog too.
#   legacy       a script that predates the runtime (stands for an old copy): read-only.
#
# The instances run the pinned upstream Groundhog script,
# internal/groundhog/testdata/upstream-9317d4e/groundhog.sh (override with
# EASYHOG_GROUNDHOG_SH, any script with runtime protocol 2). Nothing here touches this
# repository's own .groundhog/ or a real schedule, and nothing ticks after setup: Easyhog
# does that, when you choose Run.
#
# Environment:
#   RUN_DELAY   seconds each of bravo's runs takes (default 8)
set -euo pipefail

here=$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd -P)
repo=$(cd "$here/../.." && pwd -P)
gh_src=${EASYHOG_GROUNDHOG_SH:-$repo/internal/groundhog/testdata/upstream-9317d4e/groundhog.sh}
delay=${RUN_DELAY:-8}
runs=$repo/examples/runs

[ -f "$gh_src" ] || { echo "setup.sh: no groundhog.sh at $gh_src" >&2; exit 1; }
if [ "$#" -ge 1 ]; then
  root=$1
else
  root=$(mktemp -d "${TMPDIR:-/tmp}/easyhog-walk.XXXXXX")
  rmdir "$root"
fi
case $root in /*) ;; *) root=$PWD/$root ;; esac
[ ! -e "$root" ] || { echo "setup.sh: $root already exists; remove it or choose another" >&2; exit 1; }
mkdir -p "$root"
root=$(cd "$root" && pwd -P)

# instance DIR: a repository with a fresh .groundhog/ holding the pinned script.
instance() {
  mkdir -p "$root/$1/.groundhog"
  cp "$gh_src" "$root/$1/.groundhog/groundhog.sh"
  chmod 755 "$root/$1/.groundhog/groundhog.sh"
  bash "$root/$1/.groundhog/groundhog.sh" init >/dev/null
}
# item DIR WHEN ID NOTE: schedule an item whose payload is a note and a prompt.
item() {
  bash "$root/$1/.groundhog/groundhog.sh" add "$2" "$3" >/dev/null
  local dir=$root/$1/.groundhog/schedule/$2/$3
  printf '%s\n' "$4" > "$dir/note.txt"
  printf '# %s\n\n%s\n' "$3" "$4" > "$dir/prompt.md"
}
# run DIR WHEN ID SCRIPT [DELAY]: give an item SCRIPT as its run, after DELAY seconds.
run() {
  local file=$root/$1/.groundhog/schedule/$2/$3/run
  { echo '#!/bin/sh'; [ -z "${5:-}" ] || echo "sleep $5"; sed 1d "$4"; } > "$file"
  chmod 755 "$file"
}

instance alpha
item alpha once overdue-reminder "Water the plants (an overdue one-shot: fires on the first tick)."
item alpha once/2099-01-01 far-future "A one-shot dated far ahead: never due in this walkthrough."
item alpha every/1h heartbeat "An hourly item: fires once in the current hour's slot."

instance team/bravo
for id in invoice-check backup-review newsletter-draft; do
  item team/bravo once $id "Bravo: $id."
  run team/bravo once $id "$runs/collect-note.sh" "$delay"
done
item team/bravo every/1h status-ping "Post the hourly status."
run team/bravo every/1h status-ping "$runs/collect-note.sh" "$delay"

instance charlie
item charlie once nightly-export "Export the ledger (this run fails on purpose)."
run charlie once nightly-export "$runs/fail-until-fixed.sh"

instance delta
item delta every/1h weekly-digest "A digest that cron looks after."
bash "$root/delta/.groundhog/groundhog.sh" tick >/dev/null 2>&1 # something outside Easyhog ticked it

# An old copy: well-formed trays, a script that knows no runtime commands.
mkdir -p "$root/legacy/.groundhog/schedule/daily/morning-nudge" "$root/legacy/.groundhog/out" "$root/legacy/.groundhog/fired"
printf 'Say good morning.\n' > "$root/legacy/.groundhog/schedule/daily/morning-nudge/note.txt"
printf '#!/bin/sh\necho "groundhog: unknown command: $1" >&2\nexit 2\n' > "$root/legacy/.groundhog/groundhog.sh"
chmod 755 "$root/legacy/.groundhog/groundhog.sh"

{
  echo "walkthrough root: $root"
  echo "  alpha, team/bravo, charlie, delta (ticked from outside), legacy"
  echo "  open it with: easyhog $root"
} >&2
printf '%s\n' "$root"
