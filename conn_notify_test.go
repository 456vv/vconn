package vconn

import (
	"context"
	"errors"
	"io"
	"net"
	"testing"
	"time"
)

// TestNotify_PeerClose 表驱动：不同底层 × 不同预读模式下，「对端结束」都必须被提前通知，
// 且数据一个字节都不许丢、读空后 Read 必须是裸 io.EOF。
func TestNotify_PeerClose(t *testing.T) {
	const (
		transTCP  = "tcp"
		transPipe = "pipe"
	)
	cases := []struct {
		name       string
		transport  string
		background int    // SetBackgroundReadBuffer 参数（0 表示不设置）
		payload    string // 对端先发再关
		wantUnread bool   // 通知是否应携带 ErrPeerClosedWithUnread
	}{
		{"TCP/干净 FIN", transTCP, 0, "", false},
		{"TCP/数据后 FIN", transTCP, 0, "hello", true},
		{"pipe/预读检测 FIN", transPipe, 1, "", false},
		{"pipe/数据后 FIN 带哨兵", transPipe, 64, "hello", true},
		{"pipe/丢弃模式", transPipe, -1, "0123456789", false},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			var (
				vc   *Conn
				peer net.Conn
				stop func()
			)
			if tc.transport == transTCP {
				var ln net.Listener
				vc, peer, ln = tcpPair(t)
				stop = func() { _ = ln.Close() }
			} else {
				c1, c2 := pipePair()
				vc, peer = New(c1), c2
				stop = func() { _ = c1.Close() }
			}
			defer stop()
			defer vc.Close()

			if tc.background != 0 {
				vc.SetBackgroundReadBuffer(tc.background)
			}
			vc.SetMonitorInterval(20 * time.Millisecond)
			ch := vc.CloseNotify()

			if tc.payload != "" {
				if _, err := peer.Write([]byte(tc.payload)); err != nil {
					t.Fatalf("对端写入: %v", err)
				}
				if tc.transport == transTCP {
					time.Sleep(20 * time.Millisecond) // 让数据落到接收队列
				}
			}
			if err := peer.Close(); err != nil {
				t.Fatalf("对端关闭: %v", err)
			}

			err := awaitErr(t, ch, "对端结束")
			if k := StatusOf(err); k != StatusPeerClosed {
				t.Fatalf("通知应归类为 StatusPeerClosed，得到 %v (%v)", k, err)
			}
			if got := errors.Is(err, ErrPeerClosedWithUnread); got != tc.wantUnread {
				t.Fatalf("哨兵期望=%v 实际=%v（%v）", tc.wantUnread, got, err)
			}
			if !vc.IsPeerClosed() {
				t.Fatal("通知落定后 IsPeerClosed 应为 true")
			}

			// 数据不得因通知而丢失（丢弃模式是用户显式要求丢，跳过）
			if tc.payload != "" && tc.background >= 0 {
				buf := make([]byte, len(tc.payload))
				vc.SetReadDeadline(time.Now().Add(time.Second))
				if n, rerr := io.ReadFull(vc, buf); rerr != nil || string(buf[:n]) != tc.payload {
					t.Fatalf("数据丢失: n=%d data=%q err=%v", n, buf[:n], rerr)
				}
				if _, rerr := vc.Read(buf); !errors.Is(rerr, io.EOF) ||
					errors.Is(rerr, ErrPeerClosedWithUnread) {
					t.Fatalf("读空后应返回裸 io.EOF，得到 %v", rerr)
				}
			}
		})
	}
}

// TestNotify_LocalClose 本地关闭也必须通知，且重复 Close 幂等
func TestNotify_LocalClose(t *testing.T) {
	t.Run("Close 通知 net.ErrClosed", func(t *testing.T) {
		vc, srv, ln := tcpPair(t)
		defer ln.Close()
		defer srv.Close()

		ch := vc.CloseNotify()
		if err := vc.Close(); err != nil {
			t.Fatalf("Close: %v", err)
		}
		err := awaitErr(t, ch, "本地关闭")
		if !errors.Is(err, net.ErrClosed) || StatusOf(err) != StatusLocalClosed {
			t.Fatalf("通知应为 net.ErrClosed/StatusLocalClosed，得到 %v (%v)", err, StatusOf(err))
		}
		if err := vc.Close(); err != nil {
			t.Fatalf("重复 Close 应返回 nil，得到 %v", err)
		}
	})

	t.Run("Close 失败也照常通知", func(t *testing.T) {
		s1, s2 := pipePair()
		defer s2.Close()

		boom := errors.New("close boom")
		vc := New(&closeErrConn{Conn: s1, err: boom})
		ch := vc.CloseNotify()

		if err := vc.Close(); !errors.Is(err, boom) {
			t.Fatalf("Close 应返回底层错误，得到 %v", err)
		}
		if err := awaitErr(t, ch, "关闭失败"); !errors.Is(err, net.ErrClosed) {
			t.Fatalf("通知应为 net.ErrClosed，得到 %v", err)
		}
	})
}

