//go:build paid

// AC 账号管理端点（阶段1，设计 §5.2 / §7.2）：/manage/v1/account/*。
// 本包（internal/manage）为 paid 构建，所有业务能力经 Bridge 注入复用主程序实现，
// 不直接引用 main 包符号（account_gm.go 属 package main，独立维护本包所需视图类型）。
// 转发 GM 统一 actor="manage"；审计统一落 audit/manage_account.jsonl。
package manage

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"math"
	"net/http"
	"net/url"
	"os"
	"path/filepath"
	"sort"
	"strconv"
	"strings"
	"sync"
	"time"

	"dnf90ops/internal/pvfindex"
	"dnf90ops/internal/store"
)

// ---- GM-1~GM-11 原生视图类型（JSON 与 GM native_admin.go / 主程序 account_gm.go 对齐）----

type gmCharacterView struct {
	CharacterID string `json:"character_id"`
	AccountID   string `json:"account_id"`
	Name        string `json:"name"`
	Level       int    `json:"level"`
	Gold        int64  `json:"gold"`
	Cera        int64  `json:"cera"`
	Online      bool   `json:"online"`
}

type gmMailAttachment struct {
	ItemID         uint32 `json:"item_id"`
	Count          uint32 `json:"count"`
	Bind           bool   `json:"bind"`
	ReinforceLevel int    `json:"reinforce_level,omitempty"`
	AmplifyLevel   int    `json:"amplify_level,omitempty"`
	AmplifyType    int    `json:"amplify_type,omitempty"`
}

type gmMailRequest struct {
	CharacterID   string             `json:"character_id,omitempty"`
	CharacterName string             `json:"character_name,omitempty"`
	Title         string             `json:"title,omitempty"`
	Body          string             `json:"body,omitempty"`
	Gold          int64              `json:"gold,omitempty"`
	Attachments   []gmMailAttachment `json:"attachments,omitempty"`
}

type gmMailResult struct {
	MailID               string `json:"mail_id"`
	RecipientCharacterID string `json:"recipient_character_id"`
	RecipientName        string `json:"recipient_name"`
	Online               bool   `json:"online"`
	Refreshed            bool   `json:"refreshed"`
	RefreshError         string `json:"refresh_error,omitempty"`
}

type gmWalletResult struct {
	Character gmCharacterView `json:"character"`
	Currency  string          `json:"currency"`
	Mode      string          `json:"mode"`
	Before    int64           `json:"before"`
	After     int64           `json:"after"`
	Cera      int64           `json:"cera"`
	// TokenCera / HappyTokenCera 账号级代币券 / 欢乐代币券（服务端 GMWalletResult 同名字段）。
	TokenCera      int64  `json:"token_cera"`
	HappyTokenCera int64  `json:"happy_token_cera"`
	Gold           int64  `json:"gold"`
	Online         bool   `json:"online"`
	Refreshed      bool   `json:"refreshed"`
	RefreshError   string `json:"refresh_error"`
}

type gmSkillInfo struct {
	CharacterID string `json:"character_id"`
	Name        string `json:"name"`
	Level       int    `json:"level"`
	TotalSP     int    `json:"total_sp"`
	RemainingSP int    `json:"remaining_sp"`
	TotalTP     int    `json:"total_tp"`
	RemainingTP int    `json:"remaining_tp"`
}

type gmJobInfo struct {
	CharacterID string `json:"character_id"`
	Job         int    `json:"job"`
	FirstGrow   int    `json:"first_grow"`
	SecondGrow  int    `json:"second_grow"`
}

// ---- 常量（对齐 GM / 主程序） ----

const (
	accountMaxWalletBalance = int64(math.MaxInt32) // 资产邮件单次数量上限（GM maxWalletBalance）
	accountMailGoldMax      = int64(^uint32(0))    // 邮件金币上限
	accountGMTimeout        = 12 * time.Second
	// accountBlackDiamondMaxDays 黑钻单次开启天数上限，与服务端
	// GMActivateBlackDiamond 的 1..3650 天校验一致。
	accountBlackDiamondMaxDays = 3650
)

// ---- 审计辅助（设计 §7.2 字段） ----

type acctAudit struct {
	start  time.Time
	actor  string
	body   []byte
	fields map[string]any
	gmOK   bool
	gmErr  string
	gmUsed bool
}

func (a *acctAudit) ok() { a.gmUsed, a.gmOK = true, true }
func (a *acctAudit) fail(err error) {
	a.gmUsed, a.gmOK, a.gmErr = true, false, err.Error()
}

func (s *Server) acctAuditEnd(r *http.Request, audit *acctAudit) {
	if s == nil || audit == nil || s.bridge == nil || s.bridge.AcctAudit == nil {
		return
	}
	tokenHash := ""
	if raw := strings.TrimSpace(r.Header.Get("X-Manage-Token")); raw != "" {
		sum := sha256.Sum256([]byte(raw))
		tokenHash = hex.EncodeToString(sum[:6]) // sha256 前 12 位
	}
	payloadDigest := ""
	if len(audit.body) > 0 {
		sum := sha256.Sum256(audit.body)
		payloadDigest = hex.EncodeToString(sum[:])
	}
	detail := map[string]any{
		"ts":                time.Now().Format(time.RFC3339Nano),
		"method":            r.Method,
		"path":              r.URL.Path,
		"remote_ip":         clientIP(r),
		"manage_token_hash": tokenHash,
		"payload_digest":    payloadDigest,
		"actor":             audit.actor,
		"gm_ok":             audit.gmOK,
		"gm_error":          audit.gmErr,
		"elapsed_ms":        time.Since(audit.start).Milliseconds(),
	}
	for k, v := range audit.fields {
		if v != nil {
			detail[k] = v
		}
	}
	s.bridge.AcctAudit(r.Context(), audit.actor, r.Method, r.URL.Path, detail)
}

// acctDecode 读取请求体（审计需原始字节）并 JSON 解码；失败已写错误信封并返回 false。
func (s *Server) acctDecode(w http.ResponseWriter, r *http.Request, v any) ([]byte, bool) {
	raw, err := io.ReadAll(io.LimitReader(r.Body, 1<<20))
	if err != nil {
		writeErr(w, http.StatusBadRequest, "bad-request: 读取请求体失败")
		return nil, false
	}
	if len(raw) == 0 {
		writeErr(w, http.StatusBadRequest, "bad-request: 请求体为空")
		return nil, false
	}
	if err := json.Unmarshal(raw, v); err != nil {
		writeErr(w, http.StatusBadRequest, "bad-request: JSON 解析失败: "+err.Error())
		return nil, false
	}
	return raw, true
}

// acctGM 转发 DNF90Server 原生 GM 接口（actor 固定 manage）。
func (s *Server) acctGM(ctx context.Context, method, path string, body, out any) (string, error) {
	if s.bridge == nil || s.bridge.AcctGMCall == nil {
		return "", errors.New("gm-not-available: 运维端未注入 GM 直连能力")
	}
	return s.bridge.AcctGMCall(ctx, method, path, body, out, "manage")
}

