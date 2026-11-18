//go:build !plan9

package vconn

import (
	"net"
	"syscall"
	"testing"
)

// TestIsNetErrno_RejectsGenericErrno 通用参数错误不能被当成「对端已断开」。
// 旧实现把 EINVAL 列入断开错误表，会让健康连接被标记为 peerDown
// 并向订阅者发出假的关闭通知。
func TestIsNetErrno_RejectsGenericErrno(t *testing.T) {
	if isNetErrno(syscall.EINVAL) {
		t.Fatal("EINVAL 不应被判定为网络断开")
	}
	if isNetErrno(syscall.Errno(0x7ffffff0)) {
		t.Fatal("未知 errno 不应被判定为网络断开")
	}
	if isCommonNetError(&net.OpError{Op: "read", Err: syscall.EINVAL}) {
		t.Fatal("包装后的 EINVAL 也不应被判定为断开")
	}
}
