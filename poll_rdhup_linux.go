//go:build linux || android

package vconn

import "golang.org/x/sys/unix"

// pollRDHUP Linux/Android 支持 POLLRDHUP，可在有未读数据时识别对端半关闭
const pollRDHUP = unix.POLLRDHUP
