// DNF90 运维工具：S4OpsTool 风格 TabWidget 重构版。

//

// 页面一「一键启动」：服务端根目录选择 → 结构/初始化检测 → 一键初始化（调用原版

// DNF90Control database select --prepare）→ 一键启停（env 直启 DNF90Server.exe，

// 不走 DNF90Control start，避免 public_addr 被写回内网；公网下发以

// DNFBRIDGE_SERVER_IP env 为准）。

// 页面二「认证服务」：保留原登录器认证 API（公网 HTTPS 18112 + 内部回调查验），

// 供异环登录器验证登录使用。

package main

import (
	"context"

	"fmt"

	"log/slog"

	"os"

	"os/exec"

	"path/filepath"

	"strconv"

	"strings"

	"sync"

	"sync/atomic"

	"syscall"

	"time"

	"dnf90ops/internal/config"

	"dnf90ops/internal/ops"

	"dnf90ops/internal/pvfindex"

	"dnf90ops/internal/server"

	"dnf90ops/internal/serverdir"

	"dnf90ops/internal/store"

	"github.com/lxn/walk"

	. "github.com/lxn/walk/declarative"

	"github.com/lxn/win"
)

// App 主窗口状态。

type App struct {
	cfg *config.Config

	pm *ops.ProcessManager

	svc *server.Service

	svcCancel context.CancelFunc

	st *store.Store

	svcOn atomic.Bool // 认证服务运行中（loopRefresh 后台协程读、UI 线程写）

	busy atomic.Bool // 有后台任务进行中（UI 线程与多个后台协程并发读写，必须原子）

	logMu sync.Mutex

	logBuf []string

	logDirty int

	// ---- UI ----

	mw *walk.MainWindow

	statusL *walk.Label

	logEdit *walk.TextEdit

	rootEdit *walk.LineEdit

	ipEdit *walk.LineEdit

	initBtn *walk.PushButton

	startBtn *walk.PushButton

	stopBtn *walk.PushButton

	refreshBtn *walk.PushButton

	serverBtn *walk.PushButton

	authBtn *walk.PushButton

	authStateL *walk.Label

	// ---- 远程管理（paid：remote_tab.go；免费版仅保留字段不挂 UI）----
	manageChk        *walk.CheckBox // 远程管理开关
	manageHintL      *walk.Label    // 页面提示行
	manageCodeBox    *walk.LineEdit // 当前配对码明文（只读可复制）
	manageListenL    *walk.Label    // 管理 API 监听地址
	manageCertL      *walk.Label    // 证书状态
	manageCustomEdit *walk.LineEdit // 自定义配对码输入

	// ---- pointsink (pointsink.go) ----
	psOnce  sync.Once
	psMu    sync.Mutex
	psCfgMu sync.Mutex // guards cfg.Pointsink fields (psConf read / psApply write)

	// ---- 在线福利 (welfare.go) ----
	wfOnce sync.Once // 保证调度循环只启动一次
	// wfSkipMu / wfSkipUntil 在线福利"前置失败后的重试冷却"（kind -> 最早可重试时刻）。
	//
	// 为什么需要：调度循环 30 秒一跳，前置失败（认证服务没起来等）如果直接重试会刷屏；
	// 但旧行为是**直接把本期标记成已发**，等于一次瞬时故障就吃掉当天整档福利。
	// 这里取中间：失败不标记 + 冷却 5 分钟后重试。见 welfareNoteSkipped。
	wfSkipMu    sync.Mutex
	wfSkipUntil map[string]time.Time
	ps          psState

	// ---- 数据库备份页 (backup_tab.go) ----
	abMu          sync.Mutex   // 自动备份执行互斥
	abRunning     bool         // 自动备份执行中
	abLastAttempt time.Time    // 上次尝试自动备份的时刻（成败都算，用于失败重试节流）
	abCfgMu       sync.RWMutex // guards cfg.AutoBackup（UI/后台共享）
	backupChk     *walk.CheckBox
	backupIntEdit *walk.LineEdit
	backupKeepEdt *walk.LineEdit
	backupNextL   *walk.Label
	backupDirL    *walk.Label
	backupList    *walk.ListBox
	backupFiles   []string

	// ---- 落盘日志 (logfile.go)：闪退排查的唯一凭据 ----
	fileLog *fileLogger
}

// noisyServiceLog 判定高频服务噪音日志（**只**从 GUI 日志框里抑制，仍然照常落盘）。
//
// 为什么只抑制 GUI：这些行是"每次请求/每次连接探测都打一条"的纯噪音，
// 全塞进 EDIT 控件会把界面刷爆；但闪退排查恰恰需要"所有日志"，所以文件日志一条不少。
func noisyServiceLog(line string) bool {
	// dnfbridge 每次 channel 建立/关闭都会打一条 INFO，属于纯连接噪音。
	if strings.Contains(line, "service=dnfbridge") && strings.Contains(line, "channel connection") {
		return true
	}
	// 管理接口逐请求审计：运维工具自己的回环调用（/local/dnf-gm/*）每次操作都刷一行，
	// 管理工具点一下按钮就是十几行，界面全被它占满。
	if strings.Contains(line, `msg="admin request"`) {
		return true
	}
	// 登录器的在线探测（连一下就断）会让服务端刷 unexpected EOF / 帧读取失败，
	// 玩家开着登录器时几乎停不下来。属于探测副作用，不进界面（文件里仍可查）。
	if strings.Contains(line, "unexpected EOF") || strings.Contains(line, "auth-frame-read-failed") {
		return true
	}
	return false
}

// ================= 日志（内存缓冲 + UI 线程批量刷新） =================
//
// 为什么必须这么改（实测数据）：
//
//	Win32 EDIT 控件（walk TextEdit）每行 AppendText 的耗时随内容增长而劣化——
//	空控件 0.086 ms/行，10 万行（约 14MB）时 0.488 ms/行，约 5 倍。
//	旧实现每来一行就 AppendText + WM_VSCROLL/SB_BOTTOM，而 log_max_lines 配置从未生效、
//	EM_SETLIMITTEXT 又被放开到 2GB，日志无上限增长：服务端日志洪峰时 UI 线程被控件写入吃满，
//	消息队列堆积，整个界面"一卡一卡"。
//	另外 logf 有大量调用点位于后台协程，直接跨线程写控件会走 Win32 SendMessage 编组并阻塞调用方。
//
// 现在：logf 只往内存环形缓冲追加（加锁、O(1)、不碰控件），后台每 300ms 在 UI 线程用
// SetText 一次性刷出尾部 N 行。控件内容恒定在 N 行以内 → 单次写入成本恒定，
// 且任何协程都可以安全调用 logf（不再需要 uiSafe 包一层）。
const (
	defaultLogLines = 2000                   // 与 config.LogMaxLines 默认值一致
	logFlushEvery   = 300 * time.Millisecond // 刷新间隔：兼顾实时感与开销
	logEditMaxChars = 1 << 20                // 日志框字符上限（有 N 行上限后绰绰有余，避免 EDIT 病态行为）
)

