package main

import (
	"log"
	"net"
	"strconv"
	"time"

	"github.com/pion/stun/v3"
)

type NATMetadata struct {
	P2PEndpoint        string
	PublicEndpoint     string
	NATType            string
	PortsDifference    int
	RegularPortsChange bool
	Behavior           string
	MultiExit          bool
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
		log.Printf("[NAT] EasyNAT（端口保持），pub=%s", meta.PublicEndpoint)
	} else if len(results) >= 2 {
		meta.NATType = "HardNAT"
		meta.PortsDifference = abs(results[0].port - results[1].port)
		meta.RegularPortsChange = true
		log.Printf("[NAT] HardNAT（对称），ports_diff=%d pub=%s", meta.PortsDifference, meta.PublicEndpoint)
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
