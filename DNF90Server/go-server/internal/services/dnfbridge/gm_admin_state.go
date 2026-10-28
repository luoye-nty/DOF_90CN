package dnfbridge

import (
	"context"
	"errors"
	"fmt"
	"math"
	"sort"
	"strconv"
	"strings"
	"time"

	"longheng.io/server/internal/modules/dnf/alignedcmd"
	dnfcerashop "longheng.io/server/internal/modules/dnf/cerashop"
	dnfenum "longheng.io/server/internal/modules/dnf/dnfenum"
	dnfdungeonpermission "longheng.io/server/internal/modules/dnf/dungeonpermission"
	dnffatigue "longheng.io/server/internal/modules/dnf/fatigue"
	dnfpremium "longheng.io/server/internal/modules/dnf/premium"
	dnfprofession "longheng.io/server/internal/modules/dnf/profession"
	dnfquest "longheng.io/server/internal/modules/dnf/quest"
	dnfrepo "longheng.io/server/internal/modules/dnf/repository"
)

type GMWalletRequest struct {
	CharacterID   string `json:"character_id,omitempty"`
	CharacterName string `json:"character_name,omitempty"`
	Currency      string `json:"currency,omitempty"`
	Mode          string `json:"mode,omitempty"`
	Amount        int64  `json:"amount,omitempty"`
}

type GMWalletResult struct {
	Character      GMCharacterInfo `json:"character"`
	Currency       string          `json:"currency,omitempty"`
	Mode           string          `json:"mode,omitempty"`
	Before         int64           `json:"before,omitempty"`
	After          int64           `json:"after,omitempty"`
	Cera           int64           `json:"cera"`
	TokenCera      int64           `json:"token_cera"`
	HappyTokenCera int64           `json:"happy_token_cera"`
	Gold           int64           `json:"gold"`
	Online         bool            `json:"online"`
	Refreshed      bool            `json:"refreshed"`
	RefreshError   string          `json:"refresh_error,omitempty"`
}

type GMHappyTokenCeraRepairResult struct {
	Character GMCharacterInfo `json:"character"`
	// Granted 是本次补发的**欢乐代币券**数量（保持原有语义不变：既有调用方与
	// 用例都按这个口径读）。
	Granted int64 `json:"granted"`
	// TokenCeraGranted 是本次补发的**代币券**数量（2681762）。
	TokenCeraGranted int64  `json:"token_cera_granted"`
	Cera             int64  `json:"cera"`
	TokenCera        int64  `json:"token_cera"`
	HappyTokenCera   int64  `json:"happy_token_cera"`
	Online           bool   `json:"online"`
	Refreshed        bool   `json:"refreshed"`
	RefreshError     string `json:"refresh_error,omitempty"`
}

type GMCharacterSkillInfo struct {
	CharacterID string `json:"character_id"`
	Name        string `json:"name"`
	Level       int    `json:"level"`
	TotalSP     int    `json:"total_sp"`
	RemainingSP int    `json:"remaining_sp"`
	TotalTP     int    `json:"total_tp"`
	RemainingTP int    `json:"remaining_tp"`
}

type GMCharacterSkillUpdate struct {
	CharacterID string `json:"character_id"`
	RemainingSP *int   `json:"remaining_sp,omitempty"`
	RemainingTP *int   `json:"remaining_tp,omitempty"`
	// ExtendTotal authorizes this loopback-admin request to permanently raise
	// the character's SP/TP ceiling when a requested remaining value is above
	// the current earned total. The delta is persisted as bonus_sp/bonus_tp so
	// later level, scene and skill-tree ledger repairs retain the GM grant.
	ExtendTotal bool `json:"extend_total,omitempty"`
}

type GMNativeRefreshResult struct {
	Online       bool   `json:"online"`
	Refreshed    bool   `json:"refreshed"`
	RefreshError string `json:"refresh_error,omitempty"`
}

type GMCharacterSkillUpdateResult struct {
	GMNativeRefreshResult
	CharacterID string `json:"character_id"`
	ActiveTree  int    `json:"active_tree"`
	TotalSP     int    `json:"total_sp"`
	RemainingSP int    `json:"remaining_sp"`
	AddedSP     int    `json:"added_sp,omitempty"`
	TotalTP     int    `json:"total_tp"`
	RemainingTP int    `json:"remaining_tp"`
	AddedTP     int    `json:"added_tp,omitempty"`
}

type GMCharacterLevelResult struct {
	GMNativeRefreshResult
	CharacterID   string `json:"character_id"`
	PreviousLevel int    `json:"previous_level"`
	Level         int    `json:"level"`
}

type GMCharacterJobInfo struct {
	CharacterID string `json:"character_id"`
	Job         int    `json:"job"`
	FirstGrow   int    `json:"first_grow"`
	SecondGrow  int    `json:"second_grow"`
}

type GMCharacterJobUpdate struct {
	CharacterID string `json:"character_id"`
	Job         int    `json:"job"`
	FirstGrow   int    `json:"first_grow"`
	SecondGrow  int    `json:"second_grow"`
}

type GMCharacterJobUpdateResult struct {
	GMNativeRefreshResult
	CharacterID      string `json:"character_id"`
	Job              int    `json:"job"`
	PreviousGrowType int    `json:"previous_grow_type"`
	GrowType         int    `json:"grow_type"`
	TransitionCount  int    `json:"transition_count"`
	// QuestResetIDs lists the class-change/awakening quest rows removed when
	// the character was returned to its base profession.
	QuestResetIDs []int64 `json:"quest_reset_ids,omitempty"`
}

type GMDungeonUnlockRequest struct {
	CharacterID string         `json:"character_id"`
	Dungeons    map[string]int `json:"dungeons"`
}

type GMDungeonUnlockResult struct {
	GMNativeRefreshResult
	CharacterID string `json:"character_id"`
	Count       int    `json:"count"`
}

type GMEquipSlotUnlockResult struct {
	GMNativeRefreshResult
	CharacterID string `json:"character_id"`
	Before      int64  `json:"before"`
	After       int64  `json:"after"`
}

type GMBlackDiamondRequest struct {
	CharacterID string `json:"character_id"`
	Days        int    `json:"days,omitempty"`
}

type GMBlackDiamondResult struct {
	GMNativeRefreshResult
	CharacterID      string `json:"character_id"`
	AccountID        string `json:"account_id"`
	Days             int    `json:"days"`
	ExpiresAt        int64  `json:"expires_at"`
	RemainingSeconds int64  `json:"remaining_seconds"`
	Fatigue          int64  `json:"fatigue"`
	FatigueLimit     int64  `json:"fatigue_limit"`
	Deferred         bool   `json:"deferred,omitempty"`
}

