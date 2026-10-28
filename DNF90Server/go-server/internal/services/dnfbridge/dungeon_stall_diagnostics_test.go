package dnfbridge

import (
	"encoding/binary"
	"path/filepath"
	"strconv"
	"strings"
	"testing"
	"time"

	"longheng.io/server/internal/modules/dnf/dnfenum"
	dnfproto "longheng.io/server/internal/modules/dnf/protocol"
)

// 卡住诊断是"只打日志"的改动，所以测试要钉住两件事：
//  1. 判定逻辑（阈值 / 限频 / 恢复 / 上限淘汰）符合设计；
//  2. 既有 blocked 事件**一条不少、格式不变**，新增的 stuck/resumed 只在该出现时出现。
func TestDungeonStallTrackerEscalatesAtRetryThreshold(t *testing.T) {
	tracker := newDungeonStallTracker()
	now := time.Now()
	for attempt := 1; attempt < dungeonStallRetryThreshold; attempt++ {
		escalate, entry := tracker.noteBlocked(dungeonStallKindMonsterDeath, 69014, "non_hostile_ai_retirement_rejected", now)
		if escalate {
			t.Fatalf("attempt %d escalated before the threshold", attempt)
		}
		if entry.streak != attempt {
			t.Fatalf("attempt %d streak=%d", attempt, entry.streak)
		}
	}
	escalate, entry := tracker.noteBlocked(dungeonStallKindMonsterDeath, 69014, "non_hostile_ai_retirement_rejected", now)
	if !escalate {
		t.Fatalf("attempt %d must escalate", dungeonStallRetryThreshold)
	}
	if entry.streak != dungeonStallRetryThreshold || entry.lastReason != "non_hostile_ai_retirement_rejected" {
		t.Fatalf("escalated entry = %+v", entry)
	}
}

func TestDungeonStallTrackerRateLimitsRepeatedEscalation(t *testing.T) {
	tracker := newDungeonStallTracker()
	now := time.Now()
	for i := 0; i < dungeonStallRetryThreshold; i++ {
		tracker.noteBlocked(dungeonStallKindMonsterDeath, 501, "blocked", now)
	}
	// 阈值刚过：10 秒内不再刷第二条（客户端 20~60ms 重发一次，不限频会被刷屏）。
	if escalate, _ := tracker.noteBlocked(dungeonStallKindMonsterDeath, 501, "blocked", now.Add(time.Second)); escalate {
		t.Fatal("escalated again inside the repeat interval")
	}
	if escalate, _ := tracker.noteBlocked(dungeonStallKindMonsterDeath, 501, "blocked", now.Add(dungeonStallRepeatInterval-time.Millisecond)); escalate {
		t.Fatal("escalated again just before the repeat interval elapsed")
	}
	escalate, entry := tracker.noteBlocked(dungeonStallKindMonsterDeath, 501, "blocked", now.Add(dungeonStallRepeatInterval))
	if !escalate {
		t.Fatal("must re-report after the repeat interval")
	}
	if entry.streak != dungeonStallRetryThreshold+3 {
		t.Fatalf("streak=%d want %d", entry.streak, dungeonStallRetryThreshold+3)
	}
}

// 恢复：op38 真的发出并命中该目标时，本轮计量清零并补一条 resumed。
func TestDungeonStallTrackerResumeClearsStreak(t *testing.T) {
	tracker := newDungeonStallTracker()
	now := time.Now()
	for i := 0; i < dungeonStallRetryThreshold; i++ {
		tracker.noteBlocked(dungeonStallKindMonsterDeath, 1234, "blocked", now)
	}
	resumed, entry := tracker.noteDeathResponse(1234, now.Add(time.Second))
	if !resumed {
		t.Fatal("an escalated streak must report a resume")
	}
	if entry.streak != dungeonStallRetryThreshold {
		t.Fatalf("resume snapshot streak=%d", entry.streak)
	}
	// 清零后重新计数：不会接着旧的 streak 继续升级。
	escalate, fresh := tracker.noteBlocked(dungeonStallKindMonsterDeath, 1234, "blocked", now.Add(2*time.Second))
	if escalate {
		t.Fatal("a fresh streak must not escalate on its first attempt")
	}
	if fresh.streak != 1 {
		t.Fatalf("fresh streak=%d want 1", fresh.streak)
	}
	// 未升级过的单次拒绝被回答：静默恢复，不打事件。
	if resumed, _ := tracker.noteDeathResponse(1234, now.Add(3*time.Second)); resumed {
		t.Fatal("a non-escalated streak must not report a resume")
	}
}

