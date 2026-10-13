package main

import (
	"net"
	"sync"
	"time"
)

var (
	punchPrefix = []byte{0x4E, 0x32, 0x4E, 0x50} // "N2NP"
	ackPrefix   = []byte{0x4E, 0x32, 0x4E, 0x41} // "N2NA"
)

const punchAckThrottle = 500 * time.Millisecond

func isPunchProbe(data []byte) bool {
	return len(data) >= 4 &&
		data[0] == punchPrefix[0] && data[1] == punchPrefix[1] &&
		data[2] == punchPrefix[2] && data[3] == punchPrefix[3]
}

func isPunchAck(data []byte) bool {
	return len(data) >= 4 &&
		data[0] == ackPrefix[0] && data[1] == ackPrefix[1] &&
		data[2] == ackPrefix[2] && data[3] == ackPrefix[3]
}

func buildPunchProbe(virtualIP string) []byte {
	buf := make([]byte, 32)
	copy(buf[0:4], punchPrefix)
	if ip := net.ParseIP(virtualIP); ip != nil && ip.To4() != nil {
		copy(buf[4:8], ip.To4())
	}
	return buf
}

func buildPunchAck() []byte {
	buf := make([]byte, 8)
	copy(buf[0:4], ackPrefix)
	return buf
}

type punchAckLimiter struct {
	mu   sync.Mutex
	last map[string]time.Time
}

func newPunchAckLimiter() *punchAckLimiter {
	return &punchAckLimiter{last: make(map[string]time.Time)}
}

func (l *punchAckLimiter) allow(key string, now time.Time) bool {
	l.mu.Lock()
	defer l.mu.Unlock()
	if t, ok := l.last[key]; ok && now.Sub(t) < punchAckThrottle {
		return false
	}
	l.last[key] = now
	return true
}

func (l *punchAckLimiter) forget(key string) {
	l.mu.Lock()
	delete(l.last, key)
	l.mu.Unlock()
}

func (l *punchAckLimiter) clear() {
	l.mu.Lock()
	l.last = make(map[string]time.Time)
	l.mu.Unlock()
}

// sendP2PProbe 用 N2NS PING 帧触发数据面验证（由 p2p_stream.go 提供）。
func (e *Edge) sendP2PProbe(peerID string) {
	if e.p2pStreams == nil {
		return
	}
	e.p2pStreams.sendP2PProbe(peerID)
}
