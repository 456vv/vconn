package vconn

import (
	"errors"
	"io"
	"net"
	"os"
	"sync"
	"sync/atomic"
	"time"
)

// connReader 负责协调前台读取与后台预读：
//   - 前台读用 fg 串行化，多个 goroutine 并发 Read 不会互相打断；
//   - 预读只在连接空闲且无人读取时（fg.TryLock 成功）进行；
//   - 预读使用自身超时，结束后按最新快照恢复调用方的 ReadDeadline；
//   - 读取限制用 CAS 扣减，并发下不会超发。
//
// 三条内部不变量（修改时必须保持）：
//  1. r.peek 的读写只发生在 peekMu 临界区内；不使用可能与切片不同步的原子标志
//     （标志与切片不同步会导致字节乱序交付或数据滞留）；
//  2. 任何一次可能阻塞的 socket 读，都必须在 deadlineMu 内
//     「先置位 peekBusy，再写自身 deadline」，保证 stopMonitor 一定能唤醒它；
//  3. 后台预读的「是否该退出」由传入的 stop 句柄（本轮 monitorRun.quit）决定，
//     不使用 c.mon 反查（换代瞬间反查会认错实例）。
type connReader struct {
	conn *Conn

	fg sync.Mutex // 串行化所有前台读（含预读字节消费）

	peekMu sync.Mutex
	peek   []byte

	peekTmp  []byte      // 预读临时缓冲，复用避免反复分配
	peekBusy atomic.Bool // 是否正处于阻塞式预读中（此时独占 fg 锁）
	// noDeadline 底层 SetReadDeadline 已被证实不可用。
	// 一旦置位，后台预读永久跳过（安全优先：不可中断的阻塞读会拖住监控与 Close），
	// 同时避免每轮白白发起两次注定失败的 syscall。
	noDeadline atomic.Bool

	pending   error // 上次读取遗留的错误（数据与错误分两次返回）
	readLimit atomic.Int64
}

func newConnReader(c *Conn) *connReader {
	r := &connReader{conn: c}
	r.readLimit.Store(-1) // 默认不限流
	return r
}

// setReadLimit 设置读取限制。-1 为不限制，>=0 限制具体长度
func (r *connReader) setReadLimit(remain int64) {
	if remain < 0 {
		r.readLimit.Store(-1)
		return
	}
	r.readLimit.Store(remain)
}

// takePeek 原子地取走预读缓冲区
func (r *connReader) takePeek() []byte {
	r.peekMu.Lock()
	b := r.peek
	r.peek = nil
	r.peekMu.Unlock()
	return b
}

// pushPeek 把字节压入预读缓冲区
//
// 这是唯一不被 fg 保护的追加路径（NewData 对已存在 Conn 的补充），
// 因此必须在 peekMu 内完成"追加"的全部动作，保证前台读看到的数据
// 与切片内容严格一致。
func (r *connReader) pushPeek(b []byte) {
	if len(b) == 0 {
		return
	}
	r.peekMu.Lock()
	r.peek = append(r.peek, b...)
	r.peekMu.Unlock()
}

// hasPeek 报告预读缓冲区里是否还有尚未被调用方读走的字节。
func (r *connReader) hasPeek() bool {
	r.peekMu.Lock()
	defer r.peekMu.Unlock()
	return len(r.peek) > 0
}

// consumeLimit 原子扣减读取配额
func (r *connReader) consumeLimit(n int, limit int64) {
	if limit <= 0 || n <= 0 {
		return
	}
	for {
		cur := r.readLimit.Load()
		if cur <= 0 {
			return
		}
		next := max(cur-int64(n), 0)
		if r.readLimit.CompareAndSwap(cur, next) {
			return
		}
	}
}

