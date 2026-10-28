package dnfbridge

import (
	"encoding/binary"
	"errors"
	"math"
	"sort"

	"longheng.io/server/internal/modules/dnf/dnfenum"
	dnfguild "longheng.io/server/internal/modules/dnf/guild"
	dnfrepo "longheng.io/server/internal/modules/dnf/repository"
)

func (s *Service) handleCurrentGuildCargo(session *gameSession, body []byte) error {
	opcode := uint16(dnfenum.CmdPacketGuildCargo)
	if len(body) != 0 {
		return s.sendGameUpperFailure(session, opcode, currentGuildGenericError)
	}
	guild, err := s.currentGuildForSelectedCharacter(session)
	if err != nil || guild == nil {
		return s.sendGameUpperFailure(session, opcode, currentGuildGenericError)
	}
	return s.sendGameUpperSuccess(session, opcode, buildCurrentGuildCargoBody(guild))
}

func buildCurrentGuildCargoBody(guild *dnfguild.Guild) []byte {
	var writer packetWriter
	writer.writeUint32(uint32(guild.CargoLimit))
	slots := make([]int, 0, len(guild.Cargo))
	for slot := range guild.Cargo {
		slots = append(slots, int(slot))
	}
	sort.Ints(slots)
	writer.writeUint16(uint16(len(slots)))
	for _, slot := range slots {
		entry := currentItemListEntryFromStack(0, int16(slot), guild.Cargo[int16(slot)])
		writer.writeBytes(entry.data[:])
	}
	return writer.bytes()
}

func (s *Service) handleCurrentGuildCargoHistory(session *gameSession, body []byte) error {
	opcode := uint16(dnfenum.CmdPacketGuildCargoHistory)
	if len(body) != 0 {
		return s.sendGameUpperFailure(session, opcode, currentGuildGenericError)
	}
	guild, err := s.currentGuildForSelectedCharacter(session)
	if err != nil || guild == nil {
		return s.sendGameUpperFailure(session, opcode, currentGuildGenericError)
	}
	var writer packetWriter
	writer.writeUint32(uint32(len(guild.CargoHistory)))
	for _, row := range guild.CargoHistory {
		var raw [57]byte
		binary.LittleEndian.PutUint32(raw[:], uint32(row.CreatedAt.Unix()))
		raw[4] = row.Action
		var name packetWriter
		writeClientNameDstr(&name, row.Name)
		copy(raw[5:34], name.bytes()[4:])
		binary.LittleEndian.PutUint32(raw[35:], uint32(row.Item.ItemID))
		binary.LittleEndian.PutUint32(raw[39:], uint32(row.Item.Count))
		entry := currentItemListEntryFromStack(0, 0, row.Item)
		// sub_1D2C7F0 transposes the three magic-seal option columns.
		for i, off := range []int{0x48, 0x4b, 0x4e, 0x49, 0x4c, 0x4f, 0x4a, 0x4d, 0x50, 0x51, 0x54, 0x55, 0x56, 0x53} {
			raw[43+i] = entry.data[off]
		}
		if entry.data[0x52] == 0xff {
			raw[53], raw[54], raw[55] = 0, 0, 0
		}
		writer.writeBytes(raw[:])
	}
	return s.sendGameUpperSuccess(session, opcode, writer.bytes())
}

func (s *Service) handleCurrentGuildCargoPush(session *gameSession, body []byte) error {
	if len(body) != 13 || body[0] != 0 {
		return s.sendGameUpperFailure(session, 247, 196)
	}
	return s.transferCurrentGuildCargo(session, true, int16(binary.LittleEndian.Uint16(body[1:])),
		int16(binary.LittleEndian.Uint16(body[11:])), binary.LittleEndian.Uint32(body[3:]), binary.LittleEndian.Uint32(body[7:]))
}

func (s *Service) handleCurrentGuildCargoPop(session *gameSession, body []byte) error {
	if len(body) != 11 || body[10] != 0 {
		return s.sendGameUpperFailure(session, 248, 196)
	}
	return s.transferCurrentGuildCargo(session, false, -1, int16(binary.LittleEndian.Uint16(body)),
		binary.LittleEndian.Uint32(body[2:]), binary.LittleEndian.Uint32(body[6:]))
}

