package mysql

import (
	"bytes"
	"context"
	"database/sql"
	"fmt"
	"maps"
	"os"
	"slices"
	"strings"
	"testing"
	"time"

	"longheng.io/server/internal/modules/dnf/repository"
)

// openLiveScratchGroup creates a throwaway schema on DNF_MYSQL_LIVE_DSN and a
// repository group over it; both are removed when the test ends.
func openLiveScratchGroup(t *testing.T) (repository.Group, *sql.DB, string) {
	t.Helper()
	dsn := strings.TrimSpace(os.Getenv("DNF_MYSQL_LIVE_DSN"))
	if dsn == "" {
		t.Skip("DNF_MYSQL_LIVE_DSN not set")
	}
	database, err := sql.Open("mysql", dsn)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { database.Close() })
	scratch := fmt.Sprintf("dnf_diff_rt_%d", time.Now().UnixNano())
	plan := repository.DatabasePlan{
		Feature: repository.FeatureRepository, ShardID: "9999", GroupID: "live",
		ReadDatabases: []string{scratch}, WriteDatabases: []string{scratch},
	}
	if err := EnsureMySQLSchema(context.Background(), SQLDBAdapter{DB: database}, SchemaOptions{
		AutoCreate: true, CreateDatabases: true, TablePrefix: "dnf", DatabasePlan: plan,
	}); err != nil {
		t.Fatalf("create scratch schema: %v", err)
	}
	t.Cleanup(func() {
		if _, err := database.ExecContext(context.Background(), "DROP DATABASE `"+scratch+"`"); err != nil {
			t.Errorf("drop scratch database %s: %v", scratch, err)
		}
	})
	group, err := NewMySQLGroupFromDB(database, MySQLGroupOptions{DatabasePlan: plan, TablePrefix: "dnf"})
	if err != nil {
		t.Fatal(err)
	}
	return group, database, scratch
}

func assertLiveItemStacks(t *testing.T, label string, got, want map[string]repository.ItemStack) {
	t.Helper()
	if len(got) != len(want) {
		t.Fatalf("%s: %d stacks, want %d: got=%v", label, len(got), len(want), got)
	}
	for key, w := range want {
		g, ok := got[key]
		if !ok || !itemStackStoredEqual(g, w) {
			t.Fatalf("%s: stack %s=%+v found=%t want %+v", label, key, g, ok, w)
		}
	}
}

