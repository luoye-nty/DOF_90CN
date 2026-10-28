package main

// DNF90 运行维护页 —— 独立标签页（免费版/赞助版共用，无 build tag）。
//
// 两件事：
//  ① 定时清理内存：按间隔回收 DNF90Server.exe / mysqld.exe / 本工具的工作集（EmptyWorkingSet）。
//     页面把"回收前→回收后"的实际字节数写进日志，避免出现"点了没反应"的疑惑。
//  ② 落盘日志与崩溃转储：显示日志文件路径/大小、崩溃日志大小，提供打开目录、
//     立即回收、清理旧日志、导出诊断包等入口。
//
// 为什么这两件事放一页：它们服务同一个场景——"工具闪退/服务器变卡，我要找到原因"。
// 现场只有两个来源：落盘日志 + crash.log。所以页面直接把这两个文件的路径摆在最上面。

import (
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"time"

	"github.com/lxn/walk"
	. "github.com/lxn/walk/declarative"
)

// mainTabWidgets 运行维护页控件与状态。
type mainTabWidgets struct {
	// 内存回收
	memChk      *walk.CheckBox
	memIntEdit  *walk.NumberEdit
	memServerCb *walk.CheckBox
	memMySQLCb  *walk.CheckBox
	memSelfCb   *walk.CheckBox
	memLogAllCb *walk.CheckBox
	memSaveBtn  *walk.PushButton
	memNowBtn   *walk.PushButton
	memStateL   *walk.Label

	// 日志
	logPathL    *walk.Label
	logSizeL    *walk.Label
	crashSizeL  *walk.Label
	openLogBtn  *walk.PushButton
	openCrasBtn *walk.PushButton
	pruneBtn    *walk.PushButton
	exportBtn   *walk.PushButton
	logStateL   *walk.Label
}

var mainW mainTabWidgets