// logCap 日志保留行数（可在 config 里调，非法值回退默认）。
func (a *App) logCap() int {
	if a.cfg != nil && a.cfg.LogMaxLines > 0 {
		return a.cfg.LogMaxLines
	}
	return defaultLogLines
}

// logf 追加一行日志到内存缓冲。线程安全、非阻塞，可从任意协程调用。
// 同时写入落盘日志（logfile.go）：GUI 缓冲关掉工具就没了，落盘文件才是闪退排查的依据。
//
// 噪音行只从 GUI 抑制，**不**从文件抑制：闪退排查要的是"所有日志"，
// 少一类连接噪音就可能少掉定位问题的那一行。
func (a *App) logf(format string, args ...interface{}) {
	line := fmt.Sprintf(format, args...)

	stamped := "[" + time.Now().Format("15:04:05") + "] " + line

	if !noisyServiceLog(line) {
		limit := a.logCap()

		a.logMu.Lock()

		a.logBuf = append(a.logBuf, stamped)

		// 缓冲也设上限（保留 2 倍行数），防止洪峰期内存无限膨胀
		if over := len(a.logBuf) - limit*2; over > 0 {

			a.logBuf = append([]string(nil), a.logBuf[over:]...)

		}

		a.logDirty++

		a.logMu.Unlock()
	}

	// 落盘走独立锁，与上面内存缓冲互不阻塞：磁盘慢也不能卡住调用 logf 的业务协程。
	if fl := a.fileLog; fl != nil {
		fl.Write("INFO", line)
	}

}

// logLoop 周期性把日志缓冲刷到控件（在主线程执行，见 main 里的 go a.logLoop()）。
func (a *App) logLoop() {
	_ = a.guardGoroutine("日志刷新循环", func() {
		t := time.NewTicker(logFlushEvery)
		defer t.Stop()
		for range t.C {
			a.flushLogs()
		}
	})
}

// flushLogs 把主日志与账号页日志的缓冲一次性刷到各自控件。
func (a *App) flushLogs() {
	a.flushMainLog()
	flushAcctLog()
	flushInvLog()
}

func (a *App) flushMainLog() {
	a.logMu.Lock()
	dirty := a.logDirty
	a.logDirty = 0
	lines := a.logBuf
	if n := a.logCap(); len(lines) > n {
		lines = lines[len(lines)-n:]
	}
	text := ""
	if dirty > 0 {
		text = strings.Join(lines, "\r\n")
	}
	a.logMu.Unlock()

	if text == "" {

		return

	}

	a.uiSafe(func() {

		if a.logEdit == nil {

			return

		}

		a.logEdit.SetText(text + "\r\n")

		if h := a.logEdit.Handle(); h != 0 {

			win.SendMessage(h, win.WM_VSCROLL, win.SB_BOTTOM, 0)

		}

	})

}

// isBusy / tryBusy / clearBusy：busy 由 UI 线程与多个后台协程并发读写，统一走原子访问。
func (a *App) isBusy() bool { return a.busy.Load() }

func (a *App) tryBusy() bool { return a.busy.CompareAndSwap(false, true) }

func (a *App) clearBusy() { a.busy.Store(false) }

func main() {

	exeDir, err := os.Executable()

	if err != nil || filepath.Dir(exeDir) == "" {

		exeDir = "."

	}

	cfgPath := filepath.Join(filepath.Dir(exeDir), "config.json")

	cfg, err := config.Load(cfgPath)

	if err != nil {

		walk.MsgBox(nil, "DNF90运维工具", "配置加载失败: "+err.Error(), walk.MsgBoxIconError)

		return

	}

	a := &App{cfg: cfg, pm: ops.NewProcessManager(nil)}

	// PVF 索引 DFIX 落盘缓存目录：程序同目录 pvf\（按 PVF 指纹分文件）。
	pvfindex.SetCacheDir(filepath.Join(filepath.Dir(cfgPath), "pvf"))

	// 落盘日志器：必须在第一行 logf 之前就绪，否则最早那几行（版本/配置加载）进不了文件。
	// 创建失败（目录不可写等）时降级为"只有 GUI 日志"，不阻断启动。
	a.fileLog = newFileLogger(cfg.LogFile, filepath.Dir(cfgPath))

	// 把 slog 接进工具日志：internal/{server,manage,backup} 全部用 slog，
	// 而 windowsgui 构建没有 console，默认的 stderr 输出会被丢弃——不接这一步，
	// 认证服务/管理服务/备份的 Info/Error 一行都看不到也留不下（见 logbridge.go）。
	slog.SetDefault(slog.New(newAppLogHandler(a)))

	// 顶层 panic 兜底：MainWindow.Run 的 UI 消息循环一旦 panic，windowsgui 构建下进程会静默消失。
	// 这里必须放在 Run 之前注册，且 defer 里的恢复逻辑自己也要 recover，避免二次 panic。
	defer func() {
		if r := recover(); r != nil {
			func() {
				defer func() { _ = recover() }()
				a.writeCrashLog("main", r, debugStack())
				a.fileLog.Close()
			}()
		}
	}()

	a.logf("== DNF90 运维工具 ==")
	if a.fileLog != nil {
		a.logf("落盘日志已开启：%s（单文件上限 %dMB，保留最近 %d 个，脱敏=%v）",
			a.fileLog.Path(), cfg.LogFile.MaxFileMB, cfg.LogFile.KeepFiles, cfg.LogFile.Redact)
	} else {
		a.logf("警告：落盘日志不可用（日志目录不可写），闪退时将无法留下现场")
	}

	var mw *walk.MainWindow

	a.mw = mw

	if err := (MainWindow{

		AssignTo: &mw,

		Title:   "DNF90 运维工具",
		Visible: false, // 赞助版需先通过在线授权验证再显示；免费版紧随其后放行

		MinSize: Size{Width: 860, Height: 620},

		Size: Size{Width: 960, Height: 700},

		Layout: VBox{MarginsZero: true, SpacingZero: true},

		Children: []Widget{

			Composite{

				Layout: VBox{Margins: Margins{8, 6, 8, 6}, Spacing: 6},

				Children: []Widget{

					TabWidget{

						StretchFactor: 1,
						Pages:         a.mainPages(),
					},

					Label{Text: "运行日志："},

					TextEdit{

						AssignTo: &a.logEdit, ReadOnly: true,

						VScroll: true, HScroll: true,

						MinSize: Size{Width: 0, Height: 230},

						MaxSize: Size{Width: 0, Height: 380},

						Font: Font{Family: "Consolas", PointSize: 9},
					},

					Label{AssignTo: &a.statusL, Text: "就绪", TextColor: walk.RGB(32, 100, 32)},
				},
			},
		},
	}).Create(); err != nil {

		slog.Error("create main window failed", "err", err)

		return

	}

	a.mw = mw

	// 日志框默认 EDIT 控件有 32K 字符上限，超出后新日志不再显示；放开到系统允许的最大值。
	win.SendMessage(a.logEdit.Handle(), win.EM_SETLIMITTEXT, logEditMaxChars, 0)

	a.mw.SetIcon(walk.IconApplication())

	a.mw.Closing().Attach(func(canceled *bool, reason walk.CloseReason) {

		if a.busy.Load() {

			a.logf("有任务执行中，等待结束后再退出")

			*canceled = true

			return

		}

		a.logf("正在退出，停止子进程...")

		if a.pm != nil {

			a.pm.Kill()

		}

	})

	a.loadUIFromCfg()

	// 运行维护页初值 + 首次状态采样放到 Run 之后的启动协程里做（见下面 go func）。
	// 这里不能调：refreshMaintStatus 会写控件、会枚举进程，而此刻消息循环还没起来。

	// 授权门：赞助版先在线验证（登录框/心跳），免费版直接放行。
	if err := verifyLicenseGate(a); err != nil {
		walk.MsgBox(nil, "DNF90 运维工具", err.Error(), walk.MsgBoxIconError)
		return
	}
	a.mw.SetVisible(true)
	// walk 缺陷规避：MainWindow 以 Visible:false 创建、授权通过后才 SetVisible(true) 时，
	// SetVisible 内部的首次 RequestLayout 因窗口仍不可见被丢弃，导致 Tab 页/子控件
	// 首帧不按真实尺寸布局（内容缩成窄条或全空），需手动补一次布局请求。
	a.mw.RequestLayout()

	// 赞助版：若 config.manage.enabled=true 且已设置配对码，后台拉起 HTTPS 管理服务（免费版为空实现）。
	a.maybeStartManage()

	// logf 已线程安全，无需 uiSafe 包一层（旧写法每行一次 PostMessage，日志洪峰下纯属额外开销）
	a.pm = ops.NewProcessManager(func(line string) { a.logf("%s", line) })

	go a.logLoop()

	go a.loopRefresh()

	go a.autoBackupLoop()

	// 定时内存回收（默认关闭，配置开启后按间隔回收服务端/自身工作集）。
	go a.memCleanLoop()

	// 常驻调度：30 秒扫一次在线福利三个档期 + 定时公告队列
	// （未启用时只做一次廉价判定，不查库）。
	a.startScheduler()

	// UI 消息循环启动后做首次检测。

	go func() {

		time.Sleep(800 * time.Millisecond)

		a.refreshNow(true)

		// 运行维护页：回填内存回收配置并做一次状态采样（此刻控件与消息循环都已就绪）。
		a.uiSafe(func() {
			a.applyMaintConfig()
			a.refreshMaintStatus()
		})

	}()

	// 打开工具自动获取公网 IP（无需手动点击）。

	go func() {

		time.Sleep(2500 * time.Millisecond)

		a.autoPublicIP()

	}()

	a.mw.Run()

	// 正常退出：落盘日志收尾（崩溃路径由顶层 defer 单独处理）。
	a.logf("== 工具已退出 ==")
	if a.fileLog != nil {
		a.fileLog.Close()
	}

}

