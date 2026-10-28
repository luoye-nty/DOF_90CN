// CDK 兑换（/launcher/v1/characters + /launcher/v1/redeem）：
// 登录器用 ticket 换取本账号角色列表，或提交兑换码 + 角色完成核销并发系统邮件。
// 游戏侧 GM 接口仅回环调用 instance.json 配置的 adminListen（头 X-Admin-Token / X-Admin-Actor）。
package server

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"math"
	"net/http"
	"strings"
	"time"
	"unicode/utf8"

	"dnf90ops/internal/gmhttp"
	"dnf90ops/internal/serverdir"
	"dnf90ops/internal/store"
)

// gmAdminTimeout 回环服务端 GM 管理口超时。
const gmAdminTimeout = 8 * time.Second

// gmEnvelope 服务端 /local/dnf-gm/* 统一响应信封。
type gmEnvelope struct {
	OK      bool            `json:"ok"`
	Message string          `json:"message"`
	Error   string          `json:"error"`
	Data    json.RawMessage `json:"data"`
}

// gmMailAttachment 与 go-server dnfbridge.GMSystemMailAttachment 对齐。
type gmMailAttachment struct {
	ItemID uint32 `json:"item_id"`
	Count  uint32 `json:"count"`
	Bind   bool   `json:"bind"`
}

// gmMailRequest 与 go-server dnfbridge.GMSystemMailRequest 对齐（字段 JSON tag 一致）。
type gmMailRequest struct {
	CharacterID string             `json:"character_id,omitempty"`
	Title       string             `json:"title,omitempty"`
	Body        string             `json:"body,omitempty"`
	Attachments []gmMailAttachment `json:"attachments,omitempty"`
}

// redeemReward 兑换码奖励 JSON 元素（redeem_codes.reward_json 反解用）。
//
// 三种奖励形态（同一张 CDK 可以任意组合，也可以只有其中一种）：
//   - 物品：item_id + count（+bind）→ 走系统邮件附件，玩家领取后入包；
//   - 点券：cera → 直接入账号通用余额（GM wallet/update add），没有邮件附件；
//   - 黑钻：black_diamond_days → 按角色定位账号后直接激活（GM character/black-diamond），
//     同样没有邮件附件（黑钻是账号级权益 premium_expire_1000）。
//
// 点券/黑钻为什么不做成邮件：服务端点券与黑钻都是账号级 metadata，邮件附件只有物品；
// 而且这两样走"直接入账 + 到账通知"这条路（与管理工具「发送资产邮件」的 cera 分支一致）。
type redeemReward struct {
	ItemID uint32 `json:"item_id,omitempty"`
	Count  uint32 `json:"count,omitempty"`
	Bind   bool   `json:"bind,omitempty"`
	Cera   int64  `json:"cera,omitempty"`
	// BlackDiamondDays 黑钻天数（1~3650）。多行黑钻会累加后一次性激活；
	// 服务端 GMActivateBlackDiamond 按 max(now, 当前到期时间) + 天数 续期（可叠加）。
	BlackDiamondDays int64 `json:"black_diamond_days,omitempty"`
}

// redeemMaxBlackDiamondDays 单张 CDK 一次发放的黑钻总天数上限。
// 与服务端 GMActivateBlackDiamond 的 1..3650 天校验一致：超过就拒，避免
// 玩家兑换成功却在发奖阶段被服务端打回（等于白扣一次兑换次数）。
const redeemMaxBlackDiamondDays = 3650

