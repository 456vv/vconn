package vconn

import (
	"errors"
	"io"
	"net"
	"testing"
	"time"
)

// TestScenario_RequestResponse 联合使用 Write/Read + WatchClose + Snapshot + CheckReusable + Close。
// 核心价值断言：响应还没读完，就能从通知里得知「对端已结束写方向」。
func TestScenario_RequestResponse(t *testing.T) {
	vc, srv, ln := tcpPair(t)
	defer ln.Close()
	defer srv.Close()
	defer vc.Close()

	vc.SetMonitorInterval(20 * time.Millisecond)
	watch, cancel := vc.WatchClose()
	defer cancel()

	go func() {
		req := make([]byte, 3)
		if _, err := io.ReadFull(srv, req); err != nil {
			return
		}
		_, _ = srv.Write([]byte("RESP"))
		if tc, ok := srv.(*net.TCPConn); ok {
			_ = tc.CloseWrite() // 只关写方向
		}
	}()

	if _, err := vc.Write([]byte("REQ")); err != nil {
		t.Fatalf("Write: %v", err)
	}

	err := awaitErr(t, watch, "对端 FIN")
	if StatusOf(err).Class() != StatusPeerClosed {
		t.Fatalf("通知应归类为对端结束，得到 %v (%v)", StatusOf(err), err)
	}

	// 快照与复用判定必须与通知一致
	if !vc.Snapshot().Ended() {
		t.Fatalf("Snapshot 应报告 Ended，得到 %v", vc.Snapshot())
	}
	if st, rerr := vc.CheckReusable(); st.Class() != StatusPeerClosed || !errors.Is(rerr, ErrNotReusable) {
		t.Fatalf("CheckReusable 应判定不可复用，得到 (%v, %v)", st, rerr)
	}

	// 数据完整可读，读空后是裸 io.EOF
	buf := make([]byte, 4)
	vc.SetReadDeadline(time.Now().Add(time.Second))
	if n, rerr := io.ReadFull(vc, buf); rerr != nil || string(buf[:n]) != "RESP" {
		t.Fatalf("响应丢失: n=%d data=%q err=%v", n, buf[:n], rerr)
	}
	if n, rerr := vc.Read(buf); n != 0 || !errors.Is(rerr, io.EOF) {
		t.Fatalf("读空后应为 (0, EOF)，得到 (%d, %v)", n, rerr)
	}
	if !errors.Is(vc.Err(), io.EOF) {
		t.Fatalf("Err() 应保留结束原因，得到 %v", vc.Err())
	}
	if err := vc.Close(); err != nil {
		t.Fatalf("Close: %v", err)
	}
}

// TestScenario_PoolReuse 联合使用 CheckReusable + CancelNotify + ResetNotify + Snapshot
func TestScenario_PoolReuse(t *testing.T) {
	t.Run("干净连接可复用", func(t *testing.T) {
		vc, srv, ln := tcpPair(t)
		defer ln.Close()
		defer srv.Close()
		defer vc.Close()

		st, err := vc.CheckReusable()
		if st != StatusAlive || err != nil {
			t.Fatalf("干净连接应可复用，得到 (%v, %v)", st, err)
		}
		if vc.Snapshot() != StatusUnverified {
			t.Fatalf("快照不应宣称存活，得到 %v", vc.Snapshot())
		}
	})

	t.Run("脏连接丢弃", func(t *testing.T) {
		vc, srv, ln := tcpPair(t)
		defer ln.Close()
		defer srv.Close()
		defer vc.Close()

		vc.r.pushPeek([]byte("leftover"))
		st, err := vc.CheckReusable()
		if st != StatusDataPending || !errors.Is(err, ErrDataPending) || !st.Terminal() {
			t.Fatalf("脏连接应判定丢弃，得到 (%v, %v)", st, err)
		}
	})

	t.Run("落定通知重置后可复用", func(t *testing.T) {
		vc, srv, ln := tcpPair(t)
		defer ln.Close()
		defer srv.Close()
		defer vc.Close()

		vc.CancelNotify(nil)
		if st, err := vc.CheckReusable(); st != StatusLatched || !errors.Is(err, ErrNotifyLatched) {
			t.Fatalf("应为 StatusLatched，得到 (%v, %v)", st, err)
		}
		if err := vc.ResetNotify(false); err != nil {
			t.Fatalf("ResetNotify: %v", err)
		}
		if st, err := vc.CheckReusable(); st != StatusAlive || err != nil {
			t.Fatalf("重置后应可复用，得到 (%v, %v)", st, err)
		}
	})
}
