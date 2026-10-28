package epicwish

import (
	"context"
	"errors"
	"reflect"
	"testing"

	dnfrepo "longheng.io/server/internal/modules/dnf/repository"
	dnfrepomemory "longheng.io/server/internal/modules/dnf/repository/memory"
	"longheng.io/server/internal/platform/db"
)

func TestOwnerApplyPersistsCanonicalStatsAndFreezesResult(t *testing.T) {
	repositories := seededEpicWishRepositories(t, map[string]int64{
		"gold":             99,
		CountStat:          1,
		itemStatKeys[0]:    900,
		itemStatKeys[1]:    901,
		itemStatKeys[2]:    902,
		itemStatKeys[3]:    903,
		itemStatKeys[4]:    904,
		"unrelated_marker": 77,
	})
	owner, err := NewOwner(repositories)
	if err != nil {
		t.Fatal(err)
	}
	requested := []uint32{100, 0, 200}
	result, err := owner.Apply(context.Background(), Command{
		CharacterID: " 19 ",
		AccountID:   " account-1 ",
		Items:       requested,
	})
	if err != nil {
		t.Fatal(err)
	}
	if !result.Changed || !reflect.DeepEqual(result.Items, requested) {
		t.Fatalf("result=%+v", result)
	}
	assertEpicWishCharacter(t, result.Character, requested)
	if result.Character.Stats["gold"] != 99 || result.Character.Stats["unrelated_marker"] != 77 {
		t.Fatalf("unrelated stats changed: %+v", result.Character.Stats)
	}

	requested[0] = 999
	result.Items[0] = 998
	result.Character.Stats[itemStatKeys[0]] = 997
	stored := loadEpicWishCharacter(t, repositories)
	assertEpicWishCharacter(t, stored, []uint32{100, 0, 200})
	if stored.Stats["gold"] != 99 || stored.Stats["unrelated_marker"] != 77 {
		t.Fatalf("stored unrelated stats changed: %+v", stored.Stats)
	}
	items, err := ItemsFromCharacter(stored)
	if err != nil || !reflect.DeepEqual(items, []uint32{100, 0, 200}) {
		t.Fatalf("decoded items=%v err=%v", items, err)
	}
	items[0] = 996
	if again, _ := ItemsFromCharacter(stored); !reflect.DeepEqual(again, []uint32{100, 0, 200}) {
		t.Fatalf("decoded items were not frozen: %v", again)
	}
}

func TestOwnerApplyRepeatedRequestIsIdempotent(t *testing.T) {
	repositories := seededEpicWishRepositories(t, map[string]int64{"gold": 50})
	saves := 0
	repositories.CharacterAssets = countingEpicWishAssets{
		base:  repositories.CharacterAssets,
		saves: &saves,
	}
	owner, err := NewOwner(repositories)
	if err != nil {
		t.Fatal(err)
	}
	command := Command{CharacterID: "19", AccountID: "account-1", Items: []uint32{100, 0, 200}}
	first, err := owner.Apply(context.Background(), command)
	if err != nil {
		t.Fatal(err)
	}
	second, err := owner.Apply(context.Background(), command)
	if err != nil {
		t.Fatal(err)
	}
	if !first.Changed || second.Changed || saves != 1 {
		t.Fatalf("first=%+v second=%+v saves=%d", first, second, saves)
	}
	first.Items[0] = 999
	first.Character.Stats[itemStatKeys[0]] = 998
	if !reflect.DeepEqual(second.Items, command.Items) || second.Character.Stats[itemStatKeys[0]] != 100 {
		t.Fatalf("idempotent results share mutable state: first=%+v second=%+v", first, second)
	}
}

func TestOwnerApplyValidatesLimitAndNonzeroUniquenessBeforeMutation(t *testing.T) {
	repositories := seededEpicWishRepositories(t, canonicalEpicWishStats([]uint32{700}))
	owner, err := NewOwner(repositories)
	if err != nil {
		t.Fatal(err)
	}
	for _, test := range []struct {
		name  string
		items []uint32
		want  error
	}{
		{name: "too many", items: []uint32{1, 2, 3, 4, 5, 6}, want: ErrTooManyItems},
		{name: "duplicate nonzero", items: []uint32{1, 0, 1}, want: ErrDuplicateItem},
	} {
		t.Run(test.name, func(t *testing.T) {
			if _, err := owner.Apply(context.Background(), Command{
				CharacterID: "19",
				AccountID:   "account-1",
				Items:       test.items,
			}); !errors.Is(err, test.want) {
				t.Fatalf("err=%v want=%v", err, test.want)
			}
			assertEpicWishCharacter(t, loadEpicWishCharacter(t, repositories), []uint32{700})
		})
	}

	result, err := owner.Apply(context.Background(), Command{
		CharacterID: "19",
		AccountID:   "account-1",
		Items:       []uint32{0, 0},
	})
	if err != nil || !result.Changed {
		t.Fatalf("zero placeholders result=%+v err=%v", result, err)
	}
	assertEpicWishCharacter(t, loadEpicWishCharacter(t, repositories), []uint32{0, 0})
}

