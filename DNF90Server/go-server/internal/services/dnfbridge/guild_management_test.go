package dnfbridge

import (
	"bytes"
	"context"
	"encoding/binary"
	"errors"
	"testing"
	"time"

	"longheng.io/server/internal/modules/dnf/channelcatalog"
	"longheng.io/server/internal/modules/dnf/channelinfo"
	"longheng.io/server/internal/modules/dnf/dnfenum"
	dnfguild "longheng.io/server/internal/modules/dnf/guild"
	dnfrepo "longheng.io/server/internal/modules/dnf/repository"
	dnfrepomemory "longheng.io/server/internal/modules/dnf/repository/memory"
)

// op830 (SET_GUILD_RECOMMAND_CHANNEL) carries no body: the client means "the
// channel I am standing in". The handler used to require
// ResidentFor(channel).ID == channel.ID, which is false for every ordinary town
// channel (channelcatalog.isResidentChannel reads type 11 as a death-tower
// channel while 11 is this ETC's ordinary town type), so the master of guild 1
// standing in channel 181 got error 19 and the client rendered 只有公会长才能设置.
func TestCurrentGuildRecommendedChannelStoresTheCastersChannel(t *testing.T) {
	ctx := context.Background()
	repositories := dnfrepomemory.NewMemoryGroup()
	master := dnfrepo.CharacterRecord{CharacterID: "1", AccountID: "dnf:1", Name: "master", Level: 90}
	if err := repositories.Character.Save(ctx, master); err != nil {
		t.Fatal(err)
	}
	registry := dnfguild.NewRegistry()
	guild, err := registry.Create(master, "guild-chan", serviceTestNow())
	if err != nil {
		t.Fatal(err)
	}
	if err := dnfguild.Save(ctx, repositories.Settings, registry, serviceTestNow()); err != nil {
		t.Fatal(err)
	}
	const etc = "\n[dungeon]\n`[noblsky]` `noblsky` 1\n[/dungeon]\n" +
		"[dungeon]\n`[crack]` `crack`\n[/dungeon]\n" +
		"[server]\n1 181 `noblsky` 11 `[noblsky]` 0 0 1 19 `crack` 10 `[crack]` 0 0\n[/server]\n"
	index, err := channelinfo.Parse([]byte(etc))
	if err != nil {
		t.Fatalf("Parse() error = %v", err)
	}
	catalog, err := channelcatalog.New(index, channelcatalog.Options{ServerID: 1})
	if err != nil {
		t.Fatalf("New() error = %v", err)
	}
	connection := &bufferConn{}
	session := &gameSession{
		conn:                connection,
		selectedCharacterID: 1,
		accountID:           "dnf:1",
		channel:             channelcatalog.Channel{ID: 181, ServerID: 1, Type: 11, Group: "noblsky"},
	}
	service := &Service{
		options:            options{accountID: "dnf:1", gameUpperBodyCodec: gameUpperBodyCodecPlain},
		repositoryProvider: func() (dnfrepo.Group, bool) { return repositories, true },
		catalog:            catalog,
	}
	if err := service.handleCurrentGuildRecommendedChannel(session, nil); err != nil {
		t.Fatal(err)
	}
	packet, _ := splitGameServerUpperPacket(t, connection.write.Bytes())
	if packet.Header.MsgID != uint16(dnfenum.CmdPacketSetGuildRecommandChannel) {
		t.Fatalf("op830 answer opcode=%d body=%x", packet.Header.MsgID, packet.Body)
	}
	if len(packet.Body) == 0 || packet.Body[0] != 1 {
		t.Fatalf("op830 answer=%x want success", packet.Body)
	}
	reloaded, err := dnfguild.Load(ctx, repositories.Settings)
	if err != nil {
		t.Fatal(err)
	}
	saved, found := reloaded.GuildByID(guild.ID)
	if !found || saved.RecommendedChannel != 181 {
		t.Fatalf("recommended channel=%d found=%t want 181", saved.RecommendedChannel, found)
	}
	// A bootstrap channel is still refused.
	session.channel = channelcatalog.Channel{ID: 19, ServerID: 1, Type: 10, Group: "crack"}
	connection.write.Reset()
	if err := service.handleCurrentGuildRecommendedChannel(session, nil); err != nil {
		t.Fatal(err)
	}
	refused, _ := splitGameServerUpperPacket(t, connection.write.Bytes())
	if len(refused.Body) < 2 || refused.Body[0] != 0 {
		t.Fatalf("crack channel accepted: %x", refused.Body)
	}
}