// GMBlackDiamondQuery 黑钻只读查询：按角色 ID 或角色名定位账号，不修改任何数据。
type GMBlackDiamondQuery struct {
	CharacterID   string `json:"character_id,omitempty"`
	CharacterName string `json:"character_name,omitempty"`
}

// GMBlackDiamondStatus 黑钻当前状态快照（只读）。
//
// ExpiresAt/RemainingSeconds 直接来自账号级权益 premium_expire_1000，
// 与登录选人包、在线刷新读的是同一份数据；ServerNow 是服务端当前时间，
// 工具端据此换算剩余天数，不受客户端/运维机时钟差影响。
type GMBlackDiamondStatus struct {
	CharacterID      string `json:"character_id"`
	CharacterName    string `json:"character_name"`
	AccountID        string `json:"account_id"`
	Active           bool   `json:"active"`
	ExpiresAt        int64  `json:"expires_at"`
	RemainingSeconds int64  `json:"remaining_seconds"`
	RemainingDays    int64  `json:"remaining_days"`
	Fatigue          int64  `json:"fatigue"`
	FatigueLimit     int64  `json:"fatigue_limit"`
	// BlackDiamondFatigueLimit 黑钻生效时的疲劳上限（当前 EXE 为 188），
	// 供工具端识别"权益已生效但疲劳上限还没被刷新"这类中间态。
	BlackDiamondFatigueLimit int64 `json:"black_diamond_fatigue_limit"`
	Online                   bool  `json:"online"`
	ServerNow                int64 `json:"server_now"`
}

const gmAllEquipSlotsMask int64 = int64(dnfquest.ExEquipSlotAll)

func (s *Service) gmCharacter(ctx context.Context, repositories dnfrepo.Group, characterID, characterName string) (dnfrepo.CharacterRecord, error) {
	if repositories.Character == nil {
		return dnfrepo.CharacterRecord{}, ErrGMRepositoryUnavailable
	}
	characterID = strings.TrimSpace(characterID)
	characterName = strings.TrimSpace(characterName)
	if characterID == "" {
		var found bool
		var err error
		characterID, found, err = repositories.Character.FindIDByName(ctx, characterName)
		if err != nil {
			return dnfrepo.CharacterRecord{}, err
		}
		if !found {
			return dnfrepo.CharacterRecord{}, ErrGMCharacterNotFound
		}
	}
	character, found, err := repositories.Character.Load(ctx, characterID)
	if err != nil {
		return dnfrepo.CharacterRecord{}, err
	}
	// The loopback, token-authenticated GM API manages every character in the
	// active repository. The configured account ID is the gameplay bootstrap
	// owner, not an authorization boundary for launcher accounts stored beside
	// it in MySQL or SQLite.
	if !found || character.Stats["delete_flag"] != 0 || strings.TrimSpace(character.AccountID) == "" ||
		(characterName != "" && strings.TrimSpace(character.Name) != characterName) {
		return dnfrepo.CharacterRecord{}, ErrGMCharacterNotFound
	}
	return character, nil
}

func (s *Service) gmCharacterInfo(ctx context.Context, repositories dnfrepo.Group, character dnfrepo.CharacterRecord) (GMCharacterInfo, error) {
	cera := int64(0)
	if repositories.Account != nil {
		account, found, err := repositories.Account.Load(ctx, character.AccountID)
		if err != nil {
			return GMCharacterInfo{}, err
		}
		if found {
			cera = currentAccountCera(account)
		}
	}
	numericID, _ := strconv.ParseUint(character.CharacterID, 10, 16)
	_, online := s.onlineGameSession(uint16(numericID))
	return GMCharacterInfo{
		CharacterID: character.CharacterID,
		AccountID:   character.AccountID,
		Name:        character.Name,
		Level:       character.Level,
		Gold:        character.Stats["gold"],
		Cera:        cera,
		Online:      online,
	}, nil
}

func (s *Service) GMReadWallet(ctx context.Context, request GMWalletRequest) (GMWalletResult, error) {
	repositories, available := s.repositoryGroup()
	if !available {
		return GMWalletResult{}, ErrGMRepositoryUnavailable
	}
	character, err := s.gmCharacter(ctx, repositories, request.CharacterID, request.CharacterName)
	if err != nil {
		return GMWalletResult{}, err
	}
	info, err := s.gmCharacterInfo(ctx, repositories, character)
	if err != nil {
		return GMWalletResult{}, err
	}
	wallet := s.loadAccountWallet(ctx, repositories, character.AccountID)
	return GMWalletResult{
		Character:      info,
		Cera:           wallet.Cera,
		TokenCera:      wallet.TokenCera,
		HappyTokenCera: wallet.HappyTokenCera,
		Gold:           info.Gold,
		Online:         info.Online,
	}, nil
}

