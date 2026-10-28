// 本文件按当前 DNF.exe 的读取顺序构造攻坚队通知包体。
package raid

import (
	"errors"
	"unicode/utf16"
)

const (
	// Current DNF.exe notification ids. The complete raid object and mode=3
	// roster refresh share the 0x024F handler; live server traces also pin the
	// timer pair to 0x0254/0x0255. Keep these IDs aligned with the current EXE,
	// rather than the older one-byte prepare packet layout.
	RaidSymbolMsgID        uint16 = 0x0247
	RaidParticipationMsgID uint16 = 0x0248
	RaidDungeonStateMsgID  uint16 = 0x0249
	RaidDungeonChangeMsgID uint16 = 0x024a
	RaidStateMsgID         uint16 = 0x024b
	RaidMessageMsgID       uint16 = 0x024c
	RaidMemberRefreshMsgID uint16 = 0x024f
	RaidObjectMsgID        uint16 = RaidMemberRefreshMsgID
	RaidWaitingListMsgID   uint16 = 0x0250
	// Current EXE sub_1D59170 is the 0x0251 waiting-row delta reader. It
	// consumes op:u8, member:u16 and, for op=0, state:u8.
	RaidWaitingChangeMsgID uint16 = 0x0251
	// Current EXE sub_1D67770 is the bodyless 0x0252 preparation notifier. It
	// constructs the native Raid preparation panel after state/timer data has
	// been staged. 0x0253 belongs to the unrelated user-info structure path.
	RaidPrepareStartMsgID uint16 = 0x0252
	RaidTimerMsgID        uint16 = 0x0254
	RaidRemainTimeMsgID   uint16 = 0x0255
	RaidEntryCostMsgID    uint16 = 0x0256
	// RaidInfoMsgID is the current class-0 response to C2S REQUEST_RAID_INFO.
	RaidInfoMsgID uint16 = 1359
	// RaidDirectoryReadyMsgID is the current EXE's team-channel directory
	// initialization notification. Its registration at sub_344AAD8 binds
	// opcode 0x04CC to sub_34463B0, which consumes one reserved byte and calls
	// sub_17B1890 to initialize the public-directory cooldown timestamp.
	RaidDirectoryReadyMsgID uint16 = 1228
	// Current EXE registration in sub_2262A00 maps these consecutive IDs to
	// three different handlers: 0x0257 is the bodyless movie transition,
	// 0x0258 consumes the nested reward list, and 0x0259 consumes the fixed
	// ten-byte Raid result. 0x025A is an unrelated nine-byte notification.
	RaidMovieMsgID        uint16 = 0x0257
	RaidRewardListMsgID   uint16 = 0x0258
	RaidResultMsgID       uint16 = 0x0259
	RaidBuffMsgID         uint16 = 0x0279
	RaidMonsterStateMsgID uint16 = 0x027a
	// The current EXE routes the raid object, its roster, and its removal through
	// one 0x024F handler that reads u32 raid_key then u32 operation. The 86JP C#
	// reference used the same layout on its RAID_MODIFY opcode with 0=create/full
	// refresh, 1=remove, 2=object update and 3=member list, so 0 and 3 were the
	// only operations this server sent until now.
	RaidMemberRefreshMode0 uint32 = 0
	RaidMemberRefreshMode1 uint32 = 1
	RaidMemberRefreshMode3 uint32 = 3
	MaxAttackPartyMembers         = 20

	raidMemberNameMaxBytes = 256
	// raidTitleMaxBytes is the title reader's own capacity (sub_3457D20 is
	// called with 0x100 for the object title).
	raidTitleMaxBytes = 0x100
	maxRaidByteCount  = 255
)

