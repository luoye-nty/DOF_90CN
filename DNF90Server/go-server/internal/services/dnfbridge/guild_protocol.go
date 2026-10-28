package dnfbridge

import (
	"encoding/binary"
	"os"
	"strconv"
	"strings"
	"time"

	dnfguild "longheng.io/server/internal/modules/dnf/guild"
)

// NoPack sub_FCCF70 renders six rows at a time, but its page counter divides
// the op1452 total-result count by 30. The request page is therefore a
// zero-based 30-row page, not the number of currently visible controls.
const currentGuildPageSize = 30

// currentGuildMembershipNotificationMsgID is NOTIPACKET_JOIN_GUILD_INFO in
// the current client. It intentionally shares the numeric value 0x134 with
// class-1 CMDPACKET_WITHDRAW_MONEY; the class-0 notification namespace is
// separate from the command namespace.
const currentGuildMembershipNotificationMsgID uint16 = 0x0134

type currentGuildSearchRequest struct {
	Mode     byte
	Page     int
	Category byte
	Query    string
}

func parseCurrentGuildSearchRequest(body []byte) (currentGuildSearchRequest, bool) {
	if len(body) < 1 {
		return currentGuildSearchRequest{}, false
	}
	request := currentGuildSearchRequest{Mode: body[0]}
	if request.Mode == 2 {
		if len(body) != 6 {
			return currentGuildSearchRequest{}, false
		}
		request.Page = int(binary.LittleEndian.Uint32(body[1:5]))
		request.Category = body[5]
		return request, true
	}
	raw, next, ok := readCurrentRawDstr(body, 1)
	if !ok || next != len(body) {
		return currentGuildSearchRequest{}, false
	}
	query, err := decodeCharacterName(raw)
	if err != nil {
		return currentGuildSearchRequest{}, false
	}
	request.Query = strings.TrimSpace(query)
	return request, true
}

func parseCurrentGuildDSTR(body []byte, offset int) (string, int, bool) {
	raw, next, ok := readCurrentRawDstr(body, offset)
	if !ok {
		return "", offset, false
	}
	value, err := decodeCharacterName(raw)
	if err != nil {
		return "", offset, false
	}
	return strings.TrimSpace(value), next, true
}

func parseCurrentGuildUint32(body []byte, offset int) (uint32, int, bool) {
	if offset < 0 || offset > len(body)-4 {
		return 0, offset, false
	}
	return binary.LittleEndian.Uint32(body[offset : offset+4]), offset + 4, true
}

func currentGuildNumericID(value string) uint32 {
	parsed, err := strconv.ParseUint(strings.TrimSpace(value), 10, 32)
	if err != nil {
		return 0
	}
	return uint32(parsed)
}

// buildCurrentGuildApplicationNoticeBody follows the raw op365 callback at
// NoPack sub_31A4E30/sub_31AAA90. A non-zero guild ID resolves the cached guild,
// then removes a matching tuple from the guild-management application list.
// This callback does not assign the current actor's guild membership.
// The zero form is a no-op when there is no established membership to bind.
func buildCurrentGuildApplicationNoticeBody(guildID uint32, accountSlot byte, characterName string) []byte {
	var writer packetWriter
	if guildID == 0 || strings.TrimSpace(characterName) == "" {
		writer.writeUint32(0)
		writer.writeByte(0)
		writer.writeRawDstr(nil)
		return writer.bytes()
	}
	writer.writeUint32(guildID)
	writer.writeByte(accountSlot)
	writeClientNameDstr(&writer, characterName)
	return writer.bytes()
}

// buildCurrentGuildMembershipNotificationBody follows NoPack sub_1D8D520,
// registered as the class-0 NOTIPACKET_JOIN_GUILD_INFO (0x134) callback. This
// updates the board's pending-guild state, not the actor's membership. The
// actor's typed mode0 tail owns the joined guild ID. The present branch consumes:
//
//	u8 present, u32 guild id, DSTR guild name, DSTR master name,
//	u32 unused, u32 member count, u8 join mode, DSTR promotion
//
// Only the second u32 reaches sub_26AE4C0, the guild member-count setter.
func buildCurrentGuildMembershipNotificationBody(guild *dnfguild.Guild) []byte {
	var writer packetWriter
	if guild == nil {
		writer.writeByte(0)
		return writer.bytes()
	}
	writer.writeByte(1)
	writer.writeUint32(guild.ID)
	writeClientNameDstr(&writer, guild.Name)
	writeClientNameDstr(&writer, guild.MasterName)
	writer.writeUint32(0)
	writer.writeUint32(uint32(len(guild.Members)))
	writer.writeByte(guild.JoinMode)
	writeClientNameDstr(&writer, guild.Promotion)
	return writer.bytes()
}

func buildCurrentGuildAccountListBody(characters []currentGuildCharacterProjection) []byte {
	if len(characters) > 255 {
		characters = characters[:255]
	}
	var writer packetWriter
	writer.writeByte(byte(len(characters)))
	for _, character := range characters {
		writeClientNameDstr(&writer, character.CharacterName)
		writeClientNameDstr(&writer, character.GuildName)
		writer.writeByte(character.ApplicationPending)
	}
	return writer.bytes()
}

// buildCurrentGuildAccountIndexBody follows current NoPack sub_FCEFB0 exactly.
// After the common success marker, op1453 carries complete guild objects and
// then account-character mappings. The three DSTR fields are name, master and
// promotion in that order. Swapping master and promotion makes the current
// client render the promotion as the guild master and leaves recommendation
// rows backed by an incoherent detail object.
//
// The mapping byte reaches sub_FCBB70 -> sub_2272670's region-name lookup.
// This local profile advertises region 0 for every account character.
func buildCurrentGuildAccountIndexBody(guilds []*dnfguild.Guild, characters []currentGuildCharacterProjection) []byte {
	rows := make([]*dnfguild.Guild, 0, min(len(guilds), 255))
	for _, guild := range guilds {
		if guild != nil {
			rows = append(rows, guild)
			if len(rows) == 255 {
				break
			}
		}
	}
	mappings := make([]currentGuildCharacterProjection, 0, min(len(characters), 255))
	for _, character := range characters {
		if character.GuildID != 0 {
			mappings = append(mappings, character)
			if len(mappings) == 255 {
				break
			}
		}
	}
	var writer packetWriter
	writer.writeByte(byte(len(rows)))
	for _, guild := range rows {
		writeCurrentGuildRichRow(&writer, guild)
	}
	writer.writeByte(byte(len(mappings)))
	for _, character := range mappings {
		writer.writeUint32(character.GuildID)
		writer.writeByte(0)
		writeClientNameDstr(&writer, character.CharacterName)
	}
	return writer.bytes()
}

