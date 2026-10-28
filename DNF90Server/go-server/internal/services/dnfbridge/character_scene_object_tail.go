package dnfbridge

import (
	"encoding/binary"
	"unicode/utf16"

	dnfrepo "longheng.io/server/internal/modules/dnf/repository"
)

func writeCurrentSceneObjectEntryTail(writer *packetWriter, character dnfrepo.CharacterRecord, hasCharacter bool) {
	writeCurrentSceneObjectEntryTailWithCreature(writer, character, hasCharacter, currentEquippedCreatureSnapshot{})
}

func writeCurrentSceneObjectEntryTailWithCreature(writer *packetWriter, character dnfrepo.CharacterRecord, hasCharacter bool, creature currentEquippedCreatureSnapshot) {
	writeCurrentSceneObjectEntryTailWithCreatureAndAdventureName(writer, character, hasCharacter, creature, "")
}

func writeCurrentSceneObjectEntryTailWithCreatureAndAdventureName(writer *packetWriter, character dnfrepo.CharacterRecord, hasCharacter bool, creature currentEquippedCreatureSnapshot, adventureName string) {
	writeCurrentSceneObjectEntryTailWithCreatureAndNames(writer, character, hasCharacter, creature, adventureName, adventureName)
}

func writeCurrentSceneObjectEntryTailWithCreatureAndNames(writer *packetWriter, character dnfrepo.CharacterRecord, hasCharacter bool, creature currentEquippedCreatureSnapshot, adventureName string, guildName string) {
	job := byte(0)
	grow := byte(0)
	level := byte(0)
	if hasCharacter {
		job = byte(numericCharacterStat(character.Job))
		grow = byte(numericCharacterStatValue(character, "grow_type"))
		level = rosterLevel(character)
	}
	writer.writeByte(job)
	// The current actor tail carries the complete packed grow type: low four
	// bits are the stage and the upper three bits carry the awakening family.
	writer.writeByte(packCurrentSceneObjectGrow(grow&0x0f, (grow>>4)&0x07))
	// Current EXE proof: sub_2003810 stores this byte at template+20. sub_24DC280
	// copies it to actor ability context 0, and the recompute chain copies contexts
	// 0 -> 1 -> 2 -> 3. The level getter sub_2416DB0 reads context 3 offset 0.
	writer.writeByte(level)
	// 下面严格对应 sub_2009160 在 selector 之后的所有 upper_pkt_read_*。
	// 旧 DOVE entry 在这里带版本相关资源号；当前 NoPack 先用安全空状态创建对象，再让后续 359/124 刷新。
	writer.writeByte(0) // sub_20038C0
	writer.writeByte(0) // sub_2003970
	writer.writeByte(0) // v112[1]
	writeCurrentSceneObjectEquipSummary(writer, currentSceneObjectMode0EquipSummary(character, hasCharacter))
	writer.writeUint32(currentCloneTitleItemID(character, hasCharacter)) // v113: clone-title state consumed by the object virtual setter.
	writer.writeByte(0)                                                  // v114
	writer.writeByte(0)                                                  // v115
	writer.writeByte(0)                                                  // v104
	writer.writeByte(0)                                                  // v97
	// Current EXE ReadAndApplyActorNameTagState (sub_2008D80) reads this exact
	// pair and resolves worn endpoint 30. Keep the existing empty sentinel when
	// no name tag is equipped; otherwise project the durable item and expiry.
	nameTagItemID, nameTagExpireTime := currentSceneObjectNameTagState(character, hasCharacter)
	writer.writeUint32(nameTagItemID)
	writer.writeUint32(nameTagExpireTime)
	writer.writeByte(0) // sub_2003A20
	// sub_2003AD0 sets template+736, used by sub_26B0CE0 to resolve membership.
	writer.writeUint32(statU32(character, "__scene_guild_id", 0))
	// Current NoPack sub_2009160 reads this byte immediately before the
	// creature template. The original server writes IsPermissionPrivateStore
	// at the same mode0 position. It creates the idle town trade companion;
	// active opened stores remain separate op78..84 village objects.
	writer.writeByte(boolByte(hasCharacter && character.Stats[currentPrivateStoreScenePermissionStat] > 0)) // v116
	// Current EXE sub_20028B0 reads the equipped creature template ID here.
	// A live trace proved that UINT32_MAX resolves to runtime type 8 and leaves
	// the empty green overhead marker.  The C# no-equipped-creature path writes
	// zero, so do not reuse the DOVE template sentinel for this field.
	creatureItemID := currentSceneNoCreatureTemplateID
	var creatureName []byte
	creatureAliveState := byte(0)
	if creature.valid() {
		creatureItemID = creature.itemID
		creatureName = rosterNameBytes(creature.name)
		creatureAliveState = creature.aliveState
	}
	writer.writeUint32(creatureItemID)
	writer.writeRawDstr(creatureName)
	writer.writeByte(creatureAliveState)
	writer.writeByte(0) // sub_2003B80: premium-PC-room badge
	// Current NoPack sub_2003C90/sub_23EBE50 consumes this dedicated China
	// membership flag. Value 1 selects the cyan actor name and draws the Black
	// Diamond badge; it is not the runtime-PVF premium type.
	writer.writeByte(boolByte(hasCharacter && character.Stats[currentBlackDiamondNameplateStat] > 0))
	writer.writeUint32(0) // v120
	// Current EXE sub_2003D40 consumes the organization name as a DSTR followed
	// immediately by a u32 guild mark (sub_26AE4F0). Adventure-group data lives in the fixed raw actor state above
	// and must not be substituted for the guild name here.
	guildNameBytes := rosterNameBytes(guildName)
	writer.writeRawDstr(guildNameBytes)
	writer.writeUint32(statU32(character, "__scene_guild_mark", 0))
	writer.writeUint32(currentSceneActorStateEventArg) // v121 -> selected actor event 17/6599
	writer.writeByte(0)                                // v122
	writer.writeByte(0)                                // sub_2005B70
	writer.writeByte(0)                                // sub_2005B70
	// sub_2003F20 reads this pair and is the ONLY place the current client fills
	// the sub-profession fields of the actor's profile record:
	//
	//	FUN_034e3970(&type, 1);        // this u8
	//	FUN_034e3a50(&experience);     // this u32
	//	...
	//	*(uint *)(record + 0x368) = type;      // u8 -> the record's +0x368
	//	<encoded u32 setter>(experience);      // u32 -> the encoded slot at +0x370
	//
	// and the early-out above that store only fires when the record's field is
	// already non-zero. +0x368 is the gate the whole profession UI reads through
	// sub_3A4C1A0(): sub_322E590 (the op205 continuation for professions 2 and 4)
	// builds the UI only while it is non-zero, and sub_3A4C1A0-derived readers
	// compare it against 1/2/3. Nothing else in any packet the server sends writes
	// it, which is why the profession row stayed empty while both this pair and
	// the profession state were zero.
	//
	// 礼白666 = 4 (人偶师), 6666 = 2 (炼金术士), 3333 = 3 (分解师), test5 = 1 (附魔师).
	// The experience goes into the same encoded slot the client's own op205
	// experience writer uses (sub_3A4C1A0(actor)+0x36C with the alignment-selected
	// setter writing +0x370), so it must be the durable expert-job experience.
	writer.writeByte(currentSceneExpertJobType(character, hasCharacter))         // sub_2003F20 -> record+0x368
	writer.writeUint32(currentSceneExpertJobExperience(character, hasCharacter)) // sub_2003F20 -> encoded +0x370
	writer.writeByte(0)                                                          // v125
	writer.writeUint32(0)                                                        // v126
	writer.writeUint16(0)                                                        // v128
	writer.writeByte(0)                                                          // sub_2004E00
	writer.writeByte(0)                                                          // sub_2004E00
	writer.writeUint16(0)                                                        // sub_2004E00
	writeCurrentHonorExpertState(writer, character)                              // sub_2003150 HonorExpert {u32 level, u64 progress EXP}
	writer.writeByte(currentSceneNormalTownVisualState)                          // sub_20042F0 normal-town actor visual state
	writer.writeUint32(0)                                                        // sub_20042F0 后的对象 flag dword
	writer.writeByte(0)                                                          // internal field +197
	writer.writeUint16(0)                                                        // sub_2691CF0/sub_2691B70
	writer.writeByte(0)                                                          // sub_34AEA80
	writer.writeUint16(0)                                                        // sub_20029A0
	writer.writeByte(0)                                                          // sub_20029A0
	writer.writeUint16(0)                                                        // sub_2002A20
	writer.writeByte(0)                                                          // sub_2004510
	writer.writeByte(0)                                                          // sub_2690AF0
	writer.writeUint32(0)                                                        // object internal +267
}