// TestNotify_CancelNotify CancelNotify 的语义与安全边界
func TestNotify_CancelNotify(t *testing.T) {
	t.Run("nil 规整为哨兵且仅首次生效", func(t *testing.T) {
		s1, s2 := pipePair()
		defer s2.Close()

		vc := New(s1)
		defer vc.Close()

		if !vc.CancelNotify(nil) {
			t.Fatal("首次 CancelNotify 应返回 true")
		}
		if vc.CancelNotify(nil) {
			t.Fatal("重复 CancelNotify 应返回 false")
		}
		err := awaitErr(t, vc.CloseNotify(), "CancelNotify(nil)")
		if !errors.Is(err, ErrNotifyCanceled) || StatusOf(err) != StatusLatched {
			t.Fatalf("应为 ErrNotifyCanceled/StatusLatched，得到 %v (%v)", err, StatusOf(err))
		}
	})

	t.Run("不可比较的 error 不 panic", func(t *testing.T) {
		s1, s2 := pipePair()
		defer s2.Close()

		vc := New(s1)
		defer vc.Close()

		defer func() {
			if r := recover(); r != nil {
				t.Fatalf("不可比较的 error 触发 panic: %v", r)
			}
		}()
		vc.CancelNotify(uncomparableErr{D: []byte{1}})
		vc.CancelNotify(uncomparableErr{D: []byte{2}})
		vc.CancelNotify(net.ErrClosed)
	})

	t.Run("所有订阅者都收到", func(t *testing.T) {
		s1, s2 := pipePair()
		defer s2.Close()

		vc := New(s1)
		defer vc.Close()

		const n = 16
		chans := make([]<-chan error, n)
		for i := range chans {
			chans[i] = vc.CloseNotify()
		}
		if !vc.CancelNotify(errors.New("boom")) {
			t.Fatal("首次 CancelNotify 应返回 true")
		}
		for i, ch := range chans {
			if err := awaitErr(t, ch, "订阅者"); err == nil || err.Error() != "boom" {
				t.Fatalf("第 %d 个订阅者得到 %v", i, err)
			}
		}
	})

	t.Run("手动通知不标记 peerDown", func(t *testing.T) {
		s1, s2 := pipePair()
		defer s2.Close()

		vc := New(s1)
		defer vc.Close()

		vc.CloseNotify()
		vc.CancelNotify(errors.New("manual"))
		if vc.IsPeerClosed() {
			t.Fatal("CancelNotify 不应把连接标记为对端断开")
		}
		if st := vc.Snapshot(); st != StatusLatched {
			t.Fatalf("快照应为 StatusLatched，得到 %v", st)
		}
	})
}

// TestWatchClose 订阅/退订/落定后再订阅
func TestWatchClose(t *testing.T) {
	t.Run("退订后无残留", func(t *testing.T) {
		s1, s2 := pipePair()
		defer s2.Close()

		vc := New(s1)
		defer vc.Close()

		ch, cancel := vc.WatchClose()
		if n := watcherCount(vc); n != 1 {
			t.Fatalf("期望 1 个订阅者，得到 %d", n)
		}
		cancel()
		if n := watcherCount(vc); n != 0 {
			t.Fatalf("退订后残留 %d 个订阅者", n)
		}
		select {
		case _, ok := <-ch:
			if ok {
				t.Fatal("退订后 channel 应被关闭")
			}
		case <-time.After(time.Second):
			t.Fatal("退订后 channel 未关闭")
		}
	})

	t.Run("反复订阅退订不泄漏", func(t *testing.T) {
		s1, s2 := pipePair()
		defer s2.Close()

		vc := New(s1)
		defer vc.Close()

		for i := 0; i < 100; i++ {
			_, cancel := vc.WatchClose()
			cancel()
		}
		if n := watcherCount(vc); n != 0 {
			t.Fatalf("反复订阅/退订后残留 %d 个订阅者", n)
		}
	})

	t.Run("落定后再订阅立即完成", func(t *testing.T) {
		s1, s2 := pipePair()
		defer s2.Close()

		vc := New(s1)
		defer vc.Close()

		vc.CancelNotify(errors.New("boom"))
		if err := awaitErr(t, vc.CloseNotify(), "落定后再订阅"); err == nil || err.Error() != "boom" {
			t.Fatalf("应立即得到落定原因，得到 %v", err)
		}
		_, cancel := vc.WatchClose() // 已落定：退订函数是 no-op，但不得 panic
		cancel()
	})
}

