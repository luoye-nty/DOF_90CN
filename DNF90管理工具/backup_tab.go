package main

// 数据库备份页（管理工具侧，仅连接赞助版运维工具时可用）：
//   - 远程读写服务器端自动备份配置（GET/POST /manage/v1/backup/config），
//     与运维工具 GUI「数据库备份」页共享同一份 config.json autobackup 段；
//   - 远程列出服务器端备份目录（GET /manage/v1/backup/list）；
//   - 远程覆盖还原（POST /manage/v1/backup/restore）：还原前先探测服务器端运行态，
//     运行中则二次确认（将自动停止服务端），由服务端执行 停服 -> 拉起 MySQL -> 还原。
// 还原完成后不会自动启动服务端，需在「服务器状态」页手动点击一键启动。

import (
	"context"
	"fmt"
	"sort"
	"strconv"
	"strings"
	"time"

	"github.com/lxn/walk"
	decl "github.com/lxn/walk/declarative"
)

func buildBackupTab(a *App) decl.TabPage {
	return decl.TabPage{
		Title:   "数据库备份",
		MinSize: decl.Size{Width: 900, Height: 520},
		Layout:  decl.VBox{},
		Children: []decl.Widget{
			decl.Composite{
				Layout: decl.VBox{},
				Children: []decl.Widget{
					decl.GroupBox{
						Title:  "自动备份设置（远程写入服务器端，仅 MySQL 模式）",
						Layout: decl.VBox{MarginsZero: true, Spacing: 8},
						Children: []decl.Widget{
							decl.Composite{
								Layout: decl.HBox{Spacing: 8},
								Children: []decl.Widget{
									decl.CheckBox{
										AssignTo: &a.bkChk,
										Text:     "启用自动备份",
									},
									decl.Label{Text: "间隔（小时）:"},
									decl.LineEdit{
										AssignTo: &a.bkIntEdit,
										MaxSize:  decl.Size{Width: 70, Height: 0},
									},
									decl.Label{Text: "小时（1~168）。保存后从服务器端当前时刻起算，到点自动备份。"},
									decl.Label{Text: "保留份数:"},
									decl.LineEdit{
										AssignTo:    &a.bkKeepEdit,
										MaxSize:     decl.Size{Width: 70, Height: 0},
										ToolTipText: "每次备份成功后自动删掉更旧的备份，最多留这么多份；不设上限会把服务器磁盘写满。",
									},
									decl.Label{Text: "份（1~500）"},
								},
							},
							decl.Composite{
								Layout: decl.HBox{Spacing: 8},
								Children: []decl.Widget{
									decl.PushButton{
										AssignTo: &a.bkSaveBtn,
										Text:     "保存设置",
										OnClicked: func() {
											a.onBkSave()
										},
									},
									decl.PushButton{
										AssignTo: &a.bkRefreshBtn,
										Text:     "刷新",
										OnClicked: func() {
											go a.refreshBkAll(true)
										},
									},
									decl.Label{
										AssignTo:  &a.bkNextL,
										Text:      "尚未加载服务器端配置",
										TextColor: walk.RGB(32, 100, 32),
									},
								},
							},
						},
					},
					decl.GroupBox{
						Title:  "服务器端备份列表",
						Layout: decl.VBox{MarginsZero: true, Spacing: 8},
						Children: []decl.Widget{
							decl.Composite{
								Layout: decl.HBox{Spacing: 8},
								Children: []decl.Widget{
									decl.PushButton{
										AssignTo: &a.bkNowBtn,
										Text:     "立即备份",
										OnClicked: func() {
											a.onBkNow()
										},
									},
									decl.Label{Text: "立即备份不停止服务端，在线生成一份完整备份；完成后自动刷新列表。"},
								},
							},
							decl.ListBox{
								AssignTo: &a.bkList,
								MinSize:  decl.Size{Width: 0, Height: 200},
								Enabled:  false,
							},
							decl.Composite{
								Layout: decl.HBox{Spacing: 8},
								Children: []decl.Widget{
									decl.PushButton{
										AssignTo: &a.bkRestoreBtn,
										Text:     "还原选中备份",
										OnClicked: func() {
											a.onBkRestore()
										},
									},
									decl.Label{Text: "还原会覆盖服务器端数据库。运行中会自动停止服务端，还原完成后不会自动启动，请到「服务器状态」页手动开服。"},
								},
							},
						},
					},
					decl.TextEdit{
						AssignTo: &a.bkStatusText,
						ReadOnly: true,
						MinSize:  decl.Size{Width: 0, Height: 180},
						Font:     decl.Font{Family: "Consolas", PointSize: 9},
					},
				},
			},
		},
	}
}

