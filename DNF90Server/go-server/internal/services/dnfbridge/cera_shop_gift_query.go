package dnfbridge

import (
	"context"
	"encoding/binary"
	"errors"
	"fmt"
	"strconv"
	"strings"

	dnfenum "longheng.io/server/internal/modules/dnf/dnfenum"
	dnfproto "longheng.io/server/internal/modules/dnf/protocol"
)

// 商城「赠送」的收件人查询（legacy cmd=1 type=136 = ENUM_CMDPACKET_QUERY_CHARAC_INFO）。
//
// 为什么需要：当前客户端在玩家确认收件人姓名之后**先问服务端要这个角色**，拿到回包
// 才画「- 赠送结果 -」/「确认赠送礼物吗？」对话框；本服原先对 136 没有任何分支，
// 于是既不回落也不回包，客户端永远停在原地（现场 2026-09-27 15:32）。
// 现场收包与客户端逆向见 `商城赠送_确认收件人后无响应_排查_20260927.md`。
//
// 请求体（现场抓包 + 客户端发包函数 RVA 0x02CAEC50 双向印证）：
//
//	DSTR 角色名 | u8 赠送类型
//
// 回包（class-1 同号 136，与邮件收件人查询 op324 同一套 class-1 信封）：
//
//	u8 1                      成功字节；客户端 class-1 分发器会先吃掉它，
//	                          并把它的值当第二个参数交给处理器（0 = 失败分支）
//	DSTR 角色名（原样回显）     客户端读包器 RVA 0x1903E90 第一项就是它
//	u16  等级                  → 窗口 +0x1FC，必须非 0，否则「赠送」按钮保持灰
//	u8   职业                  → 窗口 +0x200；**结果对话框的「职业」栏就是拿它去查表**
//	u8   grow                  → 窗口 +0x204（全类扫描：只有写，没有任何读）
//	u8   对方已收礼物数          → 窗口 +0x208；客户端判 [+0x230]+该值 <= 上限
//	u8   1                     → 作为 bool 传给结果对话框（客户端自己的初始化值就是 1）
//	u8   0                     → 窗口 +0x20C
//	u8   0                     → 结果对话框最后一位参数（客户端初始化值为 0）
//
// 字段宽度/顺序来自对客户端读包器的逐条还原（RVA 0x1903E90：0x3057D20 读 DSTR、
// 0x30E39E0 读 u16、0x30E3970 读 u8），并已用「结果对话框 RVA 0x02CAF740 的
// `ret 0x40` = 16 个 dword 形参」把每个字段落到对话框的哪一个参数上对齐过。
//
// 「职业」栏的位置是**真机实测**定下来的：2026-09-27 16:13 首版把 grow 放进了
// +0x200 那一格，客户端「职业」栏空白（该角色 job=0/grow=35）。客户端侧证据：
// 结果对话框把 +0x200 的值交给 RVA 0x209D7F0，该函数用第一个参数去查一张表
// （0x305A0B0 → 0x31F4B90(table, key)）再拼「职业」文案；35 查不到，0（鬼剑士）
// 查得到。所以 +0x200 = 职业，grow 退到没有读者的 +0x204。
// 这 5 个字段在客户端里**只有写、只有那两处灰化判据会读**，不参与任何发包构造，
// 所以即使某个字节的语义还没钉死，也不会把礼物送错人。
const (
	currentCeraShopGiftQueryOpcode = uint16(dnfenum.CmdPacketQueryCharacInfo)
	// 客户端读包器把回显的名字拷进一个 0x14(20) 字节预算的缓冲（RVA 0x3057D20
	// -> 0x3057C80）。超过预算时客户端**不会消费**名字字节、游标错位，后面 7 个
	// 字段全会读错 —— 所以服务端也绝不超过这个预算，宁可回失败让客户端弹明确提示。
	currentCeraShopGiftQueryMaxNameBytes = 20
)

