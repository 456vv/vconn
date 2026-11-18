package vconn

import (
	"bytes"
	"context"
	"errors"
	"io"
	"net"
	"runtime"
	"sync"
	"sync/atomic"
	"testing"
	"time"
)

// ==================== 测试辅助 ====================

// newTCPPair 返回 (包装后的客户端, 服务端原始连接, listener)
func newTCPPair(t *testing.T) (*Conn, net.Conn, net.Listener) {
	t.Helper()
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	accepted := make(chan net.Conn, 1)
	go func() {
		c, err := ln.Accept()
		if err != nil {
			accepted <- nil
			return
		}
		accepted <- c
	}()
	raw, err := net.Dial("tcp", ln.Addr().String())
	if err != nil {
		ln.Close()
		t.Fatal(err)
	}
	srv := <-accepted
	if srv == nil {
		raw.Close()
		ln.Close()
		t.Fatal("accept failed")
	}
	return New(raw), srv, ln
}

// memConn 永不到达 EOF 的内存连接，Read 立即返回数据
type memConn struct {
	data []byte
	off  int
	mu   sync.Mutex
}

func (m *memConn) Read(p []byte) (int, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	if len(p) == 0 {
		return 0, nil
	}
	n := copy(p, m.data[m.off:])
	m.off += n
	if n == 0 {
		return 0, io.EOF
	}
	return n, nil
}
func (m *memConn) Write(p []byte) (int, error) { return len(p), nil }
func (m *memConn) Close() error                { return nil }
func (m *memConn) LocalAddr() net.Addr         { return &net.TCPAddr{IP: net.IPv4(127, 0, 0, 1), Port: 1} }

func (m *memConn) RemoteAddr() net.Addr             { return &net.TCPAddr{IP: net.IPv4(127, 0, 0, 1), Port: 2} }
func (m *memConn) SetDeadline(time.Time) error      { return nil }
func (m *memConn) SetReadDeadline(time.Time) error  { return nil }
func (m *memConn) SetWriteDeadline(time.Time) error { return nil }

// noDeadlineConn 模拟不支持 deadline 的 net.Conn
type noDeadlineConn struct{ net.Conn }

func (d *noDeadlineConn) SetDeadline(time.Time) error { return errors.New("deadline unsupported") }

func (d *noDeadlineConn) SetReadDeadline(time.Time) error { return errors.New("deadline unsupported") }

func (d *noDeadlineConn) SetWriteDeadline(time.Time) error { return errors.New("deadline unsupported") }

type uncomparableErr struct{ D []byte }

func (e uncomparableErr) Error() string { return "uncomparable" }

// ==================== B01 对端先发数据再关闭 ====================

func TestNotify_PeerSendsDataThenCloses(t *testing.T) {
	vc, srv, ln := newTCPPair(t)
	defer ln.Close()
	defer srv.Close()

	ch := vc.CloseNotify()
	if _, err := srv.Write([]byte("hello")); err != nil {
		t.Fatal(err)
	}
	time.Sleep(50 * time.Millisecond) // 让预读吃掉首字节（旧版从此失去监控）
	if err := srv.Close(); err != nil {
		t.Fatal(err)
	}

	select {
	case err := <-ch:
		if !errors.Is(err, io.EOF) {
			t.Fatalf("期望 io.EOF，得到 %v", err)
		}
	case <-time.After(3 * time.Second):
		t.Fatal("BUG 回归：对端先发数据后关闭，CloseNotify 未触发")
	}

	// 通知归通知，数据不能丢
	buf := make([]byte, 16)
	vc.SetReadDeadline(time.Now().Add(time.Second))
	n, err := vc.Read(buf)
	if n != 5 || string(buf[:n]) != "hello" {
		t.Fatalf("数据丢失：n=%d data=%q err=%v", n, buf[:n], err)
	}
	vc.Close()
}

