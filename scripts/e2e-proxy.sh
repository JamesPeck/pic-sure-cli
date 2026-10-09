#!/usr/bin/env bash
# The proxy e2e (spec §9.10, D27, D36): a stack whose proxy is a squid
# container, with squid's access log as the evidence that each egress path
# used it. docs/testing-proxy.md describes the setup and what each check
# proves. Needs docker, git, go, jq and curl; settings are in
# scripts/e2e-lib.sh, plus:
#
#   E2E_PROXY_HOST   address that both this host and containers reach the
#                    published squid port on (default: the default bridge's
#                    gateway on Linux, en0's or en1's address on macOS)
#   E2E_SQUID_IMAGE  squid image (default: ubuntu/squid, pinned)
#   E2E_NODE_IMAGE   base image of the build-args probe (default: the first
#                    FROM of the stack's frontend Dockerfile)

# shellcheck source=scripts/e2e-lib.sh
. "$(dirname "$0")/e2e-lib.sh"

name="${E2E_NAME:-e2e-proxy}"
dir="$E2E_WORK/$name"
squid="$name-squid"
internal="$name-internal"
egress="$name-egress"
squid_image="${E2E_SQUID_IMAGE:-ubuntu/squid:latest@sha256:6a097f68bae708cedbabd6188d68c7e2e7a38cedd05a176e1cc0ba29e3bbe029}"

# A cache of our own, so init's git clones and the demo download really
# happen (through the proxy) instead of reusing the user's cache. It must
# not be under the temp dir (cache.DefaultRoot refuses that).
mkdir -p "$HOME/.cache"
proxy_cache="$(mktemp -d "$HOME/.cache/pic-sure-e2e-proxy.XXXXXX")"
export XDG_CACHE_HOME="$proxy_cache"

made_squid='' made_internal='' made_egress=''
proxy_cleanup() {
	local rc=$?
	set +e
	if [ "$rc" -ne 0 ] && [ -n "$E2E_KEEP" ]; then
		echo "e2e: E2E_KEEP set; left what it made of $squid, $internal and $egress, and the cache $proxy_cache" >&2
	else
		# Only what this run created: a name in use is another run's.
		if [ -n "$made_squid" ]; then docker rm -f "$squid" > /dev/null 2>&1; fi
		if [ -n "$made_internal" ]; then docker network rm "$internal" > /dev/null 2>&1; fi
		if [ -n "$made_egress" ]; then docker network rm "$egress" > /dev/null 2>&1; fi
		rm -rf "$proxy_cache"
	fi
	(exit "$rc")
	e2e_cleanup
}
trap proxy_cleanup EXIT

proxy_host="${E2E_PROXY_HOST:-}"
if [ -z "$proxy_host" ]; then
	if [ "$(uname -s)" = Darwin ]; then
		proxy_host="$(ipconfig getifaddr en0 || ipconfig getifaddr en1 || true)"
	else
		proxy_host="$(docker network inspect bridge -f '{{(index .IPAM.Config 0).Gateway}}' || true)"
	fi
	[ -n "$proxy_host" ] || fail "can't tell which address containers reach this host on; set E2E_PROXY_HOST"
fi

squid_log() { docker exec "$squid" cat /var/log/squid/access.log; }
mark() { squid_log | wc -l | tr -d ' '; }

# tunnelled SINCE HOST waits until squid has logged a successful CONNECT to
# HOST:443 after line SINCE. Squid logs a tunnel when it closes.
tunnelled() {
	local since="$1" host="$2" tries=15
	until squid_log | tail -n "+$((since + 1))" |
		awk -v h="$host:443" '$4 == "TCP_TUNNEL/200" && $6 == "CONNECT" && $7 == h { found = 1 } END { exit !found }'; do
		tries=$((tries - 1))
		if [ "$tries" -le 0 ]; then
			squid_log | tail -n "+$((since + 1))" >&2
			fail "squid logged no CONNECT to $host:443"
		fi
		sleep 2
	done
	echo "  through squid: $host" >&2
}

say "squid on $egress, dual-homed onto the internal network $internal"
# Each create fails if the name is taken, so a flag set after it is ours.
docker network create "$egress" > /dev/null
made_egress=1
docker network create --internal "$internal" > /dev/null
made_internal=1
# Published on the proxy address only. squid allows any private-range
# client, so on macOS, where that is the LAN address, the LAN can use it
# while the script runs.
docker create --name "$squid" --network "$egress" -p "$proxy_host::3128" "$squid_image" > /dev/null
made_squid=1
docker start "$squid" > /dev/null
docker network connect --alias squid "$internal" "$squid"
port="$(docker port "$squid" 3128 | head -n 1 | sed 's/.*://')"
proxy="http://$proxy_host:$port"
tries=15
until curl -fsS -o /dev/null -x "$proxy" https://github.com/; do
	tries=$((tries - 1))
	[ "$tries" -gt 0 ] || fail "squid at $proxy doesn't answer"
	sleep 2
