package main

// 数据库备份页（免费版/赞助版均编译）：
//   - 自动备份：按「配置保存时刻」为基准，每隔 N 小时整倍数触发（无视跨天/进程重启），
//     配置落 config.json 的 autobackup 段（enabled / interval_hours / base_at）；
//   - 备份格式与登录器 DbDumpService 对齐：zip（meta.json + dump.sql），仅 MySQL 模式；
//     备份文件保存于本工具 exe 同目录 backups\；
//   - 手动「立即备份」无需停止服务端（在线 dump）；
//   - 还原：先检测服务端运行态 -> 运行中弹窗确认 -> 复用一键停止链路（stopServerAll）
//     关停认证/DNF90Server/MySQL -> 幂等拉起便携 MySQL -> 覆盖还原 -> 完成后弹窗询问
//     是否复用一键启动（startServerAll）。赞助版停服后对管理工具的回执由远程管理桥接层处理。

import (
	"errors"
	"fmt"
	"path/filepath"
	"sort"
	"strconv"
	"strings"
	"time"

	"github.com/lxn/walk"
	. "github.com/lxn/walk/declarative"

	"dnf90ops/internal/backup"
	"dnf90ops/internal/config"
	"dnf90ops/internal/serverdir"
)

var errRestoreCancelled = errors.New("还原已取消")

// ---------- 自动备份配置读写（后台 goroutine 与 UI 线程共享 cfg.AutoBackup） ----------

func (a *App) backupCfg() config.AutoBackupConfig {
	a.abCfgMu.RLock()
	defer a.abCfgMu.RUnlock()
	ab := a.cfg.AutoBackup
	if ab == nil {
		return config.AutoBackupConfig{Enabled: false, IntervalHours: 1}
	}
	return *ab
}

// mutateAutoBackup 在 UI 线程内修改 AutoBackup 并落盘（Save 与其它页面同线程串行）。
func (a *App) mutateAutoBackup(fn func(ab *config.AutoBackupConfig)) {
	a.uiSafe(func() {
		a.abCfgMu.Lock()
		defer a.abCfgMu.Unlock()
		if a.cfg.AutoBackup == nil {
			a.cfg.AutoBackup = &config.AutoBackupConfig{Enabled: false, IntervalHours: 1}
		}
		fn(a.cfg.AutoBackup)
		if err := a.cfg.Save(); err != nil {
			a.logf("保存自动备份配置失败: %v", err)
		}
	})
}

// backupDir 备份目录 = exe 同目录 backups\
func (a *App) backupDir() string {
	return backup.DefaultBackupDir(filepath.Dir(a.cfg.Path()))
}

// ulog 供后台 goroutine 打印全局日志（切到 UI 线程）。
func (a *App) ulog(format string, args ...interface{}) {
	a.uiSafe(func() { a.logf(format, args...) })
}

// ---------- 页面构建 ----------

