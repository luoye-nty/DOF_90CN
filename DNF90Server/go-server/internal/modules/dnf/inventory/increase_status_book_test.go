package inventory

import (
	"bytes"
	"context"
	"encoding/binary"
	"testing"
	"time"

	"longheng.io/server/internal/modules/dnf/alignedcmd"
	"longheng.io/server/internal/modules/dnf/dnfenum"
	dnfrepo "longheng.io/server/internal/modules/dnf/repository"
	dnfrepomemory "longheng.io/server/internal/modules/dnf/repository/memory"
)

const increaseStatusTestCharacterID uint16 = 77

// The op30 ACK must echo the source slot of the double-clicked book, not the
// character id. 0x42 keeps the two distinguishable so a regression back to the
// character id cannot pass. (Live symptom when it did: character id 1 sat at
// main-inventory slot 1, the 复活币 wallet, so every book use decremented the
// on-screen 复活币 by one.)
const increaseStatusTestSourceSlot uint16 = 0x42

func TestDecodeIncreaseStatusRequestAcceptsBothCapturedLiveBodies(t *testing.T) {
	// Both live captures of the dropped op30 carried nothing but the slot of
	// the double-clicked book.
	for _, test := range []struct {
		body []byte
		slot int16
	}{
		{body: []byte{0x42, 0x00}, slot: 0x42},
		{body: []byte{0x43, 0x00}, slot: 0x43},
	} {
		parsed, err := DecodeIncreaseStatusRequest(test.body)
		if err != nil || parsed.SlotIndex != test.slot {
			t.Fatalf("body=%x parsed=%+v err=%v", test.body, parsed, err)
		}
	}
	for _, body := range [][]byte{nil, {}, {0x42}, {0x42, 0x00, 0x00}, {0xFF, 0xFF}} {
		if _, err := DecodeIncreaseStatusRequest(body); err == nil {
			t.Fatalf("body=%x was accepted", body)
		}
	}
}

func TestIncreaseStatusSilentKindMissesEveryCurrentClientBranch(t *testing.T) {
	// The client's op30 branch chain is 0, 1, 2..9 and 0x11. Experience books
	// and the TP reset have no branch, so their kind must miss all of them.
	for kind := byte(0); kind <= 9; kind++ {
		if increaseStatusKindSilent == kind {
			t.Fatalf("silent kind collides with client branch %d", kind)
		}
	}
	if increaseStatusKindSilent == increaseStatusKindTP ||
		increaseStatusKindSP != 0x00 || increaseStatusKindTP != 0x11 {
		t.Fatalf("kinds sp=%#x tp=%#x silent=%#x", increaseStatusKindSP, increaseStatusKindTP, increaseStatusKindSilent)
	}
}

// readIncreaseStatusAckAsClient parses the ACK exactly the way the current
// class1/op30 handler at RVA 0x0191EDE0 does: the stream it receives has
// already consumed the leading class1 result flag, and it then reads
// u16/u8/u32/u16/u16. The u16 is the source item slot the handler looks up in
// the main-inventory array (sub_1F26860 indexes `begin + slot*4`).
func readIncreaseStatusAckAsClient(t *testing.T, ack []byte) (result byte, sourceSlot uint16, kind byte, amount uint32, spGain, tpGain uint16) {
	t.Helper()
	if len(ack) != increaseStatusAckBodySize {
		t.Fatalf("ack length %d, want %d: %x", len(ack), increaseStatusAckBodySize, ack)
	}
	stream := ack[1:]
	return ack[0],
		binary.LittleEndian.Uint16(stream[0:2]),
		stream[2],
		binary.LittleEndian.Uint32(stream[3:7]),
		binary.LittleEndian.Uint16(stream[7:9]),
		binary.LittleEndian.Uint16(stream[9:11])
}