// bkFileRow 列表行数据。
type bkFileRow struct {
	Name string
	Size int64
	Mod  time.Time
}

// refreshBkAll 拉取服务器端自动备份配置 + 备份列表并渲染；manual=true 时网络错误弹窗。
func (a *App) refreshBkAll(manual bool) {
	c := a.snapshotClient()
	if c == nil {
		return
	}
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	st, err := c.BackupStatus(ctx)
	if err != nil {
		if a.handleAPIError(err) {
			return
		}
		if manual {
			a.showError("备份配置拉取失败", err)
		}
		return
	}
	listCtx, listCancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer listCancel()
	entries, err := c.BackupList(listCtx)
	if err != nil {
		if a.handleAPIError(err) {
			return
		}
		if manual {
			a.showError("备份列表拉取失败", err)
		}
		return
	}
	rows := make([]bkFileRow, 0, len(entries))
	for _, e := range entries {
		mt, _ := time.Parse(time.RFC3339, e.ModTime)
		rows = append(rows, bkFileRow{Name: e.Name, Size: e.Size, Mod: mt})
	}
	sort.Slice(rows, func(i, j int) bool { return rows[i].Mod.After(rows[j].Mod) })
	a.renderBkState(st, rows)
}

// renderBkState 将服务器端配置/列表渲染到 UI（需从任意 goroutine 调用，内部切 UI 线程）。
// rows 为 nil 时保留现有备份列表不变（仅更新配置区，例如保存设置成功后）。
func (a *App) renderBkState(st *BackupStatus, rows []bkFileRow) {
	a.sync(func() {
		a.bkChk.SetChecked(st.Enabled)
		a.bkIntEdit.SetText(strconv.Itoa(st.IntervalHours))
		if st.Keep > 0 {
			a.bkKeepEdit.SetText(strconv.Itoa(st.Keep))
		}
		a.bkNextL.SetText(nextBkLabel(st))
		// 备份列表
		if rows != nil {
			a.bkFiles = make([]string, 0, len(rows))
			if len(rows) == 0 {
				a.bkList.SetEnabled(false)
				a.bkList.SetModel([]string{"（服务器端暂无备份，请先执行一次备份）"})
			} else {
				displays := make([]string, 0, len(rows))
				for _, r := range rows {
					displays = append(displays, r.Mod.Local().Format("2006-01-02 15:04:05")+"   "+humanSize(r.Size)+"   "+r.Name)
					a.bkFiles = append(a.bkFiles, r.Name)
				}
				a.bkList.SetEnabled(true)
				a.bkList.SetModel(displays)
				a.bkList.SetCurrentIndex(-1)
			}
		}
		// 状态文本
		setEditText(a.bkStatusText, renderBkStatus(st, len(a.bkFiles)))
	})
}

