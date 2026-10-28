package server

// 登录器版本校验端点的语义测试（2026-09-26 新增）。
//
// 这几条锁的是"最坏情况"，每一条写反的后果都很直接：
//   - 未注入状态函数 → 必须按"未开启校验"返回（老装配路径/测试的行为与改造前一致）；
//   - 内置版本号为空 → 即使开关是开的也必须按"未开启"返回，
//     否则全服玩家会被一句"请联系管理员"永久锁在更新界面外；
//   - 开关没开 → 不能把直链发下去（避免登录器误用）；
//   - configured 必须等于"开关开 && 直链非空"，登录器 UI 直接拿它判断，
//     算错就会出现"版本不符 + 直链为空"却提示去更新（然后必然失败）的死局。

import (
	"encoding/json"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

// decodeLauncherVersionData 取出响应体里的 data 段。
func decodeLauncherVersionData(t *testing.T, body []byte) launcherVersionResponse {
	t.Helper()
	var envelope struct {
		OK   bool                    `json:"ok"`
		Data launcherVersionResponse `json:"data"`
	}
	if err := json.Unmarshal(body, &envelope); err != nil {
		t.Fatalf("解析响应失败: %v body=%s", err, body)
	}
	if !envelope.OK {
		t.Fatalf("ok 应为 true，body=%s", body)
	}
	return envelope.Data
}

// TestLauncherVersionStateNormalization 状态归一化的四条口径。
func TestLauncherVersionStateNormalization(t *testing.T) {
	cases := []struct {
		name        string
		inject      LauncherVersionFunc
		wantEnabled bool
		wantURL     string
		wantVersion string
	}{
		{
			name:   "未注入状态函数 → 未开启校验（行为与改造前一致）",
			inject: nil,
		},
		{
			name: "内置版本号为空 → 一律按未开启校验（不能把全服锁死）",
			inject: func() LauncherVersionState {
				return LauncherVersionState{Enabled: true, DownloadURL: "https://x/a.zip", RequiredVersion: "  "}
			},
		},
		{
			name: "开关没开 → 不下发直链",
			inject: func() LauncherVersionState {
				return LauncherVersionState{Enabled: false, DownloadURL: "https://x/a.zip", RequiredVersion: "1.5.5"}
			},
			wantVersion: "1.5.5",
		},
		{
			name: "开启校验 + 直链为空 → 合法状态（登录器提示请联系管理员）",
			inject: func() LauncherVersionState {
				return LauncherVersionState{Enabled: true, DownloadURL: "   ", RequiredVersion: " 1.5.5 "}
			},
			wantEnabled: true,
			wantVersion: "1.5.5",
		},
		{
			name: "开启校验 + 直链非空 → 可更新",
			inject: func() LauncherVersionState {
				return LauncherVersionState{Enabled: true, DownloadURL: " https://x/a.zip ", RequiredVersion: "1.5.5"}
			},
			wantEnabled: true,
			wantURL:     "https://x/a.zip",
			wantVersion: "1.5.5",
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			s := &Service{}
			if tc.inject != nil {
				s.SetLauncherVersion(tc.inject)
			}
			got := s.launcherVersionState()
			if got.Enabled != tc.wantEnabled {
				t.Fatalf("enabled = %v, want %v", got.Enabled, tc.wantEnabled)
			}
			if got.DownloadURL != tc.wantURL {
				t.Fatalf("download_url = %q, want %q", got.DownloadURL, tc.wantURL)
			}
			if got.RequiredVersion != tc.wantVersion {
				t.Fatalf("required_version = %q, want %q", got.RequiredVersion, tc.wantVersion)
			}
		})
	}
}

