//go:build !plan9 && !js && !wasip1

package vconn

import (
	"errors"
	"fmt"
	"io"
	"net"
	"os"
	"syscall"
	"testing"
)

// timeoutErr 实现 net.Error 的超时语义
type timeoutErr struct{}

func (timeoutErr) Error() string   { return "i/o timeout" }
func (timeoutErr) Timeout() bool   { return true }
func (timeoutErr) Temporary() bool { return true }

// TestIsCommonNetError 只有「真断链」才允许被判定为断开
func TestIsCommonNetError(t *testing.T) {
	cases := []struct {
		name string
		err  error
		want bool
	}{
		{"nil", nil, false},
		{"io.EOF", io.EOF, true},
		{"net.ErrClosed", net.ErrClosed, true},
		{"io.ErrUnexpectedEOF", io.ErrUnexpectedEOF, true},
		{"包装的 io.EOF", fmt.Errorf("read: %w", io.EOF), true},
		{"普通错误", errors.New("invalid argument"), false},
		{"包装后的普通错误", &net.OpError{Op: "read", Err: errors.New("invalid argument")}, false},
		{"超时（控制流）", &net.OpError{Op: "read", Err: timeoutErr{}}, false},
		{"SyscallError{普通错误}", &os.SyscallError{Syscall: "read", Err: errors.New("boom")}, false},
	}

	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			if got := isCommonNetError(c.err); got != c.want {
				t.Fatalf("isCommonNetError(%v) = %v, 期望 %v", c.err, got, c.want)
			}
		})
	}
}

// TestIsNetErrno_RejectsGeneric 通用参数错误 / 未知 errno 不是断开
func TestIsNetErrno_RejectsGeneric(t *testing.T) {
	for _, e := range []syscall.Errno{syscall.EINVAL, syscall.Errno(0x7ffffff0)} {
		if isNetErrno(e) {
			t.Fatalf("%v 不应被判定为网络断开", e)
		}
		if isCommonNetError(&net.OpError{Op: "read", Err: e}) {
			t.Fatalf("包装后的 %v 不应被判定为断开", e)
		}
	}
}

// assertDisconnect 平台测试共用：真断链 errno 必须被识别并归为 PeerReset
func assertDisconnect(t *testing.T, e syscall.Errno) {
	t.Helper()
	if !isNetErrno(e) {
		t.Fatalf("%v 应被判定为网络断开", e)
	}
	wrapped := &net.OpError{Op: "read", Net: "tcp", Err: e}
	if !isCommonNetError(wrapped) {
		t.Fatalf("包装后的 %v 应被判定为断开", e)
	}
	if k := StatusOf(wrapped); k != StatusPeerReset {
		t.Fatalf("%v 应归类为 StatusPeerReset，得到 %v", e, k)
	}
}
