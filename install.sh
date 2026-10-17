#!/usr/bin/env bash
#
# n2n-go-client 一键部署 + 自动升级
#
# 用法（推荐）：
#   curl -fsSL https://raw.githubusercontent.com/goyo123321/n2n-go-client/main/install.sh -o ~/install_n2n.sh
#   bash ~/install_n2n.sh
#
# 适用场景：
#   ✅ Linux 服务器 / 树莓派 / 路由器（有 TUN + root）
#   ✅ macOS（有 utun + sudo）
#   ✅ Windows（管理员 + wintun）
#   ✅ Termux + root（有 /dev/net/tun + tsu/su）
#   ❌ Termux 无 root（没有 TUN） → 请用 n2n-termux
#
# ★ 安装路径：默认在当前工作目录下创建 .n2n-go（可用 INSTALL_DIR 覆盖）
#   例如在 /opt 运行 → /opt/.n2n-go/
#   在 ~ 运行 → ~/.n2n-go/
#

set -eu

REPO="goyo123321/n2n-go-client"
BIN_NAME="n2n-go-client"

# ★ 安装路径：默认在当前工作目录下（绝对值，避免后续 cd 影响）
if [ -z "${INSTALL_DIR:-}" ]; then
    INSTALL_DIR="$(pwd)/.n2n-go"