// TestHandleLauncherVersionCheckConfiguredFlag configured 必须等于"开关开 && 直链非空"：
// 登录器直接用它区分"去更新"与"弹请联系管理员"，算错就是死局。
func TestHandleLauncherVersionCheckConfiguredFlag(t *testing.T) {
	cases := []struct {
		name           string
		state          LauncherVersionState
		wantConfigured bool
	}{
		{"开+有直链 → 可更新", LauncherVersionState{Enabled: true, DownloadURL: "https://x/a.zip", RequiredVersion: "1.5.5"}, true},
		{"开+无直链 → 不可更新（请联系管理员）", LauncherVersionState{Enabled: true, RequiredVersion: "1.5.5"}, false},
		{"关+有直链 → 不校验", LauncherVersionState{Enabled: false, DownloadURL: "https://x/a.zip", RequiredVersion: "1.5.5"}, false},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			state := tc.state
			s := &Service{}
			s.SetLauncherVersion(func() LauncherVersionState { return state })

			rec := httptest.NewRecorder()
			s.handleLauncherVersionCheck(rec, httptest.NewRequest(http.MethodGet, "/launcher/v1/version-check", nil))
			if rec.Code != http.StatusOK {
				t.Fatalf("状态码 = %d, want 200（版本不符不是请求失败）", rec.Code)
			}
			data := decodeLauncherVersionData(t, rec.Body.Bytes())
			if data.Configured != tc.wantConfigured {
				t.Fatalf("configured = %v, want %v", data.Configured, tc.wantConfigured)
			}
			if data.Enabled != state.Enabled {
				t.Fatalf("enabled = %v, want %v", data.Enabled, state.Enabled)
			}
		})
	}
}

// TestHandleLauncherVersionCheckRejectsNonGet 只接受 GET（与其余免 ticket 端点同口径）。
func TestHandleLauncherVersionCheckRejectsNonGet(t *testing.T) {
	s := &Service{}
	s.SetLauncherVersion(func() LauncherVersionState {
		return LauncherVersionState{Enabled: true, RequiredVersion: "1.5.5"}
	})
	rec := httptest.NewRecorder()
	s.handleLauncherVersionCheck(rec, httptest.NewRequest(http.MethodPost, "/launcher/v1/version-check", nil))
	if rec.Code != http.StatusMethodNotAllowed {
		t.Fatalf("POST 状态码 = %d, want 405", rec.Code)
	}
}

// ============================================================================
//  登录接口硬闸：这一道才管得住老登录器
// ============================================================================

// TestLauncherVersionBlocksOldLaunchers 判定表的核心：
// **没带版本头 = 老登录器 = 拦**。这条写反的话，整个功能对老玩家完全失效
// —— 他们正是唯一需要被强制更新的那批人。
func TestLauncherVersionBlocksOldLaunchers(t *testing.T) {
	state := LauncherVersionState{Enabled: true, RequiredVersion: "1.5.5", DownloadURL: "https://x.example.com/a.zip"}
	cases := []struct {
		name        string
		header      string
		hasHeader   bool
		wantBlocked bool
	}{
		{"老登录器：完全不带版本头 → 拦（唯一能覆盖老包的手段）", "", false, true},
		{"版本头是空白 → 拦", "   ", true, true},
		{"版本一致 → 放行", "1.5.5", true, false},
		{"版本一致但带首尾空白 → 放行（服务端会 TrimSpace）", " 1.5.5 ", true, false},
		{"版本过旧 → 拦", "1.5.0", true, true},
		{"版本更新（私自换包）→ 拦", "1.5.6", true, true},
		{"乱填 → 拦", "abc", true, true},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			s := &Service{}
			s.SetLauncherVersion(func() LauncherVersionState { return state })
			r := httptest.NewRequest(http.MethodPost, "/launcher/v1/login", nil)
			if tc.hasHeader {
				r.Header.Set(LauncherVersionHeader, tc.header)
			}
			blocked, reason := s.launcherVersionBlocks(r)
			if blocked != tc.wantBlocked {
				t.Fatalf("blocked = %v, want %v（header=%q）", blocked, tc.wantBlocked, tc.header)
			}
			if blocked && strings.TrimSpace(reason) == "" {
				t.Fatal("拒绝时必须给出给玩家看的中文原因（老登录器直接把这句话显示在状态栏）")
			}
		})
	}
}

