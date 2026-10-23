#!/usr/bin/env bash
set -euo pipefail

repo_dir="$(cd "$(dirname "${BASH_SOURCE[0]}")/.." && pwd)"
temp_root="${TMPDIR:-/tmp}"
if [[ -d /tmp/opencode ]]; then temp_root=/tmp/opencode; fi
work="$(mktemp -d "$temp_root/db-agent-installer-proof.XXXXXXXX")"
image="db-agent-linux-installer-proof:$(date +%s)-$$"
cleanup() {
  docker image rm "$image" >/dev/null 2>&1 || true
  rm -f "$work/install-linux.sh" "$work/test_installer.py"
  rmdir "$work"
}
trap cleanup EXIT

# Send only the bootstrap and public test fixtures to Docker, never local config.
cp "$repo_dir/scripts/install-linux.sh" "$work/install-linux.sh"
cp "$repo_dir/test/linuxinstaller/test_installer.py" "$work/test_installer.py"
docker build --file "$repo_dir/test/linuxinstaller/Dockerfile" --tag "$image" "$work"
docker run --rm --network none --env DB_AGENT_INSTALLER_PROOF=1 "$image"
