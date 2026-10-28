package main

// DNF90 账号管理功能 —— 阶段1：运维端 GM 直连封装（设计文档 §4.3）。
// 无 paid tag：免费版/付费版均编译；严禁引用 paid-only 符号（remote_tab / license_paid 等）。
// 复用 cdk_tab.go cdkGmAdminConfig/cdkGmSendMail 探测模式：读取 a.rootEdit / cfg.ServerRoot，
// 经 serverdir.Inspect 读 instance.json 的 adminListen/adminToken；
// 链路协议与 go-server 包 GM 调用一致（X-Admin-Token + X-Admin-Actor，{ok,message,error,data}）。
// 与 GM native_admin.go 的 nativeCharacterView/nativeMailRequest/... 类型一一对应，便于对照移植。
// 维护纪律：以 DNF90GM工具_独立版（上游）为唯一事实源，同步升级时逐函数对比 native_admin.go。

import (
	"bytes"
	"context"
	_ "embed"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"math"
	"net"
	"net/http"
	"net/url"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"sync"
	"time"

	"dnf90ops/internal/gmhttp"
	"dnf90ops/internal/pvfindex"
	"dnf90ops/internal/serverdir"
	"dnf90ops/internal/store"
)

//go:embed dungeons_max_difficulty.json
var acctDungeonsMaxDifficultyRaw []byte

var (
	acctDungeonsOnce sync.Once
	acctDungeonsMap  map[string]int
)

// acctDungeonsMaxDifficulty 返回 GM dungeons_max_difficulty.json 语义的 map[dungeon_id]难度上限。
func acctDungeonsMaxDifficulty() map[string]int {
	acctDungeonsOnce.Do(func() {
		parsed := make(map[string]int)
		if err := json.Unmarshal(acctDungeonsMaxDifficultyRaw, &parsed); err != nil {
			parsed = map[string]int{}
		}
		acctDungeonsMap = parsed
	})
	return acctDungeonsMap
}

const (
	// acctGMTimeout GM 原生调用超时（对齐 GM nativeHTTPClient 12s）。
	acctGMTimeout = 12 * time.Second
	// acctMaxWalletBalance 资产邮件单次数量上限（对齐 GM maxWalletBalance）。
	acctMaxWalletBalance = int64(math.MaxInt32)
	// acctMailGoldMax 邮件金币上限（uint32）。
	acctMailGoldMax = int64(^uint32(0))
	// acctBlackDiamondMaxDays 黑钻单次开启天数上限：与服务端 GMActivateBlackDiamond
	// 的 1..3650 天校验一致（服务端 Days=0 表示默认 30 天，工具端始终显式传天数）。
	acctBlackDiamondMaxDays = 3650
)

// ---- GM 原生端点视图类型（JSON 字段对齐 go-server /local/dnf-gm/*，来源 native_admin.go）----

type acctCharacterView struct {
	CharacterID string `json:"character_id"`
	AccountID   string `json:"account_id"`
	Name        string `json:"name"`
	Level       int    `json:"level"`
	Gold        int64  `json:"gold"`
	Cera        int64  `json:"cera"`
	Online      bool   `json:"online"`
}

type acctMailAttachment struct {
	ItemID         uint32 `json:"item_id"`
	Count          uint32 `json:"count"`
	Bind           bool   `json:"bind"`
	ReinforceLevel int    `json:"reinforce_level,omitempty"`
	AmplifyLevel   int    `json:"amplify_level,omitempty"`
	AmplifyType    int    `json:"amplify_type,omitempty"`
}

type acctMailRequest struct {
	CharacterID   string               `json:"character_id,omitempty"`
	CharacterName string               `json:"character_name,omitempty"`
	Title         string               `json:"title,omitempty"`
	Body          string               `json:"body,omitempty"`
	Gold          int64                `json:"gold,omitempty"`
	Attachments   []acctMailAttachment `json:"attachments,omitempty"`
}

type acctMailResult struct {
	MailID               string `json:"mail_id"`
	RecipientCharacterID string `json:"recipient_character_id"`
	RecipientName        string `json:"recipient_name"`
	Online               bool   `json:"online"`
	Refreshed            bool   `json:"refreshed"`
	RefreshError         string `json:"refresh_error,omitempty"`
}

type acctWalletResult struct {
	Character acctCharacterView `json:"character"`
	Currency  string            `json:"currency"`
	Mode      string            `json:"mode"`
	Before    int64             `json:"before"`
	After     int64             `json:"after"`
	Cera      int64             `json:"cera"`
	// TokenCera / HappyTokenCera 是账号级代币券 / 欢乐代币券（服务端 GMWalletResult 同名字段）。
	// 背包编辑页的「钱包」区要用它们，缺了就只能显示 0。
	TokenCera      int64  `json:"token_cera"`
	HappyTokenCera int64  `json:"happy_token_cera"`
	Gold           int64  `json:"gold"`
	Online         bool   `json:"online"`
	Refreshed      bool   `json:"refreshed"`
	RefreshError   string `json:"refresh_error"`
}

