#!/bin/sh
# A synthetic "agent" for the wake-adapter fixture. It calls no model. It writes a marker
# file recording its working directory and arguments, then sleeps so its pane stays alive.
# Usage (as the payload's `agent` line): wake-agent.sh MARKER_DIR   (the adapter appends
# its own arguments after that).
marks=${1:?marker directory}
shift
printf '%s\n' "$PWD" "$*" > "$marks/agent.$$"
exec sleep 600
