package vconn

import (
	"errors"
	"fmt"
	"net"
	"runtime"
	"sync"
	"testing"
	"time"
)

// TestResetNotify_Rounds 多轮「订阅 → 通知 → 重置」
func TestResetNotify_Rounds(t *testing.T) {
	s1, s2 := pipePair()
	defer s2.Close()

	vc := New(s1)
	defer vc.Close()

	const rounds = 5
	var prev <-chan error
	for i := 1; i <= rounds; i++ {
		ch := vc.CloseNotify()
		want := fmt.Errorf("round-%d", i)
		if !vc.CancelNotify(want) {
			t.Fatalf("第 %d 轮 CancelNotify 应返回 true", i)
		}
		if got := awaitErr(t, ch, fmt.Sprintf("第 %d 轮", i)); got == nil || got.Error() != want.Error() {
			t.Fatalf("第 %d 轮通知内容错误: %v", i, got)
		}
		if prev != nil {
			assertChannelClosed(t, prev, "上一轮 channel 不应被二次投递")
		}
		prev = ch

		if i == rounds {
			break
		}
		if err := vc.ResetNotify(false); err != nil {
			t.Fatalf("第 %d 轮 ResetNotify 失败: %v", i, err)
		}
		if err := vc.Err(); err != nil {
			t.Fatalf("重置后 Err() 应为 nil，得到 %v", err)
		}
		if vc.IsPeerClosed() {
			t.Fatal("手动 CancelNotify 后 IsPeerClosed() 不应为 true")
		}
	}
}

// TestResetNotify_PeerDownNeedsForce 对端已断开时默认拒绝，force=true 越过并再次上报
func TestResetNotify_PeerDownNeedsForce(t *testing.T) {
	vc, srv, ln := tcpPair(t)
	defer ln.Close()
	defer vc.Close()

	vc.SetMonitorInterval(20 * time.Millisecond)
	ch := vc.CloseNotify()
	srv.Close()
	if err := awaitErr(t, ch, "对端关闭"); err == nil {
		t.Fatal("应携带关闭原因")
	}

	if err := vc.ResetNotify(false); !errors.Is(err, ErrResetPeerDown) {
		t.Fatalf("期望 ErrResetPeerDown，得到 %v", err)
	}
	if !vc.IsPeerClosed() {
		t.Fatal("被拒绝的重置不应改变状态")
	}

	if err := vc.ResetNotify(true); err != nil {
		t.Fatalf("force 重置应成功: %v", err)
	}
	if vc.Err() != nil || vc.IsPeerClosed() {
		t.Fatalf("强制重置后状态应清空: err=%v peerDown=%v", vc.Err(), vc.IsPeerClosed())
	}

	ch2 := vc.CloseNotify()
	if err := awaitErr(t, ch2, "强制重置后"); err == nil {
		t.Fatal("应再次上报断开原因")
	}
	if vc.Err() == nil {
		t.Fatal("强制重置后应再次落定通知")
	}
}

// TestResetNotify_Refusals 本地已关闭 / 已提取原始连接时不可重置
func TestResetNotify_Refusals(t *testing.T) {
	t.Run("本地关闭后拒绝", func(t *testing.T) {
		vc, srv, ln := tcpPair(t)
		defer ln.Close()
		defer srv.Close()

		ch := vc.CloseNotify()
		vc.Close()
		if err := awaitErr(t, ch, "本地关闭"); !errors.Is(err, net.ErrClosed) {
			t.Fatalf("应为 net.ErrClosed，得到 %v", err)
		}

		for _, force := range []bool{false, true} {
			if err := vc.ResetNotify(force); !errors.Is(err, ErrResetLocallyClosed) {
				t.Fatalf("force=%v 时应返回 ErrResetLocallyClosed，得到 %v", force, err)
			}
		}
		if err := awaitErr(t, vc.CloseNotify(), "关闭后再订阅"); !errors.Is(err, net.ErrClosed) {
			t.Fatalf("关闭后再订阅应立即得到 net.ErrClosed，得到 %v", err)
		}
	})

	t.Run("提取原始连接后拒绝", func(t *testing.T) {
		s1, s2 := pipePair()
		defer s2.Close()

		vc := New(s1)
		vc.RawConn()
		if err := vc.ResetNotify(false); !errors.Is(err, ErrResetLocallyClosed) {
			t.Fatalf("期望 ErrResetLocallyClosed，得到 %v", err)
		}
	})
}