type acctSkillInfo struct {
	CharacterID string `json:"character_id"`
	Name        string `json:"name"`
	Level       int    `json:"level"`
	TotalSP     int    `json:"total_sp"`
	RemainingSP int    `json:"remaining_sp"`
	TotalTP     int    `json:"total_tp"`
	RemainingTP int    `json:"remaining_tp"`
}

type acctJobInfo struct {
	CharacterID string `json:"character_id"`
	Job         int    `json:"job"`
	FirstGrow   int    `json:"first_grow"`
	SecondGrow  int    `json:"second_grow"`
}

// acctBlackDiamondResult 与 go-server dnfbridge.GMBlackDiamondResult 对齐。
// ExpiresAt/RemainingSeconds 是**激活后**的账号级黑钻到期时间（黑钻同一账号所有角色共享）。
type acctBlackDiamondResult struct {
	CharacterID      string `json:"character_id"`
	AccountID        string `json:"account_id"`
	Days             int    `json:"days"`
	ExpiresAt        int64  `json:"expires_at"`
	RemainingSeconds int64  `json:"remaining_seconds"`
	Fatigue          int64  `json:"fatigue"`
	FatigueLimit     int64  `json:"fatigue_limit"`
	Online           bool   `json:"online"`
	Refreshed        bool   `json:"refreshed"`
	RefreshError     string `json:"refresh_error,omitempty"`
	Deferred         bool   `json:"deferred,omitempty"`
}

// acctBlackDiamondStatusResult 与 go-server dnfbridge.GMBlackDiamondStatus 对齐（只读查询）。
type acctBlackDiamondStatusResult struct {
	CharacterID              string `json:"character_id"`
	CharacterName            string `json:"character_name"`
	AccountID                string `json:"account_id"`
	Active                   bool   `json:"active"`
	ExpiresAt                int64  `json:"expires_at"`
	RemainingSeconds         int64  `json:"remaining_seconds"`
	RemainingDays            int64  `json:"remaining_days"`
	Fatigue                  int64  `json:"fatigue"`
	FatigueLimit             int64  `json:"fatigue_limit"`
	BlackDiamondFatigueLimit int64  `json:"black_diamond_fatigue_limit"`
	Online                   bool   `json:"online"`
	ServerNow                int64  `json:"server_now"`
}

// acctBlackDiamondView 黑钻现状的**统一视图**：不管是服务端实时查到的还是直读数据库兜底
// 查到的，都归一成这一个结构，界面只认它，并带上数据来源（Source）。
type acctBlackDiamondView struct {
	CharacterID      string
	CharacterName    string
	AccountID        string
	Active           bool
	ExpiresAt        int64
	RemainingSeconds int64
	Fatigue          int64
	FatigueLimit     int64
	Online           bool
	// Source 取值：acctBDSourceServer（服务端实时）/ acctBDSourceDatabase（直读游戏库兜底）
	Source string
}

const (
	acctBDSourceServer   = "server"
	acctBDSourceDatabase = "database"
)

// acctEnvelope 原生 GM 响应信封。
//
// ★ 服务端有**两套**信封，字段名不同，必须都认（2026-10-05 踩过）：
//
//	/local/dnf-gm/*        → {"ok":true,"data":{...}}        见 go-server gm_admin.go
//	/dnf/gm/characters/*   → {"success":true,"data":{...}}   见 go-server gm_tool_admin.go writeGMData
//
// 改造前只认 `ok`（bool）。于是 career 那条路（走 /dnf/gm/*）返回
// `{"success":true,...}` 时，`OK` 解析成 false → **HTTP 200 的成功响应被当成失败**；
// 而 error/message 也都是空 ⇒ 兜底拿 `response.Status` 当消息，最终报成
// 「DNF90Server 原生 GM 接口: 200 OK (HTTP 502)」—— 一句话里既有 200 又有 502，
// 完全没法排查。
//
// 两个字段都用**指针**：必须能区分"字段缺席"（另一套信封）与"显式 false"（真失败）。
type acctEnvelope struct {
	OK      *bool           `json:"ok"`
	Success *bool           `json:"success"`
	Message string          `json:"message"`
	Error   string          `json:"error"`
	Data    json.RawMessage `json:"data"`
}

// acctEnvelopeOK 判定这次调用算不算成功。
//
// 优先级：显式 ok → 显式 success → 都没有就以 HTTP 状态码为准。
// 最后那条兜底是给"将来又出现第三种信封"留的：**HTTP 2xx 一律按成功处理**，
// 绝不因为不认识对方的新字段就把成功当失败 —— 这正是本次事故的成因。
func acctEnvelopeOK(env acctEnvelope, status int) bool {
	if env.OK != nil {
		return *env.OK
	}
	if env.Success != nil {
		return *env.Success
	}
	return status >= 200 && status < 300
}

// acctEnvelopeBoolText 把三态 *bool 渲染成日志可读的文本（nil = 字段缺席）。
// 名字带 envelope 前缀：本包已有一个签名不同的 acctBoolText（account_mailbox.go）。
func acctEnvelopeBoolText(v *bool) string {
	if v == nil {
		return "缺席"
	}
	if *v {
		return "true"
	}
	return "false"
}

