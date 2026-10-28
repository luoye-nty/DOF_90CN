// 封禁管理页（90 远程认证链路）：封账号/IP/机器码、自定义时长(天)、手动解封、
// 封禁即踢下线（调 go-server admin /local/dnf-gm/kick-sessions）。
package main

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"strconv"
	"strings"
	"time"

	"dnf90ops/internal/gmhttp"
	"dnf90ops/internal/serverdir"
	"dnf90ops/internal/store"

	"github.com/lxn/walk"
	. "github.com/lxn/walk/declarative"
)

// banTab UI 控件句柄。
type banTabWidgets struct {
	banTypeCb     *walk.ComboBox
	banValueLbl   *walk.Label
	banValueEdit  *walk.LineEdit
	banDaysEdit   *walk.LineEdit
	banReasonEdit *walk.LineEdit
	banHintL      *walk.Label
	banTable      *walk.TableView
	accTable      *walk.TableView
	onlineTable   *walk.TableView
	accKeyword    *walk.LineEdit
	banBtn        *walk.PushButton
	refreshBtn    *walk.PushButton

	// 最近一次从账号/在线列表选中的账户（用于 IP/机器码封禁时回填 username 与写踢人事件）
	pending *store.Account
}

var banWidgets banTabWidgets

var banTypeLabels = []string{"账号", "IP", "机器码", "一键三封(账号+IP+机器码)"}

// 类型下拉索引 → 存储 ban_type；index==3（一键三封）返回账号类型仅作占位，
// 实际封禁走 onBanDo 中的 triple 分支。
func banTypeByIndex(idx int) string {
	switch idx {
	case 1:
		return store.BanIP
	case 2:
		return store.BanMachine
	default:
		return store.BanAccount
	}
}

func banTripleSelected() bool {
	return banWidgets.banTypeCb != nil && banWidgets.banTypeCb.CurrentIndex() == 3
}

func (a *App) banTab() []Widget {
	return []Widget{
		Composite{
			Layout: Grid{Columns: 4, Margins: Margins{10, 10, 10, 4}, Spacing: 6},
			Children: []Widget{
				Label{Text: "类型:"},
				ComboBox{
					AssignTo: &banWidgets.banTypeCb,
					Model:    banTypeLabels,
					OnCurrentIndexChanged: func() { a.refreshBanValuePlaceholder() },
				},
				Label{AssignTo: &banWidgets.banValueLbl, Text: "账号:"},
				LineEdit{AssignTo: &banWidgets.banValueEdit},
				Label{Text: "时长(天):"},
				LineEdit{AssignTo: &banWidgets.banDaysEdit, ToolTipText: "留空或填 0 = 永久封禁"},
				Label{Text: "原因:"},
				LineEdit{AssignTo: &banWidgets.banReasonEdit},
			},
		},
		Composite{
			Layout: HBox{Margins: Margins{10, 0, 10, 6}, Spacing: 8},
			Children: []Widget{
				PushButton{AssignTo: &banWidgets.banBtn, Text: "封禁并踢下线", OnClicked: a.onBanDo},
				PushButton{Text: "解封选中记录", OnClicked: a.onBanUnban},
				PushButton{AssignTo: &banWidgets.refreshBtn, Text: "刷新列表", OnClicked: a.onBanRefresh},
				Label{AssignTo: &banWidgets.banHintL, Text: "需先启动认证服务；踢下线需服务端已运行(admin 18111)"},
			},
		},
		Composite{
			Layout: HBox{Margins: Margins{10, 0, 10, 2}, Spacing: 6},
			Children: []Widget{
				Label{Text: "账号快捷查询:"},
				LineEdit{AssignTo: &banWidgets.accKeyword, MinSize: Size{Width: 180, Height: 0}},
				PushButton{Text: "查询", OnClicked: a.onBanQueryAccounts},
				PushButton{Text: "选中行填入封禁目标", OnClicked: a.onBanFillFromAccount},
			},
		},
		Composite{
			Layout: VBox{Margins: Margins{10, 2, 10, 2}, Spacing: 2},
			Children: []Widget{
				Label{Text: "在线列表（登录器心跳中且会话未过期的账号；选中行自动填入上方封禁目标，双击定位最近IP/机器码）"},
				TableView{
					AssignTo: &banWidgets.onlineTable,
					Columns: []TableViewColumn{
						{Title: "账号", Width: 110},
						{Title: "账号ID", Width: 150},
						{Title: "状态", Width: 55},
						{Title: "最近IP", Width: 105},
						{Title: "机器码", Width: 130},
						{Title: "最近活跃", Width: 120},
						{Title: "会话到期", Width: 120},
					},
					Model: new(onlineTableModel),
					MinSize: Size{Width: 820, Height: 110},
					OnCurrentIndexChanged: func() { a.fillBanFromOnline() },
				},
			},
		},
		Composite{
			Layout: HBox{Margins: Margins{10, 4, 10, 4}, Spacing: 10},
			Children: []Widget{
				Composite{
					Layout: VBox{MarginsZero: true, Spacing: 2},
					Children: []Widget{
						Label{Text: "账号列表（选中行自动填入上方封禁目标）"},
						TableView{
							AssignTo: &banWidgets.accTable,
							Columns: []TableViewColumn{
								{Title: "账号", Width: 110},
								{Title: "账号ID", Width: 150},
								{Title: "状态", Width: 55},
								{Title: "最近IP", Width: 105},
								{Title: "最近机器码", Width: 130},
								{Title: "注册时间", Width: 120},
							},
							Model: new(accountTableModel),
							MinSize: Size{Width: 480, Height: 130},
							OnCurrentIndexChanged: func() { a.fillBanFromAccount(true) },
						},
					},
				},
				Composite{
					Layout: VBox{MarginsZero: true, Spacing: 2},
					Children: []Widget{
						Label{Text: "封禁记录"},
						TableView{
							AssignTo: &banWidgets.banTable,
							Columns: []TableViewColumn{
								{Title: "类型", Width: 55},
								{Title: "目标值", Width: 170},
								{Title: "状态", Width: 60},
								{Title: "账号", Width: 100},
								{Title: "原因", Width: 110},
								{Title: "操作者", Width: 70},
								{Title: "封禁时间", Width: 120},
								{Title: "到期时间", Width: 120},
								{Title: "解封时间", Width: 120},
							},
							Model: new(banTableModel),
							MinSize: Size{Width: 560, Height: 130},
						},
					},
				},
			},
		},
	}
}

