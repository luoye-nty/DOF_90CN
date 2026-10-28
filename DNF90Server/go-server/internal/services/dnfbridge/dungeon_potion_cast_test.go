package dnfbridge

import (
	"encoding/binary"
	"os"
	"testing"
	"time"

	dnfpvf "longheng.io/server/internal/modules/dnf/pvf"
	dnfrepo "longheng.io/server/internal/modules/dnf/repository"
	platformpvf "longheng.io/server/internal/platform/pvf"
)

func TestParseCurrentConsumableRecoveryReadsOperatorAmountAndDuration(t *testing.T) {
	// Shapes copied from the runtime PVF: a flat HP potion, a percentage
	// consumable that restores both pools, and a material with no section.
	for name, probe := range map[string]struct {
		text                       string
		wantHP, wantMP             bool
		hpOperator, mpOperator     string
		hpAmount, mpAmount         int64
		hpDurationMS, mpDurationMS int64
	}{
		"flat_hp_only": {
			text:       "[name]\n`新手HP药剂`\n[stackable type]\n`[waste]` 4\n[hp recovery]\n`+` 70 1000\n",
			wantHP:     true,
			hpOperator: "+", hpAmount: 70, hpDurationMS: 1000,
		},
		"percent_both_pools": {
			text:   "[name]\n`甜蜜草莓汁`\n[stackable type]\n`[waste]` 4\n[hp recovery]\n`%` 20 500\n[mp recovery]\n`%` 20 500\n",
			wantHP: true, wantMP: true,
			hpOperator: "%", hpAmount: 20, hpDurationMS: 500,
			mpOperator: "%", mpAmount: 20, mpDurationMS: 500,
		},
		"material_without_recovery": {
			text: "[name]\n`无色小晶块`\n[stackable type]\n`[material]` 2\n",
		},
	} {
		document, err := dnfpvf.Parse("stackable/probe.stk", probe.text)
		if err != nil {
			t.Fatalf("%s: %v", name, err)
		}
		definition := dungeonDropItemDefinition{}
		parseCurrentConsumableRecovery(document, &definition)
		if definition.HasHPRecovery != probe.wantHP || definition.HasMPRecovery != probe.wantMP {
			t.Fatalf("%s: hp_found=%t mp_found=%t want %t/%t", name,
				definition.HasHPRecovery, definition.HasMPRecovery, probe.wantHP, probe.wantMP)
		}
		if definition.HPRecoveryOperator != probe.hpOperator || definition.HPRecoveryAmount != probe.hpAmount ||
			definition.HPRecoveryDurationMS != probe.hpDurationMS ||
			definition.MPRecoveryOperator != probe.mpOperator || definition.MPRecoveryAmount != probe.mpAmount ||
			definition.MPRecoveryDurationMS != probe.mpDurationMS {
			t.Fatalf("%s: parsed=%+v", name, definition)
		}
	}
}

// TestCurrentPotionRecoveryAmountKeepsPVFSemantics pins the conversion from a
// declared recovery to the single scalar the recovery frame carries.
func TestCurrentPotionRecoveryAmountKeepsPVFSemantics(t *testing.T) {
	character := dnfrepo.CharacterRecord{Stats: map[string]int64{"stat_hp_max": 11600}}
	poolless := dnfrepo.CharacterRecord{Stats: map[string]int64{}}
	for name, probe := range map[string]struct {
		declared   bool
		operator   string
		amount     int64
		character  dnfrepo.CharacterRecord
		wantAmount uint16
		wantReason string
	}{
		"flat_is_the_declared_amount":  {true, "+", 1520, character, 1520, ""},
		"percent_scales_the_pool":      {true, "%", 20, character, 2320, ""},
		"percent_without_pool_refuses": {true, "%", 20, poolless, 0, "percent_recovery_without_known_maximum"},
		// The archive carries `% -7` on a [virtual] item. A negative or zero
		// declaration is damage or a no-op, never a heal.
		"negative_never_heals":  {true, "%", -7, character, 0, ""},
		"absent_section_is_nil": {false, "+", 500, character, 0, ""},
		"unknown_operator":      {true, "*", 500, character, 0, "unproven_recovery_operator"},
	} {
		amount, reason := currentPotionRecoveryAmount(probe.declared, probe.operator, probe.amount, probe.character, "stat_hp_max")
		if amount != probe.wantAmount || reason != probe.wantReason {
			t.Fatalf("%s: amount=%d reason=%q want %d/%q", name, amount, reason, probe.wantAmount, probe.wantReason)
		}
	}
}

