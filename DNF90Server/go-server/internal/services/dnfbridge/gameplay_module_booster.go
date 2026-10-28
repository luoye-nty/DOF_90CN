package dnfbridge

import (
	"context"
	"encoding/binary"
	"errors"
	"fmt"
	"math"
	"sort"
	"strconv"
	"strings"
	"time"

	"longheng.io/server/internal/modules/dnf/alignedcmd"
	dnfbooster "longheng.io/server/internal/modules/dnf/booster"
	dnfcerashop "longheng.io/server/internal/modules/dnf/cerashop"
	"longheng.io/server/internal/modules/dnf/dnfenum"
	dnfitemcooldown "longheng.io/server/internal/modules/dnf/itemcooldown"
	dnfitemdailylimit "longheng.io/server/internal/modules/dnf/itemdailylimit"
	"longheng.io/server/internal/modules/dnf/premium"
	dnfrepo "longheng.io/server/internal/modules/dnf/repository"
	dnfrevivecoin "longheng.io/server/internal/modules/dnf/revivecoin"
)

func boosterGameplayModule() gameplayModuleDefinition {
	opcode := uint16(dnfenum.CmdPacketUseBoosterItem)
	lotteryOpcode := uint16(dnfenum.CmdPacketUseLotteryItem)
	overflowOpcode := uint16(dnfenum.CmdPacketOverflowInfo)
	handler := func(service *Service, session *gameSession, request gameplayRequest) error {
		return service.handleCurrentBoosterItem(session, request.Body)
	}
	return gameplayModuleDefinition{
		Name: "booster-item",
		LegacyHandlers: map[uint16]gameplayHandler{
			opcode: handler,
			lotteryOpcode: func(service *Service, session *gameSession, request gameplayRequest) error {
				return service.handleCurrentLotteryItem(session, request.Body)
			},
			overflowOpcode: func(service *Service, session *gameSession, request gameplayRequest) error {
				return service.handleCurrentLotteryOverflowConfirm(session, request.Body)
			},
		},
		UpperHandlers: map[uint16]gameplayHandler{
			opcode: func(service *Service, session *gameSession, request gameplayRequest) error {
				if !requireDefaultGameplayClassification(service, session, request, "game-current-booster-blocked", "current_exe_op160_command_class_mismatch") {
					return nil
				}
				return handler(service, session, request)
			},
			lotteryOpcode: func(service *Service, session *gameSession, request gameplayRequest) error {
				if !requireDefaultGameplayClassification(service, session, request, "game-lottery-item-blocked", "current_exe_op27_command_class_mismatch") {
					return nil
				}
				return service.handleCurrentLotteryItem(session, request.Body)
			},
		},
	}
}