// 连续多轮写入 + 关闭，确保监控不会因预读满而停摆
func TestNotify_DataThenCloseRepeatedly(t *testing.T) {
	for i := range 5 {
		vc, srv, ln := newTCPPair(t)
		ch := vc.CloseNotify()
		srv.Write(bytes.Repeat([]byte("x"), 32))
		time.Sleep(20 * time.Millisecond)
		srv.Close()
		select {
		case err := <-ch:
			if !errors.Is(err, io.EOF) {
				t.Fatalf("第 %d 轮：期望 io.EOF，得到 %v", i, err)
			}
		case <-time.After(3 * time.Second):
			t.Fatalf("第 %d 轮：通知未触发", i)
		}
		vc.Close()
		ln.Close()
	}
}

// ==================== B02 Close 立即返回且解阻塞 ====================

func TestClose_UnsupportedDeadlineConn(t *testing.T) {
	s1, s2 := net.Pipe()
	defer s2.Close()

	vc := New(&noDeadlineConn{Conn: s1})
	ch := vc.CloseNotify()

	done := make(chan struct{})
	go func() {
		vc.Close()
		close(done)
	}()

	select {
	case <-done:
	case <-time.After(2 * time.Second):
		s1.Close()
		s2.Close()
		t.Fatal("BUG 回归：底层不支持 deadline 时 Close 永久阻塞")
	}

	select {
	case err := <-ch:
		if !errors.Is(err, net.ErrClosed) {
			t.Fatalf("期望 net.ErrClosed，得到 %v", err)
		}
	case <-time.After(time.Second):
		t.Fatal("关闭后未收到通知")
	}
}

func TestClose_UnblocksPendingRead(t *testing.T) {
	s1, s2 := net.Pipe()
	defer s2.Close()

	vc := New(s1)
	errc := make(chan error, 1)
	go func() {
		buf := make([]byte, 8)
		_, err := vc.Read(buf)
		errc <- err
	}()

	time.Sleep(50 * time.Millisecond)
	vc.Close()

	select {
	case err := <-errc:
		if err == nil {
			t.Fatal("期望阻塞中的 Read 返回错误")
		}
	case <-time.After(2 * time.Second):
		t.Fatal("BUG 回归：Close 未能解除阻塞中的 Read")
	}
}

// ==================== B03 用户 ReadDeadline 不被污染 ====================

func TestReadDeadline_SurvivesBackgroundRead(t *testing.T) {
	s1, s2 := net.Pipe()
	defer s2.Close()

	vc := New(s1)
	vc.SetBackgroundReadBuffer(4)
	vc.CloseNotify()
	vc.SetReadDeadline(time.Now().Add(120 * time.Millisecond))

	go func() { s2.Write([]byte("ab")) }()
	time.Sleep(60 * time.Millisecond)

	buf := make([]byte, 16)
	start := time.Now()
	n, err := vc.Read(buf) // 先返回预读的 2 字节
	if n != 2 || err != nil {
		t.Fatalf("期望 (2,nil)，得到 (%d,%v)", n, err)
	}
	if time.Since(start) > 400*time.Millisecond {
		t.Fatalf("首读耗时异常: %v", time.Since(start))
	}

	// 第二次读必须命中用户设置的超时，而不是无限阻塞
	start = time.Now()
	_, err = vc.Read(buf)
	if err == nil {
		t.Fatal("BUG 回归：用户设置的 ReadDeadline 已被后台预读抹掉")
	}
	if d := time.Since(start); d > time.Second {
		t.Fatalf("超时未生效，阻塞了 %v", d)
	}
	vc.Close()
}

// ==================== B04 CancelNotify 语义与安全 ====================

func TestCancelNotify_NilIsNoop(t *testing.T) {
	s1, s2 := net.Pipe()
	defer s1.Close()
	defer s2.Close()

	vc := New(s1)
	if !vc.CancelNotify(nil) {
		t.Fatal("首次 CancelNotify 应返回 true")
	}
	if vc.CancelNotify(nil) {
		t.Fatal("重复 CancelNotify 应返回 false")
	}

	select {
	case err := <-vc.CloseNotify():
		if !errors.Is(err, ErrNotifyCanceled) {
			t.Fatalf("期望 ErrNotifyCanceled，得到 %v", err)
		}
	case <-time.After(time.Second):
		t.Fatal("BUG 回归：CancelNotify(nil) 之后 CloseNotify 永久阻塞")
	}
}

