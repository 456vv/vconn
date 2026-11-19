package vconn

import (
	"errors"
	"fmt"
	"io"
	"net"
)

// ==================== 池化专用错误 ====================
//
// 这些错误只由 CheckReusable 返回，全部满足
// errors.Is(err, ErrNotReusable)，并能用 StatusOf 进一步分类。
//
// 它们不会被写进 closeErr，也不会分发关闭通知：它们描述的是
// 「这条连接现在适不适合复用」，不是「连接已经死了」。

var (
	// ErrNotReusable 是 CheckReusable 失败错误的公共伞。
	//
	// 具体原因（io.EOF、ErrConnDirty、ErrBusy 等）通过 Unwrap 一起暴露，
	// 所以 errors.Is(err, ErrNotReusable) 与 errors.Is(err, 具体原因) 可以同时成立。
	// 裸用本错误（不经 CheckReusable 包装）没有具体原因，StatusOf 会归为 StatusUnknown。
	ErrNotReusable = errors.New("vconn: connection is not reusable")

	// ErrConnDirty 连接存活，但存在未读数据（协议失步 / 残留响应）。
	ErrConnDirty = errors.New("vconn: unexpected data pending on idle connection")

	// ErrUnverified 无法验证连接状态（拿不到 fd / 不支持阻塞探测 / 探测被占用）。
	ErrUnverified = errors.New("vconn: connection liveness could not be verified")

	// ErrNotificationLatched 关闭通知已落定（手动 / 上下文取消），
	// 未 ResetNotify 前不可复用。
	ErrNotificationLatched = errors.New("vconn: close notification already latched")

	// ErrBusy 连接正被前台读取占用，无法判定；稍后重试即可。
	ErrBusy = errors.New("vconn: connection is in use")

	// ErrQuotaExhausted 读取配额（SetReadLimit）已耗尽，下一次 Read 直接返回 io.EOF。
	//
	// 用 %w 包住 io.EOF：按 io.EOF 判定的老代码行为不变，
	// 但 StatusOf 能把它与「对端关闭」区分开（StatusQuota vs StatusPeerClosed）。
	ErrQuotaExhausted = fmt.Errorf("vconn: read quota exhausted: %w", io.EOF)
)

type notReusableError struct{ reason error }

func (e notReusableError) Error() string {
	return "vconn: connection is not reusable: " + e.reason.Error()
}

// Unwrap 同时匹配 ErrNotReusable 与具体原因，调用方可以任选粒度。
func (e notReusableError) Unwrap() []error {
	return []error{ErrNotReusable, e.reason}
}

// Status 委派给真实原因。
//
// 必须委派：否则 StatusOf 会停在 ErrNotReusable 这把伞上，
// 拿不到 io.EOF / ErrConnDirty / ErrUnverified 这些真正有用的分类。
func (e notReusableError) Status() Status { return StatusOf(e.reason) }

func notReusable(reason error) error { return notReusableError{reason: reason} }

// ==================== 复用判定 ====================

// CheckReusable 连接池专用：判断连接能否交给下一个使用者。
//
// 返回 nil 当且仅当 Status == StatusAlive（存活、干净、无延迟错误、
// 无落定通知、读取配额未耗尽）。失败时错误都满足 errors.Is(err, ErrNotReusable)，
// 并可用 StatusOf 一步分类（StatusDataPending / StatusBusy / StatusUnverified / StatusLatched /
// StatusPeerClosed / StatusPeerReset / StatusLocalClosed / StatusQuota）。
//
//	st, err := c.CheckReusable()
//	if err != nil {
//		metrics.Drop(st.String(), vconn.StatusOf(err))
//		_ = c.Close()
//		return err // 换一条连接重试
//	}
//
// 判定顺序是故意从便宜到贵：本地关闭、前台占用、遗留错误、已落定通知、
// 读取配额，最后才做一次探测。前面任何一步失败都不再碰 socket。
//
// 约束（务必遵守，否则判定会失真）：
//   - 只能在空闲连接上调用：需要「没有其它 goroutine 正在 Read/Write」。
//     前台正持有读锁时返回 StatusBusy，而不是猜测连接死活；
//   - 不关闭连接，也不修改调用方的 ReadDeadline；
//   - 有 fd 时零消耗（SO_ERROR + poll + MSG_PEEK），无 fd 时可能读 1 字节进预读缓冲
//     （探测额度见 SetBackgroundReadBuffer；显式设为 0 或负值时不探测，返回 ErrUnverified）；
//   - 不适用于 server-speaks-first / 推送型协议（它们天然有未读数据，恒判 StatusDataPending）；
//   - 池里的连接不要开 SetBackgroundReadBuffer(>0)，预读字节会被判脏；
//   - 用 NewData 交接进来的字节同样会被判脏；
//   - TLS 内部缓冲不可见，只能尽力而为；业务仍需支持换连接重试。
func (c *Conn) CheckReusable() (Status, error) {
	if c.closed.Load() || c.rawUsed.Load() {
		return StatusLocalClosed, notReusable(net.ErrClosed)
	}

	// 1. 延迟错误（含 TLS close_notify）：下一次 Read 会立刻吐出来。
	pending, idle := c.r.pendingError()
	if !idle {
		return StatusBusy, notReusable(ErrBusy)
	}
	if pending != nil {
		return peerState(pending, c.r.hasPeek()), notReusable(pending)
	}

	// 2. 已落定的手动/上下文通知，未 ResetNotify 前不可复用。
	if c.notified.Load() && !c.IsPeerClosed() {
		return StatusLatched, notReusable(ErrNotificationLatched)
	}

	// 3. 读取配额耗尽：下一次 Read 直接 EOF。
	if c.r.readLimit.Load() == 0 {
		return StatusQuota, notReusable(ErrQuotaExhausted)
	}

	// 4. 已落定的断开结论：直接复用结论，避免重复探测与原因漂移。
	if down, cerr := c.latchedPeer(); down {
		return peerState(cerr, c.r.hasPeek()), notReusable(cerr)
	}

	// 5. 探测（唯一实现；block=true 让无 fd 的连接也能退化到阻塞预读）。
	st, err := c.probe(true)

	switch st.Class() {
	case StatusAlive:
		return st, nil
	case StatusDataPending:
		return st, notReusable(ErrConnDirty)
	case StatusUnverified:
		return st, notReusable(ErrUnverified)
	case StatusBusy:
		return st, notReusable(ErrBusy)
	case StatusLocalClosed:
		return st, notReusable(net.ErrClosed)
	default: // StatusPeerClosed / StatusPeerReset（含 +unread 变体）
		if err == nil {
			err = io.EOF
		}
		return st, notReusable(err)
	}
}

// latchedPeer 读取已落定的「对端已断开」结论（连接级，first-wins）。
func (c *Conn) latchedPeer() (bool, error) {
	c.mu.Lock()
	down, err := c.peerDown, c.closeErr
	c.mu.Unlock()

	if !down {
		return false, nil
	}
	if err == nil {
		err = io.EOF
	}
	return true, err
}
