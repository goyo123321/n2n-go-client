# n2n-go-client

跨平台 P2P VPN 客户端，配合 [edge-signal](https://github.com/goyo123321/edge-signal) 使用。

基于 TUN 虚拟网卡，通过 Cloudflare Workers 信令服务器建立 **LAN 直连 / P2P / TURN / WebSocket 中继** 四级降级连接，实现异地组网。

**与 [n2n-termux](https://github.com/goyo123321/n2n-termux)、[n2n-android](https://github.com/goyo123321/n2n-android) 完全互通**——PC 通过 TUN 直接访问其他节点的服务，反之亦然。**支持 TCP / UDP / ICMP（含 ping）**。

---

## 🌐 四端协同全景

```
              ┌───────────────────────────────────────────┐
              │      edge-signal (Cloudflare Worker)      │
              │  ├─ 信令路由                              │
              │  ├─ NAT 打洞协调                          │
              │  ├─ TURN 凭证下发                         │
              │  └─ WS 中继（最后兜底）                   │
              └──────────────┬────────────────────────────┘
                             │
         ┌───────────────────┼───────────────────┐
         │                   │                   │
         ▼                   ▼                   ▼
   n2n-go-client       n2n-termux          n2n-android
   ┌──────────┐       ┌──────────┐        ┌──────────┐
   │ TUN 模式  │       │SOCKS5 +  │        │VpnService│
   │ 系统级路由│       │包转发器  │        │ + SOCKS5 │
   │ 需 root  │       │ 无 root  │        │ 无 root  │
   └──────────┘       └──────────┘        └──────────┘
     Linux/macOS         Android              Android
     Windows             Termux                APK
     路由器

       LAN / P2P / TURN / WS  --  完全互通
```

### 三种客户端定位对比

| 维度 | **n2n-go-client**（本仓库） | n2n-termux | n2n-android |
|:---|:---|:---|:---|
| **运行环境** | Linux / macOS / Windows / 路由器 | Android Termux | Android APK |
| **数据面** | TUN 虚拟网卡 | SOCKS5 + 包转发器 | VpnService |
| **权限要求** | root / 管理员 | 无 root | 无 root |
| **应用透明** | ✅ 系统级 | ⚠️ 需 App 显式配 SOCKS5 | ✅ 系统级 |
| **主动访问** | ✅ TCP/UDP/ICMP | ⚠️ 仅 TCP | ✅ TCP/UDP/ICMP |
| **被动接受** | ✅ TCP/UDP/ICMP | ✅ TCP/UDP/ICMP | ✅ TCP/UDP/ICMP |
| **使用方式** | `curl http://10.64.0.X` | `curl --socks5 127.0.0.1:1080 ...` | 同 TUN |
| **适用场景** | 服务器 / 桌面 / 路由器 | 无 root 设备 / 临时调试 | 日常使用（手机） |

**PC 端优势**：**唯一的完全 root/admin 权限的 TUN 数据面**——支持所有协议，无 SOCKS5 代理层开销，是**性能最强**的客户端。

> **关于 Termux + root**：本客户端的判断维度是 **TUN 权限**，与运行环境无关。
> - ✅ Linux / macOS / Windows / 路由器（有 TUN，必有 root）
> - ✅ **Termux + root**（有 `/dev/net/tun` + `tsu`/`su`）—— 完全可以跑，获得完整 TCP/UDP/ICMP 支持
> - ❌ Termux 无 root（没有 TUN）—— 请用 [n2n-termux](https://github.com/goyo123321/n2n-termux)

---

## ✨ 特性

- **跨平台** — Linux / macOS / Windows / 路由器全支持
- **TUN 虚拟网卡** — 内核原生 TCP/IP 栈，性能高
- **完整协议支持** — **TCP / UDP / ICMP（含 ping）**
- **四级降级** — `LAN → P2P → TURN → WS`，连接永远可用
- **LAN 直连** — 同 WiFi 场景自动发现 LAN 地址，绕过 NAT 直接内网通信
- **P2P 直连** — STUN 探测 + NAT 打洞，延迟低至 5ms
- **多目标打洞** — LAN + STUN 反射 + 观测 raddr 同时扫描，命中率大幅提升
- **CGNAT hairpin** — 同 STUN 出口 IP 时尝试端口扫描 + probe 回发
- **单向 P2P 自动降级** — 只收到 PING 而收不到 PONG 时 60 秒后自动降级到 TURN/WS
- **端口漂移保留 P2P 状态** — 仅 IP 变化时清空连接，端口漂移时保留 P2P 通道
- **UDP 保活** — 每 5 秒刷新 STUN 映射，防止 CGNAT 端口漂移
- **P2P 通道保活** — 每 15 秒往对端发 N2NS PING 多目标探针
- **TURN 中继** — 支持 Cloudflare TURN + 自定义 TURN
- **中继回退** — TURN 失败自动降级到 WebSocket 中继
- **N2NW / N2NA 二进制传输** — WS 中继数据面用二进制帧，比 base64 省 ~27% 带宽
- **协议能力协商** — 上报 `features=["n2na","n2nw"]`，服务端自动协商帧格式，**混合房间零丢包**
- **断线重连** — WebSocket 自动重连 + 90 秒读超时检测下行僵死
- **ID 持久化** — 未指定 `CLIENT_ID` 时自动生成并保存，重启复用
- **虚拟网段可配** — 服务端通过 `VIRTUAL_NETWORK` 下发网段，客户端自动适配
- **无依赖** — Windows 版内嵌 wintun.dll
- **三端互通** — 与 n2n-termux、n2n-android 无缝互通

---

## 📂 安装路径（默认当前目录）

**从 v1.0.30 起，安装目录默认是「当前工作目录下的 `.n2n-go`」**，不再是 `~/.n2n-go`。

| 用户在哪个目录运行 `install.sh` | 实际安装到 |
|:---|:---|
| `~` | `~/.n2n-go/` |
| `/opt` | `/opt/.n2n-go/` |
| `~/my-project` | `~/my-project/.n2n-go/` |
| 任意目录 `P` | `P/.n2n-go/` |

**可以用 `INSTALL_DIR` 环境变量覆盖**：

```bash
INSTALL_DIR=/opt/n2n bash install.sh            # 直接装到 /opt/n2n
INSTALL_DIR="$(pwd)" bash install.sh            # 不建子目录，直接装当前目录
```

**安装完成后的目录**：

```
<当前工作目录>/.n2n-go/
├── n2n-go-client       # 二进制
├── config.env          # 配置
├── start.sh            # 启动脚本
├── .version            # 当前版本号
└── client_id           # 持久化的 Client ID
```

**`start.sh` 会自动推导安装目录**——你可以从任意目录调用它：

```bash
# 以下所有方式都能正确工作
cd ~/.n2n-go && ./start.sh
bash ~/.n2n-go/start.sh
sudo /opt/.n2n-go/start.sh
cd /tmp && sudo ~/my-project/.n2n-go/start.sh
```

**手动下载**（不走 `install.sh`）的二进制位置由你决定：

```bash
curl -L -o ./n2n-go-client https://github.com/goyo123321/n2n-go-client/releases/latest/download/n2n-go-client-linux-amd64
chmod +x ./n2n-go-client
sudo ./n2n-go-client
```

---

## 📦 二进制命名说明

从 **v1.0.30** 起，Release 二进制名带 `n2n-go-client-` 前缀，与 `n2n-termux` 区分：

| 项目 | 二进制名 |
|:---|:---|
| **n2n-go-client**（本仓库） | `n2n-go-client-linux-amd64` / `n2n-go-client-linux-arm64` / `n2n-go-client-linux-armv7` / `n2n-go-client-darwin-amd64` / `n2n-go-client-darwin-arm64` / `n2n-go-client-windows-amd64.exe` |
| n2n-termux | `n2n-termux-linux-arm64` / `n2n-termux-linux-armv7` / `n2n-termux-linux-amd64` |

**好处**：下载后一眼看出是哪个，两个项目放同一目录也不冲突，`install.sh` 的 pgrep 也不会误杀。

**历史版本**（v1.0.2x 及之前）的二进制名是 `n2n-client-linux-*`，与 n2n-termux 同名——如果还在用旧版，下载后手动改个名：

```bash
mv n2n-client-linux-amd64 n2n-go-client
chmod +x n2n-go-client
```

---

## 🏗️ 架构

```
┌──────────────────────────────────────────┐
│              客户端进程                   │
│                                           │
│   ┌─────────┐    ┌──────────┐            │
│   │   TUN   │←──→│ 数据泵    │            │
│   │ 虚拟网卡 │    │          │            │
│   └─────────┘    └────┬─────┘            │
│        ↑              │                   │
│        │              ↓                   │
│   ┌─────────┐    ┌──────────┐            │
│   │ 内核栈   │    │ UDP/WS   │            │
│   │ TCP/IP  │    │ 传输层    │            │
│   └─────────┘    └──────────┘            │
└──────────────────────────────────────────┘
```

**与 Termux / Android 的差异**：

- **PC**：TUN 内核栈 → 应用无感知，**TCP/UDP/ICMP 全支持**
- **Termux（无 root）**：SOCKS5 代理 → 应用**必须显式配代理**，主动访问仅 TCP
- **Termux（有 root）**：TUN 内核栈 → 与本客户端行为完全一致
- **Android APK**：VpnService → 应用无感知，**TCP/UDP/ICMP 全支持**

---

## 📡 四级降级链路

```
优先级 1: LAN 直连        ← 同 WiFi 场景，零 NAT 穿越
   ↓ 不同 LAN
优先级 2: P2P 直连        ← 打洞成功，含 CGNAT hairpin
   ↓ 打洞失败
优先级 3: TURN 中继       ← 专业 UDP 中继，不消耗 Worker
   ↓ TURN 不可用/失败
优先级 4: WS 中继         ← 最后兜底，走 Worker，消耗配额
```

**每对 peer 独立决策**：A↔B 可能走 LAN，A↔C 走 P2P，A↔D 走 TURN，互不影响。

---

## 🔥 保活机制

### 为什么需要保活

CGNAT（运营商级 NAT）的 UDP 映射**不是永久的**。当 socket 空闲一段时间后，CGNAT 会**回收映射**。之后从同一 socket 出去的包会被分给**新端口**——这就是"上报的 `pubSocket` 端口和实际打洞端口差很大"的原因。

打洞成功后，P2P 通道也依赖这个映射。如果映射被回收：
- P2P 通道断裂
- 之后发往对端的数据落入黑洞

### 两层保活

| 层 | 间隔 | 目标 | 作用 |
|:---|:---|:---|:---|
| **UDP 保活** | 5 秒 | 3 个 STUN 服务器 | 保 socket 的 CGNAT 映射 |
| **P2P 通道保活** | 15 秒 | 每个 P2P 对端 | 保 A→B 会话（多目标 N2NS PING）|

### 流量代价

- UDP 保活：每设备每 5 秒 3 个 20 字节包 → 一天 ~50 MB
- P2P 保活：每 P2P 对端每 15 秒 1 个 21 字节包 → 一天 ~120 KB × 对端数

**电量代价**：几乎可忽略。

### 数据流优先

**真实数据帧天然刷新 CGNAT 映射**（最强保活）。保活机制只在**空闲时**起兜底作用。

---

## 🚀 快速开始

> ⚠️ **三个前置注意**：
>
> 1. **`SIGNALING_URL` 必须带 `wss://` 前缀**（或 `ws://` 用于本地调试）。漏写会报 `malformed ws or wss URL`。
> 2. **需要 root / sudo**（`/dev/net/tun` 或 macOS `utun` 都需要 root 权限）。
> 3. **需要 TUN 设备存在**：Linux `modprobe tun`；Termux + root 需要 `/dev/net/tun`；无 TUN 会主动拦截并引导用 n2n-termux。

### 方式 A：一键安装脚本（推荐）

```bash
# 推荐方式：下载到任意位置，再用 bash 执行
curl -fsSL https://raw.githubusercontent.com/goyo123321/n2n-go-client/main/install.sh -o ~/install_n2n.sh
bash ~/install_n2n.sh
```

> **不推荐** `bash <(curl ...)` 方式：`进程替换` 在某些环境下失败会静默退出（尤其 Termux / 容器）。下载后 `bash` 执行更稳定。

**安装位置**：`<当前工作目录>/.n2n-go/`

**日常操作**：

```bash
# 前台启动（会看到日志）
sudo ~/.n2n-go/start.sh              # 假设在 ~ 下安装
sudo /opt/.n2n-go/start.sh           # 假设在 /opt 下安装
sudo ~/proj/.n2n-go/start.sh         # 假设在 ~/proj 下安装

# 从任意目录调用 start.sh 都能工作（用自身所在目录推导）
cd /tmp && sudo ~/.n2n-go/start.sh
```

**macOS 额外**：`TUN_NAME` 会在交互配置时自动设为 `utun9`（`n2n0` 在 macOS 上无效）。

### 方式 B：手动下载

从 [Releases](https://github.com/goyo123321/n2n-go-client/releases) 下载对应平台：

| 平台 | 文件 |
|:---|:---|
| Linux x64 | `n2n-go-client-linux-amd64` |
| Linux ARM64 | `n2n-go-client-linux-arm64` |
| Linux ARMv7 | `n2n-go-client-linux-armv7` |
| macOS Intel | `n2n-go-client-darwin-amd64` |
| macOS Apple Silicon | `n2n-go-client-darwin-arm64` |
| Windows x64 | `n2n-go-client-windows-amd64.exe` |

#### Linux

```bash
chmod +x n2n-go-client-linux-amd64

sudo SIGNALING_URL="wss://<你的-worker-域名>" \
     ROOM_ID="myroom" \
     CLIENT_ID="pc-a" \
     NODE_NAME="A的电脑" \
     ./n2n-go-client-linux-amd64
```

#### macOS

```bash
chmod +x n2n-go-client-darwin-arm64
xattr -d com.apple.quarantine ./n2n-go-client-darwin-arm64

sudo SIGNALING_URL="wss://<你的-worker-域名>" \
     ROOM_ID="myroom" \
     CLIENT_ID="mac-a" \
     NODE_NAME="A的Mac" \
     TUN_NAME="utun9" \
     ./n2n-go-client-darwin-arm64
```

#### Windows（管理员 PowerShell）

```powershell
$env:SIGNALING_URL="wss://<你的-worker-域名>"
$env:ROOM_ID="myroom"
$env:CLIENT_ID="win-a"
$env:NODE_NAME="A的PC"

.\n2n-go-client-windows-amd64.exe
```

### 方式 C：Termux + root（tsu/su）

如果你在 Termux 环境，且已经有 root 权限并加载了 TUN 模块：

```bash
# 1. 确认 TUN
su -c "ls -la /dev/net/tun"
# 如不存在：su -c "insmod /system/lib/modules/tun.ko" 或 modprobe tun

# 2. 下载并运行
curl -fsSL https://raw.githubusercontent.com/goyo123321/n2n-go-client/main/install.sh -o ~/install_n2n.sh
bash ~/install_n2n.sh

# 3. 脚本会自动用 tsu 提权启动（也可以手动）
tsu -c ~/.n2n-go/start.sh
# 或
sudo ~/.n2n-go/start.sh
```

**启动后应该看到**：

```
[TUN] n2n0 已启动，IP: 10.64.0.3/24，网段: 10.64.0.0/24
```

**看到 `[TUN] n2nX 已启动` 就是 TUN 版成功了**，这是和 n2n-termux 的 `[SOCKS5] 监听` 最直接的区别。

---

## 🔗 与 Termux / Android 互通

### PC 访问 Termux / Android 上的服务

**Termux / Android 端起服务**：

```bash
# Termux 端
python3 -m http.server 8080 --bind 0.0.0.0 &
sshd  # 默认监听 8022
```

**PC 端直接访问**（走 TUN）：

```bash
# ICMP
ping 10.64.0.3

# TCP
curl http://10.64.0.3:8080/
ssh -p 8022 u0_aXXX@10.64.0.3

# UDP
echo "hello" | nc -u 10.64.0.3 5353
```

**前提**：服务必须监听 `0.0.0.0` 或 `127.0.0.1`（不能只听某个具体 IP）。

### Termux 访问 PC 上的服务

**PC 端起服务**（监听 `0.0.0.0`）：

```bash
# Linux/macOS
python3 -m http.server 8080 --bind 0.0.0.0

# Windows
python -m http.server 8080 --bind 0.0.0.0
```

**Termux 端通过 SOCKS5 访问**：

```bash
# Termux-B
curl --socks5 127.0.0.1:1080 http://10.64.0.2:8080/
ssh -o ProxyCommand='nc -X 5 -x 127.0.0.1:1080 %h %p' user@10.64.0.2
```

### 数据面差异

| 维度 | **n2n-go-client** | n2n-termux | n2n-android |
|:---|:---|:---|:---|
| 数据面 | TUN 虚拟网卡（内核 TCP/IP 栈） | SOCKS5 + 包转发器 | VpnService |
| 应用透明 | ✅ 系统级路由 | ⚠️ 需应用显式配 SOCKS5 | ✅ 系统级 |
| 主动协议 | TCP / UDP / ICMP | 仅 TCP | TCP / UDP / ICMP |
| 被动协议 | TCP / UDP / ICMP | TCP / UDP / ICMP | TCP / UDP / ICMP |
| 权限要求 | root / 管理员 | 无 root | 无 root |

**关键**：三端的**数据面协议完全独立**，但在**服务端和 P2P 通道层用同一套协议**（N2NS 帧 + 打洞 + TURN 中继），所以完全互通。

---

## ⚙️ 环境变量

| 变量 | 必填 | 默认值 | 说明 |
|:---|:---|:---|:---|
| `SIGNALING_URL` | ✅ | — | edge-signal 的 WSS 地址，**必须带 `wss://` 前缀**（或 `ws://`） |
| `CONNECT_TOKEN` | ⚠️ | `""` | 连接密码（Worker 配了才需要）|
| `CLIENT_ID` | ❌ | 自动生成并持久化 | 客户端唯一 ID，保存在 `$INSTALL_DIR/client_id` |
| `ROOM_ID` | ❌ | `default-room` | 房间名。只允许 `[A-Za-z0-9_-]+` |
| `NODE_NAME` | ❌ | hostname | 显示名 |
| `TUN_NAME` | ❌ | `n2n0` | 虚拟网卡名（macOS 自动 `utun9`）|
| `UDP_PORT` | ❌ | `0` | P2P UDP 端口。`0`=内核自动分配 |
| `STUN_SERVERS` | ❌ | Google + Cloudflare | STUN 服务器 |
| `INSTALL_DIR` | ❌ | `$(pwd)/.n2n-go` | 安装目录，默认当前工作目录 |

**虚拟网段**由服务端 `wrangler.toml` 的 `VIRTUAL_NETWORK` 决定，客户端从 `ready` 消息自动接收。

---

## 🆕 版本行为变化说明

### 端口漂移保留 P2P 状态（P0 修复）

**行为变化**：从最新版本起，**只有出口 IP 变化时才清空连接状态**，端口漂移（HardNAT 常见）时保留 P2P 状态。

**为什么改**：

旧版 PC 端（与 Termux 一致）在收到 `joined`/`ready` 时，只要 `pubSocket` 变化就无条件 `ClearPeer`。HardNAT 下端口漂移频繁，导致每次 `joined`/`ready` 都清空 P2P 状态，P2P 通道反复断开重连。

**新行为**（与服务端 `room.ts`、Android 端对齐）：

- **IP 变化** → 清空连接状态，重新打洞
- **仅端口漂移** → 保留 P2P 状态，P2P 通道通常仍可用

**日志表现**：

```
[NAT-HOLE] xxx 出口 IP 变化 1.2.3.4 → 5.6.7.8，清空连接状态 (was=1)
[NAT-HOLE] xxx 端口漂移 1.2.3.4:5000 → 1.2.3.4:5001，保留 P2P 状态 (was=0)
```

### 重连清理更彻底（P1 修复）

**行为变化**：WS 重连成功后，除了清理 `failCounts`，还会清理 `natHoleActive` 中的 in-flight 标记。

**为什么改**：旧版重连后，残留的 `targetMac@rung` 标记会导致后续打洞指令被误判为"已在处理中"而跳过，直到该标记因超时被清除。

**新行为**：重连时同时清空 `failCounts` 和 `natHoleActive`，重连后打洞指令立即正常执行。

**日志表现**：

```
[NAT-HOLE] WS 重连，清空 2 个失败计数 / 1 个 in-flight 标记
```

### `_reconnected` 消息真正触发（P1 修复）

**行为变化**：WS 重连成功后，客户端会显式发送 `_reconnected` 消息，触发 `handleSignaling` 里的完整清理逻辑。

**为什么改**：旧版 `ws_transport.go` 重连成功后只调用 `onReconnect` 回调，**不发送 `_reconnected` 消息**，导致 `handleSignaling` 里的 `if t == "_reconnected"` 分支永远是死代码。

**新行为**：重连成功后调用 `ws.routeText({type:"_reconnected"})`，触发完整的清理流程（与 `onReconnect` 幂等）。

### 协议能力协商（features）

从最新版本起，客户端在 `p2p_metadata` 里上报 `features=["n2na","n2nw"]`。

**服务端自动协商 WS 中继帧格式**：

| 场景 | 服务端行为 |
|:---|:---|
| 新 PC ↔ 新 Termux / 新 Android | N2NA 二进制（省 ~27% 带宽） |
| 新 PC ↔ 旧客户端 | 自动降级到文本 `app_stream_data` |

**旧客户端完全兼容**，无需升级。

---

## 📋 启动日志

启动成功后：

```
n2n-go-client v1.3.0 启动
[配置] 未设置 CONNECT_TOKEN
[配置] 生成并持久化 CLIENT_ID: pc-a-1704067200 -> /opt/.n2n-go/client_id
[P2P] UDP 监听端口 54321
[NAT] 启动预收集 LAN: [192.168.1.100:54321]
[NAT] STUN 74.125.250.129:19302 → x.x.x.x:19190
[NAT] HardNAT（对称），ports_diff=0 pub=x.x.x.x:19190
[WS] 连接 wss://<你的-worker-域名>/ws/myroom?cid=pc-a
已连接信令，Client ID: pc-a-1704067200，节点名: A的电脑
分配虚拟 IP: 10.64.0.2（网段 10.64.0.0/24）
[NAT] 上报 LAN 地址: [192.168.1.100:54321]
[信令] 上报 p2p_metadata: natType=HardNAT publicEndpoint="x.x.x.x:19190" multiExit=true assisted=2 lan=1 features=[n2na,n2nw]
[TUN] n2n0 已启动，IP: 10.64.0.2/24，网段: 10.64.0.0/24
[Keepalive] 启动，每 5s 刷新 3 个 STUN 服务器
[P2P-Keepalive] 启动，每 15s 刷新 P2P 通道
```

**关键检查点**：

| 日志 | 含义 |
|:---|:---|
| `[NAT] 上报 LAN 地址: [...]` 非空 | LAN 收集成功 |
| `[TUN] n2n0 已启动` | **PC 独有**（不是 Termux 版） |
| `[WS] 连接 wss://...` | **URL 带了 wss://**（不是 `malformed`） |
| `[信令] 上报 p2p_metadata: ... features=[n2na,n2nw]` | 能力上报成功 |
| `[P2P] xxx 新建 peer，初始 UDPAddr 用 LAN: ...` | LAN 优先生效 |

当其他节点上线时（**LAN 直连命中**）：

```
========== 节点就绪: termux-b ==========
  虚拟 IP   : 10.64.0.3
  公网地址  : x.x.x.x:54322
  LAN 地址  : [192.168.1.101:54322]
  features  : [n2na n2nw]
==========================================

[NAT-HOLE] 开始打洞 role=0 target=192.168.1.101:54322 rung=0 mode=3 ttl=7
[P2P] 从 termux-b (192.168.1.101) 收到 Punch → P2PAvailable（等待数据帧验证）
[P2P] 收到 PONG → P2P 直连（双向通道已验证）
[连接] termux-b → P2P 直连（prev=p2p-avail，数据面已验证）
```

**打洞失败时**：

```
[NAT-HOLE] ❌ 失败 role=0 target=x.x.x.x attempts=110
[连接] termux-b → TURN 中继 (x.x.x.x:50000)
```

**单向 P2P 场景**（A→B 通，B→A 断）：

```
[P2P] 从 termux-b 收到 Punch → P2PAvailable（等待数据帧验证）
[P2P] termux-b P2PAvailable 超时 60s 未验证数据面，强制降级
[连接] termux-b → TURN 中继 (x.x.x.x:50000)
```

---

## 🔍 P2P 数据流原理

### LAN 地址发现（双路径）

1. **UDP connect 骗内核**：`net.Dial("udp4", "8.8.8.8:80")` 后取 `LocalAddr()`，不真正发包
2. **遍历 `net.Interfaces()`**：覆盖多网卡场景

两条路径结果合并去重，随 `p2p_metadata.lanEndpoints` 上报。

### LAN 优先保护（30 秒窗口）

**问题**：同 WiFi 场景下，peer 的 `UDPAddr` 可能在 LAN 和公网之间抖动，导致 ping 从 5ms 跳 200ms。

**修复**：

- **新建 peer 时**：优先用 LAN 端点初始化 `UDPAddr`
- **更新 UDPAddr 时**：
  - `!oldIsLAN && newIsLAN` → 立即更新（切到 LAN）
  - `oldIsLAN && !newIsLAN` → **只有 LAN 侧连续 30 秒无流量**才切到公网
  - `oldIsLAN && newIsLAN` → 同类型，端口变了才更新
- **发送数据时**：如果 peer 有 LAN 端点，**先发 LAN**，全失败才回退 `UDPAddr`

**效果**：同 WiFi 场景稳定走 LAN 直达（8-15ms），不再有 200ms 尖峰。

### P2P 通道状态机

```
Unknown → P2PAvailable → P2P
           ↑                ↑
      收到 Punch/ACK    收到 PONG 或
      打洞握手成功      真实数据帧
```

- 收到 **PING** → 只回 PONG，**不升级本端状态**
- 收到 **PONG** → **升级到 P2P**
- 收到 **SYN/SYNACK/DATA/ACK/FIN/RST** → **升级到 P2P**
- **P2PAvailable 60 秒超时** → 强制降级到 TURN/WS

### P2P 数据帧格式（21 字节头 + payload）

```
偏移  长度  字段
────────────────────────────
0     4     "N2NS"      魔数
4     1     ver         版本 1
5     1     type        SYN=1 SYNACK=2 DATA=3 ACK=4 FIN=5 RST=6 PING=7 PONG=8
6     4     streamID    大端 uint32
10    4     seq         本帧序列号
14    4     ack         确认收到的对方 seq
18    1     flags       保留
19    2     payloadLen  大端 uint16
21    N     payload     数据
```

**可靠性机制**：序列号 + ACK + 200ms 重传（最多 8 次）+ 滑动窗口 16 + 乱序缓存。

### N2NW / N2NA 二进制传输（WS 中继）

WS 中继数据面使用**两套**二进制帧：

**N2NW**（IPv4 数据面，6 字节头 + peerId + 裸 IPv4 包）：

```
  0    1    2    3    4     5      6..6+N        6+N..
┌────┬────┬────┬────┬─────┬──────┬─────────────┬────────────┐
│ 'N'│ '2'│ 'N'│ 'W'│flags│len(N)│ peerId(NB)  │  IPv4 包   │
└────┴────┴────┴────┴─────┴──────┴─────────────┴────────────┘
   客户端→服务端: peerId = 目标 clientId
   服务端→客户端: peerId = 发送方 clientId（服务端重写）
```

**N2NA**（app_stream 数据面，7 字节头 + peerId + streamId + TCP 字节）：

```
  0    1    2    3    4     5      6..6+N      6+N      7+N..7+N+M      7+N+M..
┌────┬────┬────┬────┬─────┬──────┬─────────────┬───────┬──────────────┬────────────┐
│ 'N'│ '2'│ 'N'│ 'A'│flags│len(N)│ peerId(NB)  │len(M) │ streamId(MB) │ TCP 字节   │
└────┴────┴────┴────┴─────┴──────┴─────────────┴───────┴──────────────┴────────────┘
   客户端→服务端: peerId = 目标 clientId
   服务端→客户端: peerId = 发送方 clientId（服务端重写）
```

**⚠️ 兼容性**：三端客户端和 edge-signal 服务端**必须同步升级**。旧服务端遇到 N2NW / N2NA magic 会静默丢弃，WS 中继数据面不通。

---

## 🎯 使用场景

### 场景 1：异地办公室组网

```bash
CLIENT_ID="sh-gateway" ROOM_ID="office" ...
```

组网后：`ssh user@10.64.0.x` 直接连。

### 场景 2：家庭 NAS 远程访问

```bash
CLIENT_ID="nas" ...
```

之后 `http://10.64.0.2:<NAS端口>` 访问 NAS 服务。

### 场景 3：PC + Termux + Android 三端混合组网

```bash
# PC
CLIENT_ID="pc-a" NODE_NAME="A的电脑" ...

# Termux（Android 手机）
CLIENT_ID="termux-a" NODE_NAME="A的手机" ...
```

**PC → Termux**：

```bash
curl http://10.64.0.3:8080/       # PC 走 TUN
ssh -p 8022 u0_aXXX@10.64.0.3
```

**Termux → PC**：

```bash
curl --socks5 127.0.0.1:1080 http://10.64.0.2:8080/
```

### 场景 4：路由器做网关

在 OpenWrt / 华硕路由器上跑 n2n-go-client，整个 LAN 即可访问组网内的其他节点：

```bash
CLIENT_ID="router-sh" ROOM_ID="office" TUN_NAME="n2n0" ...
```

---

## 🔧 从源码构建

```bash
git clone https://github.com/goyo123321/n2n-go-client.git
cd n2n-go-client
go mod tidy

# 当前平台
go build -o n2n-go-client .

# 交叉编译
GOOS=linux   GOARCH=amd64 CGO_ENABLED=0 go build -o n2n-go-client-linux-amd64 .
GOOS=darwin  GOARCH=arm64 CGO_ENABLED=0 go build -o n2n-go-client-darwin-arm64 .
GOOS=windows GOARCH=amd64 CGO_ENABLED=0 go build -o n2n-go-client-windows-amd64.exe .
```

**Windows 构建**：需要先下载 `wintun.dll`：

```bash
WINTUN_VERSION=0.14.1
curl -L -o wintun.zip "https://www.wintun.net/builds/wintun-${WINTUN_VERSION}.zip"
unzip -q wintun.zip -d wintun-extract
cp wintun-extract/wintun/bin/amd64/wintun.dll ./wintun.dll

GOOS=windows GOARCH=amd64 CGO_ENABLED=0 go build -o n2n-go-client-windows-amd64.exe .
```

---

## 📁 项目结构

```
n2n-go-client/
├── main.go                       # 主程序
├── ws_transport.go               # WebSocket 传输层
├── relay_fallback.go             # 四级降级管理器
├── turn_client.go                # TURN 客户端
├── turn_lite.go                  # TURN-Lite（UDP + TCP）
├── turn_lite_tcp.go              # TURN over TCP / RFC 6062
├── nat_probe.go                  # NAT 类型探测
├── nathole_executor.go           # 打洞指令执行
├── p2p_stream.go                 # N2NS 可靠流协议 + N2NW / N2NA 二进制帧
├── p2p_support.go                # P2P stream 依赖方法
├── punch.go                      # Punch/ACK 定义
├── socks5.go                     # SOCKS5 入口
├── app_stream.go                 # WS 应用层隧道（app_stream + N2NA）
├── lan.go                        # LAN 地址收集（双路径）
├── tun_linux.go                  # Linux TUN
├── tun_darwin.go                 # macOS utun
├── tun_windows.go                # Windows Wintun
├── tun_other.go                  # 其他平台占位
├── wintun_embed_windows.go       # wintun.dll 内嵌
├── install.sh                    # 一键部署脚本
└── go.mod
```

---

## ⚠️ 注意事项

### 权限要求

| 平台 | 要求 |
|:---|:---|
| Linux | `root` 或 `CAP_NET_ADMIN` |
| macOS | `sudo` |
| Windows | 管理员权限 |
| Termux + root | `tsu` / `su` / `sudo` |

### 防火墙

Windows 防火墙可能拦截 TUN 接口的 ICMP：

```
控制面板 → Windows Defender 防火墙 → 高级设置
→ 入站规则 → 新建规则 → 允许 ICMPv4
```

### CLIENT_ID 稳定性

**可以不手动指定**。未设置时客户端会在 `$INSTALL_DIR/client_id` 首次生成并持久化，重启复用，虚拟 IP 不会漂移。

**但手动指定仍然推荐**，尤其在以下场景：
- 多台设备共用同一个 `$INSTALL_DIR`（如 NFS 挂载）
- 用 `sudo` 运行时 `$HOME` 会变成 `/root`，导致 ID 文件位置变化
- 想用可读的 ID（如 `pc-a`、`nas`）方便在面板里识别

### 虚拟网段

网段由**服务端** `wrangler.toml` 的 `VIRTUAL_NETWORK` 决定。**更换网段需要同时重启所有客户端**。

---

## 📊 性能参考

| 连接方式 | 延迟 | 消耗 |
|:---|:---|:---|
| **LAN 直连** | **<5ms** | **无** |
| P2P 直连（跨 NAT） | 10~50ms | 无 |
| **CGNAT hairpin** | **10~30ms** | **无** |
| TURN 中继 | 30~100ms | Cloudflare TURN 配额 |
| WebSocket 中继 | 100~300ms | Worker 请求配额 |

---

## 🐛 常见问题

### Q: 启动报错 `No such file or directory`？

**原因**：路径写错了。**安装目录现在是 `<当前工作目录>/.n2n-go`**，不再是 `~/.n2n-go`。

**快速判断**：

```bash
# 到之前运行的目录找
ls -la .n2n-go/n2n-go-client

# 或者全局搜
find ~ -name "n2n-go-client" -type f 2>/dev/null
find /opt -name "n2n-go-client" -type f 2>/dev/null
```

**建议**：安装时看到脚本输出里的 `安装目录: /实际/绝对/路径/.n2n-go`，直接复制那个路径。

### Q: 启动报错 `连接信令失败: malformed ws or wss URL`？

**原因**：`SIGNALING_URL` 漏了 `wss://` 前缀。

**修复**：

```bash
sudo SIGNALING_URL="wss://<你的-worker-域名>" \
     ROOM_ID="myroom" \
     CLIENT_ID="pc-a" \
     ./start.sh
```

**格式说明**：

| 输入 | 结果 |
|:---|:---|
| `wss://your-worker.example.com` | ✅ |
| `ws://localhost:8080` | ✅ |
| `https://your-worker.example.com` | ⚠️ 会被自动转成 `wss://` |
| `your-worker.example.com` | ❌ 缺前缀 |
| `wss://your-worker.example.com/ws/myroom` | ❌ 不要带路径 |

### Q: 启动报错 `permission denied: /dev/net/tun`？

**原因**：**没有 root 权限**。TUN 需要 root。

**修复**：

```bash
# 加 sudo
sudo ./start.sh

# 或者切换到 root
su -
./start.sh
```

**Termux + root**：

```bash
tsu -c ./start.sh
# 或
sudo ./start.sh
```

### Q: 启动报错 `未找到 /dev/net/tun`？

**原因**：系统没有加载 tun 模块。

**修复**：

```bash
# Linux
sudo modprobe tun
ls -la /dev/net/tun

# 如果 modprobe 失败，安装内核模块
sudo apt install linux-modules-extra-$(uname -r)

# Termux + root
su -c "insmod /system/lib/modules/tun.ko" 2>/dev/null
su -c "modprobe tun" 2>/dev/null
# 有些手机需要重启后才有 /dev/net/tun
```

### Q: 在 Termux 里装 n2n-go-client 提示「Termux 环境但没有 TUN」？

**原因**：**这是正常的**。Termux 无 root 时没有 TUN 权限，这个客户端跑不起来。

**两个选择**：

**选择 A（推荐）**：用 [n2n-termux](https://github.com/goyo123321/n2n-termux)——SOCKS5 模式，无需 root：

```bash
curl -fsSL https://raw.githubusercontent.com/goyo123321/n2n-termux/main/install.sh -o ~/install_n2n_termux.sh
bash ~/install_n2n_termux.sh
```

**选择 B**：手机已 root，先加载 tun 模块再装：

```bash
su -c "ls -la /dev/net/tun"               # 确认存在
su -c "insmod /system/lib/modules/tun.ko"  # 不存在时加载
bash ~/install_n2n.sh                     # 重新运行安装
```

### Q: ping 不通怎么办？

**先确认 TUN 起来了**：

```bash
ip addr show n2n0       # Linux
ifconfig utun9          # macOS
```

**再检查路由**：

```bash
ip route | grep 10.64   # Linux
netstat -rn | grep 10.64  # macOS
```

**Windows 防火墙可能拦 ICMP**——用 TCP 测更可靠：

```bash
curl -v --max-time 5 http://10.64.0.3:8080/
```

### Q: 如何判断走的是 LAN / P2P / TURN / WS？

**看日志**：

```
[P2P] 从 termux-b (192.168.1.101) 收到 Punch      ← LAN
[P2P] 从 termux-b (x.x.x.x) 收到 Punch             ← 公网 P2P
[连接] termux-b → TURN 中继 (x.x.x.x:50000)        ← TURN
[连接] termux-b → WS 中继 (最后兜底)               ← WS
```

**看服务端面板**：

```
连接状态：p2p-B      ← LAN 或 P2P
连接状态：TURN-B     ← TURN
连接状态：ws-B       ← WS
```

**中继流量**：如果 LAN/P2P 生效，**TURN 中继流量保持 0 B**。

### Q: 同 WiFi 延迟波动很大（100ms+）？

**已修复**（v1.3.0+）。原因是 `UDPAddr` 在 LAN 和公网之间抖动，导致回程走 CGNAT 回环。

**修复后**：同 WiFi 场景延迟稳定在 **8~15ms**，标准差 <2ms。

### Q: 单向 P2P 场景（A→B 通，B→A 断）会怎样？

**PC 端保持 P2PAvailable，60 秒后自动降级到 TURN/WS，不会卡在 P2P 丢包。**

**原因**：PC 收到 PING 只更新活跃时间，**只有收到 PONG 或数据帧才升级 P2P**。单向 P2P 场景下保持 `ConnP2PAvailable`，60 秒后 `scheduleP2PAvailTimeout` 触发 `ForceFallback`。

**日志表现**：

```
[P2P] 收到 PONG → P2P 直连（双向通道已验证）    ← 正常
[P2P] xxx P2PAvailable 超时 60s 未验证数据面，强制降级   ← 单向 P2P
```

### Q: HardNAT 下 P2P 反复断开重连？

**已修复**（P0）。之前每次 `joined`/`ready` 都会无条件清空 P2P 状态，HardNAT 下端口漂移频繁导致反复断开。

**修复后**：仅出口 IP 变化时才清空连接状态，端口漂移时保留 P2P 通道。

**验证日志**：

```
[NAT-HOLE] xxx 端口漂移 1.2.3.4:5000 → 1.2.3.4:5001，保留 P2P 状态
```

### Q: WS 重连后打洞指令不执行？

**已修复**（P1）。之前重连后 `natHoleActive` 里的旧标记残留，导致后续打洞指令被误判为"已在处理中"而跳过。

**修复后**：重连时同时清空 `failCounts` 和 `natHoleActive`。

**验证日志**：

```
[NAT-HOLE] WS 重连，清空 N 个失败计数 / M 个 in-flight 标记
```

### Q: 息屏后 ping 有 200-500ms 尖峰？

**这是 Android WiFi 芯片的 PS-Poll 省电模式，与 n2n 无关**。即使 PC 直连网关也会有相同尖峰。

**PC 端不受影响**——PC 没有 WiFi 芯片省电问题。

**Android / Termux 端的应对**：
1. 路由器关闭 U-APSD
2. 切 5GHz 频段
3. DTIM 周期改 1
4. Android 电池优化白名单

### Q: 重启后虚拟 IP 变了？

客户端会持久化 `CLIENT_ID` 到 `$INSTALL_DIR/client_id`，正常情况下重启 IP 不变。检查：

```bash
cat ~/.n2n-go/client_id
```

**如果变了**：可能是不同的安装目录（比如之前装在 `~/.n2n-go`，现在装在 `$(pwd)/.n2n-go`），`client_id` 文件位置不同导致 ID 变了。

### Q: 从任意目录调用 start.sh 会不会找错配置文件？

**不会**。`start.sh` 里用**自身所在目录推导 `INSTALL_DIR`**：

```bash
if [ -z "${INSTALL_DIR:-}" ]; then
    INSTALL_DIR="$(cd "$(dirname "$0")" && pwd)"
fi
```

所以从任意目录调用都能正确找到 `config.env` 和 `n2n-go-client`。

### Q: 混合房间（新旧客户端共存）会丢数据吗？

**不会**。

服务端按目标端 `features` 自动协商：

| 发送方 | 接收方 | 数据面 |
|:---|:---|:---|
| 新 PC | 新 Termux / 新 Android | N2NA 二进制 |
| 新 PC | 旧客户端 | 文本 `app_stream_data` |
| 旧客户端 | 新 PC | 文本（新 PC 兼容） |

**旧客户端完全无感知**，新 PC 自动获得二进制优化。

### Q: 管理面板为什么显示 `无能力上报`？

**旧客户端**（升级 features 之前的版本）不上报 `features` 字段。

- **不影响功能**：服务端自动降级到文本 `app_stream_data`，连接正常
- **升级后消失**：升级到最新版本后，管理面板会显示 `n2na` / `n2nw` chips

---

## 🔗 与 edge-signal 配合

| 项 | edge-signal | n2n-go-client |
|:---|:---|:---|
| 部署位置 | Cloudflare 边缘 | 用户设备 |
| 角色 | 信令服务器 + TURN 凭证下发 + 中继兜底 | 建立 TUN + LAN/P2P/TURN 连接 |
| 语言 | TypeScript | Go |
| 存储 | Durable Objects | 本地 |

**先部署 edge-signal，再运行客户端。**

---

## 📄 License

MIT

## 🔗 相关项目

| 项目 | 平台 | 模式 | 互通 |
|:---|:---|:---|:---|
| [edge-signal](https://github.com/goyo123321/edge-signal) | Cloudflare Workers | 信令服务器 | — |
| **n2n-go-client** | Linux / macOS / Windows / 路由器 | **TUN** | ✅ 全部 |
| [n2n-termux](https://github.com/goyo123321/n2n-termux) | Android Termux | **SOCKS5 + 包转发** | ✅ 全部 |
| [n2n-android](https://github.com/goyo123321/n2n-android) | Android APK | VpnService | ✅ 全部 |