// 公会基地: the client reads {u8 success, u8 code} and shows whatever toast the
// code maps to (decoded into D:\re_tools\agit_codes.txt). A member who is not the
// master must get 36 (您不是公会会长), a second create 181 (已经生成公会基地), a
// second delete 182 (公会基地不存在) and an out-of-range stage 188.
func TestCurrentGuildBaseLifecycleAnswersClientCodes(t *testing.T) {
	ctx := context.Background()
	repositories := dnfrepomemory.NewMemoryGroup()
	master := dnfrepo.CharacterRecord{CharacterID: "1", AccountID: "dnf:1", Name: "master", Level: 90}
	guest := dnfrepo.CharacterRecord{CharacterID: "2", AccountID: "dnf:2", Name: "guest", Level: 80}
	for _, character := range []dnfrepo.CharacterRecord{master, guest} {
		if err := repositories.Character.Save(ctx, character); err != nil {
			t.Fatal(err)
		}
	}
	registry := dnfguild.NewRegistry()
	guild, err := registry.Create(master, "guild-base", serviceTestNow())
	if err != nil {
		t.Fatal(err)
	}
	if err := registry.Apply(guild.ID, guest, "", serviceTestNow()); err != nil {
		t.Fatal(err)
	}
	if _, err := registry.DecideApplication(guild.ID, "1", "2", true, serviceTestNow()); err != nil {
		t.Fatal(err)
	}
	if err := dnfguild.Save(ctx, repositories.Settings, registry, serviceTestNow()); err != nil {
		t.Fatal(err)
	}
	service := &Service{
		options:            options{accountID: "dnf:1", gameUpperBodyCodec: gameUpperBodyCodecPlain},
		repositoryProvider: func() (dnfrepo.Group, bool) { return repositories, true },
	}
	masterSession := &gameSession{conn: &bufferConn{}, selectedCharacterID: 1, accountID: "dnf:1"}
	guestSession := &gameSession{conn: &bufferConn{}, selectedCharacterID: 2, accountID: "dnf:2"}

	answer := func(session *gameSession, opcode uint16, call func() error) []byte {
		t.Helper()
		connection := session.conn.(*bufferConn)
		connection.write.Reset()
		if err := call(); err != nil {
			t.Fatal(err)
		}
		packet, _ := splitGameServerUpperPacket(t, connection.write.Bytes())
		if packet.Header.MsgID != opcode {
			t.Fatalf("agit answer opcode=%d want=%d body=%x", packet.Header.MsgID, opcode, packet.Body)
		}
		if len(packet.Body) < 1 {
			t.Fatal("agit answer is empty")
		}
		return packet.Body
	}
	failure := func(body []byte, want byte, what string) {
		t.Helper()
		if body[0] != 0 || len(body) < 2 || body[1] != want {
			t.Fatalf("%s answer=%x want failure code %d", what, body, want)
		}
	}
	success := func(body []byte, what string) {
		t.Helper()
		if body[0] != 1 {
			t.Fatalf("%s answer=%x want success", what, body)
		}
	}

	// A guild is born with its base (this client has no 创建基地 button), so the
	// create packet must answer 181 已经生成公会基地 until the base is deleted.
	createOpcode := uint16(dnfenum.CmdPacketCreateGuildAgit)
	failure(answer(guestSession, createOpcode, func() error {
		return service.handleCurrentGuildAgitCreate(guestSession, nil)
	}), currentGuildAgitErrorNotMaster, "guest create")
	failure(answer(masterSession, createOpcode, func() error {
		return service.handleCurrentGuildAgitCreate(masterSession, nil)
	}), currentGuildAgitErrorExists, "create over the automatic base")

	upgradeOpcode := uint16(dnfenum.CmdPacketUpgradeGuildAgit)
	success(answer(masterSession, upgradeOpcode, func() error {
		return service.handleCurrentGuildAgitUpgrade(masterSession, []byte{3})
	}), "upgrade to 3")
	failure(answer(masterSession, upgradeOpcode, func() error {
		return service.handleCurrentGuildAgitUpgrade(masterSession, []byte{2})
	}), currentGuildAgitErrorStage, "downgrade to 2")
	failure(answer(masterSession, upgradeOpcode, func() error {
		return service.handleCurrentGuildAgitUpgrade(masterSession, []byte{dnfguild.MaxBaseStage + 1})
	}), currentGuildAgitErrorStage, "stage beyond the ladder")
	failure(answer(masterSession, upgradeOpcode, func() error {
		return service.handleCurrentGuildAgitUpgrade(masterSession, nil)
	}), currentGuildAgitErrorStage, "upgrade without a stage byte")

	deleteOpcode := uint16(dnfenum.CmdPacketDeleteGuildAgit)
	success(answer(masterSession, deleteOpcode, func() error {
		return service.handleCurrentGuildAgitDelete(masterSession, nil)
	}), "delete")
	failure(answer(masterSession, deleteOpcode, func() error {
		return service.handleCurrentGuildAgitDelete(masterSession, nil)
	}), currentGuildAgitErrorMissing, "second delete")

	// With the base gone the client's create request has to go through again.
	success(answer(masterSession, createOpcode, func() error {
		return service.handleCurrentGuildAgitCreate(masterSession, nil)
	}), "create after delete")

	reloaded, err := dnfguild.Load(ctx, repositories.Settings)
	if err != nil {
		t.Fatal(err)
	}
	saved, found := reloaded.GuildByID(guild.ID)
	if !found || saved.BaseStage != 1 {
		t.Fatalf("base stage=%d found=%t want 1 after re-create", saved.BaseStage, found)
	}
}

// TestCurrentGuildContentErrorCodesMatchClientTable pins the op761 refusal codes to
// the client's jump table. 182 is 没有基地 and must never answer a 积分 shortfall:
// that mis-mapping made every 公会内容 purchase pop 没有基地 for guilds without a
// base before 2026-09-12.
func TestCurrentGuildContentErrorCodesMatchClientTable(t *testing.T) {
	cases := []struct {
		err  error
		want byte
	}{
		{dnfguild.ErrInsufficientMileage, currentGuildContentErrorMileage},
		{dnfguild.ErrPermissionDenied, currentGuildContentErrorPermission},
		{dnfguild.ErrContentActive, currentGuildContentErrorAlreadyOwned},
		{dnfguild.ErrMemberLimit, currentGuildContentErrorMemberState},
		{dnfguild.ErrInsufficientGold, currentGuildContentErrorGold},
		{errDungeonPickupInventoryFull, currentGuildContentErrorGeneric},
		{errors.New("boom"), currentGuildContentErrorGeneric},
	}
	for _, testCase := range cases {
		if got := currentGuildContentError(testCase.err); got != testCase.want {
			t.Fatalf("currentGuildContentError(%v)=%d want=%d", testCase.err, got, testCase.want)
		}
	}
}

