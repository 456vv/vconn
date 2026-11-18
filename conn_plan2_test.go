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

// countingCloseConn 统计底层 Close 调用次数，可在首次 Close 时阻塞。
type countingCloseConn struct {
	net.Conn
	closeErr error

	mu        sync.Mutex
	count     int
	started   chan struct{}
	release   chan struct{}
	startOnce sync.Once
}

func (c *countingCloseConn) Close() error {
	c.mu.Lock()
	c.count++
	first := c.count == 1
	c.mu.Unlock()

	if first {
		if c.started != nil {
			c.startOnce.Do(func() { close(c.started) })
		}
		if c.release != nil {
			<-c.release
		}
	}
	return c.closeErr
}

func (c *countingCloseConn) countValue() int {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.count
}

// optionConn 记录透传方法的调用。
type optionConn struct {
	net.Conn

	keepAlive       atomic.Bool
	keepAlivePeriod atomic.Int64
	linger          atomic.Int64
	closeWrite      atomic.Bool
	closeRead       atomic.Bool
}

func (c *optionConn) SetKeepAlive(v bool) error {
	c.keepAlive.Store(v)
	return nil
}

func (c *optionConn) SetKeepAlivePeriod(d time.Duration) error {
	c.keepAlivePeriod.Store(int64(d))
	return nil
}

func (c *optionConn) SetLinger(sec int) error {
	c.linger.Store(int64(sec))
	return nil
}

func (c *optionConn) CloseWrite() error {
	c.closeWrite.Store(true)
	return nil
}

func (c *optionConn) CloseRead() error {
	c.closeRead.Store(true)
	return nil
}

// ==================== 探测结果分类 ====================

// TestClassifyPeek_WouldBlockIsAlive poll 看见可读、但 MSG_PEEK 得到 EAGAIN
// 说明数据被并发 Read 抢先消费，连接仍然存活，不能判成 probeClosed。
func TestClassifyPeek_WouldBlockIsAlive(t *testing.T) {
	res, err := classifyPeek(peekWouldBlock, true)
	if err != nil || res != probeIdle {
		t.Fatalf("wouldBlock 应视为存活，得到 (%v,%v)", res, err)
	}

	res, err = classifyPeek(peekData, true)
	if res != probeReadableHangup || !errors.Is(err, io.EOF) {
		t.Fatalf("data+hangup 应为 readableHangup，得到 (%v,%v)", res, err)
	}

	res, err = classifyPeek(peekEOF, false)
	if res != probeClosed || !errors.Is(err, io.EOF) {
		t.Fatalf("EOF 应为 closed，得到 (%v,%v)", res, err)
	}
}

// ==================== 关闭状态机 ====================

// TestRawConn_NeverClosedByConn RawConn 取得所有权后，包装连接的任何路径
// 都不得再关闭底层。
func TestRawConn_NeverClosedByConn(t *testing.T) {
	s1, s2 := net.Pipe()
	defer s2.Close()

	fc := &countingCloseConn{Conn: s1}
	vc := New(fc)

	raw := vc.RawConn()
	if raw == nil {
		t.Fatal("RawConn 返回 nil")
	}
	// RawConn 已接管，Close 不得再关闭底层。
	if err := vc.Close(); err != nil {
		t.Fatalf("RawConn 后 Close 应返回 nil，得到 %v", err)
	}
	if n := fc.countValue(); n != 0 {
		t.Fatalf("RawConn 后 Close 仍关闭了底层 %d 次", n)
	}
	_ = raw.Close()
}

// TestClose_ConcurrentUnderlyingOnce 并发 Close 时，底层 Close 只能执行一次，
// 后到者等待首个关闭者完成并复用同一结果。
func TestClose_ConcurrentUnderlyingOnce(t *testing.T) {
	s1, s2 := net.Pipe()
	defer s2.Close()

	boom := errors.New("close boom")
	fc := &countingCloseConn{
		Conn:     s1,
		closeErr: boom,
		started:  make(chan struct{}),
		release:  make(chan struct{}),
	}
	vc := New(fc)

	firstDone := make(chan error, 1)
	go func() { firstDone <- vc.Close() }()

	select {
	case <-fc.started:
	case <-time.After(time.Second):
		t.Fatal("Close 未进入底层 Close")
	}

	secondDone := make(chan error, 1)
	go func() { secondDone <- vc.Close() }()

	// 后到者必须等待首个关闭者收尾，而不是返回自己第二次 Close 的错误。
	select {
	case err := <-secondDone:
		t.Fatalf("并发 Close 过早返回: %v", err)
	case <-time.After(50 * time.Millisecond):
	}

	close(fc.release)

	select {
	case err := <-firstDone:
		if !errors.Is(err, boom) {
			t.Fatalf("首个 Close 返回 %v，期望 %v", err, boom)
		}
	case <-time.After(time.Second):
		t.Fatal("首个 Close 未返回")
	}

	select {
	case err := <-secondDone:
		if !errors.Is(err, boom) {
			t.Fatalf("并发 Close 返回 %v，期望与首次一致（%v）", err, boom)
		}
	case <-time.After(time.Second):
		t.Fatal("并发 Close 未返回")
	}

	if n := fc.countValue(); n != 1 {
		t.Fatalf("底层 Close 执行了 %d 次，期望 1 次", n)
	}
	if err := vc.Close(); !errors.Is(err, boom) {
		t.Fatalf("再次 Close 应复用首次结果，得到 %v", err)
	}
	if n := fc.countValue(); n != 1 {
		t.Fatalf("再次 Close 后底层 Close 次数变为 %d", n)
	}
}