// uiSafe 主线程执行 UI 更新。

func (a *App) uiSafe(fn func()) {

	if a.mw == nil {

		return

	}

	a.mw.Synchronize(fn)

}

func (a *App) loadUIFromCfg() {

	a.rootEdit.SetText(a.cfg.ServerRoot)

	a.ipEdit.SetText(a.cfg.PublicIP)

}

// ================= 页面构建 =================

func (a *App) serviceTab() []Widget {

	return []Widget{

		Composite{

			Layout: Grid{Columns: 2, Margins: Margins{10, 12, 10, 6}, Spacing: 8},

			Children: []Widget{

				Label{Text: "服务端根目录:"},

				Composite{Layout: HBox{MarginsZero: true, Spacing: 6}, Children: []Widget{

					LineEdit{AssignTo: &a.rootEdit, ToolTipText: "DNF90 服务端根目录（含 runtime 与 START.bat）"},

					PushButton{Text: "浏览...", OnClicked: a.onBrowseRoot},
				}},

				Label{Text: "公网 IP(下发地址):"},

				Composite{Layout: HBox{MarginsZero: true, Spacing: 6}, Children: []Widget{

					LineEdit{

						AssignTo: &a.ipEdit, ToolTipText: "启动时以 DNFBRIDGE_SERVER_IP 注入，玩家拿到该地址连接",

						OnEditingFinished: func() { a.persistUI() },
					},

					PushButton{Text: "自动获取", OnClicked: a.onAutoIP},
				}},

				Label{Text: "联动认证:"},

				Label{Text: "一键启动自动拉起 18112 认证服务（MySQL 就绪后）"},
			},
		},

		Composite{

			Layout: HBox{Margins: Margins{10, 0, 10, 8}, Spacing: 8},

			Children: []Widget{

				PushButton{AssignTo: &a.initBtn, Text: "① 一键初始化", OnClicked: a.onInit},

				PushButton{AssignTo: &a.startBtn, Text: "▶ 一键启动(全部)", OnClicked: a.onStart},

				PushButton{AssignTo: &a.stopBtn, Text: "■ 停止服务", OnClicked: a.onStop},

				PushButton{AssignTo: &a.refreshBtn, Text: "刷新状态", OnClicked: func() { a.refreshNow(true) }},

				PushButton{AssignTo: &a.serverBtn, Text: "服务端预检(Doctor)", OnClicked: a.onDoctor},
			},
		},

		Label{Text: "提示：一键启动按顺序执行：拉起 MySQL → 自动初始化（缺 instance.json 时）→ env 直启 DNF90Server → 拉起 18112 认证服务。\r\n       数据库模式固定为 MySQL；公网 IP 打开工具即自动获取，也可点「自动获取」手动刷新。"},
	}

}

func (a *App) authTab() []Widget {

	return []Widget{

		Composite{

			Layout: Grid{Columns: 2, Margins: Margins{10, 12, 10, 6}, Spacing: 8},

			Children: []Widget{

				Label{Text: "公网监听:"},

				Label{Text: "https" + listenAddrDisplay(a.cfg.Public.ListenAddr)},

				Label{Text: "数据库:"},

				Label{Text: fmt.Sprintf("%s:%d/%s", a.cfg.MySQL.Host, a.cfg.MySQL.Port, a.cfg.MySQL.DBName)},

				Label{Text: "注册模式:"},

				Label{Text: a.cfg.RegistrationMode},
			},
		},

		Composite{

			Layout: HBox{Margins: Margins{10, 10, 10, 6}, Spacing: 8},

			Children: []Widget{

				PushButton{AssignTo: &a.authBtn, Text: "启动认证服务", OnClicked: a.onToggleAuth},

				Label{AssignTo: &a.authStateL, Text: "已停止", TextColor: walk.RGB(160, 40, 40)},
			},
		},

		Label{Text: "说明：认证服务提供公网 HTTPS 登录/验证 API，供异环登录器使用；服务端通过\r\n       内部 HTTP " + a.cfg.Internal.ListenAddr + " 回调完成票据校验。请先在「一键启动」页启动 DNF90 服务端。"},
	}

}

