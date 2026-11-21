package vconn

import (
	"context"
	"errors"
	"io"
	"net"
	"os"
)

// Status 是本包唯一的「连接结论」类型：一个字节同时表达两个正交轴——
//
//	低 4 位（Class）：类别，即「发生了什么」（观测）/「该怎么处置」（错误）
//	第 4 位（Unread）：修饰标志，即「是否还有未读数据」
//
// 位域这样切是为了让 switch 只看类别：StatusPeerClosedWithUnread 与
// StatusPeerClosed 的 Class() 相同，调用方不会因为「还有数据」漏进 default。
// 类别超过 15 个会和 Unread 位重叠，state.go 底部的编译期除法会直接编译失败。
//
//	err := c.CheckReusable()
//	switch vconn.StatusOf(err) {       // 同一个类型、同一套常量
//	case vconn.StatusAlive:			   // 干净存活，可以交给下一个使用者
//	case vconn.StatusPeerClosed:       // 对端正常结束 → 先 drain，幂等请求可重试
//	case vconn.StatusPeerReset:        // 异常中断 → 换连接；非幂等请求不要重试
//	case vconn.StatusLocalClosed,
//		vconn.StatusDataPending:       // → 丢弃
//	case vconn.StatusBusy,
//		vconn.StatusUnverified:        // → 退避后重试
//	}
//
// # 两个轴，一个类型
//
// 观测轴（Check / CheckBlocking / CheckReusable / Snapshot 给出的现实结论）：
//
//	Readable()  现在还能读出数据吗？
//	Reusable()  能交给下一个使用者吗？
//	Ended()     这条连接已经结束了吗？
//
// 处置轴（StatusOf(err) 给出的动作建议）：
//
//	Terminal()  这条连接还能用吗？
//
// 「要不要换连接 / 稍后重试」不单独给谓词：Terminal() 为真就换连接，
// StatusBusy / StatusUnverified 则是「此刻没法判定，退避后再问」。
//
// 类型同一 ≠ 取值相同，注意两条不对称：
//   - StatusOf(err) 可能答出 StatusTimeout（连接没坏，是调用方的控制流），
//     而 Check* 永不返回 StatusTimeout；
//   - Check* 可能返回 StatusDataPending（有数据，不是错误），而 StatusOf(ErrDataPending)
//     恰好也是同一个值——因为有残留数据对两条轴本就是同一件事。
//
// 零值即「无错误」：StatusOf(nil) == StatusAlive，其 String() 为 "alive"。
//
// 调用方只使用下面导出的组合常量，不要自己拼位。
type Status uint8

// 结论类别（低 4 位）。
//
// 前 9 个是两条轴共用的类别；后 4 个只可能出现在错误空间
// ——Check / CheckBlocking / CheckReusable / Snapshot 永不返回它们。
const (
	// StatusAlive 存活、干净、无错误。零值。
	StatusAlive Status = iota

	// StatusDataPending 存活，但有未读数据（协议失步 / 残留响应）。
	StatusDataPending

	// StatusPeerClosed 对端正常结束（FIN）。
	//
	// 语义提示：这不是故障，是「先读完再收尾」的信号。
	StatusPeerClosed

	// StatusPeerReset 对端异常中断或网络故障（RST / EPIPE / 网络不可达 / 超时 / 截断）。
	StatusPeerReset

	// StatusLocalClosed 本地已 Close / 已提取原始连接。
	StatusLocalClosed

	// StatusUnverified 无法判定（内核探测不可用 / 不支持阻塞探测 / 被占用）。
	// **不是死亡**，保守处理即可。
	StatusUnverified

	// StatusBusy 连接正被前台读取占用，稍后可重试。
	StatusBusy

	// StatusLatched 关闭通知已落定（手动 / 上下文取消），未 ResetNotify 前不可复用。
	StatusLatched

	// StatusQuota 读取配额（SetReadLimit）已耗尽，下一次 Read 直接返回 io.EOF。
	StatusQuota

	// —— 以下仅出现在错误空间 ——

	// StatusTimeout 调用方自己设置的 deadline 到期。连接未必坏。
	StatusTimeout

	// StatusInUse 调用方式错误（重复 RawConn、越界 ResetNotify）。不重试，修代码。
	StatusInUse

	// StatusUnsupported 平台或底层连接不支持该操作。
	StatusUnsupported

	// StatusUnknown 无法归类（含 CancelNotify 传入的自定义错误）。
	StatusUnknown
)

