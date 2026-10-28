package dnfbridge

import (
	"context"
	"encoding/hex"
	"errors"
	"fmt"
	"strings"
	"testing"

	dnfavatarcompound "longheng.io/server/internal/modules/dnf/avatarcompound"
	dnfrepo "longheng.io/server/internal/modules/dnf/repository"
	dnfrepomemory "longheng.io/server/internal/modules/dnf/repository/memory"
	platformpvf "longheng.io/server/internal/platform/pvf"
)

type avatarCompoundPVFTestSource map[string]string

func (source avatarCompoundPVFTestSource) ReadText(relativePath string) (string, error) {
	for path, text := range source {
		if strings.EqualFold(path, relativePath) {
			return text, nil
		}
	}
	return "", fmt.Errorf("missing %s", relativePath)
}

func TestCurrentAvatarCompoundParsesObservedNoPackRequest(t *testing.T) {
	body, err := hex.DecodeString("7500150015ecb406160015ecb40675ebb40602000000")
	if err != nil {
		t.Fatal(err)
	}
	request, err := parseCurrentAvatarCompoundRequest(body)
	if err != nil {
		t.Fatal(err)
	}
	if request.ConsumeSlot != 117 || request.Slot1 != 21 || request.ExpectedItemID1 != 112520213 ||
		request.Slot2 != 22 || request.ExpectedItemID2 != 112520213 || request.RequestedItemID != 112520053 ||
		request.AbilityNo != 2 || request.MaterialClass != 0 {
		t.Fatalf("request=%+v", request)
	}
}

func TestCurrentAvatarCompoundSuccessBodyMatchesIDAClass1Op99(t *testing.T) {
	request := currentAvatarCompoundRequest{ConsumeSlot: 117, Slot1: 21, Slot2: 22, AbilityNo: 2}
	body := buildCurrentAvatarCompoundSuccessBody(request, currentAvatarCompoundResult{
		NewSlots:   []int16{21},
		NewItemIDs: []uint32{112520053},
	})
	wantHex := "0103011500010000000116000100000000750001000000" +
		"150075ebb4060000000002001e000000" + strings.Repeat("00", 30) + "04000000" + strings.Repeat("00", 4) +
		"ffff000000000000000002001e000000" + strings.Repeat("00", 30) + "04000000" + strings.Repeat("00", 4)
	if got := hex.EncodeToString(body); got != wantHex {
		t.Fatalf("body=%s\nwant=%s", got, wantHex)
	}
	if got := hex.EncodeToString(currentAvatarCompoundFailureBody()); got != "001600" {
		t.Fatalf("failure=%s", got)
	}
}

func TestCurrentAvatarCompoundPVFReadsCurrentMaterialsAndRates(t *testing.T) {
	source := newAvatarCompoundPVFTestSource(950, 325)
	config, err := loadCurrentAvatarCompoundConfig(source, 0)
	if err != nil {
		t.Fatal(err)
	}
	if config.NormalMaterialID != 21 || config.UpperMaterialID != 899 || config.MasterMaterialID != 490702265 {
		t.Fatalf("materials=(%d,%d,%d)", config.NormalMaterialID, config.UpperMaterialID, config.MasterMaterialID)
	}
	part, found := currentAvatarCompoundRequestedPart(config, 2004)
	if !found || part != "coat" {
		t.Fatalf("part=%q found=%t", part, found)
	}
	for _, test := range []struct {
		name       string
		grade1     int64
		grade2     int64
		materialID uint32
		want       int64
	}{
		{name: "ordinary", grade1: 1, grade2: 1, materialID: 21, want: 950},
		{name: "mixed-golden", grade1: 1, grade2: 2, materialID: 899, want: 1312},
		{name: "upper-master", grade1: 2, grade2: 2, materialID: 490702265, want: 1675},
	} {
		t.Run(test.name, func(t *testing.T) {
			got, rateErr := currentAvatarCompoundRareRate(config, "coat", test.grade1, test.grade2, test.materialID)
			if rateErr != nil || got != test.want {
				t.Fatalf("rate=%d err=%v want=%d", got, rateErr, test.want)
			}
		})
	}
}

