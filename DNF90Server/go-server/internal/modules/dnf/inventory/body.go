// 本文件负责背包类命令的旧客户端回包 body 编码。
// 这里只写 wire 字节顺序，不做背包、金币、宠物或装备业务判定。
package inventory

import (
	"encoding/binary"
	"encoding/hex"
	"sort"
	"strconv"
	"strings"
	"time"

	"google.golang.org/protobuf/encoding/protowire"

	"longheng.io/server/internal/modules/dnf/itemquality"
	dnfrepo "longheng.io/server/internal/modules/dnf/repository"
)

const (
	msgItemListRefresh uint16 = 0x000D
	msgItemListUpdate  uint16 = 0x000E
	msgUpgradeNotice   uint16 = 0x0056

	// premiumActivatedNotifyMsgID is the NOTI 0x0042 premium-activated
	// notification emitted after a contract item activation (86JP
	// PremiumService.ActivateAndNotify).
	premiumActivatedNotifyMsgID uint16 = 0x0042

	currentItemListEntrySize        = 0x77
	currentPetRemainSecondsOffset   = 0x16
	currentItemListExpireTimeOffset = 0x38
	legacyWrongItemExpireTimeOffset = 0x2B
)

type packetWriter struct {
	data []byte
}

func (w *packetWriter) writeByte(value byte) {
	w.data = append(w.data, value)
}

func (w *packetWriter) writeUint16(value uint16) {
	var buf [2]byte
	binary.LittleEndian.PutUint16(buf[:], value)
	w.data = append(w.data, buf[:]...)
}

func (w *packetWriter) writeInt16(value int16) {
	w.writeUint16(uint16(value))
}

func (w *packetWriter) writeUint32(value uint32) {
	var buf [4]byte
	binary.LittleEndian.PutUint32(buf[:], value)
	w.data = append(w.data, buf[:]...)
}

func (w *packetWriter) writeInt32(value int32) {
	w.writeUint32(uint32(value))
}

func (w *packetWriter) writeBytes(value []byte) {
	w.data = append(w.data, value...)
}

func (w *packetWriter) bytes() []byte {
	return append([]byte(nil), w.data...)
}

func buildDeleteItemAck(items []DeletedItem) []byte {
	if len(items) == 0 {
		return nil
	}
	protobuf := protowire.AppendTag(nil, 1, protowire.VarintType)
	protobuf = protowire.AppendVarint(protobuf, uint64(items[0].ListType))
	for _, item := range items {
		nested := protowire.AppendTag(nil, 1, protowire.VarintType)
		nested = protowire.AppendVarint(nested, uint64(item.SlotIndex))
		nested = protowire.AppendTag(nested, 2, protowire.VarintType)
		nested = protowire.AppendVarint(nested, uint64(clampInt32(item.AppliedCount)))
		nested = protowire.AppendTag(nested, 3, protowire.VarintType)
		nested = protowire.AppendVarint(nested, 0)
		protobuf = protowire.AppendTag(protobuf, 2, protowire.BytesType)
		protobuf = protowire.AppendBytes(protobuf, nested)
	}
	protobuf = protowire.AppendTag(protobuf, 3, protowire.VarintType)
	protobuf = protowire.AppendVarint(protobuf, 0)
	return buildCurrentProtobufAck(protobuf)
}

func buildDungeonSkillMaterialDeleteAck(success bool) []byte {
	// Current NoPack sub_1D21830 consumes the result byte, then a u32 protobuf
	// length and PB_ENUM_CMDPACKET_DELETE_ITEM.RETURN. Skill casting already
	// owns the local removal presentation, so both outcomes use a valid empty
	// RETURN. On success the authoritative final quantity follows through op14.
	result := byte(0)
	if success {
		result = 1
	}
	return []byte{result, 4, 0, 0, 0, 0x08, 0, 0x18, 0}
}

// buildDungeonSkillMaterialDeleteAckWithCode 是同一个失败帧，只把**失败原因码**那一格
// 换成具体码。已确认的线上帧形状是 9 字节 `00 04 00000008 001800`：
//
//	[0]   result 字节 (0 = 失败)
//	[1]   失败原因码 (sub_1D21830 的 [ebp+0x10]，查客户端 op18 表；0 = 失败但无原因)
//	[2..5] 小端 u32 = 8，其后是 RETURN 消息的字节
//
// 所以整帧是 10 字节，末尾 8 字节是 RETURN。
func buildDungeonSkillMaterialDeleteAckWithCode(code byte) []byte {
	return []byte{0, code, 8, 0, 0, 0, 0x08, 0, 0x18, 0}
}

func buildMoveItemspaceSuccessAck(cmd Command, moveValue int32) []byte {
	// Current class1/op19 dispatch registers sub_1D2DE80. The class1 result
	// marker is consumed before the handler reads these remaining ten bytes.
	var writer packetWriter
	writer.writeByte(1)
	writer.writeByte(cmd.SourceListType)
	writer.writeInt16(cmd.SourceSlotIndex)
	writer.writeInt32(moveValue)
	writer.writeByte(cmd.DestinationListType)
	writer.writeInt16(cmd.DestinationSlotIndex)
	return writer.bytes()
}