func (s *Service) GMUpdateWallet(ctx context.Context, request GMWalletRequest) (GMWalletResult, error) {
	request.Currency = strings.ToLower(strings.TrimSpace(request.Currency))
	request.Mode = strings.ToLower(strings.TrimSpace(request.Mode))
	// 钱包三种货币（点券 / 代币券 / 欢乐代币券）+ 金币。
	// 后两种是本轮新加的：没有它们就没法在真机上造"点券为 0、只有代币券"这类
	// 测试条件，也就没法验证支付瀑布与券入账。
	walletCurrency := dnfcerashop.WalletCurrency(request.Currency)
	isWalletCurrency := walletCurrency.Known()
	if (!isWalletCurrency && request.Currency != "gold") ||
		(request.Mode != "set" && request.Mode != "add") ||
		request.Amount < 0 || request.Amount > math.MaxInt32 {
		return GMWalletResult{}, ErrGMRequestInvalid
	}
	repositories, available := s.repositoryGroup()
	if !available || repositories.CharacterSettlement == nil || repositories.Account == nil {
		return GMWalletResult{}, ErrGMRepositoryUnavailable
	}
	selected, err := s.gmCharacter(ctx, repositories, request.CharacterID, request.CharacterName)
	if err != nil {
		return GMWalletResult{}, err
	}
	result := GMWalletResult{Currency: request.Currency, Mode: request.Mode}
	err = repositories.CharacterSettlement.WithinCharacterSettlement(ctx, selected.CharacterID, func(tx dnfrepo.Group) error {
		character, err := s.gmCharacter(ctx, tx, selected.CharacterID, selected.Name)
		if err != nil {
			return err
		}
		account, found, err := tx.Account.Load(ctx, character.AccountID)
		if err != nil {
			return err
		}
		if !found {
			return ErrGMRepositoryUnavailable
		}
		before := character.Stats["gold"]
		if isWalletCurrency {
			if current, ok := walletCurrency.BalanceOf(account); ok {
				before = current
			}
		}
		after := request.Amount
		if request.Mode == "add" {
			if before < 0 || before > math.MaxInt32 || request.Amount > math.MaxInt32-before {
				return fmt.Errorf("%w: wallet balance exceeds client range", ErrGMRequestInvalid)
			}
			after = before + request.Amount
		}
		if isWalletCurrency {
			metadataKey, _ := walletCurrency.MetadataKey()
			if !walletCurrency.SetOn(&account, after) {
				return ErrGMRequestInvalid
			}
			if err := dnfrepo.SaveAccountMetadataEntry(ctx, tx.Account, account, metadataKey, strconv.FormatInt(after, 10), time.Now().UTC()); err != nil {
				return err
			}
		} else {
			if character.Stats == nil {
				character.Stats = make(map[string]int64)
			}
			character.Stats["gold"] = after
			if err := dnfrepo.SaveCharacterFields(ctx, tx.Character, character, dnfrepo.CharacterFieldStats); err != nil {
				return err
			}
		}
		result.Before, result.After = before, after
		wallet := currentAccountWallet(account)
		result.Cera, result.TokenCera, result.HappyTokenCera = wallet.Cera, wallet.TokenCera, wallet.HappyTokenCera
		result.Gold = character.Stats["gold"]
		return nil
	})
	if err != nil {
		return GMWalletResult{}, err
	}
	updated, err := s.gmCharacter(ctx, repositories, selected.CharacterID, selected.Name)
	if err != nil {
		return GMWalletResult{}, err
	}
	result.Character, err = s.gmCharacterInfo(ctx, repositories, updated)
	if err != nil {
		return GMWalletResult{}, err
	}
	result.Cera, result.Gold = result.Character.Cera, result.Character.Gold
	wallet := s.loadAccountWallet(ctx, repositories, selected.AccountID)
	result.Cera, result.TokenCera, result.HappyTokenCera = wallet.Cera, wallet.TokenCera, wallet.HappyTokenCera
	characterID, _ := strconv.ParseUint(selected.CharacterID, 10, 16)
	session, online := s.onlineGameSession(uint16(characterID))
	result.Online = online
	if !online {
		return result, nil
	}
	// 用独立 context 推送，**不能用请求 ctx**：钱此刻已经落库，
	// 客户端断开（运维工具 8 秒超时）不该让这次刷新被事件循环丢弃。
	// 详见 gmPostCommitPushContext 的说明 —— 泡点"点券不实时刷新"就是这个坑。
	pushCtx, pushCancel := gmPostCommitPushContext()
	defer pushCancel()
	if err := s.callGameSession(pushCtx, session, "gm-admin-wallet-refresh", func() error {
		if session.selectedCharacterID != uint16(characterID) {
			return errors.New("selected character changed before wallet refresh")
		}
		// 三种钱包货币都推同一包（class0/op53 一次带三个数字）；
		// 只有金币才走金币体。早先这里判的是 `request.Currency == "cera"`，
		// 于是 GM 改代币券/欢乐代币券时会掉到金币分支 —— 钱包已落库、客户端却不变，
		// 而"GM 把代币券设成 10 万、商城还是 0"会被误读成支付瀑布坏了。
		if isWalletCurrency {
			return s.sendGameUpperRawClass(session, currentCeraShopUpdateMsgID, buildCurrentCeraShopWalletBody(result.Cera, result.TokenCera, result.HappyTokenCera), 0)
		}
		return s.sendGameUpperRawClass(session, uint16(dnfenum.CmdPacketWalkoutPartyMember), buildCurrentGoldStateBody(result.Gold), 0)
	}); err != nil {
		result.RefreshError = err.Error()
		return result, nil
	}
	result.Refreshed = true
	return result, nil
}

