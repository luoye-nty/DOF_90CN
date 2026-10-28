// Package wedding owns reciprocal marriage state and the two-character asset transaction.
package wedding

import (
	"context"
	"encoding/binary"
	"fmt"
	"sort"
	"strconv"
	"strings"
	"time"

	"longheng.io/server/internal/modules/dnf/quest"
	repo "longheng.io/server/internal/modules/dnf/repository"
)

type Error int32

func (e Error) Error() string { return fmt.Sprintf("wedding result %d", e) }

const (
	Missing     Error = -101
	Invalid     Error = -103
	Busy        Error = -102
	GoldMissing Error = -7
	Cooldown    Error = -151
)

type State struct {
	ChannelID                                uint16
	PairID                                   uint32
	PartnerID                                uint16
	Status, Ring, Ceremony                   byte
	Love                                     int64
	Date, DivorcedAt, NextWeddingAt, StartAt int64
	Room                                     byte
}

func Read(c repo.CharacterRecord) (State, error) {
	v := c.Stats
	if v["wedding_channel"] < 0 || v["wedding_channel"] > 65535 {
		return State{}, Invalid
	}
	s := State{PairID: uint32(v["wedding_id"]), PartnerID: uint16(v["wedding_partner_id"]), Status: byte(v["wedding_state"]), Ring: byte(v["wedding_ring"]), Ceremony: byte(v["wedding_ceremony"]), Love: v["wedding_love"], Date: v["wedding_date"], DivorcedAt: v["wedding_divorced_at"], NextWeddingAt: v["wedding_next_at"], StartAt: v["wedding_start_at"], Room: byte(v["wedding_room"])}
	s.ChannelID = uint16(v["wedding_channel"])
	for _, spec := range []struct {
		key string
		max int64
	}{{"wedding_id", 0xffffffff}, {"wedding_partner_id", 65535}, {"wedding_state", 7}, {"wedding_ring", 5}, {"wedding_ceremony", 2}, {"wedding_room", 255}, {"wedding_love", 0x7fffffff}} {
		if v[spec.key] < 0 || v[spec.key] > spec.max {
			return State{}, Invalid
		}
	}
	for _, stamp := range []int64{s.Date, s.DivorcedAt, s.NextWeddingAt, s.StartAt} {
		if stamp < 0 || stamp > 0xffffffff {
			return State{}, Invalid
		}
	}
	if s.Status > 4 || ((s.Status > 0) != (s.PartnerID > 0 && s.PairID > 0)) || (s.Status == 0 && (s.PartnerID != 0 || s.PairID != 0)) {
		return State{}, Invalid
	}
	return s, nil
}

func store(c *repo.CharacterRecord, s State, now time.Time) {
	if c.Stats == nil {
		c.Stats = make(map[string]int64)
	}
	for k, v := range map[string]int64{"id": int64(s.PairID), "partner_id": int64(s.PartnerID), "state": int64(s.Status), "ring": int64(s.Ring), "ceremony": int64(s.Ceremony), "love": s.Love, "date": s.Date, "divorced_at": s.DivorcedAt, "next_at": s.NextWeddingAt, "start_at": s.StartAt, "room": int64(s.Room)} {
		c.Stats["wedding_"+k] = v
	}
	c.UpdatedAt = now
	c.Stats["wedding_channel"] = int64(s.ChannelID)
}

// Sex follows the current EXE sub_1830F40, including the later job IDs.
func Sex(job string) (byte, error) {
	n, err := strconv.Atoi(job)
	if err != nil || n < 0 || n > 15 {
		return 0, Invalid
	}
	switch n {
	case 0, 2, 4, 7, 8, 9, 13, 15:
		return 1, nil
	default:
		return 0, nil
	}
}

