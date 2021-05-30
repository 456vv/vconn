# vconn [![Build Status](https://travis-ci.org/456vv/vconn.svg?branch=master)](https://travis-ci.org/456vv/vconn)
golang vconn，支持检测连接断开通知。

# **列表：**
```go
type CloseNotifier interface {                          // 关闭通知接口
    CloseNotify() <-chan error                                  // 关闭通知
}
type Conn struct {                                      // 连接
}
    func NewConn(c net.Conn) net.Conn                           // 新连接
    func (T *Conn) SetReadDiscard(y bool)                       // 读取读取丢弃
    func (T *Conn) SetReadLimit(remain int)                     // 限制读取大小
    func (T *Conn) DisableBackgroundRead(y bool)                // 禁止后台读取
    func (T *Conn) CloseNotify() <-chan error                   // 关闭通知
    func (T *Conn) Read(b []byte) (n int, err error)            // 读取
    func (T *Conn) Write(b []byte) (n int, err error)           // 写入
    func (T *Conn) Close() error                                // 关闭
    func (T *Conn) LocalAddr() net.Addr                         // 本地地址
    func (T *Conn) RemoteAddr() net.Addr                        // 远程地址
    func (T *Conn) SetDeadline(t time.Time) error               // 读写超时
    func (T *Conn) SetReadDeadline(t time.Time) error           // 读超时
    func (T *Conn) SetWriteDeadline(t time.Time) error          // 写超时
```