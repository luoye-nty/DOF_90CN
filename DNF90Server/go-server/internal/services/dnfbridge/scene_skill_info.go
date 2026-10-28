package dnfbridge

import (
	"context"
	"fmt"
	"sort"
	"strings"

	darkcombo "longheng.io/server/internal/modules/dnf/darkknightcombo"
	"longheng.io/server/internal/modules/dnf/dnfenum"
	dnfrepo "longheng.io/server/internal/modules/dnf/repository"
	"longheng.io/server/internal/modules/dnf/skillcmd"
)

type currentSceneSkillInfoProjection struct {
	body            []byte
	characterID     string
	job             byte
	learnedCount    int
	learnedSkillIDs []int64
	activeSlots     []int
	quickSkillIDs   []uint16
	activeTree      byte
}

func (s *Service) buildCurrentSceneSkillInfoProjection(
	session *gameSession,
	ctx context.Context,
	character dnfrepo.CharacterRecord,
	source string,
) (currentSceneSkillInfoProjection, bool, error) {
	if s == nil || session == nil {
		return currentSceneSkillInfoProjection{}, false, nil
	}
	repos, ok := s.repositoryGroup()
	if !ok || repos.Skill == nil {
		s.logGameEvent(session, "game-main-skill-info-deferred", "source", source, "reason", "skill_repository_missing")
		return currentSceneSkillInfoProjection{}, false, nil
	}
	job, ok := characterJobByte(character)
	if !ok {
		return currentSceneSkillInfoProjection{}, false, fmt.Errorf("current scene skill info has invalid job %q", character.Job)
	}
	record, backfilled, err := s.loadOrBackfillCurrentSceneSkillRecord(ctx, repos, character, job)
	if err != nil {
		return currentSceneSkillInfoProjection{}, false, fmt.Errorf("load current scene skill info: %w", err)
	}
	s.initialSkillsMu.Lock()
	catalog := s.skillCatalog
	s.initialSkillsMu.Unlock()
	if catalog == nil {
		s.logGameEvent(session, "game-main-skill-info-deferred", "source", source, "reason", "skill_catalog_missing")
		return currentSceneSkillInfoProjection{}, false, nil
	}
	layoutBackfilled := false
	for tree := 0; tree < currentSkillInfoTreeCount; tree++ {
		var changed bool
		record, _, changed, err = s.ensureCurrentSceneSkillLayout(ctx, repos, character, job, tree, catalog, record)
		if err != nil {
			return currentSceneSkillInfoProjection{}, false, fmt.Errorf("build current scene skill layout %d: %w", tree, err)
		}
		layoutBackfilled = layoutBackfilled || changed
	}
	if backfilled {
		s.logGameEvent(session, "game-main-skill-info-backfilled",
			"source", source,
			"character_id", character.CharacterID,
			"job", job,
			"learned_count", len(record.Skills),
			"layout_count", len(record.Layouts),
			"record_source", "runtime_pvf_character_initial_value_skill")
	}
	if layoutBackfilled {
		s.logGameEvent(session, "game-main-skill-layout-backfilled",
			"source", source,
			"character_id", character.CharacterID,
			"job", job,
			"layout_count", len(record.Layouts),
			"layout_source", "job_scoped_pvf_initial_skill_layout")
	}
	record = dnfrepo.CloneSkill(record)
	if record.Layouts == nil {
		record.Layouts = make(map[int]dnfrepo.SkillLayout)
	}
	var darkKnightComboPages []darkcombo.Page
	if job == darkcombo.Job {
		pages, comboErr := s.loadDarkKnightComboPages(ctx, repos, character, catalog, record)
		if comboErr != nil {
			return currentSceneSkillInfoProjection{}, false, fmt.Errorf("load dark knight combo projection: %w", comboErr)
		}
		darkKnightComboPages = pages
	}
	activeTree, err := currentSceneActiveSkillTree(character)
	if err != nil {
		return currentSceneSkillInfoProjection{}, false, err
	}
	body, activeSlots, err := buildCurrentSceneSkillInfoBodyWithDarkKnightCombo(record, record.Layouts, activeTree, darkKnightComboPages)
	if err != nil {
		return currentSceneSkillInfoProjection{}, false, err
	}
	activeState, found := dnfrepo.SkillTree(record, int(activeTree))
	if !found {
		return currentSceneSkillInfoProjection{}, false, fmt.Errorf("current scene active skill tree %d is missing", activeTree)
	}
	learnedSkillIDs := make([]int64, 0, len(activeState.Skills))
	for skillID, state := range activeState.Skills {
		if state.Enabled && state.Level > 0 {
			learnedSkillIDs = append(learnedSkillIDs, skillID)
		}
	}
	sort.Slice(learnedSkillIDs, func(left, right int) bool {
		return learnedSkillIDs[left] < learnedSkillIDs[right]
	})
	quickSkillIDs := make([]uint16, 0, len(activeSlots))
	activeLayout := record.Layouts[int(activeTree)]
	for _, slot := range activeSlots {
		quickSkillIDs = append(quickSkillIDs, activeLayout[slot])
	}
	return currentSceneSkillInfoProjection{
		body:            append([]byte(nil), body...),
		characterID:     character.CharacterID,
		job:             job,
		learnedCount:    len(activeState.Skills),
		learnedSkillIDs: append([]int64(nil), learnedSkillIDs...),
		activeSlots:     append([]int(nil), activeSlots...),
		quickSkillIDs:   append([]uint16(nil), quickSkillIDs...),
		activeTree:      activeTree,
	}, true, nil
}

