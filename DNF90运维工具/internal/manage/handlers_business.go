//go:build paid

// P1 业务端点：封禁 / CDK / 服务端启停。所有逻辑均经 Bridge 复用主程序
// 既有能力（store/ban.go、store/redeem.go、ban_tab.go kick、cdk_tab.go 补发邮件、
// main.go onStart/onStop 核心链路），manage 包内不重复实现。
package manage

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"math"
	"net/http"
	"strconv"
	"strings"
	"time"

	"dnf90ops/internal/store"
)

// ---------- GET /manage/v1/bans：封禁记录列表 ----------

func (s *Server) handleBanList(w http.ResponseWriter, r *http.Request) {
	st := s.bridgeStore()
	if st == nil {
		writeErr(w, http.StatusServiceUnavailable, "auth-service-not-running")
		return
	}
	limit, _ := strconv.Atoi(r.URL.Query().Get("limit"))
	recs, err := st.ListBans(r.Context(), limit)
	if err != nil {
		writeErr(w, http.StatusInternalServerError, "db-error: "+err.Error())
		return
	}
	out := make([]banOut, 0, len(recs))
	for i := range recs {
		out = append(out, banRecordOut(recs[i]))
	}
	writeOK(w, map[string]any{"items": out, "total": len(out)})
}

// ---------- POST /manage/v1/bans：封禁 ----------

type banCreateRequest struct {
	BanType  string `json:"ban_type"`  // account | ip | machine | triple
	BanValue string `json:"ban_value"` // 直封精确值；account/triple 可留空改走 username 解析
	Username string `json:"username"`  // account/triple 必填：用于解析 account_id / 最近 IP / 机器码
	Reason   string `json:"reason"`
	Days     int    `json:"days"` // <=0 永久
}

func (s *Server) handleBanCreate(w http.ResponseWriter, r *http.Request) {
	if s.bridge == nil || s.bridge.BanNow == nil {
		writeErr(w, http.StatusServiceUnavailable, "ban-hook-unavailable")
		return
	}
	var req banCreateRequest
	if err := decodeJSON(w, r, &req); err != nil {
		writeErr(w, http.StatusBadRequest, "bad-request")
		return
	}
	bt := strings.TrimSpace(req.BanType)
	value := strings.TrimSpace(req.BanValue)
	username := strings.TrimSpace(req.Username)
	reason := strings.TrimSpace(req.Reason)
	switch bt {
	case store.BanAccount, store.BanIP, store.BanMachine:
		if bt == store.BanAccount && value == "" && username == "" {
			writeErr(w, http.StatusBadRequest, "ban_value-or-username-required")
			return
		}
		if bt != store.BanAccount && value == "" {
			writeErr(w, http.StatusBadRequest, "ban_value-required")
			return
		}
	case "triple":
		if value == "" && username == "" {
			writeErr(w, http.StatusBadRequest, "username-required-for-triple")
			return
		}
	default:
		writeErr(w, http.StatusBadRequest, "invalid-ban-type")
		return
	}
	res, err := s.bridge.BanNow(r.Context(), bt, value, username, reason, req.Days)
	if err != nil {
		// 目标不存在是"输入问题"，不是服务端故障：返回 404 而不是 500，
		// 否则管理工具只会弹「(HTTP 500)」，运营会以为服务端崩了。
		if errors.Is(err, store.ErrUserNotFound) {
			writeErr(w, http.StatusNotFound, "ban-failed: "+err.Error())
			return
		}
		writeErr(w, http.StatusInternalServerError, "ban-failed: "+err.Error())
		return
	}
	writeOK(w, res)
}

// ---------- DELETE /manage/v1/bans：解封 ----------

type banDeleteRequest struct {
	BanType  string `json:"ban_type"`
	BanValue string `json:"ban_value"`
}

