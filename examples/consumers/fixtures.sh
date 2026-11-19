#!/usr/bin/env bash
# Runs the example consumers in this directory through a real Groundhog runner.
#
#   EASYHOG_GROUNDHOG_SH=/path/to/.groundhog/groundhog.sh \
#   [EASYHOG_WAKE_CONSUMER=/path/to/an/adapter/executable] \
#     examples/consumers/fixtures.sh        (or: make fixtures)
#
# Works under bash 3.2 (/bin/bash on macOS) and newer. Needs python3 (reads status JSON).
#
# EASYHOG_GROUNDHOG_SH  a groundhog.sh whose `capabilities --json` lists consumer-v1 and
#                       delivery-v1. The vendored .groundhog/groundhog.sh of this repository
#                       predates the runtime, so it does not qualify and is never used here.
#                       Unset or too old: the whole test skips (exit 0) with a message.
# EASYHOG_WAKE_CONSUMER an executable that wakes an agent in a tmux pane from a delivery
#                       holding prompt.md, agent, pane and tmux-socket files.
#                       Unset: only that part skips.
#                       Nothing here depends on it; it is configured like any other consumer.
#
# Everything happens in one temporary directory. Each scenario copies groundhog.sh into a
# fresh project there, so no real schedule, token or repository is touched. The tmux part
# uses a private TMUX_TMPDIR and a scratch server named with -L; every tmux call goes through
# tm(), and the test aborts unless that server's socket lies under the temporary directory.
# No model is called: the agent is wake-agent.sh, which writes a marker and sleeps.
set -uo pipefail

here=$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd)
GH_SRC=${EASYHOG_GROUNDHOG_SH:-}
WAKE=${EASYHOG_WAKE_CONSUMER:-}

skip() { echo "skip: fixtures not run: $*"; exit 0; }
[ -n "$GH_SRC" ] || skip "EASYHOG_GROUNDHOG_SH is not set (point it at a groundhog.sh with consumer-v1 and delivery-v1)"
[ -f "$GH_SRC" ] || skip "EASYHOG_GROUNDHOG_SH names no file: $GH_SRC"
command -v python3 >/dev/null 2>&1 || skip "python3 is not installed"

T=$(mktemp -d "${TMPDIR:-/tmp}/easyhog-fx.XXXXXX") || skip "cannot create a temporary directory"
T=$(cd "$T" && pwd -P)
RPIDS=""
cleanup() {
  local p
  for p in $RPIDS; do kill "$p" 2>/dev/null; done
  # Consumers run in sessions of their own; anything of ours has $T in its command line.
  pkill -f -- "$T/" 2>/dev/null
  if [ -n "${TM_NAME:-}" ]; then tm kill-server 2>/dev/null; fi
  sleep 0.3
  pkill -9 -f -- "$T/" 2>/dev/null
  rm -rf "$T"
}
trap cleanup EXIT
trap 'exit 130' INT TERM HUP

# A scratch copy decides the skip, so even a read-only probe never touches the source tree.
mkdir -p "$T/probe/.groundhog"
cp "$GH_SRC" "$T/probe/.groundhog/groundhog.sh"
caps=$("$BASH" "$T/probe/.groundhog/groundhog.sh" capabilities --json 2>/dev/null) \
  || skip "$GH_SRC does not answer 'capabilities --json' (a script that predates the runtime?)"
for c in consumer-v1 delivery-v1; do
  case $caps in *"\"$c\""*) ;; *) skip "$GH_SRC lacks the $c capability" ;; esac
done
rm -rf "$T/probe"

pass=0 fail=0
ok()    { pass=$((pass+1)); echo "ok   - $1"; }
bad()   { fail=$((fail+1)); echo "FAIL - $1"; }
check() { local d=$1; shift; if "$@"; then ok "$d"; else bad "$d"; fi; }
eq()    { if [ "$2" = "$3" ]; then ok "$1"; else bad "$1 (expected '$2', got '$3')"; fi; }
wait_for() { local i; for ((i = 0; i < $1 * 5; i++)); do "${@:2}" && return 0; sleep 0.2; done; return 1; }

