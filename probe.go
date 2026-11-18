package vconn

import (
	"errors"
	"io"
	"net"
)

// probeResult 内核级探测结果
type probeResult int

const (
	// probeUnsupported 底层连接不支持零消耗探测（net.Pipe、自定义 Conn 等）
	probeUnsupported probeResult = iota
	// probeIdle 连接存活且当前无数据可读
	probeIdle
	// probeReadable 连接存活且已有数据可读
	probeReadable
	// probeClosed 对端已关闭或连接异常
	probeClosed
	// probeReadableHangup 有数据可读，且对端已关闭（接收队列里还有未读数据）。
	// 语义是「先读完数据，再按断开处理」：数据没读空之前不能关闭底层，否则会丢数据。
	probeReadableHangup
)

// peekOutcome 是 recv/WSARecv(MSG_PEEK) 的三态结果。
//
// 必须区分 peekEOF 与 peekWouldBlock：poll 返回可读后，数据可能在
// poll 与 MSG_PEEK 之间被前台 Read 消费，此时 MSG_PEEK 得到 EAGAIN，
// 绝不能把它当成 EOF（否则会把健康连接误判为对端关闭）。
type peekOutcome uint8

const (
	peekEOF peekOutcome = iota
	peekData
	peekWouldBlock
)

// ==================== 挂断语义 ====================

// peerClosedWithUnreadError 表示「对端已结束，但还有未读数据」。
//
// 它同时匹配 io.EOF 与 ErrPeerClosedWithUnread：
//   - errors.Is(err, io.EOF) 成立 → 只判 EOF 的老代码行为完全不变；
//   - errors.Is(err, ErrPeerClosedWithUnread) 成立 → 新代码可以区分
//     「队列已读空的干净结束」与「还有数据没读完」。
//
// cause 保留真实终止原因：对端可能在发完一批数据后 RST（而不是优雅 FIN），
// 此时若把原因抹成干净的 io.EOF，上层会把「异常中断」当成「正常收尾」，
// 进而对非幂等请求做出错误的「已执行完」推断。
type peerClosedWithUnreadError struct{ cause error }

func (e peerClosedWithUnreadError) Error() string {
	if e.cause == nil {
		return "vconn: peer closed with unread data (EOF)"
	}
	return "vconn: peer closed with unread data: " + e.cause.Error()
}

// Unwrap 同时暴露 io.EOF 与真实原因（多错误解包，Go 1.20+）。
//
// 保留 io.EOF 对 terminalReadErr 至关重要：它依赖「哨兵满足 io.EOF」
// 来避免把哨兵本身从 Read 返回；真实原因则由 Cause() 单独取用。
func (e peerClosedWithUnreadError) Unwrap() []error {
	if e.cause == nil {
		return []error{io.EOF}
	}
	return []error{io.EOF, e.cause}
}

// Is 补充第二个判定目标（Unwrap 只能提供错误链，无法表达非错误的哨兵）。
func (e peerClosedWithUnreadError) Is(target error) bool {
	return target == ErrPeerClosedWithUnread
}

// Cause 返回真实终止原因（干净 FIN 时为 nil）。
func (e peerClosedWithUnreadError) Cause() error { return e.cause }

// Status 实现 Statuser：干净 FIN 归为 StatusPeerClosed，带真实原因时交给 StatusOf 判定
// （通常落到 StatusPeerReset —— 异常中断，非幂等请求不可重试）。
func (e peerClosedWithUnreadError) Status() Status {
	if e.cause == nil {
		return StatusPeerClosed
	}
	return StatusOf(e.cause)
}

// nonEOF 过滤掉 io.EOF 类的「非错误原因」，只保留真实错误。
// 用于给哨兵挂 cause 时避免出现 Unwrap 到两次 io.EOF。
func nonEOF(err error) error {
	if err == nil || errors.Is(err, io.EOF) {
		return nil
	}
	return err
}

// classifyPeek 把 MSG_PEEK 结果映射为探测结果。
//
// peekData + hangup 会带上 ErrPeerClosedWithUnread：在通知义务上与 io.EOF 等价，
// 但订阅者可以据此区分「队列已读空的干净结束」与「还有数据没读完」。
// peekEOF（recv 返回 0 字节）仍然是队列已空的确定证据，继续用 io.EOF。
func classifyPeek(outcome peekOutcome, hangup bool) (probeResult, error) {
	switch outcome {
	case peekData:
		if hangup {
			return probeReadableHangup, peerClosedWithUnreadError{}
		}
		return probeReadable, nil
	case peekWouldBlock:
		// 数据被并发的前台 Read 抢先消费；连接仍然存活，下一轮再探测。
		return probeIdle, nil
	default:
		return probeClosed, io.EOF
	}
}

// SafeBlockProber 可选接口：非 syscall.Conn 的连接可实现此接口，
// 显式声明「可以安全地对其发起带超时的阻塞 Read 探测，不会破坏内部状态」。
//
// 默认规则（未实现本接口时）：
//   - 叶子连接（net.Pipe、内存管道、不隐藏内层连接的底层实现）→ 视为可安全阻塞读；
//   - 包装型连接（实现了 NetConn() net.Conn，如 *tls.Conn 及其 wrapper）→ 视为不可读，
//     避免提前 Read 触发握手/破坏记录边界或吞掉多路复用的帧头。
//
// 典型实现者：多路复用/状态机连接在确认安全时显式 opt-in。
// 反例（务必不要实现）：*tls.Conn 及其 wrapper（Read 会触发握手/剥离记录）、
// 多路复用协议（HTTP/2、yamux 等，提前读会吞掉帧头）。
type SafeBlockProber interface {
	SafeToBlockProbe() bool
}

// safeBlockConn 包装一个可安全阻塞探测的连接，用于满足 SafeBlockProber。
type safeBlockConn struct {
	net.Conn
}

func (safeBlockConn) SafeToBlockProbe() bool { return true }

// WrapSafeBlockProbe 把连接标记为「可安全阻塞探测」。
//
// 仅对确定不会因提前读取而破坏内部状态的连接使用（net.Pipe、内存管道等）。
// 对 *tls.Conn 或其包装使用会造成 TLS 握手被打断/记录被剥离，切勿使用。
//
// 对已经是 syscall.Conn 的连接（*net.TCPConn 等）无需包装：它们走内核级探测，
// 根本不会进入阻塞预读分支。
func WrapSafeBlockProbe(c net.Conn) net.Conn {
	if c == nil {
		return nil
	}
	return safeBlockConn{Conn: c}
}

// isWrapperConn 是否为「隐藏了内层连接」的包装（声明了 NetConn()，如 *tls.Conn）。
// 这类连接的 Read 可能带协议副作用（握手、解记录、帧解析），不能盲目阻塞读。
//
// 放在本文件（无构建标签）以便所有平台共用同一份判定逻辑。
func isWrapperConn(c net.Conn) bool {
	_, ok := c.(interface{ NetConn() net.Conn })
	return ok
}