// ================= 交互 =================

func (a *App) onBrowseRoot() {

	dlg := new(walk.FileDialog)

	dlg.Title = "选择 DNF90 服务端根目录（含 runtime 与 START.bat）"

	dlg.FilePath = a.rootEdit.Text()

	if ok, err := dlg.ShowBrowseFolder(a.mw); err != nil {

		a.logf("打开目录选择失败: %v", err)

		return

	} else if !ok {

		return

	}

	path := dlg.FilePath

	a.rootEdit.SetText(path)

	a.persistUI()

	a.refreshNow(true)

}

func (a *App) onAutoIP() {

	a.uiSafe(func() { a.refreshBtn.SetEnabled(false) })

	a.runAsync("自动获取公网 IP", func() error {

		ip, err := serverdir.GetPublicIP()

		if err != nil {

			return err

		}

		a.uiSafe(func() {

			a.ipEdit.SetText(ip)

			a.logf("获取到公网 IP: %s", ip)

			a.persistUI()

		})

		return nil

	}, func(err error) {

		if err != nil {

			a.logf("获取公网 IP 失败: %v（请检查网络后重试，或手动填写）", err)

		}

	})

}

func (a *App) onDoctor() {

	root := a.rootEdit.Text()

	if strings.TrimSpace(root) == "" {

		a.logf("请先选择服务端根目录")

		return

	}

	a.persistUI()

	a.runAsync("服务端预检", func() error {

		return serverdir.RunDoctor(root, func(line string) { a.uiSafe(func() { a.logf("%s", line) }) }, 120*time.Second)

	}, func(err error) {

		if err != nil {

			a.logf("预检失败: %v", err)

		}

	})

}

func (a *App) onInit() {

	root := strings.TrimSpace(a.rootEdit.Text())

	if root == "" {

		a.logf("请先选择服务端根目录")

		return

	}

	mode := a.dbMode()

	ins := serverdir.Inspect(root)

	if ins.InstanceExists && ins.InstanceValid && !ins.NeedsInit {

		a.logf("当前目录已初始化（mode=%s），无需重复执行；如需重建请删除 runtime\\config\\instance.json 后重试", ins.DatabaseMode)

		return

	}

	a.persistUI()

	a.runAsync("一键初始化", func() error {
		if err := serverdir.EnsureDatabaseReady(root, mode, func(line string) {
			a.uiSafe(func() { a.logf("%s", line) })
		}, 240*time.Second); err != nil {
			return err
		}
		return serverdir.EnsureRuntimeConfigs(root, func(line string) {
			a.uiSafe(func() { a.logf("%s", line) })
		})
	}, func(err error) {

		if err != nil {

			a.logf("初始化失败: %v", err)

			return

		}

		a.logf("初始化完成，请点击「刷新状态」确认")

	})

}

func (a *App) onStart() {

	root := strings.TrimSpace(a.rootEdit.Text())

	if root == "" {

		a.logf("请先选择服务端根目录")

		return

	}

	a.persistUI()

	// 公网 IP 为空时的补获取**放在启动任务内部**做，绝不能在这里嵌套第二次 runAsync。
	//
	// 旧写法是：runAsync("自动获取公网 IP") → UI 线程 time.Sleep(2s) → runAsync("一键启动")。
	// 而 runAsync 第一句就是占 busy 的 tryBusy：只要取 IP 超过 2 秒（慢网/DNS 超时很常见），
	// 第二次 runAsync 就只打一句"已有任务进行中"然后 return —— 服务端根本不会启动，
	// finish 回调也不执行，界面上没有任何错误提示。经远程管理端调用时更糟：
	// manageTriggerAndWait 会等到 busy 变 false 后返回 nil，HTTP 调用方收到"启动成功"。
	// 放进同一个 job 里既保证顺序也不会自锁，UI 线程也不再被 Sleep 卡住。
	a.runAsync("一键启动(全部)", func() error {

		return a.startServerAllWithPublicIP()

	}, func(err error) {

		if err != nil {

			a.logf("一键启动失败: %v", err)

		}

	})
}

// startServerAllWithPublicIP 启动前确保公网 IP 可用，然后走完整启动链路。
// 取 IP 失败不阻断启动（只是玩家可能连不上），与旧行为一致，但会把原因写进日志。
func (a *App) startServerAllWithPublicIP() error {

	if strings.TrimSpace(a.ipEdit.Text()) == "" {

		ip, err := serverdir.GetPublicIP()

		if err != nil {

			a.logf("自动获取公网 IP 失败: %v（继续启动；玩家可能连不上，可停止后手动填写再启动）", err)

		} else {

			a.cfg.PublicIP = ip

			_ = a.cfg.Save()

			a.uiSafe(func() { a.ipEdit.SetText(ip) })

			a.logf("已自动获取公网 IP: %s", ip)

		}

	}

	return a.startServerAll()
}

