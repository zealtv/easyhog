# Example consumers

Synthetic, runnable examples of how Groundhog hands a fired item to a consumer, and what
you see when it works, fails or stops halfway. Nothing here needs a model, an agent SDK or
an account: the consumers are plain `/bin/sh` scripts that use only their argument, their
working directory, a few environment variables and their exit status.

Where the claims come from: Groundhog's accepted runtime contract (the runtime contract
document in the Groundhog repository) and the behavior of the pinned Groundhog script, as
exercised by `fixtures.sh`. Treat them as "last observed". When a newer Groundhog
disagrees, trust it and the test that says so. All paths below, such as
`/example/project`, are placeholders.

| File | What it is |
|---|---|
| `collect-note.sh` | Synchronous, useful, deduplicating consumer: appends `delivery-id<TAB>note` to `notes.log` in its cwd. |
| `fail-until-fixed.sh` | Deliberate failure: logs each call to `attempts.log`, exits 1 until a file named `fixed` exists. |
| `wake-agent.sh` | A fake agent (writes a marker, sleeps) for the optional wake-adapter example. |
| `fixtures.sh` | The test: runs every scenario below against a real Groundhog runner in a temporary directory. |

## Run the test

```sh
EASYHOG_GROUNDHOG_SH=/path/to/groundhog/.groundhog/groundhog.sh \
EASYHOG_WAKE_CONSUMER=/path/to/an/adapter/executable \
  make fixtures
```

- `EASYHOG_GROUNDHOG_SH` names a `groundhog.sh` whose `capabilities --json` lists
  `consumer-v1` and `delivery-v1`. This repository's vendored `.groundhog/groundhog.sh`
  predates the runtime and does not qualify; the test never uses it. When the variable is
  unset, or the script lacks those capabilities, the test prints why and exits 0 at once.
- `EASYHOG_WAKE_CONSUMER` is optional. It names a wake adapter, an executable that
  launches an agent in a tmux pane for a delivery holding `prompt.md`, `agent`, `pane` and
  `tmux-socket` files. Unset, only that scenario is skipped.
  This repository does not depend on any adapter.
- Needs `bash` 3.2 or newer (`/bin/bash` on macOS and a newer bash both work), `python3` for
  reading `status --json`, and `tmux` for the wake scenario. A whole run takes about a minute,
  most of it waiting for one-second runner cycles.
- Each scenario copies the script into a fresh project under a temporary directory, so no
  real schedule or token is touched. The tmux scenario uses a private `TMUX_TMPDIR` and a
  scratch server selected with `-L`, aborts unless that server's socket lies under the
  temporary directory, and never calls a bare `tmux`. The trap removes everything it started.

## Setting up a consumer

A consumer exists only when you write `.groundhog/runtime/config.json`. Nothing creates it
for you, and a script that happens to sit in a project or schedule (a `consume.sh`, say) is
never discovered or run.

```json
{
  "schema_version": 1,
  "consumer": {
    "argv": ["/bin/sh", "/example/project/collect-note.sh"],
    "cwd": "..",
    "env": {}
  }
}
```

- `argv` is a nonempty array of literal strings, never shell text. `argv[0]` must be an
  absolute path to an executable. Using `/bin/sh` plus a script path lets the script stay
  non-executable.
- `cwd` is relative to the instance directory (`..` is the project that holds `.groundhog/`).
  The consumer starts there, and its relative paths resolve there.
- The runner appends one argument: the absolute delivery directory (under `.groundhog/out/`).
  Stdin is `/dev/null`. The environment is the runner's, then `env`, then
  `GROUNDHOG_INSTANCE`, `GROUNDHOG_DELIVERY_ID` and `GROUNDHOG_ATTEMPT_ID`.
- A plain script and an adapter use exactly this shape. The wake scenario differs from
  the others only in `argv` and `env`.
- Exit 0 is the whole acknowledgement: the delivery becomes `accepted`. Anything else
  (nonzero, signal, failure to start) records `failed`. Groundhog still reports
  `execution: unknown`, because an exit status cannot prove that the work worked.
- Consumers must not move, delete or change the delivery directory. Read it or copy from it.
- Changing the config takes effect only after the runner stops and starts again.

The runner needs an owner. A project holding this repository's scenarios goes through the
same steps your own would:

```sh
cd /example/project
bash .groundhog/groundhog.sh enroll --owner-label demo --token-file /example/private/token
bash .groundhog/groundhog.sh run --token-file /example/private/token --interval-seconds 60   # foreground
# from a second terminal:
bash .groundhog/groundhog.sh status --json
bash .groundhog/groundhog.sh stop --token-file /example/private/token
bash .groundhog/groundhog.sh release --token-file /example/private/token
```

`run` does not
enroll and never clears a pause. Start it in a foreground shell: a background job of a
non-interactive shell ignores Ctrl-C. Easyhog itself is one ordinary client of these
commands. Discovery of an instance only reads: it never enrolls, writes a config,
installs a hook or starts an agent.

