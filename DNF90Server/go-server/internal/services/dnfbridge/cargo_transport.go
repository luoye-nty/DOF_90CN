package dnfbridge

import (
	"context"
	"encoding/binary"
	"errors"
	"fmt"
	"math"
	"sort"
	"strings"

	"longheng.io/server/internal/modules/dnf/dnfenum"
	dnfrepo "longheng.io/server/internal/modules/dnf/repository"
)

const currentCargoTransportOpcode uint16 = uint16(dnfenum.CmdPacketCargoTransportItem)

// The op557 response handler in the current EXE is sub_1E785B0 (registered in
// the third registry: push handler; push 0x22D; call sub_1CF2A70; call
// sub_225C880). Its failure branch at 0x1E788CD reads the class-1 error code
// from [ebp+0x10] and only recognises seven values -- 0x04, 0x07, 0x11, 0x13,
// 0x15, 0x16 and 0xD6 -- each mapping to one dstr id (0x89E4, 0x89DD, 0x89E3,
// 0x89E2, 0x33B3, 0x89E5 and, through the shared table sub_32DDC70, 0xA4F8).
// `esi` is zeroed at 0x1E788D0 before the compare chain, so **any other code
// falls through with dstr id 0** and the client pops whatever string happens
// to sit first in dstr.dat -- in this build "已超出机器人放置的个数上限", which
// has nothing to do with cargo transport.
//
// The seven texts, read out of docs/client-dstr-zh.txt:
//
//	0x04 -> 35300 帐号金库空间不足
//	0x07 -> 35293 无法转移该道具
//	0x11 -> 35299 该道具不存在
//	0x13 -> 35298 帐号金库未激活，无法使用。
//	0x15 -> 13235 无法使用。
//	0x16 -> 35301 所需材料不足
//	0xD6 -> 42232 在物品已锁定状态下，您无法进行当前操作。
//
// ServerS4A21's CargoTransportItemAckBuilder maps its SourceMissing family to
// 0x16 and everything else to 0x07; with the strings readable that turns out
// to be wrong for 0x16, which is the material message and the single most
// common rejection this opcode has. The mapping below follows the text.
const (
	// currentCargoTransportErrorNotTransferable -- "无法转移该道具" -- is the
	// catch-all: an ineligible target, an unresolvable recipient, a malformed
	// request, and any fault this server has not classified.
	currentCargoTransportErrorNotTransferable byte = 0x07
	// currentCargoTransportErrorItemMissing -- "该道具不存在" -- the slot the
	// request named holds nothing.
	currentCargoTransportErrorItemMissing byte = 0x11
	// currentCargoTransportErrorCargoInactive -- "帐号金库未激活，无法使用。"
	currentCargoTransportErrorCargoInactive byte = 0x13
	// currentCargoTransportErrorCargoFull -- "帐号金库空间不足"
	currentCargoTransportErrorCargoFull byte = 0x04
	// currentCargoTransportErrorMaterial -- "所需材料不足" -- any of the four
	// Grandi costs (soul, element crystal, epic fragment, particular item)
	// the player is short of.
	currentCargoTransportErrorMaterial byte = 0x16
)

// currentCargoTransportGrandiMode is the request's mode byte for 歌兰蒂斯
// 装备跨界, and the same value the success ACK echoes back as its trailing
// mode byte. sub_1E785B0 closes the 装备跨界 window (UI id 0x435) at
// 0x1E78870 only for mode 2.
const currentCargoTransportGrandiMode byte = 2

// currentCargoTransportRejection carries the client-recognised error code
// alongside the reason that goes to the log.
type currentCargoTransportRejection struct {
	code   byte
	reason string
}

func (e currentCargoTransportRejection) Error() string { return e.reason }

func currentCargoTransportReject(code byte, format string, args ...any) error {
	return currentCargoTransportRejection{code: code, reason: fmt.Sprintf(format, args...)}
}

// currentCargoTransportFailureCode maps one commit failure onto a code the
// op557 handler recognises. An uncoded error -- a repository or PVF fault, or
// a rejection this server has not classified yet -- becomes the generic code
// rather than reaching the client as an unknown value.
func currentCargoTransportFailureCode(err error) byte {
	var rejection currentCargoTransportRejection
	if errors.As(err, &rejection) && rejection.code != 0 {
		return rejection.code
	}
	return currentCargoTransportErrorNotTransferable
}