func TestCancelNotify_UncomparableError(t *testing.T) {
	s1, s2 := net.Pipe()
	defer s1.Close()
	defer s2.Close()

	vc := New(s1)
	defer func() {
		if r := recover(); r != nil {
			t.Fatalf("BUG 回归：不可比较的 error 触发 panic: %v", r)
		}
	}()
	vc.CancelNotify(uncomparableErr{D: []byte{1}})
	vc.CancelNotify(uncomparableErr{D: []byte{2}})
	vc.CancelNotify(net.ErrClosed)
}

func TestCancelNotify_ClosesAllWatchers(t *testing.T) {
	s1, s2 := net.Pipe()
	defer s1.Close()
	defer s2.Close()

	vc := New(s1)
	const n = 16
	chans := make([]<-chan error, n)
	for i := range chans {
		chans[i] = vc.CloseNotify()
	}
	if !vc.CancelNotify(errors.New("boom")) {
		t.Fatal("期望首次返回 true")
	}
	for i, ch := range chans {
		select {
		case err := <-ch:
			if err == nil || err.Error() != "boom" {
				t.Fatalf("第 %d 个订阅者得到 %v", i, err)
			}
		case <-time.After(time.Second):
			t.Fatalf("第 %d 个订阅者未收到通知", i)
		}
	}
}

// ==================== B06 数据与错误分两次返回 ====================

func TestRead_DataThenEOF_NotTogether(t *testing.T) {
	vc, srv, ln := newTCPPair(t)
	defer ln.Close()

	vc.SetBackgroundReadBuffer(64)
	vc.CloseNotify()
	srv.Write([]byte("abc"))
	time.Sleep(50 * time.Millisecond)
	srv.Close()
	time.Sleep(100 * time.Millisecond)

	buf := make([]byte, 32)
	n, err := vc.Read(buf)
	if n != 3 || err != nil {
		t.Fatalf("BUG 回归：期望 (3,nil)，得到 (%d,%v)", n, err)
	}
	if string(buf[:n]) != "abc" {
		t.Fatalf("数据错误: %q", buf[:n])
	}

	n, err = vc.Read(buf)
	if n != 0 || !errors.Is(err, io.EOF) {
		t.Fatalf("期望 (0,EOF)，得到 (%d,%v)", n, err)
	}
	vc.Close()
}

// ==================== B07/B12 并发读不再互相注入伪超时 ====================

func TestRead_ConcurrentNoSpuriousTimeout(t *testing.T) {
	vc, srv, ln := newTCPPair(t)
	defer ln.Close()
	defer srv.Close()

	vc.CloseNotify()

	const readers = 4
	errc := make(chan error, readers)
	for i := 0; i < readers; i++ {
		go func() {
			buf := make([]byte, 64)
			_, err := vc.Read(buf)
			errc <- err
		}()
	}

	time.Sleep(150 * time.Millisecond) // 让全部读者都进入阻塞状态
	go func() {
		for i := 0; i < 16; i++ {
			if _, err := srv.Write([]byte("0123456789")); err != nil {
				return
			}
			time.Sleep(5 * time.Millisecond)
		}
	}()

	deadline := time.After(3 * time.Second)
	for i := 0; i < readers; i++ {
		select {
		case err := <-errc:
			if err != nil {
				t.Fatalf("BUG 回归：健康的空闲连接上并发读被注入错误 %v", err)
			}
		case <-deadline:
			t.Fatal("并发读超时")
		}
	}
	vc.Close()
}

// ==================== B09 不再每次 Read 起协程 ====================

