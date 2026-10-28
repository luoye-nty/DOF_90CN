package dnfbridge

import (
	"context"
	"errors"
	"fmt"
	"math"
	"strconv"
	"time"

	dnfdungeonadmission "longheng.io/server/internal/modules/dnf/dungeonadmission"
	dnfrepo "longheng.io/server/internal/modules/dnf/repository"
)

// raidEntryRefundReceiptValueKey is the settings key under which the entry-cost
// refund records that it already returned this attempt's material ticket.
const raidEntryRefundReceiptValueKey = "raid_entry_refund_v1"

// runtimeRaidEntryRefundScope names ONE failed attempt for the durable material
// receipt, the way grantRuntimeFiendWarRetry's scope names one run for the retry
// ticket (raid_fiendwar_retry.go:50): the raid key, the start attempt and the
// lifecycle start instant are each immutable for the attempt, so a replayed
// timeout callback is read-only and a later run can never reuse an earlier
// receipt. Built from state rather than from a counter for that reason.
func runtimeRaidEntryRefundScope(state runtimeRaidState) string {
	return fmt.Sprintf("raid_entry_refund:v1:%08x:%x:%x",
		state.RaidKey, state.StartAttempt, uint64(state.Lifecycle.StartedAt.UnixNano()))
}

// runtimeRaidEntryMaterialRefundOwnedByRetry reports whether this run's entry
// material is already returned by the native retry-ticket path.
//
// 超时空之战 authors the entry ticket and the failed-run retry ticket as the SAME
// item (fiendwar.etc: EntryCost.ItemID == RetryItem.ItemID == 490007844, see
// fiendwar_pvf.go:19-20 and fiendwar_pvf_test.go:63), and
// grantRuntimeFiendWarRetry already returns it, once, under its own durable
// receipt -- on BOTH of this raid's failure paths (raid_phase_runtime.go:1310 and
// raid_fiendwar_battle.go:829). Refunding it again from here would hand out a
// second ticket, so that path owns the material and this one steps aside. With
// [RETRY] turned off in the PVF nothing owns it and the generic refund takes over;
// an owner whose retry grant itself failed keeps owing it through
// scheduleRuntimeFiendWarRetry, which is receipt-guarded too.
func runtimeRaidEntryMaterialRefundOwnedByRetry(state runtimeRaidState) bool {
	return state.RaidMode == runtimeRaidOwnerFiendWar &&
		state.FiendWarConfig != nil && state.FiendWarConfig.Retry
}

// refundRuntimeRaidEntryOnFailure gives back everything one FAILED run charged at
// its start: the three admission counters (帐号日入场场数 / 帐号周入场场数 /
// 角色周入场次数, charged by reserveRuntimeRaidAdmissionCounters) and the entry
// material ticket (consumed by consumeRuntimeRaidEntryCostBatch).
//
// WHY this exists, and why it is a deliberate divergence from the official
// server: the entry is charged at raid start -- charge_point=raid_start_entry_not_clear
// (raid_admission_counter.go:115) -- and 86JP returns none of it, 「回城/失败/放弃都不
// 返还」 (86攻坚流程_对照与下一步_20260916.md:79). The operator's rule for this server
// is narrower and friendlier: a run that FAILS in combat refunds, one the party
// deliberately abandons does not.
//
// ARMED ONLY FOR A GENUINE COMBAT FAILURE, i.e. exactly where the client paints
// the 攻坚失败 banner:
//
//   - 安徒恩 / 卢克 -- the attack-window countdown running out, which is
//     scheduleRuntimeRaidPhaseExpiry's expireRuntimeRaidPhase branch. It is the
//     only failure these two owners have.
//   - 超时空之战 -- that same countdown, plus the battle tick's own wipe-out
//     (battle.Failed in scheduleRuntimeFiendWarTick).
//
// Deliberately NOT armed for abandonRuntimeRaid (中途放弃/解散): the party chose to
// stop and the charge stands. Nor for a raid that never started or never paid --
// the guard below is the whole of that rule, and it is also what keeps this a
// no-op for the many synthetic states in tests.
//
// CALL IT AFTER the failure presentation has been published, never before: the
// counter half ends by re-pushing op1303/op1206 (see the tail of this function),
// and those two packets belong after the banner sequence rather than inside it.
//
// Idempotent twice over, which matters because all three call sites sit behind a
// timer that can fire twice: the counter half addresses durable
// dungeonadmission receipts and Owner.Restore leaves a restored tombstone behind
// (owner.go:408-412), while the material half carries its own per-attempt receipt.
func (s *Service) refundRuntimeRaidEntryOnFailure(state runtimeRaidState, now time.Time) error {
	if s == nil || state.RaidKey == 0 || !state.Started || !state.EntryCostPaid ||
		state.StartAttempt == 0 || len(state.ChargedMembers) == 0 {
		return nil
	}
	if now.IsZero() {
		now = time.Now().UTC()
	} else {
		now = now.UTC()
	}
	materialOwnedByRetry := runtimeRaidEntryMaterialRefundOwnedByRetry(state)
	countersRestored, counterErr := s.refundRuntimeRaidAdmissionCounters(state, now)
	var materialErr error
	if !materialOwnedByRetry {
		materialErr = s.refundRuntimeRaidEntryMaterial(state, now)
	}
	// The counters are the number the player reads back, and the client only
	// re-renders them when a packet arrives -- it never queries them
	// (raid_daily_availability.go:477-500) -- so a refund that does not re-push
	// stays invisible until the next relog. Display only and non-fatal by
	// construction, exactly like the push at the tail of a successful start.
	s.broadcastCurrentRaidEntryCounters(state, "raid_failure_entry_refund")
	err := errors.Join(counterErr, materialErr)
	s.logPacketEvent("game-raid-entry-refunded-on-failure",
		"raid_key", state.RaidKey,
		"raid_mode", state.RaidMode,
		"start_attempt", state.StartAttempt,
		"charged_members", len(state.ChargedMembers),
		"counters_restored", countersRestored,
		"material_refunded", !materialOwnedByRetry,
		"material_owned_by_retry_ticket", materialOwnedByRetry,
		"error", errorText(err))
	return err
}

