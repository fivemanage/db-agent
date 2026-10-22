#!/usr/bin/env bash
set -euo pipefail

repo_dir="$(cd "$(dirname "${BASH_SOURCE[0]}")/.." && pwd)"
project="db-agent-restore-proof-$(date +%s)-$$"
temp_root="${TMPDIR:-/tmp}"
if [[ -d /tmp/opencode ]]; then
  temp_root=/tmp/opencode
fi
work_dir="$(mktemp -d "$temp_root/db-agent-restore-proof.XXXXXX")"
compose=(docker compose --project-name "$project" --file "$repo_dir/docker-compose.restore-proof.yml")

cleanup() {
  "${compose[@]}" down --timeout 10
  rm -f "$work_dir/restoreproof.test" "$work_dir/marker"
  rmdir "$work_dir"
}
trap cleanup EXIT

arch="$(docker info --format '{{.Architecture}}')"
case "$arch" in
  x86_64|amd64) arch=amd64 ;;
  aarch64|arm64) arch=arm64 ;;
  *) echo "unsupported Docker architecture: $arch" >&2; exit 1 ;;
esac

cd "$repo_dir"
CGO_ENABLED=0 GOOS=linux GOARCH="$arch" go test -c -tags integration \
  -o "$work_dir/restoreproof.test" ./test/restoreproof
printf '%s' "$project" > "$work_dir/marker"
chmod 600 "$work_dir/marker"

"${compose[@]}" up --detach --wait --wait-timeout 180
result=0
for engine in mariadb mysql; do
  "${compose[@]}" cp "$work_dir/restoreproof.test" "$engine:/tmp/restoreproof.test"
  "${compose[@]}" cp "$work_dir/marker" "$engine:/tmp/db-agent-restore-proof"
  if ! "${compose[@]}" exec -T \
    --env DB_AGENT_RESTORE_PROOF_ID="$project" \
    --env DB_AGENT_RESTORE_PROOF_ENGINE="$engine" \
    --env DB_AGENT_RESTORE_BRIDGE="${DB_AGENT_RESTORE_BRIDGE:-0}" \
    "$engine" /tmp/restoreproof.test -test.v -test.timeout=3m "$@"; then
    result=1
  fi
done
exit "$result"
