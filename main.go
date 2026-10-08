package main

import (
	"context"
	"encoding/json"
	"fmt"
	"log"
	"net"
	"os"
	"os/signal"
	"path/filepath"
	"strings"
	"sync"
	"syscall"
	"time"
)

var (
	BuildVersion        = "dev"
	DefaultSignalingURL = ""
)

type PeerInfo struct {
	ClientID      string
	VirtualIP     string
	PubIP         string
	PubPort       int
	TurnRelayAddr string
	UDPAddr       *net.UDPAddr
	lastRecvAt    int64
	loggedReady   bool
	// hasRealData 记录是否从这个 peer 收到过真实数据帧（非探测包）。
	// 只有收到真实数据帧才能升级为 P2P——收到探测包只说明对方在打洞。
	hasRealData bool
}

type Edge struct {
	clientId  string
	nodeName  string
	virtualIP string
	roomId    string

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
}

func main() {
	signalingURL := getEnv("SIGNALING_URL", DefaultSignalingURL)
	roomId := getEnv("ROOM_ID", "default-room")
	clientId := getEnv("CLIENT_ID", "")
	nodeName := getEnv("NODE_NAME", "")
	tunName := getEnv("TUN_NAME", "n2n0")
	// ★ UDP_PORT 默认 0：让内核分配端口，避免同机多实例冲突
	udpPort := getEnvInt("UDP_PORT", 0)
	stunServers := getEnv("STUN_SERVERS", "")
	connectToken := getEnv("CONNECT_TOKEN", "")

	if signalingURL == "" {
		log.Fatal("请设置 SIGNALING_URL")
	}

	// CLIENT_ID 持久化到文件
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
		clientId:   clientId,
		nodeName:   nodeName,
		roomId:     roomId,
		peers:      make(map[string]*PeerInfo),
		tunWriteCh: make(chan []byte, 1024),
		udpPort:    udpPort,
	}

	// ★ UDP_PORT=0 时内核分配；显式端口被占用时回退到内核分配
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
	log.Printf("[P2P] UDP 监听端口 %d", actualPort)

	servers := []string{}
	if stunServers != "" {
		servers = splitCSV(stunServers)
	}
	edge.natMeta = probeNAT(actualPort, servers)

	ws, err := NewWSTransport(signalingURL, roomId, clientId, connectToken)
	if err != nil {
		log.Fatalf("连接信令失败: %v", err)
	}
	defer ws.Close()
	edge.ws = ws
	log.Printf("已连接信令，Client ID: %s，节点名: %s", clientId, nodeName)
	ws.StartHeartbeat(20 * time.Second)

	// ★ 断线重连后重新上报 metadata
	ws.onReconnect = func() {
		if edge.virtualIP != "" && edge.natMeta != nil {
			metaPayload := map[string]interface{}{
				"name":               edge.nodeName,
				"natType":            edge.natMeta.NATType,
				"portsDifference":    edge.natMeta.PortsDifference,
				"regularPortsChange": edge.natMeta.RegularPortsChange,
				"behavior":           edge.natMeta.Behavior,
				"assistedSockets":    edge.natMeta.AssistedSockets,
				"p2pEndpoint":        edge.natMeta.P2PEndpoint,
			}
			if edge.natMeta.PublicEndpoint != "" {
				metaPayload["publicEndpoint"] = edge.natMeta.PublicEndpoint
			}
			_ = ws.Send(map[string]interface{}{
				"type":    "p2p_metadata",
				"payload": metaPayload,
			})
		}
	}

	edge.turnClient = NewTURNClient(signalingURL, connectToken, edge)
	edge.turnClient.onMessage = func(data []byte, addr net.Addr) {
		edge.enqueueTUN(data)
	}

	edge.relayMgr = NewRelayManager(ws, edge.turnClient, edge)
	edge.relayMgr.Report(10 * time.Second)

	ws.onMessage = edge.handleSignaling

	ws.onBinary = func(data []byte) {
		edge.enqueueTUN(data)
	}

	go func() {
		buf := make([]byte, 65535)
		for {
			n, addr, err := udpConn.ReadFromUDP(buf)
			if err != nil {
				return
			}
			if n < 4 {
				continue
			}
			if buf[0] == 'N' && buf[1] == '2' && buf[2] == 'N' && buf[3] == 'P' {
				// ★ 探测包：只刷新 lastRecvAt，不标记 hasRealData
				edge.notePeerProbe(addr)
				continue
			}
			if buf[0]>>4 == 4 {
				// ★ 真实数据帧：标记 hasRealData
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

	// TURN 异步初始化
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

	sigCh := make(chan os.Signal, 1)
	signal.Notify(sigCh, syscall.SIGINT, syscall.SIGTERM)
	<-sigCh

	log.Println("正在退出...")
	if edge.turnClient != nil {
		edge.turnClient.Close()
	}
	if edge.tun != nil {
		_ = edge.tun.Close()
	}
}

func (e *Edge) printNodeSummary() {
	log.Println("")
	log.Println("================= 本机信息 =================")
	log.Printf("  Client ID   : %s", e.clientId)
	log.Printf("  节点名       : %s", e.nodeName)
	log.Printf("  虚拟 IP     : %s", e.virtualIP)
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
		log.Printf("分配虚拟 IP: %s", e.virtualIP)

		var err error
		e.tun, err = setupTUN(e.virtualIP, getEnv("TUN_NAME", "n2n0"))
		if err != nil {
			log.Printf("[TUN] 启动失败: %v", err)
		} else {
			go e.tunReadLoop()
		}

		metaPayload := map[string]interface{}{
			"name":               e.nodeName, // ★ name 从 share_announce 迁移到这里
			"natType":            e.natMeta.NATType,
			"portsDifference":    e.natMeta.PortsDifference,
			"regularPortsChange": e.natMeta.RegularPortsChange,
			"behavior":           e.natMeta.Behavior,
			"assistedSockets":    e.natMeta.AssistedSockets,
			"p2pEndpoint":        e.natMeta.P2PEndpoint,
		}
		if e.natMeta.PublicEndpoint != "" {
			metaPayload["publicEndpoint"] = e.natMeta.PublicEndpoint
		}
		_ = e.ws.Send(map[string]interface{}{
			"type":    "p2p_metadata",
			"payload": metaPayload,
		})

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
		e.ensureTargetPeer(&instr)
		go e.runNatHole(&instr)

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
	}
}

func (e *Edge) registerPeer(peerID, virtualIP, pubIP string, pubPort int) {
	var udpAddr *net.UDPAddr
	if pubIP != "" && pubPort > 0 {
		udpAddr = &net.UDPAddr{IP: net.ParseIP(pubIP), Port: pubPort}
	}
	e.peersMu.Lock()
	defer e.peersMu.Unlock()
	if existing, ok := e.peers[peerID]; ok {
		if virtualIP != "" {
			existing.VirtualIP = virtualIP
		}
		if pubIP != "" {
			existing.PubIP = pubIP
		}
		if pubPort > 0 {
			existing.PubPort = pubPort
		}
		if udpAddr != nil {
			existing.UDPAddr = udpAddr
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

	_ = e.ws.Send(map[string]interface{}{
		"type": "p2p_state_info",
		"payload": map[string]interface{}{
			"to": []map[string]interface{}{
				{
					"macAddr":            instr.TargetMac,
					"observedRaddr":      "",
					"punchResult":        res,
					"punchResultPeerMac": instr.TargetMac,
				},
			},
		},
	})

	if res.State == PunchStateSucceeded {
		e.relayMgr.MarkP2P(instr.TargetMac)
	} else {
		e.relayMgr.MarkFallback(instr.TargetMac)
	}
}

func (e *Edge) tunReadLoop() {
	buf := make([]byte, 65535)
	for {
		n, err := e.tun.Read(buf)
		if err != nil {
			log.Printf("[TUN] 读取错误: %v", err)
			return
		}
		if n < 20 {
			continue
		}
		if buf[0]>>4 != 4 {
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
			_ = e.ws.SendBinary(buf[:n])
			continue
		}

		ok := e.relayMgr.SendToPeer(target.ClientID, buf[:n], target)
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

// ★ 拆分：探测包和真实数据帧走不同路径
func (e *Edge) notePeerProbe(addr *net.UDPAddr) {
	e.notePeerCommon(addr, false)
}

func (e *Edge) notePeerTraffic(addr *net.UDPAddr) {
	e.notePeerCommon(addr, true)
}

// ★ P0-7：区分探测包和真实数据帧。
// IP 相同但端口不同的宽松匹配只在 isRealData=true 时使用——因为
// 只有真实数据帧才值得更新 UDPAddr。
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
		if bestScore == 1 {
			best.UDPAddr = &net.UDPAddr{IP: addr.IP, Port: addr.Port}
		}
	}
	clientID := best.ClientID
	ip := addr.IP.String()
	realData := best.hasRealData
	e.peersMu.Unlock()

	// ★ P0-7：只有真实数据帧才触发 P2P 升级
	if realData && isRealData {
		e.maybeUpgradeToP2P(clientID, ip)
	}
}

func (e *Edge) maybeUpgradeToP2P(clientID, ip string) {
	if e.relayMgr.ShouldRelay(clientID) {
		log.Printf("[P2P] 从 %s (%s) 收到真实数据帧，升级为 P2P", clientID, ip)
		e.relayMgr.MarkP2P(clientID)
	}
}

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
