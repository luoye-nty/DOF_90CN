package dnfbridge

import (
	"bytes"
	"context"
	"encoding/binary"
	"strconv"
	"strings"
	"testing"
	"time"

	dnfcharstat "longheng.io/server/internal/modules/dnf/charstat"
	"longheng.io/server/internal/modules/dnf/dnfenum"
	"longheng.io/server/internal/modules/dnf/dungeoncmd"
	dnfhonor "longheng.io/server/internal/modules/dnf/honor"
	"longheng.io/server/internal/modules/dnf/premium"
	"longheng.io/server/internal/modules/dnf/progression"
	dnfproto "longheng.io/server/internal/modules/dnf/protocol"
	dnfpvf "longheng.io/server/internal/modules/dnf/pvf"
	dnfquest "longheng.io/server/internal/modules/dnf/quest"
	dnfrepo "longheng.io/server/internal/modules/dnf/repository"
	"longheng.io/server/internal/modules/dnf/worldmap"
)

func TestCurrentDungeonMonsterExperienceUsesRuntimePVFBaseAnd86JPRounding(t *testing.T) {
	service, runtime, _ := prepareCurrentDungeonMonsterExperienceTest(t)
	catalog, err := service.dungeonMonsterCatalog()
	if err != nil {
		t.Fatal(err)
	}
	resources, err := currentDungeonMonsterExperienceResourcesForCatalog(catalog)
	if err != nil {
		t.Fatal(err)
	}
	monster := runtime.Room.Snapshot().Monsters[0]

	// 86JP-domain arithmetic, with this test's runtime PVF base (100):
	// int(100 * 1.25) = 125; int(125 * 1.30) = 162;
	// uint(162 * 1.12) = 181.
	runtime.Character.Level = 19
	runtime.Dungeon.Metadata.ExperienceIncreasingPoint = worldmap.OptionalNumber{Value: 1.25, Set: true}
	runtime.Request.Difficulty = 0
	award, err := currentDungeonMonsterExperienceAwardFor(runtime, monster, resources.Sources)
	if err != nil {
		t.Fatal(err)
	}
	if award.MonsterTableEXP != 100 || award.MonsterLevel != 20 || award.MonsterType != 0 ||
		award.NamedMonster || award.PrePenaltyGain != 162 || award.Gain != 181 {
		t.Fatalf("award=%+v", award)
	}

	// Named is supplied by the parsed runtime dungeon metadata; it is not
	// inferred from an actor label or a C# packet.  The authorized domain rule
	// triples the type rate before the final killer-level penalty.
	runtime.Character.Level = 20
	runtime.Dungeon.Metadata.ExperienceIncreasingPoint = worldmap.OptionalNumber{Value: 1, Set: true}
	runtime.Dungeon.Metadata.NamedMonsters = []int64{monster.Spawn.MonsterID}
	award, err = currentDungeonMonsterExperienceAwardFor(runtime, monster, resources.Sources)
	if err != nil {
		t.Fatal(err)
	}
	if !award.NamedMonster || award.MonsterTypeRate != 3 || award.PrePenaltyGain != 390 || award.Gain != 390 {
		t.Fatalf("named award=%+v", award)
	}
}

func TestCurrentDungeonMonsterDeathPersistsExperienceAndSendsCurrentEXEOp37(t *testing.T) {
	service, runtime, repositories := prepareCurrentDungeonMonsterExperienceTest(t)
	connection := &bufferConn{}
	session := &gameSession{
		conn:                connection,
		connID:              "dungeon-monster-experience-test",
		selectedCharacterID: 99,
		dungeon:             dungeonSessionState{runtime: runtime},
	}
	monster := runtime.Room.Snapshot().Monsters[0]
	request := make([]byte, dungeoncmd.DieMonsterRequestSize)
	binary.LittleEndian.PutUint32(request[0:4], monster.ObjectKey)
	binary.LittleEndian.PutUint16(request[4:6], currentSceneActorObjectKey(99))

	if err := service.handleDungeonMonsterDeath(session, request); err != nil {
		t.Fatal(err)
	}
	death, rest := splitGameServerUpperPacket(t, connection.write.Bytes())
	if death.Header.Classification != 0 || death.Header.MsgID != uint16(dnfenum.CmdPacketNotifyDieMonster) ||
		len(death.Body) != currentDungeonZeroDropDeathBodySize {
		t.Fatalf("death=%+v body=%x", death.Header, death.Body)
	}
	state, rest := splitGameServerUpperPacket(t, rest)
	if state.Header.Classification != 0 || state.Header.MsgID != currentDungeonCharacterStateMsgID ||
		len(state.Body) != currentFinishLoadingCharacterStateBodySize {
		t.Fatalf("state=%+v body_len=%d rest=%x", state.Header, len(state.Body), rest)
	}
	assertCurrentDungeonFinalClearTail(t, rest)
	if state.Body[0] != 20 || binary.LittleEndian.Uint32(state.Body[1:5]) != 200 || state.Body[0x2e] != 0 {
		t.Fatalf("op37 body=%x", state.Body)
	}

	character, found, err := repositories.Character.Load(context.Background(), "99")
	if err != nil || !found {
		t.Fatalf("character found=%t err=%v", found, err)
	}
	if character.Level != 20 || character.Stats["exp"] != 200 {
		t.Fatalf("persisted character=%+v", character)
	}
	skill, found, err := repositories.Skill.Load(context.Background(), "99")
	if err != nil || !found {
		t.Fatalf("skill found=%t err=%v", found, err)
	}
	if skill.Points != (dnfrepo.SkillPointState{TotalSP: 20, RemainingSP: 20, SyncedLevel: 20}) {
		t.Fatalf("persisted skill points=%+v", skill.Points)
	}
	if runtime.Character.Level != 20 || runtime.Character.Stats["exp"] != 200 {
		t.Fatalf("runtime character=%+v", runtime.Character)
	}

	// The room's accepted-death transition remains the duplicate guard.  A
	// byte-identical retry cannot emit a second op37 or mutate EXP again.
	connection.write.Reset()
	if err := service.handleDungeonMonsterDeath(session, request); err != nil {
		t.Fatal(err)
	}
	if connection.write.Len() != 0 {
		t.Fatalf("duplicate death emitted=%x", connection.write.Bytes())
	}
	character, _, err = repositories.Character.Load(context.Background(), "99")
	if err != nil || character.Stats["exp"] != 200 {
		t.Fatalf("duplicate death changed character=%+v err=%v", character, err)
	}
}

