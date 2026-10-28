package main

// 状态页：GET /manage/v1/status 关键状态展示，3 秒自动轮询；
// 一键启动 / 一键停止（二次确认后 POST /manage/v1/server/start|stop）。

import (
	"context"
	"fmt"
	"strings"
	"time"

	"github.com/lxn/walk"
	decl "github.com/lxn/walk/declarative"
)

func buildStatusTab(a *App) decl.TabPage {
	return decl.TabPage{
		Title:   "服务器状态",
		MinSize: decl.Size{Width: 900, Height: 520},
		Layout:  decl.VBox{},
		Children: []decl.Widget{
			decl.Composite{
				Layout: decl.HBox{},
				Children: []decl.Widget{
					decl.PushButton{
						AssignTo: &a.refreshBtn,
						Text:     "立即刷新",
						OnClicked: func() {
							go a.refreshStatusOnce(true)
						},
					},
					decl.PushButton{
						AssignTo: &a.startBtn,
						Text:     "一键启动",
						OnClicked: func() {
							a.onServerStart()
						},
					},
					decl.PushButton{
						AssignTo: &a.stopBtn,
						Text:     "一键停止",
						OnClicked: func() {
							a.onServerStop()
						},
					},
					decl.Label{Text: "状态每 3 秒自动刷新；启停操作为远端服务器核心操作，点击后有二次确认。"},
				},
			},
			decl.TextEdit{
				AssignTo: &a.statusText,
				ReadOnly: true,
				MinSize:  decl.Size{Width: 0, Height: 420},
				Font:     decl.Font{Family: "Consolas", PointSize: 9},
			},
			decl.Label{
				AssignTo:  &a.statusFoot,
				Text:      "尚未连接",
				TextColor: walk.RGB(120, 120, 120),
			},
		},
	}
}

// startPolling 启动 3s 轮询（幂等）。
func (a *App) startPolling() {
	a.stopPolling()
	ch := make(chan struct{})
	a.pollMu.Lock()
	a.pollCancel = ch
	a.pollMu.Unlock()

	go func() {
		tk := time.NewTicker(3 * time.Second)
		defer tk.Stop()
		for {
			select {
			case <-ch:
				return
			case <-tk.C:
				a.refreshStatusOnce(false)
			}
		}
	}()
}

// stopPolling 停止轮询（幂等）。
func (a *App) stopPolling() {
	a.pollMu.Lock()
	defer a.pollMu.Unlock()
	if a.pollCancel != nil {
		close(a.pollCancel)
		a.pollCancel = nil
	}
}

// refreshStatusOnce 拉取一次状态并渲染；quiet=true 时网络错误静默（避免弹窗轰炸）。
func (a *App) refreshStatusOnce(manual bool) {
	c := a.snapshotClient()
	if c == nil {
		return
	}
	ctx, cancel := context.WithTimeout(context.Background(), 8*time.Second)
	defer cancel()
	st, err := c.Status(ctx)
	if err != nil {
		if a.handleAPIError(err) {
			return
		}
		if manual {
			a.showError("状态拉取失败", err)
		}
		return
	}
	text := renderStatus(st)
	foot := fmt.Sprintf("最后刷新：%s  |  管理服务监听：%s", time.Now().Format("15:04:05"), st.Listen)
	a.sync(func() {
		setEditText(a.statusText, text)
		a.statusFoot.SetText(foot)
	})
}

func renderStatus(st *manageStatus) string {
	s := "=== 服务端运行状态 ===\n"
	s += fmt.Sprintf("目录：%s（存在=%v）\n", st.Server.Root, st.Server.RootExists)
	s += fmt.Sprintf("服务端程序：%v  实例配置：%v（有效=%v）\n", st.Server.ServerExe, st.Server.Instance, st.Server.InstanceValid)
	s += fmt.Sprintf("数据库模式：%s   频道端口：%d   管理端口：%d\n", st.Server.DatabaseMode, st.Server.ChannelPort, st.Server.AdminPort)
	s += fmt.Sprintf("运行中：%v   就绪：%v   需初始化：%v\n", st.Server.Running, st.Server.Ready, st.Server.NeedsInit)
	if st.Server.Message != "" {
		s += fmt.Sprintf("消息：%s\n", st.Server.Message)
	}

	s += "\n=== 认证服务 ===\n"
	s += fmt.Sprintf("运行中：%v   公网地址：%s   回环地址：%s\n", st.Auth.Running, st.Auth.PublicAddr, st.Auth.InternalURL)

	s += "\n=== MySQL ===\n"
	s += fmt.Sprintf("配置：%s   连接池可用：%v   模式：%s\n", st.MySQL.Configured, st.MySQL.Alive, st.MySQL.Mode)

	s += "\n=== 在线人数 ===\n"
	s += fmt.Sprintf("%d\n", st.Online)

	s += "\n=== CDK 汇总 ===\n"
	s += fmt.Sprintf("批次码总数：%d（启用 %d / 停用 %d）   兑换流水：%d   失败邮件：%d\n",
		st.CDK.Total, st.CDK.Enabled, st.CDK.Disabled, st.CDK.Redemptions, st.CDK.FailedMails)

	s += "\n=== 授权状态 ===\n"
	s += fmt.Sprintf("版本：%s   CDK 已设置：%v\n", editionLabel(st.License.Edition), st.License.CDKSet)
	s += fmt.Sprintf("卡类型：%s   到期时间：%s   剩余：%s\n",
		licenseKindLabel(st.License.Kind),
		orDash(shortLocalTime(st.License.ExpireAt)),
		licenseRemainLabel(st.License))
	if st.License.CheckedAt != "" {
		s += fmt.Sprintf("最后校验：%s\n", orDash(shortLocalTime(st.License.CheckedAt)))
	}

	s += "\n=== 远程管理 ===\n"
	s += fmt.Sprintf("配对码已设置：%v\n", st.Pairing.Enabled)
	s += fmt.Sprintf("管理证书：%s（存在=%v）\n", st.Cert.CertPath, st.Cert.Exists)
	s += fmt.Sprintf("监听：%s\n", st.Listen)
	return s
}

