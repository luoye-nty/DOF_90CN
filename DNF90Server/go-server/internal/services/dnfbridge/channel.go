// channel.go 负责 DNF bridge 频道服 7001 的握手、兼容包解析和频道目录响应。
package dnfbridge

import (
	"bufio"
	"context"
	"encoding/binary"
	"errors"
	"fmt"
	"io"
	"math"
	"net"
	"os"
	"strings"
	"syscall"
	"time"

	"longheng.io/server/internal/modules/dnf/channelcatalog"
	"longheng.io/server/internal/modules/dnf/dnfenum"
	dnfproto "longheng.io/server/internal/modules/dnf/protocol"
)

type channelSession struct {
	conn                         net.Conn
	connID                       string
	packetSeq                    uint64
	seq                          uint16
	scriptSent                   bool
	closeAfterBootstrapDirectory bool
}

// Channel bootstrap can pause while the current client loads the downloaded
// script, initializes the patch DLLs, or is scanned by endpoint security.
// Eight seconds was too aggressive on cold/low-end release machines and made
// the native client fall back to its generic "connecting server" retry UI.
const channelReadIdleTimeout = 30 * time.Second

func (s *Service) handleChannelConn(ctx context.Context, conn net.Conn) {
	defer conn.Close()
	session := &channelSession{conn: conn, connID: s.nextPacketConnID("channel")}
	reader := bufio.NewReader(conn)
	s.logInfo("dnfbridge channel connection accepted",
		"conn_id", session.connID,
		"remote", conn.RemoteAddr().String(),
		"local", conn.LocalAddr().String())
	s.logPacketEvent("channel-connect",
		"conn_id", session.connID,
		"remote", conn.RemoteAddr().String(),
		"local", conn.LocalAddr().String())
	defer s.logPacketEvent("channel-close",
		"conn_id", session.connID,
		"remote", conn.RemoteAddr().String(),
		"local", conn.LocalAddr().String())
	setChannelReadDeadline(conn)
	if err := s.acceptDofLoginPreface(reader, session); err != nil {
		s.logChannelReadError(ctx, conn, err)
		return
	}
	for {
		setChannelReadDeadline(conn)
		handled, err := s.handleLegacyChannelRequest(reader, session)
		if err != nil {
			s.logChannelReadError(ctx, conn, err)
			return
		}
		if handled {
			if session.closeAfterBootstrapDirectory {
				s.logPacketEvent("channel-bootstrap-directory-committed",
					"conn_id", session.connID,
					"remote", session.conn.RemoteAddr().String())
				return
			}
			continue
		}
		setChannelReadDeadline(conn)
		packet, err := s.readChannelPacket(reader, session)
		if err != nil {
			s.logChannelReadError(ctx, conn, err)
			return
		}
		if err := s.handleChannelPacket(session, packet); err != nil {
			s.logWarn("dnfbridge handle channel packet failed", "msg_id", packet.Header.MsgID, "error", err)
			return
		}
	}
}

func setChannelReadDeadline(conn net.Conn) {
	_ = conn.SetReadDeadline(time.Now().Add(channelReadIdleTimeout))
}

func (s *Service) acceptDofLoginPreface(reader *bufio.Reader, session *channelSession) error {
	header, err := reader.Peek(dnfproto.LegacyChannelHeaderSize)
	if err != nil {
		return err
	}
	if !dnfproto.IsDofLoginPrefacePrefix(header) {
		return nil
	}
	raw := make([]byte, dnfproto.DofLoginPrefaceSize)
	if _, err := io.ReadFull(reader, raw); err != nil {
		return err
	}
	s.logChannelPacket(session, "RECV", "channel-dof-preface", raw)
	ack := dnfproto.BuildDofLoginAck(s.aesKey())
	s.logChannelPacket(session, "SEND", "channel-dof-ack", ack)
	_, err = session.conn.Write(ack)
	return err
}

