#!/usr/bin/env bash
set -euo pipefail

# Gaitwaie Installer
# Usage: curl -fsSL https://github.com/HittyGubby/gaitwaie/releases/latest/download/install.sh | sudo bash

REPO="HittyGubby/gaitwaie"
BINARY="gateway"
INSTALL_DIR="/usr/local/bin"
CONFIG_DIR="/etc/gaitwaie"
DATA_DIR="/var/lib/gaitwaie"
SYSTEMD_DIR="/etc/systemd/system"

# Detect OS and architecture
OS=$(uname -s | tr '[:upper:]' '[:lower:]')
ARCH=$(uname -m)

case "$ARCH" in
  x86_64)  GOARCH="amd64" ;;
  aarch64) GOARCH="arm64" ;;
  armv7l)  GOARCH="arm"   ;;
  *)
    echo "❌ Unsupported architecture: $ARCH"
    exit 1
    ;;
esac

case "$OS" in
  linux)   GOOS="linux"   ;;
  darwin)  GOOS="darwin"  ;;
  *)
    echo "❌ Unsupported OS: $OS"
    exit 1
    ;;
esac

# Check root
if [ "$(id -u)" -ne 0 ]; then
  echo "❌ This script must be run as root (use sudo)"
  exit 1
fi

echo "📦 Installing Gaitwaie for ${GOOS}/${GOARCH}..."

# Determine latest version from GitHub API
echo "🔍 Fetching latest release..."
LATEST_URL=$(curl -sSfL "https://api.github.com/repos/${REPO}/releases/latest" \
  | grep "browser_download_url" \
  | grep "${GOOS}_${GOARCH}" \
  | head -n1 \
  | cut -d '"' -f 4)

if [ -z "$LATEST_URL" ]; then
  echo "❌ Could not find a release for ${GOOS}_${GOARCH}"
  exit 1
fi

# Download binary
echo "⬇️  Downloading ${LATEST_URL}..."
TMP_DIR=$(mktemp -d)
trap "rm -rf ${TMP_DIR}" EXIT

curl -sSfL -o "${TMP_DIR}/${BINARY}" "${LATEST_URL}"
chmod +x "${TMP_DIR}/${BINARY}"

# Install binary
echo "📋 Installing binary to ${INSTALL_DIR}..."
mv "${TMP_DIR}/${BINARY}" "${INSTALL_DIR}/${BINARY}"

# Create directories
mkdir -p "${CONFIG_DIR}" "${DATA_DIR}"

# Create default config if not exists
if [ ! -f "${CONFIG_DIR}/config.yaml" ]; then
  echo "📝 Creating default config at ${CONFIG_DIR}/config.yaml..."
  cat > "${CONFIG_DIR}/config.yaml" << 'EOF'
database_path: "/var/lib/gaitwaie/gateway.db"
listen_addr: ":8080"
tolerance: 3
max_concurrent_tasks: 5

# Request parameters to strip before forwarding upstream.
# Defaults to max_tokens-family fields if omitted. Set to [] to disable.
strip_params:
  - max_tokens
  - max_completion_tokens
  - max_output_tokens
  - max_gen_tokens
  - max_new_tokens

providers:
  # ds:
  #   base_url: "https://api.deepseek.com/v1"
  #   keys:
  #     - "sk-ds-main-xxxx"
  #     - "sk-ds-backup-yyyy"

receivers:
  # alice: "sk-alice-token-xxxx"
  # bob: "sk-bob-token-yyyy"
EOF
  echo "⚠️  Please edit ${CONFIG_DIR}/config.yaml to add your providers and receivers."
fi

# Create systemd service if on Linux
if [ "$OS" = "linux" ]; then
  echo "⚙️  Creating systemd service..."
  USER_NAME="${SUDO_USER:-root}"
  GROUP_NAME=$(id -gn "${USER_NAME}" 2>/dev/null || echo "root")

  cat > "${SYSTEMD_DIR}/gaitwaie.service" << EOF
[Unit]
Description=Gaitwaie AI Router Gateway
After=network-online.target
Wants=network-online.target

[Service]
Type=simple
ExecStart=${INSTALL_DIR}/gateway start --config ${CONFIG_DIR}/config.yaml
Restart=on-failure
RestartSec=5
User=${USER_NAME}
Group=${GROUP_NAME}
NoNewPrivileges=true
ProtectHome=true
ProtectSystem=full
ReadWritePaths=${CONFIG_DIR} ${DATA_DIR}

[Install]
WantedBy=multi-user.target
EOF

  systemctl daemon-reload
  echo "✅ Systemd service created. Enable and start with:"
  echo "   sudo systemctl enable --now gaitwaie"
fi

echo ""
echo "✅ Gaitwaie installed successfully!"
echo ""
echo "   Binary:  ${INSTALL_DIR}/${BINARY}"
echo "   Config:  ${CONFIG_DIR}/config.yaml"
echo "   Data:    ${DATA_DIR}/"
echo ""
echo "   Quick start:"
echo "   1. Edit ${CONFIG_DIR}/config.yaml"
echo "   2. sudo systemctl enable --now gaitwaie"
echo "   3. Check status: sudo systemctl status gaitwaie"
