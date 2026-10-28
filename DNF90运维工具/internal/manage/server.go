//go:build paid

// 管理 HTTPS 服务：监听 :18113（可配），复用 tlsutil 自签证书（SAN 含公网 IP），
// 路由 /manage/v1/*；除 auth/login 外统一校验 X-Manage-Token；
// 登录接口全局限速（简单令牌桶，防爆破）。
package manage

import (
	"context"
	"crypto/tls"
	"encoding/json"
	"errors"
	"io"
	"log/slog"
	"net"
	"net/http"
	"strings"
	"sync"
	"time"

	"dnf90ops/internal/config"
	"dnf90ops/internal/tlsutil"
)

// Server 远程管理 HTTPS 服务。
type Server struct {
	cfg      *config.Config
	auth     *Auth
	bridge   *Bridge
	log      *slog.Logger
	listen   string
	certPath string
	keyPath  string

	mux  *http.ServeMux
	http *http.Server
	rl   *rateLimiter

	announceMu   sync.Mutex
	lastAnnounce time.Time

	// tasks 后台批量任务登记表（群发邮件 / 在线福利发放，见 tasks.go）。
	tasks *taskRegistry
}

// NewServer 构造管理服务（不启动）。cfg.Manage 必须非空。
func NewServer(cfg *config.Config, bridge *Bridge, log *slog.Logger) (*Server, error) {
	if cfg == nil || cfg.Manage == nil {
		return nil, errors.New("manage: config.manage missing")
	}
	m := cfg.Manage
	s := &Server{
		cfg:      cfg,
		bridge:   bridge,
		log:      log,
		listen:   strings.TrimSpace(m.ListenAddr),
		certPath: strings.TrimSpace(m.TLSCert),
		keyPath:  strings.TrimSpace(m.TLSKey),
		auth: NewAuth(
			m.PairingCodeSalt,
			m.PairingCodeHash,
			cfg.ManageTokenTTL(),
			m.MaxFailAttempts,
			cfg.ManageLockout(),
		),
		rl:    newRateLimiter(10, 2), // 桶容量 10，每秒补充 2（登录接口全局限速）
		tasks: newTaskRegistry(),
	}
	if s.listen == "" {
		s.listen = ":18113"
	}
	s.buildMux()
	return s, nil
}

