#!/usr/bin/env bash
# Custom trust certificates (spec §9.5, §11): init with
# trust.custom_certs_dir holding one CA → psama runs with the truststore
# volume, and `keytool -list` on it shows the cert under its alias with the
# same fingerprint → a second cert, then up → psama restarts and trusts
# both → destroy.
# Needs docker, git, go, jq and openssl; settings are in scripts/e2e-lib.sh.

# shellcheck source=scripts/e2e-lib.sh
. "$(dirname "$0")/e2e-lib.sh"

name="${E2E_NAME:-e2e-truststore}"
dir="$E2E_WORK/$name"
certs="$E2E_WORK/certs"

# new_ca FILE writes a self-signed CA certificate (its key is discarded).
new_ca() {
	openssl req -x509 -newkey rsa:2048 -nodes -days 2 -subj "/CN=pic-sure e2e $(basename "$1")" \
		-keyout /dev/null -out "$1" 2> /dev/null
}

# fingerprint FILE prints the certificate's SHA-256 fingerprint as keytool
# does (upper-case hex, colon-separated).
fingerprint() {
	openssl x509 -in "$1" -noout -fingerprint -sha256 | sed 's/.*=//' | tr '[:lower:]' '[:upper:]'
}

psama() {
	docker ps -q --filter "label=com.docker.compose.project=$name" --filter label=com.docker.compose.service=psama
}

# trusted ALIAS FILE fails unless the running psama's truststore has FILE's
# certificate under ALIAS.
trusted() {
	local psama out want
	psama="$(psama)"
	[ -n "$psama" ] || fail "no psama container"
	docker exec "$psama" sh -c 'echo "$JAVA_OPTS"' | grep -qF -- '-Djavax.net.ssl.trustStore=/truststore/cacerts' ||
		fail "psama's JAVA_OPTS don't name the truststore"
	out="$(docker exec "$psama" keytool -list -v -keystore /truststore/cacerts -storepass changeit -alias "$1" 2>&1)" ||
		fail "keytool -list has no alias $1: $out"
	want="$(fingerprint "$2")"
	grep -qF "SHA256: $want" <<< "$out" || fail "alias $1 isn't $2 (want SHA256 $want): $out"
	echo "  $1: SHA256 $want" >&2
}

mkdir -p "$certs"
new_ca "$certs/e2e-one.crt"
init_stack "$name" "$dir" --set "trust.custom_certs_dir=$certs"

say "keytool -list: the custom alias"
trusted custom-1-e2e-one.crt "$certs/e2e-one.crt"

say "a second cert, then up"
new_ca "$certs/e2e-two.pem"
started="$(docker inspect -f '{{.State.StartedAt}}' "$(psama)")"
pic --stack "$dir" up
# psama reads its truststore at start, so up restarts it.
[ "$(docker inspect -f '{{.State.StartedAt}}' "$(psama)")" != "$started" ] ||
	fail "up rebuilt the truststore but didn't restart psama"
trusted custom-1-e2e-one.crt "$certs/e2e-one.crt"
trusted custom-2-e2e-two.pem "$certs/e2e-two.pem"

say "destroy"
pic --stack "$dir" destroy --yes
assert_gone "$name"

say "e2e truststore passed"