func TestRealPVFObservedAvatarCompoundRequestIsFullyResolvable(t *testing.T) {
	archive, err := platformpvf.Open(realLukeRewardPVFPath())
	if err != nil {
		t.Skipf("runtime Script.pvf unavailable: %v", err)
	}
	catalog, err := newPVFDungeonDropCatalog(archive)
	if err != nil {
		t.Fatal(err)
	}
	const sourceItemID uint32 = 112520213
	const requestedItemID uint32 = 112520053
	sourceDefinition, sourceDocument, err := currentAvatarCompoundDefinition(catalog, sourceItemID)
	if err != nil {
		t.Fatalf("source item: %v", err)
	}
	requestedDefinition, requestedDocument, err := currentAvatarCompoundDefinition(catalog, requestedItemID)
	if err != nil {
		t.Fatalf("requested item: %v", err)
	}
	sourcePart, sourcePartOK := currentAvatarCompoundDocumentPart(sourceDocument)
	requestedPart, requestedPartOK := currentAvatarCompoundDocumentPart(requestedDocument)
	if !sourcePartOK || !requestedPartOK || sourcePart != requestedPart || sourceDefinition.Grade < 0 || requestedDefinition.Grade < 0 {
		t.Fatalf("source=(%s,%d) requested=(%s,%d)", sourcePart, sourceDefinition.Grade, requestedPart, requestedDefinition.Grade)
	}
	matchedJob := -1
	for job := 0; job < 16; job++ {
		config, loadErr := loadCurrentAvatarCompoundConfig(archive, byte(job))
		if loadErr != nil {
			t.Fatalf("job=%d config: %v", job, loadErr)
		}
		if part, found := currentAvatarCompoundRequestedPart(config, requestedItemID); found && part == requestedPart {
			matchedJob = job
			for _, materialID := range []uint32{config.NormalMaterialID, config.UpperMaterialID, config.MasterMaterialID} {
				definition, resolveErr := catalog.ResolveItem(materialID)
				if resolveErr != nil || definition.Kind != dungeonDropItemStackable {
					t.Fatalf("job=%d material=%d definition=%+v err=%v", job, materialID, definition, resolveErr)
				}
			}
			break
		}
	}
	if matchedJob < 0 {
		t.Fatalf("requested item=%d part=%s is absent from all current compound-avatar job pools", requestedItemID, requestedPart)
	}
	t.Logf("observed request resolves through job=%d part=%s", matchedJob, requestedPart)
}

func TestCurrentAvatarCompoundCommitsThreeConsumesAndOneOutput(t *testing.T) {
	ctx := context.Background()
	source := newAvatarCompoundPVFTestSource(0, 0)
	source["equipment/source1.equ"] = "[equipment type] `[coat avatar]`\n[grade] 1\n"
	source["equipment/source2.equ"] = "[equipment type] `[coat avatar]`\n[grade] 2\n"
	source["equipment/output.equ"] = "[equipment type] `[coat avatar]`\n[grade] 2\n"
	repositories := dnfrepomemory.NewMemoryGroup()
	if err := repositories.Character.Save(ctx, dnfrepo.CharacterRecord{CharacterID: "19", AccountID: "dnf:1", Job: "0"}); err != nil {
		t.Fatal(err)
	}
	if err := repositories.Inventory.Save(ctx, dnfrepo.InventoryRecord{CharacterID: "19", Slots: map[string]dnfrepo.ItemStack{
		"0:117": {ItemID: 21, Count: 2},
		"1:21":  {ItemID: 3001, Count: 1},
		"1:22":  {ItemID: 3002, Count: 1},
	}}); err != nil {
		t.Fatal(err)
	}
	service := &Service{options: options{accountPrefix: defaultAccountPrefix}, repositoryProvider: func() (dnfrepo.Group, bool) { return repositories, true }}
	session := &gameSession{accountID: "dnf:1", selectedCharacterID: 19}
	catalog := &pvfDungeonDropCatalog{
		source: source,
		itemRefs: map[uint32]dungeonDropItemReference{
			21:   {kind: dungeonDropItemStackable, path: "tool.stk"},
			3001: {kind: dungeonDropItemEquipment, path: "source1.equ"},
			3002: {kind: dungeonDropItemEquipment, path: "source2.equ"},
			2004: {kind: dungeonDropItemEquipment, path: "output.equ"},
		},
		itemCache: map[uint32]dungeonDropItemDefinition{
			21:   {ItemID: 21, Kind: dungeonDropItemStackable, PVFPath: "stackable/tool.stk", StackableType: "[material]"},
			3001: {ItemID: 3001, Kind: dungeonDropItemEquipment, PVFPath: "equipment/source1.equ", EquipmentType: "[coat avatar]", Grade: 1},
			3002: {ItemID: 3002, Kind: dungeonDropItemEquipment, PVFPath: "equipment/source2.equ", EquipmentType: "[coat avatar]", Grade: 2},
			2004: {ItemID: 2004, Kind: dungeonDropItemEquipment, PVFPath: "equipment/output.equ", EquipmentType: "[coat avatar]", Grade: 2},
		},
	}
	request := currentAvatarCompoundRequest{
		ConsumeSlot: 117, Slot1: 21, ExpectedItemID1: 3001, Slot2: 22, ExpectedItemID2: 3002,
		RequestedItemID: 2004, AbilityNo: 7,
	}
	result, err := service.commitCurrentAvatarCompound(ctx, session, catalog, request)
	if err != nil {
		t.Fatal(err)
	}
	if len(result.NewItemIDs) != 1 || result.NewItemIDs[0] != 2004 || len(result.NewSlots) != 1 || result.NewSlots[0] != 21 || result.MaterialRemain != 1 {
		t.Fatalf("result=%+v", result)
	}
	inventory, found, err := repositories.Inventory.Load(ctx, "19")
	if err != nil || !found {
		t.Fatalf("load found=%t err=%v", found, err)
	}
	if inventory.Slots["0:117"].Count != 1 {
		t.Fatalf("tool=%+v", inventory.Slots["0:117"])
	}
	if _, found := inventory.Slots["1:22"]; found {
		t.Fatalf("second source remains: %+v", inventory.Slots["1:22"])
	}
	output := inventory.Slots["1:21"]
	if output.ItemID != 2004 || output.Count != 1 || output.Extra["avatar_ability_no"] != "7" || len(output.RawEntry) != currentItemListEntryWireSize {
		t.Fatalf("output=%+v", output)
	}
}

