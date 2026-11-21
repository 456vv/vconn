package vconn

import (
	"bytes"
	"context"
	"errors"
	"io"
	"net"
	"testing"
	"time"
)

// TestCombo_APIMatrix 交叉组合公开 API，盯住单测里容易漏掉的副作用：
// 限额与预读谁先算、半关闭会不会被当成全关闭、探测会不会改写 deadline、
// 所有权交还会不会丢掉已经缓冲的字节。
func TestCombo_APIMatrix(t *testing.T) {
	t.Run("构造与缓冲", func(t *testing.T) {
		t.Run("New(nil) panic", func(t *testing.T) {
			defer func() {
				if recover() == nil {
					t.Fatal("New(nil) 应 panic")
				}
			}()
			New(nil)
		})

		t.Run("NewData(nil) panic", func(t *testing.T) {
			defer func() {
				if recover() == nil {
					t.Fatal("NewData(nil) 应 panic")
				}
			}()
			NewData(nil, []byte("x"))
		})

		t.Run("New 对已包装连接幂等", func(t *testing.T) {
			a, b := pipePair()
			defer b.Close()
			vc := New(a)
			defer vc.Close()
			if again := New(vc); again != vc {
				t.Fatal("New(*Conn) 应返回同一实例")
			}
		})

		cases := []struct {
			name string
			seed []byte
		}{
			{"空种子", nil},
			{"空切片", []byte{}},
			{"有数据", []byte("abc")},
		}
		for _, tc := range cases {
			t.Run(tc.name, func(t *testing.T) {
				a, b := pipePair()
				defer b.Close()
				vc := NewData(a, tc.seed)
				defer vc.Close()

				if len(tc.seed) == 0 {
					if st := vc.Snapshot(); st != StatusUnverified {
						t.Fatalf("空种子快照 = %v，期望 unverified", st)
					}
					return
				}
				if st := vc.Snapshot(); st != StatusDataPending {
					t.Fatalf("有种子快照 = %v，期望 data-pending", st)
				}
				buf := make([]byte, 8)
				n, err := vc.Read(buf)
				if err != nil || !bytes.Equal(buf[:n], tc.seed) {
					t.Fatalf("Read = (%d, %v) %q", n, err, buf[:n])
				}
				// 再包一层：已关闭前追加，已关闭后必须丢弃。
				again := NewData(vc, []byte("Z"))
				if again != vc {
					t.Fatal("NewData(*Conn) 应返回同一实例")
				}
				n, err = vc.Read(buf)
				if err != nil || string(buf[:n]) != "Z" {
					t.Fatalf("追加预读 = (%d, %v) %q", n, err, buf[:n])
				}
				if err := vc.Close(); err != nil {
					t.Fatal(err)
				}
				lost := NewData(vc, []byte("nope"))
				if lost != vc {
					t.Fatal("关闭后再 NewData 仍应返回同一实例")
				}
				if n, err := vc.Read(buf); n != 0 || !errors.Is(err, net.ErrClosed) {
					t.Fatalf("关闭后追加的字节不应被读出: (%d, %v)", n, err)
				}
			})
		}
	})

	t.Run("限额叠在预读上", func(t *testing.T) {
		cases := []struct {
			name     string
			seed     string
			limit    int64
			chunk    int
			want     string
			wantNext error // 紧接着一次 Read 的错误；nil 表示还没到头
			peerDown bool
			notify   bool
		}{
			{"种子大于限额先交付限额", "abcdef", 3, 8, "abc", ErrQuota, false, false},
			{"限额正好吃完种子", "ab", 2, 8, "ab", ErrQuota, false, false},
			{"限额大于种子先把种子交完", "ab", 5, 2, "ab", nil, false, false},
			{"限额0不交付种子", "abc", 0, 8, "", ErrQuota, false, false},
			{"负限额取消限制", "abcd", -1, 8, "abcd", nil, false, false},
		}
		for _, tc := range cases {
			t.Run(tc.name, func(t *testing.T) {
				a, b := pipePair()
				defer b.Close()
				vc := NewData(a, []byte(tc.seed))
				defer vc.Close()
				ch := vc.CloseNotify()
				vc.SetReadLimit(tc.limit)

				buf := make([]byte, tc.chunk)
				n, err := vc.Read(buf)
				if err != nil && !errors.Is(err, io.EOF) || string(buf[:n]) != tc.want {
					t.Fatalf("Read = (%d, %v) %q，期望 %q", n, err, buf[:n], tc.want)
				}
				if tc.wantNext != nil {
					n, err = vc.Read(buf)
					if n != 0 || !errors.Is(err, tc.wantNext) || !errors.Is(err, io.EOF) {
						t.Fatalf("下一次 Read = (%d, %v)，期望 %v 且满足 io.EOF", n, err, tc.wantNext)
					}
					if StatusOf(err) != StatusQuota {
						t.Fatalf("StatusOf = %v，配额耗尽不能算对端关闭", StatusOf(err))
					}
				}
				if vc.IsPeerClosed() != tc.peerDown {
					t.Fatalf("IsPeerClosed = %v，期望 %v", vc.IsPeerClosed(), tc.peerDown)
				}
				if tc.notify {
					awaitErr(t, ch, tc.name)
				} else {
					assertNoPending(t, ch, tc.name)
				}

				rerr := vc.CheckReusable()
				if tc.wantNext == ErrQuota {
					if !errors.Is(rerr, ErrQuota) || !errors.Is(rerr, ErrNotReusable) || !errors.Is(rerr, ErrQuota) {
						t.Fatalf("配额耗尽后 CheckReusable = (%v)", rerr)
					}
					if vc.Snapshot() != StatusUnverified && len(tc.want) == len(tc.seed) {
						// 种子已交完且没有对端结论：快照不能谎称存活，也不能谎称已死。
						t.Fatalf("种子交完后 Snapshot = %v", vc.Snapshot())
					}
				}
			})
		}

		t.Run("限额用尽后重开仍能读剩余预读", func(t *testing.T) {
			a, b := pipePair()
			defer b.Close()
			vc := NewData(a, []byte("abcdef"))
			defer vc.Close()
			vc.SetReadLimit(2)

			buf := make([]byte, 8)
			n, err := vc.Read(buf)
			if err != nil || string(buf[:n]) != "ab" {
				t.Fatalf("首次 = (%d, %v) %q", n, err, buf[:n])
			}
			if _, err := vc.Read(buf); !errors.Is(err, ErrQuota) {
				t.Fatalf("耗尽 = %v", err)
			}
			vc.SetReadLimit(3)
			n, err = vc.Read(buf)
			if err != nil || string(buf[:n]) != "cde" {
				t.Fatalf("重开后 = (%d, %v) %q", n, err, buf[:n])
			}
			if _, err := vc.Read(buf); !errors.Is(err, ErrQuota) {
				t.Fatalf("剩余 1 字节被限额挡住后应再耗尽，得到 %v", err)
			}
			vc.SetReadLimit(-1)
			n, err = vc.Read(buf)
			if err != nil || string(buf[:n]) != "f" {
				t.Fatalf("取消限额后应交出最后一字节，得到 (%d, %v) %q", n, err, buf[:n])
			}
		})

		t.Run("空缓冲不消耗限额也不报错", func(t *testing.T) {
			a, b := pipePair()
			defer b.Close()
			vc := NewData(a, []byte("xyz"))
			defer vc.Close()
			vc.SetReadLimit(1)
			if n, err := vc.Read(nil); n != 0 || err != nil {
				t.Fatalf("空缓冲 Read = (%d, %v)", n, err)
			}
			buf := make([]byte, 1)
			if n, err := vc.Read(buf); n != 1 || err != nil || buf[0] != 'x' {
				t.Fatalf("限额仍应是 1，得到 (%d, %v) %q", n, err, buf[:n])
			}
		})
	})

	t.Run("写半关闭不是对端结束", func(t *testing.T) {
		vc, srv, ln := tcpPair(t)
		defer ln.Close()
		defer srv.Close()
		defer vc.Close()

		vc.SetMonitorInterval(10 * time.Millisecond)
		ch := vc.CloseNotify()

		if err := vc.CloseWrite(); err != nil {
			t.Fatal(err)
		}
		// 对端读到 EOF；本端读方向仍在。
		srv.SetReadDeadline(time.Now().Add(time.Second))
		if _, err := srv.Read(make([]byte, 1)); !errors.Is(err, io.EOF) {
			t.Fatalf("对端应看到写半关闭 EOF，得到 %v", err)
		}
		if err := vc.Check(); err != nil {
			t.Fatalf("写半关闭后 Check = (%v)", err)
		}

		if _, err := srv.Write([]byte("pong")); err != nil {
			t.Fatal(err)
		}
		vc.SetReadDeadline(time.Now().Add(time.Second))
		buf := make([]byte, 4)
		if n, err := vc.Read(buf); err != nil || string(buf[:n]) != "pong" {
			t.Fatalf("写半关闭后仍应能读: (%d, %v) %q", n, err, buf[:n])
		}

		// 让监控真的跑过几轮，而不是靠「还没轮到探测」侥幸通过
		assertNoNotify(t, ch, 100*time.Millisecond, "写半关闭")
		if vc.IsPeerClosed() || vc.Err() != nil {
			t.Fatalf("写半关闭不是对端结束: peer=%v err=%v", vc.IsPeerClosed(), vc.Err())
		}
	})

	t.Run("读半关闭不影响写方向", func(t *testing.T) {
		vc, srv, ln := tcpPair(t) // 新连接：写方向必须是开的
		defer ln.Close()
		defer srv.Close()
		defer vc.Close()

		if err := vc.CloseRead(); err != nil {
			t.Fatal(err)
		}
		if _, err := vc.Write([]byte("still")); err != nil {
			t.Fatalf("CloseRead 后写应仍可用: %v", err)
		}
		got := make([]byte, 5)
		srv.SetReadDeadline(time.Now().Add(time.Second))
		if n, err := io.ReadFull(srv, got); err != nil || string(got[:n]) != "still" {
			t.Fatalf("对端应收到写入: (%d, %v) %q", n, err, got[:n])
		}
		// 只断言「不是本地 Close」；不要断言 Err()==nil 或通知不落定（见下）
		err := vc.ResetNotify(false)
		if err != nil {
			t.Fatalf("读半关闭被误当成本地 Close: %v", err)
		}
	})

	t.Run("探测不改用户deadline", func(t *testing.T) {
		vc, srv, ln := tcpPair(t)
		defer ln.Close()
		defer srv.Close()
		defer vc.Close()

		user := time.Now().Add(30 * time.Second)
		if err := vc.SetDeadline(user); err != nil {
			t.Fatal(err)
		}
		// 三种探测都不该把用户 deadline 改掉，也不该消费随后到达的字节。
		for _, call := range []struct {
			name string
			fn   func() error
		}{
			{"Check", vc.Check},
			{"CheckBlocking", vc.CheckBlocking},
			{"CheckReusable", vc.CheckReusable},
		} {
			err := call.fn()
			if err != nil {
				t.Fatalf("%s = (%v)", call.name, err)
			}
			if vc.Snapshot() == StatusAlive {
				t.Fatal("Snapshot 不得在探测成功后宣称存活")
			}
		}

		if _, err := srv.Write([]byte("kept")); err != nil {
			t.Fatal(err)
		}
		buf := make([]byte, 4)
		// 不重新设置 deadline：若探测把 deadline 留成过去时间，这里会立刻超时。
		n, err := vc.Read(buf)
		if err != nil || string(buf[:n]) != "kept" {
			t.Fatalf("探测后 Read = (%d, %v) %q", n, err, buf[:n])
		}

		// 显式关掉预读后，无内核探测的连接不能靠吞字节来探活。
		a, b := pipePair()
		defer b.Close()
		pipe := New(a)
		defer pipe.Close()
		pipe.SetBackgroundReadBuffer(0)
		if err := pipe.CheckBlocking(); StatusOf(err).Class() != StatusUnverified {
			t.Fatalf("显式 0 后 CheckBlocking = (%v)", err)
		}
		if err := pipe.CheckReusable(); !errors.Is(err, ErrUnverified) {
			t.Fatalf("显式 0 后 CheckReusable = (%v)", err)
		}
		go func() { b.Write([]byte("Q")) }()
		pipe.SetReadDeadline(time.Now().Add(time.Second))
		if n, err := pipe.Read(buf[:1]); err != nil || n != 1 || buf[0] != 'Q' {
			t.Fatalf("探测不应提前吞掉字节: (%d, %v) %q", n, err, buf[:n])
		}
	})

	t.Run("通知锁存与读写", func(t *testing.T) {
		t.Run("CancelNotify 不打断后续读写", func(t *testing.T) {
			a, b := pipePair()
			defer b.Close()
			vc := New(a)
			defer vc.Close()

			if !vc.CancelNotify(nil) {
				t.Fatal("首次 CancelNotify 应落定")
			}
			if vc.CancelNotify(io.EOF) {
				t.Fatal("已落定后 CancelNotify 应返回 false，且不能改写原因")
			}
			if !errors.Is(vc.Err(), ErrNotifyCanceled) {
				t.Fatalf("原因应保持首次的 ErrNotifyCanceled，得到 %v", vc.Err())
			}
			if vc.IsPeerClosed() {
				t.Fatal("手动取消不是对端断开")
			}
			if st := vc.Snapshot(); st != StatusLatched {
				t.Fatalf("Snapshot = %v", st)
			}
			err := vc.CheckReusable()
			if !errors.Is(err, ErrNotifyLatched) {
				t.Fatalf("CheckReusable = (%v)", err)
			}

			go func() { b.Write([]byte("go")) }()
			buf := make([]byte, 2)
			vc.SetReadDeadline(time.Now().Add(time.Second))
			if n, err := vc.Read(buf); err != nil || string(buf[:n]) != "go" {
				t.Fatalf("锁存后仍应能读: (%d, %v) %q", n, err, buf[:n])
			}

			// net.Pipe 是同步管道：对端没有读者时 Write 会一直阻塞，
			// 因此用 pipeWrite 先起读者再写。
			pipeWrite(t, "锁存后写", vc, b, "back")

			if err := vc.ResetNotify(false); err != nil {
				t.Fatal(err)
			}
			if vc.Err() != nil || vc.Snapshot() == StatusLatched {
				t.Fatalf("重置后不应再锁存: err=%v snap=%v", vc.Err(), vc.Snapshot())
			}
			ch := vc.CloseNotify()
			assertNoPending(t, ch, "重置后立即订阅")
		})

		t.Run("WithContext 与已有订阅看到同一原因", func(t *testing.T) {
			a, b := pipePair()
			defer b.Close()
			vc := New(a)
			defer vc.Close()

			ch, cancelWatch := vc.WatchClose()
			defer cancelWatch()
			ctx, cancel := context.WithCancel(context.Background())
			vc.WithContext(ctx)
			cancel()

			err := awaitErr(t, ch, "WithContext")
			if !errors.Is(err, context.Canceled) {
				t.Fatalf("已有订阅应收到 ctx.Err()，得到 %v", err)
			}
			if vc.IsPeerClosed() {
				t.Fatal("上下文取消不是对端断开")
			}
			// WithContext 只通知不关连接：写方向仍可用（同步管道需对端有读者）。
			pipeWrite(t, "WithContext 后写", vc, b, "x")
			if rerr := vc.ResetNotify(false); rerr != nil {
				t.Fatal(rerr)
			}
			// 旧 channel 保持关闭，不能被下一轮再写一次。
			assertChannelClosed(t, ch, "重置后的旧订阅")
		})

		t.Run("预读残留时对端 FIN 先通知再交付", func(t *testing.T) {
			vc, srv, ln := tcpPair(t)
			defer ln.Close()
			defer vc.Close()

			vc.SetBackgroundReadBuffer(8)
			vc.SetMonitorInterval(10 * time.Millisecond)
			ch := vc.CloseNotify()
			if _, err := srv.Write([]byte("xyz")); err != nil {
				t.Fatal(err)
			}
			srv.Close()

			err := awaitErr(t, ch, "数据后FIN")
			if !errors.Is(err, ErrPeerClosedWithUnread) || !errors.Is(err, io.EOF) {
				t.Fatalf("通知 = %v", err)
			}
			if !vc.IsPeerClosed() {
				t.Fatal("应落定 peerDown")
			}
			snap := vc.Snapshot()
			if snap != StatusPeerClosedWithUnread {
				t.Fatalf("还有预读时 Snapshot = %v", snap)
			}
			buf := make([]byte, 8)
			n, rerr := vc.Read(buf)
			if rerr != nil || string(buf[:n]) != "xyz" {
				t.Fatalf("应先交出残留: (%d, %v) %q", n, rerr, buf[:n])
			}
			if n, rerr = vc.Read(buf); n != 0 || !errors.Is(rerr, io.EOF) {
				t.Fatalf("读空后应是 EOF，得到 (%d, %v)", n, rerr)
			}
			if vc.Snapshot().Unread() {
				t.Fatal("读空后快照不应再带 unread")
			}
			if rerr := vc.ResetNotify(false); !errors.Is(rerr, ErrPeerDown) {
				t.Fatalf("对端已结束时普通重置应拒绝，得到 %v", rerr)
			}
		})
	})

	t.Run("所有权交还", func(t *testing.T) {
		t.Run("RawConn 丢弃预读且不再关闭底层", func(t *testing.T) {
			a, b := pipePair()
			defer b.Close()
			vc := NewData(a, []byte("drop-me"))
			ch := vc.CloseNotify()

			raw := vc.RawConn()
			defer raw.Close()
			err := awaitErr(t, ch, "RawConn")
			if !errors.Is(err, ErrInUse) {
				t.Fatalf("订阅者应收到提取原因，得到 %v", err)
			}
			if st := vc.Snapshot(); st != StatusLocalClosed {
				t.Fatalf("提取后 Snapshot = %v", st)
			}
			if n, rerr := vc.Read(make([]byte, 8)); n != 0 || !errors.Is(rerr, net.ErrClosed) {
				t.Fatalf("包装侧不应再交出被丢弃的预读: (%d, %v)", n, rerr)
			}
			// 字节留在内核/管道里，不在 vconn 缓冲里：RawConn 承诺丢的是自己的预读。
			// 这里种子从未写进管道，所以底层读不到 "drop-me"。
			raw.SetReadDeadline(time.Now().Add(20 * time.Millisecond))
			if n, rerr := raw.Read(make([]byte, 8)); n != 0 && rerr == nil {
				t.Fatalf("被丢弃的预读不应从底层冒出来: %d", n)
			}
			if cerr := vc.Close(); cerr != nil {
				t.Fatalf("提取后 Close 不应再关底层，得到 %v", cerr)
			}
			// 提取后底层连接仍可用（同步管道需对端有读者）。
			pipeWrite(t, "提取原始连接后写", raw, b, "alive")
			if rerr := vc.ResetNotify(true); !errors.Is(rerr, ErrLocalClosed) {
				t.Fatalf("force 也越不过提取，得到 %v", rerr)
			}
			defer func() {
				if recover() == nil {
					t.Fatal("RawConn 之后 RawConnFull 应 panic")
				}
			}()
			vc.RawConnFull()
		})

		t.Run("RawConnFull 交还种子且订阅只通知一次", func(t *testing.T) {
			a, b := pipePair()
			defer b.Close()
			vc := NewData(a, []byte("keep"))
			ch := vc.CloseNotify()

			raw, peeked := vc.RawConnFull()
			defer raw.Close()
			if string(peeked) != "keep" {
				t.Fatalf("种子未交还: %q", peeked)
			}
			err := awaitErr(t, ch, "RawConnFull")
			if !errors.Is(err, ErrInUse) || StatusOf(err) != StatusInUse {
				t.Fatalf("通知 = %v (%v)", err, StatusOf(err))
			}
			select {
			case extra, ok := <-ch:
				if ok {
					t.Fatalf("提取只能通知一次，又收到 %v", extra)
				}
			default:
			}
			if _, rerr := vc.Write([]byte("no")); !errors.Is(rerr, net.ErrClosed) {
				t.Fatalf("包装侧 Write = %v", rerr)
			}
			if vc.LocalAddr() == nil || vc.RemoteAddr() == nil {
				t.Fatal("提取后地址快照应仍可读")
			}
		})

		t.Run("先 Close 再提取不再二次关闭", func(t *testing.T) {
			a, b := pipePair()
			defer b.Close()
			base := &countingCloseConn{Conn: a}
			vc := New(base)
			if err := vc.Close(); err != nil {
				t.Fatal(err)
			}
			vc.RawConn()
			if base.countValue() != 1 {
				t.Fatalf("底层应只被 Close 一次，实际 %d", base.countValue())
			}
		})
	})

	t.Run("无fd连接的探测开关", func(t *testing.T) {
		cases := []struct {
			name   string
			buffer int
			check  Status
			reuse  error
		}{
			{"默认允许短读", 0, StatusAlive, nil},
			{"显式关闭", 0, StatusUnverified, ErrUnverified},
			{"丢弃模式不准吞", -64, StatusUnverified, ErrUnverified},
			{"正数预读可探", 32, StatusAlive, nil},
		}
		// 第一行「默认」不能调用 SetBackgroundReadBuffer，单独走。
		t.Run(cases[0].name, func(t *testing.T) {
			assertPipeProbe(t, func(vc *Conn) {}, cases[0].check, cases[0].reuse)
		})
		for _, tc := range cases[1:] {
			t.Run(tc.name, func(t *testing.T) {
				assertPipeProbe(t, func(vc *Conn) { vc.SetBackgroundReadBuffer(tc.buffer) }, tc.check, tc.reuse)
			})
		}

		t.Run("WrapSafeBlockProbe(nil)", func(t *testing.T) {
			if WrapSafeBlockProbe(nil) != nil {
				t.Fatal("nil 应原样返回 nil")
			}
		})

		t.Run("拒绝阻塞探测的包装保持字节", func(t *testing.T) {
			a, b := pipePair()
			defer b.Close()
			vc := New(&noProbeConn{Conn: a})
			defer vc.Close()
			// 声明 SafeToBlockProbe()=false 的包装不得被阻塞探测：结论只能是
			// 「无法判定」（StatusUnverified），且不能为了探活而吞掉字节。
			if err := vc.CheckBlocking(); StatusOf(err) != StatusUnverified {
				t.Fatalf("CheckBlocking = (%v)，期望 StatusUnverified", err)
			}
			go func() { b.Write([]byte("Z")) }()
			vc.SetReadDeadline(time.Now().Add(time.Second))
			buf := make([]byte, 1)
			if n, err := vc.Read(buf); err != nil || n != 1 || buf[0] != 'Z' {
				t.Fatalf("拒绝探测不应吞字节: (%d, %v)", n, err)
			}
		})
	})

	t.Run("套接字选项关闭后", func(t *testing.T) {
		a, b := pipePair()
		defer b.Close()
		vc := New(&optionConn{Conn: a})

		// 关闭前：底层实现了可选方法，应透传而不是 ErrUnsupported。
		if err := vc.SetKeepAlive(true); err != nil {
			t.Fatal(err)
		}
		if err := vc.CloseWrite(); err != nil {
			t.Fatal(err)
		}
		if err := vc.Close(); err != nil {
			t.Fatal(err)
		}
		// 关闭后必须先于「不支持」返回 net.ErrClosed，否则调用方分不清是平台问题还是已经关掉。
		opts := []struct {
			name string
			call func() error
		}{
			{"SetKeepAlive", func() error { return vc.SetKeepAlive(false) }},
			{"SetKeepAlivePeriod", func() error { return vc.SetKeepAlivePeriod(time.Second) }},
			{"SetLinger", func() error { return vc.SetLinger(1) }},
			{"CloseWrite", func() error { return vc.CloseWrite() }},
			{"CloseRead", func() error { return vc.CloseRead() }},
			{"SetReadBuffer", func() error { return vc.SetReadBuffer(4096) }},
			{"SetWriteBuffer", func() error { return vc.SetWriteBuffer(4096) }},
		}
		for _, tc := range opts {
			t.Run(tc.name, func(t *testing.T) {
				err := tc.call()
				if !errors.Is(err, net.ErrClosed) || errors.Is(err, ErrUnsupported) {
					t.Fatalf("关闭后 %s = %v，应是 net.ErrClosed 而不是 ErrUnsupported", tc.name, err)
				}
				if StatusOf(err) != StatusLocalClosed {
					t.Fatalf("StatusOf = %v", StatusOf(err))
				}
			})
		}
	})
}

