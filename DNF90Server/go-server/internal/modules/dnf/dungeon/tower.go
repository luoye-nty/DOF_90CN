package dungeon

import (
	"crypto/sha256"
	"encoding/binary"
	"errors"
	"fmt"
	"math"
	"strconv"
	"strings"
	"time"

	"longheng.io/server/internal/modules/dnf/premium"
	dnfpvf "longheng.io/server/internal/modules/dnf/pvf"
	dnfrepo "longheng.io/server/internal/modules/dnf/repository"
)

// Tower of Despair and Tower of Grief share one floor/progress model. The two
// kinds differ only in the PVF document and in how the clear reward is drawn:
// despair grants pickups, grief draws ten independent PVF card slots.
type TowerKind string

const (
	TowerDespair             TowerKind = "despair"
	TowerGrief               TowerKind = "grief"
	TowerRewardSlots                   = 10
	towerDailyUsageDayKey              = "tower_daily_usage_day"
	towerDailyUsageKeyPrefix           = "tower_daily_used_"
)

var ErrTowerDailyLimitReached = errors.New("tower daily entry limit reached")

type TowerReward struct{ ItemID, Count uint32 }

type TowerClear struct {
	Kind    TowerKind
	Floor   uint16
	Rewards [TowerRewardSlots]TowerReward
	Grant   func(*dnfrepo.InventoryRecord, uint32, uint32) error
}

func towerStat(kind TowerKind, suffix string) string { return "tower_" + string(kind) + "_" + suffix }

// Each tower has one character entry in addition to the shared account pool.
// Legacy clears only consumed the account pool, so an absent marker preserves
// the character entry instead of charging the same old clear to both pools.
func TowerCharacterDailyRemaining(character dnfrepo.CharacterRecord, kind TowerKind, now time.Time) byte {
	if kind != TowerDespair && kind != TowerGrief {
		return 0
	}
	if day, found := character.Stats[towerStat(kind, "character_entry_day")]; found && day >= premium.ServiceDayID(now) {
		return 0
	}
	return 1
}

func recordTowerEntryClear(account *dnfrepo.AccountRecord, character *dnfrepo.CharacterRecord, kind TowerKind, now time.Time) error {
	if TowerCharacterDailyRemaining(*character, kind, now) == 0 {
		return RecordTowerDailyClear(account, kind, now)
	}
	if character.Stats == nil {
		character.Stats = make(map[string]int64)
	}
	character.Stats[towerStat(kind, "character_entry_day")] = premium.ServiceDayID(now)
	return nil
}

func TowerFloor(character dnfrepo.CharacterRecord, kind TowerKind) (uint16, error) {
	if kind != TowerDespair && kind != TowerGrief {
		return 0, ErrSettlementStateInvalid
	}
	floor := character.Stats[towerStat(kind, "floor")]
	if floor == 0 {
		return 1, nil
	}
	if floor < 1 || floor > 100 {
		return 0, ErrSettlementStateInvalid
	}
	return uint16(floor), nil
}

// TowerDailyEnterableLimit reads the account-wide daily cap from the active
// tower PVF document. The client uses this value for the account tooltip.
func TowerDailyEnterableLimit(source dnfpvf.Source, kind TowerKind, adventureLevel int) (uint32, error) {
	if kind != TowerDespair && kind != TowerGrief {
		return 0, ErrSettlementStateInvalid
	}
	doc, err := towerDocument(source, "etc/towerof"+string(kind)+".etc")
	if err != nil {
		return 0, err
	}
	value, ok := doc.Int("account enterable max count")
	if !ok || value <= 0 || value > math.MaxUint32 {
		return 0, ErrSettlementStateInvalid
	}
	rows := doc.Ints("adventure group add enterable count")
	if len(rows)%3 != 0 {
		return 0, ErrSettlementStateInvalid
	}
	var previousEnd int64
	for i := 0; i < len(rows); i += 3 {
		low, high, extra := rows[i], rows[i+1], rows[i+2]
		if low <= previousEnd || high < low || extra < 0 || extra > math.MaxUint32-value {
			return 0, ErrSettlementStateInvalid
		}
		previousEnd = high
		if int64(adventureLevel) >= low && int64(adventureLevel) <= high {
			value += extra
		}
	}
	return uint32(value), nil
}

func towerDailyUsageKey(kind TowerKind) string {
	return towerDailyUsageKeyPrefix + string(kind)
}

