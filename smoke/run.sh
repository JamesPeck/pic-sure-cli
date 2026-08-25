#!/usr/bin/env bash
# =============================================================================
# pic-sure CLI smoke harness
# =============================================================================
# Exercises the compiled binary against deterministic AIO fixtures. This repo
# owns the CLI, while pic-sure-all-in-one owns the scripts it dispatches.
#
# Usage: make smoke
# =============================================================================

set -euo pipefail

REPO_DIR="$(cd "$(dirname "${BASH_SOURCE[0]}")/.." && pwd)"
BIN="$REPO_DIR/bin/pic-sure"
FIXTURE_ROOT="$REPO_DIR/testdata/fixtures/root"

note() { printf '[smoke] %s\n' "$*"; }
fail() { printf '[smoke] FAIL: %s\n' "$*" >&2; exit 1; }

TEMP_ROOT=""
ASSET_WORK=""
cleanup() {
  if [ -n "$TEMP_ROOT" ]; then rm -rf "$TEMP_ROOT"; fi
  if [ -n "$ASSET_WORK" ]; then rm -rf "$ASSET_WORK"; fi
}
trap cleanup EXIT

[ -x "$BIN" ] || fail "binary not built; run 'make build' first"

note "--version prints build info"
"$BIN" --version | grep -q . || fail "--version produced no output"

note "--help succeeds for every subcommand"
cmds="$("$BIN" --help 2>&1 | awk '
  /^Available Commands:/ { in_block = 1; next }
  in_block && /^[[:space:]]*$/ { in_block = 0 }
  in_block && /^[[:space:]]+[a-z]/ { print $1 }
')"
[ -n "$cmds" ] || fail "could not derive the subcommand list from --help"
for cmd in $cmds; do
  "$BIN" --root "$FIXTURE_ROOT" "$cmd" --help >/dev/null \
    || fail "$cmd --help exited non-zero"
done

note "root discovery works from a nested directory"
(cd "$FIXTURE_ROOT/sub/dir" && "$BIN" status --json >/dev/null) \
  || fail "root discovery from a nested directory failed"

note "status JSON passes through byte-for-byte and matches the contract types"
TEMP_ROOT="$(mktemp -d "${TMPDIR:-/tmp}/pic-sure-cli-status.XXXXXX")"
status_cli_file="$TEMP_ROOT/cli.json"
status_script_file="$TEMP_ROOT/script.json"
"$BIN" --root "$FIXTURE_ROOT" status --json > "$status_cli_file"
"$FIXTURE_ROOT/status.sh" --json > "$status_script_file"
cmp "$status_cli_file" "$status_script_file" \
  || fail "pic-sure status --json differs from status.sh output"
go run "$REPO_DIR/smoke/validate" status < "$status_cli_file" \
  || fail "status document rejected by contract parser"
rm -rf "$TEMP_ROOT"
TEMP_ROOT=""

note "human status output keeps its section skeleton"
human_status="$("$BIN" --root "$FIXTURE_ROOT" status)"
for section in \
  "== Environment ==" \
  "== Release Control ==" \
  "== Repos ==" \
  "== Compose ==" \
  "== Health ==" \
  "== Database ==" \
  "== Migrations =="; do
  case "$human_status" in
    *"$section"*) ;;
    *) fail "human status output is missing section: $section" ;;
  esac
done

note "preflight JSON parses and preserves its failure exit code"
set +e
preflight_cli="$("$BIN" --root "$FIXTURE_ROOT" preflight --json)"
preflight_rc=$?
set -e
[ "$preflight_rc" -eq 1 ] \
  || fail "preflight fixture should exit 1, got $preflight_rc"
printf '%s' "$preflight_cli" | go run "$REPO_DIR/smoke/validate" preflight \
  || fail "preflight document rejected by contract parser"

note "safe script dispatch reaches the selected checkout"
"$BIN" --root "$FIXTURE_ROOT" update --dry-run --offline >/dev/null \
  || fail "update fixture exited non-zero"