fi
# 确保是绝对路径
case "$INSTALL_DIR" in
    /*) ;;  # 已是绝对路径
    *)  INSTALL_DIR="$(pwd)/$INSTALL_DIR" ;;  # 相对路径转绝对
esac

SIGNALING_URL="${SIGNALING_URL:-}"
CONNECT_TOKEN="${CONNECT_TOKEN:-}"
ROOM_ID="${ROOM_ID:-}"
CLIENT_ID="${CLIENT_ID:-}"
NODE_NAME="${NODE_NAME:-}"
TUN_NAME="${TUN_NAME:-}"
PREFERRED_IP="${PREFERRED_IP:-}"
UDP_PORT="${UDP_PORT:-0}"
STUN_SERVERS="${STUN_SERVERS:-}"
VERSION="${VERSION:-}"
FORCE_UPDATE="${FORCE_UPDATE:-0}"
NO_LAUNCH="${NO_LAUNCH:-0}"
AUTO_START="${AUTO_START:-0}"
CHECK_ONLY="${CHECK_ONLY:-0}"
ALLOW_TERMUX="${ALLOW_TERMUX:-0}"

BIN_PATH="$INSTALL_DIR/$BIN_NAME"
VERSION_FILE="$INSTALL_DIR/.version"
CONFIG_FILE="$INSTALL_DIR/config.env"
LAUNCHER="$INSTALL_DIR/start.sh"

GITHUB_API="https://api.github.com/repos/${REPO}"
GITHUB_DL="https://github.com/${REPO}/releases/download"
FALLBACK_VERSION="v1.0.30"

IS_TERMUX=0
IS_LINUX=0
IS_DARWIN=0
HAS_ROOT=0
HAS_TUN=0
ARCH=""
BIN_FILE=""

if [ -t 1 ] && [ "${TERM:-}" != "dumb" ]; then
    RED='\033[0;31m'; GREEN='\033[0;32m'; YELLOW='\033[1;33m'
    BLUE='\033[0;34m'; CYAN='\033[0;36m'; NC='\033[0m'
else
    RED=''; GREEN=''; YELLOW=''; BLUE=''; CYAN=''; NC=''
fi
log()  { printf "${BLUE}[*]${NC} %s\n" "$*"; }
ok()   { printf "${GREEN}[✓]${NC} %s\n" "$*"; }
warn() { printf "${YELLOW}[!]${NC} %s\n" "$*"; }
err()  { printf "${RED}[✗]${NC} %s\n" "$*" >&2; }
info() { printf "${CYAN}  %s${NC}\n" "$*"; }

read_tty() {
    local var="$1"
    local prompt="${2:-}"
    local silent="${3:-0}"
    [ -n "$prompt" ] && printf "%s" "$prompt"
    local val=""
    if [ -r /dev/tty ]; then
        if [ "$silent" = "1" ]; then read -rs val < /dev/tty || true
        else read -r val < /dev/tty || true; fi
    else
        if [ "$silent" = "1" ]; then read -rs val || true
        else read -r val || true; fi
    fi
    [ "$silent" = "1" ] && [ -n "$prompt" ] && echo
    eval "$var=\"\$val\""
}

detect_env() {
    local uname_s="$(uname -s)"
    local uname_m="$(uname -m)"

    if [ -n "${PREFIX:-}" ] && [[ "$PREFIX" == *"com.termux"* ]]; then
        IS_TERMUX=1
    fi

    case "$uname_s" in
        Linux)
            if [ "$IS_TERMUX" = "1" ]; then
                case "$uname_m" in
                    aarch64|arm64)  ARCH="linux-arm64" ;;
                    armv7l|armv8l)  ARCH="linux-armv7" ;;
                    x86_64|amd64)   ARCH="linux-amd64" ;;
                    *) err "Termux 不支持的架构: $uname_m"; exit 1 ;;
                esac
            else
                IS_LINUX=1
                case "$uname_m" in
                    x86_64|amd64)  ARCH="linux-amd64" ;;
                    aarch64|arm64) ARCH="linux-arm64" ;;
                    armv7l|armv8l) ARCH="linux-armv7" ;;
                    *) err "Linux 不支持的架构: $uname_m"; exit 1 ;;
                esac
            fi
            ;;
        Darwin)
            IS_DARWIN=1
            case "$uname_m" in
                x86_64) ARCH="darwin-amd64" ;;
                arm64)  ARCH="darwin-arm64" ;;
                *) err "macOS 不支持的架构: $uname_m"; exit 1 ;;
            esac
            ;;
        *)
            err "不支持的系统: $uname_s"
            exit 1
            ;;
    esac

    BIN_FILE="${BIN_NAME}-${ARCH}"

    if [ "$IS_TERMUX" = "1" ]; then
        log "环境: Termux（$uname_m → $ARCH）"
    elif [ "$IS_DARWIN" = "1" ]; then
        log "环境: macOS（$uname_m → $ARCH）"
    else
        log "环境: Linux（$uname_m → $ARCH）"
    fi
}

check_tun_capability() {
    if [ "$(id -u)" = "0" ]; then
        HAS_ROOT=1
    elif command -v sudo >/dev/null 2>&1 && sudo -n true 2>/dev/null; then
        HAS_ROOT=1
    elif command -v tsu >/dev/null 2>&1; then
        HAS_ROOT=1
    elif command -v su >/dev/null 2>&1 && su -c "id -u" 2>/dev/null | grep -q "^0$"; then
        HAS_ROOT=1
    fi

    if [ -e "/dev/net/tun" ]; then
        HAS_TUN=1
        log "检测到 /dev/net/tun"
    elif [ -e "/dev/tun" ]; then
        HAS_TUN=1
        log "检测到 /dev/tun"
    fi

    if [ "$HAS_TUN" = "1" ]; then
        ok "TUN 设备可用"
        if [ "$HAS_ROOT" = "1" ]; then
            log "root 权限可用"
        else
            warn "TUN 存在但当前无 root，启动时需要提权"
        fi
        return 0
    fi

    if [ "$IS_TERMUX" = "1" ]; then
        echo ""
        err "Termux 环境但没有 TUN 设备"
        echo ""
        if [ "$ALLOW_TERMUX" = "1" ]; then
            warn "ALLOW_TERMUX=1 已设置，继续安装（启动可能失败）"
            return 0
        fi
        echo "  你的手机可能有两种状态："
        echo ""
        echo "  ┌─ 未 root ─────────────────────────────────"
        echo "  │ 请用 n2n-termux（SOCKS5 模式，无需 root）"
        echo "  │   bash <(curl -fsSL https://raw.githubusercontent.com/goyo123321/n2n-termux/main/install.sh)"
        echo "  └────────────────────────────────────────────"
        echo ""
        echo "  ┌─ 已 root ─────────────────────────────────"
        echo "  │ 1. 确认 /dev/net/tun 存在："
        echo "  │    su -c 'ls -la /dev/net/tun'"
        echo "  │ 2. 如不存在，加载 tun 模块："
        echo "  │    su -c 'insmod /system/lib/modules/tun.ko'"
        echo "  │ 3. 重新运行本脚本"
        echo "  │ 4. 或用 ALLOW_TERMUX=1 强制跳过检查："
        echo "  │    ALLOW_TERMUX=1 bash install_n2n.sh"
        echo "  └────────────────────────────────────────────"
        echo ""
        exit 1
    else
        err "未找到 /dev/net/tun 或 /dev/tun，无法创建 TUN 虚拟网卡"
        info "Linux 上修复："
        info "  sudo modprobe tun"
        info "  ls -la /dev/net/tun"
        exit 1
    fi
}

fix_termux_dns() {
    if [ "$IS_TERMUX" != "1" ]; then return 0; fi
    local resolv="${PREFIX:-/data/data/com.termux/files/usr}/etc/resolv.conf"

    if [ -f "$resolv" ] && grep -q "nameserver ::1" "$resolv" 2>/dev/null; then
        warn "Termux DNS 指向 [::1]，正在修复..."
        echo "nameserver 8.8.8.8" > "$resolv"
        echo "nameserver 1.1.1.1" >> "$resolv"
        ok "DNS 已修复: $resolv"
    elif [ ! -f "$resolv" ] || ! grep -q "nameserver" "$resolv" 2>/dev/null; then
        warn "Termux 缺少 DNS 配置，正在创建..."
        mkdir -p "$(dirname "$resolv")"
        echo "nameserver 8.8.8.8" > "$resolv"
        echo "nameserver 1.1.1.1" >> "$resolv"
        ok "DNS 配置已创建: $resolv"
    fi
}

normalize_signaling_url() {
    local url="$SIGNALING_URL"
    url="$(echo "$url" | sed 's/^[[:space:]]*//;s/[[:space:]]*$//')"
    case "$url" in
        wss://*|ws://*) SIGNALING_URL="$url" ;;
        https://*)      SIGNALING_URL="wss://${url#https://}"; log "自动转换 https:// → wss://" ;;
        http://*)       SIGNALING_URL="ws://${url#http://}";   log "自动转换 http:// → ws://" ;;
        "")             SIGNALING_URL="" ;;
        *)              SIGNALING_URL="wss://${url}";          log "自动补全协议头 wss://" ;;
    esac
    if [ -n "$SIGNALING_URL" ]; then
        case "$SIGNALING_URL" in
            wss://?*|ws://?*) ok "SIGNALING_URL = $SIGNALING_URL" ;;
            *) err "SIGNALING_URL 格式非法: $SIGNALING_URL"; exit 1 ;;
        esac
    fi
}

