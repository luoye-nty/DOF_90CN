package dnfbridge

import (
	"bytes"
	"context"
	"encoding/binary"
	"errors"
	"reflect"
	"strconv"
	"strings"
	"testing"
	"time"

	"longheng.io/server/internal/modules/dnf/channelcatalog"
	dnfenum "longheng.io/server/internal/modules/dnf/dnfenum"
	dnfdungeonpermission "longheng.io/server/internal/modules/dnf/dungeonpermission"
	"longheng.io/server/internal/modules/dnf/mail"
	"longheng.io/server/internal/modules/dnf/premium"
	dnfprogression "longheng.io/server/internal/modules/dnf/progression"
	dnfrepo "longheng.io/server/internal/modules/dnf/repository"
	dnfrepomemory "longheng.io/server/internal/modules/dnf/repository/memory"
)

func TestGMSystemMailUsesRepositoryPVFAndRefreshesOnlineMailbox(t *testing.T) {
	ctx := context.Background()
	repositories := dnfrepomemory.NewMemoryGroup()
	if err := repositories.Account.Save(ctx, dnfrepo.AccountRecord{
		AccountID: "dnf:test",
		Metadata:  map[string]string{"account_cera": "456"},
	}); err != nil {
		t.Fatal(err)
	}
	if err := repositories.Character.Save(ctx, dnfrepo.CharacterRecord{
		CharacterID: "88",
		AccountID:   "dnf:test",
		Name:        "Receiver",
		Level:       90,
		Stats:       map[string]int64{"gold": 123},
	}); err != nil {
		t.Fatal(err)
	}
	catalog, err := newPVFDungeonDropCatalog(dungeonDropCatalogTestSource{
		"monster/monster.lst":         "",
		"stackable/stackable.lst":     "3227 `material/test.stk`\n",
		"equipment/equipment.lst":     "5001 `weapon/test.equ`\n",
		"stackable/material/test.stk": "[name] `Test Material`\n[stackable type] `[material]`\n[stack limit] 999\n",
		"equipment/weapon/test.equ":   "[name] `Test Weapon`\n[equipment type] `[weapon]`\n[minimum level] 90\n[rarity] 4\n[durability] 45\n",
		currentAmplifyItemPVFPath:     testAmplifyItemConfigText(),
	})
	if err != nil {
		t.Fatal(err)
	}
	connection := &bufferConn{}
	recipient := &gameSession{
		conn:                connection,
		connID:              "gm-mail-online-recipient",
		channel:             channelcatalog.Channel{ID: 19},
		selectedCharacterID: 88,
	}
	service := &Service{
		options: options{
			accountID:          "dnf:test",
			gameUpperHeader:    gameUpperHeaderServer16,
			gameUpperBodyCodec: gameUpperBodyCodecPlain,
		},
		repositoryProvider: func() (dnfrepo.Group, bool) { return repositories, true },
		gameSessions:       map[uint16]*gameSession{88: recipient},
		pvfItemCatalog:     catalog,
	}

	characters, err := service.GMCharacters(ctx)
	if err != nil {
		t.Fatalf("GMCharacters() error = %v", err)
	}
	if len(characters) != 1 || characters[0].CharacterID != "88" || characters[0].Gold != 123 ||
		characters[0].Cera != 456 || !characters[0].Online {
		t.Fatalf("GMCharacters() = %+v", characters)
	}
	result, err := service.GMSendSystemMail(ctx, GMSystemMailRequest{
		CharacterID: "88",
		Title:       "GM reward",
		Body:        "Native service mail",
		Gold:        1000,
		Attachments: []GMSystemMailAttachment{
			{ItemID: 3227, Count: 5, Bind: true},
			{ItemID: 5001, Count: 1, AmplifyLevel: 31, AmplifyType: 1},
		},
	})
	if err != nil {
		t.Fatalf("GMSendSystemMail() error = %v", err)
	}
	if result.MailID != "1" || result.RecipientCharacterID != "88" || !result.Online || !result.Refreshed || result.RefreshError != "" {
		t.Fatalf("GMSendSystemMail() = %+v", result)
	}
	mailbox, found, err := repositories.Mailbox.Load(ctx, "88")
	if err != nil || !found {
		t.Fatalf("load mailbox found=%t error=%v", found, err)
	}
	got := mailbox.Mails["1"]
	if got.Gold != 1000 || len(got.Attachments) != 2 || got.Attachments[0].ItemID != 3227 ||
		got.Attachments[0].Count != 5 || !got.Attachments[0].Bind || len(got.Attachments[0].RawEntry) != currentItemListEntryWireSize ||
		got.Attachments[0].Extra["source"] != "gm_admin_system_mail" {
		t.Fatalf("durable GM mail = %+v", got)
	}
	equipment := got.Attachments[1]
	if equipment.ItemID != 5001 || equipment.Count != 1 || len(equipment.RawEntry) != currentItemListEntryWireSize || equipment.RawEntry[0x0a]&0x1f != 31 ||
		equipment.RawEntry[0x13] != 1 || binary.LittleEndian.Uint16(equipment.RawEntry[0x14:0x16]) != 7 {
		t.Fatalf("durable upgraded equipment attachment = %+v", equipment)
	}
	packets := splitAllCurrentUpperPackets(t, connection.write.Bytes())
	if len(packets) != 1 || packets[0].Header.MsgID != 0x0063 || packets[0].Header.Classification != 0 {
		t.Fatalf("live refresh packets = %+v", packets)
	}
}

