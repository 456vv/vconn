package vconn

import (
	"bytes"
	"io"
	"net"
	"testing"

	//"fmt"
	"time"
)

type Fatal func(args ...interface{})

// 测试客户端关闭，服务端收到io.EOF
func Test_conn_Read_1(t *testing.T) {
	exit := make(chan int, 1)
	// 监听端
	netListener, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	defer netListener.Close()

	fatal := t.Fatal
	go func() {
		netConn, err := netListener.Accept()
		if err != nil {
			fatal(err)
		}

		netConn = New(netConn)
		go func(nc CloseNotifier) {
			// 连接关闭事件
			err, ok := <-nc.CloseNotify()
			if err != io.EOF {
				fatal(err)
			}
			t.Log(err, ok)
			exit <- 1
		}(netConn.(CloseNotifier))

		if _, err = io.Copy(io.Discard, netConn); err != nil {
			fatal(err)
		}
		netConn.Close()
		netConn.Close()
		netConn.Close()
	}()

	// 发送端
	netAddr := netListener.Addr()
	netConn, err := net.Dial(netAddr.Network(), netAddr.String())
	if err != nil {
		t.Fatal(err)
	}

	time.Sleep(time.Second)
	netConn.Write([]byte{1})

	time.Sleep(time.Second)
	netConn.Write([]byte{2})

	time.Sleep(time.Second)
	netConn.Write([]byte("123456"))

	netConn.Close()

	<-exit
}

// 测试客户端关闭，服务端收到io.EOF
func Test_conn_Read_2(t *testing.T) {
	exit := make(chan int, 1)
	// 监听端
	netListener, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	defer netListener.Close()

	fatal := t.Fatal
	go func() {
		netConn, err := netListener.Accept()
		if err != nil {
			fatal(err)
		}

		netConn = New(netConn)
		go func(nc CloseNotifier) {
			// 连接关闭事件
			err, ok := <-nc.CloseNotify()
			if err != io.EOF {
				fatal(err)
			}
			t.Log(err, ok)
			exit <- 1
		}(netConn.(CloseNotifier))
		time.Sleep(time.Second)

		p := make([]byte, 1)
		for {
			n, err := netConn.Read(p)
			if err != nil {
				if err == io.EOF {
					break
				}
				fatal(err)
			}
			if n == 0 {
				fatal("error")
			}
		}
		netConn.Close()
	}()

	// 发送端
	netAddr := netListener.Addr()
	netConn, err := net.Dial(netAddr.Network(), netAddr.String())
	if err != nil {
		t.Fatal(err)
	}
	netConn.Write([]byte("12345678901234567890123456789012345678901234567890123456789012345678901234567890123456789012345678901234567890123456789012345678901234567890"))
	netConn.Close()
	<-exit
}

// 测试客户端关闭，服务端收到io.EOF
// 后台读取丢弃
func Test_conn_SetBackgroundReadDiscard(t *testing.T) {
	exit := make(chan int, 1)
	// 监听端
	netListener, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	defer netListener.Close()

	fatal := t.Fatal
	go func() {
		netConn, err := netListener.Accept()
		if err != nil {
			fatal(err)
		}

		defer netConn.Close()
		netConn = New(netConn)
		netConn.(*Conn).SetBackgroundReadDiscard(true)

		// 连接关闭事件
		err, ok := <-netConn.(CloseNotifier).CloseNotify()
		if err != io.EOF {
			fatal(err)
		}
		t.Log(err, ok)
		exit <- 1
	}()

	// 发送端
	netAddr := netListener.Addr()
	netConn, err := net.Dial(netAddr.Network(), netAddr.String())
	if err != nil {
		t.Fatal(err)
	}
	netConn.Write([]byte("12345678901234567890123456789012345678901234567890123456789012345678901234567890123456789012345678901234567890123456789012345678901234567890"))

	netConn.Close()
	<-exit
}

// 测试客户端关闭，服务端收到io.EOF
// 不允许后台读取
func Test_conn_DisableBackgroundRead(t *testing.T) {
	exit := make(chan int, 1)
	// 监听端
	netListener, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	defer netListener.Close()

	fatal := t.Fatal
	p := []byte("12345678901234567890123456789012345678901234567890123456789012345678901234567890123456789012345678901234567890123456789012345678901234567890")
	go func() {
		netConn, err := netListener.Accept()
		if err != nil {
			fatal(err)
		}

		netConn = New(netConn)
		netConn.(*Conn).DisableBackgroundRead(true)
		defer netConn.Close()

		go func() {
			// 连接关闭事件
			err, ok := <-netConn.(CloseNotifier).CloseNotify()
			if err != io.EOF {
				fatal(err)
			}
			t.Log(err, ok)
			exit <- 1
		}()

		time.Sleep(time.Second)

		buf := bytes.NewBuffer(nil)
		if _, err := buf.ReadFrom(netConn); err != nil {
			fatal(err)
		}
		if !bytes.Equal(buf.Bytes(), p) {
			fatal("error")
		}
	}()

	// 发送端
	netAddr := netListener.Addr()
	netConn, err := net.Dial(netAddr.Network(), netAddr.String())
	if err != nil {
		t.Fatal(err)
	}
	netConn.Write(p)
	netConn.Close()
	<-exit
}
