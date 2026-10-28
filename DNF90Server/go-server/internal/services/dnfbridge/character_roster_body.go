package dnfbridge

import (
	"encoding/binary"

	dnfrepo "longheng.io/server/internal/modules/dnf/repository"
)

func buildRosterSlots(characters []dnfrepo.CharacterRecord) []byte {
	// 当前客户端默认只开�?8 个角色槽，额外槽位必须由道具/扩展栏位请求链路触发�
	var writer packetWriter
	writer.writeByte(2)
	writer.writeByte(latestRosterRouteNormal)
	writer.writeByte(latestRosterContextNormal)
	writer.writeUint16(defaultCharacterSlots)
	writer.writeUint16(0)
	writer.writeUint16(0)
	writer.writeUint32(0)
	writer.writeUint16(0)
	writer.writeByte(0)
	writer.writeByte(0)
	writer.writeUint32(0)
	writer.writeUint32(0)
	return writer.bytes()
}

func buildRosterChars(characters []dnfrepo.CharacterRecord) []byte {
	var writer packetWriter
	count := rosterCount(characters)
	writer.writeByte(3)
	writer.writeUint16(uint16(count))
	for idx, character := range characters {
		if idx >= count {
			break
		}
		writer.writeByte(latestRosterRouteNormal)
		writer.writeByte(latestRosterContextNormal)
		writeRosterChar(&writer, character)
	}
	return writer.bytes()
}

func rosterCount(characters []dnfrepo.CharacterRecord) int {
	return rosterCountForLimit(characters, defaultCharacterSlots)
}

func rosterCountForLimit(characters []dnfrepo.CharacterRecord, slotLimit int) int {
	slotLimit = normalizedCharacterSlotLimit(slotLimit)
	count := len(characters)
	if count > slotLimit {
		return slotLimit
	}
	if count > 0xffff {
		return 0xffff
	}
	return count
}

func writeRosterChar(writer *packetWriter, character dnfrepo.CharacterRecord) {
	charID := uint32(numericCharacterID(character))
	job := byte(numericCharacterStat(character.Job))
	growType := byte(numericCharacterStatValue(character, "grow_type"))
	level := byte(character.Level)
	if level == 0 {
		level = 1
	}
	slot := uint16(character.Slot)

	writer.writeZero(8)
	writer.writeUint16(uint16(charID))
	writer.writeByte(latestCharacterStateActive)
	writer.writeByte(0)

	writer.writeUint32(charID)
	writer.writeUint32(0)
	writer.writeUint32(0)
	writer.writeByte(job)
	writer.writeByte(growType)
	writer.writeByte(level)
	writer.writeByte(0)
	writer.writeByte(0)

	writer.writeUint32(0)
	writer.writeUint16(slot)
	writer.writeUint32(charID)
	writeFixedUTF16(writer, character.Name, 32)
	writer.writeByte(job)
	writer.writeByte(0)

	writer.writeUint32(0)
	writer.writeByte(0)
}

func buildRosterBodies(characters []dnfrepo.CharacterRecord) [][]byte {
	return [][]byte{
		buildRosterSlots(characters),
		buildRosterChars(characters),
	}
}

func buildCSharpRosterBody(characters []dnfrepo.CharacterRecord) []byte {
	return buildCSharpRosterBodyForLimit(characters, defaultCharacterSlots)
}

func buildCSharpRosterBodyForLimit(characters []dnfrepo.CharacterRecord, slotLimit int) []byte {
	return buildNoPackRosterBodyForLimit(characters, slotLimit)
}

func buildNoPackRosterBody(characters []dnfrepo.CharacterRecord) []byte {
	return buildNoPackRosterBodyForLimit(characters, defaultCharacterSlots)
}

