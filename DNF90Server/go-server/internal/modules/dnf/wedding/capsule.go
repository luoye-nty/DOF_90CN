package wedding

import (
	"context"
	"errors"
	"fmt"
	"sort"
	"strings"
	"time"

	"longheng.io/server/internal/modules/dnf/pvf"
	repo "longheng.io/server/internal/modules/dnf/repository"
)

const CapsulePVFPath = "etc/couplegrowthcapsule.etc"
const capsulePrefix = "wedding_capsule_"

type CapsuleCatalog struct {
	MaxOwn, MaxTaken, Gauge, MaxGauge, ItemID int64
}

func (c *CapsuleCatalog) valid() bool {
	return c != nil && c.MaxOwn > 0 && c.MaxOwn <= 255 && c.MaxTaken > 0 && c.MaxTaken <= 255 && c.Gauge > 0 && c.Gauge <= c.MaxGauge && c.MaxGauge <= 0x7fffffff && c.ItemID > 0 && c.ItemID <= 0x7fffffff
}

func LoadCapsuleCatalog(source pvf.Source) (*CapsuleCatalog, error) {
	if source == nil {
		return nil, Invalid
	}
	text, err := source.ReadText(CapsulePVFPath)
	if err != nil {
		return nil, err
	}
	doc, err := pvf.Parse(CapsulePVFPath, text)
	if err != nil {
		return nil, err
	}
	c := &CapsuleCatalog{}
	for name, field := range map[string]*int64{"max own count": &c.MaxOwn, "max taken count": &c.MaxTaken, "gage": &c.Gauge, "max gage": &c.MaxGauge, "capsule id": &c.ItemID} {
		tokens, ok := doc.Section(name)
		if !ok || len(tokens) != 1 || tokens[0].Kind != pvf.TokenInt {
			return nil, fmt.Errorf("invalid capsule PVF section %s", name)
		}
		*field = tokens[0].Int
	}
	if !c.valid() {
		return nil, Invalid
	}
	return c, nil
}

// Own is crafted stock; Taken is unclaimed stock sent by the spouse. Neither
// is a daily counter. The current EXE's NPC prompt displays all Taken gifts.
type CapsuleState struct{ Progress, Own, Taken int64 }

func ReadCapsule(c repo.CharacterRecord, rules *CapsuleCatalog) (CapsuleState, error) {
	if !rules.valid() {
		return CapsuleState{}, Invalid
	}
	w, err := Read(c)
	if err != nil {
		return CapsuleState{}, err
	}
	if w.Status != 4 || c.Stats[capsulePrefix+"pair"] != int64(w.PairID) || c.Stats[capsulePrefix+"date"] != w.Date {
		return CapsuleState{}, nil
	}
	s := CapsuleState{c.Stats[capsulePrefix+"progress"], c.Stats[capsulePrefix+"own"], c.Stats[capsulePrefix+"taken"]}
	if s.Progress < 0 || s.Progress > rules.MaxGauge || s.Own < 0 || s.Own > rules.MaxOwn || s.Taken < 0 || s.Taken > rules.MaxTaken {
		return CapsuleState{}, Invalid
	}
	return s, nil
}

func storeCapsule(c *repo.CharacterRecord, s CapsuleState) {
	if c.Stats == nil {
		c.Stats = make(map[string]int64)
	}
	c.Stats[capsulePrefix+"pair"] = c.Stats["wedding_id"]
	c.Stats[capsulePrefix+"date"] = c.Stats["wedding_date"]
	c.Stats[capsulePrefix+"progress"], c.Stats[capsulePrefix+"own"], c.Stats[capsulePrefix+"taken"] = s.Progress, s.Own, s.Taken
}

func clearCapsule(c *repo.CharacterRecord) {
	for k := range c.Stats {
		if strings.HasPrefix(k, capsulePrefix) {
			delete(c.Stats, k)
		}
	}
}

// AccumulateCapsuleClear is called inside the receipted dungeon settlement,
// after the server has accepted a recommended clear. It never grants an item.
func AccumulateCapsuleClear(c *repo.CharacterRecord, rules *CapsuleCatalog) error {
	if c == nil || !rules.valid() {
		return Invalid
	}
	w, err := Read(*c)
	if err != nil {
		return err
	}
	if w.Status != 4 || c.Stats["delete_flag"] != 0 {
		return nil
	}
	s, err := ReadCapsule(*c, rules)
	if err != nil {
		return err
	}
	s.Progress = min(rules.MaxGauge, s.Progress+rules.Gauge)
	storeCapsule(c, s)
	return nil
}

