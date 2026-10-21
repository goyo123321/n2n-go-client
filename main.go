package main

import (
	"bufio"
	"context"
	"crypto/rand"
	"encoding/binary"
	"encoding/json"
	"errors"
	"fmt"
	"log"
	"net"
	"os"
	"os/signal"
	"path/filepath"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"
	"syscall"
	"time"
)

var (
	BuildVersion        = "dev"
	DefaultSignalingURL = ""
)

const DefaultVirtualCIDR = "10.64.0.0/24"

// ============================================================
// ★ 方案 C：仅 Termux 环境启用 DNS/TLS 修复
//
// 从 n2n-termux 移植：
//   - initTermuxCA：自动探测 Termux 的 CA 证书，设置 SSL_CERT_FILE
//   - setupResolver：自动过滤本地回环 DNS，fallback 公网 DNS
//
// PC / macOS / Windows 通过 isTermux() 返回 false，
// 这两个函数都不会执行，100% 零影响。
// ============================================================

func init() {
	if isTermux() {
		initTermuxCA()
	}
}

// isTermux 判断是否在 Termux 环境
func isTermux() bool {
	prefix := os.Getenv("PREFIX")
	if prefix == "" {
		prefix = os.Getenv("TERMUX_PREFIX")
	}
	return prefix != "" && strings.Contains(prefix, "com.termux")
}

func initTermuxCA() {
	if os.Getenv("SSL_CERT_FILE") != "" {
		return
	}

	prefix := os.Getenv("PREFIX")
	if prefix == "" {
		prefix = os.Getenv("TERMUX_PREFIX")
	}
	if prefix == "" {
		prefix = "/data/data/com.termux/files/usr"
	}

	candidates := []string{
		prefix + "/etc/tls/cert.pem",
		prefix + "/etc/ca-certificates/ca-bundle.crt",
		prefix + "/etc/ssl/certs/ca-certificates.crt",
		prefix + "/etc/ssl/cert.pem",
		"/data/data/com.termux/files/usr/etc/tls/cert.pem",
		"/data/data/com.termux/files/usr/etc/ssl/certs/ca-certificates.crt",
	}

	for _, p := range candidates {
		if st, err := os.Stat(p); err == nil && !st.IsDir() && st.Size() > 0 {
			_ = os.Setenv("SSL_CERT_FILE", p)
			fmt.Fprintf(os.Stderr, "[TLS] CA 证书: %s (%d bytes)\n", p, st.Size())
			return
		}
	}

	fmt.Fprintf(os.Stderr, "[TLS] ⚠️ 未找到 CA 证书，HTTPS/WSS 可能失败\n")
	fmt.Fprintf(os.Stderr, "[TLS]    修复: pkg install ca-certificates\n")
}

// ============================================================
// LAN 缓存
// ============================================================

var (
	lanCacheMu   sync.Mutex
	lanCache     []string
	lanCacheTime time.Time
)

func saveLANCache(lans []string) {
	if len(lans) == 0 {
		return
	}
	lanCacheMu.Lock()
	lanCache = append([]string(nil), lans...)
	lanCacheTime = time.Now()
	lanCacheMu.Unlock()
}

func loadLANCache() []string {
	lanCacheMu.Lock()
	defer lanCacheMu.Unlock()
	if len(lanCache) == 0 || time.Since(lanCacheTime) > 5*time.Minute {
		return nil
	}
	return append([]string(nil), lanCache...)
}

// ============================================================
// 数据结构
// ============================================================

type PeerInfo struct {
	ClientID          string
	VirtualIP         string
	PubIP             string
	PubPort           int
	NATType           string
	TurnRelayAddr     string
	UDPAddr           *net.UDPAddr
	lastRecvAt        int64
	lastLANRecvAt     int64
	loggedReady       bool
	hasRealData       bool
	probeReplied      bool
	AssistedEndpoints []string
	LanEndpoints      []string
	Features          []string
}

type Edge struct {
	clientId    string
	nodeName    string
	virtualIP   string
	virtualCIDR string
	roomId      string

	serverSeenIP string

	ws         *WSTransport
	relayMgr   *RelayManager
	turnClient *TURNClient

	tun atomic.Pointer[TUNDevice]

	udpConn *net.UDPConn
	udpPort int

	peers   map[string]*PeerInfo
	peersMu sync.RWMutex

	tunWriteCh chan []byte

	natMeta *NATMetadata

	fallbackTimers   map[string]*time.Timer
	fallbackTimersMu sync.Mutex

	p2pAvailTimers   map[string]*time.Timer
	p2pAvailTimersMu sync.Mutex

	lastNoPeerLog   map[string]int64
	lastNoPeerLogMu sync.Mutex

	tunStateLog   map[string]ConnType
	tunStateLogMu sync.Mutex

	lastMetadataReportAt int64

	lastDataFrameAt int64

	p2pStatusReported   map[string]bool
	p2pStatusReportedMu sync.Mutex

	streamMgr  *AppStreamManager
	p2pStreams *P2PStreamManager
	socks5Srv  *Socks5Server

	punchAckLimiter *punchAckLimiter

	doneCh  chan struct{}
	closeMu sync.Mutex
	closed  bool
	mu      sync.Mutex
}

func (e *Edge) Done() <-chan struct{} { return e.doneCh }

// ============ TUN 读错误分类 ============

func isTransientReadError(err error) bool {
	if err == nil {
		return false
	}
	if errors.Is(err, syscall.EAGAIN) || errors.Is(err, syscall.EWOULDBLOCK) {
		return true
	}
	if errors.Is(err, syscall.EINTR) {
		return true
	}
	if errors.Is(err, os.ErrDeadlineExceeded) {
		return true
	}
	s := err.Error()
	return strings.Contains(s, "resource temporarily unavailable") ||
		strings.Contains(s, "interrupted system call") ||
		strings.Contains(s, "try again")
}

// ============ UDP 保活 ============

const keepaliveInterval = 5 * time.Second

var keepaliveServers = []string{
	"74.125.250.129:19302",
	"74.125.204.127:19302",
	"162.159.207.0:3478",
	"stun.miwifi.com:3478",
	"stun.cloudflare.com:3478",
}

func buildSTUNBindingRequest() []byte {
	buf := make([]byte, 20)
	buf[0] = 0x00
	buf[1] = 0x01
	buf[2] = 0x00
	buf[3] = 0x00
	buf[4] = 0x21
	buf[5] = 0x12
	buf[6] = 0xA4
	buf[7] = 0x42
	if _, err := rand.Read(buf[8:20]); err != nil {
		binary.BigEndian.PutUint64(buf[8:16], uint64(time.Now().UnixNano()))
	}
	return buf
}

func (e *Edge) startKeepalive() {
	if e.udpConn == nil {
		return
	}

	var addrs []*net.UDPAddr
	for _, s := range keepaliveServers {
		if a, err := net.ResolveUDPAddr("udp4", s); err == nil {
			addrs = append(addrs, a)
		}
	}
	if len(addrs) == 0 {
		log.Printf("[Keepalive] ⚠️ 无可用 STUN 服务器，跳过保活")
		return
	}

	log.Printf("[Keepalive] 启动，每 %v 刷新 %d 个 STUN 服务器（自适应：有数据时跳过）",
		keepaliveInterval, len(addrs))

	safeGo("keepalive", func() {
		sendOnce := func() {
			last := atomic.LoadInt64(&e.lastDataFrameAt)
			if last > 0 && time.Now().UnixMilli()-last < 10000 {
				return
			}
			for _, addr := range addrs {
				probe := buildSTUNBindingRequest()
				_, _ = e.udpConn.WriteToUDP(probe, addr)
			}
		}

		sendOnce()

		ticker := time.NewTicker(keepaliveInterval)
		defer ticker.Stop()
		for {
			select {
			case <-e.doneCh:
				log.Printf("[Keepalive] 已停止")
				return
			case <-ticker.C:
				sendOnce()
			}
		}
	})
}

// ============ P2P 通道保活 + 活性检测 ============

const p2pKeepaliveInterval = 10 * time.Second
const p2pIdleDeadMs = 30000

