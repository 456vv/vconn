//go:build solaris || illumos || aix

package vconn

import (
	"net"
	"os"
	"testing"
	"time"
)

// fileNetConn 把 os.File（非 socket fd）伪装成 net.Conn，
// 用于验证 probe_unix 对 ENOTSOCK 的处理。
type fileNetConn struct{ *os.File }

func (c *fileNetConn) LocalAddr() net.Addr              { return nil }
func (c *fileNetConn) RemoteAddr() net.Addr             { return nil }
func (c *fileNetConn) SetDeadline(time.Time) error      { return nil }
func (c *fileNetConn) SetReadDeadline(time.Time) error  { return nil }
func (c *fileNetConn) SetWriteDeadline(time.Time) error { return nil }

// TestProbeConn_NonSocketUnsupported 非 socket 的 syscall.Conn 必须返回
// probeUnsupported，不能误判为 probeClosed。
func TestProbeConn_NonSocketUnsupported(t *testing.T) {
	r, w, err := os.Pipe()
	if err != nil {
		t.Fatal(err)
	}
	defer r.Close()
	defer w.Close()

	res, perr := probeConn(&fileNetConn{File: r})
	if res == probeClosed {
		t.Fatalf("非 socket fd 被判为已关闭: %v", perr)
	}
	if res != probeUnsupported {
		t.Fatalf("期望 probeUnsupported，得到 %v（err=%v）", res, perr)
	}
}