// classifyAccountGMError 将 GM 直连/信封错误分类为带机器可读 code 的可读文案（§5.1）。
func classifyAccountGMError(err error) (int, string) {
	if err == nil {
		return http.StatusOK, ""
	}
	text := err.Error()
	// 已带 code 前缀（本包产生的校验/角色/索引错误）原样返回。
	for _, prefix := range []string{"gm-not-available:", "pvf-mismatch:", "role-not-found:", "bad-request:"} {
		if strings.HasPrefix(text, prefix) {
			if prefix == "bad-request:" {
				return http.StatusBadRequest, text
			}
			if prefix == "role-not-found:" {
				return http.StatusNotFound, text
			}
			return http.StatusBadGateway, text
		}
	}
	// 原生 GM 接口返回的非 2xx：**原样透传服务端状态码**，不要再一律兜底成 502。
	//
	// ── 为什么必须这么做（2026-10-05 修"转职报 HTTP 502"）─────────────────────
	// 改造前：主程序 acctGMCallRaw 丢掉了 HTTP 状态码，这里只能靠文案前缀猜，
	// 猜不到就落到最下面的 `StatusBadGateway`。于是服务端的**业务拒绝**（400）
	// 被伪装成**网关故障**（502）：
	//
	//   跨大系转职 → 服务端 400 + "dnf gm request is invalid" → 运营看到 "HTTP 502"
	//
	// 结果是把"这个操作服务端不支持"误导成"网络/网关坏了"，排查方向完全跑偏。
	// 同一兜底还会淹没角色在副本里转职、目标分支不在 PVF 等**本来有专门文案**的 400。
	//
	// 类型用**匿名接口**断言（实现在主程序 account_gm.go 的 acctGMStatusError），
	// 避免两个包互相 import。
	if sc, ok := err.(interface {
		GMStatusCode() int
		GMMessage() string
	}); ok {
		status, message := sc.GMStatusCode(), strings.TrimSpace(sc.GMMessage())
		// 2xx 却走到错误分支：只可能是"HTTP 说成功、信封判定失败"—— 协议不一致。
		// 这时报 gm-not-available 是**误导**（服务端明明活着），而且现场那句
		// 「DNF90Server 原生 GM 接口: 200 OK (HTTP 502)」既出现 200 又出现 502，
		// 让人完全无法判断到底是成功还是失败。给一句能直接指向根因的话。
		if status >= 200 && status < 300 {
			return http.StatusBadGateway, "gm-envelope-mismatch: 服务端返回成功状态码，但响应信封被判为失败。" +
				"最常见原因：调用的接口族与解析用的信封格式不一致" +
				"（/local/dnf-gm/* 用 {\"ok\":...}，/dnf/gm/* 用 {\"success\":...}）。原始响应：" + message
		}
		if status == http.StatusNotFound {
			return http.StatusNotFound, "role-not-found: " + message
		}
		if status >= 400 && status < 500 {
			// "dnf gm request is invalid" 是服务端的通用拒绝码，覆盖多种原因，
			// 原样丢给运营等于没给信息 —— 补上可操作的三条常见原因。
			if strings.Contains(message, "dnf gm request is invalid") {
				return status, "gm-rejected: 服务端拒绝了这次操作。常见原因：" +
					"① 不支持跨大系转职（如鬼剑士→魔法师；同系换分支/觉醒是支持的）；" +
					"② 角色正在副本里（需先回城）；" +
					"③ 目标职业/觉醒分支在本服 PVF 里不存在。" +
					"原始响应：" + message
			}
			return status, message
		}
		// 5xx 才是真的"服务端/链路不可用"。
		return http.StatusBadGateway, "gm-not-available: " + text
	}
	// HTTP/解析层失败（连接失败、响应解析失败、配置缺失）→ gm-not-available。
	switch {
	case strings.HasPrefix(text, "连接 DNF90Server 原生 GM 接口:"),
		strings.HasPrefix(text, "解析 DNF90Server 原生 GM 响应:"),
		strings.HasPrefix(text, "解析 DNF90Server 原生 GM 数据:"):
		return http.StatusBadGateway, "gm-not-available: " + text
	case strings.Contains(text, "adminListen"),
		strings.Contains(text, "adminToken"),
		strings.Contains(text, "instance.json"),
		strings.Contains(text, "回环"),
		strings.Contains(text, "未配置服务端根目录"),
		strings.Contains(text, "未探测到有效 instance"):
		return http.StatusBadGateway, "gm-not-available: " + text
	}
	// 信封业务错误（GM-1~GM-11 返回的 message/error，如角色不存在、老版本不支持）原样上抛。
	// 角色不存在类错误映射为机器可读 role-not-found（§5.1 错误码约定）。
	if strings.Contains(text, "角色不存在") || strings.Contains(text, "character not found") || strings.Contains(text, "characters not found") {
		return http.StatusNotFound, "role-not-found: " + text
	}
	return http.StatusBadGateway, text
}

// acctWriteGMError 记录审计并写出带 code 的错误信封。
func (s *Server) acctWriteGMError(w http.ResponseWriter, audit *acctAudit, err error) {
	if audit != nil {
		audit.fail(err)
	}
	status, text := classifyAccountGMError(err)
	writeErr(w, status, text)
}

// acctInstancePVFPath 取执行端（运维 manage）本机 instance.json 的 game.pvfPath。
func (s *Server) acctInstancePVFPath() (string, error) {
	if s.bridge == nil || s.bridge.Inspect == nil || s.bridge.ServerRoot == nil {
		return "", errors.New("gm-not-available: 运维端未注入服务端目录探测能力")
	}
	root := strings.TrimSpace(s.bridge.ServerRoot())
	if root == "" {
		return "", errors.New("gm-not-available: 未配置服务端根目录")
	}
	ins := s.bridge.Inspect(root)
	if ins == nil || ins.Instance == nil || !ins.InstanceValid {
		return "", errors.New("gm-not-available: 服务端目录未探测到有效 instance.json，请先初始化实例")
	}
	pvfPath := strings.TrimSpace(ins.Instance.PVFPath)
	if pvfPath == "" {
		return "", errors.New("gm-not-available: instance.json 未配置 game.pvfPath，无法执行本地物品校验")
	}
	if !filepath.IsAbs(pvfPath) {
		pvfPath = filepath.Join(root, "runtime", filepath.FromSlash(pvfPath))
	}
	return pvfPath, nil
}

// acctResolveItemForGrant 执行端校验（设计 §6.5）：本地 PVF 索引 resolve + 强化/增幅合法性。
func (s *Server) acctResolveItemForGrant(ctx context.Context, itemID uint32, reinforce, amplify, amplifyType int) (pvfindex.Definition, error) {
	pvfPath, err := s.acctInstancePVFPath()
	if err != nil {
		return pvfindex.Definition{}, err
	}
	catalog, err := pvfindex.Ensure(ctx, pvfPath, nil)
	if err != nil {
		return pvfindex.Definition{}, fmt.Errorf("gm-not-available: 构建本地 PVF 索引失败: %w", err)
	}
	def, ok := catalog.Resolve(itemID)
	if !ok {
		return pvfindex.Definition{}, fmt.Errorf("pvf-mismatch: 本地 PVF 无 item_id=%d，请核对两端 PVF 一致性", itemID)
	}
	def, err = pvfindex.WithUpgrade(def, reinforce, amplify, amplifyType)
	if err != nil {
		return pvfindex.Definition{}, fmt.Errorf("pvf-mismatch: %w", err)
	}
	if err := pvfindex.Validate(def); err != nil {
		return pvfindex.Definition{}, fmt.Errorf("pvf-mismatch: %w", err)
	}
	return def, nil
}

// acctMailMessage GM 邮件三态文案（设计 R4：禁止把三态抹成单一"发送成功"）。
func acctMailMessage(result gmMailResult) string {
	if result.Refreshed {
		return "邮件已提交，在线客户端邮箱已实时刷新"
	}
	if result.Online {
		return "邮件已提交；在线刷新未完成，下次打开邮箱时自动同步"
	}
	return "邮件已提交；角色离线，上线后可直接领取"
}

// ---- AC-1 角色查询 ----