func (e *Edge) startP2PKeepalive() {
	log.Printf("[P2P-Keepalive] 启动，每 %v 刷新 P2P 通道", p2pKeepaliveInterval)

	safeGo("p2p-keepalive", func() {
		ticker := time.NewTicker(p2pKeepaliveInterval)
		defer ticker.Stop()
		for {
			select {
			case <-e.doneCh:
				log.Printf("[P2P-Keepalive] 已停止")
				return
			case <-ticker.C:
				e.sendP2PKeepalive()
			}
		}
	})
}

func (e *Edge) sendP2PKeepalive() {
	if e.relayMgr == nil || e.p2pStreams == nil {
		return
	}

	now := time.Now().UnixMilli()

	e.peersMu.RLock()
	var deadPeers []string
	var alivePeers []string
	for id, p := range e.peers {
		if p.UDPAddr == nil {
			continue
		}
		st := e.relayMgr.GetState(id)

		if st == ConnP2P || st == ConnP2PAvailable {
			idleMs := now - p.lastRecvAt
			if p.lastRecvAt > 0 && idleMs > p2pIdleDeadMs {
				deadPeers = append(deadPeers, id)
				continue
			}
		}

		alivePeers = append(alivePeers, id)
	}
	e.peersMu.RUnlock()

	for _, id := range deadPeers {
		log.Printf("[P2P] %s %ds 无任何流量，判定 P2P 失效，强制降级",
			id, p2pIdleDeadMs/1000)
		e.relayMgr.ForceFallback(id, "no-traffic-30s")
	}

	for _, id := range alivePeers {
		e.p2pStreams.sendP2PProbe(id)
	}
}

func (e *Edge) getVirtualIPLocked() string {
	e.peersMu.RLock()
	defer e.peersMu.RUnlock()
	return e.virtualIP
}

// ============ 日志节流 ============

func (e *Edge) logNoPeerThrottled(dstIP string, n int) {
	now := time.Now().UnixMilli()
	e.lastNoPeerLogMu.Lock()
	last := e.lastNoPeerLog["__global__"]
	if now-last < 5000 {
		e.lastNoPeerLogMu.Unlock()
		return
	}
	e.lastNoPeerLog["__global__"] = now
	e.lastNoPeerLogMu.Unlock()
	log.Printf("[TUN] 无匹配 peer，dst=%s len=%d", dstIP, n)
}

func (e *Edge) logTunForward(peerID, dstIP string, n, proto int, sent bool, state ConnType) {
	e.tunStateLogMu.Lock()
	prev, existed := e.tunStateLog[peerID]
	if existed && prev == state {
		e.tunStateLogMu.Unlock()
		return
	}
	e.tunStateLog[peerID] = state
	e.tunStateLogMu.Unlock()

	log.Printf("[TUN] → %s dst=%s len=%d proto=%d sent=%v state=%s",
		peerID, dstIP, n, proto, sent, state)
}

func (e *Edge) forgetTunState(peerID string) {
	e.tunStateLogMu.Lock()
	delete(e.tunStateLog, peerID)
	e.tunStateLogMu.Unlock()
}

// ============ 超时降级 ============

func (e *Edge) scheduleFallbackTimer(peerID string) {
	if e.relayMgr == nil || peerID == "" {
		return
	}

	e.fallbackTimersMu.Lock()
	if t, ok := e.fallbackTimers[peerID]; ok {
		t.Stop()
	}
	t := time.AfterFunc(8*time.Second, func() {
		defer func() {
			if r := recover(); r != nil {
				log.Printf("[fallback-timer] panic: %v", r)
			}
		}()

		e.fallbackTimersMu.Lock()
		delete(e.fallbackTimers, peerID)
		e.fallbackTimersMu.Unlock()

		if e.relayMgr == nil {
			return
		}
		state := e.relayMgr.GetState(peerID)
		if state != ConnUnknown {
			return
		}
		log.Printf("[fallback-timer] %s 8s 内未收到打洞指令，主动降级", peerID)
		e.relayMgr.MarkFallback(peerID)
	})
	e.fallbackTimers[peerID] = t
	e.fallbackTimersMu.Unlock()
}

func (e *Edge) cancelFallbackTimer(peerID string) {
	e.fallbackTimersMu.Lock()
	defer e.fallbackTimersMu.Unlock()
	if t, ok := e.fallbackTimers[peerID]; ok {
		t.Stop()
		delete(e.fallbackTimers, peerID)
	}
}

func (e *Edge) scheduleP2PAvailTimeout(peerID string) {
	if e.relayMgr == nil || peerID == "" {
		return
	}

	e.p2pAvailTimersMu.Lock()
	if t, ok := e.p2pAvailTimers[peerID]; ok {
		t.Stop()
	}

	t := time.AfterFunc(60*time.Second, func() {
		e.p2pAvailTimersMu.Lock()
		delete(e.p2pAvailTimers, peerID)
		e.p2pAvailTimersMu.Unlock()

		if e.relayMgr == nil {
			return
		}
		if e.relayMgr.GetState(peerID) == ConnP2PAvailable {
			log.Printf("[P2P] %s P2PAvailable 超时 60s 未验证数据面，强制降级", peerID)
			e.relayMgr.ForceFallback(peerID, "p2p-avail-timeout")
		}
	})
	e.p2pAvailTimers[peerID] = t
	e.p2pAvailTimersMu.Unlock()
}

func (e *Edge) cancelP2PAvailTimeout(peerID string) {
	if peerID == "" {
		return
	}
	e.p2pAvailTimersMu.Lock()
	defer e.p2pAvailTimersMu.Unlock()
	if t, ok := e.p2pAvailTimers[peerID]; ok {
		t.Stop()
		delete(e.p2pAvailTimers, peerID)
	}
}

// ============ 工具函数 ============

func cidrToNetmask(cidr string) string {
	parts := strings.Split(cidr, "/")
	if len(parts) != 2 {
		return "255.255.255.0"
	}
	prefix, err := strconv.Atoi(parts[1])
	if err != nil || prefix < 0 || prefix > 32 {
		return "255.255.255.0"
	}
	var mask uint32
	if prefix > 0 {
		mask = ^uint32(0) << (32 - prefix)
	}
	return fmt.Sprintf("%d.%d.%d.%d",
		(mask>>24)&255, (mask>>16)&255, (mask>>8)&255, mask&255)
}

func cidrNetworkAddr(cidr string) string {
	parts := strings.Split(cidr, "/")
	if len(parts) != 2 {
		return cidr
	}
	return parts[0]
}

func cidrPrefix(cidr string) string {
	parts := strings.Split(cidr, "/")
	if len(parts) != 2 {
		return "24"
	}
	return parts[1]
}

func extractIPFromEndpoint(ep string) string {
	if ep == "" {
		return ""
	}
	i := strings.LastIndex(ep, ":")
	if i < 0 {
		return ep
	}
	return ep[:i]
}

func lastIndexByte(s string, c byte) int {
	for i := len(s) - 1; i >= 0; i-- {
		if s[i] == c {
			return i
		}
	}
	return -1
}

// ============ 主函数 ============

