// raid_online.go 负责当前 game 连接内的攻坚队运行态流程。
// 这里只保存在线会话可见的短生命周期攻坚队，不落 DB；跨服持久 owner 后续再替换该边界。
package dnfbridge

import (
	"bytes"
	"context"
	"encoding/binary"
	"errors"
	"fmt"
	"hash/fnv"
	"math/rand/v2"
	"sort"
	"strconv"
	"strings"
	"time"

	"longheng.io/server/internal/modules/dnf/alignedcmd"
	"longheng.io/server/internal/modules/dnf/dnfenum"
	"longheng.io/server/internal/modules/dnf/party"
	dnfproto "longheng.io/server/internal/modules/dnf/protocol"
	"longheng.io/server/internal/modules/dnf/raid"
	dnfrepo "longheng.io/server/internal/modules/dnf/repository"
)

const (
	runtimeRaidDefaultGroup = 1
	// runtimeRaidMaxGroups bounds the attack-squad index a leader may address.
	// Five is NOT the client's limit: 86JP pages the raid situation/manager list
	// five groups at a time (RaidManager.cs:837-842 `groupsPerPage = 5`), and the
	// current client's op669 sender has been observed emitting targets 2..10
	// (2026-09-15 16:02: 2/3/4 then 10/9/8/7 then 5/6; 2026-09-17 10:23: 2/3/4/5/6),
	// i.e. the second page is a real addressable range. Every value above the old
	// cap of 5 was dropped with `game-raid-manager-rejected`, which is
	// indistinguishable in game from "改编制没反应".
	runtimeRaidMaxGroups   = 10
	runtimeRaidGroupSize   = 4
	runtimeRaidFailureCode = 3
	runtimeRaidEntryItemID = 10096296
	runtimeRaidEntryCount  = 1
	// runtimeRaidBrowseTTL bounds how long an op1359 browse target stays live
	// after the request that named it. The client polls while its attack-party
	// window is open, so a target that has gone unrefreshed for this long belongs
	// to a window the player already closed and must stop receiving roster pushes.
	runtimeRaidBrowseTTL = 30 * time.Second
	// runtimeRaidJoinReasonGeneric is reason 0x13 of the class-0 op821 notice
	// handler sub_1D0F150, which renders string id 704 "对方无法加入队伍". The
	// handler's 174-entry reason table gives a message to only four codes
	// (0x13 / 0x3C / 0xBF / 0xC0) and stays silent for every other value, so a
	// refused admission has to use this one or report nothing at all.
	runtimeRaidJoinReasonGeneric byte = 0x13
)

var errRuntimeRaidEntryCost = errors.New("raid entry cost is insufficient")

type runtimeRaidState struct {
	RaidKey         uint32
	LeaderID        uint16
	RaidMode        byte
	RouteOrRaidType byte
	NameBytes       []byte
	TailFlag        byte
	SingleRaidType  byte
	AntonConfig     *raid.AntonConfig
	LukeConfig      *raid.LukeConfig
	// FiendWarConfig 只在 RaidMode == runtimeRaidOwnerFiendWar 时非 nil。它的
	// Battle.Raid 是一个合成的 *raid.AntonConfig（PhaseMax=1），所以
	// runtimeRaidBaseConfig 对超时空返回 config.Battle.Raid，走的是与安徒恩/卢克
	// 完全相同的阶段/名额/入场费骨架。
	FiendWarConfig  *raid.FiendWarConfig
	Members         []runtimeRaidMemberState
	Started         bool
	StartPending    bool
	StartAttempt    uint64
	EntryCostPaid   bool
	ChargedMembers  []uint16
	FrozenMembers   []runtimeRaidMemberState
	PendingCostSync []runtimeRaidEntryCostUpdate
	Lifecycle       runtimeRaidLifecycleState
	// SquadParties is the squad -> managed party projection table: it records
	// which ordinary party this service created for each non-zero attack squad.
	// It is the bookkeeping behind the raid's "one non-zero squad <-> one real
	// ordinary party" invariant, and it is what makes it safe to refresh or
	// dissolve that party when a squad changes without ever touching a party the
	// player formed themselves. A squad that is absent here owns no projection.
	SquadParties map[byte]uint16
	// SquadPartyLeaders is the squad -> leader of the ordinary party that squad
	// fights in. It is what the client's per-member 队长标志 is filled from once the
	// raid is running (see runtimeRaidMemberLeadsSquad): the record's leader bool is
	// a boolean to the client -- it stores "field == 1" at member+0x42 and every
	// leader-gated raid control tests its OWN record -- so it has to name the leader
	// the player can actually act as, which is the squad's, not the 团长's.
	// Written wherever an ordinary party is published or projected, dropped when
	// that leader leaves its squad.
	SquadPartyLeaders map[byte]uint16
}

type runtimeRaidMemberState struct {
	CharID     uint16
	Name       string
	Level      byte
	Job        byte
	Grow       byte
	GroupIndex byte
	SlotOrder  byte
	UserState  byte
	HPPercent  byte
	MPPercent  byte
	Waiting    byte
	// RejoinPosition 是这个成员上一次在超时空城镇里被确认过的坐标
	// (captureRuntimeFiendWarRejoinPosition)。它只作为诊断留存：进程重连一律回
	// PVF 等待室起点，不回这个坐标 —— 见 runtimeFiendWarRejoinLocation 的说明。
	RejoinPosition currentTownPositionSnapshot
}

// runtimeRaidDirectorySubscription remembers the last native op831 query for
// an open Find Raid window. Access is guarded by Service.mu.
type runtimeRaidDirectorySubscription struct {
	Active  bool
	Request raid.OtherChannelListRequest
}

type runtimeRaidDirectorySubscriber struct {
	Session *gameSession
	Request raid.OtherChannelListRequest
}

func (s *Service) handleOnlineRaidCommand(session *gameSession, typ uint16, body []byte) (bool, error) {
	if session == nil {
		return false, nil
	}
	// The public raid page can be queried before the client has completed its
	// raid-channel transition. Keep that read-only directory request available,
	// while preserving the current session/channel gate for every mutation.
	if dnfenum.CmdPacket(typ) == dnfenum.CmdPacketRaidOtherChannelList {
		return true, s.handleOnlineOtherChannelList(session, typ, body)
	}
	// C2S 820 comes from the "寻找攻坚队" window rather than from a clicked actor,
	// so it is reachable on a channel the applicant has not moved into yet. It is
	// admitted through the same pending-prompt machinery as the actor-click path,
	// which is what keeps the team-channel gate out of this branch.
	if dnfenum.CmdPacket(typ) == dnfenum.CmdPacketRaidOtherChannelRequestJoin {
		return true, s.handleOnlineRaidOtherChannelRequestJoin(session, typ, body)
	}
	if runtimeRaidModeForSession(session) == 0 {
		return false, nil
	}
	switch typ {
	case raid.RaidResultStageRequestMsgID:
		return true, s.handleRuntimeRaidResultStage(session, body)
	case raid.RaidRewardCardRequestMsgID:
		return true, s.handleRuntimeRaidRewardCard(session, body)
	case raid.RaidDoBehaviorRequestMsgID:
		return true, s.handleRuntimeRaidDoBehavior(session, body)
	case raid.RaidMessageRequestMsgID:
		return true, s.handleRuntimeRaidMessage(session, body)
	}
	switch dnfenum.CmdPacket(typ) {
	// 超时空的 BOSS 机制上报走自己的 opcode (1511)。它是 C2S，不是面板包；
	// 不接这条，双 BOSS 的四机制永远推不完，两个 BOSS 区域就永远开不了门。
	case dnfenum.CmdPacketFiendWarBossProc:
		return true, s.handleRuntimeFiendWarBoss(session, body)
	case dnfenum.CmdPacketCreateRaid:
		return true, s.handleOnlineCreateRaid(session, typ, body)
	case dnfenum.CmdPacketRequestRaidInfo:
		return true, s.handleOnlineRequestRaidInfo(session, typ, body)
	case dnfenum.CmdPacketModifyRaidInfo:
		return true, s.handleOnlineModifyRaidInfo(session, typ, body)
	case dnfenum.CmdPacketLeaveRaid:
		return true, s.handleOnlineLeaveRaid(session, typ, body)
	case dnfenum.CmdPacketStartRaid:
		return true, s.handleOnlineStartRaid(session, typ, body)
	case dnfenum.CmdPacketRaidEntryCostInfo:
		return true, s.handleOnlineRaidEntryCostInfo(session, typ, body)
	case dnfenum.CmdPacketRaidSetSymbol:
		return true, s.handleOnlineRaidSetSymbol(session, typ, body)
	case dnfenum.CmdPacketSetRaidWaiting:
		return true, s.handleOnlineSetRaidWaiting(session, typ, body)
	case dnfenum.CmdPacketRejoinRaid:
		return true, s.handleOnlineRejoinRaid(session, typ, body)
	case dnfenum.CmdPacketRaidManagerWork:
		return true, s.handleOnlineRaidManagerWork(session, typ, body)
	case dnfenum.CmdPacketRaidMemberChangeState:
		return true, s.handleOnlineRaidMemberChangeState(session, typ, body)
	case dnfenum.CmdPacketRaidMonsterHp:
		return s.handleOnlineRaidSituationMonsterHp(session, typ, body)
	case dnfenum.CmdPacketRaidBuffSystem:
		return s.handleOnlineRaidBuffSystem(session, typ, body)
	default:
		return false, nil
	}
}

// handleOnlineOtherChannelList serves the native public-raid object page.
// Current EXE sub_1D7AA10 registers class-0/op753 for this window and consumes
// total count, returned offset, and complete sub_17B8910 raid objects. op884 is
// a different cross-channel page container and cannot populate the attack-raid
// manager vector used by the “寻找攻坚队” rows.
func (s *Service) handleOnlineOtherChannelList(session *gameSession, typ uint16, body []byte) error {
	req, err := raid.DecodeOtherChannelListRequest(body)
	if err != nil {
		s.logGameEvent(session, "game-raid-other-channel-list-body-invalid", "type", typ, "body_len", len(body), "error", err)
		return s.sendRaidCommandFailure(session, typ)
	}
	s.storeRuntimeRaidDirectorySubscription(session, req)
	return s.sendRuntimePublicRaidDirectoryPage(session, req, "client_query")
}

// handleOnlineRaidOtherChannelRequestJoin serves C2S 820, the application the
// "寻找攻坚队" window sends when the applicant cannot simply click the leader's
// actor -- a different town area on the same channel, or another channel
// entirely. Before this handler existed the opcode reached only the module-level
// stub, which answers nothing, so the leader was never told.
//
// The wire layout is read from the client's own sender sub_17B1530, which uses
// the generic body writers: sub_3454FC0 advances the cursor by one byte,
// sub_3454FF0 by two, sub_3455020 by four. The body is therefore
// u8 + u16 + u32 + u16, which is exactly what
// raid.DecodeOtherChannelRequestJoinRequest already parses.
//
// The first two fields are the raid key, not "a channel plus a mode": the sender
// passes byte[raidObj+0x2E] and word[raidObj+0x2C], i.e. key bytes 2 and 0..1.
// A live sample of mode=23 target=1 belongs to raid_key 0x00170001, which is the
// key the same connection was browsing at that moment.
func (s *Service) handleOnlineRaidOtherChannelRequestJoin(session *gameSession, typ uint16, body []byte) error {
	req, err := raid.DecodeOtherChannelRequestJoinRequest(body)
	if err != nil {
		s.logGameEvent(session, "game-raid-request-join-body-invalid",
			"type", typ, "body_len", len(body), "error", err)
		return s.sendRuntimeRaidJoinRejected(session, runtimeRaidJoinReasonGeneric)
	}
	raidKey := uint32(req.Mode)<<16 | uint32(req.TargetKey)
	state, ok := s.runtimeOpenRaidByKey(raidKey)
	if !ok || !runtimeRaidOpenForPeerAdmission(state) {
		s.logGameEvent(session, "game-raid-request-join-rejected",
			"type", typ,
			"raid_key", raidKey,
			"request_mode", req.Mode,
			"request_target", req.TargetKey,
			"has_raid", ok,
			"reason", "raid_not_open_for_admission")
		return s.sendRuntimeRaidJoinRejected(session, runtimeRaidJoinReasonGeneric)
	}
	// The applicant is always the outsider in this path, so the admission target
	// is the leader rather than whoever the request named. From here the flow is
	// the already-proven one: op7 prompt, op11 response, admitRuntimeRaidPeer,
	// and the 664 binding that brings the applicant's own raid object up.
	handled, err := s.recordRuntimeRaidPeerAdmission(session, party.RequestPeerRequest{
		TargetID: state.LeaderID,
		Mode:     runtimeRaidPeerRequestMode,
		Value0:   req.ClientValue,
	})
	if err != nil {
		return err
	}
	s.logGameEvent(session, "game-raid-request-join-served",
		"type", typ,
		"raid_key", raidKey,
		"leader_id", state.LeaderID,
		"handled", handled,
		"member_count", len(state.Members),
		"source", "op820_browse_window_application")
	return nil
}

// sendRuntimeRaidJoinRejected answers a refused C2S 820.
//
// The only class-0 handler registered for op820 and op821 is sub_1D0F150, a
// "u8 result + u8 reason" notice: a non-zero result returns immediately and a
// zero result indexes a 174-entry table by (reason - 0x13). Only four reasons
// carry a message -- 0x13 对方无法加入队伍, 0x3C 个人商店开设中，无法组队,
// 0xBF 对方正作为佣兵出战中, 0xC0 商店开设中，无法组队。 -- and every other code
// renders nothing, which is why the generic one is the only safe choice here.
func (s *Service) sendRuntimeRaidJoinRejected(session *gameSession, reason byte) error {
	body := raid.BuildOtherChannelResponseJoinBody(0x00, reason)
	return s.sendGameUpperRawClassCodec(session, raid.RaidOtherChannelResponseJoinMsgID, body, 0, true)
}

func (s *Service) sendRuntimePublicRaidDirectoryPage(session *gameSession, req raid.OtherChannelListRequest, source string) error {
	typ := uint16(dnfenum.CmdPacketRaidOtherChannelList)
	states, totalCount, offset := s.runtimePublicRaidDirectoryPage(session, req)
	objects := make([]raid.RaidObjectMode0, 0, len(states))
	for _, state := range states {
		objects = append(objects, runtimeRaidObjectMode0(state))
	}
	pageBody, err := raid.BuildPublicRaidListBody(raid.PublicRaidListPage{
		TotalCount: totalCount,
		Offset:     offset,
		Objects:    objects,
	})
	if err != nil {
		s.logGameEvent(session, "game-raid-public-list-build-failed", "type", typ, "row_count", len(objects), "error", err)
		return s.sendRaidCommandFailure(session, typ)
	}
	if err := s.sendGameUpperRawClassCodec(session, raid.RaidPublicListMsgID, pageBody, 0, true); err != nil {
		return err
	}
	s.logGameEvent(session, "game-raid-other-channel-list-served",
		"type", typ,
		"source", source,
		"mode", req.Mode,
		"has_context", req.HasContext,
		"name_len", len(req.NameBytes),
		"response_type", raid.RaidPublicListMsgID,
		"response_body_len", len(pageBody),
		"total_count", totalCount,
		"offset", offset,
		"row_count", len(objects))
	return nil
}

func cloneRuntimeRaidDirectoryRequest(req raid.OtherChannelListRequest) raid.OtherChannelListRequest {
	req.NameBytes = append([]byte(nil), req.NameBytes...)
	return req
}

func (s *Service) storeRuntimeRaidDirectorySubscription(session *gameSession, req raid.OtherChannelListRequest) {
	if s == nil || session == nil {
		return
	}
	s.mu.Lock()
	session.raidDirectory = runtimeRaidDirectorySubscription{
		Active:  true,
		Request: cloneRuntimeRaidDirectoryRequest(req),
	}
	s.mu.Unlock()
}

func (s *Service) runtimeRaidDirectorySubscribers() []runtimeRaidDirectorySubscriber {
	if s == nil {
		return nil
	}
	s.mu.Lock()
	rows := make([]runtimeRaidDirectorySubscriber, 0, len(s.gameSessions))
	seen := make(map[*gameSession]struct{}, len(s.gameSessions))
	for _, session := range s.gameSessions {
		if session == nil || !session.raidDirectory.Active || session.conn == nil || session.selectedCharacterID == 0 {
			continue
		}
		if _, duplicate := seen[session]; duplicate {
			continue
		}
		seen[session] = struct{}{}
		rows = append(rows, runtimeRaidDirectorySubscriber{
			Session: session,
			Request: cloneRuntimeRaidDirectoryRequest(session.raidDirectory.Request),
		})
	}
	s.mu.Unlock()
	sort.Slice(rows, func(i, j int) bool {
		return rows[i].Session.selectedCharacterID < rows[j].Session.selectedCharacterID
	})
	return rows
}

// refreshRuntimePublicRaidDirectories pushes a fresh op753 snapshot to every
// currently open native directory window. A committed raid mutation must not
// be reported as failed merely because one observer disconnected mid-push.
func (s *Service) refreshRuntimePublicRaidDirectories(source string) {
	for _, subscriber := range s.runtimeRaidDirectorySubscribers() {
		if err := s.sendRuntimePublicRaidDirectoryPage(subscriber.Session, subscriber.Request, source); err != nil {
			s.logGameEvent(subscriber.Session, "game-raid-public-list-refresh-failed", "source", source, "error", err)
		}
	}
}

func (s *Service) runtimePublicRaidDirectoryPage(session *gameSession, req raid.OtherChannelListRequest) ([]runtimeRaidState, uint32, uint32) {
	var requestedOffset uint32
	var myChannelOnly bool
	var recruitingOnly bool
	if req.HasContext {
		requestedOffset = binary.LittleEndian.Uint32(req.Context[0:4])
		// sub_1B4B520 writes the two checkbox controls into the last two
		// context bytes: “我的频道” then “只查看招募中的攻坚队”.
		myChannelOnly = req.Context[6] != 0
		recruitingOnly = req.Context[7] != 0
	}

	s.raidMu.Lock()
	states := make([]runtimeRaidState, 0, len(s.raids))
	for _, state := range s.raids {
		if state == nil || state.TailFlag != 0 {
			continue
		}
		if recruitingOnly && (state.StartPending || state.Started) {
			continue
		}
		if myChannelOnly && session != nil && uint16(state.RaidKey>>16) != uint16(session.channel.ID) {
			continue
		}
		if len(req.NameBytes) != 0 && !bytes.Contains(state.NameBytes, req.NameBytes) {
			continue
		}
		states = append(states, cloneRuntimeRaidState(*state))
	}
	s.raidMu.Unlock()

	sort.Slice(states, func(i, j int) bool { return states[i].RaidKey < states[j].RaidKey })
	totalCount := uint32(len(states))
	if requestedOffset >= totalCount {
		requestedOffset = 0
	}
	end := requestedOffset + raid.RaidPublicListPageSize
	if end > totalCount {
		end = totalCount
	}
	return states[int(requestedOffset):int(end)], totalCount, requestedOffset
}

func (s *Service) handleOnlineRequestRaidInfo(session *gameSession, typ uint16, body []byte) error {
	req, err := raid.DecodeRequestRaidInfoRequest(body)
	if err != nil {
		s.logGameEvent(session, "game-raid-info-body-invalid", "type", typ, "body_len", len(body), "error", err)
		return s.sendRaidCommandFailure(session, typ)
	}
	state, hasRuntimeRaid := s.runtimeRaidForMember(selectedCharacterID(session))
	raidKey := uint32(0)
	if hasRuntimeRaid {
		raidKey = state.RaidKey
	}
	// The current class-0/op1359 handler sub_1D3A150 -> sub_A42AB0 does not read a
	// body; it only marks the resolved local raid object as present. Directory
	// cooldown initialization is the distinct class-0/op1228 notification.
	//
	// Measured: withholding this acknowledgement from a browsing non-member does
	// NOT stop the client adopting the raid, so it is always sent. Observed op1359
	// traffic uses mode 1; mode 2 is what the client requester sub_17B1A90 builds,
	// and the browsing flow never emitted it.
	if err := s.sendGameUpperRawClassCodec(session, raid.RaidInfoMsgID, raid.BuildRaidInfoResponseBody(), 0, true); err != nil {
		return err
	}
	s.logGameEvent(session, "game-raid-info-served",
		"type", typ,
		"mode", req.Mode,
		"requested_raid_key", req.RaidKey,
		"raid_key", raidKey,
		"has_runtime_raid", hasRuntimeRaid,
		"body_len", len(req.Raw))
	if hasRuntimeRaid {
		return nil
	}
	// The requester owns no raid, so the attack-party window is open on a raid picked
	// from the public directory. A member receives the waiting list and the material
	// snapshot from the entry-cost path, which is member-gated and answers a browsing
	// outsider with a failure, so nothing ever populated that window for them.
	return s.sendRuntimeRaidBrowseList(session, req)
}

// sendRuntimeRaidObjectRemoved tells one session that a raid object it holds is
// gone. See raid.BuildRaidObjectRemoveBody for why an explicit removal is needed
// at all: every roster push in this service targets surviving members, so neither
// a leaving member nor a browsing outsider is ever told to drop the object.
func (s *Service) sendRuntimeRaidObjectRemoved(session *gameSession, raidKey uint32) error {
	if s == nil || session == nil || raidKey == 0 {
		return nil
	}
	return s.sendGameUpperRawClassCodec(session, raid.RaidObjectMsgID, raid.BuildRaidObjectRemoveBody(raidKey), 0, true)
}

// broadcastRuntimeRaidObjectRemoved retires the object for every member of a
// disbanded raid. The leaver is excluded because the leave path already sent it
// directly. 86JP reaches the same set on its disbanded branch by broadcasting its
// remove packet to every previous member.
func (s *Service) broadcastRuntimeRaidObjectRemoved(state runtimeRaidState, excludedCharacterID uint16) error {
	if s == nil || state.RaidKey == 0 {
		return nil
	}
	body := raid.BuildRaidObjectRemoveBody(state.RaidKey)
	var firstErr error
	for _, member := range state.Members {
		if member.CharID == excludedCharacterID {
			continue
		}
		session, ok := s.onlineGameSession(member.CharID)
		if !ok {
			continue
		}
		if err := s.sendGameUpperRawClassCodec(session, raid.RaidObjectMsgID, body, 0, true); err != nil && firstErr == nil {
			firstErr = err
		}
	}
	return firstErr
}

// runtimeOpenRaidOnChannel returns the recruiting team raid scoped to one
// channel. RaidKey carries the catalog channel id in its high word, which is the
// same split the public directory filters on, so a key built for another channel
// can never be served to this requester.
func (s *Service) runtimeOpenRaidOnChannel(channelID uint16) (runtimeRaidState, bool) {
	if s == nil || channelID == 0 {
		return runtimeRaidState{}, false
	}
	s.raidMu.Lock()
	defer s.raidMu.Unlock()
	var found *runtimeRaidState
	for _, state := range s.raids {
		if state == nil || state.TailFlag != 0 || state.Started || state.StartPending {
			continue
		}
		if uint16(state.RaidKey>>16) != channelID {
			continue
		}
		if found == nil || state.RaidKey < found.RaidKey {
			found = state
		}
	}
	if found == nil {
		return runtimeRaidState{}, false
	}
	return cloneRuntimeRaidState(*found), true
}

