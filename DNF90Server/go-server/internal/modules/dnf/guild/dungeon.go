package guild

import (
	"errors"
	"fmt"
	"strconv"
	"strings"
	"time"

	dnfpvf "longheng.io/server/internal/modules/dnf/pvf"
)

// 公会地下城 (guild dungeon). The whole progression is data, not code: each
// stage owns a purification gauge, clearing a run adds GaugeOnClear to it, and
// a stage counts as conquered once the gauge reaches ConquestGaugeRatio percent
// of GaugeLimit. A stage is enterable only when every entry in RequiredConquest
// is conquered, which is why seeding a client with an empty gauge set leaves
// only 伤城 (8500, no prerequisites) open.
//
// Source: etc/guild/guilddungeon/guilddungeonstyle.lst picks the layout file by
// style id; style 0 is 0_2Way6Dungeon.etc. Live values there:
//
//	8500 伤城         clear +150 (1.5%)  conquest 50%  normal  (no prerequisite)
//	8504 呐喊之地      clear  +75         conquest 50%  normal  needs 8500
//	8507              clear  +50         conquest 50%  hard    needs 8500
//	8505 失心迷宫      clear  +50         conquest 50%  hard    needs 8504
//	8503 哀泣之穴      clear  +75         conquest 50%  normal  needs 8507
//	8508 永恒殿堂      clear  +30         conquest 100% final   needs 8503 and 8505
var (
	ErrDungeonCatalogUnavailable = errors.New("guild dungeon catalog is unavailable")
	ErrDungeonCatalogInvalid     = errors.New("guild dungeon catalog is malformed")
)

const (
	dungeonStyleListPath = "etc/guild/guilddungeon/guilddungeonstyle.lst"
	dungeonStyleDir      = "etc/guild/guilddungeon/"
)

// DungeonReward is one [gauge ratio] block: reaching that percentage of the
// stage's gauge pays the item and the guild contribution once.
type DungeonReward struct {
	GaugeRatio uint32
	ItemID     uint32
	ItemCount  uint32
	Contribute uint32
}

type DungeonStage struct {
	Index              uint32
	GaugeLimit         uint32
	GaugeOnClear       uint32
	ConquestGaugeRatio uint32
	Color              string
	Type               string
	RequiredConquest   []uint32
	Rewards            []DungeonReward
}

// RewardsCrossed returns the reward blocks whose ratio the gauge has reached
// but the previous gauge had not, so a repeat clear pays nothing twice.
func (s DungeonStage) RewardsCrossed(previous, current uint32) []DungeonReward {
	if s.GaugeLimit == 0 || current <= previous {
		return nil
	}
	crossed := make([]DungeonReward, 0, len(s.Rewards))
	for _, reward := range s.Rewards {
		threshold := uint32(uint64(s.GaugeLimit) * uint64(reward.GaugeRatio) / 100)
		if previous < threshold && current >= threshold {
			crossed = append(crossed, reward)
		}
	}
	return crossed
}

// ConquestGauge is the gauge value at which this stage counts as conquered.
func (s DungeonStage) ConquestGauge() uint32 {
	return uint32(uint64(s.GaugeLimit) * uint64(s.ConquestGaugeRatio) / 100)
}

type DungeonCatalog struct {
	FinalDungeon uint32
	Stages       []DungeonStage
	byIndex      map[uint32]DungeonStage
}

func (c DungeonCatalog) Stage(index uint32) (DungeonStage, bool) {
	stage, found := c.byIndex[index]
	return stage, found
}

// Conquered reports whether a stage's stored gauge has reached its threshold.
func (c DungeonCatalog) Conquered(gauges map[uint32]uint32, index uint32) bool {
	stage, found := c.byIndex[index]
	if !found {
		return false
	}
	return gauges[index] >= stage.ConquestGauge()
}

// Unlocked reports whether a stage may be entered: every prerequisite conquered.
// A stage with no prerequisites is always open.
func (c DungeonCatalog) Unlocked(gauges map[uint32]uint32, index uint32) bool {
	stage, found := c.byIndex[index]
	if !found {
		return false
	}
	for _, required := range stage.RequiredConquest {
		if !c.Conquered(gauges, required) {
			return false
		}
	}
	return true
}