func Validate(a, b repo.CharacterRecord, c *Catalog, operation byte, now time.Time) error {
	if c == nil {
		return Invalid
	}
	if a.CharacterID == "" || a.CharacterID == b.CharacterID || a.Stats["delete_flag"] != 0 || b.Stats["delete_flag"] != 0 {
		return Missing
	}
	for _, r := range []repo.CharacterRecord{a, b} {
		id, err := strconv.ParseUint(r.CharacterID, 10, 16)
		if err != nil || id == 0 || strconv.FormatUint(id, 10) != r.CharacterID {
			return Invalid
		}
	}
	x, err := Read(a)
	if err != nil {
		return err
	}
	y, err := Read(b)
	if err != nil {
		return err
	}
	if operation == 1 {
		if x.Status != 0 || y.Status != 0 {
			return Error(-2)
		}
		if int64(a.Level) < c.MinimumLevel || int64(b.Level) < c.MinimumLevel {
			return Error(-3)
		}
		sx, e1 := Sex(a.Job)
		sy, e2 := Sex(b.Job)
		if e1 != nil || e2 != nil || sx == sy {
			return Error(-3)
		}
		for _, s := range []State{x, y} {
			if s.DivorcedAt > 0 && now.Unix() < s.DivorcedAt+c.RemarryDays*86400 {
				return Cooldown
			}
		}
		return nil
	}
	if x.PairID == 0 || x.PairID != y.PairID || strconv.Itoa(int(x.PartnerID)) != b.CharacterID || strconv.Itoa(int(y.PartnerID)) != a.CharacterID || x.Status != y.Status {
		return Invalid
	}
	if (x.Status == 2 || x.Status == 3) && (x.StartAt <= 0 || x.StartAt != y.StartAt || x.Ceremony != y.Ceremony || x.Room < 8 || x.Room > 38 || x.Room != y.Room) {
		return Invalid
	}
	if x.ChannelID != y.ChannelID {
		return Invalid
	}
	switch operation {
	case 2:
		if x.Status != 1 {
			return Invalid
		}
		if now.Unix() < x.NextWeddingAt || now.Unix() < y.NextWeddingAt {
			return Error(-199)
		}
	case 3:
		if x.Status != 2 || now.Unix() < x.StartAt {
			return Invalid
		}
	case 4:
		if x.Status != 3 || now.Unix() < x.StartAt+c.EndSeconds {
			return Invalid
		}
	case 5:
		if x.Status != 1 {
			return Invalid
		}
	case 6, 7:
		if x.Status != 4 {
			return Invalid
		}
	case 8, 9:
		if x.Status != 4 {
			return Invalid
		}
	default:
		return Invalid
	}
	return nil
}

type Command struct {
	ChannelID                          uint16
	ActorID, PartnerID, AccountID      string
	PartnerAccountID                   string
	Operation, Kind, Delay, Room, Ring byte
	Slot                               int32
	Now                                time.Time
	// The bridge supplies PVF-backed item placement inside this transaction.
	Grant func(*repo.InventoryRecord, Item) error
}
type Result struct {
	Characters    [2]repo.CharacterRecord
	Inventories   [2]repo.InventoryRecord
	AssetsChanged bool
}

