//go:build windows

package vconn

import (
	"io"
	"net"
	"syscall"
	"unsafe"

	"golang.org/x/sys/windows"
)

// Windows winsock 常量。
//
// 这些值直接按 Winsock 文档写死，而不是从 Go 的归一化 errno 反推：
// SO_ERROR / FIONREAD / poll 事件位是套接字选项和事件掩码，
// 与 syscall.ECONNRESET 那套 APPLICATION_ERROR 编码不是同一个数域。
// 写错一个位，探测会恒定空闲或把健康连接报成挂断。
const (
	winSOError  = 0x1007     // SO_ERROR
	winFIONREAD = 0x4004667F // 查询接收队列可读字节数（不消费、不阻塞）

	winPOLLRDNORM = 0x0100 // 有数据/EOF 可读
	winPOLLRDBAND = 0x0200
	winPOLLERR    = 0x0001
	winPOLLHUP    = 0x0002
	winPOLLNVAL   = 0x0004
)

var (
	modWS2_32   = windows.NewLazySystemDLL("ws2_32.dll")
	procWSAPoll = modWS2_32.NewProc("WSAPoll")
)

// wsapollFd 对应 struct pollfd
type wsapollFd struct {
	fd      windows.Handle
	events  int16
	revents int16
}

// pollReadable 用 WSAPoll(timeout=0) 做非阻塞可读性检查。
// 返回 readable（有数据/EOF 可读）、fatal（POLLERR/POLLHUP）、invalid（POLLNVAL）。
func pollReadable(h windows.Handle) (readable, fatal, invalid bool, err error) {
	// LazyProc.Call 在 DLL/符号加载失败时会 panic；先 Find 并返回错误，
	// 交给 probeConn 归类为 probeUnsupported。
	if err := procWSAPoll.Find(); err != nil {
		return false, false, false, err
	}
	pfd := wsapollFd{fd: h, events: winPOLLRDNORM}
	r, _, e := procWSAPoll.Call(uintptr(unsafe.Pointer(&pfd)), 1, 0)
	if int32(r) == -1 {
		return false, false, false, e
	}
	if pfd.revents&winPOLLNVAL != 0 {
		return false, false, true, nil
	}
	if pfd.revents&(winPOLLERR|winPOLLHUP) != 0 {
		return true, true, false, nil
	}
	return pfd.revents&(winPOLLRDNORM|winPOLLRDBAND) != 0, false, false, nil
}

// probeConn 在不消耗任何字节的前提下探测连接状态。
//
// Windows 上不能直接对 Go 管理的 socket 做同步 WSARecv(MSG_PEEK)：
// 该 socket 在 Win32 层是阻塞模式，即使 WSAPoll 刚返回可读，只要数据在
// poll 与 recv 之间被前台 Read 消费，WSARecv 就会永久阻塞。
//
// 因此这里改用 WSAPoll + FIONREAD（WSAIoctl）：
//   - FIONREAD 只查询接收队列字节数，不消费、不阻塞；
//   - 「可读但 0 字节」可能是 FIN，也可能是并发读把数据抢走；
//     再 poll 一次：不再可读 → 数据被抢走（连接存活）；仍可读 → 按 FIN 处理。
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
		res  = probeUnsupported
		rerr error
	)
	cerr := raw.Control(func(fd uintptr) {
		res, rerr = probeFDWin(windows.Handle(fd))
	})
	if cerr != nil {
		return probeUnsupported, nil
	}
	return res, rerr
}

// winInvalidResult 生成 Windows 的「fd 已失效（POLLNVAL）」探测结果。
//
// 注意：直接返回 (probeClosed, io.EOF) 会把内核里待处理的真实错误
// （如 WSAECONNRESET）抹成"干净的 FIN"，上层无法区分对端正常关闭与连接被重置。
// POLLNVAL 通常意味着本地已关闭，但仍应先探一次 SO_ERROR：
//   - 拿到非 0 错误 → 如实上报；
//   - 读不到 SO_ERROR（fd 已回收）→ 退化为 io.EOF。
//
// 与 winClosedResult 的差别：这里**不**返回 probeUnsupported。
// fd 已失效是确定的"不可用"，降级为 probeUnsupported 会让监控继续走预读路径、
// 白白多跑一轮。
func winInvalidResult(h windows.Handle) (probeResult, error) {
	if serr, gerr := sockErrorOf(h); gerr == nil && serr != nil {
		return probeClosed, serr
	}
	return probeClosed, io.EOF
}