func TestCurrentPotionRecoveryBodyMatchesMode1Reader(t *testing.T) {
	body := buildCurrentDungeonPotionRecoveryBody(0x1234, 700)
	want := []byte{0x34, 0x12, 0x00, 0x00, 0x01, 0xff, 0x00, 0x34, 0x12, 0xbc, 0x02}
	if len(body) != len(want) {
		t.Fatalf("body=%x len=%d want len=%d", body, len(body), len(want))
	}
	for index := range want {
		if body[index] != want[index] {
			t.Fatalf("body=%x want=%x", body, want)
		}
	}
	// The zero-drop death body is the same reader with mode 0. Keeping the
	// shared prefix identical is what makes the mode byte the only difference.
	death, err := buildCurrentDungeonDeathNotificationBody(0x1234, currentDungeonDeathResponseMonster)
	if err != nil {
		t.Fatal(err)
	}
	if len(death) != currentDungeonZeroDropDeathBodySize || death[4] != 0 || body[4] != 1 {
		t.Fatalf("death=%x recovery=%x", death, body)
	}
}

func TestCurrentPotionCastPairingWindowConsumesTheDeleteExactlyOnce(t *testing.T) {
	service := &Service{}
	session := &gameSession{}
	base := time.Unix(1_800_000_000, 0).UTC()
	arm := func() {
		session.potionCast.pending = &currentPendingConsumableUse{itemID: 3001, listType: 0, slotIndex: 12, committedAt: base}
	}

	arm()
	if pending := session.takeCurrentConsumableDeleteIfFresh(base.Add(50 * time.Millisecond)); pending == nil || pending.itemID != 3001 {
		t.Fatalf("fresh delete was not returned: %+v", pending)
	}
	// A second cast must not reuse the same dose.
	if pending := session.takeCurrentConsumableDeleteIfFresh(base.Add(60 * time.Millisecond)); pending != nil {
		t.Fatalf("delete answered a second cast: %+v", pending)
	}
	arm()
	if pending := session.takeCurrentConsumableDeleteIfFresh(base.Add(currentPotionCastWindow + time.Second)); pending != nil {
		t.Fatalf("stale delete was claimed: %+v", pending)
	}
	// An expired record is still cleared, so it cannot resurface later.
	if session.potionCast.pending != nil {
		t.Fatal("expired delete stayed pending")
	}
	// A delete with no resolvable item is never recorded at all.
	service.noteCurrentConsumableDelete(session, 0, 12, 0)
	if session.potionCast.pending != nil {
		t.Fatal("item-less delete was recorded")
	}
}

func TestCurrentPotionCastIgnoresOrdinarySkillsAndShapes(t *testing.T) {
	service := &Service{}
	session := &gameSession{}
	ordinary := make([]byte, currentLocalDebugUseSkillRequestSize)
	binary.BigEndian.PutUint16(ordinary[0:2], 77)
	if handled, err := service.handleCurrentDungeonPotionCast(session, ordinary); handled || err != nil {
		t.Fatalf("ordinary skill cast was claimed: handled=%t err=%v", handled, err)
	}
	potion := make([]byte, currentLocalDebugUseSkillRequestSize)
	binary.BigEndian.PutUint16(potion[0:2], currentPotionCastSkillID)
	// Outside a dungeon runtime there is nothing to answer.
	if handled, err := service.handleCurrentDungeonPotionCast(session, potion); handled || err != nil {
		t.Fatalf("potion cast outside a dungeon was claimed: handled=%t err=%v", handled, err)
	}
	if handled, err := service.handleCurrentDungeonPotionCast(session, potion[:5]); handled || err != nil {
		t.Fatalf("short body was claimed: handled=%t err=%v", handled, err)
	}
}

