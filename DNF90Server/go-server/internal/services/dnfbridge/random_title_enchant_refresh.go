package dnfbridge

import (
	"context"
	"strconv"

	"longheng.io/server/internal/modules/dnf/dnfenum"
	dnfrepo "longheng.io/server/internal/modules/dnf/repository"
)

// 本文件补上「穿戴中的称号随机附魔」的权威行重放，修的是 2026-09-25 现场
// 「称号附魔后一进图附魔与技能等级一起消失，小退重上仍然看不见」。
//
// 取证链（全部有据可查，不是猜测）：
//
//  1. 附魔在库里一直是好的：dnf_equipment_entries 槽 13 的 raw_entry +0x61..+0x65 =
//     config/row，extras 里 random_title_enchant_config_id / _row_id 同时存在。
//  2. mode1/mode3 的**创建行**不携带那一段：writeCurrentMode1EquipmentCreateRow 只读
//     [0x0A,0x16)、[0x38,0x58)、[0x72,0x77)，而 +0x61..+0x65 落在 [0x58,0x72)。详见
//     currentMode1EquipmentRowNeedsDeferredStateRow 的注释。
//  3. mode1 的延时更新行（sub_2007B00）**实测客户端不消费**：2026-09-25 11:00 之后每个
//     mode1 的装备投影都已 deferred_update_count=1，报体里也确实出现了
//     "00 03 0D 00" + 11 27 25 00（config 10001 / row 37），客户端依旧不显示附魔。
//     所以那条通路对这个问题无效，不能只靠它。
//  4. 本部署从不单独下发穿戴栏 list-3 行（全量 packet_log 里
//     game-aligned-command-equipment-slot-refresh-send /
//     game-upper-current-equipment-slot-items-refresh-send /
//     game-upper-current-equipment-item-update-send 三个事件各 0 次），所以客户端手上那件
//     穿戴称号永远拿不到 +0x61..+0x65。
//
// 于是这里用**守护者宝石**早就落地并写明"客户端已验证会应用"的那条通路补包：在 mode1
// 之后、actor 已经建立之后，把这条穿戴行的完整 0x77 行用 list-3 op14 单独重放一次。
// 宝石的 socket 字在 +0x65..+0x6C，与称号附魔紧挨着、落在同一个缺口里，它的注释原文是
// "creation rows … do not carry generic raw 0x77 offsets" 与 "the current client has already
// proved it applies a following list-3 op14 row to the worn medal"；称号附魔以前谁都没补。
//
// 选行标准只有一条：这条**投影出来的 0x77 行**是否带着完整的随机称号附魔
// （config 与 row 都非零，见 currentItemListEntryCarriesRandomTitleEnchant）。因此没有附魔
// 的称号与其它任何装备都不会被这条通路碰到——全库今天只有 1 行命中。
//
// 与 mode1 延时更新行的关系：那条保留（数据正确、对会读它的客户端有用），是"同一份正确
// 数据多一条通路"；本文件这条是实测客户端会应用的那条。两条都可以单独回退。

// currentWornTitleEnchantActorSlot 报告这条穿戴行是否需要在 mode1 之后重放权威行，
// 需要时返回它在该客户端 actor 上的槽位号。
func currentWornTitleEnchantActorSlot(equipped dnfrepo.EquipmentEntry) (int16, bool) {
	entry, ok := currentItemListEntryFromEquipment(equipped)
	if !ok || !currentItemListEntryCarriesRandomTitleEnchant(entry) {
		return 0, false
	}
	actorSlot, ok := currentEXEActorEquipmentSlot(equipped)
	if !ok || actorSlot > 0xff {
		return 0, false
	}
	return int16(actorSlot), true
}

// sendSelectedWornTitleEnchantRowRefresh 在 actor 生成边界（mode1 之后）把带随机称号附魔的
// 穿戴行用 list-3 op14 重放一次。没有命中的槽位时一个包都不发。
func (s *Service) sendSelectedWornTitleEnchantRowRefresh(session *gameSession, source string) error {
	if session == nil || session.selectedCharacterID == 0 {
		return nil
	}
	repositories, ok := s.repositoryGroup()
	if !ok || repositories.Equipment == nil {
		return nil
	}
	ctx, cancel := context.WithTimeout(context.Background(), createWriteTimeout)
	defer cancel()
	characterID := strconv.Itoa(int(session.selectedCharacterID))
	equipment, found, err := repositories.Equipment.Load(ctx, characterID)
	if err != nil {
		return err
	}
	if !found {
		return nil
	}
	for _, equipped := range equipment.Entries {
		actorSlot, needsReplay := currentWornTitleEnchantActorSlot(equipped)
		if !needsReplay {
			continue
		}
		entry, ok := currentItemListEntryFromEquipment(equipped)
		if !ok {
			continue
		}
		// 与宝石重放同形：body = [list_type][u16][119 字节 0x77 行]，走 op14 载体。
		entry.setUint16(0, uint16(actorSlot))
		body := buildCurrentItemUpdateBody(currentSocketListEquipment, []currentItemListEntry{entry})
		s.logGameEvent(session, "game-current-worn-title-enchant-row-refresh",
			"source", source,
			"msg_id", uint16(dnfenum.CmdPacketWalkoutPartyMember),
			"list_type", currentSocketListEquipment,
			"actor_slot", actorSlot,
			"item_id", equipped.ItemID,
			"body_len", len(body),
			"reason", "mode1_create_rows_do_not_carry_the_raw_title_enchant")
		if err := s.sendGameUpperRawClass(
			session,
			uint16(dnfenum.CmdPacketWalkoutPartyMember),
			body,
			0,
		); err != nil {
			return err
		}
	}
	return nil
}
