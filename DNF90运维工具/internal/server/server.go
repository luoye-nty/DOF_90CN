// Package server 提供两个 HTTP 服务：
//   - 公网 HTTPS :18112 账号 API（/launcher/v1/*，登录器使用）
//   - 回环 HTTP 127.0.0.1:19091 内部验证 API（dnfbridge 认证回调）
package server

import (
	"context"
	"crypto/subtle"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"net"
	"net/http"
	"strconv"
	"strings"
	"sync/atomic"
	"time"

	"dnf90ops/internal/config"
	"dnf90ops/internal/store"
	"dnf90ops/internal/tlsutil"
)

const (
	maxBodyBytes = 1 << 20 // 1MB
)

// EditionHeader 登录器请求头：声明自身客户端版本（free=免费版登录器，paid=外网版登录器）。
// 服务端要求与 ServerEdition 严格一致，实现"免费版登录器 ↔ 免费版运维工具、
// 外网版登录器 ↔ 赞助版运维工具"的版本互斥连接控制。
const EditionHeader = "X-Client-Edition"

// editionMismatchMessage 版本不匹配时给客户端的业务错误码与中文提示。
const editionMismatchCode = "edition-mismatch"

// editionMismatchMessage 按服务端版本返回给客户端的中文原因。
func editionMismatchMessage() string {
	if ServerEdition == "paid" {
		return "该服务器为赞助版运维工具，仅限外网版登录器连接；免费版登录器禁止连接"
	}
	return "该服务器为免费版运维工具，仅限免费版登录器连接；外网版登录器禁止连接"
}

// editionMatchesRequest 请求携带的 X-Client-Edition 是否与本端版本一致。
//
// editionGuard 与 launcherVersionMiddleware 共用这一个判断 —— 两处各写一遍迟早会漂移，
// 而漂移的后果是"某条路径按 A 口径放行、另一条按 B 口径拦下"，排查起来极其费劲。
func editionMatchesRequest(r *http.Request) bool {
	edition := strings.TrimSpace(r.Header.Get(EditionHeader))
	return edition != "" && strings.EqualFold(edition, ServerEdition)
}

// editionHeaderConflicts 请求**明确带了一个**与本端不一致的 X-Client-Edition。
//
// 与 editionMatchesRequest 的差别就在"缺头"这一格：
//   - editionMatchesRequest：缺头 → false（做不到互斥，按不匹配处理，editionGuard 会拒）；
//   - editionHeaderConflicts：缺头 → false（**不算冲突**）。
//
// 为什么必须区分：版本闸要给"连错了登录器种类"的请求放行 health（好让它拿到
// server.edition 并提示「该服务器为赞助版运维工具…」），但**只应给"明确连错"的放行**。
// 完全不带 edition 头的是更老的登录器，不属于版本互斥场景，必须继续被版本闸拦下 ——
// 否则"老登录器连在线都不给探测"这条需求就被这条例外破掉了。
func editionHeaderConflicts(r *http.Request) bool {
	edition := strings.TrimSpace(r.Header.Get(EditionHeader))
	return edition != "" && !strings.EqualFold(edition, ServerEdition)
}

// editionGuard 强制登录器版本与运维工具版本互斥：
//   - 免费版运维工具(ServerEdition=free)：仅接受 X-Client-Edition: free
//   - 赞助版运维工具(ServerEdition=paid)：仅接受 X-Client-Edition: paid
//
// 请求头缺失或与 ServerEdition 不一致直接 403，不再执行后续 handler。
func editionGuard(next http.HandlerFunc) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		if !editionMatchesRequest(r) {
			writeError(w, http.StatusForbidden, editionMismatchCode, editionMismatchMessage())
			return
		}
		next(w, r)
	}
}

// GameUpFunc 返回 dnfbridge game 服务是否可用（GUI 注入端口探测）。
type GameUpFunc func() bool

// PointsinkPublic 泡点公开信息（登录器启动自检用）。
type PointsinkPublic struct {
	Enabled         bool `json:"enabled"`
	IntervalSeconds int  `json:"interval_seconds"`
	Points          int  `json:"points"`
}

// PointsinkInfoFunc 返回当前泡点公开信息（线程安全，由主程序注入）。
type PointsinkInfoFunc func() PointsinkPublic

// RegisterBonusPublic 注册赠送公开信息（登录器启动自检用）。
type RegisterBonusPublic struct {
	Enabled bool `json:"enabled"`
	Points  int  `json:"points"`
}

// RegisterBonusInfoFunc 返回当前注册赠送公开信息（线程安全，由主程序注入；可为 nil）。
type RegisterBonusInfoFunc func() RegisterBonusPublic

// registrationUnlimitedPerIP 「每 IP 不限制」的取值（规则见 RegistrationLimitFunc）。
// 注意 0 **不是**"不限制"，而是"不允许注册"。
const registrationUnlimitedPerIP = -1

// RegistrationPolicy 注册三道闸门的实时快照（由主程序注入，同一把锁下一次性读出，
// 避免出现"旧 closed + 新 maxPerIP"这类混合快照）。
type RegistrationPolicy struct {
	Closed   bool // true=关闭玩家自助注册（仅管理员可建号）
	MaxPerIP int  // -1=不按 IP 限制；0=不允许注册；1~1000=每个来源 IP 上限
	// CodeRequired 注册必须填注册码（注册开放时也能开启，独立于 registration_mode）。
	CodeRequired bool
}

// RegistrationLimitFunc 返回注册闸门的实时快照（线程安全，由主程序注入；nil = 不限制且不要码）。
// maxPerIP 的语义：
//   - -1：不按来源 IP 做限制；
//   - 0：不允许注册（每个来源 IP 上限 0）；
//   - 1~1000：每个来源 IP 最多这么多个。
type RegistrationLimitFunc func() RegistrationPolicy

