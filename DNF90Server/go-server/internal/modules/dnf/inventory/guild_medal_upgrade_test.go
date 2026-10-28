package inventory

import (
	"bytes"
	"context"
	"encoding/binary"
	"encoding/hex"
	"errors"
	"reflect"
	"strconv"
	"strings"
	"testing"

	"longheng.io/server/internal/modules/dnf/alignedcmd"
	"longheng.io/server/internal/modules/dnf/dnfenum"
	dnfrepo "longheng.io/server/internal/modules/dnf/repository"
)

// The 公会徽章 (guild flag) reinforce path is list 38. It is ported from
// upstream's TestGuildMedalUpgradeNativeRequestAndTransaction; the one
// deliberate divergence is the rejected mode/list table at the end, because
// this tree additionally supports the worn list-3 reinforce upstream does not.
func TestGuildMedalUpgradeNativeRequestAndTransaction(t *testing.T) {
	body, _ := hex.DecodeString("00260d007aadfb05bd00ffff0a000000bcf2c2aab5c4d1abd5c2ffffffff")
	parsed, err := DecodeUpgradeItemRequest(body)
	if err != nil || parsed.TargetListType != 38 || parsed.TargetSlotIndex != 13 || parsed.MaterialSlotIndex != 189 || parsed.Mode != "reinforce" {
		t.Fatalf("request=%+v err=%v", parsed, err)
	}
	for _, tc := range []struct {
		name                   string
		weight, penalty, value int
		level                  byte
		missing, stale         bool
	}{
		{"success", 100000, 0, 0, 5, false, false},
		{"failure", 0, 0, 0, 4, false, false},
		{"downgrade", 0, 2, 3, 1, false, false},
		{"destroy", 0, 3, 0, 4, false, false},
		{"missing material", 100000, 0, 0, 4, true, false},
		{"stale policy", 100000, 0, 0, 4, false, true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			ctx, repos, owner, cmd := protectedUpgradeFixture(t, "reinforce", 4, 7804, 2)
			bag := loadTestInventory(t, ctx, repos, "77")
			medal := bag.Slots["0:9"]
			medal.RawEntry = make([]byte, currentItemListEntrySize)
			medal.RawEntry[101], medal.RawEntry[109] = 3, 0x5a
			medal.Extra["upgrade_level"] = "4"
			bag.Slots["38:9"] = medal
			bag.Slots["0:9"] = dnfrepo.ItemStack{ItemID: 10154439, Count: 50}
			if tc.missing {
				bag.Slots["0:9"] = dnfrepo.ItemStack{ItemID: 10154439, Count: 1}
			}
			saveTestInventory(t, ctx, repos, bag)
			cmd.TargetListType, cmd.MaterialSlotIndex, cmd.OptionalTicketSlot = 38, 9, -1
			cmd.UpgradeGuildMedal, cmd.UpgradePolicyLevel, cmd.UpgradeGoldCost = true, 4, 1
			cmd.UpgradeMaterialItemID, cmd.UpgradeMaterialCount = 10154439, 5
			cmd.UpgradeSuccessWeight, cmd.UpgradePenaltyType, cmd.UpgradePenaltyValue = tc.weight, tc.penalty, tc.value
			cmd.UpgradeNoticeLevel = -1
			if tc.stale {
				cmd.UpgradePolicyLevel = 3
			}
			result, err := owner.Upgrade(ctx, cmd)
			if err != nil {
				t.Fatal(err)
			}
			after := loadTestInventory(t, ctx, repos, "77")
			character, _, _ := repos.Character.Load(ctx, "77")
			if tc.missing || tc.stale {
				if result.Success || !reflect.DeepEqual(bag.Slots, after.Slots) || character.Stats["gold"] != 1000 {
					t.Fatal("rejected upgrade changed assets")
				}
				return
			}
			if !result.Success || result.NewLevel != tc.level || result.TargetListType != 38 || character.Stats["gold"] != 999 || after.Slots["0:9"].Count != 45 {
				t.Fatalf("result=%+v bag=%+v gold=%d", result, after.Slots, character.Stats["gold"])
			}
			if tc.penalty == 3 {
				if _, exists := after.Slots["38:9"]; exists {
					t.Fatal("destroyed medal retained")
				}
			} else if raw := after.Slots["38:9"].RawEntry; raw[101] != 3 || raw[109] != 0x5a {
				t.Fatal("socket attributes lost")
			}
			if after.Slots["0:130"].Count != 2 {
				t.Fatal("ordinary protection ticket consumed")
			}
			ack := buildUpgradeItemSuccessAck(result)
			if ack[14] != 38 || !bytes.Equal(ack[len(ack)-6:], []byte{255, 255, 0, 0, 0, 0}) {
				t.Fatalf("ack=%x", ack)
			}
			response := ownerAppliedUpgradeResult(ctx, repos.Settings, 80, cmd, result)
			if len(response.ItemSlotRefreshes) != 0 || len(response.PostActions) != 0 {
				t.Fatal("medal result must retain the native item object")
			}
			for i, packet := range response.UpperResponses {
				if i == 0 {
					if !bytes.Equal(packet.Body, ack) {
						t.Fatalf("first response=%x", packet.Body)
					}
					continue
				}
				if packet.MsgID != msgItemListUpdate || packet.Body[0] != listTypeMain {
					t.Fatalf("unexpected object-replacing packet: %+v", packet)
				}
			}
		})
	}
	// Upstream also rejects {0, 3} here. This tree must keep accepting it: the
	// worn list-3 reinforce is a live-verified path this fork added (see the
	// 0x0300 mode-word note in DecodeUpgradeItemRequest).
	for _, pair := range [][2]byte{{1, 38}, {0, 255}} {
		bad := append([]byte(nil), body...)
		bad[0], bad[1] = pair[0], pair[1]
		if _, err := DecodeUpgradeItemRequest(bad); err == nil {
			t.Fatalf("accepted mode/list %v", pair)
		}
	}
	// A guild medal outside the page's 0..48 slots has no flag row to resolve.
	for _, slot := range []int16{-1, 49} {
		bad := append([]byte(nil), body...)
		bad[2], bad[3] = byte(slot), byte(slot>>8)
		if _, err := DecodeUpgradeItemRequest(bad); err == nil {
			t.Fatalf("accepted list-38 slot %d", slot)
		}
	}
	// The worn list-3 reinforce and the backpack list-0 reinforce stay legal,
	// in every sub mode this tree accepts (0 reinforce, 1 amplify, 2 the
	// 高级装备强化机).
	for _, pair := range [][2]byte{{0, 0}, {1, 0}, {2, 0}, {0, 3}, {1, 3}} {
		good := append([]byte(nil), body...)
		good[0], good[1] = pair[0], pair[1]
		if _, err := DecodeUpgradeItemRequest(good); err != nil {
			t.Fatalf("rejected mode/list %v: %v", pair, err)
		}
	}
}

