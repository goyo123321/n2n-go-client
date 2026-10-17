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
- **UDP 保活** — 每 5 秒刷新 STUN 映射，防止 CGNAT 端口漂移
- **P2P 通道保活** — 每 15 秒往对端发 N2NS PING 多目标探针，防止空闲时通道被回收
- **TURN 中继** — 支持 Cloudflare TURN + 自定义 TURN，不消耗 Worker 配额
- **中继回退** — TURN 失败自动降级到 WebSocket 中继
- **N2NW 二进制传输** — WS 中继数据面用二进制帧，比 base64 省 33% 带宽
- **断线重连** — WebSocket 自动重连（指数退避）+ 90 秒读超时检测下行僵死
- **连接密码** — 支持 `CONNECT_TOKEN` 保护 Worker
- **ID 持久化** — 未指定 `CLIENT_ID` 时自动生成并保存，重启复用
- **虚拟网段可配** — 服务端通过 `VIRTUAL_NETWORK` 下发网段，客户端自动适配
- **无依赖** — Windows 版内嵌 wintun.dll
- **三端互通** — 与 [n2n-termux](https://github.com/goyo123321/n2n-termux)、[n2n-android](https://github.com/goyo123321/n2n-android) 无缝互通

---

## 📂 二进制路径速查（⚠️ 最容易踩的坑）

**两种安装方式，二进制路径完全不同**，先看清楚再启动：

| 安装方式 | 二进制位置 | 启动命令 |
|:---|:---|:---|
| **方式 A：`install.sh` 自动安装** | `~/.n2n-go/n2n-go-client`<br>（**带点的隐藏目录**） | `sudo ~/.n2n-go/start.sh` |
| **方式 B：手动下载** | `~/n2n-go-client`<br>（`curl -o ~/n2n-go-client` 下载到的位置） | `sudo ~/n2n-go-client` |

**常见错误对照**：

```bash
# ❌ 错误：install.sh 装的，却用 ~/n2n-go-client 启动
sudo ~/n2n-go-client
# sudo: /home/user/n2n-go-client: command not found

# ✅ 正确：install.sh 装的用带点目录
sudo ~/.n2n-go/n2n-go-client
# 或直接
sudo ~/.n2n-go/start.sh

# ✅ 正确：手动下载到 ~/ 的用这个
sudo ~/n2n-go-client
```

**验证二进制在哪**：

```bash
ls -lh ~/.n2n-go/n2n-go-client    # install.sh 装的（存在 → 用这个）
ls -lh ~/n2n-go-client            # 手动下载的（存在 → 用这个）
```

---

## 📦 二进制命名说明

从 **v1.3.0** 起，Release 二进制名带 `n2n-go-client-` 前缀，与 `n2n-termux` 区分：

| 项目 | 二进制名 |
|:---|:---|
| **n2n-go-client**（本仓库） | `n2n-go-client-linux-amd64` / `n2n-go-client-linux-arm64` / `n2n-go-client-linux-armv7` / `n2n-go-client-darwin-amd64` / `n2n-go-client-darwin-arm64` / `n2n-go-client-windows-amd64.exe` |
| n2n-termux | `n2n-termux-linux-arm64` / `n2n-termux-linux-armv7` / `n2n-termux-linux-amd64` |

**好处**：下载后一眼看出是哪个，两个项目放同一目录也不冲突，`install.sh` 的 pgrep 也不会误杀。

**历史版本**（v1.2.x 及之前）的二进制名是 `n2n-client-linux-*`，与 n2n-termux 同名——如果还在用旧版，下载后手动改个名：

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
- **Termux**：SOCKS5 代理 → 应用**必须显式配代理**，主动访问仅 TCP
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

**两者互补**：

- **UDP 保活**：让 CGNAT 认为"这个 socket 活跃"，不回收端口
- **P2P 通道保活**：走 `collectN2NSTargets` 多目标发送（LAN + STUN 反射 + 观测 raddr + 注册地址），命中率远高于单目标

**流量代价**：
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
> 2. **PC 端需要 `sudo`**（`/dev/net/tun` / macOS `utun` 都需要 root 权限）。
> 3. **二进制路径**：`install.sh` 装的是 `~/.n2n-go/n2n-go-client`（带点的隐藏目录）；手动下载的是 `~/n2n-go-client`。详见 [📂 二进制路径速查](#-二进制路径速查-最容易踩的坑)。

### 方式 A：一键安装脚本（推荐）

```bash
bash <(curl -fsSL https://raw.githubusercontent.com/goyo123321/n2n-go-client/main/install.sh)
```

或：

```bash
curl -fsSL https://raw.githubusercontent.com/goyo123321/n2n-go-client/main/install.sh | bash
```

> 部署脚本执行完后默认**自动启动客户端**，直接回车即可。

**安装位置**：`~/.n2n-go/n2n-go-client`

**日常操作**：

```bash
sudo ~/.n2n-go/start.sh                # 前台启动
sudo ~/.n2n-go/start.sh                # 查看日志
```

**macOS 额外**：`TUN_NAME=utun9 sudo ~/.n2n-go/start.sh`（`n2n0` 在 macOS 上无效）

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

**安装位置**：`~/n2n-go-client`（**没有点，直接在 home 下**）

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

> ⚠️ **注意**：方式 B 下载的二进制是 `~/n2n-go-client-*`，和 install.sh 的 `~/.n2n-go/n2n-go-client` **不是同一个位置**。不要混用。

---

## 🔗 与 Termux / Android 互通

### PC 访问 Termux / Android 上的服务

**Termux / Android 端起服务**：

```bash
# Termux 端
python3 -m http.server 8080 &
sshd  # 默认监听 8022

# Android 端（VpnService 模式）
# 起 HTTP / SSH / 任意 TCP/UDP 服务
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

```bash
# ✅ 可以
python3 -m http.server 8080 --bind 0.0.0.0

# ✅ 也可以（packet_forwarder 会 dial 127.0.0.1）
python3 -m http.server 8080 --bind 127.0.0.1

# ❌ 不行（只监听某个具体 IP）
python3 -m http.server 8080 --bind 192.168.1.100
```

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

### Android 访问 PC 上的服务

Android 端也是 TUN 数据面，直接访问虚拟 IP：

```bash
# Android 上跑（Termux 或 App 内）
curl http://10.64.0.2:8080/
```

### 数据面差异

| 维度 | **n2n-go-client** | n2n-termux | n2n-android |
|:---|:---|:---|:---|
| 数据面 | TUN 虚拟网卡（内核 TCP/IP 栈） | SOCKS5 + 包转发器 | VpnService |
| 应用透明 | ✅ 系统级路由 | ⚠️ 需应用显式配 SOCKS5 | ✅ 系统级 |
| 主动协议 | TCP / UDP / ICMP | 仅 TCP | TCP / UDP / ICMP |
| 被动协议 | TCP / UDP / ICMP | TCP / UDP / ICMP | TCP / UDP / ICMP |
| 权限要求 | root / 管理员 | 无 root | 无 root |
| 使用方式 | `curl http://10.64.0.X` | `curl --socks5 127.0.0.1:1080 ...` | 同 TUN |

**关键**：**三端的数据面协议完全独立**，但**在服务端和 P2P 通道层用同一套协议**（N2NS 帧 + 打洞 + TURN 中继），所以完全互通。

---

## ⚙️ 环境变量

| 变量 | 必填 | 默认值 | 说明 |
|:---|:---|:---|:---|
| `SIGNALING_URL` | ✅ | — | edge-signal 的 WSS 地址，**必须带 `wss://` 前缀**（或 `ws://`） |
| `CONNECT_TOKEN` | ⚠️ | `""` | 连接密码（Worker 配了才需要）|
| `CLIENT_ID` | ❌ | 自动生成并持久化 | 客户端唯一 ID。未设置时首次启动生成 `hostname-随机数`，保存在 `$INSTALL_DIR/client_id`，重启复用 |
| `ROOM_ID` | ❌ | `default-room` | 房间名。只允许 `[A-Za-z0-9_-]+` |
| `NODE_NAME` | ❌ | hostname | 显示名 |
| `TUN_NAME` | ❌ | `n2n0` | 虚拟网卡名（macOS 用 `utun9`）|
| `UDP_PORT` | ❌ | `0` | P2P UDP 端口。`0`=内核自动分配（推荐），指定端口被占用时自动回退 |
| `STUN_SERVERS` | ❌ | Google + Cloudflare | STUN 服务器 |
| `SOCKS5_LISTEN` | ❌ | `127.0.0.1:1080` | SOCKS5 监听地址（额外入口，可选）|
| `INSTALL_DIR` | ❌ | `~/.n2n-go` | install.sh 的安装目录 |

**虚拟网段**由服务端 `wrangler.toml` 的 `VIRTUAL_NETWORK` 决定，客户端从 `ready` 消息自动接收，无需配置。默认 `10.64.0.0/24`。

---

## 📋 启动日志

启动成功后：

```
n2n-go-client v1.3.0 启动
[配置] 未设置 CONNECT_TOKEN
[配置] 生成并持久化 CLIENT_ID: pc-a-1704067200 -> /home/user/.n2n-go/client_id
[P2P] UDP 监听端口 54321
[NAT] 启动预收集 LAN: [192.168.1.100:54321]
[NAT] STUN 74.125.250.129:19302 → x.x.x.x:19190
[NAT] HardNAT（对称），ports_diff=0 pub=x.x.x.x:19190
[WS] 连接 wss://<你的-worker-域名>/ws/myroom?cid=pc-a
已连接信令，Client ID: pc-a-1704067200，节点名: A的电脑
[WS] 重放 1 条早期文本消息
分配虚拟 IP: 10.64.0.2（网段 10.64.0.0/24）
[信令] 服务端看到的本机出口 IP: x.x.x.x（WS/TCP 出口，仅参考）
[NAT] 上报 LAN 地址: [192.168.1.100:54321]
[信令] 上报 p2p_metadata: natType=HardNAT publicEndpoint="x.x.x.x:19190" multiExit=true assisted=2 lan=1
[TUN] n2n0 已启动，IP: 10.64.0.2/24，网段: 10.64.0.0/24
[TURN] 获取到 custom TURN 服务器: turn:xxx:3478
[TURN] 中继地址: x.x.x.x:50000
[Keepalive] 启动，每 5s 刷新 3 个 STUN 服务器
[P2P-Keepalive] 启动，每 15s 刷新 P2P 通道
[SOCKS5] 监听 127.0.0.1:1080（用 curl --socks5 或 ALL_PROXY 访问虚拟 IP）

================= 本机信息 =================
  Client ID   : pc-a-1704067200
  节点名       : A的电脑
  虚拟 IP     : 10.64.0.2
  虚拟网段    : 10.64.0.0/24
  P2P 端口    : 54321
==========================================
```

**关键检查点**：

| 日志 | 含义 |
|:---|:---|
| `[NAT] 上报 LAN 地址: [...]` 非空 | LAN 收集成功（LAN 优先前提） |
| `[TUN] n2n0 已启动` | **PC 独有**（不是 Termux 版） |
| `[WS] 连接 wss://...` | **URL 带了 wss://**（不是 `malformed`） |
| `[P2P] xxx 新建 peer，初始 UDPAddr 用 LAN: ...` | LAN 优先修复生效 |

当其他节点上线时（**LAN 直连命中**）：

```
========== 节点就绪: termux-b ==========
  虚拟 IP   : 10.64.0.3
  公网地址  : x.x.x.x:54322
  LAN 地址  : [192.168.1.101:54322]
  STUN 端点 : [x.x.x.x:54322 x.x.x.x:54335]
==========================================

[registerPeer] termux-b: 收到 assisted=2 个 [...] lanEndpoints=1 个 [192.168.1.101:54322]
[registerPeer] termux-b: sortedLAN=[192.168.1.101:54322] (prefixes=1)
[NAT-HOLE] 开始打洞 role=0 target=192.168.1.101:54322 rung=0 mode=3 ttl=7
[collectScanTargets] 去重后 4 个候选: [...]
[P2P] 从 termux-b (192.168.1.101) 收到 Punch → P2PAvailable（等待数据帧验证）
[P2P] 收到 PONG → P2P 直连（双向通道已验证）
[连接] termux-b → P2P 直连（prev=p2p-avail，数据面已验证）
[NAT-HOLE] ✅ 成功 role=0 target=192.168.1.101 attempts=4 tier=±3
```

**打洞失败时**：

```
[NAT-HOLE] ❌ 失败 role=0 target=x.x.x.x attempts=110
[连接] termux-b → TURN 中继 (x.x.x.x:50000)
```

**TURN 也不可用时**：

```
[连接] termux-b → WS 中继 (TURN 未就绪，最后兜底)
```

---

## 🔍 P2P 数据流原理

### LAN 地址发现（双路径）

**路径 1：UDP connect 骗内核**

```go
conn, _ := net.Dial("udp4", "8.8.8.8:80")
localIP := conn.LocalAddr().(*net.UDPAddr).IP
```

connect UDP **不真正发包**，只更新内核路由缓存。`getsockname()` 返回内核为该 socket 选定的出口 IP —— 即"出网卡"的 IP。

- WiFi 下返回 `192.168.x.x` —— 直接可用
- 蜂窝下返回运营商内网 IP（`10.x` / `100.64.x`）—— 会被过滤掉
- 不触发 netlink 权限检查，任何环境都能用

**路径 2：遍历 `net.Interfaces()`**

覆盖多网卡场景（WiFi + USB 网卡）。两条路径结果合并去重。

**上报链路**：`p2p_metadata` 里带 `lanEndpoints` → 服务端存储 → 广播 `joined` → 对端 `registerPeer` 时收到。

### 打洞候选（score 制匹配）

对端可能从**三个不同源地址**发来 Punch/ACK：

| 源 | 分数 | 场景 |
|:---|:---|:---|
| `p.UDPAddr` 完全匹配 | 4 | 公网地址未变 |
| `p.UDPAddr` IP 匹配 | 3 | 端口漂移 |
| `p.LanEndpoints` 完全匹配 | **5（最高）** | 同 WiFi 直连 |
| `p.LanEndpoints` IP 匹配 | 2 | LAN 端口漂移 |
| `p.AssistedEndpoints` 完全匹配 | 4 | STUN 反射 |
| `p.AssistedEndpoints` IP 匹配 | 1 | 反射端口漂移 |

**最高分匹配**优先。

### P2P 通道状态机

```
Unknown → P2PAvailable → P2P
           ↑                ↑
      收到 Punch/ACK    收到 PONG 或
      打洞握手成功      真实数据帧
```

**PING/PONG 语义**（关键）：

- 收到 **PING** → 只回 PONG，**不升级本端状态**（单向证明）
- 收到 **PONG** → 双向通道已验证 → **升级到 P2P**
- 收到 **SYN/SYNACK/DATA/ACK/FIN/RST** → **升级到 P2P**（真实数据帧）

**P2PAvailable 30 秒超时**：打洞握手成功但 30 秒内没升到 P2P → 强制降级到 TURN/WS，避免永远卡在中间态。

### LAN 优先保护（30 秒窗口）

**问题**：同 WiFi 场景下，peer 的 `UDPAddr` 可能在 LAN 和公网之间抖动，导致 ping 从 5ms 跳 200ms。

**修复**（v1.3.0+）：

- **新建 peer 时**：优先用 LAN 端点初始化 `UDPAddr`
- **更新 UDPAddr 时**：
  - `!oldIsLAN && newIsLAN` → 立即更新（切到 LAN）
  - `oldIsLAN && !newIsLAN` → **只有 LAN 侧连续 30 秒无流量**才切到公网
  - `oldIsLAN && newIsLAN` → 同类型，端口变了才更新
- **发送数据时**：如果 peer 有 LAN 端点，**先发 LAN**，全失败才回退 `UDPAddr`

**效果**：同 WiFi 场景稳定走 LAN 直达（8-15ms），不再有 200ms 尖峰。

### P2P 数据帧格式（21 字节头 + payload）

```
偏移  长度  字段
────────────────────────────
0     4     "N2NS"      魔数
4     1     ver         版本 1
5     1     type        SYN=1 SYNACK=2 DATA=3 ACK=4 FIN=5 RST=6 PING=7 PONG=8
6     4     streamID    大端 uint32（偶数）
10    4     seq         本帧序列号
14    4     ack         确认收到的对方 seq
18    1     flags       保留
19    2     payloadLen  大端 uint16
21    N     payload     数据
```

**可靠性机制**：

- 每帧带序列号 + ACK，接收方按序交付
- 未 ACK 200ms 重传，最多 8 次
- 滑动窗口 16 帧
- 乱序缓存后按序交付

### N2NW 二进制传输（WS 中继）

WS 中继数据面使用 **N2NW 二进制帧**（6 字节头 + peerId + 裸 IPv4 包）：

```
  0    1    2    3    4     5      6..6+N        6+N..
┌────┬────┬────┬────┬─────┬──────┬─────────────┬────────────┐
│ 'N'│ '2'│ 'N'│ 'W'│flags│len(N)│ peerId(NB)  │  IPv4 包   │
└────┴────┴────┴────┴─────┴──────┴─────────────┴────────────┘
   客户端→服务端: peerId = 目标 clientId
   服务端→客户端: peerId = 发送方 clientId（服务端重写）
```

**⚠️ 兼容性**：三端客户端和 edge-signal 服务端**必须同步升级**。旧服务端遇到 N2NW magic 会静默丢弃（`data[0]>>4 != 4`），导致 WS 中继数据面不通。

### 多目标发送

`sendP2PFrame` 走 `collectN2NSTargets` 收集所有候选：

1. LAN 地址（同 WiFi 优先）
2. AssistedEndpoints（STUN 反射多端点）
3. lastRecvFrom（对端最近发来的源地址）
4. peerAddr（注册分配）
5. peer.UDPAddr（观测 raddr）

**一帧同时发往所有候选**，命中率大幅提升。

---

## 🎯 使用场景

### 场景 1：异地办公室组网

```bash
# 上海
CLIENT_ID="sh-gateway" ROOM_ID="office" ...

# 北京
CLIENT_ID="bj-gateway" ROOM_ID="office" ...
```

组网后：`ssh user@10.64.0.x` 直接连。

### 场景 2：家庭 NAS 远程访问

```bash
# 在家 NAS 上
CLIENT_ID="nas" ...

# 在外面笔记本上
CLIENT_ID="laptop" ...
```

之后 `http://10.64.0.2:<NAS端口>` 访问 NAS 服务。

### 场景 3：PC + Termux + Android 三端混合组网

```bash
# PC
CLIENT_ID="pc-a" NODE_NAME="A的电脑" ...

# Termux（Android 手机）
CLIENT_ID="termux-a" NODE_NAME="A的手机" ...

# Android APK（另一台手机）
CLIENT_ID="android-b" NODE_NAME="B的手机" ...
```

**PC → Termux**：

```bash
curl http://10.64.0.3:8080/       # PC 走 TUN
ssh -p 8022 u0_aXXX@10.64.0.3
```

**PC → Android**：

```bash
curl http://10.64.0.4:8080/       # Android 走 TUN
ping 10.64.0.4                    # ICMP 也支持
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

路由器后面的设备可通过 `10.64.0.x` 直接访问其他城市的办公室资源。

### 场景 5：游戏联机

```bash
# 玩家 A（PC）
CLIENT_ID="player-a" ROOM_ID="game-night" ...

# 玩家 B（Android APK，也是 TUN）
CLIENT_ID="android-b" ROOM_ID="game-night" ...
```

**PC ↔ Android 可以玩 UDP 游戏**（双端 TUN，支持 UDP/ICMP）。

**Termux 端仅 TCP**，不支持游戏常见的 UDP 数据面。

---

## 🔧 从源码构建

### 前置要求

- Go 1.21+
- Git

### 构建

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

**Windows 构建特殊说明**：需要先下载 `wintun.dll`：

```bash
WINTUN_VERSION=0.14.1
curl -L -o wintun.zip "https://www.wintun.net/builds/wintun-${WINTUN_VERSION}.zip"
unzip -q wintun.zip -d wintun-extract
cp wintun-extract/wintun/bin/amd64/wintun.dll ./wintun.dll

GOOS=windows GOARCH=amd64 CGO_ENABLED=0 go build -o n2n-go-client-windows-amd64.exe .
```

### GitHub Actions 自动构建

手动触发：

```
Actions → Build n2n-go-client → Run workflow
  version: v1.3.0
  publish: ✓
  platforms: （留空=全部5个平台）
```

---

## 📁 项目结构

```
n2n-go-client/
├── main.go                       # 主程序（含 UDP 保活 + P2P 通道保活 + LAN 收集）
├── ws_transport.go               # WebSocket 传输层（断线重连 + 90s 读超时 + 早期消息缓冲）
├── relay_fallback.go             # 四级降级管理器（含 ForceFallback + LAN 优先发送）
├── turn_client.go                # TURN 客户端
├── turn_lite.go                  # TURN-Lite 实现（UDP + TCP）
├── turn_lite_tcp.go              # TURN over TCP / RFC 6062
├── nat_probe.go                  # NAT 类型探测
├── nathole_executor.go           # 打洞指令执行（分阶段扫描 + 30s LAN 窗口）
├── p2p_stream.go                 # N2NS 可靠流协议 + N2NW 二进制帧
├── p2p_support.go                # P2P stream 依赖方法（LAN 优先 + 30s 窗口）
├── punch.go                      # Punch/ACK 定义
├── socks5.go                     # SOCKS5 入口
├── app_stream.go                 # WS 应用层隧道（app_stream）
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

### 防火墙

Windows 防火墙可能拦截 TUN 接口的 ICMP。如无法 ping 通：

```
控制面板 → Windows Defender 防火墙 → 高级设置
→ 入站规则 → 新建规则 → 允许 ICMPv4
```

### CLIENT_ID 稳定性

**可以不手动指定**。未设置时客户端会在 `$INSTALL_DIR/client_id`
（默认 `~/.n2n-go/client_id`）首次生成并持久化一个 ID，重启复用，虚拟 IP 不会漂移。

**但手动指定仍然推荐**，尤其在以下场景：
- 多台设备共用同一个 `$INSTALL_DIR`（如 NFS 挂载）
- 用 `sudo` 运行时 `$HOME` 会变成 `/root`，导致 ID 文件位置变化
- 想用可读的 ID（如 `pc-a`、`nas`）方便在面板里识别
- 迁移机器时想保留原来的虚拟 IP

### UDP 端口

默认 `UDP_PORT=0`，内核自动分配。同一台机器跑多个客户端时不会冲突。

需要固定端口（比如为了防火墙规则）时显式指定：

```bash
UDP_PORT=50001 ./n2n-go-client ...
```

指定的端口被占用时，客户端会**自动回退**到内核分配，不会启动失败。

### 虚拟网段

网段由**服务端** `wrangler.toml` 的 `VIRTUAL_NETWORK` 决定，客户端在连接时从 `ready` 消息接收。

**更换网段需要同时重启所有客户端**——旧的 IP 和路由是旧网段的，连接后会被新网段覆盖。

---

## 📊 性能参考

| 连接方式 | 延迟 | 消耗 |
|:---|:---|:---|
| **LAN 直连** | **<5ms** | **无** |
| P2P 直连（跨 NAT） | 10~50ms | 无 |
| **CGNAT hairpin** | **10~30ms** | **无** |
| TURN 中继 | 30~100ms | Cloudflare TURN 配额 |
| WebSocket 中继 | 100~300ms | Worker 请求配额 |

**PC 端性能最优**——直接走 TUN 内核栈，无 SOCKS5 代理层开销。

---

## 🐛 常见问题

### Q: 启动报错 `No such file or directory`，找不到二进制？

**原因**：**路径写错了**。`install.sh` 装的和手动下载的路径完全不同。

**快速判断**：

```bash
# 用 install.sh 装的 → 二进制在 ~/.n2n-go/n2n-go-client
ls -lh ~/.n2n-go/n2n-go-client

# 手动下载的 → 二进制在 ~/n2n-go-client
ls -lh ~/n2n-go-client
```

**哪个存在就用哪个**：

```bash
# install.sh 装的
sudo ~/.n2n-go/n2n-go-client
# 或直接
sudo ~/.n2n-go/start.sh

# 手动下载的
sudo ~/n2n-go-client
```

**详细对照**：

| 场景 | 错误命令 | 正确命令 |
|:---|:---|:---|
| install.sh 装完，手动跑 | `~/n2n-go-client` | `~/.n2n-go/n2n-go-client` |
| install.sh 装完，正常启动 | — | `sudo ~/.n2n-go/start.sh` |
| 手动下载到 home | `~/.n2n-go/n2n-go-client` | `~/n2n-go-client` |

### Q: 启动报错 `连接信令失败: malformed ws or wss URL`？

**原因**：`SIGNALING_URL` 漏了 `wss://`（或 `ws://`）协议前缀。

**修复**：

```bash
sudo SIGNALING_URL="wss://<你的-worker-域名>" \
     ROOM_ID="myroom" \
     CLIENT_ID="pc-a" \
     ~/.n2n-go/n2n-go-client
```

**正确的 SIGNALING_URL 格式**：

| 输入 | 结果 |
|:---|:---|
| `wss://your-worker.example.com` | ✅ 正确 |
| `ws://localhost:8080` | ✅ 正确（本地调试） |
| `https://your-worker.example.com` | ⚠️ 会被自动转成 `wss://` |
| `your-worker.example.com` | ❌ 缺前缀 |
| `wss://your-worker.example.com/ws/myroom` | ❌ 不要带路径，只填域名 |

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

**Windows 的防火墙可能拦 ICMP**——用 TCP 测更可靠：

```bash
# 对端开服务
python3 -m http.server 8080

# 本机测
curl -v --max-time 5 http://10.64.0.3:8080/
```

### Q: ping Termux 不通？

**Termux 端支持 ICMP 响应**——通过内置 `packet_forwarder.go` 处理 Echo Request。

**如果 ping 不通**：

**情况 1：单向 P2P（最常见）**

日志里会看到 **每秒一次** 的 `[FWD-ICMP] Echo Request from 10.64.0.2`（PC 一直重发）。

**原因**：CGNAT 打洞单向——PC 的包能到 Termux，但 Termux 的包被 PC 侧 CGNAT 丢弃。

**修复**：Termux 端最新版已将 **ICMP 强制走 WS 中继**（牺牲延迟换可靠）。RTT 会变成 100-300ms，但**一定能通**。

**情况 2：Termux 客户端未运行**

检查 Termux 端：

```bash
ps aux | grep n2n-termux
tail -f ~/n2n.log | grep FWD
```

应该有 `[FWD] 已启动（TCP 中继 + UDP 转发 + ICMP 响应）`。

### Q: ping Android 不通？

**Android 的 TUN 对 ICMP 支持完整**——如果 ping 不通，可能是：

1. **打洞失败** → 服务端面板显示 `TURN-BD` 或 `ws-BD`
2. **Android 端 App 未启动** → 检查通知栏图标
3. **路由问题** → `adb shell ip route` 检查是否有 `10.64.0.0/24`

### Q: Termux 能 ping 通 PC 吗？

**不能**。**Termux 端 SOCKS5 只代理 TCP**，不支持 ICMP。

**用 TCP 测**：

```bash
# PC 端起服务
python3 -m http.server 8080 --bind 0.0.0.0

# Termux 端测
curl --socks5 127.0.0.1:1080 http://10.64.0.2:8080/
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

### Q: 打洞失败？

看日志 `[NAT-HOLE] ❌ 失败`。会自动降级到 TURN 中继（如果配置了）。TURN 也不可用则降级到 WS 中继。

**排查**：

1. **看 LAN 候选有没有命中**
   - `[P2P] 从 xxx (192.168.x.x) 收到 Punch` → LAN 命中
   - `[P2P] 从 xxx (x.x.x.x) 收到 Punch` → 公网命中
2. **看两端 pubSocket 端口差**
   - 差距小（< 10）→ 保活生效
   - 差距大（> 100）→ CGNAT 映射漂移严重
3. **看是否同 STUN 出口 IP**
   - 相同 → 同 CGNAT，需要 hairpin（多半失败）
   - 不同 → 正常跨 NAT，大概率成功

### Q: 客户端每 15 秒收到一次 `nat_hole_instruction`？

**已 P2P 但没告诉服务端**。

**修复**：最新版代码在跳过指令时**主动上报** `p2p_status=3`：

```
[NAT-HOLE] 跳过指令 target=xxx（已 P2P），上报 p2p_status=3    ← 只出现一次
```

如果日志里每 15 秒出现一次 `跳过指令`，说明客户端版本太旧，需要升级。

### Q: 同 WiFi 延迟波动很大（100ms+）？

**已修复**（v1.3.0+）。原因是两端同时向 LAN + 公网发包，导致对端 `peer.UDPAddr` 被公网地址覆盖，回程走 CGNAT 回环。

**修复后**：

- 新建 peer 时优先用 LAN 初始化 `UDPAddr`
- 收到包更新 `UDPAddr` 时 **LAN 优先**，30 秒窗口
- 发送数据时 **LAN 优先**，只失败才回退

现在同 WiFi 场景延迟稳定在 **8~15ms**，标准差 <2ms。

### Q: 如何确认二进制是 PC 版而不是 Termux 版？

**方法 1：看启动日志**

| 日志 | 判断 |
|:---|:---|
| `[TUN] n2n0 已启动` | ✅ PC 版 |
| `[SOCKS5] 监听 127.0.0.1:1080` + `[FWD] 已启动` | ❌ Termux 版（跑错平台了） |

**方法 2：看文件名**

```bash
ls -lh ~/.n2n-go/n2n-go-client     # ✅ PC 版（install.sh 装的）
ls -lh ~/n2n-go-client             # ✅ PC 版（手动下载的）
ls -lh ~/.n2n-termux/n2n-termux   # Termux 版
```

**方法 3：strings 检查**

```bash
strings ~/.n2n-go/n2n-go-client | grep -i "wintun\|n2n0" | head -3
```

### Q: 一直显示 `natType=EasyNAT`？

**旧版客户端的 bug**——单样本时判 EasyNAT。

**最新版**：单样本 → `unknown`，不猜。

### Q: 重启后虚拟 IP 变了？

客户端会持久化 `CLIENT_ID` 到 `$INSTALL_DIR/client_id`，正常情况下重启 IP 不变。
如果 IP 变了，说明 `client_id` 文件被删或换目录了，检查：

```bash
cat ~/.n2n-go/client_id
```

### Q: WebSocket 断线后需要手动重启吗？

不需要。客户端内置**自动重连**（指数退避，最长 5 分钟）+ **90 秒读超时检测下行僵死**。断线期间日志会显示：

```
[WS] 断线: ..., 1s 后重连
[WS] 重连成功
```

**下行僵死**（TCP 表面正常但服务端消息到达不了）也会被检测：

```
[WS] 读超时 1m30s 未收到任何消息（下行僵死），触发重连
```

重连后会自动重新上报 NAT 元数据。

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
| **n2n-go-client** | Linux / macOS / Windows | **TUN** | ✅ 全部 |
| [n2n-termux](https://github.com/goyo123321/n2n-termux) | Android Termux | **SOCKS5 + 包转发** | ✅ 全部 |
| [n2n-android](https://github.com/goyo123321/n2n-android) | Android APK | VpnService | ✅ 全部 |