// POST /manage/v1/account/characters/query
func (s *Server) handleAccountCharactersQuery(w http.ResponseWriter, r *http.Request) {
	audit := &acctAudit{start: time.Now(), actor: "manage", fields: map[string]any{}}
	defer s.acctAuditEnd(r, audit)
	var req struct {
		Username      string `json:"username"`
		CharacterName string `json:"character_name"`
	}
	raw, ok := s.acctDecode(w, r, &req)
	if !ok {
		return
	}
	audit.body = raw
	req.Username = strings.TrimSpace(req.Username)
	req.CharacterName = strings.TrimSpace(req.CharacterName)
	if req.Username == "" && req.CharacterName == "" {
		writeErr(w, http.StatusBadRequest, "bad-request: username 与 character_name 至少填写一项")
		return
	}
	audit.fields["username"] = req.Username
	audit.fields["character_name"] = req.CharacterName

	path := "/local/dnf-gm/characters"
	query := url.Values{}
	if req.Username != "" {
		// 本服登录账号在运维工具账号库（launcher_accounts），服务端无 dnf_login_accounts 映射表：
		// 先解析 account_id 再走 GM 接口，避免触发服务端对该表的查询。
		st := s.bridgeStore()
		if st == nil {
			s.acctWriteGMError(w, audit, errors.New("gm-not-available: 认证服务未运行，无法按登录账号解析"))
			return
		}
		acc, err := st.GetAccountByUsername(r.Context(), req.Username)
		if err != nil {
			if errors.Is(err, store.ErrUserNotFound) {
				s.acctWriteGMError(w, audit, fmt.Errorf("登录账号 %s 不存在", req.Username))
			} else {
				s.acctWriteGMError(w, audit, fmt.Errorf("解析登录账号失败: %w", err))
			}
			return
		}
		query.Set("account_id", acc.AccountID)
		audit.fields["account_id"] = acc.AccountID
	}
	if req.CharacterName != "" {
		query.Set("character_name", req.CharacterName)
	}
	path += "?" + query.Encode()
	var rows []gmCharacterView
	message, err := s.acctGM(r.Context(), http.MethodGet, path, nil, &rows)
	if err != nil {
		s.acctWriteGMError(w, audit, err)
		return
	}
	// 老版本服务端不支持按账号/角色名查询的判定（GM-1，R1）。
	if message != "" && message != "character query completed" {
		err := errors.New("当前运行的 DNF90Server 版本不支持登录账号/角色名查询，请替换新版服务端后重启")
		s.acctWriteGMError(w, audit, err)
		return
	}
	audit.ok()
	if rows == nil {
		rows = []gmCharacterView{}
	}
	writeOK(w, rows)
}

// ---- AC-13 在线角色表 ----

// onlineCharacterRow 在线角色行：登录账号 + GM 角色视图 + 会话来源信息。
type onlineCharacterRow struct {
	Username    string `json:"username"` // 登录账号（launcher_accounts.username）
	AccountID   string `json:"account_id"`
	CharacterID string `json:"character_id"`
	Name        string `json:"name"`
	Level       int    `json:"level"`
	Gold        int64  `json:"gold"`
	Cera        int64  `json:"cera"`
	LastIP      string `json:"last_ip"`
	MachineID   string `json:"machine_id"`
}

// onlineCharactersResult GET /manage/v1/account/online-characters 响应。
type onlineCharactersResult struct {
	Characters []onlineCharacterRow `json:"characters"`
	Accounts   int                  `json:"accounts"`   // 参与检查的在线账号数
	Failures   int                  `json:"failures"`   // 角色查询失败的账号数（不影响其余结果）
	CheckedAt  string               `json:"checked_at"` // 快照时刻（服务端当地时间）
}

// onlineFetchConcurrency 逐账号查角色时的并发上限。
const onlineFetchConcurrency = 8

// onlineCharFetcher 取某账号的角色列表（注入以便单测；生产注入 GM 直连）。
type onlineCharFetcher func(ctx context.Context, accountID string) ([]gmCharacterView, error)

// GET /manage/v1/account/online-characters
//
// 取一份"在线角色"快照（一次请求 = 一份快照，管理端不做自动轮询）。
//
// 为什么不在一条 SQL 里查完：账号库只能回答"哪些账号的登录器会话还活着"
// （包含停在登录器/选人界面的玩家），而"角色是否真在游戏内"只有 GM 接口
// /local/dnf-gm/characters 的 online 字段说了算——泡点发放用的也是这套判定。
// 因此这里先取在线账号，再逐账号查角色、只留 online=true。
func (s *Server) handleAccountOnlineCharacters(w http.ResponseWriter, r *http.Request) {
	st := s.bridgeStore()
	if st == nil {
		writeErr(w, http.StatusBadGateway, "gm-not-available: 认证服务未运行，无法读取在线会话")
		return
	}
	limit := 500
	if v := strings.TrimSpace(r.URL.Query().Get("limit")); v != "" {
		if n, err := strconv.Atoi(v); err == nil && n > 0 && n <= 5000 {
			limit = n
		}
	}
	// 整体超时兜底：个别账号查询卡住也不把管理端请求挂死。
	ctx, cancel := context.WithTimeout(r.Context(), 30*time.Second)
	defer cancel()

	accounts, err := st.ListOnlineAccounts(ctx, limit)
	if err != nil {
		writeErr(w, http.StatusBadGateway, "db-error: 读取在线会话失败: "+err.Error())
		return
	}

	rows, failures := collectOnlineCharacters(ctx, accounts, func(ctx context.Context, accountID string) ([]gmCharacterView, error) {
		var list []gmCharacterView
		path := "/local/dnf-gm/characters?account_id=" + url.QueryEscape(accountID)
		if _, err := s.acctGM(ctx, http.MethodGet, path, nil, &list); err != nil {
			return nil, err
		}
		return list, nil
	})

	writeOK(w, onlineCharactersResult{
		Characters: rows,
		Accounts:   len(accounts),
		Failures:   failures,
		CheckedAt:  time.Now().Format(time.RFC3339),
	})
}

// collectOnlineCharacters 并发查询各账号角色并只保留在线角色，最后按登录账号+角色名排序。
// 单账号查询失败只计入 failures，不影响其它账号的结果。
func collectOnlineCharacters(ctx context.Context, accounts []store.OnlineAccount, fetch onlineCharFetcher) ([]onlineCharacterRow, int) {
	var (
		mu       sync.Mutex
		rows     []onlineCharacterRow
		failures int
		wg       sync.WaitGroup
	)
	sem := make(chan struct{}, onlineFetchConcurrency)
	for _, acc := range accounts {
		acc := acc
		wg.Add(1)
		go func() {
			defer wg.Done()
			sem <- struct{}{}
			defer func() { <-sem }()

			chars, err := fetch(ctx, acc.AccountID)

			mu.Lock()
			defer mu.Unlock()
			if err != nil {
				failures++
				return
			}
			for _, ch := range chars {
				if !ch.Online {
					continue // 账号有会话但角色不在游戏内（停在登录器/选人界面）
				}
				rows = append(rows, onlineCharacterRow{
					Username:    acc.Username,
					AccountID:   acc.AccountID,
					CharacterID: ch.CharacterID,
					Name:        ch.Name,
					Level:       ch.Level,
					Gold:        ch.Gold,
					Cera:        ch.Cera,
					LastIP:      acc.LastIP,
					MachineID:   acc.MachineID,
				})
			}
		}()
	}
	wg.Wait()
	sort.Slice(rows, func(i, j int) bool {
		if rows[i].Username != rows[j].Username {
			return rows[i].Username < rows[j].Username
		}
		return rows[i].Name < rows[j].Name
	})
	return rows, failures
}

// ---- AC-2 系统邮件 ----

