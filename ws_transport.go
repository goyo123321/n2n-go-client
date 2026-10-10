package main

import (
	"encoding/json"
	"log"
	"net/url"
	"runtime/debug"
	"strings"
	"sync"
	"time"

	"github.com/gorilla/websocket"
)

type WSTransport struct {
	mu       sync.Mutex
	writeMu  sync.Mutex
	conn     *websocket.Conn
	clientId string

	signalingURL string
	roomId       string
	token        string

	onMessage   func(map[string]interface{})
	onBinary    func([]byte)
	onReconnect func()

	earlyText   []map[string]interface{}
	earlyBinary [][]byte
	handlersSet bool

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
	// ★ 用 safeGo 起 readLoop，防止漏网 panic
	safeGo("ws-readLoop", ws.readLoop)
	return ws, nil
}

// SetHandlers 设置回调并重放缓冲消息。
func (ws *WSTransport) SetHandlers(
	onMessage func(map[string]interface{}),
	onBinary func([]byte),
) {
	ws.mu.Lock()
	ws.onMessage = onMessage
	ws.onBinary = onBinary
	ws.handlersSet = true
	textBuf := ws.earlyText
	binBuf := ws.earlyBinary
	ws.earlyText = nil
	ws.earlyBinary = nil
	ws.mu.Unlock()

	if len(textBuf) > 0 {
		log.Printf("[WS] 重放 %d 条早期文本消息", len(textBuf))
		for _, msg := range textBuf {
			ws.routeText(msg)
		}
	}
	if len(binBuf) > 0 {
		log.Printf("[WS] 重放 %d 条早期二进制消息", len(binBuf))
		for _, data := range binBuf {
			ws.routeBinary(data)
		}
	}
}

// routeText 安全分发文本消息（handler panic 只记日志）。
func (ws *WSTransport) routeText(msg map[string]interface{}) {
	defer func() {
		if r := recover(); r != nil {
			log.Printf("[WS] onMessage panic: %v\n%s", r, debug.Stack())
		}
	}()
	ws.mu.Lock()
	ready := ws.handlersSet && ws.onMessage != nil
	handler := ws.onMessage
	if !ready {
		ws.earlyText = append(ws.earlyText, msg)
	}
	ws.mu.Unlock()
	if ready {
		handler(msg)
	}
}

// routeBinary 安全分发二进制消息。
func (ws *WSTransport) routeBinary(data []byte) {
	defer func() {
		if r := recover(); r != nil {
			log.Printf("[WS] onBinary panic: %v\n%s", r, debug.Stack())
		}
	}()
	ws.mu.Lock()
	ready := ws.handlersSet && ws.onBinary != nil
	handler := ws.onBinary
	if !ready {
		cp := make([]byte, len(data))
		copy(cp, data)
		ws.earlyBinary = append(ws.earlyBinary, cp)
	}
	ws.mu.Unlock()
	if ready {
		handler(data)
	}
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

// readLoop 断线自动重连（指数退避，最长 30s）。
func (ws *WSTransport) readLoop() {
	defer func() {
		if r := recover(); r != nil {
			log.Printf("[WS] readLoop panic: %v\n%s", r, debug.Stack())
		}
	}()

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
			ws.routeText(msg)
		} else if msgType == websocket.BinaryMessage {
			ws.routeBinary(data)
		}
	}
}

func (ws *WSTransport) Send(msg map[string]interface{}) error {
	data, _ := json.Marshal(msg)

	ws.writeMu.Lock()
	defer ws.writeMu.Unlock()

	ws.mu.Lock()
	conn := ws.conn
	ws.mu.Unlock()
	if conn == nil {
		return nil
	}
	return conn.WriteMessage(websocket.TextMessage, data)
}

func (ws *WSTransport) SendBinary(data []byte) error {
	ws.writeMu.Lock()
	defer ws.writeMu.Unlock()

	ws.mu.Lock()
	conn := ws.conn
	ws.mu.Unlock()
	if conn == nil {
		return nil
	}
	return conn.WriteMessage(websocket.BinaryMessage, data)
}

func (ws *WSTransport) StartHeartbeat(interval time.Duration) {
	safeGo("ws-heartbeat", func() {
		ticker := time.NewTicker(interval)
		defer ticker.Stop()
		for {
			select {
			case <-ws.closed:
				return
			case <-ticker.C:
				_ = ws.Send(map[string]interface{}{
					"type": "ping",
					"ts":   time.Now().Unix(),
				})
			}
		}
	})
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
