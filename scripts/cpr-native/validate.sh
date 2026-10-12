#!/usr/bin/env bash
# Build and selected unit tests only. Never deploys, publishes, or connects to
# real CPR accounts/production databases. Logs and build output stay local.
set -euo pipefail
umask 077
repo=$(cd -- "$(dirname -- "${BASH_SOURCE[0]}")/../.." && pwd)
for tool in git python3 go gofmt pnpm node tar; do
  command -v "$tool" >/dev/null || { echo "Missing required tool: $tool" >&2; exit 1; }
done
if [[ -n $(git -C "$repo" status --porcelain) ]]; then
  echo 'Validate a clean committed checkout; this script exports HEAD, not uncommitted files.' >&2
  exit 1
fi
head=$(git -C "$repo" rev-parse HEAD)
out=$(mktemp -d "${TMPDIR:-/tmp}/sub2api-cpr-validation.XXXXXXXX")
mkdir -p -- "$out/source"
exec > >(tee "$out/build.log") 2>&1
printf 'Source commit: %s\nOutput directory: %s\n' "$head" "$out"
# Export tracked source only, not local .env/config/credentials or Git settings.
git -C "$repo" archive --format=tar "$head" | tar -x -C "$out/source"
src="$out/source"
go version
node --version
pnpm --version
printf 'Required Go version: '
awk '$1=="go" { print $2 }' "$src/backend/go.mod"
python3 "$src/scripts/cpr-native/prepare.py" --repo "$src"
python3 "$src/scripts/cpr-native/prepare.py" --repo "$src" --check
bash "$src/scripts/cpr-native/test-core.sh"
(
  cd -- "$src/frontend"
  pnpm install --frozen-lockfile
  pnpm run typecheck
  pnpm exec vitest run src/components/account/__tests__/CPRUpstreamEntry.spec.ts
  pnpm run build
)
(
  cd -- "$src/backend"
  go test -race -count=1 ./internal/pkg/cprnative
  go test -race -count=1 -run '^TestCPRNative' ./internal/service
  # Compile test harnesses without running unrelated database TestMain hooks.
  go test -c -o "$out/handler.test" ./internal/handler
  go test -c -o "$out/routes.test" ./internal/server/routes
  CGO_ENABLED=0 go build -tags embed -trimpath -o "$out/sub2api-cpr-preview" ./cmd/server
)
python3 - "$out/result.json" "$head" <<'PY'
import datetime
import json
import pathlib
import sys
pathlib.Path(sys.argv[1]).write_text(json.dumps({
    'commit': sys.argv[2],
    'utc': datetime.datetime.now(datetime.timezone.utc).isoformat(),
    'status': 'build-and-selected-unit-tests-passed',
    'database_billing_e2e': 'NOT_RUN',
    'real_cpr_e2e': 'NOT_RUN',
    'websocket_support': False,
    'production_approved': False,
}, indent=2) + '\n', encoding='utf-8')
PY
printf '\nBuild and selected unit tests passed for %s.\n' "$head"
printf 'Reports: %s\n' "$out"
echo 'Real CPR, database billing, concurrency load and failure recovery acceptance remain required.'