// buildCurrentCargoTransportSuccessBody is the op557 success payload
// sub_1E785B0 reads at 0x1E78621: four u16 followed by the mode byte. The
// first u16 is the stone slot the handler repaints, the third its remaining
// count (0 removes the row) and the fourth a per-mode parameter; the layout
// matches ServerS4A21's CargoTransportItemAckBuilder. For mode 2 the handler
// closes the window and returns before touching the item rows, so the
// remaining count and parameter are zero.
func buildCurrentCargoTransportSuccessBody(stoneSlot, targetSlot, remainingStoneCount, parameter uint16, mode byte) []byte {
	body := make([]byte, 9)
	binary.LittleEndian.PutUint16(body[0:2], stoneSlot)
	binary.LittleEndian.PutUint16(body[2:4], targetSlot)
	binary.LittleEndian.PutUint16(body[4:6], remainingStoneCount)
	binary.LittleEndian.PutUint16(body[6:8], parameter)
	body[8] = mode
	return body
}

type currentCargoTransportRequest struct {
	StoneSlot       int16
	TargetSlot      int16
	Mode            byte
	RecipientSelect uint32
}

func decodeCurrentCargoTransportRequest(body []byte) (currentCargoTransportRequest, error) {
	if len(body) != 9 {
		return currentCargoTransportRequest{}, fmt.Errorf("cargo transport body length=%d want=9", len(body))
	}
	return currentCargoTransportRequest{
		StoneSlot:       int16(binary.LittleEndian.Uint16(body[0:2])),
		TargetSlot:      int16(binary.LittleEndian.Uint16(body[2:4])),
		Mode:            body[4],
		RecipientSelect: binary.LittleEndian.Uint32(body[5:9]),
	}, nil
}

func (s *Service) handleCurrentCargoTransport(session *gameSession, body []byte) error {
	request, err := decodeCurrentCargoTransportRequest(body)
	if err != nil {
		return s.sendGameUpperFailure(session, currentCargoTransportOpcode, currentCargoTransportErrorNotTransferable)
	}
	ctx, cancel := context.WithTimeout(context.Background(), createWriteTimeout)
	defer cancel()
	repositories, ok := s.repositoryGroup()
	if !ok {
		return s.sendGameUpperFailure(session, currentCargoTransportOpcode, currentCargoTransportErrorNotTransferable)
	}
	catalog, err := s.currentPVFItemCatalog()
	if err != nil {
		return s.sendGameUpperFailure(session, currentCargoTransportOpcode, currentCargoTransportErrorNotTransferable)
	}
	config, err := loadCurrentCargoTransportConfig(catalog)
	if err != nil {
		return s.sendGameUpperFailure(session, currentCargoTransportOpcode, currentCargoTransportErrorNotTransferable)
	}
	accountID := strings.TrimSpace(s.accountIDForSession(session))
	characterID := selectedCharacterIDString(session)
	switch request.Mode {
	case 0:
		err = s.commitCurrentStoneToAccountCargo(ctx, repositories, catalog, config, accountID, characterID, request)
	case 1:
		err = s.commitCurrentPetTransport(ctx, repositories, catalog, accountID, characterID, request)
	case 2:
		err = s.commitCurrentGrandiEquipmentTransport(ctx, repositories, catalog, config, accountID, characterID, request)
	default:
		err = errors.New("unsupported cargo transport mode")
	}
	if err != nil {
		code := currentCargoTransportFailureCode(err)
		s.logGameEvent(session, "game-cargo-transport-blocked", "mode", request.Mode, "stone_slot", request.StoneSlot, "target_slot", request.TargetSlot, "recipient", request.RecipientSelect, "error_code", code, "err", err)
		return s.sendGameUpperFailure(session, currentCargoTransportOpcode, code)
	}
	for _, listType := range currentCargoTransportRefreshLists(request.Mode) {
		listBody, _, _, built := s.buildCurrentItemListBodyForSession(ctx, session, listType)
		if !built {
			return fmt.Errorf("build cargo transport list=%d", listType)
		}
		if err := s.sendCurrentSceneItemList(session, uint16(dnfenum.CmdPacketLeaveParty), listBody); err != nil {
			return err
		}
	}
	if request.Mode == currentCargoTransportGrandiMode {
		// Only 装备跨界 gets the success ACK today: it is the one mode whose
		// client branch is settled (close UI 0x435, touch no item row). Modes
		// 0 and 1 repaint the stone stack from the same body, and sending a
		// wrong slot/count pair there would corrupt the bag, so they keep the
		// refresh-only behaviour until their fields are read off the EXE too.
		return s.sendGameUpperSuccess(session, currentCargoTransportOpcode, buildCurrentCargoTransportSuccessBody(
			uint16(request.StoneSlot), uint16(request.TargetSlot), 0, 0, currentCargoTransportGrandiMode))
	}
	return nil
}

