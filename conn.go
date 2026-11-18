// Package vconn 提供带「连接关闭提前通知」能力的 net.Conn 包装。
//
// 设计要点（审计与修复说明见 docs/audit-report-2026-11-15.md）：
//
//   - 监控：每个连接至多一个常驻协程，使用内核级、零字节消耗的探测
//     （unix: poll(2) + recv(MSG_PEEK) / windows: WSAPoll + FIONREAD）
//     判断对端是否关闭；不再"每次 Read 起一个后台协程"，也不再篡改调用方的 ReadDeadline。
//   - 预读：仅在连接空闲且无人读取时进行，且使用自己的超时；
//     兼容 SetBackgroundReadBuffer 的旧语义（>0 预读、<0 丢弃、=0 关闭）。
//   - 并发：前台读由 Conn.fg 串行化，多个 goroutine 并发 Read 不会互相注入伪超时。
//   - 关闭：先关闭底层 fd（立即解除阻塞），再分发通知，永不无限等待。
//
// 所有公开方法均并发安全。
//
// 内部不变量（实现约束，修改实现时必须保持）：
//  1. 任何一次可能阻塞的后台 socket 读，都必须先置位 peekBusy 再写自身 deadline，
//     二者同在 deadlineMu 内完成 —— stopMonitor 的「过去时间 deadline」不会被覆盖；
//  2. 监控实例身份随调用链显式传递（peekStep(stop)），禁止用 c.mon 反查「我是否该退出」；
//  3. 预读缓冲区的「有无数据」只由 peekMu 表达，不使用可能与切片不同步的原子标志；
//  4. 探测结论（监控 / CheckAlive / CheckAliveBlocking）并入同一状态机，
//     保证 Err() / IsPeerClosed() / CloseNotify() 三者一致；
//     其中 probeReadableHangup 以 ErrPeerClosedWithUnread 落定 closeErr
//     （该错误 Unwrap 到 io.EOF，且不得从 Read 返回）。
//  5. 探测阶段读走的 SO_ERROR 会保存到 closeErr，上层 Read 不会把 ECONNRESET 降级为 io.EOF；
//  6. 已经从内核读出的字节只允许追加进预读缓冲区，任何情况下不得裁剪（裁剪即丢数据）。
package vconn

import (
	"context"
	"errors"
	"io"
	"net"
	"sync"
	"sync/atomic"
	"time"
)

// CloseNotifier 提供连接关闭通知能力
type CloseNotifier interface {
	CloseNotify() <-chan error
}

var (
	// ErrPeerClosedWithUnread 对端已正常结束（FIN），但仍有未读数据
	// （可能在内核接收队列，也可能在 vconn 的预读缓冲区里）。
	//
	// 语义提示：这**不是**错误，只是「先读完再收尾」的信号。对端发完数据再关闭
	// 写方向是最常见的正常收尾方式，收到本错误同样意味着「数据可读」。
	//
	// 与 io.EOF 的关系：携带本哨兵的错误同时满足 errors.Is(err, io.EOF)，
	// 因此只判 EOF 的老代码行为完全不变；需要区分时追加一个判定：
	//
	//	switch err := <-c.CloseNotify(); {
	//	case errors.Is(err, vconn.ErrPeerClosedWithUnread):
	//		// 对端结束，但还有数据没读走 → 先 drain 再 Close（否则 close 会发 RST）
	//	case errors.Is(err, io.EOF):
	//		// 队列已读空的干净结束 → 可直接 Close
	//	}
	ErrPeerClosedWithUnread = errors.New("vconn: peer closed with unread data")

	// ErrRawConnAlreadyUsed 表示原始连接已被提取，当前包装连接失效
	ErrRawConnAlreadyUsed = errors.New("vconn: the original connection has already been extracted and cannot be used")

	// ErrNotifyCanceled CancelNotify(nil) 时写入关闭理由的哨兵错误
	ErrNotifyCanceled = errors.New("vconn: close notification canceled by CancelNotify")

	// ErrUnsupported 平台或底层连接不支持该操作
	ErrUnsupported = errors.New("vconn: operation not supported")

	// ErrResetLocallyClosed 连接已被本地关闭（Close / RawConn），不可重置通知状态
	ErrResetLocallyClosed = errors.New("vconn: cannot reset close notification on a locally closed connection")

	// ErrResetPeerDown 已探测到对端断开（或正常/异常结束），默认不允许重置通知状态；
	// 确认要重新武装请使用 ResetNotify(true)
	ErrResetPeerDown = errors.New("vconn: cannot reset close notification after peer was detected down")

	// ErrResetTimeout 上一轮监控协程未能在时限内退出，重置未完成；
	// 状态已清空，可直接重试 ResetNotify（不会重复启动监控）
	ErrResetTimeout = errors.New("vconn: previous monitor goroutine did not exit in time")
)