func TestGMSystemMailRareAvatarUsesNativeListOneRawRow(t *testing.T) {
	ctx := context.Background()
	repositories := dnfrepomemory.NewMemoryGroup()
	if err := repositories.Account.Save(ctx, dnfrepo.AccountRecord{AccountID: "dnf:test"}); err != nil {
		t.Fatal(err)
	}
	if err := repositories.Character.Save(ctx, dnfrepo.CharacterRecord{
		CharacterID: "88", AccountID: "dnf:test", Name: "Receiver", Level: 90,
	}); err != nil {
		t.Fatal(err)
	}
	catalog, err := newPVFDungeonDropCatalog(dungeonDropCatalogTestSource{
		"monster/monster.lst":            "",
		"stackable/stackable.lst":        "",
		"equipment/equipment.lst":        "6101 `avatar/rare_coat.equ`\n",
		"equipment/avatar/rare_coat.equ": "[name] `Rare Avatar Coat`\n[equipment type] `[coat avatar]`\n[rarity] 4\n[minimum level] 1\n",
	})
	if err != nil {
		t.Fatal(err)
	}
	connection := &bufferConn{}
	service := &Service{
		options: options{
			accountID:          "dnf:test",
			gameUpperHeader:    gameUpperHeaderServer16,
			gameUpperBodyCodec: gameUpperBodyCodecPlain,
		},
		repositoryProvider: func() (dnfrepo.Group, bool) { return repositories, true },
		pvfItemCatalog:     catalog,
		gameSessions: map[uint16]*gameSession{88: {
			conn: connection, connID: "gm-rare-avatar-mail", selectedCharacterID: 88,
		}},
	}
	result, err := service.GMSendSystemMail(ctx, GMSystemMailRequest{
		CharacterID: "88",
		Title:       "Rare avatar",
		Attachments: []GMSystemMailAttachment{{ItemID: 6101, Count: 1}},
	})
	if err != nil {
		t.Fatal(err)
	}
	if result.MailID != "1" || !result.Online || !result.Refreshed || result.RefreshError != "" {
		t.Fatalf("GM mail result=%+v", result)
	}
	mailbox, found, err := repositories.Mailbox.Load(ctx, "88")
	if err != nil || !found {
		t.Fatalf("load mailbox found=%t err=%v", found, err)
	}
	attachment := mailbox.Mails["1"].Attachments[0]
	if len(attachment.RawEntry) != currentItemListEntryWireSize ||
		binary.LittleEndian.Uint16(attachment.RawEntry[0:2]) != 0 ||
		binary.LittleEndian.Uint32(attachment.RawEntry[2:6]) != 6101 ||
		binary.LittleEndian.Uint32(attachment.RawEntry[6:10]) != 1 {
		t.Fatalf("rare avatar mail raw=%x extra=%+v", attachment.RawEntry, attachment.Extra)
	}
	definition, err := catalog.ResolveItem(6101)
	if err != nil {
		t.Fatal(err)
	}
	resolution, err := mailboxItemResolutionFromPVF(definition, false)
	if err != nil || resolution.ListType != currentAvatarInventoryListType {
		t.Fatalf("rare avatar resolution=%+v err=%v", resolution, err)
	}
	packets := splitAllCurrentUpperPackets(t, connection.write.Bytes())
	if len(packets) != 1 || packets[0].Header.MsgID != mail.MailboxAlarmNotificationMessageID ||
		packets[0].Header.Classification != 0 {
		t.Fatalf("rare avatar arrival packets=%+v; unsolicited op97 is unsafe", packets)
	}
	opened, err := mail.NewOwner(repositories).Open(ctx, "88")
	if err != nil {
		t.Fatal(err)
	}
	snapshot, err := mail.BuildListNotification(opened)
	if err != nil || len(snapshot) == 0 {
		t.Fatalf("client-owned rare avatar mailbox snapshot len=%d err=%v", len(snapshot), err)
	}
}

func TestGMClearMailboxRequiresConfirmationAndRefreshesOnlineClient(t *testing.T) {
	ctx := context.Background()
	repositories := dnfrepomemory.NewMemoryGroup()
	if err := repositories.Character.Save(ctx, dnfrepo.CharacterRecord{
		CharacterID: "88",
		AccountID:   "dnf:test",
		Name:        "Receiver",
		Level:       90,
	}); err != nil {
		t.Fatal(err)
	}
	now := time.Now().UTC()
	if err := repositories.Mailbox.Save(ctx, dnfrepo.MailboxRecord{
		CharacterID: "88",
		Mails: map[string]dnfrepo.MailRecord{
			"1": {
				MailID:      "1",
				Gold:        300,
				Attachments: []dnfrepo.MailAttachment{{ItemID: 3227, Count: 2}},
				CreatedAt:   now,
			},
			"2": {MailID: "2", Read: true, Claimed: true, CreatedAt: now},
		},
		UpdatedAt: now,
	}); err != nil {
		t.Fatal(err)
	}
	connection := &bufferConn{}
	recipient := &gameSession{
		conn:                connection,
		connID:              "gm-mail-clear-online-recipient",
		channel:             channelcatalog.Channel{ID: 19},
		selectedCharacterID: 88,
	}
	service := &Service{
		options: options{
			accountID:          "dnf:test",
			gameUpperHeader:    gameUpperHeaderServer16,
			gameUpperBodyCodec: gameUpperBodyCodecPlain,
		},
		repositoryProvider: func() (dnfrepo.Group, bool) { return repositories, true },
		gameSessions:       map[uint16]*gameSession{88: recipient},
	}

	if _, err := service.GMClearMailbox(ctx, GMMailboxClearRequest{CharacterName: "Receiver"}); !errors.Is(err, ErrGMRequestInvalid) {
		t.Fatalf("GMClearMailbox() without confirmation error = %v", err)
	}
	before, found, err := repositories.Mailbox.Load(ctx, "88")
	if err != nil || !found || len(before.Mails) != 2 {
		t.Fatalf("mailbox before confirmed clear found=%t error=%v record=%+v", found, err, before)
	}

	result, err := service.GMClearMailbox(ctx, GMMailboxClearRequest{CharacterName: "Receiver", Confirm: true})
	if err != nil {
		t.Fatalf("GMClearMailbox() error = %v", err)
	}
	if result.CharacterID != "88" || result.CharacterName != "Receiver" || result.RemovedMails != 2 ||
		result.RemovedUnread != 1 || result.RemovedClaimable != 1 || result.RemovedGold != 300 ||
		result.RemovedAttachments != 1 || !result.Online || !result.Refreshed || result.RefreshError != "" {
		t.Fatalf("GMClearMailbox() = %+v", result)
	}
	after, found, err := repositories.Mailbox.Load(ctx, "88")
	if err != nil || !found || len(after.Mails) != 0 {
		t.Fatalf("mailbox after clear found=%t error=%v record=%+v", found, err, after)
	}
	packets := splitAllCurrentUpperPackets(t, connection.write.Bytes())
	if len(packets) != 2 || packets[0].Header.MsgID != 0x0063 || packets[0].Header.Classification != 0 ||
		packets[1].Header.MsgID != 0x0061 || packets[1].Header.Classification != 0 {
		t.Fatalf("mailbox clear refresh packets = %+v", packets)
	}
}

func TestGMSystemMailOfflineRecipientCommitsWithoutTransport(t *testing.T) {
	ctx := context.Background()
	repositories := dnfrepomemory.NewMemoryGroup()
	if err := repositories.Character.Save(ctx, dnfrepo.CharacterRecord{
		CharacterID: "9",
		AccountID:   "dnf:test",
		Name:        "Offline",
	}); err != nil {
		t.Fatal(err)
	}
	service := &Service{
		options:            options{accountID: "dnf:test"},
		repositoryProvider: func() (dnfrepo.Group, bool) { return repositories, true },
		gameSessions:       make(map[uint16]*gameSession),
	}
	result, err := service.GMSendSystemMail(ctx, GMSystemMailRequest{
		CharacterName: "Offline",
		Body:          "Text-only system mail",
	})
	if err != nil {
		t.Fatalf("GMSendSystemMail() error = %v", err)
	}
	if result.MailID != "1" || result.Online || result.Refreshed {
		t.Fatalf("offline result = %+v", result)
	}
	mailbox, found, err := repositories.Mailbox.Load(ctx, "9")
	if err != nil || !found || mailbox.Mails["1"].Body != "Text-only system mail" {
		t.Fatalf("offline mailbox found=%t error=%v record=%+v", found, err, mailbox)
	}
}

