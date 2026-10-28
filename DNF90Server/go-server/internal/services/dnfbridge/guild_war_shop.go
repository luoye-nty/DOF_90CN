package dnfbridge

import (
	"encoding/binary"
	"errors"
	"math"

	dnfguild "longheng.io/server/internal/modules/dnf/guild"
	dnfrepo "longheng.io/server/internal/modules/dnf/repository"
)

// F97B52..F97B87 copies three u32 fields into a 25-byte native structure.
// The first 13 bytes are uninitialized client-local wrapper data, not IDs.
func parseCurrentGuildWarShopUpgrade(body []byte) (npc uint32, before, after uint16, ok bool) {
	if len(body) != 25 {
		return
	}
	npc = binary.LittleEndian.Uint32(body[13:])
	from, to := binary.LittleEndian.Uint32(body[17:]), binary.LittleEndian.Uint32(body[21:])
	if from == 0 || from > math.MaxUint16 || to > math.MaxUint16 || to != from+1 {
		return
	}
	return npc, uint16(from), uint16(to), true
}

func currentGuildWarShopError(err error) byte {
	switch {
	case errors.Is(err, dnfguild.ErrPermissionDenied):
		return 36
	case errors.Is(err, errDungeonPickupInventoryFull):
		return 136
	default:
		return currentGuildGuardianError(err)
	}
}

func (s *Service) handleCurrentGuildWarShopUpgrade(session *gameSession, body []byte) error {
	const opcode = uint16(840)
	npc, before, after, ok := parseCurrentGuildWarShopUpgrade(body)
	ready, _ := s.currentSceneTransitionReadyForState(session)
	if !ok || !ready {
		return s.sendGameUpperFailure(session, opcode, 3)
	}
	items, err := s.currentPVFItemCatalog()
	if err != nil {
		return s.sendGameUpperFailure(session, opcode, 1)
	}
	catalog, err := dnfguild.LoadWarShops(items.source)
	if err != nil {
		s.logGameEvent(session, "game-guild-war-shop-catalog-failed", "error", err)
		return s.sendGameUpperFailure(session, opcode, 1)
	}
	ctx, cancel := s.currentGuildContext()
	defer cancel()
	var guild *dnfguild.Guild
	var kind byte
	err = s.transactCurrentGuildAssets(ctx, session, func(_ dnfrepo.Group, r *dnfguild.Registry, c *dnfrepo.CharacterRecord) error {
		var e error
		kind, e = r.UpgradeWarShop(c.CharacterID, npc, before, after, catalog, s.gameplayNow())
		if e == nil {
			guild, _, _ = r.GuildForCharacter(c.CharacterID)
		}
		return e
	})
	if err != nil {
		s.logGameEvent(session, "game-guild-war-shop-upgrade-rejected", "npc", npc, "before", before, "after", after, "error", err)
		return s.sendGameUpperFailure(session, opcode, currentGuildWarShopError(err))
	}
	if err = s.sendGameUpperSuccess(session, opcode, nil); err != nil {
		return err
	}
	if err = s.publishCurrentGuildGuardianState(session, guild); err != nil {
		return err
	}
	// E84240 (class0/778) consumes u8 shop kind + u32 level, closes the
	// upgrade dialog and updates the two native u16 merchant-level fields.
	var notice packetWriter
	notice.writeByte(kind)
	notice.writeUint32(uint32(after))
	s.logGameEvent(session, "game-guild-war-shop-upgraded", "npc", npc, "kind", kind, "level", after, "ap", guild.GuardianAP, "gp", guild.GuardianGP)
	return s.sendGameUpperRawClass(session, 778, notice.bytes(), 0)
}

func (s *Service) handleCurrentGuildWarShopBuy(session *gameSession, body []byte) error {
	const opcode = uint16(1354)
	request, err := parseCurrentNPCShopBuyRequest(body)
	ready, _ := s.currentSceneTransitionReadyForState(session)
	if err != nil || !ready || request.Count == 0 {
		return s.sendGameUpperFailure(session, opcode, 3)
	}
	items, err := s.currentPVFItemCatalog()
	if err != nil {
		return s.sendGameUpperFailure(session, opcode, 1)
	}
	catalog, err := dnfguild.LoadWarShops(items.source)
	if err != nil {
		s.logGameEvent(session, "game-guild-war-shop-catalog-failed", "error", err)
		return s.sendGameUpperFailure(session, opcode, 1)
	}
	definition, err := items.ResolveItem(request.ItemID)
	if err != nil || definition.Kind != dungeonDropItemStackable || definition.InventoryListType != dnfrepo.MainInventoryListType {
		return s.sendGameUpperFailure(session, opcode, 3)
	}
	cost, err := dnfguild.LoadWarProductCost(items.source, definition.PVFPath)
	if err != nil {
		return s.sendGameUpperFailure(session, opcode, 3)
	}
	ctx, cancel := s.currentGuildContext()
	defer cancel()
	var guild *dnfguild.Guild
	var updates []currentItemListEntry
	err = s.transactCurrentGuildAssets(ctx, session, func(tx dnfrepo.Group, r *dnfguild.Registry, c *dnfrepo.CharacterRecord) error {
		if int64(c.Level) < definition.MinimumLevel {
			return dnfguild.ErrInvalidContent
		}
		if e := r.BuyWarProduct(c.CharacterID, request.ShopContext, request.ActorContext, request.ItemID, request.Count, catalog, cost, s.gameplayNow()); e != nil {
			return e
		}
		if tx.Inventory == nil {
			return dnfguild.ErrRepositoryUnavailable
		}
		inventory, found, e := tx.Inventory.Load(ctx, c.CharacterID)
		if e != nil {
			return e
		}
		if !found {
			return dnfguild.ErrRepositoryUnavailable
		}
		slots, e := grantCurrentCeraShopProduct(&inventory, definition, request.Count, s.currentInventoryPlacementMainSlotCount(ctx, &inventory, c.CharacterID))
		if e != nil {
			return e
		}
		for _, slot := range slots {
			key := currentCeraShopInventorySlotKey(definition.InventoryListType, int16(slot))
			stack := inventory.Slots[key]
			if stack.Extra == nil {
				stack.Extra = make(map[string]string)
			}
			stack.Extra["last_grant_source"] = "guild_war_shop"
			entry := currentItemListEntryFromStack(definition.InventoryListType, int16(slot), stack)
			stack.RawEntry = append([]byte(nil), entry.data[:]...)
			inventory.Slots[key] = stack
			updates = append(updates, entry)
		}
		guild, _, _ = r.GuildForCharacter(c.CharacterID)
		return tx.Inventory.Save(ctx, inventory)
	})
	if err != nil {
		s.logGameEvent(session, "game-guild-war-shop-buy-rejected", "item", request.ItemID, "count", request.Count, "shop", request.ShopContext, "npc", request.ActorContext, "error", err)
		return s.sendGameUpperFailure(session, opcode, currentGuildWarShopError(err))
	}
	// E8F730 clears the native purchase-busy flag on both success and failure.
	if err = s.sendGameUpperSuccess(session, opcode, nil); err != nil {
		return err
	}
	sortCurrentItemListEntries(updates)
	if err = s.sendGameUpperRawClass(session, 14, buildCurrentItemUpdateBody(definition.InventoryListType, updates), 0); err != nil {
		return err
	}
	s.logGameEvent(session, "game-guild-war-shop-bought", "item", request.ItemID, "count", request.Count, "shop", request.ShopContext, "ap", guild.GuardianAP, "gp", guild.GuardianGP)
	return s.publishCurrentGuildGuardianState(session, guild)
}
