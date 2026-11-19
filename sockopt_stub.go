//go:build plan9 || js || wasip1

package vconn

import "net"

// socketError 该平台读不到 SO_ERROR。
// 返回 ErrUnsupported 表示「无法探测」，checkKernel 会把它转成 probeUnsupported，
// 绝不能当成连接已经断开。
func socketError(net.Conn) error { return ErrUnsupported }
