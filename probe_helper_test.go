//go:build !plan9 && !js && !wasip1

package vconn

import (
	"net"
	"syscall"
	"testing"
)

// directConn 让 isDirectSyscallConn 返回 true 的最小实现
type directConn struct{ net.Conn }

func (d *directConn) SyscallConn() (syscall.RawConn, error) { return nil, nil }

// TestSyscallConnOf 包装链解包 + 不可比较类型不 panic
func TestSyscallConnOf(t *testing.T) {
	t.Run("不可比较的连接不 panic", func(t *testing.T) {
		s1, s2 := pipePair()
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
		if st, err := vc.Check(); err != nil || st.Class() != StatusUnverified {
			t.Fatalf("Check 应 fail-open 为 StatusUnverified，得到 (%v, %v)", st, err)
		}
	})

	t.Run("包装链可一路解包", func(t *testing.T) {
		vc, srv, ln := tcpPair(t)
		defer ln.Close()
		defer srv.Close()
		defer vc.Close()

		inner := &netConnWrapper{Conn: vc.rwc}
		outer := &netConnWrapper{Conn: inner}
		if sc, ok := syscallConnOf(outer); !ok || sc == nil {
			t.Fatal("应能从包装链解包出 syscall.Conn")
		}
	})
}

// TestProbe_ResolvePeekBlock 预读策略表
func TestProbe_ResolvePeekBlock(t *testing.T) {
	wrap, wrapPeer := pipePair()
	defer wrapPeer.Close()
	direct := &directConn{Conn: wrap}

	cases := []struct {
		name       string
		c          net.Conn
		res        probeResult
		allowBlock bool
		wantSkip   bool
		wantBlock  bool
	}{
		{"可读/直接", direct, probeReadable, false, false, false},
		{"可读/包装", wrap, probeReadable, true, true, false},
		{"空闲/直接/允许阻塞", direct, probeIdle, true, false, true},
		{"空闲/直接/不允许", direct, probeIdle, false, true, false},
		{"空闲/包装", wrap, probeIdle, true, true, false},
		{"不支持/叶子", wrap, probeUnsupported, false, false, true},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			skip, block := resolvePeekBlock(tc.c, tc.res, tc.allowBlock)
			if skip != tc.wantSkip || block != tc.wantBlock {
				t.Fatalf("got (skip=%v, block=%v), want (%v, %v)", skip, block, tc.wantSkip, tc.wantBlock)
			}
		})
	}
}
