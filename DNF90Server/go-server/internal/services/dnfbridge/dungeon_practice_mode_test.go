package dnfbridge

import "testing"

// currentDungeonProtocolModeForRoom is the room-aware half of the C2S op16
// mapping: state=1 + token=UINT16_MAX is this client's 练习模式 switch for an
// ordinary dungeon, while the two guided direct-entry rooms send the same pair
// from their own buttons and must keep the ordinary policy.
//
// Live capture 2026-09-17 (packet_log.txt): 481 select requests, every ordinary
// 进入 among them carrying state=0 + token=UINT16_MAX; the only state=1 +
// UINT16_MAX requests were the guided rooms (7142) and the operator's 练习模式
// attempt on 190, which the ordinary reading refused with the 「疲劳值不足」
// prompt because the character's fatigue had been zeroed on purpose.
func TestCurrentDungeonProtocolModeForRoomResolvesPracticeAndGuidedRooms(t *testing.T) {
	cases := []struct {
		name         string
		runtimeState byte
		runtimeToken uint16
		dungeonID    int64
		wantMode     currentDungeonProtocolMode
		wantOK       bool
	}{
		{
			name: "default ordinary entry", runtimeState: 0, runtimeToken: 0xffff, dungeonID: 190,
			wantMode: currentDungeonProtocolModeNormal, wantOK: true,
		},
		{
			name: "练习模式 on an ordinary dungeon", runtimeState: 1, runtimeToken: 0xffff, dungeonID: 190,
			wantMode: currentDungeonProtocolModePractice, wantOK: true,
		},
		{
			name: "练习模式 on another ordinary dungeon", runtimeState: 1, runtimeToken: 0xffff, dungeonID: 7115,
			wantMode: currentDungeonProtocolModePractice, wantOK: true,
		},
		{
			name: "explicit practice pair", runtimeState: 1, runtimeToken: 0, dungeonID: 190,
			wantMode: currentDungeonProtocolModePractice, wantOK: true,
		},
		{
			name:         "修炼场 direct button keeps the ordinary policy",
			runtimeState: 1, runtimeToken: 0xffff, dungeonID: int64(currentPungjinTrainingDungeonID),
			wantMode: currentDungeonProtocolModeNormal, wantOK: true,
		},
		{
			name:         "转职体验 direct button keeps the ordinary policy",
			runtimeState: 1, runtimeToken: 0xffff, dungeonID: int64(currentJobChangeTrialDungeonID),
			wantMode: currentDungeonProtocolModeNormal, wantOK: true,
		},
		{
			name: "unproven pair", runtimeState: 1, runtimeToken: 1, dungeonID: 190,
			wantMode: currentDungeonProtocolModeNormal, wantOK: false,
		},
	}
	for _, testCase := range cases {
		t.Run(testCase.name, func(t *testing.T) {
			mode, ok := currentDungeonProtocolModeForRoom(
				testCase.runtimeState, testCase.runtimeToken, testCase.dungeonID,
			)
			if mode != testCase.wantMode || ok != testCase.wantOK {
				t.Fatalf("currentDungeonProtocolModeForRoom(%d,%d,%d)=(%s,%t) want=(%s,%t)",
					testCase.runtimeState, testCase.runtimeToken, testCase.dungeonID,
					mode, ok, testCase.wantMode, testCase.wantOK)
			}
		})
	}
}

// The guided-room exemption is keyed to the room, not to the shape, so a
// neighbouring id must still reach the practice policy.
func TestCurrentDungeonGuidedDirectEntryRoomIsKeyedToTheRoom(t *testing.T) {
	if !currentDungeonGuidedDirectEntryRoom(int64(currentPungjinTrainingDungeonID)) {
		t.Fatal("the training room must own the guided direct-entry route")
	}
	if !currentDungeonGuidedDirectEntryRoom(int64(currentJobChangeTrialDungeonID)) {
		t.Fatal("the job-change trial must own the guided direct-entry route")
	}
	for _, dungeonID := range []int64{0, -1, 190, 5001, 7141, 7143, 1 << 33} {
		if currentDungeonGuidedDirectEntryRoom(dungeonID) {
			t.Fatalf("dungeon %d must not be treated as a guided direct-entry room", dungeonID)
		}
	}
}