func (s *Service) handleLegacyChannelRequest(reader *bufio.Reader, session *channelSession) (bool, error) {
	header, err := reader.Peek(dnfproto.LegacyChannelHeaderSize)
	if err != nil {
		return false, err
	}
	if dnfproto.IsDofChannelProbePrefix(header) {
		raw := make([]byte, dnfproto.LegacyChannelHeaderSize)
		if _, err := io.ReadFull(reader, raw); err != nil {
			return false, err
		}
		s.logChannelPacket(session, "RECV", "channel-dof-get-script", raw)
		return true, s.sendLegacyScript(session)
	}
	if !dnfproto.IsDofAskChannelPrefix(header) {
		return false, nil
	}
	raw := make([]byte, dnfproto.DofAskChannelSize)
	if _, err := io.ReadFull(reader, raw); err != nil {
		return false, err
	}
	s.logChannelPacket(session, "RECV", "channel-dof-ask", raw)
	bootstrap := session.scriptSent
	body, err := s.buildChannelInfoBodyFor(dnfproto.LegacyPayload(raw), bootstrap)
	if err != nil {
		return true, err
	}
	packet := dnfproto.BuildLegacyChannelPacket(dnfenum.LegacyMsgChannelInfo, body)
	s.logChannelPacket(session, "SEND", "channel-dof-info", packet, "msg_id", dnfenum.LegacyMsgChannelInfo)
	if _, err := session.conn.Write(packet); err != nil {
		return true, err
	}
	s.logPacketEvent("channel-dof-info-sent",
		"conn_id", session.connID,
		"remote", session.conn.RemoteAddr().String(),
		"local", session.conn.LocalAddr().String(),
		"bootstrap", bootstrap)
	session.closeAfterBootstrapDirectory = bootstrap
	return true, nil
}

func (s *Service) sendLegacyScript(session *channelSession) error {
	body, err := s.buildScriptBody()
	if err != nil {
		return err
	}
	packet := dnfproto.BuildLegacyChannelPacket(dnfenum.LegacyMsgDofScript, body)
	s.logChannelPacket(session, "SEND", "channel-dof-script", packet, "msg_id", dnfenum.LegacyMsgDofScript)
	if _, err := session.conn.Write(packet); err != nil {
		return err
	}
	session.scriptSent = true
	return nil
}

func (s *Service) readChannelPacket(reader io.Reader, session *channelSession) (dnfproto.ChannelPacket, error) {
	header := make([]byte, dnfproto.ChannelHeaderSize)
	if _, err := io.ReadFull(reader, header); err != nil {
		return dnfproto.ChannelPacket{}, err
	}
	if packet, ok, err := s.tryReadMixedLegacyChannelPacket(reader, header, session); ok || err != nil {
		return packet, err
	}
	length := binary.LittleEndian.Uint32(header[3:7])
	maxPacketBytes := s.options.maxPacketBytes
	if maxPacketBytes <= 0 {
		maxPacketBytes = defaultMaxPacketBytes
	}
	if length < dnfproto.ChannelHeaderSize || int(length) > maxPacketBytes {
		s.logChannelPacket(session, "RECV", "channel-invalid-header", header)
		return dnfproto.ChannelPacket{}, dnfproto.ErrPacketLength
	}
	packet := make([]byte, int(length))
	copy(packet, header)
	if _, err := io.ReadFull(reader, packet[dnfproto.ChannelHeaderSize:]); err != nil {
		return dnfproto.ChannelPacket{}, err
	}
	parsed, err := dnfproto.ParseChannelPacket(packet)
	if err != nil {
		s.logChannelPacket(session, "RECV", "channel-invalid", packet)
		return dnfproto.ChannelPacket{}, err
	}
	s.logChannelPacket(session, "RECV", "channel", packet,
		"msg_id", parsed.Header.MsgID,
		"header_seq", parsed.Header.Seq,
		"body_len", len(parsed.Body))
	return parsed, nil
}

