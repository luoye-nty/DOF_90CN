package dnfbridge

import (
	"bytes"
	"context"
	"encoding/binary"
	"strconv"
	"strings"
	"testing"
	"time"

	"golang.org/x/text/encoding/simplifiedchinese"

	"longheng.io/server/internal/modules/dnf/dnfenum"
	dnfproto "longheng.io/server/internal/modules/dnf/protocol"
	dnfrepo "longheng.io/server/internal/modules/dnf/repository"
	dnfrepomemory "longheng.io/server/internal/modules/dnf/repository/memory"
)

// getUserInfoLegacyFrame builds the legacy GET_USERINFO frame the client emits
// for both the role-selector bootstrap and the name-change-record window.
func getUserInfoLegacyFrame(t *testing.T, body []byte) []byte {
	t.Helper()
	frame, err := dnfproto.BuildLatestGameTCP(
		byte(dnfenum.GameCmdCommand),
		uint16(dnfenum.GameTypeGetUserInfo),
		body,
		dnfproto.TransportOptions{},
	)
	if err != nil {
		t.Fatalf("build get userinfo frame: %v", err)
	}
	return frame
}

// wantNameChangeLogBody builds the exact window payload: rowCount followed by one
// variable-length row per record (u32 nameLen + GBK name + u8 serverId + u32
// unixTime). names are aligned with rows; an empty name leaves nameLen at 0, which
// is the shape the live client already renders with an empty 角色名 cell.
func wantNameChangeLogBody(rows [][2]uint32, names ...string) []byte {
	var writer packetWriter
	writer.writeByte(byte(len(rows)))
	for idx, row := range rows {
		name := ""
		if idx < len(names) {
			name = names[idx]
		}
		encoded := currentCharacterNameChangeLogNameBytes(name)
		writer.writeUint32(uint32(len(encoded)))
		writer.writeBytes(encoded)
		writer.writeByte(byte(row[0]))
		writer.writeUint32(row[1])
	}
	return writer.bytes()
}

func newNameChangeLogTestService(t *testing.T, repos dnfrepo.Group) *Service {
	t.Helper()
	return &Service{
		options:             options{accountPrefix: "dnf:"},
		characterStats:      testCharacterStatTable(t),
		adventureGroupTable: loadAdventureGroupTestTables(t),
		repositoryProvider: func() (dnfrepo.Group, bool) {
			return repos, true
		},
	}
}

// TestLegacyGetUserInfoMode5AnswersNameChangeLogInsteadOfRoster locks the crash
// fix: the 角色名称变更记录 window asks GET_USERINFO mode=5 and must receive the
// empty name-change-log notification. Answering it with the character roster
// crashed the live client, because the roster reader is a character-select
// reader.
func TestLegacyGetUserInfoMode5AnswersNameChangeLogInsteadOfRoster(t *testing.T) {
	repos := testRepositoryGroup()
	if err := repos.Account.Save(context.Background(), dnfrepo.AccountRecord{
		AccountID: "dnf:1",
		State:     "active",
	}); err != nil {
		t.Fatal(err)
	}
	repos.Character.(*fakeCharacterStore).records["1"] = dnfrepo.CharacterRecord{
		CharacterID: "1",
		AccountID:   "dnf:1",
		Slot:        0,
		Name:        "hero",
		Job:         "1",
		Level:       1,
	}
	service := newNameChangeLogTestService(t, repos)
	conn := &bufferConn{}
	session := &gameSession{conn: conn, connID: "name-change-log-test"}

	// u16 target user id + u8 mode=5, byte-for-byte the live request body.
	if err := service.handleGameFrame(session, getUserInfoLegacyFrame(t, []byte{0x01, 0x00, 0x05})); err != nil {
		t.Fatalf("handle mode-5 get userinfo: %v", err)
	}
	packet, rest := splitGameServerUpperPacket(t, conn.write.Bytes())
	if len(rest) != 0 {
		t.Fatalf("mode-5 reply left trailing bytes: %x", rest)
	}
	if packet.Header.Classification != 0 ||
		packet.Header.MsgID != currentCharacterNameChangeLogMsgID ||
		!bytes.Equal(packet.Body, []byte{0}) {
		t.Fatalf("mode-5 reply header=%+v body=%x, want class0/msg%d/{0}",
			packet.Header, packet.Body, currentCharacterNameChangeLogMsgID)
	}
	if packet.Header.MsgID == uint16(dnfenum.UpperMsgCharacterRoster) {
		t.Fatal("mode-5 was answered with the character roster")
	}
	conn.write.Reset()

	// Regression: the ordinary role-selector request (mode=2) still receives the
	// roster exactly as before.
	if err := service.handleGameFrame(session, getUserInfoLegacyFrame(t, []byte{0x01, 0x00, 0x02})); err != nil {
		t.Fatalf("handle mode-2 get userinfo: %v", err)
	}
	roster, rest := splitGameServerUpperPacket(t, conn.write.Bytes())
	if len(rest) != 0 {
		t.Fatalf("mode-2 reply left trailing bytes: %x", rest)
	}
	if roster.Header.Classification != 0 || roster.Header.MsgID != uint16(dnfenum.UpperMsgCharacterRoster) {
		t.Fatalf("mode-2 reply header=%+v, want roster", roster.Header)
	}
	assertCSharpRosterBody(t, roster.Body, 1)
}

