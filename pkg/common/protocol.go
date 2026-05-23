package common

import (
	"encoding/json"
	"fmt"
	"io"
)

// Message 控制消息
type Message struct {
	Type      string `json:"type"`
	TunnelID  string `json:"tunnel_id,omitempty"`
	ConnID    string `json:"conn_id,omitempty"`
	LocalHost string `json:"local_host,omitempty"`
	LocalPort int    `json:"local_port,omitempty"`
	PubPort   int    `json:"pub_port,omitempty"`
	Error     string `json:"error,omitempty"`
}

// 消息类型常量
const (
	MsgRegister   = "register"
	MsgRegistered = "registered"
	MsgNewConn    = "new_conn"
	MsgConnReady  = "conn_ready"
	MsgError      = "error"
	MsgPing       = "ping"
	MsgPong       = "pong"
)

// ReadMsg 从连接读取一条消息
func ReadMsg(r io.Reader) (*Message, error) {
	dec := json.NewDecoder(r)
	var msg Message
	if err := dec.Decode(&msg); err != nil {
		return nil, fmt.Errorf("decode message: %w", err)
	}
	return &msg, nil
}

// WriteMsg 向连接写入一条消息
func WriteMsg(w io.Writer, msg *Message) error {
	data, err := json.Marshal(msg)
	if err != nil {
		return fmt.Errorf("marshal message: %w", err)
	}
	data = append(data, '\n')
	_, err = w.Write(data)
	return err
}