// POST /manage/v1/account/mail/send
func (s *Server) handleAccountMailSend(w http.ResponseWriter, r *http.Request) {
	audit := &acctAudit{start: time.Now(), actor: "manage", fields: map[string]any{}}
	defer s.acctAuditEnd(r, audit)
	var req struct {
		RoleName string `json:"role_name"`
		Title    string `json:"title"`
		Body     string `json:"body"`
		Gold     int64  `json:"gold"`
		ItemID   uint32 `json:"item_id"`
		Amount   uint32 `json:"amount"`
		Bind     bool   `json:"bind"`
	}
	raw, ok := s.acctDecode(w, r, &req)
	if !ok {
		return
	}
	audit.body = raw
	req.RoleName = strings.TrimSpace(req.RoleName)
	req.Title = strings.TrimSpace(req.Title)
	req.Body = strings.TrimSpace(req.Body)
	if req.RoleName == "" {
		writeErr(w, http.StatusBadRequest, "bad-request: 缺少收件角色 role_name")
		return
	}
	if req.Gold < 0 || req.Gold > accountMailGoldMax {
		writeErr(w, http.StatusBadRequest, "bad-request: 邮件金币超出 uint32 范围")
		return
	}
	if (req.ItemID == 0) != (req.Amount == 0) {
		writeErr(w, http.StatusBadRequest, "bad-request: 附件物品 ID 与数量必须同时填写")
		return
	}
	if req.Title == "" && req.Body == "" && req.Gold == 0 && req.ItemID == 0 {
		writeErr(w, http.StatusBadRequest, "bad-request: 邮件标题/正文/金币/附件至少填写一项")
		return
	}
	audit.fields["role_name"] = req.RoleName
	audit.fields["title"] = req.Title
	audit.fields["gold"] = req.Gold
	audit.fields["item_id"] = req.ItemID
	audit.fields["amount"] = req.Amount
	audit.fields["bind"] = req.Bind

	nativeRequest := gmMailRequest{
		CharacterName: req.RoleName,
		Title:         req.Title,
		Body:          req.Body,
		Gold:          req.Gold,
	}
	if req.ItemID != 0 {
		// 执行端校验（设计 §6.5：AC-2 带附件同样先 resolve）。
		if _, err := s.acctResolveItemForGrant(r.Context(), req.ItemID, 0, 0, 0); err != nil {
			s.acctWriteGMError(w, audit, err)
			return
		}
		nativeRequest.Attachments = []gmMailAttachment{{
			ItemID: req.ItemID,
			Count:  req.Amount,
			Bind:   req.Bind,
		}}
	}
	var result gmMailResult
	if _, err := s.acctGM(r.Context(), http.MethodPost, "/local/dnf-gm/mail", nativeRequest, &result); err != nil {
		s.acctWriteGMError(w, audit, err)
		return
	}
	audit.ok()
	writeOK(w, map[string]any{
		"mail_id":        result.MailID,
		"recipient_name": result.RecipientName,
		"online":         result.Online,
		"refreshed":      result.Refreshed,
		"refresh_error":  result.RefreshError,
		"message":        acctMailMessage(result),
	})
}

// ---- AC-3 钱包余额查询 ----

// POST /manage/v1/account/wallet/query
func (s *Server) handleAccountWalletQuery(w http.ResponseWriter, r *http.Request) {
	audit := &acctAudit{start: time.Now(), actor: "manage", fields: map[string]any{}}
	defer s.acctAuditEnd(r, audit)
	var req struct {
		RoleName string `json:"role_name"`
	}
	raw, ok := s.acctDecode(w, r, &req)
	if !ok {
		return
	}
	audit.body = raw
	req.RoleName = strings.TrimSpace(req.RoleName)
	if req.RoleName == "" {
		writeErr(w, http.StatusBadRequest, "bad-request: 缺少 role_name")
		return
	}
	audit.fields["role_name"] = req.RoleName
	var result gmWalletResult
	message, err := s.acctGM(r.Context(), http.MethodPost, "/local/dnf-gm/wallet/query", map[string]any{
		"character_name": req.RoleName,
	}, &result)
	if err != nil {
		s.acctWriteGMError(w, audit, err)
		return
	}
	audit.ok()
	writeOK(w, map[string]any{
		"wallet":  result,
		"message": message,
	})
}

// ---- AC-3b 钱包变动（背包编辑页的"删除钱包内容"用） ----

// POST /manage/v1/account/wallet/update {role_name, currency, mode, amount}
//
// currency 放行四种服务端钱包币种：cera（点券）/ token_cera（代币券）/
// happy_token_cera（欢乐代币券）/ gold（金币）；mode 只放行 set / add。
// 注意复活币**不在**这里：它是主背包 1 号格的物品，走 /manage/v1/account/inventory/delete。
func (s *Server) handleAccountWalletUpdate(w http.ResponseWriter, r *http.Request) {
	audit := &acctAudit{start: time.Now(), actor: "manage", fields: map[string]any{}}
	defer s.acctAuditEnd(r, audit)
	var req struct {
		RoleName string `json:"role_name"`
		Currency string `json:"currency"`
		Mode     string `json:"mode"`
		Amount   int64  `json:"amount"`
	}
	raw, ok := s.acctDecode(w, r, &req)
	if !ok {
		return
	}
	audit.body = raw
	req.RoleName = strings.TrimSpace(req.RoleName)
	req.Currency = strings.ToLower(strings.TrimSpace(req.Currency))
	req.Mode = strings.ToLower(strings.TrimSpace(req.Mode))
	if req.Mode == "" {
		req.Mode = "set"
	}
	if req.RoleName == "" || (req.Mode != "set" && req.Mode != "add") ||
		req.Amount < 0 || req.Amount > accountMaxWalletBalance {
		writeErr(w, http.StatusBadRequest, "bad-request: role_name/currency/mode/amount 不合法")
		return
	}
	if !acctWalletCurrencyKnown(req.Currency) {
		writeErr(w, http.StatusBadRequest, "bad-request: currency 只能是 cera/token_cera/happy_token_cera/gold")
		return
	}
	audit.fields["role_name"] = req.RoleName
	audit.fields["currency"] = req.Currency
	audit.fields["mode"] = req.Mode
	audit.fields["amount"] = req.Amount
	var result gmWalletResult
	message, err := s.acctGM(r.Context(), http.MethodPost, "/local/dnf-gm/wallet/update", map[string]any{
		"character_name": req.RoleName,
		"currency":       req.Currency,
		"mode":           req.Mode,
		"amount":         req.Amount,
	}, &result)
	if err != nil {
		s.acctWriteGMError(w, audit, err)
		return
	}
	audit.ok()
	writeOK(w, map[string]any{
		"wallet":  result,
		"message": message,
	})
}

// acctWalletCurrencyKnown 放行四种服务端钱包币种（与 dnfbridge.GMUpdateWallet 一致）。
func acctWalletCurrencyKnown(currency string) bool {
	switch currency {
	case "cera", "token_cera", "happy_token_cera", "gold":
		return true
	default:
		return false
	}
}

// ---- AC-4 资产邮件 ----

// walletMailRequest 资产邮件入参（单角色 handler 与批量任务**共用同一份语义**）。
type walletMailRequest struct {
	Currency string // gold / cera（由调用方校验，这里不再返回 400）
	Amount   int64
	// Title / Body 可选：留空按币种用默认文案（与重构前的单角色接口逐字一致）。
	Title string
	Body  string
}