// GMRepairHappyTokenCeraVoucher migrates rows written by the old mailbox
// implementation, which incorrectly stored item 2681917 in the material bag.
// The removal and account-currency credit share one MySQL transaction or one
// SQLite mutation gate, so the voucher can never be duplicated or lost.
//
// 现在同时覆盖**代币券**（2681762）：它以前完全没有代码引用，凡是把它当普通道具
// 发进背包的路径都会留下同样用不掉的"材料"行。判据换成共享的券识别
// （cerashop.VoucherKindOf），两种券各记各的账。
func (s *Service) GMRepairHappyTokenCeraVoucher(ctx context.Context, request GMWalletRequest) (GMHappyTokenCeraRepairResult, error) {
	if ctx == nil {
		ctx = context.Background()
	}
	repositories, available := s.repositoryGroup()
	if !available || repositories.MailboxAssets == nil || repositories.Account == nil || repositories.Inventory == nil {
		return GMHappyTokenCeraRepairResult{}, ErrGMRepositoryUnavailable
	}
	selected, err := s.gmCharacter(ctx, repositories, request.CharacterID, request.CharacterName)
	if err != nil {
		return GMHappyTokenCeraRepairResult{}, err
	}
	var result GMHappyTokenCeraRepairResult
	refreshes := make([]alignedcmd.ItemSlotRefresh, 0, 1)
	err = repositories.MailboxAssets.WithinMailboxAssets(ctx, selected.CharacterID, selected.CharacterID, func(
		accounts dnfrepo.AccountRepository,
		_ dnfrepo.AccountInventoryRepository,
		characters dnfrepo.CharacterRepository,
		inventories dnfrepo.InventoryRepository,
		_ dnfrepo.MailboxRepository,
	) error {
		character, err := s.gmCharacter(ctx, dnfrepo.Group{Character: characters}, selected.CharacterID, selected.Name)
		if err != nil {
			return err
		}
		account, found, err := accounts.Load(ctx, character.AccountID)
		if err != nil {
			return err
		}
		if !found {
			return ErrGMRepositoryUnavailable
		}
		inventory, inventoryFound, err := inventories.Load(ctx, character.CharacterID)
		if err != nil {
			return err
		}
		if inventoryFound {
			inventory = dnfrepo.CloneInventory(inventory)
			for key, stack := range inventory.Slots {
				listType, slot, ok := parseSceneInventorySlotKey(key)
				if !ok || listType != dnfrepo.MainInventoryListType || stack.Count <= 0 {
					continue
				}
				voucher := dnfcerashop.VoucherKindOf(stack.ItemID)
				if voucher == dnfcerashop.VoucherNone {
					continue
				}
				if voucher == dnfcerashop.VoucherTokenCera {
					if result.TokenCeraGranted > math.MaxInt32-stack.Count {
						return ErrGMRequestInvalid
					}
					result.TokenCeraGranted += stack.Count
				} else {
					if result.Granted > math.MaxInt32-stack.Count {
						return ErrGMRequestInvalid
					}
					result.Granted += stack.Count
				}
				delete(inventory.Slots, key)
				refreshes = append(refreshes, alignedcmd.ItemSlotRefresh{ListType: listType, SlotIndex: slot})
			}
		}
		wallet := currentAccountWallet(account)
		if result.Granted > 0 || result.TokenCeraGranted > 0 {
			// 全有或全无：任一项溢出就整笔拒绝，避免"代币券补发了、欢乐代币券没补"
			// 这种半截状态（库存行已经删掉就没法回退了）。
			updated, creditErr := dnfcerashop.CreditVoucherCounts(&account, dnfcerashop.VoucherCounts{
				TokenCera:      result.TokenCeraGranted,
				HappyTokenCera: result.Granted,
			})
			if creditErr != nil {
				return ErrGMRequestInvalid
			}
			account.UpdatedAt = time.Now().UTC()
			if err := accounts.Save(ctx, account); err != nil {
				return err
			}
			inventory.UpdatedAt = account.UpdatedAt
			if err := dnfrepo.SaveInventoryFields(ctx, inventories, inventory, dnfrepo.InventoryFieldSlots); err != nil {
				return err
			}
			wallet = updated
		}
		result.Cera = wallet.Cera
		result.TokenCera = wallet.TokenCera
		result.HappyTokenCera = wallet.HappyTokenCera
		return nil
	})
	if err != nil {
		return GMHappyTokenCeraRepairResult{}, err
	}
	result.Character, err = s.gmCharacterInfo(ctx, repositories, selected)
	if err != nil {
		return GMHappyTokenCeraRepairResult{}, err
	}
	sort.Slice(refreshes, func(left, right int) bool { return refreshes[left].SlotIndex < refreshes[right].SlotIndex })
	characterID, _ := strconv.ParseUint(selected.CharacterID, 10, 16)
	session, online := s.onlineGameSession(uint16(characterID))
	result.Online = online
	// 两种券**任一种**有补发就要刷新：`Granted` 只统计欢乐代币券（保持既有语义），
	// 只补代币券时它是 0。早先只判 `Granted == 0`，于是"背包里只有代币券行"这种
	// 情况会整段跳过刷新 —— 行已删、钱包已入账，客户端两样都看不到，还会点那行幽灵券。
	if !online || (result.Granted == 0 && result.TokenCeraGranted == 0) {
		return result, nil
	}
	// 补发已经落库（行已删、钱包已入账），推送用独立 context，避免客户端断开把刷新丢掉。
	pushCtx, pushCancel := gmPostCommitPushContext()
	defer pushCancel()
	if err := s.callGameSession(pushCtx, session, "gm-admin-happy-token-cera-repair", func() error {
		if session.selectedCharacterID != uint16(characterID) {
			return errors.New("selected character changed before happy-token Cera repair refresh")
		}
		if err := s.sendSelectedIncrementalItemSlotRefreshes(session, "gm_happy_token_cera_repair", refreshes); err != nil {
			return err
		}
		return s.sendGameUpperRawClass(session, currentCeraShopUpdateMsgID, buildCurrentCeraShopWalletBody(result.Cera, result.TokenCera, result.HappyTokenCera), 0)
	}); err != nil {
		result.RefreshError = err.Error()
		return result, nil
	}
	result.Refreshed = true
	return result, nil
}

func (s *Service) GMReadCharacterSkill(ctx context.Context, characterID string) (GMCharacterSkillInfo, error) {
	repositories, available := s.repositoryGroup()
	if !available || repositories.Skill == nil {
		return GMCharacterSkillInfo{}, ErrGMRepositoryUnavailable
	}
	character, err := s.gmCharacter(ctx, repositories, characterID, "")
	if err != nil {
		return GMCharacterSkillInfo{}, err
	}
	skill, _, err := repositories.Skill.Load(ctx, character.CharacterID)
	if err != nil {
		return GMCharacterSkillInfo{}, err
	}
	points := skill.Points
	if activeTree, treeErr := currentSceneActiveSkillTree(character); treeErr == nil {
		if state, found := dnfrepo.SkillTree(skill, int(activeTree)); found {
			points = state.Points
		}
	}
	return GMCharacterSkillInfo{
		CharacterID: character.CharacterID, Name: character.Name, Level: character.Level,
		TotalSP: points.TotalSP, RemainingSP: points.RemainingSP,
		TotalTP: points.TotalTP, RemainingTP: points.RemainingTP,
	}, nil
}

