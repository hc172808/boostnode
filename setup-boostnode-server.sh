#!/usr/bin/env bash
# ============================================================
# GYDS Chain — Boost Node Setup
# Supports: Ubuntu 20.04/22.04/24.04, Debian 11/12,
#           CentOS/RHEL/Rocky/AlmaLinux 8/9,
#           Amazon Linux 2/2023, Fedora 38+
# Usage:    sudo bash setup-boostnode-server.sh
# Repo:     https://github.com/hc172808/boostnode
# ============================================================
set -Eeuo pipefail

# ── Configuration ─────────────────────────────────────────────────────────────
APP_USER="gyds"
APP_DIR="/opt/gyds-boostnode"
REPO_URL="https://github.com/hc172808/boostnode.git"
BRANCH="main"
BINARY="gyds-boostnode"

GYDS_DATADIR="${GYDS_DATADIR:-/var/lib/gyds-boostnode}"
GYDS_CHAIN_ID="${GYDS_CHAIN_ID:-13370}"
GYDS_RPC_PORT="${GYDS_RPC_PORT:-8545}"
GYDS_P2P_PORT="${GYDS_P2P_PORT:-30306}"
GYDS_BOOST_PORT="${GYDS_BOOST_PORT:-30307}"
SSH_PORT="${SSH_PORT:-22}"
GO_VERSION="1.22.4"

# ── Colours ───────────────────────────────────────────────────────────────────
GREEN='\033[0;32m'; YELLOW='\033[1;33m'; RED='\033[0;31m'; BLUE='\033[0;34m'; NC='\033[0m'
log()  { echo -e "${GREEN}[BOOST]${NC} $*"; }
warn() { echo -e "${YELLOW}[WARN]${NC}  $*"; }
die()  { echo -e "${RED}[ERROR]${NC} $*" >&2; exit 1; }
step() { echo -e "\n${BLUE}──────────────────────────────────────────${NC}"; log "$*"; }

# ── Root check ────────────────────────────────────────────────────────────────
[[ $EUID -ne 0 ]] && die "Run as root: sudo bash $0"

# ── OS Detection ──────────────────────────────────────────────────────────────
detect_os() {
    if [ -f /etc/os-release ]; then
        . /etc/os-release
        OS_ID="${ID:-unknown}"
        OS_ID_LIKE="${ID_LIKE:-}"
        OS_VERSION_ID="${VERSION_ID:-0}"
        OS_PRETTY="${PRETTY_NAME:-Unknown OS}"
    else
        die "Cannot detect OS — /etc/os-release not found."
    fi

    # Normalise family
    case "$OS_ID" in
        ubuntu|debian|linuxmint|pop)
            OS_FAMILY="debian" ;;
        centos|rhel|rocky|almalinux|ol)
            OS_FAMILY="rhel" ;;
        fedora)
            OS_FAMILY="fedora" ;;
        amzn)
            OS_FAMILY="amazon" ;;
        *)
            if echo "$OS_ID_LIKE" | grep -qE "debian|ubuntu"; then
                OS_FAMILY="debian"
            elif echo "$OS_ID_LIKE" | grep -qE "rhel|centos|fedora"; then
                OS_FAMILY="rhel"
            else
                die "Unsupported OS: $OS_PRETTY. Supported: Ubuntu, Debian, CentOS/RHEL/Rocky/AlmaLinux, Amazon Linux, Fedora."
            fi ;;
    esac

    log "Detected OS: $OS_PRETTY (family: $OS_FAMILY)"
}

# ── Architecture Detection ────────────────────────────────────────────────────
detect_arch() {
    RAW_ARCH="$(uname -m)"
    case "$RAW_ARCH" in
        x86_64)  ARCH="amd64" ;;
        aarch64|arm64) ARCH="arm64" ;;
        armv7l)  ARCH="armv6l" ;;
        *)       die "Unsupported architecture: $RAW_ARCH" ;;
    esac
    log "Architecture: $RAW_ARCH → $ARCH"
}

# ── Package Manager Abstraction ───────────────────────────────────────────────
pkg_update() {
    case "$OS_FAMILY" in
        debian)  export DEBIAN_FRONTEND=noninteractive
                 apt-get update -qq && apt-get upgrade -y -q ;;
        rhel)    dnf update -y -q 2>/dev/null || yum update -y -q ;;
        fedora)  dnf update -y -q ;;
        amazon)  yum update -y -q ;;
    esac
}

