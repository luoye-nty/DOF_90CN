// 本文件验证攻坚队包体解析、0x24F 成员刷新和 889 检查结果构造。
package raid

import (
	"context"
	"encoding/binary"
	"reflect"
	"strings"
	"testing"

	"longheng.io/server/internal/modules/dnf/alignedcmd"
	"longheng.io/server/internal/modules/dnf/dnfenum"
)

func TestDecodeManagerWorkRequest(t *testing.T) {
	body := make([]byte, 12)
	binary.LittleEndian.PutUint32(body[0:4], 0)
	binary.LittleEndian.PutUint32(body[4:8], 1001)
	binary.LittleEndian.PutUint32(body[8:12], 3)

	got, err := DecodeManagerWorkRequest(body)
	if err != nil {
		t.Fatalf("DecodeManagerWorkRequest error = %v", err)
	}
	if got.ActionOrMode != 0 || got.MemberCharKey != 1001 || got.TargetGroup != 3 {
		t.Fatalf("manager work = %+v, want action=0 member=1001 group=3", got)
	}
}

func TestBuildCreateAndLeaveRaidResultBodies(t *testing.T) {
	if got, want := BuildCreateRaidResultBody(0x12345678), []byte{1, 0x78, 0x56, 0x34, 0x12}; !reflect.DeepEqual(got, want) {
		t.Fatalf("create raid result body = % X, want % X", got, want)
	}
	for _, tc := range []struct {
		name      string
		raidEnded bool
		want      []byte
	}{
		{name: "member left", raidEnded: false, want: []byte{1, 0}},
		{name: "raid ended", raidEnded: true, want: []byte{1, 1}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			if got := BuildLeaveRaidResultBody(tc.raidEnded); !reflect.DeepEqual(got, tc.want) {
				t.Fatalf("leave raid result body = % X, want % X", got, tc.want)
			}
		})
	}
}

func TestCurrentRaidNotificationIDs(t *testing.T) {
	if RaidObjectMsgID != 0x024f || RaidMemberRefreshMsgID != 0x024f {
		t.Fatalf("raid object/roster msg ids = %#04x/%#04x, want 0x024f", RaidObjectMsgID, RaidMemberRefreshMsgID)
	}
	if RaidTimerMsgID != 0x0254 || RaidRemainTimeMsgID != 0x0255 {
		t.Fatalf("raid timer msg ids = %#04x/%#04x, want 0x0254/0x0255", RaidTimerMsgID, RaidRemainTimeMsgID)
	}
	if RaidWaitingChangeMsgID != 0x0251 || RaidPrepareStartMsgID != 0x0252 {
		t.Fatalf("raid waiting/prepare msg ids = %#04x/%#04x, want 0x0251/0x0252", RaidWaitingChangeMsgID, RaidPrepareStartMsgID)
	}
}

func TestBuildCurrentRaidCreationBodies(t *testing.T) {
	member := MemberRecord{
		CharKey:           0x1234,
		Field4:            1,
		RawNameUTF16LE:    []byte{'L', 0},
		Field40:           2,
		Field44:           3,
		GroupIndex:        1,
		SlotOrder:         0,
		Field48:           9,
		Field52:           4,
		Field53:           5,
		Field56:           6,
		Field60:           10,
		Field64:           11,
		Field66BoolSource: 1,
	}
	got, err := BuildRaidObjectMode0Body(RaidObjectMode0{
		RaidKey:       0x11223344,
		TitleBytes:    []byte{'R', '1'},
		ObjectField24: 3,
		ObjectField28: 0,
		ObjectField30: 0xff,
		ObjectField34: 5,
		ObjectField94: 1,
		Leader:        member,
		Members:       []MemberRecord{member},
	})
	if err != nil {
		t.Fatalf("BuildRaidObjectMode0Body error = %v", err)
	}
	wantMember := []byte{
		0x34, 0x12, 1,
		2, 0, 0, 0, 'L', 0,
		2, 3, 1, 0,
		9, 0, 0, 0,
		4, 5, 6,
		10, 0, 0, 0,
		11, 0,
		1, 0, 0, 0,
	}
	want := []byte{
		0x44, 0x33, 0x22, 0x11,
		0, 0, 0, 0,
		0x44, 0x33, 0x22, 0x11,
		2, 0, 0, 0, 'R', '1',
		3, 0, 0xff,
		5, 0, 0, 0,
		1,
	}
	want = append(want, wantMember...)
	want = append(want, 1)
	want = append(want, wantMember...)
	if !reflect.DeepEqual(got, want) {
		t.Fatalf("mode0 raid object body = % X, want % X", got, want)
	}
	if got := BuildEmptyRaidBuffBody(); !reflect.DeepEqual(got, []byte{0}) {
		t.Fatalf("empty buff body = % X, want 00", got)
	}
}

