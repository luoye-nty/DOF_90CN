package dnfbridge

import (
	"bytes"
	"context"
	"encoding/binary"
	"errors"
	"testing"

	"longheng.io/server/internal/modules/dnf/channelcatalog"
	"longheng.io/server/internal/modules/dnf/dnfenum"
	dnfproto "longheng.io/server/internal/modules/dnf/protocol"
	dnfrepo "longheng.io/server/internal/modules/dnf/repository"
	dnfrepomemory "longheng.io/server/internal/modules/dnf/repository/memory"
)

func buildCurrentGameOptionSaveBody() []byte {
	body := make([]byte, currentGameOptionSaveBodySize)
	binary.LittleEndian.PutUint32(body[:currentGameOptionSaveHeaderLen], currentGameOptionMainBodySize)
	for index := currentGameOptionSaveHeaderLen; index < len(body); index++ {
		body[index] = byte(index)
	}
	return body
}

func buildCurrentGameOptionKeymapBody() []byte {
	keymap := make([]byte, 649)
	const row = "I01\t2\n"
	for index := range keymap {
		keymap[index] = row[index%len(row)]
	}
	return keymap
}

func buildCurrentGameOptionKeymapSaveBody(keymap []byte) []byte {
	body := make([]byte, currentGameOptionSaveHeaderLen+len(keymap))
	binary.LittleEndian.PutUint32(body[:currentGameOptionSaveHeaderLen], uint32(len(keymap)))
	copy(body[currentGameOptionSaveHeaderLen:], keymap)
	return body
}

func setCurrentGameOptionJobAwakenEffect(body []byte, enabled bool) {
	value := uint16(0)
	if enabled {
		value = 1
	}
	binary.LittleEndian.PutUint16(
		body[currentGameOptionSaveHeaderLen+currentGameOptionJobAwakenEffectOffset:],
		value,
	)
}

func setCurrentGameOptionShowAllAvatar(body []byte, enabled bool) {
	value := uint16(0)
	if enabled {
		value = 1
	}
	binary.LittleEndian.PutUint16(
		body[currentGameOptionSaveHeaderLen+currentGameOptionShowAllAvatarOffset:],
		value,
	)
}

func setCurrentGameOptionVisibleGrowAvatar(body []byte, enabled bool) {
	value := uint16(0)
	if enabled {
		value = 1
	}
	binary.LittleEndian.PutUint16(
		body[currentGameOptionSaveHeaderLen+currentGameOptionVisibleGrowAvatarOffset:],
		value,
	)
}

func setCurrentGameOptionRapidFire(body []byte, enabled bool) {
	value := uint16(0)
	if enabled {
		value = 1
	}
	binary.LittleEndian.PutUint16(
		body[currentGameOptionSaveHeaderLen+currentGameOptionRapidFireOffset:],
		value,
	)
}

func TestCurrentGameOptionJobAwakenEffectUsesOption1(t *testing.T) {
	main := make([]byte, currentGameOptionMainBodySize)
	binary.LittleEndian.PutUint16(main[74*2:], 1)
	if enabled, ok := currentGameOptionJobAwakenEffectEnabled(main); !ok || enabled {
		t.Fatalf("option74 selected option1 clear: enabled=%t ok=%t", enabled, ok)
	}
	binary.LittleEndian.PutUint16(main[74*2:], 0)
	binary.LittleEndian.PutUint16(main[1*2:], 1)
	if enabled, ok := currentGameOptionJobAwakenEffectEnabled(main); !ok || !enabled {
		t.Fatalf("option1 selected: enabled=%t ok=%t", enabled, ok)
	}
}

func TestCurrentGameOptionJobAwakenEffectPreservesOtherUserStateBits(t *testing.T) {
	for _, test := range []struct {
		name    string
		current byte
		enabled bool
		want    byte
	}{
		{name: "clear bit1", current: 0xa3, enabled: false, want: 0xa1},
		{name: "set bit1", current: 0xa1, enabled: true, want: 0xa3},
		{name: "default clear", current: currentActorDefaultUserStateBits, enabled: false, want: 0x01},
		{name: "preserve bit3", current: 0x09, enabled: true, want: 0x0b},
	} {
		t.Run(test.name, func(t *testing.T) {
			if got := currentGameOptionJobAwakenEffectUserState(test.current, test.enabled); got != test.want {
				t.Fatalf("user state=%#x want=%#x", got, test.want)
			}
		})
	}
}

func TestCurrentGameOptionVisibilityMapsAwakenAndFullAvatarBits(t *testing.T) {
	main := make([]byte, currentGameOptionMainBodySize)
	binary.LittleEndian.PutUint16(main[currentGameOptionVisibleGrowAvatarOffset:], 0)
	binary.LittleEndian.PutUint16(main[currentGameOptionShowAllAvatarOffset:], 1)
	if enabled, ok := currentGameOptionVisibleGrowAvatarEnabled(main); !ok || enabled {
		t.Fatalf("option74 cleared: enabled=%t ok=%t", enabled, ok)
	}
	if enabled, ok := currentGameOptionShowAllAvatarEnabled(main); !ok || !enabled {
		t.Fatalf("option55 selected: enabled=%t ok=%t", enabled, ok)
	}
	if got := currentGameOptionVisibilityUserState(0xab, false, true, true); got != 0xa1 {
		t.Fatalf("show all visibility=%#x want=0xa1", got)
	}
	if got := currentGameOptionVisibilityUserState(0xab, false, false, true); got != 0xb1 {
		t.Fatalf("hide awaken avatar visibility=%#x want=0xb1", got)
	}
	if got := currentGameOptionVisibilityUserState(0xa1, true, false, false); got != 0xbb {
		t.Fatalf("hide all and awaken avatar visibility=%#x want=0xbb", got)
	}
}

func TestCurrentGameOptionRapidFireUsesLiveType197Option50(t *testing.T) {
	main := make([]byte, currentGameOptionMainBodySize)
	if enabled, ok := currentGameOptionRapidFireEnabled(main); !ok || enabled {
		t.Fatalf("default rapid fire enabled=%t ok=%t", enabled, ok)
	}
	binary.LittleEndian.PutUint16(main[currentGameOptionRapidFireOffset:], 1)
	if enabled, ok := currentGameOptionRapidFireEnabled(main); !ok || !enabled {
		t.Fatalf("option50 rapid fire enabled=%t ok=%t", enabled, ok)
	}
	binary.LittleEndian.PutUint16(main[(currentGameOptionRapidFireIndex-1)*2:], 1)
	binary.LittleEndian.PutUint16(main[currentGameOptionRapidFireOffset:], 0)
	if enabled, ok := currentGameOptionRapidFireEnabled(main); !ok || enabled {
		t.Fatalf("neighbor option49 incorrectly enabled rapid fire=%t ok=%t", enabled, ok)
	}
}

