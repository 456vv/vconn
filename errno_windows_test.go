//go:build windows

package vconn

import (
	"net"
	"syscall"
	"testing"
)

// Windows WSA 错误码（用数字常量，避免依赖未导出的符号）
const (
	testWSAECONNRESET uintptr = 10054
	testWSAENETRESET  uintptr = 10052
)

// TestIsNetErrno_WindowsDisconnect 确认 Windows 侧「真断开」错误码仍在表内。
func TestIsNetErrno_WindowsDisconnect(t *testing.T) {
	for _, v := range []uintptr{testWSAECONNRESET, testWSAENETRESET} {
		e := syscall.Errno(v)
		if !isNetErrno(e) {
			t.Fatalf("%v 应被判定为网络断开", e)
		}
		if !isCommonNetError(&net.OpError{Op: "read", Err: e}) {
			t.Fatalf("包装后的 %v 应被判定为断开", e)
		}
	}
}