func (a *App) backupTab() []Widget {
	ab := a.backupCfg()
	nextText := "未启用自动备份"
	if ab.Enabled && ab.BaseAt != "" {
		if base, err := time.Parse(time.RFC3339, ab.BaseAt); err == nil {
			// 到点但还没执行（工具忙/备份失败重试中）时，下一个"到点时刻"可能已经过去，
			// 这时如实显示"已到期待执行"，而不是硬算一个未来时间骗人。
			if slot, due := dueAutoBackupSlot(ab, time.Now()); due {
				nextText = "已到点（" + slot.Format("2006-01-02 15:04") + "）尚未执行，正在重试" +
					fmt.Sprintf("（每 %d 小时一次）", ab.IntervalHours)
			} else {
				nextText = "下次自动备份约 " + backup.NextRunAt(base, ab.IntervalHours, time.Now()).Format("2006-01-02 15:04") +
					fmt.Sprintf("（此后每 %d 小时一次，最多保留 %d 份）", ab.IntervalHours, ab.Keep)
			}
		}
	}
	return []Widget{
		Composite{
			Layout: VBox{Margins: Margins{12, 10, 12, 10}, Spacing: 8},
			Children: []Widget{
				GroupBox{
					Title:  "自动备份设置（仅 MySQL 模式）",
					Layout: VBox{Margins: Margins{10, 8, 10, 8}, Spacing: 6},
					Children: []Widget{
						CheckBox{
							AssignTo: &a.backupChk,
							Text:     "启用自动备份",
							Checked:  ab.Enabled,
						},
						Composite{
							Layout: HBox{MarginsZero: true, Spacing: 8},
							Children: []Widget{
								Label{Text: "间隔（小时）:"},
								LineEdit{
									AssignTo: &a.backupIntEdit,
									Text:     strconv.Itoa(ab.IntervalHours),
									MaxSize:  Size{Width: 70, Height: 0},
								},
								Label{Text: "小时（1~168）。从点击「保存设置」的时刻起算，到点自动备份；进程重启不丢失相位。"},
							},
						},
						Composite{
							Layout: HBox{MarginsZero: true, Spacing: 8},
							Children: []Widget{
								Label{Text: "保留份数:"},
								LineEdit{
									AssignTo: &a.backupKeepEdt,
									Text:     strconv.Itoa(ab.Keep),
									MaxSize:  Size{Width: 70, Height: 0},
								},
								Label{
									Text:      fmt.Sprintf("份（1~%d）。每次备份成功后自动删掉更旧的备份，防止把磁盘写满。", config.AutoBackupKeepMax),
									TextColor: walk.RGB(90, 90, 90),
								},
							},
						},
						Composite{
							Layout: HBox{MarginsZero: true, Spacing: 8},
							Children: []Widget{
								PushButton{Text: "保存设置", OnClicked: a.onBackupSave},
								Label{
									AssignTo:  &a.backupNextL,
									Text:      nextText,
									TextColor: walk.RGB(32, 100, 32),
								},
							},
						},
					},
				},
				GroupBox{
					Title:  "备份目录",
					Layout: VBox{Margins: Margins{10, 8, 10, 8}, Spacing: 6},
					Children: []Widget{
						Label{
							AssignTo:  &a.backupDirL,
							Text:      a.backupDir(),
							TextColor: walk.RGB(90, 90, 90),
						},
					},
				},
				GroupBox{
					Title:  "手动备份",
					Layout: VBox{Margins: Margins{10, 8, 10, 8}, Spacing: 6},
					Children: []Widget{
						Composite{
							Layout: HBox{MarginsZero: true, Spacing: 8},
							Children: []Widget{
								PushButton{Text: "立即备份", OnClicked: a.onBackupNow},
								Label{Text: "立即导出一份当前数据库备份（无需停止服务端；自动备份执行中也可用）。"},
							},
						},
					},
				},
				GroupBox{
					Title:  "备份列表与还原",
					Layout: VBox{Margins: Margins{10, 8, 10, 8}, Spacing: 6},
					Children: []Widget{
						ListBox{
							AssignTo: &a.backupList,
							MinSize:  Size{Width: 0, Height: 130},
						},
						Composite{
							Layout: HBox{MarginsZero: true, Spacing: 8},
							Children: []Widget{
								PushButton{Text: "刷新列表", OnClicked: a.onBackupRefresh},
								PushButton{Text: "还原选中备份...", OnClicked: a.onBackupRestore},
							},
						},
						Label{
							Text:      "还原说明：检测到服务端正在运行时，会先弹窗确认并自动执行「一键停止」（认证/DNF90Server/MySQL），随后覆盖还原所选备份；完成后询问是否启动服务端。还原属覆盖操作，请谨慎选择。",
							TextColor: walk.RGB(150, 60, 0),
						},
					},
				},
			},
		},
	}
}

// ---------- 设置保存 ----------

func (a *App) onBackupSave() {
	s := strings.TrimSpace(a.backupIntEdit.Text())
	n, err := strconv.Atoi(s)
	if err != nil || n < 1 || n > config.AutoBackupIntervalMax {
		walk.MsgBox(a.mw, "自动备份设置", fmt.Sprintf("间隔需为 1~%d 的整数（小时）。", config.AutoBackupIntervalMax), walk.MsgBoxIconError)
		return
	}
	keepText := strings.TrimSpace(a.backupKeepEdt.Text())
	keep, err := strconv.Atoi(keepText)
	if err != nil || keep < 1 || keep > config.AutoBackupKeepMax {
		walk.MsgBox(a.mw, "自动备份设置", fmt.Sprintf("保留份数需为 1~%d 的整数（份）。", config.AutoBackupKeepMax), walk.MsgBoxIconError)
		return
	}
	enabled := a.backupChk.Checked()
	now := time.Now()
	a.mutateAutoBackup(func(ab *config.AutoBackupConfig) {
		ab.Enabled = enabled
		ab.IntervalHours = n
		ab.Keep = keep
		ab.BaseAt = now.Format(time.RFC3339)
	})
	if enabled {
		a.backupNextL.SetText("已保存。下次备份约 " + now.Add(time.Duration(n)*time.Hour).Format("2006-01-02 15:04") +
			fmt.Sprintf("（此后每 %d 小时一次，最多保留 %d 份）", n, keep))
		a.ulog("自动备份已启用：间隔 %d 小时，保留 %d 份，基准时刻 %s，下次约 %s", n, keep, now.Format("15:04:05"), now.Add(time.Duration(n)*time.Hour).Format("15:04"))
	} else {
		a.backupNextL.SetText("自动备份已关闭")
		a.ulog("自动备份已关闭")
	}
	// 顺手按新上限清理一次，避免"调小保留份数后旧文件一直躺着"。
	a.pruneBackups()
	a.refreshBackupList()
}