func renderBkStatus(st *BackupStatus, fileCount int) string {
	s := "=== 服务器端数据库备份 ===\n"
	s += fmt.Sprintf("自动备份：%s（间隔 %d 小时，保留最近 %d 份）\n",
		map[bool]string{true: "已启用", false: "已停用"}[st.Enabled], st.IntervalHours, st.Keep)
	if st.Enabled && st.NextRunAt != "" {
		if nt, err := time.Parse(time.RFC3339, st.NextRunAt); err == nil {
			s += fmt.Sprintf("下次自动备份：%s\n", nt.Local().Format("2006-01-02 15:04:05"))
		}
	}
	// 到点未执行要单独说清楚：只显示"下次时间"会让人以为一切正常。
	if st.Overdue {
		due := st.DueAt
		if dt, err := time.Parse(time.RFC3339, st.DueAt); err == nil {
			due = dt.Local().Format("2006-01-02 15:04:05")
		}
		s += fmt.Sprintf("⚠ 已到点（%s）但还没执行：运维工具正忙或上次备份失败，正在按 %s 间隔重试\n", due, "5 分钟")
	}
	s += fmt.Sprintf("备份目录：%s（共 %d 个备份）\n", st.BackupDir, fileCount)
	s += "\n=== 服务器运行态 ===\n"
	s += fmt.Sprintf("服务端运行中：%v\n", st.ServerRunning)
	s += fmt.Sprintf("认证服务运行中：%v\n", st.AuthRunning)
	s += fmt.Sprintf("MySQL 模式：%v   连接池可用：%v\n", st.MySQLMode, st.MySQLAlive)
	return s
}

// onBkSave 远程应用自动备份设置。
func (a *App) onBkSave() {
	c := a.snapshotClient()
	if c == nil {
		return
	}
	iv, err := strconv.Atoi(strings.TrimSpace(a.bkIntEdit.Text()))
	if err != nil || iv < 1 || iv > 168 {
		a.showInfo("保存设置", "备份间隔需为 1~168 之间的整数（小时）。")
		return
	}
	keepText := strings.TrimSpace(a.bkKeepEdit.Text())
	keep, kerr := strconv.Atoi(keepText)
	if kerr != nil || keep < 1 || keep > 500 {
		a.showInfo("保存设置", "保留份数需为 1~500 之间的整数（份）：每次备份成功后会自动删掉更旧的备份，防止把服务器磁盘写满。")
		return
	}
	enabled := a.bkChk.Checked()
	if !a.beginOp() {
		a.showInfo("操作进行中", "已有管理操作进行中，请稍候再试")
		return
	}
	defer a.endOp()
	a.bkSaveBtn.SetEnabled(false)
	defer func() { a.sync(func() { a.bkSaveBtn.SetEnabled(true) }) }()

	go func() {
		ctx, cancel := context.WithTimeout(context.Background(), 15*time.Second)
		defer cancel()
		st, err := c.BackupApply(ctx, enabled, iv, keep)
		if err != nil {
			if !a.handleAPIError(err) {
				a.showError("保存设置失败", err)
			}
			return
		}
		a.renderBkState(st, nil)
		a.showInfo("设置已保存", fmt.Sprintf("服务器端自动备份已%s（间隔 %d 小时，保留最近 %d 份）。\n基准时刻已重置为当前时刻，下次自动备份约 %s。",
			map[bool]string{true: "启用", false: "停用"}[enabled], iv, keep, nextBkLabel(st)))
	}()
}

// nextBkLabel 计算下次执行时间可读文本；"到点未执行"时如实说明（否则会误以为一切正常）。
func nextBkLabel(st *BackupStatus) string {
	if !st.Enabled {
		return "-"
	}
	if st.Overdue {
		if dt, err := time.Parse(time.RFC3339, st.DueAt); err == nil {
			return "已到点 " + dt.Local().Format("2006-01-02 15:04") + " 但未执行（正在重试）"
		}
		return "已到点但未执行（正在重试）"
	}
	if st.NextRunAt == "" {
		return "-"
	}
	nt, err := time.Parse(time.RFC3339, st.NextRunAt)
	if err != nil {
		return "-"
	}
	return nt.Local().Format("2006-01-02 15:04")
}

