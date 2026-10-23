#!/bin/bash
set -euo pipefail

PATH=/usr/local/sbin:/usr/local/bin:/usr/sbin:/usr/bin:/sbin:/bin
export PATH
umask 077

# Provisioned by release-manifest render-installer, not by the customer or feed.
release_public_key=''
release_base_url=''
release_channel='stable'
work=''

die() { printf 'Installation stopped: %s\n' "$1" >&2; exit 1; }

cleanup() {
  if [ -n "$work" ]; then
    rm -f -- "$work/SHA256SUMS" "$work/SHA256SUMS.sig" "$work/public.raw" \
      "$work/public.der" "$work/signature" "$work/payload" "$work/artifact"
    rmdir -- "$work"
    work=''
  fi
}
trap cleanup EXIT
trap 'exit 1' HUP INT TERM

protected_directory() {
  path="$1"
  [ -d "$path" ] && [ ! -L "$path" ] || die "Installation/config paths must be regular directories, not links."
  [ "$(stat -c %u -- "$path")" = 0 ] || die "Installation/config directories must be owned by root."
  permissions="$(stat -c %a -- "$path")"
  [ "$((0$permissions & 022))" -eq 0 ] || die "Installation/config directories must not be group/world writable."
}

download() {
  if ! curl --fail --silent --show-error --location --proto '=https' --proto-redir '=https' \
    --tlsv1.2 --connect-timeout 30 --max-time 300 --max-filesize "$3" \
    "$1" | head -c "$(($3 + 1))" > "$2"; then
    die "Release download failed; check HTTPS certificates, network/proxy access, release availability, size limits, and disk space."
  fi
  # Older curl versions enforce --max-filesize only with Content-Length.
  [ "$(wc -c < "$2")" -le "$3" ] || die "Release download exceeds its size limits."
}