// ---------- 列表 ----------

func (a *App) refreshBackupList() {
	entries, err := backup.ListBackups(a.backupDir())
	if err != nil {
		a.ulog("读取备份列表失败: %v", err)
		return
	}
	sort.Slice(entries, func(i, j int) bool { return entries[i].ModTime.After(entries[j].ModTime) })
	displays := make([]string, 0, len(entries))
	files := make([]string, 0, len(entries))
	for _, e := range entries {
		displays = append(displays, e.ModTime.Format("2006-01-02 15:04:05")+"   "+humanSize(e.Size)+"   "+e.Name)
		files = append(files, e.File)
	}
	a.backupFiles = files
	if len(displays) == 0 {
		a.backupList.SetEnabled(false)
		a.backupList.SetModel([]string{"（暂无备份，请先执行一次备份）"})
		return
	}
	a.backupList.SetEnabled(true)
	a.backupList.SetModel(displays)
	a.backupList.SetCurrentIndex(-1)
}

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

func (a *App) onBackupRefresh() {
	a.refreshBackupList()
}

// ---------- 手动备份 ----------

func (a *App) onBackupNow() {
	a.runAsync("立即备份数据库", func() error {
		// 手动备份是"我现在就要一份"，所以允许顺手把便携 MySQL 拉起来
		// （与还原路径同一个 EnsureDatabaseReady）；自动备份不这么做，见 doAutoBackup。
		if err := a.ensureDatabaseForBackup(); err != nil {
			return err
		}
		mcfg, err := a.mysqlFromInstance()
		if err != nil {
			return err
		}
		res, err := backup.DumpMySQL(mcfg, a.backupDir(), func(line string) { a.ulog("%s", line) })
		if err != nil {
			return err
		}
		a.ulog("备份完成：%s（%d 张表，%d 行）", filepath.Base(res.ZipPath), res.TableCount, res.RowCount)
		a.pruneBackups()
		return nil
	}, func(err error) {
		a.refreshBackupList()
		if err != nil {
			walk.MsgBox(a.mw, "立即备份", "备份失败:\n"+err.Error(), walk.MsgBoxIconError)
		} else {
			a.logf("手动备份完成")
		}
	})
}

// ensureDatabaseForBackup 备份前确保数据库可连：连不上就按「还原」那条链路
// 幂等拉起便携 MySQL（数据已就绪时是秒级空操作）。
//
// 为什么手动备份要这么做：以前直接 DumpMySQL，服务端停着（便携 MySQL 也没起）时
// 只会弹一句"连接 MySQL 失败"，用户得自己去"一键启动"先把库拉起来。
func (a *App) ensureDatabaseForBackup() error {
	root := a.currentServerRoot()
	if root == "" {
		return errors.New("未选择服务端目录")
	}
	a.ulog("备份前检查数据库连接（必要时拉起便携 MySQL）...")
	if err := serverdir.EnsureDatabaseReady(root, "mysql", func(line string) { a.ulog("%s", line) }, 180*time.Second); err != nil {
		return fmt.Errorf("启动便携 MySQL 失败: %v", err)
	}
	return nil
}

// ---------- 还原 ----------

func (a *App) onBackupRestore() {
	idx := a.backupList.CurrentIndex()
	if idx < 0 || idx >= len(a.backupFiles) {
		walk.MsgBox(a.mw, "还原数据库", "请先在列表中选中一个备份。", walk.MsgBoxIconInformation)
		return
	}
	zipFile := a.backupFiles[idx]
	root := strings.TrimSpace(a.rootEdit.Text())
	if root == "" {
		walk.MsgBox(a.mw, "还原数据库", "请先在一键启动页选择服务端根目录。", walk.MsgBoxIconInformation)
		return
	}
	a.runAsync("还原数据库", func() error {
		return a.restoreWorker(zipFile, root)
	}, func(err error) {
		a.refreshBackupList()
		if err != nil {
			if errors.Is(err, errRestoreCancelled) {
				a.logf("还原已取消")
				return
			}
			a.logf("还原失败: %v", err)
			walk.MsgBox(a.mw, "还原数据库", "还原失败:\n"+err.Error(), walk.MsgBoxIconError)
			return
		}
		a.logf("还原完成，询问是否启动服务端...")
		if walk.MsgBox(a.mw, "还原数据库", "数据库还原完成。是否立即启动服务端？", walk.MsgBoxYesNo) == walk.DlgCmdYes {
			a.onStart()
		} else {
			a.logf("还原完成，用户选择暂不启动服务端")
		}
	})
}

