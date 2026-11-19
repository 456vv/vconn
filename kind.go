package vconn

import (
	"context"
	"errors"
	"io"
	"net"
	"os"
)

// 本文件是 Status 的「处置轴」与「错误 → Status」的唯一映射。
//
// 类型定义、常量与观测轴（Class / Unread / Readable / Reusable / Ended / String）
// 在 state.go；两个轴共用同一个类型，这里只回答「我该怎么做」。

// Retryable 报告该结论是否意味着「换一条连接 / 稍后再试」有意义。
//
// 与 Terminal 的区别：Retryable 讲「要不要再试」，Terminal 讲「这条连接还能不能用」。
// 二者并不互补，例如 StatusBusy 既不终结也不该丢弃，但重试立刻能成功。
//
// StatusPeerClosed 与 StatusPeerReset 的可重试性还取决于请求是否幂等：
// 对端已 FIN 时请求可能已执行完，RST/截断时则可能完全没执行。
func (s Status) Retryable() bool {
	switch s.Class() {
	case StatusPeerClosed, StatusPeerReset, StatusBusy, StatusUnverified, StatusTimeout:
		return true
	}
	return false
}

// Terminal 报告该结论是否意味着「这条连接不能再用了」。
//
// 关系（测试会盯着）：Ended() ⊂ Terminal()。
// 终结但未结束的两个类别是 StatusDataPending（协议失步）与 StatusQuota（配额用尽）
// ——连接本身可能还活着，但对下一个人已经没有用了。
//
// 用 Class() 判：带 unread 修饰的变体（StatusPeerClosedWithUnread 等）
// 与不带修饰的类别是同一个处置结论。
func (s Status) Terminal() bool {
	switch s.Class() {
	case StatusPeerClosed, StatusPeerReset, StatusLocalClosed, StatusDataPending, StatusQuota:
		return true
	}
	return false
}

// Statuser 可选接口：错误可以声明自己的结论（分类）。
//
// 本包内部的错误类型都实现了它；用户通过 CancelNotify(err) 传入自定义错误时，
// 也可以实现本接口让 StatusOf 给出准确分类，否则会被归为 StatusUnknown。
//
// 注意：内部类型必须实现它——这是 StatusOf 无需猜测错误语义的前提。
// 新加的错误类型如果没有实现 Statuser，只能在 StatusOf 的 switch 里登记，
// 否则分类会静默落到 StatusUnknown（status_test.go 的黄金表会盯着）。
//
// 实现时只回答「这个错误属于哪一类」，不要在 Status() 里做 I/O、探测或修改连接。
// StatusOf 可能在日志、指标和每次 Read 错误路径上被调用，必须是纯函数。
type Statuser interface {
	Status() Status
}

// StatusOf 返回错误的处置结论，是本包唯一的错误分类入口。
//
// 只有两份映射，顺序不可颠倒：
//  1. 先问错误自己（内部类型都实现 Statuser：零猜测、零字符串匹配、可被用户扩展）；
//  2. 再对标准库/系统错误做集中映射（全包只有这一处）。
//
// 为什么不用字符串匹配：错误串是给日志看的，一旦有人改了文案，
// 分类就会静默失效；errors.Is/As + Status 是结构化契约。
//
// # 映射表（权威版本是 status_test.go 里的黄金表）
//
//	StatusAlive        nil
//	StatusPeerClosed   io.EOF / ErrPeerClosedWithUnread / peerClosedWithUnreadError{} /
//	                   ErrResetPeerDown（已断但要先 drain 的那类）
//	StatusPeerReset    io.ErrUnexpectedEOF / ECONNRESET / EPIPE / ENETUNREACH / 超时类断链
//	StatusLocalClosed  net.ErrClosed / io.ErrClosedPipe / ErrResetLocallyClosed
//	StatusDataPending  ErrConnDirty
//	StatusBusy         ErrBusy / ErrResetTimeout
//	StatusLatched      ErrNotificationLatched / ErrNotifyCanceled / context.Canceled
//	StatusUnverified   ErrUnverified
//	StatusQuota        ErrQuotaExhausted
//	StatusTimeout      os.ErrDeadlineExceeded / context.DeadlineExceeded / net.Error.Timeout()
//	StatusMisuse       ErrRawConnAlreadyUsed
//	StatusUnsupported  ErrUnsupported
//	StatusUnknown      ErrNotReusable 裸用 / 自定义错误
//
// 与 Check* 的关系（status_test.go 里的不变量测试）：
// err 非 nil 时 StatusOf(err).Class() 应与 Check* 返回的类别相同；
// 唯一例外是「对端确实结束了，但原因无法归类」——那时 Check* 保守报
// StatusPeerReset，而 StatusOf 如实报 StatusUnknown。
func StatusOf(err error) Status {
	if err == nil {
		return StatusAlive
	}

	var s Statuser
	if errors.As(err, &s) {
		return s.Status()
	}

	switch {
	// 配额耗尽要先于 io.EOF 判定：它是「调用方自己设的上限」，不是对端关闭。
	case errors.Is(err, ErrQuotaExhausted):
		return StatusQuota

	// 流在中途被截断：对端没有正常收尾，不能算 StatusPeerClosed。
	case errors.Is(err, io.ErrUnexpectedEOF):
		return StatusPeerReset

	// 对端正常结束（含 unread 变体）。
	case errors.Is(err, ErrPeerClosedWithUnread), errors.Is(err, io.EOF):
		return StatusPeerClosed

	// 本地已关闭（含管道：net.Pipe 本地端关闭时 Read/SetReadDeadline 返回 io.ErrClosedPipe）。
	case errors.Is(err, net.ErrClosed), errors.Is(err, io.ErrClosedPipe), errors.Is(err, ErrResetLocallyClosed):
		return StatusLocalClosed

	case errors.Is(err, ErrConnDirty):
		return StatusDataPending

	case errors.Is(err, ErrBusy), errors.Is(err, ErrResetTimeout):
		return StatusBusy

	case errors.Is(err, ErrNotificationLatched), errors.Is(err, ErrNotifyCanceled), errors.Is(err, context.Canceled):
		// 上下文取消走的是"手动通知"路径：连接本身未必坏，
		// 只是这一轮等待被撤销了（需要 ResetNotify 才能重新武装）。
		return StatusLatched

	case errors.Is(err, ErrUnverified):
		return StatusUnverified

	case errors.Is(err, ErrRawConnAlreadyUsed):
		return StatusMisuse

	case errors.Is(err, ErrUnsupported):
		return StatusUnsupported

	case errors.Is(err, ErrResetPeerDown):
		// 重置被拒是因为「已经探测到对端下线」：真实状态是对端结束，
		// 只是这一轮通知不能重新武装。
		return StatusPeerClosed

	case errors.Is(err, os.ErrDeadlineExceeded), errors.Is(err, context.DeadlineExceeded):
		return StatusTimeout
	}

	// 底层超时（含自定义 net.Conn 返回的 timeout 错误）：属于调用方的控制流。
	var ne net.Error
	if errors.As(err, &ne) && ne.Timeout() {
		return StatusTimeout
	}

	// 系统级网络断开错误码（复用 isCommonNetError 的平台白名单，避免各平台漂移）。
	if isCommonNetError(err) {
		return StatusPeerReset
	}

	return StatusUnknown
}