func TestCurrentGameOptionJobAwakenEffectChangePersistsAndRefreshesReadyTownActorOnce(t *testing.T) {
	ctx := context.Background()
	repositories := dnfrepomemory.NewMemoryGroup()
	character := dnfrepo.CharacterRecord{
		CharacterID: "19",
		AccountID:   "account-a",
		Name:        "Actor19",
		Job:         "1",
		Level:       90,
		Stats:       map[string]int64{"grow_type": 3, currentGameOptionUserStateStat: 0xa3},
	}
	if err := repositories.Character.Save(ctx, character); err != nil {
		t.Fatal(err)
	}
	connection := &bufferConn{}
	service := &Service{
		options: options{
			accountID:          "account-a",
			gameUpperHeader:    gameUpperHeaderServer16,
			gameUpperBodyCodec: gameUpperBodyCodecPlain,
		},
		repositoryProvider: func() (dnfrepo.Group, bool) { return repositories, true },
	}
	session := &gameSession{
		conn:                            connection,
		connID:                          "game-option-user-state-refresh",
		accountID:                       "account-a",
		channel:                         channelcatalog.Channel{ID: 19},
		selectedCharacterID:             19,
		townSceneReadyCharacterID:       19,
		connectionTownActorOwnerChannel: 19,
		townActorOwnerChannel:           19,
	}
	body := buildCurrentGameOptionSaveBody()
	setCurrentGameOptionJobAwakenEffect(body, true)
	if err := service.handleCurrentSaveGameOption(session, body); err != nil {
		t.Fatal(err)
	}
	if connection.write.Len() != 0 {
		t.Fatalf("first save using default-enabled state refreshed actor: %x", connection.write.Bytes())
	}
	setCurrentGameOptionJobAwakenEffect(body, false)
	if err := service.handleCurrentSaveGameOption(session, body); err != nil {
		t.Fatal(err)
	}
	packet, trailing := splitLongHengGameServerUpperPacket(t, connection.write.Bytes())
	if packet.Header.Classification != 0 || packet.Header.MsgID != currentActorUserStateMsgID || len(trailing) != 0 {
		t.Fatalf("user-state refresh header=%+v trailing=%x", packet.Header, trailing)
	}
	if !bytes.Equal(packet.Body, []byte{19, 0, 0xa1}) {
		t.Fatalf("user-state refresh body=%x want=1300a1", packet.Body)
	}
	stored, found, err := repositories.Character.Load(ctx, "19")
	if err != nil || !found || stored.Stats[currentGameOptionUserStateStat] != 0xa1 {
		t.Fatalf("stored user state found=%t err=%v stats=%v", found, err, stored.Stats)
	}
	legacy := fakeLegacyUserInfoRepo{rows: map[string][]dnfrepo.LegacyUserInfoRow{
		"legacy_character_subtype0_fields": {{"user_state_bits": "3"}},
	}}
	serviceForReselect := Service{repositoryProvider: func() (dnfrepo.Group, bool) { return repositories, true }}
	userinfo := serviceForReselect.buildCSharpSelectedUserInfoBody(
		ctx,
		nil,
		legacy,
		0,
		stored,
		true,
		19,
		"Actor19",
	)
	const subtype0UserStateOffset = 5 + 4 + len("Actor19") + 7 + 65
	if len(userinfo) <= subtype0UserStateOffset || userinfo[subtype0UserStateOffset] != 0xa1 {
		t.Fatalf("reselect USERINFO0 user-state byte=%x want=a1", userinfo)
	}
	main, found, err := loadCurrentGameOption(ctx, repositories.Settings, "account-a")
	if err != nil || !found {
		t.Fatalf("stored game option found=%t err=%v", found, err)
	}
	if enabled, ok := currentGameOptionJobAwakenEffectEnabled(main); !ok || enabled {
		t.Fatalf("stored option1 enabled=%t ok=%t want=false", enabled, ok)
	}
	before := connection.write.Len()
	if err := service.handleCurrentSaveGameOption(session, body); err != nil {
		t.Fatal(err)
	}
	if connection.write.Len() != before {
		t.Fatalf("unchanged save sent another op357: before=%d after=%d", before, connection.write.Len())
	}
}

func TestCurrentGameOptionJobAwakenEffectChangePersistsWithoutRefreshingUnreadyScene(t *testing.T) {
	ctx := context.Background()
	repositories := dnfrepomemory.NewMemoryGroup()
	character := dnfrepo.CharacterRecord{
		CharacterID: "19",
		AccountID:   "account-a",
		Name:        "Actor19",
		Level:       90,
		Stats:       map[string]int64{currentGameOptionUserStateStat: int64(currentActorDefaultUserStateBits)},
	}
	if err := repositories.Character.Save(ctx, character); err != nil {
		t.Fatal(err)
	}
	connection := &bufferConn{}
	service := &Service{
		options:            options{accountID: "account-a", gameUpperHeader: gameUpperHeaderServer16, gameUpperBodyCodec: gameUpperBodyCodecPlain},
		repositoryProvider: func() (dnfrepo.Group, bool) { return repositories, true },
	}
	session := &gameSession{conn: connection, accountID: "account-a", selectedCharacterID: 19}
	body := buildCurrentGameOptionSaveBody()
	setCurrentGameOptionJobAwakenEffect(body, false)
	if err := service.handleCurrentSaveGameOption(session, body); err != nil {
		t.Fatal(err)
	}
	if connection.write.Len() != 0 {
		t.Fatalf("unready scene received op357 refresh: %x", connection.write.Bytes())
	}
	stored, found, err := repositories.Character.Load(ctx, "19")
	if err != nil || !found || stored.Stats[currentGameOptionUserStateStat] != 1 {
		t.Fatalf("unready scene did not persist user state found=%t err=%v stats=%v", found, err, stored.Stats)
	}
}

func TestCurrentGameOptionJobAwakenEffectChangePersistsWithoutTownRefreshInDungeon(t *testing.T) {
	ctx := context.Background()
	repositories := dnfrepomemory.NewMemoryGroup()
	character := dnfrepo.CharacterRecord{
		CharacterID: "19",
		AccountID:   "account-a",
		Name:        "Actor19",
		Level:       90,
		Stats:       map[string]int64{currentGameOptionUserStateStat: int64(currentActorDefaultUserStateBits)},
	}
	if err := repositories.Character.Save(ctx, character); err != nil {
		t.Fatal(err)
	}
	connection := &bufferConn{}
	service := &Service{
		options:            options{accountID: "account-a", gameUpperHeader: gameUpperHeaderServer16, gameUpperBodyCodec: gameUpperBodyCodecPlain},
		repositoryProvider: func() (dnfrepo.Group, bool) { return repositories, true },
	}
	session := &gameSession{
		conn:                            connection,
		accountID:                       "account-a",
		selectedCharacterID:             19,
		townSceneReadyCharacterID:       19,
		connectionTownActorOwnerChannel: 19,
		townActorOwnerChannel:           19,
		dungeon:                         dungeonSessionState{runtime: &runtimeDungeonState{Character: character}},
	}
	body := buildCurrentGameOptionSaveBody()
	setCurrentGameOptionJobAwakenEffect(body, false)
	if err := service.handleCurrentSaveGameOption(session, body); err != nil {
		t.Fatal(err)
	}
	if connection.write.Len() != 0 {
		t.Fatalf("active dungeon received town op357 refresh: %x", connection.write.Bytes())
	}
	main, found, err := loadCurrentGameOption(ctx, repositories.Settings, "account-a")
	if err != nil || !found {
		t.Fatalf("load active-dungeon save found=%t err=%v", found, err)
	}
	if enabled, ok := currentGameOptionJobAwakenEffectEnabled(main); !ok || enabled {
		t.Fatalf("active-dungeon saved option enabled=%t ok=%t want=false", enabled, ok)
	}
	stored, found, err := repositories.Character.Load(ctx, "19")
	if err != nil || !found || stored.Stats[currentGameOptionUserStateStat] != 1 {
		t.Fatalf("active-dungeon stored user state found=%t err=%v stats=%v", found, err, stored.Stats)
	}
}