func TestBuildCurrentRaidStateNotificationBodies(t *testing.T) {
	symbols, err := BuildRaidSymbolBody([]RaidSymbolRow{
		{Key: 0x11223344, Value: 0x55667788},
		{Key: 0x01020304, Value: 0xaabbccdd},
	})
	if err != nil {
		t.Fatalf("BuildRaidSymbolBody error = %v", err)
	}
	wantSymbols := []byte{
		2,
		0x44, 0x33, 0x22, 0x11, 0x88, 0x77, 0x66, 0x55,
		0x04, 0x03, 0x02, 0x01, 0xdd, 0xcc, 0xbb, 0xaa,
	}
	if !reflect.DeepEqual(symbols, wantSymbols) {
		t.Fatalf("raid symbol body = % X, want % X", symbols, wantSymbols)
	}

	participation, err := BuildRaidParticipationBody([]RaidParticipationRow{
		{DungeonID: 0x11223344, Op: 5, MemberKeys: []uint32{0x1234, 0xabcd}},
		{DungeonID: 0x01020304},
	})
	if err != nil {
		t.Fatalf("BuildRaidParticipationBody error = %v", err)
	}
	wantParticipation := []byte{
		2,
		0x44, 0x33, 0x22, 0x11, 5, 2, 0x34, 0x12, 0xcd, 0xab,
		0x04, 0x03, 0x02, 0x01, 0, 0,
	}
	if !reflect.DeepEqual(participation, wantParticipation) {
		t.Fatalf("raid participation body = % X, want % X", participation, wantParticipation)
	}

	dungeonState, err := BuildRaidDungeonStateBody(RaidDungeonStateSnapshot{
		Unknown0: 7,
		Rows: []RaidDungeonStateRow{
			{DungeonID: 0x11223344, State: 1},
			{DungeonID: 0x01020304, State: 9},
		},
		TrailingValue: 0xaabbccdd,
	})
	if err != nil {
		t.Fatalf("BuildRaidDungeonStateBody error = %v", err)
	}
	wantDungeonState := []byte{
		7, 2,
		0x44, 0x33, 0x22, 0x11, 1,
		0x04, 0x03, 0x02, 0x01, 9,
		0xdd, 0xcc, 0xbb, 0xaa,
	}
	if !reflect.DeepEqual(dungeonState, wantDungeonState) {
		t.Fatalf("raid dungeon state body = % X, want % X", dungeonState, wantDungeonState)
	}

	if got, want := BuildRaidDungeonChangeBody(RaidDungeonChange{
		DungeonID: 0x11223344,
		State:     3,
		Argument:  0x80,
	}), []byte{0x44, 0x33, 0x22, 0x11, 3, 0x80}; !reflect.DeepEqual(got, want) {
		t.Fatalf("raid dungeon change body = % X, want % X", got, want)
	}
	if got, want := BuildRaidStateBody(RaidState{State: 9, Argument: -2}), []byte{9, 0xfe}; !reflect.DeepEqual(got, want) {
		t.Fatalf("raid state body = % X, want % X", got, want)
	}
}

func TestBuildCurrentRaidTimerMovieAndResultBodies(t *testing.T) {
	if got, want := BuildRaidTimerBody(RaidTimer{
		TimerType: 2,
		Value0:    0x11223344,
		Value1:    0x55667788,
		Value2:    0xaabbccdd,
	}), []byte{
		2,
		0x44, 0x33, 0x22, 0x11,
		0x88, 0x77, 0x66, 0x55,
		0xdd, 0xcc, 0xbb, 0xaa,
	}; !reflect.DeepEqual(got, want) {
		t.Fatalf("raid timer body = % X, want % X", got, want)
	}
	if got, want := BuildRaidRemainTimeBody(RaidRemainTime{
		TimerType: 3,
		Seconds:   0x01020304,
	}), []byte{3, 0x04, 0x03, 0x02, 0x01}; !reflect.DeepEqual(got, want) {
		t.Fatalf("raid remain-time body = % X, want % X", got, want)
	}
	if got, want := BuildRaidMovieBody(), []byte{}; !reflect.DeepEqual(got, want) {
		t.Fatalf("raid movie body = % X, want empty", got)
	}
	if got, want := BuildRaidResultBody(RaidResult{
		ResultType: 2,
		Field1:     3,
		Field2:     0x11223344,
		Field3:     0x5566,
		Field4:     7,
		Field5:     8,
	}), []byte{2, 3, 0x44, 0x33, 0x22, 0x11, 0x66, 0x55, 7, 8}; !reflect.DeepEqual(got, want) {
		t.Fatalf("raid result body = % X, want % X", got, want)
	}
}

func TestBuildCurrentRaidRewardAndBuffBodies(t *testing.T) {
	rewards, err := BuildRaidRewardListBody(RaidRewardList{
		ResultType: 1,
		Groups: []RaidRewardGroup{
			{
				MemberKey: 0x1234,
				Field0:    5,
				Entries: []RaidRewardEntry{
					{Field1: 7, ItemOrValue: 0x11223344, CountOrValue: 0x5566},
					{Field1: 8, ItemOrValue: 0xaabbccdd, CountOrValue: 0xeeff},
				},
			},
			{MemberKey: 0xabcd},
		},
	})
	if err != nil {
		t.Fatalf("BuildRaidRewardListBody error = %v", err)
	}
	wantRewards := []byte{
		1, 2,
		0x34, 0x12, 5, 2,
		7, 0x44, 0x33, 0x22, 0x11, 0x66, 0x55,
		8, 0xdd, 0xcc, 0xbb, 0xaa, 0xff, 0xee,
		0xcd, 0xab, 0, 0,
	}
	if !reflect.DeepEqual(rewards, wantRewards) {
		t.Fatalf("raid reward-list body = % X, want % X", rewards, wantRewards)
	}

	buffs, err := BuildRaidBuffBody([]RaidBuffGroup{
		{
			Field0: 1,
			Entries: []RaidBuffEntry{
				{
					PartyIndex: 0x0203,
					MemberKey:  0x1234,
					Value0:     0x11223344,
					Value1:     0x55667788,
				},
			},
		},
		{Field0: 9},
	})
	if err != nil {
		t.Fatalf("BuildRaidBuffBody error = %v", err)
	}
	wantBuffs := []byte{
		2,
		1, 1,
		0x03, 0x02, 0x34, 0x12,
		0x44, 0x33, 0x22, 0x11,
		0x88, 0x77, 0x66, 0x55,
		9, 0,
	}
	if !reflect.DeepEqual(buffs, wantBuffs) {
		t.Fatalf("raid buff body = % X, want % X", buffs, wantBuffs)
	}
	emptyBuffs, err := BuildRaidBuffBody(nil)
	if err != nil {
		t.Fatalf("BuildRaidBuffBody empty error = %v", err)
	}
	if got, want := emptyBuffs, BuildEmptyRaidBuffBody(); !reflect.DeepEqual(got, want) {
		t.Fatalf("empty raid buff body = % X, want % X", got, want)
	}
}

