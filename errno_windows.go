//go:build windows

package vconn

import (
	"syscall"

	"golang.org/x/sys/windows"
)

// isNetErrno 判定 Windows WSA 错误码
func isNetErrno(errno syscall.Errno) bool {
	switch uintptr(errno) {
	case uintptr(windows.WSAECONNRESET), // 连接被对端重置
		uintptr(windows.WSAECONNABORTED), // 连接被本地中止
		uintptr(windows.WSAENETRESET),    // 网络重置
		uintptr(windows.WSAETIMEDOUT),    // 连接超时
		uintptr(windows.WSAECONNREFUSED): // 连接被拒绝
		return true
	}
	return false
}

// syscallErrno 把内核错误码转换为 error
func syscallErrno(v int) error {
	if v == 0 {
		return nil
	}
	return syscall.Errno(v)
}
