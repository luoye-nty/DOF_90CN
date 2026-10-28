package wedding

import (
	"context"
	"encoding/binary"
	"errors"
	modulepvf "longheng.io/server/internal/modules/dnf/pvf"
	"longheng.io/server/internal/modules/dnf/quest"
	"os"
	"reflect"
	"testing"
	"time"

	repo "longheng.io/server/internal/modules/dnf/repository"
	"longheng.io/server/internal/modules/dnf/repository/memory"
	platformpvf "longheng.io/server/internal/platform/pvf"
)

func TestWeddingCeremonyAssetsAndReplay(t *testing.T) {
	ctx := context.Background()
	now := time.Unix(1788854400, 0)
	cat := &Catalog{MinimumLevel: 30, RemarryDays: 1, WeddingDays: 1, Alimony: [2]int64{500000, 500000}, MaxLove: 999, LoveItems: map[int64]int64{10002901: 1}, QuestIDs: [2]int64{20737, 20738}, Delays: [3]int64{300, 600, 900}, EndSeconds: 60, PaidType: 2, PaidTicket: Item{2683401, 1}}
	cat.Ceremonies[0] = Ceremony{Gold: 500000, Ring: 0, Gift: Item{10002629, 1}}
	cat.Ceremonies[2] = Ceremony{Ring: 2, Gift: Item{10002631, 1}}
	cat.RingTickets[1] = Item{10002611, 1}
	for _, mode := range []string{"complete", "missing_quest", "gold", "full_bag", "paid_ticket_missing", "paid", "rollback"} {
		t.Run(mode, func(t *testing.T) {
			repos := memory.NewMemoryGroup()
			for _, id := range []string{"1", "2"} {
				job := "0"
				if id == "2" {
					job = "1"
				}
				gold := int64(600000)
				if mode == "gold" {
					gold = 499999
				}
				if err := repos.Character.Save(ctx, repo.CharacterRecord{CharacterID: id, AccountID: "a" + id, Job: job, Level: 30, Stats: map[string]int64{"gold": gold}}); err != nil {
					t.Fatal(err)
				}
				if err := repos.Inventory.Save(ctx, repo.InventoryRecord{CharacterID: id, Slots: map[string]repo.ItemStack{"0:3": {ItemID: 2683401, Count: 1}, "0:4": {ItemID: 10002611, Count: 1}, "0:5": {ItemID: 10002901, Count: 2, RawEntry: make([]byte, 119), Extra: map[string]string{"quantity": "2"}}}}); err != nil {
					t.Fatal(err)
				}
				q := repo.QuestRecord{CharacterID: id, States: map[int64]repo.QuestState{20737: {Status: "completed"}, 20738: {Status: "completed"}}}
				if mode == "missing_quest" && id == "2" {
					delete(q.States, 20738)
				}
				if err := repos.Quest.Save(ctx, q); err != nil {
					t.Fatal(err)
				}
			}
			cmd := Command{ActorID: "1", PartnerID: "2", AccountID: "a1", PartnerAccountID: "a2", Operation: 1, Now: now}
			if _, err := Commit(ctx, repos, cat, cmd); err != nil {
				t.Fatal(err)
			}
			cmd.Operation, cmd.Room = 2, 8
			cmd.Grant = func(bag *repo.InventoryRecord, item Item) error {
				if mode == "full_bag" && bag.CharacterID == "2" {
					return errors.New("bag full")
				}
				bag.Slots["0:6"] = repo.ItemStack{ItemID: item.ID, Count: item.Count}
				return nil
			}
			if mode == "paid" || mode == "paid_ticket_missing" {
				cmd.Kind = 2
			}
			if mode == "paid_ticket_missing" {
				bag, _, _ := repos.Inventory.Load(ctx, "1")
				delete(bag.Slots, "0:3")
				if err := repos.Inventory.Save(ctx, bag); err != nil {
					t.Fatal(err)
				}
			}
			beforeC, _, _ := repos.Character.Load(ctx, "1")
			beforeB, _, _ := repos.Inventory.Load(ctx, "1")
			if mode == "rollback" {
				repos.CharacterTrade = failingWeddingTrade{repos.CharacterTrade}
			}
			result, err := Commit(ctx, repos, cat, cmd)
			if mode != "complete" && mode != "paid" {
				if err == nil {
					t.Fatal("invalid reservation committed")
				}
				a, _, _ := repos.Character.Load(ctx, "1")
				bag, _, _ := repos.Inventory.Load(ctx, "1")
				if !reflect.DeepEqual(a, beforeC) || !reflect.DeepEqual(bag, beforeB) {
					t.Fatal("failed reservation charged assets")
				}
				return
			}
			if err != nil {
				t.Fatal(err)
			}
			wantGold := int64(100000)
			if mode == "paid" {
				wantGold = 600000
				if _, ok := result.Inventories[0].Slots["0:3"]; ok {
					t.Fatal("ticket not consumed")
				}
			}
			if result.Characters[0].Stats["gold"] != wantGold || result.Characters[1].Stats["gold"] != 600000 {
				t.Fatal("wrong payment")
			}
			if _, err := Commit(ctx, repos, cat, cmd); err == nil {
				t.Fatal("reservation replay charged again")
			}
			cmd.Operation = 3
			if _, err := Commit(ctx, repos, cat, cmd); err == nil {
				t.Fatal("early ceremony")
			}
			cmd.Now = now.Add(300 * time.Second)
			if _, err := Commit(ctx, repos, cat, cmd); err != nil {
				t.Fatal(err)
			}
			cmd.Operation = 4
			if _, err := Commit(ctx, repos, cat, cmd); err == nil {
				t.Fatal("early completion")
			}
			cmd.Now = now.Add(360 * time.Second)
			result, err = Commit(ctx, repos, cat, cmd)
			if err != nil {
				t.Fatal(err)
			}
			for i, c := range result.Characters {
				state, e := Read(c)
				if e != nil || state.Status != 4 || state.Room != 0 || state.Date != cmd.Now.Unix() || result.Inventories[i].Slots["0:6"].ItemID != cat.Ceremonies[cmd.Kind].Gift.ID {
					t.Fatalf("completion: %+v %v", result, e)
				}
			}
			if _, err := Commit(ctx, repos, cat, cmd); err == nil {
				t.Fatal("completion replay granted again")
			}
			if mode == "paid" {
				return
			}
			cmd.Operation, cmd.Slot, cmd.Ring = 8, 4, 1
			result, err = Commit(ctx, repos, cat, cmd)
			if err != nil {
				t.Fatal(err)
			}
			if result.Characters[1].Stats["wedding_ring"] != 1 {
				t.Fatal("partner ring stale")
			}
			cmd.Operation, cmd.Slot = 9, 5
			result, err = Commit(ctx, repos, cat, cmd)
			if err != nil {
				t.Fatal(err)
			}
			stack := result.Inventories[0].Slots["0:5"]
			if stack.Count != 1 || stack.Extra["quantity"] != "1" || binary.LittleEndian.Uint32(stack.RawEntry[6:10]) != 1 || result.Characters[1].Stats["wedding_love"] != 1 {
				t.Fatal("love item projection mismatch")
			}
			cat.MaxLove = 1
			if _, err := Commit(ctx, repos, cat, cmd); err == nil {
				t.Fatal("overflow consumed love item")
			}
			cat.MaxLove = 999
			cmd.Operation = 7
			if _, err := Commit(ctx, repos, cat, cmd); err == nil {
				t.Fatal("divorce without sufficient gold")
			}
		})
	}
}

