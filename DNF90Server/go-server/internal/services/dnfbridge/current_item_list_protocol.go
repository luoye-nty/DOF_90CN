package dnfbridge

import (
	"encoding/binary"

	dnfrepo "longheng.io/server/internal/modules/dnf/repository"
)

func buildCurrentItemListBody(listType byte, entries []currentItemListEntry, state dnfrepo.CharacterContainerState) []byte {
	var writer packetWriter
	writer.writeByte(listType)
	switch listType {
	case 0:
		writer.writeUint16(state.MainSlotCount)
	case 1:
		writer.writeUint16(state.AvatarExpansion)
	case 2:
		writer.writeUint16(state.PersonalCargoSlotCount)
	case 12:
		writer.writeUint16(state.AccountCargoSelectionKey)
		writer.writeUint32(state.AccountCargoStateValue)
	}
	writer.writeUint16(uint16(len(entries)))
	for _, entry := range entries {
		writer.writeBytes(entry.data[:])
		if listType == 1 {
			// Current sub_1D72380 always calls both avatar optional-blob
			// readers after the fixed 0x77-byte row. The first blob carries the
			// avatar socket layout when it exists; otherwise keep the
			// constructor-safe zero-length defaults so old rows still parse.
			writeCurrentAvatarItemListOptionalBlobs(&writer, entry)
		}
	}
	if listType == 2 {
		// Current sub_1D72380 always consumes a cargo grouping trailer after
		// the ordinary rows: u8 groupCount followed by groupCount*raw[8].
		// Group persistence is not owned yet, so terminate the proved grammar
		// with the constructor-safe empty group list.
		writer.writeByte(0)
	}
	return writer.bytes()
}

func currentItemListEntrySizeForType(listType byte) int {
	if listType == 1 {
		return currentItemListEntryWireSize + currentAvatarItemListOptionalBlobCount*4
	}
	return currentItemListEntryWireSize
}

func writeCurrentAvatarItemListOptionalBlobs(writer *packetWriter, entry currentItemListEntry) {
	if writer == nil {
		return
	}
	writeCurrentItemListOptionalBlob(writer, entry.avatarSocketData)
	writeCurrentItemListOptionalBlob(writer, entry.avatarColorData)
}

func writeCurrentItemListOptionalBlob(writer *packetWriter, data []byte) {
	if writer == nil {
		return
	}
	if len(data) == 0 {
		writer.writeUint32(0)
		return
	}
	writer.writeUint32(uint32(len(data)))
	writer.writeBytes(data)
}

func buildCurrentEquipmentUpdateBody(listType byte, entries []currentEquipmentUpdateEntry) []byte {
	var writer packetWriter
	writer.writeByte(listType)
	writer.writeUint16(uint16(len(entries)))
	for _, entry := range entries {
		writer.writeBytes(entry.data[:])
	}
	return writer.bytes()
}

// reverseCurrentMainItemListRows re-orders the wire rows of a main-inventory
// (list 0) snapshot from ascending into descending slot order.
//
// The current client's container-list parser builds the main-container list from
// the tail, so an ascending wire order leaves its in-memory list reversed. The
// enchant-store result window then resolves "the item I just enchanted" by list
// index instead of by slot, and paints the mirrored row. Live evidence
// (2026-09-13, character 1, all four enchant-store calls on one connection):
//
//	request TargetSlot 34 -> painted the item of slot 11  (row 1, cell 3)
//	request TargetSlot 35 -> painted the item of slot 10  (row 1, cell 2)
//	request TargetSlot 33 -> painted the item of slot 12  (row 1, cell 4)
//	request TargetSlot  9 -> painted the item of slot 53  (the lone row-6 item)
//
// The bag held 28 equipment rows (slots 9..35 plus the lone slot 53), i.e. the
// painted slot is exactly index 27-i of that ascending list - a plain reversal,
// not a slot arithmetic offset (slot 9 must have painted slot 36 if it were).
// The bag grid itself is rendered from each row's slot field, so the visible
// layout does not depend on this order.
//
// Revert = make sendCurrentSceneItemList pass the body through unchanged.
func reverseCurrentMainItemListRows(body []byte) []byte {
	if len(body) < 5 || body[0] != dnfrepo.MainInventoryListType {
		return body
	}
	count := int(binary.LittleEndian.Uint16(body[3:5]))
	if count < 2 || len(body) != 5+count*currentItemListEntryWireSize {
		return body
	}
	reversed := make([]byte, len(body))
	copy(reversed[:5], body[:5])
	for index := 0; index < count; index++ {
		source := 5 + (count-1-index)*currentItemListEntryWireSize
		target := 5 + index*currentItemListEntryWireSize
		copy(reversed[target:target+currentItemListEntryWireSize], body[source:source+currentItemListEntryWireSize])
	}
	return reversed
}
