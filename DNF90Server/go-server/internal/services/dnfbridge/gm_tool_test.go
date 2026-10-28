package dnfbridge

import (
	"context"
	"errors"
	"testing"
	"time"

	"longheng.io/server/internal/modules/dnf/channelcatalog"
	"longheng.io/server/internal/modules/dnf/channelinfo"
	dnfpvf "longheng.io/server/internal/modules/dnf/pvf"
	dnfquest "longheng.io/server/internal/modules/dnf/quest"
	dnfrepo "longheng.io/server/internal/modules/dnf/repository"
	dnfrepomemory "longheng.io/server/internal/modules/dnf/repository/memory"
	appkit "longheng.io/server/internal/platform/app"
)

type gmQuestPVFSource map[string]string

func (s gmQuestPVFSource) ReadText(relativePath string) (string, error) {
	value, ok := s[relativePath]
	if !ok {
		return "", errors.New("missing " + relativePath)
	}
	return value, nil
}

func TestGMChannelsReportsAvailableActiveAndSinglePlayerSets(t *testing.T) {
	const text = "[server] 1\n6 `<chn_channel_info_002>` 36 `[goldenroad]` 0 0\n11 `<chn_channel_info_028>` 11 `[granfloris]` 0 0\n19 `<chn_channel_info_028>` 11 `[granfloris]` 0 0\n20 `<chn_channel_info_028>` 11 `[granfloris]` 0 0\n21 `<chn_channel_info_029>` 11 `[sky_catle]` 0 0\n24 `<chn_channel_info_029>` 11 `[sky_catle]` 0 0\n[/server]\n"
	index, err := channelinfo.Parse([]byte(text))
	if err != nil {
		t.Fatal(err)
	}
	available, err := channelcatalog.New(index, channelcatalog.Options{})
	if err != nil {
		t.Fatal(err)
	}
	service := &Service{catalog: available}
	overview, err := service.GMChannels()
	if err != nil {
		t.Fatal(err)
	}
	if overview.TotalChannels != 6 || overview.EnabledChannels != 6 || overview.GamePorts != 6 {
		t.Fatalf("overview counts = %+v", overview)
	}
	if len(overview.SinglePlayerIDs) != 0 {
		t.Fatalf("single player ids = %v", overview.SinglePlayerIDs)
	}
	if len(overview.MinimalSinglePlayerIDs) != 0 {
		t.Fatalf("minimal single player ids = %v", overview.MinimalSinglePlayerIDs)
	}
	byID := make(map[int]GMChannel)
	for _, channel := range overview.Channels {
		byID[channel.ID] = channel
	}
	if !byID[6].Required || byID[6].GroupMinimum != 1 || !byID[11].Required || !byID[19].Required || !byID[20].Enabled || !byID[21].Enabled || !byID[21].Ordinary || byID[21].GroupMinimum != 1 {
		t.Fatalf("channel projection = %+v", byID)
	}
}

func TestGMAccountCharacterAndOfflineGoldFlow(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	repositories := dnfrepomemory.NewMemoryGroup()
	if err := repositories.Account.Save(ctx, dnfrepo.AccountRecord{
		AccountID:            "dnf:test",
		State:                "active",
		RepresentAccountName: "GM Test",
		UpdatedAt:            time.Now().UTC(),
	}); err != nil {
		t.Fatalf("save account: %v", err)
	}
	if err := repositories.Character.Save(ctx, dnfrepo.CharacterRecord{
		CharacterID: "1",
		AccountID:   "dnf:test",
		Slot:        0,
		Name:        "Test Character",
		Job:         "0",
		Level:       10,
		Stats:       map[string]int64{"gold": 25, "exp": 100},
	}); err != nil {
		t.Fatalf("save character: %v", err)
	}
	service := NewWithOptions(&appkit.Env{}, ServiceOptions{
		RepositoryProvider: func() (dnfrepo.Group, bool) { return repositories, true },
	})

	accounts, err := service.GMListAccounts(ctx, "gm test", 10)
	if err != nil || len(accounts) != 1 || accounts[0].AccountID != "dnf:test" {
		t.Fatalf("GMListAccounts = %+v, %v", accounts, err)
	}
	characters, err := service.GMListCharacters(ctx, "dnf:test")
	if err != nil || len(characters) != 1 || characters[0].Gold != 25 || characters[0].ProfessionName != "男鬼剑士" || characters[0].Online {
		t.Fatalf("GMListCharacters = %+v, %v", characters, err)
	}
	updated, err := service.GMSetCharacterGold(ctx, "1", 123456)
	if err != nil || updated.Gold != 123456 {
		t.Fatalf("GMSetCharacterGold = %+v, %v", updated, err)
	}
	persisted, found, err := repositories.Character.Load(ctx, "1")
	if err != nil || !found || persisted.Stats["gold"] != 123456 {
		t.Fatalf("persisted character = %+v, %v, found=%v", persisted, err, found)
	}

	service.gameSessions[1] = &gameSession{selectedCharacterID: 1}
	if _, err := service.GMSetCharacterGold(ctx, "1", 99); !errors.Is(err, ErrGMCharacterOnline) {
		t.Fatalf("online GMSetCharacterGold error = %v, want %v", err, ErrGMCharacterOnline)
	}
}