# --- one scratch project per scenario ------------------------------------------------------
n=0 P="" TOK="" RPID=""
hog() { "$BASH" "$P/.groundhog/groundhog.sh" "$@"; }
new_project() {  # sets P and TOK; the project holds a private copy of groundhog.sh
  n=$((n+1)); P=$T/proj$n; TOK=$T/token$n
  mkdir -p "$P/.groundhog"
  cp "$GH_SRC" "$P/.groundhog/groundhog.sh"
  hog init >/dev/null
}
add_item() {  # add_item NAME NOTE: a one-shot item whose payload is note.txt
  hog add once "$1" >/dev/null
  printf '%s\n' "$2" > "$P/.groundhog/schedule/once/$1/note.txt"
}
write_config() {  # write_config ARGV_JSON ENV_JSON
  mkdir -p "$P/.groundhog/runtime"
  printf '{"schema_version":1,"consumer":{"argv":%s,"cwd":"..","env":%s}}\n' "$1" "$2" \
    > "$P/.groundhog/runtime/config.json"
}
sh_config() {  # sh_config SCRIPT [ENV_JSON]: the same shape for every plain script
  local envj=${2:-}
  [ -n "$envj" ] || envj='{}'
  cp "$here/$1" "$P/$1"
  write_config "[\"/bin/sh\",\"$P/$1\"]" "$envj"
}
release_quiet() { hog release --token-file "$TOK" >/dev/null 2>&1; }
enroll() { hog enroll --owner-label example-fixtures --token-file "$TOK" >/dev/null; }
start_runner() {  # the runner stays in the foreground of its own process; we poll its status
  hog run --token-file "$TOK" --interval-seconds 1 > "$P/runner.log" 2>&1 &
  RPID=$!; RPIDS="$RPIDS $RPID"
}
runner_gone() { ! kill -0 "$RPID" 2>/dev/null; }
stop_runner() {  # `stop`, then reap; returns the runner's exit status
  hog stop --token-file "$TOK" >/dev/null 2>&1
  wait_for 10 runner_gone || kill -9 "$RPID" 2>/dev/null
  wait "$RPID" 2>/dev/null
}
st() {  # st PYTHON_EXPR: evaluate an expression over the status document d
  hog status --json 2>/dev/null | python3 -I -c 'import json,sys
d=json.load(sys.stdin)
print(eval(sys.argv[1]))' "$1" 2>/dev/null
}
dv() {  # dv ITEM PYTHON_EXPR: evaluate over x, the delivery fired from schedule item ITEM
  hog status --json 2>/dev/null | python3 -I -c 'import json,sys
d=json.load(sys.stdin)
x=[y for y in d["deliveries"] if y["schedule_path"]=="schedule/once/"+sys.argv[1]][0]
print(eval(sys.argv[2]))' "$1" "$2" 2>/dev/null
}
is_state() { [ "$(dv "$1" 'x["state"]')" = "$2" ]; }
lines() { if [ -f "$1" ]; then wc -l < "$1" | tr -d ' '; else echo 0; fi; }

echo "== delivery only: no runtime/config.json, nothing is invoked"
new_project
add_item greet "synthetic note"
# A script that merely sits where a consumer might be expected must not be discovered.
for d in "$P" "$P/.groundhog"; do
  printf '#!/bin/sh\necho ran > "%s/canary"\n' "$T" > "$d/consume.sh"; chmod +x "$d/consume.sh"
done
hog capabilities --json >/dev/null; hog status --json >/dev/null
check "capabilities and status create no config" test ! -e "$P/.groundhog/runtime/config.json"
eq "status reports no consumer configured" False "$(st 'd["consumer"]["configured"]')"
eq "status reports nobody owns the instance" None "$(st 'd["owner"]')"
enroll; start_runner
check "the delivery registers" wait_for 15 is_state greet pending
eq "reason is no_consumer" no_consumer "$(dv greet 'x["reason"]')"
sleep 2.5   # a couple more cycles
eq "still pending after more cycles" pending "$(dv greet 'x["state"]')"
eq "no attempt was made" 0 "$(dv greet 'len(x["attempts"])')"
check "no consume.sh was run" test ! -e "$T/canary"
check "the delivery sits in out/" test -f "$P/.groundhog/out/greet-$(date +%F)/note.txt"
stop_runner
check "release succeeds with nothing in flight" release_quiet
check "no config was ever created" test ! -e "$P/.groundhog/runtime/config.json"