// TestGuildMedalUpgradeOptionalTicketSentinel ports the 255 -> -1 normalisation:
// the medal page has no ticket slot, so the client sends its unsigned "none"
// value and the owner's list-38 gate requires a negative slot.
func TestGuildMedalUpgradeOptionalTicketSentinel(t *testing.T) {
	parsed := UpgradeItemRequest{TargetListType: listTypeGuildMedal, OptionalTicketSlotIndex: 255}
	if got := NewUpgradeCommand(alignedcmd.Request{}, parsed).OptionalTicketSlot; got != -1 {
		t.Fatalf("guild medal optional ticket slot=%d, want -1", got)
	}
	parsed = UpgradeItemRequest{TargetListType: listTypeMain, OptionalTicketSlotIndex: 255}
	if got := NewUpgradeCommand(alignedcmd.Request{}, parsed).OptionalTicketSlot; got != 255 {
		t.Fatalf("list-0 optional ticket slot=%d, want 255 untouched", got)
	}
}

// TestGuildMedalUpgradeGateRefusals covers the list-38-only admission gate: a
// medal reachable through any other route must not be upgradable.
func TestGuildMedalUpgradeGateRefusals(t *testing.T) {
	for _, tc := range []struct {
		name    string
		mutate  func(*Command)
		wantErr bool
	}{
		{"resolver did not mark the medal", func(c *Command) { c.UpgradeGuildMedal = false }, true},
		{"amplify on list 38", func(c *Command) { c.Mode = "amplify" }, true},
		{"optional ticket present", func(c *Command) { c.OptionalTicketSlot = 130 }, true},
		{"policy error recorded", func(c *Command) { c.UpgradePolicyError = "missing row" }, true},
		{"slot past the page", func(c *Command) { c.TargetSlotIndex = 49 }, true},
		{"well formed", func(c *Command) {}, false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			ctx, repos, owner, cmd := protectedUpgradeFixture(t, "reinforce", 4, 7804, 2)
			bag := loadTestInventory(t, ctx, repos, "77")
			medal := bag.Slots["0:9"]
			medal.RawEntry = make([]byte, currentItemListEntrySize)
			medal.Extra["upgrade_level"] = "4"
			bag.Slots["38:9"] = medal
			bag.Slots["0:9"] = dnfrepo.ItemStack{ItemID: 10154439, Count: 50}
			saveTestInventory(t, ctx, repos, bag)
			cmd.TargetListType, cmd.MaterialSlotIndex, cmd.OptionalTicketSlot = 38, 9, -1
			cmd.UpgradeGuildMedal, cmd.UpgradePolicyLevel, cmd.UpgradeGoldCost = true, 4, 1
			cmd.UpgradeMaterialItemID, cmd.UpgradeMaterialCount = 10154439, 5
			cmd.UpgradeSuccessWeight, cmd.UpgradeNoticeLevel = 100000, -1
			tc.mutate(&cmd)
			result, err := owner.Upgrade(ctx, cmd)
			if err != nil {
				t.Fatal(err)
			}
			after := loadTestInventory(t, ctx, repos, "77")
			character, _, _ := repos.Character.Load(ctx, "77")
			if tc.wantErr {
				if result.Success || !reflect.DeepEqual(bag.Slots, after.Slots) || character.Stats["gold"] != 1000 {
					t.Fatalf("refused upgrade changed assets: result=%+v gold=%d", result, character.Stats["gold"])
				}
				return
			}
			if !result.Success || character.Stats["gold"] != 999 || after.Slots["0:9"].Count != 45 {
				t.Fatalf("allowed upgrade did not apply: result=%+v gold=%d material=%d", result, character.Stats["gold"], after.Slots["0:9"].Count)
			}
		})
	}
}

