# Example runs

Synthetic, runnable examples of what an item does when it fires. Since Groundhog 9317d4e
(runtime protocol 2), an item that holds an executable file named `run` says itself what
happens: any tick that fires the item starts that `run`, once, in a session of its own, and
a record inside the delivery says what became of it. Nothing here needs a model, an agent
SDK or an account: both examples are plain `/bin/sh` scripts.

Where the claims come from: Groundhog's runtime contract (protocol 2, in the Groundhog
repository) and the behavior of the pinned script that Easyhog's tests use. Treat them as
"last observed"; when a newer Groundhog disagrees, trust it. Paths below, such as
`/example/project`, are placeholders.

| File | What it is |
|---|---|
| `collect-note.sh` | Synchronous, useful, deduplicating: appends `delivery-id<TAB>note` to `notes.log` in the project. |
| `fail-until-fixed.sh` | Deliberate failure: logs each attempt to `attempts.log`, exits 1 until a file named `fixed` exists. |

Easyhog's acceptance session (`make acceptance`) puts both into items as their `run` and
drives them through the palette: Run, a run accepted, a run failed, Retry.

## Giving an item a run

```sh
cd /example/project
bash .groundhog/groundhog.sh add daily/09 collect
cp collect-note.sh .groundhog/schedule/daily/09/collect/run
chmod +x .groundhog/schedule/daily/09/collect/run
echo "say hello" > .groundhog/schedule/daily/09/collect/note.txt
```

- `run` gets one argument, the delivery's absolute path (under `.groundhog/out/`). Its cwd
  is the project, the directory that holds `.groundhog/`. Stdin is `/dev/null`; stdout and
  stderr go to `.groundhog-log` in the delivery. Its environment is the ticker's, plus
  `GROUNDHOG_INSTANCE`, `GROUNDHOG_DELIVERY_ID` and `GROUNDHOG_ATTEMPT_ID`.
- Exit 0 reads `accepted`; anything else `failed`. An exit status cannot prove the work
  worked; it is what Groundhog records.
- An item without a `run` is only delivered, as before: the delivery reads `delivered`.
- `run`, `.groundhog-record` and `.groundhog-log` are reserved names inside an item.
- Anything that ticks the instance starts runs: cron, `groundhog.sh run` in a terminal, or
  Easyhog's Run. Two tickers are fine: each occurrence fires once and each run starts once.
  A `git pull` that brings in a new `run` brings in code the next tick starts; the instance
  pause (`groundhog.sh pause`) holds every tick back.

## Failure, then Retry

`fail-until-fixed.sh` exits 1, so the delivery reads `failed`. Groundhog never starts it
again by itself. Create `fixed` in the project, then retry it, from Easyhog's palette
(Retry) or by hand:

```sh
bash .groundhog/groundhog.sh retry out/<delivery>
```

`retry` starts the run again now as a new attempt with the same delivery id. It exits 4
while that delivery's run is going (busy), 3 while the instance is paused and 5 when there
is nothing to run. A run that de-duplicates on the delivery id, as `collect-note.sh` does,
skips a retry once its effect happened; de-duplicate on the attempt id, or only on success,
if a retry should do the work again.

## Not covered here

Discovery, the TUI, and Run and Stop in Easyhog itself: see the [README](../../README.md) and
the [walkthrough](../walkthrough/), which uses these runs. Nothing in Easyhog's Go code reads
these files or knows their names.
