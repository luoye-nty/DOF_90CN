package dnfbridge

import (
	"context"
	"encoding/binary"
	"encoding/hex"
	"errors"
	"fmt"
	"os"
	"strconv"
	"strings"
	"time"

	"longheng.io/server/internal/modules/dnf/dnfenum"
	dnfguild "longheng.io/server/internal/modules/dnf/guild"
	dnfrepo "longheng.io/server/internal/modules/dnf/repository"
)

const (
	currentGuildGenericError = byte(34)
	// currentGuildChannelSettingError is op830's refusal code. The client maps
	// 19 to string 31583 只有 "%s" 才能使用。 with %s filled from the grade-1 name,
	// which is the message the master saw while every ordinary channel was
	// rejected by the ResidentFor guard.
	currentGuildChannelSettingError = byte(19)
	// currentGuildImmediateJoinMode is the op740 join-mode value that the client
	// renders as 31592 立即加入; 0 renders as 31591 接受后加入. Live evidence
	// 2026-09-12: the settings window compares [row+0x3c] with 0 and 1 at
	// 00FC903A / 00FC9081, and op740 bodies carried mode 0 and mode 1 verbatim.
	currentGuildImmediateJoinMode    = byte(1)
	currentGuildInvalidNameError     = byte(106)
	currentGuildDuplicateNameError   = byte(107)
	currentGuildPromoterMissingError = byte(108)
	currentGuildCreateGoldError      = byte(31)
	currentGuildCreateStateError     = byte(104)
	// The current EXE gives op741 its own failure table (NoPack sub_1D0D250:
	// `code - 3` indexes the byte table at 0x1D0D318, then a nine-entry jump
	// table at 0x1D0D2F0). Only codes 3, 5, 23, 34, 39, 40, 45, 77 and 104
	// render a message there; every other code closes the create window in
	// silence. Code 104 renders dstr 40150 — the "wait for the 06:00 daily
	// reset" text — a rule this server does not implement, so an op741
	// precondition failure must not reuse currentGuildCreateStateError.
	// Code 34 renders dstr 19487, the same shared guild-failure text the
	// class-0 guild notifier at sub_1D2F238 renders for 34.
	currentGuildCreatePermitReject    = currentGuildGenericError
	currentGuildCreatePermitStale     = byte(0)
	currentGuildCreateInviteOpcode    = uint16(685)
	currentGuildCreateAuthStateOpcode = uint16(686)
	currentGuildCreateCancelOpcode    = uint16(687)
	currentGuildCreateAuthResetValue  = byte(2)
	currentGuildApplicationCancelable = byte(0)
)

type currentGuildCreateInvitation struct {
	creator  boundGameSessionCharacter
	target   boundGameSessionCharacter
	accepted bool
	timer    *time.Timer
}

// All invitation transitions, including their notifications, hold guildCreateMu.
func (s *Service) clearCurrentGuildCreateInvitation(invite *currentGuildCreateInvitation) {
	if invite.timer != nil {
		invite.timer.Stop()
	}
	for _, identity := range []boundGameSessionCharacter{invite.creator, invite.target} {
		if identity.session.guild.createInvitation == invite {
			identity.session.guild.createInvitation = nil
		}
	}
}

func (s *Service) cancelCurrentGuildCreateInvitation(invite *currentGuildCreateInvitation, result byte, notifyTarget bool) error {
	s.clearCurrentGuildCreateInvitation(invite)
	var err error
	if s.boundGameSessionCharacterCurrent(invite.creator) {
		err = s.sendGameUpperSecondaryRaw(invite.creator.session, currentGuildCreateAuthStateOpcode, []byte{result})
	}
	if notifyTarget && s.boundGameSessionCharacterCurrent(invite.target) {
		err = errors.Join(err, s.sendGameUpperSecondaryRaw(invite.target.session, currentGuildCreateCancelOpcode, nil))
	}
	return err
}

func (s *Service) closeCurrentGuildCreateSession(session *gameSession) {
	s.guildCreateMu.Lock()
	defer s.guildCreateMu.Unlock()
	if invite := session.guild.createInvitation; invite != nil {
		if err := s.cancelCurrentGuildCreateInvitation(invite, currentGuildCreateAuthResetValue, true); err != nil {
			s.logGameEvent(session, "game-guild-create-cancel-notify-failed", "error", err)
		}
	}
	session.guild.mu.Lock()
	session.guild.pendingName = ""
	session.guild.nameChecked = false
	session.guild.promotion = ""
	session.guild.createCommitted = false
	session.guild.mu.Unlock()
}

func (s *Service) expireCurrentGuildCreateInvitation(invite *currentGuildCreateInvitation) {
	s.guildCreateMu.Lock()
	defer s.guildCreateMu.Unlock()
	if invite.creator.session.guild.createInvitation == invite && !invite.accepted {
		if err := s.cancelCurrentGuildCreateInvitation(invite, currentGuildCreateAuthResetValue, true); err != nil {
			s.logGameEvent(invite.creator.session, "game-guild-create-expiry-notify-failed", "error", err)
		}
	}
}

func (s *Service) currentGuildContext() (context.Context, context.CancelFunc) {
	return context.WithTimeout(context.Background(), createWriteTimeout)
}

func (s *Service) loadCurrentGuildRegistry(ctx context.Context) (dnfrepo.Group, dnfguild.Registry, error) {
	repositories, ok := s.repositoryGroup()
	if !ok || repositories.Settings == nil || repositories.Character == nil {
		return dnfrepo.Group{}, dnfguild.Registry{}, dnfguild.ErrRepositoryUnavailable
	}
	registry, err := dnfguild.Load(ctx, repositories.Settings)
	return repositories, registry, err
}

func (s *Service) mutateCurrentGuild(
	ctx context.Context,
	apply func(dnfrepo.Group, *dnfguild.Registry) error,
) error {
	s.guildMu.Lock()
	defer s.guildMu.Unlock()
	repositories, registry, err := s.loadCurrentGuildRegistry(ctx)
	if err != nil {
		return err
	}
	if err := apply(repositories, &registry); err != nil {
		return err
	}
	return dnfguild.Save(ctx, repositories.Settings, registry, s.gameplayNow())
}

func (s *Service) readCurrentGuild(
	ctx context.Context,
	read func(dnfrepo.Group, dnfguild.Registry) error,
) error {
	s.guildMu.Lock()
	defer s.guildMu.Unlock()
	repositories, registry, err := s.loadCurrentGuildRegistry(ctx)
	if err != nil {
		return err
	}
	return read(repositories, registry)
}

// readCurrentGuildNonBlocking is readCurrentGuild for display-only projections
// that may be reached from inside a guild critical section.
//
// guildMu is a plain, non-reentrant mutex, and this tree has already been frozen
// once by exactly this shape of bug: on 2026-09-22 20:06 sessions 1111 and 2222,
// in the same guild, blocked on each other's locks for six minutes (see
// sendCurrentGuildManagementStateToPeer, which was rewritten to TryLock for that
// reason). A missed ranking refresh costs nothing -- the next op70/op140 resend
// carries it -- while a blocked one freezes the session, so busy means "skip
// this time" and the caller decides how to report it.
func (s *Service) readCurrentGuildNonBlocking(
	ctx context.Context,
	read func(dnfrepo.Group, dnfguild.Registry) error,
) (bool, error) {
	if !s.guildMu.TryLock() {
		return false, nil
	}
	defer s.guildMu.Unlock()
	repositories, registry, err := s.loadCurrentGuildRegistry(ctx)
	if err != nil {
		return false, err
	}
	return true, read(repositories, registry)
}

func selectedGuildCharacter(ctx context.Context, repositories dnfrepo.Group, session *gameSession) (dnfrepo.CharacterRecord, error) {
	if session == nil || session.selectedCharacterID == 0 || repositories.Character == nil {
		return dnfrepo.CharacterRecord{}, dnfguild.ErrCharacterNotFound
	}
	characterID := strconv.Itoa(int(session.selectedCharacterID))
	character, found, err := repositories.Character.Load(ctx, characterID)
	if err != nil {
		return dnfrepo.CharacterRecord{}, err
	}
	if !found || character.CharacterID == "" {
		return dnfrepo.CharacterRecord{}, dnfguild.ErrCharacterNotFound
	}
	return character, nil
}

func (s *Service) currentGuildOwnedSelectedCharacter(session *gameSession) (dnfrepo.CharacterRecord, error) {
	ctx, cancel := s.currentGuildContext()
	defer cancel()
	repositories, ok := s.repositoryGroup()
	if !ok || repositories.Character == nil {
		return dnfrepo.CharacterRecord{}, dnfguild.ErrRepositoryUnavailable
	}
	character, err := selectedGuildCharacter(ctx, repositories, session)
	if err != nil {
		return dnfrepo.CharacterRecord{}, err
	}
	if character.AccountID != s.accountIDForSession(session) {
		return dnfrepo.CharacterRecord{}, dnfguild.ErrCharacterNotFound
	}
	return character, nil
}

func currentGuildTargetCharacterID(body []byte) (string, bool) {
	if len(body) >= 4 {
		value := binary.LittleEndian.Uint32(body[len(body)-4:])
		if value != 0 {
			return strconv.FormatUint(uint64(value), 10), true
		}
	}
	return "", false
}

func currentGuildDSTRAtStart(body []byte) (string, bool) {
	value, next, ok := parseCurrentGuildDSTR(body, 0)
	return value, ok && next == len(body) && value != ""
}

func (s *Service) handleCurrentGuildNameCheck(session *gameSession, body []byte) error {
	s.guildCreateMu.Lock()
	defer s.guildCreateMu.Unlock()
	if invite := session.guild.createInvitation; invite != nil {
		if err := s.cancelCurrentGuildCreateInvitation(invite, currentGuildCreateAuthResetValue, true); err != nil {
			return err
		}
	}
	session.guild.mu.Lock()
	session.guild.nameChecked = false
	session.guild.mu.Unlock()
	name, ok := currentGuildDSTRAtStart(body)
	if !ok || dnfguild.ValidateName(name) != nil {
		return s.sendGameUpperFailure(session, uint16(dnfenum.CmdPacketCheckGuildNameDouble), currentGuildInvalidNameError)
	}
	ctx, cancel := s.currentGuildContext()
	defer cancel()
	available := false
	err := s.readCurrentGuild(ctx, func(_ dnfrepo.Group, registry dnfguild.Registry) error {
		available = registry.NameAvailable(name)
		return nil
	})
	if err != nil {
		return s.sendGameUpperFailure(session, uint16(dnfenum.CmdPacketCheckGuildNameDouble), currentGuildGenericError)
	}
	if !available {
		return s.sendGameUpperFailure(session, uint16(dnfenum.CmdPacketCheckGuildNameDouble), currentGuildDuplicateNameError)
	}
	session.guild.mu.Lock()
	session.guild.pendingName = name
	session.guild.nameChecked = true
	session.guild.promotion = ""
	session.guild.createCommitted = false
	session.guild.mu.Unlock()
	if err := s.sendGameUpperSuccess(session, uint16(dnfenum.CmdPacketCheckGuildNameDouble), nil); err != nil {
		return err
	}
	// Class 0/op686: wire 2 resets the controller; wire 0 means rejected.
	return s.sendGameUpperSecondaryRaw(session, currentGuildCreateAuthStateOpcode, []byte{currentGuildCreateAuthResetValue})
}

func (s *Service) handleCurrentGuildPromoterCheck(session *gameSession, body []byte) error {
	s.guildCreateMu.Lock()
	defer s.guildCreateMu.Unlock()
	promotion, ok := currentGuildDSTRAtStart(body)
	if !ok || dnfguild.ValidatePromotion(promotion) != nil {
		return s.sendGameUpperFailure(session, uint16(dnfenum.CmdPacketCheckGuildCreatePromoteMsg), currentGuildPromoterMissingError)
	}
	_, err := s.currentGuildOwnedSelectedCharacter(session)
	if err != nil {
		// op739's callback (sub_1D0D1A0) only renders a message for code 108;
		// a state rejection closes the window silently, so log the reason.
		s.logGameEvent(session, "game-guild-promoter-check-rejected", "reason", "selected character", "error", err)
		return s.sendGameUpperFailure(session, uint16(dnfenum.CmdPacketCheckGuildCreatePromoteMsg), currentGuildCreateStateError)
	}
	session.guild.mu.Lock()
	valid := session.guild.nameChecked && session.guild.createInvitation == nil
	if valid {
		session.guild.promotion = promotion
	}
	session.guild.mu.Unlock()
	if !valid {
		s.logGameEvent(session, "game-guild-promoter-check-rejected", "reason", "guild name not checked or invitation pending")
		return s.sendGameUpperFailure(session, uint16(dnfenum.CmdPacketCheckGuildCreatePromoteMsg), currentGuildCreateStateError)
	}
	// The native success callback sends op741 with the region and target name.
	return s.sendGameUpperSuccess(session, uint16(dnfenum.CmdPacketCheckGuildCreatePromoteMsg), nil)
}