read_installed_version() {
    [ -f "$VERSION_FILE" ] && cat "$VERSION_FILE" 2>/dev/null | tr -d '[:space:]' || echo ""
}

write_installed_version() {
    mkdir -p "$INSTALL_DIR"
    echo "$1" > "$VERSION_FILE"
}

resolve_latest_version() {
    if [ -n "$VERSION" ]; then echo "$VERSION"; return 0; fi
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
    if [ -n "$latest" ]; then echo "$latest"
    else warn "GitHub API 请求失败，回退到 $FALLBACK_VERSION" >&2; echo "$FALLBACK_VERSION"; fi
}

stop_running_client() {
    if command -v pgrep >/dev/null 2>&1; then
        local pids
        pids=$(pgrep -f "${INSTALL_DIR}/${BIN_NAME}" 2>/dev/null || true)
        if [ -n "$pids" ]; then
            log "停止运行中的客户端 (PID: $pids)"
            # shellcheck disable=SC2086
            kill $pids 2>/dev/null || true
            sleep 1
            pids=$(pgrep -f "${INSTALL_DIR}/${BIN_NAME}" 2>/dev/null || true)
            if [ -n "$pids" ]; then
                # shellcheck disable=SC2086
                kill -9 $pids 2>/dev/null || true
            fi
            ok "旧进程已停止"
        fi
    fi
}

install_binary() {
    local version="$1"
    mkdir -p "$INSTALL_DIR"
    local url="${GITHUB_DL}/${version}/${BIN_FILE}"
    log "下载: $url"

    local tmp="${INSTALL_DIR}/.${BIN_NAME}.tmp.$$"
    trap 'rm -f "$tmp"' EXIT

    if command -v curl >/dev/null 2>&1; then
        if ! curl -fL --connect-timeout 10 --max-time 300 -o "$tmp" "$url" 2>&1; then
            err "下载失败: $url"
            err "请检查: https://github.com/${REPO}/releases"
            exit 1
        fi
    elif command -v wget >/dev/null 2>&1; then
        if ! wget -O "$tmp" "$url"; then
            err "下载失败: $url"
            exit 1
        fi
    else
        err "curl 和 wget 都不可用"
        exit 1
    fi

    if command -v file >/dev/null 2>&1; then
        if ! file "$tmp" | grep -q "ELF\|Mach-O"; then
            err "下载的文件不是可执行文件，可能是 404 页面"
            rm -f "$tmp"
            exit 1
        fi
    fi

    chmod +x "$tmp"
    mv "$tmp" "$BIN_PATH"
    trap - EXIT
    ok "已安装: $BIN_PATH ($(du -h "$BIN_PATH" 2>/dev/null | cut -f1 || echo '?'))"
}