func TestCurrentGuildManagementNativeProjection(t *testing.T) {
	guild := &dnfguild.Guild{
		ID: 7, Name: "guild", MasterName: "master", Level: 3, Mark: 4,
		AuraMark: 2, AuraFrame: 6,
		Benefits:    map[byte]dnfguild.Benefit{21: {Type: 21, Value: 300, ExpiresAt: serviceTestNow().Add(time.Hour)}},
		MemberLimit: 1000, Notice: "notice", Promotion: "welcome", JoinMode: 1,
		GradeNames:        map[byte]string{1: "master", 4: "member"},
		RecommendedRegion: 0, RecommendedChannel: 253, Tags: []byte{0, 3},
		Members: map[string]dnfguild.Member{
			"16": {CharacterID: "16", Name: "master", Job: "3", Level: 90, Grade: 1},
			"46": {CharacterID: "46", Name: "member", Job: "4", Level: 80, Grade: 4},
		},
	}
	body := buildCurrentGuildManagementBody(guild, 1, serviceTestNow(), 1)
	reader := bytes.NewReader(body)
	u8 := func() byte {
		t.Helper()
		v, err := reader.ReadByte()
		if err != nil {
			t.Fatal(err)
		}
		return v
	}
	u16 := func() uint16 {
		t.Helper()
		var v uint16
		if err := binary.Read(reader, binary.LittleEndian, &v); err != nil {
			t.Fatal(err)
		}
		return v
	}
	u32 := func() uint32 {
		t.Helper()
		var v uint32
		if err := binary.Read(reader, binary.LittleEndian, &v); err != nil {
			t.Fatal(err)
		}
		return v
	}
	dstr := func(want string) {
		t.Helper()
		n := u32()
		b := make([]byte, n)
		if _, err := reader.Read(b); err != nil && n != 0 {
			t.Fatal(err)
		}
		if !bytes.Equal(b, rosterNameBytes(want)) {
			t.Fatalf("string=%x want=%q", b, want)
		}
	}
	dstr("guild")
	u8()
	u8()
	if u16() != 2 {
		t.Fatal("management member count")
	}
	u8()
	// The notice must occupy this 256-unit DSTR: it is the slot
	// sub_1D8B560 assigns to guild+0x18 and the notice edit box reads back.
	// The 1024-unit introduction slot comes after the experience u32.
	dstr("notice")
	if u8() != 3 {
		t.Fatal("guild level")
	}
	u8()
	u8()
	if u8() != 4 {
		t.Fatal("guild mark")
	}
	u32()
	// Both name slots carry the guild master: the first is the one the board
	// renders in the 会长 field (guild+0x20), the second is the 17-unit name
	// sub_2690140 stores on the local actor.
	dstr("master")
	dstr("master")
	if u16() != 1 {
		t.Fatal("online count")
	}
	dstr("welcome")
	for grade := byte(0); grade < 6; grade++ {
		permissions := u32()
		name := make([]byte, 24)
		if _, err := reader.Read(name); err != nil {
			t.Fatal(err)
		}
		if grade == 1 && (permissions&(1<<1|1<<4|1<<5|1<<8|1<<11|1<<17)) != (1<<1|1<<4|1<<5|1<<8|1<<11|1<<17) {
			t.Fatal("missing master settings permissions")
		}
		if grade == 4 && permissions != 0 {
			t.Fatal("ordinary member received management permissions")
		}
		if !bytes.Equal(bytes.TrimRight(name, "\x00"), rosterNameBytes(guild.GradeNames[grade])) {
			t.Fatal("grade name boundary")
		}
	}
	if u8() != 2 || u8() != 6 || u32() != 0 || u8() != 0 || u32() != 1000 {
		t.Fatal("member capacity benefit")
	}
	if u8() != 21 || u32() != uint32(serviceTestNow().Add(time.Hour).Unix()) || u8() != 0 || u32() != 300 {
		t.Fatal("native contract benefit")
	}
	u32()
	u8()
	u32()
	u8()
	if u32() != 0 || u32() != 253 {
		t.Fatal("advertised guild channel")
	}
	if u32() != 2 || u32() != 6 {
		t.Fatal("aura mark/frame order")
	}
	u16()
	u8()
	u8()
	if u32() != 9 || u8() != 1 || reader.Len() != 0 {
		t.Fatal("recruitment tail")
	}

	body = buildCurrentGuildMemberListBody(guild, true, true, map[string]byte{"16": 253}, testGuildMemberListNow())
	if binary.LittleEndian.Uint32(body) != 7 {
		t.Fatal("member list must start with guild ID")
	}
	_, next, ok := readCurrentRawDstr(body, 4)
	if !ok || binary.LittleEndian.Uint16(body[next:]) != 2 || binary.LittleEndian.Uint16(body[next+2:]) != 2 {
		t.Fatal("member total/count")
	}
	pos := next + 4
	for _, want := range []struct {
		id                  uint32
		job, grade, channel byte
	}{{16, 3, 1, 253}, {46, 4, 4, 255}} {
		if binary.LittleEndian.Uint32(body[pos:]) != want.id {
			t.Fatal("member ID")
		}
		pos += 4
		for i := 0; i < 2; i++ {
			_, n, ok := readCurrentRawDstr(body, pos)
			if !ok {
				t.Fatal("member string")
			}
			pos = n
		}
		pos += 2
		if body[pos] != want.job || body[pos+4] != want.channel || body[pos+7] != want.grade {
			t.Fatalf("member job/channel/grade fields=%x", body[pos:pos+8])
		}
		// The roster key must be the member's own character ID: the client
		// keys its guild member map (guild+0x2D8) on it, so a shared value
		// collapses every row onto one map entry.
		if binary.LittleEndian.Uint32(body[pos+8+4:]) != want.id {
			t.Fatalf("member roster key=%x want=%d", body[pos+8+4:pos+8+8], want.id)
		}
		// sub_FB4910's third pass is the only one that can list an offline
		// member and it refuses any row whose member+0x15C is 0 (00FB4C95);
		// the second member view additionally needs member+0x181 (00FC4E35).
		// Marking an online row would pin it above every other row, because
		// sub_FA6F00 orders on this field before it looks at the sort column.
		wantListable := byte(0)
		if want.channel == currentGuildMemberOfflineChannel {
			wantListable = 1
		}
		if body[pos+8+4+4] != wantListable {
			t.Fatalf("member offline-listable=%d want=%d", body[pos+8+4+4], wantListable)
		}
		pos += 8 + 4 + 4 + 1
		_, pos, ok = readCurrentRawDstr(body, pos)
		if !ok {
			t.Fatal("member trailing name")
		}
		// member+0x181 then member+0x182, the region byte the op1454 settings
		// row has to match and the member context menu refuses when it is 0.
		if body[pos] != wantListable || body[pos+1] != currentGuildMemberRegion {
			t.Fatalf("member trailing flags=%x want=%d,%d", body[pos:pos+2], wantListable, currentGuildMemberRegion)
		}
		pos += 2
	}
	if pos+4 != len(body) {
		t.Fatal("member cursor boundary")
	}
}