// sendRuntimeRaidBrowseList answers a non-member's op1359 with the complete
// payload the browsing attack-party window needs. A member obtains the roster
// from the entry-cost path, which is member-gated and answers an outsider with a
// failure, so nothing ever populated that window for an outsider.
//
// The payload has two halves, split here because the live-refresh path replays
// them at different rates:
//
//   - identity: the member actor records plus 0x0250, 819 and 0x0256. The roster
//     level and portrait are not carried by any raid packet: the client resolves
//     the 0x0250/0x0333 member key through sub_1A089E0 -> sub_1C5B070, which looks
//     it up in the container at [this+0x278] and does nothing when the lookup
//     misses. A browsing outsider has never met those characters, so every row
//     renders with level 0 and the default portrait. Only a changed member id set
//     needs this half replayed.
//   - roster: 0x024F mode 3. This is the packet that carries each row's subgroup
//     and slot, so it is pushed on every roster mutation.
//
// 0x024F is safe to send to a browsing outsider only because RaidProbe.dll patches
// out the single call inside that handler which made the client adopt the raid:
// the mode 3 branch ends with
//
//	cmp ecx, <own channel> / jne skip / call sub_17C3470
//
// and with that call NOPed the roster still renders and the claim no longer
// happens. The path that legitimately sets the field for real members is the 664
// response handler sub_1D04340, which is not touched.
func (s *Service) sendRuntimeRaidBrowseList(session *gameSession, req raid.RequestRaidInfoRequest) error {
	if s == nil || session == nil {
		return nil
	}
	state, source, ok := s.runtimeRaidBrowseTarget(req.RaidKey, townPresenceChannelID(session))
	// The roster order is shared by the key list and the actor pass below.
	sortedMembers := runtimeRaidSortedMembers(state)

	if !ok {
		s.clearRuntimeRaidBrowseTarget(session)
		return nil
	}
	if err := s.sendRuntimeRaidBrowseMemberActors(session, sortedMembers); err != nil {
		return err
	}
	// No suppression here. A member-id fingerprint used to swallow this whole
	// payload when the roster had not changed, and it was only cleared on a
	// character change, so switching channels and coming back still got nothing.
	// The load is a few hundred bytes and the client only asks while its window is
	// open. The same fingerprint is still used on the refresh path, but there it
	// only decides whether the identity half is replayed -- never whether the
	// mode=3 roster is.
	rosterBody, err := raid.BuildMemberRefreshMode3Body(runtimeRaidMemberRefresh(state))
	if err != nil {
		return err
	}
	if err := s.sendGameUpperRawClassCodec(session, raid.RaidMemberRefreshMsgID, rosterBody, 0, true); err != nil {
		return err
	}
	if err := s.sendRuntimeRaidBrowseKeyLists(session, state, sortedMembers); err != nil {
		return err
	}
	s.markRuntimeRaidBrowseTarget(session, state.RaidKey, runtimeRaidMemberFingerprint(state))
	s.logGameEvent(session, "game-raid-browse-list-sent",
		"raid_key", state.RaidKey,
		"channel_id", uint16(state.RaidKey>>16),
		"channel_source", source,
		"request_mode", req.Mode,
		"leader_id", state.LeaderID,
		"member_count", len(state.Members),
		"waiting_body_len", len(runtimeRaidWaitingListBody(state)),
		"userinfo_keys", len(sortedMembers),
		"source", "op1359_non_member_browse_list")
	return nil
}

// sendRuntimeRaidBrowseMemberActors installs each member as a remote actor in the
// browsing client's town scene, which is what fills the container at [this+0x278]
// that the roster renderer resolves its rows against. The roster packet itself
// carries name and job, but the level and portrait are looked up from that
// container, so a member with no actor record renders as an empty Lv cell.
//
// The lookup goes through peerActorInfoForCharacter, not the presence map
// directly: a member standing in a Seria room is deliberately absent from
// presence, and reading the presence map here meant the browsing window showed a
// blank Lv for exactly that member until it walked out into the public town.
//
// Restricted to the requester's own channel: the coordinates only make sense
// inside one town scene, and cross-channel browsing is already accepted as
// incomplete.
func (s *Service) sendRuntimeRaidBrowseMemberActors(session *gameSession, sortedMembers []runtimeRaidMemberState) error {
	ownChannel := townPresenceChannelID(session)
	// 批次缓存（2026-09-26）。改前这里逐成员调 sendTownRemoteActorState，它一路走到
	// sendTownRemoteActorProjection 并自己造一个 context.Background()，于是两层缓存
	// 全都没有 —— 与 sendChannelWidePeerActors 改前完全相同的毛病（见
	// online_player_manager.go 里那段说明）。读缓存能把这一批里共享的 PVF/配置读取去重。
	ctx, _ := withProjectionReadCache(context.Background())
	ctx = withActorProjectionBodiesCache(ctx)
	for _, member := range sortedMembers {
		actor, ok := s.peerActorInfoForCharacter(member.CharID)
		if !ok || actor.Session == nil {
			s.logGameEvent(session, "game-raid-browse-member-actor-unavailable",
				"char_id", member.CharID,
				"source", "raid_browse_member_actor")
			continue
		}
		if ownChannel == 0 || townPresenceChannelID(actor.Session) != ownChannel {
			continue
		}
		// 只发 mode0（2026-09-26）。本函数的文档写明：roster 自带 name/job，而 level 与
		// portrait 是从这份 actor 记录里查的 —— 那正是 mode0 的职责
		// （见 sendTownRemoteActorRecord 的说明）。mode1 是绑对方装备与技能页的那一半，
		// 名单不需要；而且它在副本里会把对方的技能页绑到本地受控角色上
		// （见 runtimeRaidPeerActorInstallModeFor 的注释）。
		if err := s.sendTownRemoteActorRecordWithContext(ctx, session, actor, "raid_browse_member_actor"); err != nil {
			s.logGameEvent(session, "game-raid-browse-member-actor-failed",
				"char_id", member.CharID,
				"error", err)
		}
	}
	return nil
}

// sendRuntimeRaidBrowseKeyLists sends the two member-key lists the browsing window
// needs to resolve names, jobs and levels, followed by the material snapshot.
//
//   - 0x0250 waiting list: u16 count + rows{u16 memberKey, u8 state}.
//   - 819 carries the raid user key list. The 0x0250 rows hold only a member key and
//     a state byte, so without this the client has no character record to resolve
//     and every roster row renders with level 0 and no portrait. Keys are the member
//     character ids, sorted exactly like the roster.
//   - 0x0256 entry-cost snapshot: the 入场材料 column.
func (s *Service) sendRuntimeRaidBrowseKeyLists(session *gameSession, state runtimeRaidState, sortedMembers []runtimeRaidMemberState) error {
	if err := s.sendGameUpperRawClassCodec(session, raid.RaidWaitingListMsgID, runtimeRaidWaitingListBody(state), 0, true); err != nil {
		return err
	}
	keys := make([]uint32, 0, len(state.Members))
	for _, member := range sortedMembers {
		keys = append(keys, uint32(member.CharID))
	}
	if err := s.sendGameUpperRawClassCodec(session, raid.RaidOtherChannelUserinfoMsgID, raid.BuildOtherChannelUserinfoBody(keys), 0, true); err != nil {
		return err
	}
	return s.sendRuntimeRaidEntryCostSnapshot(session, state)
}

// runtimeRaidSortedMembers returns the roster order shared by every browse packet.
func runtimeRaidSortedMembers(state runtimeRaidState) []runtimeRaidMemberState {
	sortedMembers := append([]runtimeRaidMemberState(nil), state.Members...)
	runtimeRaidSortMembers(sortedMembers)
	return sortedMembers
}

// runtimeRaidMemberFingerprint hashes the member id set alone. A subgroup move
// leaves it unchanged; any add, remove or replacement changes it.
func runtimeRaidMemberFingerprint(state runtimeRaidState) uint64 {
	ids := make([]uint16, 0, len(state.Members))
	for _, member := range state.Members {
		ids = append(ids, member.CharID)
	}
	sort.Slice(ids, func(i, j int) bool { return ids[i] < ids[j] })
	hash := fnv.New64a()
	var raw [2]byte
	for _, id := range ids {
		binary.LittleEndian.PutUint16(raw[:], id)
		_, _ = hash.Write(raw[:])
	}
	return hash.Sum64()
}

// markRuntimeRaidBrowseTarget records that this session's attack-party window is
// open on raidKey, together with the identity fingerprint it was just sent.
func (s *Service) markRuntimeRaidBrowseTarget(session *gameSession, raidKey uint32, memberFingerprint uint64) {
	if s == nil || session == nil {
		return
	}
	// gameplayNow() takes Service.mu internally, so it must be sampled first.
	now := s.gameplayNow()
	s.mu.Lock()
	session.raidBrowseTarget = raidKey
	session.raidBrowseAt = now
	session.raidBrowseMembers = memberFingerprint
	s.mu.Unlock()
}

// clearRuntimeRaidBrowseTarget drops the browse subscription. Called when op1359
// no longer resolves to an open raid, so a stale window stops receiving pushes.
func (s *Service) clearRuntimeRaidBrowseTarget(session *gameSession) {
	if s == nil || session == nil {
		return
	}
	s.mu.Lock()
	session.raidBrowseTarget = 0
	session.raidBrowseAt = time.Time{}
	session.raidBrowseMembers = 0
	s.mu.Unlock()
}

// runtimeRaidBrowseSubscribers lists the live sessions whose attack-party window
// is open on raidKey and which are not themselves members of it. Members are
// excluded because the caller already pushes to them, and a duplicate mode=3
// rebuild would only make the client re-render the roster twice.
func (s *Service) runtimeRaidBrowseSubscribers(raidKey uint32, excluded uint16, members map[uint16]struct{}) []*gameSession {
	if s == nil || raidKey == 0 {
		return nil
	}
	now := s.gameplayNow()
	expiry := now.Add(-runtimeRaidBrowseTTL)
	s.mu.Lock()
	rows := make([]*gameSession, 0, len(s.gameSessions))
	seen := make(map[*gameSession]struct{}, len(s.gameSessions))
	for _, session := range s.gameSessions {
		if session == nil || session.conn == nil || session.selectedCharacterID == 0 {
			continue
		}
		if session.raidBrowseTarget != raidKey || session.raidBrowseAt.Before(expiry) {
			continue
		}
		if _, duplicate := seen[session]; duplicate {
			continue
		}
		if session.selectedCharacterID == excluded {
			continue
		}
		seen[session] = struct{}{}
		if _, isMember := members[session.selectedCharacterID]; isMember {
			continue
		}
		rows = append(rows, session)
	}
	s.mu.Unlock()
	sort.Slice(rows, func(i, j int) bool { return rows[i].selectedCharacterID < rows[j].selectedCharacterID })
	return rows
}

// broadcastRuntimeRaidBrowseRefresh pushes the current roster to every outsider
// currently browsing this raid. The member loop in broadcastRuntimeRaidRefresh
// only walks state.Members, so a leader regrouping its squads produced nothing at
// all for an outsider: the open window kept the old layout until it was closed and
// reopened.
//
// rosterBody is reused from the caller so both paths stay byte-identical.
func (s *Service) broadcastRuntimeRaidBrowseRefresh(state runtimeRaidState, rosterBody []byte, excluded uint16) {
	if s == nil || state.RaidKey == 0 {
		return
	}
	members := make(map[uint16]struct{}, len(state.Members))
	for _, member := range state.Members {
		members[member.CharID] = struct{}{}
	}
	subscribers := s.runtimeRaidBrowseSubscribers(state.RaidKey, excluded, members)
	if len(subscribers) == 0 {
		return
	}
	fingerprint := runtimeRaidMemberFingerprint(state)
	sortedMembers := runtimeRaidSortedMembers(state)
	for _, session := range subscribers {
		if err := s.sendRuntimeRaidBrowseRoster(session, state, sortedMembers, rosterBody, fingerprint); err != nil {
			s.logGameEvent(session, "game-raid-browse-refresh-failed",
				"raid_key", state.RaidKey,
				"error", err)
		}
	}
}

// sendRuntimeRaidBrowseRoster replays the browsed raid into one outsider window.
// The mode=3 roster always goes out; the identity half only when the member id set
// changed, because a brand new member has no key list entry and would otherwise
// render as a nameless level-0 row.
func (s *Service) sendRuntimeRaidBrowseRoster(
	session *gameSession,
	state runtimeRaidState,
	sortedMembers []runtimeRaidMemberState,
	rosterBody []byte,
	fingerprint uint64,
) error {
	now := s.gameplayNow()
	s.mu.Lock()
	identityStale := session.raidBrowseMembers != fingerprint
	session.raidBrowseMembers = fingerprint
	session.raidBrowseAt = now
	s.mu.Unlock()

	if identityStale {
		if err := s.sendRuntimeRaidBrowseMemberActors(session, sortedMembers); err != nil {
			return err
		}
	}
	if err := s.sendGameUpperRawClassCodec(session, raid.RaidMemberRefreshMsgID, rosterBody, 0, true); err != nil {
		return err
	}
	if !identityStale {
		return nil
	}
	if err := s.sendRuntimeRaidBrowseKeyLists(session, state, sortedMembers); err != nil {
		return err
	}
	s.logGameEvent(session, "game-raid-browse-identity-refreshed",
		"raid_key", state.RaidKey,
		"member_count", len(state.Members),
		"source", "roster_mutation_member_set_changed")
	return nil
}

// runtimeRaidBrowseTarget resolves which raid an op1359 request names.
//
// The body is u8 mode + u32 raid_key (see raid.DecodeRequestRaidInfoRequest), so the
// key is read directly instead of being guessed from a channel. The requester's own
// channel is only a fallback for a body that carries no key, so a shape change
// degrades to the old behaviour instead of hiding the raid.
func (s *Service) runtimeRaidBrowseTarget(raidKey uint32, sessionChannel uint16) (runtimeRaidState, string, bool) {
	if state, ok := s.runtimeOpenRaidByKey(raidKey); ok {
		return state, "op1359_raid_key", true
	}
	if state, ok := s.runtimeOpenRaidOnChannel(sessionChannel); ok {
		return state, "session_channel_fallback", true
	}
	return runtimeRaidState{}, "", false
}

// runtimeOpenRaidByKey returns the recruiting team raid with this exact key.
func (s *Service) runtimeOpenRaidByKey(raidKey uint32) (runtimeRaidState, bool) {
	if s == nil || raidKey == 0 {
		return runtimeRaidState{}, false
	}
	s.raidMu.Lock()
	defer s.raidMu.Unlock()
	state := s.raids[raidKey]
	if state == nil || state.TailFlag != 0 || state.Started || state.StartPending {
		return runtimeRaidState{}, false
	}
	return cloneRuntimeRaidState(*state), true
}

func (s *Service) handleOnlineRaidEntryCostInfo(session *gameSession, typ uint16, body []byte) error {
	if _, err := raid.DecodeEntryCostInfoRequest(body); err != nil {
		s.logGameEvent(session, "game-raid-entry-cost-body-invalid", "type", typ, "body_len", len(body), "error", err)
		return s.sendRaidCommandFailure(session, typ)
	}
	state, ok := s.runtimeRaidForMember(selectedCharacterID(session))
	if !ok {
		return s.handleOnlineRaidBrowseEntryCostInfo(session, typ)
	}
	if err := s.sendGameUpperRawClassCodec(session, typ, raid.BuildRaidCommandSuccessBody(), dnfproto.DefaultChannelClassification, true); err != nil {
		return err
	}
	if err := s.sendGameUpperRawClassCodec(session, raid.RaidWaitingListMsgID, runtimeRaidWaitingListBody(state), 0, true); err != nil {
		return err
	}
	// Broadcast rather than answer only the requester. A member sends this request when
	// its own material state changes, and answering only that one session left every
	// other member on a stale snapshot: the item showed up in the requester window and
	// nowhere else.
	return s.broadcastRuntimeRaidEntryCost(state)
}

// handleOnlineRaidBrowseEntryCostInfo answers the same request from a session that
// is only browsing a raid it has not joined.
//
// The member gate alone used to refuse it outright. That refusal is measurable on
// the wire: the same connection received a 598 (0x0256) snapshot whose every row
// already read field=1 value=1, while the 入场材料 column still rendered "-",
// because the client learns *which* item the raid charges from this ACK and 0x0256
// only reports who has it.
//
// Only the read-only half of the member answer is reproduced -- the success byte,
// the waiting list and the material snapshot. Nothing here claims the raid: the
// 0x024F object and the 664 binding stay where they are, which is what keeps a
// browsing outsider from adopting the raid.
func (s *Service) handleOnlineRaidBrowseEntryCostInfo(session *gameSession, typ uint16) error {
	state, ok := s.runtimeRaidBrowseSubscription(session)
	if !ok {
		return s.sendRaidCommandFailure(session, typ)
	}
	if err := s.sendGameUpperRawClassCodec(session, typ, raid.BuildRaidCommandSuccessBody(), dnfproto.DefaultChannelClassification, true); err != nil {
		return err
	}
	if err := s.sendGameUpperRawClassCodec(session, raid.RaidWaitingListMsgID, runtimeRaidWaitingListBody(state), 0, true); err != nil {
		return err
	}
	if err := s.sendRuntimeRaidEntryCostSnapshot(session, state); err != nil {
		return err
	}
	s.logGameEvent(session, "game-raid-browse-entry-cost-served",
		"type", typ,
		"raid_key", state.RaidKey,
		"member_count", len(state.Members),
		"source", "op658_browsing_outsider_read_only_ack")
	return nil
}

// runtimeRaidBrowseSubscription resolves the raid a session's attack-party window
// is currently open on, reusing the same open-raid rule as the op1359 browse list.
func (s *Service) runtimeRaidBrowseSubscription(session *gameSession) (runtimeRaidState, bool) {
	if s == nil || session == nil {
		return runtimeRaidState{}, false
	}
	s.mu.Lock()
	raidKey := session.raidBrowseTarget
	s.mu.Unlock()
	if raidKey == 0 {
		return runtimeRaidState{}, false
	}
	return s.runtimeOpenRaidByKey(raidKey)
}

// broadcastRuntimeRaidEntryCost sends the 0x0256 material snapshot to every online
// member of the raid.
func (s *Service) broadcastRuntimeRaidEntryCost(state runtimeRaidState) error {
	var firstErr error
	for _, member := range state.Members {
		memberSession, online := s.onlineGameSession(member.CharID)
		if !online {
			continue
		}
		if err := s.sendRuntimeRaidEntryCostSnapshot(memberSession, state); err != nil && firstErr == nil {
			firstErr = err
		}
	}
	return firstErr
}

// handleOnlineRaidSetSymbol stores the native single-raid route selector.
// The current client sends symbol values 0, 1, or 2; the PVF trigger graph
// names the corresponding branches 0, 3, and 6. This command is one-way in
// the current EXE, so no synthetic ACK is emitted.
func (s *Service) handleOnlineRaidSetSymbol(session *gameSession, typ uint16, body []byte) error {
	req, err := raid.DecodeSetSymbolRequest(body)
	if err != nil {
		s.logGameEvent(session, "game-raid-set-symbol-body-invalid", "type", typ, "body_len", len(body), "error", err)
		return nil
	}
	characterID := selectedCharacterID(session)
	if characterID == 0 {
		return nil
	}
	// Inside a started attack party the same body is a dungeon script's
	// [RAID SET SYMBOL] (symbol, operand, operation): Luke's phase-one generator
	// report or Anton's phase-two shield report.
	if current, ok := s.runtimeRaidForMember(characterID); ok && current.TailFlag != 1 && current.Started {
		// 卢克一阶段的发电机上报（op662）先认领；认领不到才落回安徒恩的护盾上报。
		// 安徒恩状态永远认领不到（owner != Luke），路径逐字不变。
		if s.applyRuntimeRaidLukeGeneratorReport(session, req) {
			return s.sendGameUpperRawClassCodec(session, typ, raid.BuildRaidCommandSuccessBody(), dnfproto.DefaultChannelClassification, true)
		}
		handled, err := s.applyRuntimeRaidBarrierRequest(session, req)
		if err != nil || !handled {
			return err
		}
		return s.sendGameUpperRawClassCodec(session, typ, raid.BuildRaidCommandSuccessBody(), dnfproto.DefaultChannelClassification, true)
	}
	s.raidMu.Lock()
	state := s.runtimeRaidByMemberLocked(characterID)
	if state == nil || state.TailFlag != 1 || state.Started || state.StartPending {
		s.raidMu.Unlock()
		return nil
	}
	if state.RaidMode == runtimeRaidOwnerLuke {
		if req.Symbol == 0 {
			state.SingleRaidType = runtimeRaidLukeLightRoute
		} else {
			state.SingleRaidType = runtimeRaidLukeDarkRoute
		}
	} else {
		state.SingleRaidType = req.Symbol * 3
	}
	updated := cloneRuntimeRaidState(*state)
	s.raidMu.Unlock()
	s.logGameEvent(session, "game-raid-single-route-selected",
		"type", typ,
		"raid_key", updated.RaidKey,
		"source_value", req.SourceValue,
		"target_value", req.TargetValue,
		"symbol", req.Symbol,
		"single_raid_type", updated.SingleRaidType)
	return nil
}

func (s *Service) handleOnlineSetRaidWaiting(session *gameSession, typ uint16, body []byte) error {
	req, err := raid.DecodeSetWaitingRequest(body)
	if err != nil {
		s.logGameEvent(session, "game-raid-waiting-body-invalid", "type", typ, "body_len", len(body), "error", err)
		return s.sendRaidCommandFailure(session, typ)
	}
	state, row, ok := s.setRuntimeRaidWaiting(selectedCharacterID(session), req.RouteRaidType)
	if !ok {
		return s.sendRaidCommandFailure(session, typ)
	}
	if err := s.sendGameUpperRawClassCodec(session, typ, raid.BuildRaidCommandSuccessBody(), dnfproto.DefaultChannelClassification, true); err != nil {
		return err
	}
	body = raid.BuildWaitingChangeBody(0, row)
	for _, member := range state.Members {
		memberSession, online := s.onlineGameSession(member.CharID)
		if !online {
			continue
		}
		if err := s.sendGameUpperRawClassCodec(memberSession, raid.RaidWaitingChangeMsgID, body, 0, true); err != nil {
			return err
		}
	}
	return nil
}

func (s *Service) sendRuntimeRaidEntryCostSnapshot(session *gameSession, state runtimeRaidState) error {
	return s.sendGameUpperRawClassCodec(session, raid.RaidEntryCostMsgID, s.runtimeRaidEntryCostSnapshotBody(state), 0, true)
}

func (s *Service) runtimeRaidEntryCostSnapshotBody(state runtimeRaidState) []byte {
	entryItemID, entryCount := runtimeRaidEntryCostForState(state)
	rows := make([]raid.EntryCostRow, 0, len(state.Members))
	repositories, repositoriesOK := s.repositoryGroup()
	ctx, cancel := context.WithTimeout(context.Background(), createWriteTimeout)
	defer cancel()
	members := append([]runtimeRaidMemberState(nil), state.Members...)
	runtimeRaidSortMembers(members)
	for _, member := range members {
		row := raid.EntryCostRow{MemberKey: member.CharID}
		if state.EntryCostPaid || runtimeRaidMemberHasEntryCost(ctx, repositories, repositoriesOK, member.CharID, entryItemID, entryCount) {
			row.Field = 1
			row.Value = 1
		}
		rows = append(rows, row)
	}
	return raid.BuildEntryCostBody(rows)
}

func runtimeRaidMemberHasEntryCost(ctx context.Context, repositories dnfrepo.Group, repositoriesOK bool, characterID uint16, itemID uint32, count uint32) bool {
	if !repositoriesOK || repositories.Inventory == nil || characterID == 0 {
		return false
	}
	record, found, err := repositories.Inventory.Load(ctx, strconv.Itoa(int(characterID)))
	if err != nil || !found {
		return false
	}
	_, ok := currentEpicProductionFindMaterialSlot(record, itemID, count)
	return ok
}

func runtimeRaidEntryCostForState(state runtimeRaidState) (uint32, uint32) {
	config := runtimeRaidBaseConfig(state)
	if config != nil && config.EntryCost.ItemID != 0 && config.EntryCost.Count != 0 {
		return config.EntryCost.ItemID, config.EntryCost.Count
	}
	return runtimeRaidEntryItemID, runtimeRaidEntryCount
}

func runtimeRaidWaitingListBody(state runtimeRaidState) []byte {
	members := append([]runtimeRaidMemberState(nil), state.Members...)
	runtimeRaidSortMembers(members)
	rows := make([]raid.WaitingRow, 0, len(members))
	for _, member := range members {
		rows = append(rows, raid.WaitingRow{MemberKey: member.CharID, State: member.Waiting})
	}
	return raid.BuildWaitingListBody(rows)
}

type runtimeRaidEntryCostUpdate struct {
	CharacterID uint16
	Entries     []currentItemListEntry
}

func (s *Service) consumeRuntimeRaidEntryCost(state runtimeRaidState) ([]runtimeRaidEntryCostUpdate, error) {
	// 超时空的入场费走 runtimeRaidBaseConfig -> Battle.Raid.EntryCost，与安徒恩/
	// 卢克同一条路径；账号日/周次数由 reserveRuntimeRaidAdmissionCounters 统一扣
	// (currentRaidAdmissionPolicyKeys 现在含 FiendWar 三个 key)，不再另起一套。
	batch, err := s.consumeRuntimeRaidEntryCostBatch(state)
	if err != nil || batch == nil {
		return nil, err
	}
	return cloneRuntimeRaidEntryCostUpdates(batch.updates), nil
}

type runtimeRaidEntryCostBatch struct {
	characterIDs []string
	before       []dnfrepo.InventoryRecord
	updates      []runtimeRaidEntryCostUpdate
	restored     bool
}

