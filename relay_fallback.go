package main

import (
	"log"
	"net"
	"sync"
	"time"
)

type ConnType string

const (
	ConnP2P     ConnType = "p2p"
	ConnTURN    ConnType = "turn"
	ConnRelay   ConnType = "relay"
	ConnUnknown ConnType = "unknown"
)

type RelayManager struct {
	mu         sync.RWMutex
	ws         *WSTransport
	turnClient *TURNClient
	edge       *Edge
	states     map[string]ConnType
}

func NewRelayManager(ws *WSTransport, turnClient *TURNClient, edge *Edge) *RelayManager {
	return &RelayManager{
		ws:         ws,
		turnClient: turnClient,
		edge:       edge,
		states:     make(map[string]ConnType),
	}
}

func (rm *RelayManager) MarkP2P(peerId string) {
	rm.mu.Lock()
	defer rm.mu.Unlock()
	if rm.states[peerId] != ConnP2P {
		log.Printf("[连接] %s → P2P 直连", peerId)
		rm.states[peerId] = ConnP2P
	}
}

// MarkFallback 打洞失败降级：优先 TURN，TURN 不可用则 WS
func (rm *RelayManager) MarkFallback(peerId string) {
	rm.mu.Lock()
	defer rm.mu.Unlock()

	if rm.states[peerId] == ConnTURN || rm.states[peerId] == ConnRelay {
		return
	}

	if rm.turnClient != nil && rm.turnClient.IsReady() {
		rm.states[peerId] = ConnTURN
		log.Printf("[连接] %s → TURN 中继 (%s)", peerId, rm.turnClient.GetRelayAddr())
		return
	}

	rm.states[peerId] = ConnRelay
	log.Printf("[连接] %s → WS 中继 (TURN 未就绪，最后兜底)", peerId)
}

// ★ 修复：TURN 就绪后，把所有仍走 WS relay 的 peer 升级到 TURN
func (rm *RelayManager) UpgradeRelaysToTURN() {
	rm.mu.Lock()
	defer rm.mu.Unlock()
	if rm.turnClient == nil || !rm.turnClient.IsReady() {
		return
	}
	relayAddr := rm.turnClient.GetRelayAddr()
	upgraded := 0
	for peerId, state := range rm.states {
		if state == ConnRelay {
			rm.states[peerId] = ConnTURN
			upgraded++
			log.Printf("[连接] %s → TURN 中继 (延迟升级, %s)", peerId, relayAddr)
		}
	}
	if upgraded > 0 {
		log.Printf("[连接] TURN 就绪，升级 %d 个 WS 中继到 TURN", upgraded)
	}
}

// DowngradeToWS TURN 发送失败，降级到 WS
func (rm *RelayManager) DowngradeToWS(peerId string, reason string) {
	rm.mu.Lock()
	defer rm.mu.Unlock()
	if rm.states[peerId] != ConnRelay {
		log.Printf("[连接] %s → WS 中继 (TURN 失败: %s)", peerId, reason)
		rm.states[peerId] = ConnRelay
	}
}

// MarkRelay 兼容旧代码
func (rm *RelayManager) MarkRelay(peerId string) {
	rm.MarkFallback(peerId)
}

func (rm *RelayManager) ShouldRelay(peerId string) bool {
	rm.mu.RLock()
	defer rm.mu.RUnlock()
	s, ok := rm.states[peerId]
	if !ok {
		return true
	}
	return s == ConnTURN || s == ConnRelay
}

func (rm *RelayManager) IsP2P(peerId string) bool {
	rm.mu.RLock()
	defer rm.mu.RUnlock()
	return rm.states[peerId] == ConnP2P
}

func (rm *RelayManager) IsTURN(peerId string) bool {
	rm.mu.RLock()
	defer rm.mu.RUnlock()
	return rm.states[peerId] == ConnTURN
}

func (rm *RelayManager) IsWSRelay(peerId string) bool {
	rm.mu.RLock()
	defer rm.mu.RUnlock()
	return rm.states[peerId] == ConnRelay
}

func (rm *RelayManager) GetState(peerId string) ConnType {
	rm.mu.RLock()
	defer rm.mu.RUnlock()
	if s, ok := rm.states[peerId]; ok {
		return s
	}
	return ConnUnknown
}

func (rm *RelayManager) SendViaRelay(data []byte) error {
	return rm.ws.SendBinary(data)
}

// SendToPeer 三级降级：P2P → TURN → WS
func (rm *RelayManager) SendToPeer(peerId string, data []byte, target *PeerInfo) bool {
	state := rm.GetState(peerId)

	switch state {
	case ConnP2P:
		if target != nil && target.UDPAddr != nil {
			_, err := rm.edge.udpConn.WriteToUDP(data, target.UDPAddr)
			if err == nil {
				return true
			}
			log.Printf("[P2P] 发送到 %s 失败: %v", peerId, err)
			rm.MarkFallback(peerId)
		}
		return false

	case ConnTURN:
		if target != nil && target.TurnRelayAddr != "" && rm.turnClient != nil {
			relayAddr, err := net.ResolveUDPAddr("udp", target.TurnRelayAddr)
			if err == nil {
				if err := rm.turnClient.Send(data, relayAddr); err == nil {
					return true
				} else {
					log.Printf("[TURN] 发送到 %s 失败: %v", peerId, err)
				}
			}
		}
		rm.DowngradeToWS(peerId, "send failed")
		return false

	case ConnRelay:
		err := rm.ws.SendBinary(data)
		return err == nil
	}

	return false
}

func (rm *RelayManager) Report(interval time.Duration) {
	go func() {
		ticker := time.NewTicker(interval)
		for range ticker.C {
			rm.mu.RLock()
			conns := make(map[string]string)
			for peerId, t := range rm.states {
				conns[peerId] = string(t)
			}
			rm.mu.RUnlock()

			// ★ 修复：空 map 不上报，否则服务端会清空已有连接状态
			if len(conns) == 0 {
				continue
			}
			_ = rm.ws.Send(map[string]interface{}{
				"type":    "connection_status",
				"payload": map[string]interface{}{"connections": conns},
			})
		}
	}()
}
