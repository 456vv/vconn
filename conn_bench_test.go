package vconn

import (
	"io"
	"net"
	"testing"
	"time"
)

// memBenchConn 永远有数据可读的内存连接，用于测量包装层自身开销
type memBenchConn struct {
	data []byte
	off  int
}

func (m *memBenchConn) Read(p []byte) (int, error) {
	if m.off >= len(m.data) {
		m.off = 0
	}
	n := copy(p, m.data[m.off:])
	m.off += n
	return n, nil
}
func (m *memBenchConn) Write(p []byte) (int, error)        { return len(p), nil }
func (m *memBenchConn) Close() error                       { return nil }
func (m *memBenchConn) LocalAddr() net.Addr                { return &net.TCPAddr{} }
func (m *memBenchConn) RemoteAddr() net.Addr               { return &net.TCPAddr{} }
func (m *memBenchConn) SetDeadline(t time.Time) error      { return nil }
func (m *memBenchConn) SetReadDeadline(t time.Time) error  { return nil }
func (m *memBenchConn) SetWriteDeadline(t time.Time) error { return nil }

func benchRead(b *testing.B, wrap, notify bool) {
	const size = 4096
	data := make([]byte, size*2)

	var r io.Reader = &memBenchConn{data: data}
	if wrap {
		vc := New(&memBenchConn{data: data})
		if notify {
			_ = vc.CloseNotify()
		}
		r = vc
	}

	buf := make([]byte, size)
	b.SetBytes(size)
	b.ResetTimer()
	for i := 0; i < b.N; i++ {
		if _, err := r.Read(buf); err != nil {
			b.Fatal(err)
		}
	}
	b.StopTimer()
}

// BenchmarkRead_Plain 裸 conn 基线
func BenchmarkRead_Plain(b *testing.B) { benchRead(b, false, false) }

// BenchmarkRead_Wrapped 仅包装，不订阅关闭通知
func BenchmarkRead_Wrapped(b *testing.B) { benchRead(b, true, false) }

// BenchmarkRead_WrappedNotify 包装 + 订阅关闭通知（旧版此处约 7.5x 开销）
func BenchmarkRead_WrappedNotify(b *testing.B) { benchRead(b, true, true) }