// acctNativeHTTPClient 对齐 GM nativeHTTPClient：无代理、禁压缩、小连接池、12s 超时。
var acctNativeHTTPClient = &http.Client{
	Timeout: acctGMTimeout,
	Transport: &http.Transport{
		Proxy:               nil,
		DisableCompression:  true,
		MaxIdleConns:        2,
		MaxIdleConnsPerHost: 2,
		IdleConnTimeout:     30 * time.Second,
	},
	CheckRedirect: func(_ *http.Request, _ []*http.Request) error {
		return http.ErrUseLastResponse
	},
}

// acctGMInstance 探测当前服务端根目录实例（adminListen/adminToken/PVFPath）。
func (a *App) acctGMInstance() (*serverdir.InstanceInfo, error) {
	if a == nil {
		return nil, errors.New("运维端尚未就绪")
	}
	// 用 currentServerRoot 而不是 a.uiSafe + 局部变量：walk 的 Synchronize 是异步投递，
	// 包在 uiSafe 里读到的只会是零值，函数会永远退化成"只用 cfg.ServerRoot"
	//（界面上刚改的服务端目录不生效）。详见 channel_admin.go currentServerRoot 的注释。
	root := a.currentServerRoot()
	if root == "" {
		return nil, errors.New("未配置服务端根目录，无法连接 DNF90Server 原生 GM 接口")
	}
	ins := serverdir.Inspect(root)
	if ins == nil || ins.Instance == nil || !ins.InstanceValid {
		return nil, fmt.Errorf("服务端根目录 %s 未探测到有效 instance.json，请先在服务端初始化实例", root)
	}
	if strings.TrimSpace(ins.Instance.AdminListen) == "" {
		return nil, errors.New("instance.json 未配置 server.adminListen，请先在服务端初始化实例")
	}
	return ins.Instance, nil
}

// acctPVFPathAbs 探测实例根目录并返回 instance.json game.pvfPath 的绝对路径。
// pvfPath 通常为相对路径（如 data/dnf/Script.pvf），相对实例根下的 runtime 目录解析。
func (a *App) acctPVFPathAbs() (string, error) {
	if a == nil {
		return "", errors.New("运维端尚未就绪")
	}
	// 同上：不能用 uiSafe 包着读控件，否则拿不到界面上当前填的服务端目录。
	root := a.currentServerRoot()
	if root == "" {
		return "", errors.New("未配置服务端根目录，无法定位 PVF 文件")
	}
	ins := serverdir.Inspect(root)
	if ins == nil || ins.Instance == nil || !ins.InstanceValid {
		return "", fmt.Errorf("服务端根目录 %s 未探测到有效 instance.json，请先在服务端初始化实例", root)
	}
	pvfPath := strings.TrimSpace(ins.Instance.PVFPath)
	if pvfPath == "" {
		return "", errors.New("instance.json 未配置 game.pvfPath，无法校验物品定义")
	}
	if !filepath.IsAbs(pvfPath) {
		pvfPath = filepath.Join(root, "runtime", filepath.FromSlash(pvfPath))
	}
	return pvfPath, nil
}

// acctGMConfig 校验并返回 GM 管理 baseURL 与 adminToken（设计 §4.3）。
// adminListen 必须回环（沿用 GM loopbackAdminAddress 语义）；adminToken 空/AUTO_GENERATE 时报可读错误。
func (a *App) acctGMConfig() (baseURL, token string, err error) {
	instance, err := a.acctGMInstance()
	if err != nil {
		return "", "", err
	}
	address, err := acctLoopbackAdminAddress(instance.AdminListen)
	if err != nil {
		return "", "", err
	}
	token = strings.TrimSpace(instance.AdminToken)
	if token == "" || strings.EqualFold(token, "AUTO_GENERATE") {
		return "", "", errors.New("instance.json 尚未生成 server.adminToken，无法连接 DNF90Server 原生 GM 接口")
	}
	return address, token, nil
}

// acctGMDo 低层 GM 调用：baseURL+path，Header X-Admin-Token + X-Admin-Actor，Content-Type json，
// 12s 超时，解析 {ok,message,error,data}。auditFile 非空时在 <运维配置目录>/audit/ 下追加 JSONL。
// actor 为空默认 "ops"（运维直连页操作人）。
func (a *App) acctGMDo(ctx context.Context, method, path string, body, out any, actor, auditFile string) (string, error) {
	if actor == "" {
		actor = "ops"
	}
	baseURL, token, err := a.acctGMConfig()
	if err != nil {
		return "", err
	}
	start := time.Now()
	message, callErr := acctGMCallRaw(ctx, baseURL, token, actor, method, path, body, out)
	if auditFile != "" && a != nil && a.cfg != nil {
		gmErr := ""
		if callErr != nil {
			gmErr = callErr.Error()
		}
		a.acctAuditAppend(auditFile, map[string]any{
			"ts":         time.Now().Format(time.RFC3339Nano),
			"actor":      actor,
			"method":     method,
			"path":       path,
			"gm_ok":      callErr == nil,
			"gm_error":   gmErr,
			"elapsed_ms": time.Since(start).Milliseconds(),
		})
	}
	return message, callErr
}

// acctGMCall 运维账号管理页（直连）GM 调用封装（设计 §4.3）；审计落 audit/account_direct.jsonl。
func (a *App) acctGMCall(ctx context.Context, method, path string, body, out any, actor string) (string, error) {
	return a.acctGMDo(ctx, method, path, body, out, actor, "account_direct.jsonl")
}