func (s *Server) handleBanDelete(w http.ResponseWriter, r *http.Request) {
	st := s.bridgeStore()
	if st == nil {
		writeErr(w, http.StatusServiceUnavailable, "auth-service-not-running")
		return
	}
	var req banDeleteRequest
	if err := decodeJSON(w, r, &req); err != nil {
		writeErr(w, http.StatusBadRequest, "bad-request")
		return
	}
	bt := strings.TrimSpace(req.BanType)
	bv := strings.TrimSpace(req.BanValue)
	if !validBanType(bt) || bv == "" {
		writeErr(w, http.StatusBadRequest, "invalid-ban-target")
		return
	}

	// IP / 机器码：保持"按值精确匹配"，与修复前完全一致。
	if bt != store.BanAccount {
		ok, err := st.Unban(r.Context(), bt, bv)
		if err != nil {
			writeErr(w, http.StatusInternalServerError, "db-error: "+err.Error())
			return
		}
		if !ok {
			writeErr(w, http.StatusNotFound, "no-active-ban-found")
			return
		}
		writeOK(w, map[string]any{"unbanned": true})
		return
	}

	// 账号型解封（2026-09-24 修）：
	//
	// 此前只按 ban_value 精确匹配，而账号型封禁写入的 ban_value 是 account_id
	// （dnf:<sha256hex>），运营照着封禁列表里的「用户名」填就必然得到
	// `no-active-ban-found (HTTP 404)`，看起来就像"解封坏了"。
	// 封禁侧一直有"用户名→account_id"解析（ban_resolve.go / manageBanNow），
	// 解封侧缺失属于两侧语义不对称：封得进、解不出。
	//
	// 现在：值填用户名或 dnf:<账号ID> 都认；账号那条解掉的同时，把它名下
	// （username 关联 + 最近登录 IP/机器码）仍生效的 IP/机器码封禁一并解除，
	// 与「一键三封」对称 —— 否则运营解了账号，玩家换账号从同一台机器照样进。
	ctx := r.Context()
	var in accountUnbanInput
	in.TypedValue = bv
	target, terr := st.AccountBanTargets(ctx, bv)
	switch {
	case terr == nil:
		in.AccountID, in.Username = target.AccountID, target.Username
		in.LastIP, in.MachineID = target.LastIP, target.MachineID
		related, rerr := st.ListActiveIPMachineBansByUsername(ctx, target.Username)
		if rerr != nil {
			writeErr(w, http.StatusInternalServerError, "db-error: "+rerr.Error())
			return
		}
		in.Related = related
	case errors.Is(terr, store.ErrUserNotFound):
		// 不是账号库里的账号（例如老库迁移来的 dnf:<md5> 直封，本地账号表没有它）：
		// 保留原值做精确匹配，行为与修复前一致。
	default:
		writeErr(w, http.StatusInternalServerError, "db-error: "+terr.Error())
		return
	}

	plan := planAccountUnban(in)
	lifted, err := st.UnbanTargets(ctx, plan.Targets)
	if err != nil {
		writeErr(w, http.StatusInternalServerError, "db-error: "+err.Error())
		return
	}
	if len(lifted) == 0 {
		status, code, msg := accountUnbanMiss(ctx, st, plan, bv)
		writeErr(w, status, code+": "+msg)
		return
	}
	writeOK(w, map[string]any{
		"unbanned":   true,
		"account_id": plan.AccountID,
		"username":   plan.Username,
		"lifted":     liftedBansOut(lifted),
	})
}

// accountUnbanInput 账号型解封的解析结果与关联封禁（纯数据，便于单测）。
type accountUnbanInput struct {
	TypedValue string            // 管理工具「解封」区填的「值」
	AccountID  string            // 解析到的 account_id；空 = 账号库里没有这个账号
	Username   string            // 解析到的用户名
	LastIP     string            // 账号最近登录 IP
	MachineID  string            // 账号最近机器码
	Related    []store.BanRecord // 该账号名下仍生效的 IP/机器码封禁
}

// unbanPlan 本次要解封的目标集合。
type unbanPlan struct {
	AccountID string
	Username  string
	Targets   []store.BanTarget
	Resolved  bool // 是否解析到真实账号；false = 只能按填写值精确匹配
}

