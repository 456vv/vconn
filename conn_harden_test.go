package vconn

import (
	"errors"
	"io"
	"net"
	"sync"
	"sync/atomic"
	"testing"
	"time"
)

// ==================== 测试辅助 ====================

// waitCond 轮询等待条件成立
func waitCond(t *testing.T, what string, d time.Duration, cond func() bool) {
	t.Helper()
	deadline := time.Now().Add(d)
	for time.Now().Before(deadline) {
		if cond() {
			return
		}
		time.Sleep(5 * time.Millisecond)
	}
	t.Fatalf("%s: 等待超时", what)
}

// deadlineCounter 统计底层 SetReadDeadline 的调用次数
type deadlineCounter struct {
	net.Conn
	n atomic.Int64
}

func (d *deadlineCounter) SetReadDeadline(t time.Time) error {
	d.n.Add(1)
	return d.Conn.SetReadDeadline(t)
}

// closeErrConn 让 Close 返回固定错误，便于验证「重复 Close 结果一致」
type closeErrConn struct {
	net.Conn
	err error
}

func (c *closeErrConn) Close() error {
	_ = c.Conn.Close()
	return c.err
}

// errConn 一次 Read 同时返回数据与自定义错误
type errConn struct {
	net.Conn
	data []byte
	err  error
}

func (c *errConn) Read(p []byte) (int, error) {
	n := copy(p, c.data)
	c.data = c.data[n:]
	return n, c.err
}

// ==================== 限流：预读字节也必须计入额度 ====================

// TestReadLimit_IncludesPeekedBytes 混合路径（peek + socket）必须按总字节数扣额度。
// 旧实现只按 socket 读到的字节扣，导致 SetReadLimit(N) 实际可返回 N+len(peek)。
func TestReadLimit_IncludesPeekedBytes(t *testing.T) {
	s1, s2 := net.Pipe()
	defer s2.Close()

	// 8 字节种子进入预读缓冲区
	vc := NewData(s1, []byte("12345678"))
	defer vc.Close()
	vc.SetReadLimit(10)

	go func() { _, _ = s2.Write([]byte("abcdefgh")) }()

	var (
		total int
		buf   = make([]byte, 64)
	)
	for i := 0; i < 5; i++ {
		n, err := vc.Read(buf)
		total += n
		if err != nil {
			break
		}
	}
	if total != 10 {
		t.Fatalf("限流被预读字节绕过：共读到 %d 字节，期望 10", total)
	}
}

// ==================== 对端 FIN：通知但不关底层 ====================

// TestProbeHangup_NotifiesWithoutClosing 「有未读数据 + 对端已 FIN」时，
// 必须立刻分发通知（这正是本包的价值），但**不能**顺手关闭底层 —— 关闭是
// 调用方的责任，此处关闭还会丢掉未读数据。
func TestProbeHangup_NotifiesWithoutClosing(t *testing.T) {
	s1, s2 := net.Pipe()
	defer s2.Close()

	vc := New(s1)
	defer vc.Close()
	ch := vc.CloseNotify()

	go func() { _, _ = s2.Write([]byte("hello")) }()
	time.Sleep(30 * time.Millisecond) // 让数据进入管道

	// 模拟监控探测到「有数据 + 对端 FIN」
	if !vc.notifyClose(io.EOF, kindRemote) {
		t.Fatal("首次通知应成功")
	}
	select {
	case err := <-ch:
		if !errors.Is(err, io.EOF) {
			t.Fatalf("期望 io.EOF，得到 %v", err)
		}
	case <-time.After(time.Second):
		t.Fatal("通知未送达")
	}

	if !vc.IsPeerClosed() {
		t.Fatal("应落定 peerDown")
	}

	buf := make([]byte, 16)
	vc.SetReadDeadline(time.Now().Add(time.Second))
	n, err := vc.Read(buf)
	if n != 5 || string(buf[:n]) != "hello" || err != nil {
		t.Fatalf("数据被丢弃：n=%d data=%q err=%v", n, buf[:n], err)
	}
}