func TestCurrentDungeonMonsterDeathSendsVisualACKBeforeProgressionPersistence(t *testing.T) {
	service, runtime, repositories := prepareCurrentDungeonMonsterExperienceTest(t)
	connection := &bufferConn{}
	entered := make(chan struct{})
	release := make(chan struct{})
	blockingGroup := repositories
	// 角色经验与账号级冒险团状态现在同属一个提交，所以这里拦的是
	// settlement 工作单元，而不是过去的角色-技能工作单元。
	blockingGroup.CharacterSettlement = &blockingMonsterExperienceSettlementUOW{
		base:    repositories.CharacterSettlement,
		entered: entered,
		release: release,
	}
	service.repositoryProvider = func() (dnfrepo.Group, bool) { return blockingGroup, true }
	session := &gameSession{
		conn:                connection,
		connID:              "dungeon-monster-instant-visual-death-test",
		selectedCharacterID: 99,
		dungeon:             dungeonSessionState{runtime: runtime},
	}
	monster := runtime.Room.Snapshot().Monsters[0]
	request := make([]byte, dungeoncmd.DieMonsterRequestSize)
	binary.LittleEndian.PutUint32(request[0:4], monster.ObjectKey)
	binary.LittleEndian.PutUint16(request[4:6], currentSceneActorObjectKey(99))

	done := make(chan error, 1)
	go func() { done <- service.handleDungeonMonsterDeath(session, request) }()
	select {
	case <-entered:
	case <-time.After(time.Second):
		t.Fatal("monster progression transaction did not start")
	}
	death, trailing := splitGameServerUpperPacket(t, append([]byte(nil), connection.write.Bytes()...))
	if death.Header.MsgID != uint16(dnfenum.CmdPacketNotifyDieMonster) || len(trailing) != 0 {
		t.Fatalf("visual death ACK was not emitted before progression: header=%+v trailing=%x", death.Header, trailing)
	}
	close(release)
	select {
	case err := <-done:
		if err != nil {
			t.Fatal(err)
		}
	case <-time.After(time.Second):
		t.Fatal("monster death did not finish after progression release")
	}
}

type blockingMonsterExperienceSettlementUOW struct {
	base    dnfrepo.CharacterSettlementUnitOfWork
	entered chan struct{}
	release chan struct{}
}

func (u *blockingMonsterExperienceSettlementUOW) WithinCharacterSettlement(
	ctx context.Context,
	characterID string,
	apply func(dnfrepo.Group) error,
) error {
	close(u.entered)
	select {
	case <-u.release:
	case <-ctx.Done():
		return ctx.Err()
	}
	return u.base.WithinCharacterSettlement(ctx, characterID, apply)
}