// rejectCurrentGuildCreatePermit answers op741 with the shared guild-failure
// code and records which precondition refused the request. Without the log
// line every rejection looks identical on the wire (`00 22`), so the reason
// has to come from the server side.
func (s *Service) rejectCurrentGuildCreatePermit(session *gameSession, founder string, reason string, cause error) error {
	fields := []any{"reason", reason}
	if founder != "" {
		fields = append(fields, "founder", founder)
	}
	if cause != nil {
		fields = append(fields, "error", cause)
	}
	s.logGameEvent(session, "game-guild-create-permit-rejected", fields...)
	return s.sendGameUpperFailure(
		session,
		uint16(dnfenum.CmdPacketRequestGuildCreatePermit),
		currentGuildCreatePermitReject,
	)
}

func (s *Service) handleCurrentGuildCreatePermit(session *gameSession, body []byte) error {
	s.guildCreateMu.Lock()
	defer s.guildCreateMu.Unlock()
	session.guild.mu.Lock()
	committed := session.guild.createCommitted
	session.guild.mu.Unlock()
	if committed {
		// A late native callback must not put a committed creator back in wait.
		return s.sendGameUpperFailure(
			session,
			uint16(dnfenum.CmdPacketRequestGuildCreatePermit),
			currentGuildCreatePermitStale,
		)
	}
	if len(body) < 5 || int(body[0]) != s.channelAdvertiseID() || session.guild.createInvitation != nil {
		return s.rejectCurrentGuildCreatePermit(session, "", "request state", nil)
	}
	name, next, ok := parseCurrentGuildDSTR(body, 1)
	if !ok || next != len(body) || name == "" {
		return s.rejectCurrentGuildCreatePermit(session, "", "founder name", nil)
	}
	ctx, cancel := s.currentGuildContext()
	defer cancel()
	creator, online := s.boundGameSessionCharacterSnapshot(session)
	var target boundGameSessionCharacter
	var character dnfrepo.CharacterRecord
	err := s.readCurrentGuild(ctx, func(repositories dnfrepo.Group, registry dnfguild.Registry) error {
		var err error
		character, err = selectedGuildCharacter(ctx, repositories, session)
		if err != nil || !online || character.AccountID != s.accountIDForSession(session) {
			return dnfguild.ErrCharacterNotFound
		}
		id, found, err := repositories.Character.FindIDByName(ctx, name)
		if err != nil || !found || id == character.CharacterID {
			return dnfguild.ErrCharacterNotFound
		}
		numericID, err := strconv.ParseUint(id, 10, 16)
		if err != nil {
			return dnfguild.ErrCharacterNotFound
		}
		target, found = s.onlineGameSessionCharacterSnapshot(uint16(numericID))
		if !found || target.session.guild.createInvitation != nil {
			return dnfguild.ErrCharacterNotFound
		}
		promoter, found, err := repositories.Character.Load(ctx, id)
		if err != nil || !found || promoter.AccountID != s.accountIDForSession(target.session) {
			return dnfguild.ErrCharacterNotFound
		}
		for _, memberID := range []string{character.CharacterID, id} {
			if _, _, found := registry.GuildForCharacter(memberID); found {
				return dnfguild.ErrAlreadyMember
			}
		}
		return nil
	})
	session.guild.mu.Lock()
	guildName, promotion := session.guild.pendingName, session.guild.promotion
	nameChecked := session.guild.nameChecked
	session.guild.mu.Unlock()
	switch {
	case err != nil:
		// The named founder must resolve to another character that is online
		// right now, owned by the session it is bound to, and not already in a
		// guild. A single client therefore cannot reach the invitation stage.
		return s.rejectCurrentGuildCreatePermit(session, name, "founder lookup", err)
	case !nameChecked:
		return s.rejectCurrentGuildCreatePermit(session, name, "guild name not checked", nil)
	case promotion == "" || dnfguild.ValidatePromotion(promotion) != nil:
		return s.rejectCurrentGuildCreatePermit(session, name, "promotion message", nil)
	}
	if err := s.sendGameUpperSuccess(session, uint16(dnfenum.CmdPacketRequestGuildCreatePermit), nil); err != nil {
		return err
	}
	invite := &currentGuildCreateInvitation{creator: creator, target: target}
	session.guild.createInvitation = invite
	target.session.guild.createInvitation = invite
	var writer packetWriter
	writeClientNameDstr(&writer, character.Name)
	writer.writeByte(byte(s.channelAdvertiseID()))
	writeClientNameDstr(&writer, guildName)
	writeClientNameDstr(&writer, promotion)
	if err := s.sendGameUpperSecondaryRaw(target.session, currentGuildCreateInviteOpcode, writer.bytes()); err != nil {
		return errors.Join(err, s.cancelCurrentGuildCreateInvitation(invite, currentGuildCreateAuthResetValue, false))
	}
	invite.timer = time.AfterFunc(time.Minute, func() { s.expireCurrentGuildCreateInvitation(invite) })
	return nil
}

func (s *Service) handleCurrentGuildCreatePermitReply(session *gameSession, body []byte) error {
	s.guildCreateMu.Lock()
	defer s.guildCreateMu.Unlock()
	invite := session.guild.createInvitation
	if len(body) != 1 || body[0] > 1 || invite == nil || invite.target.session != session || invite.accepted ||
		!s.boundGameSessionCharacterCurrent(invite.creator) || !s.boundGameSessionCharacterCurrent(invite.target) {
		return s.sendGameUpperFailure(session, uint16(dnfenum.CmdPacketReplyGuildCreatePermit), currentGuildCreateStateError)
	}
	if err := s.sendGameUpperSuccess(session, uint16(dnfenum.CmdPacketReplyGuildCreatePermit), nil); err != nil {
		return errors.Join(err, s.cancelCurrentGuildCreateInvitation(invite, currentGuildCreateAuthResetValue, false))
	}
	if body[0] == 0 {
		return s.cancelCurrentGuildCreateInvitation(invite, 0, false)
	}
	invite.accepted = true
	invite.timer.Stop()
	if err := s.sendGameUpperSecondaryRaw(invite.creator.session, currentGuildCreateAuthStateOpcode, []byte{1}); err != nil {
		return errors.Join(err, s.cancelCurrentGuildCreateInvitation(invite, currentGuildCreateAuthResetValue, true))
	}
	return nil
}

func (s *Service) handleCurrentGuildCreateCancel(session *gameSession, _ []byte) error {
	s.closeCurrentGuildCreateSession(session)
	return s.sendGameUpperSuccess(session, uint16(dnfenum.CmdPacketCancelGuildCreate), nil)
}

func (s *Service) handleCurrentGuildCreate(session *gameSession, body []byte) error {
	s.guildCreateMu.Lock()
	defer s.guildCreateMu.Unlock()
	if len(body) != 1 || body[0] != 1 || session == nil || session.selectedCharacterID == 0 {
		return s.sendGameUpperFailure(session, uint16(dnfenum.CmdPacketCallGuildCreateRight), currentGuildCreateStateError)
	}
	session.guild.mu.Lock()
	name := session.guild.pendingName
	promotion := session.guild.promotion
	invite := session.guild.createInvitation
	ready := session.guild.nameChecked && invite != nil && invite.creator.session == session && invite.accepted &&
		s.boundGameSessionCharacterCurrent(invite.creator) && s.boundGameSessionCharacterCurrent(invite.target)
	session.guild.mu.Unlock()
	if !ready || dnfguild.ValidateName(name) != nil {
		return s.sendGameUpperFailure(session, uint16(dnfenum.CmdPacketCallGuildCreateRight), currentGuildCreateStateError)
	}
	repositories, ok := s.repositoryGroup()
	if !ok || repositories.CharacterSettlement == nil {
		return s.sendGameUpperFailure(session, uint16(dnfenum.CmdPacketCallGuildCreateRight), currentGuildGenericError)
	}
	ctx, cancel := s.currentGuildContext()
	defer cancel()
	characterID := strconv.Itoa(int(session.selectedCharacterID))
	now := s.gameplayNow()
	var created *dnfguild.Guild
	var goldAfter int64
	s.guildMu.Lock()
	// The settlement repository is scoped to the creator. The founder is read-only;
	// both memberships are committed together in the locked guild registry.
	promoterID := strconv.Itoa(int(invite.target.character))
	promoter, found, loadErr := repositories.Character.Load(ctx, promoterID)
	err := repositories.CharacterSettlement.WithinCharacterSettlement(ctx, characterID, func(tx dnfrepo.Group) error {
		if loadErr != nil {
			return loadErr
		}
		if !found || promoter.AccountID != s.accountIDForSession(invite.target.session) ||
			!s.boundGameSessionCharacterCurrent(invite.creator) || !s.boundGameSessionCharacterCurrent(invite.target) {
			return dnfguild.ErrCharacterNotFound
		}
		character, found, err := tx.Character.Load(ctx, characterID)
		if err != nil {
			return err
		}
		if !found || character.AccountID != s.accountIDForSession(session) {
			return dnfguild.ErrCharacterNotFound
		}
		registry, err := dnfguild.Load(ctx, tx.Settings)
		if err != nil {
			return err
		}
		if character.Stats == nil {
			character.Stats = make(map[string]int64)
		}
		gold := character.Stats["gold"]
		if gold < dnfguild.DefaultCreateGold {
			return dnfguild.ErrInsufficientGold
		}
		created, err = registry.Create(character, name, now)
		if err != nil {
			return err
		}
		if dnfguild.ValidatePromotion(promotion) != nil {
			return dnfguild.ErrInvalidPromotion
		}
		created.Promotion = strings.TrimSpace(promotion)
		// Use the targeted membership path; unrelated regular invitations may exist.
		if err := registry.Apply(created.ID, promoter, "", now); err != nil {
			return err
		}
		if _, err := registry.DecideApplication(created.ID, characterID, promoterID, true, now); err != nil {
			return err
		}
		character.Stats["gold"] = gold - dnfguild.DefaultCreateGold
		character.UpdatedAt = now
		goldAfter = character.Stats["gold"]
		if err := dnfrepo.SaveCharacterFields(ctx, tx.Character, character, dnfrepo.CharacterFieldStats); err != nil {
			return err
		}
		return dnfguild.Save(ctx, tx.Settings, registry, now)
	})
	s.guildMu.Unlock()
	if err != nil {
		code := currentGuildGenericError
		if errors.Is(err, dnfguild.ErrInsufficientGold) {
			code = currentGuildCreateGoldError
		} else if errors.Is(err, dnfguild.ErrNameExists) {
			code = currentGuildDuplicateNameError
		} else if errors.Is(err, dnfguild.ErrAlreadyMember) {
			code = 33
		}
		s.logGameEvent(session, "game-guild-create-rejected", "name", name, "error", err)
		return s.sendGameUpperFailure(session, uint16(dnfenum.CmdPacketCallGuildCreateRight), code)
	}
	session.guild.mu.Lock()
	session.guild.pendingName = ""
	session.guild.nameChecked = false
	session.guild.promotion = ""
	session.guild.createCommitted = true
	session.guild.mu.Unlock()
	s.clearCurrentGuildCreateInvitation(invite)
	if err := s.sendGameUpperSuccess(session, uint16(dnfenum.CmdPacketCallGuildCreateRight), nil); err != nil {
		return err
	}
	if err := s.sendGameUpperRawClass(session, uint16(dnfenum.CmdPacketWalkoutPartyMember), buildCurrentGoldStateBody(goldAfter), 0); err != nil {
		return err
	}
	refreshCtx, refreshCancel := s.currentGuildContext()
	defer refreshCancel()
	if err := s.refreshCurrentGuildMembership(refreshCtx, session, created); err != nil {
		return err
	}
	if err := s.sendCurrentGuildAccountProjection(refreshCtx, session); err != nil {
		s.logWarn("dnfbridge deferred guild account projection refresh",
			"char_id", session.selectedCharacterID,
			"guild_id", created.ID,
			"error", err)
	}
	s.logGameEvent(session, "game-guild-create-committed", "guild_id", created.ID, "guild_name", created.Name, "gold_after", goldAfter)
	if s.boundGameSessionCharacterCurrent(invite.target) {
		target := invite.target.session
		if err := s.refreshCurrentGuildMembership(refreshCtx, target, created); err != nil {
			s.logGameEvent(target, "game-guild-founder-notify-failed", "error", err)
		}
		if err := s.sendCurrentGuildAccountProjection(refreshCtx, target); err != nil {
			s.logGameEvent(target, "game-guild-founder-projection-failed", "error", err)
		}
	}
	return nil
}

