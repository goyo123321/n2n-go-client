package main

import (
	"encoding/base64"
	"fmt"
	"io"
	"log"
	"net"
	"sync"
	"sync/atomic"
	"time"
)

type AppStream struct {
	ID        string
	PeerID    string
	LocalConn net.Conn
	CreatedAt time.Time

	ackCh   chan bool
	ackOnce sync.Once
	closed  atomic.Bool
}

func (s *AppStream) WaitAck(timeout time.Duration) bool {
	select {
	case ok := <-s.ackCh:
		return ok
	case <-time.After(timeout):
		return false
	}
}

func (s *AppStream) signalAck(ok bool) {
	s.ackOnce.Do(func() {
		select {
		case s.ackCh <- ok:
		default:
		}
	})
}

type AppStreamManager struct {
	mu      sync.RWMutex
	streams map[string]*AppStream
	seq     atomic.Uint64
}

func NewAppStreamManager() *AppStreamManager {
	return &AppStreamManager{
		streams: make(map[string]*AppStream),
	}
}

func (m *AppStreamManager) allocID(prefix string) string {
	n := m.seq.Add(1)
	return fmt.Sprintf("%s-%d", prefix, n)
}

func (m *AppStreamManager) add(s *AppStream) {
	m.mu.Lock()
	m.streams[s.ID] = s
	m.mu.Unlock()
}

func (m *AppStreamManager) get(id string) *AppStream {
	m.mu.RLock()
	defer m.mu.RUnlock()
	return m.streams[id]
}

func (m *AppStreamManager) remove(id string) *AppStream {
	m.mu.Lock()
	s := m.streams[id]
	delete(m.streams, id)
	m.mu.Unlock()
	return s
}

func (m *AppStreamManager) closeAll() {
	m.mu.Lock()
	for _, s := range m.streams {
		s.closed.Store(true)
		if s.LocalConn != nil {
			_ = s.LocalConn.Close()
		}
	}
	m.streams = make(map[string]*AppStream)
	m.mu.Unlock()
}

// ============ Edge 端方法 ============

func (e *Edge) findPeerByVIP(vip string) (string, bool) {
	e.peersMu.RLock()
	defer e.peersMu.RUnlock()
	for id, p := range e.peers {
		if p.VirtualIP == vip {
			return id, true
		}
	}
	return "", false
}

func (e *Edge) openLocalStream(peerID string, targetPort int, conn net.Conn) (*AppStream, error) {
	streamID := e.streamMgr.allocID(e.clientId)
	s := &AppStream{
		ID:        streamID,
		PeerID:    peerID,
		LocalConn: conn,
		CreatedAt: time.Now(),
		ackCh:     make(chan bool, 1),
	}
	e.streamMgr.add(s)

	err := e.ws.Send(map[string]interface{}{
		"type":       "app_stream_open",
		"to":         peerID,
		"streamId":   streamID,
		"targetPort": targetPort,
	})
	if err != nil {
		e.streamMgr.remove(streamID)
		return nil, err
	}
	log.Printf("[STREAM] %s → %s:%d 打开", streamID, peerID, targetPort)
	return s, nil
}

func (e *Edge) pumpLocalToRemote(s *AppStream) {
	defer func() {
		if r := recover(); r != nil {
			log.Printf("[STREAM] pumpLocalToRemote panic: %v", r)
		}
	}()

	buf := make([]byte, 2048)
	for {
		if s.closed.Load() {
			return
		}
		n, err := s.LocalConn.Read(buf)
		if n > 0 {
			dataB64 := base64.StdEncoding.EncodeToString(buf[:n])
			_ = e.ws.Send(map[string]interface{}{
				"type":     "app_stream_data",
				"to":       s.PeerID,
				"streamId": s.ID,
				"data":     dataB64,
			})
		}
		if err != nil {
			if err != io.EOF {
				log.Printf("[STREAM] %s 本地读错误: %v", s.ID, err)
			}
			e.closeStream(s, "local-eof")
			return
		}
	}
}

func (e *Edge) closeStream(s *AppStream, reason string) {
	if s == nil {
		return
	}
	if !s.closed.CompareAndSwap(false, true) {
		return
	}
	e.streamMgr.remove(s.ID)
	if s.LocalConn != nil {
		_ = s.LocalConn.Close()
	}
	_ = e.ws.Send(map[string]interface{}{
		"type":     "app_stream_close",
		"to":       s.PeerID,
		"streamId": s.ID,
		"reason":   reason,
	})
	log.Printf("[STREAM] %s 关闭 (%s)", s.ID, reason)
}

// ============ 远端消息处理 ============

func (e *Edge) onRemoteStreamOpen(msg map[string]interface{}) {
	from, _ := msg["from"].(string)
	streamID, _ := msg["streamId"].(string)
	targetPort := 0
	if f, ok := msg["targetPort"].(float64); ok {
		targetPort = int(f)
	}
	if from == "" || streamID == "" || targetPort <= 0 || targetPort > 65535 {
		return
	}

	log.Printf("[STREAM] 收到 open %s from %s targetPort=%d", streamID, from, targetPort)

	addr := fmt.Sprintf("127.0.0.1:%d", targetPort)
	conn, err := net.DialTimeout("tcp", addr, 5*time.Second)
	if err != nil {
		log.Printf("[STREAM] %s dial %s 失败: %v", streamID, addr, err)
		_ = e.ws.Send(map[string]interface{}{
			"type":     "app_stream_open_ack",
			"to":       from,
			"streamId": streamID,
			"ok":       false,
		})
		return
	}

	s := &AppStream{
		ID:        streamID,
		PeerID:    from,
		LocalConn: conn,
		CreatedAt: time.Now(),
		ackCh:     make(chan bool, 1),
	}
	e.streamMgr.add(s)

	_ = e.ws.Send(map[string]interface{}{
		"type":     "app_stream_open_ack",
		"to":       from,
		"streamId": streamID,
		"ok":       true,
	})

	safeGo("app-stream-pump", func() { e.pumpLocalToRemote(s) })
}

func (e *Edge) onRemoteStreamAck(msg map[string]interface{}) {
	streamID, _ := msg["streamId"].(string)
	ok, _ := msg["ok"].(bool)
	s := e.streamMgr.get(streamID)
	if s == nil {
		return
	}
	s.signalAck(ok)
}

func (e *Edge) onRemoteStreamData(msg map[string]interface{}) {
	streamID, _ := msg["streamId"].(string)
	dataB64, _ := msg["data"].(string)
	if streamID == "" || dataB64 == "" {
		return
	}

	s := e.streamMgr.get(streamID)
	if s == nil || s.closed.Load() {
		return
	}

	data, err := base64.StdEncoding.DecodeString(dataB64)
	if err != nil {
		log.Printf("[STREAM] %s base64 解码失败: %v", streamID, err)
		return
	}

	if _, err := s.LocalConn.Write(data); err != nil {
		log.Printf("[STREAM] %s 本地写失败: %v", streamID, err)
		e.closeStream(s, "local-write-fail")
	}
}

func (e *Edge) onRemoteStreamClose(msg map[string]interface{}) {
	streamID, _ := msg["streamId"].(string)
	reason, _ := msg["reason"].(string)
	s := e.streamMgr.remove(streamID)
	if s == nil {
		return
	}
	if s.closed.CompareAndSwap(false, true) {
		if s.LocalConn != nil {
			_ = s.LocalConn.Close()
		}
		log.Printf("[STREAM] %s 对端关闭 (%s)", streamID, reason)
	}
}