func TestCurrentDungeonMonsterLevelUpRefreshesLevelGatedTaskManual(t *testing.T) {
	service, runtime, repositories := prepareCurrentDungeonMonsterExperienceTest(t)
	catalog, err := service.dungeonMonsterCatalog()
	if err != nil {
		t.Fatal(err)
	}
	source := catalog.source.(bridgePVFSource)
	// The ordinary monster award crosses only level 20 -> 21.
	source[progression.ExperienceTablePath] = strings.TrimSpace(strings.Repeat("0 ", 19) + "100 1000")
	source[progression.SkillPointTablePath] = "[sp table]\n20 20\n21 10\n[/sp table]\n[tp table]\n50 1\n[/tp table]\n"
	service.questCatalog = currentDungeonLevelGatedQuestCatalog(t)

	connection := &bufferConn{}
	session := &gameSession{
		conn:                connection,
		connID:              "dungeon-monster-level-task-refresh",
		selectedCharacterID: 99,
		dungeon:             dungeonSessionState{runtime: runtime},
	}
	monster := runtime.Room.Snapshot().Monsters[0]
	request := make([]byte, dungeoncmd.DieMonsterRequestSize)
	binary.LittleEndian.PutUint32(request[0:4], monster.ObjectKey)
	binary.LittleEndian.PutUint16(request[4:6], currentSceneActorObjectKey(99))

	if err := service.handleDungeonMonsterDeath(session, request); err != nil {
		t.Fatal(err)
	}
	death, rest := splitGameServerUpperPacket(t, connection.write.Bytes())
	if death.Header.MsgID != uint16(dnfenum.CmdPacketNotifyDieMonster) {
		t.Fatalf("first packet=%d want=%d", death.Header.MsgID, dnfenum.CmdPacketNotifyDieMonster)
	}
	state, rest := splitGameServerUpperPacket(t, rest)
	if state.Header.MsgID != currentDungeonCharacterStateMsgID || state.Body[0] != 21 {
		t.Fatalf("level-up state msg=%d body=%x", state.Header.MsgID, state.Body)
	}
	skillRuntime, rest := splitGameServerUpperPacket(t, rest)
	if skillRuntime.Header.Classification != 0 || skillRuntime.Header.MsgID != currentSkillInfoMsgID {
		t.Fatalf("level-up skill runtime refresh=%+v body=%x", skillRuntime.Header, skillRuntime.Body)
	}
	persistedSkill, found, err := repositories.Skill.Load(context.Background(), "99")
	if err != nil || !found {
		t.Fatalf("load committed level-up skill found=%t err=%v", found, err)
	}
	wantSkillRuntime, _, err := buildCurrentSceneSkillInfoBody(persistedSkill, persistedSkill.Layouts, currentSkillInfoTreeIndex)
	if err != nil {
		t.Fatal(err)
	}
	if !bytes.Equal(skillRuntime.Body, wantSkillRuntime) {
		t.Fatalf("level-up skill runtime body=%x want=%x", skillRuntime.Body, wantSkillRuntime)
	}
	if state, ok := persistedSkill.Skills[46]; !ok || state.Level != 1 || !state.Enabled {
		t.Fatalf("level-up passive was not retained in committed skill record=%+v", persistedSkill.Skills)
	}
	acceptable, rest := splitGameServerUpperPacket(t, rest)
	active, rest := splitGameServerUpperPacket(t, rest)
	if acceptable.Header.Classification != 0 || acceptable.Header.MsgID != currentAcceptableQuestListMsgID ||
		active.Header.Classification != 0 || active.Header.MsgID != currentActiveQuestSnapshotMsgID {
		t.Fatalf("level task refresh ids=%d/%d then %d/%d", acceptable.Header.Classification, acceptable.Header.MsgID, active.Header.Classification, active.Header.MsgID)
	}
	assertFinishBridgeSuccessorSnapshots(t, []dnfproto.ChannelPacket{acceptable, active}, 8800, 8801)
	assertCurrentDungeonFinalClearTail(t, rest)
}

func currentDungeonLevelGatedQuestCatalog(t *testing.T) *dnfquest.Catalog {
	t.Helper()
	source := finishBridgePVFSource{
		dnfquest.DefaultList:     "8801 `level_gate.qst`\n",
		"n_quest/level_gate.qst": "[grade]\n`[epic]`\n[type]\n`[seeking]`\n[difficulty]\n`N`\n[reward type]\n`[item]`\n[reward int data]\n10404 1\n[level]\n21 99\n[job]\n`[all]`\n[exposed by npc]\n1\n",
	}
	index, err := dnfpvf.Build(context.Background(), source, dnfpvf.BuildOptions{Lists: []string{dnfquest.DefaultList}})
	if err != nil {
		t.Fatal(err)
	}
	catalog, err := dnfquest.Load(context.Background(), index)
	if err != nil {
		t.Fatal(err)
	}
	return catalog
}

func assertCurrentDungeonFinalClearTail(t *testing.T, data []byte) {
	t.Helper()
	bossCheck, rest := splitGameServerUpperPacket(t, data)
	if bossCheck.Header.Classification != 0 || bossCheck.Header.MsgID != uint16(dnfenum.CmdPacketNotifyBossDieCheck) ||
		len(bossCheck.Body) != currentDungeonBossDieCheckResponseSize {
		t.Fatalf("boss check=%+v body=%x rest=%x", bossCheck.Header, bossCheck.Body, rest)
	}
	settlement, rest := splitGameServerUpperPacket(t, rest)
	if settlement.Header.Classification != 0 || settlement.Header.MsgID != currentDungeonSettlementEntryMsgID ||
		!bytes.Equal(settlement.Body, []byte{0}) || len(rest) != 0 {
		t.Fatalf("settlement=%+v body=%x rest=%x", settlement.Header, settlement.Body, rest)
	}
}

