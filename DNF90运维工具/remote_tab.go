//go:build paid

// 远程管理页（仅赞助版编译）：服务器端 P0+P1 的 GUI 配套。
//   - 远程管理开关（默认关）：开启时生成盐 + 配对码哈希并落 config.json；
//   - 展示当前配对码明文 + 复制；重新生成 16 位去易混码；自定义修改（≥8 位）；
//   - 关闭 / 重新生成 / 修改后旧管理 token 全量作废（Auth.SetPairing / RevokeAll）；
//   - 页面同时展示管理 API 监听地址与证书状态；
//   - main.go 在 verifyLicenseGate 通过后调用 maybeStartManage()：manage.enabled=true
//     且已设置配对码时后台拉起 HTTPS 管理服务（internal/manage）。
//   - 业务端点（bans / redeem-codes / server start-stop）通过 manage.Bridge 复用
//     本文件既有 GUI/底层实现，不在 manage 包内重复业务逻辑。
package main

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"time"

	"github.com/lxn/walk"
	. "github.com/lxn/walk/declarative"

	"dnf90ops/internal/config"
	"dnf90ops/internal/manage"
	"dnf90ops/internal/serverdir"
	"dnf90ops/internal/store"
)

// 远程管理服务运行时状态（仅 paid 版存在）。
var (
	manageMu        sync.Mutex
	manageSvc       *manage.Server
	manageRunCtx    context.Context
	manageRunCancel context.CancelFunc
)

// mainPages 赞助版：基础四页 + 账号管理 + 背包编辑 + 远程管理页。
// 账号管理页走免费版同源实现 accountTab()（无 paid tag），两端功能一致。
func (a *App) mainPages() []TabPage {
	return []TabPage{
		{Title: "一键启动", Content: Composite{Layout: VBox{MarginsZero: true, SpacingZero: true}, Children: a.serviceTab()}},
		{Title: "认证服务", Content: Composite{Layout: VBox{MarginsZero: true, SpacingZero: true}, Children: a.authTab()}},
		{Title: "封禁管理", Content: Composite{Layout: VBox{MarginsZero: true, SpacingZero: true}, Children: a.banTab()}},
		{Title: "账号管理", Content: Composite{Layout: VBox{MarginsZero: true, SpacingZero: true}, Children: a.accountTab()}},
		{Title: "背包编辑", Content: Composite{Layout: VBox{MarginsZero: true, SpacingZero: true}, Children: a.inventoryTab()}},
		{Title: "频道管理", Content: Composite{Layout: VBox{MarginsZero: true, SpacingZero: true}, Children: a.channelTab()}},
		{Title: "CDK管理", Content: Composite{Layout: VBox{MarginsZero: true, SpacingZero: true}, Children: a.cdkTab()}},
		{Title: "远程管理", Content: Composite{Layout: VBox{MarginsZero: true, SpacingZero: true}, Children: a.remoteTab()}},
		{Title: "数据库备份", Content: Composite{Layout: VBox{MarginsZero: true, SpacingZero: true}, Children: a.backupTab()}},
		{Title: "运行维护", Content: Composite{Layout: VBox{MarginsZero: true, SpacingZero: true}, Children: a.maintTab()}},
		{Title: "授权信息", Content: Composite{Layout: VBox{MarginsZero: true, SpacingZero: true}, Children: a.licenseTab()}},
	}
}

// maybeStartManage 赞助版：授权门通过后，若 manage.enabled 且已设置配对码则后台拉起管理服务。
func (a *App) maybeStartManage() {
	// 泡点 Runner 与远程管理服务同生命周期：启动即拉起常驻循环（幂等），由配置开关控制发放。
	a.startPointsinkRunner()
	m := a.cfg.Manage
	if m == nil {
		return
	}
	if m.Enabled && strings.TrimSpace(m.PairingCodeHash) != "" && strings.TrimSpace(m.PairingCodeSalt) != "" {
		if err := a.ensureManageStarted(); err != nil {
			a.logf("后台管理服务启动失败: %v", err)
			walk.MsgBox(a.mw, "远程管理", "后台管理服务启动失败:\n"+err.Error(), walk.MsgBoxIconError)
		} else {
			a.logf("后台管理服务已启动，监听 %s", m.ListenAddr)
		}
	} else if m.Enabled {
		a.logf("config.manage.enabled=true 但未设置配对码，请在「远程管理」页开启并复制配对码")
	}
	a.updateManageUI()
}

// remoteTab 远程管理页 UI。
func (a *App) remoteTab() []Widget {
	return []Widget{
		Composite{
			Layout: VBox{Margins: Margins{12, 10, 12, 10}, Spacing: 8},
			Children: []Widget{
				GroupBox{
					Title:  "远程管理总开关",
					Layout: VBox{Margins: Margins{10, 8, 10, 8}, Spacing: 6},
					Children: []Widget{
						Composite{
							Layout: HBox{MarginsZero: true, Spacing: 8},
							Children: []Widget{
								CheckBox{
									AssignTo:         &a.manageChk,
									Text:             "启用后台管理服务（HTTPS）",
									OnCheckedChanged: a.onManageToggle,
								},
								Label{
									Text:      "默认关闭。开启后工具将监听 18113 端口提供 /manage/v1/* API（需配对码登录），请配合家用电脑端客户端使用。",
									TextColor: walk.RGB(90, 90, 90),
									MinSize:   Size{Width: 420, Height: 0},
								},
							},
						},
						Label{
							AssignTo:  &a.manageHintL,
							Text:      "提示：开启时会自动生成 16 位配对码；请立即复制保存，客户端登录后凭此配对码换取短时管理 token。",
							TextColor: walk.RGB(120, 90, 0),
						},
					},
				},
				GroupBox{
					Title:  "服务状态",
					Layout: VBox{Margins: Margins{10, 8, 10, 8}, Spacing: 6},
					Children: []Widget{
						Label{AssignTo: &a.manageListenL, Text: "监听地址：-"},
						Label{AssignTo: &a.manageCertL, Text: "证书状态：-"},
					},
				},
				GroupBox{
					Title:  "配对码管理",
					Layout: VBox{Margins: Margins{10, 8, 10, 8}, Spacing: 8},
					Children: []Widget{
						Composite{
							Layout: HBox{MarginsZero: true, Spacing: 8},
							Children: []Widget{
								LineEdit{
									AssignTo: &a.manageCodeBox,
									ReadOnly: true,
									MinSize:  Size{Width: 330, Height: 0},
									Font:     Font{Family: "Consolas", PointSize: 11},
								},
								PushButton{Text: "复制配对码", OnClicked: a.onManageCopy},
								PushButton{Text: "重新生成（16位）", OnClicked: a.onManageRegen},
							},
						},
						Composite{
							Layout: HBox{MarginsZero: true, Spacing: 8},
							Children: []Widget{
								LineEdit{
									AssignTo: &a.manageCustomEdit,
									MinSize:  Size{Width: 330, Height: 0},
								},
								PushButton{Text: "应用自定义配对码（≥8位）", OnClicked: a.onManageApplyCustom},
							},
						},
						Label{
							Text:      "配对码仅存储 sha256(salt+code) 哈希用于鉴权；明文仅在本页展示并同步写入 config.json（本地）。修改配对码后已签发的管理 token 将全部失效。",
							TextColor: walk.RGB(90, 90, 90),
						},
					},
				},
			},
		},
	}
}