func TestBuildCurrentRaidMonsterStateBody(t *testing.T) {
	got, err := BuildRaidMonsterStateBody([]RaidMonsterStateGroup{
		{
			PartyIndex: 0x0203,
			Members:    []uint16{0x1234, 0xabcd},
			Value0:     0x11223344,
			Value1:     0x55667788,
			Values:     []uint32{0x01020304, 0xaabbccdd},
		},
		{
			PartyIndex: 0x0405,
			Value1:     0x10203040,
		},
	})
	if err != nil {
		t.Fatalf("BuildRaidMonsterStateBody error = %v", err)
	}
	want := []byte{
		2,
		0x03, 0x02, 2, 0x34, 0x12, 0xcd, 0xab,
		0x44, 0x33, 0x22, 0x11,
		0x88, 0x77, 0x66, 0x55,
		2, 0x04, 0x03, 0x02, 0x01, 0xdd, 0xcc, 0xbb, 0xaa,
		0x05, 0x04, 0,
		0, 0, 0, 0,
		0x40, 0x30, 0x20, 0x10,
		0,
	}
	if !reflect.DeepEqual(got, want) {
		t.Fatalf("raid monster-state body = % X, want % X", got, want)
	}
	empty, err := BuildRaidMonsterStateBody(nil)
	if err != nil {
		t.Fatalf("BuildRaidMonsterStateBody empty error = %v", err)
	}
	if got, want := empty, BuildEmptyRaidMonsterStateBody(); !reflect.DeepEqual(got, want) {
		t.Fatalf("empty raid monster-state body = % X, want % X", got, want)
	}
}

func TestRaidByteSizedCountsRejectTheTwoHundredFiftySixthRow(t *testing.T) {
	tooManyMembers := make([]uint16, maxRaidByteCount+1)
	tooManyValues := make([]uint32, maxRaidByteCount+1)
	tooManySymbols := make([]RaidSymbolRow, maxRaidByteCount+1)
	tests := []struct {
		name string
		call func() error
	}{
		{
			name: "outer count",
			call: func() error {
				_, err := BuildRaidSymbolBody(tooManySymbols)
				return err
			},
		},
		{
			name: "participation members",
			call: func() error {
				_, err := BuildRaidParticipationBody([]RaidParticipationRow{{Members: tooManyMembers}})
				return err
			},
		},
		{
			name: "reward entries",
			call: func() error {
				_, err := BuildRaidRewardListBody(RaidRewardList{Groups: []RaidRewardGroup{{
					Entries: make([]RaidRewardEntry, maxRaidByteCount+1),
				}}})
				return err
			},
		},
		{
			name: "buff entries",
			call: func() error {
				_, err := BuildRaidBuffBody([]RaidBuffGroup{{
					Entries: make([]RaidBuffEntry, maxRaidByteCount+1),
				}})
				return err
			},
		},
		{
			name: "monster members",
			call: func() error {
				_, err := BuildRaidMonsterStateBody([]RaidMonsterStateGroup{{Members: tooManyMembers}})
				return err
			},
		},
		{
			name: "monster values",
			call: func() error {
				_, err := BuildRaidMonsterStateBody([]RaidMonsterStateGroup{{Values: tooManyValues}})
				return err
			},
		},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			if err := tc.call(); err != errRaidByteCountOverflow {
				t.Fatalf("error = %v, want %v", err, errRaidByteCountOverflow)
			}
		})
	}
}

