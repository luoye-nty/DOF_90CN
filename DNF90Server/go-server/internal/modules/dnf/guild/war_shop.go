package guild

import (
	"math"
	"path"
	"strings"
	"time"

	dnfpvf "longheng.io/server/internal/modules/dnf/pvf"
)

type WarShop struct {
	ID, NPC uint32
	Items   map[uint32]bool
}

type WarMerchant struct {
	NPC    uint32
	Levels map[uint16]WarShop
	Costs  map[uint16]GuardianCost
}

type WarShopCatalog [2]WarMerchant

func warList(source dnfpvf.Source, listPath string) (map[uint32]string, error) {
	doc, err := readWarDocument(source, listPath)
	if err != nil {
		return nil, err
	}
	out := make(map[uint32]string)
	for _, entry := range dnfpvf.ParseList(doc) {
		if entry.ID <= 0 || entry.ID > math.MaxInt32 {
			continue
		}
		out[uint32(entry.ID)] = path.Join(path.Dir(listPath), strings.TrimPrefix(entry.Path, path.Dir(listPath)+"/"))
	}
	return out, nil
}

func LoadWarShops(source dnfpvf.Source) (WarShopCatalog, error) {
	var out WarShopCatalog
	doc, err := readWarDocument(source, "etc/agitwar.etc")
	if err != nil {
		return out, err
	}
	npcs, err := warList(source, "npc/npc.lst")
	if err != nil {
		return out, err
	}
	shops, err := warList(source, "itemshop/itemshop.lst")
	if err != nil {
		return out, err
	}
	// Current EXE's upgrade requests identify these original NPCs, while
	// purchases identify the level-specific NPC from each [upgrade] row.
	for kind, npc := range []uint32{390, 389} {
		merchant := WarMerchant{NPC: npc, Levels: make(map[uint16]WarShop), Costs: make(map[uint16]GuardianCost)}
		npcdoc, e := readWarDocument(source, npcs[npc])
		if e != nil {
			return out, e
		}
		role, _ := npcdoc.Section("guild agit shop npc")
		if len(role) != 1 || role[0].Value != []string{"defence", "mercenary"}[kind] {
			return out, ErrInvalidContent
		}
		upgrade, _ := npcdoc.Section("upgrade")
		if len(upgrade) < 3 || upgrade[0].Value != "guild agit" {
			return out, ErrInvalidContent
		}
		v, e := warUnsigned(upgrade[1:])
		if e != nil || len(v)%2 != 0 || len(v)/2 > 10 {
			return out, ErrInvalidContent
		}
		for i := 0; i < len(v); i += 2 {
			if v[i] != uint32(i/2+1) || v[i+1] == 0 {
				return out, ErrInvalidContent
			}
			merchant.Levels[uint16(v[i])] = WarShop{NPC: v[i+1]}
		}
		inUpgrade := false
		for _, sec := range doc.Sections {
			if sec.Name == "building upgrade" {
				inUpgrade = true
				continue
			}
			if sec.Name == "/building upgrade" {
				inUpgrade = false
				continue
			}
			if !inUpgrade || sec.Name != []string{"building", "mecenary"}[kind] {
				continue
			}
			v, e = warUnsigned(doc.Tokens[sec.Start:sec.End])
			if e != nil || len(v) != len(merchant.Levels)*2 {
				return out, ErrInvalidContent
			}
			for i := 0; i < len(v); i += 2 {
				merchant.Costs[uint16(i/2+1)] = GuardianCost{v[i], v[i+1]}
			}
		}
		if len(merchant.Costs) != len(merchant.Levels) {
			return out, ErrInvalidContent
		}
		out[kind] = merchant
	}
	tokens, _ := doc.Section("shop list")
	ids, err := warUnsigned(tokens)
	if err != nil || len(ids) == 0 {
		return out, ErrInvalidContent
	}
	for _, id := range ids {
		d, e := readWarDocument(source, shops[id])
		if e != nil {
			return out, e
		}
		npc, ok := d.Int("NPC")
		typ, _ := d.Section("type")
		if !ok || npc <= 0 || len(typ) != 1 || typ[0].Value != "[agitwar build shop]" {
			return out, ErrInvalidContent
		}
		for k := range out {
			for level, shop := range out[k].Levels {
				if shop.NPC != uint32(npc) {
					continue
				}
				if shop.ID != 0 {
					return out, ErrInvalidContent
				}
				shop.ID = id
				shop.Items = make(map[uint32]bool)
				for _, sec := range d.Sections {
					if sec.Name == "item list" {
						v, e := warUnsigned(d.Tokens[sec.Start:sec.End])
						if e != nil {
							return out, e
						}
						for _, item := range v {
							if item > 0 {
								shop.Items[item] = true
							}
						}
					}
				}
				if len(shop.Items) == 0 {
					return out, ErrInvalidContent
				}
				out[k].Levels[level] = shop
			}
		}
	}
	for _, merchant := range out {
		for _, shop := range merchant.Levels {
			if shop.ID == 0 {
				return out, ErrInvalidContent
			}
		}
	}
	return out, nil
}