// ---------- GUI 事件 ----------

func (a *App) onManageToggle() {
	if a.manageChk.Checked() {
		if walk.MsgBox(a.mw, "远程管理", "开启后台管理服务将监听 "+a.cfg.Manage.ListenAddr+"（HTTPS），并自动生成 16 位配对码。\n请确保仅在可信内网/公网部署并妥善保存配对码。是否继续？", walk.MsgBoxYesNo|walk.MsgBoxIconQuestion) != walk.DlgCmdYes {
			a.manageChk.SetChecked(false)
			return
		}
		a.manageEnable()
	} else {
		if walk.MsgBox(a.mw, "远程管理", "确定关闭后台管理服务？\n已签发的管理 token 将全部作废，配对码将被清除（下次开启会生成新码）。", walk.MsgBoxYesNo|walk.MsgBoxIconWarning) != walk.DlgCmdYes {
			a.manageChk.SetChecked(true)
			return
		}
		a.manageDisable()
	}
}

func (a *App) manageEnable() {
	m := a.cfg.Manage
	if m == nil {
		a.manageChk.SetChecked(false)
		return
	}
	// 开启时才生成盐+配对码；已有可用配对码（明文+哈希匹配）则复用，避免重复变码。
	needPair := strings.TrimSpace(m.PairingCodeHash) == "" ||
		strings.TrimSpace(m.PairingCodePlain) == "" ||
		manage.PairingHash(m.PairingCodeSalt, m.PairingCodePlain) != m.PairingCodeHash
	if needPair {
		if err := a.manageSetupPairing(); err != nil {
			walk.MsgBox(a.mw, "远程管理", "生成配对码失败: "+err.Error(), walk.MsgBoxIconError)
			a.manageChk.SetChecked(false)
			return
		}
	}
	m.Enabled = true
	if err := a.cfg.Save(); err != nil {
		walk.MsgBox(a.mw, "远程管理", "保存配置失败: "+err.Error(), walk.MsgBoxIconError)
		a.manageChk.SetChecked(false)
		return
	}
	if err := a.ensureManageStarted(); err != nil {
		m.Enabled = false
		_ = a.cfg.Save()
		a.manageChk.SetChecked(false)
		walk.MsgBox(a.mw, "远程管理", "后台管理服务启动失败:\n"+err.Error(), walk.MsgBoxIconError)
		return
	}
	a.updateManageUI()
	a.logf("后台管理服务已开启（监听 %s），配对码已生成/复用，请妥善保存", m.ListenAddr)
}

func (a *App) manageDisable() {
	m := a.cfg.Manage
	if m == nil {
		return
	}
	m.Enabled = false
	// 关闭远程管理：清除配对码与盐，旧 token 随服务停止与 SetPairing 全量作废。
	m.PairingCodeSalt = ""
	m.PairingCodeHash = ""
	m.PairingCodePlain = ""
	if err := a.cfg.Save(); err != nil {
		walk.MsgBox(a.mw, "远程管理", "保存配置失败: "+err.Error(), walk.MsgBoxIconError)
		return
	}
	a.manageStopService()
	a.manageChk.SetChecked(false)
	a.updateManageUI()
	a.logf("后台管理服务已关闭")
}

// manageSetupPairing 随机盐 + 16 位配对码，写入 cfg.Manage（未落盘，由调用方 Save）。
func (a *App) manageSetupPairing() error {
	m := a.cfg.Manage
	if m == nil {
		return errors.New("manage 配置段缺失")
	}
	salt, err := manage.GenSalt()
	if err != nil {
		return err
	}
	code, err := manage.GenPairingCode(16)
	if err != nil {
		return err
	}
	m.PairingCodeSalt = salt
	m.PairingCodeHash = manage.PairingHash(salt, code)
	m.PairingCodePlain = code
	// 服务运行中立即同步内存认证状态（变更后旧 token 全量作废）。
	if manageSvc != nil {
		manageSvc.Auth().SetPairing(salt, m.PairingCodeHash)
	}
	return nil
}

func (a *App) onManageRegen() {
	if walk.MsgBox(a.mw, "重新生成配对码", "将生成新的 16 位配对码，旧配对码立即失效，已签发的管理 token 全部作废。\n是否继续？", walk.MsgBoxYesNo|walk.MsgBoxIconWarning) != walk.DlgCmdYes {
		return
	}
	if err := a.manageSetupPairing(); err != nil {
		walk.MsgBox(a.mw, "重新生成配对码", "生成失败: "+err.Error(), walk.MsgBoxIconError)
		return
	}
	if err := a.cfg.Save(); err != nil {
		walk.MsgBox(a.mw, "重新生成配对码", "保存配置失败: "+err.Error(), walk.MsgBoxIconError)
		return
	}
	a.updateManageUI()
	a.logf("配对码已重新生成，旧码与旧 token 全部失效")
}

func (a *App) onManageApplyCustom() {
	raw := strings.TrimSpace(a.manageCustomEdit.Text())
	if len(raw) < 8 {
		walk.MsgBox(a.mw, "自定义配对码", "配对码长度不能少于 8 位（当前 "+fmt.Sprint(len(raw))+" 位）", walk.MsgBoxIconWarning)
		return
	}
	if len(raw) > 64 {
		walk.MsgBox(a.mw, "自定义配对码", "配对码长度不能超过 64 位", walk.MsgBoxIconWarning)
		return
	}
	norm := strings.ToUpper(raw)
	m := a.cfg.Manage
	if m == nil {
		return
	}
	salt, err := manage.GenSalt()
	if err != nil {
		walk.MsgBox(a.mw, "自定义配对码", "生成盐失败: "+err.Error(), walk.MsgBoxIconError)
		return
	}
	m.PairingCodeSalt = salt
	m.PairingCodeHash = manage.PairingHash(salt, norm)
	m.PairingCodePlain = norm
	if manageSvc != nil {
		manageSvc.Auth().SetPairing(salt, m.PairingCodeHash)
	}
	if err := a.cfg.Save(); err != nil {
		walk.MsgBox(a.mw, "自定义配对码", "保存配置失败: "+err.Error(), walk.MsgBoxIconError)
		return
	}
	a.manageCustomEdit.SetText("")
	a.updateManageUI()
	a.logf("已应用自定义配对码，旧 token 全部作废")
}

