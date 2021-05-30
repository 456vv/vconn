package vconn

import (
	"testing"
	"net"
	"io"
	"io/ioutil"
	//"fmt"
	"time"
)


func Test_conn_Read_1(t *testing.T){
	var exit = make(chan int, 1)
	//监听端
	netListener, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	defer netListener.Close()
	go func (t *testing.T){
		netConn, err := netListener.Accept()
		if err != nil {
			t.Fatal(err.Error())
		}
		
		netConn = NewConn(netConn)
		go func (t *testing.T, nc CloseNotifier){
			//连接关闭事件
			select{
			case err, ok := <-nc.CloseNotify():
				if err != io.EOF {
					t.Fatal(err)
				}
				t.Log(err, ok)
				exit<-1
			}
		}(t, netConn.(CloseNotifier))
		
		_, err = io.Copy(ioutil.Discard, netConn)
		if err != nil {
			t.Fatal(err.Error())
		}
		netConn.Close()
		netConn.Close()
		netConn.Close()
	}(t)
	
	//发送端
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

func Test_conn_Read_2(t *testing.T){
	var exit = make(chan int, 1)
	//监听端
	netListener, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	defer netListener.Close()

	go func (t *testing.T){
		netConn, err := netListener.Accept()
		if err != nil {
			t.Fatal(err.Error())
		}
		
		netConn = NewConn(netConn)
		go func (t *testing.T, nc CloseNotifier){
			//连接关闭事件
			select{
			case err, ok := <-nc.CloseNotify():
				if err != io.EOF {
					t.Fatal(err)
				}
				t.Log(err, ok)
				exit<-1
			}
		}(t, netConn.(CloseNotifier))
		
		time.Sleep(time.Second)
		
		p := make([]byte, 1)
		for {
			n, err := netConn.Read(p)
			if err != nil {
				if err == io.EOF {
					break
				}
				t.Fatal(err)
			}
			if n == 0 {
				t.Fatal("error")
			}
		}
		netConn.Close()
	}(t)
	
	//发送端
	netAddr := netListener.Addr()
	netConn, err := net.Dial(netAddr.Network(), netAddr.String())
	if err != nil {
		t.Fatal(err)
	}
	netConn.Write([]byte("12345678901234567890123456789012345678901234567890123456789012345678901234567890123456789012345678901234567890123456789012345678901234567890"))
	netConn.Close()
	<-exit
}

func Test_conn_SetBackgroundReadDiscard(t *testing.T){
	var exit = make(chan int, 1)
	//监听端
	netListener, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	defer netListener.Close()

	go func (t *testing.T){
		netConn, err := netListener.Accept()
		if err != nil {
			t.Fatal(err.Error())
		}
		
		netConn = NewConn(netConn)
		//netConn.(*Conn).DisableBackgroundRead(true)
		netConn.(*Conn).SetBackgroundReadDiscard(true)
		defer netConn.Close()
		
		//连接关闭事件
		select{
		case err, ok := <-netConn.(CloseNotifier).CloseNotify():
			if err != io.EOF {
				t.Fatal(err)
			}
			t.Log(err, ok)
			exit<-1
		}
	}(t)
	
	//发送端
	netAddr := netListener.Addr()
	netConn, err := net.Dial(netAddr.Network(), netAddr.String())
	if err != nil {
		t.Fatal(err)
	}
	netConn.Write([]byte("12345678901234567890123456789012345678901234567890123456789012345678901234567890123456789012345678901234567890123456789012345678901234567890"))
	netConn.Close()
	<-exit
}

func Test_conn_DisableBackgroundRead(t *testing.T){
	var exit = make(chan int, 1)
	//监听端
	netListener, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	defer netListener.Close()

	go func (t *testing.T){
		netConn, err := netListener.Accept()
		if err != nil {
			t.Fatal(err.Error())
		}
		
		netConn = NewConn(netConn)
		netConn.(*Conn).DisableBackgroundRead(true)
		defer netConn.Close()
		
		go func (t *testing.T, nc CloseNotifier){
			//连接关闭事件
			select{
			case err, ok := <-nc.CloseNotify():
				if err != io.EOF {
					t.Fatal(err)
				}
				t.Log(err, ok)
				exit<-1
			}
		}(t, netConn.(CloseNotifier))
		
		time.Sleep(time.Second)
		
		p := make([]byte, 1)
		for {
			n, err := netConn.Read(p)
			if err != nil {
				if err == io.EOF {
					break
				}
				t.Fatal(err)
			}
			if n == 0 {
				t.Fatal("error")
			}
		}
	}(t)
	
	//发送端
	netAddr := netListener.Addr()
	netConn, err := net.Dial(netAddr.Network(), netAddr.String())
	if err != nil {
		t.Fatal(err)
	}
	netConn.Write([]byte("12345678901234567890123456789012345678901234567890123456789012345678901234567890123456789012345678901234567890123456789012345678901234567890"))
	netConn.Close()
	<-exit
}