func buildMoveItemspaceFailureAck(cmd Command, errorCode byte) []byte {
	// Current class1/op19 handler sub_1D2DE80 receives the leading result and
	// error bytes through its dispatcher arguments.  Its failure branch then
	// reads exactly two u8 list owners from the remaining packet before releasing
	// the pending move objects.  Supplying both owners avoids the null/stale drag
	// path while deliberately omitting every success-only slot/count field.
	return []byte{0, errorCode, cmd.SourceListType, cmd.DestinationListType}
}

func buildUseStackableSuccessAck(result UseStackableResult) []byte {
	// Current class1/op44 handler sub_1D14380 consumes the result byte before
	// reading u16 slot, u8 list, u32 instance value, and u32 item id.
	var writer packetWriter
	writer.writeByte(1)
	writer.writeInt16(result.SlotIndex)
	writer.writeByte(result.ListType)
	writer.writeInt32(result.InstanceValue)
	writer.writeInt32(int32(result.ItemID))
	return writer.bytes()
}

func buildUseStackableFailureAck(cmd Command, errorCode byte) []byte {
	// Current class1/op44 handler sub_1D14380 consumes the result and error
	// bytes before reading u8 list, u32 instance value, and u32 item id. Its
	// error-17 branch is intentionally silent, but still releases the client's
	// pending item-use state so a stale quick-slot request cannot wedge retries.
	// Failure must not echo the request's opaque key/item pair: the native reader
	// treats two positive values as an additional item-side operation before it
	// releases the list owner.
	var writer packetWriter
	writer.writeByte(0)
	writer.writeByte(errorCode)
	writer.writeByte(cmd.SourceListType)
	writer.writeInt32(0)
	writer.writeInt32(0)
	return writer.bytes()
}

func buildEventServerLevelUpSuccessAck(result UseStackableResult) []byte {
	// Current NoPack class1/op418 handler sub_1D197C0 consumes the generic
	// success byte, then reads u16 slot, u8 list, u32 gained EXP, u16 gained SP,
	// and u16 gained TP. These are deltas from the committed progression plan.
	var writer packetWriter
	writer.writeByte(1)
	writer.writeUint16(uint16(result.SlotIndex))
	writer.writeByte(result.ListType)
	writer.writeInt32(int32(result.DirectExperienceGain))
	writer.writeUint16(uint16(result.DirectExperienceSPGain))
	writer.writeUint16(uint16(result.DirectExperienceTPGain))
	return writer.bytes()
}

func buildEventServerLevelUpFailureAck(errorCode byte) []byte {
	// On failure sub_1D197C0 consumes only the generic result and error bytes.
	return []byte{0, errorCode}
}

// Current NoPack class1/op30 handler sub_0191EDE0 (registered at 0x019368E9)
// reads u16/u8/u32/u16/u16 through the stream readers 0x030E39E0, 0x030E3970
// and 0x030E3A50, but the stream it receives has already consumed the body's
// first byte: that byte is the class1 result flag, exactly as in the op418 ACK
// above. Omitting it printed an SP+5 gain as 5<<24 = 83886080 on a live client.
//
// kind selects the branch, with the live message text as ground truth for
// string 0x303 ("增加%d点SP点数"):
//
//	0x00  SP gain, amount from the u32, HUD gauge 0x01B44720, string 0x303
//	0x11  TP gain, amount from the u32, HUD gauge 0x01B44780, string 0x331C
//	0x01  level-up, the ONLY branch that reads spGain/tpGain, and it also
//	      prints string 0x304 unconditionally
//	0x02..0x09 per-stat lines whose amount is the u32 divided by ten
//	other values miss the branch chain entirely and print nothing
//
// Skill books therefore use kind 0x00/0x11 with the amount in the u32 and both
// u16 gain fields left at zero. A first attempt used kind 1 with the gain in
// spGain and produced both a misaligned number and a spurious level line.
const (
	increaseStatusKindSP byte = 0x00
	increaseStatusKindTP byte = 0x11
	// Experience books have no op30 branch of their own: the client never
	// prints "gained EXP" here. A kind outside the branch chain acknowledges
	// the request silently and releases the pending item state, leaving the
	// experience bar and the level-up presentation to the following op37.
	increaseStatusKindSilent byte = 0xFF

	increaseStatusAckBodySize = 12
)

