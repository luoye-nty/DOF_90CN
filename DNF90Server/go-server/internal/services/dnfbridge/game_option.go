package dnfbridge

import (
	"bytes"
	"context"
	"encoding/base64"
	"encoding/binary"
	"fmt"
	"strconv"
	"strings"
	"time"

	"longheng.io/server/internal/modules/dnf/dnfenum"
	dnfrepo "longheng.io/server/internal/modules/dnf/repository"
)

const (
	currentGameOptionMainBodySize                   = 178
	currentGameOptionQuickChatBodySize              = 300
	currentGameOptionSaveHeaderLen                  = 4
	currentGameOptionQuickChatSaveBodySize          = 1 + currentGameOptionSaveHeaderLen + currentGameOptionQuickChatBodySize
	currentGameOptionSaveBodySize                   = currentGameOptionSaveHeaderLen + currentGameOptionMainBodySize
	currentGameOptionValueKey                       = "main_body_base64"
	currentGameOptionKeymapValueKey                 = "keymap_body_base64"
	currentGameOptionQuickChat0ValueKey             = "quickchat_0_body_base64"
	currentGameOptionQuickChat1ValueKey             = "quickchat_1_body_base64"
	currentGameOptionKeymapMaxBodySize              = 4096
	currentGameOptionMsgID                   uint16 = 0x00ad
	currentGameOptionKeymapMsgID             uint16 = 0x01c6
	currentGameOptionKeymapMode                     = byte(0)
	currentGameOptionRapidFireKeymapID              = "I01"
	currentGameOptionRapidFireIndex                 = 50
	currentGameOptionRapidFireOffset                = currentGameOptionRapidFireIndex * 2
	currentGameOptionNativeApplyGracePeriod         = 2 * time.Second
	currentGameOptionJobAwakenEffectIndex           = 1
	currentGameOptionJobAwakenEffectOffset          = currentGameOptionJobAwakenEffectIndex * 2
	currentGameOptionJobAwakenEffectBit      byte   = 0x02
	currentGameOptionVisibleGrowAvatarIndex         = 74
	currentGameOptionVisibleGrowAvatarOffset        = currentGameOptionVisibleGrowAvatarIndex * 2
	currentGameOptionHideGrowAvatarBit       byte   = 0x10
	currentGameOptionShowAllAvatarIndex             = 55
	currentGameOptionShowAllAvatarOffset            = currentGameOptionShowAllAvatarIndex * 2
	currentGameOptionHideAllAvatarBit        byte   = 0x08
	currentGameOptionUserStateStat                  = "user_state_bits"
)

func currentGameOptionScope(accountID string) string {
	return "account:" + strings.TrimSpace(accountID) + ":game_option"
}

func validCurrentGameOptionSaveBody(body []byte) bool {
	return len(body) == currentGameOptionSaveBodySize &&
		binary.LittleEndian.Uint32(body[:currentGameOptionSaveHeaderLen]) == currentGameOptionMainBodySize
}

func validCurrentGameOptionMainBody(body []byte) bool {
	return len(body) == currentGameOptionMainBodySize
}

func validCurrentGameOptionKeymapBody(body []byte) bool {
	return len(body) > 0 && len(body) <= currentGameOptionKeymapMaxBodySize
}

func validCurrentGameOptionKeymapSaveBody(body []byte) bool {
	return len(body) > currentGameOptionSaveHeaderLen &&
		len(body) <= currentGameOptionSaveHeaderLen+currentGameOptionKeymapMaxBodySize &&
		binary.LittleEndian.Uint32(body[:currentGameOptionSaveHeaderLen]) == uint32(len(body)-currentGameOptionSaveHeaderLen)
}

// currentGameOptionJobAwakenEffectEnabled reads zero-based type197 option 1.
// Live current-EXE saves proved that this word follows the setting checkbox.
func currentGameOptionJobAwakenEffectEnabled(main []byte) (bool, bool) {
	if !validCurrentGameOptionMainBody(main) {
		return false, false
	}
	return binary.LittleEndian.Uint16(
		main[currentGameOptionJobAwakenEffectOffset:currentGameOptionJobAwakenEffectOffset+2],
	) != 0, true
}

func currentGameOptionJobAwakenEffectUserState(current byte, enabled bool) byte {
	if enabled {
		return current | currentGameOptionJobAwakenEffectBit
	}
	return current &^ currentGameOptionJobAwakenEffectBit
}

// currentGameOptionVisibleGrowAvatarEnabled reads zero-based type197 option 74.
// The corresponding user-state bit is inverted: bit4 means hide 觉醒装扮.
func currentGameOptionVisibleGrowAvatarEnabled(main []byte) (bool, bool) {
	if !validCurrentGameOptionMainBody(main) {
		return false, false
	}
	return binary.LittleEndian.Uint16(
		main[currentGameOptionVisibleGrowAvatarOffset:currentGameOptionVisibleGrowAvatarOffset+2],
	) != 0, true
}

func currentGameOptionGrowAvatarUserState(current byte, visible bool) byte {
	if visible {
		return current &^ currentGameOptionHideGrowAvatarBit
	}
	return current | currentGameOptionHideGrowAvatarBit
}

// currentGameOptionShowAllAvatarEnabled reads zero-based type197 option 55.
// The corresponding visibility bit is inverted: bit 3 means hide all avatars.
func currentGameOptionShowAllAvatarEnabled(main []byte) (bool, bool) {
	if !validCurrentGameOptionMainBody(main) {
		return false, false
	}
	return binary.LittleEndian.Uint16(
		main[currentGameOptionShowAllAvatarOffset:currentGameOptionShowAllAvatarOffset+2],
	) != 0, true
}

