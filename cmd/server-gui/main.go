package main

import (
	"fmt"
	"image/color"
	"io"
	"log"
	"math/rand"
	"net"
	"strings"
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

// tunnelInfo represents a registered tunnel with its public listener and connections.
type tunnelInfo struct {
	ID        string
	LocalHost string
	LocalPort int
	PubPort   int
	CtrlConn  net.Conn
	PubLn     net.Listener
	mu        sync.Mutex
	Conns     map[string]net.Conn // connID -> external connection
}

type userEntry struct {
	Addr   string
	Port   int
	ConnID string
}

type ServerState struct {
	mu        sync.RWMutex // protects running, tunnels, and userData
	running   bool
	controlLn net.Listener
	dataLn    net.Listener
	tunnels   map[string]*tunnelInfo

	win        fyne.Window
	statusDot  *canvas.Circle
	statusLbl  *widget.Label
	addrLbl    *widget.Label
	actionBtn   *widget.Button
	portEntry   *widget.Entry
	portMinEntry *widget.Entry
	portMaxEntry *widget.Entry
	userList    *widget.List
	userData   []userEntry
	logEntry   *widget.Entry
	userCnt    *widget.Label
}

var state = &ServerState{
	tunnels:  make(map[string]*tunnelInfo),
	userData: make([]userEntry, 0),
}

// uiAddLog safely appends log text from any goroutine (uses fyne.Do for thread safety).
func (s *ServerState) uiAddLog(msg string) {
	t := time.Now().Format("15:04:05")
	prev := s.logEntry.Text
	if len(prev) > 20000 {
		prev = prev[len(prev)-10000:]
	}
	s.logEntry.SetText(prev + fmt.Sprintf("[%s] %s\n", t, msg))
	s.logEntry.CursorRow = 999999 // auto-scroll to bottom
	s.logEntry.Refresh()
}

// updateStatusRunning sets UI to running state. Must be called from main thread or fyne.Do.
func (s *ServerState) updateStatusRunning(ctrlPort int) {
	s.statusDot.FillColor = colorGreen
	s.statusDot.Refresh()
	s.statusLbl.SetText("运行中")
	s.addrLbl.SetText(fmt.Sprintf("[::]:%d (数据端口 %d)", ctrlPort, ctrlPort+1))
	s.actionBtn.SetText("停止服务")
	s.actionBtn.Importance = widget.DangerImportance
	s.portEntry.Disable()
	s.portMinEntry.Disable()
	s.portMaxEntry.Disable()
}

// updateStatusStopped sets UI to stopped state.
func (s *ServerState) updateStatusStopped() {
	s.statusDot.FillColor = colorGray
	s.statusDot.Refresh()
	s.statusLbl.SetText("已停止")
	s.addrLbl.SetText("")
	s.actionBtn.SetText("启动服务")
	s.actionBtn.Importance = widget.HighImportance
	s.portEntry.Enable()
	s.portMinEntry.Enable()
	s.portMaxEntry.Enable()
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
	// Listen on all interfaces for both IPv4 and IPv6
	s.controlLn, err = net.Listen("tcp", fmt.Sprintf("[::]:%d", ctrlPort))
	if err != nil {
		if strings.Contains(err.Error(), "address already in use") ||
			strings.Contains(err.Error(), "通常每个套接字地址") ||
			strings.Contains(err.Error(), "Only one usage") {
			return fmt.Errorf("端口 %d 已被占用，可能已有服务端实例在运行，请先关闭或更换端口", ctrlPort)
		}
		return fmt.Errorf("监听控制端口失败: %v", err)
	}

	dataPort := ctrlPort + 1
	s.dataLn, err = net.Listen("tcp", fmt.Sprintf("[::]:%d", dataPort))
	if err != nil {
		s.controlLn.Close()
		if strings.Contains(err.Error(), "address already in use") ||
			strings.Contains(err.Error(), "通常每个套接字地址") ||
			strings.Contains(err.Error(), "Only one usage") {
			return fmt.Errorf("数据端口 %d 已被占用，可能已有服务端实例在运行，请先关闭或更换端口", dataPort)
		}
		return fmt.Errorf("监听数据端口 %d 失败: %v", dataPort, err)
	}

	s.mu.Lock()
	s.running = true
	s.mu.Unlock()

	// Data connection accept loop
	go func() {
		for {
			conn, err := s.dataLn.Accept()
			if err != nil {
				return
			}
			go s.handleDataConn(conn)
		}
	}()

	// Control connection accept loop
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
	if !s.running {
		s.mu.Unlock()
		return
	}
	s.running = false

	// Close listeners first to stop accepting new connections
	if s.controlLn != nil {
		s.controlLn.Close()
		s.controlLn = nil
	}
	if s.dataLn != nil {
		s.dataLn.Close()
		s.dataLn = nil
	}

	// Close all tunnels
	for _, t := range s.tunnels {
		if t.PubLn != nil {
			t.PubLn.Close()
		}
		t.mu.Lock()
		for _, c := range t.Conns {
			c.Close()
		}
		t.Conns = make(map[string]net.Conn)
		t.mu.Unlock()
	}
	s.tunnels = make(map[string]*tunnelInfo)
	s.userData = make([]userEntry, 0)
	s.mu.Unlock()

	if s.userList != nil {
		s.userList.Refresh()
	}
	if s.userCnt != nil {
		s.userCnt.SetText("共 0 个连接")
	}
}

func (s *ServerState) handleControl(conn net.Conn) {
	defer conn.Close()

	s.uiAddLog(fmt.Sprintf("客户端连接: %s", conn.RemoteAddr()))

	for {
		// Set read deadline to detect stale connections (90s = 3 ping intervals)
		conn.SetReadDeadline(time.Now().Add(90 * time.Second))

		msg, err := common.ReadMsg(conn)
		if err != nil {
			s.uiAddLog(fmt.Sprintf("客户端断开: %s", conn.RemoteAddr()))
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

// findPortInRange finds an available TCP port randomly within the configured range.
func (s *ServerState) findPortInRange() (int, error) {
	var minPort, maxPort int
	fmt.Sscanf(s.portMinEntry.Text, "%d", &minPort)
	fmt.Sscanf(s.portMaxEntry.Text, "%d", &maxPort)
	if minPort <= 0 || maxPort <= 0 || minPort > maxPort {
		return 0, fmt.Errorf("端口范围无效: %s-%s", s.portMinEntry.Text, s.portMaxEntry.Text)
	}
	rng := maxPort - minPort + 1
	start := minPort + rand.Intn(rng)
	for i := 0; i < rng; i++ {
		port := minPort + (start - minPort + i) % rng
		ln, err := net.Listen("tcp", fmt.Sprintf("[::]:%d", port))
		if err == nil {
			ln.Close()
			return port, nil
		}
	}
	return 0, fmt.Errorf("端口范围 %d-%d 内无可用端口", minPort, maxPort)
}

func (s *ServerState) handleRegister(ctrlConn net.Conn, msg *common.Message) {
	pubPort, err := s.findPortInRange()
	if err != nil {
		common.WriteMsg(ctrlConn, &common.Message{Type: common.MsgError, Error: "分配公网端口失败: " + err.Error()})
		return
	}
	pubLn, err := net.Listen("tcp", fmt.Sprintf("[::]:%d", pubPort))
	if err != nil {
		common.WriteMsg(ctrlConn, &common.Message{Type: common.MsgError, Error: "分配公网端口失败"})
		return
	}

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
	s.uiAddLog(fmt.Sprintf("隧道注册: %s → 公网:%d (内网 %s:%d)", msg.TunnelID, pubPort, msg.LocalHost, msg.LocalPort))

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
		s.uiAddLog(fmt.Sprintf("新连接: %s → 隧道 %s", extConn.RemoteAddr(), t.ID))
		s.refreshUserList()
	}
}

func (s *ServerState) handleDataConn(conn net.Conn) {
	// Set a read deadline for the initial handshake
	conn.SetReadDeadline(time.Now().Add(10 * time.Second))

	msg, err := common.ReadMsg(conn)
	if err != nil || msg.Type != common.MsgConnReady {
		conn.Close()
		return
	}

	conn.SetReadDeadline(time.Time{}) // clear deadline for data transfer

	// Look up tunnel with read lock
	s.mu.RLock()
	t, ok := s.tunnels[msg.TunnelID]
	s.mu.RUnlock()
	if !ok {
		conn.Close()
		return
	}

	// Look up external connection
	t.mu.Lock()
	extConn, ok := t.Conns[msg.ConnID]
	t.mu.Unlock()
	if !ok {
		conn.Close()
		return
	}

	// Cleanup callback
	cleanup := func() {
		t.mu.Lock()
		if c, ok := t.Conns[msg.ConnID]; ok {
			c.Close()
			delete(t.Conns, msg.ConnID)
		}
		t.mu.Unlock()
		s.refreshUserList()
	}

	// Bidirectional copy with cleanup
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

// removeClientTunnels removes all tunnels owned by a disconnected client and cleans up resources.
func (s *ServerState) removeClientTunnels(ctrlConn net.Conn) {
	s.mu.Lock()
	defer s.mu.Unlock()

	for id, t := range s.tunnels {
		if t.CtrlConn == ctrlConn {
			// Close public listener first (stops new incoming connections)
			if t.PubLn != nil {
				t.PubLn.Close()
			}
			// Close all active connections for this tunnel
			t.mu.Lock()
			for connID, c := range t.Conns {
				c.Close()
				delete(t.Conns, connID)
			}
			t.mu.Unlock()
			delete(s.tunnels, id)
			s.uiAddLog(fmt.Sprintf("隧道已移除: %s", id))
		}
	}
}

func (s *ServerState) refreshUserList() {
	// Collect data under lock, then update UI on main thread
	s.mu.RLock()
	var entries []userEntry

	for _, t := range s.tunnels {
		// Add control connection entry
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

		// Count data connections for this tunnel
		t.mu.Lock()
		ds := len(t.Conns)
		t.mu.Unlock()
		_ = ds // 数据连接数已计入，可用于后续扩展显示
	}

	s.userData = entries
	s.mu.RUnlock()

	// UI updates
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
		s.uiAddLog("服务已停止")
		s.updateStatusStopped()
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

		s.updateStatusRunning(ctrlPort)
		s.uiAddLog(fmt.Sprintf("服务已启动 (控制端口 %d, 数据端口 %d)", ctrlPort, ctrlPort+1))
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

	state.portMinEntry = widget.NewEntry()
	state.portMinEntry.SetPlaceHolder("20000")
	state.portMinEntry.SetText("20000")
	state.portMinEntry.Wrapping = fyne.TextWrapOff

	state.portMaxEntry = widget.NewEntry()
	state.portMaxEntry.SetPlaceHolder("30000")
	state.portMaxEntry.SetText("30000")
	state.portMaxEntry.Wrapping = fyne.TextWrapOff

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
		widget.NewForm(
			widget.NewFormItem("端口", state.portEntry),
			widget.NewFormItem("端口范围", container.NewGridWithColumns(3,
				state.portMinEntry,
				widget.NewLabel("—"),
				state.portMaxEntry,
			)),
		),
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

	// Periodic user list refresh (screen refresh only, no data modification)
	go func() {
		ticker := time.NewTicker(3 * time.Second)
		defer ticker.Stop()
		for range ticker.C {
			state.mu.RLock()
			r := state.running
			state.mu.RUnlock()
			if r {
				state.refreshUserList()
			}
		}
	}()

	w.ShowAndRun()
}