func (s *Service) sendCurrentSceneSkillInfoProjection(
	session *gameSession,
	projection currentSceneSkillInfoProjection,
	source string,
) error {
	s.logGameEvent(session, "game-main-skill-info-send",
		"source", source,
		"character_id", projection.characterID,
		"job", projection.job,
		"msg_id", currentSkillInfoMsgID,
		"classification", 0,
		"body_len", len(projection.body),
		"learned_count", projection.learnedCount,
		"learned_skill_ids", projection.learnedSkillIDs,
		"active_quick_slots", projection.activeSlots,
		"active_quick_skill_ids", projection.quickSkillIDs,
		"tree_count", currentSkillInfoTreeCount,
		"active_tree", projection.activeTree,
		"wire_level_encoding", "persisted_level_direct",
		"body_source", "current_exe_sub_1D6E240_length_prefixed_DNFPB_skillinfo")
	if err := s.sendGameUpperRawClass(session, currentSkillInfoMsgID, projection.body, 0); err != nil {
		return err
	}
	return nil
}

func (s *Service) sendCurrentSceneSkillInfo(session *gameSession, ctx context.Context, character dnfrepo.CharacterRecord, source string) error {
	projection, ok, err := s.buildCurrentSceneSkillInfoProjection(session, ctx, character, source)
	if err != nil || !ok {
		return err
	}
	return s.sendCurrentSceneSkillInfoProjectionWithTreeRestore(session, projection, source)
}

func currentSceneActiveSkillTree(character dnfrepo.CharacterRecord) (byte, error) {
	value, ok := character.Stats["skill_tree_index"]
	if !ok {
		return currentSkillInfoTreeIndex, nil
	}
	if value < 0 || value >= currentSkillInfoTreeCount {
		return 0, fmt.Errorf("character %s has invalid active skill tree %d", character.CharacterID, value)
	}
	return byte(value), nil
}