// currentGameOptionRapidFireEnabled reads zero-based type197 option 50.
// Three independent live current-EXE saves toggled 普通攻击连发 and changed
// only this u16 (0 <-> 1). I01 in type198 is the ordinary-attack binding; it
// is necessary to arm rapid fire, but is not the persisted on/off switch.
func currentGameOptionRapidFireEnabled(main []byte) (bool, bool) {
	if !validCurrentGameOptionMainBody(main) {
		return false, false
	}
	return binary.LittleEndian.Uint16(
		main[currentGameOptionRapidFireOffset:currentGameOptionRapidFireOffset+2],
	) != 0, true
}

func currentGameOptionVisibilityUserState(current byte, advanced, visibleGrowAvatar, showAllAvatar bool) byte {
	current = currentGameOptionJobAwakenEffectUserState(current, advanced)
	current = currentGameOptionGrowAvatarUserState(current, visibleGrowAvatar)
	if showAllAvatar {
		return current &^ currentGameOptionHideAllAvatarBit
	}
	return current | currentGameOptionHideAllAvatarBit
}

type currentGameOptionState struct {
	main      []byte
	keymap    []byte
	quickChat [2][]byte
}

func decodeCurrentGameOptionValue(record dnfrepo.SettingsRecord, key string, valid func([]byte) bool) ([]byte, bool) {
	encoded := strings.TrimSpace(record.Values[key])
	if encoded == "" {
		return nil, false
	}
	body, err := base64.StdEncoding.DecodeString(encoded)
	if err != nil || !valid(body) {
		return nil, false
	}
	return append([]byte(nil), body...), true
}

func loadCurrentGameOptionState(
	ctx context.Context,
	repository dnfrepo.SettingsRepository,
	accountID string,
) (currentGameOptionState, bool, error) {
	if repository == nil || strings.TrimSpace(accountID) == "" {
		return currentGameOptionState{}, false, nil
	}
	record, found, err := repository.Load(ctx, currentGameOptionScope(accountID))
	if err != nil || !found {
		return currentGameOptionState{}, found, err
	}
	main, mainFound := decodeCurrentGameOptionValue(record, currentGameOptionValueKey, validCurrentGameOptionMainBody)
	keymap, keymapFound := decodeCurrentGameOptionValue(record, currentGameOptionKeymapValueKey, validCurrentGameOptionKeymapBody)
	quickChat0, quickChat0Found := decodeCurrentGameOptionValue(record, currentGameOptionQuickChat0ValueKey, validCurrentGameOptionQuickChatBody)
	quickChat1, quickChat1Found := decodeCurrentGameOptionValue(record, currentGameOptionQuickChat1ValueKey, validCurrentGameOptionQuickChatBody)
	return currentGameOptionState{
		main:      main,
		keymap:    keymap,
		quickChat: [2][]byte{quickChat0, quickChat1},
	}, mainFound || keymapFound || quickChat0Found || quickChat1Found, nil
}

func loadCurrentGameOption(
	ctx context.Context,
	repository dnfrepo.SettingsRepository,
	accountID string,
) ([]byte, bool, error) {
	state, _, err := loadCurrentGameOptionState(ctx, repository, accountID)
	return state.main, len(state.main) != 0, err
}

func loadCurrentGameOptionKeymap(
	ctx context.Context,
	repository dnfrepo.SettingsRepository,
	accountID string,
) ([]byte, bool, error) {
	state, _, err := loadCurrentGameOptionState(ctx, repository, accountID)
	return state.keymap, len(state.keymap) != 0, err
}

func saveCurrentGameOptionValue(
	ctx context.Context,
	repository dnfrepo.SettingsRepository,
	accountID string,
	key string,
	body []byte,
) error {
	if repository == nil || strings.TrimSpace(accountID) == "" || strings.TrimSpace(key) == "" {
		return fmt.Errorf("current game option is invalid")
	}
	return saveCurrentGameOptionScopedValue(ctx, repository, currentGameOptionScope(accountID), key, body)
}

func saveCurrentGameOptionScopedValue(
	ctx context.Context,
	repository dnfrepo.SettingsRepository,
	scope string,
	key string,
	body []byte,
) error {
	if repository == nil || strings.TrimSpace(scope) == "" || strings.TrimSpace(key) == "" {
		return fmt.Errorf("current game option scope is invalid")
	}
	record, found, err := repository.Load(ctx, scope)
	if err != nil {
		return err
	}
	if !found {
		record = dnfrepo.SettingsRecord{Scope: scope}
	} else {
		record = dnfrepo.CloneSettings(record)
	}
	if record.Values == nil {
		record.Values = make(map[string]string)
	}
	record.Values[key] = base64.StdEncoding.EncodeToString(body)
	record.UpdatedAt = time.Now().UTC()
	return repository.Save(ctx, record)
}

func saveCurrentGameOption(
	ctx context.Context,
	repository dnfrepo.SettingsRepository,
	accountID string,
	body []byte,
) error {
	if repository == nil || strings.TrimSpace(accountID) == "" || !validCurrentGameOptionMainBody(body) {
		return fmt.Errorf("current game option is invalid")
	}
	return saveCurrentGameOptionValue(ctx, repository, accountID, currentGameOptionValueKey, body)
}

func saveCurrentGameOptionKeymap(
	ctx context.Context,
	repository dnfrepo.SettingsRepository,
	accountID string,
	body []byte,
) error {
	if repository == nil || strings.TrimSpace(accountID) == "" || !validCurrentGameOptionKeymapBody(body) {
		return fmt.Errorf("current game option keymap is invalid")
	}
	return saveCurrentGameOptionValue(ctx, repository, accountID, currentGameOptionKeymapValueKey, body)
}

func validCurrentGameOptionQuickChatBody(body []byte) bool {
	return len(body) == currentGameOptionQuickChatBodySize
}

func validCurrentGameOptionQuickChatSaveBody(body []byte) bool {
	return len(body) == currentGameOptionQuickChatSaveBodySize &&
		body[0] < 2 &&
		binary.LittleEndian.Uint32(body[1:1+currentGameOptionSaveHeaderLen]) == currentGameOptionQuickChatBodySize
}

