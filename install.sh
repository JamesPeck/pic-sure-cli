#!/usr/bin/env bash
# =============================================================================
# pic-sure CLI v2: Installer
# =============================================================================
# Installs the newest v2 release of pic-sure (or a pinned one) for this
# OS/architecture. It verifies the archive against checksums.txt and, when
# cosign 3.0 or newer is installed, verifies checksums.txt against its
# Sigstore bundle. An older cosign can't read the bundle, and is treated
# like no cosign: a warning, and the checksum only.
#
#   curl -fsSL https://raw.githubusercontent.com/JamesPeck/pic-sure-cli/main/install.sh | bash
#   curl -fsSL .../install.sh | bash -s -- --version v2.0.0
#
# Usage:
#   install.sh                      # newest stable v2.x.y release → ~/.local/bin
#   install.sh --bin-dir /usr/local/bin
#   install.sh --version v2.0.0
#   install.sh --repo OWNER/NAME    # override the GitHub repository
#
# Environment:
#   PIC_SURE_REQUIRE_SIGNATURE=1 fail, rather than warn, when no cosign 3.0
#                                or newer is here to check the signature
#
# Test hooks (environment), for a local mirror of GitHub:
#   PIC_SURE_INSTALL_GITHUB_URL  replaces https://github.com in download URLs;
#                                any URL curl reads, file:// included
#   PIC_SURE_INSTALL_API_URL     replaces https://api.github.com
# =============================================================================

set -euo pipefail

# Everything runs from main, called on the last line inside braces, so a
# download cut short anywhere, even within that line, installs nothing.

REPO="JamesPeck/pic-sure-cli"
BIN_DIR="$HOME/.local/bin"
VERSION=""
GITHUB_URL="${PIC_SURE_INSTALL_GITHUB_URL:-https://github.com}"
API_URL="${PIC_SURE_INSTALL_API_URL:-https://api.github.com}"

# Asset names are a contract with .goreleaser.yaml and self-update.
CHECKSUMS="checksums.txt"
BUNDLE="checksums.txt.sigstore.json"
OIDC_ISSUER="https://token.actions.githubusercontent.com"
# The oldest cosign that reads the release's bundle (cosign 3's new bundle
# format). self-update has the same minimum (internal/selfupdate).
MIN_COSIGN_MAJOR=3

say() { printf '%s\n' "$*"; }
fail() { printf 'install.sh: %s\n' "$*" >&2; exit 1; }

usage() {
  cat <<'EOF'
Install the pic-sure CLI (v2).

Usage: install.sh [--version vX.Y.Z] [--bin-dir DIR] [--repo OWNER/NAME]

  --version   release tag to install (default: the newest stable v2.x.y release)
  --bin-dir   where to put the binary (default: ~/.local/bin)
  --repo      GitHub repository to install from (default: JamesPeck/pic-sure-cli)

Set PIC_SURE_REQUIRE_SIGNATURE=1 to fail, rather than warn, when cosign 3.0
or newer isn't installed to check the release's signature.
EOF
}

fetch() { # URL DEST
  curl -fsSL --retry 3 --proto-redir '=https' -o "$2" "$1"
}

# cosign_version prints the installed cosign's version without the v, or
# nothing if `cosign version` doesn't say.
cosign_version() {
  { cosign version 2>&1 || true; } | awk '$1 == "GitVersion:" { sub(/^v/, "", $2); print $2; exit }'
}

# unchecked WHY FIX: no usable cosign can check the signature. That's a
# failure when PIC_SURE_REQUIRE_SIGNATURE is set, and a warning otherwise.
unchecked() {
  if [ "$REQUIRE_SIGNATURE" = true ]; then
    fail "$1, and PIC_SURE_REQUIRE_SIGNATURE requires a checked signature; $2 and run the installer again"
  fi
  say "WARNING: $1, so the signature on $CHECKSUMS wasn't checked;" >&2
  say "  relying on the checksum only ($2 to check it, or see below to verify by hand)." >&2
}