// buildIncreaseStatusSuccessAck writes the current op30 success body. The u16 is
// the SOURCE ITEM SLOT the client refreshes/consumes, exactly like the slot the
// op44 (buildUseStackableSuccessAck) and op418
// (buildEventServerLevelUpSuccessAck) acknowledgements carry in the same
// position.
//
// Client evidence, class1/op30 handler RVA 0x0191EDE0: it reads u16/u8/u32/u16/
// u16, and immediately passes that u16 to the main-inventory finder
// (sub_1F26860, manager at 0x51B2768). That finder is a plain slot-indexed array
// lookup -- it bounds-checks against (end-begin)/4 and then does
// `mov eax, dword ptr [eax + ecx*4]` -- and the handler consumes/refreshes that
// row for EVERY kind before it branches on kind.
//
// This used to be passed the character id. Character id 1 sits at main-inventory
// slot 1, which is the 复活币 wallet, so every SP/TP book use made the client
// consume mainInventory[1]: the on-screen 复活币 dropped by one per book while
// the server (correctly) never touched it, and the true count came back on the
// next login. Every other character id landed on an empty slot, which is why the
// symptom only ever showed for the character whose id collides with a populated
// slot.
func buildIncreaseStatusSuccessAck(sourceSlot uint16, kind byte, amount uint32) []byte {
	var writer packetWriter
	writer.writeByte(1)
	writer.writeUint16(sourceSlot)
	writer.writeByte(kind)
	writer.writeUint32(amount)
	// spGain/tpGain are read only by the kind==1 level-up branch. They must
	// stay zero here or the client prints an extra level line.
	writer.writeUint16(0)
	writer.writeUint16(0)
	return writer.bytes()
}

// buildIncreaseStatusFailureAck mirrors the op418 failure shape: the class1
// result flag plus one error code. op30 has no visible failure text; the packet
// exists only so the client leaves its pending item-use state. Without it every
// later action reports "请关闭正在使用的其他窗口后再尝试".
func buildIncreaseStatusFailureAck(errorCode byte) []byte {
	return []byte{0, errorCode}
}

func buildUseSharedEffectItemSuccessAck() []byte {
	// Current type251 response processing consumes only the generic result byte;
	// unlike op44 it does not read a slot/list/instance/item tail.
	return []byte{1}
}

func buildUseStackableActionSuccessAck(sourceSlotIndex int16, remainingCount int64) []byte {
	var writer packetWriter
	writer.writeByte(1)
	writer.writeInt16(sourceSlotIndex)
	writer.writeInt16(0)
	writer.writeInt16(int16(remainingCount))
	writer.writeInt16(0)
	writer.writeByte(1)
	return writer.bytes()
}

func buildUseStackableActionFailureAck(errorCode byte) []byte {
	// Current class1/op515 handler sub_1E785B0 receives success and error as
	// dispatcher arguments. Its failure branch reads no command-specific tail,
	// so the exact terminal frame is only the common result/error pair. This
	// must be emitted even when parsing, PVF authorization, or persistence fails
	// so the shared native item-operation controller cannot remain pending.
	return []byte{0, errorCode}
}

func buildSelectDamageFontSuccessAck(fontIndex uint16) []byte {
	var writer packetWriter
	writer.writeByte(1)
	writer.writeUint16(fontIndex)
	return writer.bytes()
}

func buildSelectDamageFontErrorAck(errorCode byte) []byte {
	return []byte{0, errorCode}
}

// buildSortItemAck 对齐 C# SortItemAckBuilder；handler 只在后续 0x000D 列表刷新可生成时使用。
func buildSortItemAck(listType byte) []byte {
	protobuf := protowire.AppendTag(nil, 1, protowire.VarintType)
	protobuf = protowire.AppendVarint(protobuf, uint64(listType))
	protobuf = protowire.AppendTag(protobuf, 2, protowire.VarintType)
	protobuf = protowire.AppendVarint(protobuf, 0)
	return buildCurrentProtobufAck(protobuf)
}

func buildCurrentProtobufAck(protobuf []byte) []byte {
	body := make([]byte, 5, 5+len(protobuf))
	body[0] = 1
	binary.LittleEndian.PutUint32(body[1:5], uint32(len(protobuf)))
	return append(body, protobuf...)
}

// buildCommonItemListRefreshBody 生成 Main/PersonalCargo 的 0x000D 全量列表刷新体。
// 当前 NoPack EXE 的 op13/op14 handler 读取 0x77 字节 entry。
func buildCommonItemListRefreshBodyWithState(listType byte, slots map[string]dnfrepo.ItemStack, state dnfrepo.CharacterContainerState) []byte {
	entries := commonItemListEntries(listType, slots)
	var writer packetWriter
	writer.writeByte(listType)
	switch listType {
	case listTypeMain:
		writer.writeUint16(state.MainSlotCount)
	case listTypeAvatar:
		writer.writeUint16(state.AvatarExpansion)
	case listTypePersonalCargo:
		writer.writeUint16(state.PersonalCargoSlotCount)
	default:
		writer.writeUint16(0)
	}
	writer.writeUint16(uint16(len(entries)))
	for _, entry := range entries {
		writeCommonItemListEntry(&writer, listType, entry.slot, entry.stack)
		if listType == listTypeAvatar {
			// list1 appends two length-prefixed optional blobs after every
			// fixed 0x77-byte row. Their payload formats are not yet owned;
			// two zero lengths preserve the current constructor defaults.
			writer.writeInt32(0)
			writer.writeInt32(0)
		}
	}
	if listType == listTypePersonalCargo {
		// Current sub_1D72380 requires a final u8 groupCount followed by
		// groupCount*raw[8]. The repository has no group owner yet, so emit
		// the explicit empty terminator instead of leaving the reader short.
		writer.writeByte(0)
	}
	return writer.bytes()
}

