// channel_follow.go 实现「频道跟随」(class-1 opcode 804 = REQUEST_USER_CHANNEL)。
//
// 语义与格式全部来自客户端反汇编 + 活进程验证, 证据见
// WorkBuddy/2026-09-10-15-53-33/channel_follow_re_notes.md:
//
//	上行 legacy cmd=1 type=804, body = u8 region + dstr 目标名
//	下行必须是 class-1 同号 804(不是 class-0 的同号包, 那是另一个功能),
//	body = u8 mode + u8 flag + dstr 目标名 + u8 serverId + u8 channelId
//
// 客户端侧链路(真机验证: 目标在 171 频道时跟随成功):
//
//  1. class-1 处理器 sub_3250DC0: mode != 0 时读包体, 经构造函数 0x324F920 落到
//     「跟随状态对象」([[0x51B2764]+0x60]):
//     +0x54 = flag, +0x58 = 名字, +0x78 = serverId, +0x7C = channelId。
//  2. 换频道时客户端先用自己的频道表筛大区(0x1898AC0 比对 [频道对象+4]),
//     再按频道号查目标(0x324E3F0 比对 [频道对象+0x70])。
//  3. 目标不在线时回状态包(mode == 0 + u8 state), 客户端把 100 映射成
//     「缺少目标角色。」(字符串 0x8226)。
package dnfbridge

import (
	"context"
	"strings"

	"longheng.io/server/internal/modules/dnf/dnfenum"
	dnfproto "longheng.io/server/internal/modules/dnf/protocol"
)

const (
	// currentChannelFollowDataMode 是响应首字节(客户端分发给处理器的 a2 参数)。
	// 非 0 才会走「读包体填跟随状态」的分支。
	currentChannelFollowDataMode = 1
	// currentChannelFollowFlag 是响应第 2 字节(客户端落到 +0x54)。
	// 换频道链路不消费它, 作为占位/结果位固定为 1。
	currentChannelFollowFlag = 1
	// currentChannelFollowStateMode 是「只设置跟随状态」的分支(客户端 a2 == 0)。
	currentChannelFollowStateMode = 0
	// currentChannelFollowMissingTargetState 是目标不可用时的跟随状态。
	// 客户端状态门把 100 映射成本地化串 0x8226「缺少目标角色。」。
	currentChannelFollowMissingTargetState = 100
)

// currentChannelFollowRequest 是客户端「频道跟随」上行包。
type currentChannelFollowRequest struct {
	region byte
	name   string
}

// decodeCurrentChannelFollowRequest 解析 u8 region + dstr 目标名。
func decodeCurrentChannelFollowRequest(body []byte) (currentChannelFollowRequest, bool) {
	if len(body) < 1 {
		return currentChannelFollowRequest{}, false
	}
	raw, _, ok := readCurrentRawDstr(body, 1)
	if !ok {
		return currentChannelFollowRequest{}, false
	}
	name, err := decodeCharacterName(raw)
	if err != nil || strings.TrimSpace(name) == "" {
		return currentChannelFollowRequest{}, false
	}
	return currentChannelFollowRequest{region: body[0], name: name}, true
}

// handleCurrentUserChannelRequest 是 legacy/handleGameCommand 的 804 分支。
func (s *Service) handleCurrentUserChannelRequest(session *gameSession, body []byte) error {
	opcode := uint16(dnfenum.CmdPacketRequestUserChannel)
	request, ok := decodeCurrentChannelFollowRequest(body)
	if !ok {
		s.logGameEvent(session, "game-channel-follow-request-rejected",
			"char_id", session.selectedCharacterID,
			"body_len", len(body),
			"reason", "current_client_u8_region_plus_dstr_name_shape_required")
		return nil
	}
	target, found := s.currentOnlinePlayerByName(request.name)
	if !found {
		// 目标不在线/查不到: 只回状态包, 客户端自己弹「缺少目标角色。」。
		s.logPacketEvent("game-channel-follow-target-missing",
			"conn_id", session.connID,
			"channel_id", session.channel.ID,
			"source_char_id", session.selectedCharacterID,
			"target_name", request.name,
			"request_region", request.region,
			"state", currentChannelFollowMissingTargetState,
			"msg_id", opcode,
			"classification", dnfproto.DefaultChannelClassification)
		return s.sendGameUpperRawClass(session, opcode,
			buildCurrentChannelFollowStateBody(currentChannelFollowMissingTargetState),
			dnfproto.DefaultChannelClassification)
	}

	serverID := s.currentChannelFollowServerID(target)
	s.logPacketEvent("game-channel-follow-response-send",
		"conn_id", session.connID,
		"channel_id", session.channel.ID,
		"source_char_id", session.selectedCharacterID,
		"target_char_id", target.CharacterID,
		"target_name", target.Name,
		"target_channel_id", target.ChannelID,
		"target_town_id", target.TownID,
		"request_region", request.region,
		"response_flag", currentChannelFollowFlag,
		"response_server_id", serverID,
		"response_channel", byte(target.ChannelID),
		"response_mode", currentChannelFollowDataMode,
		"msg_id", opcode,
		"classification", dnfproto.DefaultChannelClassification)

	return s.sendGameUpperRawClass(session, opcode,
		buildCurrentChannelFollowDataBody(currentChannelFollowDataMode,
			currentChannelFollowFlag, target.Name, serverID, byte(target.ChannelID)),
		dnfproto.DefaultChannelClassification)
}