func (s *Service) tryReadMixedLegacyChannelPacket(reader io.Reader, header []byte, session *channelSession) (dnfproto.ChannelPacket, bool, error) {
	if len(header) < dnfproto.ChannelHeaderSize || header[0] == dnfproto.DefaultChannelClassification {
		return dnfproto.ChannelPacket{}, false, nil
	}
	bodyLen := int(binary.BigEndian.Uint16(header[0:2]))
	msgID := binary.LittleEndian.Uint16(header[2:4])
	total := 4 + bodyLen
	if !isMixedLegacyChannelMsg(msgID) || total < dnfproto.ChannelHeaderSize {
		return dnfproto.ChannelPacket{}, false, nil
	}
	maxPacketBytes := s.options.maxPacketBytes
	if maxPacketBytes <= 0 {
		maxPacketBytes = defaultMaxPacketBytes
	}
	if total > maxPacketBytes {
		s.logChannelPacket(session, "RECV", "channel-mixed-legacy-invalid", header, "msg_id", msgID)
		return dnfproto.ChannelPacket{}, true, dnfproto.ErrPacketLength
	}
	raw := make([]byte, total)
	copy(raw, header)
	if total > dnfproto.ChannelHeaderSize {
		if _, err := io.ReadFull(reader, raw[dnfproto.ChannelHeaderSize:]); err != nil {
			return dnfproto.ChannelPacket{}, true, err
		}
	}
	body := append([]byte(nil), raw[4:]...)
	s.logChannelPacket(session, "RECV", "channel-mixed-legacy", raw,
		"msg_id", msgID,
		"body_len", len(body))
	return dnfproto.ChannelPacket{
		Header: dnfproto.ChannelHeader{
			Classification: dnfproto.DefaultChannelClassification,
			MsgID:          msgID,
			Length:         uint32(total),
			Seq:            mixedLegacySequence(body),
		},
		Body: body,
	}, true, nil
}

func isMixedLegacyChannelMsg(msgID uint16) bool {
	switch dnfenum.ChannelMsg(msgID) {
	case dnfenum.MsgCSConnect, dnfenum.MsgCSAskChannelInfoNew:
		return true
	default:
		return false
	}
}

func mixedLegacySequence(body []byte) uint16 {
	for _, value := range body {
		if value != 0 {
			return uint16(value)
		}
	}
	return 1
}

func (s *Service) logChannelReadError(ctx context.Context, conn net.Conn, err error) {
	if !isQuietChannelReadError(err) && ctx.Err() == nil {
		s.logWarn("dnfbridge read channel packet failed", "remote", conn.RemoteAddr().String(), "error", err)
	}
}

func isQuietChannelReadError(err error) bool {
	var netErr net.Error
	if errors.As(err, &netErr) && netErr.Timeout() {
		return true
	}
	return errors.Is(err, io.EOF) ||
		errors.Is(err, net.ErrClosed) ||
		errors.Is(err, syscall.ECONNRESET)
}

func (s *Service) handleChannelPacket(session *channelSession, packet dnfproto.ChannelPacket) error {
	switch dnfenum.ChannelMsg(packet.Header.MsgID) {
	case dnfenum.MsgCSUpdateChannelInfo:
		body, err := s.buildChannelUpdateBody(packet.Body)
		if err != nil {
			return err
		}
		return s.sendChannelClass(session, uint16(dnfenum.MsgSCAskChannelInfo), body, packet.Header.Classification)
	case dnfenum.MsgCSConnect:
		return s.sendChannel(session, uint16(dnfenum.MsgSCConnect), s.buildConnectBody())
	case dnfenum.MsgCSCheckScript:
		body, err := s.buildScriptVersionBody()
		if err != nil {
			return err
		}
		return s.sendChannel(session, uint16(dnfenum.MsgSCCheckScript), body)
	case dnfenum.MsgCSGetScript:
		return s.sendChannelScript(session)
	case dnfenum.MsgCSAskChannelInfoNew:
		if !session.scriptSent {
			if err := s.sendChannelScript(session); err != nil {
				return err
			}
		}
		body, err := s.buildChannelInfoBodyFor(packet.Body, true)
		if err != nil {
			return err
		}
		return s.sendChannel(session, uint16(dnfenum.MsgSCAskChannelInfoNew), body)
	default:
		return nil
	}
}

