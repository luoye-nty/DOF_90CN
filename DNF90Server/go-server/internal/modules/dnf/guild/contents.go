package guild

import (
	"errors"
	"fmt"
	"math"
	"time"

	dnfpvf "longheng.io/server/internal/modules/dnf/pvf"
	dnfrepo "longheng.io/server/internal/modules/dnf/repository"
)

var (
	ErrInvalidContent      = errors.New("invalid guild content")
	ErrContentActive       = errors.New("guild content already active")
	ErrCargoLimitReached   = errors.New("guild cargo limit reached")
	ErrInsufficientMileage = errors.New("insufficient guild mileage")
	ErrContractClaim       = errors.New("guild contract reward is not available")
)

type Content struct {
	ID      uint32
	Type    byte
	Value   int32
	Days    int32
	Counted bool
	ItemID  uint32
	Price   uint32
	Gold    bool
}

type Benefit struct {
	Type      byte            `json:"type"`
	Value     uint32          `json:"value"`
	ItemID    uint32          `json:"item_id,omitempty"`
	Mode      byte            `json:"mode"`
	ExpiresAt time.Time       `json:"expires_at,omitempty"`
	Claimed   map[string]bool `json:"claimed,omitempty"`
}

// Active reports whether this benefit still grants anything.
//
// The `Type != 7 || Value > 0` conjunct comes from the upstream guild branch and
// exists for 公会传送 (type 7), whose Value IS its remaining-use counter rather than
// a magnitude: a type-7 benefit that has spent its last use is not active even
// though its window has not closed. No other benefit type changes behaviour.
func (b Benefit) Active(now time.Time) bool {
	return (b.Type != 7 || b.Value > 0) && (b.ExpiresAt.IsZero() || now.Before(b.ExpiresAt))
}

type Catalog struct {
	Contents      map[uint32]Content
	AuraMarks     uint32
	AuraFrames    uint32
	ContractItem  uint32
	ContractCount uint32
}

func LoadCatalog(source dnfpvf.Source) (Catalog, error) {
	catalog := Catalog{Contents: make(map[uint32]Content)}
	if source == nil {
		return catalog, ErrRepositoryUnavailable
	}
	text, err := source.ReadText("etc/(r)guild.etc")
	if err != nil {
		return catalog, err
	}
	doc, err := dnfpvf.Parse("etc/(r)guild.etc", text)
	if err != nil {
		return catalog, err
	}
	for _, section := range []struct {
		name  string
		width int
	}{{"guild contents list", 9}, {"additional guild contents list", 10}} {
		tokens, found := doc.Section(section.name)
		if !found || len(tokens)%section.width != 0 {
			return catalog, fmt.Errorf("guild catalog: invalid %s", section.name)
		}
		for offset := 0; offset < len(tokens); offset += section.width {
			row := tokens[offset : offset+section.width]
			for i, token := range row {
				if i == 8 {
					if token.Kind != dnfpvf.TokenString {
						return catalog, ErrInvalidContent
					}
					continue
				}
				if token.Kind != dnfpvf.TokenInt || token.Int < -1 || token.Int > math.MaxInt32 {
					return catalog, ErrInvalidContent
				}
			}
			if row[0].Int <= 0 || row[1].Int <= 0 || row[1].Int > 255 || row[3].Int < -1 || row[3].Int > 36500 || row[4].Int < 0 || row[4].Int > 1 || row[5].Int <= 0 || row[7].Int <= 0 {
				return catalog, ErrInvalidContent
			}
			c := Content{ID: uint32(row[0].Int), Type: byte(row[1].Int), Value: int32(row[2].Int), Days: int32(row[3].Int), Counted: row[4].Int == 1, ItemID: uint32(row[5].Int), Price: uint32(row[7].Int)}
			if section.width == 10 {
				if row[9].Int < 0 || row[9].Int > 1 {
					return catalog, ErrInvalidContent
				}
				c.Gold = row[9].Int == 1
			}
			if _, exists := catalog.Contents[c.ID]; exists {
				return catalog, ErrInvalidContent
			}
			catalog.Contents[c.ID] = c
		}
	}
	contract, found := doc.Section("guild contracts")
	if !found || len(contract) != 3 || contract[0].Int != 21 || contract[1].Int <= 0 || contract[1].Int > math.MaxUint32 || contract[2].Int <= 0 || contract[2].Int > math.MaxUint32 {
		return catalog, ErrInvalidContent
	}
	catalog.ContractItem, catalog.ContractCount = uint32(contract[1].Int), uint32(contract[2].Int)
	text, err = source.ReadText("etc/guild/insigniashapelist.etc")
	if err != nil {
		return catalog, err
	}
	doc, err = dnfpvf.Parse("etc/guild/insigniashapelist.etc", text)
	if err != nil {
		return catalog, err
	}
	for _, section := range []struct {
		name  string
		count *uint32
	}{{"insignia mark", &catalog.AuraMarks}, {"insignia frame", &catalog.AuraFrames}} {
		tokens, found := doc.Section(section.name)
		if !found || len(tokens) == 0 {
			return catalog, ErrInvalidContent
		}
		for _, token := range tokens {
			if token.Kind != dnfpvf.TokenString || token.Value == "" {
				return catalog, ErrInvalidContent
			}
		}
		*section.count = uint32(len(tokens))
	}
	return catalog, nil
}