// TestNotify_WithContext 上下文集成：只通知 vs 取消即关闭
func TestNotify_WithContext(t *testing.T) {
	t.Run("WithContext 只通知不关闭", func(t *testing.T) {
		s1, s2 := pipePair()
		defer s2.Close()

		vc := New(s1)
		defer vc.Close()

		ctx, cancel := context.WithCancel(context.Background())
		vc.WithContext(ctx)

		cancel()
		waitCond(t, "上下文取消后落定通知", 2*time.Second, func() bool { return vc.Err() != nil })
		if k := StatusOf(vc.Err()); k != StatusLatched {
			t.Fatalf("应归类为 StatusLatched，得到 %v (%v)", k, vc.Err())
		}

		go func() { _, _ = s2.Write([]byte("ok")) }()
		buf := make([]byte, 2)
		vc.SetReadDeadline(time.Now().Add(time.Second))
		if n, err := vc.Read(buf); err != nil || string(buf[:n]) != "ok" {
			t.Fatalf("连接应保持可用: n=%d err=%v", n, err)
		}
	})

	t.Run("WithContextClose 取消即关闭并解阻塞", func(t *testing.T) {
		s1, s2 := pipePair()
		defer s2.Close()

		vc := New(s1)
		defer vc.Close()

		ctx, cancel := context.WithCancel(context.Background())
		vc.WithContextClose(ctx)

		errc := make(chan error, 1)
		go func() {
			buf := make([]byte, 8)
			_, err := vc.Read(buf)
			errc <- err
		}()
		time.Sleep(30 * time.Millisecond)
		cancel()

		select {
		case err := <-errc:
			if err == nil {
				t.Fatal("阻塞 Read 应返回错误")
			}
		case <-time.After(2 * time.Second):
			t.Fatal("上下文取消未解阻塞 Read")
		}
		waitCond(t, "关闭落定", 2*time.Second, func() bool { return vc.Err() != nil })
		if k := StatusOf(vc.Err()); k != StatusLocalClosed {
			t.Fatalf("应归类为 StatusLocalClosed，得到 %v (%v)", k, vc.Err())
		}
	})
}

// TestNotify_NoSpurious 健康连接不得误报
func TestNotify_NoSpurious(t *testing.T) {
	t.Run("空闲连接不误报", func(t *testing.T) {
		vc, srv, ln := tcpPair(t)
		defer ln.Close()
		defer srv.Close()
		defer vc.Close()

		vc.SetMonitorInterval(20 * time.Millisecond)
		ch := vc.CloseNotify()
		assertNoNotify(t, ch, 300*time.Millisecond, "空闲连接")

		if _, err := srv.Write([]byte("ping")); err != nil {
			t.Fatal(err)
		}
		vc.SetReadDeadline(time.Now().Add(2 * time.Second))
		buf := make([]byte, 8)
		if n, err := vc.Read(buf); err != nil || string(buf[:n]) != "ping" {
			t.Fatalf("误判后连接仍应可用: n=%d err=%v", n, err)
		}
	})

	t.Run("活跃连接不误报", func(t *testing.T) {
		vc, srv, ln := tcpPair(t)
		defer ln.Close()
		defer srv.Close()
		defer vc.Close()

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
		deadline := time.Now().Add(300 * time.Millisecond)
		for time.Now().Before(deadline) {
			if _, err := vc.Read(buf); err != nil {
				t.Fatalf("活跃连接读取失败: %v", err)
			}
			assertNoPending(t, ch, "活跃连接")
		}
		close(stop)
	})
}