// buildCurrentGuildSearchListBody follows NoPack sub_FCE850 (op1452). After
// the common success byte the client reads:
//
//	u32 total result count, u8 zero-based page, u8 row count,
//	rows of {u32 id, u8 join mode, u32 member limit, u16 member count,
//	         u8 level, DSTR name, DSTR master, DSTR promotion}.
//
// The row is the same rich-guild structure consumed by op1453. Returning only
// the success byte leaves the native page and row fields uninitialised, which
// is why the board displayed 49/26948815, 12336 and six placeholder G icons.
func buildCurrentGuildSearchListBody(total int, page int, guilds []*dnfguild.Guild) []byte {
	if total < 0 {
		total = 0
	}
	if page < 0 {
		page = 0
	}
	if page > 255 {
		page = 255
	}
	rows := make([]*dnfguild.Guild, 0, min(len(guilds), 255))
	for _, guild := range guilds {
		if guild != nil {
			rows = append(rows, guild)
			if len(rows) == 255 {
				break
			}
		}
	}
	var writer packetWriter
	writer.writeUint32(uint32(total))
	writer.writeByte(byte(page))
	writer.writeByte(byte(len(rows)))
	for _, guild := range rows {
		writeCurrentGuildRichRow(&writer, guild)
	}
	return writer.bytes()
}

func writeCurrentGuildRichRow(writer *packetWriter, guild *dnfguild.Guild) {
	writer.writeUint32(guild.ID)
	writer.writeByte(guild.JoinMode)
	writer.writeUint32(currentGuildRichRowTagsMask(guild))
	writer.writeUint16(uint16(min(len(guild.Members), 0xffff)))
	writer.writeByte(byte(min(guild.Level, 0xff)))
	writeClientNameDstr(writer, guild.Name)
	writeClientNameDstr(writer, guild.MasterName)
	writeClientNameDstr(writer, guild.Promotion)
}

// currentGuildRichRowTagsMask returns the u32 the rich guild row carries between
// its join mode and its member count.
//
// Live evidence (2026-09-11, packet_log 15:24:08Z): the slot used to carry the
// member limit. A master had just saved 有爱/欢迎萌新/搬砖 (op740 tags bitmask 7,
// later 0x13 = 有爱/欢迎萌新/决斗场) yet another account's guild board rendered
// 攻坚队/单挑/阿拉德好公民. The historic value, dnfguild.DefaultMemberLimit
// (1000 == 0b1111101000), has exactly bits 3, 5 and 6 set -- the three names the
// board showed, with 决斗场 (bit 4) absent, which is the fingerprint of the client
// reading this u32 as the 公会倾向 mask. The guild-side getter (00FCAFB0) renders
// "公会倾向 : 无" when the mask is zero and otherwise "公会倾向 : " plus one name
// per set bit (sub_228C820 over the mask table {1,2,4,8,16,32,64} for
// 有爱/欢迎萌新/搬砖/攻坚队/决斗场/单挑/阿拉德好公民), so a stored member limit
// shows up as three arbitrary tags. The member limit still reaches the client
// through op70's member-capacity benefit row.
//
// Operators can restore or probe the slot without a rebuild:
//
//	environment DNF90_GUILD_ROW_TAGS_SLOT, or
//	file guild_row_tags_slot.txt, holding "tags" (default), "limit", or a decimal
func currentGuildRichRowTagsMask(guild *dnfguild.Guild) uint32 {
	switch slot := currentGuildRichRowTagsSlotOverride(); slot {
	case "limit":
		if guild == nil {
			return 0
		}
		return uint32(max(guild.MemberLimit, 0))
	case "":
		return currentGuildTagsMask(guild)
	default:
		if parsed, err := strconv.ParseUint(slot, 10, 32); err == nil {
			return uint32(parsed)
		}
		return currentGuildTagsMask(guild)
	}
}

func currentGuildRichRowTagsSlotOverride() string {
	const key = "DNF90_GUILD_ROW_TAGS_SLOT"
	raw := strings.TrimSpace(strings.ToLower(os.Getenv(key)))
	return raw
}

// currentGuildChannelPermissionBit is the grade-table bit the 公会频道设置 dialog
// checks before it sends its save. The dialog itself tests permission 1
// (00FAC0BA push 1 ; 00FAC0BE call sub_26A3BE0, which shifts 1 by that number
// and ANDs it with gradeTable[myGrade].permissions at 026A3C37), so the same bit
// decides whether the button may be enabled at all.
const currentGuildChannelPermissionBit = 1

