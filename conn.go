package vconn

import (
	"net"
	"sync"
	"sync/atomic"
	"time"
)

// CloseNotifier 提供连接关闭通知能力
type CloseNotifier interface {
	CloseNotify() <-chan error
}

// Conn 封装了标准的 net.Conn，具备高级异步断开监控、限流及防死锁提取功能
// 所有公开方法均为并发安全
type Conn struct {
	rwc net.Conn // 底层真实连接

	mu           sync.RWMutex // 保护 closeErr 和 closeSignals
	closeErr     error        // 首次关闭错误（只记录一次）
	closeSignals []chan error // 关闭通知管道队列

	r *connReader // 读取器，负责后台预读和限流

	// 用户设置的超时时间快照，用于恢复被后台读取修改的 deadline
	readDeadline  atomic.Value // time.Time
	writeDeadline atomic.Value // time.Time

	closed                atomic.Bool // 连接是否已关闭
	rawReadUsed           atomic.Bool // 原始连接是否已被提取
	disableBackgroundRead atomic.Bool // 是否禁用后台读取
	backgroundReadDiscard atomic.Bool // 后台读取是否丢弃数据
}

// New 包装原始连接。若已被包装，则直接返回本身
// 参数 c 不能为 nil，否则会 panic
func New(c net.Conn) *Conn {
	if c == nil {
		panic("vconn: nil conn")
	}
	// 避免重复包装
	if vc, ok := c.(*Conn); ok {
		return vc
	}
	conn := &Conn{rwc: c}
	conn.r = newConnReader(conn)
	// 初始化 deadline 为零值
	conn.readDeadline.Store(time.Time{})
	conn.writeDeadline.Store(time.Time{})
	return conn
}

// NewConn 是 New 的别名，返回 net.Conn 接口
func NewConn(c net.Conn) net.Conn {
	return New(c)
}

// RawConn 剥离包装，归还底层真实连接
// 剥离后，此 Conn 无法再读写，任何操作将返回 net.ErrClosed
// 该方法只能调用一次，重复调用会 panic
func (c *Conn) RawConn() net.Conn {
	// 使用 CAS 确保只能提取一次
	if c.rawReadUsed.Swap(true) {
		panic(ErrRawConnAlreadyUsed)
	}

	if c.closed.Swap(true) {
		// 已经关闭，直接返回底层连接
		return c.rwc
	}

	// 先禁用后台读取，再标记为已关闭
	c.disableBackgroundRead.Store(true)

	// 通知所有等待者连接已关闭
	c.notifyClose(net.ErrClosed)

	// 中断后台读取协程
	c.r.abortBackgroundRead()

	return c.rwc
}

// RawConnFull 归还原始连接，如果后台已经预读了 1 字节，会将其写回传入的 b 切片首位
// 返回值：(原始连接, 预读字节数)
func (c *Conn) RawConnFull(b []byte) (net.Conn, int) {
	if c.r.hasPeekedByte() {
		if len(b) > 0 {
			b[0] = c.r.peekedByte()
		}
		return c.RawConn(), 1
	}
	return c.RawConn(), 0
}

// SetBackgroundReadDiscard 设置后台读取是否丢弃数据
// true: 循环读取并丢弃数据（用于检测断开）
// false: 只预读 1 字节并缓存（默认模式）
func (c *Conn) SetBackgroundReadDiscard(y bool) {
	c.backgroundReadDiscard.Store(y)
}

// SetReadLimit 设置读取限制字节数
// remain < 0: 不限制
// remain >= 0: 限制剩余可读字节数，超出后返回 io.EOF
func (c *Conn) SetReadLimit(remain int64) {
	c.r.setReadLimit(remain)
}

// DisableBackgroundRead 禁用或启用后台读取
// 禁用后，CloseNotify 将无法自动检测对端断开
func (c *Conn) DisableBackgroundRead(y bool) {
	c.disableBackgroundRead.Store(y)
	if y {
		c.r.abortBackgroundRead()
	}
}

// CloseNotify 获取关闭通知 Channel
// 支持多次调用；在对端意外断开或本地主动 Close 时均会触发通知
// 返回的 channel 会在连接关闭时接收到错误并关闭
func (c *Conn) CloseNotify() <-chan error {
	ch := make(chan error, 1)

	c.mu.Lock()
	if c.closeErr != nil {
		// 连接已关闭，立即通知
		closeErr := c.closeErr
		c.mu.Unlock()
		ch <- closeErr
		close(ch)
		return ch
	}

	// 注册通知管道
	c.closeSignals = append(c.closeSignals, ch)
	c.mu.Unlock()

	// 启动后台读取以监控连接状态
	c.r.startBackgroundRead()
	return ch
}

