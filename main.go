package main

import (
	"context"
	"crypto/rand"
	"encoding/binary"
	"encoding/json"
	"fmt"
	"log"
	"net"
	"os"
	"os/signal"
	"path/filepath"
	"strconv"
	"strings"
	"sync"
	"syscall"
	"time"
)

var (
	BuildVersion        = "dev"
	DefaultSignalingURL = ""
)

const DefaultVirtualCIDR = "10.64.0.0/24"

type PeerInfo struct {
	ClientID      string
	VirtualIP     string
	PubIP         string
	PubPort       int
	TurnRelayAddr string
	UDPAddr       *net.UDPAddr
	lastRecvAt    int64
	loggedReady   bool
	hasRealData   bool
}

type Edge struct {
	clientId    string
	nodeName    string
	virtualIP   string
	virtualCIDR string
	roomId      string

	myLanIPs     []string
	serverSeenIP string

	ws         *WSTransport
	relayMgr   *RelayManager
	turnClient *TURNClient
	tun        *TUNDevice

	udpConn *net.UDPConn
	udpPort int

	peers   map[string]*PeerInfo
	peersMu sync.RWMutex

	tunWriteCh chan []byte

	natMeta *NATMetadata

	fallbackTimers   map[string]*time.Timer
	fallbackTimersMu sync.Mutex

	// ★ 日志节流：同一 dstIP 每 5 秒最多打一次"无匹配 peer"
	lastNoPeerLog   map[string]int64
	lastNoPeerLogMu sync.Mutex

	// ★ 状态变化日志：同一 peer 状态不变时不打 TUN 转发日志
	tunStateLog   map[string]ConnType
	tunStateLogMu sync.Mutex

	doneCh  chan struct{}
	closeMu sync.Mutex
	closed  bool
	mu      sync.Mutex
}

func (e *Edge) Done() <-chan struct{} { return e.doneCh }

// ============ UDP 保活 ============

const keepaliveInterval = 5 * time.Second

var keepaliveServers = []string{
	"74.125.250.129:19302",
	"162.159.207.0:3478",
	"74.125.204.127:19302",
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

// startKeepalive 启动 UDP 保活协程（条件触发：仅在有 peer 时发）。
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

	log.Printf("[Keepalive] 启动（条件触发），每 %v 刷新 %d 个 STUN 服务器（仅在有 peer 时）",
		keepaliveInterval, len(addrs))

	safeGo("keepalive", func() {
		sendOnce := func() {
			for _, addr := range addrs {
				probe := buildSTUNBindingRequest()
				_, _ = e.udpConn.WriteToUDP(probe, addr)
			}
		}

		hasPeers := func() bool {
			e.peersMu.RLock()
			defer e.peersMu.RUnlock()
			return len(e.peers) > 0
		}

		if hasPeers() {
			sendOnce()
		}

		ticker := time.NewTicker(keepaliveInterval)
		defer ticker.Stop()
		for {
			select {
			case <-e.doneCh:
				log.Printf("[Keepalive] 已停止")
				return
			case <-ticker.C:
				if !hasPeers() {
					continue
				}
				sendOnce()
			}
		}
	})
}

// ============ 日志节流 ============

func (e *Edge) logNoPeerThrottled(dstIP string, n int) {
	now := time.Now().UnixMilli()
	e.lastNoPeerLogMu.Lock()
	last := e.lastNoPeerLog[dstIP]
	if now-last < 5000 {
		e.lastNoPeerLogMu.Unlock()
		return
	}
	e.lastNoPeerLog[dstIP] = now
	e.lastNoPeerLogMu.Unlock()
	log.Printf("[TUN] 无匹配 peer，dst=%s len=%d", dstIP, n)
}

// logTunForward 状态变化才打日志：同一 peer 状态不变时静默。
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
			// ★ P2P 状态不打日志（正常流程）
			if state != ConnP2P {
				log.Printf("[fallback-timer] %s 已有状态 %s，跳过降级", peerID, state)
			}
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

// ============ 主函数 ============