// splitRedeemRewards 把 reward_json 拆成"物品邮件附件"、"点券总额"与"黑钻天数"三部分。
// 返回的 items 只含真正能发邮件的行（item_id>0 且 count>0）；点券行累加到 cera，
// 黑钻行累加到 blackDiamondDays。
func splitRedeemRewards(rewardJSON string) (items []gmMailAttachment, cera int64, blackDiamondDays int64, err error) {
	rewardJSON = strings.TrimSpace(rewardJSON)
	if rewardJSON == "" || rewardJSON == "null" {
		return nil, 0, 0, nil
	}
	var rewards []redeemReward
	if err := json.Unmarshal([]byte(rewardJSON), &rewards); err != nil {
		return nil, 0, 0, fmt.Errorf("reward json invalid: %w", err)
	}
	for _, reward := range rewards {
		if reward.Cera > 0 {
			if cera > math.MaxInt32-reward.Cera {
				return nil, 0, 0, errors.New("reward cera exceeds client range")
			}
			cera += reward.Cera
			continue
		}
		if reward.BlackDiamondDays > 0 {
			if blackDiamondDays+reward.BlackDiamondDays > redeemMaxBlackDiamondDays {
				return nil, 0, 0, fmt.Errorf("reward black diamond days must be 1..%d", redeemMaxBlackDiamondDays)
			}
			blackDiamondDays += reward.BlackDiamondDays
			continue
		}
		if reward.ItemID == 0 || reward.Count == 0 {
			continue
		}
		items = append(items, gmMailAttachment{ItemID: reward.ItemID, Count: reward.Count, Bind: reward.Bind})
	}
	return items, cera, blackDiamondDays, nil
}

// gmWalletUpdateRequest 与 go-server dnfbridge.GMWalletRequest 对齐。
type gmWalletUpdateRequest struct {
	CharacterID   string `json:"character_id,omitempty"`
	CharacterName string `json:"character_name,omitempty"`
	Currency      string `json:"currency,omitempty"`
	Mode          string `json:"mode,omitempty"`
	Amount        int64  `json:"amount,omitempty"`
}

// gmWalletUpdateResult 与 go-server dnfbridge.GMWalletResult 对齐（只取需要的字段）。
type gmWalletUpdateResult struct {
	Cera      int64 `json:"cera"`
	Online    bool  `json:"online"`
	Refreshed bool  `json:"refreshed"`
}

// gmBlackDiamondRequest 与 go-server dnfbridge.GMBlackDiamondRequest 对齐。
type gmBlackDiamondRequest struct {
	CharacterID string `json:"character_id,omitempty"`
	Days        int    `json:"days,omitempty"`
}

// gmBlackDiamondResult 与 go-server dnfbridge.GMBlackDiamondResult 对齐（只取需要的字段）。
type gmBlackDiamondResult struct {
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

// blackDiamondExpireText 把黑钻到期时间戳格式化为本地时间文案（0 表示无到期时间）。
func blackDiamondExpireText(expiresAt int64) string {
	if expiresAt <= 0 {
		return "（未知）"
	}
	return time.Unix(expiresAt, 0).Local().Format("2006-01-02 15:04:05")
}

// handleCharacters GET /launcher/v1/characters?ticket=xxx：
// 解析 ticket → launcher account_id → 直查游戏侧 dnf_characters 返回本账号全部角色，
// 响应 {ok, data:[...]}（data 元素含 character_id/name/level，兼容登录器 GMCharacterInfo 视图）。
func (s *Service) handleCharacters(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodGet {
		writeError(w, http.StatusMethodNotAllowed, "method-not-allowed", "GET required")
		return
	}
	ticket := strings.TrimSpace(r.URL.Query().Get("ticket"))
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
		s.log.Error("characters resolve ticket failed", "error", err)
		writeError(w, http.StatusInternalServerError, "internal-error", "query failed")
		return
	}
	characters, err := s.store.ListGameCharactersByAccount(r.Context(), accountID)
	if err != nil {
		s.log.Error("characters load from dnf_characters failed", "account_id", accountID, "error", err)
		writeError(w, http.StatusInternalServerError, "internal-error", "query failed")
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{"ok": true, "data": characters})
}

// redeemDataStore CDK 兑换路径需要的最小数据访问能力。
//
// 为什么单独抽这一层：这条链路的硬约束是"**任何拒绝都必须发生在核销之前**"，
// 而 *store.Store 要真实 MySQL 才起得来，单测起不了库。抽成接口之后，单测可以注入
// 一个只实现这几个方法的替身，直接断言"目标角色不可用时 RedeemCodeTx 一次都没被调用"
// —— 也就是 2026-09-30 线上事故（兑换码被消耗、奖励发给已删除的角色）的复现点。
// 生产路径下 Service.redeemData 恒为 nil，一律回落到 s.store。
type redeemDataStore interface {
	ResolveAccountIDByTicket(ctx context.Context, ticket string) (string, error)
	GetAccountByID(ctx context.Context, accountID string) (*store.Account, error)
	// GameCharacterActive 发放目标权威判据：存在 && 未删除 && 属于该账号。
	GameCharacterActive(ctx context.Context, accountID, characterID string) (store.GameCharacter, bool, error)
	RedeemCodeTx(ctx context.Context, code, accountID, username, characterID, characterName, remoteIP string) (*store.RedeemReceipt, error)
	SetRedemptionMailResult(ctx context.Context, redemptionID int64, status, mailError string) error
}