func TestCurrentGuildManagementRequestsPersistAndApproveCapturedApplicant(t *testing.T) {
	ctx := context.Background()
	repositories := dnfrepomemory.NewMemoryGroup()
	master := dnfrepo.CharacterRecord{CharacterID: "16", AccountID: "dnf:master", Name: "master", Level: 90}
	applicant := dnfrepo.CharacterRecord{CharacterID: "3", AccountID: "dnf:applicant", Name: "applicant", Level: 80}
	for _, c := range []dnfrepo.CharacterRecord{master, applicant} {
		if err := repositories.Character.Save(ctx, c); err != nil {
			t.Fatal(err)
		}
	}
	registry := dnfguild.NewRegistry()
	guild, err := registry.Create(master, "guild", serviceTestNow())
	if err != nil {
		t.Fatal(err)
	}
	if err = registry.Apply(guild.ID, applicant, "please", serviceTestNow()); err != nil {
		t.Fatal(err)
	}
	if err = dnfguild.Save(ctx, repositories.Settings, registry, serviceTestNow()); err != nil {
		t.Fatal(err)
	}
	index, err := channelinfo.Parse([]byte("[dungeon]\n`[elven_guard]` `town` 1 2\n[/dungeon]\n[server]\n1 253 `town` 0 `[elven_guard]` 1 0\n[/server]"))
	if err != nil {
		t.Fatal(err)
	}
	catalog, err := channelcatalog.New(index, channelcatalog.Options{})
	if err != nil {
		t.Fatal(err)
	}
	channel, _ := catalog.Channel(253)
	owner := &gameSession{conn: &bufferConn{}, selectedCharacterID: 16, accountID: master.AccountID, channel: channel}
	guest := &gameSession{conn: &bufferConn{}, selectedCharacterID: 3, accountID: applicant.AccountID, channel: channel}
	for _, session := range []*gameSession{owner, guest} {
		session.initialTownRouteCharacterID = session.selectedCharacterID
		session.initialTownRouteStage = currentInitialTownRoutePlayerStateSent
	}
	service := &Service{options: options{gameUpperBodyCodec: gameUpperBodyCodecPlain}, catalog: catalog, gameSessions: map[uint16]*gameSession{16: owner, 3: guest}, repositoryProvider: func() (dnfrepo.Group, bool) { return repositories, true }}
	module := guildGameplayModule()
	send := func(session *gameSession, opcode uint16, body []byte, wantSuccess bool) {
		t.Helper()
		session.conn.(*bufferConn).write.Reset()
		handler := module.UpperHandlers[opcode]
		if handler == nil {
			t.Fatalf("missing guild route %d", opcode)
		}
		if err := handler(service, session, gameplayRequest{Opcode: opcode, Classification: 1, Body: body}); err != nil {
			t.Fatal(err)
		}
		packet, _ := splitGameServerUpperPacket(t, session.conn.(*bufferConn).write.Bytes())
		if packet.Header.MsgID != opcode || (packet.Body[0] == 1) != wantSuccess {
			t.Fatalf("opcode=%d packet=%+v body=%x", opcode, packet.Header, packet.Body)
		}
	}
	// Captured request for character 3 previously decoded its last four bytes as ID 0.
	// The trailing byte is the region the applicant row carried; the sender at
	// 03198789 echoes [esi+0x54] back, so 0 is rejected.
	send(owner, 350, []byte{3, 0, 0, 0, currentGuildMemberRegion}, true)
	reloaded, err := dnfguild.Load(ctx, repositories.Settings)
	if err != nil {
		t.Fatal(err)
	}
	joined, member, found := reloaded.GuildForCharacter("3")
	if !found || joined.ID != guild.ID || member.Grade != 4 {
		t.Fatal("captured application did not join")
	}
	seenActor, seenGrade := false, false
	for wire := guest.conn.(*bufferConn).write.Bytes(); len(wire) > 0; {
		packet, rest := splitGameServerUpperPacket(t, wire)
		wire = rest
		if packet.Header.MsgID == uint16(dnfenum.CmdPacketSetUDPIPPort) {
			// The 公会徽章 aura block rides this same opcode, so skip it and
			// keep looking for the actor creation/binding body.
			if isCurrentGuildAuraBody(packet.Body) {
				continue
			}
			assertCurrentGuildActorState(t, packet.Body, guild.ID, uint32(guild.Mark), guild.Name)
			seenActor = true
		}
		if packet.Header.MsgID == 127 && bytes.Equal(packet.Body, []byte{4}) {
			seenGrade = true
		}
	}
	if !seenActor || !seenGrade {
		t.Fatalf("approved member refresh: actor=%t grade=%t", seenActor, seenGrade)
	}
	for _, bad := range [][]byte{nil, {3, 0, 0, 0}, {3, 0, 0, 0, 1}, {3, 0, 0, 0, 0, 0}} {
		send(owner, 350, bad, false)
	}
	send(owner, 787, []byte{4}, true)
	send(guest, 787, []byte{1}, false)
	send(owner, 787, []byte{5}, false)
	send(owner, 830, nil, true)
	send(guest, 830, nil, false)
	send(owner, 154, currentGuildTestDSTR("new notice"), true)
	var settings packetWriter
	settings.writeUint32(0x20022)
	writeClientNameDstr(&settings, "new promotion")
	settings.writeByte(1)
	settings.writeUint32(9)
	settings.writeByte(2)
	send(owner, 740, settings.bytes(), true)
	send(guest, 740, settings.bytes(), false)
	for _, bad := range [][]byte{nil, settings.bytes()[:13], append(settings.bytes(), 0)} {
		send(owner, 740, bad, false)
	}
	for _, index := range []int{0, len(settings.bytes()) - 5, len(settings.bytes()) - 1} {
		bad := append([]byte(nil), settings.bytes()...)
		bad[index] = 255
		send(owner, 740, bad, false)
	}
	reloaded, err = dnfguild.Load(ctx, repositories.Settings)
	if err != nil {
		t.Fatal(err)
	}
	saved, _ := reloaded.GuildByID(guild.ID)
	// op154 owns the notice (op70's 256-unit DSTR), not the 1024-unit
	// introduction slot: routing it to Introduction is what made every
	// notice save look lost in the client's edit box.
	if saved.Mark != 4 || saved.RecommendedRegion != 0 || saved.RecommendedChannel != 253 || saved.Notice != "new notice" || saved.Promotion != "new promotion" || saved.JoinMode != 2 || !saved.AllianceRecruitment || !bytes.Equal(saved.Tags, []byte{0, 3}) {
		t.Fatalf("settings not durable: %+v", saved)
	}
	assertGrade := func(session *gameSession, want byte) {
		t.Helper()
		var grades []byte
		for wire := session.conn.(*bufferConn).write.Bytes(); len(wire) > 0; {
			packet, rest := splitGameServerUpperPacket(t, wire)
			wire = rest
			if packet.Header.Classification == 0 && packet.Header.MsgID == 127 {
				grades = append(grades, packet.Body...)
			}
		}
		if len(grades) == 0 || grades[len(grades)-1] != want {
			t.Fatalf("grade notifications=%v want=%d", grades, want)
		}
	}
	// Appointing 副会长/优秀会员 is op126. sub_26A3590 sends
	// sub_3454EB0(w, 0x7E) / sub_3454FC0(w, selector) /
	// sub_3455180(w, name) / sub_3454FC0(w, grade), and the guild member
	// context menu calls it with grade 2, 3 or 4 (0325239C, 032523D5,
	// 03252363). This step used to drive op748 with {3,0,0,0,2}, which only
	// passed because the handler read that as "target 3, grade 2" -- op748
	// is the per-grade permission editor and sends a u8 grade followed by a
	// u32 mask (00F9CAA1..00F9CABD), so it is rejected below instead.
	var appoint packetWriter
	appoint.writeByte(1)
	writeClientNameDstr(&appoint, "applicant")
	appoint.writeByte(2)
	send(owner, 126, appoint.bytes(), true)
	assertGrade(guest, 2)
	// op748 is the per-grade permission editor: u8 grade + u32 mask. Its
	// class-1 reply handler (sub_1D0EA20) reads only the success flag, and
	// mapped the generic failure 34 to dstr 0x9CD9 -- "该公会不存在" -- while
	// this was rejected.
	send(owner, 748, []byte{2, 0xff, 0xff, 0x03, 0x00}, true)
	reloaded, err = dnfguild.Load(ctx, repositories.Settings)
	if err != nil {
		t.Fatal(err)
	}
	saved, _ = reloaded.GuildByID(guild.ID)
	if saved.GradePermissions[2] != dnfguild.GradePermissionMask {
		t.Fatalf("grade permissions=%v", saved.GradePermissions)
	}
	// op70's grade table is the only place the client rereads the mask from,
	// and buildCurrentGuildManagementBody sources every row through this
	// resolver. Grades the master never edited keep the fixed defaults.
	if got := dnfguild.GradePermissionsFor(saved, 2); got != dnfguild.GradePermissionMask {
		t.Fatalf("op70 grade 2 permissions=%x", got)
	}
	if got := dnfguild.GradePermissionsFor(saved, dnfguild.DefaultMasterGrade); got != dnfguild.GradePermissionMask {
		t.Fatalf("op70 master permissions=%x", got)
	}
	if got := dnfguild.GradePermissionsFor(saved, 3); got != 0 {
		t.Fatalf("op70 grade 3 permissions=%x", got)
	}
	send(owner, 155, []byte{3, 0, 0, 0}, true)
	assertGrade(owner, 2)
	assertGrade(guest, 1)
}

