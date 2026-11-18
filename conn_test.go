package vconn

import (
	"bytes"
	"errors"
	"io"
	"net"
	"sync"
	"syscall"
	"testing"

	//"fmt"
	"time"
)

type Fatal func(args ...interface{})

// assertPeerClosed 断言错误属于「对端正常结束」这一类。
//
// 用 StatusOf 而不是 err == io.EOF：对端「发完数据再关闭」时 vconn 会带
// ErrPeerClosedWithUnread（同时满足 errors.Is(err, io.EOF)），
// 直接比较会在监控轮次与前台读的时序竞争下随机失败。
func assertPeerClosed(t *testing.T, stage string, err error) {
	t.Helper()
	if k := StatusOf(err); k != StatusPeerClosed {
		t.Fatalf("%s: 期望对端正常结束，得到 %v (kind=%v)", stage, err, k)
	}
}

// 测试客户端关闭，服务端收到 io.EOF
//
// 断言统一走 StatusOf，而不是 err == io.EOF（或 err != io.EOF）：
// 对端「发完数据再关闭」时 vconn 会带 ErrPeerClosedWithUnread（同时满足
// errors.Is(err, io.EOF)），直接比较会在「监控轮次」与「前台读」的时序竞争下
// 随机失败——这正是引入 Status 分类要解决的问题。
func Test_conn_Read_1(t *testing.T) {
	notify := make(chan error, 1)
	// 监听端
	netListener, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	defer netListener.Close()

	go func() {
		netConn, err := netListener.Accept()
		if err != nil {
			notify <- err
			return
		}

		vc := New(netConn)
		ch := vc.CloseNotify()
		_, _ = io.Copy(io.Discard, vc)
		notify <- <-ch
		vc.Close()
	}()

	// 发送端
	netAddr := netListener.Addr()
	netConn, err := net.Dial(netAddr.Network(), netAddr.String())
	if err != nil {
		t.Fatal(err)
	}
	netConn.Write([]byte("123456"))
	netConn.Close()

	assertPeerClosed(t, "关闭通知", <-notify)
}

// 测试客户端关闭，服务端收到 io.EOF
func TestConn_Read_2(t *testing.T) {
	notify := make(chan error, 1)
	readErr := make(chan error, 1)
	// 监听端
	netListener, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	defer netListener.Close()

	go func() {
		netConn, err := netListener.Accept()
		if err != nil {
			notify <- err
			readErr <- err
			return
		}
		defer netConn.Close()

		vc := New(netConn)
		ch := vc.CloseNotify()

		go func() {
			p := make([]byte, 8)
			for {
				n, err := vc.Read(p)
				if err != nil {
					readErr <- err
					return
				}
				if n == 0 {
					readErr <- errors.New("unexpected zero read")
					return
				}
			}
		}()

		// 连接关闭事件
		notify <- <-ch
	}()

	// 发送端
	netAddr := netListener.Addr()
	netConn, err := net.Dial(netAddr.Network(), netAddr.String())
	if err != nil {
		t.Fatal(err)
	}
	netConn.Write([]byte("1234567890"))
	netConn.Close()

	assertPeerClosed(t, "关闭通知", <-notify)
	assertPeerClosed(t, "前台 Read", <-readErr)
}

// 测试客户端关闭，服务端收到 io.EOF
// 后台读取丢弃
func Test_conn_SetBackgroundReadDiscard(t *testing.T) {
	notify := make(chan error, 1)
	// 监听端
	netListener, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	defer netListener.Close()

	go func() {
		netConn, err := netListener.Accept()
		if err != nil {
			notify <- err
			return
		}
		defer netConn.Close()

		vc := New(netConn)
		vc.SetBackgroundReadBuffer(-1)
		notify <- <-vc.CloseNotify()
	}()

	// 发送端
	netAddr := netListener.Addr()
	netConn, err := net.Dial(netAddr.Network(), netAddr.String())
	if err != nil {
		t.Fatal(err)
	}
	netConn.Write([]byte("12345678901234567890123456789012345678901234567890123456789012345678901234567890123456789012345678901234567890123456789012345678901234567890"))
	netConn.Close()

	assertPeerClosed(t, "丢弃模式关闭通知", <-notify)
}

