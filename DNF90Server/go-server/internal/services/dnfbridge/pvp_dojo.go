package dnfbridge

import (
	"context"
	"fmt"
	"sort"
	"strconv"

	dnfpvp "longheng.io/server/internal/modules/dnf/pvp"
	dnfrepo "longheng.io/server/internal/modules/dnf/repository"
	"longheng.io/server/internal/modules/dnf/skillcmd"
)

// Class0/1229 sub_1093540 reads one byte, replacing its initial -1 sentinel.
const currentPVPDojoSkillStateMsgID uint16 = 1229

func (s *Service) handleCurrentPVPDojoSkillMode(session *gameSession, body []byte) error {
	if session == nil || session.selectedCharacterID == 0 || len(body) != 1 || body[0] < 1 || body[0] > 2 {
		return nil
	}
	repos, ok := s.repositoryGroup()
	if !ok {
		return nil
	}
	ctx, cancel := context.WithTimeout(context.Background(), createWriteTimeout)
	defer cancel()
	if err := dnfpvp.SaveDojoSkillMode(ctx, repos, session.accountID,
		strconv.Itoa(int(session.selectedCharacterID)), body[0], s.gameplayNow()); err != nil {
		return err
	}
	return s.sendGameUpperRawClass(session, currentPVPDojoSkillStateMsgID, body, 0)
}

func (s *Service) sendCurrentPVPDojoSkillMode(session *gameSession) error {
	if session == nil || session.selectedCharacterID == 0 {
		return nil
	}
	ctx, cancel := context.WithTimeout(context.Background(), createWriteTimeout)
	defer cancel()
	repos, ok := s.repositoryGroup()
	if !ok || repos.Character == nil {
		return nil
	}
	character, found, err := repos.Character.Load(ctx, strconv.Itoa(int(session.selectedCharacterID)))
	if err != nil || !found {
		return err
	}
	if character.AccountID != session.accountID {
		return nil
	}
	return s.sendGameUpperRawClass(session, currentPVPDojoSkillStateMsgID, []byte{dnfpvp.DojoSkillMode(character)}, 0)
}

func (s *Service) currentPVPDojoSkillsActive(session *gameSession) (bool, error) {
	if session == nil || session.selectedCharacterID == 0 {
		return false, nil
	}
	session.townMu.Lock()
	position := session.townPositionSnapshot
	session.townMu.Unlock()
	if position.CharacterID != session.selectedCharacterID || !position.PositionValid || position.TownID != 10 || position.AreaID != 4 {
		return false, nil
	}
	repos, ok := s.repositoryGroup()
	if !ok || repos.Character == nil {
		return false, fmt.Errorf("dojo skill repository unavailable")
	}
	ctx, cancel := context.WithTimeout(context.Background(), createWriteTimeout)
	defer cancel()
	character, found, err := repos.Character.Load(ctx, strconv.Itoa(int(session.selectedCharacterID)))
	if err != nil {
		return false, err
	}
	if !found || character.AccountID != session.accountID {
		return false, fmt.Errorf("dojo skill character ownership mismatch")
	}
	return dnfpvp.DojoSkillMode(character) == 0, nil
}

// sub_1099650 registers class0/1230. sub_1097EB0 loops over two pages,
// each with u8 count and {slot:u8, skill:u16, level:u8} entries, followed
// by SP0, SP1, TP0, TP1 as four u16 values.
func buildCurrentPVPDojoSkills(record dnfrepo.SkillRecord) ([]byte, error) {
	var w packetWriter
	var points [2]dnfrepo.SkillPointState
	for tree := 0; tree < 2; tree++ {
		state, found := dnfrepo.SkillTree(record, tree)
		if !found {
			return nil, fmt.Errorf("dojo skill page %d missing", tree)
		}
		points[tree] = state.Points
		slots := make([]int, 0, len(record.Layouts[tree]))
		for slot, id := range record.Layouts[tree] {
			level := state.Skills[int64(id)].Level
			if slot < 0 || slot >= currentSkillSlotCount || level < 0 || level > 255 {
				return nil, fmt.Errorf("invalid dojo skill slot/level")
			}
			if level > 0 {
				slots = append(slots, slot)
			}
		}
		sort.Ints(slots)
		w.writeByte(byte(len(slots)))
		for _, slot := range slots {
			id := record.Layouts[tree][slot]
			w.writeByte(byte(slot))
			w.writeUint16(id)
			w.writeByte(byte(state.Skills[int64(id)].Level))
		}
	}
	for _, value := range []int{points[0].RemainingSP, points[1].RemainingSP, points[0].RemainingTP, points[1].RemainingTP} {
		if value < 0 || value > 65535 {
			return nil, fmt.Errorf("dojo skill points out of native range")
		}
		w.writeUint16(uint16(value))
	}
	return w.bytes(), nil
}

func (s *Service) sendCurrentPVPDojoSkillsBeforeTransition(ctx context.Context, session *gameSession, character dnfrepo.CharacterRecord) error {
	repos, ok := s.repositoryGroup()
	if !ok {
		return nil
	}
	record, err := skillcmd.PrepareDojoSkills(ctx, repos, character.CharacterID)
	if err != nil {
		return err
	}
	body, err := buildCurrentPVPDojoSkills(record)
	if err != nil {
		return err
	}
	if err := s.sendCurrentPVPDojoSkillMode(session); err != nil {
		return err
	}
	s.logGameEvent(session, "game-pvp-dojo-skills-initial", "char_id", session.selectedCharacterID, "body_len", len(body), "mode", dnfpvp.DojoSkillMode(character))
	return s.sendGameUpperRawClass(session, 1230, body, 0)
}
