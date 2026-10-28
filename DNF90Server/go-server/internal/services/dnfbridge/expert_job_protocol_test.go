package dnfbridge

import (
	"bytes"
	"context"
	"encoding/binary"
	"errors"
	"testing"

	dnfexpertjob "longheng.io/server/internal/modules/dnf/expertjob"
	dnfrepo "longheng.io/server/internal/modules/dnf/repository"
	dnfrepomemory "longheng.io/server/internal/modules/dnf/repository/memory"
)

func TestCurrentExpertJobRequestsMatchCurrentEXELayout(t *testing.T) {
	compoundBody := make([]byte, 8)
	binary.LittleEndian.PutUint32(compoundBody[0:4], 2600149)
	binary.LittleEndian.PutUint16(compoundBody[4:6], 3)
	binary.LittleEndian.PutUint16(compoundBody[6:8], uint16(0xffff))
	compound, err := parseCurrentExpertJobCompoundRequest(compoundBody)
	if err != nil || compound.RecipeItemID != 2600149 || compound.Count != 3 || compound.CardSlot != -1 {
		t.Fatalf("compound=%+v error=%v", compound, err)
	}
	binary.LittleEndian.PutUint16(compoundBody[4:6], 1)
	binary.LittleEndian.PutUint16(compoundBody[6:8], 12)
	cardCompound, err := parseCurrentExpertJobCompoundRequest(compoundBody)
	if err != nil || cardCompound.CardSlot != 12 || cardCompound.Count != 1 {
		t.Fatalf("card compound=%+v error=%v", cardCompound, err)
	}
	// 【2026-09-18】原来"卡片槽 + 数量!=1"会被判非法（附魔机界面选 50/49 时必被拒，
	// 回失败码 19）。制作宝珠改成按数量循环后，这里也必须放行同一个请求体。
	binary.LittleEndian.PutUint16(compoundBody[4:6], 49)
	cardBatch, err := parseCurrentExpertJobCompoundRequest(compoundBody)
	if err != nil || cardBatch.CardSlot != 12 || cardBatch.Count != 49 {
		t.Fatalf("card batch compound=%+v error=%v", cardBatch, err)
	}
	binary.LittleEndian.PutUint16(compoundBody[4:6], 0)
	if _, err := parseCurrentExpertJobCompoundRequest(compoundBody); err == nil {
		t.Fatal("zero-count compound request accepted")
	}
	extractionBody := []byte{2, 9, 0, 0, 10, 0}
	extraction, err := parseCurrentExpertJobExtractionRequest(extractionBody)
	if err != nil || extraction.ExtractorType != 2 || extraction.ExtractorSlot != 9 || extraction.TargetList != 0 || extraction.TargetSlot != 10 {
		t.Fatalf("extraction=%+v error=%v", extraction, err)
	}
}

func TestCurrentExpertJobModuleRegistersCompleteCurrentEXECommandChain(t *testing.T) {
	module := expertJobGameplayModule()
	opcodes := []struct {
		name       string
		opcode     uint16
		normalized bool
	}{
		{name: "store disjoint", opcode: 235},
		{name: "repair disjoint machine", opcode: 236},
		{name: "compound", opcode: 238, normalized: true},
		{name: "give up", opcode: 239, normalized: true},
		{name: "upgrade disjoint machine", opcode: 240},
		{name: "extract", opcode: 416, normalized: true},
		{name: "create store", opcode: 598},
		{name: "enter store", opcode: 599},
		{name: "close store", opcode: 600},
		{name: "use enchant store", opcode: 601},
		{name: "repair expert-job store", opcode: 611},
	}
	if len(module.LegacyHandlers) != len(opcodes) || len(module.UpperHandlers) != len(opcodes) {
		t.Fatalf("expert-job routes legacy=%d upper=%d want=%d", len(module.LegacyHandlers), len(module.UpperHandlers), len(opcodes))
	}
	for _, opcode := range opcodes {
		t.Run(opcode.name, func(t *testing.T) {
			if module.LegacyHandlers[opcode.opcode] == nil || module.UpperHandlers[opcode.opcode] == nil {
				t.Fatalf("opcode %d is not registered in both gameplay routes", opcode.opcode)
			}
			if normalized := module.LegacyNormalizers[opcode.opcode] != nil; normalized != opcode.normalized {
				t.Fatalf("opcode %d legacy normalizer=%t want=%t", opcode.opcode, normalized, opcode.normalized)
			}
		})
	}
}