// Service 认证服务主对象。
type Service struct {
	cfg               *config.Config
	store             *store.Store
	log               *slog.Logger
	gameUp            GameUpFunc
	pointsink         PointsinkInfoFunc
	registerBonus     RegisterBonusInfoFunc
	registrationLimit RegistrationLimitFunc
	yhCheck           YhCheckFunc
	announce          *AnnounceHub
	// 更新维护（见 maintenance.go）：状态函数由主程序注入，hub/版本用于长轮询推送。
	maintenanceFn  MaintenanceFunc
	maintenanceHub *MaintenanceHub
	maintenanceVer atomic.Int64
	// 登录器版本校验（见 launcher_version.go）：状态函数由主程序注入。
	// 不需要 hub/版本号 —— 登录器在自检阶段与点登录时各主动查一次，没有推送需求。
	launcherVersionFn LauncherVersionFunc
	// 双开（多开）开关（见 multi_open.go）：状态函数由主程序注入。
	// 另有 hub 做实时推送（关开关不影响已在运行的副账号，但"以后不许再开"必须立刻生效）。
	multiOpenFn  MultiOpenFunc
	multiOpenHub *MultiOpenHub
	// GM 事件实时推送（见 gm_events_wait.go）：按账号唤醒长轮询的登录器。
	gmEventHub *GMEventHub
	// CDK 兑换的数据访问入口（见 redeem.go 的 redeemDataStore）。
	// **仅供单测注入替身**：生产恒为 nil，handleRedeem 一律回落到 store。
	// 之所以留这一个缝：兑换链路的正确性要求"任何拒绝都发生在核销之前"，
	// 而真实 *store.Store 需要 MySQL，单测起不了库，就断言不了"兑换码没被消耗"。
	redeemData  redeemDataStore
	internalSrv   *http.Server
	publicSrv      *http.Server
	internalLn     net.Listener
	publicLn       net.Listener
}

// registrationPolicy 读取注册闸门快照；未注入时用"全放行"的缺省值。
//
// 缺省 MaxPerIP 必须是"不限制"(-1)：0 在新语义下是"不允许注册"，
// 拿零值当缺省会把没注入限制函数的场景（自检/测试）的注册全拦死。
func (s *Service) registrationPolicy() RegistrationPolicy {
	if s.registrationLimit == nil {
		return RegistrationPolicy{MaxPerIP: registrationUnlimitedPerIP}
	}
	return s.registrationLimit()
}

// registrationNeedsCode 注册是否需要注册码：显式开关打开，或本来就是 invite 模式。
// 登录器自检阶段也用它决定注册界面要不要显示注册码输入框（两条路必须同源，
// 否则会出现"界面没让填、服务端却要码"这种死局）。
func (s *Service) registrationNeedsCode(policy RegistrationPolicy) bool {
	return policy.CodeRequired || s.cfg.RegistrationMode == "invite"
}

// New 构造认证服务。gameUp 可空。
func New(cfg *config.Config, st *store.Store, log *slog.Logger, gameUp GameUpFunc, ps PointsinkInfoFunc, rb RegisterBonusInfoFunc, rl RegistrationLimitFunc) *Service {
	if log == nil {
		log = slog.Default()
	}
	return &Service{cfg: cfg, store: st, log: log, gameUp: gameUp, pointsink: ps, registerBonus: rb, registrationLimit: rl,
		maintenanceHub: NewMaintenanceHub(), announce: NewAnnounceHub(), gmEventHub: NewGMEventHub(),
		multiOpenHub: NewMultiOpenHub()}
}

// Announce 返回公告广播 hub（manage 发布公告时广播给等待中的长轮询）。
func (s *Service) Announce() *AnnounceHub { return s.announce }

// SetYhCheck 注入 yh 授权回调核销器。
//
// 【已去授权】本方法现在是**空实现**：不再记录任何回调，登录时的 yh 授权核销整体停用。
// 之所以保留这个入口而不删掉调用点（cmd/serve、main.go 各有一处），是为了让
// "要不要恢复授权"只是本文件内部的一处改动，不必再动启动装配流程。
//
// 注意：调用方（NewYhCheckAuto）仍然可以正常构造出回调对象，只是这里不再采用。
func (s *Service) SetYhCheck(_ YhCheckFunc) {
	// 去授权：有意留空，不再装配回调。
}

// yhCheckDeny 依据登录请求中的 yh 字段执行回调核销。
//
// 【已去授权】现在恒放行：无论登录器有没有携带 yh_ticket / yh_nonce / target_ip，
// 都不再向任何验证服务器回调，也不因缺少授权而拒绝登录。
// 这样旧版（仍带票的）登录器与新版本（不带票）都能正常登录。
func (s *Service) yhCheckDeny(_ context.Context, _, _, _ string) (code, message string) {
	return "", ""
}