// buildCommonItemListUpdateBody encodes current class0/op14: list type,
// changed-row count, then fixed 0x77-byte current item rows.
func buildCommonItemListUpdateBody(listType byte, entries []commonItemListEntry) []byte {
	var writer packetWriter
	writer.writeByte(listType)
	writer.writeUint16(uint16(len(entries)))
	for _, entry := range entries {
		writeCommonItemListEntry(&writer, listType, entry.slot, entry.stack)
	}
	return writer.bytes()
}

func buildPetItemListUpdateBody(entries []commonItemListEntry) []byte {
	var writer packetWriter
	writer.writeByte(listTypePet)
	writer.writeUint16(uint16(len(entries)))
	for _, entry := range entries {
		writePetItemListEntry(&writer, entry.slot, entry.stack)
	}
	return writer.bytes()
}

func currentRawEntryForStack(slot int16, stack dnfrepo.ItemStack) []byte {
	var writer packetWriter
	writeCommonItemListEntry(&writer, listTypeMain, slot, stack)
	return writer.bytes()
}

// buildPetItemListRefreshBody 生成 Pet 背包的 0x000D 全量列表刷新体。
// 当前 NoPack EXE 的 pet list 同样读取 0x77 字节 entry。
func buildPetItemListRefreshBody(slots map[string]dnfrepo.ItemStack) []byte {
	entries := commonItemListEntries(listTypePet, slots)
	var writer packetWriter
	writer.writeByte(listTypePet)
	writer.writeUint16(uint16(len(entries)))
	for _, entry := range entries {
		writePetItemListEntry(&writer, entry.slot, entry.stack)
	}
	return writer.bytes()
}

// BuildPetItemListRefreshBody exposes the single current-EXE list-7 encoder
// to the pet owner. Keeping one 0x77-row implementation prevents hatch and
// normal inventory refreshes from drifting to different wire layouts.
func BuildPetItemListRefreshBody(slots map[string]dnfrepo.ItemStack) []byte {
	return buildPetItemListRefreshBody(slots)
}

func buildSellItemAck(item DeletedItem, updatedGold int64) []byte {
	var writer packetWriter
	writer.writeByte(1)
	writer.writeInt32(clampInt32(updatedGold))
	writer.writeByte(item.ListType)
	writer.writeInt16(item.SlotIndex)
	writer.writeInt16(clampInt16(item.AppliedCount))
	return writer.bytes()
}

// buildRepairEquipmentAck 对齐 C# RepairEquipmentAckBuilder：成功标记、当前金币、原 invenType、slot 和尾部 u16。
func buildRepairEquipmentAck(invenType byte, slotIndex int16, updatedGold int64) []byte {
	var writer packetWriter
	writer.writeByte(1)
	writer.writeInt32(clampInt32(updatedGold))
	writer.writeByte(invenType)
	writer.writeInt16(slotIndex)
	writer.writeInt16(0)
	return writer.bytes()
}

func buildUpgradeItemSuccessAck(result UpgradeResult) []byte {
	var writer packetWriter
	writer.writeByte(1)
	writer.writeByte(upgradeModeByte(result.Mode))
	writer.writeInt16(result.MaterialSlotIndex)
	writer.writeInt32(clampInt32(result.MaterialRemainingStackCount))
	writer.writeInt16(result.OptionalTicketSlotIndex)
	writer.writeByte(0)
	writer.writeByte(result.OldLevel)
	writer.writeByte(result.ResultCode)
	writer.writeByte(result.NewLevel)
	// byte[14] is the target list selector, not a pad byte: the client repaints
	// the slot in the list named here. The 86JP builder's constant 0 happened to
	// be right for backpack targets and made a worn-item upgrade repaint the
	// same slot index in the backpack instead of the equipped row.
	writer.writeByte(result.TargetListType)
	writer.writeInt16(result.TargetSlotIndex)
	writer.writeInt16(result.OptionalTicketSlotIndex)
	if result.ResultCode == upgradeResultCodeDestroy {
		// u8 row count, then (i16 slot, i32 item, i32 count) per granted row:
		// the compensation item first, then any fixed extra refunds (usdof
		// opcode0050 build_upgrade_item_success destroy_rewards).
		rows := make([]UpgradeDestroyGrantedReward, 0, 1+len(result.DestroyExtraRewards))
		if result.DestroyBonusItemID > 0 {
			rows = append(rows, UpgradeDestroyGrantedReward{Slot: result.DestroyBonusSlot, ItemID: result.DestroyBonusItemID, Count: result.DestroyBonusCount})
		}
		rows = append(rows, result.DestroyExtraRewards...)
		if len(rows) > 0xff {
			rows = rows[:0xff]
		}
		writer.writeByte(byte(len(rows)))
		for _, row := range rows {
			writer.writeInt16(row.Slot)
			writer.writeInt32(row.ItemID)
			writer.writeInt32(row.Count)
		}
	}
	if result.TargetListType == listTypeGuildMedal {
		// The 公会徽章 window reads a trailing (i16 -1, i32 0) after the common
		// prefix: no destroy-compensation slot and no material remainder are
		// shown for a flag. Upstream writes it only for list 38.
		writer.writeInt16(-1)
		writer.writeInt32(0)
	}
	return writer.bytes()
}