// Read 实现流式读取，具备限流、预读字节消费等复合功能
// 实现 io.Reader 接口，线程安全
func (r *connReader) Read(p []byte) (n int, err error) {
	if len(p) == 0 {
		return 0, nil
	}

	r.fg.Lock()
	defer r.fg.Unlock()

	// 1. 上次读取遗留的终止错误：早于配额判定（与 CheckReusable 的顺序一致），
	//    但晚于预读字节 —— 预读里的字节在流顺序上排在它之前，有预读时先交付数据，
	//    pending 留到下一次。hasPeek 只在 pending 非 nil 时调用，热路径零开销。
	if r.pending != nil && !r.hasPeek() {
		err = r.pending
		r.pending = nil
		// 同样经过连接级规整：pending 可能是 net.ErrClosed，而对端实际是异常断开
		// （真实原因已被探测路径从 SO_ERROR 读走），这条路径不能漏掉 ECONNRESET。
		if term := r.conn.terminalReadErr(err); term != nil {
			return 0, term
		}
		return 0, err
	}

	// 2. 读取配额：配额是「允许交付多少字节」的闸门，必须先截断 p，
	//    因此它排在预读拷贝之前（limit == 0 时预读字节也不交付，留在缓冲里）。
	limit := r.readLimit.Load()
	if limit == 0 {
		// 用哨兵而不是裸 io.EOF：StatusOf 能把它与「对端关闭」区分开
		// （StatusQuota vs StatusPeerClosed），否则池化代码会为一次配额用尽
		// 去重连。哨兵用 %w 包住 io.EOF，按 io.EOF 判定的语义不变。
		return 0, ErrQuota
	}
	if limit > 0 && int64(len(p)) > limit {
		p = p[:limit]
	}

	// 3. 预读缓冲优先：拿到字节就立即返回，绝不为了凑满 p 再向 socket 发起
	//    一次可能阻塞的读（那会把已经到手的字节扣到 deadline 甚至永久）。
	r.peekMu.Lock()
	if len(r.peek) > 0 {
		cn := copy(p, r.peek)
		r.peek = r.peek[cn:]
		if len(r.peek) == 0 {
			r.peek = nil
		}
		n += cn
	}
	r.peekMu.Unlock()

	if n > 0 {
		r.consumeLimit(n, limit)
		return n, nil
	}

	// 4. 本地已关闭（走到这里预读必为空，不会有数据被丢弃）
	if r.conn.closed.Load() {
		return 0, net.ErrClosed
	}

	// 5. 真实网络读取（前台独占，不会被后台预读打断）
	rn, rerr := r.conn.rwc.Read(p)
	n = rn
	if rn > 0 {
		// 无条件计数：监控启动前的读也必须计入，否则首轮会把活跃连接误判为空闲。
		//
		// 注意：这里故意**不**统计「仅消费预读缓冲」的读取。readSeq 的语义是
		// "内核 socket 上最近有没有读活动"，用于决定监控是否值得做零消耗内核探测。
		r.conn.readSeq.Add(1)
	}
	// 配额按「实际交付给调用方的字节数」扣减。预读已在第 3 步提前返回，
	// 此处 n == rn；仍用 n 而非 rn，是为了让将来有人重新引入「预读 + socket
	// 同一次交付」时，不会悄悄退化成「配额被预读字节绕过」。
	r.consumeLimit(n, limit)

	if rerr != nil {
		if n > 0 {
			// 已有数据：先返回数据，错误留到下一次，避免调用方丢弃最后一段数据。
			// 除超时外的错误都要留存（超时是控制流，重放没有意义）。
			if !isTimeoutErr(rerr) {
				r.pending = rerr
			}
			return n, nil
		}
		// 结束原因规整：探测路径可能已经把 SO_ERROR 读走，
		// 这里用连接级结论替换底层那个"干净"的 EOF，避免 ECONNRESET 被降级。
		if term := r.conn.terminalReadErr(rerr); term != nil {
			return 0, term
		}
		return 0, rerr
	}
	return n, nil
}

// resolvePeekSkip 决定本次后台/探活预读是否必须跳过。
//
// 分开「内核已确认可读」与「需要干等一会儿」，是为了保护有协议状态的包装连接：
// TLS、多路复用的 Read 不是纯字节搬运，只有直接持有 fd 的连接才能把原始字节
// 放进预读缓冲。
//
//	probeReadable / probeReadableHangup  直接连接 → 读；包装连接 → 跳过
//	                                     （读到的是密文/帧，会破坏记录边界）
//	probeIdle                            直接连接且 allowBlock → 读（等待型探测）；
//	                                     其它 → 跳过（没有可观察的就绪事件）
//	probeUnsupported                     isBlockProbeSafe → 读；其它 → 跳过（安全优先）
//
// 所有预读一律带自身超时（poll 与 Read 之间数据可能被前台读消费，无超时的 Read
// 会永久阻塞并占住 fg），因此这里不需要「是否设超时」的开关。
func resolvePeekSkip(c net.Conn, res probeResult, allowBlock bool) bool {
	direct := isDirectSyscallConn(c)
	switch res {
	case probeReadable, probeReadableHangup:
		return !direct
	case probeIdle:
		return !direct || !allowBlock
	default: // probeUnsupported
		return !isBlockProbeSafe(c)
	}
}

