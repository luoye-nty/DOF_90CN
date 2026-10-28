package dnfbridge

import (
	"context"
	"encoding/binary"
	"errors"
	"fmt"
	"math"
	"strings"

	"longheng.io/server/internal/modules/dnf/dnfenum"
	dnfproto "longheng.io/server/internal/modules/dnf/protocol"
	dnfquest "longheng.io/server/internal/modules/dnf/quest"
	dnfrepo "longheng.io/server/internal/modules/dnf/repository"
)

const currentGiveUpQuestRequestBodySize = 4

// currentGiveUpQuestReplayKey scopes the destructive operation receipt to one
// selected character and one TCP session. The first ACK removes the row from
// the current client's task UI, so a duplicate success receipt is unsafe.
type currentGiveUpQuestReplayKey struct {
	characterID uint16
	questID     uint16
}

func newCurrentGiveUpQuestReplayKey(characterID uint16, request dnfquest.QuestIDRequest) currentGiveUpQuestReplayKey {
	return currentGiveUpQuestReplayKey{characterID: characterID, questID: request.QuestID}
}

func (session *gameSession) currentGiveUpQuestReplaySuppressed(key currentGiveUpQuestReplayKey) bool {
	if session == nil {
		return false
	}
	session.questReplay.giveUpMu.Lock()
	defer session.questReplay.giveUpMu.Unlock()
	_, ok := session.questReplay.giveUpAnswered[key]
	return ok
}

func (session *gameSession) suppressCurrentGiveUpQuestReplay(key currentGiveUpQuestReplayKey) {
	if session == nil {
		return
	}
	session.questReplay.giveUpMu.Lock()
	defer session.questReplay.giveUpMu.Unlock()
	if session.questReplay.giveUpAnswered == nil {
		session.questReplay.giveUpAnswered = make(map[currentGiveUpQuestReplayKey]struct{}, 1)
	}
	session.questReplay.giveUpAnswered[key] = struct{}{}
}

func (session *gameSession) clearCurrentGiveUpQuestReplay(key currentGiveUpQuestReplayKey) {
	if session == nil {
		return
	}
	session.questReplay.giveUpMu.Lock()
	defer session.questReplay.giveUpMu.Unlock()
	delete(session.questReplay.giveUpAnswered, key)
}

func (s *Service) handleCurrentGiveUpQuest(session *gameSession, body []byte) error {
	if session == nil || session.selectedCharacterID == 0 {
		return nil
	}
	opcode := uint16(dnfenum.CmdPacketGiveupQuest)
	if len(body) != currentGiveUpQuestRequestBodySize || binary.LittleEndian.Uint16(body[:2]) != opcode {
		s.logGameEvent(session, "game-upper-give-up-quest-blocked",
			"body_len", len(body),
			"reason", "current_exe_op32_requires_exact_echo_and_quest_id")
		return nil
	}
	request, err := dnfquest.DecodeQuestIDRequest(body)
	if err != nil || request.QuestID == 0 {
		s.logGameEvent(session, "game-upper-give-up-quest-blocked",
			"body_len", len(body),
			"reason", "request_decode_failed_or_zero_quest_id",
			"error", err)
		return nil
	}
	replayKey := newCurrentGiveUpQuestReplayKey(session.selectedCharacterID, request)
	if session.currentGiveUpQuestReplaySuppressed(replayKey) {
		return nil
	}

	ctx, cancel := context.WithTimeout(context.Background(), createWriteTimeout)
	defer cancel()
	catalog, err := s.loadQuestCatalog(ctx)
	if err != nil {
		s.logGameEvent(session, "game-upper-give-up-quest-blocked",
			"quest_id", request.QuestID,
			"reason", "quest_catalog_unavailable",
			"error", err)
		return nil
	}
	repositories, ok := s.repositoryGroup()
	if !ok || repositories.Character == nil || repositories.Quest == nil {
		s.logGameEvent(session, "game-upper-give-up-quest-blocked",
			"quest_id", request.QuestID,
			"reason", "character_or_quest_repository_unavailable")
		return nil
	}
	owner, err := dnfquest.NewOwner(repositories)
	if err != nil {
		return fmt.Errorf("create quest give-up owner: %w", err)
	}
	result, err := owner.ApplyGiveUpSettlement(ctx, catalog, dnfquest.GiveUpInput{
		AccountID:           s.accountIDForSession(session),
		SelectedCharacterID: session.selectedCharacterID,
		QuestID:             request.QuestID,
		IsQuestItem:         s.currentGiveUpQuestItemClassifier(session),
	})
	if err != nil {
		s.logGameEvent(session, "game-upper-give-up-quest-blocked",
			"quest_id", request.QuestID,
			"reason", "pvf_db_give_up_preflight_or_persist_failed",
			"error", err)
		switch {
		case errors.Is(err, dnfquest.ErrQuestNotActive), errors.Is(err, dnfquest.ErrQuestDefinitionMissing):
			// 86JP's matching current protocol uses error 19 when the active
			// quest cannot be found. No mutation has occurred on this path.
			return s.sendGameUpperFailure(session, opcode, 19)
		case errors.Is(err, dnfquest.ErrQuestCannotGiveUp):
			return s.sendGameUpperFailure(session, opcode, 20)
		default:
			// Persistence uncertainty must never produce a success receipt.
			return nil
		}
	}
	if result.ReclaimSkipped {
		s.logGameEvent(session, "game-upper-give-up-quest-reclaim-skipped",
			"quest_id", result.QuestID,
			"reason", result.ReclaimSkipReason)
	}

	var payload packetWriter
	payload.writeUint16(result.QuestID)
	s.logGameEvent(session, "game-upper-give-up-quest-success-send",
		"quest_id", result.QuestID,
		"msg_id", opcode,
		"classification", dnfproto.DefaultChannelClassification,
		"plain_body_len", 3,
		"reclaimed_slots", len(result.Reclaimed),
		"body_source", "current_exe_sub_1D1A4B0_u16_success_reader_after_persisted_removal")
	if err := s.sendGameUpperSuccess(session, opcode, payload.bytes()); err != nil {
		return err
	}
	if err := s.sendCurrentGiveUpQuestReclaimedItemUpdate(session, result); err != nil {
		return err
	}
	session.clearCurrentAcceptQuestReplay(newCurrentAcceptQuestReplayKey(session.selectedCharacterID, request))
	session.suppressCurrentGiveUpQuestReplay(replayKey)
	return nil
}

