#!/usr/bin/env bash
set -e

# ============================================
# n2n-go-client 一键部署脚本
# 支持 Linux / macOS / Termux(proot)
# ============================================

RED='\033[0;31m'
GREEN='\033[0;32m'
YELLOW='\033[1;33m'
BLUE='\033[0;34m'
NC='\033[0m'

log()  { echo -e "${BLUE}[*]${NC} $1"; }
ok()   { echo -e "${GREEN}[✓]${NC} $1"; }
warn() { echo -e "${YELLOW}[!]${NC} $1"; }
err()  { echo -e "${RED}[✗]${NC} $1" >&2; }

# ---------- 配置区 ----------
INSTALL_DIR="${INSTALL_DIR:-$HOME/.n2n-go}"
CONFIG_FILE="$INSTALL_DIR/config.env"
BIN_DIR="$INSTALL_DIR/bin"
REPO="goyo123321/n2n-go-client"
VERSION="${VERSION:-latest}"

SIGNALING_URL="${SIGNALING_URL:-}"
CONNECT_TOKEN="${CONNECT_TOKEN:-}"
ROOM_ID="${ROOM_ID:-}"
CLIENT_ID="${CLIENT_ID:-}"
NODE_NAME="${NODE_NAME:-}"
SHARE_DIR="${SHARE_DIR:-}"
SHARE_PORT="${SHARE_PORT:-}"
TUN_NAME="${TUN_NAME:-}"
UDP_PORT="${UDP_PORT:-}"
STUN_SERVERS="${STUN_SERVERS:-}"

# ---------- 平台检测 ----------
detect_platform() {
    OS="$(uname -s)"
    ARCH="$(uname -m)"

    case "$OS" in
        Linux)  GOOS="linux" ;;
        Darwin) GOOS="darwin" ;;
        *)      err "不支持的操作系统: $OS"; exit 1 ;;
    esac

    case "$ARCH" in
        x86_64|amd64)  GOARCH="amd64" ;;
        arm64|aarch64) GOARCH="arm64" ;;
        armv7l)        err "armv7 不支持，仅支持 arm64"; exit 1 ;;
        *)             err "不支持的架构: $ARCH"; exit 1 ;;
    esac

    log "检测到平台: $GOOS/$GOARCH"
}

# ---------- 依赖检查 ----------
check_deps() {
    for cmd in curl uname; do
        if ! command -v "$cmd" >/dev/null 2>&1; then
            err "缺少依赖: $cmd"
            exit 1
        fi
    done

    if [ -n "$TERMUX_VERSION" ] || [ -d "/data/data/com.termux" ]; then
        IS_TERMUX=1
        warn "检测到 Termux 环境，TUN 网卡可能无法创建（需要 root）"
    else
        IS_TERMUX=0
    fi
}