// LoadDungeonCatalog reads the style list and parses the layout it selects.
func LoadDungeonCatalog(source dnfpvf.Source) (DungeonCatalog, error) {
	if source == nil {
		return DungeonCatalog{}, ErrDungeonCatalogUnavailable
	}
	listText, err := source.ReadText(dungeonStyleListPath)
	if err != nil {
		return DungeonCatalog{}, fmt.Errorf("guild dungeon style list: %w", err)
	}
	layout, err := firstDungeonStyleFile(listText)
	if err != nil {
		return DungeonCatalog{}, err
	}
	path := dungeonStyleDir + strings.ToLower(layout)
	text, err := source.ReadText(path)
	if err != nil {
		return DungeonCatalog{}, fmt.Errorf("guild dungeon layout %s: %w", path, err)
	}
	return parseDungeonCatalog(text)
}

// firstDungeonStyleFile reads `0 \`0_2Way6Dungeon.etc\“ style rows and returns
// the file for style 0, which is the only style this PVF ships.
func firstDungeonStyleFile(text string) (string, error) {
	for _, field := range strings.Fields(text) {
		if strings.HasPrefix(field, "`") {
			name := strings.Trim(field, "`")
			if name != "" {
				return name, nil
			}
		}
	}
	return "", fmt.Errorf("%w: style list has no layout file", ErrDungeonCatalogInvalid)
}

// parseDungeonCatalog scans the layout text. The file nests `[dungeon]` blocks
// with their own subsections, which the flat Section() accessor cannot express,
// so this walks tokens the way the upgrade tables are read.
func parseDungeonCatalog(text string) (DungeonCatalog, error) {
	catalog := DungeonCatalog{byIndex: make(map[uint32]DungeonStage)}
	fields := strings.Fields(text)
	var (
		stage      *DungeonStage
		reward     *DungeonReward
		pending    string
		collecting bool
	)
	flushReward := func() {
		if stage != nil && reward != nil && reward.GaugeRatio != 0 {
			stage.Rewards = append(stage.Rewards, *reward)
		}
		reward = nil
	}
	flush := func() {
		if stage != nil && stage.Index != 0 {
			catalog.Stages = append(catalog.Stages, *stage)
			catalog.byIndex[stage.Index] = *stage
		}
		stage = nil
	}
	for _, field := range fields {
		if strings.HasPrefix(field, "[") {
			switch field {
			case "[dungeon]":
				flushReward()
				flush()
				stage = &DungeonStage{}
			case "[/dungeon]":
				flushReward()
				flush()
			case "[gauge":
				// "[gauge ratio]" opens a reward block; "[gauge limit]" and
				// "[gauge on clear]" are plain stage fields and are told apart
				// by the completed section name below.
			case "[/gauge":
				flushReward()
			case "[required":
				// "[required dungeon conquest]" splits across fields.
				collecting = true
			case "[/required":
				collecting = false
			}
			if !strings.HasPrefix(field, "[/") {
				pending = strings.Trim(field, "[]")
			} else {
				pending = ""
			}
			continue
		}
		// Section names that span several fields keep appending until a value
		// arrives, so "[gauge on clear]" is seen as "gauge" + "on" + "clear]".
		if strings.HasSuffix(field, "]") {
			pending += " " + strings.TrimSuffix(field, "]")
			if strings.HasPrefix(pending, "/required") || pending == "/required dungeon conquest" {
				collecting = false
			}
			continue
		}
		if pending != "" && !strings.HasSuffix(pending, "]") && !isDungeonValue(field) {
			pending += " " + field
			continue
		}
		value, err := strconv.ParseInt(strings.Trim(field, "`"), 10, 64)
		isNumber := err == nil
		key := strings.TrimSpace(pending)
		switch {
		case key == "final dungeon" && isNumber:
			catalog.FinalDungeon = uint32(value)
		case stage == nil:
			// Outside a [dungeon] block; nothing else is needed.
		case collecting && isNumber:
			stage.RequiredConquest = append(stage.RequiredConquest, uint32(value))
		case key == "gauge ratio" && isNumber:
			// A new [gauge ratio] block; the previous one is complete.
			flushReward()
			reward = &DungeonReward{GaugeRatio: uint32(value)}
		case key == "item" && reward != nil && isNumber:
			if reward.ItemID == 0 {
				reward.ItemID = uint32(value)
			} else if reward.ItemCount == 0 {
				reward.ItemCount = uint32(value)
			}
		case key == "contribute" && reward != nil && isNumber:
			reward.Contribute = uint32(value)
		case key == "dungeon index" && isNumber:
			stage.Index = uint32(value)
		case key == "gauge limit" && isNumber:
			stage.GaugeLimit = uint32(value)
		case key == "gauge on clear" && isNumber:
			stage.GaugeOnClear = uint32(value)
		case key == "conquest gauge ratio" && isNumber:
			stage.ConquestGaugeRatio = uint32(value)
		case key == "color":
			stage.Color = strings.Trim(field, "`")
		case key == "type":
			stage.Type = strings.Trim(field, "`")
		}
	}
	flushReward()
	flush()
	if len(catalog.Stages) == 0 {
		return catalog, fmt.Errorf("%w: no dungeon stages", ErrDungeonCatalogInvalid)
	}
	for _, stage := range catalog.Stages {
		if stage.GaugeLimit == 0 || stage.ConquestGaugeRatio == 0 || stage.ConquestGaugeRatio > 100 {
			return catalog, fmt.Errorf("%w: stage %d limit=%d ratio=%d",
				ErrDungeonCatalogInvalid, stage.Index, stage.GaugeLimit, stage.ConquestGaugeRatio)
		}
	}
	return catalog, nil
}

