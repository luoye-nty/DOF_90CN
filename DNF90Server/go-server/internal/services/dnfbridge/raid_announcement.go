package dnfbridge

import (
	"context"
	"encoding/binary"
	"fmt"
	"strconv"
	"strings"

	dnfproto "longheng.io/server/internal/modules/dnf/protocol"
	"longheng.io/server/internal/modules/dnf/raid"
)

// The refusal codes of the client's C2S 666 (START_RAID) answer.
//
// sub_1D04620 is registered for opcode 666 and is a (result, message code)
// dispatcher: a non-zero result byte jumps straight to its epilogue, and a zero
// one picks a string out of the client's own table, formats it and opens the
// announcement window (scene UI owner 0x298) with it. The codes it knows are
//
//	0x08 -> 49075 只有攻坚队长才能开始。
//	0x11 -> 49085 攻坚队中存在未准备入场材料的队员。
//	0x13 -> 65066 当前状态下， 无法挑战安徒恩团队模式。
//	0x16 -> 49077 攻坚队要求的最低人数不足， 无法开始。
//	0xD9 -> 66021 攻坚队有队员超过每日的通关次数上限。
//
// 0x11 and 0xD9 additionally read a u32 count followed by count u32s -- the
// members the message is about. The client consumes and discards that list, but
// the cursor still has to line up, so it is only written for those two codes.
const (
	runtimeRaidStartFailureNotLeader     byte = 0x08
	runtimeRaidStartFailureEntryCost     byte = 0x11
	runtimeRaidStartFailureInvalidState  byte = 0x13
	runtimeRaidStartFailureMinimumRoster byte = 0x16
	runtimeRaidStartFailureDailyLimit    byte = 0xD9
)

// sendRaidStartFailure answers C2S 666 with one of the client's own refusal
// messages. The generic sendRaidCommandFailure body is {0, 3}, and code 3 is not
// in the dispatcher's table, which is why a refused start used to look like the
// client had ignored the click.
func (s *Service) sendRaidStartFailure(session *gameSession, typ uint16, code byte, members []uint16) error {
	body := []byte{0, code}
	switch code {
	case runtimeRaidStartFailureEntryCost, runtimeRaidStartFailureDailyLimit:
		body = binary.LittleEndian.AppendUint32(body, uint32(len(members)))
		for _, member := range members {
			body = binary.LittleEndian.AppendUint32(body, uint32(member))
		}
	}
	return s.sendGameUpperRawClassCodec(session, typ, body, dnfproto.DefaultChannelClassification, true)
}

// runtimeRaidMembersMissingEntryCost lists the frozen members whose inventory has
// no entry ticket left. The list feeds code 0x11 so the leader can see who the
// client's "攻坚队中存在未准备入场材料的队员。" is about in the logs.
func (s *Service) runtimeRaidMembersMissingEntryCost(state runtimeRaidState) []uint16 {
	repositories, repositoriesOK := s.repositoryGroup()
	itemID, count := runtimeRaidEntryCostForState(state)
	ctx, cancel := context.WithTimeout(context.Background(), createWriteTimeout)
	defer cancel()
	missing := make([]uint16, 0, len(state.FrozenMembers))
	for _, member := range state.FrozenMembers {
		if runtimeRaidMemberHasEntryCost(ctx, repositories, repositoriesOK, member.CharID, itemID, count) {
			continue
		}
		missing = append(missing, member.CharID)
	}
	return missing
}

// runtimeRaidChannelID is the channel a raid key belongs to: nextRuntimeRaidKeyLocked
// packs the channel into the high half and the creator's character id into the low
// half.
func runtimeRaidChannelID(raidKey uint32) int {
	return int(uint16(raidKey >> 16))
}

// runtimeRaidClearedByPhase reports whether finishing this phase finished the raid
// itself: a team raid clears on phase two, the single mode on its only phase, and
// Fiend War on its only phase too ([PHASE MAX] 1 in its .etc — 打完罗什就是整场收尾).
func runtimeRaidClearedByPhase(state runtimeRaidState, phase byte) bool {
	if state.RaidMode == runtimeRaidOwnerFiendWar {
		return phase == runtimeRaidDungeonPhaseOne
	}
	if state.TailFlag == 1 {
		return phase == runtimeRaidDungeonPhaseOne
	}
	return phase == runtimeRaidDungeonPhaseTwo
}

