package vconn

import (
	"net"
	"testing"
)

// TestConn_TmpBufSizing 固定 tmpBuf 的长度契约：
//   - 预读模式（want > 0）必须恰好等于 want，多读会超出预读配额；
//   - 丢弃模式（want < 0）取 |want|，不足 minPeekBuf 时按 minPeekBuf 分配；
//   - 两种模式都不超过 maxPeekSize。
func TestConn_TmpBufSizing(t *testing.T) {
	c1, c2 := net.Pipe()
	defer c2.Close()
	c := New(c1)
	defer c.Close()

	cases := []struct {
		want int
		exp  int
	}{
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
}

// TestConn_DiscardReadBufferSize 验证丢弃模式确实把 |n| 用作单次读取长度
// （net.Pipe 无内核探测，走的是带 peekReadTimeout 的阻塞预读路径，读会超时返回）。
func TestConn_DiscardReadBufferSize(t *testing.T) {
	c1, c2 := net.Pipe()
	defer c2.Close()
	c := New(c1)
	defer c.Close()

	for _, tc := range []struct {
		size int
		exp  int
	}{
		{-1, minPeekBuf},
		{-minPeekBuf - 1, minPeekBuf + 1},
		{-8192, 8192},
	} {
		c.r.peekTmp = nil // 每次从零开始，便于观察本次分配
		c.SetBackgroundReadBuffer(tc.size)

		closed, err := c.r.peekStep(nil, probeUnsupported, false)
		if closed || err != nil {
			t.Fatalf("peekSize=%d: closed=%v err=%v", tc.size, closed, err)
		}
		if got := cap(c.r.peekTmp); got != tc.exp {
			t.Errorf("peekSize=%d: cap(peekTmp) = %d, want %d", tc.size, got, tc.exp)
		}
	}
}