func TestCurrentDungeonMonsterExperienceLevelUpCommitsSPWithExperience(t *testing.T) {
	service, runtime, repositories := prepareCurrentDungeonMonsterExperienceTest(t)
	catalog, err := service.dungeonMonsterCatalog()
	if err != nil {
		t.Fatal(err)
	}
	source := catalog.source.(bridgePVFSource)
	// The level-20 threshold is crossed by this ordinary kill; level 21 has
	// a later threshold so the result is exactly one level and one PVF SP row.
	source[progression.ExperienceTablePath] = strings.TrimSpace(strings.Repeat("0 ", 19) + "100 1000")
	source[progression.SkillPointTablePath] = "[sp table]\n20 20\n21 10\n[/sp table]\n[tp table]\n50 1\n[/tp table]\n"

	session := &gameSession{selectedCharacterID: 99, dungeon: dungeonSessionState{runtime: runtime}}
	monster := runtime.Room.Snapshot().Monsters[0]
	result, err := service.awardCurrentDungeonMonsterExperience(context.Background(), session, runtime, monster)
	if err != nil {
		t.Fatal(err)
	}
	if result.PreviousLevel != 20 || result.Award.Gain != 200 || result.Character.Level != 21 || result.Character.Stats["exp"] != 200 || result.SPGain != 10 ||
		result.Skill.Points != (dnfrepo.SkillPointState{TotalSP: 30, RemainingSP: 30, SyncedLevel: 21}) {
		t.Fatalf("result=%+v", result)
	}
	// 未满级击杀现在也要进骁勇结晶：全额经验进结晶账本，但没有满级部分，
	// 所以胶囊槽与名誉经验都不动（2026-09-24 服主口径）。
	if result.AdventureGroupCredit != 200 || result.AdventureGroupMaximum != 0 || result.HonorExpertGain != 0 {
		t.Fatalf(
			"sub-cap adventure-group credit=%d maximum=%d honor=%d",
			result.AdventureGroupCredit,
			result.AdventureGroupMaximum,
			result.HonorExpertGain,
		)
	}
	creditAccount, found, err := repositories.Account.Load(context.Background(), "account-1")
	if err != nil || !found {
		t.Fatalf("credit account found=%t err=%v", found, err)
	}
	if creditAccount.HonorExp != 0 {
		t.Fatalf("sub-cap kill wrote honor_exp=%d, want 0", creditAccount.HonorExp)
	}
	character, _, err := repositories.Character.Load(context.Background(), "99")
	if err != nil || character.Level != 21 || character.Stats["exp"] != 200 {
		t.Fatalf("persisted character=%+v err=%v", character, err)
	}
	skill, _, err := repositories.Skill.Load(context.Background(), "99")
	if err != nil || skill.Points != result.Skill.Points {
		t.Fatalf("persisted skill=%+v err=%v", skill, err)
	}
}

func TestCurrentDungeonMonsterExperienceAtCapCommitsHonorExpertWithProgression(t *testing.T) {
	service, runtime, repositories, entryExperience := prepareCurrentDungeonMonsterExperienceCapTest(t)
	session := &gameSession{selectedCharacterID: 99, dungeon: dungeonSessionState{runtime: runtime}}
	result, err := service.awardCurrentDungeonMonsterExperience(
		context.Background(), session, runtime, runtime.Room.Snapshot().Monsters[0],
	)
	if err != nil {
		t.Fatal(err)
	}
	if result.HonorExpertGain != 10 || result.Character.Level != currentAdventureCharacterLevelCap ||
		result.Character.Stats["exp"] != int64(entryExperience) ||
		result.PresentedTotalExperience != entryExperience+10 ||
		result.Character.Stats[currentHonorExpertLevelStatKey] != 1 ||
		result.Character.Stats[currentHonorExpertProgressExperienceStatKey] != 5 {
		t.Fatalf("result=%+v", result)
	}
	// 同一笔经验必须同时镜像进账号冒险团。满级角色的整笔经验都属于"越过上限"
	// 的部分，所以进结晶的经验与进胶囊槽/名誉的经验相等
	// （这里没有装冒险团运行时 PVF，结晶换算由 adventuregroup 的用例覆盖）。
	if result.AdventureGroupCredit != uint64(result.HonorExpertGain) ||
		result.AdventureGroupMaximum != uint64(result.HonorExpertGain) {
		t.Fatalf(
			"adventure-group credit=%d maximum=%d want=%d",
			result.AdventureGroupCredit,
			result.AdventureGroupMaximum,
			result.HonorExpertGain,
		)
	}
	account, found, err := repositories.Account.Load(context.Background(), "account-1")
	if err != nil || !found {
		t.Fatalf("account found=%t err=%v", found, err)
	}
	if account.HonorExp != uint64(result.HonorExpertGain) {
		t.Fatalf("account honor_exp=%d want=%d", account.HonorExp, result.HonorExpertGain)
	}
	persisted, _, err := repositories.Character.Load(context.Background(), "99")
	if err != nil || persisted.Stats[currentHonorExpertLevelStatKey] != 1 ||
		persisted.Stats[currentHonorExpertProgressExperienceStatKey] != 5 {
		t.Fatalf("persisted=%+v err=%v", persisted, err)
	}
	body := buildCurrentFinishLoadingCharacterStateBody(result.Character, result.Skill.Points)
	if got := binary.LittleEndian.Uint32(body[55:59]); got != 1 {
		t.Fatalf("op37 HonorExpert level=%d", got)
	}
	if got := binary.LittleEndian.Uint64(body[59:67]); got != 5 {
		t.Fatalf("op37 HonorExpert progress=%d", got)
	}
}

