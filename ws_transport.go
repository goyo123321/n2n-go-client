package main

import (
	"encoding/json"
	"log"
	"net/url"
	"strings"
	"sync"
	"time"

	"github.com/gorilla/websocket"
)

type WSTransport struct {
	conn      *websocket.Conn
	mu        sync.Mutex
	clientId  string
	onMessage func(map[string]interface{})
	onBinary  func([]byte)

	// ★ 新增：重连相关
	fullURL      string
	dialer       *websocket.Dialer
	stopCh       chan struct{}
	reconnectMu  sync.Mutex
	stopping     bool
	heartbeatCh  chan struct{}
}

// NewWSTransport 建立 WebSocket 连接。
// connectToken 为空时不加 token 参数（Worker 未启用校验）；
// 非空时附加 &token=xxx。
func NewWSTransport(signalingURL, roomId, clientId, connectToken string) (*WSTransport, error) {
	base := strings.TrimRight(signalingURL, "/")
	// ★ 修复：ROOM_ID 做 URL PathEscape，避免 "a/b" 之类的路径歧义
	fullURL := base + "/ws/" + url.PathEscape(roomId) + "?cid=" + url.QueryEscape(clientId)
	if connectToken != "" {
		fullURL += "&token=" + url.QueryEscape(connectToken)
	}

	log.Printf("[WS] 连接 %s", maskToken(fullURL))

	dialer := &websocket.Dialer{
		HandshakeTimeout: 10 * time.Second,
	}

	conn, _, err := dialer.Dial(fullURL, nil)
	if err != nil {
		return nil, err
	}

	ws := &WSTransport{
		conn:        conn,
		clientId:    clientId,
		fullURL:     fullURL,
		dialer:      dialer,
		stopCh:      make(chan struct{}),
		heartbeatCh: make(chan struct{}),
	}
	go ws.readLoop()
	go ws.heartbeat(20 * time.Second)
	return ws, nil
}

// maskToken 打日志时把 token 值用 *** 替换，避免泄露
func maskToken(u string) string {
	if !strings.Contains(u, "token=") {
		return u
	}
	parts := strings.Split(u, "token=")
	if len(parts) != 2 {
		return u
	}
	return parts[0] + "token=***"
}

func (ws *WSTransport) readLoop() {
	for {
		msgType, data, err := ws.conn.ReadMessage()
		if err != nil {
			log.Printf("[WS] 读取错误: %v", err)

			ws.reconnectMu.Lock()
			stopping := ws.stopping
			ws.reconnectMu.Unlock()

			if stopping {
				return
			}
			go ws.tryReconnect()
			return
		}
		if msgType == websocket.TextMessage {
			var msg map[string]interface{}
			if err := json.Unmarshal(data, &msg); err != nil {
				continue
			}
			if ws.onMessage != nil {
				ws.onMessage(msg)
			}
		} else if msgType == websocket.BinaryMessage {
			if ws.onBinary != nil {
				ws.onBinary(data)
			}
		}
	}
}

// ★ 新增：重连逻辑，10 次指数退避，参考 Android 端
func (ws *WSTransport) tryReconnect() {
	ws.reconnectMu.Lock()
	defer ws.reconnectMu.Unlock()
	if ws.stopping {
		return
	}

	delays := []time.Duration{1, 2, 5, 10, 30, 60, 60, 60, 60, 60}
	for i, d := range delays {
		select {
		case <-ws.stopCh:
			return
		case <-time.After(d * time.Second):
		}
		log.Printf("[WS] 重连 (%d/10)...", i+1)

		conn, _, err := ws.dialer.Dial(ws.fullURL, nil)
		if err != nil {
			log.Printf("[WS] 重连失败: %v", err)
			continue
		}
		ws.mu.Lock()
		ws.conn = conn
		ws.mu.Unlock()
		log.Printf("[WS] ✅ 重连成功")

		go ws.readLoop()
		go ws.heartbeat(20 * time.Second)

		// 通知上层：重连成功，需要重新上报元数据
		if ws.onMessage != nil {
			ws.onMessage(map[string]interface{}{"type": "_reconnected"})
		}
		return
	}

	log.Printf("[WS] 重连 10 次全部失败，放弃")
}

func (ws *WSTransport) heartbeat(interval time.Duration) {
	ticker := time.NewTicker(interval)
	defer ticker.Stop()
	for {
		select {
		case <-ticker.C:
			if err := ws.Send(map[string]interface{}{
				"type": "ping",
				"ts":   time.Now().Unix(),
			}); err != nil {
				return
			}
		case <-ws.stopCh:
			return
		}
	}
}

func (ws *WSTransport) Send(msg map[string]interface{}) error {
	ws.mu.Lock()
	defer ws.mu.Unlock()
	data, _ := json.Marshal(msg)
	return ws.conn.WriteMessage(websocket.TextMessage, data)
}

func (ws *WSTransport) SendBinary(data []byte) error {
	ws.mu.Lock()
	defer ws.mu.Unlock()
	return ws.conn.WriteMessage(websocket.BinaryMessage, data)
}

// StartHeartbeat 保留兼容旧调用方（新代码已在 NewWSTransport 里自动启动）
func (ws *WSTransport) StartHeartbeat(interval time.Duration) {
	// 已在构造里启动，这里不再重复启动
}

func (ws *WSTransport) Close() error {
	ws.reconnectMu.Lock()
	ws.stopping = true
	select {
	case <-ws.stopCh:
	default:
		close(ws.stopCh)
	}
	ws.reconnectMu.Unlock()

	ws.mu.Lock()
	defer ws.mu.Unlock()
	return ws.conn.Close()
}