func main() {
	// ★ 方案 C：只在 Termux 环境启用 DNS 修复
	//   PC / macOS / Windows 完全跳过，100% 零影响
	if isTermux() {
		setupResolver()
	}

	signalingURL := getEnv("SIGNALING_URL", DefaultSignalingURL)
	roomId := getEnv("ROOM_ID", "default-room")
	clientId := getEnv("CLIENT_ID", "")
	nodeName := getEnv("NODE_NAME", "")
	tunName := getEnv("TUN_NAME", "n2n0")
	udpPort := getEnvInt("UDP_PORT", 0)
	stunServers := getEnv("STUN_SERVERS", "")
	connectToken := getEnv("CONNECT_TOKEN", "")
	socks5Listen := getEnv("SOCKS5_LISTEN", "127.0.0.1:1080")

	if signalingURL == "" {
		log.Fatal("请设置 SIGNALING_URL")
	}

	if clientId == "" {
		installDir := getEnv("INSTALL_DIR", filepath.Join(os.Getenv("HOME"), ".n2n-go"))
		idFile := filepath.Join(installDir, "client_id")
		if b, err := os.ReadFile(idFile); err == nil {
			clientId = strings.TrimSpace(string(b))
			if clientId != "" {
				log.Printf("[配置] 从 %s 复用 CLIENT_ID: %s", idFile, clientId)
			}
		}
		if clientId == "" {
			hostname, _ := os.Hostname()
			clientId = fmt.Sprintf("%s-%d", hostname, time.Now().UnixNano()%1e9)
			_ = os.MkdirAll(installDir, 0700)
			if err := os.WriteFile(idFile, []byte(clientId), 0600); err != nil {
				log.Printf("[配置] 持久化 CLIENT_ID 失败: %v", err)
			} else {
				log.Printf("[配置] 生成并持久化 CLIENT_ID: %s -> %s", clientId, idFile)
			}
		}
	}
	if nodeName == "" {
		hostname, _ := os.Hostname()
		nodeName = hostname
	}
	_ = tunName

	log.Printf("n2n-go-client %s 启动", BuildVersion)
	if connectToken != "" {
		log.Printf("[配置] CONNECT_TOKEN 已设置（%d 字符）", len(connectToken))
	} else {
		log.Printf("[配置] 未设置 CONNECT_TOKEN")
	}

	edge := &Edge{
		clientId:          clientId,
		nodeName:          nodeName,
		roomId:            roomId,
		virtualCIDR:       DefaultVirtualCIDR,
		peers:             make(map[string]*PeerInfo),
		tunWriteCh:        make(chan []byte, 1024),
		udpPort:           udpPort,
		fallbackTimers:    make(map[string]*time.Timer),
		p2pAvailTimers:    make(map[string]*time.Timer),
		lastNoPeerLog:     make(map[string]int64),
		tunStateLog:       make(map[string]ConnType),
		p2pStatusReported: make(map[string]bool),
		doneCh:            make(chan struct{}),
	}
	edge.punchAckLimiter = newPunchAckLimiter()
	edge.streamMgr = NewAppStreamManager()

	udpAddr := &net.UDPAddr{IP: net.IPv4zero, Port: udpPort}
	udpConn, err := net.ListenUDP("udp", udpAddr)
	if err != nil && udpPort != 0 {
		log.Printf("[P2P] 端口 %d 被占用，回退到内核分配", udpPort)
		udpConn, err = net.ListenUDP("udp", &net.UDPAddr{IP: net.IPv4zero, Port: 0})
	}
	if err != nil {
		log.Fatalf("绑定 UDP 失败: %v", err)
	}
	edge.udpConn = udpConn
	defer udpConn.Close()

	actualPort := udpConn.LocalAddr().(*net.UDPAddr).Port
	edge.udpPort = actualPort
	log.Printf("[P2P] UDP 监听端口 %d", actualPort)

	if lans := collectLANEndpoints(actualPort, ""); len(lans) > 0 {
		saveLANCache(lans)
		log.Printf("[NAT] 启动预收集 LAN: %v", lans)
	} else {
		log.Printf("[NAT] 启动预收集 LAN 为空（可能蜂窝网络，或 WiFi 未就绪）")
	}

	servers := []string{}
	if stunServers != "" {
		servers = splitCSV(stunServers)
	}
	edge.natMeta = probeNAT(actualPort, servers)

	edge.p2pStreams = NewP2PStreamManager(edge)

	ws, err := NewWSTransport(signalingURL, roomId, clientId, connectToken)
	if err != nil {
		log.Fatalf("连接信令失败: %v", err)
	}
	defer ws.Close()
	edge.ws = ws
	log.Printf("已连接信令，Client ID: %s，节点名: %s", clientId, nodeName)
	ws.StartHeartbeat(20 * time.Second)

	ws.onReconnect = func() {
		log.Printf("[信令] WebSocket 重连成功，清空所有连接状态")

		if edge.relayMgr != nil {
			edge.relayMgr.ClearAll()
		}
		if edge.p2pStreams != nil {
			edge.p2pStreams.closeAll()
		}
		if edge.punchAckLimiter != nil {
			edge.punchAckLimiter.clear()
		}
		if edge.streamMgr != nil {
			edge.streamMgr.closeAll()
		}

		edge.p2pStatusReportedMu.Lock()
		edge.p2pStatusReported = make(map[string]bool)
		edge.p2pStatusReportedMu.Unlock()

		edge.p2pAvailTimersMu.Lock()
		for _, t := range edge.p2pAvailTimers {
			t.Stop()
		}
		edge.p2pAvailTimers = make(map[string]*time.Timer)
		edge.p2pAvailTimersMu.Unlock()

		// ★ P1 修复：同时清空 failCounts 和 natHoleActive
		natHoleActiveMu.Lock()
		failCount := len(failCounts)
		if failCount > 0 {
			failCounts = make(map[string]int)
		}
		nActive := len(natHoleActive)
		if nActive > 0 {
			natHoleActive = make(map[string]bool)
		}
		natHoleActiveMu.Unlock()
		if failCount > 0 || nActive > 0 {
			log.Printf("[NAT-HOLE] WS 重连，清空 %d 个失败计数 / %d 个 in-flight 标记",
				failCount, nActive)
		}

		if edge.getVirtualIPLocked() != "" {
			edge.reportMetadata()
		}
	}

	edge.turnClient = NewTURNClient(signalingURL, connectToken, edge)
	edge.turnClient.onMessage = func(data []byte, addr net.Addr) {
		ua, ok := addr.(*net.UDPAddr)
		if !ok {
			edge.enqueueTUN(data)
			return
		}
		if isP2PFrame(data) {
			edge.handleP2PFrame(ua, true, data)
			return
		}
		if isPunchProbe(data) {
			edge.notePeerProbeTURN(ua)
			return
		}
		if isPunchAck(data) {
			edge.notePeerAckTURN(ua)
			return
		}
		if len(data) > 0 && data[0]>>4 == 4 {
			edge.notePeerTrafficTURN(ua)
			edge.enqueueTUN(data)
			return
		}
	}

	edge.relayMgr = NewRelayManager(ws, edge.turnClient, edge)
	edge.relayMgr.Report(10 * time.Second)

	ws.SetHandlers(
		edge.handleSignaling,
		func(data []byte) {
			if len(data) >= 4 {
				magic := string(data[:4])
				if magic == "N2NW" || magic == "N2NA" {
					edge.handleAppStreamBinary(data)
					return
				}
			}
			edge.enqueueTUN(data)
		},
	)

	safeGo("udpReadLoop", func() {
		buf := make([]byte, 65535)
		for {
			n, addr, err := udpConn.ReadFromUDP(buf)
			if err != nil {
				select {
				case <-edge.doneCh:
					return
				default:
				}
				log.Printf("[P2P] UDP 读错误: %v", err)
				return
			}
			if n < 4 {
				continue
			}

			if buf[0] == 0x01 && buf[1] == 0x01 {
				if ip, port, ok := parseSTUNResponse(buf[:n]); ok {
					edge.addAssistedEndpoint(ip, port)
				}
				continue
			}

			if isP2PFrame(buf[:n]) {
				edge.handleP2PFrame(addr, false, buf[:n])
				continue
			}

			if isPunchProbe(buf[:n]) {
				edge.notePeerProbe(addr)
				continue
			}
			if isPunchAck(buf[:n]) {
				edge.notePeerAck(addr)
				continue
			}

			if buf[0]>>4 == 4 {
				edge.notePeerTraffic(addr)
				edge.enqueueTUN(buf[:n])
			}
		}
	})

	safeGo("tunWriteLoop", func() {
		for data := range edge.tunWriteCh {
			if tun := edge.tun.Load(); tun != nil {
				_, _ = tun.Write(data)
			}
		}
	})

	edge.startKeepalive()
	edge.startP2PKeepalive()

	edge.socks5Srv = NewSocks5Server(edge, socks5Listen)
	safeGo("socks5-server", func() { edge.socks5Srv.Serve() })

	safeGo("turn-init", func() {
		time.Sleep(2 * time.Second)
		ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
		defer cancel()
		if err := edge.turnClient.FetchAndSetup(ctx); err != nil {
			log.Printf("[TURN] 初始化失败: %v（将使用 WS 中继兜底）", err)
		} else {
			log.Printf("[TURN] TURN 中继就绪: %s", edge.turnClient.GetRelayAddr())
			edge.relayMgr.UpgradeRelaysToTURN()
			_ = edge.ws.Send(map[string]interface{}{
				"type":      "turn_relay_info",
				"relayAddr": edge.turnClient.GetRelayAddr(),
			})
		}
	})

	edge.turnClient.StartReconnectLoop()
	edge.startFailCountResetLoop()

	sigCh := make(chan os.Signal, 1)
	signal.Notify(sigCh, syscall.SIGINT, syscall.SIGTERM)
	<-sigCh

	log.Println("正在退出...")
	edge.Stop()
}