func increaseStatusSkillPointFixture(t *testing.T, itemID int64, pvfPath string) dnfrepo.Group {
	t.Helper()
	ctx := context.Background()
	repositories := dnfrepomemory.NewMemoryGroup()
	if err := repositories.Character.Save(ctx, dnfrepo.CharacterRecord{
		CharacterID: "77", AccountID: "acc", Job: "0", Level: 50,
		Stats: map[string]int64{"bonus_sp": 0, "bonus_tp": 0},
	}); err != nil {
		t.Fatal(err)
	}
	if err := repositories.Skill.Save(ctx, dnfrepo.SkillRecord{
		CharacterID: "77",
		Points:      dnfrepo.SkillPointState{TotalSP: 100, RemainingSP: 100, TotalTP: 10, RemainingTP: 10, SyncedLevel: 50},
	}); err != nil {
		t.Fatal(err)
	}
	if err := repositories.Inventory.Save(ctx, dnfrepo.InventoryRecord{
		CharacterID: "77",
		Slots: map[string]dnfrepo.ItemStack{
			slotKey(listTypeMain, 0x42): {
				ItemID: itemID, Count: 2,
				Extra: map[string]string{"item_kind": "stackable", "pvf_path": pvfPath, "stackable_type": "[etc]"},
			},
		},
	}); err != nil {
		t.Fatal(err)
	}
	return repositories
}

func TestIncreaseStatusSkillBooksReturnTheExactCurrentClientGainACK(t *testing.T) {
	for _, test := range []struct {
		name     string
		itemID   int64
		pvfPath  string
		effect   alignedcmd.TPBookResolution
		wantKind byte
		wantGain uint32
	}{
		{
			name: "SP+5", itemID: 1031, pvfPath: "stackable/book_skill1.stk",
			effect:   alignedcmd.TPBookResolution{Kind: alignedcmd.SkillPointBookAddSP, Delta: 5, MinimumLevel: 1},
			wantKind: increaseStatusKindSP, wantGain: 5,
		},
		{
			name: "SP+20", itemID: 1038, pvfPath: "stackable/book_skill2.stk",
			effect:   alignedcmd.TPBookResolution{Kind: alignedcmd.SkillPointBookAddSP, Delta: 20, MinimumLevel: 1},
			wantKind: increaseStatusKindSP, wantGain: 20,
		},
		{
			name: "TP+1", itemID: 1204, pvfPath: "stackable/book_fskill1.stk",
			effect:   alignedcmd.TPBookResolution{Kind: alignedcmd.SkillPointBookAddTP, Delta: 1, MinimumLevel: 50},
			wantKind: increaseStatusKindTP, wantGain: 1,
		},
		{
			name: "TP+5", itemID: 1205, pvfPath: "stackable/book_fskill2.stk",
			effect:   alignedcmd.TPBookResolution{Kind: alignedcmd.SkillPointBookAddTP, Delta: 5, MinimumLevel: 50},
			wantKind: increaseStatusKindTP, wantGain: 5,
		},
	} {
		t.Run(test.name, func(t *testing.T) {
			ctx := context.Background()
			repositories := increaseStatusSkillPointFixture(t, test.itemID, test.pvfPath)
			result, err := (handler{}).Handle(ctx, alignedcmd.Request{
				Opcode:               uint16(dnfenum.CmdPacketIncreaseStatus),
				Body:                 []byte{0x42, 0x00},
				AccountID:            "acc",
				SelectedCharacterID:  increaseStatusTestCharacterID,
				Repositories:         repositories,
				StackableUseResolver: skillPointBookTestResolver(test.itemID, test.pvfPath, test.effect),
				InitialSkillLevels:   map[uint16]int{},
				SkillPointBaseline:   &dnfrepo.SkillPointState{TotalSP: 100, RemainingSP: 100, TotalTP: 10, RemainingTP: 10, SyncedLevel: 50},
			})
			if err != nil {
				t.Fatal(err)
			}
			if !result.Handled || !result.ResponseAllowed || result.Operation != "increase_status_book" ||
				len(result.UpperResponses) != 1 ||
				result.UpperResponses[0].MsgID != uint16(dnfenum.CmdPacketIncreaseStatus) ||
				result.UpperResponses[0].Classification != 1 {
				t.Fatalf("result=%+v", result)
			}
			ack := result.UpperResponses[0].Body

			wantACK := []byte{1, byte(increaseStatusTestSourceSlot), 0, test.wantKind, 0, 0, 0, 0, 0, 0, 0, 0}
			binary.LittleEndian.PutUint32(wantACK[4:8], test.wantGain)
			if !bytes.Equal(ack, wantACK) {
				t.Fatalf("ack=%x want=%x", ack, wantACK)
			}

			resultFlag, sourceSlot, kind, amount, spGain, tpGain := readIncreaseStatusAckAsClient(t, ack)
			if resultFlag != 1 || sourceSlot != increaseStatusTestSourceSlot || kind != test.wantKind ||
				amount != test.wantGain || spGain != 0 || tpGain != 0 {
				t.Fatalf("client view result=%d slot=%#x kind=%#x amount=%d sp=%d tp=%d", resultFlag, sourceSlot, kind, amount, spGain, tpGain)
			}
			// Regression guard for the character-id ACK. The client consumes
			// mainInventory[that u16] for every kind, so the character id made
			// the 复活币 wallet at slot 1 drop by one per book (live 2026-09-28).
			if sourceSlot == increaseStatusTestCharacterID || sourceSlot != 0x42 {
				t.Fatalf("ack slot=%#x must be the source slot %#x, never the character id %#x", sourceSlot, 0x42, increaseStatusTestCharacterID)
			}
			// Regression guard for the first implementation, which dropped the
			// class1 result flag and carried the gain in spGain instead of the
			// u32. A live client rendered SP+5 as 5<<24 = 83886080 and SP+20 as
			// 20<<24 = 335544320. The amount must never drift back there.
			if amount == test.wantGain<<24 || spGain == uint16(test.wantGain) || tpGain == uint16(test.wantGain) {
				t.Fatalf("gain drifted into the misaligned position: amount=%d sp=%d tp=%d", amount, spGain, tpGain)
			}

			if len(result.ItemSlotRefreshes) != 1 || result.ItemSlotRefreshes[0].ListType != listTypeMain ||
				result.ItemSlotRefreshes[0].SlotIndex != 0x42 ||
				len(result.PostActions) != 1 || result.PostActions[0] != alignedcmd.PostActionRefreshSelectedActorSkills {
				t.Fatalf("refresh=%+v post=%+v", result.ItemSlotRefreshes, result.PostActions)
			}

			character, _, _ := repositories.Character.Load(ctx, "77")
			skill, _, _ := repositories.Skill.Load(ctx, "77")
			inventory, _, _ := repositories.Inventory.Load(ctx, "77")
			wantSP := int64(0)
			wantTP := int64(0)
			if test.wantKind == increaseStatusKindSP {
				wantSP = test.effect.Delta
			} else {
				wantTP = test.effect.Delta
			}
			if character.Stats["bonus_sp"] != wantSP || character.Stats["bonus_tp"] != wantTP ||
				skill.Points.RemainingSP != 100+int(wantSP) || skill.Points.RemainingTP != 10+int(wantTP) ||
				inventory.Slots[slotKey(listTypeMain, 0x42)].Count != 1 {
				t.Fatalf("character=%+v skill=%+v inventory=%+v", character.Stats, skill.Points, inventory.Slots)
			}
		})
	}
}