// TestLiveMySQLDiffCollectionWrites covers the diffing bag / warehouse /
// equipment writers on a real server: every save is read back and must equal
// the full intended state (moves, count and extra edits, additions, removals,
// expiry set/cleared, collections kept apart, orphan extras not resurrected).
func TestLiveMySQLDiffCollectionWrites(t *testing.T) {
	group, database, scratch := openLiveScratchGroup(t)
	ctx := context.Background()
	const characterID = "4343"
	expire := time.Date(2030, 1, 2, 3, 4, 5, 123456789, time.UTC)

	bag := map[string]repository.ItemStack{}
	for index := range 150 {
		bag[fmt.Sprintf("0:%d", index)] = repository.ItemStack{
			ItemID: int64(5000 + index), Count: int64(index + 1), RawEntry: []byte{byte(index), 0, '\\', '\''},
			Extra: map[string]string{"pvf_path": fmt.Sprintf("stackable/%d.stk", index), "n": fmt.Sprint(index)},
		}
	}
	warehouse := map[string]repository.ItemStack{"0:1": {ItemID: 777, Count: 9, Extra: map[string]string{"w": "1"}}}
	save := func(label string, slots, ware map[string]repository.ItemStack) {
		t.Helper()
		if err := group.Inventory.Save(ctx, repository.InventoryRecord{CharacterID: characterID, Slots: slots, Warehouse: ware}); err != nil {
			t.Fatalf("%s: save inventory: %v", label, err)
		}
		loaded, found, err := group.Inventory.Load(ctx, characterID)
		if err != nil || !found {
			t.Fatalf("%s: load inventory found=%t err=%v", label, found, err)
		}
		assertLiveItemStacks(t, label+" slots", loaded.Slots, slots)
		assertLiveItemStacks(t, label+" warehouse", loaded.Warehouse, ware)
	}
	clone := func(values map[string]repository.ItemStack) map[string]repository.ItemStack {
		out := make(map[string]repository.ItemStack, len(values))
		for key, value := range values {
			value.Extra = maps.Clone(value.Extra)
			out[key] = value
		}
		return out
	}

	save("initial", bag, warehouse)
	save("unchanged resave", bag, warehouse)

	next := clone(bag)
	moved := next["0:10"]
	delete(next, "0:10")
	next["0:200"] = moved // move to an empty key
	next["0:11"], next["0:12"] = next["0:12"], next["0:11"]
	counted := next["0:20"]
	counted.Count = 99
	next["0:20"] = counted
	edited := next["0:21"]
	edited.Extra["n"] = "edited"
	delete(edited.Extra, "pvf_path")
	edited.Extra["new"] = "中文'\\"
	next["0:21"] = edited
	expiring := next["0:22"]
	expiring.ExpireAt = expire
	next["0:22"] = expiring
	delete(next, "0:30")
	next["0:201"] = repository.ItemStack{ItemID: 9999, Count: 1}
	nextWarehouse := clone(warehouse)
	nextWarehouse["0:1"] = repository.ItemStack{ItemID: 778, Count: 1, Extra: map[string]string{"w": "2"}}
	save("mixed edits", next, nextWarehouse)

	cleared := clone(next)
	stack := cleared["0:22"]
	stack.ExpireAt = time.Time{}
	stack.Extra = nil
	cleared["0:22"] = stack
	save("expiry and extras cleared", cleared, nextWarehouse)

	// An extra row whose item row is gone must not reappear on a new item
	// placed on the same key.
	if _, err := database.ExecContext(ctx, "INSERT INTO `"+scratch+"`.`dnf_inventory_item_extra` (character_id, collection_name, entry_key, extra_key, extra_value) VALUES (?, 'slots', '0:300', 'orphan', 'x')", characterID); err != nil {
		t.Fatalf("seed orphan extra: %v", err)
	}
	orphan := clone(cleared)
	orphan["0:300"] = repository.ItemStack{ItemID: 1234, Count: 1}
	save("orphan extra not resurrected", orphan, nextWarehouse)

	save("emptied", map[string]repository.ItemStack{}, map[string]repository.ItemStack{})

	equipment := map[string]repository.EquipmentEntry{}
	for index := range 30 {
		equipment[fmt.Sprint(index)] = repository.EquipmentEntry{
			SlotIndex: int16(index), ItemID: int64(100 + index), Bind: index%2 == 0, RawEntry: []byte{byte(index)},
			Extra: map[string]string{"a": fmt.Sprint(index), "b": "x"},
		}
	}
	saveEquipment := func(label string, entries map[string]repository.EquipmentEntry) {
		t.Helper()
		if err := group.Equipment.Save(ctx, repository.EquipmentRecord{CharacterID: characterID, Entries: entries}); err != nil {
			t.Fatalf("%s: save equipment: %v", label, err)
		}
		loaded, found, err := group.Equipment.Load(ctx, characterID)
		if err != nil || !found || len(loaded.Entries) != len(entries) {
			t.Fatalf("%s: load equipment found=%t err=%v entries=%d want %d", label, found, err, len(loaded.Entries), len(entries))
		}
		for key, want := range entries {
			if got, ok := loaded.Entries[key]; !ok || !equipmentEntryStoredEqual(got, want) {
				t.Fatalf("%s: entry %s=%+v found=%t want %+v", label, key, got, ok, want)
			}
		}
	}
	saveEquipment("equipment initial", equipment)
	nextEquipment := make(map[string]repository.EquipmentEntry, len(equipment))
	for key, value := range equipment {
		value.Extra = maps.Clone(value.Extra)
		nextEquipment[key] = value
	}
	swapped := nextEquipment["3"]
	swapped.SlotIndex, swapped.ItemID, swapped.ExpireAt = 26, 400990168, expire
	nextEquipment["3"] = swapped
	delete(nextEquipment["4"].Extra, "b")
	delete(nextEquipment, "5")
	nextEquipment["26"] = repository.EquipmentEntry{SlotIndex: 26, ItemID: 1, Extra: map[string]string{"pet": "1"}}
	saveEquipment("equipment edits", nextEquipment)
	saveEquipment("equipment emptied", map[string]repository.EquipmentEntry{})
}

