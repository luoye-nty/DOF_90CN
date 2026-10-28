package dnfbridge

import (
	"errors"
	"fmt"
	"math"
	"strings"

	"longheng.io/server/internal/modules/dnf/jobmap"
	dnfpvf "longheng.io/server/internal/modules/dnf/pvf"
)

const currentAvatarFittingRoomPVFPath = "etc/chn_avatar_fitting_room.etc"

var currentAvatarFittingRoomPartNames = [currentAvatarFittingRoomPartCount]string{
	"hair", "hat", "face", "neck", "coat", "belt", "pants", "shoes",
}

// The PVF file and current EXE enumerate fitting-room parts in the order
// above. Durable equipment uses the ordinary avatar slot numbering below.
var currentAvatarFittingRoomEquipmentSlots = [currentAvatarFittingRoomPartCount]int16{
	1, 0, 2, 6, 3, 7, 4, 5,
}

type currentAvatarFittingRoomSet struct {
	Rare bool
	Jobs map[byte][currentAvatarFittingRoomPartCount]uint32
}

type currentAvatarFittingRoomConfig struct {
	TicketItemID     uint32
	GaugeThreshold   int64
	GaugeRewardID    uint32
	GaugeRewardCount int64
	NormalRate       int64
	RareRate         int64
	Sets             []currentAvatarFittingRoomSet
}

func loadCurrentAvatarFittingRoomConfig(source dnfpvf.Source) (currentAvatarFittingRoomConfig, error) {
	if source == nil {
		return currentAvatarFittingRoomConfig{}, errCurrentAvatarFittingRoomUnavailable
	}
	text, err := source.ReadText(currentAvatarFittingRoomPVFPath)
	if err != nil {
		return currentAvatarFittingRoomConfig{}, errors.Join(errCurrentAvatarFittingRoomUnavailable, err)
	}
	document, err := dnfpvf.Parse(currentAvatarFittingRoomPVFPath, text)
	if err != nil {
		return currentAvatarFittingRoomConfig{}, errors.Join(errCurrentAvatarFittingRoomUnavailable, err)
	}
	config := currentAvatarFittingRoomConfig{}
	ticket := currentAvatarFittingRoomSectionInts(document, "ticket index")
	vip := currentAvatarFittingRoomSectionInts(document, "vip gauge")
	rates := currentAvatarFittingRoomSectionInts(document, "rate")
	if len(ticket) < 1 || ticket[0] <= 0 || ticket[0] > math.MaxUint32 ||
		len(vip) < 3 || vip[0] <= 0 || vip[1] <= 0 || vip[1] > math.MaxUint32 || vip[2] <= 0 ||
		len(rates) < 2 || rates[0] < 0 || rates[1] < 0 || rates[0]+rates[1] <= 0 {
		return currentAvatarFittingRoomConfig{}, fmt.Errorf("%w: incomplete %s", errCurrentAvatarFittingRoomUnavailable, currentAvatarFittingRoomPVFPath)
	}
	config.TicketItemID = uint32(ticket[0])
	config.GaugeThreshold = vip[0]
	config.GaugeRewardID = uint32(vip[1])
	config.GaugeRewardCount = vip[2]
	config.NormalRate = rates[0]
	config.RareRate = rates[1]
	for _, section := range document.Sections {
		if !strings.EqualFold(strings.TrimSpace(section.Name), "fitting room avatar") {
			continue
		}
		if section.Start < 0 || section.End > len(document.Tokens) || section.Start > section.End {
			return currentAvatarFittingRoomConfig{}, errCurrentAvatarFittingRoomUnavailable
		}
		set, parseErr := parseCurrentAvatarFittingRoomSet(document.Tokens[section.Start:section.End])
		if parseErr != nil {
			return currentAvatarFittingRoomConfig{}, parseErr
		}
		config.Sets = append(config.Sets, set)
	}
	if len(config.Sets) == 0 {
		return currentAvatarFittingRoomConfig{}, errCurrentAvatarFittingRoomUnavailable
	}
	return config, nil
}

