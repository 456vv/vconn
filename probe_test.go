package vconn

import (
	"errors"
	"io"
	"net"
	"testing"
	"time"
)

// TestProbe_ClassifyPeek MSG_PEEK 三态 → 探测结论
func TestProbe_ClassifyPeek(t *testing.T) {
	cases := []struct {
		name    string
		outcome peekOutcome
		hangup  bool
		wantRes probeResult
		wantIs  []error
		wantNot []error
	}{
		{"wouldBlock 视为存活", peekWouldBlock, true, probeIdle, nil, nil},
		{
			"data+hangup → 哨兵", peekData, true, probeReadableHangup,
			[]error{io.EOF, ErrPeerClosedWithUnread},
			nil,
		},
		{"data 无 hangup", peekData, false, probeReadable, nil, []error{ErrPeerClosedWithUnread}},
		{
			"EOF → 干净关闭", peekEOF, true, probeClosed,
			[]error{io.EOF},
			[]error{ErrPeerClosedWithUnread},
		},
	}

	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			res, err := classifyPeek(c.outcome, c.hangup)
			if res != c.wantRes {
				t.Fatalf("res = %v，期望 %v（err=%v）", res, c.wantRes, err)
			}
			for _, target := range c.wantIs {
				if !errors.Is(err, target) {
					t.Fatalf("期望匹配 %v，err=%v", target, err)
				}
			}
			for _, target := range c.wantNot {
				if errors.Is(err, target) {
					t.Fatalf("不应匹配 %v，err=%v", target, err)
				}
			}
		})
	}
}

// TestProbe_IsCleanPeerClose 全包唯一的「干净 FIN」判定
func TestProbe_IsCleanPeerClose(t *testing.T) {
	cases := []struct {
		name string
		err  error
		want bool
	}{
		{"nil", nil, true},
		{"io.EOF", io.EOF, true},
		{"干净哨兵", peerClosedWithUnreadError{}, true},
		{"带 cause 的哨兵", peerClosedWithUnreadError{cause: errors.New("reset")}, false},
		{"自定义错误", errors.New("boom"), false},
		{"net.ErrClosed", net.ErrClosed, false},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			if got := isCleanPeerClose(c.err); got != c.want {
				t.Fatalf("isCleanPeerClose(%v) = %v, 期望 %v", c.err, got, c.want)
			}
		})
	}
}

// TestProbe_IsBlockProbeSafe 叶子连接可探测，包装型默认不碰
func TestProbe_IsBlockProbeSafe(t *testing.T) {
	s1, s2 := pipePair()
	defer s1.Close()
	defer s2.Close()

	if !isBlockProbeSafe(s1) {
		t.Fatal("叶子连接（net.Pipe）应允许阻塞探测")
	}
	if isWrapperConn(s1) {
		t.Fatal("net.Pipe 不是包装型连接")
	}

	wrapped := &netConnWrapper{Conn: s1}
	if !isWrapperConn(wrapped) || isBlockProbeSafe(wrapped) {
		t.Fatal("声明 NetConn() 的包装型连接默认不应被阻塞探测")
	}
	if !isBlockProbeSafe(WrapSafeBlockProbe(wrapped)) {
		t.Fatal("WrapSafeBlockProbe 应恢复阻塞探测能力")
	}
	if isBlockProbeSafe(noProbeConn{Conn: s1}) {
		t.Fatal("SafeToBlockProbe()=false 应拒绝阻塞探测")
	}
}