func TestCurrentGameOptionVisibilityReconcilesNewlySelectedCharacterFromAccountState(t *testing.T) {
	ctx := context.Background()
	repositories := dnfrepomemory.NewMemoryGroup()
	character := dnfrepo.CharacterRecord{
		CharacterID: "13",
		AccountID:   "account-a",
		Stats:       map[string]int64{currentGameOptionUserStateStat: 0x03},
	}
	if err := repositories.Character.Save(ctx, character); err != nil {
		t.Fatal(err)
	}
	option := buildCurrentGameOptionSaveBody()
	setCurrentGameOptionJobAwakenEffect(option, false)
	setCurrentGameOptionVisibleGrowAvatar(option, false)
	setCurrentGameOptionShowAllAvatar(option, true)
	if err := saveCurrentGameOption(
		ctx,
		repositories.Settings,
		"account-a",
		option[currentGameOptionSaveHeaderLen:],
	); err != nil {
		t.Fatal(err)
	}
	service := &Service{
		repositoryProvider: func() (dnfrepo.Group, bool) { return repositories, true },
	}
	session := &gameSession{accountID: "account-a"}
	reconciled, changed, err := service.reconcileCurrentGameOptionVisibilityForSelectedCharacter(
		ctx,
		session,
		character,
	)
	if err != nil {
		t.Fatal(err)
	}
	if !changed || reconciled.Stats[currentGameOptionUserStateStat] != 0x11 {
		t.Fatalf("reconciled changed=%t stats=%v", changed, reconciled.Stats)
	}
	stored, found, err := repositories.Character.Load(ctx, "13")
	if err != nil || !found || stored.Stats[currentGameOptionUserStateStat] != 0x11 {
		t.Fatalf("stored selected character visibility found=%t err=%v stats=%v", found, err, stored.Stats)
	}
}

func TestCurrentGameOptionPersistsExactMainBodyPerAccount(t *testing.T) {
	ctx := context.Background()
	repositories := dnfrepomemory.NewMemoryGroup()
	connection := &bufferConn{}
	service := &Service{
		options:            options{gameUpperHeader: gameUpperHeaderServer16, gameUpperBodyCodec: gameUpperBodyCodecPlain},
		repositoryProvider: func() (dnfrepo.Group, bool) { return repositories, true },
	}
	body := buildCurrentGameOptionSaveBody()
	if err := repositories.Character.Save(ctx, dnfrepo.CharacterRecord{
		CharacterID: "3",
		AccountID:   "account-a",
		Stats:       map[string]int64{currentGameOptionUserStateStat: int64(currentActorDefaultUserStateBits)},
	}); err != nil {
		t.Fatal(err)
	}
	session := &gameSession{conn: connection, accountID: "account-a", selectedCharacterID: 3}

	if err := service.handleCurrentSaveGameOption(session, buildCurrentGameOptionSaveBody()); err != nil {
		t.Fatalf("save game option: %v", err)
	}
	main, found, err := loadCurrentGameOption(ctx, repositories.Settings, "account-a")
	if err != nil || !found || !bytes.Equal(main, body[currentGameOptionSaveHeaderLen:]) {
		t.Fatalf("load account-a found=%t err=%v body=%x", found, err, main)
	}
	if _, found, err := loadCurrentGameOption(ctx, repositories.Settings, "account-b"); err != nil || found {
		t.Fatalf("other account found=%t err=%v", found, err)
	}
	if connection.write.Len() != 0 {
		t.Fatalf("saving game options injected a native apply packet: %x", connection.write.Bytes())
	}
}

func TestCurrentGameOptionWithoutSelectedCharacterSavesAccountOnly(t *testing.T) {
	ctx := context.Background()
	repositories := dnfrepomemory.NewMemoryGroup()
	service := &Service{repositoryProvider: func() (dnfrepo.Group, bool) { return repositories, true }}
	persisted := buildCurrentGameOptionSaveBody()
	setCurrentGameOptionJobAwakenEffect(persisted, true)
	setCurrentGameOptionShowAllAvatar(persisted, true)
	if err := saveCurrentGameOption(ctx, repositories.Settings, "account-a", persisted[currentGameOptionSaveHeaderLen:]); err != nil {
		t.Fatal(err)
	}
	body := buildCurrentGameOptionSaveBody()
	setCurrentGameOptionJobAwakenEffect(body, false)
	setCurrentGameOptionShowAllAvatar(body, false)

	if err := service.handleCurrentSaveGameOption(&gameSession{accountID: "account-a"}, body); err != nil {
		t.Fatal(err)
	}
	main, found, err := loadCurrentGameOption(ctx, repositories.Settings, "account-a")
	if err != nil || !found || !bytes.Equal(main, body[currentGameOptionSaveHeaderLen:]) {
		t.Fatalf("account-only save found=%t err=%v body=%x", found, err, main)
	}
	characters, err := repositories.Character.ListByAccount(ctx, "account-a", 0)
	if err != nil || len(characters) != 0 {
		t.Fatalf("account-only save created character rows=%v err=%v", characters, err)
	}
}

func TestCurrentGameOptionSavePersistsDisableDuringSceneRebuild(t *testing.T) {
	ctx := context.Background()
	repositories := dnfrepomemory.NewMemoryGroup()
	if err := repositories.Character.Save(ctx, dnfrepo.CharacterRecord{
		CharacterID: "20",
		AccountID:   "account-a",
		Stats:       map[string]int64{currentGameOptionUserStateStat: 0x03},
	}); err != nil {
		t.Fatal(err)
	}
	persisted := buildCurrentGameOptionSaveBody()
	setCurrentGameOptionJobAwakenEffect(persisted, true)
	setCurrentGameOptionShowAllAvatar(persisted, true)
	if err := saveCurrentGameOption(ctx, repositories.Settings, "account-a", persisted[currentGameOptionSaveHeaderLen:]); err != nil {
		t.Fatal(err)
	}
	service := &Service{repositoryProvider: func() (dnfrepo.Group, bool) { return repositories, true }}
	incoming := buildCurrentGameOptionSaveBody()
	setCurrentGameOptionJobAwakenEffect(incoming, false)
	setCurrentGameOptionVisibleGrowAvatar(incoming, false)
	setCurrentGameOptionShowAllAvatar(incoming, true)

	if err := service.handleCurrentSaveGameOption(&gameSession{accountID: "account-a", selectedCharacterID: 20}, incoming); err != nil {
		t.Fatal(err)
	}
	main, found, err := loadCurrentGameOption(ctx, repositories.Settings, "account-a")
	if err != nil || !found {
		t.Fatalf("load saved game option found=%t err=%v", found, err)
	}
	if enabled, ok := currentGameOptionJobAwakenEffectEnabled(main); !ok || enabled {
		t.Fatalf("scene rebuild save did not persist opt[1]=0: enabled=%t ok=%t", enabled, ok)
	}
	if enabled, ok := currentGameOptionShowAllAvatarEnabled(main); !ok || !enabled {
		t.Fatalf("scene rebuild save did not persist opt[55]=1: enabled=%t ok=%t", enabled, ok)
	}
	character, found, err := repositories.Character.Load(ctx, "20")
	if err != nil || !found || character.Stats[currentGameOptionUserStateStat] != 0x11 {
		t.Fatalf("scene rebuild save user state found=%t err=%v stats=%v", found, err, character.Stats)
	}
}

func TestCurrentGameOptionRejectsMalformedSaveWithoutReplacingState(t *testing.T) {
	ctx := context.Background()
	repositories := dnfrepomemory.NewMemoryGroup()
	service := &Service{repositoryProvider: func() (dnfrepo.Group, bool) { return repositories, true }}
	body := buildCurrentGameOptionSaveBody()
	session := &gameSession{accountID: "account-a"}

	if err := service.handleCurrentSaveGameOption(session, body); err != nil {
		t.Fatal(err)
	}
	malformed := append([]byte(nil), body...)
	binary.LittleEndian.PutUint32(malformed[:currentGameOptionSaveHeaderLen], currentGameOptionMainBodySize-1)
	if err := service.handleCurrentSaveGameOption(session, malformed); err != nil {
		t.Fatalf("malformed save returned error: %v", err)
	}
	main, found, err := loadCurrentGameOption(ctx, repositories.Settings, "account-a")
	if err != nil || !found || !bytes.Equal(main, body[currentGameOptionSaveHeaderLen:]) {
		t.Fatalf("malformed save replaced state found=%t err=%v body=%x", found, err, main)
	}
}

