//go:build paid

// 赞助版在线授权：启动弹登录框验证 CDK → 通过后进入主界面，
// 后台 30s 心跳；证书固定（内嵌 DER SHA256 指纹）防中间人；
// 心跳失败达到阈值 / 服务端吊销（REVOKED）立即退出，不做宽限。
//
// 限时卡（月/季/年）支持（与 authserver license_tier.go 配套）：
//   - 上报 proto=2；服务端 min-proto=2 时，未上报 proto 的老版本工具一律被拒登录；
//   - verify 成功返回 kind/expire_at/remain_days，落盘 config.License 供「授权信息」页与
//     远程管理状态页展示（判定始终以服务端为准，客户端本地时间不参与放行）；
//   - 心跳收到 ERR_CDK_EXPIRED：旧卡此刻已失效，但给一次「原地续费」机会
//     （管理员续期后重试原卡，或粘贴新卡），取消才退出——这不是宽限，是避免客户丢工作；
//   - 剩余 ≤7 天：每次启动提醒一次。
package main

import (
	"bytes"
	"crypto/sha256"
	"crypto/tls"
	"crypto/x509"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"strings"
	"sync"
	"time"

	"github.com/lxn/walk"
	. "github.com/lxn/walk/declarative"
)

const (
	// 【已去授权】验证服务器地址与证书指纹不再内置：留空即代表"无在线授权"。
	// 恢复在线授权时把这两项填回真实值（地址形如 https://<host>:8443，
	// 指纹为服务端证书 DER 的 SHA-256 十六进制小写）。
	authServerURL   = ""
	authCertDERHash = ""

	authTimeout      = 10 * time.Second
	heartbeatEvery   = 30 * time.Second
	heartbeatMaxFail = 3 // 连续失败即断线退出（不宽限）

	// clientProto 客户端协议版本，须与服务端 clientProtoCurrent 一致。
	// 服务端 min-proto=2 时：不发 proto 的老版本工具（≤2026-09 构建）被拒绝登录。
	clientProto = 2

	// licenseWarnDays 剩余天数 ≤ 该值时，每次启动提醒续费。
	licenseWarnDays = 7

	kindForever = "forever"
)

// authResp 与 authserver 的通用响应结构一致。
type authResp struct {
	Ok   bool   `json:"ok"`
	Code string `json:"code"`
	Msg  string `json:"msg"`
	Data *struct {
		Token            string `json:"token"`
		Kind             string `json:"kind"`
		KindLabel        string `json:"kind_label"`
		ExpireAt         string `json:"expire_at"`
		ExpireText       string `json:"expire_text"`
		RemainDays       int    `json:"remain_days"`
		FirstActivatedAt string `json:"first_activated_at"`
		ServerTime       string `json:"server_time"`
	} `json:"data"`
}

// verifyOutcome /api/verify 成功后的授权信息。
type verifyOutcome struct {
	Token            string
	Kind             string
	KindLabel        string
	ExpireAt         string
	ExpireText       string
	RemainDays       int
	FirstActivatedAt string
	ServerTime       string
}

// ---------------- 授权运行态 ----------------

// licState 当前授权运行态。心跳协程与 UI 线程并发访问，统一走 mutex。
// cdk/token 放在这里（而不是心跳启动时的闭包）是为了支持「运行中更换授权码 / 到期后续费」：
// 心跳每次都用最新值，换卡后无需重启工具。
type licState struct {
	mu         sync.Mutex
	cdk        string
	token      string
	out        verifyOutcome
	lastOK     time.Time
	lastErr    string
	lastErrAt  time.Time
	hasOutcome bool
}

var lic licState

func (s *licState) set(cdk, token string) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.cdk, s.token = cdk, token
}

func (s *licState) setOutcome(cdk, token string, out verifyOutcome) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if cdk != "" {
		s.cdk = cdk
	}
	s.token = token
	s.out = out
	s.hasOutcome = true
	s.lastOK = time.Now()
	s.lastErr = ""
}

func (s *licState) setToken(token string) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.token = token
}

func (s *licState) markOK() {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.lastOK = time.Now()
	s.lastErr = ""
}

func (s *licState) markErr(msg string) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.lastErr = msg
	s.lastErrAt = time.Now()
}