// startServerAll 同步执行一键启动链路（初始化/拉起 MySQL + 补齐配置 + env 直启 DNF90Server + 联动认证服务）。
// 行为与点击「启动」一致，供还原完成后询问启动等内部流程复用。
func (a *App) startServerAll() error {

	root := strings.TrimSpace(a.rootEdit.Text())

	// 1) 目录与可执行文件检测。

	ins := serverdir.Inspect(root)

	if !ins.RootExists || !ins.ServerExeExists {

		return fmt.Errorf("服务端目录无效：%s", ins.Message)

	}

	// 1.1) 服务端认的是**私有 IPv4**（instance.json 的 advertiseIp=AUTO_DETECT 只接受
	//      10./172.16-31./192.168.）；只绑了公网 IP 的云主机会起不来。与初始化同一条兜底：
	//      探测不到就自动给主网卡加一个备用地址（不设网关，不影响公网）。见 serverdir/private_ip.go。

	if err := serverdir.EnsurePrivateLANIPv4(func(line string) { a.logf("%s", line) }); err != nil {

		return err

	}

	// 2) 若已有服务端进程（非本工具拉起）在跑，先停掉，避免双开。

	if pmRun, _ := a.pm.Status(); pmRun {

		return fmt.Errorf("已有由本工具启动的服务进程，请先点「停止服务」")

	}
	if ins.Running {

		a.logf("检测到服务端已在监听（可能由原版 START.bat 拉起），先停止旧进程...")

		procs, _ := serverdir.ListRootProcesses(root, "DNF90Server.exe", "DNF90Control.exe")

		for _, p := range procs {

			a.logf("停止 %s (pid=%d)", p.Name, p.PID)

			_ = serverdir.KillTree(p.PID, false)

		}

		time.Sleep(2 * time.Second)

	}

	// 3) 拉起 MySQL（固定 mysql 模式；端口未监听则完整初始化并拉起内置便携 MySQL，未携带时回退本机服务）。

	host := strings.TrimSpace(a.cfg.MySQL.Host)

	if host == "" {

		host = "127.0.0.1"

	}

	port := a.cfg.MySQL.Port

	if port <= 0 {

		port = 13306

	}

	if serverdir.ProbePort(host, port, 500*time.Millisecond) {

		a.logf("MySQL 已就绪: %s:%d", host, port)

	} else {

		a.logf("MySQL %s:%d 未监听，正在完整初始化并拉起 MySQL（内置便携优先，未携带则回退本机服务）...", host, port)

		if err := serverdir.EnsureDatabaseReady(root, "mysql", func(line string) {

			a.uiSafe(func() { a.logf("%s", line) })

		}, 240*time.Second); err != nil {

			return fmt.Errorf("MySQL 初始化/启动失败: %v", err)

		}

	}

	// 4) 复查：instance/MySQL 数据仍未就绪时自动补齐（MySQL 模式）。

	ins = serverdir.Inspect(root)

	if ins.NeedsInit || !ins.InstanceValid {

		a.logf("检测到初始化不完整（%s），自动补齐...", ins.Message)

		if err := serverdir.EnsureDatabaseReady(root, "mysql", func(line string) {

			a.uiSafe(func() { a.logf("%s", line) })

		}, 240*time.Second); err != nil {

			return fmt.Errorf("自动初始化失败: %v", err)

		}

		ins = serverdir.Inspect(root)

		if ins.NeedsInit || !ins.InstanceValid {

			return fmt.Errorf("初始化后仍未就绪: %s", ins.Message)

		}

	}

	// 4.1) 补齐运行配置（dnfbridge.toml / logic.toml / plan.json / channel_info.etc），幂等可重复执行。
	if err := serverdir.EnsureRuntimeConfigs(root, func(line string) {
		a.uiSafe(func() { a.logf("%s", line) })
	}); err != nil {
		return fmt.Errorf("生成运行配置失败: %v", err)
	}

	// 4.2) 队伍 UDP 中继端口池数量：instance.json 里不是 1024 就改写为 1024。
	//      必须排在第 5 步之前 —— 第 5 步构造运行环境时会把该值注入
	//      DNFBRIDGE_PARTY_UDP_RELAY_PORT_COUNT，改晚了本次启动拿到的还是旧值。
	//      失败只记日志、不阻断启动（宁可按旧值起，也不能因为改配置把服务卡住）。
	if _, err := serverdir.EnsurePartyUDPRelayPortCount(root, serverdir.WantedPartyUDPRelayPortCount, func(line string) {
		a.uiSafe(func() { a.logf("%s", line) })
	}); err != nil {
		a.logf("检查 partyUdpRelayPortCount 失败（不阻断启动，本次仍按 instance.json 现值注入）: %v", err)
	}

	// 5) env 直启 DNF90Server。

	exe := filepath.Join(root, "runtime", "bin", "DNF90Server.exe")

	if !serverdir.FileExists(exe) {

		return fmt.Errorf("找不到 DNF90Server.exe: %s", exe)

	}

	workDir := filepath.Join(root, "runtime")

	publicIP := strings.TrimSpace(a.ipEdit.Text())

	if ins.Instance != nil && ins.Instance.AdvertiseIP != "" && publicIP != "" && serverdir.IsPrivateIP(ins.Instance.AdvertiseIP) {

		a.logf("提示: instance 下发地址 %q 为私网/自动，启动前将改写 dnfbridge public_addr 并注入 DNFBRIDGE_SERVER_IP=%s 双保险", ins.Instance.AdvertiseIP, publicIP)

	}

	if publicIP == "" {

		a.logf("警告: 公网 IP 为空，玩家可能连不上；请点「自动获取」或手动填写")

	} else {

		// 5.1) 将公网 IP 直接改写进 dnfbridge.toml 的 [service].public_addr（保留端口），原文件首次改写前备份为 .bak。

		if addr, err := serverdir.WritePublicAddr(root, publicIP); err != nil {

			a.logf("改写 public_addr 失败（不影响 env 注入启动，稍后可手动处理）: %v", err)

		} else {

			a.logf("已将 dnfbridge.toml [service].public_addr 改写为 %s（原文件已备份为 dnfbridge.toml.bak）", addr)

		}

	}

	// 5.2) 频道监听对外暴露：instance.json 默认 channelListen=127.0.0.1:port，
	// 若直接注入原值，DNF90Server 会把 7001 绑在回环上，公网玩家无法连接。
	// 公网部署时统一改写为 0.0.0.0:port（端口不变）并持久化 instance.json。
	channelListen := ""
	if ins.Instance != nil {
		channelListen = ins.Instance.ChannelListen
		if publicIP != "" && !strings.HasPrefix(channelListen, "0.0.0.0:") {
			chPort := ins.ChannelPort
			if chPort <= 0 {
				chPort = 7001
			}
			channelListen = "0.0.0.0:" + strconv.Itoa(chPort)
			if _, err := serverdir.RewriteChannelListen(root, channelListen); err != nil {
				a.logf("改写 instance.json channelListen 失败（本次启动仍注入 %s，可稍后手动处理）: %v", channelListen, err)
			} else {
				a.logf("已将 instance.json channelListen 改写为 %s（原文件已备份为 instance.json.bak）", channelListen)
			}
			ins.Instance.ChannelListen = channelListen
		}
	}

	env := append(ops.DNFBridgeEnv(a.cfg),
		"DNFBRIDGE_SERVER_IP="+publicIP,
		// game 端口监听地址：优先真实网卡私有 IP（NAT 场景暴露面最小），
		// 无私有 IP 时回退 0.0.0.0（纯公网服务器绑所有接口直连）。
		"DNFBRIDGE_GAME_LISTEN_HOST="+serverdir.DetectGameListenHost(),
		"DNFBRIDGE_CHANNEL_LISTEN="+channelListen,
		// party UDP 可达性探测绑定 0.0.0.0：公网云 NAT 会把入站数据报
		// 目标地址改写为私网网卡地址，绑公网/单网卡 IP 会收不到探测，
		// 导致组队静默失败（B 接受后双方无反应）。0.0.0.0 绑所有接口。
		"DNFBRIDGE_UDP_BIND_IP=0.0.0.0")

	env = append(env, instanceEnv(ins.Instance)...)

	a.logf("正在直启 DNF90Server.exe -config configs/dnfbridge.toml（工作目录 runtime，注入 env 直启，不调 control start）...")

	if err := a.pm.StartWithEnv(exe, workDir, []string{"-config", "configs/dnfbridge.toml"}, env); err != nil {

		return err

	}

	// 6) 等待频道口就绪。

	port2 := ins.ChannelPort

	if port2 <= 0 {

		port2 = 7001

	}

	deadline := time.Now().Add(60 * time.Second)

	for time.Now().Before(deadline) {

		if serverdir.ProbePort("127.0.0.1", port2, 500*time.Millisecond) {

			a.logf("服务端频道口 %d 已就绪，启动完成", port2)

			break

		}

		time.Sleep(1 * time.Second)

	}

	if !serverdir.ProbePort("127.0.0.1", port2, 500*time.Millisecond) {

		a.logf("60 秒内频道口 %d 未开放，请结合上方日志检查（首次启动可能仍在初始化数据库）", port2)

	}

	// 7) 联动拉起 18112 认证服务（一键启动=启动所有）。

	if !a.svcOn.Load() {

		a.logf("正在联动启动认证服务（HTTPS %s）...", listenAddrDisplay(a.cfg.Public.ListenAddr))

		if err := a.startAuth(); err != nil {

			a.logf("认证服务启动失败: %v（不影响服务端运行，可到「认证服务」页重试）", err)

		} else {

			a.logf("认证服务已启动")

		}

	}

	return nil

}

