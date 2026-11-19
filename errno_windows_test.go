//go:build windows

package vconn

import (
	"syscall"
	"testing"
)

// Windows WSA 原始码（用数字常量，避免依赖未导出符号）
const (
	testWSAECONNRESET uintptr = 10054
	testWSAENETRESET  uintptr = 10052
)

// TestIsNetErrno_WindowsDisconnect 两套表示（WSA 原始码 + 归一化常量）都必须认
func TestIsNetErrno_WindowsDisconnect(t *testing.T) {
	for _, v := range []uintptr{testWSAECONNRESET, testWSAENETRESET} {
		t.Run("WSA", func(t *testing.T) { assertDisconnect(t, syscall.Errno(v)) })
	}
	for _, e := range []syscall.Errno{syscall.ECONNRESET, syscall.EPIPE, syscall.ECONNABORTED} {
		t.Run(e.Error(), func(t *testing.T) { assertDisconnect(t, e) })
	}
}
