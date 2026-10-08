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
	mu       sync.Mutex
	conn     *websocket.Conn
	clientId string

	signalingURL string
	roomId       string
	token        string

	onMessage   func(map[string]interface{})
	onBinary    func([]byte)
	onReconnect func()

	closed chan struct{}
	once   sync.Once
}

func NewWSTransport(signalingURL, roomId, clientId, connectToken string) (*WSTransport, error) {
	ws := &WSTransport{
		signalingURL: signalingURL,
		roomId:       roomId,
		clientId:     clientId,
		token:        connectToken,
		closed:       make(chan struct{}),
	}
	if err := ws.dial(); err != nil {
		return nil, err
	}
	go ws.readLoop()
	return ws, nil
}

func (ws *WSTransport) dial() error {
	base := strings.TrimRight(ws.signalingURL, "/")
	fullURL := base + "/ws/" + url.PathEscape(ws.roomId) + "?cid=" + url.QueryEscape(ws.clientId)
	if ws.token != "" {
		fullURL += "&token=" + url.QueryEscape(ws.token)
	}

	log.Printf("[WS] 连接 %s", maskToken(fullURL))

	conn, _, err := websocket.DefaultDialer.Dial(fullURL, nil)
	if err != nil {
		return err
	}

	ws.mu.Lock()
	ws.conn = conn
	ws.mu.Unlock()
	return nil
}

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

// ★ P0-8：断线自动重连。
// 早期版本在读到 error 后直接 return，主 goroutine 不会因此退出，
// 进程变成"活着但失联"的状态。
func (ws *WSTransport) readLoop() {
	backoff := 1 * time.Second
	const maxBackoff = 30 * time.Second

	for {
		select {
		case <-ws.closed:
			return
		default:
		}

		ws.mu.Lock()
		conn := ws.conn
		ws.mu.Unlock()
		if conn == nil {
			time.Sleep(backoff)
			if err := ws.dial(); err != nil {
				log.Printf("[WS] 重连失败: %v，%v 后重试", err, backoff)
				backoff *= 2
				if backoff > maxBackoff {
					backoff = maxBackoff
				}
				continue
			}
			backoff = 1 * time.Second
			log.Printf("[WS] 重连成功")
			if ws.onReconnect != nil {
				ws.onReconnect()
			}
			continue
		}

		msgType, data, err := conn.ReadMessage()
		if err != nil {
			select {
			case <-ws.closed:
				return
			default:
			}
			log.Printf("[WS] 断线: %v，%v 后重连", err, backoff)
			ws.mu.Lock()
			ws.conn = nil
			ws.mu.Unlock()
			time.Sleep(backoff)
			backoff *= 2
			if backoff > maxBackoff {
				backoff = maxBackoff
			}
			continue
		}

		backoff = 1 * time.Second

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
	data, _ := json.Marshal(msg)
	ws.mu.Lock()
	defer ws.mu.Unlock()
	if ws.conn == nil {
		return nil // 断线期间静默丢弃；重连后由 onReconnect 重新上报
	}
	return ws.conn.WriteMessage(websocket.TextMessage, data)
}

func (ws *WSTransport) SendBinary(data []byte) error {
	ws.mu.Lock()
	defer ws.mu.Unlock()
	if ws.conn == nil {
		return nil
	}
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
	ws.once.Do(func() { close(ws.closed) })
	ws.mu.Lock()
	defer ws.mu.Unlock()
	if ws.conn != nil {
		return ws.conn.Close()
	}
	return nil
}
