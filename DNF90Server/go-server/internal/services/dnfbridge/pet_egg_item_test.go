package dnfbridge

import (
	"os"
	"testing"

	platformpvf "longheng.io/server/internal/platform/pvf"
)

// TestCurrentPVFClassifiesPetEgg 用真实 Script.pvf 钉住判据：
//   - 2681466 `egg_SDCharacterCreature_ft.equ` 有 [output index] -> 是蛋
//   - 2681467 / 63088 / 400990167 / 400990168 是（直接发放或已孵化的）真宠物 -> 不是蛋
//
// 文件名不能当判据：线上 359 个带 [output index] 的 creature 文档里，大量叫
// 100980042.equ / 63483.equ；反过来还有 6 个 egg_*.equ 根本没有 output index。
func TestCurrentPVFClassifiesPetEgg(t *testing.T) {
	pvfPath := os.Getenv("DNFBRIDGE_REAL_PVF_SMOKE")
	if pvfPath == "" {
		t.Skip("set DNFBRIDGE_REAL_PVF_SMOKE to run real Script.pvf pet-egg smoke")
	}
	archive, err := platformpvf.LoadArchive(platformpvf.Options{Path: pvfPath})
	if err != nil {
		t.Fatal(err)
	}
	service := &Service{initialEquipmentArchive: archive}

	// 63483 这类编号只存在于 passiveobject.lst，不是道具 ID，判据必须失败关闭，
	// 所以这里只取 equipment.lst 里真实存在的蛋。
	for _, itemID := range []int64{2681466, 100980042} {
		if !service.currentIsPetEggItem(itemID) {
			t.Fatalf("item %d must classify as a pet egg", itemID)
		}
	}
	for _, itemID := range []int64{2681467, 63088, 400990167, 400990168} {
		if service.currentIsPetEggItem(itemID) {
			t.Fatalf("item %d is a real pet and must NOT classify as an egg", itemID)
		}
	}
	// 非装备/未知 id 一律失败关闭。
	for _, itemID := range []int64{0, -1, 27000} {
		if service.currentIsPetEggItem(itemID) {
			t.Fatalf("item %d must not classify as an egg", itemID)
		}
	}
}