func TestBuildCurrentRaidEntryCostAndWaitingBodies(t *testing.T) {
	if got, want := BuildRaidCommandSuccessBody(), []byte{1}; !reflect.DeepEqual(got, want) {
		t.Fatalf("raid success body = % X, want % X", got, want)
	}
	if got, want := BuildEntryCostBody([]EntryCostRow{
		{MemberKey: 0x1234, Field: 1, Value: 7},
		{MemberKey: 0xabcd, Field: 0, Value: 0},
	}), []byte{
		2, 0, 0, 0,
		0x34, 0x12, 1, 7, 0,
		0xcd, 0xab, 0, 0, 0,
	}; !reflect.DeepEqual(got, want) {
		t.Fatalf("entry cost body = % X, want % X", got, want)
	}
	rows := []WaitingRow{{MemberKey: 0x1234, State: 2}, {MemberKey: 0xabcd, State: 0}}
	if got, want := BuildWaitingListBody(rows), []byte{2, 0, 0x34, 0x12, 2, 0xcd, 0xab, 0}; !reflect.DeepEqual(got, want) {
		t.Fatalf("waiting list body = % X, want % X", got, want)
	}
	if got, want := BuildWaitingChangeBody(0, rows[0]), []byte{0, 0x34, 0x12, 2}; !reflect.DeepEqual(got, want) {
		t.Fatalf("waiting add body = % X, want % X", got, want)
	}
	if got, want := BuildWaitingChangeBody(1, rows[1]), []byte{1, 0xcd, 0xab}; !reflect.DeepEqual(got, want) {
		t.Fatalf("waiting remove body = % X, want % X", got, want)
	}
}

func TestDecodeCreateAndModifyRaidInfoRequest(t *testing.T) {
	body := buildRaidInfoBodyForTest(2, []byte("anton"), 7)

	create, err := DecodeCreateRaidRequest(body)
	if err != nil {
		t.Fatalf("DecodeCreateRaidRequest error = %v", err)
	}
	modify, err := DecodeModifyRaidInfoRequest(body)
	if err != nil {
		t.Fatalf("DecodeModifyRaidInfoRequest error = %v", err)
	}
	for name, got := range map[string]InfoRequest{"create": create, "modify": modify} {
		if got.RouteOrRaidType != 2 || !bytesEqual(got.NameBytes, []byte("anton")) || got.TailFlag != 7 {
			t.Fatalf("%s request = %+v, want route=2 name=anton tail=7", name, got)
		}
	}
}

func TestDecodeRequestRaidInfoRequest(t *testing.T) {
	body := []byte{2, 0x78, 0x56, 0x34, 0x12}
	got, err := DecodeRequestRaidInfoRequest(body)
	if err != nil {
		t.Fatalf("DecodeRequestRaidInfoRequest error = %v", err)
	}
	if !bytesEqual(got.Raw[:], body) {
		t.Fatalf("request raid info = %+v", got)
	}
	for _, size := range []int{0, 4, 6} {
		if _, err := DecodeRequestRaidInfoRequest(make([]byte, size)); err == nil {
			t.Fatalf("DecodeRequestRaidInfoRequest accepted %d-byte body", size)
		}
	}
}

func TestBuildRaidInfoResponseBodyIsEmpty(t *testing.T) {
	if got := BuildRaidInfoResponseBody(); len(got) != 0 {
		t.Fatalf("raid info response body = % X, want empty", got)
	}
}

func TestBuildRaidDirectoryReadyBodyHasReservedByte(t *testing.T) {
	if got := BuildRaidDirectoryReadyBody(); len(got) != 1 || got[0] != 0 {
		t.Fatalf("raid directory ready body = % X, want 00", got)
	}
}

func TestDecodeCreateRaidInfoRequestRejectsTruncatedName(t *testing.T) {
	body := []byte{1, 5, 0, 0, 0, 'a', 'b'}
	if _, err := DecodeCreateRaidRequest(body); err == nil {
		t.Fatalf("DecodeCreateRaidRequest accepted truncated dstr")
	}
}

func TestRaidRequestDecodersRejectTrailingBytes(t *testing.T) {
	info := append(buildRaidInfoBodyForTest(2, []byte("anton"), 7), 0)
	if _, err := DecodeCreateRaidRequest(info); err == nil {
		t.Fatalf("DecodeCreateRaidRequest accepted trailing byte")
	}

	tests := []struct {
		name string
		body []byte
		call func([]byte) error
	}{
		{name: "leave", body: []byte{1, 0, 0}, call: func(body []byte) error { _, err := DecodeLeaveRaidRequest(body); return err }},
		{name: "rejoin", body: []byte{1, 0, 0, 0, 0}, call: func(body []byte) error { _, err := DecodeRejoinRaidRequest(body); return err }},
		{name: "waiting", body: []byte{0, 2, 0}, call: func(body []byte) error { _, err := DecodeSetWaitingRequest(body); return err }},
		{name: "entry cost", body: []byte{1, 0}, call: func(body []byte) error { _, err := DecodeEntryCostInfoRequest(body); return err }},
		{name: "set symbol", body: make([]byte, 10), call: func(body []byte) error { _, err := DecodeSetSymbolRequest(body); return err }},
		{name: "manager work", body: make([]byte, 13), call: func(body []byte) error { _, err := DecodeManagerWorkRequest(body); return err }},
		{name: "other channel join", body: make([]byte, 10), call: func(body []byte) error { _, err := DecodeOtherChannelRequestJoinRequest(body); return err }},
		{name: "member state", body: []byte{1, 0}, call: func(body []byte) error { _, err := DecodeMemberChangeStateRequest(body); return err }},
		{name: "move channel fail", body: []byte{1, 2, 0, 0}, call: func(body []byte) error { _, err := DecodeUserMoveChannelFailRequest(body); return err }},
		{name: "simple other channel list", body: []byte{1, 0}, call: func(body []byte) error { _, err := DecodeOtherChannelListRequest(body); return err }},
		{name: "check user", body: []byte{1, 2, 0, 0}, call: func(body []byte) error { _, err := DecodeCheckRaidUserRequest(body); return err }},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			if err := tc.call(tc.body); err == nil {
				t.Fatalf("decoder accepted trailing byte: % X", tc.body)
			}
		})
	}

	fullList := make([]byte, 14)
	fullList[0] = 0
	if _, err := DecodeOtherChannelListRequest(fullList); err == nil {
		t.Fatalf("DecodeOtherChannelListRequest accepted trailing byte after empty dstr")
	}
}

