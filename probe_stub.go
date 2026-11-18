//go:build plan9 || js || wasip1

package vconn

import "net"

// probeConn 该平台不支持内核级探测，退化为"带自身超时的阻塞预读"
func probeConn(net.Conn) (probeResult, error) {
	return probeUnsupported, nil
}

// isDirectSyscallConn 该平台无 syscall.Conn
func isDirectSyscallConn(net.Conn) bool { return false }

// isBlockProbeSafe 该平台无 syscall.Conn：叶子连接走阻塞预读，包装型连接不碰
func isBlockProbeSafe(c net.Conn) bool {
	if c == nil {
		return false
	}
	if sp, ok := c.(SafeBlockProber); ok {
		return sp.SafeToBlockProbe()
	}
	if isWrapperConn(c) {
		return false
	}
	return true
}