func currentCargoTransportRefreshLists(mode byte) []byte {
	switch mode {
	case 0:
		return []byte{dnfrepo.MainInventoryListType, 12}
	case 1:
		return []byte{dnfrepo.MainInventoryListType, 7}
	default:
		return []byte{dnfrepo.MainInventoryListType}
	}
}

func (s *Service) commitCurrentStoneToAccountCargo(ctx context.Context, repositories dnfrepo.Group, catalog *pvfDungeonDropCatalog, config currentCargoTransportConfig, accountID, characterID string, request currentCargoTransportRequest) error {
	if repositories.AccountItems == nil || repositories.Account == nil {
		return errors.New("account cargo transaction unavailable")
	}
	account, found, err := repositories.Account.Load(ctx, accountID)
	if err != nil || !found {
		return firstNonNilError(err, errors.New("account missing"))
	}
	capacity := int(currentAccountCargoMetadataInt(account.Metadata, "account_cargo_level"))
	if capacity <= 0 || capacity > math.MaxInt16 {
		return currentCargoTransportReject(currentCargoTransportErrorCargoInactive,
			"account cargo not created: capacity=%d", capacity)
	}
	return repositories.AccountItems.WithinAccountCharacterItems(ctx, accountID, characterID, func(accounts dnfrepo.AccountInventoryRepository, inventories dnfrepo.InventoryRepository) error {
		inventory, found, loadErr := inventories.Load(ctx, characterID)
		if loadErr != nil || !found {
			return firstNonNilError(loadErr, errors.New("source inventory missing"))
		}
		cargo, _, loadErr := accounts.Load(ctx, accountID)
		if loadErr != nil {
			return loadErr
		}
		inventory = dnfrepo.CloneInventory(inventory)
		cargo = dnfrepo.CloneAccountInventory(cargo)
		cargo.AccountID = accountID
		if cargo.Slots == nil {
			cargo.Slots = make(map[string]dnfrepo.ItemStack)
		}
		stoneKey := currentMainInventorySlotKey(request.StoneSlot)
		stone, found := inventory.Slots[stoneKey]
		if !found || stone.ItemID <= 0 || stone.Count <= 0 || stone.ItemID > math.MaxUint32 {
			return currentCargoTransportReject(currentCargoTransportErrorItemMissing,
				"cargo transport stone missing: slot=%d", request.StoneSlot)
		}
		stoneDefinition, resolveErr := catalog.ResolveItem(uint32(stone.ItemID))
		if resolveErr != nil || stoneDefinition.Kind != dungeonDropItemStackable || !stoneDefinition.CargoTransportStone {
			return currentCargoTransportReject(currentCargoTransportErrorNotTransferable,
				"cargo transport stone invalid: item=%d", stone.ItemID)
		}
		rule, found := config.Rules[stoneDefinition.CargoTransportStoneType]
		if !found {
			return currentCargoTransportReject(currentCargoTransportErrorNotTransferable,
				"cargo transport stone rule missing: type=%d", stoneDefinition.CargoTransportStoneType)
		}
		targetKey := currentMainInventorySlotKey(request.TargetSlot)
		target, found := inventory.Slots[targetKey]
		if !found || target.ItemID <= 0 || target.ItemID > math.MaxUint32 {
			return currentCargoTransportReject(currentCargoTransportErrorItemMissing,
				"cargo transport target missing: slot=%d", request.TargetSlot)
		}
		targetDefinition, resolveErr := catalog.ResolveItem(uint32(target.ItemID))
		if resolveErr != nil || !currentCargoRuleAllows(rule, targetDefinition, target) {
			return currentCargoTransportReject(currentCargoTransportErrorNotTransferable,
				"cargo transport target ineligible: item=%d", target.ItemID)
		}
		destinationSlot := int16(-1)
		for slot := 0; slot < capacity; slot++ {
			if _, occupied := cargo.Slots[fmt.Sprintf("12:%d", slot)]; !occupied {
				destinationSlot = int16(slot)
				break
			}
		}
		if destinationSlot < 0 {
			return currentCargoTransportReject(currentCargoTransportErrorCargoFull,
				"account cargo full: capacity=%d", capacity)
		}
		delete(inventory.Slots, targetKey)
		stone.Count--
		if stone.Count == 0 {
			delete(inventory.Slots, stoneKey)
		} else {
			inventory.Slots[stoneKey] = stone
		}
		cargo.Slots[fmt.Sprintf("12:%d", destinationSlot)] = target
		now := s.gameplayNow().UTC()
		inventory.UpdatedAt = now
		cargo.UpdatedAt = now
		if saveErr := dnfrepo.SaveInventoryFields(ctx, inventories, inventory, dnfrepo.InventoryFieldSlots); saveErr != nil {
			return saveErr
		}
		return accounts.Save(ctx, cargo)
	})
}