// TestGuildMedalUpgradeZeroWeightAlwaysFails pins upstream's list-38 carve-out
// in the RNG step: the "weight <= 0 means guaranteed success" backward
// compatibility must not apply to a medal, because a flag row legitimately
// states zero success weight. Result.Success means "the attempt was applied",
// so the probability roll is the field that discriminates.
func TestGuildMedalUpgradeZeroWeightAlwaysFails(t *testing.T) {
	ctx, repos, owner, cmd := protectedUpgradeFixture(t, "reinforce", 4, 7804, 2)
	bag := loadTestInventory(t, ctx, repos, "77")
	medal := bag.Slots["0:9"]
	medal.RawEntry = make([]byte, currentItemListEntrySize)
	medal.Extra["upgrade_level"] = "4"
	bag.Slots["38:9"] = medal
	bag.Slots["0:9"] = dnfrepo.ItemStack{ItemID: 10154439, Count: 50}
	saveTestInventory(t, ctx, repos, bag)
	cmd.TargetListType, cmd.MaterialSlotIndex, cmd.OptionalTicketSlot = 38, 9, -1
	cmd.UpgradeGuildMedal, cmd.UpgradePolicyLevel, cmd.UpgradeGoldCost = true, 4, 1
	cmd.UpgradeMaterialItemID, cmd.UpgradeMaterialCount = 10154439, 5
	cmd.UpgradeSuccessWeight, cmd.UpgradeNoticeLevel = 0, -1
	// Penalty type 0 (fail without change) so the flag level is the observable;
	// the fixture's default is 3, which destroys the target instead.
	cmd.UpgradePenaltyType, cmd.UpgradePenaltyValue = 0, 0
	result, err := owner.Upgrade(ctx, cmd)
	if err != nil {
		t.Fatal(err)
	}
	if !result.Success {
		t.Fatalf("zero-weight attempt was refused instead of resolved: %+v", result)
	}
	if result.UpgradeSucceeded || result.NewLevel != 4 {
		t.Fatalf("zero success weight was treated as guaranteed success: %+v", result)
	}
	if after := loadTestInventory(t, ctx, repos, "77"); after.Slots["38:9"].Extra["upgrade_level"] != "4" {
		t.Fatal("failed medal upgrade changed the flag level")
	}
}

