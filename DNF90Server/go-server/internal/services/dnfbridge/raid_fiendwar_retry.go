package dnfbridge

import (
	"context"
	"fmt"
	"math"
	"strconv"
	"time"

	dnfrepo "longheng.io/server/internal/modules/dnf/repository"
)

// fiendWarRetryReceiptValueKey is the settings key under which the retry-ticket
// refund records that it already paid this attempt.
const fiendWarRetryReceiptValueKey = "fiendwar_retry_v1"

// grantRuntimeFiendWarRetry is the native failed-run ticket refund: when a Fiend
// War run fails, every member that paid the entry ticket gets it back, once.
//
// The receipt scope carries the immutable attempt identity (raid key, start
// attempt, lifecycle start instant), so a replayed timeout/failure callback is
// read-only and a later run cannot reuse an earlier receipt. That is the whole
// reason the scope is built from state rather than from a counter.
func (s *Service) grantRuntimeFiendWarRetry(state runtimeRaidState, now time.Time) error {
	if state.RaidMode != runtimeRaidOwnerFiendWar || state.FiendWarConfig == nil ||
		!state.FiendWarConfig.Retry || state.StartAttempt == 0 || len(state.ChargedMembers) == 0 {
		return nil
	}
	items, err := s.currentPVFItemCatalog()
	if err != nil {
		return err
	}
	definition, err := items.ResolveItem(state.FiendWarConfig.RetryItem.ItemID)
	if err != nil {
		return err
	}
	definition, err = currentPVFItemDefinitionForGrantAt(definition, now)
	if err != nil {
		return err
	}
	repositories, ok := s.repositoryGroup()
	if !ok || repositories.RaidRewards == nil {
		return errRuntimeRaidRewardRepository
	}
	ids := make([]string, 0, len(state.ChargedMembers))
	refreshes := make([]runtimeRaidRewardInventoryRefresh, 0, len(state.ChargedMembers))
	for _, id := range state.ChargedMembers {
		ids = append(ids, strconv.Itoa(int(id)))
	}
	scope := fmt.Sprintf("fiendwar_retry:v1:%08x:%x:%x", state.RaidKey, state.StartAttempt, uint64(state.Lifecycle.StartedAt.UnixNano()))
	ctx, cancel := context.WithTimeout(context.Background(), createWriteTimeout)
	defer cancel()
	err = repositories.RaidRewards.WithinRaidRewards(ctx, ids, scope, func(inventories dnfrepo.InventoryRepository, receipts dnfrepo.SettingsRepository) error {
		record, found, err := receipts.Load(ctx, scope)
		if err != nil {
			return err
		}
		if found && record.Values[fiendWarRetryReceiptValueKey] != "" {
			return nil
		}
		for index, id := range ids {
			inventory, found, err := inventories.Load(ctx, id)
			if err != nil {
				return err
			}
			if !found {
				return fmt.Errorf("%w: character=%s inventory missing", errRuntimeRaidRewardGrant, id)
			}
			// 本服务端把"主背包已解锁到第几格"做成了必填参数，漏传会把道具放进
			// 客户端不渲染的扩展格 —— 见 cera_shop_grant.go 里那段注释(15 个调用点
			// 曾静默不传)。这里按攻坚结算发奖的既有做法取当前角色的放置上限，与
			// raid_reward_runtime.go 的 grantCurrentCeraShopProduct 调用一致。
			slots, err := grantCurrentCeraShopProduct(
				&inventory,
				definition,
				state.FiendWarConfig.RetryItem.Count,
				s.currentInventoryPlacementMainSlotCount(ctx, &inventory, id),
			)
			if err != nil {
				return err
			}
			refresh := make([]int16, 0, len(slots))
			for _, slot := range slots {
				if slot <= math.MaxInt16 {
					refresh = append(refresh, int16(slot))
				}
			}
			refreshes = append(refreshes, runtimeRaidRewardInventoryRefresh{CharacterID: state.ChargedMembers[index], Slots: refresh})
			inventory.UpdatedAt = now
			if err := dnfrepo.SaveInventoryFields(ctx, inventories, inventory, dnfrepo.InventoryFieldSlots); err != nil {
				return err
			}
		}
		if record.Values == nil {
			record.Values = map[string]string{}
		}
		record.Scope = scope
		record.Values[fiendWarRetryReceiptValueKey] = fmt.Sprintf("raid=%d;attempt=%d;started=%d;item=%d;count=%d", state.RaidKey, state.StartAttempt, state.Lifecycle.StartedAt.UnixNano(), state.FiendWarConfig.RetryItem.ItemID, state.FiendWarConfig.RetryItem.Count)
		record.UpdatedAt = now
		return receipts.Save(ctx, record)
	})
	if err == nil && len(refreshes) != 0 {
		err = s.sendRuntimeRaidSettlementInventoryRefreshes(refreshes)
	}
	return err
}

// scheduleRuntimeFiendWarRetry retries the refund once after a second. The
// caller reaches it when the grant itself failed (typically the reward
// repository was momentarily unavailable), so this is a single retry, not a
// loop: a second failure is logged and the attempt keeps its unspent receipt,
// which a later run's callback can still claim.
func (s *Service) scheduleRuntimeFiendWarRetry(state runtimeRaidState, now time.Time) {
	queue := s.ensureGameplayTimeQueue()
	if queue == nil {
		return
	}
	name := fmt.Sprintf("fiendwar-retry:%d:%d", state.RaidKey, state.StartAttempt)
	_ = queue.ScheduleAfter(name, time.Second, func(due time.Time) {
		if err := s.grantRuntimeFiendWarRetry(state, due); err != nil {
			s.logPacketEvent("game-fiendwar-retry-ticket-grant-retry-failed", "raid_key", state.RaidKey, "error", err)
		}
	})
}
