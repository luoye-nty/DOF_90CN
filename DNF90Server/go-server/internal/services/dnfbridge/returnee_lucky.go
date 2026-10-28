package dnfbridge

import (
	"context"
	"encoding/binary"
	"fmt"
	"strconv"
	"strings"
	"time"

	dnfguild "longheng.io/server/internal/modules/dnf/guild"
	dnfonlineevent "longheng.io/server/internal/modules/dnf/onlineevent"
	dnfpvf "longheng.io/server/internal/modules/dnf/pvf"
	dnfrepo "longheng.io/server/internal/modules/dnf/repository"
)

const currentReturneeStateID uint16 = 1370 // sub_99E480: push 55Ah
const currentReturneeTimeID uint16 = 304   // sub_1D80E30: u32 seconds, u8 legacy reward flag; time slot 49
const currentReturneeGuideID uint16 = 1431
const currentReturneeClaimID uint16 = 1432
const currentLuckyCharacterStateID uint16 = 1054 // sub_180AF71: push 41Eh

type currentLuckyCatalog struct {
	Jobs                         [][2]uint32
	Monster, Clear, Party, Quest int64
}

type currentReturneeProjection struct {
	Character       uint16
	Generation      uint64
	Body            [176]byte
	ActorGeneration uint64
	ExpiresAt       int64
}

func (s *Service) currentReturneeLuckyCatalogs(ctx context.Context) (*dnfonlineevent.ReturneeCatalog, *currentLuckyCatalog, error) {
	s.returneeLuckyMu.Lock()
	defer s.returneeLuckyMu.Unlock()
	if s.returneeCatalog != nil && s.luckyCharacterCatalog != nil {
		return s.returneeCatalog, s.luckyCharacterCatalog, nil
	}
	s.initialEquipmentMu.Lock()
	archive := s.initialEquipmentArchive
	s.initialEquipmentMu.Unlock()
	if archive == nil {
		return s.returneeCatalog, s.luckyCharacterCatalog, nil
	}
	if s.returneeCatalog == nil {
		c, err := dnfonlineevent.LoadReturneeCatalog(ctx, archive)
		if err != nil {
			return nil, nil, err
		}
		items, err := s.currentPVFItemCatalog()
		if err != nil {
			return nil, nil, err
		}
		for _, m := range c.Missions {
			if _, err := items.ResolveItem(uint32(m.Reward.ItemID)); err != nil {
				return nil, nil, err
			}
		}
		s.returneeCatalog = c
	}
	if s.luckyCharacterCatalog == nil {
		text, err := archive.ReadText("etc/chn_activatecharacter.etc")
		if err != nil {
			return nil, nil, err
		}
		c, err := parseCurrentLuckyCharacterCatalog(text)
		if err != nil {
			return nil, nil, err
		}
		s.luckyCharacterCatalog = c
	}
	return s.returneeCatalog, s.luckyCharacterCatalog, nil
}

func parseCurrentLuckyCharacterCatalog(text string) (*currentLuckyCatalog, error) {
	doc, err := dnfpvf.Parse("etc/chn_activatecharacter.etc", text)
	if err != nil {
		return nil, err
	}
	c := &currentLuckyCatalog{}
	tokens, ok := doc.Section("activate character schedule")
	if !ok || len(tokens) == 0 || len(tokens)%4 != 0 {
		return nil, fmt.Errorf("invalid lucky character schedule")
	}
	// The user explicitly enabled this test edition for all configured professions.
	// Zero PVF dates are not interpreted as an existing live schedule.
	for i := 0; i < len(tokens); i += 4 {
		job, grow := tokens[i+2], tokens[i+3]
		if job.Kind != dnfpvf.TokenInt || grow.Kind != dnfpvf.TokenInt || job.Int < 0 || job.Int > 14 || grow.Int < 0 || grow.Int > 127 {
			return nil, fmt.Errorf("invalid lucky profession")
		}
		c.Jobs = append(c.Jobs, [2]uint32{uint32(job.Int), uint32(grow.Int)})
	}
	for name, dst := range map[string]*int64{"monster kill exp buff ratio": &c.Monster, "dungeon clear exp buff ratio": &c.Clear, "party play buff ratio": &c.Party, "quest clear buff ratio": &c.Quest} {
		v, ok := doc.Section(name)
		if !ok || len(v) != 1 || v[0].Kind != dnfpvf.TokenInt || v[0].Int < 0 || v[0].Int > 1000 {
			return nil, fmt.Errorf("invalid lucky ratio %s", name)
		}
		*dst = v[0].Int
	}
	return c, nil
}