// walletMailOutcome 一次资产邮件发放的结果。
type walletMailOutcome struct {
	// Data 直接交给 writeOK 的 data 载荷，形状与重构前的单角色接口逐字一致：
	// gold 是扁平结构（自带 message），cera 是 {"result":..., "message":...}。
	Data map[string]any
	// Credited 钱是否已经真正进了玩家账户（cera 入账成功即 true；gold 要领了邮件才到账，恒 false）。
	Credited bool
	// NotifyFailed 仅 cera：点券已入账、但到账通知邮件失败。
	//
	// ⚠ 批量任务必须把它**单独计数**并且**绝不重试**：钱已经在玩家账上了，
	// 把"发放"整体重试一遍等于再入账一次 —— 那是重复发券，比少一封通知严重得多。
	NotifyFailed bool
	// NotifyError 通知邮件失败原因（写任务 errors / 运维日志用）。
	NotifyError string
}

// walletMailToTarget 给**一个角色**发资产邮件：
//   - gold → GM-2 金币邮件（玩家领取邮件后到账）；
//   - cera → GM-4(add) 入账 + GM-2 到账通知邮件（对齐 GM nativeCurrencyMail，R10）。
//
// 单角色接口（POST /manage/v1/account/wallet/mail）与批量接口（wallet/mail-batch）
// 都只调这一个函数：cera 的两步语义（先入账、再发通知；通知失败时钱也已经到账）
// 只有一处实现才不会两边跑偏，批量才不会把"已到账"重复做一遍。
func (s *Server) walletMailToTarget(ctx context.Context, roleName string, req walletMailRequest) (walletMailOutcome, error) {
	roleName = strings.TrimSpace(roleName)
	title := strings.TrimSpace(req.Title)
	body := strings.TrimSpace(req.Body)

	if req.Currency == "gold" {
		var before gmWalletResult
		if _, err := s.acctGM(ctx, http.MethodPost, "/local/dnf-gm/wallet/query", map[string]any{
			"character_name": roleName,
		}, &before); err != nil {
			return walletMailOutcome{}, err
		}
		if title == "" {
			title = "GM 金币邮件"
		}
		if body == "" {
			body = fmt.Sprintf("系统发放金币 %d，请领取附件。", req.Amount)
		}
		nativeRequest := gmMailRequest{
			CharacterName: roleName,
			Title:         title,
			Body:          body,
			Gold:          req.Amount,
		}
		var mail gmMailResult
		if _, err := s.acctGM(ctx, http.MethodPost, "/local/dnf-gm/mail", nativeRequest, &mail); err != nil {
			return walletMailOutcome{}, err
		}
		message := "金币邮件已发送，玩家领取邮件后到账"
		if mail.Refreshed {
			message = "金币邮件已发送，在线邮箱已实时刷新；领取后到账"
		}
		return walletMailOutcome{
			// 金币是"邮件附件"而不是直接入账：Credited 保持 false（钱还在邮件里）。
			Data: map[string]any{
				"character": before.Character,
				"currency":  "gold", "mode": "mail", "amount": req.Amount,
				"before": before.Gold, "after": before.Gold,
				"cera": before.Cera, "gold": before.Gold,
				"online": mail.Online, "refreshed": mail.Refreshed,
				"delivery": "claimable_mail", "mail": mail,
				"message": message,
			},
		}, nil
	}

	// cera：wallet/update add 入账 + 通知邮件。
	var before gmWalletResult
	if _, err := s.acctGM(ctx, http.MethodPost, "/local/dnf-gm/wallet/query", map[string]any{
		"character_name": roleName,
	}, &before); err != nil {
		return walletMailOutcome{}, err
	}
	var credited gmWalletResult
	if _, err := s.acctGM(ctx, http.MethodPost, "/local/dnf-gm/wallet/update", map[string]any{
		"character_name": roleName, "currency": "cera", "mode": "add", "amount": req.Amount,
	}, &credited); err != nil {
		return walletMailOutcome{}, err
	}
	if title == "" {
		title = "GM 点券到账通知"
	}
	if body == "" {
		body = fmt.Sprintf("系统已发放点券 %d，当前点券余额 %d。", req.Amount, credited.Cera)
	}
	noticeRequest := gmMailRequest{
		CharacterName: roleName,
		Title:         title,
		Body:          body,
	}
	var notice gmMailResult
	_, mailErr := s.acctGM(ctx, http.MethodPost, "/local/dnf-gm/mail", noticeRequest, &notice)
	data := map[string]any{
		"character": credited.Character,
		"currency":  "cera", "mode": "mail", "amount": req.Amount,
		"before": before.Cera, "after": credited.Cera,
		"cera": credited.Cera, "gold": credited.Gold,
		"online":    credited.Online || notice.Online,
		"refreshed": credited.Refreshed && (mailErr == nil && (!notice.Online || notice.Refreshed)),
		"delivery":  "credited_with_notice", "mail": notice,
	}
	if mailErr != nil {
		// 点券已到账但通知失败：必须原样提示请勿重复操作（R10）。
		data["mail_error"] = mailErr.Error()
		return walletMailOutcome{
			Data:         map[string]any{"result": data, "message": "点券已到账，但通知邮件发送失败；请勿重复操作"},
			Credited:     true,
			NotifyFailed: true,
			NotifyError:  mailErr.Error(),
		}, nil
	}
	message := "点券已到账，通知邮件已发送"
	if notice.Refreshed {
		message = "点券已到账，通知邮件与在线邮箱均已实时刷新"
	}
	return walletMailOutcome{
		Data:     map[string]any{"result": data, "message": message},
		Credited: true,
	}, nil
}

// POST /manage/v1/account/wallet/mail
// gold → GM-2 金币邮件；cera → GM-4(add) 入账 + GM-2 到账通知邮件（对齐 GM nativeCurrencyMail，R10）。
//
// 业务实现已抽到 walletMailToTarget（批量接口复用同一份），这里只留"校验 + 审计 + 回显"，
// **对外形状与文案一字未改**（管理工具按这些字段名解析）。
func (s *Server) handleAccountWalletMail(w http.ResponseWriter, r *http.Request) {
	audit := &acctAudit{start: time.Now(), actor: "manage", fields: map[string]any{}}
	defer s.acctAuditEnd(r, audit)
	var req struct {
		RoleName string `json:"role_name"`
		Currency string `json:"currency"`
		Amount   int64  `json:"amount"`
	}
	raw, ok := s.acctDecode(w, r, &req)
	if !ok {
		return
	}
	audit.body = raw
	req.RoleName = strings.TrimSpace(req.RoleName)
	req.Currency = strings.ToLower(strings.TrimSpace(req.Currency))
	// 校验口径保持原样（含"两个币种共用 accountMaxWalletBalance"这条历史写法）：
	// 它是对外行为的一部分，改动它等于悄悄放宽单角色接口能发多少。
	if req.RoleName == "" || (req.Currency != "cera" && req.Currency != "gold") || req.Amount <= 0 || req.Amount > accountMaxWalletBalance {
		writeErr(w, http.StatusBadRequest, "bad-request: 角色、资产类型或数量无效（数量上限 2147483647）")
		return
	}
	audit.fields["role_name"] = req.RoleName
	audit.fields["currency"] = req.Currency
	audit.fields["amount"] = req.Amount

	outcome, err := s.walletMailToTarget(r.Context(), req.RoleName, walletMailRequest{
		Currency: req.Currency,
		Amount:   req.Amount,
	})
	if err != nil {
		s.acctWriteGMError(w, audit, err)
		return
	}
	audit.ok()
	writeOK(w, outcome.Data)
}

// ---- AC-5 物品发放 ----