// 内部时序参数
const (
	// defaultMonitorInterval 空闲连接的探测周期（关闭检测灵敏度/CPU 占用的折中）
	defaultMonitorInterval = 200 * time.Millisecond
	// minMonitorInterval 允许的最小探测周期
	minMonitorInterval = 10 * time.Millisecond
	// peekReadTimeout 监控协程（peekStep）阻塞预读的自身超时上限。
	// 后台路径，宁可略长也不要空转：10ms 与实际探测周期匹配，开销可忽略。
	peekReadTimeout = 10 * time.Millisecond
	// peekTimeout CheckAliveBlocking（peekProbe）阻塞预读的自身超时。
	// 同步路径，调用方在等；但受系统 timer 精度限制，不建议低于 1ms（见下）。
	peekTimeout = 2 * time.Millisecond
	// monitorStopTimeout 等待监控协程退出的上限，绝不无限等待
	monitorStopTimeout = time.Second
	// minPeekBuf 预读/丢弃临时缓冲的最小长度：
	// 需求小于该值时仍按该长度分配，避免频繁的小 syscall
	minPeekBuf = 4096
	// maxPeekSize 预读/丢弃单次缓冲的上限，防止误传大值导致 OOM。
	// 注意：该缓冲随连接常驻（r.peekTmp），丢弃模式的 |n| 会直接决定其大小。
	maxPeekSize = 1 << 20 // 1 MiB
	// peekTmpKeep 预读临时缓冲的保留阈值：
	// 超过该值的缓冲在下次小额读取时直接释放，避免「一次大丢弃模式把 1 MiB
	// 永久钉在每条连接上」。
	peekTmpKeep = 64 << 10 // 64 KiB

	// defaultPeekSize CheckAliveBlocking 单次阻塞探测的默认预读额度。
	// 取 minPeekBuf（4 KiB）：小到不会显著增加常驻内存，大到单次 syscall 不会被截断。
	// 注意：这只是「本次探测的 peek 缓冲上限」，CheckAliveBlocking 单次只读 1 字节，
	// 且不会把该值写回连接（不产生"探活一次就永久开启后台预读"的副作用）。
	defaultPeekSize = minPeekBuf
)

// closeSignal 单个关闭通知订阅者
type closeSignal struct {
	ch   chan error
	once sync.Once
}

// notifyKind 通知来源，决定是否把连接标记为"对端已断开"（peerDown）
type notifyKind uint8

const (
	// kindLocal 本地主动关闭（Close / RawConn）：不代表对端断开
	kindLocal notifyKind = iota
	// kindRemote 对端断开或网络异常（探测/读写错误）
	kindRemote
	// kindManual 用户手动分发（CancelNotify / WithContext 取消）：不代表对端断开
	kindManual
)

func newCloseSignal() *closeSignal { return &closeSignal{ch: make(chan error, 1)} }

// fire 发送错误并关闭 channel，仅生效一次
func (s *closeSignal) fire(err error) {
	s.once.Do(func() {
		select {
		case s.ch <- err:
		default:
		}
		close(s.ch)
	})
}

// discard 仅关闭 channel（退订时使用）
func (s *closeSignal) discard() {
	s.once.Do(func() { close(s.ch) })
}

// Conn 封装了标准的 net.Conn，具备异步断开监控、限流及防死锁提取功能
// 所有公开方法均为并发安全
type Conn struct {
	rwc net.Conn // 底层真实连接（构造后不可变）

	mu        sync.Mutex
	signals   map[*closeSignal]struct{} // 关闭通知订阅者
	closeErr  error                     // 首次关闭原因
	peerDown  bool                      // 是否检测到对端已关闭
	closeRet  error                     // Close 首次返回值（供重复 Close 复用）
	closeDone chan struct{}             // 首个 Close 的完成信号；nil 表示已收尾

	r *connReader // 预读 / 限流 / 前台读串行化

	localAddr  net.Addr // 地址快照，关闭后依然可用
	remoteAddr net.Addr

	// 用户设置的超时时间快照：后台预读结束后按此恢复，避免污染调用方设置。
	// 仅由 deadlineMu 保护（零值表示"无超时"）。
	readDeadline time.Time  // 类型化快照（零值可用，无 atomic.Value 类型断言风险）
	deadlineMu   sync.Mutex // 串行化底层 deadline 的写入（set/restore）

	closed  atomic.Bool // 关闭快速路径标志
	rawUsed atomic.Bool // 原始连接是否已被提取
	// notified notified 的无锁镜像，供探测热路径（isNotified）使用；
	// 写侧仍必须在 c.mu 内先更新 notified，再更新本镜像。
	notified    atomic.Bool   // 是否已分发过关闭通知（可被 ResetNotify 撤销）
	peekSize    atomic.Int64  // 后台预读/丢弃字节数：=0 关闭，>0 预读，<0 丢弃
	peekUserSet atomic.Bool   // 是否被 SetBackgroundReadBuffer 显式设置过（决定 CheckAliveBlocking 能否隐式武装）
	readSeq     atomic.Uint64 // 成功读取计数，用于判定连接是否空闲（比时间戳更廉价）
	monInterval atomic.Int64  // 探测周期（纳秒）

	monMu sync.Mutex                 // 串行化监控实例的创建
	mon   atomic.Pointer[monitorRun] // 当前监控实例（nil 表示未运行）
}

// New 包装原始连接。若已被包装，则直接返回本身。
// 参数 c 不能为 nil，否则会 panic。
func New(c net.Conn) *Conn {
	return NewData(c, nil)
}

// NewData 包装原始连接，并把 b 作为「已到达的预读数据」放入读取缓冲区
// （调用方的 Read 会优先返回这些字节）。
//
// 若 c 已经是 *Conn，则把 b 追加到既有预读缓冲区后返回 c 本身。
// 参数 c 不能为 nil，否则会 panic。
//
// 注意：对本包返回的 *Conn 并发调用 NewData 是安全的（内部用 peekMu 串行化），
// 但不得与 RawConnFull 并发 —— 后者正在做所有权转移，交接期间追加的字节
// 不会出现在返回的预读切片里。
func NewData(c net.Conn, b []byte) *Conn {
	if c == nil {
		panic("vconn: nil conn")
	}
	if vc, ok := c.(*Conn); ok {
		if len(b) > 0 && !vc.closed.Load() {
			vc.r.pushPeek(b)
		}
		return vc
	}

	conn := &Conn{
		rwc:        c,
		signals:    make(map[*closeSignal]struct{}),
		localAddr:  snapshotAddr(c.LocalAddr()),
		remoteAddr: snapshotAddr(c.RemoteAddr()),
	}
	conn.r = newConnReader(conn)
	if len(b) > 0 {
		conn.r.pushPeek(b)
	}
	conn.monInterval.Store(int64(defaultMonitorInterval))
	return conn
}