func TestGMFindCharactersQueriesAccountOrExactCharacterName(t *testing.T) {
	ctx := context.Background()
	repositories := dnfrepomemory.NewMemoryGroup()
	for _, account := range []dnfrepo.AccountRecord{
		{AccountID: "dnf:one", Metadata: map[string]string{"account_cera": "11", "login_username": "account-one"}},
		{AccountID: "dnf:two", Metadata: map[string]string{"account_cera": "22", "login_username": "account-two"}},
	} {
		if err := repositories.Account.Save(ctx, account); err != nil {
			t.Fatal(err)
		}
	}
	for _, character := range []dnfrepo.CharacterRecord{
		{CharacterID: "1", AccountID: "dnf:one", Name: "Alpha", Level: 10, Stats: map[string]int64{"gold": 100}},
		{CharacterID: "2", AccountID: "dnf:two", Name: "Beta", Level: 20, Stats: map[string]int64{"gold": 200}},
	} {
		if err := repositories.Character.Save(ctx, character); err != nil {
			t.Fatal(err)
		}
	}
	service := &Service{
		options:            options{accountID: "dnf:one"},
		repositoryProvider: func() (dnfrepo.Group, bool) { return repositories, true },
		gameSessions:       make(map[uint16]*gameSession),
	}
	byAccount, err := service.GMFindCharacters(ctx, "dnf:two", "")
	if err != nil || len(byAccount) != 1 || byAccount[0].Name != "Beta" || byAccount[0].Cera != 22 {
		t.Fatalf("GMFindCharacters(account) = %+v error=%v", byAccount, err)
	}
	byName, err := service.GMFindCharacters(ctx, "", "Alpha")
	if err != nil || len(byName) != 1 || byName[0].AccountID != "dnf:one" || byName[0].Gold != 100 {
		t.Fatalf("GMFindCharacters(name) = %+v error=%v", byName, err)
	}
	missing, err := service.GMFindCharacters(ctx, "", "Missing")
	if err != nil || len(missing) != 0 {
		t.Fatalf("GMFindCharacters(missing) = %+v error=%v", missing, err)
	}
	if _, err := service.GMFindCharacters(ctx, "dnf:one", "Alpha"); !errors.Is(err, ErrGMRequestInvalid) {
		t.Fatalf("GMFindCharacters accepted two selectors: %v", err)
	}
	byUsername, err := service.GMFindCharactersByUsername(ctx, "ACCOUNT-TWO")
	if err != nil || len(byUsername) != 1 || byUsername[0].Name != "Beta" {
		t.Fatalf("GMFindCharactersByUsername() = %+v error=%v", byUsername, err)
	}
	wallet, err := service.GMReadWallet(ctx, GMWalletRequest{CharacterName: "Beta"})
	if err != nil || wallet.Character.AccountID != "dnf:two" || wallet.Cera != 22 || wallet.Gold != 200 {
		t.Fatalf("GMReadWallet(cross-account) = %+v error=%v", wallet, err)
	}
	updated, err := service.GMUpdateWallet(ctx, GMWalletRequest{
		CharacterID: "2", Currency: "cera", Mode: "add", Amount: 5,
	})
	if err != nil || updated.Before != 22 || updated.After != 27 || updated.Character.AccountID != "dnf:two" {
		t.Fatalf("GMUpdateWallet(cross-account) = %+v error=%v", updated, err)
	}
}

func TestGMUpdateCharacterSkillCanPersistentlyExtendTotalsAcrossTrees(t *testing.T) {
	ctx := context.Background()
	repositories := dnfrepomemory.NewMemoryGroup()
	if err := repositories.Character.Save(ctx, dnfrepo.CharacterRecord{
		CharacterID: "88", AccountID: "dnf:test", Name: "Receiver", Job: "2", Level: 30,
		Stats: map[string]int64{"skill_tree_index": 0, "bonus_sp": 5, "bonus_tp": 2},
	}); err != nil {
		t.Fatal(err)
	}
	if err := repositories.Skill.Save(ctx, dnfrepo.SkillRecord{
		CharacterID: "88",
		Skills:      map[int64]dnfrepo.SkillState{1: {Level: 1, Enabled: true}},
		Points:      dnfrepo.SkillPointState{TotalSP: 600, RemainingSP: 100, TotalTP: 10, RemainingTP: 2, SyncedLevel: 30},
		Trees: map[int]dnfrepo.SkillTreeState{1: {
			Skills: map[int64]dnfrepo.SkillState{1: {Level: 1, Enabled: true}},
			Points: dnfrepo.SkillPointState{TotalSP: 600, RemainingSP: 200, TotalTP: 10, RemainingTP: 3, SyncedLevel: 30},
		}},
	}); err != nil {
		t.Fatal(err)
	}
	service := &Service{
		options:            options{accountID: "dnf:test"},
		repositoryProvider: func() (dnfrepo.Group, bool) { return repositories, true },
	}

	sp, tp := 1000, 50
	if _, err := service.GMUpdateCharacterSkill(ctx, GMCharacterSkillUpdate{
		CharacterID: "88", RemainingSP: &sp, RemainingTP: &tp,
	}); !errors.Is(err, ErrGMRequestInvalid) || !strings.Contains(err.Error(), "extend_total") {
		t.Fatalf("GMUpdateCharacterSkill() without extension error=%v", err)
	}
	before, _, _ := repositories.Skill.Load(ctx, "88")
	if before.Points.TotalSP != 600 || before.Points.RemainingSP != 100 || before.Trees[1].Points.TotalSP != 600 {
		t.Fatalf("rejected GM extension mutated skill=%+v", before)
	}

	result, err := service.GMUpdateCharacterSkill(ctx, GMCharacterSkillUpdate{
		CharacterID: "88", RemainingSP: &sp, RemainingTP: &tp, ExtendTotal: true,
	})
	if err != nil {
		t.Fatal(err)
	}
	if result.ActiveTree != 0 || result.TotalSP != 1000 || result.RemainingSP != 1000 || result.AddedSP != 400 ||
		result.TotalTP != 50 || result.RemainingTP != 50 || result.AddedTP != 40 || result.Online || result.Refreshed {
		t.Fatalf("GMUpdateCharacterSkill() result=%+v", result)
	}
	character, _, _ := repositories.Character.Load(ctx, "88")
	skill, _, _ := repositories.Skill.Load(ctx, "88")
	if character.Stats["bonus_sp"] != 405 || character.Stats["bonus_tp"] != 42 ||
		skill.Points.TotalSP != 1000 || skill.Points.RemainingSP != 1000 || skill.Points.TotalTP != 50 || skill.Points.RemainingTP != 50 ||
		skill.Trees[1].Points.TotalSP != 1000 || skill.Trees[1].Points.RemainingSP != 600 ||
		skill.Trees[1].Points.TotalTP != 50 || skill.Trees[1].Points.RemainingTP != 43 {
		t.Fatalf("persistent GM SP/TP extension character=%+v skill=%+v", character, skill)
	}
	info, err := service.GMReadCharacterSkill(ctx, "88")
	if err != nil || info.TotalSP != 1000 || info.RemainingSP != 1000 || info.TotalTP != 50 || info.RemainingTP != 50 {
		t.Fatalf("GMReadCharacterSkill()=%+v error=%v", info, err)
	}

	overflow := 65536
	if _, err := service.GMUpdateCharacterSkill(ctx, GMCharacterSkillUpdate{
		CharacterID: "88", RemainingSP: &overflow, ExtendTotal: true,
	}); !errors.Is(err, ErrGMRequestInvalid) || !strings.Contains(err.Error(), "0..65535") {
		t.Fatalf("GMUpdateCharacterSkill() overflow error=%v", err)
	}
}