func (s *Service) transferCurrentGuildCargo(session *gameSession, deposit bool, bagSlot, cargoSlot int16, itemID, count uint32) error {
	opcode := uint16(248)
	if deposit {
		opcode = 247
	}
	ready, readySource := s.currentSceneTransitionReadyForState(session)
	if !ready || itemID == 0 || itemID > math.MaxInt32 {
		// 196 is the only byte this path can hand the client, and the CN client
		// renders it as 「公会仓库尚未完成加载。」(dstr 21684) -- i.e. the player is
		// told the warehouse has not loaded. Without this line a scene-state
		// rejection is indistinguishable in the log from a malformed request:
		// 2026-10-04 19:44 took a log round trip for exactly that reason.
		s.logGameEvent(session, "game-guild-cargo-refused",
			"opcode", opcode,
			"item_id", itemID,
			"count", count,
			"deposit", deposit,
			"bag_slot", bagSlot,
			"cargo_slot", cargoSlot,
			"scene_ready", ready,
			"scene_ready_source", readySource)
		return s.sendGameUpperFailure(session, opcode, 196)
	}
	catalog, err := s.currentPVFItemCatalog()
	if err != nil {
		return s.sendGameUpperFailure(session, opcode, 196)
	}
	definition, err := catalog.ResolveItem(itemID)
	if err != nil {
		return s.sendGameUpperFailure(session, opcode, 196)
	}
	ctx, cancel := s.currentGuildContext()
	defer cancel()
	var selected *dnfguild.Guild
	var update currentItemListEntry
	err = s.transactCurrentGuildAssets(ctx, session, func(tx dnfrepo.Group, registry *dnfguild.Registry, character *dnfrepo.CharacterRecord) error {
		var found bool
		selected, _, found = registry.GuildForCharacter(character.CharacterID)
		if !found {
			return dnfguild.ErrNotMember
		}
		if tx.Inventory == nil {
			return dnfguild.ErrRepositoryUnavailable
		}
		inventory, found, err := tx.Inventory.Load(ctx, character.CharacterID)
		if err != nil {
			return err
		}
		if !found {
			return dnfguild.ErrRepositoryUnavailable
		}
		source := selected.Cargo[cargoSlot]
		if deposit {
			source = inventory.Slots[currentCeraShopInventorySlotKey(0, bagSlot)]
		}
		if currentDungeonDiscardStackLocked(source) || currentDungeonDiscardStackExpired(source, s.gameplayNow()) {
			return dnfguild.ErrPermissionDenied
		}
		allowed := currentDungeonDiscardTradeable(definition)
		if definition.Kind == dungeonDropItemEquipment {
			attach := normalizeMagicBoxPVFType(definition.AttachType)
			allowed = allowed || (attach == "sealing" && currentItemSealFlag(source) != 0)
			if definition.Rarity >= 2 && currentItemSealFlag(source) == 0 {
				allowed = false
			}
		} else {
			allowed = allowed && !source.Bind && currentItemSealFlag(source) == 0
		}
		rule := dnfguild.CargoRule{ItemID: itemID, Equipment: definition.Kind == dungeonDropItemEquipment,
			Allowed: allowed, StackLimit: definition.StackLimit, SlotStart: definition.SlotStart, SlotEnd: definition.SlotEnd}
		bagSlot, err = selected.TransferCargo(character.CharacterID, &inventory, deposit, bagSlot, cargoSlot, itemID, count, rule, s.gameplayNow())
		if err != nil {
			return err
		}
		if remaining, occupied := inventory.Slots[currentCeraShopInventorySlotKey(0, bagSlot)]; occupied {
			update = currentItemListEntryFromStack(0, bagSlot, remaining)
		} else {
			// 原生物品编号 0 是金币，清空格子必须使用 -1。
			update.patchCore(bagSlot, math.MaxUint32, 0)
		}
		return tx.Inventory.Save(ctx, inventory)
	})
	if err != nil {
		s.logGameEvent(session, "game-guild-cargo-rejected", "opcode", opcode, "item_id", itemID, "count", count, "error", err)
		return s.sendGameUpperFailure(session, opcode, currentGuildCargoError(err, deposit))
	}
	var ack packetWriter
	if deposit {
		ack.writeByte(0)
		ack.writeUint16(uint16(bagSlot))
		ack.writeUint32(count)
		ack.writeUint16(uint16(cargoSlot))
	} else {
		ack.writeUint16(uint16(cargoSlot))
		ack.writeByte(0)
		ack.writeUint16(uint16(bagSlot))
		ack.writeUint32(count)
	}
	// Native ACK moves the source object. Authoritative refresh must follow it.
	if err := s.sendGameUpperSuccess(session, opcode, ack.bytes()); err != nil {
		return err
	}
	if err := s.sendGameUpperRawClass(session, 14, buildCurrentItemUpdateBody(0, []currentItemListEntry{update}), 0); err != nil {
		return err
	}
	s.logGameEvent(session, "game-guild-cargo-saved", "opcode", opcode, "item_id", itemID, "count", count, "bag_slot", bagSlot, "cargo_slot", cargoSlot)
	return s.sendGameUpperSuccess(session, 261, buildCurrentGuildCargoBody(selected))
}

func currentGuildCargoError(err error, deposit bool) byte {
	switch {
	case errors.Is(err, dnfguild.ErrNotMember):
		return 100
	case errors.Is(err, dnfguild.ErrPermissionDenied):
		return 36
	case errors.Is(err, dnfguild.ErrCargoFull):
		if !deposit {
			return 204
		}
		return 198
	default:
		return 196
	}
}

func (s *Service) handleCurrentGuildCargoMove(session *gameSession, body []byte) error {
	ready, readySource := s.currentSceneTransitionReadyForState(session)
	if !ready || len(body) != 12 {
		// Same invisible-196 shape as the deposit path: log why before the byte.
		s.logGameEvent(session, "game-guild-cargo-refused",
			"opcode", 249,
			"body_len", len(body),
			"scene_ready", ready,
			"scene_ready_source", readySource)
		return s.sendGameUpperFailure(session, 249, 196)
	}
	ctx, cancel := s.currentGuildContext()
	defer cancel()
	var selected *dnfguild.Guild
	err := s.transactCurrentGuildAssets(ctx, session, func(_ dnfrepo.Group, registry *dnfguild.Registry, character *dnfrepo.CharacterRecord) error {
		var found bool
		selected, _, found = registry.GuildForCharacter(character.CharacterID)
		if !found {
			return dnfguild.ErrNotMember
		}
		return selected.MoveCargo(character.CharacterID, int16(binary.LittleEndian.Uint16(body)), int16(binary.LittleEndian.Uint16(body[6:])),
			binary.LittleEndian.Uint32(body[2:]), binary.LittleEndian.Uint32(body[8:]), s.gameplayNow())
	})
	if err != nil {
		return s.sendGameUpperFailure(session, 249, currentGuildCargoError(err, false))
	}
	ack := append(append([]byte(nil), body[:2]...), body[6:8]...)
	if err := s.sendGameUpperSuccess(session, 249, ack); err != nil {
		return err
	}
	return s.sendGameUpperSuccess(session, 261, buildCurrentGuildCargoBody(selected))
}
