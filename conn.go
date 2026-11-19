// Package vconn 为任意 net.Conn 提供「连接关闭提前通知」与「连接状态判定」能力。
//
// # 它解决什么问题
//
// 标准 net.Conn 只能通过 Read 返回 io.EOF 才知道对端已经关闭，这带来三个麻烦：
//
//   - 对端「先发数据再 FIN」时，调用方必须把接收缓冲读空才知道连接已经结束，
//     这段时间里无法区分「还在等下一段数据」与「对方已经走了」；
//   - 纯写入场景（上传、推送、请求已发出等响应）永远等不到 EOF，
//     只能靠超时或业务心跳兜底；
//   - 连接池回收时无法安全判断「这条连接还干净吗」。
//
// vconn 在底层 socket 上做零字节消耗的内核级探测，对端一断开就能立刻通知，
// 并且明确区分「干净结束」「异常重置」「还有未读数据」三种状态。
//
// # 核心设计
//
//   - 监控：每条连接至多一个常驻协程，用内核级、零字节消耗的探测
//     （unix: poll(2) + recv(MSG_PEEK) / windows: WSAPoll + FIONREAD）
//     判断对端是否关闭；不做「每次 Read 起一个后台协程」，也不篡改调用方设置的
//     ReadDeadline。
//   - 预读：仅在连接空闲且无人读取时进行，且使用自己的超时；
//     SetBackgroundReadBuffer 的语义是 n>0 预读、n<0 丢弃、n=0 关闭。
//   - 并发：前台读由 Conn.r.fg 串行化，多个 goroutine 并发 Read
//     不会互相注入伪超时。
//   - 关闭：先关闭底层 fd（立即解除阻塞），再分发通知，永不无限等待。
//
// 所有公开方法均并发安全。
//
// # 状态与错误分类
//
// Status 是唯一的「连接结论」类型，一个字节同时表达「类别」与「是否还有未读数据」。
// 探测与错误共用同一套常量，调用方只需要学一个枚举：
//
//   - Check / CheckBlocking 返回 (Status, error)，CheckReusable 直接给出池化判定，
//     Snapshot() 给出不探测的纯快照（永不会宣称「存活」），
//     StatusOf(err) 把任意错误归到同一个 Status（Read / Write / CloseNotify / Check*
//     的错误空间共用同一套答案，一个 switch 就够）。
//   - 观测轴与处置轴共用类型但问题不同：Readable / Reusable / Ended 讲「连接怎么样」，
//     Retryable / Terminal 讲「我该怎么做」。
//
// 类型与常量的定义在 state.go，「错误 → Status」的映射在 kind.go。
//
// # 内部不变量（实现约束，修改实现时必须保持）
//
//  1. 任何一次可能阻塞的后台 socket 读，都必须先置位 peekBusy 再写自身 deadline，
//     二者同在 deadlineMu 内完成 —— stopMonitor 的「过去时间 deadline」不会被覆盖；
//  2. 监控实例身份随调用链显式传递（peekStep(stop)），禁止用 c.mon 反查「我是否该退出」；
//  3. 预读缓冲区的「有无数据」只由 peekMu 表达，不使用可能与切片不同步的原子标志；
//  4. 探测结论（监控 / Check / CheckBlocking / CheckReusable）并入同一状态机，
//     保证 Err() / IsPeerClosed() / CloseNotify() 三者一致；
//     其中 probeReadableHangup 以 ErrPeerClosedWithUnread 落定 closeErr
//     （该错误 Unwrap 到 io.EOF，且不得从 Read 返回）。
//     探测只有一份实现（Conn.probe），其它方法都是它的视图；
//  5. 探测阶段读走的 SO_ERROR 会保存到 closeErr，上层 Read 不会把 ECONNRESET 降级为 io.EOF；
//     带 cause 的 ErrPeerClosedWithUnread 同理（见 terminalReadErr）；
//  6. 已经从内核读出的字节只允许追加进预读缓冲区，任何情况下不得裁剪（裁剪即丢数据）；
//  7. 「干净 FIN」的判定只有一处（isCleanPeerClose，建立在 StatusOf 之上）：
//     探测升级、状态归类、通知内容都依赖它。
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