// POST /manage/v1/account/items/grant
func (s *Server) handleAccountItemsGrant(w http.ResponseWriter, r *http.Request) {
	audit := &acctAudit{start: time.Now(), actor: "manage", fields: map[string]any{}}
	defer s.acctAuditEnd(r, audit)
	var req struct {
		RoleName       string `json:"role_name"`
		ItemID         uint32 `json:"item_id"`
		Amount         uint32 `json:"amount"`
		ReinforceLevel int    `json:"reinforce_level"`
		AmplifyLevel   int    `json:"amplify_level"`
		AmplifyType    int    `json:"amplify_type"`
	}
	raw, ok := s.acctDecode(w, r, &req)
	if !ok {
		return
	}
	audit.body = raw
	req.RoleName = strings.TrimSpace(req.RoleName)
	if req.RoleName == "" {
		writeErr(w, http.StatusBadRequest, "bad-request: 缺少 role_name")
		return
	}
	if req.ItemID == 0 || req.Amount == 0 || req.Amount > pvfindex.MaxItemAmount {
		writeErr(w, http.StatusBadRequest, "bad-request: item_id/amount 必须为正且数量不超上限")
		return
	}
	if req.ReinforceLevel < 0 || req.AmplifyLevel < 0 || req.AmplifyType < 0 {
		writeErr(w, http.StatusBadRequest, "bad-request: 强化/增幅参数不能为负")
		return
	}
	audit.fields["role_name"] = req.RoleName
	audit.fields["item_id"] = req.ItemID
	audit.fields["amount"] = req.Amount
	audit.fields["reinforce_level"] = req.ReinforceLevel
	audit.fields["amplify_level"] = req.AmplifyLevel
	audit.fields["amplify_type"] = req.AmplifyType

	// 执行端校验（设计 §6.5：不存在/强化非法 → pvf-mismatch）。
	def, err := s.acctResolveItemForGrant(r.Context(), req.ItemID, req.ReinforceLevel, req.AmplifyLevel, req.AmplifyType)
	if err != nil {
		s.acctWriteGMError(w, audit, err)
		return
	}
	nativeRequest := gmMailRequest{
		CharacterName: req.RoleName,
		Title:         "GM 物品发放",
		Body:          fmt.Sprintf("系统发放：%s（%d）", def.Name, def.ItemID),
		Attachments: []gmMailAttachment{{
			ItemID: def.ItemID, Count: req.Amount, Bind: false,
			ReinforceLevel: req.ReinforceLevel, AmplifyLevel: req.AmplifyLevel, AmplifyType: req.AmplifyType,
		}},
	}
	var result gmMailResult
	if _, err := s.acctGM(r.Context(), http.MethodPost, "/local/dnf-gm/mail", nativeRequest, &result); err != nil {
		s.acctWriteGMError(w, audit, err)
		return
	}
	audit.ok()
	writeOK(w, map[string]any{
		"item_id":         def.ItemID,
		"item_name":       def.Name,
		"amount":          req.Amount,
		"route":           "系统邮件附件",
		"reinforce_level": req.ReinforceLevel,
		"amplify_level":   req.AmplifyLevel,
		"amplify_type":    req.AmplifyType,
		"mail":            result,
		"message":         acctMailMessage(result),
	})
}

// ---- AC-6 SP/TP ----

// POST /manage/v1/account/character/sptp（只带需修改的字段）
func (s *Server) handleAccountCharacterSPTPSet(w http.ResponseWriter, r *http.Request) {
	audit := &acctAudit{start: time.Now(), actor: "manage", fields: map[string]any{}}
	defer s.acctAuditEnd(r, audit)
	var req struct {
		CharacterID string `json:"character_id"`
		RemainingSP *int   `json:"remaining_sp"`
		RemainingTP *int   `json:"remaining_tp"`
	}
	raw, ok := s.acctDecode(w, r, &req)
	if !ok {
		return
	}
	audit.body = raw
	req.CharacterID = strings.TrimSpace(req.CharacterID)
	if req.CharacterID == "" {
		writeErr(w, http.StatusBadRequest, "bad-request: 缺少 character_id")
		return
	}
	if req.RemainingSP == nil && req.RemainingTP == nil {
		writeErr(w, http.StatusBadRequest, "bad-request: remaining_sp/remaining_tp 至少填写一项")
		return
	}
	audit.fields["character_id"] = req.CharacterID
	if req.RemainingSP != nil {
		audit.fields["remaining_sp"] = *req.RemainingSP
	}
	if req.RemainingTP != nil {
		audit.fields["remaining_tp"] = *req.RemainingTP
	}
	body := map[string]any{"character_id": req.CharacterID}
	if req.RemainingSP != nil {
		body["remaining_sp"] = *req.RemainingSP
	}
	if req.RemainingTP != nil {
		body["remaining_tp"] = *req.RemainingTP
	}
	message, err := s.acctGM(r.Context(), http.MethodPost, "/local/dnf-gm/character/sptp", body, nil)
	if err != nil {
		s.acctWriteGMError(w, audit, err)
		return
	}
	audit.ok()
	writeOK(w, map[string]any{"message": message})
}

// ---- AC-7 等级 ----

// POST /manage/v1/account/character/level
func (s *Server) handleAccountCharacterLevelSet(w http.ResponseWriter, r *http.Request) {
	audit := &acctAudit{start: time.Now(), actor: "manage", fields: map[string]any{}}
	defer s.acctAuditEnd(r, audit)
	var req struct {
		CharacterID string `json:"character_id"`
		Level       int    `json:"level"`
	}
	raw, ok := s.acctDecode(w, r, &req)
	if !ok {
		return
	}
	audit.body = raw
	req.CharacterID = strings.TrimSpace(req.CharacterID)
	if req.CharacterID == "" {
		writeErr(w, http.StatusBadRequest, "bad-request: 缺少 character_id")
		return
	}
	if req.Level < 1 || req.Level > 90 {
		writeErr(w, http.StatusBadRequest, "bad-request: level 必须在 1..90 之间")
		return
	}
	audit.fields["character_id"] = req.CharacterID
	audit.fields["level"] = req.Level
	message, err := s.acctGM(r.Context(), http.MethodPost, "/local/dnf-gm/character/level", map[string]any{
		"character_id": req.CharacterID, "level": req.Level,
	}, nil)
	if err != nil {
		s.acctWriteGMError(w, audit, err)
		return
	}
	audit.ok()
	writeOK(w, map[string]any{"message": message})
}

// ---- AC-8 转职现状 ----

// GET /manage/v1/account/character/job?character_id=
func (s *Server) handleAccountCharacterJobGet(w http.ResponseWriter, r *http.Request) {
	audit := &acctAudit{start: time.Now(), actor: "manage", fields: map[string]any{}}
	defer s.acctAuditEnd(r, audit)
	characterID := strings.TrimSpace(r.URL.Query().Get("character_id"))
	if characterID == "" {
		writeErr(w, http.StatusBadRequest, "bad-request: 缺少 character_id")
		return
	}
	audit.fields["character_id"] = characterID
	var result gmJobInfo
	if _, err := s.acctGM(r.Context(), http.MethodGet, "/local/dnf-gm/character/job?id="+url.QueryEscape(characterID), nil, &result); err != nil {
		s.acctWriteGMError(w, audit, err)
		return
	}
	audit.ok()
	writeOK(w, result)
}

// ---- AC-9 转职设置 ----

