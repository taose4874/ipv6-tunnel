package main

import (
	"fmt"
	"image/color"
	"io"
	"log"
	"net"
	"sync"
	"time"

	"intranet-pen/pkg/common"

	"fyne.io/fyne/v2"
	"fyne.io/fyne/v2/app"
	"fyne.io/fyne/v2/canvas"
	"fyne.io/fyne/v2/container"
	"fyne.io/fyne/v2/dialog"
	"fyne.io/fyne/v2/widget"
)

var (
	colorGreen = color.NRGBA{R: 0x4C, G: 0xAF, B: 0x50, A: 0xFF}
	colorGray  = color.NRGBA{R: 0x9E, G: 0x9E, B: 0x9E, A: 0xFF}
)

type tunnelInfo struct {
	ID        string
	LocalHost string
	LocalPort int
	PubPort   int
	CtrlConn  net.Conn
	PubLn     net.Listener
	mu        sync.Mutex
	Conns     map[string]net.Conn
}

type userEntry struct {
	Addr   string
	Port   int
	ConnID string
}

type ServerState struct {
	mu        sync.Mutex
	running   bool
	controlLn net.Listener
	dataLn    net.Listener
	tunnels   map[string]*tunnelInfo

	win        fyne.Window
	statusDot  *canvas.Circle
	statusLbl  *widget.Label
	addrLbl    *widget.Label
	actionBtn  *widget.Button
	portEntry  *widget.Entry
	userList   *widget.List
	userData   []userEntry
	logEntry   *widget.Entry
	userCnt    *widget.Label
}

var state = &ServerState{
	tunnels:  make(map[string]*tunnelInfo),
	userData: make([]userEntry, 0),
}

func (s *ServerState) addLog(msg string) {
	t := time.Now().Format("15:04:05")
	prev := s.logEntry.Text
	if len(prev) > 20000 {
		prev = prev[len(prev)-10000:]
	}
	s.logEntry.SetText(prev + fmt.Sprintf("[%s] %s\n", t, msg))
}

func (s *ServerState) start() error {
	s.mu.Lock()
	if s.running {
		s.mu.Unlock()
		return fmt.Errorf("服务已在运行中")
	}
	s.mu.Unlock()

	portStr := s.portEntry.Text
	var ctrlPort int
	fmt.Sscanf(portStr, "%d", &ctrlPort)
	if ctrlPort == 0 {
		ctrlPort = 8888
	}

	var err error
	s.controlLn, err = net.Listen("tcp", fmt.Sprintf("[::]:%d", ctrlPort))
	if err != nil {
		return fmt.Errorf("监听控制端口失败: %v", err)
	}

	dataPort := ctrlPort + 1
	s.dataLn, err = net.Listen("tcp", fmt.Sprintf("[::]:%d", dataPort))
	if err != nil {
		s.controlLn.Close()
		return fmt.Errorf("监听数据端口 %d 失败: %v", dataPort, err)
	}

	s.running = true

	go func() {
		for {
			conn, err := s.dataLn.Accept()
			if err != nil {
				return
			}
			go s.handleDataConn(conn)
		}
	}()

	go func() {
		for {
			conn, err := s.controlLn.Accept()
			if err != nil {
				return
			}
			go s.handleControl(conn)
		}
	}()

	return nil
}

func (s *ServerState) stop() {
	s.mu.Lock()
	defer s.mu.Unlock()
	if !s.running {
		return
	}
	s.running = false
	if s.controlLn != nil {
		s.controlLn.Close()
	}
	if s.dataLn != nil {
		s.dataLn.Close()
	}
	for _, t := range s.tunnels {
		t.PubLn.Close()
		t.mu.Lock()
		for _, c := range t.Conns {
			c.Close()
		}
		t.mu.Unlock()
	}
	s.tunnels = make(map[string]*tunnelInfo)
}

func (s *ServerState) handleControl(conn net.Conn) {
	defer conn.Close()
	s.addLog(fmt.Sprintf("客户端连接: %s", conn.RemoteAddr()))

	for {
		msg, err := common.ReadMsg(conn)
		if err != nil {
			s.addLog(fmt.Sprintf("客户端断开: %s", conn.RemoteAddr()))
			s.removeClientTunnels(conn)
			s.refreshUserList()
			return
		}
		switch msg.Type {
		case common.MsgRegister:
			s.handleRegister(conn, msg)
		case common.MsgPing:
			common.WriteMsg(conn, &common.Message{Type: common.MsgPong})
		}
	}
}