func isDungeonValue(field string) bool {
	if strings.HasPrefix(field, "`") {
		return true
	}
	_, err := strconv.ParseInt(field, 10, 64)
	return err == nil
}

// ResetDungeonWeek clears every gauge and paid-reward marker when the stored
// week id is behind the current one, and returns whether it reset. The layout's
// [weekly reward] block ties the cycle to the final dungeon, and the boundary
// is the server's existing weekly line (Monday 06:00, premium.ServiceDayID),
// not a new one invented here.
func (r *Registry) ResetDungeonWeek(guildID uint32, weekID int64) (bool, error) {
	guild, found := r.Guilds[guildID]
	if !found || guild == nil {
		return false, ErrGuildNotFound
	}
	if guild.DungeonWeekID == weekID {
		return false, nil
	}
	guild.DungeonWeekID = weekID
	if len(guild.DungeonGauges) == 0 && len(guild.DungeonRewards) == 0 {
		return false, nil
	}
	guild.DungeonGauges = nil
	guild.DungeonRewards = nil
	return true, nil
}

// AddDungeonGauge credits one clear to a guild's stage and returns the gauge
// before and after. Clamped to the stage's own limit so a repeat clear of a
// finished stage cannot overflow it; the pair lets the caller pay only the
// reward thresholds this clear actually crossed.
func (r *Registry) AddDungeonGauge(guildID, index, amount, limit uint32) (uint32, uint32, error) {
	guild, found := r.Guilds[guildID]
	if !found || guild == nil {
		return 0, 0, ErrGuildNotFound
	}
	if guild.DungeonGauges == nil {
		guild.DungeonGauges = make(map[uint32]uint32)
	}
	previous := guild.DungeonGauges[index]
	gauge := previous + amount
	if limit != 0 && gauge > limit {
		gauge = limit
	}
	guild.DungeonGauges[index] = gauge
	return previous, gauge, nil
}

// ClaimDungeonReward records that one stage's gauge-ratio reward has been paid
// this week and reports whether this call is the one that claimed it. Rewards
// are guild-wide: the first member to push the gauge over the line collects.
func (r *Registry) ClaimDungeonReward(guildID, index, gaugeRatio uint32) (bool, error) {
	guild, found := r.Guilds[guildID]
	if !found || guild == nil {
		return false, ErrGuildNotFound
	}
	key := strconv.FormatUint(uint64(index), 10) + ":" + strconv.FormatUint(uint64(gaugeRatio), 10)
	if guild.DungeonRewards[key] {
		return false, nil
	}
	if guild.DungeonRewards == nil {
		guild.DungeonRewards = make(map[string]bool)
	}
	guild.DungeonRewards[key] = true
	return true, nil
}

// AddDungeonContribution pays a [contribute] block into the clearing member's
// mileage and the guild's, mirroring how attendance credits mileage. Upstream
// appends the history row inline; this tree funnels every 公会贡献 write through
// grantMileage, so the row is written there instead. The reason byte stays zero,
// which is what upstream's row carries (Reason unset) and what the op747 reader
// turns into its per-role 签到 fallback.
func (r *Registry) AddDungeonContribution(characterID string, amount uint32, now time.Time) error {
	guild, member, found := r.GuildForCharacter(characterID)
	if !found {
		return ErrNotMember
	}
	if amount == 0 {
		return nil
	}
	r.grantMileage(guild, characterID, member, amount, 0, now)
	return nil
}

// DungeonGauges returns a copy of a guild's stored gauges.
func (r *Registry) DungeonGauges(guildID uint32) map[uint32]uint32 {
	guild, found := r.Guilds[guildID]
	if !found || guild == nil {
		return nil
	}
	gauges := make(map[uint32]uint32, len(guild.DungeonGauges))
	for index, value := range guild.DungeonGauges {
		gauges[index] = value
	}
	return gauges
}
