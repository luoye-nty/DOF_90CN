package dnfrobot

import (
	"context"
	"encoding/binary"
	"errors"
	"io"
	"log/slog"
	"net"
	"strconv"
	"testing"
	"time"

	dnfrepo "longheng.io/server/internal/modules/dnf/repository"
	dnfmemory "longheng.io/server/internal/modules/dnf/repository/memory"
	dnfrobotdomain "longheng.io/server/internal/modules/dnf/robot"
)

type staticRobotEndpoint string

func (endpoint staticRobotEndpoint) RobotGameEndpoint(channelID int) (string, error) {
	if channelID != 19 {
		return "", errors.New("unexpected channel")
	}
	return string(endpoint), nil
}

type robotStoreEndpoint struct {
	staticRobotEndpoint
	rows []PrivateStoreRow
}

func (endpoint robotStoreEndpoint) RobotPrivateStoreRows(_ context.Context, characterID uint16, maximumRows int, unitPrice uint32) ([]PrivateStoreRow, error) {
	if characterID != 7 || maximumRows != 2 || unitPrice != 1234 {
		return nil, errors.New("unexpected private store plan")
	}
	rows := append([]PrivateStoreRow(nil), endpoint.rows...)
	for index := range rows {
		rows[index].Price = unitPrice
	}
	return rows, nil
}

func TestManagerAutostartsPersistedProfileAndDrivesReadyClient(t *testing.T) {
	runtime := dnfmemory.NewRuntime()
	group := runtime.Group()
	ctx := context.Background()
	character := dnfrepo.CharacterRecord{
		CharacterID: strconv.Itoa(7), AccountID: "robot-account", Slot: 3,
		Name: "robot-seven", Level: 90,
	}
	if err := group.Character.Save(ctx, character); err != nil {
		t.Fatalf("Save character: %v", err)
	}
	owner := dnfrobotdomain.NewOwner(func() (dnfrepo.Group, bool) { return group, true })
	profile := dnfrobotdomain.Profile{
		ID: "robot-7", AccountID: character.AccountID, CharacterID: 7, ChannelID: 19,
		AutoStart: true, Village: 1, Area: 2, X: 100, Y: 200,
	}
	if _, err := owner.Save(ctx, dnfrobotdomain.Config{Profiles: []dnfrobotdomain.Profile{profile}}); err != nil {
		t.Fatalf("Save config: %v", err)
	}

	listener, err := net.Listen("tcp4", "127.0.0.1:0")
	if err != nil {
		t.Fatalf("Listen: %v", err)
	}
	defer listener.Close()
	serverErr := make(chan error, 1)
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
			{17, 0, nil}, {35, 0, nil},
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
			if index == 7 && (len(body) < 12 || body[0] != 3 || binary.LittleEndian.Uint32(body[7:11]) == 0) {
				serverErr <- io.ErrUnexpectedEOF
				return
			}
			if index == 8 && (len(body) != 7 || binary.LittleEndian.Uint16(body[0:2]) != 333 || binary.LittleEndian.Uint16(body[2:4]) != 444) {
				serverErr <- io.ErrUnexpectedEOF
				return
			}
			if step.send != 0 {
				if _, writeErr := conn.Write(buildServerPacket(step.send, step.body)); writeErr != nil {
					serverErr <- writeErr
					return
				}
			}
		}
		serverErr <- nil
		_, _ = io.Copy(io.Discard, conn)
	}()

	manager := NewManager(
		slog.New(slog.NewTextHandler(io.Discard, nil)),
		func() (dnfrepo.Group, bool) { return group, true },
		staticRobotEndpoint(listener.Addr().String()), "fixture-secret",
	)
	managerCtx, cancel := context.WithCancel(context.Background())
	defer cancel()
	if err := manager.Start(managerCtx); err != nil {
		t.Fatalf("Start: %v", err)
	}
	deadline := time.Now().Add(3 * time.Second)
	for {
		snapshots := manager.List()
		if len(snapshots) == 1 && snapshots[0].State == StateTownReady {
			break
		}
		if time.Now().After(deadline) {
			t.Fatalf("robot did not become ready: %+v", snapshots)
		}
		time.Sleep(time.Millisecond)
	}
	if err := manager.Chat(profile.ID, 3, "hello"); err != nil {
		t.Fatalf("Chat: %v", err)
	}
	if err := manager.Move(profile.ID, 1, 2, 333, 444, 1); err != nil {
		t.Fatalf("Move: %v", err)
	}
	if err := <-serverErr; err != nil {
		t.Fatal(err)
	}
	stopCtx, stopCancel := context.WithTimeout(context.Background(), time.Second)
	defer stopCancel()
	if err := manager.Stop(stopCtx); err != nil {
		t.Fatalf("Stop: %v", err)
	}
	loaded, err := owner.Load(ctx)
	if err != nil || len(loaded.Profiles) != 1 || loaded.Profiles[0].ID != profile.ID {
		t.Fatalf("persisted config = %+v, %v", loaded, err)
	}
}