// redeemStore 返回本 handler 的数据访问入口：未注入替身时就是真实的 store。
//
// 注意 *store.Store 必须由注入方保证非 nil（生产由 New 注入）；
// 这条回退只服务于"正常运行时"，不是给测试用的第二条路。
func (s *Service) redeemStore() redeemDataStore {
	if s.redeemData != nil {
		return s.redeemData
	}
	return s.store
}

// handleRedeem POST /launcher/v1/redeem，body {ticket, code, character_id}。
//
// 流程：ticket → 账号 username → **发放目标校验（权威判据，见 ③）** → 核销事务 →
// 系统邮件/点券/黑钻 → 回写流水状态。
// 顺序即契约：③ 必须在 ② 之前 —— 校验不放行的请求，绝不能消耗兑换码。
func (s *Service) handleRedeem(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		writeError(w, http.StatusMethodNotAllowed, "method-not-allowed", "POST required")
		return
	}
	var req struct {
		Ticket      string `json:"ticket"`
		Code        string `json:"code"`
		CharacterID string `json:"character_id"`
	}
	if err := decodeBody(r, &req); err != nil {
		writeError(w, http.StatusBadRequest, "bad-request", err.Error())
		return
	}
	req.Ticket = strings.TrimSpace(req.Ticket)
	req.Code = strings.TrimSpace(req.Code)
	req.CharacterID = strings.TrimSpace(req.CharacterID)
	if req.Ticket == "" || req.Code == "" || req.CharacterID == "" {
		writeError(w, http.StatusBadRequest, "bad-request", "ticket, code and character_id required")
		return
	}

	// data：本 handler 的全部数据访问入口（生产恒为真实 store，见 redeemDataStore）。
	data := s.redeemStore()

	// ① ticket → account_id + username。
	accountID, err := data.ResolveAccountIDByTicket(r.Context(), req.Ticket)
	if err != nil {
		if errors.Is(err, store.ErrTicketNotFound) {
			writeError(w, http.StatusUnauthorized, "ticket-not-found", "invalid or expired ticket")
			return
		}
		s.log.Error("redeem resolve ticket failed", "error", err)
		writeError(w, http.StatusInternalServerError, "internal-error", "query failed")
		return
	}
	// username 仅用于兑换流水落库（launcher_accounts 为本库现有表，非 GM 回环依赖）。
	username := ""
	if account, err := data.GetAccountByID(r.Context(), accountID); err == nil {
		username = account.Username
	}

	// 兑换码先规范化（非法格式直接拒，不消耗任何资源）。
	//
	// 400 + code-unrecognized 与"码不存在"（下面 404 invalid-code）**刻意分开**：
	// 老版本登录器码框有 MaxLength=24，粘贴「卡号：XXXX-… 」时会被截断，玩家看到的是
	// 一句"兑换码无效、不存在或已停用"，既不知道自己贴错了、也不知道该怎么贴。
	// 这个新提示带中文，登录器 Services\RemoteAuthService.cs 的 ExtractRedeemApiError
	// 对"未知 code + message 含中文"会**原样透出**，所以不需要同步发登录器就能生效。
	normCode, err := store.NormalizeRedeemCode(req.Code)
	if err != nil {
		writeError(w, http.StatusBadRequest, "code-unrecognized",
			"没能从粘贴的内容里识别出兑换码，请只复制 20 位兑换码（例：XXXX-XXXX-XXXX-XXXX-XXXX）")
		return
	}

	// ③ 发放目标校验（核销前，权威判据）：一次查询同时校验"该角色存在 && 未删除 && 属于该账号"。
	//
	// 为什么必须走 GameCharacterActive，而不是"拉本账号角色列表 + 在内存里比对"：
	// 后者曾经漏掉 delete_flag（游戏侧删号是软删除，dnf_characters 里那行还在），
	// 已删除的角色照样通过校验 → 兑换码先被消耗（落流水 + redeemed_count+1）→
	// 奖励发给一张服务端已经不认的角色（GM 邮件/钱包/黑钻全部返回 character not found）
	// → 玩家手里只剩一次被烧掉的兑换（2026-09-30 线上事故）。
	// 顺序同样重要：这一段必须在下面的 RedeemCodeTx **之前**，任何拒绝都不能发生在核销之后。
	target, ok, err := data.GameCharacterActive(r.Context(), accountID, req.CharacterID)
	if err != nil {
		s.log.Error("redeem load character target failed",
			"account_id", accountID, "character_id", req.CharacterID, "error", err)
		writeError(w, http.StatusInternalServerError, "internal-error", "query failed")
		return
	}
	if !ok {
		// code 沿用 character-not-owned（403），不新造 code：外网版登录器
		// Services\RemoteAuthService.cs 的 ExtractRedeemApiError 已经把
		// character-not-owned 映射成「所选角色不属于当前账号，请重新选择」，
		// 新造 code 登录器不认识，玩家只会看到一句泛化错误。
		// 修掉角色列表过滤后，已删除的角色不会再出现在选择列表里，这条提示只在
		// "列表是旧的 / 有人手工构造请求"时才会命中，措辞可以接受；
		// 真实原因（不存在 / 已删除 / 不属于本账号）写进 message 与服务端日志，便于运营核对。
		s.log.Warn("redeem rejected: character is not a usable grant target",
			"account_id", accountID, "character_id", req.CharacterID)
		writeError(w, http.StatusForbidden, "character-not-owned",
			"角色不存在或已被删除（或不属于当前账号），请重新选择角色后重试")
		return
	}

	// ② 核销事务：唯一键防同账号重复兑换，行锁 + 条件计数防超发。
	// 走到这里说明发放目标已经通过权威判据 —— 兑换码一旦消耗就必须能发出奖励。
	receipt, err := data.RedeemCodeTx(r.Context(), normCode, accountID, username, target.CharacterID, target.Name, clientIP(r))
	if err != nil {
		switch {
		case errors.Is(err, store.ErrRedeemCodeNotFound):
			writeError(w, http.StatusNotFound, "invalid-code", "redeem code not found or disabled")
		case errors.Is(err, store.ErrRedeemAlreadyRedeemed):
			writeError(w, http.StatusConflict, "already-redeemed", "this account already redeemed this code")
		case errors.Is(err, store.ErrRedeemExhausted):
			writeError(w, http.StatusConflict, "exhausted", "redeem code has been exhausted")
		default:
			s.log.Error("redeem transaction failed", "error", err)
			writeError(w, http.StatusInternalServerError, "internal-error", "redeem failed")
		}
		return
	}

	// ④ 发奖：物品走系统邮件（一封一个附件），点券走账号余额入账，黑钻走账号权益激活。
	// 一个物品发一封邮件：服务端 GM mail 每封只落第一个附件，多物品必须逐封发送。
	items, ceraTotal, bdDays, planErr := splitRedeemRewards(receipt.RewardJSON)
	if planErr != nil {
		s.log.Error("redeem reward json invalid", "redemption_id", receipt.RedemptionID, "error", planErr)
		_ = data.SetRedemptionMailResult(r.Context(), receipt.RedemptionID, "failed", truncateErr(planErr))
		writeError(w, http.StatusBadGateway, "invalid-reward", "reward config is invalid, please contact admin")
		return
	}
	if len(items) == 0 && ceraTotal <= 0 && bdDays <= 0 {
		err := errors.New("reward is empty (no items, no cera and no black diamond)")
		s.log.Error("redeem reward empty", "redemption_id", receipt.RedemptionID, "code", normCode)
		_ = data.SetRedemptionMailResult(r.Context(), receipt.RedemptionID, "failed", truncateErr(err))
		writeError(w, http.StatusBadGateway, "empty-reward", "reward is empty, please contact admin")
		return
	}

	var itemErr error
	sentItems := 0
	for _, att := range items {
		if err := s.gmSendMail(r.Context(), gmMailRequest{
			CharacterID: target.CharacterID,
			Title:       "CDK兑换奖励",
			Body:        "兑换成功：" + receipt.Name,
			Attachments: []gmMailAttachment{att},
		}); err != nil {
			itemErr = err
			break
		}
		sentItems++
	}

	// 点券：add 入账（在线/离线都能入；在线会顺带刷新客户端钱包显示）。
	var ceraErr error
	var ceraAfter int64
	if ceraTotal > 0 {
		var credited gmWalletUpdateResult
		if err := s.gmWalletAddCera(r.Context(), target.CharacterID, ceraTotal, &credited); err != nil {
			ceraErr = err
		} else {
			ceraAfter = credited.Cera
			// 到账通知邮件：尽力而为，失败不算兑换失败（点券已经入账了）。
			if err := s.gmSendMail(r.Context(), gmMailRequest{
				CharacterID: target.CharacterID,
				Title:       "CDK点券到账通知",
				Body:        fmt.Sprintf("兑换成功：%s\n点券已到账 +%d，当前余额 %d。", receipt.Name, ceraTotal, ceraAfter),
			}); err != nil {
				s.log.Warn("redeem cera notice mail failed", "redemption_id", receipt.RedemptionID, "error", err)
			}
		}
	}

	// 黑钻：按角色定位所属账号后直接激活（账号级权益，同一账号下所有角色共享）。
	// 在线角色会顺带下发原生黑钻包（城内安全场景立即生效，否则推迟到回城）；
	// 离线角色则在下一次登录时由 select-character 读取权益。
	var bdErr error
	var bdGranted gmBlackDiamondResult
	if bdDays > 0 {
		if err := s.gmActivateBlackDiamond(r.Context(), target.CharacterID, bdDays, &bdGranted); err != nil {
			bdErr = err
		} else if err := s.gmSendMail(r.Context(), gmMailRequest{
			CharacterID: target.CharacterID,
			Title:       "CDK黑钻到账通知",
			Body: fmt.Sprintf("兑换成功：%s\n黑钻 +%d 天已到账，到期时间 %s（账号共享，当前疲劳上限 %d）。",
				receipt.Name, bdDays, blackDiamondExpireText(bdGranted.ExpiresAt), bdGranted.FatigueLimit),
		}); err != nil {
			s.log.Warn("redeem black diamond notice mail failed", "redemption_id", receipt.RedemptionID, "error", err)
		}
	}

	// ⑤ 回写流水状态：全部成功 sent；只成功一部分 partial（已到账的部分不能当失败，
	//    否则玩家以为全失败会去重复兑换）；全都失败才是 failed。
	requestedParts := 0
	if len(items) > 0 {
		requestedParts++
	}
	if ceraTotal > 0 {
		requestedParts++
	}
	if bdDays > 0 {
		requestedParts++
	}
	failedParts := make([]string, 0, 3)
	if itemErr != nil {
		failedParts = append(failedParts, "物品邮件: "+itemErr.Error())
	}
	if ceraErr != nil {
		failedParts = append(failedParts, "点券: "+ceraErr.Error())
	}
	if bdErr != nil {
		failedParts = append(failedParts, "黑钻: "+bdErr.Error())
	}
	switch {
	case len(failedParts) == 0:
		_ = data.SetRedemptionMailResult(r.Context(), receipt.RedemptionID, "sent", "")
	case len(failedParts) >= requestedParts:
		combined := errors.New(strings.Join(failedParts, "; "))
		s.log.Error("redeem grant failed", "redemption_id", receipt.RedemptionID, "error", combined)
		_ = data.SetRedemptionMailResult(r.Context(), receipt.RedemptionID, "failed", truncateErr(combined))
		writeError(w, http.StatusBadGateway, "grant-failed", "reward delivery failed, please contact admin")
		return
	default:
		partial := errors.New(strings.Join(failedParts, "; "))
		s.log.Warn("redeem grant partially failed", "redemption_id", receipt.RedemptionID, "failed_parts", strings.Join(failedParts, " | "))
		_ = data.SetRedemptionMailResult(r.Context(), receipt.RedemptionID, "partial", truncateErr(partial))
	}

	// ⑥ 响应（partial 也返回 200：玩家确实拿到了东西，不能让他以为全失败又去重复兑换）。
	message := "兑换成功：" + receipt.Name
	if ceraTotal > 0 {
		if ceraErr == nil {
			message = fmt.Sprintf("%s（点券 +%d，当前余额 %d）", message, ceraTotal, ceraAfter)
		} else {
			message = fmt.Sprintf("%s（点券 %d 发放失败，请联系管理员）", message, ceraTotal)
		}
	}
	if bdDays > 0 {
		if bdErr == nil {
			message = fmt.Sprintf("%s（黑钻 +%d 天，到期 %s）", message, bdDays, blackDiamondExpireText(bdGranted.ExpiresAt))
		} else {
			message = fmt.Sprintf("%s（黑钻 %d 天发放失败，请联系管理员）", message, bdDays)
		}
	}
	if len(items) > 0 {
		message = fmt.Sprintf("%s（物品 %d 件已发到邮箱）", message, sentItems)
	}
	if itemErr != nil && sentItems == 0 {
		message = fmt.Sprintf("%s（物品邮件发送失败，请联系管理员）", message)
	}
	writeJSON(w, http.StatusOK, map[string]any{
		"code":    "ok",
		"name":    receipt.Name,
		"message": message,
	})
}