// sendChannelScript 用最新频道包下发 channel_info.etc，只影响客户端频道静态表加载，不写玩家状态。
func (s *Service) sendChannelScript(session *channelSession) error {
	body, err := s.buildScriptBody()
	if err != nil {
		return err
	}
	if err := s.sendChannel(session, uint16(dnfenum.MsgSCGetScript), body); err != nil {
		return err
	}
	session.scriptSent = true
	return nil
}

func (s *Service) sendChannel(session *channelSession, msgID uint16, body []byte) error {
	return s.sendChannelClass(session, msgID, body, dnfproto.DefaultChannelClassification)
}

func (s *Service) sendChannelClass(session *channelSession, msgID uint16, body []byte, class byte) error {
	packet, err := dnfproto.BuildChannelPacket(msgID, body, session.seq, dnfproto.DefaultChannelClassification)
	if err != nil {
		return err
	}
	if class != 0 && class != dnfproto.DefaultChannelClassification {
		packet, err = dnfproto.BuildChannelPacket(msgID, body, session.seq, class)
		if err != nil {
			return err
		}
	}
	session.seq++
	s.logChannelPacket(session, "SEND", "channel", packet,
		"msg_id", msgID,
		"header_seq", session.seq-1,
		"body_len", len(body))
	_, err = session.conn.Write(packet)
	return err
}

func (s *Service) logChannelPacket(session *channelSession, direction string, kind string, data []byte, fields ...any) {
	if session == nil {
		s.logPacket(direction, kind, data, fields...)
		return
	}
	session.packetSeq++
	args := make([]any, 0, len(fields)+4)
	args = append(args, "conn_id", session.connID, "pkt_seq", session.packetSeq)
	args = append(args, fields...)
	s.logPacket(direction, kind, data, args...)
}

func (s *Service) buildConnectBody() []byte {
	key := s.aesKey()
	body := make([]byte, 36)
	copy(body[4:], []byte(key))
	return body
}

func (s *Service) buildScriptVersionBody() ([]byte, error) {
	raw := make([]byte, 20)
	copy(raw[4:], []byte(s.options.scriptVersion))
	return encryptChannelData(raw, s.aesKey(), false)
}

func (s *Service) buildScriptBody() ([]byte, error) {
	script := s.currentChannelScript()
	if len(script) == 0 {
		raw, err := osReadFile(s.options.channelInfoFile)
		if err != nil {
			return nil, err
		}
		script, err = s.buildChannelScript(raw)
		if err != nil {
			return nil, fmt.Errorf("build 90CN online channel script: %w", err)
		}
	}
	return encryptChannelData(script, s.aesKey(), true)
}

func (s *Service) buildChannelUpdateBody(_ []byte) ([]byte, error) {
	channel, err := s.loginChannel()
	if err != nil {
		return nil, err
	}
	var writer packetWriter
	writer.writeUint32(0)
	writer.buffer.Write(fixedASCII(s.options.serverIP, 16))
	writer.writeInt32(channel.Port)
	writer.writeInt32(channel.MaxUsers)
	writer.writeInt32(channel.CurrentUsers)
	return writer.bytes(), nil
}

// currentSpecialAreaChannelType 特殊区域频道类型。
//
// 线上 channel_info.etc 里 121（ancient 远古）与 171（deathtower 绝望之塔）都是 type 22。
// 服主口径（2026-09-30）：任何时刻必须至少有一个 22 类型频道处于开启状态——运维工具的关闭守卫
// （channelstore.checkDisableTargets）会拒绝关掉最后一个，服务端这边只告警，让手改表的人也能看到。
//
// 注意别用 dnfenum.DeathTowerChannelType（=11）：那份 ETC 里 type 11 是普通城镇频道，
// 这个常量错位的问题在 channelcatalog.isResidentChannel 的注释里已经记过一次。
const currentSpecialAreaChannelType = 22

func catalogHasSpecialAreaChannel(catalog *channelcatalog.Catalog) bool {
	if catalog == nil {
		return false
	}
	for _, channel := range catalog.Channels() {
		if int(channel.Type) == currentSpecialAreaChannelType {
			return true
		}
	}
	return false
}

