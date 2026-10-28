package dnfrobot

import (
	"bytes"
	"context"
	"encoding/binary"
	"io"
	"net"
	"testing"
	"time"

	dnfrobotdomain "longheng.io/server/internal/modules/dnf/robot"
)

func TestClientRunsCurrentTownHandshakeAndActions(t *testing.T) {
	listener, err := net.Listen("tcp4", "127.0.0.1:0")
	if err != nil {
		t.Fatalf("Listen: %v", err)
	}
	defer listener.Close()
	serverErr := make(chan error, 1)
	ready := make(chan struct{})
	go func() {
		conn, acceptErr := listener.Accept()
		if acceptErr != nil {
			serverErr <- acceptErr
			return
		}
		defer conn.Close()
		steps := []struct {
			want uint16
			send uint16
			body []byte
		}{
			{robotBindOpcode, robotBindOpcode, []byte{1, 3, 7, 0}},
			// The production server answers GET_USERINFO with a class-0/op2
			// fixed-16 upper roster, not the older op8 test response.
			{8, 2, make([]byte, 153)},
			{4, 4, nil},
			{143, 24, nil},
			{1345, 0, nil},
			{36, 0, nil},
			{35, 0, nil},
			{17, 0, nil},
			{17, 0, nil},
			{36, 0, nil},
			{35, 0, nil},
		}
		for index, step := range steps {
			opcode, body, readErr := readClientPacket(conn)
			if readErr != nil {
				serverErr <- readErr
				return
			}
			if opcode != step.want {
				serverErr <- &unexpectedOpcodeError{got: opcode, want: step.want}
				return
			}
			if index == 7 {
				if len(body) < 12 || body[0] != 3 || binary.LittleEndian.Uint32(body[7:11]) == 0 {
					serverErr <- io.ErrUnexpectedEOF
					return
				}
			}
			if index == 8 {
				if len(body) < 12 || body[0] != 11 || binary.LittleEndian.Uint32(body[3:7]) != 23 || binary.LittleEndian.Uint32(body[7:11]) == 0 {
					serverErr <- io.ErrUnexpectedEOF
					return
				}
			}
			if step.send != 0 {
				if _, writeErr := conn.Write(buildServerPacket(step.send, step.body)); writeErr != nil {
					serverErr <- writeErr
					return
				}
			}
			if index == 6 {
				close(ready)
			}
		}
		serverErr <- nil
		_, _ = io.Copy(io.Discard, conn)
	}()

	profile := dnfrobotdomain.Profile{
		ID: "fixture", AccountID: "account", CharacterID: 7, ChannelID: 19,
		Village: 1, Area: 2, X: 100, Y: 200,
	}
	client := NewClient(profile, listener.Addr().String(), []byte("secret"))
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	runErr := make(chan error, 1)
	go func() { runErr <- client.Run(ctx) }()
	select {
	case <-ready:
	case <-time.After(3 * time.Second):
		t.Fatal("client did not complete current town handshake")
	}
	deadline := time.Now().Add(time.Second)
	for client.Snapshot().State != StateTownReady && time.Now().Before(deadline) {
		time.Sleep(time.Millisecond)
	}
	if snapshot := client.Snapshot(); snapshot.State != StateTownReady || snapshot.CharacterID != 7 || snapshot.Slot != 3 {
		t.Fatalf("unexpected ready snapshot: %+v", snapshot)
	}
	if err := client.Chat(3, "你好"); err != nil {
		t.Fatalf("Chat: %v", err)
	}
	if err := client.Megaphone(23, "你好"); err != nil {
		t.Fatalf("Megaphone: %v", err)
	}
	if err := client.Move(2, 4, 300, 400, 1); err != nil {
		t.Fatalf("Move: %v", err)
	}
	if err := <-serverErr; err != nil {
		t.Fatal(err)
	}
	cancel()
	select {
	case <-runErr:
	case <-time.After(time.Second):
		t.Fatal("client did not stop after context cancellation")
	}
}

type unexpectedOpcodeError struct{ got, want uint16 }

func (e *unexpectedOpcodeError) Error() string { return "unexpected opcode" }

func readClientPacket(conn net.Conn) (uint16, []byte, error) {
	header := make([]byte, 13)
	if _, err := io.ReadFull(conn, header); err != nil {
		return 0, nil, err
	}
	size := int(binary.LittleEndian.Uint32(header[3:7]))
	if size < 13 || size > maximumFrameSize {
		return 0, nil, io.ErrUnexpectedEOF
	}
	body := make([]byte, size-13)
	if _, err := io.ReadFull(conn, body); err != nil {
		return 0, nil, err
	}
	return binary.LittleEndian.Uint16(header[1:3]), body, nil
}

