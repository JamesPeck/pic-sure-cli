#!/usr/bin/env bash
# Remote MySQL (spec §9.1 step 8, §11): a mysql container beside the stack
# plays the external server. init --db-mode remote bootstraps it (databases,
# users, grants) and migrates it, with no local picsure-db → db bootstrap
# --check passes → destroy leaves the remote's databases alone. Needs
# docker, git, go, jq and openssl; settings are in scripts/e2e-lib.sh, plus:
#
#   E2E_DB_HOST   address the stack reaches the published MySQL port on
#                 (default: host.docker.internal on macOS, the default
#                 bridge's gateway on Linux, where host.docker.internal
#                 doesn't resolve)
#   E2E_DB_BIND   host address the port is published on (default:
#                 127.0.0.1 on macOS, else E2E_DB_HOST)

# shellcheck source=scripts/e2e-lib.sh
. "$(dirname "$0")/e2e-lib.sh"

name="${E2E_NAME:-e2e-remote-db}"
dir="$E2E_WORK/$name"
remote="$name-mysql"
mysql_image=mysql:8.0 # the catalog's (internal/catalog/images.go)

made_remote=
e2e_teardown() {
	[ -n "$made_remote" ] || return 0
	if [ "$1" -ne 0 ] && [ -n "$E2E_ARTIFACTS" ]; then
		mkdir -p "$E2E_ARTIFACTS"
		docker logs --timestamps "$remote" > "$E2E_ARTIFACTS/$remote.log" 2>&1
	fi
	docker rm -f -v "$remote" > /dev/null
}

db_host="${E2E_DB_HOST:-}" db_bind="${E2E_DB_BIND:-}"
if [ "$(uname -s)" = Darwin ]; then
	db_host="${db_host:-host.docker.internal}" db_bind="${db_bind:-127.0.0.1}"
else
	db_host="${db_host:-$(docker network inspect bridge -f '{{(index .IPAM.Config 0).Gateway}}' || true)}"
	db_bind="${db_bind:-$db_host}"
fi
[ -n "$db_host" ] || fail "can't tell which address containers reach this host on; set E2E_DB_HOST"

MYSQL_PWD="$(openssl rand -hex 16)"
export MYSQL_PWD

# remote_sql SQL runs SQL as root from a client container, the way the
# stack reaches the server, and prints the rows.
remote_sql() {
	docker run --rm -e MYSQL_PWD "$mysql_image" \
		mysql -h "$db_host" -P "$port" -u root --connect-timeout=5 -N -B -e "$1" < /dev/null
}

say "the remote MySQL: $remote"
if docker container inspect "$remote" > /dev/null 2>&1; then
	fail "a container named $remote exists; remove it or set E2E_NAME"
fi
MYSQL_ROOT_PASSWORD="$MYSQL_PWD" docker run -d --name "$remote" -e MYSQL_ROOT_PASSWORD -p "$db_bind::3306" "$mysql_image" > /dev/null
made_remote=1
port="$(docker port "$remote" 3306 | head -n 1 | sed 's/.*://')"
# The image's entrypoint runs a temporary server without networking first,
# so wait for a client that comes in over TCP.
tries=60
until remote_sql 'SELECT 1' > /dev/null 2>&1; do
	tries=$((tries - 1))
	[ "$tries" -gt 0 ] || fail "$remote doesn't answer on $db_host:$port"
	sleep 2
done
echo "  remote: $db_host:$port" >&2

printf '%s\n' "$MYSQL_PWD" > "$E2E_WORK/$name-root-password"
pic_stdin="$E2E_WORK/$name-root-password" init_stack "$name" "$dir" --db-mode remote \
	--db-host "$db_host" --db-port "$port" --db-root-user root --db-root-password-stdin
rm -f "$E2E_WORK/$name-root-password"

say "no local database; the remote has the stack's databases, users and migrations"
[ -z "$(docker ps -aq --filter "label=com.docker.compose.project=$name" --filter label=com.docker.compose.service=picsure-db)" ] ||
	fail "a remote-db stack runs picsure-db"
pic --stack "$dir" db bootstrap --check
users="$(remote_sql "SELECT user FROM mysql.user WHERE host = '%' AND user IN ('picsure', 'auth', 'airflow') ORDER BY user" | tr '\n' ' ')"
[ "$users" = "airflow auth picsure " ] || fail "remote users: got '$users', want airflow auth picsure"
for db in auth picsure; do
	n="$(remote_sql "SELECT COUNT(*) FROM $db.flyway_schema_history WHERE success")"
	[ "$n" -gt 0 ] || fail "no migrations applied to $db on the remote"
	echo "  $db: $n migrations" >&2
done

say "psama and the gateway run against the remote"
deep_status "$dir" '.deep.gateway.status != ""' > /dev/null || fail "status --deep: the gateway doesn't answer"

say "destroy leaves the remote's databases"
pic --stack "$dir" destroy --yes
assert_gone "$name"
[ "$(remote_sql "SELECT COUNT(*) FROM information_schema.schemata WHERE schema_name IN ('auth', 'picsure')")" = 2 ] ||
	fail "destroy dropped the remote's databases"

say "e2e remote-db passed"