// currentGuildChannelSettingByte returns op70's byte after the guild-name DSTR,
// which the client stores at guildManager+0x30 and treats as "this viewer may
// open and save the 公会频道设置 dialog".
//
// Live evidence (2026-09-12): with the historic 0 the master could not press the
// button either -- 00FABC98 test eax,eax / je disables it -- while op127 already
// carried the viewer's grade correctly. The viewer's grade-table permission for
// the dialog's own save check is therefore the value that keeps the button in
// step with the save: the master's grade 1 mask (0x3FFFF) has the bit, plain
// members (mask 0) do not, so the tooltip 只有 "会长" 才能使用 stays truthful.
//
// Operators can probe the field without a rebuild:
//
//	environment DNF90_GUILD_CHANNEL_FLAG, or
//	file guild_channel_flag.txt, holding
//	    perm    (default) the viewer's grade-table permission bit, 0 or 1
//	    grade   the viewer's grade byte
//	    master  1 for the guild master, 0 for everyone else
//	    <0-255> that value verbatim
func currentGuildChannelSettingByte(guild *dnfguild.Guild, viewerGrade byte) byte {
	switch mode := currentGuildChannelFlagOverride(); mode {
	case "grade":
		if viewerGrade >= 100 { // sub_1D85270's nonmember sentinel
			return 0
		}
		return viewerGrade
	case "master":
		if guild != nil && viewerGrade == dnfguild.DefaultMasterGrade {
			return 1
		}
		return 0
	case "", "perm":
		if guild == nil || viewerGrade > 6 {
			return 0
		}
		if dnfguild.GradePermissionsFor(guild, viewerGrade)&(1<<currentGuildChannelPermissionBit) != 0 {
			return 1
		}
		return 0
	default:
		if parsed, err := strconv.ParseUint(mode, 10, 8); err == nil {
			return byte(parsed)
		}
		return 0
	}
}

func currentGuildChannelFlagOverride() string {
	const key = "DNF90_GUILD_CHANNEL_FLAG"
	raw := strings.TrimSpace(strings.ToLower(os.Getenv(key)))
	return raw
}

// The byte right after 公会等级 in the op70 body lands in the client's guild
// object at +0x80 (sub_1D8B560 reads it at 01D8B6C5 and stores it at 01D8B6E0;
// the live object at [0x51B2754]+0x80 is 0 while we send 0). The 公会基地 window
// renders 当前公会基地 : %d阶段 (text 21502, 00... 0309FCE0) from a value that no
// decoded packet fills yet, so this slot is the current candidate for it and can
// be driven from a file without a rebuild:
//
//	environment DNF90_GUILD_BASE_STAGE_BYTE, or
//	file guild_base_stage_byte.txt, holding
//	    off    (default) keep the historical 0
//	    stage  the guild's 公会基地 stage (1..5)
//	    <0-255> that value verbatim
func currentGuildBaseStageByte(guild *dnfguild.Guild) byte {
	switch mode := currentGuildBaseStageByteOverride(); mode {
	case "", "off":
		return 0
	case "stage":
		if guild == nil {
			return 0
		}
		return guild.BaseStage
	default:
		if parsed, err := strconv.ParseUint(mode, 10, 8); err == nil {
			return byte(parsed)
		}
		return 0
	}
}

func currentGuildBaseStageByteOverride() string {
	const key = "DNF90_GUILD_BASE_STAGE_BYTE"
	raw := strings.TrimSpace(strings.ToLower(os.Getenv(key)))
	return raw
}

// currentGuildTagsMask collapses the stored tag index list into the bitmask the
// current client renders.
func currentGuildTagsMask(guild *dnfguild.Guild) uint32 {
	if guild == nil {
		return 0
	}
	mask := uint32(0)
	for _, tag := range guild.Tags {
		if tag < 7 {
			mask |= 1 << tag
		}
	}
	return mask
}

// buildCurrentGuildRecommendationBody follows NoPack sub_1D24680 exactly.
// The current client consumes an op762 success envelope, then a u8 row count
// followed by {u32 guild id, DSTR guild name, DSTR guild master name}. These
// summary rows seed the promotion-board model; only after that model exists
// does the client issue per-name op745 detail requests.
func buildCurrentGuildRecommendationBody(guilds []*dnfguild.Guild) []byte {
	rows := make([]*dnfguild.Guild, 0, min(len(guilds), 255))
	for _, guild := range guilds {
		if guild != nil {
			rows = append(rows, guild)
			if len(rows) == 255 {
				break
			}
		}
	}
	var writer packetWriter
	writer.writeByte(byte(len(rows)))
	for _, guild := range rows {
		writer.writeUint32(guild.ID)
		writeClientNameDstr(&writer, guild.Name)
		writeClientNameDstr(&writer, guild.MasterName)
	}
	return writer.bytes()
}

type currentGuildCharacterProjection struct {
	CharacterName      string
	GuildName          string
	GuildID            uint32
	ApplicationPending byte
}

func buildCurrentGuildSearchResultBody(guild *dnfguild.Guild) []byte {
	var writer packetWriter
	writer.writeUint32(guild.ID)
	writeClientNameDstr(&writer, guild.Name)
	writer.writeUint16(uint16(len(guild.Members)))
	writeClientNameDstr(&writer, guild.MasterName)
	writer.writeUint32(uint32(guild.Level))
	tags := guild.Tags
	if len(tags) > 255 {
		tags = tags[:255]
	}
	writer.writeByte(byte(len(tags)))
	for _, tag := range tags {
		writer.writeByte(tag)
	}
	writeClientNameDstr(&writer, guild.Promotion)
	writer.writeByte(guild.JoinMode)
	writer.writeUint32(uint32(guild.MemberLimit))
	return writer.bytes()
}

// currentGuildMemberOfflineSeconds is how long the member has been away, which
// is the only field in the row that separates an offline member from an online
// one -- the channel byte 0xff merely turns into member+0x34 == -1, which the
// window renders as a flag rather than dropping the row. An online member
// reports zero. An offline member whose last-online mark was never stamped
// falls back to its join time so the value is still non-zero; reporting zero
// there would make it look online.
func currentGuildMemberOfflineSeconds(
	member dnfguild.Member,
	online bool,
	now time.Time,
) uint32 {
	if online {
		return 0
	}
	since := member.LastOnlineAt
	if since.IsZero() {
		since = member.JoinedAt
	}
	if since.IsZero() {
		return 0
	}
	elapsed := now.Sub(since).Seconds()
	if elapsed <= 0 {
		return 0
	}
	if elapsed >= float64(^uint32(0)) {
		return ^uint32(0)
	}
	return uint32(elapsed)
}