// buildCurrentSceneSkillInfoRuntimeBody is the single formatter boundary for
// already-committed skill snapshots outside the normal scene-build path.
// Dark Knight roots require their persisted page children in op19 field 4;
// using the generic builder after a level/quest mutation would recreate an
// ordinary manager and silently drop the ASDFGH combat chains.
func (s *Service) buildCurrentSceneSkillInfoRuntimeBody(
	ctx context.Context,
	character dnfrepo.CharacterRecord,
	record dnfrepo.SkillRecord,
	job byte,
	activeTree byte,
) ([]byte, []int, error) {
	if job != darkcombo.Job {
		return buildCurrentSceneSkillInfoBody(record, record.Layouts, activeTree)
	}
	repos, ok := s.repositoryGroup()
	if !ok || repos.Settings == nil {
		return nil, nil, fmt.Errorf("dark knight combo runtime repository is unavailable")
	}
	s.initialSkillsMu.Lock()
	catalog := s.skillCatalog
	s.initialSkillsMu.Unlock()
	if catalog == nil {
		return nil, nil, fmt.Errorf("dark knight combo runtime catalog is unavailable")
	}
	pages, err := s.loadDarkKnightComboPages(ctx, repos, character, catalog, record)
	if err != nil {
		return nil, nil, fmt.Errorf("load dark knight combo runtime pages: %w", err)
	}
	return buildCurrentSceneSkillInfoBodyWithDarkKnightCombo(record, record.Layouts, activeTree, pages)
}

func (s *Service) sendCurrentSceneSkillInfoProjectionWithTreeRestore(
	session *gameSession,
	projection currentSceneSkillInfoProjection,
	source string,
) error {
	if err := s.sendCurrentSceneSkillInfoProjectionWithTreeRestoreOnly(session, projection, source); err != nil {
		return err
	}
	if projection.job != darkcombo.Job {
		return nil
	}
	if session == nil || projection.characterID != selectedCharacterIDString(session) {
		return fmt.Errorf(
			"dark knight combo projection character mismatch after op19: projection=%s selected=%s",
			projection.characterID,
			selectedCharacterIDString(session),
		)
	}
	// Stable actor refreshes replace the current client's ordinary skill
	// manager, so restore the independent combo pages after the op19/tree
	// projection.  The native op510 auto-combination transaction is the one
	// deliberate exception: it must use the *Only helper below because the
	// client itself writes the new page with op508 after the reset ACK.
	return s.sendSelectedDarkKnightComboInitialization(session, source+"_after_op19_before_hud_commit")
}

// sendCurrentSceneSkillInfoProjectionWithTreeRestoreOnly writes only the
// ordinary skill snapshot and optional active-tree ACK.  Keeping this boundary
// explicit prevents an unrelated class-0/op447 projection from being inserted
// into the native op510 -> op508 auto-combination transaction.
func (s *Service) sendCurrentSceneSkillInfoProjectionWithTreeRestoreOnly(
	session *gameSession,
	projection currentSceneSkillInfoProjection,
	source string,
) error {
	if err := s.sendCurrentSceneSkillInfoProjection(session, projection, source); err != nil {
		return err
	}
	if projection.activeTree != currentSkillInfoTreeIndex {
		body, err := skillcmd.BuildChangeAnotherSkillTreeSuccess(skillcmd.TreeSwitchMutationResult{
			Current: projection.activeTree ^ 1,
			Target:  projection.activeTree,
		})
		if err != nil {
			return err
		}
		s.logGameEvent(session, "game-main-skill-tree-selection-restore-send",
			"source", source,
			"character_id", projection.characterID,
			"msg_id", uint16(dnfenum.CmdPacketChangeAnotherSkillTree),
			"classification", 1,
			"active_tree", projection.activeTree,
			"body_source", "current_exe_sub_1D0C8F0_success_target_after_final_op19")
		if err := s.sendGameUpperRawClass(session, uint16(dnfenum.CmdPacketChangeAnotherSkillTree), body, 1); err != nil {
			return err
		}
	}
	return nil
}

// sendInitialTownSkillInfoBeforeTransition installs the selected actor's skill
// projection exactly once after the final town actor bind and before op24.
// Retain the projection only as a route invariant until op24 commits; sending
// the same snapshot again after op24 makes the current client treat
// already-owned qualification skills as newly learned skills.
func (s *Service) sendInitialTownSkillInfoBeforeTransition(
	session *gameSession,
	ctx context.Context,
	character dnfrepo.CharacterRecord,
	source string,
) error {
	projection, ready, err := s.buildCurrentSceneSkillInfoProjection(session, ctx, character, source)
	if err != nil || !ready {
		return err
	}
	if err := s.sendInitialTownSkillInfoProjectionBeforeTransition(session, projection, source); err != nil {
		return err
	}
	// 修炼道场：装完普通技能快照后再推道场自己的两页（op1230）+ 模式（op1229）。
	return s.sendCurrentPVPDojoSkillsBeforeTransition(ctx, session, character)
}

