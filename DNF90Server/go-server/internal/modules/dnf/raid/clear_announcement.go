package raid

import "fmt"

// RaidClearAnnouncementMsgID is the class-0 notification the current EXE renders
// as the server's raid clear announcement.
//
// The handler sub_1D5D350 (registered for 626 / 0x272) reads
//
//	u32 mode, dstr raidName, dstr leaderName, u8 solo
//
// and then formats one of the client's own strings with those two arguments,
// drawing the result as a chat line in colour 0xFF0061FF by calling
// sub_2728160(text, 0, 0, 0, 0, 0x1E, 0xFF0061FF). That colour is ABGR --
// RGB(255, 97, 0), the orange the announcement uses:
//
//	solo == 0, mode 0 -> 60454 太棒了！ 热烈祝贺[%s]攻坚队(队长[%s])成功击杀第七使徒安徒恩！
//	solo == 0, mode 1 -> 66985 (Luke, team)
//	solo == 0, mode 2 -> 75517 强大的%s攻坚队队长%s和队员们破坏了量子时空传送站！ 恭喜！
//	solo == 1, mode 0 -> 46877 伟大的战士%s成功讨伐了安徒恩， 请大家祝贺。
//	solo == 1, mode 1 -> 67052 伟大的战士%s破坏了卢克的实验室。 恭喜！
//
// The solo strings take one argument and the client passes only leaderName, so a
// solo raid puts the warrior's own name there.
const RaidClearAnnouncementMsgID uint16 = 0x0272

// The three announcement kinds the client's own switch understands.
const (
	RaidClearKindAnton  uint32 = 0
	RaidClearKindLuke   uint32 = 1
	RaidClearKindSpiral uint32 = 2
)

// raidClearAnnouncementArgumentLimit is the reader's own bound: both arguments go
// through sub_3457D20 with a 0x100 cap, and a longer declared length makes it fall
// back to an empty string rather than truncating, so it is enforced here.
const raidClearAnnouncementArgumentLimit = 0x100

// RaidClearAnnouncement is one 0x0272 body.
type RaidClearAnnouncement struct {
	// Kind selects the raid whose string the client formats.
	Kind uint32
	// RaidName is the first %s: the raid's own name.
	RaidName []byte
	// LeaderName is the second %s: the raid leader, or the warrior in solo mode.
	LeaderName []byte
	// Solo switches to the single-player text (46877 / 67052), which ignores
	// RaidName.
	Solo bool
}

// BuildRaidClearAnnouncementBody encodes the 0x0272 body. The strings are raw
// client-encoding bytes exactly like every other dstr on this wire.
func BuildRaidClearAnnouncementBody(announcement RaidClearAnnouncement) ([]byte, error) {
	if len(announcement.RaidName) > raidClearAnnouncementArgumentLimit {
		return nil, fmt.Errorf("raid clear announcement name is %d bytes, the client reads at most %d",
			len(announcement.RaidName), raidClearAnnouncementArgumentLimit)
	}
	if len(announcement.LeaderName) > raidClearAnnouncementArgumentLimit {
		return nil, fmt.Errorf("raid clear announcement leader is %d bytes, the client reads at most %d",
			len(announcement.LeaderName), raidClearAnnouncementArgumentLimit)
	}
	writer := packetWriter{}
	writer.writeUint32(announcement.Kind)
	writer.writeRawDstr(announcement.RaidName)
	writer.writeRawDstr(announcement.LeaderName)
	if announcement.Solo {
		writer.writeByte(1)
	} else {
		writer.writeByte(0)
	}
	return writer.bytes(), nil
}
