// Package statbuff 保存"城镇增益药剂"落下的**角色级限时效果**。
//
// 第一个（也是目前唯一一个）消费者是深渊派对的史诗掉落率：
//
//	PVF `[stackable type] [expert town potion]` + 没有 `[action type]` +
//	`[stat change duration] 600000 myself` 的一族药剂（史诗Buff药剂 / 欧皇药水 /
//	史派克的欧气药水 / 小灯笼的大瓶·小瓶史诗Buff药剂 …）在客户端里是**自施放增益**：
//	客户端自己画 buff 图标，而"深渊派对史诗掉落率"是**服务端**才知道的量，
//	所以服务端必须自己记住这个窗口，否则玩家看到的是"药吃了、掉落没变"。
//
// 现场 2026-10-03（服主报，角色 测试x = char 11）：史诗Buff药剂 490002458 连点 15 次
// 全部被 op44 静默拒绝（error 17），连"吃掉了"这一步都没发生；本包负责第二步。
//
// 存储位置：角色记录的 Stats（与 dnfepicwish 的 [wish list]、fatigue 等同一张表）。
// 选它而不是账号元数据的原因很实际：深渊掉落那条路径手上只有 `runtimeDungeonState.Character`
// 这个角色快照，用 Stats 就能直接读到，不必在掉落热路径上多读一次账号。
//
// 覆盖语义：**后来者覆盖**，绝不叠乘 —— 与道具说明的"效果不与其他史诗Buff药剂叠加"
// 一致；同族药剂的 10 分钟共享冷却（`[cooltime group] 251`）本身也挡住了连续使用。
package statbuff

import (
	"errors"
	"strconv"
	"strings"
	"time"

	dnfrepo "longheng.io/server/internal/modules/dnf/repository"
)

const (
	// EpicDropRatePercentStatKey 是掉落率倍率（百分比，200 = 2 倍）。
	EpicDropRatePercentStatKey = "epic_drop_rate_buff_percent"
	// EpicDropRateExpireStatKey 是窗口到期的 Unix 秒。
	EpicDropRateExpireStatKey = "epic_drop_rate_buff_expire_at"
	// EpicDropRateItemStatKey 是施加它的道具 id（日志与排查用）。
	EpicDropRateItemStatKey = "epic_drop_rate_buff_item_id"
	// EpicDropRateBuffIDStatKey 是掷点得到的 buff id（etc/variousbufflist.etc 的
	// [index]；1136=史诗Buff药剂、1090/1091/1092=赛丽亚三档、1239/1240=小灯笼大/小瓶）。
	// 客户端画图标就是按这个 id 去取 [icon index] + [tooltip text]，所以服务端把它
	// 一起记下来：投影和排查都不用再猜。
	EpicDropRateBuffIDStatKey = "epic_drop_rate_buff_id"
)

var ErrEffectInvalid = errors.New("stat buff effect is invalid")

// Effect 是一个正在生效的掉落率窗口。
type Effect struct {
	ItemID            int64
	BuffID            int64
	MultiplierPercent int64
	ExpireAt          time.Time
}

// ActiveEpicDropRate 读角色身上**当前有效**的掉落率增益。
//
// 任何一个字段缺失/非法、或者已经过期，都按"没有增益"处理（返回 false），
// 绝不 fail-open：宁可少给一次加成，也不能让一个过期的记录永久加掉落。
func ActiveEpicDropRate(character dnfrepo.CharacterRecord, now time.Time) (Effect, bool) {
	if character.Stats == nil {
		return Effect{}, false
	}
	percent, percentErr := strconv.ParseInt(strings.TrimSpace(statValue(character, EpicDropRatePercentStatKey)), 10, 64)
	expireUnix, expireErr := strconv.ParseInt(strings.TrimSpace(statValue(character, EpicDropRateExpireStatKey)), 10, 64)
	if percentErr != nil || expireErr != nil || percent <= 0 || expireUnix <= 0 {
		return Effect{}, false
	}
	expireAt := time.Unix(expireUnix, 0).UTC()
	if !expireAt.After(now.UTC()) {
		return Effect{}, false
	}
	itemID, _ := strconv.ParseInt(strings.TrimSpace(statValue(character, EpicDropRateItemStatKey)), 10, 64)
	buffID, _ := strconv.ParseInt(strings.TrimSpace(statValue(character, EpicDropRateBuffIDStatKey)), 10, 64)
	return Effect{ItemID: itemID, BuffID: buffID, MultiplierPercent: percent, ExpireAt: expireAt}, true
}