pkg_install() {
    case "$OS_FAMILY" in
        debian)  export DEBIAN_FRONTEND=noninteractive
                 apt-get install -y -q --no-install-recommends "$@" ;;
        rhel)    dnf install -y -q "$@" 2>/dev/null || yum install -y -q "$@" ;;
        fedora)  dnf install -y -q "$@" ;;
        amazon)  yum install -y -q "$@" ;;
    esac
}

# ── Base Packages ─────────────────────────────────────────────────────────────
install_base_packages() {
    step "Installing base packages..."
    case "$OS_FAMILY" in
        debian)
            pkg_install curl wget git build-essential ca-certificates \
                jq ufw fail2ban net-tools iperf3 logrotate \
                gnupg software-properties-common ;;
        rhel|fedora)
            pkg_install curl wget git gcc make ca-certificates \
                jq firewalld fail2ban net-tools iperf3 logrotate gnupg2
            # epel for some packages on RHEL/CentOS
            if [[ "$OS_FAMILY" == "rhel" ]]; then
                dnf install -y epel-release 2>/dev/null || yum install -y epel-release 2>/dev/null || true
            fi ;;
        amazon)
            yum install -y curl wget git gcc make ca-certificates \
                jq iptables fail2ban net-tools logrotate gnupg2 ;;
    esac
}

# ── Kernel Tuning ─────────────────────────────────────────────────────────────
apply_kernel_tuning() {
    step "Applying kernel tuning for high-throughput P2P..."

    # Check BBR support
    BBR_SETTINGS=""
    if modprobe tcp_bbr 2>/dev/null && \
       grep -q "bbr" /proc/sys/net/ipv4/tcp_allowed_congestion_control 2>/dev/null; then
        BBR_SETTINGS="net.ipv4.tcp_congestion_control = bbr
net.core.default_qdisc = fq"
        log "BBR congestion control: enabled"
    else
        warn "BBR not available on this kernel — skipping BBR tuning"
    fi

    cat > /etc/sysctl.d/99-gyds-boostnode.conf <<EOF
net.core.rmem_max = 134217728
net.core.wmem_max = 134217728
net.core.netdev_max_backlog = 5000
net.ipv4.tcp_rmem = 4096 87380 134217728
net.ipv4.tcp_wmem = 4096 65536 134217728
net.ipv4.ip_local_port_range = 1024 65535
net.ipv4.tcp_tw_reuse = 1
fs.file-max = 2097152
${BBR_SETTINGS}
EOF
    sysctl --system >/dev/null 2>&1
    log "Kernel parameters applied."
}

# ── File Limits ───────────────────────────────────────────────────────────────
apply_limits() {
    cat > /etc/security/limits.d/gyds.conf <<EOF
${APP_USER} soft nofile 131072
${APP_USER} hard nofile 131072
${APP_USER} soft nproc  32768
${APP_USER} hard nproc  32768
EOF
    log "File descriptor limits configured."
}

# ── Go Installation ───────────────────────────────────────────────────────────
install_go() {
    step "Installing Go ${GO_VERSION}..."
    local TARBALL="/tmp/go${GO_VERSION}.linux-${ARCH}.tar.gz"
    wget -q "https://go.dev/dl/go${GO_VERSION}.linux-${ARCH}.tar.gz" -O "$TARBALL" \
        || die "Failed to download Go ${GO_VERSION}"
    rm -rf /usr/local/go
    tar -C /usr/local -xzf "$TARBALL"
    ln -sf /usr/local/go/bin/go /usr/local/bin/go
    ln -sf /usr/local/go/bin/gofmt /usr/local/bin/gofmt
    rm -f "$TARBALL"
    cat > /etc/profile.d/go.sh <<'GOPATH'
export PATH=$PATH:/usr/local/go/bin
export GOPATH=$HOME/go
GOPATH
    log "Go installed: $(go version)"
}

ensure_go() {
    if ! command -v go &>/dev/null; then
        install_go
    else
        CURRENT="$(go version | awk '{print $3}' | tr -d 'go')"
        if [[ "$CURRENT" != "$GO_VERSION" ]]; then
            warn "Go $CURRENT found, upgrading to $GO_VERSION..."
            install_go
        else
            log "Go $CURRENT already installed."
        fi
    fi
    export PATH=$PATH:/usr/local/go/bin
}

