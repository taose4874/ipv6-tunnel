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

type ClientState struct {
	mu         sync.Mutex
	connected  bool
	ctrlConn   net.Conn
	localPort  int
	pubPort    int
	dataAddr   string
	stopCh     chan struct{}

	win        fyne.Window
	statusDot  *canvas.Circle
	statusLbl  *widget.Label
	infoEntry  *widget.Entry
	copyBtn    *widget.Button
	actionBtn  *widget.Button
	logEntry   *widget.Entry
	addrEntry  *widget.Entry
	portEntry  *widget.Entry
	localEntry *widget.Entry
	prefs      fyne.Preferences

	settingBox  *fyne.Container
	infoBox     *fyne.Container
}

var cstate *ClientState

func (s *ClientState) addLog(msg string) {
	t := time.Now().Format("15:04:05")
	prev := s.logEntry.Text
	if len(prev) > 20000 {
		prev = prev[len(prev)-10000:]
	}
	s.logEntry.SetText(prev + fmt.Sprintf("[%s] %s\n", t, msg))
}

func (s *ClientState) savePrefs() {
	s.prefs.SetString("server_addr", s.addrEntry.Text)
	s.prefs.SetString("server_port", s.portEntry.Text)
	s.prefs.SetString("local_port", s.localEntry.Text)
}

func (s *ClientState) connect() error {
	s.mu.Lock()
	if s.connected {
		s.mu.Unlock()
		return fmt.Errorf("已连接")
	}
	s.mu.Unlock()

	addr := s.addrEntry.Text
	portStr := s.portEntry.Text
	if addr == "" || portStr == "" {
		return fmt.Errorf("请填写服务端地址和端口")
	}

	localStr := s.localEntry.Text
	var localPort int
	fmt.Sscanf(localStr, "%d", &localPort)
	if localPort == 0 {
		return fmt.Errorf("请填写本地端口")
	}

	s.savePrefs()

	serverAddr := fmt.Sprintf("%s:%s", addr, portStr)

	ctrlConn, err := net.DialTimeout("tcp", serverAddr, 5*time.Second)
	if err != nil {
		return fmt.Errorf("连接服务端失败: %v", err)
	}

	err = common.WriteMsg(ctrlConn, &common.Message{
		Type:      common.MsgRegister,
		TunnelID:  "default",
		LocalHost: "127.0.0.1",
		LocalPort: localPort,
	})
	if err != nil {
		ctrlConn.Close()
		return fmt.Errorf("注册失败: %v", err)
	}

	ctrlConn.SetReadDeadline(time.Now().Add(5 * time.Second))
	msg, err := common.ReadMsg(ctrlConn)
	if err != nil {
		ctrlConn.Close()
		return fmt.Errorf("读取确认失败: %v", err)
	}
	if msg.Type == common.MsgError {
		ctrlConn.Close()
		return fmt.Errorf("%s", msg.Error)
	}

	var cp int
	fmt.Sscanf(portStr, "%d", &cp)
	dataAddr := fmt.Sprintf("%s:%d", addr, cp+1)

	s.mu.Lock()
	s.connected = true
	s.ctrlConn = ctrlConn
	s.localPort = localPort
	s.pubPort = msg.PubPort
	s.dataAddr = dataAddr
	s.stopCh = make(chan struct{})
	s.mu.Unlock()

	s.addLog(fmt.Sprintf("隧道就绪 → 公网端口 %d", msg.PubPort))

	go func() {
		ticker := time.NewTicker(30 * time.Second)
		defer ticker.Stop()
		for {
			select {
			case <-ticker.C:
				s.mu.Lock()
				conn := s.ctrlConn
				s.mu.Unlock()
				if conn != nil {
					common.WriteMsg(conn, &common.Message{Type: common.MsgPing})
				}
			case <-s.stopCh:
				return
			}
		}
	}()

	go func() {
		for {
			ctrlConn.SetReadDeadline(time.Time{})
			msg, err := common.ReadMsg(ctrlConn)
			if err != nil {
				s.addLog("与服务端断开连接")
				s.doDisconnect()
				return
			}
			switch msg.Type {
			case common.MsgNewConn:
				go s.handleNewConn(msg)
			case common.MsgPong:
			}
		}
	}()

	return nil
}

func (s *ClientState) disconnect() {
	s.mu.Lock()
	if !s.connected {
		s.mu.Unlock()
		return
	}
	s.mu.Unlock()
	s.doDisconnect()
}

func (s *ClientState) doDisconnect() {
	s.mu.Lock()
	defer s.mu.Unlock()
	if !s.connected {
		return
	}
	s.connected = false
	if s.ctrlConn != nil {
		s.ctrlConn.Close()
		s.ctrlConn = nil
	}
	select {
	case <-s.stopCh:
	default:
		close(s.stopCh)
	}
	s.addLog("已断开连接")
}

func (s *ClientState) handleNewConn(msg *common.Message) {
	s.mu.Lock()
	lp := s.localPort
	da := s.dataAddr
	s.mu.Unlock()

	localConn, err := net.DialTimeout("tcp", fmt.Sprintf("127.0.0.1:%d", lp), 5*time.Second)
	if err != nil {
		s.addLog(fmt.Sprintf("连接本地服务失败: %v", err))
		return
	}

	dataConn, err := net.DialTimeout("tcp", da, 5*time.Second)
	if err != nil {
		localConn.Close()
		s.addLog(fmt.Sprintf("连接数据通道失败: %v", err))
		return
	}

	common.WriteMsg(dataConn, &common.Message{
		Type:     common.MsgConnReady,
		TunnelID: "default",
		ConnID:   msg.ConnID,
	})
	go func() { io.Copy(dataConn, localConn); dataConn.Close(); localConn.Close() }()
	go func() { io.Copy(localConn, dataConn); localConn.Close(); dataConn.Close() }()
}