// commitCurrentGrandiEquipmentTransport moves one piece of equipment to
// another character on the same account and **delivers it by mail**, the way
// the official 歌兰蒂斯 跨界 does: dropping it straight into the recipient's
// backpack silently fails when that character is short of space, and gives no
// record of the transfer on either side.
//
// Mail delivery also narrows the transaction. The old two-character trade
// boundary existed only to write the destination inventory; the letter needs
// the sender's assets plus the recipient's mailbox, which is exactly
// WithinMailboxClaimAssets -- and it still covers the Epic Book, so the epic
// fragment cost commits or rolls back with the removal and the letter.
func (s *Service) commitCurrentGrandiEquipmentTransport(ctx context.Context, repositories dnfrepo.Group, catalog *pvfDungeonDropCatalog, config currentCargoTransportConfig, accountID, sourceCharacterID string, request currentCargoTransportRequest) error {
	if repositories.MailboxAssets == nil || repositories.Character == nil {
		return errors.New("equipment transport transaction unavailable")
	}
	claimAssets, ok := repositories.MailboxAssets.(dnfrepo.MailboxClaimAssetUnitOfWork)
	if !ok {
		return errors.New("equipment transport mailbox transaction unavailable")
	}
	destination, err := s.currentCargoRecipientBySelection(ctx, repositories.Character, accountID, sourceCharacterID, request.RecipientSelect)
	if err != nil {
		return err
	}
	rule, found := config.Rules[7]
	if !found {
		return errors.New("Grandi transport rule missing")
	}
	return claimAssets.WithinMailboxClaimAssets(ctx, sourceCharacterID, destination.CharacterID, func(
		_ dnfrepo.AccountRepository,
		accounts dnfrepo.AccountInventoryRepository,
		characters dnfrepo.CharacterRepository,
		inventories dnfrepo.InventoryRepository,
		mailboxes dnfrepo.MailboxRepository,
		epicBooks dnfrepo.EpicBookRepository,
	) error {
		sourceCharacter, sourceFound, loadErr := characters.Load(ctx, sourceCharacterID)
		if loadErr != nil || !sourceFound || strings.TrimSpace(sourceCharacter.AccountID) != accountID {
			return firstNonNilError(loadErr, errors.New("source character mismatch"))
		}
		source, sourceFound, loadErr := inventories.Load(ctx, sourceCharacterID)
		if loadErr != nil || !sourceFound {
			return firstNonNilError(loadErr, errors.New("source inventory missing"))
		}
		account, _, loadErr := accounts.Load(ctx, accountID)
		if loadErr != nil {
			return loadErr
		}
		epicBook, _, loadErr := epicBooks.Load(ctx, accountID)
		if loadErr != nil {
			return loadErr
		}
		source = dnfrepo.CloneInventory(source)
		account = dnfrepo.CloneAccountInventory(account)
		epicBook = dnfrepo.CloneEpicBook(epicBook)
		account.AccountID = accountID
		epicBook.AccountID = accountID
		targetKey := currentMainInventorySlotKey(request.TargetSlot)
		target, targetFound := source.Slots[targetKey]
		if !targetFound || target.ItemID <= 0 || target.ItemID > math.MaxUint32 {
			return currentCargoTransportReject(currentCargoTransportErrorItemMissing,
				"equipment transport target missing: slot=%d", request.TargetSlot)
		}
		definition, resolveErr := catalog.ResolveItem(uint32(target.ItemID))
		if resolveErr != nil || !currentCargoRuleAllows(rule, definition, target) {
			return currentCargoTransportReject(currentCargoTransportErrorNotTransferable,
				"equipment transport target ineligible: item=%d", target.ItemID)
		}
		category := currentCargoEquipmentCategory(definition.EquipmentType)
		if category < 0 {
			return currentCargoTransportReject(currentCargoTransportErrorNotTransferable,
				"equipment transport category unsupported: type=%q", definition.EquipmentType)
		}
		base := rangeWeightFor(rule.GradeWeights, definition.MinimumLevel) * rule.RarityWeights[definition.Rarity]
		if base <= 0 {
			return currentCargoTransportReject(currentCargoTransportErrorNotTransferable,
				"equipment transport base cost invalid: item=%d", target.ItemID)
		}
		soulWeights := rule.SoulWeights[definition.Rarity]
		if category >= len(soulWeights) || soulWeights[category] <= 0 {
			return currentCargoTransportReject(currentCargoTransportErrorNotTransferable,
				"equipment transport soul cost invalid: item=%d", target.ItemID)
		}
		soulID := rule.SoulItemIDs[definition.Rarity]
		if soulID == 0 {
			return currentCargoTransportReject(currentCargoTransportErrorNotTransferable,
				"equipment transport soul missing: rarity=%d", definition.Rarity)
		}
		// The four cost checks below are the "所需材料不足" case the dialog has
		// its own text for; anything else here is a rule fault, not the
		// player being short.
		if _, _, consumeErr := consumeCurrentItemByID(account.Slots, soulID, base*soulWeights[category]); consumeErr != nil {
			return currentCargoTransportReject(currentCargoTransportErrorMaterial,
				"equipment transport soul cost: item=%d need=%d: %v", soulID, base*soulWeights[category], consumeErr)
		}
		if elementWeights := rule.ElementWeights[definition.Rarity]; rule.ElementItemID > 0 && category < len(elementWeights) && elementWeights[category] > 0 {
			if _, _, consumeErr := consumeCurrentItemByID(source.Slots, rule.ElementItemID, base*elementWeights[category]); consumeErr != nil {
				return currentCargoTransportReject(currentCargoTransportErrorMaterial,
					"equipment transport element cost: item=%d need=%d: %v", rule.ElementItemID, base*elementWeights[category], consumeErr)
			}
		}
		if category < len(rule.EpicPieceWeight) && rule.EpicPieceWeight[category] > 0 {
			need := uint64(base) * uint64(rule.EpicPieceWeight[category])
			balance := uint64(epicBook.Balances[uint32(target.ItemID)])
			if epicBook.Balances == nil || need > balance {
				return currentCargoTransportReject(currentCargoTransportErrorMaterial,
					"epic fragment balance insufficient: item=%d need=%d have=%d", target.ItemID, need, balance)
			}
			epicBook.Balances[uint32(target.ItemID)] = uint32(balance - need)
		}
		for _, particular := range rule.ParticularCosts {
			if particular.Rarity == definition.Rarity && particular.Level == definition.MinimumLevel {
				if _, _, consumeErr := consumeCurrentItemByID(source.Slots, particular.ItemID, particular.Count); consumeErr != nil {
					return currentCargoTransportReject(currentCargoTransportErrorMaterial,
						"equipment transport particular cost: item=%d need=%d: %v", particular.ItemID, particular.Count, consumeErr)
				}
			}
		}
		now := s.gameplayNow().UTC()
		delete(source.Slots, targetKey)
		if _, mailErr := dnfrepo.AppendSystemMail(ctx, mailboxes, dnfrepo.SystemMailDelivery{
			RecipientCharacterID: destination.CharacterID,
			RecipientName:        destination.Name,
			Title:                "装备跨界",
			Body: fmt.Sprintf("%s 通过歌兰蒂斯的装备跨界寄送了一件装备，请在附件中领取。",
				currentCargoTransportSenderName(sourceCharacter, sourceCharacterID)),
			Source:      "grandi_equipment_transport",
			Attachments: []dnfrepo.MailAttachment{currentCargoTransportMailAttachment(target)},
			CreatedAt:   now,
		}); mailErr != nil {
			return mailErr
		}
		source.UpdatedAt, account.UpdatedAt, epicBook.UpdatedAt = now, now, now
		if saveErr := dnfrepo.SaveInventoryFields(ctx, inventories, source, dnfrepo.InventoryFieldSlots); saveErr != nil {
			return saveErr
		}
		if saveErr := accounts.Save(ctx, account); saveErr != nil {
			return saveErr
		}
		return epicBooks.Save(ctx, epicBook)
	})
}