// snapshotAddr 对常见 net.Addr 做防御性深拷贝，避免底层连接关闭后地址对象被复用/修改。
// 未知类型则原样返回（多数自定义 Addr 已是值语义或不可变）。
func snapshotAddr(a net.Addr) net.Addr {
	if a == nil {
		return nil
	}

	switch addr := a.(type) {
	case *net.TCPAddr:
		if addr == nil {
			return nil
		}
		ip := make(net.IP, len(addr.IP))
		copy(ip, addr.IP)
		return &net.TCPAddr{
			IP:   ip,
			Port: addr.Port,
			Zone: addr.Zone,
		}

	case *net.UDPAddr:
		if addr == nil {
			return nil
		}
		ip := make(net.IP, len(addr.IP))
		copy(ip, addr.IP)
		return &net.UDPAddr{
			IP:   ip,
			Port: addr.Port,
			Zone: addr.Zone,
		}

	case *net.IPAddr:
		if addr == nil {
			return nil
		}
		ip := make(net.IP, len(addr.IP))
		copy(ip, addr.IP)
		return &net.IPAddr{
			IP:   ip,
			Zone: addr.Zone,
		}

	case *net.UnixAddr:
		if addr == nil {
			return nil
		}
		// Name 是 string，已不可变，直接拷贝结构体即可
		return &net.UnixAddr{
			Name: addr.Name,
			Net:  addr.Net,
		}

	default:
		// 未知实现（自定义 Addr、包装类型等）不做假设，直接返回
		return a
	}
}

// ==================== 原始连接提取 ====================

// RawConn 剥离包装，归还底层真实连接
// 剥离后，此 Conn 无法再读写，任何操作将返回 net.ErrClosed
// 该方法只能调用一次，重复调用会 panic
func (c *Conn) RawConn() net.Conn {
	c.mu.Lock()
	if c.rawUsed.Load() {
		c.mu.Unlock()
		panic(ErrRawConnAlreadyUsed)
	}
	c.rawUsed.Store(true)
	if c.closed.Load() {
		// 连接已经开始/完成本地关闭：等待首个关闭者收尾，
		// 避免与 Close 并发操作同一 fd。
		done := c.closeDone
		c.mu.Unlock()
		if done != nil {
			<-done
		}
		// Close 会先 close(done) 再 stopMonitor；这里再确认一次监控已退出，
		// 保证返回 rwc 后不会再有探测协程访问同一 fd。
		c.finishLocalClose()
		return c.rwc
	}
	c.closed.Store(true)
	c.mu.Unlock()
	c.finishLocalClose()
	return c.rwc
}

// finishLocalClose 完成本地关闭的收尾：停监控、分发本地通知。
func (c *Conn) finishLocalClose() {
	c.stopMonitor()
	c.notifyClose(ErrRawConnAlreadyUsed, kindLocal)
}

// RawConnFull 归还原始连接，并返回后台已预读但尚未被读取的字节
// 返回值：(原始连接, 预读字节)。无预读字节时第二返回值为 nil。
//
// 实现上先停监控，再用「过去时间」的 ReadDeadline 唤醒可能阻塞中的前台 Read，
// 取得前台读锁（fg）后恢复调用方设置的最新 deadline，取走预读数据并在仍持有 fg
// 时完成 RawConn 的所有权转移。这样不会出现「预读仍在写已放弃缓冲区」的丢数据
// 竞态，也不会出现「RawConn 已返回、探测协程仍继续操作同一 fd」的 TOCTOU。
//
// 注意：若调用本方法时有其它 goroutine 正阻塞在 Read 上，该 Read 会被临时 deadline
// 唤醒并返回一个超时错误；取得 fg 后会恢复调用方设置的 deadline。因此调用方应把
// RawConnFull 视为「连接所有权即将转移」，不要再让其它 goroutine 并发使用该连接，
// 也不要再并发调用 NewData。
// 对不支持 SetReadDeadline 的底层连接，本方法无法主动唤醒前台 Read。
func (c *Conn) RawConnFull() (net.Conn, []byte) {
	// 1. 先停监控：此后不会再有新的 peekStep/peekProbe 启动。
	c.stopMonitor()

	// 2. 催促可能阻塞中的前台 Read 退出（预读也可能持有 fg，stopMonitor 已处理）。
	//    连接不支持 deadline 时无法中断，退化为等待调用方不再并发 Read。
	c.deadlineMu.Lock()
	c.rwc.SetReadDeadline(time.Unix(0, 1))
	c.deadlineMu.Unlock()

	// 3. 等所有在途读取结束，然后恢复调用方最新设置的 ReadDeadline。
	c.r.fg.Lock()
	c.deadlineMu.Lock()
	c.rwc.SetReadDeadline(c.readDeadline)
	c.deadlineMu.Unlock()

	data := c.r.takePeek()
	// 4. 仍持有 fg 时完成所有权转移：此后 Read/CheckAliveBlocking 不会再碰预读缓冲区。
	raw := c.RawConn()
	c.r.fg.Unlock()

	return raw, data
}

// ==================== 配置 ====================