note "script exit codes propagate unchanged"
TEMP_ROOT="$(mktemp -d "${TMPDIR:-/tmp}/pic-sure-cli-smoke.XXXXXX")"
cp -R "$FIXTURE_ROOT/." "$TEMP_ROOT/"
printf '#!/usr/bin/env bash\nexit 7\n' > "$TEMP_ROOT/status.sh"
chmod +x "$TEMP_ROOT/status.sh"
set +e
"$BIN" --root "$TEMP_ROOT" status --json >/dev/null 2>&1
status_rc=$?
set -e
[ "$status_rc" -eq 7 ] \
  || fail "expected exit 7 from the stub status.sh, got $status_rc"
rm -rf "$TEMP_ROOT"
TEMP_ROOT=""

note "reset refuses a non-interactive run without --yes"
set +e
reset_output="$("$BIN" --root "$FIXTURE_ROOT" reset </dev/null 2>&1)"
reset_rc=$?
set -e
[ "$reset_rc" -eq 2 ] \
  || fail "reset without --yes should exit 2, got $reset_rc"
case "$reset_output" in
  *"pass --yes"*) ;;
  *) fail "reset refusal did not explain that --yes is required" ;;
esac

note "install.sh installs a host-built release and verifies its checksum"
case "$(uname -s)" in
  Linux) install_os=linux ;;
  Darwin) install_os=darwin ;;
  *) fail "install.sh smoke: unsupported OS $(uname -s)" ;;
esac
case "$(uname -m)" in
  x86_64|amd64) install_arch=amd64 ;;
  arm64|aarch64) install_arch=arm64 ;;
  *) fail "install.sh smoke: unsupported arch $(uname -m)" ;;
esac
asset="pic-sure_${install_os}_${install_arch}.tar.gz"

if command -v sha256sum >/dev/null 2>&1; then
  checksum_cmd="sha256sum"
elif command -v shasum >/dev/null 2>&1; then
  checksum_cmd="shasum -a 256"
else
  fail "install.sh smoke: neither sha256sum nor shasum is available"
fi

ASSET_WORK="$(mktemp -d "${TMPDIR:-/tmp}/pic-sure-cli-install.XXXXXX")"
asset_dir="$ASSET_WORK/assets"
bin_dir="$ASSET_WORK/bin"
mkdir -p "$asset_dir" "$bin_dir"

make -C "$REPO_DIR" build-release GOOS="$install_os" GOARCH="$install_arch" \
  OUT="$ASSET_WORK/pic-sure" >/dev/null 2>&1 \
  || fail "make build-release failed"
tar -C "$ASSET_WORK" -czf "$asset_dir/$asset" pic-sure
(cd "$asset_dir" && $checksum_cmd "$asset" > checksums.txt)

PIC_SURE_INSTALL_ASSET_DIR="$asset_dir" "$REPO_DIR/install.sh" \
  --bin-dir "$bin_dir" >/dev/null \
  || fail "install.sh failed against local assets"
[ -x "$bin_dir/pic-sure" ] \
  || fail "install.sh did not install an executable pic-sure"
"$bin_dir/pic-sure" --version | grep -q . \
  || fail "installed binary --version produced no output"

note "install.sh rejects a corrupted checksum"
printf '%s  %s\n' \
  "0000000000000000000000000000000000000000000000000000000000000000" \
  "$asset" > "$asset_dir/checksums.txt"
bad_bin_dir="$ASSET_WORK/bin-bad"
set +e
PIC_SURE_INSTALL_ASSET_DIR="$asset_dir" "$REPO_DIR/install.sh" \
  --bin-dir "$bad_bin_dir" >/dev/null 2>&1
install_rc=$?
set -e
[ "$install_rc" -ne 0 ] \
  || fail "install.sh accepted a corrupted checksum"
[ ! -e "$bad_bin_dir/pic-sure" ] \
  || fail "install.sh installed a binary despite a checksum mismatch"

rm -rf "$ASSET_WORK"
ASSET_WORK=""

note "PASS"
