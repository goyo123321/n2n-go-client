package main

import (
	"context"
	"encoding/json"
	"fmt"
	"log"
	"net"
	"net/http"
	"net/url"
	"strings"
	"sync"

	"github.com/pion/turn/v4"
)

// ============ 数据结构 ============

type TURNServerInfo struct {
	URL      string `json:"url"`
	Username string `json:"username"`
	Password string `json:"password"`
	TTL      uint32 `json:"ttl"`
}

type TURNResponse struct {
	Success bool             `json:"success"`
	Source  string           `json:"source,omitempty"`
	Servers []TURNServerInfo `json:"servers"`
	Error   string           `json:"error,omitempty"`
}

type TURNClient struct {
	mu           sync.RWMutex
	client       *turn.Client
	relayConn    net.PacketConn
	relayAddr    net.Addr
	server       *TURNServerInfo
	signalingURL string
	connectToken string
	edge         *Edge
	onMessage    func([]byte, net.Addr)
	stopCh       chan struct{}
	permissions  map[string]bool
}

// ============ 构造 ============

func NewTURNClient(signalingURL string, connectToken string, edge *Edge) *TURNClient {
	return &TURNClient{
		signalingURL: signalingURL,
		connectToken: connectToken,
		edge:         edge,
		stopCh:       make(chan struct{}),
		permissions:  make(map[string]bool),
	}
}

// ============ 请求凭证并建立分配 ============

func (tc *TURNClient) FetchAndSetup(ctx context.Context) error {
	// wss:// → https://, ws:// → http://
	httpBase := tc.signalingURL
	if strings.HasPrefix(httpBase, "wss://") {
		httpBase = "https://" + httpBase[len("wss://"):]
	} else if strings.HasPrefix(httpBase, "ws://") {
		httpBase = "http://" + httpBase[len("ws://"):]
	}
	httpBase = strings.TrimRight(httpBase, "/")

	credURL := fmt.Sprintf("%s/api/turn-credentials?ttl=86400", httpBase)
	if tc.connectToken != "" {
		credURL += "&token=" + url.QueryEscape(tc.connectToken)
	}

	log.Printf("[TURN] 请求凭证: %s", redactToken(credURL))

	req, err := http.NewRequestWithContext(ctx, http.MethodGet, credURL, nil)
	if err != nil {
		return fmt.Errorf("create request: %w", err)
	}

	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		return fmt.Errorf("fetch credentials: %w", err)
	}
	defer resp.Body.Close()

	if resp.StatusCode == http.StatusUnauthorized ||
		resp.StatusCode == http.StatusForbidden {
		return fmt.Errorf("turn-credentials 未授权 (CONNECT_TOKEN 不匹配, http=%d)", resp.StatusCode)
	}
	if resp.StatusCode != http.StatusOK {
		return fmt.Errorf("credentials endpoint returned %d", resp.StatusCode)
	}

	var creds TURNResponse
	if err := json.NewDecoder(resp.Body).Decode(&creds); err != nil {
		return fmt.Errorf("decode credentials: %w", err)
	}
	if !creds.Success || len(creds.Servers) == 0 {
		return fmt.Errorf("no TURN servers: %s", creds.Error)
	}

	tc.mu.Lock()
	tc.server = &creds.Servers[0]
	tc.mu.Unlock()

	log.Printf("[TURN] 获取到 %s TURN 服务器: %s", creds.Source, creds.Servers[0].URL)
	return tc.setupAllocation(ctx)
}

// ============ 建立 TURN Allocation ============

func (tc *TURNClient) setupAllocation(ctx context.Context) error {
	tc.mu.RLock()
	srv := tc.server
	tc.mu.RUnlock()
	if srv == nil {
		return fmt.Errorf("TURN server not set")
	}

	// pion/turn 的 TURNServerAddr 只接受 "host:port" 格式
	turnAddr := srv.URL
	turnAddr = strings.TrimPrefix(turnAddr, "turn://")
	turnAddr = strings.TrimPrefix(turnAddr, "turns://")
	turnAddr = strings.TrimPrefix(turnAddr, "turn:")
	turnAddr = strings.TrimPrefix(turnAddr, "turns:")
	turnAddr = strings.TrimPrefix(turnAddr, "//")

	log.Printf("[TURN] 连接 TURN 服务器: %s (user=%s)", turnAddr, srv.Username)

	// ★ 修复：pion/turn v4 要求调用方提供底层 UDP conn，不再自动创建。
	// 之前没设 cfg.Conn，导致 turn.NewClient 报 "turn: conn cannot not be nil"。
	//
	// 这个 conn 会被 turn.Client 接管，turnClient.Close() 时会一并关闭。
	// 只在创建失败路径上需要手动关。
	conn, err := net.ListenPacket("udp4", "0.0.0.0:0")
	if err != nil {
		return fmt.Errorf("listen for TURN: %w", err)
	}

	cfg := &turn.ClientConfig{
		TURNServerAddr: turnAddr,
		Conn:           conn, // ★ 关键：把 conn 交给 turn.Client
		Username:       srv.Username,
		Password:       srv.Password,
	}
	// Cloudflare 官方 TURN 使用 realm=cloudflare；自建 coturn 一般交给服务端下发
	if strings.Contains(strings.ToLower(srv.URL), "cloudflare") {
		cfg.Realm = "cloudflare"
	}

	turnClient, err := turn.NewClient(cfg)
	if err != nil {
		conn.Close() // ★ 创建失败，需要手动释放
		return fmt.Errorf("create TURN client: %w", err)
	}

	if err := turnClient.Listen(); err != nil {
		turnClient.Close()
		return fmt.Errorf("listen: %w", err)
	}

	relayConn, err := turnClient.Allocate()
	if err != nil {
		turnClient.Close()
		return fmt.Errorf("allocate: %w", err)
	}

	tc.mu.Lock()
	tc.client = turnClient
	tc.relayConn = relayConn
	tc.relayAddr = relayConn.LocalAddr()
	tc.mu.Unlock()

	log.Printf("[TURN] 中继地址: %s", tc.relayAddr)
	go tc.readLoop()
	return nil
}