func currentLuckyCharacterMatches(c *currentLuckyCatalog, ch dnfrepo.CharacterRecord) bool {
	if c == nil || ch.CharacterID == "" || ch.Stats["is_hardcore_mode"] != 0 {
		return false
	}
	job, err := strconv.Atoi(ch.Job)
	if err != nil {
		return false
	}
	grow := ch.Stats["grow_type"]
	j, g, a := false, false, false
	for _, row := range c.Jobs {
		j = j || int(row[0]) == job
		g = g || int64(row[1]&15) == grow&15
		a = a || int64((row[1]>>4)&7) == (grow>>4)&7
	}
	return j && g && a
}

func (s *Service) currentLuckyCharacterEffect(session *gameSession, ch dnfrepo.CharacterRecord) *currentLuckyCatalog {
	if s == nil || session == nil || currentLuckyCharacterPVP(session) {
		return nil
	}
	s.returneeLuckyMu.Lock()
	c := s.luckyCharacterCatalog
	s.returneeLuckyMu.Unlock()
	if !currentLuckyCharacterMatches(c, ch) {
		return nil
	}
	return c
}

func currentLuckyCharacterPVP(session *gameSession) bool {
	if isPVPChannel(session.channel) {
		return true
	}
	switch session.channel.Type {
	case 4, 5, 7, 8, 9, 10, 13, 14, 20, 24, 31, 34, 35, 41:
		return true
	}
	return false
}

func (s *Service) currentReturneeWelcomeExperienceBonus(ctx context.Context, session *gameSession, character dnfrepo.CharacterRecord, base uint32) (uint32, error) {
	if s == nil || session == nil || base == 0 || currentLuckyCharacterPVP(session) {
		return 0, nil
	}
	accountID := s.accountIDForSession(session)
	if strings.TrimSpace(character.AccountID) != accountID || character.CharacterID != strconv.Itoa(int(session.selectedCharacterID)) {
		return 0, fmt.Errorf("returnee experience character ownership mismatch")
	}
	repos, ok := s.repositoryGroup()
	if !ok || repos.Account == nil {
		return 0, nil
	}
	account, found, err := repos.Account.Load(ctx, accountID)
	if err != nil || !found {
		return 0, err
	}
	// Read the durable account qualification, not a stale B+ flag or a
	// party leader's identity. All characters use the existing account period.
	state, err := dnfonlineevent.LoadReturneeState(account)
	if err != nil || !state.ActiveAt(s.gameplayNow()) {
		return 0, err
	}
	return currentPercentageBonusUint32(base, dnfonlineevent.ReturneeWelcomeExperiencePercent), nil
}

func buildCurrentLuckyCharacterBody(c *currentLuckyCatalog) []byte {
	var w packetWriter
	if c == nil {
		w.writeUint32(0)
		return w.bytes()
	}
	w.writeUint32(uint32(len(c.Jobs)))
	for _, row := range c.Jobs {
		w.writeUint32(row[0])
		w.writeUint32(row[1])
	}
	return w.bytes()
}

func buildCurrentReturneeBodyAt(c *dnfonlineevent.ReturneeCatalog, state dnfonlineevent.ReturneeState, now time.Time) [176]byte {
	var b [176]byte
	// Native values 1..5 select the returnee group; 0 selects the new-user group.
	binary.LittleEndian.PutUint16(b[:], 4)
	binary.LittleEndian.PutUint64(b[8:], state.Guide)
	if !state.ActiveAt(now) {
		return b
	}
	for i, m := range c.Missions {
		o := 16 + i*16
		binary.LittleEndian.PutUint32(b[o:], m.ID)
		binary.LittleEndian.PutUint32(b[o+4:], state.Progress[i])
		if state.Progress[i] >= m.Required {
			b[o+12] = 1
		}
		if state.Claimed&(1<<i) != 0 {
			b[o+13] = 1
		}
	}
	return b
}

func buildCurrentReturneeBody(c *dnfonlineevent.ReturneeCatalog, state dnfonlineevent.ReturneeState) [176]byte {
	return buildCurrentReturneeBodyAt(c, state, time.Now())
}