// TowerDailyUsed returns the number of successful clears in the current DNF
// service day. Stale metadata is treated as zero without mutating storage.
func TowerDailyUsed(account dnfrepo.AccountRecord, kind TowerKind, now time.Time) (uint32, error) {
	if kind != TowerDespair && kind != TowerGrief {
		return 0, ErrSettlementStateInvalid
	}
	if account.Metadata == nil || account.Metadata[towerDailyUsageDayKey] != strconv.FormatInt(premium.ServiceDayID(now), 10) {
		return 0, nil
	}
	value := strings.TrimSpace(account.Metadata[towerDailyUsageKey(kind)])
	if value == "" {
		return 0, nil
	}
	used, err := strconv.ParseUint(value, 10, 32)
	if err != nil {
		return 0, fmt.Errorf("%w: invalid tower daily usage", ErrSettlementStateInvalid)
	}
	return uint32(used), nil
}

// RecordTowerDailyClear updates account-wide usage inside the settlement
// transaction. Admission owns the limit check: an accepted, completed run must
// still receive its rewards. The settlement receipt makes retries apply once.
func RecordTowerDailyClear(account *dnfrepo.AccountRecord, kind TowerKind, now time.Time) error {
	if account == nil || (kind != TowerDespair && kind != TowerGrief) {
		return ErrSettlementStateInvalid
	}
	used, err := TowerDailyUsed(*account, kind, now)
	if err != nil {
		return err
	}
	if used == math.MaxUint32 {
		return ErrSettlementStateInvalid
	}
	if account.Metadata == nil {
		account.Metadata = make(map[string]string)
	}
	day := strconv.FormatInt(premium.ServiceDayID(now), 10)
	if account.Metadata[towerDailyUsageDayKey] != day {
		delete(account.Metadata, towerDailyUsageKey(TowerDespair))
		delete(account.Metadata, towerDailyUsageKey(TowerGrief))
	}
	account.Metadata[towerDailyUsageDayKey] = day
	account.Metadata[towerDailyUsageKey(kind)] = strconv.FormatUint(uint64(used+1), 10)
	return nil
}

// The selector consumes the next challenge floor, independently of the floor
// shown in the clear window. The settlement receipt owns duplicate delivery.
func applyTowerClear(character *dnfrepo.CharacterRecord, inventory *dnfrepo.InventoryRecord, clear *TowerClear, now time.Time) error {
	if clear == nil {
		return nil
	}
	if _, err := TowerFloor(*character, clear.Kind); err != nil {
		return err
	}
	if clear.Floor == 0 || clear.Floor > 100 {
		return ErrSettlementStateInvalid
	}
	for slot, reward := range clear.Rewards {
		if (reward.ItemID == 0) != (reward.Count == 0) || reward.ItemID == math.MaxUint32 {
			return ErrSettlementStateInvalid
		}
		if reward.ItemID != 0 {
			if clear.Grant == nil {
				return ErrSettlementStateInvalid
			}
			if err := clear.Grant(inventory, reward.ItemID, reward.Count); err != nil {
				return err
			}
		}
		character.Stats[towerStat(clear.Kind, fmt.Sprintf("reward_%d_item", slot))] = int64(reward.ItemID)
		character.Stats[towerStat(clear.Kind, fmt.Sprintf("reward_%d_count", slot))] = int64(reward.Count)
	}
	character.Stats[towerStat(clear.Kind, "floor")] = int64(clear.Floor%100 + 1)
	character.Stats[towerStat(clear.Kind, "last_clear_floor")] = int64(clear.Floor)
	character.Stats[towerStat(clear.Kind, "last_clear_at")] = now.Unix()
	return nil
}

func TowerLastRewards(character dnfrepo.CharacterRecord, kind TowerKind) ([TowerRewardSlots]TowerReward, error) {
	var out [TowerRewardSlots]TowerReward
	for slot := range out {
		id := character.Stats[towerStat(kind, fmt.Sprintf("reward_%d_item", slot))]
		count := character.Stats[towerStat(kind, fmt.Sprintf("reward_%d_count", slot))]
		if id < 0 || id >= math.MaxUint32 || count < 0 || count > math.MaxUint32 || (id == 0) != (count == 0) {
			return out, ErrSettlementStateInvalid
		}
		out[slot] = TowerReward{uint32(id), uint32(count)}
	}
	return out, nil
}

