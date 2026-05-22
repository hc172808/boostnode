#!/usr/bin/env bash
# ============================================================
# GYDS Chain — Boost/Relay Node Setup (Ubuntu 22.04 / Debian)
# High-performance P2P relay with kernel tuning.
# Usage: sudo bash setup-boostnode-server.sh
# Repo:  https://github.com/hc172808/boostnode
# ============================================================
set -Eeuo pipefail

APP_USER="gyds"
APP_DIR="/opt/gyds-boostnode"
REPO_URL="https://github.com/hc172808/boostnode.git"
BRANCH="main"

GYDS_DATADIR="${GYDS_DATADIR:-/var/lib/gyds-boostnode}"
GYDS_CHAIN_ID="${GYDS_CHAIN_ID:-13370}"
GYDS_RPC_PORT="${GYDS_RPC_PORT:-8545}"
GYDS_P2P_PORT="${GYDS_P2P_PORT:-30306}"
GYDS_BOOST_PORT="${GYDS_BOOST_PORT:-30307}"
SSH_PORT="22"
GO_VERSION="1.22.4"

GREEN='\033[0;32m'; YELLOW='\033[1;33m'; RED='\033[0;31m'; BLUE='\033[0;34m'; NC='\033[0m'
log()  { echo -e "${GREEN}[BOOST]${NC} $*"; }
warn() { echo -e "${YELLOW}[WARN]${NC}  $*"; }
die()  { echo -e "${RED}[ERROR]${NC}  $*"; exit 1; }

[[ $EUID -ne 0 ]] && die "Run as root: sudo bash $0"
export DEBIAN_FRONTEND=noninteractive

log "Updating system..."
apt-get update -qq && apt-get upgrade -y

log "Installing base packages..."
apt-get install -y --no-install-recommends \
  curl wget git build-essential ca-certificates \
  jq ufw fail2ban net-tools iperf3 tcpdump logrotate \
  gnupg software-properties-common

log "Applying kernel tuning for high-throughput P2P..."
cat > /etc/sysctl.d/99-gyds-boostnode.conf <<-SYSCTL
	net.core.rmem_max = 134217728
	net.core.wmem_max = 134217728
	net.core.netdev_max_backlog = 5000
	net.ipv4.tcp_rmem = 4096 87380 134217728
	net.ipv4.tcp_wmem = 4096 65536 134217728
	net.ipv4.tcp_congestion_control = bbr
	net.core.default_qdisc = fq
	net.ipv4.ip_local_port_range = 1024 65535
	net.ipv4.tcp_tw_reuse = 1
	fs.file-max = 2097152
	SYSCTL
sysctl --system >/dev/null

cat > /etc/security/limits.d/gyds.conf <<-LIMITS
	$APP_USER soft nofile 131072
	$APP_USER hard nofile 131072
	$APP_USER soft nproc  32768
	$APP_USER hard nproc  32768
	LIMITS

log "Installing Go ${GO_VERSION}..."
install_go() {
  ARCH=$(dpkg --print-architecture | sed 's/x86_64/amd64/;s/aarch64/arm64/')
  wget -q "https://go.dev/dl/go${GO_VERSION}.linux-${ARCH}.tar.gz" -O /tmp/go.tar.gz
  rm -rf /usr/local/go
  tar -C /usr/local -xzf /tmp/go.tar.gz
  ln -sf /usr/local/go/bin/go /usr/local/bin/go
  rm -f /tmp/go.tar.gz
  echo 'export PATH=$PATH:/usr/local/go/bin' > /etc/profile.d/go.sh
}
if ! command -v go &>/dev/null; then
  install_go
else
  CURRENT="$(go version | awk '{print $3}' | tr -d 'go')"
  [[ "${CURRENT}" != "${GO_VERSION}" ]] && { warn "Upgrading Go..."; install_go; }
fi
export PATH=$PATH:/usr/local/go/bin
log "Go: $(go version)"

log "Installing Docker..."
install -m 0755 -d /etc/apt/keyrings
curl -fsSL https://download.docker.com/linux/ubuntu/gpg | gpg --dearmor -o /etc/apt/keyrings/docker.gpg
echo "deb [arch=$(dpkg --print-architecture) signed-by=/etc/apt/keyrings/docker.gpg] https://download.docker.com/linux/ubuntu $(lsb_release -cs) stable" \
  > /etc/apt/sources.list.d/docker.list
apt-get update && apt-get install -y docker-ce docker-ce-cli containerd.io docker-compose-plugin
systemctl enable --now docker

id "$APP_USER" &>/dev/null || adduser --disabled-password --gecos "" "$APP_USER"
usermod -aG docker "$APP_USER"