// op136 回包的**结果码** = 回包第一个字节。客户端 class-1 分发器先吃掉它，并把它
// 当第二个参数交给结果对话框 FUN_02CAF740，由它的 switch（`mov eax,[ebp+8]`）
// 决定弹哪一句提示。
//
// 这张表是逆向出来的、不是猜的（把 switch 的立即数逐个回查字符串表）：
//
//	7    → 5559  无法对自身赠送礼物
//	2    → 5560  请稍后再使用
//	0x15 → 5558  该ID不存在
//	0xA3 → 70107 每个账号每天最多可以赠送10万点券的礼物
//	0xA4 → 70108 成为好友7天后才能赠送礼物
//	0xA5 → 70109 您与该用户还不是好友关系
//
// 成功 = 1：现场 2026-09-27 16:10 真机验证，回 `01 + 8 个字段` 时客户端正常画出
// 「职业」「等级」。所以**失败必须回一个上面列出的码**，回 0 会落进 switch 的
// default 分支（既不是成功也不是任何已知提示）。
const (
	currentCeraShopGiftQueryCodeSuccess      byte = 0x01
	currentCeraShopGiftQueryCodeRetry        byte = 0x02
	currentCeraShopGiftQueryCodeSelf         byte = 0x07
	currentCeraShopGiftQueryCodeNotFound     byte = 0x15
	currentCeraShopGiftQueryCodeDailyCap     byte = 0xA3
	currentCeraShopGiftQueryCodeFriendTooNew byte = 0xA4
	currentCeraShopGiftQueryCodeNotFriend    byte = 0xA5
)

var (
	errCurrentCeraShopGiftQueryMalformed = errors.New("dnf cera shop gift recipient query is malformed")
	errCurrentCeraShopGiftQueryName      = errors.New("dnf cera shop gift recipient query name is invalid")
)

// currentCeraShopGiftQueryRequest 是 136 的请求体。
type currentCeraShopGiftQueryRequest struct {
	Name    string
	NameRaw []byte
	Kind    byte
	RawLen  int
}

func parseCurrentCeraShopGiftQuery(body []byte) (currentCeraShopGiftQueryRequest, error) {
	if len(body) < 5 {
		return currentCeraShopGiftQueryRequest{}, errCurrentCeraShopGiftQueryMalformed
	}
	length := int(binary.LittleEndian.Uint32(body[:4]))
	// 现场实测 body = `04 00 00 00 '9999' 01`：u32 字节长度 + DSTR 字节 + u8 类型。
	// 多一个字节少一个字节都判非法——畸形包不能落到「猜」的分支上。
	if length <= 0 || 4+length+1 != len(body) {
		return currentCeraShopGiftQueryRequest{}, errCurrentCeraShopGiftQueryMalformed
	}
	raw := body[4 : 4+length]
	if strings.IndexByte(string(raw), 0) >= 0 {
		return currentCeraShopGiftQueryRequest{}, errCurrentCeraShopGiftQueryName
	}
	name, err := decodeCharacterName(raw)
	if err != nil || strings.TrimSpace(name) == "" {
		return currentCeraShopGiftQueryRequest{}, fmt.Errorf("%w: %v", errCurrentCeraShopGiftQueryName, err)
	}
	return currentCeraShopGiftQueryRequest{
		Name:    name,
		NameRaw: append([]byte(nil), raw...),
		Kind:    body[len(body)-1],
		RawLen:  len(body),
	}, nil
}

// buildCurrentCeraShopGiftQueryAck 生成成功回包（含 class-1 成功字节）。
//
// 参数顺序 = 回包字段顺序：等级 → 职业 → grow。职业必须放第 4 个字段
// （窗口 +0x200），那是客户端「职业」栏唯一会去查表的位置。
func buildCurrentCeraShopGiftQueryAck(
	nameRaw []byte,
	level uint16,
	job byte,
	grow byte,
	receivedGifts byte,
) []byte {
	var writer packetWriter
	writer.writeByte(currentCeraShopGiftQueryCodeSuccess)
	writer.writeRawDstr(nameRaw)
	writer.writeUint16(level)
	writer.writeByte(job)
	writer.writeByte(grow)
	writer.writeByte(receivedGifts)
	writer.writeByte(1)
	writer.writeByte(0)
	writer.writeByte(0)
	return writer.bytes()
}

