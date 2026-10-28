package main

// DNF90 账号管理扩展 —— GM Tool 通道封装（背包 / 仓库 / 邮箱）。
//
// 为什么需要第二条 GM 通道：
//
//	account_gm.go 走的是 /local/dnf-gm/*（旧原生接口，信封 {ok,message,error,data}），
//	它只覆盖 角色查询/系统邮件/钱包/SPTP/等级/转职/副本/装备槽；
//	背包与仓库的查询和删除、邮箱列表与定向删除只存在于 /dnf/gm/*（GM Tool 接口，
//	信封 {success,data,error}，由 go-server internal/app/dnf90/gm_tool_admin.go 注册）。
//	两者共用同一个回环 adminListen + adminToken，但鉴权包装不同，所以必须分开实现。
//
// 鉴权差异（go-server internal/platform/httpx/admin.go）：
//   - 读端点用 env.AdminAuth 包装 → 只要 X-Admin-Token。
//   - 写端点用 env.AdminDangerous 包装 → 除 token 外还强制要求
//     X-Admin-Idempotency-Key 非空（否则 428 Precondition Required）；
//     当配置 admin.dangerous_confirm=true 时还要求 X-Admin-Confirm 等于操作 ID。
//     本文件对所有写操作统一带上这两个头，配置怎么变都不会 428。
//   - X-Admin-Confirm 必须等于服务端注册时用的 operation 常量（含 {character_id}
//     这类字面占位符），不是替换成真实 ID 的路径，故下面用 op 常量单独记录。
//
// 在线约束：go-server 的 GMDeleteInventoryItem / GMClearInventory / GMDeleteMailboxMails
// 第一句就是在线判定，角色在线时返回 ErrGMCharacterOnline（HTTP 409）。因此：
//   - 背包/仓库 删除：需要角色离线；工具按服务端返回如实提示，不假装成功。
//   - 邮箱清空：改走 /local/dnf-gm/mail/clear（GMClearMailbox），它支持在线并推送
//     客户端邮箱刷新，见 acctMailboxClearAll。

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"strings"
	"time"

	"dnf90ops/internal/config"
)

const (
	// acctGMTTimeout GM Tool 调用超时。背包/邮箱快照要连带解析 PVF 物品名，
	// 大背包（背包+仓库+账号金库+装备）实测比旧接口慢，给到 20s 留余量。
	acctGMTTimeout = 20 * time.Second
	// acctGMTMaxMailIDs 服务端 GMDeleteMailboxMails 的硬上限（1..100）。
	acctGMTMaxMailIDs = 100
)

// GM Tool 操作 ID：必须与服务端 gm_tool_admin.go 注册时传入 write() 的常量逐字相同。
const (
	acctGMTOpInventoryGet    = "GET /dnf/gm/characters/{character_id}/inventory"
	acctGMTOpInventoryDelete = "POST /dnf/gm/characters/{character_id}/inventory/delete"
	acctGMTOpInventoryClear  = "POST /dnf/gm/characters/{character_id}/inventory/clear"
	acctGMTOpMailboxGet      = "GET /dnf/gm/characters/{character_id}/mailbox"
	acctGMTOpMailboxDelete   = "POST /dnf/gm/characters/{character_id}/mailbox/delete"
	acctGMTOpMailboxClear    = "POST /local/dnf-gm/mail/clear"
	acctGMTOpChannelReload   = "POST /dnf/gm/channels/reload"
)

// acctGMTChannelReload 频道热重载结果（对齐 dnfbridge.GMChannelReloadResult）。
//
// 热重载 = 服务端重新读取 channel_info.etc，把新的频道目录与客户端频道脚本换进内存，
// 并把游戏端口监听对账到新表——**不需要重启 DNF90Server**。
// 生效范围有限：新连入的登录/频道会话立刻拿到新表；已停在选频道界面的玩家要重开列表
// 或重新登录（客户端会本地缓存目录，服务端不主动推未请求的目录包）。
type acctGMTChannelReload struct {
	Path             string    `json:"path"`
	TotalChannels    int       `json:"total_channels"`
	AddedChannels    []int     `json:"added_channels"`
	RemovedChannels  []int     `json:"removed_channels"`
	GamePortsOpened  []int     `json:"game_ports_opened"`
	GamePortsClosed  []int     `json:"game_ports_closed"`
	OnlineCharacters int       `json:"online_characters"`
	Warnings         []string  `json:"warnings,omitempty"`
	ReloadedAt       time.Time `json:"reloaded_at"`
}

