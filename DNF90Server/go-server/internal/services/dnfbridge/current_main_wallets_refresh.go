package dnfbridge

import (
	"context"
	"fmt"
	"strconv"
	"strings"

	"longheng.io/server/internal/modules/dnf/dnfenum"
	dnfrepo "longheng.io/server/internal/modules/dnf/repository"
)

// sendSelectedMainWalletsRefresh restores the current client's process-local
// list-0 wallet objects after a list-0/list-2 move. The op19 handler can drop
// slot 0 plus the fixed crystal/soul rows even though their durable values did
// not change. Sending one narrow op14 keeps the open cargo object graph intact.
func (s *Service) sendSelectedMainWalletsRefresh(session *gameSession, source string) error {
	if session == nil || session.selectedCharacterID == 0 {
		return fmt.Errorf("main wallet refresh requires a selected character")
	}
	repos, ok := s.repositoryGroup()
	if !ok || repos.Character == nil || repos.AccountInventory == nil {
		return fmt.Errorf("main wallet refresh requires character and account inventory repositories")
	}

	ctx, cancel := context.WithTimeout(context.Background(), currentRepositorySnapshotTimeout)
	defer cancel()
	characterID := strconv.Itoa(int(session.selectedCharacterID))
	character, found, err := repos.Character.Load(ctx, characterID)
	if err != nil {
		return fmt.Errorf("load character %s wallet: %w", characterID, err)
	}
	if !found {
		return fmt.Errorf("character %s wallet not found", characterID)
	}
	gold := int64(0)
	if character.Stats != nil {
		gold = character.Stats["gold"]
	}

	accountID := strings.TrimSpace(s.accountIDForSession(session))
	if accountID == "" {
		return fmt.Errorf("main wallet refresh requires account id")
	}
	account, _, err := repos.AccountInventory.Load(ctx, accountID)
	if err != nil {
		return fmt.Errorf("load account %s wallets: %w", accountID, err)
	}

	entries := make([]currentItemListEntry, 0, 1+int(dnfrepo.SoulWarehouseLastSlot-dnfrepo.CrystalWarehouseFirstSlot+1))
	entries = append(entries, currentGoldWalletItemListEntry(gold))
	for slot := dnfrepo.CrystalWarehouseFirstSlot; slot <= dnfrepo.SoulWarehouseLastSlot; slot++ {
		stack, occupied := account.Slots[dnfrepo.AccountSharedInventorySlotKey(slot)]
		if !occupied || stack.ItemID <= 0 || stack.Count < 0 {
			continue
		}
		entries = append(entries, currentItemListEntryFromStack(dnfrepo.MainInventoryListType, slot, stack))
	}
	sortCurrentItemListEntries(entries)
	body := buildCurrentItemUpdateBody(dnfrepo.MainInventoryListType, entries)
	s.logPacketEvent("game-upper-current-main-wallets-refresh-send",
		"conn_id", session.connID,
		"channel_id", session.channel.ID,
		"source", source,
		"char_id", session.selectedCharacterID,
		"account_id", accountID,
		"msg_id", uint16(dnfenum.CmdPacketWalkoutPartyMember),
		"classification", 0,
		"list_type", dnfrepo.MainInventoryListType,
		"entry_count", len(entries),
		"body_len", len(body),
		"sequence", "durable_move_then_op19_ack_then_narrow_gold_crystal_soul_op14")
	return s.sendGameUpperRawClass(session, uint16(dnfenum.CmdPacketWalkoutPartyMember), body, 0)
}
