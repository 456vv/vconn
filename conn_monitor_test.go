package vconn

import (
	"runtime"
	"testing"
	"time"
)

// TestMonitor_GoroutineStable 不再「每次 Read 起一个协程」
func TestMonitor_GoroutineStable(t *testing.T) {
	vc, srv, ln := tcpPair(t)
	defer ln.Close()
	defer srv.Close()
	defer vc.Close()

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
	for i := 0; i < 300; i++ { // 预热
		if _, err := vc.Read(buf); err != nil {
			t.Fatal(err)
		}
	}
	base := runtime.NumGoroutine()
	for i := 0; i < 2000; i++ {
		if _, err := vc.Read(buf); err != nil {
			t.Fatal(err)
		}
	}
	if after := runtime.NumGoroutine(); after > base+8 {
		t.Fatalf("goroutine 数量异常增长: %d -> %d", base, after)
	}
}

// TestMonitor_Lifecycle 监控实例生命周期
func TestMonitor_Lifecycle(t *testing.T) {
	t.Run("无订阅者时不起监控", func(t *testing.T) {
		vc, srv, ln := tcpPair(t)
		defer ln.Close()
		defer srv.Close()
		defer vc.Close()

		if vc.runningMonitor() != nil {
			t.Fatal("未订阅时不应有监控协程")
		}
	})

	t.Run("自行退出后清空实例", func(t *testing.T) {
		vc, srv, ln := tcpPair(t)
		defer ln.Close()
		defer vc.Close()

		vc.SetMonitorInterval(10 * time.Millisecond)
		ch := vc.CloseNotify()

		srv.Close()
		if err := awaitErr(t, ch, "对端关闭"); err == nil {
			t.Fatal("应携带关闭原因")
		}
		waitCond(t, "自行退出的监控实例被清理", 2*time.Second, func() bool {
			return vc.mon.Load() == nil
		})
	})

	t.Run("停监控不写多余 deadline", func(t *testing.T) {
		s1, s2 := pipePair()
		defer s2.Close()

		dc := &deadlineCounter{Conn: s1}
		vc := New(dc)
		_ = vc.CloseNotify() // peekSize=0，不会进入阻塞预读

		time.Sleep(30 * time.Millisecond)
		before := dc.n.Load()
		vc.stopMonitor()
		if after := dc.n.Load(); after != before {
			t.Fatalf("停监控时多写了 %d 次 ReadDeadline", after-before)
		}
		vc.Close()
	})
}

// TestPeekBuffer 预读缓冲：长度契约、丢弃模式、deadline 恢复
func TestPeekBuffer(t *testing.T) {
	t.Run("tmpBuf 长度契约", func(t *testing.T) {
		c1, c2 := pipePair()
		defer c2.Close()

		c := New(c1)
		defer c.Close()

		cases := []struct{ want, exp int }{
			{1, 1},
			{10, 10},
			{maxPeekSize, maxPeekSize},
			{maxPeekSize + 4096, maxPeekSize},
			{0, minPeekBuf},
			{-1, minPeekBuf},
			{-minPeekBuf, minPeekBuf},
			{-minPeekBuf - 1, minPeekBuf + 1},
			{-8192, 8192},
			{-maxPeekSize, maxPeekSize},
			{-(maxPeekSize + 4096), maxPeekSize},
		}
		for _, tc := range cases {
			if got := len(c.r.tmpBuf(tc.want)); got != tc.exp {
				t.Errorf("tmpBuf(%d) len = %d, want %d", tc.want, got, tc.exp)
			}
		}
	})

	t.Run("丢弃模式按 |n| 分配", func(t *testing.T) {
		c1, c2 := pipePair()
		defer c2.Close()

		c := New(c1)
		defer c.Close()

		cases := []struct{ size, exp int }{
			{-1, minPeekBuf},
			{-minPeekBuf - 1, minPeekBuf + 1},
			{-8192, 8192},
		}
		for _, tc := range cases {
			c.r.peekTmp = nil
			c.SetBackgroundReadBuffer(tc.size)

			closed, err := c.r.peekStep(nil, probeUnsupported, false)
			if closed || err != nil {
				t.Fatalf("peekSize=%d: closed=%v err=%v", tc.size, closed, err)
			}
			if got := cap(c.r.peekTmp); got != tc.exp {
				t.Errorf("peekSize=%d: cap(peekTmp) = %d, want %d", tc.size, got, tc.exp)
			}
		}
	})

	t.Run("后台预读不污染用户 deadline", func(t *testing.T) {
		s1, s2 := pipePair()
		defer s2.Close()

		vc := New(s1)
		defer vc.Close()

		vc.SetBackgroundReadBuffer(4)
		vc.CloseNotify()
		vc.SetReadDeadline(time.Now().Add(120 * time.Millisecond))

		go func() { _, _ = s2.Write([]byte("ab")) }()
		time.Sleep(60 * time.Millisecond)

		buf := make([]byte, 16)
		start := time.Now()
		if n, err := vc.Read(buf); n != 2 || err != nil {
			t.Fatalf("期望 (2,nil)，得到 (%d,%v)", n, err)
		}
		if d := time.Since(start); d > 400*time.Millisecond {
			t.Fatalf("首读耗时异常: %v", d)
		}

		start = time.Now()
		if _, err := vc.Read(buf); err == nil {
			t.Fatal("用户设置的 ReadDeadline 已被后台预读抹掉")
		}
		if d := time.Since(start); d > time.Second {
			t.Fatalf("超时未生效，阻塞了 %v", d)
		}
	})
}
