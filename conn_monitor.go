package vconn

import (
	"sync"
	"time"
)

// monitorRun 一轮监控实例。
//
// 每轮监控的生命周期由自己的 quit/done 承载，因此可以在
// 「通知已落定 → ResetNotify 重置」之后开启新一轮监控，
// 而不需要重建 Conn。（若用「close 一次的 channel」表达停止信号，
// 监控就只能是一次性的，无法分轮。）
//
// 本实例的 quit 会显式传递给 peekStep/peekRead：
// 预读路径用它判断「我这一轮是否已被要求退出」，而不是到 c.mon 去反查。
// 反查在换代瞬间会认错实例，让旧监控的阻塞预读不受约束。
//
// 三个 channel 的分工：
//   - quit：退出信号（本轮停止的唯一凭据，由 stop 幂等关闭）；
//   - wake：唤醒信号（请求按最新周期重置 timer，容量 1，重复请求自动合并）；
//   - done：收尾信号（监控协程真正退出后关闭，stopMonitor 据此判断是否退出成功）。

type monitorRun struct {
	quit     chan struct{}
	wake     chan struct{}
	done     chan struct{}
	stopOnce sync.Once
}

func newMonitorRun() *monitorRun {
	return &monitorRun{
		quit: make(chan struct{}),
		wake: make(chan struct{}, 1),
		done: make(chan struct{}),
	}
}

// stop 通知本轮监控退出（幂等、非阻塞）
func (m *monitorRun) stop() {
	m.stopOnce.Do(func() { close(m.quit) })
}

// wakeup 请求立即按最新周期重置 timer（非阻塞、合并重复请求）
func (m *monitorRun) wakeup() {
	select {
	case m.wake <- struct{}{}:
	default:
	}
}

// stopping 本轮监控是否已收到退出信号
func (m *monitorRun) stopping() bool {
	select {
	case <-m.quit:
		return true
	default:
		return false
	}
}

// runningMonitor 当前是否有存活（未收到退出信号）的监控实例
func (c *Conn) runningMonitor() *monitorRun {
	m := c.mon.Load()
	if m == nil || m.stopping() {
		return nil
	}
	return m
}

// startMonitor 惰性启动监控协程（可重复调用，重置后可再次启动）
//
// 若已有存活实例则直接返回；若上一轮实例正在退出（quit 已关闭但未收尾），
// 则新起一轮，旧实例会自行结束，两者不会同时读取同一 socket
// （预读由 fg 锁串行化）。
func (c *Conn) startMonitor() {
	if c.closed.Load() || c.rawUsed.Load() {
		return
	}

	c.monMu.Lock()
	if c.closed.Load() || c.rawUsed.Load() || c.runningMonitor() != nil {
		c.monMu.Unlock()
		return
	}
	m := newMonitorRun()
	c.mon.Store(m)
	c.monMu.Unlock()

	go c.monitorLoop(m)
}

// signalMonitorStop 通知当前监控协程退出（非阻塞、幂等）
func (c *Conn) signalMonitorStop() {
	if m := c.mon.Load(); m != nil {
		m.stop()
	}
}

// wakeMonitor 请求当前监控协程按最新周期重置 timer（非阻塞）
func (c *Conn) wakeMonitor() {
	if m := c.mon.Load(); m != nil {
		m.wakeup()
	}
}

// stopMonitor 通知并等待当前监控协程退出，返回是否确认已退出。
//
// 有上限（monitorStopTimeout），绝不无限等待；只清理本轮实例，
// 不会误清并发重置写入的新实例。
func (c *Conn) stopMonitor() bool {
	m := c.mon.Load()
	if m == nil {
		return true
	}
	m.stop()

	// 若监控正卡在阻塞式预读里，用一个"过去时间"的 deadline 催促它退出。
	// 此时预读独占前台读锁（peekBusy），不会误伤调用方的 Read。
	// 检查与 peekRead 的 SetReadDeadline 都在 deadlineMu 内，避免互相覆盖。
	//
	// peekRead 对任何一次后台 socket 读都会置位 peekBusy，
	// 因此不存在「阻塞读正在进行、peekBusy 却为 false」而无法唤醒的情况。
	pushed := false
	c.deadlineMu.Lock()
	if c.r.peekBusy.Load() {
		if err := c.rwc.SetReadDeadline(time.Unix(0, 1)); err == nil {
			pushed = true
		}
	}
	c.deadlineMu.Unlock()

	exited := false
	timer := time.NewTimer(monitorStopTimeout)
	select {
	case <-m.done:
		exited = true
	case <-timer.C:
	}
	timer.Stop()
	c.mon.CompareAndSwap(m, nil)

	// 仅在真的"催促"过时才恢复调用方设置的读超时：
	// 否则每次停监控都会多一次无意义的 syscall，还可能覆盖调用方刚刚设置的值。
	if pushed {
		c.deadlineMu.Lock()
		_ = c.rwc.SetReadDeadline(c.readDeadline)
		c.deadlineMu.Unlock()
	}
	return exited
}