func TestNotify_NoGoroutineGrowthPerRead(t *testing.T) {
	vc, srv, ln := newTCPPair(t)
	defer ln.Close()
	defer srv.Close()

	vc.CloseNotify()
	go func() {
		buf := make([]byte, 4096)
		for {
			if _, err := srv.Write(buf); err != nil {
				return
			}
		}
	}()

	buf := make([]byte, 1024)
	for i := 0; i < 500; i++ { // 预热
		if _, err := vc.Read(buf); err != nil {
			t.Fatal(err)
		}
	}
	base := runtime.NumGoroutine()
	for i := 0; i < 3000; i++ {
		if _, err := vc.Read(buf); err != nil {
			t.Fatal(err)
		}
	}
	after := runtime.NumGoroutine()
	if after > base+8 {
		t.Fatalf("goroutine 数量异常增长: %d -> %d", base, after)
	}
	vc.Close()
}

// ==================== B10 可退订、无泄漏 ====================

func TestWatchClose_Unsubscribe(t *testing.T) {
	s1, s2 := net.Pipe()
	defer s2.Close()

	vc := New(s1)
	ch, cancel := vc.WatchClose()

	vc.mu.Lock()
	n1 := len(vc.signals)
	vc.mu.Unlock()
	if n1 != 1 {
		t.Fatalf("期望 1 个订阅者，得到 %d", n1)
	}

	cancel()
	vc.mu.Lock()
	n2 := len(vc.signals)
	vc.mu.Unlock()
	if n2 != 0 {
		t.Fatalf("退订后仍残留 %d 个订阅者", n2)
	}

	select {
	case _, ok := <-ch:
		if ok {
			t.Fatal("退订后 channel 应被关闭")
		}
	case <-time.After(time.Second):
		t.Fatal("退订后 channel 未关闭")
	}
	vc.Close()
}

func TestWatchClose_RepeatedSubscribe(t *testing.T) {
	s1, s2 := net.Pipe()
	defer s2.Close()

	vc := New(s1)
	for i := 0; i < 100; i++ {
		_, cancel := vc.WatchClose()
		cancel()
	}
	vc.mu.Lock()
	n := len(vc.signals)
	vc.mu.Unlock()
	if n != 0 {
		t.Fatalf("反复订阅/退订后残留 %d 个订阅者", n)
	}
	vc.Close()
}

func TestWithContextClose_UnblocksRead(t *testing.T) {
	s1, s2 := net.Pipe()
	defer s2.Close()

	vc := New(s1)
	ctx, cancel := context.WithCancel(context.Background())
	vc.WithContextClose(ctx)

	errc := make(chan error, 1)
	go func() {
		buf := make([]byte, 8)
		_, err := vc.Read(buf)
		errc <- err
	}()
	time.Sleep(50 * time.Millisecond)
	cancel()

	select {
	case err := <-errc:
		if err == nil {
			t.Fatal("期望 Read 返回错误")
		}
	case <-time.After(2 * time.Second):
		t.Fatal("BUG 回归：上下文取消未能解除阻塞的 Read")
	}
}

// ==================== B13/B14 关闭后的 net.Conn 契约 ====================

func TestAddr_ValidAfterClose(t *testing.T) {
	vc, srv, ln := newTCPPair(t)
	defer ln.Close()
	defer srv.Close()

	la, ra := vc.LocalAddr(), vc.RemoteAddr()
	if la == nil || ra == nil {
		t.Fatal("构造后地址不应为 nil")
	}
	vc.Close()

	if vc.LocalAddr() == nil || vc.RemoteAddr() == nil {
		t.Fatal("BUG 回归：Close 后地址变为 nil（用户日志/统计会 nil panic）")
	}
	if vc.LocalAddr().String() != la.String() || vc.RemoteAddr().String() != ra.String() {
		t.Fatal("关闭后地址快照不一致")
	}
	if err := vc.SetReadDeadline(time.Now()); !errors.Is(err, net.ErrClosed) {
		t.Fatalf("关闭后 SetReadDeadline 应返回 net.ErrClosed，得到 %v", err)
	}
	if err := vc.SetDeadline(time.Now()); !errors.Is(err, net.ErrClosed) {
		t.Fatalf("关闭后 SetDeadline 应返回 net.ErrClosed，得到 %v", err)
	}
	if err := vc.Close(); err != nil {
		t.Fatalf("重复 Close 应返回首次结果，得到 %v", err)
	}
}

