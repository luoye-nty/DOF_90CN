package robot

import (
	"context"
	"errors"
	"testing"
	"time"

	dnfrepo "longheng.io/server/internal/modules/dnf/repository"
)

type settingsFixture struct {
	record dnfrepo.SettingsRecord
	has    bool
	err    error
}

func (f *settingsFixture) Check(context.Context) error { return nil }
func (f *settingsFixture) Load(_ context.Context, key string) (dnfrepo.SettingsRecord, bool, error) {
	if key != SettingsScope {
		return dnfrepo.SettingsRecord{}, false, errors.New("unexpected scope")
	}
	return dnfrepo.CloneSettings(f.record), f.has, f.err
}
func (f *settingsFixture) Save(_ context.Context, record dnfrepo.SettingsRecord) error {
	if f.err != nil {
		return f.err
	}
	f.record, f.has = dnfrepo.CloneSettings(record), true
	return nil
}

func TestOwnerRoundTripsNormalizedProfiles(t *testing.T) {
	repo := &settingsFixture{}
	owner := NewOwner(func() (dnfrepo.Group, bool) {
		return dnfrepo.Group{Settings: repo}, true
	})
	owner.now = func() time.Time { return time.Unix(123, 0) }
	want := Config{Profiles: []Profile{
		{ID: " beta ", AccountID: " account-b ", CharacterID: 2, ChannelID: 19},
		{ID: "alpha", AccountID: "account-a", CharacterID: 1, ChannelID: 6, AutoStart: true, X: 20, Y: 30},
	}}
	saved, err := owner.Save(context.Background(), want)
	if err != nil {
		t.Fatalf("Save: %v", err)
	}
	if len(saved.Profiles) != 2 || saved.Profiles[0].ID != "alpha" || saved.Profiles[1].ID != "beta" {
		t.Fatalf("unexpected normalized profiles: %+v", saved.Profiles)
	}
	loaded, err := owner.Load(context.Background())
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	if len(loaded.Profiles) != 2 || loaded.Profiles[0].AccountID != "account-a" || loaded.Profiles[1].AccountID != "account-b" {
		t.Fatalf("unexpected loaded profiles: %+v", loaded.Profiles)
	}
	saved.Profiles[0].AccountID = "mutated"
	if loaded.Profiles[0].AccountID != "account-a" {
		t.Fatal("owner result leaked mutable profile storage")
	}
}

func TestNormalizeRejectsConflictingCharacterOwnership(t *testing.T) {
	_, err := Normalize(Config{Profiles: []Profile{
		{ID: "a", AccountID: "one", CharacterID: 7, ChannelID: 19},
		{ID: "b", AccountID: "two", CharacterID: 7, ChannelID: 19},
	}})
	if !errors.Is(err, ErrInvalid) {
		t.Fatalf("Normalize error = %v, want ErrInvalid", err)
	}
}

func TestNormalizeAutomationPolicyAndCloneMessages(t *testing.T) {
	config, err := Normalize(Config{
		Profiles: []Profile{{ID: "robot", AccountID: "account", CharacterID: 1, ChannelID: 19}},
		Automation: AutomationPolicy{
			Enabled: true, MoveIntervalMinSeconds: 5, MoveIntervalMaxSeconds: 10,
			ChatIntervalMinSeconds: 30, ChatIntervalMaxSeconds: 60,
			ChatMessages: []string{" hello ", "world"},
		},
	})
	if err != nil {
		t.Fatalf("Normalize: %v", err)
	}
	if config.Automation.MoveRadius != 80 || config.Automation.ChatMode != 3 || config.Automation.ChatMessages[0] != "hello" {
		t.Fatalf("unexpected normalized automation: %+v", config.Automation)
	}
	clone := CloneConfig(config)
	clone.Automation.ChatMessages[0] = "mutated"
	if config.Automation.ChatMessages[0] != "hello" {
		t.Fatal("CloneConfig leaked automation message storage")
	}
}

func TestNormalizePreservesSupportedAutomaticFleetVersion(t *testing.T) {
	config, err := Normalize(Config{AutomaticFleetVersion: 7})
	if err != nil || config.AutomaticFleetVersion != 7 || CloneConfig(config).AutomaticFleetVersion != 7 {
		t.Fatalf("automatic fleet version config=%+v err=%v", config, err)
	}
	if _, err := Normalize(Config{AutomaticFleetVersion: 8}); err == nil {
		t.Fatal("expected unsupported automatic fleet version rejection")
	}
}

