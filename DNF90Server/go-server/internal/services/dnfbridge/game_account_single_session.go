package dnfbridge

import (
	"crypto/sha256"
	"encoding/hex"
	"strings"
	"time"

	"longheng.io/server/internal/modules/dnf/dnfenum"
	dnfproto "longheng.io/server/internal/modules/dnf/protocol"
)

// currentLoginIdentityDigest 把身份串(登录票据 / 机器码)压成可安全落日志的摘要。
//
// **绝不记录原文** —— 票据是凭证，机器码会被封禁流程当判据。这里只保留 16 字节，
// 足够现场区分"两条连接是不是同一个客户端/同一台机器"，又不可逆推原文。
//
// 之所以需要它：外网部署上 clientPID 恒为 0、IP 可能相同、机器码也可能相同，
// 唯一可能按"客户端实例"区分的就是每次启动的那张登录票据。先用摘要确认它是不是
// 一次启动一张(该客户端的上百条连接共用)，再决定能不能拿来当顶号判据。
func currentLoginIdentityDigest(value string) string {
	value = strings.TrimSpace(value)
	if value == "" {
		return ""
	}
	sum := sha256.Sum256([]byte(value))
	return hex.EncodeToString(sum[:16])
}

// currentDuplicateLoginNoticeReason 是 class-1 / opcode 1 上的挤下线通知码。
//
// 它是客户端 `CNGameSocket::ShowDisconnectPopup` 的 socket 关闭原因选择子：
//
//	class-1/op1 body = 00 12
//	  → CNGameCore::NetworkProc 0x2261E30 预读 a2=0、a3=0x12
//	  → handler 0x01CFF660 → a3 分派表 → 0x01D0050A `push 7`
//	  → set_CloseReason(7)  [socket+0x2BD064] = 7
//	  → reason 7 → GetString(157) = 「此ID已在游戏中」 → 0x2FDD1A0(0x298, 文案, 2) 弹窗
//
// a3=18 (0x12) 是唯一能到 reason 7 的取值（0xEA 个 a3 全量枚举确认）。
//
// 这个数字不是我们发明的：官方老服务端 `df_game_r` 的
// `Inter_LoginInfo::dispatch_sig`（0x084C5BAA）在顶号时对**新旧两侧**都调用
// `SendCmdErrorPacket(ENUM_CMDPACKET_LOGIN, 0x12)`，再把旧会话 `DisConnSig` 掉、
// 把新登录中止。十多年前的 0x12 与今天的 a3=18 是同一个数。
const currentDuplicateLoginNoticeReason = 0x12

// currentDuplicateLoginCloseGrace 是"发完通知再关套接字"之间的等待。
//
// 关得太早，那 2 个字节还没冲出去，客户端只会看到一条普通的「网络连接中断」(150)，
// 而不是 157；关得太晚，被顶的玩家还在操作。1 秒对局域网与公网都够。
const currentDuplicateLoginCloseGrace = time.Second

// enforceCurrentAccountSingleSession 让一个账号同时只有一个客户端在玩。
//
// **挂点是角色绑定之后，不是连接建立/登录鉴权之后。** 后者看着更早更合适，实际不能用：
//
//   - 客户端一启动就同时连上**所有**频道（实测一个客户端 250ms 内连了 6/12/13/14/15/
//     19/26/27/43/45/55/56/58/59/60/96/98/99/112/113/114/116/141/154/182… 上百条），
//     每条连接都会绑定同一个账号，按账号踢会让客户端自己踢自己；
//   - 同一条连接还会反复认证（实测单条最多 19 次），切频道和回角色选择都会再认一次。
//
// 挂在绑定之后就没有这些问题：s.gameSessions 是**按角色**索引的，只装"已经选好角色、
// 正在玩"的那一条会话。换频道时新会话接管的是同一个角色，bindGameSessionCharacter
// 已经把注册持有者换成了新会话，旧会话不再是任何角色的持有者，查不到也就不会被误踢。
// 能被查到的只剩"另一个客户端正拿着这个账号的另一个角色在玩"，那才是要踢的。
//
// **只对账号确实已知的会话生效。** accountID 为空表示这条连接既没通过带内鉴权、也没有
// PID 注册，服务端只能靠 instance.json 的实例账号兜底 —— 而那个兜底值本身就是一个真实
// 账号，拿它当判据会让一个未鉴权的客户端把真正的持有者踢下线。账号不确定时什么都不做。
// 返回值表示**本会话**是否已被判定为顶号并被安排下线：调用方据此提前结束本次处理，
// 不要再把进城数据发给一个马上就要收 157 的会话。
func (s *Service) enforceCurrentAccountSingleSession(session *gameSession, accountID string) bool {
	accountID = strings.TrimSpace(accountID)
	if s == nil || session == nil || accountID == "" {
		return false
	}
	kicked := false
	for _, old := range s.currentAccountInGameSessions(accountID, session) {
		if !s.currentDuplicateLoginWouldDisplace(old, session) {
			continue
		}
		kicked = true
		s.displaceCurrentDuplicateLogin(old, session, accountID, "another_client_plays_the_same_account")
	}
	if !kicked {
		return false
	}
	// 官方对两侧发的是同一个包：新会话同样收到 0x12，于是它的登录也到此为止。
	s.closeCurrentDuplicateLoginSession(session, accountID, "new_session_rejected_by_duplicate_login")
	s.mu.Lock()
	thisSessionKicked := session.duplicateLoginKicked
	s.mu.Unlock()
	return thisSessionKicked
}