func TestRead_ClosedReturnsErrClosed(t *testing.T) {
	vc, srv, ln := newTCPPair(t)
	defer ln.Close()
	defer srv.Close()

	vc.Close()
	buf := make([]byte, 4)
	if _, err := vc.Read(buf); !errors.Is(err, net.ErrClosed) {
		t.Fatalf("关闭后 Read 应返回 net.ErrClosed，得到 %v", err)
	}
	if _, err := vc.Write(buf); !errors.Is(err, net.ErrClosed) {
		t.Fatalf("关闭后 Write 应返回 net.ErrClosed，得到 %v", err)
	}
}

// ==================== B15 并发下限流精确 ====================

func TestReadLimit_ConcurrentExact(t *testing.T) {
	const (
		limit   = 1000
		workers = 8
	)
	mc := &memConn{data: make([]byte, 64*1024)}
	vc := New(mc)
	vc.SetBackgroundReadBuffer(0)
	vc.SetReadLimit(limit)

	var (
		wg  sync.WaitGroup
		sum atomic.Int64
	)
	for i := 0; i < workers; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			buf := make([]byte, 200)
			for {
				n, err := vc.Read(buf)
				sum.Add(int64(n))
				if err != nil {
					return
				}
			}
		}()
	}
	wg.Wait()

	if got := sum.Load(); got != limit {
		t.Fatalf("限流不精确：读到 %d 字节，期望 %d", got, limit)
	}
}

// ==================== B17 健康检查 ====================

func TestCheckAlive(t *testing.T) {
	vc, srv, ln := newTCPPair(t)
	defer ln.Close()

	if err := vc.CheckAlive(); err != nil {
		t.Fatalf("健康连接 CheckAlive 应为 nil，得到 %v", err)
	}

	srv.Close()
	deadline := time.Now().Add(2 * time.Second)
	for time.Now().Before(deadline) {
		if err := vc.CheckAlive(); err != nil {
			vc.Close()
			return
		}
		time.Sleep(20 * time.Millisecond)
	}
	vc.Close()
	t.Fatal("对端已关闭，CheckAlive 仍返回 nil")
}

// ==================== B21 NewData 种子语义 ====================

func TestNewData_AppendsSeed(t *testing.T) {
	s1, s2 := net.Pipe()
	defer s1.Close()
	defer s2.Close()

	vc1 := New(s1)
	vc2 := NewData(vc1, []byte("SEED"))
	if vc2 != vc1 {
		t.Fatal("对已包装连接应返回同一对象")
	}
	buf := make([]byte, 4)
	if _, err := io.ReadFull(vc2, buf); err != nil {
		t.Fatal(err)
	}
	if string(buf) != "SEED" {
		t.Fatalf("BUG 回归：种子数据被丢弃，实际读到 %q", buf)
	}
}

// ==================== 无误报 ====================

func TestMonitor_NoSpuriousNotifyOnIdle(t *testing.T) {
	vc, srv, ln := newTCPPair(t)
	defer ln.Close()
	defer srv.Close()

	ch := vc.CloseNotify()
	select {
	case err := <-ch:
		t.Fatalf("空闲健康连接被误判为关闭: %v", err)
	case <-time.After(600 * time.Millisecond):
	}

	srv.Write([]byte("ping"))
	vc.SetReadDeadline(time.Now().Add(2 * time.Second))
	buf := make([]byte, 8)
	n, err := vc.Read(buf)
	if err != nil || string(buf[:n]) != "ping" {
		t.Fatalf("误判之后连接仍应可用: n=%d err=%v", n, err)
	}
	vc.Close()
}