func (a *App) refreshBanValuePlaceholder() {
	if banWidgets.banValueEdit == nil {
		return
	}
	idx := banWidgets.banTypeCb.CurrentIndex()
	switch idx {
	case 1:
		if banWidgets.banValueLbl != nil {
			banWidgets.banValueLbl.SetText("IP:")
		}
		banWidgets.banValueEdit.SetToolTipText("要封禁的来源 IP，如 1.2.3.4")
	case 2:
		if banWidgets.banValueLbl != nil {
			banWidgets.banValueLbl.SetText("机器码:")
		}
		banWidgets.banValueEdit.SetToolTipText("要封禁的机器码（登录器 machine_id），可留空后从账号列表选择")
	case 3:
		if banWidgets.banValueLbl != nil {
			banWidgets.banValueLbl.SetText("账号:")
		}
		banWidgets.banValueEdit.SetToolTipText("要一键三封的登录账号（username）：同封账号+最近IP+最近机器码")
	default:
		if banWidgets.banValueLbl != nil {
			banWidgets.banValueLbl.SetText("账号:")
		}
		banWidgets.banValueEdit.SetToolTipText("要封禁的登录账号（username）；也可直接粘贴精确账号ID（dnf:…）直封")
	}
}

func (a *App) requireAuthStore() *store.Store {
	if !a.svcOn.Load() || a.st == nil {
		banWidgets.banHintL.SetText("认证服务未运行，请先在「认证服务」页启动")
		return nil
	}
	return a.st
}

// 账号列表模型
type accountTableModel struct {
	walk.TableModelBase
	items []store.Account
}

func (m *accountTableModel) RowCount() int { return len(m.items) }

func (m *accountTableModel) Value(row, col int) any {
	if row < 0 || row >= len(m.items) {
		return nil
	}
	it := m.items[row]
	switch col {
	case 0:
		return it.Username
	case 1:
		return it.AccountID
	case 2:
		return it.State
	case 3:
		return it.LastLoginIP
	case 4:
		return it.MachineID
	case 5:
		if it.CreatedAt.Valid {
			return it.CreatedAt.Time.Format("2006-01-02 15:04")
		}
	}
	return ""
}

