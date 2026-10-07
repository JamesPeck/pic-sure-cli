#!/usr/bin/env bash
# Tests install.sh against release artifacts (`make snapshot` builds them into
# dist/), served by a local HTTP server laid out like GitHub.
#
#   smoke/install_test.sh [DIST]    # default: dist
#
# cosign is a stub here: the real one would need a signed release.
set -euo pipefail

ROOT="$(cd "$(dirname "$0")/.." && pwd)"
DIST="$(cd "${1:-$ROOT/dist}" && pwd)"
REPO="example/pic-sure-cli"
TAG="v2.0.0"

case "$(uname -s)" in Linux) os=linux ;; Darwin) os=darwin ;; *) echo "unsupported OS" >&2; exit 1 ;; esac
case "$(uname -m)" in x86_64 | amd64) arch=amd64 ;; arm64 | aarch64) arch=arm64 ;; *) echo "unsupported arch" >&2; exit 1 ;; esac
ASSET="pic-sure_${os}_${arch}.tar.gz"
[ -f "$DIST/$ASSET" ] && [ -f "$DIST/checksums.txt" ] || { echo "no $ASSET or checksums.txt in $DIST; run make snapshot" >&2; exit 1; }

WORK="$(mktemp -d "${TMPDIR:-/tmp}/pic-sure-install-test.XXXXXX")"
server=""
cleanup() {
  [ -z "$server" ] || kill "$server" 2>/dev/null || true
  rm -rf "$WORK"
}
trap cleanup EXIT

# Commands install.sh needs, without cosign, so each case picks its own.
mkdir "$WORK/tools"
for cmd in bash sh curl tar gzip install mkdir mktemp rm cp mv chmod awk sed grep sort tail uname cat dirname \
  sha256sum shasum perl; do
  if p="$(command -v "$cmd")"; then ln -s "$p" "$WORK/tools/$cmd"; fi
done

# A stub cosign that records its arguments; COSIGN_FAIL makes it reject.
mkdir "$WORK/cosign"
cat >"$WORK/cosign/cosign" <<'EOF'
#!/bin/sh
printf '%s\n' "$@" >"$COSIGN_LOG"
[ -z "${COSIGN_FAIL:-}" ] || { echo "error: none of the expected identities matched" >&2; exit 1; }
EOF
chmod +x "$WORK/cosign/cosign"

# The served tree: /gh/<repo>/releases/download/<tag>/... and
# /api/repos/<repo>/releases (the query string is ignored).
WWW="$WORK/www"
REL="$WWW/gh/$REPO/releases/download"
mkdir -p "$WWW/api/repos/$REPO"
cat >"$WWW/api/repos/$REPO/releases" <<'EOF'
[
  {"tag_name": "v3.0.0"},
  {"tag_name": "v2.10.0-rc.1"},
  {"tag_name": "v2.9.1"},
  {"tag_name": "v2.10.0"},
  {"tag_name": "v1.4.0"}
]
EOF

# release TAG: a release holding the snapshot's archive, checksums and a
# placeholder bundle (the stub cosign doesn't read it).
release() {
  mkdir -p "$REL/$1"
  cp "$DIST/$ASSET" "$DIST/checksums.txt" "$REL/$1/"
  echo '{"placeholder": true}' >"$REL/$1/checksums.txt.sigstore.json"
}
release "$TAG"
release v2.10.0

port="$(python3 -c 'import socket; s=socket.socket(); s.bind(("127.0.0.1", 0)); print(s.getsockname()[1])')"
python3 -m http.server "$port" --bind 127.0.0.1 --directory "$WWW" >"$WORK/server.log" 2>&1 &
server=$!
for _ in $(seq 50); do
  curl -fs "http://127.0.0.1:$port/api/repos/$REPO/releases" >/dev/null 2>&1 && break
  sleep 0.1
done

failures=0
pass() { printf 'ok   %s\n' "$1"; }
flunk() {
  printf 'FAIL %s: %s\n' "$1" "$2"
  sed 's/^/     | /' "$WORK/out" | tail -n 20
  failures=$((failures + 1))
}