// Start 依次：建表 -> 准备 TLS 证书 -> 启动内部验证（127.0.0.1:19091）-> 启动公网 HTTPS。
// invite 模式首次启动自动生成引导邀请码。
func (s *Service) Start(ctx context.Context) error {
	if err := s.store.EnsureSchema(ctx); err != nil {
		return fmt.Errorf("ensure mysql schema: %w", err)
	}
	if s.cfg.RegistrationMode == "invite" {
		code, created, err := s.store.EnsureBootstrapInvite(ctx)
		if err != nil {
			s.log.Warn("ensure bootstrap invite", "error", err)
		} else if created {
			s.log.Info("invite mode bootstrap invite code generated", "invite_code", code)
		}
	}

	// 内部验证监听（无 TLS，仅回环）。
	internalMux := http.NewServeMux()
	internalMux.HandleFunc("/internal/verify-game-ticket", s.handleVerifyGameTicket)
	internalMux.HandleFunc("/internal/health", s.handleInternalHealth)
	internalMux.HandleFunc("/internal/online-report", s.handleOnlineReport)
	s.internalSrv = &http.Server{
		Handler:           internalMux,
		ReadHeaderTimeout: 5 * time.Second,
		// 噪声过滤：见 http_noise.go。不挂这个的话，标准库 log 会被 slog 桥接进
		// 工具的输出框（`[INFO] http: TLS handshake error ...`），被裸 TCP 探测刷爆。
		ErrorLog: newHTTPErrorLog(func(line string) { s.log.Info(line) }),
	}
	ln, err := net.Listen("tcp", s.cfg.Internal.ListenAddr)
	if err != nil {
		return fmt.Errorf("internal listen %s: %w", s.cfg.Internal.ListenAddr, err)
	}
	s.internalLn = ln
	go func() {
		s.log.Info("internal verify api listening", "addr", s.cfg.Internal.ListenAddr)
		if err := s.internalSrv.Serve(ln); err != nil && !errors.Is(err, http.ErrServerClosed) {
			s.log.Error("internal verify api exited", "error", err)
		}
	}()

	// 公网 HTTPS 账号 API。
	relCert, relKey := s.cfg.Public.TLSCert, s.cfg.Public.TLSKey
	if err := tlsutil.EnsureCertificate(relCert, relKey, s.cfg.Public.AdvertiseAddr); err != nil {
		return fmt.Errorf("ensure tls certificate: %w", err)
	}
	publicMux := http.NewServeMux()
	// 版本互斥：除 health（探测/展示在线状态）外，全部登录器业务接口强制校验 X-Client-Edition，
	// 免费版登录器(free) ↔ 免费版运维工具、外网版登录器(paid) ↔ 赞助版运维工具。
	publicMux.HandleFunc("/launcher/v1/register", editionGuard(s.handleRegister))
	publicMux.HandleFunc("/launcher/v1/login", editionGuard(s.handleLogin))
	publicMux.HandleFunc("/launcher/v1/logout", editionGuard(s.handleLogout))
	publicMux.HandleFunc("/launcher/v1/rescue-code", editionGuard(s.handleSetRescueCode))
	publicMux.HandleFunc("/launcher/v1/password/reset", editionGuard(s.handlePasswordReset))
	publicMux.HandleFunc("/launcher/v1/health", s.handleHealth)
	publicMux.HandleFunc("/launcher/v1/heartbeat", editionGuard(s.handleHeartbeat))
	publicMux.HandleFunc("/launcher/v1/pointsink", editionGuard(s.handlePointsink))
	// 注册信息（免 ticket）：登录器自检阶段用它决定注册界面要不要显示注册码输入框。
	publicMux.HandleFunc("/launcher/v1/registration-info", editionGuard(s.handleRegistrationInfo))
	// 更新维护（免 ticket）：自检拿初始状态 + 长轮询实时推送"开始/结束维护"。
	// 必须免 ticket —— 登录器停在登录界面（还没登录）时也要能立刻收到维护通知。
	publicMux.HandleFunc("/launcher/v1/maintenance", editionGuard(s.handleMaintenanceInfo))
	publicMux.HandleFunc("/launcher/v1/maintenance/wait", editionGuard(s.handleMaintenanceWait))
	// 登录器版本校验（免 ticket）：自检阶段 + 玩家点登录时各查一次。
	// 免 ticket 的理由同维护：自检阶段玩家还没登录，登录器手里没有票。
	// 返回 200 + enabled/required_version/download_url，版本不符由登录器自己走更新流程。
	publicMux.HandleFunc("/launcher/v1/version-check", editionGuard(s.handleLauncherVersionCheck))
	// 双开（多开）开关（免 ticket）：自检阶段查一次，决定登录器「双开」勾选框与
	// 副账号登录按钮是否可用。免 ticket 的理由同维护/版本校验：自检时玩家还没登录。
	// 永远 200；后台**从未设置过**时不下发 enabled 字段，登录器收到 null = 保持允许双开
	// （见 internal/server/multi_open.go 的三态说明）。
	publicMux.HandleFunc("/launcher/v1/multi-open", editionGuard(s.handleMultiOpen))
	// 双开开关**实时推送**（长轮询，免 ticket —— 与短端点同源：它服务的是还没登录的
	// 登录界面，玩家手里没有票）。运营一改开关，主程序 moApply 就 Broadcast 唤醒所有
	// 挂起的登录器 → 已打开的登录器秒级置灰/恢复，不必让玩家重开登录器。
	// 老登录器只认上面的短端点，所以两条都保留。
	publicMux.HandleFunc("/launcher/v1/multi-open/wait", editionGuard(s.handleMultiOpenWait))
	publicMux.HandleFunc("/launcher/v1/gm-events", editionGuard(s.handleGMEvents))
	// GM 事件长轮询（免 ticket 之外的鉴权仍走 ticket）：泡点到账等提示"秒级"送达，
	// 不必再等下一个 10 秒轮询周期。老登录器只认上面的短轮询，所以两条都保留。
	publicMux.HandleFunc("/launcher/v1/gm-events/wait", editionGuard(s.handleGMEventsWait))
	publicMux.HandleFunc("/launcher/v1/announcements/wait", editionGuard(s.handleAnnounceWait))
	// 登录器「服务器公告」栏（登录页左侧公告板）：静态列表，免 ticket，直接 GET。
	publicMux.HandleFunc("/launcher/v1/notices", editionGuard(s.handleNoticeList))
	// 服务端指定 DLL 清单（文件名 + SHA256 + 校验失败策略）：免 ticket，
	// 登录器启动自检与玩家点登录时各拉一次，用于校验客户端根目录文件并决定是否注入。
	publicMux.HandleFunc("/launcher/v1/dlls", editionGuard(s.handleDLLList))
	publicMux.HandleFunc("/launcher/v1/characters", editionGuard(s.handleCharacters))
	publicMux.HandleFunc("/launcher/v1/redeem", editionGuard(s.handleRedeem))
	publicMux.HandleFunc("/launcher/v1/pvf/key", editionGuard(s.handlePvfKey))
	publicMux.HandleFunc("/launcher/v1/pvf/latest", editionGuard(s.handlePvfLatest))
	s.publicSrv = &http.Server{
		// 版本闸罩在整个登录器公开 API 外面：开关一开，老登录器（不带版本头）
		// **所有**接口都吃 403 —— 注册、登录、在线探测、公告、泡点、PVF、CDK、心跳全在内。
		// 唯一放行的是 /launcher/v1/version-check（"发现真相"的入口，见 launcherVersionOpenPaths）。
		// 开关关闭或未装配时本中间件完全不改变行为。
		Handler:           s.launcherVersionMiddleware(publicMux),
		ReadHeaderTimeout: 10 * time.Second,
		// 噪声过滤：公网口是刷屏重灾区 —— 每台在线登录器每 10 秒用裸 TCP 探一次
		// "服务器在不在"（不做 TLS 握手），每次都会产生一条 TLS handshake error。
		// 不挂这个过滤器，输出框会被这行彻底刷满，真正的业务日志全被顶掉。
		ErrorLog: newHTTPErrorLog(func(line string) { s.log.Info(line) }),
	}
	pln, err := net.Listen("tcp", s.cfg.Public.ListenAddr)
	if err != nil {
		return fmt.Errorf("public listen %s: %w", s.cfg.Public.ListenAddr, err)
	}
	s.publicLn = pln
	go func() {
		s.log.Info("public account api listening", "addr", s.cfg.Public.ListenAddr, "tls", true)
		if err := s.publicSrv.ServeTLS(pln, relCert, relKey); err != nil && !errors.Is(err, http.ErrServerClosed) {
			s.log.Error("public account api exited", "error", err)
		}
	}()
	return nil
}