done
echo "  proxy: $proxy" >&2

say "git: init clones the release and the sources through the proxy"
m="$(mark)"
init_stack "$name" "$dir" --http-proxy "$proxy" --https-proxy "$proxy"
tunnelled "$m" github.com

frontend="$(jq -r '.components.frontend.commit' "$dir/.pic-sure/state.json")"
node_image="${E2E_NODE_IMAGE:-$(git --git-dir "$XDG_CACHE_HOME/pic-sure/git/PIC-SURE-Frontend.git" show "$frontend:Dockerfile" |
	awk '$1 == "FROM" { print $2; exit }')}"
[ -n "$node_image" ] || fail "no FROM in the frontend Dockerfile at $frontend"

say "control: the internal network has no direct egress"
if docker run --rm --network "$internal" "$node_image" \
	wget -q -T 5 -O /dev/null https://registry.npmjs.org/ 2> /dev/null; then
	fail "a container on $internal reached the internet directly"
fi

say "doctor --network through the proxy"
m="$(mark)"
pic --stack "$dir" doctor --network --json > "$E2E_WORK/doctor.json" ||
	fail "doctor --network: $(jq -c '[.checks[] | select(.status == "fail")]' "$E2E_WORK/doctor.json")"
for host in github.com repo.maven.apache.org registry.npmjs.org dl-cdn.alpinelinux.org; do
	tunnelled "$m" "$host"
done

say "demo download through the CLI's HTTP client"
m="$(mark)"
pic --stack "$dir" data demo nhanes --heap "$E2E_LOAD_HEAP_MB"
tunnelled "$m" raw.githubusercontent.com

say "self-update's release lookup through the CLI's HTTP client"
# An unknown version fails after the lookup, so nothing is replaced. Run
# a copy anyway: self-update refuses a binary it can't write.
mkdir -p "$E2E_WORK/self-update"
cp "$PIC_SURE" "$E2E_WORK/self-update/pic-sure"
m="$(mark)"
if "$E2E_WORK/self-update/pic-sure" --non-interactive --stack "$dir" self-update --to 0.0.1 < /dev/null; then
	fail "self-update --to 0.0.1 succeeded"
fi
tunnelled "$m" api.github.com

say "psama: the rendered JAVA_OPTS reach Auth0 through the proxy"
psama="$(docker ps -q --filter "label=com.docker.compose.project=$name" --filter label=com.docker.compose.service=psama)"
[ -n "$psama" ] || fail "no psama container"
tenant="$(pic --stack "$dir" config get auth.auth0.tenant)"
cat > "$E2E_WORK/Fetch.java" << 'EOF'
import java.net.ProxySelector;
import java.net.URI;
import java.net.http.HttpClient;
import java.net.http.HttpRequest;
import java.net.http.HttpResponse;

// GETs a URL through the JVM's default ProxySelector, which reads the
// http(s).proxyHost/Port and http.nonProxyHosts properties.
public class Fetch {
    public static void main(String[] args) throws Exception {
        HttpClient c = HttpClient.newBuilder().proxy(ProxySelector.getDefault()).build();
        HttpResponse<Void> r = c.send(HttpRequest.newBuilder(URI.create(args[0])).build(), HttpResponse.BodyHandlers.discarding());
        if (r.statusCode() != 200) {
            throw new RuntimeException(args[0] + ": HTTP " + r.statusCode());
        }
    }
}
EOF
docker cp "$E2E_WORK/Fetch.java" "$psama:/tmp/Fetch.java"
m="$(mark)"
# set -f: the nonProxyHosts patterns hold * and must not be globbed.
# shellcheck disable=SC2016 # expanded by the container's shell
docker exec "$psama" sh -c 'set -f; java $(printf "%s\n" $JAVA_OPTS | grep -i proxy) /tmp/Fetch.java "$1"' \
	sh "https://$tenant.auth0.com/.well-known/openid-configuration"
tunnelled "$m" "$tenant.auth0.com"