func TestManagerRejectsProfileWhoseCharacterBelongsToAnotherAccount(t *testing.T) {
	runtime := dnfmemory.NewRuntime()
	group := runtime.Group()
	if err := group.Character.Save(context.Background(), dnfrepo.CharacterRecord{
		CharacterID: "9", AccountID: "actual-account", Slot: 1, Name: "nine",
	}); err != nil {
		t.Fatalf("Save character: %v", err)
	}
	manager := NewManager(
		slog.New(slog.NewTextHandler(io.Discard, nil)),
		func() (dnfrepo.Group, bool) { return group, true },
		staticRobotEndpoint("127.0.0.1:1"), "fixture-secret",
	)
	if err := manager.Start(context.Background()); err != nil {
		t.Fatalf("Start: %v", err)
	}
	defer manager.Stop(context.Background())
	_, err := manager.Upsert(context.Background(), dnfrobotdomain.Profile{
		ID: "wrong-owner", AccountID: "other-account", CharacterID: 9, ChannelID: 19,
	})
	if !errors.Is(err, dnfrobotdomain.ErrInvalid) {
		t.Fatalf("Upsert error = %v, want ErrInvalid", err)
	}
}

func TestManagerAutomationTickUsesReadyTownClientOnly(t *testing.T) {
	serverConn, clientConn := net.Pipe()
	defer serverConn.Close()
	defer clientConn.Close()
	client := NewClient(dnfrobotdomain.Profile{}, "pipe", []byte("secret"))
	client.mu.Lock()
	client.conn = clientConn
	client.state = StateTownReady
	client.village, client.area, client.x, client.y = 1, 2, 100, 200
	client.mu.Unlock()
	now := time.Unix(100, 0).UTC()
	runtime := &profileRuntime{
		profile: dnfrobotdomain.Profile{ID: "auto", Village: 1, Area: 2, X: 100, Y: 200, MoveType: 1},
		client:  client, nextMove: now.Add(-time.Second), nextChat: now.Add(-time.Second),
	}
	manager := NewManager(slog.New(slog.NewTextHandler(io.Discard, nil)), nil, staticRobotEndpoint("pipe"), "secret")
	result := make(chan error, 1)
	go func() {
		manager.automateRuntime(now, dnfrobotdomain.AutomationPolicy{
			Enabled: true, MoveIntervalMinSeconds: 5, MoveIntervalMaxSeconds: 5, MoveRadius: 20,
			ChatIntervalMinSeconds: 10, ChatIntervalMaxSeconds: 10, ChatMode: 3,
			ChatMessages: []string{"hello"},
		}, runtime)
		result <- nil
	}()
	opcode, body, err := readClientPacket(serverConn)
	if err != nil || opcode != 35 || len(body) != 7 || binary.LittleEndian.Uint16(body[0:2]) != 120 || binary.LittleEndian.Uint16(body[2:4]) != 200 {
		t.Fatalf("move packet = op%d %x, %v", opcode, body, err)
	}
	opcode, body, err = readClientPacket(serverConn)
	if err != nil || opcode != 17 || len(body) < 16 || body[0] != 3 || string(body[11:]) != "hello" {
		t.Fatalf("chat packet = op%d %x, %v", opcode, body, err)
	}
	if err := <-result; err != nil {
		t.Fatal(err)
	}
	runtime.mu.RLock()
	nextMove, nextChat := runtime.nextMove, runtime.nextChat
	runtime.mu.RUnlock()
	if !nextMove.Equal(now.Add(5*time.Second)) || !nextChat.Equal(now.Add(10*time.Second)) {
		t.Fatalf("next actions = %v / %v", nextMove, nextChat)
	}
}