// snapshot 返回当前 CDK/token 与授权信息副本。
func (s *licState) snapshot() (cdk, token string, out verifyOutcome, has bool, lastOK time.Time, lastErr string) {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.cdk, s.token, s.out, s.hasOutcome, s.lastOK, s.lastErr
}

// ---------------- 授权门 ----------------

// verifyLicenseGate 授权门。
//
// 【已去授权】本函数现在**直接放行**：不弹 CDK 登录框、不校验授权、不启动心跳，
// 返回 nil 让 main 继续进入主界面。行为与免费版（license_free.go）一致。
//
// 为什么保留函数外壳：main.go 里 `if err := verifyLicenseGate(a); err != nil` 的
// 调用结构、以及 remote_tab.go 里"通过后才 maybeStartManage"的注释都还在，
// 保留外壳可以让"要不要恢复授权"只是本文件内部的事，不必再动启动流程。
//
// 恢复方法：把下面这行 return nil 换成 verifyLicenseGateLegacy(a)，
// 并重新填入验证服务器地址与证书指纹（见文件头常量区）。
func verifyLicenseGate(a *App) error {
	a.logf("授权门已跳过（去授权版本），直接进入主界面")
	return nil
}

// verifyLicenseGateLegacy 是去授权前的原实现，**当前不会被调用**。
// 恢复在线授权时调用它即可（需先在常量区填回 authServerURL 与 authCertDERHash）。
func verifyLicenseGateLegacy(a *App) error {
	for {
		cdk := strings.TrimSpace(a.cfg.License.CDK)
		// 本地缓存只用于「离线展示剩余天数」，不放行任何访问；服务端仍是唯一裁决方。
		if cdk != "" {
			out, code, msg, err := authVerify(cdk)
			if err == nil {
				a.logf("赞助版授权验证通过，CDK %s", maskCDK(cdk))
				logLicenseInfo(a, out)
				persistLicense(a, cdk, out)
				lic.setOutcome(cdk, out.Token, out)
				startHeartbeat(a, cdk, out.Token)
				warnIfExpiringSoon(a, out)
				return nil
			}
			reason := msg
			if reason == "" {
				reason = err.Error()
			}
			a.logf("授权验证失败(%s): %s", code, reason)
			// 已存 CDK 失效/被吊销/到期/换机：告知原因后请用户决定是否重输。
			retry := walk.MsgBox(a.mw, "授权验证失败", reason+"\n\n是否重新输入 CDK 重试？", walk.MsgBoxYesNo|walk.MsgBoxIconError) == walk.DlgCmdYes
			if !retry {
				return errors.New("授权验证失败，工具退出")
			}
			a.cfg.License.CDK = "" // 清空后走登录框重输
			_ = a.cfg.Save()
			continue
		}

		cdk, token, ok, err := promptCDKDialog(a, cdkDialogOpts{})
		if err != nil {
			return err
		}
		if !ok {
			return errors.New("已取消授权验证，工具退出")
		}
		if _, _, out, has, _, _ := lic.snapshot(); has {
			persistLicense(a, cdk, out)
			logLicenseInfo(a, out)
			warnIfExpiringSoon(a, out)
		}
		startHeartbeat(a, cdk, token)
		return nil
	}
}

// persistLicense 把授权信息落盘（供「授权信息」页与远程管理状态页展示；不参与放行判定）。
func persistLicense(a *App, cdk string, out verifyOutcome) {
	a.cfg.License.CDK = cdk
	a.cfg.License.Kind = out.Kind
	a.cfg.License.ExpireAt = out.ExpireAt
	a.cfg.License.RemainDays = out.RemainDays
	a.cfg.License.CheckedAt = time.Now().Format(time.RFC3339)
	if err := a.cfg.Save(); err != nil {
		a.logf("保存授权配置失败: %v", err)
	}
}

// logLicenseInfo 启动/续期时打印一行授权概况（永久卡不提示剩余）。
func logLicenseInfo(a *App, out verifyOutcome) {
	label := out.KindLabel
	if label == "" {
		label = "授权卡"
	}
	if isForeverOutcome(out) {
		a.logf("授权类型：%s（长期有效）", label)
		return
	}
	a.logf("授权类型：%s，到期 %s，剩余 %d 天", label, out.ExpireText, out.RemainDays)
}

func isForeverOutcome(out verifyOutcome) bool {
	return out.Kind == "" || out.Kind == kindForever || out.RemainDays < 0
}

// ---------------- 到期提醒（非模态，禁止改成 MsgBox） ----------------