// buildCurrentCeraShopGiftQueryFailure 生成失败回包：`u8 0 + u8 结果码`（**两个字节**）。
//
// ⚠ 这里曾经只回一个字节（直接就是结果码），那是错的，而且现场 2026-09-27 17:17 真的
// 撞上了：class-1 回包的**第一个字节是标志位**（1 = 成功、0 = 失败），客户端看到
// `a5` 这种非 0 值会走**成功分支**，于是去读一个根本没发过去的 name/level/职业，
// 对话框画出来的就是垃圾值（"Lv 12336"、职业空白），玩家完全看不懂发生了什么。
//
// 形状与全服其它 class-1 失败包一致（`sendGameUpperFailure`、`character_create_transport`、
// `compound_recipe`、`guild_compound`… 全是 `{0, code}`）：失败分支先吃掉标志位 0，
// **然后才读结果码**，再拿它去查文案表。
//
// code 必须是上面那张码表里的失败码。传 0/1（或任何表外的值）一律退化成
// 「请稍后再使用」：那是表里语义最弱的一句，任何情况下弹它都不会误导玩家。
func buildCurrentCeraShopGiftQueryFailure(code byte) []byte {
	switch code {
	case currentCeraShopGiftQueryCodeRetry,
		currentCeraShopGiftQueryCodeSelf,
		currentCeraShopGiftQueryCodeNotFound,
		currentCeraShopGiftQueryCodeDailyCap,
		currentCeraShopGiftQueryCodeFriendTooNew,
		currentCeraShopGiftQueryCodeNotFriend:
		return []byte{0, code}
	default:
		return []byte{0, currentCeraShopGiftQueryCodeRetry}
	}
}