func (s *Service) handleCurrentGuildSearch(session *gameSession, body []byte) error {
	request, ok := parseCurrentGuildSearchRequest(body)
	if !ok {
		rememberCurrentGuildRecommendationReplay(session, nil)
		return s.sendGameUpperFailure(session, uint16(dnfenum.CmdPacketSearchGuildList), currentGuildGenericError)
	}
	ctx, cancel := s.currentGuildContext()
	defer cancel()
	var results []*dnfguild.Guild
	var total int
	err := s.readCurrentGuild(ctx, func(_ dnfrepo.Group, registry dnfguild.Registry) error {
		results = registry.Search(request.Query, request.Page, currentGuildPageSize)
		all := registry.Search(request.Query, 0, max(len(registry.Guilds), 1))
		total = len(all)
		return nil
	})
	if err != nil {
		return s.sendGameUpperFailure(session, uint16(dnfenum.CmdPacketSearchGuildList), currentGuildGenericError)
	}
	rememberCurrentGuildRecommendationReplay(session, nil)
	return s.sendGameUpperSuccess(
		session,
		uint16(dnfenum.CmdPacketSearchGuildList),
		buildCurrentGuildSearchListBody(total, request.Page, results),
	)
}

func (s *Service) handleCurrentGuildAccountListIndex(session *gameSession, _ []byte) error {
	replayPending, replayGuildIDs := takeCurrentGuildRecommendationReplay(session)
	ctx, cancel := s.currentGuildContext()
	defer cancel()
	var membershipProjection []currentGuildCharacterProjection
	var applicationProjection []currentGuildCharacterProjection
	var accountGuilds []*dnfguild.Guild
	var applicationGuilds []*dnfguild.Guild
	var replayGuilds []*dnfguild.Guild
	var selectedGuild *dnfguild.Guild
	err := s.readCurrentGuild(ctx, func(repositories dnfrepo.Group, registry dnfguild.Registry) error {
		var projectionErr error
		membershipProjection, accountGuilds, projectionErr = currentGuildAccountProjection(
			ctx,
			repositories,
			registry,
			s.accountIDForSession(session),
		)
		if projectionErr != nil {
			return projectionErr
		}
		applicationProjection, applicationGuilds, projectionErr = currentGuildAccountApplicationProjection(
			ctx,
			repositories,
			registry,
			s.accountIDForSession(session),
			strconv.Itoa(int(session.selectedCharacterID)),
		)
		if projectionErr != nil {
			return projectionErr
		}
		accountGuilds = currentGuildUniqueRows(accountGuilds, applicationGuilds)
		selectedGuild = currentGuildSelectedForSession(registry, session)
		if replayPending {
			replayGuilds = make([]*dnfguild.Guild, 0, len(replayGuildIDs))
			for _, guildID := range replayGuildIDs {
				if guild, found := registry.GuildByID(guildID); found {
					replayGuilds = append(replayGuilds, guild)
				}
			}
		}
		return nil
	})
	if err != nil {
		return err
	}
	if err := s.sendGameUpperSuccess(
		session,
		uint16(dnfenum.CmdPacketRequestAccountGuildList),
		buildCurrentGuildAccountIndexBody(accountGuilds, membershipProjection),
	); err != nil {
		return err
	}
	if err := s.sendGameUpperSuccess(
		session,
		uint16(dnfenum.CmdPacketReqGuildInfoOfMyChars),
		buildCurrentGuildAccountListBody(applicationProjection),
	); err != nil {
		return err
	}
	// The current EXE rebuilds all three guild-board models when op1453/op744
	// arrives. That account refresh therefore clears the type-2 promotion rows
	// that the immediately preceding search populated with op762. Replay only
	// the exact one-shot search result after the account chain; unsolicited
	// promotion packets during login previously opened unrelated native panels.
	if replayPending {
		if err := s.sendGameUpperSuccess(
			session,
			uint16(dnfenum.CmdPacketReqRecommendGuild),
			buildCurrentGuildRecommendationBody(replayGuilds),
		); err != nil {
			return err
		}
	}
	// The native account/application handlers rebuild the guild-board model
	// while the board is already open. Reassert the selected character's
	// membership after that refresh so its final state is joined, not a stale
	// pending-application presentation.
	return s.sendCurrentGuildMembershipNotification(session, selectedGuild)
}

func rememberCurrentGuildRecommendationReplay(session *gameSession, guilds []*dnfguild.Guild) {
	if session == nil {
		return
	}
	guildIDs := make([]uint32, 0, len(guilds))
	for _, guild := range guilds {
		if guild != nil {
			guildIDs = append(guildIDs, guild.ID)
		}
	}
	session.guild.mu.Lock()
	session.guild.recommendationReplayPending = len(guildIDs) != 0
	session.guild.recommendationReplayGuildIDs = guildIDs
	session.guild.mu.Unlock()
}

func takeCurrentGuildRecommendationReplay(session *gameSession) (bool, []uint32) {
	if session == nil {
		return false, nil
	}
	session.guild.mu.Lock()
	defer session.guild.mu.Unlock()
	pending := session.guild.recommendationReplayPending
	guildIDs := append([]uint32(nil), session.guild.recommendationReplayGuildIDs...)
	session.guild.recommendationReplayPending = false
	session.guild.recommendationReplayGuildIDs = nil
	return pending, guildIDs
}

func (s *Service) handleCurrentGuildAccountList(session *gameSession, _ []byte) error {
	ctx, cancel := s.currentGuildContext()
	defer cancel()
	var projection []currentGuildCharacterProjection
	err := s.readCurrentGuild(ctx, func(repositories dnfrepo.Group, registry dnfguild.Registry) error {
		var err error
		projection, _, err = currentGuildAccountApplicationProjection(
			ctx,
			repositories,
			registry,
			s.accountIDForSession(session),
			strconv.Itoa(int(session.selectedCharacterID)),
		)
		return err
	})
	if err != nil {
		return s.sendGameUpperFailure(session, uint16(dnfenum.CmdPacketReqGuildInfoOfMyChars), currentGuildGenericError)
	}
	return s.sendGameUpperSuccess(session, uint16(dnfenum.CmdPacketReqGuildInfoOfMyChars), buildCurrentGuildAccountListBody(projection))
}

func currentGuildAccountProjection(
	ctx context.Context,
	repositories dnfrepo.Group,
	registry dnfguild.Registry,
	accountID string,
) ([]currentGuildCharacterProjection, []*dnfguild.Guild, error) {
	characters, err := repositories.Character.ListByAccount(ctx, accountID, dnfrepo.MaxCharacterSlotLimit)
	if err != nil {
		return nil, nil, err
	}
	projection := make([]currentGuildCharacterProjection, 0, len(characters))
	accountGuilds := make([]*dnfguild.Guild, 0)
	seenGuilds := make(map[uint32]struct{})
	for _, character := range characters {
		if character.Slot < 0 || character.Slot >= dnfrepo.MaxCharacterSlotLimit {
			return nil, nil, dnfguild.ErrCharacterNotFound
		}
		entry := currentGuildCharacterProjection{
			CharacterName: character.Name,
		}
		if guild, _, found := registry.GuildForCharacter(character.CharacterID); found {
			entry.GuildID = guild.ID
			entry.GuildName = guild.Name
			if _, seen := seenGuilds[guild.ID]; !seen {
				seenGuilds[guild.ID] = struct{}{}
				accountGuilds = append(accountGuilds, guild)
			}
		}
		projection = append(projection, entry)
	}
	return projection, accountGuilds, nil
}

// currentGuildAccountApplicationProjection builds the model consumed by
// NoPack sub_1D24010 (op744). That callback clears the native category-0
// "waiting for application acceptance" rows and recreates one row for every
// character/guild tuple in this body. The model is scoped to the currently
// selected character: sending a different character on the same account here
// still raises the current character's global "waiting" banner. Membership
// itself belongs to op1453's durable member mapping; the mapping state is not
// the character's persisted roster slot.
func currentGuildAccountApplicationProjection(
	ctx context.Context,
	repositories dnfrepo.Group,
	registry dnfguild.Registry,
	accountID string,
	selectedCharacterID string,
) ([]currentGuildCharacterProjection, []*dnfguild.Guild, error) {
	selectedCharacterID = strings.TrimSpace(selectedCharacterID)
	if selectedCharacterID == "" {
		return nil, nil, nil
	}
	projection := make([]currentGuildCharacterProjection, 0)
	applicationGuilds := make([]*dnfguild.Guild, 0)
	seenGuilds := make(map[uint32]struct{})
	for _, guild := range registry.Search("", 0, max(len(registry.Guilds), 1)) {
		for _, application := range dnfguild.SortedApplications(guild) {
			if application.AccountID != accountID || application.CharacterID != selectedCharacterID {
				continue
			}
			if _, _, alreadyMember := registry.GuildForCharacter(application.CharacterID); alreadyMember {
				continue
			}
			character, found, err := repositories.Character.Load(ctx, application.CharacterID)
			if err != nil {
				return nil, nil, err
			}
			if !found || character.AccountID != accountID {
				continue
			}
			if character.Slot < 0 || character.Slot >= dnfrepo.MaxCharacterSlotLimit {
				return nil, nil, dnfguild.ErrCharacterNotFound
			}
			projection = append(projection, currentGuildCharacterProjection{
				CharacterName:      character.Name,
				GuildName:          guild.Name,
				GuildID:            guild.ID,
				ApplicationPending: currentGuildApplicationCancelable,
			})
			if _, seen := seenGuilds[guild.ID]; !seen {
				seenGuilds[guild.ID] = struct{}{}
				applicationGuilds = append(applicationGuilds, guild)
			}
		}
	}
	return projection, applicationGuilds, nil
}

func currentGuildUniqueRows(groups ...[]*dnfguild.Guild) []*dnfguild.Guild {
	rows := make([]*dnfguild.Guild, 0)
	seen := make(map[uint32]struct{})
	for _, group := range groups {
		for _, guild := range group {
			if guild == nil {
				continue
			}
			if _, found := seen[guild.ID]; found {
				continue
			}
			seen[guild.ID] = struct{}{}
			rows = append(rows, guild)
		}
	}
	return rows
}

func currentGuildSelectedForSession(registry dnfguild.Registry, session *gameSession) *dnfguild.Guild {
	if session == nil || session.selectedCharacterID == 0 {
		return nil
	}
	selected, _, found := registry.GuildForCharacter(strconv.Itoa(int(session.selectedCharacterID)))
	if !found {
		return nil
	}
	return selected
}

func (s *Service) sendCurrentGuildMembershipNotification(session *gameSession, selected *dnfguild.Guild) error {
	return s.sendGameUpperRawClass(
		session,
		currentGuildMembershipNotificationMsgID,
		buildCurrentGuildMembershipNotificationBody(selected),
		0,
	)
}

func (s *Service) refreshCurrentGuildMembership(ctx context.Context, session *gameSession, selected *dnfguild.Guild) error {
	// The board callback reads the actor's guild ID, so update the actor first.
	if err := s.currentSendNameTagActorRefresh(ctx, session); err != nil {
		return err
	}
	if err := s.sendCurrentGuildManagementState(session, selected); err != nil {
		return err
	}
	return s.sendCurrentGuildMembershipNotification(session, selected)
}

