package dnfbridge

import (
	"context"
	"encoding/binary"
	"fmt"
	"reflect"
	"strconv"
	"testing"

	"longheng.io/server/internal/modules/dnf/channelcatalog"
	"longheng.io/server/internal/modules/dnf/dnfenum"
	dnfproto "longheng.io/server/internal/modules/dnf/protocol"
	dnfpvf "longheng.io/server/internal/modules/dnf/pvf"
	dnfquest "longheng.io/server/internal/modules/dnf/quest"
	dnfrepo "longheng.io/server/internal/modules/dnf/repository"
	dnfrepomemory "longheng.io/server/internal/modules/dnf/repository/memory"
)

func TestCurrentAutoCompleteMainQuestLegacyRouteCommitsNoRewardAndSendsExactReaderThenSnapshots(t *testing.T) {
	ctx := context.Background()
	repositories := dnfrepomemory.NewMemoryGroup()
	character := dnfrepo.CharacterRecord{
		CharacterID: "17",
		AccountID:   defaultAccountPrefix + "1",
		Job:         "2",
		Level:       90,
		Stats:       map[string]int64{"grow_type": 0, "sp": 999, "exp": 54321},
	}
	if err := repositories.Character.Save(ctx, character); err != nil {
		t.Fatal(err)
	}
	if err := repositories.Quest.Save(ctx, dnfrepo.QuestRecord{
		CharacterID: "17",
		States: map[int64]dnfrepo.QuestState{
			100: {Status: "active", ProgressValue: 1},
			102: {Status: "active", ProgressValue: 1},
			103: {Status: "active", ProgressValue: 1},
			106: {Status: "active", ProgressValue: 1},
		},
	}); err != nil {
		t.Fatal(err)
	}
	connection := &bufferConn{}
	service := &Service{
		options:            options{gameUpperBodyCodec: gameUpperBodyCodecPlain},
		questCatalog:       buildAutoCompleteMainBridgeCatalog(t),
		repositoryProvider: func() (dnfrepo.Group, bool) { return repositories, true },
	}
	session := &gameSession{
		conn:                connection,
		connID:              "auto-complete-main-legacy",
		channel:             channelcatalog.Channel{ID: 19},
		selectedCharacterID: 17,
	}
	if err := service.handleGameCommand(
		session,
		byte(dnfenum.GameCmdCommand),
		uint16(dnfenum.CmdPacketClearQuestTicket),
		[]byte{100, 0, 0, 0},
	); err != nil {
		t.Fatal(err)
	}

	stored, found, err := repositories.Quest.Load(ctx, "17")
	if err != nil || !found {
		t.Fatalf("load quest found=%t err=%v", found, err)
	}
	for _, questID := range []int64{100} {
		if state := stored.States[questID]; state.Status != "completed" || state.Extra["reward_state"] != "granted" {
			t.Fatalf("epic quest %d state=%+v", questID, state)
		}
	}
	if _, exists := stored.States[101]; exists {
		t.Fatalf("absent successor quest 101 was manufactured: %+v", stored.States[101])
	}
	for _, questID := range []int64{102, 103, 106} {
		if state := stored.States[questID]; state.Status != "active" || state.ProgressValue != 1 {
			t.Fatalf("non-target quest %d state=%+v", questID, state)
		}
	}
	postCharacter, found, err := repositories.Character.Load(ctx, "17")
	if err != nil || !found || !reflect.DeepEqual(postCharacter, character) {
		t.Fatalf("no-reward operation changed character found=%t err=%v got=%+v want=%+v", found, err, postCharacter, character)
	}

	response, rest := splitGameServerUpperPacket(t, connection.write.Bytes())
	if response.Header.MsgID != uint16(dnfenum.CmdPacketClearQuestTicket) ||
		response.Header.Classification != dnfproto.DefaultChannelClassification ||
		len(response.Body) != currentAutoCompleteMainQuestAckSuccessSize ||
		response.Body[0] != 1 {
		t.Fatalf("op1429 response header=%+v body=%x", response.Header, response.Body)
	}

	if len(rest) < 16 || rest[0] != 0 || binary.LittleEndian.Uint16(rest[1:3]) != currentClearQuestListMsgID {
		t.Fatalf("completed-quest snapshot header=%x", rest[:minInt(len(rest), 16)])
	}
	clearPacketLength := int(binary.LittleEndian.Uint32(rest[3:7]))
	if clearPacketLength < 16 || clearPacketLength > len(rest) || rest[15] != 1 {
		t.Fatalf("completed-quest snapshot length=%d wire=%x", clearPacketLength, rest[:minInt(len(rest), 16)])
	}
	clearBody, err := zlibDecompress(rest[16:clearPacketLength])
	if err != nil {
		t.Fatal(err)
	}
	if clearBody[4+100] != 1 || clearBody[4+101] != 0 || clearBody[4+102] != 0 || clearBody[4+103] != 0 {
		t.Fatalf("completed-quest flags 100=%d 101=%d 102=%d 103=%d", clearBody[4+100], clearBody[4+101], clearBody[4+102], clearBody[4+103])
	}
	active, acceptableWire := splitGameServerUpperPacket(t, rest[clearPacketLength:])
	if active.Header.MsgID != currentActiveQuestSnapshotMsgID || active.Header.Classification != 0 {
		t.Fatalf("active snapshot header=%+v body=%x", active.Header, active.Body)
	}
	acceptable, trailing := splitGameServerUpperPacket(t, acceptableWire)
	if acceptable.Header.MsgID != currentAcceptableQuestListMsgID || acceptable.Header.Classification != 0 || len(trailing) != 0 {
		t.Fatalf("acceptable snapshot header=%+v trailing=%x", acceptable.Header, trailing)
	}
	if len(acceptable.Body) < 4 || int(binary.LittleEndian.Uint32(acceptable.Body[:4])) != len(acceptable.Body)-4 {
		t.Fatalf("acceptable snapshot protobuf length mismatch: %x", acceptable.Body)
	}
	_, messages := consumeCurrentSkillInfoFields(t, acceptable.Body[4:])
	if len(messages[4]) != 1 {
		t.Fatalf("acceptable snapshot field4 rows=%d body=%x", len(messages[4]), acceptable.Body)
	}
	questIDs := consumePackedQuestIDs(t, messages[4][0])
	foundCompleted := false
	foundSuccessor := false
	for _, questID := range questIDs {
		if questID == 100 {
			foundCompleted = true
		}
		if questID == 101 {
			foundSuccessor = true
		}
	}
	if foundCompleted {
		t.Fatalf("completed task-manual definition 100 leaked into op21: %v", questIDs)
	}
	if !foundSuccessor {
		t.Fatalf("successor main quest 101 missing from task-manual snapshot: %v", questIDs)
	}
	activeCount := int(binary.LittleEndian.Uint32(active.Body[:4]))
	activeIDs := make([]uint16, 0, activeCount)
	for offset := 4; offset+6 <= len(active.Body); offset += 6 {
		activeIDs = append(activeIDs, binary.LittleEndian.Uint16(active.Body[offset:offset+2]))
	}
	if !reflect.DeepEqual(activeIDs, []uint16{102, 103, 106}) {
		t.Fatalf("active quest ids=%v body=%x", activeIDs, active.Body)
	}
}

