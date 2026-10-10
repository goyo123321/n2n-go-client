package main

import (
	"context"
	"encoding/json"
	"fmt"
	"log"
	"net"
	"net/http"
	"net/url"
	"runtime/debug"
	"strings"
	"sync"
	"time"

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
	stopOnce     sync.Once // ★ 新增：stopCh 只关一次
	permissions  map[string]bool
	alive        bool
}

// ============ 辅助函数 ============

func isNetworkUnreachable(err error) bool {
	if err == nil {
		return false
	}
	s := err.Error()
	return strings.Contains(s, "network is unreachable") ||
		strings.Contains(s, "no route to host") ||
		strings.Contains(s, "network is down")
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

func (tc *TURNClient) IsReady() bool {
	tc.mu.RLock()
	defer tc.mu.RUnlock()
	return tc.alive && tc.relayConn != nil
}

func (tc *TURNClient) markDead(reason string) {
	tc.mu.Lock()
	wasAlive := tc.alive
	tc.alive = false
	client := tc.client
	conn := tc.relayConn
	tc.mu.Unlock()

	if !wasAlive {
		return
	}
	log.Printf("[TURN] 标记失效（%s），等待重建", reason)

	if client != nil {
		client.Close()
	}
	if conn != nil {
		_ = conn.Close()
	}
}

// ============ 请求凭证并建立分配 ============

func (tc *TURNClient) FetchAndSetup(ctx context.Context) (err error) {
	// ★ 加 recover：从 StartReconnectLoop / turn-init 两处调用，
	//   任一路径的 panic 都不该崩穿进程
	defer func() {
		if r := recover(); r != nil {
			log.Printf("[TURN] FetchAndSetup panic: %v\n%s", r, debug.Stack())
			err = fmt.Errorf("panic: %v", r)
		}
	}()

	if tc.IsReady() {
		return nil
	}

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

	turnAddr := srv.URL
	turnAddr = strings.TrimPrefix(turnAddr, "turn://")
	turnAddr = strings.TrimPrefix(turnAddr, "turns://")
	turnAddr = strings.TrimPrefix(turnAddr, "turn:")
	turnAddr = strings.TrimPrefix(turnAddr, "turns:")
	turnAddr = strings.TrimPrefix(turnAddr, "//")

	log.Printf("[TURN] 连接 TURN 服务器: %s (user=%s)", turnAddr, srv.Username)

	conn, err := net.ListenPacket("udp4", "0.0.0.0:0")
	if err != nil {
		return fmt.Errorf("listen for TURN: %w", err)
	}

	cfg := &turn.ClientConfig{
		TURNServerAddr: turnAddr,
		Conn:           conn,
		Username:       srv.Username,
		Password:       srv.Password,
	}
	if strings.Contains(strings.ToLower(srv.URL), "cloudflare") {
		cfg.Realm = "cloudflare"
	}

	turnClient, err := turn.NewClient(cfg)
	if err != nil {
		_ = conn.Close()
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
	tc.alive = true
	tc.mu.Unlock()

	log.Printf("[TURN] 中继地址: %s", tc.relayAddr)

	// ★ 用 safeGo 起 readLoop：panic 时标记失效，让重连循环重建
	safeGo("turn-readLoop", tc.readLoop)
	return nil
}

// ============ 收发 ============

func (tc *TURNClient) readLoop() {
	defer func() {
		if r := recover(); r != nil {
			log.Printf("[TURN] readLoop panic: %v\n%s", r, debug.Stack())
			tc.markDead("readLoop panic")
		}
	}()

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
			if isNetworkUnreachable(err) {
				tc.markDead("read failed: network unreachable")
				return
			}
			log.Printf("[TURN] 读取错误: %v", err)
			return
		}
		if tc.onMessage != nil && n > 0 {
			data := make([]byte, n)
			copy(data, buf[:n])
			// ★ 独立 recover：即使调用方没包 recover，也不会崩穿 readLoop
			func() {
				defer func() {
					if r := recover(); r != nil {
						log.Printf("[TURN] onMessage panic: %v\n%s", r, debug.Stack())
					}
				}()
				tc.onMessage(data, addr)
			}()
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
		if isNetworkUnreachable(err) {
			tc.markDead("CreatePermission failed: network unreachable")
		}
		return fmt.Errorf("CreatePermission 失败: %w", err)
	}
	_, err := conn.WriteTo(data, remoteAddr)
	if err != nil && isNetworkUnreachable(err) {
		tc.markDead("write failed: network unreachable")
	}
	return err
}

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

// ============ 后台重连循环 ============

func (tc *TURNClient) StartReconnectLoop() {
	safeGo("turn-reconnect", func() {
		ticker := time.NewTicker(30 * time.Second)
		defer ticker.Stop()
		for {
			select {
			case <-tc.stopCh:
				return
			case <-ticker.C:
				if tc.IsReady() {
					continue
				}

				log.Printf("[TURN] 检测到分配失效，尝试重建")
				ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
				err := tc.FetchAndSetup(ctx)
				cancel()
				if err != nil {
					log.Printf("[TURN] 重建失败: %v（30 秒后重试）", err)
					continue
				}

				log.Printf("[TURN] ✅ 重建成功: %s", tc.GetRelayAddr())

				if tc.edge != nil && tc.edge.ws != nil {
					_ = tc.edge.ws.Send(map[string]interface{}{
						"type":      "turn_relay_info",
						"relayAddr": tc.GetRelayAddr(),
					})
				}

				if tc.edge != nil && tc.edge.relayMgr != nil {
					tc.edge.relayMgr.UpgradeRelaysToTURN()
				}
			}
		}
	})
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

// ============ 关闭 ============

func (tc *TURNClient) Close() {
	// ★ sync.Once 保证 stopCh 只关一次：
	//   多个 goroutine 同时调 Close 不会 panic: close of closed channel
	tc.stopOnce.Do(func() {
		close(tc.stopCh)
	})

	tc.mu.Lock()
	defer tc.mu.Unlock()

	if tc.client != nil {
		tc.client.Close()
		tc.client = nil
	}
	if tc.relayConn != nil {
		_ = tc.relayConn.Close()
		tc.relayConn = nil
	}
	tc.alive = false
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