// currentCargoTransportMailAttachment carries the exact equipment instance --
// its packed row, reinforcement/amplification extras, binding and expiry --
// into the letter. A cross-transfer must not silently reset the piece to a
// fresh template the way a template-id attachment would.
func currentCargoTransportMailAttachment(target dnfrepo.ItemStack) dnfrepo.MailAttachment {
	count := target.Count
	if count <= 0 {
		count = 1
	}
	extra := make(map[string]string, len(target.Extra)+2)
	for key, value := range target.Extra {
		extra[key] = value
	}
	extra["source"] = "grandi_equipment_transport"
	extra["last_grant_source"] = "grandi_equipment_transport"
	return dnfrepo.MailAttachment{
		ItemID:   target.ItemID,
		Count:    count,
		Bind:     target.Bind,
		ExpireAt: target.ExpireAt,
		RawEntry: append([]byte(nil), target.RawEntry...),
		Extra:    extra,
	}
}

func currentCargoTransportSenderName(sender dnfrepo.CharacterRecord, fallbackID string) string {
	if name := strings.TrimSpace(sender.Name); name != "" {
		return name
	}
	return "角色 " + fallbackID
}

func (s *Service) commitCurrentPetTransport(ctx context.Context, repositories dnfrepo.Group, catalog *pvfDungeonDropCatalog, accountID, sourceCharacterID string, request currentCargoTransportRequest) error {
	if repositories.AccountCharacterTrade == nil || repositories.Character == nil {
		return errors.New("pet transport transaction unavailable")
	}
	destination, err := s.currentCargoRecipientBySelection(ctx, repositories.Character, accountID, sourceCharacterID, request.RecipientSelect)
	if err != nil {
		return err
	}
	return repositories.AccountCharacterTrade.WithinAccountCharacterTrade(ctx, accountID, sourceCharacterID, destination.CharacterID, func(_ dnfrepo.AccountInventoryRepository, characters dnfrepo.CharacterRepository, inventories dnfrepo.InventoryRepository, _ dnfrepo.EpicBookRepository, pets dnfrepo.PetRepository) error {
		for _, characterID := range []string{sourceCharacterID, destination.CharacterID} {
			character, found, loadErr := characters.Load(ctx, characterID)
			if loadErr != nil || !found || strings.TrimSpace(character.AccountID) != accountID {
				return firstNonNilError(loadErr, errors.New("pet transport character mismatch"))
			}
		}
		source, found, loadErr := inventories.Load(ctx, sourceCharacterID)
		if loadErr != nil || !found {
			return firstNonNilError(loadErr, errors.New("pet transport source inventory missing"))
		}
		dest, _, loadErr := inventories.Load(ctx, destination.CharacterID)
		if loadErr != nil {
			return loadErr
		}
		sourcePets, _, loadErr := pets.Load(ctx, sourceCharacterID)
		if loadErr != nil {
			return loadErr
		}
		destPets, _, loadErr := pets.Load(ctx, destination.CharacterID)
		if loadErr != nil {
			return loadErr
		}
		source = dnfrepo.CloneInventory(source)
		dest = dnfrepo.CloneInventory(dest)
		sourcePets = dnfrepo.ClonePet(sourcePets)
		destPets = dnfrepo.ClonePet(destPets)
		dest.CharacterID = destination.CharacterID
		sourcePets.CharacterID = sourceCharacterID
		destPets.CharacterID = destination.CharacterID
		if dest.Slots == nil {
			dest.Slots = make(map[string]dnfrepo.ItemStack)
		}
		stoneKey := currentMainInventorySlotKey(request.StoneSlot)
		stone, found := source.Slots[stoneKey]
		if !found || stone.ItemID <= 0 || stone.ItemID > math.MaxUint32 || stone.Count <= 0 {
			return errors.New("pet transport stone missing")
		}
		stoneDefinition, resolveErr := catalog.ResolveItem(uint32(stone.ItemID))
		if resolveErr != nil || !stoneDefinition.CreatureTransportStone {
			return errors.New("pet transport stone invalid")
		}
		targetKey := fmt.Sprintf("7:%d", request.TargetSlot)
		target, found := source.Slots[targetKey]
		if !found || target.ItemID <= 0 || target.ItemID > math.MaxUint32 {
			return errors.New("pet transport target missing")
		}
		targetDefinition, resolveErr := catalog.ResolveItem(uint32(target.ItemID))
		if resolveErr != nil || normalizeCargoEquipmentType(targetDefinition.EquipmentType) != "creature" {
			return errors.New("pet transport target invalid")
		}
		var sourcePetKey string
		var sourcePet dnfrepo.PetEntry
		for petKey, entry := range sourcePets.Entries {
			if entry.SourceListType == 7 && entry.SourceSlotIndex == request.TargetSlot && entry.ItemID == target.ItemID {
				sourcePetKey, sourcePet = petKey, entry
				break
			}
		}
		if sourcePetKey == "" || sourcePets.EquippedKey == sourcePetKey {
			return errors.New("pet transport durable state unavailable or equipped")
		}
		destinationSlot := int16(-1)
		for slot := int16(0); slot <= 139; slot++ {
			if _, occupied := dest.Slots[fmt.Sprintf("7:%d", slot)]; !occupied {
				destinationSlot = slot
				break
			}
		}
		if destinationSlot < 0 {
			return errors.New("destination pet inventory full")
		}
		if destPets.Entries == nil {
			destPets.Entries = make(map[string]dnfrepo.PetEntry)
		}
		if _, collision := destPets.Entries[sourcePetKey]; collision {
			return errors.New("destination pet identity collision")
		}
		delete(source.Slots, targetKey)
		delete(sourcePets.Entries, sourcePetKey)
		stone.Count--
		if stone.Count == 0 {
			delete(source.Slots, stoneKey)
		} else {
			source.Slots[stoneKey] = stone
		}
		dest.Slots[fmt.Sprintf("7:%d", destinationSlot)] = target
		sourcePet.SourceSlotIndex = destinationSlot
		destPets.Entries[sourcePetKey] = sourcePet
		now := s.gameplayNow().UTC()
		source.UpdatedAt, dest.UpdatedAt, sourcePets.UpdatedAt, destPets.UpdatedAt = now, now, now, now
		if saveErr := dnfrepo.SaveInventoryFields(ctx, inventories, source, dnfrepo.InventoryFieldSlots); saveErr != nil {
			return saveErr
		}
		if saveErr := dnfrepo.SaveInventoryFields(ctx, inventories, dest, dnfrepo.InventoryFieldSlots); saveErr != nil {
			return saveErr
		}
		if saveErr := dnfrepo.SavePetFields(ctx, pets, sourcePets, dnfrepo.PetFieldEntries); saveErr != nil {
			return saveErr
		}
		return dnfrepo.SavePetFields(ctx, pets, destPets, dnfrepo.PetFieldEntries)
	})
}

