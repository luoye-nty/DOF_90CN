package dnfbridge

import (
	"bytes"
	"context"
	"encoding/binary"
	"os"
	"path/filepath"
	"testing"

	"longheng.io/server/internal/modules/dnf/channelcatalog"
	dnfchat "longheng.io/server/internal/modules/dnf/chat"
	dnfrepo "longheng.io/server/internal/modules/dnf/repository"
	platformpvf "longheng.io/server/internal/platform/pvf"
)

func TestCurrentChannelMegaphoneAudienceIsLimitedToSpeakerChannel(t *testing.T) {
	newSession := func(character uint16, channelID int) *gameSession {
		return &gameSession{conn: &bufferConn{}, channel: channelcatalog.Channel{ID: channelID}, selectedCharacterID: character}
	}
	speaker := newSession(1, 42)
	sameChannel := newSession(2, 42)
	otherChannel := newSession(3, 77)
	service := &Service{gameSessions: map[uint16]*gameSession{
		1: speaker, 2: sameChannel, 3: otherChannel,
	}}

	audience := currentChatSessionsOnChannel(service.currentOnlineChatSessions(), speaker.channel.ID)
	found := make(map[*gameSession]struct{}, len(audience))
	for _, listener := range audience {
		found[listener] = struct{}{}
	}
	if len(audience) != 2 {
		t.Fatalf("channel audience size=%d want 2", len(audience))
	}
	if _, ok := found[speaker]; !ok {
		t.Fatal("the speaker must hear their own 频道喇叭")
	}
	if _, ok := found[sameChannel]; !ok {
		t.Fatal("a listener on the speaker's channel was dropped")
	}
	if _, ok := found[otherChannel]; ok {
		t.Fatal("a listener on another channel received the 频道喇叭")
	}

	// The passive robot projection must echo the mode it was given, otherwise a
	// robot 频道喇叭 would reach other clients labelled as a server megaphone.
	body := buildCurrentPassiveRobotMegaphoneNoticeBody(currentTownChatChannelMegaphoneMode, 42, []byte("robot"), []byte("hi"))
	if len(body) == 0 || body[0] != currentTownChatChannelMegaphoneMode {
		t.Fatalf("passive robot notice mode=%x", body)
	}
}