type failingWeddingTrade struct{ repo.CharacterTradeUnitOfWork }

func (u failingWeddingTrade) WithinCharacterTrade(ctx context.Context, a, b string, apply func(repo.CharacterRepository, repo.InventoryRepository) error) error {
	return u.CharacterTradeUnitOfWork.WithinCharacterTrade(ctx, a, b, func(chars repo.CharacterRepository, bags repo.InventoryRepository) error {
		if err := apply(chars, bags); err != nil {
			return err
		}
		return errors.New("injected commit failure")
	})
}

func TestWeddingReciprocalTransaction(t *testing.T) {
	ctx := context.Background()
	now := time.Unix(1788854400, 0)
	cat := &Catalog{MinimumLevel: 30, RemarryDays: 1, Alimony: [2]int64{500000, 500000}, MaxLove: 999, LoveItems: map[int64]int64{10002901: 1}}
	cat.RingTickets[1] = Item{10002611, 1}
	for _, failure := range []bool{false, true} {
		t.Run(map[bool]string{false: "commit", true: "rollback"}[failure], func(t *testing.T) {
			repos := memory.NewMemoryGroup()
			initial := []repo.CharacterRecord{
				{CharacterID: "1", AccountID: "dnf:1", Job: "0", Level: 30, Stats: map[string]int64{"gold": 600000}},
				{CharacterID: "2", AccountID: "dnf:2", Job: "1", Level: 30, Stats: map[string]int64{"gold": 600000}},
			}
			for _, c := range initial {
				if err := repos.Character.Save(ctx, c); err != nil {
					t.Fatal(err)
				}
			}
			if failure {
				repos.CharacterTrade = failingWeddingTrade{repos.CharacterTrade}
			}
			cmd := Command{ActorID: "1", PartnerID: "2", AccountID: "dnf:1", PartnerAccountID: "dnf:2", Operation: 1, Now: now}
			result, err := Commit(ctx, repos, cat, cmd)
			if failure {
				if err == nil {
					t.Fatal("injected failure accepted")
				}
				for _, want := range initial {
					got, _, err := repos.Character.Load(ctx, want.CharacterID)
					if err != nil || !reflect.DeepEqual(got, want) {
						t.Fatalf("partial write: %+v %v", got, err)
					}
				}
				return
			}
			if err != nil {
				t.Fatal(err)
			}
			a, _ := Read(result.Characters[0])
			b, _ := Read(result.Characters[1])
			if a.Status != 1 || b.Status != 1 || a.PartnerID != 2 || b.PartnerID != 1 || a.PairID != b.PairID {
				t.Fatalf("inconsistent pair: %+v %+v", a, b)
			}
			if _, err := Commit(ctx, repos, cat, cmd); err == nil {
				t.Fatal("replayed engagement committed")
			}
			cmd.Operation = 5
			if _, err := Commit(ctx, repos, cat, cmd); err != nil {
				t.Fatal(err)
			}
			for _, c := range initial {
				got, _, _ := repos.Character.Load(ctx, c.CharacterID)
				state, e := Read(got)
				if e != nil || state.Status != 0 || state.PartnerID != 0 || got.Stats["gold"] != 600000 {
					t.Fatalf("breakup: %+v %v", got, e)
				}
			}
		})
	}
	for _, alter := range []func(*repo.CharacterRecord){
		func(c *repo.CharacterRecord) { c.Level = 29 },
		func(c *repo.CharacterRecord) { c.Job = "0" },
		func(c *repo.CharacterRecord) { c.CharacterID = "1" },
		func(c *repo.CharacterRecord) { c.Stats = map[string]int64{"delete_flag": 1} },
		func(c *repo.CharacterRecord) { c.Stats = map[string]int64{"wedding_divorced_at": now.Unix() - 60} },
	} {
		a := repo.CharacterRecord{CharacterID: "1", Job: "0", Level: 30}
		b := repo.CharacterRecord{CharacterID: "2", Job: "1", Level: 30}
		alter(&b)
		if err := Validate(a, b, cat, 1, now); err == nil {
			t.Fatalf("invalid proposal accepted: %+v", b)
		}
	}
}