// sendCurrentSceneSkillInfoProjectionBeforeEquipment creates the selected
// character's native skill manager before an equipment-bearing mode1 packet.
// On a cross-character selection the current client otherwise validates the
// new character's weapon against the previous character's mastery skills and
// queues an automatic unequip request. This preload deliberately sends only
// op19: mode1 can replace the manager, so the ordinary post-mode1 op19 remains
// the authoritative skill/quickbar snapshot and owns op1428 tree restoration.
func (s *Service) sendCurrentSceneSkillInfoProjectionBeforeEquipment(
	session *gameSession,
	projection currentSceneSkillInfoProjection,
	source string,
) error {
	s.logGameEvent(session, "game-main-skill-info-equipment-validation-preload-send",
		"source", source,
		"character_id", projection.characterID,
		"job", projection.job,
		"msg_id", currentSkillInfoMsgID,
		"classification", 0,
		"body_len", len(projection.body),
		"learned_count", projection.learnedCount,
		"tree_count", currentSkillInfoTreeCount,
		"body_source", "same_selected_character_op19_as_post_mode1_without_buff_ui_side_effects")
	return s.sendGameUpperRawClass(session, currentSkillInfoMsgID, projection.body, 0)
}

// sendInitialTownSkillInfoProjectionBeforeTransition installs an already-built
// projection at the accepted pre-op24 boundary. The initial-town route builds
// the projection once so the weapon-validation preload and this authoritative
// write carry the identical selected-character snapshot.
func (s *Service) sendInitialTownSkillInfoProjectionBeforeTransition(
	session *gameSession,
	projection currentSceneSkillInfoProjection,
	source string,
) error {
	if err := s.sendCurrentSceneSkillInfoProjectionWithTreeRestore(session, projection, source); err != nil {
		return err
	}
	session.initialTownSkillInfo = projection
	session.initialTownSkillInfoPrepared = true
	session.initialTownSkillInfoSent = true
	s.logGameEvent(session, "game-initial-town-skill-info-installed-before-op24",
		"source", source,
		"character_id", projection.characterID,
		"learned_count", projection.learnedCount,
		"active_quick_slots", projection.activeSlots)
	return nil
}

// commitPreparedInitialTownSkillInfoAfterTransition consumes the initial-town
// route invariant after op24. The authoritative op19 was already installed
// after the final actor bind but before op24, so no second op19 is sent here. The current
// client completes that typed scene transition by rebuilding the Dark Knight
// combo manager, so the independent native op447 pages are restored here.
// Movement and dungeon-return routes retain their independent post-transition
// skill reload.
func (s *Service) commitPreparedInitialTownSkillInfoAfterTransition(
	session *gameSession,
	source string,
) error {
	if session == nil || !session.initialTownSkillInfoPrepared {
		return nil
	}
	projection := session.initialTownSkillInfo
	if projection.characterID == "" || len(projection.body) == 0 {
		return fmt.Errorf("initial town skill projection is incomplete after op24")
	}
	if projection.characterID != fmt.Sprintf("%d", session.selectedCharacterID) {
		return fmt.Errorf(
			"initial town skill projection character mismatch: projection=%s selected=%d",
			projection.characterID,
			session.selectedCharacterID,
		)
	}
	if err := s.sendSelectedDarkKnightComboInitialization(session, source+"_after_typed_op24"); err != nil {
		return err
	}
	session.initialTownSkillInfoPrepared = false
	session.initialTownSkillInfoSent = true
	session.initialTownSkillInfo = currentSceneSkillInfoProjection{}
	s.logGameEvent(session, "game-initial-town-skill-info-committed-without-op24-replay",
		"source", source,
		"character_id", projection.characterID,
		"learned_count", projection.learnedCount,
		"active_quick_slots", projection.activeSlots,
		"reason", "current_client_treats_duplicate_initial_op19_as_new_skill_acquisition_and_requires_native_dark_knight_restore_after_typed_transition")
	return nil
}

