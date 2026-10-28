package main

// 登录窗口：独立于主界面的登录对话框。服务器 IP/端口/配对码输入、
// 连接并登录、证书指纹首连确认、本地配置记忆（IP/端口/配对码可选 + 证书指纹必存）、
// 登出与令牌过期后回到登录窗口。

import (
	"context"
	"fmt"
	"log"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/lxn/walk"
	decl "github.com/lxn/walk/declarative"
)

// runLoginDialog 弹出独立登录窗口并阻塞，直到登录成功或用户退出。
// 返回 true 表示登录成功（a.client 已就绪）。
func (a *App) runLoginDialog() bool {
	var dlg *walk.Dialog
	var ipLE, portLE, codeLE *walk.LineEdit
	var rememberCk *walk.CheckBox
	var msgL *walk.Label
	var connectBtn, cancelBtn *walk.PushButton

	busyMu := sync.Mutex{}
	busy := false

	setBusy := func(b bool) {
		if connectBtn != nil {
			connectBtn.SetEnabled(!b)
		}
		if cancelBtn != nil {
			cancelBtn.SetEnabled(!b)
		}
	}
	setMsg := func(text string, isErr bool) {
		if isErr {
			msgL.SetTextColor(walk.RGB(200, 40, 40))
		} else {
			msgL.SetTextColor(walk.RGB(30, 120, 30))
		}
		msgL.SetText(text)
	}

	onConnect := func() {
		busyMu.Lock()
		if busy {
			busyMu.Unlock()
			return
		}
		busy = true
		busyMu.Unlock()

		ip := strings.TrimSpace(ipLE.Text())
		portStr := strings.TrimSpace(portLE.Text())
		code := codeLE.Text()
		remember := rememberCk.CheckState() == walk.CheckChecked

		fail := func(text string) {
			busyMu.Lock()
			busy = false
			busyMu.Unlock()
			setMsg(text, true)
		}

		if ip == "" {
			fail("请填写服务器 IP 或域名")
			return
		}
		port := 18113
		if portStr != "" {
			p, err := strconv.Atoi(portStr)
			if err != nil || p <= 0 || p > 65535 {
				fail("端口无效，应为 1-65535 的数字")
				return
			}
			port = p
		}
		if strings.TrimSpace(code) == "" {
			fail("请填写配对码")
			return
		}

		setBusy(true)
		setMsg("连接中……", false)

		go func() {
			ok := a.doLogin(dlg, ip, port, code, remember, func(status string, isErr bool) {
				dlg.Synchronize(func() { setMsg(status, isErr) })
			})
			dlg.Synchronize(func() {
				busyMu.Lock()
				busy = false
				busyMu.Unlock()
				setBusy(false)
				if ok {
					dlg.Accept()
				}
			})
		}()
	}

	if err := (decl.Dialog{
		AssignTo: &dlg,
		Title:    "登录 - DNF90 后台管理工具",
		MinSize:  decl.Size{Width: 580, Height: 300},
		Size:     decl.Size{Width: 580, Height: 300},
		Layout:   decl.VBox{Margins: decl.Margins{16, 16, 16, 12}, Spacing: 10},
		Children: []decl.Widget{
			decl.GroupBox{
				Title:  "服务器连接设置",
				Layout: decl.VBox{},
				Children: []decl.Widget{
					decl.Composite{
						Layout: decl.HBox{},
						Children: []decl.Widget{
							decl.Label{Text: "服务器 IP / 域名："},
							decl.LineEdit{AssignTo: &ipLE, Text: a.cfg.ServerIP},
							decl.Label{Text: "端口："},
							decl.LineEdit{AssignTo: &portLE, Text: strconv.Itoa(a.cfg.Port), MaxSize: decl.Size{Width: 90}},
						},
					},
					decl.Composite{
						Layout: decl.HBox{},
						Children: []decl.Widget{
							decl.Label{Text: "配对码："},
							decl.LineEdit{AssignTo: &codeLE, Text: a.cfg.PairingCode, PasswordMode: true},
							decl.CheckBox{
								AssignTo: &rememberCk,
								Text:     "记住配对码（明文存本机 config.json，存在泄露风险）",
								Checked:  a.cfg.RememberCode,
							},
						},
					},
				},
			},
			decl.Label{AssignTo: &msgL, Text: "输入服务器地址与配对码后点击「连接并登录」。", TextColor: walk.RGB(90, 90, 90)},
			decl.Composite{
				Layout: decl.HBox{MarginsZero: true, Spacing: 8},
				Children: []decl.Widget{
					decl.HSpacer{},
					decl.PushButton{AssignTo: &cancelBtn, Text: "退出", OnClicked: func() { dlg.Cancel() }},
					decl.PushButton{AssignTo: &connectBtn, Text: "连接并登录", OnClicked: onConnect},
				},
			},
		},
	}).Create(nil); err != nil {
		log.Printf("create login dialog failed: %v", err)
		walk.MsgBox(nil, "错误", "创建登录窗口失败："+err.Error(), walk.MsgBoxIconError)
		return false
	}

	return dlg.Run() == walk.DlgCmdOK
}