// TestLiveMySQLDiffPetWrites covers the per-pet, per-part diffing pet writer on
// a real server; every save is read back and compared in stored form.
func TestLiveMySQLDiffPetWrites(t *testing.T) {
	group, database, scratch := openLiveScratchGroup(t)
	ctx := context.Background()
	const characterID = "4444"
	tokens := func(n int, prefix string) (map[string]bool, []string) {
		applied := make(map[string]bool, n)
		order := make([]string, 0, n)
		for index := range n {
			token := fmt.Sprintf("%s-%03d", prefix, index)
			applied[token] = index%2 == 0
			order = append(order, token)
		}
		return applied, order
	}
	pet := func(key string, item int64, tokenCount int) repository.PetEntry {
		applied, order := tokens(tokenCount, "clear"+key)
		return repository.PetEntry{
			PetKey: key, CreatureKey: 7, ItemID: item, SourceListType: 7, SourceSlotIndex: 1,
			Name: "宠物" + key, NameRaw: []byte{0, '\\', 0xff}, Satiety: 33, SatietyMicros: 33000000,
			Level: 12, Exp: 119, RawEntry: []byte{1, 2, 3}, Extra: map[string]string{"k": key, "q": "'"},
			AppliedClearTokens: applied, AppliedClearTokenOrder: order,
		}
	}
	clonePets := func(entries map[string]repository.PetEntry) map[string]repository.PetEntry {
		out := make(map[string]repository.PetEntry, len(entries))
		for key, entry := range entries {
			entry.Extra = maps.Clone(entry.Extra)
			entry.AppliedClearTokens = maps.Clone(entry.AppliedClearTokens)
			entry.AppliedClearTokenOrder = slices.Clone(entry.AppliedClearTokenOrder)
			out[key] = entry
		}
		return out
	}
	save := func(label string, entries map[string]repository.PetEntry) {
		t.Helper()
		if err := group.Pet.Save(ctx, repository.PetRecord{CharacterID: characterID, Entries: entries, EquippedKey: "1"}); err != nil {
			t.Fatalf("%s: save pets: %v", label, err)
		}
		loaded, found, err := group.Pet.Load(ctx, characterID)
		if err != nil || !found || len(loaded.Entries) != len(entries) {
			t.Fatalf("%s: load pets found=%t err=%v entries=%d want %d", label, found, err, len(loaded.Entries), len(entries))
		}
		for key, want := range entries {
			got, ok := loaded.Entries[key]
			if !ok || !petEntryRowStoredEqual(got, want) || !stringMapsEqual(got.Extra, want.Extra) ||
				!slices.Equal(petClearTokenRows(got), petClearTokenRows(want)) {
				t.Fatalf("%s: pet %s=%+v found=%t want %+v", label, key, got, ok, want)
			}
		}
	}

	entries := map[string]repository.PetEntry{"1": pet("1", 400990168, 150), "2": pet("2", 63023, 3), "3": pet("3", 100330838, 0)}
	save("initial", entries)
	save("unchanged resave", entries)

	next := clonePets(entries)
	satiety := next["1"]
	satiety.Satiety, satiety.SatietyMicros = 32, 32999999
	next["1"] = satiety
	save("satiety only", next)

	next = clonePets(next)
	edited := next["2"]
	edited.AppliedClearTokens["clear2-001"] = !edited.AppliedClearTokens["clear2-001"]
	edited.AppliedClearTokens["late"] = true // not in the order list: appended sorted
	edited.AppliedClearTokenOrder = []string{"clear2-002", "clear2-000", "clear2-002", "gone"}
	edited.Extra["k"] = "edited"
	delete(edited.Extra, "q")
	edited.SourceListType, edited.SourceSlotIndex = 3, 26
	next["2"] = edited
	save("tokens, order, extras and slot", next)

	next = clonePets(next)
	delete(next, "3")
	if _, err := database.ExecContext(ctx, "INSERT INTO `"+scratch+"`.`dnf_pet_clear_tokens` (character_id, pet_key, token_order, token, applied) VALUES (?, '9', 0, 'orphan', 1)", characterID); err != nil {
		t.Fatalf("seed orphan token: %v", err)
	}
	if _, err := database.ExecContext(ctx, "INSERT INTO `"+scratch+"`.`dnf_pet_entry_extra` (character_id, entry_key, extra_key, extra_value) VALUES (?, '9', 'orphan', 'x')", characterID); err != nil {
		t.Fatalf("seed orphan extra: %v", err)
	}
	fresh := pet("9", 555, 2)
	fresh.Extra = nil
	next["9"] = fresh
	save("remove one, add one over orphans", next)

	save("emptied", map[string]repository.PetEntry{})
}