func (s *Service) handleCurrentBoosterItem(session *gameSession, body []byte) error {
	request, err := parseCurrentBoosterOpenRequest(body)
	if err != nil {
		s.logGameEvent(session, "game-current-booster-rejected", "body_len", len(body), "reason", err)
		return s.sendGameUpperFailure(session, uint16(dnfenum.CmdPacketUseBoosterItem), 4)
	}
	ctx, cancel := context.WithTimeout(context.Background(), createWriteTimeout)
	defer cancel()
	catalog, err := s.currentPVFItemCatalog()
	if err != nil {
		s.logGameEvent(session, "game-current-booster-rejected", "source_slot", request.SourceSlot, "reason", err)
		return s.sendGameUpperFailure(session, uint16(dnfenum.CmdPacketUseBoosterItem), 4)
	}
	definition, err := s.prepareCurrentBooster(ctx, session, catalog, request)
	if err != nil {
		s.logGameEvent(session, "game-current-booster-rejected",
			"source_slot", request.SourceSlot,
			"selected_item", request.SelectedItemID,
			"selected_count", len(request.Selections),
			"request_kind", request.Kind,
			"reason", err)
		return s.sendGameUpperFailure(session, uint16(dnfenum.CmdPacketUseBoosterItem), 4)
	}
	result, err := s.commitCurrentBooster(ctx, session, catalog, definition, request)
	if err != nil {
		s.logGameEvent(session, "game-current-booster-rejected",
			"source_slot", request.SourceSlot,
			"source_item", definition.Source.ItemID,
			"selected_item", request.SelectedItemID,
			"selected_count", len(request.Selections),
			"request_kind", request.Kind,
			"reason", err)
		return s.sendGameUpperFailure(session, uint16(dnfenum.CmdPacketUseBoosterItem), 4)
	}
	if err := s.sendGameUpperSuccess(
		session,
		uint16(dnfenum.CmdPacketUseBoosterItem),
		buildCurrentBoosterSuccessBody(result),
	); err != nil {
		return err
	}
	if err := s.sendSelectedIncrementalItemSlotRefreshes(
		session,
		"use_booster_item",
		[]alignedcmd.ItemSlotRefresh{{
			ListType:  dnfrepo.MainInventoryListType,
			SlotIndex: result.SourceSlot,
		}},
	); err != nil {
		return err
	}
	if result.ReviveCoinWalletChanged {
		if err := s.sendSelectedIncrementalItemSlotRefreshes(
			session,
			"use_booster_item_revive_coin_wallet",
			[]alignedcmd.ItemSlotRefresh{{
				ListType:  dnfrepo.MainInventoryListType,
				SlotIndex: dnfrevivecoin.WalletSlot,
			}},
		); err != nil {
			return err
		}
	}
	for _, listType := range result.ChangedLists {
		listBody, _, _, ok := s.buildCurrentItemListBodyForSession(ctx, session, listType)
		if !ok {
			return errCurrentBoosterOwnerUnavailable
		}
		if err := s.sendCurrentSceneItemList(session, uint16(dnfenum.CmdPacketLeaveParty), listBody); err != nil {
			return err
		}
	}
	// 礼盒开出了代币券 / 欢乐代币券：它们已经直接进账号钱包（没有占背包格），
	// 所以必须把新的钱包值推给客户端，否则玩家看不到到账。
	// 纯展示：失败只记日志，不让一次开启因为推送失败而变成失败。
	if !result.VoucherCredits.IsZero() {
		if err := s.sendCurrentCeraShopWalletProjection(ctx, session, "booster_voucher_credit"); err != nil {
			s.logGameEvent(session, "game-cera-shop-wallet-send-failed", "source", "booster_voucher_credit", "err", err)
		}
	}
	crystalPremiumActivated := false
	blackDiamondActivated := false
	for _, activation := range result.PremiumActivations {
		if activation.PremiumType == premium.TypeCrystal {
			crystalPremiumActivated = true
		}
		if premium.IsBlackDiamondStorageType(activation.PremiumType) {
			blackDiamondActivated = true
		}
		wireType, canNotify := currentPremiumActivationWireType(activation.PremiumType)
		if !canNotify {
			continue
		}
		if err := s.sendGameUpperRawClass(
			session,
			currentPremiumActivatedMsgID,
			buildCurrentPremiumActivatedBody(wireType, activation.RemainingSeconds),
			0,
		); err != nil {
			return err
		}
	}
	if blackDiamondActivated {
		if err := s.refreshCurrentBlackDiamondAfterActivation(ctx, session, "booster_after_black_diamond_activation"); err != nil {
			return err
		}
	}
	if crystalPremiumActivated {
		if err := s.sendCurrentCrystalContractState(session, "booster_after_crystal_contract_activation"); err != nil {
			return err
		}
	}
	if len(result.EpicBookRows) > 0 {
		body, err := buildCurrentEpicBookInfoBody(1, result.EpicBookRows)
		if err != nil {
			return err
		}
		if err := s.sendGameUpperRawClass(session, currentEpicBookInfoWireOpcode, body, 0); err != nil {
			return err
		}
	}
	s.scheduleCurrentRewardSlotHighlights(session, "use_booster_item", result.RewardSlots)
	s.logGameEvent(session, "game-current-booster-committed",
		"source_slot", result.SourceSlot,
		"source_item", result.SourceItemID,
		"source_remaining", result.SourceRemaining,
		"request_kind", request.Kind,
		"selected_item", request.SelectedItemID,
		"selected_count", len(request.Selections),
		"reward_count", len(result.Rewards),
		"rewards", fmt.Sprint(result.Rewards),
		"reward_slots", fmt.Sprint(result.RewardSlots),
		"changed_lists", fmt.Sprint(result.ChangedLists),
		"epic_book_rows", fmt.Sprint(result.EpicBookRows),
		"premium_activations", fmt.Sprint(result.PremiumActivations),
		"revive_coin_wallet_changed", result.ReviveCoinWalletChanged,
		// 券直接入钱包的数量（0 = 本次没开出券）。核对线上到账看这两个数。
		"voucher_token_cera", result.VoucherCredits.TokenCera,
		"voucher_happy_token_cera", result.VoucherCredits.HappyTokenCera,
		// 冷却窗口的证据：非 0 表示这次开启落下了一份跨会话保留的冷却（PVF
		// [cool time] + [cooltime maintenance]），重上后同组道具会被服务端拦住。
		"cooltime_until", result.SourceCooltimeUntil,
		"cooltime_remaining_seconds", currentBoosterCooltimeRemainingSeconds(result.SourceCooltimeUntil, s.gameplayNow()),
		"ack_body", "current_exe_op160_u32_source_u16_slot_u32_remaining_u32_mode_u16_rewards",
		"inventory_refresh", "current_exe_op14_source_slot_then_op13_real_repository_lists")
	// The client keeps its blue lock icons in the op251 lock list, not in the item
	// rows, and an op14 row refresh makes it rebuild those cells and drop them
	// (live 2026-09-15 07:36: opening a package cleared the equipment and avatar
	// locks until the bag was sorted, whose container refresh does re-send op251).
	// A package that hands out new rows must therefore replay the list; the replay
	// only happens when something is actually locked, so an all-unlocked character
	// pays nothing.
	if err := s.sendCurrentSelectedItemLockSnapshotIfAny(session, "use_booster_item_after_reward_grant"); err != nil {
		s.logGameEvent(session, "game-current-booster-lock-snapshot-send-failed", "error", err)
	}
	// The little blue padlock (sort lock) is client-only state dropped by the same
	// rebuild, so replay it too. Both replays are skipped when nothing is locked.
	if err := s.sendCurrentSortLockReplayIfAny(session, "use_booster_item_after_reward_grant"); err != nil {
		s.logGameEvent(session, "game-current-booster-sort-lock-replay-failed", "error", err)
	}
	// 每天可用次数（PVF [total usable count]）：结果包已经发出，这里补 op541，
	// 让礼盒的悬浮说明跟着走、也让客户端自己拦住超限的下一次点击。
	s.sendCurrentItemDailyUseState(
		session, result.SourceItemID, result.SourceDailyUsed, result.SourceDailyLimit, "use_booster_item")
	return nil
}

