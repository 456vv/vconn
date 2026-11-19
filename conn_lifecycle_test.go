package vconn

import (
	"bytes"
	"errors"
	"io"
	"net"
	"sync"
	"sync/atomic"
	"testing"
	"time"
)

// TestConn_ReadWrite 基础读写与双向流动
func TestConn_ReadWrite(t *testing.T) {
	c1, c2 := pipePair()
	defer c1.Close()
	defer c2.Close()

	vc := New(c1)
	payload := []byte("hello virtual connection")
	go func() { _, _ = c2.Write(payload) }()

	buf := make([]byte, len(payload))
	if n, err := io.ReadFull(vc, buf); err != nil || !bytes.Equal(buf[:n], payload) {
		t.Fatalf("ReadFull = (%d, %v) data=%q", n, err, buf[:n])
	}

	got := make([]byte, 4)
	go func() {
		if _, werr := vc.Write([]byte("pong")); werr != nil {
			t.Errorf("Write: %v", werr)
		}
	}()
	if n, err := io.ReadFull(c2, got); err != nil || string(got[:n]) != "pong" {
		t.Fatalf("对端读到 (%d, %v) %q", n, err, got[:n])
	}
}

// TestConn_ReadLimit 表驱动：预读字节计入额度、单次截断、limit=0、limit<0
func TestConn_ReadLimit(t *testing.T) {
	cases := []struct {
		name      string
		limit     int64
		seed      string
		sockData  string
		chunk     int
		wantTotal int
		wantQuota bool
	}{
		{"预读字节也计入额度", 10, "12345678", "abcdefgh", 64, 10, true},
		{"单次读被截断到剩余额度", 5, "", "abcdefgh", 10, 5, true},
		{"limit=0 立即配额耗尽", 0, "", "abcdefgh", 8, 0, true},
		{"limit<0 不限制", -1, "ab", "123456", 8, 8, false},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			s1, s2 := pipePair()
			defer s2.Close()

			vc := NewData(s1, []byte(tc.seed))
			defer vc.Close()
			vc.SetReadLimit(tc.limit)

			go func() { _, _ = s2.Write([]byte(tc.sockData)) }()

			buf := make([]byte, tc.chunk)
			total := 0
			for total < tc.wantTotal {
				n, err := vc.Read(buf)
				total += n
				if err != nil || n == 0 {
					break
				}
			}
			if total != tc.wantTotal {
				t.Fatalf("共读到 %d 字节，期望 %d", total, tc.wantTotal)
			}
			if !tc.wantQuota {
				return
			}

			_, err := vc.Read(buf)
			if k := StatusOf(err); k != StatusQuota {
				t.Fatalf("配额耗尽应归类为 StatusQuota，得到 %v (%v)", k, err)
			}
			if !errors.Is(err, ErrQuotaExhausted) || !errors.Is(err, io.EOF) {
				t.Fatalf("ErrQuotaExhausted 必须同时匹配 io.EOF，得到 %v", err)
			}
			if vc.IsPeerClosed() {
				t.Fatal("配额耗尽不得落定为对端断开")
			}
			assertNoPending(t, vc.CloseNotify(), "配额耗尽")
		})
	}
}