func TestManagerAutomationOpensAndClosesCurrentPrivateStore(t *testing.T) {
	serverConn, clientConn := net.Pipe()
	defer serverConn.Close()
	defer clientConn.Close()
	profile := dnfrobotdomain.Profile{ID: "store", AccountID: "account", CharacterID: 7, ChannelID: 19, Village: 1, Area: 2, X: 100, Y: 200}
	client := NewClient(profile, "pipe", []byte("secret"))
	client.mu.Lock()
	client.conn = clientConn
	client.state = StateTownReady
	client.village, client.area, client.x, client.y = 1, 2, 100, 200
	client.mu.Unlock()
	now := time.Unix(200, 0).UTC()
	runtime := &profileRuntime{profile: profile, client: client, nextStore: now.Add(-time.Second)}
	manager := NewManager(slog.New(slog.NewTextHandler(io.Discard, nil)), nil, robotStoreEndpoint{
		staticRobotEndpoint: "pipe",
		rows:                []PrivateStoreRow{{DisplayIndex: 0, ListType: 0, Slot: 12, WireValue: 4}},
	}, "secret")
	policy := dnfrobotdomain.AutomationPolicy{
		Enabled: true, StoreIntervalMinSeconds: 10, StoreIntervalMaxSeconds: 10,
		StoreDurationSeconds: 5, StoreConcurrent: 1, StoreMaximumRows: 2,
		StoreUnitPrice: 1234, StoreTitle: "robot shop",
	}
	done := make(chan struct{})
	go func() { manager.automateRuntime(now, policy, runtime); close(done) }()
	opcode, body, err := readClientPacket(serverConn)
	if err != nil || opcode != privateStoreCreateOpcode || len(body) != 8 || binary.LittleEndian.Uint16(body[6:8]) != 0xffff {
		t.Fatalf("create packet = op%d %x, %v", opcode, body, err)
	}
	<-done

	handled := make(chan error, 1)
	go func() { handled <- client.handle(packet{Opcode: privateStoreCreateOpcode, Body: []byte{1}}) }()
	opcode, body, err = readClientPacket(serverConn)
	if err != nil || opcode != privateStoreCompleteOpcode || len(body) < 20 || body[4+len("robot shop")] != 1 {
		t.Fatalf("complete packet = op%d %x, %v", opcode, body, err)
	}
	if err := <-handled; err != nil {
		t.Fatal(err)
	}
	if err := client.handle(packet{Opcode: privateStoreCompleteOpcode, Body: []byte{1}}); err != nil {
		t.Fatal(err)
	}
	done = make(chan struct{})
	go func() { manager.automateRuntime(now.Add(5*time.Second), policy, runtime); close(done) }()
	opcode, body, err = readClientPacket(serverConn)
	if err != nil || opcode != privateStoreCloseOpcode || len(body) != 0 {
		t.Fatalf("close packet = op%d %x, %v", opcode, body, err)
	}
	<-done
}

func TestManagerAutomationRelocatesAndReconnectsAfterPrivateStoreRejection(t *testing.T) {
	serverConn, clientConn := net.Pipe()
	defer serverConn.Close()
	profile := dnfrobotdomain.Profile{
		ID: "store-retry", AccountID: "account", CharacterID: 7, ChannelID: 19,
		Village: 38, Area: 1, X: 500, Y: 600,
	}
	client := NewClient(profile, "pipe", []byte("secret"))
	client.mu.Lock()
	client.conn = clientConn
	client.state = StateStoreOpening
	client.mu.Unlock()
	client.resetPrivateStore(errors.New("fixture store rejected"))
	now := time.Unix(250, 0).UTC()
	runtime := &profileRuntime{
		profile: profile, client: client, autoStore: true,
		behaviorSeed: automationBehaviorSeed(profile), storeCursor: 3,
	}
	manager := NewManager(slog.New(slog.NewTextHandler(io.Discard, nil)), nil, staticRobotEndpoint("pipe"), "secret")
	manager.automateRuntime(now, dnfrobotdomain.AutomationPolicy{
		Enabled: true, StoreIntervalMinSeconds: 10, StoreIntervalMaxSeconds: 10,
	}, runtime)
	if state := client.Snapshot().State; state != StateStopped {
		t.Fatalf("client state = %s, want stopped", state)
	}
	runtime.mu.RLock()
	gotProfile, retryAt, lastAction := runtime.profile, runtime.storeRetryAt, runtime.lastAction
	runtime.mu.RUnlock()
	if gotProfile.X == profile.X && gotProfile.Y == profile.Y {
		t.Fatalf("store retry did not relocate profile: %+v", gotProfile)
	}
	if !retryAt.Equal(now.Add(30*time.Second)) || lastAction != "store_retry_relocate" {
		t.Fatalf("store retry state = retry:%v action:%q", retryAt, lastAction)
	}
}

func TestAutomationBehaviorPositionClampsToProfileBounds(t *testing.T) {
	profile := dnfrobotdomain.Profile{
		X: 500, Y: 200, MoveBoundsSet: true,
		MoveMinX: 420, MoveMaxX: 580, MoveMinY: 194, MoveMaxY: 206,
	}
	for cursor := 0; cursor < 100; cursor++ {
		x, y := automationBehaviorPositionForProfile(profile, 120, cursor, uint64(cursor+1))
		if x < profile.MoveMinX || x > profile.MoveMaxX || y < profile.MoveMinY || y > profile.MoveMaxY {
			t.Fatalf("cursor %d position=(%d,%d) outside profile=%+v", cursor, x, y, profile)
		}
	}
}

