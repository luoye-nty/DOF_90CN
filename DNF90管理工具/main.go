package main

// DNF90 后台管理工具（家用电脑客户端）。
// 直连服务器端赞助版运维工具的 /manage/v1/* 管理 API（HTTPS :18113），
// 实现本地等价服务器 GUI 的远程管理操作。
//
// 登录独立为单独的登录窗口；登录成功后进入主界面。
// 页面组织（buildAllPages）：
//   0 状态（status_tab）  1 服务器选项/公告/泡点（placeholder_tab）
//   2 账号管理（account_tab）  3 封禁（ban_tab）  4 CDK（cdk_tab）  5 PVF 加密（pvf_tab）

import (
	"fmt"
	"log"
	"path/filepath"
	"strings"
	"sync"
	"time"

	"github.com/lxn/walk"
	decl "github.com/lxn/walk/declarative"

	pvfindex "dnf90manager/internal/pvfindex"
)

// App 跨页共享的应用状态与 UI 句柄（各 *_tab.go 通过 App 访问公共控件）。
type App struct {
	cfg      *LocalConfig
	store    *ConfigStore
	cfgSaveM sync.Mutex

	clientMu sync.Mutex
	client   *APIClient // token 仅存内存

	mw  *walk.MainWindow
	tab *walk.TabWidget

	// 顶部栏
	connInfoL *walk.Label
	logoutBtn *walk.PushButton

	// 状态页控件与轮询
	statusText *walk.TextEdit
	statusFoot *walk.Label
	refreshBtn *walk.PushButton
	startBtn   *walk.PushButton
	stopBtn    *walk.PushButton
	pollMu     sync.Mutex
	pollCancel chan struct{}

	// 封禁页控件
	banListText   *walk.TextEdit
	banRefreshBtn *walk.PushButton
	banTypeCb     *walk.ComboBox
	banValueLE    *walk.LineEdit
	banUserLE     *walk.LineEdit
	banReasonLE   *walk.LineEdit
	banDaysLE     *walk.LineEdit
	banDoBtn      *walk.PushButton
	ubTypeCb      *walk.ComboBox
	ubValueLE     *walk.LineEdit
	ubBtn         *walk.PushButton
	kickLE        *walk.LineEdit
	kickBtn       *walk.PushButton

	// 收益制裁（占位）控件
	sanctionAcctLE  *walk.LineEdit
	sanctionMinLE   *walk.LineEdit
	sanctionBtn     *walk.PushButton
	sanctionLiftBtn *walk.PushButton

	// CDK 页控件（对齐运维工具：生成表单 + 列表/流水表格）
	cdkNameEdit     *walk.LineEdit
	cdkTimesEdit    *walk.NumberEdit
	cdkGenCountEdit *walk.NumberEdit
	cdkRemarkEdit   *walk.LineEdit
	cdkRewardTable  *walk.TableView
	cdkItemIDEdit   *walk.LineEdit
	cdkItemCntEdit  *walk.LineEdit
	cdkCeraEdit     *walk.LineEdit
	cdkBdDaysEdit   *walk.LineEdit
	cdkGenBtn       *walk.PushButton
	cdkHintL        *walk.Label
	cdkCodeTable    *walk.TableView
	cdkKeywordLE    *walk.LineEdit
	cdkRedeemTable  *walk.TableView
	cdkRedeemFilter *walk.LineEdit
	cdkRewardRows   []cdkRewardRow

	// 服务器选项页（全服公告）控件
	announceEdit *walk.TextEdit

	// 状态栏「已连接」旁边的一键按钮（见 global_config.go）
	globalSaveBtn    *walk.PushButton
	globalRefreshBtn *walk.PushButton
	announceColorCb  *walk.ComboBox
	announceBtn      *walk.PushButton
	announceMsgL     *walk.Label

	// 定时公告（服务器选项页）控件：排队多条"一次性/每天/每周"公告，到点在服务器上自动发送。
	asContentLE    *walk.LineEdit
	asColorCb      *walk.ComboBox
	asModeCb       *walk.ComboBox
	asTimeHourCb   *walk.ComboBox
	asTimeMinCb    *walk.ComboBox
	asDateLE       *walk.LineEdit
	asDateLabel    *walk.Label
	asWeekdayCb    *walk.ComboBox
	asWeekdayLabel *walk.Label
	asAddBtn       *walk.PushButton
	asUpdateBtn    *walk.PushButton
	asDelBtn       *walk.PushButton
	asSaveBtn      *walk.PushButton
	asRefreshBtn   *walk.PushButton
	asTable        *walk.TableView
	asMsgL         *walk.Label
	asBusy         bool
	// asItems 表格行与 ScheduledAnnouncement 一一对应（id 由服务端维护，保存时原样回传）。
	asItems  []ScheduledAnnouncement
	asLoaded bool

	// 服务器公告栏（登录器登录页左侧公告板，服务器选项页）控件：标题 + 日期 + 启用
	ntTitleLE    *walk.LineEdit
	ntDateLE     *walk.LineEdit
	ntAddBtn     *walk.PushButton
	ntUpdateBtn  *walk.PushButton
	ntDelBtn     *walk.PushButton
	ntSaveBtn    *walk.PushButton
	ntRefreshBtn *walk.PushButton
	ntTable      *walk.TableView
	ntMsgL       *walk.Label
	ntBusy       bool
	ntItems      []LauncherNotice
	// ntLoaded 是否成功从服务器加载过一次公告栏。
	// 用途：公告栏是"整表替换"保存，内存里的空表提交上去 = 把服务器上的公告清空；
	// 「保存全局配置」用它挡掉"还没刷新回来/刷新失败"的空表（见 global_config.go）。
	ntLoaded bool

	// 可加载 DLL（服务器选项页）控件：选本地 dll 算 SHA256 → 列表 → 整表保存；
	// 登录器（外网版）启动时校验客户端根目录同名文件并注入。
	dlPickBtn    *walk.PushButton
	dlNameLE     *walk.LineEdit
	dlHashLE     *walk.LineEdit
	dlPolicyCb   *walk.ComboBox
	dlAddBtn     *walk.PushButton
	dlUpdateBtn  *walk.PushButton
	dlDelBtn     *walk.PushButton
	dlSaveBtn    *walk.PushButton
	dlRefreshBtn *walk.PushButton
	dlTable      *walk.TableView
	dlMsgL       *walk.Label
	dlBusy       bool
	dlItems      []LauncherDLL
	dlPickedPath string // 最近一次选中的本地 dll 路径（仅用于显示与计算哈希，不上传文件本体）
	// dlLoaded 是否成功从服务器加载过一次 DLL 清单（同 ntLoaded：防止空表整表替换把清单清空）。
	dlLoaded bool

	// 泡点页控件
	psChk   *walk.CheckBox
	psIntLE *walk.LineEdit
	psPtsLE *walk.LineEdit
	// psLimitLE 账号级每日限额（当前币种的数量；0/留空 = 不限）。
	psLimitLE *walk.LineEdit
	// psCurCb 发放币种：点券 / 代币券 / 欢乐代币券（2026-10-05 起可选）。
	psCurCb   *walk.ComboBox
	psSaveBtn *walk.PushButton
	psRefresh *walk.PushButton
	psStatus  *walk.TextEdit
	psBusy    bool

	// 注册赠送（服务器选项页）控件
	rbChk     *walk.CheckBox
	rbPtsLE   *walk.LineEdit
	rbCurCb   *walk.ComboBox // 赠送币种：点券 / 代币券
	rbSaveBtn *walk.PushButton
	rbRefresh *walk.PushButton
	rbStateL  *walk.Label
	rbBusy    bool

	// 注册限制（服务器选项页）控件
	regChk     *walk.CheckBox
	regMaxIPLE *walk.LineEdit // 每 IP 最多注册账号数（0=不限制）
	regSaveBtn *walk.PushButton
	regRefresh *walk.PushButton
	regStateL  *walk.Label
	regBusy    bool

	// 注册码（邀请码，服务器选项页）控件：勾选"注册需要注册码"+ 生成/列表/复制/作废。
	//
	// regCodeReloadBtn 是 2026-09-28 补的「刷新列表」按钮：列表原来只有"打开工具时自动拉一次 /
	// 切换显示下拉 / 生成之后 / 作废之后"四个刷新时机，而页面上那个「刷新状态」刷的是
	// **注册限制策略**、不是注册码列表 —— 运营很容易点错并以为"刷新不好使"。
	regCodeChk       *walk.CheckBox
	regCodeCount     *walk.LineEdit
	regCodeGenBtn    *walk.PushButton
	regCodeList      *walk.ListBox
	regCodeCopyBtn   *walk.PushButton
	regCodeRevokeBtn *walk.PushButton
	regCodeReloadBtn *walk.PushButton
	regCodeStateL    *walk.Label
	regCodeFilter    *walk.ComboBox
	regCodeBusy      bool
	regCodeItems     []RegistrationCode // 列表行与 RegistrationCode 一一对应
	// regCodeStartedAt 本次注册码刷新的开始时刻：用来判断"上一次查询是不是卡死了"（见 regCodeStuckAfter）。
	regCodeStartedAt time.Time

	// 更新维护（服务器选项页）控件：维护期间玩家不能登录/进游戏（白名单放行；注册照常）
	maintChk     *walk.CheckBox
	maintAllowLE *walk.LineEdit
	maintSaveBtn *walk.PushButton
	maintRefresh *walk.PushButton
	maintStateL  *walk.Label
	maintBusy    bool

	// 登录器版本校验（服务器选项页）控件：登录器自检/点登录时上报版本号，
	// 与服务端内置版本号完全一致才放行；不符则强制更新（更新前禁止进入登录界面）。
	// 逻辑见 lv_admin_tab.go。
	lvChk     *walk.CheckBox
	lvUrlLE   *walk.LineEdit
	lvSaveBtn *walk.PushButton
	lvRefresh *walk.PushButton
	lvStateL  *walk.Label
	lvBusy    bool
	// lvVersionCache 最近一次从服务器回读到的"要求的登录器版本号"（只读，仅在内存）。
	// 只用于保存确认框里显示真实版本号；服务端才是权威来源。
	lvVersionCache string

	// 双开（多开）开关（服务器选项页）控件：控制登录器「双开」勾选框能不能用。
	// 逻辑见 multi_open_admin_tab.go。
	// ⚠ 关闭**不会**踢掉已经在跑的副账号 —— 只影响"以后还能不能再开"。
	moChk     *walk.CheckBox
	moSaveBtn *walk.PushButton
	moRefresh *walk.PushButton
	moStateL  *walk.Label
	moBusy    bool
	// moLoaded 是否成功从服务器加载过一次双开开关。
	// 与公告/福利那些"整组替换"的组不同：本组只有一个 bool，没加载成功也不该拿
	// 界面上的默认勾选去覆盖服务器配置（一键保存会先看这个标记，见 globalSaveMultiOpen）。
	moLoaded bool

	// 在线福利页控件（三档期细节在 welfare_admin_tab.go 的 welfareSlots 里）
	wfRefreshBtn *walk.PushButton
	wfSaveBtn    *walk.PushButton
	wfStateL     *walk.Label
	wfBusy       bool
	// wfLoaded 是否成功从服务器加载过一次在线福利（三档期是整组替换保存，
	// 没加载成功就提交 = 把服务器上已配好的福利改回"三档全停用"）。
	wfLoaded bool

	// 管理员建号/改密/强制下线（服务器选项页）控件
	adUserLE   *walk.LineEdit
	adPassLE   *walk.LineEdit
	adRescueLE *walk.LineEdit
	adRegBtn   *walk.PushButton
	adPwUserLE *walk.LineEdit
	adPwNewLE  *walk.LineEdit
	adPwBtn    *walk.PushButton
	adKoUserLE *walk.LineEdit
	adKoBtn    *walk.PushButton
	adMsgL     *walk.Label
	adBusy     bool

	// PVF 加密页控件
	pvSrcLE         *walk.LineEdit
	pvBrowseSrcBtn  *walk.PushButton
	pvEncBtn        *walk.PushButton
	pvUrlLE         *walk.LineEdit   // 客户端更新包直链（zip/7z/rar，可选）
	pvClientLE      *walk.LineEdit   // 客户端整包直链（zip/7z/rar，可选；本机无客户端时下载用）
	pvEnableChk     *walk.CheckBox   // 是否启用 PVF 加密校验
	pvReportBtn     *walk.PushButton // 重新上报（读本地记录补报）
	pvSaveUrlBtn    *walk.PushButton // 保存更新包直链（复用最近一次加密记录上报）
	pvSaveClientBtn *walk.PushButton // 保存整包直链（复用最近一次加密记录上报）
	pvResult        *walk.TextEdit
	pvBusy          bool
	pvLastEnc       *pvfLocalRecord // 最近一次本地加密结果（含 key_set，供补报）

	// 数据库备份页控件（P1 备份闭环：远程读写运维工具自动备份配置/列表/还原）
	bkChk        *walk.CheckBox
	bkIntEdit    *walk.LineEdit
	bkKeepEdit   *walk.LineEdit
	bkSaveBtn    *walk.PushButton
	bkNowBtn     *walk.PushButton
	bkRefreshBtn *walk.PushButton
	bkList       *walk.ListBox
	bkRestoreBtn *walk.PushButton
	bkStatusText *walk.TextEdit
	bkNextL      *walk.Label
	bkFiles      []string // 当前列表行对应的服务器端备份文件名

	opMu sync.Mutex // 写操作互斥（防止并发管理操作）

	relogin bool // 主窗口关闭后是否需要回到登录窗口

	// lastKickResult 最近一次批量踢下线结果（acctRun 的 job 闭包写、done 回调读，
	// 两者一前一后不会并发，所以不加锁）。
	lastKickResult *AdminKickResult
}