func currentGameOptionQuickChatValueKey(slot byte) string {
	if slot == 0 {
		return currentGameOptionQuickChat0ValueKey
	}
	return currentGameOptionQuickChat1ValueKey
}

func saveCurrentGameOptionQuickChat(
	ctx context.Context,
	repository dnfrepo.SettingsRepository,
	accountID string,
	slot byte,
	body []byte,
) error {
	if repository == nil || strings.TrimSpace(accountID) == "" || slot >= 2 || !validCurrentGameOptionQuickChatBody(body) {
		return fmt.Errorf("current game option quick chat is invalid")
	}
	return saveCurrentGameOptionValue(ctx, repository, accountID, currentGameOptionQuickChatValueKey(slot), body)
}

// reconcileCurrentGameOptionVisibilityForSelectedCharacter makes the
// account-scoped visibility options authoritative before USERINFO creates a
// newly selected actor. Without this step, another character can retain its
// old/default user_state_bits even though op173 restores the checkbox itself.
func (s *Service) reconcileCurrentGameOptionVisibilityForSelectedCharacter(
	ctx context.Context,
	session *gameSession,
	character dnfrepo.CharacterRecord,
) (dnfrepo.CharacterRecord, bool, error) {
	if s == nil || session == nil || strings.TrimSpace(character.CharacterID) == "" {
		return character, false, nil
	}
	repositories, ok := s.repositoryGroup()
	if !ok || repositories.Settings == nil || repositories.Character == nil {
		return character, false, nil
	}
	accountID := strings.TrimSpace(s.accountIDForSession(session))
	if accountID == "" {
		return character, false, nil
	}
	if owner := strings.TrimSpace(character.AccountID); owner != "" && owner != accountID {
		return character, false, fmt.Errorf("selected character %s owner mismatch", character.CharacterID)
	}
	main, found, err := loadCurrentGameOption(ctx, repositories.Settings, accountID)
	if err != nil || !found {
		return character, false, err
	}
	advanced, valid := currentGameOptionJobAwakenEffectEnabled(main)
	if !valid {
		return character, false, fmt.Errorf("stored current game option job/awakening effect is invalid")
	}
	visibleGrowAvatar, valid := currentGameOptionVisibleGrowAvatarEnabled(main)
	if !valid {
		return character, false, fmt.Errorf("stored current game option awakening avatar visibility is invalid")
	}
	showAllAvatar, valid := currentGameOptionShowAllAvatarEnabled(main)
	if !valid {
		return character, false, fmt.Errorf("stored current game option avatar visibility is invalid")
	}

	oldUserStateBits := currentActorDefaultUserStateBits
	if value, exists := character.Stats[currentGameOptionUserStateStat]; exists {
		if value < 0 || value > 0xff {
			return character, false, fmt.Errorf("selected character user state bits %d are outside u8", value)
		}
		oldUserStateBits = byte(value)
	}
	newUserStateBits := currentGameOptionVisibilityUserState(oldUserStateBits, advanced, visibleGrowAvatar, showAllAvatar)
	if newUserStateBits == oldUserStateBits {
		return character, false, nil
	}

	character = dnfrepo.CloneCharacter(character)
	if character.Stats == nil {
		character.Stats = make(map[string]int64)
	}
	character.Stats[currentGameOptionUserStateStat] = int64(newUserStateBits)
	character.UpdatedAt = time.Now().UTC()
	if err := dnfrepo.SaveCharacterFields(
		ctx,
		repositories.Character,
		character,
		dnfrepo.CharacterFieldStats,
	); err != nil {
		return character, false, fmt.Errorf("save selected character visibility from account game option: %w", err)
	}
	s.logGameEvent(session, "game-game-option-selected-character-visibility-reconciled",
		"account_id", accountID,
		"char_id", character.CharacterID,
		"job_awaken_effect_enabled", advanced,
		"visible_grow_avatar_enabled", visibleGrowAvatar,
		"show_all_avatar_enabled", showAllAvatar,
		"old_user_state_bits", oldUserStateBits,
		"user_state_bits", newUserStateBits,
		"source", "account_game_option_before_selected_actor_userinfo")
	return character, true, nil
}

// buildCurrentGameOptionTransportBody follows HandleRequestGameOptionSnapshot
// in the current EXE: option1[178], quick-chat slot0[300], slot1[300]. The
// native reader memcpy's each quick-chat vector without a destination-capacity
// check, so both vectors must always be exactly 300 bytes.
func buildCurrentGameOptionTransportBody(main, quickChat0, quickChat1 []byte) ([]byte, error) {
	if !validCurrentGameOptionMainBody(main) {
		return nil, fmt.Errorf("current game option main body is invalid")
	}
	quickChats := [2][]byte{quickChat0, quickChat1}
	for index := range quickChats {
		if len(quickChats[index]) == 0 {
			quickChats[index] = make([]byte, currentGameOptionQuickChatBodySize)
		}
		if !validCurrentGameOptionQuickChatBody(quickChats[index]) {
			return nil, fmt.Errorf("current game option quick chat slot%d body is invalid", index)
		}
	}
	body := make([]byte, 0, 3*currentGameOptionSaveHeaderLen+currentGameOptionMainBodySize+2*currentGameOptionQuickChatBodySize)
	body = appendCurrentGameOptionVector(body, main)
	body = appendCurrentGameOptionVector(body, quickChats[0])
	body = appendCurrentGameOptionVector(body, quickChats[1])
	return body, nil
}

// buildCurrentGameOptionKeymapTransportBody follows the current EXE's class0
// op454 reader: u8 mode followed by the same DSTR emitted by C2S op198.
func buildCurrentGameOptionKeymapTransportBody(keymap []byte) ([]byte, error) {
	if !validCurrentGameOptionKeymapBody(keymap) {
		return nil, fmt.Errorf("current game option keymap body is invalid")
	}
	body := make([]byte, 1, 1+currentGameOptionSaveHeaderLen+len(keymap))
	body[0] = currentGameOptionKeymapMode
	return appendCurrentGameOptionVector(body, keymap), nil
}