func (r *Registry) SetAura(actorID string, mark, frame uint32, catalog Catalog, now time.Time) error {
	guild, actor, found := r.GuildForCharacter(actorID)
	if !found || actor.Grade != DefaultMasterGrade {
		return ErrPermissionDenied
	}
	if mark >= catalog.AuraMarks || frame >= catalog.AuraFrames {
		return ErrInvalidContent
	}
	guild.AuraMark, guild.AuraFrame, guild.UpdatedAt = mark, frame, now
	return nil
}

// Purchase runs inside the caller's character/registry transaction. Inventory
// delivery (type 20) must succeed before that transaction commits.
func (r *Registry) Purchase(character *dnfrepo.CharacterRecord, content Content, now time.Time) (*Guild, error) {
	if character == nil {
		return nil, ErrCharacterNotFound
	}
	guild, actor, found := r.GuildForCharacter(character.CharacterID)
	if !found || actor.Grade != DefaultMasterGrade {
		return nil, ErrPermissionDenied
	}
	if content.ID == 0 || content.Price == 0 {
		return nil, ErrInvalidContent
	}
	if content.Gold {
		if character.Stats["gold"] < int64(content.Price) {
			return nil, ErrInsufficientGold
		}
	} else if guild.Mileage < content.Price {
		return nil, ErrInsufficientMileage
	}
	// Type 7 is a shared counted benefit -- its Value is a remaining-use counter --
	// so buying more must top it up instead of being refused as "already active".
	// Type 4 is an increment rather than an absolute capacity target, for the same
	// reason. Upstream exempts exactly these two from the active check.
	if content.Type != 7 && content.Type != 4 {
		if existing, exists := guild.Benefits[content.Type]; exists && existing.Active(now) {
			return nil, ErrContentActive
		}
	}
	switch content.Type {
	case 6:
		if content.Value <= int32(guild.MemberLimit) {
			return nil, ErrContentActive
		}
		guild.MemberLimit = int(content.Value)
	case 4:
		// 公会金库扩容. PVF describes these products as "expand by 8/16 slots", so
		// Value is an increment, not an absolute target: without this branch the
		// cargo limit could never leave DefaultCargoLimit and the ported 金库 would
		// be permanently stuck at its starting size.
		if content.Value <= 0 || guild.CargoLimit >= MaxCargoLimit || int(content.Value) > MaxCargoLimit-guild.CargoLimit {
			return nil, ErrCargoLimitReached
		}
		guild.CargoLimit += int(content.Value)
	case 1, 2, 3, 5:
		// 公会 BUFF 一类按时效生效的权益. Days 0 is meaningless, -1 means "does not
		// expire" and Value -1 means "disabled", which is why both are compared
		// against -1 rather than 0. Types 3 and 5 are the Guild_Status rows the HUD
		// projection reads, so without this branch that projection can never light
		// up: Purchase is the only writer of Benefits[1/2/3/5] in this tree.
		if content.Days == 0 || content.Days < -1 || content.Value < -1 {
			return nil, ErrInvalidContent
		}
		if guild.Benefits == nil {
			guild.Benefits = make(map[byte]Benefit)
		}
		expiresAt := time.Time{}
		if content.Days > 0 {
			expiresAt = now.Add(time.Duration(content.Days) * 24 * time.Hour)
		}
		guild.Benefits[content.Type] = Benefit{
			Type:      content.Type,
			Value:     uint32(content.Value),
			ItemID:    content.ItemID,
			ExpiresAt: expiresAt,
		}
	case 7:
		// 公会传送次数. Type 7 is a shared counted benefit and its PVF item is only a
		// shop icon, so the purchase tops up the guild-wide counter that
		// Registry.UseTeleport later spends. Without this branch no guild can ever
		// hold a use, and because op434/435 now requires one (decision D4) party
		// teleport would be refused for everyone, permanently.
		if content.Value <= 0 || content.Days <= 0 || !content.Counted {
			return nil, ErrInvalidContent
		}
		if err := guild.AddTeleportUses(uint32(content.Value), content, now); err != nil {
			return nil, err
		}
	case 20:
		// The PVF item is the actual rename approval delivered to the buyer.
	case 21:
		if content.Value < int32(len(guild.Members)) || content.Days <= 0 {
			return nil, ErrMemberLimit
		}
		if guild.Benefits == nil {
			guild.Benefits = make(map[byte]Benefit)
		}
		guild.Benefits[content.Type] = Benefit{Type: content.Type, Value: uint32(content.Value), ExpiresAt: now.Add(time.Duration(content.Days) * 24 * time.Hour), Claimed: make(map[string]bool)}
	default:
		// Other content types require their gameplay owner before charging.
		return nil, ErrInvalidContent
	}
	if content.Gold {
		character.Stats["gold"] -= int64(content.Price)
		character.UpdatedAt = now
	} else {
		guild.Mileage -= content.Price
	}
	guild.UpdatedAt = now
	return guild, nil
}