// 测试客户端关闭，服务端收到 io.EOF
// 不允许后台读取
func TestConn_DisableBackgroundRead(t *testing.T) {
	notify := make(chan error, 1)
	dataErr := make(chan error, 1)
	// 监听端
	netListener, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	defer netListener.Close()

	p := []byte("12345678901234567890123456789012345678901234567890123456789012345678901234567890123456789012345678901234567890123456789012345678901234567890")
	go func() {
		netConn, err := netListener.Accept()
		if err != nil {
			notify <- err
			dataErr <- err
			return
		}
		defer netConn.Close()

		vc := New(netConn)
		vc.SetBackgroundReadBuffer(0)
		ch := vc.CloseNotify()

		// 关闭通知与前台读并行：通知必须在读完之前就绪（本包的核心能力）
		time.Sleep(time.Second)

		buf := bytes.NewBuffer(nil)
		if _, err := buf.ReadFrom(vc); err != nil {
			dataErr <- err
		} else if !bytes.Equal(buf.Bytes(), p) {
			dataErr <- errors.New("read data mismatch: " + buf.String())
		} else {
			dataErr <- nil
		}
		notify <- <-ch
	}()

	// 发送端
	netAddr := netListener.Addr()
	netConn, err := net.Dial(netAddr.Network(), netAddr.String())
	if err != nil {
		t.Fatal(err)
	}
	netConn.Write(p)
	netConn.Close()

	assertPeerClosed(t, "关闭通知", <-notify)
	if err := <-dataErr; err != nil {
		t.Fatalf("前台读数据失败: %v", err)
	}
}

func TestConn_Basic(t *testing.T) {
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	defer ln.Close()

	var wg sync.WaitGroup
	wg.Add(1)
	go func() {
		defer wg.Done()
		c, _ := ln.Accept()
		vc := New(c)
		defer vc.Close()

		ch := vc.CloseNotify()
		_, err := io.Copy(io.Discard, vc)
		if isCommonNetError(err) {
			// 其它错误
			t.Error(err)
		}

		select {
		case e := <-ch:
			assertPeerClosed(t, "关闭通知", e)
		case <-time.After(2 * time.Second):
			t.Error("timeout")
		}
	}()

	conn, _ := net.Dial("tcp", ln.Addr().String())
	conn.Write([]byte("hello"))
	conn.Close()
	wg.Wait()
}

func TestConn_RawConn(t *testing.T) {
	ln, _ := net.Listen("tcp", "127.0.0.1:0")
	defer ln.Close()

	done := make(chan struct{})
	go func() {
		defer close(done)
		c, _ := ln.Accept()
		vc := New(c)
		// 预读默认关闭（n=0）：要 RawConnFull 归还字节必须显式开启
		vc.SetBackgroundReadBuffer(1)
		ch := vc.CloseNotify() // start background

		// 后台预读是周期性的（默认 200ms 一轮），用轮询替代固定 sleep
		if !waitPeekLen(vc, 1, 2*time.Second) {
			t.Error("background peek did not happen in time")
		}
		raw, peeked := vc.RawConnFull()
		if len(peeked) != 1 {
			t.Errorf("unexpected peek: %v", peeked)
		}
		defer raw.Close()
		// 等待后台读结束
		select {
		case e := <-ch:
			if e != ErrRawConnAlreadyUsed {
				t.Errorf("expected close, got %v", e)
			}
		case <-time.After(2 * time.Second):
			t.Error("timeout")
		}
	}()

	c, _ := net.Dial("tcp", ln.Addr().String())
	c.Write([]byte{1, 2, 3})
	defer c.Close()
	<-done
}

func TestConn_ReadLimit(t *testing.T) {
	ln, _ := net.Listen("tcp", "127.0.0.1:0")
	defer ln.Close()

	go func() {
		c, _ := ln.Accept()
		vc := New(c)
		vc.SetReadLimit(5)
		defer vc.Close()

		b := make([]byte, 10)
		n, err := vc.Read(b)
		if n != 5 || err != nil {
			t.Errorf("want 5,nil got %d,%v", n, err)
		}
	}()

	c, _ := net.Dial("tcp", ln.Addr().String())
	c.Write([]byte("1234567890"))
	c.Close()
	time.Sleep(100 * time.Millisecond)
}

