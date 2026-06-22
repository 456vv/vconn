package vconn

import (
	"io"
	"sync"
	"sync/atomic"
	"time"
)

// connReader 负责协调后台预读与前台用户主动读取的冲突与转换
// 所有方法均为并发安全
type connReader struct {
	conn *Conn

	mu   sync.Mutex // 保护 peekByte 和状态转换
	cond *sync.Cond // 用于等待后台读取退出

	peeked   atomic.Bool // 是否已预读到 1 字节
	peekByte []byte      // 缓存的预读字节（受 mu 保护）

	inRead  atomic.Bool // 标志当前是否有活跃的读取流程（前台或后台）
	aborted atomic.Bool // 标志后台预读是否正处于中断恢复流程

	readLimit atomic.Int64 // 限流字节数: -1 表示无限制，>= 0 表示剩余可用配额
}

func newConnReader(c *Conn) *connReader {
	r := &connReader{conn: c}
	r.cond = sync.NewCond(&r.mu)
	r.readLimit.Store(-1) // 默认不限流
	return r
}

// setReadLimit 设置读取限制。-1 为不限制，>=0 限制具体长度
// 线程安全
func (r *connReader) setReadLimit(remain int64) {
	if remain < 0 {
		r.readLimit.Store(-1)
	} else {
		r.readLimit.Store(remain)
	}
}

// hasPeekedByte 返回是否已预读到 1 字节
// 线程安全
func (r *connReader) hasPeekedByte() bool {
	return r.peeked.Load()
}

// peekedByte 返回预读的字节
// 调用前应先检查 hasPeekedByte()
func (r *connReader) peekedByte() []byte {
	r.mu.Lock()
	b := r.peekByte
	r.peekByte = nil
	r.peeked.Store(false)
	r.mu.Unlock()
	return b
}

// startBackgroundRead 安全拉起一个后台预读协程
// 线程安全，可重复调用（会自动去重）
func (r *connReader) startBackgroundRead() {
	// 如果连接已关闭或后台读取被禁用，无需启动
	if r.conn.disableBackgroundRead.Load() || r.conn.IsClosed() {
		return
	}

	r.mu.Lock()
	defer r.mu.Unlock()

	// 若当前有读操作正在运行、已存在预读数据或正处于中断流程，无需重复启动
	if r.inRead.Load() || r.peeked.Load() || r.aborted.Load() {
		return
	}

	r.inRead.Store(true)
	go r.backgroundReadLoop()
}

// backgroundReadLoop 执行后台阻塞读，用于捕获远端主动断开事件
// 该方法在独立 goroutine 中运行
func (r *connReader) backgroundReadLoop() {
	// 这里上锁防止多线程环境下，调用 backgroundReadLoop() 和 abortBackgroundRead() 设置了混乱的时间。
	r.mu.Lock()
	if r.conn.IsClosed() || r.aborted.Load() {
		r.inRead.Store(false)
		r.mu.Unlock()
		// 由于调用 abortBackgroundRead() 的时候会等待后台读取完全退出
		// 可能 backgroundReadLoop() 在等待锁的过程中，优先 abortBackgroundRead() 执行了 r.cond.Wait() 等待退出，此时需要唤醒等待，否则会造成死锁。
		r.cond.Broadcast()
		return
	}

	// 取消前台读取的任何超时设置，确保后台读取以阻塞方式等待数据或对端关闭
	// 这里不检查错误，因为连接可能已关闭
	r.conn.rwc.SetReadDeadline(time.Time{})
	r.mu.Unlock()

	var err error
	var n int
	if r.conn.backgroundReadDiscard.Load() {
		// 丢弃模式：循环读取并扔掉数据，直到出错或被中断
		buf := make([]byte, 4096) // 使用较大缓冲区提升性能
		for !r.aborted.Load() && !r.conn.IsClosed() {
			_, err = r.conn.rwc.Read(buf)
			if err != nil {
				break
			}
		}
	} else if r.conn.backgroundReadBuffer > 0 {
		// 预读模式：仅读取指定字节存入缓存区，直到出错或被中断或缓存已满
		buf := make([]byte, r.conn.backgroundReadBuffer)
		for !r.aborted.Load() && !r.conn.IsClosed() {
			r.mu.Lock()
			lpb := len(r.peekByte)
			remainSize := r.conn.backgroundReadBuffer - lpb
			r.mu.Unlock()
			if lpb >= r.conn.backgroundReadBuffer {
				break
			}
			n, err = r.conn.rwc.Read(buf[:remainSize])
			if n > 0 {
				r.mu.Lock()
				r.peekByte = append(r.peekByte, buf[:n]...)
				r.peeked.Store(true)
				r.mu.Unlock()
			}
			if err != nil {
				break
			}
		}
	} else if !r.aborted.Load() && !r.conn.IsClosed() {
		// 预读模式，但未设置缓存大小，仅读取一个字节
		buf := make([]byte, 1)
		n, err = r.conn.rwc.Read(buf)
		if n > 0 {
			r.mu.Lock()
			r.peekByte = append(r.peekByte, buf[0])
			r.peeked.Store(true)
			r.mu.Unlock()
		}
	}

	// 标记读取完成
	r.mu.Lock()
	r.inRead.Store(false)
	aborted := r.aborted.Load() // 记录是否为中断退出
	r.mu.Unlock()

	// 唤醒所有可能在等待后台读取退出的协程
	r.cond.Broadcast()

	// 如果是非中断退出且发生了网络异常，通知上层连接已关闭
	if err != nil && !aborted {
		// 只处理网络错误
		if isCommonNetError(err) {
			r.conn.notifyClose(err)
		}
	}
}