say "Maven: the reactor's settings.xml resolves into an empty repo with no direct egress"
# The MavenSettings() bytes the reactor build mounts, for squid's name on the
# internal network (the internal network can't reach the published port).
(cd "$repo_root" && go run ./internal/testfixtures/maven-settings -http http://squid:3128 -https http://squid:3128) \
	> "$E2E_WORK/settings.xml"
# The reactor's Maven image (catalog "maven"); any Maven would do.
mvn_get=(docker run --rm --network "$internal" -v "$E2E_WORK/settings.xml:/pic-sure/settings.xml:ro"
	maven:3-amazoncorretto-25 mvn -B -q -Dmaven.repo.local=/tmp/m2 dependency:get
	-Dartifact=org.apache.commons:commons-lang3:3.17.0)
if "${mvn_get[@]}" > /dev/null 2>&1; then
	fail "Maven resolved without the proxy"
fi
m="$(mark)"
"${mvn_get[@]}" -s /pic-sure/settings.xml
tunnelled "$m" repo.maven.apache.org

say "docker build: apk and pnpm/npm through the proxy build args"
# The frontend Dockerfile's first steps. The build args are the stack's
# proxy variables, which psama has from the same netproxy Env().
mkdir -p "$E2E_WORK/probe"
cat > "$E2E_WORK/probe/Dockerfile" << EOF
FROM $node_image
RUN apk add --no-cache pnpm
WORKDIR /probe
RUN echo '{"name":"probe","version":"1.0.0"}' > package.json && CI=true pnpm add is-number@7.0.0 && npm view is-number@7.0.0 version
EOF
build_args=()
while IFS= read -r kv; do
	export "${kv?}"
	build_args+=(--build-arg "${kv%%=*}")
done < <(docker exec "$psama" env | grep -E '^(HTTP_PROXY|HTTPS_PROXY|NO_PROXY|http_proxy|https_proxy|no_proxy)=')
[ "${#build_args[@]}" -eq 12 ] || fail "psama has $((${#build_args[@]} / 2)) proxy variables, want 6"
m="$(mark)"
docker build -q --no-cache "${build_args[@]}" -t "$name-probe" "$E2E_WORK/probe" > /dev/null
docker rmi "$name-probe" > /dev/null
unset HTTP_PROXY HTTPS_PROXY NO_PROXY http_proxy https_proxy no_proxy
tunnelled "$m" dl-cdn.alpinelinux.org
tunnelled "$m" registry.npmjs.org

say "D36: a failed daemon pull prints the daemon proxy instructions"
# The shim sends doctor's test pull to a registry that doesn't resolve, so
# the real daemon fails it as it would without egress; daemon settings stay
# untouched.
mkdir -p "$E2E_WORK/shim"
cat > "$E2E_WORK/shim/docker" << EOF
#!/bin/sh
if [ "\$1" = pull ]; then exec $(command -v docker) pull "$name.invalid/\$2"; fi
exec $(command -v docker) "\$@"
EOF
chmod +x "$E2E_WORK/shim/docker"
if PATH="$E2E_WORK/shim:$PATH" pic --stack "$dir" doctor --network --json > "$E2E_WORK/doctor-pull.json"; then
	fail "doctor passed with a failing pull"
fi
# Every runtime's instructions name the proxy URL; Docker Desktop's also
# name its settings page.
want="$proxy"
if [ "$(docker info -f '{{.OperatingSystem}}')" = "Docker Desktop" ]; then want="Settings > Resources > Proxies"; fi
jq -e --arg proxy "$proxy" --arg want "$want" '.checks[] | select(.name == "network-docker-pull") |
	.status == "fail" and (.detail | contains($proxy) and contains($want))' "$E2E_WORK/doctor-pull.json" > /dev/null ||
	fail "network-docker-pull: $(jq -c '.checks[] | select(.name == "network-docker-pull")' "$E2E_WORK/doctor-pull.json")"
jq -r '.checks[] | select(.name == "network-docker-pull") | .detail' "$E2E_WORK/doctor-pull.json" | sed 's/^/  /' >&2

say "nothing was denied, and nothing local went to the proxy"
# Local: a single-label host (the stack's services, localhost) or 127.*.
denied="$(squid_log | awk '{ h = $7; sub(/^[a-z]+:\/\//, "", h); sub(/[:\/].*/, "", h) }
	$7 !~ /^error:/ && ($4 ~ /DENIED/ || h !~ /\./ || h ~ /^127\./)')"
[ -z "$denied" ] || fail "squid denied or saw local requests: $denied"

say "destroy"
pic --stack "$dir" destroy --yes
assert_gone "$name"

say "e2e-proxy passed"