func TestDecodeSetSymbolRejectsOutOfRangeSymbol(t *testing.T) {
	body := make([]byte, 9)
	body[8] = 3
	if _, err := DecodeSetSymbolRequest(body); err == nil {
		t.Fatalf("DecodeSetSymbolRequest accepted symbol 3")
	}
}

func TestDecodeLeaveStartAndRejoinRaidRequests(t *testing.T) {
	leave, err := DecodeLeaveRaidRequest([]byte{0x34, 0x12})
	if err != nil {
		t.Fatalf("DecodeLeaveRaidRequest error = %v", err)
	}
	if leave.RaidOrMemberKey != 0x1234 {
		t.Fatalf("leave key = 0x%04X, want 0x1234", leave.RaidOrMemberKey)
	}
	if err := DecodeStartRaidRequest(nil); err != nil {
		t.Fatalf("DecodeStartRaidRequest empty error = %v", err)
	}
	if err := DecodeStartRaidRequest([]byte{1}); err == nil {
		t.Fatalf("DecodeStartRaidRequest accepted non-empty body")
	}
	rejoin, err := DecodeRejoinRaidRequest([]byte{0x78, 0x56, 0x34, 0x12})
	if err != nil {
		t.Fatalf("DecodeRejoinRaidRequest error = %v", err)
	}
	if rejoin.RaidKey != 0x12345678 {
		t.Fatalf("rejoin raid key = 0x%08X, want 0x12345678", rejoin.RaidKey)
	}
}

func TestDecodeRaidNeighborRequests(t *testing.T) {
	waiting, err := DecodeSetWaitingRequest([]byte{0, 2})
	if err != nil {
		t.Fatalf("DecodeSetWaitingRequest error = %v", err)
	}
	if waiting.Flag != 0 || waiting.RouteRaidType != 2 {
		t.Fatalf("waiting = %+v, want flag=0 route=2", waiting)
	}

	entryCost, err := DecodeEntryCostInfoRequest([]byte{1})
	if err != nil {
		t.Fatalf("DecodeEntryCostInfoRequest error = %v", err)
	}
	if entryCost.Enabled != 1 {
		t.Fatalf("entry cost enabled = %d, want 1", entryCost.Enabled)
	}

	setSymbolBody := make([]byte, 9)
	binary.LittleEndian.PutUint32(setSymbolBody[0:4], 0x11223344)
	binary.LittleEndian.PutUint32(setSymbolBody[4:8], 0x55667788)
	setSymbolBody[8] = 2
	setSymbol, err := DecodeSetSymbolRequest(setSymbolBody)
	if err != nil {
		t.Fatalf("DecodeSetSymbolRequest error = %v", err)
	}
	if setSymbol.SourceValue != 0x11223344 || setSymbol.TargetValue != 0x55667788 || setSymbol.Symbol != 2 {
		t.Fatalf("set symbol = %+v, want source=0x11223344 target=0x55667788 symbol=2", setSymbol)
	}

	joinBody := []byte{1, 0x34, 0x12, 0x78, 0x56, 0x34, 0x12, 0x02, 0x00}
	join, err := DecodeOtherChannelRequestJoinRequest(joinBody)
	if err != nil {
		t.Fatalf("DecodeOtherChannelRequestJoinRequest error = %v", err)
	}
	if join.Mode != 1 || join.TargetKey != 0x1234 || join.ClientValue != 0x12345678 || join.RouteRaidType != 2 {
		t.Fatalf("join = %+v, want mode=1 target=0x1234 client=0x12345678 route=2", join)
	}

	state, err := DecodeMemberChangeStateRequest([]byte{3})
	if err != nil {
		t.Fatalf("DecodeMemberChangeStateRequest error = %v", err)
	}
	if state.State != 3 {
		t.Fatalf("state = %d, want 3", state.State)
	}

	moveFail, err := DecodeUserMoveChannelFailRequest([]byte{4, 0x78, 0x56})
	if err != nil {
		t.Fatalf("DecodeUserMoveChannelFailRequest error = %v", err)
	}
	if moveFail.Mode != 4 || moveFail.TargetKey != 0x5678 {
		t.Fatalf("move fail = %+v, want mode=4 target=0x5678", moveFail)
	}

	check, err := DecodeCheckRaidUserRequest([]byte{5, 0xbc, 0x9a})
	if err != nil {
		t.Fatalf("DecodeCheckRaidUserRequest error = %v", err)
	}
	if check.Mode != 5 || check.TargetKey != 0x9abc {
		t.Fatalf("check = %+v, want mode=5 target=0x9abc", check)
	}
}