// CloseNotifier 提供连接关闭通知能力。
//
// 任何实现了 CloseNotify() <-chan error 的类型都可以被本包的辅助逻辑
// 当作「可订阅关闭通知的连接」使用；*Conn 天然满足该接口。
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
	// 因此只判 EOF 的代码行为完全不变；需要区分时追加一个判定：
	//
	//	switch err := <-c.CloseNotify(); {
	//	case errors.Is(err, vconn.ErrPeerClosedWithUnread):
	//		// 对端结束，但还有数据没读走 → 先 drain 再 Close（否则 close 会发 RST）
	//	case errors.Is(err, io.EOF):
	//		// 队列已读空的干净结束 → 可直接 Close
	//	}
	//
	// 对应的状态类别：StatusPeerClosed（带 unread 修饰时为 StatusPeerClosedWithUnread）。
	ErrPeerClosedWithUnread = errors.New("vconn: peer closed with unread data")

	// ErrRawConnAlreadyUsed 表示原始连接已被提取，当前包装连接失效。
	//
	// 触发场景：
	//   - 重复调用 RawConn / RawConnFull（这会 panic，不是返回本错误）；
	//   - RawConn 提取所有权后，原包装连接的分发通知以本错误作为关闭原因。
	//
	// 对应的状态类别：StatusMisuse。
	ErrRawConnAlreadyUsed = errors.New("vconn: the original connection has already been extracted and cannot be used")

	// ErrNotifyCanceled CancelNotify(nil) 时写入关闭理由的哨兵错误。
	//
	// CancelNotify 传入 nil 时用它兜底，避免把「收到 nil 错误」这种不确定语义
	// 交给订阅者。也用于 WithContext 取消时规整自定义 Context 违反契约的情况。
	//
	// 对应的状态类别：StatusLatched（表示「这一轮等待被撤销」，连接本身未必坏）。
	ErrNotifyCanceled = errors.New("vconn: close notification canceled by CancelNotify")

	// ErrUnsupported 平台或底层连接不支持该操作。
	//
	// 例如对 net.Pipe 调用 SetKeepAlive / SetLinger / CloseWrite 等可选方法，
	// 或在拿不到 fd 的连接上做内核级探测。
	//
	// 对应的状态类别：StatusUnsupported。
	ErrUnsupported = errors.New("vconn: operation not supported")

	// ErrResetLocallyClosed 连接已被本地关闭（Close / RawConn），不可重置通知状态。
	//
	// 这是 ResetNotify 不可逾越的边界：底层连接已经不可用，重新武装没有意义。
	//
	// 对应的状态类别：StatusLocalClosed。
	ErrResetLocallyClosed = errors.New("vconn: cannot reset close notification on a locally closed connection")

	// ErrResetPeerDown 已探测到对端断开（或正常/异常结束），默认不允许重置通知状态；
	// 确认要重新武装请使用 ResetNotify(true)。
	//
	// 之所以要确认：连接池回收路径上，「对端下线」往往正是应该丢弃这条连接的理由，
	// 无脑重置会让一条已死的连接重新进入池子。
	//
	// 对应的状态类别：StatusPeerClosed（真实状态是对端结束，只是这一轮不能重新武装）。
	ErrResetPeerDown = errors.New("vconn: cannot reset close notification after peer was detected down")

	// ErrResetTimeout 上一轮监控协程未能在时限内退出，重置未完成；
	// 状态已清空，可直接重试 ResetNotify（不会重复启动监控）。
	//
	// 出现原因：上一轮监控正卡在阻塞式预读里，没能在 monitorStopTimeout 内退出。
	// 这属于「忙」而非「坏」，稍后重试即可。
	//
	// 对应的状态类别：StatusBusy。
	ErrResetTimeout = errors.New("vconn: previous monitor goroutine did not exit in time")
)