func TestCurrentExpertJobStoreLayoutsMatch86JPReaders(t *testing.T) {
	create := make([]byte, 18)
	create[0] = currentExpertJobEnchantStoreKind
	binary.LittleEndian.PutUint32(create[1:5], 3)
	copy(create[5:8], []byte("abc"))
	binary.LittleEndian.PutUint32(create[8:12], 500)
	binary.LittleEndian.PutUint16(create[12:14], uint16(120))
	binary.LittleEndian.PutUint16(create[14:16], uint16(220))
	binary.LittleEndian.PutUint16(create[16:18], uint16(5))
	request, err := parseCurrentExpertJobStoreCreateRequest(create)
	if err != nil || request.Kind != currentExpertJobEnchantStoreKind || string(request.Name) != "abc" || request.Cost != 500 || request.PositionX != 120 || request.PositionY != 220 || request.OpaqueObjectLink != 5 {
		t.Fatalf("create request=%+v error=%v", request, err)
	}
	store := &currentExpertJobStore{OwnerCharacterID: 19, Kind: currentExpertJobEnchantStoreKind, Name: []byte("abc"), Cost: 500, TownID: 38, AreaID: 2, PositionX: 120, PositionY: 220, Endurance: 297, Qualifications: []byte{0, 1}}
	// Trailing mode byte per the 86 reference: mode=0 turns the owner's existing
	// town clone into the store (no second entity), mode=1/2 leave a residual
	// clone. Verified live on 2026-09-22 with no client patch injected - the store
	// stays visible, clickable and usable on both sides without the stand-in body;
	// see the constant's comment.
	wantCreate := []byte{3, 19, 0, 3, 0, 0, 0, 'a', 'b', 'c', 38, 2, 0, 0, 0, 120, 0, 220, 0, 0xf4, 1, 0, 0, 0, 2, 0, 1}
	if got := buildCurrentExpertJobStoreCreateNotification(store); !bytes.Equal(got, wantCreate) {
		t.Fatalf("create notification=%x want=%x", got, wantCreate)
	}
	wantUpdate := []byte{3, 19, 0, 3, 0, 0, 0, 'a', 'b', 'c', 2, 0, 1}
	if got := buildCurrentExpertJobStoreUpdateNotification(store); !bytes.Equal(got, wantUpdate) {
		t.Fatalf("update notification=%x want=%x", got, wantUpdate)
	}
	wantEnter := []byte{1, 3, 19, 0, 0x29, 1, 0, 0}
	if got := buildCurrentExpertJobStoreEnterSuccess(store); !bytes.Equal(got, wantEnter) {
		t.Fatalf("enter notification=%x want=%x", got, wantEnter)
	}
	disjoint, err := parseCurrentExpertJobStoreDisjointRequest([]byte{19, 0, 10, 0, 0})
	if err != nil || disjoint.OwnerID != 19 || disjoint.TargetSlot != 10 || disjoint.TargetList != 0 {
		t.Fatalf("disjoint request=%+v error=%v", disjoint, err)
	}
	// 【批量分解 2026-09-23】op235 批量信封的契约：
	//   u16 ownerID | u16 0xFFFF | u8 listType | u32 "B35\x01" | u8 count | i16 slots[count]
	// 三条硬要求：(1) 合法信封被批量解析器接住；(2) 5 字节单件请求绝不被误判成批量；
	// (3) 批量信封绝不被单件解析器接收（否则会当成 TargetSlot=-1 的怪请求）。
	batchBody := make([]byte, currentExpertJobStoreDisjointBatchHeaderSize+2*3)
	batchSentinel := currentExpertJobStoreDisjointBatchSentinel // via a variable: uint16(-1) as a constant is an overflow error
	binary.LittleEndian.PutUint16(batchBody[0:2], 19)
	binary.LittleEndian.PutUint16(batchBody[2:4], uint16(batchSentinel))
	batchBody[4] = dnfrepo.MainInventoryListType
	copy(batchBody[5:9], currentExpertJobStoreDisjointBatchMarker[:])
	batchBody[9] = 3
	for index, slot := range []int16{9, 10, 16} {
		binary.LittleEndian.PutUint16(batchBody[10+index*2:12+index*2], uint16(slot))
	}
	batch, isBatch, err := parseCurrentExpertJobStoreDisjointBatchRequest(batchBody)
	if err != nil || !isBatch || batch.OwnerID != 19 || batch.TargetList != dnfrepo.MainInventoryListType ||
		len(batch.Slots) != 3 || batch.Slots[0] != 9 || batch.Slots[1] != 10 || batch.Slots[2] != 16 {
		t.Fatalf("batch request=%+v isBatch=%t error=%v", batch, isBatch, err)
	}
	if _, isBatch, err := parseCurrentExpertJobStoreDisjointBatchRequest([]byte{19, 0, 10, 0, 0}); isBatch || err != nil {
		t.Fatalf("single request misread as batch: isBatch=%t error=%v", isBatch, err)
	}
	if _, err := parseCurrentExpertJobStoreDisjointRequest(batchBody); err == nil {
		t.Fatal("batch envelope must not be accepted by the single-item parser")
	}
	if _, isBatch, err := parseCurrentExpertJobStoreDisjointBatchRequest(batchBody[:len(batchBody)-1]); !isBatch || err == nil {
		t.Fatalf("truncated batch isBatch=%t error=%v", isBatch, err)
	}
	duplicateSlot := append([]byte(nil), batchBody...)
	binary.LittleEndian.PutUint16(duplicateSlot[12:14], 9)
	if _, isBatch, err := parseCurrentExpertJobStoreDisjointBatchRequest(duplicateSlot); !isBatch || err == nil {
		t.Fatalf("duplicate-slot batch isBatch=%t error=%v", isBatch, err)
	}
	if len(batchBody) != currentExpertJobStoreDisjointBatchHeaderSize+3*2 {
		t.Fatalf("batch envelope length=%d", len(batchBody))
	}
	// 耐久失败码必须落在客户端 op235 显式表里：190 = dstr 44069。
	// 原来的 189 在显式表和通用映射器里都不存在，客户端 `xor al,al; ret`，什么都不画。
	if got := currentExpertJobStoreError(dnfexpertjob.ErrMachineEndurance); got != 190 {
		t.Fatalf("endurance failure code=%d want 190 (dstr 44069); 189 renders no message", got)
	}
	enchantBody := make([]byte, 13)
	binary.LittleEndian.PutUint16(enchantBody[0:2], 19)
	binary.LittleEndian.PutUint32(enchantBody[2:6], 10015129)
	enchantBody[6], enchantBody[7] = 2, 0
	binary.LittleEndian.PutUint16(enchantBody[8:10], 10)
	enchantBody[10] = 0
	binary.LittleEndian.PutUint16(enchantBody[11:13], 11)
	enchant, err := parseCurrentExpertJobStoreEnchantRequest(enchantBody)
	if err != nil || enchant.OwnerID != 19 || enchant.RecipeID != 10015129 || enchant.TargetSlot != 10 || enchant.CardSlot != 11 {
		t.Fatalf("enchant request=%+v error=%v", enchant, err)
	}
	disjointResult := buildCurrentExpertJobStoreDisjointSuccessBody(
		currentExpertJobStoreDisjointRequest{OwnerID: 19, TargetSlot: 10, TargetList: 0},
		currentExpertJobStoreUseResult{RequesterGold: 900, Endurance: 299, Materials: []currentExpertJobExtractionMaterial{{Slot: 11, ItemID: 2610024, Count: 5}}},
	)
	wantDisjointResult := []byte{1, 10, 0, 0, 19, 0, 1, 11, 0, 0x68, 0xd3, 0x27, 0, 5, 0, 0, 0, 0x84, 3, 0, 0, 0x2b, 1, 0, 0}
	if !bytes.Equal(disjointResult, wantDisjointResult) {
		t.Fatalf("disjoint result=%x want=%x", disjointResult, wantDisjointResult)
	}
	// Eleven bytes: the leading container byte the class-1 pump consumes as a2,
	// then success/experience/reserved/endurance. Shortening this to ten made a
	// live client render the machine's 耐久 as 805306369 - see the comment on
	// buildCurrentExpertJobStoreEnchantSuccessBody.
	enchantResult := buildCurrentExpertJobStoreEnchantSuccessBody(currentExpertJobStoreUseResult{Success: true, Experience: 42, Endurance: 297})
	wantEnchantResult := []byte{1, 1, 42, 0, 0, 0, 0, 0x29, 1, 0, 0}
	if !bytes.Equal(enchantResult, wantEnchantResult) {
		t.Fatalf("enchant result=%x want=%x", enchantResult, wantEnchantResult)
	}
}