func (s *Service) GMUpdateCharacterSkill(ctx context.Context, request GMCharacterSkillUpdate) (GMCharacterSkillUpdateResult, error) {
	repositories, available := s.repositoryGroup()
	if !available || repositories.CharacterProgression == nil {
		return GMCharacterSkillUpdateResult{}, ErrGMRepositoryUnavailable
	}
	character, err := s.gmCharacter(ctx, repositories, request.CharacterID, "")
	if err != nil {
		return GMCharacterSkillUpdateResult{}, err
	}
	if request.RemainingSP == nil && request.RemainingTP == nil {
		return GMCharacterSkillUpdateResult{}, fmt.Errorf("%w: remaining_sp or remaining_tp is required", ErrGMRequestInvalid)
	}
	if request.RemainingSP != nil && (*request.RemainingSP < 0 || *request.RemainingSP > math.MaxUint16) {
		return GMCharacterSkillUpdateResult{}, fmt.Errorf("%w: remaining_sp=%d is outside 0..%d", ErrGMRequestInvalid, *request.RemainingSP, uint64(math.MaxUint16))
	}
	if request.RemainingTP != nil && (*request.RemainingTP < 0 || *request.RemainingTP > math.MaxUint16) {
		return GMCharacterSkillUpdateResult{}, fmt.Errorf("%w: remaining_tp=%d is outside 0..%d", ErrGMRequestInvalid, *request.RemainingTP, uint64(math.MaxUint16))
	}
	result := GMCharacterSkillUpdateResult{CharacterID: character.CharacterID}
	err = repositories.CharacterProgression.WithinCharacterProgression(ctx, character.CharacterID, func(characters dnfrepo.CharacterRepository, skills dnfrepo.SkillRepository) error {
		current, found, err := characters.Load(ctx, character.CharacterID)
		if err != nil {
			return err
		}
		if !found {
			return ErrGMCharacterNotFound
		}
		activeTree, err := currentSceneActiveSkillTree(current)
		if err != nil {
			return fmt.Errorf("%w: %v", ErrGMRequestInvalid, err)
		}
		result.ActiveTree = int(activeTree)
		record, found, err := skills.Load(ctx, character.CharacterID)
		if err != nil {
			return err
		}
		if !found {
			record.CharacterID = character.CharacterID
		}
		state, treeFound := dnfrepo.SkillTree(record, int(activeTree))
		if !treeFound {
			if activeTree != 0 {
				return fmt.Errorf("%w: active skill tree %d is missing", ErrGMRequestInvalid, activeTree)
			}
			state = dnfrepo.SkillTreeState{Skills: record.Skills, Points: record.Points}
		}
		addedSP, addedTP := 0, 0
		if request.RemainingSP != nil {
			if found && *request.RemainingSP > state.Points.TotalSP {
				if !request.ExtendTotal {
					return fmt.Errorf("%w: remaining_sp=%d exceeds total_sp=%d; set extend_total to grant the difference", ErrGMRequestInvalid, *request.RemainingSP, state.Points.TotalSP)
				}
				addedSP = *request.RemainingSP - state.Points.TotalSP
			}
			if !found {
				state.Points.TotalSP = *request.RemainingSP
			}
			state.Points.RemainingSP = *request.RemainingSP
		}
		if request.RemainingTP != nil {
			if found && *request.RemainingTP > state.Points.TotalTP {
				if !request.ExtendTotal {
					return fmt.Errorf("%w: remaining_tp=%d exceeds total_tp=%d; set extend_total to grant the difference", ErrGMRequestInvalid, *request.RemainingTP, state.Points.TotalTP)
				}
				addedTP = *request.RemainingTP - state.Points.TotalTP
			}
			if !found {
				state.Points.TotalTP = *request.RemainingTP
			}
		}
		if addedSP > 0 || addedTP > 0 {
			if err := extendGMSkillPointTotals(&record, addedSP, addedTP); err != nil {
				return err
			}
			current = dnfrepo.CloneCharacter(current)
			if current.Stats == nil {
				current.Stats = make(map[string]int64)
			}
			bonusSP := current.Stats["bonus_sp"]
			bonusTP := current.Stats["bonus_tp"]
			if bonusSP < 0 || bonusTP < 0 || bonusSP > int64(math.MaxUint16-addedSP) || bonusTP > int64(math.MaxUint16-addedTP) {
				return fmt.Errorf("%w: persisted bonus SP/TP cannot represent added_sp=%d added_tp=%d", ErrGMRequestInvalid, addedSP, addedTP)
			}
			current.Stats["bonus_sp"] = bonusSP + int64(addedSP)
			current.Stats["bonus_tp"] = bonusTP + int64(addedTP)
			if err := dnfrepo.SaveCharacterFields(ctx, characters, current, dnfrepo.CharacterFieldStats); err != nil {
				return err
			}
			state, treeFound = dnfrepo.SkillTree(record, int(activeTree))
			if !treeFound && activeTree == 0 {
				state = dnfrepo.SkillTreeState{Skills: record.Skills, Points: record.Points}
			}
		}
		if request.RemainingSP != nil {
			state.Points.RemainingSP = *request.RemainingSP
		}
		if request.RemainingTP != nil {
			state.Points.RemainingTP = *request.RemainingTP
		}
		dnfrepo.SetSkillTree(&record, int(activeTree), state)
		result.TotalSP = state.Points.TotalSP
		result.RemainingSP = state.Points.RemainingSP
		result.AddedSP = addedSP
		result.TotalTP = state.Points.TotalTP
		result.RemainingTP = state.Points.RemainingTP
		result.AddedTP = addedTP
		return dnfrepo.SaveSkillFields(ctx, skills, record, dnfrepo.SkillFieldPoints, dnfrepo.SkillFieldTrees)
	})
	if err != nil {
		return GMCharacterSkillUpdateResult{}, err
	}
	result.GMNativeRefreshResult = s.gmRefreshOnlineCharacter(character.CharacterID, "gm-admin-sptp", func(session *gameSession) error {
		return s.sendSelectedActorCurrentSceneSkillInfo(session, "gm_admin_sptp_after_atomic_commit")
	})
	return result, nil
}

func extendGMSkillPointTotals(record *dnfrepo.SkillRecord, addedSP, addedTP int) error {
	if record == nil || addedSP < 0 || addedTP < 0 {
		return fmt.Errorf("%w: invalid SP/TP total extension", ErrGMRequestInvalid)
	}
	if err := extendGMSkillPointLedger(&record.Points, addedSP, addedTP); err != nil {
		return err
	}
	for treeIndex, tree := range record.Trees {
		if err := extendGMSkillPointLedger(&tree.Points, addedSP, addedTP); err != nil {
			return fmt.Errorf("%w: tree=%d", err, treeIndex)
		}
		record.Trees[treeIndex] = tree
	}
	return nil
}

func extendGMSkillPointLedger(points *dnfrepo.SkillPointState, addedSP, addedTP int) error {
	if points == nil || points.TotalSP < 0 || points.RemainingSP < 0 || points.TotalTP < 0 || points.RemainingTP < 0 ||
		points.RemainingSP > points.TotalSP || points.RemainingTP > points.TotalTP ||
		points.TotalSP > math.MaxUint16-addedSP || points.RemainingSP > math.MaxUint16-addedSP ||
		points.TotalTP > math.MaxUint16-addedTP || points.RemainingTP > math.MaxUint16-addedTP {
		return fmt.Errorf("%w: SP/TP ledger cannot be extended by sp=%d tp=%d", ErrGMRequestInvalid, addedSP, addedTP)
	}
	points.TotalSP += addedSP
	points.RemainingSP += addedSP
	points.TotalTP += addedTP
	points.RemainingTP += addedTP
	return nil
}