// TestLauncherVersionBlocksDisabledOrUnset 未开启/未装配时必须一律放行，行为与改动前一致。
func TestLauncherVersionBlocksDisabledOrUnset(t *testing.T) {
	// 未注入状态函数（老装配路径/测试）
	bare := &Service{}
	if blocked, _ := bare.launcherVersionBlocks(httptest.NewRequest(http.MethodPost, "/", nil)); blocked {
		t.Fatal("未注入版本校验时必须放行")
	}
	// 注入了但开关是关的
	off := &Service{}
	off.SetLauncherVersion(func() LauncherVersionState {
		return LauncherVersionState{Enabled: false, RequiredVersion: "1.5.5"}
	})
	if blocked, _ := off.launcherVersionBlocks(httptest.NewRequest(http.MethodPost, "/", nil)); blocked {
		t.Fatal("开关关闭时必须放行")
	}
	// 内置版本号为空（运营手滑改坏常量）→ launcherVersionState 归一化成未开启 → 放行
	empty := &Service{}
	empty.SetLauncherVersion(func() LauncherVersionState {
		return LauncherVersionState{Enabled: true, RequiredVersion: "  ", DownloadURL: "https://x/a.zip"}
	})
	if blocked, _ := empty.launcherVersionBlocks(httptest.NewRequest(http.MethodPost, "/", nil)); blocked {
		t.Fatal("内置版本号为空时必须放行（不能把全服锁死）")
	}
}

// TestLauncherVersionMsgKeywordsCoverLauncherMapper 关键词表是跨端契约：
// 服务端少一个词，就多一种"玩家看到胡说八道提示"的可能。
// 这里锁住几个最容易撞上的短词（都在登录器 MapServerCodeToChinese 里）。
func TestLauncherVersionMsgKeywordsCoverLauncherMapper(t *testing.T) {
	mustHave := []string{"banned", "maintenance", "维护", "rate", "taken", "already", "password", "exists", "invite"}
	for _, keyword := range mustHave {
		found := false
		for _, have := range launcherVersionMsgKeywords {
			if strings.EqualFold(have, keyword) {
				found = true
				break
			}
		}
		if !found {
			t.Fatalf("关键词表缺少 %q —— 登录器 MapServerCodeToChinese 里有它，"+
				"文案撞上就会让玩家看到错误提示", keyword)
		}
	}
}

// TestLauncherVersionBlockCodeIsUnique 错误码要能被登录器原样透传，
// 不能撞上登录器关键词表里的任何一个词（否则文案同样会被替换掉）。
func TestLauncherVersionBlockCodeIsUnique(t *testing.T) {
	lower := strings.ToLower(launcherVersionBlockCode)
	for _, keyword := range launcherVersionMsgKeywords {
		if strings.Contains(lower, strings.ToLower(keyword)) {
			t.Fatalf("错误码 %q 含关键词 %q，会被登录器的错误映射吞掉", launcherVersionBlockCode, keyword)
		}
	}
}

// ============================================================================
//  整站版本闸（中间件）：老登录器一个接口都拿不到
// ============================================================================

// newVersionGateTestHandler 造一个"下游永远返回 200 ok"的假 mux，用来观察闸门放不放行。
func newVersionGateTestHandler(t *testing.T) (http.Handler, *int) {
	t.Helper()
	hits := 0
	downstream := http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		hits++
		writeJSON(w, http.StatusOK, map[string]any{"ok": true})
	})
	s := &Service{}
	// 生产环境里 log 由 New() 注入；测试直接构造 Service 时必须自己补一个，
	// 否则闸门记日志那一步会空指针 panic。
	s.log = slog.Default()
	s.SetLauncherVersion(func() LauncherVersionState {
		return LauncherVersionState{Enabled: true, RequiredVersion: "1.5.5", DownloadURL: "https://x.example.com/a.zip"}
	})
	return s.launcherVersionMiddleware(downstream), &hits
}

// TestLauncherVersionBlockMessageIsFixed 需求（2026-09-28 确认）：
// **无论有没有配下载直链，老登录器看到的都是同一句固定文案。**
//
// 为什么专门锁这条：带直链在界面上根本用不了 —— 老登录器登录失败走的是右上角 Toast，
// 固定 320px 宽、3 秒自动消失，而运营实际配的云盘直链中文被百分号编码，一条就折成 6 行，
// 读不完也复制不了，还把"版本过旧"这句话本身挤没了。
// 所以文案里**不允许**再出现任何变量（尤其是直链）。
func TestLauncherVersionBlockMessageIsFixed(t *testing.T) {
	const want = "登录器版本过旧，请联系管理员下载新版登录器"
	if launcherVersionBlockText != want {
		t.Fatalf("固定文案 = %q, want %q（这句是需求方指定的，改动前先确认）", launcherVersionBlockText, want)
	}

	// 三种配置状态必须给出**逐字相同**的文案：填直链 / 没填直链 / 内置版本号带空白。
	states := []LauncherVersionState{
		{Enabled: true, RequiredVersion: "1.5.6", DownloadURL: "https://pan.example.com/dnf/launcher.zip"},
		{Enabled: true, RequiredVersion: "1.5.6"},
		{Enabled: true, RequiredVersion: " 1.5.6 ", DownloadURL: ""},
	}
	for _, state := range states {
		s := &Service{}
		s.SetLauncherVersion(func() LauncherVersionState { return state })
		r := httptest.NewRequest(http.MethodPost, "/launcher/v1/login", nil) // 老登录器：不带版本头
		blocked, reason := s.launcherVersionBlocks(r)
		if !blocked {
			t.Fatalf("老登录器必须被拦（state=%+v）", state)
		}
		if reason != want {
			t.Fatalf("文案随配置变了：state=%+v 得到 %q, want %q", state, reason, want)
		}
		// 文案里绝不能漏出直链（这就是这次改动的核心）。
		if strings.Contains(reason, "http") {
			t.Fatalf("固定文案里不允许出现直链，实际 %q", reason)
		}
	}
}

