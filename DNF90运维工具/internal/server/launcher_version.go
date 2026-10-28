// 登录器版本校验：给登录器的公开端点（免 ticket）。
//
// 背景与三端联动说明见主程序 launcher_version.go（那里是版本号的唯一事实源）。
//
// 登录器（**外网版 · 远程模式**）在两处主动拉这个接口：
//  1. 启动自检阶段（MainWindow.RunDemoFlowAsync）—— 版本不符就地更新，
//     **更新完成前禁止进入登录界面**；
//  2. 玩家点「登录」时再拉一次 —— 防止"自检通过之后服务端才发布新版本、
//     拿着旧登录器继续登进去"。
//
// 为什么必须免 ticket：第 1 处发生在玩家还没登录的时候，登录器手里根本没有票。
// 与 /launcher/v1/maintenance、/launcher/v1/registration-info、/launcher/v1/dlls 同一口径。
//
// ── 光有这个接口是不够的：整个 API 外面还罩着一层版本闸 ──────────────────────
// 这个接口只能管住**新登录器**。玩家手头的老登录器根本不知道"版本校验"这回事，
// 它永远不会来问，于是"开开关"对它们完全无效 —— 而它们恰恰是这个功能唯一想清理的
// 那批人。结论：/launcher/v1/* 整个外面罩一层 launcherVersionMiddleware：
//
//	没带 X-Client-Launcher-Version 请求头 = 老登录器 = **每一个接口都 403**，
//	只有 /launcher/v1/version-check 放行（那是它问"我该是什么版本"的唯一入口）。
//
// 也就是说开关一开，老登录器：不给注册、不给登录、不给探测在线、不给拉公告/泡点/
// PVF/CDK/心跳 —— 什么都拿不到，只剩一句"版本过旧，请去下载最新登录器"。
//
// 为什么老登录器收到 403 不会"莫名其妙"：登录器 RemoteAuthService.FriendlyApiError
// 会把服务端返回的中文 message 原样展示（90 远程登录失败时走 ShowStatus），
// 所以文案本身就是给玩家看的操作指引 —— 这与"更新维护"用的是同一条链路。
package server

import (
	"net/http"
	"strings"
)

// LauncherVersionHeader 登录器上报自身版本号的请求头（值为 LauncherVersion.Current）。
//
// 新登录器在 CreateClient 里给所有 /launcher/v1/* 直连请求都带上它；
// 老登录器（加这个功能之前的任何版本）**一定不带** —— 这正是服务端识别它们的手段。
//
// 注意：这是"客户端自报"，可以被伪造。这里不追求防作弊（那要靠 client.yh 那条链），
// 只追求"让还在用老包的玩家被拦住并被告知去更新"。
const LauncherVersionHeader = "X-Client-Launcher-Version"

// launcherVersionBlockCode 版本过旧的错误码（登录器侧不依赖它做分支，只给人看与排障）。
const launcherVersionBlockCode = "launcher-version"

// launcherVersionMsgKeywords **必须**与登录器 MapServerCodeToChinese 的触发词保持一致。
//
// 为什么服务端要知道这个：老登录器拿到 403 之后，是把服务端的中文 message 直接展示给
// 玩家的；但那个函数会先在整个消息里扫这些英文关键词，命中就把消息替换成它自己的文案。
// 例如消息里出现 "rate"，玩家会看到「操作过于频繁」而不是「版本过旧」—— 完全误导，
// 而且我们改不了老登录器的解析逻辑。
//
// 这张表的用法（2026-09-28 调整）：固定文案 launcherVersionBlockText 落地之后，
// 文案里已经没有任何变量（直链不再进消息），所以它不再用于"直链体检"，而是变成一条
// **契约守卫** —— 单测用它断言「固定文案 + 错误码」永远不含任何一个触发词。
// 将来谁改文案（比如顺手加个"维护"两个字）会在 CI 上直接挂掉，而不是等到线上
// 玩家看到一句莫名其妙的话才发现。
//
// 维护纪律：登录器 MapServerCodeToChinese 增删关键词时，这里要同步（有单测锁住基本盘）。
var launcherVersionMsgKeywords = []string{
	// 封禁 / 维护
	"banned", "封禁", "maintenance", "维护",
	// 账号名 / 密码字段校验
	"invalid-username", "invalid_username", "username", "3-32", "3 到 32", "3 to 32", "too short",
	"character", "characters", "只能包含", "字母",
	"password-too-short", "password_too_short", "at least 8", "8 characters", "password", "too long", "72",
	// 注册码
	"registration-code-required", "invalid-invite", "invite", "邀请码", "注册码",
	// 账号已存在 / 不存在 / 密码错
	"exists", "already", "duplicate", "taken", "已注册",
	"not-found", "not found", "不存在", "未注册",
	"wrong-password", "invalid-credentials", "invalid password", "密码错误",
	// 限流
	"rate", "frequent", "频繁",
}