// 已知边界：op38 只能寻址 u16 目标，key 超过 0xFFFF 的目标永远收不到应答，
// 因此别的目标的 op38 不能把它误判成"恢复了"。
func TestDungeonStallTrackerDoesNotResumeUnaddressableKey(t *testing.T) {
	tracker := newDungeonStallTracker()
	now := time.Now()
	unaddressable := uint32(69014) // > 0xFFFF，线上真实样本（变量而非常量：要看截断后的行为）
	for i := 0; i < dungeonStallRetryThreshold; i++ {
		tracker.noteBlocked(dungeonStallKindMonsterDeath, unaddressable, "non_hostile_ai_retirement_rejected", now)
	}
	if resumed, _ := tracker.noteDeathResponse(uint16(unaddressable), now.Add(time.Second)); resumed {
		t.Fatal("a truncated target key must not clear an unaddressable key")
	}
	// 计量没有被清掉：继续累加（这一步在限频窗口内，所以不再重复升级）。
	escalate, entry := tracker.noteBlocked(dungeonStallKindMonsterDeath, unaddressable, "non_hostile_ai_retirement_rejected", now.Add(2*time.Second))
	if !escalate {
		// 阈值那一次已经升级过，10 秒窗口内不该再打一条。
		if entry.streak != dungeonStallRetryThreshold+1 {
			t.Fatalf("streak=%d, want %d", entry.streak, dungeonStallRetryThreshold+1)
		}
	}
	if entry.attempts != dungeonStallRetryThreshold+1 {
		t.Fatalf("attempts=%d, want %d", entry.attempts, dungeonStallRetryThreshold+1)
	}
}

func TestDungeonStallTrackerEvictsOldestKeyBeyondCap(t *testing.T) {
	tracker := newDungeonStallTracker()
	now := time.Now()
	for i := 0; i < dungeonStallMaxKeys+8; i++ {
		tracker.noteBlocked(dungeonStallKindSettlement, uint32(1000+i), "blocked", now.Add(time.Duration(i)*time.Millisecond))
	}
	tracker.mu.Lock()
	tracked := len(tracker.keys)
	tracker.mu.Unlock()
	if tracked > dungeonStallMaxKeys {
		t.Fatalf("tracked %d keys, cap is %d", tracked, dungeonStallMaxKeys)
	}
	if tracked == 0 {
		t.Fatal("eviction dropped everything")
	}
}

func TestDungeonStallReasonFromFields(t *testing.T) {
	cases := []struct {
		name   string
		fields []any
		want   string
	}{
		{"reason first", []any{"reason", "a", "object_key", 1}, "a"},
		{"reason later", []any{"dungeon_id", 247, "reason", "b"}, "b"},
		{"absent", []any{"object_key", 1}, ""},
		{"non string value", []any{"reason", 7}, ""},
		{"dangling key", []any{"reason"}, ""},
	}
	for _, testCase := range cases {
		if got := dungeonStallReasonFromFields(testCase.fields); got != testCase.want {
			t.Errorf("%s: got %q want %q", testCase.name, got, testCase.want)
		}
	}
}

func TestUpperSendDiagnosticsExtractsOpcodeAndTarget(t *testing.T) {
	addressable := uint16(3478)
	death := buildTestUpperPacket(uint16(dnfenum.CmdPacketNotifyDieMonster), addressable, 7)
	msgID, target, ok := upperSendDiagnostics(death)
	if !ok || msgID != uint16(dnfenum.CmdPacketNotifyDieMonster) || target != addressable {
		t.Fatalf("op38: msgID=%d target=%d ok=%t", msgID, target, ok)
	}
	other := buildTestUpperPacket(45, 3, 8)
	if msgID, target, ok := upperSendDiagnostics(other); !ok || msgID != 45 || target != 0 {
		t.Fatalf("op45: msgID=%d target=%d ok=%t", msgID, target, ok)
	}
	if _, _, ok := upperSendDiagnostics([]byte{1, 2}); ok {
		t.Fatal("a runt packet must be ignored")
	}
}