func TestGMNativeStateOperationsUseRepositoryTransactions(t *testing.T) {
	ctx := context.Background()
	repositories := dnfrepomemory.NewMemoryGroup()
	if err := repositories.Account.Save(ctx, dnfrepo.AccountRecord{
		AccountID: "dnf:test",
		Metadata:  map[string]string{"account_cera": "100"},
	}); err != nil {
		t.Fatal(err)
	}
	if err := repositories.Character.Save(ctx, dnfrepo.CharacterRecord{
		CharacterID: "88", AccountID: "dnf:test", Name: "Receiver", Job: "2", Level: 30,
		Stats: map[string]int64{"gold": 200, "grow_type": 0},
	}); err != nil {
		t.Fatal(err)
	}
	if err := repositories.Skill.Save(ctx, dnfrepo.SkillRecord{
		CharacterID: "88",
		Skills:      map[int64]dnfrepo.SkillState{1: {Level: 1, Enabled: true}},
		Points:      dnfrepo.SkillPointState{TotalSP: 600, RemainingSP: 100, SyncedLevel: 30},
	}); err != nil {
		t.Fatal(err)
	}
	profiles, skillCatalog := finishBridgeProfessionResources(t)
	spTable := make(map[int]int, 90)
	tpTable := make(map[int]int, 90)
	for level := 1; level <= 90; level++ {
		spTable[level] = 20
		if level >= 50 {
			tpTable[level] = 1
		}
	}
	connection := &bufferConn{}
	experienceSource := bridgePVFSource{
		dnfprogression.ExperienceTablePath: strings.TrimSpace(strings.Repeat("100 ", 89)),
	}
	experienceTable, err := dnfprogression.LoadExperienceTable(ctx, experienceSource)
	if err != nil {
		t.Fatal(err)
	}
	service := &Service{
		options:            options{accountID: "dnf:test", gameUpperHeader: gameUpperHeaderServer16, gameUpperBodyCodec: gameUpperBodyCodecPlain},
		repositoryProvider: func() (dnfrepo.Group, bool) { return repositories, true },
		initialSkillsByJob: map[byte][]initialSkillEntry{2: {{SkillID: 1, Level: 1}}},
		initialSPTable:     spTable,
		initialTPTable:     tpTable,
		experienceTable:    experienceTable,
		skillCatalog:       skillCatalog,
		professionProfiles: profiles,
		questCatalog:       finishBridgeProfessionCatalog(t),
		gameSessions: map[uint16]*gameSession{88: {
			conn: connection, connID: "gm-state-online", selectedCharacterID: 88,
		}},
	}

	cera, err := service.GMUpdateWallet(ctx, GMWalletRequest{CharacterID: "88", Currency: "cera", Mode: "add", Amount: 50})
	if err != nil || cera.Before != 100 || cera.After != 150 || cera.Cera != 150 || !cera.Refreshed {
		t.Fatalf("GMUpdateWallet(cera) = %+v error=%v", cera, err)
	}
	gold, err := service.GMUpdateWallet(ctx, GMWalletRequest{CharacterName: "Receiver", Currency: "gold", Mode: "set", Amount: 999})
	if err != nil || gold.Before != 200 || gold.After != 999 || gold.Gold != 999 || !gold.Refreshed {
		t.Fatalf("GMUpdateWallet(gold) = %+v error=%v", gold, err)
	}
	packets := splitAllCurrentUpperPackets(t, connection.write.Bytes())
	if len(packets) != 2 || packets[0].Header.MsgID != currentCeraShopUpdateMsgID ||
		packets[1].Header.MsgID != uint16(dnfenum.CmdPacketWalkoutPartyMember) {
		t.Fatalf("wallet refresh packets = %+v", packets)
	}

	sp, tp := 500, 0
	connection.write.Reset()
	skillUpdate, err := service.GMUpdateCharacterSkill(ctx, GMCharacterSkillUpdate{CharacterID: "88", RemainingSP: &sp, RemainingTP: &tp})
	if err != nil || !skillUpdate.Refreshed {
		t.Fatal(err)
	}
	skillPackets := splitAllCurrentUpperPackets(t, connection.write.Bytes())
	if len(skillPackets) != 1 || skillPackets[0].Header.Classification != 0 || skillPackets[0].Header.MsgID != currentSkillInfoMsgID {
		t.Fatalf("GM SP/TP native packets = %+v", skillPackets)
	}
	skillInfo, err := service.GMReadCharacterSkill(ctx, "88")
	if err != nil || skillInfo.RemainingSP != sp || skillInfo.RemainingTP != tp {
		t.Fatalf("GMReadCharacterSkill() = %+v error=%v", skillInfo, err)
	}
	connection.write.Reset()
	levelUpdate, err := service.GMSetCharacterLevel(ctx, "88", 90)
	if err != nil || !levelUpdate.Refreshed || levelUpdate.PreviousLevel != 30 || levelUpdate.Level != 90 {
		t.Fatalf("GMSetCharacterLevel() = %+v error=%v", levelUpdate, err)
	}
	levelPackets := splitAllCurrentUpperPackets(t, connection.write.Bytes())
	levelWant := []uint16{currentDungeonCharacterStateMsgID, currentSkillInfoMsgID, currentActiveQuestSnapshotMsgID, currentClearQuestListMsgID, currentAcceptableQuestListMsgID}
	if len(levelPackets) != len(levelWant) {
		t.Fatalf("GM level packet count=%d packets=%+v", len(levelPackets), levelPackets)
	}
	for index, want := range levelWant {
		if levelPackets[index].Header.MsgID != want {
			t.Fatalf("GM level packet[%d]=%d want=%d", index, levelPackets[index].Header.MsgID, want)
		}
	}
	connection.write.Reset()
	jobUpdate, err := service.GMSetCharacterJob(ctx, GMCharacterJobUpdate{CharacterID: "88", Job: 2, FirstGrow: 2, SecondGrow: 0})
	if err != nil || !jobUpdate.Refreshed || jobUpdate.TransitionCount != 1 {
		t.Fatalf("GMSetCharacterJob() = %+v error=%v", jobUpdate, err)
	}
	jobPackets := splitAllCurrentUpperPackets(t, connection.write.Bytes())
	jobWant := []uint16{uint16(dnfenum.CmdPacketSetUDPIPPort), uint16(dnfenum.CmdPacketSetUDPIPPort), currentSkillInfoMsgID, currentActiveQuestSnapshotMsgID, currentClearQuestListMsgID, currentAcceptableQuestListMsgID}
	if len(jobPackets) != len(jobWant) {
		t.Fatalf("GM profession packet count=%d packets=%+v", len(jobPackets), jobPackets)
	}
	for index, want := range jobWant {
		if jobPackets[index].Header.MsgID != want {
			t.Fatalf("GM profession packet[%d]=%d want=%d", index, jobPackets[index].Header.MsgID, want)
		}
	}
	job, err := service.GMReadCharacterJob(ctx, "88")
	if err != nil || job.Job != 2 || job.FirstGrow != 2 || job.SecondGrow != 0 {
		t.Fatalf("GMReadCharacterJob() = %+v error=%v", job, err)
	}
	character, _, _ := repositories.Character.Load(ctx, "88")
	if character.Level != 90 || character.Stats["exp"] != 100 {
		t.Fatalf("level = %d cumulative_exp=%d, want level=90 PVF entry exp=100", character.Level, character.Stats["exp"])
	}
	character.Stats["exp"] = 0
	if err := repositories.Character.Save(ctx, character); err != nil {
		t.Fatal(err)
	}
	connection.write.Reset()
	repaired, err := service.GMSetCharacterLevel(ctx, "88", 90)
	if err != nil || !repaired.Refreshed || repaired.PreviousLevel != 90 {
		t.Fatalf("same-level GM repair = %+v error=%v", repaired, err)
	}
	character, _, _ = repositories.Character.Load(ctx, "88")
	if character.Stats["exp"] != 100 {
		t.Fatalf("same-level GM repair cumulative_exp=%d, want PVF entry exp=100", character.Stats["exp"])
	}
	skill, _, _ := repositories.Skill.Load(ctx, "88")
	if skill.Skills[2].Level != 1 || skill.Skills[197].Level != 1 || skill.Points.RemainingSP != skill.Points.TotalSP || skill.Points.RemainingTP != skill.Points.TotalTP {
		t.Fatalf("PVF profession skill transition = %+v", skill)
	}
	// A same-branch forward awakening remains the ordinary additive path: it
	// preserves learned levels and spent points while adding the PVF awakening
	// grant. This must stay separate from the administrative rebase below.
	skill.Skills[2] = dnfrepo.SkillState{Level: 7, Enabled: true}
	skill.Points.RemainingSP = 1
	if err := repositories.Skill.Save(ctx, skill); err != nil {
		t.Fatal(err)
	}
	connection.write.Reset()
	awakeningUpdate, err := service.GMSetCharacterJob(ctx, GMCharacterJobUpdate{CharacterID: "88", Job: 2, FirstGrow: 2, SecondGrow: 1})
	if err != nil || !awakeningUpdate.Refreshed || awakeningUpdate.PreviousGrowType != 2 || awakeningUpdate.GrowType != 0x12 || awakeningUpdate.TransitionCount != 1 {
		t.Fatalf("GM forward awakening = %+v error=%v", awakeningUpdate, err)
	}
	skill, _, _ = repositories.Skill.Load(ctx, "88")
	if skill.Skills[2].Level != 7 || skill.Skills[4].Level != 1 || skill.Points.RemainingSP != 1 {
		t.Fatalf("GM forward awakening replaced learned state = %+v", skill)
	}
	// A GM branch rebase is intentionally destructive to the old branch: it
	// must not leak purchased skills, quick-slot layouts, commands or cooldowns
	// into the target profession. Seed all of those states before switching.
	skill.Skills[99] = dnfrepo.SkillState{Level: 7, Enabled: true}
	skill.Points.RemainingSP = 1
	skill.Points.RemainingTP = 0
	alternate, found := dnfrepo.SkillTree(skill, 1)
	if !found {
		t.Fatalf("PVF profession alternate tree is unavailable: %+v", skill)
	}
	alternate.Skills[98] = dnfrepo.SkillState{Level: 5, Enabled: true}
	alternate.Points.RemainingSP = 2
	alternate.Points.RemainingTP = 0
	dnfrepo.SetSkillTree(&skill, 1, alternate)
	skill.Layouts[0][11] = 99
	skill.Layouts[1][11] = 98
	skill.Commands = map[int64][]byte{99: {1, 2, 3}}
	skill.Cooldowns = map[int64]time.Time{99: time.Unix(1_800_000_000, 0).UTC()}
	if err := repositories.Skill.Save(ctx, skill); err != nil {
		t.Fatal(err)
	}
	connection.write.Reset()
	branchUpdate, err := service.GMSetCharacterJob(ctx, GMCharacterJobUpdate{CharacterID: "88", Job: 2, FirstGrow: 1, SecondGrow: 0})
	if err != nil || !branchUpdate.Refreshed || branchUpdate.PreviousGrowType != 0x12 || branchUpdate.GrowType != 1 || branchUpdate.TransitionCount != 1 {
		t.Fatalf("GM branch rebase = %+v error=%v", branchUpdate, err)
	}
	branchPackets := splitAllCurrentUpperPackets(t, connection.write.Bytes())
	if len(branchPackets) != len(jobWant) {
		t.Fatalf("GM branch-rebase packet count=%d packets=%+v", len(branchPackets), branchPackets)
	}
	for index, want := range jobWant {
		if branchPackets[index].Header.MsgID != want {
			t.Fatalf("GM branch-rebase packet[%d]=%d want=%d", index, branchPackets[index].Header.MsgID, want)
		}
	}
	job, err = service.GMReadCharacterJob(ctx, "88")
	if err != nil || job.Job != 2 || job.FirstGrow != 1 || job.SecondGrow != 0 {
		t.Fatalf("GMReadCharacterJob(after rebase) = %+v error=%v", job, err)
	}
	skill, found, err = repositories.Skill.Load(ctx, "88")
	if err != nil || !found {
		t.Fatalf("load GM rebased skill found=%t error=%v", found, err)
	}
	primary, primaryFound := dnfrepo.SkillTree(skill, 0)
	alternate, alternateFound := dnfrepo.SkillTree(skill, 1)
	for _, tree := range []struct {
		name  string
		state dnfrepo.SkillTreeState
		found bool
	}{
		{name: "primary", state: primary, found: primaryFound},
		{name: "alternate", state: alternate, found: alternateFound},
	} {
		if !tree.found || len(tree.state.Skills) != 2 || tree.state.Skills[1].Level != 1 || tree.state.Skills[3].Level != 1 ||
			tree.state.Skills[2].Level != 0 || tree.state.Skills[197].Level != 0 || tree.state.Skills[98].Level != 0 || tree.state.Skills[99].Level != 0 ||
			tree.state.Points.RemainingSP != tree.state.Points.TotalSP || tree.state.Points.RemainingTP != tree.state.Points.TotalTP {
			t.Fatalf("GM rebased %s tree=%+v found=%t", tree.name, tree.state, tree.found)
		}
	}
	if len(skill.Commands) != 0 || len(skill.Cooldowns) != 0 || len(skill.Layouts) != 2 {
		t.Fatalf("GM rebased transient state=%+v", skill)
	}
	for tree, layout := range skill.Layouts {
		for _, skillID := range layout {
			if skillID != 1 && skillID != 3 {
				t.Fatalf("GM rebased layout tree=%d contains obsolete skill=%d layout=%+v", tree, skillID, layout)
			}
		}
	}
	// Returning to the base profession must also drop the persisted class
	// quest (100 is [job change quest] 1 in the test catalog) so the chain can
	// be accepted again, while unrelated quest rows survive.
	if err := repositories.Quest.Save(ctx, dnfrepo.QuestRecord{
		CharacterID: "88",
		States: map[int64]dnfrepo.QuestState{
			100:  {Status: "completed"},
			3145: {Status: "completed"},
		},
		Progress: map[int64]dnfrepo.QuestState{
			100: {Status: "active"},
		},
	}); err != nil {
		t.Fatal(err)
	}
	connection.write.Reset()
	baseUpdate, err := service.GMSetCharacterJob(ctx, GMCharacterJobUpdate{CharacterID: "88", Job: 2, FirstGrow: 0, SecondGrow: 0})
	if err != nil || !baseUpdate.Refreshed || baseUpdate.PreviousGrowType != 1 || baseUpdate.GrowType != 0 ||
		!reflect.DeepEqual(baseUpdate.QuestResetIDs, []int64{100}) {
		t.Fatalf("GM base-career reset = %+v error=%v", baseUpdate, err)
	}
	basePackets := splitAllCurrentUpperPackets(t, connection.write.Bytes())
	if len(basePackets) != len(jobWant) {
		t.Fatalf("GM base-career packet count=%d packets=%+v", len(basePackets), basePackets)
	}
	resetQuests, found, err := repositories.Quest.Load(ctx, "88")
	if err != nil || !found {
		t.Fatalf("load base-career quests found=%t error=%v", found, err)
	}
	if _, exists := resetQuests.States[100]; exists {
		t.Fatalf("profession quest state remained after base-career reset: %+v", resetQuests)
	}
	if _, exists := resetQuests.Progress[100]; exists {
		t.Fatalf("profession quest progress remained after base-career reset: %+v", resetQuests)
	}
	if _, exists := resetQuests.States[3145]; !exists {
		t.Fatalf("ordinary quest was removed by base-career reset: %+v", resetQuests)
	}
	job, err = service.GMReadCharacterJob(ctx, "88")
	if err != nil || job.Job != 2 || job.FirstGrow != 0 || job.SecondGrow != 0 {
		t.Fatalf("GMReadCharacterJob(after base reset) = %+v error=%v", job, err)
	}

	connection.write.Reset()
	if _, err := service.GMSetCharacterLevel(ctx, "88", 89); !errors.Is(err, ErrGMRequestInvalid) {
		t.Fatalf("GM level regression error=%v, want ErrGMRequestInvalid", err)
	}
	if _, err := service.GMSetCharacterJob(ctx, GMCharacterJobUpdate{CharacterID: "88", Job: 5, FirstGrow: 1}); !errors.Is(err, ErrGMRequestInvalid) {
		t.Fatalf("GM base-job conversion error=%v, want ErrGMRequestInvalid", err)
	}
	if connection.write.Len() != 0 {
		t.Fatalf("invalid GM progression wrote native packets=%x", connection.write.Bytes())
	}

	connection.write.Reset()
	equip, err := service.GMUnlockEquipSlots(ctx, "88")
	if err != nil || equip.Before != 0 || equip.After != gmAllEquipSlotsMask || !equip.Refreshed {
		t.Fatalf("GMUnlockEquipSlots() = %+v error=%v", equip, err)
	}
	equipPackets := splitAllCurrentUpperPackets(t, connection.write.Bytes())
	if len(equipPackets) != 1 || equipPackets[0].Header.MsgID != uint16(dnfenum.CmdPacketSetUDPIPPort) || len(equipPackets[0].Body) < 4 || equipPackets[0].Body[3] != 1 {
		t.Fatalf("GM equipment-slot native packets = %+v", equipPackets)
	}
	dungeons, err := service.GMUnlockDungeons(ctx, GMDungeonUnlockRequest{CharacterID: "88", Dungeons: map[string]int{"100": 4, "200": 0}})
	if err != nil || dungeons.Count != 1 || !dungeons.Online || dungeons.Refreshed || dungeons.RefreshError == "" {
		t.Fatalf("GMUnlockDungeons() = %+v error=%v", dungeons, err)
	}
	accountKey, keyErr := dnfdungeonpermission.AccountOwnerKey(character.AccountID)
	if keyErr != nil {
		t.Fatal(keyErr)
	}
	permission, found, err := repositories.DungeonPermission.Load(ctx, accountKey)
	if err != nil || !found || len(permission.Entries) != 1 || permission.Entries[0].ClearState != 4 {
		t.Fatalf("dungeon permission = %+v found=%t error=%v", permission, found, err)
	}
}