// TestCheck 探测结论全覆盖
func TestCheck(t *testing.T) {
	t.Run("健康TCP", func(t *testing.T) {
		vc, srv, ln := tcpPair(t)
		defer ln.Close()
		defer srv.Close()
		defer vc.Close()

		if err := vc.Check(); err != nil {
			t.Fatalf("健康连接应返回 nil（存活且干净），得到 %v", err)
		}
		if _, err := srv.Write([]byte("x")); err != nil {
			t.Fatalf("Check 不应影响底层连接: %v", err)
		}
	})

	t.Run("本地已关闭", func(t *testing.T) {
		vc, srv, ln := tcpPair(t)
		defer ln.Close()
		defer srv.Close()

		vc.Close()
		err := vc.Check()
		st := StatusOf(err)
		if st != StatusLocalClosed || !errors.Is(err, net.ErrClosed) {
			t.Fatalf("应为 (StatusLocalClosed, net.ErrClosed)，得到 (%v, %v)", st, err)
		}
		// 单一 error 边界上，结论与原因必须同时可读：Status() 给结论，Cause() 给原因。
		if StatusOf(err).Class() != StatusLocalClosed || !errors.Is(err, net.ErrClosed) {
			t.Fatalf("result 应同时暴露结论与原因，得到 %v", err)
		}
	})

	t.Run("无fd无法验证", func(t *testing.T) {
		s, c := pipePair()
		defer s.Close()
		defer c.Close()

		vc := New(c)
		defer vc.Close()

		st, cause := splitProbe(vc.Check())
		if st != StatusUnverified {
			t.Fatalf("应为 StatusUnverified，得到 (%v, %v)", st, cause)
		}
		if st.Ended() {
			t.Fatal("无法验证不等于已结束")
		}
		// 「无法判定」不是故障：错误非 nil（它是结论的载体），但不带真实原因。
		if cause != nil {
			t.Fatalf("StatusUnverified 不应携带原因，得到 %v", cause)
		}
	})

	t.Run("预读有数据算脏", func(t *testing.T) {
		vc, srv, ln := tcpPair(t)
		defer ln.Close()
		defer srv.Close()
		defer vc.Close()

		vc.r.pushPeek([]byte("abc"))
		st, err := splitProbe(vc.Check())
		if st != StatusDataPending || err != nil {
			t.Fatalf("应为 (StatusDataPending, nil)，得到 (%v, %v)", st, err)
		}
		if !st.Readable() || st.Reusable() {
			t.Fatal("DataPending 应可读但不可复用")
		}
	})

	t.Run("对端干净FIN", func(t *testing.T) {
		vc, srv, ln := tcpPair(t)
		defer ln.Close()
		defer vc.Close()

		srv.Close()
		st, err := waitStatus(t, vc, func(s Status) bool { return s.Class() == StatusPeerClosed }, 2*time.Second)
		if st != StatusPeerClosed {
			t.Fatalf("应为 StatusPeerClosed，得到 (%v, %v)", st, err)
		}
		if StatusOf(err) != StatusPeerClosed {
			t.Fatalf("错误应归类为 StatusPeerClosed，得到 %v", StatusOf(err))
		}
		if !vc.IsPeerClosed() || vc.Snapshot() != StatusPeerClosed {
			t.Fatalf("状态机不一致: peerDown=%v snapshot=%v", vc.IsPeerClosed(), vc.Snapshot())
		}
	})

	t.Run("数据后FIN仍可读", func(t *testing.T) {
		vc, srv, ln := tcpPair(t)
		defer ln.Close()
		defer vc.Close()

		if _, err := srv.Write([]byte("hello")); err != nil {
			t.Fatal(err)
		}
		srv.Close()

		st, err := waitStatus(t, vc, func(s Status) bool { return s.Readable() && s.Class() != StatusAlive }, 2*time.Second)
		if !st.Readable() || st == StatusAlive {
			t.Fatalf("有未读数据时必须仍可读，得到 (%v, %v)", st, err)
		}
		if st.Unread() && StatusOf(err) != StatusPeerClosed {
			t.Fatalf("未读数据 + FIN 应归类为 StatusPeerClosed，得到 %v", StatusOf(err))
		}

		buf := make([]byte, 8)
		vc.SetReadDeadline(time.Now().Add(time.Second))
		if n, rerr := vc.Read(buf); n != 5 || string(buf[:n]) != "hello" || rerr != nil {
			t.Fatalf("数据丢失: n=%d data=%q err=%v", n, buf[:n], rerr)
		}
	})

	t.Run("对端RST", func(t *testing.T) {
		vc, srv, ln := tcpPair(t)
		defer ln.Close()
		defer vc.Close()

		if tc, ok := srv.(*net.TCPConn); ok {
			if err := tc.SetLinger(0); err != nil {
				t.Fatal(err)
			}
		}
		srv.Close()

		st, _ := waitStatus(t, vc, func(s Status) bool { return s.Ended() }, 2*time.Second)
		if !st.Ended() {
			t.Fatalf("RST 后必须判定已结束，得到 %v", st)
		}
		buf := make([]byte, 16)
		_, err := vc.Read(buf)
		if err == nil {
			t.Fatal("RST 后 Read 必须返回错误")
		}
		if st.Class() == StatusPeerReset && StatusOf(err) != StatusPeerReset {
			t.Fatalf("Read 不得把 RST 降级，得到 %v", err)
		}
	})
}