var (
	licNoticeMu sync.Mutex
	licNotice   *walk.Dialog
)

// closeLicenseNotice 关闭尚未关闭的到期提醒；必须在 UI 线程调用。
func closeLicenseNotice() {
	licNoticeMu.Lock()
	d := licNotice
	licNotice = nil
	licNoticeMu.Unlock()
	if d != nil {
		d.Close(0)
	}
}

// warnIfExpiringSoon 剩余 ≤7 天时提醒一次。
//
// 这里必须用【非模态】对话框（Show 而非 Run/MsgBox）：
// 模态 MsgBox 会占住 UI 线程的消息循环，实测后果是——客户把"即将到期"提示框晾着不管时
// （多数人都会），到点后心跳触发的续费框要排队 20 秒以上才建得起来，且建起来就被连带关闭，
// 客户端于是直接退出：一个"提醒"就这样变成了"提前踢人"，期间整个界面也是冻结的。
// 非模态 + 30 秒自动关闭：不冻结界面、不与续费框抢消息循环。
func warnIfExpiringSoon(a *App, out verifyOutcome) {
	if isForeverOutcome(out) || out.RemainDays < 0 || out.RemainDays > licenseWarnDays {
		return
	}
	a.logf("授权将于 %s 到期（剩余 %d 天），请及时续费", out.ExpireText, out.RemainDays)
	go func() {
		time.Sleep(1200 * time.Millisecond) // 等主窗口真正可见
		a.uiSafe(func() {
			var dlg *walk.Dialog
			if err := (Dialog{
				AssignTo: &dlg,
				Title:    "授权即将到期",
				MinSize:  Size{Width: 480, Height: 200},
				Size:     Size{Width: 480, Height: 200},
				Layout:   VBox{Margins: Margins{16, 16, 16, 12}, Spacing: 10},
				Children: []Widget{
					Label{Text: fmt.Sprintf("您的%s将于 %s 到期（剩余 %d 天）。",
						orDefault(out.KindLabel, "授权"), out.ExpireText, out.RemainDays)},
					Label{Text: "到期后工具会立即停止服务，请及时联系作者续费。"},
					Label{Text: "（本提示不影响工具使用，30 秒后自动关闭）", TextColor: walk.RGB(120, 120, 120)},
					Composite{
						Layout: HBox{MarginsZero: true, Spacing: 8},
						Children: []Widget{
							HSpacer{},
							PushButton{Text: "知道了", OnClicked: func() {
								if dlg != nil {
									dlg.Close(0)
								}
							}},
						},
					},
				},
			}).Create(a.mw); err != nil {
				a.logf("到期提醒弹框创建失败（不影响使用）: %v", err)
				return
			}
			// 任何关闭路径（按钮 / 右上角 / 自动关闭 / 被续费框顶掉）都清掉句柄
			dlg.Closing().Attach(func(canceled *bool, reason walk.CloseReason) {
				licNoticeMu.Lock()
				if licNotice == dlg {
					licNotice = nil
				}
				licNoticeMu.Unlock()
			})
			licNoticeMu.Lock()
			licNotice = dlg
			licNoticeMu.Unlock()
			dlg.Show() // 非模态：立即返回，不占消息循环
			time.AfterFunc(30*time.Second, func() {
				a.uiSafe(func() { closeLicenseNotice() })
			})
		})
	}()
}

func orDefault(s, def string) string {
	if strings.TrimSpace(s) == "" {
		return def
	}
	return s
}

// ---------------- 授权码输入/续费对话框 ----------------

// cdkDialogOpts 授权码对话框参数；零值 = 首次登录场景（与历史行为一致）。
type cdkDialogOpts struct {
	Title    string // 窗口标题
	Reason   string // 顶部原因提示（红字，续费/失败重试场景）
	Hint     string // 输入框上方说明
	OKText   string // 确认按钮文案
	RetryCDK string // 非空时显示「重试原卡」按钮（管理员已续期，客户无需换码）
}

