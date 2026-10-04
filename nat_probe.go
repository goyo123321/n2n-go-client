package main

import (
    "log"
    "net"
    "strconv"
    "time"

    "github.com/pion/stun/v2"
)

type NATMetadata struct {
    P2PEndpoint        string
    PublicEndpoint     string
    NATType            string
    PortsDifference    int
    RegularPortsChange bool
    Behavior           string
    AssistedSockets    []string
}

func probeNAT(localUDPPort int, stunServers []string) *NATMetadata {
    meta := &NATMetadata{
        // ★ 修复：P2PEndpoint 不再预设 0.0.0.0，STUN 成功后才填
        P2PEndpoint:     "",
        NATType:         "unknown",
        Behavior:        "BehaviorPortChanged",
        AssistedSockets: localLANAddrs(localUDPPort),
    }

    if len(stunServers) == 0 {
        stunServers = []string{"stun.l.google.com:19302", "stun.cloudflare.com:3478"}
    }

    type result struct {
        ip   string
        port int
    }
    var results []result

    for _, server := range stunServers {
        c, err := stun.Dial("udp", server)
        if err != nil {
            continue
        }

        // ★ 修复：显式生成 TransactionID，避免使用包级变量
        msg := stun.MustBuild(stun.NewTransactionID(), stun.BindingRequest)
        var xorAddr stun.XORMappedAddress

        // ★ 修复：给每个 STUN 服务器加 3 秒超时
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
        case <-time.After(3 * time.Second):
            log.Printf("[NAT] STUN %s 超时", server)
        }
        _ = c.Close()
        if xorAddr.Port == 0 {
            continue
        }

        results = append(results, result{ip: xorAddr.IP.String(), port: xorAddr.Port})
    }

    if len(results) == 0 {
        log.Printf("[NAT] 所有 STUN 探测失败，NAT 类型未知")
        return meta
    }

    meta.PublicEndpoint = net.JoinHostPort(results[0].ip, strconv.Itoa(results[0].port))
    // ★ 修复：用 STUN 结果作为 P2PEndpoint，而不是 0.0.0.0
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
        meta.NATType = "EasyNAT"
        meta.Behavior = "BehaviorNoChange"
        log.Printf("[NAT] 单 STUN 结果，保守判为 EasyNAT，pub=%s", meta.PublicEndpoint)
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

func abs(n int) int {
    if n < 0 {
        return -n
    }
    return n
}
