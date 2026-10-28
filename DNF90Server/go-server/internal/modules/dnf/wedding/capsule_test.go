package wedding

import (
	"context"
	"errors"
	"reflect"
	"strconv"
	"sync"
	"testing"
	"time"

	repo "longheng.io/server/internal/modules/dnf/repository"
	"longheng.io/server/internal/modules/dnf/repository/memory"
)

func capsuleTestPair(t *testing.T) (repo.Group, *Catalog, *CapsuleCatalog, CapsuleCommand) {
	t.Helper()
	repos := memory.NewMemoryGroup()
	cat := &Catalog{MaxLove: 999, MinimumLevel: 30}
	cat.RingTickets[3] = Item{10002613, 1}
	rules := &CapsuleCatalog{MaxOwn: 10, MaxTaken: 10, Gauge: 2, MaxGauge: 10, ItemID: 10002910}
	now := time.Unix(1789200000, 0)
	for i := 1; i <= 2; i++ {
		id := strconv.Itoa(i)
		c := repo.CharacterRecord{CharacterID: id, AccountID: "a" + id, Level: 30, Stats: map[string]int64{"gold": 9000000}}
		store(&c, State{PairID: 65538, PartnerID: uint16(3 - i), Status: 4, Ring: 2, Love: 946, Date: now.Unix() - 86400}, now)
		if err := repos.Character.Save(context.Background(), c); err != nil {
			t.Fatal(err)
		}
		if err := repos.Inventory.Save(context.Background(), repo.InventoryRecord{CharacterID: id, Slots: map[string]repo.ItemStack{}}); err != nil {
			t.Fatal(err)
		}
	}
	cmd := CapsuleCommand{ActorID: "1", PartnerID: "2", AccountID: "a1", Opcode: 1052, Now: now, Grant: func(b *repo.InventoryRecord, item Item) error {
		if b.Slots == nil {
			b.Slots = make(map[string]repo.ItemStack)
		}
		stack := b.Slots["0:57"]
		stack.ItemID = item.ID
		stack.Count += item.Count
		b.Slots["0:57"] = stack
		return nil
	}}
	return repos, cat, rules, cmd
}

func TestWeddingCapsuleWorkflowAndRepeatedClaims(t *testing.T) {
	ctx := context.Background()
	repos, cat, rules, cmd := capsuleTestPair(t)
	for made := 0; made < 2; made++ {
		for clear := 0; clear < 5; clear++ {
			c, _, _ := repos.Character.Load(ctx, "1")
			if err := AccumulateCapsuleClear(&c, rules); err != nil {
				t.Fatal(err)
			}
			if err := repos.Character.Save(ctx, c); err != nil {
				t.Fatal(err)
			}
			if clear < 4 {
				if _, err := CommitCapsule(ctx, repos, cat, rules, cmd); !errors.Is(err, ErrCapsuleEnergy) {
					t.Fatalf("early make: %v", err)
				}
			}
		}
		res, err := CommitCapsule(ctx, repos, cat, rules, cmd)
		if err != nil || res.States[0].Progress != 0 || res.States[0].Own != int64(made+1) {
			t.Fatalf("make=%+v %v", res.States, err)
		}
	}
	cmd.Opcode = 1053
	res, err := CommitCapsule(ctx, repos, cat, rules, cmd)
	if err != nil || res.States[0].Own != 0 || res.States[1].Taken != 2 {
		t.Fatalf("give=%+v %v", res.States, err)
	}
	if _, err := CommitCapsule(ctx, repos, cat, rules, cmd); !errors.Is(err, ErrCapsuleEmpty) {
		t.Fatalf("repeat give=%v", err)
	}
	// The persisted pending stock survives the next day and has no online-session dependency.
	cmd.ActorID, cmd.PartnerID, cmd.AccountID, cmd.Opcode, cmd.Now = "2", "1", "a2", 1054, cmd.Now.Add(48*time.Hour)
	var wg sync.WaitGroup
	errs := make(chan error, 2)
	for i := 0; i < 2; i++ {
		wg.Add(1)
		go func() { defer wg.Done(); _, err := CommitCapsule(ctx, repos, cat, rules, cmd); errs <- err }()
	}
	wg.Wait()
	close(errs)
	success := 0
	for err := range errs {
		if err == nil {
			success++
		} else if !errors.Is(err, ErrCapsuleEmpty) {
			t.Fatal(err)
		}
	}
	bag, _, _ := repos.Inventory.Load(ctx, "2")
	c, _, _ := repos.Character.Load(ctx, "2")
	state, _ := ReadCapsule(c, rules)
	if success != 1 || bag.Slots["0:57"].Count != 2 || state.Taken != 0 {
		t.Fatalf("claims success=%d bag=%+v state=%+v", success, bag, state)
	}
}