func (a *App) onBanQueryAccounts() {
	if a.requireAuthStore() == nil {
		return
	}
	a.runAsync("查询账号", func() error {
		return a.loadAccounts()
	}, func(err error) {
		if err != nil {
			a.logf("查询账号失败: %v", err)
			return
		}
	})
}

// 在线账号表格模型
type onlineTableModel struct {
	walk.TableModelBase
	items []store.OnlineAccount
}

func (m *onlineTableModel) RowCount() int { return len(m.items) }

func (m *onlineTableModel) Value(row, col int) any {
	if row < 0 || row >= len(m.items) {
		return nil
	}
	it := m.items[row]
	ft := func(t time.Time) string { return t.Format("2006-01-02 15:04") }
	switch col {
	case 0:
		return it.Username
	case 1:
		return it.AccountID
	case 2:
		return it.State
	case 3:
		return it.LastIP
	case 4:
		return it.MachineID
	case 5:
		if it.IssuedAt.Valid {
			return ft(it.IssuedAt.Time)
		}
	case 6:
		return ft(it.ExpiresAt)
	}
	return ""
}

func (a *App) loadOnline() error {
	recs, err := a.st.ListOnlineAccounts(context.Background(), 0)
	if err != nil {
		return err
	}
	a.uiSafe(func() {
		if banWidgets.onlineTable == nil {
			return
		}
		// 内容未变化时跳过 SetModel，避免周期刷新把用户正在选中的行重置掉
		if cur, ok := banWidgets.onlineTable.Model().(*onlineTableModel); ok && sameOnline(cur.items, recs) {
			return
		}
		banWidgets.onlineTable.SetModel(&onlineTableModel{items: recs})
	})
	return nil
}

// sameOnline 比较在线列表的账号集合是否一致（到期时间随心跳续期变化，不参与比较）。
func sameOnline(a, b []store.OnlineAccount) bool {
	if len(a) != len(b) {
		return false
	}
	for i := range a {
		if a[i].AccountID != b[i].AccountID || a[i].Username != b[i].Username ||
			a[i].LastIP != b[i].LastIP || a[i].MachineID != b[i].MachineID {
			return false
		}
	}
	return true
}

// fillBanValueFromAccount 按当前封禁类型把选中账户填充到封禁目标输入框，并记录 pending 账户。
func (a *App) fillBanValueFromAccount(acc *store.Account) {
	if acc == nil {
		return
	}
	banWidgets.pending = acc
	switch {
	case banTripleSelected():
		banWidgets.banValueEdit.SetText(acc.Username)
	case banTypeByIndex(banWidgets.banTypeCb.CurrentIndex()) == store.BanIP:
		banWidgets.banValueEdit.SetText(acc.LastLoginIP)
	case banTypeByIndex(banWidgets.banTypeCb.CurrentIndex()) == store.BanMachine:
		banWidgets.banValueEdit.SetText(acc.MachineID)
	default:
		banWidgets.banValueEdit.SetText(acc.Username)
	}
	if banWidgets.banValueEdit.Text() == "" {
		banWidgets.banHintL.SetText("该账号无可用目标值（IP/机器码为空），请手动填写")
	} else {
		banWidgets.banHintL.SetText(fmt.Sprintf("已填入 %s", banWidgets.banValueEdit.Text()))
	}
}

// fillBanFromAccount 从账号列表选中行填充封禁目标（选中即自动填充）。
func (a *App) fillBanFromAccount(_ bool) {
	if banWidgets.accTable == nil {
		return
	}
	idx := banWidgets.accTable.CurrentIndex()
	model, ok := banWidgets.accTable.Model().(*accountTableModel)
	if !ok || idx < 0 || idx >= len(model.items) {
		return
	}
	a.fillBanValueFromAccount(&model.items[idx])
}

