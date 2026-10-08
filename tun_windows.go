//go:build windows

package main

import (
	"fmt"
	"log"
	"os/exec"

	"golang.org/x/sys/windows"
	"golang.zx2c4.com/wintun"
)

type TUNDevice struct {
	adapter   *wintun.Adapter
	session   wintun.Session
	name      string
	virtualIP string
	mtu       int
}

func setupTUN(virtualIP, cidr, name string) (*TUNDevice, error) {
	if err := ensureWintunLoaded(); err != nil {
		return nil, fmt.Errorf("load wintun.dll: %w", err)
	}

	adapter, err := wintun.CreateAdapter(name, "Wintun", nil)
	if err != nil {
		return nil, fmt.Errorf("create wintun adapter: %w", err)
	}

	session, err := adapter.StartSession(0x800000)
	if err != nil {
		adapter.Close()
		return nil, fmt.Errorf("start session: %w", err)
	}

	// ★ 用配置的 CIDR 计算 netmask 和网络地址
	netmask := cidrToNetmask(cidr)
	networkAddr := cidrNetworkAddr(cidr)

	exec.Command("netsh", "interface", "ip", "set", "address",
		name, "static", virtualIP, netmask).Run()
	exec.Command("netsh", "interface", "ipv4", "set", "subinterface",
		name, "mtu=1400", "store=persistent").Run()
	exec.Command("route", "add", networkAddr, "mask", netmask, virtualIP).Run()

	log.Printf("[TUN] Wintun %s 已启动，IP: %s/%s，网段: %s", name, virtualIP, cidrPrefix(cidr), cidr)
	return &TUNDevice{
		adapter:   adapter,
		session:   session,
		name:      name,
		virtualIP: virtualIP,
		mtu:       1400,
	}, nil
}

func (t *TUNDevice) Read(buf []byte) (int, error) {
	for {
		packet, err := t.session.ReceivePacket()
		if err == nil {
			n := copy(buf, packet)
			t.session.ReleaseReceivePacket(packet)
			return n, nil
		}
		waitEvent := t.session.ReadWaitEvent()
		windows.WaitForSingleObject(waitEvent, windows.INFINITE)
	}
}

func (t *TUNDevice) Write(buf []byte) (int, error) {
	packet, err := t.session.AllocateSendPacket(len(buf))
	if err != nil {
		return 0, err
	}
	copy(packet, buf)
	t.session.SendPacket(packet)
	return len(buf), nil
}

func (t *TUNDevice) Close() error {
	t.session.End()
	return t.adapter.Close()
}

func (t *TUNDevice) Name() string { return t.name }
func (t *TUNDevice) MTU() int     { return t.mtu }
