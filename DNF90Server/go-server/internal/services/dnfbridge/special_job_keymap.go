package dnfbridge

import (
	"bytes"
	"context"
	"fmt"
	"strconv"
	"strings"
	"time"

	dnfrepo "longheng.io/server/internal/modules/dnf/repository"
)

func currentCharacterGameOptionKeymapScope(characterID string) string {
	return "character:" + strings.TrimSpace(characterID) + ":game_option_keymap"
}

func loadCurrentGameOptionKeymapFromScope(
	ctx context.Context,
	repository dnfrepo.SettingsRepository,
	scope string,
) ([]byte, bool, error) {
	if repository == nil || strings.TrimSpace(scope) == "" {
		return nil, false, nil
	}
	record, found, err := repository.Load(ctx, scope)
	if err != nil || !found {
		return nil, found, err
	}
	body, ok := decodeCurrentGameOptionValue(record, currentGameOptionKeymapValueKey, validCurrentGameOptionKeymapBody)
	return body, ok, nil
}

func saveCurrentGameOptionKeymapToScope(
	ctx context.Context,
	repository dnfrepo.SettingsRepository,
	scope string,
	body []byte,
) error {
	if repository == nil || strings.TrimSpace(scope) == "" || !validCurrentGameOptionKeymapBody(body) {
		return fmt.Errorf("current game option scoped keymap is invalid")
	}
	return saveCurrentGameOptionScopedValue(ctx, repository, scope, currentGameOptionKeymapValueKey, body)
}

func (s *Service) selectedCharacterJobForKeymap(
	ctx context.Context,
	repositories dnfrepo.Group,
	session *gameSession,
) (dnfrepo.CharacterRecord, byte, bool, error) {
	if session == nil || session.selectedCharacterID == 0 || repositories.Character == nil {
		return dnfrepo.CharacterRecord{}, 0, false, nil
	}
	characterID := strconv.Itoa(int(session.selectedCharacterID))
	character, found, err := repositories.Character.Load(ctx, characterID)
	if err != nil || !found {
		return dnfrepo.CharacterRecord{}, 0, found, err
	}
	if accountID := strings.TrimSpace(s.accountIDForSession(session)); accountID != "" {
		if owner := strings.TrimSpace(character.AccountID); owner != "" && owner != accountID {
			return dnfrepo.CharacterRecord{}, 0, false, fmt.Errorf("character %s keymap owner mismatch", characterID)
		}
	}
	job, ok := characterJobByte(character)
	if !ok {
		return dnfrepo.CharacterRecord{}, 0, false, fmt.Errorf("character %s keymap job %q is invalid", characterID, character.Job)
	}
	return character, job, true, nil
}

func (s *Service) currentGameOptionKeymapForSelectedCharacter(
	ctx context.Context,
	repositories dnfrepo.Group,
	session *gameSession,
	accountKeymap []byte,
) ([]byte, string, error) {
	_, job, found, err := s.selectedCharacterJobForKeymap(ctx, repositories, session)
	if err != nil {
		return nil, "", err
	}
	if !found || !currentSpecialJobOwnsCharacterKeymap(job) {
		return append([]byte(nil), accountKeymap...), "account_scoped_current_exe_type198_keymap", nil
	}
	// Creator 的输入模型不是普通可重排的快捷栏表，它的那一份存在**角色**作用域
	// （handleCurrentSaveGameOptionKeymap 用 currentCharacterGameOptionKeymapScope）。
	//
	// 活体证据 2026-09-15：库里 `character:13:game_option_keymap` 存着玩家改过的
	// `I21=30, I22=25`，而登录时这里**无视它、直接下发 PVF 默认表**
	// （`I21..I25 = 46..50`），于是"改技能快捷键 → 小退重上全恢复原样"。
	//
	// 但也不能无条件信任那份存储：客户端点"恢复默认"会给出一张**普通角色**的表
	// （I01..I05 = 2/0/3/1/78，而不是缔造者的 77/55/73/58/78），教学期也会回显一张临时表。
	// 所以判据是"这份存储必须带着缔造者的输入模型"（keymapHasCreatorInputModel）：
	// I01..I05 逐行等于职业默认表那五行。玩家改键只动 I21..I25 那些技能选择键，
	// 依然满足这条；普通表怎么都满足不了。
	profile, profileErr := s.currentCreatorHotkeyProfile(ctx)
	if profileErr != nil {
		return nil, "", profileErr
	}
	stored, storedFound, loadErr := loadCurrentGameOptionKeymapFromScope(
		ctx,
		repositories.Settings,
		currentCharacterGameOptionKeymapScope(strconv.Itoa(int(session.selectedCharacterID))),
	)
	if loadErr == nil && storedFound && validStoredCreatorKeymap(stored) &&
		keymapHasCreatorInputModel(stored, profile.keymap) {
		return append([]byte(nil), stored...), "character_scoped_creator_type198_keymap", nil
	}
	if loadErr == nil && storedFound && len(stored) != 0 {
		// 老数据自愈：库里存着一份不带缔造者输入模型的表（客户端"恢复默认"或教学回显
		// 留下的），当它不存在，下发 PVF 表。
		s.logGameEvent(session, "game-game-option-keymap-creator-default-store-ignored",
			"char_id", session.selectedCharacterID,
			"body_len", len(stored),
			"reason", "stored_creator_keymap_does_not_carry_the_creator_input_model",
			"state_source", "pvf_creator_ver2_wasd_space_digit_skill_keymap_locked")
	}
	return append([]byte(nil), profile.keymap...), "pvf_creator_ver2_wasd_space_digit_skill_keymap_locked", nil
}

