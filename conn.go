package vconn

import (
	"errors"
	"io"
	"math"
	"net"
	"os"
	"sync"
	"sync/atomic"
	"time"
)

var errorConnRAWRead = errors.New("vconn: The original connection cannot be read repeatedly")

type atomicBool int32

func (T *atomicBool) isTrue() bool   { return atomic.LoadInt32((*int32)(T)) != 0 }
func (T *atomicBool) isFalse() bool  { return atomic.LoadInt32((*int32)(T)) != 1 }
func (T *atomicBool) setTrue() bool  { return !atomic.CompareAndSwapInt32((*int32)(T), 0, 1) }
func (T *atomicBool) setFalse() bool { return atomic.CompareAndSwapInt32((*int32)(T), 1, 0) }

type CloseNotifier interface {
	CloseNotify() <-chan error // 事件通知
}

type Conn struct {
	rwc                   net.Conn
	closeErr              error
	closeSignal           []chan error
	r                     *connReader
	readDeadline          time.Time
	writeDeadline         time.Time
	m                     sync.Mutex
	closed                atomicBool
	rawRead               atomicBool
	disableBackgroundRead bool
	backgroundReadDiscard bool
}

func New(c net.Conn) *Conn {
	if conn, ok := c.(*Conn); ok {
		return conn
	}
	conn := &Conn{rwc: c}
	conn.r = &connReader{conn: conn}
	return conn
}

func NewConn(c net.Conn) net.Conn {
	return New(c)
}

// 返回原始连接
func (T *Conn) RawConn() net.Conn {
	if T.rawRead.setTrue() {
		panic(errorConnRAWRead)
	}

	T.m.Lock()
	defer T.m.Unlock()

	T.DisableBackgroundRead(true)
	T.close()

	rwc := T.rwc
	T.rwc = nil
	T.r = nil
	return rwc
}

// 后台读取丢弃，只要用于连接加入连接池后，期间收到的数据全部丢弃。
// 用于特殊环境，普通用户正常不需要用到他。
func (T *Conn) SetBackgroundReadDiscard(y bool) {
	T.backgroundReadDiscard = y
}

// 设置读取限制
//	remain int	设置 0 默认限制为(math.MaxInt32)
func (T *Conn) SetReadLimit(remain int) {
	T.r.setReadLimit(remain)
}

// 停止后台实时监听的连接关闭状态，在你自己调用Read或Write发生错误，依然 CloseNotify 返回连接关闭信号。
func (T *Conn) DisableBackgroundRead(y bool) {
	T.disableBackgroundRead = y
	if y {
		// 中止后台读取
		T.r.abortBackgroundRead()
	}
}

// 注意：这里会有两个通知，1）远程主动断开 2）本地调用断开
// 如果你是用于断开连接重连，需要判断返回的 error 状态。
// error != nil 表示远程主动断开（一般用于这个）
// error == nil 表示本地调用断开（关闭连接，通道关闭，返回nil）
func (T *Conn) CloseNotify() <-chan error {
	T.m.Lock()
	defer T.m.Unlock()

	c := make(chan error, 1)
	if T.closeErr != nil {
		c <- T.closeErr
		return c
	}

	T.r.startBackgroundRead()
	T.closeSignal = append(T.closeSignal, c)
	return c
}

func (T *Conn) closeNotify(err error) {
	T.m.Lock()
	defer T.m.Unlock()

	if isCommonNetError(err) {

		for _, c := range T.closeSignal {
			c <- err
			close(c)
		}
		T.closeSignal = nil
		T.closeErr = err
	}
}

func (T *Conn) Read(b []byte) (n int, err error) {
	n, err = T.r.Read(b)
	T.closeNotify(err)
	// 仅限在用户主动读取的时候，并之前没有收到通知事件情况下才能再次开启后台监听
	// 因为用户主动读取时候关闭了后台监听
	if T.closeErr == nil && len(T.closeSignal) != 0 {
		T.r.startBackgroundRead()
	}
	return
}

func (T *Conn) Write(b []byte) (n int, err error) {
	n, err = T.rwc.Write(b)
	T.closeNotify(err)
	return
}

func (T *Conn) Close() error {
	T.m.Lock()
	defer T.m.Unlock()
	return T.close()
}

func (T *Conn) close() error {
	for _, c := range T.closeSignal {
		close(c)
	}
	T.closeSignal = nil
	T.closeErr = io.EOF

	if T.rwc != nil {
		return T.rwc.Close()
	}
	return nil
}

func (T *Conn) LocalAddr() net.Addr {
	return T.rwc.LocalAddr()
}

func (T *Conn) RemoteAddr() net.Addr {
	return T.rwc.RemoteAddr()
}

func (T *Conn) SetDeadline(t time.Time) error {
	T.readDeadline = t
	T.writeDeadline = t
	return T.rwc.SetDeadline(t)
}

func (T *Conn) SetReadDeadline(t time.Time) error {
	T.readDeadline = t
	return T.rwc.SetReadDeadline(t)
}

func (T *Conn) SetWriteDeadline(t time.Time) error {
	T.writeDeadline = t
	return T.rwc.SetWriteDeadline(t)
}