func TestGMRejectsInvalidCharacterAndGold(t *testing.T) {
	t.Parallel()
	repositories := dnfrepomemory.NewMemoryGroup()
	service := NewWithOptions(&appkit.Env{}, ServiceOptions{
		RepositoryProvider: func() (dnfrepo.Group, bool) { return repositories, true },
	})
	if _, err := service.GMSetCharacterGold(context.Background(), "not-a-character", 1); !errors.Is(err, ErrGMInvalidRequest) {
		t.Fatalf("invalid character error = %v", err)
	}
	if _, err := service.GMSetCharacterGold(context.Background(), "1", -1); !errors.Is(err, ErrGMInvalidRequest) {
		t.Fatalf("negative gold error = %v", err)
	}
}

func TestGMOfflineQuestReadyCompleteAndUncomplete(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	repositories := dnfrepomemory.NewMemoryGroup()
	if err := repositories.Character.Save(ctx, dnfrepo.CharacterRecord{
		CharacterID: "7", AccountID: "dnf:test", Job: "0", Level: 20,
		Stats: map[string]int64{"grow_type": 0},
	}); err != nil {
		t.Fatal(err)
	}
	if err := repositories.Quest.Save(ctx, dnfrepo.QuestRecord{
		CharacterID: "7",
		States:      map[int64]dnfrepo.QuestState{10: {Status: "active", ProgressValue: 9}},
	}); err != nil {
		t.Fatal(err)
	}
	source := gmQuestPVFSource{
		dnfquest.DefaultList: "10 `gm.qst`\n",
		"n_quest/gm.qst":     "[name]\n`GM任务`\n[depend message]\n`请帮助赛丽亚。`\n[condition message]\n`通关测试地下城。`\n[solve message]\n`谢谢你的帮助！`\n[grade]\n`[normal]`\n[difficulty]\n`I`\n[type]\n`[clear map]`\n[level]\n1 99\n[job]\n`[all]`\n",
	}
	index, err := dnfpvf.Build(ctx, source, dnfpvf.BuildOptions{Lists: []string{dnfquest.DefaultList}})
	if err != nil {
		t.Fatal(err)
	}
	catalog, err := dnfquest.Load(ctx, index)
	if err != nil {
		t.Fatal(err)
	}
	service := NewWithOptions(&appkit.Env{}, ServiceOptions{
		RepositoryProvider: func() (dnfrepo.Group, bool) { return repositories, true },
	})
	service.questCatalog = catalog
	detail, err := service.GMQuestDetail(ctx, "7", 10)
	if err != nil || detail.Quest.QuestID != 10 || detail.Introduction != "请帮助赛丽亚。" ||
		detail.Objective != "通关测试地下城。" || detail.Completion != "谢谢你的帮助！" || detail.Difficulty != "I" {
		t.Fatalf("quest detail = %+v, %v", detail, err)
	}

	ready, err := service.GMMarkQuestReady(ctx, "7", 10)
	if err != nil || !ready.Quest.Active || ready.Quest.ProgressValue != 0 {
		t.Fatalf("ready = %+v, %v", ready, err)
	}
	completed, err := service.GMForceCompleteQuest(ctx, "7", 10)
	if err != nil || !completed.Quest.Completed || completed.RewardGranted || completed.ProfessionChanged {
		t.Fatalf("completed = %+v, %v", completed, err)
	}
	uncompleted, err := service.GMUncompleteQuest(ctx, "7", 10)
	if err != nil || uncompleted.Quest.Completed || uncompleted.Quest.Active {
		t.Fatalf("uncompleted = %+v, %v", uncompleted, err)
	}

	service.gameSessions[7] = &gameSession{selectedCharacterID: 7}
	if _, err := service.GMForceCompleteQuest(ctx, "7", 10); !errors.Is(err, ErrGMCharacterOnline) {
		t.Fatalf("online force-complete error = %v", err)
	}
}

