package main

// ============================================================================
//  登录器版本号 —— 三端联动功能「登录器版本校验」的核心
// ============================================================================
//
// ★★★ 每次发布新登录器，必须同步修改 DNF90Ops.build.json 中的 launcherVersion ★★★
//
// 这个常量就是全服"当前应该使用的登录器版本号"。登录器（外网版 · 远程模式）在
// **启动自检阶段**和**点登录时**各上报一次自己 exe 里内置的版本号，服务端拿它和
// 本常量做**完全一致**的字符串比对，不一致就下发"需要更新"，登录器立刻走强制更新
// 流程（下载 → 删原登录器 → 拉起新版本），**更新完成前禁止进入登录界面**。
//
// ── 版本号一共存在于这几处，发布时请一起改（顺序无所谓，但必须全改到）────────────
//   1) 运维工具   internal/launcherver/DNF90Ops.build.json
//      → 生效方式：重新编译运维工具并重启认证服务（配置在构建时嵌入）。
//   2) 登录器              LauncherVersion.cs → LauncherVersion.Current
//      → 生效方式：重新发布登录器。
//   3) 登录器「设置 → 关于」页显示的版本号
//      → **不用单独改**：SettingsControl 已改成绑定 LauncherVersion.Current，
//        改第 2 处即可，关于页会自动跟着变。
//   4) 管理工具            **不用改**：它没有本地副本，"当前要求的版本号"是从
//      运维工具的管理接口（GET /manage/v1/launcher-version → required_version）读回来的，
//      只用于在「服务器选项 → 登录器版本校验」分组里显示，方便运营核对。
//
// ⚠ 这段注释里**故意不写具体版本号**：以前这里抄了一份 "1.5.5"，常量升到 1.5.6 之后
//   注释就变成了错的，用 grep 核对版本号的人会先匹配到注释、得出"没改"的错误结论
//   （2026-09-28 真踩过）。要核对版本号请直接看 DNF90Ops.build.json。
//
// ── 为什么版本号放在服务端而不是让管理工具填 ────────────────────────────────
// 需求明确要求"版本号内置在运维工具中"。这样运营在管理工具里只需要：
//   ☑ 勾选「登录器版本校验」 + 填一个「登录器下载直链」 → 保存，全服立刻生效。
// 版本号本身不开放编辑，避免手滑填错把全服玩家全部锁在更新界面。
//
// ── 与「更新维护」的关系 ─────────────────────────────────────────────────
// 两者互不干扰：维护是"谁都别登"，版本校验是"版本不对先去更新"。
//
// 实现细节见：
//   - 对外接口（给登录器）：internal/server/launcher_version.go
//   - 管理接口（给管理工具）：internal/manage/handlers_launcher_version.go
//   - 配置落盘字段：internal/config/config.go → LauncherVersionCheck / LauncherDownloadURL

import (
	"context"
	"fmt"
	"net/url"
	"strings"

	"dnf90ops/internal/launcherver"
	"dnf90ops/internal/server"
)

// builtinLauncherVersion 运维工具内置的登录器版本号（来自构建参数文件）。
//
// 发布新登录器时修改 internal/launcherver/DNF90Ops.build.json，
// 然后重新编译运维工具并重启认证服务。
//
// 格式：与登录器 LauncherVersion.Current 逐字符完全一致（大小写敏感、不带 "v" 前缀）。
// 留空表示"服务端没配版本号"→ 对外接口会按**未开启校验**返回，登录器直接放行
// （防止运营手滑把版本号删了导致全服进不去）。
//
// 2026-10-17：常量搬到 internal/launcherver，让无 GUI 的认证服务入口（cmd/serve）
// 也能读到同一个号 —— 否则两边各写一份，升版本时必然漂移。
var builtinLauncherVersion = launcherver.Required

const (
	// lvMaxURLLen 直链长度上限（只做前端预校验的兜底，防止异常长的串写进 config.json）。
	lvMaxURLLen = 2048
	// lvMaxVersionLen 版本号长度上限（内置常量不可能超，纯防御：将来若改成可配置）。
	lvMaxVersionLen = 64
)

// lvStat 登录器版本校验配置快照。
//
// 与 paid 版 manage.LauncherVersionStatus 字段一一对应（JSON tag 必须一致）。
// RequiredVersion 是只读的（来自编译期常量），管理工具不能改。
type lvStat struct {
	// Enabled 校验总开关：true 时登录器版本必须与 RequiredVersion 完全一致。
	Enabled bool `json:"enabled"`
	// DownloadURL 登录器下载直链（压缩包）。Enabled=true 且本字段为空 =
	// 登录器会弹「请联系管理员」并禁止进入 —— 这是一个**合法但危险**的状态，
	// 管理工具会把它标红提醒。
	DownloadURL string `json:"download_url"`
	// RequiredVersion 服务端内置要求的登录器版本号（只读）。
	RequiredVersion string `json:"required_version"`
}