func Commit(ctx context.Context, repos repo.Group, c *Catalog, cmd Command) (Result, error) {
	if c == nil || repos.CharacterTrade == nil || cmd.ActorID == cmd.PartnerID || cmd.AccountID == "" || cmd.Now.IsZero() {
		return Result{}, Invalid
	}
	ids := []string{cmd.ActorID, cmd.PartnerID}
	sort.Strings(ids)
	if cmd.Operation == 2 {
		// Completed quests are prerequisites, not wedding writes. Load before the two-character UOW.
		if repos.Quest == nil {
			return Result{}, Invalid
		}
		for _, id := range ids {
			quests, found, err := repos.Quest.Load(ctx, id)
			if err != nil {
				return Result{}, err
			}
			if !found || !quest.IsCompleted(quests, c.QuestIDs[0]) || !quest.IsCompleted(quests, c.QuestIDs[1]) {
				return Result{}, Error(-9)
			}
		}
	}
	var result Result
	err := repos.CharacterTrade.WithinCharacterTrade(ctx, ids[0], ids[1], func(chars repo.CharacterRepository, bags repo.InventoryRepository) error {
		loaded := map[string]repo.CharacterRecord{}
		for _, id := range ids {
			v, found, e := chars.Load(ctx, id)
			if e != nil {
				return e
			}
			if !found {
				return Missing
			}
			loaded[id] = repo.CloneCharacter(v)
		}
		a, b := loaded[cmd.ActorID], loaded[cmd.PartnerID]
		if a.AccountID != cmd.AccountID || (cmd.PartnerAccountID != "" && b.AccountID != cmd.PartnerAccountID) {
			return Invalid
		}
		if err := Validate(a, b, c, cmd.Operation, cmd.Now); err != nil {
			return err
		}
		x, _ := Read(a)
		y, _ := Read(b)
		var ia, ib repo.InventoryRecord
		if cmd.Operation == 2 || cmd.Operation == 4 || cmd.Operation == 8 || cmd.Operation == 9 {
			for _, id := range ids {
				bag, found, e := bags.Load(ctx, id)
				if e != nil {
					return e
				}
				if !found {
					return Invalid
				}
				if bag.CharacterID != id {
					return Invalid
				}
				if id == cmd.ActorID {
					ia = repo.CloneInventory(bag)
				} else {
					ib = repo.CloneInventory(bag)
				}
			}
		}
		switch cmd.Operation {
		case 1:
			clearCapsule(&a)
			clearCapsule(&b)
			clearRoom(&a)
			clearRoom(&b)
			ai, _ := strconv.ParseUint(a.CharacterID, 10, 16)
			bi, _ := strconv.ParseUint(b.CharacterID, 10, 16)
			// Both IDs are validated u16; this is an injective unordered-pair key, not a hash.
			pair := uint32(min(ai, bi)<<16 | max(ai, bi))
			x.PairID, x.PartnerID, x.Status = pair, uint16(bi), 1
			y.PairID, y.PartnerID, y.Status = pair, uint16(ai), 1
		case 2:
			if cmd.Kind > 2 || cmd.Delay > 2 || cmd.Room < 8 || cmd.Room > 38 {
				return Invalid
			}
			ceremony := c.Ceremonies[cmd.Kind]
			if cmd.Grant == nil {
				return Invalid
			}
			// Validate the real reward and both bag capacities before accepting payment.
			for _, bag := range []repo.InventoryRecord{ia, ib} {
				preview := repo.CloneInventory(bag)
				if err := cmd.Grant(&preview, ceremony.Gift); err != nil {
					return err
				}
			}
			if a.Stats["gold"] < ceremony.Gold {
				return Error(-16)
			}
			if cmd.Kind == c.PaidType {
				if err := consume(&ia, -1, c.PaidTicket, cmd.Now); err != nil {
					return Error(-14)
				}
			}
			if a.Stats == nil {
				a.Stats = make(map[string]int64)
			}
			a.Stats["gold"] -= ceremony.Gold
			for _, s := range []*State{&x, &y} {
				s.Status = 2
				s.Ceremony = cmd.Kind
				s.Room = cmd.Room
				if cmd.Operation == 2 {
					s.ChannelID = cmd.ChannelID
				}
				s.StartAt = cmd.Now.Unix() + c.Delays[cmd.Delay]
				s.NextWeddingAt = cmd.Now.Unix() + c.WeddingDays*86400
			}
			result.AssetsChanged = true
		case 3:
			x.Status, y.Status = 3, 3
		case 4:
			if cmd.Grant == nil {
				return Invalid
			}
			gift := c.Ceremonies[x.Ceremony].Gift
			if err := cmd.Grant(&ia, gift); err != nil {
				return err
			}
			if err := cmd.Grant(&ib, gift); err != nil {
				return err
			}
			for _, s := range []*State{&x, &y} {
				s.Status = 4
				s.Ring = c.Ceremonies[s.Ceremony].Ring
				s.Date = cmd.Now.Unix()
				s.StartAt = 0
				s.Room = 0
			}
			result.AssetsChanged = true
		case 5, 6, 7:
			clearCapsule(&a)
			clearCapsule(&b)
			clearRoom(&a)
			clearRoom(&b)
			for _, c := range []*repo.CharacterRecord{&a, &b} {
				if c.Stats["mood_value"] == 1001 {
					c.Stats["mood_value"] = 0
				}
			}
			if cmd.Operation == 5 {
				x = State{DivorcedAt: x.DivorcedAt, NextWeddingAt: x.NextWeddingAt}
				y = State{DivorcedAt: y.DivorcedAt, NextWeddingAt: y.NextWeddingAt}
				break
			}
			cost := c.Alimony[0]
			if cmd.Operation == 6 {
				cost = c.Alimony[1]
			}
			if a.Stats["gold"] < cost || (cmd.Operation == 7 && b.Stats["gold"] < cost) {
				return GoldMissing
			}
			a.Stats["gold"] -= cost
			if cmd.Operation == 7 {
				b.Stats["gold"] -= cost
			}
			x = State{DivorcedAt: cmd.Now.Unix(), NextWeddingAt: x.NextWeddingAt}
			y = State{DivorcedAt: cmd.Now.Unix(), NextWeddingAt: y.NextWeddingAt}
			result.AssetsChanged = cost != 0
		case 8:
			if cmd.Slot < 3 || cmd.Slot > 353 {
				return Invalid
			}
			if cmd.Ring < 1 || cmd.Ring > 5 || x.Ring+1 != cmd.Ring || y.Ring != x.Ring {
				return Error(-2)
			}
			if err := consume(&ia, cmd.Slot, c.RingTickets[cmd.Ring], cmd.Now); err != nil {
				return Error(-4)
			}
			x.Ring, y.Ring = cmd.Ring, cmd.Ring
			result.AssetsChanged = true
		case 9:
			if cmd.Slot < 3 || cmd.Slot > 353 {
				return Invalid
			}
			stack := ia.Slots[fmt.Sprintf("0:%d", cmd.Slot)]
			points := c.LoveItems[stack.ItemID]
			if points <= 0 || x.Love < 0 || x.Love > c.MaxLove || points > c.MaxLove-x.Love || y.Love != x.Love {
				return Error(-2)
			}
			if err := consume(&ia, cmd.Slot, Item{stack.ItemID, 1}, cmd.Now); err != nil {
				return Error(-4)
			}
			x.Love += points
			y.Love = x.Love
			result.AssetsChanged = true
		}
		store(&a, x, cmd.Now)
		store(&b, y, cmd.Now)
		for _, character := range []repo.CharacterRecord{a, b} {
			if err := repo.SaveCharacterFields(ctx, chars, character, repo.CharacterFieldStats); err != nil {
				return err
			}
		}
		if ia.CharacterID != "" {
			ia.UpdatedAt, ib.UpdatedAt = cmd.Now, cmd.Now
			for _, bag := range []repo.InventoryRecord{ia, ib} {
				if err := repo.SaveInventoryFields(ctx, bags, bag, repo.InventoryFieldSlots); err != nil {
					return err
				}
			}
		}
		result.Characters = [2]repo.CharacterRecord{a, b}
		result.Inventories = [2]repo.InventoryRecord{ia, ib}
		return nil
	})
	if err != nil {
		return Result{}, err
	}
	return result, nil
}