func (s *Service) handleCurrentCeraShopGiftQuery(session *gameSession, body []byte) error {
	request, err := parseCurrentCeraShopGiftQuery(body)
	if err != nil {
		s.logGameEvent(session, "game-cera-shop-gift-query-rejected",
			"body_len", len(body), "reason", err)
		return s.sendCurrentCeraShopGiftQueryFailure(session, currentCeraShopGiftQueryCodeRetry, "malformed")
	}
	// 赠送功能暂时关闭：**不查库、不判规则**，直接给提示 + 失败回包。
	// 提示必须走服务端公告（客户端结果码表里没有"功能关闭"这句话），但**回包也必须发**：
	// 不回的话客户端会停在等待状态（就是 2026-09-27 那次"确认收件人后没反应"）。
	if !currentCeraShopGiftEnabled {
		if noticeErr := s.sendCurrentCeraShopGiftClosedNotice(session, "cera_shop_gift_query_disabled"); noticeErr != nil {
			s.logGameEvent(session, "game-cera-shop-gift-closed-notice-failed",
				"char_id", session.selectedCharacterID,
				"error", noticeErr)
		}
		s.logGameEvent(session, "game-cera-shop-gift-query-rejected",
			"char_id", session.selectedCharacterID,
			"name", request.Name,
			"kind", request.Kind,
			"reason", "gift_feature_disabled")
		return s.sendCurrentCeraShopGiftQueryFailure(session, currentCeraShopGiftDisabledFailureCode, "gift_feature_disabled")
	}
	// 客户端自己读不回这么长的名字（见上面的预算常量）。这里必须回失败而不是回成功：
	// 回成功会让客户端读包游标停在名字中间，后面 7 个字段全错位。
	if len(request.NameRaw) >= currentCeraShopGiftQueryMaxNameBytes {
		s.logGameEvent(session, "game-cera-shop-gift-query-rejected",
			"char_id", session.selectedCharacterID,
			"name_byte_len", len(request.NameRaw),
			"limit", currentCeraShopGiftQueryMaxNameBytes,
			"reason", "recipient name exceeds the client reply reader budget")
		return s.sendCurrentCeraShopGiftQueryFailure(session, currentCeraShopGiftQueryCodeNotFound, "name_too_long")
	}
	ctx, cancel := context.WithTimeout(context.Background(), createWriteTimeout)
	defer cancel()
	character, found, err := s.lookupCurrentCeraShopGiftRecipient(ctx, request.Name)
	if err != nil {
		s.logGameEvent(session, "game-cera-shop-gift-query-failed",
			"char_id", session.selectedCharacterID,
			"name", request.Name,
			"kind", request.Kind,
			"reason", "lookup_error",
			"error", err)
		return s.sendCurrentCeraShopGiftQueryFailure(session, currentCeraShopGiftQueryCodeRetry, "lookup_error")
	}
	if !found {
		// 查无此人：客户端弹「该ID不存在」。
		s.logGameEvent(session, "game-cera-shop-gift-query-rejected",
			"char_id", session.selectedCharacterID,
			"name", request.Name,
			"kind", request.Kind,
			"reason", "recipient_not_found")
		return s.sendCurrentCeraShopGiftQueryFailure(session, currentCeraShopGiftQueryCodeNotFound, "recipient_not_found")
	}
	// 赠送规则（互为好友满 7 天 / 同大区 / 不能送自己）：判定失败时回**客户端自己
	// 那套文案对应的结果码**，玩家看到的是「成为好友7天后才能赠送礼物」这类明确
	// 提示，而不是静默或者一句无关的报错。
	code, err := s.evaluateCurrentCeraShopGiftQueryRules(ctx, session, request.Name, character)
	if err != nil {
		s.logGameEvent(session, "game-cera-shop-gift-query-failed",
			"char_id", session.selectedCharacterID,
			"name", request.Name,
			"reason", "rule_evaluation_error",
			"error", err)
		return s.sendCurrentCeraShopGiftQueryFailure(session, currentCeraShopGiftQueryCodeRetry, "rule_evaluation_error")
	}
	if code != 0 {
		s.logGameEvent(session, "game-cera-shop-gift-query-rule-rejected",
			"char_id", session.selectedCharacterID,
			"name", request.Name,
			"kind", request.Kind,
			"target_character_id", character.characterID,
			"code", code,
			"reason", currentCeraShopGiftQueryCodeReason(code))
		return s.sendCurrentCeraShopGiftQueryFailure(session, code, currentCeraShopGiftQueryCodeReason(code))
	}
	ack := buildCurrentCeraShopGiftQueryAck(
		request.NameRaw,
		character.level,
		character.job,
		character.grow,
		0,
	)
	if err := s.sendGameUpperRawClass(
		session,
		currentCeraShopGiftQueryOpcode,
		ack,
		dnfproto.DefaultChannelClassification,
	); err != nil {
		return err
	}
	// 记下**整包 hex**：客户端把回包怎么用（尤其 u16 是等级还是 characNo）只能靠
	// 真机复验，日志里没有原始字节就没法对照，下一轮只能重新逆向。
	s.logGameEvent(session, "game-cera-shop-gift-query-acked",
		"char_id", session.selectedCharacterID,
		"name", request.Name,
		"kind", request.Kind,
		"target_character_id", character.characterID,
		"level", character.level,
		"grow", character.grow,
		"job", character.job,
		"body_len", len(ack),
		"body_hex", fmt.Sprintf("%x", ack))
	return nil
}