// TestLauncherVersionBlockTextSurvivesLauncherMapper 固定文案 + 错误码都**不能**含
// 登录器 MapServerCodeToChinese 的任何触发词。
//
// 老登录器拿到 message 会先扫一遍这些词，命中就把整句换掉。文案里一旦混进
// "维护"/"rate" 之类，玩家看到的就不是"版本过旧"了 —— 而那是老登录器，我们改不了它。
// 这条测试让"顺手改文案"这件事在 CI 上直接挂掉。
func TestLauncherVersionBlockTextSurvivesLauncherMapper(t *testing.T) {
	haystacks := map[string]string{
		"固定文案": launcherVersionBlockText,
		"错误码":  launcherVersionBlockCode,
	}
	for name, haystack := range haystacks {
		lower := strings.ToLower(haystack)
		for _, keyword := range launcherVersionMsgKeywords {
			if strings.Contains(lower, strings.ToLower(keyword)) {
				t.Fatalf("%s %q 含触发词 %q —— 老登录器会把它换掉，玩家看到的就是别的话了",
					name, haystack, keyword)
			}
		}
	}
	// 头几个字必须是"版本过旧"，玩家扫一眼就知道怎么回事（Toast 只有 3 秒）。
	if !strings.HasPrefix(launcherVersionBlockText, "登录器版本过旧") {
		t.Fatalf("文案开头必须是「登录器版本过旧」，实际 %q", launcherVersionBlockText)
	}
}

// TestLauncherVersionMiddlewareBlocksEveryLauncherEndpoint
// 需求："老登录器什么都不给用" —— 不给注册、不给探测在线、不给拉公告/泡点/PVF/CDK/心跳。
// 这条测试逐个点名这些接口，任何一条漏在闸外都会被这里抓住。
func TestLauncherVersionMiddlewareBlocksEveryLauncherEndpoint(t *testing.T) {
	paths := []string{
		"/launcher/v1/register",         // 不给注册
		"/launcher/v1/login",            // 不给登录
		"/launcher/v1/logout",           //
		"/launcher/v1/health",           // 不给探测服务器在线
		"/launcher/v1/heartbeat",        //
		"/launcher/v1/pointsink",        // 不给泡点
		"/launcher/v1/registration-info",//
		"/launcher/v1/maintenance",      // 不给维护状态
		"/launcher/v1/maintenance/wait", //
		"/launcher/v1/multi-open",       // 不给双开开关（老登录器不认识它，必须照样 403）
		"/launcher/v1/multi-open/wait",  // 不给双开开关的实时推送（同上，且长轮询端点也不能漏在闸外）
		"/launcher/v1/gm-events",        //
		"/launcher/v1/announcements/wait",//
		"/launcher/v1/notices",          // 不给公告栏
		"/launcher/v1/dlls",             //
		"/launcher/v1/characters",       // 不给角色列表
		"/launcher/v1/redeem",           // 不给 CDK 兑换
		"/launcher/v1/pvf/key",          // 不给换钥
		"/launcher/v1/pvf/latest",       //
		"/launcher/v1/rescue-code",      //
		"/launcher/v1/password/reset",   //
		// 将来新增的接口自动落在闸内；这里额外用一个不存在的路径钉住"默认全拦"的语义。
		"/launcher/v1/some-future-endpoint",
	}
	for _, path := range paths {
		t.Run(path, func(t *testing.T) {
			handler, hits := newVersionGateTestHandler(t)
			// 老登录器：一个版本头都不带
			rec := httptest.NewRecorder()
			handler.ServeHTTP(rec, httptest.NewRequest(http.MethodPost, path, nil))
			if rec.Code != http.StatusForbidden {
				t.Fatalf("%s 状态码 = %d, want 403（老登录器必须被挡下）", path, rec.Code)
			}
			if *hits != 0 {
				t.Fatalf("%s 竟然穿透到了业务 handler（下游被调用 %d 次）", path, *hits)
			}
			// 响应体必须是给玩家看的中文原因，而不是空的或英文的。
			var envelope struct {
				Code    string `json:"code"`
				Message string `json:"message"`
			}
			if err := json.Unmarshal(rec.Body.Bytes(), &envelope); err != nil {
				t.Fatalf("解析响应失败: %v body=%s", err, rec.Body.String())
			}
			if envelope.Code != launcherVersionBlockCode {
				t.Fatalf("错误码 = %q, want %q", envelope.Code, launcherVersionBlockCode)
			}
			if !strings.Contains(envelope.Message, "版本过旧") {
				t.Fatalf("文案必须是给玩家看的中文提示，实际 %q", envelope.Message)
			}
		})
	}
}