func TestIncreaseStatusExperienceBookAcknowledgesSilentlyAndRefreshesProgression(t *testing.T) {
	ctx := context.Background()
	repositories := dnfrepomemory.NewMemoryGroup()
	if err := repositories.Character.Save(ctx, dnfrepo.CharacterRecord{
		CharacterID: "77", AccountID: "acc", Job: "2", Level: 1,
		Stats: map[string]int64{"exp": 0, "hp": 100},
	}); err != nil {
		t.Fatal(err)
	}
	if err := repositories.Skill.Save(ctx, dnfrepo.SkillRecord{
		CharacterID: "77", Points: dnfrepo.SkillPointState{SyncedLevel: 1},
	}); err != nil {
		t.Fatal(err)
	}
	const pvfPath = "stackable/book_exp2.stk"
	if err := repositories.Inventory.Save(ctx, dnfrepo.InventoryRecord{
		CharacterID: "77",
		Slots: map[string]dnfrepo.ItemStack{
			slotKey(listTypeMain, 0x43): {
				ItemID: 1035, Count: 1,
				Extra: map[string]string{"item_kind": "stackable", "pvf_path": pvfPath, "stackable_type": "[etc]"},
			},
		},
	}); err != nil {
		t.Fatal(err)
	}
	planner := func(character dnfrepo.CharacterRecord, skill dnfrepo.SkillRecord, effect alignedcmd.DirectExperienceConsumableResolution, updatedAt time.Time) (alignedcmd.DirectExperienceConsumablePlan, error) {
		nextCharacter := dnfrepo.CloneCharacter(character)
		nextCharacter.Level = 2
		nextCharacter.Stats["exp"] = 1000
		nextCharacter.UpdatedAt = updatedAt
		nextSkill := dnfrepo.CloneSkill(skill)
		nextSkill.Points = dnfrepo.SkillPointState{TotalSP: 3, RemainingSP: 3, SyncedLevel: 2}
		nextSkill.UpdatedAt = updatedAt
		return alignedcmd.DirectExperienceConsumablePlan{
			Character:       nextCharacter,
			Skill:           nextSkill,
			CharacterFields: []dnfrepo.CharacterField{dnfrepo.CharacterFieldBase, dnfrepo.CharacterFieldStats},
			SkillFields:     []dnfrepo.SkillField{dnfrepo.SkillFieldPoints},
			Gain:            1000, PreviousLevel: 1, NewLevel: 2, PreviousExperience: 0, NewExperience: 1000, SPGain: 3,
		}, nil
	}
	result, err := (handler{}).Handle(ctx, alignedcmd.Request{
		Opcode:              uint16(dnfenum.CmdPacketIncreaseStatus),
		Body:                []byte{0x43, 0x00},
		AccountID:           "acc",
		SelectedCharacterID: increaseStatusTestCharacterID,
		Repositories:        repositories,
		StackableUseResolver: func(itemID int64) (alignedcmd.StackableUseResolution, error) {
			return alignedcmd.StackableUseResolution{
				ItemID: itemID, PVFPath: pvfPath, StackableType: "[etc]",
				DirectExperience: &alignedcmd.DirectExperienceConsumableResolution{
					ItemID: itemID, PVFPath: pvfPath, EffectType: "[expUp]", EffectValues: []int64{1000},
				},
			}, nil
		},
		DirectExperiencePlanner: planner,
	})
	if err != nil {
		t.Fatal(err)
	}
	// The client has no "gained EXP" branch in op30. The ACK only releases the
	// pending item state; op37/op19 own the bar and the level-up presentation.
	wantACK := []byte{1, 0x43, 0, increaseStatusKindSilent, 0, 0, 0, 0, 0, 0, 0, 0}
	if len(result.UpperResponses) != 1 || !bytes.Equal(result.UpperResponses[0].Body, wantACK) {
		t.Fatalf("ack=%+v want=%x", result.UpperResponses, wantACK)
	}
	if len(result.PostActions) != 1 || result.PostActions[0] != alignedcmd.PostActionRefreshSelectedCharacterProgression ||
		len(result.ItemSlotRefreshes) != 1 || result.ItemSlotRefreshes[0].SlotIndex != 0x43 {
		t.Fatalf("post=%+v refresh=%+v", result.PostActions, result.ItemSlotRefreshes)
	}
	character, _, _ := repositories.Character.Load(ctx, "77")
	inventory, _, _ := repositories.Inventory.Load(ctx, "77")
	if character.Level != 2 || character.Stats["exp"] != 1000 {
		t.Fatalf("character=%+v", character)
	}
	if _, found := inventory.Slots[slotKey(listTypeMain, 0x43)]; found {
		t.Fatalf("source book was not consumed: %+v", inventory.Slots)
	}
}

