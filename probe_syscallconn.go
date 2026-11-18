//go:build !plan9 && !js && !wasip1

package vconn

import "syscall"

// SyscallConn 转发底层 syscall.Conn，避免 WrapSafeBlockProbe 把
// *net.TCPConn 等本来可做内核探测的连接降级为阻塞预读。
//
// 底层不是 syscall.Conn（如 net.Pipe）时返回 ErrUnsupported；
// 调用方（probeConn）会把它归类为 probeUnsupported，再按叶子连接规则处理。
func (c safeBlockConn) SyscallConn() (syscall.RawConn, error) {
	if sc, ok := c.Conn.(syscall.Conn); ok {
		return sc.SyscallConn()
	}
	return nil, ErrUnsupported
}
