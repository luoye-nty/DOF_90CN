package dnfbridge

import (
	"context"
	"errors"
	"strconv"
	"strings"
	"time"

	dnfrepo "longheng.io/server/internal/modules/dnf/repository"
)

// 商城「赠送」的业务规则（2026-09-27 由用户确认的口径）：
//
//	1. 双方必须**互为好友满 7 天**；
//	2. 赠送方账号注册满 7 天；
//	3. 双方必须在同一个大区（跨区不能赠送）；
//	4. 时装/礼包类：双方**基础职业**必须相同。
//
// 这些规则分别落在两个包上，因为客户端自己也分两步：
//
//	op136（确认收件人姓名 → 查收件人）  规则 1、3 + 自身/查无此人
//	op64 （点「赠送」→ 真正下单）        规则 2、4 + 商品可赠送性
//
// 客户端为这两步各准备了一套失败文案（见 cera_shop_gift_query.go 的结果码表），
// 所以服务端只要回**它认得的码**，玩家就能看到「成为好友7天后才能赠送礼物」
// 这类明确提示，而不是静默或者一句无关的报错。

const (
	// currentCeraShopGiftFriendMinimumAge 是「互为好友满 7 天」的阈值。
	currentCeraShopGiftFriendMinimumAge = 7 * 24 * time.Hour
	// currentCeraShopGiftAccountMinimumAge 是「赠送方账号注册满 7 天」的阈值。
	currentCeraShopGiftAccountMinimumAge = 7 * 24 * time.Hour
)

var (
	errCurrentCeraShopGiftSenderUnavailable = errors.New("dnf cera shop gift sender is unavailable")
	errCurrentCeraShopGiftSelf              = errors.New("dnf cera shop gift recipient is the sender")
	errCurrentCeraShopGiftCrossRegion       = errors.New("dnf cera shop gift recipient is in another server group")
	errCurrentCeraShopGiftNotFriend         = errors.New("dnf cera shop gift recipient is not a mutual friend")
	errCurrentCeraShopGiftFriendTooNew      = errors.New("dnf cera shop gift friendship is younger than the minimum age")
	errCurrentCeraShopGiftAccountTooNew     = errors.New("dnf cera shop gift sender account is younger than the minimum age")
	errCurrentCeraShopGiftJobMismatch       = errors.New("dnf cera shop gift job does not match the recipient")
)

// currentCeraShopGiftSender 是赠送方（= 付款方）在规则判定里需要的投影。
type currentCeraShopGiftSender struct {
	characterID string
	name        string
	job         byte
	hasJob      bool
	serverGroup int64
	accountID   string
	accountAge  time.Duration
	hasAccount  bool
	// giftDailyRemaining 是今天还剩多少赠送额度（点券）。hasGiftAllowance=false 表示
	// 账号读不出来 ⇒ 这条规则不拦（数据缺失不该误伤玩家）。
	giftDailyRemaining int64
	hasGiftAllowance   bool
}

// currentCeraShopGiftFriendState 是一条"赠送方 ↔ 收件人"关系的判定结果。
type currentCeraShopGiftFriendState struct {
	// Mutual 表示两个方向的好友边都存在（本服的好友边是单向存储的，
	// 「互为好友」必须两条都查到，只查一边会把"我加了他"当成"我们是好友"）。
	Mutual bool
	// Age 是这条**双向**关系已经存在多久：在**所有可信方向**里取**最短**的那一段
	// （等价于"A→B 与 B→A 里更晚建立的那一条"）。A→B 建了 10 天、B→A 只建了 1 天，
	// 那"互为好友"就只有 1 天。
	//
	// 老数据（NULL）只豁免**它自己那个方向**，不会把另一个方向的年轻边一起放行 ——
	// 否则"一方留着迁移前的老行、另一方昨天才加回来"就能立刻赠送。
	Age time.Duration
	// Trusted=false 表示两个方向都是老数据（friend_since 字段上线之前的关系）。
	// 这种情况必须整体按"已满足 7 天"处理，否则新规则会把所有老好友追溯封锁。
	Trusted bool
}