// ActivateEpicDropRate 写入（或覆盖）掉落率窗口。
//
// buffID 是掷点得到的 buff id（0 = 走 `[explain]` 回退路径，没有 buff id）。
func ActivateEpicDropRate(
	character *dnfrepo.CharacterRecord,
	itemID int64,
	buffID int64,
	multiplierPercent int64,
	duration time.Duration,
	now time.Time,
) (Effect, error) {
	if character == nil || itemID <= 0 || multiplierPercent <= 0 || duration <= 0 {
		return Effect{}, ErrEffectInvalid
	}
	now = now.UTC()
	if character.Stats == nil {
		character.Stats = make(map[string]int64, 4)
	}
	effect := Effect{
		ItemID:            itemID,
		BuffID:            buffID,
		MultiplierPercent: multiplierPercent,
		ExpireAt:          now.Add(duration).UTC(),
	}
	character.Stats[EpicDropRatePercentStatKey] = effect.MultiplierPercent
	character.Stats[EpicDropRateExpireStatKey] = effect.ExpireAt.Unix()
	character.Stats[EpicDropRateItemStatKey] = effect.ItemID
	character.Stats[EpicDropRateBuffIDStatKey] = effect.BuffID
	character.UpdatedAt = now
	return effect, nil
}

func statValue(character dnfrepo.CharacterRecord, key string) string {
	if character.Stats == nil {
		return ""
	}
	return strconv.FormatInt(character.Stats[key], 10)
}

// ---------------------------------------------------------------------------
// 第二类消费者：强化 / 增幅成功率药剂（「强化秘药」「强化的奥秘」「永恒的增幅的秘密」
// 「赛丽亚的幸运强化药剂」…）。
//
// 套路与爆率药剂**完全一样**（服主原话："套路都是一样的"）：
//
//	PVF 侧：etc/variousbufflist.etc 的 `[effect][name] item_usual_upgrade_success_rate`
//	        加上 `[param] N` —— N 就是"成功率增加多少个百分点"：
//	        1097=10（下级）/1098=20（中级）/1099=30（上级），1180..1187=1..8（活动）
//	        道具→buff 的掷点写在 etc/variousbuffstackablelist.etc
//	        （赛丽亚的幸运强化药剂 10008220 → `1097 60 1098 30 1099 10`）
//	服务端侧：把窗口记在角色记录里，在**掷点之前**把成功率按 N 放大。
//
// 强化与增幅各用一套键：它们是两件不同的道具、也该各自生效（不像爆率那样共享一族）。
// 语义与爆率窗口一致：后来者覆盖、绝不叠加；任何字段缺失/非法/过期都按"没有增益"处理。
// ---------------------------------------------------------------------------

// Kind 是限时道具增益的种类。
type Kind string

const (
	// KindUpgradeSuccess 是"强化成功率 +N%"。
	KindUpgradeSuccess Kind = "upgrade_success"
	// KindAmplifySuccess 是"增幅成功率 +N%"。
	KindAmplifySuccess Kind = "amplify_success"
	// KindDungeonClearExp 是"地下城通关经验值 +N%"（PVF 效果名 `dungeon_clear_exp_rate`；
	// 赛丽亚的幸运经验药剂 10008222 的三档是 50/100/200）。这一条**必须服务端算**：
	// 通关经验是服务端发的，客户端算不出来。
	KindDungeonClearExp Kind = "dungeon_clear_exp_rate"
)

// RateWindow 是一个正在生效的"成功率 +N%"窗口。
type RateWindow struct {
	ItemID   int64
	BuffID   int64
	Percent  int64
	ExpireAt time.Time
}

// KindForUpgradeMode 把 alignedcmd 里的 UpgradeKind（"reinforce"/"amplify"，来自 PVF 的
// `[stackable type]`）翻译成窗口种类。
func KindForUpgradeMode(mode string) (Kind, bool) {
	switch strings.ToLower(strings.TrimSpace(mode)) {
	case "reinforce":
		return KindUpgradeSuccess, true
	case "amplify":
		return KindAmplifySuccess, true
	}
	return "", false
}