func buildUpgradeItemErrorAck(errorCode byte) []byte {
	return []byte{0, errorCode}
}

// buildForgingResultAck matches the command-response wrapper and callback
// registered for current NoPack op438. The wrapper consumes the leading 1;
// the callback then reads the remaining 12 bytes. The forging outcome is 0
// for success and 1 for failure.
func buildForgingResultAck(result ForgingResult) []byte {
	var writer packetWriter
	writer.writeByte(1)
	writer.writeInt16(result.MaterialSlotIndex)
	writer.writeInt32(clampInt32(result.MaterialRemainingCount))
	writer.writeByte(result.OldLevel)
	if result.UpgradeSucceeded {
		writer.writeByte(0)
	} else {
		writer.writeByte(1)
	}
	writer.writeByte(result.NewLevel)
	writer.writeByte(result.TargetListType)
	writer.writeInt16(result.TargetSlotIndex)
	return writer.bytes()
}

func buildForgingErrorAck(errorCode byte) []byte {
	return []byte{0, errorCode}
}

// buildChronicleRefineResultAck is the current client's complete op370 result
// body. A chance failure is still a handled attempt; trailing rows describe the
// two granted destruction rewards.
func buildChronicleRefineResultAck(result ChronicleRefineResult) []byte {
	var writer packetWriter
	writer.writeByte(1)
	writer.writeInt16(result.MaterialSlotIndex)
	writer.writeUint16(clampUint16(result.MaterialRemainingCount))
	if result.RefineSucceeded {
		writer.writeByte(1)
	} else {
		writer.writeByte(0)
	}
	writer.writeByte(0)
	writer.writeInt16(result.TargetSlotIndex)
	rewardCount := len(result.FailureRewards)
	if rewardCount > 255 {
		rewardCount = 255
	}
	writer.writeByte(byte(rewardCount))
	for index := 0; index < rewardCount; index++ {
		reward := result.FailureRewards[index]
		writer.writeInt16(reward.SlotIndex)
		writer.writeInt32(clampInt32(reward.ItemID))
		writer.writeInt32(clampInt32(reward.Count))
	}
	return writer.bytes()
}

func buildChronicleRefineErrorAck(errorCode byte) []byte {
	return []byte{0, errorCode}
}

// buildForgingNoticeBody is current NoPack op86 subtype 14:
// success, town actor ID, weapon item ID, reinforce level, forging level.
func buildForgingNoticeBody(success bool, userID uint16, itemID int32, reinforceLevel, forgingLevel byte) []byte {
	var writer packetWriter
	writer.writeByte(14)
	if success {
		writer.writeByte(1)
	} else {
		writer.writeByte(0)
	}
	writer.writeUint16(userID)
	writer.writeInt32(itemID)
	writer.writeByte(reinforceLevel)
	writer.writeByte(forgingLevel)
	return writer.bytes()
}

// buildUpgradeNoticeBody builds NOTI 0x0056 subtype 1 upgrade announcement.
// The current client resolves userID against the active town actor table and
// suppresses the chat message when that lookup fails.
// Layout: u8 subtype, u8 successFlag, u16 userID, i32 itemID, u8 level.
func buildUpgradeNoticeBody(success bool, userID uint16, itemID int32, level byte) []byte {
	var writer packetWriter
	writer.writeByte(1) // subtype
	if success {
		writer.writeByte(1)
	} else {
		writer.writeByte(0)
	}
	writer.writeUint16(userID)
	writer.writeInt32(itemID)
	writer.writeByte(level)
	return writer.bytes()
}

// buildUpgradeTicketSuccessAck matches the 86JP ItemUpgradeAckBuilder layout
// consumed by the current EXE op50 handler: u8 0x01, u8 mode, i16 material
// slot, i32 material remaining, i16 optional ticket slot (always -1), u8 0,
// u8 old level, u8 result code, u8 new level, u8 target list type (byte[14],
// a list selector the client repaints - see buildUpgradeItemSuccessAck), i16
// target slot, i16 optional ticket slot (always -1).
func buildUpgradeTicketSuccessAck(result UpgradeTicketResult) []byte {
	var writer packetWriter
	writer.writeByte(1)
	writer.writeByte(upgradeModeByte(result.Mode))
	writer.writeInt16(result.MaterialSlotIndex)
	writer.writeInt32(clampInt32(result.MaterialRemainingStackCount))
	writer.writeInt16(-1)
	writer.writeByte(0)
	writer.writeByte(result.OldLevel)
	writer.writeByte(result.ResultCode)
	writer.writeByte(result.NewLevel)
	writer.writeByte(result.TargetListType)
	writer.writeInt16(result.TargetSlotIndex)
	writer.writeInt16(-1)
	return writer.bytes()
}