func TestCurrentGameOptionKeymapSavePersistsExactBodyWithoutReplacingMainOrOtherValues(t *testing.T) {
	ctx := context.Background()
	repositories := testRepositoryGroup()
	main := buildCurrentGameOptionSaveBody()[currentGameOptionSaveHeaderLen:]
	if err := repositories.Settings.Save(ctx, dnfrepo.SettingsRecord{
		Scope:  currentGameOptionScope("account-a"),
		Values: map[string]string{"unrelated": "preserve-me"},
	}); err != nil {
		t.Fatal(err)
	}
	if err := saveCurrentGameOption(ctx, repositories.Settings, "account-a", main); err != nil {
		t.Fatal(err)
	}
	service := &Service{repositoryProvider: func() (dnfrepo.Group, bool) { return repositories, true }}
	session := &gameSession{accountID: "account-a", selectedCharacterID: 19}
	keymap := buildCurrentGameOptionKeymapBody()
	if err := service.handleCurrentSaveGameOptionKeymap(session, buildCurrentGameOptionKeymapSaveBody(keymap)); err != nil {
		t.Fatal(err)
	}
	state, found, err := loadCurrentGameOptionState(ctx, repositories.Settings, "account-a")
	if err != nil || !found || !bytes.Equal(state.main, main) || !bytes.Equal(state.keymap, keymap) {
		t.Fatalf("saved state found=%t err=%v main_len=%d keymap_len=%d", found, err, len(state.main), len(state.keymap))
	}
	updatedMain := append([]byte(nil), main...)
	updatedMain[0] ^= 0xff
	if err := saveCurrentGameOption(ctx, repositories.Settings, "account-a", updatedMain); err != nil {
		t.Fatal(err)
	}
	state, found, err = loadCurrentGameOptionState(ctx, repositories.Settings, "account-a")
	if err != nil || !found || !bytes.Equal(state.main, updatedMain) || !bytes.Equal(state.keymap, keymap) {
		t.Fatalf("updated state found=%t err=%v main_len=%d keymap_len=%d", found, err, len(state.main), len(state.keymap))
	}
	record, found, err := repositories.Settings.Load(ctx, currentGameOptionScope("account-a"))
	if err != nil || !found || record.Values["unrelated"] != "preserve-me" {
		t.Fatalf("unrelated setting found=%t err=%v values=%v", found, err, record.Values)
	}
}

func TestCurrentGameOptionKeymapSaveUsesCharacterScopeForCreator(t *testing.T) {
	for _, testCase := range []struct {
		name string
		job  string
	}{
		{name: "creator", job: "10"},
	} {
		t.Run(testCase.name, func(t *testing.T) {
			ctx := context.Background()
			repositories := testRepositoryGroup()
			if err := repositories.Character.Save(ctx, dnfrepo.CharacterRecord{
				CharacterID: "19",
				AccountID:   "account-a",
				Job:         testCase.job,
			}); err != nil {
				t.Fatal(err)
			}
			accountKeymap := []byte("I01\t2\n")
			if err := saveCurrentGameOptionKeymap(ctx, repositories.Settings, "account-a", accountKeymap); err != nil {
				t.Fatal(err)
			}
			service := &Service{repositoryProvider: func() (dnfrepo.Group, bool) { return repositories, true }}
			session := &gameSession{accountID: "account-a", selectedCharacterID: 19}
			specialKeymap := []byte("I01\t77\nI21\t46\n")
			if err := service.handleCurrentSaveGameOptionKeymap(session, buildCurrentGameOptionKeymapSaveBody(specialKeymap)); err != nil {
				t.Fatal(err)
			}
			storedAccount, found, err := loadCurrentGameOptionKeymap(ctx, repositories.Settings, "account-a")
			if err != nil || !found || !bytes.Equal(storedAccount, accountKeymap) {
				t.Fatalf("account keymap changed found=%t err=%v body=%q", found, err, storedAccount)
			}
			storedCharacter, found, err := loadCurrentGameOptionKeymapFromScope(
				ctx,
				repositories.Settings,
				currentCharacterGameOptionKeymapScope("19"),
			)
			if err != nil || !found || !bytes.Equal(storedCharacter, specialKeymap) {
				t.Fatalf("special keymap found=%t err=%v body=%q", found, err, storedCharacter)
			}
		})
	}
}

func TestCurrentGameOptionKeymapSaveUsesAccountScopeForDarkKnight(t *testing.T) {
	ctx := context.Background()
	repositories := testRepositoryGroup()
	character := dnfrepo.CharacterRecord{CharacterID: "19", AccountID: "account-a", Job: "9"}
	if err := repositories.Character.Save(ctx, character); err != nil {
		t.Fatal(err)
	}
	service := &Service{repositoryProvider: func() (dnfrepo.Group, bool) { return repositories, true }}
	session := &gameSession{accountID: "account-a", selectedCharacterID: 19}
	scope, source, err := service.currentGameOptionKeymapSaveScope(ctx, repositories, session, character.AccountID)
	if err != nil {
		t.Fatal(err)
	}
	if want := currentGameOptionScope(character.AccountID); scope != want || source != "account_settings" {
		t.Fatalf("dark knight keymap scope=%q source=%q, want scope=%q source=account_settings", scope, source, want)
	}
}

func TestCurrentGameOptionKeymapRejectsMalformedLengthWithoutReplacingState(t *testing.T) {
	ctx := context.Background()
	repositories := testRepositoryGroup()
	service := &Service{repositoryProvider: func() (dnfrepo.Group, bool) { return repositories, true }}
	session := &gameSession{accountID: "account-a"}
	keymap := buildCurrentGameOptionKeymapBody()
	if err := service.handleCurrentSaveGameOptionKeymap(session, buildCurrentGameOptionKeymapSaveBody(keymap)); err != nil {
		t.Fatal(err)
	}
	malformed := buildCurrentGameOptionKeymapSaveBody(append([]byte(nil), keymap...))
	binary.LittleEndian.PutUint32(malformed[:currentGameOptionSaveHeaderLen], uint32(len(keymap)-1))
	malformed[currentGameOptionSaveHeaderLen] ^= 0xff
	if err := service.handleCurrentSaveGameOptionKeymap(session, malformed); err != nil {
		t.Fatalf("malformed keymap save returned error: %v", err)
	}
	stored, found, err := loadCurrentGameOptionKeymap(ctx, repositories.Settings, "account-a")
	if err != nil || !found || !bytes.Equal(stored, keymap) {
		t.Fatalf("malformed keymap replaced state found=%t err=%v keymap_len=%d", found, err, len(stored))
	}
}

func TestCurrentGameOptionRepeatedSaveDoesNotInjectNativeApply(t *testing.T) {
	ctx := context.Background()
	repositories := dnfrepomemory.NewMemoryGroup()
	connection := &bufferConn{}
	service := &Service{
		options:            options{gameUpperHeader: gameUpperHeaderServer16, gameUpperBodyCodec: gameUpperBodyCodecPlain},
		repositoryProvider: func() (dnfrepo.Group, bool) { return repositories, true },
	}
	body := buildCurrentGameOptionSaveBody()
	if err := repositories.Character.Save(ctx, dnfrepo.CharacterRecord{
		CharacterID: "3",
		AccountID:   "account-a",
		Stats:       map[string]int64{currentGameOptionUserStateStat: int64(currentActorDefaultUserStateBits)},
	}); err != nil {
		t.Fatal(err)
	}
	session := &gameSession{conn: connection, accountID: "account-a", selectedCharacterID: 3}

	if err := service.handleCurrentSaveGameOption(session, body); err != nil {
		t.Fatalf("initial save: %v", err)
	}
	if err := service.handleCurrentSaveGameOption(session, body); err != nil {
		t.Fatalf("repeat save: %v", err)
	}
	if connection.write.Len() != 0 {
		t.Fatalf("repeat save injected a native apply packet: %x", connection.write.Bytes())
	}
	main, found, err := loadCurrentGameOption(ctx, repositories.Settings, "account-a")
	if err != nil || !found || !bytes.Equal(main, body[currentGameOptionSaveHeaderLen:]) {
		t.Fatalf("repeat save changed saved main found=%t err=%v body=%x", found, err, main)
	}
}