// TestLiveMySQLDiffStringMapWrites covers the diffing key/value writer behind
// account metadata and settings on a real server.
func TestLiveMySQLDiffStringMapWrites(t *testing.T) {
	group, _, _ := openLiveScratchGroup(t)
	ctx := context.Background()
	const accountID = "dnf:live-diff"
	saveAccount := func(label string, metadata map[string]string) {
		t.Helper()
		if err := group.Account.Save(ctx, repository.AccountRecord{AccountID: accountID, Metadata: metadata}); err != nil {
			t.Fatalf("%s: save account: %v", label, err)
		}
		loaded, found, err := group.Account.Load(ctx, accountID)
		if err != nil || !found || !stringMapsEqual(loaded.Metadata, metadata) {
			t.Fatalf("%s: metadata=%v found=%t err=%v want %v", label, loaded.Metadata, found, err, metadata)
		}
	}
	metadata := map[string]string{}
	for index := range 150 {
		metadata[fmt.Sprintf("key_%03d", index)] = fmt.Sprintf("value %d '中文'\\", index)
	}
	saveAccount("initial", metadata)
	saveAccount("unchanged resave", metadata)
	next := maps.Clone(metadata)
	next["key_000"] = "changed"
	delete(next, "key_001")
	next["key_new"] = ""
	next["KEY_002"] = next["key_002"] // same key under the case-insensitive collation
	delete(next, "key_002")
	saveAccount("edits and case-only rename", next)
	saveAccount("emptied", map[string]string{})

	const scope = "live-diff-scope"
	for _, values := range []map[string]string{{"a": "1", "b": "2"}, {"a": "1", "b": "3", "c": "4"}, {"c": "4"}} {
		if err := group.Settings.Save(ctx, repository.SettingsRecord{Scope: scope, Values: values}); err != nil {
			t.Fatalf("save settings %v: %v", values, err)
		}
		loaded, found, err := group.Settings.Load(ctx, scope)
		if err != nil || !found || !stringMapsEqual(loaded.Values, values) {
			t.Fatalf("settings=%v found=%t err=%v want %v", loaded.Values, found, err, values)
		}
	}
}