// validStoredCreatorKeymap 判断一份存储的 keymap 是不是"一份完整的缔造者表"。
//
// 只认形状，不认具体键位：缔造者的表必须有 I01（普攻）与 I21..I25（五个技能选择键），
// 这几行正是玩家在游戏里改技能快捷键时会被改写的那几行。教学期回显进来的临时普通
// 表不是这个形状，所以它永远拿不到权威地位。
func validStoredCreatorKeymap(keymap []byte) bool {
	if len(keymap) == 0 || len(keymap) > currentGameOptionKeymapMaxBodySize {
		return false
	}
	rows := keymapRowsByIdentifier(keymap)
	for _, identifier := range []string{"I01", "I21", "I22", "I23", "I24", "I25"} {
		if _, present := rows[identifier]; !present {
			return false
		}
	}
	return true
}

// keymapRowsByIdentifier 把 keymap 正文解析成 行名 -> 值。
// 行格式是 `I21\t30\n`；值原样保留字符串，比较时不需要解释成按键。
func keymapRowsByIdentifier(keymap []byte) map[string]string {
	rows := make(map[string]string, 32)
	for _, rawLine := range bytes.Split(keymap, []byte{'\n'}) {
		fields := bytes.Split(bytes.TrimRight(rawLine, "\r"), []byte{'\t'})
		if len(fields) != 2 {
			continue
		}
		rows[string(fields[0])] = string(fields[1])
	}
	return rows
}

// currentCreatorKeymapAttackRows 是缔造者输入模型里 I01..I05 这五行 —— 也就是
// "普攻/移动"那一组。它们同时也是**分辨一份表是不是缔造者表**的指纹。
//
// 线上数据（2026-09-15）：
//   - PVF 缔造者表：  I01=77 I02=55 I03=73 I04=58 I05=78
//   - 客户端"恢复默认"给的表（普通角色表）：I01=2 I02=0 I03=3 I04=1 I05=78
//
// 两边的 I21..I25 都可能是 55/73/58/60/61（技能与"物品/技能"分组的对应关系在客户端的
// 默认表里是错位的），所以**只能靠 I01..I05 认**。
var currentCreatorKeymapAttackRows = []string{"I01", "I02", "I03", "I04", "I05"}

// keymapHasCreatorInputModel 判断一份 keymap 是不是"缔造者的输入模型"：I01..I05
// 逐行等于职业默认表里那五行。
//
// 这一条既是"玩家自己改过键"的合格线（改键只会动 I21..I25 那些技能选择键），
// 也是"客户端把普通角色表塞进来"的检出线（普通表的 I01..I05 是 2/0/3/1/78）。
func keymapHasCreatorInputModel(keymap, creatorProfile []byte) bool {
	if len(keymap) == 0 || len(creatorProfile) == 0 {
		return false
	}
	rows := keymapRowsByIdentifier(keymap)
	profileRows := keymapRowsByIdentifier(creatorProfile)
	matched := 0
	for _, identifier := range currentCreatorKeymapAttackRows {
		profileValue, profilePresent := profileRows[identifier]
		rowValue, rowPresent := rows[identifier]
		if !profilePresent || !rowPresent {
			continue
		}
		if rowValue == profileValue {
			matched++
		}
	}
	return matched == len(currentCreatorKeymapAttackRows)
}