// prepareCurrentDungeonMonsterExperienceCapTest builds the shared level-cap
// fixture: a cap-level character whose persisted EXP already sits above the cap
// entry threshold, the cap honor table, and the account row the award
// transaction requires. entryExperience is the cumulative EXP the cap freezes
// at, i.e. the value op37 must now advertise for capped kills.
func prepareCurrentDungeonMonsterExperienceCapTest(
	t *testing.T,
) (*Service, *runtimeDungeonState, dnfrepo.Group, uint32) {
	t.Helper()
	service, runtime, repositories := prepareCurrentDungeonMonsterExperienceTest(t)
	catalog, err := service.dungeonMonsterCatalog()
	if err != nil {
		t.Fatal(err)
	}
	source, ok := catalog.source.(bridgePVFSource)
	if !ok {
		t.Fatalf("monster catalog source type=%T", catalog.source)
	}
	source[progression.ExperienceTablePath] = strings.TrimSpace(strings.Repeat("1000 ", currentAdventureCharacterLevelCap))
	source[progression.SkillPointTablePath] = "[sp table]\n20 20\n90 90\n[/sp table]\n[tp table]\n50 1\n[/tp table]\n"
	resources, err := currentDungeonMonsterExperienceResourcesForCatalog(catalog)
	if err != nil {
		t.Fatal(err)
	}
	entryExperience, err := resources.Progression.ThresholdToNext(currentAdventureCharacterLevelCap - 1)
	if err != nil {
		t.Fatal(err)
	}
	honorTables, err := dnfhonor.LoadTables(honorServiceTestSource{
		dnfhonor.TablePath: strings.Replace(honorServiceTestDocument(), "1 `100`", "1 `5`", 1),
	})
	if err != nil {
		t.Fatal(err)
	}
	service.honorTable = honorTables

	character, found, err := repositories.Character.Load(context.Background(), "99")
	if err != nil || !found {
		t.Fatalf("load character found=%t err=%v", found, err)
	}
	character.Level = currentAdventureCharacterLevelCap
	character.Stats = map[string]int64{"fatigue": 100, "exp": int64(entryExperience + 123), "grow_type": 0}
	if err := repositories.Character.Save(context.Background(), character); err != nil {
		t.Fatal(err)
	}
	skill, found, err := repositories.Skill.Load(context.Background(), "99")
	if err != nil || !found {
		t.Fatalf("load skill found=%t err=%v", found, err)
	}
	skill.Points = dnfrepo.SkillPointState{TotalSP: 20, RemainingSP: 20, SyncedLevel: currentAdventureCharacterLevelCap}
	if err := repositories.Skill.Save(context.Background(), skill); err != nil {
		t.Fatal(err)
	}
	runtime.Character = dnfrepo.CloneCharacter(character)
	// 推荐副本判据读进图等级快照（runtime.entryCharacterLevel），测试直接替换
	// runtime.Character 时必须同步刷新，否则快照会停在 fixture 建的那个等级上。
	runtime.entryCharacterLevel = character.Level

	// 账号行必须存在：满级击杀经验现在会在同一个事务里记进账号级冒险团状态
	// （骁勇结晶 / 成长胶囊槽 / 荣誉经验），账号缺失要整笔失败而不是静默丢弃。
	if err := repositories.Account.Save(context.Background(), dnfrepo.AccountRecord{AccountID: "account-1"}); err != nil {
		t.Fatal(err)
	}
	return service, runtime, repositories, entryExperience
}

// TestCurrentMonsterKillOp37TotalExperienceOnlySilencesLevelCapKills pins both
// branches of the op37 +0x01 decision: 非满级一律沿用 presented（客户端照旧弹
// "获得X(基础…+成长之契约奖励…)"），满级压回持久化经验（客户端差值为 0 → 不再
// 弹提示，而经验照旧进名誉经验/骁勇结晶）。
func TestCurrentMonsterKillOp37TotalExperienceOnlySilencesLevelCapKills(t *testing.T) {
	subCap := currentDungeonMonsterExperienceCommitResult{
		Character: dnfrepo.CharacterRecord{
			Level: currentAdventureCharacterLevelCap - 1,
			Stats: map[string]int64{"exp": 1200},
		},
		PresentedTotalExperience: 1500,
	}
	if got := currentMonsterKillOp37TotalExperience(subCap); got != 1500 {
		t.Fatalf("sub-cap op37 total exp=%d, want presented 1500", got)
	}
	atCap := currentDungeonMonsterExperienceCommitResult{
		Character: dnfrepo.CharacterRecord{
			Level: currentAdventureCharacterLevelCap,
			Stats: map[string]int64{"exp": 1732686583},
		},
		PresentedTotalExperience: 1732686898,
	}
	if got := currentMonsterKillOp37TotalExperience(atCap); got != 1732686583 {
		t.Fatalf("level-cap op37 total exp=%d, want durable 1732686583", got)
	}
}

// TestCurrentDungeonMonsterDeathAtCapSendsNoExperienceNotice drives the real
// kill path at the level cap: the emitted op37 must carry the durable EXP (so
// the client's difference is zero and it stops printing one exp line per kill)
// while the honor-expert block keeps advancing, which is where a capped award
// actually goes.
func TestCurrentDungeonMonsterDeathAtCapSendsNoExperienceNotice(t *testing.T) {
	service, runtime, _, entryExperience := prepareCurrentDungeonMonsterExperienceCapTest(t)
	connection := &bufferConn{}
	session := &gameSession{
		conn:                connection,
		connID:              "dungeon-monster-experience-cap-notice-test",
		selectedCharacterID: 99,
		dungeon:             dungeonSessionState{runtime: runtime},
	}
	monster := runtime.Room.Snapshot().Monsters[0]
	request := make([]byte, dungeoncmd.DieMonsterRequestSize)
	binary.LittleEndian.PutUint32(request[0:4], monster.ObjectKey)
	binary.LittleEndian.PutUint16(request[4:6], currentSceneActorObjectKey(99))

	if err := service.handleDungeonMonsterDeath(session, request); err != nil {
		t.Fatal(err)
	}
	death, rest := splitGameServerUpperPacket(t, connection.write.Bytes())
	if death.Header.Classification != 0 || death.Header.MsgID != uint16(dnfenum.CmdPacketNotifyDieMonster) {
		t.Fatalf("death=%+v", death.Header)
	}
	state, _ := splitGameServerUpperPacket(t, rest)
	if state.Header.Classification != 0 || state.Header.MsgID != currentDungeonCharacterStateMsgID ||
		len(state.Body) != currentFinishLoadingCharacterStateBodySize {
		t.Fatalf("state=%+v body_len=%d", state.Header, len(state.Body))
	}
	if got := binary.LittleEndian.Uint32(state.Body[1:5]); got != entryExperience {
		t.Fatalf("level-cap op37 total exp=%d, want durable %d", got, entryExperience)
	}
	// 名誉经验块照旧推进：满级那笔经验没有丢，只是不再播报。
	if got := binary.LittleEndian.Uint32(state.Body[0x37:0x3B]); got != 1 {
		t.Fatalf("op37 honor-expert level=%d, want 1", got)
	}
	if got := binary.LittleEndian.Uint64(state.Body[0x3B:0x43]); got != 5 {
		t.Fatalf("op37 honor-expert progress=%d, want 5", got)
	}
}