func TestCurrentExpertJobGiveUpAndStoreNotificationContracts(t *testing.T) {
	if currentExpertJobStoreCreateNotification != 538 || currentExpertJobStoreCloseNotification != 539 ||
		currentExpertJobStoreUpdateNotification != 544 || currentExpertJobEnchantOwnerNotification != 533 {
		t.Fatalf("store notifications create=%d close=%d update=%d owner=%d",
			currentExpertJobStoreCreateNotification, currentExpertJobStoreCloseNotification,
			currentExpertJobStoreUpdateNotification, currentExpertJobEnchantOwnerNotification)
	}
	body := buildCurrentExpertJobGiveUpSuccessBody(currentExpertJobGiveUpResult{FinalGold: 59000, State: 1, Cost: 1000})
	want := []byte{1, 0x78, 0xe6, 0, 0, 1, 0}
	if !bytes.Equal(body, want) {
		t.Fatalf("give-up body=%x want=%x", body, want)
	}
	module := expertJobGameplayModule()
	opcode := uint16(239)
	if module.LegacyHandlers[opcode] == nil || module.UpperHandlers[opcode] == nil || module.LegacyNormalizers[opcode] == nil {
		t.Fatalf("op239 is not registered in every gameplay route")
	}

	connection := &bufferConn{}
	service := &Service{options: options{gameUpperBodyCodec: gameUpperBodyCodecPlain}}
	session := &gameSession{conn: connection, selectedCharacterID: 19}
	if err := service.sendCurrentExpertJobClearedInfo(session, 1); err != nil {
		t.Fatal(err)
	}
	packet, rest := splitGameServerUpperPacket(t, connection.write.Bytes())
	// The cleared form is two native bytes plus the mode block and the experience
	// pair, and it is always silent: it restates an absent profession rather than
	// announcing an acquisition. Byte 0 carries the give-up tier, which drives the
	// escalating gold cost of giving the profession up again.
	if len(rest) != 0 || packet.Header.Classification != 0 || packet.Header.MsgID != currentExpertJobInfoNotification ||
		!bytes.Equal(packet.Body, []byte{
			1, 0,
			0x45, 0x4a, 0x58, 0x4d, 1, 0, 0, 0,
			0x45, 0x4a, 0x58, 0x50, 0, 0, 0, 0,
		}) {
		t.Fatalf("cleared expert-job notification header=%+v body=%x trailing=%x", packet.Header, packet.Body, rest)
	}
}

