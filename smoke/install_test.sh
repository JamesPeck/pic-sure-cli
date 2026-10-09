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
if [ ! -f "$DIST/$ASSET" ] || [ ! -f "$DIST/checksums.txt" ]; then
  echo "no $ASSET or checksums.txt in $DIST; run make snapshot" >&2
  exit 1
fi

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

# A stub cosign that records verify-blob's arguments. `cosign version`
# reports COSIGN_VERSION (default 3.1.3) the way cosign does; COSIGN_FAIL
# makes verify-blob reject (1) or fail to fetch the trust root (tuf).
mkdir "$WORK/cosign"
cat >"$WORK/cosign/cosign" <<'EOF'
#!/bin/sh
if [ "$1" = version ]; then
  printf '  ______\nGitVersion:    v%s\nGitCommit:     0000000\n' "${COSIGN_VERSION:-3.1.3}"
  exit 0
fi
printf '%s\n' "$@" >"$COSIGN_LOG"
case "${COSIGN_FAIL:-}" in
  "") ;;
  tuf) echo 'Error: getting trusted root from TUF: tuf refresh failed: proxyconnect tcp: connection refused' >&2; exit 1 ;;
  *) echo "error: none of the expected identities matched" >&2; exit 1 ;;
esac
EOF
chmod +x "$WORK/cosign/cosign"

# The served tree: /gh/<repo>/releases/download/<tag>/... and
# /api/repos/<repo>/releases?...page=N, served from releases.N.
WWW="$WORK/www"
REL="$WWW/gh/$REPO/releases/download"
API="$WWW/api/repos/$REPO"
mkdir -p "$API"
# A full first page of v3 releases, so the v2 ones are on page 2. Indented
# like the API's answers, with nested objects; v2.11.0 is marked prerelease.
{
  printf '[\n'
  for i in $(seq 0 99); do printf '  {\n    "tag_name": "v3.0.%s",\n    "prerelease": false\n  },\n' "$i"; done
  printf '  {\n    "tag_name": "v3.1.0",\n    "prerelease": false\n  }\n]\n'
} >"$API/releases.1"
cat >"$API/releases.2" <<'EOF'
[
  {
    "author": {
      "login": "x"
    },
    "tag_name": "v3.0.0",
    "draft": false,
    "prerelease": false,
    "assets": [
      {
        "uploader": {
          "login": "x"
        }
      }
    ]
  },
  {
    "tag_name": "v2.11.0",
    "prerelease": true,
    "assets": []
  },
  {
    "tag_name": "v2.10.0-rc.1",
    "prerelease": true
  },
  {
    "tag_name": "v2.9.1",
    "prerelease": false
  },
  {
    "tag_name": "v2.10.0",
    "prerelease": false
  },
  {
    "tag_name": "v1.4.0",
    "prerelease": false
  }
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
release v2.11.0

port="$(python3 -c 'import socket; s=socket.socket(); s.bind(("127.0.0.1", 0)); print(s.getsockname()[1])')"
cat >"$WORK/server.py" <<'EOF'
import http.server, sys, urllib.parse

class Handler(http.server.SimpleHTTPRequestHandler):
    def translate_path(self, path):
        url = urllib.parse.urlsplit(path)
        page = urllib.parse.parse_qs(url.query).get("page")
        return super().translate_path(url.path + ("." + page[0] if page else ""))

http.server.ThreadingHTTPServer(("127.0.0.1", int(sys.argv[1])),
    lambda *a: Handler(*a, directory=sys.argv[2])).serve_forever()
EOF
python3 -I "$WORK/server.py" "$port" "$WWW" >"$WORK/server.log" 2>&1 &
server=$!
for _ in $(seq 50); do
  curl -fs "http://127.0.0.1:$port/api/repos/$REPO/releases?page=1" >/dev/null 2>&1 && break
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
    COSIGN_VERSION="${COSIGN_VERSION:-}" PIC_SURE_REQUIRE_SIGNATURE="${PIC_SURE_REQUIRE_SIGNATURE:-}" \
    PIC_SURE_INSTALL_GITHUB_URL="http://127.0.0.1:$port/gh" \
    PIC_SURE_INSTALL_API_URL="http://127.0.0.1:$port/api" \
    bash "$ROOT/install.sh" --repo "$REPO" --bin-dir "$BIN" "$@" >"$WORK/out" 2>&1 || status=$?
}

installed() { [ -x "$BIN/pic-sure" ] && "$BIN/pic-sure" version >/dev/null; }
said() { grep -qF -- "$1" "$WORK/out"; }