// SetBackgroundReadBuffer 设置后台预读字节数
//
//	n > 0：预读最多 n 字节缓存，供 Read 返回
//	n = 0：关闭后台预读（默认），仅保留内核级关闭探测
//	n < 0：丢弃模式，后台读取到的数据直接丢弃，仅用于探测关闭（会造成数据丢失）
//
// 注意：对 net.Pipe、自定义包装等不支持内核探测的连接，n = 0 时没有可观察的
// 就绪事件，因而检测不到对端关闭；需要显式设置 n > 0，退化为带短超时的阻塞预读。
//
// |n| 同时决定「单次后台读的缓冲长度」：不足 minPeekBuf 时按 minPeekBuf 分配，
// 否则按 |n| 分配，因此丢弃模式下一次就能吞掉 |n| 字节，不会被 4 KiB 隐式限速。
// 该缓冲随连接常驻且不会自动缩小，传入大负值（如 -1 MiB）请自行评估内存占用。
//
// 为安全起见，|n| 超过 maxPeekSize 时按 maxPeekSize 截断。
//
// 调用本方法后，CheckAliveBlocking 不再隐式武装预读（完全尊重用户设置，
// 包括显式设为 0 关闭）。
func (c *Conn) SetBackgroundReadBuffer(n int) {
	if n > maxPeekSize {
		n = maxPeekSize
	}
	if n < -maxPeekSize {
		n = -maxPeekSize
	}
	c.peekSize.Store(int64(n))
	c.peekUserSet.Store(true) // 用户显式表态：无论 n 是 0 还是非 0，都尊重
}

// SetReadLimit 设置读取限制字节数
// remain < 0: 不限制
// remain >= 0: 限制剩余可读字节数，超出后返回 io.EOF
func (c *Conn) SetReadLimit(remain int64) {
	c.r.setReadLimit(remain)
}

// SetMonitorInterval 设置空闲连接的关闭探测周期，默认 200ms
// d <= 0 时恢复默认值
func (c *Conn) SetMonitorInterval(d time.Duration) {
	if d <= 0 {
		d = defaultMonitorInterval
	}
	if d < minMonitorInterval {
		d = minMonitorInterval
	}
	c.monInterval.Store(int64(d))
	c.wakeMonitor()
}

// ==================== 关闭通知 ====================

// CloseNotify 获取关闭通知 Channel
// 支持多次调用；在对端意外断开或本地主动 Close 时均会触发通知
// 返回的 channel 会在连接关闭时接收到错误并关闭
//
// 注意：本方法不提供退订手段，订阅者会一直保留到连接关闭。
// 需要退订请使用 WatchClose；需要「本次订阅没等到通知就想撤销」也请用 WatchClose。
func (c *Conn) CloseNotify() <-chan error {
	ch, _ := c.WatchClose()
	return ch
}

// WatchClose 获取关闭通知 Channel 以及退订函数
// 退订后 channel 会被关闭（不携带错误），保证等待方能够退出，不会泄漏。
//
// 可反复订阅/退订：
//   - 未落定通知时退订只移除订阅者，不影响其它订阅者；
//   - 通知已落定（channel 已关闭）后再次订阅，会立即得到一个已完成的 channel，
//     内容为当时的关闭原因；
//   - 若希望"同一连接再次收到下一轮关闭通知"，先调用 ResetNotify 清除落定状态，
//     再重新订阅：旧 channel 保持已关闭（不会被二次投递），新订阅进入新一轮。
func (c *Conn) WatchClose() (<-chan error, func()) {
	sig := newCloseSignal()

	c.mu.Lock()
	if c.notified.Load() || c.rawUsed.Load() {
		err := c.closeErr
		// 提取原始连接后 closeErr 可能尚未写入（RawConn 与 WatchClose 并发）：
		// 这里补一个确定的原因，避免把 nil 交给订阅者。
		if err == nil && c.rawUsed.Load() {
			err = ErrRawConnAlreadyUsed
		}
		c.mu.Unlock()
		sig.fire(err)
		return sig.ch, func() {}
	}
	c.signals[sig] = struct{}{}
	c.mu.Unlock()

	// 启动监控以检测连接状态
	c.startMonitor()

	var once sync.Once
	cancel := func() {
		once.Do(func() {
			c.mu.Lock()
			if _, ok := c.signals[sig]; ok {
				delete(c.signals, sig)
				c.mu.Unlock()
				sig.discard()
				return
			}
			c.mu.Unlock()
			sig.discard()
		})
	}
	return sig.ch, cancel
}

// CancelNotify 主动分发一次关闭通知，参数 err 将作为通知内容发送给所有等待者
// 返回值表示本次调用是否真正触发了关闭通知（首次返回 true）。
//
// err 为 nil 时使用 ErrNotifyCanceled 作为原因，避免出现"收到 nil 错误"的不确定语义。
//
// 本方法只分发通知，不关闭连接；通知落定后监控协程会停止。
// 若需要"分发一次 → 重置 → 再分发一次"（反复订阅场景），配合 ResetNotify(false) 使用。
//
// 注意：本方法是「上锁/发射」，不是「撤销」。已经发出的通知无法收回
// （channel 已关闭、订阅者已被唤醒），要清空锁存进入下一轮请用 ResetNotify。
func (c *Conn) CancelNotify(err error) bool {
	if err == nil {
		err = ErrNotifyCanceled
	}
	return c.notifyClose(err, kindManual)
}