// lvPublicState 返回登录器版本校验实时快照（认证服务注入用；线程安全）。
//
// 与 maintPublicState 一样在同一把配置锁里一次读完，避免读到"开关已改、直链还没改"的混合快照。
func (a *App) lvPublicState() server.LauncherVersionState {
	// 服务端没配版本号（常量被清空）：对外一律按"未开启校验"处理 ——
	// 宁可不校验，也不能因为一个空常量把全服玩家锁在更新界面外面。
	if lvEffectiveVersion() == "" {
		return server.LauncherVersionState{}
	}

	a.psCfgMu.Lock()
	defer a.psCfgMu.Unlock()
	if a.cfg == nil {
		return server.LauncherVersionState{}
	}
	return server.LauncherVersionState{
		Enabled:         a.cfg.LauncherVersionCheck,
		DownloadURL:     strings.TrimSpace(a.cfg.LauncherDownloadURL),
		RequiredVersion: lvEffectiveVersion(),
	}
}

// lvEffectiveVersion 归一化后的内置版本号（去空白；长度异常时视为未配置）。
func lvEffectiveVersion() string {
	v := strings.TrimSpace(builtinLauncherVersion)
	if v == "" || len(v) > lvMaxVersionLen {
		return ""
	}
	return v
}

// lvStatus 返回给管理工具展示的配置快照。
func (a *App) lvStatus() lvStat {
	state := a.lvPublicState()
	return lvStat{
		Enabled:         state.Enabled,
		DownloadURL:     state.DownloadURL,
		RequiredVersion: lvEffectiveVersion(),
	}
}

// lvApply 应用并持久化登录器版本校验配置（两项一起写，失败整体回滚）。
//
// 允许"勾选 + 直链为空"：这是需求确认过的分支（登录器弹「请联系管理员」），
// 所以这里不拒绝，只记一条显眼日志提醒运营。
func (a *App) lvApply(_ context.Context, enabled bool, downloadURL string) (lvStat, error) {
	if a == nil || a.cfg == nil {
		return lvStat{}, fmt.Errorf("配置尚未加载，无法应用登录器版本校验")
	}
	cleaned, err := lvNormalizeURL(downloadURL)
	if err != nil {
		return a.lvStatus(), err
	}

	a.psCfgMu.Lock()
	oldEnabled := a.cfg.LauncherVersionCheck
	oldURL := a.cfg.LauncherDownloadURL
	a.cfg.LauncherVersionCheck = enabled
	a.cfg.LauncherDownloadURL = cleaned
	saveErr := a.cfg.Save()
	if saveErr != nil {
		a.cfg.LauncherVersionCheck = oldEnabled
		a.cfg.LauncherDownloadURL = oldURL
	}
	a.psCfgMu.Unlock()
	if saveErr != nil {
		return a.lvStatus(), fmt.Errorf("配置持久化失败: %w", saveErr)
	}

	ver := lvEffectiveVersion()
	a.uiSafe(func() {
		switch {
		case !enabled:
			a.logf("登录器版本校验已关闭（当前内置版本号 %s，直链保留 %d 字）", ver, len(cleaned))
		case cleaned == "":
			// 这是需求里"弹窗提示请联系管理员"的那条分支，日志里必须显眼。
			a.logf("⚠ 登录器版本校验已开启，但**未填下载直链** —— 版本不符的登录器会提示「请联系管理员」并禁止进入登录界面！"+
				"（内置版本号 %s）", ver)
		default:
			a.logf("登录器版本校验已开启：要求版本 %s，直链 %s", ver, cleaned)
		}
	})
	return a.lvStatus(), nil
}

// lvNormalizeURL 归一化直链：去首尾空白。空串合法（= 未配置）。
// 非空时必须是 http/https 且带主机名，否则报错 —— 让运营在保存那一刻就发现填错，
// 而不是等玩家更新时才发现下不动。
func lvNormalizeURL(raw string) (string, error) {
	trimmed := strings.TrimSpace(raw)
	if trimmed == "" {
		return "", nil
	}
	if len(trimmed) > lvMaxURLLen {
		return "", fmt.Errorf("直链过长：最多 %d 个字符", lvMaxURLLen)
	}
	// 只接受 ASCII 可见字符：直链里出现中文/空格/换行几乎必然是复制粘贴带进来的垃圾，
	// 提前拒绝比让登录器 HttpClient 在玩家机器上报错好得多。
	for _, r := range trimmed {
		if r <= 0x20 || r >= 0x7F {
			return "", fmt.Errorf("直链只能包含英文/数字/符号，不能有中文、空格或换行")
		}
	}

	parsed, err := url.Parse(trimmed)
	if err != nil {
		return "", fmt.Errorf("直链格式不合法：%v", err)
	}
	if parsed.Scheme != "http" && parsed.Scheme != "https" {
		return "", fmt.Errorf("直链必须以 http:// 或 https:// 开头")
	}
	if strings.TrimSpace(parsed.Host) == "" {
		return "", fmt.Errorf("直链缺少主机名（例如 https://example.com/launcher.zip）")
	}
	return trimmed, nil
}