// acctGMCallRaw 无审计的底层 HTTP 转发（内部 manage 端点复用：Bridge 注入时审计由 handlers 负责）。
func acctGMCallRaw(ctx context.Context, baseURL, token, actor, method, path string, body, out any) (string, error) {
	if ctx == nil {
		ctx = context.Background()
	}
	callCtx, cancel := context.WithTimeout(ctx, acctGMTimeout)
	defer cancel()
	var reader io.Reader
	if body != nil {
		raw, err := json.Marshal(body)
		if err != nil {
			return "", err
		}
		reader = bytes.NewReader(raw)
	}
	request, err := http.NewRequestWithContext(callCtx, method, baseURL+path, reader)
	if err != nil {
		return "", err
	}
	request.Header.Set("Accept", "application/json")
	request.Header.Set("X-Admin-Token", token)
	request.Header.Set("X-Admin-Actor", actor)
	if body != nil {
		request.Header.Set("Content-Type", "application/json")
	}
	// 写操作补危险操作确认头（服务端 RequireConfirmation=true 时会校验，见 internal/gmhttp）。
	gmhttp.SetConfirm(request, actor)
	response, err := acctNativeHTTPClient.Do(request)
	if err != nil {
		return "", fmt.Errorf("连接 DNF90Server 原生 GM 接口: %w", err)
	}
	defer response.Body.Close()
	var envelope acctEnvelope
	if err := json.NewDecoder(io.LimitReader(response.Body, 1<<20)).Decode(&envelope); err != nil {
		return "", fmt.Errorf("解析 DNF90Server 原生 GM 响应: %w", err)
	}
	if response.StatusCode < 200 || response.StatusCode >= 300 || !acctEnvelopeOK(envelope, response.StatusCode) {
		message := strings.TrimSpace(envelope.Error)
		if message == "" {
			message = strings.TrimSpace(envelope.Message)
		}
		if message == "" {
			// 走到这里说明"信封说失败、但既没 error 也没 message"。
			// 把信封实际长什么样带出来，否则只剩一句 "200 OK" 完全没法排查。
			message = fmt.Sprintf("%s（响应无 error/message，信封解析：ok=%s success=%s）",
				response.Status, acctEnvelopeBoolText(envelope.OK), acctEnvelopeBoolText(envelope.Success))
		}
		return "", &acctGMStatusError{Status: response.StatusCode, Message: message}
	}
	if out != nil && len(envelope.Data) > 0 && string(envelope.Data) != "null" {
		if err := json.Unmarshal(envelope.Data, out); err != nil {
			return "", fmt.Errorf("解析 DNF90Server 原生 GM 数据: %w", err)
		}
	}
	return envelope.Message, nil
}

// acctGMStatusError 原生 GM 接口返回的**非 2xx 业务错误**，并且带上了服务端原始 HTTP 状态码。
//
// ── 为什么要单独一个类型（2026-10-05 修"转职报 HTTP 502"）──────────────────
// 改造前这里返回的是一句光秃秃的 `fmt.Errorf("DNF90Server 原生 GM 接口: %s", message)`，
// **状态码被丢掉了**。manage 层（另一个包）的 classifyAccountGMError 只能靠文案前缀去猜，
// 猜不到就一律兜底成 `502 Bad Gateway`。后果是把服务端的**业务拒绝**（400）伪装成**网关故障**：
//
//	跨大系转职 → 服务端 400 + "dnf gm request is invalid" → 运营看到 "HTTP 502"
//	→ 以为是网络/网关问题，实际是"这个操作服务端本来就不支持"
//
// 同一个兜底还会连带淹没其它真实 400：角色在副本里转职、目标分支不在 PVF —— 它们
// 服务端都给了专门文案，全被 502 吃掉。
type acctGMStatusError struct {
	Status  int
	Message string
}

func (e *acctGMStatusError) Error() string {
	return fmt.Sprintf("DNF90Server 原生 GM 接口: %s", e.Message)
}

// GMStatusCode / GMMessage 暴露服务端原始状态码与文案。
//
// manage 包通过**匿名接口**断言这两个方法取值，而不是 import 本类型 ——
// 既避免两个包互相依赖，又把"状态码必须传下去"变成了结构化契约：
// 以后新写的 GM 错误类型只要带上这两个方法就会被正确分类；不带则仍走 502 兜底
// （不会悄悄变成 200）。见 internal/manage/handlers_account.go classifyAccountGMError。
func (e *acctGMStatusError) GMStatusCode() int { return e.Status }

func (e *acctGMStatusError) GMMessage() string { return e.Message }