// TestResetNotify_Idempotent 幂等分支：不打断工作、补回缺失监控、保留预读
func TestResetNotify_Idempotent(t *testing.T) {
	t.Run("不打断正在工作的监控", func(t *testing.T) {
		vc, srv, ln := tcpPair(t)
		defer ln.Close()
		defer srv.Close()

		ch := vc.CloseNotify()
		before := vc.mon.Load()
		if before == nil {
			t.Fatal("订阅后监控应已启动")
		}
		for i := 0; i < 10; i++ {
			if err := vc.ResetNotify(false); err != nil {
				t.Fatalf("第 %d 次幂等重置失败: %v", i, err)
			}
		}
		if vc.mon.Load() != before {
			t.Fatal("幂等重置不应重启正在工作的监控")
		}
		if n := watcherCount(vc); n != 1 {
			t.Fatalf("幂等重置不应影响订阅者，得到 %d", n)
		}
		srv.Close()
		if err := awaitErr(t, ch, "幂等重置后"); err == nil {
			t.Fatal("对端关闭应携带原因")
		}
		vc.Close()
	})

	t.Run("补回缺失的监控", func(t *testing.T) {
		vc, srv, ln := tcpPair(t)
		defer ln.Close()
		defer vc.Close()

		vc.SetMonitorInterval(20 * time.Millisecond)
		ch := vc.CloseNotify()

		vc.stopMonitor() // 人为让监控退出，通知尚未落定
		if vc.runningMonitor() != nil {
			t.Fatal("监控应已停止")
		}
		if err := vc.ResetNotify(false); err != nil {
			t.Fatalf("幂等重置失败: %v", err)
		}
		if vc.runningMonitor() == nil {
			t.Fatal("幂等重置应把缺失的监控补回来")
		}

		srv.Close()
		if err := awaitErr(t, ch, "补回监控后"); err == nil {
			t.Fatal("应检测到对端关闭")
		}
	})

	t.Run("保留预读数据", func(t *testing.T) {
		s1, s2 := pipePair()
		defer s2.Close()

		vc := NewData(s1, []byte("hello"))
		defer vc.Close()

		vc.CloseNotify()
		vc.CancelNotify(errors.New("manual"))
		if err := vc.ResetNotify(false); err != nil {
			t.Fatal(err)
		}
		buf := make([]byte, 5)
		if n, err := vc.Read(buf); err != nil || n != 5 || string(buf[:n]) != "hello" {
			t.Fatalf("重置后预读数据丢失: n=%d err=%v data=%q", n, err, buf[:n])
		}
	})
}

// TestResetNotify_NoGoroutineLeak 反复重置不泄漏协程
func TestResetNotify_NoGoroutineLeak(t *testing.T) {
	s1, s2 := pipePair()
	defer s2.Close()

	vc := New(s1)
	defer vc.Close()

	for i := 0; i < 5; i++ { // 预热
		ch := vc.CloseNotify()
		vc.CancelNotify(errors.New("warmup"))
		if err := awaitErr(t, ch, "预热"); err == nil {
			t.Fatal("应携带原因")
		}
		if err := vc.ResetNotify(false); err != nil {
			t.Fatal(err)
		}
	}
	time.Sleep(50 * time.Millisecond)
	base := runtime.NumGoroutine()

	for i := 0; i < 100; i++ {
		ch := vc.CloseNotify()
		vc.CancelNotify(errors.New("boom"))
		if err := awaitErr(t, ch, "循环"); err == nil {
			t.Fatal("应携带原因")
		}
		if err := vc.ResetNotify(false); err != nil {
			t.Fatalf("第 %d 轮重置失败: %v", i, err)
		}
	}
	time.Sleep(100 * time.Millisecond)

	if after := runtime.NumGoroutine(); after > base+8 {
		t.Fatalf("协程泄漏: %d -> %d", base, after)
	}
	if n := watcherCount(vc); n != 0 {
		t.Fatalf("残留订阅者 %d 个", n)
	}
	if vc.runningMonitor() != nil {
		t.Fatal("无订阅者时不应残留监控协程")
	}
}

// TestResetNotify_Concurrent 并发「订阅/退订/通知/重置」（配合 -race）
func TestResetNotify_Concurrent(t *testing.T) {
	vc, srv, ln := tcpPair(t)
	defer ln.Close()
	defer srv.Close()

	vc.SetMonitorInterval(10 * time.Millisecond)

	stop := make(chan struct{})
	var wg sync.WaitGroup

	for g := 0; g < 4; g++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			for {
				select {
				case <-stop:
					return
				default:
				}
				ch, cancel := vc.WatchClose()
				select {
				case <-ch:
				case <-time.After(time.Millisecond):
				}
				cancel()
			}
		}()
	}

	wg.Add(1)
	go func() {
		defer wg.Done()
		for i := 0; ; i++ {
			select {
			case <-stop:
				return
			default:
			}
			vc.CancelNotify(fmt.Errorf("round-%d", i))
			_ = vc.ResetNotify(false)
			_ = vc.Err()
		}
	}()

	time.Sleep(300 * time.Millisecond)
	close(stop)
	wg.Wait()
	vc.Close()
}