// currentBoosterCooltimeRemainingSeconds 把持久冷却的绝对到期时间换算成日志里的剩余秒数
// （运营验收时直接对着 packet_log 读）。until<=0（该道具没有 maintenance 冷却）返回 0。
func currentBoosterCooltimeRemainingSeconds(until int64, now time.Time) int64 {
	if until <= 0 {
		return 0
	}
	remaining := until - now.UTC().Unix()
	if remaining < 0 {
		return 0
	}
	return remaining
}

func (s *Service) commitCurrentBooster(
	ctx context.Context,
	session *gameSession,
	catalog *pvfDungeonDropCatalog,
	definition currentBoosterDefinition,
	request currentBoosterOpenRequest,
) (currentBoosterCommitResult, error) {
	if s == nil || session == nil || session.selectedCharacterID == 0 || catalog == nil || catalog.source == nil || definition.Source.ItemID == 0 {
		return currentBoosterCommitResult{}, errCurrentBoosterOwnerUnavailable
	}
	rewardAmounts, rewardOptions, goldAmount, err := resolveCurrentBoosterRewardAmounts(definition, request, secureCurrentBoosterRoll)
	if err != nil {
		return currentBoosterCommitResult{}, err
	}
	if request.RewardMultiplier > 1 {
		for itemID, count := range rewardAmounts {
			multiplied := uint64(count) * uint64(request.RewardMultiplier)
			if multiplied > currentBoosterMaxRewardUnits {
				return currentBoosterCommitResult{}, errCurrentBoosterPVFInvalid
			}
			rewardAmounts[itemID] = uint32(multiplied)
		}
		multipliedGold := uint64(goldAmount) * uint64(request.RewardMultiplier)
		if multipliedGold > math.MaxInt32 {
			return currentBoosterCommitResult{}, errCurrentBoosterPVFInvalid
		}
		goldAmount = uint32(multipliedGold)
	}
	epicCatalog, err := s.currentEpicBookCatalog()
	if err != nil {
		return currentBoosterCommitResult{}, err
	}
	premiumCatalog, err := s.currentPremiumCatalog()
	if err != nil {
		return currentBoosterCommitResult{}, err
	}
	accountID, characterID, owner, err := s.currentBoosterMutationOwner(session)
	if err != nil {
		return currentBoosterCommitResult{}, err
	}
	now := time.Now().UTC()
	result := currentBoosterCommitResult{
		SourceItemID: definition.Source.ItemID,
		SourceSlot:   request.SourceSlot,
	}
	err = owner.Open(ctx, dnfbooster.Command{
		AccountID:   accountID,
		CharacterID: characterID,
		UpdatedAt:   now,
		Project: func(assets *dnfbooster.Assets) (dnfbooster.Changes, error) {
			accountState := assets.Account
			inventory := assets.Inventory
			account := assets.AccountInventory
			epicBook := assets.EpicBook
			if accountState == nil || epicBook == nil || epicBook.Balances == nil {
				return dnfbooster.Changes{}, errCurrentBoosterOwnerUnavailable
			}
			sourceKey := currentCeraShopInventorySlotKey(dnfrepo.MainInventoryListType, request.SourceSlot)
			sourceStack, found := inventory.Slots[sourceKey]
			if !found || sourceStack.ItemID != int64(definition.Source.ItemID) || sourceStack.Count <= 0 || sourceStack.Count > math.MaxUint32 {
				return dnfbooster.Changes{}, errCurrentBoosterSourceMissing
			}
			if !definition.Source.ExpirationDate.IsZero() {
				sourceStack, _ = applyCurrentPVFItemExpirationAt(sourceStack, definition.Source, now)
			}
			if err := validateCurrentBoosterSourceExpiration(sourceStack, definition.Source, now); err != nil {
				return dnfbooster.Changes{}, err
			}
			characterDirty := false
			// 冷却（PVF `[cool time]` + `[cooltime group]` + `[cooltime maintenance]`）：
			// 声明了 maintenance 的道具由服务端保留冷却窗口。客户端的冷却门只是进程内内存
			// ——现场 2026-09-17：1 天冷却的礼盒小退重上后又能用。检查和写入都在同一个
			// 事务里：被拦时整条命令回滚，源道具不会被消耗；成功则落一份到期时间，重上后
			// 由 select 阶段的类 0 通知 172 重放给客户端。同 [cooltime group] 的道具共享
			// 一个窗口，所以解析器要能认出**已存在冷却行**的组。
			if definition.Source.HasCooltimeMaintenance && definition.Source.CooltimeMS > 0 {
				seconds := dnfitemcooldown.MillisecondsToSeconds(definition.Source.CooltimeMS)
				if seconds > 0 {
					if assets.Character == nil {
						return dnfbooster.Changes{}, errCurrentBoosterOwnerUnavailable
					}
					if assets.Character.Stats == nil {
						assets.Character.Stats = make(map[string]int64)
					}
					nowUnix := now.Unix()
					if blocked, active := dnfitemcooldown.Blocks(
						assets.Character.Stats, nowUnix, int64(definition.Source.ItemID),
						definition.Source.CooltimeGroup, s.currentPVFItemCooltimeGroup,
					); active {
						return dnfbooster.Changes{}, fmt.Errorf(
							"%w: item=%d blocked_by=%d until=%d",
							errCurrentBoosterCooltimeActive, definition.Source.ItemID, blocked.ItemID, blocked.ExpireAt)
					}
					dnfitemcooldown.Prune(assets.Character.Stats, nowUnix)
					result.SourceCooltimeUntil = nowUnix + seconds
					dnfitemcooldown.Set(assets.Character.Stats, int64(definition.Source.ItemID), result.SourceCooltimeUntil)
					characterDirty = true
				}
			}
			// 每天可用次数（PVF `[total usable count]`）：礼盒同样是消耗品，上限又属于
			// 道具本身，所以这里用的是和 op44/op515 完全相同的那份计数。判定与自增都在
			// **这个事务**里：到上限就整笔失败，源礼盒不会被消耗；成功则随 Character
			// 一起落库，没有任何"先占位再回滚"的中间态。
			if dailyLimit := definition.Source.TotalUsableCount; dailyLimit > 0 {
				if assets.Character == nil {
					return dnfbooster.Changes{}, errCurrentBoosterOwnerUnavailable
				}
				if assets.Character.Stats == nil {
					assets.Character.Stats = make(map[string]int64)
				}
				used, chargeErr := dnfitemdailylimit.Consume(
					assets.Character.Stats, int64(definition.Source.ItemID), dailyLimit, now, assets.Character.Level)
				if chargeErr != nil {
					return dnfbooster.Changes{}, fmt.Errorf(
						"%w: item=%d: %v",
						errCurrentBoosterDailyUseLimitReached, definition.Source.ItemID, chargeErr)
				}
				result.SourceDailyUsed = used
				result.SourceDailyLimit = dailyLimit
				characterDirty = true
			}
			if goldAmount > 0 {
				if assets.Character == nil {
					return dnfbooster.Changes{}, errCurrentBoosterOwnerUnavailable
				}
				if assets.Character.Stats == nil {
					assets.Character.Stats = make(map[string]int64)
				}
				goldBefore := assets.Character.Stats["gold"]
				if goldBefore < 0 || goldBefore > math.MaxInt32-int64(goldAmount) {
					return dnfbooster.Changes{}, errCurrentBoosterPVFInvalid
				}
				result.GoldGranted = goldAmount
				result.GoldAfter = goldBefore + int64(goldAmount)
				assets.Character.Stats["gold"] = result.GoldAfter
				characterDirty = true
			}
			if request.ConsumePremiumDaily {
				if assets.Character == nil || !premium.TryConsumeDaily(assets.Character, request.PremiumDailySlot, now) {
					return dnfbooster.Changes{}, fmt.Errorf("premium slot %d daily limit reached", request.PremiumDailySlot)
				}
				characterDirty = true
			}
			rewards, err := resolveCurrentBoosterRewards(catalog, rewardAmounts, rewardOptions, sourceStack, now)
			if err != nil {
				return dnfbooster.Changes{}, err
			}
			// 选择型 booster（含升级预约的武器箱）要把本分类的强化等级带到奖励上，
			// 否则 [booster equipment upgrade] 读了也没人用，发出去的还是 +0。
			if request.Kind == currentBoosterRequestSelection {
				selected, err := currentBoosterSelectedCandidates(definition, request)
				if err != nil {
					return dnfbooster.Changes{}, err
				}
				for i := range rewards {
					for _, candidate := range selected {
						if rewards[i].Definition.ItemID == candidate.ItemID {
							rewards[i].UpgradeLevel = candidate.UpgradeLevel
						}
					}
				}
			}

			materialKey := ""
			var materialItemID int64
			var materialCount int64
			if request.Kind == currentBoosterRequestRandom || request.Kind == currentBoosterRequestLottery {
				materialItemID = definition.Random.MaterialItemID
				materialCount = definition.Random.MaterialCountPerUse
			}
			if materialItemID > 0 && materialCount > 0 {
				materialKey = findCurrentBoosterMaterial(inventory.Slots, materialItemID, materialCount, sourceKey)
				if materialKey == "" {
					return dnfbooster.Changes{}, fmt.Errorf("%w: material item=%d count=%d", errCurrentBoosterSourceMissing, materialItemID, materialCount)
				}
			}

			result.SourceRemaining = uint32(sourceStack.Count - 1)
			changedLists := map[byte]struct{}{dnfrepo.MainInventoryListType: {}}
			if sourceStack.Count == 1 {
				delete(inventory.Slots, sourceKey)
			} else {
				sourceStack.Count--
				entry := currentItemListEntryFromStack(dnfrepo.MainInventoryListType, request.SourceSlot, sourceStack)
				sourceStack.RawEntry = append([]byte(nil), entry.data[:]...)
				inventory.Slots[sourceKey] = sourceStack
			}
			if materialKey != "" {
				materialStack := inventory.Slots[materialKey]
				_, materialSlot, parsed := parseSceneInventorySlotKey(materialKey)
				if !parsed {
					return dnfbooster.Changes{}, errCurrentBoosterPVFInvalid
				}
				result.MaterialItemID = materialItemID
				result.MaterialSlot = materialSlot
				materialStack.Count -= materialCount
				if materialStack.Count <= 0 {
					delete(inventory.Slots, materialKey)
				} else {
					if materialStack.Count > math.MaxUint32 {
						return dnfbooster.Changes{}, errCurrentBoosterPVFInvalid
					}
					result.MaterialRemaining = uint32(materialStack.Count)
					listType, slot, parsed := parseSceneInventorySlotKey(materialKey)
					if !parsed || listType != dnfrepo.MainInventoryListType {
						return dnfbooster.Changes{}, errCurrentBoosterPVFInvalid
					}
					entry := currentItemListEntryFromStack(listType, slot, materialStack)
					materialStack.RawEntry = append([]byte(nil), entry.data[:]...)
					inventory.Slots[materialKey] = materialStack
				}
			}

			changedRewardKeys := make(map[string]struct{})
			accountDirty := false
			accountStateDirty := false
			epicBookDirty := false
			epicBookBalances := make(map[uint32]uint32)
			premiumTypes := make(map[int64]struct{})
			voucherCredits := dnfcerashop.VoucherCounts{}
			for _, reward := range rewards {
				// 代币券 / 欢乐代币券：在道具 PVF 里它们是普通 `[material]`，既没有
				// `[action type]` 也没有任何"使用后转点券"的定义 —— 客户端**自己
				// 根本无法**把它们变成钱包余额。当成道具发进背包，玩家只会拿到一件
				// 永远用不掉的"材料"（现场 2026-09-19 的抱怨）。所以在这里直接入账号
				// 钱包、不占背包格，与邮件领取路径（mail/owner.go）同一套语义。
				//
				// 放在循环最前面：券的道具号是明确的，不可能同时是契约或史诗图鉴碎片，
				// 先判可以彻底排除被其它分支吞掉的可能。
				if voucher := dnfcerashop.VoucherKindOf(int64(reward.Definition.ItemID)); voucher != dnfcerashop.VoucherNone {
					counts := dnfcerashop.VoucherCounts{}
					if voucher == dnfcerashop.VoucherTokenCera {
						counts.TokenCera = int64(reward.Count)
					} else {
						counts.HappyTokenCera = int64(reward.Count)
					}
					// 溢出就整笔失败（礼盒不会被消耗），绝不静默截断余额。
					if _, creditErr := dnfcerashop.CreditVoucherCounts(accountState, counts); creditErr != nil {
						return dnfbooster.Changes{}, creditErr
					}
					accountStateDirty = true
					voucherCredits = voucherCredits.Add(counts)
					result.Rewards = append(result.Rewards, currentBoosterGrantedReward{ItemID: reward.Definition.ItemID, Count: reward.Count})
					continue
				}
				if contract, exists := premiumCatalog.contractsByItem[int64(reward.Definition.ItemID)]; exists {
					if contract.PremiumType <= 0 || contract.DurationSeconds <= 0 {
						return dnfbooster.Changes{}, errCurrentBoosterPVFInvalid
					}
					premium.Upsert(accountState, contract.PremiumType, contract.DurationSeconds, int64(reward.Count), now)
					for _, activationType := range premium.ActivationTypes(contract.PremiumType) {
						premiumTypes[activationType] = struct{}{}
					}
					accountStateDirty = true
					result.Rewards = append(result.Rewards, currentBoosterGrantedReward{ItemID: reward.Definition.ItemID, Count: reward.Count})
					continue
				}
				if entry, fragment := epicCatalog.EntryByFragment(reward.Definition.ItemID); fragment {
					current := epicBook.Balances[entry.EquipmentID]
					if reward.Count > math.MaxUint32-current {
						return dnfbooster.Changes{}, errCurrentEpicBookBalanceOverflow
					}
					balance := current + reward.Count
					epicBook.Balances[entry.EquipmentID] = balance
					epicBookBalances[entry.FragmentID] = balance
					epicBookDirty = true
					result.Rewards = append(result.Rewards, currentBoosterGrantedReward{ItemID: reward.Definition.ItemID, Count: reward.Count})
					continue
				}
				keys, sharedChanged, grantErr := grantCurrentBoosterRewardByOwner(inventory, account, catalog.source, reward, s.currentInventoryPlacementMainSlotCount(ctx, inventory, ""))
				if grantErr != nil {
					return dnfbooster.Changes{}, grantErr
				}
				accountDirty = accountDirty || sharedChanged
				for _, key := range keys {
					changedRewardKeys[key] = struct{}{}
				}
				result.Rewards = append(result.Rewards, currentBoosterGrantedReward{ItemID: reward.Definition.ItemID, Count: reward.Count})
				if dnfrevivecoin.IsReward(int64(reward.Definition.ItemID)) {
					result.ReviveCoinWalletChanged = true
				}
			}
			premiumTypeIDs := make([]int64, 0, len(premiumTypes))
			for premiumType := range premiumTypes {
				premiumTypeIDs = append(premiumTypeIDs, premiumType)
			}
			sort.Slice(premiumTypeIDs, func(i, j int) bool { return premiumTypeIDs[i] < premiumTypeIDs[j] })
			for _, premiumType := range premiumTypeIDs {
				result.PremiumActivations = append(result.PremiumActivations, currentCeraShopPremiumActivation{
					PremiumType:      premiumType,
					RemainingSeconds: premium.ExpireAt(*accountState, premiumType) - now.Unix(),
				})
			}
			result.EpicBookRows = make([]currentEpicBookBalanceRow, 0, len(epicBookBalances))
			for fragmentID, balance := range epicBookBalances {
				result.EpicBookRows = append(result.EpicBookRows, currentEpicBookBalanceRow{FragmentID: fragmentID, Balance: balance})
			}
			sort.Slice(result.EpicBookRows, func(i, j int) bool { return result.EpicBookRows[i].FragmentID < result.EpicBookRows[j].FragmentID })
			changedRewardKeyList := make([]string, 0, len(changedRewardKeys))
			for key := range changedRewardKeys {
				changedRewardKeyList = append(changedRewardKeyList, key)
			}
			sort.Strings(changedRewardKeyList)
			for _, key := range changedRewardKeyList {
				stack, found := inventory.Slots[key]
				if !found || stack.ItemID <= 0 {
					return dnfbooster.Changes{}, errCurrentBoosterPVFInvalid
				}
				if stack.Extra == nil {
					stack.Extra = make(map[string]string, 6)
				}
				stack.Extra["source"] = "booster_item"
				stack.Extra["last_grant_source"] = "booster_item"
				stack.Extra["booster_source_item"] = strconv.FormatUint(uint64(definition.Source.ItemID), 10)
				listType, slot, parsed := parseSceneInventorySlotKey(key)
				if !parsed {
					return dnfbooster.Changes{}, errCurrentBoosterPVFInvalid
				}
				entry := currentItemListEntryFromStack(listType, slot, stack)
				stack.RawEntry = append([]byte(nil), entry.data[:]...)
				inventory.Slots[key] = stack
				changedLists[listType] = struct{}{}
				if request.Kind == currentBoosterRequestLottery &&
					listType == dnfrepo.MainInventoryListType &&
					!result.LotteryDisplayEntrySet {
					displayEntry := entry
					// The op27 row drives the native result animation. Its amount is
					// the quantity granted by this opening, not the post-merge stack
					// total. The following op14/list refresh retains the authoritative
					// accumulated inventory count.
					if binary.LittleEndian.Uint32(displayEntry.data[6:10]) != 0 {
						var granted uint32
						for _, reward := range result.Rewards {
							if int64(reward.ItemID) == stack.ItemID {
								granted += reward.Count
							}
						}
						if granted > 0 {
							binary.LittleEndian.PutUint32(displayEntry.data[6:10], granted)
						}
					}
					result.LotteryDisplayEntry = displayEntry
					result.LotteryDisplayEntrySet = true
				}
			}
			rewardRefreshes, refreshErr := currentRewardSlotRefreshesFromKeys(changedRewardKeys)
			if refreshErr != nil {
				return dnfbooster.Changes{}, errCurrentBoosterPVFInvalid
			}
			result.RewardSlots = rewardRefreshes
			result.ChangedLists = make([]byte, 0, len(changedLists))
			for listType := range changedLists {
				result.ChangedLists = append(result.ChangedLists, listType)
			}
			sort.Slice(result.ChangedLists, func(i, j int) bool { return result.ChangedLists[i] < result.ChangedLists[j] })
			// 券的入账数量用**赋值**而不是累加：赋值天然幂等，将来若某个后端改成重跑
			// apply 也不会翻倍。
			//
			// 注：本仓库当前三套后端 × 四个事务入口的 apply 回调**都不会**重跑
			// （memory 走补偿回滚，mysql/sqlite 直接失败返回），所以这是防御性写法，
			// 不是在修一个已存在的重复记账问题。同函数里 result.Rewards 等仍用 append，
			// 那是存量写法、不是本次引入。
			result.VoucherCredits = voucherCredits
			return dnfbooster.Changes{Account: accountStateDirty, AccountInventory: accountDirty, Character: characterDirty, Inventory: true, EpicBook: epicBookDirty}, nil
		},
	})
	if err != nil {
		return currentBoosterCommitResult{}, currentBoosterMutationError(err)
	}
	return result, nil
}