func (s *Service) consumeRuntimeRaidEntryCostBatch(state runtimeRaidState) (*runtimeRaidEntryCostBatch, error) {
	repositories, ok := s.repositoryGroup()
	if !ok || repositories.RaidEntryCost == nil {
		return nil, dnfrepo.ErrRaidEntryCostTransactionUnavailable
	}
	members := append([]runtimeRaidMemberState(nil), state.Members...)
	runtimeRaidSortMembers(members)
	entryItemID, entryCount := runtimeRaidEntryCostForState(state)
	characterIDs := make([]string, 0, len(members))
	for _, member := range members {
		characterIDs = append(characterIDs, strconv.Itoa(int(member.CharID)))
	}
	batch := &runtimeRaidEntryCostBatch{
		characterIDs: append([]string(nil), characterIDs...),
		before:       make([]dnfrepo.InventoryRecord, 0, len(members)),
		updates:      make([]runtimeRaidEntryCostUpdate, 0, len(members)),
	}
	ctx, cancel := context.WithTimeout(context.Background(), createWriteTimeout)
	defer cancel()
	err := repositories.RaidEntryCost.WithinRaidEntryCost(ctx, characterIDs, func(inventories dnfrepo.InventoryRepository) error {
		for _, member := range members {
			characterID := strconv.Itoa(int(member.CharID))
			record, found, loadErr := inventories.Load(ctx, characterID)
			if loadErr != nil {
				return loadErr
			}
			if !found {
				return fmt.Errorf("%w: character=%d inventory missing", errRuntimeRaidEntryCost, member.CharID)
			}
			batch.before = append(batch.before, dnfrepo.CloneInventory(record))
			slot, found := currentEpicProductionFindMaterialSlot(record, entryItemID, entryCount)
			if !found {
				return fmt.Errorf("%w: character=%d item=%d count=%d", errRuntimeRaidEntryCost, member.CharID, entryItemID, entryCount)
			}
			entry, consumeErr := currentEpicProductionConsumeStack(&record, slot, entryItemID, entryCount)
			if consumeErr != nil {
				return fmt.Errorf("%w: character=%d: %v", errRuntimeRaidEntryCost, member.CharID, consumeErr)
			}
			if saveErr := inventories.Save(ctx, record); saveErr != nil {
				return saveErr
			}
			batch.updates = append(batch.updates, runtimeRaidEntryCostUpdate{CharacterID: member.CharID, Entries: []currentItemListEntry{entry}})
		}
		return nil
	})
	if err != nil {
		return nil, err
	}
	return batch, nil
}

func (s *Service) restoreRuntimeRaidEntryCostBatch(batch *runtimeRaidEntryCostBatch) error {
	if batch == nil || batch.restored {
		return nil
	}
	repositories, ok := s.repositoryGroup()
	if !ok || repositories.RaidEntryCost == nil || len(batch.before) != len(batch.characterIDs) {
		return dnfrepo.ErrRaidEntryCostTransactionUnavailable
	}
	ctx, cancel := context.WithTimeout(context.Background(), createWriteTimeout)
	defer cancel()
	err := repositories.RaidEntryCost.WithinRaidEntryCost(ctx, batch.characterIDs, func(inventories dnfrepo.InventoryRepository) error {
		for _, record := range batch.before {
			if err := inventories.Save(ctx, dnfrepo.CloneInventory(record)); err != nil {
				return err
			}
		}
		return nil
	})
	if err != nil {
		return err
	}
	batch.restored = true
	return nil
}

func (s *Service) sendRuntimeRaidEntryCostUpdates(updates []runtimeRaidEntryCostUpdate) (bool, error) {
	allDelivered := true
	for _, update := range updates {
		session, ok := s.onlineGameSession(update.CharacterID)
		if !ok || len(update.Entries) == 0 {
			allDelivered = false
			continue
		}
		body := buildCurrentItemUpdateBody(dnfrepo.MainInventoryListType, update.Entries)
		if err := s.sendGameUpperRawClass(session, uint16(dnfenum.CmdPacketWalkoutPartyMember), body, 0); err != nil {
			return false, err
		}
	}
	return allDelivered, nil
}

func (s *Service) flushRuntimeRaidEntryCostUpdates(raidKey uint32) error {
	s.raidMu.Lock()
	state := s.raids[raidKey]
	if state == nil || len(state.PendingCostSync) == 0 {
		s.raidMu.Unlock()
		return nil
	}
	attempt := state.StartAttempt
	updates := cloneRuntimeRaidEntryCostUpdates(state.PendingCostSync)
	s.raidMu.Unlock()

	allDelivered, err := s.sendRuntimeRaidEntryCostUpdates(updates)
	if err != nil || !allDelivered {
		return err
	}
	s.raidMu.Lock()
	defer s.raidMu.Unlock()
	state = s.raids[raidKey]
	if state != nil && state.StartAttempt == attempt {
		state.PendingCostSync = nil
	}
	return nil
}

func (s *Service) handleOnlineCreateRaid(session *gameSession, typ uint16, body []byte) error {
	req, err := raid.DecodeCreateRaidRequest(body)
	if err != nil {
		s.logGameEvent(session, "game-raid-create-body-invalid", "type", typ, "body_len", len(body), "error", err)
		s.logWarn("dnfbridge raid create rejected: invalid body",
			"conn_id", session.connID,
			"channel_id", session.channel.ID,
			"client_pid", sessionClientPID(session),
			"type", typ,
			"body_len", len(body),
			"error", err)
		return s.sendRaidCommandFailure(session, typ)
	}
	sourceID := selectedCharacterID(session)
	if sourceID == 0 {
		s.logWarn("dnfbridge raid create rejected: session has no selected character",
			"conn_id", session.connID,
			"channel_id", session.channel.ID,
			"client_pid", sessionClientPID(session),
			"account_id", s.accountIDForSession(session),
			"type", typ,
			"body_len", len(body))
		return s.sendRaidCommandFailure(session, typ)
	}
	// 普通（队伍）卢克以前在这里被拒，因为两阶段触发图当时还没有 owner。
	// 现在有了（raid_luke_phase_runtime.go），所以卢克的两种模式都跟安徒恩走同一条创建路径。
	// 那道拒绝连提示都是错的：失败码落到客户端 op664 的分支表就是 dstr 32059
	// 「抱歉， 不存在该玩家。」—— 也就是玩家给队伍起完名之后看到的弹窗。
	// （上游 bb52082「实现卢克团本两阶段触发图并放开普通攻坚队门禁」）
	ownerMode := runtimeRaidModeForSession(session)
	ownerConfig, configErr := s.runtimeRaidOwnerConfigForSession(session)
	// Refuse a single raid before allocating the object when the selected owner
	// has no native SingleMode. Fiend War now publishes one through its optional
	// fiendwar_single.etc overlay; an unmodified team-only PVF still keeps the
	// old fail-closed behaviour.
	if runtimeRaidSingleRequested(req.TailFlag) && (configErr != nil || !runtimeRaidOwnerSupportsSingleRaid(ownerConfig)) {
		s.logGameEvent(session, "game-raid-create-single-unsupported",
			"type", typ,
			"source_char_id", sourceID,
			"owner_mode", ownerMode,
			"route_or_raid_type", req.RouteOrRaidType,
			"tail_flag", req.TailFlag,
			"config_error", configErr,
			"reason", "owner_has_no_single_raid_mode")
		return s.sendRaidCommandFailure(session, typ)
	}
	if ownerMode == runtimeRaidOwnerLuke && (configErr != nil || ownerConfig.Raid == nil) {
		s.logGameEvent(session, "game-raid-create-pvf-rejected",
			"type", typ,
			"source_char_id", sourceID,
			"error", configErr)
		return s.sendRaidCommandFailure(session, typ)
	}
	state, created := s.createRuntimeRaid(session, sourceID, req)
	if !created {
		// A reconnecting client can submit CREATE_RAID before it has consumed the
		// post-select rejoin snapshot. Preserve the unfinished server-owned raid
		// (including its single/normal mode) and bind that object back to the EXE.
		// Returning the generic reason=3 here is incorrect: this client renders it
		// as "player does not exist", using the requested raid name as the title.
		if existing, ok := s.runtimeRaidForMember(sourceID); ok &&
			existing.LeaderID == sourceID &&
			(existing.RaidMode == 0 || existing.RaidMode == ownerMode) &&
			!existing.StartPending &&
			!existing.Started && !existing.EntryCostPaid {
			if existing.TailFlag == 1 {
				if err := s.clearSingleRaidOrdinaryPartyProjection(session, "unstarted_single_raid_create_recovery"); err != nil {
					return err
				}
			}
			if err := s.sendRuntimeRaidCreateBinding(session, typ, existing); err != nil {
				return err
			}
			s.logGameEvent(session, "game-raid-create-recovered-existing",
				"type", typ,
				"raid_key", existing.RaidKey,
				"source_char_id", sourceID,
				"existing_route_or_raid_type", existing.RouteOrRaidType,
				"requested_route_or_raid_type", req.RouteOrRaidType,
				"existing_tail_flag", existing.TailFlag,
				"requested_tail_flag", req.TailFlag)
			return nil
		}
		s.logWarn("dnfbridge raid create rejected: character already has an incompatible raid state",
			"conn_id", session.connID,
			"channel_id", session.channel.ID,
			"client_pid", sessionClientPID(session),
			"account_id", s.accountIDForSession(session),
			"type", typ,
			"source_char_id", sourceID,
			"body_len", len(body))
		return s.sendRaidCommandFailure(session, typ)
	}
	if ownerConfig.Raid != nil {
		state, created = s.attachRuntimeRaidOwnerConfig(state.RaidKey, ownerConfig)
		if !created {
			s.logGameEvent(session, "game-raid-create-owner-bind-rejected",
				"type", typ,
				"source_char_id", sourceID,
				"raid_key", state.RaidKey,
				"owner_mode", ownerConfig.Mode)
			return s.sendRaidCommandFailure(session, typ)
		}
	}
	if state.TailFlag == 1 {
		if err := s.clearSingleRaidOrdinaryPartyProjection(session, "single_raid_created"); err != nil {
			return err
		}
	}
	if err := s.sendRuntimeRaidCreateBinding(session, typ, state); err != nil {
		return err
	}
	s.logGameEvent(session, "game-raid-created",
		"type", typ,
		"raid_key", state.RaidKey,
		"leader_id", state.LeaderID,
		"member_count", len(state.Members),
		"route_or_raid_type", state.RouteOrRaidType,
		"tail_flag", state.TailFlag,
		"single_raid_type", state.SingleRaidType)
	s.logInfo("dnfbridge raid created",
		"conn_id", session.connID,
		"channel_id", session.channel.ID,
		"client_pid", sessionClientPID(session),
		"account_id", s.accountIDForSession(session),
		"type", typ,
		"raid_key", state.RaidKey,
		"leader_id", state.LeaderID,
		"member_count", len(state.Members),
		"route_or_raid_type", state.RouteOrRaidType,
		"tail_flag", state.TailFlag,
		"single_raid_type", state.SingleRaidType)
	return nil
}

func (s *Service) sendRuntimeRaidCreateBinding(session *gameSession, typ uint16, state runtimeRaidState) error {
	// Current EXE selects the raid key immediately while consuming the 664
	// success ACK. The complete mode=0 object must already exist by then.
	if err := s.broadcastRuntimeRaidCreate(state); err != nil {
		return err
	}
	if err := s.sendGameUpperRawClassCodec(session, typ, raid.BuildCreateRaidResultBody(state.RaidKey), dnfproto.DefaultChannelClassification, true); err != nil {
		return err
	}
	// The success ACK selects the created or restored raid. Send the canonical
	// mode=3 roster immediately afterwards so the ordinary raid member table
	// and party-allocation window materialize the leader row.
	return s.broadcastRuntimeRaidRefresh(state)
}

func (s *Service) broadcastRuntimeRaidCreate(state runtimeRaidState) error {
	body, err := raid.BuildRaidObjectMode0Body(runtimeRaidObjectMode0(state))
	if err != nil {
		return err
	}
	buffBody, err := s.runtimeRaidBuffStatusBody(state)
	if err != nil {
		return err
	}
	for _, member := range state.Members {
		session, ok := s.onlineGameSession(member.CharID)
		if !ok {
			continue
		}
		if err := s.sendGameUpperRawClassCodec(session, raid.RaidObjectMsgID, body, 0, true); err != nil {
			return err
		}
		if err := s.sendGameUpperRawClassCodec(session, raid.RaidBuffMsgID, buffBody, 0, true); err != nil {
			return err
		}
		// Do not synthesize an empty op634 snapshot here. Current EXE
		// sub_1D50990 replaces the complete native objective/monster-state
		// container with the received rows. A zero-row packet therefore erases
		// the locally materialized Anton graph; the next scene reports op695
		// {dungeon=0,count=0} and its actors retire without combat. Until a
		// dedicated raid service owns real rows, leaving this container under
		// the EXE is the only evidence-backed behavior.
	}
	return nil
}

func (s *Service) handleOnlineModifyRaidInfo(session *gameSession, typ uint16, body []byte) error {
	req, err := raid.DecodeModifyRaidInfoRequest(body)
	if err != nil {
		s.logGameEvent(session, "game-raid-modify-body-invalid", "type", typ, "body_len", len(body), "error", err)
		return s.sendRaidCommandFailure(session, typ)
	}
	sourceID := selectedCharacterID(session)
	state, ok := s.modifyRuntimeRaidInfo(sourceID, req)
	if !ok {
		return s.sendRaidCommandFailure(session, typ)
	}
	if err := s.sendGameUpperRawClassCodec(session, typ, []byte{1}, dnfproto.DefaultChannelClassification, true); err != nil {
		return err
	}
	return s.broadcastRuntimeRaidRefresh(state)
}

func (s *Service) handleOnlineLeaveRaid(session *gameSession, typ uint16, body []byte) error {
	request, err := raid.DecodeLeaveRaidRequest(body)
	if err != nil {
		s.logGameEvent(session, "game-raid-leave-body-invalid", "type", typ, "body_len", len(body), "error", err)
		return s.sendRaidCommandFailure(session, typ)
	}
	sourceID := selectedCharacterID(session)
	// op665 carries a target key and the client uses the one opcode for two
	// gestures: leaving names the sender, and the leader kicking a member names
	// that member. Measured: the leader's own connection sent 665 with body u16=2
	// while member 2 was kicked, and the old code removed the sender, so the
	// leader kicked itself out and the raid passed to the member.
	//
	// A self-leave stays unambiguous under either reading of the field, because
	// the raid key's low u16 is always the creator's character id
	// (nextRuntimeRaidKeyLocked builds channel<<16|sourceID). A key that names a
	// CURRENT member of the raid can therefore only be a kick.
	//
	// ⚠ 真机 2026-10-01 20:50 补的一条：超时空名册只剩一个人之后，客户端按"退出攻坚队"
	// 发出来的 key 是 **3**（既不是自己 22、也不是攻坚队 key 的低 16 位）。以前这里
	// 只看"是队长且 key 不是我"，于是这一脚被当成踢人 → 3 不是成员 → 拒绝 → 玩家
	// 看到的就是"点了没反应"。
	//
	// 所以超时空多一条：key 认不出名册里的人时**不算踢人**，回落到"自己退出"。
	// 安东/卢克逐字不变 —— 那边"队长的 stale key 一律按踢人处理、认不出就拒"是
	// 既有测试钉住的行为（raid_kick_member_test.go:129），不动它。
	if targetID := request.RaidOrMemberKey; targetID != 0 && targetID != sourceID {
		if state, ok := s.runtimeRaidForMember(sourceID); ok &&
			state.TailFlag != 1 && state.LeaderID == sourceID {
			fiendWarUnknownKey := state.RaidMode == runtimeRaidOwnerFiendWar &&
				runtimeRaidMemberIndex(state, targetID) < 0
			if !fiendWarUnknownKey {
				return s.handleOnlineKickRaidMember(session, typ, targetID)
			}
		}
	}
	// 超时空的离队语义与另外两个 owner 都不同：一个人离开只是把从名册里摘掉、把
	// 场景送回公开城镇，其余人继续打；只有名册空了才解散。它也因此不能塞进下面那条
	// 以 TailFlag(单人) 为条件的队伍分支 —— TailFlag 对超时空是 0。
	if state, ok := s.runtimeRaidForMember(sourceID); ok && state.Started &&
		state.RaidMode == runtimeRaidOwnerFiendWar && isCurrentFiendWarRaidChannel(session) {
		return s.handleRuntimeFiendWarLeave(session, typ, request.RaidOrMemberKey, state)
	}
	if state, ok := s.runtimeRaidForMember(sourceID); ok && state.Started && state.TailFlag == 1 {
		// A started SINGLE raid has one member and an unfinished run, so leaving
		// it abandons the run instead of just dropping a roster row. A started
		// TEAM raid falls through to the ordinary leave below -- the 86JP
		// reference lets a member (or the leader) leave at any point, and refusing
		// it left the player stuck in a raid they could not create another one
		// beside.
		_, frozenMember := runtimeRaidFrozenMember(state, sourceID)
		if state.StartPending || state.LeaderID != sourceID || !frozenMember ||
			uint16(state.RaidKey) != request.RaidOrMemberKey {
			return s.sendRaidCommandFailure(session, typ)
		}
		abandoned, abandonErr := s.abandonRuntimeRaid(state.RaidKey, sourceID, s.gameplayNow())
		if abandonErr != nil {
			s.logGameEvent(session, "game-raid-active-single-leave-rejected",
				"type", typ,
				"char_id", sourceID,
				"request_key", request.RaidOrMemberKey,
				"raid_key", state.RaidKey,
				"error", abandonErr)
			return s.sendRaidCommandFailure(session, typ)
		}
		storeRuntimeRaidEndedProjection(session, abandoned.RaidKey)
		if err := s.sendRuntimeSingleRaidEnded(session, typ, "active_single_raid_leave"); err != nil {
			return err
		}
		s.refreshRuntimePublicRaidDirectories("active_single_raid_left")
		s.logGameEvent(session, "game-raid-active-single-left",
			"type", typ,
			"char_id", sourceID,
			"request_key", request.RaidOrMemberKey,
			"raid_key", abandoned.RaidKey,
			"raid_phase", abandoned.Lifecycle.Phase,
			"lifecycle_status", abandoned.Lifecycle.Status)
		return nil
	}
	// Capture the roster before the removal consumes it. A disband leaves no
	// surviving member list to address, and every character that was in the raid
	// still has to be told the object is gone.
	previousRaid, hadPreviousRaid := s.runtimeRaidForMember(sourceID)
	state, removed, disbanded := s.removeRuntimeRaidMember(sourceID)
	if !removed {
		if state.RaidKey == 0 {
			if runtimeRaidEndedProjectionMatches(session, request.RaidOrMemberKey) {
				// 超时空的收尾不是"单人已结束"，而是"离队的那次重试"：把本地面板
				// 基线清掉再回公开城镇。
				if isCurrentFiendWarRaidChannel(session) {
					return s.finishRuntimeFiendWarLeave(session, typ)
				}
				return s.sendRuntimeSingleRaidEnded(session, typ, "ended_single_raid_leave_retry")
			}
			if completed, ok := s.completedSingleRuntimeRaidForLeave(sourceID, request.RaidOrMemberKey); ok {
				storeRuntimeRaidEndedProjection(session, completed.RaidKey)
				if err := s.sendRuntimeSingleRaidEnded(session, typ, "completed_single_raid_leave"); err != nil {
					return err
				}
				s.logGameEvent(session, "game-raid-completed-single-leave-acknowledged",
					"type", typ,
					"char_id", sourceID,
					"request_key", request.RaidOrMemberKey,
					"raid_key", completed.RaidKey,
					"raid_phase", completed.Lifecycle.Phase,
					"reason", "completed_reward_record_matches_released_runtime_raid")
				return nil
			}
			// A finished TEAM raid is reached the same way: finalizeRuntimeRaid
			// deleted the live row (raid_lifecycle.go:2097) while the member's client
			// still held the raid object, so the request must be answered from the
			// committed completion record instead of the live roster.
			if finished, ok := s.completedTeamRuntimeRaidForLeave(sourceID); ok {
				return s.leaveFinishedTeamRuntimeRaid(session, typ, finished, "completed_team_raid_leave_retry")
			}
		}
		return s.sendRaidCommandFailure(session, typ)
	}
	// A player who leaves on their own was never forcibly removed, so the op665
	// flag stays clear even when the departure empties the raid and disbands it.
	// The flag is what makes the client print 46791 "已经强制退出攻坚队。", and passing
	// the disband result there made an ordinary leave show that message.
	if err := s.sendGameUpperRawClassCodec(session, typ, raid.BuildLeaveRaidResultBody(false), dnfproto.DefaultChannelClassification, true); err != nil {
		return err
	}
	// The leaver always drops the object it materialized. No roster push in this
	// service targets a character that has just left, so without an explicit
	// removal the window keeps listing the raid and the client stays stuck on a
	// raid it is no longer in. A borrowed browse object is dropped with it, or a
	// later browse would treat the object as still held and send a roster-only
	// refresh for one the client no longer has.
	removedRaidKey := previousRaid.RaidKey
	if removedRaidKey == 0 {
		removedRaidKey = state.RaidKey
	}
	if err := s.sendRuntimeRaidObjectRemoved(session, removedRaidKey); err != nil {
		return err
	}
	// Leaving the attack party is ALSO leaving the squad party it projected.
	//
	// Operator rule, 2026-09-29: "退出攻坚队也视作自动退队并清理组队状态". The
	// projection-only clear below was not enough: it rewrote this session's own party
	// cache, but the central RuntimePartyManager kept the leaver in the squad party,
	// so the survivors' clients kept drawing a member row whose peer no longer
	// existed -- and every one of them, the leaver included, sat on 连接中 forever
	// ("退队以后还显示连接中 其他还未退出攻坚队的小队员就完蛋了 一直显示连接中").
	//
	// leaveRuntimePartyOnAbandonedDungeonExit is the proven sequence for exactly this
	// transition: manager leave + the leaver's own removal frame + relay re-sync or
	// close + the survivor broadcast that drops the row. It is deliberately a no-op
	// for a one-member party, where leaving is indistinguishable from staying.
	if err := s.leaveRuntimePartyOnAbandonedDungeonExit(
		session,
		runtimePartyStateSnapshot(session),
		"raid_leave_treated_as_automatic_party_leave",
	); err != nil {
		return err
	}
	previousParty := runtimePartyStateSnapshot(session)
	storeRuntimePartyState(session, alignedcmd.PartyState{})
	if err := s.sendRuntimePartyMemberRemoved(session, uint16(previousParty.PartyID), alignedcmd.PartyState{}); err != nil {
		return err
	}
	if disbanded {
		// Nobody survives to receive a roster refresh, so the removal also has to
		// reach every character that was in the disbanded raid.
		if hadPreviousRaid {
			if err := s.broadcastRuntimeRaidObjectRemoved(previousRaid, sourceID); err != nil {
				return err
			}
		}
		s.refreshRuntimePublicRaidDirectories("raid_disbanded")
		s.logGameEvent(session, "game-raid-disbanded-on-leave",
			"type", typ,
			"raid_key", previousRaid.RaidKey,
			"char_id", sourceID,
			"was_leader", previousRaid.LeaderID == sourceID,
			"member_count", len(previousRaid.Members))
		// The raid row is gone; the cost summary's final window is published before
		// the caller returns, so a disbanded raid still leaves one line behind.
		s.publishRuntimeRaidCostSummary(previousRaid.RaidKey, "raid_disbanded_on_leave")
		return nil
	}
	if state.LeaderID != previousRaid.LeaderID {
		// The departing leader handed the raid to the next member in roster order;
		// the survivors learn about it from the roster refresh below, and this line
		// records who took over.
		s.logGameEvent(session, "game-raid-leave-leadership-transferred",
			"type", typ,
			"raid_key", state.RaidKey,
			"char_id", sourceID,
			"previous_leader_id", previousRaid.LeaderID,
			"new_leader_id", state.LeaderID,
			"member_count", len(state.Members))
	}
	// The survivors' squad parties have to lose the leaver and renumber: the
	// projection detaches the departed member from the party it created for their
	// squad and rebuilds what is left.
	if err := s.applyRuntimeRaidParties(state); err != nil {
		s.logGameEvent(session, "game-raid-squad-party-projection-failed",
			"type", typ,
			"raid_key", state.RaidKey,
			"stage", "leave_raid",
			"error", err)
	}
	if err := s.broadcastRuntimeRaidRefresh(state); err != nil {
		return err
	}
	return s.broadcastRuntimeRaidBuffRegistration(state, 0)
}