// TestClose_IdempotentAfterPeerFin 对端 FIN 后（未开启任何自动关闭）重复 Close
// 必须返回首次结果（nil）。
func TestClose_IdempotentAfterPeerFin(t *testing.T) {
	s1, s2 := net.Pipe()

	vc := New(s1)
	vc.SetBackgroundReadBuffer(1) // net.Pipe 无内核探测，需预读才能感知 FIN
	vc.CloseNotify()

	_ = s2.Close()
	waitCond(t, "检测到对端 FIN", 2*time.Second, vc.IsPeerClosed)

	if err := vc.Close(); err != nil {
		t.Fatalf("Close 应返回 nil，得到 %v", err)
	}
	if err := vc.Close(); err != nil {
		t.Fatalf("重复 Close 仍应返回 nil，得到 %v", err)
	}
}

// ==================== 并发 Close：结果一致 ====================

func TestClose_ConcurrentSameResult(t *testing.T) {
	s1, s2 := net.Pipe()
	defer s2.Close()

	boom := errors.New("close boom")
	vc := New(&closeErrConn{Conn: s1, err: boom})

	const n = 8
	res := make([]error, n)
	start := make(chan struct{})
	var wg sync.WaitGroup
	for i := 0; i < n; i++ {
		wg.Add(1)
		go func(i int) {
			defer wg.Done()
			<-start
			res[i] = vc.Close()
		}(i)
	}
	close(start)
	wg.Wait()

	for i, err := range res {
		if !errors.Is(err, boom) {
			t.Fatalf("第 %d 个并发 Close 返回 %v，期望与首次一致（%v）", i, err, boom)
		}
	}
}

// ==================== ResetNotify：幂等分支必须补回监控 ====================

// TestResetNotify_IdempotentRearmsMonitor 上一轮监控已退出、状态又处于「未落定」时，
// 幂等重置必须把监控补回来（否则重试 ErrResetTimeout 会永久失去关闭通知）。
func TestResetNotify_IdempotentRearmsMonitor(t *testing.T) {
	vc, srv, ln := newTCPPair(t)
	defer ln.Close()

	vc.SetMonitorInterval(20 * time.Millisecond)
	ch := vc.CloseNotify()

	vc.stopMonitor() // 人为让监控退出，但通知尚未落定
	if vc.runningMonitor() != nil {
		t.Fatal("监控应已停止")
	}

	if err := vc.ResetNotify(false); err != nil {
		t.Fatalf("幂等重置失败: %v", err)
	}
	if vc.runningMonitor() == nil {
		t.Fatal("幂等重置应把缺失的监控补回来")
	}

	srv.Close()
	select {
	case err := <-ch:
		if err == nil {
			t.Fatal("对端关闭应携带原因")
		}
	case <-time.After(3 * time.Second):
		t.Fatal("补回监控后仍未检测到对端关闭")
	}
	vc.Close()
}

// ==================== 原始连接提取：不丢字节 ====================

// TestRawConnFull_NoByteLoss 在监控高频预读的同时提取原始连接，字节必须一个不少。
func TestRawConnFull_NoByteLoss(t *testing.T) {
	vc, srv, ln := newTCPPair(t)
	defer ln.Close()

	const total = 4096
	vc.SetBackgroundReadBuffer(512)
	vc.SetMonitorInterval(5 * time.Millisecond)
	vc.CloseNotify()

	go func() {
		buf := make([]byte, total)
		for i := range buf {
			buf[i] = byte(i)
		}
		_, _ = srv.Write(buf)
		_ = srv.Close()
	}()

	// 让监控跑若干轮预读，制造「提取时有在途读取」的窗口
	time.Sleep(80 * time.Millisecond)

	raw, peeked := vc.RawConnFull()
	defer raw.Close()
	_ = raw.SetReadDeadline(time.Now().Add(2 * time.Second))

	got := append([]byte(nil), peeked...)
	buf := make([]byte, 1024)
	for len(got) < total {
		n, err := raw.Read(buf)
		got = append(got, buf[:n]...)
		if err != nil {
			break
		}
	}
	if len(got) != total {
		t.Fatalf("提取原始连接时丢字节：得到 %d，期望 %d（其中预读 %d）", len(got), total, len(peeked))
	}
	for i := range got {
		if got[i] != byte(i) {
			t.Fatalf("字节顺序错误 @%d: got %d want %d", i, got[i], byte(i))
		}
	}
}

// ==================== 监控生命周期 ====================