func TestCurrentGameOptionTransportUsesExactMainAndQuickChatVectors(t *testing.T) {
	main := buildCurrentGameOptionSaveBody()[currentGameOptionSaveHeaderLen:]
	quickChat0 := bytes.Repeat([]byte{0x11}, currentGameOptionQuickChatBodySize)
	quickChat1 := bytes.Repeat([]byte{0x22}, currentGameOptionQuickChatBodySize)
	transport, err := buildCurrentGameOptionTransportBody(main, quickChat0, quickChat1)
	if err != nil {
		t.Fatal(err)
	}
	quickChat0LengthOffset := currentGameOptionSaveHeaderLen + currentGameOptionMainBodySize
	quickChat0Offset := quickChat0LengthOffset + currentGameOptionSaveHeaderLen
	quickChat1LengthOffset := quickChat0Offset + currentGameOptionQuickChatBodySize
	quickChat1Offset := quickChat1LengthOffset + currentGameOptionSaveHeaderLen
	if len(transport) != quickChat1Offset+currentGameOptionQuickChatBodySize ||
		binary.LittleEndian.Uint32(transport[:4]) != currentGameOptionMainBodySize ||
		!bytes.Equal(transport[4:4+currentGameOptionMainBodySize], main) ||
		binary.LittleEndian.Uint32(transport[quickChat0LengthOffset:quickChat0Offset]) != currentGameOptionQuickChatBodySize ||
		!bytes.Equal(transport[quickChat0Offset:quickChat1LengthOffset], quickChat0) ||
		binary.LittleEndian.Uint32(transport[quickChat1LengthOffset:quickChat1Offset]) != currentGameOptionQuickChatBodySize ||
		!bytes.Equal(transport[quickChat1Offset:], quickChat1) {
		t.Fatalf("game option transport=%x", transport)
	}
}

func TestCurrentGameOptionTransportFillsMissingQuickChatWithFixedEmptySlots(t *testing.T) {
	main := buildCurrentGameOptionSaveBody()[currentGameOptionSaveHeaderLen:]
	transport, err := buildCurrentGameOptionTransportBody(main, nil, nil)
	if err != nil {
		t.Fatal(err)
	}
	if len(transport) != 3*currentGameOptionSaveHeaderLen+currentGameOptionMainBodySize+2*currentGameOptionQuickChatBodySize {
		t.Fatalf("game option transport len=%d", len(transport))
	}
	firstQuickChatLength := currentGameOptionSaveHeaderLen + currentGameOptionMainBodySize
	firstQuickChatBody := firstQuickChatLength + currentGameOptionSaveHeaderLen
	secondQuickChatLength := firstQuickChatBody + currentGameOptionQuickChatBodySize
	secondQuickChatBody := secondQuickChatLength + currentGameOptionSaveHeaderLen
	if binary.LittleEndian.Uint32(transport[firstQuickChatLength:firstQuickChatBody]) != currentGameOptionQuickChatBodySize ||
		binary.LittleEndian.Uint32(transport[secondQuickChatLength:secondQuickChatBody]) != currentGameOptionQuickChatBodySize ||
		!bytes.Equal(transport[firstQuickChatBody:secondQuickChatLength], make([]byte, currentGameOptionQuickChatBodySize)) ||
		!bytes.Equal(transport[secondQuickChatBody:], make([]byte, currentGameOptionQuickChatBodySize)) {
		t.Fatalf("game option empty quick chats=%x", transport)
	}
}

func TestCurrentGameOptionKeymapTransportUsesClass0Op454Layout(t *testing.T) {
	keymap := buildCurrentGameOptionKeymapBody()
	transport, err := buildCurrentGameOptionKeymapTransportBody(keymap)
	if err != nil {
		t.Fatal(err)
	}
	if len(transport) != 1+currentGameOptionSaveHeaderLen+len(keymap) ||
		transport[0] != currentGameOptionKeymapMode ||
		binary.LittleEndian.Uint32(transport[1:5]) != uint32(len(keymap)) ||
		!bytes.Equal(transport[5:], keymap) {
		t.Fatalf("game option keymap transport=%x", transport)
	}
}

func TestCurrentGameOptionLoginRapidFireKeymapExtractsOnlyI01(t *testing.T) {
	keymap := []byte("I00\t7\r\nI01\t2\r\nI02\t65\n")
	got, found := currentGameOptionLoginRapidFireKeymap(keymap)
	if !found || !bytes.Equal(got, []byte("I01\t2\n")) {
		t.Fatalf("rapid-fire keymap found=%t body=%q", found, got)
	}
	for _, malformed := range [][]byte{
		[]byte("I00\t7\nI02\t65\n"),
		[]byte("I01\tnot-a-number\n"),
		[]byte("I01\t2\textra\n"),
	} {
		if got, found := currentGameOptionLoginRapidFireKeymap(malformed); found || len(got) != 0 {
			t.Fatalf("malformed rapid-fire keymap found=%t body=%q input=%q", found, got, malformed)
		}
	}
}

func TestSavedRapidFireRelogRestoresSwitchBeforeI01AfterSuccessWithoutOp455(t *testing.T) {
	ctx := context.Background()
	repositories := testRepositoryGroup()
	keymap := []byte("I00\t7\nI01\t2\nI02\t65\n")
	if err := saveCurrentGameOptionKeymap(ctx, repositories.Settings, "account-a", keymap); err != nil {
		t.Fatal(err)
	}
	service := &Service{
		options: options{
			serverIP:           "192.168.1.10",
			gameUpperHeader:    gameUpperHeaderServer16,
			gameUpperBodyCodec: gameUpperBodyCodecPlain,
		},
		repositoryProvider: func() (dnfrepo.Group, bool) { return repositories, true },
	}
	savedMain := buildCurrentGameOptionSaveBody()
	setCurrentGameOptionRapidFire(savedMain, true)
	if err := service.handleCurrentSaveGameOption(
		&gameSession{accountID: "account-a"},
		savedMain,
	); err != nil {
		t.Fatalf("save rapid-fire option50: %v", err)
	}
	persistedMain, found, err := loadCurrentGameOption(ctx, repositories.Settings, "account-a")
	if err != nil || !found {
		t.Fatalf("load persisted rapid-fire main found=%t err=%v", found, err)
	}
	if enabled, valid := currentGameOptionRapidFireEnabled(persistedMain); !valid || !enabled {
		t.Fatalf("persisted rapid-fire option50 enabled=%t valid=%t", enabled, valid)
	}
	connection := &bufferConn{}
	session := &gameSession{
		conn:      connection,
		connID:    "login-rapid-fire",
		accountID: "account-a",
		channel:   channelcatalog.Channel{ID: 19, Type: 22, Name: "ch.19", Port: 10019},
	}
	if _, _, sent, err := service.sendGameInitial(session); err != nil || !sent {
		t.Fatalf("send initial sent=%t err=%v", sent, err)
	}
	loginPacket, rest := splitGameServerUpperPacketWithHeader(
		t,
		connection.write.Bytes(),
		dnfproto.GameServerUpperHeaderSize16,
	)
	rapidFireSwitchPacket, rest := splitGameServerUpperPacketWithHeader(
		t,
		rest,
		dnfproto.GameServerUpperHeaderSize16,
	)
	rapidFirePacket, rest := splitGameServerUpperPacketWithHeader(
		t,
		rest,
		dnfproto.GameServerUpperHeaderSize16,
	)
	if len(rest) != 0 {
		t.Fatalf("login rapid-fire trailing bytes=%x", rest)
	}
	if loginPacket.Header.Classification != dnfproto.DefaultChannelClassification ||
		loginPacket.Header.MsgID != uint16(dnfenum.UpperMsgGameEndpoint) {
		t.Fatalf("login packet header=%+v", loginPacket.Header)
	}
	wantMainBody, err := buildCurrentGameOptionTransportBody(persistedMain, nil, nil)
	if err != nil {
		t.Fatal(err)
	}
	if rapidFireSwitchPacket.Header.Classification != 0 ||
		rapidFireSwitchPacket.Header.MsgID != currentGameOptionMsgID ||
		!bytes.Equal(rapidFireSwitchPacket.Body, wantMainBody) {
		t.Fatalf("rapid-fire switch packet header=%+v body=%x want=%x", rapidFireSwitchPacket.Header, rapidFireSwitchPacket.Body, wantMainBody)
	}
	if enabled, valid := currentGameOptionRapidFireEnabled(
		rapidFireSwitchPacket.Body[currentGameOptionSaveHeaderLen : currentGameOptionSaveHeaderLen+currentGameOptionMainBodySize],
	); !valid || !enabled {
		t.Fatalf("wire rapid-fire option50 enabled=%t valid=%t", enabled, valid)
	}
	wantBody, err := buildCurrentGameOptionKeymapTransportBody([]byte("I01\t2\n"))
	if err != nil {
		t.Fatal(err)
	}
	if rapidFirePacket.Header.Classification != 0 ||
		rapidFirePacket.Header.MsgID != currentGameOptionKeymapMsgID ||
		!bytes.Equal(rapidFirePacket.Body, wantBody) {
		t.Fatalf("rapid-fire packet header=%+v body=%x want=%x", rapidFirePacket.Header, rapidFirePacket.Body, wantBody)
	}
	if rapidFirePacket.Header.MsgID == 455 || bytes.Contains(rapidFirePacket.Body, []byte("I02")) {
		t.Fatalf("login projection leaked op455/full keymap: header=%+v body=%q", rapidFirePacket.Header, rapidFirePacket.Body)
	}
}