func (a *App) onManageCopy() {
	code := strings.TrimSpace(a.manageCodeBox.Text())
	if code == "" || code == "（未设置）" {
		walk.MsgBox(a.mw, "复制配对码", "当前没有可复制的配对码，请先开启远程管理或重新生成", walk.MsgBoxIconWarning)
		return
	}
	clip := walk.Clipboard()
	if err := clip.SetText(code); err != nil {
		walk.MsgBox(a.mw, "复制配对码", "写入剪贴板失败: "+err.Error(), walk.MsgBoxIconError)
		return
	}
	a.logf("配对码已复制到剪贴板")
}

// ---------- 服务启停 ----------

// ensureManageStarted 启动（或复用）后台管理 HTTPS 服务；幂等。
func (a *App) ensureManageStarted() error {
	manageMu.Lock()
	defer manageMu.Unlock()
	if manageSvc != nil {
		return nil
	}
	m := a.cfg.Manage
	if m == nil || !m.Enabled || strings.TrimSpace(m.PairingCodeSalt) == "" || strings.TrimSpace(m.PairingCodeHash) == "" {
		return errors.New("manage 未启用或未设置配对码")
	}

	srv, err := manage.NewServer(a.cfg, a.manageBridge(), slog.Default())
	if err != nil {
		return err
	}
	ctx, cancel := context.WithCancel(context.Background())
	if err := srv.Start(ctx, nil); err != nil {
		cancel()
		return err
	}
	manageSvc = srv
	manageRunCtx = ctx
	manageRunCancel = cancel
	return nil
}

// manageStopService 停止管理服务并释放状态（幂等）。
func (a *App) manageStopService() {
	manageMu.Lock()
	defer manageMu.Unlock()
	if manageSvc != nil {
		manageSvc.Close()
		if manageRunCancel != nil {
			manageRunCancel()
		}
		manageSvc = nil
		manageRunCancel = nil
	}
}

// updateManageUI 依据 cfg 刷新远程管理页展示（须在 UI 线程调用）。
func (a *App) updateManageUI() {
	m := a.cfg.Manage
	if m == nil || a.manageChk == nil {
		return
	}
	a.manageChk.SetChecked(m.Enabled)
	if a.manageCodeBox != nil {
		if strings.TrimSpace(m.PairingCodePlain) != "" {
			a.manageCodeBox.SetText(m.PairingCodePlain)
		} else {
			a.manageCodeBox.SetText("（未设置）")
		}
	}
	// 服务状态文本。
	running := manageSvc != nil
	state := "未运行"
	if running {
		state = "运行中"
	}
	if a.manageListenL != nil {
		if m.Enabled && running {
			a.manageListenL.SetText(fmt.Sprintf("监听地址：%s（HTTPS，%s）", m.ListenAddr, state))
		} else if m.Enabled {
			a.manageListenL.SetText(fmt.Sprintf("监听地址：%s（HTTPS，已配置未运行）", m.ListenAddr))
		} else {
			a.manageListenL.SetText(fmt.Sprintf("监听地址：%s（HTTPS，未启用）", m.ListenAddr))
		}
	}
	if a.manageCertL != nil {
		cert := strings.TrimSpace(m.TLSCert)
		key := strings.TrimSpace(m.TLSKey)
		exists := cert != "" && key != ""
		if exists {
			cAbs, _ := filepath.Abs(cert)
			kAbs, _ := filepath.Abs(key)
			_, cerr := os.Stat(cAbs)
			_, kerr := os.Stat(kAbs)
			st := "已存在"
			if cerr != nil || kerr != nil {
				st = "不存在（服务启动时自动生成，SAN 含公网 IP）"
			}
			a.manageCertL.SetText(fmt.Sprintf("证书状态：%s / %s（%s）", cAbs, kAbs, st))
		} else {
			a.manageCertL.SetText("证书状态：未配置路径")
		}
	}
}

// ---------- 复用桥 ----------