// 内部时序参数
const (
	// defaultMonitorInterval 空闲连接的探测周期（关闭检测灵敏度/CPU 占用的折中）
	defaultMonitorInterval = 200 * time.Millisecond
	// minMonitorInterval 允许的最小探测周期
	minMonitorInterval = 10 * time.Millisecond
	// peekTimeout 后台/同步阻塞预读统一使用的自身超时。
	//
	// 两条调用路径共用同一个值：
	//   - 监控协程的 peekStep（后台预读 / 丢弃）；
	//   - CheckBlocking / CheckReusable 的 peekProbeStrict（同步探测）。
	//
	// 取 2ms 的理由：这是「等一小会儿看有没有数据」的等待窗口，不是业务超时。
	// 后台路径每轮至多浪费 2ms（相对默认 200ms 的探测周期可忽略）；
	// 同步路径调用方本来就在等，2ms 的感知延迟也远低于人眼可辨范围。
	// 注意：受系统 timer 精度限制，低于 1ms 没有实际意义。
	peekTimeout = 2 * time.Millisecond
	// peekReadTimeout peekRead 的兜底超时：仅当调用方传入 timeout<=0 时使用。
	// 正常路径上（peekStep / peekProbeStrict）都会显式传 peekTimeout，
	// 因此该常量只在防御性场景生效。
	peekReadTimeout = 10 * time.Millisecond
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

	// defaultPeekSize CheckBlocking / CheckReusable 单次阻塞探测的默认预读额度。
	// 取 minPeekBuf（4 KiB）：小到不会显著增加常驻内存，大到单次 syscall 不会被截断。
	// 注意：这只是「本次探测的 peek 缓冲上限」，探测单次只读 1 字节，
	// 且不会把该值写回连接（不产生"探活一次就永久开启后台预读"的副作用）。
	defaultPeekSize = minPeekBuf
)

// closeSignal 单个关闭通知订阅者。
//
// 每个订阅者一个实例，由 Conn.signals 集合持有。
// ch 容量为 1：通知最多投递一次，且即便调用方来不及接收也不会阻塞分发方。
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

// newCloseSignal 创建一个订阅者实例（ch 缓冲 1，尚未投递任何错误）
func newCloseSignal() *closeSignal { return &closeSignal{ch: make(chan error, 1)} }

// fire 发送错误并关闭 channel，仅生效一次
//
// 用 once 保证「投递 + 关闭」整体只发生一次：
// 多次触发（例如监控与 Close 同时落定）时，订阅者只会收到第一个原因，
// 不会出现「往已关闭的 channel 发送」而 panic。
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
//
// 不投递任何错误：退订意味着「我不关心结果」，关闭 channel 即可让等待方退出。
func (s *closeSignal) discard() {
	s.once.Do(func() { close(s.ch) })
}

// Conn 封装了标准的 net.Conn，额外提供：
//
//   - 异步断开监控（CloseNotify / WatchClose）；
//   - 连接状态判定（Check / CheckBlocking / CheckReusable / Snapshot）；
//   - 读取限流（SetReadLimit）与后台预读（SetBackgroundReadBuffer）；
//   - 所有权转移（RawConn / RawConnFull），用于把连接交还调用方或交给连接池。
//
// # 并发约定
//
// 所有公开方法均并发安全。内部有两把锁值得知道：
//
//   - mu：保护关闭状态机（订阅者集合、closeErr、peerDown、关闭返回值）；
//   - r.fg：串行化所有前台读，因此多个 goroutine 并发 Read 不会互相
//     注入伪超时，也不会打乱预读字节的交付顺序。
//
// # 生命周期
//
// 正常用法是「New → 使用 → Close」。Close 会先关闭底层 fd（立即解除所有阻塞中的
// Read/Write），再分发关闭通知。调用 Close 之后，本连接不可再用于读写，
// 但 LocalAddr / RemoteAddr / Err / Snapshot 等只读方法依然可用。
type Conn struct {
	rwc net.Conn // 底层真实连接（构造后不可变）

	// —— 关闭状态机（由 mu 保护）——
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

	// —— 热路径无锁标志 ——
	closed  atomic.Bool // 关闭快速路径标志
	rawUsed atomic.Bool // 原始连接是否已被提取
	// notified 是 mu 保护下 notified 字段的无锁镜像，供探测热路径（isNotified）使用；
	// 写侧仍必须在 c.mu 内先更新 notified，再更新本镜像。
	notified    atomic.Bool   // 是否已分发过关闭通知（可被 ResetNotify 撤销）
	peekSize    atomic.Int64  // 后台预读/丢弃字节数：=0 关闭，>0 预读，<0 丢弃
	peekUserSet atomic.Bool   // 是否被 SetBackgroundReadBuffer 显式设置过（决定 CheckBlocking 能否隐式武装）
	readSeq     atomic.Uint64 // 成功读取计数，用于判定连接是否空闲（比时间戳更廉价）
	monInterval atomic.Int64  // 探测周期（纳秒）

	monMu sync.Mutex                 // 串行化监控实例的创建
	mon   atomic.Pointer[monitorRun] // 当前监控实例（nil 表示未运行）
}

