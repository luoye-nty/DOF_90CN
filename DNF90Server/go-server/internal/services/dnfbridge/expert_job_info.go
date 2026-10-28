package dnfbridge

import (
	"bytes"
	"context"
	"fmt"
	"math"
	"sort"
	"strconv"
	"strings"
	"time"

	dnfexpertjob "longheng.io/server/internal/modules/dnf/expertjob"
	dnfrepo "longheng.io/server/internal/modules/dnf/repository"
)

// 0x00CD is a local class-0 notification here, distinct from command op205.
// Current NoPack sub_1D89C20 reads state, expert-job mode, recipe count, then
// recipe IDs for alchemist (2) and doll controller (4).
const currentExpertJobInfoNotification uint16 = 0x00CD

// The current compatibility DLL consumes this optional trailer after the
// native op205 body. The native client reader stops after the
// profession-specific fields and leaves trailing bytes untouched.
//
// Wire layout, in this exact order:
//
//	... native body ... | "EJXM" | uint32 mode | "EJXP" | uint32 experience
//
// The mode block must precede the experience block: the DLL locates the
// experience pair by reading the LAST eight bytes of the body, so nothing may
// ever be appended after "EJXP".
const (
	currentExpertJobProjectionTrailerMagic uint32 = 0x50584a45 // "EJXP" on wire
	currentExpertJobProjectionModeMagic    uint32 = 0x4d584a45 // "EJXM" on wire
)

// Projection modes. The native op205 handler sub_1D89C20 is a single
// acquisition-shaped notification: for the enchanter type it always falls
// through to the native qualification dispatch FUN_01a8b960, which is the
// 「副职业资格认证 / 您学会新技能了」 window. The client cannot tell a real
// acquisition from a re-projection of state it already holds, so the server has
// to say which one this is.
//
//   - announce: the character really just acquired/upgraded the profession
//     (expert-job quest completion). The window is wanted.
//   - silent: the body restates durable state that the client is merely
//     rebuilding (login, relogin, scene reconstruction, store refreshes). The
//     window is an artefact and must not be raised.
//
// The body itself stays byte-identical in both modes, so the native reader and
// the scene-rebuild body comparison are unaffected; only the compatibility DLL
// reads the mode block.
const (
	currentExpertJobProjectionAnnounce uint32 = 0
	currentExpertJobProjectionSilent   uint32 = 1
)

func (s *Service) sendCurrentExpertJobInfoForCharacter(session *gameSession, character dnfrepo.CharacterRecord, force bool) error {
	return s.sendCurrentExpertJobInfoForCharacterMode(session, character, force, false)
}

// sendCurrentExpertJobInfoForCharacterAnnouncing publishes the same body as
// sendCurrentExpertJobInfoForCharacter but marks it as a real acquisition, so
// the client raises the native 「副职业资格认证」 window.
//
// Only the expert-job quest completion path may use this. Every other caller is
// restating durable state that the client already holds - a login, a relogin, a
// scene reconstruction or a store refresh - and would otherwise make an existing
// profession look newly acquired.
//
// The actor's mode-0 record is refreshed first: op205's own profession UI is
// gated on the record field only mode 0 writes (sub_322E590 skips everything
// while sub_3A4C1A0(actor)+0x368 is zero), so announcing an acquisition before
// that field changes shows the player nothing until the next relogin.
func (s *Service) sendCurrentExpertJobInfoForCharacterAnnouncing(session *gameSession, character dnfrepo.CharacterRecord, force bool) error {
	ctx, cancel := context.WithTimeout(context.Background(), createWriteTimeout)
	err := s.currentSendExpertJobActorRefresh(ctx, session, character)
	cancel()
	if err != nil {
		s.logGameEvent(session, "game-expert-job-acquisition-actor-refresh-failed",
			"char_id", session.selectedCharacterID,
			"error", err,
			"reason", "announcement_continues_without_the_record_refresh")
	}
	return s.sendCurrentExpertJobInfoForCharacterMode(session, character, force, true)
}