// TestCharacterNameChangeLogRequestIgnoresOtherModes keeps the mode-5 branch
// from swallowing the neighbouring GET_USERINFO shapes.
func TestCharacterNameChangeLogRequestIgnoresOtherModes(t *testing.T) {
	service := &Service{}
	session := &gameSession{}
	for name, body := range map[string][]byte{
		"empty":        nil,
		"short":        {0x01, 0x00},
		"mode0":        {0x01, 0x00, 0x00},
		"mode2_roster": {0x01, 0x00, 0x02},
		"mode3_peer":   {0x01, 0x00, 0x03},
	} {
		handled, err := service.handleCurrentCharacterNameChangeLogRequest(session, body)
		if handled || err != nil {
			t.Fatalf("%s: handled=%t err=%v, want not handled", name, handled, err)
		}
	}
}

// TestCurrentCharacterRenameRecordsHistoryForTheNameChangeWindow covers the
// writer: a committed rename has to leave exactly one record that the window can
// project.
func TestCurrentCharacterRenameRecordsHistoryForTheNameChangeWindow(t *testing.T) {
	repositories := dnfrepomemory.NewMemoryGroup()
	ctx := context.Background()
	if err := repositories.Account.Save(ctx, dnfrepo.AccountRecord{AccountID: "account-1"}); err != nil {
		t.Fatal(err)
	}
	if err := repositories.Character.Save(ctx, dnfrepo.CharacterRecord{
		CharacterID: "19", AccountID: "account-1", Name: "OldName", Level: 50,
		Stats: map[string]int64{currentCharacterRenameEntitlementStat: 1},
	}); err != nil {
		t.Fatal(err)
	}

	now := time.Unix(1800000000, 0).UTC()
	if err := commitCurrentCharacterRename(ctx, repositories, "account-1", "19", "OldName", "NewName", 7, now); err != nil {
		t.Fatalf("commit rename: %v", err)
	}

	account, found, err := repositories.Account.Load(ctx, "account-1")
	if err != nil || !found {
		t.Fatalf("load account found=%t err=%v", found, err)
	}
	entries := currentCharacterRenameLogEntries(account, "19")
	if len(entries) != 1 || entries[0].Unix != now.Unix() || entries[0].OldName != "OldName" || entries[0].ServerID != 7 {
		t.Fatalf("rename history=%+v, want one record for OldName@%d on server 7", entries, now.Unix())
	}

	// The window body carries variable-length rows: u32 nameLen + GBK name +
	// u8 serverId + u32 unix.
	body := buildCurrentCharacterNameChangeLogBody(entries, 3)
	want := wantNameChangeLogBody([][2]uint32{{7, uint32(now.Unix())}}, "OldName")
	if !bytes.Equal(body, want) {
		t.Fatalf("window body=%x want %x", body, want)
	}
	if len(body) != 1+4+len("OldName")+1+4 {
		t.Fatalf("window body length=%d want name length plus the nine fixed bytes", len(body))
	}
	// Without a recorded name the four-byte length stays zero, which is the
	// shape already verified against the live client.
	legacy := buildCurrentCharacterNameChangeLogBody(
		[]currentCharacterRenameLogEntry{{Unix: now.Unix()}}, 3)
	if len(legacy) != 1+9 || legacy[5] != 3 {
		t.Fatalf("fallback window body=%x want server 3 at the row's server byte", legacy)
	}

	// A character without history keeps the well-formed empty list.
	if empty := buildCurrentCharacterNameChangeLogBody(nil, 3); !bytes.Equal(empty, []byte{0}) {
		t.Fatalf("empty window body=%x want 00", empty)
	}
}