# run NAME [--cosign] [install.sh args...]: runs install.sh into a fresh
# bin dir ($BIN). Output goes to $WORK/out, the exit status to $status.
run() {
  local name="$1" path="$WORK/tools"
  shift
  if [ "${1:-}" = --cosign ]; then
    path="$WORK/cosign:$path"
    shift
  fi
  BIN="$WORK/bin-$name"
  COSIGN_LOG="$WORK/cosign-$name.log"
  status=0
  env -i HOME="$WORK" PATH="$path" TMPDIR="$WORK" COSIGN_LOG="$COSIGN_LOG" COSIGN_FAIL="${COSIGN_FAIL:-}" \
    PIC_SURE_INSTALL_GITHUB_URL="http://127.0.0.1:$port/gh" \
    PIC_SURE_INSTALL_API_URL="http://127.0.0.1:$port/api" \
    bash "$ROOT/install.sh" --repo "$REPO" --bin-dir "$BIN" "$@" >"$WORK/out" 2>&1 || status=$?
}

installed() { [ -x "$BIN/pic-sure" ] && "$BIN/pic-sure" version >/dev/null; }
said() { grep -qF -- "$1" "$WORK/out"; }

identity="https://github.com/$REPO/.github/workflows/release.yml@refs/tags/$TAG"

run verified --cosign --version "$TAG"
if [ "$status" -ne 0 ] || ! installed; then
  flunk verified "exit $status, or no working binary"
elif ! grep -qxF "$identity" "$COSIGN_LOG" || ! grep -qxF https://token.actions.githubusercontent.com "$COSIGN_LOG"; then
  flunk verified "cosign wasn't asked for the release workflow's identity: $(tr '\n' ' ' <"$COSIGN_LOG")"
elif ! said "cosign verify-blob --bundle checksums.txt.sigstore.json"; then
  flunk verified "no manual verification commands"
else
  pass verified
fi

run no-cosign --version "${TAG#v}"
if [ "$status" -ne 0 ] || ! installed; then
  flunk no-cosign "exit $status, or no working binary"
elif ! said "WARNING: cosign not found"; then
  flunk no-cosign "no warning that the signature went unchecked"
else
  pass no-cosign
fi

run newest-v2 --cosign
if [ "$status" -ne 0 ] || ! installed; then
  flunk newest-v2 "exit $status, or no working binary"
elif ! said "pic-sure v2.10.0"; then
  flunk newest-v2 "didn't pick v2.10.0"
else
  pass newest-v2
fi

COSIGN_FAIL=1 run bad-signature --cosign --version "$TAG"
if [ "$status" -eq 0 ] || [ -e "$BIN/pic-sure" ] || ! said "signature verification FAILED"; then
  flunk bad-signature "exit $status; a bad signature must abort before installing"
else
  pass bad-signature
fi

release v2.0.1
rm "$REL/v2.0.1/checksums.txt.sigstore.json"
run unsigned --version v2.0.1
if [ "$status" -eq 0 ] || [ -e "$BIN/pic-sure" ] || ! said "has no checksums.txt.sigstore.json"; then
  flunk unsigned "exit $status; a v2 release without a bundle must be refused, cosign or not"
else
  pass unsigned
fi

release v2.0.2
printf 'corrupt' >>"$REL/v2.0.2/$ASSET"
run checksum-mismatch --cosign --version v2.0.2
if [ "$status" -eq 0 ] || [ -e "$BIN/pic-sure" ] || ! said "checksum verification FAILED"; then
  flunk checksum-mismatch "exit $status; a corrupt archive must abort before installing"
else
  pass checksum-mismatch
fi

run missing-release --version v2.7.7
if [ "$status" -eq 0 ] || [ -e "$BIN/pic-sure" ] || ! said "download failed"; then
  flunk missing-release "exit $status"
else
  pass missing-release
fi

[ "$failures" -eq 0 ] || { echo "$failures install.sh test(s) failed" >&2; exit 1; }
echo "install.sh: all tests passed"
