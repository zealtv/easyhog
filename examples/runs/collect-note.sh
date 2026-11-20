#!/bin/sh
# A synchronous, non-AI Groundhog run: it copies the first line of the delivery's note.txt
# into notes.log in the project (its cwd), once per delivery.
#
# Put it in an item as its `run` (executable). It uses only what Groundhog gives every run:
# the delivery's absolute path as its one argument, the project as its cwd,
# GROUNDHOG_DELIVERY_ID, and its exit status. Exit 0 reads "accepted". It never changes the
# delivery directory.
#
# Deduplication: a line starts with the delivery id. A `retry` keeps the delivery id, so a
# second run for the same delivery finds its line and changes nothing: the effect happens
# once, however often it runs.
set -eu
[ "$#" -eq 1 ] || { echo "usage: run ABSOLUTE_DELIVERY_DIR" >&2; exit 2; }
case $1 in /*) ;; *) echo "collect-note: not an absolute path: $1" >&2; exit 2 ;; esac
id=${GROUNDHOG_DELIVERY_ID:?GROUNDHOG_DELIVERY_ID is not set}
[ -r "$1/note.txt" ] || { echo "collect-note: no note.txt in $1" >&2; exit 1; }

if [ -f notes.log ] && awk -F '\t' -v id="$id" '$1 == id { found = 1 } END { exit !found }' notes.log; then
  echo "collect-note: delivery $id already collected; nothing to do"
  exit 0
fi
IFS= read -r note < "$1/note.txt" || true
printf '%s\t%s\n' "$id" "$note" >> notes.log
