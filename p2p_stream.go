package main

import (
	"encoding/binary"
	"errors"
	"fmt"
	"io"
	"log"
	"net"
	"strconv"
	"sync"
	"time"
)

var p2pMagic = [4]byte{'N', '2', 'N', 'S'}

const (
	p2pTypeSYN    = 1
	p2pTypeSYNACK = 2
	p2pTypeDATA   = 3
	p2pTypeACK    = 4
	p2pTypeFIN    = 5
	p2pTypeRST    = 6
	p2pTypePING   = 7 // ★ 新增：数据面活跃探针
	p2pTypePONG   = 8 // ★ 新增：PING 响应
)

const (
	p2pVer         = 1
	p2pHeaderLen   = 21
	p2pMaxPayload  = 1200
	p2pWindow      = 16
	p2pResendMs    = 200
	p2pMaxRetries  = 8
	p2pIdleTimeout = 120 * time.Second
)

type p2pFrame struct {
	Type     uint8
	StreamID uint32
	Seq      uint32
	Ack      uint32
	Flags    uint8
	Payload  []byte
}

func encodeP2PFrame(f *p2pFrame) []byte {
	buf := make([]byte, p2pHeaderLen+len(f.Payload))
	copy(buf[0:4], p2pMagic[:])
	buf[4] = p2pVer
	buf[5] = f.Type
	binary.BigEndian.PutUint32(buf[6:10], f.StreamID)
	binary.BigEndian.PutUint32(buf[10:14], f.Seq)
	binary.BigEndian.PutUint32(buf[14:18], f.Ack)
	buf[18] = f.Flags
	binary.BigEndian.PutUint16(buf[19:21], uint16(len(f.Payload)))
	if len(f.Payload) > 0 {
		copy(buf[21:], f.Payload)
	}
	return buf
}

func decodeP2PFrame(data []byte) (*p2pFrame, error) {
	if len(data) < p2pHeaderLen {
		return nil, errors.New("short")
	}
	if data[0] != 'N' || data[1] != '2' || data[2] != 'N' || data[3] != 'S' {
		return nil, errors.New("magic")
	}
	if data[4] != p2pVer {
		return nil, errors.New("ver")
	}
	plen := int(binary.BigEndian.Uint16(data[19:21]))
	if len(data) < p2pHeaderLen+plen {
		return nil, errors.New("trunc")
	}
	return &p2pFrame{
		Type:     data[5],
		StreamID: binary.BigEndian.Uint32(data[6:10]),
		Seq:      binary.BigEndian.Uint32(data[10:14]),
		Ack:      binary.BigEndian.Uint32(data[14:18]),
		Flags:    data[18],
		Payload:  data[21 : 21+plen],
	}, nil
}

func isP2PFrame(data []byte) bool {
	return len(data) >= 4 && data[0] == 'N' && data[1] == '2' && data[2] == 'N' && data[3] == 'S'
}

type p2pSeg struct {
	seq      uint32
	data     []byte
	sentAt   time.Time
	retries  int
	isSyn    bool
	isSynAck bool
}

type P2PStream struct {
	ID     uint32
	PeerID string

	mu       sync.Mutex
	peerAddr *net.UDPAddr
	viaTURN  bool

	sendNext uint32
	sendBase uint32
	segments map[uint32]*p2pSeg

	recvNext uint32
	recvBuf  map[uint32][]byte

	localConn net.Conn
	ownsConn  bool

	closed        bool
	closeCh       chan struct{}
	establishedCh chan bool
	lastAct       time.Time
}

type P2PStreamManager struct {
	mu   sync.RWMutex
	byID map[uint32]*P2PStream
	next uint32
	edge *Edge
}

func NewP2PStreamManager(edge *Edge) *P2PStreamManager {
	m := &P2PStreamManager{byID: make(map[uint32]*P2PStream), next: 2, edge: edge}
	safeGo("p2p-reaper", m.reaper)
	return m
}