func TestCurrentAutoCompleteMainQuestNonzeroSelectorCompletesOnlyExactQuest(t *testing.T) {
	repositories := dnfrepomemory.NewMemoryGroup()
	ctx := context.Background()
	if err := repositories.Character.Save(ctx, dnfrepo.CharacterRecord{
		CharacterID: "17", AccountID: defaultAccountPrefix + "1", Job: "2", Level: 90,
	}); err != nil {
		t.Fatal(err)
	}
	if err := repositories.Quest.Save(ctx, dnfrepo.QuestRecord{
		CharacterID: "17",
		States: map[int64]dnfrepo.QuestState{
			100: {Status: "active"},
			106: {Status: "active"},
		},
	}); err != nil {
		t.Fatal(err)
	}
	connection := &bufferConn{}
	service := &Service{
		options:            options{gameUpperBodyCodec: gameUpperBodyCodecPlain},
		questCatalog:       buildAutoCompleteMainBridgeCatalog(t),
		repositoryProvider: func() (dnfrepo.Group, bool) { return repositories, true },
	}
	session := &gameSession{conn: connection, selectedCharacterID: 17}
	if err := service.handleCurrentAutoCompleteMainQuest(session, []byte{100, 0, 0, 0}); err != nil {
		t.Fatal(err)
	}
	stored, found, err := repositories.Quest.Load(ctx, "17")
	if err != nil || !found || stored.States[100].Status != "completed" || stored.States[106].Status != "active" {
		t.Fatalf("exact selector result found=%t err=%v record=%+v", found, err, stored)
	}
	response, _ := splitGameServerUpperPacket(t, connection.write.Bytes())
	if len(response.Body) != currentAutoCompleteMainQuestAckSuccessSize || response.Body[0] != 1 {
		t.Fatalf("exact selector response=%x", response.Body)
	}
}