func (s *Service) currentBoosterMutationOwner(session *gameSession) (string, string, *dnfbooster.Owner, error) {
	if s == nil || session == nil || session.selectedCharacterID == 0 {
		return "", "", nil, errCurrentBoosterOwnerUnavailable
	}
	repositories, ok := s.repositoryGroup()
	if !ok || repositories.BoosterAssets == nil {
		return "", "", nil, errCurrentBoosterOwnerUnavailable
	}
	accountID := strings.TrimSpace(s.accountIDForSession(session))
	if accountID == "" {
		return "", "", nil, errCurrentBoosterOwnerUnavailable
	}
	owner, err := dnfbooster.NewOwner(repositories)
	if err != nil {
		return "", "", nil, errCurrentBoosterOwnerUnavailable
	}
	characterID := strconv.FormatUint(uint64(session.selectedCharacterID), 10)
	return accountID, characterID, owner, nil
}

func currentBoosterMutationError(err error) error {
	switch {
	case errors.Is(err, dnfbooster.ErrOwnerUnavailable),
		errors.Is(err, dnfbooster.ErrAccountRequired),
		errors.Is(err, dnfbooster.ErrCharacterRequired),
		errors.Is(err, dnfbooster.ErrCharacterNotFound),
		errors.Is(err, dnfbooster.ErrAccountMismatch),
		errors.Is(err, dnfbooster.ErrInventoryNotFound):
		return errors.Join(errCurrentBoosterOwnerUnavailable, err)
	default:
		return err
	}
}