func TestNormalizeValidatesProfileMovementBounds(t *testing.T) {
	profile := Profile{
		ID: "bounded", AccountID: "account", CharacterID: 1, ChannelID: 19,
		X: 500, Y: 200, MoveBoundsSet: true, MoveMinX: 400, MoveMaxX: 600, MoveMinY: 190, MoveMaxY: 210,
	}
	if _, err := Normalize(Config{Profiles: []Profile{profile}}); err != nil {
		t.Fatalf("Normalize valid bounds: %v", err)
	}
	profile.X = 700
	if _, err := Normalize(Config{Profiles: []Profile{profile}}); !errors.Is(err, ErrInvalid) {
		t.Fatalf("Normalize outside bounds error = %v, want ErrInvalid", err)
	}
	profile = Profile{ID: "orphan", AccountID: "account", CharacterID: 2, ChannelID: 19, MoveMaxX: 1}
	if _, err := Normalize(Config{Profiles: []Profile{profile}}); !errors.Is(err, ErrInvalid) {
		t.Fatalf("Normalize orphan bounds error = %v, want ErrInvalid", err)
	}
}

func TestNormalizeAutomationMegaphone(t *testing.T) {
	config, err := Normalize(Config{Automation: AutomationPolicy{
		ChatMessages:                []string{"hello"},
		MegaphoneIntervalMinSeconds: 900, MegaphoneIntervalMaxSeconds: 1800,
		MegaphoneProfileModulo: 50,
	}})
	if err != nil {
		t.Fatalf("Normalize megaphone: %v", err)
	}
	if config.Automation.MegaphoneProfileModulo != 50 {
		t.Fatalf("megaphone policy = %+v", config.Automation)
	}
	if len(config.Automation.MegaphoneMessages) != 1 || config.Automation.MegaphoneMessages[0] != "hello" {
		t.Fatalf("legacy megaphone messages were not copied from chat: %+v", config.Automation.MegaphoneMessages)
	}
	separate, err := Normalize(Config{Automation: AutomationPolicy{
		ChatMessages:                []string{"local"},
		MegaphoneMessages:           []string{"global"},
		MegaphoneIntervalMinSeconds: 60, MegaphoneIntervalMaxSeconds: 120,
	}})
	if err != nil {
		t.Fatalf("Normalize separate megaphone messages: %v", err)
	}
	if len(separate.Automation.MegaphoneMessages) != 1 || separate.Automation.MegaphoneMessages[0] != "global" {
		t.Fatalf("separate megaphone messages = %+v", separate.Automation.MegaphoneMessages)
	}
	if _, err := Normalize(Config{Automation: AutomationPolicy{
		MegaphoneIntervalMinSeconds: 1, MegaphoneIntervalMaxSeconds: 2, MegaphoneProfileModulo: 1,
	}}); !errors.Is(err, ErrInvalid) {
		t.Fatalf("megaphone without messages error = %v, want ErrInvalid", err)
	}
}

func TestNormalizeMigratesLegacyProfilesIntoRobotTypes(t *testing.T) {
	config, err := Normalize(Config{
		Automation: AutomationPolicy{
			MoveIntervalMinSeconds: 8, MoveIntervalMaxSeconds: 30, MoveRadius: 120,
			ChatIntervalMinSeconds: 60, ChatIntervalMaxSeconds: 180,
			ChatMessages: []string{"legacy local"},
		},
		Recycle: RecyclePolicy{Enabled: true, BuyItems: []RecycleBuyItem{{ItemID: 3037, Price: 10}}},
		Profiles: []Profile{
			{ID: "expert", AccountID: "a1", CharacterID: 1, ChannelID: 19, ExpertStoreKind: "disjoint"},
			{ID: "shop", AccountID: "a2", CharacterID: 2, ChannelID: 19, PrivateStoreTitle: "shop",
				PrivateStoreItems: []PrivateStoreItem{{ItemID: 3037, Count: 1, Price: 2}}},
			{ID: "normal", AccountID: "a3", CharacterID: 3, ChannelID: 19},
		},
	})
	if err != nil {
		t.Fatal(err)
	}
	byID := make(map[string]Profile, len(config.Profiles))
	for _, profile := range config.Profiles {
		byID[profile.ID] = profile
	}
	if byID["expert"].RobotType != RobotTypeProfessional || len(byID["expert"].SpeechMessages) != 0 {
		t.Fatalf("legacy expert migration = %+v", byID["expert"])
	}
	if byID["shop"].RobotType != RobotTypePersonalStore || len(byID["shop"].SpeechMessages) != 0 {
		t.Fatalf("legacy shop migration = %+v", byID["shop"])
	}
	normal := byID["normal"]
	if normal.RobotType != RobotTypeNormal || normal.MoveIntervalMinSeconds != 8 ||
		normal.MoveIntervalMaxSeconds != 30 || !normal.Recycle.Enabled ||
		len(normal.SpeechMessages) != 1 || normal.SpeechMessages[0] != "legacy local" {
		t.Fatalf("legacy normal migration = %+v", normal)
	}
}

