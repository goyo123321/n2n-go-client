package main

// 网络切换（WiFi ↔ Cellular / 出口 IP 变化）时的状态清理。
//
// PC / 路由器没有 Android ConnectivityManager，用 10 秒定时探测
// 本机出口 IP 变化，变了就触发 OnNetworkChanged。

import (
	"context"
	"log"
	"sync/atomic"
	"time"
)

const networkChangeMergeDelay = 1500 * time.Millisecond
const networkProbeInterval = 10 * time.Second

// OnNetworkChanged 网络切换入口（1.5 秒合并）。
func (e *Edge) OnNetworkChanged(networkType string) {
	e.pendingNetChangeMu.Lock()
	e.pendingNetChange = networkType
	if e.pendingNetChangeT != nil {
		e.pendingNetChangeT.Stop()
	}
	e.pendingNetChangeT = time.AfterFunc(networkChangeMergeDelay, func() {
		defer func() {
			if r := recover(); r != nil {
				log.Printf("[Network] pending apply panic: %v", r)
			}
		}()
		e.pendingNetChangeMu.Lock()
		t := e.pendingNetChange
		e.pendingNetChange = ""
		e.pendingNetChangeT = nil
		e.pendingNetChangeMu.Unlock()
		if t != "" {
			e.applyNetworkChanged(t)
		}
	})
	e.pendingNetChangeMu.Unlock()

	log.Printf("[Network] %s 已排队（1.5s 内合并）", networkType)
}

// applyNetworkChanged 实际执行 10 步清理。
func (e *Edge) applyNetworkChanged(networkType string) {
	log.Printf("[Network] 执行网络变化处理: %s", networkType)

	e.mu.Lock()
	e.lastNetworkChangedAt = time.Now().UnixMilli()
	e.lastNetworkType = networkType
	e.mu.Unlock()

	// 1. 清空旧 natMeta
	e.mu.Lock()
	if e.natMeta != nil {
		oldEndpoint := e.natMeta.PublicEndpoint
		e.natMeta.PublicEndpoint = ""
		e.natMeta.P2PEndpoint = ""
		e.natMeta.AllEndpoints = nil
		e.natMeta.NATType = "unknown"
		e.natMeta.Behavior = "BehaviorPortChanged"
		e.natMeta.MultiExit = false
		e.natMeta.PortsDifference = 0
		e.natMeta.RegularPortsChange = false
		if oldEndpoint != "" {
			log.Printf("[Network] 清空旧 natMeta.PublicEndpoint=%q", oldEndpoint)
		}
	}
	e.mu.Unlock()

	// 2. 清打洞失败计数
	natHoleActiveMu.Lock()
	failCount := len(failCounts)
	if failCount > 0 {
		failCounts = make(map[string]int)
	}
	nActive := len(natHoleActive)
	if nActive > 0 {
		natHoleActive = make(map[string]bool)
	}
	natHoleActiveMu.Unlock()

	// 3. 重置 relayMgr
	if e.relayMgr != nil {
		e.relayMgr.ResetP2PStates("network-changed")
	}

	// 4. 清所有 peer
	e.peersMu.Lock()
	count := 0
	for _, p := range e.peers {
		if p.UDPAddr != nil {
			count++
		}
		p.UDPAddr = nil
		p.lastLANRecvAt = 0
		p.lastRecvAt = 0
		p.lastP2PRecvAt = 0
		p.LanEndpoints = nil
		p.TurnRelayAddr = ""
	}
	e.peersMu.Unlock()
	log.Printf("[Network] 清空 %d 个 peer 的 UDPAddr/LanEndpoints/TurnRelayAddr", count)

	// 5. 取消所有 pending timer
	e.fallbackTimersMu.Lock()
	for _, t := range e.fallbackTimers {
		t.Stop()
	}
	e.fallbackTimers = make(map[string]*time.Timer)
	e.fallbackTimersMu.Unlock()

	e.p2pAvailTimersMu.Lock()
	for _, t := range e.p2pAvailTimers {
		t.Stop()
	}
	e.p2pAvailTimers = make(map[string]*time.Timer)
	e.p2pAvailTimersMu.Unlock()

	// 6. 立即上报 metadata（空地址）
	atomic.StoreInt64(&e.lastMetadataReportAt, 0)
	e.reportMetadata()

	// 7. 通知服务端
	if e.ws != nil {
		_ = e.ws.Send(map[string]interface{}{
			"type":   "network_changed",
			"reason": networkType,
		})
		log.Printf("[Network] 已通知服务端重新协调")
	}

	// 8. 立即上报 connection_status
	if e.relayMgr != nil {
		e.relayMgr.ForceReportNow()
	}

	// 9. 触发 STUN 重探
	safeGo("nat-reprobe", func() {
		time.Sleep(500 * time.Millisecond)
		if e.udpConn == nil {
			return
		}
		meta := probeNAT(e.udpPort, nil)
		if meta == nil {
			return
		}
		e.mu.Lock()
		e.natMeta = meta
		e.mu.Unlock()
		log.Printf("[NAT] 重探测完成: %s pub=%s", meta.NATType, meta.PublicEndpoint)
		e.reportMetadata()
	})

	// 10. 立即重建 TURN allocation
	safeGo("turn-rebuild", func() {
		time.Sleep(500 * time.Millisecond)
		if e.turnClient == nil {
			return
		}
		ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
		defer cancel()

		log.Printf("[TURN] 网络变化，开始重建 allocation...")
		if err := e.turnClient.RebuildAllocation(ctx); err != nil {
			log.Printf("[TURN] 网络变化后重建失败: %v（走 WS 兜底，30 秒后 StartReconnectLoop 重试）", err)
			return
		}
		log.Printf("[TURN] ✅ 网络变化后重建成功: %s", e.turnClient.GetRelayAddr())

		if e.ws != nil {
			_ = e.ws.Send(map[string]interface{}{
				"type":      "turn_relay_info",
				"relayAddr": e.turnClient.GetRelayAddr(),
			})
			log.Printf("[TURN] 已上报新 relay 地址给服务端")
		}

		if e.relayMgr != nil {
			e.relayMgr.UpgradeRelaysToTURN()
		}
	})
}

// startNetworkMonitor 定时探测本机出口 IP 变化（10 秒周期）。
func (e *Edge) startNetworkMonitor() {
	if e.udpConn == nil {
		return
	}

	log.Printf("[Network] 启动网络变化探测（每 %v）", networkProbeInterval)

	safeGo("net-monitor", func() {
		ticker := time.NewTicker(networkProbeInterval)
		defer ticker.Stop()

		var lastIP string
		if ip := detectOutboundIP(); ip != nil {
			lastIP = ip.String()
			log.Printf("[Network] 初始出口 IP: %s", lastIP)
		}

		for {
			select {
			case <-e.doneCh:
				log.Printf("[Network] 探测已停止")
				return
			case <-ticker.C:
				ip := detectOutboundIP()
				if ip == nil {
					continue
				}
				cur := ip.String()

				if lastIP == "" {
					lastIP = cur
					continue
				}
				if cur != lastIP {
					log.Printf("[Network] 出口 IP 变化 %s → %s", lastIP, cur)
					e.OnNetworkChanged("ip-change:" + cur)
					lastIP = cur
				}
			}
		}
	})
}