// TestCheckBlocking 阻塞兜底路径 + 无副作用 + 有界等待
func TestCheckBlocking(t *testing.T) {
	t.Run("无fd确认存活（有界等待且无副作用）", func(t *testing.T) {
		s, c := pipePair()
		defer s.Close()
		defer c.Close()

		vc := New(c)
		defer vc.Close()

		start := time.Now()
		err := vc.CheckBlocking()
		elapsed := time.Since(start)

		if err != nil {
			t.Fatalf("静默对端应视为存活（nil），得到 %v", err)
		}
		if elapsed > 250*time.Millisecond {
			t.Fatalf("探测过慢: %v", elapsed)
		}
		if elapsed < 500*time.Microsecond {
			t.Fatalf("探测过快，是否真的等待了？%v", elapsed)
		}
		if got := vc.peekSize.Load(); got != 0 {
			t.Fatalf("探活不应改写 peekSize，得到 %d", got)
		}
		if vc.peekUserSet.Load() {
			t.Fatal("探活不应把连接标记为「用户已设置预读」")
		}
	})

	t.Run("无fd发现对端关闭", func(t *testing.T) {
		s, c := pipePair()
		defer c.Close()

		vc := New(c)
		defer vc.Close()

		s.Close()
		if err := vc.CheckBlocking(); StatusOf(err) != StatusPeerClosed || !errors.Is(err, io.EOF) {
			t.Fatalf("阻塞兜底应发现对端关闭，得到 (%v, %v)", StatusOf(err), err)
		}
	})

	t.Run("预读被显式关闭时 fail-open", func(t *testing.T) {
		s, c := pipePair()
		defer s.Close()
		defer c.Close()

		vc := New(c)
		defer vc.Close()
		vc.SetBackgroundReadBuffer(0)

		if err := vc.CheckBlocking(); StatusOf(err) != StatusUnverified {
			t.Fatalf("应为 StatusUnverified，得到 %v", err)
		}
	})

	t.Run("本地已关闭", func(t *testing.T) {
		vc, srv, ln := tcpPair(t)
		defer ln.Close()
		defer srv.Close()
		vc.Close()

		for _, fn := range []func() error{vc.Check, vc.CheckBlocking} {
			err := fn()
			if st := StatusOf(err); !errors.Is(err, net.ErrClosed) || st.Class() != StatusLocalClosed {
				t.Fatalf("应为 (StatusLocalClosed, net.ErrClosed)，得到 (%v, %v)", st, err)
			}
		}
	})
}