// ============ 收发 ============

func (tc *TURNClient) readLoop() {
	buf := make([]byte, 65535)
	for {
		select {
		case <-tc.stopCh:
			return
		default:
		}

		tc.mu.RLock()
		conn := tc.relayConn
		tc.mu.RUnlock()
		if conn == nil {
			return
		}

		n, addr, err := conn.ReadFrom(buf)
		if err != nil {
			select {
			case <-tc.stopCh:
				return
			default:
			}
			log.Printf("[TURN] 读取错误: %v", err)
			return
		}
		if tc.onMessage != nil && n > 0 {
			data := make([]byte, n)
			copy(data, buf[:n])
			tc.onMessage(data, addr)
		}
	}
}

func (tc *TURNClient) Send(data []byte, remoteAddr net.Addr) error {
	tc.mu.RLock()
	conn := tc.relayConn
	tc.mu.RUnlock()
	if conn == nil {
		return fmt.Errorf("TURN 未就绪")
	}
	if err := tc.ensurePermission(remoteAddr); err != nil {
		return fmt.Errorf("CreatePermission 失败: %w", err)
	}
	_, err := conn.WriteTo(data, remoteAddr)
	return err
}

// ensurePermission 确保向 remoteAddr 的发送已被 TURN 服务器授权。
//
// TURN 协议（RFC 5766）要求客户端向某个对端地址发送数据前，必须先
// 用 CreatePermission 在服务器上建立对该 IP 的权限。pion/turn 的 Client
// 不会自动处理这一步，漏掉的话 WriteTo 会被服务器静默丢弃。
//
// CreatePermission 接受 net.Addr（完整地址含端口），不是 net.IP。
// *net.UDPAddr 实现了 net.Addr 接口，所以直接传 udpAddr 即可。
func (tc *TURNClient) ensurePermission(remoteAddr net.Addr) error {
	udpAddr, ok := remoteAddr.(*net.UDPAddr)
	if !ok {
		return fmt.Errorf("remoteAddr 不是 UDPAddr: %T", remoteAddr)
	}
	key := udpAddr.IP.String()

	tc.mu.RLock()
	client := tc.client
	already := tc.permissions[key]
	tc.mu.RUnlock()

	if already {
		return nil
	}
	if client == nil {
		return fmt.Errorf("TURN client 未就绪")
	}

	if err := client.CreatePermission(udpAddr); err != nil {
		return err
	}

	tc.mu.Lock()
	tc.permissions[key] = true
	tc.mu.Unlock()
	return nil
}

// ============ 状态查询 ============

func (tc *TURNClient) GetRelayAddr() string {
	tc.mu.RLock()
	defer tc.mu.RUnlock()
	if tc.relayAddr == nil {
		return ""
	}
	return tc.relayAddr.String()
}

func (tc *TURNClient) IsReady() bool {
	tc.mu.RLock()
	defer tc.mu.RUnlock()
	return tc.relayConn != nil
}

// ============ 关闭 ============

func (tc *TURNClient) Close() {
	select {
	case <-tc.stopCh:
	default:
		close(tc.stopCh)
	}

	tc.mu.Lock()
	defer tc.mu.Unlock()
	if tc.client != nil {
		tc.client.Close()
		tc.client = nil
	}
	if tc.relayConn != nil {
		tc.relayConn.Close()
		tc.relayConn = nil
	}
	tc.permissions = make(map[string]bool)
}

// ============ 辅助 ============

func redactToken(raw string) string {
	u, err := url.Parse(raw)
	if err != nil {
		return raw
	}
	q := u.Query()
	if q.Get("token") != "" {
		q.Set("token", "***")
		u.RawQuery = q.Encode()
	}
	return u.String()
}
