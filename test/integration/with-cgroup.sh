#!/usr/bin/env bash
# Test setup only; ooth never invokes this script or these management commands.
set -euo pipefail
if [[ $EUID != 0 || $# -lt 2 ]]; then
  echo 'Usage: sudo bash test/integration/with-cgroup.sh USER [--outside] COMMAND [ARGS...]' >&2
  exit 1
fi
test_user=$1
shift
outside=false
if [[ ${1:-} == --outside ]]; then outside=true; shift; fi
test_group=$(mktemp -d /sys/fs/cgroup/ooth-tests.XXXXXX)
original="/sys/fs/cgroup$(sed -n 's/^0:://p' /proc/self/cgroup)"
cleanup() {
  echo $$ > "$original/cgroup.procs"
  # This subtree was created by this script exclusively for its test command.
  echo 1 > "$test_group/cgroup.kill"
  find "$test_group" -depth -type d -exec rmdir '{}' \;
}
trap cleanup EXIT
chown "$test_user" "$test_group" "$test_group/cgroup.procs" "$test_group/cgroup.threads" "$test_group/cgroup.subtree_control"
if $outside; then
  runuser -u "$test_user" -- env OOTH_TEST_UNDELEGATED_GROUP="$test_group" "$@"
else
  echo $$ > "$test_group/cgroup.procs"
  runuser -u "$test_user" -- env OOTH_CGROUP_ROOT="$test_group" "$@"
fi