// TestMonitor_SelfExitClearsInstance 监控自行退出后必须清空 c.mon，
// 否则 startMonitor 会把死实例当成活实例，永远无法再开新一轮。
func TestMonitor_SelfExitClearsInstance(t *testing.T) {
	vc, srv, ln := newTCPPair(t)
	defer ln.Close()

	vc.SetMonitorInterval(10 * time.Millisecond)
	ch := vc.CloseNotify()

	srv.Close()
	select {
	case <-ch:
	case <-time.After(3 * time.Second):
		t.Fatal("未收到关闭通知")
	}

	waitCond(t, "自行退出的监控实例被清理", 2*time.Second, func() bool {
		return vc.mon.Load() == nil
	})
	vc.Close()
}

// TestStopMonitor_NoRedundantDeadlineWrite 停监控时若没有「催促」预读，
// 不应再写一次 ReadDeadline（多余 syscall，且可能覆盖调用方刚设置的值）。
func TestStopMonitor_NoRedundantDeadlineWrite(t *testing.T) {
	s1, s2 := net.Pipe()
	defer s2.Close()

	dc := &deadlineCounter{Conn: s1}
	vc := New(dc)
	_ = vc.CloseNotify() // 起监控（peekSize=0，不会进入阻塞预读）

	time.Sleep(30 * time.Millisecond)
	before := dc.n.Load()
	vc.stopMonitor()
	if after := dc.n.Load(); after != before {
		t.Fatalf("停监控时多写了 %d 次 ReadDeadline", after-before)
	}
	vc.Close()
}

// ==================== 错误语义 ====================

// TestIsCommonNetError_IgnoresGenericError 通用错误（非网络断开）不能被判定为断开，
// 否则健康连接会被标记为 peerDown，并给订阅者发出假的关闭通知。
func TestIsCommonNetError_IgnoresGenericError(t *testing.T) {
	generic := errors.New("invalid argument")
	if isCommonNetError(generic) {
		t.Fatal("普通错误不应被判定为网络断开")
	}
	if isCommonNetError(&net.OpError{Op: "read", Err: generic}) {
		t.Fatal("包装后的普通错误也不应被判定为断开")
	}
	// 超时是正常控制流
	if isCommonNetError(&net.OpError{Op: "read", Err: timeoutErr{}}) {
		t.Fatal("超时不应被判定为网络断开")
	}
	// 真正的断开仍要识别
	if !isCommonNetError(io.EOF) {
		t.Fatal("io.EOF 应被判定为断开")
	}
}

// timeoutErr 实现 net.Error 的 Timeout
type timeoutErr struct{}

func (timeoutErr) Error() string   { return "i/o timeout" }
func (timeoutErr) Timeout() bool   { return true }
func (timeoutErr) Temporary() bool { return true }

// TestSocketError_UnsupportedNotClosed 拿不到 fd 时必须返回 ErrUnsupported，
// 由 probeConn 兜底，而不是把「探测失败」当成「连接已断开」。
func TestSocketError_UnsupportedNotClosed(t *testing.T) {
	s1, s2 := net.Pipe()
	defer s2.Close()

	if err := socketError(s1); !errors.Is(err, ErrUnsupported) {
		t.Fatalf("net.Pipe 上 socketError 应返回 ErrUnsupported，得到 %v", err)
	}

	vc := New(s1)
	defer vc.Close()
	st, err := vc.Check()
	if err != nil || st.Class() != StatusUnverified {
		t.Fatalf("net.Pipe 上 Check 应 fail-open 返回 StatusUnverified，得到 (%v, %v)", st, err)
	}
}

// TestRead_NonCommonErrorPreserved 数据与非超时错误同时返回时，错误必须留存到下一次。
func TestRead_NonCommonErrorPreserved(t *testing.T) {
	s1, s2 := net.Pipe()
	defer s2.Close()

	boom := errors.New("boom")
	vc := New(&errConn{Conn: s1, data: []byte("ab"), err: boom})
	defer vc.Close()

	buf := make([]byte, 8)
	n, err := vc.Read(buf)
	if n != 2 || err != nil {
		t.Fatalf("首次读应返回 (2,nil)，得到 (%d,%v)", n, err)
	}
	n, err = vc.Read(buf)
	if n != 0 || !errors.Is(err, boom) {
		t.Fatalf("遗留错误丢失：得到 (%d,%v)，期望 (0,boom)", n, err)
	}
}