func (a *App) onStop() {

	a.persistUI()

	a.runAsync("停止服务", a.stopServerAll, func(err error) {

		if err != nil {

			a.logf("停止失败: %v", err)

		}

	})
}

// stopServerAll 同步执行一键停止链路（认证服务 + DNF90Server + 便携 MySQL）。
// 行为与点击「停止」一致，供还原数据库等内部流程复用。
func (a *App) stopServerAll() error {

	root := strings.TrimSpace(a.rootEdit.Text())

	var firstErr error

	// 先停 18112 认证服务。

	if a.svcOn.Load() {

		a.logf("停止认证服务...")

		if err := a.stopAuth(); err != nil {

			firstErr = err

		}

	}

	// 再停 DNF90 服务端。

	serverStopped := false

	if pmRun, pid := a.pm.Status(); pmRun {

		a.logf("停止本工具启动的服务进程（pid=%d）...", pid)

		if err := a.pm.Stop(5 * time.Second); err != nil {

			a.logf("停止失败，尝试强制结束: %v", err)

			a.pm.Kill()

		}

		serverStopped = true

	} else if root != "" {

		procs, err := serverdir.ListRootProcesses(root, "DNF90Server.exe", "DNF90Control.exe")

		if err == nil && len(procs) > 0 {

			for _, p := range procs {

				a.logf("发现 %s (pid=%d, %s)，正在停止...", p.Name, p.PID, p.ExecutablePath)

				_ = serverdir.KillTree(p.PID, false)

			}

			time.Sleep(2 * time.Second)

			for _, p := range procs {

				if procAlive(p.PID) {

					_ = serverdir.KillTree(p.PID, true)

					a.logf("已强制结束 pid=%d", p.PID)

				}

			}

			serverStopped = true

		}

	}

	// 最后停便携 MySQL（一键停止 = 停止所有）。

	if root != "" {

		mys, err := serverdir.ListRootProcesses(root, "mysqld.exe")

		if err == nil && len(mys) > 0 {

			for _, p := range mys {

				a.logf("停止便携 MySQL %s (pid=%d)...", p.Name, p.PID)

				_ = serverdir.KillTree(p.PID, false)

			}

			time.Sleep(3 * time.Second)

			for _, p := range mys {

				if procAlive(p.PID) {

					_ = serverdir.KillTree(p.PID, true)

					a.logf("MySQL 未退出，已强制结束 pid=%d", p.PID)

				}

			}

		}

	}

	if !serverStopped {

		if firstErr != nil {

			return firstErr

		}

		return fmt.Errorf("未发现正在运行的 DNF90 服务端（本工具未启动它，且目录内无匹配进程）")

	}

	return firstErr

}

func (a *App) onToggleAuth() {

	a.persistUI()

	if a.svcOn.Load() {

		a.runAsync("停止认证服务", func() error {

			return a.stopAuth()

		}, func(err error) {

			if err != nil {

				a.logf("停止认证服务失败: %v", err)

			}

		})

		return

	}

	a.runAsync("启动认证服务", func() error {

		if err := a.startAuth(); err != nil {

			return err

		}

		a.logf("认证服务已启动（公网 HTTPS :%s）", a.cfg.Public.ListenAddr)

		return nil

	}, func(err error) {

		if err != nil {

			a.logf("启动认证服务失败: %v", err)

		}

	})

}

// mysqlFromInstance 实时从服务端 runtime\config\instance.json 读取 MySQL 凭据。

// 未初始化（instance.json 缺失或没有 database 段）时返回错误，引导先执行初始化，

// 避免用 config.json 的静态空密码去连库。

func (a *App) mysqlFromInstance() (config.MySQLConfig, error) {

	root := strings.TrimSpace(a.rootEdit.Text())

	if root == "" {

		root = strings.TrimSpace(a.cfg.ServerRoot)

	}

	if root == "" {

		return config.MySQLConfig{}, fmt.Errorf("未选择服务端目录")

	}

	ins := serverdir.Inspect(root)

	if ins == nil || ins.Instance == nil || ins.Instance.DBName == "" {

		return config.MySQLConfig{}, fmt.Errorf("服务端数据库未初始化（runtime\\config\\instance.json 缺失或缺少 database 段），请先执行「一键启动」完成初始化后再启动认证服务")

	}

	it := ins.Instance

	mcfg := config.MySQLConfig{

		Host: it.DBHost,

		Port: it.DBPort,

		User: it.DBUser,

		Password: it.DBPassword,

		DBName: it.DBName,
	}

	// 个别字段缺失时回退默认/静态配置，保证兼容。

	if mcfg.Host == "" {

		mcfg.Host = a.cfg.MySQL.Host

	}

	if mcfg.Port <= 0 {

		mcfg.Port = a.cfg.MySQL.Port

	}

	if mcfg.User == "" {

		mcfg.User = a.cfg.MySQL.User

	}

	if mcfg.Password == "" {

		mcfg.Password = a.cfg.MySQL.Password

	}

	if mcfg.DBName == "" {

		mcfg.DBName = a.cfg.MySQL.DBName

	}

	if mcfg.Host == "" {

		mcfg.Host = "127.0.0.1"

	}

	if mcfg.Port <= 0 {

		mcfg.Port = 13306

	}

	if mcfg.User == "" {

		mcfg.User = "root"

	}

	if mcfg.DBName == "" {

		mcfg.DBName = "dnf_local"

	}

	return mcfg, nil

}

// startAuth 启动认证服务（依赖 MySQL；凭据实时取 instance.json）。