// Stop 优雅关闭两个 HTTP 服务。
func (s *Service) Stop(ctx context.Context) error {
	var first error
	if s.internalSrv != nil {
		if err := s.internalSrv.Shutdown(ctx); err != nil {
			first = err
		}
	}
	if s.publicSrv != nil {
		if err := s.publicSrv.Shutdown(ctx); err != nil {
			if first == nil {
				first = err
			}
		}
	}
	return first
}

// 通用 JSON 辅助
func writeJSON(w http.ResponseWriter, status int, payload any) {
	w.Header().Set("Content-Type", "application/json; charset=utf-8")
	w.WriteHeader(status)
	_ = json.NewEncoder(w).Encode(payload)
}

func writeError(w http.ResponseWriter, status int, code, message string) {
	writeJSON(w, status, map[string]string{"code": code, "message": message})
}

func clientIP(r *http.Request) string {
	host, _, err := net.SplitHostPort(r.RemoteAddr)
	if err != nil {
		return r.RemoteAddr
	}
	return host
}

// ---------- 账号 API（/launcher/v1） ----------

type registerRequest struct {
	Username   string `json:"username"`
	Password   string `json:"password"`
	Email      string `json:"email,omitempty"`
	InviteCode string `json:"invite_code,omitempty"`
	// RescueCode：6 位数字救援代码，找回密码用（bcrypt 存储）。登录器注册窗必填，
	// 服务端开放注册模式下为可选（管理员 CLI/GUI 建号可不填）。
	RescueCode string `json:"rescue_code,omitempty"`
}

type registerResponse struct {
	AccountID string `json:"account_id"`
	Message   string `json:"message"`
}

// registrationBlockedByIP 判断"每 IP 注册上限"这一道闸是否拦截（纯函数，便于测试）。
//
// 规则：
//   - 没有来源 IP（管理员建号 / 命令行工具）：不拦 —— 这是那条路径唯一的豁免信号，
//     且这些账号写库时 register_ip 为空，天然不计入任何 IP 的额度；
//   - maxPerIP < 0：不限制；
//   - maxPerIP == 0：不允许注册（existing >= 0 恒成立）；
//   - maxPerIP > 0：该 IP 已注册数 >= 上限时拦截。
//
// 返回值第二项是给登录器直接弹窗的中文文案。
func registrationBlockedByIP(remoteIP string, maxPerIP, existing int) (bool, string) {
	if remoteIP == "" || maxPerIP < 0 {
		return false, ""
	}
	if existing < maxPerIP {
		return false, ""
	}
	if maxPerIP == 0 {
		return true, "当前服务器已限制注册，请联系管理员"
	}
	return true, fmt.Sprintf("当前 IP（%s）注册的账号数已达上限（%d 个），请联系管理员", remoteIP, maxPerIP)
}