// ResetNotify 清除已落定的关闭通知状态，使连接可以再次被订阅并再次收到通知。
//
// ResetNotify(false) 是常规重置，保留两条安全边界：
//   - 本地已 Close / RawConn 提取过（底层不可再用）→ ErrResetLocallyClosed
//   - 已探测到对端断开（需确认连接仍然可用）→ ErrResetPeerDown
//
// ResetNotify(true) 是「重新武装」：越过第二条边界（peerDown）。语义上清空
// closeErr/peerDown 后，新的监控会立刻重新探测到底层仍是断开状态，并再次分发通知。
// 只应在"确认底层连接已被替换/接管，或需要把已断开状态重新上报一次"的场景使用。
// 第一条边界（本地已关闭）任何参数都不可越过。
//
// 返回 nil 表示重置完成（无论此前是否落定过）。
//
// 重置会做三件事：清空 notified/closeErr/peerDown；保留已有订阅者
// （它们收到的是上一轮通知，channel 已关闭，不会被重新投递）；在仍有订阅者时重启监控协程。
// 因此典型用法是「ResetNotify(false) → WatchClose()」重新进入下一轮，而不是复用旧 channel。
//
// 若重置前它本来就处于"未落定"状态，本方法是幂等的：只补一次 startMonitor
// （已有存活实例时内部直接返回），不会打断正在工作的监控，也不会做无谓的 stop/start。
//
// 与其它方法的并发关系：可与 WatchClose/CancelNotify/Close 并发调用；
// 与 Close 竞争时以最终状态为准——本地已关闭后重置会返回 ErrResetLocallyClosed。
func (c *Conn) ResetNotify(force bool) error {
	c.mu.Lock()
	if c.closed.Load() || c.rawUsed.Load() {
		c.mu.Unlock()
		return ErrResetLocallyClosed
	}
	if c.peerDown && !force {
		c.mu.Unlock()
		return ErrResetPeerDown
	}
	if !c.notified.Load() && c.closeErr == nil && !c.peerDown {
		// 本来就是"未落定"状态：幂等返回，不动正在工作的监控。
		needMonitor := len(c.signals) > 0
		c.mu.Unlock()
		if needMonitor {
			c.startMonitor() // 已有存活实例时内部直接返回
		}
		return nil
	}

	c.notified.Store(false)
	c.closeErr = nil
	c.peerDown = false
	// 仅当仍有订阅者时才需要重新武装监控；无人等待就不额外烧系统调用
	needMonitor := len(c.signals) > 0
	c.mu.Unlock()

	if !needMonitor {
		return nil
	}

	// 先等上一轮监控收尾，再起新一轮：
	// 两代监控同时存活时，阻塞预读可能互相覆盖 ReadDeadline。
	if !c.stopMonitor() {
		// 状态已清空，监控会在底层读超时后自行退出；调用方可直接重试
		return ErrResetTimeout
	}
	c.startMonitor()
	return nil
}

// WithContext 将连接与上下文关联，在上下文取消时触发 CloseNotify 通知。
// 本方法保持旧版语义：只通知，不关闭底层连接（阻塞中的 Read 不会被打断）。
// 需要「取消即断开」请使用 WithContextClose。
func (c *Conn) WithContext(ctx context.Context) {
	if ctx == nil {
		return
	}
	ch, cancel := c.WatchClose()
	go func() {
		select {
		case <-ch:
			cancel()
		case <-ctx.Done():
			cancel()
			// 自定义 Context 可能违反契约（Done 已关闭但 Err() 返回 nil），
			// 这里做一次规整，避免把 nil 交给订阅者
			err := ctx.Err()
			if err == nil {
				err = ErrNotifyCanceled
			}
			c.notifyClose(err, kindManual)
		}
	}()
}

// WithContextClose 将连接与上下文关联：上下文取消时关闭连接。
// 关闭会立即解除正在阻塞的 Read/Write，并把通知分发给所有订阅者。
func (c *Conn) WithContextClose(ctx context.Context) {
	if ctx == nil {
		return
	}
	ch, cancel := c.WatchClose()
	go func() {
		select {
		case <-ch:
			cancel()
		case <-ctx.Done():
			cancel()
			c.Close()
		}
	}()
}

// notifyRemote 分发「远端/异常」类关闭通知；本地已 Close 时不再重复通知
func (c *Conn) notifyRemote(err error) bool {
	if c.closed.Load() {
		return false
	}
	return c.notifyClose(err, kindRemote)
}

// notifyClose 分发关闭通知，只有首次调用生效（重置后可再次生效）
// 返回 true 表示本次调用完成了状态迁移。
//
// kind 决定是否把连接标记为 peerDown：
//   - kindRemote：对端断开/网络异常 → peerDown=true；
//   - kindLocal / kindManual：只落定通知，不改写"对端是否断开"的事实。
//
// 本方法只分发通知，不关闭底层连接：关闭始终是调用方的责任（保持 net.Conn
// 的半关闭语义，对端 FIN 后仍可写入响应）。需要「通知即关闭」时由调用方写
//
//	go func() { <-c.CloseNotify(); c.Close() }()
//
// 约定：调用方保证 err 非 nil（CancelNotify 会把 nil 规整为 ErrNotifyCanceled）。
// 本函数不做 nil 兜底：一旦传入 nil，订阅者与 Err() 就会拿到 nil，
// 而"已落定 + 原因为空"与"尚未落定"将只能靠 notified 区分。
func (c *Conn) notifyClose(err error, kind notifyKind) bool {
	c.mu.Lock()
	if c.notified.Load() {
		c.mu.Unlock()
		return false
	}
	if kind == kindRemote && c.closed.Load() {
		// 本地已经关闭（Close/RawConn 已生效）：远端探测结果不再有语义，
		// 避免把「本地主动关闭」误记成 peerDown。
		c.mu.Unlock()
		return false
	}
	c.notified.Store(true) // 无锁镜像，供探测热路径读取
	c.closeErr = err
	if kind == kindRemote {
		c.peerDown = true
	}
	sigs := c.signals
	c.signals = make(map[*closeSignal]struct{})
	c.mu.Unlock()

	// 先停监控信号（非阻塞），再在锁外分发，避免死锁
	c.signalMonitorStop()

	for s := range sigs {
		s.fire(err)
	}
	return true
}

// IsPeerClosed 返回是否探测到对端已关闭或连接异常
func (c *Conn) IsPeerClosed() bool {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.peerDown
}

// Err 返回连接的关闭原因（未关闭时返回 nil）
func (c *Conn) Err() error {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.closeErr
}