func TestCurrentGuildLoginRequestsWaitForActorTransition(t *testing.T) {
	for _, member := range []bool{false, true} {
		t.Run(map[bool]string{false: "nonmember", true: "master"}[member], func(t *testing.T) {
			ctx := context.Background()
			repositories := dnfrepomemory.NewMemoryGroup()
			character := dnfrepo.CharacterRecord{CharacterID: "16", AccountID: "dnf:1", Name: "master", Level: 90}
			if err := repositories.Character.Save(ctx, character); err != nil {
				t.Fatal(err)
			}
			registry := dnfguild.NewRegistry()
			if member {
				if _, err := registry.Create(character, "guild", serviceTestNow()); err != nil {
					t.Fatal(err)
				}
			}
			if err := dnfguild.Save(ctx, repositories.Settings, registry, serviceTestNow()); err != nil {
				t.Fatal(err)
			}
			connection := &bufferConn{}
			session := &gameSession{conn: connection, selectedCharacterID: 16, accountID: character.AccountID}
			service := &Service{options: options{gameUpperBodyCodec: gameUpperBodyCodecPlain}, repositoryProvider: func() (dnfrepo.Group, bool) { return repositories, true }}
			module := guildGameplayModule()
			for _, state := range []struct {
				characterID uint16
				stage       currentInitialTownRouteStage
			}{{16, currentInitialTownRouteArmed}, {16, currentInitialTownRouteActorBound}, {3, currentInitialTownRoutePlayerStateSent}} {
				session.initialTownRouteCharacterID, session.initialTownRouteStage = state.characterID, state.stage
				for _, opcode := range []uint16{67, 140, 339} {
					connection.write.Reset()
					if err := module.UpperHandlers[opcode](service, session, gameplayRequest{Opcode: opcode, Classification: 1, Body: []byte{0, 0, 0, 0}}); err != nil {
						t.Fatal(err)
					}
					packet, rest := splitGameServerUpperPacket(t, connection.write.Bytes())
					if packet.Header.Classification != 1 || packet.Header.MsgID != opcode || len(rest) != 0 {
						t.Fatalf("early guild request emitted actor-dependent state: opcode=%d packet=%+v trailing=%x", opcode, packet.Header, rest)
					}
				}
				connection.write.Reset()
				session.townMu.Lock()
				err := service.sendCurrentInitialTownGuildStateProjectionLocked(session, "test_before_transition")
				session.townMu.Unlock()
				if err != nil || connection.write.Len() != 0 || session.initialTownGuildStateProjectionSent {
					t.Fatalf("premature login projection: err=%v bytes=%d sent=%t", err, connection.write.Len(), session.initialTownGuildStateProjectionSent)
				}
			}
			session.initialTownRouteCharacterID = 16
			session.initialTownRouteStage = currentInitialTownRouteTransitionSent
			session.townMu.Lock()
			err := service.sendCurrentInitialTownGuildStateProjectionLocked(session, "test_after_transition")
			session.townMu.Unlock()
			if err != nil {
				t.Fatal(err)
			}
			wire := connection.write.Bytes()
			// op70 goes only to a member. Its handler shows the notice once per
			// client run gated on nothing but the global flag at 0x519297C
			// (01D8B8A2), so the empty projection this used to send for a
			// guildless character popped a blank 公会公告 line on every login.
			//
			// Between op127 and the membership notification sit the three
			// projections writeCurrentGuildManagementState now ends with, which
			// upstream places in exactly this order: the 公会徽章 aura block
			// (opcode CmdPacketSetUDPIPPort, which it shares with the actor
			// packets) and the DLL-private 公会经验排名 413. No 公会 BUFF row
			// appears because this guild has no Guild_Status benefit.
			expected := []uint16{70, 127, uint16(dnfenum.CmdPacketSetUDPIPPort), 413, 308, 1453, 744}
			if !member {
				expected = expected[1:]
			}
			for _, opcode := range expected {
				packet, rest := splitGameServerUpperPacket(t, wire)
				wire = rest
				if packet.Header.MsgID != opcode {
					t.Fatalf("post-transition guild state=%+v want=%d", packet.Header, opcode)
				}
				if opcode == 127 {
					grade := byte(100)
					if member {
						grade = 1
					}
					if !bytes.Equal(packet.Body, []byte{grade}) {
						t.Fatalf("guild grade=%x want=%d", packet.Body, grade)
					}
				}
			}
			if len(wire) != 0 || !session.initialTownGuildStateProjectionSent {
				t.Fatal("post-transition guild projection incomplete")
			}
			connection.write.Reset()
			session.townMu.Lock()
			err = service.sendCurrentInitialTownGuildStateProjectionLocked(session, "test_duplicate")
			session.townMu.Unlock()
			if err != nil || connection.write.Len() != 0 {
				t.Fatal("duplicate login projection")
			}
		})
	}
}

