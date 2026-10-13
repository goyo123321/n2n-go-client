package main

// NAT 打洞握手 + 数据面探针。
//
// ★ PC 端说明：
//   - Punch/ACK 使用不同 prefix，分离控制面
//   - 数据面探针统一为 N2NS PING 帧（由 p2p_stream.go 提供），
//     不再使用 ICMP Echo —— 因为 Termux 端不识别 ICMP Echo
//   - PC↔PC 场景下数据面走 TUN（IPv4 包），与 N2NS 共存

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

// buildPunchProbe 生成 32 字节 Punch。
//
// ★ 全工程唯一实现。nathole_executor.go 里不再定义同名函数，
//   调用点直接解析到这里。
func buildPunchProbe(virtualIP string) []byte {
	buf := make([]byte, 32)
	copy(buf[0:4], punchPrefix)
	if ip := net.ParseIP(virtualIP); ip != nil && ip.To4() != nil {
		copy(buf[4:8], ip.To4())
	}
	return buf
}

// buildPunchAck 生成 8 字节 ACK。
func buildPunchAck() []byte {
	buf := make([]byte, 8)
	copy(buf[0:4], ackPrefix)
	return buf
}

// punchAckLimiter 按 peer 限流 ACK 发送。
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

// sendP2PProbe 用 N2NS PING 帧触发数据面验证。
// 具体逻辑在 p2p_stream.go 的 P2PStreamManager.sendP2PProbe。
func (e *Edge) sendP2PProbe(peerID string) {
	if e.p2pStreams == nil {
		return
	}
	e.p2pStreams.sendP2PProbe(peerID)
}