// 端到端（写进加密包日志再解开读）：既有 blocked 事件一条不少、格式不变，
// stuck 只在阈值那一次出现；目标不可寻址时不得误报 resumed。
func TestDungeonStallDiagnosticsReachTheEncryptedPacketLog(t *testing.T) {
	packetLogTestKeyEnv(t)
	anchor := filepath.Join(t.TempDir(), "packet_log.txt")
	logger, err := openPacketLogger(anchor)
	if err != nil {
		t.Fatalf("open packet logger: %v", err)
	}
	service := &Service{}
	service.packetLog.Store(logger)
	session := &gameSession{connID: "game-000123", stall: newDungeonStallTracker()}

	const objectKey = uint32(69014) // > 0xFFFF：op38 无法寻址，必须一直是"卡住"
	for i := 0; i < dungeonStallRetryThreshold; i++ {
		service.logDungeonStallBlocked(session, dungeonStallKindMonsterDeath,
			"game-dungeon-monster-death-blocked", objectKey,
			"object_key", objectKey,
			"reason", "non_hostile_ai_retirement_rejected")
	}
	// 另一条 op38（目标是 1234）不能把 69014 的计量清掉。
	service.noteUpperSendDiagnostics(session, buildTestUpperPacket(uint16(dnfenum.CmdPacketNotifyDieMonster), 1234, 9))
	if err := logger.Close(); err != nil {
		t.Fatalf("close packet logger: %v", err)
	}

	text := packetLogPlaintext(t, anchor)
	if got := strings.Count(text, "EVENT kind=game-dungeon-monster-death-blocked"); got != dungeonStallRetryThreshold {
		t.Fatalf("existing blocked events = %d, want %d（既有事件必须一条不少）\n%s", got, dungeonStallRetryThreshold, text)
	}
	if got := strings.Count(text, "EVENT kind=game-dungeon-monster-death-stuck"); got != 1 {
		t.Fatalf("stuck events = %d, want 1\n%s", got, text)
	}
	if got := strings.Count(text, "EVENT kind=game-dungeon-monster-death-resumed"); got != 0 {
		t.Fatalf("resumed events = %d, want 0（op38 目标是 1234，不是 69014）\n%s", got, text)
	}
	stuckLine := firstLineContaining(text, "game-dungeon-monster-death-stuck")
	for _, want := range []string{
		"retry_count=" + strconv.Itoa(dungeonStallRetryThreshold),
		"reason=non_hostile_ai_retirement_rejected",
		"object_key=69014",
		"dungeon_state=no_runtime",
		"conn_id=game-000123",
	} {
		if !strings.Contains(stuckLine, want) {
			t.Fatalf("stuck line missing %q:\n%s", want, stuckLine)
		}
	}
}

// 可寻址目标真的回了包时，必须补一条 resumed，并带上重试次数与原因。
func TestDungeonStallResumeIsLoggedForAddressableKey(t *testing.T) {
	packetLogTestKeyEnv(t)
	anchor := filepath.Join(t.TempDir(), "packet_log.txt")
	logger, err := openPacketLogger(anchor)
	if err != nil {
		t.Fatalf("open packet logger: %v", err)
	}
	service := &Service{}
	service.packetLog.Store(logger)
	session := &gameSession{connID: "game-000124", stall: newDungeonStallTracker()}

	const objectKey = uint32(3478) // 可寻址
	for i := 0; i < dungeonStallRetryThreshold; i++ {
		service.logDungeonStallBlocked(session, dungeonStallKindMonsterDeath,
			"game-dungeon-monster-death-blocked", objectKey,
			"object_key", objectKey,
			"reason", "non_hostile_ai_retirement_rejected")
	}
	service.noteUpperSendDiagnostics(session, buildTestUpperPacket(uint16(dnfenum.CmdPacketNotifyDieMonster), uint16(objectKey), 11))
	if err := logger.Close(); err != nil {
		t.Fatalf("close packet logger: %v", err)
	}

	text := packetLogPlaintext(t, anchor)
	if got := strings.Count(text, "EVENT kind=game-dungeon-monster-death-resumed"); got != 1 {
		t.Fatalf("resumed events = %d, want 1\n%s", got, text)
	}
	resumedLine := firstLineContaining(text, "game-dungeon-monster-death-resumed")
	for _, want := range []string{
		"retry_count=" + strconv.Itoa(dungeonStallRetryThreshold),
		"resolved_by=op38",
		"reason=non_hostile_ai_retirement_rejected",
		"object_key=" + strconv.Itoa(int(objectKey)),
	} {
		if !strings.Contains(resumedLine, want) {
			t.Fatalf("resumed line missing %q:\n%s", want, resumedLine)
		}
	}
}

