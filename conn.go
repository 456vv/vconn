package vconn

import(
	"net"
	"time"
	"sync"
	"io"
	"sync/atomic"
)
type atomicBool int32
func (T *atomicBool) isTrue() bool 	{ return atomic.LoadInt32((*int32)(T)) != 0 }
func (T *atomicBool) isFalse() bool	{ return atomic.LoadInt32((*int32)(T)) != 1 }
func (T *atomicBool) setTrue() bool	{ return !atomic.CompareAndSwapInt32((*int32)(T), 0, 1)}
func (T *atomicBool) setFalse() bool{ return atomic.CompareAndSwapInt32((*int32)(T), 1, 0)}

type CloseNotifier interface {
    CloseNotify() <-chan error
}

type Conn struct {
	rwc				net.Conn
	closedSignal 	chan error
	r				*connReader
	readDeadline	time.Time
	m				sync.Mutex
	closed			atomicBool
}
func NewConn(c net.Conn) net.Conn {
	if conn, ok := c.(*Conn); ok {
		return conn
	}
	conn := &Conn{rwc:c, closedSignal:make(chan error, 1)}
	conn.r = &connReader{conn:conn}
	return conn
}
func (T *Conn) CloseNotify() <-chan error {
	if T.closed.isFalse() {
		T.r.startBackgroundRead()
	}
	return T.closedSignal
}
func (T *Conn) closeNotify(err error) {
	select{
	case _, ok := <-T.closedSignal:
		if !ok {
			return
		}
	default:
	}
	if err == io.EOF {
		T.closedSignal <- err
	}else if oe, ok := err.(*net.OpError); ok && (oe.Op == "read" || oe.Op == "write") {
		T.closedSignal <- err
	}
}
func (T *Conn) Read(b []byte) (n int, err error) {
	n, err = T.r.Read(b)
	T.closeNotify(err)
	if T.closed.isFalse() {
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
	if T.closed.setTrue() {
		return nil
	}
	select{
	case _, ok := <-T.closedSignal:
		if ok {
			close(T.closedSignal)
		}
	default:
		close(T.closedSignal)
	}
	return T.rwc.Close()
}
func (T *Conn) LocalAddr() net.Addr {
	return T.rwc.LocalAddr()
}
func (T *Conn) RemoteAddr() net.Addr {
	return T.rwc.RemoteAddr()
}
func (T *Conn) SetDeadline(t time.Time) error  {
	T.readDeadline = t
	return T.rwc.SetDeadline(t)
}
func (T *Conn) SetReadDeadline(t time.Time) error {
	T.readDeadline = t
	return T.rwc.SetReadDeadline(t)
}
func (T *Conn) SetWriteDeadline(t time.Time) error {
	return T.rwc.SetWriteDeadline(t)
}

type connReader struct {
  	conn 	*Conn															// 上级
  	                            											
  	mu      sync.Mutex														// 锁
  	hasByte atomicBool														// 检测有数据
  	byteBuf [1]byte															// 第一个数据，检测时候得到一个数据
  	cond    *sync.Cond														// 组
  	inRead  atomicBool														// 正在读取
  	aborted atomicBool														// 结束
}

//锁，条件等待
func (T *connReader) lock() {
	T.mu.Lock()
	if T.cond == nil {
		T.cond = sync.NewCond(&T.mu)
	}
}

//解锁
func (T *connReader) unlock() {T.mu.Unlock()}

//开始后台读取
func (T *connReader) startBackgroundRead() {
  	if T.inRead.isTrue() {
  		return
  	}
  	if T.hasByte.isTrue() {
  		return
  	}
  	go T.backgroundRead()
}

//后台读取
func (T *connReader) backgroundRead() {
	T.lock()
	if T.inRead.setTrue() {
		T.unlock()
		return
	}
	T.unlock()
  	T.conn.rwc.SetReadDeadline(time.Time{})
	n, err := T.conn.rwc.Read(T.byteBuf[:])
	T.lock()
	if n == 1 {
		T.hasByte.setTrue()
	}
	if ne, ok := err.(net.Error); ok && T.aborted.isTrue() && ne.Timeout() {
		//忽略这个错误。 这是另一个调用abortPendingRead的例程的预期错误。
	}else if err != nil {
		//主动关闭连接，造成读取失败
		T.conn.closeNotify(err)
	}
	T.aborted.setFalse()
	T.inRead.setFalse()
	T.unlock()
	T.cond.Broadcast()
}

//中止等待读取
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

//读取数据
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
	
	if T.hasByte.isTrue() {
		p[0] = T.byteBuf[0]
		T.hasByte.setFalse()
		T.unlock()
		return 1, nil
	}

	T.inRead.setTrue()
	T.unlock()
	n, err = T.conn.rwc.Read(p)
	T.inRead.setFalse()
	T.cond.Broadcast()
	return n, err
}











