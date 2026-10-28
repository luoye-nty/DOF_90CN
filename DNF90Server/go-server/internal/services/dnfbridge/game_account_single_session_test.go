package dnfbridge

import (
	"encoding/binary"
	"testing"

	"longheng.io/server/internal/modules/dnf/dnfenum"
	dnfproto "longheng.io/server/internal/modules/dnf/protocol"
)

// TestCurrentDuplicateLoginWouldDisplaceBoundaries 钉住"另一个客户端"判定的每个边界。
// 放过是安全的错，误踢会把玩家自己换频道踢下线，所以边界都必须明确。
func TestCurrentDuplicateLoginWouldDisplaceBoundaries(t *testing.T) {
	service := &Service{}

	cases := []struct {
		name     string
		previous *gameSession
		session  *gameSession
		want     bool
	}{
		{
			name:     "本地 同账号 两个进程 = 第二个客户端",
			previous: &gameSession{accountID: "dnf:1", clientPID: 100},
			session:  &gameSession{accountID: "dnf:1", clientPID: 200},
			want:     true,
		},
		{
			name:     "本地 同账号 同一个进程 = 换频道/返回选角",
			previous: &gameSession{accountID: "dnf:1", clientPID: 100},
			session:  &gameSession{accountID: "dnf:1", clientPID: 100},
			want:     false,
		},
		{
			name:     "远程 同账号 令牌摘要不同 = 另一台机器",
			previous: &gameSession{accountID: "dnf:1", loginIdentity: "aaaa"},
			session:  &gameSession{accountID: "dnf:1", loginIdentity: "bbbb"},
			want:     true,
		},
		{
			name:     "远程 同账号 同一个令牌摘要 = 同一次启动的多条连接",
			previous: &gameSession{accountID: "dnf:1", loginIdentity: "aaaa"},
			session:  &gameSession{accountID: "dnf:1", loginIdentity: "aaaa"},
			want:     false,
		},
		{
			name:     "远程 同账号 但身份未知 = 放过(宁可漏踢)",
			previous: &gameSession{accountID: "dnf:1"},
			session:  &gameSession{accountID: "dnf:1"},
			want:     false,
		},
		{
			name:     "不同账号 永不踢",
			previous: &gameSession{accountID: "dnf:1", clientPID: 100},
			session:  &gameSession{accountID: "dnf:2", clientPID: 200},
			want:     false,
		},
		{
			name:     "账号未知 放过",
			previous: &gameSession{clientPID: 100},
			session:  &gameSession{clientPID: 200},
			want:     false,
		},
		{
			name:     "只有一侧有 PID = 放过",
			previous: &gameSession{accountID: "dnf:1", clientPID: 100},
			session:  &gameSession{accountID: "dnf:1"},
			want:     false,
		},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if got := service.currentDuplicateLoginWouldDisplace(tc.previous, tc.session); got != tc.want {
				t.Fatalf("wouldDisplace = %v, want %v", got, tc.want)
			}
		})
	}

	// 同一条会话永远不踢自己。
	same := &gameSession{accountID: "dnf:1", clientPID: 100}
	if service.currentDuplicateLoginWouldDisplace(same, same) {
		t.Fatal("a session must never displace itself")
	}
}