// handleOnlineKickRaidMember removes one member on the leader's behalf.
//
// Only the leader may name somebody else, and only a member that is still on the
// roster. The kicked character is released exactly like a leaver: it has to drop
// the raid object it materialized and its ordinary-party projection, or the raid
// window stays on screen listing a raid it is no longer part of. The leader keeps
// the raid and receives the op665 result for its own request.
func (s *Service) handleOnlineKickRaidMember(session *gameSession, typ uint16, targetID uint16) error {
	leaderID := selectedCharacterID(session)
	leaderRaid, inRaid := s.runtimeRaidForMember(leaderID)
	if !inRaid || targetID == 0 || targetID == leaderID {
		return s.sendRaidCommandFailure(session, typ)
	}
	if leaderRaid.LeaderID != leaderID {
		s.logGameEvent(session, "game-raid-kick-rejected",
			"type", typ,
			"raid_key", leaderRaid.RaidKey,
			"leader_id", leaderID,
			"target_id", targetID,
			"reason", "requester_is_not_the_leader")
		return s.sendRaidCommandFailure(session, typ)
	}
	if runtimeRaidMemberIndex(leaderRaid, targetID) < 0 {
		s.logGameEvent(session, "game-raid-kick-rejected",
			"type", typ,
			"raid_key", leaderRaid.RaidKey,
			"leader_id", leaderID,
			"target_id", targetID,
			"reason", "target_is_not_a_member")
		return s.sendRaidCommandFailure(session, typ)
	}
	targetSession, targetOnline := s.onlineGameSession(targetID)

	// The leader is still a member, so a kick can never disband the raid; the
	// returned flag is deliberately not used to signal anything to the leader.
	remaining, removed, _ := s.removeRuntimeRaidMember(targetID)
	if !removed {
		return s.sendRaidCommandFailure(session, typ)
	}
	// The op665 result is receiver-relative: sub_1D04A00 closes the receiver's raid
	// windows and tears down its raid projection whichever flag it carries. It is
	// therefore the "you are out" notification and belongs to the KICKED member, not
	// to the leader who asked. Sending it to the leader made the leader's own member
	// list disappear and its browse window fall back to 参加攻坚队.
	//
	// The non-zero flag additionally pops string 46791 "已经强制退出攻坚队。" and
	// runs the forced-leave cleanup, which is what distinguishes a kick from leaving.
	//
	// The leader learns about the removal from the roster refresh broadcast below.
	if targetOnline && targetSession != nil {
		if err := s.sendGameUpperRawClassCodec(targetSession, typ, raid.BuildForcedLeaveRaidResultBody(), dnfproto.DefaultChannelClassification, true); err != nil {
			return err
		}
		if err := s.sendRuntimeRaidObjectRemoved(targetSession, leaderRaid.RaidKey); err != nil {
			return err
		}
		previousParty := runtimePartyStateSnapshot(targetSession)
		storeRuntimePartyState(targetSession, alignedcmd.PartyState{})
		if err := s.sendRuntimePartyMemberRemoved(targetSession, uint16(previousParty.PartyID), alignedcmd.PartyState{}); err != nil {
			return err
		}
		clearRuntimeRaidRejoinProjection(targetSession)
	}
	if err := s.applyRuntimeRaidParties(remaining); err != nil {
		s.logGameEvent(session, "game-raid-squad-party-projection-failed",
			"type", typ,
			"raid_key", leaderRaid.RaidKey,
			"stage", "kick_raid_member",
			"error", err)
	}
	if err := s.broadcastRuntimeRaidRefresh(remaining); err != nil {
		return err
	}
	if err := s.broadcastRuntimeRaidBuffRegistration(remaining, 0); err != nil {
		return err
	}
	s.logGameEvent(session, "game-raid-member-kicked",
		"type", typ,
		"raid_key", leaderRaid.RaidKey,
		"leader_id", leaderID,
		"target_id", targetID,
		"target_online", targetOnline,
		"member_count", len(remaining.Members))
	return nil
}

func (s *Service) sendRuntimeSingleRaidEnded(session *gameSession, typ uint16, source string) error {
	if err := s.sendGameUpperRawClassCodec(session, typ, raid.BuildLeaveRaidResultBody(true), dnfproto.DefaultChannelClassification, true); err != nil {
		return err
	}
	clearRuntimeRaidRejoinProjection(session)
	return s.clearSingleRaidOrdinaryPartyProjection(session, source)
}

// publishExpiredRuntimeRaid is the terminal notification for a phase that ran
// out of time. expireRuntimeRaidPhase has already removed the authoritative
// raid, so every frozen member must be released here: the attack party is gone
// server-side and a client that is never told keeps its raid window, its rejoin
// projection, and its ordinary-party projection forever.
//
// Attack parties are notified exactly like single raids. Both modes end on the
// same native op665 disband result, and a per-member socket failure must not
// stop the remaining members from being released - with a full twenty-member
// roster the old early return left most of the raid stranded.
// publishFailedRuntimeRaid announces the attack-window failure, in 86JP's order
// (RaidHandler.Runtime.cs RunAttackTimeoutAsync:646-711): the closed gate table,
// the phase-one failure symbol, a zeroed remaining time, the state-4 object, the
// per-member fail result, and finally the "you may leave the dungeon" op31 to every
// member still standing inside an Anton gate.
//
// This is not the release: the raid stays in the raid table for ten seconds so a
// client that reconnects can still read the result, and removeRuntimeRaidFailedPhase
// releases it afterwards.
func (s *Service) publishFailedRuntimeRaid(state runtimeRaidState) error {
	if state.RaidKey == 0 || len(state.FrozenMembers) == 0 ||
		state.Lifecycle.Status != runtimeRaidLifecycleFailed {
		return errRuntimeRaidLifecycleState
	}
	phase := state.Lifecycle.Phase
	wirePhase, err := runtimeRaidSettlementWirePhase(phase)
	if err != nil {
		return err
	}
	_, dungeonBody, dungeonRows, err := runtimeRaidPhaseSnapshotBodies(state)
	if err != nil {
		return err
	}
	// The rows are already closed by expireRuntimeRaidPhase (state 2 everywhere);
	// the full table travels once here so every client redraws the selector from the
	// same closed state, and the local table is recorded so a later snapshot only
	// sends what moves. The trailer is recorded with the rows because this table is
	// the one that carries it: dungeonBody is the same runtimeRaidPhaseSnapshotBodies
	// projection.
	s.recordRuntimeRaidDungeonStateRows(state.RaidKey, phase, dungeonRows, runtimeRaidInfectionMapIDForWire(state))
	notifications := []runtimeRaidWireNotification{
		{msgID: raid.RaidDungeonStateMsgID, body: dungeonBody},
		{msgID: raid.RaidRemainTimeMsgID, body: raid.BuildRaidRemainTimeBody(raid.RaidRemainTime{TimerType: 0, Seconds: 0})},
		{
			msgID: raid.RaidStateMsgID,
			body: raid.BuildRaidStateBody(raid.RaidState{
				State:    runtimeRaidFailedState,
				Argument: runtimeRaidFailedStateArgument,
			}),
		},
	}
	if phase == runtimeRaidDungeonPhaseOne {
		body, err := raid.BuildRaidSymbolBody([]raid.RaidSymbolRow{
			{Key: antonSymbolPhaseOneFail, Value: 1},
		})
		if err != nil {
			return err
		}
		notifications = append(notifications, runtimeRaidWireNotification{
			msgID: raid.RaidSymbolMsgID,
			body:  body,
		})
	}
	// 失败结算也带本阶段死亡次数，同样**所有团本都填**（服主 2026-09-29 拍板）：
	// 这个 u16 与阵营无关，依据见 raid_phase_death_count.go。
	failureDeaths := s.runtimeRaidPhaseDeathCount(state.RaidKey, state.Lifecycle.Phase)
	resultBody, err := raid.BuildRaidSettlementResultBody(raid.RaidSettlementResult{
		ResultType:   runtimeRaidFailResultType,
		PhaseIndex:   wirePhase,
		ClearSeconds: runtimeRaidDurationSeconds(runtimeRaidPhaseElapsed(state, s.gameplayNow())),
		DeathCount:   failureDeaths,
		// 86JP ships Score 0 and the trailing flag 1 with a failed result
		// (RaidPacketBuilder.BuildRaidResult(1u, phaseIndex, seconds, deaths, 0u, 1)).
		Score: 0,
		Flags: 1,
	})
	if err != nil {
		return err
	}
	if err := s.broadcastRuntimeRaidWireSequence(state, notifications...); err != nil {
		return err
	}
	source := "raid_phase_attack_timeout"
	event := "game-raid-phase-timeout-published"
	if state.TailFlag == 1 {
		source = "single_raid_phase_attack_timeout"
		event = "game-raid-single-phase-timeout-published"
	}
	var firstErr error
	for _, member := range state.FrozenMembers {
		session, online := s.onlineGameSession(member.CharID)
		if !online {
			continue
		}
		if err := s.sendGameUpperRawClassCodec(session, raid.RaidResultMsgID, resultBody, 0, true); err != nil {
			if firstErr == nil {
				firstErr = err
			}
			s.logGameEvent(session, "game-raid-phase-timeout-result-failed",
				"char_id", member.CharID, "raid_key", state.RaidKey, "raid_phase", phase, "error", err)
			continue
		}
		s.logGameEvent(session, event,
			"char_id", member.CharID,
			"raid_key", state.RaidKey,
			"raid_phase", phase,
			"lifecycle_revision", state.Lifecycle.Revision,
			"result_type", runtimeRaidFailResultType,
			"source", source)
	}
	s.publishRuntimeRaidFailedGateReturn(state)
	return firstErr
}

// publishRuntimeRaidFailedGateReturn tells every member still inside one of the
// failed phase's gates that it may leave it.
//
// It is 86JP's EnablePhaseOneDungeonReturnAsync (RaidHandler.Rewards.cs:1993-2013):
// class-0 op31 with the one-byte enable-clear body, sent only to members whose
// current run is an Anton dungeon. The current client registers exactly that pair
// as its settlement entry (see currentDungeonSettlementEntryMsgID), so without it a
// member that ignores the result screen has no way out of a dungeon the raid no
// longer owns.
func (s *Service) publishRuntimeRaidFailedGateReturn(state runtimeRaidState) {
	for _, member := range state.FrozenMembers {
		session, online := s.onlineGameSession(member.CharID)
		if !online || session == nil {
			continue
		}
		session.dungeon.mu.Lock()
		runtime := session.dungeon.runtime
		inside := runtime != nil && runtime.Session != nil &&
			dungeonRuntimeOwnsCharacter(runtime, member.CharID) &&
			isAntonRaidEntryDungeonID(runtime.Dungeon.ID)
		session.dungeon.mu.Unlock()
		if !inside {
			continue
		}
		if err := s.sendGameUpperRawClass(session, currentDungeonSettlementEntryMsgID, []byte{0}, 0); err != nil {
			s.logGameEvent(session, "game-raid-phase-timeout-gate-return-failed",
				"char_id", member.CharID,
				"raid_key", state.RaidKey,
				"dungeon_id", runtime.Dungeon.ID,
				"error", err)
			continue
		}
		s.logGameEvent(session, "game-raid-phase-timeout-gate-return-enabled",
			"char_id", member.CharID,
			"raid_key", state.RaidKey,
			"raid_phase", state.Lifecycle.Phase,
			"dungeon_id", runtime.Dungeon.ID,
			"msg_id", currentDungeonSettlementEntryMsgID)
	}
}

// publishExpiredRuntimeRaid announces the release of a raid that is already gone
// from the raid table: it is the ten-second-later half of the timeout
// (removeRuntimeRaidFailedPhase) and the only packet a member still holding the
// terminal object needs.
func (s *Service) publishExpiredRuntimeRaid(state runtimeRaidState) error {
	if state.RaidKey == 0 || len(state.FrozenMembers) == 0 ||
		(state.Lifecycle.Status != runtimeRaidLifecycleFailed &&
			state.Lifecycle.Status != runtimeRaidLifecycleAbandoned) {
		return errRuntimeRaidLifecycleState
	}
	source := "raid_phase_expired"
	event := "game-raid-phase-expired-published"
	if state.TailFlag == 1 {
		source = "single_raid_phase_expired"
		event = "game-raid-single-phase-expired-published"
	}
	var firstErr error
	for _, member := range state.FrozenMembers {
		session, online := s.onlineGameSession(member.CharID)
		if !online {
			continue
		}
		storeRuntimeRaidEndedProjection(session, state.RaidKey)
		if err := s.sendRuntimeSingleRaidEnded(session, uint16(dnfenum.CmdPacketLeaveRaid), source); err != nil {
			if firstErr == nil {
				firstErr = err
			}
			s.logGameEvent(session, "game-raid-phase-expired-publish-failed",
				"char_id", member.CharID,
				"raid_key", state.RaidKey,
				"raid_phase", state.Lifecycle.Phase,
				"error", err)
			continue
		}
		s.logGameEvent(session, event,
			"char_id", member.CharID,
			"raid_key", state.RaidKey,
			"raid_phase", state.Lifecycle.Phase,
			"lifecycle_revision", state.Lifecycle.Revision)
	}
	s.refreshRuntimePublicRaidDirectories(source)
	return firstErr
}

// completedSingleRuntimeRaidForLeave is the post-settlement ownership proof
// for op665. Final reward commit deliberately removes the live Raid object, so
// a later client leave request must be checked against the committed completion
// record instead of a member index that no longer exists.
func (s *Service) completedSingleRuntimeRaidForLeave(characterID uint16, requestKey uint16) (runtimeRaidState, bool) {
	if s == nil || characterID == 0 || requestKey == 0 {
		return runtimeRaidState{}, false
	}
	s.raidRewardCommitMu.Lock()
	defer s.raidRewardCommitMu.Unlock()
	var matched runtimeRaidState
	found := false
	for _, completion := range s.raidRewardCompleted {
		state := completion.Raid
		if state.TailFlag != 1 || state.RaidKey == 0 || uint16(state.RaidKey) != requestKey ||
			state.Lifecycle.Status != runtimeRaidLifecycleFinished {
			continue
		}
		if _, member := runtimeRaidFrozenMember(state, characterID); !member {
			continue
		}
		if found && matched.RaidKey != state.RaidKey {
			return runtimeRaidState{}, false
		}
		matched = cloneRuntimeRaidState(state)
		found = true
	}
	return matched, found
}

// completedTeamRuntimeRaidForLeave is the team-raid half of the same proof.
//
// A team raid that finished phase two is released by finalizeRuntimeRaid
// (raid_lifecycle.go:2080-2099): the live raid row is deleted while every member's
// client still holds the raid object. The operator's rule (2026-09-20) is that
// clicking 退出攻坚队 there simply leaves the attack party -- "通关后点退出 就是直接
// 退出攻坚队了 不是什么已结束和静默消失" -- but before this the request matched no
// branch at all and was answered with the silent failure body {0,3}:
//
//	11:30:55 RECV type=665 body 0d 00 → SEND msg_id=665 hex …150000000003
//	11:31:00 / 11:31:02 / 11:31:52 / 11:32:17 / 11:35:11 / 11:35:13 / 11:47:03 / 11:47:06 …
//
// The request key is deliberately NOT used here: a self-leave carries the SENDER's
// own character id (measured above: char 13 sends 0d 00, char 10 sends 0a 00), while
// a team raid key's low u16 is the CREATOR's character id, so the key can only ever
// match for the creator. Membership in the finished raid's frozen roster is the proof.
func (s *Service) completedTeamRuntimeRaidForLeave(characterID uint16) (runtimeRaidState, bool) {
	if s == nil || characterID == 0 {
		return runtimeRaidState{}, false
	}
	s.raidRewardCommitMu.Lock()
	defer s.raidRewardCommitMu.Unlock()
	var matched runtimeRaidState
	found := false
	for _, completion := range s.raidRewardCompleted {
		state := completion.Raid
		if state.TailFlag == 1 || state.RaidKey == 0 ||
			state.Lifecycle.Status != runtimeRaidLifecycleFinished {
			continue
		}
		if _, member := runtimeRaidFrozenMember(state, characterID); !member {
			continue
		}
		if found && matched.RaidKey != state.RaidKey {
			return runtimeRaidState{}, false
		}
		matched = cloneRuntimeRaidState(state)
		found = true
	}
	return matched, found
}

// leaveFinishedTeamRuntimeRaid answers a leave request for a team raid the server has
// already released. It is the ordinary leave sequence of handleOnlineLeaveRaid
// (success ACK → drop the leaver's raid object → drop the party the raid projected),
// with no roster refresh: there is no live raid table left to refresh.
func (s *Service) leaveFinishedTeamRuntimeRaid(
	session *gameSession,
	typ uint16,
	finished runtimeRaidState,
	source string,
) error {
	if session == nil || finished.RaidKey == 0 {
		return nil
	}
	if err := s.sendGameUpperRawClassCodec(
		session,
		typ,
		raid.BuildLeaveRaidResultBody(false),
		dnfproto.DefaultChannelClassification,
		true,
	); err != nil {
		return err
	}
	storeRuntimeRaidEndedProjection(session, finished.RaidKey)
	// A stale rejoin projection would let a later op668 answer a rejoin window the
	// player no longer has; the single-raid end path drops it for the same reason
	// (sendRuntimeSingleRaidEnded).
	clearRuntimeRaidRejoinProjection(session)
	if err := s.sendRuntimeRaidObjectRemoved(session, finished.RaidKey); err != nil {
		return err
	}
	previousParty := runtimePartyStateSnapshot(session)
	storeRuntimePartyState(session, alignedcmd.PartyState{})
	if err := s.sendRuntimePartyMemberRemoved(session, uint16(previousParty.PartyID), alignedcmd.PartyState{}); err != nil {
		return err
	}
	s.logGameEvent(session, "game-raid-completed-team-leave-acknowledged",
		"type", typ,
		"char_id", session.selectedCharacterID,
		"raid_key", finished.RaidKey,
		"raid_phase", finished.Lifecycle.Phase,
		"member_count", len(finished.FrozenMembers),
		"party_id", previousParty.PartyID,
		"source", source,
		"reason", "finished_team_raid_released_the_live_row_but_the_client_still_held_its_object")
	s.markFinishedRuntimeRaidLeaver(finished, session.selectedCharacterID)
	return nil
}

// markFinishedRuntimeRaidLeaver records one departure from a raid the server already
// released and pushes the reduced roster to everyone still listed.
//
// Operator's report, 2026-09-20: "攻坚结束后 这个成员列表不实时刷新 1队的两个人已经退出
// 攻坚队了 还在攻坚队内的人看成员列表 显示这俩人还在攻坚队". Live raid 1507329:
//
//	13:21:29 game-raid-completed-team-leave-acknowledged char_id=1  member_count=4
//	13:21:36 game-raid-completed-team-leave-acknowledged char_id=21 member_count=4
//
// and no roster packet followed either time, because finalizeRuntimeRaid had already
// deleted the live row (raid_lifecycle.go:2097). The frozen completion record is the only
// roster left, so the reduced list is built from it minus everyone who has left so far.
func (s *Service) markFinishedRuntimeRaidLeaver(finished runtimeRaidState, leaverID uint16) {
	if s == nil || finished.RaidKey == 0 || leaverID == 0 || len(finished.FrozenMembers) == 0 {
		return
	}
	s.finishedRaidLeaverMu.Lock()
	if s.finishedRaidLeavers == nil {
		s.finishedRaidLeavers = make(map[uint32]map[uint16]bool)
	}
	gone := s.finishedRaidLeavers[finished.RaidKey]
	if gone == nil {
		gone = make(map[uint16]bool, len(finished.FrozenMembers))
		s.finishedRaidLeavers[finished.RaidKey] = gone
	}
	gone[leaverID] = true
	remaining := make([]runtimeRaidMemberState, 0, len(finished.FrozenMembers))
	for _, member := range finished.FrozenMembers {
		if !gone[member.CharID] {
			remaining = append(remaining, member)
		}
	}
	if len(remaining) == 0 {
		delete(s.finishedRaidLeavers, finished.RaidKey)
	}
	s.finishedRaidLeaverMu.Unlock()
	if len(remaining) == 0 {
		// The last member left: nobody is left to draw a roster for.
		return
	}
	state := cloneRuntimeRaidState(finished)
	state.Members = remaining
	state.FrozenMembers = append([]runtimeRaidMemberState(nil), remaining...)
	if state.LeaderID == leaverID {
		state.LeaderID = remaining[0].CharID
	}
	if err := s.broadcastRuntimeRaidRefresh(state); err != nil {
		s.logPacketEvent("game-raid-finished-roster-refresh-failed",
			"raid_key", finished.RaidKey,
			"leaver_char_id", leaverID,
			"error", err)
		return
	}
	s.logPacketEvent("game-raid-finished-roster-refreshed",
		"raid_key", finished.RaidKey,
		"leaver_char_id", leaverID,
		"remaining_members", len(remaining),
		"reason", "a_member_left_a_released_raid_and_the_list_must_drop_him")
}

