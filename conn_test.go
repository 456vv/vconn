package vconn

import (
	"testing"
	"net"
	"io"
	"io/ioutil"
	//"fmt"
	"time"
)


func Test_conn_Read(t *testing.T){
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
		go func (nc CloseNotifier){
			//连接关闭事件
			select{
			case c, ok := <-nc.CloseNotify():
				if c != ok {
					t.Fatal("发生错误")
				}
				exit<-1
			}
		}(netConn.(CloseNotifier))
		
		_, err = io.Copy(ioutil.Discard, netConn)
		if err != nil {
			t.Fatal(err.Error())
		}
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