type guildMedalFailingInventory struct{ dnfrepo.InventoryRepository }

func (r guildMedalFailingInventory) Save(context.Context, dnfrepo.InventoryRecord) error {
	return errors.New("injected inventory save failure")
}

type guildMedalFailingTransaction struct{ dnfrepo.Group }

func (tx guildMedalFailingTransaction) WithinCharacterItems(ctx context.Context, id string, apply func(dnfrepo.InventoryRepository, dnfrepo.EquipmentRepository) error) error {
	return tx.Group.CharacterItems.WithinCharacterItems(ctx, id, func(inv dnfrepo.InventoryRepository, eq dnfrepo.EquipmentRepository) error {
		return apply(guildMedalFailingInventory{inv}, eq)
	})
}

func (tx guildMedalFailingTransaction) WithinCharacterAssets(ctx context.Context, id string, apply func(dnfrepo.CharacterRepository, dnfrepo.InventoryRepository, dnfrepo.EquipmentRepository) error) error {
	return tx.Group.CharacterAssets.WithinCharacterAssets(ctx, id, func(char dnfrepo.CharacterRepository, inv dnfrepo.InventoryRepository, eq dnfrepo.EquipmentRepository) error {
		return apply(char, guildMedalFailingInventory{inv}, eq)
	})
}

// TestGuildMedalUpgradeDatabaseFailureRollsBackAndAcks ports upstream's
// rollback test, plus its list-38 handler assertion: a failed medal
// transaction must answer with the ordinary op50 error ACK rather than the
// generic blocked form the medal window cannot read.
func TestGuildMedalUpgradeDatabaseFailureRollsBackAndAcks(t *testing.T) {
	ctx, repos, _, cmd := protectedUpgradeFixture(t, "reinforce", 0, 7804, 2)
	bag := loadTestInventory(t, ctx, repos, "77")
	bag.Slots["38:9"] = bag.Slots["0:9"]
	bag.Slots["38:10"] = bag.Slots["0:9"]
	bag.Slots["38:49"] = dnfrepo.ItemStack{ItemID: 90002, Count: 5}
	saveTestInventory(t, ctx, repos, bag)
	failure := guildMedalFailingTransaction{repos}
	repos.CharacterItems, repos.CharacterAssets = failure, failure
	owner, _ := NewOwner(repos)
	cmd.TargetListType, cmd.OptionalTicketSlot = 38, -1
	cmd.UpgradeGuildMedal, cmd.UpgradeGoldCost = true, 1
	cmd.UpgradeSuccessWeight = 100000
	if _, err := owner.Upgrade(ctx, cmd); err == nil {
		t.Fatal("upgrade ignored save failure")
	}
	character, _, _ := repos.Character.Load(ctx, "77")
	if character.Stats["gold"] != 1000 || !reflect.DeepEqual(bag.Slots, loadTestInventory(t, ctx, repos, "77").Slots) {
		t.Fatal("upgrade failed to roll back assets")
	}

	body, _ := hex.DecodeString("00260900bc0200007900ffff00000000ffffffff")
	response, err := NewHandler().Handle(ctx, alignedcmd.Request{
		Opcode: 80, Body: body, AccountID: "acc", SelectedCharacterID: 77, Repositories: repos,
		UpgradePolicyResolver: func(string, int, int64, int) (alignedcmd.UpgradePolicyResolution, error) {
			return alignedcmd.UpgradePolicyResolution{GuildMedal: true, GoldCost: 1, SuccessWeight: 100000, MaterialItemID: 5000, MaterialCount: 1, NoticeLevel: -1}, nil
		},
	})
	if err != nil || !response.ResponseAllowed || len(response.UpperResponses) != 1 || response.UpperResponses[0].Body[0] != 0 {
		t.Fatalf("missing failure ACK response=%+v err=%v", response, err)
	}
}