prompt_config() {
    if [ "$NO_LAUNCH" = "1" ]; then return 0; fi

    if [ -f "$CONFIG_FILE" ] && [ -z "$SIGNALING_URL" ]; then
        log "使用已有配置: $CONFIG_FILE"
        set -a
        # shellcheck disable=SC1090
        source "$CONFIG_FILE"
        set +a
        normalize_signaling_url
        return 0
    fi

    echo ""
    log "配置客户端（直接回车用默认值）"

    if [ -z "$SIGNALING_URL" ]; then
        read_tty SIGNALING_URL "SIGNALING_URL (wss://xxx.workers.dev): "
        normalize_signaling_url
    fi
    if [ -z "$SIGNALING_URL" ]; then
        warn "未填写 SIGNALING_URL，跳过启动"
        NO_LAUNCH=1
        return 0
    fi

    if [ -z "$ROOM_ID" ]; then
        read_tty input "ROOM_ID [default-room]: "
        ROOM_ID="${input:-default-room}"
    fi

    if [ -z "$CLIENT_ID" ]; then
        if [ -f "$INSTALL_DIR/client_id" ]; then
            CLIENT_ID=$(tr -d '[:space:]' < "$INSTALL_DIR/client_id" 2>/dev/null || echo "")
        fi
        if [ -z "$CLIENT_ID" ]; then
            local default_cid
            if [ "$IS_TERMUX" = "1" ]; then
                default_cid="termux-$(date +%s | tail -c 6)"
            else
                default_cid="$(hostname 2>/dev/null || echo 'n2n-node')-$(date +%s | tail -c 6)"
            fi
            read_tty input "CLIENT_ID [$default_cid]: "
            CLIENT_ID="${input:-$default_cid}"
        fi
    fi

    if [ -z "$NODE_NAME" ]; then
        local hostname_default
        hostname_default="$(hostname 2>/dev/null || echo 'n2n-node')"
        read_tty input "NODE_NAME [$hostname_default]: "
        NODE_NAME="${input:-$hostname_default}"
    fi

    if [ -z "$CONNECT_TOKEN" ]; then
        read_tty input "CONNECT_TOKEN [留空]: " 1
        CONNECT_TOKEN="$input"
    fi

    if [ -z "$TUN_NAME" ]; then
        if [ "$IS_DARWIN" = "1" ]; then
            TUN_NAME="utun9"
            log "macOS 自动设置 TUN_NAME=utun9"
        else
            TUN_NAME="n2n0"
        fi
    fi
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
TUN_NAME="$(printf '%s' "${TUN_NAME:-n2n0}" | sed 's/"/\\"/g')"
PREFERRED_IP="$(printf '%s' "${PREFERRED_IP:-}" | sed 's/"/\\"/g')"
UDP_PORT="${UDP_PORT:-0}"
STUN_SERVERS="$(printf '%s' "${STUN_SERVERS:-}" | sed 's/"/\\"/g')"
EOF
    chmod 600 "$CONFIG_FILE"
    ok "配置已保存: $CONFIG_FILE"
}

# ============================================================
# ★ start.sh 里的 INSTALL_DIR 用「自身所在目录」自动推导
#   - 用户从任意目录调用 start.sh 都能正确工作
#   - 不需要硬编码绝对路径
#   - 如果用户显式设置 INSTALL_DIR 环境变量，优先用用户值
# ============================================================
write_launcher() {
    cat > "$LAUNCHER" <<EOF
#!/usr/bin/env bash
set -eu

# ★ 自动推导安装目录（start.sh 所在目录），而不是当前工作目录
#   这样从任意路径调用 start.sh 都能找到 config.env 和二进制
if [ -z "\${INSTALL_DIR:-}" ]; then
    INSTALL_DIR="\$(cd "\$(dirname "\$0")" && pwd)"
fi

CONFIG_FILE="\$INSTALL_DIR/config.env"
BIN="\$INSTALL_DIR/${BIN_NAME}"

if [ ! -f "\$CONFIG_FILE" ]; then echo "配置文件不存在: \$CONFIG_FILE"; exit 1; fi
if [ ! -x "\$BIN" ]; then echo "二进制不存在: \$BIN"; exit 1; fi

set -a
# shellcheck disable=SC1090
source "\$CONFIG_FILE"
set +a
exec "\$BIN" "\$@"
EOF
    chmod +x "$LAUNCHER"
    ok "启动脚本: $LAUNCHER"
}

