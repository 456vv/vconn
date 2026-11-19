package vconn

import (
	"errors"
	"io"
	"testing"
	"time"
)

// TestConnReader_PeekShorterThanP_ReturnsWithoutBlocking 覆盖两个必须「立即返回」的场景：
//
//  1. peek 小于 p、内核接收队列为空、**没有设置** ReadDeadline：
//     已到手的预读字节必须立刻交付，不能为了凑满 p 去读一个会永久阻塞的 socket；
//  2. peek 小于 p、设置了**很长**的 ReadDeadline：
//     同理，不得等到 deadline 才返回。
//
// 实现退化时的表现：场景 1 永久阻塞；场景 2 卡到 deadline 超时才返回。
func TestConnReader_PeekShorterThanP_ReturnsWithoutBlocking(t *testing.T) {
	const seed = "abc"

	cases := []struct {
		name     string
		deadline time.Time
	}{
		{"无 deadline", time.Time{}},
		{"很长的 ReadDeadline", time.Now().Add(time.Hour)},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			a, b := pipePair()
			defer b.Close() // 对端永不写入：内核接收队列保持为空
			vc := NewData(a, []byte(seed))
			defer vc.Close()

			if !tc.deadline.IsZero() {
				if err := vc.SetReadDeadline(tc.deadline); err != nil {
					t.Fatal(err)
				}
			}

			// p 明显大于 peek：正确实现应在交付 peek 后立即返回，而不是继续读 socket 凑满 p。
			buf := make([]byte, len(seed)+64)

			type result struct {
				n   int
				err error
			}
			done := make(chan result, 1)
			start := time.Now()
			go func() {
				n, err := vc.Read(buf)
				done <- result{n: n, err: err}
			}()

			select {
			case r := <-done:
				if r.err != nil || r.n != len(seed) || string(buf[:r.n]) != seed {
					t.Fatalf("Read = (%d, %v) %q，期望 (%d, nil) %q",
						r.n, r.err, buf[:r.n], len(seed), seed)
				}
				if d := time.Since(start); d > 500*time.Millisecond {
					t.Fatalf("peek 小于 p 时不应阻塞或等待 deadline，耗时 %v", d)
				}
			case <-time.After(time.Second):
				// defer 中的 vc.Close()/b.Close() 会解除阻塞的 Read，不会泄漏协程。
				t.Fatalf("peek 小于 p（%s）时 Read 阻塞了，应立即返回", tc.name)
			}
		})
	}
}

// TestConnReader_PendingEOFWithPeek_DeliversPeekFirst 覆盖：
//
//  3. 上一次读遗留 io.EOF（pending）且预读缓冲区非空时，
//     必须先交付预读字节，下一次 Read 才返回遗留的 io.EOF。
//
// 反例：先返回 io.EOF，把已经读到的字节留在缓冲区里（数据丢失 / 交付顺序错乱）。
func TestConnReader_PendingEOFWithPeek_DeliversPeekFirst(t *testing.T) {
	a, b := pipePair()
	defer b.Close()
	const seed = "hello"
	vc := NewData(a, []byte(seed))
	defer vc.Close()

	// 人为制造「遗留 io.EOF + 预读非空」的组合（自然路径难以稳定复现），
	// 与 probe_test.go 中直接写 vc.r.pending 的做法保持一致。
	vc.r.fg.Lock()
	vc.r.pending = io.EOF
	vc.r.fg.Unlock()

	buf := make([]byte, 8)

	// 第一次：peek 优先，必须交付数据而不是 EOF。
	n, err := vc.Read(buf)
	if err != nil || n != len(seed) || string(buf[:n]) != seed {
		t.Fatalf("首次 Read 应交付预读 (%d, %v) %q，期望 (%d, nil) %q",
			n, err, buf[:n], len(seed), seed)
	}
	if vc.r.hasPeek() {
		t.Fatal("首次 Read 后预读缓冲区应已读空")
	}

	// 第二次：peek 已读空，此时才交出遗留的 io.EOF。
	n, err = vc.Read(buf)
	if n != 0 || !errors.Is(err, io.EOF) {
		t.Fatalf("第二次 Read 应返回遗留 io.EOF，得到 (%d, %v)", n, err)
	}
}