func (r *Registry) ClaimContract(characterID string, now time.Time) (*Guild, error) {
	guild, _, found := r.GuildForCharacter(characterID)
	if !found {
		return nil, ErrPermissionDenied
	}
	benefit, found := guild.Benefits[21]
	if !found || !benefit.Active(now) || benefit.Claimed[characterID] || uint32(len(benefit.Claimed)) >= benefit.Value {
		return nil, ErrContractClaim
	}
	if benefit.Claimed == nil {
		benefit.Claimed = make(map[string]bool)
	}
	benefit.Claimed[characterID] = true
	guild.Benefits[21], guild.UpdatedAt = benefit, now
	return guild, nil
}

// AddTeleportUses tops up the 公会传送 (type 7) benefit. Value is a remaining-use
// counter, which is why Benefit.Active treats a depleted type-7 row as inactive.
// Ported from the upstream guild branch; a purchase that carries the teleport
// content is what calls it.
func (g *Guild) AddTeleportUses(count uint32, content Content, now time.Time) error {
	if count == 0 || content.Type != 7 || content.Days <= 0 || content.Days > 36500 || !content.Counted {
		return ErrInvalidContent
	}
	benefit := g.Benefits[7]
	if !benefit.Active(now) {
		benefit = Benefit{}
	}
	if count > math.MaxUint32-benefit.Value {
		return ErrInvalidContent
	}
	benefit.Type, benefit.Mode, benefit.ItemID = 7, 1, content.ItemID
	benefit.Value += count
	if expires := now.Add(time.Duration(content.Days) * 24 * time.Hour); expires.After(benefit.ExpiresAt) {
		benefit.ExpiresAt = expires
	}
	if g.Benefits == nil {
		g.Benefits = make(map[byte]Benefit)
	}
	g.Benefits[7], g.UpdatedAt = benefit, now
	return nil
}

// UseTeleport spends one 公会传送 use. The caller must already hold the guild
// mutation lock; the guild id is re-checked so a member who changed guild between
// the request and the commit cannot spend the new guild's uses.
func (r *Registry) UseTeleport(characterID string, guildID uint32, now time.Time) error {
	guild, _, found := r.GuildForCharacter(characterID)
	if !found || guild.ID != guildID {
		return ErrPermissionDenied
	}
	benefit, found := guild.Benefits[7]
	if !found || !benefit.Active(now) || benefit.Value == 0 {
		return ErrInvalidContent
	}
	benefit.Value--
	guild.Benefits[7], guild.UpdatedAt = benefit, now
	return nil
}
