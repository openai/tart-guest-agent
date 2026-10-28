#!/usr/bin/env bash
# Deploy and configure tart-guest-agent to a running macOS Tart VM.
# Enforces a single-active-process architecture with zero duplicate background entries.
set -euo pipefail

VM_NAME="${1:-macos-mini-sandbox}"
SSH_USER="${2:-admin}"
SSH_PASS="${3:-admin}"

SCRIPT_DIR="$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd)"
REPO_DIR="$(cd "${SCRIPT_DIR}/.." && pwd)"
BIN_PATH="${REPO_DIR}/bin/tart-guest-agent-darwin-arm64"

echo "==> Building tart-guest-agent for darwin/arm64..."
mkdir -p "${REPO_DIR}/bin"
(cd "${REPO_DIR}" && go build -o "${BIN_PATH}" ./cmd/main.go)
echo "[OK] Built ${BIN_PATH} ($(wc -c < "${BIN_PATH}" | tr -d ' ') bytes)"

echo "==> Resolving IP for VM '${VM_NAME}'..."
VM_IP="$(tart ip "${VM_NAME}" 2>/dev/null || true)"
if [ -z "${VM_IP}" ]; then
    echo "[ERROR] Could not resolve IP for VM '${VM_NAME}'. Is it running?"
    exit 1
fi
echo "[OK] Target VM IP: ${VM_IP}"

SSH_OPTS=(-o StrictHostKeyChecking=no -o UserKnownHostsFile=/dev/null -o ConnectTimeout=10)

echo "==> Transferring binary and launchd configs..."
sshpass -p "${SSH_PASS}" scp "${SSH_OPTS[@]}" \
    "${BIN_PATH}" \
    "${SCRIPT_DIR}/tart-guest-agent.plist" \
    "${SCRIPT_DIR}/tart-guest-daemon.plist" \
    "${SSH_USER}@${VM_IP}:/tmp/"

echo "==> Installing into guest system..."
sshpass -p "${SSH_PASS}" ssh "${SSH_OPTS[@]}" "${SSH_USER}@${VM_IP}" "bash -s" << 'EOF'
set -euo pipefail

# 1. Install canonical binary to /usr/local/bin
echo admin | sudo -S mkdir -p /usr/local/bin
echo admin | sudo -S cp /tmp/tart-guest-agent-darwin-arm64 /usr/local/bin/tart-guest-agent
echo admin | sudo -S chmod 755 /usr/local/bin/tart-guest-agent
echo admin | sudo -S codesign -s - -f /usr/local/bin/tart-guest-agent

# 2. Stop and purge legacy launchd instances
echo admin | sudo -S launchctl unload /Library/LaunchDaemons/org.cirruslabs.tart-guest-daemon.plist 2>/dev/null || true
launchctl unload /Library/LaunchAgents/org.cirruslabs.tart-guest-agent.plist 2>/dev/null || true
killall tart-guest-agent 2>/dev/null || true

# 3. Configure one-shot boot daemon (resizes disk once on boot, KeepAlive: false)
echo admin | sudo -S mv /tmp/tart-guest-daemon.plist /Library/LaunchDaemons/org.cirruslabs.tart-guest-daemon.plist
echo admin | sudo -S chown root:wheel /Library/LaunchDaemons/org.cirruslabs.tart-guest-daemon.plist
echo admin | sudo -S chmod 0644 /Library/LaunchDaemons/org.cirruslabs.tart-guest-daemon.plist
echo admin | sudo -S launchctl load /Library/LaunchDaemons/org.cirruslabs.tart-guest-daemon.plist

# 4. Configure single persistent user GUI agent (handles clipboard, file-transfer, AF_VSOCK RPC)
echo admin | sudo -S mv /tmp/tart-guest-agent.plist /Library/LaunchAgents/org.cirruslabs.tart-guest-agent.plist
echo admin | sudo -S chown root:wheel /Library/LaunchAgents/org.cirruslabs.tart-guest-agent.plist
echo admin | sudo -S chmod 0644 /Library/LaunchAgents/org.cirruslabs.tart-guest-agent.plist
launchctl load /Library/LaunchAgents/org.cirruslabs.tart-guest-agent.plist

rm -f /tmp/tart-guest-agent-darwin-arm64
EOF

sleep 1

echo "==> Verifying guest agent doctor via tart exec..."
tart exec "${VM_NAME}" /usr/local/bin/tart-guest-agent doctor
echo "[OK] tart-guest-agent successfully installed and verified on '${VM_NAME}'."
