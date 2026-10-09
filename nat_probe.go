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
    AssistedSockets    []string
    MultiExit          bool // ★ 新增
}

func probeNAT(localUDPPort int, stunServers []string) *NATMetadata {
    meta := &NATMetadata{
        P2PEndpoint:     "",
        NATType:         "unknown",
        Behavior:        "BehaviorPortChanged",
        AssistedSockets: localLANAddrs(localUDPPort),
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

    // 每个 STUN 加 2s 超时，拿到 2 个样本就提前退出
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
        // ★ 单样本 → unknown，不猜 EasyNAT
        //   猜错会让服务端按错误模式派发指令，且两端判定不一致
        meta.NATType = "unknown"
        meta.Behavior = "BehaviorPortChanged"
        log.Printf("[NAT] 单 STUN 结果，NAT 类型未知，pub=%s", meta.PublicEndpoint)
    }

    return meta
}

func localLANAddrs(port int) []string {
    var out []string
    ifaces, err := net.Interfaces()
    if err != nil {
        return out
    }
    for _, iface := range ifaces {
        if iface.Flags&net.FlagUp == 0 || iface.Flags&net.FlagLoopback != 0 {
            continue
        }
        addrs, _ := iface.Addrs()
        for _, a := range addrs {
            ipnet, ok := a.(*net.IPNet)
            if !ok {
                continue
            }
            ip4 := ipnet.IP.To4()
            if ip4 == nil {
                continue
            }
            out = append(out, net.JoinHostPort(ip4.String(), strconv.Itoa(port)))
        }
    }
    return out
}

// extractLanIPs 从 AssistedSockets（"IP:Port"）提取纯 IP 列表。
func extractLanIPs(sockets []string) []string {
    var out []string
    seen := make(map[string]bool)
    for _, s := range sockets {
        host, _, err := net.SplitHostPort(s)
        if err != nil {
            continue
        }
        ip := net.ParseIP(host)
        if ip == nil {
            continue
        }
        ip4 := ip.To4()
        if ip4 == nil {
            continue
        }
        ipStr := ip4.String()
        if seen[ipStr] {
            continue
        }
        seen[ipStr] = true
        out = append(out, ipStr)
    }
    return out
}

func abs(n int) int {
    if n < 0 {
        return -n
    }
    return n
}