func (s *Service) settleCurrentReturnee(session *gameSession, login bool, claim uint32, guide *uint32) (dnfonlineevent.ReturneeState, []string, error) {
	ctx, cancel := context.WithTimeout(context.Background(), currentSpendTimeWriteTimeout)
	defer cancel()
	c, _, err := s.currentReturneeLuckyCatalogs(ctx)
	if err != nil {
		return dnfonlineevent.ReturneeState{}, nil, err
	}
	if c == nil {
		if claim != 0 || guide != nil {
			return dnfonlineevent.ReturneeState{}, nil, dnfonlineevent.ErrOwnerUnavailable
		}
		return dnfonlineevent.ReturneeState{}, nil, nil
	}
	repos, ok := s.repositoryGroup()
	if !ok {
		return dnfonlineevent.ReturneeState{}, nil, dnfonlineevent.ErrOwnerUnavailable
	}
	characterID := session.selectedCharacterID
	if characterID == 0 {
		characterID = session.returneePreparedCharacter
	}
	charID := strconv.Itoa(int(characterID))
	o := dnfonlineevent.ReturneeObservation{}
	if repos.Settings != nil {
		registry, err := dnfguild.Load(ctx, repos.Settings)
		if err != nil {
			return dnfonlineevent.ReturneeState{}, nil, err
		}
		_, _, o.Guild = registry.GuildForCharacter(charID)
	}
	now := s.gameplayNow()
	return dnfonlineevent.SettleReturnee(ctx, repos, c, dnfonlineevent.ReturneeCommand{
		AccountID: s.accountIDForSession(session), CharacterID: charID, Now: now, Login: login, Claim: claim, Guide: guide, Observation: o,
		Deliver: func(mailboxes dnfrepo.MailboxRepository, r dnfonlineevent.AttendancePVFItemReward, key string) (string, error) {
			items, err := s.currentPVFItemCatalog()
			if err != nil {
				return "", err
			}
			definition, err := items.ResolveItem(uint32(r.ItemID))
			if err != nil {
				return "", err
			}
			definition, err = currentPVFItemDefinitionForGrantAt(definition, now)
			if err != nil {
				return "", err
			}
			if definition.Kind != dungeonDropItemStackable && definition.Kind != dungeonDropItemEquipment {
				return "", fmt.Errorf("returnee reward %d unsupported", r.ItemID)
			}
			stack := dnfrepo.ItemStack{ItemID: r.ItemID, Count: r.Count, Extra: map[string]string{"item_kind": string(definition.Kind), "equipment_type": definition.EquipmentType, "durability": strconv.Itoa(int(definition.Durability)), "max_durability": strconv.Itoa(int(definition.Durability))}}
			stack, _ = applyCurrentPVFItemExpirationAt(stack, definition, now)
			return dnfrepo.AppendSystemMail(ctx, mailboxes, dnfrepo.SystemMailDelivery{RecipientCharacterID: charID, Title: c.MailTitle, Body: c.MailBody, Source: "seria_returnee", CompletionKey: key, CreatedAt: now, Attachments: []dnfrepo.MailAttachment{{ItemID: r.ItemID, Count: r.Count, ExpireAt: stack.ExpireAt, RawEntry: stack.RawEntry, Extra: stack.Extra}}})
		},
	})
}

func (s *Service) prepareCurrentReturnee(session *gameSession) error {
	if session == nil || session.selectedCharacterID == 0 || (session.returneePreparedCharacter == session.selectedCharacterID && session.returneePreparedGeneration == session.characterGeneration) {
		return nil
	}
	state, _, err := s.settleCurrentReturnee(session, !session.returneeConnectionObserved, 0, nil)
	if err != nil {
		return err
	}
	session.returneeConnectionObserved = true
	session.returneePreparedCharacter, session.returneePreparedGeneration = session.selectedCharacterID, session.characterGeneration
	session.returneeActive = state.ActiveAt(s.gameplayNow())
	if session.returneeActive {
		session.returneePreparedExpiresAt = state.ExpiresAt.Unix()
	} else {
		session.returneePreparedExpiresAt = 0
	}
	return nil
}

func (s *Service) saveCurrentReturneeLastSeen(session *gameSession) {
	if session == nil || session.returneePreparedCharacter == 0 {
		return
	}
	if _, _, err := s.settleCurrentReturnee(session, false, 0, nil); err != nil {
		s.logGameEvent(session, "game-returnee-logout-save-failed", "error", err)
	}
}

