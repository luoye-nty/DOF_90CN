// Package weaknesspotion owns the 防虚弱药剂 item family and the durable
// weakness-immunity window those items grant.
//
// Why the family is pinned as data instead of being derived from the PVF: the
// client applies the potion's buff locally and never reports it. A live capture
// (2026-09-15 00:43) shows the only uplink around a use is the ordinary
// class-1/op44 request - op593/op123/op42 carry no buff state - so the server has
// nothing to read. The PVF offers no structural tag either: [cooltime group] is
// the HP-potion group (雷米之吻), and the 71 variants (10/15/30/60-minute pots,
// 安魂香, 绿色/红色, 网吧/圣诞/兽人王国 versions ...) share no field except the
// [explain] text. The list below was enumerated from the live Script.pvf with
// zz_scan_daily_usable_count_test.go, so it is data, not guesswork.
//
// The window is durable on purpose: several variants state the effect survives a
// death, a town return and a relog ("该效果不会因角色在地下城中死亡、 返回城镇或
// 重新登录游戏而消失"), so it lives in the character's overflow stat map
// (dnf_character_stats) and needs no schema change.
package weaknesspotion

import "time"

const (
	// ImmuneUntilStatKey is the unix second the current immunity window ends.
	ImmuneUntilStatKey = "weakness_immune_until"
	// WeaknessLevelStatKey is dnfbridge's penalty field: "stamina" carries the
	// weakness level (currentWeaknessStatKey). Using a 防虚弱药剂 clears the
	// penalty immediately as well as preventing the next one.
	WeaknessLevelStatKey = "stamina"
	// WeaknessRecoveryAtStatKey is dnfbridge's countdown anchor; it is cleared
	// with the penalty so no half-finished recovery is left behind.
	WeaknessRecoveryAtStatKey = "weakness_recovery_at"
)