// maintTab 运行维护页控件树。
// 控件初值统一由 main() 在窗口创建完成后调 applyMaintConfig 回填：
// 在声明期赋值会与 config 形成两份事实源，改完配置不容易发现没生效。
func (a *App) maintTab() []Widget {
	return []Widget{
		ScrollView{
			HorizontalFixed: true,
			Layout:          VBox{Margins: Margins{10, 8, 10, 8}, Spacing: 8},
			Children: []Widget{
				GroupBox{
					Title:  "定时清理内存（回收工作集）",
					Layout: VBox{Margins: Margins{10, 8, 10, 8}, Spacing: 6},
					Children: []Widget{
						Label{
							Text: "作用：按间隔调用 Windows EmptyWorkingSet，把目标进程工作集里的闲置物理页交还系统。\r\n" +
								"　　　不重启进程、不断线；但也不会释放进程正在使用的内存——若内存持续单调上涨（真实泄漏），需要的是重启服务端。",
							TextColor: walk.RGB(90, 90, 90),
						},
						Composite{
							Layout: HBox{MarginsZero: true, Spacing: 6},
							Children: []Widget{
								CheckBox{AssignTo: &mainW.memChk, Text: "启用定时回收"},
								Label{Text: "间隔(分钟):"},
								NumberEdit{AssignTo: &mainW.memIntEdit, MinValue: memCleanMinInterval, MaxValue: memCleanMaxInterval, Decimals: 0, Increment: 1, SpinButtonsVisible: true, MinSize: Size{Width: 90, Height: 0}},
							},
						},
						Composite{
							Layout: HBox{MarginsZero: true, Spacing: 12},
							Children: []Widget{
								Label{Text: "回收目标:"},
								CheckBox{AssignTo: &mainW.memServerCb, Text: "DNF90Server"},
								CheckBox{AssignTo: &mainW.memMySQLCb, Text: "便携 MySQL"},
								CheckBox{AssignTo: &mainW.memSelfCb, Text: "本工具"},
								CheckBox{AssignTo: &mainW.memLogAllCb, Text: "每次执行都记日志", ToolTipText: "不勾选时，只在确有回收量（≥1MB）或发生失败时记日志，避免刷屏"},
							},
						},
						Composite{
							Layout: HBox{MarginsZero: true, Spacing: 6},
							Children: []Widget{
								PushButton{AssignTo: &mainW.memSaveBtn, Text: "保存设置", OnClicked: a.onMaintSaveMemory},
								PushButton{AssignTo: &mainW.memNowBtn, Text: "立即回收一次", OnClicked: a.onMaintCleanNow},
								PushButton{Text: "刷新状态", OnClicked: func() { a.refreshMaintStatus() }},
							},
						},
						Label{AssignTo: &mainW.memStateL, Text: "状态：尚未查询", TextColor: walk.RGB(120, 120, 120)},
					},
				},
				GroupBox{
					Title:  "日志与崩溃转储（保密提示：落盘日志写盘前会脱敏）",
					Layout: VBox{Margins: Margins{10, 8, 10, 8}, Spacing: 6},
					Children: []Widget{
						Label{AssignTo: &mainW.logPathL, Text: "日志文件：-"},
						Label{AssignTo: &mainW.logSizeL, Text: "日志大小：-"},
						Label{AssignTo: &mainW.crashSizeL, Text: "崩溃日志：-"},
						Composite{
							Layout: HBox{MarginsZero: true, Spacing: 6},
							Children: []Widget{
								PushButton{AssignTo: &mainW.openLogBtn, Text: "打开日志目录", OnClicked: a.onMaintOpenLogDir},
								PushButton{AssignTo: &mainW.openCrasBtn, Text: "打开 crash.log", OnClicked: a.onMaintOpenCrashLog},
								PushButton{AssignTo: &mainW.pruneBtn, Text: "清理旧日志", OnClicked: a.onMaintPruneLogs, ToolTipText: "按配置的「保留最近 N 个」删掉更早的 ops-*.log；crash.log 不会被删"},
								PushButton{AssignTo: &mainW.exportBtn, Text: "导出诊断包", OnClicked: a.onMaintExportDiagnostics, ToolTipText: "把当前日志、崩溃日志、配置摘要（已脱敏）打包到桌面，便于发人排查"},
							},
						},
						Label{AssignTo: &mainW.logStateL, Text: "", TextColor: walk.RGB(120, 120, 120)},
						Label{
							Text: "脱敏范围：adminToken（adm_*）、数据库口令与 DSN、配对码、internal token、ticket、CDK、32 位以上十六进制串。\r\n" +
								"　　　　GUI 里的日志框仍显示原文（本机排查用），只有落盘文件会被脱敏。",
							TextColor: walk.RGB(90, 90, 90),
						},
					},
				},
			},
		},
	}
}

// applyMaintConfig 把 config 里的内存回收配置回填到控件。
func (a *App) applyMaintConfig() {
	cfg := a.memCleanConfig()
	if mainW.memChk != nil {
		mainW.memChk.SetChecked(cfg.Enabled)
	}
	if mainW.memIntEdit != nil {
		mainW.memIntEdit.SetValue(float64(cfg.IntervalMinutes))
	}
	if mainW.memServerCb != nil {
		mainW.memServerCb.SetChecked(cfg.Server)
	}
	if mainW.memMySQLCb != nil {
		mainW.memMySQLCb.SetChecked(cfg.MySQL)
	}
	if mainW.memSelfCb != nil {
		mainW.memSelfCb.SetChecked(cfg.Self)
	}
	if mainW.memLogAllCb != nil {
		mainW.memLogAllCb.SetChecked(cfg.LogEveryRun)
	}
}

// onMaintSaveMemory 校验并持久化内存回收配置。
func (a *App) onMaintSaveMemory() {
	if a.cfg == nil {
		return
	}
	if a.cfg.MemoryClean == nil {
		a.logf("配置缺少 memory_clean 段，无法保存（请删除 config.json 后重开工具以重建默认段）")
		return
	}
	interval := int(mainW.memIntEdit.Value())
	if interval < memCleanMinInterval || interval > memCleanMaxInterval {
		a.logf("间隔必须在 %d~%d 分钟之间", memCleanMinInterval, memCleanMaxInterval)
		return
	}
	a.cfg.MemoryClean.Enabled = mainW.memChk.Checked()
	a.cfg.MemoryClean.IntervalMinutes = interval
	a.cfg.MemoryClean.Server = mainW.memServerCb.Checked()
	a.cfg.MemoryClean.MySQL = mainW.memMySQLCb.Checked()
	a.cfg.MemoryClean.Self = mainW.memSelfCb.Checked()
	a.cfg.MemoryClean.LogEveryRun = mainW.memLogAllCb.Checked()
	if err := a.cfg.Save(); err != nil {
		a.logf("保存内存回收配置失败: %v", err)
		return
	}
	state := "已关闭"
	if a.cfg.MemoryClean.Enabled {
		state = fmt.Sprintf("已启用，每 %d 分钟一次", interval)
	}
	a.logf("内存回收配置已保存：%s（服务端=%v，MySQL=%v，本工具=%v）", state,
		a.cfg.MemoryClean.Server, a.cfg.MemoryClean.MySQL, a.cfg.MemoryClean.Self)
	a.refreshMaintStatus()
}