// currentGameOptionLoginRapidFireKeymap extracts only the ordinary-attack
// binding used by the account-level rapid-fire runtime from textual op454.
// The on/off switch itself is type197 option 50, not this I01 row.
//
// Keeping the early projection to I01 also avoids resetting character-specific
// Creator and Dark Knight bindings before a character has been selected.
func currentGameOptionLoginRapidFireKeymap(keymap []byte) ([]byte, bool) {
	for _, rawLine := range bytes.Split(keymap, []byte{'\n'}) {
		fields := strings.Fields(strings.TrimSpace(string(rawLine)))
		if len(fields) != 2 || fields[0] != currentGameOptionRapidFireKeymapID {
			continue
		}
		value, err := strconv.ParseUint(fields[1], 10, 16)
		if err != nil {
			continue
		}
		return []byte(fmt.Sprintf("%s\t%d\n", currentGameOptionRapidFireKeymapID, value)), true
	}
	return nil, false
}

// sendCurrentGameOptionLoginOptions restores the current EXE's account-level
// fixed option table at the proven post-login/pre-userinfo boundary. Restoring
// only when rapid fire was enabled left every other option until the later
// selected-scene layout pass, after process-level input/HUD switches such as
// skill-slot locking had already initialized from defaults.
//
// When rapid fire is enabled, the switch-bearing op173 must still precede the
// narrow I01 op454 projection: otherwise I01 is applied while option50 is at
// its process default. Class-0 op455 is intentionally not used: in this client
// it owns dual-raid state.
func (s *Service) sendCurrentGameOptionLoginOptions(session *gameSession) error {
	if s == nil || session == nil {
		return nil
	}
	accountID := strings.TrimSpace(session.accountID)
	if accountID == "" {
		return nil
	}
	repositories, ok := s.repositoryGroup()
	if !ok || repositories.Settings == nil {
		return nil
	}
	ctx, cancel := context.WithTimeout(context.Background(), createWriteTimeout)
	defer cancel()
	state, _, err := loadCurrentGameOptionState(ctx, repositories.Settings, accountID)
	if err != nil {
		return fmt.Errorf("load login game option: %w", err)
	}
	if !validCurrentGameOptionMainBody(state.main) {
		return nil
	}
	mainTransport, err := buildCurrentGameOptionTransportBody(
		state.main,
		state.quickChat[0],
		state.quickChat[1],
	)
	if err != nil {
		return err
	}
	enabled, _ := currentGameOptionRapidFireEnabled(state.main)
	s.logPacketEvent("game-upper-current-game-option-login-main-send",
		"conn_id", session.connID,
		"channel_id", session.channel.ID,
		"account_id", accountID,
		"msg_id", currentGameOptionMsgID,
		"classification", 0,
		"body_len", len(mainTransport),
		"main_body_len", len(state.main),
		"rapid_fire_option_index", currentGameOptionRapidFireIndex,
		"rapid_fire_enabled", enabled,
		"body_codec", "plaintext",
		"state_source", "account_scoped_type197_after_login_success_before_userinfo")
	if err := s.sendGameUpperRawClass(session, currentGameOptionMsgID, mainTransport, 0); err != nil {
		return err
	}
	if !enabled {
		return nil
	}
	rapidFireKeymap, found := currentGameOptionLoginRapidFireKeymap(state.keymap)
	if !found {
		return nil
	}
	keymapTransport, err := buildCurrentGameOptionKeymapTransportBody(rapidFireKeymap)
	if err != nil {
		return err
	}
	s.logPacketEvent("game-upper-current-game-option-login-rapid-fire-send",
		"conn_id", session.connID,
		"channel_id", session.channel.ID,
		"account_id", accountID,
		"msg_id", currentGameOptionKeymapMsgID,
		"classification", 0,
		"body_len", len(keymapTransport),
		"keymap_body_len", len(rapidFireKeymap),
		"keymap_mode", currentGameOptionKeymapMode,
		"rapid_fire_option_index", currentGameOptionRapidFireIndex,
		"rapid_fire_enabled", enabled,
		"body_codec", "plaintext",
		"state_source", "account_scoped_i01_after_type197_option50_before_userinfo")
	return s.sendGameUpperRawClass(session, currentGameOptionKeymapMsgID, keymapTransport, 0)
}

func appendCurrentGameOptionVector(body, vector []byte) []byte {
	var length [currentGameOptionSaveHeaderLen]byte
	binary.LittleEndian.PutUint32(length[:], uint32(len(vector)))
	body = append(body, length[:]...)
	return append(body, vector...)
}

func clearCurrentGameOptionRestoreSnapshot(session *gameSession) {
	if session == nil {
		return
	}
	session.initialTownGameOptionRestoreCharacterID = 0
	clearCurrentActorUserStateRestoreSnapshot(session)
	session.currentGameOptionMu.Lock()
	session.currentGameOptionMainRestoreCharacterID = 0
	session.currentGameOptionNativeApplyBody = nil
	session.currentGameOptionNativeApplyKeymapBody = nil
	session.currentGameOptionNativeApplyMainAt = time.Time{}
	session.currentGameOptionNativeApplyKeymapAt = time.Time{}
	session.currentGameOptionMu.Unlock()
}