// currentCreatorSaveIsClientDefaultTable 判断一张表是不是"客户端自己的默认表"。
//
// 指纹来自线上实测（2026-09-15）：
//
//	移动组 I01..I04 —— 缔造者 77/55/73/58(WASD) vs 客户端默认 2/0/3/1(←↑↓→)
//	跳跃   I06       —— 缔造者 21(空格)        vs 客户端默认 57(C)
//
// 只要这五行里对上了移动组至少 3 行且 I06 也一致，就认定是客户端默认表。
// 只看"是不是缔造者形状"是不够的：玩家手动把键全清空（整表 134）也是一种合法状态，
// 那不该被当成"恢复默认"（现场 05:53:17 那次就是这样被我误判的）。
func currentCreatorSaveIsClientDefaultTable(keymap []byte) bool {
	rows := keymapRowsByIdentifier(keymap)
	if len(rows) == 0 {
		return false
	}
	movement := []string{"I01", "I02", "I03", "I04"}
	matched := 0
	for _, identifier := range movement {
		if value, present := rows[identifier]; present && value == currentClientDefaultKeymapMovement[identifier] {
			matched++
		}
	}
	if matched < 3 {
		return false
	}
	jump, present := rows["I06"]
	return present && jump == currentClientDefaultKeymapJumpRow
}

// currentClientDefaultKeymapMovement / ...JumpRow 是客户端默认表的移动组与跳跃行，
// 实测值（见上）。它们对"普通角色默认"与"客户端给缔造者的默认"是同一组值。
var currentClientDefaultKeymapMovement = map[string]string{
	"I01": "2", "I02": "0", "I03": "3", "I04": "1",
}

const currentClientDefaultKeymapJumpRow = "57"

// keymapBodyMatchesPrefix 判断 candidate 的每一行都与 reference 同行的值相同。
//
// 逐字节比较在这里不成立：客户端发上来的普通表与库里账号表可能行数/个别行不同
// （实测 94 行 vs 91 行），所以按行比、并且允许 candidate 更短。
func keymapBodyMatchesPrefix(candidate, reference []byte) bool {
	candidateRows := keymapRowsByIdentifier(candidate)
	referenceRows := keymapRowsByIdentifier(reference)
	if len(candidateRows) == 0 {
		return false
	}
	for identifier, value := range candidateRows {
		if referenceRows[identifier] != value {
			return false
		}
	}
	return true
}

// restoreCurrentCreatorKeymapDefault 处理缔造者的"恢复默认按键"。
//
// 客户端的这个按钮只是在**本地**把键位表换成普通角色的默认表，然后 op198 通知服务端；
// 所以光拒绝保存是不够的 —— 界面上已经变成错的表了，必须由服务端把**这个职业自己的**
// PVF 默认表推回去（同一条 op454 应用路径）。
//
// 同时清掉角色作用域里存的自定义键位表：这样下次登录也会回到 PVF 默认表，而不是又发出
// 玩家之前改过的绑定 —— "恢复默认"本来就该把改动一并丢掉。
func (s *Service) restoreCurrentCreatorKeymapDefault(
	ctx context.Context,
	repositories dnfrepo.Group,
	session *gameSession,
	character dnfrepo.CharacterRecord,
) error {
	if s == nil || session == nil || repositories.Settings == nil {
		return nil
	}
	if err := clearCurrentSpecialJobKeymapStore(ctx, repositories.Settings, character.CharacterID); err != nil {
		return fmt.Errorf("clear creator keymap store: %w", err)
	}
	return s.sendCurrentCreatorKeymapInitialization(session, ctx, character, "creator_restore_default_keymap")
}

// clearCurrentSpecialJobKeymapStore 写一条没有键位表正文的记录，等价于"回到职业默认"。
// 不清空记录本身是为了不动设置表的其它值，也避免依赖仓储的删除语义。
func clearCurrentSpecialJobKeymapStore(
	ctx context.Context,
	repository dnfrepo.SettingsRepository,
	characterID string,
) error {
	characterID = strings.TrimSpace(characterID)
	if repository == nil || characterID == "" {
		return fmt.Errorf("current special job keymap store is invalid")
	}
	record, found, err := repository.Load(ctx, currentCharacterGameOptionKeymapScope(characterID))
	if err != nil {
		return err
	}
	if !found {
		return nil
	}
	record = dnfrepo.CloneSettings(record)
	if record.Values != nil {
		delete(record.Values, currentGameOptionKeymapValueKey)
	}
	record.UpdatedAt = time.Now().UTC()
	return repository.Save(ctx, record)
}

