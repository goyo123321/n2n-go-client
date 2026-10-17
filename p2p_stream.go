// ============ N2NW 二进制帧（WS 中继数据面） ============
//
// 与文本 app_stream_* 协议并存：
//   - app_stream_*（base64 文本）→ 应用层 TCP 隧道
//   - N2NW（二进制）             → IPv4 数据面
//
// 帧格式：
//   magic(4B)     = "N2NW"
//   flags(1B)     = 保留位，当前固定 0（payload = 裸 IPv4）
//   peerIdLen(1B) = peerId 字节长度
//   peerId(N)     = 客户端→服务端时填目标 clientId；
//                   服务端→客户端时被重写为发送方 clientId
//   payload       = 裸 IPv4 包
//
// PC 端：
//   - 发送侧：buildN2NWFrame(targetID, ipv4Packet) 交给 ws.SendBinary
//   - 接收侧：handleAppStreamBinary 从帧头读出 senderID → 更新 peer 活跃
//             时间 → payload 送 TUN

const (
	n2nwHdrMin  = 6
	n2nwPeerMax = 255
)

// buildN2NWFrame 构造 N2NW 帧。targetID 是目标 peer 的 clientId，
// 服务端会把它重写为发送方 ID 后再转发给对端。
func buildN2NWFrame(targetID string, payload []byte) []byte {
	idBytes := []byte(targetID)
	if len(idBytes) > n2nwPeerMax {
		idBytes = idBytes[:n2nwPeerMax]
	}
	frame := make([]byte, n2nwHdrMin+len(idBytes)+len(payload))
	frame[0] = 'N'
	frame[1] = '2'
	frame[2] = 'N'
	frame[3] = 'W'
	frame[4] = 0 // flags
	frame[5] = byte(len(idBytes))
	copy(frame[n2nwHdrMin:], idBytes)
	copy(frame[n2nwHdrMin+len(idBytes):], payload)
	return frame
}

// handleAppStreamBinary 处理服务端转发来的 N2NW 帧。
// 帧头 peerId 是发送方 clientId（服务端重写后），payload 必须是 IPv4 包。
func (e *Edge) handleAppStreamBinary(data []byte) {
	if len(data) < n2nwHdrMin {
		return
	}
	if data[0] != 'N' || data[1] != '2' || data[2] != 'N' || data[3] != 'W' {
		return
	}

	flags := data[4]
	peerLen := int(data[5])
	if peerLen <= 0 || peerLen > n2nwPeerMax {
		return
	}
	if len(data) < n2nwHdrMin+peerLen {
		return
	}

	senderID := string(data[n2nwHdrMin : n2nwHdrMin+peerLen])
	payload := data[n2nwHdrMin+peerLen:]
	if len(payload) < 20 || payload[0]>>4 != 4 {
		// 当前 flags=0 只承诺 IPv4 payload；未来扩展走 flags
		log.Printf("[WS-BIN] %s → 非 IPv4 payload (len=%d flags=%d)",
			senderID, len(payload), flags)
		return
	}

	// 更新发送方 peer 的活跃时间
	//
	// 用途：
	//   - P2P 空闲检测（sendP2PKeepalive 里的 60s p2pIdleDeadMs）
	//   - MarkFallback 的 3 秒保护（relay_fallback.go）
	now := time.Now().UnixMilli()
	e.peersMu.Lock()
	if p, ok := e.peers[senderID]; ok {
		p.lastRecvAt = now
	}
	e.peersMu.Unlock()

	// PC 有 TUN，直接写入；enqueueTUN 内部有 VIP 校验
	e.enqueueTUN(payload)
}