func TestConn_ReadWithPeek(t *testing.T) {
	s1, c2 := pipe()
	defer s1.Close()
	defer c2.Close()

	go c2.Write([]byte{0xFF, 0xAA})
	vc := New(s1)

	// 触发 background peek
	vc.CloseNotify()

	time.Sleep(100 * time.Millisecond) // 等待后台

	b := make([]byte, 2)
	n, err := vc.Read(b)
	if n != 2 || err != nil || bytes.Equal(b, []byte{0xFF, 0xAA}) == false {
		t.Errorf("read failed: n=%d err=%v", n, err)
	}
}

// 创建一对内存管道连接以加速测试
func pipe() (net.Conn, net.Conn) {
	c1, c2 := net.Pipe()
	return c1, c2
}

// waitPeekLen 等待后台预读缓冲区攒到至少 n 字节（测试专用，直接观察内部状态）。
// 预读是周期性的（默认 200ms 一轮），轮询比固定 sleep 更稳定。
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

func TestConn_BasicReadWrite(t *testing.T) {
	c1, c2 := pipe()
	defer c1.Close()
	defer c2.Close()

	vc := New(c1)

	payload := []byte("hello virtual connection")
	go func() {
		_, _ = c2.Write(payload)
	}()

	buf := make([]byte, len(payload))
	n, err := vc.Read(buf)
	if err != nil {
		t.Fatalf("Read failed: %v", err)
	}
	if n != len(payload) || !bytes.Equal(buf, payload) {
		t.Errorf("Expected %q, got %q", payload, buf[:n])
	}
}

func TestConn_ConcurrentReadWrite(t *testing.T) {
	s, c := net.Pipe()
	defer s.Close()
	vc := New(c)

	go func() {
		for {
			buf := make([]byte, 10)
			if _, err := vc.Read(buf); err != nil {
				return
			}
		}
	}()

	for i := 0; i < 100; i++ {
		go vc.Write([]byte("test"))
	}
	time.Sleep(100 * time.Millisecond)
	vc.Close()
}

func TestConn_CloseNotifyRemoteClose(t *testing.T) {
	c1, c2 := pipe()
	vc := New(c1)
	// net.Pipe 没有内核级可读探测，只能退化为「带短超时的阻塞预读」，
	// 因此必须显式开启预读（默认 0 = 关闭），否则永远检测不到对端关闭。
	vc.SetBackgroundReadBuffer(1)

	notify := vc.CloseNotify()

	// 模拟远程主动挂断连接
	_ = c2.Close()

	select {
	case err := <-notify:
		if err == nil {
			t.Error("Expected connection drop error, got nil")
		}
		if !errors.Is(err, io.EOF) {
			t.Errorf("Expected EOF error, got: %v", err)
		}
	case <-time.After(2 * time.Second):
		t.Fatal("Timeout waiting for remote close notification")
	}
	vc.Close()
}

func TestConn_CloseNotifyLocalClose(t *testing.T) {
	c1, c2 := pipe()
	defer c2.Close()

	vc := New(c1)
	notify := vc.CloseNotify()

	// 本地关闭包装连接
	_ = vc.Close()

	select {
	case err := <-notify:
		if !errors.Is(err, net.ErrClosed) {
			t.Errorf("Expected net.ErrClosed error on local close, got: %v", err)
		}
	case <-time.After(2 * time.Second):
		t.Fatal("Timeout waiting for local close notification")
	}
}

func TestConn_ContinuousIdleBackgroundRead(t *testing.T) {
	c1, c2 := pipe()
	vc := New(c1)
	defer c2.Close()
	// net.Pipe 无内核探测，需显式开启预读才能在对端关闭时被唤醒
	vc.SetBackgroundReadBuffer(1)

	notify := vc.CloseNotify()

	// 1. 先行写入第一批数据
	go func() {
		_, _ = c2.Write([]byte("A"))
	}()

	// 2. 主动前台读取，验证是否能成功消费，且保证将后台读取中断并重新挂起
	buf := make([]byte, 1)
	n, err := vc.Read(buf)
	if err != nil || n != 1 || buf[0] != 'A' {
		t.Fatalf("Failed to read first chunk: %v", err)
	}

	// 3. 此时连接进入第二阶段的 Idle。关闭对端，验证后台检测是否持续起效！
	_ = c2.Close()

	select {
	case err := <-notify:
		if !errors.Is(err, io.EOF) {
			t.Errorf("Failed to detect secondary drop, err: %v", err)
		}
	case <-time.After(2 * time.Second):
		t.Fatal("Failed to re-trigger idle background check after user read")
	}
}