func (s *Service) currentCeraShopGiftSender(ctx context.Context, session *gameSession) (currentCeraShopGiftSender, error) {
	if s == nil || session == nil || session.selectedCharacterID == 0 {
		return currentCeraShopGiftSender{}, errCurrentCeraShopGiftSenderUnavailable
	}
	repositories, ok := s.repositoryGroup()
	if !ok || repositories.Character == nil {
		return currentCeraShopGiftSender{}, errCurrentCeraShopGiftSenderUnavailable
	}
	characterID := strconv.FormatUint(uint64(session.selectedCharacterID), 10)
	record, found, err := repositories.Character.Load(ctx, characterID)
	if err != nil {
		return currentCeraShopGiftSender{}, err
	}
	if !found {
		return currentCeraShopGiftSender{}, errCurrentCeraShopGiftSenderUnavailable
	}
	sender := currentCeraShopGiftSender{
		characterID: characterID,
		name:        strings.TrimSpace(record.Name),
		serverGroup: record.Stats["server_group_id"],
		accountID:   strings.TrimSpace(record.AccountID),
	}
	if sender.name == "" {
		return currentCeraShopGiftSender{}, errCurrentCeraShopGiftSenderUnavailable
	}
	// 职业是"基础职业"（dnf_characters.job），空值/非数字都按"没有职业"处理：
	// 规则 4 只在双方都能解析出职业时才判定，解析不出来的角色不应该被这条规则误伤。
	if value, err := strconv.ParseUint(strings.TrimSpace(record.Job), 10, 8); err == nil {
		sender.job = byte(value)
		sender.hasJob = true
	}
	if repositories.Account != nil && sender.accountID != "" {
		account, accountFound, loadErr := repositories.Account.Load(ctx, sender.accountID)
		if loadErr != nil {
			return currentCeraShopGiftSender{}, loadErr
		}
		if accountFound && !account.CreatedAt.IsZero() {
			sender.accountAge = s.gameplayNow().UTC().Sub(account.CreatedAt.UTC())
			sender.hasAccount = true
		}
		// 今天还剩多少赠送额度（账号维度、10 万点券/天）。账号读不出来时
		// hasGiftAllowance=false ⇒ 不拦（和账号年龄规则同一套"数据缺失不误伤"的口径）。
		if accountFound {
			if remaining, ok := currentCeraShopGiftDailyRemaining(&account, s.gameplayNow().UTC()); ok {
				sender.giftDailyRemaining = remaining
				sender.hasGiftAllowance = true
			}
		}
	}
	return sender, nil
}

// currentCeraShopGiftFriendship 读取两个方向的好友边，得出"互为好友"的结论。
//
// 只读：不写库、不改任何状态。仓库里存的是**单向**边，所以必须查两次。
func (s *Service) currentCeraShopGiftFriendship(ctx context.Context, senderName, recipientName string) (currentCeraShopGiftFriendState, error) {
	repositories, ok := s.repositoryGroup()
	if !ok || repositories.UnitedFriend == nil {
		return currentCeraShopGiftFriendState{}, errCurrentCeraShopGiftSenderUnavailable
	}
	senderName = strings.TrimSpace(senderName)
	recipientName = strings.TrimSpace(recipientName)
	if senderName == "" || recipientName == "" {
		return currentCeraShopGiftFriendState{}, errCurrentCeraShopGiftSenderUnavailable
	}
	forward, forwardSince, forwardTrusted, err := currentCeraShopGiftDirectedFriendship(ctx, repositories, senderName, recipientName)
	if err != nil {
		return currentCeraShopGiftFriendState{}, err
	}
	backward, backwardSince, backwardTrusted, err := currentCeraShopGiftDirectedFriendship(ctx, repositories, recipientName, senderName)
	if err != nil {
		return currentCeraShopGiftFriendState{}, err
	}
	if !forward || !backward {
		return currentCeraShopGiftFriendState{Mutual: false, Trusted: true}, nil
	}
	now := s.gameplayNow().UTC()
	// ★ 每个方向**各自**判定，两个方向都必须满足 7 天。
	//
	// 早先是"只要有一条边是老数据就把整对按已满足处理"，那是个真实可绕过的洞：
	// A→B 是迁移前留下的 NULL 老行、B 昨天才把 A 加回来，那么"互为好友"其实只有一天，
	// 却因为 A 那条边老而被放行。老数据只应该**豁免它自己那一条边**。
	state := currentCeraShopGiftFriendState{Mutual: true}
	trustedAge := currentCeraShopGiftFriendMinimumAge
	hasTrustedAge := false
	for _, edge := range []struct {
		since   time.Time
		trusted bool
	}{{forwardSince, forwardTrusted}, {backwardSince, backwardTrusted}} {
		if !edge.trusted {
			// 这条边早于 friend_since 上线：只豁免**它自己**，不影响另一条边的判定。
			continue
		}
		age := now.Sub(edge.since.UTC())
		// 取**最短**的那一段：两边里更年轻的那条决定"互为好友"有多久。
		if !hasTrustedAge || age < trustedAge {
			trustedAge = age
			hasTrustedAge = true
		}
	}
	if hasTrustedAge {
		state.Trusted = true
		state.Age = trustedAge
		return state, nil
	}
	// 两条边都是老数据：整对都按已满足处理（Age 给到阈值即"通过"）。
	state.Age = currentCeraShopGiftFriendMinimumAge
	return state, nil
}