func (s *Service) GMSetCharacterLevel(ctx context.Context, characterID string, level int) (GMCharacterLevelResult, error) {
	if level < 1 || level > currentAdventureCharacterLevelCap {
		return GMCharacterLevelResult{}, ErrGMRequestInvalid
	}
	repositories, available := s.repositoryGroup()
	if !available || repositories.CharacterProgression == nil {
		return GMCharacterLevelResult{}, ErrGMRepositoryUnavailable
	}
	character, err := s.gmCharacter(ctx, repositories, characterID, "")
	if err != nil {
		return GMCharacterLevelResult{}, err
	}
	targetExperience, err := s.currentPVFLevelEntryExperience(ctx, level)
	if err != nil {
		return GMCharacterLevelResult{}, fmt.Errorf("resolve PVF entry experience for level %d: %w", level, err)
	}
	result := GMCharacterLevelResult{CharacterID: character.CharacterID, PreviousLevel: character.Level, Level: level}
	err = repositories.CharacterProgression.WithinCharacterProgression(ctx, character.CharacterID, func(characters dnfrepo.CharacterRepository, skills dnfrepo.SkillRepository) error {
		current, err := s.gmCharacter(ctx, dnfrepo.Group{Character: characters}, character.CharacterID, "")
		if err != nil {
			return err
		}
		result.PreviousLevel = current.Level
		if level < current.Level {
			return fmt.Errorf("%w: level regression has no in-game progression transition", ErrGMRequestInvalid)
		}
		current.Level = level
		if current.Stats == nil {
			current.Stats = make(map[string]int64)
		}
		// Stats["exp"] is cumulative. Writing zero for a level above one makes
		// the current client subtract that level's PVF entry threshold and show
		// a negative value. Start the target level at its exact PVF baseline.
		current.Stats["exp"] = int64(targetExperience)
		target, err := s.currentPVFSkillPointTarget(ctx, current)
		if err != nil {
			return err
		}
		skill, found, err := skills.Load(ctx, character.CharacterID)
		if err != nil {
			return err
		}
		if !found {
			skill = dnfrepo.SkillRecord{CharacterID: character.CharacterID, Points: target}
		} else {
			for tree := 0; tree < currentSkillInfoTreeCount; tree++ {
				state, treeFound := dnfrepo.SkillTree(skill, tree)
				if !treeFound {
					continue
				}
				next, _, syncErr := syncCurrentSkillPointState(state.Points, target)
				if syncErr != nil {
					return syncErr
				}
				state.Points = next
				dnfrepo.SetSkillTree(&skill, tree, state)
			}
		}
		if err := dnfrepo.SaveSkillFields(ctx, skills, skill, dnfrepo.SkillFieldPoints, dnfrepo.SkillFieldTrees); err != nil {
			return err
		}
		return dnfrepo.SaveCharacterFields(ctx, characters, current, dnfrepo.CharacterFieldBase, dnfrepo.CharacterFieldStats)
	})
	if err != nil {
		return GMCharacterLevelResult{}, err
	}
	result.GMNativeRefreshResult = s.gmRefreshOnlineCharacter(character.CharacterID, "gm-admin-level", func(session *gameSession) error {
		return s.sendGMLevelPostCommitState(session)
	})
	return result, nil
}

func (s *Service) GMReadCharacterJob(ctx context.Context, characterID string) (GMCharacterJobInfo, error) {
	repositories, available := s.repositoryGroup()
	if !available {
		return GMCharacterJobInfo{}, ErrGMRepositoryUnavailable
	}
	character, err := s.gmCharacter(ctx, repositories, characterID, "")
	if err != nil {
		return GMCharacterJobInfo{}, err
	}
	job, err := strconv.Atoi(strings.TrimSpace(character.Job))
	if err != nil {
		job = int(character.Stats["job"])
	}
	grow := int(character.Stats["grow_type"])
	return GMCharacterJobInfo{CharacterID: character.CharacterID, Job: job, FirstGrow: grow & 0x0f, SecondGrow: (grow >> 4) & 0x0f}, nil
}

func (s *Service) GMSetCharacterJob(ctx context.Context, request GMCharacterJobUpdate) (GMCharacterJobUpdateResult, error) {
	if request.Job < 0 || request.Job > 15 || request.FirstGrow < 0 || request.FirstGrow > 15 || request.SecondGrow < 0 || request.SecondGrow > 2 {
		return GMCharacterJobUpdateResult{}, ErrGMRequestInvalid
	}
	repositories, available := s.repositoryGroup()
	if !available || repositories.CharacterSettlement == nil {
		return GMCharacterJobUpdateResult{}, ErrGMRepositoryUnavailable
	}
	character, err := s.gmCharacter(ctx, repositories, request.CharacterID, "")
	if err != nil {
		return GMCharacterJobUpdateResult{}, err
	}
	if s.gmOnlineCharacterInDungeon(character.CharacterID) {
		return GMCharacterJobUpdateResult{}, fmt.Errorf("%w: profession transition is only valid while the online character is in town", ErrGMRequestInvalid)
	}
	profiles, skillCatalog, err := s.currentProfessionResources(ctx)
	if err != nil {
		return GMCharacterJobUpdateResult{}, err
	}
	// Returning to the base profession must also retire the persisted
	// class-change/awakening quest rows; otherwise the profession chain cannot
	// be accepted again. Resolve the catalog before the transaction so a PVF
	// failure aborts the request instead of committing a half-reset state.
	var questCatalog *dnfquest.Catalog
	if request.FirstGrow == 0 && request.SecondGrow == 0 {
		questCatalog, err = s.loadQuestCatalog(ctx)
		if err != nil {
			return GMCharacterJobUpdateResult{}, err
		}
	}
	result := GMCharacterJobUpdateResult{CharacterID: character.CharacterID, Job: request.Job}
	err = repositories.CharacterSettlement.WithinCharacterSettlement(ctx, character.CharacterID, func(tx dnfrepo.Group) error {
		current, err := s.gmCharacter(ctx, tx, character.CharacterID, "")
		if err != nil {
			return err
		}
		currentJob, err := strconv.Atoi(strings.TrimSpace(current.Job))
		if err != nil {
			currentJob = int(current.Stats["job"])
		}
		// A base-job conversion has no equivalent in-game profession quest or
		// current-client completion packet. Refuse it instead of fabricating a
		// grow-type ACK that would leave the actor and skill manager divergent.
		if currentJob != request.Job {
			return ErrGMRequestInvalid
		}
		if current.Stats == nil {
			current.Stats = make(map[string]int64)
		}
		currentGrowValue := current.Stats["grow_type"]
		if currentGrowValue < 0 || currentGrowValue > math.MaxUint8 {
			return ErrGMRequestInvalid
		}
		targetGrow, err := dnfprofession.Encode(byte(request.FirstGrow), byte(request.SecondGrow))
		if err != nil {
			return ErrGMRequestInvalid
		}
		result.PreviousGrowType = int(currentGrowValue)
		result.GrowType = int(targetGrow)
		currentFirst, currentAwakening := dnfprofession.Decode(byte(currentGrowValue))
		targetFirst, targetAwakening := dnfprofession.Decode(targetGrow)
		administrativeRebase := byte(currentGrowValue) != targetGrow &&
			(currentFirst != targetFirst || targetAwakening < currentAwakening)
		var transitions []dnfprofession.Transition
		if administrativeRebase {
			// Validate the complete target path from the base profession. The
			// rebuild below intentionally does not relax ordinary quest
			// transition rules; this is the explicit GM-only branch-rebase path.
			if _, err := gmProfessionTransitions(profiles, byte(request.Job), 0, targetGrow); err != nil {
				return ErrGMRequestInvalid
			}
		} else {
			transitions, err = gmProfessionTransitions(profiles, byte(request.Job), byte(currentGrowValue), targetGrow)
			if err != nil {
				return ErrGMRequestInvalid
			}
		}
		if !administrativeRebase && len(transitions) == 0 {
			return nil
		}
		skill, found, err := tx.Skill.Load(ctx, character.CharacterID)
		if err != nil {
			return err
		}
		if !administrativeRebase && (!found || len(skill.Skills) == 0) {
			return fmt.Errorf("GM profession transition requires an initialized skill record for character %s", character.CharacterID)
		}
		if !found {
			skill = dnfrepo.SkillRecord{CharacterID: character.CharacterID}
		}
		pointTarget, err := s.currentPVFSkillPointTarget(ctx, current)
		if err != nil {
			return err
		}
		if administrativeRebase {
			skill, err = profiles.RebuildSkillRecordForGrowType(
				skillCatalog,
				byte(request.Job),
				current.Level,
				targetGrow,
				skill,
				pointTarget,
			)
			if err != nil {
				return err
			}
			result.TransitionCount = 1
		} else {
			for tree := 0; tree < currentSkillInfoTreeCount; tree++ {
				state, treeFound := dnfrepo.SkillTree(skill, tree)
				if !treeFound {
					continue
				}
				next, _, syncErr := syncCurrentSkillPointState(state.Points, pointTarget)
				if syncErr != nil {
					return syncErr
				}
				state.Points = next
				dnfrepo.SetSkillTree(&skill, tree, state)
			}
			for _, transition := range transitions {
				skill, err = profiles.ApplySkillTransition(
					skillCatalog,
					byte(request.Job),
					current.Level,
					transition,
					skill,
					skill.Points,
				)
				if err != nil {
					return err
				}
			}
			result.TransitionCount = len(transitions)
		}
		current.Stats["grow_type"] = int64(targetGrow)
		if err := dnfrepo.SaveCharacterFields(ctx, tx.Character, current, dnfrepo.CharacterFieldStats); err != nil {
			return err
		}
		if err := dnfrepo.SaveSkillFields(ctx, tx.Skill, skill, dnfrepo.AllSkillFields()...); err != nil {
			return err
		}
		if questCatalog != nil && administrativeRebase && targetFirst == 0 && targetAwakening == 0 {
			// Base-career reset: drop [job change quest] types 1..3 inside the
			// same settlement transaction as the character and skill rebuild.
			// Expert-job type 20 and ordinary quests remain untouched.
			if tx.Quest == nil {
				return ErrGMRepositoryUnavailable
			}
			questRecord, questFound, questErr := tx.Quest.Load(ctx, character.CharacterID)
			if questErr != nil {
				return questErr
			}
			if questFound {
				resetPlan, planErr := questCatalog.PlanBaseProfessionQuestReset(questRecord, time.Now().UTC())
				if planErr != nil {
					return planErr
				}
				if len(resetPlan.ChangedFields) != 0 {
					if saveErr := dnfrepo.SaveQuestFields(ctx, tx.Quest, resetPlan.Record, resetPlan.ChangedFields...); saveErr != nil {
						return saveErr
					}
				}
				result.QuestResetIDs = append([]int64(nil), resetPlan.ResetQuestIDs...)
			}
		}
		return nil
	})
	if err != nil {
		return GMCharacterJobUpdateResult{}, err
	}
	result.GMNativeRefreshResult = s.gmRefreshOnlineCharacter(character.CharacterID, "gm-admin-profession", func(session *gameSession) error {
		return s.sendGMProfessionPostCommitState(session)
	})
	return result, nil
}