echo "== synchronous plain script: collect-note.sh"
new_project
add_item note-1 "first synthetic note"
add_item note-2 "second synthetic note"
sh_config collect-note.sh
enroll; start_runner
check "note-1 accepted" wait_for 20 is_state note-1 accepted
check "note-2 accepted" wait_for 20 is_state note-2 accepted
id1=$(dv note-1 'x["id"]'); id2=$(dv note-2 'x["id"]')
eq "notes.log has one line per delivery" 2 "$(lines "$P/notes.log")"
eq "line for note-1 holds its delivery id and note" "$id1	first synthetic note" "$(grep "^$id1" "$P/notes.log")"
eq "line for note-2 holds its delivery id and note" "$id2	second synthetic note" "$(grep "^$id2" "$P/notes.log")"
eq "one attempt, exit code 0" "1 0" "$(dv note-1 'str(len(x["attempts"]))+" "+str(x["attempts"][0]["exit_code"])')"
eq "execution stays unknown (exit 0 is not proof of the effect)" unknown "$(dv note-1 'x["execution"]')"
check "the delivery directory is untouched" test "$(cat "$P/.groundhog/out/note-1-$(date +%F)/note.txt")" = "first synthetic note"
sleep 2.5
eq "later cycles do not replay accepted deliveries" 2 "$(lines "$P/notes.log")"
eq "...and add no attempt" 1 "$(dv note-1 'len(x["attempts"])')"
stop_runner
check "release succeeds" release_quiet
# Deduplication, checked directly: the same delivery id twice changes nothing.
dd=$T/dedupe; mkdir -p "$dd/delivery"; echo "dedupe note" > "$dd/delivery/note.txt"
(cd "$dd" && GROUNDHOG_DELIVERY_ID=d-one /bin/sh "$here/collect-note.sh" "$dd/delivery" 2>/dev/null; rc1=$?
 GROUNDHOG_DELIVERY_ID=d-one /bin/sh "$here/collect-note.sh" "$dd/delivery" 2>/dev/null; rc2=$?
 GROUNDHOG_DELIVERY_ID=d-two /bin/sh "$here/collect-note.sh" "$dd/delivery" 2>/dev/null; rc3=$?
 echo "$rc1$rc2$rc3" > rcs)
eq "dedupe: three calls exit 0" 000 "$(cat "$dd/rcs")"
eq "dedupe: a repeated delivery id adds no line" 2 "$(lines "$dd/notes.log")"
(cd "$dd" && /bin/sh "$here/collect-note.sh" relative/path 2>/dev/null); eq "a relative delivery path is refused (exit 2)" 2 "$?"

echo "== deliberate failure, then retry"
new_project
add_item flaky "synthetic flaky note"
sh_config fail-until-fixed.sh
enroll; start_runner
check "the delivery fails" wait_for 20 is_state flaky failed
did=$(dv flaky 'x["id"]')
eq "recorded exit code 1" 1 "$(dv flaky 'x["attempts"][0]["exit_code"]')"
sleep 3   # at least two more cycles
eq "no automatic retry: still one attempt" 1 "$(dv flaky 'len(x["attempts"])')"
eq "...and the consumer ran once" 1 "$(lines "$P/attempts.log")"
hog retry --token-file "$TOK" --delivery "$did" >/dev/null 2>&1; rc=$?
check "retry is refused while the runner runs (got $rc)" test "$rc" -ne 0
stop_runner
hog retry --token-file "$TOK" --delivery "$did" >/dev/null 2>&1; eq "retry on a stopped runner succeeds" 0 "$?"
eq "the delivery is pending again, same id" "pending $did" "$(dv flaky 'x["state"]+" "+x["id"]')"
start_runner
check "the second attempt fails too" wait_for 20 is_state flaky failed
eq "two attempts, two distinct attempt ids" "2 2" "$(dv flaky 'str(len(x["attempts"]))+" "+str(len(set(a["id"] for a in x["attempts"])))')"
stop_runner
: > "$P/fixed"
hog retry --token-file "$TOK" --delivery "$did" >/dev/null 2>&1; eq "retry again after the fix" 0 "$?"
start_runner
check "the third attempt is accepted" wait_for 20 is_state flaky accepted
eq "attempts.log: 3 calls, one delivery id" "3 1" "$(awk '{a[$1]=1; n++} END {c=0; for (k in a) c++; print n, c}' "$P/attempts.log")"
eq "attempts.log: 3 distinct attempt ids" 3 "$(awk '{print $2}' "$P/attempts.log" | sort -u | wc -l | tr -d ' ')"
stop_runner
check "release succeeds" release_quiet