// promptCDKDialog 弹出输入框让用户填写 CDK；对话框内即完成 /api/verify，
// 失败红字提示可重试，成功 Accept 返回 (cdk, token, true)。
func promptCDKDialog(a *App, opts cdkDialogOpts) (cdk, token string, ok bool, err error) {
	title := opts.Title
	if title == "" {
		title = "赞助版授权验证"
	}
	hint := opts.Hint
	if hint == "" {
		hint = "请输入授权 CDK（联系作者获取；CDK 绑定您当前公网 IP，一台一码）"
	}
	okText := opts.OKText
	if okText == "" {
		okText = "验证并启动"
	}

	var dlg *walk.Dialog
	var le *walk.LineEdit
	var statusL *walk.Label
	var okBtn, cancelBtn *walk.PushButton
	gotToken := "" // 验证成功后由 Synchronize 回调写入，随后 Accept 返回
	gotCDK := ""   // 验证成功时捕获的 CDK 明文；Run 返回后控件已销毁，不能读 le.Text()

	busyMu := sync.Mutex{}
	busy := false
	setBusy := func(b bool) {
		if okBtn != nil {
			okBtn.SetEnabled(!b)
		}
		if cancelBtn != nil {
			cancelBtn.SetEnabled(!b)
		}
	}
	setStatus := func(text string, red bool) {
		if red {
			statusL.SetTextColor(walk.RGB(200, 30, 30))
		} else {
			statusL.SetTextColor(walk.RGB(32, 120, 32))
		}
		statusL.SetText(text)
	}

	onVerify := func() {
		busyMu.Lock()
		if busy {
			busyMu.Unlock()
			return
		}
		busy = true
		busyMu.Unlock()
		setBusy(true)
		setStatus("正在连接授权服务器...", false)

		input := strings.TrimSpace(le.Text())
		go func() {
			out, code, msg, verr := authVerify(input)
			dlg.Synchronize(func() {
				busyMu.Lock()
				busy = false
				busyMu.Unlock()
				if verr == nil {
					gotToken = out.Token
					gotCDK = input
					lic.setOutcome(input, out.Token, out)
					setStatus("验证通过："+describeOutcome(out), false)
					time.Sleep(80 * time.Millisecond) // 让状态落屏
					dlg.Accept()
					return
				}
				setBusy(false)
				reason := msg
				if reason == "" {
					reason = verr.Error()
				}
				setStatus("验证失败："+reason, true)
				_ = code
			})
		}()
	}

	children := []Widget{Label{Text: hint}}
	if strings.TrimSpace(opts.Reason) != "" {
		children = append(children, Label{
			Text:      opts.Reason,
			TextColor: walk.RGB(200, 30, 30),
		})
	}
	children = append(children,
		LineEdit{
			AssignTo:    &le,
			ToolTipText: "CDK 形如 AAAAA-BBBBB-CCCCC-DDDDD",
		},
		Label{AssignTo: &statusL, Text: "提示：首次验证通过后自动保存到 config.json，下次启动免输入。"},
		Composite{
			Layout: HBox{MarginsZero: true, Spacing: 8},
			Children: func() []Widget {
				ws := []Widget{HSpacer{}}
				if strings.TrimSpace(opts.RetryCDK) != "" {
					retry := strings.TrimSpace(opts.RetryCDK)
					ws = append(ws, PushButton{
						Text:        "重试原卡（已续期）",
						ToolTipText: "管理员已为您续期时点此按钮：用原授权码重新验证，无需换码",
						OnClicked: func() {
							le.SetText(retry)
							onVerify()
						},
					})
				}
				ws = append(ws,
					PushButton{AssignTo: &cancelBtn, Text: "退出", OnClicked: func() { dlg.Cancel() }},
					PushButton{AssignTo: &okBtn, Text: okText, OnClicked: onVerify},
				)
				return ws
			}(),
		},
	)

	if err := (Dialog{
		AssignTo: &dlg,
		Title:    title,
		MinSize:  Size{Width: 560, Height: 260},
		Size:     Size{Width: 560, Height: 260},
		Layout:   VBox{Margins: Margins{16, 16, 16, 12}, Spacing: 10},
		Children: children,
	}).Create(a.mw); err != nil {
		return "", "", false, err
	}

	if r := dlg.Run(); r != walk.DlgCmdOK {
		return "", "", false, nil
	}
	return gotCDK, gotToken, true, nil
}

// describeOutcome 授权信息一句话描述（永久卡不显示剩余）。
func describeOutcome(out verifyOutcome) string {
	label := orDefault(out.KindLabel, "授权卡")
	if isForeverOutcome(out) {
		return label + "（长期有效）"
	}
	return fmt.Sprintf("%s（到期 %s，剩余 %d 天）", label, out.ExpireText, out.RemainDays)
}

