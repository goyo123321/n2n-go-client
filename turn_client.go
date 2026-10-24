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
	stopOnce     sync.Once
	permissions  map[string]bool
	alive        bool

	// ★ 新增：Send 连续失败计数，5 次失败主动 markDead
	consecutiveSendFails int
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
	tc.consecutiveSendFails = 0
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

	safeGo("turn-readLoop", tc.readLoop)
	return nil
}

// RebuildAllocation 网络变化后重建 TURN allocation。
//
// 保留 tc.server（TURN 凭证 24 小时内有效），避免重复调
// /api/turn-credentials 消耗 Worker 配额。
func (tc *TURNClient) RebuildAllocation(ctx context.Context) error {
	tc.mu.Lock()
	oldClient := tc.client
	oldConn := tc.relayConn
	tc.client = nil
	tc.relayConn = nil
	tc.relayAddr = nil
	tc.alive = false
	tc.consecutiveSendFails = 0
	tc.permissions = make(map[string]bool)
	serverCached := tc.server
	tc.mu.Unlock()

	if oldClient != nil {
		oldClient.Close()
	}
	if oldConn != nil {
		_ = oldConn.Close()
	}

	if serverCached == nil {
		return tc.FetchAndSetup(ctx)
	}
	return tc.setupAllocation(ctx)
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
	if err != nil {
		if isNetworkUnreachable(err) {
			tc.markDead("write failed: network unreachable")
		} else {
			// ★ 新增：连续失败计数，5 次失败主动 markDead
			tc.mu.Lock()
			tc.consecutiveSendFails++
			fails := tc.consecutiveSendFails
			tc.mu.Unlock()
			if fails >= 5 {
				log.Printf("[TURN] Send 连续失败 %d 次，主动标记失效", fails)
				tc.markDead("send fails")
			}
		}
		return err
	}
	tc.mu.Lock()
	tc.consecutiveSendFails = 0
	tc.mu.Unlock()
	return nil
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

// ping 主动发送 CreatePermission 验证 TURN 服务器活着。
//
// CreatePermission 是同步阻塞的（pion 内部有重传），用 goroutine + ctx
// 包装，超时后返回 ctx.Err()，不阻塞 StartReconnectLoop 的下一轮。
func (tc *TURNClient) ping(ctx context.Context) error {
	tc.mu.RLock()
	client := tc.client
	tc.mu.RUnlock()
	if client == nil {
		return fmt.Errorf("TURN client nil")
	}

	done := make(chan error, 1)
	go func() {
		defer func() {
			if r := recover(); r != nil {
				done <- fmt.Errorf("panic: %v", r)
			}
		}()
		dummyAddr := &net.UDPAddr{IP: net.ParseIP("1.1.1.1"), Port: 53}
		done <- client.CreatePermission(dummyAddr)
	}()

	select {
	case err := <-done:
		return err
	case <-ctx.Done():
		return ctx.Err()
	}
}

// ============ 后台重连循环 ============

func (tc *TURNClient) StartReconnectLoop() {
	safeGo("turn-reconnect", func() {
		consecutiveFails := 0
		lastPingAt := time.Now()
		for {
			// 5s 周期检测；重建失败时指数退避：5s → 10s → 20s → 40s → ... 封顶 5min
			wait := 5 * time.Second
			if consecutiveFails > 0 {
				wait = time.Duration(5<<uint(consecutiveFails-1)) * time.Second
				if wait > 5*time.Minute {
					wait = 5 * time.Minute
				}
			}

			select {
			case <-tc.stopCh:
				return
			case <-time.After(wait):
			}

			if tc.IsReady() {
				consecutiveFails = 0

				// ★ 新增：每 30s 主动 ping TURN（验证服务器活着）
				if time.Since(lastPingAt) >= 30*time.Second {
					lastPingAt = time.Now()
					ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
					err := tc.ping(ctx)
					cancel()
					if err != nil {
						log.Printf("[TURN] 主动 ping 失败: %v，标记失效", err)
						tc.markDead("ping failed")
					}
				}
				continue
			}

			log.Printf("[TURN] 检测到分配失效，尝试重建 (失败 %d 次)", consecutiveFails)
			ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
			err := tc.FetchAndSetup(ctx)
			cancel()
			if err != nil {
				consecutiveFails++
				log.Printf("[TURN] 重建失败: %v", err)
				continue
			}

			consecutiveFails = 0
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
