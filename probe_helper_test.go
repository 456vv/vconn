//go:build !plan9 && !js && !wasip1

package vconn

import (
	"net"
	"testing"
)

// uncomparableConn 值语义的 net.Conn，且含不可比较字段（slice）。
// 旧实现用 map[net.Conn]struct{} 做环检测，对这种连接会直接 panic。
type uncomparableConn struct {
	net.Conn
	buf []byte
}

// TestSyscallConnOf_UncomparableConn 对不可比较的连接类型做解包不得 panic。
func TestSyscallConnOf_UncomparableConn(t *testing.T) {
	s1, s2 := net.Pipe()
	defer s2.Close()

	c := uncomparableConn{Conn: s1, buf: []byte{1, 2, 3}}

	defer func() {
		if r := recover(); r != nil {
			t.Fatalf("解包不可比较的连接时 panic: %v", r)
		}
	}()

	if _, ok := syscallConnOf(c); ok {
		t.Fatal("net.Pipe 不应被识别为 syscall.Conn")
	}

	vc := New(c)
	defer vc.Close()
	if err := vc.CheckAlive(); err != nil {
		t.Fatalf("CheckAlive 应 fail-open 返回 nil，得到 %v", err)
	}
}

// TestSyscallConnOf_WrapperChain 包装链应能一路解包到 syscall.Conn
func TestSyscallConnOf_WrapperChain(t *testing.T) {
	vc, srv, ln := newTCPPair(t)
	defer ln.Close()
	defer srv.Close()
	defer vc.Close()

	inner := &netConnWrapper{Conn: vc.rwc}
	outer := &netConnWrapper{Conn: inner}

	sc, ok := syscallConnOf(outer)
	if !ok || sc == nil {
		t.Fatal("应能从包装链中解包出 syscall.Conn")
	}
}