func TestConn_RawConnExtraction(t *testing.T) {
	c1, c2 := pipe()
	defer c2.Close()

	vc := New(c1)
	notify := vc.CloseNotify()

	// 提取原始连接
	raw := vc.RawConn()
	if raw == nil {
		t.Fatal("RawConn returned nil")
	}

	select {
	case err := <-notify:
		if !errors.Is(err, ErrRawConnAlreadyUsed) {
			t.Errorf("Expected notify channel to close with net.ErrClosed, got %v", err)
		}
	default:
		t.Error("Extracting RawConn should immediately broadcast close to CloseNotify channels")
	}

	// 原始连接应当可以继续读写
	go func() {
		_, _ = c2.Write([]byte("raw test"))
	}()

	buf := make([]byte, 8)
	n, err := raw.Read(buf)
	if err != nil || n != 8 || string(buf) != "raw test" {
		t.Errorf("Extracted raw connection was damaged: %v, read %d bytes", err, n)
	}
	_ = raw.Close()
}

func TestConn_RawConnFullPeeked(t *testing.T) {
	c1, c2 := pipe()
	defer c2.Close()

	vc := New(c1)
	vc.SetBackgroundReadBuffer(1) // net.Pipe 无内核探测，需显式开启预读
	_ = vc.CloseNotify()          // 触发后台预读

	// 等后台真正预读到这一个字节（周期 200ms，轮询避免固定 sleep 抖动）
	go func() {
		_, _ = c2.Write([]byte("X"))
	}()
	if !waitPeekLen(vc, 1, 2*time.Second) {
		t.Fatal("background peek did not happen in time")
	}

	raw, peeked := vc.RawConnFull()
	defer raw.Close()

	if len(peeked) != 1 || peeked[0] != 'X' {
		t.Errorf("Failed to capture peeked byte. Got peeked=%q", peeked)
	}
}

func TestConn_RawConnPanicOnRepeatedCalls(t *testing.T) {
	c1, c2 := pipe()
	defer c1.Close()
	defer c2.Close()

	vc := New(c1)
	_ = vc.RawConn()

	defer func() {
		if r := recover(); r == nil {
			t.Error("Expected panic when calling RawConn() twice")
		}
	}()
	_ = vc.RawConn()
}

func TestConn_CurrentRawConnAndReadWriteNoPanic(t *testing.T) {
	// 这个测试旨在验证并发安全性，通过高频交叉调用保证绝不发生死锁或 Nil 指针 Panic
	for i := 0; i < 500; i++ {
		c1, c2 := pipe()
		vc := New(c1)

		var wg sync.WaitGroup
		wg.Add(3)

		go func() {
			defer wg.Done()
			defer c2.Close()
			for k := 0; k < 5; k++ {
				_, _ = c2.Write([]byte("data"))
				time.Sleep(2 * time.Millisecond)
			}
		}()

		// 协程 1: 并发前台读取
		go func() {
			defer wg.Done()
			buf := make([]byte, 10)
			for {
				_, err := vc.Read(buf)
				if err != nil {
					return
				}
			}
		}()

		// 协程 2: 随机时机剥离原始连接
		go func() {
			defer wg.Done()
			time.Sleep(5 * time.Millisecond)
			defer func() { _ = recover() }() // 忽略由于 RawConn 重复提取导致的预期 Panic
			raw := vc.RawConn()
			if raw != nil {
				_ = raw.Close()
			}
		}()

		wg.Wait()
		_ = vc.Close()
	}
}

func TestConn_CloseNotify(t *testing.T) {
	ln, _ := net.Listen("tcp", "127.0.0.1:0")
	defer ln.Close()

	go func() {
		conn, _ := ln.Accept()
		time.Sleep(100 * time.Millisecond)
		conn.Close() // 模拟远端断开
	}()

	raw, _ := net.Dial("tcp", ln.Addr().String())
	vc := New(raw)

	select {
	case err := <-vc.CloseNotify():
		if err == nil {
			t.Fatal("expected error, got nil")
		}
		t.Logf("Detected expected close: %v", err)
	case <-time.After(2 * time.Second):
		t.Fatal("timeout waiting for close notification")
	}
}