// editionLabel 把服务器上报的版本代号转成可读名称。
func editionLabel(e string) string {
	switch e {
	case "paid":
		return "赞助版"
	case "free":
		return "免费版"
	default:
		return e
	}
}

// licenseKindLabel 卡类型代号 → 可读名称（空 = 服务端未上报）。
func licenseKindLabel(k string) string {
	switch k {
	case "forever":
		return "永久卡"
	case "month":
		return "月卡"
	case "quarter":
		return "季卡"
	case "year":
		return "年卡"
	case "":
		return "未知"
	default:
		return k
	}
}

// licenseRemainLabel 剩余时长文案。
// 老服务端不返回 remain_days（Go 零值 0）且 kind/expire_at 也为空，按"未上报"处理，
// 避免把"没这个字段"误显示成"已到期"。
func licenseRemainLabel(l manageLicense) string {
	if l.RemainDays == 0 && l.Kind == "" && strings.TrimSpace(l.ExpireAt) == "" {
		return "未知（服务端未上报）"
	}
	switch {
	case l.RemainDays == -1:
		return "长期有效"
	case l.RemainDays == -2 || l.RemainDays < -1:
		return "未知"
	case l.RemainDays == 0:
		return "已到期"
	default:
		return fmt.Sprintf("%d 天", l.RemainDays)
	}
}

// shortLocalTime RFC3339 → 本地可读时间；空串返回空，解析失败原样返回。
func shortLocalTime(s string) string {
	s = strings.TrimSpace(s)
	if s == "" {
		return ""
	}
	t, err := time.Parse(time.RFC3339, s)
	if err != nil {
		return s
	}
	return t.Local().Format("2006-01-02 15:04")
}

func (a *App) onServerStart() {
	c := a.snapshotClient()
	if c == nil {
		return
	}
	if !a.confirm("一键启动确认", "确认远程启动服务器？\n该操作将按服务端一键启动链路拉起 DNF90 服务端进程。") {
		return
	}
	if !a.beginOp() {
		a.showInfo("操作进行中", "已有管理操作进行中，请稍候再试")
		return
	}
	defer a.endOp()
	a.startBtn.SetEnabled(false)
	defer func() { a.sync(func() { a.startBtn.SetEnabled(true) }) }()

	go func() {
		ctx, cancel := context.WithTimeout(context.Background(), 5*time.Minute)
		defer cancel()
		err := c.ServerStart(ctx)
		if err != nil {
			if !a.handleAPIError(err) {
				a.showError("启动失败", err)
			}
			return
		}
		a.showInfo("启动完成", "服务器启动指令已执行成功。")
		a.refreshStatusOnce(true)
	}()
}

func (a *App) onServerStop() {
	c := a.snapshotClient()
	if c == nil {
		return
	}
	if !a.confirm("一键停止确认", "确认远程停止服务器？\n该操作会停止 DNF90 服务端进程，在线玩家将掉线。") {
		return
	}
	if !a.beginOp() {
		a.showInfo("操作进行中", "已有管理操作进行中，请稍候再试")
		return
	}
	defer a.endOp()
	a.stopBtn.SetEnabled(false)
	defer func() { a.sync(func() { a.stopBtn.SetEnabled(true) }) }()

	go func() {
		ctx, cancel := context.WithTimeout(context.Background(), 5*time.Minute)
		defer cancel()
		err := c.ServerStop(ctx)
		if err != nil {
			if !a.handleAPIError(err) {
				a.showError("停止失败", err)
			}
			return
		}
		a.showInfo("停止完成", "服务器停止指令已执行成功。")
		a.refreshStatusOnce(true)
	}()
}