func TestCurrentAutoCompleteMainQuestZeroSelectorCompletesAllEligibleMainQuests(t *testing.T) {
	repositories := dnfrepomemory.NewMemoryGroup()
	ctx := context.Background()
	if err := repositories.Character.Save(ctx, dnfrepo.CharacterRecord{
		CharacterID: "17", AccountID: defaultAccountPrefix + "1", Job: "2", Level: 90,
		// A bulk request past level 50 needs the class change taken; see
		// TestCurrentAutoCompleteMainQuestUntransferredJobIsRejectedWithNotice.
		Stats: map[string]int64{"grow_type": 2},
	}); err != nil {
		t.Fatal(err)
	}
	if err := repositories.Quest.Save(ctx, dnfrepo.QuestRecord{
		CharacterID: "17",
		States: map[int64]dnfrepo.QuestState{
			100: {Status: "active"},
			106: {Status: "active"},
		},
	}); err != nil {
		t.Fatal(err)
	}
	connection := &bufferConn{}
	service := &Service{
		options:            options{gameUpperBodyCodec: gameUpperBodyCodecPlain},
		questCatalog:       buildAutoCompleteMainBridgeCatalog(t),
		repositoryProvider: func() (dnfrepo.Group, bool) { return repositories, true },
	}
	session := &gameSession{conn: connection, selectedCharacterID: 17}
	if err := service.handleCurrentAutoCompleteMainQuest(session, []byte{0, 0, 0, 0}); err != nil {
		t.Fatal(err)
	}
	stored, found, err := repositories.Quest.Load(ctx, "17")
	if err != nil || !found || stored.States[100].Status != "completed" || stored.States[106].Status != "completed" {
		t.Fatalf("zero selector result found=%t err=%v record=%+v", found, err, stored)
	}
	if stored.States[100].Extra["reward_state"] != "granted" || stored.States[106].Extra["reward_state"] != "granted" {
		t.Fatalf("zero selector must remain no-reward completion record=%+v", stored)
	}
	response, trailing := splitGameServerUpperPacket(t, connection.write.Bytes())
	// The bulk request carries selector 0, so the old packed body opened with a
	// zero byte and the class-1 envelope read the whole success as a failure.
	if len(response.Body) != currentAutoCompleteMainQuestAckSuccessSize || response.Body[0] != 1 {
		t.Fatalf("zero selector response=%x trailing=%x", response.Body, trailing)
	}
	if len(trailing) == 0 {
		t.Fatal("zero selector success missing post-commit quest snapshots")
	}
}

func buildAutoCompleteMainBridgeCatalog(t *testing.T) *dnfquest.Catalog {
	t.Helper()
	definition := func(grade string, level int, job, extra string) string {
		return "[grade]\n`" + grade + "`\n[level]\n" + strconv.Itoa(level) + " 99\n[job]\n`" + job + "`\n[exposed by npc]\n1\n" + extra
	}
	source := questListTestSource{
		dnfquest.DefaultList:            "100 `epic_start.qst`\n101 `epic_next.qst`\n102 `epic_level_90.qst`\n103 `side.qst`\n106 `other_active_epic.qst`\n",
		"n_quest/epic_start.qst":        definition("[epic]", 20, "[gunner]", "[type]\n`[clear map]`\n"),
		"n_quest/epic_next.qst":         definition("[epic]", 30, "[gunner]", "[type]\n`[meet npc]`\n[pre required quest]\n100\n"),
		"n_quest/epic_level_90.qst":     definition("[epic]", 90, "[gunner]", "[type]\n`[meet npc]`\n[pre required quest]\n101\n"),
		"n_quest/side.qst":              definition("[side]", 10, "[gunner]", "[type]\n`[meet npc]`\n"),
		"n_quest/other_active_epic.qst": definition("[epic]", 35, "[gunner]", "[type]\n`[meet npc]`\n"),
	}
	index, err := dnfpvf.Build(context.Background(), source, dnfpvf.BuildOptions{Lists: []string{dnfquest.DefaultList}})
	if err != nil {
		t.Fatal(err)
	}
	catalog, err := dnfquest.Load(context.Background(), index)
	if err != nil {
		t.Fatal(err)
	}
	return catalog
}