func (s *ServerState) handleRegister(ctrlConn net.Conn, msg *common.Message) {
	pubLn, err := net.Listen("tcp", "[::]:0")
	if err != nil {
		common.WriteMsg(ctrlConn, &common.Message{Type: common.MsgError, Error: "分配公网端口失败"})
		return
	}
	pubPort := pubLn.Addr().(*net.TCPAddr).Port

	t := &tunnelInfo{
		ID:        msg.TunnelID,
		LocalHost: msg.LocalHost,
		LocalPort: msg.LocalPort,
		PubPort:   pubPort,
		PubLn:     pubLn,
		CtrlConn:  ctrlConn,
		Conns:     make(map[string]net.Conn),
	}

	s.mu.Lock()
	s.tunnels[msg.TunnelID] = t
	s.mu.Unlock()

	common.WriteMsg(ctrlConn, &common.Message{
		Type:     common.MsgRegistered,
		TunnelID: msg.TunnelID,
		PubPort:  pubPort,
	})
	s.addLog(fmt.Sprintf("隧道注册: %s → 公网:%d (内网 %s:%d)", msg.TunnelID, pubPort, msg.LocalHost, msg.LocalPort))

	go s.acceptPublic(t)
}

func (s *ServerState) acceptPublic(t *tunnelInfo) {
	defer t.PubLn.Close()
	for {
		extConn, err := t.PubLn.Accept()
		if err != nil {
			return
		}
		connID := fmt.Sprintf("%d", time.Now().UnixNano())
		t.mu.Lock()
		t.Conns[connID] = extConn
		t.mu.Unlock()

		common.WriteMsg(t.CtrlConn, &common.Message{
			Type:     common.MsgNewConn,
			TunnelID: t.ID,
			ConnID:   connID,
		})
		s.addLog(fmt.Sprintf("新连接: %s → 隧道 %s", extConn.RemoteAddr(), t.ID))
		s.refreshUserList()
	}
}

func (s *ServerState) handleDataConn(conn net.Conn) {
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
	extConn, ok := t.Conns[msg.ConnID]
	t.mu.Unlock()
	if !ok {
		conn.Close()
		return
	}

	cleanup := func() {
		t.mu.Lock()
		delete(t.Conns, msg.ConnID)
		t.mu.Unlock()
		s.refreshUserList()
	}
	done := make(chan struct{}, 2)
	go func() { io.Copy(conn, extConn); done <- struct{}{} }()
	go func() { io.Copy(extConn, conn); done <- struct{}{} }()
	go func() {
		<-done
		conn.Close()
		extConn.Close()
		<-done
		cleanup()
	}()
}

func (s *ServerState) removeClientTunnels(ctrlConn net.Conn) {
	s.mu.Lock()
	defer s.mu.Unlock()
	for id, t := range s.tunnels {
		if t.CtrlConn == ctrlConn {
			t.PubLn.Close()
			t.mu.Lock()
			for _, c := range t.Conns {
				c.Close()
			}
			t.mu.Unlock()
			delete(s.tunnels, id)
			s.addLog(fmt.Sprintf("隧道已移除: %s", id))
		}
	}
}

func (s *ServerState) refreshUserList() {
	s.mu.Lock()
	var entries []userEntry
	for _, t := range s.tunnels {
		// 显示控制连接（客户端地址）
		ctrlAddr := t.CtrlConn.RemoteAddr().String()
		host, _, err := net.SplitHostPort(ctrlAddr)
		if err == nil {
			ctrlAddr = host
		}
		if ip := net.ParseIP(ctrlAddr); ip != nil && ip.To4() == nil {
			if ip.IsLoopback() {
				ctrlAddr = "本地IPv6"
			} else {
				ctrlAddr = ip.String()
			}
		}
		entries = append(entries, userEntry{
			Addr:   ctrlAddr,
			Port:   t.PubPort,
			ConnID: t.ID,
		})

		// 显示每个转发连接的远端地址
		t.mu.Lock()
		for id, conn := range t.Conns {
			addr := conn.RemoteAddr().String()
			host, _, err := net.SplitHostPort(addr)
			if err == nil {
				addr = host
			}
			if ip := net.ParseIP(addr); ip != nil && ip.To4() == nil {
				if ip.IsLoopback() {
					addr = "本地IPv6"
				} else {
					addr = ip.String()
				}
			}
			entries = append(entries, userEntry{
				Addr:   addr,
				Port:   t.PubPort,
				ConnID: id,
			})
		}
		t.mu.Unlock()
	}
	s.userData = entries
	s.mu.Unlock()
	if s.userList != nil {
		s.userList.Refresh()
	}
	if s.userCnt != nil {
		s.userCnt.SetText(fmt.Sprintf("共 %d 个连接", len(entries)))
	}
}