func buildNoPackRosterBodyForLimit(characters []dnfrepo.CharacterRecord, slotLimit int) []byte {
	var writer packetWriter
	slotLimit = effectiveCharacterSlotLimit(characters, slotLimit)
	totalCount := rosterCountForLimit(characters, slotLimit)
	rows := noPackRosterRowsForBatch(characters, totalCount, 0, slotLimit)
	header := defaultNoPackRosterHeaderForLimit(totalCount, slotLimit)
	// Current EXE sub_200BEA0 stores the tail byte as a one-based roster
	// page number.  It subtracts one and records SelectedOrPage against that
	// page. Current EXE sub_33D3470/sub_33D5410 prove that its transport page
	// boundary is 36, while the account's default unlocked capacity remains 32.
	header.PageCount = noPackRosterLastOccupiedPage(characters, totalCount)

	// sub_200BEA0 mode=2 consumes this exact 15-byte prefix before it begins
	// calling sub_200B250 for role rows.  Use fixed offsets rather than a
	// sequential writer: byte 13..14 is the only row-loop count, and a zero
	// there produces a healthy-but-empty (0/17) character selection screen.
	prefix := make([]byte, 15)
	prefix[0] = 2
	prefix[1] = byte(totalCount)
	prefix[2] = 5
	binary.LittleEndian.PutUint16(prefix[3:5], clampRosterUint16(header.TotalOrSlotLimit))
	binary.LittleEndian.PutUint16(prefix[5:7], clampRosterUint16(header.UsedOrRemain))
	binary.LittleEndian.PutUint16(prefix[7:9], rosterUint16Value(header.SelectedOrPage, 0))
	binary.LittleEndian.PutUint32(prefix[9:13], rosterUint32Value(header.RosterState, 0))
	binary.LittleEndian.PutUint16(prefix[13:15], uint16(len(rows)))
	writer.writeBytes(prefix)
	for _, character := range rows {
		// Extension batches carry the account-wide roster slot, not the
		// row's zero-based position inside this 36-slot page. Re-numbering
		// slot 36 as zero makes the client overwrite the first-page actor.
		writeNoPackRosterEntry(&writer, character.Slot, character)
	}
	writer.writeByte(rosterByteValue(header.PageCount, rosterDefaultPageCount))
	writer.writeByte(rosterHeaderByte(header.RosterFlag, 0))
	// mode=2 β��״̬ flag �ᴥ���ͻ��˽�ɫ��״̬ˢ�£�Ĭ���� 0������ÿ�δ�ѡ��ҳ��������������Ч��
	writer.writeUint32(rosterUint32Value(header.RosterValueA, 0))
	writer.writeUint32(rosterUint32Value(header.RosterValueB, 0))
	return writer.bytes()
}

func buildNoPackRosterExtensionBodyForLimit(characters []dnfrepo.CharacterRecord, slotLimit int, start int) ([]byte, bool) {
	slotLimit = effectiveCharacterSlotLimit(characters, slotLimit)
	if start < currentRosterExtensionBatchSize ||
		start >= slotLimit ||
		start%currentRosterExtensionBatchSize != 0 {
		return nil, false
	}
	totalCount := rosterCountForLimit(characters, slotLimit)
	rows := noPackRosterRowsForBatch(characters, totalCount, start, slotLimit)
	if len(rows) == 0 {
		return nil, false
	}

	var writer packetWriter
	writer.writeByte(4)
	writer.writeUint16(uint16(totalCount))
	writer.writeUint16(uint16(len(rows)))
	for _, character := range rows {
		// �� mode-2 һ������ʵ��λ�����������ڵ������ţ�һ����¼�Ĳ�λ����
		// 0..59 ֮�⣬rosterSlotValue ����䵽����������ѵ� 36 �Ų�λд�� 0��
		// �ͻ��˾ͻ�����չ���ε��и��ǵ��һҳ�Ľ�ɫ��
		writeNoPackRosterEntry(&writer, character.Slot, character)
	}
	return writer.bytes(), true
}