// A bulk request that finds nothing it may complete must still answer with the
// class-1 failure envelope - one zero status byte plus the error byte the
// envelope reader consumes for this opcode - and then say why. The old packed
// body opened with the selector instead, so success and failure were
// indistinguishable on the wire and the client drew nothing either way.
func TestCurrentAutoCompleteMainQuestWithoutEligibleTargetSendsFailureEnvelopeAndNotice(t *testing.T) {
	repositories := dnfrepomemory.NewMemoryGroup()
	ctx := context.Background()
	if err := repositories.Character.Save(ctx, dnfrepo.CharacterRecord{
		CharacterID: "17", AccountID: defaultAccountPrefix + "1", Job: "2", Level: 90,
		Stats: map[string]int64{"grow_type": 2},
	}); err != nil {
		t.Fatal(err)
	}
	if err := repositories.Quest.Save(ctx, dnfrepo.QuestRecord{
		CharacterID: "17",
		// 102 stays open on purpose: it sits at level 90, above the level-89
		// cutoff, so the bulk pass has nothing left it may touch.
		States: map[int64]dnfrepo.QuestState{
			100: {Status: "completed"},
			101: {Status: "completed"},
			102: {Status: "active"},
			106: {Status: "completed"},
		},
	}); err != nil {
		t.Fatal(err)
	}
	connection := &bufferConn{}
	service := &Service{
		options:            options{gameUpperBodyCodec: gameUpperBodyCodecPlain},
		questCatalog:       buildAutoCompleteMainBridgeCatalog(t),
		repositoryProvider: func() (dnfrepo.Group, bool) { return repositories, true },
	}
	session := &gameSession{conn: connection, selectedCharacterID: 17}
	if err := service.handleCurrentAutoCompleteMainQuest(session, []byte{0, 0, 0, 0}); err != nil {
		t.Fatal(err)
	}
	response, trailing := splitGameServerUpperPacket(t, connection.write.Bytes())
	if response.Header.MsgID != uint16(dnfenum.CmdPacketClearQuestTicket) ||
		response.Header.Classification != dnfproto.DefaultChannelClassification ||
		len(response.Body) != currentAutoCompleteMainQuestAckFailureSize ||
		response.Body[0] != 0 {
		t.Fatalf("no-target response header=%+v body=%x", response.Header, response.Body)
	}
	assertAutoCompleteRejectionNotice(t, trailing, currentAutoCompleteNoTargetNoticeText)
}

// The bulk pass past level 50 can only end in an empty candidate set when the
// character never took its class change: the story chain is gated on the first
// awakening quest group, and that group is the one thing a bulk pass must not
// complete for the player. Reject it up front with a notice that names the
// missing step instead of a silent empty result.
func TestCurrentAutoCompleteMainQuestUntransferredJobIsRejectedWithNotice(t *testing.T) {
	repositories := dnfrepomemory.NewMemoryGroup()
	ctx := context.Background()
	if err := repositories.Character.Save(ctx, dnfrepo.CharacterRecord{
		CharacterID: "17", AccountID: defaultAccountPrefix + "1", Job: "2", Level: 90,
		Stats: map[string]int64{"grow_type": 0},
	}); err != nil {
		t.Fatal(err)
	}
	// Same exhausted set as the no-target case above; only grow_type differs, so
	// the rejection reason is the missing class change instead of "nothing left".
	wantQuest := dnfrepo.QuestRecord{
		CharacterID: "17",
		States: map[int64]dnfrepo.QuestState{
			100: {Status: "completed"},
			101: {Status: "completed"},
			102: {Status: "active"},
			106: {Status: "completed"},
		},
	}
	if err := repositories.Quest.Save(ctx, wantQuest); err != nil {
		t.Fatal(err)
	}
	connection := &bufferConn{}
	service := &Service{
		options:            options{gameUpperBodyCodec: gameUpperBodyCodecPlain},
		questCatalog:       buildAutoCompleteMainBridgeCatalog(t),
		repositoryProvider: func() (dnfrepo.Group, bool) { return repositories, true },
	}
	session := &gameSession{conn: connection, selectedCharacterID: 17}
	if err := service.handleCurrentAutoCompleteMainQuest(session, []byte{0, 0, 0, 0}); err != nil {
		t.Fatal(err)
	}
	stored, found, err := repositories.Quest.Load(ctx, "17")
	if err != nil || !found || !reflect.DeepEqual(stored, wantQuest) {
		t.Fatalf("rejected bulk request mutated quest found=%t err=%v got=%+v want=%+v", found, err, stored, wantQuest)
	}
	response, trailing := splitGameServerUpperPacket(t, connection.write.Bytes())
	if response.Header.MsgID != uint16(dnfenum.CmdPacketClearQuestTicket) ||
		len(response.Body) != currentAutoCompleteMainQuestAckFailureSize ||
		response.Body[0] != 0 {
		t.Fatalf("class-change rejection header=%+v body=%x", response.Header, response.Body)
	}
	assertAutoCompleteRejectionNotice(t, trailing, currentAutoCompleteClassChangeNoticeText)
}