// terminalReadErr 把底层终止错误规整为连接级结论（仅供 connReader 调用）。
//
// 背景：getsockopt(SO_ERROR) 会清除待处理错误。探测路径（SO_ERROR /
// poll + MSG_PEEK）为了尽早发现断开，可能先把 ECONNRESET 读走；此时前台 Read
// 只会拿到一个"干净"的 io.EOF，上层就无法区分「对端正常关闭」与「连接被重置」。
//
// 规则：
//   - 对端已被判定断开（peerDown）且保存了非 EOF 的真实原因 → 返回该原因；
//   - 对端正常关闭（peerDown + io.EOF）且底层报 net.ErrClosed → 统一返回 io.EOF，
//     避免上层把"正常结束"误判为异常；
//   - 其它情况返回 nil，表示保持底层错误原样（超时等控制流错误绝不改写）。
func (c *Conn) terminalReadErr(err error) error {
	// 只处理「流结束」类错误；超时、EAGAIN 等一律不改写。
	if !errors.Is(err, io.EOF) && !errors.Is(err, net.ErrClosed) {
		return nil
	}
	c.mu.Lock()
	peerDown, cerr := c.peerDown, c.closeErr
	c.mu.Unlock()

	if !peerDown {
		return nil
	}
	if cerr != nil && !errors.Is(cerr, io.EOF) {
		return cerr
	}
	if errors.Is(err, net.ErrClosed) {
		return io.EOF
	}
	return nil
}

// recordProbe 把探测结论并入状态机：落定 peerDown / closeErr 并分发通知。
// 返回规整后的关闭原因（保证非 nil）。
//
// probeClosed 与 probeReadableHangup 在「是否要通知」上一致，
// 但通知内容不同：后者带 ErrPeerClosedWithUnread（同时满足 io.EOF），
// 供调用方区分「队列已读空」与「还有数据没读完」。
//
// 本地已关闭（closed）时本方法是空操作（由 notifyClose 内部拦截）。
func (c *Conn) recordProbe(err error) error {
	if err == nil {
		err = io.EOF
	}
	c.notifyClose(err, kindRemote)
	return err
}

// ==================== 读写 ====================

// Read 从连接读取数据
// 实现 io.Reader 接口，线程安全
//
// 注意：本方法不用 closedA 做短路。预读缓冲区里可能还存着已经到达的数据，
// 必须交由 connReader.Read 先交付；只有缓冲区为空且连接已关闭时才报
// net.ErrClosed。关闭后的 Read 不是热路径，这点开销可以接受。
func (c *Conn) Read(b []byte) (n int, err error) {
	n, err = c.r.Read(b)
	if n > 0 {
		return n, nil // 有数据时优先返回数据，错误留到下一次（io.Reader 惯例）
	}
	if err != nil {
		if isCommonNetError(err) {
			c.notifyRemote(err)
		}
	}
	return n, err
}

// Write 向连接写入数据
// 实现 io.Writer 接口，线程安全
func (c *Conn) Write(b []byte) (n int, err error) {
	if c.closed.Load() {
		return 0, net.ErrClosed
	}

	n, err = c.rwc.Write(b)
	// 如果发生网络错误，通知所有等待者
	if err != nil && n == 0 && isCommonNetError(err) {
		c.notifyRemote(err)
	}
	return
}

// waitCloseDone 等待首个本地关闭者收尾并返回其返回值。
// done 为 nil 表示已经收尾完毕，直接读结果。
func (c *Conn) waitCloseDone(done chan struct{}) error {
	if done != nil {
		<-done
	}
	c.mu.Lock()
	err := c.closeRet
	c.mu.Unlock()
	return err
}

// Close 主动关闭连接，干净清理所有通知管道
// 实现 io.Closer 接口，线程安全；重复调用返回首次关闭的错误
//
// 并发调用时，后到者会等待首个 Close 完成，从而拿到与之完全相同的结果
// （不会出现"首个 Close 返回错误、并发的第二个 Close 返回 nil"）。
func (c *Conn) Close() error {
	c.mu.Lock()
	if c.closed.Load() {
		done := c.closeDone
		c.mu.Unlock()
		// 补一次有上限的 stopMonitor，保证 Close 返回时探测协程已结束。
		c.stopMonitor()
		return c.waitCloseDone(done)
	}
	c.closed.Store(true)
	done := make(chan struct{})
	c.closeDone = done
	c.mu.Unlock()

	// 先关闭底层连接：立即解除所有阻塞中的 Read/Write
	var err error
	if !c.rawUsed.Load() {
		err = c.rwc.Close()
	}

	c.mu.Lock()
	c.closeRet = err
	c.closeDone = nil
	c.mu.Unlock()
	close(done)

	c.stopMonitor()
	c.notifyClose(net.ErrClosed, kindLocal)
	return err
}

// ==================== net.Conn 接口实现 ====================

// LocalAddr 返回本地网络地址（关闭后依然返回构造时的地址快照）
func (c *Conn) LocalAddr() net.Addr {
	return c.localAddr
}

// RemoteAddr 返回远程网络地址（关闭后依然返回构造时的地址快照）
func (c *Conn) RemoteAddr() net.Addr {
	return c.remoteAddr
}

// SetDeadline 同时设置读写超时时间
func (c *Conn) SetDeadline(t time.Time) error {
	if c.closed.Load() {
		return net.ErrClosed
	}

	c.deadlineMu.Lock()
	c.readDeadline = t
	err := c.rwc.SetDeadline(t)
	c.deadlineMu.Unlock()
	return err
}

// SetReadDeadline 设置读取超时时间
func (c *Conn) SetReadDeadline(t time.Time) error {
	if c.closed.Load() {
		return net.ErrClosed
	}

	c.deadlineMu.Lock()
	c.readDeadline = t
	err := c.rwc.SetReadDeadline(t)
	c.deadlineMu.Unlock()
	return err
}

// SetWriteDeadline 设置写入超时时间
func (c *Conn) SetWriteDeadline(t time.Time) error {
	if c.closed.Load() {
		return net.ErrClosed
	}
	return c.rwc.SetWriteDeadline(t)
}

