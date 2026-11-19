//go:build plan9 || js || wasip1

package vconn

import "syscall"

// isNetErrno 该平台没有可移植的网络 errno 白名单。
// 上层仍会识别 io.EOF、net.ErrClosed、io.ErrUnexpectedEOF；
// 平台特有的断开错误如果没有被这些哨兵包住，不会在这里被当成网络断开。
func isNetErrno(syscall.Errno) bool { return false }

// syscallErrno 该平台无对应转换，返回 nil
func syscallErrno(int) error { return nil }