// gmActivateBlackDiamond 回环 POST /local/dnf-gm/character/black-diamond，给角色所属账号
// 叠加 days 天黑钻。服务端按 max(now, 当前到期时间) + 天数 续期，在线角色顺带刷新原生黑钻包。
func (s *Service) gmActivateBlackDiamond(ctx context.Context, characterID string, days int64, out *gmBlackDiamondResult) error {
	characterID = strings.TrimSpace(characterID)
	if characterID == "" {
		return errors.New("black diamond requires a character")
	}
	if days < 1 || days > redeemMaxBlackDiamondDays {
		return fmt.Errorf("black diamond days must be 1..%d", redeemMaxBlackDiamondDays)
	}
	baseURL, token, err := s.gmAdminConfig()
	if err != nil {
		return err
	}
	payload, err := json.Marshal(gmBlackDiamondRequest{CharacterID: characterID, Days: int(days)})
	if err != nil {
		return err
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, baseURL+"/local/dnf-gm/character/black-diamond", bytes.NewReader(payload))
	if err != nil {
		return err
	}
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("X-Admin-Token", token)
	req.Header.Set("X-Admin-Actor", "ops")
	// 写操作补危险操作确认头（服务端 RequireConfirmation=true 时校验，见 internal/gmhttp）。
	gmhttp.SetConfirm(req, "ops")

	env, err := s.doGmRequest(req)
	if err != nil {
		return err
	}
	if !env.OK {
		msg := env.Error
		if msg == "" {
			msg = env.Message
		}
		if msg == "" {
			msg = "gm admin rejected black diamond activation"
		}
		return errors.New(msg)
	}
	if out != nil && len(env.Data) > 0 {
		// 回显解析失败不影响发放事实（服务端已经返回 ok），只是少一个到期时间回显。
		if err := json.Unmarshal(env.Data, out); err != nil {
			s.log.Warn("redeem black diamond response decode failed", "error", err)
		}
	}
	return nil
}