// sendCurrentGameOptionMainBeforeSceneLayoutOnce restores the account-scoped
// fixed option table at the current EXE's first safe layout boundary. Current
// client packet-order regressions require the following class-0 op124 before
// UI-size-dependent state becomes visible. Callers must therefore bind the
// selected actor's mode1 descriptor
// first, then use sendCurrentSceneLayoutCommit instead of writing op124
// directly. The character-aware op454 keymap remains owned by the later
// HUD-ready boundary.
func (s *Service) sendCurrentGameOptionMainBeforeSceneLayoutOnce(session *gameSession, source string) error {
	if s == nil || session == nil || session.selectedCharacterID == 0 {
		return nil
	}
	characterID := session.selectedCharacterID
	session.currentGameOptionMu.Lock()
	alreadySent := session.currentGameOptionMainRestoreCharacterID == characterID
	session.currentGameOptionMu.Unlock()
	if alreadySent {
		return nil
	}

	repositories, ok := s.repositoryGroup()
	if !ok || repositories.Settings == nil {
		return nil
	}
	accountID := strings.TrimSpace(s.accountIDForSession(session))
	ctx, cancel := context.WithTimeout(context.Background(), createWriteTimeout)
	defer cancel()
	state, _, err := loadCurrentGameOptionState(ctx, repositories.Settings, accountID)
	if err != nil {
		return fmt.Errorf("load current game option before scene layout: %w", err)
	}
	var transport []byte
	if len(state.main) != 0 {
		transport, err = buildCurrentGameOptionTransportBody(state.main, state.quickChat[0], state.quickChat[1])
		if err != nil {
			return err
		}
	}

	// Serialize the final identity check and write so parallel scene callbacks
	// cannot publish the same op173 twice. A failed write leaves the cursor open
	// and clears only the uncommitted native-echo snapshot for an exact retry.
	session.currentGameOptionMu.Lock()
	defer session.currentGameOptionMu.Unlock()
	if session.selectedCharacterID != characterID ||
		session.currentGameOptionMainRestoreCharacterID == characterID {
		return nil
	}
	if len(transport) == 0 {
		session.currentGameOptionMainRestoreCharacterID = characterID
		return nil
	}
	now := time.Now()
	session.currentGameOptionNativeApplyBody = append([]byte(nil), state.main...)
	session.currentGameOptionNativeApplyMainAt = now
	s.logPacketEvent("game-upper-current-game-option-main-before-scene-layout-send",
		"conn_id", session.connID,
		"channel_id", session.channel.ID,
		"source", source,
		"account_id", accountID,
		"char_id", characterID,
		"msg_id", currentGameOptionMsgID,
		"classification", 0,
		"body_len", len(transport),
		"main_body_len", len(state.main),
		"quickchat0_body_len", currentGameOptionQuickChatBodySize,
		"quickchat1_body_len", currentGameOptionQuickChatBodySize,
		"body_codec", "plaintext",
		"state_source", "account_scoped_current_exe_type197_main_before_first_op124_ui_layout")
	if err := s.sendGameUpperRawClass(session, currentGameOptionMsgID, transport, 0); err != nil {
		session.currentGameOptionNativeApplyBody = nil
		session.currentGameOptionNativeApplyMainAt = time.Time{}
		return err
	}
	session.currentGameOptionMainRestoreCharacterID = characterID
	return nil
}

// sendCurrentSceneLayoutCommit keeps the current EXE's exact dependency:
// actor mode1 descriptor -> account option table -> op124 layout calculation.
func (s *Service) sendCurrentSceneLayoutCommit(session *gameSession, source string) error {
	if err := s.sendCurrentGameOptionMainBeforeSceneLayoutOnce(session, source+"_before_op124"); err != nil {
		return err
	}
	return s.sendGameUpperRawClass(session, uint16(dnfenum.CmdPacketReportClientSpec), nil, 0)
}