// assertPipeProbe 是被「无fd连接的探测开关」子测试直接复用的辅助函数，
// 因此保留为包级函数，不内联到 t.Run 中。
func assertPipeProbe(t *testing.T, setup func(*Conn), want Status, reuse error) {
	t.Helper()
	a, b := pipePair()
	defer a.Close()
	defer b.Close()
	vc := New(a)
	setup(vc)

	// Check 不阻塞：无 fd 的连接只能报「无法判定」，不是死亡。
	if err := vc.Check(); StatusOf(err) != StatusUnverified {
		t.Fatalf("无 fd 的 Check 应是 unverified，得到 %v", err)
	}
	// CheckBlocking 能不能给出结论由预读额度决定：能探 → nil（存活），
	// 不能探 → StatusUnverified（fail-open，不带原因）。
	if err := vc.CheckBlocking(); StatusOf(err) != want {
		t.Fatalf("CheckBlocking = %v，期望 %v", err, want)
	}
	rerr := vc.CheckReusable()
	if reuse == nil {
		if rerr != nil {
			t.Fatalf("CheckReusable = %v，期望可复用", rerr)
		}
		return
	}
	if StatusOf(rerr) != want || !errors.Is(rerr, ErrNotReusable) || !errors.Is(rerr, reuse) {
		t.Fatalf("CheckReusable = (%v, %v)，期望 (%v, %v)", StatusOf(rerr), rerr, want, reuse)
	}
}
