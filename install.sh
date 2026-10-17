#!/usr/bin/env bash
#
# n2n-go-client 一键部署 + 自动升级
#
# 用法：
#   bash <(curl -fsSL https://raw.githubusercontent.com/goyo123321/n2n-go-client/main/install.sh)
#
set -euo pipefail

REPO="goyo123321/n2n-go-client"

# ★ 二进制名统一改为 n2n-go-client（不再用 n2n-client，避免与 Termux 混淆）
BIN_NAME="n2n-go-client"

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
INSTALL_DIR="${INSTALL_DIR:-$HOME/.n2n-go}"

BIN_PATH="$INSTALL_DIR/$BIN_NAME"
VERSION_FILE="$INSTALL_DIR/.version"
CONFIG_FILE="$INSTALL_DIR/config.env"
LAUNCHER="$INSTALL_DIR/start.sh"

GITHUB_API="https://api.github.com/repos/${REPO}"
GITHUB_DL="https://github.com/${REPO}/releases/download"
# ★ 注意：FALLBACK_VERSION 请根据你实际发版情况调整
FALLBACK_VERSION="v1.3.0"

IS_TERMUX=0
IS_LINUX=0
IS_DARWIN=0

# 颜色定义省略...

# 环境与架构检测
detect_env() {
    local uname_s="$(uname -s)"
    local uname_m="$(uname -m)"

    # ★ 修复1：准确检测 Termux 环境
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

    # ★ 二进制文件名统一
    BIN_FILE="${BIN_NAME}-${ARCH}"
}

# ★ 修复2：Termux DNS 解析失败问题
fix_termux_dns() {
    if [ "$IS_TERMUX" != "1" ]; then
        return 0
    fi

    local resolv="$PREFIX/etc/resolv.conf"
    
    # 检查是否指向了不可用的 IPv6 [::1]
    if grep -q "nameserver ::1" "$resolv" 2>/dev/null; then
        warn "Termux DNS 配置异常 (指向 [::1])，正在修复..."
        echo "nameserver 8.8.8.8" > "$resolv"
        echo "nameserver 1.1.1.1" >> "$resolv"
        ok "DNS 已修复: $resolv"
    elif [ ! -f "$resolv" ] || ! grep -q "nameserver" "$resolv" 2>/dev/null; then
        warn "Termux 缺少 DNS 配置，正在创建..."
        mkdir -p "$PREFIX/etc"
        echo "nameserver 8.8.8.8" > "$resolv"
        echo "nameserver 1.1.1.1" >> "$resolv"
        ok "DNS 配置已创建: $resolv"
    fi
}

# ★ 修复3：自动补全 URL 协议前缀
normalize_signaling_url() {
    local url="$SIGNALING_URL"
    # 去除首尾空格
    url="$(echo "$url" | sed 's/^[[:space:]]*//;s/[[:space:]]*$//')"

    if [[ "$url" != wss://* && "$url" != ws://* ]]; then
        if [[ "$url" == https://* ]]; then
            SIGNALING_URL="wss://${url#https://}"
            log "自动转换: https:// → wss://"
        elif [[ "$url" == http://* ]]; then
            SIGNALING_URL="ws://${url#http://}"
            log "自动转换: http:// → ws://"
        else
            SIGNALING_URL="wss://${url}"
            log "自动补全协议头: wss://"
        fi
    fi
    
    # 校验最终结果
    if [[ "$SIGNALING_URL" != wss://* && "$SIGNALING_URL" != ws://* ]]; then
        err "SIGNALING_URL 格式非法: $SIGNALING_URL"
        exit 1
    fi
}

# 交互式配置
prompt_config() {
    # ...（读取已有 config.env 逻辑省略）...

    if [ -z "$SIGNALING_URL" ]; then
        printf "SIGNALING_URL (wss://xxx.workers.dev): "
        read -r SIGNALING_URL || true
        normalize_signaling_url
    fi

    # ★ 修复4：Termux 下 hostname 可能是 localhost，自动加随机后缀
    if [ -z "$CLIENT_ID" ]; then
        local default_cid
        if [ "$IS_TERMUX" = "1" ]; then
            default_cid="termux-$(date +%s | tail -c 6)"
        else
            default_cid="$(hostname 2>/dev/null || echo 'n2n-node')-$(date +%s | tail -c 6)"
        fi
        printf "CLIENT_ID [%s]: " "$default_cid"
        read -r input || true
        CLIENT_ID="${input:-$default_cid}"
    fi
    # ...
}

# 启动脚本
launch_client() {
    # ...
    if [ "$IS_TERMUX" = "1" ]; then
        ok "启动中（Termux 环境）..."
        exec "$LAUNCHER"
    elif [ "$IS_LINUX" = "1" ]; then
        if [ "$(id -u)" != "0" ]; then
            warn "需要 root 权限创建 TUN"
            exec sudo -E "$LAUNCHER"
        else
            exec "$LAUNCHER"
        fi
    elif [ "$IS_DARWIN" = "1" ]; then
        if [ "$(id -u)" != "0" ]; then
            warn "需要 sudo 权限创建 utun"
            exec sudo -E "$LAUNCHER"
        else
            exec "$LAUNCHER"
        fi
    fi
}

main() {
    detect_env
    fix_termux_dns  # ★ 在下载前先修复 DNS
    # ...（版本检查、下载逻辑省略）...
}

main "$@"