// onBkNow 远程触发一次立即备份（在线 dump，不停止服务端）。
func (a *App) onBkNow() {
	c := a.snapshotClient()
	if c == nil {
		return
	}
	if !a.beginOp() {
		a.showInfo("操作进行中", "已有管理操作进行中，请稍候再试")
		return
	}
	defer a.endOp()
	a.bkNowBtn.SetEnabled(false)
	defer func() { a.sync(func() { a.bkNowBtn.SetEnabled(true) }) }()

	go func() {
		ctx, cancel := context.WithTimeout(context.Background(), 10*time.Minute)
		defer cancel()
		res, err := c.BackupNow(ctx)
		if err != nil {
			if !a.handleAPIError(err) {
				a.showError("立即备份失败", err)
			}
			return
		}
		a.showInfo("立即备份完成", fmt.Sprintf("已生成备份：%s\n表数：%d\n行数：%d\n\n列表已刷新，备份保存在服务器端备份目录。",
			res.ZipName, res.TableCount, res.RowCount))
		a.refreshBkAll(false)
	}()
}

// onBkRestore 还原选中的服务器端备份。
func (a *App) onBkRestore() {
	c := a.snapshotClient()
	if c == nil {
		return
	}
	idx := a.bkList.CurrentIndex()
	if idx < 0 || idx >= len(a.bkFiles) {
		a.showInfo("还原数据库", "请先在备份列表中选中一个备份。")
		return
	}
	file := a.bkFiles[idx]

	// 先取一次服务器端运行态，运行中则提示将自动停服。
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	st, err := c.BackupStatus(ctx)
	if err != nil {
		if !a.handleAPIError(err) {
			a.showError("还原数据库", err)
		}
		return
	}
	msg := fmt.Sprintf("将覆盖还原服务器端数据库：\n%s\n\n备份目录：%s", file, st.BackupDir)
	if st.ServerRunning {
		msg += "\n\n检测到服务端正在运行。\n还原过程将自动停止服务器（认证服务/DNF90Server/MySQL），\n还原完成后不会自动启动，请到「服务器状态」页手动开服。"
	} else {
		msg += "\n\n服务器当前未运行，将直接拉起 MySQL 后还原。"
	}
	if !a.confirm("还原数据库", msg) {
		return
	}
	if !a.beginOp() {
		a.showInfo("操作进行中", "已有管理操作进行中，请稍候再试")
		return
	}
	defer a.endOp()
	a.bkRestoreBtn.SetEnabled(false)
	a.bkRefreshBtn.SetEnabled(false)
	defer func() {
		a.sync(func() {
			a.bkRestoreBtn.SetEnabled(true)
			a.bkRefreshBtn.SetEnabled(true)
		})
	}()

	go func() {
		// 还原含停服/拉起 MySQL/导入，最长给 10 分钟。
		rctx, rcancel := context.WithTimeout(context.Background(), 10*time.Minute)
		defer rcancel()
		res, err := c.BackupRestore(rctx, file)
		if err != nil {
			if !a.handleAPIError(err) {
				a.showError("还原失败", err)
			}
			return
		}
		stoppedTxt := "未触发停服"
		if res.Stopped {
			stoppedTxt = "已自动停止服务端"
		}
		a.showInfo("还原完成", fmt.Sprintf("备份 %s 还原完成。\n%s\n还原表数：%d\n还原行数：%d\n\n服务器当前未运行，如需开服请点击「服务器状态」页的一键启动。",
			res.ZipName, stoppedTxt, res.TableCount, res.RowCount))
		a.refreshBkAll(false)
	}()
}

// humanSize 人类可读字节数。
func humanSize(n int64) string {
	switch {
	case n >= 1<<30:
		return fmt.Sprintf("%.1f GB", float64(n)/(1<<30))
	case n >= 1<<20:
		return fmt.Sprintf("%.1f MB", float64(n)/(1<<20))
	case n >= 1<<10:
		return fmt.Sprintf("%.1f KB", float64(n)/(1<<10))
	default:
		return fmt.Sprintf("%d B", n)
	}
}
