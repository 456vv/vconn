// Package vconn provides a virtual net.Conn wrapper with background read
// capability for early close detection.
package vconn

import (
	"errors"
	"io"
	"net"
	"os"
	"syscall"
)

// ErrRawConnAlreadyUsed 表示原始连接已被提取，当前包装连接失效
var ErrRawConnAlreadyUsed = errors.New("vconn: the original connection has already been extracted and cannot be used")

// isCommonNetError 精准识别由于远程断开或系统网络中断引起的非正常关闭错误
// 该函数是线程安全的，可以并发调用
func isCommonNetError(err error) bool {
	if err == nil {
		return false
	}

	// 1. 匹配标准 Go 网络及文件结束符
	if errors.Is(err, io.EOF) ||
		errors.Is(err, net.ErrClosed) ||
		errors.Is(err, io.ErrUnexpectedEOF) {
		return true
	}

	// 2. 使用 errors.As 链式解包 net.OpError
	var opErr *net.OpError
	if errors.As(err, &opErr) {
		// 读写超时属于正常控制流，不判定为连接非正常关闭
		if opErr.Timeout() {
			return false
		}
		// 递归检查内层错误
		return isCommonNetError(opErr.Err)
	}

	// 3. 使用 errors.As 链式解包 os.SyscallError
	var sysErr *os.SyscallError
	if errors.As(err, &sysErr) {
		return isCommonNetError(sysErr.Err)
	}

	// 4. 精准判定系统调用级别的网络断开错误
	var errno syscall.Errno
	if errors.As(err, &errno) {
		// Unix/Linux 常见网络错误码
		switch errno {
		case syscall.ECONNRESET, // 连接被对端重置
			syscall.ECONNABORTED, // 连接被本地中止
			syscall.EPIPE,        // 管道破裂（对端已关闭）
			syscall.ETIMEDOUT,    // 连接超时
			syscall.EINVAL,       // 无效参数（可能是已关闭的 socket）
			syscall.ENETRESET:    // 网络重置
			return true
		}

		// 跨平台处理 Windows WSA 错误码
		// 使用数字常量避免非 Windows 平台编译错误
		val := uintptr(errno)
		const (
			WSAECONNRESET   uintptr = 10054 // 连接被对端重置
			WSAECONNABORTED uintptr = 10053 // 连接被本地中止
			WSAENETRESET    uintptr = 10052 // 网络重置
			WSAETIMEDOUT    uintptr = 10060 // 连接超时
			WSAECONNREFUSED uintptr = 10061 // 连接被拒绝
		)
		switch val {
		case WSAECONNRESET, WSAECONNABORTED, WSAENETRESET, WSAETIMEDOUT, WSAECONNREFUSED:
			return true
		}
	}

	return false
}
