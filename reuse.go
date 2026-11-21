package vconn

import (
	"errors"
	"fmt"
	"io"
)

// ==================== 池化专用错误 ====================
//
// 命名规则与 conn.go 的哨兵完全一致：Err[域]状态词[修饰]。
//
// 这些错误只由 CheckReusable 返回（或被它包进 notReusableError），全部满足
// errors.Is(err, ErrNotReusable)，并能用 StatusOf 一步分类。
//
// 它们不会被写进 closeErr，也不会分发关闭通知：它们描述的是
// 「这条连接现在适不适合复用」，不是「连接已经死了」。
//
// 下面按类别分组，与 conn.go 的哨兵同一套规则。

var (
	// —— 伞错误：不含状态词，必须配合 StatusOf ——

	// ErrNotReusable 是 CheckReusable 失败错误的公共伞。
	//
	// 具体原因（io.EOF、ErrDataPending、ErrBusy 等）通过 Unwrap 一起暴露，
	// 所以 errors.Is(err, ErrNotReusable) 与 errors.Is(err, 具体原因) 可以同时成立。
	// 裸用本错误（不经 CheckReusable 包装）没有具体原因，StatusOf 会归为 StatusUnknown
	// —— 这正是它不带状态词的原因：它是「容器」，不是「结论」。
	ErrNotReusable = errors.New("vconn: connection is not reusable")

	// —— 类别：StatusDataPending ——

	// ErrDataPending 连接存活，但存在未读数据（协议失步 / 残留响应）。
	//
	// 类别：StatusDataPending。名字与状态同词：对池化调用方而言，
	// 「有残留数据」就是「这条连接不干净」——所以它同时是 Check* 的观测结论
	// 与 CheckReusable 的拒绝理由。
	ErrDataPending = errors.New("vconn: unexpected data pending on idle connection")

	// —— 类别：StatusUnverified ——

	// ErrUnverified 无法验证连接状态（拿不到 fd / 不支持阻塞探测 / 探测被占用）。
	//
	// 类别：StatusUnverified。**不是死亡**，只是「现在没法判定」。
	ErrUnverified = errors.New("vconn: connection liveness could not be verified")

	// —— 类别：StatusLatched ——

	// ErrNotifyLatched 关闭通知已落定（手动 / 上下文取消），
	// 未 ResetNotify 前不可复用。
	//
	// 类别：StatusLatched。与 conn.go 的 ErrNotifyCanceled 同类别但分工不同：
	// 本错误回答「为什么不能复用」，ErrNotifyCanceled 回答「这一轮为什么结束」。
	ErrNotifyLatched = errors.New("vconn: close notification already latched")

	// —— 类别：StatusBusy ——

	// ErrBusy 连接正被前台读取占用，无法判定；稍后重试即可。
	//
	// 类别：StatusBusy 是「忙」，不是「坏」。
	ErrBusy = errors.New("vconn: connection is in use")

	// —— 类别：StatusQuota ——

	// ErrQuota 读取配额（SetReadLimit）已耗尽，下一次 Read 直接返回 io.EOF。
	//
	// 用 %w 包住 io.EOF：按 io.EOF 判定的老代码行为不变，
	// 但 StatusOf 能把它与「对端关闭」区分开（StatusQuota vs StatusPeerClosed）。
	ErrQuota = fmt.Errorf("vconn: read quota exhausted: %w", io.EOF)
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
// 拿不到 io.EOF / ErrDataPending / ErrUnverified 这些真正有用的分类。
func (e notReusableError) Status() Status { return StatusOf(e.reason) }

func notReusable(reason error) error { return notReusableError{reason: reason} }