type currentGuildEligibleApplication struct {
	Application dnfguild.Application
	Character   dnfrepo.CharacterRecord
}

// currentGuildEligibleApplications removes impossible character-level overlap
// and orphaned records from the read projection without rewriting persisted
// state while a guild window is being opened.
func currentGuildEligibleApplications(
	ctx context.Context,
	repositories dnfrepo.Group,
	guild *dnfguild.Guild,
) ([]currentGuildEligibleApplication, error) {
	if guild == nil {
		return nil, nil
	}
	applications := dnfguild.SortedApplications(guild)
	eligible := make([]currentGuildEligibleApplication, 0, len(applications))
	for _, application := range applications {
		if _, alreadyMember := guild.Members[application.CharacterID]; alreadyMember {
			continue
		}
		applicant, found, err := repositories.Character.Load(ctx, application.CharacterID)
		if err != nil {
			return nil, err
		}
		if !found {
			continue
		}
		eligible = append(eligible, currentGuildEligibleApplication{
			Application: application,
			Character:   applicant,
		})
	}
	return eligible, nil
}

func (s *Service) sendCurrentGuildAccountProjection(ctx context.Context, session *gameSession) error {
	var membershipProjection []currentGuildCharacterProjection
	var applicationProjection []currentGuildCharacterProjection
	var accountGuilds []*dnfguild.Guild
	var applicationGuilds []*dnfguild.Guild
	if err := s.readCurrentGuild(ctx, func(repositories dnfrepo.Group, registry dnfguild.Registry) error {
		var err error
		membershipProjection, accountGuilds, err = currentGuildAccountProjection(
			ctx,
			repositories,
			registry,
			s.accountIDForSession(session),
		)
		if err != nil {
			return err
		}
		applicationProjection, applicationGuilds, err = currentGuildAccountApplicationProjection(
			ctx,
			repositories,
			registry,
			s.accountIDForSession(session),
			strconv.Itoa(int(session.selectedCharacterID)),
		)
		if err != nil {
			return err
		}
		accountGuilds = currentGuildUniqueRows(accountGuilds, applicationGuilds)
		return nil
	}); err != nil {
		return err
	}
	if err := s.sendGameUpperSuccess(
		session,
		uint16(dnfenum.CmdPacketRequestAccountGuildList),
		buildCurrentGuildAccountIndexBody(accountGuilds, membershipProjection),
	); err != nil {
		return err
	}
	return s.sendGameUpperSuccess(
		session,
		uint16(dnfenum.CmdPacketReqGuildInfoOfMyChars),
		buildCurrentGuildAccountListBody(applicationProjection),
	)
}

func (s *Service) handleCurrentGuildApplicationNotice(session *gameSession, _ []byte) error {
	ctx, cancel := s.currentGuildContext()
	defer cancel()
	var guildID uint32
	var accountSlot byte
	var characterName string
	err := s.readCurrentGuild(ctx, func(repositories dnfrepo.Group, registry dnfguild.Registry) error {
		character, err := selectedGuildCharacter(ctx, repositories, session)
		if err != nil {
			return err
		}
		if character.Slot < 0 || character.Slot >= dnfrepo.MaxCharacterSlotLimit {
			return dnfguild.ErrCharacterNotFound
		}
		guild, _, found := registry.GuildForCharacter(character.CharacterID)
		if !found || guild == nil {
			return nil
		}
		guildID = guild.ID
		accountSlot = byte(character.Slot)
		characterName = character.Name
		return nil
	})
	if err != nil {
		return s.sendGameUpperFailure(session, uint16(dnfenum.CmdPacketJoinGuildInfo), currentGuildGenericError)
	}
	// NoPack sub_31A4E30 -> sub_31AAA90 removes the exact tuple from the
	// op1453 account mapping. Returning zero here leaves the native window in
	// "waiting for application acceptance" even after op349 succeeded.
	return s.sendGameUpperRaw(
		session,
		uint16(dnfenum.CmdPacketJoinGuildInfo),
		buildCurrentGuildApplicationNoticeBody(guildID, accountSlot, characterName),
	)
}

func (s *Service) currentGuildForSelectedCharacter(session *gameSession) (*dnfguild.Guild, error) {
	ctx, cancel := s.currentGuildContext()
	defer cancel()
	var selected *dnfguild.Guild
	err := s.readCurrentGuild(ctx, func(_ dnfrepo.Group, registry dnfguild.Registry) error {
		selected, _, _ = registry.GuildForCharacter(strconv.Itoa(int(session.selectedCharacterID)))
		return nil
	})
	return selected, err
}

func (s *Service) handleCurrentGuildInfo(session *gameSession, _ []byte) error {
	selected, err := s.currentGuildForSelectedCharacter(session)
	if err != nil {
		return err
	}
	return s.sendGameUpperSuccess(
		session,
		uint16(dnfenum.CmdPacketGuildInfo),
		buildCurrentGuildInfoBody(uint32(session.selectedCharacterID), selected),
	)
}

func (s *Service) handleCurrentGuildRefreshInfo(session *gameSession, _ []byte) error {
	selected, err := s.currentGuildForSelectedCharacter(session)
	if err != nil || selected == nil {
		return s.sendGameUpperFailure(session, uint16(dnfenum.CmdPacketRefreshGuildInfo), currentGuildGenericError)
	}
	if err := s.sendCurrentGuildManagementState(session, selected); err != nil {
		return err
	}
	return s.sendGameUpperSuccess(session, uint16(dnfenum.CmdPacketRefreshGuildInfo), buildCurrentGuildRefreshInfoBody())
}

func (s *Service) handleCurrentGuildBoardOpen(session *gameSession, _ []byte) error {
	selected, err := s.currentGuildForSelectedCharacter(session)
	if err != nil || selected == nil {
		return s.sendGameUpperFailure(session, uint16(dnfenum.CmdPacketOpenGuildBoard), currentGuildGenericError)
	}
	return s.sendGameUpperSuccess(session, uint16(dnfenum.CmdPacketOpenGuildBoard), nil)
}

func (s *Service) handleCurrentGuildBoardRefresh(session *gameSession, _ []byte) error {
	selected, err := s.currentGuildForSelectedCharacter(session)
	if err != nil || selected == nil {
		return s.sendGameUpperFailure(session, uint16(dnfenum.CmdPacketRefreshGuildboard), currentGuildGenericError)
	}
	return s.sendGameUpperSuccess(session, uint16(dnfenum.CmdPacketRefreshGuildboard), buildCurrentGuildBoardEmptyBody())
}

// op154 NOTIFY_MESSAGE_TO_GUILD carries the guild notice (公会公告). The
// current client reads it back from op70's second DSTR, not the 1024-unit
// introduction slot after the experience u32, so storing it as the
// introduction made every save look lost: the notice edit box was refilled
// from a field the server always sent empty.
func (s *Service) handleCurrentGuildNotify(session *gameSession, body []byte) error {
	message, ok := currentGuildDSTRAtStart(body)
	if !ok || dnfguild.ValidateNotice(message) != nil {
		return s.sendGameUpperFailure(session, uint16(dnfenum.CmdPacketNotifyMessageToGuild), currentGuildGenericError)
	}
	ctx, cancel := s.currentGuildContext()
	defer cancel()
	mutationErr := s.mutateCurrentGuild(ctx, func(_ dnfrepo.Group, registry *dnfguild.Registry) error {
		return registry.SetNotice(strconv.Itoa(int(session.selectedCharacterID)), message, s.gameplayNow())
	})
	if err := s.finishCurrentGuildSettings(session, uint16(dnfenum.CmdPacketNotifyMessageToGuild), mutationErr, nil); err != nil {
		return err
	}
	if mutationErr != nil {
		return nil
	}
	// op70 alone leaves the panel showing the old notice until the next
	// login: see currentGuildNoticeMessageOpcode.
	guild, loadErr := s.currentGuildForSelectedCharacter(session)
	if loadErr != nil || guild == nil {
		return loadErr
	}
	s.publishCurrentGuildNotice(guild)
	return nil
}

func (s *Service) handleCurrentGuildLegacyChangeGrade(session *gameSession, body []byte) error {
	// Every rejection below used to return the same generic failure with
	// nothing in the log, so "副会长/优秀会员 设置不了" could not be told apart
	// from "the client never sent anything". The request is
	// u8 selector / dstr target name / u8 grade, mirroring the success body
	// 86JP's GuildPacketBuilder.BuildMemberGradeChangeSuccessBody writes.
	if len(body) < 4 {
		s.logGameEvent(session, "game-guild-grade-body-short", "body", hex.EncodeToString(body))
		return s.sendGameUpperFailure(session, uint16(dnfenum.CmdPacketSetSubGuildMaster), currentGuildGenericError)
	}
	memberSelector := body[0]
	name, next, ok := parseCurrentGuildDSTR(body, 1)
	if !ok || next != len(body)-1 {
		s.logGameEvent(session, "game-guild-grade-body-unparsed",
			"body", hex.EncodeToString(body), "name_ok", ok, "next", next, "body_len", len(body))
		return s.sendGameUpperFailure(session, uint16(dnfenum.CmdPacketSetSubGuildMaster), currentGuildGenericError)
	}
	grade := body[next]
	ctx, cancel := s.currentGuildContext()
	defer cancel()
	actorID := strconv.Itoa(int(session.selectedCharacterID))
	resolvedTarget := ""
	actorGrade := byte(0xff)
	err := s.mutateCurrentGuild(ctx, func(_ dnfrepo.Group, registry *dnfguild.Registry) error {
		guild, actor, found := registry.GuildForCharacter(actorID)
		if !found {
			return dnfguild.ErrNotMember
		}
		actorGrade = actor.Grade
		targetID := ""
		for characterID, member := range guild.Members {
			if strings.EqualFold(strings.TrimSpace(member.Name), name) {
				targetID = characterID
				break
			}
		}
		if targetID == "" {
			return dnfguild.ErrNotMember
		}
		resolvedTarget = targetID
		return registry.ChangeGrade(actorID, targetID, grade, s.gameplayNow())
	})
	if err != nil {
		// ChangeGrade denies unless the actor is grade 1 and the requested
		// grade is 2..4, and the name lookup is an exact GB18030 round trip,
		// so log all three inputs rather than just the outcome.
		s.logGameEvent(session, "game-guild-grade-rejected",
			"body", hex.EncodeToString(body), "selector", memberSelector,
			"target_name", name, "grade", grade, "actor", actorID,
			"actor_grade", actorGrade, "target", resolvedTarget, "error", err)
	}
	return s.finishCurrentGuildGradeChange(
		session,
		uint16(dnfenum.CmdPacketSetSubGuildMaster),
		err,
		buildCurrentGuildLegacyGradeBody(memberSelector, name, grade),
	)
}

// sendCurrentInitialTownGuildStateProjectionLocked restores the current
// character's guild branch after the native selected-town scene has committed.
// The caller owns session.townMu. An explicit empty projection is intentional:
// it clears a previous character's guild state when the new character is not a
// member. The generation flag also lets a failed database/write attempt resume
// without replaying op24.
func (s *Service) sendCurrentInitialTownGuildStateProjectionLocked(session *gameSession, source string) error {
	if session == nil || session.selectedCharacterID == 0 || session.initialTownGuildStateProjectionSent ||
		session.initialTownRouteCharacterID != session.selectedCharacterID ||
		session.initialTownRouteStage < currentInitialTownRouteTransitionSent {
		return nil
	}
	ctx, cancel := s.currentGuildContext()
	defer cancel()
	characterID := strconv.Itoa(int(session.selectedCharacterID))
	var selected *dnfguild.Guild
	if err := s.readCurrentGuild(ctx, func(_ dnfrepo.Group, registry dnfguild.Registry) error {
		guild, _, found := registry.GuildForCharacter(characterID)
		if found {
			selected = guild
		}
		return nil
	}); err != nil {
		return err
	}
	if err := s.writeCurrentGuildManagementState(session, selected); err != nil {
		return err
	}
	if err := s.sendGameUpperRawClass(
		session,
		currentGuildMembershipNotificationMsgID,
		buildCurrentGuildMembershipNotificationBody(selected),
		0,
	); err != nil {
		return err
	}
	if err := s.sendCurrentGuildAccountProjection(ctx, session); err != nil {
		return err
	}
	session.initialTownGuildStateProjectionSent = true
	guildID := uint32(0)
	guildName := ""
	member := false
	if selected != nil {
		guildID = selected.ID
		guildName = selected.Name
		member = true
	}
	s.logGameEvent(session, "game-initial-town-guild-state-projected",
		"source", source,
		"char_id", session.selectedCharacterID,
		"guild_id", guildID,
		"guild_name", guildName,
		"member", member,
		"order", "after_typed_op24_scene_commit")
	return nil
}