func prepareCurrentDungeonMonsterExperienceTest(t *testing.T) (*Service, *runtimeDungeonState, dnfrepo.Group) {
	t.Helper()
	service, runtime, repositories := prepareCurrentDungeonDropTest(t, 0, 3227)
	catalog, err := service.dungeonMonsterCatalog()
	if err != nil {
		t.Fatal(err)
	}
	source, ok := catalog.source.(bridgePVFSource)
	if !ok {
		t.Fatalf("monster catalog source type=%T", catalog.source)
	}
	installCurrentDungeonMonsterExperienceTestPVF(source)
	statTable, err := dnfcharstat.Load(context.Background(), source, dnfcharstat.Options{})
	if err != nil {
		t.Fatalf("load character stat test PVF: %v", err)
	}
	service.characterStats = statTable
	// 账号行必须存在：从 2026-09-24 起**任意等级**的击杀经验都会在同一事务里
	// 镜像进账号级骁勇结晶，账号缺失要整笔失败而不是静默丢掉这笔经验。
	if err := repositories.Account.Save(context.Background(), dnfrepo.AccountRecord{AccountID: "account-1"}); err != nil {
		t.Fatal(err)
	}

	character, found, err := repositories.Character.Load(context.Background(), "99")
	if err != nil || !found {
		t.Fatalf("load character found=%t err=%v", found, err)
	}
	character.Level = 20
	character.Job = "0"
	character.Stats = map[string]int64{"fatigue": 100, "exp": 0, "grow_type": 0}
	if err := repositories.Character.Save(context.Background(), character); err != nil {
		t.Fatal(err)
	}
	if err := repositories.Skill.Save(context.Background(), dnfrepo.SkillRecord{
		CharacterID: "99",
		Skills:      map[int64]dnfrepo.SkillState{46: {Level: 1, Enabled: true}},
		Points: dnfrepo.SkillPointState{
			TotalSP: 20, RemainingSP: 20, SyncedLevel: 20,
		},
		Trees: map[int]dnfrepo.SkillTreeState{
			0: {
				Skills: map[int64]dnfrepo.SkillState{46: {Level: 1, Enabled: true}},
				Points: dnfrepo.SkillPointState{TotalSP: 20, RemainingSP: 20, SyncedLevel: 20},
			},
			1: {
				Skills: map[int64]dnfrepo.SkillState{46: {Level: 1, Enabled: true}},
				Points: dnfrepo.SkillPointState{TotalSP: 20, RemainingSP: 20, SyncedLevel: 20},
			},
		},
		Layouts: map[int]dnfrepo.SkillLayout{0: {0: 46}, 1: {0: 46}},
	}); err != nil {
		t.Fatal(err)
	}
	runtime.Character = dnfrepo.CloneCharacter(character)
	// 推荐副本判据读进图等级快照（runtime.entryCharacterLevel），测试直接替换
	// runtime.Character 时必须同步刷新，否则快照会停在 fixture 建的那个等级上。
	runtime.entryCharacterLevel = character.Level
	return service, runtime, repositories
}

func installCurrentDungeonMonsterExperienceTestPVF(source bridgePVFSource) {
	source["character/character.lst"] = "0 `test.chr`\n"
	source["character/test.chr"] = "[initial value]\n[HP MAX] 100\n[MP MAX] 100\n[physical attack] 10\n" +
		"[growtype 1]\n[HP MAX] 1\n[MP MAX] 1\n[physical attack] 1\n"
	source[progression.ExperienceTablePath] = strings.TrimSpace(strings.Repeat("1000 ", 20))
	source[progression.SkillPointTablePath] = "[sp table]\n20 20\n[/sp table]\n[tp table]\n50 1\n[/tp table]\n"
	source[progression.QuestParameterPath] = "[difficulty]\n`N` 100\n[/difficulty]\n" +
		"[exp reward table]\n100 -1\n200 -1\n300 -1\n400 -1\n500 -1\n" +
		"[gold reward table]\n0 10\n" +
		"[green level penalty]\n80\n[grey level penalty]\n30\n"

	var monsterTable strings.Builder
	for level := 1; level <= 20; level++ {
		value := 30 + (level-1)*3
		if level == 20 {
			value = 100
		}
		monsterTable.WriteString(strconv.Itoa(level))
		monsterTable.WriteByte(' ')
		monsterTable.WriteString(strconv.Itoa(value))
		monsterTable.WriteByte(' ')
	}
	source[progression.MonsterExperienceTablePath] = strings.TrimSpace(monsterTable.String())
	source[progression.ServerParameterPath] = "[monster exp bonusrate]\n1.00\n" +
		"[party user number exp bonusrate]\n1 2 3 4\n" +
		"[party user number exp bonusrate starter server]\n1 2 3 4\n" +
		"[dungeon difficulty exp bonusrate]\n1.30 2.00 2.50 3.00 4.00\n" +
		"[clear exp bonusrate]\n1.00\n" +
		"[clear rank exp bonusrate]\n0.05 0.10 0.12 0.15 0.20\n" +
		"[experience increasing point]\n700 1.00\n[/experience increasing point]\n"
	source[progression.WorldMapExperiencePenaltyPath] = "[penalty table info]\n" +
		"-16 0.30 -15 0.50 -14 0.50 -13 0.50 -12 0.70 -11 0.70 -10 0.70 -9 0.80 -8 0.80 -7 0.80 -6 0.90 -5 0.90 -4 0.90 -3 1.00 -2 1.00 -1 1.00 0 1.00 1 1.00 2 1.00 3 1.00 4 1.00 5 1.00 6 0.90 7 0.90 8 0.90 9 0.80 10 0.80 11 0.80 12 0.70 13 0.70 14 0.70 15 0.50 16 0.50 17 0.50 18 0.30 19 0.30 20 0.30\n" +
		"[/penalty table info]\n"
}