func noPackRosterRowsForBatch(characters []dnfrepo.CharacterRecord, count int, start int, slotLimit int) []dnfrepo.CharacterRecord {
	if start < 0 || start >= slotLimit || count <= 0 {
		return nil
	}
	end := start + currentRosterExtensionBatchSize
	if end > slotLimit {
		end = slotLimit
	}
	rows := make([]dnfrepo.CharacterRecord, 0, count)
	for idx, character := range characters {
		if idx >= count {
			break
		}
		if character.Slot < start || character.Slot >= end {
			continue
		}
		rows = append(rows, character)
	}
	return rows
}

func noPackRosterLastOccupiedPage(characters []dnfrepo.CharacterRecord, count int) int64 {
	page := int64(rosterDefaultPageCount)
	for idx, character := range characters {
		if idx >= count {
			break
		}
		if character.Slot < 0 || character.Slot >= maxCharacterSlots {
			continue
		}
		candidate := int64(character.Slot/currentRosterExtensionBatchSize + 1)
		if candidate > page {
			page = candidate
		}
	}
	return page
}

func noteEmptyRosterSlotProbe(session *gameSession, body []byte) {
	// Current EXE sub_200BEA0(mode=2) finalizes an empty selector with index
	// UINT16_MAX, then immediately sends class1/op679 and clears selector ready.
	// Remember only that exact mode-2 empty snapshot so the request handler can
	// complete the client's no-extension handshake without changing ordinary
	// slot-extension behavior.
	session.emptyRosterSlotProbePending = len(body) >= 15 &&
		body[0] == 2 &&
		body[1] == 0 &&
		binary.LittleEndian.Uint16(body[7:9]) == 0 &&
		binary.LittleEndian.Uint16(body[13:15]) == 0
}

// defaultNoPackRosterHeader ����ɶ˰�ȫ�Ĳ�λ�����ֶΡ�
func defaultNoPackRosterHeader(count int) dnfrepo.CharacterRosterHeader {
	return defaultNoPackRosterHeaderForLimit(count, defaultCharacterSlots)
}

func defaultNoPackRosterHeaderForLimit(count int, slotLimit int) dnfrepo.CharacterRosterHeader {
	slotLimit = normalizedCharacterSlotLimit(slotLimit)
	return dnfrepo.CharacterRosterHeader{
		UnkA:             latestRosterRouteNormal,
		UnkB:             latestRosterContextNormal,
		TotalOrSlotLimit: int64(slotLimit),
		UsedOrRemain:     int64(slotLimit),
		SelectedOrPage:   int64(count),
		PageCount:        rosterDefaultPageCount,
		RosterFlag:       0,
	}
}

func normalizedCharacterSlotLimit(limit int) int {
	if limit < defaultCharacterSlots || limit > maxCharacterSlots {
		return defaultCharacterSlots
	}
	return limit
}

// effectiveCharacterSlotLimit �������޶�̧���ܸ����������ò�λ��
//
// �˺ż�¼ȱʧ��Ԫ�����𻵻��ȡʧ��ʱ���޶����䵽���� 32��������Ľ�ɫ��Ȼ����
// 32..59 �Ų�λ�ϡ����ͶӰ���Ż��䣬��Щ��ɫ��������ѡ��ҳ��ʧ��op679 ��չ����Ҳ��
// ��Ϊ start >= slotLimit ���ܣ���ҿ����ľ��ǡ���ɫȫû�ˡ����޶�ֻ�þ������ܲ���
// �ٽ������þ����Ѿ����ڵĽ�ɫ¶��¶�档
func effectiveCharacterSlotLimit(characters []dnfrepo.CharacterRecord, limit int) int {
	effective := normalizedCharacterSlotLimit(limit)
	for _, character := range characters {
		if character.Slot >= effective && character.Slot < maxCharacterSlots {
			effective = character.Slot + 1
		}
	}
	if count := len(characters); count > effective {
		effective = count
	}
	if effective > maxCharacterSlots {
		effective = maxCharacterSlots
	}
	return effective
}