// manageBridge 组装 internal/manage 所需桥接；业务逻辑全部复用现有实现。
func (a *App) manageBridge() *manage.Bridge {
	return &manage.Bridge{
		ServerRoot: func() string {
			// 直接读控件，**不能**用 a.uiSafe 包一层：walk 的 WindowBase.Synchronize
			// （window.go Synchronize → WindowGroup.Synchronize）只是把闭包排进队列并
			// PostMessage，是纯异步投递，闭包在读到 root 之后才被执行，结果永远是零值。
			// 这条路径上的后果是：管理端拿到的一直是 cfg.ServerRoot，界面上刚改的
			// 服务端目录不生效。LineEdit.Text 本身是同步 Win32 调用，跨线程读会被编组
			// 到 UI 线程并阻塞到处理完——与 main.refreshNow / channel_admin.currentServerRoot
			// 对同一控件的读法一致，代价可忽略。
			root := ""
			if a.rootEdit != nil {
				root = strings.TrimSpace(a.rootEdit.Text())
			}
			if root == "" {
				root = strings.TrimSpace(a.cfg.ServerRoot)
			}
			return root
		},
		PublicIP: func() string {
			return strings.TrimSpace(a.cfg.PublicIP)
		},
		StartServer: func(ctx context.Context) error {
			return a.manageServerStart(ctx)
		},
		StopServer: func(ctx context.Context) error {
			return a.manageServerStop(ctx)
		},
		AuthRunning: func() bool {
			return a.svcOn.Load()
		},
		Inspect: func(root string) *serverdir.Inspection {
			if strings.TrimSpace(root) == "" {
				return &serverdir.Inspection{Message: "服务端根目录为空"}
			}
			return serverdir.Inspect(root)
		},
		// AC 账号管理（阶段1）：GM 直连封装（account_gm.go）；审计由 handlers_account.go 统一落 manage_account.jsonl。
		AcctGMCall: func(ctx context.Context, method, path string, body, out any, actor string) (string, error) {
			return a.acctGMDo(ctx, method, path, body, out, actor, "")
		},
		// GM Tool 通道（/dnf/gm/*）：背包/仓库查询与删除、邮箱列表与定向删除都只在这条通道上，
		// 与 AcctGMCall 共用同一回环 adminListen+adminToken，但信封与鉴权头不同（见 account_gmt.go）。
		AcctGMTCall: func(ctx context.Context, method, path, operation string, body, out any, actor string) error {
			baseURL, token, err := a.acctGMConfig()
			if err != nil {
				return err
			}
			return acctGMTCallRaw(ctx, baseURL, token, method, path, operation, body, out, actor)
		},
		AcctDungeons: func() (map[string]int, error) {
			return acctDungeonsMaxDifficulty(), nil
		},
		AcctAudit: func(ctx context.Context, actor, method, path string, detail map[string]any) {
			a.acctAuditAppend("manage_account.jsonl", detail)
		},
		// 频道管理（落盘编辑 channel_info.etc）；远程管理工具只发协议，实现全在 internal/channelstore。
		ChannelsStatus: func(ctx context.Context) (manage.ChannelStatus, error) {
			return a.channelsStatus()
		},
		ChannelsApply: func(ctx context.Context, channelIDs []int, enabled bool) ([]int, manage.ChannelStatus, error) {
			return a.channelsApply(channelIDs, enabled)
		},
		// 频道热生效：转发到 DNF90Server 的 /dnf/gm/channels/reload（不重启进程）。
		ChannelsReload: func(ctx context.Context) (manage.ChannelReloadStatus, error) {
			reloaded, err := a.chanHotReload(ctx)
			if err != nil {
				return manage.ChannelReloadStatus{}, err
			}
			return manage.ChannelReloadStatus{
				Path:             reloaded.Path,
				TotalChannels:    reloaded.TotalChannels,
				AddedChannels:    reloaded.AddedChannels,
				RemovedChannels:  reloaded.RemovedChannels,
				GamePortsOpened:  reloaded.GamePortsOpened,
				GamePortsClosed:  reloaded.GamePortsClosed,
				OnlineCharacters: reloaded.OnlineCharacters,
				Warnings:         reloaded.Warnings,
				ReloadedAt:       reloaded.ReloadedAt,
			}, nil
		},
		Store: func() *store.Store {
			if !a.svcOn.Load() || a.st == nil {
				return nil
			}
			return a.st
		},
		// 批量任务的目标来源（群发物品 / 批量资产邮件 / 批量清空邮箱）：注入的就是
		// 认证服务连接池上的同名只读查询，与 manage 侧未注入时的退回路径完全同源。
		AcctOnlineAccounts: func(ctx context.Context, limit int) ([]store.OnlineAccount, error) {
			st := a.manageStore()
			if st == nil {
				return nil, errors.New("认证服务未运行")
			}
			return st.ListOnlineAccounts(ctx, limit)
		},
		AcctAccountsPage: func(ctx context.Context, offset, limit int) ([]store.Account, error) {
			st := a.manageStore()
			if st == nil {
				return nil, errors.New("认证服务未运行")
			}
			return st.ListAccountsPage(ctx, offset, limit)
		},
		AcctServerFirstCharacter: func(ctx context.Context) (store.ServerFirstCharacter, bool, error) {
			st := a.manageStore()
			if st == nil {
				return store.ServerFirstCharacter{}, false, errors.New("认证服务未运行")
			}
			return st.ServerFirstCharacter(ctx)
		},
		OnlineCount: func() int {
			if !a.svcOn.Load() || a.st == nil {
				return 0
			}
			recs, err := a.st.ListOnlineAccounts(context.Background(), 0)
			if err != nil {
				return 0
			}
			return len(recs)
		},
		BanNow: func(ctx context.Context, banType, banValue, username, reason string, days int) (manage.BanNowResult, error) {
			return a.manageBanNow(ctx, banType, banValue, username, reason, days)
		},
		BanKick: func(ctx context.Context, banType, banValue string) (int, error) {
			banType = strings.ToLower(strings.TrimSpace(banType))
			banValue = strings.TrimSpace(banValue)
			// account 维度：用户常填用户名，而游戏会话 accountID 存的是数字 account_id，
			// 需先解析成 account_id（与封禁流程 manageBanNow 一致）；解析不到则视为已填数字 ID，保留原值。
			accountID := ""
			if banType == store.BanAccount && banValue != "" {
				if st := a.manageStore(); st != nil {
					if acc, err := st.GetAccountByUsername(ctx, banValue); err == nil {
						accountID = acc.AccountID
						banValue = acc.AccountID
					} else {
						accountID = banValue
					}
				}
			}
			kicked, err := a.kickByAdmin(banType, banValue)
			// 断开游戏会话后写 GM kick 事件：登录器轮询到即结束对应账号的游戏进程（联动闭环）。
			// ip/machine 维度无法精确映射到账号，不写事件。
			if err == nil && accountID != "" {
				if st := a.manageStore(); st != nil {
					if _, eerr := st.CreateGMKickEvent(ctx, accountID, "kick", "踢下线", "ops"); eerr != nil {
						slog.Warn("kick gm-event write failed", "account_id", accountID, "error", eerr)
					}
				}
			}
			return kicked, err
		},
		RedeemCreate: func(ctx context.Context, name string, count, maxRedeems int, contentRaw, rewardJSON, remark, createdBy string) ([]string, error) {
			st := a.manageStore()
			if st == nil {
				return nil, errors.New("认证服务未运行，无法生成 CDK")
			}
			return st.CreateRedeemCodeBatch(ctx, count, name, contentRaw, maxRedeems, rewardJSON, remark, createdBy)
		},
		RedeemExported: func(ctx context.Context, name string, codes []string) {
			a.manageExportRedeemTxt(name, codes)
		},
		RetryRedeemMails: func(ctx context.Context, code string) (int, int, error) {
			return a.manageRetryRedeemMails(ctx, code)
		},
		PublishAnnounce: func(ctx context.Context, content, color string) (int64, error) {
			// 统一走 publishAnnouncementID：写库 + 广播给等待中的登录器长轮询
			// （与定时公告、在线福利公告同一条路径，避免两套实现走偏）。
			return a.publishAnnouncementID(ctx, content, color, "admin")
		},
		PointsinkStatus: func() manage.PointsinkStatus {
			return a.psBridgeStatus(a.psStatus())
		},
		PointsinkApply: func(ctx context.Context, enabled bool, intervalSeconds, points, dailyLimit int, currency string) (manage.PointsinkStatus, error) {
			st, err := a.psApply(ctx, enabled, intervalSeconds, points, dailyLimit, currency)
			return a.psBridgeStatus(st), err
		},
		RegisterBonusStatus: func() manage.RegisterBonusStatus {
			return a.rbBridgeStatus(a.rbStatus())
		},
		RegisterBonusApply: func(ctx context.Context, enabled bool, points int, currency string) (manage.RegisterBonusStatus, error) {
			st, err := a.rbApply(ctx, enabled, points, currency)
			return a.rbBridgeStatus(st), err
		},
		// 注册限制（关闭自助注册 + 每 IP 注册数量上限 + 是否需要注册码）+ 管理员建号/改密/强制下线
		// （管理工具服务器选项页闭环）。
		RegistrationClosedStatus: func() manage.RegistrationClosedStatus {
			st := a.regStatus()
			return manage.RegistrationClosedStatus{Closed: st.Closed, MaxPerIP: st.MaxPerIP, CodeRequired: st.CodeRequired}
		},
		RegistrationClosedApply: func(ctx context.Context, closed bool, maxPerIP int, codeRequired bool) (manage.RegistrationClosedStatus, error) {
			st, err := a.regApply(ctx, closed, maxPerIP, codeRequired)
			return manage.RegistrationClosedStatus{Closed: st.Closed, MaxPerIP: st.MaxPerIP, CodeRequired: st.CodeRequired}, err
		},
		// 注册码生成 / 列表 / 作废（管理工具「限制注册」页）。
		RegistrationCodeCreate: func(ctx context.Context, count int) ([]string, error) {
			st := a.manageStore()
			if st == nil {
				return nil, errors.New("认证服务未运行，无法生成注册码")
			}
			codes, err := st.CreateInvites(ctx, count)
			if err == nil {
				a.logf("已生成注册码 %d 枚", len(codes))
			}
			return codes, err
		},
		RegistrationCodeList: func(ctx context.Context, state string, limit int) ([]manage.RegistrationCode, error) {
			st := a.manageStore()
			if st == nil {
				return nil, errors.New("认证服务未运行，无法查询注册码")
			}
			recs, err := st.ListInvites(ctx, state, limit)
			if err != nil {
				return nil, err
			}
			out := make([]manage.RegistrationCode, 0, len(recs))
			for _, r := range recs {
				usedAt := ""
				if r.UsedAt.Valid {
					usedAt = r.UsedAt.Time.Format(time.RFC3339)
				}
				out = append(out, manage.RegistrationCode{
					Code:      r.Code,
					State:     r.State,
					UsedBy:    r.UsedBy,
					UsedAt:    usedAt,
					CreatedAt: r.CreatedAt.Format(time.RFC3339),
				})
			}
			return out, nil
		},
		RegistrationCodeRevoke: func(ctx context.Context, codes []string) (int, error) {
			st := a.manageStore()
			if st == nil {
				return 0, errors.New("认证服务未运行，无法作废注册码")
			}
			n, err := st.RevokeInvites(ctx, codes)
			if err == nil {
				a.logf("已作废注册码 %d 枚", n)
			}
			return n, err
		},
		// 更新维护：保存后 maintApply 内部会广播给所有登录器长轮询。
		MaintenanceStatus: func() manage.MaintenanceStatus {
			st := a.maintStatus()
			return manage.MaintenanceStatus{Enabled: st.Enabled, AllowUsers: st.AllowUsers}
		},
		MaintenanceApply: func(ctx context.Context, enabled bool, allowUsers []string) (manage.MaintenanceStatus, error) {
			st, err := a.maintApply(ctx, enabled, allowUsers)
			return manage.MaintenanceStatus{Enabled: st.Enabled, AllowUsers: st.AllowUsers}, err
		},
		// 登录器版本校验：只读写配置，不需要广播 —— 登录器在自检阶段与点登录时各主动查一次
		// （见 internal/server/launcher_version.go）。RequiredVersion 来自运维工具内置常量，
		// 只读，不接受管理工具下发。
		LauncherVersionStatus: func() manage.LauncherVersionStatus {
			st := a.lvStatus()
			return manage.LauncherVersionStatus{
				Enabled:         st.Enabled,
				DownloadURL:     st.DownloadURL,
				RequiredVersion: st.RequiredVersion,
			}
		},
		LauncherVersionApply: func(ctx context.Context, enabled bool, downloadURL string) (manage.LauncherVersionStatus, error) {
			st, err := a.lvApply(ctx, enabled, downloadURL)
			return manage.LauncherVersionStatus{
				Enabled:         st.Enabled,
				DownloadURL:     st.DownloadURL,
				RequiredVersion: st.RequiredVersion,
			}, err
		},
		// 双开（多开）开关：与版本校验一样只读写配置，不需要广播 —— 登录器在自检阶段
		// 主动查一次（见 internal/server/multi_open.go）。关闭**不碰任何进程**：
		// 已在运行的副账号不会被踢下线（与更新维护故意不同，不要照抄 KillAll）。
		MultiOpenStatus: func() manage.MultiOpenStatus {
			st := a.moStatus()
			return manage.MultiOpenStatus{Enabled: st.Enabled, Configured: st.Configured}
		},
		MultiOpenApply: func(ctx context.Context, enabled bool) (manage.MultiOpenStatus, error) {
			st, err := a.moApply(ctx, enabled)
			return manage.MultiOpenStatus{Enabled: st.Enabled, Configured: st.Configured}, err
		},
		// 在线福利：三档期配置读写 + 立即试发（调度在 App 侧常驻跑，见 welfare.go）。
		WelfareStatus: func() manage.WelfareStatus {
			return a.welfareBridgeStatus()
		},
		WelfareApply: func(ctx context.Context, cfg config.WelfareConfig) (manage.WelfareStatus, error) {
			if _, err := a.welfareApply(ctx, cfg); err != nil {
				return a.welfareBridgeStatus(), err
			}
			a.logf("在线福利配置已由管理工具更新")
			return a.welfareBridgeStatus(), nil
		},
		WelfareApplySlot: func(ctx context.Context, kind string, slot config.WelfareSlot) (manage.WelfareStatus, error) {
			if _, err := a.welfareApplySlot(ctx, kind, slot); err != nil {
				return a.welfareBridgeStatus(), err
			}
			a.logf("在线福利·%s 配置已由管理工具单独保存", kind)
			return a.welfareBridgeStatus(), nil
		},
		WelfareTry: func(ctx context.Context, kind string) (manage.WelfareRunResult, error) {
			res, err := a.welfareTryNow(ctx, kind)
			return manage.WelfareRunResult{
				Accounts:    res.Accounts,
				Characters:  res.Characters,
				ItemMails:   res.ItemMails,
				CeraGranted: res.CeraGranted,
				BlackGrant:  res.BlackGrant,
				Failed:      res.Failed,
				NoticeSent:  res.NoticeSent,
			}, err
		},
		// 定时公告（一次性 / 每天 / 每周，多条排队）：列表 + 整批保存。
		// 调度在 App 侧常驻跑（announce_schedule.go），到点自动发；这里只读写配置。
		AnnounceScheduleStatus: func() []manage.ScheduledAnnouncementStatus {
			return announceScheduleBridge(a.announceScheduleView())
		},
		AnnounceScheduleApply: func(ctx context.Context, items []config.ScheduledAnnouncement) ([]manage.ScheduledAnnouncementStatus, error) {
			view, err := a.announceScheduleApply(ctx, items)
			if err != nil {
				// 校验失败时把当前（未改动的）列表一起回给前端，避免界面停在错误状态。
				return announceScheduleBridge(a.announceScheduleView()), err
			}
			a.logf("定时公告配置已由管理工具更新")
			return announceScheduleBridge(view), nil
		},
		// 登录器「服务器公告」栏（登录页左侧公告板）：整表读写，直接落到 MySQL。
		NoticesList: func(ctx context.Context) ([]store.LauncherNotice, error) {
			st := a.manageStore()
			if st == nil {
				return nil, errors.New("认证服务未运行，无法读取服务器公告")
			}
			return st.ListNotices(ctx, false)
		},
		NoticesReplace: func(ctx context.Context, items []store.LauncherNotice) ([]store.LauncherNotice, error) {
			st := a.manageStore()
			if st == nil {
				return nil, errors.New("认证服务未运行，无法保存服务器公告")
			}
			saved, err := st.ReplaceNotices(ctx, items)
			if err != nil {
				return nil, err
			}
			a.logf("服务器公告栏已由管理工具更新：共 %d 条", len(saved))
			return saved, nil
		},
		// 账号主键 ↔ 用户名的翻译（记录里存的是 dnf:<sha256(用户名)> 主键，管理工具
		// 要显示真实账号名；映射表只在运维工具侧的 MySQL）。记录查询与
		// POST /manage/v1/account/names 共用这一份实现。
		AccountNamesByID: func(ctx context.Context, accountIDs []string) (map[string]string, error) {
			st := a.manageStore()
			if st == nil {
				return nil, errors.New("认证服务未运行，无法翻译账号名")
			}
			return st.AccountNamesByID(ctx, accountIDs)
		},
		ResolveAccountInput: func(ctx context.Context, value string) (string, error) {
			st := a.manageStore()
			if st == nil {
				// 认证服务没起时原样放行：查询照旧按填的值走，不因为翻译不可用而报错。
				return strings.TrimSpace(value), nil
			}
			return st.ResolveAccountInput(ctx, value)
		},
		// 服务端指定 DLL（登录器外网版校验 + 注入）：整表读写，落到 MySQL。
		DLLList: func(ctx context.Context) ([]store.LauncherDLL, error) {
			st := a.manageStore()
			if st == nil {
				return nil, errors.New("认证服务未运行，无法读取可加载 DLL 清单")
			}
			return st.ListDLLs(ctx, false)
		},
		DLLReplace: func(ctx context.Context, items []store.LauncherDLL) ([]store.LauncherDLL, error) {
			st := a.manageStore()
			if st == nil {
				return nil, errors.New("认证服务未运行，无法保存可加载 DLL 清单")
			}
			saved, err := st.ReplaceDLLs(ctx, items)
			if err != nil {
				return nil, err
			}
			a.logf("可加载 DLL 清单已由管理工具更新：共 %d 条", len(saved))
			return saved, nil
		},
		AdminRegister: func(ctx context.Context, username, password, email, rescueCode string) (string, error) {
			st := a.manageStore()
			if st == nil {
				return "", errors.New("认证服务未运行，无法建号")
			}
			username = store.NormalizeUsername(username)
			if len(username) < 3 || len(username) > 32 {
				return "", errors.New("账号名需 3~32 个字符")
			}
			if len(password) < 8 {
				return "", errors.New("password-too-short")
			}
			// 管理员建号传空 remoteIP（唯一的豁免信号，register_ip 记为 NULL 不占额度），
			// maxPerIP 传 -1（不限制）表明与"每 IP 上限"无关：0 现在是"不允许注册"，
			// 再传 0 会让这行代码读起来像"故意拦自己"。
			accountID, err := st.Register(ctx, username, password, email, "", "open", "", rescueCode, -1)
			if err != nil {
				return "", err
			}
			// 管理员建号自动套用注册赠送规则（与玩家自助注册同源）。
			if rb := a.rbPublicInfo(); rb.Enabled && rb.Points >= 1 && rb.Points <= rbMaxPoints {
				if err := st.MarkRegisterBonusPending(ctx, accountID, rb.Points); err != nil {
					return accountID, fmt.Errorf("账号已创建，但点券登记失败: %w", err)
				}
			}
			return accountID, nil
		},
		AdminChangePassword: func(ctx context.Context, username, newPassword string) error {
			st := a.manageStore()
			if st == nil {
				return errors.New("认证服务未运行")
			}
			return st.ChangePassword(ctx, username, newPassword)
		},
		AdminForceLogout: func(ctx context.Context, username string) (int64, error) {
			st := a.manageStore()
			if st == nil {
				return 0, errors.New("认证服务未运行")
			}
			return st.ForceLogoutByUsername(ctx, username)
		},
		// 批量踢下线（管理工具在线列表「踢下选中 / 全部踢下」）。
		// 每个账号三件事：GM 立即断开在线游戏连接 → 写 GM 踢人事件（登录器关本地游戏进程）
		// → 作废该账号全部 ticket。任一步失败不影响其余账号，失败原因随结果回给管理工具。
		AdminKickUsers: func(ctx context.Context, usernames []string) (manage.AdminKickResult, error) {
			st := a.manageStore()
			if st == nil {
				return manage.AdminKickResult{}, errors.New("认证服务未运行，无法踢下线")
			}
			res := manage.AdminKickResult{Failed: []string{}}
			// 后台协程里只认配置里的服务端根目录（不能读 GUI 控件，见 ban_tab.go 注释）。
			root := strings.TrimSpace(a.cfg.ServerRoot)
			seen := make(map[string]struct{}, len(usernames))
			for _, raw := range usernames {
				name := store.NormalizeUsername(raw)
				if name == "" {
					continue
				}
				if _, dup := seen[name]; dup {
					continue
				}
				seen[name] = struct{}{}
				res.Requested++

				acc, err := st.GetAccountByUsername(ctx, name)
				if err != nil {
					res.Failed = append(res.Failed, name+"（账号不存在）")
					continue
				}
				if n, kerr := a.kickByAdminWithRoot(store.BanAccount, acc.AccountID, root); kerr != nil {
					res.Failed = append(res.Failed, name+"（断开游戏连接失败: "+kerr.Error()+"）")
				} else {
					res.Kicked += n
				}
				if _, eerr := st.CreateGMKickEvent(ctx, acc.AccountID, "kick", "管理员批量踢下线", "ops"); eerr != nil {
					a.logf("批量踢下线：写 GM 踢人事件失败 account=%s err=%v", acc.Username, eerr)
				}
				if n, serr := st.ForceLogoutByUsername(ctx, acc.Username); serr == nil {
					res.Sessions += n
				}
			}
			a.logf("批量踢下线：请求 %d 个账号，断开游戏会话 %d 个，作废票 %d 张，失败 %d 个",
				res.Requested, res.Kicked, res.Sessions, len(res.Failed))
			return res, nil
		},
		// 数据库备份远程桥接（管理工具 P1 备份闭环）。
		BackupStatus: func() manage.BackupStatus {
			return a.manageBackupStatus()
		},
		BackupApply: func(ctx context.Context, enabled bool, intervalHours, keep int) (manage.BackupStatus, error) {
			return a.manageBackupApply(ctx, enabled, intervalHours, keep)
		},
		BackupList: func() ([]manage.BackupFileEntry, error) {
			return a.manageBackupList()
		},
		BackupNow: func(ctx context.Context) (manage.BackupNowResult, error) {
			return a.manageBackupNow(ctx)
		},
		BackupRestore: func(ctx context.Context, zipName string, logf func(string)) (manage.BackupRestoreResult, error) {
			return a.manageBackupRestore(ctx, zipName, logf)
		},
	}
}