func main() {
	a := &App{}
	a.store = NewConfigStore(DefaultConfigPath())
	cfg, err := a.store.Load()
	if err != nil {
		log.Printf("config load failed, use default: %v", err)
		cfg = DefaultLocalConfig()
	}
	a.cfg = cfg

	// PVF 索引 DFIX 落盘缓存目录：程序同目录 pvf\（按 PVF 指纹分文件）。
	pvfindex.SetCacheDir(filepath.Join(filepath.Dir(DefaultConfigPath()), "pvf"))

	for {
		if !a.runLoginDialog() {
			return // 用户在登录窗口选择退出
		}
		if !a.runMainWindow() {
			return // 用户关闭主窗口
		}
		// a.relogin == true：登出或令牌过期，回到登录窗口继续循环
	}
}

// runMainWindow 创建并运行主窗口。返回 false 表示退出程序；
// true 表示需要重新登录（登出/令牌过期）。
func (a *App) runMainWindow() bool {
	a.relogin = false

	var mw *walk.MainWindow
	// 窗口尺寸按屏幕自适应：写死 1280x768 在 768 高的屏幕上底部会被任务栏吃掉，
	// 按钮点不到（见 window_size.go 的说明）。
	winSize, winMinSize := desktopWindowSize()
	if err := (decl.MainWindow{
		AssignTo: &mw,
		Title:    "DNF90 后台管理工具",
		MinSize:  winMinSize,
		Size:     winSize,
		Layout:   decl.VBox{MarginsZero: true, SpacingZero: true},
		Children: []decl.Widget{
			decl.Composite{
				Layout: decl.HBox{Margins: decl.Margins{8, 6, 8, 4}, Spacing: 8},
				Children: append([]decl.Widget{
					decl.Label{AssignTo: &a.connInfoL, Text: "已连接"},
					// 一键保存 / 刷新全部配置（见 global_config.go）
				}, append(a.globalSaveButtons(), []decl.Widget{
					decl.HSpacer{},
					decl.PushButton{
						AssignTo:  &a.logoutBtn,
						Text:      "登出",
						OnClicked: func() { a.onLogout() },
					},
				}...)...),
			},
			decl.TabWidget{
				AssignTo: &a.tab,
				Pages:    buildAllPages(a),
			},
		},
	}).Create(); err != nil {
		log.Fatalf("create window failed: %v", err)
	}
	a.mw = mw
	a.connInfoL.SetText(fmt.Sprintf("已连接 %s:%d", a.cfg.ServerIP, a.cfg.Port))

	// 记录查询页「最多加载」的默认值只能在这里设：declarative 的 NumberEdit.Value
	// 传 int 会退化成 0.0 让 Create() 失败，不写又会被 SetRange 夹成 MinValue=1
	// （查询只回 1 条）。见 record_tab.go 里 recDefaultLoad 的注释。
	if err := applyRecordTabDefaults(); err != nil {
		log.Printf("记录查询页默认值初始化失败: %v", err)
	}

	mw.Closing().Attach(func(canceled *bool, reason walk.CloseReason) {
		a.stopPolling()
	})

	a.startPolling()
	go a.refreshStatusOnce(true)

	// 打开主窗口即拉取服务器选项页配置（泡点/注册赠送/注册限制），回填勾选与数值
	a.onPointsinkRefresh(true)
	a.onRegisterBonusRefresh(true)
	a.onRegistrationLimitRefresh(true)
	// 注册码列表：与注册限制同一页，打开窗口时静默拉一次（失败不弹窗）
	a.onRegCodeRefresh(true)
	// 更新维护状态：同一页，打开窗口时静默拉一次（失败不弹窗）
	a.onMaintenanceRefresh(true)
	// 登录器版本校验状态：同一页，打开窗口时静默拉一次（失败不弹窗）。
	// 必须拉这一次 —— 保存确认框要靠它显示服务端内置的真实版本号。
	a.onLauncherVersionRefresh(true)
	// 双开（多开）开关：同一页，打开窗口时静默拉一次（失败不弹窗）。
	// 必须拉这一次 —— 除了回填勾选，还关系到"一键保存"能不能安全提交：
	// 没成功加载过就提交，会拿界面上的默认勾选去覆盖服务器配置（见 globalSaveMultiOpen）。
	a.onMultiOpenRefresh(true)
	// 定时公告列表：同一页（全服公告下方），打开窗口时静默拉一次
	a.onAnnounceScheduleRefresh(true)
	// 服务器公告栏：同一页，打开窗口时静默拉一次
	a.onNoticesRefresh(true)
	// 可加载 DLL 清单：同一页，打开窗口时静默拉一次
	a.onDllRefresh(true)
	// 在线福利：独立页，打开窗口时静默拉一次
	a.onWelfareRefresh(true)

	// 数据库备份页：拉取服务器端自动备份配置（静默，失败不弹窗）
	go a.refreshBkAll(false)

	mw.Run()

	a.stopPolling()
	return a.relogin
}

