#!/usr/bin/env bash

set -euo pipefail

fixture_root="$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd)"

for arg in "$@"; do
  if [ "$arg" = "--json" ]; then
    exec cat "$fixture_root/../status.json"
  fi
done

printf '%s\n' \
  '== Environment ==' \
  '== Release Control ==' \
  '== Repos ==' \
  '== Compose ==' \
  '== Health ==' \
  '== Database ==' \
  '== Migrations =='