// prepareCurrentBooster is a read-only fast-fail pass. The domain Owner
// repeats authorization and inventory validation inside the commit
// transaction before consuming anything.
func (s *Service) prepareCurrentBooster(
	ctx context.Context,
	session *gameSession,
	catalog *pvfDungeonDropCatalog,
	request currentBoosterOpenRequest,
) (currentBoosterDefinition, error) {
	if s == nil || session == nil || session.selectedCharacterID == 0 || request.SourceSlot < 0 || catalog == nil || catalog.source == nil {
		return currentBoosterDefinition{}, errCurrentBoosterOwnerUnavailable
	}
	repositories, ok := s.repositoryGroup()
	if !ok || repositories.Character == nil || repositories.Inventory == nil {
		return currentBoosterDefinition{}, errCurrentBoosterOwnerUnavailable
	}
	characterID := strconv.FormatUint(uint64(session.selectedCharacterID), 10)
	character, found, err := repositories.Character.Load(ctx, characterID)
	if err != nil {
		return currentBoosterDefinition{}, err
	}
	if !found || character.CharacterID != characterID || strings.TrimSpace(character.AccountID) != strings.TrimSpace(s.accountIDForSession(session)) {
		return currentBoosterDefinition{}, errCurrentBoosterOwnerUnavailable
	}
	inventory, found, err := repositories.Inventory.Load(ctx, characterID)
	if err != nil {
		return currentBoosterDefinition{}, err
	}
	if !found || inventory.CharacterID != characterID {
		return currentBoosterDefinition{}, errCurrentBoosterOwnerUnavailable
	}
	stack, found := inventory.Slots[currentCeraShopInventorySlotKey(dnfrepo.MainInventoryListType, request.SourceSlot)]
	if !found || stack.ItemID <= 0 || stack.ItemID > math.MaxUint32 || stack.Count <= 0 || stack.Count > math.MaxUint32 {
		return currentBoosterDefinition{}, errCurrentBoosterSourceMissing
	}
	definition, err := resolveCurrentBoosterDefinition(catalog, uint32(stack.ItemID), request.Kind)
	if err != nil {
		return currentBoosterDefinition{}, err
	}
	// [booster info]/[charactor] 是**按职业**（并带等级区间）分组的：只有开启者自己
	// 职业那一组才该发。不过滤的话，鬼剑士开豪华婚礼化妆箱会把全职业的时装/道具
	// 一起领走 —— 现场（2026-10-04）正是如此。
	// 上游在同一位置做同样的事（gameplay_module_booster.go 的 prepare 路径）。
	if len(definition.Random.Groups) > 0 {
		definition.Random.Groups = alignedcmd.MagicBoxGroupsForCharacter(definition.Random.Groups, character)
	}
	if err := validateCurrentBoosterSourceExpiration(stack, definition.Source, time.Now().UTC()); err != nil {
		return currentBoosterDefinition{}, err
	}
	if request.Kind == currentBoosterRequestSelection {
		if _, err := currentBoosterSelectedCandidates(definition, request); err != nil {
			return currentBoosterDefinition{}, err
		}
	}
	return definition, nil
}