// gmWalletAddCera 回环 POST /local/dnf-gm/wallet/update，给角色所属账号的点券余额加 amount。
// 服务端该接口在线/离线都可用；在线时会顺带刷新客户端钱包显示。
func (s *Service) gmWalletAddCera(ctx context.Context, characterID string, amount int64, out *gmWalletUpdateResult) error {
	if amount <= 0 {
		return nil
	}
	baseURL, token, err := s.gmAdminConfig()
	if err != nil {
		return err
	}
	payload, err := json.Marshal(gmWalletUpdateRequest{
		CharacterID: characterID,
		Currency:    "cera",
		Mode:        "add",
		Amount:      amount,
	})
	if err != nil {
		return err
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, baseURL+"/local/dnf-gm/wallet/update", bytes.NewReader(payload))
	if err != nil {
		return err
	}
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("X-Admin-Token", token)
	req.Header.Set("X-Admin-Actor", "ops")
	// 写操作补危险操作确认头（服务端 RequireConfirmation=true 时校验，见 internal/gmhttp）。
	gmhttp.SetConfirm(req, "ops")

	env, err := s.doGmRequest(req)
	if err != nil {
		return err
	}
	if !env.OK {
		msg := env.Error
		if msg == "" {
			msg = env.Message
		}
		if msg == "" {
			msg = "gm admin rejected wallet update"
		}
		return errors.New(msg)
	}
	if out != nil && len(env.Data) > 0 {
		// 钱包响应解析失败不影响入账事实（服务端已经返回 ok），只是少一个余额回显。
		if err := json.Unmarshal(env.Data, out); err != nil {
			s.log.Warn("redeem wallet response decode failed", "error", err)
		}
	}
	return nil
}

