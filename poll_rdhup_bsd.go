//go:build darwin || ios || freebsd || netbsd || openbsd || dragonfly

package vconn

// pollRDHUP BSD/macOS 没有 POLLRDHUP。
//
// 注意：BSD/macOS 在「收到 FIN 且接收队列为空」时通常只置 POLLIN（不置 POLLHUP），
// 因此不能只靠事件位判断半关闭。probe_poll.go 会在事件就绪后统一用
// recv(MSG_PEEK) 区分「有数据」与「FIN（0 字节）」，本常量保持 0 即可。
const pollRDHUP = 0
