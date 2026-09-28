#!/usr/bin/env bash
set -euo pipefail

if [[ $# -ne 5 ]]; then
  echo "usage: $0 VERSION PUBLIC_KEY PRIVATE_KEY RELEASE_BASE_URL OUTPUT_DIR" >&2
  exit 2
fi

release_version="$1"
release_public_key="$2"
release_private_key="$3"
release_base_url="${4%/}"
release_output_dir="$5"

actual_public_key="$(go run ./tools/release-manifest public-key --private "$release_private_key")"
if [[ "$actual_public_key" != "$release_public_key" ]]; then
  echo "public key does not match release signing key" >&2
  exit 1
fi

mkdir -p "$release_output_dir"

for target in windows/amd64 linux/amd64 linux/arm64 darwin/arm64; do
  target_os="${target%/*}"
  target_arch="${target#*/}"
  extension=""
  if [[ "$target_os" == "windows" ]]; then
    extension=".exe"
  fi
  artifact_name="db-agent-${release_version}-${target_os}-${target_arch}${extension}"
  artifact_path="${release_output_dir}/${artifact_name}"

  CGO_ENABLED=0 GOOS="$target_os" GOARCH="$target_arch" go build -trimpath \
    -ldflags "-X github.com/fivemanage/db-agent/cmd.version=${release_version} -X github.com/fivemanage/db-agent/cmd.updatePublicKey=${release_public_key}" \
    -o "$artifact_path" .

  go run ./tools/release-manifest sign \
    --private "$release_private_key" \
    --artifact "$artifact_path" \
    --version "$release_version" \
    --os "$target_os" \
    --arch "$target_arch" \
    --url "${release_base_url}/${artifact_name}" \
    --output "${release_output_dir}/${target_os}-${target_arch}.json"
done