// A cross-town move and the Seria-room exit (op1425) commit their own scene
// transition and then reset the initial-town route cursor to idle. The guild
// management state must keep flowing after that: the master who walked out of
// the room before creating the guild otherwise kept op127 grade 100 (the
// nonmember sentinel) and an empty op70 body for the rest of the session.
func TestCurrentGuildManagementStateSurvivesTownRouteCursorReset(t *testing.T) {
	ctx := context.Background()
	repositories := dnfrepomemory.NewMemoryGroup()
	character := dnfrepo.CharacterRecord{CharacterID: "16", AccountID: "dnf:1", Name: "master", Level: 90}
	if err := repositories.Character.Save(ctx, character); err != nil {
		t.Fatal(err)
	}
	registry := dnfguild.NewRegistry()
	guild, err := registry.Create(character, "guild", serviceTestNow())
	if err != nil {
		t.Fatal(err)
	}
	if err := dnfguild.Save(ctx, repositories.Settings, registry, serviceTestNow()); err != nil {
		t.Fatal(err)
	}
	connection := &bufferConn{}
	session := &gameSession{conn: connection, selectedCharacterID: 16, accountID: character.AccountID}
	service := &Service{
		options:            options{gameUpperBodyCodec: gameUpperBodyCodecPlain},
		repositoryProvider: func() (dnfrepo.Group, bool) { return repositories, true },
	}
	// Exactly the post-move state: cursor idle, town actor committed.
	session.initialTownRouteCharacterID = 0
	session.initialTownRouteStage = currentInitialTownRouteIdle
	session.townSceneReadyCharacterID = 16
	if err := service.sendCurrentGuildManagementState(session, guild); err != nil {
		t.Fatal(err)
	}
	wire := connection.write.Bytes()
	// op127 is followed by the 公会徽章 aura block and the DLL-private 公会经验排名,
	// the two projections writeCurrentGuildManagementState now ends with. The
	// guild has no Guild_Status benefit, so no 公会 BUFF row is emitted, and
	// publishCurrentGuildAuraToTownPeers is a no-op here because this Service has
	// no online player manager.
	for _, opcode := range []uint16{70, 127, uint16(dnfenum.CmdPacketSetUDPIPPort), 413} {
		packet, rest := splitGameServerUpperPacket(t, wire)
		wire = rest
		if packet.Header.MsgID != opcode {
			t.Fatalf("post-move guild state=%+v want=%d", packet.Header, opcode)
		}
		if opcode == 127 && !bytes.Equal(packet.Body, []byte{1}) {
			t.Fatalf("post-move guild grade=%x want master grade 1", packet.Body)
		}
	}
	if len(wire) != 0 {
		t.Fatalf("post-move guild state trailing=%x", wire)
	}
	// A session with neither cursor nor committed town actor still defers.
	connection.write.Reset()
	session.townSceneReadyCharacterID = 0
	if err := service.sendCurrentGuildManagementState(session, guild); err != nil {
		t.Fatal(err)
	}
	if connection.write.Len() != 0 {
		t.Fatalf("guild state sent before any actor: bytes=%x", connection.write.Bytes())
	}
}

