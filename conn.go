package vconn

import(
	"net"
	"time"
	"sync"
	"io"
)

type CloseNotifier interface {
    CloseNotify() <-chan bool
}

type Conn struct {
	rwc		net.Conn
	closed 	chan bool
	r		*connReader
	readDeadline	time.Time
	m		sync.Mutex
}
func NewConn(c net.Conn) net.Conn {
	if conn, ok := c.(*Conn); ok {
		return conn
	}
	
	conn := &Conn{rwc:c, closed:make(chan bool, 1)}
	conn.r = &connReader{conn:conn}
	conn.r.startBackgroundRead()
	return conn
}
func (T *Conn) CloseNotify() <-chan bool {
	return T.closed
}

func (T *Conn) closeNotify(err error) {
	select{
	case <-T.closed:
		return
	default:
	}
	if(err == io.EOF){
		T.closed <- true
		return
	}
	if neterr, ok := err.(net.Error); ok && neterr.Timeout() {
		T.closed <- true
		return
	}
	if oe, ok := err.(*net.OpError); ok && (oe.Op == "read" || oe.Op == "write") {
		T.closed <- true
		return
	}

	T.r.startBackgroundRead()
}


func (T *Conn) Read(b []byte) (n int, err error) {
	T.r.abortPendingRead()
	n, err = T.r.Read(b)
	T.closeNotify(err)
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
	select{
	case _, ok := <- T.closed:
		if ok {
			close(T.closed)
		}
	default:
		close(T.closed)
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
  	hasByte bool															// 检测有数据
  	byteBuf [1]byte															// 第一个数据，检测时候得到一个数据
  	cond    *sync.Cond														// 组
  	inRead  bool															// 正在读取
  	aborted bool															// 结束
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
	T.lock()
  	defer T.unlock()
  	if T.inRead {
  		return
  	}
  	if T.hasByte {
  		return
  	}
  	T.inRead = true
  	T.conn.rwc.SetReadDeadline(time.Time{})
  	go T.backgroundRead()
}
//后台读取
func (T *connReader) backgroundRead() {
	n, err := T.conn.rwc.Read(T.byteBuf[:])
	T.lock()
	if n == 1 {
		T.hasByte = true
	}
	if ne, ok := err.(net.Error); ok && T.aborted && ne.Timeout() {
		//忽略这个错误。 这是另一个调用abortPendingRead的例程的预期错误。
	}else if err != nil {
		//主动关闭连接，造成读取失败
		T.conn.closeNotify(err)
	}
	T.aborted = false
	T.inRead = false
	T.unlock()
	T.cond.Broadcast()
}
//中止等待读取
func (T *connReader) abortPendingRead() {
	T.lock()
	defer T.unlock()
	if !T.inRead {
		return
	}
	T.aborted = true
	T.conn.rwc.SetReadDeadline(time.Unix(1, 0))
	for T.inRead {
		T.cond.Wait()
	}
	T.conn.rwc.SetReadDeadline(T.conn.readDeadline)
}


//读取数据
func (T *connReader) Read(p []byte) (n int, err error) {
	T.lock()
	if T.inRead {
		T.unlock()
		panic("vconn: current call .Read() invalid")
	}

	if len(p) == 0 {
		T.unlock()
		return 0, nil
	}

	if T.hasByte {
		p[0] = T.byteBuf[0]
		T.hasByte = false
		T.unlock()
		return 1, nil
	}

	T.inRead = true
	T.unlock()
	n, err = T.conn.rwc.Read(p)
	T.lock()
	T.inRead = false
	T.unlock()
	T.cond.Broadcast()
	return n, err
}