# ── Docker Installation ───────────────────────────────────────────────────────
install_docker() {
    step "Installing Docker..."

    if command -v docker &>/dev/null && docker info &>/dev/null 2>&1; then
        log "Docker already running — skipping install."
        return
    fi

    case "$OS_FAMILY" in
        debian)
            install -m 0755 -d /etc/apt/keyrings

            # Determine correct Docker repo (Ubuntu vs Debian)
            if [[ "$OS_ID" == "ubuntu" ]]; then
                DOCKER_REPO_DISTRO="ubuntu"
            else
                DOCKER_REPO_DISTRO="debian"
            fi

            CODENAME=""
            if command -v lsb_release &>/dev/null; then
                CODENAME="$(lsb_release -cs)"
            elif [ -f /etc/os-release ]; then
                CODENAME="$(. /etc/os-release && echo "${VERSION_CODENAME:-}")"
            fi
            [[ -z "$CODENAME" ]] && die "Could not detect OS codename for Docker repo."

            curl -fsSL "https://download.docker.com/linux/${DOCKER_REPO_DISTRO}/gpg" \
                | gpg --dearmor -o /etc/apt/keyrings/docker.gpg
            chmod a+r /etc/apt/keyrings/docker.gpg
            echo "deb [arch=$(dpkg --print-architecture) signed-by=/etc/apt/keyrings/docker.gpg] \
https://download.docker.com/linux/${DOCKER_REPO_DISTRO} ${CODENAME} stable" \
                > /etc/apt/sources.list.d/docker.list
            apt-get update -qq
            pkg_install docker-ce docker-ce-cli containerd.io docker-compose-plugin ;;

        rhel|fedora)
            if [[ "$OS_FAMILY" == "rhel" ]]; then
                dnf config-manager --add-repo \
                    https://download.docker.com/linux/centos/docker-ce.repo 2>/dev/null \
                || yum-config-manager --add-repo \
                    https://download.docker.com/linux/centos/docker-ce.repo
            else
                dnf config-manager --add-repo \
                    https://download.docker.com/linux/fedora/docker-ce.repo
            fi
            pkg_install docker-ce docker-ce-cli containerd.io docker-compose-plugin ;;

        amazon)
            # Amazon Linux 2023
            if [[ "$OS_VERSION_ID" == "2023" ]]; then
                dnf install -y docker
            else
                # Amazon Linux 2
                amazon-linux-extras install -y docker 2>/dev/null || yum install -y docker
            fi
            # docker compose plugin via binary for Amazon Linux
            COMPOSE_VERSION="2.27.0"
            mkdir -p /usr/local/lib/docker/cli-plugins
            curl -SL "https://github.com/docker/compose/releases/download/v${COMPOSE_VERSION}/docker-compose-linux-$(uname -m)" \
                -o /usr/local/lib/docker/cli-plugins/docker-compose
            chmod +x /usr/local/lib/docker/cli-plugins/docker-compose ;;
    esac

    systemctl enable --now docker
    log "Docker installed: $(docker --version)"
}

# ── Firewall Configuration ────────────────────────────────────────────────────
configure_firewall() {
    step "Configuring firewall..."

    if command -v ufw &>/dev/null && [[ "$OS_FAMILY" == "debian" ]]; then
        ufw default deny incoming
        ufw default allow outgoing
        ufw allow "$SSH_PORT"/tcp
        ufw allow "$GYDS_P2P_PORT"/tcp
        ufw allow "$GYDS_P2P_PORT"/udp
        ufw allow "$GYDS_BOOST_PORT"/tcp
        ufw allow "$GYDS_BOOST_PORT"/udp
        ufw --force enable
        log "ufw configured."

    elif command -v firewall-cmd &>/dev/null; then
        systemctl enable --now firewalld
        firewall-cmd --permanent --add-port="${SSH_PORT}/tcp"
        firewall-cmd --permanent --add-port="${GYDS_P2P_PORT}/tcp"
        firewall-cmd --permanent --add-port="${GYDS_P2P_PORT}/udp"
        firewall-cmd --permanent --add-port="${GYDS_BOOST_PORT}/tcp"
        firewall-cmd --permanent --add-port="${GYDS_BOOST_PORT}/udp"
        firewall-cmd --reload
        log "firewalld configured."

    elif command -v iptables &>/dev/null; then
        warn "Neither ufw nor firewalld found — using iptables directly."
        iptables -A INPUT -p tcp --dport "$SSH_PORT" -j ACCEPT
        iptables -A INPUT -p tcp --dport "$GYDS_P2P_PORT" -j ACCEPT
        iptables -A INPUT -p udp --dport "$GYDS_P2P_PORT" -j ACCEPT
        iptables -A INPUT -p tcp --dport "$GYDS_BOOST_PORT" -j ACCEPT
        iptables -A INPUT -p udp --dport "$GYDS_BOOST_PORT" -j ACCEPT
        # Persist iptables rules
        if command -v iptables-save &>/dev/null; then
            iptables-save > /etc/iptables.rules 2>/dev/null || true
        fi
        log "iptables rules applied."
    else
        warn "No firewall tool found — skipping firewall configuration."
    fi
}