// currentChannelFollowServerID 返回目标频道所属的大区(服务器)ID。
//
// 取自 channel_info.etc —— 客户端频道表里每个频道对象的大区字段就是它;
// 不能用请求里那个 region 字节代替, 两者在本服并不等价。
func (s *Service) currentChannelFollowServerID(target onlinePlayerInfo) byte {
	// 必须走带锁的 currentCatalog()：频道热重载会在 s.mu 下整体替换 s.catalog，
	// 直接读字段属于数据竞争（旧代码只在启动时写一次，所以一直没暴露）。
	if s != nil && target.ChannelID != 0 {
		if catalog := s.currentCatalog(); catalog != nil {
			if channel, ok := catalog.Channel(int(target.ChannelID)); ok && channel.ServerID > 0 {
				return byte(channel.ServerID)
			}
		}
	}
	if s != nil {
		return byte(s.channelServerID())
	}
	return 1
}

// currentOnlinePlayerByName 按角色名查在线玩家。
//
// 先用在线玩家表(覆盖城镇/场景内已登记的玩家), 命中即可拿到频道号;
// 未命中再退回账号仓储: 角色名 -> 角色 ID -> 在线会话 -> 会话频道。
// 仓储回退会为每个在线会话查一次库, 因此放在第二步。
func (s *Service) currentOnlinePlayerByName(name string) (onlinePlayerInfo, bool) {
	if s == nil || name == "" {
		return onlinePlayerInfo{}, false
	}
	sessions := s.currentOnlineChatSessions()
	if s.onlinePlayers != nil {
		for _, candidate := range sessions {
			if candidate == nil || candidate.selectedCharacterID == 0 {
				continue
			}
			info, ok := s.onlinePlayers.PlayerForCharacter(candidate.selectedCharacterID)
			if ok && info.Name == name {
				return info, true
			}
		}
	}
	for _, candidate := range sessions {
		if candidate == nil || candidate.selectedCharacterID == 0 {
			continue
		}
		sessionName, ok := s.currentSessionCharacterName(candidate)
		if !ok || sessionName != name {
			continue
		}
		return onlinePlayerInfo{
			CharacterID: candidate.selectedCharacterID,
			AccountID:   candidate.accountID,
			Name:        sessionName,
			ChannelID:   uint16(candidate.channel.ID),
		}, true
	}
	return onlinePlayerInfo{}, false
}

// currentSessionCharacterName 用账号仓储把在线会话解析成角色名。
func (s *Service) currentSessionCharacterName(session *gameSession) (string, bool) {
	if s == nil || session == nil || session.selectedCharacterID == 0 {
		return "", false
	}
	ctx, cancel := context.WithTimeout(context.Background(), createWriteTimeout)
	defer cancel()
	_, name, _, found := s.selectedCharacterForEnter(ctx, session)
	if !found || name == "" {
		return "", false
	}
	return name, true
}

// buildCurrentChannelFollowDataBody 构造 class-1 op804 的「数据」响应体。
//
// 字段顺序由客户端处理器 sub_3250DC0 的读取顺序 + 构造函数 0x324F920 的落点决定,
// 真机验证(目标在 171 频道时跟随成功):
//
//	u8 mode    = a2, 非 0 才会走读包体填状态对象的分支
//	u8 flag    = 落在 +0x54, 换频道链路不消费
//	dstr name  = 目标角色名
//	u8 server  = 落在 +0x78, 用于筛客户端频道表(比对频道对象的大区字段)
//	u8 channel = 落在 +0x7C, 用于在筛出的频道里查目标频道(比对频道号字段)
func buildCurrentChannelFollowDataBody(mode, flag byte, name string, serverID, channel byte) []byte {
	var writer packetWriter
	writer.writeByte(mode)
	writer.writeByte(flag)
	writeClientNameDstr(&writer, name)
	writer.writeByte(serverID)
	writer.writeByte(channel)
	return writer.bytes()
}

// buildCurrentChannelFollowStateBody 构造「只设置跟随状态」的响应体(mode == 0)。
func buildCurrentChannelFollowStateBody(state byte) []byte {
	return []byte{currentChannelFollowStateMode, state}
}
