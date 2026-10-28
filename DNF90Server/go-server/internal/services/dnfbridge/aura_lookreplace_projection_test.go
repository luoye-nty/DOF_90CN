package dnfbridge

import (
	"encoding/hex"
	"os"
	"strconv"
	"testing"

	dnfrepo "longheng.io/server/internal/modules/dnf/repository"
	platformpvf "longheng.io/server/internal/platform/pvf"
)

// TestCurrentAuraLinkedSocketBlockResolvesLiveInstance 钉住"镶嵌属性靠实时读背包"
// 这条链路：关联块的内容必须来自被覆盖光环**当前所在的背包行**，而不是穿戴时的
// 快照（玩家可能在穿好转职光环之后才打徽章）。绑定失效时必须什么都不发。
func TestCurrentAuraLinkedSocketBlockResolvesLiveInstance(t *testing.T) {
	const (
		coveredAura int64 = 101590068
		otherAura   int64 = 101590051
	)
	sockets := make([]byte, currentAvatarSocketBytes)
	for i := range sockets {
		sockets[i] = byte(0x10 + i)
	}
	hexSockets := hex.EncodeToString(sockets)

	extra := map[string]string{
		dnfrepo.AuraLinkedTemplateExtraKey: strconv.FormatInt(coveredAura, 10),
		dnfrepo.AuraLinkedSourceExtraKey:   "1:31",
	}
	slots := map[string]dnfrepo.ItemStack{
		"1:31": {ItemID: coveredAura, Extra: map[string]string{"avatar_socket_data": hexSockets}},
	}

	if got := currentAuraLinkedSocketBlock(extra, slots); len(got) == 0 {
		t.Fatal("live inventory row must yield the covered aura socket block")
	}

	// 绑定失效的若干情形都必须 fail closed（不发块，也绝不改猜别的光环）。
	cases := []struct {
		name string
		ex   map[string]string
		sl   map[string]dnfrepo.ItemStack
	}{
		{"no inventory", extra, nil},
		{"empty inventory", extra, map[string]dnfrepo.ItemStack{}},
		{"source slot missing", extra, map[string]dnfrepo.ItemStack{"1:9": {ItemID: coveredAura}}},
		{"source slot holds another item", extra, map[string]dnfrepo.ItemStack{"1:31": {ItemID: otherAura}}},
		{"no source recorded", map[string]string{dnfrepo.AuraLinkedTemplateExtraKey: strconv.FormatInt(coveredAura, 10)}, slots},
		{"no template recorded", map[string]string{dnfrepo.AuraLinkedSourceExtraKey: "1:31"}, slots},
		{"clone avatar key only", map[string]string{dnfrepo.CloneAvatarAppearanceExtraKey: "415500058"}, slots},
	}
	for _, c := range cases {
		if got := currentAuraLinkedSocketBlock(c.ex, c.sl); got != nil {
			t.Fatalf("%s: expected no block, got %d bytes", c.name, len(got))
		}
	}

	// 显式捕获的别名优先，保持改动前的行为。
	if got := currentMode1EquipmentLinkedRaw(
		map[string]string{"mode1_linked_raw_hex": "aabb"}, slots); len(got) != 2 {
		t.Fatalf("explicit mode1_linked_raw_hex must win, got %d bytes", len(got))
	}
}

// TestCurrentActorAppearanceItemIDIgnoresAuraLink 是这次修复的直接护栏：
// 转职光环把自己的 item id 作为外观下发，只有被覆盖光环的关联模板写在另一个
// key 上；克隆装扮的借用行为必须完全不变。
func TestCurrentActorAppearanceItemIDIgnoresAuraLink(t *testing.T) {
	const (
		aura        int64 = 113590008 // 转职光环
		coveredAura int64 = 101590068 // 被它覆盖的普通光环
	)
	// 槽 9 的转职光环：带关联模板，但外观必须是它自己。旧代码把关联写进
	// clone_avatar_appearance_id，于是这里会返回 101590068，客户端渲染回原光环。
	equipped := dnfrepo.EquipmentEntry{
		SlotIndex: 9,
		ItemID:    aura,
		Extra: map[string]string{
			"current_exe_equipment_type":       "9",
			dnfrepo.AuraLinkedTemplateExtraKey: "101590068",
		},
	}
	if got := currentActorAppearanceItemID(equipped); got != uint32(aura) {
		t.Fatalf("aura appearance item=%d want=%d (转职光环 must render itself)", got, aura)
	}

	// 对照组：透明克隆时装仍然借用被克隆模板。
	clone := dnfrepo.EquipmentEntry{
		SlotIndex: 3,
		ItemID:    415500017,
		Extra: map[string]string{
			"current_exe_equipment_type":          "3",
			dnfrepo.CloneAvatarAppearanceExtraKey: "415500058",
		},
	}
	if got := currentActorAppearanceItemID(clone); got != 415500058 {
		t.Fatalf("clone appearance item=%d want=415500058", got)
	}

	// 两种 key 同时存在（不应发生）时，关联 key 也不能影响外观。
	both := dnfrepo.EquipmentEntry{
		SlotIndex: 3,
		ItemID:    415500017,
		Extra: map[string]string{
			"current_exe_equipment_type":          "3",
			dnfrepo.AuraLinkedTemplateExtraKey:    "101590068",
			dnfrepo.CloneAvatarAppearanceExtraKey: "415500058",
		},
	}
	if got := currentActorAppearanceItemID(both); got != 415500058 {
		t.Fatalf("appearance with both keys=%d want=415500058", got)
	}
}

// TestCurrentPVFClassifiesAuroraLookReplaceAvatar 用真实 Script.pvf 钉住判据：
// 全库只有 113590006 / 113590008 / 113590009 同时满足
// `[equipment type] [aurora avatar]` 与 `[item category] clear avatar`，
// 并且它们仍然是 clear avatar（分类契约不能被改动，否则克隆路径会漂移）。
func TestCurrentPVFClassifiesAuroraLookReplaceAvatar(t *testing.T) {
	pvfPath := os.Getenv("DNFBRIDGE_REAL_PVF_SMOKE")
	if pvfPath == "" {
		t.Skip("set DNFBRIDGE_REAL_PVF_SMOKE to run real Script.pvf aura smoke")
	}
	archive, err := platformpvf.LoadArchive(platformpvf.Options{Path: pvfPath})
	if err != nil {
		t.Fatal(err)
	}
	service := &Service{initialEquipmentArchive: archive}

	for _, itemID := range []uint32{113590006, 113590008, 113590009} {
		if !service.currentIsAuroraLookReplaceAvatar(itemID) {
			t.Fatalf("item %d must classify as 转职光环", itemID)
		}
		if isAvatar, isClone := service.currentClassifyAvatarItem(itemID); !isAvatar || !isClone {
			t.Fatalf("item %d classification avatar=%t clone=%t (must stay clear avatar)",
				itemID, isAvatar, isClone)
		}
	}

	// 普通光环、克隆帽、普通装扮、普通装备都必须返回 false。
	for _, itemID := range []uint32{101590068, 415500017, 415500058, 27000} {
		if service.currentIsAuroraLookReplaceAvatar(itemID) {
			t.Fatalf("item %d must not classify as 转职光环", itemID)
		}
	}
}
