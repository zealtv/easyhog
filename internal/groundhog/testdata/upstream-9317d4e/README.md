# Pinned upstream Groundhog script (test fixture, protocol 2)

`groundhog.sh` here is a byte-for-byte copy of `.groundhog/groundhog.sh` from
the Groundhog source repository at commit `9317d4e72037f775de382573594b80e75e50c918`
(`git show 9317d4e:.groundhog/groundhog.sh`). SHA-256:
`5a961d101ffc6d9186b836bd1f140c4c78e0b92770d682858a5a799d0cc562c2`.

It speaks Groundhog's runtime protocol 2, in which an item carries its own
executable `run` and a record inside each delivery says what became of it. The
tests run against it; Easyhog reads no older protocol.

It is a test fixture: tests copy it into a temporary instance and run it there.
It is not Easyhog's own instance, and nothing here is installed or upgraded
anywhere. To move the pin, copy the new upstream script over this file and update
the commit and digest above and in any test that checks it, deliberately.