// TestCheckReusable 池化判定（表驱动：一次 setup → 一个结论 + 一个哨兵）
func TestCheckReusable(t *testing.T) {
	cases := []struct {
		name    string
		setup   func(t *testing.T, vc *Conn, srv net.Conn)
		want    Status
		wantErr error
	}{
		{"干净连接", nil, StatusAlive, nil},
		{
			"预读脏", func(_ *testing.T, vc *Conn, _ net.Conn) { vc.r.pushPeek([]byte("leftover")) },
			StatusDataPending, ErrDataPending,
		},
		{
			"通知已落定", func(_ *testing.T, vc *Conn, _ net.Conn) { vc.CancelNotify(nil) },
			StatusLatched, ErrNotifyLatched,
		},
		{
			"配额耗尽", func(_ *testing.T, vc *Conn, _ net.Conn) { vc.SetReadLimit(0) },
			StatusQuota, ErrQuota,
		},
		{"延迟错误", func(_ *testing.T, vc *Conn, _ net.Conn) {
			vc.r.fg.Lock()
			vc.r.pending = io.EOF
			vc.r.fg.Unlock()
		}, StatusPeerClosed, io.EOF},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			vc, srv, ln := tcpPair(t)
			defer ln.Close()
			defer srv.Close()
			defer vc.Close()

			if tc.setup != nil {
				tc.setup(t, vc, srv)
			}

			st, err := splitProbe(vc.CheckReusable())
			if st != tc.want {
				t.Fatalf("st = %v，期望 %v（err=%v）", st, tc.want, err)
			}
			if tc.wantErr == nil {
				if err != nil {
					t.Fatalf("期望 nil，得到 %v", err)
				}
				if !st.Reusable() {
					t.Fatal("StatusAlive 应可复用")
				}
				return
			}
			if !errors.Is(err, ErrNotReusable) || !errors.Is(err, tc.wantErr) {
				t.Fatalf("应同时匹配 ErrNotReusable 与 %v，得到 %v", tc.wantErr, err)
			}
			if got := StatusOf(err); got.Class() != tc.want.Class() {
				t.Fatalf("StatusOf(err) = %v，期望 %v", got, tc.want)
			}
		})
	}

	t.Run("本地已关闭", func(t *testing.T) {
		vc, srv, ln := tcpPair(t)
		defer ln.Close()
		defer srv.Close()
		vc.Close()

		st, err := splitProbe(vc.CheckReusable())
		if st != StatusLocalClosed || !errors.Is(err, net.ErrClosed) || !errors.Is(err, ErrNotReusable) {
			t.Fatalf("应为 (StatusLocalClosed, net.ErrClosed)，得到 (%v, %v)", st, err)
		}
	})

	t.Run("对端已关闭", func(t *testing.T) {
		vc, srv, ln := tcpPair(t)
		defer ln.Close()
		defer vc.Close()

		srv.Close()
		deadline := time.Now().Add(2 * time.Second)
		for {
			st, err := splitProbe(vc.CheckReusable())
			if st.Ended() {
				if !errors.Is(err, ErrNotReusable) || StatusOf(err) != StatusPeerClosed {
					t.Fatalf("应归类为 StatusPeerClosed，得到 (%v, %v)", st, err)
				}
				return
			}
			if time.Now().After(deadline) {
				t.Fatalf("对端已关闭，CheckReusable 仍报告 %v", st)
			}
			time.Sleep(5 * time.Millisecond)
		}
	})
}

// TestSnapshot 纯快照：不探测、永不宣称存活
func TestSnapshot(t *testing.T) {
	t.Run("健康连接只报 Unverified", func(t *testing.T) {
		vc, srv, ln := tcpPair(t)
		defer ln.Close()
		defer srv.Close()
		defer vc.Close()

		if st := vc.Snapshot(); st != StatusUnverified {
			t.Fatalf("未探测过的健康连接快照应为 StatusUnverified，得到 %v", st)
		}
		if err := vc.Check(); err != nil {
			t.Fatalf("Check 应返回 nil（存活且干净），得到 %v", err)
		}
		if st := vc.Snapshot(); st != StatusUnverified {
			t.Fatalf("探测过也不缓存「存活」，得到 %v", st)
		}
	})

	cases := []struct {
		name  string
		setup func(*Conn)
		want  Status
	}{
		{"已落定通知", func(vc *Conn) { vc.CancelNotify(nil) }, StatusLatched},
		{"预读有数据", func(vc *Conn) { vc.r.pushPeek([]byte("x")) }, StatusDataPending},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			vc, srv, ln := tcpPair(t)
			defer ln.Close()
			defer srv.Close()
			defer vc.Close()

			tc.setup(vc)
			if st := vc.Snapshot(); st != tc.want {
				t.Fatalf("快照 = %v，期望 %v", st, tc.want)
			}
		})
	}

	t.Run("本地关闭", func(t *testing.T) {
		vc, srv, ln := tcpPair(t)
		defer ln.Close()
		defer srv.Close()
		vc.Close()

		if st := vc.Snapshot(); st != StatusLocalClosed {
			t.Fatalf("快照 = %v，期望 StatusLocalClosed", st)
		}
	})
}

