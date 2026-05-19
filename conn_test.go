package vconn

import (
	"bytes"
	"errors"
	"io"
	"net"
	"sync"
	"testing"

	//"fmt"
	"time"
)

type Fatal func(args ...interface{})

// 测试客户端关闭，服务端收到io.EOF
func Test_conn_Read_1(t *testing.T) {
	exit := make(chan int, 1)
	// 监听端
	netListener, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	defer netListener.Close()

	fatal := t.Fatal
	go func() {
		netConn, err := netListener.Accept()
		if err != nil {
			fatal(err)
		}

		netConn = New(netConn)
		go func(nc CloseNotifier) {
			// 连接关闭事件
			err := <-nc.CloseNotify()
			if err != io.EOF {
				fatal(err)
			}
			exit <- 1
		}(netConn.(CloseNotifier))

		if _, err = io.Copy(io.Discard, netConn); err != nil {
			fatal(err)
		}
		netConn.Close()
	}()

	// 发送端
	netAddr := netListener.Addr()
	netConn, err := net.Dial(netAddr.Network(), netAddr.String())
	if err != nil {
		t.Fatal(err)
	}
	netConn.Write([]byte("123456"))
	netConn.Close()
	<-exit
}

// 测试客户端关闭，服务端收到io.EOF
func TestConn_Read_2(t *testing.T) {
	exit := make(chan int, 1)
	// 监听端
	netListener, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	defer netListener.Close()

	fatal := t.Fatal
	go func() {
		netConn, err := netListener.Accept()
		if err != nil {
			fatal(err)
		}
		defer netConn.Close()

		netConn = NewConn(netConn)
		go func() {
			p := make([]byte, 8)
			for {
				n, err := netConn.Read(p)
				if err != nil {
					if err != io.EOF {
						fatal(err)
					}
					break
				} else if n == 0 {
					fatal("unexpected zero read")
					break
				}
			}
			exit <- 1
		}()

		// 连接关闭事件
		err = <-netConn.(CloseNotifier).CloseNotify()
		if err != io.EOF {
			fatal(err)
		}
	}()

	// 发送端
	netAddr := netListener.Addr()
	netConn, err := net.Dial(netAddr.Network(), netAddr.String())
	if err != nil {
		t.Fatal(err)
	}
	netConn.Write([]byte("1234567890"))
	netConn.Close()
	<-exit
}

// 测试客户端关闭，服务端收到io.EOF
// 后台读取丢弃
func Test_conn_SetBackgroundReadDiscard(t *testing.T) {
	exit := make(chan int, 1)
	// 监听端
	netListener, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	defer netListener.Close()

	fatal := t.Fatal
	go func() {
		netConn, err := netListener.Accept()
		if err != nil {
			fatal(err)
		}

		defer netConn.Close()
		netConn = New(netConn)
		netConn.(*Conn).SetBackgroundReadDiscard(true)

		// 连接关闭事件
		err = <-netConn.(CloseNotifier).CloseNotify()
		if err != io.EOF {
			fatal(err)
		}
		exit <- 1
	}()

	// 发送端
	netAddr := netListener.Addr()
	netConn, err := net.Dial(netAddr.Network(), netAddr.String())
	if err != nil {
		t.Fatal(err)
	}
	netConn.Write([]byte("12345678901234567890123456789012345678901234567890123456789012345678901234567890123456789012345678901234567890123456789012345678901234567890"))
	netConn.Close()
	<-exit
}

// 测试客户端关闭，服务端收到io.EOF
// 不允许后台读取
func TestConn_DisableBackgroundRead(t *testing.T) {
	exit := make(chan int, 1)
	// 监听端
	netListener, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	defer netListener.Close()

	fatal := t.Fatal
	p := []byte("12345678901234567890123456789012345678901234567890123456789012345678901234567890123456789012345678901234567890123456789012345678901234567890")
	go func() {
		netConn, err := netListener.Accept()
		if err != nil {
			fatal(err)
		}

		netConn = New(netConn)
		netConn.(*Conn).DisableBackgroundRead(true)
		defer netConn.Close()

		go func() {
			// 连接关闭事件
			err := <-netConn.(CloseNotifier).CloseNotify()
			if err != io.EOF {
				fatal(err)
			}
			exit <- 1
		}()

		time.Sleep(time.Second)

		buf := bytes.NewBuffer(nil)
		if _, err := buf.ReadFrom(netConn); err != nil {
			fatal(err)
		}
		if !bytes.Equal(buf.Bytes(), p) {
			fatal("error")
		}
	}()

	// 发送端
	netAddr := netListener.Addr()
	netConn, err := net.Dial(netAddr.Network(), netAddr.String())
	if err != nil {
		t.Fatal(err)
	}
	netConn.Write(p)
	netConn.Close()
	<-exit
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
			if e != io.EOF {
				t.Errorf("expected EOF, got %v", e)
			}
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

	go func() {
		c, _ := ln.Accept()
		vc := New(c)
		ch := vc.CloseNotify() // start background
		time.Sleep(100 * time.Millisecond)
		raw, peeked := vc.RawConnFull(make([]byte, 1))
		if peeked != 1 {
			t.Error("unexpected peek")
		}
		defer raw.Close()
		// 等待后台读结束
		select {
		case e := <-ch:
			if e != net.ErrClosed {
				t.Errorf("expected close, got %v", e)
			}
		case <-time.After(2 * time.Second):
			t.Error("timeout")
		}
	}()

	c, _ := net.Dial("tcp", ln.Addr().String())
	c.Write([]byte{1, 2, 3})
	defer c.Close()
	time.Sleep(200 * time.Millisecond)
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

	if !vc.IsClosed() {
		// 虽收到通知，但连接可能需要显式 Close 释放本地句柄或已由底层联动关闭
		_ = vc.Close()
	}
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

	// 验证包装连接的状态
	if !vc.IsClosed() {
		t.Error("Expected vconn to mark itself as closed after RawConn extraction")
	}

	select {
	case err := <-notify:
		if !errors.Is(err, net.ErrClosed) {
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
	_ = vc.CloseNotify() // 触发后台预读

	// 给后台足够时间预读这一个字节
	go func() {
		_, _ = c2.Write([]byte("X"))
	}()
	time.Sleep(100 * time.Millisecond)

	out := make([]byte, 1)
	raw, peeked := vc.RawConnFull(out)
	defer raw.Close()

	if peeked != 1 || out[0] != 'X' {
		t.Errorf("Failed to capture peeked byte. Got peeked=%d, byte=%q", peeked, out[0])
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
	if err != io.EOF || n != 0 {
		t.Fatalf("Expected EOF after limit, got %v", err)
	}
}

func TestConn_RawConnFull(t *testing.T) {
	s, c := net.Pipe()
	vc := New(c)

	// 启动后台读触发预读
	vc.CloseNotify()
	go s.Write([]byte("A"))

	// 给一点点时间让后台协程读取到 A
	time.Sleep(50 * time.Millisecond)

	buf := make([]byte, 10)
	raw, n := vc.RawConnFull(buf)
	if n != 1 || buf[0] != 'A' {
		t.Fatalf("RawConnFull failed to recover peeked byte: n=%d, val=%c", n, buf[0])
	}

	// 验证 raw 是否正常工作
	go s.Write([]byte("B"))
	raw.Read(buf)
	if buf[0] != 'B' {
		t.Fatalf("Raw connection read failed")
	}
}