// acctLoopbackAdminAddress 校验 adminListen 仅允许回环地址（对齐 GM loopbackAdminAddress，R8）。
func acctLoopbackAdminAddress(listen string) (string, error) {
	listen = strings.TrimSpace(listen)
	host, port, err := net.SplitHostPort(listen)
	if err != nil {
		return "", fmt.Errorf("server.adminListen 无效: %w", err)
	}
	host = strings.TrimSpace(host)
	ip := net.ParseIP(host)
	if !strings.EqualFold(host, "localhost") && (ip == nil || !ip.IsLoopback()) {
		return "", fmt.Errorf("拒绝非回环管理地址 %q", listen)
	}
	portNumber, err := strconv.Atoi(port)
	if err != nil || portNumber < 1 || portNumber > 65535 {
		return "", fmt.Errorf("server.adminListen 端口无效: %q", port)
	}
	return "http://" + net.JoinHostPort(host, port), nil
}

// acctAuditAppend 在运维配置目录 audit/ 下追加一行 JSONL（目录不存在自动创建；失败静默不影响主流程）。
func (a *App) acctAuditAppend(sub string, rec map[string]any) {
	if a == nil || a.cfg == nil || rec == nil {
		return
	}
	cfgDir := filepath.Dir(a.cfg.Path())
	if strings.TrimSpace(cfgDir) == "" {
		return
	}
	dir := filepath.Join(cfgDir, "audit")
	if err := os.MkdirAll(dir, 0o755); err != nil {
		return
	}
	file, err := os.OpenFile(filepath.Join(dir, sub), os.O_CREATE|os.O_APPEND|os.O_WRONLY, 0o644)
	if err != nil {
		return
	}
	defer file.Close()
	_ = json.NewEncoder(file).Encode(rec)
}

// ---- 与 §2.2 GM-1~GM-11 一一对应的薄封装函数（签名对齐 GM native_admin.go，便于对照移植）----

// acctCharacters GM-1 角色查询。username/characterName 任一为空则不做该维度筛选；
// 带任一查询条件时沿用 GM 老版本判定（message != "character query completed" 视为不支持按条件查询）。
func (a *App) acctCharacters(ctx context.Context, username, characterName string) ([]acctCharacterView, string, error) {
	path := "/local/dnf-gm/characters"
	query := url.Values{}
	username = strings.TrimSpace(username)
	characterName = strings.TrimSpace(characterName)
	if username != "" {
		// 本服登录账号在运维工具账号库（launcher_accounts），服务端无 dnf_login_accounts 映射表：
		// 先解析 account_id 再走 GM 接口，避免触发服务端对该表的查询。
		if !a.svcOn.Load() || a.st == nil {
			return nil, "", errors.New("认证服务未运行，无法按登录账号解析，请先在「认证服务」页启动服务")
		}
		acc, err := a.st.GetAccountByUsername(ctx, username)
		if err != nil {
			if errors.Is(err, store.ErrUserNotFound) {
				return nil, "", fmt.Errorf("登录账号 %s 不存在", username)
			}
			return nil, "", fmt.Errorf("解析登录账号失败: %w", err)
		}
		query.Set("account_id", acc.AccountID)
	}
	if characterName != "" {
		query.Set("character_name", characterName)
	}
	if encoded := query.Encode(); encoded != "" {
		path += "?" + encoded
	}
	var rows []acctCharacterView
	message, err := a.acctGMCall(ctx, http.MethodGet, path, nil, &rows, "ops")
	if err != nil {
		return nil, "", err
	}
	if (username != "" || characterName != "") && message != "character query completed" {
		return nil, "", errors.New("当前运行的 DNF90Server 版本不支持登录账号/角色名查询，请替换新版服务端后重启")
	}
	return rows, message, nil
}

// acctMailSend GM-2 系统邮件（原生 body 语义由调用方组装）。
func (a *App) acctMailSend(ctx context.Context, request acctMailRequest) (acctMailResult, string, error) {
	var result acctMailResult
	message, err := a.acctGMCall(ctx, http.MethodPost, "/local/dnf-gm/mail", request, &result, "ops")
	return result, message, err
}

// acctWalletQuery GM-3 钱包余额查询。
func (a *App) acctWalletQuery(ctx context.Context, roleName string) (acctWalletResult, string, error) {
	return a.acctWalletDo(ctx, roleName, "", "", 0, false)
}

// acctWalletUpdate GM-4 钱包变动（mode: add|set）。
func (a *App) acctWalletUpdate(ctx context.Context, roleName, currency, mode string, amount int64) (acctWalletResult, string, error) {
	return a.acctWalletDo(ctx, roleName, currency, mode, amount, true)
}

func (a *App) acctWalletDo(ctx context.Context, roleName, currency, mode string, amount int64, update bool) (acctWalletResult, string, error) {
	path := "/local/dnf-gm/wallet/query"
	if update {
		path = "/local/dnf-gm/wallet/update"
	}
	body := map[string]any{"character_name": strings.TrimSpace(roleName)}
	if update {
		body["currency"] = currency
		body["mode"] = mode
		body["amount"] = amount
	}
	var result acctWalletResult
	message, err := a.acctGMCall(ctx, http.MethodPost, path, body, &result, "ops")
	return result, message, err
}

