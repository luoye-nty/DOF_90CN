package dnfbridge

import (
	"os"
	"strings"
	"testing"

	platformpvf "longheng.io/server/internal/platform/pvf"
)

// TestCurrentGuildStatusBuffsRealPVF 把公会属性用到的两个 VariousBuff 定义从真 PVF 里挖出来。
//
// 背景（2026-10-08 现场）：公会商店卖两个 "公会属性" 权益 —— type 3 → buff 167（限时）、
// type 5 → buff 169（永久，Days=-1）。玩家两个都买了（各 +60 四维，正常应 +120），
// 但进图回城后只掉了 60 点。到底"只生效一个"还是"只失效一个"，取决于客户端这两个 buff
// 的定义（图标/分组/覆盖规则/效果数值）。这个测试把它们打印出来，便于对照。
//
// 需要真 PVF：DNFBRIDGE_REAL_PVF_SMOKE=<Script.pvf 路径>
func TestCurrentGuildStatusBuffsRealPVF(t *testing.T) {
	pvfPath := os.Getenv("DNFBRIDGE_REAL_PVF_SMOKE")
	if pvfPath == "" {
		t.Skip("set DNFBRIDGE_REAL_PVF_SMOKE to dump the guild status buff definitions")
	}
	archive, err := platformpvf.LoadArchive(platformpvf.Options{Path: pvfPath})
	if err != nil {
		t.Fatal(err)
	}
	tables, err := loadCurrentVariousBuffTables(archive)
	if err != nil {
		t.Fatal(err)
	}
	t.Logf("variousbufflist.etc: %d buff 定义, variousbuffstackablelist.etc: %d 道具映射",
		len(tables.ByBuff), len(tables.ByItem))
	for _, index := range []int64{167, 169} {
		effect, found := tables.ByBuff[index]
		if !found {
			t.Logf("buff %d: 不在 variousbufflist.etc 里（可能是别的表定义，例如 character/ 下的 Guild_Status 行）", index)
			continue
		}
		t.Logf("buff %d: group=%q icon=%d apply=%q effect=%q params=%v tooltip=%q",
			index, effect.SystemGroup, effect.IconIndex, effect.ApplyType,
			effect.EffectName, effect.EffectParams, effect.Tooltip)
	}
	// 搜索所有 buff 里与婚房/结婚/情侣相关的定义（"婚房属性"很可能也是一个 VariousBuff）。
	keywords := []string{"wedding", "couple", "marriage", "guild_status"}
	for index, effect := range tables.ByBuff {
		haystack := strings.ToLower(effect.SystemGroup + " " + effect.Tooltip)
		for _, keyword := range keywords {
			if strings.Contains(haystack, keyword) {
				t.Logf("buff %d: group=%q icon=%d apply=%q effect=%q params=%v tooltip=%q",
					index, effect.SystemGroup, effect.IconIndex, effect.ApplyType,
					effect.EffectName, effect.EffectParams, effect.Tooltip)
				break
			}
		}
	}
}