// TestConn_ReadLimitConcurrent 多线程下限流必须精确（不超发）
func TestConn_ReadLimitConcurrent(t *testing.T) {
	const (
		limit   = 1000
		workers = 8
	)
	mc := &memConn{data: make([]byte, 64*1024)}
	vc := New(mc)
	defer vc.Close()
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

// TestConn_ReadErrorSplit 数据与非超时错误分两次返回
func TestConn_ReadErrorSplit(t *testing.T) {
	s1, s2 := pipePair()
	defer s2.Close()

	boom := errors.New("boom")
	vc := New(&errConn{Conn: s1, data: []byte("ab"), err: boom})
	defer vc.Close()

	buf := make([]byte, 8)
	if n, err := vc.Read(buf); n != 2 || err != nil {
		t.Fatalf("首次读应返回 (2, nil)，得到 (%d, %v)", n, err)
	}
	if n, err := vc.Read(buf); n != 0 || !errors.Is(err, boom) {
		t.Fatalf("遗留错误应留存到下一次，得到 (%d, %v)", n, err)
	}
}

// TestConn_ClosedContract 关闭后的 net.Conn 契约：地址快照 + 全部方法返回 net.ErrClosed
func TestConn_ClosedContract(t *testing.T) {
	vc, srv, ln := tcpPair(t)
	defer ln.Close()
	defer srv.Close()

	la, ra := vc.LocalAddr(), vc.RemoteAddr()
	if la == nil || ra == nil {
		t.Fatal("构造后地址不应为 nil")
	}
	if err := vc.Close(); err != nil {
		t.Fatalf("Close: %v", err)
	}
	if vc.LocalAddr() == nil || vc.RemoteAddr() == nil ||
		vc.LocalAddr().String() != la.String() || vc.RemoteAddr().String() != ra.String() {
		t.Fatal("关闭后地址快照应保持一致")
	}

	cases := []struct {
		name string
		call func() error
	}{
		{"Read", func() error { _, err := vc.Read(make([]byte, 4)); return err }},
		{"Write", func() error { _, err := vc.Write([]byte("x")); return err }},
		{"SetDeadline", func() error { return vc.SetDeadline(time.Now()) }},
		{"SetReadDeadline", func() error { return vc.SetReadDeadline(time.Now()) }},
		{"SetWriteDeadline", func() error { return vc.SetWriteDeadline(time.Now()) }},
		{"SetReadBuffer", func() error { return vc.SetReadBuffer(1024) }},
		{"SetWriteBuffer", func() error { return vc.SetWriteBuffer(1024) }},
		{"SetKeepAlive", func() error { return vc.SetKeepAlive(true) }},
		{"SetKeepAlivePeriod", func() error { return vc.SetKeepAlivePeriod(time.Second) }},
		{"SetLinger", func() error { return vc.SetLinger(0) }},
		{"CloseWrite", func() error { return vc.CloseWrite() }},
		{"CloseRead", func() error { return vc.CloseRead() }},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			if err := c.call(); !errors.Is(err, net.ErrClosed) {
				t.Fatalf("关闭后 %s 应返回 net.ErrClosed，得到 %v", c.name, err)
			}
		})
	}

	if err := vc.Close(); err != nil {
		t.Fatalf("重复 Close 应复用首次结果，得到 %v", err)
	}
}

// TestConn_CloseOnce 并发 Close：结果一致，且底层只关一次
func TestConn_CloseOnce(t *testing.T) {
	t.Run("并发结果一致", func(t *testing.T) {
		s1, s2 := pipePair()
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
				t.Fatalf("第 %d 个并发 Close 返回 %v，期望 %v", i, err, boom)
			}
		}
	})

	t.Run("底层只关闭一次", func(t *testing.T) {
		s1, s2 := pipePair()
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
		select {
		case err := <-secondDone:
			t.Fatalf("并发 Close 过早返回: %v", err)
		case <-time.After(50 * time.Millisecond):
		}
		close(fc.release)

		for _, ch := range []chan error{firstDone, secondDone} {
			select {
			case err := <-ch:
				if !errors.Is(err, boom) {
					t.Fatalf("Close 返回 %v，期望 %v", err, boom)
				}
			case <-time.After(time.Second):
				t.Fatal("Close 未返回")
			}
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
	})
}