func currentSceneObjectNameTagState(character dnfrepo.CharacterRecord, hasCharacter bool) (uint32, uint32) {
	if !hasCharacter {
		return currentSceneNoAttachedUIResourceID, currentSceneNoAttachedUIResourceID
	}
	itemID := statU32(character, "name_tag_item_id", 0)
	if itemID == 0 {
		return currentSceneNoAttachedUIResourceID, currentSceneNoAttachedUIResourceID
	}
	return itemID, statU32(character, "name_tag_expire_time", 0)
}

func currentCloneTitleItemID(character dnfrepo.CharacterRecord, hasCharacter bool) uint32 {
	if !hasCharacter {
		return 0
	}
	return statU32(character, "clone_title_item_id", 0)
}

// currentSceneExpertJobType is the sub-profession type carried by the mode-0 actor
// record (0 = no profession, 1..4 = 附魔师 / 炼金术士 / 分解师 / 人偶师).
//
// Current EXE sub_2009160 -> sub_2003F20 reads it as one u8 and stores it at the
// actor profile record's +0x368; that field is the gate every profession reader
// tests through sub_3A4C1A0() (sub_322E590 for professions 2/4, and the readers
// that compare it against 1/2/3). An out-of-range value would push an unknown
// profession into the client's UI, so anything outside 1..4 is projected as "no
// profession" exactly like a missing stat.
func currentSceneExpertJobType(character dnfrepo.CharacterRecord, hasCharacter bool) byte {
	if !hasCharacter {
		return 0
	}
	value := numericCharacterStatValue(character, "expert_job_type")
	if value <= 0 || value > 4 {
		return 0
	}
	return byte(value)
}