func (a *App) startAuth() error {

	mcfg, err := a.mysqlFromInstance()

	if err != nil {

		return err

	}

	st, err := store.Open(mcfg)

	if err != nil {

		return fmt.Errorf("连接 MySQL(%s:%d) 失败: %w；请确认服务端数据库已就绪", mcfg.Host, mcfg.Port, err)

	}

	svc := server.New(a.cfg, st, slog.Default(), func() bool { return ops.GameUp(a.cfg) }, a.psPublicInfo, a.rbPublicInfo, a.regPublicLimit)
	// 更新维护状态注入：登录与进游戏验票两处闸门读它；开关变更时由 maintApply 广播给登录器。
	svc.SetMaintenance(a.maintPublicState)
	// 登录器版本校验注入：/launcher/v1/version-check 读它（自检阶段 + 点登录时各查一次）。
	// 版本号来自 launcher_version.go 的内置常量，发布新登录器时必须同步改那里。
	svc.SetLauncherVersion(a.lvPublicState)
	// 双开（多开）开关注入：/launcher/v1/multi-open 读它（登录器自检阶段查一次）。
	// 未显式设置过时该端点不下发 enabled 字段 → 登录器保持"允许双开"（multi_open.go）。
	svc.SetMultiOpen(a.moPublicState)
	// GM 事件实时推送：写事件的十来处（封禁踢人 / 泡点到账 / 注册赠送 / 福利 …）都走 store，
	// 把 hub 的回调挂在 store 上 —— "只要落了库就一定唤醒正在长轮询的登录器"，
	// 不需要每个调用点自己记得通知（漏一处就是一类事件不实时）。
	// 到账绿字的延迟从"平均 5 秒（10 秒轮询）"降到毫秒级。
	st.SetGMEventNotifier(svc.GMEventAdded)
	// 第③步：yh 授权回调核销装配（赞助版内置回调地址/密钥自动开启；免费版不装配；
	// 环境变量 YH_CHECK_URL/YH_CHECK_KEY 可成对覆盖内置配置）。
	if yh, yhErr := server.NewYhCheckAuto(slog.Default()); yhErr != nil {
		a.logf("yh 授权回调未装配: %v", yhErr)
	} else if yh != nil {
		svc.SetYhCheck(yh.Check)
		a.logf("yh 授权回调已装配（authserver 核销开启）")
	}

	ctx, cancel := context.WithCancel(context.Background())

	if err := svc.Start(ctx); err != nil {

		cancel()

		st.Close()

		return err

	}

	a.svcCancel = cancel

	a.svc = svc

	a.st = st

	a.svcOn.Store(true)

	a.updateAuthUI()

	return nil

}

// stopAuth 停止认证服务。

func (a *App) stopAuth() error {

	var firstErr error

	if a.svc != nil {

		if err := a.svc.Stop(context.Background()); err != nil {

			firstErr = err

		}

		if a.svcCancel != nil {

			a.svcCancel()

			a.svcCancel = nil

		}

		a.svc = nil

	}

	if a.st != nil {

		_ = a.st.Close()

		a.st = nil

	}

	a.svcOn.Store(false)

	a.updateAuthUI()

	return firstErr

}

func (a *App) updateAuthUI() {

	a.uiSafe(func() {

		if a.authStateL == nil {

			return

		}

		if a.svcOn.Load() {

			a.authStateL.SetText("运行中")

			a.authStateL.SetTextColor(walk.RGB(32, 130, 32))

			if a.authBtn != nil {

				a.authBtn.SetText("停止认证服务")

			}

		} else {

			a.authStateL.SetText("已停止")

			a.authStateL.SetTextColor(walk.RGB(160, 40, 40))

			if a.authBtn != nil {

				a.authBtn.SetText("启动认证服务")

			}

		}

	})

}

// ================= 状态刷新 =================

func (a *App) refreshNow(verbose bool) {

	// 原子占位：旧写法是"先 if 检查、再赋值"，后台协程与 UI 线程之间存在竞态窗口，
	// 可能出现两个检测并发跑、或 busy 被对方提前清掉。
	if !a.tryBusy() {

		return

	}

	// 注意：这里读控件属于跨线程访问（loopRefresh 在后台协程调用）。UI 线程会被阻塞到
	// 消息被处理完，但每 3 秒一次、代价可忽略，且改成缓存值会让"刚输入未落盘的路径"
	// 读不到、反而引入行为回归，故保持现状。
	root := strings.TrimSpace(a.rootEdit.Text())

	if verbose {

		a.logf("正在检测: %s", root)

	}

	go func() {

		defer a.clearBusy()

		ins := serverdir.Inspect(root)

		pmRun, pid := a.pm.Status()

		a.uiSafe(func() {

			if ins.Running || pmRun {

				a.statusL.SetTextColor(walk.RGB(32, 130, 32))

			} else {

				a.statusL.SetTextColor(walk.RGB(90, 90, 90))

			}

			if root == "" {

				a.statusL.SetText("未选择服务端目录")

			} else if pmRun {

				a.statusL.SetText(fmt.Sprintf("运行中（本工具 pid=%d）· %s", pid, ins.Message))

			} else {

				a.statusL.SetText(ins.Message)

			}

			a.updateButtons(ins, pmRun)

		})

		if verbose {

			a.uiSafe(func() {

				a.logf("检测结果: %s", ins.Message)

			})

		}

	}()

}

func (a *App) updateButtons(ins *serverdir.Inspection, pmRun bool) {

	enabled := !a.isBusy()

	rootOK := ins != nil && ins.RootExists

	needInit := ins != nil && ins.NeedsInit

	canStart := enabled && rootOK && !needInit && ins != nil && ins.Ready && !ins.Running && !pmRun

	canStop := enabled && (pmRun || (ins != nil && ins.Running))

	a.initBtn.SetEnabled(enabled && rootOK && (needInit || ins.InstanceValid))

	a.startBtn.SetEnabled(canStart)

	a.stopBtn.SetEnabled(canStop)

	a.refreshBtn.SetEnabled(enabled)

	a.serverBtn.SetEnabled(enabled && rootOK)

}

func (a *App) loopRefresh() {
	_ = a.guardGoroutine("状态轮询循环", func() {
		tick := time.NewTicker(3 * time.Second)

		defer tick.Stop()

		for range tick.C {

			if a.mw == nil {

				continue

			}

			a.refreshNow(false)

			// 认证服务运行中：后台自动刷新封禁页在线列表（服务停止后自然跳过）
			if a.svcOn.Load() && a.st != nil && banWidgets.onlineTable != nil {
				go func() { _ = a.loadOnline() }()
			}

		}
	})
}

// ================= 工具 =================

// dbMode 数据库模式固定为 MySQL（不再提供选择）。

func (a *App) dbMode() string {

	return "mysql"

}

// persistUI 把 UI 值写回 config 并保存。

func (a *App) persistUI() {

	if a.cfg == nil {

		return

	}

	a.cfg.ServerRoot = strings.TrimSpace(a.rootEdit.Text())

	a.cfg.DatabaseMode = a.dbMode()

	a.cfg.PublicIP = strings.TrimSpace(a.ipEdit.Text())

	// 选择服务端目录后自动推导 dnfbridge executable/working_dir（兼容旧联动入口）。

	if a.cfg.ServerRoot != "" {

		derived := filepath.Join(a.cfg.ServerRoot, "runtime", "bin", "DNF90Server.exe")

		if a.cfg.DNFBridge.Executable == "" || strings.Contains(strings.ToLower(a.cfg.DNFBridge.Executable), "dnf90server.exe") {

			a.cfg.DNFBridge.Executable = derived

			a.cfg.DNFBridge.WorkingDir = filepath.Join(a.cfg.ServerRoot, "runtime")

		}

	}

	if err := a.cfg.Save(); err != nil {

		a.logf("保存配置失败: %v", err)

	}

}