func TestCurrentCharacterRenameLogPrunesOldestBeyondLimit(t *testing.T) {
	account := dnfrepo.AccountRecord{AccountID: "account-1", Metadata: map[string]string{}}
	base := time.Unix(1800000000, 0).UTC()
	for idx := 0; idx < currentCharacterRenameLogLimit+3; idx++ {
		appendCurrentCharacterRenameLog(&account, "19", "name-"+strconv.Itoa(idx), 4, base.Add(time.Duration(idx)*time.Minute))
	}
	entries := currentCharacterRenameLogEntries(account, "19")
	if len(entries) != currentCharacterRenameLogLimit {
		t.Fatalf("kept %d records, want %d", len(entries), currentCharacterRenameLogLimit)
	}
	if entries[0].OldName != "name-"+strconv.Itoa(currentCharacterRenameLogLimit+2) {
		t.Fatalf("newest record=%+v", entries[0])
	}
	// Unrelated metadata and other characters' records must survive the prune.
	account.Metadata["character_rename_reservation:4142"] = "1800000000"
	appendCurrentCharacterRenameLog(&account, "20", "other", 4, base)
	if _, kept := account.Metadata["character_rename_reservation:4142"]; !kept {
		t.Fatal("pruning dropped an unrelated metadata entry")
	}
	if got := currentCharacterRenameLogEntries(account, "20"); len(got) != 1 {
		t.Fatalf("other character history=%+v", got)
	}
}

// TestCurrentCharacterRenameLogToleratesRecordsWithoutServerID keeps older
// records ("<name>" only) loadable after the 大区 index joined the value.
func TestCurrentCharacterRenameLogToleratesRecordsWithoutServerID(t *testing.T) {
	account := dnfrepo.AccountRecord{AccountID: "account-1", Metadata: map[string]string{
		currentCharacterRenameLogKey("19", 1800000000): "OldName",
	}}
	entries := currentCharacterRenameLogEntries(account, "19")
	if len(entries) != 1 || entries[0].ServerID != 0 || entries[0].HasServerID || entries[0].OldName != "OldName" {
		t.Fatalf("legacy record=%+v", entries)
	}
	// The inline name pushes the server byte behind the name bytes, so its
	// offset is 1 + 4 + len(name) for a single row.
	body := buildCurrentCharacterNameChangeLogBody(entries, 5)
	serverOffset := 1 + 4 + len(currentCharacterNameChangeLogNameBytes("OldName"))
	if len(body) != serverOffset+5 || body[serverOffset] != 5 {
		t.Fatalf("legacy projection=%x want fallback server 5 at offset %d", body, serverOffset)
	}
	// A record that explicitly stores server index 0 keeps it.
	zero := []currentCharacterRenameLogEntry{{Unix: 1800000000, HasServerID: true, ServerID: 0, OldName: "x"}}
	if body := buildCurrentCharacterNameChangeLogBody(zero, 5); body[6] != 0 {
		t.Fatalf("explicit server 0 projection=%x", body)
	}
}