// rbBridgeStatus rbStat -> manage.RegisterBonusStatus。
func (a *App) rbBridgeStatus(s rbStat) manage.RegisterBonusStatus {
	return manage.RegisterBonusStatus{
		Enabled:  s.Enabled,
		Points:   s.Points,
		Currency: rbNormalizeCurrency(s.Currency),
	}
}

// psBridgeStatus psStat -> manage.PointsinkStatus。
func (a *App) psBridgeStatus(s psStat) manage.PointsinkStatus {
	return manage.PointsinkStatus{
		Enabled:         s.Enabled,
		IntervalSeconds: s.IntervalSeconds,
		Points:          s.Points,
		DailyLimit:      s.DailyLimit,
		Currency:        s.Currency,
		CurrencyLabel:   s.CurrencyLabel,
		Running:         s.Running,
		Awarded:         s.Awarded,
		GrantedPoints:   s.GrantedPoints,
		ErrorCount:      s.ErrorCount,
		LastGrantAt:     s.LastGrantAt,
		NextGrantAt:     s.NextGrantAt,
	}
}

// manageStore 返回可用 store 或 nil（与 GUI requireAuthStore 同源判断）。
func (a *App) manageStore() *store.Store {
	if !a.svcOn.Load() || a.st == nil {
		return nil
	}
	return a.st
}

