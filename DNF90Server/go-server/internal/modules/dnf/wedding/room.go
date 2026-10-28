package wedding

import (
	"context"
	"crypto/rand"
	"fmt"
	"math/big"
	"sort"
	"strconv"
	"strings"
	"time"

	"longheng.io/server/internal/modules/dnf/pvf"
	repo "longheng.io/server/internal/modules/dnf/repository"
)

type TreeReward struct {
	Item
	Weight int64
}
type Tree struct {
	Days, DailyWater, MaxGrowth int64
	Water                       map[int64]int64
	Rewards                     []TreeReward
}
type RoomCatalog struct {
	Seeds     map[int64]int64
	Trees     map[int64]Tree
	Positions map[int64]map[int]bool
}
type RoomState struct {
	Installed                                   [28]int64
	Cargo                                       [40]Item
	Tree, Expires, Growth, WaterDay, WaterCount int64
}
type RoomCommand struct {
	ActorID, PartnerID, AccountID string
	Opcode                        uint16
	Slot                          int32
	ItemID, Count                 int64
	From, To                      byte
	FromSlot, ToSlot              int
	FromItem, ToItem              int64
	Gold                          int64
	Materials                     []Item
	Now                           time.Time
	Grant                         func(*repo.InventoryRecord, Item) error
}
type RoomResult struct {
	Result
	Room   RoomState
	Reward Item
	Slot   int
}

func LoadRoomCatalog(source pvf.Source) (*RoomCatalog, error) {
	c := &RoomCatalog{Seeds: map[int64]int64{}, Trees: map[int64]Tree{}, Positions: map[int64]map[int]bool{}}
	for _, path := range []string{"decoration/coupletree.etc", "decoration/coupleroomiteminfo.etc"} {
		text, err := source.ReadText(path)
		if err != nil {
			return nil, err
		}
		doc, err := pvf.Parse(path, text)
		if err != nil {
			return nil, err
		}
		var tree Tree
		var index int64
		for _, section := range doc.Sections {
			tokens := doc.Tokens[section.Start:section.End]
			var values []int64
			for _, token := range tokens {
				if token.Kind == pvf.TokenInt {
					values = append(values, token.Int)
				}
			}
			switch strings.ToLower(section.Name) {
			case "seed":
				if len(values)%2 != 0 {
					return nil, Invalid
				}
				for i := 0; i < len(values); i += 2 {
					c.Seeds[values[i]] = values[i+1]
				}
			case "tree":
				tree = Tree{Water: map[int64]int64{}}
				index = 0
			case "index":
				if len(values) == 1 {
					index = values[0]
				}
			case "expire day":
				if len(values) == 1 {
					tree.Days = values[0]
				}
			case "day water count":
				if len(values) == 1 {
					tree.DailyWater = values[0]
				}
			case "grow point":
				if len(values)%2 != 0 {
					return nil, Invalid
				}
				for i := 0; i < len(values); i += 2 {
					if values[i] <= 0 || values[i+1] <= 0 {
						return nil, Invalid
					}
					tree.Water[values[i]] = values[i+1]
				}
			case "step":
				for _, v := range values {
					tree.MaxGrowth = max(tree.MaxGrowth, v)
				}
			case "reward":
				if len(values)%3 != 0 {
					return nil, Invalid
				}
				for i := 0; i < len(values); i += 3 {
					if values[i] <= 0 || values[i+1] <= 0 || values[i+1] > 1000000 || values[i+2] <= 0 {
						return nil, Invalid
					}
					tree.Rewards = append(tree.Rewards, TreeReward{Item{values[i], values[i+2]}, values[i+1]})
				}
			case "/tree":
				if index <= 0 || tree.Days <= 0 || tree.Days > 365 || tree.DailyWater <= 0 || tree.DailyWater > 255 || tree.MaxGrowth <= 0 || len(tree.Water) == 0 || len(tree.Rewards) == 0 {
					return nil, Invalid
				}
				c.Trees[index] = tree
			case "coupleroom item":
				if len(values) < 3 || values[1] < 1 || values[1] > 28 || len(values) != 3+5*int(values[1]) {
					return nil, Invalid
				}
				positions := map[int]bool{}
				for i := int64(0); i < values[1]; i++ {
					p := values[2+5*i]
					if p < 0 || p >= 28 {
						return nil, Invalid
					}
					positions[int(p)] = true
				}
				c.Positions[values[0]] = positions
			}
		}
	}
	if len(c.Seeds) == 0 || len(c.Positions) == 0 {
		return nil, Invalid
	}
	for item, index := range c.Seeds {
		if item <= 0 || c.Trees[index].MaxGrowth <= 0 {
			return nil, Invalid
		}
	}
	return c, nil
}