// monitorLoop 常驻监控协程：每轮一个实例，空闲时按周期探测。
//
// 首轮立即执行，是为了「订阅之后马上知道已经死了的连接」，不用空等一个周期。
// 首轮只有在这条连接还没有任何成功的内核读取时才允许阻塞预读：
// readSeq > 0 说明前台刚刚读过，再阻塞读既可能抢数据，也会把 ReadDeadline 临时改掉。
// 之后每一轮用 readSeq 判断空闲。序号没变才探测；序号变了说明前台 Read 自己会看到 EOF。
func (c *Conn) monitorLoop(m *monitorRun) {
	// 先清掉实例指针，再通知 done：让 stopMonitor 的等待者看到一致状态，
	// 也避免「监控已自行退出但 c.mon 仍指向死实例」导致无法再开新一轮。
	defer func() {
		c.mon.CompareAndSwap(m, nil)
		close(m.done)
	}()

	// 首轮立即执行：既覆盖"连接建立后立刻订阅关闭事件"的用法，
	// 也让「重置后重新武装」立刻生效（不必等一个探测周期）。
	// 重置后的首轮不允许阻塞预读（readSeq>0 时），只做内核级探测。
	allowBlock := c.readSeq.Load() == 0
	if c.monitorPass(m, allowBlock, true) {
		return
	}

	timer := time.NewTimer(c.monitorInterval())
	defer timer.Stop()

	lastSeq := c.readSeq.Load()
	for {
		select {
		case <-m.quit:
			return
		case <-m.wake:
			resetTimer(timer, c.monitorInterval())
			continue
		case <-timer.C:
		}

		cur := c.readSeq.Load()
		idle := cur == lastSeq
		lastSeq = cur

		if c.monitorPass(m, false, idle) {
			return
		}
		timer.Reset(c.monitorInterval())
	}
}

// resetTimer 安全地按新周期重置 timer（兼容 timer 已触发/已停止的情况）
func resetTimer(timer *time.Timer, d time.Duration) {
	if !timer.Stop() {
		select {
		case <-timer.C:
		default:
		}
	}
	timer.Reset(d)
}

// monitorPass 执行一轮探测，返回 true 表示监控应当结束。
//
// m 为本轮监控实例：它会被透传给 peekStep，供阻塞预读在发起前做
// 「本实例是否已被要求退出」的拒绝迟到检查（不能用 c.mon 反查）。
//
// allowBlock 仅在首轮、且连接尚未发生过成功读取时为 true：允许做一次等待型探测。
// idle 表示上一周期内没有发生前台读取。
//
// 结束条件只有三类：本地已关闭、通知已经落定、本轮探测确认对端结束。
// 预读缓冲满、前台正在读、内核不支持探测，都只是本轮跳过，监控继续按周期运行。
func (c *Conn) monitorPass(m *monitorRun, allowBlock, idle bool) bool {
	if c.closed.Load() || c.isNotified() {
		return true
	}

	// 活跃连接由 Read 路径负责（读到 EOF 会立刻通知），只有空闲时才周期性探测，
	// 这样繁忙连接不会额外消耗系统调用。
	if !idle {
		return c.isNotified()
	}

	// 内核级零消耗探测：判断对端是否已关闭（FIN/RST）
	res, perr := c.probePeekAware()
	switch res {
	case probeClosed, probeReadableHangup:
		// probeClosed：接收队列已空 + 对端 FIN/RST；
		// probeReadableHangup：对端已 FIN，但接收队列里还有未读数据。
		// 两种情况都只落定通知（"提前通知"正是本包的价值），**绝不**顺手关闭
		// 底层：关闭是调用方的责任，此时关闭还会丢掉未读数据。
		if c.closed.Load() { // 本地已关闭，通知由 Close 负责
			return true
		}
		c.recordProbe(perr)
		return true
	}

	if c.peekSize.Load() == 0 {
		return c.isNotified()
	}

	closed, err := c.r.peekStep(m.quit, res, allowBlock)
	if closed {
		if c.closed.Load() {
			return true
		}
		c.notifyClose(err, kindRemote)
		return true
	}
	return c.isNotified()
}

// isNotified 读取"关闭通知是否已落定"。
//
// 无锁：c.notified 是 c.mu 保护下的 notified 字段的无锁镜像
// （写侧在 c.mu 内成对更新）。探测热路径每周期都会调用本方法，
// 避免为此反复抢 c.mu。
func (c *Conn) isNotified() bool {
	return c.notified.Load()
}

// monitorInterval 返回当前探测周期
func (c *Conn) monitorInterval() time.Duration {
	d := time.Duration(c.monInterval.Load())
	if d <= 0 {
		return defaultMonitorInterval
	}
	return d
}
