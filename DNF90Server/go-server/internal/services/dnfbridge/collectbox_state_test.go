package dnfbridge

import (
	"context"
	"testing"
	"time"

	dnfcollectbox "longheng.io/server/internal/modules/dnf/collectbox"
	dnfrepo "longheng.io/server/internal/modules/dnf/repository"
	"longheng.io/server/internal/modules/dnf/repository/memory"
)

func TestLoadCollectboxStatePrefersAccountMetadata(t *testing.T) {
	ctx := context.Background()
	repos := memory.NewMemoryGroup()
	accountState := dnfcollectbox.State{Boxes: map[string]map[string]uint32{"1": {"0": 1001}}}
	settingsState := dnfcollectbox.State{Boxes: map[string]map[string]uint32{"1": {"0": 2002}}}
	raw, err := dnfcollectbox.EncodeState(accountState)
	if err != nil {
		t.Fatal(err)
	}
	if err := repos.Account.Save(ctx, dnfrepo.AccountRecord{AccountID: "account-1", Metadata: map[string]string{dnfcollectbox.MetadataKey: raw}}); err != nil {
		t.Fatal(err)
	}
	if err := dnfcollectbox.SaveState(ctx, repos.Settings, "1", settingsState, time.Now()); err != nil {
		t.Fatal(err)
	}
	got, err := loadCollectboxState(ctx, repos, "account-1", 1)
	if err != nil {
		t.Fatal(err)
	}
	if got.Boxes["1"]["0"] != 1001 {
		t.Fatalf("state = %+v, want account metadata", got)
	}
}

func TestLoadCollectboxStateFallsBackToCharacterSettings(t *testing.T) {
	ctx := context.Background()
	repos := memory.NewMemoryGroup()
	if err := repos.Account.Save(ctx, dnfrepo.AccountRecord{AccountID: "account-2"}); err != nil {
		t.Fatal(err)
	}
	settingsState := dnfcollectbox.State{Boxes: map[string]map[string]uint32{"2": {"0": 3003}}}
	if err := dnfcollectbox.SaveState(ctx, repos.Settings, "2", settingsState, time.Now()); err != nil {
		t.Fatal(err)
	}
	got, err := loadCollectboxState(ctx, repos, "account-2", 2)
	if err != nil {
		t.Fatal(err)
	}
	if got.Boxes["2"]["0"] != 3003 {
		t.Fatalf("state = %+v, want settings fallback", got)
	}
}