func TestManagerAutomationDisablesStoresAtDungeonEntrances(t *testing.T) {
	serverConn, clientConn := net.Pipe()
	defer serverConn.Close()
	defer clientConn.Close()
	profile := dnfrobotdomain.Profile{
		ID: "gate", AccountID: "account", CharacterID: 7, ChannelID: 19,
		Village: 38, Area: 3, X: 500, Y: 200, AutomaticStoreDisabled: true,
	}
	client := NewClient(profile, "pipe", []byte("secret"))
	client.mu.Lock()
	client.conn = clientConn
	client.state = StateTownReady
	client.mu.Unlock()
	now := time.Unix(300, 0).UTC()
	runtime := &profileRuntime{profile: profile, client: client, nextStore: now.Add(-time.Second)}
	manager := NewManager(slog.New(slog.NewTextHandler(io.Discard, nil)), nil, staticRobotEndpoint("pipe"), "secret")
	manager.automateRuntime(now, dnfrobotdomain.AutomationPolicy{
		Enabled: true, StoreIntervalMinSeconds: 10, StoreIntervalMaxSeconds: 10,
		StoreDurationSeconds: 5, StoreConcurrent: 1, StoreMaximumRows: 2,
		StoreUnitPrice: 1234, StoreTitle: "robot shop",
	}, runtime)
	runtime.mu.RLock()
	nextStore, automaticStore := runtime.nextStore, runtime.autoStore
	runtime.mu.RUnlock()
	if !nextStore.IsZero() || automaticStore {
		t.Fatalf("dungeon entrance store state next=%v active=%t", nextStore, automaticStore)
	}
}

func TestManagerExpiresUnacknowledgedCurrentProtocolOperation(t *testing.T) {
	serverConn, clientConn := net.Pipe()
	defer serverConn.Close()
	now := time.Unix(500, 0).UTC()
	client := NewClient(dnfrobotdomain.Profile{}, "pipe", []byte("secret"))
	client.mu.Lock()
	client.conn = clientConn
	client.state = StateStoreOpening
	client.stateSince = now.Add(-31 * time.Second)
	client.mu.Unlock()
	runtime := &profileRuntime{profile: dnfrobotdomain.Profile{ID: "timeout"}, client: client}
	manager := NewManager(slog.New(slog.NewTextHandler(io.Discard, nil)), nil, staticRobotEndpoint("pipe"), "secret")
	manager.running["timeout"] = runtime
	manager.automationTick(now)
	if state := client.Snapshot().State; state != StateStopped {
		t.Fatalf("state = %s, want stopped", state)
	}
	runtime.mu.RLock()
	lastError := runtime.lastError
	runtime.mu.RUnlock()
	if lastError != "dnf robot store_opening acknowledgement timed out" {
		t.Fatalf("last error = %q", lastError)
	}
}

func TestManagerSchedulerScalesOnlySchedulerManagedProfiles(t *testing.T) {
	manager := NewManager(slog.New(slog.NewTextHandler(io.Discard, nil)), nil, staticRobotEndpoint("127.0.0.1:1"), "secret")
	ctx, cancel := context.WithCancel(context.Background())
	manager.ctx, manager.cancel = ctx, cancel
	for index := 1; index <= 3; index++ {
		manager.config.Profiles = append(manager.config.Profiles, dnfrobotdomain.Profile{
			ID: "scale-" + strconv.Itoa(index), AccountID: "account-" + strconv.Itoa(index),
			CharacterID: uint16(index), ChannelID: 19,
		})
	}
	manager.config.Automation = dnfrobotdomain.AutomationPolicy{
		Enabled: true, TargetOnline: 2, ScaleIntervalSeconds: 1, ScaleUpBatch: 2, ScaleDownBatch: 2,
	}
	now := time.Unix(900, 0).UTC()
	manager.reconcileTargetOnline(now)
	if status := manager.Status(); status.Running != 2 || status.SchedulerManaged != 2 {
		t.Fatalf("scaled-up status = %+v", status)
	}
	if _, err := manager.StartProfile("scale-3"); err != nil {
		t.Fatalf("manual StartProfile: %v", err)
	}
	manager.mu.Lock()
	manager.config.Automation.TargetOnline = 1
	manager.nextScale = time.Time{}
	manager.mu.Unlock()
	manager.reconcileTargetOnline(now.Add(time.Second))
	status := manager.Status()
	if status.Running != 1 || status.SchedulerManaged != 0 {
		t.Fatalf("scaled-down status = %+v", status)
	}
	if snapshots := manager.List(); len(snapshots) != 3 || !snapshots[2].Running || snapshots[2].SchedulerManaged {
		t.Fatalf("manual profile was not preserved: %+v", snapshots)
	}
	stopCtx, stopCancel := context.WithTimeout(context.Background(), time.Second)
	defer stopCancel()
	if err := manager.Stop(stopCtx); err != nil {
		t.Fatalf("Stop: %v", err)
	}
}