type connReader struct {
	conn *Conn // 上级

	mu         sync.Mutex // 锁
	hasByte    atomicBool // 检测有数据
	byteBuf    [1]byte    // 第一个数据，检测时候得到一个数据
	cond       *sync.Cond // 组
	inRead     atomicBool // 正在读取
	aborted    atomicBool // 结束
	inBackRead atomicBool // 当前是在后台读取
	remain     int        // 限制读取
	readLimit  bool
}

// 锁，条件等待
func (T *connReader) lock() {
	T.mu.Lock()
	if T.cond == nil {
		T.cond = sync.NewCond(&T.mu)
	}
}

// 解锁
func (T *connReader) unlock() { T.mu.Unlock() }

// 设置读取限制
//	remain int	设置 0 默认限制为(math.MaxInt32)
func (T *connReader) setReadLimit(remain int) {
	if remain == 0 {
		T.remain = math.MaxInt32
		T.readLimit = false
		return
	}
	T.remain = remain
	T.readLimit = true
}

// 超出读取限制
func (T *connReader) hitReadLimit() bool {
	if !T.readLimit {
		return false
	}
	return T.remain <= 0
}

// 开始后台读取
func (T *connReader) startBackgroundRead() {
	if T.inRead.isTrue() || T.conn.disableBackgroundRead {
		return
	}
	if T.hasByte.isTrue() {
		return
	}
	go T.backgroundRead()
}

// 后台读取
func (T *connReader) backgroundRead() {
	T.lock()
	if T.conn.disableBackgroundRead {
		T.unlock()
		return
	}
	if T.inRead.setTrue() { // 这里是设置，并返回原值
		T.unlock()
		return
	}
	T.inBackRead.setTrue()
	T.unlock()
	var n int
	var err error
	if T.conn.backgroundReadDiscard {
		buf := make([]byte, 512)
		for T.aborted.isFalse() { // 多线程，已经中止后，防止依然执行。
			T.conn.rwc.SetReadDeadline(time.Time{})
			_, err = T.conn.rwc.Read(buf)
			if err != nil {
				break
			}
		}
	} else if T.aborted.isFalse() { // 多线程，已经中止后，防止依然执行。
		T.conn.rwc.SetReadDeadline(time.Time{})
		n, err = T.conn.rwc.Read(T.byteBuf[:])
	}
	T.lock()
	if n == 1 {
		T.hasByte.setTrue()
	}
	if ne, ok := err.(net.Error); ok && T.aborted.isTrue() && ne.Timeout() {
		// 忽略这个错误。 这是另一个调用abortPendingRead的例程的预期错误。
	} else if err != nil {
		// 主动关闭连接，造成读取失败
		T.conn.closeNotify(err)
	}
	T.aborted.setFalse()
	T.inRead.setFalse()
	T.inBackRead.setFalse()
	T.unlock()
	T.cond.Broadcast()
}

// 中止后台读取
func (T *connReader) abortBackgroundRead() {
	T.lock()
	if T.inBackRead.isTrue() {
		T.unlock()
		T.abortPendingRead()
		return
	}
	T.unlock()
}

// 中止正在读取
func (T *connReader) abortPendingRead() {
	T.lock()
	defer T.unlock()
	if T.inRead.isFalse() {
		return
	}
	T.aborted.setTrue()
	T.conn.rwc.SetReadDeadline(time.Unix(1, 0))
	for T.inRead.isTrue() {
		T.cond.Wait()
	}
	T.conn.rwc.SetReadDeadline(T.conn.readDeadline)
}

// 读取数据
func (T *connReader) Read(p []byte) (n int, err error) {
	if len(p) == 0 {
		return 0, nil
	}

	T.lock()
	for T.inRead.isTrue() {
		T.unlock()
		T.abortPendingRead()
		T.lock()
	}

	if T.hitReadLimit() {
		T.unlock()
		return 0, io.EOF
	}

	if T.readLimit && len(p) > T.remain {
		p = p[:T.remain]
	}

	pt := p
	hasByte := T.hasByte.isTrue()
	if hasByte {
		p[0] = T.byteBuf[0]
		T.hasByte.setFalse()
		if len(p) == 1 {
			T.unlock()
			T.remain--
			return 1, nil
		}
		pt = p[1:]
	}
	T.inRead.setTrue()
	T.unlock()
	n, err = T.conn.rwc.Read(pt)
	T.inRead.setFalse()
	T.cond.Broadcast()
	if hasByte {
		n++
		err = nil
	}
	if T.readLimit {
		T.remain -= n
	}
	return n, err
}

func isCommonNetError(err error) bool {
	nerr := err
	if op, ok := nerr.(*net.OpError); ok {
		if op.Timeout() {
			return false
		}
		nerr = op.Err
	}
	if sys, ok := nerr.(*os.SyscallError); ok {
		nerr = sys.Err
	}

	if isConnError(nerr) || nerr == io.EOF {
		return true
	}
	//这里是常规错误，不使用
	//switch se.Syscall {
	//case "wsarecv", "wsasend", "wsarecvfrom", "wsasendto", "wsarecvmsg", "wsasendmsg":
	//	fallthrough
	//case "read", "write", "recvfrom", "write", "recvmsg", "sendmsg":
	//	return true
	//default:
	//	return false
	//}
	return false
}