func TestMonitor_NoNotifyWhileActive(t *testing.T) {
	vc, srv, ln := newTCPPair(t)
	defer ln.Close()
	defer srv.Close()

	ch := vc.CloseNotify()
	stop := make(chan struct{})
	go func() {
		for {
			select {
			case <-stop:
				return
			default:
			}
			if _, err := srv.Write([]byte("z")); err != nil {
				return
			}
			time.Sleep(time.Millisecond)
		}
	}()

	buf := make([]byte, 8)
	deadline := time.Now().Add(700 * time.Millisecond)
	for time.Now().Before(deadline) {
		if _, err := vc.Read(buf); err != nil {
			t.Fatalf("活跃连接读取失败: %v", err)
		}
		select {
		case err := <-ch:
			t.Fatalf("活跃连接被误判为关闭: %v", err)
		default:
		}
	}
	close(stop)
	vc.Close()
}

// ==================== 关闭通知（不关闭底层） ====================

func TestClose_RemoteDropNotifiesOnly(t *testing.T) {
	vc, srv, ln := newTCPPair(t)
	defer ln.Close()

	ch := vc.CloseNotify()
	srv.Close()

	select {
	case <-ch:
	case <-time.After(3 * time.Second):
		t.Fatal("未收到关闭通知")
	}
	if !vc.IsPeerClosed() {
		t.Fatal("应落定 peerDown")
	}
	if err := vc.Close(); err != nil {
		t.Fatalf("调用方 Close 应返回 nil，得到 %v", err)
	}
}

// ==================== 压力/竞态（配合 -race） ====================

func TestConcurrent_Stress(t *testing.T) {
	vc, srv, ln := newTCPPair(t)
	defer ln.Close()
	defer srv.Close()

	vc.CloseNotify()

	stop := make(chan struct{})
	var wg sync.WaitGroup

	// 写
	wg.Add(1)
	go func() {
		defer wg.Done()
		for {
			select {
			case <-stop:
				return
			default:
			}
			if _, err := vc.Write([]byte("ping")); err != nil {
				return
			}
			time.Sleep(time.Millisecond)
		}
	}()

	// 服务端回写
	wg.Add(1)
	go func() {
		defer wg.Done()
		buf := make([]byte, 64)
		for {
			if _, err := srv.Write([]byte("pong")); err != nil {
				return
			}
			if _, err := srv.Read(buf); err != nil {
				return
			}
		}
	}()

	// 并发读
	for i := 0; i < 3; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			buf := make([]byte, 64)
			for {
				select {
				case <-stop:
					return
				default:
				}
				vc.SetReadDeadline(time.Now().Add(20 * time.Millisecond))
				if _, err := vc.Read(buf); err != nil && !isTimeoutErr(err) {
					return
				}
			}
		}()
	}

	// 订阅/退订
	wg.Add(1)
	go func() {
		defer wg.Done()
		for {
			select {
			case <-stop:
				return
			default:
			}
			_, cancel := vc.WatchClose()
			vc.CheckAlive()
			vc.Err()
			vc.IsPeerClosed()
			cancel()
			time.Sleep(2 * time.Millisecond)
		}
	}()

	// 截止时间反复设置
	wg.Add(1)
	go func() {
		defer wg.Done()
		for {
			select {
			case <-stop:
				return
			default:
			}
			vc.SetDeadline(time.Now().Add(30 * time.Millisecond))
			vc.SetReadDeadline(time.Now().Add(30 * time.Millisecond))
			vc.SetWriteDeadline(time.Now().Add(30 * time.Millisecond))
			time.Sleep(2 * time.Millisecond)
		}
	}()

	time.Sleep(600 * time.Millisecond)
	close(stop)
	vc.Close()

	done := make(chan struct{})
	go func() { wg.Wait(); close(done) }()
	select {
	case <-done:
	case <-time.After(5 * time.Second):
		t.Fatal("并发压力测试出现死锁/挂起")
	}
}