// guildMedalUpgradeBody builds the live op50 list-38 wire: mode word high byte
// 38 (list) + low byte 0 (reinforce), then target slot, item id, material slot,
// the medal page's unsigned "no ticket" value, and the DSTR target name.
func guildMedalUpgradeBody(targetSlot int16, targetItemID int32, materialSlot int16, name string) []byte {
	body := make([]byte, 16+len(name))
	body[0] = 0x00
	body[1] = listTypeGuildMedal
	binary.LittleEndian.PutUint16(body[2:4], uint16(targetSlot))
	binary.LittleEndian.PutUint32(body[4:8], uint32(targetItemID))
	binary.LittleEndian.PutUint16(body[8:10], uint16(materialSlot))
	binary.LittleEndian.PutUint16(body[10:12], 0xFFFF)
	binary.LittleEndian.PutUint32(body[12:16], uint32(len(name)))
	copy(body[16:], name)
	return body
}

// guildMedalHandlerFixture seeds the live medal-page shape: one [flag] medal in
// list 38 (with its real level) and the flag_upgrade_table material 公会贡献证书
// in the backpack. Any caller-supplied bag row is placed by the caller.
func guildMedalHandlerFixture(t *testing.T, targetSlot int16, level byte, materialSlot int16, materialCount int64) (context.Context, dnfrepo.Group) {
	t.Helper()
	ctx, repos, _, _ := protectedUpgradeFixture(t, "reinforce", 0, 7804, 2)
	bag := loadTestInventory(t, ctx, repos, "77")
	medal := dnfrepo.ItemStack{
		ItemID:   100380058,
		Count:    1,
		RawEntry: make([]byte, currentItemListEntrySize),
		Extra: map[string]string{
			"item_kind":      "equipment",
			"durability":     "45",
			"max_durability": "45",
			"upgrade_level":  strconv.Itoa(int(level)),
			"reinforce":      strconv.Itoa(int(level)),
		},
	}
	medal.RawEntry[0x0A] = level
	bag.Slots[slotKey(listTypeGuildMedal, targetSlot)] = medal
	bag.Slots[slotKey(listTypeMain, materialSlot)] = dnfrepo.ItemStack{
		ItemID:   10154439,
		Count:    materialCount,
		RawEntry: make([]byte, currentItemListEntrySize),
		Extra:    map[string]string{"item_kind": "stackable"},
	}
	saveTestInventory(t, ctx, repos, bag)
	return ctx, repos
}

