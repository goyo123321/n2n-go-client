//go:build darwin

package main

import (
    "encoding/binary"
    "fmt"
    "log"
    "os/exec"
    "syscall"
    "unsafe"

    "golang.org/x/sys/unix"
)

const (
    utunControlName = "com.apple.net.utun_control"
    utunOptIfName   = 2
    // SYSPROTO_CONTROL = 2，来自 macOS <sys/kern_control.h>。
    // 不用 unix.SYSPROTO_CONTROL（该常量在部分 x/sys 版本里未导出）。
    sysprotoControl = 2
)

type TUNDevice struct {
    fd        int
    name      string
    virtualIP string
    mtu       int
}

func setupTUN(virtualIP, name string) (*TUNDevice, error) {
    fd, ifName, err := openUTun(name)
    if err != nil {
        return nil, fmt.Errorf("open utun: %w", err)
    }

    if out, err := exec.Command("ifconfig", ifName, "inet", virtualIP, "10.64.0.1", "up").CombinedOutput(); err != nil {
        unix.Close(fd)
        return nil, fmt.Errorf("ifconfig inet failed: %v: %s", err, string(out))
    }
    if out, err := exec.Command("ifconfig", ifName, "mtu", "1400").CombinedOutput(); err != nil {
        log.Printf("[TUN] 设置 MTU 失败（可忽略）: %v: %s", err, string(out))
    }
    if out, err := exec.Command("route", "add", "-net", "10.64.0.0/24", "-interface", ifName).CombinedOutput(); err != nil {
        unix.Close(fd)
        return nil, fmt.Errorf("route add failed: %v: %s", err, string(out))
    }

    log.Printf("[TUN] %s 已启动，IP: %s", ifName, virtualIP)
    return &TUNDevice{fd: fd, name: ifName, virtualIP: virtualIP, mtu: 1400}, nil
}

func openUTun(name string) (int, string, error) {
    fd, err := unix.Socket(unix.AF_SYSTEM, unix.SOCK_DGRAM, 2)
    if err != nil {
        return -1, "", err
    }

    ctlInfo := unix.CtlInfo{}
    copy(ctlInfo.Name[:], utunControlName)
    if err := unix.IoctlCtlInfo(fd, &ctlInfo); err != nil {
        unix.Close(fd)
        return -1, "", err
    }

    sa := &unix.SockaddrCtl{ID: ctlInfo.Id, Unit: 0}
    if len(name) > 4 && name[:4] == "utun" {
        var n uint32
        fmt.Sscanf(name[4:], "%d", &n)
        sa.Unit = n + 1
    }

    if err := unix.Connect(fd, sa); err != nil {
        unix.Close(fd)
        return -1, "", err
    }

    var buf [16]byte
    bufLen := uint32(len(buf))
    _, _, errno := syscall.Syscall6(
        syscall.SYS_GETSOCKOPT,
        uintptr(fd),
        uintptr(sysprotoControl), // ← 用本地常量
        uintptr(utunOptIfName),
        uintptr(unsafe.Pointer(&buf[0])),
        uintptr(unsafe.Pointer(&bufLen)),
        0,
    )
    if errno != 0 {
        unix.Close(fd)
        return -1, "", errno
    }

    ifName := string(buf[:bufLen-1])
    return fd, ifName, nil
}

func (t *TUNDevice) Read(buf []byte) (int, error) {
    n, err := unix.Read(t.fd, buf)
    if err != nil {
        return 0, err
    }
    if n <= 4 {
        return 0, nil
    }
    copy(buf, buf[4:n])
    return n - 4, nil
}

func (t *TUNDevice) Write(buf []byte) (int, error) {
    out := make([]byte, len(buf)+4)
    if len(buf) > 0 {
        if buf[0]>>4 == 4 {
            binary.BigEndian.PutUint32(out[:4], unix.AF_INET)
        } else if buf[0]>>4 == 6 {
            binary.BigEndian.PutUint32(out[:4], unix.AF_INET6)
        }
    }
    copy(out[4:], buf)
    return unix.Write(t.fd, out)
}

func (t *TUNDevice) Close() error { return unix.Close(t.fd) }
func (t *TUNDevice) Name() string { return t.name }
func (t *TUNDevice) MTU() int     { return t.mtu }