// TestProbe_NeverReturnsErrorOnlyClasses 探测 API 只返回观测空间的类别。
//
// Check* 只返回一个 error，结论一律经 StatusOf 取出；Snapshot 直接给 Status。
// 两者共用 obs 断言，保证「探测永不返回错误空间专属类别」。
func TestProbe_NeverReturnsErrorOnlyClasses(t *testing.T) {
	obs := func(name string, st Status, err error) {
		t.Helper()
		if st.Class() > StatusQuota {
			t.Fatalf("%s 返回了错误空间专属类别 %v (%v)", name, st, err)
		}
	}
	// check 把 Check* 的单一 error 收敛成 obs 需要的 (Status, error)。
	check := func(name string, err error) {
		t.Helper()
		obs(name, StatusOf(err), err)
	}

	t.Run("健康TCP", func(t *testing.T) {
		vc, srv, ln := tcpPair(t)
		defer ln.Close()
		defer srv.Close()
		defer vc.Close()

		check("Check", vc.Check())
		check("CheckBlocking", vc.CheckBlocking())
		check("CheckReusable", vc.CheckReusable())
		obs("Snapshot", vc.Snapshot(), vc.Err())
	})

	t.Run("无fd连接", func(t *testing.T) {
		s, c := pipePair()
		defer s.Close()
		defer c.Close()

		vc := New(c)
		defer vc.Close()

		check("Check", vc.Check())
		check("CheckBlocking", vc.CheckBlocking())
		obs("Snapshot", vc.Snapshot(), vc.Err())
	})

	t.Run("预读有数据", func(t *testing.T) {
		vc, srv, ln := tcpPair(t)
		defer ln.Close()
		defer srv.Close()
		defer vc.Close()

		vc.r.pushPeek([]byte("x"))
		check("Check", vc.Check())
		obs("Snapshot", vc.Snapshot(), vc.Err())
	})
}

// TestProbe_NoDriftWithProbe 探测结论与 StatusOf 不得互相打架
func TestProbe_NoDriftWithProbe(t *testing.T) {
	t.Run("可归类的延迟错误", func(t *testing.T) {
		vc, srv, ln := tcpPair(t)
		defer ln.Close()
		defer srv.Close()
		defer vc.Close()

		vc.r.fg.Lock()
		vc.r.pending = io.EOF
		vc.r.fg.Unlock()

		st, err := splitProbe(vc.CheckReusable())
		if st.Class() != StatusPeerClosed || StatusOf(err).Class() != StatusPeerClosed {
			t.Fatalf("两轴必须一致，得到 (%v, StatusOf=%v)", st, StatusOf(err))
		}
	})

	t.Run("无法归类的原因（唯一合法例外）", func(t *testing.T) {
		vc, srv, ln := tcpPair(t)
		defer ln.Close()
		defer srv.Close()
		defer vc.Close()

		vc.r.fg.Lock()
		vc.r.pending = errors.New("boom")
		vc.r.fg.Unlock()

		st, err := splitProbe(vc.CheckReusable())
		if st.Class() != StatusPeerReset {
			t.Fatalf("无法归类时应保守报 StatusPeerReset，得到 %v", st)
		}
		if got := StatusOf(err).Class(); got != StatusUnknown {
			t.Fatalf("StatusOf 应如实报 StatusUnknown，得到 %v", got)
		}
	})
}

// TestSocketError_Unsupported 拿不到 fd 时不得判定为已断开
func TestSocketError_Unsupported(t *testing.T) {
	s, c := pipePair()
	defer s.Close()

	if err := socketError(c); !errors.Is(err, ErrUnsupported) {
		t.Fatalf("net.Pipe 上 socketError 应返回 ErrUnsupported，得到 %v", err)
	}

	vc := New(c)
	defer vc.Close()
	if err := vc.Check(); StatusOf(err).Class() != StatusUnverified {
		t.Fatalf("Check 应 fail-open 为 StatusUnverified，得到 %v", err)
	}
}

// TestProbeConn_DataThenFIN 有未读数据时任何一轮都不得判为 probeClosed
func TestProbeConn_DataThenFIN(t *testing.T) {
	vc, srv, ln := tcpPair(t)
	defer ln.Close()
	defer vc.Close()

	if _, err := srv.Write([]byte("hello")); err != nil {
		t.Fatal(err)
	}
	time.Sleep(20 * time.Millisecond)
	if err := srv.Close(); err != nil {
		t.Fatal(err)
	}

	deadline := time.Now().Add(400 * time.Millisecond)
	for time.Now().Before(deadline) {
		if res, err := probeConn(vc.rwc); res == probeClosed {
			t.Fatalf("有未读数据时被判为已关闭（会丢数据）：err=%v", err)
		}
		time.Sleep(10 * time.Millisecond)
	}

	buf := make([]byte, 8)
	vc.SetReadDeadline(time.Now().Add(time.Second))
	if n, err := vc.Read(buf); n != 5 || string(buf[:n]) != "hello" {
		t.Fatalf("数据丢失: n=%d data=%q err=%v", n, buf[:n], err)
	}
}
