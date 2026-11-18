//go:build solaris || illumos || aix

package vconn

import (
	"errors"
	"io"
	"net"
	"syscall"
)

// probeConn 在不消耗任何字节的前提下探测连接状态：
// 用 recv(MSG_PEEK) 观察内核接收队列——有数据即存活，返回 0 即对端 FIN。
// 说明：Solaris/AIX 无 POLLRDHUP，只能"缓冲区排空后"才能识别 FIN。
func probeConn(c net.Conn) (probeResult, error) {
	sc, ok := syscallConnOf(c)
	if !ok {
		return probeUnsupported, nil
	}
	raw, err := sc.SyscallConn()
	if err != nil {
		return probeUnsupported, nil
	}

	var (
		n    int
		serr error
	)
	if cerr := raw.Control(func(fd uintptr) {
		var b [1]byte
		n, _, serr = syscall.Recvfrom(int(fd), b[:], syscall.MSG_PEEK)
	}); cerr != nil {
		return probeUnsupported, nil
	}

	switch {
	case serr == nil && n == 0:
		return probeClosed, io.EOF
	case serr == nil:
		return probeReadable, nil
	case errors.Is(serr, syscall.EAGAIN), errors.Is(serr, syscall.EWOULDBLOCK), errors.Is(serr, syscall.EINTR):
		return probeIdle, nil
	case errors.Is(serr, syscall.ENOTSOCK),
		errors.Is(serr, syscall.EOPNOTSUPP),
		errors.Is(serr, syscall.EINVAL):
		// 这不是 socket（或该 fd 不支持 MSG_PEEK）：无法探测 ≠ 已断开。
		return probeUnsupported, nil
	default:
		return probeClosed, serr
	}
}