// TestConn_CloseUnblocksRead Close 必须立即返回并解除阻塞中的 Read
func TestConn_CloseUnblocksRead(t *testing.T) {
	cases := []struct {
		name string
		wrap func(net.Conn) net.Conn
	}{
		{"普通连接", func(c net.Conn) net.Conn { return c }},
		{"不支持 deadline 的连接", func(c net.Conn) net.Conn { return &noDeadlineConn{Conn: c} }},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			s1, s2 := pipePair()
			defer s2.Close()

			vc := New(tc.wrap(s1))
			ch := vc.CloseNotify()

			errc := make(chan error, 1)
			go func() {
				buf := make([]byte, 8)
				_, err := vc.Read(buf)
				errc <- err
			}()
			time.Sleep(30 * time.Millisecond)

			done := make(chan error, 1)
			go func() { done <- vc.Close() }()
			select {
			case <-done:
			case <-time.After(2 * time.Second):
				t.Fatal("Close 永久阻塞")
			}

			select {
			case err := <-errc:
				if err == nil {
					t.Fatal("阻塞中的 Read 应返回错误")
				}
			case <-time.After(2 * time.Second):
				t.Fatal("Close 未能解除阻塞中的 Read")
			}

			if err := awaitErr(t, ch, "本地关闭"); !errors.Is(err, net.ErrClosed) {
				t.Fatalf("通知应为 net.ErrClosed，得到 %v", err)
			}
		})
	}
}

// TestConn_OptionsPassThrough 可选方法透传（表驱动）
func TestConn_OptionsPassThrough(t *testing.T) {
	s1, s2 := pipePair()
	defer s2.Close()

	oc := &optionConn{Conn: s1}
	vc := New(oc)
	defer vc.Close()

	cases := []struct {
		name  string
		apply func() error
		check func(*testing.T)
	}{
		{
			"SetKeepAlive", func() error { return vc.SetKeepAlive(true) },
			func(t *testing.T) {
				if !oc.keepAlive.Load() {
					t.Fatal("SetKeepAlive 未透传")
				}
			},
		},
		{
			"SetKeepAlivePeriod", func() error { return vc.SetKeepAlivePeriod(3 * time.Second) },
			func(t *testing.T) {
				if d := time.Duration(oc.keepAlivePeriod.Load()); d != 3*time.Second {
					t.Fatalf("SetKeepAlivePeriod 透传为 %v", d)
				}
			},
		},
		{
			"SetLinger", func() error { return vc.SetLinger(5) },
			func(t *testing.T) {
				if oc.linger.Load() != 5 {
					t.Fatal("SetLinger 未透传")
				}
			},
		},
		{
			"CloseWrite", func() error { return vc.CloseWrite() },
			func(t *testing.T) {
				if !oc.closeWrite.Load() {
					t.Fatal("CloseWrite 未透传")
				}
			},
		},
		{
			"CloseRead", func() error { return vc.CloseRead() },
			func(t *testing.T) {
				if !oc.closeRead.Load() {
					t.Fatal("CloseRead 未透传")
				}
			},
		},
	}

	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			if err := c.apply(); err != nil {
				t.Fatalf("%s 返回错误: %v", c.name, err)
			}
			c.check(t)
		})
	}
}

// TestConn_OptionsUnsupported 底层不支持时返回 ErrUnsupported（表驱动）
func TestConn_OptionsUnsupported(t *testing.T) {
	s1, s2 := pipePair()
	defer s2.Close()

	vc := New(s1)
	defer vc.Close()

	cases := []struct {
		name string
		call func() error
	}{
		{"SetReadBuffer", func() error { return vc.SetReadBuffer(1024) }},
		{"SetWriteBuffer", func() error { return vc.SetWriteBuffer(1024) }},
		{"SetKeepAlive", func() error { return vc.SetKeepAlive(true) }},
		{"SetKeepAlivePeriod", func() error { return vc.SetKeepAlivePeriod(time.Second) }},
		{"SetLinger", func() error { return vc.SetLinger(0) }},
		{"CloseWrite", func() error { return vc.CloseWrite() }},
		{"CloseRead", func() error { return vc.CloseRead() }},
	}

	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			if err := c.call(); !errors.Is(err, ErrUnsupported) {
				t.Fatalf("%s 应返回 ErrUnsupported，得到 %v", c.name, err)
			}
		})
	}
	if k := StatusOf(ErrUnsupported); k != StatusUnsupported {
		t.Fatalf("ErrUnsupported 应归类为 StatusUnsupported，得到 %v", k)
	}
}