const (
	statusMask Status = 0x0f   // 类别掩码
	flagUnread Status = 1 << 4 // 仍有未读数据
)

// 编译期保证类别只占低 4 位：一旦新增类别越过 16 个，这里除零、编译失败。
// （位域是这个类型的地基，越界会与 flagUnread 互相污染，必须在编译期挡住。）
const _ = 1 / (1 - StatusUnknown/16)

// 组合常量：调用方只需要认识这些（类别 + unread 修饰）。
const (
	// StatusPeerClosedWithUnread 对端已结束，但仍有数据可读：先 drain 再收尾。
	//
	// 语义提示：这不是错误。对端「发完数据再关闭写方向」是最常见的正常收尾，
	// 这条状态只说明还有数据没拿走；此时不要直接 Close，否则会丢数据（甚至发 RST）。
	StatusPeerClosedWithUnread = StatusPeerClosed | flagUnread

	// StatusPeerResetWithUnread 对端异常中断，但仍有数据可读。
	StatusPeerResetWithUnread = StatusPeerReset | flagUnread
)

// Class 返回不带修饰标志的类别，用于 switch（避免被 flagUnread 干扰）。
func (s Status) Class() Status { return s & statusMask }

// Unread 报告连接上是否还有未被取走的数据（内核接收队列或预读缓冲）。
func (s Status) Unread() bool { return s&flagUnread != 0 }

// Readable 报告连接是否仍能读出数据。
//
// 与 Ended 是两个不同的轴：对端已 FIN 但仍有未读数据时，
// Readable 与 Ended 会同时为真——「先读完，再收尾」。
func (s Status) Readable() bool {
	switch s.Class() {
	case StatusAlive, StatusDataPending:
		return true
	case StatusPeerClosed, StatusPeerReset:
		return s.Unread() // 有未读 = 还能读，别急着收尾
	}
	return false
}

// Reusable 报告该状态是否可以交给下一个使用者（只有干净存活算）。
func (s Status) Reusable() bool { return s == StatusAlive }

// Is 报告 err 是否归于本类别（带 unread 修饰的变体同样命中）。
//
// 它等价于 StatusOf(err).Class() == s.Class()，是后者的可读性糖：
//
//	if vconn.StatusDataPending.Is(err) {
//		return pool.Drop
//	}
//	if vconn.StatusBusy.Is(err) {
//		backoff()
//		continue
//	}
//
// 两点注意：
//   - 只比较类别，不比较 unread 修饰位。StatusOf 返回的是错误自己声明的 Status：
//     Check / CheckBlocking / CheckReusable 返回的 *result 会把「检查当时的
//     unread 位」一并带上，因此 StatusOf(err).Unread() 在错误侧同样可用；
//     而 CloseNotify 给出的 peerClosedWithUnreadError 不带该位（它描述创建时刻的
//     状态，之后数据可能已被读走）。要区分「队列读空的干净结束」与「还有数据没读完」：
//     观测侧看 StatusOf(err).Unread()（Check* 路径）或 Snapshot().Unread()；
//     错误侧看手上的这个错误本身是否 errors.Is(err, ErrPeerClosedWithUnread)；
//   - StatusOf(nil) == StatusAlive，因此 StatusAlive.Is(nil) 为 true。
func (s Status) Is(err error) bool {
	return StatusOf(err).Class() == s.Class()
}

// Ended 报告「这条连接的对端或本地已经结束」。
//
// 注意：Ended 为真不等于必须立刻 Close —— StatusPeerClosedWithUnread 仍应先 drain。
// 同时 StatusUnverified / StatusBusy / StatusLatched 都不是 Ended。
func (s Status) Ended() bool {
	switch s.Class() {
	case StatusPeerClosed, StatusPeerReset, StatusLocalClosed:
		return true
	}
	return false
}