func consume(bag *repo.InventoryRecord, slot int32, item Item, now time.Time) error {
	if item.ID <= 0 || item.Count <= 0 {
		return Invalid
	}
	keys := make([]string, 0, len(bag.Slots))
	for key := range bag.Slots {
		keys = append(keys, key)
	}
	sort.Strings(keys)
	remaining := item.Count
	for _, key := range keys {
		list, raw, valid := strings.Cut(key, ":")
		index, err := strconv.Atoi(raw)
		if !valid || err != nil || list != "0" || raw != strconv.Itoa(index) || index < 3 || index > 353 || (slot >= 0 && index != int(slot)) {
			continue
		}
		stack := bag.Slots[key]
		if stack.ItemID != item.ID || stack.Count <= 0 || (!stack.ExpireAt.IsZero() && !now.Before(stack.ExpireAt)) {
			continue
		}
		switch strings.ToLower(stack.Extra["equipment_lock_state"]) {
		case "1", "2", "active", "locked", "unlocking", "pending_unlock":
			continue
		}
		take := min(remaining, stack.Count)
		remaining -= take
		stack.Count -= take
		if stack.Count == 0 {
			delete(bag.Slots, key)
		} else {
			if stack.Extra == nil {
				stack.Extra = make(map[string]string)
			}
			for _, field := range []string{"amount_or_count", "amount", "count", "stack", "quantity"} {
				stack.Extra[field] = strconv.FormatInt(stack.Count, 10)
			}
			if len(stack.RawEntry) >= 10 {
				binary.LittleEndian.PutUint32(stack.RawEntry[6:10], uint32(stack.Count))
			}
			bag.Slots[key] = stack
		}
		if remaining == 0 {
			return nil
		}
	}
	return Invalid
}