// acctCurrencyMail 资产邮件（对齐 GM nativeCurrencyMail）：gold → GM-2 金币邮件；
// cera → GM-4(add) 入账 + GM-2 到账通知。返回 data 结构一致，便于 UI 展示「已到账但通知失败」分支。
func (a *App) acctCurrencyMail(ctx context.Context, roleName, currency string, amount int64) (map[string]any, string, error) {
	roleName = strings.TrimSpace(roleName)
	currency = strings.ToLower(strings.TrimSpace(currency))
	if roleName == "" || (currency != "cera" && currency != "gold") || amount <= 0 || amount > acctMaxWalletBalance {
		return nil, "", errors.New("角色、资产类型或数量无效")
	}
	before, _, err := a.acctWalletQuery(ctx, roleName)
	if err != nil {
		return nil, "", err
	}
	if currency == "gold" {
		mailRequest := acctMailRequest{
			CharacterName: roleName,
			Title:         "GM 金币邮件",
			Body:          fmt.Sprintf("系统发放金币 %d，请领取附件。", amount),
			Gold:          amount,
		}
		var mailResult acctMailResult
		if _, err := a.acctGMCall(ctx, http.MethodPost, "/local/dnf-gm/mail", mailRequest, &mailResult, "ops"); err != nil {
			return nil, "", err
		}
		message := "金币邮件已发送，玩家领取邮件后到账"
		if mailResult.Refreshed {
			message = "金币邮件已发送，在线邮箱已实时刷新；领取后到账"
		}
		return map[string]any{
			"character": before.Character, "currency": "gold", "mode": "mail", "amount": amount,
			"before": before.Gold, "after": before.Gold,
			"cera": before.Cera, "gold": before.Gold,
			"online": mailResult.Online, "refreshed": mailResult.Refreshed,
			"delivery": "claimable_mail", "mail": mailResult,
		}, message, nil
	}
	credited, _, err := a.acctWalletUpdate(ctx, roleName, "cera", "add", amount)
	if err != nil {
		return nil, "", err
	}
	mailRequest := acctMailRequest{
		CharacterName: roleName,
		Title:         "GM 点券到账通知",
		Body:          fmt.Sprintf("系统已发放点券 %d，当前点券余额 %d。", amount, credited.Cera),
	}
	var mailResult acctMailResult
	_, mailErr := a.acctGMCall(ctx, http.MethodPost, "/local/dnf-gm/mail", mailRequest, &mailResult, "ops")
	data := map[string]any{
		"character": credited.Character, "currency": "cera", "mode": "mail", "amount": amount,
		"before": before.Cera, "after": credited.Cera,
		"cera": credited.Cera, "gold": credited.Gold,
		"online":    credited.Online || mailResult.Online,
		"refreshed": credited.Refreshed && (mailErr == nil && (!mailResult.Online || mailResult.Refreshed)),
		"delivery":  "credited_with_notice", "mail": mailResult,
	}
	if mailErr != nil {
		data["mail_error"] = mailErr.Error()
		return data, "点券已到账，但通知邮件发送失败；请勿重复操作", nil
	}
	if mailResult.Refreshed {
		return data, "点券已到账，通知邮件与在线邮箱均已实时刷新", nil
	}
	return data, "点券已到账，通知邮件已发送", nil
}

// acctItemMail 物品发放（系统邮件附件）：本地 PVF 索引 resolve 校验通过后提交 GM-2。
// 执行端校验见设计 §6.5：不存在 → 拒绝；不支持强化/增幅却填了参数 → 拒绝。
func (a *App) acctItemMail(ctx context.Context, roleName string, itemID, amount uint32, reinforce, amplify, amplifyType int) (map[string]any, string, error) {
	pvfPath, err := a.acctPVFPathAbs()
	if err != nil {
		return nil, "", err
	}
	catalog, err := pvfindex.Ensure(ctx, pvfPath, nil)
	if err != nil {
		return nil, "", fmt.Errorf("构建本地 PVF 索引失败: %w", err)
	}
	def, ok := catalog.Resolve(itemID)
	if !ok {
		return nil, "", fmt.Errorf("本地 PVF 无 item_id=%d，请核对两端 PVF 目录一致性", itemID)
	}
	def, err = pvfindex.WithUpgrade(def, reinforce, amplify, amplifyType)
	if err != nil {
		return nil, "", err
	}
	if err := pvfindex.Validate(def); err != nil {
		return nil, "", fmt.Errorf("PVF 物品定义不完整: %w", err)
	}
	if (reinforce > 0 || amplify > 0 || amplifyType > 0) && !def.SupportsUpgrade {
		return nil, "", fmt.Errorf("物品 %s（%d）不支持强化/增幅，无法按此参数发放", def.Name, def.ItemID)
	}
	nativeRequest := acctMailRequest{
		CharacterName: strings.TrimSpace(roleName),
		Title:         "GM 物品发放",
		Body:          fmt.Sprintf("系统发放：%s", def.Name),
		Attachments: []acctMailAttachment{{
			ItemID: def.ItemID, Count: amount, Bind: false,
			ReinforceLevel: reinforce, AmplifyLevel: amplify, AmplifyType: amplifyType,
		}},
	}
	var result acctMailResult
	message, err := a.acctGMCall(ctx, http.MethodPost, "/local/dnf-gm/mail", nativeRequest, &result, "ops")
	if err != nil {
		return nil, "", err
	}
	if result.Refreshed {
		message = "物品邮件已发送，在线邮箱已实时刷新"
	} else if result.Online {
		message = "物品邮件已发送；在线刷新未完成，下次打开邮箱时同步"
	} else {
		message = "物品邮件已发送；角色上线后可领取"
	}
	return map[string]any{
		"character": strings.TrimSpace(roleName),
		"item_id":   def.ItemID,
		"item_name": def.Name,
		"amount":    amount,
		"route":     "系统邮件附件",
		"mail":      result,
	}, message, nil
}

