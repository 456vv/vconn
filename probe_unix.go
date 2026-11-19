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
//
// Solaris/illumos/AIX 没有 POLLRDHUP，也没有本包采用的 poll+FIONREAD 组合，
// 所以这里只能看到「接收队列当前是否为空」。对端已经 FIN、但队列里还有字节时，
// MSG_PEEK 会返回那 1 字节并报告 probeReadable，关闭通知要等队列被读空后才能发出。
// 这是平台能力限制，不是把「有数据的挂断」误判成健康：字节仍在内核里，前台 Read 能取走。
//
// ENOTSOCK / EOPNOTSUPP / EINVAL 表示这个 fd 不能做 MSG_PEEK（例如不是 socket）。
// 这种情况返回 probeUnsupported：无法探测不等于已经断开，交给上层决定要不要阻塞预读。
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