// currentCargoRecipientBySelection resolves the recipient the client picked in
// the 装备跨界 dropdown. The value on the wire is the **position** of the
// character in the client's own roster array, not its slot: the combo is
// filled at 0x107DA0B..0x107DA8E by walking that array with `edi` and storing
// `edi` itself as each row's data (`push edi` at 0x107DA79), skipping the
// acting character's own row but not its index; the request then sends the
// selected row's data (`sub_353C2E0` reads combo+0x3D8, which
// `sub_353C96C`/`sub_353CA32` fill from the selected row's +0x420).
// ServerS4A21's InventoryHandler.CargoTransport.cs:158-174 resolves the same
// field the same way.
//
// The roster array is what listCharacters/ListByAccount produce: non-deleted
// characters ordered by slot then character id, densely packed. Index and slot
// coincide only while the slots run 0..n-1, which is why matching on Slot --
// the previous behaviour -- worked on a full account and silently failed for
// anyone whose slots have a gap.
func (s *Service) currentCargoRecipientBySelection(ctx context.Context, characters dnfrepo.CharacterRepository, accountID, sourceCharacterID string, selection uint32) (dnfrepo.CharacterRecord, error) {
	rows, err := characters.ListByAccount(ctx, accountID, dnfrepo.MaxCharacterSlotLimit)
	if err != nil {
		return dnfrepo.CharacterRecord{}, err
	}
	roster := make([]dnfrepo.CharacterRecord, 0, len(rows))
	for _, row := range rows {
		if strings.TrimSpace(row.AccountID) == accountID {
			roster = append(roster, row)
		}
	}
	sort.SliceStable(roster, func(left, right int) bool {
		if roster[left].Slot == roster[right].Slot {
			return roster[left].CharacterID < roster[right].CharacterID
		}
		return roster[left].Slot < roster[right].Slot
	})
	if selection > math.MaxInt32 || int(selection) >= len(roster) {
		return dnfrepo.CharacterRecord{}, currentCargoTransportReject(currentCargoTransportErrorNotTransferable,
			"same-account recipient missing: selection=%d roster=%d", selection, len(roster))
	}
	picked := roster[selection]
	if picked.CharacterID == sourceCharacterID {
		return dnfrepo.CharacterRecord{}, currentCargoTransportReject(currentCargoTransportErrorNotTransferable,
			"same-account recipient is the sender: selection=%d", selection)
	}
	if s != nil && picked.Slot != int(selection) {
		// Worth a line: on a gap-free account the two readings agree, so any
		// divergence is the first live evidence for the index reading.
		s.logPacketEvent("dnf-cargo-transport-recipient-index-differs-from-slot",
			"selection", selection, "picked_character_id", picked.CharacterID, "picked_slot", picked.Slot)
	}
	return picked, nil
}

