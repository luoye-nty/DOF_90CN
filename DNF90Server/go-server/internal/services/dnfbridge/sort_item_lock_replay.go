package dnfbridge

import (
	"context"
	"fmt"
	"strconv"
	"strings"

	"longheng.io/server/internal/modules/dnf/dnfenum"
	dnfitemlock "longheng.io/server/internal/modules/dnf/itemlock"
	dnfproto "longheng.io/server/internal/modules/dnf/protocol"
	dnfrepo "longheng.io/server/internal/modules/dnf/repository"
)

// sendCurrentSortLockReplayForSession restores the client-only blue
// sort-lock markers after a scene or a complete op13 container refresh has
// rebuilt the local item objects.  The durable state remains attached to the
// item stack, so positions are resolved fresh on every lifecycle boundary.
func (s *Service) sendCurrentSortLockReplayForSession(session *gameSession, source string) error {
	if session == nil || session.selectedCharacterID == 0 {
		return nil
	}
	ctx, cancel := context.WithTimeout(context.Background(), currentRepositorySnapshotTimeout)
	defer cancel()
	positions, ok := s.buildCurrentSortLockPositionsForSession(ctx, session)
	if !ok {
		return fmt.Errorf("item sort-lock snapshot unavailable for character %d", session.selectedCharacterID)
	}
	return s.sendCurrentSelectSortLockReplay(session, source, positions)
}

// sendCurrentSortLockReplayIfAny replays the blue sort-lock markers only when at
// least one row is actually sort-locked.
//
// The little blue padlock is client-only state, exactly like the equipment lock:
// rebuilding the item objects (op13/op14 rows) drops both. A package that hands
// out rows therefore has to replay them - which is why sorting the bag brings the
// markers back, since that path re-sends them itself (live 2026-09-15 07:36-07:40:
// opening a package cleared both locks on an equipped item and on an avatar).
// A character with nothing sort-locked already holds the correct empty state, so
// this variant sends nothing and costs no packet.
func (s *Service) sendCurrentSortLockReplayIfAny(session *gameSession, source string) error {
	if session == nil || session.selectedCharacterID == 0 {
		return nil
	}
	ctx, cancel := context.WithTimeout(context.Background(), currentRepositorySnapshotTimeout)
	defer cancel()
	positions, ok := s.buildCurrentSortLockPositionsForSession(ctx, session)
	if !ok {
		return fmt.Errorf("item sort-lock snapshot unavailable for character %d", session.selectedCharacterID)
	}
	if len(positions) == 0 {
		return nil
	}
	return s.sendCurrentSelectSortLockReplay(session, source+"_had_locked_rows", positions)
}

func (s *Service) buildCurrentSortLockPositionsForSession(
	ctx context.Context,
	session *gameSession,
) ([]dnfitemlock.SortLockPosition, bool) {
	if s == nil || session == nil || session.selectedCharacterID == 0 {
		return nil, false
	}
	repos, ok := s.repositoryGroup()
	if !ok || repos.Inventory == nil {
		return nil, false
	}
	record, found, err := repos.Inventory.Load(ctx, strconv.Itoa(int(session.selectedCharacterID)))
	if err != nil {
		return nil, false
	}
	positions := []dnfitemlock.SortLockPosition(nil)
	if found {
		positions = dnfitemlock.SortLockPositions(record)
	}
	// list 12 是账号所有者容器，锁定行不在角色记录里，但它的 padlock 一样会被
	// op13 重建销毁，所以一起重发。
	positions = append(positions, s.currentSelectedAccountCargoSortLockPositions(ctx, session, repos)...)
	return positions, true
}

// currentSelectedAccountCargoSortLockPositions 读出当前账号金库里仍然锁定的槽位。
// 读不到账号仓库时只是不重发金库的 padlock，不影响角色容器的重发。
func (s *Service) currentSelectedAccountCargoSortLockPositions(
	ctx context.Context,
	session *gameSession,
	repos dnfrepo.Group,
) []dnfitemlock.SortLockPosition {
	if repos.AccountInventory == nil {
		return nil
	}
	accountID := strings.TrimSpace(s.accountIDForSession(session))
	if accountID == "" {
		return nil
	}
	cargo, found, err := repos.AccountInventory.Load(ctx, accountID)
	if err != nil {
		s.logPacketEvent("game-upper-select-sort-lock-replay-deferred",
			"conn_id", session.connID,
			"channel_id", session.channel.ID,
			"char_id", session.selectedCharacterID,
			"reason", "account_inventory_load_failed",
			"err", err)
		return nil
	}
	if !found {
		return nil
	}
	return dnfitemlock.AccountCargoSortLockPositions(cargo)
}

func (s *Service) sendCurrentSelectSortLockReplay(
	session *gameSession,
	source string,
	positions []dnfitemlock.SortLockPosition,
) error {
	for index, position := range positions {
		body := dnfitemlock.BuildSortLockStateBody(position.ListType, position.SlotIndex)
		s.logPacketEvent("game-upper-select-sort-lock-replay-send",
			"conn_id", session.connID,
			"channel_id", session.channel.ID,
			"source", source,
			"char_id", session.selectedCharacterID,
			"packet_index", index,
			"msg_id", uint16(dnfenum.CmdPacketRequestItemSortLock),
			"classification", dnfproto.DefaultChannelClassification,
			"list_type", position.ListType,
			"slot_index", position.SlotIndex,
			"body_len", len(body),
			"sequence", "op13_selected_containers_then_native_op713_success_list_slot")
		if err := s.sendGameUpperRawClassCodec(
			session,
			uint16(dnfenum.CmdPacketRequestItemSortLock),
			body,
			dnfproto.DefaultChannelClassification,
			true,
		); err != nil {
			return err
		}
	}
	return nil
}