func buildServerPacket(opcode uint16, body []byte) []byte {
	headerSize := 13
	if opcode == 2 {
		headerSize = 16
	}
	frame := make([]byte, headerSize+len(body))
	frame[0] = 0
	binary.LittleEndian.PutUint16(frame[1:3], opcode)
	binary.LittleEndian.PutUint32(frame[3:7], uint32(len(frame)))
	copy(frame[headerSize:], body)
	return frame
}

func TestDecodeFrameAcceptsCurrentOuterBusinessPacket(t *testing.T) {
	business := make([]byte, 7+3)
	business[0] = 1
	binary.LittleEndian.PutUint16(business[1:3], 17)
	copy(business[7:], []byte{1, 2, 3})
	frame := make([]byte, outerHeaderSize+innerHeaderSize+len(business))
	binary.LittleEndian.PutUint16(frame[2:4], uint16(len(frame)))
	frame[12] = 2
	binary.LittleEndian.PutUint16(frame[17:19], uint16(len(business)))
	copy(frame[outerHeaderSize+innerHeaderSize:], business)
	size, outer, ready, valid := nextFrame(frame)
	if !valid || !ready || !outer || size != len(frame) {
		t.Fatalf("nextFrame = size %d outer %v ready %v valid %v", size, outer, ready, valid)
	}
	packets, err := decodeFrame(frame, true)
	if err != nil || len(packets) != 1 || packets[0].Opcode != 17 || string(packets[0].Body) != "\x01\x02\x03" {
		t.Fatalf("decodeFrame = %+v, %v", packets, err)
	}
}

func TestClientOpensAndClosesCurrentPrivateStore(t *testing.T) {
	listener, err := net.Listen("tcp4", "127.0.0.1:0")
	if err != nil {
		t.Fatalf("Listen: %v", err)
	}
	defer listener.Close()
	serverErr := make(chan error, 1)
	ready := make(chan struct{})
	storeReady := make(chan struct{})
	go func() {
		conn, acceptErr := listener.Accept()
		if acceptErr != nil {
			serverErr <- acceptErr
			return
		}
		defer conn.Close()
		steps := []struct {
			want uint16
			send uint16
			body []byte
		}{
			{robotBindOpcode, robotBindOpcode, []byte{1, 3, 7, 0}},
			{8, 8, nil}, {4, 4, nil}, {143, 24, nil},
			{1345, 0, nil}, {36, 0, nil}, {35, 0, nil},
			{privateStoreCreateOpcode, privateStoreCreateOpcode, []byte{1}},
			{privateStoreCompleteOpcode, privateStoreCompleteOpcode, []byte{1, 0}},
			{privateStoreCloseOpcode, privateStoreCloseOpcode, []byte{1}},
		}
		for index, step := range steps {
			opcode, body, readErr := readClientPacket(conn)
			if readErr != nil {
				serverErr <- readErr
				return
			}
			if opcode != step.want {
				serverErr <- &unexpectedOpcodeError{got: opcode, want: step.want}
				return
			}
			if index == 7 {
				if len(body) != 8 || body[0] != 1 || body[1] != 2 || binary.LittleEndian.Uint16(body[6:8]) != 0xffff {
					serverErr <- io.ErrUnexpectedEOF
					return
				}
			}
			if index == 8 {
				titleLength := int(binary.LittleEndian.Uint32(body[:4]))
				if titleLength != 4 || string(body[4:8]) != "shop" || body[8] != 2 ||
					binary.LittleEndian.Uint32(body[11:15]) != 1000 || body[15] != 0 ||
					binary.LittleEndian.Uint16(body[16:18]) != 10 || binary.LittleEndian.Uint32(body[18:22]) != 5 {
					serverErr <- io.ErrUnexpectedEOF
					return
				}
			}
			if step.send != 0 {
				if _, writeErr := conn.Write(buildServerPacket(step.send, step.body)); writeErr != nil {
					serverErr <- writeErr
					return
				}
			}
			if index == 6 {
				close(ready)
			}
			if index == 8 {
				close(storeReady)
			}
		}
		serverErr <- nil
		_, _ = io.Copy(io.Discard, conn)
	}()
	profile := dnfrobotdomain.Profile{ID: "store", AccountID: "account", CharacterID: 7, ChannelID: 19, Village: 1, Area: 2, X: 100, Y: 200}
	client := NewClient(profile, listener.Addr().String(), []byte("secret"))
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	runErr := make(chan error, 1)
	go func() { runErr <- client.Run(ctx) }()
	select {
	case <-ready:
	case <-time.After(3 * time.Second):
		t.Fatal("client did not become ready")
	}
	deadline := time.Now().Add(time.Second)
	for client.Snapshot().State != StateTownReady && time.Now().Before(deadline) {
		time.Sleep(time.Millisecond)
	}
	if err := client.OpenPrivateStore("shop", []PrivateStoreRow{
		{DisplayIndex: 0, Price: 1000, ListType: 0, Slot: 10, WireValue: 5},
		{DisplayIndex: 1, Price: 2000, ListType: 1, Slot: 11, WireValue: 1},
	}); err != nil {
		t.Fatalf("OpenPrivateStore: %v", err)
	}
	select {
	case <-storeReady:
	case <-time.After(time.Second):
		t.Fatal("client did not send private store contents")
	}
	deadline = time.Now().Add(time.Second)
	for client.Snapshot().State != StateStoreReady && time.Now().Before(deadline) {
		time.Sleep(time.Millisecond)
	}
	if state := client.Snapshot().State; state != StateStoreReady {
		t.Fatalf("state after open = %s", state)
	}
	if err := client.ClosePrivateStore(); err != nil {
		t.Fatalf("ClosePrivateStore: %v", err)
	}
	if err := <-serverErr; err != nil {
		t.Fatal(err)
	}
	deadline = time.Now().Add(time.Second)
	for client.Snapshot().State != StateTownReady && time.Now().Before(deadline) {
		time.Sleep(time.Millisecond)
	}
	if state := client.Snapshot().State; state != StateTownReady {
		t.Fatalf("state after close = %s", state)
	}
	cancel()
	select {
	case <-runErr:
	case <-time.After(time.Second):
		t.Fatal("client did not stop")
	}
}