func gmProfessionTransitions(profiles *dnfprofession.Profiles, job byte, current, target byte) ([]dnfprofession.Transition, error) {
	currentFirst, currentAwakening := dnfprofession.Decode(current)
	targetFirst, targetAwakening := dnfprofession.Decode(target)
	if targetFirst == 0 && targetAwakening != 0 {
		return nil, dnfprofession.ErrTransitionInvalid
	}
	if currentFirst != 0 && currentFirst != targetFirst {
		return nil, dnfprofession.ErrTransitionOutOfStep
	}
	if targetAwakening < currentAwakening {
		return nil, dnfprofession.ErrTransitionOutOfStep
	}
	out := make([]dnfprofession.Transition, 0, 3)
	working := current
	if currentFirst == 0 && targetFirst != 0 {
		transition, err := profiles.PlanTransition(job, working, dnfprofession.Request{
			Kind: dnfprofession.KindClassChange, ChainType: 1, GrowNumber: targetFirst,
		})
		if err != nil {
			return nil, err
		}
		out = append(out, transition)
		working = transition.NewGrowType
		_, currentAwakening = dnfprofession.Decode(working)
	} else if currentFirst == 0 && targetFirst == 0 && targetAwakening == 0 {
		return out, nil
	}
	for stage := currentAwakening + 1; stage <= targetAwakening; stage++ {
		transition, err := profiles.PlanTransition(job, working, dnfprofession.Request{
			Kind: dnfprofession.KindAwakening, ChainType: 2, GrowNumber: stage,
		})
		if err != nil {
			return nil, err
		}
		out = append(out, transition)
		working = transition.NewGrowType
	}
	if working != target {
		return nil, dnfprofession.ErrTransitionOutOfStep
	}
	return out, nil
}

func (s *Service) GMUnlockDungeons(ctx context.Context, request GMDungeonUnlockRequest) (GMDungeonUnlockResult, error) {
	repositories, available := s.repositoryGroup()
	if !available || repositories.DungeonPermission == nil {
		return GMDungeonUnlockResult{}, ErrGMRepositoryUnavailable
	}
	if len(request.Dungeons) == 0 || len(request.Dungeons) > 4096 {
		return GMDungeonUnlockResult{}, ErrGMRequestInvalid
	}
	character, err := s.gmCharacter(ctx, repositories, request.CharacterID, "")
	if err != nil {
		return GMDungeonUnlockResult{}, err
	}
	ids := make([]int, 0, len(request.Dungeons))
	states := make(map[int]byte, len(request.Dungeons))
	for rawID, rawState := range request.Dungeons {
		id, err := strconv.Atoi(strings.TrimSpace(rawID))
		if err != nil || id <= 0 || uint64(id) > math.MaxUint32 || rawState < 0 || rawState > math.MaxUint8 {
			return GMDungeonUnlockResult{}, ErrGMRequestInvalid
		}
		if rawState == 0 {
			continue
		}
		ids = append(ids, id)
		states[id] = byte(rawState)
	}
	sort.Ints(ids)
	owner, err := dnfdungeonpermission.NewOwner(repositories)
	if err != nil {
		return GMDungeonUnlockResult{}, err
	}
	accountID := strings.TrimSpace(character.AccountID)
	accountStates := make(map[uint32]byte, len(ids))
	for _, id := range ids {
		accountStates[uint32(id)] = states[id]
	}
	if _, _, err := owner.ApplyMax(ctx, accountID, accountStates); err != nil {
		return GMDungeonUnlockResult{CharacterID: character.CharacterID}, err
	}
	result := GMDungeonUnlockResult{CharacterID: character.CharacterID, Count: len(ids)}
	result.GMNativeRefreshResult = s.gmRefreshOnlineCharacter(character.CharacterID, "gm-admin-dungeon-permissions", func(session *gameSession) error {
		return s.sendGMDungeonPermissionPostCommitState(ctx, session)
	})
	return result, nil
}