func TestIncreaseStatusAlwaysRepliesOnEveryRejectionWithoutMutating(t *testing.T) {
	const pvfPath = "stackable/book_skill1.stk"
	for _, test := range []struct {
		name     string
		body     []byte
		resolver alignedcmd.StackableUseResolver
		empty    bool
	}{
		{name: "malformed body", body: []byte{0x42}},
		{name: "negative slot", body: []byte{0xFF, 0xFF}},
		{name: "empty slot", body: []byte{0x10, 0x00}},
		{
			name: "not a book",
			body: []byte{0x42, 0x00},
			resolver: func(itemID int64) (alignedcmd.StackableUseResolution, error) {
				return alignedcmd.StackableUseResolution{ItemID: itemID, PVFPath: pvfPath, StackableType: "[waste]"}, nil
			},
		},
		{
			// Level-up tickets resolve through the same PVF fields but belong
			// to op418, which owns their quest settlement.
			name: "level-up ticket",
			body: []byte{0x42, 0x00},
			resolver: func(itemID int64) (alignedcmd.StackableUseResolution, error) {
				return alignedcmd.StackableUseResolution{
					ItemID: itemID, PVFPath: pvfPath, StackableType: "[etc]",
					DirectExperience: &alignedcmd.DirectExperienceConsumableResolution{
						ItemID: itemID, PVFPath: pvfPath, EffectType: "[level up ticket]", EffectValues: []int64{1},
					},
					QuestCompletionTicket: &alignedcmd.QuestCompletionTicketResolution{ActionType: "[quest clear]"},
				}, nil
			},
		},
	} {
		t.Run(test.name, func(t *testing.T) {
			ctx := context.Background()
			repositories := increaseStatusSkillPointFixture(t, 1031, pvfPath)
			resolver := test.resolver
			if resolver == nil {
				resolver = skillPointBookTestResolver(1031, pvfPath,
					alignedcmd.TPBookResolution{Kind: alignedcmd.SkillPointBookAddSP, Delta: 5, MinimumLevel: 1})
			}
			result, err := (handler{}).Handle(ctx, alignedcmd.Request{
				Opcode:               uint16(dnfenum.CmdPacketIncreaseStatus),
				Body:                 test.body,
				AccountID:            "acc",
				SelectedCharacterID:  increaseStatusTestCharacterID,
				Repositories:         repositories,
				StackableUseResolver: resolver,
				InitialSkillLevels:   map[uint16]int{},
				SkillPointBaseline:   &dnfrepo.SkillPointState{TotalSP: 100, RemainingSP: 100, TotalTP: 10, RemainingTP: 10, SyncedLevel: 50},
			})
			if err != nil {
				t.Fatal(err)
			}
			// Never leave the request unanswered: the client would stay parked
			// in its item wait state and refuse every later window.
			if !result.Handled || !result.ResponseAllowed || len(result.UpperResponses) != 1 ||
				result.UpperResponses[0].MsgID != uint16(dnfenum.CmdPacketIncreaseStatus) ||
				!bytes.Equal(result.UpperResponses[0].Body, []byte{0, increaseStatusFailureCode}) {
				t.Fatalf("result=%+v", result)
			}
			if len(result.ItemSlotRefreshes) != 0 || len(result.PostActions) != 0 {
				t.Fatalf("rejection published state: refresh=%+v post=%+v", result.ItemSlotRefreshes, result.PostActions)
			}
			character, _, _ := repositories.Character.Load(ctx, "77")
			skill, _, _ := repositories.Skill.Load(ctx, "77")
			inventory, _, _ := repositories.Inventory.Load(ctx, "77")
			if character.Stats["bonus_sp"] != 0 || character.Stats["bonus_tp"] != 0 ||
				skill.Points.RemainingSP != 100 || skill.Points.RemainingTP != 10 ||
				inventory.Slots[slotKey(listTypeMain, 0x42)].Count != 2 {
				t.Fatalf("rejected request mutated state character=%+v skill=%+v inventory=%+v", character.Stats, skill.Points, inventory.Slots)
			}
		})
	}
}

func TestIncreaseStatusTPResetBookAcknowledgesSilently(t *testing.T) {
	// The client has no TP-reset branch either; the reset shows through op19.
	ack := increaseStatusAck(increaseStatusTestSourceSlot, UseStackableResult{
		TPBookApplied: true, SkillPointBookTPReset: true,
	})
	want := []byte{1, byte(increaseStatusTestSourceSlot), 0, increaseStatusKindSilent, 0, 0, 0, 0, 0, 0, 0, 0}
	if !bytes.Equal(ack, want) {
		t.Fatalf("ack=%x want=%x", ack, want)
	}
}