log "Configuring firewall..."
ufw default deny incoming
ufw default allow outgoing
ufw allow "$SSH_PORT"/tcp
ufw allow "$GYDS_P2P_PORT"/tcp
ufw allow "$GYDS_P2P_PORT"/udp
ufw allow "$GYDS_BOOST_PORT"/tcp
ufw allow "$GYDS_BOOST_PORT"/udp
ufw --force enable

log "Configuring Fail2Ban..."
cat > /etc/fail2ban/jail.local <<-EOF
	[DEFAULT]
	bantime = 1h
	findtime = 10m
	maxretry = 5
	[sshd]
	enabled = true
	port = $SSH_PORT
	EOF
systemctl restart fail2ban && systemctl enable fail2ban

log "Cloning repo..."
mkdir -p "$APP_DIR"
if [ ! -d "$APP_DIR/.git" ]; then
  git clone "$REPO_URL" "$APP_DIR"
else
  git -C "$APP_DIR" config --global --add safe.directory "$APP_DIR"
  git -C "$APP_DIR" fetch origin
  git -C "$APP_DIR" reset --hard "origin/$BRANCH"
fi
chown -R "$APP_USER:$APP_USER" "$APP_DIR"

log "Setting up .env..."
[ -f "$APP_DIR/.env.example" ] || die ".env.example not found in repo"
cp "$APP_DIR/.env.example" "$APP_DIR/.env"
chmod 600 "$APP_DIR/.env"
printf '\nGYDS_RPC_PORT=%s\nGYDS_P2P_PORT=%s\nGYDS_DATA_DIR=%s\n' \
  "$GYDS_RPC_PORT" "$GYDS_P2P_PORT" "$GYDS_DATADIR" >> "$APP_DIR/.env"

log "Creating data directories..."
mkdir -p "${GYDS_DATADIR}"/{logs,peers}
chown -R "$APP_USER:$APP_USER" "$GYDS_DATADIR"

log "Building binary..."
cd "$APP_DIR"
make build 2>/dev/null || go build -ldflags="-s -w" -o bin/gyds-boostnode .

log "Building + starting Docker container..."
docker compose down --remove-orphans 2>/dev/null || true
docker compose build --no-cache
docker compose up -d

log "Configuring log rotation..."
cat > /etc/logrotate.d/gyds-boostnode <<-ROTATE
	${GYDS_DATADIR}/logs/*.log {
	    daily
	    rotate 7
	    compress
	    delaycompress
	    missingok
	    notifempty
	    copytruncate
	}
	ROTATE

log "Creating hardened systemd service (native binary)..."
cat > /etc/systemd/system/gyds-boostnode.service <<-SERVICE
	[Unit]
	Description=GYDS Chain Boost Node
	After=network-online.target
	Wants=network-online.target
	[Service]
	Type=simple
	User=$APP_USER
	WorkingDirectory=$APP_DIR
	EnvironmentFile=$APP_DIR/.env
	ExecStart=$APP_DIR/bin/gyds-boostnode start
	Restart=always
	RestartSec=5s
	LimitNOFILE=131072
	LimitNPROC=32768
	StandardOutput=append:${GYDS_DATADIR}/logs/boost.log
	StandardError=append:${GYDS_DATADIR}/logs/boost-error.log
	NoNewPrivileges=true
	PrivateTmp=true
	ProtectSystem=strict
	ProtectHome=true
	MemoryDenyWriteExecute=true
	ReadWritePaths=${GYDS_DATADIR}
	[Install]
	WantedBy=multi-user.target
	SERVICE
systemctl daemon-reload

PUBLIC_IP="$(curl -4 -s --max-time 5 https://api.ipify.org 2>/dev/null || echo 'YOUR_SERVER_IP')"

echo ""
echo -e "${BLUE}╔══════════════════════════════════════╗${NC}"
echo -e "${GREEN}║     GYDS BOOST NODE DEPLOYED         ║${NC}"
echo -e "${BLUE}╚══════════════════════════════════════╝${NC}"
echo ""
echo "  P2P Port:   tcp://${PUBLIC_IP}:$GYDS_P2P_PORT"
echo "  Boost Port: tcp://${PUBLIC_IP}:$GYDS_BOOST_PORT"
echo "  RPC Port:   http://127.0.0.1:$GYDS_RPC_PORT (local only)"
echo ""
echo "  Add to other nodes as bootstrap peer:"
echo "    GYDS_BOOTSTRAP_NODES=tcp://${PUBLIC_IP}:${GYDS_P2P_PORT}"
echo ""
echo "  Logs:   cd $APP_DIR && docker compose logs -f"
echo "  Re-run: sudo ./setup-boostnode-server.sh"
echo ""