// sendCurrentGameOptionAfterLegacySceneReadyOnce restores the character-aware
// keymap after the current EXE has finished its HUD rebuild. The fixed main
// table normally arrived before the first op124 through
// sendCurrentSceneLayoutCommit; a main-table fallback is retained only for a
// route that reaches this boundary without that proven layout commit.
// This deliberately precedes the per-character op391 restore in the same
// type1345 boundary, matching the independent native option readers.
func (s *Service) sendCurrentGameOptionAfterLegacySceneReadyOnce(session *gameSession, source string) error {
	if s == nil || session == nil || session.selectedCharacterID == 0 {
		return nil
	}
	session.townMu.Lock()
	characterID := session.selectedCharacterID
	ready := session.initialTownLegacySceneReadyAccepted &&
		session.initialTownRouteCharacterID == characterID &&
		session.initialTownRouteStage >= currentInitialTownRoutePlayerStateSent &&
		!session.sceneBootstrapTailDeferred &&
		session.sceneBootstrapTailSent &&
		session.initialTownGameOptionRestoreCharacterID != characterID
	session.townMu.Unlock()
	if !ready {
		return nil
	}
	repositories, ok := s.repositoryGroup()
	if !ok || repositories.Settings == nil {
		return nil
	}
	accountID := strings.TrimSpace(s.accountIDForSession(session))
	ctx, cancel := context.WithTimeout(context.Background(), createWriteTimeout)
	defer cancel()
	state, found, err := loadCurrentGameOptionState(ctx, repositories.Settings, accountID)
	if err != nil {
		return fmt.Errorf("load current game option: %w", err)
	}
	var keymapStateSource string
	state.keymap, keymapStateSource, err = s.currentGameOptionKeymapForSelectedCharacter(
		ctx,
		repositories,
		session,
		state.keymap,
	)
	if err != nil {
		return fmt.Errorf("load selected character keymap: %w", err)
	}
	found = found || len(state.keymap) != 0
	var mainTransport []byte
	var keymapTransport []byte
	session.currentGameOptionMu.Lock()
	mainRestoredBeforeLayout := session.currentGameOptionMainRestoreCharacterID == characterID
	session.currentGameOptionMu.Unlock()
	if found {
		if len(state.main) != 0 && !mainRestoredBeforeLayout {
			mainTransport, err = buildCurrentGameOptionTransportBody(state.main, state.quickChat[0], state.quickChat[1])
			if err != nil {
				return err
			}
		}
		if len(state.keymap) != 0 {
			keymapTransport, err = buildCurrentGameOptionKeymapTransportBody(state.keymap)
			if err != nil {
				return err
			}
		}
	}

	session.townMu.Lock()
	defer session.townMu.Unlock()
	if !session.initialTownLegacySceneReadyAccepted ||
		session.selectedCharacterID != characterID ||
		session.initialTownRouteCharacterID != characterID ||
		session.initialTownRouteStage < currentInitialTownRoutePlayerStateSent ||
		session.sceneBootstrapTailDeferred ||
		!session.sceneBootstrapTailSent ||
		session.initialTownGameOptionRestoreCharacterID == characterID {
		return nil
	}
	if !found {
		session.initialTownGameOptionRestoreCharacterID = characterID
		return nil
	}
	session.currentGameOptionMu.Lock()
	defer session.currentGameOptionMu.Unlock()
	if session.currentGameOptionMainRestoreCharacterID == characterID {
		mainTransport = nil
	}
	now := time.Now()
	if len(mainTransport) != 0 {
		session.currentGameOptionNativeApplyBody = append([]byte(nil), state.main...)
		session.currentGameOptionNativeApplyMainAt = now
	}
	if len(keymapTransport) != 0 {
		session.currentGameOptionNativeApplyKeymapBody = append([]byte(nil), state.keymap...)
		session.currentGameOptionNativeApplyKeymapAt = now
	}
	if len(mainTransport) != 0 {
		s.logPacketEvent("game-upper-current-game-option-restore-send",
			"conn_id", session.connID,
			"channel_id", session.channel.ID,
			"source", source,
			"account_id", accountID,
			"char_id", characterID,
			"msg_id", currentGameOptionMsgID,
			"classification", 0,
			"body_len", len(mainTransport),
			"main_body_len", len(state.main),
			"quickchat0_body_len", currentGameOptionQuickChatBodySize,
			"quickchat1_body_len", currentGameOptionQuickChatBodySize,
			"body_codec", "plaintext",
			"state_source", "account_scoped_current_exe_type197_main_and_type368_quickchat_after_type1345_hud_ready")
		if err := s.sendGameUpperRawClass(session, currentGameOptionMsgID, mainTransport, 0); err != nil {
			session.currentGameOptionNativeApplyBody = nil
			session.currentGameOptionNativeApplyKeymapBody = nil
			session.currentGameOptionNativeApplyMainAt = time.Time{}
			session.currentGameOptionNativeApplyKeymapAt = time.Time{}
			return err
		}
		session.currentGameOptionMainRestoreCharacterID = characterID
	}
	if len(keymapTransport) != 0 {
		s.logPacketEvent("game-upper-current-game-option-keymap-restore-send",
			"conn_id", session.connID,
			"channel_id", session.channel.ID,
			"source", source,
			"account_id", accountID,
			"char_id", characterID,
			"msg_id", currentGameOptionKeymapMsgID,
			"classification", 0,
			"body_len", len(keymapTransport),
			"keymap_body_len", len(state.keymap),
			"keymap_mode", currentGameOptionKeymapMode,
			"body_codec", "plaintext",
			"state_source", keymapStateSource)
		if err := s.sendGameUpperRawClass(session, currentGameOptionKeymapMsgID, keymapTransport, 0); err != nil {
			session.currentGameOptionNativeApplyBody = nil
			session.currentGameOptionNativeApplyKeymapBody = nil
			session.currentGameOptionNativeApplyMainAt = time.Time{}
			session.currentGameOptionNativeApplyKeymapAt = time.Time{}
			return err
		}
	}
	session.initialTownGameOptionRestoreCharacterID = characterID
	return nil
}

