package dnfbridge

import (
	"context"
	"encoding/hex"
	"fmt"
	"strconv"

	"longheng.io/server/internal/modules/dnf/dnfenum"
	dnfequip "longheng.io/server/internal/modules/dnf/equip"
)

const currentDungeonDurabilityReportSize = 1

func isCurrentDungeonDurabilitySlot(slot int16) bool {
	switch slot {
	case 12, 14, 15, 16, 17, 18:
		return true
	default:
		return false
	}
}

// handleDungeonDecreaseDurability owns legacy cmd=1/type=48. The one-byte
// body is the worn slot, not a loss amount or a bitmap. Durability is changed
// only while that character owns an active dungeon runtime.
func (s *Service) handleDungeonDecreaseDurability(session *gameSession, body []byte) error {
	if session == nil {
		return nil
	}
	if len(body) != currentDungeonDurabilityReportSize {
		s.logGameEvent(session, "game-dungeon-durability-blocked",
			"body_len", len(body),
			"reason", "current_exe_op48_body_must_be_one_slot_byte")
		return nil
	}
	slot := int16(body[0])
	if !isCurrentDungeonDurabilitySlot(slot) {
		s.logGameEvent(session, "game-dungeon-durability-blocked",
			"slot", slot,
			"reason", "current_exe_op48_slot_not_durable_equipment")
		return nil
	}

	session.dungeon.mu.Lock()
	runtime := session.dungeon.runtime
	owned := runtime != nil && runtime.Session != nil && runtime.Room != nil &&
		dungeonRuntimeOwnsCharacter(runtime, session.selectedCharacterID)
	session.dungeon.mu.Unlock()
	if !owned {
		s.logGameEvent(session, "game-dungeon-durability-blocked",
			"slot", slot,
			"reason", "active_dungeon_runtime_owner_invalid")
		return nil
	}

	repositories, ok := s.repositoryGroup()
	if !ok || repositories.Equipment == nil {
		s.logGameEvent(session, "game-dungeon-durability-blocked",
			"slot", slot,
			"reason", "equipment_repository_unavailable")
		return nil
	}
	catalog, err := s.currentRepairCostCatalog()
	if err != nil {
		return fmt.Errorf("load durability PVF evidence: %w", err)
	}
	owner, err := dnfequip.NewOwner(repositories)
	if err != nil {
		return err
	}
	ctx, cancel := context.WithTimeout(context.Background(), createWriteTimeout)
	defer cancel()
	result, err := owner.DecreaseDurability(ctx, dnfequip.DecreaseDurabilityCommand{
		SelectedCharacterID: session.selectedCharacterID,
		SlotIndex:           slot,
		Amount:              1,
		Resolve:             catalog.resolve,
	})
	if err != nil {
		return err
	}
	if !result.Changed {
		s.logGameEvent(session, "game-dungeon-durability-ignored",
			"slot", slot,
			"item_id", result.ItemID,
			"durability", result.OldDurability,
			"reason", "slot_missing_zero_or_nonrepairable")
		return nil
	}
	s.logGameEvent(session, "game-dungeon-durability-decreased",
		"slot", result.SlotIndex,
		"item_id", result.ItemID,
		"before", result.OldDurability,
		"after", result.NewDurability,
		"source", "legacy_op48_slot_report")
	if err := s.sendDungeonDurabilitySlotUpdate(
		session,
		result.SlotIndex,
		result.OldDurability,
		result.NewDurability,
	); err != nil {
		s.logPacketEvent("game-upper-dungeon-durability-slot-refresh-failed",
			"conn_id", session.connID,
			"channel_id", session.channel.ID,
			"char_id", session.selectedCharacterID,
			"slot", result.SlotIndex,
			"before", result.OldDurability,
			"after", result.NewDurability,
			"err", err)
		return fmt.Errorf("send dungeon durability slot %d refresh: %w", result.SlotIndex, err)
	}
	// 魔王契约「自动修理」的触发点：耐久被消耗到 0（>0 → 0，而不是本来就 0）。
	// 放在单行 delta 之后：客户端先看到掉到 0，再收到全身修满的变更行。
	// best-effort —— 契约/次数/仓库任何一环出问题都只记日志，不影响 op48 本身。
	s.applyCurrentDevilAutoRepairOnZeroDurability(session, catalog, result)
	return nil
}

// sendDungeonDurabilitySlotUpdate updates the one client-side equipment object
// reported by op48. Sending the complete list-3 table while dungeon objects are
// active crashes this client build, so this path must remain a one-row delta.
func (s *Service) sendDungeonDurabilitySlotUpdate(
	session *gameSession,
	slot int16,
	before uint16,
	after uint16,
) error {
	if session == nil || session.selectedCharacterID == 0 {
		return fmt.Errorf("dungeon durability refresh requires a selected character")
	}
	repositories, ok := s.repositoryGroup()
	if !ok || repositories.Equipment == nil {
		return fmt.Errorf("dungeon durability refresh requires equipment repository")
	}
	ctx, cancel := context.WithTimeout(context.Background(), currentRepositorySnapshotTimeout)
	defer cancel()
	characterID := strconv.Itoa(int(session.selectedCharacterID))
	equipment, found, err := repositories.Equipment.Load(ctx, characterID)
	if err != nil {
		return fmt.Errorf("load character %s equipment: %w", characterID, err)
	}
	if !found {
		return fmt.Errorf("character %s equipment not found", characterID)
	}
	equipped, found := equipment.Entries[strconv.FormatInt(int64(slot), 10)]
	if !found || equipped.SlotIndex != slot {
		return fmt.Errorf("character %s equipment slot %d not found", characterID, slot)
	}
	entry, ok := currentItemListEntryFromEquipment(equipped)
	if !ok {
		return fmt.Errorf("character %s equipment slot %d is not projectable", characterID, slot)
	}
	// op48 reports the current client actor slot. Preserve that exact identity
	// even for historical rows whose bootstrap projection uses legacy numbering.
	entry.setUint16(0, uint16(slot))
	body := buildCurrentItemUpdateBody(currentSocketListEquipment, []currentItemListEntry{entry})
	s.logPacketEvent("game-upper-dungeon-durability-slot-refresh-send",
		"conn_id", session.connID,
		"channel_id", session.channel.ID,
		"source", "dungeon_op48_durability_decreased",
		"char_id", session.selectedCharacterID,
		"msg_id", uint16(dnfenum.CmdPacketWalkoutPartyMember),
		"classification", 0,
		"list_type", currentSocketListEquipment,
		"entry_count", 1,
		"entry_size", currentItemListEntryWireSize,
		"slot", slot,
		"before", before,
		"after", after,
		"body_len", len(body),
		// 崩溃取证用：2026-09-29 三次闪退（char 51 鬼鬼，22:30:37 / 22:32:39 /
		// 22:37:07 +08:00）的崩溃边缘都是"他发 op48 → 我们回这一包"，而这条日志
		// 以前只记 body_len，拿不到字节，无法与客户端 trace 的 traced_body 对照。
		// 只加日志，不改任何发送逻辑。
		"body_hex", hex.EncodeToString(body),
		"sequence", "durability_commit_then_op14_list3_single_changed_row")
	return s.sendGameUpperRawClass(
		session,
		uint16(dnfenum.CmdPacketWalkoutPartyMember),
		body,
		0,
	)
}