func assertAutoCompleteRejectionNotice(t *testing.T, wire []byte, wantText string) {
	t.Helper()
	notice, rest := splitGameServerUpperPacketWithHeader(t, wire, dnfproto.GameServerUpperHeaderSize16)
	if notice.Header.MsgID != currentNativeNoticeMsgID || notice.Header.Classification != 0 || len(rest) != 0 {
		t.Fatalf("rejection notice header=%+v rest=%x", notice.Header, rest)
	}
	wantBody, err := buildCurrentNativeNoticeBody(
		currentAutoCompleteMainQuestNoticeSurface.Type,
		currentAutoCompleteMainQuestNoticeSurface.Channel,
		0,
		wantText,
	)
	if err != nil {
		t.Fatal(err)
	}
	if string(notice.Body) != string(wantBody) {
		t.Fatalf("rejection notice body=%x want=%x", notice.Body, wantBody)
	}
}

// The blocking awakening quest is still sitting in the player's list, so
// "当前没有可以一键完成的主线任务" reads as a lie. Name the quest instead.
func TestCurrentAutoCompleteMainQuestBlockedByAwakeningNamesTheQuest(t *testing.T) {
	repositories := dnfrepomemory.NewMemoryGroup()
	ctx := context.Background()
	if err := repositories.Character.Save(ctx, dnfrepo.CharacterRecord{
		CharacterID: "17", AccountID: defaultAccountPrefix + "1", Job: "2", Level: 90,
		Stats: map[string]int64{"grow_type": 0x13},
	}); err != nil {
		t.Fatal(err)
	}
	wantQuest := dnfrepo.QuestRecord{
		CharacterID: "17",
		States:      map[int64]dnfrepo.QuestState{200: {Status: "active", ProgressValue: 1}},
	}
	if err := repositories.Quest.Save(ctx, wantQuest); err != nil {
		t.Fatal(err)
	}
	connection := &bufferConn{}
	service := &Service{
		options:            options{gameUpperBodyCodec: gameUpperBodyCodecPlain},
		questCatalog:       buildAutoCompleteAwakeningBridgeCatalog(t),
		repositoryProvider: func() (dnfrepo.Group, bool) { return repositories, true },
	}
	session := &gameSession{conn: connection, selectedCharacterID: 17}
	if err := service.handleCurrentAutoCompleteMainQuest(session, []byte{0, 0, 0, 0}); err != nil {
		t.Fatal(err)
	}
	stored, found, err := repositories.Quest.Load(ctx, "17")
	if err != nil || !found || !reflect.DeepEqual(stored, wantQuest) {
		t.Fatalf("awakening rejection mutated quest found=%t err=%v got=%+v want=%+v", found, err, stored, wantQuest)
	}
	response, trailing := splitGameServerUpperPacket(t, connection.write.Bytes())
	if len(response.Body) != currentAutoCompleteMainQuestAckFailureSize || response.Body[0] != 0 {
		t.Fatalf("awakening rejection body=%x", response.Body)
	}
	assertAutoCompleteRejectionNotice(t, trailing,
		fmt.Sprintf(currentAutoCompleteAwakeningNoticeFormat, "依然存在的不祥之感"))
}