// IsClosed 返回连接是否已关闭
func (c *Conn) IsClosed() bool {
	return c.closed.Load()
}

// Read 从连接读取数据
// 实现 io.Reader 接口，线程安全
func (c *Conn) Read(b []byte) (n int, err error) {
	if c.closed.Load() {
		return 0, net.ErrClosed
	}

	n, err = c.r.Read(b)

	// 如果发生网络错误，通知所有等待者
	c.notifyClose(err)
	if err == nil {
		// 若前台 Read 顺利完成且连接依旧活跃，自动重新挂起后台监测协程
		c.mu.RLock()
		hasSignals := len(c.closeSignals) > 0
		c.mu.RUnlock()
		if hasSignals {
			c.r.startBackgroundRead()
		}
	}
	return
}

// Write 向连接写入数据
// 实现 io.Writer 接口，线程安全
func (c *Conn) Write(b []byte) (n int, err error) {
	if c.closed.Load() {
		return 0, net.ErrClosed
	}

	n, err = c.rwc.Write(b)
	// 如果发生网络错误，通知所有等待者
	c.notifyClose(err)
	return
}

// Close 主动关闭连接，干净清理所有通知管道
// 实现 io.Closer 接口，线程安全，可重复调用
func (c *Conn) Close() error {
	// 使用 CAS 确保只关闭一次
	if c.closed.Swap(true) {
		return nil
	}

	// 通知所有等待者
	c.notifyClose(net.ErrClosed)

	// 中断后台读取
	c.r.abortBackgroundRead()

	return c.rwc.Close()
}

// notifyClose 在探测到对端非正常断开时分发错误通知
// 只有首次调用会真正通知，后续调用会被忽略
func (c *Conn) notifyClose(err error) {
	// 只处理网络错误
	if !isCommonNetError(err) {
		return
	}

	c.mu.Lock()
	// 已经通知过，直接返回
	if c.closeErr != nil {
		c.mu.Unlock()
		return
	}

	// 记录首次错误，取出所有通知管道
	closeSignals := c.closeSignals
	c.closeErr = err
	c.closeSignals = nil
	c.mu.Unlock()

	// 在锁外通知，避免死锁
	for _, ch := range closeSignals {
		// 非阻塞发送，避免接收者未准备好导致阻塞
		select {
		case ch <- err:
		default:
		}
		close(ch)
	}
}

// ==================== net.Conn 接口实现 ====================

// LocalAddr 返回本地网络地址
func (c *Conn) LocalAddr() net.Addr {
	if c.closed.Load() {
		return nil
	}
	return c.rwc.LocalAddr()
}

// RemoteAddr 返回远程网络地址
func (c *Conn) RemoteAddr() net.Addr {
	if c.closed.Load() {
		return nil
	}
	return c.rwc.RemoteAddr()
}

// SetDeadline 同时设置读写超时时间
func (c *Conn) SetDeadline(t time.Time) error {
	c.readDeadline.Store(t)
	c.writeDeadline.Store(t)
	if c.closed.Load() {
		return nil
	}
	return c.rwc.SetDeadline(t)
}

// SetReadDeadline 设置读取超时时间
func (c *Conn) SetReadDeadline(t time.Time) error {
	c.readDeadline.Store(t)
	if c.closed.Load() {
		return nil
	}
	return c.rwc.SetReadDeadline(t)
}

// SetWriteDeadline 设置写入超时时间
func (c *Conn) SetWriteDeadline(t time.Time) error {
	c.writeDeadline.Store(t)
	if c.closed.Load() {
		return nil
	}
	return c.rwc.SetWriteDeadline(t)
}

// SetReadBuffer 设置操作系统接收缓冲区大小（仅 TCP 连接有效）
func (c *Conn) SetReadBuffer(bytes int) error {
	if !c.closed.Load() {
		if tc, ok := c.rwc.(*net.TCPConn); ok {
			return tc.SetReadBuffer(bytes)
		}
	}
	return nil
}

// SetWriteBuffer 设置操作系统发送缓冲区大小（仅 TCP 连接有效）
func (c *Conn) SetWriteBuffer(bytes int) error {
	if !c.closed.Load() {
		if tc, ok := c.rwc.(*net.TCPConn); ok {
			return tc.SetWriteBuffer(bytes)
		}
	}
	return nil
}
