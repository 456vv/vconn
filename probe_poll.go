//go:build linux || android || darwin || ios || freebsd || netbsd || openbsd || dragonfly

package vconn

import (
	"errors"
	"io"
	"net"

	"golang.org/x/sys/unix"
)

// probeConn 在不消耗任何字节的前提下探测连接状态。
//
// 两步：
//  1. poll(0) 观察就绪事件（Linux 有 POLLRDHUP；BSD/macOS 没有）；
//  2. 就绪时用 recv(MSG_PEEK) 区分「真有数据」与「对端 FIN」。
//
// 第 2 步是必须的：
//   - BSD/macOS 上「收到 FIN 且接收队列为空」只置 POLLIN（不置 POLLHUP），
//     仅看事件位会把对端关闭误判为「有数据可读」，导致关闭通知永远不触发；
//   - Linux 上 POLLRDHUP 会与 POLLIN 同时置位（对端"发完数据再 FIN"），
//     仅看事件位会把「还有未读数据」误判为「已断开」，上层会因此提前收尾并丢数据。
//
// 两种平台因此统一按 recv(MSG_PEEK) 的结果判定：0 字节 = FIN，>0 = 有数据。
func probeConn(c net.Conn) (probeResult, error) {
	sc, ok := syscallConnOf(c)
	if !ok {
		return probeUnsupported, nil
	}
	raw, err := sc.SyscallConn()
	if err != nil {
		return probeUnsupported, nil
	}

	res := probeUnsupported
	var rerr error

	if cerr := raw.Control(func(fd uintptr) {
		res, rerr = probeFDPoll(int(fd))
	}); cerr != nil {
		return probeUnsupported, nil
	}

	return res, rerr
}

// probeFDPoll 对单个 fd 做一次零消耗探测（调用方需保证 fd 在本次 Control 回调内有效）。
//
// 事件位只负责回答「要不要再看一眼」，最终结论以 MSG_PEEK 为准：
//   - POLLIN 在 BSD 上既可能是数据，也可能是队列已空的 FIN；
//   - POLLRDHUP/POLLHUP 在 Linux 上可能和未读数据同时出现；
//   - POLLNVAL 表示 fd 已经失效，按断开上报，让调用方停止继续探测这个 fd。
//
// EINTR 不当成断开：这次探测被信号打断，连接状态并没有变化，下一轮再看。
func probeFDPoll(fd int) (probeResult, error) {
	pfd := []unix.PollFd{{
		Fd:     int32(fd),
		Events: unix.POLLIN | unix.POLLERR | unix.POLLHUP | pollRDHUP,
	}}
	n, perr := unix.Poll(pfd, 0)
	if perr != nil {
		if errors.Is(perr, unix.EINTR) {
			return probeIdle, nil
		}
		return probeUnsupported, nil
	}
	if n == 0 {
		return probeIdle, nil
	}

	rev := pfd[0].Revents
	if rev&unix.POLLNVAL != 0 {
		// fd 已失效（通常是本地已关闭）：按断开上报，让调用方尽快收尾
		return probeClosed, io.EOF
	}

	hangup := rev&(unix.POLLHUP|unix.POLLERR|pollRDHUP) != 0
	readable := rev&unix.POLLIN != 0
	if !hangup && !readable {
		return probeIdle, nil
	}

	hasData, serr := peekHasData(fd)
	switch {
	case serr != nil:
		if isPeekUnsupportedErr(serr) {
			return probeUnsupported, nil
		}
		return probeClosed, serr

	case hasData == peekWouldBlock:
		// poll 与 MSG_PEEK 之间数据可能被前台 Read 消费。
		// 这表示连接仍然存活，绝不能当成 recv==0（FIN）。
		return probeIdle, nil

	case hasData == peekData:
		if hangup {
			// 有数据 + 挂断：既可能是「发完数据再 FIN」，也可能是 RST。
			// 取一次 SO_ERROR 保留真实原因，否则上层会把 RST 当成优雅关闭，
			// 对非幂等请求做出「请求已执行完」的错误推断。
			if e := sockErrorFD(fd); e != nil {
				return probeReadableHangup, peerClosedWithUnreadError{cause: e}
			}
		}
		return classifyPeek(hasData, hangup)

	default: // peekEOF
		// 可读事件成立却读不到字节 → recv 返回 0 → 对端 FIN
		if e := sockErrorFD(fd); e != nil {
			return probeClosed, e
		}
		return classifyPeek(hasData, hangup)
	}
}

// peekHasData 用 recv(MSG_PEEK) 判断接收队列里是否有数据（不消耗任何字节）
func peekHasData(fd int) (peekOutcome, error) {
	var b [1]byte
	n, _, err := unix.Recvfrom(fd, b[:], unix.MSG_PEEK)
	if err != nil {
		if errors.Is(err, unix.EAGAIN) || errors.Is(err, unix.EWOULDBLOCK) || errors.Is(err, unix.EINTR) {
			return peekWouldBlock, nil // 暂无数据/被并发读消费，连接仍然存活
		}
		return peekEOF, err
	}
	if n == 0 {
		return peekEOF, nil
	}
	return peekData, nil
}

// isPeekUnsupportedErr 判断 MSG_PEEK 失败是否属于「这个 fd 不是 socket，无法探测」
func isPeekUnsupportedErr(err error) bool {
	return errors.Is(err, unix.ENOTSOCK) ||
		errors.Is(err, unix.EOPNOTSUPP) ||
		errors.Is(err, unix.EINVAL)
}

// sockErrorFD 读取指定 fd 的 SO_ERROR
func sockErrorFD(fd int) error {
	v, err := unix.GetsockoptInt(fd, unix.SOL_SOCKET, unix.SO_ERROR)
	if err != nil || v == 0 {
		return nil
	}
	return syscallErrno(v)
}