func TestClientRetriesPrivateStoreDisplayAfterLostCompletionResponse(t *testing.T) {
	serverConn, clientConn := net.Pipe()
	defer serverConn.Close()
	defer clientConn.Close()

	client := NewClient(dnfrobotdomain.Profile{ID: "store-retry", CharacterID: 7}, "pipe", []byte("secret"))
	now := time.Now().UTC()
	client.mu.Lock()
	client.conn = clientConn
	client.state = StateStoreOpening
	client.pendingStore = &pendingPrivateStore{
		title: []byte("shop"),
		rows:  []PrivateStoreRow{{DisplayIndex: 0, Price: 1000, ListType: 0, Slot: 10, WireValue: 5}},
	}
	client.storeCreated = true
	client.storeAttempts = 1
	client.storeRetryAt = now.Add(-time.Second)
	client.mu.Unlock()

	type receivedPacket struct {
		opcode uint16
		body   []byte
		err    error
	}
	received := make(chan receivedPacket, 1)
	go func() {
		opcode, body, err := readClientPacket(serverConn)
		received <- receivedPacket{opcode: opcode, body: body, err: err}
	}()
	attempted, err := client.retryPrivateStoreDisplay(now, false)
	if err != nil || !attempted {
		t.Fatalf("retry attempted=%t err=%v", attempted, err)
	}
	packet := <-received
	if packet.err != nil || packet.opcode != privateStoreCompleteOpcode ||
		!bytes.Equal(packet.body, buildPrivateStoreCompleteBody(client.pendingStore)) {
		t.Fatalf("retry packet opcode=%d body=%x err=%v", packet.opcode, packet.body, packet.err)
	}
	client.mu.RLock()
	attempts, retryAt := client.storeAttempts, client.storeRetryAt
	client.mu.RUnlock()
	if attempts != 2 || !retryAt.Equal(now.Add(privateStoreDisplayRetryGap)) {
		t.Fatalf("retry state attempts=%d retry_at=%v", attempts, retryAt)
	}
	if attempted, err := client.retryPrivateStoreDisplay(now.Add(time.Second), false); err != nil || attempted {
		t.Fatalf("early retry attempted=%t err=%v", attempted, err)
	}
	if err := client.handle(packetForTest(privateStoreCompleteOpcode, []byte{1, 0})); err != nil {
		t.Fatal(err)
	}
	client.mu.RLock()
	state, pending, created, attempts := client.state, client.pendingStore, client.storeCreated, client.storeAttempts
	client.mu.RUnlock()
	if state != StateStoreReady || pending != nil || created || attempts != 0 {
		t.Fatalf("completion state=%s pending=%+v created=%t attempts=%d", state, pending, created, attempts)
	}
}

func TestClientStopsPrivateStoreDisplayAfterFourAttempts(t *testing.T) {
	client := NewClient(dnfrobotdomain.Profile{ID: "store-retry-limit", CharacterID: 7}, "unused", []byte("secret"))
	now := time.Now().UTC()
	client.mu.Lock()
	client.state = StateStoreOpening
	client.pendingStore = &pendingPrivateStore{title: []byte("shop"), rows: []PrivateStoreRow{{DisplayIndex: 0, Price: 1, Slot: 1, WireValue: 1}}}
	client.storeCreated = true
	client.storeAttempts = privateStoreDisplayMaxAttempts
	client.storeRetryAt = now.Add(-time.Second)
	client.mu.Unlock()
	if attempted, err := client.retryPrivateStoreDisplay(now, false); err == nil || attempted {
		t.Fatalf("retry limit attempted=%t err=%v", attempted, err)
	}
}

func packetForTest(opcode uint16, body []byte) packet {
	return packet{Opcode: opcode, Body: body}
}