func currentCeraShopGiftDirectedFriendship(
	ctx context.Context,
	repositories dnfrepo.Group,
	ownerName string,
	friendName string,
) (bool, time.Time, bool, error) {
	record, found, err := repositories.UnitedFriend.Load(ctx, ownerName)
	if err != nil {
		return false, time.Time{}, false, err
	}
	if !found {
		return false, time.Time{}, false, nil
	}
	for _, name := range record.FriendNames {
		if strings.TrimSpace(name) != friendName {
			continue
		}
		at, trusted := dnfrepo.UnitedFriendSince(record, friendName)
		return true, at, trusted, nil
	}
	return false, time.Time{}, false, nil
}

// evaluateCurrentCeraShopGiftQueryRules 判定 op136 阶段的规则，返回客户端结果码。
// 返回 0 表示通过；否则是 cera_shop_gift_query.go 里那张码表的一个值。
func (s *Service) evaluateCurrentCeraShopGiftQueryRules(
	ctx context.Context,
	session *gameSession,
	recipientName string,
	recipient currentCeraShopGiftRecipient,
) (byte, error) {
	sender, err := s.currentCeraShopGiftSender(ctx, session)
	if err != nil {
		return 0, err
	}
	if strings.TrimSpace(recipientName) == sender.name {
		return currentCeraShopGiftQueryCodeSelf, nil
	}
	// 每日赠送额度用完：客户端有现成文案（70107「每个账号每天最多可以赠送10万点券的
	// 礼物」），而且这一步在"查收件人"之前就能给出确定答案，不用等玩家点下单。
	if sender.hasGiftAllowance && sender.giftDailyRemaining <= 0 {
		s.logGameEvent(session, "game-cera-shop-gift-query-rejected",
			"char_id", session.selectedCharacterID,
			"name", recipientName,
			"reason", errCurrentCeraShopGiftDailyLimit,
			"account_id", sender.accountID,
			"daily_limit", currentCeraShopGiftDailyCeraLimit)
		return currentCeraShopGiftQueryCodeDailyCap, nil
	}
	// 跨区：双方必须在同一个大区。当前部署（单区、shard 9999）所有角色的
	// server_group_id 都是 0，所以这条规则现在不会拦任何人；一旦多区部署开始
	// 写入真实分组，它就是生效的。用"该ID不存在"回，与官方"别区角色在本区查不到"
	// 的表现一致。
	if sender.serverGroup != recipient.serverGroup {
		s.logGameEvent(session, "game-cera-shop-gift-query-rejected",
			"char_id", session.selectedCharacterID,
			"name", recipientName,
			"reason", errCurrentCeraShopGiftCrossRegion,
			"sender_server_group", sender.serverGroup,
			"recipient_server_group", recipient.serverGroup)
		return currentCeraShopGiftQueryCodeNotFound, nil
	}
	friendship, err := s.currentCeraShopGiftFriendship(ctx, sender.name, recipientName)
	if err != nil {
		return 0, err
	}
	if !friendship.Mutual {
		return currentCeraShopGiftQueryCodeNotFriend, nil
	}
	if friendship.Trusted && friendship.Age < currentCeraShopGiftFriendMinimumAge {
		return currentCeraShopGiftQueryCodeFriendTooNew, nil
	}
	return 0, nil
}

// evaluateCurrentCeraShopGiftPurchaseRules 判定 op64 阶段的规则，返回客户端失败码。
//
// 这里只看"跟商品无关"的两条：赠送方账号年龄、双方基础职业。
// 商品本身能不能赠送由结账时的落格路径判定（见 cera_shop_gift_purchase.go）。
func (s *Service) evaluateCurrentCeraShopGiftPurchaseRules(
	ctx context.Context,
	session *gameSession,
	recipientName string,
	recipientJob byte,
	recipientHasJob bool,
) (byte, error) {
	sender, err := s.currentCeraShopGiftSender(ctx, session)
	if err != nil {
		return 0, err
	}
	if sender.hasAccount && sender.accountAge < currentCeraShopGiftAccountMinimumAge {
		s.logGameEvent(session, "game-cera-shop-gift-purchase-rejected",
			"char_id", session.selectedCharacterID,
			"account_id", sender.accountID,
			"account_age_seconds", int64(sender.accountAge/time.Second),
			"reason", errCurrentCeraShopGiftAccountTooNew)
		return currentCeraShopPurchaseFailureCodeGiftAccountTooNew, nil
	}
	// 职业限制：只在两边都能解析出基础职业时判定。解析不出来（老角色/异常数据）
	// 时不拦——不能用"数据缺失"当成"职业不同"把玩家挡在门外。
	if sender.hasJob && recipientHasJob && sender.job != recipientJob {
		s.logGameEvent(session, "game-cera-shop-gift-purchase-rejected",
			"char_id", session.selectedCharacterID,
			"name", recipientName,
			"reason", errCurrentCeraShopGiftJobMismatch,
			"sender_job", sender.job,
			"recipient_job", recipientJob)
		return currentCeraShopPurchaseFailureCodeGiftNotGiftable, nil
	}
	return 0, nil
}