func (e *Edge) Stop() {
	e.closeMu.Lock()
	if e.closed {
		e.closeMu.Unlock()
		return
	}
	e.closed = true
	close(e.doneCh)
	e.closeMu.Unlock()

	e.fallbackTimersMu.Lock()
	for _, t := range e.fallbackTimers {
		t.Stop()
	}
	e.fallbackTimers = make(map[string]*time.Timer)
	e.fallbackTimersMu.Unlock()

	e.p2pAvailTimersMu.Lock()
	for _, t := range e.p2pAvailTimers {
		t.Stop()
	}
	e.p2pAvailTimers = make(map[string]*time.Timer)
	e.p2pAvailTimersMu.Unlock()

	if e.turnClient != nil {
		e.turnClient.Close()
	}
	if e.socks5Srv != nil {
		e.socks5Srv.Close()
	}
	if e.streamMgr != nil {
		e.streamMgr.closeAll()
	}
	if e.p2pStreams != nil {
		e.p2pStreams.closeAll()
	}
	if tun := e.tun.Load(); tun != nil {
		_ = tun.Close()
	}
}

// ============ p2p_metadata 上报（含 features） ============

func (e *Edge) reportMetadata() {
	if e.ws == nil || e.natMeta == nil {
		return
	}

	e.mu.Lock()
	nm := e.natMeta
	serverSeenIP := e.serverSeenIP
	e.mu.Unlock()

	if serverSeenIP != "" && nm.PublicEndpoint != "" {
		stunIP := extractIPFromEndpoint(nm.PublicEndpoint)
		if stunIP != "" && stunIP != serverSeenIP {
			nm.MultiExit = true
			nm.NATType = "HardNAT"
			nm.Behavior = "BehaviorPortChanged"
		}
	}

	metaPayload := map[string]interface{}{
		"name":               e.nodeName,
		"natType":            nm.NATType,
		"portsDifference":    nm.PortsDifference,
		"regularPortsChange": nm.RegularPortsChange,
		"behavior":           nm.Behavior,
		"p2pEndpoint":        nm.P2PEndpoint,
		"multiExit":          nm.MultiExit,
		"features":           []string{"n2na", "n2nw"},
	}
	if nm.PublicEndpoint != "" {
		metaPayload["publicEndpoint"] = nm.PublicEndpoint
	}
	if len(nm.AllEndpoints) > 0 {
		metaPayload["assistedEndpoints"] = nm.AllEndpoints
	}

	vip := e.getVirtualIPLocked()
	lanEndpoints := collectLANEndpoints(e.udpPort, vip)
	if len(lanEndpoints) > 0 {
		saveLANCache(lanEndpoints)
	} else {
		if cached := loadLANCache(); len(cached) > 0 {
			lanEndpoints = cached
			log.Printf("[NAT] LAN 实时为空，用缓存: %v", cached)
		}
	}
	if len(lanEndpoints) > 0 {
		metaPayload["lanEndpoints"] = lanEndpoints
		log.Printf("[NAT] 上报 LAN 地址: %v", lanEndpoints)
	}

	log.Printf("[信令] 上报 p2p_metadata: natType=%s publicEndpoint=%q multiExit=%v assisted=%d lan=%d features=[n2na,n2nw]",
		nm.NATType, nm.PublicEndpoint, nm.MultiExit, len(nm.AllEndpoints), len(lanEndpoints))

	_ = e.ws.Send(map[string]interface{}{
		"type":    "p2p_metadata",
		"payload": metaPayload,
	})
}

func (e *Edge) retryLANCollection() {
	safeGo("lan-retry", func() {
		for i := 0; i < 20; i++ {
			select {
			case <-e.doneCh:
				return
			case <-time.After(500 * time.Millisecond):
			}

			vip := e.getVirtualIPLocked()
			lans := collectLANEndpoints(e.udpPort, vip)
			if len(lans) > 0 {
				saveLANCache(lans)
				log.Printf("[NAT] ✅ LAN 重试成功（第 %d 次）: %v，重新上报 p2p_metadata", i+1, lans)
				e.reportMetadata()
				return
			}
		}
		log.Printf("[NAT] ⚠️ LAN 重试 20 次仍为空（10s），放弃")
	})
}

func (e *Edge) printNodeSummary() {
	log.Println("")
	log.Println("================= 本机信息 =================")
	log.Printf("  Client ID   : %s", e.clientId)
	log.Printf("  节点名       : %s", e.nodeName)
	log.Printf("  虚拟 IP     : %s", e.getVirtualIPLocked())
	log.Printf("  虚拟网段    : %s", e.virtualCIDR)
	log.Printf("  P2P 端口    : %d", e.udpPort)
	log.Printf("  模式         : proxy (SOCKS5)")
	log.Println("==========================================")
	log.Println("")
}

func (e *Edge) logPeerReady(peerID string) {
	if peerID == "" {
		return
	}
	e.peersMu.Lock()
	p, ok := e.peers[peerID]
	if !ok || p.loggedReady {
		e.peersMu.Unlock()
		return
	}
	p.loggedReady = true
	vip := p.VirtualIP
	pubIP := p.PubIP
	pubPort := p.PubPort
	clientID := p.ClientID
	lans := p.LanEndpoints
	assisted := p.AssistedEndpoints
	features := append([]string(nil), p.Features...)
	e.peersMu.Unlock()

	log.Println("")
	log.Printf("========== 节点就绪: %s ==========", clientID)
	log.Printf("  虚拟 IP   : %s", vip)
	if pubIP != "" && pubPort > 0 {
		log.Printf("  公网地址  : %s:%d", pubIP, pubPort)
	}
	if len(lans) > 0 {
		log.Printf("  LAN 地址  : %v", lans)
	}
	if len(assisted) > 0 {
		log.Printf("  STUN 端点 : %v", assisted)
	}
	if len(features) > 0 {
		log.Printf("  features  : %v", features)
	}
	log.Println("==========================================")
	log.Println("")
}