// fillBanFromOnline 从在线列表选中行填充封禁目标（选中即自动填充）。
func (a *App) fillBanFromOnline() {
	if banWidgets.onlineTable == nil {
		return
	}
	idx := banWidgets.onlineTable.CurrentIndex()
	model, ok := banWidgets.onlineTable.Model().(*onlineTableModel)
	if !ok || idx < 0 || idx >= len(model.items) {
		return
	}
	it := model.items[idx]
	a.fillBanValueFromAccount(&store.Account{
		AccountID:   it.AccountID,
		Username:    it.Username,
		State:       it.State,
		LastLoginIP: it.LastIP,
		MachineID:   it.MachineID,
	})
}

// 封禁记录模型
type banRow struct {
	Rec   store.BanRecord
	State string
}

type banTableModel struct {
	walk.TableModelBase
	items []banRow
}

func (m *banTableModel) RowCount() int { return len(m.items) }

func (m *banTableModel) Value(row, col int) any {
	if row < 0 || row >= len(m.items) {
		return nil
	}
	r := m.items[row].Rec
	ft := func(t time.Time) string { return t.Format("2006-01-02 15:04") }
	switch col {
	case 0:
		return map[string]string{store.BanAccount: "账号", store.BanIP: "IP", store.BanMachine: "机器码"}[r.BanType]
	case 1:
		return r.BanValue
	case 2:
		return m.items[row].State
	case 3:
		return r.Username
	case 4:
		return r.Reason
	case 5:
		return r.Operator
	case 6:
		return ft(r.BannedAt)
	case 7:
		if r.ExpiresAt.Valid {
			return ft(r.ExpiresAt.Time)
		}
		return "永久"
	case 8:
		if r.RevokedAt.Valid {
			return ft(r.RevokedAt.Time)
		}
		return ""
	}
	return ""
}

func (a *App) loadBans() error {
	recs, err := a.st.ListBans(context.Background(), 0)
	if err != nil {
		return err
	}
	items := make([]banRow, 0, len(recs))
	now := time.Now()
	for _, r := range recs {
		items = append(items, banRow{Rec: r, State: r.StateLabel(now)})
	}
	model := &banTableModel{items: items}
	a.uiSafe(func() {
		if banWidgets.banTable != nil {
			banWidgets.banTable.SetModel(model)
		}
	})
	return nil
}

func (a *App) loadAccounts() error {
	recs, err := a.st.ListAccounts(context.Background(), banWidgets.accKeyword.Text())
	if err != nil {
		return err
	}
	model := &accountTableModel{items: recs}
	a.uiSafe(func() {
		if banWidgets.accTable != nil {
			banWidgets.accTable.SetModel(model)
		}
	})
	return nil
}

func (a *App) onBanRefresh() {
	st := a.requireAuthStore()
	if st == nil {
		return
	}
	a.runAsync("刷新封禁列表", func() error {
		if err := a.loadBans(); err != nil {
			return err
		}
		if err := a.loadAccounts(); err != nil {
			return err
		}
		return a.loadOnline()
	}, func(err error) {
		if err != nil {
			a.logf("刷新封禁列表失败: %v", err)
		} else {
			a.logf("封禁列表已刷新")
		}
	})
}

func (a *App) onBanFillFromAccount() {
	a.fillBanFromAccount(false)
}

