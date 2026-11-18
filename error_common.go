package vconn

import (
	"errors"
	"io"
	"net"
	"os"
	"syscall"
)

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

	// 4. 按平台判定系统调用级别的网络断开错误码
	var errno syscall.Errno
	if errors.As(err, &errno) {
		return isNetErrno(errno)
	}

	return false
}
