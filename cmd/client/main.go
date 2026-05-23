package main

import (
	"flag"
	"fmt"
	"io"
	"log"
	"net"
	"os"
	"os/signal"
	"syscall"
	"time"

	"intranet-pen/pkg/common"
)

var (
	serverAddr = flag.String("server", "", "服务端地址 (ip:port)")
	localAddr  = flag.String("local", "", "本地服务地址 (ip:port)")
	tunnelID   = flag.String("id", "default", "隧道标识")
)

func main() {
	flag.Parse()

	if *serverAddr == "" || *localAddr == "" {
		fmt.Println("用法: client.exe -server <服务器IP:端口> -local <本地IP:端口> [-id <隧道ID>]")
		fmt.Println("示例: client.exe -server 1.2.3.4:8888 -local 127.0.0.1:25565 -id mc")
		os.Exit(1)
	}

	localHost, localPort, err := parseAddr(*localAddr)
	if err != nil {
		log.Fatalf("本地地址格式错误: %v", err)
	}

	log.Printf("内网穿透客户端已启动")
	log.Printf("  服务端: %s", *serverAddr)
	log.Printf("  本地服务: %s:%d", localHost, localPort)
	log.Printf("  隧道ID: %s", *tunnelID)

	for {
		if err := run(localHost, localPort); err != nil {
			log.Printf("[重连] 5秒后重试... %v", err)
			time.Sleep(5 * time.Second)
		}

		select {
		case <-sigCh():
			return
		default:
		}
	}
}

func sigCh() chan os.Signal {
	ch := make(chan os.Signal, 1)
	signal.Notify(ch, syscall.SIGINT, syscall.SIGTERM)
	return ch
}

func parseAddr(addr string) (string, int, error) {
	var host string
	var port int
	_, err := fmt.Sscanf(addr, "%[^:]:%d", &host, &port)
	return host, port, err
}

func run(localHost string, localPort int) error {
	ctrlConn, err := net.Dial("tcp", *serverAddr)
	if err != nil {
		return fmt.Errorf("连接服务端失败: %w", err)
	}
	defer ctrlConn.Close()

	log.Printf("[连接] 已连接到服务端 %s", *serverAddr)

	// 注册隧道
	err = common.WriteMsg(ctrlConn, &common.Message{
		Type:      common.MsgRegister,
		TunnelID:  *tunnelID,
		LocalHost: localHost,
		LocalPort: localPort,
	})
	if err != nil {
		return fmt.Errorf("注册失败: %w", err)
	}

	// 等待注册确认
	msg, err := common.ReadMsg(ctrlConn)
	if err != nil {
		return fmt.Errorf("读取注册确认失败: %w", err)
	}
	if msg.Type == common.MsgError {
		return fmt.Errorf("服务端拒绝: %s", msg.Error)
	}
	if msg.Type != common.MsgRegistered {
		return fmt.Errorf("未知响应: %s", msg.Type)
	}

	log.Printf("[就绪] 隧道 %s → 公网端口 %d", msg.TunnelID, msg.PubPort)

	// 数据端口 = 控制端口 + 1
	dataAddr := fmt.Sprintf("%s:%d", parseHost(*serverAddr), parsePort(*serverAddr)+1)

	// 心跳
	go func() {
		ticker := time.NewTicker(30 * time.Second)
		defer ticker.Stop()
		for range ticker.C {
			if err := common.WriteMsg(ctrlConn, &common.Message{Type: common.MsgPing}); err != nil {
				return
			}
		}
	}()

	// 处理新连接通知
	for {
		msg, err := common.ReadMsg(ctrlConn)
		if err != nil {
			return fmt.Errorf("与控制端断开: %w", err)
		}

		switch msg.Type {
		case common.MsgNewConn:
			go handleNewConn(dataAddr, msg, localHost, localPort)
		case common.MsgPong:
			// 心跳响应，忽略
		default:
			log.Printf("[未知] 收到未知消息: %s", msg.Type)
		}
	}
}

func handleNewConn(dataAddr string, msg *common.Message, localHost string, localPort int) {
	// 连接本地服务
	localConn, err := net.Dial("tcp", fmt.Sprintf("%s:%d", localHost, localPort))
	if err != nil {
		log.Printf("[错误] 连接本地服务失败: %v", err)
		return
	}

	// 建立到服务端的数据通道
	dataConn, err := net.Dial("tcp", dataAddr)
	if err != nil {
		localConn.Close()
		log.Printf("[错误] 连接数据通道失败: %v", err)
		return
	}

	// 发送CONN_READY
	err = common.WriteMsg(dataConn, &common.Message{
		Type:     common.MsgConnReady,
		TunnelID: msg.TunnelID,
		ConnID:   msg.ConnID,
	})
	if err != nil {
		localConn.Close()
		dataConn.Close()
		return
	}

	// 双向转发
	go func() {
		io.Copy(dataConn, localConn)
		dataConn.Close()
		localConn.Close()
	}()
	go func() {
		io.Copy(localConn, dataConn)
		localConn.Close()
		dataConn.Close()
	}()
}

func parseHost(addr string) string {
	host, _, _ := net.SplitHostPort(addr)
	return host
}

func parsePort(addr string) int {
	_, portStr, _ := net.SplitHostPort(addr)
	var port int
	fmt.Sscanf(portStr, "%d", &port)
	return port
}