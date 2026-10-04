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

	conn, _, err := websocket.DefaultDialer.Dial(fullURL, nil)
	if err != nil {
		return nil, err
	}
	ws := &WSTransport{conn: conn, clientId: clientId}
	go ws.readLoop()
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

func (ws *WSTransport) StartHeartbeat(interval time.Duration) {
	ticker := time.NewTicker(interval)
	go func() {
		for range ticker.C {
			_ = ws.Send(map[string]interface{}{
				"type": "ping",
				"ts":   time.Now().Unix(),
			})
		}
	}()
}

func (ws *WSTransport) Close() error {
	return ws.conn.Close()
}