// restoreWorker 在后台 goroutine 中执行还原（runAsync fn）：
// 运行中 -> 确认 -> stopServerAll（复用一键停止链路）-> 幂等拉起 MySQL -> RestoreMySQL。
func (a *App) restoreWorker(zipFile, root string) error {
	ins := serverdir.Inspect(root)
	pmRun, _ := a.pm.Status()
	running := ins.Running || pmRun || a.svcOn.Load()
	if running {
		confirmed := false
		a.mw.Synchronize(func() {
			confirmed = walk.MsgBox(a.mw, "还原数据库",
				"检测到服务端正在运行。\n还原前将自动执行「一键停止」（认证服务/DNF90Server/MySQL）。\n\n是否继续？",
				walk.MsgBoxYesNo) == walk.DlgCmdYes
		})
		if !confirmed {
			return errRestoreCancelled
		}
		a.ulog("正在关闭服务端（认证服务/DNF90Server/MySQL）...")
		if err := a.stopServerAll(); err != nil {
			return fmt.Errorf("关闭服务端失败: %v", err)
		}
		a.ulog("服务端已停止，开始还原数据库...")
	} else {
		a.ulog("服务端当前未运行，直接开始还原数据库...")
	}
	// 停止链路会停掉 mysqld，这里幂等拉起便携 MySQL（未停过则为纯补启动）。
	if err := serverdir.EnsureDatabaseReady(root, "mysql", func(line string) {
		a.ulog("%s", line)
	}, 180*time.Second); err != nil {
		return fmt.Errorf("启动便携 MySQL 失败: %v", err)
	}
	mcfg, err := a.mysqlFromInstance()
	if err != nil {
		return err
	}
	res, err := backup.RestoreMySQL(mcfg, zipFile, func(line string) { a.ulog("%s", line) })
	if err != nil {
		return err
	}
	a.ulog("还原完成：%s（%d 张表，%d 行）", filepath.Base(zipFile), res.TableCount, res.RowCount)
	return nil
}

// ---------- 自动备份循环（进程常驻，与 UI 生命周期一致） ----------

// autoBackupRetry 到点却没跑成（工具忙 / 备份失败）时的重试间隔。
//
// 为什么需要重试：早先的实现是"先推进基准、再判断能不能跑"，
// 于是工具正好在到点那一刻忙、或者数据库没起来，这一次备份就被永久吞掉了 ——
// 下次机会要再等一整个 interval（默认 1 小时，配 24 小时就是一天）。
// 现在改成"跑成功才推进基准"，配合这个节流避免每 20 秒刷屏重试。
const autoBackupRetry = 5 * time.Minute

// autoBackupAction 一次 tick 该做什么。
type autoBackupAction int

const (
	autoBackupIdle     autoBackupAction = iota // 没到点 / 未启用 / 正在节流
	autoBackupInitBase                         // 首次启用（没有基准）：写入基准时刻
	autoBackupRun                              // 到点，执行备份
)

// dueAutoBackupSlot 求"这次该执行的到点时刻"：BaseAt + k*interval 中不晚于 now 的最大者。
// due=false 表示还没到点（含基准在未来这种时钟回拨/换机的情况）。
func dueAutoBackupSlot(ab config.AutoBackupConfig, now time.Time) (time.Time, bool) {
	if ab.IntervalHours <= 0 {
		return time.Time{}, false
	}
	base, err := time.Parse(time.RFC3339, strings.TrimSpace(ab.BaseAt))
	if err != nil {
		return time.Time{}, false
	}
	step := time.Duration(ab.IntervalHours) * time.Hour
	if step <= 0 || !now.After(base) {
		return time.Time{}, false
	}
	steps := int64(now.Sub(base) / step)
	if steps < 1 {
		return time.Time{}, false
	}
	return base.Add(time.Duration(steps) * step), true
}

