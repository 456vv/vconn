//go:build !plan9 && !js && !wasip1

package vconn

import (
	"errors"
	"io"
	"net"
	"syscall"
	"testing"
	"time"
)

func TestIsCleanPeerClose(t *testing.T) {
	cases := []struct {
		err  error
		want bool
	}{
		{nil, true},
		{io.EOF, true},
		{peerClosedWithUnreadError{}, true},
		{syscall.ECONNRESET, false},
		{syscall.EPIPE, false},
		{net.ErrClosed, false},
	}
	for _, c := range cases {
		if got := isCleanPeerClose(c.err); got != c.want {
			t.Errorf("%v: got %v want %v", c.err, got, c.want)
		}
	}
}

func TestClassifyPeekSentinel(t *testing.T) {
	if res, err := classifyPeek(peekData, true); res != probeReadableHangup ||
		!errors.Is(err, io.EOF) || !errors.Is(err, ErrPeerClosedWithUnread) {
		t.Fatalf("hangup: res=%v err=%v", res, err)
	}
	if res, err := classifyPeek(peekEOF, true); res != probeClosed ||
		!errors.Is(err, io.EOF) || errors.Is(err, ErrPeerClosedWithUnread) {
		t.Fatalf("clean: res=%v err=%v", res, err)
	}
}

func TestNotifyHangupPipe(t *testing.T) {
	cli, srv := net.Pipe()
	c := New(cli)
	defer c.Close()
	c.SetBackgroundReadBuffer(64)
	c.SetMonitorInterval(20 * time.Millisecond)

	ch := c.CloseNotify()
	go func() {
		_, _ = srv.Write([]byte("hello"))
		_ = srv.Close()
	}()

	assertSentinel(t, ch)
	assertPayload(t, c, "hello")
}

func TestNotifyHangupPeekBuffer(t *testing.T) {
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	defer ln.Close()

	go func() {
		s, aerr := ln.Accept()
		if aerr != nil {
			return
		}
		defer s.Close()
		_, _ = s.Write([]byte("payload"))
	}()

	raw, err := net.Dial("tcp", ln.Addr().String())
	if err != nil {
		t.Fatal(err)
	}
	c := New(raw)
	defer c.Close()
	c.SetBackgroundReadBuffer(1024)
	c.SetMonitorInterval(20 * time.Millisecond)

	ch := c.CloseNotify()
	assertSentinel(t, ch)
	assertPayload(t, c, "payload")
}

func assertSentinel(t *testing.T, ch <-chan error) {
	t.Helper()
	select {
	case err := <-ch:
		if !errors.Is(err, io.EOF) {
			t.Fatalf("必须保持 io.EOF 兼容，got %v", err)
		}
		if !errors.Is(err, ErrPeerClosedWithUnread) {
			t.Fatalf("want hangup sentinel, got %v", err)
		}
	case <-time.After(3 * time.Second):
		t.Fatal("没有收到关闭通知")
	}
}

func assertPayload(t *testing.T, c *Conn, want string) {
	t.Helper()
	buf := make([]byte, 32)
	if n, err := io.ReadFull(c, buf[:len(want)]); err != nil || string(buf[:n]) != want {
		t.Fatalf("预读数据丢失: n=%d err=%v", n, err)
	}
	// 读空后 Read 必须返回裸 io.EOF，不得泄漏哨兵（io.Copy 依赖这一点）
	if _, err := c.Read(buf); !errors.Is(err, io.EOF) ||
		errors.Is(err, ErrPeerClosedWithUnread) {
		t.Fatalf("Read 泄漏哨兵: %v", err)
	}
}