// TestHandlerGuildMedalUpgradeIsNotClaimedByTicketPrepass reproduces the
// 2026-10-04 19:16:30 / 19:17:16 live failure exactly: production always
// assembles an UpgradeTicketResolver, the list-38 medals (100380058 in 38:2 and
// 100380021 in 38:1) are reinforced with 公会贡献证书 in 0:179, and the op50
// ticket pre-pass used to swallow the request — UpgradeTicket cannot serve a
// list-38 target, so it answered TicketResolved=true with error 4 and the client
// only ever saw "勋章不给强化".
//
// Note the fixture below is the same shape the previous, passing test used; the
// only difference is the resolvers production actually wires up.
func TestHandlerGuildMedalUpgradeIsNotClaimedByTicketPrepass(t *testing.T) {
	ctx, repos := guildMedalHandlerFixture(t, 2, 0, 179, 50)

	ticketResolverCalls := 0
	ticketResolver := func(int64, int64) (alignedcmd.UpgradeTicketResolution, error) {
		ticketResolverCalls++
		return alignedcmd.UpgradeTicketResolution{
			TicketMode:    "reinforce",
			TargetLevel:   10,
			SuccessWeight: 100000,
			TargetKind:    "equipment",
			TicketPVFPath: "stackable/ticket/plus10.stk",
		}, nil
	}
	policyLevelSeen := -1
	response, err := NewHandler().Handle(ctx, alignedcmd.Request{
		Opcode: 80, Body: guildMedalUpgradeBody(2, 100380058, 179, "完美的守护图腾勋章"),
		AccountID: "acc", SelectedCharacterID: 77, Repositories: repos,
		UpgradeTicketResolver: ticketResolver,
		UpgradePolicyResolver: func(mode string, level int, itemID int64, _ int) (alignedcmd.UpgradePolicyResolution, error) {
			policyLevelSeen = level
			return alignedcmd.UpgradePolicyResolution{
				GuildMedal: true, CurrentLevel: level, GoldCost: 1,
				SuccessWeight: 100000, MaterialItemID: 10154439, MaterialCount: 5, NoticeLevel: -1,
			}, nil
		},
	})
	if err != nil {
		t.Fatalf("Handle error = %v", err)
	}
	if ticketResolverCalls != 0 {
		t.Fatalf("the list-38 medal page has no ticket scene, but the PVF ticket resolver was consulted %d time(s)", ticketResolverCalls)
	}
	if policyLevelSeen != 0 {
		t.Fatalf("policy resolved for level %d, want the medal's own level 0", policyLevelSeen)
	}
	if !response.Handled || !response.ResponseAllowed || len(response.UpperResponses) == 0 {
		t.Fatalf("response = %+v", response)
	}
	if strings.Contains(response.Reason, "rejected") {
		t.Fatalf("the medal request was rejected instead of applied: %s", response.Reason)
	}
	ack := response.UpperResponses[0]
	if ack.MsgID != uint16(dnfenum.CmdPacketUpgradeItem) || ack.Classification != 1 || len(ack.Body) < 15 || ack.Body[14] != listTypeGuildMedal {
		t.Fatalf("ack = msg 0x%04X class %d body=%x, want the list-38 success ACK", ack.MsgID, ack.Classification, ack.Body)
	}
	if !bytes.Equal(ack.Body[len(ack.Body)-6:], []byte{255, 255, 0, 0, 0, 0}) {
		t.Fatalf("ack body = %x, want the list-38 tail i16(-1) + i32(0)", ack.Body)
	}
	after := loadTestInventory(t, ctx, repos, "77")
	if got := after.Slots[slotKey(listTypeMain, 179)].Count; got != 45 {
		t.Fatalf("material count = %d, want 45 (50 - the row's 5)", got)
	}
	if got := after.Slots[slotKey(listTypeGuildMedal, 2)].RawEntry[0x0A] & 0x1F; got != 1 {
		t.Fatalf("medal level = %d, want 1", got)
	}
	character, _, _ := repos.Character.Load(ctx, "77")
	if character.Stats["gold"] != 999 {
		t.Fatalf("gold = %d, want 999", character.Stats["gold"])
	}
}