// decideAutoBackup 纯函数决策：给定配置、当前时间与上次尝试时间，返回本次动作与到点时刻。
// 抽出来是为了能写单测 —— 这段逻辑早先出的正是"到点没跑成被吞掉"的问题。
func decideAutoBackup(ab config.AutoBackupConfig, now, lastAttempt time.Time) (autoBackupAction, time.Time) {
	if !ab.Enabled {
		return autoBackupIdle, time.Time{}
	}
	if strings.TrimSpace(ab.BaseAt) == "" {
		return autoBackupInitBase, time.Time{}
	}
	slot, due := dueAutoBackupSlot(ab, now)
	if !due {
		return autoBackupIdle, time.Time{}
	}
	// 节流：上一次尝试（无论成败）太近就不再重试，避免每 20s 打一次日志/连一次库。
	if !lastAttempt.IsZero() && now.Sub(lastAttempt) < autoBackupRetry {
		return autoBackupIdle, slot
	}
	return autoBackupRun, slot
}

func (a *App) autoBackupLoop() {
	// 启动后延迟 3 秒检查一次（等 UI 就绪），随后每 20s 轮询到点触发。
	time.Sleep(3 * time.Second)
	a.tickAutoBackup()
	t := time.NewTicker(20 * time.Second)
	defer t.Stop()
	for range t.C {
		a.tickAutoBackup()
	}
}

func (a *App) tickAutoBackup() {
	ab := a.backupCfg()
	now := time.Now()
	a.abMu.Lock()
	lastAttempt := a.abLastAttempt
	running := a.abRunning
	a.abMu.Unlock()
	if running {
		return
	}
	action, slot := decideAutoBackup(ab, now, lastAttempt)
	switch action {
	case autoBackupIdle:
		return
	case autoBackupInitBase:
		// 首次启用没有基准：以当前时刻为基准落盘，下一周期开始计算。
		a.mutateAutoBackup(func(ab *config.AutoBackupConfig) {
			ab.BaseAt = now.Format(time.RFC3339)
		})
		return
	}

	// 到点：工具忙就先不动基准（保持"待执行"），下个 tick 继续试。
	if a.busy.Load() {
		a.abMu.Lock()
		a.abLastAttempt = now
		a.abMu.Unlock()
		a.ulog("自动备份到点但工具正忙（有手动任务执行中），本次不执行，%s 后重试", autoBackupRetry)
		return
	}
	a.abMu.Lock()
	a.abRunning = true
	a.abLastAttempt = now
	a.abMu.Unlock()
	go func() {
		defer func() {
			a.abMu.Lock()
			a.abRunning = false
			a.abMu.Unlock()
		}()
		a.doAutoBackup(slot)
	}()
}

// doAutoBackup 执行一次自动备份。成功才推进基准：失败/忙都不吞掉这个到点时刻。
func (a *App) doAutoBackup(slot time.Time) {
	if a.busy.Load() {
		a.ulog("自动备份执行前检测到工具正忙，本次不执行，%s 后重试", autoBackupRetry)
		return
	}
	mcfg, err := a.mysqlFromInstance()
	if err != nil {
		a.ulog("自动备份失败（读取数据库配置）: %v；已保留本次待执行，%s 后重试", err, autoBackupRetry)
		return
	}
	// 自动备份**不会**主动拉起数据库：那会在服务端停机维护时凭空起一个 mysqld。
	// 数据库不可用时留待下个周期，等它恢复后自动补上。
	res, err := backup.DumpMySQL(mcfg, a.backupDir(), func(line string) { a.ulog("%s", line) })
	if err != nil {
		a.ulog("自动备份失败: %v；已保留本次待执行，%s 后重试", err, autoBackupRetry)
		return
	}
	// 成功才推进基准（用本次到点时刻，避免"补跑"把相位越推越后）。
	a.mutateAutoBackup(func(ab *config.AutoBackupConfig) {
		ab.BaseAt = slot.Format(time.RFC3339)
	})
	a.ulog("自动备份完成：%s（%d 张表，%d 行）", filepath.Base(res.ZipPath), res.TableCount, res.RowCount)
	a.pruneBackups()
	a.uiSafe(func() { a.refreshBackupList() })
}

// pruneBackups 成功备份后按「保留份数」清理旧备份（GUI/远程/自动三条路共用）。
func (a *App) pruneBackups() {
	keep := a.backupCfg().Keep
	removed, err := backup.PruneBackups(a.backupDir(), keep)
	if len(removed) > 0 {
		a.ulog("已清理 %d 个旧备份（保留最近 %d 份）：%s", len(removed), keep, strings.Join(removed, ", "))
	}
	if err != nil {
		a.ulog("清理旧备份出错: %v", err)
	}
}