// probeFDWin 对单个 socket 做一次零消耗探测（调用方需保证 fd 有效）
func probeFDWin(h windows.Handle) (probeResult, error) {
	readable, fatal, invalid, perr := pollReadable(h)
	if perr != nil {
		return probeUnsupported, nil
	}
	if invalid {
		// fd 已失效（通常是本地已关闭）：优先保住 SO_ERROR，不要抹成干净 EOF。
		return winInvalidResult(h)
	}
	if !readable && !fatal {
		return probeIdle, nil
	}

	// 最多两轮：第一轮用于区分「FIN」与「数据被并发读消费」。
	for range 2 {
		n, aerr := availableBytesWin(h)
		if aerr != nil {
			if fatal {
				return winClosedResult(h)
			}
			return probeUnsupported, nil
		}
		if n > 0 {
			if fatal {
				return winReadableHangup(h)
			}
			return probeReadable, nil
		}
		if fatal {
			return winClosedResult(h)
		}

		// 可读但 0 字节：再 poll 一次。
		readable, fatal, invalid, perr = pollReadable(h)
		if perr != nil {
			return probeUnsupported, nil
		}
		if invalid {
			return winInvalidResult(h)
		}
		if !readable {
			// 数据在 poll 与 FIONREAD 之间被前台 Read 消费，连接仍然存活。
			return probeIdle, nil
		}
	}
	// 连续两轮「可读 + 0 字节」：按对端 FIN 处理。
	return winClosedResult(h)
}

// winReadableHangup 生成 Windows 的「有数据 + 挂断」探测结果。
//
// 尽量保留 SO_ERROR 作为 cause（RST 等真实原因）：直接给零值哨兵会把
// 异常中断抹成「干净的 FIN」，上层就无法区分「对端正常收尾」与「连接被重置」。
// 拿不到 SO_ERROR 时按干净 FIN 处理（数据确实还在队列里，可以读）。
func winReadableHangup(h windows.Handle) (probeResult, error) {
	if serr, gerr := sockErrorOf(h); gerr == nil && serr != nil {
		return probeReadableHangup, peerClosedWithUnreadError{cause: serr}
	}
	return probeReadableHangup, peerClosedWithUnreadError{}
}

// availableBytesWin 查询接收队列中可读字节数（不消费、不阻塞）
func availableBytesWin(h windows.Handle) (uint32, error) {
	var (
		n   uint32
		ret uint32
	)
	err := windows.WSAIoctl(
		h,
		winFIONREAD,
		nil, 0,
		(*byte)(unsafe.Pointer(&n)), uint32(unsafe.Sizeof(n)),
		&ret,
		nil, 0,
	)
	if err != nil {
		return 0, err
	}
	return n, nil
}

// sockErrorOf 读取 SO_ERROR。
// 返回 (socketError, getsockoptError)：
//   - getsockopt 失败表示无法探测（probeUnsupported），不能当成“已断开”；
//   - socketError 非 nil 才是内核报告的真实连接错误。
func sockErrorOf(h windows.Handle) (error, error) {
	v, err := windows.GetsockoptInt(h, windows.SOL_SOCKET, winSOError)
	if err != nil {
		return nil, err
	}
	if v == 0 {
		return nil, nil
	}
	return syscall.Errno(v), nil
}

// winClosedResult 生成 Windows 的「已关闭」探测结果：
// 优先返回 SO_ERROR；拿不到 SO_ERROR 时按正常 FIN（io.EOF）处理。
//
// 已知局限：Winsock 只在 recv 时报告 TCP RST，SO_ERROR 通常为 0，
// 因此「可读 + 0 字节」有可能把 RST 报成干净的 FIN。真实原因不会丢——
// 前台 Read 仍会拿到 wsarecv 的 RST 错误，且 terminalReadErr 不会把它降级
// （不变量第 5 条）。想在这里补一次 recv(MSG_PEEK) 取原因是不安全的：
// 该 socket 在 Win32 层是阻塞模式，无数据时会永久阻塞监控协程。
func winClosedResult(h windows.Handle) (probeResult, error) {
	serr, gerr := sockErrorOf(h)
	if gerr != nil {
		return probeUnsupported, nil
	}
	if serr != nil {
		return probeClosed, serr
	}
	return probeClosed, io.EOF
}