func ReadRoom(character repo.CharacterRecord, pairID uint32, now time.Time) (RoomState, error) {
	var room RoomState
	v := character.Stats
	if v["wedding_house_id"] != int64(pairID) {
		return room, nil
	}
	for i := range room.Installed {
		room.Installed[i] = v[fmt.Sprintf("wedding_house_installed_%d", i)]
		if room.Installed[i] < 0 || room.Installed[i] > 0xffffffff {
			return RoomState{}, Invalid
		}
	}
	for i := range room.Cargo {
		room.Cargo[i] = Item{v[fmt.Sprintf("wedding_house_cargo_%d", i)], v[fmt.Sprintf("wedding_house_count_%d", i)]}
		if room.Cargo[i].ID < 0 || room.Cargo[i].ID > 0xffffffff || room.Cargo[i].Count < 0 || room.Cargo[i].Count > 65535 || (room.Cargo[i].ID == 0) != (room.Cargo[i].Count == 0) {
			return RoomState{}, Invalid
		}
	}
	room.Tree, room.Expires, room.Growth = v["wedding_house_tree"], v["wedding_house_expires"], v["wedding_house_growth"]
	room.WaterDay, room.WaterCount = v["wedding_house_day"], v["wedding_house_water"]
	if room.Tree < 0 || room.Tree > 0xffffffff || room.Expires < 0 || room.Expires > 0xffffffff || room.Growth < 0 || room.Growth > 0xffffffff || room.WaterCount < 0 || room.WaterCount > 255 {
		return RoomState{}, Invalid
	}
	if room.Tree != 0 && room.Expires <= now.Unix() {
		room.Tree, room.Expires, room.Growth = 0, 0, 0
	}
	day, _ := strconv.ParseInt(now.In(time.FixedZone("CST", 8*3600)).Format("20060102"), 10, 64)
	if room.WaterDay != day {
		room.WaterDay, room.WaterCount = day, 0
	}
	return room, nil
}

func clearRoom(c *repo.CharacterRecord) {
	for key := range c.Stats {
		if strings.HasPrefix(key, "wedding_house_") {
			delete(c.Stats, key)
		}
	}
}

func storeRoom(c *repo.CharacterRecord, pairID uint32, r RoomState) {
	clearRoom(c)
	c.Stats["wedding_house_id"] = int64(pairID)
	for i, id := range r.Installed {
		if id != 0 {
			c.Stats[fmt.Sprintf("wedding_house_installed_%d", i)] = id
		}
	}
	for i, item := range r.Cargo {
		if item.ID != 0 {
			c.Stats[fmt.Sprintf("wedding_house_cargo_%d", i)] = item.ID
			c.Stats[fmt.Sprintf("wedding_house_count_%d", i)] = item.Count
		}
	}
	for key, value := range map[string]int64{"tree": r.Tree, "expires": r.Expires, "growth": r.Growth, "day": r.WaterDay, "water": r.WaterCount} {
		c.Stats["wedding_house_"+key] = value
	}
}