func TestGMActivateBlackDiamondCommitsAccountEntitlementAndDefersUnsafeOnlineRefresh(t *testing.T) {
	ctx := context.Background()
	now := time.Unix(1_800_000_000, 0).UTC()
	repositories := dnfrepomemory.NewMemoryGroup()
	if err := repositories.Account.Save(ctx, dnfrepo.AccountRecord{
		AccountID: "dnf:test",
		Metadata: map[string]string{
			"account_cera":          "100",
			premium.MetadataKey(29): strconv.FormatInt(now.Add(24*time.Hour).Unix(), 10),
		},
	}); err != nil {
		t.Fatal(err)
	}
	if err := repositories.Character.Save(ctx, dnfrepo.CharacterRecord{
		CharacterID: "88", AccountID: "dnf:test", Name: "Receiver", Level: 90,
		Stats: map[string]int64{"fatigue": 120, "fatigue_limit": 156},
	}); err != nil {
		t.Fatal(err)
	}
	connection := &bufferConn{}
	session := &gameSession{conn: connection, connID: "gm-black-diamond-online", selectedCharacterID: 88}
	service := &Service{
		options:            options{accountID: "dnf:test", gameUpperHeader: gameUpperHeaderServer16, gameUpperBodyCodec: gameUpperBodyCodecPlain},
		repositoryProvider: func() (dnfrepo.Group, bool) { return repositories, true },
		gameSessions:       map[uint16]*gameSession{88: session},
		gameplayTimers:     &processGameplayTimeQueue{now: func() time.Time { return now }},
	}

	result, err := service.GMActivateBlackDiamond(ctx, GMBlackDiamondRequest{CharacterID: "88"})
	if err != nil {
		t.Fatal(err)
	}
	wantExpiry := now.Add(30 * 24 * time.Hour).Unix()
	if result.CharacterID != "88" || result.AccountID != "dnf:test" || result.Days != 30 ||
		result.ExpiresAt != wantExpiry || result.RemainingSeconds != int64(30*24*time.Hour/time.Second) ||
		result.Fatigue != 152 || result.FatigueLimit != 188 || !result.Online || result.Refreshed || !result.Deferred || result.RefreshError != "" {
		t.Fatalf("GMActivateBlackDiamond() = %+v", result)
	}
	if connection.write.Len() != 0 {
		t.Fatalf("deferred Black Diamond activation wrote unsafe town packets=%x", connection.write.Bytes())
	}
	if !service.currentBlackDiamondActive(session) {
		t.Fatal("online session did not receive the committed Black Diamond expiry")
	}
	account, found, err := repositories.Account.Load(ctx, "dnf:test")
	if err != nil || !found {
		t.Fatalf("load account found=%t err=%v", found, err)
	}
	if premium.BlackDiamondExpireAt(account) != wantExpiry || account.Metadata[premium.MetadataKey(29)] == "" || account.Metadata["account_cera"] != "100" {
		t.Fatalf("durable Black Diamond account metadata=%+v", account.Metadata)
	}
	character, found, err := repositories.Character.Load(ctx, "88")
	if err != nil || !found || character.Stats["fatigue"] != 152 || character.Stats["fatigue_limit"] != 188 {
		t.Fatalf("durable Black Diamond character=%+v found=%t err=%v", character, found, err)
	}

	delete(service.gameSessions, 88)
	renewed, err := service.GMActivateBlackDiamond(ctx, GMBlackDiamondRequest{CharacterID: "88", Days: 7})
	if err != nil || renewed.ExpiresAt != now.Add(37*24*time.Hour).Unix() || renewed.Online || renewed.Deferred {
		t.Fatalf("renewed Black Diamond result=%+v err=%v", renewed, err)
	}
	if _, err := service.GMActivateBlackDiamond(ctx, GMBlackDiamondRequest{CharacterID: "88", Days: 3651}); !errors.Is(err, ErrGMRequestInvalid) {
		t.Fatalf("invalid Black Diamond duration error=%v", err)
	}
}