func (m *P2PStreamManager) allocID() uint32 {
	m.mu.Lock()
	defer m.mu.Unlock()
	for {
		id := m.next
		m.next += 2
		if m.next == 0 {
			m.next = 2
		}
		if _, dup := m.byID[id]; !dup {
			return id
		}
	}
}

func (m *P2PStreamManager) add(s *P2PStream) {
	m.mu.Lock()
	m.byID[s.ID] = s
	m.mu.Unlock()
}

func (m *P2PStreamManager) get(id uint32) *P2PStream {
	m.mu.RLock()
	defer m.mu.RUnlock()
	return m.byID[id]
}

func (m *P2PStreamManager) remove(id uint32) {
	m.mu.Lock()
	delete(m.byID, id)
	m.mu.Unlock()
}

func (m *P2PStreamManager) closeAll() {
	m.mu.Lock()
	all := make([]*P2PStream, 0, len(m.byID))
	for _, s := range m.byID {
		all = append(all, s)
	}
	m.byID = make(map[uint32]*P2PStream)
	m.mu.Unlock()
	for _, s := range all {
		s.close("manager-shutdown")
	}
}

func (m *P2PStreamManager) reaper() {
	t := time.NewTicker(30 * time.Second)
	defer t.Stop()
	for range t.C {
		now := time.Now()
		m.mu.RLock()
		var stale []*P2PStream
		for _, s := range m.byID {
			s.mu.Lock()
			idle := now.Sub(s.lastAct)
			s.mu.Unlock()
			if idle > p2pIdleTimeout {
				stale = append(stale, s)
			}
		}
		m.mu.RUnlock()
		for _, s := range stale {
			s.close("idle")
		}
	}
}

// sendP2PProbe 向 peerID 发一个裸 P2P PING 帧（无需 stream）。
//
// 目的：打洞握手成功后（P2PAvailable 状态），主动触发一次数据面验证。
// 对端收到 PING 会走 handleP2PFrame → MarkP2P，同时回 PONG；
// 本端收到 PONG 也会 MarkP2P。双方都在 1 个 RTT 内完成 FullDuplex 升级，
// 不用等上层应用发起业务流量。
//
// 帧的 StreamID 用 0（保留值，与真实 stream 的偶数 ID 不冲突），
// 对端 handleFrame 找不到 stream 会直接走裸探针分支。
func (m *P2PStreamManager) sendP2PProbe(peerID string) {
	if m == nil || m.edge == nil || peerID == "" {
		return
	}
	addr, viaTURN := m.edge.peerAddrAndMode(peerID)
	if addr == nil {
		return
	}
	frame := &p2pFrame{
		Type:     p2pTypePING,
		StreamID: 0,
	}
	tmp := &P2PStream{
		ID:       0,
		PeerID:   peerID,
		peerAddr: addr,
		viaTURN:  viaTURN,
	}
	if err := m.edge.sendP2PFrame(tmp, frame); err != nil {
		log.Printf("[P2P-PROBE] 向 %s 发 P2P PING 失败: %v", peerID, err)
	}
}