// LauncherVersionState 登录器版本校验状态快照（由主程序在同一把锁下一次读出）。
type LauncherVersionState struct {
	// Enabled 校验总开关：true 时登录器版本必须与 RequiredVersion 完全一致。
	Enabled bool
	// DownloadURL 登录器下载直链（压缩包），由 /launcher/v1/version-check 下发给**新登录器**
	// 做自动更新用。
	//
	// 注意：它**不会**出现在拒绝老登录器的那句提示里 —— 那句是固定文案
	// （launcherVersionBlockText），见那里的说明。本字段为空时新登录器更新不了，
	// 会提示"请联系管理员"。
	DownloadURL string
	// RequiredVersion 服务端内置要求的登录器版本号（编译期常量，只读）。
	RequiredVersion string
}

// LauncherVersionFunc 返回登录器版本校验实时快照（线程安全，由主程序注入）。
// nil = 未注入 = 始终"未开启校验"（行为与改造前一致：老装配路径/测试不受影响）。
type LauncherVersionFunc func() LauncherVersionState

// SetLauncherVersion 注入登录器版本校验状态读取函数。
func (s *Service) SetLauncherVersion(fn LauncherVersionFunc) { s.launcherVersionFn = fn }

// launcherVersionState 当前版本校验状态快照；每项都做归一化，保证对外永远是自洽的。
func (s *Service) launcherVersionState() LauncherVersionState {
	if s.launcherVersionFn == nil {
		return LauncherVersionState{}
	}
	state := s.launcherVersionFn()
	state.RequiredVersion = strings.TrimSpace(state.RequiredVersion)
	state.DownloadURL = strings.TrimSpace(state.DownloadURL)

	// 服务端没配版本号（内置常量为空/异常）→ 一律按"未开启校验"返回。
	// 宁可不校验，也不能因为一个空常量把全服玩家锁在更新界面外面。
	if state.RequiredVersion == "" {
		return LauncherVersionState{}
	}
	// 开关没开：直链也不必下发（避免登录器误用）。
	if !state.Enabled {
		state.DownloadURL = ""
	}
	return state
}

// launcherVersionResponse GET /launcher/v1/version-check 的 data 项。
//
// 字段语义（登录器的判断顺序，两端必须一致）：
//
//	enabled          —— 没开校验：登录器**不再看后面任何字段**，直接放行。
//	required_version —— 开了校验时，登录器拿自己内置的版本号与它做**完全一致**比对。
//	download_url     —— 版本不符时去哪儿下新登录器；空串 = 管理员没填直链，
//	                    登录器弹「登录器版本不符，请联系管理员」并禁止进入。
//	configured       —— 服务端算好的"能不能真的更新"（= enabled && 直链非空）。
//	                    登录器可以直接用它，省得自己拼条件拼错；纯冗余字段，不改变语义。
type launcherVersionResponse struct {
	Enabled         bool   `json:"enabled"`
	RequiredVersion string `json:"required_version"`
	DownloadURL     string `json:"download_url"`
	Configured      bool   `json:"configured"`
}

