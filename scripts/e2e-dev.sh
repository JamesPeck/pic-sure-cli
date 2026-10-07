#!/usr/bin/env bash
# Dev overlays (spec §9.9, §11): init → a checkout of the stack's pic-sure
# commit as components.pic-sure.source → dev on psama builds the reactor and
# its images from it and recreates psama with a JDWP agent on 127.0.0.1 →
# the debug port answers the JDWP handshake → dev off psama closes it, and
# psama keeps running the source build → destroy. Needs docker, git, go and
# jq; settings are in scripts/e2e-lib.sh. The reactor build reuses the
# pic-sure-m2 volume, so a warm one saves most of its time.

# shellcheck source=scripts/e2e-lib.sh
. "$(dirname "$0")/e2e-lib.sh"

name="${E2E_NAME:-e2e-dev}"
dir="$E2E_WORK/$name"
src="$E2E_WORK/pic-sure"

psama() {
	docker ps -q --filter "label=com.docker.compose.project=$name" --filter label=com.docker.compose.service=psama
}

# jdwp PORT succeeds when 127.0.0.1:PORT answers the JDWP handshake.
jdwp() {
	local reply=
	exec 3<> "/dev/tcp/127.0.0.1/$1" || return 1
	printf 'JDWP-Handshake' >&3
	IFS= read -r -t 10 -n 14 reply <&3 || true
	exec 3>&-
	[ "$reply" = JDWP-Handshake ]
}

init_stack "$name" "$dir"

say "a checkout of the stack's pic-sure commit as its source"
commit="$(jq -r '.components["pic-sure"].commit' "$dir/.pic-sure/state.json")"
# init cloned it into the host cache; a local clone shares those objects.
git clone -q "${XDG_CACHE_HOME:-$HOME/.cache}/pic-sure/git/pic-sure.git" "$src"
git -C "$src" checkout -q --detach "$commit"
pic --stack "$dir" config set components.pic-sure.source "$src"

say "dev on psama"
on="$(pic --stack "$dir" --json dev on psama | tail -n 1)"
port="$(jq -r '.data.port' <<< "$on")"
[ "$port" -gt 0 ] 2> /dev/null || fail "dev on psama reported no debug port: $on"
image="$(docker inspect -f '{{.Config.Image}}' "$(psama)")"
case "$image" in
*:"dev-$name-${commit:0:12}") echo "  psama runs $image" >&2 ;;
*) fail "psama runs $image, not the source build dev-$name-${commit:0:12}" ;;
esac
jdwp "$port" || fail "127.0.0.1:$port doesn't answer the JDWP handshake"
echo "  127.0.0.1:$port answered JDWP-Handshake" >&2

say "dev off psama"
pic --stack "$dir" dev off psama
if (exec 3<> "/dev/tcp/127.0.0.1/$port") 2> /dev/null; then
	fail "127.0.0.1:$port still accepts connections after dev off"
fi
[ "$(docker inspect -f '{{.Config.Image}}' "$(psama)")" = "$image" ] ||
	fail "dev off psama changed psama's image; it keeps the source build"
deep_status "$dir" '.deep.gateway.status != ""' > /dev/null || fail "status --deep: the gateway doesn't answer"

say "destroy"
pic --stack "$dir" destroy --yes
assert_gone "$name"
if docker image ls --format '{{.Tag}}' | grep -q "^dev-$name-"; then fail "destroy left dev images"; fi

say "e2e dev passed"