// planAccountUnban 决定账号型解封动哪几条记录（纯函数，便于单测）：
//  1. 账号那条：解析到的 account_id；解析不到就退回按填写原值精确匹配
//     （兼容历史库里以用户名直存 ban_value 的行）；
//  2. 该账号名下（username 关联）仍生效的 IP/机器码封禁；
//  3. 该账号最近登录 IP / 机器码对应的封禁 —— 「一键三封」写的就是这两条。
//
// 目标自动去重、空值跳过，顺序固定（账号 → 关联 → 最近IP → 最近机器码），
// 保证同一输入永远产生同一批目标。
func planAccountUnban(in accountUnbanInput) unbanPlan {
	acctValue := strings.TrimSpace(in.AccountID)
	if acctValue == "" {
		return unbanPlan{
			Targets:  []store.BanTarget{{BanType: store.BanAccount, BanValue: strings.TrimSpace(in.TypedValue)}},
			Resolved: false,
		}
	}
	plan := unbanPlan{
		AccountID: acctValue,
		Username:  strings.TrimSpace(in.Username),
		Resolved:  true,
	}
	seen := make(map[store.BanTarget]struct{}, 4)
	add := func(bt, bv string) {
		bt, bv = strings.TrimSpace(bt), strings.TrimSpace(bv)
		if !validBanType(bt) || bv == "" {
			return
		}
		key := store.BanTarget{BanType: bt, BanValue: bv}
		if _, dup := seen[key]; dup {
			return
		}
		seen[key] = struct{}{}
		plan.Targets = append(plan.Targets, key)
	}

	add(store.BanAccount, acctValue)
	for _, rec := range in.Related {
		if rec.BanType != store.BanIP && rec.BanType != store.BanMachine {
			continue
		}
		add(rec.BanType, rec.BanValue)
	}
	add(store.BanIP, in.LastIP)
	add(store.BanMachine, in.MachineID)
	return plan
}

// accountUnbanMiss 一条都没解到时的准确原因。
// 修复前三种情况都只回 no-active-ban-found(404)：账号根本不存在、没有封禁记录、
// 早就解封过 —— 运营只能反复试，看不出该改哪里。现在分开回文案（状态码仍是 404，
// 管理工具对 404 只弹原文，不改它的分支逻辑）。
func accountUnbanMiss(ctx context.Context, st *store.Store, plan unbanPlan, typed string) (int, string, string) {
	who := plan.Username
	if who == "" {
		who = plan.AccountID
	}
	if !plan.Resolved {
		return http.StatusNotFound, "account-not-found",
			fmt.Sprintf("账号 %s 不存在（可填用户名，或封禁列表里的 dnf: 账号ID）", typed)
	}
	exists, revoked, err := st.BanLookup(ctx, store.BanAccount, plan.AccountID)
	if err != nil {
		return http.StatusInternalServerError, "db-error", err.Error()
	}
	switch {
	case !exists:
		return http.StatusNotFound, "no-ban-record", fmt.Sprintf("账号 %s 没有封禁记录", who)
	case revoked:
		return http.StatusNotFound, "already-unbanned", fmt.Sprintf("账号 %s 的封禁已解除过，无需重复解封", who)
	default:
		// 记录在、也没解封过，却没能置为"已解封"：并发下已被别的会话解掉了。
		return http.StatusNotFound, "no-active-ban-found", fmt.Sprintf("账号 %s 当前没有生效中的封禁", who)
	}
}

// liftedBansOut 把"本次真正解除了哪几条"按列表接口同一套字段回给调用方。
func liftedBansOut(recs []store.BanRecord) []banOut {
	out := make([]banOut, 0, len(recs))
	for i := range recs {
		out = append(out, banRecordOut(recs[i]))
	}
	return out
}

// ---------- POST /manage/v1/bans/kick：踢下线 ----------

type banKickRequest struct {
	BanType  string `json:"ban_type"`
	BanValue string `json:"ban_value"`
}

func (s *Server) handleBanKick(w http.ResponseWriter, r *http.Request) {
	if s.bridge == nil || s.bridge.BanKick == nil {
		writeErr(w, http.StatusServiceUnavailable, "kick-hook-unavailable")
		return
	}
	var req banKickRequest
	if err := decodeJSON(w, r, &req); err != nil {
		writeErr(w, http.StatusBadRequest, "bad-request")
		return
	}
	kicked, err := s.bridge.BanKick(r.Context(), strings.TrimSpace(req.BanType), strings.TrimSpace(req.BanValue))
	if err != nil {
		writeErr(w, http.StatusInternalServerError, "kick-failed: "+err.Error())
		return
	}
	writeOK(w, map[string]any{"kicked": kicked})
}

// ---------- GET /manage/v1/redeem-codes：CDK 列表（可选携带流水） ----------