func TestDecodeOtherChannelListRequest(t *testing.T) {
	simple, err := DecodeOtherChannelListRequest([]byte{1})
	if err != nil {
		t.Fatalf("DecodeOtherChannelListRequest simple error = %v", err)
	}
	if simple.Mode != 1 || simple.HasContext || simple.HasNameDstr {
		t.Fatalf("simple list = %+v, want mode=1 without context", simple)
	}

	body := make([]byte, 13+4)
	body[0] = 0
	copy(body[1:9], []byte{1, 2, 3, 4, 5, 6, 7, 8})
	binary.LittleEndian.PutUint32(body[9:13], 4)
	copy(body[13:], []byte("raid"))
	full, err := DecodeOtherChannelListRequest(body)
	if err != nil {
		t.Fatalf("DecodeOtherChannelListRequest full error = %v", err)
	}
	if full.Mode != 0 || !full.HasContext || !full.HasNameDstr || !bytesEqual(full.NameBytes, []byte("raid")) {
		t.Fatalf("full list = %+v, want mode=0 context name=raid", full)
	}
	if full.Context != [8]byte{1, 2, 3, 4, 5, 6, 7, 8} {
		t.Fatalf("context = % X", full.Context)
	}
}

func TestBuildCheckUserResultBody(t *testing.T) {
	got := BuildCheckUserResultBody(CheckUserResult{
		Header: CheckUserHeader{
			Mode:        CheckUserModeRefreshWithHeaderState,
			Field4:      0x11223344,
			Field8:      0x55667788,
			Field12:     0x99,
			Raw13To20:   [8]byte{1, 2, 3, 4, 5, 6, 7, 8},
			Field21To24: 0xaabbccdd,
		},
		FirstRows: [][12]byte{
			{0x10, 0x11, 0x12, 0x13, 0x14, 0x15, 0x16, 0x17, 0x18, 0x19, 0x1a, 0x1b},
		},
		SecondRows: [][12]byte{
			{0x20, 0x21, 0x22, 0x23, 0x24, 0x25, 0x26, 0x27, 0x28, 0x29, 0x2a, 0x2b},
		},
		StateRows: [][5]byte{
			{0x30, 0x31, 0x32, 0x33, 0x34},
		},
	})
	want := []byte{
		0x01, 0x00, 0x00, 0x00,
		0x44, 0x33, 0x22, 0x11,
		0x88, 0x77, 0x66, 0x55,
		0x99,
		0x01, 0x02, 0x03, 0x04,
		0x05, 0x06, 0x07, 0x08,
		0xdd, 0xcc, 0xbb, 0xaa,
		0x01, 0x00, 0x00, 0x00,
		0x10, 0x11, 0x12, 0x13,
		0x14, 0x15, 0x16, 0x17,
		0x18, 0x19, 0x1a, 0x1b,
		0x01, 0x00, 0x00, 0x00,
		0x20, 0x21, 0x22, 0x23,
		0x24, 0x25, 0x26, 0x27,
		0x28, 0x29, 0x2a, 0x2b,
		0x01, 0x00, 0x00, 0x00,
		0x30, 0x31, 0x32, 0x33, 0x34,
	}
	if !reflect.DeepEqual(got, want) {
		t.Fatalf("check user body = % X, want % X", got, want)
	}
}

func TestBuildRequestMembersResultBody(t *testing.T) {
	got := BuildRequestMembersResultBody(RequestMembersResultEnterPublicRaidState, 0x55)
	want := []byte{0x02, 0x00, 0x00, 0x00, 0x55}
	if !reflect.DeepEqual(got, want) {
		t.Fatalf("request members result body = % X, want % X", got, want)
	}
}

func TestRaidCheckUserAndRequestMemberConstants(t *testing.T) {
	if CheckUserModeRefresh != 0 || CheckUserModeRefreshWithHeaderState != 1 {
		t.Fatalf("check user modes = %d/%d, want 0/1", CheckUserModeRefresh, CheckUserModeRefreshWithHeaderState)
	}
	if RequestMembersResultEnterPublicRaidState != 2 || RequestMembersResultSkipLocalError != 3 {
		t.Fatalf("request member results = %d/%d, want 2/3", RequestMembersResultEnterPublicRaidState, RequestMembersResultSkipLocalError)
	}
	if requestMembersUIStatePublicRaid != 8 || requestMembersUIStateLocalFailure != 10 {
		t.Fatalf("request member UI states = %d/%d, want 8/10", requestMembersUIStatePublicRaid, requestMembersUIStateLocalFailure)
	}
}

