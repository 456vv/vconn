// Copyright 2018 The Go Authors. All rights reserved.
// Use of this source code is governed by a BSD-style
// license that can be found in the LICENSE file.

package vconn

import "syscall"

func isConnError(err error) bool {
	if se, ok := err.(syscall.Errno); ok {
		switch se {
		case syscall.WSAECONNRESET, syscall.WSAECONNABORTED:
			fallthrough
		case syscall.EINVAL, syscall.ETIMEDOUT, syscall.EPIPE:
			return true
		default:
		}
	}
	return false
}
