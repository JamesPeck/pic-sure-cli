# shellcheck shell=bash
# Shared by the e2e scripts (spec §11); source it, don't run it.
#
# Environment:
#   PIC_SURE            pic-sure binary (default: built from this checkout)
#   E2E_NAME            stack name, also its compose project (default per script)
#   E2E_WORK            directory the stack directories go in (default: a new
#                       temp dir, removed after a pass)
#   E2E_RELEASE_BRANCH  release-control branch or tag (default james_mono)
#   E2E_JAVA_OPTS       HPDS JVM options (default -Xmx1g)
#   E2E_LOAD_HEAP_MB    loader JVM heap for data demo (default 1024)
#   E2E_ARTIFACTS       on failure, compose logs, run logs and the support
#                       bundle of every stack go here (default: none kept)
#   E2E_KEEP=1          leave the stacks running instead of destroying them
#   E2E_IMAGES_FILE     each init appends the images its stack runs here
#                       (the workflow caches them)

set -euo pipefail

repo_root="$(cd "$(dirname "${BASH_SOURCE[0]}")/.." && pwd)"
E2E_RELEASE_BRANCH="${E2E_RELEASE_BRANCH:-james_mono}"
E2E_JAVA_OPTS="${E2E_JAVA_OPTS:--Xmx1g}"
E2E_LOAD_HEAP_MB="${E2E_LOAD_HEAP_MB:-1024}"
E2E_ARTIFACTS="${E2E_ARTIFACTS:-}"
E2E_KEEP="${E2E_KEEP:-}"
e2e_made_work=
if [ -z "${E2E_WORK:-}" ]; then
	E2E_WORK="$(mktemp -d "${TMPDIR:-/tmp}/pic-sure-e2e.XXXXXX")"
	e2e_made_work=1
fi
mkdir -p "$E2E_WORK"
E2E_WORK="$(cd "$E2E_WORK" && pwd -P)"

if [ -z "${PIC_SURE:-}" ]; then
	PIC_SURE="$E2E_WORK/bin/pic-sure"
	(cd "$repo_root" && go build -o "$PIC_SURE" ./cmd/pic-sure)
fi

# Stack names, and their directories, that cleanup collects from and destroys.
e2e_stacks=()
e2e_dirs=()

say() { printf '\n==> %s\n' "$*" >&2; }
fail() {
	echo "e2e: FAIL: $*" >&2
	exit 1
}

# pic runs pic-sure without a terminal; stdout and stderr pass through.
pic() { "$PIC_SURE" --non-interactive "$@" < /dev/null; }

# init_stack NAME DIR creates an open-mode stack with auto ports and no
# client secret, so init generates one.
init_stack() {
	local name="$1" dir="$2"
	e2e_stacks+=("$name")
	e2e_dirs+=("$dir")
	say "init $name"
	pic init "$dir" --name "$name" --auth-mode open --admin-email admin@example.com \
		--auto-ports --release-branch "$E2E_RELEASE_BRANCH" --set "hpds.java_opts=$E2E_JAVA_OPTS"
	grep -q '^auth0_client_secret_generated: true$' "$dir/.pic-sure/secrets.yaml" ||
		fail "$name: init didn't generate the open-mode client secret"
	if [ -n "${E2E_IMAGES_FILE:-}" ]; then
		jq -r '.images | to_entries[] | "hms-dbmi/\(.key):\(.value)"' "$dir/.pic-sure/state.json" >> "$E2E_IMAGES_FILE"
	fi
}

# deep_status DIR prints `status --deep --json`, retrying until jq FILTER
# holds (a just-started service may need a moment) or the tries run out.
deep_status() {
	local dir="$1" filter="$2" out tries=12
	while :; do
		out="$(pic --stack "$dir" status --deep --json || true)"
		if jq -e "$filter" > /dev/null 2>&1 <<< "$out"; then
			printf '%s\n' "$out"
			return 0
		fi
		tries=$((tries - 1))
		if [ "$tries" -le 0 ]; then
			printf '%s\n' "$out" | jq '.deep // .' >&2 || printf '%s\n' "$out" >&2
			return 1
		fi
		sleep 10
	done
}

# assert_gone NAME fails if any container or volume of the stack is left.
assert_gone() {
	local name="$1" left
	left="$(
		docker ps -aq --filter "label=com.docker.compose.project=$name"
		docker ps -aq --filter "label=org.hms-dbmi.picsure.stack=$name"
		docker volume ls -q --filter "label=com.docker.compose.project=$name"
		docker volume ls -q --filter "label=org.hms-dbmi.picsure.stack=$name"
	)"
	[ -z "$left" ] || fail "$name: destroy left containers or volumes: $(echo "$left" | tr '\n' ' ')"
}

# collect NAME DIR saves what's needed to debug a failed run.
collect() {
	local name="$1" dir="$2" out="$E2E_ARTIFACTS/$1"
	mkdir -p "$out"
	[ -f "$dir/pic-sure.yaml" ] || return 0
	pic --stack "$dir" compose -- ps --all > "$out/compose-ps.txt" 2>&1 || true
	pic --stack "$dir" compose -- logs --no-color --timestamps > "$out/compose.log" 2>&1 || true
	pic --stack "$dir" status --json > "$out/status.json" 2> "$out/status.err" || true
	# support-bundle is optional: older builds don't implement it.
	pic --stack "$dir" support-bundle --output "$out/support-bundle.tar.gz" > "$out/support-bundle.txt" 2>&1 ||
		echo "support-bundle failed or isn't available; see support-bundle.txt" >&2
	[ -d "$dir/.pic-sure/logs" ] && cp -R "$dir/.pic-sure/logs" "$out/run-logs" || true
}

e2e_cleanup() {
	local rc=$? i
	set +e
	if [ "$rc" -ne 0 ] && [ -n "$E2E_ARTIFACTS" ]; then
		for i in "${!e2e_stacks[@]}"; do collect "${e2e_stacks[$i]}" "${e2e_dirs[$i]}"; done
	fi
	if [ -n "$E2E_KEEP" ]; then
		echo "e2e: E2E_KEEP set; left the stacks in $E2E_WORK" >&2
		exit "$rc"
	fi
	for i in "${!e2e_stacks[@]}"; do
		if [ -f "${e2e_dirs[$i]}/pic-sure.yaml" ]; then
			pic --stack "${e2e_dirs[$i]}" destroy --yes > /dev/null 2>&1 ||
				docker compose -p "${e2e_stacks[$i]}" down -v --remove-orphans > /dev/null 2>&1
		fi
	done
	if [ "$rc" -eq 0 ] && [ -n "$e2e_made_work" ]; then rm -rf "$E2E_WORK"; fi
	exit "$rc"
}
trap e2e_cleanup EXIT