// MemberRecord 是 sub_17B6EF0 读取的一条攻坚队成员记录。
type MemberRecord struct {
	CharKey uint16
	Field4  byte
	Name    string
	// RawNameBytes is the DSTR payload in the encoding expected by the
	// current client member-info handler (GB18030 for character names). It
	// takes precedence over Name and the legacy UTF-16 field.
	RawNameBytes []byte
	// RawNameUTF16LE is retained for older callers and captures the native
	// compatibility record used by the previous client path.
	RawNameUTF16LE []byte
	// Field40 is the base job. Current EXE sub_1B4ED60 passes it as the first
	// argument to the native profession-name resolver sub_249D7F0.
	Field40 byte
	// Field44 is the persisted grow_type byte. The client splits its low nibble
	// into the class branch and bits 4..6 into the awakening stage.
	Field44           byte
	GroupIndex        byte
	SlotOrder         byte
	Field48           uint32
	Field52           byte
	Field53           byte
	Field56           byte
	Field60           uint32
	Field64           uint16
	Field66BoolSource uint32
}

// MemberRefresh 保存 0x24F mode=3 的成员名单刷新数据。
type MemberRefresh struct {
	RaidKey uint32
	Members []MemberRecord
}

// RaidObjectMode0 is the complete object consumed by current EXE
// sub_1D71CA0 -> sub_17B8910 -> sub_17BF120 during initial creation.
// The native 90CN creation trace contains the leader record, a byte-sized
// member count, and that many compatibility member records. The client uses
// the latter to populate the initial member-info rows before the mode=3 roster
// refresh arrives, so keep them on the wire even though mode=3 is the canonical
// subsequent refresh path.
type RaidObjectMode0 struct {
	RaidKey uint32
	// TitleBytes is the raid's own name as a raw dstr payload in the client's
	// narrow text encoding (GB18030), byte-for-byte what the client sent in the
	// 664/670 request body. It must NOT be converted to UTF-16: the client reads
	// this field with sub_3457D20(dst, 0x100, GetString(0x29D)), which is the
	// same length-prefixed narrow-string reader every other dstr on this wire
	// uses, and it widens the payload one byte at a time (0x3457D20 -> 0x34D9CA0).
	//
	// Sending UTF-16LE here is the "纯数字攻坚队名只显示第一位 / 汉字后跟数字乱码"
	// bug: the reader consumes the payload as local-code-page bytes, so "1111"
	// (31 00 31 00 ...) stops at the first NUL after "1", and "礼白1"
	// (3C 79 7D 76 31 00) renders as "<y}v1". Verified against the 86 reference
	// server, which carries the request's own bytes through WriteRawDstr
	// unchanged (RaidPacketBuilder.WriteRaidObject -> GamePacketWriter.WriteRawDstr).
	TitleBytes    []byte
	ObjectField24 byte
	ObjectField28 byte
	ObjectField30 byte
	ObjectField34 uint32
	ObjectField94 byte
	Leader        MemberRecord
	Members       []MemberRecord
}

// EntryCostRow is one current 0x0256 row. The current EXE consumes a member
// key, a one-byte material-present field, and a u16 value. ItemID/Value32 are
// retained only as a compatibility bridge for older callers.
type EntryCostRow struct {
	MemberKey uint16
	// Field/Value are retained for source compatibility with older callers.
	Field   byte
	Value   uint16
	ItemID  uint32
	Value32 uint32
}

type WaitingRow struct {
	MemberKey uint16
	State     byte
}

// RaidSymbolRow is one 0x0247 key/value row.
type RaidSymbolRow struct {
	Key   uint32
	Value uint32
}

// RaidParticipationRow is one native 0x0248 dungeon participation row.
// The current client reads a byte-sized member count followed by that many
// u16 member keys. MemberKeys/Members are alternative source representations.
type RaidParticipationRow struct {
	DungeonID  uint32
	Op         uint32
	MemberKeys []uint32
	// Deprecated wire shape retained for source compatibility.
	Field0  byte
	Members []uint16
}

// RaidDungeonStateRow is one dungeon/state pair in a 0x0249 snapshot.
type RaidDungeonStateRow struct {
	DungeonID uint32
	State     uint32
}

