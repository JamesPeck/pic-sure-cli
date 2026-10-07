#!/usr/bin/env bash
# Two stacks side by side on one host (spec §6.5, §11): both inited with
# --auto-ports, demo data loaded into both at once, then destroying one
# leaves the other running with its data. Needs docker, git, go and jq;
# settings are in scripts/e2e-lib.sh.
#
# The two inits run at once, which checks that each reserves the ports it
# chose before the other chooses.

# shellcheck source=scripts/e2e-lib.sh
. "$(dirname "$0")/e2e-lib.sh"

base="${E2E_NAME:-e2e-two}"
a="$base-a" b="$base-b"
dir_a="$E2E_WORK/$a" dir_b="$E2E_WORK/$b"

say "init $a and $b at once"
# A background job's changes to the cleanup lists are lost, so they are
# made here.
e2e_stacks+=("$a" "$b")
e2e_dirs+=("$dir_a" "$dir_b")
init_stack "$a" "$dir_a" > "$E2E_WORK/init-$a.log" 2>&1 &
pid_a=$!
init_stack "$b" "$dir_b" > "$E2E_WORK/init-$b.log" 2>&1 &
pid_b=$!
rc_a=0 rc_b=0
wait "$pid_a" || rc_a=$?
wait "$pid_b" || rc_b=$?
cat "$E2E_WORK/init-$a.log" "$E2E_WORK/init-$b.log" >&2
[ "$rc_a" -eq 0 ] || fail "$a: init exited $rc_a"
[ "$rc_b" -eq 0 ] || fail "$b: init exited $rc_b"

ports_a="$(pic --stack "$dir_a" config get network.http_port)/$(pic --stack "$dir_a" config get network.https_port)"
ports_b="$(pic --stack "$dir_b" config get network.http_port)/$(pic --stack "$dir_b" config get network.https_port)"
echo "ports: $a $ports_a, $b $ports_b" >&2
[ "$ports_a" != "$ports_b" ] || fail "both stacks got ports $ports_a"

say "data demo nhanes into both at once"
pic --stack "$dir_a" data demo nhanes --heap "$E2E_LOAD_HEAP_MB" > "$E2E_WORK/demo-$a.log" 2>&1 &
pid_a=$!
pic --stack "$dir_b" data demo nhanes --heap "$E2E_LOAD_HEAP_MB" > "$E2E_WORK/demo-$b.log" 2>&1 &
pid_b=$!
rc_a=0 rc_b=0
wait "$pid_a" || rc_a=$?
wait "$pid_b" || rc_b=$?
cat "$E2E_WORK/demo-$a.log" "$E2E_WORK/demo-$b.log" >&2
[ "$rc_a" -eq 0 ] || fail "$a: data demo exited $rc_a"
[ "$rc_b" -eq 0 ] || fail "$b: data demo exited $rc_b"

ready='.deep.data.ready == true and .deep.http.csp == "frontend"'
deep_status "$dir_a" "$ready" > /dev/null || fail "$a: status --deep: want data ready and a frontend CSP"
deep_status "$dir_b" "$ready" > /dev/null || fail "$b: status --deep: want data ready and a frontend CSP"

say "destroy $a; $b keeps running"
pic --stack "$dir_a" destroy --yes
assert_gone "$a"
deep_status "$dir_b" "$ready" > /dev/null || fail "$b: not ready after $a was destroyed"

say "destroy $b"
pic --stack "$dir_b" destroy --yes
assert_gone "$b"

say "e2e two stacks passed"