func (s *Service) selectedActorCurrentSceneSkillInfoProjection(
	session *gameSession,
	source string,
) (currentSceneSkillInfoProjection, bool, error) {
	if session == nil || session.selectedCharacterID == 0 {
		return currentSceneSkillInfoProjection{}, false, nil
	}
	ctx, cancel := context.WithTimeout(context.Background(), createWriteTimeout)
	defer cancel()
	repos, ok := s.repositoryGroup()
	if !ok || repos.Character == nil {
		return currentSceneSkillInfoProjection{}, false, fmt.Errorf("selected actor skill projection could not load character repository")
	}
	characterID := fmt.Sprintf("%d", session.selectedCharacterID)
	character, found, err := repos.Character.Load(ctx, characterID)
	if err != nil {
		return currentSceneSkillInfoProjection{}, false, fmt.Errorf("load selected actor %s for skill projection: %w", characterID, err)
	}
	if !found {
		return currentSceneSkillInfoProjection{}, false, fmt.Errorf("selected actor %s for skill projection was not found", characterID)
	}
	if accountID := strings.TrimSpace(s.accountIDForSession(session)); accountID != "" &&
		accountID != strings.TrimSpace(character.AccountID) {
		return currentSceneSkillInfoProjection{}, false, fmt.Errorf("selected actor %s skill projection owner mismatch", characterID)
	}
	projection, ready, err := s.buildCurrentSceneSkillInfoProjection(
		session,
		ctx,
		character,
		source,
	)
	if err != nil {
		return currentSceneSkillInfoProjection{}, false, err
	}
	if !ready {
		return currentSceneSkillInfoProjection{}, false, fmt.Errorf("selected actor %s skill projection is unavailable", characterID)
	}
	return projection, true, nil
}

// sendSelectedActorCurrentSceneSkillInfo reloads the selected character before
// projecting op19. Existing-actor mode0/mode1 rebinds can replace the native
// skill manager, so callers must invoke this only after the final actor rebind,
// never from the DLL or before the repository-backed appearance packets.
func (s *Service) sendSelectedActorCurrentSceneSkillInfo(session *gameSession, source string) error {
	// 修炼道场：在道场里 op19 会把演员技能清空，所以改推道场自己的两页（op1231）。
	dojoSkills, dojoErr := s.currentPVPDojoSkillsActive(session)
	if dojoErr != nil {
		return dojoErr
	}
	if dojoSkills {
		ctx, cancel := context.WithTimeout(context.Background(), createWriteTimeout)
		defer cancel()
		repos, _ := s.repositoryGroup()
		record, err := skillcmd.PrepareDojoSkills(ctx, repos, fmt.Sprint(session.selectedCharacterID))
		if err != nil {
			return err
		}
		body, _, err := buildCurrentSceneSkillInfoBody(record, record.Layouts, 0)
		if err != nil {
			return err
		}
		// sub_10982E0 updates the dojo manager; ordinary op19 clears actor skills.
		return s.sendGameUpperRawClass(session, 1231, body, 0)
	}
	projection, ready, err := s.selectedActorCurrentSceneSkillInfoProjection(session, source)
	if err != nil || !ready {
		return err
	}
	return s.sendCurrentSceneSkillInfoProjectionWithTreeRestore(session, projection, source)
}

// sendSelectedActorCurrentSceneSkillInfoOnly is reserved for protocol flows
// that own their subsequent Dark Knight combo mutation.  In particular op510
// cleans ordinary shortcut duplicates, installs this op19 projection, receives
// its ACK, and then lets the client submit the generated page through op508.
func (s *Service) sendSelectedActorCurrentSceneSkillInfoOnly(session *gameSession, source string) error {
	projection, ready, err := s.selectedActorCurrentSceneSkillInfoProjection(session, source)
	if err != nil || !ready {
		return err
	}
	return s.sendCurrentSceneSkillInfoProjectionWithTreeRestoreOnly(session, projection, source)
}
