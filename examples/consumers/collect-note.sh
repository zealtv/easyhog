#!/bin/sh
# A synchronous, non-AI Groundhog consumer: it copies the first line of the delivery's
# note.txt into notes.log in its working directory, once per delivery.
#
# It uses only what the runner gives every consumer: the absolute delivery directory as its
# one argument, its cwd, GROUNDHOG_DELIVERY_ID and its exit status. Exit 0 means "done";
# Groundhog records the delivery as accepted. It never changes the delivery directory.
#
# Deduplication: a line starts with the delivery id. A second call for the same id (after
# `retry`, or `recover --decision retry`) finds the line and changes nothing, so the effect
# happens once even though the handoff may run twice.
#
# COLLECT_DELAY (whole seconds, optional) pauses before the work, only so a fixture can stop
# the runner mid-call. A real consumer needs no such thing.
set -eu
[ "$#" -eq 1 ] || { echo "usage: collect-note.sh ABSOLUTE_DELIVERY_DIR" >&2; exit 2; }
case $1 in /*) ;; *) echo "collect-note: not an absolute path: $1" >&2; exit 2 ;; esac
id=${GROUNDHOG_DELIVERY_ID:?GROUNDHOG_DELIVERY_ID is not set}
[ -r "$1/note.txt" ] || { echo "collect-note: no note.txt in $1" >&2; exit 1; }

if [ -f notes.log ] && awk -F '\t' -v id="$id" '$1 == id { found = 1 } END { exit !found }' notes.log; then
  echo "collect-note: delivery $id already collected; nothing to do" >&2
  exit 0
fi
[ -z "${COLLECT_DELAY:-}" ] || sleep "$COLLECT_DELAY"
IFS= read -r note < "$1/note.txt" || true
printf '%s\t%s\n' "$id" "$note" >> notes.log
