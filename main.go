package main

import (
	"context"
	"encoding/json"
	"fmt"
	"log"
	"net"
	"net/http"
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
	SharePort     int
	TurnRelayAddr string
	UDPAddr       *net.UDPAddr
	lastRecvAt    int64
	loggedReady   bool
}

type Edge struct {
	clientId  string
	nodeName  string
	virtualIP string
	roomId    string

	ws          *WSTransport
	relayMgr    *RelayManager
	turnClient  *TURNClient
	tun         *TUNDevice
	shareServer *ShareServer
	registry    *ShareRegistry

	udpConn *net.UDPConn
	udpPort int

	peers   map[string]*PeerInfo
	peersMu sync.RWMutex

	tunWriteCh chan []byte

	natMeta *NATMetadata
}

func main() {
	initShareServerPort()

	signalingURL := getEnv("SIGNALING_URL", DefaultSignalingURL)
	roomId := getEnv("ROOM_ID", "default-room")
	clientId := getEnv("CLIENT_ID", "")
	nodeName := getEnv("NODE_NAME", "")
	shareDir := getEnv("SHARE_DIR", "./shared")
	tunName := getEnv("TUN_NAME", "n2n0")
	udpPort := getEnvInt("UDP_PORT", 50001)
	stunServers := getEnv("STUN_SERVERS", "")
	connectToken := getEnv("CONNECT_TOKEN", "")

	if signalingURL == "" {
		log.Fatal("请设置 SIGNALING_URL")
	}

	// ★ 修复：CLIENT_ID 持久化到文件，重启复用，避免虚拟 IP 漂移
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
	_ = shareDir
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

	udpAddr := &net.UDPAddr{IP: net.IPv4zero, Port: udpPort}
	udpConn, err := net.ListenUDP("udp", udpAddr)
	if err != nil {
		log.Fatalf("绑定 UDP %d 失败: %v", udpPort, err)
	}
	edge.udpConn = udpConn
	defer udpConn.Close()
	log.Printf("[P2P] UDP 监听端口 %d", udpPort)

	servers := []string{}
	if stunServers != "" {
		servers = splitCSV(stunServers)
	}
	edge.natMeta = probeNAT(udpPort, servers)

	ws, err := NewWSTransport(signalingURL, roomId, clientId, connectToken)
	if err != nil {
		log.Fatalf("连接信令失败: %v", err)
	}
	defer ws.Close()
	edge.ws = ws
	log.Printf("已连接信令，Client ID: %s，节点名: %s", clientId, nodeName)
	ws.StartHeartbeat(20 * time.Second)

	edge.turnClient = NewTURNClient(signalingURL, connectToken, edge)
	edge.turnClient.onMessage = func(data []byte, addr net.Addr) {
		edge.enqueueTUN(data)
	}

	edge.relayMgr = NewRelayManager(ws, edge.turnClient, edge)
	edge.relayMgr.Report(10 * time.Second)

	edge.registry = NewShareRegistry()
	go func() {
		mux := http.NewServeMux()
		mux.Handle("/", edge.registry)
		log.Printf("[发现] 本地面板: http://localhost:9091/api/nodes")
		_ = http.ListenAndServe("127.0.0.1:9091", mux)
	}()

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
				edge.notePeerTraffic(addr)
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

	// TURN 异步初始化
	go func() {
		time.Sleep(2 * time.Second)
		ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
		defer cancel()
		if err := edge.turnClient.FetchAndSetup(ctx); err != nil {
			log.Printf("[TURN] 初始化失败: %v（将使用 WS 中继兜底）", err)
		} else {
			log.Printf("[TURN] TURN 中继就绪: %s", edge.turnClient.GetRelayAddr())
			// ★ 修复：TURN 就绪后，把仍然走 WS 中继的 peer 升级到 TURN
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
	if edge.shareServer != nil {
		_ = edge.shareServer.Stop()
	}
	if edge.tun != nil {
		_ = edge.tun.Close()
	}
	_ = ws.Send(map[string]interface{}{
		"type":    "share_withdraw",
		"payload": map[string]interface{}{"virtualIp": edge.virtualIP},
	})
}

func (e *Edge) printNodeSummary() {
	addr := fmt.Sprintf("http://%s:%d/", e.virtualIP, ShareServerPort)
	webdav := fmt.Sprintf("http://%s:%d/webdav/", e.virtualIP, ShareServerPort)
	shareDir := getEnv("SHARE_DIR", "./shared")

	log.Println("")
	log.Println("================= 本机信息 =================")
	log.Printf("  Client ID   : %s", e.clientId)
	log.Printf("  节点名       : %s", e.nodeName)
	log.Printf("  虚拟 IP     : %s", e.virtualIP)
	log.Printf("  P2P 端口    : %d", e.udpPort)
	log.Printf("  共享盘端口   : %d", ShareServerPort)
	log.Printf("  共享盘地址   : %s", addr)
	log.Printf("  WebDAV      : %s", webdav)
	log.Printf("  共享目录     : %s", shareDir)
	log.Println("==========================================")
	log.Println("")
}

func (e *Edge) logPeerReady(peerID string) {
	if peerID == "" {
		return
	}
	e.peersMu.Lock()
	p, ok := e.peers[peerID]
	if !ok || p.loggedReady || p.SharePort <= 0 {
		e.peersMu.Unlock()
		return
	}
	p.loggedReady = true
	vip := p.VirtualIP
	pubIP := p.PubIP
	pubPort := p.PubPort
	sharePort := p.SharePort
	clientID := p.ClientID
	e.peersMu.Unlock()

	log.Println("")
	log.Printf("========== 节点就绪: %s ==========", clientID)
	log.Printf("  虚拟 IP   : %s", vip)
	if pubIP != "" && pubPort > 0 {
		log.Printf("  公网地址  : %s:%d", pubIP, pubPort)
	}
	log.Printf("  共享盘端口 : %d", sharePort)
	log.Printf("  共享盘地址 : http://%s:%d/", vip, sharePort)
	log.Printf("  WebDAV    : http://%s:%d/webdav/", vip, sharePort)
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

		e.shareServer = NewShareServer(
			getEnv("SHARE_DIR", "./shared"),
			e.virtualIP,
			e.nodeName,
		)
		if err := e.shareServer.Start(); err != nil {
			log.Printf("[共享盘] 启动失败: %v", err)
		}

		metaPayload := map[string]interface{}{
			"natType":            e.natMeta.NATType,
			"portsDifference":    e.natMeta.PortsDifference,
			"regularPortsChange": e.natMeta.RegularPortsChange,
			"behavior":           e.natMeta.Behavior,
			"assistedSockets":    e.natMeta.AssistedSockets,
			"sharePort":          ShareServerPort,
			"p2pEndpoint":        e.natMeta.P2PEndpoint,
		}
		if e.natMeta.PublicEndpoint != "" {
			metaPayload["publicEndpoint"] = e.natMeta.PublicEndpoint
		}
		_ = e.ws.Send(map[string]interface{}{
			"type":    "p2p_metadata",
			"payload": metaPayload,
		})

		_ = e.ws.Send(map[string]interface{}{
			"type": "share_announce",
			"payload": map[string]interface{}{
				"name":      e.nodeName,
				"virtualIp": e.virtualIP,
				"port":      ShareServerPort,
			},
		})

		if shares, ok := payload["shares"].([]interface{}); ok {
			for _, s := range shares {
				sm, ok := s.(map[string]interface{})
				if !ok {
					continue
				}
				sid, _ := sm["id"].(string)
				sname, _ := sm["name"].(string)
				svip, _ := sm["virtualIp"].(string)
				sport := ShareServerPort
				if p, ok := sm["port"].(float64); ok {
					sport = int(p)
				}
				e.registry.Register(sid, sname, svip, sport)
			}
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
				sharePort := 0
				if f, ok := pm["sharePort"].(float64); ok {
					sharePort = int(f)
				}
				relayAddr, _ := pm["turnRelayAddr"].(string) // ★ 修复
				if pid == "" || pip == "" {
					continue
				}
				e.registerPeer(pid, pip, pubIP, pubPort, sharePort)
				// ★ 修复：记录对端的 TURN 中继地址
				if relayAddr != "" {
					e.peersMu.Lock()
					if pi, ok := e.peers[pid]; ok {
						pi.TurnRelayAddr = relayAddr
					}
					e.peersMu.Unlock()
				}
				e.logPeerReady(pid)
				go e.discoverPeer(pid, pip, sharePort)
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
		sharePort := 0
		if f, ok := payload["sharePort"].(float64); ok {
			sharePort = int(f)
		}
		relayAddr, _ := payload["turnRelayAddr"].(string) // ★ 修复
		if from != "" && pip != "" {
			e.peersMu.RLock()
			p, exists := e.peers[from]
			alreadyReady := exists && p.loggedReady
			e.peersMu.RUnlock()

			e.registerPeer(from, pip, pubIP, pubPort, sharePort)
			// ★ 修复：记录对端 TURN 中继地址
			if relayAddr != "" {
				e.peersMu.Lock()
				if pi, ok := e.peers[from]; ok {
					pi.TurnRelayAddr = relayAddr
				}
				e.peersMu.Unlock()
			}

			if !alreadyReady && sharePort <= 0 {
				log.Printf("[信令] 节点上线: %s (vip=%s)", from, pip)
			}

			e.logPeerReady(from)
			if sharePort > 0 {
				go e.discoverPeer(from, pip, sharePort)
			}
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

	case "share_announce":
		payload, _ := msg["payload"].(map[string]interface{})
		name, _ := payload["name"].(string)
		vip, _ := payload["virtualIp"].(string)
		port := ShareServerPort
		if p, ok := payload["port"].(float64); ok {
			port = int(p)
		}
		e.registry.Register(from, name, vip, port)
		e.registerPeer(from, vip, "", 0, port)
		e.logPeerReady(from)
		if from != "" && vip != "" {
			go e.discoverPeer(from, vip, port)
		}

	// 收到其他 Peer 的 TURN 中继地址
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
		// ★ 修复：接收服务端 pong（当前仅忽略）
		return

	case "left":
		log.Printf("[信令] 节点离开: %s", from)
		e.registry.Unregister(from)
		e.peersMu.Lock()
		delete(e.peers, from)
		e.peersMu.Unlock()

	case "share_withdraw":
		e.registry.Unregister(from)
	}
}

func (e *Edge) registerPeer(peerID, virtualIP, pubIP string, pubPort, sharePort int) {
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
		if sharePort > 0 {
			existing.SharePort = sharePort
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
			SharePort: sharePort,
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
		// 打洞失败：优先 TURN，TURN 不可用则 WS
		e.relayMgr.MarkFallback(instr.TargetMac)
	}
}

func (e *Edge) discoverPeer(peerID, virtualIP string, sharePort int) {
	e.peersMu.Lock()
	if p, ok := e.peers[peerID]; ok {
		if sharePort > 0 {
			p.SharePort = sharePort
		} else {
			sharePort = p.SharePort
		}
	} else {
		e.peers[peerID] = &PeerInfo{
			ClientID:  peerID,
			VirtualIP: virtualIP,
			SharePort: sharePort,
		}
	}
	e.peersMu.Unlock()

	port := sharePort
	if port <= 0 {
		port = e.registry.GetPort(peerID)
	}
	if port <= 0 {
		port = ShareServerPort
	}

	for i := 0; i < 3; i++ {
		node, err := DiscoverRemoteNode(virtualIP, port)
		if err == nil {
			e.registry.Register(peerID, node.NodeName, virtualIP, node.Port)
			break
		}
		time.Sleep(2 * time.Second)
	}

	if e.relayMgr.GetState(peerID) == ConnUnknown {
		e.relayMgr.MarkFallback(peerID)
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

		// 三级降级：P2P → TURN → WS
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

// ★ 修复：合并双次加锁解锁为单次评分遍历，可读性更好
func (e *Edge) notePeerTraffic(addr *net.UDPAddr) {
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
	if bestScore == 1 {
		best.UDPAddr = &net.UDPAddr{IP: addr.IP, Port: addr.Port}
	}
	clientID := best.ClientID
	ip := addr.IP.String()
	e.peersMu.Unlock()

	e.maybeUpgradeToP2P(clientID, ip)
}

func (e *Edge) maybeUpgradeToP2P(clientID, ip string) {
	if e.relayMgr.ShouldRelay(clientID) {
		log.Printf("[P2P] 从 %s (%s) 收到 UDP 包，自动升级为 P2P", clientID, ip)
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
