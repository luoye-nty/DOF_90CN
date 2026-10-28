package dnfbridge

import (
	"context"
	"sort"

	dnfrepo "longheng.io/server/internal/modules/dnf/repository"
)

// currentUserInfoSkillPageEntry is one {u16 skill id, u8 level} row of the
// mode1/mode3 skill groups.
type currentUserInfoSkillPageEntry struct {
	SkillID uint16
	Level   byte
}

// currentUserInfoSkillPages is the tail block sub_2003420 reads right after
// the equipped-creature {item id, serial} pair: a u8 selected tree (0xff keeps
// the client's current selection), then for tree 0 and tree 1 a u8 count
// followed by {u16 skill, u8 level} rows.
//
// The current EXE applies these rows only to actors other than the controlled
// one (0x0200342B `cmp [controlledActor], esi`): per row it selects the tree
// (sub_249FF80), resolves the skill object (vtbl+0xBB0), sets its rank through
// sub_2666AE0 and stores the level (vtbl+0xB60); afterwards a non-0xff selected
// byte switches the actor's active tree. For the controlled actor the same
// bytes are consumed by sub_2002F10 with a null target and discarded, so the
// owner-produced row can be replayed to peers byte for byte.
//
// These two groups are the only channel through which a client learns a
// *party member's* skill levels: op19 (sub_1D6E240) binds exclusively to the
// controlled actor and the reliable-UDP handshake carries no skill table.
// Publishing empty groups - what every peer received until 2026-09-07 - left
// every remote actor with a zero skill table. A party buff such as 武器祝福 is
// simulated locally by each client from the caster's skill state (the P2P
// k02s19 cast message carries skill 7 and the caster's level 22, decoded live),
// so on every teammate it resolved to level 0 while the caster saw its own
// level. The S4A12 reference server writes the same layout after
// SkillTreeIndex in its subtype-1 body (UserInfoSubtype1Builder.WriteSkillPage).
type currentUserInfoSkillPages struct {
	Selected byte
	Pages    [currentSkillInfoTreeCount][]currentUserInfoSkillPageEntry
}

func emptyCurrentUserInfoSkillPages() currentUserInfoSkillPages {
	return currentUserInfoSkillPages{Selected: 0xff}
}

func (r *csharpLegacyUserInfoReader) currentUserInfoSkillPages(character dnfrepo.CharacterRecord) currentUserInfoSkillPages {
	pages := emptyCurrentUserInfoSkillPages()
	if r == nil || r.service == nil || character.CharacterID == "" {
		return pages
	}
	repos, ok := r.service.projectionRepositories(r.ctx)
	if !ok || repos.Skill == nil {
		return pages
	}
	job, ok := characterJobByte(character)
	if !ok {
		return pages
	}
	ctx := r.ctx
	if ctx == nil {
		ctx = context.Background()
	}
	record, _, err := r.service.loadOrBackfillCurrentSceneSkillRecord(ctx, repos, character, job)
	if err != nil {
		r.service.logGameEvent(r.session, "game-upper-userinfo-mode1-skill-pages-deferred",
			"character_id", character.CharacterID,
			"error", err)
		return pages
	}
	pages = currentUserInfoSkillPagesFromRecord(character, record)

	// The durable skill record carries only the learned level. The BUFF
	// enhancement (换装) registration adds equipment level bonuses to its
	// registered buff skill on the caster's own client; fold that bonus into
	// the wire skill page so peers, which simulate the caster's buff from this
	// table, see the same level instead of the unregistered baseline.
	if r.session != nil {
		buffSkillID, bonus, bonusErr := r.service.skillSwitchRegisteredBuffLevelBonus(ctx, r.session, character)
		if bonusErr == nil && buffSkillID != 0 && bonus != 0 {
			for tree := 0; tree < currentSkillInfoTreeCount; tree++ {
				for index := range pages.Pages[tree] {
					if pages.Pages[tree][index].SkillID != buffSkillID {
						continue
					}
					level := int(pages.Pages[tree][index].Level) + bonus
					if level < 0 {
						level = 0
					}
					if level > 0xff {
						level = 0xff
					}
					pages.Pages[tree][index].Level = byte(level)
				}
			}
		}
	}
	return pages
}

// currentUserInfoSkillPagesFromRecord projects the durable skill record into
// the wire groups: every learned skill (level > 0) of each tree in ascending
// skill-id order, level clamped to the u8 the reader stores, and the
// character's active tree as the selected byte.
func currentUserInfoSkillPagesFromRecord(character dnfrepo.CharacterRecord, record dnfrepo.SkillRecord) currentUserInfoSkillPages {
	pages := emptyCurrentUserInfoSkillPages()
	for tree := 0; tree < currentSkillInfoTreeCount; tree++ {
		state, ok := dnfrepo.SkillTree(record, tree)
		if !ok {
			if tree != currentSkillInfoTreeIndex {
				continue
			}
			state = dnfrepo.SkillTreeState{Skills: record.Skills}
		}
		ids := make([]int64, 0, len(state.Skills))
		for id, skill := range state.Skills {
			if id <= 0 || id > 0xffff || skill.Level <= 0 {
				continue
			}
			ids = append(ids, id)
		}
		sort.Slice(ids, func(i, j int) bool { return ids[i] < ids[j] })
		if len(ids) > 0xff {
			ids = ids[:0xff]
		}
		rows := make([]currentUserInfoSkillPageEntry, 0, len(ids))
		for _, id := range ids {
			level := state.Skills[id].Level
			if level > 0xff {
				level = 0xff
			}
			rows = append(rows, currentUserInfoSkillPageEntry{SkillID: uint16(id), Level: byte(level)})
		}
		pages.Pages[tree] = rows
	}
	if active, err := currentSceneActiveSkillTree(character); err == nil && int(active) < currentSkillInfoTreeCount {
		pages.Selected = active
	}
	return pages
}

func writeCurrentUserInfoSkillPages(w *packetWriter, pages currentUserInfoSkillPages) {
	w.writeByte(pages.Selected)
	for _, rows := range pages.Pages {
		w.writeByte(byte(len(rows)))
		for _, row := range rows {
			w.writeUint16(row.SkillID)
			w.writeByte(row.Level)
		}
	}
}