// broadcastRuntimeRaidClearAnnouncement tells the raid's channel that the raid is
// over, on the client's own 0x0272 announcement line.
//
// The audience is the channel the raid was created on, which is where every raid
// member sits, so the announcement reaches them whether or not they are still in
// the raid party screen.
func (s *Service) broadcastRuntimeRaidClearAnnouncement(state runtimeRaidState) error {
	leaderName := []byte(nil)
	if index := runtimeRaidMemberIndex(state, state.LeaderID); index >= 0 {
		leaderName = rosterNameBytes(state.Members[index].Name)
	}
	kind := raid.RaidClearKindAnton
	switch state.RaidMode {
	case runtimeRaidOwnerLuke:
		kind = raid.RaidClearKindLuke
	case runtimeRaidOwnerFiendWar:
		// kind 2 是客户端 switch 里那条超时空通报：
		//	75517 强大的%s攻坚队队长%s和队员们破坏了量子时空传送站！ 恭喜！
		// 以前这里只有安东/卢克两个分支，超时空通关连通报都不会发（操作员
		// 2026-10-01 报的"缺少系统提示"就是它）。
		kind = raid.RaidClearKindSpiral
	}
	body, err := raid.BuildRaidClearAnnouncementBody(raid.RaidClearAnnouncement{
		Kind:       kind,
		RaidName:   state.NameBytes,
		LeaderName: leaderName,
		// The current EXE only has solo strings for Anton (mode 0) and Luke
		// (mode 1). Its Fiend War branch (mode 2) is team-only; passing solo=1
		// there leaves the formatted wide-string buffer untouched and renders
		// a single garbage character. Use the valid team sentence for a solo
		// Fiend War run as well.
		Solo: state.TailFlag == 1 && kind != raid.RaidClearKindSpiral,
	})
	if err != nil {
		return err
	}
	channelID := runtimeRaidChannelID(state.RaidKey)
	recipients := currentChatSessionsOnChannel(s.currentOnlineChatSessions(), channelID)
	delivered := 0
	for _, recipient := range recipients {
		if recipient == nil {
			continue
		}
		if err := s.sendGameUpperRawClassCodec(recipient, raid.RaidClearAnnouncementMsgID, body, 0, true); err != nil {
			continue
		}
		delivered++
	}
	s.logPacketEvent("game-raid-clear-announcement",
		"raid_key", state.RaidKey,
		"channel_id", channelID,
		"kind", kind,
		"solo", state.TailFlag == 1,
		"raid_name_len", len(state.NameBytes),
		"leader_name_len", len(leaderName),
		"recipients", delivered)
	return nil
}

// announceRuntimeRaidClearIfNeeded raises the clear announcement exactly once, on
// the transition that finishes the raid. The lifecycle status is the first guard:
// the phase is still PhaseActive when its objectives complete, and the settlement
// moves it to AwaitingMovie, so a later completion callback cannot announce again.
//
// The status alone is not enough, because the same phase can complete more than
// once while the settlement cannot start -- a gate still occupied by a member whose
// client froze keeps openRuntimeRaidPhaseMovie on errRuntimeRaidLifecycleState, and
// the next clear of the same phase re-enters this function with the status still
// PhaseActive. Live 2026-09-17 07:26:44Z: character 22 cleared the heart twice in
// 92ms and both members received msg 0x0272 twice. The raid state therefore carries
// its own one-shot claim per phase, which makes the banner independent of how many
// times the completion was observed.
func (s *Service) announceRuntimeRaidClearIfNeeded(state runtimeRaidState, phase byte) {
	if state.Lifecycle.Status != runtimeRaidLifecyclePhaseActive || !runtimeRaidClearedByPhase(state, phase) {
		return
	}
	if !s.claimRuntimeRaidClearAnnouncement(state.RaidKey, phase) {
		s.logPacketEvent("game-raid-clear-announcement-duplicate-skipped",
			"raid_key", state.RaidKey,
			"raid_phase", phase,
			"reason", "clear_banner_already_announced_for_this_phase")
		return
	}
	if err := s.broadcastRuntimeRaidClearAnnouncement(state); err != nil {
		s.logPacketEvent("game-raid-clear-announcement-failed",
			"raid_key", state.RaidKey, "phase", phase, "error", fmt.Sprint(err))
	}
}

// formatRuntimeRaidMemberIDs renders a member list for one log field.
func formatRuntimeRaidMemberIDs(members []uint16) string {
	if len(members) == 0 {
		return ""
	}
	parts := make([]string, 0, len(members))
	for _, member := range members {
		parts = append(parts, strconv.Itoa(int(member)))
	}
	return strings.Join(parts, ",")
}
