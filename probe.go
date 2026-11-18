package vconn

import (
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

// peerClosedWithUnreadError 同时匹配 io.EOF 与 ErrPeerClosedWithUnread。
//
// 用自定义类型而非 fmt.Errorf("%w: %w", ...) 的原因：
//   - 错误串稳定（便于日志检索与告警规则匹配），不暴露 io.EOF 的原文；
//   - 值类型、零分配；
//   - 可精确控制两个 Is 目标，不引入多余的中间节点。
type peerClosedWithUnreadError struct{}

func (peerClosedWithUnreadError) Error() string {
	return "vconn: peer closed with unread data (EOF)"
}

// Unwrap 让 errors.Is(err, io.EOF) 成立。
//
// 这一点对 terminalReadErr 至关重要：其首个分支是
// `if cerr != nil && !errors.Is(cerr, io.EOF) { return cerr }`，
// Unwrap 到 io.EOF 保证该分支不命中，Read 的返回语义保持不变。
func (peerClosedWithUnreadError) Unwrap() error { return io.EOF }

// Is 补充第二个判定目标（Unwrap 只能提供一个）。
func (peerClosedWithUnreadError) Is(target error) bool {
	return target == ErrPeerClosedWithUnread
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