// TestGMBlackDiamondStatusIsReadOnly：黑钻只读查询必须不落盘、不发包，且读的是
// 账号级权益（premium_expire_1000）而不是展示类型 56。
func TestGMBlackDiamondStatusIsReadOnly(t *testing.T) {
	ctx := context.Background()
	now := time.Unix(1_800_000_000, 0).UTC()
	repositories := dnfrepomemory.NewMemoryGroup()
	// 展示类型 56 故意写一个"看起来有效"的值：只读查询必须忽略它。
	if err := repositories.Account.Save(ctx, dnfrepo.AccountRecord{
		AccountID: "dnf:test",
		Metadata: map[string]string{
			premium.MetadataKey(premium.TypeBlackDiamondEntitlement): strconv.FormatInt(now.Add(30*24*time.Hour).Unix(), 10),
			"premium_expire_56": strconv.FormatInt(now.Add(365*24*time.Hour).Unix(), 10),
		},
	}); err != nil {
		t.Fatal(err)
	}
	if err := repositories.Character.Save(ctx, dnfrepo.CharacterRecord{
		CharacterID: "88", AccountID: "dnf:test", Name: "Receiver", Level: 90,
		Stats: map[string]int64{"fatigue": 120, "fatigue_limit": 188},
	}); err != nil {
		t.Fatal(err)
	}
	connection := &bufferConn{}
	session := &gameSession{conn: connection, connID: "gm-black-diamond-status", selectedCharacterID: 88}
	service := &Service{
		options:            options{accountID: "dnf:test", gameUpperHeader: gameUpperHeaderServer16, gameUpperBodyCodec: gameUpperBodyCodecPlain},
		repositoryProvider: func() (dnfrepo.Group, bool) { return repositories, true },
		gameSessions:       map[uint16]*gameSession{88: session},
		gameplayTimers:     &processGameplayTimeQueue{now: func() time.Time { return now }},
	}

	status, err := service.GMBlackDiamondStatus(ctx, GMBlackDiamondQuery{CharacterID: "88"})
	if err != nil {
		t.Fatal(err)
	}
	if status.CharacterID != "88" || status.CharacterName != "Receiver" || status.AccountID != "dnf:test" {
		t.Fatalf("GMBlackDiamondStatus() identity = %+v", status)
	}
	if !status.Active || status.ExpiresAt != now.Add(30*24*time.Hour).Unix() ||
		status.RemainingSeconds != int64(30*24*time.Hour/time.Second) || status.RemainingDays != 30 {
		t.Fatalf("GMBlackDiamondStatus() entitlement = %+v", status)
	}
	if status.Fatigue != 120 || status.FatigueLimit != 188 || status.ServerNow != now.Unix() || !status.Online {
		t.Fatalf("GMBlackDiamondStatus() runtime = %+v", status)
	}
	// 只读：不能写任何网络包，也不能改动账号/角色数据。
	if connection.write.Len() != 0 {
		t.Fatalf("read-only query wrote packets=%x", connection.write.Bytes())
	}
	account, found, err := repositories.Account.Load(ctx, "dnf:test")
	if err != nil || !found {
		t.Fatalf("load account found=%t err=%v", found, err)
	}
	if premium.BlackDiamondExpireAt(account) != now.Add(30*24*time.Hour).Unix() {
		t.Fatalf("read-only query mutated entitlement=%+v", account.Metadata)
	}
	character, found, err := repositories.Character.Load(ctx, "88")
	if err != nil || !found || character.Stats["fatigue_limit"] != 188 {
		t.Fatalf("read-only query mutated character=%+v found=%t err=%v", character, found, err)
	}

	// 过期账号：Active=false 但到期时间仍要回显（工具端要显示"已过期"）。
	if err := repositories.Account.Save(ctx, dnfrepo.AccountRecord{
		AccountID: "dnf:test",
		Metadata: map[string]string{
			premium.MetadataKey(premium.TypeBlackDiamondEntitlement): strconv.FormatInt(now.Add(-time.Hour).Unix(), 10),
		},
	}); err != nil {
		t.Fatal(err)
	}
	expired, err := service.GMBlackDiamondStatus(ctx, GMBlackDiamondQuery{CharacterID: "88"})
	if err != nil || expired.Active || expired.RemainingSeconds != 0 || expired.ExpiresAt != now.Add(-time.Hour).Unix() {
		t.Fatalf("expired status=%+v err=%v", expired, err)
	}

	// 按角色名查（工具端用名字时）与找不到角色。
	if byName, err := service.GMBlackDiamondStatus(ctx, GMBlackDiamondQuery{CharacterName: "Receiver"}); err != nil || byName.CharacterID != "88" {
		t.Fatalf("query by name status=%+v err=%v", byName, err)
	}
	if _, err := service.GMBlackDiamondStatus(ctx, GMBlackDiamondQuery{CharacterID: "999"}); !errors.Is(err, ErrGMCharacterNotFound) {
		t.Fatalf("missing character error=%v", err)
	}
	if _, err := service.GMBlackDiamondStatus(ctx, GMBlackDiamondQuery{}); !errors.Is(err, ErrGMRequestInvalid) {
		t.Fatalf("empty query error=%v", err)
	}
}