# ── Fail2Ban ──────────────────────────────────────────────────────────────────
configure_fail2ban() {
    step "Configuring Fail2Ban..."

    if ! command -v fail2ban-server &>/dev/null; then
        warn "fail2ban not installed — skipping."
        return
    fi

    cat > /etc/fail2ban/jail.local <<EOF
[DEFAULT]
bantime  = 1h
findtime = 10m
maxretry = 5

[sshd]
enabled = true
port    = $SSH_PORT
EOF
    systemctl restart fail2ban
    systemctl enable fail2ban
    log "Fail2Ban configured."
}

# ── User Setup ────────────────────────────────────────────────────────────────
setup_user() {
    step "Setting up service user '$APP_USER'..."
    if ! id "$APP_USER" &>/dev/null; then
        if command -v adduser &>/dev/null && [[ "$OS_FAMILY" == "debian" ]]; then
            adduser --disabled-password --gecos "" "$APP_USER"
        else
            useradd -r -s /sbin/nologin -d "$APP_DIR" -m "$APP_USER"
        fi
    fi
    usermod -aG docker "$APP_USER" 2>/dev/null || true
    log "User '$APP_USER' ready."
}

# ── Repository ───────────────────────────────────────────────────────────────
setup_repo() {
    step "Setting up repository in $APP_DIR..."
    mkdir -p "$APP_DIR"

    # Disable all git credential prompts — public repo needs no auth
    export GIT_TERMINAL_PROMPT=0
    export GIT_ASKPASS=echo

    # Pre-flight: verify the repo is reachable before trying to clone
    if ! git ls-remote "$REPO_URL" HEAD &>/dev/null; then
        die "Cannot reach repo: $REPO_URL
       Make sure the repository is set to PUBLIC on GitHub.
       Visit: https://github.com/hc172808/boostnode -> Settings -> Change visibility -> Public"
    fi

    if [ ! -d "$APP_DIR/.git" ]; then
        git clone --branch "$BRANCH" --depth 1 "$REPO_URL" "$APP_DIR"
    else
        git config --global safe.directory "$APP_DIR"
        git -C "$APP_DIR" fetch origin
        git -C "$APP_DIR" reset --hard "origin/$BRANCH"
    fi
    chown -R "$APP_USER:$APP_USER" "$APP_DIR"
    log "Repository ready."
}

# ── Environment File ──────────────────────────────────────────────────────────
setup_env() {
    step "Setting up .env..."
    local EXAMPLE="$APP_DIR/.env.example"
    local ENV_FILE="$APP_DIR/.env"

    [ -f "$EXAMPLE" ] || die ".env.example not found in repo"

    if [ ! -f "$ENV_FILE" ]; then
        cp "$EXAMPLE" "$ENV_FILE"
    else
        warn ".env already exists — merging new values only."
    fi

    chmod 600 "$ENV_FILE"

    # Append/overwrite key values
    {
        printf '\nGYDS_CHAIN_ID=%s\n'   "$GYDS_CHAIN_ID"
        printf 'GYDS_RPC_PORT=%s\n'     "$GYDS_RPC_PORT"
        printf 'GYDS_P2P_PORT=%s\n'     "$GYDS_P2P_PORT"
        printf 'GYDS_BOOST_PORT=%s\n'   "$GYDS_BOOST_PORT"
        printf 'GYDS_DATA_DIR=%s\n'     "$GYDS_DATADIR"
        printf 'GYDS_LOG_LEVEL=info\n'
        printf 'GYDS_LOG_FORMAT=json\n'
    } >> "$ENV_FILE"

    log ".env configured."
}

# ── Data Directories ──────────────────────────────────────────────────────────
setup_data_dirs() {
    mkdir -p "${GYDS_DATADIR}"/{logs,peers,blocks}
    chown -R "$APP_USER:$APP_USER" "$GYDS_DATADIR"
    log "Data directories created at $GYDS_DATADIR"
}

# ── Build Binary ──────────────────────────────────────────────────────────────
build_binary() {
    step "Building $BINARY..."
    cd "$APP_DIR"

    # Ensure go is on PATH for this shell
    export PATH=$PATH:/usr/local/go/bin

    mkdir -p bin
    if go build -buildvcs=false -ldflags="-s -w -X main.version=1.0.0" -o "bin/$BINARY" .; then
        log "Binary built: $APP_DIR/bin/$BINARY"
    else
        die "Build failed. Check Go installation and source code."
    fi
    chown "$APP_USER:$APP_USER" "bin/$BINARY"
}