// currentChannelTableWarnings 返回"表能跑、但玩家侧或运维约定会受影响"的告警。
// 启动与热重载共用；includeDefaultTown=false 时跳过"缺默认城镇频道"那条
// （严格模式已经把它当硬错误拒绝了，不该再重复告警一次）。
func currentChannelTableWarnings(catalog *channelcatalog.Catalog, includeDefaultTown bool) []string {
	if catalog == nil {
		return nil
	}
	warnings := make([]string, 0, 2)
	if includeDefaultTown {
		if _, ok := catalog.Channel(dnfenum.DefaultGameChannelID); !ok {
			warnings = append(warnings, fmt.Sprintf(
				"频道表没有默认城镇频道 %d：登录时客户端只会自动拨它记住的那个频道号，"+
					"记住的不是表里这些频道的玩家会停在选频道界面（没有手动入口）",
				dnfenum.DefaultGameChannelID))
		}
	}
	if !catalogHasSpecialAreaChannel(catalog) {
		warnings = append(warnings, fmt.Sprintf(
			"频道表没有任何特殊区域频道（type %d）：线上默认是 121 远古 / 171 绝望之塔，"+
				"关闭它们的守卫要求至少留一个开启",
			currentSpecialAreaChannelType))
	}
	return warnings
}

// loginChannel 选出 game class0/op1 首包与"刷新频道信息"回包要广告的那个频道端点。
//
// 顺序：默认城镇频道 11 → 引导频道 19 → 目录里第一个能承载城镇 actor 的频道。
// 最后这档是给"运维只开少数频道（甚至只开特殊区域频道）"用的：原来只认 11/19，
// 两个都不在表里就返回 error —— 而 buildChannelUpdateBody 的错误会让
// handleChannelPacket 失败、进而把 7001 那条频道连接关掉（见 handleChannelConn 的错误分支）。
func (s *Service) loginChannel() (channelcatalog.Channel, error) {
	catalog := s.currentCatalog()
	if catalog == nil {
		return channelcatalog.Channel{}, channelcatalog.ErrEmpty
	}
	if channel, ok := catalog.Channel(dnfenum.DefaultGameChannelID); ok {
		return channel, nil
	}
	if channel, ok := catalog.Channel(dnfenum.BootstrapChannelID); ok {
		return channel, nil
	}
	for _, channel := range catalog.Channels() {
		if channelFitsTownActorOwner(channel.ID) {
			return channel, nil
		}
	}
	return channelcatalog.Channel{}, fmt.Errorf(
		"%w: resident channels %d and %d are missing and no catalog channel fits the town actor owner",
		channelcatalog.ErrEmpty,
		dnfenum.DefaultGameChannelID,
		dnfenum.BootstrapChannelID,
	)
}

func (s *Service) buildChannelInfoBody(request []byte) ([]byte, error) {
	return s.buildChannelInfoBodyFor(request, false)
}

func (s *Service) buildChannelInfoBodyFor(request []byte, bootstrap bool) ([]byte, error) {
	catalog := s.currentCatalog()
	if catalog == nil {
		return nil, channelcatalog.ErrEmpty
	}
	group := readNullASCII(request, 0, 20)
	channels := s.channelsForRequestMode(group, bootstrap)
	firstChannel := 0
	lastChannel := 0
	if len(channels) > 0 {
		firstChannel = channels[0].ID
		lastChannel = channels[len(channels)-1].ID
	}
	s.logPacketEvent("channel-advertise",
		"requested_group", group,
		"source_server", s.channelServerID(),
		"advertise_server", s.channelAdvertiseID(),
		"body_mode", s.channelInfoBodyMode(),
		"bootstrap", bootstrap,
		"count", len(channels),
		"first_channel", firstChannel,
		"last_channel", lastChannel)
	var writer packetWriter
	s.writeChannelInfoPrefix(&writer, len(channels))
	s.writeChannelInfoEntries(&writer, channels)
	return encryptChannelData(writer.bytes(), s.aesKey(), true)
}