// runAsync 后台任务：加 busy、禁用 UI、执行 fn、完成后回调 finish（均在主线程回调 finish）。

func (a *App) runAsync(label string, fn func() error, finish func(err error)) {

	if !a.tryBusy() {

		a.logf("已有任务进行中，请稍候")

		return

	}

	a.uiSafe(func() {

		a.statusL.SetText("执行中: " + label + " ...")

		a.refreshBtn.SetEnabled(false)

		a.initBtn.SetEnabled(false)

		a.startBtn.SetEnabled(false)

		a.stopBtn.SetEnabled(false)

		a.serverBtn.SetEnabled(false)

	})

	go func() {

		// 兜底 recover：旧实现里 fn 一旦 panic，协程直接终止进程，且 busy 永久停在 true
		//（界面从此"刷新不动、任务点不动"）。这里把 panic 转成错误交给 finish 处理，
		// 并把现场写进 crash.log（panic 转错误后进程活着，但堆栈只此一份，必须落盘）。
		var err error

		func() {

			defer func() {

				if r := recover(); r != nil {

					err = fmt.Errorf("任务内部异常: %v", r)
					a.writeCrashLog("runAsync:"+label, r, debugStack())

				}

			}()

			err = fn()

		}()

		a.uiSafe(func() {

			a.clearBusy()

			a.statusL.SetText("执行完成: " + label)

			if finish != nil {

				finish(err)

			}

			a.refreshNow(false)

		})

	}()

}

// instanceEnv 从 instance.json 派生 dnfbridge 直启所需的完整运行环境，

// 覆盖 gameUpperHeader / gameOuterToken / party UDP 等仅存在 instance.json 的字段。

func instanceEnv(ins *serverdir.InstanceInfo) []string {

	if ins == nil {

		return nil

	}

	b := func(v bool) string { return strconv.FormatBool(v) }

	i := func(v int) string { return strconv.Itoa(v) }

	return []string{

		"DNFBRIDGE_GAME_UPPER_HEADER=" + ins.GameUpperHeader,

		"DNFBRIDGE_GAME_UPPER_BODY_CODEC=" + ins.GameUpperBodyCodec,

		"DNFBRIDGE_GAME_UPPER_CLIENT_BODY_CODEC=" + ins.GameUpperClientBodyCodec,

		"DNFBRIDGE_GAME_OUTER_TOKEN=" + ins.GameOuterToken,

		"DNFBRIDGE_DPROTO_MODE=legacy_patch",

		"DNFBRIDGE_CHANNEL_SERVER_INDEX=" + i(ins.ChannelServerIndex),

		"DNFBRIDGE_CHANNEL_ADVERTISE_SERVER_INDEX=" + i(ins.ChannelAdvertiseIndex),

		"DNFBRIDGE_REGION_ID=" + i(ins.RegionID),

		"DNFBRIDGE_PARTY_UDP_RELAY_ENABLED=" + b(ins.PartyUDPRelayEnabled),

		"DNFBRIDGE_PARTY_UDP_RELAY_PORT_START=" + i(ins.PartyUDPRelayPortStart),

		"DNFBRIDGE_PARTY_UDP_RELAY_PORT_COUNT=" + i(ins.PartyUDPRelayPortCount),

		"DNFBRIDGE_PVF_PATH=" + ins.PVFPath,

		"DNFBRIDGE_CHANNEL_INFO_PATH=" + ins.ChannelInfoPath,

		"DNFBRIDGE_CHANNEL_INFO_BODY_MODE=latest",

		"DNFBRIDGE_DUNGEON_BATCH_PICKUP_ENABLED=" + b(ins.DungeonBatchPickupEnabled),

		"DNFBRIDGE_ACCOUNT_ID=" + ins.AccountID,

		"DNFBRIDGE_PACKET_LOG=" + b(ins.PacketLog),

		"DNFBRIDGE_DEBUG_MODE=" + b(ins.DebugMode),

		"DNFBRIDGE_DEBUG_STRIKE_ON_SKILL_USE=" + b(ins.DebugStrikeOnSkillUse),

		"DNFBRIDGE_ROBOT_ENABLED=" + b(ins.RobotEnabled),

		// 固定项（对齐原版 DNF90Control runtimeEnvironment；logic 配置入口 + 引导模式）。
		"LONGHENG_DNF_CONFIG=configs/dnf/logic.toml",
		"DNFBRIDGE_GAME_INITIAL_MODE=notice",
		"DNFBRIDGE_GAME_PRE_BOOTSTRAP=none",
		"DNFBRIDGE_GAME_POST_BOOTSTRAP=none",
		"DNFBRIDGE_PACKET_LOG_PATH=logs/packet_log.txt",
		"DNF_SCRIPT_VERSION=61",
	}

}

// mysqlPortOf 取 portable MySQL 端口（instance.json 缺失时用 13306）。

func mysqlPortOf(root string, ins *serverdir.Inspection) string {

	return "13306"

}

// listenAddrDisplay 将 ":18112" 展示为 "0.0.0.0:18112"。

func listenAddrDisplay(addr string) string {

	addr = strings.TrimSpace(addr)

	if strings.HasPrefix(addr, ":") {

		return "0.0.0.0" + addr

	}

	return addr

}

// autoPublicIP 打开工具后自动获取公网 IP 并回填、持久化（不阻塞 UI）。

func (a *App) autoPublicIP() {

	if a.cfg == nil || a.ipEdit == nil {

		return

	}

	go func() {

		ip, err := serverdir.GetPublicIP()

		if err != nil {

			a.uiSafe(func() {

				if strings.TrimSpace(a.ipEdit.Text()) == "" {

					a.logf("自动获取公网 IP 失败: %v（可稍后点「自动获取」重试）", err)

				}

			})

			return

		}

		a.uiSafe(func() {

			a.ipEdit.SetText(ip)

			a.logf("已自动获取公网 IP: %s", ip)

		})

		a.cfg.PublicIP = ip

		_ = a.cfg.Save()

	}()

}

// procAlive 简单判断 PID 存活（tasklist）。

func procAlive(pid int) bool {

	cmd := exec.Command("tasklist", "/FI", "PID eq "+fmt.Sprint(pid), "/NH")

	cmd.SysProcAttr = &syscall.SysProcAttr{HideWindow: true}

	out, err := cmd.Output()

	if err != nil {

		return false

	}

	return strings.Contains(string(out), fmt.Sprint(pid))

}
