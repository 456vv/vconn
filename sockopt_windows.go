//go:build windows

package vconn

import (
	"net"

	"golang.org/x/sys/windows"
)

// socketError 返回内核 socket 上待处理的错误（SO_ERROR），无错误时返回 nil
//
// 注意：SO_ERROR 才是「内核待处理的连接错误」的正确选项。
// 不能用 SO_KEEPALIVE 之类的其它选项代替——那会得到一个恒定的无关值，
// 导致本检查永远返回 nil（连接已断开却探测不出来）。
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