// TestLauncherVersionMiddlewareAllowsVersionCheckPath
// **放行白名单是最要命的一条**：version-check 是"问我现在该是什么版本"的唯一入口。
// 把它也拦掉，版本不对的登录器就问不到该更新到哪一版、去哪儿下载，直接死局。
func TestLauncherVersionMiddlewareAllowsVersionCheckPath(t *testing.T) {
	handler, hits := newVersionGateTestHandler(t)
	// 连版本头都不带也必须放行（新装的、还没对齐版本的登录器都要能问）
	rec := httptest.NewRecorder()
	handler.ServeHTTP(rec, httptest.NewRequest(http.MethodGet, "/launcher/v1/version-check", nil))
	if rec.Code != http.StatusOK {
		t.Fatalf("version-check 状态码 = %d, want 200（发现真相的入口必须永远开着）", rec.Code)
	}
	if *hits != 1 {
		t.Fatalf("version-check 必须穿透到业务 handler，实际 %d 次", *hits)
	}
}

// TestLauncherVersionMiddlewarePassesCorrectLauncher 版本正确的登录器必须畅通无阻，
// 否则等于把全服正常玩家也一起挡了。
func TestLauncherVersionMiddlewarePassesCorrectLauncher(t *testing.T) {
	for _, path := range []string{"/launcher/v1/login", "/launcher/v1/health", "/launcher/v1/register"} {
		handler, hits := newVersionGateTestHandler(t)
		req := httptest.NewRequest(http.MethodPost, path, nil)
		req.Header.Set(LauncherVersionHeader, "1.5.5")
		rec := httptest.NewRecorder()
		handler.ServeHTTP(rec, req)
		if rec.Code != http.StatusOK {
			t.Fatalf("%s 状态码 = %d, want 200", path, rec.Code)
		}
		if *hits != 1 {
			t.Fatalf("%s 应当穿透到业务 handler，实际 %d 次", path, *hits)
		}
	}
}

// TestLauncherVersionMiddlewareNoOpWhenDisabled 开关关闭/未装配时，中间件不能改变任何行为
// —— 这是"改动前后一致"的底线。
func TestLauncherVersionMiddlewareNoOpWhenDisabled(t *testing.T) {
	downstream := http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		writeJSON(w, http.StatusOK, map[string]any{"ok": true})
	})
	// ① 完全没装配
	bare := &Service{}
	rec := httptest.NewRecorder()
	bare.launcherVersionMiddleware(downstream).ServeHTTP(rec, httptest.NewRequest(http.MethodPost, "/launcher/v1/login", nil))
	if rec.Code != http.StatusOK {
		t.Fatalf("未装配时状态码 = %d, want 200", rec.Code)
	}
	// ② 装配了但开关是关的
	off := &Service{}
	off.SetLauncherVersion(func() LauncherVersionState {
		return LauncherVersionState{Enabled: false, RequiredVersion: "1.5.5"}
	})
	rec = httptest.NewRecorder()
	off.launcherVersionMiddleware(downstream).ServeHTTP(rec, httptest.NewRequest(http.MethodPost, "/launcher/v1/health", nil))
	if rec.Code != http.StatusOK {
		t.Fatalf("开关关闭时状态码 = %d, want 200", rec.Code)
	}
}