// buildMux 注册 /manage/v1/* 路由（Go 1.22+ method pattern）。
func (s *Server) buildMux() {
	s.mux = http.NewServeMux()

	s.mux.HandleFunc("POST /manage/v1/auth/login", s.handleAuthLogin)
	s.mux.HandleFunc("POST /manage/v1/auth/logout", s.handleAuthLogout)

	s.mux.HandleFunc("GET /manage/v1/status", s.handleStatus)

	// PVF 加密上报 / 已发布列表。
	s.mux.HandleFunc("POST /manage/v1/pvf/info", s.handlePvfInfo)
	s.mux.HandleFunc("GET /manage/v1/pvf/info", s.handlePvfInfoList)

	// 全服公告（P0）。
	s.mux.HandleFunc("POST /manage/v1/announce", s.handleAnnounce)

	// P1 封禁。
	s.mux.HandleFunc("GET /manage/v1/bans", s.handleBanList)
	s.mux.HandleFunc("POST /manage/v1/bans", s.handleBanCreate)
	s.mux.HandleFunc("DELETE /manage/v1/bans", s.handleBanDelete)
	s.mux.HandleFunc("POST /manage/v1/bans/kick", s.handleBanKick)

	// P1 CDK。
	s.mux.HandleFunc("GET /manage/v1/redeem-codes", s.handleRedeemList)
	s.mux.HandleFunc("POST /manage/v1/redeem-codes", s.handleRedeemCreate)
	s.mux.HandleFunc("DELETE /manage/v1/redeem-codes/{code}", s.handleRedeemDisable)
	s.mux.HandleFunc("POST /manage/v1/redeem-codes/retry-mail", s.handleRedeemRetryMail)

	// P1 服务端启停。
	s.mux.HandleFunc("POST /manage/v1/server/start", s.handleServerStart)
	s.mux.HandleFunc("POST /manage/v1/server/stop", s.handleServerStop)

	// P1 数据库备份（自动备份配置读写 / 列表 / 覆盖还原）。
	s.mux.HandleFunc("GET /manage/v1/backup/config", s.handleBackupConfigGet)
	s.mux.HandleFunc("POST /manage/v1/backup/config", s.handleBackupConfigApply)
	s.mux.HandleFunc("GET /manage/v1/backup/list", s.handleBackupList)
	s.mux.HandleFunc("POST /manage/v1/backup/now", s.handleBackupNow)
	s.mux.HandleFunc("POST /manage/v1/backup/restore", s.handleBackupRestore)

	// P1 泡点点券。
	s.mux.HandleFunc("GET /manage/v1/pointsink", s.handlePointsinkGet)
	s.mux.HandleFunc("POST /manage/v1/pointsink", s.handlePointsinkApply)

	// P1 注册赠送点券。
	s.mux.HandleFunc("GET /manage/v1/register-bonus", s.handleRegisterBonusGet)
	s.mux.HandleFunc("POST /manage/v1/register-bonus", s.handleRegisterBonusApply)
	s.mux.HandleFunc("GET /manage/v1/registration-limit", s.handleRegistrationLimitGet)
	s.mux.HandleFunc("POST /manage/v1/registration-limit", s.handleRegistrationLimitApply)
	// 注册码（邀请码）：生成 / 列表 / 作废（管理工具「限制注册」页）。
	s.mux.HandleFunc("POST /manage/v1/registration-codes", s.handleRegistrationCodeCreate)
	s.mux.HandleFunc("GET /manage/v1/registration-codes", s.handleRegistrationCodeList)
	s.mux.HandleFunc("POST /manage/v1/registration-codes/revoke", s.handleRegistrationCodeRevoke)
	// 更新维护：维护期间禁止玩家登录与进游戏（白名单放行）；保存后即刻广播给所有登录器。
	s.mux.HandleFunc("GET /manage/v1/maintenance", s.handleMaintenanceGet)
	s.mux.HandleFunc("POST /manage/v1/maintenance", s.handleMaintenanceApply)
	// 登录器版本校验：登录器自检/点登录时上报版本号，与服务端内置版本完全一致才放行。
	// 不需要广播（登录器主动来查），所以只有一个读写对。
	s.mux.HandleFunc("GET /manage/v1/launcher-version", s.handleLauncherVersionGet)
	s.mux.HandleFunc("POST /manage/v1/launcher-version", s.handleLauncherVersionApply)
	// 双开（多开）开关：控制登录器「双开」勾选框能不能用。
	// 同样不需要广播（登录器自检阶段主动来查），只有一个读写对。
	// ⚠ 关闭**不会**踢掉已在运行的副账号 —— 这是"以后不许再开"，不是"把双开的人赶下线"。
	s.mux.HandleFunc("GET /manage/v1/multi-open", s.handleMultiOpenGet)
	s.mux.HandleFunc("POST /manage/v1/multi-open", s.handleMultiOpenApply)
	// 在线福利：三档期（每日/每周/每月）配置读写 + 立即试发。
	s.mux.HandleFunc("GET /manage/v1/welfare", s.handleWelfareGet)
	s.mux.HandleFunc("POST /manage/v1/welfare", s.handleWelfareApply)
	s.mux.HandleFunc("POST /manage/v1/welfare/try", s.handleWelfareTry)
	// 单档期保存：不会因为别的档期"已启用但没奖励"而挡住本次保存。
	s.mux.HandleFunc("POST /manage/v1/welfare/slot", s.handleWelfareApplySlot)
	// 定时公告（一次性 / 每天 / 每周，多条排队）：配置读写，调度在运维工具侧常驻跑。
	s.mux.HandleFunc("GET /manage/v1/announce-schedule", s.handleAnnounceScheduleGet)
	s.mux.HandleFunc("POST /manage/v1/announce-schedule", s.handleAnnounceScheduleApply)
	// 登录器「服务器公告」栏（登录页左侧公告板）：整表读写（标题 + 日期 + 启用）。
	s.mux.HandleFunc("GET /manage/v1/notices", s.handleNoticesGet)
	s.mux.HandleFunc("POST /manage/v1/notices", s.handleNoticesApply)
	// 服务端指定 DLL（登录器外网版校验 + 启动时注入）：整表读写（文件名 + SHA256 + 策略）。
	s.mux.HandleFunc("GET /manage/v1/dlls", s.handleDLLsGet)
	s.mux.HandleFunc("POST /manage/v1/dlls", s.handleDLLsApply)
	s.mux.HandleFunc("POST /manage/v1/account/register", s.handleAccountRegister)
	s.mux.HandleFunc("POST /manage/v1/account/password", s.handleAccountPassword)
	s.mux.HandleFunc("POST /manage/v1/account/force-logout", s.handleAccountForceLogout)
	// 批量踢下线（在线列表「踢下选中 / 全部踢下」）：服务端循环，管理工具只发一次请求。
	s.mux.HandleFunc("POST /manage/v1/account/kick", s.handleAccountKickUsers)
	s.mux.HandleFunc("POST /manage/v1/account/kick-all", s.handleAccountKickAll)

	// AC 账号管理（阶段1，设计 §5.2）。
	// 账号名批量翻译：管理工具界面上所有要显示的账号主键都走这里换成真实用户名
	// （主键是 dnf:<sha256(用户名)>，不可逆，只能靠运维工具侧的 MySQL 反查）。
	s.mux.HandleFunc("POST /manage/v1/account/names", s.handleAccountNames)
	s.mux.HandleFunc("POST /manage/v1/account/characters/query", s.handleAccountCharactersQuery)
	s.mux.HandleFunc("GET /manage/v1/account/online-characters", s.handleAccountOnlineCharacters)
	s.mux.HandleFunc("POST /manage/v1/account/mail/send", s.handleAccountMailSend)
	s.mux.HandleFunc("POST /manage/v1/account/wallet/query", s.handleAccountWalletQuery)
	s.mux.HandleFunc("POST /manage/v1/account/wallet/update", s.handleAccountWalletUpdate)
	s.mux.HandleFunc("POST /manage/v1/account/wallet/mail", s.handleAccountWalletMail)
	s.mux.HandleFunc("POST /manage/v1/account/items/grant", s.handleAccountItemsGrant)
	// 群发邮件（物品发放页的批量按钮）：异步任务 + 进度查询 + 取消。
	s.mux.HandleFunc("POST /manage/v1/account/items/grant-batch", s.handleAccountItemsGrantBatch)
	s.mux.HandleFunc("GET /manage/v1/tasks", s.handleTaskGet)
	s.mux.HandleFunc("GET /manage/v1/tasks/{id}", s.handleTaskGet)
	s.mux.HandleFunc("POST /manage/v1/tasks/{id}/cancel", s.handleTaskCancel)
	s.mux.HandleFunc("POST /manage/v1/account/character/sptp", s.handleAccountCharacterSPTPSet)
	s.mux.HandleFunc("POST /manage/v1/account/character/level", s.handleAccountCharacterLevelSet)
	s.mux.HandleFunc("GET /manage/v1/account/character/job", s.handleAccountCharacterJobGet)
	s.mux.HandleFunc("POST /manage/v1/account/character/job", s.handleAccountCharacterJobSet)
	// 跨大系转职（走服务端 career 路由：真改 job、重建技能、重算属性、要求角色离线）。
	s.mux.HandleFunc("POST /manage/v1/account/character/career", s.handleAccountCharacterCareerSet)
	s.mux.HandleFunc("POST /manage/v1/account/dungeons/unlock-all", s.handleAccountDungeonsUnlockAll)
	s.mux.HandleFunc("POST /manage/v1/account/equip-slots/unlock-all", s.handleAccountEquipSlotsUnlockAll)
	s.mux.HandleFunc("POST /manage/v1/account/character/black-diamond", s.handleAccountCharacterBlackDiamond)
	s.mux.HandleFunc("POST /manage/v1/account/character/black-diamond/query", s.handleAccountBlackDiamondQuery)
	s.mux.HandleFunc("POST /manage/v1/account/pvf/index/status", s.handleAccountPVFIndexStatus)

	// 背包/仓库与邮箱管理（走 GM Tool 通道 /dnf/gm/*，见 handlers_inventory.go）。
	s.mux.HandleFunc("POST /manage/v1/account/inventory/query", s.handleAccountInventoryQuery)
	s.mux.HandleFunc("POST /manage/v1/account/inventory/delete", s.handleAccountInventoryDelete)
	s.mux.HandleFunc("POST /manage/v1/account/inventory/clear", s.handleAccountInventoryClear)
	s.mux.HandleFunc("POST /manage/v1/account/mailbox/query", s.handleAccountMailboxQuery)
	s.mux.HandleFunc("POST /manage/v1/account/mailbox/delete", s.handleAccountMailboxDelete)
	s.mux.HandleFunc("POST /manage/v1/account/mailbox/clear", s.handleAccountMailboxClear)
	// 删除全服邮件：异步批量任务（逐角色转发 /local/dnf-gm/mail/clear，物理删除不可恢复，
	// 见 handlers_mailbox_batch.go；进度/取消复用 /manage/v1/tasks）。
	s.mux.HandleFunc("POST /manage/v1/account/mailbox/clear-batch", s.handleAccountMailboxClearBatch)
	// 批量资产邮件发放（在线角色 / 每账号首角色 / 全服首个角色，见 handlers_wallet_batch.go）。
	s.mux.HandleFunc("POST /manage/v1/account/wallet/mail-batch", s.handleAccountWalletMailBatch)

	// 频道管理（落盘编辑 channel_info.etc，见 handlers_channel.go）。
	s.mux.HandleFunc("GET /manage/v1/channels", s.handleChannelsGet)
	s.mux.HandleFunc("POST /manage/v1/channels/apply", s.handleChannelsApply)

	// 记录查询（点券消费/对话/副本，只读；转发 /dnf/gm/records/*，见 handlers_records.go）。
	s.mux.HandleFunc("POST /manage/v1/records/query", s.handleRecordsQuery)
	s.mux.HandleFunc("POST /manage/v1/records/meta", s.handleRecordsMeta)
}

