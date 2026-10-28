package dnfbridge

import (
	"context"
	"encoding/binary"
	"errors"
	"math"
	"strconv"

	"longheng.io/server/internal/modules/dnf/dnfenum"
	dnfguild "longheng.io/server/internal/modules/dnf/guild"
	dnfrepo "longheng.io/server/internal/modules/dnf/repository"
)

func (s *Service) handleCurrentGuildAura(session *gameSession, body []byte) error {
	opcode := uint16(dnfenum.CmdPacketSetGuildFlagAura)
	if len(body) != 8 {
		return s.sendGameUpperFailure(session, opcode, 19)
	}
	items, err := s.currentPVFItemCatalog()
	if err != nil {
		return s.sendGameUpperFailure(session, opcode, 19)
	}
	catalog, err := dnfguild.LoadCatalog(items.source)
	if err != nil {
		return s.sendGameUpperFailure(session, opcode, 19)
	}
	ctx, cancel := s.currentGuildContext()
	defer cancel()
	err = s.mutateCurrentGuild(ctx, func(repositories dnfrepo.Group, registry *dnfguild.Registry) error {
		character, err := selectedGuildCharacter(ctx, repositories, session)
		if err != nil || character.AccountID != s.accountIDForSession(session) {
			return dnfguild.ErrCharacterNotFound
		}
		return registry.SetAura(character.CharacterID, binary.LittleEndian.Uint32(body), binary.LittleEndian.Uint32(body[4:]), catalog, s.gameplayNow())
	})
	return s.finishCurrentGuildSettings(session, opcode, err, nil)
}

func (s *Service) transactCurrentGuildAssets(ctx context.Context, session *gameSession, apply func(dnfrepo.Group, *dnfguild.Registry, *dnfrepo.CharacterRecord) error) error {
	repositories, ok := s.repositoryGroup()
	if !ok || repositories.CharacterSettlement == nil {
		return dnfguild.ErrRepositoryUnavailable
	}
	characterID := strconv.Itoa(int(session.selectedCharacterID))
	s.guildMu.Lock()
	defer s.guildMu.Unlock()
	return repositories.CharacterSettlement.WithinCharacterSettlement(ctx, characterID, func(tx dnfrepo.Group) error {
		character, err := selectedGuildCharacter(ctx, tx, session)
		if err != nil {
			return err
		}
		if character.AccountID != s.accountIDForSession(session) {
			return dnfguild.ErrCharacterNotFound
		}
		registry, err := dnfguild.Load(ctx, tx.Settings)
		if err != nil {
			return err
		}
		if err := apply(tx, &registry, &character); err != nil {
			return err
		}
		return dnfguild.Save(ctx, tx.Settings, registry, s.gameplayNow())
	})
}

// op761's refusal codes. Handler 0x01D0EAD0 turns the code into a text id with
// `byte[0x1D0EBC0 + code - 9]` -> slot and `jmp [0x1D0EB8C + slot*4]`; every case
// body is a two-push stub whose first immediate is the text id. Decoded with
// D:\re_tools\content_codes.py (byte table 0x1D0EBC0, jump table 0x1D0EB8C):
//
//	9   -> 19489 只能购买1次永久性公会内容。            (content already owned)
//	10  -> 256   \n 错误 : 金币不足                     (gold too low)
//	13  -> 19338 购买后公会仓库将超过上限，因此无法购买。 (generic / does not fit)
//	14  -> 19488 公会仓库升级错误
//	19  -> 76639 公会之契约只能购买30天。
//	20  -> 48428 超出可购买次数上限，无法继续购买。
//	22  -> 19336 公会积分不足。                         (mileage too low)
//	36  -> 60120 您没有权限
//	38  -> 76638 当前公会成员扩张状态和公会之契约的类型不符。
//	105 -> 48421 只有购买前置公会成员扩张券后，才能购买该道具。
//	116 -> 48420 已使用的公会成员扩张券。
//	182 -> 19337 没有基地。
//	everything else (slots 12) -> no toast at all
//
// The mapping used to answer 182 for ErrInsufficientMileage, so a guild whose 积分
// was too low was told 没有基地 whenever it bought a 公会内容 item -- the live
// symptom of 2026-09-12. A full bag has no code of its own in this table (255
// 物品栏空间不足 is not reachable through it), so it shares the generic refusal.
const (
	currentGuildContentErrorAlreadyOwned = byte(9)
	currentGuildContentErrorGold         = byte(10)
	currentGuildContentErrorGeneric      = byte(13)
	currentGuildContentErrorMileage      = byte(22)
	currentGuildContentErrorPermission   = byte(36)
	currentGuildContentErrorMemberState  = byte(38)
)