// RaidDungeonStateSnapshot is the current EXE's 0x0249 snapshot:
// u8 unknown0, u8 count, repeat {u32 dungeonID,u8 state}, u32 tail.
type RaidDungeonStateSnapshot struct {
	Unknown0      uint32
	Rows          []RaidDungeonStateRow
	TrailingValue uint32
	// Deprecated compatibility field. It is folded into Unknown0 only when
	// callers still use the old one-byte initializer.
	Field0 byte
}

// RaidDungeonChange is the complete fixed-width 0x024A body.
type RaidDungeonChange struct {
	DungeonID uint32
	State     byte
	Argument  byte
}

// RaidState is the current 0x024B body: state:u8, phase-index:u8.
type RaidState struct {
	State    byte
	Argument int8
}

// RaidTimer is the complete fixed-width 0x0254 body.
type RaidTimer struct {
	TimerType byte
	Value0    uint32
	Value1    uint32
	Value2    uint32
}

// RaidRemainTime is the complete fixed-width 0x0255 body.
type RaidRemainTime struct {
	TimerType byte
	Seconds   uint32
}

// RaidRewardEntry is one inner 0x0258 reward-list row. Field1 and the two
// values intentionally remain neutral until their business meanings are
// proved independently from the current EXE.
type RaidRewardEntry struct {
	Field1       byte
	ItemOrValue  uint32
	CountOrValue uint16
}

// RaidRewardGroup is one outer 0x0258 member reward group.
type RaidRewardGroup struct {
	MemberKey uint16
	Field0    byte
	Entries   []RaidRewardEntry
}

// RaidRewardList is the complete variable-width 0x0258 body.
type RaidRewardList struct {
	ResultType byte
	Groups     []RaidRewardGroup
}

// RaidResult is the complete fixed-width 0x0259 body. Only ResultType has a
// confirmed semantic name; the remaining fields retain their wire identity.
type RaidResult struct {
	ResultType byte
	Field1     byte
	Field2     uint32
	Field3     uint16
	Field4     byte
	Field5     byte
}

// RaidBuffEntry is one inner 0x0279 buff row.
type RaidBuffEntry struct {
	PartyIndex uint16
	MemberKey  uint16
	Value0     uint32
	Value1     uint32
}

// RaidBuffGroup is one outer 0x0279 buff group.
type RaidBuffGroup struct {
	Field0  byte
	Entries []RaidBuffEntry
}

// RaidMonsterStateGroup is one outer 0x027A group. The current EXE consumes
// both variable lists; their business meanings remain deliberately opaque.
type RaidMonsterStateGroup struct {
	PartyIndex uint16
	Members    []uint16
	Value0     uint32
	Value1     uint32
	Values     []uint32
}

var errRaidMemberTooMany = errors.New("raid member count exceeds 20")
var errRaidMemberNameInvalid = errors.New("raid member name utf16 bytes invalid")
var errRaidMemberNameTooLong = errors.New("raid member name utf16 bytes too long")
var errRaidByteCountOverflow = errors.New("raid byte-sized count exceeds 255")
var errRaidWireByteOverflow = errors.New("raid wire byte value exceeds 255")
var errRaidMemberKeyOverflow = errors.New("raid member key exceeds uint16")

// BuildRaidCommandSuccessBody constructs the exact successful ACK used by
// current 658 and 667: only the common success byte is present.
func BuildRaidCommandSuccessBody() []byte {
	return []byte{1}
}

// BuildRaidInfoResponseBody returns the empty notification consumed by the
// current DNF.exe class-0/op1359 handler sub_1D3A150 -> sub_A42AB0.
func BuildRaidInfoResponseBody() []byte { return nil }

// BuildRaidDirectoryReadyBody supplies the reserved byte consumed by the
// current DNF.exe op1228 handler before it initializes raid-directory state.
func BuildRaidDirectoryReadyBody() []byte { return []byte{0} }