// manageServerStart 复用 onStart 一键启动链路：仅在 UI 线程触发，随后等待 busy 翻转确认完成。
func (a *App) manageServerStart(ctx context.Context) error {
	return a.manageTriggerAndWait(ctx, "启动", func() { a.onStart() })
}

// manageServerStop 复用 onStop 停止链路。
func (a *App) manageServerStop(ctx context.Context) error {
	return a.manageTriggerAndWait(ctx, "停止", func() { a.onStop() })
}

// manageTriggerAndWait 在 UI 线程执行 UI 动作（onStart/onStop），等待其 runAsync 完成（busy=false）。
func (a *App) manageTriggerAndWait(ctx context.Context, label string, fn func()) error {
	if a.mw == nil {
		return errors.New("GUI 尚未就绪")
	}
	var triggerErr error
	done := make(chan struct{})
	a.mw.Synchronize(func() {
		if a.busy.Load() {
			triggerErr = errors.New("工具正忙（有其他任务执行中），请稍后再试")
			close(done)
			return
		}
		fn() // 内部 runAsync 同步置 busy=true 并异步执行
		close(done)
	})
	select {
	case <-ctx.Done():
		return ctx.Err()
	case <-done:
	}
	if triggerErr != nil {
		return triggerErr
	}
	// 等待 UI 后台任务结束（onStart/onStop 的 runAsync 完成会把 busy 置回 false）。
	//
	// 这里必须直接读 a.busy（atomic.Bool），**不能**用 a.mw.Synchronize 去读：
	// Synchronize 只入队 + PostMessage、不同步执行，busy 会在闭包执行前就被读走（一直是
	// false），于是循环第一轮就返回 nil——管理端会在"任务刚被触发"时就收到成功，
	// 而服务端其实还在起。busy 是原子量，跨协程直读既安全又正确。
	deadline := time.After(10 * time.Minute)
	for {
		if !a.busy.Load() {
			return nil
		}
		select {
		case <-ctx.Done():
			return ctx.Err()
		case <-deadline:
			return fmt.Errorf("%s任务超时未完成", label)
		case <-time.After(700 * time.Millisecond):
		}
	}
}