func buildAllPages(a *App) []decl.TabPage {
	return []decl.TabPage{
		buildStatusTab(a),
		buildAnnounceTab(a),
		buildAcctTab(a),
		buildInventoryTab(a),
		buildChannelTab(a),
		buildRecordTab(a),
		buildBanTab(a),
		buildCdkTab(a),
		buildPvfTab(a),
		buildBackupTab(a),
		buildWelfarePage(a),
	}
}

// ---------------- 通用 UI 辅助 ----------------

// sync 将 fn 投递到 GUI 线程执行（后台 goroutine 更新 UI 必须走这里）。
func (a *App) sync(fn func()) {
	if a.mw == nil {
		return
	}
	a.mw.Synchronize(fn)
}

// confirm 弹二次确认框（在 GUI 线程调用），返回是否确认。
func (a *App) confirm(title, msg string) bool {
	r := walk.MsgBox(a.mw, title, msg, walk.MsgBoxYesNo|walk.MsgBoxIconQuestion)
	return r == walk.DlgCmdYes
}

func (a *App) showInfo(title, msg string) {
	a.sync(func() {
		walk.MsgBox(a.mw, title, msg, walk.MsgBoxOK|walk.MsgBoxIconInformation)
	})
}

func (a *App) showError(title string, err error) {
	if err == nil {
		return
	}
	a.sync(func() {
		walk.MsgBox(a.mw, title, err.Error(), walk.MsgBoxOK|walk.MsgBoxIconError)
	})
}