// askRenewal 到期时的原地续费：在 UI 线程弹续费框并等待结果。
// 返回 true 表示已用（续期后的原卡或新卡）验证成功，心跳可继续。
func (a *App) askRenewal(reason string) bool {
	if a.mw == nil {
		return false
	}
	ch := make(chan bool, 1)
	a.uiSafe(func() {
		closeLicenseNotice() // 续费框是模态的，先把非模态提醒收掉，避免两个窗口叠在一起让客户点错
		curCDK, _, _, _, _, _ := lic.snapshot()
		cdk, token, ok, err := promptCDKDialog(a, cdkDialogOpts{
			Title:    "授权已到期 · 续费",
			Reason:   reason,
			Hint:     "管理员已续期 → 点「重试原卡（已续期）」；购买了新卡 → 粘贴新授权码后验证。",
			OKText:   "验证并继续",
			RetryCDK: curCDK,
		})
		if err != nil || !ok {
			ch <- false
			return
		}
		if _, _, out, has, _, _ := lic.snapshot(); has {
			persistLicense(a, cdk, out)
			logLicenseInfo(a, out)
			refreshLicenseUI(a)
		}
		_ = token
		ch <- true
	})
	return <-ch
}

// ---------------- 心跳 ----------------

// startHeartbeat 启动后台心跳；断线（连续失败）/ 服务端吊销 / 版本过期即退出。
//
// 与旧版的差别：
//   - CDK/token 每次心跳从 lic 运行态读取 → 运行中更换授权码或续费后无需重启；
//   - ERR_TOKEN_INVALID 用「当前」CDK 重新 verify 续期（授权服务器重启/改绑清会话）；
//   - ERR_CDK_EXPIRED 先给一次原地续费机会（旧卡此刻已失效，不算宽限），取消才退出；
//   - ERR_CLIENT_OUTDATED 为致命码：必须升级客户端。
func startHeartbeat(a *App, cdk, token string) {
	lic.set(cdk, token)
	go func() {
		time.Sleep(2 * time.Second) // 等待主窗口消息循环启动
		ticker := time.NewTicker(heartbeatEvery)
		defer ticker.Stop()
		fails := 0
		for range ticker.C {
			curCDK, curToken, _, _, _, _ := lic.snapshot()
			if strings.TrimSpace(curCDK) == "" {
				continue
			}
			code, msg, err := authHeartbeat(curCDK, curToken)
			if err == nil {
				if fails > 0 {
					a.logf("心跳恢复")
				}
				fails = 0
				lic.markOK()
				refreshLicenseUI(a)
				continue
			}

			switch code {
			case "ERR_TOKEN_INVALID":
				a.logf("会话已失效，自动重新验证授权…")
				out, vcode, vmsg, verr := authVerify(curCDK)
				if verr == nil {
					a.logf("重新验证成功，授权已续期")
					lic.setOutcome(curCDK, out.Token, out)
					persistLicense(a, curCDK, out)
					refreshLicenseUI(a)
					fails = 0
					continue
				}
				code, msg, err = vcode, vmsg, verr

			case "ERR_CDK_EXPIRED":
				// 到期：旧卡已立即失效；给一次原地续费机会，取消则退出。
				a.logf("授权已到期：%s", msg)
				lic.markErr(msg)
				refreshLicenseUI(a)
				if a.askRenewal(msg) {
					a.logf("续费成功，授权已恢复")
					fails = 0
					continue
				}
				exitWithMsg(a, msg+"\n工具将退出。")
				return

			case "ERR_CLIENT_OUTDATED":
				// 版本闸门：服务端已停止对本版本授权，必须升级。
				lic.markErr(msg)
				exitWithMsg(a, msg+"\n工具将退出。")
				return
			}

			fails++
			reason := msg
			if reason == "" && err != nil {
				reason = err.Error()
			}
			lic.markErr(reason)
			refreshLicenseUI(a)
			a.logf("心跳失败(%d/%d): %s", fails, heartbeatMaxFail, reason)
			if code == "REVOKED" || code == "ERR_CDK_REVOKED" ||
				code == "ERR_CDK_INACTIVE" || code == "ERR_IP_MISMATCH" || code == "ERR_CDK_NOT_FOUND" {
				exitWithMsg(a, "授权已被吊销/解绑："+reason+"\n工具将退出。")
				return
			}
			if fails >= heartbeatMaxFail {
				exitWithMsg(a, "与授权服务器连接中断，工具将退出。\n请恢复网络后重新启动验证。")
				return
			}
		}
	}()
}