func (s *Service) handleRegister(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		writeError(w, http.StatusMethodNotAllowed, "method-not-allowed", "POST required")
		return
	}
	var req registerRequest
	if err := decodeBody(r, &req); err != nil {
		writeError(w, http.StatusBadRequest, "bad-request", err.Error())
		return
	}
	username := store.NormalizeUsername(req.Username)
	// 注册限制（三道闸，同一次读取保证快照一致）：
	// ① 关闭自助注册 → 直接拦截（仅管理员可建号）；
	// ② 该来源 IP 已注册数达上限 → 按 IP 拦截；maxPerIP=0 表示"不允许注册"（上限就是 0），
	//    所以这里不能用 `maxPerIP > 0` 当开关，否则 0 又变成"不限制"。
	// ③ 需要注册码（registration_code_required，或本来就是 invite 模式）→ 必须填对一枚未使用的码。
	// 中文 message 由登录器原样弹出。
	policy := s.registrationPolicy()
	if policy.Closed {
		writeError(w, http.StatusForbidden, "registration-closed", "当前服务器已限制注册，请联系管理员")
		return
	}
	needsCode := s.registrationNeedsCode(policy)
	if needsCode && strings.TrimSpace(req.InviteCode) == "" {
		// 单独给"没填码"一个明确错误码：登录器据此提示"本次注册需要注册码"，
		// 而不是含糊的"邀请码无效"。invite 模式（老语义）保持原有错误码不变。
		if policy.CodeRequired {
			writeError(w, http.StatusForbidden, "registration-code-required",
				"本次注册需要注册码，请向管理员索取后填入注册码")
			return
		}
		writeError(w, http.StatusForbidden, "invalid-invite", "注册码无效或已被使用，请检查后重试")
		return
	}
	remoteIP := clientIP(r)
	count, countErr := s.store.CountAccountsByRegisterIP(r.Context(), remoteIP)
	if countErr != nil {
		// 计数查不到就放行（与旧行为一致）：宁可少拦一次，也不要因为一次查询失败把正常玩家挡在门外。
		s.log.Warn("count accounts by register ip failed", "ip", remoteIP, "error", countErr)
	}
	if blocked, message := registrationBlockedByIP(remoteIP, policy.MaxPerIP, count); blocked {
		writeError(w, http.StatusForbidden, "register-ip-limit", message)
		return
	}
	if len(username) < 3 || len(username) > 32 {
		writeError(w, http.StatusBadRequest, "invalid-username", "username must be 3-32 characters")
		return
	}
	for _, ch := range username {
		if !(ch >= 'a' && ch <= 'z' || ch >= '0' && ch <= '9' || ch == '_' || ch == '-' || ch == '@' || ch == '.') {
			writeError(w, http.StatusBadRequest, "invalid-username", "username contains unsupported characters")
			return
		}
	}
	if len(req.Password) < 8 {
		writeError(w, http.StatusBadRequest, "password-too-short", "password must be at least 8 characters")
		return
	}
	// 救援代码：客户端必填，格式非法直接拒绝；管理员 CLI/GUI 建号可不传。
	if req.RescueCode != "" && !store.ValidRescueCode(req.RescueCode) {
		writeError(w, http.StatusBadRequest, "invalid-rescue-code", "救援代码须为6位数字")
		return
	}
	// 需要注册码时，按 invite 模式的校验路径走（store 里 mode=="invite" 才会消费注册码）；
	// registration_mode 本身不改：open 仍然是 open，只是多了一道"要有码"的闸。
	effectiveMode := s.cfg.RegistrationMode
	if needsCode {
		effectiveMode = "invite"
	}
	accountID, err := s.store.Register(r.Context(), username, req.Password, req.Email, req.InviteCode, effectiveMode, remoteIP, req.RescueCode, policy.MaxPerIP)
	if err != nil {
		switch {
		case errors.Is(err, store.ErrUsernameTaken):
			writeError(w, http.StatusConflict, "username-taken", "username already registered")
		case errors.Is(err, store.ErrInvalidInvite):
			writeError(w, http.StatusForbidden, "invalid-invite", "注册码无效或已被使用，请检查后重试")
		case errors.Is(err, store.ErrInvalidRescueCode):
			writeError(w, http.StatusBadRequest, "invalid-rescue-code", "救援代码须为6位数字")
		case errors.Is(err, store.ErrRegisterIPLimit):
			// 事务内计数兜底：上面那次预检与这里之间存在竞态窗口，以事务内的判定为准。
			writeError(w, http.StatusForbidden, "register-ip-limit",
				fmt.Sprintf("当前 IP（%s）注册的账号数已达上限（%d 个），请联系管理员", remoteIP, policy.MaxPerIP))
		default:
			s.log.Error("register failed", "error", err)
			writeError(w, http.StatusInternalServerError, "internal-error", "register failed")
		}
		return
	}
	// 注册赠送点券：开启且数额在 1~1000000（与泡点对齐）时登记待发放；
	// 实际入账延迟到玩家首次上线角色后，由泡点循环走游戏服务端 GM 接口发放，
	// 避免注册时直写账号点券被游戏建号初始化覆盖。
	if s.registerBonus != nil {
		if rb := s.registerBonus(); rb.Enabled && rb.Points >= 1 && rb.Points <= 1000000 {
			if err := s.store.MarkRegisterBonusPending(r.Context(), accountID, rb.Points); err != nil {
				s.log.Error("register bonus pending write failed", "account_id", accountID, "error", err)
			} else {
				s.log.Info("register bonus pending registered", "account_id", accountID, "points", rb.Points)
			}
		} else if rb := s.registerBonus(); rb.Enabled {
			s.log.Warn("register bonus skipped: invalid points", "points", rb.Points)
		}
	}
	writeJSON(w, http.StatusOK, registerResponse{AccountID: accountID, Message: "registered"})
}

type loginRequest struct {
	Username  string `json:"username"`
	Password  string `json:"password"`
	MachineID string `json:"machine_id,omitempty"`
	// ComputerName：计算机名（= 2026-09-24 之前的旧机器码）。
	// 机器码改硬件指纹后，历史 machine 维度封禁是按计算机名写的，
	// 服务端靠这个字段做兜底匹配（见 store.HasActiveBanForMachine）。
	// 老登录器不发该字段 → 空串 → 只按 machine_id 匹配，行为与改动前一致。
	ComputerName string `json:"computer_name,omitempty"`
	// YhTicket / YhNonce：第③步 yh 授权回调字段。登录器登录上报时携带
	// authserver 下发的 ticket 与 nonce（见 YhAuthClient），launcher 回调核销。
	YhTicket string `json:"yh_ticket,omitempty"`
	YhNonce  string `json:"yh_nonce,omitempty"`
	// YhTargetIP：登录器本次要连接的目标服务器 IP（与 client.yh 内嵌 server_ip
	// 比对用），随回调 /api/yh/check 上报，防止跨服复用票据。
	YhTargetIP string `json:"target_ip,omitempty"`
}

type loginResponse struct {
	AccountID string    `json:"account_id"`
	Ticket    string    `json:"ticket"`
	ExpiresAt time.Time `json:"expires_at"`
	// HasRescueCode：账号是否已设置救援代码。false 表示老号无码，
	// 登录器据此弹窗引导补设（不阻断登录）。
	HasRescueCode bool `json:"has_rescue_code"`
}

func (s *Service) handleLogin(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		writeError(w, http.StatusMethodNotAllowed, "method-not-allowed", "POST required")
		return
	}
	var req loginRequest
	if err := decodeBody(r, &req); err != nil {
		writeError(w, http.StatusBadRequest, "bad-request", err.Error())
		return
	}
	// 版本过旧的登录器在更外层就被挡掉了（见 launcherVersionMiddleware），
	// 这里不再重复判断 —— 闸只有一道，避免两处逻辑漂移。
	// 更新维护：禁止玩家登录（白名单账号除外）。放在其余闸门前面 —— 这条提示最直接，
	// 也避免玩家在维护期间因为别的原因看到误导性的报错。
	if s.maintenanceBlocks(req.Username) {
		writeError(w, http.StatusForbidden, "maintenance", "服务器正在更新维护，请稍后再试")
		return
	}
	// 第③步：yh 授权回调核销（带 yh 字段才校验，旧客户端自动跳过）。
	if code, msg := s.yhCheckDeny(r.Context(), req.YhTicket, req.YhNonce, req.YhTargetIP); code != "" {
		writeError(w, http.StatusForbidden, code, msg)
		return
	}
	accountID, ticket, hasRescueCode, err := s.store.Login(r.Context(), req.Username, req.Password,
		store.MachineIdentity{MachineID: req.MachineID, Name: req.ComputerName}, clientIP(r), s.cfg.TicketTTL())
	if err != nil {
		switch {
		case errors.Is(err, store.ErrUserNotFound), errors.Is(err, store.ErrInvalidPassword):
			writeError(w, http.StatusUnauthorized, "invalid-credentials", "invalid username or password")
		case errors.Is(err, store.ErrBanned):
			writeError(w, http.StatusForbidden, "account-banned", "account is banned")
		case errors.Is(err, store.ErrMachineBanned):
			writeError(w, http.StatusForbidden, "machine-banned", "this machine is banned")
		case errors.Is(err, store.ErrIPBanned):
			writeError(w, http.StatusForbidden, "ip-banned", "this ip is banned")
		default:
			s.log.Error("login failed", "error", err)
			writeError(w, http.StatusInternalServerError, "internal-error", "login failed")
		}
		return
	}
	writeJSON(w, http.StatusOK, loginResponse{
		AccountID:     accountID,
		Ticket:        ticket,
		ExpiresAt:     time.Now().Add(s.cfg.TicketTTL()),
		HasRescueCode: hasRescueCode,
	})
}