// handleLauncherVersionCheck 公开端点（无需 ticket）：登录器自检与点登录时各拉一次。
//
// 这个接口**永远返回 200**（只要认证服务活着）：版本不符不是"请求失败"，
// 而是"一个正常业务结果"。登录器据此走更新流程，不走错误分支。
func (s *Service) handleLauncherVersionCheck(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodGet {
		writeError(w, http.StatusMethodNotAllowed, "method-not-allowed", "GET required")
		return
	}
	state := s.launcherVersionState()
	writeJSON(w, http.StatusOK, map[string]any{"ok": true, "data": launcherVersionResponse{
		Enabled:         state.Enabled,
		RequiredVersion: state.RequiredVersion,
		DownloadURL:     state.DownloadURL,
		Configured:      state.Enabled && state.DownloadURL != "",
	}})
}

// ============================================================================
//  老登录器判定与拒绝文案（判定结果由下面的整站中间件统一使用）
// ============================================================================

// launcherVersionBlockText 老登录器看到的**固定文案**（需求 2026-09-28 确认）。
//
// 「无论有没有配下载直链，都固定这一句」—— 为什么这么定：
//
//  1. **带直链在界面上根本用不了**。老登录器登录失败走的是右上角 Toast：固定 320px 宽、
//     3 秒后自动滑走，没有弹窗、没有常驻文字、也选不中复制。而运营实际配的直链长这样：
//     https://1830977083.cdn.123clouddisk.com/1830977083/%E7%9B%B4%E9%93%BE/%E5%BC%82%E7%8E%AF...
//     —— 中文全被百分号编码，一条就折成 6 行，3 秒读不完、复制不了，等于什么信息都没给到，
//     还把"版本过旧"这句话本身挤没了。
//  2. **固定文案才可控**。文案里没有任何变量，就不存在"某条直链碰巧含关键词、
//     被老登录器的错误映射吃成别的意思"这一类问题（见 launcherVersionMsgKeywords）。
//  3. 玩家的动作本来就是明确的：**找管理员要新包**。运营在群里/Q 群里发一次链接，
//     比指望玩家从一条 3 秒的 Toast 里抄下一个百分号编码的长地址可靠得多。
//
// 注意：这**不代表**「登录器下载直链」那个配置项没用 —— 它仍然是**新登录器自更新**的
// 下载地址（走 /launcher/v1/version-check 下发的 download_url），那才是它的主要用途。
const launcherVersionBlockText = "登录器版本过旧，请联系管理员下载新版登录器"

// launcherVersionBlocks 判断这次请求是否因为登录器版本问题被拒绝。
// 返回 (是否拒绝, 给玩家看的中文原因)。
//
// 判定表（state.Enabled=false 时一律放行，行为与改动前完全一致）：
//
//	请求头缺失               → 拒绝。老登录器不认识版本校验，永远不带这个头。
//	请求头 == 要求版本       → 放行。
//	请求头 != 要求版本       → 拒绝（玩家手改了头、或是自己编译的版本）。
//
// 为什么"缺头"必须当成拒绝而不是放行：这是整个功能唯一能覆盖老登录器的手段。
// 放行的话，"开启登录器版本校验"就只对已经会自更新的新登录器生效 —— 毫无意义。
//
// 为什么不像 maintenance 那样也按白名单放行：维护是临时状态，需要给管理员留自测口子；
// 版本校验是"你必须先更新"，管理员自己也有管理工具可以随时把开关关掉，不需要白名单。
func (s *Service) launcherVersionBlocks(r *http.Request) (bool, string) {
	state := s.launcherVersionState()
	if !state.Enabled {
		return false, ""
	}
	got := strings.TrimSpace(r.Header.Get(LauncherVersionHeader))
	if got == state.RequiredVersion {
		return false, ""
	}
	return true, launcherVersionBlockText
}

// ============================================================================
//  整站版本闸：老登录器一个接口都拿不到
// ============================================================================

