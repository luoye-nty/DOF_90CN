package dnfbridge

import (
	"context"
	"strconv"
	"testing"
	"time"

	dnffatigue "longheng.io/server/internal/modules/dnf/fatigue"
	"longheng.io/server/internal/modules/dnf/premium"
	dnfrepo "longheng.io/server/internal/modules/dnf/repository"
)

func TestCurrentMode0BlackDiamondUsesSecondChinaNameplateFlagOnly(t *testing.T) {
	ordinaryCharacter := dnfrepo.CharacterRecord{Job: "0", Level: 90, Stats: map[string]int64{}}
	memberCharacter := dnfrepo.CloneCharacter(ordinaryCharacter)
	memberCharacter.Stats = map[string]int64{}
	memberCharacter.Stats[currentBlackDiamondNameplateStat] = 1
	var ordinary packetWriter
	var member packetWriter
	writeCurrentSceneObjectEntryTail(&ordinary, ordinaryCharacter, true)
	writeCurrentSceneObjectEntryTail(&member, memberCharacter, true)
	ordinaryBody, memberBody := ordinary.bytes(), member.bytes()
	if len(ordinaryBody) != len(memberBody) {
		t.Fatalf("mode0 tail lengths=%d/%d", len(ordinaryBody), len(memberBody))
	}
	difference := -1
	for index := range ordinaryBody {
		if ordinaryBody[index] == memberBody[index] {
			continue
		}
		if difference >= 0 {
			t.Fatalf("mode0 tail changed more than the Black Diamond flag: first=%d second=%d", difference, index)
		}
		difference = index
	}
	if difference <= 0 || ordinaryBody[difference] != 0 || memberBody[difference] != 1 || memberBody[difference-1] != 0 {
		t.Fatalf("Black Diamond flag difference=%d ordinary=% X member=% X", difference, ordinaryBody, memberBody)
	}
}

func TestCurrentBlackDiamondExperienceUsesSessionEntitlement(t *testing.T) {
	now := time.Unix(1_800_000_000, 0).UTC()
	service := &Service{gameplayTimers: &processGameplayTimeQueue{now: func() time.Time { return now }}}
	session := &gameSession{blackDiamondExpiresAt: now.Add(time.Hour)}
	if got := service.currentBlackDiamondBonusExpForSession(session, 495); got != 49 {
		t.Fatalf("active Black Diamond bonus=%d, want 49", got)
	}
	session.blackDiamondExpiresAt = now
	if got := service.currentBlackDiamondBonusExpForSession(session, 495); got != 0 {
		t.Fatalf("expired Black Diamond bonus=%d, want 0", got)
	}
}

func TestCurrentBlackDiamondSelectionExpiryRestoresNativeFatigue(t *testing.T) {
	service, _, session, _, _ := prepareCompletedSettlementRuntime(t)
	now := time.Unix(1_800_000_000, 0).UTC()
	service.gameplayTimers = &processGameplayTimeQueue{now: func() time.Time { return now }}
	repositories, ok := service.repositoryGroup()
	if !ok {
		t.Fatal("repository group unavailable")
	}
	if err := repositories.Account.Save(context.Background(), dnfrepo.AccountRecord{
		AccountID: "account-1",
		Metadata: map[string]string{
			premium.MetadataKey(premium.TypeBlackDiamondEntitlement): strconv.FormatInt(now.Add(-time.Second).Unix(), 10),
			premium.MetadataKey(29):                                  strconv.FormatInt(now.Add(24*time.Hour).Unix(), 10),
		},
	}); err != nil {
		t.Fatal(err)
	}
	character, found, err := repositories.Character.Load(context.Background(), "99")
	if err != nil || !found {
		t.Fatalf("load character found=%t err=%v", found, err)
	}
	character.Stats["fatigue"] = 152
	character.Stats["fatigue_limit"] = 188
	if err := repositories.Character.Save(context.Background(), character); err != nil {
		t.Fatal(err)
	}
	result, err := service.reconcileCurrentBlackDiamondSelection(context.Background(), session, character)
	if err != nil {
		t.Fatal(err)
	}
	if !result.Found || result.Active || result.Character.Stats["fatigue"] != 120 || result.Character.Stats["fatigue_limit"] != 156 {
		t.Fatalf("expired selection result=%+v stats=%+v", result, result.Character.Stats)
	}
}

func TestSelectedCharacterForEnterUsesCurrentBlackDiamondEntitlement(t *testing.T) {
	service, _, session, _, _ := prepareCompletedSettlementRuntime(t)
	now := time.Unix(1_800_000_000, 0).UTC()
	service.gameplayTimers = &processGameplayTimeQueue{now: func() time.Time { return now }}
	repositories, ok := service.repositoryGroup()
	if !ok {
		t.Fatal("repository group unavailable")
	}
	if err := repositories.Account.Save(context.Background(), dnfrepo.AccountRecord{
		AccountID: "account-1",
		Metadata: map[string]string{
			premium.MetadataKey(premium.TypeBlackDiamondEntitlement): strconv.FormatInt(now.Add(-time.Second).Unix(), 10),
		},
	}); err != nil {
		t.Fatal(err)
	}
	character, found, err := repositories.Character.Load(context.Background(), "99")
	if err != nil || !found {
		t.Fatalf("load character found=%t err=%v", found, err)
	}
	character.Stats["fatigue"] = 152
	character.Stats["fatigue_limit"] = dnffatigue.BlackDiamondLimit
	dayID := premium.ServiceDayID(now)
	character.Stats[dnffatigue.DailyRecoveryDayStatKey] = dayID
	character.Stats[dnffatigue.PotionUsageDayStatKey] = dayID
	if err := repositories.Character.Save(context.Background(), character); err != nil {
		t.Fatal(err)
	}
	_, _, got, loaded := service.selectedCharacterForEnter(context.Background(), session)
	if !loaded || got.Stats["fatigue"] != 120 || got.Stats["fatigue_limit"] != dnffatigue.DefaultLimit {
		t.Fatalf("selected character=%+v loaded=%t", got, loaded)
	}
}