## Scenarios

Each heading matches a section of `fixtures.sh`.

### Delivery only (no consumer)

Leave `runtime/config.json` absent. An enrolled, running instance still fires items into
`out/` and registers each as a delivery in state `pending` with reason `no_consumer`. Nothing
is invoked, no matter how many cycles pass. Writing a config later and starting the runner
again makes those pending deliveries eligible.

### Synchronous work: `collect-note.sh`

For each delivery it copies the first line of the payload's `note.txt` into `notes.log` in
the project (its cwd), prefixed by `GROUNDHOG_DELIVERY_ID`, and exits 0. The delivery goes
`pending → in_flight → accepted`, once; later cycles and runner restarts do not replay it.
The payload filename `note.txt` is this example's private convention, not a Groundhog field.

It deduplicates on the delivery id: a line starting with that id means the effect already
happened, so a second call for the same id changes nothing and exits 0. That matters because
Groundhog promises no exactly-once effects; a retry after a lost result may call it again.

### Deliberate failure, then retry: `fail-until-fixed.sh`

Exit 1 records `failed`. Later cycles do not try again by themselves. `retry` turns a
failed delivery back into `pending`, keeps its delivery id, and makes a new attempt (new
`GROUNDHOG_ATTEMPT_ID`) at the next run:

```sh
bash .groundhog/groundhog.sh stop    --token-file TOKEN       # retry needs a stopped runner
bash .groundhog/groundhog.sh retry   --token-file TOKEN --delivery DELIVERY_ID
bash .groundhog/groundhog.sh run     --token-file TOKEN ...
```

`retry` against a live runner exits 4. If a failed attempt may already have had side
effects, reconcile them before retrying; the consumer's own deduplication is the safety net.

### Invalid configuration

A config the runner cannot use (for example a relative `argv[0]`, a missing executable or
cwd) is not the same as no consumer. `run` exits 2 before its first tick: no firing, no
delivery, no fired marker, and the schedule item stays where it was. `status --json` shows
`consumer.valid: false` and a `consumer_config_invalid` diagnostic naming the problem. Fix
the file and start the runner again.

### Out collision

If `out/<item>-<date>` already exists when an item fires, Groundhog never overwrites it. On
`run` it first imports the existing directory as a delivery (`origin: legacy_out`), then
records the firing without a second delivery, and `status` adds an `out_collision`
diagnostic. The consumer is handed the **existing** directory's payload. The fresh
payload is not delivered, and a one-shot item is removed after firing as usual, so look at
`out_collision` diagnostics rather than assuming both payloads arrived.

### A consumer that outlives its runner

`stop`, Ctrl-C or SIGTERM on the runner detaches from a consumer call that is in progress.
Groundhog does not kill it. The delivery becomes `uncertain` (error `consumer_detached`; a
`kill -9` of the runner leaves `in_flight`, which the next `run` turns into `uncertain`).
An unresolved `in_flight` or `uncertain` delivery blocks every consumer handoff on that
instance, for other deliveries and under a changed config, across runner restarts. Ticking
still registers new deliveries. `release` is refused (exit 5) until it is resolved.

To resolve it, first establish for yourself that the original call has finished (the runner
being gone proves nothing), then with the runner stopped and the token in hand:

```sh
bash .groundhog/groundhog.sh recover --token-file TOKEN --delivery DELIVERY_ID --decision accept
# or, after reconciling side effects:
bash .groundhog/groundhog.sh recover --token-file TOKEN --delivery DELIVERY_ID --decision retry
```

`accept` records that the work was done; `retry` makes the delivery pending again with the
same id. The scenario uses `retry` on `collect-note.sh`: the consumer already finished, so
the second call finds its id in `notes.log` and the effect still happens once.

### Orchestrator wake without a paid model

With `EASYHOG_WAKE_CONSUMER` set, the test configures that executable through the same
`runtime/config.json` shape. `argv` holds only its path, and `env` holds the adapter's own
settings (a state directory under the temporary directory and short quiet/patience/poll
times). The payload is whatever the adapter documents; here it is `prompt.md`, an `agent` file
naming `wake-agent.sh`, a `pane` on the scratch tmux server and a `tmux-socket` naming that
server. Groundhog does not read any of these files. The test checks the same observable
lifecycle as for any consumer (one attempt, exit 0, `accepted`, `execution: unknown`) plus
what only this adapter promises: the fake agent ran in the project directory with a prompt
path, and the pane id survived. A real setup replaces `agent` with the real command; the
Groundhog side stays identical.

Adapters can take a long time to return (this one waits for its pane to go quiet). While
a call waits, that instance's later deliveries wait too.

## Not covered here

Discovery, the TUI, and Run/Stop in Easyhog itself: see the [README](../../README.md) and the
[walkthrough](../walkthrough/), which uses these consumers. Nothing in Easyhog's Go code reads these files or
knows any consumer's name.