// 结算通道（op123）走同一套聚合：key 用角色 id，事件名带自己的 kind。
func TestDungeonStallSettlementEscalatesWithCharacterKey(t *testing.T) {
	packetLogTestKeyEnv(t)
	anchor := filepath.Join(t.TempDir(), "packet_log.txt")
	logger, err := openPacketLogger(anchor)
	if err != nil {
		t.Fatalf("open packet logger: %v", err)
	}
	service := &Service{}
	service.packetLog.Store(logger)
	session := &gameSession{connID: "game-000125", selectedCharacterID: 397, stall: newDungeonStallTracker()}

	for i := 0; i < dungeonStallRetryThreshold; i++ {
		service.logDungeonStallBlocked(session, dungeonStallKindSettlement,
			"game-dungeon-character-statistic-blocked", uint32(session.selectedCharacterID),
			"char_id", session.selectedCharacterID,
			"reason", "completed_settlement_entry_owner_not_ready")
	}
	if err := logger.Close(); err != nil {
		t.Fatalf("close packet logger: %v", err)
	}
	text := packetLogPlaintext(t, anchor)
	if got := strings.Count(text, "EVENT kind=game-dungeon-character-statistic-blocked"); got != dungeonStallRetryThreshold {
		t.Fatalf("existing settlement events = %d, want %d\n%s", got, dungeonStallRetryThreshold, text)
	}
	if got := strings.Count(text, "EVENT kind=game-dungeon-settlement-entry-stuck"); got != 1 {
		t.Fatalf("settlement stuck events = %d, want 1\n%s", got, text)
	}
	line := firstLineContaining(text, "game-dungeon-settlement-entry-stuck")
	for _, want := range []string{
		"object_key=397",
		"reason=completed_settlement_entry_owner_not_ready",
		"retry_count=" + strconv.Itoa(dungeonStallRetryThreshold),
	} {
		if !strings.Contains(line, want) {
			t.Fatalf("settlement stuck line missing %q:\n%s", want, line)
		}
	}
}

// 回归：op39 的被拒分支是在 session.dungeon.mu **锁内**记日志的，升级事件因此绝不能
// 再去 Lock 同一把锁——Go 的 Mutex 不可重入，会当场自我死锁（会话读循环就再也醒不过来）。
// 这里就按"锁内调用"复现一次，看它能不能在超时前返回。
func TestDungeonStallEscalationDoesNotReenterDungeonLock(t *testing.T) {
	packetLogTestKeyEnv(t)
	anchor := filepath.Join(t.TempDir(), "packet_log.txt")
	logger, err := openPacketLogger(anchor)
	if err != nil {
		t.Fatalf("open packet logger: %v", err)
	}
	service := &Service{}
	service.packetLog.Store(logger)
	session := &gameSession{connID: "game-000126", stall: newDungeonStallTracker()}
	session.dungeon.mu.Lock()
	session.dungeon.runtime = &runtimeDungeonState{}

	done := make(chan struct{})
	go func() {
		defer close(done)
		for i := 0; i < dungeonStallRetryThreshold; i++ {
			service.logDungeonStallBlocked(session, dungeonStallKindMonsterDeath,
				"game-dungeon-monster-death-blocked", 42,
				"object_key", 42,
				"reason", "non_hostile_ai_retirement_rejected")
		}
	}()
	select {
	case <-done:
	case <-time.After(10 * time.Second):
		t.Fatal("升级事件在 dungeon.mu 锁内死锁：TryLock 契约被破坏")
	}
	session.dungeon.mu.Unlock()
	if err := logger.Close(); err != nil {
		t.Fatalf("close packet logger: %v", err)
	}
	text := packetLogPlaintext(t, anchor)
	if got := strings.Count(text, "EVENT kind=game-dungeon-monster-death-stuck"); got != 1 {
		t.Fatalf("stuck events = %d, want 1\n%s", got, text)
	}
	// 锁拿不到时也要给出 char_id，并由调用方现场字段兜底 dungeon_id。
	stuckLine := firstLineContaining(text, "game-dungeon-monster-death-stuck")
	if !strings.Contains(stuckLine, "char_id=") {
		t.Fatalf("stuck line lost the caller context:\n%s", stuckLine)
	}
}

func buildTestUpperPacket(msgID uint16, bodyFirst uint16, sequence uint16) []byte {
	packet := make([]byte, dnfproto.ChannelHeaderSize+2)
	packet[0] = dnfproto.DefaultChannelClassification
	binary.LittleEndian.PutUint16(packet[1:3], msgID)
	binary.LittleEndian.PutUint32(packet[3:7], uint32(len(packet)))
	binary.LittleEndian.PutUint16(packet[11:13], sequence)
	binary.LittleEndian.PutUint16(packet[dnfproto.ChannelHeaderSize:], bodyFirst)
	return packet
}

func firstLineContaining(text string, needle string) string {
	for _, line := range strings.Split(text, "\n") {
		if strings.Contains(line, needle) {
			return line
		}
	}
	return ""
}