// ==================== 锁与状态的分工总览 ====================
//
// 本包有四把锁，职责严格分离，阅读代码前先记住这张表：
//
//	c.mu         关闭状态机：signals / closeErr / peerDown / closeRet / closeDone
//	c.deadlineMu 底层 socket 的 deadline 写入（设置、临时改写、恢复）
//	c.r.fg       前台读串行化（所有 Read + 后台预读共用）
//	c.r.peekMu   预读缓冲区的读写
//
// 锁序约束（改动时必须保持）：
//
//	c.mu → c.r.peekMu    （Snapshot 会这样嵌套）
//	c.deadlineMu 不与 c.mu / c.r.peekMu 嵌套（它只包住 deadline 的写入）
//	c.r.fg 不与 c.mu 嵌套（后台预读持 fg 时绝不抢 c.mu）
//
// 特别注意 c.r.fg 与 c.deadlineMu 的配合：
// 「设置后台读的超时」必须在 deadlineMu 内完成，且必须先置位 peekBusy，
// 否则 stopMonitor 的「过去时间 deadline」会与后台读自己的 deadline 相互覆盖，
// 后台读可能永远阻塞（见 conn_reader.go 的 peekRead 不变量）。
//
// ==================== 监控协程的生命周期 ====================
//
// 监控是「惰性启动、按需分轮」的：
//
//	启动：第一次调用 CloseNotify / WatchClose 时（startMonitor）
//	停止：通知落定（notifyClose）、Close、RawConn、ResetNotify 换代时
//	分轮：每次 ResetNotify 成功后开启新一轮（新的 monitorRun 实例）
//
// 整条连接在任意时刻至多有一个「存活」的监控实例；
// 换代期间新旧实例可能短暂共存，但预读由 fg 锁串行化，不会同时读同一个 socket。

// New 包装原始连接，使其具备关闭提前通知与状态判定能力。
//
// 若 c 已经是 *Conn，则直接返回它自身（幂等，不会重复包装）。
// 参数 c 不能为 nil，否则会 panic。
//
// 典型用法：
//
//	raw, err := net.Dial("tcp", "127.0.0.1:8080")
//	if err != nil {
//		return err
//	}
//	c := vconn.New(raw)
//	defer c.Close()
//
//	go func() {
//		if err := <-c.CloseNotify(); err != nil {
//			log.Printf("连接已断开: %v", err)
//		}
//	}()
//
// 注意：New 本身不会启动监控协程。第一次调用 CloseNotify / WatchClose
// 才会惰性启动监控——没人关心关闭事件时，不值得为它付出系统调用。
func New(c net.Conn) *Conn {
	return NewData(c, nil)
}

// NewData 包装原始连接，并把 b 作为「已到达的预读数据」放入读取缓冲区
// （调用方的 Read 会优先返回这些字节）。
//
// 典型用途：把「从别处读到的、属于这条连接的数据」交接进来，例如连接池
// 把某个连接的响应字节转交给新持有者，或 TLS 握手后从底层读出的剩余明文。
//
// 若 c 已经是 *Conn，则把 b 追加到既有预读缓冲区后返回 c 本身（幂等）。
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
//
// 提取原始连接 = 把连接的「所有权」交还调用方。语义要点：
//
//   - 提取之后，本 Conn 不再拥有底层 fd：Close 不再关闭它；
//   - 提取之后，本 Conn 的读写路径全部失效（Read/Write 返回 net.ErrClosed）；
//   - 提取只能发生一次：重复调用 panic（ErrRawConnAlreadyUsed）；
//   - 提取时会分发一次 kindLocal 通知，原因是 ErrRawConnAlreadyUsed，
//     告诉订阅者「连接已经不属于 vconn 了」。