func main() {
	signalingURL := getEnv("SIGNALING_URL", DefaultSignalingURL)
	roomId := getEnv("ROOM_ID", "default-room")
	clientId := getEnv("CLIENT_ID", "")
	nodeName := getEnv("NODE_NAME", "")
	tunName := getEnv("TUN_NAME", "n2n0")
	udpPort := getEnvInt("UDP_PORT", 0)
	stunServers := getEnv("STUN_SERVERS", "")
	connectToken := getEnv("CONNECT_TOKEN", "")

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
		clientId:       clientId,
		nodeName:       nodeName,
		roomId:         roomId,
		virtualCIDR:    DefaultVirtualCIDR,
		peers:          make(map[string]*PeerInfo),
		tunWriteCh:     make(chan []byte, 1024),
		udpPort:        udpPort,
		fallbackTimers: make(map[string]*time.Timer),
		lastNoPeerLog:  make(map[string]int64),
		tunStateLog:    make(map[string]ConnType),
		doneCh:         make(chan struct{}),
	}

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

	servers := []string{}
	if stunServers != "" {
		servers = splitCSV(stunServers)
	}
	edge.natMeta = probeNAT(actualPort, servers)

	edge.myLanIPs = extractLanIPs(edge.natMeta.AssistedSockets)
	log.Printf("[LAN] 本机局域网 IP: %v", edge.myLanIPs)

	ws, err := NewWSTransport(signalingURL, roomId, clientId, connectToken)
	if err != nil {
		log.Fatalf("连接信令失败: %v", err)
	}
	defer ws.Close()
	edge.ws = ws
	log.Printf("已连接信令，Client ID: %s，节点名: %s", clientId, nodeName)
	ws.StartHeartbeat(20 * time.Second)

	ws.onReconnect = func() {
		if edge.virtualIP != "" {
			edge.reportMetadata()
		}
	}

	edge.turnClient = NewTURNClient(signalingURL, connectToken, edge)
	edge.turnClient.onMessage = func(data []byte, addr net.Addr) {
		edge.enqueueTUN(data)
	}

	edge.relayMgr = NewRelayManager(ws, edge.turnClient, edge)
	edge.relayMgr.Report(10 * time.Second)

	ws.SetHandlers(
		edge.handleSignaling,
		func(data []byte) {
			edge.enqueueTUN(data)
		},
	)

	// UDP 读循环
	go func() {
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
			if buf[0] == 'N' && buf[1] == '2' && buf[2] == 'N' && buf[3] == 'P' {
				edge.notePeerProbe(addr)
				continue
			}
			if buf[0]>>4 == 4 {
				edge.notePeerTraffic(addr)
				edge.enqueueTUN(buf[:n])
			}
		}
	}()

	go func() {
		for data := range edge.tunWriteCh {
			if edge.tun != nil {
				_, _ = edge.tun.Write(data)
			}
		}
	}()

	// ★ 启动 UDP 保活
	edge.startKeepalive()

	// TURN 初始化
	go func() {
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
	}()

	// ★ 启动 TURN 后台重连循环（网络切换后自动重建）
	edge.turnClient.StartReconnectLoop()

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

	if e.turnClient != nil {
		e.turnClient.Close()
	}
	if e.tun != nil {
		_ = e.tun.Close()
	}
}

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
		"assistedSockets":    nm.AssistedSockets,
		"p2pEndpoint":        nm.P2PEndpoint,
		"lanIps":             e.myLanIPs,
		"udpPort":            e.udpPort,
		"multiExit":          nm.MultiExit,
		"wsPublicIp":         serverSeenIP,
	}
	if nm.PublicEndpoint != "" {
		metaPayload["publicEndpoint"] = nm.PublicEndpoint
	}

	log.Printf("[信令] 上报 p2p_metadata: natType=%s publicEndpoint=%q wsPublicIp=%q lanIps=%v udpPort=%d multiExit=%v",
		nm.NATType, nm.PublicEndpoint, serverSeenIP, e.myLanIPs, e.udpPort, nm.MultiExit)

	_ = e.ws.Send(map[string]interface{}{
		"type":    "p2p_metadata",
		"payload": metaPayload,
	})
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