func (s *Service) sendCurrentExpertJobInfoForCharacterMode(session *gameSession, character dnfrepo.CharacterRecord, force bool, announce bool) error {
	rawJobType := character.Stats["expert_job_type"]
	if rawJobType == 0 {
		if !force && session != nil && session.expertJobInfoCharacterID == session.selectedCharacterID {
			return nil
		}
		return s.sendCurrentExpertJobClearedInfo(session, 0)
	}
	if rawJobType < int64(dnfexpertjob.EnchanterType) || rawJobType > int64(dnfexpertjob.DollControllerType) {
		return nil
	}
	jobType := byte(rawJobType)
	experience := character.Stats["expert_job_exp"]
	if experience < 0 {
		experience = 0
	}
	return s.sendCurrentExpertJobInfo(session, jobType, experience, currentExpertJobLearnedRecipes(character.Stats, jobType), character.Stats, force, announce)
}

// sendCurrentExpertJobInfoForSelectedCharacter reloads the durable profession
// state for the selected actor. Actor-recreation paths use force=true because
// expertJobInfoCharacterID only identifies the character, not the native actor
// generation that last consumed op205.
func (s *Service) sendCurrentExpertJobInfoForSelectedCharacter(session *gameSession, force bool) error {
	if s == nil || session == nil || session.selectedCharacterID == 0 {
		return nil
	}
	repositories, ok := s.repositoryGroup()
	if !ok || repositories.Character == nil {
		return dnfexpertjob.ErrOwnerUnavailable
	}
	ctx, cancel := context.WithTimeout(context.Background(), currentRepositorySnapshotTimeout)
	defer cancel()
	characterID := strconv.FormatUint(uint64(session.selectedCharacterID), 10)
	character, found, err := repositories.Character.Load(ctx, characterID)
	if err != nil {
		return err
	}
	if !found || character.CharacterID != characterID {
		return dnfexpertjob.ErrCharacterNotFound
	}
	if accountID := strings.TrimSpace(s.accountIDForSession(session)); accountID != "" &&
		accountID != strings.TrimSpace(character.AccountID) {
		return dnfexpertjob.ErrCharacterNotFound
	}
	return s.sendCurrentExpertJobInfoForCharacter(session, character, force)
}

// currentExpertJobProjection is one exact class-0/op205 body plus the fields
// this package logs for it. Ordinary sends and the scene-rebuild path both
// build through these functions, so the bytes compared against the last
// accepted body can never drift from the bytes actually written.
type currentExpertJobProjection struct {
	body             []byte
	jobType          byte
	experience       int64
	level            int64
	recipeCount      int
	machineGrade     int64
	machineEndurance int64
	disjointer       bool
}

// rememberCurrentExpertJobProjection records the exact body this client just
// accepted. It is the client's authoritative expert-job state as far as the
// server can observe it, and it is what lets a later scene reconstruction skip
// a byte-identical replay.
func (s *Service) rememberCurrentExpertJobProjection(session *gameSession, body []byte) {
	if session == nil || len(body) == 0 {
		return
	}
	session.expertJobProjectionBody = append(session.expertJobProjectionBody[:0], body...)
}