// currentGiveUpQuestItemClassifier 返回"该道具是否属于 PVF `[quest]` 家族"的判定函数。
// `[quest]` 家族就是落在任务栏（list 0 slot 177..232）、不可交易的**任务专属**道具，
// 与 dungeonDropStackableSlotRange 的落格规则同源。
//
// 物品目录不可用时返回 nil：放弃照常成功，只是不回收任何东西 —— 与参考端在物品
// 元数据不可用时的降级一致，绝不因为"回收判定不可用"而挡住玩家放弃任务。
func (s *Service) currentGiveUpQuestItemClassifier(session *gameSession) func(int64) bool {
	catalog, err := s.currentPVFItemCatalog()
	if err != nil || catalog == nil {
		s.logGameEvent(session, "game-upper-give-up-quest-item-catalog-unavailable",
			"reason", "reclaim_classifier_unavailable_abandon_still_allowed",
			"error", err)
		return nil
	}
	return func(itemID int64) bool {
		if itemID <= 0 || itemID > math.MaxUint32 {
			return false
		}
		definition, err := catalog.ResolveItem(uint32(itemID))
		if err != nil {
			return false
		}
		return strings.HasPrefix(normalizeDungeonDropStackableType(definition.StackableType), "[quest]")
	}
}

var errCurrentGiveUpQuestReclaimShape = errors.New("current EXE give-up reclaim row shape is invalid")

// sendCurrentGiveUpQuestReclaimedItemUpdate 把回收后的背包行推给客户端
// （与任务交付扣除材料走同一种 op14 class-0 增量行）。不回收时不发任何包，
// 保持改动前的行为。
func (s *Service) sendCurrentGiveUpQuestReclaimedItemUpdate(session *gameSession, result dnfquest.GiveUpResult) error {
	if len(result.Reclaimed) == 0 {
		return nil
	}
	entries := make([]currentItemListEntry, 0, len(result.Reclaimed))
	for _, reclaimed := range result.Reclaimed {
		if reclaimed.SlotIndex > math.MaxInt16 || reclaimed.ItemID <= 0 ||
			reclaimed.ItemID > math.MaxUint32 || reclaimed.Delta <= 0 || reclaimed.PostCount < 0 {
			return errCurrentGiveUpQuestReclaimShape
		}
		var entry currentItemListEntry
		if reclaimed.PostCount <= 0 {
			// 整行删掉：客户端按 slot + 0xFFFFFFFF 移除该格。
			entry.patchCore(int16(reclaimed.SlotIndex), math.MaxUint32, 0)
			entries = append(entries, entry)
			continue
		}
		if len(reclaimed.RawEntry) == currentItemListEntryWireSize &&
			binary.LittleEndian.Uint16(reclaimed.RawEntry[0:2]) == reclaimed.SlotIndex &&
			binary.LittleEndian.Uint32(reclaimed.RawEntry[2:6]) == uint32(reclaimed.ItemID) {
			copy(entry.data[:], reclaimed.RawEntry)
			entries = append(entries, entry)
			continue
		}
		// 原始行不可用时用提交后的快照重建该行，绝不拿旧行糊弄客户端。
		if !result.HasPostCommitInventory {
			return errCurrentGiveUpQuestReclaimShape
		}
		stack, found := result.PostCommitInventory.Slots[reclaimed.SlotKey]
		if !found || stack.ItemID != reclaimed.ItemID || stack.Count != reclaimed.PostCount {
			return errCurrentGiveUpQuestReclaimShape
		}
		entries = append(entries, currentItemListEntryFromStack(
			dnfrepo.MainInventoryListType, int16(reclaimed.SlotIndex), stack))
	}
	body := buildCurrentItemUpdateBody(0, entries)
	s.logGameEvent(session, "game-upper-give-up-quest-reclaim-item-update-send",
		"quest_id", result.QuestID,
		"msg_id", uint16(dnfenum.CmdPacketWalkoutPartyMember),
		"classification", 0,
		"list_type", 0,
		"entry_count", len(entries),
		"entry_size", currentItemListEntryWireSize,
		"body_len", len(body),
		"body_source", "committed_give_up_quest_reclaim_op14_raw77")
	return s.sendGameUpperRawClass(session, uint16(dnfenum.CmdPacketWalkoutPartyMember), body, 0)
}