identity="https://github.com/$REPO/.github/workflows/release.yml@refs/tags/$TAG"
# cosign's arguments, with absolute paths cut to their base names.
want_cosign="verify-blob --bundle checksums.txt.sigstore.json --certificate-identity $identity --certificate-oidc-issuer https://token.actions.githubusercontent.com checksums.txt "

run verified --cosign --version "$TAG"
if [ "$status" -ne 0 ] || ! installed; then
  flunk verified "exit $status, or no working binary"
elif [ "$(awk '/^\// { n = split($0, p, "/"); $0 = p[n] } { printf "%s ", $0 }' "$COSIGN_LOG")" != "$want_cosign" ]; then
  flunk verified "cosign wasn't asked to verify checksums.txt as the release workflow: $(tr '\n' ' ' <"$COSIGN_LOG")"
elif ! said "cosign verify-blob --bundle checksums.txt.sigstore.json"; then
  flunk verified "no manual verification commands"
else
  pass verified
fi

run no-cosign --version "${TAG#v}"
if [ "$status" -ne 0 ] || ! installed; then
  flunk no-cosign "exit $status, or no working binary"
elif ! said "WARNING: cosign isn't installed"; then
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

COSIGN_VERSION=2.4.1 run old-cosign --cosign --version "$TAG"
if [ "$status" -ne 0 ] || ! installed; then
  flunk old-cosign "exit $status, or no working binary"
elif [ -e "$COSIGN_LOG" ] || said "FAILED" || ! said "cosign 2.4.1 is older than 3.0"; then
  flunk old-cosign "a cosign older than 3.0 must be skipped with a warning, never reported as a failed signature"
else
  pass old-cosign
fi

PIC_SURE_REQUIRE_SIGNATURE=1 run require-no-cosign --version "$TAG"
if [ "$status" -eq 0 ] || [ -e "$BIN/pic-sure" ] || ! said "PIC_SURE_REQUIRE_SIGNATURE requires a checked signature"; then
  flunk require-no-cosign "exit $status; PIC_SURE_REQUIRE_SIGNATURE without cosign must abort before installing"
else
  pass require-no-cosign
fi

PIC_SURE_REQUIRE_SIGNATURE=1 COSIGN_VERSION=2.6.5 run require-old-cosign --cosign --version "$TAG"
if [ "$status" -eq 0 ] || [ -e "$BIN/pic-sure" ] || ! said "cosign 2.6.5 is older than 3.0"; then
  flunk require-old-cosign "exit $status; PIC_SURE_REQUIRE_SIGNATURE with a too-old cosign must abort before installing"
else
  pass require-old-cosign
fi

PIC_SURE_REQUIRE_SIGNATURE=1 run require-verified --cosign --version "$TAG"
if [ "$status" -ne 0 ] || ! installed; then
  flunk require-verified "exit $status, or no working binary"
else
  pass require-verified
fi

COSIGN_FAIL=tuf run no-trust-root --cosign --version "$TAG"
if [ "$status" -eq 0 ] || [ -e "$BIN/pic-sure" ] || said "signature verification FAILED" || ! said "network or proxy problem"; then
  flunk no-trust-root "exit $status; cosign failing to reach Sigstore must abort, as a network problem"
else
  pass no-trust-root
fi

# A download cut short must install nothing: feed bash every prefix of
# install.sh that ends on a line boundary.
total="$(wc -l <"$ROOT/install.sh")"
truncated_ok=true
for n in $(seq 1 $((total - 1))); do
  BIN="$WORK/bin-truncated"
  head -n "$n" "$ROOT/install.sh" \
    | env -i HOME="$WORK" PATH="$WORK/cosign:$WORK/tools" TMPDIR="$WORK" COSIGN_LOG=/dev/null \
      PIC_SURE_INSTALL_GITHUB_URL="http://127.0.0.1:$port/gh" PIC_SURE_INSTALL_API_URL="http://127.0.0.1:$port/api" \
      bash -s -- --repo "$REPO" --bin-dir "$BIN" --version "$TAG" >"$WORK/out" 2>&1 || true
  if [ -e "$BIN" ] || said "Downloading"; then
    truncated_ok=false
    flunk truncated "the first $n lines of install.sh started an install"
    break
  fi
done
if [ "$truncated_ok" = true ]; then
  pass truncated
fi

release v2.0.1
rm "$REL/v2.0.1/checksums.txt.sigstore.json"
run unsigned --version v2.0.1
if [ "$status" -eq 0 ] || [ -e "$BIN/pic-sure" ] || ! said "could not download checksums.txt.sigstore.json"; then
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
