#!/usr/bin/env bash
#
# n2n-go-client 一键部署脚本
#
# 用法：
#   bash <(curl -fsSL https://raw.githubusercontent.com/goyo123321/n2n-go-client/main/install.sh)
#
# 环境变量（可选）：
#   SIGNALING_URL   wss 信令地址（不设置则交互式询问）
#   CONNECT_TOKEN   连接密码
#   ROOM_ID         房间名，默认 default-room
#   CLIENT_ID       客户端 ID，留空自动生成
#   NODE_NAME       显示名，默认 hostname
#   VERSION         指定版本（如 v1.2.0），留空自动用最新版
#   INSTALL_DIR     安装目录，默认 ~/.n2n-go
#   NO_LAUNCH       =1 只安装不启动
#
set -euo pipefail

REPO="goyo123321/n2n-go-client"
BINARY_BASE="n2n-client"
INSTALL_DIR="${INSTALL_DIR:-$HOME/.n2n-go}"

# ============================================================
# ★ 关键改动：默认用最新版；GitHub API 不可用时回退
#
# 更新新版本发布后，把 FALLBACK_VERSION 改成最新的 tag 即可
# （作为 API 限流/断网时的兜底）
# ============================================================
FALLBACK_VERSION="v1.0.27"

GITHUB_API="https://api.github.com/repos/${REPO}"
GITHUB_DL="https://github.com/${REPO}/releases/download"

# ============================================================
# 若 stdin 不是终端（curl | bash 场景），尝试重定向到 /dev/tty
# ============================================================
if [ ! -t 0 ] && [ -r /dev/tty ]; then
    exec < /dev/tty
fi

# ============================================================
# 颜色
# ============================================================
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
                *)
                    err "不支持的 Linux 架构: $uname_m"
                    exit 1
                    ;;
            esac
            ;;
        Darwin)
            case "$uname_m" in
                x86_64) PLATFORM="darwin-amd64" ;;
                arm64)  PLATFORM="darwin-arm64" ;;
                *)
                    err "不支持的 macOS 架构: $uname_m"
                    exit 1
                    ;;
            esac
            ;;
        *)
            err "不支持的系统: $uname_s"
            exit 1
            ;;
    esac

    BIN_FILE="${BINARY_BASE}-${PLATFORM}"

    log "系统: $uname_s / $uname_m"
    log "目标: $PLATFORM"
    [ "$IS_TERMUX" = "1" ] && log "环境: Termux"
    return 0
}

# ============================================================
# ★ 解析版本：优先 VERSION 环境变量，否则 GitHub API，最后回退
# ============================================================
resolve_version() {
    if [ -n "${VERSION:-}" ]; then
        ok "使用指定版本: $VERSION"
        return 0
    fi

    log "查询 GitHub 最新版本..."

    LATEST=""

    if command -v curl >/dev/null 2>&1; then
        LATEST=$(curl -fsSL --connect-timeout 8 --max-time 20 \
            "${GITHUB_API}/releases/latest" 2>/dev/null \
            | sed -n 's/.*"tag_name"[[:space:]]*:[[:space:]]*"\([^"]*\)".*/\1/p' \
            | head -1 || true)
    elif command -v wget >/dev/null 2>&1; then
        LATEST=$(wget -qO- --timeout=20 \
            "${GITHUB_API}/releases/latest" 2>/dev/null \
            | sed -n 's/.*"tag_name"[[:space:]]*:[[:space:]]*"\([^"]*\)".*/\1/p' \
            | head -1 || true)
    else
        err "需要 curl 或 wget"
        exit 1
    fi

    if [ -n "$LATEST" ]; then
        VERSION="$LATEST"
        ok "最新版本: $VERSION"
    else
        VERSION="$FALLBACK_VERSION"
        warn "无法从 GitHub 获取最新版本（可能限流或断网）"
        warn "回退到内置版本: $VERSION"
        warn "如需指定版本：VERSION=vX.Y.Z 重新运行"
    fi
    return 0
}

# ============================================================
# 下载并安装二进制
# ============================================================
install_binary() {
    mkdir -p "$INSTALL_DIR"

    TARGET="${INSTALL_DIR}/${BINARY_BASE}"
    URL="${GITHUB_DL}/${VERSION}/${BIN_FILE}"

    log "下载: $URL"

    TMP="${INSTALL_DIR}/.${BINARY_BASE}.tmp.$$"
    trap 'rm -f "$TMP"' EXIT

    if command -v curl >/dev/null 2>&1; then
        if ! curl -fL --connect-timeout 10 --max-time 300 \
            -o "$TMP" "$URL" 2>&1; then
            err "下载失败: $URL"
            err "请确认版本 $VERSION 是否包含 $PLATFORM 二进制"
            exit 1
        fi
    elif command -v wget >/dev/null 2>&1; then
        if ! wget -O "$TMP" "$URL"; then
            err "下载失败: $URL"
            exit 1
        fi
    fi

    chmod +x "$TMP"

    if [ "$(uname -s)" = "Darwin" ]; then
        xattr -d com.apple.quarantine "$TMP" 2>/dev/null || true
    fi

    mv "$TMP" "$TARGET"
    trap - EXIT

    ok "已安装: $TARGET"
    info "版本: $VERSION"
    info "大小: $(du -h "$TARGET" | cut -f1)"
    return 0
}