func (s *Service) handleCurrentGuildMemberList(session *gameSession, _ []byte) error {
	return s.handleCurrentGuildMemberListForOpcode(session, uint16(dnfenum.CmdPacketGuildAllMemberList))
}

func (s *Service) handleCurrentGuildMemberListLegacy(session *gameSession, _ []byte) error {
	return s.handleCurrentGuildMemberListForOpcode(session, uint16(dnfenum.CmdPacketGuildMemerList))
}

func (s *Service) handleCurrentGuildMemberListLegacyNext(session *gameSession, _ []byte) error {
	return s.handleCurrentGuildMemberListForOpcode(session, uint16(dnfenum.CmdPacketGuildMemerListNext))
}

func (s *Service) handleCurrentGuildMemberListNext(session *gameSession, _ []byte) error {
	return s.handleCurrentGuildMemberListForOpcode(session, uint16(dnfenum.CmdPacketGuildAllMemberListNext))
}

// reconcileCurrentGuildMemberProfiles refreshes the member rows from the
// character repository and reports whether anything moved. Name, Job and Level
// are snapshotted when a character joins and were never refreshed afterwards,
// so the member window showed everyone at their join-time level -- a level 85
// character was reported as the 70 it had on 2026-09-07 -- and GrowType was
// not stored at all, which left every row rendering the base class instead of
// the advancement. The repository is the authority for all four.
func (s *Service) reconcileCurrentGuildMemberProfiles(
	ctx context.Context,
	repositories dnfrepo.Group,
	guild *dnfguild.Guild,
	now time.Time,
) bool {
	if guild == nil || repositories.Character == nil {
		return false
	}
	changed := false
	for id, member := range guild.Members {
		// Stamp the last-online mark while the member still has a session, so
		// the row can report how long an offline member has been away. The
		// client reads that elapsed time out of the member row; without it an
		// offline member is indistinguishable from an online one.
		if identity, online := s.onlineGameSessionCharacterSnapshot(
			uint16(currentGuildNumericID(id))); online &&
			identity.session != nil && identity.session.conn != nil {
			member.LastOnlineAt = now
			guild.Members[id] = member
			changed = true
		}
		character, found, err := repositories.Character.Load(ctx, id)
		if err != nil || !found {
			continue
		}
		job := strconv.Itoa(numericCharacterStat(character.Job))
		grow := strconv.FormatInt(
			numericCharacterStatValue(character, "grow_type"), 10)
		name := strings.TrimSpace(character.Name)
		if member.Job == job && member.GrowType == grow &&
			member.Level == character.Level &&
			(name == "" || member.Name == name) {
			continue
		}
		member.Job = job
		member.GrowType = grow
		member.Level = character.Level
		if name != "" {
			member.Name = name
		}
		guild.Members[id] = member
		changed = true
	}
	return changed
}

func (s *Service) handleCurrentGuildMemberListForOpcode(session *gameSession, opcode uint16) error {
	// A presence refresh may only replay the list for a client that asked for
	// it at least once; see publishCurrentGuildPresence.
	markCurrentGuildMemberListRequested(session)
	ctx, cancel := s.currentGuildContext()
	defer cancel()
	characterID := strconv.Itoa(int(session.selectedCharacterID))
	// Refresh before reading. A failure here is not fatal: the stored snapshot
	// still renders a complete list, it is only out of date.
	if err := s.mutateCurrentGuild(ctx, func(
		repositories dnfrepo.Group,
		registry *dnfguild.Registry,
	) error {
		guild, _, found := registry.GuildForCharacter(characterID)
		if !found {
			return nil
		}
		if s.reconcileCurrentGuildMemberProfiles(
			ctx, repositories, guild, s.gameplayNow()) {
			guild.UpdatedAt = s.gameplayNow()
		}
		return nil
	}); err != nil {
		s.logGameEvent(session, "game-guild-member-profile-refresh-failed",
			"char_id", session.selectedCharacterID,
			"opcode", opcode,
			"error", err)
	}
	var selected *dnfguild.Guild
	err := s.readCurrentGuild(ctx, func(_ dnfrepo.Group, registry dnfguild.Registry) error {
		guild, _, found := registry.GuildForCharacter(characterID)
		if found {
			selected = guild
		}
		return nil
	})
	if err != nil {
		return s.sendGameUpperFailure(session, opcode, currentGuildGenericError)
	}
	if opcode == uint16(dnfenum.CmdPacketGuildAllMemberList) || opcode == uint16(dnfenum.CmdPacketGuildMemerList) {
		if err := s.sendCurrentGuildManagementState(session, selected); err != nil {
			return err
		}
	}
	return s.sendGameUpperSuccess(
		session,
		opcode,
		buildCurrentGuildMemberListBody(
			selected,
			opcode == uint16(dnfenum.CmdPacketGuildMemerList) || opcode == uint16(dnfenum.CmdPacketGuildAllMemberList),
			opcode == uint16(dnfenum.CmdPacketGuildAllMemberList) || opcode == uint16(dnfenum.CmdPacketGuildAllMemberListNext),
			s.currentGuildOnlineChannels(selected, session),
			s.gameplayNow(),
		),
	)
}

func (s *Service) handleCurrentGuildDetail(session *gameSession, body []byte) error {
	name, ok := currentGuildDSTRAtStart(body)
	if !ok {
		return s.sendGameUpperFailure(session, uint16(dnfenum.CmdPacketReqGuildSerchForJoin), currentGuildGenericError)
	}
	ctx, cancel := s.currentGuildContext()
	defer cancel()
	var selected *dnfguild.Guild
	err := s.readCurrentGuild(ctx, func(_ dnfrepo.Group, registry dnfguild.Registry) error {
		if guild, found := registry.GuildByName(name); found {
			selected = guild
		}
		return nil
	})
	if err != nil || selected == nil {
		return s.sendGameUpperFailure(session, uint16(dnfenum.CmdPacketReqGuildSerchForJoin), currentGuildGenericError)
	}
	if err := s.sendGameUpperSuccess(session, uint16(dnfenum.CmdPacketReqGuildSerchForJoin), buildCurrentGuildSearchResultBody(selected)); err != nil {
		return err
	}
	return nil
}

func (s *Service) handleCurrentGuildRecommend(session *gameSession, _ []byte) error {
	ctx, cancel := s.currentGuildContext()
	defer cancel()
	var results []*dnfguild.Guild
	var projection []currentGuildCharacterProjection
	var accountGuilds []*dnfguild.Guild
	var selectedGuild *dnfguild.Guild
	err := s.readCurrentGuild(ctx, func(repositories dnfrepo.Group, registry dnfguild.Registry) error {
		results = registry.Search("", 0, currentGuildPageSize)
		var projectionErr error
		projection, accountGuilds, projectionErr = currentGuildAccountProjection(
			ctx,
			repositories,
			registry,
			s.accountIDForSession(session),
		)
		selectedGuild = currentGuildSelectedForSession(registry, session)
		return projectionErr
	})
	if err != nil {
		return s.sendGameUpperFailure(session, uint16(dnfenum.CmdPacketReqRecommendGuild), currentGuildGenericError)
	}
	if err := s.sendGameUpperSuccess(
		session,
		uint16(dnfenum.CmdPacketRequestAccountGuildList),
		buildCurrentGuildAccountIndexBody(currentGuildUniqueRows(results, accountGuilds), projection),
	); err != nil {
		return err
	}
	if err := s.sendGameUpperSuccess(
		session,
		uint16(dnfenum.CmdPacketReqRecommendGuild),
		buildCurrentGuildRecommendationBody(results),
	); err != nil {
		return err
	}
	return s.sendCurrentGuildMembershipNotification(session, selectedGuild)
}

func (s *Service) handleCurrentGuildJoinCheck(session *gameSession, body []byte) error {
	name, ok := currentGuildDSTRAtStart(body)
	if !ok {
		return s.sendGameUpperFailure(session, uint16(dnfenum.CmdPacketCheckJoinGuild), currentGuildGenericError)
	}
	ctx, cancel := s.currentGuildContext()
	defer cancel()
	var selected *dnfguild.Guild
	err := s.readCurrentGuild(ctx, func(_ dnfrepo.Group, registry dnfguild.Registry) error {
		selected, _ = registry.GuildByName(name)
		return nil
	})
	if err != nil || selected == nil {
		return s.sendGameUpperFailure(session, uint16(dnfenum.CmdPacketCheckJoinGuild), currentGuildGenericError)
	}
	return s.sendGameUpperSuccess(session, uint16(dnfenum.CmdPacketCheckJoinGuild), buildCurrentGuildJoinCheckBody(selected))
}

func (s *Service) handleCurrentGuildApply(session *gameSession, body []byte) error {
	name, next, ok := parseCurrentGuildDSTR(body, 0)
	if !ok {
		return s.sendGameUpperFailure(session, uint16(dnfenum.CmdPacketRequestJoinGuild), currentGuildGenericError)
	}
	message := ""
	if next < len(body) {
		message, _, _ = parseCurrentGuildDSTR(body, next)
	}
	ctx, cancel := s.currentGuildContext()
	defer cancel()
	var selected *dnfguild.Guild
	joinedDirectly := false
	err := s.mutateCurrentGuild(ctx, func(repositories dnfrepo.Group, registry *dnfguild.Registry) error {
		character, err := selectedGuildCharacter(ctx, repositories, session)
		if err != nil {
			return err
		}
		guild, found := registry.GuildByName(name)
		if !found {
			return dnfguild.ErrGuildNotFound
		}
		// currentGuildImmediateJoinMode is 立即加入: op740's mode byte, which the
		// settings window renders as 31592 when [record+0x3c] == 1 and as 31591
		// 接受后加入 when it is 0 (00FC903A..00FC90C3). The client sends one op348
		// either way and expects the server to honour the mode, so an immediate
		// guild must not leave the player on the applicant list. Everything else
		// keeps the historic application flow.
		if guild.JoinMode == currentGuildImmediateJoinMode {
			if _, err := registry.JoinDirect(guild.ID, character, s.gameplayNow()); err != nil {
				return err
			}
			selected = guild
			joinedDirectly = true
			return nil
		}
		if err := registry.Apply(guild.ID, character, message, s.gameplayNow()); err != nil {
			return err
		}
		selected = guild
		return nil
	})
	if err != nil {
		return s.sendGameUpperFailure(session, uint16(dnfenum.CmdPacketRequestJoinGuild), currentGuildGenericError)
	}
	if joinedDirectly {
		// The 申请 ack has to land BEFORE the membership refresh. The client's
		// op348 handler rewrites the join window from the ack body, so a refresh
		// that arrives first is overwritten and the window keeps showing the
		// pre-join state until it is closed and reopened (captured order before
		// this fix: refresh at 15:52:21.792-.796, ack at .797).
		if ackErr := s.sendGameUpperSuccess(
			session,
			uint16(dnfenum.CmdPacketRequestJoinGuild),
			buildCurrentGuildApplyBody(selected),
		); ackErr != nil {
			return ackErr
		}
		// Mirror the approve path so the joiner's own client switches from
		// "waiting for acceptance" to a real membership.
		if refreshErr := s.refreshCurrentGuildMembership(ctx, session, selected); refreshErr != nil {
			s.logGameEvent(session, "game-guild-immediate-join-refresh-failed", "error", refreshErr)
		}
		if projectionErr := s.sendCurrentGuildAccountProjection(ctx, session); projectionErr != nil {
			s.logGameEvent(session, "game-guild-immediate-join-account-refresh-failed", "error", projectionErr)
		}
		s.publishCurrentGuildPresence(session.selectedCharacterID, "guild_immediate_join")
		// The approve path refreshes the approver's window with a full member
		// list (handleCurrentGuildMemberList -> op140). Nobody approves an
		// immediate join, so the master and every other online member get the
		// same packet here; op67 alone left their open window stale until it
		// was reopened.
		s.broadcastCurrentGuildMemberListRefresh(ctx, selected, session.selectedCharacterID, "guild_immediate_join")
		s.logGameEvent(session, "game-guild-immediate-join",
			"guild_id", selected.ID, "char_id", session.selectedCharacterID,
			"join_mode", selected.JoinMode)
		return nil
	}
	return s.sendGameUpperSuccess(session, uint16(dnfenum.CmdPacketRequestJoinGuild), buildCurrentGuildApplyBody(selected))
}