// currentGuildRowKeyBytes returns the two row bytes that sit between the
// advancement byte and the channel byte of a guild member record.
//
// Confirmed live on 2026-09-12: with 0/0 the member window renders
// "ch181.未知"; with the advertised server id in the first byte the window
// renders "ch181.<频道名>".  The first byte is therefore member+0x31, the
// primary key of the channel-name lookup performed by the guild channel column
//
//	currentGuildRowKeyBytes, and its value is the advertised server id -- the
//	same value the channel directory packet advertises in
//	writeChannelInfoPrefix (instance.json: channelAdvertiseServerIndex, default
//	1, which is also channelServerID()'s fallback).
//
// The second byte lands at member+0x30 and becomes a boolean at member+0x180
// (sub_1D21FD0: cmp byte [ebp-0x281],0 / seta).  member+0x180 is read by the
// member-list builder (00FAAD90, gates drawing
// Interface/newstyle/windows/community/friend.img) and by the channel-follow
// handler (00FB52D3, passed as a flag into sub_325F590 -> op804).  It is NOT
// used by the channel-name lookup, so the default mirrors the server id, which
// is the combination verified live.
//
// Operators can still override both without a rebuild:
//
//	environment DNF90_GUILD_ROW_KEY, or
//	file guild_row_key.txt, holding 1 or 2 space separated decimals:
//	    <member+0x30> [member+0x31]     (0 0 restores the historic wire form)
const guildRowKeyDefaultServerID = byte(1)

func currentGuildRowKeyBytes() (byte, byte) {
	const key = "DNF90_GUILD_ROW_KEY"
	raw := strings.TrimSpace(os.Getenv(key))
	if raw == "" {
		return guildRowKeyDefaultServerID, guildRowKeyDefaultServerID
	}
	fields := strings.Fields(raw)
	if len(fields) > 2 {
		fields = fields[:2]
	}
	values := make([]byte, 0, 2)
	for _, field := range fields {
		parsed, err := strconv.Atoi(field)
		if err != nil || parsed < 0 || parsed > 255 {
			return guildRowKeyDefaultServerID, guildRowKeyDefaultServerID
		}
		values = append(values, byte(parsed))
	}
	var first, second byte
	if len(values) > 0 {
		first = values[0]
	}
	if len(values) > 1 {
		second = values[1]
	} else {
		second = first
	}
	return first, second
}

// buildCurrentGuildMemberListBody follows NoPack sub_1D21FD0 exactly. The
// current handler consumes the common success marker before these fields.
func buildCurrentGuildMemberListBody(guild *dnfguild.Guild, includeGuildIdentity, includeCursor bool, onlineChannels map[string]byte, now time.Time) []byte {
	var writer packetWriter
	if guild == nil {
		writer.writeUint32(0)
		if includeGuildIdentity {
			writer.writeRawDstr(nil)
			writer.writeUint16(0)
		}
		writer.writeUint16(0)
		if includeCursor {
			writer.writeUint32(0)
		}
		return writer.bytes()
	}
	// sub_269A120 returns the actor's guild ID, not its character ID.
	writer.writeUint32(guild.ID)
	if includeGuildIdentity {
		writeClientNameDstr(&writer, guild.Name)
		writer.writeUint16(uint16(min(len(guild.Members), 0xffff)))
	}
	members := dnfguild.SortedMembers(guild)
	if len(members) > 0xffff {
		members = members[:0xffff]
	}
	writer.writeUint16(uint16(len(members)))
	for _, member := range members {
		writer.writeUint32(currentGuildNumericID(member.CharacterID))
		writeClientNameDstr(&writer, member.Name)
		writeClientNameDstr(&writer, member.Memo)
		writer.writeUint16(uint16(member.Level))
		writer.writeByte(byte(numericCharacterStat(member.Job)))
		// Advancement byte. sub_1D21FD0 reads it here, copies it through
		// sub_3A51780 and splits it into a low nibble and bits 4..6 exactly
		// like the role-select card does, so sending 0 rendered every member
		// as their base class.
		writer.writeByte(byte(numericCharacterStat(member.GrowType)))
		// The two bytes between the advancement byte and the channel byte land
		// at member+0x30 and member+0x31.  sub_1D21FD0 reads them as the 3rd and
		// 4th byte of the row and hands the 3rd one to sub_1D2218C -> [ebp-0x290]
		// -> member+0x31, which the guild window's channel column then uses as
		// the FIRST key of sub_1892CA0(guildChannelStore = 0x5188FF0, key1, key2):
		//
		//	00FA7730: mov eax,[esi+0x34]        ; channel  (key2)
		//	          movzx ecx,byte [esi+0x31] ; key1
		//	          call sub_18A5110 ; call sub_1892CA0
		//	          if (result == 0) name = sub_367BE00(0x26C)  ; "未知"
		//	          else             name = *(wstring*)(result + 4)
		//	          sprintf("ch%02d.%s", channel, name)
		//
		// Sending 0 for that byte makes every lookup miss, so every online member
		// renders "ch181.未知" instead of "ch181.<频道名>".  currentGuildRowKeyBytes
		// lets an operator sweep the correct value without a rebuild.
		key1, key2 := currentGuildRowKeyBytes()
		writer.writeByte(key1)
		writer.writeByte(key2)
		channel := currentGuildMemberOfflineChannel
		online := false
		if encoded, found := onlineChannels[member.CharacterID]; found {
			channel = encoded
			online = true
		}
		writer.writeByte(channel)
		writer.writeByte(0)
		writer.writeByte(0)
		writer.writeByte(member.Grade) // sub_1D21FD0 -> member+324
		// Seconds this member has been offline, not mileage. sub_1D21FD0 stores
		// this u32 at member+0x154, immediately before the roster key at
		// +0x158, and 86JP's GuildPacketBuilder writes OfflineSeconds here:
		// zero for an online member, now - lastOnline for an offline one. This
		// tree sent Mileage, which is always 0, so an offline member looked
		// exactly like an online one in the only field that distinguishes them.
		writer.writeUint32(currentGuildMemberOfflineSeconds(member, online, now))
		// Roster key. sub_1D21FD0 stores this u32 at member+0x158 and
		// sub_26A9C30 uses it as the key of the guild's std::map at
		// guild+0x2D8 (its size lives at guild+0x2E4, cleared together with
		// the member vector by sub_26A90E0). Sending 0 for every row made all
		// members collapse onto one map entry, so the window rendered exactly
		// one member. usdof_v54 `opcode0043-guild_member_list` writes
		// stage3_guild_member_roster_key(member.character_id) in this slot.
		writer.writeUint32(currentGuildNumericID(member.CharacterID))
		// Offline-listable marker. sub_FB4910 rebuilds the member window in
		// several passes over the roster: the first keeps rows whose
		// member+0x3C resolved to a live peer, the second keeps rows that are
		// unresolved but still carry a channel, and the third
		// (00FB4C89..00FB4D0F) is the only pass that can take an offline row
		// -- it requires member+0x3C == 0, member+0x34 < 0 *and*
		// member+0x15C != 0 (00FB4C95 cmp dword [eax+0x15c],0 / je skip).
		// This byte lands at member+0x15C (sub_1D21FD0 reads it right after
		// the roster key at 01D2221E and zero-extends it at 01D2234C), and
		// sending 0 dropped every offline member before the window ever saw
		// it -- which is why the "show offline members" checkbox looked dead.
		// The checkbox itself was never the problem: 00FB4F19 only skips the
		// closing erase pass that would remove those same rows again.
		// Only offline rows need the marker. The manual sort in sub_FAD7F0
		// calls sub_FA6F00, which orders any record with +0x15C != 0 ahead of
		// every other row before it looks at the sort column, so marking
		// online rows too would pin the whole list. 1 is the value the client
		// itself tests for (00FC1813 / 00F9E3BD cmp ...,1 / sete).
		offlineListable := byte(0)
		if !online {
			offlineListable = 1
		}
		writer.writeByte(offlineListable)
		writer.writeRawDstr(nil)
		// Second member view (00FC4E20..) repeats the same offline test and
		// additionally requires member+0x181 != 0 (00FC4E35 / 00FC4F2B).
		// sub_1D21FD0 turns this byte into that flag with a setne (01D223AF).
		writer.writeByte(offlineListable)
		// member+0x182, the region byte. It has to match the op1454 settings
		// row and be non-zero for the member context menu to send anything;
		// see currentGuildMemberRegion.
		writer.writeByte(currentGuildMemberRegion)
	}
	if includeCursor {
		writer.writeUint32(0)
	}
	return writer.bytes()
}

