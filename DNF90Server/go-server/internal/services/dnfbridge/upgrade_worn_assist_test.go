package dnfbridge

import (
	"context"
	"strings"
	"testing"

	"longheng.io/server/internal/modules/dnf/alignedcmd"
	dnfrepo "longheng.io/server/internal/modules/dnf/repository"
	dnfmemory "longheng.io/server/internal/modules/dnf/repository/memory"
)

// 服主 2026-10-03 截图口径：未佩戴赫尔德 273960 金币，佩戴后 263002。
// 273960 × (100-4) / 100 = 263001.6 ⇒ 客户端显示 263002（服务端同口径取下整）。
func TestCurrentUpgradeAssistFromText(t *testing.T) {
	for _, tc := range []struct {
		name         string
		text         string
		wantDiscount int64
		wantCompound int64
	}{
		{
			name:         "hilda the pet",
			text:         "[name]\n`赫尔德` \n[upgrade cost discount]\n4 \n[increase upgrade compound rate]\n400 \n",
			wantDiscount: 4,
			wantCompound: 400,
		},
		{
			name:         "a belt with only the cost discount",
			text:         "[name]\n`传承腰带` \n[upgrade cost discount]\n10 \n",
			wantDiscount: 10,
		},
		{
			name:         "a title with only the rate",
			text:         "[name]\n`craftsmanship` \n[increase upgrade compound rate]\n200 \n",
			wantCompound: 200,
		},
		{name: "an unrelated item", text: "[name]\n`potion` \n[rarity]\n2 \n"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			discount, compound := currentUpgradeAssistFromText(tc.text)
			if discount != tc.wantDiscount || compound != tc.wantCompound {
				t.Fatalf("discount=%d compound=%d want %d/%d",
					discount, compound, tc.wantDiscount, tc.wantCompound)
			}
		})
	}
}

// 端到端：把 PVF 策略包一层，戴上赫尔德之后
//
//	金币 273960 → 263002（与客户端确认框一致）
//	成功权重 13000 → 13520（+400/10000 = +4%）
//
// 夹具刻意照**线上 PVF 的真实形状**摆：equipment.lst 里记的是相对路径
// `creature/hilda/hilda.equ`，归档里的真名是 `equipment/creature/hilda/hilda.equ`。
// 早先这里两边都手写成带 equipment/ 前缀，于是测试全绿而线上永远取到 0
// （见 upgrade_worn_assist.go 里那段断点注释）——现在夹具复现真实形状，这个用例就是回归锁。
func TestWithCurrentWornUpgradeAssistMatchesTheClientPopup(t *testing.T) {
	ctx := context.Background()
	repositories := dnfmemory.NewMemoryGroup()
	if err := repositories.Equipment.Save(ctx, dnfrepo.EquipmentRecord{
		CharacterID: "11",
		Entries: map[string]dnfrepo.EquipmentEntry{
			// 宠物在穿戴槽 26（inventory.isWornCreatureSlot）。
			"26": {SlotIndex: 26, ItemID: 400990169},
		},
	}); err != nil {
		t.Fatal(err)
	}
	service := &Service{
		pvfItemCatalog: &pvfDungeonDropCatalog{
			source: dungeonDropCatalogTestSource{
				"equipment/creature/hilda/hilda.equ": hildaEquFixtureText,
			},
			itemRefs: map[uint32]dungeonDropItemReference{
				// lst 原文：相对 equipment/ 的写法（线上就是这样）。
				400990169: {kind: dungeonDropItemEquipment, path: "creature/hilda/hilda.equ"},
			},
		},
	}
	session := &gameSession{connID: "worn-assist-test", selectedCharacterID: 11}

	wrapped := service.withCurrentWornUpgradeAssist(
		func(string, int, int64, int) (alignedcmd.UpgradePolicyResolution, error) {
			return alignedcmd.UpgradePolicyResolution{SuccessWeight: 13000, GoldCost: 273960}, nil
		},
		session,
		repositories,
	)
	policy, err := wrapped("reinforce", 0, 0x1234, 0)
	if err != nil {
		t.Fatal(err)
	}
	if policy.GoldCost != 263002 {
		t.Fatalf("gold cost=%d want 263002 (the client popup shows the discounted price)", policy.GoldCost)
	}
	if policy.SuccessWeight != 13520 {
		t.Fatalf("success weight=%d want 13520 (+4%%)", policy.SuccessWeight)
	}

	// 没戴宠物：策略原样不动。
	empty := dnfmemory.NewMemoryGroup()
	plain := service.withCurrentWornUpgradeAssist(
		func(string, int, int64, int) (alignedcmd.UpgradePolicyResolution, error) {
			return alignedcmd.UpgradePolicyResolution{SuccessWeight: 13000, GoldCost: 273960}, nil
		},
		session,
		empty,
	)
	got, err := plain("reinforce", 0, 0x1234, 0)
	if err != nil {
		t.Fatal(err)
	}
	if got.GoldCost != 273960 || got.SuccessWeight != 13000 {
		t.Fatalf("without the pet the policy must be untouched: %+v", got)
	}
}

