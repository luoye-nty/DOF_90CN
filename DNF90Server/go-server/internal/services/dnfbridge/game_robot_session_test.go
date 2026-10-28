package dnfbridge

import (
	"bytes"
	"context"
	"net"
	"testing"
	"time"

	"longheng.io/server/internal/modules/dnf/channelcatalog"
	"longheng.io/server/internal/modules/dnf/dnfenum"
	dnfrepo "longheng.io/server/internal/modules/dnf/repository"
	"longheng.io/server/internal/modules/dnf/repository/memory"
)

type currentRobotSessionTestConn struct {
	bufferConn
	remote net.Addr
}

func (c *currentRobotSessionTestConn) RemoteAddr() net.Addr { return c.remote }
func (c *currentRobotSessionTestConn) LocalAddr() net.Addr {
	return &net.TCPAddr{IP: net.ParseIP("127.0.0.1"), Port: 10001}
}

func TestCurrentRobotSessionBindAuthenticatesAccountAndRejectsReplay(t *testing.T) {
	repositories := memory.NewMemoryGroup()
	ctx := context.Background()
	for _, character := range []dnfrepo.CharacterRecord{
		{CharacterID: "41", AccountID: "dnf:robot-7", Slot: 3, Name: "three"},
		{CharacterID: "19", AccountID: "dnf:robot-7", Slot: 1, Name: "one"},
	} {
		if err := repositories.Character.Save(ctx, character); err != nil {
			t.Fatalf("seed character: %v", err)
		}
	}
	secret := []byte("test-only-robot-session-secret")
	service := &Service{
		robotSessionSecret:  append([]byte(nil), secret...),
		robotSessionReplays: make(map[[currentRobotSessionBindSignatureLen]byte]time.Time),
		repositoryProvider:  func() (dnfrepo.Group, bool) { return repositories, true },
		gameSessions:        make(map[uint16]*gameSession),
	}
	request, err := buildCurrentRobotSessionBindRequest(secret, time.Now().UTC(), 91, 41, "dnf:robot-7")
	if err != nil {
		t.Fatalf("build request: %v", err)
	}

	conn := &currentRobotSessionTestConn{remote: &net.TCPAddr{IP: net.ParseIP("192.168.10.20"), Port: 45000}}
	session := &gameSession{conn: conn, channel: channelcatalog.Channel{ID: 16}}
	if err := service.handleGameCommand(session, byte(dnfenum.GameCmdCommand), currentRobotSessionBindOpcode, request); err != nil {
		t.Fatalf("bind session: %v", err)
	}
	if session.accountID != "dnf:robot-7" {
		t.Fatalf("bound account=%q", session.accountID)
	}
	records, err := splitWrittenGameRecords(conn.write.Bytes())
	if err != nil {
		t.Fatalf("split bind response: %v", err)
	}
	if len(records) != 1 || records[0].GameHeader.Type != currentRobotSessionBindOpcode || !bytes.Equal(records[0].Body, []byte{1, 3, 41, 0}) {
		t.Fatalf("bind response=%+v", records)
	}

	replayConn := &currentRobotSessionTestConn{remote: &net.TCPAddr{IP: net.ParseIP("10.20.30.40"), Port: 45001}}
	replaySession := &gameSession{conn: replayConn, channel: channelcatalog.Channel{ID: 16}}
	if err := service.handleGameCommand(replaySession, byte(dnfenum.GameCmdCommand), currentRobotSessionBindOpcode, request); err != nil {
		t.Fatalf("replay response: %v", err)
	}
	replayRecords, err := splitWrittenGameRecords(replayConn.write.Bytes())
	if err != nil {
		t.Fatalf("split replay response: %v", err)
	}
	if len(replayRecords) != 1 || !bytes.Equal(replayRecords[0].Body, []byte{0, currentRobotSessionBindErrorReplay}) || replaySession.accountID != "" {
		t.Fatalf("replay response=%+v account=%q", replayRecords, replaySession.accountID)
	}
}

func TestCurrentRobotSessionBindRejectsPublicRemoteAndWrongCharacter(t *testing.T) {
	repositories := memory.NewMemoryGroup()
	if err := repositories.Character.Save(context.Background(), dnfrepo.CharacterRecord{
		CharacterID: "19", AccountID: "dnf:robot-7", Slot: 1,
	}); err != nil {
		t.Fatalf("seed character: %v", err)
	}
	secret := []byte("test-only-robot-session-secret")
	service := &Service{
		robotSessionSecret:  secret,
		robotSessionReplays: make(map[[currentRobotSessionBindSignatureLen]byte]time.Time),
		repositoryProvider:  func() (dnfrepo.Group, bool) { return repositories, true },
	}

	for _, test := range []struct {
		name      string
		remote    net.Addr
		desired   uint16
		wantError byte
	}{
		{name: "public remote", remote: &net.TCPAddr{IP: net.ParseIP("8.8.8.8"), Port: 1}, desired: 19, wantError: currentRobotSessionBindErrorAuth},
		{name: "wrong character", remote: &net.TCPAddr{IP: net.ParseIP("127.0.0.1"), Port: 2}, desired: 77, wantError: currentRobotSessionBindErrorAccount},
	} {
		t.Run(test.name, func(t *testing.T) {
			request, err := buildCurrentRobotSessionBindRequest(secret, time.Now().UTC(), uint64(test.desired)+100, test.desired, "dnf:robot-7")
			if err != nil {
				t.Fatalf("build request: %v", err)
			}
			conn := &currentRobotSessionTestConn{remote: test.remote}
			session := &gameSession{conn: conn, channel: channelcatalog.Channel{ID: 16}}
			if err := service.handleCurrentRobotSessionBind(session, request); err != nil {
				t.Fatalf("bind: %v", err)
			}
			records, err := splitWrittenGameRecords(conn.write.Bytes())
			if err != nil {
				t.Fatalf("split: %v", err)
			}
			if len(records) != 1 || !bytes.Equal(records[0].Body, []byte{0, test.wantError}) || session.accountID != "" {
				t.Fatalf("response=%+v account=%q", records, session.accountID)
			}
		})
	}
}