func (s *ClientState) toggleAction() {
	if s.connected {
		s.disconnect()
		s.statusDot.FillColor = colorGray
		s.statusDot.Refresh()
		s.statusLbl.SetText("未连接")
		s.actionBtn.SetText("连接")
		s.actionBtn.Importance = widget.HighImportance
		s.addrEntry.Enable()
		s.portEntry.Enable()
		s.localEntry.Enable()
		s.infoBox.Hide()
		s.settingBox.Show()
	} else {
		if err := s.connect(); err != nil {
			dialog.ShowError(err, s.win)
			return
		}
		s.statusDot.FillColor = colorGreen
		s.statusDot.Refresh()
		s.statusLbl.SetText("已连接")
		pubAddr := fmt.Sprintf("%s:%d", s.addrEntry.Text, s.pubPort)
		s.infoEntry.SetText(pubAddr)
		s.actionBtn.SetText("断开")
		s.actionBtn.Importance = widget.DangerImportance
		s.addrEntry.Disable()
		s.portEntry.Disable()
		s.localEntry.Disable()
		s.settingBox.Hide()
		s.infoBox.Show()
	}
}

func main() {
	log.SetFlags(0)

	a := app.NewWithID("intranet-pen-client")
	a.Settings().SetTheme(&bigTheme{})
	prefs := a.Preferences()

	savedAddr := prefs.StringWithFallback("server_addr", "")
	savedPort := prefs.StringWithFallback("server_port", "8888")
	savedLocal := prefs.StringWithFallback("local_port", "25565")

	w := a.NewWindow("内网穿透 · 客户端")
	w.Resize(fyne.NewSize(520, 540))

	cstate = &ClientState{prefs: prefs}

	// Status
	cstate.statusDot = canvas.NewCircle(colorGray)
	cstate.statusDot.Resize(fyne.NewSize(12, 12))
	cstate.statusLbl = widget.NewLabel("未连接")

	cstate.actionBtn = widget.NewButton("连接", func() { cstate.toggleAction() })
	cstate.actionBtn.Importance = widget.HighImportance

	// Connection form
	cstate.addrEntry = widget.NewEntry()
	cstate.addrEntry.SetPlaceHolder("例如 192.168.1.100")
	cstate.addrEntry.SetText(savedAddr)

	cstate.portEntry = widget.NewEntry()
	cstate.portEntry.SetText(savedPort)

	cstate.localEntry = widget.NewEntry()
	cstate.localEntry.SetPlaceHolder("例如 25565")
	cstate.localEntry.SetText(savedLocal)

	formBox := container.NewVBox(
		widget.NewForm(
			widget.NewFormItem("服务端地址", cstate.addrEntry),
			widget.NewFormItem("服务端端口", cstate.portEntry),
			widget.NewFormItem("本地端口", cstate.localEntry),
		),
	)

	cstate.settingBox = container.NewVBox(
		widget.NewLabelWithStyle("连接设置", fyne.TextAlignLeading, fyne.TextStyle{Bold: true}),
		widget.NewSeparator(),
		container.NewPadded(formBox),
	)

	// Connection info
	cstate.infoEntry = widget.NewEntry()
	cstate.infoEntry.Disable()

	cstate.copyBtn = widget.NewButton("复制", func() {
		w.Clipboard().SetContent(cstate.infoEntry.Text)
	})

	infoRow := container.NewBorder(nil, nil, nil, cstate.copyBtn, cstate.infoEntry)

	cstate.infoBox = container.NewVBox(
		widget.NewLabelWithStyle("公网地址", fyne.TextAlignLeading, fyne.TextStyle{Bold: true}),
		widget.NewSeparator(),
		container.NewPadded(infoRow),
	)
	cstate.infoBox.Hide()

	// Section: settings or info (swap when connected)
	midSection := container.NewMax(cstate.settingBox, cstate.infoBox)

	// Log
	cstate.logEntry = widget.NewEntry()
	cstate.logEntry.MultiLine = true
	cstate.logEntry.Wrapping = fyne.TextWrapOff
	cstate.logEntry.SetPlaceHolder("日志输出...")

	logSection := container.NewBorder(
		widget.NewLabelWithStyle("日志", fyne.TextAlignLeading, fyne.TextStyle{Bold: true}),
		nil, nil, nil,
		cstate.logEntry,
	)

	// Top status bar
	topBar := container.NewBorder(
		nil, nil,
		container.NewHBox(
			container.NewPadded(cstate.statusDot),
			cstate.statusLbl,
		),
		cstate.actionBtn,
	)

	topBox := container.NewVBox(
		container.NewPadded(topBar),
		widget.NewSeparator(),
	)

	logScroll := container.NewScroll(logSection)
	logScroll.SetMinSize(fyne.NewSize(0, 100))

	content := container.NewBorder(
		topBox,
		nil, nil, nil,
		container.NewBorder(
			container.NewVBox(
				container.NewPadded(midSection),
				widget.NewSeparator(),
			),
			nil, nil, nil,
			logScroll,
		),
	)
	w.SetContent(content)

	w.SetOnClosed(func() {
		cstate.savePrefs()
		cstate.disconnect()
	})

	w.ShowAndRun()
}