// 穿戴中的宠物**不在装备记录里**，而在宠物记录的 EquippedKey 上 —— 现场 2026-10-03
// 第一次只扫了装备记录，结果戴了赫尔德还是按原价扣（服主："费用还是不对"）。
func TestWornUpgradeAssistReadsTheEquippedPetRecord(t *testing.T) {
	ctx := context.Background()
	repositories := dnfmemory.NewMemoryGroup()
	if err := repositories.Pet.Save(ctx, dnfrepo.PetRecord{
		CharacterID: "11",
		EquippedKey: "pet-a",
		Entries: map[string]dnfrepo.PetEntry{
			"pet-a": {PetKey: "pet-a", ItemID: 400990169},
		},
	}); err != nil {
		t.Fatal(err)
	}
	service := &Service{
		pvfItemCatalog: &pvfDungeonDropCatalog{
			source: dungeonDropCatalogTestSource{
				"equipment/creature/hilda/hilda.equ": hildaEquFixtureText,
			},
			itemRefs: map[uint32]dungeonDropItemReference{
				400990169: {kind: dungeonDropItemEquipment, path: "creature/hilda/hilda.equ"},
			},
		},
	}
	session := &gameSession{connID: "worn-pet-test", selectedCharacterID: 11}
	wrapped := service.withCurrentWornUpgradeAssist(
		func(string, int, int64, int) (alignedcmd.UpgradePolicyResolution, error) {
			return alignedcmd.UpgradePolicyResolution{SuccessWeight: 13000, GoldCost: 273960}, nil
		},
		session,
		repositories,
	)
	policy, err := wrapped("reinforce", 0, 0x1234, 0)
	if err != nil {
		t.Fatal(err)
	}
	if policy.GoldCost != 263002 || policy.SuccessWeight != 13520 {
		t.Fatalf("equipped pet record was ignored: cost=%d weight=%d want 263002/13520",
			policy.GoldCost, policy.SuccessWeight)
	}
}

// hildaEquFixtureText 是线上 equipment/creature/hilda/hilda.equ 原文的等价夹具
// （字段名、值、行尾多余空格都照抄；服主那只宠物 id 400990076 就是这份）。
const hildaEquFixtureText = "[name]\n`赫尔德` \n[explain]\n`装备后， 强化费用降低4%%且强化成功率增加4%%。` \n" +
	"[upgrade cost discount]\n4 \n[increase upgrade compound rate]\n400 \n"

