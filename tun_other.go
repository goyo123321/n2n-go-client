//go:build !linux && !darwin && !windows

package main

import "errors"

type TUNDevice struct{}

func setupTUN(virtualIP, name string) (*TUNDevice, error) {
    return nil, errors.New("当前平台不支持 TUN")
}

func (t *TUNDevice) Read(buf []byte) (int, error)  { return 0, errors.New("not supported") }
func (t *TUNDevice) Write(buf []byte) (int, error) { return 0, errors.New("not supported") }
func (t *TUNDevice) Close() error                  { return nil }
func (t *TUNDevice) Name() string                  { return "" }
func (t *TUNDevice) MTU() int                      { return 1400 }