// BuildCreateRaidResultBody constructs class1/op664 CREATE_RAID success.
// Current EXE sub_1D04340 consumes the common success byte followed by raid_key:u32.
func BuildCreateRaidResultBody(raidKey uint32) []byte {
	var writer packetWriter
	writer.writeByte(1)
	writer.writeUint32(raidKey)
	return writer.bytes()
}

// BuildForcedLeaveRaidResultBody constructs the class1/op665 result that tells the
// receiver it was removed from the raid instead of leaving on its own.
//
// sub_1D04A00 reads u8 result and then u8 flag. A non-zero flag pushes string
// 46791 "已经强制退出攻坚队。" and runs the forced-leave cleanup (sub_17C2640 /
// sub_17B16D0); both branches then close windows 0x75/0x77 and tear down the
// receiver's raid projection. The handler is entirely receiver-relative, so this
// packet means "you are out" and must never be sent to the leader who performed
// the kick -- doing that made the leader's own member list vanish and turned its
// browse window back into 参加攻坚队.
func BuildForcedLeaveRaidResultBody() []byte {
	return []byte{1, 1}
}

// BuildLeaveRaidResultBody constructs class1/op665 LEAVE_RAID success.
//
// The second byte is NOT "the raid ended". sub_1D04A00 reads u8 result and then
// u8 flag, and a non-zero flag pushes string 46791 "已经强制退出攻坚队。" on top of
// the forced-leave cleanup (sub_17C2640 / sub_17B16D0). Both branches close
// windows 0x75/0x77 and tear the receiver's raid projection down, so the flag only
// adds the "you were removed" wording. It was named raidEnded from the earlier
// reading and that is what made an ordinary self-leave pop the forced-removal
// message whenever the departure emptied the raid. Pass true only when the
// receiver really was removed against its will.
func BuildLeaveRaidResultBody(forciblyRemoved bool) []byte {
	var writer packetWriter
	writer.writeByte(1)
	if forciblyRemoved {
		writer.writeByte(1)
	} else {
		writer.writeByte(0)
	}
	return writer.bytes()
}

// BuildRaidObjectRemoveBody builds 0x024F mode=1: u32 raid_key, u32 operation and
// nothing else. It retires a raid object the client has already materialized.
//
// Reference: 86JP RaidPacketBuilder.BuildRaidRemove writes u32 raidId + u32 1 on
// RAID_MODIFY, and RaidHandler.HandleLeaveRaid sends it to the leaving member
// unconditionally and to every remaining member when the raid disbands. Without
// an equivalent the client keeps rendering a raid it has left, which is the
// observed "left the raid but the roster still lists me, then the window hangs"
// symptom: every roster push here iterates the surviving member list only, so
// the leaver was never told anything at all.
func BuildRaidObjectRemoveBody(raidKey uint32) []byte {
	var writer packetWriter
	writer.writeUint32(raidKey)
	writer.writeUint32(RaidMemberRefreshMode1)
	return writer.bytes()
}

// BuildRaidObjectMode0Body builds the initial object. Current EXE must receive
// this notification before the successful create ACK selects RaidKey.
func BuildRaidObjectMode0Body(object RaidObjectMode0) ([]byte, error) {
	var writer packetWriter
	writer.writeUint32(object.RaidKey)
	writer.writeUint32(RaidMemberRefreshMode0)
	if err := writeRaidObjectBase(&writer, object); err != nil {
		return nil, err
	}
	if err := validateRaidByteCount(len(object.Members)); err != nil {
		return nil, err
	}
	// sub_17BF120 reads this member count after the leader record, then consumes
	// exactly that many compatibility member records.
	writer.writeByte(byte(len(object.Members)))
	for _, member := range object.Members {
		if err := writeMemberRecord(&writer, member); err != nil {
			return nil, err
		}
	}
	return writer.bytes(), nil
}

