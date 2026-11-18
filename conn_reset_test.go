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

// ==================== 测试辅助 ====================

// awaitErr 等待一次通知（channel 投递后即关闭）
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

// assertClosedEmpty 断言 channel 已关闭且没有残留的未读通知
func assertClosedEmpty(t *testing.T, ch <-chan error, what string) {
	t.Helper()
	select {
	case err := <-ch:
		if err != nil {
			t.Fatalf("%s: 旧 channel 不应被二次投递，得到 %v", what, err)
		}
	default:
		t.Fatalf("%s: channel 应保持已关闭状态", what)
	}
}

// runningWatchers 在测试内部直接读取订阅者数量（公开的 WatcherCount 已移除）
func runningWatchers(c *Conn) int {
	c.mu.Lock()
	defer c.mu.Unlock()
	return len(c.signals)
}

// ==================== 反复订阅 / 重置 ====================

// TestResetNotify_RepeatedRounds 同一连接可以「订阅 → 通知 → 重置」多轮
func TestResetNotify_RepeatedRounds(t *testing.T) {
	s1, s2 := net.Pipe()
	defer s2.Close()

	vc := New(s1)
	defer vc.Close()

	const rounds = 5
	for i := 1; i <= rounds; i++ {
		ch := vc.CloseNotify()
		want := fmt.Errorf("round-%d", i)
		if !vc.CancelNotify(want) {
			t.Fatalf("第 %d 轮 CancelNotify 应返回 true", i)
		}
		if got := awaitErr(t, ch, fmt.Sprintf("第 %d 轮", i)); got == nil || got.Error() != want.Error() {
			t.Fatalf("第 %d 轮通知内容错误: %v", i, got)
		}
		// 第二轮开始前必须重置，否则新订阅者会立刻拿到上一轮结果
		if i < rounds {
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
}

// TestResetNotify_OldChannelNotRedelivered 重置不会把上一轮的 channel 重新投递
func TestResetNotify_OldChannelNotRedelivered(t *testing.T) {
	s1, s2 := net.Pipe()
	defer s2.Close()

	vc := New(s1)
	defer vc.Close()

	ch1 := vc.CloseNotify()
	vc.CancelNotify(errors.New("first"))
	if err := awaitErr(t, ch1, "第一轮"); err == nil {
		t.Fatal("第一轮应携带错误")
	}

	if err := vc.ResetNotify(false); err != nil {
		t.Fatal(err)
	}

	// 旧 channel 保持关闭（无新值），新订阅进入新一轮
	assertClosedEmpty(t, ch1, "第一轮 channel")

	ch2 := vc.CloseNotify()
	vc.CancelNotify(errors.New("second"))
	if err := awaitErr(t, ch2, "第二轮"); err == nil || err.Error() != "second" {
		t.Fatalf("第二轮通知内容错误: %v", err)
	}
	assertClosedEmpty(t, ch1, "第一轮 channel（第二轮后）")
}

// TestResetNotify_ManualNotifyNotPeerDown 手动通知不改写「对端已断开」事实
func TestResetNotify_ManualNotifyNotPeerDown(t *testing.T) {
	s1, s2 := net.Pipe()
	defer s2.Close()

	vc := New(s1)
	defer vc.Close()

	vc.CloseNotify()
	vc.CancelNotify(errors.New("manual"))
	if vc.IsPeerClosed() {
		t.Fatal("CancelNotify 不应把连接标记为对端断开")
	}
	// 因为不是对端断开，重置不需要 force
	if err := vc.ResetNotify(false); err != nil {
		t.Fatalf("手动通知后重置不应被拒绝: %v", err)
	}
}

// TestResetNotify_NoopOnHealthyConn 未落定时重置是幂等的，且不打断正在工作的监控
func TestResetNotify_NoopOnHealthyConn(t *testing.T) {
	vc, srv, ln := newTCPPair(t)
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
	if n := runningWatchers(vc); n != 1 {
		t.Fatalf("幂等重置不应影响订阅者，得到 %d", n)
	}
	// 监控仍然有效：对端关闭仍能收到通知
	srv.Close()
	if err := awaitErr(t, ch, "幂等重置后"); err == nil {
		t.Fatal("对端关闭应携带原因")
	}
}

// TestResetNotify_RearmsMonitor 重置后监控能重新武装并再次发现真实断开
func TestResetNotify_RearmsMonitor(t *testing.T) {
	vc, srv, ln := newTCPPair(t)
	defer ln.Close()

	vc.SetMonitorInterval(20 * time.Millisecond)

	ch1 := vc.CloseNotify()
	vc.CancelNotify(errors.New("manual-1"))
	awaitErr(t, ch1, "第一轮")

	if err := vc.ResetNotify(false); err != nil {
		t.Fatalf("重置失败: %v", err)
	}
	if vc.mon.Load() != nil {
		// 重置时无订阅者：监控应已停（不额外烧系统调用），由新订阅负责启动
		if vc.runningMonitor() != nil {
			t.Fatal("无订阅者时不应保留监控协程")
		}
	}

	ch2 := vc.CloseNotify()
	if vc.runningMonitor() == nil {
		t.Fatal("重新订阅后监控应重新启动")
	}
	srv.Close()

	if err := awaitErr(t, ch2, "重新武装后"); err == nil {
		t.Fatal("应携带断开原因")
	}
	if !vc.IsPeerClosed() {
		t.Fatal("真实断开应标记 IsPeerClosed()=true")
	}
	// 第二轮通知已落定：状态应再次处于「已通知」
	if vc.Err() == nil {
		t.Fatal("重新武装后应再次落定关闭通知")
	}
}

// TestResetNotify_PeerDownNeedsForce 对端已断开时默认拒绝重置，force=true 可越过
func TestResetNotify_PeerDownNeedsForce(t *testing.T) {
	vc, srv, ln := newTCPPair(t)
	defer ln.Close()

	vc.SetMonitorInterval(20 * time.Millisecond)
	ch := vc.CloseNotify()
	srv.Close()
	awaitErr(t, ch, "对端关闭")

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

	// 强制重置后监控会立刻重新探测到底层仍断开，并再次通知（第 2 轮）
	ch2 := vc.CloseNotify()
	if err := awaitErr(t, ch2, "强制重置后"); err == nil {
		t.Fatal("应携带断开原因")
	}
	if vc.Err() == nil {
		t.Fatal("强制重置后应再次落定通知")
	}
}

// TestResetNotify_RefusedAfterClose 本地关闭后不可重置
func TestResetNotify_RefusedAfterClose(t *testing.T) {
	vc, srv, ln := newTCPPair(t)
	defer ln.Close()
	defer srv.Close()

	ch := vc.CloseNotify()
	vc.Close()
	awaitErr(t, ch, "本地关闭")

	if err := vc.ResetNotify(false); !errors.Is(err, ErrResetLocallyClosed) {
		t.Fatalf("期望 ErrResetLocallyClosed，得到 %v", err)
	}
	if err := vc.ResetNotify(true); !errors.Is(err, ErrResetLocallyClosed) {
		t.Fatalf("force 也不能越过本地关闭，得到 %v", err)
	}
	// 关闭后订阅仍然立刻得到落定结果（不阻塞、不泄漏）
	if err := awaitErr(t, vc.CloseNotify(), "关闭后再订阅"); err == nil {
		t.Fatal("关闭后再订阅应得到 net.ErrClosed")
	}
}

// TestResetNotify_RawConnRefused 提取原始连接后不可重置
func TestResetNotify_RawConnRefused(t *testing.T) {
	s1, s2 := net.Pipe()
	defer s2.Close()

	vc := New(s1)
	vc.RawConn()
	if err := vc.ResetNotify(false); !errors.Is(err, ErrResetLocallyClosed) {
		t.Fatalf("期望 ErrResetLocallyClosed，得到 %v", err)
	}
}

// TestResetNotify_KeepsPeekData 重置不清空预读数据
func TestResetNotify_KeepsPeekData(t *testing.T) {
	s1, s2 := net.Pipe()
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
}

// TestResetNotify_NoGoroutineLeak 反复「订阅-通知-重置」不泄漏协程
func TestResetNotify_NoGoroutineLeak(t *testing.T) {
	s1, s2 := net.Pipe()
	defer s2.Close()

	vc := New(s1)
	defer vc.Close()

	// 预热：让监控协程建立/退出模式稳定
	for i := 0; i < 5; i++ {
		ch := vc.CloseNotify()
		vc.CancelNotify(errors.New("warmup"))
		awaitErr(t, ch, "预热")
		if err := vc.ResetNotify(false); err != nil {
			t.Fatal(err)
		}
	}
	time.Sleep(50 * time.Millisecond)
	base := runtime.NumGoroutine()

	for i := 0; i < 100; i++ {
		ch := vc.CloseNotify()
		vc.CancelNotify(errors.New("boom"))
		awaitErr(t, ch, "循环")
		if err := vc.ResetNotify(false); err != nil {
			t.Fatalf("第 %d 轮重置失败: %v", i, err)
		}
	}
	time.Sleep(100 * time.Millisecond)

	after := runtime.NumGoroutine()
	if after > base+8 {
		t.Fatalf("协程泄漏: %d -> %d", base, after)
	}
	if n := runningWatchers(vc); n != 0 {
		t.Fatalf("残留订阅者 %d 个", n)
	}
	if m := vc.runningMonitor(); m != nil {
		t.Fatal("无订阅者时不应残留监控协程")
	}
}

// TestResetNotify_Concurrent 并发「订阅/退订/通知/重置」（配合 -race）
func TestResetNotify_Concurrent(t *testing.T) {
	vc, srv, ln := newTCPPair(t)
	defer ln.Close()
	defer srv.Close()

	vc.SetMonitorInterval(10 * time.Millisecond)

	stop := make(chan struct{})
	var wg sync.WaitGroup

	// 订阅者：反复订阅、退订、等待
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

	// 重置者：反复通知 + 重置
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