main() {
  while [ "$#" -gt 0 ]; do
    case "$1" in
      --bin-dir)
        [ -n "${2:-}" ] || fail "--bin-dir requires a directory"
        BIN_DIR="$2"
        shift 2
        ;;
      --bin-dir=*) BIN_DIR="${1#*=}"; shift ;;
      --version)
        [ -n "${2:-}" ] || fail "--version requires a tag (e.g. v2.0.0)"
        VERSION="$2"
        shift 2
        ;;
      --version=*) VERSION="${1#*=}"; shift ;;
      --repo)
        [ -n "${2:-}" ] || fail "--repo requires OWNER/NAME"
        REPO="$2"
        shift 2
        ;;
      --repo=*) REPO="${1#*=}"; shift ;;
      -h|--help)
        usage
        exit 0
        ;;
      *)
        fail "unknown option: $1"
        ;;
    esac
  done

  case "$VERSION" in
    [0-9]*) VERSION="v$VERSION" ;;
  esac

  # Any value but empty, 0 or false turns it on, so a typo can't weaken it.
  case "${PIC_SURE_REQUIRE_SIGNATURE:-}" in
    ""|0|false) REQUIRE_SIGNATURE=false ;;
    *) REQUIRE_SIGNATURE=true ;;
  esac

  # --- platform detection ----------------------------------------------------
  case "$(uname -s)" in
    Linux) OS=linux ;;
    Darwin) OS=darwin ;;
    *) fail "unsupported OS: $(uname -s) (linux and darwin are supported)" ;;
  esac

  case "$(uname -m)" in
    x86_64|amd64) ARCH=amd64 ;;
    arm64|aarch64) ARCH=arm64 ;;
    *) fail "unsupported architecture: $(uname -m) (amd64 and arm64 are supported)" ;;
  esac

  ASSET="pic-sure_${OS}_${ARCH}.tar.gz"

  # --- tools -----------------------------------------------------------------
  command -v curl >/dev/null 2>&1 || fail "curl is required"

  if command -v sha256sum >/dev/null 2>&1; then
    CHECKSUM_CMD="sha256sum"
  elif command -v shasum >/dev/null 2>&1; then
    CHECKSUM_CMD="shasum -a 256"
  else
    fail "neither sha256sum nor shasum is available; install one to verify the download"
  fi

  TMP="$(mktemp -d "${TMPDIR:-/tmp}/pic-sure-install.XXXXXX")"
  trap 'rm -rf "$TMP"' EXIT

  # --- choose the release ----------------------------------------------------
  # Without --version, install the newest stable v2.x.y. GitHub's "latest"
  # release could belong to another major line, so pick from the list.
  # `pic-sure self-update` picks by the same rule (internal/selfupdate).
  # tag_name precedes prerelease in each release object, and nested objects
  # (author, assets) have neither.
  if [ -z "$VERSION" ]; then
    say "Finding the newest v2 release of $REPO..."
    : >"$TMP/releases.json"
    for page in 1 2 3 4 5 6 7 8 9 10; do
      fetch "$API_URL/repos/$REPO/releases?per_page=100&page=$page" "$TMP/page.json" \
        || fail "could not list releases of $REPO; pass --version vX.Y.Z to choose one"
      cat "$TMP/page.json" >>"$TMP/releases.json"
      [ "$(grep -o '"tag_name":' "$TMP/page.json" | grep -c .)" -ge 100 ] || break
    done
    VERSION="$(grep -oE '"(tag_name|prerelease)": *("[^"]*"|true|false)' "$TMP/releases.json" \
      | awk -F'"' '$2 == "tag_name" { tag = $4 } $2 == "prerelease" && $3 ~ /false/ { print tag }' \
      | grep -E '^v2\.[0-9]+\.[0-9]+$' \
      | sort -t. -k2,2n -k3,3n \
      | tail -n 1 || true)"
    [ -n "$VERSION" ] || fail "$REPO has no v2.x.y release; pass --version to choose one"
  fi

  BASE_URL="$GITHUB_URL/$REPO/releases/download/$VERSION"
  # The release workflow signs with the tag's own identity, so a bundle from
  # any other tag or workflow fails verification.
  IDENTITY="https://github.com/$REPO/.github/workflows/release.yml@refs/tags/$VERSION"

  # --- download --------------------------------------------------------------
  say "Downloading pic-sure $VERSION ($ASSET) from $BASE_URL..."
  fetch "$BASE_URL/$ASSET" "$TMP/$ASSET" \
    || fail "download failed: $BASE_URL/$ASSET (is there a release with this asset?)"
  fetch "$BASE_URL/$CHECKSUMS" "$TMP/$CHECKSUMS" \
    || fail "download failed: $BASE_URL/$CHECKSUMS"
  have_bundle=false
  if fetch "$BASE_URL/$BUNDLE" "$TMP/$BUNDLE" 2>"$TMP/bundle.err"; then
    have_bundle=true
  fi

  # --- verify the signature on checksums.txt ---------------------------------
  # Every v2 release is signed, so a v2 release without a bundle is refused
  # even when cosign isn't here to check it (self-update does the same).
  if [ "$have_bundle" = false ]; then
    case "$VERSION" in
      v2.*)
        cat "$TMP/bundle.err" >&2
        fail "could not download $BUNDLE for $VERSION, and every v2 release is signed; aborting"
        ;;
      *)
        if [ "$REQUIRE_SIGNATURE" = true ]; then
          fail "release $VERSION has no $BUNDLE, and PIC_SURE_REQUIRE_SIGNATURE requires a checked signature"
        fi
        say "Release $VERSION has no $BUNDLE; verifying the checksum only."
        ;;
    esac
  elif ! command -v cosign >/dev/null 2>&1; then
    unchecked "cosign isn't installed" "install cosign 3.0 or newer"
  else
    cosign_ver="$(cosign_version)"
    case "${cosign_ver%%.*}" in
      ''|*[!0-9]*) too_old=false ;; # unreadable: let cosign try
      *) if [ "${cosign_ver%%.*}" -lt "$MIN_COSIGN_MAJOR" ]; then too_old=true; else too_old=false; fi ;;
    esac
    if [ "$too_old" = true ]; then
      unchecked "cosign $cosign_ver is older than 3.0 and can't check this release's signature" "upgrade cosign"
    else
      say "Verifying the signature on $CHECKSUMS (cosign)..."
      if ! out="$(cosign verify-blob --bundle "$TMP/$BUNDLE" \
          --certificate-identity "$IDENTITY" \
          --certificate-oidc-issuer "$OIDC_ISSUER" \
          "$TMP/$CHECKSUMS" 2>&1)"; then
        printf '%s\n' "$out" >&2
        case "$out" in
          *"trusted root"*)
            fail "cosign couldn't fetch Sigstore's trust root to check the signature on $CHECKSUMS; that's a network or proxy problem (cosign uses HTTPS_PROXY), not a bad signature; aborting"
            ;;
        esac
        fail "signature verification FAILED for $CHECKSUMS (expected signer $IDENTITY); aborting"
      fi
    fi
  fi

  # --- verify the archive ----------------------------------------------------
  # Compare hashes explicitly rather than via `-c`, whose handling of
  # malformed lines varies between implementations (some warn and exit 0).
  say "Verifying checksum ($CHECKSUM_CMD)..."
  expected_hash="$(awk -v asset="$ASSET" '$2 == asset || $2 == "*"asset {print $1}' "$TMP/$CHECKSUMS")"
  [ -n "$expected_hash" ] || fail "$CHECKSUMS has no entry for $ASSET"
  actual_hash="$($CHECKSUM_CMD "$TMP/$ASSET" | awk '{print $1}')"
  if [ "$actual_hash" != "$expected_hash" ]; then
    fail "checksum verification FAILED for $ASSET (expected $expected_hash, got $actual_hash); aborting"
  fi

  # --- install ---------------------------------------------------------------
  mkdir "$TMP/x"
  tar -C "$TMP/x" -xzf "$TMP/$ASSET"
  [ -f "$TMP/x/pic-sure" ] || fail "archive did not contain the pic-sure binary"

  mkdir -p "$BIN_DIR"
  install -m 0755 "$TMP/x/pic-sure" "$BIN_DIR/pic-sure"

  say ""
  say "Installed: $BIN_DIR/pic-sure"
  "$BIN_DIR/pic-sure" --version || true

  say ""
  say "To verify this release by hand, download $ASSET, $CHECKSUMS and"
  say "$BUNDLE from $GITHUB_URL/$REPO/releases/tag/$VERSION, then run:"
  say "  cosign verify-blob --bundle $BUNDLE \\"
  say "    --certificate-identity $IDENTITY \\"
  say "    --certificate-oidc-issuer $OIDC_ISSUER $CHECKSUMS"
  say "  $CHECKSUM_CMD --ignore-missing -c $CHECKSUMS"
  say "  gh attestation verify $ASSET --repo $REPO"

  case ":$PATH:" in
    *":$BIN_DIR:"*) ;;
    *)
      say ""
      say "Note: $BIN_DIR is not on your PATH. Add it with:"
      say "  bash/zsh:  export PATH=\"$BIN_DIR:\$PATH\""
      say "  fish:      fish_add_path \"$BIN_DIR\""
      ;;
  esac

  say ""
  say "Get started:"
  say "  mkdir my-stack && cd my-stack && pic-sure   # opens the setup wizard"
  say "  pic-sure help"
  say "For a scripted setup with pic-sure init, see the Quick start in"
  say "  https://github.com/$REPO#quick-start"
}

{ main "$@"; }