// doLogin 在后台 goroutine 执行完整登录链路：TLS 指纹 → 首连确认 → 保存参数 → 换 token。
func (a *App) doLogin(dlg *walk.Dialog, ip string, port int, code string, remember bool, setMsg func(string, bool)) bool {
	// 1. TLS 握手取指纹。
	fp, err := FetchFingerprint(ip, port)
	if err != nil {
		setMsg("连接失败："+err.Error(), true)
		return false
	}

	// 2. 指纹比对 / 首连确认（弹窗等待用户核对）。
	expect := strings.TrimSpace(a.cfg.CertFingerprint)
	if expect == "" {
		ok := make(chan bool, 1)
		dlg.Synchronize(func() {
			r := walk.MsgBox(dlg, "首次连接 - 证书指纹核对",
				"服务器证书 SHA256 指纹：\n\n"+fp+"\n\n"+
					"请与服务器端运维工具「远程管理」GUI 上展示的证书指纹人工核对。\n"+
					"确认一致后点击「是」信任并继续登录；点击「否」中止连接。",
				walk.MsgBoxYesNo|walk.MsgBoxIconQuestion|walk.MsgBoxDefButton2)
			ok <- r == walk.DlgCmdYes
		})
		if !<-ok {
			setMsg("用户取消：未信任服务器证书", true)
			return false
		}
		a.cfg.CertFingerprint = fp
	} else if expect != fp {
		setMsg("证书指纹不匹配，拒绝连接：\n当前 "+fp+"\n期望 "+expect, true)
		return false
	}

	// 3. 记住连接参数并保存（含指纹与可选配对码）。
	a.cfg.ServerIP = ip
	a.cfg.Port = port
	a.cfg.RememberCode = remember
	if remember {
		a.cfg.PairingCode = code
	} else {
		a.cfg.PairingCode = ""
	}
	a.saveConfig()

	// 4. 登录换 token。
	client := NewAPIClient(ip, port, fp)
	ctx, cancel := context.WithTimeout(context.Background(), 15*time.Second)
	defer cancel()
	token, _, err := client.Login(ctx, code)
	if err != nil {
		if apie, ok := asAPIErrPointer(err); ok {
			setMsg("登录失败："+apie.Code+"，请核对配对码/是否已启用", true)
			return false
		}
		setMsg("登录失败："+err.Error(), true)
		return false
	}
	client.SetToken(token)

	a.clientMu.Lock()
	a.client = client
	a.clientMu.Unlock()

	return true
}

// onLogout 登出：尽力调用 auth/logout（服务端未注册则 404，降级为本地清除）。
func (a *App) onLogout() {
	c := a.snapshotClient()
	if c != nil {
		go func() {
			ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
			defer cancel()
			_ = c.Logout(ctx) // 404/网络错误忽略，本地会话必清
		}()
	}
	a.requestRelogin(false)
}

// requestRelogin 清理本地会话并关闭主窗口，让 main 循环回到登录窗口。
func (a *App) requestRelogin(expired bool) {
	a.stopPolling()
	a.clientMu.Lock()
	a.client = nil
	a.clientMu.Unlock()
	// 账号名缓存（account_names.go）跟着会话走：换服/换登录身份后旧的 account_id → 用户名
	// 映射可能已经不对（同名不同服的账号主键不同），必须一起清掉，否则会串号显示。
	acctNames = newAccountNameResolver()
	a.relogin = true
	a.sync(func() {
		if a.statusText != nil {
			a.statusText.SetText("")
		}
		if a.statusFoot != nil {
			a.statusFoot.SetText("")
		}
		if a.banListText != nil {
			a.banListText.SetText("")
		}
		if a.cdkCodeTable != nil {
			a.cdkCodeTable.SetModel(&cdkCodeModel{})
		}
		if a.cdkRedeemTable != nil {
			a.cdkRedeemTable.SetModel(&cdkRedemptionModel{})
		}
		if expired {
			walk.MsgBox(a.mw, "登录已过期", "管理会话已过期或无效，请重新登录。", walk.MsgBoxOK|walk.MsgBoxIconWarning)
		}
		if a.mw != nil {
			a.mw.Close()
		}
	})
}

// requireRelogin 令牌失效统一处理：停止轮询、回到登录窗口并提示。
func (a *App) requireRelogin() {
	a.requestRelogin(true)
}

// ---------------- 小工具 ----------------

func asAPIErrPointer(err error) (*APIError, bool) {
	var apie *APIError
	if asAPIError(err, &apie) {
		return apie, true
	}
	return nil, false
}

func humanDuration(seconds int64) string {
	if seconds <= 0 {
		return "默认"
	}
	d := time.Duration(seconds) * time.Second
	return fmt.Sprintf("%d 分钟", int(d.Minutes()))
}