// buildCurrentExpertJobInfoProjection writes the profession-carrying op205
// body. It must stay byte-identical to what the wire previously received:
// callers compare its output against the remembered body.
func (s *Service) buildCurrentExpertJobInfoProjection(stats map[string]int64, jobType byte, experience int64, learned []int64, announce bool) (currentExpertJobProjection, error) {
	projection := currentExpertJobProjection{
		jobType:    jobType,
		experience: experience,
	}
	catalog, err := s.currentExpertJobCatalog()
	if err != nil {
		return projection, err
	}
	config, configured := catalog.Config(jobType)
	if jobType != dnfexpertjob.DisjointerType && !configured {
		return projection, dnfexpertjob.ErrJobUnsupported
	}
	w := packetWriter{}
	// Current EXE sub_1D89C20 stores this byte straight into its native expert-job
	// manager before dispatching on the second-byte profession type. It carries the
	// give-up tier, which drives the escalating gold cost of giving the profession up
	// again, so it is deliberately kept on the wire.
	w.writeByte(currentExpertJobGiveUpState(stats))
	w.writeByte(jobType)
	if jobType == dnfexpertjob.DisjointerType {
		disjointer, ok := catalog.Disjointer()
		if !ok {
			return projection, dnfexpertjob.ErrJobUnsupported
		}
		grade := currentExpertJobStatDefault(stats, currentExpertJobMachineGradeStat, 1)
		endurance := currentExpertJobStatDefault(stats, currentExpertJobMachineEnduranceStat, disjointer.InitialEndurance)
		w.writeInt32(int(grade))
		w.writeInt32(int(endurance))
		writeCurrentExpertJobProjectionTrailer(&w, experience, announce)
		projection.body = w.bytes()
		projection.disjointer = true
		projection.machineGrade = grade
		projection.machineEndurance = endurance
		return projection, nil
	}
	recipes := config.AutoRecipeIDs(experience)
	seen := make(map[int64]struct{}, len(recipes)+len(learned))
	for _, recipeID := range recipes {
		seen[recipeID] = struct{}{}
	}
	for _, recipeID := range learned {
		_, generic := config.Recipes[recipeID]
		card := false
		if jobType == dnfexpertjob.EnchanterType {
			if enchanter, ok := catalog.Enchanter(); ok {
				_, card = enchanter.CardRecipes[recipeID]
			}
		}
		if generic || card {
			seen[recipeID] = struct{}{}
		}
	}
	recipes = recipes[:0]
	for recipeID := range seen {
		recipes = append(recipes, recipeID)
	}
	sort.Slice(recipes, func(i, j int) bool { return recipes[i] < recipes[j] })
	if len(recipes) > math.MaxUint8 {
		return projection, fmt.Errorf("expert job recipe list is too large: type=%d count=%d", jobType, len(recipes))
	}
	w.writeByte(byte(len(recipes)))
	for _, recipeID := range recipes {
		if recipeID <= 0 || recipeID > math.MaxUint32 {
			return projection, fmt.Errorf("expert job recipe id is outside wire range: %d", recipeID)
		}
		w.writeUint32(uint32(recipeID))
	}
	if jobType == dnfexpertjob.EnchanterType {
		enchanter, ok := catalog.Enchanter()
		if !ok {
			return projection, dnfexpertjob.ErrJobUnsupported
		}
		// The sparse look of this list ([0 1 2 3 4 11] at level 11) is correct:
		// it is the card-recipe qualification set, not a level ladder. A panel
		// once showing "Lv4" beside a level-11 character was the character's own
		// level capping the job level, not this list.
		qualifications := enchanter.Qualifications(experience)
		if len(qualifications) > math.MaxUint8 {
			return projection, fmt.Errorf("enchanter qualification list is too large: %d", len(qualifications))
		}
		w.writeByte(byte(len(qualifications)))
		w.writeBytes(qualifications)
		w.writeInt32(config.Level(experience))
		w.writeInt32(int(currentExpertJobStatDefault(stats, currentExpertJobMachineEnduranceStat, enchanter.InitialEndurance)))
	}
	writeCurrentExpertJobProjectionTrailer(&w, experience, announce)
	projection.body = w.bytes()
	projection.level = int64(config.Level(experience))
	projection.recipeCount = len(recipes)
	return projection, nil
}

// sendCurrentExpertJobClearedInfo publishes the two-byte type-zero form read
// by current op205. The compatibility unit mirrors that zero into the current
// actor so the system-menu and self-click gates stop advertising the old job.
func (s *Service) sendCurrentExpertJobClearedInfo(session *gameSession, giveUpState byte) error {
	if s == nil || session == nil || session.selectedCharacterID == 0 {
		return nil
	}
	w := packetWriter{}
	// Same rule as the active form: the give-up tier is kept on the wire.
	w.writeByte(giveUpState)
	// The current client stores this first byte as the expert-job manager's
	// learned/enabled state before it reads the job type. A nonzero type with
	// state zero is treated as an unlearned profession, leaving the system-menu
	// entry disabled and the personal-info profession row empty.
	w.writeByte(0)
	// The cleared form is always a state reconstruction, never an acquisition.
	writeCurrentExpertJobProjectionTrailer(&w, 0, false)
	if err := s.sendGameUpperRawClass(session, currentExpertJobInfoNotification, w.bytes(), 0); err != nil {
		return err
	}
	s.rememberCurrentExpertJobProjection(session, w.bytes())
	session.expertJobInfoCharacterID = session.selectedCharacterID
	s.logPacketEvent("game-current-expert-job-info-cleared",
		"char_id", session.selectedCharacterID,
		"give_up_state", giveUpState,
		"sent_at", time.Now().UTC())
	return nil
}