// exitWithMsg 在 UI 线程弹框后关闭主窗退出进程。
func exitWithMsg(a *App, msg string) {
	a.uiSafe(func() {
		walk.MsgBox(a.mw, "DNF90 运维工具", msg, walk.MsgBoxIconWarning)
		a.mw.Close()
	})
}

// ---------------- 授权信息页 ----------------

// licenseTabWidgets 授权信息页控件句柄（页面可能尚未创建，全部做 nil 保护）。
type licenseTabWidgets struct {
	statusL   *walk.Label
	cdkL      *walk.Label
	kindL     *walk.Label
	expireL   *walk.Label
	remainL   *walk.Label
	activeL   *walk.Label
	verifyL   *walk.Label
	serverL   *walk.Label
	changeBtn *walk.PushButton
	checkBtn  *walk.PushButton
}

var licWidgets licenseTabWidgets

// licenseTab 授权信息页（仅赞助版）：展示卡类型/到期/剩余与最后校验结果，
// 并提供「更换授权码」与「立即校验」（换码后无需重启，心跳自动切到新码）。
func (a *App) licenseTab() []Widget {
	return []Widget{
		Composite{
			Layout: Grid{Columns: 2, Margins: Margins{10, 12, 10, 6}, Spacing: 8},
			Children: []Widget{
				Label{Text: "授权状态:"},
				Label{AssignTo: &licWidgets.statusL, Text: "-", TextColor: walk.RGB(32, 100, 32)},
				Label{Text: "授权码:"},
				Label{AssignTo: &licWidgets.cdkL, Text: "-"},
				Label{Text: "卡类型:"},
				Label{AssignTo: &licWidgets.kindL, Text: "-"},
				Label{Text: "到期时间:"},
				Label{AssignTo: &licWidgets.expireL, Text: "-"},
				Label{Text: "剩余天数:"},
				Label{AssignTo: &licWidgets.remainL, Text: "-"},
				Label{Text: "首次激活:"},
				Label{AssignTo: &licWidgets.activeL, Text: "-"},
				Label{Text: "最后校验:"},
				Label{AssignTo: &licWidgets.verifyL, Text: "-"},
				Label{Text: "服务器时间:"},
				Label{AssignTo: &licWidgets.serverL, Text: "-"},
			},
		},
		Composite{
			Layout: HBox{Margins: Margins{10, 0, 10, 8}, Spacing: 8},
			Children: []Widget{
				PushButton{AssignTo: &licWidgets.changeBtn, Text: "更换授权码", OnClicked: a.onChangeLicense},
				PushButton{AssignTo: &licWidgets.checkBtn, Text: "立即校验", OnClicked: a.onCheckLicense},
			},
		},
		Composite{
			Layout: VBox{Margins: Margins{10, 0, 10, 8}, Spacing: 4},
			Children: []Widget{
				Label{Text: "说明：", TextColor: walk.RGB(90, 90, 90)},
				Label{Text: "· 有效期从客户【首次验证】起算，中途重启 / 换绑 / 重装都不会重置计时。", TextColor: walk.RGB(90, 90, 90)},
				Label{Text: "· 到期后工具立即停止服务；此时可在弹出框里重试原卡（管理员已续期）或粘贴新卡。", TextColor: walk.RGB(90, 90, 90)},
				Label{Text: "· 本页显示的是最近一次校验结果，最终裁决始终以授权服务器为准。", TextColor: walk.RGB(90, 90, 90)},
			},
		},
	}
}