func currentCargoRuleAllows(rule currentCargoTransportRule, definition dungeonDropItemDefinition, stack dnfrepo.ItemStack) bool {
	if definition.ItemID == 0 || definition.Kind != dungeonDropItemEquipment || rule.ExcludeItemIDs[definition.ItemID] {
		return false
	}
	// 跨界一次搬一件：装备一件一行，Count != 1 只可能是魔盒发奖缺陷留下的历史堆叠行
	// （现场 2026-09-27 线上 90 行 357 件）。跨界石按件计价、寄送附件按 stack.Count 整行
	// 带走，放它进来就是「一颗跨界石/一次运费搬走 N 件」。
	if !currentStackIsSingleEquipment(stack) {
		return false
	}
	if len(rule.IncludeItemIDs) > 0 && !rule.IncludeItemIDs[definition.ItemID] {
		return false
	}
	if len(rule.EquipmentTypes) > 0 && !rule.EquipmentTypes[normalizeCargoEquipmentType(definition.EquipmentType)] {
		return false
	}
	if len(rule.GradeWeights) > 0 && rangeWeightFor(rule.GradeWeights, definition.MinimumLevel) <= 0 {
		return false
	}
	if len(rule.RarityWeights) > 0 && rule.RarityWeights[definition.Rarity] <= 0 {
		return false
	}
	if rule.ExceptGrowth && definition.HasGrowthType || rule.ExceptPeriod && (definition.PeriodEquipment || !stack.ExpireAt.IsZero()) {
		return false
	}
	return true
}

func currentCargoEquipmentCategory(equipmentType string) int {
	switch normalizeCargoEquipmentType(equipmentType) {
	case "weapon":
		return 0
	case "coat", "shoulder", "pants", "shoes", "waist":
		return 1
	case "amulet", "wrist", "ring":
		return 2
	case "support", "magic stone", "earring":
		return 3
	case "title name":
		return 4
	default:
		return -1
	}
}