func (s *Service) handleOnlineStartRaid(session *gameSession, typ uint16, body []byte) error {
	if err := raid.DecodeStartRaidRequest(body); err != nil {
		s.logGameEvent(session, "game-raid-start-body-invalid", "type", typ, "body_len", len(body), "error", err)
		return s.sendRaidCommandFailure(session, typ)
	}
	sourceID := selectedCharacterID(session)
	ownerConfig, configErr := s.runtimeRaidOwnerConfigForSession(session)
	config := ownerConfig.Raid
	// Record the complete local state before any validation mutates a start
	// reservation.  This distinguishes a missing C2S 666 from a legitimate
	// single/team roster rejection during client testing.
	if current, ok := s.runtimeRaidForMember(sourceID); ok {
		minimumMembers, maximumMembers := runtimeRaidStartRosterBounds(current, config)
		entryItemID, entryCount := runtimeRaidEntryCostForState(current)
		s.logGameEvent(session, "game-raid-start-received",
			"type", typ,
			"body_len", len(body),
			"source_char_id", sourceID,
			"raid_key", current.RaidKey,
			"route_or_raid_type", current.RouteOrRaidType,
			"tail_flag", current.TailFlag,
			"single_raid_type", current.SingleRaidType,
			"member_count", len(current.Members),
			"minimum_members", minimumMembers,
			"maximum_members", maximumMembers,
			"entry_item_id", entryItemID,
			"entry_count", entryCount)
	} else {
		s.logGameEvent(session, "game-raid-start-received-without-state",
			"type", typ, "body_len", len(body), "source_char_id", sourceID)
	}
	if configErr != nil {
		s.logGameEvent(session, "game-raid-start-pvf-rejected", "type", typ, "source_char_id", sourceID, "error", configErr)
		return s.sendRaidCommandFailure(session, typ)
	}
	// 创建侧那道「卢克只允许单人」的子句跟着一起撤了：卢克两种模式现在都有自己的阶段图。
	// （上游 bb52082）
	if current, ok := s.runtimeRaidForMember(sourceID); !ok || (current.RaidMode != 0 && current.RaidMode != ownerConfig.Mode) {
		s.logGameEvent(session, "game-raid-start-owner-rejected",
			"type", typ,
			"source_char_id", sourceID,
			"channel_raid_mode", ownerConfig.Mode,
			"state_found", ok,
			"state_raid_mode", current.RaidMode,
			"tail_flag", current.TailFlag)
		return s.sendRaidCommandFailure(session, typ)
	}
	// Second gate for the same defect the create path now refuses: a Fiend War
	// raid object that still carries TailFlag=1 (an older save, or one created
	// before the create-side refusal landed) must not be allowed to reserve,
	// debit and then fail to commit. Refuse it here, while the raid is untouched.
	if existing, ok := s.runtimeRaidForMember(sourceID); ok &&
		!runtimeRaidOwnerSupportsSingleRaid(ownerConfig) &&
		runtimeRaidSingleRequested(existing.TailFlag) {
		s.logGameEvent(session, "game-raid-start-single-unsupported",
			"type", typ,
			"source_char_id", sourceID,
			"raid_key", existing.RaidKey,
			"owner_mode", existing.RaidMode,
			"tail_flag", existing.TailFlag,
			"started", existing.Started,
			"reason", "owner_has_no_single_raid_mode")
		return s.sendRaidCommandFailure(session, typ)
	}
	reservation, err := s.reserveRuntimeRaidStart(sourceID)
	if err != nil {
		s.logGameEvent(session, "game-raid-start-rejected",
			"type", typ,
			"source_char_id", sourceID,
			"error", err)
		if sendErr := s.sendRaidCommandFailure(session, typ); sendErr != nil {
			return sendErr
		}
		if reservation.State.RaidKey != 0 {
			return s.sendRuntimeRaidEntryCostSnapshot(session, reservation.State)
		}
		return nil
	}
	state := reservation.State
	// C2S 666 with an empty body is also the client's NEXT-PHASE start request:
	// 86JP RaidHandler.HandleStartRaid dispatches an empty START_RAID whose raid
	// sits in state 5 (the between-phase standby UI) to
	// HandleStartNextRaidPhaseAsync -> TryPrepareNextPhase(leader) -> start phase
	// two, and its break timer only auto-starts the phase when nobody asks first.
	// Treating it as an idempotent re-start of phase one left the raid parked in
	// standby: live report 2026-09-16 20:30Z, the leader pressed 开始 three times
	// and every answer was a standby-timer replay with lifecycle_status=5.
	if reservation.AlreadyStarted && len(body) == 0 &&
		state.Lifecycle.Status == runtimeRaidLifecycleStandby &&
		state.Lifecycle.Phase == runtimeRaidDungeonPhaseOne {
		if state.LeaderID != sourceID {
			s.logGameEvent(session, "game-raid-next-phase-rejected",
				"type", typ,
				"raid_key", state.RaidKey,
				"source_char_id", sourceID,
				"leader_id", state.LeaderID,
				"reason", "only_the_raid_leader_can_start_the_next_phase")
			return s.sendRaidCommandFailure(session, typ)
		}
		// The next-phase start activates a phase on the same clients as the
		// first one, so it carries the same waiting-area gate: a member pulled
		// into the activation burst from outside the waiting area is the storm in
		// raid_start_waiting_area.go, regardless of which phase is starting.
		if spec, specOK := runtimeRaidTownSpecForOwnerMode(ownerConfig.Mode); specOK &&
			isCurrentImplementedRaidChannel(session) {
			if outside := s.runtimeRaidMembersOutsideWaitingArea(state, spec); len(outside) != 0 {
				return s.rejectRuntimeRaidStartOutsideWaitingArea(
					session, typ, reservation, state, spec, outside, "next_phase_start")
			}
		}
		activated, activateErr := s.activateRuntimeRaidPhase(
			state.RaidKey,
			runtimeRaidDungeonPhaseTwo,
			time.Now().UTC(),
			true,
		)
		if activateErr != nil {
			s.logGameEvent(session, "game-raid-next-phase-rejected",
				"type", typ,
				"raid_key", state.RaidKey,
				"source_char_id", sourceID,
				"lifecycle_status", byte(state.Lifecycle.Status),
				"lifecycle_phase", state.Lifecycle.Phase,
				"error", activateErr)
			return s.sendRaidCommandFailure(session, typ)
		}
		if err := s.sendGameUpperRawClassCodec(session, typ, []byte{1}, dnfproto.DefaultChannelClassification, true); err != nil {
			return err
		}
		s.logGameEvent(session, "game-raid-next-phase-started",
			"type", typ,
			"raid_key", activated.RaidKey,
			"raid_phase", activated.Lifecycle.Phase,
			"source_char_id", sourceID,
			"reason", "leader_start_command_during_between_phase_standby")
		return nil
	}
	if !reservation.AlreadyStarted {
		minimumMembers, maximumMembers := runtimeRaidStartRosterBounds(state, config)
		if len(state.FrozenMembers) < minimumMembers || len(state.FrozenMembers) > maximumMembers {
			// This rejection used to be silent, which made a two-member roster look
			// like the client had ignored the click.
			s.logGameEvent(session, "game-raid-start-roster-rejected",
				"type", typ,
				"raid_key", state.RaidKey,
				"member_count", len(state.FrozenMembers),
				"minimum_members", minimumMembers,
				"maximum_members", maximumMembers,
				"reason", "roster_out_of_bounds")
			s.cancelRuntimeRaidStart(reservation)
			return s.sendRaidStartFailure(session, typ, runtimeRaidStartFailureMinimumRoster, nil)
		}
		if err := s.validateRuntimeRaidRosterReady(session, state.FrozenMembers); err != nil {
			s.logGameEvent(session, "game-raid-start-roster-rejected",
				"type", typ,
				"raid_key", state.RaidKey,
				"member_count", len(state.FrozenMembers),
				"reason", "roster_not_standing_by",
				"error", err)
			s.cancelRuntimeRaidStart(reservation)
			if sendErr := s.sendRaidCommandFailure(session, typ); sendErr != nil {
				return sendErr
			}
			return nil
		}
		// Every member has to be standing in the raid's waiting area before the
		// phase is activated. This is a deliberate extra gate with no 86JP
		// counterpart; raid_start_waiting_area.go records the measured client
		// storm that it exists to prevent and why the channel alone is not the
		// test. It runs before the entry ticket is consumed, so a refused start
		// leaves the raid exactly as it was.
		if spec, specOK := runtimeRaidTownSpecForOwnerMode(ownerConfig.Mode); specOK &&
			isCurrentImplementedRaidChannel(session) {
			if outside := s.runtimeRaidMembersOutsideWaitingArea(state, spec); len(outside) != 0 {
				return s.rejectRuntimeRaidStartOutsideWaitingArea(
					session, typ, reservation, state, spec, outside, "initial_start")
			}
		}
		state.AntonConfig = nil
		state.LukeConfig = nil
		state.FiendWarConfig = nil
		if ownerConfig.Mode == runtimeRaidOwnerLuke {
			state.LukeConfig = ownerConfig.Luke
		} else {
			state.AntonConfig = config
		}
		// 超时空额外的 owner 配置。安徒恩/卢克走到这里 FiendWarConfig 仍是 nil，
		// 与改动前逐字节一致；它们的分支一个都没有变。
		if ownerConfig.Mode == runtimeRaidOwnerFiendWar {
			state.FiendWarConfig = ownerConfig.FiendWar
		}
		startTime := time.Now().UTC()
		counterCtx, counterCancel := context.WithTimeout(context.Background(), createWriteTimeout)
		counterBatch, err := s.reserveRuntimeRaidAdmissionCounters(
			counterCtx, state, ownerConfig.Mode, startTime)
		counterCancel()
		if err != nil {
			s.logGameEvent(session, "game-raid-start-admission-counter-rejected",
				"type", typ,
				"raid_key", state.RaidKey,
				"raid_mode", ownerConfig.Mode,
				"error", err)
			s.cancelRuntimeRaidStart(reservation)
			if sendErr := s.sendRaidStartFailure(session, typ, runtimeRaidStartFailureDailyLimit, nil); sendErr != nil {
				return sendErr
			}
			return s.sendRuntimeRaidEntryCostSnapshot(session, state)
		}
		entryCostBatch, err := s.consumeRuntimeRaidEntryCostBatch(state)
		if err != nil {
			if restoreErr := s.restoreCurrentDungeonAdmissionCounterBatch(counterBatch); restoreErr != nil {
				err = errors.Join(err, restoreErr)
			}
			missing := s.runtimeRaidMembersMissingEntryCost(state)
			s.logGameEvent(session, "game-raid-start-entry-cost-rejected",
				"type", typ,
				"raid_key", state.RaidKey,
				"item_id", config.EntryCost.ItemID,
				"count", config.EntryCost.Count,
				"missing_members", formatRuntimeRaidMemberIDs(missing),
				"error", err)
			s.cancelRuntimeRaidStart(reservation)
			if sendErr := s.sendRaidStartFailure(session, typ, runtimeRaidStartFailureEntryCost, missing); sendErr != nil {
				return sendErr
			}
			return s.sendRuntimeRaidEntryCostSnapshot(session, state)
		}
		var committed bool
		state, committed = s.commitRuntimeRaidStartWithOwner(reservation, entryCostBatch.updates, ownerConfig, startTime)
		if !committed {
			s.cancelRuntimeRaidStart(reservation)
			commitErr := fmt.Errorf("commit runtime raid start after durable entry debit: raid=%d attempt=%d", reservation.State.RaidKey, reservation.Attempt)
			if restoreErr := s.restoreRuntimeRaidEntryCostBatch(entryCostBatch); restoreErr != nil {
				commitErr = errors.Join(commitErr, restoreErr)
			}
			if restoreErr := s.restoreCurrentDungeonAdmissionCounterBatch(counterBatch); restoreErr != nil {
				commitErr = errors.Join(commitErr, restoreErr)
			}
			return commitErr
		}
	}
	// The repository transaction above is the durable boundary. The current
	// client must observe the successful command ACK before any op14 inventory
	// projection of that committed debit.
	if err := s.sendGameUpperRawClassCodec(session, typ, []byte{1}, dnfproto.DefaultChannelClassification, true); err != nil {
		return err
	}
	if err := s.flushRuntimeRaidEntryCostUpdates(state.RaidKey); err != nil {
		s.logGameEvent(session, "game-raid-start-post-ack-failed", "stage", "entry_cost_flush", "raid_key", state.RaidKey, "error", err)
		return err
	}
	if err := s.broadcastRuntimeRaidRefresh(state); err != nil {
		s.logGameEvent(session, "game-raid-start-post-ack-failed", "stage", "member_refresh", "raid_key", state.RaidKey, "error", err)
		return err
	}
	if !reservation.AlreadyStarted {
		// The current EXE materializes the native 3,2,1 preparation stage only
		// after its Raid Buff registration model exists. Publish op633 before
		// state=1/op596/op597 rather than waiting for the later client query.
		if err := s.broadcastRuntimeRaidBuffRegistration(state, 0); err != nil {
			s.logGameEvent(session, "game-raid-start-post-ack-failed", "stage", "buff_registration", "raid_key", state.RaidKey, "error", err)
			return err
		}
		if err := s.applyRuntimeRaidParties(state); err != nil {
			s.logGameEvent(session, "game-raid-start-post-ack-failed", "stage", "party_apply", "raid_key", state.RaidKey, "error", err)
			return err
		}
	}
	configSingleMode := false
	configSinglePhaseDungeons := 0
	configSingleTimeLimit := 0
	configStartDelay := 0
	if stateConfig := runtimeRaidBaseConfig(state); stateConfig != nil {
		configStartDelay = stateConfig.StartDelaySeconds
		if stateConfig.SingleMode != nil {
			configSingleMode = true
			configSinglePhaseDungeons = len(stateConfig.SingleMode.Phase.Dungeons)
			configSingleTimeLimit = stateConfig.SingleMode.TimeLimitSeconds
		}
	}
	s.logGameEvent(session, "game-raid-start-lifecycle",
		"raid_key", state.RaidKey,
		"raid_mode", state.RaidMode,
		"tail_flag", state.TailFlag,
		"single_raid_type", state.SingleRaidType,
		"lifecycle_status", byte(state.Lifecycle.Status),
		"lifecycle_phase", state.Lifecycle.Phase,
		"started_at", state.Lifecycle.StartedAt,
		"ready_deadline", state.Lifecycle.ReadyDeadline,
		"phase_deadline", state.Lifecycle.PhaseDeadline,
		"config_start_delay_seconds", configStartDelay,
		"config_single_mode", configSingleMode,
		"config_single_phase_dungeons", configSinglePhaseDungeons,
		"config_single_time_limit_seconds", configSingleTimeLimit)
	switch state.Lifecycle.Status {
	case runtimeRaidLifecyclePreparing:
		if err := s.publishRuntimeRaidReady(state); err != nil {
			s.logGameEvent(session, "game-raid-start-post-ack-failed", "stage", "publish_ready", "raid_key", state.RaidKey, "error", err)
			return err
		}
	case runtimeRaidLifecycleStandby:
		// The break countdown is read from 0x0255 timerType 1 while the raid object
		// state is 5, so a standby replay has to carry both packets (see
		// publishRuntimeRaidStandbyTimerLocked).
		standbyTimer := runtimeRaidStandbyTimer(state)
		if err := s.broadcastRuntimeRaidWireSequence(state,
			runtimeRaidWireNotification{
				msgID: raid.RaidTimerMsgID,
				body:  raid.BuildRaidTimerBody(standbyTimer),
			},
			runtimeRaidWireNotification{
				msgID: raid.RaidRemainTimeMsgID,
				body: raid.BuildRaidRemainTimeBody(raid.RaidRemainTime{
					TimerType: 1,
					Seconds:   runtimeRaidStandbyRemainingSeconds(standbyTimer, s.gameplayNow()),
				}),
			},
		); err != nil {
			s.logGameEvent(session, "game-raid-start-post-ack-failed", "stage", "publish_standby", "raid_key", state.RaidKey, "error", err)
			return err
		}
	case runtimeRaidLifecyclePhaseActive:
		if err := s.broadcastRuntimeRaidPhaseSnapshot(state, false, time.Now().UTC()); err != nil {
			s.logGameEvent(session, "game-raid-start-post-ack-failed", "stage", "publish_active", "raid_key", state.RaidKey, "error", err)
			return err
		}
	}
	// The admission counters charged above ARE what the entry window prints as
	// 帐号日入场数 / 帐号周入场场数 / 角色周入场次数, and the client only re-renders them
	// when a packet arrives -- it never queries them. Without this push every member
	// keeps reading the pre-charge numbers until they relog (live 2026-09-30: start
	// Anton, leave the party, reopen the entry window, still read 3/3 and 2/2).
	//
	// Deliberately LAST: the handshake above (ACK, op14, mode3 roster, op633, party
	// frames, ready timer) is pinned packet-for-packet by
	// TestRuntimeRaidStartFreezesOnlineRosterAndOrdersAckBeforeCostSync, so the
	// counters ride the tail instead of joining it. Non-fatal by construction.
	s.broadcastCurrentRaidEntryCounters(state, "raid_start_after_admission_charge")
	s.logGameEvent(session, "game-raid-started",
		"type", typ,
		"raid_key", state.RaidKey,
		"member_count", len(state.FrozenMembers),
		"idempotent_replay", reservation.AlreadyStarted)
	return nil
}

// runtimeRaidTeamStartMinimumMembers is the server-side floor for a team raid.
//
// etc/raid/anton.etc says 10 ("raid start minimum member"), but the current client
// enforces its own floor of 2 -- clicking start with a lone member pops "需要2人以上"
// and sends nothing -- so a roster the client happily submits (the live log shows
// C2S 666 arriving with member_count=2) was being rejected here and the client
// silently swallowed the failure. The floor matches the client's own number; the
// parsed PVF value stays 10 and TestRealScriptPVFAntonConfigMatchesCurrentTwoPhaseCatalog
// still asserts it.
const runtimeRaidTeamStartMinimumMembers = 2

func runtimeRaidStartRosterBounds(state runtimeRaidState, config *raid.AntonConfig) (int, int) {
	if config == nil {
		return 1, 0
	}
	if state.TailFlag == 1 {
		return config.SingleStartMinimumMembers, 1
	}
	minimum := config.StartMinimumMembers
	if minimum > runtimeRaidTeamStartMinimumMembers {
		minimum = runtimeRaidTeamStartMinimumMembers
	}
	return minimum, config.MemberMax
}

// validateRuntimeRaidRosterReady is the "everybody is standing by, not off
// somewhere else" precondition of a start.
//
// Each frozen member's own session has to be on the raid channel and outside a
// dungeon run. A member still inside a sub-dungeon, or back on an ordinary
// channel, cannot reach the raid portal, and the roster must not start without
// them -- the official server refuses the start for exactly that reason.
//
// The town presence registry is deliberately not consulted here: the raid town
// suppresses presence for some transitions (the live log shows
// game-town-presence-suppressed reason=private_seria_room for a member standing in
// the raid town gate), so requiring a presence row would reject rosters that are
// perfectly fine. The member's town/area is reported in the diagnostic instead.
func (s *Service) validateRuntimeRaidRosterReady(session *gameSession, members []runtimeRaidMemberState) error {
	if s == nil || len(members) == 0 {
		return errRuntimeRaidNotMember
	}
	if !isCurrentImplementedRaidChannel(session) {
		return nil
	}
	for _, member := range members {
		identity, online := s.onlineGameSessionCharacterSnapshot(member.CharID)
		if !online || identity.session == nil {
			return fmt.Errorf("%w: frozen member %d is offline", errRuntimeRaidLifecycleState, member.CharID)
		}
		memberSession := identity.session
		if !isCurrentImplementedRaidChannel(memberSession) {
			return fmt.Errorf("%w: frozen member %d left the raid channel (channel=%d type=%d)",
				errRuntimeRaidLifecycleState, member.CharID, memberSession.channel.ID, memberSession.channel.Type)
		}
		memberSession.dungeon.mu.Lock()
		inDungeon := memberSession.dungeon.runtime != nil
		memberSession.dungeon.mu.Unlock()
		if inDungeon {
			return fmt.Errorf("%w: frozen member %d is still inside a dungeon",
				errRuntimeRaidLifecycleState, member.CharID)
		}
	}
	return nil
}

func (s *Service) handleOnlineRejoinRaid(session *gameSession, typ uint16, body []byte) error {
	identity, identityOK := s.boundGameSessionCharacterSnapshot(session)
	if !identityOK {
		return nil
	}
	req, err := raid.DecodeRejoinRaidRequest(body)
	if err != nil {
		s.logGameEvent(session, "game-raid-rejoin-body-invalid", "type", typ, "body_len", len(body), "error", err)
		return normalizeRuntimeRaidRejoinError(s.sendRuntimeRaidRejoinPacket(
			identity,
			typ,
			[]byte{0, runtimeRaidFailureCode},
			dnfproto.DefaultChannelClassification,
		))
	}
	state, added, ok := s.rejoinRuntimeRaidAdmission(req.RaidKey, identity.character)
	if !ok {
		return normalizeRuntimeRaidRejoinError(s.sendRuntimeRaidRejoinPacket(
			identity,
			typ,
			[]byte{0, runtimeRaidFailureCode},
			dnfproto.DefaultChannelClassification,
		))
	}
	if err := s.sendRuntimeRaidRejoinPacket(
		identity,
		typ,
		raid.BuildRaidCommandSuccessBody(),
		dnfproto.DefaultChannelClassification,
	); err != nil {
		return normalizeRuntimeRaidRejoinError(err)
	}
	if added {
		if err := s.broadcastRuntimeRaidRefreshExcept(state, identity.character); err != nil {
			return err
		}
		if err := s.broadcastRuntimeRaidBuffRegistration(state, identity.character); err != nil {
			return err
		}
	}
	return normalizeRuntimeRaidRejoinError(s.sendRuntimeRaidRejoinRecovery(identity, state, s.runtimeRaidRejoinNow()))
}

func (s *Service) handleOnlineRaidManagerWork(session *gameSession, typ uint16, body []byte) error {
	req, err := raid.DecodeManagerWorkRequest(body)
	if err != nil {
		s.logGameEvent(session, "game-raid-manager-body-invalid", "type", typ, "body_len", len(body), "error", err)
		return nil
	}
	sourceID := selectedCharacterID(session)
	// The first u32 selects the leader operation: 0 moves a member between
	// subgroups, 1 hands leadership to the named member. Treating every request
	// as a subgroup move used to turn a leader transfer into a bogus regroup.
	switch req.ActionOrMode {
	case runtimeRaidManagerWorkAssignGroup:
		state, ok := s.moveRuntimeRaidMember(sourceID, uint16(req.MemberCharKey), byte(req.TargetGroup))
		if !ok {
			// Log why the gate refused. "The leader drags a member and neither
			// side reacts" has two very different causes -- the value was out of
			// range, or the roster was frozen -- and only this line separates
			// them without a debugger.
			reason, editable, status, occupancy, leader := s.describeRuntimeRaidRegroupRefusal(sourceID)
			s.logGameEvent(session, "game-raid-manager-rejected",
				"type", typ,
				"operation", req.ActionOrMode,
				"source_char_id", sourceID,
				"source_scene", s.runtimeRaidSceneLabel(sourceID),
				"member_char_id", req.MemberCharKey,
				"member_scene", s.runtimeRaidSceneLabel(uint16(req.MemberCharKey)),
				"target_group", req.TargetGroup,
				"max_groups", runtimeRaidMaxGroups,
				"reason", reason,
				"groups_editable", editable,
				"lifecycle_status", status,
				"group_occupancy", occupancy,
				"sender_is_leader", leader)
			return nil
		}
		s.logGameEvent(session, "game-raid-manager-regrouped",
			"type", typ,
			"operation", req.ActionOrMode,
			"raid_key", state.RaidKey,
			"source_char_id", sourceID,
			"source_scene", s.runtimeRaidSceneLabel(sourceID),
			"member_char_id", req.MemberCharKey,
			"member_scene", s.runtimeRaidSceneLabel(uint16(req.MemberCharKey)),
			"target_group", req.TargetGroup,
			"groups_editable", runtimeRaidGroupsEditable(state),
			"group_occupancy", len(state.Lifecycle.GroupOccupancy),
			"started", state.Started)
		// The regroup is authoritative for the ordinary parties too once the raid
		// has started: 86JP drives a live reassignment through
		// ReassignLiveRaidMember, which retires the old party and forms the new
		// one (PartyManager.cs:210-326). Before the ready countdown this stays a
		// no-op on purpose -- see the gate inside applyRuntimeRaidParties.
		if err := s.applyRuntimeRaidParties(state); err != nil {
			s.logGameEvent(session, "game-raid-squad-party-projection-failed",
				"type", typ,
				"raid_key", state.RaidKey,
				"stage", "assign_group",
				"error", err)
		}
		// MCP/IDA evidence says S2C 669 is DoNothing, and the current client
		// materializes the attack-party panel from the mode=0 raid object
		// (raid_phase_runtime.go:158-162), not from the mode=3 roster. Pushing
		// only mode=3 left the leader's own panel on its previous snapshot --
		// live 2026-09-17T09:40:22.878Z the leader's last mode=0 was 09:40:18.270
		// with character 22 still in squad 1 (single/standby), while the member's
		// window already read the new mode=3 row. Publish both, exactly like
		// runtimeRaidManagerWorkTransferLeader just below.
		if err := s.broadcastRuntimeRaidCreate(state); err != nil {
			return err
		}
		if err := s.broadcastRuntimeRaidRefresh(state); err != nil {
			return err
		}
		return s.broadcastRuntimeRaidBuffRegistration(state, 0)
	case runtimeRaidManagerWorkTransferLeader:
		// Validate the argument before touching state: the transfer commits
		// under the raid lock and cannot be rolled back by a later check.
		var state runtimeRaidState
		ok := req.TargetGroup == 0
		if ok {
			state, ok = s.transferRuntimeRaidLeader(sourceID, uint16(req.MemberCharKey))
		}
		if !ok {
			s.logGameEvent(session, "game-raid-manager-rejected",
				"type", typ,
				"operation", req.ActionOrMode,
				"source_char_id", sourceID,
				"member_char_id", req.MemberCharKey,
				"argument", req.TargetGroup)
			return nil
		}
		// The mode=0 object carries the leader record; the mode=3 roster carries
		// the per-row leader flag. Publish both so every window agrees.
		if err := s.broadcastRuntimeRaidCreate(state); err != nil {
			return err
		}
		s.logGameEvent(session, "game-raid-leader-transferred",
			"raid_key", state.RaidKey,
			"old_leader_id", sourceID,
			"new_leader_id", state.LeaderID)
		return s.broadcastRuntimeRaidRefresh(state)
	default:
		s.logGameEvent(session, "game-raid-manager-rejected",
			"type", typ,
			"operation", req.ActionOrMode,
			"source_char_id", sourceID,
			"reason", "unknown_operation")
		return nil
	}
}

const (
	runtimeRaidManagerWorkAssignGroup    uint32 = 0
	runtimeRaidManagerWorkTransferLeader uint32 = 1
)

// transferRuntimeRaidLeader hands leadership to a live member before the
// attack starts. A member inside its disconnect grace window cannot lead: the
// raid would have nobody able to start it.
func (s *Service) transferRuntimeRaidLeader(sourceID uint16, targetID uint16) (runtimeRaidState, bool) {
	if sourceID == 0 || targetID == 0 || sourceID == targetID {
		return runtimeRaidState{}, false
	}
	s.raidMu.Lock()
	defer s.raidMu.Unlock()
	state := s.runtimeRaidByMemberLocked(sourceID)
	if state == nil || state.LeaderID != sourceID || state.StartPending || state.Started || state.TailFlag == 1 {
		return runtimeRaidState{}, false
	}
	if runtimeRaidMemberIndex(*state, targetID) < 0 || s.runtimeRaidMemberDisconnectedLocked(*state, targetID) {
		return runtimeRaidState{}, false
	}
	if _, online := s.onlineGameSession(targetID); !online {
		return runtimeRaidState{}, false
	}
	state.LeaderID = targetID
	return cloneRuntimeRaidState(*state), true
}

func (s *Service) handleOnlineRaidMemberChangeState(session *gameSession, typ uint16, body []byte) error {
	req, err := raid.DecodeMemberChangeStateRequest(body)
	if err != nil {
		s.logGameEvent(session, "game-raid-member-state-body-invalid", "type", typ, "body_len", len(body), "error", err)
		return nil
	}
	sourceID := selectedCharacterID(session)
	state, ok := s.changeRuntimeRaidMemberState(sourceID, normalizeRuntimeRaidMemberState(req.State))
	if !ok {
		s.logGameEvent(session, "game-raid-member-state-ignored",
			"type", typ,
			"source_char_id", sourceID,
			"state", req.State)
		return nil
	}
	// MCP/IDA 证据显示 823 是客户端成员状态变化；可见同步走 0x24F 成员名单刷新。
	return s.broadcastRuntimeRaidRefresh(state)
}

func normalizeRuntimeRaidMemberState(state byte) byte {
	if state == 0 {
		return 0
	}
	return 1
}