type logoutRequest struct {
	Ticket string `json:"ticket"`
}

func (s *Service) handleLogout(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		writeError(w, http.StatusMethodNotAllowed, "method-not-allowed", "POST required")
		return
	}
	var req logoutRequest
	if err := decodeBody(r, &req); err != nil {
		writeError(w, http.StatusBadRequest, "bad-request", err.Error())
		return
	}
	if err := s.store.Logout(r.Context(), req.Ticket); err != nil {
		s.log.Error("logout failed", "error", err)
		writeError(w, http.StatusInternalServerError, "internal-error", "logout failed")
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{"ok": true})
}

// ---------- 救援代码（找回密码辅助） ----------

type setRescueCodeRequest struct {
	Ticket     string `json:"ticket"`
	RescueCode string `json:"rescue_code"`
}

// handleSetRescueCode 登录器补设救援代码：携带有效 ticket（登录已过密码校验）
// 写入 rescue_code_hash（bcrypt）。允许覆盖已设置代码——登录器仅对无码老号引导，
// 该接口同时保留未来"主动重置救援代码"能力。
func (s *Service) handleSetRescueCode(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		writeError(w, http.StatusMethodNotAllowed, "method-not-allowed", "POST required")
		return
	}
	var req setRescueCodeRequest
	if err := decodeBody(r, &req); err != nil {
		writeError(w, http.StatusBadRequest, "bad-request", err.Error())
		return
	}
	if strings.TrimSpace(req.Ticket) == "" {
		writeError(w, http.StatusBadRequest, "ticket-required", "ticket required")
		return
	}
	if !store.ValidRescueCode(req.RescueCode) {
		writeError(w, http.StatusBadRequest, "invalid-rescue-code", "救援代码须为6位数字")
		return
	}
	accountID, err := s.resolveAccountIDByTicket(r.Context(), strings.TrimSpace(req.Ticket))
	if err != nil {
		if errors.Is(err, store.ErrTicketNotFound) || errors.Is(err, store.ErrTicketExpired) {
			writeError(w, http.StatusUnauthorized, "ticket-not-found", "invalid or expired ticket")
			return
		}
		s.log.Error("set-rescue-code verify ticket failed", "error", err)
		writeError(w, http.StatusInternalServerError, "internal-error", "set rescue code failed")
		return
	}
	account, err := s.store.GetAccountByID(r.Context(), accountID)
	if err != nil {
		if errors.Is(err, store.ErrUserNotFound) {
			writeError(w, http.StatusUnauthorized, "ticket-not-found", "invalid or expired ticket")
			return
		}
		s.log.Error("set-rescue-code get account failed", "error", err)
		writeError(w, http.StatusInternalServerError, "internal-error", "set rescue code failed")
		return
	}
	if err := s.store.SetRescueCode(r.Context(), account.Username, req.RescueCode); err != nil {
		s.log.Error("set rescue code failed", "error", err)
		writeError(w, http.StatusInternalServerError, "internal-error", "set rescue code failed")
		return
	}
	s.log.Info("rescue code set", "account_id", accountID)
	writeJSON(w, http.StatusOK, map[string]any{"ok": true})
}

type passwordResetRequest struct {
	Username    string `json:"username"`
	RescueCode  string `json:"rescue_code"`
	NewPassword string `json:"new_password"`
}

// handlePasswordReset 救援代码找回密码（忘记密码）：无需登录态，
// 凭 账号+救援代码 直接改密；改密成功即作废该账号全部在线会话（踢下线）。
func (s *Service) handlePasswordReset(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		writeError(w, http.StatusMethodNotAllowed, "method-not-allowed", "POST required")
		return
	}
	var req passwordResetRequest
	if err := decodeBody(r, &req); err != nil {
		writeError(w, http.StatusBadRequest, "bad-request", err.Error())
		return
	}
	if strings.TrimSpace(req.Username) == "" {
		writeError(w, http.StatusBadRequest, "bad-request", "username required")
		return
	}
	if len(req.NewPassword) < 8 {
		writeError(w, http.StatusBadRequest, "password-too-short", "新密码长度至少8位")
		return
	}
	err := s.store.ResetPasswordWithRescueCode(r.Context(), req.Username, req.RescueCode, req.NewPassword)
	if err != nil {
		switch {
		case errors.Is(err, store.ErrUserNotFound):
			writeError(w, http.StatusUnauthorized, "invalid-credentials", "账号不存在或救援代码错误")
		case errors.Is(err, store.ErrInvalidRescueCode):
			writeError(w, http.StatusForbidden, "invalid-rescue-code", "救援代码错误或该账号未设置救援代码")
		default:
			s.log.Error("password reset failed", "error", err)
			writeError(w, http.StatusInternalServerError, "internal-error", "password reset failed")
		}
		return
	}
	s.log.Info("password reset by rescue code", "username", store.NormalizeUsername(req.Username))
	writeJSON(w, http.StatusOK, map[string]any{"ok": true})
}

type healthServerInfo struct {
	PublicAddr  string `json:"public_addr"`
	GameUp      bool   `json:"game_up"`
	OnlineCount int    `json:"online_count"`
	MaxOnline   int    `json:"max_online"`
	Edition     string `json:"edition"` // free=免费版运维工具 / paid=赞助版运维工具
}

type healthResponse struct {
	OK     bool             `json:"ok"`
	Server healthServerInfo `json:"server"`
}