// writeRaidObjectBase writes the object consumed by sub_17B8910. The same
// object is embedded after the mode header in op591 and directly in each
// op753 public-directory row.
func writeRaidObjectBase(writer *packetWriter, object RaidObjectMode0) error {
	title, err := validateRaidTitleBytes(object.TitleBytes)
	if err != nil {
		return err
	}
	writer.writeUint32(object.RaidKey)
	writer.writeRawDstr(title)
	writer.writeByte(object.ObjectField24)
	writer.writeByte(object.ObjectField28)
	writer.writeByte(object.ObjectField30)
	writer.writeUint32(object.ObjectField34)
	writer.writeByte(object.ObjectField94)
	return writeMemberRecord(writer, object.Leader)
}

// BuildRaidSymbolBody builds current 0x0247:
// u8 count + repeat {u32 key,u32 value}.
func BuildRaidSymbolBody(rows []RaidSymbolRow) ([]byte, error) {
	if err := validateRaidByteCount(len(rows)); err != nil {
		return nil, err
	}
	var writer packetWriter
	writer.writeByte(byte(len(rows)))
	for _, row := range rows {
		writer.writeUint32(row.Key)
		writer.writeUint32(row.Value)
	}
	return writer.bytes(), nil
}

// BuildRaidParticipationBody builds current 0x0248:
// u8 rowCount + repeat {u32 dungeonID,u8 operation,u8 memberCount,
// memberCount*u16 memberKey}.
func BuildRaidParticipationBody(rows []RaidParticipationRow) ([]byte, error) {
	if err := validateRaidByteCount(len(rows)); err != nil {
		return nil, errRaidByteCountOverflow
	}
	for _, row := range rows {
		if len(row.MemberKeys) > maxRaidByteCount || len(row.Members) > maxRaidByteCount {
			return nil, errRaidByteCountOverflow
		}
	}
	var writer packetWriter
	writer.writeByte(byte(len(rows)))
	for _, row := range rows {
		op := row.Op
		if op == 0 && row.Field0 != 0 {
			op = uint32(row.Field0)
		}
		if op > 0xff {
			return nil, errRaidWireByteOverflow
		}
		memberKeys := row.MemberKeys
		if len(memberKeys) == 0 && len(row.Members) != 0 {
			memberKeys = make([]uint32, len(row.Members))
			for index, memberKey := range row.Members {
				memberKeys[index] = uint32(memberKey)
			}
		}
		writer.writeUint32(row.DungeonID)
		writer.writeByte(byte(op))
		writer.writeByte(byte(len(memberKeys)))
		for _, memberKey := range memberKeys {
			if memberKey > 0xffff {
				return nil, errRaidMemberKeyOverflow
			}
			writer.writeUint16(uint16(memberKey))
		}
	}
	return writer.bytes(), nil
}

// BuildRaidDungeonStateBody builds current 0x0249:
// u8 unknown0,u8 count,repeat {u32 dungeonID,u8 state},u32 trailingValue.
func BuildRaidDungeonStateBody(snapshot RaidDungeonStateSnapshot) ([]byte, error) {
	if err := validateRaidByteCount(len(snapshot.Rows)); err != nil {
		return nil, err
	}
	var writer packetWriter
	unknown0 := snapshot.Unknown0
	if unknown0 == 0 && snapshot.Field0 != 0 {
		unknown0 = uint32(snapshot.Field0)
	}
	if unknown0 > 0xff {
		return nil, errRaidWireByteOverflow
	}
	writer.writeByte(byte(unknown0))
	writer.writeByte(byte(len(snapshot.Rows)))
	for _, row := range snapshot.Rows {
		if row.State > 0xff {
			return nil, errRaidWireByteOverflow
		}
		writer.writeUint32(row.DungeonID)
		writer.writeByte(byte(row.State))
	}
	writer.writeUint32(snapshot.TrailingValue)
	return writer.bytes(), nil
}

