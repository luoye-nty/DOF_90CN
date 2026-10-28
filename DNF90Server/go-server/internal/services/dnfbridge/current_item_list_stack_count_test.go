package dnfbridge

import (
	"encoding/binary"
	"testing"

	dnfrepo "longheng.io/server/internal/modules/dnf/repository"
)

func TestCurrentItemListPersonalCargoStackUsesAuthoritativeDurableCount(t *testing.T) {
	raw := make([]byte, currentItemListEntryWireSize)
	binary.LittleEndian.PutUint32(raw[0x06:0x0A], 20470)
	entry := currentItemListEntryFromStack(2, 0, dnfrepo.ItemStack{
		ItemID:   3330,
		Count:    26320,
		RawEntry: raw,
		Extra: map[string]string{
			"item_kind":       "stackable",
			"amount":          "17970",
			"amount_or_count": "17970",
		},
	})

	if got := binary.LittleEndian.Uint32(entry.data[0x06:0x0A]); got != 26320 {
		t.Fatalf("projected personal-cargo count = %d, want 26320", got)
	}
}

// 玩家反馈：未增幅的装备放进仓库后重新选角进游戏，品级变了；取回背包再小退就恢复。
//
// 装备行的 +0x06 是品级种子，而客户端对装备能待的四张容器表（背包 0、个人仓库 2、
// 穿戴 3、账号仓库 12）用的是同一个 0x77 行读取器。以前只对 0/3 写种子，仓库里的装备
// 落到数量回退取到 Count=1，客户端就按种子 1 重算品级。老行看不出问题是因为它们把种子
// 同时存在 amount_or_count 里，回退路径恰好读得到；itemquality.Apply 造的新装备只写
// quality_seed，所以必然踩中。
//
// 选角重建的表就是 currentSelectInventoryBootstrapListTypes = {0,1,2,7,12,38}，仓库
// 与账号仓库都在里面，所以这条测试钉的正是"重登后品级变了"那条链路。
func TestCurrentItemListEquipmentRowKeepsQualitySeedInEveryEquipmentContainer(t *testing.T) {
	const seed = uint32(123456)
	for _, listType := range []byte{
		dnfrepo.MainInventoryListType,
		currentEquipmentPlacementListPersonalCargo,
		currentSocketListEquipment,
		12,
	} {
		stack := dnfrepo.ItemStack{
			ItemID: 1001,
			Count:  1,
			Extra: map[string]string{
				"item_kind":    "equipment",
				"quality_seed": "123456",
			},
		}
		entry := currentItemListEntryFromStack(listType, 9, stack)
		if got := binary.LittleEndian.Uint32(entry.data[0x06:0x0A]); got != seed {
			t.Fatalf("list %d 装备行 +0x06 = %d, want 品级种子 %d（品级不能取决于装备放在哪个容器）",
				listType, got, seed)
		}
		if got := binary.LittleEndian.Uint32(entry.data[0x0E:0x12]); got != 0 {
			t.Fatalf("list %d 品级种子漏进了独立的 value_a = %d", listType, got)
		}
	}
}

// 反面：四张装备容器之外的行必须一字不改，这样"顺手把 list 1/38 也加进去"会被立刻挡住。
//
//   - 时装 list 1 与公会勋章 list 38 的 +0x06 本来就是 1，证据分别钉在
//     TestGMSystemMailRareAvatarUsesNativeListOneRawRow（RawEntry[6:10]==1）与
//     TestCurrentNPCShopBuyGuildMedalExchangeLandsInGuildMedalList（row[6:10]==1）；
//     即使行里带着一个合法种子，它们也不能改成发种子。
//   - 仓库里的消耗品仍按数量投影。
//   - 宠物 list 7 的 +0x06 是生物序列号，优先级高于品级种子。
func TestCurrentItemListNonEquipmentContainersKeepTheirOwnAmountWord(t *testing.T) {
	for _, listType := range []byte{currentAvatarInventoryListType, currentGuildMedalInventoryListType} {
		entry := currentItemListEntryFromStack(listType, 3, dnfrepo.ItemStack{
			ItemID: 5005,
			Count:  1,
			Extra: map[string]string{
				"item_kind":    "equipment",
				"quality_seed": "123456",
			},
		})
		if got := binary.LittleEndian.Uint32(entry.data[0x06:0x0A]); got != 1 {
			t.Fatalf("list %d 的 +0x06 = %d，按既有证据应保持 1", listType, got)
		}
	}

	entry := currentItemListEntryFromStack(currentEquipmentPlacementListPersonalCargo, 10, dnfrepo.ItemStack{
		ItemID: 3330,
		Count:  7,
		Extra:  map[string]string{"item_kind": "stackable"},
	})
	if got := binary.LittleEndian.Uint32(entry.data[0x06:0x0A]); got != 7 {
		t.Fatalf("仓库消耗品行 +0x06 = %d, want 数量 7", got)
	}

	entry = currentItemListEntryFromStack(currentPetInventoryListType, 1, dnfrepo.ItemStack{
		ItemID: 4001,
		Count:  1,
		Extra: map[string]string{
			"creature_serial_or_handle": "88",
			"quality_seed":              "123456",
		},
	})
	if got := binary.LittleEndian.Uint32(entry.data[0x06:0x0A]); got != 88 {
		t.Fatalf("宠物行 +0x06 = %d, want 生物序列号 88", got)
	}
}

// 老行把种子同时镜像在 amount_or_count 里，修前修后都必须得到同一个值：这条测试保证
// 扩容容器表没有把"老装备"这条路径改坏（改前它靠回退读到种子，改后靠 StackSeed 读）。
func TestCurrentItemListLegacySeedAliasRowProjectsIdenticallyInCargo(t *testing.T) {
	const seed = uint32(87654321)
	stack := dnfrepo.ItemStack{
		ItemID: 1002,
		Count:  1,
		Extra: map[string]string{
			"item_kind":       "equipment",
			"amount_or_count": "87654321",
		},
	}
	entry := currentItemListEntryFromStack(currentEquipmentPlacementListPersonalCargo, 4, stack)
	if got := binary.LittleEndian.Uint32(entry.data[0x06:0x0A]); got != seed {
		t.Fatalf("老行（种子在 amount_or_count）仓库投影 = %d, want %d", got, seed)
	}
}
