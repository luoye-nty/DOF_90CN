package wedding

import (
	"context"
	"errors"
	"os"
	"reflect"
	"testing"
	"time"

	repo "longheng.io/server/internal/modules/dnf/repository"
	"longheng.io/server/internal/modules/dnf/repository/memory"
	platformpvf "longheng.io/server/internal/platform/pvf"
)

func TestWeddingRoomSharedAssetsAndRollback(t *testing.T) {
	ctx := context.Background()
	now := time.Date(2026, 9, 8, 2, 0, 0, 0, time.UTC)
	repos := memory.NewMemoryGroup()
	for _, id := range []string{"1", "2"} {
		partner := int64(2)
		if id == "2" {
			partner = 1
		}
		if err := repos.Character.Save(ctx, repo.CharacterRecord{CharacterID: id, AccountID: "a" + id, Stats: map[string]int64{"wedding_state": 4, "wedding_id": 65538, "wedding_partner_id": partner, "wedding_love": 300, "gold": 1000}}); err != nil {
			t.Fatal(err)
		}
		if err := repos.Inventory.Save(ctx, repo.InventoryRecord{CharacterID: id, Slots: map[string]repo.ItemStack{"0:3": {ItemID: 10002766, Count: 2}, "0:4": {ItemID: 10002765, Count: 10}}}); err != nil {
			t.Fatal(err)
		}
	}
	rules := &RoomCatalog{Seeds: map[int64]int64{10002766: 1}, Trees: map[int64]Tree{1: {Days: 30, DailyWater: 2, MaxGrowth: 30, Water: map[int64]int64{10002765: 10}, Rewards: []TreeReward{{Item{10002901, 2}, 1}}}}, Positions: map[int64]map[int]bool{10002694: {0: true}}}
	cat := &Catalog{}
	cmd := RoomCommand{ActorID: "1", PartnerID: "2", AccountID: "a1", Now: now}
	apply := func() RoomResult {
		t.Helper()
		result, err := CommitRoom(ctx, repos, cat, rules, cmd)
		if err != nil {
			t.Fatal(err)
		}
		return result
	}
	cmd.Opcode, cmd.ItemID, cmd.Count, cmd.Materials = 21, 10002694, 1, []Item{{10002606, 100}}
	r := apply()
	if r.Room.Cargo[0] != (Item{10002694, 1}) || r.Characters[0].Stats["wedding_love"] != 200 || r.Characters[1].Stats["wedding_love"] != 200 {
		t.Fatalf("purchase: %+v", r)
	}
	cmd.Opcode, cmd.From, cmd.FromSlot, cmd.FromItem, cmd.To, cmd.ToSlot = 1046, 1, 0, 10002694, 0, 0
	r = apply()
	if r.Room.Installed[0] != 10002694 || r.Room.Cargo[0] != (Item{}) {
		t.Fatalf("install: %+v", r.Room)
	}
	cmd.ActorID, cmd.PartnerID, cmd.AccountID = "2", "1", "a2"
	cmd.From, cmd.To, cmd.ToSlot, cmd.ToItem = 0, 1, 3, 10002694
	r = apply()
	if r.Room.Installed[0] != 0 || r.Room.Cargo[3] != (Item{10002694, 1}) {
		t.Fatalf("partner retrieve: %+v", r.Room)
	}
	cmd.Opcode, cmd.Slot, cmd.ItemID = 1048, 3, 10002766
	r = apply()
	if r.Room.Tree != 1 || r.Room.Expires != now.Unix()+30*86400 || r.Inventories[0].Slots["0:3"].Count != 1 {
		t.Fatalf("plant: %+v", r.Room)
	}
	cmd.Opcode, cmd.Slot, cmd.ItemID = 1049, 4, 10002765
	apply()
	r = apply()
	if r.Room.Growth != 20 || r.Room.WaterCount != 2 {
		t.Fatalf("water: %+v", r.Room)
	}
	if _, err := CommitRoom(ctx, repos, cat, rules, cmd); err != Error(-6) {
		t.Fatalf("daily water limit must use native daily-limit message, got %v", err)
	}
	stored, _, _ := repos.Character.Load(ctx, "1")
	waterBag, _, _ := repos.Inventory.Load(ctx, cmd.ActorID)
	if stored.Stats["wedding_house_growth"] != 20 || stored.Stats["wedding_house_water"] != 2 || waterBag.Slots["0:4"].Count != 8 {
		t.Fatal("rejected watering changed growth, daily count or water items")
	}
	cmd.Now = now.Add(24 * time.Hour)
	r = apply()
	if r.Room.Growth != 30 || r.Room.WaterCount != 1 {
		t.Fatalf("next day: %+v", r.Room)
	}
	if _, err := CommitRoom(ctx, repos, cat, rules, cmd); err != Error(-9) {
		t.Fatalf("mature tree must use native sufficient-water message, got %v", err)
	}
	cmd.Opcode = 1050
	cmd.Grant = func(b *repo.InventoryRecord, item Item) error {
		b.Slots["0:5"] = repo.ItemStack{ItemID: item.ID, Count: item.Count}
		if b.CharacterID == "1" {
			return errors.New("partner bag full")
		}
		return nil
	}
	before, _, _ := repos.Inventory.Load(ctx, "2")
	if _, err := CommitRoom(ctx, repos, cat, rules, cmd); err == nil {
		t.Fatal("partial reward committed")
	}
	after, _, _ := repos.Inventory.Load(ctx, "2")
	if !reflect.DeepEqual(before, after) {
		t.Fatal("failed harvest changed actor inventory")
	}
	cmd.Grant = func(b *repo.InventoryRecord, item Item) error {
		b.Slots["0:5"] = repo.ItemStack{ItemID: item.ID, Count: item.Count}
		return nil
	}
	r = apply()
	if r.Room.Growth != 0 || r.Room.Tree != 1 {
		t.Fatalf("harvest: %+v", r.Room)
	}
	for _, b := range r.Inventories {
		if b.Slots["0:5"].ItemID != 10002901 || b.Slots["0:5"].Count != 2 {
			t.Fatalf("reward: %+v", b)
		}
	}
	if _, err := CommitRoom(ctx, repos, cat, rules, cmd); err == nil {
		t.Fatal("repeated harvest granted rewards")
	}
	cmd.Opcode = 1045
	cmd.AccountID = "a1"
	if _, err := CommitRoom(ctx, repos, cat, rules, cmd); err == nil {
		t.Fatal("foreign account admitted")
	}
	cmd.AccountID = "a2"
	cmd.Now = now.Add(31 * 24 * time.Hour)
	if expired := apply(); expired.Room.Tree != 0 {
		t.Fatal("expired tree blocks new planting")
	}
	cmd.Opcode, cmd.Slot, cmd.ItemID = 1048, 3, 10002766
	repos.CharacterTrade = failingWeddingTrade{repos.CharacterTrade}
	if _, err := CommitRoom(ctx, repos, cat, rules, cmd); err == nil {
		t.Fatal("save failure accepted")
	}
	after, _, _ = repos.Inventory.Load(ctx, "2")
	if after.Slots["0:3"].Count != 1 {
		t.Fatal("save failure consumed seed")
	}
}

func TestWeddingRoomRuntimePVF(t *testing.T) {
	path := os.Getenv("DNF90_PVF_PATH")
	if path == "" {
		t.Skip("set DNF90_PVF_PATH for room catalog validation")
	}
	a, err := platformpvf.Open(path)
	if err != nil {
		t.Fatal(err)
	}
	c, err := LoadRoomCatalog(a)
	if err != nil {
		t.Fatal(err)
	}
	if len(c.Trees) != 3 || c.Seeds[10002766] != 1 || c.Seeds[10002768] != 2 || c.Seeds[10002767] != 3 || len(c.Positions[10002694]) == 0 {
		t.Fatalf("catalog: %+v", c)
	}
	for _, tree := range c.Trees {
		if tree.Days != 30 || tree.DailyWater != 2 || tree.MaxGrowth != 30 || tree.Water[10002765] != 10 || tree.Water[10002905] != 20 || len(tree.Rewards) != 6 {
			t.Fatalf("tree: %+v", tree)
		}
	}
}