// TestHandlerGuildMedalUpgradeResolvesPolicyFromTheMedalRow pins the second
// half of the same live bug: the op50 pre-transaction load must read the
// target's OWN list. Reading `0:<slot>` for a medal at `38:<slot>` either
// misses (level 0) or picks up an unrelated stack's level, and the owner's
// list-38 gate (`int(oldLevel) != cmd.UpgradePolicyLevel`) then rejects the
// attempt with error 4 — the identical "勋章不给强化" symptom. Here the medal
// sits at level 3 in 38:9 while its material 公会贡献证书 occupies the SAME
// index 0:9, which is exactly the live shape (medal 38:1/38:2, material 0:179
// would collide the moment the indices line up).
func TestHandlerGuildMedalUpgradeResolvesPolicyFromTheMedalRow(t *testing.T) {
	ctx, repos := guildMedalHandlerFixture(t, 9, 3, 9, 50)

	policyLevelSeen := -1
	response, err := NewHandler().Handle(ctx, alignedcmd.Request{
		Opcode: 80, Body: guildMedalUpgradeBody(9, 100380058, 9, "完美的守护图腾勋章"),
		AccountID: "acc", SelectedCharacterID: 77, Repositories: repos,
		UpgradeTicketResolver: reinforceTicketResolver(10, 100000),
		UpgradePolicyResolver: func(mode string, level int, itemID int64, _ int) (alignedcmd.UpgradePolicyResolution, error) {
			policyLevelSeen = level
			if itemID != 100380058 {
				t.Errorf("policy resolved for item %d, want the requested medal 100380058", itemID)
			}
			return alignedcmd.UpgradePolicyResolution{
				GuildMedal: true, CurrentLevel: level, GoldCost: 1,
				SuccessWeight: 100000, MaterialItemID: 10154439, MaterialCount: 5, NoticeLevel: -1,
			}, nil
		},
	})
	if err != nil {
		t.Fatalf("Handle error = %v", err)
	}
	if policyLevelSeen != 3 {
		t.Fatalf("policy resolved for level %d, want the medal's own level 3", policyLevelSeen)
	}
	if strings.Contains(response.Reason, "rejected") || len(response.UpperResponses) == 0 {
		t.Fatalf("level-3 medal upgrade was rejected: %s", response.Reason)
	}
	// A medal reinforced with a backpack stack that carries the same index is
	// legal: the material guard only compares indices for list-0 targets.
	after := loadTestInventory(t, ctx, repos, "77")
	if got := after.Slots[slotKey(listTypeMain, 9)].Count; got != 45 {
		t.Fatalf("material count = %d, want 45", got)
	}
	if got := after.Slots[slotKey(listTypeGuildMedal, 9)].RawEntry[0x0A] & 0x1F; got != 4 {
		t.Fatalf("medal level = %d, want 4", got)
	}
}

// TestUpgradeTargetStackOfSlotReadsTheTargetList is the unit-level half of the
// same rule: with the same item id present in both lists, the list-38 row must
// be the one whose level feeds the policy resolver.
func TestUpgradeTargetStackOfSlotReadsTheTargetList(t *testing.T) {
	ctx, repos := guildMedalHandlerFixture(t, 9, 3, 179, 50)
	bag := loadTestInventory(t, ctx, repos, "77")
	bagRow := bag.Slots[slotKey(listTypeGuildMedal, 9)]
	bagRow.RawEntry = make([]byte, currentItemListEntrySize)
	bagRow.RawEntry[0x0A] = 6
	bagRow.Extra = map[string]string{"item_kind": "equipment", "upgrade_level": "6", "reinforce": "6"}
	bag.Slots[slotKey(listTypeMain, 9)] = bagRow
	saveTestInventory(t, ctx, repos, bag)

	cmd := NewUpgradeCommand(alignedcmd.Request{AccountID: "acc", SelectedCharacterID: 77}, UpgradeItemRequest{
		Mode:                 "reinforce",
		TargetListType:       listTypeGuildMedal,
		TargetSlotIndex:      9,
		TargetItemTemplateID: 100380058,
		MaterialSlotIndex:    179,
	})
	stack, ok := upgradeTargetStackOfSlot(repos, ctx, cmd)
	if !ok {
		t.Fatal("list-38 target was not found")
	}
	if got := upgradeLevelOf(stack); got != 3 {
		t.Fatalf("policy input level = %d, want the 38:9 medal's 3 (the 0:9 row says 6)", got)
	}
}