func (s *Service) sendCurrentReturneeState(session *gameSession, force bool) error {
	if session == nil || session.selectedCharacterID == 0 {
		return nil
	}
	state, _, err := s.settleCurrentReturnee(session, false, 0, nil)
	if err != nil {
		return err
	}
	s.returneeLuckyMu.Lock()
	c := s.returneeCatalog
	s.returneeLuckyMu.Unlock()
	if c == nil {
		return nil
	}
	now := s.gameplayNow()
	session.returneeActive = state.ActiveAt(now)
	p := currentReturneeProjection{
		Character:       session.selectedCharacterID,
		Generation:      session.characterGeneration,
		Body:            buildCurrentReturneeBodyAt(c, state, now),
		ActorGeneration: session.sceneBootstrapActorGeneration,
	}
	if session.returneeActive {
		p.ExpiresAt = state.ExpiresAt.Unix()
	}
	// The op304 timer lives in the global premium manager, across actor rebuilds.
	// A positive value also calls sub_2102530 and restarts ALL contract notices.
	// Sync only on character selection or a real period change; scene transitions
	// restore the actor/mission state through their existing packets.
	sameTimeOwner := p.Character == session.returneeTimeCharacter && p.Generation == session.returneeTimeGeneration
	timeChanged := !sameTimeOwner || p.ExpiresAt != session.returneeTimeExpiresAt
	if timeChanged && (p.ExpiresAt != 0 || (session.returneeTimeExpiresAt != 0 && sameTimeOwner)) {
		var body [5]byte
		if session.returneeActive {
			seconds := (state.ExpiresAt.Sub(now) + time.Second - 1) / time.Second
			binary.LittleEndian.PutUint32(body[:], uint32(seconds))
		}
		// Legacy return-gift availability is separate from the 1370 missions.
		if err := s.sendGameUpperRawClass(session, currentReturneeTimeID, body[:], 0); err != nil {
			return err
		}
	}
	// Commit the timer separately so a failed 1370 retry cannot replay notices.
	session.returneeTimeCharacter, session.returneeTimeGeneration = p.Character, p.Generation
	session.returneeTimeExpiresAt = p.ExpiresAt
	if force || p != session.returneeProjection {
		if err := s.sendGameUpperRawClass(session, currentReturneeStateID, p.Body[:], 0); err != nil {
			return err
		}
		session.returneeProjection = p
		s.logGameEvent(session, "game-returnee-state-sent", "active", session.returneeActive,
			"expires_at", state.ExpiresAt, "claimed", state.Claimed, "time_changed", timeChanged)
	}
	return nil
}

func (s *Service) sendCurrentLuckyCharacterState(session *gameSession) error {
	if session == nil || session.selectedCharacterID == 0 || (session.luckyPreparedCharacter == session.selectedCharacterID && session.luckyPreparedGeneration == session.characterGeneration && session.luckyPreparedActorGeneration == session.sceneBootstrapActorGeneration) {
		return nil
	}
	_, c, err := s.currentReturneeLuckyCatalogs(context.Background())
	if err != nil || c == nil {
		return err
	}
	if currentLuckyCharacterPVP(session) {
		c = nil
	}
	if err := s.sendGameUpperRawClass(session, currentLuckyCharacterStateID, buildCurrentLuckyCharacterBody(c), 0); err != nil {
		return err
	}
	session.luckyPreparedCharacter, session.luckyPreparedGeneration = session.selectedCharacterID, session.characterGeneration
	session.luckyPreparedActorGeneration = session.sceneBootstrapActorGeneration
	return nil
}

func (s *Service) handleCurrentReturneeCommand(session *gameSession, opcode uint16, body []byte) error {
	if session == nil || session.selectedCharacterID == 0 || len(body) != 4 {
		return s.sendGameUpperRawClass(session, opcode, []byte{0, 3}, 1)
	}
	id := binary.LittleEndian.Uint32(body)
	claim := id
	var guide *uint32
	if opcode == currentReturneeClaimID && id == 0 {
		return s.sendGameUpperRawClass(session, opcode, []byte{0, 3}, 1)
	}
	if opcode == currentReturneeGuideID {
		if id >= 41 {
			return s.sendGameUpperRawClass(session, opcode, []byte{0, 3}, 1)
		}
		claim = 0
		guide = &id
	}
	_, mails, err := s.settleCurrentReturnee(session, false, claim, guide)
	if err != nil {
		s.logGameEvent(session, "game-returnee-command-rejected", "opcode", opcode, "mission", id, "error", err)
		return s.sendGameUpperRawClass(session, opcode, []byte{0, 22}, 1)
	}
	if err := s.sendGameUpperRawClass(session, opcode, []byte{1}, 1); err != nil {
		return err
	}
	if err := s.sendCurrentReturneeState(session, true); err != nil {
		return err
	}
	if len(mails) > 0 {
		return s.sendMailboxAlarmForSession(session, session.selectedCharacterID, "seria_returnee")
	}
	return nil
}
