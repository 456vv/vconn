//go:build !windows && !plan9 && !js && !wasip1

package vconn

import (
	"syscall"
	"testing"
)

// TestIsNetErrno_UnixDisconnect Unix 侧「真断开」错误码表
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
		t.Run(e.Error(), func(t *testing.T) { assertDisconnect(t, e) })
	}
}