// sendCurrentExpertJobInfoForSceneRebuild restores the durable expert-job
// state during a scene reconstruction without re-opening the client's
// qualification UI.
//
// On this 90CN client op205 is an acquisition/mutation notification: the
// client only reads it as "nothing changed" when the same actor generation has
// already consumed a full op19 skill snapshot, which scene reconstruction
// deliberately does not replay (see initial_town_route_packets.go and
// town_post_transition_player_state.go). Publishing an unchanged body on such a
// generation therefore makes the client treat an existing auxiliary profession
// as newly acquired - the top-left "副职业资格认证" banner plus the one-shot
// "您学会新技能了" message.
//
// A profession-carrying body is consequently published only when it differs
// from the exact body this client already accepted. The cleared type-zero form
// is not acquisition-shaped and keeps its previous unconditional behaviour.
func (s *Service) sendCurrentExpertJobInfoForSceneRebuild(session *gameSession, character dnfrepo.CharacterRecord, source string) error {
	if s == nil || session == nil || session.selectedCharacterID == 0 {
		return nil
	}
	rawJobType := character.Stats["expert_job_type"]
	if rawJobType < int64(dnfexpertjob.EnchanterType) || rawJobType > int64(dnfexpertjob.DollControllerType) {
		return s.sendCurrentExpertJobInfoForCharacter(session, character, false)
	}
	experience := character.Stats["expert_job_exp"]
	if experience < 0 {
		experience = 0
	}
	jobType := byte(rawJobType)
	projection, err := s.buildCurrentExpertJobInfoProjection(
		character.Stats,
		jobType,
		experience,
		currentExpertJobLearnedRecipes(character.Stats, jobType),
		false,
	)
	if err != nil {
		return err
	}
	if len(session.expertJobProjectionBody) != 0 && bytes.Equal(projection.body, session.expertJobProjectionBody) {
		s.logGameEvent(session, "game-expert-job-scene-rebuild-suppressed",
			"char_id", session.selectedCharacterID,
			"source", source,
			"msg_id", currentExpertJobInfoNotification,
			"job_type", jobType,
			"body_len", len(projection.body),
			"reason", "scene_reconstruction_replayed_unchanged_expert_job_state")
		return nil
	}
	if err := s.sendGameUpperRawClass(session, currentExpertJobInfoNotification, projection.body, 0); err != nil {
		return err
	}
	s.rememberCurrentExpertJobProjection(session, projection.body)
	session.expertJobInfoCharacterID = session.selectedCharacterID
	s.logPacketEvent("game-current-expert-job-info-send",
		"char_id", session.selectedCharacterID,
		"source", source,
		"job_type", jobType,
		"experience", experience,
		"forced", false,
		"scene_rebuild_state_changed", true,
		"sent_at", time.Now().UTC())
	return nil
}

// sendCurrentExpertJobInfoForSceneRebuildForSelectedCharacter is the
// selected-actor entry point of sendCurrentExpertJobInfoForSceneRebuild.
func (s *Service) sendCurrentExpertJobInfoForSceneRebuildForSelectedCharacter(session *gameSession, source string) error {
	if s == nil || session == nil || session.selectedCharacterID == 0 {
		return nil
	}
	repositories, ok := s.repositoryGroup()
	if !ok || repositories.Character == nil {
		return dnfexpertjob.ErrOwnerUnavailable
	}
	ctx, cancel := context.WithTimeout(context.Background(), currentRepositorySnapshotTimeout)
	defer cancel()
	characterID := strconv.FormatUint(uint64(session.selectedCharacterID), 10)
	character, found, err := repositories.Character.Load(ctx, characterID)
	if err != nil {
		return err
	}
	if !found || character.CharacterID != characterID {
		return dnfexpertjob.ErrCharacterNotFound
	}
	if accountID := strings.TrimSpace(s.accountIDForSession(session)); accountID != "" &&
		accountID != strings.TrimSpace(character.AccountID) {
		return dnfexpertjob.ErrCharacterNotFound
	}
	return s.sendCurrentExpertJobInfoForSceneRebuild(session, character, source)
}