// TestCurrentPotionRealPVFRecoveryIsAmountNotRange is the evidence for reading
// `[hp recovery]` as `<operator> <amount> <duration>`. The twelve ordinary
// potions are the proof: their second value is a constant 1000 while the first
// climbs in tier order, and two of them declare a first value LARGER than the
// second, which no minimum/maximum pair could be.
func TestCurrentPotionRealPVFRecoveryIsAmountNotRange(t *testing.T) {
	pvfPath := os.Getenv("DNFBRIDGE_REAL_PVF_SMOKE")
	if pvfPath == "" {
		t.Skip("set DNFBRIDGE_REAL_PVF_SMOKE to validate potion recovery parsing")
	}
	archive, err := platformpvf.LoadArchive(platformpvf.Options{Path: pvfPath})
	if err != nil {
		t.Fatal(err)
	}
	catalog, err := newPVFDungeonDropCatalog(archive)
	if err != nil {
		t.Fatal(err)
	}

	wantHP := map[string]int64{
		"ptn_hplowest.stk":       70,
		"ptn_hplow.stk":          180,
		"ptn_hpintermediate.stk": 410,
		"ptn_hpupper.stk":        710,
		"ptn_hphighest.stk":      1080,
		"ptn_hpspecial.stk":      1520,
	}
	wantMP := map[string]int64{
		"ptn_mplowest.stk":       65,
		"ptn_mplow.stk":          150,
		"ptn_mpintermediate.stk": 330,
		"ptn_mpupper.stk":        560,
		"ptn_mphighest.stk":      850,
		"ptn_mpspecial.stk":      1180,
	}
	seenHP, seenMP := 0, 0
	inverted := 0
	for itemID, reference := range catalog.itemRefs {
		if reference.kind != dungeonDropItemStackable {
			continue
		}
		expectHP, isHP := wantHP[reference.path]
		expectMP, isMP := wantMP[reference.path]
		if !isHP && !isMP {
			continue
		}
		definition, resolveErr := catalog.ResolveItem(itemID)
		if resolveErr != nil {
			t.Fatalf("resolve %s: %v", reference.path, resolveErr)
		}
		if !currentPotionWasteStackable(definition.StackableType) {
			t.Fatalf("%s stackable type=%q, want [waste]", reference.path, definition.StackableType)
		}
		if isHP {
			seenHP++
			if !definition.HasHPRecovery || definition.HPRecoveryOperator != "+" ||
				definition.HPRecoveryAmount != expectHP || definition.HPRecoveryDurationMS != 1000 {
				t.Fatalf("%s hp=%q %d/%d, want + %d/1000", reference.path,
					definition.HPRecoveryOperator, definition.HPRecoveryAmount, definition.HPRecoveryDurationMS, expectHP)
			}
			if definition.HasMPRecovery {
				t.Fatalf("%s declares mp recovery", reference.path)
			}
			if definition.HPRecoveryAmount > definition.HPRecoveryDurationMS {
				inverted++
			}
		}
		if isMP {
			seenMP++
			if !definition.HasMPRecovery || definition.MPRecoveryOperator != "+" ||
				definition.MPRecoveryAmount != expectMP || definition.MPRecoveryDurationMS != 1000 {
				t.Fatalf("%s mp=%q %d/%d, want + %d/1000", reference.path,
					definition.MPRecoveryOperator, definition.MPRecoveryAmount, definition.MPRecoveryDurationMS, expectMP)
			}
			if definition.MPRecoveryAmount > definition.MPRecoveryDurationMS {
				inverted++
			}
		}
	}
	if seenHP != len(wantHP) || seenMP != len(wantMP) {
		t.Fatalf("resolved hp=%d/%d mp=%d/%d ordinary potions", seenHP, len(wantHP), seenMP, len(wantMP))
	}
	if inverted == 0 {
		t.Fatal("no potion has first value > second; the min/max reading would still be tenable and this test proves nothing")
	}
}