// refreshLicenseUI 刷新授权信息页（控件未创建时静默跳过；由心跳与按钮触发）。
func refreshLicenseUI(a *App) {
	if licWidgets.statusL == nil && licWidgets.cdkL == nil {
		return // 页面从未创建
	}
	cdk, _, out, has, lastOK, lastErr := lic.snapshot()
	a.uiSafe(func() {
		if licWidgets.cdkL != nil {
			licWidgets.cdkL.SetText(orDefault(maskCDK(cdk), "-"))
		}
		if !has {
			if licWidgets.statusL != nil {
				licWidgets.statusL.SetTextColor(walk.RGB(200, 30, 30))
				licWidgets.statusL.SetText("未获取到授权信息")
			}
			return
		}
		forever := isForeverOutcome(out)
		if licWidgets.kindL != nil {
			licWidgets.kindL.SetText(orDefault(out.KindLabel, out.Kind))
		}
		if licWidgets.expireL != nil {
			licWidgets.expireL.SetText(orDefault(out.ExpireText, "永久"))
		}
		if licWidgets.remainL != nil {
			if forever {
				licWidgets.remainL.SetText("长期有效")
			} else {
				licWidgets.remainL.SetText(fmt.Sprintf("%d 天", out.RemainDays))
			}
		}
		if licWidgets.activeL != nil {
			licWidgets.activeL.SetText(orDefault(shortTime(out.FirstActivatedAt), "-"))
		}
		if licWidgets.serverL != nil {
			licWidgets.serverL.SetText(orDefault(shortTime(out.ServerTime), "-"))
		}
		if licWidgets.verifyL != nil {
			txt := "-"
			if !lastOK.IsZero() {
				txt = lastOK.Format("2006-01-02 15:04:05")
			}
			if lastErr != "" {
				txt += "（最近异常：" + lastErr + "）"
			}
			licWidgets.verifyL.SetText(txt)
		}
		if licWidgets.statusL != nil {
			switch {
			case lastErr != "":
				licWidgets.statusL.SetTextColor(walk.RGB(200, 30, 30))
				licWidgets.statusL.SetText("异常：" + lastErr)
			case forever:
				licWidgets.statusL.SetTextColor(walk.RGB(32, 100, 32))
				licWidgets.statusL.SetText("正常（长期有效）")
			case out.RemainDays <= licenseWarnDays:
				licWidgets.statusL.SetTextColor(walk.RGB(200, 120, 0))
				licWidgets.statusL.SetText(fmt.Sprintf("即将到期：剩余 %d 天", out.RemainDays))
			default:
				licWidgets.statusL.SetTextColor(walk.RGB(32, 100, 32))
				licWidgets.statusL.SetText(fmt.Sprintf("正常：剩余 %d 天", out.RemainDays))
			}
		}
	})
}

// onChangeLicense 运行中更换授权码：验证新码成功后立即落盘，心跳下一个周期自动切到新码。
func (a *App) onChangeLicense() {
	cdk, token, ok, err := promptCDKDialog(a, cdkDialogOpts{
		Title:  "更换授权码",
		Hint:   "请输入新的授权 CDK（会覆盖当前授权码并立即生效，无需重启工具）",
		OKText: "更换并生效",
	})
	if err != nil {
		walk.MsgBox(a.mw, "更换授权码", "打开对话框失败: "+err.Error(), walk.MsgBoxIconError)
		return
	}
	if !ok {
		a.logf("已取消更换授权码")
		return
	}
	// promptCDKDialog 内部已写入 lic 运行态（含新 token），此处只需落盘 + 刷新展示。
	if _, _, out, has, _, _ := lic.snapshot(); has {
		persistLicense(a, cdk, out)
		logLicenseInfo(a, out)
		refreshLicenseUI(a)
		a.logf("授权码已更换并生效，CDK %s", maskCDK(cdk))
		walk.MsgBox(a.mw, "更换授权码", "授权码已更新并立即生效。\n"+describeOutcome(out), walk.MsgBoxIconInformation)
	}
	_ = token
}

// onCheckLicense 立即向授权服务器重新校验一次并刷新展示。
func (a *App) onCheckLicense() {
	cdk, _, _, _, _, _ := lic.snapshot()
	cdk = strings.TrimSpace(cdk)
	if cdk == "" {
		cdk = strings.TrimSpace(a.cfg.License.CDK)
	}
	if cdk == "" {
		walk.MsgBox(a.mw, "立即校验", "当前没有已保存的授权码，请先点「更换授权码」。", walk.MsgBoxIconWarning)
		return
	}
	if licWidgets.checkBtn != nil {
		licWidgets.checkBtn.SetEnabled(false)
	}
	a.runAsync("校验授权", func() error {
		out, code, msg, err := authVerify(cdk)
		if err != nil {
			reason := msg
			if reason == "" {
				reason = err.Error()
			}
			lic.markErr(reason)
			a.logf("授权校验失败(%s): %s", code, reason)
			refreshLicenseUI(a)
			return nil
		}
		lic.setOutcome(cdk, out.Token, out)
		persistLicense(a, cdk, out)
		logLicenseInfo(a, out)
		a.logf("授权校验通过，%s", describeOutcome(out))
		refreshLicenseUI(a)
		return nil
	}, func(err error) {
		// finish 回调在主线程执行
		if licWidgets.checkBtn != nil {
			licWidgets.checkBtn.SetEnabled(true)
		}
		if err != nil {
			a.logf("授权校验异常: %v", err)
		}
	})
}