// Class 0/op70, sub_1D8B560. The fixed grade table is six {u32 permissions,
// char name[24]} rows. All optional lists/flags keep their exact empty encoding.
func buildCurrentGuildManagementBody(guild *dnfguild.Guild, onlineCount int, now time.Time, viewerGrade byte) []byte {
	if guild == nil {
		guild = &dnfguild.Guild{}
	}
	var writer packetWriter
	writeClientNameDstr(&writer, guild.Name)
	// sub_1D8B560 reads this byte into guildManager+0x30 (01D8B5F2..01D8B5F7).
	// The 公会频道设置 dialog is the only window that gates a button on it:
	// 00FABC93 call [mgr+0x30] ; 00FABC98 test eax,eax ; je -> disabled, and the
	// disabled tooltip is string 31583 "只有 \"%s\" 才能使用。" with the grade-1
	// name from the op70 grade table. Sending 0 greyed the button for everyone,
	// the master included; see currentGuildChannelSettingByte.
	writer.writeByte(currentGuildChannelSettingByte(guild, viewerGrade))
	writer.writeByte(0)
	writer.writeUint16(uint16(min(len(guild.Members), 0xffff)))
	writer.writeByte(0)
	// Guild notice. sub_1D8B560 reads this DSTR into a 256-unit buffer,
	// assigns it to guild+0x18 and pops it once per session through a
	// template string; it is the field the notice edit box is refilled from.
	// The 1024-unit DSTR after the experience u32 is the separate
	// introduction slot (guild+0x20).
	writeClientNameDstr(&writer, guild.Notice)
	writer.writeByte(byte(guild.Level)) // actor template +844
	writer.writeByte(0)
	// guild+0x80 in the client: currentGuildBaseStageByte documents the probe.
	writer.writeByte(currentGuildBaseStageByte(guild))
	writer.writeByte(guild.Mark) // sub_26AE4F0 updates the displayed guild mark
	writer.writeUint32(guild.Experience)
	// Guild master, as shown in the window's 会长 field. sub_1D8B560 assigns
	// this DSTR to guild+0x20 and the board (sub_00FCC5E0 at 00FCC721) pours
	// guild+0x20 into that text control. Live proof: while this slot carried
	// the misrouted op154 text the 会长 field rendered exactly that text, and
	// the notice slot above rendered exactly its own. The next DSTR is a
	// 17-unit name that sub_2690140 stores on the local actor instead, so the
	// master name has to be written twice.
	writeClientNameDstr(&writer, guild.MasterName)
	writeClientNameDstr(&writer, guild.MasterName)
	writer.writeUint16(uint16(min(onlineCount, 0xffff)))
	writeClientNameDstr(&writer, guild.Promotion)
	for grade := byte(0); grade < 6; grade++ {
		// op748 edits these; dnfguild.GradePermissionsFor falls back to the
		// fixed table this used to hardcode for grades the master never
		// touched.
		writer.writeUint32(dnfguild.GradePermissionsFor(guild, grade))
		var name []byte
		for _, r := range guild.GradeNames[grade] {
			encoded := rosterNameBytes(string(r))
			if len(name)+len(encoded) > 23 {
				break
			}
			name = append(name, encoded...)
		}
		writeFixedBytes(&writer, name, 23)
		writer.writeByte(0)
	}
	// 公会福利 rows. Upstream emits one row per ACTIVE guild benefit, in a fixed
	// content order; this tree only ever wrote 6 (member capacity) and 21
	// (contract), so a guild that bought 金库扩容 / 公会 BUFF / 公会传送次数 saw an
	// empty 公会福利 panel for them even though the benefit was persisted and in
	// force. Each row is {u8 type, u32 expires(unix, 0 = never), u8 mode, u32 value}.
	type benefitRow struct {
		typ     byte
		expires time.Time
		mode    byte
		value   uint32
	}
	rows := make([]benefitRow, 0, 8)
	if guild.MemberLimit > 0 {
		rows = append(rows, benefitRow{typ: 6, value: uint32(guild.MemberLimit)})
	}
	if contract, found := guild.Benefits[21]; found && contract.Active(now) {
		rows = append(rows, benefitRow{typ: 21, expires: contract.ExpiresAt, mode: contract.Mode, value: contract.Value})
	}
	if guild.CargoLimit > dnfguild.DefaultCargoLimit {
		// Type 4 is 公会金库扩容 and its value is the ABSOLUTE row count, which is
		// what the client renders; only report it once it differs from the default.
		rows = append(rows, benefitRow{typ: 4, value: uint32(guild.CargoLimit)})
	}
	// Keep content order stable; the native client treats each row as a separate
	// active guild benefit. Types 3 and 5 are the two Guild_Status HUD rows.
	for _, typ := range []byte{1, 2, 3, 5, 7} {
		if benefit, found := guild.Benefits[typ]; found && benefit.Active(now) {
			rows = append(rows, benefitRow{typ: typ, expires: benefit.ExpiresAt, mode: benefit.Mode, value: benefit.Value})
		}
	}
	writer.writeByte(byte(len(rows)))
	for _, row := range rows {
		expires := uint32(0)
		if !row.expires.IsZero() {
			expires = uint32(row.expires.Unix())
		}
		writer.writeByte(row.typ)
		writer.writeUint32(expires)
		writer.writeByte(row.mode)
		writer.writeUint32(row.value)
	}
	writer.writeUint32(guild.Mileage)
	writer.writeByte(0)
	writer.writeUint32(0)
	writer.writeByte(0)
	writer.writeUint32(guild.RecommendedRegion)
	writer.writeUint32(guild.RecommendedChannel)
	writer.writeUint32(guild.AuraMark)
	writer.writeUint32(guild.AuraFrame)
	writer.writeUint16(0)
	writer.writeByte(boolByte(guild.AllianceRecruitment))
	writer.writeByte(0)
	tags := currentGuildTagsMask(guild)
	writer.writeUint32(tags)
	writer.writeByte(guild.JoinMode)
	return writer.bytes()
}