// launcherVersionOpenPaths 版本闸的**放行白名单**。
//
// 只有 /launcher/v1/version-check 在里面，而且它**必须**在里面：
// 那是"问我现在该是什么版本"的唯一入口。把它也拦掉，版本不对的登录器就问不到
// 该更新到哪一版、去哪儿下载，直接变成死局（玩家只会看到"无法校验版本，请联系管理员"，
// 管理员则完全不知道问题出在哪）。所以口径是：
//
//	**发现真相的接口永远开着，其余一律关死。**
var launcherVersionOpenPaths = map[string]struct{}{
	"/launcher/v1/version-check": {},
}

// launcherVersionMiddleware 给整个登录器公开 API（/launcher/v1/*）罩一层版本闸。
//
// 为什么做成中间件而不是在登录接口上拦一道：需求是"老登录器什么都不给用"——
// 不给注册、不给探测在线、不给拉公告/泡点/PVF/CDK/心跳……逐个接口加判断既容易漏，
// 也会在将来新增接口时忘记加。罩在整个 mux 外面，**默认全拦、白名单放行**，
// 新增接口自动被保护，漏不了。
//
// 判定见 launcherVersionBlocks：
//   - 开关没开 / 没装配 → 全部放行，行为与改动前完全一致；
//   - 开关开着 → 请求头必须是本端内置版本号，否则 403 + 给玩家看的中文原因。
//     老登录器永远不带这个头，所以它们会撞在**每一个**接口上。
//
// 与 editionGuard 的关系：本中间件在更外层，先判版本再判版本互斥。
// 副作用是"免费版登录器打赞助版运维工具"现在会先看到"版本过旧"而不是"版本不匹配"——
// 但这个开关只有赞助版运维工具能开、且默认关闭，属于运营自己的选择，可以接受。
func (s *Service) launcherVersionMiddleware(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		// 只管登录器公开 API：将来若在这个 mux 上挂别的东西，不会被误伤。
		if !strings.HasPrefix(r.URL.Path, "/launcher/v1/") {
			next.ServeHTTP(w, r)
			return
		}
		if _, open := launcherVersionOpenPaths[r.URL.Path]; open {
			next.ServeHTTP(w, r)
			return
		}

		// /launcher/v1/health 例外：**版本互斥的判断必须先于版本闸**。
		//
		// health 是登录器判断"我是不是连错了服务器版本种类"的唯一途径 ——
		// 服务端在它的响应里回 server.edition，登录器据此禁用登录按钮并提示
		// 「该服务器为赞助版运维工具，仅限外网版登录器连接」。
		// 而版本闸罩在整个 mux 外面、先于 editionGuard 执行，一旦把 health 也 403 掉，
		// 登录器那边 `if (health.Ok)` 整段（含版本互斥检查）就被跳过 ——
		// 玩家看到的会是「登录器版本过旧」，于是他跑去折腾登录器版本号，
		// 而实际上他该换的是登录器**种类**（免费版 ↔ 外网版）。病因指错方向。
		//
		// 所以只对"**明确连错**"这一种情况放行（带了 edition 头但与本文不符）。
		// 完全不带 edition 头的是更老的登录器，不属于版本互斥场景，
		// 必须继续被版本闸拦下 —— 需求要的"老登录器连在线都不给探测"不能被这条例外破掉。
		if r.URL.Path == "/launcher/v1/health" && editionHeaderConflicts(r) {
			next.ServeHTTP(w, r)
			return
		}

		blocked, reason := s.launcherVersionBlocks(r)
		if !blocked {
			next.ServeHTTP(w, r)
			return
		}

		// 只为"玩家主动发起、值得记一笔"的路径打日志。
		// 老登录器每 10 秒就会轮询一次 health / gm-events，逐个记会把日志刷爆，
		// 反而看不到真正重要的"有人在拿老登录器登"。
		switch r.URL.Path {
		case "/launcher/v1/login", "/launcher/v1/register":
			s.log.Info("launcher version rejected a request",
				"path", r.URL.Path,
				"got", strings.TrimSpace(r.Header.Get(LauncherVersionHeader)),
				"required", s.launcherVersionState().RequiredVersion)
		}
		writeError(w, http.StatusForbidden, launcherVersionBlockCode, reason)
	})
}