func buildUpgradeTicketErrorAck(errorCode byte) []byte {
	return []byte{0, errorCode}
}

// buildEnchantByBeadSuccessAck matches the current EXE S2C 0x0110 success
// reader (sub_1D0B960): u8 nonzero flag, u8 target list type, i16 target
// slot. The reference 86JP server emits the identical four-byte body.
//
// 2026-09-13 现场实验记录：把这里改成 3 字节 [flag][slot u16]（依据是
// sub_30E3970/sub_30E39E0 的 u8+u16 读取）之后，客户端连"附魔成功"弹窗都不再
// 出现，账号晶体/灵魂面板依旧被清空，说明 sub_1D0B960 实际读取的字段不止这三个
// 字节、或调用方对包长有约束。该实验已回滚，恢复 4 字节形状；下一次改动前必须
// 先把 sub_1D0B960 两条分支完整反汇编，确认逐字段布局。
func buildEnchantByBeadSuccessAck(result EnchantResult) []byte {
	return []byte{
		1,
		result.TargetListType,
		byte(result.TargetSlotIndex),
		byte(result.TargetSlotIndex >> 8),
	}
}

func buildEnchantByBeadErrorAck(errorCode byte) []byte {
	return []byte{0, errorCode}
}

// buildMonsterCardUpgradeSuccessAck matches current NoPack op603 and the
// retained native card-upgrade reader: generic success, roll result, target
// item id, then the resulting enchant-table index.
func buildMonsterCardUpgradeSuccessAck(result MonsterCardUpgradeResult) []byte {
	var writer packetWriter
	writer.writeByte(1)
	if result.UpgradeSucceeded {
		writer.writeByte(1)
	} else {
		writer.writeByte(0)
	}
	writer.writeInt32(int32(result.TargetItemID))
	writer.writeByte(result.UpgradeCount)
	return writer.bytes()
}

func buildMonsterCardUpgradeErrorAck(errorCode byte) []byte {
	return []byte{0, errorCode}
}

func upgradeModeByte(mode string) byte {
	if strings.EqualFold(strings.TrimSpace(mode), "amplify") {
		return 1
	}
	return 0
}

type commonItemListEntry struct {
	slot  int16
	stack dnfrepo.ItemStack
}

func commonItemListEntries(listType byte, slots map[string]dnfrepo.ItemStack) []commonItemListEntry {
	entries := make([]commonItemListEntry, 0, len(slots))
	for key, stack := range slots {
		keyListType, slot, ok := parseSlotKey(key)
		if !ok || keyListType != listType {
			continue
		}
		entries = append(entries, commonItemListEntry{slot: slot, stack: stack})
	}
	sort.Slice(entries, func(i, j int) bool {
		return entries[i].slot < entries[j].slot
	})
	return entries
}

// commonItemListRowAmount resolves the u32 the current 0x77 row carries at
// raw+0x06 for one class0 item row.
//
// For an equipment row in any container the client reads grade from -- backpack
// 0, personal cargo 2, worn 3, account cargo 12 -- that word is the instance's
// durable quality seed (品级), never a stack count: an equipment row always
// stores Count 1, and the client derives the displayed grade from this word.
// Resolving it through itemquality.StackSeed keeps these re-encoded rows
// bit-identical to dnfbridge's login/full-list projection, which is the
// authority the client re-reads on select-character. Writing the count here
// made an enchant's 0x000D main-list refresh repaint the whole bag as seed 1
// (最下级) until the next character load.
//
// The container set must stay identical to dnfbridge's
// currentItemListEquipmentContainer. They diverged once already: this encoder
// listed only 0/3 while the bridge listed only 0/3, so equipment parked in
// personal cargo (list 2) went out with +0x06 = Count = 1 and the client
// repainted it with the grade of seed 1 after re-select. A row whose grade is
// only in Extra["quality_seed"] (everything itemquality.Apply creates) shows
// it; rows that also mirror the seed into amount_or_count hid it because the
// fallback below happens to read that alias.
//
// Avatar 1 and guild medal 38 are deliberately absent: their rows carry 1 at
// +0x06 by design. Pet list 7 has its own encoder. Every other list type keeps
// the historical amount aliases, because that is exactly what its own login
// projection encodes.
func commonItemListRowAmount(listType byte, stack dnfrepo.ItemStack) int64 {
	switch listType {
	case listTypeMain, listTypePersonalCargo, listTypeEquipment, listTypeAccountCargo:
		if seed := itemquality.StackSeed(stack); seed != 0 {
			return int64(seed)
		}
	}
	return firstExtraInt(stack.Extra, stack.Count, "amount_or_count", "amount", "count", "stack", "quantity")
}