# ---------- 交互式配置（WSS 必填）----------
ask_config() {
    echo ""
    echo "==================== 配置 ===================="
    echo "（直接回车 = 使用括号里的默认值）"
    echo ""

    # WSS 必填，循环询问
    while [ -z "$SIGNALING_URL" ]; do
        echo -e "${YELLOW}Worker WSS 地址（必填）${NC}"
        echo "  示例: wss://edge-signal.abc.workers.dev"
        read -rp "  WSS 地址: " SIGNALING_URL
        SIGNALING_URL="$(echo "$SIGNALING_URL" | tr -d '[:space:]')"

        if [ -z "$SIGNALING_URL" ]; then
            err "WSS 地址不能为空，请重新输入"
            echo ""
            continue
        fi

        if [[ ! "$SIGNALING_URL" =~ ^wss?:// ]]; then
            err "格式错误：必须以 wss:// 或 ws:// 开头"
            SIGNALING_URL=""
            echo ""
            continue
        fi
    done
    ok "WSS: $SIGNALING_URL"
    echo ""

    if [ -z "$CONNECT_TOKEN" ]; then
        read -rp "连接密码 CONNECT_TOKEN (无则回车): " CONNECT_TOKEN
    fi

    # ★ 修复：ROOM_ID 只允许 [A-Za-z0-9_-]，防止 URL 路径歧义
    while [ -z "$ROOM_ID" ]; do
        read -rp "房间名 ROOM_ID (default-room): " input
        input="${input:-default-room}"
        if [[ ! "$input" =~ ^[A-Za-z0-9_-]+$ ]]; then
            err "ROOM_ID 只允许字母/数字/下划线/短横线，请重新输入"
            continue
        fi
        ROOM_ID="$input"
    done

    if [ -z "$CLIENT_ID" ]; then
        # ★ 修复：默认 CLIENT_ID 用 hostname + 持久化提示（实际持久化由客户端程序完成）
        default_id="$(hostname)"
        echo -e "${YELLOW}提示：强烈建议手动指定 CLIENT_ID，否则重启后虚拟 IP 会漂移${NC}"
        read -rp "客户端 ID CLIENT_ID ($default_id): " input
        CLIENT_ID="${input:-$default_id}"
    fi

    if [ -z "$NODE_NAME" ]; then
        read -rp "显示名 NODE_NAME ($(hostname)): " input
        NODE_NAME="${input:-$(hostname)}"
    fi

    if [ -z "$SHARE_DIR" ]; then
        read -rp "共享目录 SHARE_DIR ($HOME/shared): " input
        SHARE_DIR="${input:-$HOME/shared}"
    fi

    if [ -z "$SHARE_PORT" ]; then
        read -rp "共享盘端口 SHARE_PORT (9090): " input
        SHARE_PORT="${input:-9090}"
    fi

    if [ -z "$TUN_NAME" ]; then
        if [ "$GOOS" = "darwin" ]; then
            default_tun="utun9"
        else
            default_tun="n2n0"
        fi
        read -rp "虚拟网卡名 TUN_NAME ($default_tun): " input
        TUN_NAME="${input:-$default_tun}"
    fi

    if [ -z "$UDP_PORT" ]; then
        read -rp "P2P UDP 端口 UDP_PORT (50001): " input
        UDP_PORT="${input:-50001}"
    fi

    echo "==============================================="
    echo ""
}

# ---------- 保存配置 ----------
save_config() {
    mkdir -p "$INSTALL_DIR" "$BIN_DIR" "$SHARE_DIR"

    cat > "$CONFIG_FILE" <<EOF
# n2n-go-client 配置
# 由 install.sh 生成于 $(date)

SIGNALING_URL="$SIGNALING_URL"
CONNECT_TOKEN="$CONNECT_TOKEN"
ROOM_ID="$ROOM_ID"
CLIENT_ID="$CLIENT_ID"
NODE_NAME="$NODE_NAME"
SHARE_DIR="$SHARE_DIR"
SHARE_PORT="$SHARE_PORT"
TUN_NAME="$TUN_NAME"
UDP_PORT="$UDP_PORT"
STUN_SERVERS="$STUN_SERVERS"
EOF

    chmod 600 "$CONFIG_FILE"
    ok "配置已保存到: $CONFIG_FILE"
}

# ---------- 下载客户端 ----------
download_binary() {
    local output="$BIN_DIR/n2n-client"

    if [ -f "$output" ]; then
        local local_size
        local_size=$(stat -c%s "$output" 2>/dev/null || stat -f%z "$output" 2>/dev/null || echo 0)
        if [ "$local_size" -gt 1000000 ]; then
            ok "客户端已存在: $output"
            return 0
        fi
    fi

    log "获取最新版本信息..."
    if [ "$VERSION" = "latest" ]; then
        local release_json
        release_json=$(curl -sSL "https://api.github.com/repos/$REPO/releases/latest") || {
            err "无法访问 GitHub API"
            exit 1
        }
        VERSION=$(echo "$release_json" | grep '"tag_name"' | head -1 | sed 's/.*: "\(.*\)".*/\1/')
        if [ -z "$VERSION" ]; then
            err "无法获取最新版本号"
            exit 1
        fi
    fi

    log "版本: $VERSION"

    local file_name="n2n-client-${GOOS}-${GOARCH}"
    local download_url="https://github.com/$REPO/releases/download/${VERSION}/${file_name}"

    log "下载: $download_url"
    if ! curl -L --progress-bar -o "$output" "$download_url"; then
        err "下载失败"
        exit 1
    fi

    chmod +x "$output"
    ok "下载完成: $output"
}

# ---------- 生成启动脚本 ----------
create_launcher() {
    local launcher="$INSTALL_DIR/start.sh"

    cat > "$launcher" <<'EOF'
#!/usr/bin/env bash
set -e

INSTALL_DIR="${INSTALL_DIR:-$HOME/.n2n-go}"
CONFIG_FILE="$INSTALL_DIR/config.env"
BIN="$INSTALL_DIR/bin/n2n-client"

if [ ! -f "$CONFIG_FILE" ]; then
    echo "配置文件不存在: $CONFIG_FILE"
    exit 1
fi

if [ ! -x "$BIN" ]; then
    echo "客户端不存在: $BIN"
    exit 1
fi

set -a
# shellcheck disable=SC1090
source "$CONFIG_FILE"
set +a

if [ -n "$TERMUX_VERSION" ] || [ -d "/data/data/com.termux" ]; then
    exec "$BIN"
fi

if [ "$(id -u)" != "0" ]; then
    exec sudo -E "$BIN"
else
    exec "$BIN"
fi
EOF

    chmod +x "$launcher"
    ok "启动脚本: $launcher"
}

# ---------- 主流程 ----------
main() {
    echo ""
    echo "========================================="
    echo "   n2n-go-client 一键部署脚本"
    echo "========================================="
    echo ""

    detect_platform
    check_deps

    if [ -f "$CONFIG_FILE" ] && [ -z "$SIGNALING_URL" ]; then
        warn "已存在配置文件: $CONFIG_FILE"
        read -rp "重新配置？(y/N): " input
        if [[ ! "$input" =~ ^[Yy]$ ]]; then
            log "使用已有配置"
            # shellcheck disable=SC1090
            source "$CONFIG_FILE"
        else
            ask_config
            save_config
        fi
    else
        ask_config
        save_config
    fi

    download_binary
    create_launcher

    echo ""
    echo "========================================="
    ok "部署完成！"
    echo "========================================="
    echo ""
    echo "  客户端:  $BIN_DIR/n2n-client"
    echo "  配置:    $CONFIG_FILE"
    echo "  共享目录: $SHARE_DIR"
    echo ""
    echo "  启动命令:"
    echo -e "    ${GREEN}$INSTALL_DIR/start.sh${NC}"
    echo ""

    read -rp "现在启动？(Y/n): " input
    if [[ "$input" =~ ^[Yy]$ ]]; then
        exec "$INSTALL_DIR/start.sh"
    fi
}

main "$@"