// RawConn 剥离包装，归还底层真实连接
//
// 剥离后，此 Conn 无法再读写，任何操作将返回 net.ErrClosed
// 该方法只能调用一次，重复调用会 panic（ErrRawConnAlreadyUsed）
//
// 与 Close 的区别：Close 会关闭底层连接；RawConn 只「解除所有权」，
// 底层连接仍然打开，调用方拿到后可以继续使用（这也是它不关闭底层的原因）。
//
// 若与 Close 并发调用：本方法会等待首个 Close 收尾后再返回，
// 确保不会与 Close 同时操作同一个 fd。
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
//
// 顺序很重要：先停监控（探测协程不得再访问 fd），再分发通知
// （订阅者收到通知时，连接已经彻底不可用了）。
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
// 与 RawConn 的差别：RawConn 会丢弃预读缓冲区里的字节（数据丢失），
// RawConnFull 会把它们一并交回，**零丢失**。连接池回收请务必用 RawConnFull。
//
// 注意：若调用本方法时有其它 goroutine 正阻塞在 Read 上，该 Read 会被临时 deadline
// 唤醒并返回一个超时错误；取得 fg 后会恢复调用方设置的 deadline。因此调用方应把
// RawConnFull 视为「连接所有权即将转移」，不要再让其它 goroutine 并发使用该连接，
// 也不要再并发调用 NewData。
// 对不支持 SetReadDeadline 的底层连接，本方法无法主动唤醒前台 Read。
func (c *Conn) RawConnFull() (net.Conn, []byte) {
	// 1. 先停监控：此后不会再有新的 peekStep/peekProbeStrict 启动。
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
	// 4. 仍持有 fg 时完成所有权转移：此后 Read/CheckBlocking 不会再碰预读缓冲区。
	raw := c.RawConn()
	c.r.fg.Unlock()

	return raw, data
}

// ==================== 配置 ====================
//
// 配置项分三类，语义互不重叠：
//
//	连接级（作用于整条连接）：SetMonitorInterval
//	数据面（影响 Read 的行为）：SetBackgroundReadBuffer / SetReadLimit
//	套接字级（透传给内核）：SetReadBuffer / SetKeepAlive / SetLinger / ...
//
// 配置项都可以并发修改：内部要么走原子变量，要么走独立的锁，
// 不会与读写路径争用同一把锁。

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
// 调用本方法后，CheckBlocking / CheckReusable 不再隐式武装预读（完全尊重用户设置，
// 包括显式设为 0 关闭）。
//
// 内存占用提示：预读缓冲（peek）与临时缓冲（peekTmp）都随连接常驻。
// 丢弃模式（n < 0）下 |n| 直接决定 peekTmp 的长度；当之后不再需要大缓冲时，
// vconn 会在需求变小且旧缓冲超过 64 KiB 时主动释放（见 tmpBuf）。
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
// remain >= 0: 限制剩余可读字节数，超出后返回 ErrQuotaExhausted
// （它用 %w 包住 io.EOF，按 io.EOF 判定与比较的语义不变；
// 想区分「配额用尽」与「对端关闭」请用 errors.Is(err, ErrQuotaExhausted)
// 或 StatusOf(err) == StatusQuota）。配额耗尽不会触发关闭通知。
//
// 语义：这是一个「读多少字节」的闸门，不是「连接最多存活多久」。
// 配额按**实际交付给调用方的字节数**扣减（包含从预读缓冲区取走的部分），
// 并发调用下用 CAS 扣减，不会超发。
//
// 配额耗尽后：Read 立即返回 ErrQuotaExhausted（不再触碰 socket），
// 且**不会**落定 peerDown —— 因为对端并没有断开。
// 需要恢复读能力请重新调用 SetReadLimit。
func (c *Conn) SetReadLimit(remain int64) {
	c.r.setReadLimit(remain)
}

// SetMonitorInterval 设置空闲连接的关闭探测周期，默认 200ms
// d <= 0 时恢复默认值
//
// 语义：这是「空闲连接多久探测一次」的节流阀，不是探测超时。
// 探测本身是非阻塞的（一次 poll/WSAPoll + MSG_PEEK），不会挂起任何 goroutine。
//
// 调小的代价：CPU 与系统调用次数上升（每条空闲连接每个周期一次探测）；
// 调大的代价：对端断开后，通知延迟变大（上限约等于该周期）。
//
// 生效方式：立即唤醒当前监控协程按新周期重置定时器，无需重启连接。
// 探测周期下限为 10ms（低于此值没有实际意义）。
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
//
// 关闭通知是一个「一次性事件」模型：
//
//	未落定 → 订阅者等待 → 某个原因落定 → 所有订阅者被唤醒 → channel 关闭
//
// 落定后 notified 被锁存，之后所有新订阅都会立即拿到同一个结果（不会重新探测）。
// 需要在同一条连接上「多轮订阅」请用 ResetNotify 清除锁存。
//
// 订阅者集合在分发时被整体换走：先复制出本轮订阅者，再在锁外逐个投递。
// 这样 fire 不会在持有 c.mu 时运行，订阅回调里再调用 Conn 方法也不会和分发死锁。
//
// 通知内容（channel 里收到的 error）按落定来源分为：
//
//	对端断开/网络异常：io.EOF（干净 FIN）、ECONNRESET 等（异常中断）、
//	                  或 ErrPeerClosedWithUnread（对端已结束但还有未读数据）
//	本地 Close：net.ErrClosed
//	原始连接被提取：ErrRawConnAlreadyUsed
//	手动取消：调用方传入的错误（nil 时用 ErrNotifyCanceled）
//
// 重要：通知只「告知」，不关闭底层连接。收到通知后是否 Close 由调用方决定
// （若带 ErrPeerClosedWithUnread，应先 drain 再 Close，否则可能丢数据）。

