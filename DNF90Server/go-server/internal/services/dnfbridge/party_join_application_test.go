package dnfbridge

import (
	"encoding/binary"
	"testing"

	"longheng.io/server/internal/modules/dnf/dnfenum"
)

// 现场 2026-09-26 13:23:14–13:23:29（packet_log）：
//
//	13:23:12  char 20 接受 char 1 的邀请            → party 4 = [1, 20]
//	13:23:15  char 2 点 char 1，服务端答
//	          target_party_id=4 party_marker=0000   → 菜单只有「加入」
//	13:23:16  char 2 发 op10 mode 0（与「邀请组队」同一串字节）
//	13:23:17  char 1 点「是」
//	          game-party-disbanded-by-single-member-left party_id=4 removed_id=1
//	          → char 1 被拔进 char 2 的新队，party 4 解散，char 20 被丢下
//
// 正确结果是 char 2 加入 party 4（3 人、队长仍是 char 1），B 留在队里。方向只能
// 在收到请求时按「被点的目标有没有活跃队伍」定死 —— 那正是让客户端画出「加入」
// 的那个判据；到了 op11 再按包体猜是猜不出来的，两种请求的字节完全一样。

// peerSelectionMode0RequestBody 是 op10 普通组队请求的线上形状：
// u16 target + u8 mode + u32 value0 + u16 value1 + u32 value2。
func peerSelectionMode0RequestBody(targetID uint16) []byte {
	body := make([]byte, 13)
	binary.LittleEndian.PutUint16(body[0:2], targetID)
	body[2] = 0
	return body
}

// peerResponseAcceptBody 是 op11「同意」的七字节形状（点「否」会多一个 u16 原因码）。
func peerResponseAcceptBody(targetID uint16) []byte {
	body := make([]byte, 7)
	binary.LittleEndian.PutUint16(body[0:2], targetID)
	return body
}

func TestTownPeerJoinApplicationAddsApplicantToTheExistingParty(t *testing.T) {
	service, leader, leftover, outsider := newPeerSelectionGhostPartyFixture(t)
	partyID := runtimePartyStateSnapshot(leader).PartyID
	if partyID == 0 {
		t.Fatal("fixture party has no id")
	}
	for host, session := range map[byte]*gameSession{1: leader, 2: leftover, 3: outsider} {
		registerPartyPeerEndpointForTest(session, host, uint16(5000)+uint16(host))
	}

	// 前置：被点的 char 1 有活跃队伍，菜单因此只出「加入」。
	if marker, _ := clickTownPeerForSelection(t, service, outsider, leader.selectedCharacterID); marker != 0 {
		t.Fatalf("an owned party answered marker=0x%04x, want 0x0000", marker)
	}

	// char 2 点「加入」：op10 mode 0 落到 char 1 头上。
	outsider.conn.(*bufferConn).write.Reset()
	if err := service.handleOnlineRequestPeer(
		outsider,
		uint16(dnfenum.CmdPacketRequestPeer),
		peerSelectionMode0RequestBody(leader.selectedCharacterID),
	); err != nil {
		t.Fatalf("join application request: %v", err)
	}
	// 申请人自己只拿到 class1 ACK；提示框发给被申请的队长。
	prompt, ok := runtimeRaidPeerFind(runtimeRaidPeerTake(t, leader), 0x0007)
	if !ok || len(prompt.Body) < 3 ||
		binary.LittleEndian.Uint16(prompt.Body[0:2]) != outsider.selectedCharacterID ||
		prompt.Body[2] != 0 {
		t.Fatalf("owner prompt = % X (found=%v), want requester=%d mode=0",
			prompt.Body, ok, outsider.selectedCharacterID)
	}

	// char 1 点「是」。
	leader.conn.(*bufferConn).write.Reset()
	outsider.conn.(*bufferConn).write.Reset()
	if err := service.handleOnlineResponsePeer(
		leader,
		uint16(dnfenum.CmdPacketResponsePeer),
		peerResponseAcceptBody(outsider.selectedCharacterID),
	); err != nil {
		t.Fatalf("join application acceptance: %v", err)
	}

	// 队伍没有重建：同一个 party id、同一个队长，只是多了一个人。
	state := runtimePartyStateSnapshot(leader)
	if state.PartyID != partyID {
		t.Fatalf("party id = %d, want the applied-to party %d", state.PartyID, partyID)
	}
	if state.UserID != leader.selectedCharacterID || !state.IsLeader {
		t.Fatalf("leadership moved: leader=%d is_leader=%v, want %d",
			state.UserID, state.IsLeader, leader.selectedCharacterID)
	}
	members := runtimePartyMembers(state)
	if len(members) != 3 {
		t.Fatalf("party has %d members, want 3: %+v", len(members), members)
	}
	for _, want := range []uint16{leader.selectedCharacterID, leftover.selectedCharacterID, outsider.selectedCharacterID} {
		if !containsRuntimePartyMember(members, want) {
			t.Fatalf("char %d is missing from the party: %+v", want, members)
		}
	}

	// 被留下的队友 char 20 必须还在队里 —— 这正是本次事故丢掉的那个人。
	survivor := runtimePartyStateSnapshot(leftover)
	if survivor.PartyID != partyID || len(runtimePartyMembers(survivor)) != 3 {
		t.Fatalf("survivor party = %+v, want party %d with 3 members", survivor, partyID)
	}

	// 申请人拿到的投影是「别人的队」，不是自己新建的队。
	applicant := runtimePartyStateSnapshot(outsider)
	if applicant.PartyID != partyID || applicant.UserID != leader.selectedCharacterID || applicant.IsLeader {
		t.Fatalf("applicant projection = %+v, want a follower row in party %d", applicant, partyID)
	}

	// 权威图与三个客户端投影一致，且没有留下任何第二支队伍。
	snapshot, found := service.runtimePartyManagerForService().SnapshotByID(uint16(partyID))
	if !found || snapshot.Leader != leader.selectedCharacterID || len(snapshot.Members) != 3 {
		t.Fatalf("authoritative party = %+v found=%v", snapshot, found)
	}
	if parties := service.runtimePartyManagerForService().Snapshots(); len(parties) != 1 {
		t.Fatalf("manager holds %d parties, want exactly the applied-to one: %+v", len(parties), parties)
	}

	// 三条队伍帧都按三行发出（申请人这一侧第一包就是 op11 端点表）。
	if rest := assertRuntimePartySnapshot(t, outsider.conn.(*bufferConn).write.Bytes(), 3); len(rest) == 0 {
		t.Fatal("applicant received no peer/party frame beyond the roster rows")
	}
}