launch_client() {
    if [ "$NO_LAUNCH" = "1" ]; then
        echo ""
        ok "安装完成（未启动）"
        info "启动: $LAUNCHER"
        return 0
    fi

    if [ "$AUTO_START" != "1" ]; then
        local answer=""
        read_tty answer "是否立即启动客户端？[Y/n] "
        case "$answer" in
            n|N|no|No|NO)
                ok "安装完成（未启动）"
                info "启动: $LAUNCHER"
                return 0
                ;;
        esac
    fi

    if [ -z "$SIGNALING_URL" ]; then
        err "未设置 SIGNALING_URL，无法启动"
        return 1
    fi

    if [ "$(id -u)" = "0" ]; then
        ok "启动中（root）..."
        exec "$LAUNCHER"
    fi

    if [ "$IS_TERMUX" = "1" ]; then
        if command -v tsu >/dev/null 2>&1; then
            ok "启动中（tsu 提权）..."
            exec tsu -c "$LAUNCHER"
        elif command -v sudo >/dev/null 2>&1; then
            ok "启动中（sudo 提权）..."
            exec sudo -E "$LAUNCHER"
        else
            err "Termux 下有 TUN 但无提权工具"
            info "安装 tsu：pkg install tsu"
            info "或手动：su -c '$LAUNCHER'"
            exit 1
        fi
    elif [ "$IS_DARWIN" = "1" ] || [ "$IS_LINUX" = "1" ]; then
        if command -v sudo >/dev/null 2>&1; then
            warn "需要 root 权限创建 TUN，使用 sudo"
            exec sudo -E "$LAUNCHER"
        else
            err "需要 root，但 sudo 不可用"
            info "切换到 root 后运行：$LAUNCHER"
            exit 1
        fi
    fi
}

main() {
    echo ""
    echo "========================================================"
    echo "  n2n-go-client 一键部署"
    echo "  https://github.com/${REPO}"
    echo "========================================================"
    echo ""
    log "安装目录: $INSTALL_DIR"
    echo ""

    detect_env
    check_tun_capability
    fix_termux_dns

    local installed latest need_update
    installed="$(read_installed_version)"

    log "查询最新版本..."
    latest="$(resolve_latest_version)"

    [ -n "$installed" ] && log "本地已装: $installed" || log "本地未装"
    log "远端最新: $latest"

    need_update=1
    if [ -n "$installed" ] && [ "$installed" = "$latest" ] && [ "$FORCE_UPDATE" != "1" ] \
        && [ -f "$BIN_PATH" ] && [ -x "$BIN_PATH" ]; then
        need_update=0
    fi

    if [ "$CHECK_ONLY" = "1" ]; then
        if [ "$need_update" = "1" ]; then ok "有新版本可更新: ${installed:-无} → $latest"
        else ok "已是最新版本: $latest"; fi
        exit 0
    fi

    if [ "$need_update" = "0" ]; then
        ok "已是最新版本: $latest（FORCE_UPDATE=1 可强制重装）"
    else
        [ -n "$installed" ] && log "升级: $installed → $latest" || log "首次安装: $latest"
        stop_running_client
        install_binary "$latest"
        write_installed_version "$latest"
        ok "版本已更新: $latest"
    fi

    prompt_config
    [ -n "$SIGNALING_URL" ] && save_config

    if [ -n "${CLIENT_ID:-}" ]; then
        mkdir -p "$INSTALL_DIR"
        echo "$CLIENT_ID" > "$INSTALL_DIR/client_id"
        chmod 600 "$INSTALL_DIR/client_id"
    fi

    write_launcher

    echo ""
    ok "部署完成！"
    echo ""
    info "安装目录: $INSTALL_DIR"
    info "二进制:  $BIN_PATH"
    info "版本:    $latest"
    info "配置:    $CONFIG_FILE"
    info "启动:    $LAUNCHER"
    echo ""

    launch_client
}

main "$@"