// CloseNotify 获取关闭通知 Channel
// 支持多次调用；在对端意外断开或本地主动 Close 时均会触发通知
// 返回的 channel 会在连接关闭时接收到错误并关闭
//
// 注意：本方法不提供退订手段，订阅者会一直保留到连接关闭。
// 需要退订请使用 WatchClose；需要「本次订阅没等到通知就想撤销」也请用 WatchClose。
//
// 副作用：第一次调用会启动监控协程（惰性启动）。没人关心关闭事件时，
// New 本身不会付出任何系统调用成本。
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
//
// 退订函数是幂等的：多次调用安全无副作用。
// 多个订阅者并存时，退订只删除自己的 closeSignal，不影响其它等待者。
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
				// 必须按订阅者指针删除。c.signals[sig] 的值是空结构体，
				// 既不是 map 的 key，删它也会在多个订阅者并存时删错人。
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
//
// 语义：**只通知，不关闭底层连接**——阻塞中的 Read 不会被 ctx 取消打断，
// 底层 socket 依然可用。需要「取消即断开」请使用 WithContextClose。
//
// 取消时写入通知的错误是 ctx.Err()（通常是 context.Canceled）；
// 若自定义 Context 违反契约（Done 已关闭但 Err() 返回 nil），
// 会用 ErrNotifyCanceled 兜底，避免把 nil 交给订阅者。
//
// 该通知属于「手动」类型：不会把连接标记为 peerDown（对端并没有断开），
// 但会锁存通知状态（StatusLatched），需要 ResetNotify 才能重新武装。
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
//   - 哨兵带 cause（ErrPeerClosedWithUnread + ECONNRESET）→ 返回该 cause，
//     保证 Read 不会把异常断开降级成干净的 io.EOF（不变量第 5 条）；
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
	if cerr != nil {
		// 哨兵携带 cause 时优先上抛真实原因：它的 Unwrap 里含 io.EOF，
		// 下面那条「非 EOF 才上抛」的守卫对它不生效，若在这里放过，
		// ECONNRESET 就会被 Read 静默降级为 io.EOF。
		var causer interface{ Cause() error }
		if errors.As(cerr, &causer) {
			if cause := causer.Cause(); cause != nil {
				return cause
			}
		}
		if !errors.Is(cerr, io.EOF) {
			return cerr
		}
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

// Read 从连接读取数据，实现 io.Reader，并发安全。
//
// 交付顺序固定为：上次遗留的终止错误、读取配额、预读缓冲、底层 Read。
// 已经从内核取出的字节一定先交给调用方；有数据时按 io.Reader 惯例返回
// (n, nil)，终止错误留到下一次调用。
//
// 本方法不用 c.closed 做入口短路。预读缓冲区里可能还存着已经到达的数据，
// 必须交由 connReader.Read 先交付；只有缓冲区为空且连接已关闭时才报
// net.ErrClosed。关闭后的 Read 不是热路径，这点开销可以接受。
//
// 配额耗尽返回 ErrQuotaExhausted。它满足 io.EOF，但不是对端关闭，
// 不会把连接标记为 peerDown，也不会分发关闭通知。
func (c *Conn) Read(b []byte) (n int, err error) {
	n, err = c.r.Read(b)
	if n > 0 {
		return n, nil // 有数据时优先返回数据，错误留到下一次（io.Reader 惯例）
	}
	if err != nil {
		// 配额耗尽（ErrQuotaExhausted）用 %w 包住了 io.EOF，但它不是「对端关闭」：
		// 那是调用方自己设的上限，不能借此落定 peerDown，否则 IsPeerClosed /
		// Status / CloseNotify 会一起撒谎（池化代码会因此误判连接已死）。
		if isCommonNetError(err) && !errors.Is(err, ErrQuotaExhausted) {
			c.notifyRemote(err)
		}
	}
	return n, err
}

// Write 向连接写入数据，实现 io.Writer，并发安全。
//
// 本地已关闭或原始连接已提取时直接返回 net.ErrClosed，不再触碰底层。
// 底层返回网络断开类错误且本次没有写出任何字节时，会分发一次远端关闭通知；
// 部分写入（n > 0）不在这里落定，避免把「写了一半」误报成连接已经结束。
// 超时属于调用方的控制流，不会触发关闭通知。
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

// Close 主动关闭连接，实现 io.Closer，并发安全。
//
// 顺序是：标记本地关闭、关闭底层 fd（立即解除阻塞中的读写）、记录首次返回值、
// 停止监控、以 net.ErrClosed 分发 kindLocal 通知。通知不会把连接标成 peerDown，
// 因为这是本地动作，不是对端断开。
//
// 重复调用返回首次关闭的错误。并发调用时，后到者会等待首个 Close 完成，
// 从而拿到与之完全相同的结果（不会出现「首个返回错误、第二个返回 nil」）。
// 原始连接已被 RawConn 提取时，本方法不再关闭底层 fd，只完成通知收尾。
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

// probe 是连接状态探测的唯一实现：
// Check / CheckBlocking / CheckReusable
// 全部由它派生，保证「同一结论只有一份映射逻辑」。
//
// block=true 时，若内核探测不可用（probeUnsupported：net.Pipe、未知 wrapper
// 等拿不到 fd 的连接），退化为一次带 peekTimeout（2ms）超时的阻塞预读；
// 额度由 peekProbeBudget 决定，只影响本次调用，不写回连接状态。
//
// 返回值：
//   - Status：本次观测的结论（含 flagUnread 修饰）；
//   - error：本次探测的原因。终结类结论（对端结束 / 本地关闭）非 nil，
//     非终结类结论（存活 / 有数据 / 无法验证）为 nil。
//
// 与 Err() 的区别：Err() 是连接级首次落定的结论（可能来自更早的监控轮次或
// CancelNotify），本方法的 error 是「这一次为什么」。终结类结论在返回前
// 会经由 recordProbe 并入状态机，因此 Err() / IsPeerClosed() / CloseNotify()
// 与本方法的结论保持一致。
func (c *Conn) probe(block bool) (Status, error) {
	if c.closed.Load() {
		return StatusLocalClosed, net.ErrClosed
	}

	res, err := c.checkKernel()
	switch res {
	case probeClosed:
		if c.closed.Load() {
			return StatusLocalClosed, net.ErrClosed
		}
		if err == nil {
			err = io.EOF
		}
		err = c.recordProbe(err)
		return peerState(err, c.r.hasPeek()), err

	case probeReadableHangup:
		if c.closed.Load() {
			return StatusLocalClosed, net.ErrClosed
		}
		if err == nil {
			err = peerClosedWithUnreadError{}
		}
		err = c.recordProbe(err)
		return peerState(err, true), err

	case probeReadable:
		// 存活且有数据：对「探活」是活，对「复用」是脏（由 CheckReusable 判定）。
		return StatusDataPending, nil

	case probeIdle:
		if c.r.hasPeek() {
			return StatusDataPending, nil
		}
		return StatusAlive, nil
	}

	// probeUnsupported：内核探测不可用（拿不到 fd / 非 socket）。
	if !block {
		return StatusUnverified, nil
	}
	if c.r.hasPeek() {
		return StatusDataPending, nil
	}
	size := c.peekProbeBudget()
	if size == 0 {
		// 用户显式关闭了预读，或处于丢弃模式：不能吞数据 → 无法验证。
		return StatusUnverified, nil
	}

	switch res, cerr := c.r.peekProbeStrict(size); res {
	case probeClosed:
		if c.closed.Load() {
			return StatusLocalClosed, net.ErrClosed
		}
		if cerr == nil {
			cerr = io.EOF
		}
		cerr = c.recordProbe(cerr)
		return peerState(cerr, c.r.hasPeek()), cerr

	case probeReadableHangup:
		if c.closed.Load() {
			return StatusLocalClosed, net.ErrClosed
		}
		if cerr == nil {
			cerr = peerClosedWithUnreadError{}
		}
		cerr = c.recordProbe(cerr)
		return peerState(cerr, true), cerr

	case probeReadable:
		return StatusDataPending, nil

	case probeIdle:
		if c.r.hasPeek() {
			return StatusDataPending, nil
		}
		return StatusAlive, nil
	}

	// 阻塞探测也没能给出答案（连接不支持 deadline / 前台正在读 / 本地已关闭）。
	if c.closed.Load() {
		return StatusLocalClosed, net.ErrClosed
	}
	return StatusUnverified, nil
}

// Check 探测连接状态：非阻塞、零消耗（net.Pipe 等无 fd 连接除外）、不关闭连接。
//
// 返回值：
//
//	StatusAlive                存活且当前无数据（已确认）
//	StatusDataPending          存活，但有未读数据
//	StatusPeerClosed(±Unread)  对端已结束；带 Unread 时数据仍可读，先 drain 再收尾
//	StatusPeerReset(±Unread)   对端异常中断（RST/EPIPE/网络故障）
//	StatusLocalClosed          本地已 Close / RawConn 提取
//	StatusUnverified           无法判定（内核探测不可用）—— **不是死亡**
//
// 副作用边界：不消耗数据、不丢数据、不关闭底层连接；终结类结论会并入状态机
// 并分发关闭通知（与监控协程结论一致）。
//
// 与 CheckReusable 的区别：本方法回答「活着吗」，StatusDataPending 对探活是活；
// 池化判定要的是「能不能交给下一个人」，请用 CheckReusable。
func (c *Conn) Check() (Status, error) { return c.probe(false) }

// CheckBlocking 同 Check，但在内核探测不可用时退化为一次带 2ms 超时的阻塞预读。
//
// 预读额度：用户从未调用 SetBackgroundReadBuffer 时使用 defaultPeekSize（4 KiB），
// 只作用于本次调用，不写回连接状态（一次探活不会把后台预读永久打开）。
//   - 设置正值：使用该值；
//   - 设置 0：不探测，返回 StatusUnverified（fail-open）；
//   - 设置负值（丢弃模式）：探活不得吞数据，同样返回 StatusUnverified。
//
// 单次只读 1 字节，可高频重复调用；预读到的字节进入预读缓冲，前台 Read 优先消费。
// 最坏等待 2ms。若前台正持有读锁，无法安全地再读，直接返回 StatusUnverified；
// 这是「现在没法判定」，不是「连接已死」，也不是可复用证明。
// 包装型连接（实现了 NetConn，且没有显式声明 SafeBlockProber）同样跳过阻塞读，
// 避免提前 Read 破坏 TLS 记录或吞掉多路复用帧。
func (c *Conn) CheckBlocking() (Status, error) { return c.probe(true) }

// checkKernel 执行内核级健康检查，供 probe 共用。
// CheckReusable 不直接调用它，而是走 probe(true)，从而在拿不到 fd 时还能退化为阻塞预读。
//
// 返回值：
//   - (probeClosed, err):         已确认断开；err 为断开原因（干净 FIN 时为 io.EOF，异常时为具体错误）
//   - (probeIdle, nil):           已确认存活且当前无数据
//   - (probeReadable, nil):       已确认存活且有数据可读
//   - (probeReadableHangup, err): 有未读数据且对端已结束。err 是 peerClosedWithUnreadError，
//     同时满足 ErrPeerClosedWithUnread 与 io.EOF；异常结束时 Cause() 带真实原因。
//     数据仍可读，但连接已经结束：probe 会映射成 StatusPeerClosedWithUnread
//     或 StatusPeerResetWithUnread，**不能**当成干净存活。
//   - (probeUnsupported, nil):    内核探测不可用，由调用方决定兜底策略
//
// 检查顺序：本地关闭状态 → SO_ERROR → poll/WSAPoll/MSG_PEEK 探测。
//
// socketError 返回 ErrUnsupported 时直接返回 probeUnsupported：
// socketError 与 probeConn 都通过 syscallConnOf 解包 fd，前者拿不到 fd 时
// 后者必然也拿不到，不必再浪费一次 probeConn 调用。
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

// peekProbeBudget 返回 CheckBlocking / CheckReusable 本次探测可用的预读额度。
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
