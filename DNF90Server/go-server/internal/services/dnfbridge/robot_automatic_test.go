package dnfbridge

import (
	"context"
	"strconv"
	"strings"
	"testing"
	"time"

	"longheng.io/server/internal/modules/dnf/channelcatalog"
	"longheng.io/server/internal/modules/dnf/channelinfo"
	"longheng.io/server/internal/modules/dnf/jobmap"
	"longheng.io/server/internal/modules/dnf/premium"
	dnfprogression "longheng.io/server/internal/modules/dnf/progression"
	dnfpvf "longheng.io/server/internal/modules/dnf/pvf"
	dnfquest "longheng.io/server/internal/modules/dnf/quest"
	dnfrepo "longheng.io/server/internal/modules/dnf/repository"
	dnfrepomemory "longheng.io/server/internal/modules/dnf/repository/memory"
)

func TestEnsureAutomaticRobotProfileCreatesThenReusesNativeCharacter(t *testing.T) {
	repositories := dnfrepomemory.NewMemoryGroup()
	experience, err := dnfprogression.LoadExperienceTable(context.Background(), dungeonDropCatalogTestSource{
		dnfprogression.ExperienceTablePath: strings.Repeat("100 ", 100),
	})
	if err != nil {
		t.Fatal(err)
	}
	index, err := channelinfo.LoadFile("testdata/channel_info.etc")
	if err != nil {
		t.Fatal(err)
	}
	channels, err := channelcatalog.New(index, channelcatalog.Options{ServerID: 1})
	if err != nil {
		t.Fatal(err)
	}
	service := &Service{
		repositoryProvider: func() (dnfrepo.Group, bool) { return repositories, true },
		experienceTable:    experience,
		catalog:            channels,
		questCatalog:       automaticRobotTestQuestCatalog(t),
	}
	job := int((automaticRobotSeed(1) >> 8) % uint64(jobmap.MaxID))
	level := currentAdventureCharacterLevelCap
	prepareTestCharacterInitialization(service, byte(job))
	prepareAutomaticRobotAvatarCatalog(service, byte(job))
	service.initialSPTable[level] = 100

	first, err := service.EnsureAutomaticRobotProfile(context.Background(), 1)
	if err != nil {
		t.Fatal(err)
	}
	legacy, found, err := repositories.Character.Load(context.Background(), strconv.Itoa(int(first.CharacterID)))
	if err != nil || !found {
		t.Fatalf("load automatic character: found=%v err=%v", found, err)
	}
	legacy = dnfrepo.CloneCharacter(legacy)
	legacy.Level = 55
	legacy.Stats[currentDungeonTutorialCompletedKey] = 0
	legacy.Stats["tutorial_reward_progress_38"] = 0
	legacy.Stats[dnfrepo.CharacterStoryDigestLastLevelStatKey] = 0
	legacy.Stats[dnfrepo.CharacterStoryDigestMigrationVersionStatKey] = 0
	legacy.Stats["ex_equip_slot_stat"] = 0
	if err := repositories.Character.Save(context.Background(), legacy); err != nil {
		t.Fatal(err)
	}
	second, err := service.EnsureAutomaticRobotProfile(context.Background(), 1)
	if err != nil {
		t.Fatal(err)
	}
	if first != second || first.CharacterID == 0 || first.AutoStart || first.ChannelID != automaticRobotPreferredChannelID || first.ID != "auto-generated-001" ||
		first.Village != automaticRobotTownID || first.Area != automaticRobotTownAreaID {
		t.Fatalf("automatic profiles differ: first=%+v second=%+v", first, second)
	}
	characters, err := repositories.Character.ListByAccount(context.Background(), first.AccountID, dnfrepo.MaxCharacterSlotLimit)
	if err != nil || len(characters) != 1 || characters[0].Level != level || characters[0].Job != strconv.Itoa(job) ||
		characters[0].Stats[currentDungeonTutorialCompletedKey] != currentDungeonTutorialCompleteFlag ||
		characters[0].Stats["tutorial_reward_progress_38"] != 1 ||
		characters[0].Stats[dnfrepo.CharacterStoryDigestLastLevelStatKey] != int64(currentAdventureCharacterLevelCap) ||
		characters[0].Stats[dnfrepo.CharacterStoryDigestMigrationVersionStatKey] != int64(dnfrepo.CurrentCharacterStoryDigestMigrationVersion) ||
		characters[0].Stats["ex_equip_slot_stat"] != gmAllEquipSlotsMask ||
		characters[0].Stats["town_id"] != automaticRobotTownID || characters[0].Stats["area_id"] != automaticRobotTownAreaID {
		t.Fatalf("automatic native characters = %+v err=%v", characters, err)
	}
	quests, found, err := repositories.Quest.Load(context.Background(), characters[0].CharacterID)
	if err != nil || !found || quests.States[900001].Status != "completed" || quests.States[900002].Status != "completed" {
		t.Fatalf("automatic main story = %+v found=%t err=%v", quests, found, err)
	}
	equipment, found, err := repositories.Equipment.Load(context.Background(), characters[0].CharacterID)
	if err != nil || !found {
		t.Fatalf("automatic avatar equipment: found=%t err=%v", found, err)
	}
	for index, slot := range currentAvatarFittingRoomEquipmentSlots {
		entry := equipment.Entries[strconv.Itoa(int(slot))]
		if entry.ItemID != int64(810000+index) || len(entry.RawEntry) == 0 {
			t.Fatalf("automatic avatar part=%d slot=%d entry=%+v", index, slot, entry)
		}
	}
	account, found, err := repositories.Account.Load(context.Background(), first.AccountID)
	if err != nil || !found || !premium.Active(account, premium.TypeAuction, time.Now().UTC()) ||
		!premium.Active(account, premium.TypePrivateStore, time.Now().UTC()) {
		t.Fatalf("automatic store contract account=%+v found=%t err=%v", account, found, err)
	}
}

