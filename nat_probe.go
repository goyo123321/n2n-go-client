package main

import (
	"encoding/binary"
	"log"
	"net"
	"strconv"
	"time"

	"github.com/pion/stun/v3"
)

type NATMetadata struct {
	P2PEndpoint        string
	PublicEndpoint     string
	AllEndpoints       []string
	NATType            string
	PortsDifference    int
	RegularPortsChange bool
	Behavior           string
	MultiExit          bool
}

// parseSTUNResponse 解析 STUN Binding Response 里的 XOR-MAPPED-ADDRESS。
//
// 输入：udpConn 收到的任意 UDP 包
// 输出：(publicIP, publicPort, ok)
//
// 只识别 msgType=0x0101（Binding Success Response），其他一律返回 false。
func parseSTUNResponse(data []byte) (string, int, bool) {
	if len(data) < 20 {
		return "", 0, false
	}
	if data[0] != 0x01 || data[1] != 0x01 {
		return "", 0, false
	}
	if binary.BigEndian.Uint32(data[4:8]) != 0x2112A442 {
		return "", 0, false
	}

	msgLen := int(binary.BigEndian.Uint16(data[2:4]))
	end := 20 + msgLen
	if end > len(data) {
		end = len(data)
	}

	offset := 20
	for offset+4 <= end {
		typ := binary.BigEndian.Uint16(data[offset : offset+2])
		l := int(binary.BigEndian.Uint16(data[offset+2 : offset+4]))
		if offset+4+l > end {
			break
		}
		// XOR-MAPPED-ADDRESS = 0x0020
		if typ == 0x0020 {
			ip, port, err := xorDecodePeer(data[offset+4 : offset+4+l])
			if err == nil && ip.To4() != nil {
				return ip.To4().String(), port, true
			}
		}
		offset += 4 + ((l + 3) &^ 3)
	}
	return "", 0, false
}

func probeNAT(localUDPPort int, stunServers []string) *NATMetadata {
	meta := &NATMetadata{
		P2PEndpoint: "",
		NATType:     "unknown",
		Behavior:    "BehaviorPortChanged",
	}

	if len(stunServers) == 0 {
		stunServers = []string{
			"74.125.250.129:19302",
			"162.159.207.0:3478",
			"stun.l.google.com:19302",
			"stun.cloudflare.com:3478",
		}
	}

	type result struct {
		ip   string
		port int
	}
	var results []result

	for _, server := range stunServers {
		if len(results) >= 2 {
			break
		}

		c, err := stun.Dial("udp", server)
		if err != nil {
			continue
		}

		msg := stun.MustBuild(stun.TransactionID, stun.BindingRequest)
		var xorAddr stun.XORMappedAddress

		done := make(chan struct{})
		go func() {
			defer close(done)
			_ = c.Do(msg, func(res stun.Event) {
				if res.Error != nil {
					return
				}
				_ = xorAddr.GetFrom(res.Message)
			})
		}()
		select {
		case <-done:
		case <-time.After(2 * time.Second):
			log.Printf("[NAT] STUN %s 超时", server)
		}
		_ = c.Close()

		if xorAddr.Port == 0 {
			continue
		}
		ip4 := xorAddr.IP.To4()
		if ip4 == nil {
			continue
		}
		results = append(results, result{ip: ip4.String(), port: xorAddr.Port})
		log.Printf("[NAT] STUN %s → %s:%d", server, ip4.String(), xorAddr.Port)
	}

	if len(results) == 0 {
		log.Printf("[NAT] 所有 STUN 探测失败，NAT 类型未知")
		return meta
	}

	meta.PublicEndpoint = net.JoinHostPort(results[0].ip, strconv.Itoa(results[0].port))
	meta.P2PEndpoint = meta.PublicEndpoint

	// 收集所有端点（去重）
	seen := make(map[string]bool)
	for _, r := range results {
		ep := net.JoinHostPort(r.ip, strconv.Itoa(r.port))
		if !seen[ep] {
			seen[ep] = true
			meta.AllEndpoints = append(meta.AllEndpoints, ep)
		}
	}

	firstPort := results[0].port
	allSame := true
	for _, r := range results {
		if r.port != firstPort {
			allSame = false
			break
		}
	}

	if allSame && len(results) >= 2 {
		meta.NATType = "EasyNAT"
		meta.Behavior = "BehaviorNoChange"
		meta.PortsDifference = 0
		log.Printf("[NAT] EasyNAT（端口保持），pub=%s endpoints=%d",
			meta.PublicEndpoint, len(meta.AllEndpoints))
	} else if len(results) >= 2 {
		meta.NATType = "HardNAT"
		meta.PortsDifference = abs(results[0].port - results[1].port)
		meta.RegularPortsChange = true
		log.Printf("[NAT] HardNAT（对称），ports_diff=%d pub=%s endpoints=%d",
			meta.PortsDifference, meta.PublicEndpoint, len(meta.AllEndpoints))
	} else {
		meta.NATType = "unknown"
		meta.Behavior = "BehaviorPortChanged"
		log.Printf("[NAT] 单 STUN 结果，NAT 类型未知，pub=%s", meta.PublicEndpoint)
	}

	return meta
}

func abs(n int) int {
	if n < 0 {
		return -n
	}
	return n
}