// buildCurrentGuildApplicationListBody projects op352's applicant rows.
// sub_1D30B10 reads a u32 count and then, per row: u32 character id,
// DSTR name, u8, u8, u8, u8, DSTR message, u32, u8, u8. The four bytes between
// the name and the message are a character card, and the client maps them by
// position (record base = ebp-0x86C in the parser, fields at record+0x24..):
//
//   - read 1 (01D30C0A -> record+0x24, 01D30D10): the job byte. The list's row
//     builder loads record+0x24 and record+0x25 and hands both to the class
//     call at 03197BFB (sub_249D7F0), so a level in this slot renders the
//     applicant with another class's avatar.
//
//   - read 2 (01D30C18 -> sub_3A51780 at 01D30C8D, split into record+0x25 grow
//     and record+0x26 advancement): the packed advancement byte, the same value
//     the member row carries (member.GrowType); sending 0 rendered every
//     applicant as their base class.
//
//   - read 3 (01D30C26 -> record+0x27): the level. The applicant window builds
//     the hover bubble from exactly this byte:
//     03197DAF movzx ecx,byte [eax+0x27] ; 03197DB4 push 0x42AA1F4 ('Lv.%d').
//     Before this fix the level sat in read 1 and read 3 carried 0, which is why
//     the bubble read "Lv.0" and the card showed the wrong class.
//
//   - read 4 (01D30C34 -> record+0x28): not mapped yet and stays 0.
//
//   - the last read (01D30C68) lands at record+0x54 (01D30C94), and that is
//     the field the approve/deny request echoes back: the sender at 03198789
//     pushes [esi] as the character id and [esi+0x54] as the second field.
//     decideCurrentGuildApplication rejects any request whose region byte is
//     not channelAdvertiseID(), so sending 0 here made every 通过/拒绝 fail
//     with the generic error -- confirmed by the captured op350 body
//     2b000000 00 (character 43, region 0). Read 5 (01D30C5A) becomes the
//     boolean at record+0x50.
func buildCurrentGuildApplicationListBody(applications []dnfguild.Application) []byte {
	var writer packetWriter
	writer.writeUint32(uint32(len(applications)))
	for _, application := range applications {
		writer.writeUint32(currentGuildNumericID(application.CharacterID))
		writeClientNameDstr(&writer, application.Name)
		writer.writeByte(byte(numericCharacterStat(application.Job)))
		writer.writeByte(byte(numericCharacterStat(application.GrowType)))
		writer.writeByte(byte(application.Level))
		writer.writeByte(0)
		writeClientNameDstr(&writer, application.Message)
		writer.writeUint32(uint32(application.CreatedAt.Unix()))
		writer.writeByte(0)
		writer.writeByte(currentGuildMemberRegion)
	}
	return writer.bytes()
}