// sendP2PFrame 发送一帧到对端。
//
// 优先级：
//   1. LAN 地址（同 WiFi 直连，最低延迟，绕开 NAT）
//   2. TURN relay（打洞失败时）
//   3. P2P 公网地址（打洞成功时）
func (e *Edge) sendP2PFrame(s *P2PStream, f *p2pFrame) error {
	s.mu.Lock()
	addr := s.peerAddr
	viaTURN := s.viaTURN
	peerID := s.PeerID
	s.mu.Unlock()

	buf := encodeP2PFrame(f)

	// 优先发 LAN 地址（广播给该 peer 的所有 LAN 地址）
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
		// LAN 已发出，同时也继续走原有路径（保底）
	}

	if viaTURN {
		if e.turnClient == nil || !e.turnClient.IsReady() {
			if lanSent {
				return nil
			}
			return errors.New("turn down and no lan")
		}
		e.peersMu.RLock()
		var relayAddr string
		if p := e.peers[peerID]; p != nil {
			relayAddr = p.TurnRelayAddr
		}
		e.peersMu.RUnlock()

		if relayAddr == "" {
			if lanSent {
				return nil
			}
			return errors.New("peer turn relay unknown and no lan")
		}
		ra, err := net.ResolveUDPAddr("udp", relayAddr)
		if err != nil {
			if lanSent {
				return nil
			}
			return fmt.Errorf("bad relay addr %q: %w", relayAddr, err)
		}
		if err := e.turnClient.Send(buf, ra); err != nil {
			if lanSent {
				return nil
			}
			return err
		}
		return nil
	}

	if addr == nil {
		if lanSent {
			return nil
		}
		return errors.New("no addr and no lan")
	}
	if e.udpConn == nil {
		if lanSent {
			return nil
		}
		return errors.New("no udp")
	}
	_, err := e.udpConn.WriteToUDP(buf, addr)
	return err
}

// handleP2PFrame 收到 P2P 帧时的入口。
//
// ★ 修复：收到真实 P2P 数据帧后，升级 relayMgr 状态到 P2P。
//
// 这是项目一/二 HasVerifiedDataPath 的等价物：打洞握手（Punch/ACK）
// 只能证明双方 NAT 映射已打开，但真实数据帧才能证明数据面可用。
// 在收到第一个真实数据帧之前，状态是 P2PAvailable；收到后升级到 P2P。
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

	// ★ 收到真实 P2P 数据帧 → 升级到完全 P2P
	if e.relayMgr != nil {
		if e.relayMgr.GetState(peerID) != ConnP2P {
			log.Printf("[P2P] 收到真实 P2P 数据帧（stream=%d）→ P2P 直连", f.StreamID)
			e.relayMgr.MarkP2P(peerID)
		}
	}

	e.p2pStreams.handleFrame(peerID, peerAddr, viaTURN, f)
}

// findPeerIDByAddr 按对端地址查 peerID。
//
// 匹配优先级：
//   1. IP + Port 精确匹配（对端公网地址）
//   2. LAN 地址匹配（同 WiFi 场景，源地址是 192.168.x.x）
//   3. IP-only 匹配（对称 NAT 端口漂移）
func (e *Edge) findPeerIDByAddr(addr *net.UDPAddr) string {
	if addr == nil {
		return ""
	}
	e.peersMu.RLock()
	defer e.peersMu.RUnlock()

	// 精确匹配
	for id, p := range e.peers {
		if p.UDPAddr != nil && p.UDPAddr.IP.Equal(addr.IP) && p.UDPAddr.Port == addr.Port {
			return id
		}
	}

	// LAN 地址匹配
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

	// IP-only 匹配
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
		// P2P 和 P2PAvailable 都算"不走 TURN"
		viaTURN = st != ConnP2P && st != ConnP2PAvailable
	}
	return p.UDPAddr, viaTURN
}

