package dnfbridge

import (
	"context"
	"encoding/binary"
	"fmt"
	"sort"
	"time"

	"longheng.io/server/internal/modules/dnf/alignedcmd"
	"longheng.io/server/internal/modules/dnf/dnfenum"
	"longheng.io/server/internal/modules/dnf/premium"
	dnfproto "longheng.io/server/internal/modules/dnf/protocol"
	dnfrepo "longheng.io/server/internal/modules/dnf/repository"
)

const currentLotteryNativeResultBodySize = 1 + 2 + currentItemListEntryWireSize + 4

func decodeCurrentLotteryRequest(body []byte) (bool, int16, error) {
	if len(body) < 4 {
		return false, 0, fmt.Errorf("lottery body length %d is below 4", len(body))
	}
	mode := binary.LittleEndian.Uint16(body[:2])
	slot := int16(binary.LittleEndian.Uint16(body[2:4]))
	if mode > 1 || slot < 0 {
		return false, 0, fmt.Errorf("lottery mode=%d slot=%d is invalid", mode, slot)
	}
	return mode == 1, slot, nil
}

func buildCurrentLotteryErrorBody() []byte {
	// NoPack.exe's class-1 op27 handler receives the first byte as the success
	// flag and the second byte as the native failure reason. A short, complete
	// failure body prevents the handler from interpreting phase-start fields as
	// an error code.
	return []byte{0, 4}
}

func (s *Service) handleCurrentLotteryItem(session *gameSession, body []byte) error {
	wantsDouble, slot, err := decodeCurrentLotteryRequest(body)
	if err != nil {
		s.logGameEvent(session, "game-lottery-item-rejected", "body_len", len(body), "reason", err)
		return s.sendGameUpperRawClass(session, uint16(dnfenum.CmdPacketUseLotteryItem), buildCurrentLotteryErrorBody(), dnfproto.DefaultChannelClassification)
	}
	ctx, cancel := context.WithTimeout(context.Background(), createWriteTimeout)
	defer cancel()
	catalog, err := s.currentPVFItemCatalog()
	if err != nil {
		return s.sendGameUpperRawClass(session, uint16(dnfenum.CmdPacketUseLotteryItem), buildCurrentLotteryErrorBody(), dnfproto.DefaultChannelClassification)
	}
	// 塔罗牌类消耗品（`[monster tarot card]`）走的是**同一个 op27**，但既有的
	// booster 解析器对它是必然拒绝（booster_pvf.go:64：type is not random）。所以
	// 在构造 booster 请求之前做一次显式分派：只有源道具确实是塔罗牌时才由
	// tarot_lottery_item.go 接管并直接返回；其它任何道具都继续走下面这条**逐字节
	// 未改动**的罐子路径。
	//
	// 为什么要在这里而不是继续挂在 op44：见 tarot_lottery_item.go 文件头（现场
	// 2026-09-30 的实机包证明客户端对一个塔罗牌发的是 class-1/type-27）。
	if tarotHandled, tarotErr := s.tryHandleCurrentTarotLotteryItem(ctx, session, catalog, wantsDouble, slot); tarotHandled || tarotErr != nil {
		return tarotErr
	}

	request := currentBoosterOpenRequest{Kind: currentBoosterRequestLottery, SourceSlot: slot}
	definition, err := s.prepareCurrentBooster(ctx, session, catalog, request)
	if err != nil || !isCurrentLotteryJarType(definition.StackableType) {
		s.logGameEvent(session, "game-lottery-item-rejected",
			"slot", slot,
			"stackable_type", definition.StackableType,
			"reason", err)
		return s.sendGameUpperRawClass(session, uint16(dnfenum.CmdPacketUseLotteryItem), buildCurrentLotteryErrorBody(), dnfproto.DefaultChannelClassification)
	}
	doubleReward := false
	now := time.Now().UTC()
	if wantsDouble {
		_, account, character, premiumErr := s.currentPremiumServiceRecords(ctx, session)
		doubleReward = premiumErr == nil &&
			premium.Active(account, premium.DevilSlotType(premium.DevilSlotDoubleJar), now) &&
			premium.DailyUsage(character, premium.DevilSlotDoubleJar, now) < premium.DailyLimit(premium.DevilSlotDoubleJar)
	}
	request.RewardMultiplier = 1
	request.ConsumePremiumDaily = doubleReward
	request.PremiumDailySlot = premium.DevilSlotDoubleJar
	if doubleReward {
		request.RewardMultiplier = 2
	}
	return s.commitAndSendCurrentLottery(ctx, session, catalog, definition, request, wantsDouble, doubleReward)
}