// refundRuntimeRaidAdmissionCounters decrements the three admission counters of
// every member this run charged, and reports how many members it actually moved
// (as opposed to how many it replayed).
//
// It rebuilds the policies from the same single source of truth the charge used
// (currentRaidAdmissionPolicyKeys), so 「扣什么」 and 「退什么」 cannot drift, and it
// rebuilds each receipt with the same helper the charge committed under
// (runtimeRaidAdmissionReceipt) for the same reason.
//
// Owner.Restore is receipt-addressed and idempotent, and it is the ONLY correct
// primitive here: unlike restoreCurrentDungeonAdmissionCounterBatch it needs no
// in-memory batch, which is exactly what makes a refund possible at all -- the
// batch the start path held went out of scope when the start returned.
//
// One deliberate limitation, inherited from the counter storage rather than
// chosen here: a counter belongs to the service window it was charged in, so a
// run that fails after the 06:00 rollover refunds nothing into the new window.
// That is the right outcome -- the new window never paid.
func (s *Service) refundRuntimeRaidAdmissionCounters(state runtimeRaidState, now time.Time) (int, error) {
	policyKeys := currentRaidAdmissionPolicyKeys(state.RaidMode)
	if len(policyKeys) == 0 {
		return 0, nil
	}
	policies := make([]dnfdungeonadmission.Policy, 0, len(policyKeys))
	for _, policyKey := range policyKeys {
		policy, found, err := s.currentDungeonAdmissionPolicy(policyKey)
		if err != nil {
			return 0, err
		}
		if !found {
			// Same defensive branch as the charge: a catalog without the section
			// was never able to charge this member, so there is nothing to give back.
			continue
		}
		policies = append(policies, policy)
	}
	if len(policies) == 0 {
		return 0, nil
	}
	repositories, ok := s.repositoryGroup()
	if !ok {
		return 0, dnfdungeonadmission.ErrOwnerUnavailable
	}
	owner, err := dnfdungeonadmission.NewOwner(repositories)
	if err != nil {
		return 0, err
	}
	bootNonce, err := s.currentDungeonAdmissionReceiptNonce()
	if err != nil {
		return 0, err
	}
	restored := 0
	var refundErr error
	for _, characterID := range state.ChargedMembers {
		if characterID == 0 {
			continue
		}
		receipt := runtimeRaidAdmissionReceipt(bootNonce, state, state.RaidMode, characterID)
		restoreCtx, cancel := context.WithTimeout(context.Background(), createWriteTimeout)
		snapshots, restoreErr := owner.Restore(restoreCtx, fmt.Sprint(characterID), policies, receipt, now)
		cancel()
		if restoreErr != nil {
			refundErr = errors.Join(refundErr, fmt.Errorf("refund character %d raid admission counter: %w", characterID, restoreErr))
			continue
		}
		// Restored && !Replayed is the only combination that means this call moved a
		// counter: a replay reports Restored as well, because the tombstone it found
		// is itself the proof the counter was given back (owner.go:550-567).
		moved := false
		for _, snapshot := range snapshots {
			if snapshot.Restored && !snapshot.Replayed {
				moved = true
				break
			}
		}
		if moved {
			restored++
		}
		session, _ := s.onlineGameSession(characterID)
		s.logGameEvent(session, "game-raid-admission-counter-refunded",
			"raid_key", state.RaidKey,
			"raid_mode", state.RaidMode,
			"start_attempt", state.StartAttempt,
			"char_id", characterID,
			"policy_count", len(policies),
			"result", map[bool]string{true: "restored", false: "replayed"}[moved],
			"reason", "the_run_failed_in_combat_and_the_entry_count_is_given_back")
	}
	return restored, refundErr
}