func currentGuildContentError(err error) byte {
	switch {
	case errors.Is(err, dnfguild.ErrContentActive):
		return currentGuildContentErrorAlreadyOwned
	case errors.Is(err, dnfguild.ErrInsufficientMileage):
		return currentGuildContentErrorMileage
	case errors.Is(err, dnfguild.ErrPermissionDenied):
		return currentGuildContentErrorPermission
	case errors.Is(err, dnfguild.ErrMemberLimit):
		return currentGuildContentErrorMemberState
	case errors.Is(err, dnfguild.ErrInsufficientGold):
		return currentGuildContentErrorGold
	case errors.Is(err, errDungeonPickupInventoryFull):
		return currentGuildContentErrorGeneric
	default:
		return currentGuildContentErrorGeneric
	}
}

func (s *Service) handleCurrentGuildContentsPurchase(session *gameSession, body []byte) error {
	opcode := uint16(dnfenum.CmdPacketBuyGuildContents)
	if len(body) < 8 {
		return s.sendGameUpperFailure(session, opcode, 13)
	}
	name, next, valid := readCurrentRawDstr(body, 4)
	if !valid || next != len(body) || len(name) != 0 {
		return s.sendGameUpperFailure(session, opcode, 13)
	}
	contentID := binary.LittleEndian.Uint32(body)
	items, err := s.currentPVFItemCatalog()
	if err != nil {
		return s.sendGameUpperFailure(session, opcode, 13)
	}
	catalog, err := dnfguild.LoadCatalog(items.source)
	if err != nil {
		return s.sendGameUpperFailure(session, opcode, 13)
	}
	content, found := catalog.Contents[contentID]
	if !found {
		return s.sendGameUpperFailure(session, opcode, 13)
	}
	var definition dungeonDropItemDefinition
	if content.Type == 20 {
		definition, err = items.ResolveItem(content.ItemID)
		if err != nil || definition.Kind != dungeonDropItemStackable {
			return s.sendGameUpperFailure(session, opcode, 13)
		}
	}
	ctx, cancel := s.currentGuildContext()
	defer cancel()
	var update []currentItemListEntry
	var gold int64
	err = s.transactCurrentGuildAssets(ctx, session, func(tx dnfrepo.Group, registry *dnfguild.Registry, character *dnfrepo.CharacterRecord) error {
		if _, err := registry.Purchase(character, content, s.gameplayNow()); err != nil {
			return err
		}
		if content.Type == 20 {
			inventory, found, err := tx.Inventory.Load(ctx, character.CharacterID)
			if err != nil {
				return err
			}
			if !found {
				return dnfguild.ErrRepositoryUnavailable
			}
			if currentQuestDropHeldCount(inventory, int64(content.ItemID)) > 0 {
				return dnfguild.ErrContentActive
			}
			slot, err := currentQuestDropGrantStack(&inventory, definition, 1)
			if err != nil {
				return err
			}
			key := currentCeraShopInventorySlotKey(dnfrepo.MainInventoryListType, slot)
			stack := inventory.Slots[key]
			stack.Extra["source"] = "guild_content_purchase"
			stack.Extra["last_grant_source"] = "guild_content_purchase"
			inventory.Slots[key] = stack
			update = append(update, currentItemListEntryFromStack(dnfrepo.MainInventoryListType, slot, stack))
			if err := tx.Inventory.Save(ctx, inventory); err != nil {
				return err
			}
		}
		gold = character.Stats["gold"]
		if content.Gold {
			return dnfrepo.SaveCharacterFields(ctx, tx.Character, *character, dnfrepo.CharacterFieldStats)
		}
		return nil
	})
	if err != nil {
		s.logGameEvent(session, "game-guild-content-purchase-rejected", "content_id", contentID, "error", err)
		return s.sendGameUpperFailure(session, opcode, currentGuildContentError(err))
	}
	if err := s.sendGameUpperSuccess(session, opcode, nil); err != nil {
		return err
	}
	if content.Gold {
		if err := s.sendGameUpperRawClass(session, 14, buildCurrentGoldStateBody(gold), 0); err != nil {
			return err
		}
	}
	if len(update) > 0 {
		if err := s.sendGameUpperRawClass(session, 14, buildCurrentItemUpdateBody(dnfrepo.MainInventoryListType, update), 0); err != nil {
			return err
		}
	}
	guild, err := s.currentGuildForSelectedCharacter(session)
	if err != nil || guild == nil {
		return err
	}
	s.logGameEvent(session, "game-guild-content-purchased", "content_id", contentID, "gold_after", gold, "mileage_after", guild.Mileage)
	return s.publishCurrentGuildManagement(session, guild)
}