func TestOwnerApplyRejectsNilOwnerMissingIdentityAndAccountMismatch(t *testing.T) {
	if _, err := NewOwner(dnfrepo.Group{}); !errors.Is(err, ErrOwnerUnavailable) {
		t.Fatalf("NewOwner err=%v", err)
	}
	var nilOwner *Owner
	if _, err := nilOwner.Apply(context.Background(), Command{}); !errors.Is(err, ErrOwnerUnavailable) {
		t.Fatalf("nil owner err=%v", err)
	}

	repositories := seededEpicWishRepositories(t, canonicalEpicWishStats([]uint32{700}))
	owner, err := NewOwner(repositories)
	if err != nil {
		t.Fatal(err)
	}
	for _, test := range []struct {
		name    string
		command Command
		want    error
	}{
		{name: "character", command: Command{AccountID: "account-1"}, want: ErrCharacterRequired},
		{name: "account", command: Command{CharacterID: "19"}, want: ErrAccountRequired},
		{name: "mismatch", command: Command{CharacterID: "19", AccountID: "account-2"}, want: ErrAccountMismatch},
	} {
		t.Run(test.name, func(t *testing.T) {
			if _, err := owner.Apply(context.Background(), test.command); !errors.Is(err, test.want) {
				t.Fatalf("err=%v want=%v", err, test.want)
			}
			assertEpicWishCharacter(t, loadEpicWishCharacter(t, repositories), []uint32{700})
		})
	}
}

func TestOwnerApplyHandlesNilAndCanceledContext(t *testing.T) {
	repositories := seededEpicWishRepositories(t, canonicalEpicWishStats([]uint32{700}))
	owner, err := NewOwner(repositories)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := owner.Apply(nil, Command{
		CharacterID: "19",
		AccountID:   "account-1",
		Items:       []uint32{701},
	}); err != nil {
		t.Fatalf("nil context err=%v", err)
	}

	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	if _, err := owner.Apply(ctx, Command{
		CharacterID: "19",
		AccountID:   "account-1",
		Items:       []uint32{702},
	}); !errors.Is(err, context.Canceled) {
		t.Fatalf("canceled context err=%v", err)
	}
	assertEpicWishCharacter(t, loadEpicWishCharacter(t, repositories), []uint32{701})
}

func TestOwnerApplyRollsBackAndReturnsNoUncommittedResult(t *testing.T) {
	repositories := seededEpicWishRepositories(t, canonicalEpicWishStats([]uint32{700}))
	wantErr := errors.New("reject epic wish commit")
	repositories.CharacterAssets = rejectEpicWishAssetsAfterApply{
		base: repositories.CharacterAssets,
		err:  wantErr,
	}
	owner, err := NewOwner(repositories)
	if err != nil {
		t.Fatal(err)
	}
	result, err := owner.Apply(context.Background(), Command{
		CharacterID: "19",
		AccountID:   "account-1",
		Items:       []uint32{800},
	})
	if !errors.Is(err, wantErr) || !reflect.DeepEqual(result, Result{}) {
		t.Fatalf("result=%+v err=%v", result, err)
	}
	assertEpicWishCharacter(t, loadEpicWishCharacter(t, repositories), []uint32{700})
}

func TestItemsFromCharacterRejectsCorruptPersistedState(t *testing.T) {
	for _, test := range []struct {
		name  string
		stats map[string]int64
	}{
		{name: "negative count", stats: map[string]int64{CountStat: -1}},
		{name: "oversized count", stats: map[string]int64{CountStat: MaximumItems + 1}},
		{name: "negative item", stats: map[string]int64{CountStat: 1, itemStatKeys[0]: -1}},
		{name: "oversized item", stats: map[string]int64{CountStat: 1, itemStatKeys[0]: int64(^uint32(0)) + 1}},
		{name: "duplicate item", stats: map[string]int64{CountStat: 2, itemStatKeys[0]: 9, itemStatKeys[1]: 9}},
	} {
		t.Run(test.name, func(t *testing.T) {
			if _, err := ItemsFromCharacter(dnfrepo.CharacterRecord{Stats: test.stats}); !errors.Is(err, ErrPersistedStateInvalid) {
				t.Fatalf("err=%v", err)
			}
		})
	}
	items, err := ItemsFromCharacter(dnfrepo.CharacterRecord{})
	if err != nil || items != nil {
		t.Fatalf("empty character items=%v err=%v", items, err)
	}
}