// peekStep 执行一次后台预读（或丢弃）动作。
// 返回 closed=true 表示探测到连接已关闭（err 为原因）。
//
// stop 为**本轮监控实例**的退出信号（monitorRun.quit）。发起阻塞读之前必须先
// 检查它，否则监控换代时旧实例仍会发起新的阻塞读，无谓拖长 stopMonitor。
//
// res 为上一步内核探测的结果；allowBlock 为 true 时允许一次等待型探测。
//
// 关键约束：
//   - 只在 fg 可获取（无前台读）时执行，绝不干扰前台读；
//   - 预读模式的缓冲长度严格等于缺口 want（绝不越配额多读）；
//     丢弃模式按 |n| 决定单次读取长度（不足 minPeekBuf 按 minPeekBuf），一次吞掉更多；
//   - 任何一次 socket 读都带自身超时
//     结束后恢复调用方设置的 ReadDeadline。
func (r *connReader) peekStep(stop <-chan struct{}, res probeResult, allowBlock bool) (closed bool, err error) {
	size := int(r.conn.peekSize.Load())
	if size == 0 || r.conn.closed.Load() {
		return false, nil
	}

	discard := size < 0
	// 丢弃模式下 want 保持负值：既表示「丢弃」（discard），其绝对值又是本次读取的
	// 缓冲长度（由 tmpBuf 消解负号）。预读模式必须扣掉已有预读量，避免越配额多读。
	want := size
	if !discard {
		r.peekMu.Lock()
		have := len(r.peek)
		r.peekMu.Unlock()
		if have >= want {
			return false, nil // 预读缓冲已满，等前台消费
		}
		want -= have
	}

	if resolvePeekSkip(r.conn.rwc, res, allowBlock) {
		// 没有可观察的就绪事件，或该连接不能安全地提前读
		return false, nil
	}

	// 前台正在读，让路（不会造成任何延迟）
	if !r.fg.TryLock() {
		return false, nil
	}
	defer r.fg.Unlock()

	// 统一带自身超时：即使内核已报"可读"，poll 与 Read 之间数据仍可能被前台读
	// 消费掉（fg 是在 poll 之后才拿到的），无超时的 Read 会永久阻塞并一直占着 fg，
	// 让所有前台 Read 饿死。
	return r.peekRead(stop, want, discard, peekTimeout)
}

// pendingError 返回上一次读取遗留的错误（数据与错误分两次返回的那一个）。
//
// idle=false 表示前台正在读（或后台预读持锁），此时无法给出确定答案，
// 调用方应视为「忙」而不是「不可复用」。
func (r *connReader) pendingError() (err error, idle bool) {
	if !r.fg.TryLock() {
		return nil, false
	}
	defer r.fg.Unlock()
	return r.pending, true
}