func (m *P2PStreamManager) OpenP2PStream(peerID string, targetPort int, localConn net.Conn, timeout time.Duration) (*P2PStream, error) {
	addr, viaTURN := m.edge.peerAddrAndMode(peerID)
	if addr == nil {
		return nil, errors.New("peer addr unknown")
	}
	s := &P2PStream{
		ID:            m.allocID(),
		PeerID:        peerID,
		peerAddr:      addr,
		viaTURN:       viaTURN,
		segments:      make(map[uint32]*p2pSeg),
		recvBuf:       make(map[uint32][]byte),
		localConn:     localConn,
		ownsConn:      false,
		closeCh:       make(chan struct{}),
		establishedCh: make(chan bool, 1),
		lastAct:       time.Now(),
	}
	m.add(s)

	portBuf := make([]byte, 4)
	binary.BigEndian.PutUint32(portBuf, uint32(targetPort))
	syn := &p2pFrame{Type: p2pTypeSYN, StreamID: s.ID, Seq: 0, Payload: portBuf}

	s.mu.Lock()
	s.segments[0] = &p2pSeg{seq: 0, sentAt: time.Now(), isSyn: true}
	s.sendNext = 1
	s.recvNext = 1
	s.mu.Unlock()

	if err := m.edge.sendP2PFrame(s, syn); err != nil {
		m.remove(s.ID)
		s.abortNoConn()
		return nil, err
	}

	safeGo("p2p-rtx", func() { s.retransmitLoop(m.edge) })

	select {
	case ok := <-s.establishedCh:
		if !ok {
			m.remove(s.ID)
			s.abortNoConn()
			return nil, errors.New("peer rejected")
		}
		s.mu.Lock()
		s.ownsConn = true
		s.mu.Unlock()
		safeGo("p2p-tx", func() { s.readLocalAndSend(m.edge) })
		return s, nil

	case <-time.After(timeout):
		m.remove(s.ID)
		log.Printf("[P2P-STREAM] %d 建流超时（%v），保留 conn 让调用方回落", s.ID, timeout)
		s.abortNoConn()
		return nil, errors.New("syn timeout")

	case <-s.closeCh:
		m.remove(s.ID)
		s.mu.Lock()
		s.localConn = nil
		s.closed = true
		s.mu.Unlock()
		return nil, errors.New("closed")
	}
}

func (s *P2PStream) abortNoConn() {
	s.mu.Lock()
	if s.closed {
		s.mu.Unlock()
		return
	}
	s.closed = true
	s.localConn = nil
	s.mu.Unlock()
	close(s.closeCh)
}

func (m *P2PStreamManager) acceptSYN(peerID string, peerAddr *net.UDPAddr, viaTURN bool, f *p2pFrame) {
	if len(f.Payload) < 4 {
		return
	}
	targetPort := int(binary.BigEndian.Uint32(f.Payload[:4]))
	if targetPort <= 0 || targetPort > 65535 {
		return
	}

	localConn, err := net.DialTimeout(
		"tcp",
		net.JoinHostPort("127.0.0.1", strconv.Itoa(targetPort)),
		5*time.Second,
	)
	if err != nil {
		rst := &p2pFrame{Type: p2pTypeRST, StreamID: f.StreamID}
		tmp := &P2PStream{ID: f.StreamID, peerAddr: peerAddr, viaTURN: viaTURN}
		_ = m.edge.sendP2PFrame(tmp, rst)
		log.Printf("[P2P-STREAM] %d dial 127.0.0.1:%d 失败: %v", f.StreamID, targetPort, err)
		return
	}

	s := &P2PStream{
		ID:            f.StreamID,
		PeerID:        peerID,
		peerAddr:      peerAddr,
		viaTURN:       viaTURN,
		segments:      make(map[uint32]*p2pSeg),
		recvBuf:       make(map[uint32][]byte),
		localConn:     localConn,
		ownsConn:      true,
		closeCh:       make(chan struct{}),
		establishedCh: make(chan bool, 1),
		lastAct:       time.Now(),
	}
	s.establishedCh <- true
	m.add(s)

	s.mu.Lock()
	s.segments[0] = &p2pSeg{seq: 0, sentAt: time.Now(), isSyn: true, isSynAck: true}
	s.sendNext = 1
	s.recvNext = 1
	s.mu.Unlock()

	synack := &p2pFrame{
		Type:     p2pTypeSYNACK,
		StreamID: s.ID,
		Seq:      0,
		Ack:      1,
	}
	_ = m.edge.sendP2PFrame(s, synack)

	log.Printf("[P2P-STREAM] %d 收到 SYN from %s, 转发到 127.0.0.1:%d",
		s.ID, peerID, targetPort)

	safeGo("p2p-rtx", func() { s.retransmitLoop(m.edge) })
	safeGo("p2p-tx", func() { s.readLocalAndSend(m.edge) })
}

