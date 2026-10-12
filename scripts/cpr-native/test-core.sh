#!/usr/bin/env bash
# Test the real stdlib-only CPR core in a temporary module.
# This is NOT a full sub2api, database, or CPR integration test.
set -euo pipefail
repo=$(cd -- "$(dirname -- "${BASH_SOURCE[0]}")/../.." && pwd)
command -v go >/dev/null || { echo 'Go is required.' >&2; exit 1; }
tmp=$(mktemp -d "${TMPDIR:-/tmp}/sub2api-cpr-core.XXXXXXXX")
trap 'rm -rf -- "$tmp"' EXIT
cp -- "$repo"/backend/internal/pkg/cprnative/*.go "$tmp/"
printf 'module cpr-native-core-check\n\ngo 1.23\n' > "$tmp/go.mod"
cd -- "$tmp"
GOTOOLCHAIN=local GOPROXY=off go test -race -count=1 -v ./...