// ==================== RawConnFull ====================

// TestRawConnFull_UnblocksPendingRead 前台 Read 阻塞时，RawConnFull 必须能
// 通过临时 deadline 唤醒它，而不是永久等待 fg 锁。
func TestRawConnFull_UnblocksPendingRead(t *testing.T) {
	s1, s2 := net.Pipe()
	defer s2.Close()

	vc := New(s1)

	readDone := make(chan error, 1)
	go func() {
		buf := make([]byte, 1)
		_, err := vc.Read(buf)
		readDone <- err
	}()

	time.Sleep(20 * time.Millisecond) // 让 Read 真正阻塞并持有 fg

	type result struct {
		raw  net.Conn
		data []byte
	}
	resCh := make(chan result, 1)
	go func() {
		raw, data := vc.RawConnFull()
		resCh <- result{raw: raw, data: data}
	}()

	var res result
	select {
	case res = <-resCh:
	case <-time.After(2 * time.Second):
		t.Fatal("RawConnFull 被阻塞的前台 Read 卡住")
	}
	if res.raw == nil {
		t.Fatal("RawConnFull 返回 nil")
	}
	defer res.raw.Close()

	select {
	case err := <-readDone:
		if err == nil {
			t.Fatal("被 deadline 唤醒的 Read 应返回错误")
		}
	case <-time.After(time.Second):
		t.Fatal("前台 Read 未被唤醒")
	}

	// 原始连接必须仍然可用。
	go func() { _, _ = s2.Write([]byte("Z")) }()
	_ = res.raw.SetReadDeadline(time.Now().Add(time.Second))
	buf := make([]byte, 1)
	n, err := res.raw.Read(buf)
	if err != nil || n != 1 || buf[0] != 'Z' {
		t.Fatalf("提取后的原始连接不可用: n=%d data=%q err=%v", n, buf[:n], err)
	}
}

// ==================== 其他行为 ====================

func TestNewData_ClosedConnIgnored(t *testing.T) {
	s1, s2 := net.Pipe()
	defer s2.Close()

	vc := New(s1)
	vc.Close()

	NewData(vc, []byte("ignored"))
	if got := vc.r.takePeek(); len(got) != 0 {
		t.Fatalf("已关闭连接不应接受预读数据，得到 %q", got)
	}
}

func TestReadSeq_CountedBeforeMonitor(t *testing.T) {
	s1, s2 := net.Pipe()
	defer s2.Close()

	vc := New(s1)
	defer vc.Close()
	go func() { _, _ = s2.Write([]byte("a")) }()

	buf := make([]byte, 1)
	if _, err := vc.Read(buf); err != nil {
		t.Fatal(err)
	}
	if vc.readSeq.Load() == 0 {
		t.Fatal("监控启动前的读也必须计入 readSeq")
	}
}

func TestCheckAlive_AfterClose(t *testing.T) {
	s1, s2 := net.Pipe()
	defer s2.Close()

	vc := New(s1)
	vc.Close()
	if err := vc.CheckAlive(); !errors.Is(err, net.ErrClosed) {
		t.Fatalf("关闭后 CheckAlive 应返回 net.ErrClosed，得到 %v", err)
	}
}

func TestOptionPassThrough(t *testing.T) {
	s1, s2 := net.Pipe()
	defer s2.Close()

	oc := &optionConn{Conn: s1}
	vc := New(oc)
	defer vc.Close()

	if err := vc.SetKeepAlive(true); err != nil || !oc.keepAlive.Load() {
		t.Fatalf("SetKeepAlive 透传失败: %v", err)
	}
	if err := vc.SetKeepAlivePeriod(3 * time.Second); err != nil || time.Duration(oc.keepAlivePeriod.Load()) != 3*time.Second {
		t.Fatalf("SetKeepAlivePeriod 透传失败: %v", err)
	}
	if err := vc.SetLinger(5); err != nil || oc.linger.Load() != 5 {
		t.Fatalf("SetLinger 透传失败: %v", err)
	}
	if err := vc.CloseWrite(); err != nil || !oc.closeWrite.Load() {
		t.Fatalf("CloseWrite 透传失败: %v", err)
	}
	if err := vc.CloseRead(); err != nil || !oc.closeRead.Load() {
		t.Fatalf("CloseRead 透传失败: %v", err)
	}

	// 不支持时返回 ErrUnsupported。
	plain := New(s1)
	defer plain.Close()
	if err := plain.SetKeepAlive(true); !errors.Is(err, ErrUnsupported) {
		t.Fatalf("net.Pipe 上 SetKeepAlive 应返回 ErrUnsupported，得到 %v", err)
	}
}