func TestConn_SetReadLimit(t *testing.T) {
	s, c := net.Pipe()
	defer s.Close()
	vc := New(c)
	vc.SetReadLimit(5) // 限制读取 5 字节

	go s.Write([]byte("hello world"))

	buf := make([]byte, 10)
	n, _ := vc.Read(buf)
	if n != 5 || string(buf[:n]) != "hello" {
		t.Fatalf("ReadLimit failed, n=%d, data=%s", n, string(buf[:n]))
	}

	n, err := vc.Read(buf)
	if !errors.Is(err, io.EOF) || n != 0 {
		t.Fatalf("Expected EOF after limit, got %v", err)
	}
	// 配额耗尽必须与「对端关闭」区分开：否则池化代码会为一次配额用尽去重连。
	if k := StatusOf(err); k != StatusQuota {
		t.Fatalf("配额耗尽应归类为 StatusQuota，得到 %v (%v)", k, err)
	}
	select {
	case e := <-vc.CloseNotify():
		t.Fatalf("配额耗尽不应触发关闭通知，却收到 %v", e)
	default:
	}
}

func TestConn_RawConnFull(t *testing.T) {
	s, c := net.Pipe()
	vc := New(c)
	vc.SetBackgroundReadBuffer(1) // net.Pipe 无内核探测，需显式开启预读

	// 启动后台读触发预读
	vc.CloseNotify()
	go s.Write([]byte("A"))

	// 等后台真正读取到 A（周期 200ms，轮询避免固定 sleep 抖动）
	if !waitPeekLen(vc, 1, 2*time.Second) {
		t.Fatal("background peek did not happen in time")
	}

	raw, buf := vc.RawConnFull()
	defer raw.Close()
	if len(buf) != 1 || buf[0] != 'A' {
		t.Fatalf("RawConnFull failed to recover peeked byte: %q", buf)
	}

	// 验证 raw 是否正常工作
	go s.Write([]byte("B"))
	n, err := raw.Read(buf)
	if err != nil || n != 1 || buf[0] != 'B' {
		t.Fatalf("Raw connection read failed: n=%d err=%v buf=%q", n, err, buf)
	}
}

func TestConn_SetBackgroundReadBuffer(t *testing.T) {
	s, c := net.Pipe()
	defer s.Close()

	vc := New(c)
	defer vc.Close()
	vc.SetBackgroundReadBuffer(10) // 设置背景读取缓冲区大小为 10 字节
	vc.CloseNotify()

	s.SetWriteDeadline(time.Now().Add(time.Second))
	n, err := s.Write([]byte("hello world"))
	if n != 10 {
		t.Fatalf("Write failed, n=%d, err=%v", n, err)
	}
	if err == nil {
		t.Fatalf("Write should have failed due to deadline, got %v", err)
	}
	conn, buf := vc.RawConnFull()
	conn.Close()
	if !bytes.Equal(buf, []byte("hello worl")) {
		t.Fatalf("RawConnFull failed to recover peeked bytes: val=%s", string(buf))
	}
}

func TestResolvePeekBlock(t *testing.T) {
	tcp, _ := net.Pipe() // 非 syscall.Conn：direct=false
	defer tcp.Close()
	raw := &directConn{} // 见下，模拟 syscall.Conn

	cases := []struct {
		name       string
		c          net.Conn
		res        probeResult
		allowBlock bool
		wantSkip   bool
		wantBlock  bool
	}{
		{"readable-direct", raw, probeReadable, false, false, false},
		{"readable-wrap", tcp, probeReadable, true, true, false},
		{"idle-direct-allow", raw, probeIdle, true, false, true},
		{"idle-direct-noallow", raw, probeIdle, false, true, false},
		{"idle-wrap", tcp, probeIdle, true, true, false},
		{"unsupported", tcp, probeUnsupported, false, false, true},
	}
	for _, tc := range cases {
		skip, block := resolvePeekBlock(tc.c, tc.res, tc.allowBlock)
		if skip != tc.wantSkip || block != tc.wantBlock {
			t.Errorf("%s: got (skip=%v, block=%v), want (%v, %v)",
				tc.name, skip, block, tc.wantSkip, tc.wantBlock)
		}
	}
}