echo "== invalid configuration: the runner refuses, nothing ticks"
new_project
add_item untouched "synthetic note"
write_config '["collect-note.sh"]' '{}'   # relative argv[0]
enroll; start_runner
wait_for 10 runner_gone; wait "$RPID" 2>/dev/null; rc=$?
eq "run exits 2" 2 "$rc"
eq "the runner log says nothing was ticked" 1 "$(grep -c 'nothing was ticked' "$P/runner.log")"
eq "no delivery in out/" 0 "$(ls "$P/.groundhog/out" | wc -l | tr -d ' ')"
eq "no fired marker" 0 "$(find "$P/.groundhog/fired" -type f | wc -l | tr -d ' ')"
check "the schedule item is still there" test -f "$P/.groundhog/schedule/once/untouched/note.txt"
eq "status reports the config as invalid" "True False" "$(st 'str(d["consumer"]["configured"])+" "+str(d["consumer"]["valid"])')"
eq "status carries a consumer_config_invalid diagnostic" consumer_config_invalid "$(st 'd["diagnostics"][0]["code"]')"
sh_config collect-note.sh
start_runner
check "after the config is fixed the same item is delivered" wait_for 20 is_state untouched accepted
stop_runner
check "release succeeds" release_quiet

echo "== out collision: an existing out/ directory is never overwritten"
new_project
day=$(date +%F)
mkdir "$P/.groundhog/out/clash-$day"; echo "stale note" > "$P/.groundhog/out/clash-$day/note.txt"
add_item clash "fresh note"
sh_config collect-note.sh
enroll; start_runner
one_accepted() { [ "$(st 'len([x for x in d["deliveries"] if x["state"]=="accepted"])')" = 1 ]; }
check "one delivery is accepted" wait_for 20 one_accepted
eq "only one delivery exists" 1 "$(st 'len(d["deliveries"])')"
eq "it is the directory that was already there" legacy_out "$(st 'd["deliveries"][0]["origin"]')"
eq "status reports out_collision" out_collision "$(st '[x["code"] for x in d["diagnostics"] if x["code"]=="out_collision"][0]')"
eq "the existing payload won: its note was delivered" "stale note" "$(cut -f2 "$P/notes.log")"
check "the out/ payload was not overwritten" test "$(cat "$P/.groundhog/out/clash-$day/note.txt")" = "stale note"
eq "the firing was recorded" 1 "$(find "$P/.groundhog/fired" -type f | wc -l | tr -d ' ')"
check "the one-shot source was removed, as after any firing" test ! -e "$P/.groundhog/schedule/once/clash"
stop_runner
check "release succeeds" release_quiet

echo "== detached consumer: stop mid-call leaves uncertain and a barrier"
new_project
add_item detach-a "note a"
add_item detach-b "note b"
sh_config collect-note.sh '{"COLLECT_DELAY":"2"}'
enroll; start_runner
inflight() { [ "$(st 'len([x for x in d["deliveries"] if x["state"]=="in_flight"])')" = 1 ]; }
check "a delivery is in flight" wait_for 20 inflight
first=$(st '[x["id"] for x in d["deliveries"] if x["state"]=="in_flight"][0]')
stop_runner; eq "stop ends the runner with status 0" 0 "$?"
eq "the in-flight delivery is now uncertain" uncertain "$(st '[x["state"] for x in d["deliveries"] if x["id"]=="'"$first"'"][0]')"
eq "with error consumer_detached" consumer_detached "$(st '[x["attempts"][-1]["error"]["code"] for x in d["deliveries"] if x["id"]=="'"$first"'"][0]')"
collected() { grep -q "^$first" "$P/notes.log" 2>/dev/null; }
check "the detached consumer was not killed and finished its work" wait_for 10 collected
eq "the other delivery is still pending, never attempted" "pending 0" "$(st '[x["state"]+" "+str(len(x["attempts"])) for x in d["deliveries"] if x["id"]!="'"$first"'"][0]')"
start_runner; sleep 3.5
eq "the barrier holds across a runner restart: still pending" "pending 0" "$(st '[x["state"]+" "+str(len(x["attempts"])) for x in d["deliveries"] if x["id"]!="'"$first"'"][0]')"
eq "...and still one line in notes.log" 1 "$(lines "$P/notes.log")"
stop_runner
hog release --token-file "$TOK" >/dev/null 2>&1; rc=$?
check "release is refused while an attempt is unresolved (got $rc)" test "$rc" -ne 0
hog recover --token-file "$TOK" --delivery "$first" --decision retry >/dev/null 2>&1; eq "recover --decision retry succeeds" 0 "$?"
start_runner
both() { [ "$(st 'len([x for x in d["deliveries"] if x["state"]=="accepted"])')" = 2 ]; }
check "both deliveries are accepted after recovery" wait_for 25 both
eq "the retried delivery kept its id and has two attempts" 2 "$(st '[len(x["attempts"]) for x in d["deliveries"] if x["id"]=="'"$first"'"][0]')"
eq "notes.log: the effect happened once per delivery" 2 "$(lines "$P/notes.log")"
eq "...and once for the retried id" 1 "$(grep -c "^$first" "$P/notes.log")"
stop_runner
check "release succeeds once nothing is unresolved" release_quiet