func (s *Service) handleCurrentSaveGameOption(session *gameSession, body []byte) error {
	if s == nil || session == nil || !validCurrentGameOptionSaveBody(body) {
		if s != nil {
			s.logGameEvent(session, "game-game-option-save-deferred",
				"body_len", len(body),
				"reason", "current_exe_type197_body_invalid")
		}
		return nil
	}
	repositories, ok := s.repositoryGroup()
	if !ok || repositories.Settings == nil || repositories.Character == nil {
		s.logGameEvent(session, "game-game-option-save-deferred",
			"body_len", len(body),
			"reason", "settings_or_character_repository_unavailable")
		return nil
	}
	accountID := strings.TrimSpace(s.accountIDForSession(session))
	main := append([]byte(nil), body[currentGameOptionSaveHeaderLen:]...)
	if currentGameOptionIsNativeApplyEcho(session, main) {
		s.logGameEvent(session, "game-game-option-native-apply-echo-ignored",
			"account_id", accountID,
			"body_len", len(main),
			"reason", "current_exe_op173_apply_reemits_identical_type197")
		return nil
	}
	ctx, cancel := context.WithTimeout(context.Background(), createWriteTimeout)
	defer cancel()
	enabled, valid := currentGameOptionJobAwakenEffectEnabled(main)
	if !valid {
		return fmt.Errorf("current game option job/awakening effect is invalid")
	}
	visibleGrowAvatar, valid := currentGameOptionVisibleGrowAvatarEnabled(main)
	if !valid {
		return fmt.Errorf("current game option awakening avatar visibility is invalid")
	}
	showAllAvatar, valid := currentGameOptionShowAllAvatarEnabled(main)
	if !valid {
		return fmt.Errorf("current game option avatar visibility is invalid")
	}
	characterID := strconv.Itoa(int(session.selectedCharacterID))
	visibilityChanged := false
	newUserStateBits := currentActorDefaultUserStateBits
	if session.selectedCharacterID == 0 {
		if err := saveCurrentGameOption(ctx, repositories.Settings, accountID, main); err != nil {
			return err
		}
	} else {
		if repositories.CeraShopAssets == nil {
			return fmt.Errorf("save current game option and character visibility: %w", dnfrepo.ErrCeraShopAssetTransactionUnavailable)
		}
		if err := repositories.CeraShopAssets.WithinCeraShopAssets(
			ctx,
			accountID,
			characterID,
			currentGameOptionScope(accountID),
			func(
				_ dnfrepo.AccountRepository,
				_ dnfrepo.AccountInventoryRepository,
				characters dnfrepo.CharacterRepository,
				_ dnfrepo.InventoryRepository,
				_ dnfrepo.EquipmentRepository,
				settings dnfrepo.SettingsRepository,
			) error {
				character, found, err := characters.Load(ctx, characterID)
				if err != nil {
					return fmt.Errorf("load selected character before game option save: %w", err)
				}
				if !found {
					return fmt.Errorf("selected character %s was not found", characterID)
				}
				if accountID != "" && strings.TrimSpace(character.AccountID) != accountID {
					return fmt.Errorf("selected character %s owner mismatch", characterID)
				}
				character = dnfrepo.CloneCharacter(character)
				if character.Stats == nil {
					character.Stats = make(map[string]int64)
				}
				oldUserStateBits := currentActorDefaultUserStateBits
				if value, exists := character.Stats[currentGameOptionUserStateStat]; exists {
					if value < 0 || value > 0xff {
						return fmt.Errorf("selected character user state bits %d are outside u8", value)
					}
					oldUserStateBits = byte(value)
				}
				newUserStateBits = currentGameOptionVisibilityUserState(
					oldUserStateBits,
					enabled,
					visibleGrowAvatar,
					showAllAvatar,
				)
				visibilityChanged = oldUserStateBits != newUserStateBits
				if visibilityChanged {
					character.Stats[currentGameOptionUserStateStat] = int64(newUserStateBits)
					character.UpdatedAt = time.Now().UTC()
					if err := dnfrepo.SaveCharacterFields(ctx, characters, character, dnfrepo.CharacterFieldStats); err != nil {
						return fmt.Errorf("save selected character visibility: %w", err)
					}
				}
				if err := saveCurrentGameOption(ctx, settings, accountID, main); err != nil {
					return fmt.Errorf("save account game option: %w", err)
				}
				return nil
			}); err != nil {
			return err
		}
	}
	s.logGameEvent(session, "game-game-option-saved",
		"account_id", accountID,
		"char_id", session.selectedCharacterID,
		"body_len", len(main),
		"job_awaken_effect_enabled", enabled,
		"visible_grow_avatar_enabled", visibleGrowAvatar,
		"show_all_avatar_enabled", showAllAvatar,
		"user_state_bits", newUserStateBits,
		"storage", "account_settings_and_character_visibility")
	if visibilityChanged && !currentDungeonSceneActive(session) && currentTownSelfActorAppearanceReady(session) {
		if err := s.sendSelectedActorUserStateRefresh(session, "save_game_option_avatar_visibility_options1_55"); err != nil {
			s.logWarn("dnfbridge deferred avatar visibility user-state refresh",
				"conn_id", session.connID,
				"char_id", session.selectedCharacterID,
				"error", err)
		}
	}
	return nil
}

func (s *Service) handleCurrentSaveGameOptionKeymap(session *gameSession, body []byte) error {
	if s == nil || session == nil || !validCurrentGameOptionKeymapSaveBody(body) {
		if s != nil {
			s.logGameEvent(session, "game-game-option-keymap-save-deferred",
				"body_len", len(body),
				"reason", "current_exe_type198_body_invalid")
		}
		return nil
	}
	repositories, ok := s.repositoryGroup()
	if !ok || repositories.Settings == nil {
		s.logGameEvent(session, "game-game-option-keymap-save-deferred",
			"body_len", len(body),
			"reason", "settings_repository_unavailable")
		return nil
	}
	keymap := append([]byte(nil), body[currentGameOptionSaveHeaderLen:]...)
	accountID := strings.TrimSpace(s.accountIDForSession(session))
	if currentGameOptionKeymapIsNativeApplyEcho(session, keymap) {
		s.logGameEvent(session, "game-game-option-keymap-native-apply-echo-ignored",
			"account_id", accountID,
			"body_len", len(keymap),
			"reason", "current_exe_class0_op454_apply_reemits_identical_type198")
		return nil
	}
	ctx, cancel := context.WithTimeout(context.Background(), createWriteTimeout)
	defer cancel()
	// 缔造者把键位表交回职业默认（客户端点"恢复默认"后保存，或教学期回显）：客户端那张
	// 表不带缔造者的输入模型（I01..I05 是普通角色的 2/0/3/1/78）。这条不是"玩家改键"，
	// 而是"回到职业默认"：既要丢掉角色作用域里存的绑定，又必须把 PVF 的缔造者表推回
	// 客户端 —— 客户端本地已经换成普通默认表了，只拒绝保存界面还是错的。
	if s.currentGameOptionKeymapSaveIsCreatorDefaultRequest(ctx, repositories, session, keymap, accountID) {
		character, job, found, err := s.selectedCharacterJobForKeymap(ctx, repositories, session)
		if err != nil || !found || !currentSpecialJobOwnsCharacterKeymap(job) {
			return err
		}
		s.logGameEvent(session, "game-game-option-keymap-creator-restore-default",
			"account_id", accountID,
			"char_id", session.selectedCharacterID,
			"body_len", len(keymap),
			"reason", "saved_keymap_does_not_carry_the_creator_input_model",
			"action", "drop_stored_binding_and_push_the_pvf_creator_table",
			"state_source", "pvf_creator_ver2_wasd_space_digit_skill_keymap_locked")
		return s.restoreCurrentCreatorKeymapDefault(ctx, repositories, session, character)
	}
	scope, storage, err := s.currentGameOptionKeymapSaveScope(ctx, repositories, session, accountID)
	if err != nil {
		return fmt.Errorf("resolve current game option keymap scope: %w", err)
	}
	if err := saveCurrentGameOptionKeymapToScope(ctx, repositories.Settings, scope, keymap); err != nil {
		return fmt.Errorf("save current game option keymap: %w", err)
	}
	s.logGameEvent(session, "game-game-option-keymap-saved",
		"account_id", accountID,
		"char_id", session.selectedCharacterID,
		"body_len", len(keymap),
		"storage", storage,
		"scope", scope,
		"protocol_evidence", "current_exe_type198_u32_length_prefixed_keymap")
	return nil
}