// peekProbeStrict 执行一次「最小化」阻塞预读探测（单次最多读 1 字节），
// 供 CheckBlocking / CheckReusable 在内核探测不可用（拿不到 fd）时兜底。
//
// size 为本次探测可用的预读额度（由 Conn.peekProbeBudget 决定）：
// 它只影响本次调用，不会写回连接状态。
//
// 与 peekStep 的差异：
//   - 单次读长度固定为 1（peekStep 预读模式读 want = size-have）；
//   - 预读模式下若 peek 缓冲已达 size 上限，视为「有未读数据」，
//     直接返回 probeReadable（不阻塞、不报错、不再读）。
//     这只说明连接上还有数据，不说明它适合复用：上层会映射成 StatusDataPending；
//   - 丢弃模式下按 1 字节丢弃，不按 minPeekBuf 放大（避免高频探测吞掉大段数据）；
//     注意：丢弃模式已被 Conn.peekProbeBudget 挡在门外（返回 0 → 不调用本函数），
//     这里的支持仅为防御性保留；
//   - stop 传 nil：用户态探活不受监控换代影响，且自身超时保证可被唤醒。
//
// 无法探测（不支持 deadline / 前台在读 / 已本地关闭）时返回 probeUnsupported，
// 而不是伪装成「存活」。CheckReusable 必须能区分「确认存活」与「未能验证」，
// 否则会把状态未知的连接当作健康连接交给下一个使用者。
//
// 本地关闭与「底层不支持 deadline」在这里都返回 probeUnsupported：
// 本地关闭由 probe 在调用前后用 c.closed 改判为 StatusLocalClosed；
// 不支持 deadline 则保持 StatusUnverified。本函数不下「对端已结束」的结论。
func (r *connReader) peekProbeStrict(size int) (probeResult, error) {
	if size == 0 || r.conn.closed.Load() || r.noDeadline.Load() {
		return probeUnsupported, nil
	}
	discard := size < 0

	// 预读模式：缓冲已达上限 → 有未消费数据 → 连接必然存活，不再读取。
	// 丢弃模式 have 恒为 0，不参与此判断。
	if !discard {
		r.peekMu.Lock()
		have := len(r.peek)
		r.peekMu.Unlock()
		if have >= size {
			return probeReadable, nil
		}
	}

	if resolvePeekSkip(r.conn.rwc, probeUnsupported, true) {
		return probeUnsupported, nil
	}

	if !r.fg.TryLock() {
		// 前台正在读（或后台预读持锁）：让路，本次无法判定。
		return probeUnsupported, nil
	}
	defer r.fg.Unlock()

	closed, rerr := r.peekRead(nil, 1, discard, peekTimeout)

	if r.conn.closed.Load() || r.noDeadline.Load() {
		// 不下对端结论。本地关闭由 probe 用 c.closed 改判为 StatusLocalClosed；
		// 不支持 deadline 则保持 probeUnsupported，最终是 StatusUnverified。
		return probeUnsupported, nil
	}

	unread := r.hasPeek()
	if closed {
		if rerr == nil {
			rerr = io.EOF
		}
		if !discard && unread {
			// 与内核探测的 probeReadableHangup 语义对齐：数据仍可读。
			var sentinel peerClosedWithUnreadError
			if !errors.As(rerr, &sentinel) {
				rerr = peerClosedWithUnreadError{cause: nonEOF(rerr)}
			}
			return probeReadableHangup, rerr
		}
		return probeClosed, rerr
	}
	if !discard && unread {
		return probeReadable, nil
	}
	return probeIdle, nil
}