func (e *Edge) handleSignaling(msg map[string]interface{}) {
	defer func() {
		if r := recover(); r != nil {
			log.Printf("[信令] handleSignaling panic: %v", r)
		}
	}()

	t, _ := msg["type"].(string)
	from, _ := msg["from"].(string)

	if t == "_reconnected" {
		log.Printf("[信令] WebSocket 重连成功，重新上报元数据")

		// ★ P1 修复：同时清空 failCounts 和 natHoleActive
		natHoleActiveMu.Lock()
		count := len(failCounts)
		if count > 0 {
			failCounts = make(map[string]int)
		}
		nActive := len(natHoleActive)
		if nActive > 0 {
			natHoleActive = make(map[string]bool)
		}
		natHoleActiveMu.Unlock()
		if count > 0 || nActive > 0 {
			log.Printf("[NAT-HOLE] WS 重连，清空 %d 个失败计数 / %d 个 in-flight 标记",
				count, nActive)
		}

		if e.relayMgr != nil {
			e.relayMgr.ClearAll()
		}
		if e.punchAckLimiter != nil {
			e.punchAckLimiter.clear()
		}
		if e.p2pStreams != nil {
			e.p2pStreams.closeAll()
		}
		if e.streamMgr != nil {
			e.streamMgr.closeAll()
		}

		e.p2pStatusReportedMu.Lock()
		e.p2pStatusReported = make(map[string]bool)
		e.p2pStatusReportedMu.Unlock()

		e.reportMetadata()
		return
	}

	switch t {
	case "ready":
		payload, _ := msg["payload"].(map[string]interface{})

		e.peersMu.Lock()
		oldVIP := e.virtualIP
		oldCIDR := e.virtualCIDR

		e.virtualIP, _ = payload["virtualIp"].(string)
		if cidr, ok := payload["virtualNetwork"].(string); ok && cidr != "" {
			e.virtualCIDR = cidr
		}
		newVIP := e.virtualIP
		newCIDR := e.virtualCIDR
		e.peersMu.Unlock()

		log.Printf("分配虚拟 IP: %s（网段 %s）", newVIP, newCIDR)

		if serverIP, ok := payload["yourPublicIp"].(string); ok && serverIP != "" {
			e.mu.Lock()
			e.serverSeenIP = serverIP
			e.mu.Unlock()
			log.Printf("[信令] 服务端看到的本机出口 IP: %s（WS/TCP 出口，仅参考）", serverIP)
		}

		oldTun := e.tun.Load()
		needBuild := oldTun == nil || oldVIP != newVIP || oldCIDR != newCIDR

		if needBuild {
			if oldTun != nil {
				log.Printf("[TUN] 重建（VIP %s→%s，网段 %s→%s）",
					oldVIP, newVIP, oldCIDR, newCIDR)
				e.tun.Store(nil)
				_ = oldTun.Close()
			}
			tun, err := setupTUN(newVIP, newCIDR, getEnv("TUN_NAME", "n2n0"))
			if err != nil {
				log.Printf("[TUN] 启动失败: %v", err)
			} else {
				e.tun.Store(tun)
				safeGo("tunReadLoop", func() { e.tunReadLoop(tun) })
			}
		} else {
			log.Printf("[TUN] 已存在且未变（vip=%s cidr=%s），跳过重建",
				newVIP, newCIDR)
		}

		e.reportMetadata()

		if len(collectLANEndpoints(e.udpPort, newVIP)) == 0 {
			log.Printf("[NAT] 首次 LAN 收集为空，启动后台重试")
			e.retryLANCollection()
		}

		if peers, ok := payload["peers"].([]interface{}); ok {
			for _, p := range peers {
				pm, ok := p.(map[string]interface{})
				if !ok {
					continue
				}
				pid, _ := pm["id"].(string)
				pip, _ := pm["virtualIp"].(string)
				pubIP, _ := pm["publicIp"].(string)
				pubPort := 0
				if f, ok := pm["publicPort"].(float64); ok {
					pubPort = int(f)
				}
				relayAddr, _ := pm["turnRelayAddr"].(string)
				natType, _ := pm["natType"].(string)

				var assisted []string
				if raw, ok := pm["assistedEndpoints"].([]interface{}); ok {
					for _, v := range raw {
						if s, ok := v.(string); ok && s != "" {
							assisted = append(assisted, s)
						}
					}
				}

				var lanEndpoints []string
				if raw, ok := pm["lanEndpoints"].([]interface{}); ok {
					for _, v := range raw {
						if s, ok := v.(string); ok && s != "" {
							lanEndpoints = append(lanEndpoints, s)
						}
					}
				}

				var features []string
				if raw, ok := pm["features"].([]interface{}); ok {
					for _, v := range raw {
						if s, ok := v.(string); ok && s != "" {
							features = append(features, s)
						}
					}
				}

				if pid == "" || pip == "" {
					continue
				}

				e.maybeResetFailCountForPeer(pid, pubIP, pubPort)

				e.registerPeer(pid, pip, pubIP, pubPort, natType, assisted, lanEndpoints)
				e.peersMu.Lock()
				if pi, ok := e.peers[pid]; ok {
					if relayAddr != "" {
						pi.TurnRelayAddr = relayAddr
					}
					if len(features) > 0 {
						pi.Features = features
					}
				}
				e.peersMu.Unlock()
				e.logPeerReady(pid)
			}
		}

		e.printNodeSummary()

	case "joined":
		payload, _ := msg["payload"].(map[string]interface{})
		if payload == nil {
			return
		}
		pip, _ := payload["virtualIp"].(string)
		pubIP, _ := payload["publicIp"].(string)
		pubPort := 0
		if f, ok := payload["publicPort"].(float64); ok {
			pubPort = int(f)
		}
		relayAddr, _ := payload["turnRelayAddr"].(string)
		natType, _ := payload["natType"].(string)

		var assisted []string
		if raw, ok := payload["assistedEndpoints"].([]interface{}); ok {
			for _, v := range raw {
				if s, ok := v.(string); ok && s != "" {
					assisted = append(assisted, s)
				}
			}
		}

		var lanEndpoints []string
		if raw, ok := payload["lanEndpoints"].([]interface{}); ok {
			for _, v := range raw {
				if s, ok := v.(string); ok && s != "" {
					lanEndpoints = append(lanEndpoints, s)
				}
			}
		}

		var features []string
		if raw, ok := payload["features"].([]interface{}); ok {
			for _, v := range raw {
				if s, ok := v.(string); ok && s != "" {
					features = append(features, s)
				}
			}
		}

		if from != "" && pip != "" {
			e.peersMu.RLock()
			p, exists := e.peers[from]
			alreadyReady := exists && p.loggedReady
			e.peersMu.RUnlock()

			e.maybeResetFailCountForPeer(from, pubIP, pubPort)

			e.registerPeer(from, pip, pubIP, pubPort, natType, assisted, lanEndpoints)
			e.peersMu.Lock()
			if pi, ok := e.peers[from]; ok {
				if relayAddr != "" {
					pi.TurnRelayAddr = relayAddr
				}
				if len(features) > 0 {
					pi.Features = features
				}
			}
			e.peersMu.Unlock()

			if !alreadyReady {
				log.Printf("[信令] 节点上线: %s (vip=%s) features=%v", from, pip, features)
			}
			e.logPeerReady(from)

			if e.p2pStreams != nil && e.relayMgr != nil {
				if e.relayMgr.GetState(from) != ConnP2P {
					targetID := from
					safeGo("p2p-preconnect", func() {
						time.Sleep(200 * time.Millisecond)
						e.p2pStreams.sendP2PProbe(targetID)
						log.Printf("[P2P] 预连接试探 → %s", targetID)
					})
				}
			}
		}

	case "nat_hole_instruction":
		raw, _ := json.Marshal(msg["payload"])
		var instr NatHoleInstruction
		if err := json.Unmarshal(raw, &instr); err != nil {
			log.Printf("[NAT-HOLE] 指令解析失败: %v", err)
			return
		}
		if !e.peerExists(instr.TargetMac) {
			log.Printf("[NAT-HOLE] 忽略指令 target=%s（peer 不在线）", instr.TargetMac)
			return
		}

		if e.relayMgr != nil && e.relayMgr.GetState(instr.TargetMac) == ConnP2P {
			log.Printf("[NAT-HOLE] 跳过指令 target=%s（已 P2P），上报 p2p_status=3",
				instr.TargetMac)
			e.reportP2PStatus(instr.TargetMac, 3)
			return
		}

		e.ensureTargetPeer(&instr)
		e.scheduleFallbackTimer(instr.TargetMac)
		instrCopy := instr
		safeGo("nat-hole", func() { e.runNatHole(&instrCopy) })

	case "force_fallback":
		payload, _ := msg["payload"].(map[string]interface{})
		if payload == nil {
			return
		}
		peersRaw, _ := payload["peers"].([]interface{})
		reason, _ := payload["reason"].(string)
		if reason == "" {
			reason = "server-forced"
		}
		log.Printf("[信令] 收到 force_fallback: %d 个 peer, reason=%s", len(peersRaw), reason)
		for _, pRaw := range peersRaw {
			peerID, _ := pRaw.(string)
			if peerID == "" {
				continue
			}
			if e.relayMgr == nil {
				continue
			}
			if e.relayMgr.GetState(peerID) == ConnP2P {
				continue
			}
			e.relayMgr.ForceFallback(peerID, reason)
		}

	case "turn_peer_info":
		edgeMac, _ := msg["edgeMac"].(string)
		relayAddr, _ := msg["relayAddr"].(string)
		if edgeMac != "" && relayAddr != "" {
			e.peersMu.Lock()
			if p, ok := e.peers[edgeMac]; ok {
				p.TurnRelayAddr = relayAddr
			}
			e.peersMu.Unlock()
			log.Printf("[TURN] 记录 %s 的中继地址: %s", edgeMac, relayAddr)
		}

	case "app_stream_open":
		safeGo("app-stream-open", func() { e.onRemoteStreamOpen(msg) })
	case "app_stream_open_ack":
		e.onRemoteStreamAck(msg)
	case "app_stream_data":
		e.onRemoteStreamData(msg)
	case "app_stream_close":
		e.onRemoteStreamClose(msg)

	case "pong":
		return

	case "left":
		log.Printf("[信令] 节点离开: %s", from)

		if e.relayMgr != nil {
			e.relayMgr.ClearPeer(from)
		}
		if e.punchAckLimiter != nil {
			e.punchAckLimiter.forget(from)
		}

		e.cancelP2PAvailTimeout(from)

		e.p2pStatusReportedMu.Lock()
		delete(e.p2pStatusReported, from)
		e.p2pStatusReportedMu.Unlock()

		natHoleActiveMu.Lock()
		delete(failCounts, from)
		delete(natHoleActive, from)
		natHoleActiveMu.Unlock()

		e.peersMu.Lock()
		delete(e.peers, from)
		e.peersMu.Unlock()

		e.cancelFallbackTimer(from)
		e.lastNoPeerLogMu.Lock()
		delete(e.lastNoPeerLog, from)
		e.lastNoPeerLogMu.Unlock()
		e.forgetTunState(from)
		if e.p2pStreams != nil {
			e.p2pStreams.dropPeer(from)
		}
	}
}