// currentGuildMileageHistoryBody builds class1/op747's PAYLOAD, i.e. what
// follows the success byte the transport already writes: the client's handler
// 0x01D11310 reads a u32 row count and then copies that many rows verbatim as
// 48-byte structs (call 0x34E3AC0 with size 0x30 at 01D11380).
//
// Two live failures shaped this. (1) The handler used to answer with the bare
// success byte, so the client read the count out of invalid memory and memcpyed
// 48 bytes per bogus row: 0xC0000005 eip 0x03B10747, eax=0x098B101F,
// access_address=0x098B1000 at 2026-09-12 00:44:36, the moment 公会积分 was
// clicked. (2) Writing the success byte here as well shifted the count by one
// byte -- the window then rendered one row of leftover memory (a zero-filled name
// column, "getStr(20952)==none+8" in the content column, 1995.08.15 for the
// date) -- so this body must start at the count.
//
// The row struct is partly mapped from a live probe (2026-09-12, switch
// guild_mileage_rows.txt=3) plus the client's own initialiser. Probe rows carried
// character id 1000+i, name "P<i>", amount 111+i and time 1700000000+i, and the
// window showed:
//
//	角色名  P1 P2 P3                  -> +0x04 is the fixed name buffer
//	日期/时间 1970.01.01 08:16:40+i    -> +0x00 is the unix timestamp, and 1000
//	                                     seconds lands on 1970-01-01 08:16:40 in
//	                                     this client's local (UTC+8) rendering
//	内容   公会成员每日签到 + 1700000000 -> +0x28 is the points figure
//
// The content column also resolves a reason text (the string block 20991..20999:
// 20991 拒绝占领战申请, 20992 使用积分累计道具, 20993 占领战申请, 20994 公会擂台战,
// 20995 公会内容, 20996 金币捐赠, 20997 通关公会推荐地下城, 20998 公会成员每日签到,
// 20999 加入公会) through the 0x30-byte row, which is what the garbage row exposed
// when it printed the DLL's fallback "getStr(20952)==none" debug text.
//
//	+0x00 u32 unix seconds
//	+0x04 28-byte fixed character-name buffer (GBK, NUL padded)
//	+0x20 u16 0 (unmapped)
//	+0x22 u8  reason -- the client turns this byte into a text id as
//	     id = 21000 - byte, pinned twice live: byte 0 rendered the text of 21000
//	     and byte 0x0B rendered 20989 公会会长每日签到, which is why every row
//	     (member rows included) showed the master's wording until the pick below
//	     started sending 2 (21000 - 2 = 20998 公会成员每日签到) for non-masters.
//	+0x24 u32 character id
//	+0x28 u32 points
//	+0x2C u8  0
//	+0x2D .. 0x2F padding
//
// Rows are emitted for the stored history by default; the mode switch is only a
// safety valve and a way to map the remaining reason values.
func currentGuildMileageHistoryBody(rows [][]byte) []byte {
	valid := make([][]byte, 0, len(rows))
	for _, row := range rows {
		if len(row) == currentGuildMileageRowSize {
			valid = append(valid, row)
		}
	}
	var writer packetWriter
	writer.writeUint32(uint32(len(valid)))
	for _, row := range valid {
		writer.writeBytes(row)
	}
	return writer.bytes()
}

const currentGuildMileageRowSize = 0x30

// currentGuildMileageRowKind is the 会长签到 reason byte. The client maps the row
// byte to a text id as id = 21000 - byte, which the live captures pinned twice:
// byte 0 rendered the text of 21000 and byte 0x0B rendered 20989
// (公会会长每日签到), so the master's own attendance row carries 0x0B.
const currentGuildMileageRowKind = byte(0x0B)

// currentGuildMileageMemberReason is the 公会成员每日签到 reason byte for every
// member that is not the guild master: 21000 - 2 = 20998.
const currentGuildMileageMemberReason = byte(2)

// currentGuildMileageReasonByte picks the reason row byte for one history row.
// A row that recorded its own reason keeps it (公会聊天 10 / 同公会成员组队 12);
// the older 签到 rows carry zero and fall back per role.
func currentGuildMileageReasonByte(guild *dnfguild.Guild, entry dnfguild.MileageHistory) byte {
	if entry.Reason != 0 {
		return entry.Reason
	}
	if guild != nil && guild.MasterCharacterID != "" && entry.CharacterID == guild.MasterCharacterID {
		return currentGuildMileageRowKind
	}
	return currentGuildMileageMemberReason
}

func currentGuildMileageRow(characterID uint32, name string, reason byte, amount, unixSeconds uint32) []byte {
	row := make([]byte, currentGuildMileageRowSize)
	binary.LittleEndian.PutUint32(row[0x00:], unixSeconds)
	encoded := rosterNameBytes(name)
	if len(encoded) > 27 {
		encoded = encoded[:27]
	}
	copy(row[0x04:0x20], encoded)
	row[0x22] = reason
	binary.LittleEndian.PutUint32(row[0x24:], characterID)
	binary.LittleEndian.PutUint32(row[0x28:], amount)
	return row
}

func buildCurrentGuildJoinCheckBody(guild *dnfguild.Guild) []byte {
	var writer packetWriter
	if guild == nil {
		writer.writeRawDstr(nil)
		writer.writeRawDstr(nil)
		writer.writeUint32(0)
		writer.writeUint32(0)
		writer.writeByte(0)
		return writer.bytes()
	}
	writeClientNameDstr(&writer, guild.Name)
	writeClientNameDstr(&writer, guild.MasterName)
	writer.writeUint32(uint32(len(guild.Members)))
	writer.writeUint32(uint32(guild.MemberLimit))
	writer.writeByte(guild.JoinMode)
	return writer.bytes()
}

func buildCurrentGuildApplyBody(guild *dnfguild.Guild) []byte {
	var writer packetWriter
	writer.writeUint32(guild.ID)
	writeClientNameDstr(&writer, guild.Name)
	writeClientNameDstr(&writer, guild.MasterName)
	writer.writeUint32(uint32(len(guild.Members)))
	writer.writeByte(guild.JoinMode)
	return writer.bytes()
}

