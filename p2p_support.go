package main

// PC 端为 p2p_stream.go 补齐的依赖方法。
//
// p2p_stream.go 从 Termux 端复制时删除了 4 个函数（findPeerIDByAddr、
// peerAddrAndMode、sendP2PFrame、handleP2PFrame），因为它们依赖 PC 端
// main.go 的 Edge 结构。此文件统一补齐。

import (
	"errors"
	"fmt"
	"log"
	"net"
)

// findPeerByVIP 供 socks5.go 使用。
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

func (e *Edge) findPeerIDByAddr(addr *net.UDPAddr) string {
	if addr == nil {
		return ""
	}
	e.peersMu.RLock()
	defer e.peersMu.RUnlock()

	for id, p := range e.peers {
		if p.UDPAddr != nil && p.UDPAddr.IP.Equal(addr.IP) && p.UDPAddr.Port == addr.Port {
			return id
		}
	}

	for id, p := range e.peers {
		for _, lan := range p.LanEndpoints {
			la, err := net.ResolveUDPAddr("udp", lan)
			if err != nil {
				continue
			}
			if la.IP.Equal(addr.IP) && la.Port == addr.Port {
				return id
			}
		}
	}

	for id, p := range e.peers {
		if p.UDPAddr != nil && p.UDPAddr.IP.Equal(addr.IP) {
			return id
		}
	}
	return ""
}

func (e *Edge) peerAddrAndMode(peerID string) (*net.UDPAddr, bool) {
	e.peersMu.RLock()
	p := e.peers[peerID]
	e.peersMu.RUnlock()
	if p == nil || p.UDPAddr == nil {
		return nil, false
	}
	viaTURN := true
	if e.relayMgr != nil {
		st := e.relayMgr.GetState(peerID)
		viaTURN = st != ConnP2P && st != ConnP2PAvailable
	}
	return p.UDPAddr, viaTURN
}

// sendP2PFrame 发送一个 N2NS 帧到指定 peer。
// 优先级：LAN > TURN > P2P 公网。
func (e *Edge) sendP2PFrame(s *P2PStream, f *p2pFrame) error {
	s.mu.Lock()
	addr := s.peerAddr
	viaTURN := s.viaTURN
	peerID := s.PeerID
	s.mu.Unlock()

	buf := encodeP2PFrame(f)

	e.peersMu.RLock()
	var lanEndpoints []string
	if p := e.peers[peerID]; p != nil {
		lanEndpoints = p.LanEndpoints
	}
	e.peersMu.RUnlock()

	lanSent := false
	if e.udpConn != nil {
		for _, lan := range lanEndpoints {
			ra, err := net.ResolveUDPAddr("udp", lan)
			if err != nil {
				continue
			}
			if _, err := e.udpConn.WriteToUDP(buf, ra); err == nil {
				lanSent = true
			}
		}
	}
	if lanSent {
		return nil
	}

	if viaTURN {
		if e.turnClient == nil || !e.turnClient.IsReady() {
			return errors.New("turn down and no lan")
		}
		e.peersMu.RLock()
		var relayAddr string
		if p := e.peers[peerID]; p != nil {
			relayAddr = p.TurnRelayAddr
		}
		e.peersMu.RUnlock()

		if relayAddr == "" {
			return errors.New("peer turn relay unknown and no lan")
		}
		ra, err := net.ResolveUDPAddr("udp", relayAddr)
		if err != nil {
			return fmt.Errorf("bad relay addr %q: %w", relayAddr, err)
		}
		return e.turnClient.Send(buf, ra)
	}

	if addr == nil {
		return errors.New("no addr and no lan")
	}
	if e.udpConn == nil {
		return errors.New("no udp")
	}
	_, err := e.udpConn.WriteToUDP(buf, addr)
	return err
}

// handleP2PFrame 收到 N2NS 帧时的入口。
//
// PC 端接受 Termux 端的 SOCKS5 隧道，收到 N2NS 帧即升级到 P2P。
func (e *Edge) handleP2PFrame(peerAddr *net.UDPAddr, viaTURN bool, data []byte) {
	if e.p2pStreams == nil {
		return
	}
	f, err := decodeP2PFrame(data)
	if err != nil {
		return
	}
	peerID := e.findPeerIDByAddr(peerAddr)
	if peerID == "" {
		return
	}

	if e.relayMgr != nil {
		if e.relayMgr.GetState(peerID) != ConnP2P {
			log.Printf("[P2P] 收到 N2NS 数据帧（stream=%d）→ P2P 直连", f.StreamID)
			e.relayMgr.MarkP2P(peerID)
		}
	}

	e.p2pStreams.handleFrame(peerID, peerAddr, viaTURN, f)
}