// TestLiveMySQLBulkCollectionRoundTrip writes the three collections that moved
// from per-row INSERTs to mysqlBulkInserter (character stats, equipment,
// pets) to a real server through an interpolateParams DSN, reads them back and
// compares every field, including raw blobs holding all 256 byte values.
//
// It runs only when DNF_MYSQL_LIVE_DSN names a server, e.g.
//
//	root:pass@tcp(127.0.0.1:13306)/?parseTime=true&charset=utf8mb4,utf8&interpolateParams=true
//
// and works in a throwaway database that it creates and drops itself.
func TestLiveMySQLBulkCollectionRoundTrip(t *testing.T) {
	dsn := strings.TrimSpace(os.Getenv("DNF_MYSQL_LIVE_DSN"))
	if dsn == "" {
		t.Skip("DNF_MYSQL_LIVE_DSN not set")
	}
	ctx := context.Background()
	database, err := sql.Open("mysql", dsn)
	if err != nil {
		t.Fatal(err)
	}
	// Registered first so it runs last, after the scratch database is dropped.
	t.Cleanup(func() { database.Close() })
	scratch := fmt.Sprintf("dnf_bulk_rt_%d", time.Now().UnixNano())
	plan := repository.DatabasePlan{
		Feature: repository.FeatureRepository, ShardID: "9999", GroupID: "live",
		ReadDatabases: []string{scratch}, WriteDatabases: []string{scratch},
	}
	if err := EnsureMySQLSchema(ctx, SQLDBAdapter{DB: database}, SchemaOptions{
		AutoCreate: true, CreateDatabases: true, TablePrefix: "dnf", DatabasePlan: plan,
	}); err != nil {
		t.Fatalf("create scratch schema: %v", err)
	}
	t.Cleanup(func() {
		if _, err := database.ExecContext(context.Background(), "DROP DATABASE `"+scratch+"`"); err != nil {
			t.Errorf("drop scratch database %s: %v", scratch, err)
		}
	})
	group, err := NewMySQLGroupFromDB(database, MySQLGroupOptions{DatabasePlan: plan, TablePrefix: "dnf"})
	if err != nil {
		t.Fatal(err)
	}

	blob := make([]byte, 256)
	for index := range blob {
		blob[index] = byte(index)
	}
	const characterID = "4242"

	// More rows than one mysqlReplaceBatchSize chunk, so the flush-at-limit
	// path and the final flush both run.
	stats := map[string]int64{}
	for index := range 300 {
		stats[fmt.Sprintf("item_daily_used_%d", 1000+index)] = int64(index) - 150
	}
	if err := group.Character.Save(ctx, repository.CharacterRecord{CharacterID: characterID, AccountID: "acc", Name: "rt", Stats: stats}); err != nil {
		t.Fatalf("save character: %v", err)
	}
	loaded, found, err := group.Character.Load(ctx, characterID)
	if err != nil || !found {
		t.Fatalf("load character found=%t err=%v", found, err)
	}
	for key, want := range stats {
		if got := loaded.Stats[key]; got != want {
			t.Fatalf("stat %s=%d want %d", key, got, want)
		}
	}

	equipment := repository.EquipmentRecord{CharacterID: characterID, Entries: map[string]repository.EquipmentEntry{}}
	for index := range 200 {
		equipment.Entries[fmt.Sprintf("e%03d", index)] = repository.EquipmentEntry{
			SlotIndex: int16(index), ItemID: int64(100000 + index), Bind: index%2 == 0,
			RawEntry: blob, Extra: map[string]string{"a": "中文'\\\"", "b": fmt.Sprint(index)},
		}
	}
	if err := group.Equipment.Save(ctx, equipment); err != nil {
		t.Fatalf("save equipment: %v", err)
	}
	loadedEquipment, found, err := group.Equipment.Load(ctx, characterID)
	if err != nil || !found || len(loadedEquipment.Entries) != len(equipment.Entries) {
		t.Fatalf("load equipment found=%t err=%v entries=%d", found, err, len(loadedEquipment.Entries))
	}
	for key, want := range equipment.Entries {
		got := loadedEquipment.Entries[key]
		if got.SlotIndex != want.SlotIndex || got.ItemID != want.ItemID || got.Bind != want.Bind ||
			!bytes.Equal(got.RawEntry, want.RawEntry) || got.Extra["a"] != want.Extra["a"] || got.Extra["b"] != want.Extra["b"] {
			t.Fatalf("equipment %s=%+v want %+v", key, got, want)
		}
	}

	tokens := map[string]bool{}
	order := make([]string, 0, 150)
	for index := range 150 {
		token := fmt.Sprintf("clear-%03d", index)
		tokens[token] = index%3 != 0
		order = append(order, token)
	}
	pet := repository.PetRecord{CharacterID: characterID, Entries: map[string]repository.PetEntry{
		"1": {PetKey: "1", CreatureKey: 7, ItemID: 400990168, Name: "宠物", NameRaw: blob, Satiety: 25, SatietyMicros: 25693982,
			Level: 10, Exp: 82, RawEntry: blob, Extra: map[string]string{"k": "v"},
			AppliedClearTokens: tokens, AppliedClearTokenOrder: order},
	}}
	if err := group.Pet.Save(ctx, pet); err != nil {
		t.Fatalf("save pet: %v", err)
	}
	loadedPet, found, err := group.Pet.Load(ctx, characterID)
	if err != nil || !found {
		t.Fatalf("load pet found=%t err=%v", found, err)
	}
	got := loadedPet.Entries["1"]
	want := pet.Entries["1"]
	if got.Name != want.Name || !bytes.Equal(got.NameRaw, blob) || !bytes.Equal(got.RawEntry, blob) ||
		got.Satiety != want.Satiety || got.SatietyMicros != want.SatietyMicros || got.Level != want.Level ||
		got.Exp != want.Exp || got.Extra["k"] != "v" || len(got.AppliedClearTokens) != len(tokens) {
		t.Fatalf("pet=%+v", got)
	}
	for index, token := range order {
		if got.AppliedClearTokenOrder[index] != token || got.AppliedClearTokens[token] != tokens[token] {
			t.Fatalf("token %d: order=%q applied=%t want %q %t", index, got.AppliedClearTokenOrder[index], got.AppliedClearTokens[token], token, tokens[token])
		}
	}
}