# ── Docker Container ──────────────────────────────────────────────────────────
start_docker() {
    step "Building and starting Docker container..."
    cd "$APP_DIR"

    # Compose v2 (plugin) vs v1 (standalone)
    if docker compose version &>/dev/null 2>&1; then
        COMPOSE="docker compose"
    elif command -v docker-compose &>/dev/null; then
        COMPOSE="docker-compose"
    else
        warn "docker compose not found — skipping container start."
        return
    fi

    $COMPOSE down --remove-orphans 2>/dev/null || true
    $COMPOSE build --no-cache
    $COMPOSE up -d
    log "Docker container started."
}

# ── Log Rotation ──────────────────────────────────────────────────────────────
setup_logrotate() {
    cat > /etc/logrotate.d/gyds-boostnode <<EOF
${GYDS_DATADIR}/logs/*.log {
    daily
    rotate 7
    compress
    delaycompress
    missingok
    notifempty
    copytruncate
}
EOF
    log "Log rotation configured."
}

# ── Systemd Service ───────────────────────────────────────────────────────────
setup_systemd() {
    step "Creating systemd service..."

    if ! command -v systemctl &>/dev/null; then
        warn "systemd not available — skipping service installation."
        warn "Start manually: sudo -u $APP_USER $APP_DIR/bin/$BINARY start"
        return
    fi

    cat > /etc/systemd/system/gyds-boostnode.service <<EOF
[Unit]
Description=GYDS Chain Boost Node
After=network-online.target
Wants=network-online.target

[Service]
Type=simple
User=${APP_USER}
WorkingDirectory=${APP_DIR}
EnvironmentFile=${APP_DIR}/.env
ExecStart=${APP_DIR}/bin/${BINARY} start
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
ReadWritePaths=${GYDS_DATADIR}

[Install]
WantedBy=multi-user.target
EOF

    systemctl daemon-reload
    log "Systemd service 'gyds-boostnode' installed."
    log "To start natively (without Docker): systemctl enable --now gyds-boostnode"
}

# ── Summary ───────────────────────────────────────────────────────────────────
print_summary() {
    PUBLIC_IP="$(curl -4 -s --max-time 8 https://api.ipify.org 2>/dev/null \
               || curl -4 -s --max-time 8 https://ifconfig.me 2>/dev/null \
               || echo 'YOUR_SERVER_IP')"

    echo ""
    echo -e "${BLUE}╔══════════════════════════════════════════════╗${NC}"
    echo -e "${GREEN}║       GYDS BOOST NODE DEPLOYED               ║${NC}"
    echo -e "${BLUE}╚══════════════════════════════════════════════╝${NC}"
    echo ""
    echo "  P2P Port:    tcp://${PUBLIC_IP}:${GYDS_P2P_PORT}"
    echo "  Boost Port:  tcp://${PUBLIC_IP}:${GYDS_BOOST_PORT}"
    echo "  RPC Port:    http://127.0.0.1:${GYDS_RPC_PORT}  (local only)"
    echo ""
    echo "  Bootstrap peer string:"
    echo "    GYDS_BOOTSTRAP_NODES=tcp://${PUBLIC_IP}:${GYDS_P2P_PORT}"
    echo ""
    echo "  ── Docker mode (default) ──────────────────────────────"
    echo "    Logs:    cd ${APP_DIR} && docker compose logs -f"
    echo "    Restart: docker compose restart"
    echo "    Stop:    docker compose down"
    echo ""
    echo "  ── Native systemd mode ────────────────────────────────"
    echo "    Enable:  systemctl enable --now gyds-boostnode"
    echo "    Logs:    journalctl -fu gyds-boostnode"
    echo "    Restart: systemctl restart gyds-boostnode"
    echo ""
    echo "  Re-run setup: sudo bash ${APP_DIR}/setup-boostnode-server.sh"
    echo ""
}

# ── Main ──────────────────────────────────────────────────────────────────────
main() {
    echo -e "${BLUE}"
    echo "  ╔══════════════════════════════════════════════╗"
    echo "  ║    GYDS Chain — Boost Node Installer         ║"
    echo "  ╚══════════════════════════════════════════════╝"
    echo -e "${NC}"

    detect_os
    detect_arch
    pkg_update
    install_base_packages
    apply_kernel_tuning
    apply_limits
    ensure_go
    install_docker
    configure_firewall
    configure_fail2ban
    setup_user
    setup_repo
    setup_env
    setup_data_dirs
    build_binary
    start_docker
    setup_logrotate
    setup_systemd
    print_summary
}

main "$@"