func TestBuildOtherChannelBodies(t *testing.T) {
	publicObject := RaidObjectMode0{
		RaidKey:       0x00170001,
		TitleBytes:    []byte{'R', '1'},
		ObjectField24: 5,
		ObjectField30: 0xff,
		Leader:        MemberRecord{CharKey: 1, RawNameBytes: []byte("leader")},
		Members:       []MemberRecord{{CharKey: 1, RawNameBytes: []byte("leader")}},
	}
	publicBody, err := BuildPublicRaidListBody(PublicRaidListPage{
		TotalCount: 1,
		Objects:    []RaidObjectMode0{publicObject},
	})
	if err != nil {
		t.Fatalf("BuildPublicRaidListBody error = %v", err)
	}
	coreObject := publicObject
	coreObject.Members = nil
	mode0Body, err := BuildRaidObjectMode0Body(coreObject)
	if err != nil {
		t.Fatalf("BuildRaidObjectMode0Body core error = %v", err)
	}
	wantPublic := []byte{
		1, 0, 0, 0, // total count
		0, 0, 0, 0, // returned offset
		1, 0, 0, 0, // page row count
	}
	// op591 adds raid-key/mode before the shared object and its own member count
	// plus member records after it. op753 embeds only the shared object plus its
	// member count.
	wantPublic = append(wantPublic, mode0Body[8:len(mode0Body)-1]...)
	wantPublic = append(wantPublic, 1)
	if !reflect.DeepEqual(publicBody, wantPublic) {
		t.Fatalf("public raid list body = % X, want % X", publicBody, wantPublic)
	}

	emptyPage := BuildOtherChannelListPageBody(OtherChannelListPage{})
	if len(emptyPage) != 28 {
		t.Fatalf("empty other channel list page length = %d, want 28", len(emptyPage))
	}
	if !reflect.DeepEqual(emptyPage[20:], []byte{0, 0, 0, 0, 0, 0, 0, 0}) {
		t.Fatalf("empty other channel list counts = % X, want eight zero bytes", emptyPage[20:])
	}

	userinfo := BuildOtherChannelUserinfoBody([]uint32{0x11223344, 0x55667788})
	wantUserinfo := []byte{
		0x02, 0x00, 0x00, 0x00,
		0x44, 0x33, 0x22, 0x11,
		0x88, 0x77, 0x66, 0x55,
	}
	if !reflect.DeepEqual(userinfo, wantUserinfo) {
		t.Fatalf("other channel userinfo body = % X, want % X", userinfo, wantUserinfo)
	}

	join := BuildOtherChannelRequestJoinResultBody(0x01020304, 0xaabbccdd)
	wantJoin := []byte{0x04, 0x03, 0x02, 0x01, 0xdd, 0xcc, 0xbb, 0xaa}
	if !reflect.DeepEqual(join, wantJoin) {
		t.Fatalf("other channel join result body = % X, want % X", join, wantJoin)
	}

	response := BuildOtherChannelResponseJoinBody(7, 1)
	if !reflect.DeepEqual(response, []byte{7, 1}) {
		t.Fatalf("other channel response join body = % X, want 07 01", response)
	}

	var header [20]byte
	for i := range header {
		header[i] = byte(i + 1)
	}
	page := BuildOtherChannelListPageBody(OtherChannelListPage{
		Header: header,
		Rows12: [][12]byte{
			{0x10, 0x11, 0x12, 0x13, 0x14, 0x15, 0x16, 0x17, 0x18, 0x19, 0x1a, 0x1b},
		},
		Rows13: [][13]byte{
			{0x20, 0x21, 0x22, 0x23, 0x24, 0x25, 0x26, 0x27, 0x28, 0x29, 0x2a, 0x2b, 0x2c},
		},
	})
	wantPage := []byte{
		0x01, 0x02, 0x03, 0x04,
		0x05, 0x06, 0x07, 0x08,
		0x09, 0x0a, 0x0b, 0x0c,
		0x0d, 0x0e, 0x0f, 0x10,
		0x11, 0x12, 0x13, 0x14,
		0x01, 0x00, 0x00, 0x00,
		0x10, 0x11, 0x12, 0x13,
		0x14, 0x15, 0x16, 0x17,
		0x18, 0x19, 0x1a, 0x1b,
		0x01, 0x00, 0x00, 0x00,
		0x20, 0x21, 0x22, 0x23,
		0x24, 0x25, 0x26, 0x27,
		0x28, 0x29, 0x2a, 0x2b,
		0x2c,
	}
	if !reflect.DeepEqual(page, wantPage) {
		t.Fatalf("other channel list page body = % X, want % X", page, wantPage)
	}
}

func TestBuildMemberRefreshMode3Body(t *testing.T) {
	got, err := BuildMemberRefreshMode3Body(MemberRefresh{
		RaidKey: 0x01020304,
		Members: []MemberRecord{
			{
				CharKey:           1001,
				Field4:            1,
				Name:              "A",
				Field40:           2,
				Field44:           3,
				GroupIndex:        4,
				SlotOrder:         5,
				Field48:           0x11223344,
				Field52:           6,
				Field53:           7,
				Field56:           8,
				Field60:           0x55667788,
				Field64:           0x99aa,
				Field66BoolSource: 1,
			},
		},
	})
	if err != nil {
		t.Fatalf("BuildMemberRefreshMode3Body error = %v", err)
	}
	want := []byte{
		0x04, 0x03, 0x02, 0x01,
		0x03, 0x00, 0x00, 0x00,
		0x01,
		0xe9, 0x03,
		0x01,
		0x02, 0x00, 0x00, 0x00,
		0x41, 0x00,
		0x02,
		0x03,
		0x04,
		0x05,
		0x44, 0x33, 0x22, 0x11,
		0x06,
		0x07,
		0x08,
		0x88, 0x77, 0x66, 0x55,
		0xaa, 0x99,
		0x01, 0x00, 0x00, 0x00,
	}
	if !reflect.DeepEqual(got, want) {
		t.Fatalf("refresh body = % X, want % X", got, want)
	}
}