// POST /manage/v1/account/character/job（job/first_grow/second_grow 一次性提交，需先 GET 现状）
func (s *Server) handleAccountCharacterJobSet(w http.ResponseWriter, r *http.Request) {
	audit := &acctAudit{start: time.Now(), actor: "manage", fields: map[string]any{}}
	defer s.acctAuditEnd(r, audit)
	var req struct {
		CharacterID string `json:"character_id"`
		Job         int    `json:"job"`
		FirstGrow   int    `json:"first_grow"`
		SecondGrow  int    `json:"second_grow"`
	}
	raw, ok := s.acctDecode(w, r, &req)
	if !ok {
		return
	}
	audit.body = raw
	req.CharacterID = strings.TrimSpace(req.CharacterID)
	if req.CharacterID == "" {
		writeErr(w, http.StatusBadRequest, "bad-request: 缺少 character_id")
		return
	}
	if req.Job < 0 || req.FirstGrow < 0 || req.SecondGrow < 0 {
		writeErr(w, http.StatusBadRequest, "bad-request: job/first_grow/second_grow 不能为负")
		return
	}
	audit.fields["character_id"] = req.CharacterID
	audit.fields["job"] = req.Job
	audit.fields["first_grow"] = req.FirstGrow
	audit.fields["second_grow"] = req.SecondGrow
	message, err := s.acctGM(r.Context(), http.MethodPost, "/local/dnf-gm/character/job", map[string]any{
		"character_id": req.CharacterID, "job": req.Job, "first_grow": req.FirstGrow, "second_grow": req.SecondGrow,
	}, nil)
	if err != nil {
		s.acctWriteGMError(w, audit, err)
		return
	}
	audit.ok()
	writeOK(w, map[string]any{"message": message})
}

// ---- AC-9b 跨大系转职 ----

// POST /manage/v1/account/character/career（跨职业大类，如鬼剑士→魔法师）
//
// ── 为什么单开一个端点而不是让 job 端点自己判断（2026-10-05）──────────────
// 两条路在服务端是**两个不同接口、两套语义**：
//
//	/local/dnf-gm/character/job  只改 grow_type，且 currentJob != request.Job 直接拒绝
//	/dnf/gm/characters/{id}/career  真写 job，重建技能、重算属性、清旧系转职任务
//
// 后者**要求角色离线**，且后果是产品级的（技能重置 / 装扮可能因职业限制失效）。
// 放在同一个端点上"自动选路"会让调用方在不知情的情况下触发一次破坏性操作，
// 所以独立成端点，由界面上的独立按钮显式触发。
//
// 这条路由服务端早就挂好了（internal/app/dnf90/gm_tool_admin.go），只是从没被接过来。
func (s *Server) handleAccountCharacterCareerSet(w http.ResponseWriter, r *http.Request) {
	audit := &acctAudit{start: time.Now(), actor: "manage", fields: map[string]any{}}
	defer s.acctAuditEnd(r, audit)
	var req struct {
		CharacterID string `json:"character_id"`
		Job         int    `json:"job"`
		FirstGrow   int    `json:"first_grow"`
		SecondGrow  int    `json:"second_grow"`
	}
	raw, ok := s.acctDecode(w, r, &req)
	if !ok {
		return
	}
	audit.body = raw
	req.CharacterID = strings.TrimSpace(req.CharacterID)
	if req.CharacterID == "" {
		writeErr(w, http.StatusBadRequest, "bad-request: 缺少 character_id")
		return
	}
	if req.Job < 0 || req.FirstGrow < 0 || req.SecondGrow < 0 {
		writeErr(w, http.StatusBadRequest, "bad-request: job/first_grow/second_grow 不能为负")
		return
	}
	audit.fields["character_id"] = req.CharacterID
	audit.fields["job"] = req.Job
	audit.fields["first_grow"] = req.FirstGrow
	audit.fields["second_grow"] = req.SecondGrow
	// 路径参数 + 服务端要求的字段名（job / first / awakening）。
	path := "/dnf/gm/characters/" + url.PathEscape(req.CharacterID) + "/career"
	message, err := s.acctGM(r.Context(), http.MethodPost, path, map[string]any{
		"job": req.Job, "first": req.FirstGrow, "awakening": req.SecondGrow,
	}, nil)
	if err != nil {
		s.acctWriteGMError(w, audit, err)
		return
	}
	audit.ok()
	writeOK(w, map[string]any{"message": message})
}

// ---- AC-10 副本全部解锁 ----

// POST /manage/v1/account/dungeons/unlock-all（dungeons 表由执行端 dungeons_max_difficulty.json 装载）
func (s *Server) handleAccountDungeonsUnlockAll(w http.ResponseWriter, r *http.Request) {
	audit := &acctAudit{start: time.Now(), actor: "manage", fields: map[string]any{}}
	defer s.acctAuditEnd(r, audit)
	var req struct {
		CharacterID string `json:"character_id"`
	}
	raw, ok := s.acctDecode(w, r, &req)
	if !ok {
		return
	}
	audit.body = raw
	req.CharacterID = strings.TrimSpace(req.CharacterID)
	if req.CharacterID == "" {
		writeErr(w, http.StatusBadRequest, "bad-request: 缺少 character_id")
		return
	}
	audit.fields["character_id"] = req.CharacterID
	if s.bridge == nil || s.bridge.AcctDungeons == nil {
		s.acctWriteGMError(w, audit, errors.New("gm-not-available: 运维端未注入副本解锁表"))
		return
	}
	dungeons, err := s.bridge.AcctDungeons()
	if err != nil {
		s.acctWriteGMError(w, audit, fmt.Errorf("gm-not-available: 载入副本难度表失败: %w", err))
		return
	}
	if dungeons == nil {
		dungeons = map[string]int{}
	}
	var result struct {
		Count int `json:"count"`
	}
	message, err := s.acctGM(r.Context(), http.MethodPost, "/local/dnf-gm/dungeons/unlock-all", map[string]any{
		"character_id": req.CharacterID,
		"dungeons":     dungeons,
	}, &result)
	if err != nil {
		s.acctWriteGMError(w, audit, err)
		return
	}
	audit.ok()
	writeOK(w, map[string]any{"count": result.Count, "message": message})
}

// ---- AC-11 装备槽全部解锁 ----

// POST /manage/v1/account/equip-slots/unlock-all
func (s *Server) handleAccountEquipSlotsUnlockAll(w http.ResponseWriter, r *http.Request) {
	audit := &acctAudit{start: time.Now(), actor: "manage", fields: map[string]any{}}
	defer s.acctAuditEnd(r, audit)
	var req struct {
		CharacterID string `json:"character_id"`
	}
	raw, ok := s.acctDecode(w, r, &req)
	if !ok {
		return
	}
	audit.body = raw
	req.CharacterID = strings.TrimSpace(req.CharacterID)
	if req.CharacterID == "" {
		writeErr(w, http.StatusBadRequest, "bad-request: 缺少 character_id")
		return
	}
	audit.fields["character_id"] = req.CharacterID
	var result map[string]any
	message, err := s.acctGM(r.Context(), http.MethodPost, "/local/dnf-gm/equip-slots/unlock-all", map[string]any{
		"character_id": req.CharacterID,
	}, &result)
	if err != nil {
		s.acctWriteGMError(w, audit, err)
		return
	}
	audit.ok()
	writeOK(w, map[string]any{"ok": true, "message": message})
}

// ---- AC-13 黑钻：只读查询 + 开启/续期（账号级权益，按天） ----