func writeCommonItemListEntry(writer *packetWriter, listType byte, slot int16, stack dnfrepo.ItemStack) {
	extra := stack.Extra
	// The character wallet is main-list slot 0 with item id 0 and the balance
	// in the count field (dnfbridge's currentGoldWalletItemListEntry, see also
	// currentGoldWalletReservedSlot). It is the one legitimate zero-id row, so
	// it must never be rewritten into the "slot is empty" marker below.
	goldWallet := slot == 0 && stack.ItemID == 0 && stack.Count > 0
	if stack.ItemID <= 0 && !goldWallet {
		// An empty stack means "this slot is now empty", and the current EXE
		// only accepts that as id -1. Its op14 handler sub_1D73120 tests the
		// row's id against -1 at 0x1D73912 and calls RemoveItem; any other id
		// -- including 0 -- falls through to the in-place update path, which
		// at 0x1D73AB2 refuses to change an object's item id unless the object
		// type is 2. Equipment is not type 2, so a 0 row was silently dropped
		// and the client kept painting the old piece until the next full list
		// refresh. The bridge-side refresh (current_item_list_send.go) has
		// always written math.MaxUint32 here, and the enchant path already
		// spells it ItemStack{ItemID: -1}; this just makes a zero-value stack
		// mean the same thing instead of emitting id 0.
		writer.writeBytes(makeCurrentItemListEntry(slot, -1, 0))
		return
	}
	entry := makeCurrentItemListEntry(
		slot,
		stack.ItemID,
		commonItemListRowAmount(listType, stack),
		stack.RawEntry,
	)
	entry[0x0A] = clampByte(firstExtraInt(extra, 0, "ext_data0", "ext0", "packed_flag_byte", "packed_flag", "packed"))
	binary.LittleEndian.PutUint16(entry[0x0B:0x0D], clampUint16(firstExtraInt(extra, 0, "durability", "max_durability")))
	entry[0x0D] = clampByte(firstExtraInt(extra, boolInt64(stack.Bind), "seal_flag", "seal", "bind_flag", "bind"))
	binary.LittleEndian.PutUint32(entry[0x0E:0x12], uint32(clampInt32(commonItemListStackValueA(stack))))
	entry[0x12] = clampByte(firstExtraInt(extra, 0, "byte_12", "value_12"))
	entry[0x13] = clampByte(firstExtraInt(extra, 0, "byte_13", "value_13", "value_c"))
	binary.LittleEndian.PutUint16(entry[0x14:0x16], clampUint16(firstExtraInt(extra, 0, "marker_16", "marker16", "value_d")))
	if expire := itemListStackExpireUnix(stack); expire != 0 {
		if binary.LittleEndian.Uint32(entry[legacyWrongItemExpireTimeOffset:legacyWrongItemExpireTimeOffset+4]) == expire {
			binary.LittleEndian.PutUint32(entry[legacyWrongItemExpireTimeOffset:legacyWrongItemExpireTimeOffset+4], 0)
		}
		binary.LittleEndian.PutUint32(entry[currentItemListExpireTimeOffset:currentItemListExpireTimeOffset+4], expire)
	}
	entry[currentForgingRawOffset] = clampByte(firstExtraInt(extra, int64(entry[currentForgingRawOffset]), "forging_level", "byte_57", "value_57"))
	entry[0x58] = clampByte(firstExtraInt(extra, 0, "byte_58", "value_58"))
	entry[0x59] = clampByte(firstExtraInt(extra, 0, "byte_59", "value_59"))
	copy(entry[0x65:0x6E], fixedExtraBytes(extra, 9, "raw_data_65", "raw65"))
	copy(entry[0x72:0x77], fixedExtraBytes(extra, 5, "tail_data_72", "tail72"))
	writer.writeBytes(entry)
}

func commonItemListStackValueA(stack dnfrepo.ItemStack) int64 {
	if value := firstExtraInt(stack.Extra, 0, "value_a", "instance_value", "item_uid", "serial", "count_or_instance"); value != 0 {
		return value
	}
	// Keep op14 row refreshes aligned with dnfbridge's login/full-list
	// projection: current op44 requires a nonzero raw+0x0E identity before the
	// client will emit a right-click request. Imported stackables without an
	// explicit historical identity use their stable item id on the wire.
	if strings.EqualFold(strings.TrimSpace(stack.Extra["item_kind"]), "stackable") && stack.ItemID > 0 {
		return stack.ItemID
	}
	return 0
}