// broadcastCurrentGuildMemberListRefresh replays the member list for every other
// online member of guild that has already asked for it in this session, exactly
// like the approve path does for the approver. Sessions that never opened the
// guild window are skipped so an unsolicited member list can never raise the
// panel on its own (see publishCurrentGuildPresence).
func (s *Service) broadcastCurrentGuildMemberListRefresh(
	ctx context.Context,
	guild *dnfguild.Guild,
	skipCharacterID uint16,
	source string,
) {
	if s == nil || guild == nil {
		return
	}
	for id := range guild.Members {
		memberID := uint16(currentGuildNumericID(id))
		if memberID == 0 || memberID == skipCharacterID {
			continue
		}
		target, online := s.onlineGameSessionCharacterSnapshot(memberID)
		if !online || target.session == nil {
			continue
		}
		if !currentGuildMemberListRequested(target.session) {
			continue
		}
		if err := s.handleCurrentGuildMemberList(target.session, nil); err != nil {
			s.logGameEvent(target.session, "game-guild-member-list-broadcast-failed",
				"source", source, "char_id", memberID, "error", err)
		}
	}
}

func (s *Service) handleCurrentGuildCancelApply(session *gameSession, body []byte) error {
	guildID, _, ok := parseCurrentGuildUint32(body, 0)
	if !ok {
		return s.sendGameUpperFailure(session, uint16(dnfenum.CmdPacketCancelJoinGuild), currentGuildGenericError)
	}
	ctx, cancel := s.currentGuildContext()
	defer cancel()
	characterID := strconv.Itoa(int(session.selectedCharacterID))
	var staleMemberGuild *dnfguild.Guild
	var realApplication bool
	if err := s.readCurrentGuild(ctx, func(_ dnfrepo.Group, registry dnfguild.Registry) error {
		guild, found := registry.GuildByID(guildID)
		if !found {
			return dnfguild.ErrGuildNotFound
		}
		if _, found := guild.Members[characterID]; found {
			staleMemberGuild = guild
			return nil
		}
		_, realApplication = guild.Applications[characterID]
		if !realApplication {
			return dnfguild.ErrApplicationNotFound
		}
		return nil
	}); err != nil {
		return s.sendGameUpperFailure(session, uint16(dnfenum.CmdPacketCancelJoinGuild), currentGuildGenericError)
	}
	if staleMemberGuild != nil {
		// A guild member cannot simultaneously have an application. Older direct
		// creation builds nevertheless left a client-only pending row behind. A
		// successful cancel response removes that stale row; then the empty op744
		// projection and op308 membership notification restore the authoritative
		// member state without changing or rewriting the guild registry.
		if err := s.sendGameUpperSuccess(session, uint16(dnfenum.CmdPacketCancelJoinGuild), nil); err != nil {
			return err
		}
		if err := s.sendGameUpperSecondaryRaw(
			session,
			currentGuildCreateAuthStateOpcode,
			[]byte{currentGuildCreateAuthResetValue},
		); err != nil {
			return err
		}
		if err := s.sendCurrentGuildAccountProjection(ctx, session); err != nil {
			return err
		}
		return s.sendGameUpperRawClass(
			session,
			currentGuildMembershipNotificationMsgID,
			buildCurrentGuildMembershipNotificationBody(staleMemberGuild),
			0,
		)
	}
	if !realApplication {
		return s.sendGameUpperFailure(session, uint16(dnfenum.CmdPacketCancelJoinGuild), currentGuildGenericError)
	}
	err := s.mutateCurrentGuild(ctx, func(_ dnfrepo.Group, registry *dnfguild.Registry) error {
		return registry.CancelApplication(guildID, characterID, s.gameplayNow())
	})
	if err != nil {
		return s.sendGameUpperFailure(session, uint16(dnfenum.CmdPacketCancelJoinGuild), currentGuildGenericError)
	}
	return s.sendGameUpperSuccess(session, uint16(dnfenum.CmdPacketCancelJoinGuild), nil)
}

func (s *Service) handleCurrentGuildApplicationList(session *gameSession, _ []byte) error {
	ctx, cancel := s.currentGuildContext()
	defer cancel()
	var applications []dnfguild.Application
	err := s.readCurrentGuild(ctx, func(repositories dnfrepo.Group, registry dnfguild.Registry) error {
		selected, _, found := registry.GuildForCharacter(strconv.Itoa(int(session.selectedCharacterID)))
		if !found || selected == nil {
			return dnfguild.ErrNotMember
		}
		eligible, eligibleErr := currentGuildEligibleApplications(ctx, repositories, selected)
		if eligibleErr != nil {
			return eligibleErr
		}
		applications = make([]dnfguild.Application, 0, len(eligible))
		for _, application := range eligible {
			applications = append(applications, application.Application)
		}
		return nil
	})
	if err != nil {
		return s.sendGameUpperFailure(session, uint16(dnfenum.CmdPacketGuildJoinList), currentGuildGenericError)
	}
	return s.sendGameUpperSuccess(session, uint16(dnfenum.CmdPacketGuildJoinList), buildCurrentGuildApplicationListBody(applications))
}

func (s *Service) decideCurrentGuildApplication(session *gameSession, body []byte, approve bool, opcode uint16) error {
	// Approve and deny do not agree on the region field's width. Both senders
	// write {applicant ID, region}, but op350 emits the region through
	// sub_3454FC0 (one byte) while op351 uses sub_3455020 (four) -- see
	// 03198793..031987B7. Captured bodies: approve 2b000000 00, deny
	// 2b00000001000000. Only the 5-byte form used to be decoded, so every
	// 拒绝 fell through with target 0 and was refused, leaving the applicant
	// on the list.
	var response packetWriter
	target := uint32(0)
	region := byte(0)
	wideRegion := false
	switch len(body) {
	case 5:
		target, region = binary.LittleEndian.Uint32(body), body[4]
	case 8:
		target, wideRegion = binary.LittleEndian.Uint32(body), true
		if wide := binary.LittleEndian.Uint32(body[4:]); wide < 256 {
			region = byte(wide)
		}
	}
	// Mirror the width back. No class-1 handler is registered for op351 in
	// this client -- it refreshes by re-requesting op352 instead, which the
	// game-guild-request log confirms -- but echoing the shape received keeps
	// this honest if a build ever registers one.
	response.writeUint32(target)
	if wideRegion {
		response.writeUint32(uint32(region))
	} else {
		response.writeByte(region)
	}
	failure := func() error {
		return s.sendGameUpperRawClass(session, opcode, append([]byte{0, currentGuildGenericError}, response.bytes()...), 1)
	}
	// The region byte is whatever buildCurrentGuildApplicationListBody put in
	// the applicant row's last byte, echoed back by the sender at 03198789.
	// Compare against that constant rather than channelAdvertiseID(), so the
	// two can never drift apart if the advertised id is ever configured.
	if target == 0 || region != currentGuildMemberRegion {
		s.logGameEvent(session, "game-guild-application-decision-rejected",
			"body", hex.EncodeToString(body), "target", target,
			"region", region, "want_region", currentGuildMemberRegion, "approve", approve)
		return failure()
	}
	targetID := strconv.FormatUint(uint64(target), 10)
	ctx, cancel := s.currentGuildContext()
	defer cancel()
	var selected *dnfguild.Guild
	err := s.mutateCurrentGuild(ctx, func(_ dnfrepo.Group, registry *dnfguild.Registry) error {
		guild, _, found := registry.GuildForCharacter(strconv.Itoa(int(session.selectedCharacterID)))
		if !found {
			return dnfguild.ErrNotMember
		}
		_, err := registry.DecideApplication(guild.ID, strconv.Itoa(int(session.selectedCharacterID)), targetID, approve, s.gameplayNow())
		selected = guild
		return err
	})
	if err != nil {
		return failure()
	}
	if err := s.sendGameUpperSuccess(session, opcode, response.bytes()); err != nil {
		return err
	}
	if approve {
		if target, found := s.onlineGameSessionCharacterSnapshot(uint16(currentGuildNumericID(targetID))); found {
			if err := s.refreshCurrentGuildMembership(ctx, target.session, selected); err != nil {
				s.logGameEvent(target.session, "game-guild-approval-refresh-failed", "error", err)
			}
			if err := s.sendCurrentGuildAccountProjection(ctx, target.session); err != nil {
				s.logGameEvent(target.session, "game-guild-approval-account-refresh-failed", "error", err)
			}
		}
		return s.handleCurrentGuildMemberList(session, nil)
	}
	return nil
}

func (s *Service) handleCurrentGuildApprove(session *gameSession, body []byte) error {
	return s.decideCurrentGuildApplication(session, body, true, uint16(dnfenum.CmdPacketApproveJoinGuild))
}

func (s *Service) handleCurrentGuildDeny(session *gameSession, body []byte) error {
	return s.decideCurrentGuildApplication(session, body, false, uint16(dnfenum.CmdPacketDenyJoinGuild))
}

func (s *Service) handleCurrentGuildInvite(session *gameSession, body []byte) error {
	name, ok := currentGuildDSTRAtStart(body)
	if !ok {
		return s.sendGameUpperFailure(session, uint16(dnfenum.CmdPacketCallGuildInvite), currentGuildGenericError)
	}
	ctx, cancel := s.currentGuildContext()
	defer cancel()
	err := s.mutateCurrentGuild(ctx, func(repositories dnfrepo.Group, registry *dnfguild.Registry) error {
		id, found, err := repositories.Character.FindIDByName(ctx, name)
		if err != nil || !found {
			return dnfguild.ErrCharacterNotFound
		}
		target, found, err := repositories.Character.Load(ctx, id)
		if err != nil || !found {
			return dnfguild.ErrCharacterNotFound
		}
		return registry.Invite(strconv.Itoa(int(session.selectedCharacterID)), target, s.gameplayNow())
	})
	if err != nil {
		return s.sendGameUpperFailure(session, uint16(dnfenum.CmdPacketCallGuildInvite), currentGuildGenericError)
	}
	return s.sendGameUpperSuccess(session, uint16(dnfenum.CmdPacketCallGuildInvite), nil)
}

func (s *Service) handleCurrentGuildInviteReply(session *gameSession, body []byte) error {
	if len(body) < 1 {
		return s.sendGameUpperFailure(session, uint16(dnfenum.CmdPacketReplyGuildInvite), currentGuildGenericError)
	}
	ctx, cancel := s.currentGuildContext()
	defer cancel()
	var selected *dnfguild.Guild
	err := s.mutateCurrentGuild(ctx, func(repositories dnfrepo.Group, registry *dnfguild.Registry) error {
		character, err := selectedGuildCharacter(ctx, repositories, session)
		if err != nil {
			return err
		}
		selected, err = registry.ReplyInvitation(character, body[0] == 1, s.gameplayNow())
		return err
	})
	if err != nil {
		return s.sendGameUpperFailure(session, uint16(dnfenum.CmdPacketReplyGuildInvite), currentGuildGenericError)
	}
	if err := s.sendGameUpperSuccess(session, uint16(dnfenum.CmdPacketReplyGuildInvite), nil); err != nil {
		return err
	}
	if body[0] == 1 {
		return s.refreshCurrentGuildMembership(ctx, session, selected)
	}
	return nil
}

