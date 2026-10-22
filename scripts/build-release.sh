#!/usr/bin/env bash
set -euo pipefail

if [[ $# -ne 4 ]]; then
  echo "usage: $0 VERSION PUBLIC_KEY API_URL OUTPUT_DIR" >&2
  exit 2
fi

release_version="$1"
release_public_key="$2"
release_api_url="$3"
release_output_dir="$4"

go run ./tools/release-manifest validate --version "$release_version" \
  --public-key "$release_public_key" --api-url "$release_api_url"

if [[ -e "$release_output_dir" ]]; then
  echo "release output directory must not already exist" >&2
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

  CGO_ENABLED=0 GOOS="$target_os" GOARCH="$target_arch" GOAMD64=v1 GOARM64=v8.0 GOFLAGS='' GOEXPERIMENT='' \
    go build -mod=readonly -trimpath -buildvcs=false \
    -ldflags "-X github.com/fivemanage/db-agent/cmd.version=${release_version} -X github.com/fivemanage/db-agent/cmd.updatePublicKey=${release_public_key} -X github.com/fivemanage/db-agent/cmd.defaultAPIURL=${release_api_url}" \
    -o "$artifact_path" .
done