// ★ P0 修复：只在 IP 变化时清空连接状态，端口漂移保留 P2P 状态
func (e *Edge) maybeResetFailCountForPeer(peerID, newPubIP string, newPubPort int) {
	if peerID == "" || newPubIP == "" || newPubPort <= 0 {
		return
	}

	e.peersMu.RLock()
	oldPubIP := ""
	oldPubPort := 0
	if p, ok := e.peers[peerID]; ok {
		oldPubIP = p.PubIP
		oldPubPort = p.PubPort
	}
	e.peersMu.RUnlock()

	if oldPubIP == "" || (oldPubIP == newPubIP && oldPubPort == newPubPort) {
		return
	}

	natHoleActiveMu.Lock()
	hadCount := failCounts[peerID]
	delete(failCounts, peerID)
	natHoleActiveMu.Unlock()

	if oldPubIP != newPubIP {
		if e.relayMgr != nil {
			e.relayMgr.ClearPeer(peerID)
		}
		log.Printf("[NAT-HOLE] %s 出口 IP 变化 %s → %s，清空连接状态 (was=%d)",
			peerID, oldPubIP, newPubIP, hadCount)
	} else {
		log.Printf("[NAT-HOLE] %s 端口漂移 %s:%d → %s:%d，保留 P2P 状态 (was=%d)",
			peerID, oldPubIP, oldPubPort, newPubIP, newPubPort, hadCount)
	}
}

func (e *Edge) maybeResetOwnFailCounts(newEndpoint string) {
	if newEndpoint == "" {
		return
	}

	newIP := extractIPFromEndpoint(newEndpoint)
	if newIP == "" {
		return
	}

	e.mu.Lock()
	oldEndpoint := ""
	if e.natMeta != nil {
		oldEndpoint = e.natMeta.PublicEndpoint
	}
	e.mu.Unlock()

	oldIP := extractIPFromEndpoint(oldEndpoint)

	if oldIP == "" || oldIP == newIP {
		return
	}

	e.mu.Lock()
	if e.natMeta != nil {
		e.natMeta.PublicEndpoint = newEndpoint
		e.natMeta.P2PEndpoint = newEndpoint
	}
	e.mu.Unlock()

	natHoleActiveMu.Lock()
	count := len(failCounts)
	if count > 0 {
		failCounts = make(map[string]int)
	}
	natHoleActiveMu.Unlock()

	log.Printf("[NAT-HOLE] 本机出口 IP 变化 %s → %s，重置所有失败计数 (count=%d)",
		oldIP, newIP, count)
}

func (e *Edge) peerExists(peerID string) bool {
	if peerID == "" {
		return false
	}
	e.peersMu.RLock()
	defer e.peersMu.RUnlock()
	_, ok := e.peers[peerID]
	return ok
}

func (e *Edge) registerPeer(peerID, virtualIP, pubIP string, pubPort int, natType string, assisted, lanEndpoints []string) {
	log.Printf("[registerPeer] %s: 收到 assisted=%d 个 %v lanEndpoints=%d 个 %v",
		peerID, len(assisted), assisted, len(lanEndpoints), lanEndpoints)

	var udpAddr *net.UDPAddr
	if pubIP != "" && pubPort > 0 {
		udpAddr = &net.UDPAddr{IP: net.ParseIP(pubIP), Port: pubPort}
	}

	selfVIP := e.getVirtualIPLocked()
	prefixes := localPrefixes(selfVIP)
	sortedLAN := sortLANByAffinity(lanEndpoints, prefixes)

	log.Printf("[registerPeer] %s: sortedLAN=%v (prefixes=%d)", peerID, sortedLAN, len(prefixes))

	e.peersMu.Lock()
	_, existed := e.peers[peerID]
	if existed {
		p := e.peers[peerID]
		if virtualIP != "" {
			p.VirtualIP = virtualIP
		}
		if pubIP != "" {
			p.PubIP = pubIP
		}
		if pubPort > 0 {
			p.PubPort = pubPort
		}

		if udpAddr != nil {
			oldIsLAN := p.UDPAddr != nil && isPrivateIPv4(p.UDPAddr.IP)
			if !oldIsLAN {
				p.UDPAddr = udpAddr
			}
		}

		if natType != "" {
			p.NATType = natType
		}
		if len(assisted) > 0 {
			p.AssistedEndpoints = assisted
		}
		if len(sortedLAN) > 0 {
			p.LanEndpoints = sortedLAN
		}
	} else {
		var initialUDP *net.UDPAddr
		if len(sortedLAN) > 0 {
			for _, lan := range sortedLAN {
				if ra, err := net.ResolveUDPAddr("udp", lan); err == nil && ra != nil && ra.IP != nil {
					initialUDP = ra
					log.Printf("[P2P] %s 新建 peer，初始 UDPAddr 用 LAN: %s", peerID, lan)
					break
				}
			}
		}
		if initialUDP == nil {
			initialUDP = udpAddr
		}

		e.peers[peerID] = &PeerInfo{
			ClientID:          peerID,
			VirtualIP:         virtualIP,
			PubIP:             pubIP,
			PubPort:           pubPort,
			UDPAddr:           initialUDP,
			NATType:           natType,
			AssistedEndpoints: assisted,
			LanEndpoints:      sortedLAN,
		}
	}
	e.peersMu.Unlock()

	if !existed {
		e.scheduleFallbackTimer(peerID)
	}
}

func (e *Edge) ensureTargetPeer(instr *NatHoleInstruction) {
	if instr.TargetMac == "" {
		return
	}
	var udpAddr *net.UDPAddr
	if instr.TargetPubSocket != "" {
		udpAddr = parseSockAddr(instr.TargetPubSocket)
	}
	e.peersMu.Lock()
	defer e.peersMu.Unlock()
	if p, ok := e.peers[instr.TargetMac]; ok {
		if udpAddr != nil {
			oldIsLAN := p.UDPAddr != nil && isPrivateIPv4(p.UDPAddr.IP)
			if !oldIsLAN {
				p.UDPAddr = udpAddr
			}
		}
		if instr.TargetVirtualIp != "" {
			p.VirtualIP = instr.TargetVirtualIp
		}
	} else {
		var initialUDP *net.UDPAddr
		if len(instr.TargetLanEndpoints) > 0 {
			for _, lan := range instr.TargetLanEndpoints {
				if ra, err := net.ResolveUDPAddr("udp", lan); err == nil && ra != nil && ra.IP != nil {
					initialUDP = ra
					log.Printf("[P2P] %s 新建 peer（ensureTargetPeer），初始 UDPAddr 用 LAN: %s",
						instr.TargetMac, lan)
					break
				}
			}
		}
		if initialUDP == nil {
			initialUDP = udpAddr
		}

		e.peers[instr.TargetMac] = &PeerInfo{
			ClientID:     instr.TargetMac,
			VirtualIP:    instr.TargetVirtualIp,
			UDPAddr:      initialUDP,
			LanEndpoints: instr.TargetLanEndpoints,
		}
	}
}