// PeerDown 报告是否探测到对端断开（正常 FIN 或异常中断）。
// 与 Ended 的差别：本地主动 Close 不算 PeerDown。
func (s Status) PeerDown() bool {
	switch s.Class() {
	case StatusPeerClosed, StatusPeerReset:
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

func (s Status) String() string {
	var b string
	switch s.Class() {
	case StatusAlive:
		b = "alive"
	case StatusDataPending:
		b = "data-pending"
	case StatusPeerClosed:
		b = "peer-closed"
	case StatusPeerReset:
		b = "peer-reset"
	case StatusLocalClosed:
		b = "local-closed"
	case StatusUnverified:
		b = "unverified"
	case StatusBusy:
		b = "busy"
	case StatusLatched:
		b = "latched"
	case StatusQuota:
		b = "quota"
	case StatusTimeout:
		b = "timeout"
	case StatusInUse:
		b = "in-use"
	case StatusUnsupported:
		b = "unsupported"
	case StatusUnknown:
		b = "unknown"
	default:
		return "invalid" // 类别位被污染（拼位 / 越界常量）
	}
	if s.Unread() {
		b += "+unread"
	}
	return b
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
//	                   ErrPeerDown（ResetNotify 被拒：对端已下线，故归本类）
//	StatusPeerReset    io.ErrUnexpectedEOF / ECONNRESET / EPIPE / ENETUNREACH / 超时类断链
//	StatusLocalClosed  net.ErrClosed / io.ErrClosedPipe / ErrLocalClosed
//	StatusDataPending  ErrDataPending
//	StatusBusy         ErrBusy
//	StatusLatched      ErrNotifyLatched / ErrNotifyCanceled / context.Canceled
//	StatusUnverified   ErrUnverified
//	StatusQuota        ErrQuota
//	StatusTimeout      os.ErrDeadlineExceeded / context.DeadlineExceeded / net.Error.Timeout()
//	StatusInUse       ErrInUse
//	StatusUnsupported  ErrUnsupported
//	StatusUnknown      ErrNotReusable 裸用 / 自定义错误
//
// 表里每一行的哨兵，名字都带着所属类别的状态词（ErrBusy → StatusBusy），
// 所以「读名字猜类别」与实际分类一致；该契约由 status_test.go 的 errNamingTable
// 守着（Status.String() 的词干必须出现在名字里，或显式标注豁免理由）。
//
// 只想判「是否属于某类别」时用 Status.Is(err)，它等价于本函数的 Class() 比较：
//
//	if vconn.StatusBusy.Is(err) {
//		backoff()
//		continue
//	}
//
// 与 Check* 的关系（status_test.go 里的不变量测试）：
// Check* 只返回一个 error，结论由 *result 的 Status() 声明，本函数优先取用它，
// 因此 StatusOf(err).Class() 恒等于 Check* 给出的类别 —— 两轴不会漂移。
// 「原因无法归类」的例外只存在于 Cause() 轴：对端确实结束了、但原因归不进任何
// 类别时，Check* 保守报 StatusPeerReset，而 StatusOf(cause) 如实报 StatusUnknown
// （cause 不是 *result，没有结论可以覆盖它）。
//
// *result 走「先问错误自己」这条分支，因此 StatusOf 的结果可能带 flagUnread ——
// 这是唯一会产生 unread 位的错误类型，其它哨兵保持不带位。
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
	case errors.Is(err, ErrQuota):
		return StatusQuota

	// 流在中途被截断：对端没有正常收尾，不能算 StatusPeerClosed。
	case errors.Is(err, io.ErrUnexpectedEOF):
		return StatusPeerReset

	// 对端正常结束（含 unread 变体）。
	case errors.Is(err, ErrPeerClosedWithUnread), errors.Is(err, io.EOF):
		return StatusPeerClosed

	// 本地已关闭（含管道：net.Pipe 本地端关闭时 Read/SetReadDeadline 返回 io.ErrClosedPipe）。
	case errors.Is(err, net.ErrClosed), errors.Is(err, io.ErrClosedPipe), errors.Is(err, ErrLocalClosed):
		return StatusLocalClosed

	case errors.Is(err, ErrDataPending):
		return StatusDataPending

	case errors.Is(err, ErrBusy):
		return StatusBusy

	case errors.Is(err, ErrNotifyLatched), errors.Is(err, ErrNotifyCanceled), errors.Is(err, context.Canceled):
		// 上下文取消走的是"手动通知"路径：连接本身未必坏，
		// 只是这一轮等待被撤销了（需要 ResetNotify 才能重新武装）。
		return StatusLatched

	case errors.Is(err, ErrUnverified):
		return StatusUnverified

	case errors.Is(err, ErrInUse):
		return StatusInUse

	case errors.Is(err, ErrUnsupported):
		return StatusUnsupported

	case errors.Is(err, ErrPeerDown):
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