// handler 顶层：登录接口免 token，其余统一校验 X-Manage-Token。
func (s *Server) handler() http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		defer func() {
			if v := recover(); v != nil {
				s.log.Error("manage handler panic", "panic", v, "path", r.URL.Path)
				writeErr(w, http.StatusInternalServerError, "internal-error")
			}
		}()
		path := r.URL.Path
		if path == "/manage/v1/auth/login" {
			s.mux.ServeHTTP(w, r)
			return
		}
		if !strings.HasPrefix(path, "/manage/v1/") {
			http.NotFound(w, r)
			return
		}
		if !s.auth.Validate(strings.TrimSpace(r.Header.Get("X-Manage-Token"))) {
			writeErr(w, http.StatusUnauthorized, "unauthorized")
			return
		}
		s.mux.ServeHTTP(w, r)
	})
}

// Start 确保证书并启动 HTTPS 监听；extraIPs 用于证书 SAN。
func (s *Server) Start(ctx context.Context, extraIPs []string) error {
	// 确保证书存在（自动生成自签，SAN 含 localhost/127.0.0.1/::1 与 extraIPs）。
	if s.bridge != nil && s.bridge.PublicIP != nil {
		if ip := strings.TrimSpace(s.bridge.PublicIP()); ip != "" {
			extraIPs = append(extraIPs, ip)
		}
	}
	if err := tlsutil.EnsureCertificate(s.certPath, s.keyPath, extraIPs...); err != nil {
		return err
	}

	cert, err := tls.LoadX509KeyPair(s.certPath, s.keyPath)
	if err != nil {
		return err
	}

	ln, err := net.Listen("tcp", s.listen)
	if err != nil {
		return err
	}

	s.http = &http.Server{
		Handler:           s.handler(),
		ReadHeaderTimeout: 10 * time.Second,
		IdleTimeout:       60 * time.Second,
		TLSConfig: &tls.Config{
			Certificates: []tls.Certificate{cert},
			MinVersion:   tls.VersionTLS12,
		},
	}

	go func() {
		<-ctx.Done()
		shutCtx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
		defer cancel()
		_ = s.http.Shutdown(shutCtx)
	}()

	go func() {
		if err := s.http.ServeTLS(ln, "", ""); err != nil && !errors.Is(err, http.ErrServerClosed) {
			s.log.Error("manage server stopped", "err", err)
		}
	}()
	s.log.Info("manage service started", "listen", s.listen, "cert", s.certPath)
	return nil
}

