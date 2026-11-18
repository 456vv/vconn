//go:build plan9 || js || wasip1

package vconn

import "syscall"

// isNetErrno 该平台无网络错误码常量，统一交由上层错误串判断
func isNetErrno(syscall.Errno) bool { return false }

// syscallErrno 该平台无对应转换，返回 nil
func syscallErrno(int) error { return nil }