func rateWindowKeys(kind Kind) (percentKey, expireKey, itemKey, buffKey string, ok bool) {
	switch kind {
	case KindUpgradeSuccess:
		return "upgrade_success_rate_buff_percent",
			"upgrade_success_rate_buff_expire_at",
			"upgrade_success_rate_buff_item_id",
			"upgrade_success_rate_buff_id", true
	case KindAmplifySuccess:
		return "amplify_success_rate_buff_percent",
			"amplify_success_rate_buff_expire_at",
			"amplify_success_rate_buff_item_id",
			"amplify_success_rate_buff_id", true
	case KindDungeonClearExp:
		return "dungeon_clear_exp_rate_buff_percent",
			"dungeon_clear_exp_rate_buff_expire_at",
			"dungeon_clear_exp_rate_buff_item_id",
			"dungeon_clear_exp_rate_buff_id", true
	case KindClientBuff:
		// 客户端自施放的那几族（城镇移速、进图属性…）按 family 各自一套键，
		// 见下面 ActiveClientBuff/ActivateClientBuff。
		return "", "", "", "", false
	}
	return "", "", "", "", false
}

// KindClientBuff 是"客户端自己生效"的一族：服务端只负责把掷到的 buff 注册给客户端
// （图标 + 客户端按 buff 表施加效果），所以键位按 buff 的 effect name（family）分开。
const KindClientBuff Kind = "client_buff"

// ClientBuffWindow 是"客户端自施放 buff"的窗口记录。Percent 只用于日志/排查：
// 真正生效的数值由客户端从 etc/variousbufflist.etc 自己读（它认得这个 buff id）。
type ClientBuffWindow struct {
	Family   string
	ItemID   int64
	BuffID   int64
	Percent  int64
	ExpireAt time.Time
}

func clientBuffKeys(family string) (percentKey, expireKey, itemKey, buffKey string, ok bool) {
	family = strings.ToLower(strings.TrimSpace(family))
	if family == "" {
		return "", "", "", "", false
	}
	// effect name 本身只含 ASCII 字母/下划线，直接做键前缀即可（可读、可直接查库）。
	base := family + "_client_buff"
	return base + "_percent", base + "_expire_at", base + "_item_id", base + "_buff_id", true
}

// ActiveClientBuff 读某个 family 的客户端 buff 窗口（fail-closed 同其它窗口）。
func ActiveClientBuff(character dnfrepo.CharacterRecord, family string, now time.Time) (ClientBuffWindow, bool) {
	percentKey, expireKey, itemKey, buffKey, ok := clientBuffKeys(family)
	if !ok || character.Stats == nil {
		return ClientBuffWindow{}, false
	}
	expireUnix, expireErr := strconv.ParseInt(strings.TrimSpace(statValue(character, expireKey)), 10, 64)
	if expireErr != nil || expireUnix <= 0 {
		return ClientBuffWindow{}, false
	}
	expireAt := time.Unix(expireUnix, 0).UTC()
	if !expireAt.After(now.UTC()) {
		return ClientBuffWindow{}, false
	}
	buffID, _ := strconv.ParseInt(strings.TrimSpace(statValue(character, buffKey)), 10, 64)
	if buffID <= 0 {
		// 没有 buff id 就没有可注册的东西，按"没有窗口"处理。
		return ClientBuffWindow{}, false
	}
	itemID, _ := strconv.ParseInt(strings.TrimSpace(statValue(character, itemKey)), 10, 64)
	percent, _ := strconv.ParseInt(strings.TrimSpace(statValue(character, percentKey)), 10, 64)
	return ClientBuffWindow{
		Family:   strings.ToLower(strings.TrimSpace(family)),
		ItemID:   itemID,
		BuffID:   buffID,
		Percent:  percent,
		ExpireAt: expireAt,
	}, true
}

// ActivateClientBuff 写入（或覆盖）某个 family 的客户端 buff 窗口。
func ActivateClientBuff(
	character *dnfrepo.CharacterRecord,
	family string,
	itemID int64,
	buffID int64,
	percent int64,
	duration time.Duration,
	now time.Time,
) (ClientBuffWindow, error) {
	percentKey, expireKey, itemKey, buffKey, ok := clientBuffKeys(family)
	if !ok || character == nil || itemID <= 0 || buffID <= 0 || duration <= 0 {
		return ClientBuffWindow{}, ErrEffectInvalid
	}
	now = now.UTC()
	if character.Stats == nil {
		character.Stats = make(map[string]int64, 4)
	}
	window := ClientBuffWindow{
		Family:   strings.ToLower(strings.TrimSpace(family)),
		ItemID:   itemID,
		BuffID:   buffID,
		Percent:  percent,
		ExpireAt: now.Add(duration).UTC(),
	}
	character.Stats[percentKey] = window.Percent
	character.Stats[expireKey] = window.ExpireAt.Unix()
	character.Stats[itemKey] = window.ItemID
	character.Stats[buffKey] = window.BuffID
	character.UpdatedAt = now
	return window, nil
}

