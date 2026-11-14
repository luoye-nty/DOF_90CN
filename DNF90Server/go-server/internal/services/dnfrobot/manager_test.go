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

type anyRobotEndpoint string

func (endpoint anyRobotEndpoint) RobotGameEndpoint(channelID int) (string, error) {
	if channelID <= 0 {
		return "", errors.New("invalid channel")
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

type configuredRobotStoreEndpoint struct {
	staticRobotEndpoint
	rows []PrivateStoreRow
}

func (endpoint configuredRobotStoreEndpoint) PrepareRobotPrivateStoreRows(
	_ context.Context,
	accountID string,
	characterID uint16,
	items []dnfrobotdomain.PrivateStoreItem,
) ([]PrivateStoreRow, error) {
	if accountID != "account" || characterID != 7 || len(items) != 1 || items[0].ItemID != 9001 {
		return nil, errors.New("unexpected configured private store plan")
	}
	return append([]PrivateStoreRow(nil), endpoint.rows...), nil
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
	if err := manager.MoveAndPersist(context.Background(), profile.ID, 1, 2, 333, 444, 1, PlacementBounds{
		Set: true, MinX: 300, MaxX: 400, MinY: 400, MaxY: 500,
	}); err != nil {
		t.Fatalf("MoveAndPersist: %v", err)
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
	saved := loaded.Profiles[0]
	if saved.X != 333 || saved.Y != 444 || !saved.MoveBoundsSet ||
		saved.MoveMinX != 300 || saved.MoveMaxX != 400 ||
		saved.MoveMinY != 400 || saved.MoveMaxY != 500 {
		t.Fatalf("persisted movement = %+v", saved)
	}
}

func TestManagerRelocatePersistsChannelTownAreaPositionAndBounds(t *testing.T) {
	runtime := dnfmemory.NewRuntime()
	group := runtime.Group()
	ctx := context.Background()
	if err := group.Character.Save(ctx, dnfrepo.CharacterRecord{
		CharacterID: "19", AccountID: "manual-robot-001", Slot: 1, Name: "机器人测试", Level: 90,
	}); err != nil {
		t.Fatalf("Save character: %v", err)
	}
	owner := dnfrobotdomain.NewOwner(func() (dnfrepo.Group, bool) { return group, true })
	expertCost := uint32(500)
	profile := dnfrobotdomain.Profile{
		ID: "manual-robot-001", AccountID: "manual-robot-001", CharacterID: 19, ChannelID: 19,
		Village: 38, Area: 0, X: 500, Y: 300, AutoStart: true,
		ExpertStoreKind: "disjoint", ExpertStoreCost: &expertCost,
	}
	if _, err := owner.Save(ctx, dnfrobotdomain.Config{Profiles: []dnfrobotdomain.Profile{profile}}); err != nil {
		t.Fatalf("Save config: %v", err)
	}
	manager := NewManager(
		slog.New(slog.NewTextHandler(io.Discard, nil)),
		func() (dnfrepo.Group, bool) { return group, true },
		anyRobotEndpoint("127.0.0.1:1"), "fixture-secret",
	)
	if err := manager.Start(ctx); err != nil {
		t.Fatalf("Start: %v", err)
	}
	defer manager.Stop(context.Background())

	autoStart := false
	snapshot, err := manager.RelocateWithAutoStart(ctx, profile.ID, 41, 40, 0, 1480, 480, 0, PlacementBounds{
		Set: true, MinX: 1469, MaxX: 1568, MinY: 464, MaxY: 513,
	}, &autoStart)
	if err != nil {
		t.Fatalf("Relocate: %v", err)
	}
	got := snapshot.Profile
	if got.ChannelID != 41 || got.Village != 40 || got.Area != 0 || got.X != 1480 || got.Y != 480 ||
		got.AutoStart ||
		!got.MoveBoundsSet || got.MoveMinX != 1469 || got.MoveMaxX != 1568 ||
		got.MoveMinY != 464 || got.MoveMaxY != 513 {
		t.Fatalf("relocated profile = %+v", got)
	}
	loaded, err := owner.Load(ctx)
	if err != nil || len(loaded.Profiles) != 1 {
		t.Fatalf("Load persisted config = %+v, %v", loaded, err)
	}
	saved := loaded.Profiles[0]
	if saved.ChannelID != 41 || saved.Village != 40 || saved.Area != 0 ||
		saved.X != 1480 || saved.Y != 480 || saved.AutoStart || !saved.MoveBoundsSet {
		t.Fatalf("persisted profile = %+v", saved)
	}
	updated, err := manager.SetExpertStoreCost(ctx, profile.ID, 0)
	if err != nil {
		t.Fatalf("SetExpertStoreCost: %v", err)
	}
	if updated.Profile.ExpertStoreCost == nil || *updated.Profile.ExpertStoreCost != 0 {
		t.Fatalf("updated expert store cost = %v", updated.Profile.ExpertStoreCost)
	}
	loaded, err = owner.Load(ctx)
	if err != nil || loaded.Profiles[0].ExpertStoreCost == nil || *loaded.Profiles[0].ExpertStoreCost != 0 {
		t.Fatalf("persisted expert store cost = %+v, %v", loaded, err)
	}
}

func TestManagerUpdateSpeechModesPersistsPerRobotMegaphoneChoice(t *testing.T) {
	runtime := dnfmemory.NewRuntime()
	group := runtime.Group()
	ctx := context.Background()
	if err := group.Character.Save(ctx, dnfrepo.CharacterRecord{
		CharacterID: "7", AccountID: "manual-robot-007", Slot: 1, Name: "speaker",
	}); err != nil {
		t.Fatalf("Save character: %v", err)
	}
	owner := dnfrobotdomain.NewOwner(func() (dnfrepo.Group, bool) { return group, true })
	profile := dnfrobotdomain.Profile{
		ID: "manual-robot-007", AccountID: "manual-robot-007", CharacterID: 7, ChannelID: 19,
		Village: 38, Area: 0, X: 500, Y: 300,
	}
	if _, err := owner.Save(ctx, dnfrobotdomain.Config{Profiles: []dnfrobotdomain.Profile{profile}}); err != nil {
		t.Fatalf("Save config: %v", err)
	}
	manager := NewManager(
		slog.New(slog.NewTextHandler(io.Discard, nil)),
		func() (dnfrepo.Group, bool) { return group, true },
		staticRobotEndpoint("127.0.0.1:1"), "fixture-secret",
	)
	if err := manager.Start(ctx); err != nil {
		t.Fatalf("Start: %v", err)
	}
	defer manager.Stop(context.Background())
	rows, err := manager.UpdateSpeechModes(ctx, []SpeechMode{{ID: profile.ID, Megaphone: true}})
	if err != nil {
		t.Fatalf("UpdateSpeechModes: %v", err)
	}
	if len(rows) != 1 || !rows[0].Profile.MegaphoneEnabled {
		t.Fatalf("updated speech modes = %+v", rows)
	}
	loaded, err := owner.Load(ctx)
	if err != nil || len(loaded.Profiles) != 1 || !loaded.Profiles[0].MegaphoneEnabled {
		t.Fatalf("persisted speech mode = %+v, %v", loaded, err)
	}
}

func TestManagerUpdateProfileRecyclePolicyPersistsPerNormalRobot(t *testing.T) {
	runtime := dnfmemory.NewRuntime()
	group := runtime.Group()
	ctx := context.Background()
	if err := group.Character.Save(ctx, dnfrepo.CharacterRecord{
		CharacterID: "8", AccountID: "manual-robot-008", Slot: 1, Name: "recycler",
	}); err != nil {
		t.Fatal(err)
	}
	owner := dnfrobotdomain.NewOwner(func() (dnfrepo.Group, bool) { return group, true })
	profile := dnfrobotdomain.Profile{
		ID: "manual-robot-008", AccountID: "manual-robot-008", CharacterID: 8, ChannelID: 19,
		RobotType: dnfrobotdomain.RobotTypeNormal, Village: 38, Area: 0, X: 500, Y: 300,
	}
	if _, err := owner.Save(ctx, dnfrobotdomain.Config{Profiles: []dnfrobotdomain.Profile{profile}}); err != nil {
		t.Fatal(err)
	}
	manager := NewManager(
		slog.New(slog.NewTextHandler(io.Discard, nil)),
		func() (dnfrepo.Group, bool) { return group, true },
		staticRobotEndpoint("127.0.0.1:1"), "fixture-secret",
	)
	if err := manager.Start(ctx); err != nil {
		t.Fatal(err)
	}
	defer manager.Stop(context.Background())
	policy := dnfrobotdomain.RecyclePolicy{
		Enabled: true, BuyItems: []dnfrobotdomain.RecycleBuyItem{{ItemID: 3037, Price: 25}},
	}
	snapshot, err := manager.UpdateProfileRecyclePolicy(ctx, profile.ID, policy)
	if err != nil {
		t.Fatal(err)
	}
	if !snapshot.Profile.Recycle.Enabled || len(snapshot.Profile.Recycle.BuyItems) != 1 {
		t.Fatalf("updated recycle policy = %+v", snapshot.Profile.Recycle)
	}
	loaded, err := owner.Load(ctx)
	if err != nil || len(loaded.Profiles) != 1 || !loaded.Profiles[0].Recycle.Enabled {
		t.Fatalf("persisted recycle policy = %+v, %v", loaded, err)
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

func TestAmbientAutomationPolicyUsesDefaultsWithoutEnablingAutomaticStores(t *testing.T) {
	policy := ambientAutomationPolicy(dnfrobotdomain.AutomationPolicy{})
	if !policy.Enabled || policy.TargetOnline != 0 ||
		policy.MoveIntervalMinSeconds != 8 || policy.MoveIntervalMaxSeconds != 30 || policy.MoveRadius != 120 ||
		policy.ChatIntervalMinSeconds != 60 || policy.ChatIntervalMaxSeconds != 180 || policy.ChatMode != 3 ||
		len(policy.ChatMessages) == 0 ||
		policy.StoreIntervalMinSeconds != 0 || policy.StoreIntervalMaxSeconds != 0 ||
		policy.MegaphoneIntervalMinSeconds != 0 {
		t.Fatalf("ambient policy = %+v", policy)
	}
	custom := ambientAutomationPolicy(dnfrobotdomain.AutomationPolicy{
		MoveIntervalMinSeconds: 12, MoveIntervalMaxSeconds: 20, MoveRadius: 80,
		ChatIntervalMinSeconds: 100, ChatIntervalMaxSeconds: 200, ChatMode: 3,
		ChatMessages:                []string{"custom"},
		MegaphoneIntervalMinSeconds: 900, MegaphoneIntervalMaxSeconds: 1800,
		MegaphoneProfileModulo: 3,
	})
	if custom.MoveIntervalMinSeconds != 12 || custom.MoveRadius != 80 ||
		custom.ChatIntervalMinSeconds != 100 || len(custom.ChatMessages) != 1 || custom.ChatMessages[0] != "custom" ||
		custom.MegaphoneIntervalMinSeconds != 900 || custom.MegaphoneIntervalMaxSeconds != 1800 ||
		custom.MegaphoneProfileModulo != 3 {
		t.Fatalf("custom ambient policy = %+v", custom)
	}
}

func TestOpenConfiguredPrivateStoreMovesToProfilePositionFirst(t *testing.T) {
	serverConn, clientConn := net.Pipe()
	defer serverConn.Close()
	defer clientConn.Close()
	profile := dnfrobotdomain.Profile{
		ID: "shop", AccountID: "account", CharacterID: 7, ChannelID: 19,
		Village: 1, Area: 2, X: 333, Y: 444,
		PrivateStoreTitle: "shop",
		PrivateStoreItems: []dnfrobotdomain.PrivateStoreItem{{ItemID: 9001, Count: 1, Price: 777}},
	}
	client := NewClient(profile, "pipe", []byte("secret"))
	client.mu.Lock()
	client.conn = clientConn
	client.state = StateTownReady
	client.village, client.area, client.x, client.y = 1, 2, 100, 200
	client.mu.Unlock()
	manager := NewManager(
		slog.New(slog.NewTextHandler(io.Discard, nil)), nil,
		configuredRobotStoreEndpoint{
			staticRobotEndpoint: "pipe",
			rows:                []PrivateStoreRow{{DisplayIndex: 0, Price: 777, ListType: 0, Slot: 9, WireValue: 1}},
		}, "secret",
	)
	manager.config = dnfrobotdomain.Config{Profiles: []dnfrobotdomain.Profile{profile}}
	runtime := &profileRuntime{profile: profile, client: client}
	manager.running[profileKey(profile.ID)] = runtime

	packets := make(chan [2]struct {
		opcode uint16
		body   []byte
	}, 1)
	go func() {
		var received [2]struct {
			opcode uint16
			body   []byte
		}
		for index := range received {
			opcode, body, err := readClientPacket(serverConn)
			if err != nil {
				packets <- received
				return
			}
			received[index].opcode, received[index].body = opcode, body
		}
		packets <- received
	}()
	if err := manager.OpenConfiguredPrivateStore(context.Background(), profile.ID); err != nil {
		t.Fatalf("OpenConfiguredPrivateStore: %v", err)
	}
	received := <-packets
	if received[0].opcode != 35 || len(received[0].body) != 7 ||
		binary.LittleEndian.Uint16(received[0].body[0:2]) != 333 ||
		binary.LittleEndian.Uint16(received[0].body[2:4]) != 444 {
		t.Fatalf("position packet = op%d %x", received[0].opcode, received[0].body)
	}
	if received[1].opcode != privateStoreCreateOpcode {
		t.Fatalf("create packet opcode = %d", received[1].opcode)
	}
	runtime.mu.RLock()
	transition := runtime.privateStoreTransition
	runtime.mu.RUnlock()
	if transition {
		t.Fatal("private store transition flag remained set")
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

func TestExpertMachineRecoveryThresholdIsStrictlyBelowTenPercent(t *testing.T) {
	tests := []struct {
		name  string
		state ExpertMachineState
		want  bool
	}{
		{name: "below", state: ExpertMachineState{Endurance: 69, MaxEndurance: 700}, want: true},
		{name: "exact-ten-percent", state: ExpertMachineState{Endurance: 70, MaxEndurance: 700}, want: false},
		{name: "above", state: ExpertMachineState{Endurance: 71, MaxEndurance: 700}, want: false},
		{name: "zero", state: ExpertMachineState{Endurance: 0, MaxEndurance: 700}, want: true},
		{name: "unknown-max", state: ExpertMachineState{Endurance: 0, MaxEndurance: 0}, want: false},
	}
	for _, test := range tests {
		if got := expertMachineBelowTenPercent(test.state); got != test.want {
			t.Fatalf("%s: got=%t want=%t state=%+v", test.name, got, test.want, test.state)
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
