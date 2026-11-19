//go:build windows

package vconn

import (
	"io"
	"testing"
	"time"
)

// TestProbeConn_WindowsIdleNotClosed 空闲的健康连接必须返回 probeIdle。
//
// Windows 上同步 WSARecv 会阻塞，所以 peekHasDataWin 只在 WSAPoll 报可读后调用；
// 本用例同时守护「不能绕过 poll 直接 peek」这一前提。
func TestProbeConn_WindowsIdleNotClosed(t *testing.T) {
	vc, srv, ln := tcpPair(t)
	defer ln.Close()
	defer srv.Close()
	defer vc.Close()

	for i := 0; i < 20; i++ {
		res, err := probeConn(vc.rwc)
		if res != probeIdle {
			t.Fatalf("空闲连接应为 probeIdle，得到 %v（err=%v）", res, err)
		}
		time.Sleep(2 * time.Millisecond)
	}
}

// TestProbeConn_WindowsReadableAndFIN 有数据时判 probeReadable，
// 数据读空后对端 FIN 判 probeClosed。
func TestProbeConn_WindowsReadableAndFIN(t *testing.T) {
	vc, srv, ln := tcpPair(t)
	defer ln.Close()
	defer vc.Close()

	if _, err := srv.Write([]byte("hi")); err != nil {
		t.Fatal(err)
	}
	if !waitProbe(vc, probeReadable, time.Second) {
		t.Fatal("有数据时未判为 probeReadable")
	}

	buf := make([]byte, 8)
	vc.SetReadDeadline(time.Now().Add(time.Second))
	if n, err := vc.Read(buf); n != 2 || err != nil {
		t.Fatalf("读取失败: n=%d err=%v", n, err)
	}

	if err := srv.Close(); err != nil {
		t.Fatal(err)
	}
	if !waitProbe(vc, probeClosed, 2*time.Second) {
		t.Fatal("对端 FIN 后未判为 probeClosed")
	}
}

// TestProbeConn_WindowsDataThenFIN 有未读数据 + 对端 FIN 时不得判为 probeClosed，
// 且数据必须仍然可读（数据安全优先）。Windows 的 WSAPoll 对 FIN 是否置
// POLLHUP 并不稳定，因此这里不强制要求 probeReadableHangup。
func TestProbeConn_WindowsDataThenFIN(t *testing.T) {
	vc, srv, ln := tcpPair(t)
	defer ln.Close()
	defer vc.Close()

	if _, err := srv.Write([]byte("body")); err != nil {
		t.Fatal(err)
	}
	if !waitProbe(vc, probeReadable, time.Second) {
		t.Fatal("有数据时未判为 probeReadable")
	}
	if err := srv.Close(); err != nil {
		t.Fatal(err)
	}

	deadline := time.Now().Add(300 * time.Millisecond)
	for time.Now().Before(deadline) {
		res, _ := probeConn(vc.rwc)
		if res == probeClosed {
			t.Fatal("有未读数据时被判为 probeClosed，会丢数据")
		}
		time.Sleep(2 * time.Millisecond)
	}

	buf := make([]byte, 8)
	vc.SetReadDeadline(time.Now().Add(time.Second))
	n, err := io.ReadFull(vc, buf[:4])
	if err != nil || n != 4 || string(buf[:n]) != "body" {
		t.Fatalf("未读数据丢失: n=%d data=%q err=%v", n, buf[:n], err)
	}
}

// waitProbe 轮询等待 probeConn 返回期望结果
func waitProbe(c *Conn, want probeResult, d time.Duration) bool {
	deadline := time.Now().Add(d)
	for time.Now().Before(deadline) {
		if res, _ := probeConn(c.rwc); res == want {
			return true
		}
		time.Sleep(2 * time.Millisecond)
	}
	return false
}