// TestLauncherVersionMiddlewareLeavesOtherPathsAlone 只罩 /launcher/v1/ 前缀，
// 将来在这个 mux 上挂别的东西不会被误伤。
func TestLauncherVersionMiddlewareLeavesOtherPathsAlone(t *testing.T) {
	handler, hits := newVersionGateTestHandler(t)
	rec := httptest.NewRecorder()
	handler.ServeHTTP(rec, httptest.NewRequest(http.MethodGet, "/manage/v1/status", nil))
	if rec.Code != http.StatusOK || *hits != 1 {
		t.Fatalf("非登录器路径不应被版本闸影响：code=%d hits=%d", rec.Code, *hits)
	}
}

// TestLauncherVersionMiddlewareKeepsHealthOpenForEditionMismatch
// **这是一条回归测试**（2026-09-28 真踩过）：
//
// /launcher/v1/health 是登录器判断"我是不是连错了服务器版本种类"的唯一途径 ——
// 服务端在它的响应里回 server.edition，登录器据此禁用登录按钮并提示
// 「该服务器为赞助版运维工具，仅限外网版登录器连接」。
//
// 版本闸罩在整个 mux 外面、先于 editionGuard 执行。如果它把 health 也 403 掉，
// 登录器那边 `if (health.Ok)` 整段（含版本互斥检查）会被跳过 ——
// 玩家看到的是「登录器版本过旧」，于是跑去折腾登录器版本号，
// 而实际上他该换的是登录器**种类**。病因被指错方向，
// 而这条路径恰恰只在"版本校验开着"时才走，平时测不出来。
func TestLauncherVersionMiddlewareKeepsHealthOpenForEditionMismatch(t *testing.T) {
	state := LauncherVersionState{Enabled: true, RequiredVersion: "1.5.6", DownloadURL: "https://x.example.com/a.zip"}
	newHandler := func() (http.Handler, *int) {
		hits := 0
		downstream := http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			hits++
			writeJSON(w, http.StatusOK, map[string]any{"ok": true})
		})
		s := &Service{log: discardLogger()}
		s.SetLauncherVersion(func() LauncherVersionState { return state })
		return s.launcherVersionMiddleware(downstream), &hits
	}

	// ① 明确连错了版本种类（带了 edition 头但不对）→ 必须放行，让登录器拿到 server.edition
	wrongEditions := []string{"free", "paid", "whatever"}
	for _, edition := range wrongEditions {
		if strings.EqualFold(edition, ServerEdition) {
			continue // 与本端同名的那个不算"连错"
		}
		handler, hits := newHandler()
		req := httptest.NewRequest(http.MethodGet, "/launcher/v1/health", nil)
		req.Header.Set(EditionHeader, edition)
		rec := httptest.NewRecorder()
		handler.ServeHTTP(rec, req)
		if rec.Code != http.StatusOK || *hits != 1 {
			t.Fatalf("X-Client-Edition=%q 时 health 被版本闸挡下了（code=%d hits=%d）—— "+
				"登录器会因为 health.Ok=false 跳过版本互斥检查，玩家看到的是「版本过旧」而不是"+
				"「你连错服务器了」", edition, rec.Code, *hits)
		}
	}

	// ② **完全不带 edition 头**（更老的登录器）→ 必须照拦。
	//    这条例外是为"连错版本种类"开的，不能被"没带头的更老登录器"蹭到 ——
	//    否则"老登录器连在线都不给探测"这条需求就被破了。
	handler, hits := newHandler()
	rec := httptest.NewRecorder()
	handler.ServeHTTP(rec, httptest.NewRequest(http.MethodGet, "/launcher/v1/health", nil))
	if rec.Code != http.StatusForbidden || *hits != 0 {
		t.Fatalf("不带 edition 头的老登录器探测 health 必须被拦，实际 code=%d hits=%d", rec.Code, *hits)
	}

	// ③ 版本头匹配（连对了服务器）但登录器版本旧 → 照拦
	handler, hits = newHandler()
	req := httptest.NewRequest(http.MethodGet, "/launcher/v1/health", nil)
	req.Header.Set(EditionHeader, ServerEdition) // 与本端一致
	rec = httptest.NewRecorder()
	handler.ServeHTTP(rec, req)
	if rec.Code != http.StatusForbidden {
		t.Fatalf("连对服务器但版本不符时 health 必须照拦，实际 code=%d", rec.Code)
	}
	if *hits != 0 {
		t.Fatal("被拦的 health 不应穿透到业务 handler")
	}
}