// buildCurrentGuildAttendanceDetailBody projects 今日签到成员 (op746).
//
// sub_1D04F90 (registered for 746 at 01D372AD) reads a u16 row count and then,
// per row, one length-prefixed client name through sub_3457D20 with a 0x80
// buffer, appending each name to the guild manager (sub_26AA5A0). The previous
// u32 zero happened to be safe -- its first two bytes are the u16 zero, so the
// loop never ran -- but it could never report a signed-in member either, which is
// what 今日签到成员 (%d/%d) displays.
func buildCurrentGuildAttendanceDetailBody(guild *dnfguild.Guild, now time.Time) []byte {
	var writer packetWriter
	names := currentGuildAttendanceNames(guild, now)
	if len(names) > 0xffff {
		names = names[:0xffff]
	}
	writer.writeUint16(uint16(len(names)))
	for _, name := range names {
		writeClientNameDstr(&writer, name)
	}
	return writer.bytes()
}

// currentGuildAttendanceNames lists the members whose AttendedAt falls on the
// same local day as now, the set Registry.Attend stamps once per day.
func currentGuildAttendanceNames(guild *dnfguild.Guild, now time.Time) []string {
	if guild == nil {
		return nil
	}
	year, month, day := now.In(now.Location()).Date()
	names := make([]string, 0, len(guild.Members))
	for _, member := range dnfguild.SortedMembers(guild) {
		if member.AttendedAt.IsZero() {
			continue
		}
		attendedYear, attendedMonth, attendedDay := member.AttendedAt.In(now.Location()).Date()
		if attendedYear != year || attendedMonth != month || attendedDay != day {
			continue
		}
		names = append(names, member.Name)
	}
	return names
}

// buildCurrentGuildRefreshInfoBody follows NoPack sub_1D0ECD0. The parser
// consumes two u8 state flags, one u32 value and one trailing u8 flag. The
// first flag asks the client to immediately issue another op339 when set, so
// the stable projection must keep it clear. The remaining optional systems
// are not represented by the local guild aggregate and are sent as their
// exact empty states rather than fabricated values.
func buildCurrentGuildRefreshInfoBody() []byte {
	var writer packetWriter
	writer.writeByte(0)
	writer.writeByte(0)
	writer.writeUint32(0)
	writer.writeByte(0)
	return writer.bytes()
}

// buildCurrentGuildBoardEmptyBody follows NoPack sub_1D0EE50's exact empty
// branch: the first u8 selects the normal board result and the second u8 says
// that no board record is present.
func buildCurrentGuildBoardEmptyBody() []byte {
	return []byte{0, 0}
}

// buildCurrentGuildAttendanceBody follows the empty lists accepted by NoPack
// sub_1D34610. Both list counts are read unconditionally by the client.
func buildCurrentGuildAttendanceBody() []byte {
	return []byte{0, 0}
}

func buildCurrentGuildInfoBody(characterID uint32, guild *dnfguild.Guild) []byte {
	var writer packetWriter
	writer.writeUint32(characterID)
	if guild == nil {
		writer.writeRawDstr(nil)
	} else {
		writeClientNameDstr(&writer, guild.Name)
	}
	return writer.bytes()
}

// buildCurrentGuildLegacyGradeBody follows NoPack sub_1D22770. The first
// status byte is consumed for notification formatting, then the client uses
// the member selector, character name and target grade to update its model.
func buildCurrentGuildLegacyGradeBody(memberSelector byte, name string, grade byte) []byte {
	var writer packetWriter
	writer.writeByte(0)
	writer.writeByte(memberSelector)
	writeClientNameDstr(&writer, name)
	writer.writeByte(grade)
	return writer.bytes()
}

// buildCurrentGuildGradeBody projects the member roster op1454/op1455 carry.
// NoPack sub_1D259E0 (op1454 tail-jumps into it from sub_1D268D0) consumes the
// guild id, an op1454-only u16, a u16 row count, then {u8 region, DSTR name,
// u8} rows, each becoming a 0x30-byte record in the global container at
// 0x517970C.
//
// The DSTR is a *member character name*, not a grade name. sub_FBE4D0 walks
// the guild member records (sub_26A3810 counts them at stride 0x184) and joins
// each one against these rows on (row string == record name, row+4 ==
// record+0x182) before copying record+0x31 into row+0x2C, and sub_FBE700 looks
// a row up by that same (name, region) pair. This used to send the four grade
// names with the grade number in the region byte, so the member settings list
// rendered 会长/副会长/优秀会员/普通会员 as though they were four members and
// no row ever joined a real record -- the grade shown per row comes from the
// joined record, which is why every fake row collapsed into one heading.
//
// firstPage distinguishes the two opcodes. The op1454-only u16 is stored at
// 0x51796EC by sub_FBD600, which then clears the container (sub_FBCFA0), so
// op1454 is the first page and op1455 appends to it: that u16 is the roster
// total, and op1455 must answer with an empty page or every member would be
// listed twice.
//
// row+0x28 is the member's grade. sub_FBE280 is the tab filter and copies a
// row into the visible list only when the selected tab matches it: tab 1 wants
// 2, tab 2 wants 3, and tab 3 wants 4 or 0 (00FBE359, 00FBE364, 00FBE36F).
// Sending 0 for everyone therefore piled the whole roster under 普通会员.
func buildCurrentGuildGradeBody(guild *dnfguild.Guild, firstPage bool) []byte {
	var writer packetWriter
	if guild == nil {
		writer.writeUint32(0)
		if firstPage {
			writer.writeUint16(0)
		}
		writer.writeUint16(0)
		return writer.bytes()
	}
	members := dnfguild.SortedMembers(guild)
	if len(members) > 0xffff {
		members = members[:0xffff]
	}
	writer.writeUint32(guild.ID)
	if !firstPage {
		// Everything went out with op1454; terminate the paging.
		writer.writeUint16(0)
		return writer.bytes()
	}
	writer.writeUint16(uint16(len(members)))
	writer.writeUint16(uint16(len(members)))
	for _, member := range members {
		// Must equal the region byte the member row carries at member+0x182.
		writer.writeByte(currentGuildMemberRegion)
		writeClientNameDstr(&writer, member.Name)
		writer.writeByte(member.Grade)
	}
	return writer.bytes()
}