func TestWeddingRuntimePVF(t *testing.T) {
	path := os.Getenv("DNF90_PVF_PATH")
	if path == "" {
		t.Skip("set DNF90_PVF_PATH for runtime wedding catalog validation")
	}
	archive, err := platformpvf.Open(path)
	if err != nil {
		t.Fatal(err)
	}
	c, err := LoadCatalog(archive)
	if err != nil {
		t.Fatal(err)
	}
	if c.MinimumLevel != 30 || c.MaxLove != 999 || c.Alimony != [2]int64{500000, 500000} || c.Delays != [3]int64{300, 600, 900} || c.PaidTicket != (Item{2683401, 1}) || c.Ceremonies[0].Gold != 500000 || c.Ceremonies[1].Gold != 2000000 || c.RingTickets[5].ID != 10002615 || c.LoveItems[10002901] != 1 {
		t.Fatalf("catalog mismatch: %+v", c)
	}
	index, err := modulepvf.Build(context.Background(), archive, modulepvf.BuildOptions{Lists: []string{quest.DefaultList}})
	if err != nil {
		t.Fatal(err)
	}
	quests, err := quest.Load(context.Background(), index)
	if err != nil {
		t.Fatal(err)
	}
	eligibility := quest.CharacterEligibility{Level: 30, Job: 0, Wedding: true}
	first, err := quests.PlanAccept(eligibility, repo.QuestRecord{CharacterID: "1"}, 20737)
	if err != nil || first.InitTrigger != 5 {
		t.Fatalf("real wedding quest 1: %+v %v", first, err)
	}
	second, err := quests.PlanAccept(eligibility, repo.QuestRecord{CharacterID: "1", States: map[int64]repo.QuestState{20737: {Status: "completed"}}}, 20738)
	if err != nil || second.QuestID != 20738 {
		t.Fatalf("real wedding quest 2: %+v %v", second, err)
	}
}
