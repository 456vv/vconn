//go:build !windows

package vconn

import (
	"fmt"
	"net"
	"syscall"
)

func checkConnection(conn *net.TCPConn) error {
	raw, err := conn.SyscallConn()
	if err != nil {
		return fmt.Errorf("syscallConn: %v", err)
	}

	var sockErr error
	err = raw.Control(func(fd uintptr) {
		_, sockErr = syscall.GetsockoptInt(int(fd), syscall.SOL_SOCKET, syscall.SO_ERROR)
	})
	if err != nil {
		return fmt.Errorf("control: %v", err)
	}
	if sockErr != nil {
		return fmt.Errorf("socket error: %v", sockErr)
	}
	return nil
}