func TestNormalizeRejectsRobotTypeAndStoreMismatch(t *testing.T) {
	tests := []Profile{
		{ID: "normal-expert", AccountID: "a", CharacterID: 1, ChannelID: 19,
			RobotType: RobotTypeNormal, ExpertStoreKind: "disjoint"},
		{ID: "professional-empty", AccountID: "b", CharacterID: 2, ChannelID: 19,
			RobotType: RobotTypeProfessional},
		{ID: "shop-empty", AccountID: "c", CharacterID: 3, ChannelID: 19,
			RobotType: RobotTypePersonalStore},
	}
	for _, profile := range tests {
		if _, err := Normalize(Config{Profiles: []Profile{profile}}); !errors.Is(err, ErrInvalid) {
			t.Fatalf("profile %q mismatch error = %v, want ErrInvalid", profile.ID, err)
		}
	}
}

func TestNormalizeProfileKeepsSpeechModeWithoutMessages(t *testing.T) {
	config, err := Normalize(Config{Profiles: []Profile{{
		ID: "speaker", AccountID: "a", CharacterID: 1, ChannelID: 19,
		RobotType: RobotTypeNormal, SpeechMode: SpeechModeMegaphone,
	}}})
	if err != nil {
		t.Fatal(err)
	}
	profile := config.Profiles[0]
	if profile.SpeechMode != SpeechModeMegaphone || !profile.MegaphoneEnabled ||
		profile.SpeechIntervalMinSeconds != 0 || len(profile.SpeechMessages) != 0 {
		t.Fatalf("speech tag migration = %+v", profile)
	}
}

func TestNormalizeRejectsPartialAutomationIntervals(t *testing.T) {
	_, err := Normalize(Config{Automation: AutomationPolicy{MoveIntervalMinSeconds: 5}})
	if !errors.Is(err, ErrInvalid) {
		t.Fatalf("Normalize error = %v, want ErrInvalid", err)
	}
	_, err = Normalize(Config{Automation: AutomationPolicy{
		ChatIntervalMinSeconds: 5, ChatIntervalMaxSeconds: 5,
	}})
	if !errors.Is(err, ErrInvalid) {
		t.Fatalf("Normalize error = %v, want ErrInvalid", err)
	}
}

func TestNormalizeAutomationPrivateStore(t *testing.T) {
	config, err := Normalize(Config{Automation: AutomationPolicy{
		Enabled: true, StoreIntervalMinSeconds: 30, StoreIntervalMaxSeconds: 60,
		StoreDurationSeconds: 120, StoreUnitPrice: 10000, StoreTitle: " robot shop ",
	}})
	if err != nil {
		t.Fatalf("Normalize: %v", err)
	}
	if config.Automation.StoreConcurrent != 1 || config.Automation.StoreMaximumRows != 8 || config.Automation.StoreTitle != "robot shop" {
		t.Fatalf("store automation = %+v", config.Automation)
	}
	_, err = Normalize(Config{Automation: AutomationPolicy{
		StoreIntervalMinSeconds: 30, StoreIntervalMaxSeconds: 60,
		StoreDurationSeconds: 120, StoreTitle: "missing price",
	}})
	if !errors.Is(err, ErrInvalid) {
		t.Fatalf("Normalize missing price error = %v, want ErrInvalid", err)
	}
}

func TestNormalizeAutomationScaling(t *testing.T) {
	config, err := Normalize(Config{Automation: AutomationPolicy{Enabled: true, TargetOnline: 12}})
	if err != nil {
		t.Fatalf("Normalize scaling: %v", err)
	}
	policy := config.Automation
	if policy.ScaleIntervalSeconds != 5 || policy.ScaleUpBatch != 2 || policy.ScaleDownBatch != 2 {
		t.Fatalf("scaling defaults = %+v", policy)
	}
	if _, err := Normalize(Config{Automation: AutomationPolicy{TargetOnline: MaxProfiles + 1}}); err == nil {
		t.Fatal("oversized scaling target unexpectedly accepted")
	}
}