func CommitRoom(ctx context.Context, repos repo.Group, cat *Catalog, rules *RoomCatalog, cmd RoomCommand) (RoomResult, error) {
	if repos.CharacterTrade == nil || rules == nil || cmd.ActorID == cmd.PartnerID || cmd.Now.IsZero() || cmd.AccountID == "" {
		return RoomResult{}, Invalid
	}
	ids := []string{cmd.ActorID, cmd.PartnerID}
	sort.Strings(ids)
	var out RoomResult
	err := repos.CharacterTrade.WithinCharacterTrade(ctx, ids[0], ids[1], func(chars repo.CharacterRepository, bags repo.InventoryRepository) error {
		for i, id := range []string{cmd.ActorID, cmd.PartnerID} {
			c, found, err := chars.Load(ctx, id)
			if err != nil {
				return err
			}
			if !found {
				return Missing
			}
			out.Characters[i] = repo.CloneCharacter(c)
			b, found, err := bags.Load(ctx, id)
			if err != nil {
				return err
			}
			if !found || b.CharacterID != id {
				return Invalid
			}
			out.Inventories[i] = repo.CloneInventory(b)
		}
		a, b := &out.Characters[0], &out.Characters[1]
		if a.AccountID != cmd.AccountID {
			return Invalid
		}
		if err := Validate(*a, *b, cat, 9, cmd.Now); err != nil {
			return err
		}
		s, _ := Read(*a)
		canonical := a
		if b.CharacterID < a.CharacterID {
			canonical = b
		}
		r, err := ReadRoom(*canonical, s.PairID, cmd.Now)
		if err != nil {
			return err
		}
		bag := &out.Inventories[0]
		switch cmd.Opcode {
		case 1045: // Query the shared decoration storage.
		case 21: // NPC pricing is resolved by the existing shop catalog.
			if len(rules.Positions[cmd.ItemID]) == 0 || cmd.Count < 1 || cmd.Count > 65535 || cmd.Gold < 0 || a.Stats["gold"] < 0 || a.Stats["gold"] > 0x7fffffff {
				return Invalid
			}
			if cmd.Gold > a.Stats["gold"] {
				return GoldMissing
			}
			for _, cost := range cmd.Materials {
				if cost.Count <= 0 {
					return Invalid
				}
				if cost.ID == 10002606 {
					if a.Stats["wedding_love"] != b.Stats["wedding_love"] || a.Stats["wedding_love"] < cost.Count {
						return Error(-9)
					}
					a.Stats["wedding_love"] -= cost.Count
					b.Stats["wedding_love"] -= cost.Count
				} else if err := consume(bag, -1, cost, cmd.Now); err != nil {
					return Error(-9)
				}
			}
			a.Stats["gold"] -= cmd.Gold
			slot := -1
			for i, item := range r.Cargo {
				if item.ID == cmd.ItemID && item.Count <= 65535-cmd.Count {
					slot = i
					break
				}
			}
			if slot < 0 {
				for i, item := range r.Cargo {
					if item.ID == 0 {
						slot = i
						break
					}
				}
			}
			if slot < 0 {
				return Error(-20)
			}
			r.Cargo[slot] = Item{cmd.ItemID, r.Cargo[slot].Count + cmd.Count}
			out.Slot = slot
			out.AssetsChanged = true
		case 1046:
			if err := moveRoomItem(&r, rules, cmd); err != nil {
				return err
			}
		case 1047: // Delete the selected number of stored decorations.
			if cmd.Slot < 0 || cmd.Slot >= 40 || cmd.Count < 1 {
				return Invalid
			}
			item := &r.Cargo[cmd.Slot]
			if item.ID != cmd.ItemID || item.Count < cmd.Count {
				return Invalid
			}
			item.Count -= cmd.Count
			if item.Count == 0 {
				*item = Item{}
			}
		case 1048:
			index := rules.Seeds[cmd.ItemID]
			tree, ok := rules.Trees[index]
			if r.Tree != 0 || !ok {
				return Error(-4)
			}
			if err := consume(bag, cmd.Slot, Item{cmd.ItemID, 1}, cmd.Now); err != nil {
				return Error(-4)
			}
			r.Tree, r.Expires, r.Growth = index, cmd.Now.Unix()+tree.Days*86400, 0
			out.AssetsChanged = true
		case 1049:
			tree, ok := rules.Trees[r.Tree]
			if !ok || cmd.Now.Unix() >= r.Expires {
				return Error(-4)
			}
			points := tree.Water[cmd.ItemID]
			if points <= 0 {
				return Error(-9)
			}
			if r.Growth >= tree.MaxGrowth {
				return Error(-9)
			}
			if r.WaterCount >= tree.DailyWater {
				return Error(-6)
			}
			if err := consume(bag, cmd.Slot, Item{cmd.ItemID, 1}, cmd.Now); err != nil {
				return Error(-9)
			}
			r.Growth = min(tree.MaxGrowth, r.Growth+points)
			r.WaterCount++
			out.AssetsChanged = true
		case 1050:
			tree, ok := rules.Trees[r.Tree]
			if !ok || cmd.Now.Unix() >= r.Expires {
				return Error(-4)
			}
			if r.Growth < tree.MaxGrowth {
				return Error(-6)
			}
			if cmd.Grant == nil {
				return Invalid
			}
			var total int64
			for _, reward := range tree.Rewards {
				total += reward.Weight
			}
			if total <= 0 {
				return Invalid
			}
			roll, err := rand.Int(rand.Reader, big.NewInt(total))
			if err != nil {
				return err
			}
			n := roll.Int64()
			for _, reward := range tree.Rewards {
				n -= reward.Weight
				if n < 0 {
					out.Reward = reward.Item
					break
				}
			}
			for i := range out.Inventories {
				if err := cmd.Grant(&out.Inventories[i], out.Reward); err != nil {
					return err
				}
			}
			r.Growth = 0
			out.AssetsChanged = true
		case 1051:
			r.Tree, r.Expires, r.Growth = 0, 0, 0
		default:
			return Invalid
		}
		if cmd.Opcode != 1045 {
			storeRoom(canonical, s.PairID, r)
			for _, c := range out.Characters {
				c.UpdatedAt = cmd.Now
				if err := repo.SaveCharacterFields(ctx, chars, c, repo.CharacterFieldStats); err != nil {
					return err
				}
			}
			if out.AssetsChanged {
				for _, bag := range out.Inventories {
					bag.UpdatedAt = cmd.Now
					if err := repo.SaveInventoryFields(ctx, bags, bag, repo.InventoryFieldSlots); err != nil {
						return err
					}
				}
			}
		}
		out.Room = r
		return nil
	})
	if err != nil {
		return RoomResult{}, err
	}
	return out, nil
}