func (s *Service) createRuntimeRaid(session *gameSession, sourceID uint16, req raid.InfoRequest) (runtimeRaidState, bool) {
	s.raidMu.Lock()
	defer s.raidMu.Unlock()
	s.ensureRuntimeRaidMapsLocked()
	if existing := s.runtimeRaidByMemberLocked(sourceID); existing != nil {
		ownerMode := runtimeRaidModeForSession(session)
		if existing.LeaderID != sourceID ||
			(existing.RaidMode != 0 && existing.RaidMode != ownerMode) ||
			existing.StartPending || existing.Started || existing.EntryCostPaid ||
			existing.RouteOrRaidType != req.RouteOrRaidType || !bytes.Equal(existing.NameBytes, req.NameBytes) ||
			existing.TailFlag != req.TailFlag {
			return runtimeRaidState{}, false
		}
		// Replayed CREATE_RAID is idempotent only when it describes the exact
		// server-owned object. A reconnecting client can reopen the mode selector
		// and submit a different single/normal choice before it has received the
		// rejoin snapshot; never replace the unfinished raid or its progress.
		return cloneRuntimeRaidState(*existing), true
	}
	members := s.initialRuntimeRaidMembersLocked(session, sourceID)
	if req.TailFlag == 1 && len(members) > 1 {
		members = members[:1]
	}
	// The leader opens the raid in squad 1; everybody else joins in standby.
	//
	// Live 2026-09-17 22:0x, the attack-party status window: the leader's own row is
	// expected to carry 1队 while the members it invited are 单人 until it assigns
	// them (the client reads the roster byte straight and renders 0 as 单人). The
	// members the leader is already partied with are left in standby too: an
	// ordinary party is projected into one squad by
	// syncRuntimeRaidSquadsWithOrdinaryParty, and the 编制 is the leader's to hand
	// out until that happens.
	if len(members) > 0 {
		members[0].GroupIndex = runtimeRaidDefaultGroup
		members[0].SlotOrder = 0
	}
	for _, member := range members {
		if s.runtimeRaidByMemberLocked(member.CharID) != nil {
			return runtimeRaidState{}, false
		}
	}
	key := s.nextRuntimeRaidKeyLocked(session, sourceID)
	state := runtimeRaidState{
		RaidKey:         key,
		LeaderID:        sourceID,
		RaidMode:        runtimeRaidModeForSession(session),
		RouteOrRaidType: req.RouteOrRaidType,
		NameBytes:       append([]byte(nil), req.NameBytes...),
		TailFlag:        req.TailFlag,
		SingleRaidType:  0,
	}
	state.Members = members
	s.raids[key] = &state
	s.logPacketEvent("game-raid-created-roster",
		"raid_key", key,
		"leader_id", sourceID,
		"member_count", len(members),
		"leader_group", runtimeRaidMemberSquad(members[0].GroupIndex),
		"standby_members", countRuntimeRaidStandbyMembers(members),
		"source_party_members", len(members)-1,
		"tail_flag", req.TailFlag)
	return cloneRuntimeRaidState(state), true
}

func (s *Service) attachRuntimeRaidOwnerConfig(raidKey uint32, ownerConfig runtimeRaidOwnerConfig) (runtimeRaidState, bool) {
	if s == nil || raidKey == 0 || ownerConfig.Mode == 0 || ownerConfig.Raid == nil {
		return runtimeRaidState{}, false
	}
	s.raidMu.Lock()
	defer s.raidMu.Unlock()
	state := s.raids[raidKey]
	if state == nil || state.Started || state.StartPending || state.EntryCostPaid ||
		(state.RaidMode != 0 && state.RaidMode != ownerConfig.Mode) {
		return runtimeRaidState{}, false
	}
	state.RaidMode = ownerConfig.Mode
	state.AntonConfig = nil
	state.LukeConfig = nil
	state.FiendWarConfig = nil
	if ownerConfig.Mode == runtimeRaidOwnerLuke {
		if ownerConfig.Luke == nil || ownerConfig.Luke.Raid != ownerConfig.Raid {
			return runtimeRaidState{}, false
		}
		state.LukeConfig = ownerConfig.Luke
	} else {
		state.AntonConfig = ownerConfig.Raid
	}
	if ownerConfig.Mode == runtimeRaidOwnerFiendWar {
		if ownerConfig.FiendWar == nil || ownerConfig.FiendWar.Battle == nil ||
			ownerConfig.FiendWar.Battle.Raid != ownerConfig.Raid {
			return runtimeRaidState{}, false
		}
		state.FiendWarConfig = ownerConfig.FiendWar
	}
	return cloneRuntimeRaidState(*state), true
}

func (s *Service) modifyRuntimeRaidInfo(sourceID uint16, req raid.InfoRequest) (runtimeRaidState, bool) {
	s.raidMu.Lock()
	defer s.raidMu.Unlock()
	state := s.runtimeRaidByMemberLocked(sourceID)
	if state == nil || state.LeaderID != sourceID || state.StartPending || state.Started {
		return runtimeRaidState{}, false
	}
	if state.TailFlag == 1 {
		return runtimeRaidState{}, false
	}
	state.RouteOrRaidType = req.RouteOrRaidType
	state.NameBytes = append(state.NameBytes[:0], req.NameBytes...)
	state.TailFlag = req.TailFlag
	return cloneRuntimeRaidState(*state), true
}

func (s *Service) runtimeRaidForMember(sourceID uint16) (runtimeRaidState, bool) {
	if sourceID == 0 {
		return runtimeRaidState{}, false
	}
	s.raidMu.Lock()
	defer s.raidMu.Unlock()
	state := s.runtimeRaidByMemberLocked(sourceID)
	if state == nil {
		return runtimeRaidState{}, false
	}
	return cloneRuntimeRaidState(*state), true
}

// runtimeRaidKeyForMember is the cheap form of runtimeRaidForMember: callers only
// need the key, and it runs on every town area enter, so it must not clone the
// whole roster.
func (s *Service) runtimeRaidKeyForMember(sourceID uint16) uint32 {
	if s == nil || sourceID == 0 {
		return 0
	}
	s.raidMu.Lock()
	defer s.raidMu.Unlock()
	state := s.runtimeRaidByMemberLocked(sourceID)
	if state == nil {
		return 0
	}
	return state.RaidKey
}

type runtimeRaidStartReservation struct {
	State          runtimeRaidState
	Attempt        uint64
	AlreadyStarted bool
}

func (s *Service) reserveRuntimeRaidStart(sourceID uint16) (runtimeRaidStartReservation, error) {
	if sourceID == 0 {
		return runtimeRaidStartReservation{}, errRuntimeRaidNotMember
	}
	s.raidMu.Lock()
	state := s.runtimeRaidByMemberLocked(sourceID)
	if state == nil || state.LeaderID != sourceID {
		s.raidMu.Unlock()
		return runtimeRaidStartReservation{}, errRuntimeRaidNotMember
	}
	if len(state.Members) == 0 || runtimeRaidHasOversizedGroup(*state) {
		s.raidMu.Unlock()
		return runtimeRaidStartReservation{}, errRuntimeRaidLifecycleState
	}
	if state.Started {
		reservation := runtimeRaidStartReservation{
			State:          cloneRuntimeRaidState(*state),
			Attempt:        state.StartAttempt,
			AlreadyStarted: true,
		}
		s.raidMu.Unlock()
		return reservation, nil
	}
	if state.StartPending {
		s.raidMu.Unlock()
		return runtimeRaidStartReservation{}, errRuntimeRaidLifecycleState
	}
	state.StartAttempt++
	if state.StartAttempt == 0 {
		state.StartAttempt++
	}
	state.StartPending = true
	state.FrozenMembers = append(state.FrozenMembers[:0], state.Members...)
	runtimeRaidSortMembers(state.FrozenMembers)
	reservation := runtimeRaidStartReservation{
		State:   cloneRuntimeRaidState(*state),
		Attempt: state.StartAttempt,
	}
	s.raidMu.Unlock()
	if err := s.validateRuntimeRaidRosterOnline(reservation.State.FrozenMembers); err != nil {
		s.cancelRuntimeRaidStart(reservation)
		return reservation, err
	}
	return reservation, nil
}

func (s *Service) validateRuntimeRaidRosterOnline(members []runtimeRaidMemberState) error {
	if len(members) == 0 {
		return errRuntimeRaidNotMember
	}
	for _, member := range members {
		identity, ok := s.onlineGameSessionCharacterSnapshot(member.CharID)
		if !ok || !s.boundGameSessionCharacterCurrent(identity) {
			return fmt.Errorf("%w: frozen member %d is offline", errRuntimeRaidLifecycleState, member.CharID)
		}
	}
	return nil
}

func (s *Service) cancelRuntimeRaidStart(reservation runtimeRaidStartReservation) {
	s.raidMu.Lock()
	defer s.raidMu.Unlock()
	state := s.raids[reservation.State.RaidKey]
	if state == nil || state.Started || state.LeaderID != reservation.State.LeaderID || state.StartAttempt != reservation.Attempt {
		return
	}
	state.StartPending = false
	state.FrozenMembers = nil
}

func (s *Service) setRuntimeRaidWaiting(sourceID uint16, waiting byte) (runtimeRaidState, raid.WaitingRow, bool) {
	if sourceID == 0 {
		return runtimeRaidState{}, raid.WaitingRow{}, false
	}
	s.raidMu.Lock()
	defer s.raidMu.Unlock()
	state := s.runtimeRaidByMemberLocked(sourceID)
	if state == nil || state.StartPending || state.Started {
		return runtimeRaidState{}, raid.WaitingRow{}, false
	}
	index := runtimeRaidMemberIndex(*state, sourceID)
	if index < 0 {
		return runtimeRaidState{}, raid.WaitingRow{}, false
	}
	state.Members[index].Waiting = waiting
	return cloneRuntimeRaidState(*state), raid.WaitingRow{MemberKey: sourceID, State: waiting}, true
}

// removeRuntimeRaidMember removes one character under the raid lock.
func (s *Service) removeRuntimeRaidMember(sourceID uint16) (runtimeRaidState, bool, bool) {
	s.raidMu.Lock()
	defer s.raidMu.Unlock()
	state, removed, disbanded := s.removeRuntimeRaidMemberLocked(sourceID)
	if state == nil {
		return runtimeRaidState{}, removed, disbanded
	}
	return cloneRuntimeRaidState(*state), removed, disbanded
}

func (s *Service) commitRuntimeRaidStart(reservation runtimeRaidStartReservation, updates []runtimeRaidEntryCostUpdate, config *raid.AntonConfig, now time.Time) (runtimeRaidState, bool) {
	return s.commitRuntimeRaidStartWithOwner(reservation, updates, runtimeRaidOwnerConfig{
		Mode: runtimeRaidOwnerAnton,
		Raid: config,
	}, now)
}

func (s *Service) commitRuntimeRaidStartWithOwner(reservation runtimeRaidStartReservation, updates []runtimeRaidEntryCostUpdate, ownerConfig runtimeRaidOwnerConfig, now time.Time) (runtimeRaidState, bool) {
	s.raidMu.Lock()
	defer s.raidMu.Unlock()
	state := s.raids[reservation.State.RaidKey]
	if state == nil || state.LeaderID != reservation.State.LeaderID || len(state.FrozenMembers) == 0 || !state.StartPending || state.StartAttempt != reservation.Attempt {
		return runtimeRaidState{}, false
	}
	if runtimeRaidHasOversizedGroup(*state) {
		return runtimeRaidState{}, false
	}
	chargedMembers := runtimeRaidMemberIDs(runtimeRaidState{Members: reservation.State.FrozenMembers})
	if !sameRuntimeRaidMembers(state.FrozenMembers, chargedMembers) || !sameRuntimeRaidMembers(state.Members, chargedMembers) {
		return runtimeRaidState{}, false
	}
	if state.RaidMode == 0 {
		state.RaidMode = ownerConfig.Mode
	}
	if ownerConfig.Raid == nil || state.RaidMode != ownerConfig.Mode {
		return runtimeRaidState{}, false
	}
	if state.TailFlag == 1 {
		switch state.RaidMode {
		case runtimeRaidOwnerLuke:
			state.SingleRaidType = runtimeRaidLukeSingleTypeForTime(now)
		case runtimeRaidOwnerFiendWar:
			state.SingleRaidType = 0
		default:
			state.SingleRaidType = runtimeRaidSingleTypeForTime(now)
		}
	}
	lifecycle, err := newRuntimeRaidLifecycleStateFromModeAndType(ownerConfig.Raid, state.RaidMode, now, state.TailFlag == 1, state.SingleRaidType)
	if err != nil {
		return runtimeRaidState{}, false
	}
	if ownerConfig.Mode == runtimeRaidOwnerFiendWar {
		if ownerConfig.FiendWar == nil || ownerConfig.FiendWar.Battle == nil {
			return runtimeRaidState{}, false
		}
		lifecycle.FiendWar, err = raid.NewFiendWarBattle(ownerConfig.FiendWar.Battle)
		if err != nil {
			return runtimeRaidState{}, false
		}
		if state.TailFlag == 1 {
			if ownerConfig.FiendWar.Single == nil || len(ownerConfig.FiendWar.Single.Routes) == 0 || len(ownerConfig.FiendWar.Single.BossAreas) == 0 {
				return runtimeRaidState{}, false
			}
			route := ownerConfig.FiendWar.Single.Routes[rand.IntN(len(ownerConfig.FiendWar.Single.Routes))]
			bossArea := ownerConfig.FiendWar.Single.BossAreas[rand.IntN(len(ownerConfig.FiendWar.Single.BossAreas))]
			if err := lifecycle.FiendWar.ConfigureSingle(route, bossArea); err != nil {
				return runtimeRaidState{}, false
			}
			s.logGameEvent(nil, "game-fiendwar-single-plan-selected",
				"raid_key", state.RaidKey,
				"leader_id", state.LeaderID,
				"route", route.Name,
				"boss_area", bossArea,
				"boss_dungeon", ownerConfig.FiendWar.Battle.Areas[bossArea].Dungeon)
		}
		// 全员从 PVF 等待区起步，而且必须在开团这一刻就填好：
		// moveRuntimeFiendWarGroup 与 tryRuntimeFiendWarEncounter 都靠
		// GroupAreas/MemberAreas 判断"队伍现在在哪"，空表会让第一次走位被判成
		// "两个区域不相邻"。安东/卢克不建战场，这段整个不执行。
		for _, member := range state.FrozenMembers {
			lifecycle.FiendWar.GroupAreas[runtimeRaidNormalizedGroup(member.GroupIndex)] = ownerConfig.FiendWar.Battle.WaitingArea
			lifecycle.FiendWar.MemberAreas[member.CharID] = ownerConfig.FiendWar.Battle.WaitingArea
		}
	}
	state.Started = true
	state.StartPending = false
	state.EntryCostPaid = true
	state.ChargedMembers = append(state.ChargedMembers[:0], chargedMembers...)
	state.PendingCostSync = cloneRuntimeRaidEntryCostUpdates(updates)
	state.AntonConfig = nil
	state.LukeConfig = nil
	state.FiendWarConfig = nil
	if ownerConfig.Mode == runtimeRaidOwnerLuke {
		state.LukeConfig = ownerConfig.Luke
	} else {
		state.AntonConfig = ownerConfig.Raid
	}
	if ownerConfig.Mode == runtimeRaidOwnerFiendWar {
		state.FiendWarConfig = ownerConfig.FiendWar
	}
	state.Lifecycle = lifecycle
	return cloneRuntimeRaidState(*state), true
}

func (s *Service) rejoinRuntimeRaid(raidKey uint32, sourceID uint16) (runtimeRaidState, bool) {
	state, _, ok := s.rejoinRuntimeRaidAdmission(raidKey, sourceID)
	return state, ok
}

func (s *Service) rejoinRuntimeRaidAdmission(raidKey uint32, sourceID uint16) (runtimeRaidState, bool, bool) {
	if sourceID == 0 {
		return runtimeRaidState{}, false, false
	}
	s.raidMu.Lock()
	defer s.raidMu.Unlock()
	return s.rejoinRuntimeRaidAdmissionLocked(raidKey, sourceID)
}

// rejoinRuntimeRaidAdmissionLocked is the single roster-admission rule shared
// by the native op668 path and leader-approved peer admission. Caller holds
// raidMu. The second result reports whether a new member row was appended.
func (s *Service) rejoinRuntimeRaidAdmissionLocked(raidKey uint32, sourceID uint16) (runtimeRaidState, bool, bool) {
	if sourceID == 0 {
		return runtimeRaidState{}, false, false
	}
	state := s.raids[raidKey]
	if state == nil {
		return runtimeRaidState{}, false, false
	}
	for otherRaidKey, otherState := range s.raids {
		if otherRaidKey != raidKey && otherState != nil && runtimeRaidMemberIndex(*otherState, sourceID) >= 0 {
			return runtimeRaidState{}, false, false
		}
	}
	added := false
	if runtimeRaidMemberIndex(*state, sourceID) < 0 {
		if state.StartPending || state.Started {
			return runtimeRaidState{}, false, false
		}
		if state.TailFlag == 1 {
			return runtimeRaidState{}, false, false
		}
		if len(state.Members) >= raid.MaxAttackPartyMembers {
			return runtimeRaidState{}, false, false
		}
		// A joiner lands in standby, not in squad 1. The roster record carries the
		// squad byte straight from the wire (sub_17B6EF0 stores it at +0x2D with no
		// arithmetic), and every squad lookup in the client compares it against 1..
		// so 0 matches no squad and the member shows as unassigned -- which is what
		// the leader is then expected to fix by dragging them into a squad.
		state.Members = append(state.Members, s.newRuntimeRaidMemberLocked(sourceID, 0, 0))
		runtimeRaidSortMembers(state.Members)
		// A seat reserved for an earlier raid is void once the character joins
		// a roster through a live session.
		delete(s.raidDisconnects, sourceID)
		added = true
	}
	return cloneRuntimeRaidState(*state), added, true
}

func runtimeRaidFirstAvailablePosition(state runtimeRaidState) (byte, byte, bool) {
	maxGroups := runtimeRaidGroupMaxFor(state)
	size := runtimeRaidGroupSizeFor(state)
	if maxGroups == 0 || size == 0 {
		return 0, 0, false
	}
	occupied := make([][]bool, int(maxGroups)+1)
	counts := make([]int, int(maxGroups)+1)
	for index := range occupied {
		occupied[index] = make([]bool, size)
	}
	for _, member := range state.Members {
		group := member.GroupIndex
		// Unassigned members occupy no squad slot. Folding them into squad 1, as
		// this used to, made a squad read as full while the roster showed nobody
		// in it.
		if group < 1 || group > maxGroups {
			continue
		}
		counts[group]++
		if int(member.SlotOrder) < size {
			occupied[group][member.SlotOrder] = true
		}
	}
	for group := byte(1); group <= maxGroups; group++ {
		if counts[group] >= size {
			continue
		}
		for slot := 0; slot < size; slot++ {
			if !occupied[group][slot] {
				return group, byte(slot), true
			}
		}
	}
	return 0, 0, false
}

func (s *Service) changeRuntimeRaidMemberState(sourceID uint16, userState byte) (runtimeRaidState, bool) {
	if sourceID == 0 {
		return runtimeRaidState{}, false
	}
	s.raidMu.Lock()
	defer s.raidMu.Unlock()
	state := s.runtimeRaidByMemberLocked(sourceID)
	if state == nil {
		return runtimeRaidState{}, false
	}
	index := runtimeRaidMemberIndex(*state, sourceID)
	if index < 0 {
		return runtimeRaidState{}, false
	}
	state.Members[index].UserState = userState
	return cloneRuntimeRaidState(*state), true
}

func (s *Service) moveRuntimeRaidMember(sourceID uint16, memberID uint16, targetGroup byte) (runtimeRaidState, bool) {
	s.raidMu.Lock()
	defer s.raidMu.Unlock()
	state := s.runtimeRaidByMemberLocked(sourceID)
	if state == nil || state.LeaderID != sourceID || !runtimeRaidGroupsEditable(*state) {
		return runtimeRaidState{}, false
	}
	if state.TailFlag == 1 {
		return runtimeRaidState{}, false
	}
	if targetGroup > runtimeRaidGroupMaxFor(*state) {
		// 超时空只有 8 个编制；9/10 是给安东/卢克那 40 人格子留的。
		return runtimeRaidState{}, false
	}
	index := runtimeRaidMemberIndex(*state, memberID)
	if index < 0 {
		return runtimeRaidState{}, false
	}
	if targetGroup == 0 {
		// Group 0 is "no squad", and the client uses it on the wire: the op669
		// sender at 0x01B48292 walks the roster and emits {action 0, memberKey, 0}
		// for each member it wants pulled out of its squad. Refusing it here left
		// the leader unable to return anyone to standby.
		state.Members[index].GroupIndex = 0
		state.Members[index].SlotOrder = 0
	} else {
		// The native roster is a fixed runtimeRaidMaxGroups x runtimeRaidGroupSize
		// grid addressed by (group, slot). A full target group has no free slot, so
		// nextRuntimeRaidSlotExcluding would fall back to the last slot and place
		// two members on the same grid cell.
		//
		// 超时空不是"拒绝"，而是**挤人**：一个小队编制只能放 1 人（操作员
		// 2026-10-02 的定案，客户端小队编制界面每队后面三格锁着），所以把 B 拖进
		// 已经有 A 的队时，A 被退回未编队（单人），B 站上这个位置：
		//
		//	「就比如说 团长给A放在2队 然后再给B放在2队 A就应该变单人了
		//	  一个小队仅能有1人」
		//
		// 安东/卢克保持原样：满队仍是整脚拒绝，客户端的编制界面也不会因此变形。
		size := runtimeRaidGroupSizeFor(*state)
		if runtimeRaidGroupCountExcluding(*state, targetGroup, memberID) >= size {
			if state.RaidMode != runtimeRaidOwnerFiendWar {
				// 安东/卢克：满队仍然是整脚拒绝（原样，客户端编制界面不会因此变形）。
				return runtimeRaidState{}, false
			}
			if !s.evictRuntimeRaidGroupLocked(state, targetGroup, memberID, "fiendwar_squad_holds_one_member") {
				return runtimeRaidState{}, false
			}
		}
		state.Members[index].GroupIndex = targetGroup
		state.Members[index].SlotOrder = nextRuntimeRaidSlotExcluding(*state, targetGroup, memberID)
	}
	runtimeRaidSortMembers(state.Members)
	if state.Started {
		// Phase-two dungeon entry binds groups from the frozen roster, so a
		// standby regroup has to land there as well or the next phase would
		// still use the phase-one squads.
		for i := range state.FrozenMembers {
			if state.FrozenMembers[i].CharID != memberID {
				continue
			}
			state.FrozenMembers[i].GroupIndex = state.Members[index].GroupIndex
			state.FrozenMembers[i].SlotOrder = state.Members[index].SlotOrder
		}
		runtimeRaidSortMembers(state.FrozenMembers)
	}
	return cloneRuntimeRaidState(*state), true
}

// evictRuntimeRaidGroupLocked 把 targetGroup 里除 keepID 以外的人退回**未编队**
// （GroupIndex 0 = 客户端画的"单人"），返回是否真的挪走了人。
//
// 只在"一个小队只能放 1 人"的团（超时空）上调用，见 moveRuntimeRaidMember。
// 调用者持 raidMu。活名册与冻结名册一起改：进图名单、清数记账、战况表都读后者，
// 只改一边会让"编制里是谁"和"谁能进图"对不上。
func (s *Service) evictRuntimeRaidGroupLocked(
	state *runtimeRaidState,
	targetGroup byte,
	keepID uint16,
	reason string,
) bool {
	if state == nil || targetGroup == 0 {
		return false
	}
	evicted := make([]uint16, 0, 1)
	for i := range state.Members {
		if state.Members[i].CharID == keepID || runtimeRaidMemberSquad(state.Members[i].GroupIndex) != targetGroup {
			continue
		}
		evicted = append(evicted, state.Members[i].CharID)
		state.Members[i].GroupIndex = 0
		state.Members[i].SlotOrder = 0
	}
	if len(evicted) == 0 {
		return false
	}
	for _, charID := range evicted {
		for i := range state.FrozenMembers {
			if state.FrozenMembers[i].CharID != charID {
				continue
			}
			state.FrozenMembers[i].GroupIndex = 0
			state.FrozenMembers[i].SlotOrder = 0
		}
	}
	runtimeRaidSortMembers(state.FrozenMembers)
	s.logGameEvent(nil, "game-raid-manager-squad-evicted",
		"raid_key", state.RaidKey,
		"target_group", targetGroup,
		"kept_char_id", keepID,
		"evicted_char_ids", evicted,
		"evicted_count", len(evicted),
		"reason", reason)
	return true
}