// POST /manage/v1/account/character/black-diamond/query
//
// 只读查询黑钻现状：优先问服务端原生 GM 只读接口（服务端版本较旧时返回 405），
// 失败则直读游戏库（premium_expire_1000 + dnf_character_stats）兜底，
// 与运维工具 GUI 的「查询黑钻」走同一套数据来源，管理工具不会看到两套结果。
func (s *Server) handleAccountBlackDiamondQuery(w http.ResponseWriter, r *http.Request) {
	audit := &acctAudit{start: time.Now(), actor: "manage", fields: map[string]any{}}
	defer s.acctAuditEnd(r, audit)
	var req struct {
		CharacterID string `json:"character_id"`
	}
	raw, ok := s.acctDecode(w, r, &req)
	if !ok {
		return
	}
	audit.body = raw
	req.CharacterID = strings.TrimSpace(req.CharacterID)
	if req.CharacterID == "" {
		writeErr(w, http.StatusBadRequest, "bad-request: 缺少 character_id")
		return
	}
	audit.fields["character_id"] = req.CharacterID

	var result struct {
		CharacterID              string `json:"character_id"`
		CharacterName            string `json:"character_name"`
		AccountID                string `json:"account_id"`
		Active                   bool   `json:"active"`
		ExpiresAt                int64  `json:"expires_at"`
		RemainingSeconds         int64  `json:"remaining_seconds"`
		Fatigue                  int64  `json:"fatigue"`
		FatigueLimit             int64  `json:"fatigue_limit"`
		BlackDiamondFatigueLimit int64  `json:"black_diamond_fatigue_limit"`
		Online                   bool   `json:"online"`
		ServerNow                int64  `json:"server_now"`
	}
	message, gmErr := s.acctGM(r.Context(), http.MethodGet,
		"/local/dnf-gm/character/black-diamond?id="+url.QueryEscape(req.CharacterID), nil, &result)
	source := "server"
	if gmErr != nil {
		// 服务端没有这个只读接口（旧版本）→ 直读游戏库；仍失败才报错。
		st := s.bridgeStore()
		if st == nil {
			s.acctWriteGMError(w, audit, gmErr)
			return
		}
		row, found, dbErr := st.BlackDiamondStatus(r.Context(), req.CharacterID)
		if dbErr != nil {
			s.acctWriteGMError(w, audit, fmt.Errorf("%v; 直读游戏库失败: %w", gmErr, dbErr))
			return
		}
		if !found {
			s.acctWriteGMError(w, audit, fmt.Errorf("role-not-found: 游戏库里没有角色 %s", req.CharacterID))
			return
		}
		audit.ok()
		writeOK(w, map[string]any{
			"character_id":      row.CharacterID,
			"character_name":    row.CharacterName,
			"account_id":        row.AccountID,
			"active":            row.Active,
			"expires_at":        row.ExpiresAt,
			"remaining_seconds": row.RemainingSeconds,
			// 剩余天数：与"服务端实时"分支同口径（管理工具直接展示它，
			// 少了这个字段旧服务端上会显示 0 天，看起来像黑钻已过期）。
			"remaining_days": row.RemainingSeconds / 86400,
			"fatigue":        row.Fatigue,
			"fatigue_limit":  row.FatigueLimit,
			"online":         false,
			"server_now":     time.Now().Unix(),
			"source":         "database",
			"message":        "服务端黑钻只读接口不可用（" + gmErr.Error() + "），已改为直读游戏库",
		})
		return
	}
	audit.ok()
	remaining := result.RemainingSeconds
	if result.ServerNow > 0 && result.ExpiresAt > 0 {
		if remaining = result.ExpiresAt - result.ServerNow; remaining < 0 {
			remaining = 0
		}
	}
	writeOK(w, map[string]any{
		"character_id":                result.CharacterID,
		"character_name":              result.CharacterName,
		"account_id":                  result.AccountID,
		"active":                      result.Active,
		"expires_at":                  result.ExpiresAt,
		"remaining_seconds":           remaining,
		"remaining_days":              remaining / 86400,
		"fatigue":                     result.Fatigue,
		"fatigue_limit":               result.FatigueLimit,
		"black_diamond_fatigue_limit": result.BlackDiamondFatigueLimit,
		"online":                      result.Online,
		"server_now":                  result.ServerNow,
		"source":                      source,
		"message":                     message,
	})
}

// POST /manage/v1/account/character/black-diamond
//
// 黑钻是账号级权益（服务端 premium_expire_1000），同一账号下所有角色共享；
// 服务端按 max(现在, 当前到期时间) + 天数 计算新到期时间，因此重复提交就是续期叠加。
func (s *Server) handleAccountCharacterBlackDiamond(w http.ResponseWriter, r *http.Request) {
	audit := &acctAudit{start: time.Now(), actor: "manage", fields: map[string]any{}}
	defer s.acctAuditEnd(r, audit)
	var req struct {
		CharacterID string `json:"character_id"`
		Days        int    `json:"days"`
	}
	raw, ok := s.acctDecode(w, r, &req)
	if !ok {
		return
	}
	audit.body = raw
	req.CharacterID = strings.TrimSpace(req.CharacterID)
	if req.CharacterID == "" {
		writeErr(w, http.StatusBadRequest, "bad-request: 缺少 character_id")
		return
	}
	if req.Days < 1 || req.Days > accountBlackDiamondMaxDays {
		writeErr(w, http.StatusBadRequest, fmt.Sprintf("bad-request: 黑钻天数（days）必须在 1~%d 之间（按天算）", accountBlackDiamondMaxDays))
		return
	}
	audit.fields["character_id"] = req.CharacterID
	audit.fields["days"] = req.Days
	var result struct {
		CharacterID      string `json:"character_id"`
		AccountID        string `json:"account_id"`
		Days             int    `json:"days"`
		ExpiresAt        int64  `json:"expires_at"`
		RemainingSeconds int64  `json:"remaining_seconds"`
		Fatigue          int64  `json:"fatigue"`
		FatigueLimit     int64  `json:"fatigue_limit"`
		Online           bool   `json:"online"`
		Refreshed        bool   `json:"refreshed"`
		RefreshError     string `json:"refresh_error"`
		Deferred         bool   `json:"deferred"`
	}
	message, err := s.acctGM(r.Context(), http.MethodPost, "/local/dnf-gm/character/black-diamond", map[string]any{
		"character_id": req.CharacterID,
		"days":         req.Days,
	}, &result)
	if err != nil {
		s.acctWriteGMError(w, audit, err)
		return
	}
	audit.ok()
	writeOK(w, map[string]any{
		"character_id":      result.CharacterID,
		"account_id":        result.AccountID,
		"days":              result.Days,
		"expires_at":        result.ExpiresAt,
		"remaining_seconds": result.RemainingSeconds,
		"fatigue":           result.Fatigue,
		"fatigue_limit":     result.FatigueLimit,
		"online":            result.Online,
		"refreshed":         result.Refreshed,
		"refresh_error":     result.RefreshError,
		"deferred":          result.Deferred,
		"message":           message,
	})
}

// ---- AC-14 PVF 索引状态（本地上报，不转发 GM） ----
// POST /manage/v1/account/pvf/index/status
func (s *Server) handleAccountPVFIndexStatus(w http.ResponseWriter, r *http.Request) {
	audit := &acctAudit{start: time.Now(), actor: "manage", fields: map[string]any{}}
	defer s.acctAuditEnd(r, audit)
	var req struct {
		PVFPath string `json:"pvf_path"`
	}
	raw, ok := s.acctDecode(w, r, &req)
	if !ok {
		return
	}
	audit.body = raw
	pvfPath := strings.TrimSpace(req.PVFPath)
	if pvfPath == "" {
		var err error
		pvfPath, err = s.acctInstancePVFPath()
		if err != nil {
			s.acctWriteGMError(w, audit, err)
			return
		}
	}
	info, err := os.Stat(pvfPath)
	if err != nil {
		s.acctWriteGMError(w, audit, fmt.Errorf("pvf-mismatch: 读取 PVF 文件失败: %w", err))
		return
	}
	_ = info // 仅校验文件存在性；索引指纹由 catalog.Stats() 提供
	catalog, err := pvfindex.Ensure(r.Context(), pvfPath, nil)
	if err != nil {
		s.acctWriteGMError(w, audit, fmt.Errorf("pvf-mismatch: 构建索引失败: %w", err))
		return
	}
	stats := catalog.Stats()
	audit.ok()
	audit.fields["pvf_path"] = pvfPath
	writeOK(w, map[string]any{
		"path":     stats.Path,
		"size":     stats.Size,
		"mod_time": stats.ModTime,
		"indexed":  true,
		"entries":  stats.Entries,
	})
}
