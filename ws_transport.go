package main

import (
	"encoding/json"
	"log"
	"net"
	"net/url"
	"runtime/debug"
	"strings"
	"sync"
	"time"

	"github.com/gorilla/websocket"
)

// ★ 新增：读超时。
//   客户端每 20 秒发一次 ping，服务端立即回 pong。
//   90 秒内没收到任何消息（包括 pong）→ 认为下行通道已死 → 主动重连。
const wsReadTimeout = 90 * time.Second

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
	safeGo("ws-readLoop", ws.readLoop)
	return ws, nil
}

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

// readLoop 断线自动重连（指数退避，封顶 5 分钟，无限重试）。
//
// ★ 修复：每次读之前设置 90 秒读超时。
//   场景：客户端网络抖动后，下行通道可能"僵死"——
//   底层 TCP 表面正常，但服务端发的消息到达不了客户端。
//   这时客户端发 ping 依然成功（上行 OK），但收不到 pong。
//   设置读超时后，90 秒无任何消息会触发重连。
func (ws *WSTransport) readLoop() {
	backoff := 1 * time.Second
	const maxBackoff = 5 * time.Minute

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
				switch {
				case backoff < 2*time.Second:
					backoff = 2 * time.Second
				case backoff < 5*time.Second:
					backoff = 5 * time.Second
				case backoff < 10*time.Second:
					backoff = 10 * time.Second
				case backoff < 30*time.Second:
					backoff = 30 * time.Second
				case backoff < 60*time.Second:
					backoff = 60 * time.Second
				case backoff < 2*time.Minute:
					backoff = 2 * time.Minute
				default:
					backoff = maxBackoff
				}
				continue
			}
			backoff = 1 * time.Second
			log.Printf("[WS] ✅ 重连成功")
			if ws.onReconnect != nil {
				ws.onReconnect()
			}
			continue
		}

		// ★ 设置读超时
		_ = conn.SetReadDeadline(time.Now().Add(wsReadTimeout))
		msgType, data, err := conn.ReadMessage()
		if err != nil {
			select {
			case <-ws.closed:
				return
			default:
			}
			if ne, ok := err.(net.Error); ok && ne.Timeout() {
				log.Printf("[WS] 读超时 %v 未收到任何消息（下行僵死），触发重连", wsReadTimeout)
			} else {
				log.Printf("[WS] 断线: %v，%v 后重连", err, backoff)
			}
			ws.mu.Lock()
			ws.conn = nil
			ws.mu.Unlock()
			continue
		}

		backoff = 1 * time.Second

		if msgType == websocket.TextMessage {
			var msg map[string]interface{}
			if err := json.Unmarshal(data, &msg); err != nil {
				continue
			}
			// ★ 服务端主动 ping，立即回 pong
			if t, _ := msg["type"].(string); t == "server_ping" {
				_ = ws.Send(map[string]interface{}{"type": "server_pong"})
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
