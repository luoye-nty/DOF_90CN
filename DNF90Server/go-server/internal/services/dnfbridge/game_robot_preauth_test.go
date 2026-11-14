package dnfbridge

import (
	"bytes"
	"context"
	"encoding/binary"
	"net"
	"testing"
	"time"

	dnfrepo "longheng.io/server/internal/modules/dnf/repository"
	"longheng.io/server/internal/modules/dnf/repository/memory"
)

type robotPreauthTestConn struct {
	reader *bytes.Reader
	writer bytes.Buffer
}

func (c *robotPreauthTestConn) Read(data []byte) (int, error)  { return c.reader.Read(data) }
func (c *robotPreauthTestConn) Write(data []byte) (int, error) { return c.writer.Write(data) }
func (c *robotPreauthTestConn) Close() error                   { return nil }
func (c *robotPreauthTestConn) LocalAddr() net.Addr {
	return &net.TCPAddr{IP: net.ParseIP("127.0.0.1"), Port: 10041}
}
func (c *robotPreauthTestConn) RemoteAddr() net.Addr {
	return &net.TCPAddr{IP: net.ParseIP("127.0.0.1"), Port: 45000}
}
func (c *robotPreauthTestConn) SetDeadline(time.Time) error      { return nil }
func (c *robotPreauthTestConn) SetReadDeadline(time.Time) error  { return nil }
func (c *robotPreauthTestConn) SetWriteDeadline(time.Time) error { return nil }

func TestCurrentRobotPreauthConsumesPrivateBindBeforeLauncherAuth(t *testing.T) {
	repositories := memory.NewMemoryGroup()
	if err := repositories.Character.Save(context.Background(), dnfrepo.CharacterRecord{
		CharacterID: "19", AccountID: "manual-robot-001", Slot: 0, Name: "robot",
	}); err != nil {
		t.Fatal(err)
	}
	secret := []byte("robot-preauth-test-secret")
	service := &Service{
		robotSessionSecret:  secret,
		robotSessionReplays: make(map[[currentRobotSessionBindSignatureLen]byte]time.Time),
		repositoryProvider:  func() (dnfrepo.Group, bool) { return repositories, true },
	}
	body, err := buildCurrentRobotSessionBindRequest(secret, time.Now().UTC(), 77, 19, "manual-robot-001")
	if err != nil {
		t.Fatal(err)
	}
	frame := make([]byte, 13+len(body))
	frame[0] = 1
	binary.LittleEndian.PutUint16(frame[1:3], currentRobotSessionBindOpcode)
	binary.LittleEndian.PutUint32(frame[3:7], uint32(len(frame)))
	copy(frame[13:], body)
	rawConn := &robotPreauthTestConn{reader: bytes.NewReader(frame)}
	session := &gameSession{conn: newBufferedGameConn(rawConn)}
	bound, err := service.tryCurrentRobotPreauth(context.Background(), session)
	if err != nil {
		t.Fatalf("preauth: %v", err)
	}
	if !bound || !session.robotSessionBound || session.accountID != "manual-robot-001" {
		t.Fatalf("preauth bound=%t robot=%t account=%q", bound, session.robotSessionBound, session.accountID)
	}
	if rawConn.writer.Len() == 0 {
		t.Fatal("preauth did not write the robot bind acknowledgement")
	}
}