// onBanDo 封禁（写库）→ 成功后踢下线（admin HTTP）+ 记录 GM 踢人事件（登录器拉取关进程）→ 刷新。
func (a *App) onBanDo() {
	st := a.requireAuthStore()
	if st == nil {
		return
	}
	triple := banTripleSelected()
	banType := banTypeByIndex(banWidgets.banTypeCb.CurrentIndex())
	value := strings.TrimSpace(banWidgets.banValueEdit.Text())
	if value == "" {
		banWidgets.banHintL.SetText("请填写封禁目标值")
		return
	}
	days := 0
	if raw := strings.TrimSpace(banWidgets.banDaysEdit.Text()); raw != "" {
		n, err := strconv.Atoi(raw)
		if err != nil || n < 0 {
			banWidgets.banHintL.SetText("时长(天)必须是正整数，留空或 0 表示永久")
			return
		}
		days = n
	}
	reason := strings.TrimSpace(banWidgets.banReasonEdit.Text())
	operator := "ops"

	a.runAsync("执行封禁", func() error {
		ctx := context.Background()

		// 一键三封：按 username 解析 account_id，依次封 账号 + 最近IP + 最近机器码。
		if triple {
			// 修复（2026-09-24）：GetAccountByUsername 的 SELECT 不含 last_login_ip /
			// machine_id，用它取的"最近IP/机器码"恒为空串 → 两条被 `if bv == "" { continue }`
			// 静默跳过，一键三封实际只封了账号那一条。改用 AccountBanTargets
			// （与账号列表同源取值：最近登录 IP + launcher_sessions 最新票据的 machine_id）。
			acc, err := st.AccountBanTargets(ctx, value)
			if err != nil {
				return fmt.Errorf("账号不存在或查询失败: %w", err)
			}
			targets := []struct {
				bt, bv string
			}{
				{store.BanAccount, acc.AccountID},
				{store.BanIP, acc.LastIP},
				{store.BanMachine, acc.MachineID},
			}
			kicked := 0
			for _, t := range targets {
				if strings.TrimSpace(t.bv) == "" {
					a.logf("跳过%s：该账号没有这项记录，本次只封到已列出的维度", banTargetDimensionLabel(t.bt))
					continue
				}
				if _, err := st.Ban(ctx, t.bt, t.bv, acc.Username, reason, operator, days); err != nil {
					return fmt.Errorf("封禁 %s=%s 失败: %w", t.bt, t.bv, err)
				}
				a.logf("一键三封已写入 type=%s value=%s", t.bt, t.bv)
				n, kerr := a.kickByAdmin(t.bt, t.bv)
				if kerr != nil {
					a.logf("踢下线请求失败(%s=%s): %v（若玩家不在线可忽略）", t.bt, t.bv, kerr)
				} else {
					kicked += n
				}
			}
			// GM 事件：让该账号登录器拉取后关闭本地游戏进程（含双开中该账号的槽位）。
			if _, err := st.CreateGMKickEvent(ctx, acc.AccountID, "kick", reason, operator); err != nil {
				a.logf("写入 GM 踢人事件失败: %v", err)
			}
			a.logf("一键三封完成，在线会话匹配数=%d", kicked)
			if err := a.loadBans(); err != nil {
				return err
			}
			if err := a.loadAccounts(); err != nil {
				return err
			}
			return a.loadOnline()
		}

		// 普通单类型封禁。
		banValue := value
		username := ""
		if banType == store.BanAccount {
			// 解析顺序见 ban_resolve.go：dnf:<hex> 视为精确账号ID 直封，否则当用户名解析。
			// （GUI 只有一个输入框，所以 username 字段传空。）
			accID, displayName, rerr := resolveAccountTarget(ctx, st, value, "")
			if rerr != nil {
				return fmt.Errorf("账号不存在或查询失败（%s）: %w", banTargetLabel(value, ""), rerr)
			}
			banValue = accID
			username = displayName
		} else if banWidgets.pending != nil {
			// IP/机器码 从账号列表选中而来：冗余记录 username，便于识别与事件通知
			username = banWidgets.pending.Username
		}
		rec, err := st.Ban(ctx, banType, banValue, username, reason, operator, days)
		if err != nil {
			return err
		}
		_ = rec
		a.logf("封禁成功 type=%s value=%s days=%d", banType, banValue, days)
		// 踢下线（服务端可能未在跑，失败不影响封禁结果）
		kicked, kerr := a.kickByAdmin(banType, banValue)
		if kerr != nil {
			a.logf("封禁成功，但踢下线请求失败: %v（若玩家不在线可忽略）", kerr)
		} else {
			a.logf("已向服务端下发踢下线，匹配在线会话数=%d", kicked)
		}
		// GM 踢人事件：账号型封禁总能定位账号（banValue 就是 account_id，含 dnf:<hex> 直封）；
		// IP/机器码型仅在从账号行选中时能定位账号。
		if banType == store.BanAccount {
			if _, err := st.CreateGMKickEvent(ctx, banValue, "kick", reason, operator); err != nil {
				a.logf("写入 GM 踢人事件失败: %v", err)
			}
		} else if banWidgets.pending != nil && banWidgets.pending.AccountID != "" {
			if _, err := st.CreateGMKickEvent(ctx, banWidgets.pending.AccountID, "kick", reason, operator); err != nil {
				a.logf("写入 GM 踢人事件失败: %v", err)
			}
		}
		if err := a.loadBans(); err != nil {
			return err
		}
		if err := a.loadAccounts(); err != nil {
			return err
		}
		return a.loadOnline()
	}, func(err error) {
		if err != nil {
			a.logf("封禁失败: %v", err)
			return
		}
		banWidgets.banValueEdit.SetText("")
		banWidgets.banReasonEdit.SetText("")
	})
}

