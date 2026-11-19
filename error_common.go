package vconn

import (
	"errors"
	"io"
	"net"
	"os"
	"syscall"
)

// isCommonNetError 判断错误是不是「连接已经不可用」而不是普通控制流。
//
// 命中范围：io.EOF、net.ErrClosed、io.ErrUnexpectedEOF，以及各平台白名单里的
// 断开 errno（ECONNRESET、EPIPE、网络不可达等）。读写超时一律不算：
// 超时是调用方自己的 deadline，连接可能仍然健康。
//
// Read/Write 用它决定要不要分发远端关闭通知；StatusOf 用同一份白名单把
// 这些 errno 归到 StatusPeerReset。两处必须共用，否则「通知说断了」和
// 「分类说未知」会漂移。本函数不分配、不修改错误，可并发调用。
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
