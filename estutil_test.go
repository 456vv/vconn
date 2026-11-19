package vconn

import (
	"errors"
	"fmt"
	"io"
	"net"
	"sync"
	"sync/atomic"
	"testing"
	"time"
)

// ==================== 连接构造 ====================

// tcpPair 建立一对本地 TCP 连接：(包装后的客户端, 服务端原始连接, listener)。
func tcpPair(t *testing.T) (*Conn, net.Conn, net.Listener) {
	t.Helper()
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatalf("listen: %v", err)
	}
	accepted := make(chan net.Conn, 1)
	go func() {
		c, aerr := ln.Accept()
		if aerr != nil {
			accepted <- nil
			return
		}
		accepted <- c
	}()
	raw, err := net.Dial("tcp", ln.Addr().String())
	if err != nil {
		_ = ln.Close()
		t.Fatalf("dial: %v", err)
	}
	srv := <-accepted
	if srv == nil {
		_ = raw.Close()
		_ = ln.Close()
		t.Fatal("accept 失败")
	}
	return New(raw), srv, ln
}

// pipePair 返回一对内存管道连接（无内核探测能力）。
func pipePair() (net.Conn, net.Conn) { return net.Pipe() }

// pipeWrite 在同步管道（net.Pipe 等无缓冲实现）上写入，并在对端起读者校验收到的字节。
//
// net.Pipe 的 Write 会一直阻塞到对端读走，所以「先写、再让对端读」的写法必然死锁；
// 这里统一成「先起读者 → 再写 → 校验对端收到什么」。
func pipeWrite(t *testing.T, what string, w io.Writer, peer io.Reader, payload string) {
	t.Helper()

	readErr := make(chan error, 1)
	go func() {
		buf := make([]byte, len(payload))
		if _, err := io.ReadFull(peer, buf); err != nil {
			readErr <- err
			return
		}
		if string(buf) != payload {
			readErr <- fmt.Errorf("对端收到 %q，期望 %q", buf, payload)
			return
		}
		readErr <- nil
	}()

	if _, err := w.Write([]byte(payload)); err != nil {
		t.Fatalf("%s: 写入失败: %v", what, err)
	}
	if err := <-readErr; err != nil {
		t.Fatalf("%s: 对端读取失败: %v", what, err)
	}
}

// ==================== 等待 ====================

// waitCond 轮询等待条件成立。
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

// waitPeekLen 等待后台预读缓冲区攒到至少 n 字节。
func waitPeekLen(vc *Conn, n int, d time.Duration) bool {
	deadline := time.Now().Add(d)
	for time.Now().Before(deadline) {
		vc.r.peekMu.Lock()
		have := len(vc.r.peek)
		vc.r.peekMu.Unlock()
		if have >= n {
			return true
		}
		time.Sleep(5 * time.Millisecond)
	}
	return false
}

// waitStatus 反复探测直到满足条件或超时（FIN/RST 是异步到达的）。
func waitStatus(t *testing.T, vc *Conn, want func(Status) bool, d time.Duration) (Status, error) {
	t.Helper()
	deadline := time.Now().Add(d)
	for {
		st, err := vc.Check()
		if want(st) || time.Now().After(deadline) {
			return st, err
		}
		time.Sleep(5 * time.Millisecond)
	}
}

// ==================== 断言 ====================

// awaitErr 等待一次关闭通知。
func awaitErr(t *testing.T, ch <-chan error, what string) error {
	t.Helper()
	select {
	case err := <-ch:
		return err
	case <-time.After(3 * time.Second):
		t.Fatalf("%s: 等待关闭通知超时", what)
		return nil
	}
}

// assertNoPending 断言当前没有待投递的通知（同步判定）。
func assertNoPending(t *testing.T, ch <-chan error, what string) {
	t.Helper()
	select {
	case err := <-ch:
		t.Fatalf("%s: 不应有通知，却收到 %v", what, err)
	default:
	}
}

// assertNoNotify 断言 d 内没有收到通知。
func assertNoNotify(t *testing.T, ch <-chan error, d time.Duration, what string) {
	t.Helper()
	select {
	case err := <-ch:
		t.Fatalf("%s: 不应收到通知，却收到 %v", what, err)
	case <-time.After(d):
	}
}

// assertChannelClosed 断言 channel 已关闭且没有残留值。
func assertChannelClosed(t *testing.T, ch <-chan error, what string) {
	t.Helper()
	select {
	case v, ok := <-ch:
		if ok {
			t.Fatalf("%s: channel 应已关闭，却收到 %v", what, v)
		}
	default:
		t.Fatalf("%s: channel 应处于已关闭状态", what)
	}
}

// watcherCount 返回当前订阅者数量。
func watcherCount(c *Conn) int {
	c.mu.Lock()
	defer c.mu.Unlock()
	return len(c.signals)
}

// ==================== 伪连接 ====================

// memConn 永远有数据的内存连接（Read 不阻塞）
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

// noDeadlineConn 模拟不支持 deadline 的底层连接
type noDeadlineConn struct{ net.Conn }

func (d *noDeadlineConn) SetDeadline(time.Time) error { return errors.New("deadline unsupported") }

func (d *noDeadlineConn) SetReadDeadline(time.Time) error { return errors.New("deadline unsupported") }

func (d *noDeadlineConn) SetWriteDeadline(time.Time) error { return errors.New("deadline unsupported") }

// closeErrConn 让 Close 返回固定错误
type closeErrConn struct {
	net.Conn
	err error
}

func (c *closeErrConn) Close() error {
	_ = c.Conn.Close()
	return c.err
}

// countingCloseConn 统计底层 Close 次数，首次 Close 可被阻塞
type countingCloseConn struct {
	net.Conn
	closeErr  error
	started   chan struct{}
	release   chan struct{}
	startOnce sync.Once
	mu        sync.Mutex
	count     int
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

// deadlineCounter 统计底层 SetReadDeadline 调用次数
type deadlineCounter struct {
	net.Conn
	n atomic.Int64
}

func (d *deadlineCounter) SetReadDeadline(t time.Time) error {
	d.n.Add(1)
	return d.Conn.SetReadDeadline(t)
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

// optionConn 记录可选方法的透传
type optionConn struct {
	net.Conn
	keepAlive       atomic.Bool
	keepAlivePeriod atomic.Int64
	linger          atomic.Int64
	closeWrite      atomic.Bool
	closeRead       atomic.Bool
}

func (c *optionConn) SetKeepAlive(v bool) error { c.keepAlive.Store(v); return nil }
func (c *optionConn) SetKeepAlivePeriod(d time.Duration) error {
	c.keepAlivePeriod.Store(int64(d))
	return nil
}
func (c *optionConn) SetLinger(sec int) error { c.linger.Store(int64(sec)); return nil }
func (c *optionConn) CloseWrite() error       { c.closeWrite.Store(true); return nil }
func (c *optionConn) CloseRead() error        { c.closeRead.Store(true); return nil }

// netConnWrapper 模拟「声明了 NetConn() 的包装连接」（如 *tls.Conn）
type netConnWrapper struct{ net.Conn }

func (w *netConnWrapper) NetConn() net.Conn { return w.Conn }

// noProbeConn 显式声明不可阻塞探测
type noProbeConn struct{ net.Conn }

func (noProbeConn) SafeToBlockProbe() bool { return false }

// uncomparableConn 值语义 + 不可比较字段，用于验证解包环检测不 panic
type uncomparableConn struct {
	net.Conn
	buf []byte
}

// uncomparableErr 不可比较的 error
type uncomparableErr struct{ D []byte }

func (e uncomparableErr) Error() string { return "uncomparable" }