func TestSavedDisabledRapidFireRelogRestoresMainWithoutProjectingI01(t *testing.T) {
	ctx := context.Background()
	repositories := testRepositoryGroup()
	main := buildCurrentGameOptionSaveBody()
	setCurrentGameOptionRapidFire(main, false)
	if err := saveCurrentGameOption(ctx, repositories.Settings, "account-a", main[currentGameOptionSaveHeaderLen:]); err != nil {
		t.Fatal(err)
	}
	if err := saveCurrentGameOptionKeymap(
		ctx,
		repositories.Settings,
		"account-a",
		[]byte("I00\t7\nI01\t2\nI02\t65\n"),
	); err != nil {
		t.Fatal(err)
	}
	connection := &bufferConn{}
	service := &Service{
		options: options{
			serverIP:           "192.168.1.10",
			gameUpperHeader:    gameUpperHeaderServer16,
			gameUpperBodyCodec: gameUpperBodyCodecPlain,
		},
		repositoryProvider: func() (dnfrepo.Group, bool) { return repositories, true },
	}
	session := &gameSession{
		conn:      connection,
		accountID: "account-a",
		channel:   channelcatalog.Channel{ID: 19, Type: 22, Name: "ch.19", Port: 10019},
	}
	if _, _, sent, err := service.sendGameInitial(session); err != nil || !sent {
		t.Fatalf("send initial sent=%t err=%v", sent, err)
	}
	loginPacket, rest := splitGameServerUpperPacketWithHeader(
		t,
		connection.write.Bytes(),
		dnfproto.GameServerUpperHeaderSize16,
	)
	mainPacket, rest := splitGameServerUpperPacketWithHeader(
		t,
		rest,
		dnfproto.GameServerUpperHeaderSize16,
	)
	if loginPacket.Header.MsgID != uint16(dnfenum.UpperMsgGameEndpoint) ||
		mainPacket.Header.Classification != 0 ||
		mainPacket.Header.MsgID != currentGameOptionMsgID ||
		len(rest) != 0 {
		t.Fatalf("disabled rapid fire login=%+v main=%+v trailing=%x", loginPacket.Header, mainPacket.Header, rest)
	}
	wantMain, err := buildCurrentGameOptionTransportBody(
		main[currentGameOptionSaveHeaderLen:],
		nil,
		nil,
	)
	if err != nil {
		t.Fatal(err)
	}
	if !bytes.Equal(mainPacket.Body, wantMain) {
		t.Fatalf("disabled rapid fire main=%x want=%x", mainPacket.Body, wantMain)
	}
}

func TestSavedOpaqueMainOptionsRelogRestoresEveryByteBeforeCharacterSelection(t *testing.T) {
	ctx := context.Background()
	repositories := testRepositoryGroup()
	main := buildCurrentGameOptionSaveBody()[currentGameOptionSaveHeaderLen:]
	// Lock/display/input switches share the fixed type197 table. Use several
	// otherwise-uninterpreted words to prove the login replay is byte-exact and
	// is not gated on the separately understood rapid-fire option 50.
	binary.LittleEndian.PutUint16(main[9*2:], 1)
	binary.LittleEndian.PutUint16(main[37*2:], 0x1234)
	binary.LittleEndian.PutUint16(main[81*2:], 1)
	binary.LittleEndian.PutUint16(main[currentGameOptionRapidFireOffset:], 0)
	if err := saveCurrentGameOption(ctx, repositories.Settings, "account-a", main); err != nil {
		t.Fatal(err)
	}
	connection := &bufferConn{}
	service := &Service{
		options: options{
			serverIP:           "192.168.1.10",
			gameUpperHeader:    gameUpperHeaderServer16,
			gameUpperBodyCodec: gameUpperBodyCodecPlain,
		},
		repositoryProvider: func() (dnfrepo.Group, bool) { return repositories, true },
	}
	session := &gameSession{
		conn:      connection,
		accountID: "account-a",
		channel:   channelcatalog.Channel{ID: 19, Type: 22, Name: "ch.19", Port: 10019},
	}
	if _, _, sent, err := service.sendGameInitial(session); err != nil || !sent {
		t.Fatalf("send initial sent=%t err=%v", sent, err)
	}
	loginPacket, rest := splitGameServerUpperPacketWithHeader(
		t,
		connection.write.Bytes(),
		dnfproto.GameServerUpperHeaderSize16,
	)
	mainPacket, rest := splitGameServerUpperPacketWithHeader(
		t,
		rest,
		dnfproto.GameServerUpperHeaderSize16,
	)
	if loginPacket.Header.MsgID != uint16(dnfenum.UpperMsgGameEndpoint) ||
		mainPacket.Header.Classification != 0 ||
		mainPacket.Header.MsgID != currentGameOptionMsgID ||
		len(rest) != 0 {
		t.Fatalf("login=%+v main=%+v trailing=%x", loginPacket.Header, mainPacket.Header, rest)
	}
	want, err := buildCurrentGameOptionTransportBody(main, nil, nil)
	if err != nil {
		t.Fatal(err)
	}
	if !bytes.Equal(mainPacket.Body, want) {
		t.Fatalf("login main=%x want=%x", mainPacket.Body, want)
	}
}