func TestCurrentExpertJobStorePairMutationCommitsAndRollsBackBothCharacters(t *testing.T) {
	repositories := dnfrepomemory.NewMemoryGroup()
	ctx := context.Background()
	for _, id := range []string{"19", "20"} {
		if err := repositories.Character.Save(ctx, dnfrepo.CharacterRecord{CharacterID: id, AccountID: "account-" + id, Stats: map[string]int64{"expert_job_exp": 0, "gold": 1000}}); err != nil {
			t.Fatal(err)
		}
		if err := repositories.Inventory.Save(ctx, dnfrepo.InventoryRecord{CharacterID: id, Slots: map[string]dnfrepo.ItemStack{"0:10": {ItemID: 500, Count: 1}}}); err != nil {
			t.Fatal(err)
		}
	}
	if err := repositories.AccountInventory.Save(ctx, dnfrepo.AccountInventoryRecord{
		AccountID: "account-19",
		Slots:     map[string]dnfrepo.ItemStack{"0:358": {ItemID: 3037, Count: 10}},
	}); err != nil {
		t.Fatal(err)
	}
	service := &Service{options: options{accountID: "account-19"}, repositoryProvider: func() (dnfrepo.Group, bool) { return repositories, true }}
	session := &gameSession{selectedCharacterID: 19}
	store := &currentExpertJobStore{OwnerCharacterID: 20}
	if err := service.mutateCurrentExpertJobStorePair(ctx, session, store, func(requesterCharacter *dnfrepo.CharacterRecord, requesterInventory *dnfrepo.InventoryRecord, requesterAccount *dnfrepo.AccountInventoryRecord, ownerCharacter *dnfrepo.CharacterRecord, ownerInventory *dnfrepo.InventoryRecord) error {
		requesterCharacter.Stats["expert_job_exp"] = 1
		ownerCharacter.Stats["expert_job_exp"] = 2
		currentExpertJobSetWalletGold(requesterCharacter, 900)
		currentExpertJobSetWalletGold(ownerCharacter, 1100)
		requesterAccount.Slots["0:358"] = dnfrepo.ItemStack{ItemID: 3037, Count: 15}
		return nil
	}); err != nil {
		t.Fatal(err)
	}
	requester, _, _ := repositories.Character.Load(ctx, "19")
	owner, _, _ := repositories.Character.Load(ctx, "20")
	if requester.Stats["gold"] != 900 || owner.Stats["gold"] != 1100 {
		t.Fatalf("committed wallets requester=%d owner=%d", requester.Stats["gold"], owner.Stats["gold"])
	}
	requesterAccount, found, err := repositories.AccountInventory.Load(ctx, "account-19")
	if err != nil || !found || requesterAccount.Slots["0:358"].Count != 15 {
		t.Fatalf("committed requester account inventory found=%t err=%v slots=%+v", found, err, requesterAccount.Slots)
	}
	wantErr := errors.New("reject pair")
	if err := service.mutateCurrentExpertJobStorePair(ctx, session, store, func(requesterCharacter *dnfrepo.CharacterRecord, _ *dnfrepo.InventoryRecord, requesterAccount *dnfrepo.AccountInventoryRecord, ownerCharacter *dnfrepo.CharacterRecord, _ *dnfrepo.InventoryRecord) error {
		currentExpertJobSetWalletGold(requesterCharacter, 0)
		currentExpertJobSetWalletGold(ownerCharacter, 0)
		requesterAccount.Slots["0:358"] = dnfrepo.ItemStack{ItemID: 3037, Count: 0}
		return wantErr
	}); !errors.Is(err, wantErr) {
		t.Fatalf("rollback error=%v", err)
	}
	requester, _, _ = repositories.Character.Load(ctx, "19")
	owner, _, _ = repositories.Character.Load(ctx, "20")
	if requester.Stats["gold"] != 900 || owner.Stats["gold"] != 1100 {
		t.Fatalf("rolled-back wallets requester=%d owner=%d", requester.Stats["gold"], owner.Stats["gold"])
	}
	requesterAccount, found, err = repositories.AccountInventory.Load(ctx, "account-19")
	if err != nil || !found || requesterAccount.Slots["0:358"].Count != 15 {
		t.Fatalf("rolled-back requester account inventory found=%t err=%v slots=%+v", found, err, requesterAccount.Slots)
	}
}