func (s *Service) handleHealth(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodGet {
		writeError(w, http.StatusMethodNotAllowed, "method-not-allowed", "GET required")
		return
	}
	gameUp := false
	if s.gameUp != nil {
		gameUp = s.gameUp()
	}
	resp := healthResponse{OK: true}
	addr := s.cfg.Public.AdvertiseAddr
	if addr == "" {
		addr = s.cfg.Public.ListenAddr
	}
	resp.Server.PublicAddr = addr
	resp.Server.GameUp = gameUp
	resp.Server.OnlineCount = currentReportedOnline()
	resp.Server.MaxOnline = configuredMaxOnline()
	resp.Server.Edition = ServerEdition
	writeJSON(w, http.StatusOK, resp)
}

// handlePointsink 登录器启动自检时获取泡点公开信息（无需 ticket）。
func (s *Service) handlePointsink(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodGet {
		writeError(w, http.StatusMethodNotAllowed, "method-not-allowed", "GET required")
		return
	}
	var info PointsinkPublic
	if s.pointsink != nil {
		info = s.pointsink()
	}
	writeJSON(w, http.StatusOK, map[string]any{"ok": true, "data": info})
}

// ---------- 注册信息（登录器自检用，免 ticket） ----------

// registrationInfoResponse GET /launcher/v1/registration-info 响应。
//
//	code_required —— 本次注册是否需要填注册码（registration_code_required 打开，
//	                 或 registration_mode=invite）。登录器据此决定注册界面
//	                 要不要显示「注册码」输入框；两条路必须同源，否则会出现
//	                 "界面没让填、服务端却要码"的死局。
//	closed        —— 是否已关闭玩家自助注册（登录器可据此提前提示，不用等到提交才报错）。
type registrationInfoResponse struct {
	CodeRequired bool `json:"code_required"`
	Closed       bool `json:"closed"`
}

// handleRegistrationInfo 公开端点（无需 ticket）：登录器自检阶段拉取注册闸门状态。
func (s *Service) handleRegistrationInfo(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodGet {
		writeError(w, http.StatusMethodNotAllowed, "method-not-allowed", "GET required")
		return
	}
	policy := s.registrationPolicy()
	writeJSON(w, http.StatusOK, map[string]any{"ok": true, "data": registrationInfoResponse{
		CodeRequired: s.registrationNeedsCode(policy),
		Closed:       policy.Closed,
	}})
}

// ---------- 登录器心跳 / GM 踢人事件（拉模式下行） ----------

type heartbeatRequest struct {
	Ticket string `json:"ticket"`
}

// handleHeartbeat 登录器 10s 周期心跳：顺延 ticket 过期时间，
// 使 launcher_sessions 能真实反映"登录器在线的账号"，供运维工具在线列表使用。
func (s *Service) handleHeartbeat(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		writeError(w, http.StatusMethodNotAllowed, "method-not-allowed", "POST required")
		return
	}
	var req heartbeatRequest
	if err := decodeBody(r, &req); err != nil {
		writeError(w, http.StatusBadRequest, "bad-request", err.Error())
		return
	}
	renewed, err := s.store.HeartbeatTicket(r.Context(), req.Ticket, s.cfg.TicketTTL())
	if err != nil {
		s.log.Error("heartbeat failed", "error", err)
		writeError(w, http.StatusInternalServerError, "internal-error", "heartbeat failed")
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{"ok": true, "renewed": renewed})
}

type gmEventItem struct {
	ID        int64  `json:"id"`
	AccountID string `json:"account_id"`
	Username  string `json:"username"`
	EventType string `json:"event_type"`
	Reason    string `json:"reason"`
	Operator  string `json:"operator"`
	CreatedAt string `json:"created_at"`
}

type gmEventsResponse struct {
	OK     bool          `json:"ok"`
	Events []gmEventItem `json:"events"`
}

// handleGMEvents 登录器按 ticket 轮询本账号的 GM 踢人事件（GET ?ticket=&after_id=）。
// ticket 即鉴权凭证：仅能查到该 ticket 对应账号的事件；被封禁账号仍可查询（用于关闭本地游戏进程）。
func (s *Service) handleGMEvents(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodGet {
		writeError(w, http.StatusMethodNotAllowed, "method-not-allowed", "GET required")
		return
	}
	ticket := strings.TrimSpace(r.URL.Query().Get("ticket"))
	afterID, _ := strconv.ParseInt(r.URL.Query().Get("after_id"), 10, 64)
	since := parseSinceParam(r.URL.Query().Get("since"))
	if ticket == "" {
		writeError(w, http.StatusBadRequest, "bad-request", "ticket required")
		return
	}
	accountID, err := s.resolveAccountIDByTicket(r.Context(), ticket)
	if err != nil {
		if errors.Is(err, store.ErrTicketNotFound) {
			writeError(w, http.StatusUnauthorized, "ticket-not-found", "invalid or expired ticket")
			return
		}
		s.log.Error("gm-events resolve ticket failed", "error", err)
		writeError(w, http.StatusInternalServerError, "internal-error", "query failed")
		return
	}
	events, err := s.store.ListGMKickEventsAfter(r.Context(), accountID, afterID, since, 0)
	if err != nil {
		s.log.Error("gm-events list failed", "error", err)
		writeError(w, http.StatusInternalServerError, "internal-error", "query failed")
		return
	}
	writeJSON(w, http.StatusOK, gmEventsResponse{OK: true, Events: gmEventItems(events)})
}

// announcementItem 全服公告载荷（长轮询响应）。
type announcementItem struct {
	ID      int64  `json:"id"`
	Content string `json:"content"`
	Color   string `json:"color"`
}

// announceWaitResponse 长轮询响应；Announcement 为 null 表示超时无新公告。
type announceWaitResponse struct {
	OK           bool              `json:"ok"`
	Announcement *announcementItem `json:"announcement"`
}

