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
	alpha   string // no run: delivery only; an overdue once item, a future one, a daily item
	bravo   string // collect-note.sh as each item's run; three overdue items and a daily item
	charlie string // daily items: fail-until-fixed.sh, and a run that fails once and then waits for a gate
	delta   string // ticked from outside Easyhog a moment before the session opens
	foxtrot string // a legacy (pre-runtime) script
	india   string // an older Groundhog: runtime protocol 1
	golf    string // a current script whose `status` fails
	hotel   string // malformed: script not executable, directories missing
	private string // an unreadable directory beneath the root
	gate    string // charlie's slow run waits for this file
}

// legacyStub is what a script that predates the runtime does: it knows nothing
// of `capabilities`. Every call is logged beside the repository so the test can
// show which commands Easyhog ever ran.
const legacyStub = `#!/usr/bin/env bash
echo "$*" >> "$(dirname "$0")/../calls.log"
echo "error: unknown command '$1'" >&2
exit 1
`

// protocol1Stub answers `capabilities` as Groundhog did before items carried
// their own run, and logs every call.
const protocol1Stub = `#!/usr/bin/env bash
echo "$*" >> "$(dirname "$0")/../calls.log"
if [ "$1" = capabilities ]; then
  echo '{"protocol":"groundhog-runtime","protocol_version":1,"capabilities":["status-json-v1","ownership-v1","runner-v1","pause-v1","consumer-v1","delivery-v1"]}'
  exit 0
fi
exit 9
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
// reader's first screen has to discover them all.
func (w *world) build(t *testing.T) *fixture {
	t.Helper()
	f := &fixture{gate: filepath.Join(w.tmp, "gate")}
	t.Cleanup(func() { _ = os.WriteFile(f.gate, nil, 0o644) }) // a waiting run ends with the test

	f.alpha = w.instance("alpha")
	w.item(f.alpha, overdue, "old-note", "alpha overdue note")
	// A payload file that would leave a canary if anything ever ran it: only an
	// executable named `run` runs.
	w.must(os.WriteFile(filepath.Join(f.alpha, "schedule", overdue, "old-note", "run-me.sh"),
		[]byte("#!/bin/sh\ntouch "+filepath.Join(w.tmp, "canary")+"\n"), 0o755))
	w.item(f.alpha, future, "future-note", "alpha future note")
	w.item(f.alpha, "daily", "day-note", "alpha daily note")

	f.bravo = w.instance("team/bravo")
	collect := w.example("collect-note.sh")
	for _, n := range []string{"b1", "b2", "b3"} {
		w.item(f.bravo, overdue, n, "bravo note "+n)
		w.run(f.bravo, overdue, n, collect)
	}
	w.item(f.bravo, "daily", "day-note", "bravo daily note")
	w.run(f.bravo, "daily", "day-note", collect)

	// Daily, not one-shots: a one-shot's source goes when it fires, and the
	// steps select these items' rows after they have fired.
	f.charlie = w.instance("charlie")
	w.item(f.charlie, "daily", "c1", "charlie note")
	w.run(f.charlie, "daily", "c1", w.example("fail-until-fixed.sh"))
	w.item(f.charlie, "daily", "c2", "charlie slow note")
	// Fails at once the first time; every later attempt waits for the gate, then succeeds.
	w.run(f.charlie, "daily", "c2", "#!/bin/sh\nif [ -e \"$1/tried\" ]; then\n  while [ ! -e '"+f.gate+"' ]; do sleep 0.1; done\n  exit 0\nfi\n: > \"$1/tried\"\necho 'failing once on purpose' >&2\nexit 1\n")

	f.delta = w.instance("delta")
	w.item(f.delta, "daily", "day-note", "delta daily note")
	w.hog(f.delta, "tick") // something outside Easyhog ticked it just now

	f.foxtrot = w.instance("foxtrot")
	w.must(os.WriteFile(filepath.Join(f.foxtrot, "groundhog.sh"), []byte(legacyStub), 0o755))

	f.india = w.instance("india")
	w.must(os.WriteFile(filepath.Join(f.india, "groundhog.sh"), []byte(protocol1Stub), 0o755))

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
	return f
}
