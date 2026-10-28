package dnfbridge

import (
	"fmt"
	"sort"
	"time"

	"longheng.io/server/internal/modules/dnf/alignedcmd"
	dnfrepo "longheng.io/server/internal/modules/dnf/repository"
)

const (
	currentRewardSlotHighlightDelay = 50 * time.Millisecond
	// Current DNF90CN accepts a single changed row reliably. Publishing a
	// mixed gift package as one large op14 per inventory list can crash the
	// client while it is still creating the package rewards. Keep the first
	// highlight immediate, then let the UI consume one row at a time.
	currentRewardSlotHighlightStep = 15 * time.Millisecond
)

func currentRewardSlotRefreshesFromKeys(keys map[string]struct{}) ([]alignedcmd.ItemSlotRefresh, error) {
	refreshes := make([]alignedcmd.ItemSlotRefresh, 0, len(keys))
	for key := range keys {
		listType, slot, ok := parseSceneInventorySlotKey(key)
		if !ok || slot < 0 {
			return nil, fmt.Errorf("reward slot key %q is invalid", key)
		}
		refreshes = append(refreshes, alignedcmd.ItemSlotRefresh{ListType: listType, SlotIndex: slot})
	}
	sort.Slice(refreshes, func(left, right int) bool {
		if refreshes[left].ListType != refreshes[right].ListType {
			return refreshes[left].ListType < refreshes[right].ListType
		}
		return refreshes[left].SlotIndex < refreshes[right].SlotIndex
	})
	return refreshes, nil
}

func currentRewardSlotHighlightSupported(refresh alignedcmd.ItemSlotRefresh) bool {
	// New list-7 rows are already delivered by the package's full pet-list
	// snapshot. Replaying one through op14 enters the current client pet-update
	// path with an uninitialized object and crashes at 0x0206430B. Main-bag and
	// avatar rows are the two live-proved changed-slot highlight carriers.
	return refresh.ListType == dnfrepo.MainInventoryListType || refresh.ListType == currentExeInventoryAvatarListType
}

// Full list snapshots make every reward visible, but they do not identify new
// rows to the client and can overwrite an immediate slot animation. Re-publish
// only the committed reward rows after the snapshot settles so the current
// client plays its native changed-slot highlight without changing item state.
//
// Lottery jars are deliberately not routed here: their op27 result row is the
// sole inventory mutation packet during the native opening sequence, and any
// op14 during "open all" can dereference stale client objects.
func (s *Service) scheduleCurrentRewardSlotHighlights(
	session *gameSession,
	operation string,
	refreshes []alignedcmd.ItemSlotRefresh,
) {
	if s == nil || session == nil || len(refreshes) == 0 || s.gameplayTimers == nil {
		return
	}
	characterID, generation, err := gameSessionCharacterEventIdentity(session, session.selectedCharacterID)
	if err != nil {
		s.logGameEvent(session, "game-reward-slot-highlight-skipped", "operation", operation, "reason", err)
		return
	}
	supported := make([]alignedcmd.ItemSlotRefresh, 0, len(refreshes))
	skipped := make([]alignedcmd.ItemSlotRefresh, 0, len(refreshes))
	for _, refresh := range refreshes {
		if currentRewardSlotHighlightSupported(refresh) {
			supported = append(supported, refresh)
		} else {
			skipped = append(skipped, refresh)
		}
	}
	if len(skipped) > 0 {
		s.logGameEvent(session, "game-reward-slot-highlight-unsupported-skipped",
			"operation", operation,
			"reward_slots", fmt.Sprint(skipped),
			"reason", "current_client_list7_new_row_op14_access_violation")
	}
	if len(supported) == 0 {
		return
	}
	refreshes = supported
	batchID := s.gameplayTimers.Now().UnixNano()
	scheduled := 0
	for index, refresh := range refreshes {
		refresh := refresh
		delay := currentRewardSlotHighlightDelay + time.Duration(index)*currentRewardSlotHighlightStep
		timerName := fmt.Sprintf(
			"dnf-reward-slot-highlight:%s:%s:%d:%d",
			session.connID,
			operation,
			batchID,
			index,
		)
		err = s.gameplayTimers.ScheduleAfter(timerName, delay, func(time.Time) {
			postErr := s.postGameSessionCharacterEvent(
				session,
				operation+"-reward-slot-highlight",
				characterID,
				generation,
				func() error {
					return s.sendSelectedIncrementalItemSlotRefreshes(
						session,
						operation+"_reward_slot_highlight",
						[]alignedcmd.ItemSlotRefresh{refresh},
					)
				},
			)
			if postErr != nil && !isClosedGameSessionEventError(postErr) {
				s.logGameEvent(session, "game-reward-slot-highlight-failed",
					"operation", operation,
					"reward_slot", fmt.Sprint(refresh),
					"reason", postErr)
			}
		})
		if err != nil {
			s.logGameEvent(session, "game-reward-slot-highlight-schedule-failed",
				"operation", operation,
				"reward_slot", fmt.Sprint(refresh),
				"scheduled", scheduled,
				"reason", err)
			return
		}
		scheduled++
	}
	lastDelay := currentRewardSlotHighlightDelay + time.Duration(len(refreshes)-1)*currentRewardSlotHighlightStep
	s.logGameEvent(session, "game-reward-slot-highlight-scheduled",
		"operation", operation,
		"reward_slots", fmt.Sprint(refreshes),
		"slot_count", scheduled,
		"first_delay_ms", currentRewardSlotHighlightDelay.Milliseconds(),
		"last_delay_ms", lastDelay.Milliseconds(),
		"step_ms", currentRewardSlotHighlightStep.Milliseconds())
}
