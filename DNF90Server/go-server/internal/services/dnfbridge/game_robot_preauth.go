package dnfbridge

import (
	"bufio"
	"context"
	"encoding/binary"
	"fmt"
	"io"
	"net"
	"time"

	dnfproto "longheng.io/server/internal/modules/dnf/protocol"
)

const robotPreauthProbeTimeout = 150 * time.Millisecond

// bufferedGameConn preserves bytes inspected before the launcher-auth gate.
type bufferedGameConn struct {
	net.Conn
	reader *bufio.Reader
}

func newBufferedGameConn(conn net.Conn) *bufferedGameConn {
	return &bufferedGameConn{Conn: conn, reader: bufio.NewReaderSize(conn, 4096)}
}

func (c *bufferedGameConn) Read(data []byte) (int, error) {
	return c.reader.Read(data)
}

func (c *bufferedGameConn) peek(size int) ([]byte, error) {
	return c.reader.Peek(size)
}

// tryCurrentRobotPreauth consumes the private robot bind frame before launcher
// ticket authentication starts. The probe never consumes non-robot bytes:
// early launcher-auth clients can continue through the normal challenge path.
func (s *Service) tryCurrentRobotPreauth(ctx context.Context, session *gameSession) (bool, error) {
	if s == nil || session == nil || len(s.robotSessionSecret) == 0 || !currentRobotSessionRemoteAllowed(session.conn) {
		return false, nil
	}
	conn, ok := session.conn.(*bufferedGameConn)
	if !ok {
		return false, nil
	}
	if deadline, hasDeadline := ctx.Deadline(); !hasDeadline || deadline.After(time.Now().Add(robotPreauthProbeTimeout)) {
		_ = conn.SetReadDeadline(time.Now().Add(robotPreauthProbeTimeout))
	} else {
		_ = conn.SetReadDeadline(deadline)
	}
	header, err := conn.peek(13)
	_ = conn.SetReadDeadline(time.Time{})
	if err != nil {
		if timeout, ok := err.(net.Error); ok && timeout.Timeout() {
			return false, nil
		}
		if err == io.EOF || ctx.Err() != nil {
			return false, err
		}
		return false, nil
	}
	if header[0] != 1 || binary.LittleEndian.Uint16(header[1:3]) != currentRobotSessionBindOpcode {
		return false, nil
	}
	length := int(binary.LittleEndian.Uint32(header[3:7]))
	if length < dnfproto.LegacyGameHeaderSize || length > 1024*1024 {
		return false, fmt.Errorf("robot bind frame length %d is invalid", length)
	}
	frame := make([]byte, length)
	if _, err := io.ReadFull(conn.reader, frame); err != nil {
		return false, fmt.Errorf("read robot bind frame: %w", err)
	}
	if err := s.handleCurrentRobotSessionBind(session, frame[dnfproto.LegacyGameHeaderSize:]); err != nil {
		return false, err
	}
	return session.robotSessionBound, nil
}