var (
	ErrCapsuleEnergy = errors.New("wedding capsule energy insufficient")
	ErrCapsuleLimit  = errors.New("wedding capsule stock limit")
	ErrCapsuleEmpty  = errors.New("wedding capsule stock empty")
	ErrCapsuleBag    = errors.New("wedding capsule inventory grant failed")
)

type CapsuleCommand struct {
	ActorID, PartnerID, AccountID string
	Opcode                        uint16
	Now                           time.Time
	Grant                         func(*repo.InventoryRecord, Item) error
}
type CapsuleResult struct {
	Result
	States  [2]CapsuleState
	Granted int64
}

// Make converts one full gauge into one gift; Give transfers all crafted
// gifts; Take claims all received gifts. Each operation is one dual UOW, so
// offline spouses, concurrent requests and bag failures preserve both stocks.
func CommitCapsule(ctx context.Context, repos repo.Group, cat *Catalog, rules *CapsuleCatalog, cmd CapsuleCommand) (CapsuleResult, error) {
	if cat == nil || !rules.valid() || repos.CharacterTrade == nil || cmd.ActorID == "" || cmd.PartnerID == "" || cmd.ActorID == cmd.PartnerID || cmd.AccountID == "" || cmd.Now.IsZero() || cmd.Opcode < 1052 || cmd.Opcode > 1054 {
		return CapsuleResult{}, Invalid
	}
	ids := []string{cmd.ActorID, cmd.PartnerID}
	sort.Strings(ids)
	var result CapsuleResult
	err := repos.CharacterTrade.WithinCharacterTrade(ctx, ids[0], ids[1], func(chars repo.CharacterRepository, bags repo.InventoryRepository) error {
		for i, id := range []string{cmd.ActorID, cmd.PartnerID} {
			c, found, err := chars.Load(ctx, id)
			if err != nil {
				return err
			}
			if !found {
				return Missing
			}
			result.Characters[i] = repo.CloneCharacter(c)
		}
		a, b := &result.Characters[0], &result.Characters[1]
		if a.AccountID != cmd.AccountID {
			return Invalid
		}
		if err := Validate(*a, *b, cat, 9, cmd.Now); err != nil {
			return err
		}
		for i, c := range result.Characters {
			s, err := ReadCapsule(c, rules)
			if err != nil {
				return err
			}
			result.States[i] = s
		}
		x, y := &result.States[0], &result.States[1]
		switch cmd.Opcode {
		case 1052:
			if x.Progress < rules.MaxGauge {
				return ErrCapsuleEnergy
			}
			if x.Own >= rules.MaxOwn {
				return ErrCapsuleLimit
			}
			x.Progress -= rules.MaxGauge
			x.Own++
		case 1053:
			if x.Own == 0 {
				return ErrCapsuleEmpty
			}
			if x.Own > rules.MaxTaken-y.Taken {
				return ErrCapsuleLimit
			}
			y.Taken += x.Own
			x.Own = 0
		case 1054:
			if x.Taken == 0 {
				return ErrCapsuleEmpty
			}
			if cmd.Grant == nil {
				return Invalid
			}
			bag, found, err := bags.Load(ctx, cmd.ActorID)
			if err != nil {
				return err
			}
			if !found || bag.CharacterID != cmd.ActorID {
				return Invalid
			}
			bag = repo.CloneInventory(bag)
			if err := cmd.Grant(&bag, Item{rules.ItemID, x.Taken}); err != nil {
				return fmt.Errorf("%w: %w", ErrCapsuleBag, err)
			}
			bag.UpdatedAt = cmd.Now
			if err := repo.SaveInventoryFields(ctx, bags, bag, repo.InventoryFieldSlots); err != nil {
				return err
			}
			result.Granted = x.Taken
			x.Taken = 0
			result.Inventories[0], result.AssetsChanged = bag, true
		}
		for i := range result.Characters {
			c := &result.Characters[i]
			storeCapsule(c, result.States[i])
			c.UpdatedAt = cmd.Now
			if err := repo.SaveCharacterFields(ctx, chars, *c, repo.CharacterFieldStats); err != nil {
				return err
			}
		}
		return nil
	})
	if err != nil {
		return CapsuleResult{}, err
	}
	return result, nil
}