// BuildRaidDungeonChangeBody builds current 0x024A:
// u32 dungeonID,u8 state,u8 argument.
func BuildRaidDungeonChangeBody(change RaidDungeonChange) []byte {
	var writer packetWriter
	writer.writeUint32(change.DungeonID)
	writer.writeByte(change.State)
	writer.writeByte(change.Argument)
	return writer.bytes()
}

// BuildRaidStateBody builds current 0x024B: u8 state,u8 argument.
func BuildRaidStateBody(state RaidState) []byte {
	return []byte{state.State, byte(state.Argument)}
}

// BuildRaidTimerBody builds current 0x0254:
// u8 timerType,u32 value0,u32 value1,u32 value2.
func BuildRaidTimerBody(timer RaidTimer) []byte {
	var writer packetWriter
	writer.writeByte(timer.TimerType)
	writer.writeUint32(timer.Value0)
	writer.writeUint32(timer.Value1)
	writer.writeUint32(timer.Value2)
	return writer.bytes()
}

// BuildRaidRemainTimeBody builds current 0x0255: u8 timerType,u32 seconds.
func BuildRaidRemainTimeBody(timer RaidRemainTime) []byte {
	var writer packetWriter
	writer.writeByte(timer.TimerType)
	writer.writeUint32(timer.Seconds)
	return writer.bytes()
}

// BuildRaidMovieBody builds current 0x0257, whose body is empty.
func BuildRaidMovieBody() []byte {
	return []byte{}
}

// BuildRaidRewardListBody builds current 0x0258:
// u8 resultType,u8 groupCount, then the current nested member/reward rows.
func BuildRaidRewardListBody(rewards RaidRewardList) ([]byte, error) {
	if err := validateRaidByteCount(len(rewards.Groups)); err != nil {
		return nil, err
	}
	for _, group := range rewards.Groups {
		if err := validateRaidByteCount(len(group.Entries)); err != nil {
			return nil, err
		}
	}
	var writer packetWriter
	writer.writeByte(rewards.ResultType)
	writer.writeByte(byte(len(rewards.Groups)))
	for _, group := range rewards.Groups {
		writer.writeUint16(group.MemberKey)
		writer.writeByte(group.Field0)
		writer.writeByte(byte(len(group.Entries)))
		for _, entry := range group.Entries {
			writer.writeByte(entry.Field1)
			writer.writeUint32(entry.ItemOrValue)
			writer.writeUint16(entry.CountOrValue)
		}
	}
	return writer.bytes(), nil
}

// BuildRaidResultBody builds current 0x0259's fixed ten-byte body.
func BuildRaidResultBody(result RaidResult) []byte {
	var writer packetWriter
	writer.writeByte(result.ResultType)
	writer.writeByte(result.Field1)
	writer.writeUint32(result.Field2)
	writer.writeUint16(result.Field3)
	writer.writeByte(result.Field4)
	writer.writeByte(result.Field5)
	return writer.bytes()
}

// BuildRaidBuffBody builds current 0x0279:
// u8 groupCount + repeat {u8 field0,u8 entryCount,repeat four-field entry}.
func BuildRaidBuffBody(groups []RaidBuffGroup) ([]byte, error) {
	if err := validateRaidByteCount(len(groups)); err != nil {
		return nil, err
	}
	for _, group := range groups {
		if err := validateRaidByteCount(len(group.Entries)); err != nil {
			return nil, err
		}
	}
	var writer packetWriter
	writer.writeByte(byte(len(groups)))
	for _, group := range groups {
		writer.writeByte(group.Field0)
		writer.writeByte(byte(len(group.Entries)))
		for _, entry := range group.Entries {
			writer.writeUint16(entry.PartyIndex)
			writer.writeUint16(entry.MemberKey)
			writer.writeUint32(entry.Value0)
			writer.writeUint32(entry.Value1)
		}
	}
	return writer.bytes(), nil
}