// peekRead 读取一批数据放入预读缓冲区（discard=true 时丢弃）。
//
// 不变量（务必保持）：只要本函数会碰 socket，就一定要
//  1. 在 deadlineMu 内判定"是否该退出"；
//  2. 在 deadlineMu 内置位 peekBusy；
//  3. 在 deadlineMu 内写入自身 deadline；
//  4. 函数返回前清除 peekBusy 并恢复调用方的 ReadDeadline。
//
// 这样 stopMonitor 只要看到 peekBusy=true，其"过去时间"的 deadline 就一定晚于
// 本次设置，从而必定能唤醒这次读；反过来，任何一次后台读都不会永久阻塞。
//
// timeout<=0 时按 peekReadTimeout 兜底（调用方一律显式传值，此处仅为防御）。
//
// 若底层连接不支持 deadline（SetReadDeadline 返回错误），则放弃本次阻塞预读：
// 否则这个后台读可能永远阻塞，把监控协程（甚至 Close/RawConnFull）一起拖住。
func (r *connReader) peekRead(stop <-chan struct{}, want int, discard bool, timeout time.Duration) (closed bool, err error) {
	if timeout <= 0 {
		timeout = peekReadTimeout
	}
	if r.noDeadline.Load() {
		// 该连接已证实不支持 deadline：不做后台读（没有可中断手段）。
		// 内核探测路径仍然生效，关闭检测不受影响。
		return false, nil
	}
	buf := r.tmpBuf(want)

	r.conn.deadlineMu.Lock()
	if r.conn.closed.Load() {
		r.conn.deadlineMu.Unlock()
		return false, nil
	}
	if stop != nil {
		select {
		case <-stop:
			// 本轮监控已被要求退出（关闭通知已落定 / RawConn / ResetNotify）：
			// 此时再发起阻塞预读只会白白拖延 stopMonitor，直接放弃本次预读。
			r.conn.deadlineMu.Unlock()
			return false, nil
		default:
		}
	}
	r.peekBusy.Store(true)
	derr := r.conn.rwc.SetReadDeadline(time.Now().Add(timeout))
	r.conn.deadlineMu.Unlock()

	if derr != nil {
		r.peekBusy.Store(false)
		// 连接已经处于终态时（net.Pipe 在 localDone/remoteDone 关闭后
		// 会直接从 SetReadDeadline 返回 io.ErrClosedPipe），必须如实上报，
		// 不能当成「无法探测」而放过——否则关闭通知永远不会触发。
		if errors.Is(derr, io.ErrClosedPipe) || errors.Is(derr, net.ErrClosed) {
			if !discard && r.hasPeek() {
				return true, peerClosedWithUnreadError{}
			}
			return true, io.EOF
		}
		// 其它情况（底层不支持 deadline）：无法保证可中断，直接跳过本次阻塞预读，
		// 并记为永久不可用，之后不再重试（避免每轮浪费两次注定失败的 syscall）。
		r.noDeadline.Store(true)
		return false, nil
	}
	defer func() {
		r.peekBusy.Store(false)
		r.conn.deadlineMu.Lock()
		r.conn.rwc.SetReadDeadline(r.conn.readDeadline)
		r.conn.deadlineMu.Unlock()
	}()

	n, rerr := r.conn.rwc.Read(buf)
	if n > 0 {
		if !discard {
			r.peekMu.Lock()
			// 注意：这里 append 的是已经从内核取出的字节，任何情况下都不得裁剪
			// （裁剪即丢数据）。并发 NewData 可能使长度短暂超过 SetBackgroundReadBuffer
			// 设定的上限，其超出量由 NewData 的入参体量界定，属调用方自担。
			r.peek = append(r.peek, buf[:n]...)
			r.peekMu.Unlock()
		}
	}

	if rerr != nil {
		if isTimeoutErr(rerr) {
			// 自身超时到期：这是等待型探测的正常结果，不是错误。
			// 注意：可能是 stopMonitor 的"过去时间" deadline 把它催促出来的，
			// 此时 fg 已释放，下一次 stopMonitor 会立即看到监控退出。
			return false, nil
		}
		// 阻塞预读同样会收到 FIN：若预读缓冲里还有字节（含本次刚读到的），
		// 语义与内核探测的 probeReadableHangup 一致，统一上报哨兵。
		// 丢弃模式下数据已被主动丢弃，不构成「未读数据」，按原错误上报。
		if !discard && errors.Is(rerr, io.EOF) {
			r.peekMu.Lock()
			hasUnread := len(r.peek) > 0
			r.peekMu.Unlock()
			if hasUnread {
				return true, peerClosedWithUnreadError{}
			}
		}
		return true, rerr
	}
	return false, nil
}

// tmpBuf 返回可复用的临时缓冲（避免每次预读都分配）。
//
// want > 0：预读模式，缓冲长度严格等于 want（多读会超出预读配额）；
// want < 0：丢弃模式，-want 为单次最多丢弃的字节数，不足 minPeekBuf 时按 minPeekBuf。
//
// 收缩策略：丢弃模式可能一次就把缓冲撑到 maxPeekSize。该缓冲随连接常驻，
// 若之后只需小额读取（如无 fd 连接探活的 1 字节），继续保留大缓冲会让
// 「曾用过大丢弃模式」的连接永久占用 1 MiB。这里在"当前需求不大且现有缓冲过大"时
// 直接释放，交回 GC。
func (r *connReader) tmpBuf(want int) []byte {
	if want <= 0 {
		// 丢弃模式取 |want|；want==0 仅作防御性兜底。预读模式不得走这里，
		// 否则缓冲被放大到 minPeekBuf 就会越配额多读。
		want = max(-want, minPeekBuf)
	}
	if want > maxPeekSize {
		want = maxPeekSize
	}
	if cap(r.peekTmp) > peekTmpKeep && want <= peekTmpKeep {
		r.peekTmp = nil // 释放历史大缓冲，避免 1 MiB/连接 常驻
	}
	if cap(r.peekTmp) < want {
		r.peekTmp = make([]byte, want)
	}
	return r.peekTmp[:want]
}

// isTimeoutErr 判断是否为超时类错误（含 deadline 超出）
func isTimeoutErr(err error) bool {
	if err == nil {
		return false
	}
	if errors.Is(err, os.ErrDeadlineExceeded) {
		return true
	}
	var ne net.Error
	if errors.As(err, &ne) {
		return ne.Timeout()
	}
	return false
}
