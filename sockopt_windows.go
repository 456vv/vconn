//go:build windows

package vconn

import (
	"net"

	"golang.org/x/sys/windows"
)

// socketError 返回内核 socket 上待处理的错误（SO_ERROR），无错误时返回 nil
//
// 注意：旧版本误用 SO_KEEPALIVE 判断连接状态，导致该检查恒为 nil。
func socketError(c net.Conn) error {
	sc, ok := syscallConnOf(c)
	if !ok {
		return ErrUnsupported
	}
	raw, err := sc.SyscallConn()
	if err != nil {
		return ErrUnsupported
	}

	var serr error
	if cerr := raw.Control(func(fd uintptr) {
		s, gerr := sockErrorOf(windows.Handle(fd))
		if gerr != nil {
			serr = ErrUnsupported
			return
		}
		serr = s
	}); cerr != nil {
		// 拿不到 fd：交给 probeConn 判断，不能当成「已断开」
		return ErrUnsupported
	}
	return serr
}