// Auth 暴露给 GUI（运行期更新配对码/作废 token 用）。
func (s *Server) Auth() *Auth { return s.auth }

// ListenAddr 返回实际监听地址。
func (s *Server) ListenAddr() string { return s.listen }

// Close 停止服务（幂等）。
func (s *Server) Close() {
	if s.http != nil {
		ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
		defer cancel()
		_ = s.http.Shutdown(ctx)
	}
}

// ---------- 登录接口 ----------

type loginRequest struct {
	Code string `json:"code"`
}

type loginResponse struct {
	Token     string `json:"token"`
	ExpiresIn int64  `json:"expires_in_seconds"`
}

func (s *Server) handleAuthLogin(w http.ResponseWriter, r *http.Request) {
	if !s.rl.allow() {
		writeErr(w, http.StatusTooManyRequests, "too-many-login-requests")
		return
	}
	var req loginRequest
	if err := decodeJSON(w, r, &req); err != nil {
		writeErr(w, http.StatusBadRequest, "bad-request")
		return
	}
	if strings.TrimSpace(req.Code) == "" {
		writeErr(w, http.StatusBadRequest, "bad-request")
		return
	}
	ip := clientIP(r)
	token, err := s.auth.Login(ip, req.Code)
	if err != nil {
		switch {
		case errors.Is(err, ErrNotEnabled):
			writeErr(w, http.StatusServiceUnavailable, ErrNotEnabled.Error())
		case errors.Is(err, ErrInvalidPair):
			writeErr(w, http.StatusUnauthorized, ErrInvalidPair.Error())
		case errors.Is(err, ErrPairLocked):
			writeErr(w, http.StatusLocked, ErrPairLocked.Error())
		default:
			writeErr(w, http.StatusInternalServerError, "internal-error")
		}
		return
	}
	writeOK(w, loginResponse{
		Token:     token,
		ExpiresIn: int64(s.cfg.ManageTokenTTL().Seconds()),
	})
}