func TestCurrentGameOptionRestoreSendsMainBeforeSceneLayoutAndSeparateKeymapAfterHUDReadyOnce(t *testing.T) {
	ctx := context.Background()
	repositories := testRepositoryGroup()
	main := buildCurrentGameOptionSaveBody()[currentGameOptionSaveHeaderLen:]
	binary.LittleEndian.PutUint16(main[87*2:], 75)
	keymap := buildCurrentGameOptionKeymapBody()
	if err := saveCurrentGameOption(ctx, repositories.Settings, "account-a", main); err != nil {
		t.Fatal(err)
	}
	if err := saveCurrentGameOptionKeymap(ctx, repositories.Settings, "account-a", keymap); err != nil {
		t.Fatal(err)
	}
	mainTransport, err := buildCurrentGameOptionTransportBody(main, nil, nil)
	if err != nil {
		t.Fatal(err)
	}
	keymapTransport, err := buildCurrentGameOptionKeymapTransportBody(keymap)
	if err != nil {
		t.Fatal(err)
	}
	connection := &bufferConn{}
	service := &Service{
		options:            options{gameUpperHeader: gameUpperHeaderServer16, gameUpperBodyCodec: gameUpperBodyCodecPlain},
		repositoryProvider: func() (dnfrepo.Group, bool) { return repositories, true },
	}
	session := &gameSession{
		conn:                                connection,
		accountID:                           "account-a",
		selectedCharacterID:                 3,
		initialTownRouteCharacterID:         3,
		initialTownRouteStage:               currentInitialTownRoutePlayerStateSent,
		initialTownLegacySceneReadyAccepted: true,
		sceneBootstrapTailSent:              true,
		upperSeq:                            23,
	}
	if err := service.sendCurrentSceneLayoutCommit(session, "test_game_option_before_scene_layout"); err != nil {
		t.Fatal(err)
	}
	earlyWire := append([]byte(nil), connection.write.Bytes()...)
	mainPacket, rest := splitGameServerUpperPacketWithHeader(t, earlyWire, dnfproto.GameServerUpperHeaderSize16)
	layoutPacket, rest := splitGameServerUpperPacketWithHeader(t, rest, dnfproto.GameServerUpperHeaderSize16)
	if len(rest) != 0 {
		t.Fatalf("pre-layout game option trailing bytes=%x", rest)
	}
	if mainPacket.Header.Classification != 0 || mainPacket.Header.MsgID != currentGameOptionMsgID ||
		!bytes.Equal(mainPacket.Body, mainTransport) {
		t.Fatalf("pre-layout game option header=%+v body=%x", mainPacket.Header, mainPacket.Body)
	}
	if layoutPacket.Header.Classification != 0 ||
		layoutPacket.Header.MsgID != uint16(dnfenum.CmdPacketReportClientSpec) ||
		len(layoutPacket.Body) != 0 {
		t.Fatalf("scene layout packet header=%+v body=%x", layoutPacket.Header, layoutPacket.Body)
	}
	if session.currentGameOptionMainRestoreCharacterID != 3 {
		t.Fatalf("pre-layout main restore cursor=%d want=3", session.currentGameOptionMainRestoreCharacterID)
	}
	if err := service.sendCurrentGameOptionAfterLegacySceneReadyOnce(session, "test_game_option_restore"); err != nil {
		t.Fatal(err)
	}
	wire := append([]byte(nil), connection.write.Bytes()...)
	keymapPacket, rest := splitGameServerUpperPacketWithHeader(
		t,
		wire[len(earlyWire):],
		dnfproto.GameServerUpperHeaderSize16,
	)
	if len(rest) != 0 {
		t.Fatalf("game option restore trailing bytes=%x", rest)
	}
	if keymapPacket.Header.Classification != 0 || keymapPacket.Header.MsgID != currentGameOptionKeymapMsgID ||
		!bytes.Equal(keymapPacket.Body, keymapTransport) {
		t.Fatalf("game option keymap restore header=%+v body=%x", keymapPacket.Header, keymapPacket.Body)
	}
	if bytes.Contains(mainPacket.Body, keymap) {
		t.Fatalf("variable keymap leaked into fixed-size op173 body")
	}
	if !bytes.Equal(session.currentGameOptionNativeApplyBody, main) ||
		!bytes.Equal(session.currentGameOptionNativeApplyKeymapBody, keymap) {
		t.Fatalf("native apply snapshots main_len=%d keymap_len=%d", len(session.currentGameOptionNativeApplyBody), len(session.currentGameOptionNativeApplyKeymapBody))
	}
	if err := service.sendCurrentGameOptionAfterLegacySceneReadyOnce(session, "test_game_option_restore_repeat"); err != nil {
		t.Fatal(err)
	}
	if connection.write.Len() != len(wire) {
		t.Fatalf("repeat restore grew wire from %d to %d", len(wire), connection.write.Len())
	}
	nativeMainEcho := make([]byte, currentGameOptionSaveHeaderLen+len(main))
	binary.LittleEndian.PutUint32(nativeMainEcho[:currentGameOptionSaveHeaderLen], uint32(len(main)))
	copy(nativeMainEcho[currentGameOptionSaveHeaderLen:], main)
	if err := service.handleCurrentSaveGameOption(session, nativeMainEcho); err != nil {
		t.Fatalf("restore native apply echo: %v", err)
	}
	if connection.write.Len() != len(wire) {
		t.Fatalf("restore native apply echo appended packet: before=%d after=%d", len(wire), connection.write.Len())
	}
	if err := service.handleCurrentSaveGameOptionKeymap(session, buildCurrentGameOptionKeymapSaveBody(keymap)); err != nil {
		t.Fatalf("restore keymap native apply echo: %v", err)
	}
}

func TestCurrentGameOptionSceneLayoutRetryDoesNotReplayCommittedMain(t *testing.T) {
	ctx := context.Background()
	repositories := testRepositoryGroup()
	main := buildCurrentGameOptionSaveBody()[currentGameOptionSaveHeaderLen:]
	binary.LittleEndian.PutUint16(main[87*2:], 50)
	if err := saveCurrentGameOption(ctx, repositories.Settings, "account-a", main); err != nil {
		t.Fatal(err)
	}
	wantLayoutErr := errors.New("test op124 write failure")
	failing := &failNthDungeonWriteConn{failAt: 2, err: wantLayoutErr}
	service := &Service{
		options:            options{gameUpperHeader: gameUpperHeaderServer16, gameUpperBodyCodec: gameUpperBodyCodecPlain},
		repositoryProvider: func() (dnfrepo.Group, bool) { return repositories, true },
	}
	session := &gameSession{
		conn:                failing,
		accountID:           "account-a",
		selectedCharacterID: 3,
	}
	if err := service.sendCurrentSceneLayoutCommit(session, "test_layout_first_attempt"); !errors.Is(err, wantLayoutErr) {
		t.Fatalf("first scene layout err=%v want=%v", err, wantLayoutErr)
	}
	mainPacket, rest := splitGameServerUpperPacketWithHeader(
		t,
		failing.write.Bytes(),
		dnfproto.GameServerUpperHeaderSize16,
	)
	if len(rest) != 0 || mainPacket.Header.MsgID != currentGameOptionMsgID {
		t.Fatalf("failed layout committed stream header=%+v rest=%x", mainPacket.Header, rest)
	}
	if session.currentGameOptionMainRestoreCharacterID != 3 {
		t.Fatalf("successful main cursor=%d want=3", session.currentGameOptionMainRestoreCharacterID)
	}

	retry := &bufferConn{}
	session.conn = retry
	if err := service.sendCurrentSceneLayoutCommit(session, "test_layout_retry"); err != nil {
		t.Fatal(err)
	}
	layoutPacket, rest := splitGameServerUpperPacketWithHeader(
		t,
		retry.write.Bytes(),
		dnfproto.GameServerUpperHeaderSize16,
	)
	if len(rest) != 0 ||
		layoutPacket.Header.MsgID != uint16(dnfenum.CmdPacketReportClientSpec) ||
		len(layoutPacket.Body) != 0 {
		t.Fatalf("layout retry packet header=%+v body=%x rest=%x", layoutPacket.Header, layoutPacket.Body, rest)
	}
}