// sub_1D21FD0 stores channel 0xff as member+0x34 == -1 and the window then
// renders no row at all, so an online member above channel 254 must not be
// announced as offline. Live evidence: two op67 bodies for the same guild
// differed in exactly one byte — the member's channel — and only the client
// that received 0xfe/0xfc rendered both members.
func TestCurrentGuildMemberChannelByteKeepsOnlineMembersVisible(t *testing.T) {
	for _, testCase := range []struct {
		name      string
		channelID int
		want      byte
	}{
		{"ordinary channel", 121, 121},
		{"highest representable", 254, 254},
		{"offline sentinel collision", 255, currentGuildMemberUnrepresentedChannel},
		{"pvp channel above the byte", 508, currentGuildMemberUnrepresentedChannel},
		{"low byte would read as offline", 511, currentGuildMemberUnrepresentedChannel},
		{"unbound channel", 0, currentGuildMemberUnrepresentedChannel},
	} {
		if got := currentGuildMemberChannelByte(testCase.channelID); got != testCase.want {
			t.Fatalf("%s: channel %d encoded as %#x want %#x", testCase.name, testCase.channelID, got, testCase.want)
		}
		if currentGuildMemberChannelByte(testCase.channelID) == currentGuildMemberOfflineChannel {
			t.Fatalf("%s: online member encoded as the offline sentinel", testCase.name)
		}
	}
}

