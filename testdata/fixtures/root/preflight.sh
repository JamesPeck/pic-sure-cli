#!/usr/bin/env bash

set -euo pipefail

fixture_root="$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd)"

for arg in "$@"; do
  if [ "$arg" = "--json" ]; then
    cat "$fixture_root/../preflight.json"
    exit 1
  fi
done

printf '%s\n' 'Preflight fixture failed as expected.'
exit 1