// gmSendMail 回环 POST /local/dnf-gm/mail 发系统邮件。
func (s *Service) gmSendMail(ctx context.Context, payload gmMailRequest) error {
	baseURL, token, err := s.gmAdminConfig()
	if err != nil {
		return err
	}
	body, err := json.Marshal(payload)
	if err != nil {
		return err
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, baseURL+"/local/dnf-gm/mail", bytes.NewReader(body))
	if err != nil {
		return err
	}
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("X-Admin-Token", token)
	req.Header.Set("X-Admin-Actor", "ops")
	// 写操作补危险操作确认头（服务端 RequireConfirmation=true 时校验，见 internal/gmhttp）。
	gmhttp.SetConfirm(req, "ops")

	env, err := s.doGmRequest(req)
	if err != nil {
		return err
	}
	if !env.OK {
		msg := env.Error
		if msg == "" {
			msg = env.Message
		}
		if msg == "" {
			msg = "gm admin rejected mail"
		}
		return errors.New(msg)
	}
	return nil
}

// doGmRequest 执行回环请求并解码统一信封；非 2xx 视为不可达/拒绝。
func (s *Service) doGmRequest(req *http.Request) (*gmEnvelope, error) {
	client := &http.Client{Timeout: gmAdminTimeout}
	resp, err := client.Do(req)
	if err != nil {
		return nil, fmt.Errorf("gm admin request failed: %w", err)
	}
	defer resp.Body.Close()
	if resp.StatusCode < 200 || resp.StatusCode >= 300 {
		return nil, fmt.Errorf("gm admin http status %d", resp.StatusCode)
	}
	var env gmEnvelope
	body, err := io.ReadAll(io.LimitReader(resp.Body, 1<<20))
	if err != nil {
		return nil, fmt.Errorf("read gm admin response: %w", err)
	}
	if err := json.Unmarshal(body, &env); err != nil {
		return nil, fmt.Errorf("parse gm admin response: %w", err)
	}
	return &env, nil
}