func (s *ServerState) toggleAction() {
	if s.running {
		s.stop()
		s.refreshUserList()
		s.statusDot.FillColor = colorGray
		s.statusDot.Refresh()
		s.statusLbl.SetText("已停止")
		s.addrLbl.SetText("")
		s.actionBtn.SetText("启动服务")
		s.actionBtn.Importance = widget.HighImportance
		s.portEntry.Enable()
		s.addLog("服务已停止")
	} else {
		if err := s.start(); err != nil {
			dialog.ShowError(err, s.win)
			return
		}
		portStr := s.portEntry.Text
		var ctrlPort int
		fmt.Sscanf(portStr, "%d", &ctrlPort)
		if ctrlPort == 0 {
			ctrlPort = 8888
		}
		s.statusDot.FillColor = colorGreen
		s.statusDot.Refresh()
		s.statusLbl.SetText("运行中")
		s.addrLbl.SetText(fmt.Sprintf("[::]:%d (数据端口 %d)", ctrlPort, ctrlPort+1))
		s.actionBtn.SetText("停止服务")
		s.actionBtn.Importance = widget.DangerImportance
		s.portEntry.Disable()
		s.addLog(fmt.Sprintf("服务已启动 (控制端口 %d, 数据端口 %d)", ctrlPort, ctrlPort+1))
	}
}

func main() {
	log.SetFlags(0)

	a := app.New()
	a.Settings().SetTheme(&bigTheme{})
	w := a.NewWindow("内网穿透 · 服务端")
	w.Resize(fyne.NewSize(560, 600))
	state.win = w

	// Status indicator
	state.statusDot = canvas.NewCircle(colorGray)
	state.statusDot.Resize(fyne.NewSize(12, 12))
	state.statusLbl = widget.NewLabel("已停止")
	state.addrLbl = widget.NewLabel("")

	state.portEntry = widget.NewEntry()
	state.portEntry.SetText("8888")

	state.actionBtn = widget.NewButton("启动服务", func() { state.toggleAction() })
	state.actionBtn.Importance = widget.HighImportance

	state.userCnt = widget.NewLabel("共 0 个连接")

	// User list
	state.userList = widget.NewList(
		func() int { return len(state.userData) },
		func() fyne.CanvasObject {
			addr := widget.NewLabel("address")
			info := widget.NewLabel("info")
			return container.NewVBox(addr, info)
		},
		func(id widget.ListItemID, obj fyne.CanvasObject) {
			if id < len(state.userData) {
				u := state.userData[id]
				box := obj.(*fyne.Container)
				addrLbl := box.Objects[0].(*widget.Label)
				infoLbl := box.Objects[1].(*widget.Label)
				addrLbl.SetText(u.Addr)
				addrLbl.TextStyle = fyne.TextStyle{Bold: true}
				infoLbl.SetText(fmt.Sprintf("公网端口 %d  |  %s", u.Port, time.Now().Format("15:04:05")))
			}
		},
	)

	state.logEntry = widget.NewEntry()
	state.logEntry.MultiLine = true
	state.logEntry.Wrapping = fyne.TextWrapOff
	state.logEntry.SetPlaceHolder("日志输出...")

	// Top bar
	topBar := container.NewBorder(
		nil, nil,
		container.NewHBox(
			container.NewPadded(state.statusDot),
			state.statusLbl,
		),
		state.actionBtn,
		widget.NewForm(widget.NewFormItem("端口", state.portEntry)),
	)

	addrBar := container.NewHBox(state.addrLbl)

	topBox := container.NewVBox(
		container.NewPadded(topBar),
		container.NewPadded(addrBar),
		widget.NewSeparator(),
	)

	// User section
	userHeader := container.NewBorder(
		nil, nil,
		widget.NewLabelWithStyle("已连接用户", fyne.TextAlignLeading, fyne.TextStyle{Bold: true}),
		state.userCnt,
	)

	userSection := container.NewBorder(
		container.NewVBox(container.NewPadded(userHeader), widget.NewSeparator()),
		nil, nil, nil,
		state.userList,
	)

	// Log section
	logSection := container.NewBorder(
		widget.NewLabelWithStyle("日志", fyne.TextAlignLeading, fyne.TextStyle{Bold: true}),
		nil, nil, nil,
		state.logEntry,
	)

	logScroll := container.NewScroll(logSection)
	logScroll.SetMinSize(fyne.NewSize(0, 100))

	content := container.NewBorder(
		topBox,
		nil, nil, nil,
		container.NewBorder(
			container.NewVBox(
				container.NewPadded(userSection),
				widget.NewSeparator(),
			),
			nil, nil, nil,
			logScroll,
		),
	)
	w.SetContent(content)

	w.SetOnClosed(func() { state.stop() })

	go func() {
		ticker := time.NewTicker(3 * time.Second)
		defer ticker.Stop()
		for range ticker.C {
			if state.running {
				state.refreshUserList()
			}
		}
	}()

	w.ShowAndRun()
}