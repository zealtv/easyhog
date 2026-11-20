#!/bin/sh
# A deliberately failing Groundhog run. Each attempt appends "delivery-id attempt-id" to
# attempts.log in the project (its cwd), then exits 1 until a file named "fixed" exists
# there, when it exits 0. It shows `failed`, that Groundhog never starts it again by
# itself, and that `retry` makes a new attempt (a new attempt id) for the same delivery id.
set -eu
[ "$#" -eq 1 ] || exit 2
printf '%s %s\n' "${GROUNDHOG_DELIVERY_ID:?}" "${GROUNDHOG_ATTEMPT_ID:?}" >> attempts.log
if [ -e fixed ]; then
  exit 0
fi
echo "fail-until-fixed: no file named 'fixed' in $PWD; failing on purpose" >&2
exit 1