func TestCurrentGameOptionSceneLayoutRetriesMainWhenItsWriteDidNotCommit(t *testing.T) {
	ctx := context.Background()
	repositories := testRepositoryGroup()
	main := buildCurrentGameOptionSaveBody()[currentGameOptionSaveHeaderLen:]
	binary.LittleEndian.PutUint16(main[87*2:], 25)
	if err := saveCurrentGameOption(ctx, repositories.Settings, "account-a", main); err != nil {
		t.Fatal(err)
	}
	wantMainErr := errors.New("test op173 write failure")
	failing := &failNthDungeonWriteConn{failAt: 1, err: wantMainErr}
	service := &Service{
		options:            options{gameUpperHeader: gameUpperHeaderServer16, gameUpperBodyCodec: gameUpperBodyCodecPlain},
		repositoryProvider: func() (dnfrepo.Group, bool) { return repositories, true },
	}
	session := &gameSession{
		conn:                failing,
		accountID:           "account-a",
		selectedCharacterID: 3,
	}
	if err := service.sendCurrentSceneLayoutCommit(session, "test_main_first_attempt"); !errors.Is(err, wantMainErr) {
		t.Fatalf("first main restore err=%v want=%v", err, wantMainErr)
	}
	if failing.write.Len() != 0 || session.currentGameOptionMainRestoreCharacterID != 0 {
		t.Fatalf("failed main committed bytes=%x cursor=%d", failing.write.Bytes(), session.currentGameOptionMainRestoreCharacterID)
	}
	if len(session.currentGameOptionNativeApplyBody) != 0 || !session.currentGameOptionNativeApplyMainAt.IsZero() {
		t.Fatalf("failed main retained native echo snapshot len=%d at=%v", len(session.currentGameOptionNativeApplyBody), session.currentGameOptionNativeApplyMainAt)
	}

	retry := &bufferConn{}
	session.conn = retry
	if err := service.sendCurrentSceneLayoutCommit(session, "test_main_retry"); err != nil {
		t.Fatal(err)
	}
	mainPacket, rest := splitGameServerUpperPacketWithHeader(
		t,
		retry.write.Bytes(),
		dnfproto.GameServerUpperHeaderSize16,
	)
	layoutPacket, rest := splitGameServerUpperPacketWithHeader(
		t,
		rest,
		dnfproto.GameServerUpperHeaderSize16,
	)
	if len(rest) != 0 ||
		mainPacket.Header.MsgID != currentGameOptionMsgID ||
		layoutPacket.Header.MsgID != uint16(dnfenum.CmdPacketReportClientSpec) {
		t.Fatalf("main retry headers main=%+v layout=%+v rest=%x", mainPacket.Header, layoutPacket.Header, rest)
	}
}

func TestCurrentGameOptionQuickChatSavePersistsBothExactSlots(t *testing.T) {
	ctx := context.Background()
	repositories := dnfrepomemory.NewMemoryGroup()
	service := &Service{repositoryProvider: func() (dnfrepo.Group, bool) { return repositories, true }}
	session := &gameSession{accountID: "account-a", selectedCharacterID: 19}
	if err := repositories.Character.Save(ctx, dnfrepo.CharacterRecord{CharacterID: "19", AccountID: "account-a", Stats: map[string]int64{}}); err != nil {
		t.Fatal(err)
	}
	for slot := byte(0); slot < 2; slot++ {
		body := make([]byte, currentGameOptionQuickChatSaveBodySize)
		body[0] = slot
		binary.LittleEndian.PutUint32(body[1:1+currentGameOptionSaveHeaderLen], currentGameOptionQuickChatBodySize)
		copy(body[1+currentGameOptionSaveHeaderLen:], bytes.Repeat([]byte{slot + 1}, currentGameOptionQuickChatBodySize))
		if err := service.handleCurrentSaveGameOptionQuickChat(session, body); err != nil {
			t.Fatal(err)
		}
	}
	// The current EXE saves op198, both op368 slots, and only then op197.
	// The later main-option transaction must preserve both shortcut values.
	if err := service.handleCurrentSaveGameOption(session, buildCurrentGameOptionSaveBody()); err != nil {
		t.Fatal(err)
	}
	state, found, err := loadCurrentGameOptionState(ctx, repositories.Settings, "account-a")
	if err != nil || !found {
		t.Fatalf("quick chat state found=%t err=%v", found, err)
	}
	for slot := 0; slot < 2; slot++ {
		want := bytes.Repeat([]byte{byte(slot + 1)}, currentGameOptionQuickChatBodySize)
		if !bytes.Equal(state.quickChat[slot], want) {
			t.Fatalf("quick chat slot%d len=%d", slot, len(state.quickChat[slot]))
		}
	}
}

func TestCurrentGameOptionQuickChatRejectsMalformedBodyWithoutReplacingState(t *testing.T) {
	ctx := context.Background()
	repositories := testRepositoryGroup()
	service := &Service{repositoryProvider: func() (dnfrepo.Group, bool) { return repositories, true }}
	session := &gameSession{accountID: "account-a"}
	want := bytes.Repeat([]byte{0x5a}, currentGameOptionQuickChatBodySize)
	if err := saveCurrentGameOptionQuickChat(ctx, repositories.Settings, "account-a", 0, want); err != nil {
		t.Fatal(err)
	}
	for _, malformed := range [][]byte{
		{0},
		func() []byte {
			body := make([]byte, currentGameOptionQuickChatSaveBodySize)
			body[0] = 2
			binary.LittleEndian.PutUint32(body[1:1+currentGameOptionSaveHeaderLen], currentGameOptionQuickChatBodySize)
			return body
		}(),
		func() []byte {
			body := make([]byte, currentGameOptionQuickChatSaveBodySize-1)
			body[0] = 0
			binary.LittleEndian.PutUint32(body[1:1+currentGameOptionSaveHeaderLen], currentGameOptionQuickChatBodySize)
			return body
		}(),
		func() []byte {
			body := make([]byte, currentGameOptionQuickChatSaveBodySize)
			body[0] = 0
			binary.LittleEndian.PutUint32(body[1:1+currentGameOptionSaveHeaderLen], currentGameOptionQuickChatBodySize-1)
			return body
		}(),
	} {
		if err := service.handleCurrentSaveGameOptionQuickChat(session, malformed); err != nil {
			t.Fatalf("malformed quick chat returned error: %v", err)
		}
	}
	state, found, err := loadCurrentGameOptionState(ctx, repositories.Settings, "account-a")
	if err != nil || !found || !bytes.Equal(state.quickChat[0], want) {
		t.Fatalf("malformed quick chat replaced state found=%t err=%v len=%d", found, err, len(state.quickChat[0]))
	}
}

func TestCurrentGameOptionRestoreSkipsAccountsWithoutSavedState(t *testing.T) {
	repositories := testRepositoryGroup()
	connection := &bufferConn{}
	service := &Service{
		options:            options{gameUpperHeader: gameUpperHeaderServer16, gameUpperBodyCodec: gameUpperBodyCodecPlain},
		repositoryProvider: func() (dnfrepo.Group, bool) { return repositories, true },
	}
	session := &gameSession{
		conn:                                connection,
		accountID:                           "account-without-game-option",
		selectedCharacterID:                 3,
		initialTownRouteCharacterID:         3,
		initialTownRouteStage:               currentInitialTownRoutePlayerStateSent,
		initialTownLegacySceneReadyAccepted: true,
		sceneBootstrapTailSent:              true,
	}
	if err := service.sendCurrentGameOptionAfterLegacySceneReadyOnce(session, "test_missing_game_option"); err != nil {
		t.Fatal(err)
	}
	if connection.write.Len() != 0 {
		t.Fatalf("missing game option sent %x", connection.write.Bytes())
	}
	if session.initialTownGameOptionRestoreCharacterID != 3 {
		t.Fatalf("missing game option restore cursor=%d, want 3", session.initialTownGameOptionRestoreCharacterID)
	}
}

func TestGameOptionGameplayModuleOwnsGameOptionSaveOpcodes(t *testing.T) {
	for _, opcode := range []uint16{
		uint16(dnfenum.CmdPacketSaveGameOption1),
		uint16(dnfenum.CmdPacketSaveGameOption2),
		uint16(dnfenum.CmdPacketSaveGameOptionQuickchatting),
	} {
		route, ok := currentGameplayModules.legacyHandlers[opcode]
		if !ok || route.module != "game-option" {
			t.Fatalf("legacy op%d owner=%q found=%t", opcode, route.module, ok)
		}
	}
}