// onMaintCleanNow 立即执行一次回收（手工触发，无论有没有回收量都打日志）。
func (a *App) onMaintCleanNow() {
	a.runAsync("立即回收内存", func() error {
		a.runMemClean(a.memCleanConfig(), true)
		return nil
	}, func(err error) {
		if err != nil {
			return
		}
		a.refreshMaintStatus()
	})
}

// refreshMaintStatus 刷新本页状态行。
//
// 说明：这个函数会去 tasklist 枚举进程（拿服务端/MySQL 的工作集），因此只在
// 打开工具时、点「刷新状态」时、保存配置后、手工回收后调用——不放进 3 秒轮询里，
// 否则光是采样就把 CPU 吃掉了。
func (a *App) refreshMaintStatus() {
	if mainW.memStateL == nil {
		return
	}
	status := a.memCleanStatusSnapshot()
	lines := make([]string, 0, 2)
	parts := []string{fmt.Sprintf("启用=%v 间隔=%d分钟", status.Enabled, status.IntervalMin)}
	if !status.LastRun.IsZero() {
		parts = append(parts, fmt.Sprintf("上次=%s（回收 %s）", status.LastRun.Format("15:04:05"), humanBytes(status.LastFreed)))
		parts = append(parts, fmt.Sprintf("下次≈%s", status.NextRun.Format("15:04:05")))
	} else {
		parts = append(parts, "尚未执行过")
	}
	parts = append(parts, fmt.Sprintf("累计回收 %s / %d 次（失败 %d 次）",
		humanBytes(status.TotalFreed), status.RunCount, status.FailCount))
	lines = append(lines, "状态："+strings.Join(parts, " ｜ "))

	memParts := []string{fmt.Sprintf("服务端 %s", humanBytes(status.ServerWS))}
	if status.MySQLWS > 0 {
		memParts = append(memParts, fmt.Sprintf("MySQL %s", humanBytes(status.MySQLWS)))
	}
	memParts = append(memParts, fmt.Sprintf("本工具 %s", humanBytes(status.SelfWS)))
	lines = append(lines, "内存现状："+strings.Join(memParts, " ｜ "))

	mainW.memStateL.SetText(strings.Join(lines, "\r\n"))
	mainW.memStateL.SetTextColor(walk.RGB(60, 90, 140))

	a.refreshMaintLogInfo()
}

// refreshMaintLogInfo 刷新日志路径/大小信息。
func (a *App) refreshMaintLogInfo() {
	if mainW.logPathL == nil {
		return
	}
	if a.fileLog == nil {
		mainW.logPathL.SetText("日志文件：未启用（config.json → log_file.enabled）")
		mainW.logSizeL.SetText("日志大小：-")
	} else {
		mainW.logPathL.SetText("日志文件：" + a.fileLog.Path())
		mainW.logSizeL.SetText(fmt.Sprintf("日志大小：%s（目录 %s）", humanBytes(a.fileLog.Size()), a.fileLog.Dir()))
	}
	crashPath := filepath.Join(a.crashDir(), crashLogName)
	if info, err := os.Stat(crashPath); err == nil {
		mainW.crashSizeL.SetText(fmt.Sprintf("崩溃日志：%s（%s，最后修改 %s）",
			crashPath, humanBytes(info.Size()), info.ModTime().Format("2006-01-02 15:04:05")))
	} else {
		mainW.crashSizeL.SetText("崩溃日志：尚无（" + crashPath + "）—— 没出现过 panic 是好消息")
	}
}

