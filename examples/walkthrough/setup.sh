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
#   alpha        no consumer. An overdue one-shot, a far-future one-shot and an hourly item.
#   team/bravo   a plain-script consumer (collect-note.sh) that takes COLLECT_DELAY seconds
#                per delivery, and four items that fire on the first tick, so a Stop
#                leaves a backlog of pending deliveries.
#   charlie      a consumer that fails on purpose (fail-until-fixed.sh).
#   delta        owned by some other supervisor (an enrollment nobody runs; its token is kept
#                outside the root, so you can release it by hand afterwards).
#   contested    unowned and runnable, until you (or capture.sh) enroll it from outside while
#                Easyhog still shows it as free: the case of a refused Run.
#   legacy       a script that predates the runtime (stands for an old copy): read-only.
#
# The instances run the pinned upstream Groundhog script,
# internal/groundhog/testdata/upstream-1c68c9d/groundhog.sh (override with
# EASYHOG_GROUNDHOG_SH, any script with the runtime commands, consumer-v1 and delivery-v1).
# Nothing here touches this repository's own .groundhog/, a real schedule or a real token,
# and nothing starts a runner: Easyhog does that, when you choose Run.
#
# Environment:
#   COLLECT_DELAY   seconds bravo's consumer takes per delivery (default 8)
set -euo pipefail

here=$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd -P)
repo=$(cd "$here/../.." && pwd -P)
gh_src=${EASYHOG_GROUNDHOG_SH:-$repo/internal/groundhog/testdata/upstream-1c68c9d/groundhog.sh}
delay=${COLLECT_DELAY:-8}
consumers=$repo/examples/consumers

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
aux=$root-aux # the "other supervisor's" token lives here, outside the root and every instance
mkdir -m 700 "$aux"

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
# consumer DIR ENVJSON SCRIPT: configure a plain-script consumer; cwd ".." is the repository.
consumer() {
  mkdir -p "$root/$1/.groundhog/runtime"
  cat > "$root/$1/.groundhog/runtime/config.json" <<JSON
{
  "schema_version": 1,
  "consumer": {
    "argv": ["/bin/sh", "$3"],
    "cwd": "..",
    "env": $2
  }
}
JSON
}

instance alpha
item alpha once overdue-reminder "Water the plants (an overdue one-shot: fires on the first tick)."
item alpha once/2099-01-01 far-future "A one-shot dated far ahead: never due in this walkthrough."
item alpha every/1h heartbeat "An hourly item: fires once in the current hour's slot."

instance team/bravo
item team/bravo once invoice-check "Check this month's invoices."
item team/bravo once backup-review "Review last night's backup log."
item team/bravo once newsletter-draft "Draft the newsletter."
item team/bravo every/1h status-ping "Post the hourly status."
consumer team/bravo "{\"COLLECT_DELAY\": \"$delay\"}" "$consumers/collect-note.sh"

instance charlie
item charlie once nightly-export "Export the ledger (this consumer fails on purpose)."
consumer charlie "{}" "$consumers/fail-until-fixed.sh"

instance delta
item delta once weekly-digest "A digest that another supervisor looks after."
bash "$root/delta/.groundhog/groundhog.sh" enroll --owner-label other-supervisor --token-file "$aux/delta.token" >/dev/null

instance contested
item contested once shared-chore "A chore two supervisors might both want."

# An old copy: well-formed trays, a script that knows no runtime commands.
mkdir -p "$root/legacy/.groundhog/schedule/daily/morning-nudge" "$root/legacy/.groundhog/out" "$root/legacy/.groundhog/fired"
printf 'Say good morning.\n' > "$root/legacy/.groundhog/schedule/daily/morning-nudge/note.txt"
printf '#!/bin/sh\necho "groundhog: unknown command: $1" >&2\nexit 2\n' > "$root/legacy/.groundhog/groundhog.sh"
chmod 755 "$root/legacy/.groundhog/groundhog.sh"

{
  echo "walkthrough root: $root"
  echo "  alpha, team/bravo, charlie, contested, delta (owned elsewhere), legacy"
  echo "  delta's owner token, for releasing it by hand: $aux/delta.token"
  echo "  open it with: easyhog $root"
} >&2
printf '%s\n' "$root"