// acctSPTPGet GM-5 读取 SP/TP。
func (a *App) acctSPTPGet(ctx context.Context, characterID string) (acctSkillInfo, error) {
	var result acctSkillInfo
	_, err := a.acctGMCall(ctx, http.MethodGet, "/local/dnf-gm/character/sptp?id="+url.QueryEscape(characterID), nil, &result, "ops")
	return result, err
}

// acctSPTPSet GM-6 更新 SP/TP（remainingSP/remainingTP 为 nil 时不下发对应字段）。
func (a *App) acctSPTPSet(ctx context.Context, characterID string, remainingSP, remainingTP *int) error {
	body := map[string]any{"character_id": characterID}
	if remainingSP != nil {
		body["remaining_sp"] = *remainingSP
	}
	if remainingTP != nil {
		body["remaining_tp"] = *remainingTP
	}
	_, err := a.acctGMCall(ctx, http.MethodPost, "/local/dnf-gm/character/sptp", body, nil, "ops")
	return err
}

// acctLevelSet GM-7 设置等级。
func (a *App) acctLevelSet(ctx context.Context, characterID string, level int) error {
	_, err := a.acctGMCall(ctx, http.MethodPost, "/local/dnf-gm/character/level",
		map[string]any{"character_id": characterID, "level": level}, nil, "ops")
	return err
}

// acctCareerSet 跨大系转职：POST /dnf/gm/characters/{id}/career {job, first, awakening}。
//
// ── 为什么是另一条接口（2026-10-05）──────────────────────────────────────
// 同系的换分支/觉醒走 /local/dnf-gm/character/job；但那条路服务端**只改 grow_type、
// 不改 job**，而且对 `currentJob != request.Job` 直接拒绝（"base-job 转换在游戏内
// 没有对应的转职任务与客户端完成包，宁拒绝也不伪造 ACK"）。
//
// 跨大系必须走这条 career 路由：它**真的写 job**，并重建技能记录、重算 PVF 属性、
// 清掉旧系的转职/觉醒任务。代价是**要求角色离线**（在线热刷新撑不住换基础职业）。
//
// 这条路由早就挂在服务端了（internal/app/dnf90/gm_tool_admin.go，与 /local/dnf-gm/*
// 同一个 AdminMux），只是工具一直没接 —— 所以以前跨系只能撞在那句
// "dnf gm request is invalid" 上，还被误报成 HTTP 502。
func (a *App) acctCareerSet(ctx context.Context, characterID string, job, firstGrow, secondGrow int) error {
	_, err := a.acctGMCall(ctx, http.MethodPost,
		"/dnf/gm/characters/"+url.PathEscape(characterID)+"/career", map[string]any{
			"job":       job,
			"first":     firstGrow,
			"awakening": secondGrow,
		}, nil, "ops")
	return err
}

// acctJobGet GM-8 读取转职现状。
func (a *App) acctJobGet(ctx context.Context, characterID string) (acctJobInfo, error) {
	var result acctJobInfo
	_, err := a.acctGMCall(ctx, http.MethodGet, "/local/dnf-gm/character/job?id="+url.QueryEscape(characterID), nil, &result, "ops")
	return result, err
}

// acctJobSet GM-9 设置转职（GM 前端一次性提交 job/first_grow/second_grow，调用前需先 GET 现状）。
func (a *App) acctJobSet(ctx context.Context, characterID string, job, firstGrow, secondGrow int) error {
	_, err := a.acctGMCall(ctx, http.MethodPost, "/local/dnf-gm/character/job", map[string]any{
		"character_id": characterID, "job": job, "first_grow": firstGrow, "second_grow": secondGrow,
	}, nil, "ops")
	return err
}

// acctDungeonsUnlockAll GM-10 副本全部解锁（dungeons 表来自嵌入的 dungeons_max_difficulty.json）。
func (a *App) acctDungeonsUnlockAll(ctx context.Context, characterID string) (int, error) {
	var result struct {
		Count int `json:"count"`
	}
	_, err := a.acctGMCall(ctx, http.MethodPost, "/local/dnf-gm/dungeons/unlock-all", map[string]any{
		"character_id": characterID,
		"dungeons":     acctDungeonsMaxDifficulty(),
	}, &result, "ops")
	return result.Count, err
}

// acctEquipSlotsUnlockAll GM-11 装备槽全部解锁。
func (a *App) acctEquipSlotsUnlockAll(ctx context.Context, characterID string) (map[string]any, error) {
	var result map[string]any
	_, err := a.acctGMCall(ctx, http.MethodPost, "/local/dnf-gm/equip-slots/unlock-all",
		map[string]any{"character_id": characterID}, &result, "ops")
	if result == nil {
		result = make(map[string]any)
	}
	result["ok"] = err == nil
	return result, err
}