func TestWeddingCapsuleFailureRollbackAndRelationshipReset(t *testing.T) {
	ctx := context.Background()
	for _, mode := range []string{"make_limit", "give_limit", "full_bag", "rollback", "bad_pair", "deleted", "wrong_account"} {
		t.Run(mode, func(t *testing.T) {
			repos, cat, rules, cmd := capsuleTestPair(t)
			a, _, _ := repos.Character.Load(ctx, "1")
			b, _, _ := repos.Character.Load(ctx, "2")
			storeCapsule(&a, CapsuleState{Progress: 10, Own: 2, Taken: 2})
			switch mode {
			case "make_limit":
				storeCapsule(&a, CapsuleState{Progress: 10, Own: 10})
			case "give_limit":
				cmd.Opcode = 1053
				storeCapsule(&b, CapsuleState{Taken: 9})
			case "full_bag":
				cmd.Opcode = 1054
				cmd.Grant = func(*repo.InventoryRecord, Item) error { return errors.New("full") }
			case "rollback":
				cmd.Opcode = 1054
				repos.CharacterTrade = failingWeddingTrade{repos.CharacterTrade}
			case "bad_pair":
				b.Stats["wedding_partner_id"] = 3
			case "deleted":
				b.Stats["delete_flag"] = 1
			case "wrong_account":
				cmd.AccountID = "wrong"
			}
			if err := repos.Character.Save(ctx, a); err != nil {
				t.Fatal(err)
			}
			if err := repos.Character.Save(ctx, b); err != nil {
				t.Fatal(err)
			}
			bag, _, _ := repos.Inventory.Load(ctx, "1")
			if _, err := CommitCapsule(ctx, repos, cat, rules, cmd); err == nil {
				t.Fatal("invalid operation succeeded")
			}
			afterA, _, _ := repos.Character.Load(ctx, "1")
			afterB, _, _ := repos.Character.Load(ctx, "2")
			afterBag, _, _ := repos.Inventory.Load(ctx, "1")
			if !reflect.DeepEqual(a, afterA) || !reflect.DeepEqual(b, afterB) || !reflect.DeepEqual(bag, afterBag) {
				t.Fatal("failure changed assets")
			}
		})
	}
	repos, cat, rules, cmd := capsuleTestPair(t)
	a, _, _ := repos.Character.Load(ctx, "1")
	storeCapsule(&a, CapsuleState{10, 2, 3})
	if err := repos.Character.Save(ctx, a); err != nil {
		t.Fatal(err)
	}
	res, err := Commit(ctx, repos, cat, Command{ActorID: "1", PartnerID: "2", AccountID: "a1", Operation: 7, Now: cmd.Now})
	if err != nil {
		t.Fatal(err)
	}
	for _, c := range res.Characters {
		if state, err := ReadCapsule(c, rules); err != nil || state != (CapsuleState{}) || c.Stats[capsulePrefix+"pair"] != 0 {
			t.Fatalf("divorce retained capsule: %+v %v", state, err)
		}
	}
	// Even an imported stale row is scoped to its marriage date and pair.
	a.Stats["wedding_date"]++
	if state, err := ReadCapsule(a, rules); err != nil || state != (CapsuleState{}) {
		t.Fatalf("old marriage reused %+v %v", state, err)
	}
}

func TestWeddingRingPurchaseAtomicCurrencies(t *testing.T) {
	ctx := context.Background()
	for _, mode := range []string{"success", "gold", "love", "mismatch", "full_bag", "rollback", "not_married"} {
		t.Run(mode, func(t *testing.T) {
			repos, cat, _, capsule := capsuleTestPair(t)
			cmd := RingPurchase{ActorID: "1", PartnerID: "2", AccountID: "a1", Item: Item{10002613, 1}, Gold: 3500000, Love: 500, Now: capsule.Now, Grant: capsule.Grant}
			a, _, _ := repos.Character.Load(ctx, "1")
			b, _, _ := repos.Character.Load(ctx, "2")
			switch mode {
			case "gold":
				a.Stats["gold"] = 3499999
			case "love":
				a.Stats["wedding_love"] = 499
				b.Stats["wedding_love"] = 499
			case "mismatch":
				b.Stats["wedding_love"] = 945
			case "full_bag":
				cmd.Grant = func(*repo.InventoryRecord, Item) error { return errors.New("full") }
			case "rollback":
				repos.CharacterTrade = failingWeddingTrade{repos.CharacterTrade}
			case "not_married":
				a.Stats["wedding_state"] = 1
				b.Stats["wedding_state"] = 1
			}
			if err := repos.Character.Save(ctx, a); err != nil {
				t.Fatal(err)
			}
			if err := repos.Character.Save(ctx, b); err != nil {
				t.Fatal(err)
			}
			bag, _, _ := repos.Inventory.Load(ctx, "1")
			res, err := BuyRingTicket(ctx, repos, cat, cmd)
			if mode == "success" {
				if err != nil || res.Characters[0].Stats["gold"] != 5500000 || res.Characters[1].Stats["gold"] != 9000000 || res.Characters[0].Stats["wedding_love"] != 446 || res.Characters[1].Stats["wedding_love"] != 446 || res.Inventories[0].Slots["0:57"].Count != 1 || res.Characters[0].Stats["wedding_ring"] != 2 || res.Characters[1].Stats["wedding_ring"] != 2 {
					t.Fatalf("purchase=%+v %v", res, err)
				}
				return
			}
			if err == nil {
				t.Fatal("invalid purchase succeeded")
			}
			afterA, _, _ := repos.Character.Load(ctx, "1")
			afterB, _, _ := repos.Character.Load(ctx, "2")
			afterBag, _, _ := repos.Inventory.Load(ctx, "1")
			if !reflect.DeepEqual(a, afterA) || !reflect.DeepEqual(b, afterB) || !reflect.DeepEqual(bag, afterBag) {
				t.Fatal("failed purchase changed assets")
			}
		})
	}
}