func (g *Guild) WarShopLevel(kind int) uint16 {
	if g == nil || kind < 0 || kind >= len(g.WarShopLevels) {
		return 0
	}
	level := g.WarShopLevels[kind]
	if level == 0 {
		return 1
	} // Native 38D1020 constructor: two u16 levels, both 1.
	return level
}

func (r *Registry) UpgradeWarShop(characterID string, npc uint32, before, after uint16, catalog WarShopCatalog, now time.Time) (byte, error) {
	g, err := r.warManager(characterID)
	if err != nil {
		return 0, err
	}
	for kind, merchant := range catalog {
		if merchant.NPC != npc {
			continue
		}
		cost, valid := merchant.Costs[after]
		if !valid || before == 0 || uint32(after) != uint32(before)+1 {
			return 0, ErrInvalidContent
		}
		current := g.WarShopLevel(kind)
		if current == after {
			return byte(kind), nil
		}
		if current != before {
			return 0, ErrInvalidContent
		}
		if g.GuardianAP < cost.AP || g.GuardianGP < cost.GP {
			return 0, ErrInsufficientGuardian
		}
		g.GuardianAP -= cost.AP
		g.GuardianGP -= cost.GP
		g.WarShopLevels[kind] = after
		g.UpdatedAt = now
		return byte(kind), nil
	}
	return 0, ErrInvalidContent
}

func LoadWarProductCost(source dnfpvf.Source, itemPath string) (GuardianCost, error) {
	doc, err := readWarDocument(source, itemPath)
	if err != nil {
		return GuardianCost{}, err
	}
	ap, apOK := doc.Int("require ap")
	gp, gpOK := doc.Int("require gp")
	if (!apOK && !gpOK) || ap < 0 || gp < 0 || ap > math.MaxInt32 || gp > math.MaxInt32 || ap+gp == 0 {
		return GuardianCost{}, ErrInvalidContent
	}
	return GuardianCost{uint32(ap), uint32(gp)}, nil
}

func (r *Registry) BuyWarProduct(characterID string, shopID, npc, item, count uint32, catalog WarShopCatalog, cost GuardianCost, now time.Time) error {
	g, err := r.warManager(characterID)
	if err != nil {
		return err
	}
	if count == 0 || count > math.MaxUint16 || cost.AP == 0 && cost.GP == 0 {
		return ErrInvalidContent
	}
	valid := false
	for kind, merchant := range catalog {
		shop, ok := merchant.Levels[g.WarShopLevel(kind)]
		if ok && shop.ID == shopID && shop.NPC == npc && shop.Items[item] {
			valid = true
			break
		}
	}
	if !valid {
		return ErrInvalidContent
	}
	ap, gp := uint64(cost.AP)*uint64(count), uint64(cost.GP)*uint64(count)
	if ap > uint64(g.GuardianAP) || gp > uint64(g.GuardianGP) {
		return ErrInsufficientGuardian
	}
	g.GuardianAP -= uint32(ap)
	g.GuardianGP -= uint32(gp)
	g.UpdatedAt = now
	return nil
}