func TestCurrentDungeonMonsterExperienceGrowthContractAddsTwentyPercent(t *testing.T) {
	service, runtime, repositories := prepareCurrentDungeonMonsterExperienceTest(t)
	service.premiumCatalog = &currentPremiumCatalog{
		effectsByType: map[int64]currentPremiumEffectInfo{
			premium.TypeBonusExp: {BonusExperiencePercent: 20},
		},
	}
	future := time.Now().Add(24 * time.Hour).Unix()
	if err := repositories.Account.Save(context.Background(), dnfrepo.AccountRecord{
		AccountID: "account-1",
		Metadata:  map[string]string{"premium_expire_84": strconv.FormatInt(future, 10)},
	}); err != nil {
		t.Fatal(err)
	}
	session := &gameSession{selectedCharacterID: 99, dungeon: dungeonSessionState{runtime: runtime}}
	monster := runtime.Room.Snapshot().Monsters[0]
	result, err := service.awardCurrentDungeonMonsterExperience(context.Background(), session, runtime, monster)
	if err != nil {
		t.Fatal(err)
	}
	if result.Award.Gain != 200 || result.GrowthContractBonus != 40 {
		t.Fatalf("result award=%+v bonus=%d, want gain=200 bonus=40", result.Award, result.GrowthContractBonus)
	}
	if result.Character.Stats["exp"] != 240 {
		t.Fatalf("result character exp=%d, want 240", result.Character.Stats["exp"])
	}
	stateBody := buildCurrentFinishLoadingCharacterStateBodyWithPresentation(
		result.Character,
		result.Skill.Points,
		&currentFinishLoadingExperiencePresentation{GrowthContractBonus: result.GrowthContractBonus},
	)
	if binary.LittleEndian.Uint32(stateBody[1:5]) != 240 ||
		binary.LittleEndian.Uint32(stateBody[30:34]) != 40 ||
		binary.LittleEndian.Uint32(stateBody[38:42]) != 0 {
		t.Fatalf("growth-contract op37 body=%x", stateBody)
	}
	character, _, err := repositories.Character.Load(context.Background(), "99")
	if err != nil || character.Stats["exp"] != 240 || character.Level != 20 {
		t.Fatalf("persisted character=%+v err=%v", character, err)
	}

	// Expired contract: no bonus.
	account, _, _ := repositories.Account.Load(context.Background(), "account-1")
	account.Metadata["premium_expire_84"] = strconv.FormatInt(time.Now().Add(-time.Hour).Unix(), 10)
	if err := repositories.Account.Save(context.Background(), account); err != nil {
		t.Fatal(err)
	}
	if bonus := service.currentGrowthContractBonusExp(context.Background(), "account-1", 200); bonus != 0 {
		t.Fatalf("expired contract bonus=%d, want 0", bonus)
	}
}