func TestGMUnlockDungeonsSendsCurrentPVFPermissionSnapshotToOnlineCharacter(t *testing.T) {
	ctx := context.Background()
	source := bridgeDungeonPVF(false)
	source["dungeon/test.dgn"] = "[name]\n`Synthetic Dungeon`\n" +
		"[difficulty level]\n0 1 2 3 4\n" +
		"[maze info]\n[size]\n1 1\n[greed]\n`A`\n" +
		"[map specification]\n`map` 0 0 100\n[start map]\n0 0\n[boss map]\n0 0\n"
	table, resolver, _ := loadBridgeDungeonStaticData(t, source)
	repositories := dnfrepomemory.NewMemoryGroup()
	if err := repositories.Character.Save(ctx, dnfrepo.CharacterRecord{
		CharacterID: "88", AccountID: "dnf:test", Name: "Receiver", Job: "2", Level: 90,
	}); err != nil {
		t.Fatal(err)
	}
	connection := &bufferConn{}
	service := &Service{
		options:            options{gameUpperHeader: gameUpperHeaderServer16, gameUpperBodyCodec: gameUpperBodyCodecPlain},
		worldMapTable:      table,
		worldMapResolver:   resolver,
		repositoryProvider: func() (dnfrepo.Group, bool) { return repositories, true },
		gameSessions: map[uint16]*gameSession{88: {
			conn: connection, connID: "gm-dungeon-permission-online", accountID: "dnf:test", selectedCharacterID: 88,
		}},
	}
	result, err := service.GMUnlockDungeons(ctx, GMDungeonUnlockRequest{
		CharacterID: "88",
		Dungeons:    map[string]int{"700": 4},
	})
	if err != nil || !result.Online || !result.Refreshed || result.Count != 1 || result.RefreshError != "" {
		t.Fatalf("GMUnlockDungeons() = %+v error=%v", result, err)
	}
	packets := splitAllCurrentUpperPackets(t, connection.write.Bytes())
	// The current-PVF projection clamps the persisted request to the dungeon's
	// client-visible maximum instead of echoing an unproved GM request byte.
	want := buildCurrentDungeonResourceStateEntriesBody([]currentDungeonResourceStateEntry{{DungeonID: 700, State: 3}})
	if len(packets) != 1 || packets[0].Header.MsgID != currentDungeonResourceStateMsgID ||
		packets[0].Header.Classification != 0 || !bytes.Equal(packets[0].Body[3:], want) {
		t.Fatalf("GM dungeon permission native packets = %+v want_body=%x", packets, want)
	}
}