// RawRateWindowKeys 返回某个族**未经有效性判断**的 (到期 Unix 秒, buff id)。
//
// 与 ActiveRate 的区别：ActiveRate 过期就 fail-closed（返回 false），而这里要的正是
// "已经过期、但记录里还写着 buff id" 的那一条 —— 服务端靠它精确摘掉到点的那几个图标。
func RawRateWindowKeys(character dnfrepo.CharacterRecord, kind Kind) (int64, int64, bool) {
	_, expireKey, _, buffKey, ok := rateWindowKeys(kind)
	if !ok || character.Stats == nil {
		return 0, 0, false
	}
	expireUnix, expireErr := strconv.ParseInt(strings.TrimSpace(statValue(character, expireKey)), 10, 64)
	buffID, _ := strconv.ParseInt(strings.TrimSpace(statValue(character, buffKey)), 10, 64)
	if expireErr != nil || expireUnix <= 0 {
		return 0, 0, false
	}
	return expireUnix, buffID, true
}

// RawClientBuffKeys 同上，给"客户端自施放"那一族用。
func RawClientBuffKeys(character dnfrepo.CharacterRecord, family string) (int64, int64, bool) {
	_, expireKey, _, buffKey, ok := clientBuffKeys(family)
	if !ok || character.Stats == nil {
		return 0, 0, false
	}
	expireUnix, expireErr := strconv.ParseInt(strings.TrimSpace(statValue(character, expireKey)), 10, 64)
	buffID, _ := strconv.ParseInt(strings.TrimSpace(statValue(character, buffKey)), 10, 64)
	if expireErr != nil || expireUnix <= 0 {
		return 0, 0, false
	}
	return expireUnix, buffID, true
}

// ActiveRate 读角色身上**当前有效**的成功率增益（fail-closed，与 ActiveEpicDropRate 同）。
func ActiveRate(character dnfrepo.CharacterRecord, kind Kind, now time.Time) (RateWindow, bool) {
	percentKey, expireKey, itemKey, buffKey, ok := rateWindowKeys(kind)
	if !ok || character.Stats == nil {
		return RateWindow{}, false
	}
	percent, percentErr := strconv.ParseInt(strings.TrimSpace(statValue(character, percentKey)), 10, 64)
	expireUnix, expireErr := strconv.ParseInt(strings.TrimSpace(statValue(character, expireKey)), 10, 64)
	if percentErr != nil || expireErr != nil || percent <= 0 || expireUnix <= 0 {
		return RateWindow{}, false
	}
	expireAt := time.Unix(expireUnix, 0).UTC()
	if !expireAt.After(now.UTC()) {
		return RateWindow{}, false
	}
	itemID, _ := strconv.ParseInt(strings.TrimSpace(statValue(character, itemKey)), 10, 64)
	buffID, _ := strconv.ParseInt(strings.TrimSpace(statValue(character, buffKey)), 10, 64)
	return RateWindow{ItemID: itemID, BuffID: buffID, Percent: percent, ExpireAt: expireAt}, true
}

// ActivateRate 写入（或覆盖）一个成功率窗口。buffID 允许为 0（PVF 无候选表的那几件道具
// 没有 buff id 可掷，但窗口照样要生效）。
func ActivateRate(
	character *dnfrepo.CharacterRecord,
	kind Kind,
	itemID int64,
	buffID int64,
	percent int64,
	duration time.Duration,
	now time.Time,
) (RateWindow, error) {
	percentKey, expireKey, itemKey, buffKey, ok := rateWindowKeys(kind)
	if !ok {
		return RateWindow{}, ErrEffectInvalid
	}
	if character == nil || itemID <= 0 || percent <= 0 || duration <= 0 {
		return RateWindow{}, ErrEffectInvalid
	}
	now = now.UTC()
	if character.Stats == nil {
		character.Stats = make(map[string]int64, 4)
	}
	window := RateWindow{
		ItemID:   itemID,
		BuffID:   buffID,
		Percent:  percent,
		ExpireAt: now.Add(duration).UTC(),
	}
	character.Stats[percentKey] = window.Percent
	character.Stats[expireKey] = window.ExpireAt.Unix()
	character.Stats[itemKey] = window.ItemID
	character.Stats[buffKey] = window.BuffID
	character.UpdatedAt = now
	return window, nil
}
