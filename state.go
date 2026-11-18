package vconn

import "errors"

// Status 是本包唯一的「连接结论」类型：一个字节同时表达两个正交轴——
//
//	低 4 位（Class）：类别，即「发生了什么」（观测）/「该怎么处置」（错误）
//	第 4 位（Unread）：修饰标志，即「是否还有未读数据」
//
//	st, err := c.CheckReusable()
//	if st == vconn.StatusAlive {
//		reuse()                       // 干净存活，可以交给下一个使用者
//	}
//	switch vconn.StatusOf(err) {       // 同一个类型、同一套常量
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
//	Retryable() 换一条连接 / 稍后再试有意义吗？
//	Terminal()  这条连接还能用吗？
//
// 类型同一 ≠ 取值相同，注意两条不对称：
//   - StatusOf(err) 可能答出 StatusTimeout（连接没坏，是调用方的控制流），
//     而 Check* 永不返回 StatusTimeout；
//   - Check* 可能返回 StatusDataPending（有数据，不是错误），而 StatusOf(ErrConnDirty)
//     恰好也是同一个值——因为「脏」对两条轴本就是同一件事。
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

	// StatusMisuse 调用方式错误（重复 RawConn、越界 ResetNotify）。不重试，修代码。
	StatusMisuse

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
		b = "quota-exhausted"
	case StatusTimeout:
		b = "timeout"
	case StatusMisuse:
		b = "misuse"
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

// ==================== 纯快照 ====================

// Snapshot 返回连接「已确定的结论」快照：不探测、不落定、不产生通知。
//
// 可达集合（除 ResetNotify 外只增不减，因此过期是安全的）：
//
//	StatusLocalClosed / StatusPeerClosed(±WithUnread) / StatusPeerReset(±WithUnread) /
//	StatusLatched / StatusDataPending / StatusUnverified
//
// 它**不会**返回 StatusAlive：缓存无法证明「对端还活着」，那需要一次探测
// （Check / CheckBlocking）。这正是本方法安全的原因——过期的「已结束」仍然是真的，
// 而过期的「存活」可能是假的。
//
// 用途：探测前的快速否定过滤（已知死就不必再花系统调用）、指标、日志，
// 以及替代 IsPeerClosed 做语义化判断。需要「现在到底怎么样」请用 Check。
//
//	if c.Snapshot().Ended() {       // 已知死：省掉一次探测
//		return pool.Drop
//	}
//	if st, err := c.Check(); !st.Readable() { ... }
//
// 方法名既不叫 State()（随类型 State 一起并入 Status），也不叫 Status()
// ——后者会和 Statuser（错误自己声明分类的接口：Status() Status）撞名，
// 让 *Conn 意外实现该接口。Snapshot 描述的就是它真正做的事：取快照。
func (c *Conn) Snapshot() Status {
	if c.closed.Load() || c.rawUsed.Load() {
		return StatusLocalClosed
	}

	c.mu.Lock()
	down, notified, cerr := c.peerDown, c.notified.Load(), c.closeErr
	c.mu.Unlock()

	if down {
		// 锁外取读，避免 c.mu → peekMu 的嵌套（虽然当前锁序安全，但不必制造约束）
		return peerState(cerr, c.r.hasPeek())
	}
	if notified {
		// CancelNotify / WithContext 取消：连接未必坏，但通知已落定。
		return StatusLatched
	}
	if c.r.hasPeek() {
		return StatusDataPending
	}
	return StatusUnverified
}

// ==================== 分类核心 ====================

// isCleanPeerClose 判断探测给出的结束原因是否为「干净的 FIN」。
//
// 这是全包唯一判定「干净 FIN」的地方，三处依赖它，判定必须只有一份：
//   - probePeekAware：只有干净 FIN 才允许把 probeClosed 升级为 probeReadableHangup；
//   - peerState（下方）：干净 FIN → StatusPeerClosed，否则 StatusPeerReset；
//   - StatusOf（kind.go）：异常原因必须归到 StatusPeerReset，而不是 StatusPeerClosed。
//
// 判定规则 = StatusOf 判为「对端正常结束」**且**不带真实 cause：
// 带 cause 的哨兵（peerClosedWithUnreadError{cause: ECONNRESET}）虽然 Unwrap 里含
// io.EOF，但真实原因可能是 RST，抹成「正常收尾」会让上层对非幂等请求做出错误推断。
func isCleanPeerClose(err error) bool {
	if err == nil {
		return true
	}
	if StatusOf(err).Class() != StatusPeerClosed {
		return false
	}
	var causer interface{ Cause() error }
	if errors.As(err, &causer) && causer.Cause() != nil {
		return false
	}
	return true
}

// peerState 把「对端结束」的原因归类为干净 FIN（StatusPeerClosed）或异常中断（StatusPeerReset），
// 并叠加 unread 修饰位。
//
// 分类不在这里做第二遍：类别取自 StatusOf（唯一映射表），本函数只做两件事——
//
//  1. 把类别**收窄**到「对端结束」这两个值。原因无法归类（StatusUnknown：
//     自定义错误、平台未收录的 errno）时保守报 StatusPeerReset ——
//     「对端已经没了，但为什么不知道」按异常处理，绝不上报 StatusPeerClosed
//     让上层误以为请求已执行完；
//  2. 叠加 unread 位（数据可能在内核队列，也可能在 vconn 的预读缓冲）。
func peerState(err error, unread bool) Status {
	st := StatusOf(err).Class()
	switch st {
	case StatusPeerClosed, StatusPeerReset:
		// 保持 StatusOf 的判定
	default:
		st = StatusPeerReset
	}
	if unread {
		st |= flagUnread
	}
	return st
}
