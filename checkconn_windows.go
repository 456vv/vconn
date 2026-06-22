//go:build windows

package vconn

import (
	"fmt"
	"net"

	"golang.org/x/sys/windows"
)

func checkConnection(conn *net.TCPConn) error {
	raw, err := conn.SyscallConn()
	if err != nil {
		return fmt.Errorf("syscallConn: %v", err)
	}

	var sockErr error
	err = raw.Control(func(fd uintptr) {
		_, sockErr = windows.GetsockoptInt(windows.Handle(fd), windows.SOL_SOCKET, windows.SO_KEEPALIVE)
	})
	if err != nil {
		return fmt.Errorf("control: %v", err)
	}

	if sockErr != nil {
		return fmt.Errorf("socket error: %v", sockErr)
	}
	return nil
}
