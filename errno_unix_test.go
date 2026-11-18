//go:build !windows && !plan9 && !js && !wasip1

package vconn

import (
	"net"
	"syscall"
	"testing"
)

// TestIsNetErrno_UnixDisconnect 确认 Unix 侧「真断开」错误码仍在表内。
func TestIsNetErrno_UnixDisconnect(t *testing.T) {
	for _, e := range []syscall.Errno{
		syscall.ECONNRESET,
		syscall.ECONNABORTED,
		syscall.EPIPE,
		syscall.ENETRESET,
		syscall.ENETDOWN,
		syscall.ENETUNREACH,
		syscall.EHOSTUNREACH,
	} {
		if !isNetErrno(e) {
			t.Fatalf("%v 应被判定为网络断开", e)
		}
		if !isCommonNetError(&net.OpError{Op: "read", Err: e}) {
			t.Fatalf("包装后的 %v 应被判定为断开", e)
		}
	}
}