func (s *Server) handleRedeemList(w http.ResponseWriter, r *http.Request) {
	st := s.bridgeStore()
	if st == nil {
		writeErr(w, http.StatusServiceUnavailable, "auth-service-not-running")
		return
	}
	q := r.URL.Query()
	page, _ := strconv.Atoi(q.Get("page"))
	if page <= 0 {
		page = 1
	}
	pageSize, _ := strconv.Atoi(q.Get("page_size"))
	if pageSize <= 0 {
		pageSize = 100
	}
	codes, err := st.ListRedeemCodes(r.Context(), q.Get("keyword"), page, pageSize)
	if err != nil {
		writeErr(w, http.StatusInternalServerError, "db-error: "+err.Error())
		return
	}
	out := make([]redeemCodeOut, 0, len(codes))
	for i := range codes {
		out = append(out, redeemCodeOutOf(codes[i]))
	}
	data := map[string]any{"items": out, "total": len(out)}
	// redemptions=1 且指定了 code 时附带该码流水（默认不返回避免体积膨胀）。
	if q.Get("redemptions") == "1" && strings.TrimSpace(q.Get("code")) != "" {
		reds, err := st.ListRedeemRedemptions(r.Context(), q.Get("code"), 200)
		if err != nil {
			writeErr(w, http.StatusInternalServerError, "db-error: "+err.Error())
			return
		}
		redOut := make([]redeemRedemptionOut, 0, len(reds))
		for i := range reds {
			redOut = append(redOut, redemptionOutOf(reds[i]))
		}
		data["redemptions"] = redOut
	}
	writeOK(w, data)
}

// ---------- POST /manage/v1/redeem-codes：批量生成 ----------

type redeemCreateRequest struct {
	Name       string `json:"name"`
	Count      int    `json:"count"`
	MaxRedeems int    `json:"max_redeems"` // 0=无限
	ContentRaw string `json:"content_raw"`
	RewardJSON string `json:"reward_json"`
	Remark     string `json:"remark"`
}

func (s *Server) handleRedeemCreate(w http.ResponseWriter, r *http.Request) {
	if s.bridge == nil || s.bridge.RedeemCreate == nil {
		writeErr(w, http.StatusServiceUnavailable, "redeem-hook-unavailable")
		return
	}
	var req redeemCreateRequest
	if err := decodeJSON(w, r, &req); err != nil {
		writeErr(w, http.StatusBadRequest, "bad-request")
		return
	}
	if strings.TrimSpace(req.Name) == "" || req.Count <= 0 || req.Count > 1000 {
		writeErr(w, http.StatusBadRequest, "name/count-required(1-1000)")
		return
	}
	if strings.TrimSpace(req.ContentRaw) == "" {
		writeErr(w, http.StatusBadRequest, "content_raw-required")
		return
	}
	// reward_json 是兑换时真正发奖的依据，必须在这里挡住"存进去但发不出来"的配置：
	// 至少有一条可发奖励（物品 item_id+count 或点券 cera），点券不能超出服务端钱包上限。
	if err := validateRedeemRewardJSON(req.RewardJSON); err != nil {
		writeErr(w, http.StatusBadRequest, "bad-request: "+err.Error())
		return
	}
	ctx, cancel := context.WithTimeout(r.Context(), 30*time.Second)
	defer cancel()
	createdBy := "manage-api"
	code, err := s.bridge.RedeemCreate(ctx, req.Name, req.Count, req.MaxRedeems, req.ContentRaw, req.RewardJSON, req.Remark, createdBy)
	if err != nil {
		writeErr(w, http.StatusInternalServerError, "create-failed: "+err.Error())
		return
	}
	// 生成成功后自动触发导出记录（与 GUI onCdkGen 行为对齐，由 Bridge 决定是否执行）。
	if s.bridge.RedeemExported != nil {
		s.bridge.RedeemExported(ctx, req.Name, code)
	}
	writeOK(w, map[string]any{"name": req.Name, "count": len(code), "codes": code})
}

// redeemRewardEntry 兑换奖励元素（与 internal/server.splitRedeemRewards 同一套 schema）。
type redeemRewardEntry struct {
	ItemID uint32 `json:"item_id,omitempty"`
	Count  uint32 `json:"count,omitempty"`
	Bind   bool   `json:"bind,omitempty"`
	Cera   int64  `json:"cera,omitempty"`
	// BlackDiamondDays 黑钻天数（1~3650；多行按天数累加后一次性激活）。
	BlackDiamondDays int64 `json:"black_diamond_days,omitempty"`
}