// TestCurrentDuplicateLoginNoticeIsClass1Opcode1Reason18 钉住线上字节。
//
// 客户端只认 class=1 / opcode=1 / body = 00 12：a2=0 走通知分支，a3=0x12 是唯一能到
// reason 7 的取值，而 reason 7 正是 GetString(157)「此ID已在游戏中」。
// 包体必须是明文 —— 客户端只对 class==0 && opcode==1 解码，套上 codec 这个包就废了。
func TestCurrentDuplicateLoginNoticeIsClass1Opcode1Reason18(t *testing.T) {
	service := &Service{}
	conn := &bufferConn{}
	session := &gameSession{conn: conn, connID: "game-000001", accountID: "dnf:1"}

	if err := service.sendCurrentDuplicateLoginNotice(session); err != nil {
		t.Fatalf("send notice: %v", err)
	}
	packet := conn.write.Bytes()
	if len(packet) < 3 {
		t.Fatalf("packet too short: %d bytes", len(packet))
	}
	if got, want := packet[0], byte(dnfproto.DefaultChannelClassification); got != want {
		t.Fatalf("classification = %d, want %d (class-1)", got, want)
	}
	if got, want := binary.LittleEndian.Uint16(packet[1:3]), uint16(dnfenum.CmdPacketLogin); got != want {
		t.Fatalf("opcode = %d, want %d (ENUM_CMDPACKET_LOGIN)", got, want)
	}
	total := int(binary.LittleEndian.Uint32(packet[3:7]))
	if total != len(packet) {
		t.Fatalf("declared length %d != actual %d", total, len(packet))
	}
	headerSize := len(packet) - 2
	if headerSize != dnfproto.ChannelHeaderSize && headerSize != dnfproto.GameServerUpperHeaderSize16 {
		t.Fatalf("unexpected header size %d", headerSize)
	}
	body := packet[headerSize:]
	if body[0] != 0x00 {
		t.Fatalf("a2 = 0x%02X, want 0x00 (notice branch)", body[0])
	}
	if body[1] != 0x12 {
		t.Fatalf("a3 = 0x%02X, want 0x12 (reason 7 -> text 157)", body[1])
	}
}

// TestEnforceCurrentAccountSingleSessionCountsSelectCharacterAsOnline 钉住"进了频道就算在线"。
//
// 停在角色选择界面的会话没有绑定任何角色（selectedCharacterID==0），但它已经进了频道、
// 已经占着这个账号。第二个客户端进频道时必须能把它当成目标顶掉，而不是等双方都进城镇。
func TestEnforceCurrentAccountSingleSessionCountsSelectCharacterAsOnline(t *testing.T) {
	atSelectScreen := &gameSession{
		conn: &bufferConn{}, connID: "game-000001", accountID: "dnf:1",
		selectedCharacterID: 0, clientPID: 100, // 停在选角，没选角色
	}
	entering := &gameSession{
		conn: &bufferConn{}, connID: "game-000002", accountID: "dnf:1",
		selectedCharacterID: 0, clientPID: 200,
	}
	service := &Service{onlineSeats: map[*gameSession]struct{}{atSelectScreen: {}}}

	kicked := service.enforceCurrentAccountSingleSession(entering, "dnf:1")

	if !atSelectScreen.duplicateLoginKicked {
		t.Fatal("a session sitting at character select must count as online")
	}
	if !kicked {
		t.Fatal("the entering session must be told to stop before it enters the channel")
	}
}

// TestEnforceCurrentAccountSingleSessionKicksBothSides 钉住"两边都断"。
// 官方老服务端对旧会话与新会话发的是同一个 0x12，并把新登录中止。
func TestEnforceCurrentAccountSingleSessionKicksBothSides(t *testing.T) {
	old := &gameSession{
		conn: &bufferConn{}, connID: "game-000001", accountID: "dnf:1",
		selectedCharacterID: 1, clientPID: 100,
	}
	cur := &gameSession{
		conn: &bufferConn{}, connID: "game-000002", accountID: "dnf:1",
		selectedCharacterID: 2, clientPID: 200,
	}
	service := &Service{onlineSeats: map[*gameSession]struct{}{old: {}}}

	service.enforceCurrentAccountSingleSession(cur, "dnf:1")

	if !old.duplicateLoginKicked {
		t.Fatal("the older client was not kicked")
	}
	if !cur.duplicateLoginKicked {
		t.Fatal("the new session must also be rejected, like the official server")
	}
}