# ============================================================
# 交互式配置（可选；环境变量已设置则不询问）
# ============================================================
prompt_config() {
    if [ "${NO_LAUNCH:-0}" = "1" ]; then
        return 0
    fi

    echo ""
    log "配置客户端（直接回车用默认值）"

    if [ -z "${SIGNALING_URL:-}" ]; then
        printf "SIGNALING_URL (wss://xxx.workers.dev): "
        read -r SIGNALING_URL || true
    fi

    if [ -z "${SIGNALING_URL:-}" ]; then
        warn "未填写 SIGNALING_URL，跳过启动"
        NO_LAUNCH=1
        return 0
    fi

    if [ -z "${ROOM_ID:-}" ]; then
        printf "ROOM_ID [default-room]: "
        read -r input || true
        ROOM_ID="${input:-default-room}"
    fi

    if [ -z "${CLIENT_ID:-}" ]; then
        printf "CLIENT_ID [自动生成]: "
        read -r input || true
        CLIENT_ID="$input"
    fi

    if [ -z "${NODE_NAME:-}" ]; then
        hostname_default=$(hostname 2>/dev/null || echo "n2n-node")
        printf "NODE_NAME [$hostname_default]: "
        read -r input || true
        NODE_NAME="${input:-$hostname_default}"
    fi

    if [ -z "${CONNECT_TOKEN:-}" ]; then
        printf "CONNECT_TOKEN [留空]: "
        read -rs input || true
        echo
        CONNECT_TOKEN="$input"
    fi

    return 0
}

# ============================================================
# 生成启动脚本（持久化配置，便于二次启动）
# ============================================================
write_launcher() {
    LAUNCHER="${INSTALL_DIR}/start.sh"

    # 转义单引号，避免配置里有 ' 破坏脚本
    esc() { printf "%s" "$1" | sed "s/'/'\\\\''/g"; }

    cat > "$LAUNCHER" <<EOF
#!/usr/bin/env bash
# 自动生成：n2n-go-client 启动脚本
# 修改下面的默认配置，或运行时覆盖环境变量
set -euo pipefail

export SIGNALING_URL="\${SIGNALING_URL:-'$(esc "${SIGNALING_URL:-}")'}"
export ROOM_ID="\${ROOM_ID:-'$(esc "${ROOM_ID:-default-room}")'}"
export CLIENT_ID="\${CLIENT_ID:-'$(esc "${CLIENT_ID:-}")'}"
export NODE_NAME="\${NODE_NAME:-'$(esc "${NODE_NAME:-}")'}"
export CONNECT_TOKEN="\${CONNECT_TOKEN:-'$(esc "${CONNECT_TOKEN:-}")'}"
export INSTALL_DIR="\${INSTALL_DIR:-'$(esc "$INSTALL_DIR")'}"

exec "${INSTALL_DIR}/${BINARY_BASE}" "\$@"
EOF

    chmod +x "$LAUNCHER"
    ok "启动脚本: $LAUNCHER"
    return 0
}

# ============================================================
# 启动客户端
# ============================================================
launch_client() {
    if [ "${NO_LAUNCH:-0}" = "1" ]; then
        echo ""
        ok "安装完成"
        info "启动命令: ${INSTALL_DIR}/start.sh"
        return 0
    fi

    echo ""
    printf "是否立即启动客户端？[Y/n] "
    read -r answer || true
    case "$answer" in
        n|N|no|No|NO)
            ok "安装完成（未启动）"
            info "启动命令: ${INSTALL_DIR}/start.sh"
            return 0
            ;;
    esac

    if [ -z "${SIGNALING_URL:-}" ]; then
        err "未设置 SIGNALING_URL，无法启动"
        info "示例: SIGNALING_URL=wss://xxx.workers.dev ${INSTALL_DIR}/start.sh"
        return 1
    fi

    case "$(uname -s)" in
        Linux)
            if [ "$IS_TERMUX" = "1" ]; then
                ok "启动中（Termux，无需 root）..."
                exec "${INSTALL_DIR}/start.sh"
            else
                if [ "$(id -u)" != "0" ]; then
                    warn "需要 root 权限创建 TUN 设备，将使用 sudo"
                    exec sudo -E "${INSTALL_DIR}/start.sh"
                else
                    exec "${INSTALL_DIR}/start.sh"
                fi
            fi
            ;;
        Darwin)
            if [ "$(id -u)" != "0" ]; then
                warn "需要 sudo 权限创建 utun 设备"
                exec sudo -E "${INSTALL_DIR}/start.sh"
            else
                exec "${INSTALL_DIR}/start.sh"
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
    resolve_version
    install_binary
    prompt_config
    write_launcher
    launch_client
}

main "$@"