func TestConsumeCurrentExpertJobRecipeUsesCharacterGoldWithoutPersistedWalletRow(t *testing.T) {
	character := &dnfrepo.CharacterRecord{Stats: map[string]int64{"gold": 1000}}
	inventory := &dnfrepo.InventoryRecord{Slots: map[string]dnfrepo.ItemStack{
		"0:10": {ItemID: 500, Count: 3},
	}}
	account := &dnfrepo.AccountInventoryRecord{Slots: map[string]dnfrepo.ItemStack{}}
	accountChanged, err := consumeCurrentExpertJobRecipe(character, inventory, account, []dnfexpertjob.RecipeEntry{{ItemID: 500, Count: 2}}, 250)
	if err != nil {
		t.Fatal(err)
	}
	if accountChanged {
		t.Fatal("ordinary backpack material unexpectedly changed account inventory")
	}
	if character.Stats["gold"] != 750 || inventory.Slots["0:10"].Count != 1 {
		t.Fatalf("committed gold=%d material=%d", character.Stats["gold"], inventory.Slots["0:10"].Count)
	}
	if _, exists := inventory.Slots["0:0"]; exists {
		t.Fatal("expert-job recipe persisted a synthetic wallet row")
	}
}

func TestConsumeCurrentExpertJobRecipeConsumesSharedCubeAndBackpackEssenceTogether(t *testing.T) {
	character := &dnfrepo.CharacterRecord{Stats: map[string]int64{"gold": 1000}}
	inventory := &dnfrepo.InventoryRecord{Slots: map[string]dnfrepo.ItemStack{
		"0:10": {ItemID: 2610041, Count: 5},
	}}
	account := &dnfrepo.AccountInventoryRecord{Slots: map[string]dnfrepo.ItemStack{
		dnfrepo.AccountSharedInventorySlotKey(359): {ItemID: 3262, Count: 20},
	}}
	changed, err := consumeCurrentExpertJobRecipe(character, inventory, account, []dnfexpertjob.RecipeEntry{
		{ItemID: 3262, Count: 10},
		{ItemID: 2610041, Count: 3},
	}, 0)
	if err != nil {
		t.Fatal(err)
	}
	if !changed || inventory.Slots["0:10"].Count != 2 || account.Slots[dnfrepo.AccountSharedInventorySlotKey(359)].Count != 10 {
		t.Fatalf("shared/backpack consumption changed=%t inventory=%+v account=%+v", changed, inventory.Slots, account.Slots)
	}
}