func (e *Edge) runNatHole(instr *NatHoleInstruction) {
	res := e.executeNatHole(instr)
	if res == nil {
		return
	}

	if res.State == PunchStateSucceeded {
		e.relayMgr.MarkP2PAvailable(instr.TargetMac)
		safeGo("p2p-probe", func() {
			e.sendP2PProbe(instr.TargetMac)
		})
	} else if res.State == PunchStateFailed {
		e.relayMgr.MarkFallback(instr.TargetMac)
	}

	p2pStatus := 0
	switch e.relayMgr.GetState(instr.TargetMac) {
	case ConnP2P:
		p2pStatus = 3
	case ConnP2PAvailable:
		p2pStatus = 2
	default:
		p2pStatus = 0
	}

	_ = e.ws.Send(map[string]interface{}{
		"type": "p2p_state_info",
		"payload": map[string]interface{}{
			"to": []map[string]interface{}{
				{
					"macAddr":            instr.TargetMac,
					"observedRaddr":      "",
					"punchResult":        res,
					"punchResultPeerMac": instr.TargetMac,
					"p2pStatus":          p2pStatus,
				},
			},
		},
	})
}

func (e *Edge) reportP2PStatus(peerID string, status int) {
	if e.ws == nil || peerID == "" {
		return
	}

	e.p2pStatusReportedMu.Lock()
	if e.p2pStatusReported[peerID] {
		e.p2pStatusReportedMu.Unlock()
		return
	}
	e.p2pStatusReported[peerID] = true
	e.p2pStatusReportedMu.Unlock()

	_ = e.ws.Send(map[string]interface{}{
		"type": "p2p_state_info",
		"payload": map[string]interface{}{
			"to": []map[string]interface{}{
				{
					"macAddr": peerID,
					"punchResult": map[string]interface{}{
						"state":    3,
						"attempts": 0,
						"detail":   "already-p2p",
					},
					"punchResultPeerMac": peerID,
					"p2pStatus":          status,
				},
			},
		},
	})
}

func (e *Edge) tunReadLoop(tun *TUNDevice) {
	defer func() {
		if r := recover(); r != nil {
			log.Printf("[TUN] tunReadLoop panic: %v", r)
		}
	}()

	if tun == nil {
		return
	}

	buf := make([]byte, 65535)
	for {
		n, err := tun.Read(buf)
		if err != nil {
			select {
			case <-e.doneCh:
				return
			default:
			}
			if isTransientReadError(err) {
				time.Sleep(10 * time.Millisecond)
				continue
			}
			log.Printf("[TUN] 读错误，退出读循环: %v", err)
			return
		}
		if n < 20 || buf[0]>>4 != 4 {
			continue
		}

		dstIP := net.IP(buf[16:20]).String()

		e.peersMu.RLock()
		var target *PeerInfo
		for _, p := range e.peers {
			if p.VirtualIP == dstIP {
				target = p
				break
			}
		}
		e.peersMu.RUnlock()

		proto := 0
		if n >= 10 {
			proto = int(buf[9])
		}

		if target == nil {
			if proto == 17 && n < 200 {
				if e.ws != nil {
					_ = e.ws.SendBinary(buf[:n])
				}
			} else {
				e.logNoPeerThrottled(dstIP, n)
			}
			continue
		}

		if proto == 1 {
			e.sendICMPDirect(target, buf[:n])
			atomic.StoreInt64(&e.lastDataFrameAt, time.Now().UnixMilli())
			if n >= 10 {
				e.logTunForward(target.ClientID, dstIP, n, proto, true, ConnP2P)
			}
			continue
		}

		state := ConnUnknown
		if e.relayMgr != nil {
			state = e.relayMgr.GetState(target.ClientID)
		}

		if state == ConnTURN {
			pktCopy := make([]byte, n)
			copy(pktCopy, buf[:n])
			targetID := target.ClientID
			targetCopy := target
			pktLen := n

			safeGo("tun-send-turn", func() {
				sent := false
				if e.relayMgr != nil {
					sent = e.relayMgr.SendToPeer(targetID, pktCopy, targetCopy)
				}
				e.logTunForward(targetID, dstIP, pktLen, proto, sent, ConnTURN)
				if sent {
					atomic.StoreInt64(&e.lastDataFrameAt, time.Now().UnixMilli())
				}
				if !sent && e.ws != nil {
					frame := buildN2NWFrame(targetID, pktCopy)
					_ = e.ws.SendBinary(frame)
				}
			})
			continue
		}

		sent := false
		if e.relayMgr != nil {
			sent = e.relayMgr.SendToPeer(target.ClientID, buf[:n], target)
		}
		if sent {
			atomic.StoreInt64(&e.lastDataFrameAt, time.Now().UnixMilli())
		}
		if n >= 10 {
			e.logTunForward(target.ClientID, dstIP, n, proto, sent, state)
		}
		if !sent && e.ws != nil {
			frame := buildN2NWFrame(target.ClientID, buf[:n])
			_ = e.ws.SendBinary(frame)
			atomic.StoreInt64(&e.lastDataFrameAt, time.Now().UnixMilli())
		}
	}
}

func (e *Edge) sendICMPDirect(target *PeerInfo, pkt []byte) {
	if target == nil {
		return
	}

	if len(target.LanEndpoints) > 0 && e.udpConn != nil {
		for _, lan := range target.LanEndpoints {
			ra, err := net.ResolveUDPAddr("udp", lan)
			if err != nil || ra == nil || ra.IP == nil {
				continue
			}
			if _, err := e.udpConn.WriteToUDP(pkt, ra); err == nil {
				return
			}
		}
	}

	if target.UDPAddr != nil && e.udpConn != nil {
		if _, err := e.udpConn.WriteToUDP(pkt, target.UDPAddr); err == nil {
			return
		}
	}

	if target.TurnRelayAddr != "" && e.turnClient != nil && e.turnClient.IsReady() {
		relayAddr, err := net.ResolveUDPAddr("udp", target.TurnRelayAddr)
		if err == nil {
			if sendErr := e.turnClient.Send(pkt, relayAddr); sendErr == nil {
				return
			}
		}
	}

	if e.ws != nil {
		frame := buildN2NWFrame(target.ClientID, pkt)
		_ = e.ws.SendBinary(frame)
	}
}

func (e *Edge) enqueueTUN(data []byte) {
	cp := make([]byte, len(data))
	copy(cp, data)
	select {
	case e.tunWriteCh <- cp:
	default:
		log.Printf("[TUN] 写入队列已满，丢弃 %d 字节", len(cp))
	}
}

// ============ peer 探测处理 ============

func (e *Edge) notePeerProbe(addr *net.UDPAddr) {
	e.notePeerCommon(addr, false, false, false)
}

func (e *Edge) notePeerAck(addr *net.UDPAddr) {
	e.notePeerCommon(addr, false, false, true)
}

func (e *Edge) notePeerProbeTURN(addr *net.UDPAddr) {
	e.notePeerCommon(addr, false, true, false)
}

func (e *Edge) notePeerAckTURN(addr *net.UDPAddr) {
	e.notePeerCommon(addr, false, true, true)
}

func (e *Edge) notePeerTraffic(addr *net.UDPAddr) {
	e.notePeerCommon(addr, true, false, false)
}

func (e *Edge) notePeerTrafficTURN(addr *net.UDPAddr) {
	e.notePeerCommon(addr, true, true, false)
}