main() {
  [ "$(uname -s)" = Linux ] || die "This installer supports Linux only; Windows needs PowerShell."
  case "$(uname -m)" in
    x86_64|amd64) arch=amd64 ;;
    aarch64|arm64) arch=arm64 ;;
    *) die "Unsupported Linux architecture; only amd64 and arm64 are supported." ;;
  esac
  [ "$(id -u)" = 0 ] || die "Installation requires root; rerun with sudo."
  [ -d /run/systemd/system ] && command -v systemctl >/dev/null 2>&1 \
    || die "A running systemd system is required."
  for command in curl openssl sha256sum awk stat mktemp timeout sync head; do
    command -v "$command" >/dev/null 2>&1 || die "Required installation tool is missing: $command."
  done

  if [ -z "$release_public_key" ] && [ -z "$release_base_url" ]; then
    # Only an unprovisioned development copy accepts explicit test settings.
    release_public_key="${DB_AGENT_RELEASE_PUBLIC_KEY:-}"
    release_base_url="${DB_AGENT_RELEASE_BASE_URL:-}"
  fi
  [ -n "$release_public_key" ] && [ -n "$release_base_url" ] \
    || die "Release hosting/signing trust is not provisioned; obtain the published FiveManage installer."
  case "$release_channel" in stable|staging) ;; *) die "Invalid installer release channel." ;; esac
  release_base_url="${release_base_url%/}"
  case "$release_base_url" in
    https://?*) ;;
    *) die "A public HTTPS GitHub release directory is required." ;;
  esac
  case "$release_base_url" in
    *'@'*|*'?'*|*'#'*|*[[:space:]]*) die "Release URLs must not contain credentials, query strings, fragments, or whitespace." ;;
  esac
  executable=/opt/db-agent/db-agent
  [ ! -e "$executable" ] && [ ! -L "$executable" ] \
    || die "An installation already exists. Resume with /opt/db-agent/db-agent install or use its signed update command; this script will not overwrite it."
  interactive=true
  for argument do
    if [ "$argument" = --service-only ]; then interactive=false; fi
  done
  if [ "$interactive" = true ]; then
    [ -t 1 ] && [ -t 2 ] || die "Interactive setup needs an unredirected terminal with recording disabled."
    (: </dev/tty) 2>/dev/null || die "Interactive setup needs a controlling terminal."
  fi

  for path in / /opt /etc; do protected_directory "$path"; done
  if [ ! -e /opt/db-agent ] && [ ! -L /opt/db-agent ]; then
    mkdir -m 755 /opt/db-agent
    chmod 755 /opt/db-agent
  fi
  if [ ! -e /etc/db-agent ] && [ ! -L /etc/db-agent ]; then mkdir -m 700 /etc/db-agent; fi
  protected_directory /opt/db-agent
  protected_directory /etc/db-agent
  permissions="$(stat -c %a -- /etc/db-agent)"
  [ "$((0$permissions & 077))" -eq 0 ] || die "Existing /etc/db-agent must be private (0700); inspect it before retrying."
  work="$(mktemp -d /opt/db-agent/.install-XXXXXXXX)"

  download "$release_base_url/SHA256SUMS" "$work/SHA256SUMS" 65536
  download "$release_base_url/SHA256SUMS.sig" "$work/SHA256SUMS.sig" 1024
  printf '%s' "$release_public_key" | openssl base64 -d -A > "$work/public.raw" \
    || die "The pinned release public key is invalid."
  [ "$(wc -c < "$work/public.raw")" -eq 32 ] || die "The pinned Ed25519 public key must be 32 bytes."
  printf '\060\052\060\005\006\003\053\145\160\003\041\000' > "$work/public.der"
  cat "$work/public.raw" >> "$work/public.der"
  tr -d '\r\n' < "$work/SHA256SUMS.sig" | openssl base64 -d -A > "$work/signature" \
    || die "The release signature encoding is invalid."
  [ "$(wc -c < "$work/signature")" -eq 64 ] || die "The release signature is invalid."
  { printf 'fivemanage-db-agent-checksums-v1\n'; cat "$work/SHA256SUMS"; } > "$work/payload"
  openssl pkeyutl -verify -pubin -keyform DER -inkey "$work/public.der" -rawin \
    -in "$work/payload" -sigfile "$work/signature" >/dev/null 2>&1 \
    || die "The release signature is invalid or OpenSSL lacks Ed25519 support. Nothing was installed."

  # Parse filenames only after authenticating the existing release inventory.
  # One exact target match also prevents path traversal or ambiguous selection.
  record="$(awk -v arch="$arch" -v channel="$release_channel" '
    BEGIN { n="(0|[1-9][0-9]*)"; suffix=(channel == "staging" ? "-rc[.]" n : ""); target="^db-agent-" n "[.]" n "[.]" n suffix "-linux-" arch "$" }
    $2 ~ target {
      count++
      if (NF != 2 || length($1) != 64 || $1 ~ /[^0-9a-f]/ || substr($0,65,2) != "  ") invalid=1
      digest=$1; name=$2
    }
    END { if (count != 1 || invalid) exit 1; print digest " " name }
  ' "$work/SHA256SUMS")" || die "The signed inventory is missing or ambiguous for this Linux platform."
  digest="${record%% *}"
  name="${record#* }"
  version="${name#db-agent-}"
  version="${version%-linux-$arch}"
  download "$release_base_url/$name" "$work/artifact" 268435456
  (cd "$work"; printf '%s  artifact\n' "$digest" | sha256sum --check --status) \
    || die "Release checksum mismatch. No downloaded code was executed or installed."
  chmod 755 "$work/artifact"
  reported="$(timeout 15 "$work/artifact" version)" || die "The verified executable could not report its version. Nothing was installed."
  [ "$reported" = "$version" ] || die "The verified executable reports an unexpected version. Nothing was installed."
  sync -f "$work/artifact"
  ln -- "$work/artifact" "$executable" || die "Could not publish without overwriting an existing installation."
  sync -f /opt/db-agent
  cleanup
  printf 'Verified db-agent %s installed at %s. Starting setup; service startup is not proof of a completed backup.\n' "$version" "$executable"
  if [ "$interactive" = true ]; then
    exec "$executable" install "$@" </dev/tty
  else
    exec "$executable" install "$@"
  fi
}

main "$@"