func (s *Service) handleCurrentLotteryOverflowConfirm(session *gameSession, body []byte) error {
	// The current NoPack.exe consumes the complete replacement item row from
	// the op27 response before it starts the native jar animation. Some client
	// paths still emit the historical op217 completion notification; the
	// reward has already been committed by op27, so acknowledging it here would
	// duplicate both source consumption and rewards.
	return nil
}

func (s *Service) commitAndSendCurrentLottery(
	ctx context.Context,
	session *gameSession,
	catalog *pvfDungeonDropCatalog,
	definition currentBoosterDefinition,
	request currentBoosterOpenRequest,
	requestedDouble bool,
	doubleReward bool,
) error {
	slot := request.SourceSlot
	result, err := s.commitCurrentBooster(ctx, session, catalog, definition, request)
	if err != nil {
		s.logGameEvent(session, "game-lottery-item-open-rejected", "slot", slot, "double_reward", doubleReward, "reason", err)
		return s.sendGameUpperRawClass(session, uint16(dnfenum.CmdPacketUseLotteryItem), buildCurrentLotteryErrorBody(), dnfproto.DefaultChannelClassification)
	}
	resultBody, err := buildCurrentLotteryResultBody(result)
	if err != nil {
		return err
	}
	// NoPack's op27 handler starts a native jar animation before it consumes the
	// response's source/reward row. Sending any inventory update after that
	// response can replace objects the animation queue still holds. The consumed
	// material row is a server-owned mutation the native handler does not apply,
	// so publish that single row before the op27 result: by the time the client
	// enters the animation, its material count is already current.
	if result.MaterialItemID > 0 && result.MaterialSlot >= 0 {
		if err := s.sendSelectedIncrementalItemSlotRefreshes(
			session,
			"lottery_material_before_native_result",
			[]alignedcmd.ItemSlotRefresh{{
				ListType:  dnfrepo.MainInventoryListType,
				SlotIndex: result.MaterialSlot,
			}},
		); err != nil {
			return err
		}
	}
	if err := s.sendGameUpperRawClass(session, uint16(dnfenum.CmdPacketUseLotteryItem), resultBody, dnfproto.DefaultChannelClassification); err != nil {
		return err
	}
	// NoPack.exe sub_1D31260 applies both sides of a successful op27 locally:
	// it decrements/removes the source jar before reading the 0x77-byte reward
	// row, then creates or increments the reward object from that row. Sending
	// an op14 source-slot refresh or an op13 full-list refresh here is therefore
	// not only redundant; during "open all" it replaces inventory objects while
	// the native animation queue still holds them. The next automatic op27 can
	// then dereference stale client state and terminate the process. Keep op27 as
	// the sole inventory mutation packet for the entire native opening sequence.
	if result.GoldGranted > 0 {
		if err := s.sendGameUpperRawClass(
			session,
			uint16(dnfenum.CmdPacketWalkoutPartyMember),
			buildCurrentGoldStateBody(result.GoldAfter),
			0,
		); err != nil {
			return err
		}
	}
	if doubleReward {
		if err := s.sendCurrentPremiumServiceState(session, "lottery_double_reward_after_commit", currentPremiumServiceTypeForSession(session)); err != nil {
			return err
		}
	}
	s.logGameEvent(session, "game-lottery-item-open-committed",
		"slot", slot,
		"source_item", result.SourceItemID,
		"source_remaining", result.SourceRemaining,
		"material_item", result.MaterialItemID,
		"material_slot", result.MaterialSlot,
		"material_remaining", result.MaterialRemaining,
		"requested_double", requestedDouble,
		"double_reward", doubleReward,
		"gold_granted", result.GoldGranted,
		"gold_after", result.GoldAfter,
		"reward_count", len(result.Rewards),
		"rewards", fmt.Sprint(result.Rewards),
		"op27_body_len", len(resultBody),
		"state_source", "NoPack.exe_sub_1D31260_u16_source_plus_raw_0x77_item_plus_u32",
		"inventory_sync", "native_op27_only_no_interleaved_op14_or_op13")
	// 每天可用次数（PVF [total usable count]）：抽奖罐同样受这份契约约束 —— 但**只**
	// 在服务端拦（计数与判定已经在 commitCurrentBooster 的同一个事务里完成），这里
	// 刻意**不**追加 op541。原因见上面那段：op27 的成功路径有一条被测试钉死的
	// "结果包之后不得再有包"约束（booster_item_test.go 的
	// TestCurrentLotteryUpgradableLegacyJarUsesNativeItemRowAndGrantsPVFReward），
	// 它记录的是真机上"动画队列仍握着背包对象时插入背包包 → 客户端崩溃"的教训。
	// op541 只在既有道具对象上写两个计数，理论上不同于被禁止的 op14/op13，但那条
	// 断言无法离线证明安全，所以罐子的 tooltip 计数改由**登录回放**刷新
	// （select_character_inventory_bootstrap.go 的 sendCurrentCharacterDailyUseStates，
	// 它按角色 stats 重放全部 item_daily_used_*，不区分道具是盒还是罐）。
	// 要让罐子也即时刷新，需要一次真机取证后再放开这里。
	return nil
}