// displaceReplacedAccountSession 处理"新会话接管了旧会话正拿着的那个角色"。
//
// 这一步是必须的：同账号的两个客户端看到的是同一批角色，玩家在两边选同一个角色时，
// bindGameSessionCharacter 会把注册持有者换成新会话，旧会话于是不再是任何角色的持有
// 者，enforceCurrentAccountSingleSession 查不到它，就这样漏掉。
func (s *Service) displaceReplacedAccountSession(previous, session *gameSession) {
	if !s.currentDuplicateLoginWouldDisplace(previous, session) {
		return
	}
	accountID := strings.TrimSpace(previous.accountID)
	s.displaceCurrentDuplicateLogin(previous, session, accountID, "another_client_took_over_the_same_character")
	s.closeCurrentDuplicateLoginSession(session, accountID, "new_session_replaced_the_same_character")
}

// displaceCurrentDuplicateLogin 是两条路径共用的动作：记录 + 把旧会话请出去。
func (s *Service) displaceCurrentDuplicateLogin(
	old, session *gameSession,
	accountID, reason string,
) {
	s.logPacketEvent("game-account-duplicate-login-displaced",
		"conn_id", old.connID,
		"account_id", accountID,
		"displaced_char_id", old.selectedCharacterID,
		"displaced_client_pid", old.clientPID,
		"displaced_login_identity", currentDuplicateLoginIdentity(old),
		"new_conn_id", session.connID,
		"new_char_id", session.selectedCharacterID,
		"new_client_pid", session.clientPID,
		"new_login_identity", currentDuplicateLoginIdentity(session),
		"reason", reason)
	s.closeCurrentDuplicateLoginSession(old, accountID, reason)
}