func writePetItemListEntry(writer *packetWriter, slot int16, stack dnfrepo.ItemStack) {
	extra := stack.Extra
	entry := makeCurrentItemListEntry(
		slot,
		stack.ItemID,
		firstExtraInt(extra, stack.Count, "creature_serial_or_handle", "serial", "handle", "instance_value", "item_uid"),
		stack.RawEntry,
	)
	entry[0x0A] = clampByte(firstExtraInt(extra, 0, "ext_data0", "ext0", "packed_flag_byte", "packed_flag", "packed"))
	binary.LittleEndian.PutUint16(entry[0x0B:0x0D], clampUint16(firstExtraInt(extra, 0, "durability", "max_durability")))
	entry[0x0D] = clampByte(firstExtraInt(extra, boolInt64(stack.Bind), "seal_flag", "seal", "bind_flag", "bind"))
	binary.LittleEndian.PutUint32(entry[0x0E:0x12], uint32(clampInt32(firstExtraInt(extra, 0, "value_a"))))
	copy(entry[0x12:0x77], fixedExtraBytes(extra, currentItemListEntrySize-0x12, "tail_data_12", "tail12", "pet_tail_12"))
	entry[0x12] = clampByte(firstExtraInt(extra, 0, "byte_12", "value_12", "enchant_upgrade_count", "pet_enchant_upgrade_count"))
	expire := itemListStackExpireUnix(stack)
	binary.LittleEndian.PutUint32(
		entry[currentPetRemainSecondsOffset:currentPetRemainSecondsOffset+4],
		petRemainingSecondsAt(expire, time.Now().UTC()),
	)
	if expire != 0 {
		if binary.LittleEndian.Uint32(entry[legacyWrongItemExpireTimeOffset:legacyWrongItemExpireTimeOffset+4]) == expire {
			binary.LittleEndian.PutUint32(entry[legacyWrongItemExpireTimeOffset:legacyWrongItemExpireTimeOffset+4], 0)
		}
		binary.LittleEndian.PutUint32(entry[currentItemListExpireTimeOffset:currentItemListExpireTimeOffset+4], expire)
	}
	writer.writeBytes(entry)
}

func petRemainingSecondsAt(expire uint32, now time.Time) uint32 {
	if expire == 0 {
		return 0
	}
	if now.IsZero() {
		now = time.Now().UTC()
	}
	remaining := int64(expire) - now.Unix()
	if remaining <= 0 {
		return 0
	}
	return uint32(remaining)
}

func itemListStackExpireUnix(stack dnfrepo.ItemStack) uint32 {
	if value := firstExtraInt(stack.Extra, 0, "expire_time", "expire_unix"); value > 0 {
		return uint32(clampInt32(value))
	}
	if !stack.ExpireAt.IsZero() && stack.ExpireAt.Unix() > 0 {
		return uint32(clampInt32(stack.ExpireAt.Unix()))
	}
	return 0
}

func makeCurrentItemListEntry(slot int16, itemID int64, amount int64, rawEntries ...[]byte) []byte {
	entry := make([]byte, currentItemListEntrySize)
	if len(rawEntries) > 0 && len(rawEntries[0]) == currentItemListEntrySize {
		copy(entry, rawEntries[0])
	}
	binary.LittleEndian.PutUint16(entry[0x00:0x02], uint16(slot))
	binary.LittleEndian.PutUint32(entry[0x02:0x06], uint32(clampInt32(itemID)))
	binary.LittleEndian.PutUint32(entry[0x06:0x0A], uint32(clampInt32(amount)))
	return entry
}

func boolInt64(value bool) int64 {
	if value {
		return 1
	}
	return 0
}

func firstExtraInt(extra map[string]string, fallback int64, keys ...string) int64 {
	for _, key := range keys {
		raw := strings.TrimSpace(extra[key])
		if raw == "" {
			continue
		}
		value, err := strconv.ParseInt(raw, 0, 64)
		if err == nil {
			return value
		}
	}
	return fallback
}

func fixedExtraBytes(extra map[string]string, length int, keys ...string) []byte {
	for _, key := range keys {
		raw := strings.TrimSpace(extra[key])
		if raw == "" {
			continue
		}
		raw = strings.NewReplacer(" ", "", "\t", "", "\r", "", "\n", "", "0x", "", "0X", "").Replace(raw)
		decoded, err := hex.DecodeString(raw)
		if err != nil || len(decoded) == 0 {
			continue
		}
		out := make([]byte, length)
		copy(out, decoded)
		return out
	}
	return nil
}

func clampInt32(value int64) int32 {
	if value > 2147483647 {
		return 2147483647
	}
	if value < -2147483648 {
		return -2147483648
	}
	return int32(value)
}

func clampByte(value int64) byte {
	if value < 0 {
		return 0
	}
	if value > 255 {
		return 255
	}
	return byte(value)
}

func clampUint16(value int64) uint16 {
	if value < 0 {
		return 0
	}
	if value > 65535 {
		return 65535
	}
	return uint16(value)
}

func clampInt16(value int64) int16 {
	if value > 32767 {
		return 32767
	}
	if value < -32768 {
		return -32768
	}
	return int16(value)
}