// refundRuntimeRaidEntryMaterial hands every charged member the material ticket
// this run took from them, once per attempt.
//
// It is grantRuntimeFiendWarRetry's shape deliberately -- same unit of work, same
// per-member placement bound, same post-commit inventory refresh -- because that
// path is the established way this service returns an item to a whole raid
// roster, and the two must stay interchangeable for 超时空 where they meet.
func (s *Service) refundRuntimeRaidEntryMaterial(state runtimeRaidState, now time.Time) error {
	itemID, count := runtimeRaidEntryCostForState(state)
	if itemID == 0 || count == 0 {
		return nil
	}
	items, err := s.currentPVFItemCatalog()
	if err != nil {
		return err
	}
	definition, err := items.ResolveItem(itemID)
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
		if id == 0 {
			continue
		}
		ids = append(ids, strconv.Itoa(int(id)))
	}
	if len(ids) == 0 {
		return nil
	}
	scope := runtimeRaidEntryRefundScope(state)
	ctx, cancel := context.WithTimeout(context.Background(), createWriteTimeout)
	defer cancel()
	err = repositories.RaidRewards.WithinRaidRewards(ctx, ids, scope, func(inventories dnfrepo.InventoryRepository, receipts dnfrepo.SettingsRepository) error {
		record, found, err := receipts.Load(ctx, scope)
		if err != nil {
			return err
		}
		if found && record.Values[raidEntryRefundReceiptValueKey] != "" {
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
			// 本服务端把"主背包已解锁到第几格"做成了必填参数，漏传会把道具放进客户端
			// 不渲染的扩展格 —— 与 raid_fiendwar_retry.go:73-78、raid_reward_runtime.go
			// 的既有做法一致，按当前角色的放置上限传。
			slots, err := grantCurrentCeraShopProduct(
				&inventory,
				definition,
				count,
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
		record.Values[raidEntryRefundReceiptValueKey] = fmt.Sprintf(
			"raid=%d;attempt=%d;started=%d;item=%d;count=%d",
			state.RaidKey, state.StartAttempt, state.Lifecycle.StartedAt.UnixNano(), itemID, count)
		record.UpdatedAt = now
		return receipts.Save(ctx, record)
	})
	if err == nil && len(refreshes) != 0 {
		err = s.sendRuntimeRaidSettlementInventoryRefreshes(refreshes)
	}
	return err
}

// refundFailedRuntimeRaidEntry is the timer-side entry point used by every
// failure site: run the refund, and when the durable work failed, log it and arm
// the single retry. Kept separate from the refund itself so a call site reads as
// one line and the retry policy lives in exactly one place.
func (s *Service) refundFailedRuntimeRaidEntry(state runtimeRaidState, now time.Time) {
	if err := s.refundRuntimeRaidEntryOnFailure(state, now); err != nil {
		s.logPacketEvent("game-raid-entry-refund-failed",
			"raid_key", state.RaidKey, "start_attempt", state.StartAttempt, "error", err)
		s.scheduleRuntimeRaidEntryRefundRetry(state, now)
	}
}

// scheduleRuntimeRaidEntryRefundRetry retries the whole refund once after a
// second, the way scheduleRuntimeFiendWarRetry retries the retry ticket: the
// callers reach it when the durable work failed (typically a repository that was
// momentarily unavailable), so this is a single retry and not a loop. A second
// failure is logged and the attempt keeps its unspent receipts -- both halves are
// idempotent and addressable by (raid key, attempt, started-at), so a later
// callback can still claim them.
//
// It works from the SNAPSHOT rather than from the live raid row on purpose: the
// FiendWar failure release resets that row to its lobby five seconds later, which
// clears ChargedMembers, and the refund must still be able to pay out.
func (s *Service) scheduleRuntimeRaidEntryRefundRetry(state runtimeRaidState, now time.Time) {
	queue := s.ensureGameplayTimeQueue()
	if queue == nil {
		return
	}
	name := fmt.Sprintf("raid-entry-refund:%d:%d", state.RaidKey, state.StartAttempt)
	_ = queue.ScheduleAfter(name, time.Second, func(due time.Time) {
		if err := s.refundRuntimeRaidEntryOnFailure(state, due); err != nil {
			s.logPacketEvent("game-raid-entry-refund-retry-failed",
				"raid_key", state.RaidKey, "start_attempt", state.StartAttempt, "error", err)
		}
	})
}