// ---------- 业务复用实现 ----------

// manageBanNow 对齐 GUI onBanDo 语义（无 UI）：写库 → 踢下线(GM) → GM 踢人事件。
// banType 支持 account / ip / machine / triple；triple 时按 username 解析账号并一键三封。
func (a *App) manageBanNow(ctx context.Context, banType, banValue, username, reason string, days int) (manage.BanNowResult, error) {
	st := a.manageStore()
	if st == nil {
		return manage.BanNowResult{}, errors.New("认证服务未运行，无法执行封禁")
	}
	operator := "ops"
	banType = strings.ToLower(strings.TrimSpace(banType))
	banValue = strings.TrimSpace(banValue)
	reason = strings.TrimSpace(reason)

	if banType == "triple" {
		accName := strings.TrimSpace(username)
		if accName == "" {
			accName = banValue
		}
		// 修复（2026-09-24）：原来用 GetAccountByUsername，而它的 SELECT 不含
		// last_login_ip / machine_id —— acc.LastLoginIP / acc.MachineID 恒为空串，
		// 下面 `if bv == "" { continue }` 会把 IP/机器码两条静默跳过，
		// "一键三封"实际只封了账号一条（封禁列表里也只出现一行）。
		// 改用 AccountBanTargets（与账号列表同源取值），并把跳过原因写进 warnings。
		acc, err := st.AccountBanTargets(ctx, accName)
		if err != nil {
			return manage.BanNowResult{}, fmt.Errorf("账号不存在或查询失败: %w", err)
		}
		targets := []struct {
			bt, bv string
		}{
			{store.BanAccount, acc.AccountID},
			{store.BanIP, acc.LastIP},
			{store.BanMachine, acc.MachineID},
		}
		res := manage.BanNowResult{Type: "account", Value: acc.AccountID}
		for _, t := range targets {
			if strings.TrimSpace(t.bv) == "" {
				res.Warnings = append(res.Warnings,
					fmt.Sprintf("跳过%s：该账号没有这项记录，本次只封到已列出的维度", banTargetDimensionLabel(t.bt)))
				continue
			}
			if _, err := st.Ban(ctx, t.bt, t.bv, acc.Username, reason, operator, days); err != nil {
				res.Warnings = append(res.Warnings, fmt.Sprintf("封禁 %s=%s 失败: %v", t.bt, t.bv, err))
				continue
			}
			n, kerr := a.kickByAdmin(t.bt, t.bv)
			if kerr != nil {
				res.Warnings = append(res.Warnings, fmt.Sprintf("踢下线请求失败(%s=%s): %v", t.bt, t.bv, kerr))
			} else {
				res.Kicked += n
			}
		}
		if _, err := st.CreateGMKickEvent(ctx, acc.AccountID, "kick", reason, operator); err != nil {
			res.Warnings = append(res.Warnings, fmt.Sprintf("写入 GM 踢人事件失败: %v", err))
		}
		return res, nil
	}

	// 普通单类型封禁（account / ip / machine）。
	res := manage.BanNowResult{Type: banType, Value: banValue}
	banValue2 := banValue
	userName2 := strings.TrimSpace(username)
	if banType == store.BanAccount {
		// 解析顺序见 ban_resolve.go：username 优先 → dnf:<hex> 直封 → 否则把 value 当用户名。
		// 修复点（2026-09-20）：原实现直接 GetAccountByUsername(banValue)，管理工具
		// 只填「用户名」不填「封禁值」时 banValue 为空 → 必然 user-not-found。
		accID, displayName, rerr := resolveAccountTarget(ctx, st, banValue, username)
		if rerr != nil {
			return res, fmt.Errorf("账号不存在或查询失败（%s）: %w", banTargetLabel(banValue, username), rerr)
		}
		banValue2 = accID
		userName2 = displayName
	}
	rec, err := st.Ban(ctx, banType, banValue2, userName2, reason, operator, days)
	if err != nil {
		return res, err
	}
	res.Value = banValue2
	n, kerr := a.kickByAdmin(banType, banValue2)
	if kerr != nil {
		res.Warnings = append(res.Warnings, fmt.Sprintf("踢下线请求失败: %v（若玩家不在线可忽略）", kerr))
	} else {
		res.Kicked = n
	}
	// account 型一定知道账号ID（banValue2）：GM 踢人事件按账号ID写。
	// 精确账号ID 直封时可能查不到用户名，但事件照写（登录器按 account_id 拉取）；
	// ip/machine 型只有从账号行选中才有用户名，无法定位账号时不写。
	if banType == store.BanAccount {
		if _, err := st.CreateGMKickEvent(ctx, banValue2, "kick", reason, operator); err != nil {
			res.Warnings = append(res.Warnings, fmt.Sprintf("写入 GM 踢人事件失败: %v", err))
		}
	} else if rec != nil && strings.TrimSpace(rec.Username) != "" {
		if _, err := st.CreateGMKickEvent(ctx, banValue2, "kick", reason, operator); err != nil {
			res.Warnings = append(res.Warnings, fmt.Sprintf("写入 GM 踢人事件失败: %v", err))
		}
	}
	return res, nil
}

