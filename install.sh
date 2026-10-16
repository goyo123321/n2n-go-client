#!/usr/bin/env bash
#
# n2n-go-client 一键部署 + 自动升级
#
# 用法：
#   bash <(curl -fsSL https://raw.githubusercontent.com/goyo123321/n2n-go-client/main/install.sh)
#
# 环境变量：
#   SIGNALING_URL     WSS 地址
#   CONNECT_TOKEN     连接密码（可选）
#   ROOM_ID           房间名，默认 default-room
#   CLIENT_ID         客户端 ID，留空自动生成
#   NODE_NAME         显示名，默认 hostname
#   VERSION           指定版本（如 v1.2.0）；留空自动获取最新
#   INSTALL_DIR       安装目录，默认 ~/.n2n-go
#   FORCE_UPDATE      =1 强制重装
#   NO_LAUNCH         =1 只安装不启动
#   AUTO_START        =1 静默自动启动
#   CHECK_ONLY        =1 只检查版本不下载
#

set -euo pipefail

# ★ 修复：set -u 下未定义变量会报错，统一给空默认值
REPO="goyo123321/n2n-go-client"
BIN_NAME="n2n-client"

SIGNALING_URL="${SIGNALING_URL:-}"
CONNECT_TOKEN="${CONNECT_TOKEN:-}"
ROOM_ID="${ROOM_ID:-}"
CLIENT_ID="${CLIENT_ID:-}"
NODE_NAME="${NODE_NAME:-}"
VERSION="${VERSION:-}"
FORCE_UPDATE="${FORCE_UPDATE:-0}"
NO_LAUNCH="${NO_LAUNCH:-0}"
AUTO_START="${AUTO_START:-0}"
CHECK_ONLY="${CHECK_ONLY:-0}"
INSTALL_DIR="${INSTALL_DIR:-$HOME/.n2n-go}"

BIN_PATH="$INSTALL_DIR/$BIN_NAME"
VERSION_FILE="$INSTALL_DIR/.version"
CONFIG_FILE="$INSTALL_DIR/config.env"
LAUNCHER="$INSTALL_DIR/start.sh"

GITHUB_API="https://api.github.com/repos/${REPO}"
GITHUB_DL="https://github.com/${REPO}/releases/download"
FALLBACK_VERSION="v1.2.0"

# 非交互式（curl | bash）时把 stdin 重定向到 tty
if [ ! -t 0 ] && [ -r /dev/tty ]; then
    exec < /dev/tty || true
fi

# 颜色
if [ -t 1 ] && [ "${TERM:-}" != "dumb" ]; then
    RED='\033[0;31m'; GREEN='\033[0;32m'
    YELLOW='\033[1;33m'; BLUE='\033[0;34m'; CYAN='\033[0;36m'; NC='\033[0m'
else
    RED=''; GREEN=''; YELLOW=''; BLUE=''; CYAN=''; NC=''
fi
log()  { printf "${BLUE}[*]${NC} %s\n" "$*"; }
ok()   { printf "${GREEN}[✓]${NC} %s\n" "$*"; }
warn() { printf "${YELLOW}[!]${NC} %s\n" "$*"; }
err()  { printf "${RED}[✗]${NC} %s\n" "$*" >&2; }
info() { printf "${CYAN}  %s${NC}\n" "$*"; }

# ============================================================
# 检测环境
# ============================================================
IS_TERMUX=0
PLATFORM=""
BIN_FILE=""

detect_env() {
    uname_s="$(uname -s)"
    uname_m="$(uname -m)"

    if [ -n "${PREFIX:-}" ] && [[ "$PREFIX" == *"com.termux"* ]]; then
        IS_TERMUX=1
    fi

    case "$uname_s" in
        Linux)
            case "$uname_m" in
                x86_64|amd64)  PLATFORM="linux-amd64" ;;
                aarch64|arm64) PLATFORM="linux-arm64" ;;
                armv7l|armv8l) PLATFORM="linux-armv7" ;;
                *) err "不支持的 Linux 架构: $uname_m"; exit 1 ;;
            esac
            ;;
        Darwin)
            case "$uname_m" in
                x86_64) PLATFORM="darwin-amd64" ;;
                arm64)  PLATFORM="darwin-arm64" ;;
                *) err "不支持的 macOS 架构: $uname_m"; exit 1 ;;
            esac
            ;;
        *) err "不支持的系统: $uname_s"; exit 1 ;;
    esac

    BIN_FILE="${BIN_NAME}-${PLATFORM}"
    log "系统: $uname_s / $uname_m → $PLATFORM"
    [ "$IS_TERMUX" = "1" ] && log "环境: Termux"
}

# ============================================================
# 版本管理
# ============================================================
read_installed_version() {
    if [ -f "$VERSION_FILE" ]; then
        cat "$VERSION_FILE" 2>/dev/null | tr -d '[:space:]'
    else
        echo ""
    fi
}

write_installed_version() {
    mkdir -p "$INSTALL_DIR"
    echo "$1" > "$VERSION_FILE"
}