// handleAnnounceWait 登录器长轮询：GET ?ticket=&after_id=。
// 有 id>after_id 的新公告立即返回；否则挂起至多 25s 等待广播，超时返回 null。
func (s *Service) handleAnnounceWait(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodGet {
		writeError(w, http.StatusMethodNotAllowed, "method-not-allowed", "GET required")
		return
	}
	ticket := strings.TrimSpace(r.URL.Query().Get("ticket"))
	afterID, _ := strconv.ParseInt(r.URL.Query().Get("after_id"), 10, 64)
	if ticket == "" {
		writeError(w, http.StatusBadRequest, "bad-request", "ticket required")
		return
	}
	if _, err := s.resolveAccountIDByTicket(r.Context(), ticket); err != nil {
		if errors.Is(err, store.ErrTicketNotFound) {
			writeError(w, http.StatusUnauthorized, "ticket-not-found", "invalid or expired ticket")
			return
		}
		s.log.Error("announce-wait resolve ticket failed", "error", err)
		writeError(w, http.StatusInternalServerError, "internal-error", "query failed")
		return
	}

	ch, unsubscribe := s.announce.Subscribe()
	defer unsubscribe()

	// 订阅后再查库，避免公告在订阅前到达被漏掉。
	items, err := s.store.ListAnnouncementsAfter(r.Context(), afterID, 1)
	if err != nil {
		s.log.Error("announce-wait list failed", "error", err)
		writeError(w, http.StatusInternalServerError, "internal-error", "query failed")
		return
	}
	if len(items) > 0 {
		it := items[0]
		writeJSON(w, http.StatusOK, announceWaitResponse{OK: true, Announcement: &announcementItem{
			ID: it.ID, Content: it.Content, Color: it.Color,
		}})
		return
	}

	select {
	case payload := <-ch:
		var it announcementItem
		if err := json.Unmarshal(payload, &it); err != nil {
			s.log.Error("announce-wait decode payload", "error", err)
			writeJSON(w, http.StatusOK, announceWaitResponse{OK: true, Announcement: nil})
			return
		}
		writeJSON(w, http.StatusOK, announceWaitResponse{OK: true, Announcement: &it})
	case <-time.After(25 * time.Second):
		writeJSON(w, http.StatusOK, announceWaitResponse{OK: true, Announcement: nil})
	case <-r.Context().Done():
		return
	}
}

// resolveAccountIDByTicket 轻量解析 ticket → account_id（仅校验会话存在且未过期，不验封禁）。
func (s *Service) resolveAccountIDByTicket(ctx context.Context, ticket string) (string, error) {
	return s.store.ResolveAccountIDByTicket(ctx, ticket)
}

// ---------- 内部验证 API（127.0.0.1:19091） ----------

type verifyRequest struct {
	Ticket      string `json:"ticket"`
	MachineID   string `json:"machine_id,omitempty"`
	RemoteIP    string `json:"remote_ip,omitempty"`
	ChannelID   int    `json:"channel_id,omitempty"`
	ServerNonce string `json:"server_nonce,omitempty"`
	OnlineCount int    `json:"online_count,omitempty"`
}

type verifyResponse struct {
	OK        bool   `json:"ok"`
	AccountID string `json:"account_id,omitempty"`
	Reason    string `json:"reason,omitempty"`
}

func (s *Service) handleVerifyGameTicket(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		writeJSON(w, http.StatusMethodNotAllowed, verifyResponse{OK: false, Reason: "method-not-allowed"})
		return
	}
	token := s.cfg.Internal.Token
	if token == "" || subtle.ConstantTimeCompare([]byte(token), []byte(strings.TrimSpace(r.Header.Get("X-Internal-Token")))) != 1 {
		writeJSON(w, http.StatusUnauthorized, verifyResponse{OK: false, Reason: "unauthorized"})
		return
	}
	var req verifyRequest
	if err := decodeBody(r, &req); err != nil {
		writeJSON(w, http.StatusBadRequest, verifyResponse{OK: false, Reason: "bad-request"})
		return
	}
	// 游戏服验票：机器码来自 LANA 认证帧，游戏侧没有计算机名，故只按 machine_id 匹配。
	accountID, err := s.store.VerifyTicket(r.Context(), req.Ticket,
		store.MachineIdentity{MachineID: req.MachineID}, req.RemoteIP)
	if err != nil {
		reason := "verify-failed"
		switch {
		case errors.Is(err, store.ErrTicketNotFound):
			reason = "ticket-not-found"
		case errors.Is(err, store.ErrTicketExpired):
			reason = "ticket-expired"
		case errors.Is(err, store.ErrBanned):
			reason = "account-banned"
		case errors.Is(err, store.ErrMachineBanned):
			reason = "machine-banned"
		case errors.Is(err, store.ErrIPBanned):
			reason = "ip-banned"
		default:
			s.log.Error("verify-game-ticket db error", "error", err)
		}
		writeJSON(w, http.StatusOK, verifyResponse{OK: false, Reason: reason})
		return
	}
	if reason := s.rejectByOnlineCapacity(req.OnlineCount); reason != "" {
		s.log.Info("online capacity reached", "reason", reason, "account_id", accountID, "online_count", req.OnlineCount)
		writeJSON(w, http.StatusOK, verifyResponse{OK: false, Reason: reason})
		return
	}
	// 更新维护：票还有效也不放行**新开的游戏连接**（白名单账号除外）。
	// 返回 200 + ok:false：游戏服据此拒绝这条连接，日志里会记 reason=maintenance
	// （见 dnfbridge launcher_auth.go —— 它把非 ok 一律当拒绝，不会 fail-open）。
	if s.maintenanceBlocks(s.usernameForAccount(r.Context(), accountID)) {
		s.log.Info("maintenance mode rejected a game connection", "account_id", accountID)
		writeJSON(w, http.StatusOK, verifyResponse{OK: false, Reason: "maintenance"})
		return
	}
	writeJSON(w, http.StatusOK, verifyResponse{OK: true, AccountID: accountID})
}

func (s *Service) handleInternalHealth(w http.ResponseWriter, r *http.Request) {
	writeJSON(w, http.StatusOK, map[string]any{"ok": true})
}

func decodeBody(r *http.Request, dst any) error {
	body, err := io.ReadAll(io.LimitReader(r.Body, maxBodyBytes))
	if err != nil {
		return fmt.Errorf("read body: %w", err)
	}
	if len(body) == 0 {
		return errors.New("empty body")
	}
	if err := json.Unmarshal(body, dst); err != nil {
		return fmt.Errorf("invalid json: %w", err)
	}
	return nil
}