// onMaintOpenLogDir 用资源管理器打开日志目录。
func (a *App) onMaintOpenLogDir() {
	dir := a.crashDir()
	if err := os.MkdirAll(dir, 0o755); err != nil {
		a.logf("打开日志目录失败: %v", err)
		return
	}
	if err := openInExplorer(dir); err != nil {
		a.logf("打开日志目录失败: %v（路径：%s）", err, dir)
		return
	}
	mainW.logStateL.SetText("已打开：" + dir)
}

// onMaintOpenCrashLog 打开 crash.log（不存在时用记事本创建一个空文件并在日志里说明）。
func (a *App) onMaintOpenCrashLog() {
	path := filepath.Join(a.crashDir(), crashLogName)
	if _, err := os.Stat(path); err != nil {
		mainW.logStateL.SetText("还没有崩溃日志：" + path)
		a.logf("还没有崩溃日志（%s）——工具尚未发生过 panic", path)
		return
	}
	if err := openInExplorer(path); err != nil {
		a.logf("打开 crash.log 失败: %v（路径：%s）", err, path)
		return
	}
	mainW.logStateL.SetText("已打开：" + path)
}

// onMaintPruneLogs 立即按「保留最近 N 个」清理旧日志。
func (a *App) onMaintPruneLogs() {
	if a.fileLog == nil {
		a.logf("落盘日志未启用，无需清理")
		return
	}
	before := countOpsLogs(a.fileLog.Dir())
	a.fileLog.mu.Lock()
	a.fileLog.pruneLocked()
	a.fileLog.mu.Unlock()
	after := countOpsLogs(a.fileLog.Dir())
	mainW.logStateL.SetText(fmt.Sprintf("清理完成：%d → %d 个日志文件（crash.log 不参与清理）", before, after))
	a.logf("日志清理完成：%d → %d 个文件（保留最近 %d 个）", before, after, a.cfg.LogFile.KeepFiles)
	a.refreshMaintLogInfo()
}

// onMaintExportDiagnostics 把日志与配置摘要打包到桌面，便于发人排查。
//
// 注意：这里刻意不复用 internal/backup 的 zip 实现——那个包是"数据库备份"语义
// （带 mysqldump/停服逻辑），拿来做诊断包会引入不必要的耦合。
func (a *App) onMaintExportDiagnostics() {
	stamp := time.Now().Format("20060102-150405")
	desktop := desktopDir()
	if desktop == "" {
		desktop = a.crashDir()
	}
	target := filepath.Join(desktop, fmt.Sprintf("dnf90-ops-diag-%s.txt", stamp))

	var builder strings.Builder
	fmt.Fprintf(&builder, "DNF90 运维工具诊断摘要\n生成时间: %s\n\n", time.Now().Format("2006-01-02 15:04:05"))
	fmt.Fprintf(&builder, "== 配置摘要（脱敏）==\n%s\n\n", redactSecrets(a.diagnosticsConfigSummary()))
	fmt.Fprintf(&builder, "== 内存回收状态 ==\n%s\n\n", a.diagnosticsMemorySummary())
	fmt.Fprintf(&builder, "== 日志文件尾部（脱敏，最多 500 行）==\n%s\n", redactSecrets(a.logTail(500)))
	if crashTail := readTail(filepath.Join(a.crashDir(), crashLogName), 64<<10); crashTail != "" {
		fmt.Fprintf(&builder, "\n== crash.log 尾部（脱敏）==\n%s\n", redactSecrets(crashTail))
	}
	if err := os.WriteFile(target, []byte(builder.String()), 0o644); err != nil {
		a.logf("导出诊断包失败: %v", err)
		return
	}
	mainW.logStateL.SetText("诊断摘要已导出：" + target)
	a.logf("诊断摘要已导出（内容已脱敏）：%s", target)
	if err := openInExplorer(target); err != nil {
		a.logf("打开导出的诊断摘要失败: %v", err)
	}
}