func (s *Service) handleCurrentSaveGameOptionQuickChat(session *gameSession, body []byte) error {
	if s == nil || session == nil || !validCurrentGameOptionQuickChatSaveBody(body) {
		if s != nil {
			s.logGameEvent(session, "game-game-option-quickchat-save-deferred",
				"body_len", len(body),
				"reason", "current_exe_type368_body_invalid")
		}
		return nil
	}
	repositories, ok := s.repositoryGroup()
	if !ok || repositories.Settings == nil {
		s.logGameEvent(session, "game-game-option-quickchat-save-deferred",
			"body_len", len(body),
			"reason", "settings_repository_unavailable")
		return nil
	}
	accountID := strings.TrimSpace(s.accountIDForSession(session))
	slot := body[0]
	quickChat := append([]byte(nil), body[1+currentGameOptionSaveHeaderLen:]...)
	ctx, cancel := context.WithTimeout(context.Background(), createWriteTimeout)
	defer cancel()
	if err := saveCurrentGameOptionQuickChat(ctx, repositories.Settings, accountID, slot, quickChat); err != nil {
		return fmt.Errorf("save current game option quick chat slot%d: %w", slot, err)
	}
	s.logGameEvent(session, "game-game-option-quickchat-saved",
		"account_id", accountID,
		"char_id", session.selectedCharacterID,
		"slot", slot,
		"body_len", len(quickChat),
		"storage", "account_settings",
		"protocol_evidence", "current_exe_type368_u8_slot_plus_u32_300_plus_raw300")
	return nil
}

// sendCurrentGameOptionImmediately makes a saved settings panel take effect in
// the current town. The client owns the individual option semantics, including
// whether the selected character's job and grow type qualify for an effect.
func (s *Service) sendCurrentGameOptionImmediately(session *gameSession, main, keymap []byte) error {
	if s == nil || session == nil || session.conn == nil {
		return nil
	}
	transport, err := buildCurrentGameOptionTransportBody(main, nil, nil)
	if err != nil {
		return err
	}
	s.logPacketEvent("game-upper-current-game-option-immediate-apply-send",
		"conn_id", session.connID,
		"channel_id", session.channel.ID,
		"source", "legacy_type197_type198_save",
		"char_id", session.selectedCharacterID,
		"msg_id", currentGameOptionMsgID,
		"classification", 0,
		"body_len", len(transport),
		"body_codec", "plaintext",
		"state_source", "current_exe_type197_main_and_fixed_empty_quickchat_immediate_native_apply")
	rememberCurrentGameOptionNativeApply(session, main, keymap)
	if err := s.sendGameUpperRawClass(session, currentGameOptionMsgID, transport, 0); err != nil {
		clearCurrentGameOptionNativeApply(session)
		return err
	}
	if len(keymap) != 0 {
		keymapTransport, err := buildCurrentGameOptionKeymapTransportBody(keymap)
		if err != nil {
			clearCurrentGameOptionNativeApply(session)
			return err
		}
		if err := s.sendGameUpperRawClass(session, currentGameOptionKeymapMsgID, keymapTransport, 0); err != nil {
			clearCurrentGameOptionNativeApply(session)
			return err
		}
	}
	return nil
}

func rememberCurrentGameOptionNativeApply(session *gameSession, main, keymap []byte) {
	if session == nil {
		return
	}
	session.currentGameOptionMu.Lock()
	now := time.Now()
	session.currentGameOptionNativeApplyBody = append([]byte(nil), main...)
	session.currentGameOptionNativeApplyKeymapBody = append([]byte(nil), keymap...)
	session.currentGameOptionNativeApplyMainAt = now
	session.currentGameOptionNativeApplyKeymapAt = now
	session.currentGameOptionMu.Unlock()
}

func clearCurrentGameOptionNativeApply(session *gameSession) {
	if session == nil {
		return
	}
	session.currentGameOptionMu.Lock()
	session.currentGameOptionNativeApplyBody = nil
	session.currentGameOptionNativeApplyKeymapBody = nil
	session.currentGameOptionNativeApplyMainAt = time.Time{}
	session.currentGameOptionNativeApplyKeymapAt = time.Time{}
	session.currentGameOptionMu.Unlock()
}

func currentGameOptionIsNativeApplyEcho(session *gameSession, main []byte) bool {
	if session == nil || !validCurrentGameOptionMainBody(main) {
		return false
	}
	session.currentGameOptionMu.Lock()
	defer session.currentGameOptionMu.Unlock()
	if session.currentGameOptionNativeApplyMainAt.IsZero() ||
		time.Since(session.currentGameOptionNativeApplyMainAt) > currentGameOptionNativeApplyGracePeriod {
		return false
	}
	return bytes.Equal(session.currentGameOptionNativeApplyBody, main)
}

func currentGameOptionKeymapIsNativeApplyEcho(session *gameSession, keymap []byte) bool {
	if session == nil || !validCurrentGameOptionKeymapBody(keymap) {
		return false
	}
	session.currentGameOptionMu.Lock()
	defer session.currentGameOptionMu.Unlock()
	if session.currentGameOptionNativeApplyKeymapAt.IsZero() ||
		time.Since(session.currentGameOptionNativeApplyKeymapAt) > currentGameOptionNativeApplyGracePeriod {
		return false
	}
	return bytes.Equal(session.currentGameOptionNativeApplyKeymapBody, keymap)
}