func (e *Edge) notePeerCommon(addr *net.UDPAddr, isRealData bool, viaTURN bool, isAck bool) {
	now := time.Now().UnixMilli()

	e.peersMu.Lock()
	var best *PeerInfo
	bestScore := 0
	addrIP := addr.IP
	addrPort := addr.Port

	for _, p := range e.peers {
		score := 0

		if p.UDPAddr != nil && p.UDPAddr.IP.Equal(addrIP) {
			if p.UDPAddr.Port == addrPort {
				score = 4
			} else {
				score = 3
			}
		}

		for _, lan := range p.LanEndpoints {
			la, err := net.ResolveUDPAddr("udp", lan)
			if err != nil || la == nil || la.IP == nil {
				continue
			}
			if la.IP.Equal(addrIP) {
				if la.Port == addrPort {
					if score < 5 {
						score = 5
					}
				} else if score < 2 {
					score = 2
				}
			}
		}

		for _, ass := range p.AssistedEndpoints {
			aa, err := net.ResolveUDPAddr("udp", ass)
			if err != nil || aa == nil || aa.IP == nil {
				continue
			}
			if aa.IP.Equal(addrIP) {
				if aa.Port == addrPort {
					if score < 4 {
						score = 4
					}
				} else if score < 1 {
					score = 1
				}
			}
		}

		if score > 0 && score > bestScore {
			best = p
			bestScore = score
		}
	}
	if best == nil {
		e.peersMu.Unlock()
		return
	}

	newIsLAN := isPrivateIPv4(addr.IP)
	oldIsLAN := best.UDPAddr != nil && isPrivateIPv4(best.UDPAddr.IP)

	shouldUpdate := false
	if best.UDPAddr == nil {
		shouldUpdate = true
	} else if !oldIsLAN && newIsLAN {
		shouldUpdate = true
	} else if oldIsLAN && !newIsLAN {
		if best.lastLANRecvAt > 0 && now-best.lastLANRecvAt > 30000 {
			shouldUpdate = true
			log.Printf("[P2P] %s (note) LAN 超过 30s 无流量，改用公网地址", best.ClientID)
		}
	} else {
		if best.UDPAddr.Port != addrPort {
			shouldUpdate = true
		}
	}

	if newIsLAN {
		best.lastLANRecvAt = now
	}

	if shouldUpdate {
		best.UDPAddr = &net.UDPAddr{IP: addr.IP, Port: addr.Port}
	}

	best.lastRecvAt = now
	if isRealData {
		best.hasRealData = true
	}
	clientID := best.ClientID
	ip := addr.IP.String()
	e.peersMu.Unlock()

	if e.relayMgr == nil {
		return
	}

	if isRealData {
		if e.relayMgr.GetState(clientID) != ConnP2P {
			log.Printf("[P2P] 从 %s (%s) 收到真实数据帧 → P2P 直连", clientID, ip)
			e.relayMgr.MarkP2P(clientID)
		}
		return
	}

	curState := e.relayMgr.GetState(clientID)
	justUpgraded := false
	if curState != ConnP2P && curState != ConnP2PAvailable {
		kind := "Punch"
		if isAck {
			kind = "ACK"
		}
		log.Printf("[P2P] 从 %s (%s) 收到 %s → P2PAvailable（等待数据帧验证）", clientID, ip, kind)
		e.relayMgr.MarkP2PAvailable(clientID)
		justUpgraded = true
	}

	if justUpgraded {
		if e.p2pStreams != nil {
			safeGo("p2p-probe", func() {
				e.p2pStreams.sendP2PProbe(clientID)
			})
		}
		e.scheduleP2PAvailTimeout(clientID)
	}

	if isAck {
		return
	}
	if e.punchAckLimiter == nil || !e.punchAckLimiter.allow(clientID, time.Now()) {
		return
	}

	safeGo("punch-ack", func() {
		ack := buildPunchAck()
		e.writeProbe(addr, ack, viaTURN)
	})
}

func (e *Edge) writeProbe(addr *net.UDPAddr, probe []byte, viaTURN bool) {
	if addr == nil {
		return
	}
	if viaTURN {
		if e.turnClient == nil || !e.turnClient.IsReady() {
			return
		}
		_ = e.turnClient.Send(probe, addr)
		return
	}
	if e.udpConn == nil {
		return
	}
	_, _ = e.udpConn.WriteToUDP(probe, addr)
}

func (e *Edge) sendProbeTo(addr *net.UDPAddr) {
	if e.udpConn == nil || addr == nil {
		return
	}
	probe := buildPunchProbe(e.getVirtualIPLocked())
	e.writeProbe(addr, probe, false)
}

// ============ 多 STUN 端点收集 ============

func (e *Edge) addAssistedEndpoint(ip string, port int) {
	if ip == "" || port <= 0 {
		return
	}
	ep := net.JoinHostPort(ip, strconv.Itoa(port))

	e.mu.Lock()
	if e.natMeta == nil {
		e.mu.Unlock()
		return
	}
	for _, existing := range e.natMeta.AllEndpoints {
		if existing == ep {
			e.mu.Unlock()
			return
		}
	}
	if len(e.natMeta.AllEndpoints) >= 5 {
		e.natMeta.AllEndpoints = e.natMeta.AllEndpoints[1:]
	}
	e.natMeta.AllEndpoints = append(e.natMeta.AllEndpoints, ep)
	count := len(e.natMeta.AllEndpoints)
	e.mu.Unlock()

	e.maybeResetOwnFailCounts(ep)

	log.Printf("[NAT] +assisted: %s (total=%d)", ep, count)
	e.reportMetadataThrottled()
}

func (e *Edge) reportMetadataThrottled() {
	now := time.Now().UnixMilli()
	last := atomic.LoadInt64(&e.lastMetadataReportAt)
	if now-last < 30000 {
		return
	}
	if !atomic.CompareAndSwapInt64(&e.lastMetadataReportAt, last, now) {
		return
	}
	e.reportMetadata()
}

// ============ 环境变量 / CSV ============

func getEnv(k, fb string) string {
	if v := os.Getenv(k); v != "" {
		return v
	}
	return fb
}

func getEnvInt(k string, fb int) int {
	if v := os.Getenv(k); v != "" {
		var n int
		_, _ = fmt.Sscanf(v, "%d", &n)
		return n
	}
	return fb
}

func splitCSV(s string) []string {
	var out []string
	cur := ""
	for _, c := range s {
		if c == ',' {
			if cur != "" {
				out = append(out, cur)
				cur = ""
			}
		} else {
			cur += string(c)
		}
	}
	if cur != "" {
		out = append(out, cur)
	}
	return out
}

// ============================================================
// ★ 方案 C：setupResolver 只在 Termux 环境被调用
//
// 行为：
//   1. 读 Termux 的 resolv.conf
//   2. 过滤本地回环 DNS（::1 / 127.0.0.1）
//   3. 过滤后为空 → fallback 公网 DNS
//   4. 过滤后非空 → 用这些 DNS
//
// PC / macOS / Windows 不会调用此函数（isTermux() 返回 false）。
// ============================================================

func setupResolver() {
	prefix := os.Getenv("PREFIX")
	if prefix == "" {
		prefix = os.Getenv("TERMUX_PREFIX")
	}

	candidates := []string{}
	if prefix != "" {
		candidates = append(candidates, filepath.Join(prefix, "etc", "resolv.conf"))
	}
	candidates = append(candidates,
		"/data/data/com.termux/files/usr/etc/resolv.conf",
		"/etc/resolv.conf",
	)

	var nameservers []string
	var source string
	for _, p := range candidates {
		nss := readNameservers(p)
		if len(nss) > 0 {
			nameservers = nss
			source = p
			break
		}
	}

	// ★ 过滤本地回环 DNS
	filtered := make([]string, 0, len(nameservers))
	for _, ns := range nameservers {
		host := ns
		if h, _, err := net.SplitHostPort(ns); err == nil {
			host = h
		}
		host = strings.Trim(host, "[]")
		if host == "::1" || host == "127.0.0.1" || host == "localhost" {
			continue
		}
		filtered = append(filtered, ns)
	}

	if len(filtered) == 0 {
		filtered = []string{"8.8.8.8:53", "1.1.1.1:53", "114.114.114.114:53"}
		if source == "" {
			source = "<empty>"
		}
		source += " (filtered local, using public)"
	}

	log.Printf("[DNS] 使用 %s 的 %d 个 nameserver: %v", source, len(filtered), filtered)

	var idx uint32
	net.DefaultResolver = &net.Resolver{
		PreferGo: true,
		Dial: func(ctx context.Context, network, address string) (net.Conn, error) {
			n := atomic.AddUint32(&idx, 1)
			ns := filtered[int(n)%len(filtered)]

			netProto := "udp"
			if strings.HasPrefix(network, "tcp") {
				netProto = "tcp"
			}
			d := net.Dialer{Timeout: 5 * time.Second}
			return d.DialContext(ctx, netProto, ns)
		},
	}
}

func readNameservers(path string) []string {
	f, err := os.Open(path)
	if err != nil {
		return nil
	}
	defer f.Close()

	var result []string
	scanner := bufio.NewScanner(f)
	for scanner.Scan() {
		line := strings.TrimSpace(scanner.Text())
		if line == "" || strings.HasPrefix(line, "#") || strings.HasPrefix(line, ";") {
			continue
		}
		fields := strings.Fields(line)
		if len(fields) >= 2 && fields[0] == "nameserver" {
			ns := fields[1]
			if strings.Contains(ns, ":") {
				if !strings.HasPrefix(ns, "[") {
					ns = "[" + ns + "]"
				}
				ns += ":53"
			} else {
				ns += ":53"
			}
			result = append(result, ns)
		}
	}
	return result
}
