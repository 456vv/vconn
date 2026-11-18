//go:build !plan9 && !js && !wasip1

package vconn

import (
	"net"
	"syscall"
)

// syscallConnOf 从连接（或可解包的包装，如 *tls.Conn）中取出 syscall.Conn
func syscallConnOf(c net.Conn) (syscall.Conn, bool) {
	for i := 0; i < 8 && c != nil; i++ {
		if sc, ok := c.(syscall.Conn); ok {
			return sc, true
		}
		if nc, ok := c.(interface{ NetConn() net.Conn }); ok {
			next := nc.NetConn()
			if next == nil {
				return nil, false
			}
			c = next
			continue
		}
		return nil, false
	}
	return nil, false
}

// isDirectSyscallConn 是否为可直接读取的 syscall.Conn（无需解包，如 *net.TCPConn）
func isDirectSyscallConn(c net.Conn) bool {
	_, ok := c.(syscall.Conn)
	return ok
}

// isBlockProbeSafe 判断能否对该连接发起「带超时的阻塞预读探测」。
//
// 规则（顺序敏感）：
//  1. 直接 syscall.Conn（*net.TCPConn 等）→ 可以；
//  2. 显式实现 SafeBlockProber → 以它的答复为准；
//  3. 包装型连接（NetConn()）→ 不可以（TLS/多路复用等，安全优先）；
//  4. 其它叶子连接（net.Pipe、内存管道、自定义底层连接）→ 可以。
//
// 第 4 条是 SetBackgroundReadBuffer 文档承诺的语义：对 net.Pipe 这类
// 没有内核就绪事件的连接，必须靠「带短超时的阻塞预读」才能发现对端关闭。
func isBlockProbeSafe(c net.Conn) bool {
	if c == nil {
		return false
	}
	if isDirectSyscallConn(c) {
		return true
	}
	if sp, ok := c.(SafeBlockProber); ok {
		return sp.SafeToBlockProbe()
	}
	if isWrapperConn(c) {
		return false
	}
	return true
}