// handleAuthLogout 作废当前 X-Manage-Token（带 token 校验由 handler() 统一完成）。
func (s *Server) handleAuthLogout(w http.ResponseWriter, r *http.Request) {
	s.auth.Revoke(strings.TrimSpace(r.Header.Get("X-Manage-Token")))
	writeOK(w, map[string]bool{"ok": true})
}

// ---------- 工具函数 ----------

func clientIP(r *http.Request) string {
	host, _, err := net.SplitHostPort(r.RemoteAddr)
	if err != nil {
		return r.RemoteAddr
	}
	return host
}

func decodeJSON(w http.ResponseWriter, r *http.Request, v any) error {
	body, err := io.ReadAll(io.LimitReader(r.Body, 1<<20))
	if err != nil {
		return err
	}
	return json.Unmarshal(body, v)
}

func writeOK(w http.ResponseWriter, data any) {
	writeJSON(w, http.StatusOK, envelope{OK: true, Data: data})
}

func writeErr(w http.ResponseWriter, status int, msg string) {
	writeJSON(w, status, envelope{OK: false, Error: msg})
}

func writeJSON(w http.ResponseWriter, status int, v any) {
	w.Header().Set("Content-Type", "application/json; charset=utf-8")
	w.WriteHeader(status)
	_ = json.NewEncoder(w).Encode(v)
}

// rateLimiter 简易令牌桶（全局限速）。
type rateLimiter struct {
	mu       sync.Mutex
	tokens   float64
	last     time.Time
	capacity float64
	perSec   float64
}

func newRateLimiter(capacity, perSec float64) *rateLimiter {
	return &rateLimiter{tokens: capacity, last: time.Now(), capacity: capacity, perSec: perSec}
}

func (r *rateLimiter) allow() bool {
	r.mu.Lock()
	defer r.mu.Unlock()
	now := time.Now()
	r.tokens += now.Sub(r.last).Seconds() * r.perSec
	r.last = now
	if r.tokens > r.capacity {
		r.tokens = r.capacity
	}
	if r.tokens >= 1 {
		r.tokens--
		return true
	}
	return false
}
