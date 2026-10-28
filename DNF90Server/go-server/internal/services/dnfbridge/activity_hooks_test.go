package dnfbridge

import (
	"strings"
	"testing"
	"time"

	"longheng.io/server/internal/modules/dnf/alignedcmd"
	"longheng.io/server/internal/modules/dnf/channelcatalog"
	"longheng.io/server/internal/modules/dnf/worldmap"
)

// TestChatRecordTextDecodesGBK 锁住聊天正文的解码：客户端发的是 GBK 字节，
// 记录里必须是能看懂的中文，而不是一串问号或乱码。
func TestChatRecordTextDecodesGBK(t *testing.T) {
	cases := []struct {
		name    string
		raw     []byte
		want    string
		wantRaw bool
	}{
		{name: "ascii", raw: []byte("hello world"), want: "hello world"},
		{name: "empty", raw: nil, want: ""},
		// “你好” 的 GBK 编码是 C4 E3 BA C3。
		{name: "gbk", raw: []byte{0xC4, 0xE3, 0xBA, 0xC3}, want: "你好"},
	}
	for _, testCase := range cases {
		t.Run(testCase.name, func(t *testing.T) {
			if got := chatRecordText(testCase.raw); got != testCase.want {
				t.Fatalf("chatRecordText(%v) = %q, want %q", testCase.raw, got, testCase.want)
			}
		})
	}
	// 一段解不出合法文本的字节不能被丢掉：原样保留（转成字符串）比丢信息好。
	junk := []byte{0x81, 0x40, 0xFF}
	if got := chatRecordText(junk); got == "" {
		t.Fatalf("undecodable message was dropped: %v", junk)
	}
}

func TestCurrentTownChatModeName(t *testing.T) {
	cases := map[byte]string{
		currentTownChatWhisperMode:          "私聊",
		currentTownChatOneToOneMode:         "1:1会话",
		currentTownChatMegaphoneMode:        "服务器喇叭",
		currentTownChatChannelMegaphoneMode: "频道喇叭",
		currentTownChatAreaMode:             "区域",
		currentTownChatPartyMode:            "队伍",
		currentTownChatGuildMode:            "公会",
		currentTownChatGuildLegacyMode:      "公会",
		currentTownChatRaidMode:             "攻坚队",
	}
	for mode, want := range cases {
		if got := currentTownChatModeName(mode); got != want {
			t.Fatalf("mode %d name = %q, want %q", mode, got, want)
		}
	}
	if got := currentTownChatModeName(0xEE); !strings.Contains(got, "未知") {
		t.Fatalf("unknown mode name = %q", got)
	}
}

// TestRecordChatFields 覆盖一条对话记录的字段形状：查记录的人要能直接回答
// "谁、在哪个频道、用什么方式、说了什么、发给了多少人"。
func TestRecordChatFields(t *testing.T) {
	recorder, _ := newTestRecorder(t)
	service := &Service{}
	service.records.Store(recorder)
	session := &gameSession{accountID: "dnf:77", channel: channelcatalog.Channel{ID: 508}}
	session.selectedCharacterID = 21

	service.recordChat(session, currentTownChatAreaMode, currentTownChatModeName(currentTownChatAreaMode),
		[]byte{0xC4, 0xE3, 0xBA, 0xC3}, 3, 0, "ok", "", map[string]any{"scope": "visible_town_scene"})
	service.recordChat(session, currentTownChatMegaphoneMode, currentTownChatModeName(currentTownChatMegaphoneMode),
		[]byte("spam"), 0, 0, "rejected", "item 36 PVF identity mismatch", map[string]any{"material_item_id": 36})

	records, _, err := recorder.queryRecords(recordQuery{Kind: recordKindChat})
	if err != nil {
		t.Fatalf("query: %v", err)
	}
	if len(records) != 2 {
		t.Fatalf("record count = %d, want 2", len(records))
	}
	// 最新在前：第二条是被拒绝的那条喇叭。
	rejected := records[0]
	if rejected["result"] != "rejected" || rejected["mode_name"] != "服务器喇叭" {
		t.Fatalf("rejected record = %+v", rejected)
	}
	if rejected["reason"] != "item 36 PVF identity mismatch" {
		t.Fatalf("rejection reason missing: %+v", rejected)
	}
	if rejected["material_item_id"] != float64(36) {
		t.Fatalf("material item missing: %+v", rejected)
	}
	if rejected["message_hex"] != "7370616d" {
		t.Fatalf("rejected message hex = %v, want 7370616d", rejected["message_hex"])
	}
	delivered := records[1]
	if delivered["message"] != "你好" || delivered["message_len"] != float64(2) || delivered["message_bytes"] != float64(4) {
		t.Fatalf("delivered record = %+v", delivered)
	}
	if delivered["recipients"] != float64(3) || delivered["channel_id"] != float64(508) {
		t.Fatalf("audience fields = %+v", delivered)
	}
	if delivered["character_id"] != "21" || delivered["account_id"] != "dnf:77" {
		t.Fatalf("identity fields = %+v", delivered)
	}
	if delivered["scope"] != "visible_town_scene" {
		t.Fatalf("scope field = %+v", delivered)
	}
}

