# n2n-go-client

跨平台 P2P VPN 客户端，配合 [edge-signal](https://github.com/goyo123321/edge-signal) 使用。

基于 TUN 虚拟网卡，通过 Cloudflare Workers 信令服务器建立 **P2P 直连 / TURN 中继 / WebSocket 中继** 三级降级连接，实现异地组网。

## ✨ 特性

- **跨平台** — Linux / macOS / Windows 全支持
- **TUN 虚拟网卡** — 内核原生 TCP/IP 栈，性能高
- **三级降级** — `P2P → TURN → WS`，连接永远可用
- **P2P 直连** — STUN 探测 + NAT 打洞，延迟低至 5ms
- **TURN 中继** — 支持 Cloudflare TURN + 自定义 TURN，不消耗 Worker 配额
- **中继回退** — TURN 失败自动降级到 WebSocket 中继
- **断线重连** — WebSocket 自动重连（指数退避）
- **连接密码** — 支持 `CONNECT_TOKEN` 保护 Worker
- **ID 持久化** — 未指定 `CLIENT_ID` 时自动生成并保存，重启复用
- **虚拟网段可配** — 服务端通过 `VIRTUAL_NETWORK` 下发网段，客户端自动适配
- **无依赖** — Windows 版内嵌 wintun.dll

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

## 📡 三级降级链路

```
优先级 1: P2P 直连        ← 最优，延迟最低
   ↓ 打洞失败
优先级 2: TURN 中继       ← 次优，专业 UDP 中继，不消耗 Worker
   ↓ TURN 不可用/失败
优先级 3: WS 中继         ← 最后兜底，走 Worker，消耗配额
```

**每对 peer 独立决策**：A↔B 可能走 P2P，A↔C 走 TURN，A↔D 走 WS，互不影响。

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

**虚拟网段**由服务端 `wrangler.toml` 的 `VIRTUAL_NETWORK` 决定，客户端从 `ready` 消息自动接收，无需配置。

## 📋 启动日志

启动成功后：

```
n2n-go-client v1.2.0 启动
[配置] 未设置 CONNECT_TOKEN
[配置] 生成并持久化 CLIENT_ID: pc-a-1704067200 -> /home/user/.n2n-go/client_id
[P2P] UDP 监听端口 54321
[NAT] EasyNAT（端口保持），pub=1.2.3.4:54321
已连接信令，Client ID: pc-a-1704067200，节点名: A的电脑
分配虚拟 IP: 10.64.0.2（网段 10.64.0.0/24）
[TUN] n2n0 已启动，IP: 10.64.0.2/24，网段: 10.64.0.0/24
[TURN] 获取到 custom TURN 服务器: turn:xxx:3478
[TURN] 中继地址: 5.6.7.8:50000

================= 本机信息 =================
  Client ID   : pc-a-1704067200
  节点名       : A的电脑
  虚拟 IP     : 10.64.0.2
  虚拟网段    : 10.64.0.0/24
  P2P 端口    : 54321
==========================================
```

当其他节点上线时：

```
========== 节点就绪: mac-b ==========
  虚拟 IP   : 10.64.0.3
  公网地址  : 1.2.3.4:54322
==========================================

[NAT-HOLE] 开始打洞 role=0 target=1.2.3.4:54322 rung=0 mode=0 ttl=7 assisted=0
[NAT-HOLE] ✅ 成功 role=0 target=1.2.3.4 attempts=5
[连接] mac-b → P2P 直连
```

**打洞失败时**：

```
[NAT-HOLE] ❌ 失败 role=0 target=1.2.3.4 attempts=25
[连接] mac-b → WS 中继 (TURN 未就绪，最后兜底)
```

**TURN 稍后就绪时，自动升级**：

```
[TURN] TURN 中继就绪: 5.6.7.8:50000
[连接] mac-b → TURN 中继 (延迟升级, 5.6.7.8:50000)
```

**TURN 也不可用时**：

```
[连接] mac-b → WS 中继 (最后兜底)
```

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

### 场景 3：远程开发机

```bash
# 远程开发机
CLIENT_ID="dev-server" ...

# 本地
CLIENT_ID="local" ...
```

`ssh user@10.64.0.2` 直接连上。

### 场景 4：游戏联机

```bash
# 玩家 A
CLIENT_ID="player-a" ROOM_ID="game-night" ...

# 玩家 B
CLIENT_ID="player-b" ROOM_ID="game-night" ...
```

局域网游戏通过虚拟 IP 直连，延迟远低于公网。

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
├── main.go                       # 主程序
├── ws_transport.go               # WebSocket 传输层（含断线重连）
├── relay_fallback.go             # 三级降级管理器
├── turn_client.go                # TURN 客户端
├── nat_probe.go                  # NAT 类型探测
├── nathole_executor.go           # 打洞指令执行（含 TTL 语义）
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

Android 内核**不允许普通 App 创建 TUN 网卡**。Termux 里运行客户端：

- ✅ 能连上 Worker
- ✅ 能拿到虚拟 IP
- ✅ 面板显示为 peer
- ❌ **无法创建 TUN**（需要 root）
- ❌ 无法实际组网

**组网测试请用电脑**（Linux/macOS/Windows）。

## 📊 性能参考

| 连接方式 | 延迟 | 消耗 |
|:---|:---|:---|
| 同局域网 | < 5ms | 无 |
| P2P 直连（跨 NAT） | 10~50ms | 无 |
| **TURN 中继** | **30~100ms** | **Cloudflare TURN 配额** |
| WebSocket 中继 | 100~300ms | Worker 请求配额 |

## 🐛 常见问题

**Q: ping 不通怎么办？**

1. 检查 TUN 接口：`ip addr show n2n0`（Linux）/ `ifconfig utun9`（macOS）
2. 检查路由：`ip route | grep <网段前缀>`
3. 检查日志：有没有 `[TUN] 启动失败`
4. 查看 Worker 面板：`/api/public/status/<room>`

**Q: 打洞失败？**

看日志 `[NAT-HOLE] ❌ 失败`。会自动降级到 TURN 中继（如果配置了）。TURN 也不可用则降级到 WS 中继。

如果频繁失败，检查：
- 两侧是否都是 HardNAT（对称 NAT）——成功率天然较低
- 服务端日志里是否有 `rung 0 → rung 4` 的梯级切换
- `tcpdump` 抓包看探测包的 IP TTL 是否按梯级设置

**Q: TURN 初始化失败？**

```
[TURN] 初始化失败: allocate: ...
```

原因：
- TURN 服务器不可达
- TURN 凭证错误
- TURN 服务器额度用尽

**客户端会自动降级到 WS 中继**，不影响使用。

**Q: 重启后虚拟 IP 变了？**

客户端会持久化 `CLIENT_ID` 到 `$INSTALL_DIR/client_id`，正常情况下重启 IP 不变。
如果 IP 变了，说明 `client_id` 文件被删或换目录了，检查：

```bash
cat ~/.n2n-go/client_id
```

**Q: WebSocket 断线后需要手动重启吗？**

不需要。客户端内置**自动重连**（指数退避，最长 30s）。断线期间日志会显示：

```
[WS] 断线: ..., 1s 后重连
[WS] 重连成功
```

重连后会自动重新上报 NAT 元数据。

## 🔗 与 edge-signal 配合

| 项 | edge-signal | n2n-go-client |
|:---|:---|:---|
| 部署位置 | Cloudflare 边缘 | 用户设备 |
| 角色 | 信令服务器 + TURN 凭证下发 + 中继兜底 | 建立 TUN + P2P/TURN 连接 |
| 语言 | TypeScript | Go |
| 存储 | Durable Objects | 本地 |

**先部署 edge-signal，再运行客户端。**

## 📄 License

MIT

## 🔗 相关项目

- [edge-signal](https://github.com/goyo123321/edge-signal) — Cloudflare Workers 控制面
