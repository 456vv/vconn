//go:build linux || darwin || freebsd || netbsd || openbsd || dragonfly || solaris || illumos || aix || ios || android

package vconn

import (
	"net"
	"syscall"
)

// socketError 返回内核 socket 上待处理的错误（SO_ERROR），无错误时返回 nil
//
// 语义约定：
//   - 拿到非 0 的 SO_ERROR → 返回该错误（连接已异常）；
//   - 无待处理错误 → nil（连接正常）；
//   - 拿不到 fd / 读不到该选项（非 socket、fd 已失效等）→ ErrUnsupported，
//     由调用方回退到 probeConn。**不能**把「探测失败」当成「连接已断开」，
//     否则 CheckAlive 会对健康连接误报（旧版就踩过这个坑）。
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
		v, gerr := syscall.GetsockoptInt(int(fd), syscall.SOL_SOCKET, syscall.SO_ERROR)
		switch {
		case gerr != nil:
			serr = ErrUnsupported
		case v != 0:
			serr = syscall.Errno(v)
		}
	}); cerr != nil {
		return ErrUnsupported
	}
	return serr
}