func TestBuildMemberRefreshMode3BodyWritesNonZeroEmptyName(t *testing.T) {
	got, err := BuildMemberRefreshMode3Body(MemberRefresh{
		Members: []MemberRecord{{CharKey: 1001}},
	})
	if err != nil {
		t.Fatalf("BuildMemberRefreshMode3Body error = %v", err)
	}
	nameLenOffset := 8 + 1 + 2 + 1
	if length := binary.LittleEndian.Uint32(got[nameLenOffset : nameLenOffset+4]); length != 2 {
		t.Fatalf("empty name length = %d, want 2", length)
	}
	if !reflect.DeepEqual(got[nameLenOffset+4:nameLenOffset+6], []byte{0, 0}) {
		t.Fatalf("empty name bytes = % X, want 00 00", got[nameLenOffset+4:nameLenOffset+6])
	}
}

func TestBuildMemberRefreshMode3BodyUsesRawNameBytes(t *testing.T) {
	got, err := BuildMemberRefreshMode3Body(MemberRefresh{
		RaidKey: 0x01020304,
		Members: []MemberRecord{{
			CharKey:        1001,
			Name:           "七千万",
			RawNameBytes:   []byte{0xc6, 0xdf, 0xc7, 0xa7, 0xcd, 0xf2},
			RawNameUTF16LE: []byte{0x03, 0x4e, 0x43, 0x53, 0x07, 0x4e},
		}},
	})
	if err != nil {
		t.Fatalf("BuildMemberRefreshMode3Body error = %v", err)
	}
	nameLenOffset := 8 + 1 + 2 + 1
	if length := binary.LittleEndian.Uint32(got[nameLenOffset : nameLenOffset+4]); length != 6 {
		t.Fatalf("raw name length = %d, want 6", length)
	}
	if gotName := got[nameLenOffset+4 : nameLenOffset+10]; !reflect.DeepEqual(gotName, []byte{0xc6, 0xdf, 0xc7, 0xa7, 0xcd, 0xf2}) {
		t.Fatalf("raw name bytes = % X, want C6 DF C7 A7 CD F2", gotName)
	}
}

func TestBuildMemberRefreshMode3BodyRejectsTwentyFirstMember(t *testing.T) {
	members := make([]MemberRecord, MaxAttackPartyMembers+1)
	if _, err := BuildMemberRefreshMode3Body(MemberRefresh{Members: members}); err == nil {
		t.Fatalf("BuildMemberRefreshMode3Body accepted %d members", len(members))
	}
}

func TestBuildMemberRefreshMode3BodyAllowsTwentyMembers(t *testing.T) {
	members := make([]MemberRecord, MaxAttackPartyMembers)
	for i := range members {
		members[i].CharKey = uint16(1000 + i)
	}
	got, err := BuildMemberRefreshMode3Body(MemberRefresh{RaidKey: 0x20010001, Members: members})
	if err != nil {
		t.Fatalf("BuildMemberRefreshMode3Body error = %v", err)
	}
	if got[8] != MaxAttackPartyMembers {
		t.Fatalf("member count = %d, want %d", got[8], MaxAttackPartyMembers)
	}
}

func TestRaidHandlerParsesManagerWorkWithoutAck(t *testing.T) {
	body := make([]byte, 12)
	binary.LittleEndian.PutUint32(body[4:8], 1001)
	binary.LittleEndian.PutUint32(body[8:12], 4)

	got, err := NewHandler().Handle(contextForTest(), alignedcmd.Request{
		Opcode: uint16(dnfenum.CmdPacketRaidManagerWork),
		Body:   body,
	})
	if err != nil {
		t.Fatalf("Handle raid manager work error = %v", err)
	}
	if !got.Handled || got.ResponseAllowed || len(got.UpperResponses) != 0 {
		t.Fatalf("raid manager handled=%v responseAllowed=%v responses=%d", got.Handled, got.ResponseAllowed, len(got.UpperResponses))
	}
	if !strings.Contains(got.Reason, "0x24F mode=3") {
		t.Fatalf("reason = %q, want 0x24F mode=3 hint", got.Reason)
	}
}

func TestRaidHandlerParsesSetSymbolWithoutAck(t *testing.T) {
	body := make([]byte, 9)
	binary.LittleEndian.PutUint32(body[0:4], 0x11223344)
	binary.LittleEndian.PutUint32(body[4:8], 0x55667788)
	body[8] = 1

	got, err := NewHandler().Handle(contextForTest(), alignedcmd.Request{
		Opcode: uint16(dnfenum.CmdPacketRaidSetSymbol),
		Body:   body,
	})
	if err != nil {
		t.Fatalf("Handle raid set symbol error = %v", err)
	}
	if !got.Handled || got.ResponseAllowed || len(got.UpperResponses) != 0 {
		t.Fatalf("raid set symbol handled=%v responseAllowed=%v responses=%d", got.Handled, got.ResponseAllowed, len(got.UpperResponses))
	}
	if !strings.Contains(got.Reason, "RaidSetSymbol") {
		t.Fatalf("reason = %q, want RaidSetSymbol hint", got.Reason)
	}
}

func contextForTest() context.Context { return context.Background() }

func buildRaidInfoBodyForTest(route byte, name []byte, tail byte) []byte {
	body := make([]byte, 1+4+len(name)+1)
	body[0] = route
	binary.LittleEndian.PutUint32(body[1:5], uint32(len(name)))
	copy(body[5:], name)
	body[len(body)-1] = tail
	return body
}

func bytesEqual(left []byte, right []byte) bool {
	if len(left) != len(right) {
		return false
	}
	for i := range left {
		if left[i] != right[i] {
			return false
		}
	}
	return true
}