// BuildRaidMonsterStateBody builds current 0x027A, preserving both nested
// byte-sized counts and every fixed-width value in native read order.
func BuildRaidMonsterStateBody(groups []RaidMonsterStateGroup) ([]byte, error) {
	if err := validateRaidByteCount(len(groups)); err != nil {
		return nil, err
	}
	for _, group := range groups {
		if err := validateRaidByteCount(len(group.Members)); err != nil {
			return nil, err
		}
		if err := validateRaidByteCount(len(group.Values)); err != nil {
			return nil, err
		}
	}
	var writer packetWriter
	writer.writeByte(byte(len(groups)))
	for _, group := range groups {
		writer.writeUint16(group.PartyIndex)
		writer.writeByte(byte(len(group.Members)))
		for _, memberKey := range group.Members {
			writer.writeUint16(memberKey)
		}
		writer.writeUint32(group.Value0)
		writer.writeUint32(group.Value1)
		writer.writeByte(byte(len(group.Values)))
		for _, value := range group.Values {
			writer.writeUint32(value)
		}
	}
	return writer.bytes(), nil
}

// BuildEmptyRaidMonsterStateBody is the wire encoding of a current 0x027A
// zero-row snapshot. It is retained for codec tests and for a future dedicated
// raid-service owner. It must not be used as a generic initialization packet:
// current EXE sub_1D50990 treats it as an authoritative replacement and clears
// its native objective/monster-state container.
func BuildEmptyRaidMonsterStateBody() []byte {
	return []byte{0}
}

// BuildEmptyRaidBuffBody is the current 0x0279 empty snapshot.
func BuildEmptyRaidBuffBody() []byte {
	return []byte{0}
}

// BuildEntryCostBody builds current 0x0256: u32 count followed by
// {u16 memberKey,u8 field,u16 value} rows.
func BuildEntryCostBody(rows []EntryCostRow) []byte {
	var writer packetWriter
	writer.writeUint32(uint32(len(rows)))
	for _, row := range rows {
		writer.writeUint16(row.MemberKey)
		field := row.Field
		value := row.Value
		if field == 0 && (row.ItemID != 0 || row.Value32 != 0) {
			field = 1
			if value == 0 {
				value = uint16(row.Value32)
			}
		}
		writer.writeByte(field)
		writer.writeUint16(value)
	}
	return writer.bytes()
}

// BuildWaitingListBody builds current 0x0250: u16 count followed by
// {u16 memberKey,u8 state} rows.
func BuildWaitingListBody(rows []WaitingRow) []byte {
	var writer packetWriter
	writer.writeUint16(uint16(len(rows)))
	for _, row := range rows {
		writer.writeUint16(row.MemberKey)
		writer.writeByte(row.State)
	}
	return writer.bytes()
}

// BuildWaitingChangeBody builds current 0x0251. op=0 carries a state byte;
// op=1 removes the member and therefore ends after memberKey.
func BuildWaitingChangeBody(op byte, row WaitingRow) []byte {
	var writer packetWriter
	writer.writeByte(op)
	writer.writeUint16(row.MemberKey)
	if op == 0 {
		writer.writeByte(row.State)
	}
	return writer.bytes()
}

// BuildMemberRefreshMode3Body 构造 0x24F mode=3：u32 raid_key, u32 mode, u8 count, repeat member。
func BuildMemberRefreshMode3Body(refresh MemberRefresh) ([]byte, error) {
	if len(refresh.Members) > MaxAttackPartyMembers {
		return nil, errRaidMemberTooMany
	}
	var writer packetWriter
	writer.writeUint32(refresh.RaidKey)
	writer.writeUint32(RaidMemberRefreshMode3)
	writer.writeByte(byte(len(refresh.Members)))
	for _, member := range refresh.Members {
		if err := writeMemberRecord(&writer, member); err != nil {
			return nil, err
		}
	}
	return writer.bytes(), nil
}