// handleCurrentGuildLeave serves op153 for both 退出公会 and 开除会员.
// sub_26A3540 builds the request as sub_3454EB0(w, 153) / sub_3454FC0(w,
// region) / sub_3455180(w, name) (026A3569..026A357D), so the body is a u8
// region followed by the target's name -- the captured kick body is
// 01 04000000 <GB18030 name>.
//
// This used to ignore the body entirely and always remove the requester. As
// the master that returned the generic failure, which op153's reply handler
// renders as 您不是工会成员或此工会不存在, because Registry.Leave refuses to
// drop the master; as anyone else it would have silently removed the requester
// instead of the member they picked.
func (s *Service) handleCurrentGuildLeave(session *gameSession, body []byte) error {
	opcode := uint16(dnfenum.CmdPacketReqGuildSecede)
	targetName := ""
	if len(body) > 0 {
		name, next, ok := parseCurrentGuildDSTR(body, 1)
		if !ok || next != len(body) {
			s.logGameEvent(session, "game-guild-secede-body-unparsed",
				"body", hex.EncodeToString(body), "name_ok", ok, "next", next)
			return s.sendGameUpperFailure(session, opcode, currentGuildGenericError)
		}
		targetName = name
	}
	ctx, cancel := s.currentGuildContext()
	defer cancel()
	characterID := strconv.Itoa(int(session.selectedCharacterID))
	var removedID, removedName, guildName string
	err := s.mutateCurrentGuild(ctx, func(repositories dnfrepo.Group, registry *dnfguild.Registry) error {
		character, err := selectedGuildCharacter(ctx, repositories, session)
		if err != nil {
			return err
		}
		guild, _, found := registry.GuildForCharacter(characterID)
		if !found {
			return dnfguild.ErrNotMember
		}
		guildName = guild.Name
		if strings.TrimSpace(targetName) == "" || currentGuildSameName(targetName, character.Name) {
			removedID, removedName = characterID, character.Name
			return registry.Leave(characterID, s.gameplayNow())
		}
		for id, member := range guild.Members {
			if currentGuildSameName(member.Name, targetName) {
				removedID, removedName = id, member.Name
				// RemoveMember enforces the authority rules: the actor must
				// be grade 2 or better and the target must rank below them
				// and not be the master.
				return registry.RemoveMember(characterID, id, s.gameplayNow())
			}
		}
		return dnfguild.ErrNotMember
	})
	if err != nil {
		s.logGameEvent(session, "game-guild-secede-rejected",
			"body", hex.EncodeToString(body), "target_name", targetName,
			"actor", characterID, "removed", removedID, "error", err)
		return s.sendGameUpperFailure(session, opcode, currentGuildGenericError)
	}
	// sub_1D161F0 reads u8 / DSTR / u8 / DSTR and switches on the first byte:
	// 1 renders string 0x1A2C and 2 renders 0x1A2D, both with (second DSTR,
	// first DSTR); the second u8 is parsed and never read. 86JP passes 1 as
	// its expelledFlag for a kick, which is also what the 退出公会 path here
	// has always sent.
	var response packetWriter
	response.writeByte(1)
	writeClientNameDstr(&response, removedName)
	response.writeByte(0)
	writeClientNameDstr(&response, guildName)
	if err := s.sendGameUpperSuccess(session, opcode, response.bytes()); err != nil {
		return err
	}
	if removedID == characterID {
		return s.refreshCurrentGuildMembership(ctx, session, nil)
	}
	// A kick leaves the requester in the guild; it is the target whose actor
	// and guild panel have to be torn down, and everyone else's member list
	// still has the removed row in it.
	if removed := currentGuildNumericID(removedID); removed != 0 {
		if target, online := s.onlineGameSessionCharacterSnapshot(uint16(removed)); online && target.session != nil {
			if err := s.refreshCurrentGuildMembership(ctx, target.session, nil); err != nil {
				s.logGameEvent(target.session, "game-guild-secede-target-refresh-failed", "error", err)
			}
		}
	}
	guild, loadErr := s.currentGuildForSelectedCharacter(session)
	if loadErr != nil || guild == nil {
		return loadErr
	}
	if err := s.sendCurrentGuildManagementState(session, guild); err != nil {
		return err
	}
	s.republishCurrentGuildMemberLists(guild)
	return nil
}

// currentGuildSameName compares roster names the way the guild handlers match
// a client-supplied name against stored members.
func currentGuildSameName(left, right string) bool {
	return strings.EqualFold(strings.TrimSpace(left), strings.TrimSpace(right))
}

func (s *Service) handleCurrentGuildDisband(session *gameSession, _ []byte) error {
	ctx, cancel := s.currentGuildContext()
	defer cancel()
	var members []dnfguild.Member
	err := s.mutateCurrentGuild(ctx, func(_ dnfrepo.Group, registry *dnfguild.Registry) error {
		if guild, _, found := registry.GuildForCharacter(strconv.Itoa(int(session.selectedCharacterID))); found {
			members = dnfguild.SortedMembers(guild)
		}
		return registry.Disband(strconv.Itoa(int(session.selectedCharacterID)))
	})
	if err != nil {
		return s.sendGameUpperFailure(session, uint16(dnfenum.CmdPacketBreakGuild), currentGuildGenericError)
	}
	if err := s.sendGameUpperSuccess(session, uint16(dnfenum.CmdPacketBreakGuild), nil); err != nil {
		return err
	}
	for _, member := range members {
		if member.CharacterID == strconv.Itoa(int(session.selectedCharacterID)) {
			continue
		}
		if target, found := s.onlineGameSessionCharacterSnapshot(uint16(currentGuildNumericID(member.CharacterID))); found {
			if err := s.refreshCurrentGuildMembership(ctx, target.session, nil); err != nil {
				s.logGameEvent(target.session, "game-guild-disband-refresh-failed", "error", err)
			}
		}
	}
	return s.refreshCurrentGuildMembership(ctx, session, nil)
}

func (s *Service) handleCurrentGuildDelegate(session *gameSession, body []byte) error {
	targetID, ok := currentGuildTargetCharacterID(body)
	if !ok {
		if name, nameOK := currentGuildDSTRAtStart(body); nameOK {
			ctx, cancel := s.currentGuildContext()
			defer cancel()
			repositories, available := s.repositoryGroup()
			if available && repositories.Character != nil {
				id, found, _ := repositories.Character.FindIDByName(ctx, name)
				if found {
					targetID, ok = id, true
				}
			}
		}
	}
	if !ok {
		return s.sendGameUpperFailure(session, uint16(dnfenum.CmdPacketGuildMasterDelegate), currentGuildGenericError)
	}
	ctx, cancel := s.currentGuildContext()
	defer cancel()
	err := s.mutateCurrentGuild(ctx, func(_ dnfrepo.Group, registry *dnfguild.Registry) error {
		return registry.Delegate(strconv.Itoa(int(session.selectedCharacterID)), targetID, s.gameplayNow())
	})
	return s.finishCurrentGuildSettings(session, uint16(dnfenum.CmdPacketGuildMasterDelegate), err, nil)
}

func (s *Service) handleCurrentGuildMemberMemo(session *gameSession, body []byte) error {
	characterID := strconv.Itoa(int(session.selectedCharacterID))
	offset := 0
	if len(body) >= 8 {
		if target, next, ok := parseCurrentGuildUint32(body, 0); ok {
			characterID = strconv.FormatUint(uint64(target), 10)
			offset = next
		}
	}
	memo, _, ok := parseCurrentGuildDSTR(body, offset)
	if !ok {
		return s.sendGameUpperFailure(session, uint16(dnfenum.CmdPacketWriteGuildMemberMemo), currentGuildGenericError)
	}
	ctx, cancel := s.currentGuildContext()
	defer cancel()
	actorID := strconv.Itoa(int(session.selectedCharacterID))
	err := s.mutateCurrentGuild(ctx, func(_ dnfrepo.Group, registry *dnfguild.Registry) error {
		return registry.SetMemo(actorID, characterID, memo, s.gameplayNow())
	})
	if err != nil {
		return s.sendGameUpperFailure(session, uint16(dnfenum.CmdPacketWriteGuildMemberMemo), currentGuildGenericError)
	}
	return s.sendGameUpperSuccess(session, uint16(dnfenum.CmdPacketWriteGuildMemberMemo), nil)
}

func (s *Service) handleCurrentGuildPromotion(session *gameSession, body []byte) error {
	mask, promotion, alliance, tags, mode, ok := parseCurrentGuildRecruitment(body)
	if !ok {
		return s.sendGameUpperFailure(session, uint16(dnfenum.CmdPacketModifyGuildPromoteMsg), currentGuildGenericError)
	}
	ctx, cancel := s.currentGuildContext()
	defer cancel()
	err := s.mutateCurrentGuild(ctx, func(_ dnfrepo.Group, registry *dnfguild.Registry) error {
		return registry.SetRecruitment(strconv.Itoa(int(session.selectedCharacterID)), mask, promotion, alliance, tags, mode, s.gameplayNow())
	})
	return s.finishCurrentGuildSettings(session, uint16(dnfenum.CmdPacketModifyGuildPromoteMsg), err, nil)
}

func (s *Service) handleCurrentGuildAttendance(session *gameSession, _ []byte) error {
	ctx, cancel := s.currentGuildContext()
	defer cancel()
	err := s.mutateCurrentGuild(ctx, func(_ dnfrepo.Group, registry *dnfguild.Registry) error {
		_, err := registry.Attend(strconv.Itoa(int(session.selectedCharacterID)), s.gameplayNow())
		return err
	})
	if err != nil {
		return s.sendGameUpperFailure(session, uint16(dnfenum.CmdPacketGuildAttendanceInfo), currentGuildGenericError)
	}
	if err := s.sendGameUpperSuccess(session, uint16(dnfenum.CmdPacketGuildAttendanceInfo), buildCurrentGuildAttendanceBody()); err != nil {
		return err
	}
	// 公会积分 (op747) is granted by this attendance, so refresh 今日签到成员 for
	// the actor: the list gained a name and the count moved.
	guild, guildErr := s.currentGuildForSelectedCharacter(session)
	if guildErr != nil || guild == nil {
		return guildErr
	}
	if err := s.sendGameUpperSuccess(
		session,
		uint16(dnfenum.CmdPacketTodayGuildAttendanceDetailInfo),
		buildCurrentGuildAttendanceDetailBody(guild, s.gameplayNow()),
	); err != nil {
		return err
	}
	// The 公会贡献度 grant goes here, not in applyCurrentGuildAttendance: that one
	// runs on the online publish, which can already sit inside a guild critical
	// section, and applyCurrentGuildContribution reaches transactCurrentGuildAssets,
	// which takes the non-reentrant Service.guildMu. mutateCurrentGuild above has
	// released it by this point, so this call is safe.
	s.grantCurrentGuildAttendanceContribution(session)
	return nil
}

// handleCurrentGuildRepresentative moves the per-account 代表角色 flag onto the
// named character. Ported from the upstream guild branch; the request carries the
// member roster's social region byte and a DSTR name, not a character id.
func (s *Service) handleCurrentGuildRepresentative(session *gameSession, body []byte) error {
	const opcode = uint16(dnfenum.CmdPacketReqRepresentCharacter)
	// sub_26A34A0 echoes the member roster's social region, not a channel index.
	if len(body) < 6 || body[0] != s.currentRegionID() {
		return s.sendGameUpperFailure(session, opcode, currentGuildGenericError)
	}
	name, next, valid := parseCurrentGuildDSTR(body, 1)
	if !valid || next != len(body) || name == "" || len(body)-5 > 31 {
		return s.sendGameUpperFailure(session, opcode, currentGuildGenericError)
	}
	identity, current := s.boundGameSessionCharacterSnapshot(session)
	if !current {
		return s.sendGameUpperFailure(session, opcode, currentGuildGenericError)
	}
	accountID := s.accountIDForSession(session)
	ctx, cancel := s.currentGuildContext()
	defer cancel()
	err := s.mutateCurrentGuild(ctx, func(repositories dnfrepo.Group, registry *dnfguild.Registry) error {
		actor, err := selectedGuildCharacter(ctx, repositories, session)
		if err != nil {
			return err
		}
		if actor.AccountID != accountID || !s.boundGameSessionCharacterCurrent(identity) {
			return dnfguild.ErrPermissionDenied
		}
		characters, err := repositories.Character.ListByAccount(ctx, accountID, dnfrepo.MaxCharacterSlotLimit)
		if err != nil {
			return err
		}
		for _, target := range characters {
			if target.Name == name && target.AccountID == accountID {
				return registry.SetRepresentative(actor.CharacterID, accountID, target.CharacterID, s.gameplayNow())
			}
		}
		return dnfguild.ErrCharacterNotFound
	})
	if err != nil {
		s.logGameEvent(session, "game-guild-representative-rejected", "error", err)
		return s.sendGameUpperFailure(session, opcode, currentGuildGenericError)
	}
	s.logGameEvent(session, "game-guild-representative-saved", "name", name)
	// sub_1D24FB0 consumes only success, then requests a fresh op140/op67 list.
	return s.sendGameUpperSuccess(session, opcode, nil)
}