func TestCheckBlocking_ProbeTimeout(t *testing.T) {
	a, b := net.Pipe() // 非 syscall.Conn，走阻塞预读
	defer a.Close()
	defer b.Close()
	c := New(a)

	start := time.Now()
	st, err := c.CheckBlocking() // 对端静默
	elapsed := time.Since(start)

	if err != nil || st.Class() != StatusAlive {
		t.Fatalf("silent peer should be alive: (%v, %v)", st, err)
	}
	// 这里断言的是「绝不无限等待」这一不变量，而不是精确的 2ms：
	// Windows 的系统 timer 粒度约 15.6ms，且负载下调度延迟可达数十毫秒，
	// 按 2ms 量级断言会在并行跑其它测试时随机失败（无超时的实现则会直接挂到测试超时）。
	if elapsed > 250*time.Millisecond {
		t.Fatalf("probe too slow: %v", elapsed)
	}
	if elapsed < 500*time.Microsecond {
		t.Fatalf("probe returned too fast, did it actually wait? %v", elapsed)
	}
}

// directConn 仅用于测试：让 isDirectSyscallConn 返回 true
type directConn struct{ net.Conn }

func (d *directConn) SyscallConn() (syscall.RawConn, error) { return nil, nil }

// 端到端：net.Pipe（内核探测不可用 → 走 peekRead 分支）验证哨兵 + 数据不丢
func TestNotifyHangupSentinel(t *testing.T) {
	cli, srv := net.Pipe()
	c := New(cli)
	defer c.Close()
	c.SetBackgroundReadBuffer(64)
	c.SetMonitorInterval(20 * time.Millisecond)

	ch := c.CloseNotify()
	go func() {
		_, _ = srv.Write([]byte("hello"))
		_ = srv.Close() // 对端结束，但字节还压在预读缓冲里
	}()

	select {
	case err := <-ch:
		if !errors.Is(err, io.EOF) {
			t.Fatalf("必须保持 io.EOF 兼容，got %v", err)
		}
		if !errors.Is(err, ErrPeerClosedWithUnread) {
			t.Fatalf("want hangup sentinel, got %v", err)
		}
	case <-time.After(3 * time.Second):
		t.Fatal("没有收到关闭通知")
	}

	// 数据不得因通知而丢失，且 Read 的返回值必须是裸 io.EOF
	buf := make([]byte, 16)
	if n, err := io.ReadFull(c, buf[:5]); err != nil || string(buf[:n]) != "hello" {
		t.Fatalf("预读数据丢失: n=%d err=%v", n, err)
	}
	if _, err := c.Read(buf); !errors.Is(err, io.EOF) || errors.Is(err, ErrPeerClosedWithUnread) {
		t.Fatalf("Read 不应泄漏哨兵, got %v", err)
	}
}

// 端到端：预读把内核队列搬空后再收到 FIN，通知必须带哨兵，且数据仍可读
func TestNotifyHangupWithPeekBuffer(t *testing.T) {
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	defer ln.Close()

	go func() {
		c, _ := ln.Accept()
		defer c.Close()
		_, _ = c.Write([]byte("payload"))
	}()

	raw, err := net.Dial("tcp", ln.Addr().String())
	if err != nil {
		t.Fatal(err)
	}
	c := New(raw)
	defer c.Close()
	c.SetBackgroundReadBuffer(1024) // 关键：让预读把内核队列搬空
	c.SetMonitorInterval(20 * time.Millisecond)

	ch := c.CloseNotify()
	select {
	case err := <-ch:
		if !errors.Is(err, io.EOF) {
			t.Fatalf("必须保持 io.EOF 兼容，got %v", err)
		}
		if !errors.Is(err, ErrPeerClosedWithUnread) {
			t.Fatalf("want hangup sentinel, got %v", err)
		}
	case <-time.After(3 * time.Second):
		t.Fatal("没有收到关闭通知")
	}

	buf := make([]byte, 32)
	if n, err := io.ReadFull(c, buf[:7]); err != nil || string(buf[:n]) != "payload" {
		t.Fatalf("数据丢失: n=%d err=%v", n, err)
	}
	// 读空后 Read 必须是裸 io.EOF，不带哨兵
	if _, err := c.Read(buf); !errors.Is(err, io.EOF) || errors.Is(err, ErrPeerClosedWithUnread) {
		t.Fatalf("Read 泄漏哨兵: %v", err)
	}
}