func TestGMRepairHappyTokenCeraVoucherMovesLegacyMaterialIntoAccountWallet(t *testing.T) {
	ctx := context.Background()
	repositories := dnfrepomemory.NewMemoryGroup()
	if err := repositories.Account.Save(ctx, dnfrepo.AccountRecord{
		AccountID: "dnf:test",
		Metadata: map[string]string{
			"account_cera":             "700",
			"account_token_cera":       "100",
			"account_happy_token_cera": "1",
		},
	}); err != nil {
		t.Fatal(err)
	}
	if err := repositories.Character.Save(ctx, dnfrepo.CharacterRecord{
		CharacterID: "88", AccountID: "dnf:test", Name: "Receiver", Level: 90,
	}); err != nil {
		t.Fatal(err)
	}
	if err := repositories.Inventory.Save(ctx, dnfrepo.InventoryRecord{
		CharacterID: "88",
		Slots: map[string]dnfrepo.ItemStack{
			"0:121": {ItemID: 2681917, Count: 99999},
			"0:122": {ItemID: 3227, Count: 5},
		},
	}); err != nil {
		t.Fatal(err)
	}
	connection := &bufferConn{}
	service := &Service{
		options:            options{accountID: "dnf:test", gameUpperHeader: gameUpperHeaderServer16, gameUpperBodyCodec: gameUpperBodyCodecPlain},
		repositoryProvider: func() (dnfrepo.Group, bool) { return repositories, true },
		gameSessions: map[uint16]*gameSession{88: {
			conn: connection, connID: "gm-happy-token-repair", selectedCharacterID: 88,
		}},
	}

	result, err := service.GMRepairHappyTokenCeraVoucher(ctx, GMWalletRequest{CharacterID: "88"})
	if err != nil {
		t.Fatal(err)
	}
	if result.Granted != 99999 || result.Cera != 700 || result.TokenCera != 100 || result.HappyTokenCera != 100000 || !result.Refreshed {
		t.Fatalf("repair result = %+v", result)
	}
	inventory, found, err := repositories.Inventory.Load(ctx, "88")
	if err != nil || !found {
		t.Fatalf("inventory found=%t err=%v", found, err)
	}
	if _, exists := inventory.Slots["0:121"]; exists || inventory.Slots["0:122"].Count != 5 {
		t.Fatalf("repaired inventory = %+v", inventory.Slots)
	}
	account, found, err := repositories.Account.Load(ctx, "dnf:test")
	if err != nil || !found || account.Metadata["account_happy_token_cera"] != "100000" {
		t.Fatalf("repaired account found=%t err=%v metadata=%v", found, err, account.Metadata)
	}
	packets := splitAllCurrentUpperPackets(t, connection.write.Bytes())
	if len(packets) != 2 || packets[0].Header.MsgID != uint16(dnfenum.CmdPacketWalkoutPartyMember) ||
		packets[1].Header.MsgID != currentCeraShopUpdateMsgID ||
		len(packets[1].Body) < 3 || !bytes.Equal(packets[1].Body[3:], buildCurrentCeraShopWalletBody(700, 100, 100000)) {
		t.Fatalf("repair refresh packets = %+v", packets)
	}
}