func buildAutoCompleteAwakeningBridgeCatalog(t *testing.T) *dnfquest.Catalog {
	t.Helper()
	source := questListTestSource{
		dnfquest.DefaultList: "200 `awakening.qst`\n201 `after_awakening.qst`\n",
		"n_quest/awakening.qst": "[grade]\n`[epic]`\n[level]\n50 99\n[job]\n`[gunner]`\n" +
			"[exposed by npc]\n1\n[type]\n`[clear map]`\n[grow type]\n3\n[job change quest]\n2\n" +
			"[reward type]\n`[awakening type]`\n[name]\n`依然存在的不祥之感`\n",
		"n_quest/after_awakening.qst": "[grade]\n`[epic]`\n[level]\n50 99\n[job]\n`[all]`\n" +
			"[exposed by npc]\n1\n[type]\n`[meet npc]`\n[pre required quest]\n200\n",
	}
	index, err := dnfpvf.Build(context.Background(), source, dnfpvf.BuildOptions{Lists: []string{dnfquest.DefaultList}})
	if err != nil {
		t.Fatal(err)
	}
	catalog, err := dnfquest.Load(context.Background(), index)
	if err != nil {
		t.Fatal(err)
	}
	return catalog
}

// The Lv87 branch quest stays in the player's list until they answer it, so the
// rejection has to name it rather than claim the list is empty.
func TestCurrentAutoCompleteMainQuestBlockedByChoiceNamesTheQuest(t *testing.T) {
	repositories := dnfrepomemory.NewMemoryGroup()
	ctx := context.Background()
	if err := repositories.Character.Save(ctx, dnfrepo.CharacterRecord{
		CharacterID: "17", AccountID: defaultAccountPrefix + "1", Job: "2", Level: 90,
		Stats: map[string]int64{"grow_type": 0x13},
	}); err != nil {
		t.Fatal(err)
	}
	wantQuest := dnfrepo.QuestRecord{
		CharacterID: "17",
		States:      map[int64]dnfrepo.QuestState{300: {Status: "active", ProgressValue: 1}},
	}
	if err := repositories.Quest.Save(ctx, wantQuest); err != nil {
		t.Fatal(err)
	}
	source := questListTestSource{
		dnfquest.DefaultList: "300 `branch.qst`\n301 `branch_a.qst`\n",
		"n_quest/branch.qst": "[grade]\n`[epic]`\n[level]\n87 99\n[job]\n`[gunner]`\n" +
			"[exposed by npc]\n1\n[type]\n`[question]`\n[name]\n`冒险家联盟的劝说和选择`\n",
		"n_quest/branch_a.qst": "[grade]\n`[epic]`\n[level]\n87 99\n[job]\n`[gunner]`\n" +
			"[exposed by npc]\n1\n[type]\n`[meet npc]`\n[pre required quest answer]\n300 1\n",
	}
	index, err := dnfpvf.Build(context.Background(), source, dnfpvf.BuildOptions{Lists: []string{dnfquest.DefaultList}})
	if err != nil {
		t.Fatal(err)
	}
	catalog, err := dnfquest.Load(context.Background(), index)
	if err != nil {
		t.Fatal(err)
	}
	connection := &bufferConn{}
	service := &Service{
		options:            options{gameUpperBodyCodec: gameUpperBodyCodecPlain},
		questCatalog:       catalog,
		repositoryProvider: func() (dnfrepo.Group, bool) { return repositories, true },
	}
	session := &gameSession{conn: connection, selectedCharacterID: 17}
	if err := service.handleCurrentAutoCompleteMainQuest(session, []byte{0, 0, 0, 0}); err != nil {
		t.Fatal(err)
	}
	stored, found, err := repositories.Quest.Load(ctx, "17")
	if err != nil || !found || !reflect.DeepEqual(stored, wantQuest) {
		t.Fatalf("choice rejection mutated quest found=%t err=%v got=%+v", found, err, stored)
	}
	response, trailing := splitGameServerUpperPacket(t, connection.write.Bytes())
	if len(response.Body) != currentAutoCompleteMainQuestAckFailureSize || response.Body[0] != 0 {
		t.Fatalf("choice rejection body=%x", response.Body)
	}
	assertAutoCompleteRejectionNotice(t, trailing,
		fmt.Sprintf(currentAutoCompleteChoiceNoticeFormat, "冒险家联盟的劝说和选择"))
}
