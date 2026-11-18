//go:build plan9 || js || wasip1

package vconn

import "net"

// socketError 该平台不支持
func socketError(net.Conn) error { return ErrUnsupported }
