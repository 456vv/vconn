//go:build !windows && !plan9 && !js && !wasip1

package vconn

import "syscall"

// isNetErrno 判定 Unix/Linux 常见网络断开错误码
//
// 只收录「能确定连接已不可用」的错误码：
//   - EINVAL 等通用参数错误**不能**收录：它可能来自完全无关的调用，
//     一旦误判会让健康连接被标记为「对端已断开」并向订阅者发出假的关闭通知）。
func isNetErrno(errno syscall.Errno) bool {
	switch errno {
	case syscall.ECONNRESET, // 连接被对端重置
		syscall.ECONNABORTED, // 连接被本地中止
		syscall.EPIPE,        // 管道破裂（对端已关闭）
		syscall.ETIMEDOUT,    // 连接超时
		syscall.ENETRESET,    // 网络重置
		syscall.ENETDOWN,     // 网络不可用
		syscall.ENETUNREACH,  // 网络不可达
		syscall.EHOSTUNREACH: // 主机不可达
		return true
	}
	return false
}

// syscallErrno 把内核错误码转换为 error
func syscallErrno(v int) error {
	if v == 0 {
		return nil
	}
	return syscall.Errno(v)
}