// abortBackgroundRead 安全地强行中断正在运行的后台读取协程
// 该方法会阻塞直到后台读取完全退出
// 线程安全
func (r *connReader) abortBackgroundRead() {
	r.mu.Lock()
	defer r.mu.Unlock()

	// 没有后台读取在运行，直接返回
	if !r.inRead.Load() {
		return
	}

	// 标记为中断状态
	r.aborted.Store(true)

	// 设置极小超时（过去的Unix时间戳），强行逼退底层阻塞的 Read 系统调用
	r.conn.rwc.SetReadDeadline(time.Unix(0, 1))

	// 阻塞等待后台读取协程完全退出
	for r.inRead.Load() {
		r.cond.Wait()
	}

	// 恢复用户原先设置的前台读取超时时间
	if deadline, ok := r.conn.readDeadline.Load().(time.Time); ok {
		_ = r.conn.rwc.SetReadDeadline(deadline)
	}

	// 清除中断标志
	r.aborted.Store(false)
}

// Read 实现流式读取，具备限流、预读字节消费等复合功能
// 实现 io.Reader 接口，线程安全
func (r *connReader) Read(p []byte) (n int, err error) {
	if len(p) == 0 {
		return 0, nil
	}

	// 1. 处理 Read Limit 限流控制
	limit := r.readLimit.Load()
	if limit == 0 {
		return 0, io.EOF
	}

	// 限制本次读取长度
	if limit > 0 && int64(len(p)) > limit {
		p = p[:limit]
	}

	// 若当前后台协程正在霸占读取，先将其踢出并等待其彻底退出
	for r.inRead.Load() {
		r.abortBackgroundRead()
	}

	r.mu.Lock()
	// 2. 优先消费后台已预读出字节
	// 读取旧的标志并设置新的标志
	if r.peeked.Load() {
		pl := len(r.peekByte)
		if len(p) < pl {
			pl = len(p)
		}
		n = copy(p[:pl], r.peekByte[:pl])
		r.peekByte = r.peekByte[n:]

		if len(r.peekByte) == 0 {
			r.peeked.Store(false)
		}

		// 如果只需要相同长度字节，直接返回
		if len(p) == pl {
			r.mu.Unlock()
			// 扣减限流配额
			if limit > 0 {
				r.readLimit.Add(-int64(n))
			}
			return n, nil
		}

		// 继续读取剩余部分
		p = p[n:]
	}

	// 检查连接状态
	if r.conn.IsClosed() {
		r.mu.Unlock()
		return n, io.EOF
	}

	// 3. 锁定状态，准备发起前台真实网络读取
	r.inRead.Store(true)
	r.mu.Unlock()

	// 执行真实读取（在锁外，避免阻塞其他操作）
	var rn int
	rn, err = r.conn.rwc.Read(p)

	// 标记读取完成
	r.mu.Lock()
	r.inRead.Store(false)
	r.mu.Unlock()
	r.cond.Broadcast()

	n += rn

	// 扣减限流配额
	if limit > 0 && n > 0 {
		r.readLimit.Add(-int64(n))
	}

	return n, err
}