func towerDocument(source dnfpvf.Source, path string) (*dnfpvf.Document, error) {
	text, err := source.ReadText(path)
	if err != nil {
		return nil, err
	}
	return dnfpvf.Parse(path, text)
}

func TowerDungeonID(source dnfpvf.Source, kind TowerKind, floor uint16) (int64, error) {
	if kind != TowerDespair && kind != TowerGrief {
		return 0, ErrSettlementStateInvalid
	}
	doc, err := towerDocument(source, "etc/towerof"+string(kind)+".etc")
	if err != nil {
		return 0, err
	}
	rows := doc.Ints("each layer matching dungeon")
	if len(rows) == 0 || len(rows)%3 != 0 {
		return 0, ErrSettlementStateInvalid
	}
	for i := 0; i < len(rows); i += 3 {
		if rows[i] == int64(floor) && rows[i+1] > 0 {
			return rows[i+1], nil
		}
	}
	return 0, fmt.Errorf("tower %s floor %d missing from PVF", kind, floor)
}

// Each card has an independent 10000-point probability range. Unallocated
// probability produces the native empty slot. The server-owned completion key
// freezes the draw across retries; committed values are stored with progress.
func GriefTowerRewards(source dnfpvf.Source, floor uint16, completionKey string) ([TowerRewardSlots]TowerReward, error) {
	var out [TowerRewardSlots]TowerReward
	if floor == 0 || floor > 100 || completionKey == "" {
		return out, ErrSettlementStateInvalid
	}
	doc, err := towerDocument(source, "etc/towerofgrief.etc")
	if err != nil {
		return out, err
	}
	rows, ok := doc.Section("each layer dungeon clear reward")
	if !ok || len(rows)%3 != 0 {
		return out, ErrSettlementStateInvalid
	}
	name := ""
	for i := 0; i < len(rows); i += 3 {
		if rows[i].Kind != dnfpvf.TokenInt || rows[i+1].Kind != dnfpvf.TokenInt || rows[i].Int < 1 || rows[i+1].Int < rows[i].Int || rows[i+1].Int > 100 {
			return out, ErrSettlementStateInvalid
		}
		if rows[i].Int <= int64(floor) && int64(floor) <= rows[i+1].Int {
			if name != "" {
				return out, ErrSettlementStateInvalid
			}
			name = rows[i+2].Value
		}
	}
	if name == "" {
		return out, fmt.Errorf("grief floor %d reward table missing", floor)
	}
	doc, err = towerDocument(source, "etc/itemdropinfo_togclearreward.etc")
	if err != nil {
		return out, err
	}
	var table []dnfpvf.Token
	for _, section := range doc.Sections {
		if section.Name != "table" || section.Start >= section.End {
			continue
		}
		tokens := doc.Tokens[section.Start:section.End]
		if tokens[0].Value == name {
			table = tokens[1:]
			break
		}
	}
	if len(table) == 0 || len(table)%4 != 0 {
		return out, fmt.Errorf("invalid grief table %s", name)
	}
	var rolls, totals [TowerRewardSlots]uint32
	for slot := range rolls {
		hash := sha256.Sum256([]byte(completionKey + "/grief/" + strconv.Itoa(slot)))
		rolls[slot] = binary.LittleEndian.Uint32(hash[:4]) % 10000
	}
	for i := 0; i < len(table); i += 4 {
		label := table[i].Value
		if !strings.HasPrefix(label, "card ") {
			return out, ErrSettlementStateInvalid
		}
		slot, err := strconv.Atoi(strings.TrimPrefix(label, "card "))
		if err != nil || slot < 1 || slot > TowerRewardSlots {
			return out, ErrSettlementStateInvalid
		}
		slot--
		id, probability, count := table[i+1], table[i+2], table[i+3]
		if id.Kind != dnfpvf.TokenInt || probability.Kind != dnfpvf.TokenInt || count.Kind != dnfpvf.TokenInt || id.Int <= 0 || id.Int >= math.MaxUint32 || count.Int <= 0 || count.Int > math.MaxUint32 || probability.Int < 0 || probability.Int > 10000 {
			return out, ErrSettlementStateInvalid
		}
		low := totals[slot]
		totals[slot] += uint32(probability.Int)
		if totals[slot] > 10000 {
			return out, ErrSettlementStateInvalid
		}
		if rolls[slot] >= low && rolls[slot] < totals[slot] {
			out[slot] = TowerReward{uint32(id.Int), uint32(count.Int)}
		}
	}
	return out, nil
}