// handleFrame 处理收到的 P2P 帧。
//
// 分两条路径：
//   - StreamID 匹配到已有 stream → 走 stream 状态机
//   - StreamID=0（裸探针 PING/PONG）→ 简单处理，不涉及 stream
func (m *P2PStreamManager) handleFrame(peerID string, peerAddr *net.UDPAddr, viaTURN bool, f *p2pFrame) {
	s := m.get(f.StreamID)

	// ★ 无 stream 时的裸帧处理
	if s == nil {
		if f.Type == p2pTypePING {
			// 收到 PING → 回 PONG
			go func() {
				resp := &p2pFrame{Type: p2pTypePONG, StreamID: 0}
				tmp := &P2PStream{
					ID:       0,
					PeerID:   peerID,
					peerAddr: peerAddr,
					viaTURN:  viaTURN,
				}
				_ = m.edge.sendP2PFrame(tmp, resp)
			}()
			return
		}
		if f.Type == p2pTypePONG {
			// PONG 只是响应，MarkP2P 已在 handleP2PFrame 完成
			return
		}
		if f.Type == p2pTypeSYN {
			safeGo("p2p-accept", func() { m.acceptSYN(peerID, peerAddr, viaTURN, f) })
			return
		}
		// 其余（DATA/ACK/FIN/RST 但无 stream）：丢弃
		return
	}

	s.mu.Lock()
	s.peerAddr = peerAddr
	s.viaTURN = viaTURN
	s.lastAct = time.Now()

	if f.Type == p2pTypeACK || f.Type == p2pTypeDATA || f.Type == p2pTypeSYNACK {
		if f.Ack > s.sendBase {
			for seq := s.sendBase; seq < f.Ack; seq++ {
				delete(s.segments, seq)
			}
			s.sendBase = f.Ack
		}
	}
	s.mu.Unlock()

	switch f.Type {
	case p2pTypeSYNACK:
		_ = m.edge.sendP2PFrame(s, &p2pFrame{
			Type:     p2pTypeACK,
			StreamID: s.ID,
			Ack:      1,
		})
		select {
		case s.establishedCh <- true:
		default:
		}
	case p2pTypeDATA:
		s.handleData(m.edge, f)
	case p2pTypeACK:
		// ack 已处理
	case p2pTypeFIN, p2pTypeRST:
		s.close("peer-close")
	}
}

func (s *P2PStream) handleData(e *Edge, f *p2pFrame) {
	var toWrite [][]byte

	s.mu.Lock()
	if f.Seq == s.recvNext {
		if len(f.Payload) > 0 {
			cp := make([]byte, len(f.Payload))
			copy(cp, f.Payload)
			toWrite = append(toWrite, cp)
		}
		s.recvNext++
		for {
			d, ok := s.recvBuf[s.recvNext]
			if !ok {
				break
			}
			delete(s.recvBuf, s.recvNext)
			toWrite = append(toWrite, d)
			s.recvNext++
		}
	} else if f.Seq > s.recvNext {
		if _, exists := s.recvBuf[f.Seq]; !exists {
			cp := make([]byte, len(f.Payload))
			copy(cp, f.Payload)
			s.recvBuf[f.Seq] = cp
		}
	}
	ackToSend := s.recvNext
	local := s.localConn
	s.mu.Unlock()

	for _, d := range toWrite {
		if local != nil {
			_, _ = local.Write(d)
		}
	}

	_ = e.sendP2PFrame(s, &p2pFrame{
		Type:     p2pTypeACK,
		StreamID: s.ID,
		Ack:      ackToSend,
	})
}