// SetReadBuffer 设置操作系统接收缓冲区大小（仅 TCP/Unix 等支持该选项的连接有效）
func (c *Conn) SetReadBuffer(bytes int) error {
	if c.closed.Load() {
		return net.ErrClosed
	}
	if s, ok := c.rwc.(interface{ SetReadBuffer(int) error }); ok {
		return s.SetReadBuffer(bytes)
	}
	return ErrUnsupported
}

// SetWriteBuffer 设置操作系统发送缓冲区大小（仅 TCP/Unix 等支持该选项的连接有效）
func (c *Conn) SetWriteBuffer(bytes int) error {
	if c.closed.Load() {
		return net.ErrClosed
	}
	if s, ok := c.rwc.(interface{ SetWriteBuffer(int) error }); ok {
		return s.SetWriteBuffer(bytes)
	}
	return ErrUnsupported
}

// SetKeepAlive 设置是否开启 TCP keepalive（仅底层支持该选项时有效）
func (c *Conn) SetKeepAlive(keepalive bool) error {
	if c.closed.Load() {
		return net.ErrClosed
	}
	if s, ok := c.rwc.(interface{ SetKeepAlive(bool) error }); ok {
		return s.SetKeepAlive(keepalive)
	}
	return ErrUnsupported
}

// SetKeepAlivePeriod 设置 TCP keepalive 周期（仅底层支持该选项时有效）
func (c *Conn) SetKeepAlivePeriod(d time.Duration) error {
	if c.closed.Load() {
		return net.ErrClosed
	}
	if s, ok := c.rwc.(interface{ SetKeepAlivePeriod(time.Duration) error }); ok {
		return s.SetKeepAlivePeriod(d)
	}
	return ErrUnsupported
}

// SetLinger 设置关闭时的 linger 行为（仅底层支持该选项时有效）
// sec < 0：使用系统默认；sec == 0：立即关闭；sec > 0：最多等待 sec 秒。
func (c *Conn) SetLinger(sec int) error {
	if c.closed.Load() {
		return net.ErrClosed
	}
	if s, ok := c.rwc.(interface{ SetLinger(int) error }); ok {
		return s.SetLinger(sec)
	}
	return ErrUnsupported
}

// CloseWrite 关闭写方向（半关闭），仅底层支持 CloseWrite 时有效
func (c *Conn) CloseWrite() error {
	if c.closed.Load() {
		return net.ErrClosed
	}
	if s, ok := c.rwc.(interface{ CloseWrite() error }); ok {
		return s.CloseWrite()
	}
	return ErrUnsupported
}

// CloseRead 关闭读方向（半关闭），仅底层支持 CloseRead 时有效
func (c *Conn) CloseRead() error {
	if c.closed.Load() {
		return net.ErrClosed
	}
	if s, ok := c.rwc.(interface{ CloseRead() error }); ok {
		return s.CloseRead()
	}
	return ErrUnsupported
}

// Stats 返回连接的可观测计数快照（用于线上排障，不影响连接状态）。
type Stats struct {
	ReadSeq       uint64 // 成功读取次数（前台 + 底层）
	ProbeCount    uint64 // 内核级探测调用次数
	PeekBytes     uint64 // 后台预读/探活从 socket 读出的字节数（含丢弃模式）
	MonitorRounds uint64 // 监控探测轮次
	Notified      bool   // 是否已分发关闭通知
	PeerDown      bool   // 是否已探测到对端断开
	Closed        bool   // 是否本地已关闭/提取
}

// isCleanPeerClose 判断探测给出的关闭原因是否为「干净的 FIN」。
// 只有干净 FIN 才允许升级为 ErrPeerClosedWithUnread；
// ECONNRESET/EPIPE 等真实错误必须原样保留，否则 terminalReadErr
// 会把异常断开降级成 io.EOF（见包文档不变量第 5 条）。
func isCleanPeerClose(err error) bool {
	return err == nil || errors.Is(err, io.EOF)
}

// probePeekAware 在内核探测之上，把「内核队列已空、但 vconn 自己的预读缓冲
// 里还有未交付字节」的 probeClosed 归一为 probeReadableHangup。
//
// 两个前提缺一不可：
//  1. res == probeClosed（确认是关闭，不是 unsupported/idle）；
//  2. err 是干净 FIN —— 否则真实断开原因会被 hangup 语义覆盖。
func (c *Conn) probePeekAware() (probeResult, error) {
	res, err := probeConn(c.rwc)
	if res == probeClosed && isCleanPeerClose(err) && c.r.hasPeek() {
		return probeReadableHangup, peerClosedWithUnreadError{}
	}
	return res, err
}

// ==================== 健康检查 ====================

// checkKernel 执行内核级健康检查，供 CheckAlive/CheckAliveBlocking 共用。
//
// 返回值：
//   - (probeClosed, err):         已确认断开；err 为断开原因（干净 FIN 时为 io.EOF，异常时为具体错误）
//   - (probeIdle, nil):           已确认存活且当前无数据
//   - (probeReadable, nil):       已确认存活且有数据可读
//   - (probeReadableHangup, err): 有未读数据且对端已 FIN；err 为 ErrPeerClosedWithUnread
//     ErrPeerClosedWithUnread（Unwrap 到 io.EOF）。数据仍可读，按「存活」上报。
//   - (probeUnsupported, nil):    内核探测不可用，由调用方决定兜底策略
//
// 检查顺序：本地关闭状态 → SO_ERROR → poll/WSAPoll 探测。
//
// socketError 返回 ErrUnsupported 时直接返回 probeUnsupported：
// socketError 与 probeConn 都通过 syscallConnOf 解包 fd，前者拿不到 fd 时
// 后者必然也拿不到，不必再浪费一次 probeConn 调用（原 CheckAliveBlocking
// 在这里会多调一次，属于可省的冗余）。
func (c *Conn) checkKernel() (probeResult, error) {
	if c.closed.Load() {
		return probeClosed, net.ErrClosed
	}

	// 1. 内核 socket 待处理错误（SO_ERROR，能拿到 fd 时最快路径）
	if err := socketError(c.rwc); err != nil {
		if errors.Is(err, ErrUnsupported) {
			return probeUnsupported, nil
		}
		return probeClosed, err
	}

	// 2. 对端 FIN/RST 探测（unix: poll(2)；windows: WSAPoll(0) + FIONREAD
	res, perr := c.probePeekAware()
	if res == probeClosed {
		return probeClosed, perr
	}

	// Close 可能与探测并发：fd 被关闭后 probeConn 会返回 probeUnsupported。
	// 此时必须把「本地已关闭」如实上报，不能 fail-open 成存活。
	if res == probeUnsupported && c.closed.Load() {
		return probeClosed, net.ErrClosed
	}
	return res, perr
}