func moveRoomItem(r *RoomState, c *RoomCatalog, cmd RoomCommand) error {
	if cmd.From > 1 || cmd.To > 1 || cmd.FromSlot < 0 || cmd.ToSlot < 0 || (cmd.From == cmd.To && cmd.FromSlot == cmd.ToSlot) {
		return Invalid
	}
	get := func(kind byte, slot int) (Item, bool) {
		if kind == 0 {
			if slot >= len(r.Installed) {
				return Item{}, false
			}
			id := r.Installed[slot]
			if id == 0 {
				return Item{}, true
			}
			return Item{id, 1}, true
		}
		if slot >= len(r.Cargo) {
			return Item{}, false
		}
		return r.Cargo[slot], true
	}
	from, ok := get(cmd.From, cmd.FromSlot)
	if !ok || from.ID == 0 || from.ID != cmd.FromItem {
		return Invalid
	}
	to, ok := get(cmd.To, cmd.ToSlot)
	if !ok {
		return Invalid
	}
	// Native empty destination sometimes carries the source ID.
	if to.ID != 0 && to.ID != cmd.ToItem {
		return Invalid
	}
	if cmd.From == 0 && cmd.To == 0 {
		return Invalid
	}
	if cmd.To == 0 && !c.Positions[from.ID][cmd.ToSlot] {
		return Error(-20)
	}
	if cmd.From == 1 && cmd.To == 1 {
		r.Cargo[cmd.FromSlot], r.Cargo[cmd.ToSlot] = to, from
		return nil
	}
	if cmd.From == 0 {
		if to.ID != 0 && to.ID != from.ID {
			return Error(-20)
		}
		if to.Count >= 65535 {
			return Invalid
		}
		r.Cargo[cmd.ToSlot] = Item{from.ID, to.Count + 1}
		r.Installed[cmd.FromSlot] = 0
		return nil
	}
	if to.ID != 0 {
		if from.Count == 1 {
			r.Cargo[cmd.FromSlot] = to
		} else {
			empty := -1
			for i, item := range r.Cargo {
				if item.ID == 0 {
					empty = i
					break
				}
			}
			if empty < 0 {
				return Error(-20)
			}
			r.Cargo[empty] = to
			r.Cargo[cmd.FromSlot].Count--
		}
	} else {
		r.Cargo[cmd.FromSlot].Count--
		if r.Cargo[cmd.FromSlot].Count == 0 {
			r.Cargo[cmd.FromSlot] = Item{}
		}
	}
	r.Installed[cmd.ToSlot] = from.ID
	return nil
}