// currentAccountInGameSessions 返回该账号下**已经占着这个账号**的会话，按需排除一个。
//
// 遍历的是 onlineSeats —— 它装的是"已绑账号、连接还活着"的会话，连接一关就释放。
// 刻意**不**用 s.gameSessions（按角色索引、只装已选好角色正在玩的会话）：那样停在
// 角色选择界面的同账号客户端不算占用者，第二个客户端进频道时找不到目标，要等它自己
// 也进到城镇才被处理。而"能进角色选择界面"本身就说明它已经进了频道、已经占着这个账号。
//
// 同一客户端的上百条连接会同时落在这里，但判据是身份（currentDuplicateLoginWouldDisplace），
// 同身份互相放过，所以只有"另一个客户端"会被处理。
func (s *Service) currentAccountInGameSessions(accountID string, exclude *gameSession) []*gameSession {
	accountID = strings.TrimSpace(accountID)
	if s == nil || accountID == "" {
		return nil
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	result := make([]*gameSession, 0, 1)
	for candidate := range s.onlineSeats {
		if candidate == nil || candidate == exclude || candidate.duplicateLoginKicked {
			continue
		}
		if !strings.EqualFold(strings.TrimSpace(candidate.accountID), accountID) {
			continue
		}
		result = append(result, candidate)
	}
	return result
}

// currentDuplicateLoginWouldDisplace 是"这是不是另一个客户端"的判定。
//
// 判据按部署取，两种都拿不到就**放过**——宁可漏踢，也不能把玩家自己换频道踢下线：
//
//   - 本地一键端：clientPID 是套接字属主进程（tcp_owner_windows.go 反查 TCP 表拿到对端
//     行的 PID）。两边都已知且不同 = 第二个客户端；相同 = 同一个客户端换了条连接
//     （换频道、返回选角、攻坚探测），放过。
//
//   - 远程联网：服务端拿不到对端 Windows PID，两侧 clientPID 恒为 0。此时用 loginIdentity
//     ——每次登录唯一的身份串（带内 class-1/op1 凭证的令牌摘要）。同一次启动的所有连接
//     带的是同一个身份，相同则放过；另一台机器是另一次登录、身份不同即要踢。
//
// 刻意**不用** remoteClientPID：它来自按源 IP 索引的注册表，同一个 IP 后面的两个客户端
// 会读到同一条记录（这正是串号的成因），拿它比对既漏踢又可能把同一客户端的两条连接判成
// 两个客户端而误踢。也刻意**不用** accountSourceIP：实测同一客户端可能多源 IP。
func (s *Service) currentDuplicateLoginWouldDisplace(previous, session *gameSession) bool {
	if s == nil || previous == nil || session == nil || previous == session {
		return false
	}
	accountID := strings.TrimSpace(previous.accountID)
	if accountID == "" || !strings.EqualFold(accountID, strings.TrimSpace(session.accountID)) {
		return false
	}
	if previous.clientPID != 0 && session.clientPID != 0 {
		return previous.clientPID != session.clientPID
	}
	before := currentDuplicateLoginIdentity(previous)
	after := currentDuplicateLoginIdentity(session)
	if before != "" && after != "" {
		return !strings.EqualFold(before, after)
	}
	// 两种判据都拿不到。放过，但留一行日志 —— 否则表现就是"功能没做成"却查不出原因。
	s.logPacketEvent("game-account-duplicate-login-undecidable",
		"conn_id", previous.connID,
		"account_id", accountID,
		"other_conn_id", session.connID,
		"previous_client_pid", previous.clientPID,
		"session_client_pid", session.clientPID,
		"reason", "no_process_and_no_login_identity")
	return false
}

func currentDuplicateLoginIdentity(session *gameSession) string {
	if session == nil {
		return ""
	}
	return strings.TrimSpace(session.loginIdentity)
}

// sendCurrentDuplicateLoginNotice 把挤下线通知发给一条会话。
//
// 用 sendGameUpperRawClassNoCodec 而不是 sendGameUpperRawClass：客户端只对
// `class==0 && opcode==1` 的包体做解码（CNGameCore::NetworkProc 0x02261FC0/0x02261FCC），
// class-1 的包体是原样使用的。带 codec 的版本在 gameUpperBodyCodec 不是 plaintext 时会对
// msgID==1 套 CAST5，把这个 2 字节包静默加密成客户端读不懂的东西 —— 表现就只剩掉线，
// 没有 157 弹窗。
func (s *Service) sendCurrentDuplicateLoginNotice(session *gameSession) error {
	body := []byte{0x00, currentDuplicateLoginNoticeReason}
	return s.sendGameUpperRawClassNoCodec(
		session,
		uint16(dnfenum.CmdPacketLogin),
		body,
		dnfproto.DefaultChannelClassification,
	)
}

// closeCurrentDuplicateLoginSession 先把"请下线"发出去，再关掉这条连接。
//
// 延迟是必须的：立刻关会让那 2 个字节随 TCP 缓冲一起丢掉，客户端只会走普通的
// "网络连接中断"，看不到 157。延迟期间连接仍然可用，不影响任何已完成的接管。
// 关闭走的是既有的连接关闭路径（读循环报错 → releaseOnlineSeat → unbindGameSession），
// 所以城镇同屏、队伍、私人商店、攻坚队断线保位都按正常掉线处理，不需要另写一套释放顺序。
func (s *Service) closeCurrentDuplicateLoginSession(session *gameSession, accountID, reason string) {
	if s == nil || session == nil || session.conn == nil {
		return
	}
	s.mu.Lock()
	if session.duplicateLoginKicked {
		s.mu.Unlock()
		return
	}
	session.duplicateLoginKicked = true
	s.mu.Unlock()

	if err := s.sendCurrentDuplicateLoginNotice(session); err != nil {
		s.logPacketEvent("game-account-duplicate-login-notice-failed",
			"conn_id", session.connID,
			"account_id", accountID,
			"reason", reason,
			"error", err)
	}
	conn := session.conn
	connID := session.connID
	time.AfterFunc(currentDuplicateLoginCloseGrace, func() {
		s.logPacketEvent("game-account-duplicate-login-socket-closed",
			"conn_id", connID,
			"account_id", accountID,
			"grace", currentDuplicateLoginCloseGrace.String(),
			"reason", reason)
		_ = conn.Close()
	})
}