func (s *Service) handleCurrentGuildContractClaim(session *gameSession, body []byte) error {
	opcode := uint16(dnfenum.CmdPacketContractOfGuild)
	// sub_F9A0B0 writes a packed 17-byte structure. Only its final u32 is
	// business input (content type 21); the prefix is client transport state.
	if len(body) != 17 || binary.LittleEndian.Uint32(body[13:]) != 21 {
		return s.sendGameUpperFailure(session, opcode, 19)
	}
	items, err := s.currentPVFItemCatalog()
	if err != nil {
		return s.sendGameUpperFailure(session, opcode, 19)
	}
	catalog, err := dnfguild.LoadCatalog(items.source)
	if err != nil {
		return s.sendGameUpperFailure(session, opcode, 19)
	}
	definition, err := items.ResolveItem(catalog.ContractItem)
	if err != nil || definition.Kind != dungeonDropItemStackable {
		return s.sendGameUpperFailure(session, opcode, 19)
	}
	ctx, cancel := s.currentGuildContext()
	defer cancel()
	var update currentItemListEntry
	err = s.transactCurrentGuildAssets(ctx, session, func(tx dnfrepo.Group, registry *dnfguild.Registry, character *dnfrepo.CharacterRecord) error {
		if _, err := registry.ClaimContract(character.CharacterID, s.gameplayNow()); err != nil {
			return err
		}
		inventory, found, err := tx.Inventory.Load(ctx, character.CharacterID)
		if err != nil {
			return err
		}
		if !found {
			return dnfguild.ErrRepositoryUnavailable
		}
		slot, err := currentQuestDropGrantStack(&inventory, definition, catalog.ContractCount)
		if err != nil {
			return err
		}
		key := currentCeraShopInventorySlotKey(dnfrepo.MainInventoryListType, slot)
		stack := inventory.Slots[key]
		stack.Extra["source"] = "guild_contract"
		stack.Extra["last_grant_source"] = "guild_contract"
		inventory.Slots[key] = stack
		update = currentItemListEntryFromStack(dnfrepo.MainInventoryListType, slot, stack)
		return tx.Inventory.Save(ctx, inventory)
	})
	if err != nil {
		return s.sendGameUpperFailure(session, opcode, 19)
	}
	if err := s.sendGameUpperSuccess(session, opcode, nil); err != nil {
		return err
	}
	return s.sendGameUpperRawClass(session, 14, buildCurrentItemUpdateBody(dnfrepo.MainInventoryListType, []currentItemListEntry{update}), 0)
}

func (s *Service) handleCurrentGuildRename(session *gameSession, body []byte) error {
	opcode := uint16(dnfenum.CmdPacketChangeGuildName)
	if len(body) < 7 || body[2] != 0 {
		return s.sendGameUpperFailure(session, opcode, 19)
	}
	slot := binary.LittleEndian.Uint16(body)
	name, next, valid := parseCurrentGuildDSTR(body, 3)
	if slot > math.MaxInt16 || !valid || next != len(body) || dnfguild.ValidateName(name) != nil {
		return s.sendGameUpperFailure(session, opcode, 106)
	}
	items, err := s.currentPVFItemCatalog()
	if err != nil {
		return s.sendGameUpperFailure(session, opcode, 19)
	}
	catalog, err := dnfguild.LoadCatalog(items.source)
	if err != nil {
		return s.sendGameUpperFailure(session, opcode, 19)
	}
	ctx, cancel := s.currentGuildContext()
	defer cancel()
	var update currentItemListEntry
	err = s.transactCurrentGuildAssets(ctx, session, func(tx dnfrepo.Group, registry *dnfguild.Registry, character *dnfrepo.CharacterRecord) error {
		guild, member, found := registry.GuildForCharacter(character.CharacterID)
		if !found || member.Grade != dnfguild.DefaultMasterGrade {
			return dnfguild.ErrPermissionDenied
		}
		if !registry.NameAvailable(name) {
			return dnfguild.ErrNameExists
		}
		inventory, found, err := tx.Inventory.Load(ctx, character.CharacterID)
		if err != nil {
			return err
		}
		if !found {
			return dnfguild.ErrRepositoryUnavailable
		}
		key := currentCeraShopInventorySlotKey(dnfrepo.MainInventoryListType, int16(slot))
		stack, found := inventory.Slots[key]
		allowed := false
		for _, c := range catalog.Contents {
			if c.Type == 20 && stack.ItemID == int64(c.ItemID) {
				allowed = true
			}
		}
		if !found || !allowed || stack.Count != 1 || currentDungeonDiscardStackLocked(stack) || currentDungeonDiscardStackExpired(stack, s.gameplayNow()) {
			return dnfguild.ErrInvalidContent
		}
		delete(inventory.Slots, key)
		update.patchCore(int16(slot), math.MaxUint32, 0)
		guild.Name, guild.UpdatedAt = name, s.gameplayNow()
		return tx.Inventory.Save(ctx, inventory)
	})
	if err != nil {
		code := byte(19)
		if errors.Is(err, dnfguild.ErrNameExists) {
			code = 107
		}
		return s.sendGameUpperFailure(session, opcode, code)
	}
	if err := s.sendGameUpperSuccess(session, opcode, nil); err != nil {
		return err
	}
	if err := s.sendGameUpperRawClass(session, 14, buildCurrentItemUpdateBody(dnfrepo.MainInventoryListType, []currentItemListEntry{update}), 0); err != nil {
		return err
	}
	guild, err := s.currentGuildForSelectedCharacter(session)
	if err != nil || guild == nil {
		return err
	}
	return s.publishCurrentGuildManagement(session, guild)
}