resolve_latest_version() {
    if [ -n "$VERSION" ]; then
        echo "$VERSION"
        return 0
    fi

    local latest=""
    if command -v curl >/dev/null 2>&1; then
        latest=$(curl -fsSL --connect-timeout 8 --max-time 20 \
            "${GITHUB_API}/releases/latest" 2>/dev/null \
            | sed -n 's/.*"tag_name"[[:space:]]*:[[:space:]]*"\([^"]*\)".*/\1/p' \
            | head -1 || true)
    elif command -v wget >/dev/null 2>&1; then
        latest=$(wget -qO- --timeout=20 "${GITHUB_API}/releases/latest" 2>/dev/null \
            | sed -n 's/.*"tag_name"[[:space:]]*:[[:space:]]*"\([^"]*\)".*/\1/p' \
            | head -1 || true)
    fi

    if [ -n "$latest" ]; then
        echo "$latest"
    else
        echo "$FALLBACK_VERSION"
    fi
}

# ============================================================
# 停止运行中的客户端
# ============================================================
stop_running_client() {
    if ! command -v pgrep >/dev/null 2>&1; then
        return 0
    fi

    local pids
    pids=$(pgrep -f "${INSTALL_DIR}/${BIN_NAME}" 2>/dev/null || true)
    if [ -z "$pids" ]; then
        return 0
    fi

    log "检测到运行中的 n2n-client (PID: $pids)，停止..."
    # shellcheck disable=SC2086
    kill $pids 2>/dev/null || true
    sleep 1

    pids=$(pgrep -f "${INSTALL_DIR}/${BIN_NAME}" 2>/dev/null || true)
    if [ -n "$pids" ]; then
        warn "进程未响应 SIGTERM，使用 SIGKILL"
        # shellcheck disable=SC2086
        kill -9 $pids 2>/dev/null || true
        sleep 0.5
    fi
    ok "旧进程已停止"
}

# ============================================================
# 下载二进制
# ============================================================
install_binary() {
    local version="$1"
    mkdir -p "$INSTALL_DIR"

    local url="${GITHUB_DL}/${version}/${BIN_FILE}"
    log "下载: $url"

    local tmp="${INSTALL_DIR}/.${BIN_NAME}.tmp.$$"
    trap 'rm -f "$tmp"' EXIT

    if command -v curl >/dev/null 2>&1; then
        if ! curl -fL --connect-timeout 10 --max-time 300 \
            -o "$tmp" "$url" 2>&1; then
            err "下载失败: $url"
            err "请确认版本 $version 是否包含 $PLATFORM 二进制"
            exit 1
        fi
    elif command -v wget >/dev/null 2>&1; then
        if ! wget -O "$tmp" "$url"; then
            err "下载失败: $url"
            exit 1
        fi
    fi

    chmod +x "$tmp"

    if [ "$(uname -s)" = "Darwin" ]; then
        xattr -d com.apple.quarantine "$tmp" 2>/dev/null || true
    fi

    mv "$tmp" "$BIN_PATH"
    trap - EXIT

    ok "已安装: $BIN_PATH ($(du -h "$BIN_PATH" | cut -f1))"
}

# ============================================================
# 交互式配置
# ============================================================
prompt_config() {
    if [ "$NO_LAUNCH" = "1" ]; then
        return 0
    fi

    # 已有 config.env 且没传 SIGNALING_URL → 用现成的
    if [ -f "$CONFIG_FILE" ] && [ -z "$SIGNALING_URL" ]; then
        log "使用已有配置: $CONFIG_FILE"
        set -a
        # shellcheck disable=SC1090
        source "$CONFIG_FILE"
        set +a
        return 0
    fi

    echo ""
    log "配置客户端（直接回车用默认值）"

    if [ -z "$SIGNALING_URL" ]; then
        printf "SIGNALING_URL (wss://xxx.workers.dev): "
        read -r SIGNALING_URL || true
    fi

    if [ -z "$SIGNALING_URL" ]; then
        warn "未填写 SIGNALING_URL，跳过启动"
        NO_LAUNCH=1
        return 0
    fi

    if [ -z "$ROOM_ID" ]; then
        printf "ROOM_ID [default-room]: "
        read -r input || true
        ROOM_ID="${input:-default-room}"
    fi

    if [ -z "$CLIENT_ID" ]; then
        printf "CLIENT_ID [自动生成]: "
        read -r input || true
        CLIENT_ID="$input"
    fi

    if [ -z "$NODE_NAME" ]; then
        hostname_default=$(hostname 2>/dev/null || echo "n2n-node")
        printf "NODE_NAME [$hostname_default]: "
        read -r input || true
        NODE_NAME="${input:-$hostname_default}"
    fi

    if [ -z "$CONNECT_TOKEN" ]; then
        printf "CONNECT_TOKEN [留空]: "
        read -rs input || true
        echo
        CONNECT_TOKEN="$input"
    fi

    return 0
}