// CheckAlive 主动探测连接是否存活（非阻塞，不消耗任何数据）
//
// 依次检查：本地关闭状态 → 内核 socket 待处理错误（SO_ERROR）→ 对端 FIN 探测。
// 返回 nil 表示连接仍然可用（内核探测不可用时 fail-open 返回 nil）。
//
// 状态一致性：探测结论会并入连接状态机 —— peerDown / closeErr / CloseNotify
// 与返回值保持一致，不再出现「CheckAlive 报错但 IsPeerClosed() 为 false、
// 订阅者也收不到通知」的情况。
//
// 对 probeReadableHangup（对端已 FIN 但仍有未读数据）的处理与监控协程完全一致：
// 落定 peerDown 并分发通知，但仍按「存活」返回 nil（数据可读）。
//
// 副作用边界：本方法不消耗任何数据，也不会丢数据，且**不会**关闭底层连接
// （关闭始终是调用方的责任）。需要纯查询语义请改用 Stats().PeerDown。
func (c *Conn) CheckAlive() error {
	res, err := c.checkKernel()
	switch res {
	case probeClosed:
		return c.recordProbe(err)
	case probeReadableHangup:
		// 与 monitorPass 同结论：只通知，不关闭底层（数据还没读空）。
		c.recordProbe(err)
		return nil
	}
	return nil
}

// CheckAliveBlocking 在 CheckAlive 基础上，当内核探测不可用
// （probeUnsupported：net.Pipe、未知 wrapper 等拿不到 fd 的连接）时，
// 退化为一次带 peekProbeTimeout（2ms）超时的阻塞预读探测。
//
// 预读额度：若用户从未调用过 SetBackgroundReadBuffer，本次探测使用
// defaultPeekSize（4 KiB）作为「本次」缓冲上限，**不会写回连接状态**，
// 因此一次探活不会把后台预读永久打开（无副作用）。
// 用户之后可用 SetBackgroundReadBuffer 扩大/缩小/关闭：
//   - 设置正值：后续 CheckAliveBlocking 使用该值；
//   - 设置 0：显式关闭预读，CheckAliveBlocking 退化为 fail-open 返回 nil；
//   - 设置负值（丢弃模式）：视为「探活不得吞数据」，同样 fail-open 返回 nil。
//
// 单次探测只读 1 字节（见 peekProbe），因此可高频重复调用：
//   - 前台 Read 正常消费 → peek 长期接近空；
//   - 前台不消费 → peek 最多累积到额度上限，之后探测直接判活返回 nil，
//     不报错、不阻塞、不再读（"有未读数据"本身就是连接活着的证据）。
//
// 数据安全：预读到的字节进入 peek 缓冲区，前台 Read 会优先消费，不会丢失。
// 同步性：最坏等待 peekProbeTimeout（2ms）；若前台正持有 fg 锁（正在读），
// 本次探测直接放弃并视为存活（返回 nil），因为前台读本身是连接活着的证据。
//
// 状态一致性、副作用边界：与 CheckAlive 完全相同。
func (c *Conn) CheckAliveBlocking() error {
	res, err := c.checkKernel()
	switch res {
	case probeClosed:
		return c.recordProbe(err)
	case probeReadableHangup:
		c.recordProbe(err)
		return nil
	case probeUnsupported:
		// 内核探测不可用：继续走阻塞预读兜底
	default: // probeIdle / probeReadable
		return nil
	}

	// 内核探测不可用：用本次探测的额度走阻塞预读（不修改连接状态）
	size := c.peekProbeBudget()
	if size == 0 {
		return nil // 用户显式关闭了预读（或处于丢弃模式），fail-open
	}
	closed, cerr := c.r.peekProbe(probeUnsupported, true, size)
	if closed {
		return c.recordProbe(cerr)
	}
	return nil
}

// peekProbeBudget 返回 CheckAliveBlocking 本次探测可用的预读额度。
//
// 用户显式设置过（含显式 0）→ 完全尊重；否则仅在本次探测使用 defaultPeekSize，
// 不写回 peekSize —— 避免「一次探活就永久开启后台预读」的隐式副作用。
//
// 丢弃模式（负值）返回 0：探活是"查询"而非"消费"，
// 不能因为检测存活就吞掉用户尚未读取的字节。
func (c *Conn) peekProbeBudget() int {
	if c.peekUserSet.Load() {
		v := int(c.peekSize.Load())
		if v < 0 {
			return 0 // 丢弃模式：探活不得吞数据
		}
		return v
	}
	return defaultPeekSize
}

// ReaderFrom 未实现：预读缓冲区语义与 io.ReaderFrom 不兼容，故不提供该方法，
// 以免误用导致 bufio 等库把「已预读字节」丢失。io.Copy 会退化为普通缓冲复制。

var _ io.Closer = (*Conn)(nil)