func TestCurrentAvatarCompoundStaleSourceRollsBackEverything(t *testing.T) {
	ctx := context.Background()
	repositories := dnfrepomemory.NewMemoryGroup()
	if err := repositories.Character.Save(ctx, dnfrepo.CharacterRecord{CharacterID: "19", AccountID: "dnf:1", Job: "0"}); err != nil {
		t.Fatal(err)
	}
	original := dnfrepo.InventoryRecord{CharacterID: "19", Slots: map[string]dnfrepo.ItemStack{
		"0:117": {ItemID: 21, Count: 2}, "1:21": {ItemID: 9999, Count: 1}, "1:22": {ItemID: 3002, Count: 1},
	}}
	if err := repositories.Inventory.Save(ctx, original); err != nil {
		t.Fatal(err)
	}
	owner, err := dnfavatarcompound.NewOwner(repositories)
	if err != nil {
		t.Fatal(err)
	}
	err = owner.Compound(ctx, dnfavatarcompound.Command{AccountID: "dnf:1", CharacterID: "19", Project: func(assets *dnfavatarcompound.Assets) error {
		if !currentAvatarCompoundSourceStackValid(assets.Inventory.Slots["1:21"], true, 3001) {
			return errCurrentAvatarCompoundSourceInvalid
		}
		delete(assets.Inventory.Slots, "0:117")
		return nil
	}})
	if !errors.Is(err, errCurrentAvatarCompoundSourceInvalid) {
		t.Fatalf("error=%v", err)
	}
	inventory, _, _ := repositories.Inventory.Load(ctx, "19")
	if inventory.Slots["0:117"].Count != 2 || inventory.Slots["1:21"].ItemID != 9999 || inventory.Slots["1:22"].ItemID != 3002 {
		t.Fatalf("rollback slots=%+v", inventory.Slots)
	}
}

func newAvatarCompoundPVFTestSource(rareRate, upperRate int64) avatarCompoundPVFTestSource {
	parts := []string{"hat", "hair", "face", "neck", "coat", "pants", "belt", "shoes"}
	var rates strings.Builder
	var upperRates strings.Builder
	var pools strings.Builder
	for index, part := range parts {
		fmt.Fprintf(&rates, "`%s` %d ", part, rareRate)
		fmt.Fprintf(&upperRates, "`%s` %d ", part, upperRate)
		fmt.Fprintf(&pools, "[%s avatar]\n1 %d 10 %d 1000\n[/%s avatar]\n", part, 1000+index, 2000+index, part)
	}
	job := "[grade]\n1\n[rare rate]\n" + rates.String() + "\n[/rare rate]\n" +
		"[upper grade]\n2\n[upper rare rate]\n" + upperRates.String() + "\n[/upper rare rate]\n" + pools.String() +
		"[common bind cube]\n[material]\n1 999 1\n[/material]\n[/common bind cube]\n" +
		"[material]\n1 21 1\n[/material]\n[material_upper]\n1 899 1\n[/material_upper]\n" +
		"[material_master]\n1 490702265 1\n[/material_master]\n"
	return avatarCompoundPVFTestSource{
		"etc/compoundavatar.etc":          "1 `CompoundAvatar_swordman.etc`\n",
		"etc/CompoundAvatar_swordman.etc": job,
		"etc/serverparameter.etc":         "[upper bind cube rate bonus]\n200\n[master bind cube rate bonus]\n400\n",
	}
}