// diagnosticsConfigSummary 生成不含凭据的配置摘要。
func (a *App) diagnosticsConfigSummary() string {
	if a == nil || a.cfg == nil {
		return "(无配置)"
	}
	cfg := a.cfg
	lines := []string{
		fmt.Sprintf("server_root=%s", cfg.ServerRoot),
		fmt.Sprintf("database_mode=%s", cfg.DatabaseMode),
		fmt.Sprintf("public_ip=%s", cfg.PublicIP),
		fmt.Sprintf("mysql=%s:%d/%s（口令已省略）", cfg.MySQL.Host, cfg.MySQL.Port, cfg.MySQL.DBName),
		fmt.Sprintf("registration_mode=%s closed=%v max_per_ip=%d", cfg.RegistrationMode, cfg.RegistrationClosed, cfg.RegistrationMaxPerIP),
		fmt.Sprintf("public_listen=%s internal_listen=%s", cfg.Public.ListenAddr, cfg.Internal.ListenAddr),
		fmt.Sprintf("log_max_lines=%d", cfg.LogMaxLines),
	}
	if cfg.Manage != nil {
		lines = append(lines, fmt.Sprintf("manage.enabled=%v listen=%s（配对码已省略）", cfg.Manage.Enabled, cfg.Manage.ListenAddr))
	}
	if cfg.MemoryClean != nil {
		lines = append(lines, fmt.Sprintf("memory_clean.enabled=%v interval=%d server=%v mysql=%v self=%v",
			cfg.MemoryClean.Enabled, cfg.MemoryClean.IntervalMinutes, cfg.MemoryClean.Server, cfg.MemoryClean.MySQL, cfg.MemoryClean.Self))
	}
	if cfg.LogFile != nil {
		lines = append(lines, fmt.Sprintf("log_file.enabled=%v dir=%s max_mb=%d keep=%d redact=%v",
			cfg.LogFile.Enabled, cfg.LogFile.Dir, cfg.LogFile.MaxFileMB, cfg.LogFile.KeepFiles, cfg.LogFile.Redact))
	}
	return strings.Join(lines, "\n")
}

// diagnosticsMemorySummary 内存与版本摘要。
func (a *App) diagnosticsMemorySummary() string {
	status := a.memCleanStatusSnapshot()
	return fmt.Sprintf("服务端工作集=%s MySQL工作集=%s 本工具工作集=%s\n上次回收=%s 累计回收=%s 执行=%d 次 失败=%d 次",
		humanBytes(status.ServerWS), humanBytes(status.MySQLWS), humanBytes(status.SelfWS),
		status.LastRun.Format("2006-01-02 15:04:05"), humanBytes(status.TotalFreed), status.RunCount, status.FailCount)
}

// countOpsLogs 统计目录下 ops-*.log 数量。
func countOpsLogs(dir string) int {
	entries, err := os.ReadDir(dir)
	if err != nil {
		return 0
	}
	count := 0
	for _, entry := range entries {
		if entry.IsDir() {
			continue
		}
		name := entry.Name()
		if strings.HasPrefix(name, "ops-") && strings.HasSuffix(name, ".log") {
			count++
		}
	}
	return count
}

// readTail 读文件尾部最多 maxBytes 字节。
func readTail(path string, maxBytes int64) string {
	file, err := os.Open(path)
	if err != nil {
		return ""
	}
	defer file.Close()
	info, err := file.Stat()
	if err != nil {
		return ""
	}
	size := info.Size()
	offset := int64(0)
	if size > maxBytes {
		offset = size - maxBytes
	}
	buf := make([]byte, size-offset)
	if _, err := file.ReadAt(buf, offset); err != nil && len(buf) > 0 {
		return ""
	}
	return string(buf)
}

// openInExplorer 用资源管理器打开目录或文件。
func openInExplorer(target string) error {
	target = strings.TrimSpace(target)
	if target == "" {
		return fmt.Errorf("路径为空")
	}
	// explorer.exe 对含空格的路径也能正确解析；不加 HideWindow，让它正常显示窗口。
	return exec.Command("explorer", target).Start()
}

// desktopDir 返回当前用户桌面目录（取不到时返回空串）。
func desktopDir() string {
	home, err := os.UserHomeDir()
	if err != nil || home == "" {
		return ""
	}
	candidate := filepath.Join(home, "Desktop")
	if info, err := os.Stat(candidate); err == nil && info.IsDir() {
		return candidate
	}
	return ""
}