func isCurrentLotteryJarType(stackableType string) bool {
	return stackableType == "[upgradable legacy]" || stackableType == "[random upgradable legacy]"
}

func buildCurrentLotteryResultBody(result currentBoosterCommitResult) ([]byte, error) {
	if len(result.Rewards) == 0 && result.GoldGranted == 0 {
		return buildCurrentLotteryErrorBody(), nil
	}
	var displayEntry currentItemListEntry
	displayFound := result.LotteryDisplayEntrySet
	if displayFound {
		displayEntry = result.LotteryDisplayEntry
	}
	if !displayFound && len(result.Rewards) == 0 {
		// Current EXE reserves list-0 slot 0/item 0 for the gold wallet. The
		// op27 row drives the result animation and therefore carries only this
		// opening's award; the following op14 refresh carries the durable total.
		displayEntry = currentGoldWalletItemListEntry(int64(result.GoldGranted))
		displayFound = true
	}
	if !displayFound && len(result.Rewards) > 0 {
		rewards := append([]currentBoosterGrantedReward(nil), result.Rewards...)
		sort.Slice(rewards, func(i, j int) bool { return rewards[i].ItemID < rewards[j].ItemID })
		// Premium and collection rewards are intentionally persisted outside
		// the main inventory and therefore have no durable list-0 row. The
		// native animation still requires a complete item record; use a
		// transient display row and let the authoritative list refresh reconcile
		// it immediately after the animation starts.
		displayEntry = currentItemListEntryFromStack(
			dnfrepo.MainInventoryListType,
			result.SourceSlot,
			dnfrepo.ItemStack{ItemID: int64(rewards[0].ItemID), Count: int64(rewards[0].Count)},
		)
		displayFound = true
	}
	if !displayFound {
		return buildCurrentLotteryErrorBody(), nil
	}
	var writer packetWriter
	writer.writeByte(1)
	writer.writeUint16(uint16(result.SourceSlot))
	writer.writeBytes(displayEntry.data[:])
	writer.writeUint32(0)
	body := writer.bytes()
	if len(body) != currentLotteryNativeResultBodySize {
		return nil, fmt.Errorf("lottery native result body length %d, want %d", len(body), currentLotteryNativeResultBodySize)
	}
	return body, nil
}