func parseCurrentAvatarFittingRoomSet(tokens []dnfpvf.Token) (currentAvatarFittingRoomSet, error) {
	set := currentAvatarFittingRoomSet{Jobs: make(map[byte][currentAvatarFittingRoomPartCount]uint32)}
	markerFound := false
	jobTags := make(map[string]byte)
	for job := 0; job <= jobmap.MaxID; job++ {
		for _, tag := range jobmap.QuestTags(job) {
			jobTags[normalizeCurrentAvatarFittingRoomLabel(tag)] = byte(job)
		}
	}
	currentJob := byte(0)
	jobActive := false
	sequential := 0
	values := [currentAvatarFittingRoomPartCount]uint32{}
	seen := [currentAvatarFittingRoomPartCount]bool{}
	flush := func() error {
		if !jobActive {
			return nil
		}
		for index := range seen {
			if !seen[index] || values[index] == 0 {
				return fmt.Errorf("%w: job=%d missing fitting part=%s", errCurrentAvatarFittingRoomUnavailable, currentJob, currentAvatarFittingRoomPartNames[index])
			}
		}
		set.Jobs[currentJob] = values
		return nil
	}
	for index := 0; index < len(tokens); index++ {
		token := tokens[index]
		if !markerFound && token.Kind == dnfpvf.TokenInt {
			set.Rare = token.Int != 0
			markerFound = true
			continue
		}
		if token.Kind != dnfpvf.TokenString && token.Kind != dnfpvf.TokenIdent {
			continue
		}
		label := normalizeCurrentAvatarFittingRoomLabel(token.Value)
		if job, ok := jobTags[label]; ok {
			if err := flush(); err != nil {
				return currentAvatarFittingRoomSet{}, err
			}
			currentJob, jobActive, sequential = job, true, 0
			values = [currentAvatarFittingRoomPartCount]uint32{}
			seen = [currentAvatarFittingRoomPartCount]bool{}
			continue
		}
		if !jobActive {
			continue
		}
		partIndex := currentAvatarFittingRoomPartIndex(label)
		if partIndex < 0 {
			continue
		}
		for lookahead := index + 1; lookahead < len(tokens); lookahead++ {
			if tokens[lookahead].Kind == dnfpvf.TokenInt {
				if tokens[lookahead].Int <= 0 || tokens[lookahead].Int > math.MaxUint32 {
					return currentAvatarFittingRoomSet{}, errCurrentAvatarFittingRoomUnavailable
				}
				values[partIndex] = uint32(tokens[lookahead].Int)
				seen[partIndex] = true
				index = lookahead
				break
			}
			if tokens[lookahead].Kind == dnfpvf.TokenString || tokens[lookahead].Kind == dnfpvf.TokenIdent {
				break
			}
		}
		sequential++
	}
	if err := flush(); err != nil {
		return currentAvatarFittingRoomSet{}, err
	}
	if !markerFound || len(set.Jobs) == 0 {
		return currentAvatarFittingRoomSet{}, errCurrentAvatarFittingRoomUnavailable
	}
	return set, nil
}

func currentAvatarFittingRoomSectionInts(document *dnfpvf.Document, name string) []int64 {
	if document == nil {
		return nil
	}
	for _, section := range document.Sections {
		if !strings.EqualFold(strings.TrimSpace(section.Name), name) || section.Start < 0 || section.End > len(document.Tokens) || section.Start > section.End {
			continue
		}
		values := make([]int64, 0, section.End-section.Start)
		for _, token := range document.Tokens[section.Start:section.End] {
			if token.Kind == dnfpvf.TokenInt {
				values = append(values, token.Int)
			}
		}
		return values
	}
	return nil
}

func normalizeCurrentAvatarFittingRoomLabel(value string) string {
	value = strings.ToLower(strings.TrimSpace(strings.Trim(value, "`[]'\"")))
	value = strings.TrimSuffix(value, " avatar")
	return strings.Join(strings.Fields(value), " ")
}

func currentAvatarFittingRoomPartIndex(label string) int {
	switch normalizeCurrentAvatarFittingRoomLabel(label) {
	case "hair":
		return 0
	case "hat", "cap":
		return 1
	case "face":
		return 2
	case "neck", "breast":
		return 3
	case "coat", "top":
		return 4
	case "belt", "waist":
		return 5
	case "pants", "bottom":
		return 6
	case "shoes":
		return 7
	default:
		return -1
	}
}

func currentAvatarFittingRoomSetForJob(config currentAvatarFittingRoomConfig, job byte) (currentAvatarFittingRoomSet, error) {
	var normal, rare []currentAvatarFittingRoomSet
	for _, set := range config.Sets {
		if _, ok := set.Jobs[job]; !ok {
			continue
		}
		if set.Rare {
			rare = append(rare, set)
		} else {
			normal = append(normal, set)
		}
	}
	if len(normal) == 0 && len(rare) == 0 {
		return currentAvatarFittingRoomSet{}, fmt.Errorf("%w: job=%d has no fitting pool", errCurrentAvatarFittingRoomUnavailable, job)
	}
	normalRate, rareRate := config.NormalRate, config.RareRate
	if len(normal) == 0 {
		normalRate = 0
	}
	if len(rare) == 0 {
		rareRate = 0
	}
	total := normalRate + rareRate
	roll, err := currentDisjointRandomUint64(uint64(total))
	if err != nil {
		return currentAvatarFittingRoomSet{}, err
	}
	pool := normal
	if int64(roll) >= normalRate {
		pool = rare
	}
	selected, err := currentDisjointRandomUint64(uint64(len(pool)))
	if err != nil {
		return currentAvatarFittingRoomSet{}, err
	}
	return pool[selected], nil
}
