package vconn

import (
	"sync"
	"testing"
	"time"
)

// TestConcurrent_NoSpuriousTimeout 健康空闲连接上的并发读不得被注入伪超时
func TestConcurrent_NoSpuriousTimeout(t *testing.T) {
	vc, srv, ln := tcpPair(t)
	defer ln.Close()
	defer srv.Close()
	defer vc.Close()

	vc.CloseNotify()

	const readers = 4
	errc := make(chan error, readers)
	for range readers {
		go func() {
			buf := make([]byte, 64)
			_, err := vc.Read(buf)
			errc <- err
		}()
	}
	time.Sleep(150 * time.Millisecond) // 让全部读者进入阻塞

	go func() {
		for range 16 {
			if _, err := srv.Write([]byte("0123456789")); err != nil {
				return
			}
			time.Sleep(5 * time.Millisecond)
		}
	}()

	deadline := time.After(3 * time.Second)
	for range readers {
		select {
		case err := <-errc:
			if err != nil {
				t.Fatalf("健康连接上并发读被注入错误: %v", err)
			}
		case <-deadline:
			t.Fatal("并发读超时")
		}
	}
}

// TestConcurrent_Stress 多线程联合压力：读 / 写 / 探测 / 订阅 同时进行。
// 线程数由表驱动，便于横向比较开销；配合 -race 使用。
func TestConcurrent_Stress(t *testing.T) {
	cases := []struct {
		name             string
		readers, writers int
		probers          int
	}{
		{"2读/2写/2探测", 2, 2, 2},
		{"8读/4写/4探测", 8, 4, 4},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			vc, srv, ln := tcpPair(t)
			defer ln.Close()

			_ = vc.CloseNotify()
			go func() { // 回显
				buf := make([]byte, 512)
				for {
					n, err := srv.Read(buf)
					if err != nil {
						return
					}
					if _, werr := srv.Write(buf[:n]); werr != nil {
						return
					}
				}
			}()

			stop := make(chan struct{})
			var wg sync.WaitGroup

			for i := 0; i < tc.writers; i++ {
				wg.Add(1)
				go func() {
					defer wg.Done()
					payload := []byte("ping-pong")
					for {
						select {
						case <-stop:
							return
						default:
						}
						if _, err := vc.Write(payload); err != nil {
							return
						}
						time.Sleep(time.Millisecond)
					}
				}()
			}

			for i := 0; i < tc.readers; i++ {
				wg.Add(1)
				go func() {
					defer wg.Done()
					buf := make([]byte, 64)
					for {
						select {
						case <-stop:
							return
						default:
						}
						_ = vc.SetReadDeadline(time.Now().Add(20 * time.Millisecond))
						if _, err := vc.Read(buf); err != nil && !isTimeoutErr(err) {
							return
						}
					}
				}()
			}

			for i := 0; i < tc.probers; i++ {
				wg.Add(1)
				go func() {
					defer wg.Done()
					for {
						select {
						case <-stop:
							return
						default:
						}
						_, _ = vc.Check()
						_, _ = vc.CheckBlocking()
						_, _ = vc.CheckReusable()
						_ = vc.Snapshot()
						_ = vc.Err()
						ch, cancel := vc.WatchClose()
						select {
						case <-ch:
						case <-time.After(time.Millisecond):
						}
						cancel()
						time.Sleep(2 * time.Millisecond)
					}
				}()
			}

			time.Sleep(300 * time.Millisecond)
			close(stop)
			_ = vc.Close()

			done := make(chan struct{})
			go func() { wg.Wait(); close(done) }()
			select {
			case <-done:
			case <-time.After(5 * time.Second):
				t.Fatal("并发压力测试出现死锁/挂起")
			}
			_ = srv.Close()
		})
	}
}