// currentCeraShopGiftQueryCodeReason 把结果码翻成日志里的可读原因（只用于日志）。
func currentCeraShopGiftQueryCodeReason(code byte) string {
	switch code {
	case currentCeraShopGiftQueryCodeSelf:
		return "self_gift"
	case currentCeraShopGiftQueryCodeNotFound:
		return "recipient_not_found_or_cross_region"
	case currentCeraShopGiftQueryCodeDailyCap:
		return "daily_gift_amount_cap"
	case currentCeraShopGiftQueryCodeFriendTooNew:
		return "friend_younger_than_seven_days"
	case currentCeraShopGiftQueryCodeNotFriend:
		return "not_mutual_friend"
	case currentCeraShopGiftQueryCodeRetry:
		return "retry_later"
	default:
		return fmt.Sprintf("code_%#x", code)
	}
}

func (s *Service) sendCurrentCeraShopGiftQueryFailure(session *gameSession, code byte, reason string) error {
	body := buildCurrentCeraShopGiftQueryFailure(code)
	if err := s.sendGameUpperRawClass(
		session,
		currentCeraShopGiftQueryOpcode,
		body,
		dnfproto.DefaultChannelClassification,
	); err != nil {
		return err
	}
	s.logGameEvent(session, "game-cera-shop-gift-query-failure-acked",
		"char_id", session.selectedCharacterID,
		"code", code,
		"reason", reason,
		"body_hex", fmt.Sprintf("%x", body))
	return nil
}

type currentCeraShopGiftRecipient struct {
	characterID string
	level       uint16
	grow        byte
	job         byte
	// hasJob 区分"职业就是 0（鬼剑士）"和"这行数据解析不出职业"。
	// 规则 4（双方基础职业相同）只在两边都解析得出来时才判定。
	hasJob      bool
	serverGroup int64
}

// lookupCurrentCeraShopGiftRecipient 按角色名解析收件人投影。
//
// 只读：不写库、不改任何状态。可见性不做「同频道/在线」限制——商城赠送本来就允许
// 送给不在线/别频道的角色（礼物走邮件），这一点与官方 `DB_ReqQueryCharacInfo` 一致。
// **大区**限制是另一条业务规则，由 evaluateCurrentCeraShopGiftQueryRules 判定。
func (s *Service) lookupCurrentCeraShopGiftRecipient(ctx context.Context, name string) (currentCeraShopGiftRecipient, bool, error) {
	repositories, ok := s.repositoryGroup()
	if !ok || repositories.Character == nil {
		return currentCeraShopGiftRecipient{}, false, errors.New("dnf cera shop gift recipient repository is unavailable")
	}
	if ctx == nil {
		ctx = context.Background()
	}
	characterID, found, err := repositories.Character.FindIDByName(ctx, name)
	if err != nil {
		return currentCeraShopGiftRecipient{}, false, err
	}
	if !found || strings.TrimSpace(characterID) == "" {
		return currentCeraShopGiftRecipient{}, false, nil
	}
	record, found, err := repositories.Character.Load(ctx, characterID)
	if err != nil {
		return currentCeraShopGiftRecipient{}, false, err
	}
	if !found {
		return currentCeraShopGiftRecipient{}, false, nil
	}
	level := record.Level
	if level <= 0 || level > 0xffff {
		return currentCeraShopGiftRecipient{}, false, fmt.Errorf("recipient %s has an unusable level %d", characterID, record.Level)
	}
	jobValue, jobErr := strconv.ParseUint(strings.TrimSpace(record.Job), 10, 8)
	growValue, hasGrow := record.Stats["grow_type"]
	if !hasGrow || growValue < 0 || growValue > 0xff {
		return currentCeraShopGiftRecipient{}, false, fmt.Errorf("recipient %s has an invalid grow_type", characterID)
	}
	return currentCeraShopGiftRecipient{
		characterID: characterID,
		level:       uint16(level),
		grow:        byte(growValue),
		job:         byte(jobValue),
		hasJob:      jobErr == nil,
		serverGroup: record.Stats["server_group_id"],
	}, true, nil
}