// TestRealPVFCurrentChannelMegaphoneSpendsItem37OnItsOwnChannel covers 频道喇叭
// end to end. The item id is not a guess: the shipped client picks the megaphone
// from the chat mode itself (`client/DNF.exe`, mode-11..60 jump table at
// 0x027295DE and its guard at 0x027175C7) and maps mode 13 to 0x25 = 37, which
// the runtime Script.pvf names 频道喇叭 at stackable/cash/megaphone_echo.stk.
func TestRealPVFCurrentChannelMegaphoneSpendsItem37OnItsOwnChannel(t *testing.T) {
	pvfPath := realChannelMegaphonePVFPath(t)
	service, source, _, sourceConn, targetConn := newCurrentSocialPeerTest(t)
	service.options.pvfPath = pvfPath
	// Warm the item catalog the way service startup does (preloadPVFItemCatalog).
	// The handler shares one createWriteTimeout budget between the catalog load
	// and the inventory debit, and loading the real archive alone takes seconds.
	if _, err := service.currentPVFItemCatalog(); err != nil {
		t.Fatal(err)
	}
	ctx := context.Background()
	repositories, ok := service.repositoryGroup()
	if !ok {
		t.Fatal("repository group unavailable")
	}
	if err := repositories.Character.Save(ctx, dnfrepo.CharacterRecord{
		CharacterID: "9", AccountID: "dnf:3", Name: "offchannel", Job: "0", Level: 90,
	}); err != nil {
		t.Fatal(err)
	}
	channelSlot := currentCeraShopInventorySlotKey(dnfrepo.MainInventoryListType, 65)
	serverSlot := currentCeraShopInventorySlotKey(dnfrepo.MainInventoryListType, 66)
	if err := repositories.Inventory.Save(ctx, dnfrepo.InventoryRecord{
		CharacterID: "1",
		Slots: map[string]dnfrepo.ItemStack{
			channelSlot: {ItemID: int64(dnfchat.ChannelMegaphoneItemID), Count: 2},
			serverSlot:  {ItemID: int64(dnfchat.ServerMegaphoneItemID), Count: 1},
		},
	}); err != nil {
		t.Fatal(err)
	}
	otherConn := &bufferConn{}
	other := &gameSession{conn: otherConn, channel: channelcatalog.Channel{ID: 77, Name: "ch.77"}, accountID: "dnf:3", selectedCharacterID: 9}
	service.bindGameSessionCharacter(other, 9)

	message := []byte("channel-horn")
	request := buildCurrentTownChatRequestForTest(currentTownChatChannelMegaphoneMode, message)
	binary.LittleEndian.PutUint32(request[3:7], 65)
	if err := service.handleCurrentTownChat(source, request); err != nil {
		t.Fatal(err)
	}

	want := buildCurrentPeerChatNoticeBody(currentTownChatChannelMegaphoneMode, byte(source.channel.ID), source.selectedCharacterID, rosterNameBytes("source1"), message)
	for label, connection := range map[string]*bufferConn{"speaker": sourceConn, "same_channel": targetConn} {
		packet, rest := splitGameServerUpperPacket(t, connection.write.Bytes())
		if packet.Header.Classification != 0 || packet.Header.MsgID != currentPeerChatNotifyOpcode ||
			!bytes.Equal(packet.Body, want) || len(rest) != 0 {
			t.Fatalf("%s header=%+v body=%x want=%x rest=%x", label, packet.Header, packet.Body, want, rest)
		}
	}
	if otherConn.write.Len() != 0 {
		t.Fatalf("频道喇叭 crossed channels: %x", otherConn.write.Bytes())
	}

	stored, found, err := repositories.Inventory.Load(ctx, "1")
	if err != nil || !found {
		t.Fatalf("load inventory found=%t err=%v", found, err)
	}
	if got := stored.Slots[channelSlot].Count; got != 1 {
		t.Fatalf("频道喇叭 count=%d want 1", got)
	}
	if got := stored.Slots[serverSlot].Count; got != 1 {
		t.Fatalf("服务器喇叭 was debited by the channel megaphone: count=%d", got)
	}

	// A slot that holds a 服务器喇叭 instead is refused with the captured code 4
	// and broadcasts nothing: mode 13 accepts item 37 only.
	sourceConn.write.Reset()
	targetConn.write.Reset()
	wrongSlot := buildCurrentTownChatRequestForTest(currentTownChatChannelMegaphoneMode, message)
	binary.LittleEndian.PutUint32(wrongSlot[3:7], 66)
	if err := service.handleCurrentTownChat(source, wrongSlot); err != nil {
		t.Fatal(err)
	}
	assertCurrentTownChatFailurePacket(t, sourceConn.write.Bytes())
	if targetConn.write.Len() != 0 {
		t.Fatalf("refused 频道喇叭 still broadcast: %x", targetConn.write.Bytes())
	}
}

func TestRealPVFChannelMegaphoneItemIdentityMatchesClientMapping(t *testing.T) {
	pvfPath := realChannelMegaphonePVFPath(t)
	archive, err := platformpvf.LoadArchive(platformpvf.Options{Path: pvfPath})
	if err != nil {
		t.Fatal(err)
	}
	catalog, err := newPVFDungeonDropCatalog(archive)
	if err != nil {
		t.Fatal(err)
	}
	definition, err := catalog.ResolveItem(dnfchat.ChannelMegaphoneItemID)
	if err != nil {
		t.Fatal(err)
	}
	if definition.Kind != dungeonDropItemStackable ||
		cleanDungeonDropPath(definition.PVFPath) != currentChannelMegaphonePVFPath ||
		normalizeDungeonDropStackableType(definition.StackableType) != "[waste]" {
		t.Fatalf("item 37 identity kind=%v path=%q type=%q", definition.Kind, definition.PVFPath, definition.StackableType)
	}
}

func realChannelMegaphonePVFPath(t *testing.T) string {
	t.Helper()
	if value := os.Getenv("DNFBRIDGE_REAL_PVF_SMOKE"); value != "" {
		return value
	}
	path := filepath.Clean(filepath.Join("..", "..", "..", "..", "runtime", "data", "dnf", "Script.pvf"))
	if _, err := os.Stat(path); err != nil {
		t.Skipf("runtime Script.pvf unavailable: %v", err)
	}
	return path
}
