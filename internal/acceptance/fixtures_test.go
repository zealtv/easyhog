//go:build unix

package acceptance

import (
	"os"
	"path/filepath"
	"testing"
)

// fixture names the synthetic repositories (each the .groundhog directory) and
// what each one is for. Every name is made up; nothing here is a real project.
type fixture struct {
	alpha    string // no consumer: delivery only; an overdue once item, a future one, an every/ item
	bravo    string // a plain-script consumer; three overdue items and an every/ item (the backlog)
	charlie  string // a consumer that fails on purpose
	delta    string // owned by another supervisor, its runner running
	echo     string // owned by another supervisor, its runner stopped
	foxtrot  string // a legacy (pre-runtime) script
	golf     string // a current script whose `status` fails
	hotel    string // malformed: script not executable, directories missing
	private  string // an unreadable directory beneath the root
	wake     *wakeFixture
	wakeInst string
}

// legacyStub is what a script that predates the runtime does: it knows nothing
// of `capabilities`. Every call is logged beside the repository so the test can
// show which commands Easyhog ever ran.
const legacyStub = `#!/usr/bin/env bash
echo "$*" >> "$(dirname "$0")/../calls.log"
echo "error: unknown command '$1'" >&2
exit 1
`

// brokenStatus wraps the real (renamed) script and makes only `status` fail.
const brokenStatus = `#!/usr/bin/env bash
here=$(cd "$(dirname "$0")" && pwd)
if [ "$1" = status ]; then
  echo "synthetic failure: status is unavailable" >&2
  exit 3
fi
exec bash "$here/real.sh" "$@"
`

// build creates the repositories. They are in place before Easyhog opens, so a
// reader's first screen has to discover them all. wake may be nil.
func (w *world) build(t *testing.T, wake *wakeFixture) *fixture {
	t.Helper()
	collect := repoFile(t, "examples", "consumers", "collect-note.sh")
	failing := repoFile(t, "examples", "consumers", "fail-until-fixed.sh")
	f := &fixture{wake: wake}

	f.alpha = w.instance("alpha")
	w.item(f.alpha, overdue, "old-note", "alpha overdue note")
	// A payload file that would leave a canary if anything ever ran it.
	w.must(os.WriteFile(filepath.Join(f.alpha, "schedule", overdue, "old-note", "run-me.sh"),
		[]byte("#!/bin/sh\ntouch "+filepath.Join(w.tmp, "canary")+"\n"), 0o755))
	w.item(f.alpha, future, "future-note", "alpha future note")
	w.item(f.alpha, "every/1h", "hourly", "alpha hourly note")

	f.bravo = w.instance("team/bravo")
	w.consumer(f.bravo, map[string]string{"COLLECT_DELAY": "2"}, "/bin/sh", collect)
	for _, n := range []string{"b1", "b2", "b3"} {
		w.item(f.bravo, overdue, n, "bravo note "+n)
	}
	w.item(f.bravo, "every/1h", "hourly", "bravo hourly note")

	f.charlie = w.instance("charlie")
	w.consumer(f.charlie, nil, "/bin/sh", failing)
	w.item(f.charlie, overdue, "c1", "charlie note")

	f.delta = w.instance("delta")
	w.consumer(f.delta, nil, "/bin/sh", collect)
	w.item(f.delta, overdue, "d1", "delta note")

	f.echo = w.instance("echo")
	w.item(f.echo, overdue, "e1", "echo note")
	tokens := filepath.Join(w.tmp, "other-tokens")
	w.must(os.MkdirAll(tokens, 0o700))
	// Enrolled and never run: an owner is recorded and no runner is alive.
	w.must(w.client(f.echo).Enroll(w.ctx, "stopped-supervisor", filepath.Join(tokens, "echo.token")))

	f.foxtrot = w.instance("foxtrot")
	w.must(os.WriteFile(filepath.Join(f.foxtrot, "groundhog.sh"), []byte(legacyStub), 0o755))

	f.golf = w.instance("golf")
	w.must(os.Rename(filepath.Join(f.golf, "groundhog.sh"), filepath.Join(f.golf, "real.sh")))
	w.must(os.WriteFile(filepath.Join(f.golf, "groundhog.sh"), []byte(brokenStatus), 0o755))

	// Malformed: the script is not executable and out/ and fired/ are missing.
	f.hotel = filepath.Join(w.root, "hotel", ".groundhog")
	w.must(os.MkdirAll(filepath.Join(f.hotel, "schedule"), 0o755))
	w.must(os.WriteFile(filepath.Join(f.hotel, "groundhog.sh"), []byte(legacyStub), 0o644))

	f.private = filepath.Join(w.root, "private")
	w.must(os.MkdirAll(filepath.Join(f.private, "inner"), 0o755))
	w.must(os.Chmod(f.private, 0o000))
	t.Cleanup(func() { _ = os.Chmod(f.private, 0o755) })

	if wake != nil {
		w.wakeRepo(t, f, wake)
	}
	return f
}