func TestCurrentExpertJobSuccessBodiesMatchCurrentEXEReaders(t *testing.T) {
	compound := buildCurrentExpertJobCompoundSuccessBody(dnfexpertjob.CompoundPlan{AttemptedOutputs: []dnfexpertjob.RecipeEntry{{ItemID: 1110, Count: 3}}, SuccessCount: 2, FailureCount: 1})
	wantCompound := []byte{1, 1, 0x56, 0x04, 0, 0, 3, 0, 0, 0, 2, 0, 0, 0, 1, 0, 0, 0, 0}
	if !bytes.Equal(compound, wantCompound) {
		t.Fatalf("compound body=%x want=%x", compound, wantCompound)
	}
	extraction := buildCurrentExpertJobExtractionSuccessBody(currentExpertJobExtractionResult{TargetList: 0, TargetSlot: 10, Materials: []currentExpertJobExtractionMaterial{{Slot: 11, ItemID: 2610024, Count: 5}}})
	wantExtraction := []byte{1, 0, 10, 0, 1, 11, 0, 0x68, 0xd3, 0x27, 0, 5, 0, 0, 0}
	if !bytes.Equal(extraction, wantExtraction) {
		t.Fatalf("extraction body=%x want=%x", extraction, wantExtraction)
	}
}

func TestCurrentExpertJobLearnedRecipeStatsAreTypedAndSorted(t *testing.T) {
	stats := map[string]int64{"expert_job_recipe_2_2600150": 1, "expert_job_recipe_2_2600149": 1, "expert_job_recipe_4_2600083": 1, "expert_job_recipe_2_bad": 1, "expert_job_recipe_2_1": 0}
	if got := currentExpertJobLearnedRecipes(stats, 2); !bytes.Equal(int64SliceBytes(got), int64SliceBytes([]int64{2600149, 2600150})) {
		t.Fatalf("recipes=%v", got)
	}
}

func int64SliceBytes(values []int64) []byte {
	result := make([]byte, len(values)*8)
	for index, value := range values {
		binary.LittleEndian.PutUint64(result[index*8:], uint64(value))
	}
	return result
}