func (s *Service) writeChannelInfoPrefix(writer *packetWriter, count int) {
	// IDA/MCP 复核 sub_1CA2440：最新客户端按 server_count/server_id/channel_count 解析 0x12 明文头。
	// 旧 C# 的 2 字节保留位前缀已确认会把客户端解析游标带偏，不能再作为当前桥接输出。
	writer.writeUint32(1)
	writer.writeUint32(uint32(s.channelAdvertiseID()))
	writer.writeUint32(uint32(count))
}

func (s *Service) writeChannelInfoEntries(writer *packetWriter, channels []channelcatalog.Channel) {
	for _, channel := range channels {
		writer.buffer.Write(fixedASCII("#"+channel.Name, 20))
		writer.writeInt32(channel.MaxUsers)
		writer.writeInt32(channel.CurrentUsers)
		writer.buffer.Write(fixedASCII(s.options.serverIP, 16))
		writer.writeInt32(channel.Port)
	}
}

func (s *Service) channelsForRequest(group string) []channelcatalog.Channel {
	return s.channelsForRequestMode(group, false)
}

func (s *Service) channelsForRequestMode(group string, bootstrap bool) []channelcatalog.Channel {
	catalog := s.currentCatalog()
	if catalog == nil {
		return nil
	}
	var channels []channelcatalog.Channel
	if bootstrap {
		channels = catalog.FilterForBootstrap(group)
	} else {
		channels = catalog.FilterForRequest(group)
	}
	channels = s.advertisableChannels(channels, group, bootstrap)
	if len(channels) == 0 && isDefaultChannelGroup(group) {
		if channel, ok := catalog.ForPort(dnfenum.GamePortBase + dnfenum.DefaultGameChannelID); ok {
			return []channelcatalog.Channel{channel}
		}
	}
	return channels
}

// advertisableChannels removes every channel the current client cannot own a
// town actor in. A published channel is selectable, and selecting one above the
// u8 town-actor-owner range produced a session that entered the game with an
// uncommitted owner. Refusing to publish the channel is what makes that state
// unreachable, because the client derives the owner byte from the advertised id
// and the server cannot renumber it on the wire without lying about which
// channel the player is in -- the ids have to be in range in channel_info.etc.
//
// The dropped ids are logged rather than silently trimmed: a shipped
// channel_info.etc that loses entries here must be visible in the log.
func (s *Service) advertisableChannels(
	channels []channelcatalog.Channel,
	group string,
	bootstrap bool,
) []channelcatalog.Channel {
	dropped := make([]int, 0)
	for _, channel := range channels {
		if !channelFitsTownActorOwner(channel.ID) {
			dropped = append(dropped, channel.ID)
		}
	}
	if len(dropped) == 0 {
		return channels
	}
	kept := make([]channelcatalog.Channel, 0, len(channels)-len(dropped))
	for _, channel := range channels {
		if channelFitsTownActorOwner(channel.ID) {
			kept = append(kept, channel)
		}
	}
	s.logPacketEvent("channel-advertise-out-of-range-dropped",
		"requested_group", group,
		"bootstrap", bootstrap,
		"kept", len(kept),
		"dropped", len(dropped),
		"dropped_channels", dropped,
		"limit", math.MaxUint8,
		"reason", "channel_id_cannot_fit_current_exe_u8_town_actor_owner")
	return kept
}

func isDefaultChannelGroup(group string) bool {
	group = strings.TrimSpace(strings.Trim(group, "`"))
	group = strings.TrimPrefix(group, "[")
	group = strings.TrimSuffix(group, "]")
	group = strings.ToLower(strings.TrimSpace(group))
	return group == "" || group == dnfenum.GroupCain
}

func readNullASCII(data []byte, offset int, size int) string {
	if offset >= len(data) || size <= 0 {
		return ""
	}
	limit := offset + size
	if limit > len(data) {
		limit = len(data)
	}
	end := offset
	for end < limit && data[end] != 0 {
		end++
	}
	return strings.TrimSpace(string(data[offset:end]))
}

func osReadFile(path string) ([]byte, error) {
	data, err := os.ReadFile(path) //nolint:gosec // G304：路径来自部署配置，DNF bridge 只读取 channel_info.etc 活配置。
	if err != nil {
		return nil, fmt.Errorf("read channel info script: %w", err)
	}
	return data, nil
}