// redeemMaxBlackDiamondDays 与 internal/server.redeemMaxBlackDiamondDays、
// 服务端 GMActivateBlackDiamond 的 1..3650 天校验保持一致：
// 超过就挡在生成端，别让玩家兑完了才在发奖阶段被打回（白扣一次兑换次数）。
const redeemMaxBlackDiamondDays = 3650

// validateRedeemRewardJSON 校验 reward_json：空表示"没有奖励"，直接拒；
// 非空时必须能解析出至少一条可发奖励（物品/点券/黑钻），且点券总额在服务端钱包上限内、
// 黑钻总天数在服务端单次激活上限内。
func validateRedeemRewardJSON(raw string) error {
	raw = strings.TrimSpace(raw)
	if raw == "" || raw == "null" {
		return errors.New("reward_json 不能为空（至少一行物品、点券或黑钻）")
	}
	var rewards []redeemRewardEntry
	if err := json.Unmarshal([]byte(raw), &rewards); err != nil {
		return errors.New("reward_json 不是合法 JSON 数组")
	}
	deliverable := 0
	var ceraTotal int64
	var bdTotal int64
	for _, reward := range rewards {
		if reward.Cera > 0 {
			if ceraTotal > math.MaxInt32-reward.Cera {
				return errors.New("reward_json 的点券总额超出上限 2147483647")
			}
			ceraTotal += reward.Cera
			deliverable++
			continue
		}
		if reward.BlackDiamondDays > 0 {
			bdTotal += reward.BlackDiamondDays
			if bdTotal > redeemMaxBlackDiamondDays {
				return errors.New("reward_json 的黑钻总天数超出上限 3650 天（按天算）")
			}
			deliverable++
			continue
		}
		if reward.ItemID > 0 && reward.Count > 0 {
			deliverable++
		}
	}
	if deliverable == 0 {
		return errors.New("reward_json 里没有任何可发放的奖励（物品需要 item_id>0 且 count>0，点券需要 cera>0，黑钻需要 black_diamond_days>0）")
	}
	return nil
}

// ---------- DELETE /manage/v1/redeem-codes/{code}：作废 ----------

func (s *Server) handleRedeemDisable(w http.ResponseWriter, r *http.Request) {
	st := s.bridgeStore()
	if st == nil {
		writeErr(w, http.StatusServiceUnavailable, "auth-service-not-running")
		return
	}
	code := r.PathValue("code")
	if err := st.DisableRedeemCode(r.Context(), code); err != nil {
		switch {
		case err == store.ErrRedeemCodeNotFound:
			writeErr(w, http.StatusNotFound, err.Error())
		case err == store.ErrRedeemInvalidInput:
			writeErr(w, http.StatusBadRequest, err.Error())
		default:
			writeErr(w, http.StatusInternalServerError, "db-error: "+err.Error())
		}
		return
	}
	writeOK(w, map[string]any{"disabled": true, "code": code})
}

// ---------- POST /manage/v1/redeem-codes/retry-mail：失败邮件补发 ----------

type retryMailRequest struct {
	Code string `json:"code"` // 空=全量补发
}

func (s *Server) handleRedeemRetryMail(w http.ResponseWriter, r *http.Request) {
	if s.bridge == nil || s.bridge.RetryRedeemMails == nil {
		writeErr(w, http.StatusServiceUnavailable, "retry-mail-hook-unavailable")
		return
	}
	var req retryMailRequest
	if err := decodeJSON(w, r, &req); err != nil {
		writeErr(w, http.StatusBadRequest, "bad-request")
		return
	}
	ctx, cancel := context.WithTimeout(r.Context(), 60*time.Second)
	defer cancel()
	sent, failed, err := s.bridge.RetryRedeemMails(ctx, strings.TrimSpace(req.Code))
	if err != nil {
		writeErr(w, http.StatusInternalServerError, "retry-failed: "+err.Error())
		return
	}
	writeOK(w, map[string]any{"sent": sent, "failed": failed})
}

// ---------- POST /manage/v1/server/start、stop ----------

func (s *Server) handleServerStart(w http.ResponseWriter, r *http.Request) {
	if s.bridge == nil || s.bridge.StartServer == nil {
		writeErr(w, http.StatusServiceUnavailable, "start-hook-unavailable")
		return
	}
	if err := s.bridge.StartServer(r.Context()); err != nil {
		writeErr(w, http.StatusInternalServerError, "start-failed: "+err.Error())
		return
	}
	writeOK(w, map[string]any{"started": true})
}