// 断点证据链：宠物的 lst 路径与归档路径必须**分开**报出来，而且两个字段的原始值要能读到。
// 现场 2026-10-03 第二轮就是靠这一条定位的（聚合值 0/0 时看不出断在哪一环）。
func TestWornUpgradeAssistReportsPerItemPathEvidence(t *testing.T) {
	ctx := context.Background()
	repositories := dnfmemory.NewMemoryGroup()
	if err := repositories.Pet.Save(ctx, dnfrepo.PetRecord{
		CharacterID: "11",
		EquippedKey: "pet-a",
		Entries: map[string]dnfrepo.PetEntry{
			"pet-a": {PetKey: "pet-a", ItemID: 400990076},
		},
	}); err != nil {
		t.Fatal(err)
	}
	service := &Service{
		pvfItemCatalog: &pvfDungeonDropCatalog{
			source: dungeonDropCatalogTestSource{
				"equipment/creature/hilda/hilda.equ": hildaEquFixtureText,
			},
			// itemRefs 故意留空：逼着走"现查 equipment.lst"那条兜底，
			// 顺带把 id → lst 原文路径 这一段也钉住。
			itemRefs: map[uint32]dungeonDropItemReference{},
		},
	}
	catalog := service.pvfItemCatalog
	catalog.upgradeAssistPathIndex = map[uint32]string{
		400990076: "creature/hilda/hilda.equ",
	}

	assist := service.currentWornUpgradeAssist(ctx, repositories, "11")
	if !assist.PetEquipped || assist.PetItemID != 400990076 {
		t.Fatalf("the worn pet was not seen: equipped=%v item=%d", assist.PetEquipped, assist.PetItemID)
	}
	pet := assist.petEvidence()
	if pet.ListedPath != "creature/hilda/hilda.equ" {
		t.Fatalf("listed path=%q want the relative equipment.lst spelling", pet.ListedPath)
	}
	if pet.ArchivePath != "equipment/creature/hilda/hilda.equ" {
		t.Fatalf("archive path=%q want the real archive entry", pet.ArchivePath)
	}
	if pet.TextBytes == 0 {
		t.Fatalf("the PVF text was not read at all: %+v", pet)
	}
	if pet.Discount != 4 || pet.Compound != 400 {
		t.Fatalf("raw field values=%d/%d want 4/400 (err=%q)", pet.Discount, pet.Compound, pet.Err)
	}
	if pet.Err != "" {
		t.Fatalf("unexpected read error: %q", pet.Err)
	}
	if assist.DiscountPercent != 4 || assist.CompoundRate != 400 {
		t.Fatalf("aggregate=%d/%d want 4/400", assist.DiscountPercent, assist.CompoundRate)
	}
	// 日志字段必须是无空格的一行，否则会被当成多个 key=value。
	described := assist.describeItems()
	if described == "" || described == "-" {
		t.Fatalf("describeItems came back empty: %q", described)
	}
	if strings.ContainsAny(described, " \t\n") {
		t.Fatalf("describeItems must not contain whitespace: %q", described)
	}
	if !strings.Contains(described, "400990076|creature/hilda/hilda.equ|equipment/creature/hilda/hilda.equ|") {
		t.Fatalf("describeItems lost the evidence chain: %q", described)
	}
}

// 读不到文本时（归档里没这个路径）证据要写清楚原因，而不是静默 0。
func TestWornUpgradeAssistReportsAMissingDocument(t *testing.T) {
	service := &Service{
		pvfItemCatalog: &pvfDungeonDropCatalog{
			source:   dungeonDropCatalogTestSource{},
			itemRefs: map[uint32]dungeonDropItemReference{},
		},
	}
	service.pvfItemCatalog.upgradeAssistPathIndex = map[uint32]string{
		400990076: "creature/hilda/hilda.equ",
	}
	item := service.currentPVFUpgradeAssist(service.pvfItemCatalog, 400990076)
	if item.Discount != 0 || item.Compound != 0 {
		t.Fatalf("a missing document must not produce values: %+v", item)
	}
	if item.Err == "" {
		t.Fatalf("a missing document must record why: %+v", item)
	}
	if strings.ContainsAny(item.Err, " \t\n") {
		t.Fatalf("the error must stay a single log-field token: %q", item.Err)
	}
	// id 连 lst 里都没有时是另一截断点，要能区分开。
	unlisted := service.currentPVFUpgradeAssist(service.pvfItemCatalog, 123456789)
	if unlisted.Err != "item-id-not-in-equipment-lst" {
		t.Fatalf("unlisted id reported %q", unlisted.Err)
	}
}

// currentUpgradeAssistFromText 的负数/垃圾值一律按 0 处理（绝不放大成负折扣）。
func TestCurrentUpgradeAssistFromTextIgnoresJunk(t *testing.T) {
	discount, compound := currentUpgradeAssistFromText(
		"[upgrade cost discount]\nabc \n[increase upgrade compound rate]\n \n")
	if discount != 0 || compound != 0 {
		t.Fatalf("junk text produced discount=%d compound=%d", discount, compound)
	}
}