// ============================================================================
//  双开（多开）开关端点的**三态契约**
// ============================================================================
//
// 放在本文件里的理由：multi-open 与 version-check 是同一个公开 mux（/launcher/v1/*）上
// 同一类端点 —— "永远 200，结果由字段表达"，登录器不把它当错误分支。两边的口径测试
// 摆在一起，将来谁改了其中一边的形态（比如"顺手让 data 永远带 enabled"）会立刻暴露。
//
// 这条锁的是整个双开功能**最容易出事**的一处：**只有明确收到 enabled:false 才关闭**。
//
//	把"没设置过"错发成 enabled:false → 全服玩家的双开会在升级运维工具那一刻同时消失；
//	把"明确关闭"错发成空 data     → 运营点了开关却毫无效果（还会以为功能坏了）。
//
// 登录器侧对应的类型是三态 bool?（见 登录器 Services/RemoteAuthService.cs）：
// 字段缺失或为 null 都是 null = 未知 = **保持允许双开**。
func TestHandleMultiOpenTriStateContract(t *testing.T) {
	boolPtr := func(v bool) *bool { return &v }
	cases := []struct {
		name string
		fn   MultiOpenFunc
		// wantPresent=false 表示 data 里**不能**出现 enabled 键（登录器收到 null）。
		wantPresent bool
		want        bool
	}{
		{"未注入（老装配路径/测试）→ 不下发字段 = 允许", nil, false, false},
		{"后台从未设置过（老 config.json）→ 不下发字段 = 允许", func() MultiOpenState { return MultiOpenState{} }, false, false},
		{"明确允许 → enabled:true", func() MultiOpenState { return MultiOpenState{Enabled: boolPtr(true)} }, true, true},
		{"明确关闭 → enabled:false", func() MultiOpenState { return MultiOpenState{Enabled: boolPtr(false)} }, true, false},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			s := &Service{log: discardLogger()}
			if tc.fn != nil {
				s.SetMultiOpen(tc.fn)
			}
			rec := httptest.NewRecorder()
			s.handleMultiOpen(rec, httptest.NewRequest(http.MethodGet, "/launcher/v1/multi-open", nil))
			// 永远 200：双开被关掉是"一个正常业务结果"，不是请求失败（口径同 version-check）。
			if rec.Code != http.StatusOK {
				t.Fatalf("状态码 = %d, want 200", rec.Code)
			}
			var envelope struct {
				OK   bool           `json:"ok"`
				Data map[string]any `json:"data"`
			}
			if err := json.Unmarshal(rec.Body.Bytes(), &envelope); err != nil {
				t.Fatalf("解析响应失败: %v body=%s", err, rec.Body.String())
			}
			if !envelope.OK {
				t.Fatalf("ok 应为 true，body=%s", rec.Body.String())
			}
			got, present := envelope.Data["enabled"]
			if present != tc.wantPresent {
				t.Fatalf("data.enabled 是否存在 = %v, want %v（body=%s）—— "+
					"「没设置过」必须表达成字段缺失，不能表达成 false", present, tc.wantPresent, rec.Body.String())
			}
			if !tc.wantPresent {
				return
			}
			val, ok := got.(bool)
			if !ok {
				t.Fatalf("data.enabled 类型 = %T, want bool", got)
			}
			if val != tc.want {
				t.Fatalf("data.enabled = %v, want %v", val, tc.want)
			}
		})
	}

	// 只接受 GET：其余方法 405（与其它公开端点一致，且**不**影响 200 口径）。
	s := &Service{log: discardLogger()}
	rec := httptest.NewRecorder()
	s.handleMultiOpen(rec, httptest.NewRequest(http.MethodPost, "/launcher/v1/multi-open", nil))
	if rec.Code != http.StatusMethodNotAllowed {
		t.Fatalf("POST 状态码 = %d, want 405", rec.Code)
	}
}

