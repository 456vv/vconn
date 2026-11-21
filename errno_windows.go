//go:build windows

package vconn

import (
	"syscall"

	"golang.org/x/sys/windows"
)

// isNetErrno 判定 Windows 网络断开错误码。
//
// Windows 上同一个错误存在两套值，必须两套都认：
//
//   - 运行时错误：Winsock 原始码（如 WSAECONNRESET=10054），直接来自 WSARecv 等调用；
//   - 命名常量：Go 归一化后的 syscall.ECONNRESET = APPLICATION_ERROR|ECONNRESET，
//     与 Unix 的同名常量语义一致，net 包与用户代码都可能直接抛出它
//     （errors.Is(err, syscall.ECONNREFUSED) 在 Windows 上也是靠这套值成立的）。
//
// 只认一套会造成分类漂移：同一个 RST，从 Read 拿到时被归为 StatusPeerReset，
// 由别处构造的 syscall.ECONNRESET 却归为 StatusUnknown。两套都认，
// 才能保证 StatusOf 对「同一个原因的不同表示」给出同一个答案。
func isNetErrno(errno syscall.Errno) bool {
	switch uintptr(errno) {
	case uintptr(windows.WSAECONNRESET), // 连接被对端重置
		uintptr(windows.WSAECONNABORTED), // 连接被本地中止
		uintptr(windows.WSAENETRESET),    // 网络重置
		uintptr(windows.WSAETIMEDOUT),    // 连接超时
		uintptr(windows.WSAECONNREFUSED): // 连接被拒绝
		return true
	}

	// 归一化常量（APPLICATION_ERROR|unix errno）
	switch errno {
	case syscall.ECONNRESET,
		syscall.ECONNABORTED,
		syscall.EPIPE,
		syscall.ETIMEDOUT,
		syscall.ECONNREFUSED:
		return true
	}
	return false
}