// acctGMTHTTPClient GM Tool 通道专用客户端。
// 不能复用 account_gm.go 的 acctNativeHTTPClient：那个客户端整体超时是 12s（对齐旧 GM 接口），
// 会在本通道 20s 的上下文之前先掐断请求，让 acctGMTTimeout 形同虚设。
var acctGMTHTTPClient = &http.Client{
	Timeout: acctGMTTimeout,
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

// acctGMTEnvelope /dnf/gm/* 统一响应信封（服务端 gmAPIResponse）。
type acctGMTEnvelope struct {
	Success bool            `json:"success"`
	Data    json.RawMessage `json:"data"`
	Error   string          `json:"error"`
}

// acctGMTCharacter 与 GMCharacterSummary 对齐的常用字段子集。
type acctGMTCharacter struct {
	CharacterID string `json:"character_id"`
	AccountID   string `json:"account_id"`
	Slot        int    `json:"slot"`
	Name        string `json:"name"`
	Level       int    `json:"level"`
	Gold        int64  `json:"gold"`
	Online      bool   `json:"online"`
}

// acctGMTInventoryItem 与 GMInventoryItem 一一对应。
type acctGMTInventoryItem struct {
	Container string            `json:"container"`
	Key       string            `json:"key"`
	ListType  int               `json:"list_type"`
	Slot      int               `json:"slot"`
	ItemID    int64             `json:"item_id"`
	Name      string            `json:"name"`
	Count     int64             `json:"count"`
	Bind      bool              `json:"bind"`
	ExpireAt  time.Time         `json:"expire_at,omitempty"`
	ReadOnly  bool              `json:"read_only"`
	Extra     map[string]string `json:"extra,omitempty"`
}

// acctGMTInventorySnapshot 与 GMInventorySnapshot 一一对应。
type acctGMTInventorySnapshot struct {
	Character acctGMTCharacter       `json:"character"`
	Items     []acctGMTInventoryItem `json:"items"`
}

// acctGMTMailAttachment 与 GMMailAttachmentSummary 一一对应。
type acctGMTMailAttachment struct {
	ItemID   int64     `json:"item_id"`
	Count    int64     `json:"count"`
	Bind     bool      `json:"bind"`
	ExpireAt time.Time `json:"expire_at,omitempty"`
}

// acctGMTMail 与 GMMailSummary 一一对应。
type acctGMTMail struct {
	MailID      string                  `json:"mail_id"`
	SenderName  string                  `json:"sender_name"`
	Title       string                  `json:"title"`
	Body        string                  `json:"body"`
	Gold        int64                   `json:"gold"`
	Attachments []acctGMTMailAttachment `json:"attachments"`
	Read        bool                    `json:"read"`
	Claimed     bool                    `json:"claimed"`
	Claimable   bool                    `json:"claimable"`
	Saved       bool                    `json:"saved"`
	CreatedAt   time.Time               `json:"created_at,omitempty"`
	ExpireAt    time.Time               `json:"expire_at,omitempty"`
}

// acctGMTMailboxSnapshot 与 GMMailboxSnapshot 一一对应。
type acctGMTMailboxSnapshot struct {
	Character acctGMTCharacter `json:"character"`
	Mails     []acctGMTMail    `json:"mails"`
}

// acctGMTMailDeleteResult 与 GMMailDeleteResult 一一对应。
type acctGMTMailDeleteResult struct {
	Deleted int                    `json:"deleted"`
	Mailbox acctGMTMailboxSnapshot `json:"mailbox"`
}

// ================= 低层调用 =================

// acctGMTDo 调 /dnf/gm/*（GM Tool 信封）。
// operation 非空时作为 X-Admin-Confirm 发出；method 会改状态时补 X-Admin-Idempotency-Key。
// 审计落 audit/account_gmtool.jsonl，字段与 account_gm.go 的 acctGMDo 保持一致便于对比排查。
func (a *App) acctGMTDo(ctx context.Context, method, path, operation string, body, out any) error {
	baseURL, token, err := a.acctGMConfig()
	if err != nil {
		return err
	}
	if ctx == nil {
		ctx = context.Background()
	}
	callCtx, cancel := context.WithTimeout(ctx, acctGMTTimeout)
	defer cancel()

	start := time.Now()
	callErr := acctGMTCallRaw(callCtx, baseURL, token, method, path, operation, body, out, "ops")

	if a != nil && a.cfg != nil {
		gmErr := ""
		if callErr != nil {
			gmErr = callErr.Error()
		}
		a.acctAuditAppend("account_gmtool.jsonl", map[string]any{
			"ts":         time.Now().Format(time.RFC3339Nano),
			"actor":      "ops",
			"method":     method,
			"path":       path,
			"operation":  operation,
			"gm_ok":      callErr == nil,
			"gm_error":   gmErr,
			"elapsed_ms": time.Since(start).Milliseconds(),
		})
	}
	return callErr
}

// acctGMTCallRaw 无审计的底层 HTTP 转发（manage 端点复用：审计由 handlers 负责）。
// actor 为空时按 "ops"（运维直连页）处理；manage 转发固定传 "manage"。
func acctGMTCallRaw(ctx context.Context, baseURL, token, method, path, operation string, body, out any, actor string) error {
	if strings.TrimSpace(actor) == "" {
		actor = "ops"
	}
	var reader io.Reader
	if body != nil {
		raw, err := json.Marshal(body)
		if err != nil {
			return err
		}
		reader = bytes.NewReader(raw)
	}
	request, err := http.NewRequestWithContext(ctx, method, baseURL+path, reader)
	if err != nil {
		return err
	}
	request.Header.Set("Accept", "application/json")
	request.Header.Set("X-Admin-Token", token)
	request.Header.Set("X-Admin-Actor", actor)
	if body != nil {
		request.Header.Set("Content-Type", "application/json")
	}
	mutating := method != http.MethodGet && method != http.MethodHead
	if mutating {
		// 危险操作包装器强制要求幂等键非空；确认头在 dangerous_confirm=false 时被忽略。
		if operation != "" {
			request.Header.Set("X-Admin-Confirm", operation)
		}
		request.Header.Set("X-Admin-Idempotency-Key", acctGMTNewIdempotencyKey())
	}

	response, err := acctGMTHTTPClient.Do(request)
	if err != nil {
		return fmt.Errorf("连接 DNF90Server GM Tool 接口: %w", err)
	}
	defer response.Body.Close()

	var envelope acctGMTEnvelope
	if err := json.NewDecoder(io.LimitReader(response.Body, 8<<20)).Decode(&envelope); err != nil {
		// 老版本服务端没有 /dnf/gm/* 路由：Go 的 ServeMux 会回 404 纯文本，
		// 这里解不出 JSON，直接换成可执行的提示，避免用户看到 "invalid character '<'"。
		if response.StatusCode == http.StatusNotFound {
			return fmt.Errorf("当前运行的 DNF90Server 没有 GM Tool 接口（%s 返回 404），请替换为新版服务端后重启", path)
		}
		return fmt.Errorf("解析 DNF90Server GM Tool 响应: %w", err)
	}
	if response.StatusCode < 200 || response.StatusCode >= 300 || !envelope.Success {
		message := strings.TrimSpace(envelope.Error)
		if message == "" {
			message = response.Status
		}
		return fmt.Errorf("DNF90Server GM Tool 接口: %s", acctGMTHumanizeError(response.StatusCode, message))
	}
	if out != nil && len(envelope.Data) > 0 && string(envelope.Data) != "null" {
		if err := json.Unmarshal(envelope.Data, out); err != nil {
			return fmt.Errorf("解析 DNF90Server GM Tool 数据: %w", err)
		}
	}
	return nil
}

// acctGMTHumanizeError 把服务端的 ASCII 错误码换成人话，保留原文附在括号里方便对日志。
func acctGMTHumanizeError(status int, message string) string {
	lower := strings.ToLower(message)
	switch {
	case strings.Contains(lower, "character is online") || status == http.StatusConflict:
		return message + "（角色在线：背包/仓库/邮箱定向删除要求角色先下线；邮箱清空可在角色在线时用）"
	case strings.Contains(lower, "character not found") || status == http.StatusNotFound:
		return message + "（角色不存在或尚未落库）"
	case strings.Contains(lower, "read-only") || strings.Contains(lower, "unsupported"):
		return message + "（该容器只读：装备栏请用「卸下/删除装备」入口）"
	}
	return message
}

// acctGMTNewIdempotencyKey 生成一次性的幂等键。
// 服务端只把它写进审计回执（admincmd.Receipt），不做去重，所以每次调用用新值即可；
// 但**必须非空**——AdminDangerous 包装器对改状态的方法强制要求该头，否则 428。
func acctGMTNewIdempotencyKey() string {
	nonce, err := config.RandomHex(8)
	if err != nil || nonce == "" {
		nonce = fmt.Sprintf("%d", time.Now().UnixNano())
	}
	return "ops-" + nonce
}

// ================= 频道 =================

// acctGMTReloadChannels 请求服务端热重载频道表（不需要重启 DNF90Server）。
//
// 调用前提：channel_info.etc 已经改好了（运维侧由 internal/channelstore 落盘）。
// 服务端只负责"重新读文件 + 换内存表 + 对账游戏端口"，文件格式与保护规则仍然只在工具侧，
// 这样两边不会各写一套解析器。
func (a *App) acctGMTReloadChannels(ctx context.Context) (acctGMTChannelReload, error) {
	var result acctGMTChannelReload
	if err := a.acctGMTDo(ctx, http.MethodPost, "/dnf/gm/channels/reload", acctGMTOpChannelReload, nil, &result); err != nil {
		return acctGMTChannelReload{}, err
	}
	return result, nil
}

// ================= 背包 / 仓库 =================

// acctGMTInventory 读取角色完整库存快照（背包+仓库+账号金库+装备栏一次返回）。
func (a *App) acctGMTInventory(ctx context.Context, characterID string) (acctGMTInventorySnapshot, error) {
	var snapshot acctGMTInventorySnapshot
	path := "/dnf/gm/characters/" + url.PathEscape(strings.TrimSpace(characterID)) + "/inventory"
	if err := a.acctGMTDo(ctx, http.MethodGet, path, acctGMTOpInventoryGet, nil, &snapshot); err != nil {
		return acctGMTInventorySnapshot{}, err
	}
	return snapshot, nil
}

// acctGMTInventoryDelete 删除某容器里指定格子的一件/一堆物品。
// count<=0 表示整格删除（服务端 gmRemoveStack 的语义）；count>0 时按数量扣减。
func (a *App) acctGMTInventoryDelete(ctx context.Context, characterID, container, key string, count int64) (acctGMTInventorySnapshot, error) {
	characterID = strings.TrimSpace(characterID)
	container = strings.ToLower(strings.TrimSpace(container))
	key = strings.TrimSpace(key)
	if characterID == "" || container == "" || key == "" {
		return acctGMTInventorySnapshot{}, fmt.Errorf("角色、容器与格子键都不能为空")
	}
	if count < 0 {
		return acctGMTInventorySnapshot{}, fmt.Errorf("删除数量不能为负")
	}
	body := map[string]any{"container": container, "key": key, "count": count}
	var snapshot acctGMTInventorySnapshot
	path := "/dnf/gm/characters/" + url.PathEscape(characterID) + "/inventory/delete"
	if err := a.acctGMTDo(ctx, http.MethodPost, path, acctGMTOpInventoryDelete, body, &snapshot); err != nil {
		return acctGMTInventorySnapshot{}, err
	}
	return snapshot, nil
}

// acctGMTInventoryClear 清空整个容器（不可撤销）。
func (a *App) acctGMTInventoryClear(ctx context.Context, characterID, container string) (acctGMTInventorySnapshot, error) {
	characterID = strings.TrimSpace(characterID)
	container = strings.ToLower(strings.TrimSpace(container))
	if characterID == "" || container == "" {
		return acctGMTInventorySnapshot{}, fmt.Errorf("角色与容器都不能为空")
	}
	body := map[string]any{"container": container}
	var snapshot acctGMTInventorySnapshot
	path := "/dnf/gm/characters/" + url.PathEscape(characterID) + "/inventory/clear"
	if err := a.acctGMTDo(ctx, http.MethodPost, path, acctGMTOpInventoryClear, body, &snapshot); err != nil {
		return acctGMTInventorySnapshot{}, err
	}
	return snapshot, nil
}

// ================= 邮箱 =================

// acctGMTMailbox 读取角色完整邮箱（含正文与附件摘要）。
func (a *App) acctGMTMailbox(ctx context.Context, characterID string) (acctGMTMailboxSnapshot, error) {
	var snapshot acctGMTMailboxSnapshot
	path := "/dnf/gm/characters/" + url.PathEscape(strings.TrimSpace(characterID)) + "/mailbox"
	if err := a.acctGMTDo(ctx, http.MethodGet, path, acctGMTOpMailboxGet, nil, &snapshot); err != nil {
		return acctGMTMailboxSnapshot{}, err
	}
	return snapshot, nil
}

// acctGMTMailboxDelete 定向删除最多 100 封邮件（要求角色离线）。
// force=true 时连未领取/带附件的邮件一起删；false 时服务端只允许删可安全删除的邮件。
func (a *App) acctGMTMailboxDelete(ctx context.Context, characterID string, mailIDs []string, force bool) (acctGMTMailDeleteResult, error) {
	characterID = strings.TrimSpace(characterID)
	if characterID == "" {
		return acctGMTMailDeleteResult{}, fmt.Errorf("角色不能为空")
	}
	cleaned := make([]string, 0, len(mailIDs))
	seen := make(map[string]struct{}, len(mailIDs))
	for _, raw := range mailIDs {
		id := strings.TrimSpace(raw)
		if id == "" {
			continue
		}
		if _, dup := seen[id]; dup {
			continue
		}
		seen[id] = struct{}{}
		cleaned = append(cleaned, id)
	}
	if len(cleaned) == 0 {
		return acctGMTMailDeleteResult{}, fmt.Errorf("请先选择要删除的邮件")
	}
	if len(cleaned) > acctGMTMaxMailIDs {
		return acctGMTMailDeleteResult{}, fmt.Errorf("单次最多删除 %d 封邮件（当前 %d 封），请分批操作", acctGMTMaxMailIDs, len(cleaned))
	}
	body := map[string]any{"mail_ids": cleaned, "force": force}
	var result acctGMTMailDeleteResult
	path := "/dnf/gm/characters/" + url.PathEscape(characterID) + "/mailbox/delete"
	if err := a.acctGMTDo(ctx, http.MethodPost, path, acctGMTOpMailboxDelete, body, &result); err != nil {
		return acctGMTMailDeleteResult{}, err
	}
	return result, nil
}

// acctMailboxClearResult /local/dnf-gm/mail/clear 的返回体（GMMailboxClearResult）。
type acctMailboxClearResult struct {
	CharacterID        string `json:"character_id"`
	CharacterName      string `json:"character_name"`
	RemovedMails       int    `json:"removed_mails"`
	RemovedUnread      int    `json:"removed_unread"`
	RemovedClaimable   int    `json:"removed_claimable"`
	RemovedGold        int64  `json:"removed_gold"`
	RemovedAttachments int    `json:"removed_attachments"`
	Online             bool   `json:"online"`
	Refreshed          bool   `json:"refreshed"`
	RefreshError       string `json:"refresh_error,omitempty"`
}

// acctMailboxClearAll 清空角色整个邮箱。
// 走旧原生接口 /local/dnf-gm/mail/clear（GMClearMailbox）：它允许角色在线，
// 并会向在线客户端推送未读提示 + 完整邮箱列表，玩家那边立刻看到邮箱空了。
// 这是在线角色唯一能用的邮箱删除通道（/dnf/gm/* 的定向删除要求离线）。
func (a *App) acctMailboxClearAll(ctx context.Context, characterID, characterName string) (acctMailboxClearResult, string, error) {
	characterID = strings.TrimSpace(characterID)
	characterName = strings.TrimSpace(characterName)
	if characterID == "" && characterName == "" {
		return acctMailboxClearResult{}, "", fmt.Errorf("需要角色 ID 或角色名")
	}
	// 服务端 GMMailboxClearRequest 用 DisallowUnknownFields，空字段必须省略。
	body := map[string]any{"confirm": true}
	if characterID != "" {
		body["character_id"] = characterID
	}
	if characterName != "" {
		body["character_name"] = characterName
	}
	var result acctMailboxClearResult
	message, err := a.acctGMCall(ctx, http.MethodPost, "/local/dnf-gm/mail/clear", body, &result, "ops")
	if err != nil {
		return acctMailboxClearResult{}, "", err
	}
	return result, message, nil
}
