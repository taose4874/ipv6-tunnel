package main

import (
	"flag"
	"fmt"
	"io"
	"log"
	"net"
	"os"
	"os/signal"
	"sync"
	"syscall"
	"time"

	"intranet-pen/pkg/common"
)

var (
	controlPort = flag.Int("port", 8888, "控制端口")
	portStart   = flag.Int("port-start", 20000, "公网端口范围起始")
	portEnd     = flag.Int("port-end", 21000, "公网端口范围结束")
)

// tunnel 代表一个注册的隧道
type tunnel struct {
	id        string
	localHost string
	localPort int
	pubPort   int
	pubLn     net.Listener
	ctrlConn  net.Conn
	mu        sync.Mutex
	conns     map[string]net.Conn
}

// Server 服务端
type Server struct {
	mu       sync.Mutex
	tunnels  map[string]*tunnel
	nextPort int
}

func NewServer() *Server {
	return &Server{
		tunnels:  make(map[string]*tunnel),
		nextPort: *portStart,
	}
}

func (s *Server) allocPort() (int, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	for port := s.nextPort; port <= *portEnd; port++ {
		ln, err := net.Listen("tcp", fmt.Sprintf(":%d", port))
		if err == nil {
			ln.Close()
			s.nextPort = port + 1
			return port, nil
		}
	}
	return 0, fmt.Errorf("端口范围 %d-%d 已用完", *portStart, *portEnd)
}

func (s *Server) handleControl(conn net.Conn) {
	defer conn.Close()
	log.Printf("[连接] 客户端 %s 已连接", conn.RemoteAddr())

	for {
		msg, err := common.ReadMsg(conn)
		if err != nil {
			log.Printf("[断开] 客户端 %s 已断开", conn.RemoteAddr())
			s.removeClientTunnels(conn)
			return
		}

		switch msg.Type {
		case common.MsgRegister:
			s.handleRegister(conn, msg)
		case common.MsgPing:
			common.WriteMsg(conn, &common.Message{Type: common.MsgPong})
		default:
			log.Printf("[未知] 收到未知消息类型: %s", msg.Type)
		}
	}
}

func (s *Server) handleRegister(ctrlConn net.Conn, msg *common.Message) {
	pubPort, err := s.allocPort()
	if err != nil {
		common.WriteMsg(ctrlConn, &common.Message{
			Type:  common.MsgError,
			Error: err.Error(),
		})
		return
	}

	pubLn, err := net.Listen("tcp", fmt.Sprintf(":%d", pubPort))
	if err != nil {
		common.WriteMsg(ctrlConn, &common.Message{
			Type:  common.MsgError,
			Error: fmt.Sprintf("无法监听公网端口 %d: %v", pubPort, err),
		})
		return
	}

	t := &tunnel{
		id:        msg.TunnelID,
		localHost: msg.LocalHost,
		localPort: msg.LocalPort,
		pubPort:   pubPort,
		pubLn:     pubLn,
		ctrlConn:  ctrlConn,
		conns:     make(map[string]net.Conn),
	}

	s.mu.Lock()
	s.tunnels[msg.TunnelID] = t
	s.mu.Unlock()

	common.WriteMsg(ctrlConn, &common.Message{
		Type:    common.MsgRegistered,
		TunnelID: msg.TunnelID,
		PubPort: pubPort,
	})

	log.Printf("[隧道] %s → 公网端口 %d (内网 %s:%d)",
		msg.TunnelID, pubPort, msg.LocalHost, msg.LocalPort)

	go s.acceptPublic(t)
}

func (s *Server) acceptPublic(t *tunnel) {
	defer t.pubLn.Close()

	for {
		extConn, err := t.pubLn.Accept()
		if err != nil {
			return
		}

		connID := fmt.Sprintf("%d", time.Now().UnixNano())
		t.mu.Lock()
		t.conns[connID] = extConn
		t.mu.Unlock()

		log.Printf("[转发] 隧道 %s 收到外部连接 %s (ID:%s)",
			t.id, extConn.RemoteAddr(), connID)

		common.WriteMsg(t.ctrlConn, &common.Message{
			Type:     common.MsgNewConn,
			TunnelID: t.id,
			ConnID:   connID,
		})

		go s.waitDataConn(t, connID, extConn)
	}
}

func (s *Server) waitDataConn(t *tunnel, connID string, extConn net.Conn) {
	// 等待客户端建立数据通道连接到同一控制端口
	// 客户端在收到 NEW_CONN 后会新建连接并发送 CONN_READY
	// 这里需要一个机制来接收客户端的数据通道连接
	// 简化设计：客户端新建TCP连接，发送CONN_READY后开始转发
}

func (s *Server) removeClientTunnels(ctrlConn net.Conn) {
	s.mu.Lock()
	defer s.mu.Unlock()
	for id, t := range s.tunnels {
		if t.ctrlConn == ctrlConn {
			t.pubLn.Close()
			t.mu.Lock()
			for _, c := range t.conns {
				c.Close()
			}
			t.mu.Unlock()
			delete(s.tunnels, id)
			log.Printf("[清理] 隧道 %s 已移除", id)
		}
	}
}

func main() {
	flag.Parse()

	server := NewServer()

	// 监听控制端口
	ln, err := net.Listen("tcp", fmt.Sprintf(":%d", *controlPort))
	if err != nil {
		log.Fatalf("无法监听控制端口 %d: %v", *controlPort, err)
	}
	defer ln.Close()

	log.Printf("内网穿透服务端已启动")
	log.Printf("  控制端口: %d", *controlPort)
	log.Printf("  公网端口范围: %d-%d", *portStart, *portEnd)
	log.Printf("  等待客户端连接...")

	// 同时监听数据通道端口（控制端口+1）
	dataLn, err := net.Listen("tcp", fmt.Sprintf(":%d", *controlPort+1))
	if err != nil {
		log.Fatalf("无法监听数据端口 %d: %v", *controlPort+1, err)
	}
	defer dataLn.Close()

	// 处理数据通道连接
	go func() {
		for {
			conn, err := dataLn.Accept()
			if err != nil {
				return
			}
			go server.handleDataConn(conn)
		}
	}()

	// 处理控制连接
	go func() {
		for {
			conn, err := ln.Accept()
			if err != nil {
				return
			}
			go server.handleControl(conn)
		}
	}()

	// 优雅退出
	sigCh := make(chan os.Signal, 1)
	signal.Notify(sigCh, syscall.SIGINT, syscall.SIGTERM)
	<-sigCh
	log.Println("服务端正在关闭...")
}

func (s *Server) handleDataConn(conn net.Conn) {
	// 读取CONN_READY消息获取tunnelID和connID
	msg, err := common.ReadMsg(conn)
	if err != nil || msg.Type != common.MsgConnReady {
		conn.Close()
		return
	}

	s.mu.Lock()
	t, ok := s.tunnels[msg.TunnelID]
	s.mu.Unlock()
	if !ok {
		conn.Close()
		return
	}

	t.mu.Lock()
	extConn, ok := t.conns[msg.ConnID]
	if ok {
		delete(t.conns, msg.ConnID)
	}
	t.mu.Unlock()

	if !ok {
		conn.Close()
		return
	}

	// 双向转发
	go func() {
		io.Copy(conn, extConn)
		conn.Close()
		extConn.Close()
	}()
	go func() {
		io.Copy(extConn, conn)
		extConn.Close()
		conn.Close()
	}()
}