func writeMemberRecord(writer *packetWriter, member MemberRecord) error {
	nameBytes, err := raidWstrBytes(member)
	if err != nil {
		return err
	}
	writer.writeUint16(member.CharKey)
	writer.writeByte(member.Field4)
	writer.writeRawDstr(nameBytes)
	writer.writeByte(member.Field40)
	writer.writeByte(member.Field44)
	writer.writeByte(member.GroupIndex)
	writer.writeByte(member.SlotOrder)
	writer.writeUint32(member.Field48)
	writer.writeByte(member.Field52)
	writer.writeByte(member.Field53)
	writer.writeByte(member.Field56)
	writer.writeUint32(member.Field60)
	writer.writeUint16(member.Field64)
	writer.writeUint32(member.Field66BoolSource)
	return nil
}

// raidWstrBytes picks the bytes for one member record's name dstr.
//
// The dstr this feeds is read by the same narrow reader as the object title
// (sub_3457D20 -> 0x34D9CA0 widens it from the local code page), so the payload
// MUST be client-encoding (GB18030) bytes. rawNameBytes is the only branch that
// satisfies that, and it is the only branch production takes: every live
// MemberRecord is built by runtimeRaidMemberRecord with RawNameBytes =
// rosterNameBytes(name). See tools\_rev_安徒恩_修复方案.md F15.
//
// Do not route a live member record through the two fallbacks below. They encode
// UTF-16LE, which this reader renders byte by byte -- the same mistake that made
// the raid window title show "1" for "1111" and "<y}v1" for "礼白1"
// (runtimeRaidTitleBytes in raid_online.go, and the already-fixed town scene
// object name in character_scene_object_tail.go writeCurrentSceneObjectName).
// They survive only for the legacy compatibility record exercised by body_test.go.
func raidWstrBytes(member MemberRecord) ([]byte, error) {
	if len(member.RawNameBytes) > 0 {
		return validateRaidNameBytes(member.RawNameBytes)
	}
	if len(member.RawNameUTF16LE) > 0 {
		return validateRaidWstrBytes(member.RawNameUTF16LE)
	}
	units := utf16.Encode([]rune(member.Name))
	if len(units) == 0 {
		return []byte{0, 0}, nil
	}
	out := make([]byte, 0, len(units)*2)
	for _, unit := range units {
		out = append(out, byte(unit), byte(unit>>8))
	}
	return validateRaidWstrBytes(out)
}

func validateRaidNameBytes(data []byte) ([]byte, error) {
	if len(data) >= raidMemberNameMaxBytes {
		return nil, errRaidMemberNameTooLong
	}
	return append([]byte(nil), data...), nil
}

func validateRaidWstrBytes(data []byte) ([]byte, error) {
	if len(data)%2 != 0 {
		return nil, errRaidMemberNameInvalid
	}
	if len(data) == 0 {
		data = []byte{0, 0}
	}
	if len(data) >= raidMemberNameMaxBytes {
		return nil, errRaidMemberNameTooLong
	}
	return append([]byte(nil), data...), nil
}

// validateRaidTitleBytes bounds the raid-name dstr without ever reshaping it.
//
// The only rule the client imposes is its reader's capacity: sub_3457D20 is
// called with 0x100, so a declared length at or above that makes it discard the
// payload and fall back to its own default string (669). The bytes themselves
// are handed through untouched -- no parity, no NUL padding, no re-encoding:
// GB18030 names are routinely an odd number of bytes ("礼白1" is five), so the
// old even-length requirement here rejected perfectly legal names, and the
// UTF-16 conversion that produced that requirement is exactly what garbled them.
//
// An empty title is left as an empty dstr, which is what the 86 reference sends
// for a raid created without a name; the client then substitutes its own default.
func validateRaidTitleBytes(data []byte) ([]byte, error) {
	if len(data) >= raidTitleMaxBytes {
		return nil, errRaidMemberNameTooLong
	}
	return append([]byte(nil), data...), nil
}

func validateRaidByteCount(count int) error {
	if count > maxRaidByteCount {
		return errRaidByteCountOverflow
	}
	return nil
}
