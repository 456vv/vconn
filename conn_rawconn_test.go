package vconn

import (
	"errors"
	"net"
	"testing"
	"time"
)

// TestRawConn_Transfer 所有权转移：包装失效、通知落定、原始连接可用
func TestRawConn_Transfer(t *testing.T) {
	c1, c2 := pipePair()
	defer c2.Close()

	vc := New(c1)
	ch := vc.CloseNotify()

	raw := vc.RawConn()
	if raw == nil {
		t.Fatal("RawConn 返回 nil")
	}

	select {
	case err := <-ch:
		if !errors.Is(err, ErrInUse) {
			t.Fatalf("通知应为 ErrInUse，得到 %v", err)
		}
	default:
		t.Fatal("提取原始连接后应同步分发通知")
	}
	if k := StatusOf(vc.Err()); k != StatusInUse {
		t.Fatalf("Err() 应归类为 StatusInUse，得到 %v", k)
	}

	buf := make([]byte, 4)
	if _, err := vc.Read(buf); !errors.Is(err, net.ErrClosed) {
		t.Fatalf("提取后 Read 应返回 net.ErrClosed，得到 %v", err)
	}
	if _, err := vc.Write(buf); !errors.Is(err, net.ErrClosed) {
		t.Fatalf("提取后 Write 应返回 net.ErrClosed，得到 %v", err)
	}

	go func() { _, _ = c2.Write([]byte("raw")) }()
	_ = raw.SetReadDeadline(time.Now().Add(time.Second))
	if n, err := raw.Read(buf); err != nil || string(buf[:n]) != "raw" {
		t.Fatalf("原始连接不可用: n=%d err=%v", n, err)
	}
	_ = raw.Close()
}

// TestRawConn_NotClosedByConn 提取后包装连接不得再关闭底层
func TestRawConn_NotClosedByConn(t *testing.T) {
	s1, s2 := pipePair()
	defer s2.Close()

	fc := &countingCloseConn{Conn: s1}
	vc := New(fc)

	raw := vc.RawConn()
	if err := vc.Close(); err != nil {
		t.Fatalf("RawConn 后 Close 应返回 nil，得到 %v", err)
	}
	if n := fc.countValue(); n != 0 {
		t.Fatalf("RawConn 后 Close 仍关闭了底层 %d 次", n)
	}
	_ = raw.Close()
}

// TestRawConn_PanicOnRepeat 重复提取必须 panic
func TestRawConn_PanicOnRepeat(t *testing.T) {
	c1, c2 := pipePair()
	defer c2.Close()

	vc := New(c1)
	_ = vc.RawConn()

	defer func() {
		if r := recover(); r == nil {
			t.Fatal("重复 RawConn 应 panic")
		}
	}()
	_ = vc.RawConn()
}

// TestRawConnFull 归还原始连接 + 预读字节
func TestRawConnFull(t *testing.T) {
	t.Run("归还预读字节", func(t *testing.T) {
		s, c := pipePair()
		defer s.Close()

		vc := New(c)
		vc.SetBackgroundReadBuffer(1)
		_ = vc.CloseNotify()

		go func() { _, _ = s.Write([]byte("X")) }()
		if !waitPeekLen(vc, 1, 2*time.Second) {
			t.Fatal("后台预读未在时限内拿到字节")
		}

		raw, peeked := vc.RawConnFull()
		defer raw.Close()
		if len(peeked) != 1 || peeked[0] != 'X' {
			t.Fatalf("预读字节未归还: %q", peeked)
		}

		go func() { _, _ = s.Write([]byte("Y")) }()
		_ = raw.SetReadDeadline(time.Now().Add(time.Second))
		buf := make([]byte, 1)
		if n, err := raw.Read(buf); err != nil || n != 1 || buf[0] != 'Y' {
			t.Fatalf("提取后的原始连接不可用: n=%d data=%q err=%v", n, buf[:n], err)
		}
	})

	t.Run("无预读时返回 nil", func(t *testing.T) {
		vc, srv, ln := tcpPair(t)
		defer ln.Close()
		defer srv.Close()

		raw, peeked := vc.RawConnFull()
		defer raw.Close()
		if len(peeked) != 0 {
			t.Fatalf("不应有预读字节: %q", peeked)
		}
	})

	t.Run("唤醒阻塞中的前台读", func(t *testing.T) {
		s1, s2 := pipePair()
		defer s2.Close()

		vc := New(s1)
		readDone := make(chan error, 1)
		go func() {
			buf := make([]byte, 1)
			_, err := vc.Read(buf)
			readDone <- err
		}()
		time.Sleep(20 * time.Millisecond)

		type result struct {
			raw  net.Conn
			data []byte
		}
		resCh := make(chan result, 1)
		go func() {
			raw, data := vc.RawConnFull()
			resCh <- result{raw: raw, data: data}
		}()

		select {
		case res := <-resCh:
			if res.raw == nil {
				t.Fatal("RawConnFull 返回 nil")
			}
			_ = res.raw.Close()
		case <-time.After(2 * time.Second):
			t.Fatal("RawConnFull 被阻塞的前台 Read 卡住")
		}

		select {
		case err := <-readDone:
			if err == nil {
				t.Fatal("被 deadline 唤醒的 Read 应返回错误")
			}
		case <-time.After(time.Second):
			t.Fatal("前台 Read 未被唤醒")
		}
	})

	t.Run("高频预读下不丢字节", func(t *testing.T) {
		vc, srv, ln := tcpPair(t)
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

		time.Sleep(80 * time.Millisecond) // 制造「提取时有在途预读」的窗口

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
			t.Fatalf("提取原始连接时丢字节：得到 %d，期望 %d（预读 %d）", len(got), total, len(peeked))
		}
		for i := range got {
			if got[i] != byte(i) {
				t.Fatalf("字节顺序错误 @%d: got %d want %d", i, got[i], byte(i))
			}
		}
	})
}