// sendCurrentCreatorKeymapInitialization restores Creator's class-specific
// input table after a dungeon has finished rebuilding its actor/skill HUD.
// Town initialization has its own type1345 boundary, but a newly created
// Creator enters the tutorial dungeon directly and never reaches that callback.
//
// 关键：这里必须下发**角色自己那份**（玩家改过的绑定优先），只有没存过时才用 PVF 默认表。
// 现场 2026-09-15：玩家给缔造者改好技能键，一进副本就被这里推的 PVF 默认表覆盖回
// "默认"，因为副本每次重建 HUD 都会走这个边界（日志里 source=...after_final_scene_skill_manager）。
func (s *Service) sendCurrentCreatorKeymapInitialization(
	session *gameSession,
	ctx context.Context,
	character dnfrepo.CharacterRecord,
	source string,
) error {
	if s == nil || session == nil || session.selectedCharacterID == 0 {
		return nil
	}
	job, ok := characterJobByte(character)
	if !ok {
		return fmt.Errorf("creator keymap character %s job %q is invalid", character.CharacterID, character.Job)
	}
	if job != csharpCreatorMageJob {
		return nil
	}
	profile, err := s.currentCreatorHotkeyProfile(ctx)
	if err != nil {
		return fmt.Errorf("load creator dungeon keymap: %w", err)
	}
	keymap := append([]byte(nil), profile.keymap...)
	keymapSource := "pvf_creator_ver2_default"
	if repositories, ok := s.repositoryGroup(); ok && repositories.Settings != nil {
		if stored, found, loadErr := loadCurrentGameOptionKeymapFromScope(
			ctx, repositories.Settings, currentCharacterGameOptionKeymapScope(character.CharacterID),
		); loadErr == nil && found && validStoredCreatorKeymap(stored) &&
			keymapHasCreatorInputModel(stored, profile.keymap) {
			// 玩家自己改过的绑定：副本边界不能把它冲掉。
			keymap = append([]byte(nil), stored...)
			keymapSource = "character_scoped_creator_rebind"
		}
	}
	transport, err := buildCurrentGameOptionKeymapTransportBody(keymap)
	if err != nil {
		return err
	}

	session.currentGameOptionMu.Lock()
	defer session.currentGameOptionMu.Unlock()
	session.currentGameOptionNativeApplyKeymapBody = keymap
	session.currentGameOptionNativeApplyKeymapAt = time.Now()
	s.logPacketEvent("game-upper-current-creator-keymap-restore-send",
		"conn_id", session.connID,
		"channel_id", session.channel.ID,
		"source", source,
		"account_id", strings.TrimSpace(s.accountIDForSession(session)),
		"char_id", session.selectedCharacterID,
		"msg_id", currentGameOptionKeymapMsgID,
		"classification", 0,
		"body_len", len(transport),
		"keymap_body_len", len(keymap),
		"keymap_mode", currentGameOptionKeymapMode,
		"input_contract", "creator_wasd_space_1_2_3_4_5",
		"state_source", keymapSource)
	if err := s.sendGameUpperRawClass(session, currentGameOptionKeymapMsgID, transport, 0); err != nil {
		session.currentGameOptionNativeApplyKeymapBody = nil
		session.currentGameOptionNativeApplyKeymapAt = time.Time{}
		return err
	}
	return nil
}

func (s *Service) currentGameOptionKeymapSaveScope(
	ctx context.Context,
	repositories dnfrepo.Group,
	session *gameSession,
	accountID string,
) (string, string, error) {
	character, job, found, err := s.selectedCharacterJobForKeymap(ctx, repositories, session)
	if err != nil {
		return "", "", err
	}
	if found && currentSpecialJobOwnsCharacterKeymap(job) {
		return currentCharacterGameOptionKeymapScope(character.CharacterID), "character_settings_special_job", nil
	}
	return currentGameOptionScope(accountID), "account_settings", nil
}

// currentGameOptionKeymapSaveIsCreatorDefaultRequest 判断这次 op198 保存是不是
// "缔造者把键位表交回给客户端默认"（客户端点恢复默认后保存）。
//
// 判据是**这张表带不带客户端默认指纹**（currentCreatorSaveIsClientDefaultTable），
// 而不是"像不像缔造者表" —— 玩家手动清空（整表未绑定）等合法状态必须放行。
// 只对缔造者生效；非缔造者一律返回 false。
func (s *Service) currentGameOptionKeymapSaveIsCreatorDefaultRequest(
	ctx context.Context,
	repositories dnfrepo.Group,
	session *gameSession,
	keymap []byte,
	accountID string,
) bool {
	if s == nil || session == nil || len(keymap) == 0 {
		return false
	}
	_, job, found, err := s.selectedCharacterJobForKeymap(ctx, repositories, session)
	if err != nil || !found || !currentSpecialJobOwnsCharacterKeymap(job) {
		return false
	}
	if currentCreatorSaveIsClientDefaultTable(keymap) {
		return true
	}
	if repositories.Settings != nil {
		if accountKeymap, accountFound, loadErr := loadCurrentGameOptionKeymap(
			ctx, repositories.Settings, accountID); loadErr == nil && accountFound &&
			keymapBodyMatchesPrefix(keymap, accountKeymap) {
			return true
		}
	}
	return false
}