// grantCurrentGuildAttendanceContribution pays the 公会贡献度 side of 签到.//
// 签到 is one of the four PVF-configured contribution activities. This is safe to
// call on a path that already ran registry.Attend: applyCurrentGuildContribution
// re-runs Attend internally and only pays when that call reports the first
// attendance of the day, so the automatic login attendance cannot double-pay.
// Failures are deferred, never propagated: the attendance itself has already been
// acknowledged to the client.
func (s *Service) grantCurrentGuildAttendanceContribution(session *gameSession) {
	updates, err := s.applyCurrentGuildContribution(session, dnfguild.ContributionAttendance)
	if err != nil {
		s.logGameEvent(session, "game-guild-attendance-contribution-deferred", "error", err)
		return
	}
	if err := s.sendCurrentGuildContributionUpdates(session, updates); err != nil {
		s.logGameEvent(session, "game-guild-attendance-contribution-send-failed", "error", err)
	}
}

func (s *Service) handleCurrentGuildAttendanceDetail(session *gameSession, _ []byte) error {
	ctx, cancel := s.currentGuildContext()
	defer cancel()
	var selected *dnfguild.Guild
	err := s.readCurrentGuild(ctx, func(_ dnfrepo.Group, registry dnfguild.Registry) error {
		selected, _, _ = registry.GuildForCharacter(strconv.Itoa(int(session.selectedCharacterID)))
		return nil
	})
	if err != nil {
		return s.sendGameUpperFailure(session, uint16(dnfenum.CmdPacketTodayGuildAttendanceDetailInfo), currentGuildGenericError)
	}
	return s.sendGameUpperSuccess(session, uint16(dnfenum.CmdPacketTodayGuildAttendanceDetailInfo), buildCurrentGuildAttendanceDetailBody(selected, s.gameplayNow()))
}

// currentGuildMileageRowMode decides what op747 carries. "empty" keeps the answer
// always safe; "real" projects the stored 积分 records through the partially
// mapped 48-byte row; a decimal emits that many probe rows with distinctive
// values so the layout can be confirmed from what the window renders. It is read
// per request from DNF90_GUILD_MILEAGE_ROWS or guild_mileage_rows.txt, so the
// layout can be swept without a rebuild.
func currentGuildMileageRowMode() string {
	const key = "DNF90_GUILD_MILEAGE_ROWS"
	raw := strings.TrimSpace(strings.ToLower(os.Getenv(key)))
	return raw
}

// currentGuildMileageRows projects 公会积分's rows. The default is the stored
// history; "empty" is the safety valve and "probe<N>" walks the reason byte so
// the remaining values of that enum can be mapped from one capture. "probe=" with
// a comma separated byte list sweeps exactly those bytes, which is how the newer
// 公会贡献 rules' labels (聊天 10, 组队 12) get checked against the live window:
//
//	probe=2,10,11,12,13,0x0E
//
// The mode is read per request from DNF90_GUILD_MILEAGE_ROWS or
// guild_mileage_rows.txt, so a sweep needs no rebuild.
func currentGuildMileageRows(guild *dnfguild.Guild) [][]byte {
	mode := currentGuildMileageRowMode()
	if mode == "empty" {
		return nil
	}
	if strings.HasPrefix(mode, "probe") {
		if list := strings.TrimSpace(strings.TrimPrefix(mode, "probe")); strings.HasPrefix(list, "=") {
			return currentGuildMileageProbeRows(strings.TrimPrefix(list, "="))
		}
		count, err := strconv.Atoi(strings.TrimPrefix(mode, "probe"))
		if err != nil || count <= 0 {
			count = 8
		}
		if count > 16 {
			count = 16
		}
		rows := make([][]byte, 0, count)
		for index := 0; index < count; index++ {
			rows = append(rows, currentGuildMileageRow(
				uint32(4242+index), fmt.Sprintf("R%d", index), byte(index),
				uint32(111+index), uint32(1700000000)))
		}
		return rows
	}
	if guild == nil {
		return nil
	}
	rows := make([][]byte, 0, len(guild.MileageHistory))
	for _, entry := range guild.MileageHistory {
		rows = append(rows, currentGuildMileageRow(
			uint32(currentGuildNumericID(entry.CharacterID)), entry.Name,
			currentGuildMileageReasonByte(guild, entry),
			uint32(max(entry.Amount, 0)), uint32(entry.CreatedAt.Unix())))
	}
	return rows
}

// currentGuildMileageProbeRows builds one row per reason byte in a comma
// separated list, so the window can be swept for the enum's labels in a single
// look. Values are decimal or 0x-prefixed; the row name spells the byte out.
func currentGuildMileageProbeRows(list string) [][]byte {
	fields := strings.Split(list, ",")
	rows := make([][]byte, 0, len(fields))
	for _, field := range fields {
		field = strings.TrimSpace(field)
		if field == "" {
			continue
		}
		value, err := strconv.ParseUint(field, 0, 8)
		if err != nil {
			continue
		}
		index := len(rows)
		rows = append(rows, currentGuildMileageRow(
			uint32(4242+index), fmt.Sprintf("B%d", value), byte(value),
			uint32(111+index), uint32(1700000000)))
	}
	return rows
}

// handleCurrentGuildMileageHistory answers 公会积分. The body used to be the bare
// success byte, which crashed the client: its handler 0x01D11310 reads a u32
// count after the success byte and then memcpys 48 bytes per row, so it walked
// off the packet into freed memory (0xC0000005 eip 0x03B10747, eax=0x098B101F,
// access 0x098B1000 -- 2026-09-12 00:44:36, the moment the 公会积分 tab was
// clicked).
func (s *Service) handleCurrentGuildMileageHistory(session *gameSession, body []byte) error {
	opcode := uint16(dnfenum.CmdPacketReqGuildMileageHistory)
	// op747 is a pure query with no payload, and this used to ignore the body
	// outright -- the only handler in this module that did. Upstream rejects a
	// non-empty body with the generic error, which is also what every sibling
	// handler here does (op261/op262 and the other list reads), so a malformed
	// request is now refused instead of silently answered with the full 公会积分
	// list.
	if len(body) != 0 {
		return s.sendGameUpperFailure(session, opcode, currentGuildGenericError)
	}
	guild, err := s.currentGuildForSelectedCharacter(session)
	if err != nil {
		s.logGameEvent(session, "game-guild-mileage-history-load-failed", "error", err)
		guild = nil
	}
	// A non-member is refused rather than answered with an empty list, which is
	// what upstream does and what every sibling list read in this module does
	// (op261/op262 included). The well-formed count+rows body above still exists
	// for members -- that part is the crash fix, not a leniency policy.
	if guild == nil {
		return s.sendGameUpperFailure(session, opcode, currentGuildGenericError)
	}
	return s.sendGameUpperSuccess(session, opcode, currentGuildMileageHistoryBody(currentGuildMileageRows(guild)))
}

func (s *Service) handleCurrentGuildChangeGrade(session *gameSession, body []byte) error {
	// op748 is the per-grade permission editor, not a member appointment.
	// The window at 00F9C8A2..00F9CA8A folds its 18 permission checkboxes
	// (sub_3542930 reads ctrl+0x578) into one mask and sends
	// sub_3454EB0(w, 0x2EC) / sub_3454FC0(w, [ebx+0x1d0]) /
	// sub_3455020(w, mask): a u8 grade followed by a u32 mask. This handler
	// used to read a u32 target followed by a u8 grade, so it fed
	// ChangeGrade a target id built out of the grade and the low three mask
	// bytes; that only ever failed because the resulting grade byte was the
	// mask's zero high byte, but it was one 18th bit away from mutating a
	// random member. Appointing 副会长/优秀会员 goes through op126 instead.
	//
	// The class-1 reply handler is sub_1D0EA20 (registered at 01D372B4), not
	// the class-0 sub_1D88700 an earlier note here named: it reads only the
	// success flag and an error byte, showing dstr 0x4BA5 on success and
	// mapping code 34 to dstr 0x9CD9 ("该公会不存在"), which is exactly what
	// rejecting this request used to display. Nothing in the reply body is
	// read; op70 carries the new table back through
	// publishCurrentGuildManagement.
	opcode := uint16(dnfenum.CmdPacketChangeGuildGrade)
	if len(body) != 5 {
		s.logGameEvent(session, "game-guild-grade-permissions-body-unexpected",
			"body", hex.EncodeToString(body), "body_len", len(body))
		return s.sendGameUpperFailure(session, opcode, currentGuildGenericError)
	}
	grade, permissions := body[0], binary.LittleEndian.Uint32(body[1:])
	ctx, cancel := s.currentGuildContext()
	defer cancel()
	err := s.mutateCurrentGuild(ctx, func(_ dnfrepo.Group, registry *dnfguild.Registry) error {
		return registry.SetGradePermissions(strconv.Itoa(int(session.selectedCharacterID)), grade, permissions, s.gameplayNow())
	})
	if err != nil {
		s.logGameEvent(session, "game-guild-grade-permissions-rejected",
			"body", hex.EncodeToString(body), "grade", grade,
			"permissions", permissions, "error", err)
	}
	return s.finishCurrentGuildSettings(session, opcode, err, nil)
}

func (s *Service) handleCurrentGuildChangeGradeName(session *gameSession, body []byte) error {
	if len(body) < 5 {
		return s.sendGameUpperFailure(session, uint16(dnfenum.CmdPacketChangeGuildGradeName), currentGuildGenericError)
	}
	grade := body[0]
	name, next, ok := parseCurrentGuildDSTR(body, 1)
	if !ok || next != len(body) {
		return s.sendGameUpperFailure(session, uint16(dnfenum.CmdPacketChangeGuildGradeName), currentGuildGenericError)
	}
	ctx, cancel := s.currentGuildContext()
	defer cancel()
	err := s.mutateCurrentGuild(ctx, func(_ dnfrepo.Group, registry *dnfguild.Registry) error {
		return registry.SetGradeName(strconv.Itoa(int(session.selectedCharacterID)), grade, name, s.gameplayNow())
	})
	return s.finishCurrentGuildSettings(session, uint16(dnfenum.CmdPacketChangeGuildGradeName), err, nil)
}

func (s *Service) handleCurrentGuildAllGrades(session *gameSession, _ []byte) error {
	return s.handleCurrentGuildGradesForOpcode(session, uint16(dnfenum.CmdPacketGuildAllMemberGrade))
}

func (s *Service) handleCurrentGuildAllGradesNext(session *gameSession, _ []byte) error {
	return s.handleCurrentGuildGradesForOpcode(session, uint16(dnfenum.CmdPacketGuildAllMemberGradeNext))
}

func (s *Service) handleCurrentGuildGradesForOpcode(session *gameSession, opcode uint16) error {
	ctx, cancel := s.currentGuildContext()
	defer cancel()
	var selected *dnfguild.Guild
	err := s.readCurrentGuild(ctx, func(_ dnfrepo.Group, registry dnfguild.Registry) error {
		selected, _, _ = registry.GuildForCharacter(strconv.Itoa(int(session.selectedCharacterID)))
		return nil
	})
	if err != nil {
		return s.sendGameUpperFailure(session, opcode, currentGuildGenericError)
	}
	return s.sendGameUpperSuccess(session, opcode, buildCurrentGuildGradeBody(selected, opcode == uint16(dnfenum.CmdPacketGuildAllMemberGrade)))
}