// TestLegacyGetUserInfoMode5ProjectsPersistedRenameHistory is the end-to-end
// shape: persisted history goes out as rows that carry the previous name inline,
// and foreign or unknown targets still answer with the empty list instead of the
// roster.
func TestLegacyGetUserInfoMode5ProjectsPersistedRenameHistory(t *testing.T) {
	repos := testRepositoryGroup()
	ctx := context.Background()
	const renamedAt = int64(1800000000)
	if err := repos.Account.Save(ctx, dnfrepo.AccountRecord{
		AccountID: "dnf:1",
		State:     "active",
		Metadata: map[string]string{
			// "<serverID>|<previous name>": this record remembers the 大区 it
			// happened on, so the configured region (3) is not used for it.
			currentCharacterRenameLogKey("1", renamedAt): "6|OldHero",
		},
	}); err != nil {
		t.Fatal(err)
	}
	repos.Character.(*fakeCharacterStore).records["1"] = dnfrepo.CharacterRecord{
		CharacterID: "1", AccountID: "dnf:1", Slot: 0, Name: "hero", Job: "1", Level: 1,
	}
	service := newNameChangeLogTestService(t, repos)
	service.options.regionID = 3
	conn := &bufferConn{}
	session := &gameSession{conn: conn, connID: "name-change-log-history", accountID: "dnf:1"}

	if err := service.handleGameFrame(session, getUserInfoLegacyFrame(t, []byte{0x01, 0x00, 0x05})); err != nil {
		t.Fatalf("handle mode-5 get userinfo: %v", err)
	}
	packet, rest := splitGameServerUpperPacket(t, conn.write.Bytes())
	if len(rest) != 0 {
		t.Fatalf("mode-5 reply left trailing bytes: %x", rest)
	}
	want := wantNameChangeLogBody([][2]uint32{{6, uint32(renamedAt)}}, "OldHero")
	if packet.Header.Classification != 0 || packet.Header.MsgID != currentCharacterNameChangeLogMsgID ||
		!bytes.Equal(packet.Body, want) {
		t.Fatalf("mode-5 reply header=%+v body=%x want %x", packet.Header, packet.Body, want)
	}
	// The name travels inside the row, so the two fields after it must land
	// immediately behind the name bytes: server id first, then the timestamp.
	nameLen := len(currentCharacterNameChangeLogNameBytes("OldHero"))
	rowStart := 1 + 4 + nameLen
	if packet.Body[rowStart] != 6 {
		t.Fatalf("server id must follow the inline name byte-for-byte, got %d in %x", packet.Body[rowStart], packet.Body)
	}
	if got := binary.LittleEndian.Uint32(packet.Body[rowStart+1 : rowStart+5]); got != uint32(renamedAt) {
		t.Fatalf("timestamp must follow the server id, got %d want %d", got, renamedAt)
	}
	conn.write.Reset()

	// Another player inspecting this account's character sees the same record:
	// the window is per target character, not per viewer.
	repos.Character.(*fakeCharacterStore).records["2"] = dnfrepo.CharacterRecord{
		CharacterID: "2", AccountID: "dnf:2", Slot: 1, Name: "someone", Job: "1", Level: 1,
	}
	if err := repos.Account.Save(ctx, dnfrepo.AccountRecord{
		AccountID: "dnf:2",
		State:     "active",
	}); err != nil {
		t.Fatal(err)
	}
	if err := service.handleGameFrame(session, getUserInfoLegacyFrame(t, []byte{0x02, 0x00, 0x05})); err != nil {
		t.Fatalf("handle foreign mode-5 get userinfo: %v", err)
	}
	foreign, rest := splitGameServerUpperPacket(t, conn.write.Bytes())
	if len(rest) != 0 {
		t.Fatalf("foreign mode-5 reply left trailing bytes: %x", rest)
	}
	if foreign.Header.MsgID != currentCharacterNameChangeLogMsgID || !bytes.Equal(foreign.Body, []byte{0}) {
		t.Fatalf("foreign mode-5 reply header=%+v body=%x want empty log", foreign.Header, foreign.Body)
	}
	conn.write.Reset()

	// A player looking at character 1 (owned by dnf:1) must see character 1's
	// history: the request carries the *target* id on every connection.
	viewer := &gameSession{conn: conn, connID: "name-change-log-viewer", accountID: "dnf:2"}
	if err := service.handleGameFrame(viewer, getUserInfoLegacyFrame(t, []byte{0x01, 0x00, 0x05})); err != nil {
		t.Fatalf("handle other-player mode-5 get userinfo: %v", err)
	}
	viewed, rest := splitGameServerUpperPacket(t, conn.write.Bytes())
	if len(rest) != 0 {
		t.Fatalf("other-player mode-5 reply left trailing bytes: %x", rest)
	}
	if viewed.Header.MsgID != currentCharacterNameChangeLogMsgID || !bytes.Equal(viewed.Body, want) {
		t.Fatalf("other-player mode-5 reply header=%+v body=%x want %x", viewed.Header, viewed.Body, want)
	}
}

// TestNameChangeLogBodyCarriesThePreviousNameInline locks the layout the live
// client reads out of 0x3457C80: the row's leading u32 is a *length*, followed by
// that many GBK bytes, then the 大区 index and the timestamp. The four-byte field
// it replaced was read as a key and produced a blank 角色名 column.
func TestNameChangeLogBodyCarriesThePreviousNameInline(t *testing.T) {
	entries := []currentCharacterRenameLogEntry{
		{Unix: 1790093566, ServerID: 1, HasServerID: true, OldName: "1111"},
	}
	body := buildCurrentCharacterNameChangeLogBody(entries, 9)
	want := []byte{1}                           // u8 row count
	want = append(want, 0x04, 0x00, 0x00, 0x00) // u32 name length
	want = append(want, '1', '1', '1', '1')     // GBK name bytes
	want = append(want, 1)                      // u8 server id
	want = append(want, 0xFE, 0xA8, 0xB2, 0x6A) // u32 unix time, little endian
	if !bytes.Equal(body, want) {
		t.Fatalf("body=%x\n want=%x", body, want)
	}
	if body[9] != 1 {
		t.Fatalf("server id must sit directly behind the name at offset 9, got %d", body[9])
	}
}