// gmAdminConfig 由 config.ServerRoot 探测 instance.json，返回 http://adminListen 与 adminToken。
func (s *Service) gmAdminConfig() (baseURL, token string, err error) {
	root := strings.TrimSpace(s.cfg.ServerRoot)
	ins := serverdir.Inspect(root)
	if ins == nil || ins.Instance == nil || strings.TrimSpace(ins.Instance.AdminListen) == "" {
		return "", "", fmt.Errorf("instance.json 未配置 adminListen，无法连接服务端管理口")
	}
	token = strings.TrimSpace(ins.Instance.AdminToken)
	if token == "" {
		return "", "", fmt.Errorf("instance.json 未配置 adminToken，请先在服务端初始化")
	}
	return "http://" + ins.Instance.AdminListen, token, nil
}

// buildMailAttachments 解析 reward_json 文本 → GM 邮件附件数组；空/非法 JSON 返回空附件。
// 点券行与黑钻行不进附件（它们不走邮件），实现直接复用 splitRedeemRewards，避免两套解析漂移。
func (s *Service) buildMailAttachments(rewardJSON string) []gmMailAttachment {
	items, _, _, err := splitRedeemRewards(rewardJSON)
	if err != nil {
		s.log.Warn("redeem reward json invalid", "reward_json", rewardJSON)
		return nil
	}
	return items
}

// truncateErr 把错误文案裁到 mail_error 列（VARCHAR(255)）放得下的长度。
//
// 注意：255 是**字节**上限判断，而错误文案里常混中文（一条错误就三字节），
// 直接 msg[:255] 可能把多字节字符切成两半，写库时会报
// Incorrect string value 导致整条状态回写失败（流水卡在 pending）。
// 所以按 UTF-8 边界往前退。
func truncateErr(err error) string {
	msg := err.Error()
	if len(msg) <= 255 {
		return msg
	}
	cut := 255
	for cut > 0 && !utf8.RuneStart(msg[cut]) {
		cut--
	}
	return msg[:cut]
}
