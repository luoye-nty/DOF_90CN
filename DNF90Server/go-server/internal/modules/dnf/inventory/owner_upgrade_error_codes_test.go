package inventory

import "testing"

// Every op80 error code this server sends has to have a branch in the current
// EXE's jump table (sub_1D2F410 -> byte table 0x1D304A0 -> jump table
// 0x1D30440). A code outside that set falls to the default branch, dstr 1662
// "强化装备出现错误， 错误代码 : %d", and the player is shown a bare number --
// which is exactly what "错误代码：174" was.
func TestUpgradeErrorCodesHaveACurrentEXEBranch(t *testing.T) {
	// code -> the dstr text its branch renders, from docs/client-dstr-zh.txt.
	branches := map[byte]string{
		0: "", 4: "缺少物品", 7: "无法强化未修理的装备", 8: "dnf_35067", 9: "dnf_35066",
		10: "金币不足", 13: "该物品无法继续强化", 14: "当前角色Lv不符， 无法对该物品进行强化。",
		17: "只有装备才可以强化", 19: "请确认装备Lv及道具适用Lv。",
		21: "物品栏空间不足， 无法进行强化", 22: "材料不足",
		23: "该道具无法进行强化和赋予异次元气息。", 95: "该物品无法继续强化",
		103: "此机器只能强化+10以上的装备", 104: "所需材料不足", 118: "", 126: "",
		175: "无法对未净化的装备进行装备增幅。", 179: "活动未开放",
		206: "您被禁闭在赛丽亚旅馆， 无法进行该操作。",
		214: "在物品已锁定状态下， 您无法进行当前操作。",
		217: "", 235: "后续确认", 241: "",
	}
	for _, sent := range []struct {
		name string
		code byte
	}{
		{name: "invalid target", code: upgradeErrorInvalidTarget},
		{name: "insufficient gold", code: upgradeErrorInsufficientGold},
		{name: "unsupported optional ticket", code: upgradeErrorUnsupportedOptionalTicket},
		{name: "invalid material", code: upgradeErrorInvalidMaterial},
		{name: "wrong mode", code: upgradeErrorWrongMode},
		{name: "durability", code: upgradeErrorDurability},
		{name: "max level", code: upgradeErrorMaxLevel},
		{name: "amplify not identified", code: upgradeErrorAmplifyNotIdentified},
		{name: "locked", code: upgradeErrorLocked},
	} {
		if _, ok := branches[sent.code]; !ok {
			t.Fatalf("%s sends code %d, which has no branch and prints as a bare number", sent.name, sent.code)
		}
	}
	// The two the tree had one below the real code, both reported live.
	if upgradeErrorAmplifyNotIdentified != 175 {
		t.Fatalf("amplify-not-identified=%d want 175 (无法对未净化的装备进行装备增幅。)", upgradeErrorAmplifyNotIdentified)
	}
	if upgradeErrorLocked != 214 {
		t.Fatalf("locked=%d want 214 (在物品已锁定状态下， 您无法进行当前操作。)", upgradeErrorLocked)
	}
	// 21 is a bag-space message; an unusable protection ticket must not claim
	// the player's inventory is full.
	if upgradeErrorUnsupportedOptionalTicket == 21 {
		t.Fatal("optional-ticket rejection must not use code 21 (物品栏空间不足， 无法进行强化)")
	}
}