func TestCurrentDungeonMonsterExperienceAddsValidEquippedAndQuickSlotExpAdvantage(t *testing.T) {
	service, runtime, repositories := prepareCurrentDungeonMonsterExperienceTest(t)
	service.equipmentExpAdvantages = map[int64]int64{
		400300054: 5,
		400300055: 10,
		400300056: 50,
		400300057: 20,
		400300058: 70,
	}
	if err := repositories.Equipment.Save(context.Background(), dnfrepo.EquipmentRecord{
		CharacterID: "99",
		Entries: map[string]dnfrepo.EquipmentEntry{
			"19": {SlotIndex: 19, ItemID: 400300054},
			"20": {SlotIndex: 20, ItemID: 400300055},
			"21": {
				SlotIndex: 21,
				ItemID:    400300056,
				ExpireAt:  time.Now().Add(-time.Minute),
			},
		},
	}); err != nil {
		t.Fatal(err)
	}
	if err := repositories.Inventory.Save(context.Background(), dnfrepo.InventoryRecord{
		CharacterID: "99",
		Slots: map[string]dnfrepo.ItemStack{
			"0:3":  {ItemID: 400300057, Count: 99},
			"0:8":  {ItemID: 400300054, Count: 1},
			"0:9":  {ItemID: 400300058, Count: 1},
			"0:65": {ItemID: 400300058, Count: 1},
			"0:6": {
				ItemID:   400300058,
				Count:    1,
				ExpireAt: time.Now().Add(-time.Minute),
			},
		},
	}); err != nil {
		t.Fatal(err)
	}

	session := &gameSession{
		selectedCharacterID: 99,
		dungeon:             dungeonSessionState{runtime: runtime},
	}
	monster := runtime.Room.Snapshot().Monsters[0]
	result, err := service.awardCurrentDungeonMonsterExperience(context.Background(), session, runtime, monster)
	if err != nil {
		t.Fatal(err)
	}
	// Live worn and quick-slot equipment values are summed before one floor:
	// floor(200 * (5+10+20+5) / 100) = 80. Expired entries and ordinary
	// inventory slots are ignored, and the quick-slot stack count is not used.
	if result.Award.Gain != 200 || result.EquipmentBonus != 80 || result.GrowthContractBonus != 0 {
		t.Fatalf("result award=%+v equipment=%d growth=%d", result.Award, result.EquipmentBonus, result.GrowthContractBonus)
	}
	if result.Character.Stats["exp"] != 280 {
		t.Fatalf("result character exp=%d, want 280", result.Character.Stats["exp"])
	}
	stateBody := buildCurrentFinishLoadingCharacterStateBodyWithPresentation(
		result.Character,
		result.Skill.Points,
		&currentFinishLoadingExperiencePresentation{},
	)
	if binary.LittleEndian.Uint32(stateBody[30:34]) != 0 ||
		binary.LittleEndian.Uint32(stateBody[83:87]) != 0 {
		t.Fatalf("equipment-bonus op37 body=%x", stateBody)
	}
	character, _, err := repositories.Character.Load(context.Background(), "99")
	if err != nil || character.Stats["exp"] != 280 {
		t.Fatalf("persisted character=%+v err=%v", character, err)
	}
}

func TestRecordCurrentDungeonMonsterExperienceTracksResultWindowCategories(t *testing.T) {
	runtime := &runtimeDungeonState{}
	recordCurrentDungeonMonsterExperience(runtime, runtimeDungeonMonster{
		Spawn: worldmap.MonsterSpawn{MonsterID: 1, Rank: "[champion]"},
	}, currentDungeonMonsterExperienceCommitResult{
		// 结果窗累计读的是**实际支付**的 Award.Gain（等级惩罚之后），不是
		// PrePenaltyGain；两者都要给，否则这条用例会把"未支付"当成"已支付"。
		Award:               currentDungeonMonsterExperienceAward{MonsterID: 1, MonsterType: 1, PrePenaltyGain: 100, Gain: 100},
		GrowthContractBonus: 20,
		EquipmentBonus:      5,
	})
	recordCurrentDungeonMonsterExperience(runtime, runtimeDungeonMonster{
		Spawn: worldmap.MonsterSpawn{MonsterID: 2, Rank: "[normal]", SuffixMarker: "[boss]"},
	}, currentDungeonMonsterExperienceCommitResult{
		Award:               currentDungeonMonsterExperienceAward{MonsterID: 2, MonsterType: 0, PrePenaltyGain: 300, Gain: 300},
		GrowthContractBonus: 60,
		EquipmentBonus:      15,
	})
	recordCurrentDungeonMonsterExperience(runtime, runtimeDungeonMonster{
		Spawn: worldmap.MonsterSpawn{MonsterID: 3, Rank: "[super champion]"},
	}, currentDungeonMonsterExperienceCommitResult{
		Award: currentDungeonMonsterExperienceAward{
			MonsterID: 3, MonsterType: 2, PrePenaltyGain: 200, Gain: 200, NamedMonster: true,
		},
		GrowthContractBonus: 40,
		EquipmentBonus:      10,
	})

	if runtime.settlementMonsterExperienceTotal != 600 ||
		runtime.settlementMonsterGrowthContractBonus != 120 ||
		runtime.settlementMonsterEquipmentBonus != 30 ||
		runtime.settlementChampionExperience != 100 ||
		runtime.settlementSuperChampionExperience != 0 ||
		runtime.settlementBossExperience != 300 {
		t.Fatalf("monster result statistics=%+v", runtime)
	}
}

func TestRecordCurrentDungeonMonsterExperienceTracksObjectKeyRows(t *testing.T) {
	runtime := &runtimeDungeonState{}
	recordCurrentDungeonMonsterExperience(runtime, runtimeDungeonMonster{
		ObjectKey: 0x195,
		Spawn:     worldmap.MonsterSpawn{MonsterID: 1, Rank: "[champion]"},
	}, currentDungeonMonsterExperienceCommitResult{
		Award: currentDungeonMonsterExperienceAward{
			MonsterID: 1, MonsterType: 1, PrePenaltyGain: 1129, Gain: 1054,
		},
	})
	rows := currentDungeonSettlementMonsterExperienceRows(runtime)
	if len(rows) != 1 || rows[0] != (currentDungeonClearRewardPair{Key: 0x195, Value: 1054}) {
		t.Fatalf("monster object-key rows=%+v, want key=0x195 value=1054", rows)
	}
}

func TestFatigueBatteryExperienceAddsThirtyPercent(t *testing.T) {
	if got := fatigueBatteryExperience(101); got != 131 {
		t.Fatalf("fatigueBatteryExperience(101)=%d, want 131", got)
	}
}
