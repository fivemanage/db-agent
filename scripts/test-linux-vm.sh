#!/usr/bin/env bash
set -euo pipefail

if [[ $# -ne 1 ]]; then
  echo "usage: $0 TRUSTED_UBUNTU_24_04_AMD64_CLOUD_IMAGE" >&2
  exit 2
fi
repo_dir="$(cd "$(dirname "${BASH_SOURCE[0]}")/.." && pwd)"
image="$(realpath "$1")"
temp_root="${TMPDIR:-/tmp}"
if [[ -d /tmp/opencode ]]; then temp_root=/tmp/opencode; fi
work="$(mktemp -d "$temp_root/db-agent-linux-vm.XXXXXXXX")"
vm_pid=""
cleanup() {
  if [[ -n "$vm_pid" ]]; then
    kill "$vm_pid" 2>/dev/null || true
    wait "$vm_pid" 2>/dev/null || true
  fi
  rm -f "$work/ssh-key" "$work/signing.key" "$work/overlay.qcow2"
}
trap cleanup EXIT
echo "Disposable VM proof workspace: $work"

for command in qemu-system-x86_64 qemu-img genisoimage ssh scp ssh-keygen python3 timeout; do
  command -v "$command" >/dev/null
done
ssh-keygen -q -t ed25519 -N '' -f "$work/ssh-key"
public_ssh="$(cat "$work/ssh-key.pub")"
cat > "$work/user-data" <<EOF
#cloud-config
users:
  - name: proof
    groups: [sudo]
    shell: /bin/bash
    sudo: ALL=(ALL) NOPASSWD:ALL
    ssh_authorized_keys:
      - $public_ssh
ssh_pwauth: false
disable_root: true
EOF
printf 'instance-id: db-agent-linux-proof\nlocal-hostname: db-agent-linux-proof\n' > "$work/meta-data"
genisoimage -quiet -output "$work/seed.iso" -volid cidata -joliet -rock "$work/user-data" "$work/meta-data"
format="$(qemu-img info --output=json "$image" | python3 -c 'import json,sys; print(json.load(sys.stdin)["format"])')"
qemu-img create -q -f qcow2 -F "$format" -b "$image" "$work/overlay.qcow2"
qemu-img resize -q "$work/overlay.qcow2" 12G
port="$(python3 -c 'import socket; s=socket.socket(); s.bind(("127.0.0.1",0)); print(s.getsockname()[1]); s.close()')"
accel=(-accel tcg -cpu max)
if [[ -r /dev/kvm && -w /dev/kvm ]]; then accel=(-enable-kvm -cpu host); fi
qemu-system-x86_64 "${accel[@]}" -smp 2 -m 2048 -display none -monitor none \
  -serial "file:$work/serial.log" \
  -drive "file=$work/overlay.qcow2,format=qcow2,if=virtio" \
  -drive "file=$work/seed.iso,format=raw,media=cdrom,if=virtio" \
  -netdev "user,id=net0,restrict=on,hostfwd=tcp:127.0.0.1:$port-:22" \
  -device virtio-net-pci,netdev=net0 > "$work/qemu.log" 2>&1 &
vm_pid=$!
ssh_options=(-F /dev/null -i "$work/ssh-key" -o IdentitiesOnly=yes -o BatchMode=yes -o ConnectTimeout=3 -o ServerAliveInterval=5 -o ServerAliveCountMax=2 -o StrictHostKeyChecking=accept-new -o "UserKnownHostsFile=$work/known_hosts")
remote=(ssh "${ssh_options[@]}" -p "$port" proof@127.0.0.1)
ready=false
deadline=$((SECONDS + 240))
for ((attempt=0; attempt<120 && SECONDS<deadline; attempt++)); do
  if timeout 10 "${remote[@]}" true 2>"$work/ssh-readiness.log"; then ready=true; break; fi
  if ! kill -0 "$vm_pid" 2>/dev/null; then cat "$work/qemu.log"; exit 1; fi
  sleep 2
done
if [[ "$ready" != true ]]; then cat "$work/ssh-readiness.log"; echo "VM SSH did not start; inspect $work/serial.log" >&2; exit 1; fi
timeout 180 "${remote[@]}" 'sudo cloud-init status --wait'

cd "$repo_dir"
go run ./tools/release-manifest keygen --private "$work/signing.key" > "$work/public.key"
public_key="$(cat "$work/public.key")"
bash scripts/build-release.sh 1.2.3 "$public_key" https://api.example.com "$work/release"
for target in linux/amd64 linux/arm64 windows/amd64; do
  os="${target%/*}"
  arch="${target#*/}"
  suffix=""; if [[ "$os" == windows ]]; then suffix=.exe; fi
  name="db-agent-1.2.3-$os-$arch$suffix"
  go run ./tools/release-manifest sign --private "$work/signing.key" --artifact "$work/release/$name" \
    --version 1.2.3 --os "$os" --arch "$arch" --url "https://releases.example.com/1.2.3/$name" --output "$work/release/$os-$arch.json"
done
printf '{"api_url":"https://api.example.com","go_version":"%s"}\n' "$(go env GOVERSION)" > "$work/release/version.json"
CGO_ENABLED=0 GOOS=linux GOARCH=amd64 go build -trimpath -buildvcs=false \
  -ldflags '-X github.com/fivemanage/db-agent/cmd.version=0.0.0' -o "$work/baseline" .
CGO_ENABLED=0 GOOS=linux GOARCH=amd64 go test -c -tags release_smoke -o "$work/release-proof" ./test/releaseproof
scp "${ssh_options[@]}" -P "$port" -r "$work/release" "$work/baseline" "$work/release-proof" proof@127.0.0.1:
timeout 360 "${remote[@]}" "set -eu; chmod +x release/db-agent-* baseline release-proof; sudo env DB_AGENT_RELEASE_DIR=/home/proof/release DB_AGENT_RELEASE_VERSION=1.2.3 DB_AGENT_RELEASE_PUBLIC_KEY='$public_key' DB_AGENT_RELEASE_BASELINE=/home/proof/baseline DB_AGENT_RELEASE_SERVICE_SMOKE=1 ./release-proof -test.v -test.timeout=5m" \
  | tee "$work/service-proof.log"

# Check actual boot persistence separately from the in-process heartbeat server.
"${remote[@]}" 'set -eu; sudo install -d -m 755 /opt/db-agent; sudo install -m 755 release/db-agent-1.2.3-linux-amd64 /opt/db-agent/db-agent; sudo install -d -m 700 /etc/db-agent'
credential="dac_$(printf 'a%.0s' {1..43})"
"${remote[@]}" "set -eu; sudo sh -ec 'umask 077; printf \"api_url: http://127.0.0.1:1\\nagent_credential: $credential\\nauto_update: false\\ndatabase:\\n  host: 127.0.0.1\\n  port: 1\\n\" > /etc/db-agent/config.yaml; printf original-linux-proof-key > /etc/db-agent/encryption.key; sha256sum /etc/db-agent/config.yaml /etc/db-agent/encryption.key > /etc/db-agent/proof.sha256'; sudo /opt/db-agent/db-agent install --service-only; systemctl is-enabled db-agent; systemctl is-active db-agent"
old_boot="$(timeout 10 "${remote[@]}" cat /proc/sys/kernel/random/boot_id)"
timeout 30 "${remote[@]}" sudo systemctl reboot || true
rebooted=false
deadline=$((SECONDS + 240))
for ((attempt=0; attempt<120 && SECONDS<deadline; attempt++)); do
  boot="$(timeout 10 "${remote[@]}" cat /proc/sys/kernel/random/boot_id 2>/dev/null || true)"
  if [[ -n "$boot" && "$boot" != "$old_boot" ]] && timeout 10 "${remote[@]}" systemctl is-active --quiet db-agent 2>/dev/null; then rebooted=true; break; fi
  sleep 2
done
if [[ "$rebooted" != true ]]; then echo 'VM did not return from reboot' >&2; exit 1; fi
"${remote[@]}" 'set -eu; systemctl is-enabled db-agent; systemctl is-active db-agent; sudo /opt/db-agent/db-agent status; sudo /opt/db-agent/db-agent stop; sudo /opt/db-agent/db-agent start; systemctl is-active db-agent; sudo /opt/db-agent/db-agent uninstall; sudo /opt/db-agent/db-agent uninstall; sudo sha256sum --check /etc/db-agent/proof.sha256; sudo stat -c "%a %n" /etc/db-agent /etc/db-agent/config.yaml /etc/db-agent/encryption.key' \
  | tee "$work/reboot-proof.log"
echo 'Disposable Linux amd64 service install/update/reboot/stop/start/uninstall proof passed. Database backup/restore and native arm64 qualification are separate gates.'