func seededEpicWishRepositories(t *testing.T, stats map[string]int64) dnfrepo.Group {
	t.Helper()
	repositories := dnfrepomemory.NewMemoryGroup()
	character := dnfrepo.CharacterRecord{
		CharacterID: "19",
		AccountID:   "account-1",
		Stats:       make(map[string]int64, len(stats)),
	}
	for key, value := range stats {
		character.Stats[key] = value
	}
	if err := repositories.Character.Save(context.Background(), character); err != nil {
		t.Fatal(err)
	}
	return repositories
}

func canonicalEpicWishStats(items []uint32) map[string]int64 {
	stats := map[string]int64{CountStat: int64(len(items))}
	for index, key := range itemStatKeys {
		if index < len(items) {
			stats[key] = int64(items[index])
		} else {
			stats[key] = 0
		}
	}
	return stats
}

func assertEpicWishCharacter(t *testing.T, character dnfrepo.CharacterRecord, want []uint32) {
	t.Helper()
	items, err := ItemsFromCharacter(character)
	if err != nil || !reflect.DeepEqual(items, want) {
		t.Fatalf("character=%+v items=%v want=%v err=%v", character, items, want, err)
	}
	if !hasCanonicalStats(character.Stats, want) {
		t.Fatalf("character stats are not canonical: %+v", character.Stats)
	}
}

func loadEpicWishCharacter(t *testing.T, repositories dnfrepo.Group) dnfrepo.CharacterRecord {
	t.Helper()
	character, found, err := repositories.Character.Load(context.Background(), "19")
	if err != nil || !found {
		t.Fatalf("load character found=%t err=%v", found, err)
	}
	return character
}

type countingEpicWishAssets struct {
	base  dnfrepo.CharacterAssetUnitOfWork
	saves *int
}

func (u countingEpicWishAssets) WithinCharacterAssets(
	ctx context.Context,
	characterID string,
	apply func(dnfrepo.CharacterRepository, dnfrepo.InventoryRepository, dnfrepo.EquipmentRepository) error,
) error {
	return u.base.WithinCharacterAssets(ctx, characterID, func(
		characters dnfrepo.CharacterRepository,
		inventory dnfrepo.InventoryRepository,
		equipment dnfrepo.EquipmentRepository,
	) error {
		return apply(countingEpicWishCharacters{CharacterRepository: characters, saves: u.saves}, inventory, equipment)
	})
}

type countingEpicWishCharacters struct {
	dnfrepo.CharacterRepository
	saves *int
}

func (r countingEpicWishCharacters) Save(ctx context.Context, character dnfrepo.CharacterRecord) error {
	*r.saves++
	return r.CharacterRepository.Save(ctx, character)
}

func (r countingEpicWishCharacters) SaveFields(
	ctx context.Context,
	character dnfrepo.CharacterRecord,
	fields ...dnfrepo.CharacterField,
) error {
	*r.saves++
	return dnfrepo.SaveCharacterFields(ctx, r.CharacterRepository, character, fields...)
}

type rejectEpicWishAssetsAfterApply struct {
	base dnfrepo.CharacterAssetUnitOfWork
	err  error
}

func (u rejectEpicWishAssetsAfterApply) WithinCharacterAssets(
	ctx context.Context,
	characterID string,
	apply func(dnfrepo.CharacterRepository, dnfrepo.InventoryRepository, dnfrepo.EquipmentRepository) error,
) error {
	return u.base.WithinCharacterAssets(ctx, characterID, func(
		characters dnfrepo.CharacterRepository,
		inventory dnfrepo.InventoryRepository,
		equipment dnfrepo.EquipmentRepository,
	) error {
		if err := apply(characters, inventory, equipment); err != nil {
			return err
		}
		return u.err
	})
}

var _ db.FieldStore[dnfrepo.CharacterRecord, dnfrepo.CharacterField] = countingEpicWishCharacters{}