func (s *Server) handleServerStop(w http.ResponseWriter, r *http.Request) {
	if s.bridge == nil || s.bridge.StopServer == nil {
		writeErr(w, http.StatusServiceUnavailable, "stop-hook-unavailable")
		return
	}
	if err := s.bridge.StopServer(r.Context()); err != nil {
		writeErr(w, http.StatusInternalServerError, "stop-failed: "+err.Error())
		return
	}
	writeOK(w, map[string]any{"stopped": true})
}

// ---------- 输出转换 ----------

type banOut struct {
	ID        int64  `json:"id"`
	BanType   string `json:"ban_type"`
	BanValue  string `json:"ban_value"`
	Username  string `json:"username"`
	Reason    string `json:"reason"`
	Operator  string `json:"operator"`
	BannedAt  string `json:"banned_at"`
	ExpiresAt string `json:"expires_at"`
	RevokedAt string `json:"revoked_at"`
	State     string `json:"state"`
}

func banRecordOut(b store.BanRecord) banOut {
	now := time.Now()
	return banOut{
		ID:        b.ID,
		BanType:   b.BanType,
		BanValue:  b.BanValue,
		Username:  b.Username,
		Reason:    b.Reason,
		Operator:  b.Operator,
		BannedAt:  fmtTime(b.BannedAt),
		ExpiresAt: fmtNullTime(b.ExpiresAt),
		RevokedAt: fmtNullTime(b.RevokedAt),
		State:     b.StateLabel(now),
	}
}

type redeemCodeOut struct {
	ID            int64  `json:"id"`
	RedeemCode    string `json:"redeem_code"`
	Name          string `json:"name"`
	ContentRaw    string `json:"content_raw"`
	MaxRedeems    int    `json:"max_redeems"`
	RedeemedCount int    `json:"redeemed_count"`
	RewardJSON    string `json:"reward_json"`
	Enabled       bool   `json:"enabled"`
	Remark        string `json:"remark"`
	CreatedBy     string `json:"created_by"`
	CreatedAt     string `json:"created_at"`
}

func redeemCodeOutOf(c store.RedeemCode) redeemCodeOut {
	return redeemCodeOut{
		ID:            c.ID,
		RedeemCode:    c.RedeemCode,
		Name:          c.Name,
		ContentRaw:    c.ContentRaw,
		MaxRedeems:    c.MaxRedeems,
		RedeemedCount: c.RedeemedCount,
		RewardJSON:    c.RewardJSON,
		Enabled:       c.Enabled,
		Remark:        c.Remark,
		CreatedBy:     c.CreatedBy,
		CreatedAt:     fmtTime(c.CreatedAt),
	}
}

type redeemRedemptionOut struct {
	ID            int64  `json:"id"`
	RedeemCode    string `json:"redeem_code"`
	AccountID     string `json:"account_id"`
	Username      string `json:"username"`
	CharacterID   string `json:"character_id"`
	CharacterName string `json:"character_name"`
	MailStatus    string `json:"mail_status"`
	MailError     string `json:"mail_error"`
	RemoteIP      string `json:"remote_ip"`
	RedeemedAt    string `json:"redeemed_at"`
}

func redemptionOutOf(r store.RedeemRedemption) redeemRedemptionOut {
	return redeemRedemptionOut{
		ID:            r.ID,
		RedeemCode:    r.RedeemCode,
		AccountID:     r.AccountID,
		Username:      r.Username,
		CharacterID:   r.CharacterID,
		CharacterName: r.CharacterName,
		MailStatus:    r.MailStatus,
		MailError:     r.MailError,
		RemoteIP:      r.RemoteIP,
		RedeemedAt:    fmtTime(r.RedeemedAt),
	}
}

func fmtTime(t time.Time) string {
	if t.IsZero() {
		return ""
	}
	return t.Format(time.RFC3339)
}

func fmtNullTime(t sql.NullTime) string {
	if !t.Valid {
		return ""
	}
	return fmtTime(t.Time)
}

// validBanType 判断是否为合法封禁维度（与 store/ban.go 常量一致）。
func validBanType(t string) bool {
	return t == store.BanAccount || t == store.BanIP || t == store.BanMachine
}
