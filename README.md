# n2n-go-client

跨平台 P2P VPN 客户端，配合 [edge-signal](https://github.com/goyo123321/edge-signal) 使用。

基于 TUN 虚拟网卡，通过 Cloudflare Workers 信令服务器建立 **LAN 直连 / P2P / TURN / WebSocket 中继** 四级降级连接，实现异地组网。

**与 [n2n-termux](https://github.com/goyo123321/n2n-termux) 完全互通**——PC 客户端通过 TUN 直接访问 Termux 上的服务，Termux 通过 SOCKS5 访问 PC 上的服务。

## ✨ 特性

- **跨平台** — Linux / macOS / Windows 全支持
- **TUN 虚拟网卡** — 内核原生 TCP/IP 栈，性能高
- **四级降级** — `LAN → P2P → TURN → WS`，连接永远可用
- **LAN 直连** — 同 WiFi 场景自动发现 LAN 地址，绕过 NAT 直接内网通信
- **P2P 直连** — STUN 探测 + NAT 打洞，延迟低至 5ms
- **多目标打洞** — LAN + STUN 反射 + 观测 raddr 同时扫描，命中率大幅提升
- **CGNAT hairpin** — 同 STUN 出口 IP 时尝试端口扫描 + probe 回发
- **UDP 保活** — 每 5 秒刷新 STUN 映射，防止 CGNAT 端口漂移
- **P2P 通道保活** — 每 15 秒往对端发 N2NS PING 多目标探针，防止空闲时通道被回收
- **TURN 中继** — 支持 Cloudflare TURN + 自定义 TURN，不消耗 Worker 配额
- **中继回退** — TURN 失败自动降级到 WebSocket 中继
- **断线重连** — WebSocket 自动重连（指数退避）+ 90 秒读超时检测下行僵死
- **连接密码** — 支持 `CONNECT_TOKEN` 保护 Worker
- **ID 持久化** — 未指定 `CLIENT_ID` 时自动生成并保存，重启复用
- **虚拟网段可配** — 服务端通过 `VIRTUAL_NETWORK` 下发网段，客户端自动适配
- **无依赖** — Windows 版内嵌 wintun.dll
- **三端互通** — 与 [n2n-termux](https://github.com/goyo123321/n2n-termux)、[n2n-android](https://github.com/goyo123321/n2n-android) 无缝互通

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

## 🌐 三端互通拓扑

```
              edge-signal (Cloudflare Worker)
                    │
       ┌────────────┼────────────┐
       │            │            │
       ▼            ▼            ▼
  n2n-go-client  n2n-termux  n2n-android
   (TUN 模式)    (SOCKS5 模式)  (VpnService)
   Linux/macOS   Android       Android
   /Windows      Termux        APK
       │            │            │
       └────────────┴────────────┘
              P2P / TURN / WS
               完全互通
```

**场景示例**：

| 组合 | 访问方式 | 说明 |
|:---|:---|:---|
| **PC → PC** | `curl http://10.64.0.X:8080/` | 双端 TUN，直接走虚拟网 |
| **PC → Termux** | `curl http://10.64.0.X:8080/` | PC 用 TUN，Termux 端服务监听 `0.0.0.0` |
| **Termux → PC** | `curl --socks5 127.0.0.1:1080 http://10.64.0.X:8080/` | Termux 用 SOCKS5，PC 端服务监听 `0.0.0.0` |
| **Termux → Termux** | `curl --socks5 127.0.0.1:1080 http://10.64.0.X:8080/` | 双端 SOCKS5 |

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

## 🚀 快速开始

一键脚本，支持 Linux / macOS / Termux(proot)：

```bash
bash <(curl -fsSL https://raw.githubusercontent.com/goyo123321/n2n-go-client/main/install.sh)
```

或：

```bash
curl -fsSL https://raw.githubusercontent.com/goyo123321/n2n-go-client/main/install.sh | bash
```

> 部署脚本执行完后默认**自动启动客户端**，直接回车即可。

### 1. 下载

从 [Releases](https://github.com/goyo123321/n2n-go-client/releases) 下载对应平台：

| 平台 | 文件 |
|:---|:---|
| Linux x64 | `n2n-client-linux-amd64` |
| Linux ARM64 | `n2n-client-linux-arm64` |
| macOS Intel | `n2n-client-darwin-amd64` |
| macOS Apple Silicon | `n2n-client-darwin-arm64` |
| Windows x64 | `n2n-client-windows-amd64.exe` |

### 2. 启动

#### Linux

```bash
chmod +x n2n-client-linux-amd64

sudo SIGNALING_URL="wss://edge-signal.xxx.workers.dev" \
     ROOM_ID="myroom" \
     CLIENT_ID="pc-a" \
     NODE_NAME="A的电脑" \
     ./n2n-client-linux-amd64
```

#### macOS

```bash
chmod +x n2n-client-darwin-arm64
xattr -d com.apple.quarantine ./n2n-client-darwin-arm64

sudo SIGNALING_URL="wss://edge-signal.xxx.workers.dev" \
     ROOM_ID="myroom" \
     CLIENT_ID="mac-a" \
     NODE_NAME="A的Mac" \
     TUN_NAME="utun9" \
     ./n2n-client-darwin-arm64
```

#### Windows（管理员 PowerShell）

```powershell
$env:SIGNALING_URL="wss://edge-signal.xxx.workers.dev"
$env:ROOM_ID="myroom"
$env:CLIENT_ID="win-a"
$env:NODE_NAME="A的PC"

.\n2n-client-windows-amd64.exe
```

## 🔗 与 Termux 互通

### PC 访问 Termux 上的服务

**Termux 端起服务**：

```bash
# Termux-A
python3 -m http.server 8080
# 或
sshd    # SSH 服务监听 8022
```

**PC 端直接访问**（走 TUN）：

```bash
# 通过虚拟 IP
curl http://10.64.0.2:8080/
ssh -p 8022 u0_aXXX@10.64.0.2
```

**前提**：服务必须监听 `0.0.0.0` 或对应接口（不能只听 `127.0.0.1`）。

```bash
# ✅ 可以
python3 -m http.server 8080 --bind 0.0.0.0

# ❌ 不行（只监听 loopback）
python3 -m http.server 8080 --bind 127.0.0.1
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
curl --socks5 127.0.0.1:1080 http://10.64.0.4:8080/
ssh -o ProxyCommand='nc -X 5 -x 127.0.0.1:1080 %h %p' user@10.64.0.4
```

### 数据面差异

| 维度 | n2n-go-client | n2n-termux |
|:---|:---|:---|
| 数据面 | TUN 虚拟网卡（内核 TCP/IP 栈） | SOCKS5 + `N2NS` 帧 |
| 应用透明 | ✅ 系统级路由，无需配置 | ⚠️ 需要应用显式配 SOCKS5 |
| 协议支持 | TCP / UDP / ICMP | 仅 TCP |
| 权限要求 | root / 管理员 | 无 root |
| 使用方式 | `curl http://10.64.0.X` | `curl --socks5 127.0.0.1:1080 http://10.64.0.X` |

**关键**：**两端的数据面协议完全独立**——PC 端 TUN 里发出的 IPv4 包会被 n2n-go-client 打包走 P2P/TURN/WS；Termux 端的 SOCKS5 连接会被 n2n-termux 打包走 P2P/TURN/WS。**它们在服务端和 P2P 通道层用同一套协议**（N2NS 帧 + 打洞 + TURN 中继），所以完全互通。

### 打洞行为

**PC ↔ Termux 打洞**与 **Termux ↔ Termux 打洞**行为一致：

- 双端都上报 `lanEndpoints` → LAN 直连优先
- 双端都上报 `assistedEndpoints` → STUN 反射多端点
- 打洞指令由同一个 `coordinator.js` 生成
- 状态机（`P2PAvailable` → `P2P`）完全相同

**唯一区别**：PC 端还有 `peer.UDPAddr` 直发路径（TUN 里发包到虚拟 IP），Termux 端只能通过 SOCKS5 触发。

## ⚙️ 环境变量

| 变量 | 必填 | 默认值 | 说明 |
|:---|:---|:---|:---|
| `SIGNALING_URL` | ✅ | — | edge-signal 的 WSS 地址 |
| `CONNECT_TOKEN` | ⚠️ | `""` | 连接密码（Worker 配了才需要）|
| `CLIENT_ID` | ❌ | 自动生成并持久化 | 客户端唯一 ID。未设置时首次启动生成 `hostname-随机数`，保存在 `$INSTALL_DIR/client_id`，重启复用 |
| `ROOM_ID` | ❌ | `default-room` | 房间名。只允许 `[A-Za-z0-9_-]+` |
| `NODE_NAME` | ❌ | hostname | 显示名 |
| `TUN_NAME` | ❌ | `n2n0` | 虚拟网卡名（macOS 用 `utun9`）|
| `UDP_PORT` | ❌ | `0` | P2P UDP 端口。`0`=内核自动分配（推荐），指定端口被占用时自动回退 |
| `STUN_SERVERS` | ❌ | Google + Cloudflare | STUN 服务器 |
| `SOCKS5_LISTEN` | ❌ | `127.0.0.1:1080` | SOCKS5 监听地址（额外入口，可选）|

**虚拟网段**由服务端 `wrangler.toml` 的 `VIRTUAL_NETWORK` 决定，客户端从 `ready` 消息自动接收，无需配置。默认 `10.64.0.0/24`。

## 📋 启动日志

启动成功后：

```
n2n-go-client v1.2.0 启动
[配置] 未设置 CONNECT_TOKEN
[配置] 生成并持久化 CLIENT_ID: pc-a-1704067200 -> /home/user/.n2n-go/client_id
[P2P] UDP 监听端口 54321
[NAT] 启动预收集 LAN: [192.168.1.100:54321]
[NAT] STUN 74.125.250.129:19302 → 120.239.134.13:19190
[NAT] HardNAT（对称），ports_diff=0 pub=120.239.134.13:19190
[WS] 连接 wss://edge-signal.xxx.workers.dev/ws/myroom?cid=pc-a
已连接信令，Client ID: pc-a-1704067200，节点名: A的电脑
[WS] 重放 1 条早期文本消息
分配虚拟 IP: 10.64.0.2（网段 10.64.0.0/24）
[信令] 服务端看到的本机出口 IP: 120.229.199.61（WS/TCP 出口，仅参考）
[NAT] 上报 LAN 地址: [192.168.1.100:54321]
[信令] 上报 p2p_metadata: natType=HardNAT publicEndpoint="120.239.134.13:19190" multiExit=true assisted=2 lan=1
[TUN] n2n0 已启动，IP: 10.64.0.2/24，网段: 10.64.0.0/24
[TURN] 获取到 custom TURN 服务器: turn:xxx:3478
[TURN] 中继地址: 5.6.7.8:50000
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

当其他节点上线时（**LAN 直连命中**）：

```
========== 节点就绪: termux-b ==========
  虚拟 IP   : 10.64.0.3
  公网地址  : 1.2.3.4:54322
  LAN 地址  : [192.168.1.101:54322]
  STUN 端点 : [1.2.3.4:54322 1.2.3.4:54335]
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
[NAT-HOLE] ❌ 失败 role=0 target=1.2.3.4 attempts=110
[连接] termux-b → TURN 中继 (5.6.7.8:50000)
```

**TURN 也不可用时**：

```
[连接] termux-b → WS 中继 (TURN 未就绪，最后兜底)
```

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

### 多目标发送

`sendP2PFrame` 走 `collectN2NSTargets` 收集所有候选：

1. LAN 地址（同 WiFi 优先）
2. AssistedEndpoints（STUN 反射多端点）
3. lastRecvFrom（对端最近发来的源地址）
4. peerAddr（注册分配）
5. peer.UDPAddr（观测 raddr）

**一帧同时发往所有候选**，命中率大幅提升。

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

### 场景 3：PC + Termux 混合组网

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
curl --socks5 127.0.0.1:1080 http://10.64.0.2:8080/   # Termux 走 SOCKS5
ssh -o ProxyCommand='nc -X 5 -x 127.0.0.1:1080 %h %p' user@10.64.0.2
```

### 场景 4：游戏联机

```bash
# 玩家 A（PC）
CLIENT_ID="player-a" ROOM_ID="game-night" ...

# 玩家 B（手机 + Termux）
CLIENT_ID="termux-b" ROOM_ID="game-night" ...
```

**注意**：Termux 端只支持 SOCKS5（TCP），不支持游戏常见的 UDP 数据面。PC ↔ PC 才能玩 UDP 游戏。

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
go build -o n2n-client .

# 交叉编译
GOOS=linux   GOARCH=amd64 CGO_ENABLED=0 go build -o n2n-client-linux-amd64 .
GOOS=darwin  GOARCH=arm64 CGO_ENABLED=0 go build -o n2n-client-darwin-arm64 .
GOOS=windows GOARCH=amd64 CGO_ENABLED=0 go build -o n2n-client-windows-amd64.exe .
```

**Windows 构建特殊说明**：需要先下载 `wintun.dll`：

```bash
WINTUN_VERSION=0.14.1
curl -L -o wintun.zip "https://www.wintun.net/builds/wintun-${WINTUN_VERSION}.zip"
unzip -q wintun.zip -d wintun-extract
cp wintun-extract/wintun/bin/amd64/wintun.dll ./wintun.dll

GOOS=windows GOARCH=amd64 CGO_ENABLED=0 go build -o n2n-client-windows-amd64.exe .
```

### GitHub Actions 自动构建

手动触发：

```
Actions → Build n2n-go Client → Run workflow
  version: v1.2.0
  publish: ✓
  platforms: （留空=全部5个平台）
```

## 📁 项目结构

```
n2n-go-client/
├── main.go                       # 主程序（含 UDP 保活 + P2P 通道保活 + LAN 收集）
├── ws_transport.go               # WebSocket 传输层（断线重连 + 90s 读超时 + 早期消息缓冲）
├── relay_fallback.go             # 四级降级管理器（含 ForceFallback）
├── turn_client.go                # TURN 客户端
├── turn_lite.go                  # TURN-Lite 实现（UDP + TCP）
├── turn_lite_tcp.go              # TURN over TCP / RFC 6062
├── nat_probe.go                  # NAT 类型探测
├── nathole_executor.go           # 打洞指令执行（分阶段扫描）
├── p2p_stream.go                 # N2NS 可靠流协议
├── p2p_support.go                # P2P stream 依赖方法
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
UDP_PORT=50001 ./n2n-client ...
```

指定的端口被占用时，客户端会**自动回退**到内核分配，不会启动失败。

### 虚拟网段

网段由**服务端** `wrangler.toml` 的 `VIRTUAL_NETWORK` 决定，客户端在连接时从 `ready` 消息接收。

**更换网段需要同时重启所有客户端**——旧的 IP 和路由是旧网段的，连接后会被新网段覆盖。

### Termux / Android 限制

Android 内核**不允许普通 App 创建 TUN 网卡**。Termux 里运行**本客户端**：

- ✅ 能连上 Worker
- ✅ 能拿到虚拟 IP
- ✅ 面板显示为 peer
- ❌ **无法创建 TUN**（需要 root）

**组网测试请用电脑**（Linux/macOS/Windows）。或者用专门的 [n2n-termux](https://github.com/goyo123321/n2n-termux) 客户端（SOCKS5 模式，无 root）。

## 📊 性能参考

| 连接方式 | 延迟 | 消耗 |
|:---|:---|:---|
| **LAN 直连** | **<5ms** | **无** |
| P2P 直连（跨 NAT） | 10~50ms | 无 |
| **CGNAT hairpin** | **10~30ms** | **无** |
| TURN 中继 | 30~100ms | Cloudflare TURN 配额 |
| WebSocket 中继 | 100~300ms | Worker 请求配额 |

## 🐛 常见问题

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

### Q: PC 能 ping 通 Termux 吗？

**Termux 端不支持 TUN，无法响应 ICMP**。所以 PC 端 `ping 10.64.0.X` **一定失败**。

**要测 PC → Termux 连通性**，用 TCP：

```bash
# Termux-A 端起服务
python3 -m http.server 8080 --bind 0.0.0.0

# PC 端测
curl http://10.64.0.2:8080/
```

### Q: Termux 能 ping 通 PC 吗？

**Termux 端 SOCKS5 只代理 TCP**，`ping`（ICMP）和 `curl http://` 走的是不同协议栈。

```bash
# ❌ 不行（ICMP 不走 SOCKS5）
ping 10.64.0.4

# ✅ 可以（TCP 走 SOCKS5）
curl --socks5 127.0.0.1:1080 http://10.64.0.4:8080/
```

### Q: 如何判断走的是 LAN / P2P / TURN / WS？

**看日志**：

```
[P2P] 从 termux-b (192.168.1.101) 收到 Punch      ← LAN
[P2P] 从 termux-b (1.2.3.4) 收到 Punch             ← 公网 P2P
[连接] termux-b → TURN 中继 (5.6.7.8:50000)        ← TURN
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
   - `[P2P] 从 xxx (1.2.3.4) 收到 Punch` → 公网命中
2. **看两端 pubSocket 端口差**
   - 差距小（< 10）→ 保活生效
   - 差距大（> 100）→ CGNAT 映射漂移严重
3. **看是否同 STUN 出口 IP**
   - 相同 → 同 CGNAT，需要 hairpin（多半失败）
   - 不同 → 正常跨 NAT，大概率成功

### Q: LAN 地址收集不到？

**检查启动日志**：

```
[NAT] 启动预收集 LAN: [...]   ← 有这行说明能收集
[NAT] 启动预收集 LAN 为空（可能蜂窝网络，或 WiFi 未就绪）  ← 收集失败
```

**常见原因**：

1. **纯蜂窝网络** —— 没有 LAN 接口，正常现象
2. **蜂窝启动后切 WiFi** —— 后台重试补齐
3. **默认路由不在 WiFi 上** —— 检查 `detectOutboundIP` 输出

**手工验证**（在目标设备上）：

```bash
# Linux
ip route get 8.8.8.8 | head -1

# macOS
route -n get 8.8.8.8 | grep interface

# Python 跨平台
python3 -c "import socket; s=socket.socket(socket.AF_INET,socket.SOCK_DGRAM); s.connect(('8.8.8.8',80)); print(s.getsockname()[0])"
```

- 输出 `192.168.x.x` → Go 也能拿到 LAN
- 输出 `10.x` / `100.64.x` → 默认路由不在 WiFi

### Q: P2P 建立后过一段时间断了？

**CGNAT 映射或会话被回收**。检查：

1. **日志里 `[P2P-Keepalive] 启动` 有没有？**
   - 没有 → 二进制太旧
2. **端口是不是又漂移了？**
   - `[信令] 上报 p2p_metadata` 的 `publicEndpoint` 变了 → 保活无效
   - STUN 服务器被封 → 换国内 STUN
3. **P2P 通道保活生效了吗？**
   - 每 15 秒会往对端发 1 个 N2NS PING 多目标探针

### Q: `[WS] 重放 1 条早期文本消息` 是什么意思？

**正常信息**——`ready` 消息在 handler 设置前到达，被缓冲后重放。

**旧版会丢这条消息**，导致 `virtualIP` 和 `serverSeenIP` 拿不到。

### Q: 一直显示 `natType=EasyNAT`？

**旧版客户端的 bug**——单样本时判 EasyNAT。

**最新版**：单样本 → `unknown`，不猜。

### Q: `[信令] 服务端看到的本机出口 IP` 和 `[NAT] STUN` 结果不一致？

**CGNAT 池化**——运营商有多个公网出口，TCP 和 UDP 从不同出口出去。

**这是网络限制，不是 bug**。客户端会自动按 LAN → hairpin → TURN → WS 降级。

**保活生效后**，两端 UDP 出口端口保持稳定，hairpin 成功率显著提升。

### Q: TURN 初始化失败？

```
[TURN] 初始化失败: allocate: ...
```

原因：
- TURN 服务器不可达
- TURN 凭证错误
- TURN 服务器额度用尽

**客户端会自动降级到 WS 中继**，不影响使用。

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

### Q: 面板显示 `p2p-avail` 不是 `p2p`？

**`p2p-avail`** = 打洞握手成功，但 PONG 或真实数据帧还没到，尚未确认双向通道。

**正常情况 1-2 秒内**会升级到 `p2p`：

```
[P2P] 从 termux-b 收到 Punch → P2PAvailable（等待数据帧验证）
[P2P] 收到 PONG → P2P 直连（双向通道已验证）       ← 这行出现就升 P2P
```

**如果一直停在 `p2p-avail`**：30 秒后会超时降级到 TURN/WS。

## 🔗 与 edge-signal 配合

| 项 | edge-signal | n2n-go-client |
|:---|:---|:---|
| 部署位置 | Cloudflare 边缘 | 用户设备 |
| 角色 | 信令服务器 + TURN 凭证下发 + 中继兜底 | 建立 TUN + LAN/P2P/TURN 连接 |
| 语言 | TypeScript | Go |
| 存储 | Durable Objects | 本地 |

**先部署 edge-signal，再运行客户端。**

## 📄 License

MIT

## 🔗 相关项目

| 项目 | 平台 | 模式 | 互通 |
|:---|:---|:---|:---|
| [edge-signal](https://github.com/goyo123321/edge-signal) | Cloudflare Workers | 信令服务器 | — |
| **n2n-go-client** | Linux / macOS / Windows | **TUN** | ✅ 全部 |
| [n2n-termux](https://github.com/goyo123321/n2n-termux) | Android Termux | **SOCKS5** | ✅ 全部 |
| [n2n-android](https://github.com/goyo123321/n2n-android) | Android APK | VpnService | ✅ 全部 |

**三端互通**：n2n-termux ↔ n2n-go-client ↔ n2n-android 可以互相访问。
