package alignedcmd

import (
	"testing"

	dnfrepo "longheng.io/server/internal/modules/dnf/repository"
)

func magicBoxJobGroupIDs(groups []MagicBoxRewardGroup) []int64 {
	ids := make([]int64, 0, len(groups))
	for _, group := range groups {
		if len(group.Entries) > 0 {
			ids = append(ids, group.Entries[0].ItemID)
		}
	}
	return ids
}

// [booster info]/[charactor] 是按职业（并带等级区间）分组的。
// 只有开启者自己职业那一组才该发 —— 否则一个鬼剑士开豪华婚礼化妆箱，
// 会把全职业的时装/道具一起领走（2026-10-04 真机现场）。
//
// 职业编号见 jobmap：0=鬼剑士[swordman]，11=女鬼剑士[at swordman]。
func TestMagicBoxGroupsForCharacterKeepsOnlyTheOpeningJob(t *testing.T) {
	groups := []MagicBoxRewardGroup{
		{DrawCount: 1, Job: "[swordman]", MinLevel: 1, MaxLevel: 99, Entries: []MagicBoxRewardEntry{{ItemID: 3001, Weight: 100000, Count: 1}}},
		{DrawCount: 1, Job: "[at swordman]", MinLevel: 1, MaxLevel: 99, Entries: []MagicBoxRewardEntry{{ItemID: 3011, Weight: 100000, Count: 1}}},
		{DrawCount: 2, Entries: []MagicBoxRewardEntry{{ItemID: 3000, Weight: 100000, Count: 2}}},
	}

	// 鬼剑士：拿 swordman 组 + 无职业门的那组，**不能**拿到女鬼剑士的。
	swordman := magicBoxJobGroupIDs(MagicBoxGroupsForCharacter(groups, dnfrepo.CharacterRecord{Job: "0", Level: 90}))
	if len(swordman) != 2 || swordman[0] != 3001 || swordman[1] != 3000 {
		t.Fatalf("鬼剑士拿到 %v，应为 [3001 3000]", swordman)
	}

	// 女鬼剑士：只拿 at swordman 组，不能拿到男鬼剑士的。
	female := magicBoxJobGroupIDs(MagicBoxGroupsForCharacter(groups, dnfrepo.CharacterRecord{Job: "11", Level: 90}))
	if len(female) != 2 || female[0] != 3011 || female[1] != 3000 {
		t.Fatalf("女鬼剑士拿到 %v，应为 [3011 3000]", female)
	}

	// 等级掉出区间：职业组不参与，只剩无职业门的那组。
	low := magicBoxJobGroupIDs(MagicBoxGroupsForCharacter(groups, dnfrepo.CharacterRecord{Job: "0", Level: 0}))
	if len(low) != 1 || low[0] != 3000 {
		t.Fatalf("等级 0 拿到 %v，应只剩 [3000]", low)
	}
}