// immunitySeconds maps every usable 防虚弱 item to its immunity duration. The
// duration is the item's own [explain] "效果持续N分钟"; the one variant whose
// text omits it uses its [cool time] (1800000 ms).
var immunitySeconds = map[int64]int64{
	10005376: 1800, // 防虚弱药剂 (30 分钟)
	10005909: 1800, // 防虚弱药剂 (30 分钟)
	10006075: 1800, // 防虚弱药剂 (30 分钟)
	10006487: 1800, // 防虚弱药剂 (30 分钟)
	10006572: 900, // 防虚弱药剂 (15 分钟)
	10087825: 1800, // 防虚弱药剂 (30 分钟)
	490000028: 900, // 金蛋之王的祝福 (15 分钟)
	490000113: 1800, // 防虚弱药剂 (30 分钟)
	490000348: 1800, // 防虚弱药剂 (30 分钟)
	490000702: 900, // 防虚弱药剂 (15 分钟)
	490001415: 1800, // 防虚弱药剂 (30 分钟)
	490002087: 1800, // 防虚弱药剂 (30 分钟)
	490002486: 1800, // 解除虚弱状态药剂 (30 分钟)
	490002492: 1800, // 防虚弱药剂 (30 分钟)
	490003164: 1800, // 防虚弱药剂 (30 分钟)
	490003223: 1800, // 防虚弱药剂 (30 分钟)
	490003267: 1800, // 防虚弱药剂 (30 分钟)
	490003594: 1800, // 防虚弱药剂 (30 分钟)
	490003621: 600, // 防虚弱药剂 (10分钟) (10 分钟)
	490003693: 1800, // 防虚弱药剂 (30 分钟)
	490003699: 600, // 防虚弱药剂 (10 分钟)
	490003991: 1800, // 防虚弱药剂 (30 分钟)
	490004184: 1800, // 防虚弱药剂 (30 分钟)
	490004191: 600, // 防虚弱药剂 (10 分钟)
	490004314: 1800, // 防虚弱药剂 (30 分钟)
	490004398: 1800, // 防虚弱药剂 (30 分钟)
	490004403: 600, // 安魂香 (10 分钟)
	490004641: 1800, // 防虚弱药剂 (30 分钟)
	490005155: 1800, // 防虚弱药剂 (30 分钟)
	490005202: 600, // 防虚弱药剂 (10 分钟)
	490005299: 1800, // 防虚弱药剂 (30 分钟)
	490005338: 600, // 防虚弱药剂 (10 分钟)
	490005630: 600, // 防虚弱药剂 (10 分钟)
	490005699: 1800, // 防虚弱药剂 (30 分钟)
	490005756: 600, // 防虚弱药剂 (10 分钟)
	490005942: 1800, // 防虚弱药剂 (30 分钟)
	490006116: 1800, // 防虚弱药剂 (30 分钟)
	490006128: 1800, // 防虚弱药剂 (30 分钟)
	490006356: 600, // 圣诞老人的防虚弱药剂 (10 分钟)
	490006403: 1800, // 防虚弱药剂 (30 分钟)
	490006573: 1800, // 防虚弱药剂 (30 分钟)
	490006758: 600, // 兽人王国的防虚弱药剂 (10 分钟)
	490006790: 1800, // 防虚弱药剂 (30 分钟)
	490006925: 600, // 防虚弱药剂 (10 分钟)
	490006966: 1800, // 安徒恩的祝福 (30 分钟)
	490007035: 600, // 防虚弱药剂 (10 分钟)
	490007360: 600, // 防虚弱药剂 (10 分钟)
	490007515: 600, // 绿色防虚弱药剂 (10分钟) (10 分钟)
	490007894: 600, // 防虚弱药剂 (10 分钟)
	490007908: 3600, // 史诗网吧防虚弱药剂 (60 分钟)
	490007910: 3600, // 传说网吧防虚弱药剂 (60 分钟)
	490007912: 1800, // 勇者网吧防虚弱药剂 (30 分钟)
	490007926: 1800, // 防虚弱药剂 (30 分钟)
	490008119: 600, // 防虚弱药剂 (10 分钟)
	490008213: 900, // 防虚弱药剂(15分钟） (15 分钟)
	490008358: 1800, // 防虚弱药剂 (30 分钟)
	490008441: 600, // 防虚弱药剂 (10 分钟)
	490008541: 600, // 防虚弱药剂 (枪剑士) (10 分钟)
	490700369: 1800, // 防虚弱药剂 (30 分钟)
	490700706: 1800, // 防虚弱药剂 (30 分钟)
	490701082: 1800, // 防虚弱药剂 (30 分钟)
	490701255: 1800, // 防虚弱药剂 (30 分钟)
	490701313: 1800, // 防虚弱药剂 (30 分钟)
	490701627: 600, // 红色防虚弱药剂 (10 分钟)
	490701641: 1800, // 防虚弱药剂 (30 分钟)
	490701705: 600, // 防虚弱药剂 (10 分钟)
	490701808: 1800, // 防虚弱药剂 (30 分钟)
	490702160: 1800, // 防虚弱药剂 (30 分钟)
	490702200: 600, // 防虚弱药剂 (10 分钟)
	490702311: 1800, // 防虚弱药剂 (30 分钟)
}

// heldItemIDs are the passive variants: merely carrying one in the material
// inventory already prevents the penalty, no use required.
var heldItemIDs = map[int64]struct{}{
	490008656: {}, // 十周年防虚弱青石
}

// ImmunitySeconds reports the immunity window one item use grants.
func ImmunitySeconds(itemID int64) (int64, bool) {
	seconds, ok := immunitySeconds[itemID]
	if !ok || seconds <= 0 {
		return 0, false
	}
	return seconds, true
}

// HeldExempts reports whether merely holding this item id already prevents the
// forced-return penalty.
func HeldExempts(itemID int64) bool {
	_, ok := heldItemIDs[itemID]
	return ok
}

// ImmuneUntil reads the stored window end (unix seconds); zero means none.
func ImmuneUntil(stats map[string]int64) int64 {
	if stats == nil {
		return 0
	}
	until := stats[ImmuneUntilStatKey]
	if until < 0 {
		return 0
	}
	return until
}

// Active reports whether the immunity window still covers now.
func Active(stats map[string]int64, now time.Time) bool {
	until := ImmuneUntil(stats)
	return until > 0 && until > now.Unix()
}