func (e *Edge) printNodeSummary() {
	log.Println("")
	log.Println("================= 本机信息 =================")
	log.Printf("  Client ID   : %s", e.clientId)
	log.Printf("  节点名       : %s", e.nodeName)
	log.Printf("  虚拟 IP     : %s", e.virtualIP)
	log.Printf("  虚拟网段    : %s", e.virtualCIDR)
	log.Printf("  P2P 端口    : %d", e.udpPort)
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
	e.peersMu.Unlock()

	log.Println("")
	log.Printf("========== 节点就绪: %s ==========", clientID)
	log.Printf("  虚拟 IP   : %s", vip)
	if pubIP != "" && pubPort > 0 {
		log.Printf("  公网地址  : %s:%d", pubIP, pubPort)
	}
	log.Println("==========================================")
	log.Println("")
}

func (e *Edge) handleSignaling(msg map[string]interface{}) {
	t, _ := msg["type"].(string)
	from, _ := msg["from"].(string)

	switch t {
	case "ready":
		payload, _ := msg["payload"].(map[string]interface{})
		e.virtualIP, _ = payload["virtualIp"].(string)

		if cidr, ok := payload["virtualNetwork"].(string); ok && cidr != "" {
			e.virtualCIDR = cidr
		}
		log.Printf("分配虚拟 IP: %s（网段 %s）", e.virtualIP, e.virtualCIDR)

		if serverIP, ok := payload["yourPublicIp"].(string); ok && serverIP != "" {
			e.mu.Lock()
			e.serverSeenIP = serverIP
			e.mu.Unlock()
			log.Printf("[信令] 服务端看到的本机出口 IP: %s（WS/TCP 出口，仅参考）", serverIP)
		}

		var err error
		e.tun, err = setupTUN(e.virtualIP, e.virtualCIDR, getEnv("TUN_NAME", "n2n0"))
		if err != nil {
			log.Printf("[TUN] 启动失败: %v", err)
		} else {
			go e.tunReadLoop()
		}

		e.reportMetadata()

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
				if pid == "" || pip == "" {
					continue
				}
				e.registerPeer(pid, pip, pubIP, pubPort)
				if relayAddr != "" {
					e.peersMu.Lock()
					if pi, ok := e.peers[pid]; ok {
						pi.TurnRelayAddr = relayAddr
					}
					e.peersMu.Unlock()
				}
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
		if from != "" && pip != "" {
			e.peersMu.RLock()
			p, exists := e.peers[from]
			alreadyReady := exists && p.loggedReady
			e.peersMu.RUnlock()

			e.registerPeer(from, pip, pubIP, pubPort)
			if relayAddr != "" {
				e.peersMu.Lock()
				if pi, ok := e.peers[from]; ok {
					pi.TurnRelayAddr = relayAddr
				}
				e.peersMu.Unlock()
			}

			if !alreadyReady {
				log.Printf("[信令] 节点上线: %s (vip=%s)", from, pip)
			}
			e.logPeerReady(from)
		}

	case "nat_hole_instruction":
		raw, _ := json.Marshal(msg["payload"])
		var instr NatHoleInstruction
		if err := json.Unmarshal(raw, &instr); err != nil {
			log.Printf("[NAT-HOLE] 指令解析失败: %v", err)
			return
		}
		// ★ 检查 target 是否还在线
		if !e.peerExists(instr.TargetMac) {
			log.Printf("[NAT-HOLE] 忽略指令 target=%s（peer 不在线）", instr.TargetMac)
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
			e.relayMgr.MarkFallback(peerID)
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

	case "pong":
		return

	case "left":
		log.Printf("[信令] 节点离开: %s", from)
		e.peersMu.Lock()
		delete(e.peers, from)
		e.peersMu.Unlock()
		e.cancelFallbackTimer(from)
		// ★ 清理日志节流记录
		e.lastNoPeerLogMu.Lock()
		delete(e.lastNoPeerLog, from)
		e.lastNoPeerLogMu.Unlock()
		// ★ 清理 TUN 转发日志状态
		e.forgetTunState(from)
	}
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

func (e *Edge) registerPeer(peerID, virtualIP, pubIP string, pubPort int) {
	var udpAddr *net.UDPAddr
	if pubIP != "" && pubPort > 0 {
		udpAddr = &net.UDPAddr{IP: net.ParseIP(pubIP), Port: pubPort}
	}
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
			p.UDPAddr = udpAddr
		}
	} else {
		e.peers[peerID] = &PeerInfo{
			ClientID:  peerID,
			VirtualIP: virtualIP,
			PubIP:     pubIP,
			PubPort:   pubPort,
			UDPAddr:   udpAddr,
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
		if instr.TargetVirtualIp != "" {
			p.VirtualIP = instr.TargetVirtualIp
		}
		if udpAddr != nil {
			p.UDPAddr = udpAddr
		}
	} else {
		e.peers[instr.TargetMac] = &PeerInfo{
			ClientID:  instr.TargetMac,
			VirtualIP: instr.TargetVirtualIp,
			UDPAddr:   udpAddr,
		}
	}
}

func (e *Edge) runNatHole(instr *NatHoleInstruction) {
	res := e.executeNatHole(instr)
	if res == nil {
		return
	}

	if res.State == PunchStateSucceeded {
		e.relayMgr.MarkP2P(instr.TargetMac)
	} else if res.State == PunchStateFailed {
		e.relayMgr.MarkFallback(instr.TargetMac)
	}

	p2pStatus := 0
	switch e.relayMgr.GetState(instr.TargetMac) {
	case ConnP2P:
		p2pStatus = 3
	case ConnTURN, ConnRelay:
		p2pStatus = 2
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

func (e *Edge) tunReadLoop() {
	buf := make([]byte, 65535)
	for {
		n, err := e.tun.Read(buf)
		if err != nil {
			select {
			case <-e.doneCh:
				return
			default:
			}
			log.Printf("[TUN] 读取错误: %v", err)
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

		if target == nil {
			e.logNoPeerThrottled(dstIP, n)
			_ = e.ws.SendBinary(buf[:n])
			continue
		}

		state := ConnUnknown
		if e.relayMgr != nil {
			state = e.relayMgr.GetState(target.ClientID)
		}
		ok := e.relayMgr.SendToPeer(target.ClientID, buf[:n], target)
		if n >= 10 {
			// ★ 状态变化才打
			e.logTunForward(target.ClientID, dstIP, n, int(buf[9]), ok, state)
		}
		if !ok {
			_ = e.ws.SendBinary(buf[:n])
		}
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

func (e *Edge) notePeerProbe(addr *net.UDPAddr) {
	e.notePeerCommon(addr, false)
}

func (e *Edge) notePeerTraffic(addr *net.UDPAddr) {
	e.notePeerCommon(addr, true)
}

func (e *Edge) notePeerCommon(addr *net.UDPAddr, isRealData bool) {
	now := time.Now().UnixMilli()

	e.peersMu.Lock()
	var best *PeerInfo
	bestScore := -1
	for _, p := range e.peers {
		if p.UDPAddr == nil || !p.UDPAddr.IP.Equal(addr.IP) {
			continue
		}
		score := 1
		if p.UDPAddr.Port == addr.Port {
			score = 2
		}
		if score > bestScore {
			best = p
			bestScore = score
		}
	}
	if best == nil {
		e.peersMu.Unlock()
		return
	}
	best.lastRecvAt = now
	if isRealData {
		best.hasRealData = true
	}
	best.UDPAddr = &net.UDPAddr{IP: addr.IP, Port: addr.Port}
	clientID := best.ClientID
	ip := addr.IP.String()
	e.peersMu.Unlock()

	if e.relayMgr == nil {
		return
	}

	if !isRealData {
		e.sendProbeTo(addr)
	}

	state := e.relayMgr.GetState(clientID)
	if isRealData {
		if state != ConnP2P {
			log.Printf("[P2P] 从 %s (%s) 收到真实数据帧，升级为 P2P", clientID, ip)
			e.relayMgr.MarkP2P(clientID)
		}
		return
	}
	if state != ConnP2P {
		log.Printf("[P2P] 从 %s (%s) 收到打洞探测，UDP 通道可用，升级为 P2P", clientID, ip)
		e.relayMgr.MarkP2P(clientID)
	}
}

func (e *Edge) sendProbeTo(addr *net.UDPAddr) {
	if e.udpConn == nil || addr == nil {
		return
	}
	target := &net.UDPAddr{IP: addr.IP, Port: addr.Port}
	vip := e.virtualIP

	safeGo("probe-reply", func() {
		probe := buildPunchProbe(vip)
		for i := 0; i < 5; i++ {
			select {
			case <-e.doneCh:
				return
			default:
			}
			if _, err := e.udpConn.WriteToUDP(probe, target); err != nil {
				return
			}
			time.Sleep(100 * time.Millisecond)
		}
	})
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