func (a *App) onBanUnban() {
	st := a.requireAuthStore()
	if st == nil {
		return
	}
	if banWidgets.banTable == nil {
		return
	}
	idx := banWidgets.banTable.CurrentIndex()
	model, ok := banWidgets.banTable.Model().(*banTableModel)
	if !ok || idx < 0 || idx >= len(model.items) {
		banWidgets.banHintL.SetText("请先在封禁记录中选中一行")
		return
	}
	rec := model.items[idx].Rec
	if rec.StateLabel(time.Now()) != "生效中" {
		banWidgets.banHintL.SetText("该记录已解封或已到期，无需重复解封")
		return
	}
	a.runAsync("解封", func() error {
		ok, err := st.Unban(context.Background(), rec.BanType, rec.BanValue)
		if err != nil {
			return err
		}
		if !ok {
			return fmt.Errorf("未找到生效中的封禁记录（可能已解封）")
		}
		a.logf("已解封 type=%s value=%s", rec.BanType, rec.BanValue)
		return a.loadBans()
	}, func(err error) {
		if err != nil {
			a.logf("解封失败: %v", err)
			return
		}
	})
}

// kickByAdmin 调 go-server admin /local/dnf-gm/kick-sessions（仅回环，带 X-Admin-Token）。
// 根目录优先取 GUI 输入框（人可能临时改了根目录），为空再退回配置。
func (a *App) kickByAdmin(banType, banValue string) (int, error) {
	root := strings.TrimSpace(a.rootEdit.Text())
	if root == "" {
		root = strings.TrimSpace(a.cfg.ServerRoot)
	}
	return a.kickByAdminWithRoot(banType, banValue, root)
}

// kickByAdminWithRoot 与 kickByAdmin 同逻辑，但根目录由调用方给死 —— 供后台协程使用：
// manage 服务（管理工具远程调用）在自己的 goroutine 里跑，读 a.rootEdit 这种 GUI 控件
// 不是线程安全的，所以那条路径只认配置里的根目录。
func (a *App) kickByAdminWithRoot(banType, banValue, root string) (int, error) {
	root = strings.TrimSpace(root)
	if root == "" {
		root = strings.TrimSpace(a.cfg.ServerRoot)
	}
	ins := serverdir.Inspect(root)
	if ins == nil || ins.Instance == nil || strings.TrimSpace(ins.Instance.AdminListen) == "" {
		return 0, fmt.Errorf("instance.json 无 adminListen，无法定位服务端管理口")
	}
	token := strings.TrimSpace(ins.Instance.AdminToken)
	if token == "" {
		return 0, fmt.Errorf("instance.json 无 adminToken，请先在服务端初始化")
	}
	url := "http://" + ins.Instance.AdminListen + "/local/dnf-gm/kick-sessions"
	body, _ := json.Marshal(map[string]string{"ban_type": banType, "ban_value": banValue})
	req, err := http.NewRequest(http.MethodPost, url, bytes.NewReader(body))
	if err != nil {
		return 0, err
	}
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("X-Admin-Token", token)
	req.Header.Set("X-Admin-Actor", "ops")
	gmhttp.SetConfirm(req, "ops")
	client := &http.Client{Timeout: 6 * time.Second}
	resp, err := client.Do(req)
	if err != nil {
		return 0, err
	}
	defer resp.Body.Close()
	var out struct {
		OK     bool   `json:"ok"`
		Error  string `json:"error"`
		Kicked int    `json:"kicked"`
	}
	if err := json.NewDecoder(resp.Body).Decode(&out); err != nil {
		return 0, fmt.Errorf("kick 响应解析失败(status=%d): %w", resp.StatusCode, err)
	}
	if !out.OK {
		return 0, fmt.Errorf("服务端拒绝: %s", out.Error)
	}
	return out.Kicked, nil
}