// ==================== CheckBlocking 无副作用 ====================

// TestCheckBlocking_NoSideEffectOnPeekSize 一次探活不应把后台预读永久打开。
func TestCheckBlocking_NoSideEffectOnPeekSize(t *testing.T) {
	s1, s2 := net.Pipe()
	defer s1.Close()
	defer s2.Close()

	vc := New(s1)
	st, err := vc.CheckBlocking()
	if err != nil || st.Class() != StatusAlive {
		t.Fatalf("静默对端应视为存活，得到 (%v, %v)", st, err)
	}
	if got := vc.peekSize.Load(); got != 0 {
		t.Fatalf("探活不应改写 peekSize，得到 %d", got)
	}
	if vc.peekUserSet.Load() {
		t.Fatal("探活不应把连接标记为「用户已设置预读」")
	}
}

// ==================== 预读阻塞安全性判定 ====================

// TestIsBlockProbeSafe 叶子连接默认可探测，包装型连接默认不碰（安全优先）。
func TestIsBlockProbeSafe(t *testing.T) {
	s1, s2 := net.Pipe()
	defer s1.Close()
	defer s2.Close()

	if !isBlockProbeSafe(s1) {
		t.Fatal("叶子连接（net.Pipe）应允许阻塞探测")
	}
	if isWrapperConn(s1) {
		t.Fatal("net.Pipe 不是包装型连接")
	}

	// 包装型连接：声明了 NetConn() → 默认不碰
	wrapped := &netConnWrapper{Conn: s1}
	if !isWrapperConn(wrapped) {
		t.Fatal("声明 NetConn() 的连接应识别为包装型")
	}
	if isBlockProbeSafe(wrapped) {
		t.Fatal("包装型连接默认不应被阻塞探测")
	}

	// 包装型 + 显式 opt-in → 允许
	if !isBlockProbeSafe(WrapSafeBlockProbe(wrapped)) {
		t.Fatal("WrapSafeBlockProbe 应恢复阻塞探测能力")
	}

	// 叶子 + 显式 opt-out → 拒绝
	if isBlockProbeSafe(noProbeConn{Conn: s1}) {
		t.Fatal("SafeToBlockProbe()=false 应拒绝阻塞探测")
	}
}

// netConnWrapper 模拟「声明了 NetConn() 的包装连接」（如 *tls.Conn）
type netConnWrapper struct{ net.Conn }

func (w *netConnWrapper) NetConn() net.Conn { return w.Conn }

// noProbeConn 显式声明不可阻塞探测
type noProbeConn struct{ net.Conn }

func (noProbeConn) SafeToBlockProbe() bool { return false }

// ==================== 探测：有未读数据时不得判为已关闭 ====================

// TestProbeConn_DataThenFIN_NotClosed 对端「先发数据再 FIN」时，
// 只要还有未读数据就不能返回 probeClosed —— 否则上层会把「还有响应体没读」
// 误当成「接收队列已空」，进而提前关闭连接丢数据。
func TestProbeConn_DataThenFIN_NotClosed(t *testing.T) {
	vc, srv, ln := newTCPPair(t)
	defer ln.Close()
	defer vc.Close()

	if _, err := srv.Write([]byte("hello")); err != nil {
		t.Fatal(err)
	}
	time.Sleep(20 * time.Millisecond) // 让数据落到接收队列
	if err := srv.Close(); err != nil {
		t.Fatal(err)
	}

	// FIN 之后持续探测：数据未被消费前，任何一轮都不得判为 probeClosed
	deadline := time.Now().Add(400 * time.Millisecond)
	for time.Now().Before(deadline) {
		res, err := probeConn(vc.rwc)
		if res == probeClosed {
			t.Fatalf("有未读数据时被判为已关闭（会丢数据）：err=%v", err)
		}
		time.Sleep(10 * time.Millisecond)
	}

	// 数据必须仍然可读
	buf := make([]byte, 8)
	vc.SetReadDeadline(time.Now().Add(time.Second))
	n, err := vc.Read(buf)
	if n != 5 || string(buf[:n]) != "hello" {
		t.Fatalf("数据丢失：n=%d data=%q err=%v", n, buf[:n], err)
	}
}
