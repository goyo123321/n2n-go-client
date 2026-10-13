package main

// PC 端 LAN 地址收集。

import (
	"log"
	"net"
	"sort"
	"strconv"
)

// collectLANEndpoints 收集本机所有 LAN 端点。
//
// ★ 双路径：
//   1. UDP connect 骗内核返回出口 IP —— 永远可靠
//   2. 兜底遍历 net.Interfaces() —— 覆盖多网卡
func collectLANEndpoints(port int, selfVIP string) []string {
	var result []string
	seen := make(map[string]bool)

	add := func(ip4 net.IP) {
		if ip4 == nil {
			return
		}
		if selfVIP != "" && ip4.String() == selfVIP {
			return
		}
		if isCGNATOverlay(ip4) {
			return
		}
		if !isPrivateIPv4(ip4) {
			return
		}
		if ip4.IsLoopback() || ip4.IsLinkLocalUnicast() {
			return
		}
		ep := net.JoinHostPort(ip4.String(), strconv.Itoa(port))
		if seen[ep] {
			return
		}
		seen[ep] = true
		result = append(result, ep)
	}

	// 路径 1：UDP connect 骗内核
	if ip := detectOutboundIP(); ip != nil {
		add(ip)
	}

	// 路径 2：遍历接口
	ifaces, err := net.Interfaces()
	if err == nil {
		for _, iface := range ifaces {
			if iface.Flags&net.FlagUp == 0 || iface.Flags&net.FlagLoopback != 0 {
				continue
			}
			addrs, err := iface.Addrs()
			if err != nil {
				continue
			}
			for _, addr := range addrs {
				var ip net.IP
				switch v := addr.(type) {
				case *net.IPNet:
					ip = v.IP
				case *net.IPAddr:
					ip = v.IP
				}
				if ip == nil || ip.To4() == nil {
					continue
				}
				add(ip.To4())
			}
		}
	}

	return result
}

// detectOutboundIP 用一个不真正发包的 UDP connect 骗内核选出出口网卡。
//
// 原理：connect UDP 只更新内核 socket 的路由缓存，不触发 ARP、不发包。
// getsockname() 返回内核为该 socket 选定的本地 IP —— 即"出网卡"的 IP。
func detectOutboundIP() net.IP {
	targets := []string{
		"8.8.8.8:80",
		"1.1.1.1:80",
		"114.114.114.114:80",
	}
	for _, t := range targets {
		conn, err := net.Dial("udp4", t)
		if err != nil {
			continue
		}
		localAddr, ok := conn.LocalAddr().(*net.UDPAddr)
		_ = conn.Close()
		if !ok || localAddr == nil || localAddr.IP == nil {
			continue
		}
		ip4 := localAddr.IP.To4()
		if ip4 != nil {
			return ip4
		}
	}
	return nil
}

func isPrivateIPv4(ip net.IP) bool {
	ip4 := ip.To4()
	if ip4 == nil {
		return false
	}
	if ip4[0] == 10 {
		return true
	}
	if ip4[0] == 172 && ip4[1] >= 16 && ip4[1] <= 31 {
		return true
	}
	if ip4[0] == 192 && ip4[1] == 168 {
		return true
	}
	return false
}

func isCGNATOverlay(ip net.IP) bool {
	ip4 := ip.To4()
	if ip4 == nil {
		return false
	}
	return ip4[0] == 100 && ip4[1] >= 64 && ip4[1] <= 127
}

func localPrefixes(selfVIP string) []*net.IPNet {
	var result []*net.IPNet
	ifaces, err := net.Interfaces()
	if err != nil {
		return nil
	}
	for _, iface := range ifaces {
		if iface.Flags&net.FlagUp == 0 || iface.Flags&net.FlagLoopback != 0 {
			continue
		}
		addrs, err := iface.Addrs()
		if err != nil {
			continue
		}
		for _, addr := range addrs {
			ipnet, ok := addr.(*net.IPNet)
			if !ok || ipnet.IP.To4() == nil {
				continue
			}
			ip4 := ipnet.IP.To4()
			if isCGNATOverlay(ip4) {
				continue
			}
			if selfVIP != "" && ip4.String() == selfVIP {
				continue
			}
			if !isPrivateIPv4(ip4) {
				continue
			}
			if ip4.IsLoopback() || ip4.IsLinkLocalUnicast() {
				continue
			}
			result = append(result, ipnet)
		}
	}
	return result
}

func sortLANByAffinity(lan []string, prefixes []*net.IPNet) []string {
	if len(lan) < 2 || len(prefixes) == 0 {
		return lan
	}
	type entry struct {
		ep    string
		score int
	}
	scored := make([]entry, len(lan))
	for i, ep := range lan {
		host, _, err := net.SplitHostPort(ep)
		if err != nil {
			host = ep
		}
		ip := net.ParseIP(host)
		score := 0
		if ip != nil && !isCGNATOverlay(ip) {
			for _, p := range prefixes {
				if p.Contains(ip) {
					ones, _ := p.Mask.Size()
					if ones > score {
						score = ones
					}
				}
			}
		}
		scored[i] = entry{ep, score}
	}
	sort.SliceStable(scored, func(i, j int) bool {
		return scored[i].score > scored[j].score
	})
	out := make([]string, len(scored))
	for i, s := range scored {
		out[i] = s.ep
	}
	return out
}

// 保留 log 引用（未使用时不报错）
var _ = log.Printf