// shortTime RFC3339 → 本地可读时间（空/零值返回空串）。
func shortTime(s string) string {
	s = strings.TrimSpace(s)
	if s == "" {
		return ""
	}
	t, err := time.Parse(time.RFC3339, s)
	if err != nil {
		return s
	}
	if t.IsZero() || t.Year() <= 1 {
		return ""
	}
	return t.Local().Format("2006-01-02 15:04")
}

// ---------------- 网络 ----------------

// authVerify 调 /api/verify；成功返回授权信息。
func authVerify(cdk string) (verifyOutcome, string, string, error) {
	var r authResp
	if err := authPost("/api/verify", map[string]any{"cdk": cdk, "proto": clientProto}, &r); err != nil {
		return verifyOutcome{}, "", "", err
	}
	if !r.Ok {
		return verifyOutcome{}, r.Code, r.Msg, fmt.Errorf("授权验证未通过(%s)", r.Code)
	}
	if r.Data == nil || r.Data.Token == "" {
		return verifyOutcome{}, "", "", errors.New("授权服务器响应缺少 token")
	}
	return verifyOutcome{
		Token:            r.Data.Token,
		Kind:             r.Data.Kind,
		KindLabel:        r.Data.KindLabel,
		ExpireAt:         r.Data.ExpireAt,
		ExpireText:       r.Data.ExpireText,
		RemainDays:       r.Data.RemainDays,
		FirstActivatedAt: r.Data.FirstActivatedAt,
		ServerTime:       r.Data.ServerTime,
	}, "", "", nil
}

// authHeartbeat 调 /api/heartbeat；成功返回 ("","",nil)。
func authHeartbeat(cdk, token string) (code, msg string, err error) {
	var r authResp
	if err := authPost("/api/heartbeat", map[string]any{"cdk": cdk, "token": token, "proto": clientProto}, &r); err != nil {
		return "", "", err
	}
	if !r.Ok {
		return r.Code, r.Msg, fmt.Errorf("心跳被拒绝(%s)", r.Code)
	}
	return "", "", nil
}

// authHTTP 固定服务器自签证书：InsecureSkipVerify 只跳过系统链与 hostname，
// 最终安全性由 VerifyPeerCertificate 的 DER SHA256 指纹校验保证。
var authHTTP = &http.Client{
	Timeout: authTimeout,
	Transport: &http.Transport{
		TLSClientConfig: &tls.Config{
			InsecureSkipVerify: true, // 证书合法性由下方指纹校验承担
			VerifyPeerCertificate: func(rawCerts [][]byte, _ [][]*x509.Certificate) error {
				for _, raw := range rawCerts {
					c, perr := x509.ParseCertificate(raw)
					if perr != nil {
						continue
					}
					sum := sha256.Sum256(c.Raw)
					if hex.EncodeToString(sum[:]) == authCertDERHash {
						return nil
					}
				}
				return errors.New("授权服务器证书指纹不匹配")
			},
		},
	},
}

func authPost(path string, payload any, out any) error {
	raw, err := json.Marshal(payload)
	if err != nil {
		return err
	}
	resp, err := authHTTP.Post(authServerURL+path, "application/json", bytes.NewReader(raw))
	if err != nil {
		return fmt.Errorf("无法连接授权服务器: %v", err)
	}
	defer resp.Body.Close()
	body, err := io.ReadAll(resp.Body)
	if err != nil {
		return fmt.Errorf("读取授权服务器响应失败: %v", err)
	}
	if err := json.Unmarshal(body, out); err != nil {
		return fmt.Errorf("授权服务器响应异常: %v", err)
	}
	return nil
}

// maskCDK 脱敏展示 CDK，便于日志排查。
func maskCDK(cdk string) string {
	cdk = strings.TrimSpace(cdk)
	if cdk == "" {
		return ""
	}
	if len(cdk) <= 8 {
		return "****"
	}
	return cdk[:4] + "****" + cdk[len(cdk)-4:]
}