// currentSceneExpertJobExperience is the durable sub-profession experience that
// rides beside the type in the same mode-0 field pair. The client hands it to the
// encoded-u32 setter for sub_3A4C1A0(actor)+0x370, which is the same slot the
// module's op205 experience projection uses.
// currentSceneRecordWithoutExpertJob 返回一份**清掉副职业字段**的角色记录副本，专门给
// "对端 actor"（队友/同屏其他人）的 mode-0 行使用。
//
// 为什么必须清（2026-10-08 现场）：模式0 是客户端唯一填 actor profile 记录 +0x368/+0x370
// 的地方；而对端 actor 在**观看者**的客户端里是"从 0 新建的一条记录"。创建行里带非零副职业
// 字节，客户端就把"别人早就有的副职业"当成**刚刚获得** —— 在对方角色身上播一次获得特效
// 与一次性提示。最清楚的复现：AB 组队，A 先单独回城，B 随后回城 → B 看到 A 身上冒
// "副职业任务完成"特效（本机自己的那条 mode-0 行保持原样，所以 A 自己看不到异常）。
//
// 真正的副职业**变更**仍由 currentSendExpertJobActorRefresh 的对端扇出负责，所以对端状态
// 不会因此变陈旧；被清掉的只是"场景重建时把既有副职业当新获得"的那一份。
func currentSceneRecordWithoutExpertJob(character dnfrepo.CharacterRecord) dnfrepo.CharacterRecord {
	if character.Stats == nil {
		return character
	}
	_, hasType := character.Stats["expert_job_type"]
	_, hasExperience := character.Stats["expert_job_exp"]
	if !hasType && !hasExperience {
		return character
	}
	stats := make(map[string]int64, len(character.Stats))
	for key, value := range character.Stats {
		stats[key] = value
	}
	delete(stats, "expert_job_type")
	delete(stats, "expert_job_exp")
	character.Stats = stats
	return character
}

func currentSceneExpertJobExperience(character dnfrepo.CharacterRecord, hasCharacter bool) uint32 {
	if !hasCharacter {
		return 0
	}
	value := numericCharacterStatValue(character, "expert_job_exp")
	if value <= 0 {
		return 0
	}
	if value > int64(^uint32(0)) {
		return ^uint32(0)
	}
	return uint32(value)
}