func (s *P2PStream) readLocalAndSend(e *Edge) {
	defer func() {
		if r := recover(); r != nil {
			log.Printf("[P2P-STREAM] %d readLocal panic: %v", s.ID, r)
		}
	}()

	buf := make([]byte, p2pMaxPayload)
	for {
		s.mu.Lock()
		closed := s.closed
		local := s.localConn
		s.mu.Unlock()
		if closed || local == nil {
			return
		}

		for {
			s.mu.Lock()
			inflight := s.sendNext - s.sendBase
			s.mu.Unlock()
			if inflight < p2pWindow {
				break
			}
			select {
			case <-s.closeCh:
				return
			case <-time.After(10 * time.Millisecond):
			}
		}

		n, err := local.Read(buf)
		if n > 0 {
			s.mu.Lock()
			seq := s.sendNext
			s.sendNext++
			data := make([]byte, n)
			copy(data, buf[:n])
			s.segments[seq] = &p2pSeg{seq: seq, data: data, sentAt: time.Now()}
			ack := s.recvNext
			s.mu.Unlock()

			_ = e.sendP2PFrame(s, &p2pFrame{
				Type:     p2pTypeDATA,
				StreamID: s.ID,
				Seq:      seq,
				Ack:      ack,
				Payload:  data,
			})
		}
		if err != nil {
			if err != io.EOF {
				log.Printf("[P2P-STREAM] %d local read: %v", s.ID, err)
			}
			s.mu.Lock()
			finSeq := s.sendNext
			s.sendNext++
			s.mu.Unlock()
			_ = e.sendP2PFrame(s, &p2pFrame{Type: p2pTypeFIN, StreamID: s.ID, Seq: finSeq})
			s.close("local-eof")
			return
		}
	}
}

func (s *P2PStream) retransmitLoop(e *Edge) {
	defer func() {
		if r := recover(); r != nil {
			log.Printf("[P2P-STREAM] %d retransmit panic: %v", s.ID, r)
		}
	}()
	t := time.NewTicker(p2pResendMs * time.Millisecond)
	defer t.Stop()
	for {
		select {
		case <-s.closeCh:
			return
		case <-t.C:
		}
		s.mu.Lock()
		if s.closed {
			s.mu.Unlock()
			return
		}
		now := time.Now()
		var toResend []*p2pSeg
		var kill bool
		for seq := s.sendBase; seq < s.sendNext; seq++ {
			seg, ok := s.segments[seq]
			if !ok {
				continue
			}
			if now.Sub(seg.sentAt) < p2pResendMs*time.Millisecond {
				continue
			}
			if seg.retries >= p2pMaxRetries {
				kill = true
				break
			}
			seg.retries++
			seg.sentAt = now
			toResend = append(toResend, seg)
		}
		ack := s.recvNext
		s.mu.Unlock()

		if kill {
			log.Printf("[P2P-STREAM] %d 重传耗尽（对端无响应）", s.ID)
			s.close("retransmit-exhausted")
			return
		}
		for _, seg := range toResend {
			var ft uint8 = p2pTypeDATA
			if seg.isSyn {
				if seg.isSynAck {
					ft = p2pTypeSYNACK
				} else {
					ft = p2pTypeSYN
				}
			}
			_ = e.sendP2PFrame(s, &p2pFrame{
				Type:     ft,
				StreamID: s.ID,
				Seq:      seg.seq,
				Ack:      ack,
				Payload:  seg.data,
			})
		}
	}
}

func (s *P2PStream) close(reason string) {
	s.mu.Lock()
	if s.closed {
		s.mu.Unlock()
		return
	}
	s.closed = true
	local := s.localConn
	owns := s.ownsConn
	s.localConn = nil
	s.mu.Unlock()

	close(s.closeCh)
	if local != nil && owns {
		_ = local.Close()
	}
	log.Printf("[P2P-STREAM] %d 关闭 (%s)", s.ID, reason)
}