// runtimeRaidGroupsEditable reports when the leader may rearrange subgroups:
// before the ready countdown, and for the whole duration of a running attack --
// including while some squads are inside a dungeon.
//
// The old rule also required phase one, standby, and an empty GroupOccupancy, so
// a leader standing in the waiting area could not touch a squad whose members
// were inside a dungeon. That read in game as "团长和队员必须在同一区域才能改编制":
// with both players in the same area there is no occupancy and the regroup went
// through, and with one of them inside a dungeon the request was dropped
// silently (`game-raid-manager-rejected`, no packet on either side). The squad
// is roster bookkeeping, not dungeon ownership:
//
//   - the squad -> ordinary-party projection is rebuilt on every regroup
//     (applyRuntimeRaidParties -> materializeRuntimeRaidSquadParty), exactly like
//     86JP's ReassignLiveRaidMember (PartyManager.cs:210-326);
//   - phase-two dungeon binding reads FrozenMembers, which
//     moveRuntimeRaidMember keeps in step with the live roster;
//   - a member's own dungeon run stays keyed by (map, group) in the lifecycle,
//     and no path resolves a run from the current roster during the run.
//
// What still has to stay frozen is the two windows where the roster itself is
// being decided or paid out: the ready countdown (StartPending) and the
// movie/card presentation (AwaitingMovie / AwaitingRewards), which resolves the
// per-squad reward rows.
func runtimeRaidGroupsEditable(state runtimeRaidState) bool {
	if state.StartPending || state.TailFlag == 1 {
		return false
	}
	if !state.Started {
		return true
	}
	switch state.Lifecycle.Status {
	case runtimeRaidLifecyclePhaseActive, runtimeRaidLifecycleStandby:
		return true
	default:
		return false
	}
}

// runtimeRaidSceneLabel renders one character's current scene for the
// raid manager-work log lines. The leader's regroup and the member's own window
// read the same roster, so when one of them reports "no reaction", the first
// thing worth ruling out is that the two were in different scenes (a member
// inside a dungeon run, a member in a private room) when the request arrived.
// The label deliberately names the scene, not the area, because a dungeon run
// and a private room both report area/instance values the town does not use.
func (s *Service) runtimeRaidSceneLabel(characterID uint16) string {
	if characterID == 0 {
		return "none"
	}
	info, ok := s.peerActorInfoForCharacter(characterID)
	if !ok {
		return "offline"
	}
	return fmt.Sprintf("ch=%d town=%d area=%d instance=%d",
		info.ChannelID, info.TownID, info.AreaID, info.InstanceKey)
}

// describeRuntimeRaidRegroupRefusal mirrors the accept conditions of
// moveRuntimeRaidMember and reports which one refused, plus the state that made
// it refuse. Diagnostic only -- no caller branches on it.
func (s *Service) describeRuntimeRaidRegroupRefusal(sourceID uint16) (
	reason string, editable bool, status uint16, occupancy int, leader bool) {
	s.raidMu.Lock()
	defer s.raidMu.Unlock()
	state := s.runtimeRaidByMemberLocked(sourceID)
	if state == nil {
		return "sender_not_in_raid", false, 0, 0, false
	}
	status = uint16(state.Lifecycle.Status)
	occupancy = len(state.Lifecycle.GroupOccupancy)
	editable = runtimeRaidGroupsEditable(*state)
	leader = state.LeaderID == sourceID
	if !leader {
		return "sender_not_leader", editable, status, occupancy, leader
	}
	if !editable {
		return "groups_frozen", editable, status, occupancy, leader
	}
	return "unknown", editable, status, occupancy, leader
}

// runtimeRaidGroupCountExcluding counts the members already occupying one
// subgroup, ignoring the member that is about to be moved. A member that is
// re-slotted inside its current subgroup therefore never counts against its
// own capacity. Unassigned members (group 0) belong to no subgroup and are not
// counted at all.
func runtimeRaidGroupCountExcluding(state runtimeRaidState, group byte, exclude uint16) int {
	if group == 0 {
		return 0
	}
	count := 0
	for _, member := range state.Members {
		if member.CharID == exclude || member.GroupIndex != group {
			continue
		}
		count++
	}
	return count
}

// broadcastRuntimeRaidPhaseClearRegistration re-sends the mode=3 member list to
// every member of one raid. The per-member phase clear count travels inside the
// member record -- the current client has no 86JP-style column footer on this
// packet, its member-list reader ends right after the records -- so this push is
// the only way the 攻击队 window's 地下城通关 column can leave 0 before the next
// roster change. It deliberately skips the public-directory refresh that a full
// roster broadcast performs: clearing a gate changes no listing.
func (s *Service) broadcastRuntimeRaidPhaseClearRegistration(state runtimeRaidState) error {
	body, err := raid.BuildMemberRefreshMode3Body(runtimeRaidMemberRefresh(state))
	if err != nil {
		return err
	}
	var firstErr error
	for _, member := range state.Members {
		session, ok := s.onlineGameSession(member.CharID)
		if !ok {
			continue
		}
		if err := s.sendGameUpperRawClassCodec(
			session,
			raid.RaidMemberRefreshMsgID,
			body,
			0,
			true,
		); err != nil && firstErr == nil {
			firstErr = err
		}
	}
	return firstErr
}

func (s *Service) broadcastRuntimeRaidRefresh(state runtimeRaidState) error {
	body, err := raid.BuildMemberRefreshMode3Body(runtimeRaidMemberRefresh(state))
	if err != nil {
		return err
	}
	// The 0x024F mode-3 roster is the ONLY source of the squad byte the client's
	// 战况表 draws each attack group from: the client stores the wire byte as-is and
	// renders 0 as 单人 (see runtimeRaidMemberSquad). So a roster that carries a 0
	// for a member who is meant to be in a squad is the board's "显示个单" -- and
	// once it is drawn that way nothing re-sends the row unless a roster change
	// happens. Log the bytes actually put on the wire, with the members that carry
	// 0 called out, so that report is answerable from the log alone.
	s.logRuntimeRaidSquadBytes(state, "game-raid-roster-refresh-squad-bytes", "roster_refresh_mode3")
	var firstErr error
	for _, member := range state.Members {
		session, ok := s.onlineGameSession(member.CharID)
		if !ok {
			continue
		}
		if err := s.sendGameUpperRawClassCodec(session, raid.RaidMemberRefreshMsgID, body, 0, true); err != nil && firstErr == nil {
			firstErr = err
		}
	}
	s.broadcastRuntimeRaidBrowseRefresh(state, body, 0)
	s.refreshRuntimePublicRaidDirectories("raid_state_changed")
	return firstErr
}

// logRuntimeRaidSquadBytes reports the squad label (and slot) each member carries
// in the roster the client draws its 战况表 from, plus the members whose label is
// 0 (unassigned), which the client renders as 单人.
func (s *Service) logRuntimeRaidSquadBytes(state runtimeRaidState, event string, source string) {
	if s == nil || state.RaidKey == 0 {
		return
	}
	unassigned := make([]uint16, 0, len(state.Members))
	labels := make([]string, 0, len(state.Members))
	for _, member := range state.Members {
		squad := runtimeRaidMemberSquad(member.GroupIndex)
		if squad == 0 {
			unassigned = append(unassigned, member.CharID)
		}
		labels = append(labels, fmt.Sprintf("%d:%d:%d", member.CharID, squad, member.SlotOrder))
	}
	s.logPacketEvent(event,
		"raid_key", state.RaidKey,
		"raid_phase", int(state.Lifecycle.Phase),
		"lifecycle_status", int(state.Lifecycle.Status),
		"raid_started", state.Started,
		"source", source,
		"member_count", len(state.Members),
		"frozen_count", len(state.FrozenMembers),
		"squad_labels", strings.Join(labels, ","),
		"unassigned_members", unassigned,
		"unassigned_count", len(unassigned),
		"client_visible", "a_0_squad_byte_is_drawn_as_单人_in_the_battle_status_table")
	s.recordRaidStateEventLight(nil, "raid_squad_bytes", &state, map[string]any{
		"source":             source,
		"squad_labels":       strings.Join(labels, ","),
		"unassigned_members": unassigned,
		"unassigned_count":   len(unassigned),
		"result":             "roster_published",
	})
}

func (s *Service) broadcastRuntimeRaidRefreshExcept(state runtimeRaidState, excludedCharacterID uint16) error {
	body, err := raid.BuildMemberRefreshMode3Body(runtimeRaidMemberRefresh(state))
	if err != nil {
		return err
	}
	var firstErr error
	for _, member := range state.Members {
		if member.CharID == excludedCharacterID {
			continue
		}
		session, ok := s.onlineGameSession(member.CharID)
		if !ok {
			continue
		}
		if err := s.sendGameUpperRawClassCodec(session, raid.RaidMemberRefreshMsgID, body, 0, true); err != nil && firstErr == nil {
			firstErr = err
		}
	}
	s.broadcastRuntimeRaidBrowseRefresh(state, body, excludedCharacterID)
	s.refreshRuntimePublicRaidDirectories("raid_state_changed")
	return firstErr
}

func (s *Service) broadcastRuntimeRaidBuffRegistration(state runtimeRaidState, excludedCharacterID uint16) error {
	body, err := s.runtimeRaidBuffStatusBody(state)
	if err != nil {
		return err
	}
	var firstErr error
	for _, member := range state.Members {
		if member.CharID == excludedCharacterID {
			continue
		}
		session, ok := s.onlineGameSession(member.CharID)
		if !ok {
			continue
		}
		if err := s.sendGameUpperRawClassCodec(session, raid.RaidBuffMsgID, body, 0, true); err != nil && firstErr == nil {
			firstErr = err
		}
	}
	return firstErr
}

// applyRuntimeRaidParties reconciles the raid's attack squads with the ordinary
// party graph. It is the single owner of that projection and it is idempotent:
// after a call, every non-zero squad is exactly one real ordinary party whose
// members, order and slots match the roster, and no member outside every squad
// keeps a party this projection created.
//
// The invariant matters because the client draws its own party HUD -- and the
// party roster the raid windows feed it -- from the ordinary party table, not
// from the raid roster: a squad that exists only in the raid roster is invisible
// to its own members. 86JP enforces the same shape as a hard gate before a raid
// starts (PartyManager.ArePreparedRaidPartiesReady, PartyManager.cs:363-382) and
// rebuilds the parties on a live regroup (ReassignLiveRaidMember,
// PartyManager.cs:210-326). This service has no client-side gate, so it has to
// produce that state itself, at every moment a squad's membership can change.
func (s *Service) applyRuntimeRaidParties(state runtimeRaidState) error {
	// A single Raid owns a one-character attack-group row, but it is not an
	// ordinary party.  Projecting that row through RuntimePartyManager renders
	// the normal party HUD and leaves the client with ordinary-party authority
	// after returning from a Raid dungeon.
	if state.TailFlag == 1 {
		for _, member := range state.Members {
			session, online := s.onlineGameSession(member.CharID)
			if !online {
				continue
			}
			if err := s.clearSingleRaidOrdinaryPartyProjection(session, "single_raid_start"); err != nil {
				return err
			}
		}
		return nil
	}
	if !state.Started || state.StartPending || len(state.Members) == 0 {
		// Before the ready countdown an attack group is still the leader's plan
		// and a member keeps whatever ordinary party they already had; taking it
		// over there is what used to steal a town party at create time.
		return nil
	}
	projectedBefore := s.runtimeRaidSquadPartyMembership(state)
	assigned := make(map[uint16]bool, len(state.Members))
	next := make(map[byte]uint16, runtimeRaidMaxGroups)
	for _, group := range runtimeRaidPartyGroups(state) {
		if len(group) == 0 {
			continue
		}
		for _, member := range group {
			assigned[member.CharID] = true
		}
		partyID, err := s.materializeRuntimeRaidSquadParty(state, group)
		if err != nil {
			return err
		}
		if partyID != 0 {
			next[group[0].GroupIndex] = partyID
			// The projection leads each squad with its first member
			// (runtimeRaidGroupPartyState), which is also the answer the client's
			// 队长标志 must give for that squad while no party publish has named one.
			s.recordRuntimeRaidSquadPartyLeader(nil, group[0].CharID)
		}
	}
	detached, err := s.detachDepartedRuntimeRaidSquadParties(projectedBefore, assigned)
	if err != nil {
		return err
	}
	s.recordRuntimeRaidSquadParties(state.RaidKey, next)
	// The "one non-zero squad <-> one real ordinary party" invariant is otherwise
	// invisible on the wire: the projection speaks through ordinary party packets.
	s.logGameEvent(nil, "game-raid-squad-parties-projected",
		"raid_key", state.RaidKey,
		"squad_count", len(next),
		"squads", formatRuntimeRaidSquadPartyBindings(next),
		"detached_members", detached)
	return nil
}

// formatRuntimeRaidSquadPartyBindings renders the squad -> party projection table
// as "<squad>:<party_id>" pairs for the packet log.
func formatRuntimeRaidSquadPartyBindings(bindings map[byte]uint16) string {
	if len(bindings) == 0 {
		return ""
	}
	groups := make([]int, 0, len(bindings))
	for group := range bindings {
		groups = append(groups, int(group))
	}
	sort.Ints(groups)
	out := make([]byte, 0, len(groups)*8)
	for index, group := range groups {
		if index > 0 {
			out = append(out, ',')
		}
		out = strconv.AppendInt(out, int64(group), 10)
		out = append(out, ':')
		out = strconv.AppendInt(out, int64(bindings[byte(group)]), 10)
	}
	return string(out)
}

// runtimeRaidSquadPartyBindings copies the squad -> party projection table.
func runtimeRaidSquadPartyBindings(state runtimeRaidState) map[byte]uint16 {
	if len(state.SquadParties) == 0 {
		return nil
	}
	bindings := make(map[byte]uint16, len(state.SquadParties))
	for group, partyID := range state.SquadParties {
		bindings[group] = partyID
	}
	return bindings
}

// recordRuntimeRaidSquadParties stores the projection table on the authoritative
// raid state. The caller must not hold raidMu.
func (s *Service) recordRuntimeRaidSquadParties(raidKey uint32, bindings map[byte]uint16) {
	if s == nil || raidKey == 0 {
		return
	}
	s.raidMu.Lock()
	defer s.raidMu.Unlock()
	state := s.raids[raidKey]
	if state == nil {
		return
	}
	if len(bindings) == 0 {
		state.SquadParties = nil
		return
	}
	state.SquadParties = bindings
}

// runtimeRaidSquadPartyMembership maps every member of a projected squad party to
// that party's id, as recorded before the current reconcile ran.
func (s *Service) runtimeRaidSquadPartyMembership(state runtimeRaidState) map[uint16]uint16 {
	if len(state.SquadParties) == 0 {
		return nil
	}
	manager := s.runtimePartyManagerForService()
	if manager == nil {
		return nil
	}
	membership := make(map[uint16]uint16)
	for _, partyID := range state.SquadParties {
		snapshot, found := manager.SnapshotByID(partyID)
		if !found {
			continue
		}
		for _, member := range snapshot.Members {
			membership[member.UserID] = snapshot.ID
		}
	}
	return membership
}

// materializeRuntimeRaidSquadParty makes one attack squad exist as exactly one
// real ordinary party.
//
// A squad whose recorded party already holds exactly these members, in the same
// slots and on the same live sessions, is only refreshed: op9 -> op153 without an
// op11 re-handshake, because op11 is a formation handshake and replaying it tears
// down a healthy peer link (see sendRuntimePartyRosterLocal). Every other case
// forms a new party, which is the one moment op11 may be sent again.
func (s *Service) materializeRuntimeRaidSquadParty(state runtimeRaidState, group []runtimeRaidMemberState) (uint16, error) {
	if len(group) == 0 {
		return 0, nil
	}
	if projected, current := s.runtimeRaidSquadPartyAssembly(state, group); current {
		if err := s.publishRuntimeRaidSquadParty(state, group, projected, true); err != nil {
			return 0, err
		}
		return uint16(projected.PartyID), nil
	}
	managed, ok := s.replaceManagedRuntimeParty(runtimeRaidGroupPartyState(state, group))
	if !ok {
		return 0, fmt.Errorf("replace managed runtime party for raid %d group %d", state.RaidKey, group[0].GroupIndex)
	}
	if err := s.publishRuntimeRaidSquadParty(state, group, managed, false); err != nil {
		return 0, err
	}
	return uint16(managed.PartyID), nil
}

// publishRuntimeRaidSquadParty puts one squad's party frame on the wire, on that
// squad's OWN session loop.
//
// The formation used to be published inline, so the caller -- the raid leader's
// own session loop at 开始攻坚 -- wrote every squad's frames for every member, one
// after another. Live 2026-09-29 02:24 (16 members / 4 squads): op9 party frames
// left the server at 37, 42, 44, 45, 44, 47 then 102 and 151 per second,每条间隔
// ~20ms、一轮 17~23 个成员，三分钟 1614 条, and the operator's report is exactly
// that shape: "按队伍编制顺序自动组队 不是同时自动组队" plus "团长点开始以后会卡
// 很长一下".
//
// Posting each squad to its leader's loop makes the squads form in PARALLEL (each
// on its own loop) and returns the caller immediately. When the squad leader has
// no loop to post to, the work still runs inline, so nothing is ever dropped.
func (s *Service) publishRuntimeRaidSquadParty(
	state runtimeRaidState,
	group []runtimeRaidMemberState,
	partyState alignedcmd.PartyState,
	refreshOnly bool,
) error {
	if s == nil || len(group) == 0 || partyState.PartyID <= 0 {
		return nil
	}
	publish := func() error {
		if refreshOnly {
			return s.sendManagedRuntimePartyAggregateRefresh(partyState, "raid_squad_projection_refresh")
		}
		return s.sendManagedRuntimePartySnapshots(partyState)
	}
	leader, online := s.onlineGameSession(group[0].CharID)
	if !online || leader == nil || leader.events == nil {
		return publish()
	}
	if err := s.postGameSessionCharacterEvent(
		leader,
		"raid_squad_party_publish",
		group[0].CharID,
		leader.characterGeneration,
		func() error {
			started := time.Now()
			err := publish()
			s.logPacketEvent("game-raid-squad-party-publish",
				"conn_id", leader.connID,
				"raid_key", state.RaidKey,
				"squad", group[0].GroupIndex,
				"party_id", partyState.PartyID,
				"squad_size", len(group),
				"refresh_only", refreshOnly,
				"wall_ms", raidMillis(time.Since(started)),
				"reason", "published_on_the_squad_leaders_own_loop_so_every_squad_forms_in_parallel")
			return err
		},
	); err != nil {
		// The mailbox refused the hand-off: keep the old inline behaviour rather
		// than leaving a squad without its party frame.
		s.logPacketEvent("game-raid-squad-party-publish-inline-fallback",
			"raid_key", state.RaidKey,
			"squad", group[0].GroupIndex,
			"party_id", partyState.PartyID,
			"error", err)
		return publish()
	}
	return nil
}

// runtimeRaidSquadPartyAssembly reports whether the party recorded for this squad
// already is the squad's projection, in which case a refresh carries the roster
// and nothing about the party has to be rebuilt.
func (s *Service) runtimeRaidSquadPartyAssembly(state runtimeRaidState, group []runtimeRaidMemberState) (alignedcmd.PartyState, bool) {
	if len(group) == 0 {
		return alignedcmd.PartyState{}, false
	}
	partyID := state.SquadParties[group[0].GroupIndex]
	if partyID == 0 {
		return alignedcmd.PartyState{}, false
	}
	manager := s.runtimePartyManagerForService()
	if manager == nil {
		return alignedcmd.PartyState{}, false
	}
	snapshot, found := manager.SnapshotByID(partyID)
	if !found || snapshot.Leader != group[0].CharID || len(snapshot.Members) != len(group) {
		return alignedcmd.PartyState{}, false
	}
	for index, squadMember := range group {
		identity, online := s.onlineGameSessionCharacterSnapshot(squadMember.CharID)
		if !online {
			return alignedcmd.PartyState{}, false
		}
		matched := false
		for _, record := range snapshot.Members {
			if record.UserID != squadMember.CharID {
				continue
			}
			// A reconnected member carries a new session generation. The party's
			// own rebind path owns that transition, so the projection is rebuilt
			// rather than refreshed on top of a stale generation.
			if record.SessionGeneration != identity.generation {
				return alignedcmd.PartyState{}, false
			}
			if record.Slot != byte(index) {
				return alignedcmd.PartyState{}, false
			}
			matched = true
			break
		}
		if !matched {
			return alignedcmd.PartyState{}, false
		}
	}
	return snapshot.StateFor(group[0].CharID), true
}

// detachDepartedRuntimeRaidSquadParties removes the projection from every member
// that no longer fights in any squad (moved to standby, kicked, or gone) and was
// holding a party this projection created. A party the character formed itself is
// never touched: only the exact party id recorded for a squad is detached. It
// returns how many members were detached.
func (s *Service) detachDepartedRuntimeRaidSquadParties(projected map[uint16]uint16, assigned map[uint16]bool) (int, error) {
	if len(projected) == 0 {
		return 0, nil
	}
	manager := s.runtimePartyManagerForService()
	if manager == nil {
		return 0, nil
	}
	detached := 0
	charIDs := make([]int, 0, len(projected))
	for charID := range projected {
		charIDs = append(charIDs, int(charID))
	}
	sort.Ints(charIDs)
	for _, raw := range charIDs {
		charID := uint16(raw)
		if assigned[charID] {
			continue
		}
		identity, online := s.onlineGameSessionCharacterSnapshot(charID)
		if !online {
			continue
		}
		snapshot, found := manager.SnapshotByUser(charID, identity.generation)
		if !found || snapshot.ID != projected[charID] {
			continue
		}
		if err := s.detachRuntimeRaidSquadProjection(identity.session, "raid_squad_projection_detach"); err != nil {
			return detached, err
		}
		detached++
	}
	return detached, nil
}

// detachRuntimeRaidSquadProjection drops one character from the ordinary party
// this projection created and tells its client the party is gone, the same way
// clearSingleRaidOrdinaryPartyProjection does for a single raid.
func (s *Service) detachRuntimeRaidSquadProjection(session *gameSession, source string) error {
	if s == nil || session == nil || session.selectedCharacterID == 0 {
		return nil
	}
	previous := runtimePartyStateSnapshot(session)
	s.detachRuntimePartySession(session, source)
	storeRuntimePartyState(session, alignedcmd.PartyState{})
	if previous.PartyID <= 0 {
		return nil
	}
	s.logGameEvent(session, "game-raid-squad-party-detached",
		"source", source,
		"char_id", session.selectedCharacterID,
		"party_id", previous.PartyID,
		"member_count", len(runtimePartyMembers(previous)))
	return s.sendRuntimePartySnapshot(session, alignedcmd.PartyState{})
}

func (s *Service) clearSingleRaidOrdinaryPartyProjection(session *gameSession, source string) error {
	if s == nil || session == nil || session.selectedCharacterID == 0 {
		return nil
	}
	previous := runtimePartyStateSnapshot(session)
	s.detachRuntimePartySession(session, source)
	storeRuntimePartyState(session, alignedcmd.PartyState{})
	if previous.PartyID <= 0 {
		return nil
	}
	s.logGameEvent(session, "game-single-raid-ordinary-party-cleared",
		"source", source,
		"char_id", session.selectedCharacterID,
		"party_id", previous.PartyID,
		"member_count", len(runtimePartyMembers(previous)))
	return s.sendRuntimePartySnapshot(session, alignedcmd.PartyState{})
}

// initialRuntimeRaidMembersLocked seeds a new raid with the creator and whoever is
// in the creator's ordinary party at that moment, all of them in standby.
//
// Nobody is placed in a squad here. A joiner already lands in standby for the same
// reason (see the join path above): the roster record carries the squad byte
// straight from the wire (sub_17B6EF0 stores it at +0x2D with no arithmetic) and
// every squad lookup in the client compares it against 1..N, so 0 matches no squad
// and the member shows as unassigned -- which is what the leader is then expected to
// fix by dragging them into a squad. The reference server draws the same line:
// RaidManager.Create assigns no PartyIndex at all and TryAssignParty is the only
// writer (ServerS4A21-master, RaidManager.cs:40-55 and :86-109), while
// BuildSituationGroups gives a PartyIndex-0 member its own singleton group (:339-344).
//
// Live report 2026-09-17 21:3x: a raid created by one character showed that
// character in squad 1 even though nobody had assigned it.
//
// The caller assigns squad 1 itself where that is what the gesture means: a raid
// created by a real ordinary party opens with that party already projected into one
// squad, and a single raid keeps its own seat.
func (s *Service) initialRuntimeRaidMembersLocked(session *gameSession, sourceID uint16) []runtimeRaidMemberState {
	members := []runtimeRaidMemberState{s.newRuntimeRaidMemberLocked(sourceID, 0, 0)}
	state := runtimePartyStateSnapshot(session)
	for _, member := range runtimePartyMembers(state) {
		if member.UserID == 0 || member.UserID == sourceID || len(members) >= raid.MaxAttackPartyMembers {
			continue
		}
		members = append(members, s.newRuntimeRaidMemberLocked(member.UserID, 0, 0))
	}
	return members
}

// countRuntimeRaidStandbyMembers counts the roster members that belong to no attack
// group, for the creation log line.
func countRuntimeRaidStandbyMembers(members []runtimeRaidMemberState) int {
	standby := 0
	for _, member := range members {
		if runtimeRaidMemberSquad(member.GroupIndex) == 0 {
			standby++
		}
	}
	return standby
}