// beginOp / endOp 互斥管理写操作，避免并发管理动作（启停/封禁/生成等）。
func (a *App) beginOp() bool { return a.opMu.TryLock() }
func (a *App) endOp()        { a.opMu.Unlock() }

// snapshotClient 返回当前登录客户端（无锁快速读用 clientMu 保护引用一致性）。
func (a *App) snapshotClient() *APIClient {
	a.clientMu.Lock()
	defer a.clientMu.Unlock()
	return a.client
}

// saveConfig 保存本地配置（带写锁）。
func (a *App) saveConfig() {
	a.cfgSaveM.Lock()
	defer a.cfgSaveM.Unlock()
	if err := a.store.Save(a.cfg); err != nil {
		log.Printf("save config failed: %v", err)
	}
}

// handleAPIError 统一 API 错误：token 失效时自动提示并回到登录窗口。
// 调用方传入 err；返回 true 表示已处理为失效（无需再提示）。
func (a *App) handleAPIError(err error) bool {
	if err == nil {
		return false
	}
	var apie *APIError
	if ok := asAPIError(err, &apie); ok && apie.IsUnauthorized() {
		a.requireRelogin()
		return true
	}
	return false
}

// leText 读取 LineEdit 文本。
func leText(le *walk.LineEdit) string {
	if le == nil {
		return ""
	}
	return le.Text()
}

// setEditText 写入多行文本：把 LF 归一为 CRLF。
// Windows 标准 EDIT 控件只认 CRLF 换行，纯 LF 会挤成一行；
// 文本本身是 LF，所以“复制出来正常、框里乱”。
func setEditText(ed *walk.TextEdit, s string) {
	if ed == nil {
		return
	}
	s = strings.ReplaceAll(s, "\r\n", "\n")
	s = strings.ReplaceAll(s, "\n", "\r\n")
	ed.SetText(s)
}

func asAPIError(err error, target **APIError) bool {
	for err != nil {
		if e, ok := err.(*APIError); ok {
			*target = e
			return true
		}
		type unwrapper interface{ Unwrap() error }
		u, ok := err.(unwrapper)
		if !ok {
			return false
		}
		err = u.Unwrap()
	}
	return false
}