save_config() {
    mkdir -p "$INSTALL_DIR"
    cat > "$CONFIG_FILE" <<EOF
# n2n-go-client 配置
# 由 install.sh 生成于 $(date -Iseconds 2>/dev/null || date)

SIGNALING_URL="$(printf '%s' "${SIGNALING_URL:-}" | sed 's/"/\\"/g')"
CONNECT_TOKEN="$(printf '%s' "${CONNECT_TOKEN:-}" | sed 's/"/\\"/g')"
ROOM_ID="$(printf '%s' "${ROOM_ID:-default-room}" | sed 's/"/\\"/g')"
CLIENT_ID="$(printf '%s' "${CLIENT_ID:-}" | sed 's/"/\\"/g')"
NODE_NAME="$(printf '%s' "${NODE_NAME:-}" | sed 's/"/\\"/g')"
EOF
    chmod 600 "$CONFIG_FILE"
    ok "配置已保存: $CONFIG_FILE"
}

# ============================================================
# 生成启动脚本
# ============================================================
write_launcher() {
    cat > "$LAUNCHER" <<'EOF'
#!/usr/bin/env bash
# n2n-go-client 启动脚本（由 install.sh 生成）
set -euo pipefail

INSTALL_DIR="${INSTALL_DIR:-$HOME/.n2n-go}"
CONFIG_FILE="$INSTALL_DIR/config.env"
BIN="$INSTALL_DIR/n2n-client"

if [ ! -f "$CONFIG_FILE" ]; then
    echo "配置文件不存在: $CONFIG_FILE"
    exit 1
fi
if [ ! -x "$BIN" ]; then
    echo "二进制不存在: $BIN"
    exit 1
fi

set -a
# shellcheck disable=SC1090
source "$CONFIG_FILE"
set +a

exec "$BIN" "$@"
EOF
    chmod +x "$LAUNCHER"
    ok "启动脚本: $LAUNCHER"
}

# ============================================================
# 启动客户端
# ============================================================
launch_client() {
    if [ "$NO_LAUNCH" = "1" ]; then
        echo ""
        ok "安装完成"
        info "启动: sudo $LAUNCHER"
        return 0
    fi

    if [ "$AUTO_START" != "1" ]; then
        echo ""
        printf "是否立即启动客户端？[Y/n] "
        read -r answer || true
        case "$answer" in
            n|N|no|No|NO)
                ok "安装完成（未启动）"
                info "启动: sudo $LAUNCHER"
                return 0
                ;;
        esac
    fi

    if [ -z "$SIGNALING_URL" ]; then
        err "未设置 SIGNALING_URL，无法启动"
        info "手动: SIGNALING_URL=wss://... $LAUNCHER"
        return 1
    fi

    case "$(uname -s)" in
        Linux)
            if [ "$IS_TERMUX" = "1" ]; then
                ok "启动中（Termux，无需 root）..."
                exec "$LAUNCHER"
            else
                if [ "$(id -u)" != "0" ]; then
                    warn "需要 root 权限创建 TUN，将使用 sudo"
                    exec sudo -E "$LAUNCHER"
                else
                    exec "$LAUNCHER"
                fi
            fi
            ;;
        Darwin)
            if [ "$(id -u)" != "0" ]; then
                warn "需要 sudo 权限创建 utun"
                exec sudo -E "$LAUNCHER"
            else
                exec "$LAUNCHER"
            fi
            ;;
    esac
}

# ============================================================
# 主流程
# ============================================================
main() {
    echo ""
    echo "========================================================"
    echo "  n2n-go-client 一键部署"
    echo "  https://github.com/${REPO}"
    echo "========================================================"
    echo ""

    detect_env

    local installed latest need_update
    installed="$(read_installed_version)"

    log "查询最新版本..."
    latest="$(resolve_latest_version)"

    if [ -n "$installed" ]; then
        log "本地已装: $installed"
    else
        log "本地未装（或旧版无版本记录）"
    fi
    log "远端最新: $latest"

    need_update=1
    if [ -n "$installed" ] && [ "$installed" = "$latest" ] && [ "$FORCE_UPDATE" != "1" ]; then
        need_update=0
    fi

    if [ "$CHECK_ONLY" = "1" ]; then
        echo ""
        if [ "$need_update" = "1" ]; then
            ok "有新版本可更新: ${installed:-无} → $latest"
            info "运行不带 CHECK_ONLY=1 即可升级"
        else
            ok "已是最新版本: $latest"
        fi
        exit 0
    fi

    if [ "$need_update" = "0" ]; then
        ok "已是最新版本: $latest（跳过下载，FORCE_UPDATE=1 可强制重装）"
    else
        if [ -n "$installed" ]; then
            log "升级中: $installed → $latest"
        else
            log "首次安装: $latest"
        fi

        stop_running_client
        install_binary "$latest"
        write_installed_version "$latest"
        ok "版本已更新: $latest"
    fi

    prompt_config
    if [ -n "$SIGNALING_URL" ]; then
        save_config
    fi

    write_launcher

    echo ""
    ok "部署完成！"
    echo ""
    info "二进制:  $BIN_PATH"
    info "版本:    $latest"
    info "配置:    $CONFIG_FILE"
    info "启动:    sudo $LAUNCHER"
    echo ""

    launch_client
}

main "$@"
