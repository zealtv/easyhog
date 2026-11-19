# Pinned upstream Groundhog script (test fixture)

`groundhog.sh` here is a byte-for-byte copy of `.groundhog/groundhog.sh` from
the Groundhog source repository at commit `1c68c9d4fdab3559775883f6c61036d4b272cda9`
(`git show 1c68c9d:.groundhog/groundhog.sh`). SHA-256:
`298c9d5d11465607796043d44c5e8dc42a20912aa52c00988505aaf8be09dbb9`.

It is a test fixture for `internal/groundhog`'s integration tests, which copy it
into a temporary instance and run read-only `capabilities` and `status` against
it, plus `enroll`, `pause`, `resume`, `stop` and `release` on that temporary
instance only. Other packages' tests and the acceptance session run real
runners on it, in temporary instances too.

It is not Easyhog's own instance: this repository's `.groundhog/` holds its own
vendored copy and the tests never read or write it. Nothing here is installed or
upgraded anywhere. To move the pin, copy the new upstream script over this file
and update the commit and digest above and in `integration_test.go`, deliberately.
