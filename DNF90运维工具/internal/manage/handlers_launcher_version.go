//go:build paid

// 登录器版本校验端点（管理工具「服务器选项 → 登录器版本校验」区）：
//   - GET  /manage/v1/launcher-version：查询开关 + 下载直链 + 服务端内置版本号；
//   - POST /manage/v1/launcher-version：应用并持久化（落盘 config.json，运维工具重启后保持）。
//
// 语义：开关打开后，登录器（外网版 · 远程模式）在**启动自检阶段**与**点登录时**
// 各上报一次自己的版本号，与运维工具内置的 builtinLauncherVersion 完全一致才放行；
// 不一致 → 登录器按 download_url 下载新版本 → 删掉原登录器 → 自动拉起新登录器，
// **更新完成前禁止进入登录界面**。
//
// 与「更新维护」的差别：维护要在服务端硬拦登录接口（老登录器绕不过），
// 而版本校验**故意不改登录接口** —— 目的是让玩家升级，不是拒绝服务。
// 详见 internal/server/launcher_version.go 顶部说明。
package manage

import (
	"net/http"
	"strings"
)

// launcherVersionMaxURLLen 直链长度上限（与主程序 lvMaxURLLen 同口径，这里只做前置预校验）。
const launcherVersionMaxURLLen = 2048

// launcherVersionApplyRequest POST /manage/v1/launcher-version 入参。
//
// 只接受这两个字段：required_version 是服务端编译期常量，**不接受管理端下发**
// （即便请求体里带了也会被忽略，见 decodeJSON + 本结构体没有该字段）。
type launcherVersionApplyRequest struct {
	Enabled     bool   `json:"enabled"`
	DownloadURL string `json:"download_url"`
}

// handleLauncherVersionGet 返回当前登录器版本校验配置快照。
func (s *Server) handleLauncherVersionGet(w http.ResponseWriter, r *http.Request) {
	if s.bridge == nil || s.bridge.LauncherVersionStatus == nil {
		writeErr(w, http.StatusServiceUnavailable, "launcher-version-status-unavailable")
		return
	}
	writeOK(w, s.bridge.LauncherVersionStatus())
}

// handleLauncherVersionApply 应用并持久化登录器版本校验配置。
//
// 注意：这里**不拒绝**"开启校验但直链为空"——那仍然是合法配置
// （新登录器会弹「请联系管理员」，旧登录器反正看到的也是「请联系管理员」）。
// 主程序 lvApply 会记一条显眼日志，管理工具 UI 也会把这行状态标红。
func (s *Server) handleLauncherVersionApply(w http.ResponseWriter, r *http.Request) {
	if s.bridge == nil || s.bridge.LauncherVersionApply == nil {
		writeErr(w, http.StatusServiceUnavailable, "launcher-version-apply-unavailable")
		return
	}
	var req launcherVersionApplyRequest
	if err := decodeJSON(w, r, &req); err != nil {
		writeErr(w, http.StatusBadRequest, "bad-request: 请求体解析失败")
		return
	}

	downloadURL := strings.TrimSpace(req.DownloadURL)
	if len(downloadURL) > launcherVersionMaxURLLen {
		writeErr(w, http.StatusBadRequest, "下载直链过长：最多 2048 个字符")
		return
	}
	// 只接受 ASCII 可见字符：直链里出现中文/空格/换行几乎必然是复制粘贴带进来的垃圾，
	// 在这里拒掉比让登录器 HttpClient 在玩家机器上报错好得多。
	for _, r := range downloadURL {
		if r <= 0x20 || r >= 0x7F {
			writeErr(w, http.StatusBadRequest, "下载直链只能包含英文/数字/符号，不能有中文、空格或换行")
			return
		}
	}
	if downloadURL != "" && !strings.HasPrefix(downloadURL, "http://") && !strings.HasPrefix(downloadURL, "https://") {
		writeErr(w, http.StatusBadRequest, "下载直链必须以 http:// 或 https:// 开头")
		return
	}
	// 形如 "https://" 或缺主机名的串直接拒掉（主程序 lvNormalizeURL 会再兜一层）。
	if downloadURL != "" {
		host := strings.TrimPrefix(strings.TrimPrefix(downloadURL, "https://"), "http://")
		if idx := strings.IndexAny(host, "/?#"); idx >= 0 {
			host = host[:idx]
		}
		if strings.TrimSpace(host) == "" {
			writeErr(w, http.StatusBadRequest, "下载直链缺少主机名（例如 https://example.com/launcher.zip）")
			return
		}
	}

	status, err := s.bridge.LauncherVersionApply(r.Context(), req.Enabled, downloadURL)
	if err != nil {
		s.log.Error("launcher version apply failed", "enabled", req.Enabled, "error", err)
		writeErr(w, http.StatusInternalServerError, "保存登录器版本校验配置失败: "+err.Error())
		return
	}
	s.log.Info("launcher version config applied",
		"enabled", status.Enabled,
		"has_url", status.DownloadURL != "",
		"required_version", status.RequiredVersion)
	writeOK(w, status)
}