func prepareAutomaticRobotAvatarCatalog(service *Service, job byte) {
	items := [currentAvatarFittingRoomPartCount]uint32{}
	definitions := make(map[uint32]dungeonDropItemDefinition, len(items))
	for index := range items {
		itemID := uint32(810000 + index)
		items[index] = itemID
		equipmentType := "[" + currentAvatarFittingRoomPartNames[index] + " avatar]"
		if index == 3 {
			equipmentType = "[breast avatar]"
		} else if index == 5 {
			equipmentType = "[waist avatar]"
		}
		definitions[itemID] = dungeonDropItemDefinition{ItemID: itemID, Kind: dungeonDropItemEquipment, EquipmentType: equipmentType}
	}
	service.automaticRobotAvatarConfig = &currentAvatarFittingRoomConfig{Sets: []currentAvatarFittingRoomSet{{Jobs: map[byte][currentAvatarFittingRoomPartCount]uint32{job: items}}}}
	service.pvfItemCatalog = &pvfDungeonDropCatalog{source: avatarCompoundPVFTestSource{}, itemCache: definitions}
}

func automaticRobotTestQuestCatalog(t *testing.T) *dnfquest.Catalog {
	t.Helper()
	source := questListTestSource{
		dnfquest.DefaultList:                   "900001 `automatic_robot_main.qst`\n900002 `automatic_robot_cap_main.qst`\n",
		"n_quest/automatic_robot_main.qst":     "[grade]\n`[epic]`\n[level]\n1 99\n[job]\n`[all]`\n[exposed by npc]\n1\n[type]\n`[meet npc]`\n",
		"n_quest/automatic_robot_cap_main.qst": "[grade]\n`[epic]`\n[level]\n90 99\n[job]\n`[all]`\n[exposed by npc]\n1\n[type]\n`[meet npc]`\n[pre required quest]\n900001\n",
	}
	index, err := dnfpvf.Build(context.Background(), source, dnfpvf.BuildOptions{Lists: []string{dnfquest.DefaultList}})
	if err != nil {
		t.Fatal(err)
	}
	catalog, err := dnfquest.Load(context.Background(), index)
	if err != nil {
		t.Fatal(err)
	}
	return catalog
}