func TestGMUncompleteLinkedQuestReactivatesFirstOrderedPVFStage(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	repositories := dnfrepomemory.NewMemoryGroup()
	if err := repositories.Character.Save(ctx, dnfrepo.CharacterRecord{
		CharacterID: "7", AccountID: "dnf:test", Job: "0", Level: 85,
		Stats: map[string]int64{"grow_type": 0},
	}); err != nil {
		t.Fatal(err)
	}
	if err := repositories.Quest.Save(ctx, dnfrepo.QuestRecord{
		CharacterID: "7",
		States: map[int64]dnfrepo.QuestState{
			11: {Status: "completed", ProgressValue: 0},
		},
	}); err != nil {
		t.Fatal(err)
	}
	source := gmQuestPVFSource{
		dnfquest.DefaultList: "10 `parent.qst`\n11 `meet.qst`\n12 `range.qst`\n",
		"n_quest/parent.qst": "[name]\n`GM父任务`\n[grade]\n`[epic]`\n[type]\n`[quest clear]`\n[int data]\n11 12\n[level]\n85 99\n[job]\n`[all]`\n",
		"n_quest/meet.qst":   "[name]\n`NPC子目标`\n[grade]\n`[sub]`\n[attribute]\n`not give exp quest`\n[/attribute]\n[type]\n`[meet npc]`\n[main quest]\n10\n[int data]\n133\n[reward type]\n`[item]`\n[reward int data]\n0 0\n[level]\n85 99\n[job]\n`[all]`\n",
		"n_quest/range.qst":  "[name]\n`地点子目标`\n[grade]\n`[sub]`\n[attribute]\n`not give exp quest`\n[/attribute]\n[type]\n`[reach the range]`\n[main quest]\n10\n[int data]\n344 200 200\n[reward type]\n`[item]`\n[reward int data]\n0 0\n[level]\n85 99\n[job]\n`[all]`\n",
	}
	index, err := dnfpvf.Build(ctx, source, dnfpvf.BuildOptions{Lists: []string{dnfquest.DefaultList}})
	if err != nil {
		t.Fatal(err)
	}
	catalog, err := dnfquest.Load(ctx, index)
	if err != nil {
		t.Fatal(err)
	}
	service := NewWithOptions(&appkit.Env{}, ServiceOptions{
		RepositoryProvider: func() (dnfrepo.Group, bool) { return repositories, true },
	})
	service.questCatalog = catalog

	reset, err := service.GMUncompleteQuest(ctx, "7", 11)
	if err != nil || !reset.Quest.Active || reset.Quest.Completed || reset.Quest.ProgressValue != 1 {
		t.Fatalf("linked reset=%+v err=%v", reset, err)
	}
	record, found, err := repositories.Quest.Load(ctx, "7")
	if err != nil || !found {
		t.Fatalf("load reset chain found=%t err=%v", found, err)
	}
	for _, expected := range []struct {
		questID int64
		trigger int64
		parent  string
	}{
		{questID: 10, trigger: 2},
		{questID: 11, trigger: 1, parent: "10"},
	} {
		state := record.States[expected.questID]
		if state.Status != "active" || state.ProgressValue != expected.trigger {
			t.Fatalf("reset quest %d state=%+v", expected.questID, state)
		}
		if expected.parent != "" && (state.Extra["main_quest_id"] != expected.parent || state.Extra["auto_activated_by_main_quest"] != "true") {
			t.Fatalf("reset quest %d extras=%+v", expected.questID, state.Extra)
		}
	}
	if state, exists := record.States[12]; exists {
		t.Fatalf("later range step activated before meet-NPC completion: %+v", state)
	}
}