// TestRecordDungeonFields 覆盖副本记录的进入/离开字段，并确认离开记录会带上
// 生命周期结束原因与"是否通关"——排障时这两项是第一个要看的。
func TestRecordDungeonFields(t *testing.T) {
	recorder, _ := newTestRecorder(t)
	service := &Service{}
	service.records.Store(recorder)

	runtime := &runtimeDungeonState{
		Dungeon: worldmap.Dungeon{
			ID:       11006,
			Path:     "dungeon/tutorial.stk",
			Metadata: worldmap.DungeonMetadata{Name: "洛兰的陷落"},
		},
		MazeIndex:       2,
		startedAt:       time.Now().Add(-90 * time.Second),
		lifecycleToken:  777,
		clearKillNormal: 12,
		clearKillBoss:   1,
	}
	runtime.Request.DungeonID = 11006
	runtime.Request.Difficulty = 3
	session := &gameSession{accountID: "dnf:9"}
	session.selectedCharacterID = 33

	service.recordDungeonEnter(session, runtime)
	runtime.ordinaryFinalRoomClearAccepted = true
	runtime.settlementEntrySent = true
	service.recordDungeonExit(session, runtime, "completed_town_return")

	records, _, err := recorder.queryRecords(recordQuery{Kind: recordKindDungeon})
	if err != nil {
		t.Fatalf("query: %v", err)
	}
	if len(records) != 2 {
		t.Fatalf("record count = %d, want 2", len(records))
	}
	exit := records[0]
	if exit["event"] != "exit" || exit["event_name"] != "离开副本" {
		t.Fatalf("exit record = %+v", exit)
	}
	if exit["exit_reason"] != "completed_town_return" || exit["cleared"] != true {
		t.Fatalf("exit reason/cleared = %+v", exit)
	}
	if exit["dungeon_id"] != float64(11006) || exit["dungeon_name"] != "洛兰的陷落" {
		t.Fatalf("dungeon identity = %+v", exit)
	}
	if exit["difficulty"] != float64(3) || exit["maze_index"] != float64(2) {
		t.Fatalf("dungeon selection = %+v", exit)
	}
	if exit["killed_normal"] != float64(12) || exit["killed_boss"] != float64(1) {
		t.Fatalf("kill tally = %+v", exit)
	}
	if elapsed, ok := exit["elapsed_seconds"].(float64); !ok || elapsed < 89 {
		t.Fatalf("elapsed_seconds = %v", exit["elapsed_seconds"])
	}
	enter := records[1]
	if enter["event"] != "enter" || enter["event_name"] != "进入副本" {
		t.Fatalf("enter record = %+v", enter)
	}
	if enter["lifecycle_token"] != float64(777) || enter["character_id"] != "33" {
		t.Fatalf("enter identity = %+v", enter)
	}
	// 进入记录不该带"离开"专有字段，否则同一个字段在两类记录里含义不同。
	if _, present := enter["exit_reason"]; present {
		t.Fatalf("enter record carries exit fields: %+v", enter)
	}
}

// TestRecordCeraSpendFields 覆盖点券消费记录的公共字段（商城与金库升级两条路
// 都经过 recordCeraSpend）。
func TestRecordCeraSpendFields(t *testing.T) {
	recorder, _ := newTestRecorder(t)
	service := &Service{}
	service.records.Store(recorder)
	session := &gameSession{accountID: "dnf:5"}
	session.selectedCharacterID = 88

	service.recordAlignedCeraSpend(session, alignedcmd.Result{
		Operation:     "upgrade_account_cargo",
		CeraSpent:     2000,
		CeraAfter:     1000,
		CeraSpentNote: "金库升级（账号金库升到 16 阶段）",
	})
	// 没有消费的命令不得留下记录。
	service.recordAlignedCeraSpend(session, alignedcmd.Result{Operation: "deposit_money", CeraAfter: 3000})

	records, _, err := recorder.queryRecords(recordQuery{Kind: recordKindCera})
	if err != nil {
		t.Fatalf("query: %v", err)
	}
	if len(records) != 1 {
		t.Fatalf("record count = %d, want 1", len(records))
	}
	record := records[0]
	if record["source"] != "aligned_command" || record["operation"] != "upgrade_account_cargo" {
		t.Fatalf("record = %+v", record)
	}
	if record["cera_spent"] != float64(2000) || record["cera_before"] != float64(3000) || record["cera_after"] != float64(1000) {
		t.Fatalf("cera fields = %+v", record)
	}
	if record["character_id"] != "88" || record["account_id"] != "dnf:5" {
		t.Fatalf("identity fields = %+v", record)
	}
}
