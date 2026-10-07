#!/usr/bin/env bash
# The e2e core sequence (spec §11) against this host's Docker: init (open
# mode, generated client secret) → data demo nhanes → status --deep →
# update (a no-op) → reset → up → destroy. Needs docker, git, go and jq.
# The nightly e2e workflow runs it; settings are in scripts/e2e-lib.sh.

# shellcheck source=scripts/e2e-lib.sh
. "$(dirname "$0")/e2e-lib.sh"

name="${E2E_NAME:-e2e-core}"
dir="$E2E_WORK/$name"

init_stack "$name" "$dir"

say "data demo nhanes"
pic --stack "$dir" data demo nhanes --heap "$E2E_LOAD_HEAP_MB"

say "status --deep: data ready, CSP from the frontend"
deep_status "$dir" '.deep.data.ready == true and .deep.http.csp == "frontend"' > /dev/null ||
	fail "status --deep: want data ready and a frontend CSP"

say "update is a no-op"
release="$(jq -r '.release.commit' "$dir/.pic-sure/state.json")"
# Pinned to the recorded release, so a release-control push during the run
# doesn't make it a real update.
result="$(pic --stack "$dir" --json update --release-commit "$release" | tail -n 1)"
jq -e '
	.type == "result" and .ok and (.data | (
		(.config.migrations | length) == 0 and .release.from == .release.to and
		(.restarts | length) == 0 and .migrations.status == "up_to_date" and
		(.token.renew | not) and all(.components[]; .changed | not) and
		all(.images[]; .action != "build" and .action != "pull")))' <<< "$result" > /dev/null ||
	fail "update wasn't a no-op: $result"

say "reset, then up"
pic --stack "$dir" reset --yes
pic --stack "$dir" up
# A reset stack comes back up empty: HPDS answers, without data, so the
# gateway answers but reports itself degraded.
deep_status "$dir" '.deep.gateway.status != "" and .deep.data.ready == false and .deep.http.csp == "frontend"' > /dev/null ||
	fail "status --deep after reset and up: want a running stack without data"

say "destroy"
pic --stack "$dir" destroy --yes
assert_gone "$name"
[ ! -e "$dir/pic-sure.yaml" ] || fail "destroy left $dir/pic-sure.yaml"

say "e2e core passed"