// TestNameChangeLogBodyPinsTheNamelessRowLayout pins the shape already verified
// against the live client: a record without a recorded previous name still sends
// the four-byte zero length, so the server id stays at row+4 and the timestamp at
// row+5 and the client renders date and 大区 exactly as before.
func TestNameChangeLogBodyPinsTheNamelessRowLayout(t *testing.T) {
	entries := []currentCharacterRenameLogEntry{
		{Unix: 1790093566, ServerID: 1, HasServerID: true},
	}
	body := buildCurrentCharacterNameChangeLogBody(entries, 9)
	want := []byte{1}                           // u8 row count
	want = append(want, 0x00, 0x00, 0x00, 0x00) // u32 name length 0 -> built-in empty cell
	want = append(want, 1)                      // u8 server id
	want = append(want, 0xFE, 0xA8, 0xB2, 0x6A) // u32 unix time, little endian
	if !bytes.Equal(body, want) {
		t.Fatalf("nameless body=%x\n want=%x", body, want)
	}
	if body[5] != 1 {
		t.Fatalf("server id must sit at offset 5 (row+4), got %d", body[5])
	}
}

// TestNameChangeLogNameBytesCapsOnCharacterBoundary guards the client's own limit:
// a length of 31 or more makes 0x3457C80 discard the name entirely, and a slice
// through a GBK character would leave a dangling lead byte.
func TestNameChangeLogNameBytesCapsOnCharacterBoundary(t *testing.T) {
	single := currentCharacterNameChangeLogNameBytes("中")
	if len(single) != 2 {
		t.Fatalf("one CJK character must encode to two GBK bytes, got %x", single)
	}
	long := strings.Repeat("中", 20) // 40 GBK bytes, well past the cap
	capped := currentCharacterNameChangeLogNameBytes(long)
	if len(capped) != currentCharacterNameChangeLogNameMaxBytes {
		t.Fatalf("capped name=%d bytes want %d (even count, no split character)", len(capped), currentCharacterNameChangeLogNameMaxBytes)
	}
	if len(capped)%2 != 0 {
		t.Fatalf("cap split a GBK character: %x", capped)
	}
	decoded, err := simplifiedchinese.GB18030.NewDecoder().Bytes(capped)
	if err != nil {
		t.Fatalf("capped name must stay valid GBK: %v", err)
	}
	if got := string(decoded); got != strings.Repeat("中", currentCharacterNameChangeLogNameMaxBytes/2) {
		t.Fatalf("capped name decoded to %q", got)
	}
	if blank := currentCharacterNameChangeLogNameBytes("   "); len(blank) != 0 {
		t.Fatalf("whitespace-only name must not emit bytes, got %x", blank)
	}
}

// TestNameChangeLogBodyKeepsRowBytesStableWithoutNames pins the backward
// compatibility contract: when no record carries a previous name the payload is
// exactly the row-only body the stock client already accepts.
func TestNameChangeLogBodyKeepsRowBytesStableWithoutNames(t *testing.T) {
	entries := []currentCharacterRenameLogEntry{
		{Unix: 1790093566, ServerID: 1, HasServerID: true},
		{Unix: 1790093567, ServerID: 2, HasServerID: true, OldName: "   "},
	}
	body := buildCurrentCharacterNameChangeLogBody(entries, 9)
	want := []byte{2}
	want = append(want, 0, 0, 0, 0)
	want = append(want, 1)
	want = append(want, 0, 0, 0, 0)
	binary.LittleEndian.PutUint32(want[6:10], 1790093566)
	want = append(want, 0, 0, 0, 0)
	want = append(want, 2)
	want = append(want, 0, 0, 0, 0)
	binary.LittleEndian.PutUint32(want[15:19], 1790093567)
	if !bytes.Equal(body, want) {
		t.Fatalf("nameless body=%x want %x", body, want)
	}
}
