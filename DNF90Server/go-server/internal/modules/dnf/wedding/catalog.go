// 本文件从实际 PVF 加载婚前任务、费用、婚礼时间、戒指和心意物品规则。
package wedding

import (
	"fmt"

	"longheng.io/server/internal/modules/dnf/pvf"
)

const PVFPath = "etc/weddingsystem.etc"

type Item struct{ ID, Count int64 }
type Ceremony struct {
	Gold        int64
	Ring        byte
	Invitations int64
	Gift        Item
}
type Catalog struct {
	MinimumLevel                      int64
	Alimony                           [2]int64
	RemarryDays, WeddingDays, MaxLove int64
	QuestIDs                          [2]int64
	Ceremonies                        [3]Ceremony
	Delays                            [3]int64
	PaidType                          byte
	PaidTicket                        Item
	RingTickets                       [6]Item
	LoveItems                         map[int64]int64
	EndSeconds                        int64
	Positions                         [2][3]int64
}

func LoadCatalog(source pvf.Source) (*Catalog, error) {
	if source == nil {
		return nil, fmt.Errorf("wedding PVF source is required")
	}
	text, err := source.ReadText(PVFPath)
	if err != nil {
		return nil, err
	}
	doc, err := pvf.Parse(PVFPath, text)
	if err != nil {
		return nil, err
	}
	c := &Catalog{LoveItems: make(map[int64]int64)}
	for _, spec := range []struct {
		Name  string
		N     int
		Apply func([]int64)
	}{
		{"minimum level", 1, func(v []int64) { c.MinimumLevel = v[0] }},
		{"alimony", 2, func(v []int64) { copy(c.Alimony[:], v) }},
		{"days after divorce", 1, func(v []int64) { c.RemarryDays = v[0] }},
		{"once how many days", 1, func(v []int64) { c.WeddingDays = v[0] }},
		{"max love point", 1, func(v []int64) { c.MaxLove = v[0] }},
		{"quest", 2, func(v []int64) { copy(c.QuestIDs[:], v) }},
		{"minute selection", 3, func(v []int64) { copy(c.Delays[:], v) }},
		{"noti end", 1, func(v []int64) { c.EndSeconds = v[0] }},
		{"force move", 6, func(v []int64) { copy(c.Positions[0][:], v[:3]); copy(c.Positions[1][:], v[3:]) }},
		{"ceremony", 12, func(v []int64) {
			for i := 0; i < 3; i++ {
				c.Ceremonies[i] = Ceremony{Gold: v[4*i+1], Ring: byte(v[4*i+2]), Invitations: v[4*i+3]}
			}
		}},
		{"wedding gift", 9, func(v []int64) {
			for i := 0; i < 3; i++ {
				c.Ceremonies[i].Gift = Item{v[3*i+1], v[3*i+2]}
			}
		}},
		{"charged wedding ticket", 3, func(v []int64) { c.PaidType = byte(v[0]); c.PaidTicket = Item{v[1], v[2]} }},
		{"upgrade ring ticket", 12, func(v []int64) {
			for i := 0; i < 6; i++ {
				c.RingTickets[i] = Item{v[2*i], v[2*i+1]}
			}
		}},
		{"love point item", -1, func(v []int64) {
			for i := 0; i < len(v); i += 2 {
				c.LoveItems[v[i]] = v[i+1]
			}
		}},
	} {
		tokens, ok := doc.Section(spec.Name)
		if !ok || (spec.N >= 0 && len(tokens) != spec.N) || (spec.N == -1 && len(tokens)%2 != 0) {
			return nil, fmt.Errorf("wedding PVF section %s has invalid length", spec.Name)
		}
		values := make([]int64, len(tokens))
		for i, t := range tokens {
			if t.Kind != pvf.TokenInt || t.Int < 0 || t.Int > 0x7fffffff {
				return nil, fmt.Errorf("invalid wedding PVF %s[%d]", spec.Name, i)
			}
			values[i] = t.Int
		}
		if spec.Name == "charged wedding ticket" && values[0] > 2 {
			return nil, fmt.Errorf("invalid paid wedding type")
		}
		if spec.Name == "ceremony" {
			for i := 0; i < 3; i++ {
				if values[4*i+2] > 5 {
					return nil, fmt.Errorf("invalid wedding ring")
				}
			}
		}
		if spec.Name == "ceremony" || spec.Name == "wedding gift" {
			stride := 4
			if spec.Name == "wedding gift" {
				stride = 3
			}
			for i := 0; i < 3; i++ {
				if values[i*stride] != int64(i) {
					return nil, fmt.Errorf("unordered wedding PVF %s", spec.Name)
				}
			}
		}
		spec.Apply(values)
	}
	if c.MinimumLevel < 30 || c.MinimumLevel > 127 || c.MaxLove <= 0 || c.PaidType > 2 || c.RemarryDays > 365 || c.WeddingDays > 365 {
		return nil, fmt.Errorf("invalid wedding PVF rules")
	}
	for _, d := range c.Delays {
		if d <= 0 || d > 86400 {
			return nil, fmt.Errorf("invalid wedding ceremony delay")
		}
	}
	if c.EndSeconds <= 0 || c.EndSeconds > 3600 {
		return nil, fmt.Errorf("invalid wedding end interval")
	}
	if c.QuestIDs[0] <= 0 || c.QuestIDs[1] <= 0 || c.PaidTicket.ID <= 0 || c.PaidTicket.Count <= 0 {
		return nil, fmt.Errorf("invalid wedding prerequisites")
	}
	for _, ceremony := range c.Ceremonies {
		if ceremony.Gift.ID <= 0 || ceremony.Gift.Count <= 0 || ceremony.Invitations > 65535 {
			return nil, fmt.Errorf("invalid wedding gifts")
		}
	}
	for _, ticket := range c.RingTickets[1:] {
		if ticket.ID <= 0 || ticket.Count <= 0 {
			return nil, fmt.Errorf("invalid wedding ring ticket")
		}
	}
	for _, pos := range c.Positions {
		for _, v := range pos {
			if v > 32767 {
				return nil, fmt.Errorf("invalid wedding position")
			}
		}
	}
	return c, nil
}