func (s *Service) newRuntimeRaidMemberLocked(charID uint16, group byte, slot byte) runtimeRaidMemberState {
	profile := s.runtimeRaidCharacterProfile(charID)
	return runtimeRaidMemberState{
		CharID:     charID,
		Name:       profile.Name,
		Level:      profile.Level,
		Job:        profile.Job,
		Grow:       profile.Grow,
		GroupIndex: group,
		SlotOrder:  slot,
		UserState:  1,
		HPPercent:  100,
		MPPercent:  100,
	}
}

type runtimeRaidCharacterProfile struct {
	Name  string
	Level byte
	Job   byte
	Grow  byte
}

func (s *Service) runtimeRaidCharacterProfile(charID uint16) runtimeRaidCharacterProfile {
	profile := runtimeRaidCharacterProfile{Name: csharpSelectCharacterName(charID)}
	repos, ok := s.repositoryGroup()
	if ok && repos.Character != nil {
		ctx, cancel := context.WithTimeout(context.Background(), createWriteTimeout)
		defer cancel()
		if record, found, err := repos.Character.Load(ctx, strconv.Itoa(int(charID))); err == nil && found {
			if record.Name != "" {
				profile.Name = record.Name
			}
			if record.Level > 0 && record.Level <= 255 {
				profile.Level = byte(record.Level)
			}
			if job, valid := characterJobByte(record); valid {
				profile.Job = job
			}
			grow := numericCharacterStatValue(record, "grow_type")
			// The raid record carries the character grow_type byte verbatim:
			// low nibble is the class branch and bits 4..6 are awakening.
			if grow >= 0 && grow <= 0x7f {
				profile.Grow = byte(grow)
			}
		}
	}
	return profile
}

func (s *Service) sendRaidCommandFailure(session *gameSession, typ uint16) error {
	return s.sendGameUpperRawClassCodec(session, typ, []byte{0, runtimeRaidFailureCode}, dnfproto.DefaultChannelClassification, true)
}

func (s *Service) ensureRuntimeRaidMapsLocked() {
	if s.raids == nil {
		s.raids = make(map[uint32]*runtimeRaidState)
	}
}

func (s *Service) nextRuntimeRaidKeyLocked(session *gameSession, sourceID uint16) uint32 {
	channelID := uint32(0)
	if session != nil {
		channelID = uint32(uint16(session.channel.ID))
	}
	base := channelID<<16 | uint32(sourceID)
	if base != 0 {
		if _, exists := s.raids[base]; !exists {
			return base
		}
	}
	for {
		s.nextRaidSeq++
		key := channelID<<16 | uint32(uint16(s.nextRaidSeq))
		if key == 0 {
			continue
		}
		if _, exists := s.raids[key]; !exists {
			return key
		}
	}
}

func (s *Service) runtimeRaidByMemberLocked(charID uint16) *runtimeRaidState {
	if charID == 0 {
		return nil
	}
	for _, state := range s.raids {
		if runtimeRaidMemberIndex(*state, charID) >= 0 {
			return state
		}
	}
	return nil
}

// removeRuntimeRaidMemberLocked removes one character from its raid. Caller holds
// raidMu.
//
// A leave always goes through, whatever the raid is doing: the 86JP reference
// removes the member without consulting the raid state at all
// (RaidManager.LeaveLocked, RaidManager.cs:1259). Refusing once
// StartPending/Started was set left a player stuck inside a raid they could no
// longer leave and unable to create another one, because the client keeps
// answering "you are already in a raid party" while the old object stays.
//
// Departing as the leader hands the raid to the next member in roster order --
// runtimeRaidSuccessorLocked skips anyone sitting in its own disconnect grace
// window, so the raid does not land on somebody who cannot start it. Only a leave
// that empties the roster disbands.
func (s *Service) removeRuntimeRaidMemberLocked(charID uint16) (*runtimeRaidState, bool, bool) {
	state := s.runtimeRaidByMemberLocked(charID)
	if state == nil {
		return nil, false, false
	}
	next := state.Members[:0]
	removed := false
	for _, member := range state.Members {
		if member.CharID == charID {
			removed = true
			continue
		}
		next = append(next, member)
	}
	if !removed {
		return state, false, false
	}
	state.Members = next
	delete(s.raidDisconnects, charID)
	// A leaver cannot complete the run its session owned, so the gate it occupied
	// has to be released with it or the rest of the squad can never re-enter.
	s.releaseRuntimeRaidRunsOwnedByLocked(state, charID)
	// The leaver also leaves the frozen roster: a phase settlement waits until
	// every frozen member confirmed a card, and a member who is gone can never
	// confirm it.
	frozen := state.FrozenMembers[:0]
	for _, member := range state.FrozenMembers {
		if member.CharID == charID {
			continue
		}
		frozen = append(frozen, member)
	}
	state.FrozenMembers = frozen
	if len(state.Members) == 0 {
		delete(s.raids, state.RaidKey)
		// The raid is gone, so its gates' reported runtime values are meaningless:
		// drop them here or every disbanded raid leaves one entry per gate per squad
		// behind for the life of the process.
		s.purgeRuntimeRaidMonsterRuntimeValuesLocked(state.RaidKey)
		s.purgeRuntimeRaidBuffActivationsLocked(state.RaidKey)
		s.purgeRuntimeRaidReviveCoinLedgerLocked(state.RaidKey)
		s.purgeRuntimeRaidPhaseDeathsLocked(state.RaidKey)
		// A disbanding raid leaves no seats behind: every survivor's disconnect
		// reservation has to go too, or a later grace expiry would try to remove a
		// member from a raid that no longer exists.
		for member, reservation := range s.raidDisconnects {
			if reservation.RaidKey == state.RaidKey {
				delete(s.raidDisconnects, member)
			}
		}
		return nil, true, true
	}
	if state.LeaderID == charID || runtimeRaidMemberIndex(*state, state.LeaderID) < 0 {
		// Prefer a survivor whose transport is live; a leader inside its own
		// grace window would leave the raid without anyone able to start it.
		state.LeaderID = s.runtimeRaidSuccessorLocked(*state)
	}
	return state, true, false
}

func runtimeRaidMemberRefresh(state runtimeRaidState) raid.MemberRefresh {
	members := append([]runtimeRaidMemberState(nil), state.Members...)
	runtimeRaidSortMembers(members)
	out := raid.MemberRefresh{RaidKey: state.RaidKey, Members: make([]raid.MemberRecord, 0, len(members))}
	for _, member := range members {
		out.Members = append(out.Members, runtimeRaidMemberRecord(state, member))
	}
	return out
}

func runtimeRaidObjectMode0(state runtimeRaidState) raid.RaidObjectMode0 {
	members := append([]runtimeRaidMemberState(nil), state.Members...)
	runtimeRaidSortMembers(members)
	records := make([]raid.MemberRecord, 0, len(members))
	var leader raid.MemberRecord
	for _, member := range members {
		record := runtimeRaidMemberRecord(state, member)
		records = append(records, record)
		if member.CharID == state.LeaderID {
			leader = record
		}
	}
	if leader.CharKey == 0 && len(records) > 0 {
		leader = records[0]
	}
	objectState, objectPhase := runtimeRaidObjectLifecycleFields(state)
	return raid.RaidObjectMode0{
		RaidKey:       state.RaidKey,
		TitleBytes:    runtimeRaidTitleBytes(state.NameBytes),
		ObjectField24: state.RouteOrRaidType,
		ObjectField28: objectState,
		ObjectField30: objectPhase,
		ObjectField34: 0,
		ObjectField94: state.TailFlag,
		Leader:        leader,
		Members:       records,
	}
}

// runtimeRaidObjectLifecycleFields projects the server lifecycle into the
// object fields read by current EXE sub_17B8910. In particular, selector
// refreshes must retain state=2 and the zero-based phase index; resetting an
// active object to state=0/phase=0xff makes sub_F176A0 clear every map row.
func runtimeRaidObjectLifecycleFields(state runtimeRaidState) (byte, byte) {
	phase := byte(0xff)
	if state.Lifecycle.Phase > 0 {
		phase = state.Lifecycle.Phase - 1
	}
	switch state.Lifecycle.Status {
	case runtimeRaidLifecyclePhaseActive:
		return runtimeRaidPhaseStartState, phase
	case runtimeRaidLifecycleAwaitingMovie:
		return runtimeRaidSettlementStateResult, phase
	case runtimeRaidLifecycleAwaitingRewards, runtimeRaidLifecycleStandby:
		return runtimeRaidSettlementStateCardsOpened, phase
	default:
		return 0, 0xff
	}
}

func runtimeRaidMemberRecord(state runtimeRaidState, member runtimeRaidMemberState) raid.MemberRecord {
	return raid.MemberRecord{
		CharKey:      member.CharID,
		Field4:       member.UserState,
		Name:         member.Name,
		RawNameBytes: rosterNameBytes(member.Name),
		// Current EXE sub_1B4ED60 passes Field40 as the first (job) argument
		// to sub_249D7F0 and splits Field44 into grow/awakening nibbles.
		Field40:    member.Job,
		Field44:    member.Grow,
		GroupIndex: member.GroupIndex,
		SlotOrder:  member.SlotOrder,
		// Wire field 56 is the current client's per-member phase clear count. The
		// 攻击队 status window renders the 地下城通关 cell as
		// "<this byte>/<phase requirement>" (sub_1B4FA0E reads the record's byte at
		// +0x38, where sub_17B6EF0 stores this field), and the phase settlement
		// only rewards members the client can prove cleared a gate. Sending 0 left
		// the column at 0/1 forever and the phase end flipped no cards.
		Field56: runtimeRaidPhaseClearCount(state, member.CharID),
		// Wire field 66 is the client's answer to "is this member the leader?".
		// sub_17B6EF0 reads it as a u32 and stores `value == 1` as a BOOL at
		// member+0x42; the client then asks that byte about its OWN record
		// (0x51b2748 -> my key -> 0x13b29c0 -> cmp byte ptr [record+42h], 0) for
		// every leader-gated raid control, the 战况表 进入地下城/回到城镇 buttons
		// included. It therefore names the leader the player can act as -- see
		// runtimeRaidMemberLeadsSquad.
		Field66BoolSource: boolUint32(runtimeRaidMemberLeadsSquad(state, member)),
	}
}

// runtimeRaidMemberLeadsSquad answers the client's own question "am I the leader?"
// for one roster member.
//
// Operator report 2026-09-26 (raid 1507329, party 5 = [1 2 20 21], all in squad 1):
// char 1 -- the 团长 AND the squad's party leader -- pressed 返回城镇 alone, the party
// handed the leadership to char 20, and the client kept saying char 1 was the leader:
// the promoted member had no 队长标志 and no 战况表 buttons, while the member who had
// already left the party AND the dungeon still had both. The roster that caused it
// (decoded from packet_log 15:03:14.782) said char 1 group 0 / leader 1 and char 20
// group 1 slot 1 / leader 0, i.e. the flag was filled with "is the 团长", and the
// 团长 does not change hands when the squad's leadership does.
//
// So inside a running multi-member raid the flag is the SQUAD's leader:
//
//   - the ordinary party the squad fights in, mirrored by
//     recordRuntimeRaidSquadPartyLeader on every party publish (a leadership handover
//     happens while both members are inside the gate, which is exactly where the
//     squad-sync's dungeon-run guard would have skipped a relabel);
//   - otherwise the squad's lowest occupied slot, which is the member
//     runtimeRaidGroupPartyState would lead that squad's projected party with.
//
// Everything else keeps the old meaning on purpose: before the raid starts there are
// no squads and no ordinary parties to lead, and a single raid never owns one, so both
// keep answering exactly what they always did.
func runtimeRaidMemberLeadsSquad(state runtimeRaidState, member runtimeRaidMemberState) bool {
	if !state.Started || state.TailFlag == 1 {
		return member.CharID == state.LeaderID
	}
	squad := runtimeRaidMemberSquad(member.GroupIndex)
	if squad == 0 {
		return false
	}
	if leader := state.SquadPartyLeaders[squad]; leader != 0 {
		if index := runtimeRaidMemberIndex(state, leader); index >= 0 &&
			runtimeRaidMemberSquad(state.Members[index].GroupIndex) == squad {
			return member.CharID == leader
		}
	}
	return member.CharID == runtimeRaidLowestSlotMemberInSquad(state, squad)
}

// runtimeRaidLowestSlotMemberInSquad is the member a squad's projected ordinary party
// is led by when nothing else names one: runtimeRaidGroupPartyState takes the first
// member of the squad's slot order, and runtimeRaidPartyGroups hands it exactly that.
func runtimeRaidLowestSlotMemberInSquad(state runtimeRaidState, squad byte) uint16 {
	leader := uint16(0)
	slot := byte(0xff)
	for _, candidate := range state.Members {
		if candidate.CharID == 0 || runtimeRaidMemberSquad(candidate.GroupIndex) != squad {
			continue
		}
		if leader == 0 || candidate.SlotOrder < slot {
			leader, slot = candidate.CharID, candidate.SlotOrder
		}
	}
	return leader
}

// recordRuntimeRaidSquadPartyLeader mirrors the ordinary party that was just published
// into the raid squad its leader currently occupies.
//
// Called for every party snapshot, deliberately BEFORE the dungeon-run guard of the
// squad sync below: that guard protects the squad RELABELING (an ordinary party is not
// a squad gesture while its members are inside a run), while this is bookkeeping about
// who leads the party they are already in -- and the handover it has to survive happens
// mid-run by definition.
func (s *Service) recordRuntimeRaidSquadPartyLeader(memberIDs []uint16, leaderID uint16) {
	if s == nil || leaderID == 0 {
		return
	}
	s.raidMu.Lock()
	defer s.raidMu.Unlock()
	state := s.runtimeRaidByMemberLocked(leaderID)
	if state == nil {
		return
	}
	index := runtimeRaidMemberIndex(*state, leaderID)
	if index < 0 {
		return
	}
	squad := runtimeRaidMemberSquad(state.Members[index].GroupIndex)
	if squad == 0 {
		return
	}
	if state.SquadPartyLeaders == nil {
		state.SquadPartyLeaders = make(map[byte]uint16, runtimeRaidMaxGroups)
	}
	state.SquadPartyLeaders[squad] = leaderID
}

// runtimeRaidTitleBytes passes the raid name to the object builder exactly as the
// client sent it.
//
// This used to re-encode the name to UTF-16LE. That was wrong: the client reads
// the object title through the same length-prefixed narrow-string reader as every
// other dstr (sub_3457D20 -> 0x34D9CA0 widens it byte by byte from the local code
// page), so UTF-16LE bytes were rendered as local-code-page characters. The
// symptom is name-shaped, not random: a digits-only name stops at the first NUL,
// so "1111" showed as "1", and "礼白1" showed as "<y}v1" because
// 3C 79 7D 76 31 is the little-endian spelling of 礼白1.
//
// The 86 reference server does the same thing -- RaidManager.Create stores the
// request's own TitleBytes and RaidPacketBuilder.WriteRaidObject emits them with
// GamePacketWriter.WriteRawDstr. Its RaidHandler self-test pins a captured
// CREATE_RAID body to the narrow ASCII "123" (31 32 33), not 31 00 32 00 33 00,
// which is what settles that no conversion belongs here.
//
// The stored bytes are the client's own, so no migration is needed: 664/670
// bodies always carried GB18030, and only this outgoing hop ever re-encoded.
func runtimeRaidTitleBytes(raw []byte) []byte {
	return append([]byte(nil), raw...)
}

// runtimeRaidPartyGroups groups the roster by squad for the ordinary-party
// projection, the Buff registration snapshot and the rejoin recovery.
//
// Unassigned members (group 0) are skipped rather than folded into squad 1. A
// standby member is in no attack group, so projecting one gives the client a
// squad party the raid roster simultaneously shows as empty. The dungeon-phase
// binding reads runtimeRaidNormalizedGroup instead and still treats an
// unassigned member as squad 1, so skipping them here does not make anyone
// ineligible to enter.
func runtimeRaidPartyGroups(state runtimeRaidState) [][]runtimeRaidMemberState {
	members := append([]runtimeRaidMemberState(nil), state.Members...)
	runtimeRaidSortMembers(members)
	groups := make([][]runtimeRaidMemberState, 0, runtimeRaidMaxGroups)
	byGroup := make(map[byte][]runtimeRaidMemberState)
	for _, member := range members {
		group := member.GroupIndex
		if group < 1 || group > runtimeRaidMaxGroups {
			continue
		}
		member.GroupIndex = group
		byGroup[group] = append(byGroup[group], member)
	}
	keys := make([]int, 0, len(byGroup))
	for group := range byGroup {
		keys = append(keys, int(group))
	}
	sort.Ints(keys)
	for _, key := range keys {
		groups = append(groups, byGroup[byte(key)])
	}
	return groups
}

// runtimeRaidClientBuffSlots is the 90 client's raid-buff slot order.
//
// The reader sub_1D50660 (registration 0x01D517DC: push 0x1D50660 / push 0x279 /
// call 0x2262980) takes `u8 groupCount { u8 key, u8 rowCount, rows... }` where the
// key byte is used directly as the slot number: the icon refresh sub_15C6AD0
// walks a fixed five slots (`0x015C6C13 cmp esi,5 / 0x015C6C16 jl 0x15C6B50`) and
// looks each one up in the table the packet wrote (`0x017B57A3 cmp dword ptr
// [eax+0xC],edx` matches the key). Slot meanings come from the two dispatch
// tables at 0x160EDD0 / 0x160EBC0: 0 = 攻略时间增加 (+5 min), 1 = 攻击力增加 (30s),
// 2 = 无敌 (30s), 3 = 恢复 (60s), 4 = 攻击Buff.
//
// 86JP's AntonClientBuffTypes happens to be the same five values, but this list is
// the 90 client's own: see tools/_probe_279_report.md.
var runtimeRaidClientBuffSlots = [...]byte{2, 3, 4, 1, 0}

// runtimeRaidBuffIdleStatusBody is the 0x0279 body of a raid with no running
// skill: the five slots, each with no rows.
//
// Production sends the live status instead (Service.runtimeRaidBuffStatusBody),
// which is byte-identical to this whenever nothing is active. That is because an
// idle slot is a zero-row group, and a zero-row group is inert: the client's lookup
// skips the key entirely (`0x017B57E2 cmp esi,[ebp-0x28]`) and ends in the clean
// false return at 0x017B5869. So a row means "this buff is running" -- its third
// u32 is an absolute deadline that sub_17B5730 compares against the current seconds
// (`0x017B5800 mov ecx,[esi+8]` / `0x017B5806 sub ecx,eax` / `0x017B5812 jns ...`),
// and a value <= now is treated as expired.
//
// This entry point is kept as the idle reference the packet tests compare against,
// so those tests keep meaning "an untouched raid looks like this".
//
// The body this replaced attached one row per member with both deadlines zero,
// which is also inert by that same check -- but it used the group byte as a party
// index, so slot 1 (攻击力增加) was the only key present and the other four slots
// never existed for the client.
func runtimeRaidBuffIdleStatusBody() ([]byte, error) {
	groups := make([]raid.RaidBuffGroup, 0, len(runtimeRaidClientBuffSlots))
	for _, slot := range runtimeRaidClientBuffSlots {
		groups = append(groups, raid.RaidBuffGroup{Field0: slot})
	}
	return raid.BuildRaidBuffBody(groups)
}

func runtimeRaidHasOversizedGroup(state runtimeRaidState) bool {
	counts := make(map[byte]int)
	for _, member := range state.Members {
		group := member.GroupIndex
		if group < 1 || group > runtimeRaidMaxGroups {
			continue
		}
		counts[group]++
		if counts[group] > runtimeRaidGroupSize {
			return true
		}
	}
	return false
}

// runtimeRaidGroupPartyState projects one attack squad onto a native party state.
//
// Slots are explicit: the group is already in roster order (runtimeRaidSortMembers
// orders by squad, then slot, then character), and both op9 and op153 carry that
// slot, so leaving it implicit let the party table's own fill-in order decide which
// body a member was painted into.
func runtimeRaidGroupPartyState(state runtimeRaidState, members []runtimeRaidMemberState) alignedcmd.PartyState {
	leaderID := members[0].CharID
	partyState := alignedcmd.PartyState{
		PartyID:    int(state.RaidKey&0xffff) + int(members[0].GroupIndex)*1000,
		IsLeader:   true,
		UserID:     leaderID,
		UserState:  1,
		NameBytes:  append([]byte(nil), state.NameBytes...),
		MaxMembers: runtimeRaidGroupSize,
		Members:    make([]alignedcmd.PartyMemberState, 0, len(members)),
	}
	if len(partyState.NameBytes) == 0 {
		partyState.NameBytes = []byte("raid")
	}
	for index, member := range members {
		partyState.Members = append(partyState.Members, alignedcmd.PartyMemberState{
			UserID:    member.CharID,
			Slot:      byte(index),
			UserState: firstByte(member.UserState, 1),
			HPPercent: firstByte(member.HPPercent, 100),
			MPPercent: firstByte(member.MPPercent, 100),
		})
	}
	return partyState
}

func runtimeRaidMemberIndex(state runtimeRaidState, charID uint16) int {
	for i, member := range state.Members {
		if member.CharID == charID {
			return i
		}
	}
	return -1
}

func runtimeRaidMemberIDs(state runtimeRaidState) []uint16 {
	ids := make([]uint16, 0, len(state.Members))
	for _, member := range state.Members {
		ids = append(ids, member.CharID)
	}
	sort.Slice(ids, func(i, j int) bool { return ids[i] < ids[j] })
	return ids
}

func sameRuntimeRaidMembers(members []runtimeRaidMemberState, ids []uint16) bool {
	left := runtimeRaidMemberIDs(runtimeRaidState{Members: members})
	right := append([]uint16(nil), ids...)
	sort.Slice(right, func(i, j int) bool { return right[i] < right[j] })
	if len(left) != len(right) {
		return false
	}
	for index := range left {
		if left[index] != right[index] {
			return false
		}
	}
	return true
}

func nextRuntimeRaidSlot(state runtimeRaidState, group byte) byte {
	return nextRuntimeRaidSlotExcluding(state, group, 0)
}

func nextRuntimeRaidSlotExcluding(state runtimeRaidState, group byte, exclude uint16) byte {
	used := make(map[byte]bool)
	for _, member := range state.Members {
		if member.CharID == exclude || member.GroupIndex != group {
			continue
		}
		used[member.SlotOrder] = true
	}
	for i := byte(0); i < runtimeRaidGroupSize; i++ {
		if !used[i] {
			return i
		}
	}
	return runtimeRaidGroupSize - 1
}

func runtimeRaidSortMembers(members []runtimeRaidMemberState) {
	sort.SliceStable(members, func(i, j int) bool {
		leftGroup := raidSortGroup(members[i].GroupIndex)
		rightGroup := raidSortGroup(members[j].GroupIndex)
		if leftGroup != rightGroup {
			return leftGroup < rightGroup
		}
		if members[i].SlotOrder != members[j].SlotOrder {
			return members[i].SlotOrder < members[j].SlotOrder
		}
		return members[i].CharID < members[j].CharID
	})
}

func raidSortGroup(group byte) int {
	if group == 0 {
		return 1000
	}
	return int(group)
}

func cloneRuntimeRaidState(state runtimeRaidState) runtimeRaidState {
	state.NameBytes = append([]byte(nil), state.NameBytes...)
	state.Members = append([]runtimeRaidMemberState(nil), state.Members...)
	state.ChargedMembers = append([]uint16(nil), state.ChargedMembers...)
	state.FrozenMembers = append([]runtimeRaidMemberState(nil), state.FrozenMembers...)
	state.PendingCostSync = cloneRuntimeRaidEntryCostUpdates(state.PendingCostSync)
	state.SquadParties = runtimeRaidSquadPartyBindings(state)
	state.Lifecycle = cloneRuntimeRaidLifecycleState(state.Lifecycle)
	return state
}

func cloneRuntimeRaidEntryCostUpdates(source []runtimeRaidEntryCostUpdate) []runtimeRaidEntryCostUpdate {
	out := make([]runtimeRaidEntryCostUpdate, len(source))
	for updateIndex, update := range source {
		out[updateIndex].CharacterID = update.CharacterID
		out[updateIndex].Entries = make([]currentItemListEntry, len(update.Entries))
		for entryIndex, entry := range update.Entries {
			out[updateIndex].Entries[entryIndex] = entry
			out[updateIndex].Entries[entryIndex].avatarSocketData = append([]byte(nil), entry.avatarSocketData...)
			out[updateIndex].Entries[entryIndex].avatarColorData = append([]byte(nil), entry.avatarColorData...)
		}
	}
	return out
}

func boolUint32(value bool) uint32 {
	if value {
		return 1
	}
	return 0
}

func firstByte(value byte, fallback byte) byte {
	if value != 0 {
		return value
	}
	return fallback
}