// acctBlackDiamondStatus 只读查询黑钻现状：优先服务端实时接口，老服务端自动退回直读游戏库。
//
// 为什么要兜底：GM 只读接口是随本次改造一起加的，用户手上的 DNF90Server.exe
// 可能是旧版本（那台机器上还没有这条 GET 路由，会返回 405）。黑钻的权威存储
// 就在游戏库的 dnf_account_metadata.premium_expire_1000，直接读库得到的结果与
// 服务端内存里的权益一致（权益每次变更都是事务内落盘），所以兜底是可用的。
func (a *App) acctBlackDiamondStatus(ctx context.Context, characterID string) (acctBlackDiamondView, error) {
	characterID = strings.TrimSpace(characterID)
	if characterID == "" {
		return acctBlackDiamondView{}, errors.New("缺少角色 ID")
	}
	var result acctBlackDiamondStatusResult
	_, err := a.acctGMCall(ctx, http.MethodGet,
		"/local/dnf-gm/character/black-diamond?id="+url.QueryEscape(characterID), nil, &result, "ops")
	if err == nil {
		remaining := result.RemainingSeconds
		// 用服务端时间重算剩余时长：运维机与服务器时钟不一致时以服务端为准。
		if result.ServerNow > 0 && result.ExpiresAt > 0 {
			remaining = result.ExpiresAt - result.ServerNow
			if remaining < 0 {
				remaining = 0
			}
		}
		return acctBlackDiamondView{
			CharacterID:      result.CharacterID,
			CharacterName:    result.CharacterName,
			AccountID:        result.AccountID,
			Active:           result.Active,
			ExpiresAt:        result.ExpiresAt,
			RemainingSeconds: remaining,
			Fatigue:          result.Fatigue,
			FatigueLimit:     result.FatigueLimit,
			Online:           result.Online,
			Source:           acctBDSourceServer,
		}, nil
	}

	// 服务端不可用/版本过旧：走直读游戏库兜底，但要把原因记进日志，避免"看起来正常"。
	fallback, err2 := a.acctBlackDiamondStatusFromDB(ctx, characterID)
	if err2 != nil {
		return acctBlackDiamondView{}, fmt.Errorf("服务端查询黑钻失败（%v），直读游戏库也失败：%v", err, err2)
	}
	acctLog("服务端黑钻查询接口不可用（%v），已改为直读游戏库获取黑钻现状", err)
	return fallback, nil
}

// acctBlackDiamondStatusFromDB 直读游戏库兜底（老服务端没有黑钻只读接口时使用）。
func (a *App) acctBlackDiamondStatusFromDB(ctx context.Context, characterID string) (acctBlackDiamondView, error) {
	st := a.st
	if st == nil {
		return acctBlackDiamondView{}, errors.New("认证服务未运行，无法读取游戏库")
	}
	row, found, err := st.BlackDiamondStatus(ctx, characterID)
	if err != nil {
		return acctBlackDiamondView{}, err
	}
	if !found {
		return acctBlackDiamondView{}, fmt.Errorf("游戏库里没有角色 %s", characterID)
	}
	return acctBlackDiamondView{
		CharacterID:      row.CharacterID,
		CharacterName:    row.CharacterName,
		AccountID:        row.AccountID,
		Active:           row.Active,
		ExpiresAt:        row.ExpiresAt,
		RemainingSeconds: row.RemainingSeconds,
		Fatigue:          row.Fatigue,
		FatigueLimit:     row.FatigueLimit,
		Source:           acctBDSourceDatabase,
	}, nil
}

// acctBlackDiamondActivate GM-12 开启/续期黑钻（按天）。
//
// 服务端语义（go-server GMActivateBlackDiamond）：
//   - 黑钻是**账号级**权益（premium_expire_1000），同账号所有角色共享；
//   - 到期时间按 max(现在, 当前到期时间) + 天数 计算 → 到期时间未过就是"续期叠加"；
//   - days=0 时服务端按默认 30 天处理，这里始终显式传天数；
//   - 在线角色会顺带下发原生黑钻包（城内安全场景立即生效，否则推迟到回城生效），
//     离线角色在下一次登录 select-character 时读取权益。
func (a *App) acctBlackDiamondActivate(ctx context.Context, characterID string, days int) (acctBlackDiamondResult, string, error) {
	var result acctBlackDiamondResult
	if days < 1 || days > acctBlackDiamondMaxDays {
		return result, "", fmt.Errorf("黑钻天数必须在 1~%d 之间（按天算）", acctBlackDiamondMaxDays)
	}
	message, err := a.acctGMCall(ctx, http.MethodPost, "/local/dnf-gm/character/black-diamond",
		map[string]any{"character_id": characterID, "days": days}, &result, "ops")
	if err != nil {
		return result, "", err
	}
	switch {
	case result.Deferred:
		message = "黑钻已激活（账号级）；在线角色要回城后才刷新显示，离线角色下次登录生效"
	case result.Refreshed:
		message = "黑钻已激活并已下发原生黑钻状态包"
	case result.Online && result.RefreshError != "":
		message = "黑钻已激活；在线刷新失败：" + result.RefreshError
	case !result.Online:
		message = "黑钻已激活；角色离线，登录后自动生效"
	}
	return result, message, nil
}