// manageRetryRedeemMails CDK 失败邮件补发：code 为空时遍历全部 CDK；返回成功/失败条数与首个错误。
func (a *App) manageRetryRedeemMails(ctx context.Context, code string) (int, int, error) {
	st := a.manageStore()
	if st == nil {
		return 0, 0, errors.New("认证服务未运行，无法补发邮件")
	}
	code = strings.TrimSpace(code)

	var targetCodes []string
	if code != "" {
		norm, err := store.NormalizeRedeemCode(code)
		if err != nil {
			return 0, 0, err
		}
		codes, err := st.ListRedeemCodes(ctx, norm, 1, 10)
		if err != nil {
			return 0, 0, err
		}
		found := false
		for i := range codes {
			if codes[i].RedeemCode == norm {
				found = true
				break
			}
		}
		if !found {
			return 0, 0, fmt.Errorf("未找到兑换码 %s", norm)
		}
		targetCodes = append(targetCodes, norm)
	} else {
		page := 1
		for {
			codes, err := st.ListRedeemCodes(ctx, "", page, 500)
			if err != nil {
				return 0, 0, err
			}
			for i := range codes {
				targetCodes = append(targetCodes, codes[i].RedeemCode)
			}
			if len(codes) < 500 {
				break
			}
			page++
		}
	}

	sent, failed := 0, 0
	var firstErr error
	for _, tc := range targetCodes {
		s, f, err := a.manageRetryRedeemMailOne(ctx, tc)
		sent += s
		failed += f
		if err != nil && firstErr == nil {
			firstErr = err
		}
	}
	return sent, failed, firstErr
}

// manageRetryRedeemMailOne 对单个码补发失败邮件（复刻 GUI onCdkRetryMails 业务逻辑）。
func (a *App) manageRetryRedeemMailOne(ctx context.Context, code string) (int, int, error) {
	st := a.manageStore()
	if st == nil {
		return 0, 0, errors.New("认证服务未运行")
	}
	norm, err := store.NormalizeRedeemCode(code)
	if err != nil {
		return 0, 0, err
	}
	codes, err := st.ListRedeemCodes(ctx, norm, 1, 50)
	if err != nil {
		return 0, 0, err
	}
	var target *store.RedeemCode
	for i := range codes {
		if codes[i].RedeemCode == norm {
			target = &codes[i]
			break
		}
	}
	if target == nil {
		return 0, 0, fmt.Errorf("未找到兑换码 %s", norm)
	}
	attachments := parseCdkRewardJSON(target.RewardJSON)
	ceraTotal := cdkRewardCeraTotal(target.RewardJSON)
	bdDays := cdkRewardBlackDiamondDays(target.RewardJSON)
	if len(attachments) == 0 {
		switch {
		case ceraTotal > 0 && bdDays > 0:
			return 0, 0, fmt.Errorf("该 CDK 的奖励只有点券（+%d）与黑钻（%d 天），没有可补发的物品邮件；点券/黑钻都不能自动补发（会重复发放），请按角色手动补", ceraTotal, bdDays)
		case bdDays > 0:
			return 0, 0, fmt.Errorf("该 CDK 的奖励只有黑钻（%d 天），没有可补发的物品邮件；黑钻不能自动补发（会重复发放），请在「账号管理」页按角色重新开启", bdDays)
		case ceraTotal > 0:
			return 0, 0, fmt.Errorf("该 CDK 的奖励只有点券（+%d），没有可补发的物品邮件；点券不能自动补发（会重复入账），请按账号手动补发", ceraTotal)
		}
		return 0, 0, fmt.Errorf("该 CDK 没有可补发的奖励物品")
	}

	reds, err := st.ListRedeemRedemptions(ctx, norm, 1000)
	if err != nil {
		return 0, 0, err
	}
	sent, failed := 0, 0
	for i := range reds {
		r := &reds[i]
		if !redeemRetryTargetsRow(r.MailStatus, r.MailError) {
			continue
		}
		if strings.TrimSpace(r.CharacterID) == "" {
			a.manageLogf("补发跳过(无角色ID) redemption_id=%d code=%s", r.ID, norm)
			continue
		}
		mailErr := a.cdkGmSendMail(ctx, cdkGMMailRequest{
			CharacterID: r.CharacterID,
			Title:       "CDK兑换奖励",
			Body:        "兑换成功：" + target.Name,
			Attachments: attachments,
		})
		if mailErr != nil {
			failed++
			a.manageLogf("补发失败 code=%s username=%s: %v", norm, r.Username, mailErr)
			_ = st.SetRedemptionMailResult(ctx, r.ID, "failed", truncateStr(mailErr.Error()))
			continue
		}
		sent++
		_ = st.SetRedemptionMailResult(ctx, r.ID, "sent", "")
	}
	return sent, failed, nil
}

// manageExportRedeemTxt 生成成功后自动导出 TXT（对齐 GUI 自动导出行为，落 cdk_export 目录）。
func (a *App) manageExportRedeemTxt(name string, codes []string) {
	if len(codes) == 0 {
		return
	}
	lines := make([][3]string, 0, len(codes))
	for _, c := range codes {
		lines = append(lines, [3]string{c, name, "未兑换"})
	}
	p := filepath.Join(cdkExportBaseDir(), cdkExportFileName("", name))
	if err := os.WriteFile(p, []byte(buildCdkTxtContent(lines)), 0o666); err != nil {
		a.manageLogf("CDK 生成成功，但自动导出 TXT 失败: %v", err)
		return
	}
	a.manageLogf("CDK 已自动导出: %s（%d 张）", p, len(codes))
}

// manageLogf 线程安全的日志（经 UI 主线程写入日志框）。
func (a *App) manageLogf(format string, args ...interface{}) {
	a.uiSafe(func() {
		a.logf(format, args...)
	})
}