// TestEnforceCurrentAccountSingleSessionIgnoresSameProcess 是防回归的核心：
// 换频道/返回选角/启动时那上百条连接都是同一个进程，绝不能被判成第二个客户端。
func TestEnforceCurrentAccountSingleSessionIgnoresSameProcess(t *testing.T) {
	old := &gameSession{
		conn: &bufferConn{}, connID: "game-000001", accountID: "dnf:1",
		selectedCharacterID: 1, clientPID: 100,
	}
	cur := &gameSession{
		conn: &bufferConn{}, connID: "game-000002", accountID: "dnf:1",
		selectedCharacterID: 2, clientPID: 100,
	}
	service := &Service{onlineSeats: map[*gameSession]struct{}{old: {}}}

	service.enforceCurrentAccountSingleSession(cur, "dnf:1")

	if old.duplicateLoginKicked || cur.duplicateLoginKicked {
		t.Fatal("a channel switch must never be treated as a duplicate login")
	}
}

// TestEnforceCurrentAccountSingleSessionUndecidableDoesNothing 钉住"判不出来就放过"：
// 身份未知时宁可漏踢，日志里留一行 undecidable 便于现场定位。
func TestEnforceCurrentAccountSingleSessionUndecidableDoesNothing(t *testing.T) {
	old := &gameSession{
		conn: &bufferConn{}, connID: "game-000001", accountID: "dnf:1",
		selectedCharacterID: 1,
	}
	cur := &gameSession{
		conn: &bufferConn{}, connID: "game-000002", accountID: "dnf:1",
		selectedCharacterID: 2,
	}
	service := &Service{onlineSeats: map[*gameSession]struct{}{old: {}}}

	service.enforceCurrentAccountSingleSession(cur, "dnf:1")

	if old.duplicateLoginKicked || cur.duplicateLoginKicked {
		t.Fatal("an unknown identity must never be kicked")
	}
}

// TestEnforceCurrentAccountSingleSessionRemoteIdentity 钉住远程判据：
// 两侧 clientPID 都是 0（公网拿不到对端 PID），靠每次登录唯一的身份串区分。
func TestEnforceCurrentAccountSingleSessionRemoteIdentity(t *testing.T) {
	old := &gameSession{
		conn: &bufferConn{}, connID: "game-000001", accountID: "dnf:1",
		selectedCharacterID: 1, loginIdentity: "token-a",
	}
	cur := &gameSession{
		conn: &bufferConn{}, connID: "game-000002", accountID: "dnf:1",
		selectedCharacterID: 2, loginIdentity: "token-b",
	}
	service := &Service{onlineSeats: map[*gameSession]struct{}{old: {}}}

	service.enforceCurrentAccountSingleSession(cur, "dnf:1")

	if !old.duplicateLoginKicked || !cur.duplicateLoginKicked {
		t.Fatal("a different per-login identity on the same account must be kicked")
	}
}

// TestDisplaceReplacedAccountSessionHandlesSameCharacter 钉住"两边选同一个角色"。
// 这时旧会话已不再是任何角色的持有者，只有这条接管路径能看到它。
func TestDisplaceReplacedAccountSessionHandlesSameCharacter(t *testing.T) {
	previous := &gameSession{
		conn: &bufferConn{}, connID: "game-000001", accountID: "dnf:1",
		selectedCharacterID: 1, clientPID: 100,
	}
	session := &gameSession{
		conn: &bufferConn{}, connID: "game-000002", accountID: "dnf:1",
		selectedCharacterID: 1, clientPID: 200,
	}
	service := &Service{}

	service.displaceReplacedAccountSession(previous, session)

	if !previous.duplicateLoginKicked {
		t.Fatal("the displaced client was not kicked")
	}
	// 同一角色被本客户端换连接接管时（PID 相同）不能踢。
	previous.duplicateLoginKicked = false
	sameProcess := &gameSession{
		conn: &bufferConn{}, connID: "game-000003", accountID: "dnf:1",
		selectedCharacterID: 1, clientPID: 100,
	}
	service.displaceReplacedAccountSession(previous, sameProcess)
	if previous.duplicateLoginKicked {
		t.Fatal("the same process re-taking its own character must not be kicked")
	}
}
