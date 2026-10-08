//go:build linux

package main

import (
	"bytes"
	"encoding/binary"
	"fmt"
	"log"
	"os"
	"os/exec"
	"syscall"
	"unsafe"
)

type TUNDevice struct {
	file      *os.File
	name      string
	virtualIP string
	mtu       int
}

func setupTUN(virtualIP, cidr, name string) (*TUNDevice, error) {
	file, err := os.OpenFile("/dev/net/tun", os.O_RDWR, 0)
	if err != nil {
		return nil, fmt.Errorf("open /dev/net/tun: %w", err)
	}

	var ifr [40]byte
	copy(ifr[:16], name)
	binary.LittleEndian.PutUint16(ifr[16:18], 0x0001|0x1000)

	_, _, errno := syscall.Syscall(
		syscall.SYS_IOCTL,
		file.Fd(),
		uintptr(0x400454ca),
		uintptr(unsafe.Pointer(&ifr[0])),
	)
	if errno != 0 {
		file.Close()
		return nil, fmt.Errorf("TUNSETIFF: %v", errno)
	}

	dev := string(bytes.TrimRight(ifr[:16], "\x00"))

	// ★ 用配置的 CIDR 设置地址和路由
	prefix := cidrPrefix(cidr)
	exec.Command("ip", "addr", "add", virtualIP+"/"+prefix, "dev", dev).Run()
	exec.Command("ip", "link", "set", dev, "up").Run()
	exec.Command("ip", "link", "set", dev, "mtu", "1400").Run()
	exec.Command("ip", "route", "add", cidr, "dev", dev).Run()

	log.Printf("[TUN] %s 已启动，IP: %s/%s，网段: %s", dev, virtualIP, prefix, cidr)
	return &TUNDevice{file: file, name: dev, virtualIP: virtualIP, mtu: 1400}, nil
}

func (t *TUNDevice) Read(buf []byte) (int, error)  { return t.file.Read(buf) }
func (t *TUNDevice) Write(buf []byte) (int, error) { return t.file.Write(buf) }
func (t *TUNDevice) Close() error                  { return t.file.Close() }
func (t *TUNDevice) Name() string                  { return t.name }
func (t *TUNDevice) MTU() int                      { return t.mtu }