func currentSceneObjectMode0EquipSummary(character dnfrepo.CharacterRecord, hasCharacter bool) []dnfrepo.CharacterRosterEquipSummary {
	return currentSceneObjectEquipSummary(character, hasCharacter)
}

func rewriteCurrentSceneObjectTailEquipSummary(tail []byte, rows []dnfrepo.CharacterRosterEquipSummary) ([]byte, bool) {
	const equipOffset = 6
	oldEnd, ok := currentSceneObjectEquipSummaryEnd(tail, equipOffset)
	if !ok {
		return nil, false
	}
	rows = cloneRosterEquipSummary(rows)
	sortRosterEquipSummary(rows)
	var writer packetWriter
	writeCurrentSceneObjectEquipSummary(&writer, rows)
	replacement := writer.bytes()
	out := make([]byte, 0, len(tail)-oldEnd+equipOffset+len(replacement))
	out = append(out, tail[:equipOffset]...)
	out = append(out, replacement...)
	out = append(out, tail[oldEnd:]...)
	return out, true
}

const currentSceneObjectEquipSummaryRowBytes = 19

func currentSceneObjectEquipSummaryEnd(tail []byte, offset int) (int, bool) {
	if offset < 0 || len(tail) <= offset {
		return 0, false
	}
	pos := offset + 1
	count := int(tail[offset])
	for idx := 0; idx < count; idx++ {
		if pos+5 > len(tail) {
			return 0, false
		}
		pos += 5
		if pos+4 > len(tail) {
			return 0, false
		}
		rawLen := int(binary.LittleEndian.Uint32(tail[pos : pos+4]))
		pos += 4
		if rawLen < 0 || pos+rawLen > len(tail) {
			return 0, false
		}
		pos += rawLen
		if pos+10 > len(tail) {
			return 0, false
		}
		pos += 10
	}
	return pos, true
}

func writeCurrentSceneObjectEquipSummary(writer *packetWriter, rows []dnfrepo.CharacterRosterEquipSummary) {
	count := len(rows)
	if count > 0xff {
		count = 0xff
	}
	writer.writeByte(byte(count))
	for idx := 0; idx < count; idx++ {
		row := rows[idx]
		writer.writeByte(rosterByteValue(row.Slot, 0))
		writer.writeUint32(rosterUint32Value(row.ItemIDOrIcon, 0))
		writer.writeUint32(0) // sub_225C9B0(type=44) -> sub_3457C50 u32 len + raw; legacy InvenItem raw is not this current block.
		writer.writeByte(rosterByteValue(row.PackedFlags, 0))
		writer.writeUint32(rosterUint32Value(row.OptionalIDOrExpire, 0))
		writer.writeUint32(rosterUint32Value(row.AuxValue, 0))
		writer.writeByte(rosterByteValue(row.AuxFlag, 0))
	}
}

func packCurrentSceneObjectGrow(grow byte, subGrow byte) byte {
	// MCP sub_2009160：name 后第二个 u8 直接拆成 low4=grow、high3=subgrow；
	// 这里不能复用 roster 的 job|grow 打包，否则会把 job 写进动作/状态链并跳到 0x1818181C。
	return (grow & 0x0f) | ((subGrow & 0x07) << 4)
}

func writeCurrentSceneObjectName(writer *packetWriter, value string) {
	// MCP sub_2009160 -> upper_pkt_read_wstr -> sub_3457C80：
	// 字符串实际是 u32 字节长度 + raw name，再由客户端转宽字；不能写固定 UTF-16，
	// 否则名字长度和后续 job/grow/resource selector 会整体错位，城镇对象会变成“没有名字”。
	writer.writeRawDstr(rosterNameBytes(value))
}

func writeFixedUTF16(writer *packetWriter, value string, units int) {
	encoded := utf16.Encode([]rune(value))
	for idx := 0; idx < units; idx++ {
		var unit uint16
		if idx < len(encoded) {
			unit = encoded[idx]
		}
		writer.writeUint16(unit)
	}
}

func writeNullTerminatedUTF16(writer *packetWriter, value string, maxUnits int) {
	encoded := utf16.Encode([]rune(value))
	if maxUnits > 0 && len(encoded) >= maxUnits {
		encoded = encoded[:maxUnits-1]
	}
	for _, unit := range encoded {
		writer.writeUint16(unit)
	}
	writer.writeUint16(0)
}