# --- the wake adapter, through the same config shape -------------------------------
echo "== wake adapter (same runtime/config.json shape)"
if [ -z "$WAKE" ]; then
  echo "skip: EASYHOG_WAKE_CONSUMER is not set; wake-adapter fixture not run"
elif [ ! -x "$WAKE" ]; then
  echo "skip: EASYHOG_WAKE_CONSUMER is not an executable file: $WAKE; wake-adapter fixture not run"
elif ! command -v tmux >/dev/null 2>&1; then
  echo "skip: tmux is not installed; wake-adapter fixture not run"
else
  export TMUX_TMPDIR=$T/tmux; mkdir -p "$TMUX_TMPDIR"
  unset TMUX TMUX_PANE
  TM_NAME=efx$$
  tm() { env -u TMUX -u TMUX_PANE tmux -L "$TM_NAME" -f /dev/null "$@"; }
  tm new-session -d -s main -x 100 -y 30 "sleep 600" || { echo "cannot start the scratch tmux server"; exit 1; }
  sock=$(tm display -p '#{socket_path}')
  case $sock in
    "$T"/*) ok "scratch tmux server socket lies under the temporary directory" ;;
    *) echo "ABORT: tmux socket '$sock' is outside $T; refusing to continue"; tm kill-server 2>/dev/null; exit 1 ;;
  esac
  pane=$(tm new-window -d -P -F '#{pane_id}' "sleep 601")
  mkdir -p "$T/marks"

  new_project
  cp "$here/wake-agent.sh" "$P/wake-agent.sh"
  hog add once wake-demo >/dev/null
  d=$P/.groundhog/schedule/once/wake-demo
  echo "# synthetic resume prompt" > "$d/prompt.md"
  echo "$P/wake-agent.sh $T/marks" > "$d/agent"
  echo "$pane" > "$d/pane"
  echo "$sock" > "$d/tmux-socket"
  write_config "[\"$WAKE\"]" \
    "{\"GROUNDHOG_WAKE_STATE_DIR\":\"$T/wake-state\",\"GROUNDHOG_WAKE_QUIET\":\"2\",\"GROUNDHOG_WAKE_PATIENCE\":\"60\",\"GROUNDHOG_WAKE_POLL\":\"1\"}"
  enroll; start_runner
  check "the wake delivery is accepted" wait_for 40 is_state wake-demo accepted
  wid=$(dv wake-demo 'x["id"]')
  eq "one attempt, exit code 0, execution unknown" "1 0 unknown" "$(dv wake-demo 'str(len(x["attempts"]))+" "+str(x["attempts"][0]["exit_code"])+" "+x["execution"]')"
  # Exit 0 only means the adapter's tmux call succeeded; the pane's process starts a moment later.
  have_marker() { ls "$T"/marks/agent.* >/dev/null 2>&1; }
  check "the synthetic agent ran" wait_for 10 have_marker
  marker=$(ls "$T"/marks/agent.* 2>/dev/null | head -n1)
  eq "...in the project directory" "$P" "$(head -n1 "$marker" 2>/dev/null)"
  check "...with a prompt path as its argument" grep -q "resume prompt in " "$marker"
  check "the pane id survived (respawned in place)" bash -c "tmux -L '$TM_NAME' list-panes -a -F '#{pane_id}' | grep -qx '$pane'"
  check "the adapter recorded the launch under its own state, keyed by delivery id" grep -q "^delivery=$wid" "$T"/wake-state/*/"$wid"/launched
  check "nothing was written under Groundhog's runtime/ by the adapter" test ! -e "$P/.groundhog/runtime/wake-consumer"
  stop_runner
  check "release succeeds" release_quiet
  tm kill-server 2>/dev/null
fi

echo
echo "passed $pass, failed $fail"
[ "$fail" -eq 0 ]
