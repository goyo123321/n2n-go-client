package main

// PC 端 LAN 地址收集。

import (
	"net"
	"sort"
	"strconv"
)

func collectLANEndpoints(port int, selfVIP string) []string {
	var result []string
	ifaces, err := net.Interfaces()
	if err != nil {
		return nil
	}
	seen := make(map[string]bool)
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
			ip4 := ip.To4()
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
			ep := net.JoinHostPort(ip4.String(), strconv.Itoa(port))
			if seen[ep] {
				continue
			}
			seen[ep] = true
			result = append(result, ep)
		}
	}
	return result
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