func (s *Service) GMUnlockEquipSlots(ctx context.Context, characterID string) (GMEquipSlotUnlockResult, error) {
	repositories, available := s.repositoryGroup()
	if !available || repositories.CharacterAssets == nil {
		return GMEquipSlotUnlockResult{}, ErrGMRepositoryUnavailable
	}
	character, err := s.gmCharacter(ctx, repositories, characterID, "")
	if err != nil {
		return GMEquipSlotUnlockResult{}, err
	}
	result := GMEquipSlotUnlockResult{CharacterID: character.CharacterID}
	err = repositories.CharacterAssets.WithinCharacterAssets(ctx, character.CharacterID, func(characters dnfrepo.CharacterRepository, _ dnfrepo.InventoryRepository, _ dnfrepo.EquipmentRepository) error {
		current, err := s.gmCharacter(ctx, dnfrepo.Group{Character: characters}, character.CharacterID, "")
		if err != nil {
			return err
		}
		if current.Stats == nil {
			current.Stats = make(map[string]int64)
		}
		result.Before = current.Stats["ex_equip_slot_stat"]
		result.After = result.Before | gmAllEquipSlotsMask
		current.Stats["ex_equip_slot_stat"] = result.After
		return dnfrepo.SaveCharacterFields(ctx, characters, current, dnfrepo.CharacterFieldStats)
	})
	if err != nil {
		return GMEquipSlotUnlockResult{}, err
	}
	result.GMNativeRefreshResult = s.gmRefreshOnlineCharacter(character.CharacterID, "gm-admin-equip-slots", func(session *gameSession) error {
		return s.sendGMEquipSlotPostCommitState(session)
	})
	return result, err
}

// GMBlackDiamondStatus 只读查询某个角色所属账号的黑钻现状。
//
// 只读语义：不落盘、不发包、不触发任何刷新，随便查多少次都不会改变游戏状态。
// 黑钻是账号级权益，所以任取该账号下的一个角色都能查到同一份到期时间。
func (s *Service) GMBlackDiamondStatus(ctx context.Context, query GMBlackDiamondQuery) (GMBlackDiamondStatus, error) {
	repositories, available := s.repositoryGroup()
	if !available || repositories.Account == nil || repositories.Character == nil {
		return GMBlackDiamondStatus{}, ErrGMRepositoryUnavailable
	}
	if strings.TrimSpace(query.CharacterID) == "" && strings.TrimSpace(query.CharacterName) == "" {
		return GMBlackDiamondStatus{}, ErrGMRequestInvalid
	}
	character, err := s.gmCharacter(ctx, repositories, query.CharacterID, query.CharacterName)
	if err != nil {
		return GMBlackDiamondStatus{}, err
	}
	account, found, err := repositories.Account.Load(ctx, character.AccountID)
	if err != nil {
		return GMBlackDiamondStatus{}, err
	}
	if !found {
		return GMBlackDiamondStatus{}, errors.New("Black Diamond account was not found")
	}
	now := s.gameplayNow().UTC()
	expiresAt := dnfpremium.BlackDiamondExpireAt(account)
	remaining := expiresAt - now.Unix()
	if remaining < 0 {
		remaining = 0
	}
	status := GMBlackDiamondStatus{
		CharacterID:              character.CharacterID,
		CharacterName:            character.Name,
		AccountID:                character.AccountID,
		Active:                   remaining > 0,
		ExpiresAt:                expiresAt,
		RemainingSeconds:         remaining,
		RemainingDays:            remaining / int64(24*time.Hour/time.Second),
		Fatigue:                  character.Stats["fatigue"],
		FatigueLimit:             character.Stats["fatigue_limit"],
		BlackDiamondFatigueLimit: dnffatigue.BlackDiamondLimit,
		ServerNow:                now.Unix(),
	}
	if numericID, parseErr := strconv.ParseUint(character.CharacterID, 10, 16); parseErr == nil && numericID > 0 {
		_, status.Online = s.onlineGameSession(uint16(numericID))
	}
	return status, nil
}

func (s *Service) GMActivateBlackDiamond(ctx context.Context, request GMBlackDiamondRequest) (GMBlackDiamondResult, error) {
	if request.Days == 0 {
		request.Days = 30
	}
	if request.Days < 1 || request.Days > 3650 {
		return GMBlackDiamondResult{}, ErrGMRequestInvalid
	}
	repositories, available := s.repositoryGroup()
	if !available || repositories.RentalAssets == nil {
		return GMBlackDiamondResult{}, ErrGMRepositoryUnavailable
	}
	character, err := s.gmCharacter(ctx, repositories, request.CharacterID, "")
	if err != nil {
		return GMBlackDiamondResult{}, err
	}
	activated, err := s.activateBlackDiamondForCharacter(
		ctx,
		character.AccountID,
		character.CharacterID,
		time.Duration(request.Days)*24*time.Hour,
	)
	if err != nil {
		return GMBlackDiamondResult{}, err
	}
	remaining := activated.ExpiresAt.Unix() - s.gameplayNow().UTC().Unix()
	if remaining < 0 {
		remaining = 0
	}
	result := GMBlackDiamondResult{
		CharacterID:      character.CharacterID,
		AccountID:        character.AccountID,
		Days:             request.Days,
		ExpiresAt:        activated.ExpiresAt.Unix(),
		RemainingSeconds: remaining,
		Fatigue:          activated.Character.Stats["fatigue"],
		FatigueLimit:     activated.Character.Stats["fatigue_limit"],
	}
	deferred := false
	result.GMNativeRefreshResult = s.gmRefreshOnlineCharacter(character.CharacterID, "gm-admin-black-diamond", func(session *gameSession) error {
		var sendErr error
		deferred, sendErr = s.sendCurrentBlackDiamondActivationState(
			ctx,
			session,
			activated.ExpiresAt,
			"gm_admin_black_diamond",
		)
		return sendErr
	})
	if deferred && result.Online && result.RefreshError == "" {
		result.Refreshed = false
		result.Deferred = true
	}
	return result, nil
}
