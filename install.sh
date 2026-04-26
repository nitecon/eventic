#!/usr/bin/env bash
set -euo pipefail

REPO="nitecon/eventic"
INSTALL_DIR="/opt/eventic"
BIN_DIR="${INSTALL_DIR}/bin"
REPOS_DIR="${INSTALL_DIR}/repos"
STATE_DIR="${INSTALL_DIR}/state"
CONFIG_DIR="/etc/eventic"
CONFIG_FILE="${CONFIG_DIR}/config.yaml"
SYMLINK="/usr/local/bin/eventic-client"
SERVICE_NAME="eventic"
SERVICE_FILE="/etc/systemd/system/${SERVICE_NAME}.service"
OLD_SERVICE_NAME="eventic-client"
OLD_SERVICE_FILE="/etc/systemd/system/${OLD_SERVICE_NAME}.service"

# --- Helpers ----------------------------------------------------------------

info()  { printf '\033[1;32m[INFO]\033[0m  %s\n' "$*"; }
warn()  { printf '\033[1;33m[WARN]\033[0m  %s\n' "$*"; }
error() { printf '\033[1;31m[ERROR]\033[0m %s\n' "$*" >&2; exit 1; }

# --- Pre-flight checks ------------------------------------------------------

if [ "$(id -u)" -ne 0 ]; then
  error "This script must be run as root. Try: curl -fsSL <url> | sudo bash"
fi

OS=$(uname -s | tr '[:upper:]' '[:lower:]')
ARCH=$(uname -m)
case "$ARCH" in
  x86_64)        ARCH="amd64" ;;
  aarch64|arm64) ARCH="arm64" ;;
  *)             error "Unsupported architecture: $ARCH" ;;
esac

if [ "$OS" != "linux" ]; then
  warn "This installer is designed for Linux. Proceeding anyway."
fi

BINARY="eventic-client-${OS}-${ARCH}"
DOWNLOAD_URL="https://github.com/${REPO}/releases/latest/download/${BINARY}"

# --- User / group -----------------------------------------------------------

if ! id -u eventic &>/dev/null; then
  useradd -r -s /bin/false eventic
  info "Created system user 'eventic'"
else
  info "User 'eventic' already exists"
fi

# --- Directories ------------------------------------------------------------

mkdir -p "$BIN_DIR" "$REPOS_DIR" "$STATE_DIR" "$CONFIG_DIR"
chown -R eventic:eventic "$INSTALL_DIR"
info "Directory structure ready (${INSTALL_DIR})"

# --- Download binary --------------------------------------------------------

info "Downloading ${BINARY} ..."
curl -fsSL -o "/tmp/${BINARY}" "$DOWNLOAD_URL"
mv "/tmp/${BINARY}" "${BIN_DIR}/eventic-client"
chmod +x "${BIN_DIR}/eventic-client"
chown eventic:eventic "${BIN_DIR}/eventic-client"
ln -sf "${BIN_DIR}/eventic-client" "$SYMLINK"
info "Installed ${BIN_DIR}/eventic-client -> ${SYMLINK}"

# --- Template config --------------------------------------------------------

if [ ! -f "$CONFIG_FILE" ]; then
  cat > "$CONFIG_FILE" <<'EOF'
# Eventic client configuration
# Docs: https://github.com/nitecon/eventic#configuration

relay: "wss://your-server/ws"
token: "your-auth-token"
client_id: "build-server-01"
repos_dir: "/opt/eventic/repos"
subscribe:
  - "yourorg/*"
auto-update: true
# global-hooks:
#   pre: "echo preparing ${EVENTIC_REPO}..."
#   post: "echo done with ${EVENTIC_REPO}"
EOF
  chown root:eventic "$CONFIG_FILE"
  chmod 640 "$CONFIG_FILE"
  info "Template config created at ${CONFIG_FILE}"
else
  info "Config already exists at ${CONFIG_FILE}, skipping"
fi

# --- Systemd service --------------------------------------------------------

if command -v systemctl &>/dev/null; then
  # Migrate from old eventic-client.service if present
  if [ -f "$OLD_SERVICE_FILE" ]; then
    info "Migrating from ${OLD_SERVICE_NAME}.service to ${SERVICE_NAME}.service"
    systemctl stop "$OLD_SERVICE_NAME" 2>/dev/null || true
    systemctl disable "$OLD_SERVICE_NAME" 2>/dev/null || true
    rm -f "$OLD_SERVICE_FILE"
    info "Removed old ${OLD_SERVICE_NAME}.service"
  fi

  cat > "$SERVICE_FILE" <<'EOF'
[Unit]
Description=Eventic Client
After=network-online.target
Wants=network-online.target

[Service]
Type=simple
ExecStart=/usr/local/bin/eventic-client -c /etc/eventic/config.yaml
Restart=always
RestartSec=5
User=eventic
Group=eventic
WorkingDirectory=/opt/eventic

[Install]
WantedBy=multi-user.target
EOF

  systemctl daemon-reload
  systemctl enable "$SERVICE_NAME" >/dev/null 2>&1
  info "Systemd service installed and enabled"

  if [ -f "$CONFIG_FILE" ] && grep -q "your-auth-token" "$CONFIG_FILE"; then
    warn "Config still has placeholder values — service not started"
  else
    systemctl restart "$SERVICE_NAME"
    info "Service restarted"
  fi
else
  warn "systemd not detected — skipping service installation"
fi

# --- Done -------------------------------------------------------------------

echo ""
info "Installation complete!"
echo ""
echo "  Binary:  ${BIN_DIR}/eventic-client"
echo "  Symlink: ${SYMLINK}"
echo "  Config:  ${CONFIG_FILE}"
echo "  Service: ${SERVICE_NAME}"
echo ""
if grep -q "your-auth-token" "$CONFIG_FILE" 2>/dev/null; then
  echo "Next steps:"
  echo "  1. Edit ${CONFIG_FILE} with your relay URL, token, and subscriptions"
  echo "  2. sudo systemctl start ${SERVICE_NAME}"
  echo ""
fi