func (s *Service) sendCurrentExpertJobInfoFromRepository(session *gameSession, expectedType byte, force bool) error {
	if s == nil || session == nil || session.selectedCharacterID == 0 {
		return nil
	}
	repositories, ok := s.repositoryGroup()
	if !ok || repositories.Character == nil {
		return dnfexpertjob.ErrOwnerUnavailable
	}
	ctx, cancel := context.WithTimeout(context.Background(), currentRepositorySnapshotTimeout)
	defer cancel()
	characterID := strconv.FormatUint(uint64(session.selectedCharacterID), 10)
	character, found, err := repositories.Character.Load(ctx, characterID)
	if err != nil {
		return err
	}
	if !found || character.CharacterID != characterID || character.Stats["expert_job_type"] != int64(expectedType) {
		return dnfexpertjob.ErrCharacterNotFound
	}
	return s.sendCurrentExpertJobInfoForCharacter(session, character, force)
}

func (s *Service) sendCurrentExpertJobInfo(session *gameSession, jobType byte, experience int64, learned []int64, stats map[string]int64, force bool, announce bool) error {
	if s == nil || session == nil || session.selectedCharacterID == 0 {
		return nil
	}
	if !force && session.expertJobInfoCharacterID == session.selectedCharacterID {
		return nil
	}
	projection, err := s.buildCurrentExpertJobInfoProjection(stats, jobType, experience, learned, announce)
	if err != nil {
		return err
	}
	if err := s.sendGameUpperRawClass(session, currentExpertJobInfoNotification, projection.body, 0); err != nil {
		return err
	}
	s.rememberCurrentExpertJobProjection(session, projection.body)
	session.expertJobInfoCharacterID = session.selectedCharacterID
	if projection.disjointer {
		s.logPacketEvent("game-current-expert-job-info-send",
			"char_id", session.selectedCharacterID,
			"job_type", jobType,
			"experience", experience,
			"machine_grade", projection.machineGrade,
			"endurance", projection.machineEndurance,
			"forced", force,
			"announce", announce,
			"sent_at", time.Now().UTC())
		return nil
	}
	s.logPacketEvent("game-current-expert-job-info-send",
		"char_id", session.selectedCharacterID,
		"job_type", jobType,
		"experience", experience,
		"level", projection.level,
		"recipe_count", projection.recipeCount,
		"forced", force,
		"announce", announce,
		"sent_at", time.Now().UTC())
	return nil
}

func writeCurrentExpertJobProjectionTrailer(w *packetWriter, experience int64, announce bool) {
	if w == nil {
		return
	}
	if experience < 0 {
		experience = 0
	} else if experience > math.MaxUint32 {
		experience = math.MaxUint32
	}
	mode := currentExpertJobProjectionSilent
	if announce {
		mode = currentExpertJobProjectionAnnounce
	}
	w.writeUint32(currentExpertJobProjectionModeMagic)
	w.writeUint32(mode)
	w.writeUint32(currentExpertJobProjectionTrailerMagic)
	w.writeUint32(uint32(experience))
}

const (
	currentExpertJobMachineGradeStat     = "expert_job_machine_grade"
	currentExpertJobMachineEnduranceStat = "expert_job_machine_endurance"
)

func currentExpertJobStatDefault(stats map[string]int64, key string, fallback int64) int64 {
	if value, ok := stats[key]; ok && value >= 0 {
		return value
	}
	return fallback
}

func currentExpertJobGiveUpState(stats map[string]int64) byte {
	value := stats[currentExpertJobGiveUpStateStat]
	if value <= 0 {
		return 0
	}
	if value > math.MaxUint8 {
		return math.MaxUint8
	}
	return byte(value)
}

func currentExpertJobRecipeStatKey(jobType byte, recipeID int64) string {
	return "expert_job_recipe_" + strconv.Itoa(int(jobType)) + "_" + strconv.FormatInt(recipeID, 10)
}

func currentExpertJobLearnedRecipes(stats map[string]int64, jobType byte) []int64 {
	prefix := "expert_job_recipe_" + strconv.Itoa(int(jobType)) + "_"
	result := make([]int64, 0)
	for key, value := range stats {
		if value == 0 || !strings.HasPrefix(key, prefix) {
			continue
		}
		recipeID, err := strconv.ParseInt(strings.TrimPrefix(key, prefix), 10, 64)
		if err == nil && recipeID > 0 {
			result = append(result, recipeID)
		}
	}
	sort.Slice(result, func(i, j int) bool { return result[i] < result[j] })
	return result
}
