//go:build linux || android

package vconn

import (
	"testing"
	"time"
)

// TestProbeConn_LinuxHangupWithData Linux 上「对端发完数据再 FIN」会同时置
// POLLIN|POLLRDHUP。此时必须报 probeReadableHangup（数据可读 + 已断开），
// 而不是 probeClosed —— 后者会让调用方误以为接收队列已空而提前关闭连接丢数据。
func TestProbeConn_LinuxHangupWithData(t *testing.T) {
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

	var res probeResult
	deadline := time.Now().Add(2 * time.Second)
	for time.Now().Before(deadline) {
		r, _ := probeConn(vc.rwc)
		res = r
		if r == probeReadableHangup {
			break
		}
		time.Sleep(5 * time.Millisecond)
	}
	if res != probeReadableHangup {
		t.Fatalf("期望 probeReadableHangup，得到 %v", res)
	}

	// 数据必须完好
	buf := make([]byte, 8)
	vc.SetReadDeadline(time.Now().Add(time.Second))
	n, err := vc.Read(buf)
	if n != 5 || string(buf[:n]) != "hello" {
		t.Fatalf("数据丢失：n=%d data=%q err=%v", n, buf[:n], err)
	}
}

// TestProbeConn_LinuxPeerFINOnEmptyQueue 接收队列为空时收到 FIN，
// 必须立刻识别为 probeClosed（BSD/macOS 上这种情况只置 POLLIN，是本平台差异点）。
func TestProbeConn_LinuxPeerFINOnEmptyQueue(t *testing.T) {
	vc, srv, ln := tcpPair(t)
	defer ln.Close()
	defer vc.Close()

	if err := srv.Close(); err != nil {
		t.Fatal(err)
	}

	var res probeResult
	deadline := time.Now().Add(2 * time.Second)
	for time.Now().Before(deadline) {
		r, _ := probeConn(vc.rwc)
		res = r
		if r == probeClosed {
			break
		}
		time.Sleep(5 * time.Millisecond)
	}
	if res != probeClosed {
		t.Fatalf("对端 FIN 后应识别为 probeClosed，得到 %v", res)
	}
}