// The requester's own row used to bypass the range check that dropped every
// other row, so a member on a PVP channel saw itself online while the rest of
// the guild saw it offline. Both paths must now agree.
func TestCurrentGuildOnlineChannelsAgreeForRequesterAndPeers(t *testing.T) {
	ctx := context.Background()
	repositories := dnfrepomemory.NewMemoryGroup()
	master := dnfrepo.CharacterRecord{CharacterID: "1", AccountID: "dnf:1", Name: "master", Level: 90}
	peer := dnfrepo.CharacterRecord{CharacterID: "42", AccountID: "dnf:2", Name: "peer", Level: 70}
	for _, character := range []dnfrepo.CharacterRecord{master, peer} {
		if err := repositories.Character.Save(ctx, character); err != nil {
			t.Fatal(err)
		}
	}
	registry := dnfguild.NewRegistry()
	guild, err := registry.Create(master, "guild", serviceTestNow())
	if err != nil {
		t.Fatal(err)
	}
	if err := registry.Apply(guild.ID, peer, "", serviceTestNow()); err != nil {
		t.Fatal(err)
	}
	if _, err := registry.DecideApplication(guild.ID, "1", "42", true, serviceTestNow()); err != nil {
		t.Fatal(err)
	}
	masterSession := &gameSession{conn: &bufferConn{}, selectedCharacterID: 1, accountID: master.AccountID}
	masterSession.channel.ID = 121
	peerSession := &gameSession{conn: &bufferConn{}, selectedCharacterID: 42, accountID: peer.AccountID}
	peerSession.channel.ID = 508
	service := &Service{
		options:            options{gameUpperBodyCodec: gameUpperBodyCodecPlain},
		repositoryProvider: func() (dnfrepo.Group, bool) { return repositories, true },
		gameSessions:       map[uint16]*gameSession{1: masterSession, 42: peerSession},
	}
	for _, requester := range []*gameSession{masterSession, peerSession} {
		channels := service.currentGuildOnlineChannels(guild, requester)
		if channels["1"] != 121 {
			t.Fatalf("master channel=%#x want 121", channels["1"])
		}
		if channels["42"] != currentGuildMemberUnrepresentedChannel {
			t.Fatalf("peer channel=%#x want %#x", channels["42"], currentGuildMemberUnrepresentedChannel)
		}
	}
}

// op153 carries both 退出公会 and 开除会员. sub_26A3540 builds it as a u8
// region plus the target's name (026A3569..026A357D) and the captured kick
// body is 01 04000000 <name>; the handler used to ignore the body and always
// remove the requester, so a master got the generic failure the client renders
// as 您不是工会成员或此工会不存在 and anyone else would have removed themselves.
func TestCurrentGuildSecedeRemovesTheNamedMember(t *testing.T) {
	ctx := context.Background()
	repositories := dnfrepomemory.NewMemoryGroup()
	master := dnfrepo.CharacterRecord{CharacterID: "16", AccountID: "dnf:master", Name: "master", Level: 90}
	member := dnfrepo.CharacterRecord{CharacterID: "3", AccountID: "dnf:member", Name: "applicant", Level: 80}
	for _, c := range []dnfrepo.CharacterRecord{master, member} {
		if err := repositories.Character.Save(ctx, c); err != nil {
			t.Fatal(err)
		}
	}
	registry := dnfguild.NewRegistry()
	guild, err := registry.Create(master, "guild", serviceTestNow())
	if err != nil {
		t.Fatal(err)
	}
	guild.Members["3"] = dnfguild.Member{CharacterID: "3", AccountID: member.AccountID, Name: member.Name, Level: 80, Grade: 4, JoinedAt: serviceTestNow()}
	if err = dnfguild.Save(ctx, repositories.Settings, registry, serviceTestNow()); err != nil {
		t.Fatal(err)
	}
	index, err := channelinfo.Parse([]byte("[dungeon]\n`[elven_guard]` `town` 1 2\n[/dungeon]\n[server]\n1 253 `town` 0 `[elven_guard]` 1 0\n[/server]"))
	if err != nil {
		t.Fatal(err)
	}
	catalog, err := channelcatalog.New(index, channelcatalog.Options{})
	if err != nil {
		t.Fatal(err)
	}
	channel, _ := catalog.Channel(253)
	owner := &gameSession{conn: &bufferConn{}, selectedCharacterID: 16, accountID: master.AccountID, channel: channel}
	owner.initialTownRouteCharacterID = owner.selectedCharacterID
	owner.initialTownRouteStage = currentInitialTownRoutePlayerStateSent
	service := &Service{
		options:            options{gameUpperBodyCodec: gameUpperBodyCodecPlain},
		catalog:            catalog,
		gameSessions:       map[uint16]*gameSession{16: owner},
		repositoryProvider: func() (dnfrepo.Group, bool) { return repositories, true },
	}
	handler := guildGameplayModule().UpperHandlers[uint16(dnfenum.CmdPacketReqGuildSecede)]
	if handler == nil {
		t.Fatal("missing op153 route")
	}
	var kick packetWriter
	kick.writeByte(currentGuildMemberRegion)
	writeClientNameDstr(&kick, "applicant")
	if err := handler(service, owner, gameplayRequest{Opcode: uint16(dnfenum.CmdPacketReqGuildSecede), Classification: 1, Body: kick.bytes()}); err != nil {
		t.Fatal(err)
	}
	packet, _ := splitGameServerUpperPacket(t, owner.conn.(*bufferConn).write.Bytes())
	if packet.Header.MsgID != uint16(dnfenum.CmdPacketReqGuildSecede) || packet.Body[0] != 1 {
		t.Fatalf("secede reply=%+v body=%x", packet.Header, packet.Body)
	}
	// sub_1D161F0 reads u8 / DSTR / u8 / DSTR and switches on the first byte,
	// so the removed member's name has to be the first DSTR.
	name, next, ok := parseCurrentGuildDSTR(packet.Body, 2)
	if !ok || name != "applicant" {
		t.Fatalf("secede reply name=%q ok=%t", name, ok)
	}
	if packet.Body[next] != 0 {
		t.Fatalf("secede reply second flag=%d", packet.Body[next])
	}
	reloaded, err := dnfguild.Load(ctx, repositories.Settings)
	if err != nil {
		t.Fatal(err)
	}
	saved, _ := reloaded.GuildByID(guild.ID)
	if _, still := saved.Members["3"]; still {
		t.Fatal("named member was not removed")
	}
	if _, requester := saved.Members["16"]; !requester {
		t.Fatal("the requester was removed instead of the named member")
	}
}
