//go:build plan9 || js || wasip1

package vconn

import "net"

// probeConn 该平台没有可用的零消耗内核探测，固定返回 probeUnsupported。
// 监控与 CheckBlocking 会在确认阻塞读安全后，退化为带自身超时的预读；
// 包装型连接默认不读，返回 StatusUnverified。
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
