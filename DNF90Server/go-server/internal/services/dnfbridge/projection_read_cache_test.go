package dnfbridge

import (
	"context"
	"testing"

	dnfrepo "longheng.io/server/internal/modules/dnf/repository"
	dnfrepomemory "longheng.io/server/internal/modules/dnf/repository/memory"
)

// 本文件是 projection_read_cache.go 的行为契约测试。
//
// 扇出把「一个演员的投影只算一次」变成批量只读缓存之后，真正要守住的不变量有两个：
//
//  1. 缓存对线上包流完全透明 —— 同一套夹具下，「扇出（缓存开）」与「同一个有序
//     配对循环、走单对入口（缓存关）」必须给出逐字节相同的每连接包流，而且缓存
//     确实把仓储读降下来了。协和压测夹具（stress_raid_projection_test.go）里
//     还有包数与 install 计数的强断言，这里补的是"包内容不变"这一条。
//  2. 命中返回的是深拷贝，写入让缓存失效 —— 否则一个接收者就地改包里的 map/slice
//     会泄漏给下一个接收者；已经缓存的「有」和「没有」在写库之后都必须重读。

// projectionRunUncachedPairLoop 跑与
// syncRuntimeRaidMateActorsAllAtSceneBoundary 完全相同的顺序（成员排序 + 自身
// 跳过 + 自己跳过自己），但用不带 ctx 的单对入口，因此这一次批量里没有缓存。
func projectionRunUncachedPairLoop(fixture *stressRaidProjectionFixture, source string) {
	state, ok := fixture.service.runtimeRaidForMember(fixture.members[0].charID)
	if !ok {
		fixture.t.Fatalf("reference loop: leader does not resolve a raid")
	}
	members := append([]runtimeRaidMemberState(nil), state.FrozenMembers...)
	runtimeRaidSortMembers(members)
	for _, self := range members {
		selfSession, online := fixture.service.onlineGameSession(self.CharID)
		if !online || selfSession == nil || selfSession.conn == nil {
			continue
		}
		for _, mate := range members {
			if mate.CharID == self.CharID {
				continue
			}
			fixture.service.sendRuntimeRaidPeerActor(state, selfSession, mate.CharID, source)
		}
	}
}

func TestProjectionReadCacheIsTransparentOnTheRaidFanout(t *testing.T) {
	const memberCount = 8

	cached := newStressRaidProjectionFixture(t, memberCount)
	reference := newStressRaidProjectionFixture(t, memberCount)

	cachedRun := cached.runSceneBoundary("projection_read_cache_transparency", false)

	reference.resetWire()
	before := reference.counts.snapshot()
	projectionRunUncachedPairLoop(reference, "projection_read_cache_transparency")
	referenceRun := reference.counts.snapshot().deltaFrom(before)

	if cachedRun.writes != reference.totalWrites() {
		t.Fatalf("cache changed the packet count: cached=%d reference=%d", cachedRun.writes, reference.totalWrites())
	}
	for index := range cached.members {
		cachedStream := cached.members[index].conn.stream()
		referenceStream := reference.members[index].conn.stream()
		if string(cachedStream) != string(referenceStream) {
			t.Fatalf("member %s received a different packet stream under the batch read cache (%d vs %d bytes)",
				cached.members[index].name, len(cachedStream), len(referenceStream))
		}
	}
	if cachedRun.reads >= referenceRun.reads {
		t.Fatalf("the batch read cache did not reduce repository reads: cached=%d reference=%d",
			cachedRun.reads, referenceRun.reads)
	}
	t.Logf("N=%d batch read cache transparent: packets=%d, per-conn streams byte-identical to the uncached pair loop; repo_reads %d -> %d",
		memberCount, cachedRun.writes, referenceRun.reads, cachedRun.reads)
}

func TestProjectionReadCacheWriteInvalidation(t *testing.T) {
	ctx := context.Background()
	inner := dnfrepomemory.NewMemoryGroup()
	cache := newProjectionReadCache()
	repos := newProjectionReadWrappers(inner, cache).apply(inner)

	// 1) 命中必须是深拷贝，且写入之后必须重读。
	if err := inner.Equipment.Save(ctx, dnfrepo.EquipmentRecord{
		CharacterID: "1",
		Entries:     map[string]dnfrepo.EquipmentEntry{"1": {SlotIndex: 1, ItemID: 111}},
	}); err != nil {
		t.Fatalf("seed equipment: %v", err)
	}
	if first, found, err := repos.Equipment.Load(ctx, "1"); err != nil || !found || first.Entries["1"].ItemID != 111 {
		t.Fatalf("first load = %+v found=%t err=%v", first, found, err)
	}
	hit, found, err := repos.Equipment.Load(ctx, "1")
	if err != nil || !found || hit.Entries["1"].ItemID != 111 {
		t.Fatalf("cached load = %+v found=%t err=%v", hit, found, err)
	}
	hit.Entries["1"] = dnfrepo.EquipmentEntry{SlotIndex: 1, ItemID: 999}
	if after, _, _ := repos.Equipment.Load(ctx, "1"); after.Entries["1"].ItemID != 111 {
		t.Fatalf("cache hit shared mutable state: item=%d want 111", after.Entries["1"].ItemID)
	}
	if err := repos.Equipment.Save(ctx, dnfrepo.EquipmentRecord{
		CharacterID: "1",
		Entries:     map[string]dnfrepo.EquipmentEntry{"1": {SlotIndex: 1, ItemID: 222}},
	}); err != nil {
		t.Fatalf("save equipment: %v", err)
	}
	if rewritten, _, _ := repos.Equipment.Load(ctx, "1"); rewritten.Entries["1"].ItemID != 222 {
		t.Fatalf("write did not invalidate the cached record: item=%d want 222", rewritten.Entries["1"].ItemID)
	}

	// 2) 缓存「不存在」之后写入，再读必须变成存在。
	if _, found, _ := repos.Settings.Load(ctx, "global:zz"); found {
		t.Fatalf("settings global:zz unexpectedly present")
	}
	if _, found, _ := repos.Settings.Load(ctx, "global:zz"); found {
		t.Fatalf("cached miss lookup reported present")
	}
	if err := repos.Settings.Save(ctx, dnfrepo.SettingsRecord{
		Scope:  "global:zz",
		Values: map[string]string{"a": "b"},
	}); err != nil {
		t.Fatalf("save settings: %v", err)
	}
	if _, found, err := repos.Settings.Load(ctx, "global:zz"); err != nil || !found {
		t.Fatalf("write did not invalidate the cached miss: found=%t err=%v", found, err)
	}

	// 3) 角色初始化回填走 CharacterCreate 事务视图；视图里的写入必须让外层缓存失效。
	if _, found, _ := repos.Inventory.Load(ctx, "7"); found {
		t.Fatalf("inventory 7 unexpectedly present")
	}
	creation := inner.CharacterCreate
	if creation == nil {
		t.Fatal("memory group has no character creation unit of work")
	}
	created := projectionMemoCharacterCreateUnitOfWork{CharacterCreationUnitOfWork: creation, cache: cache}
	if err := created.WithinCharacterCreation(ctx, "7", inner, func(tx dnfrepo.Group) error {
		if tx.Inventory == nil {
			return nil
		}
		_, found, loadErr := tx.Inventory.Load(ctx, "7")
		if loadErr != nil || found {
			return loadErr
		}
		return tx.Inventory.Save(ctx, dnfrepo.InventoryRecord{CharacterID: "7"})
	}); err != nil {
		t.Fatalf("WithinCharacterCreation: %v", err)
	}
	if _, found, err := repos.Inventory.Load(ctx, "7"); err != nil || !found {
		t.Fatalf("character-creation write did not invalidate the cached miss: found=%t err=%v", found, err)
	}

	// 4) 技能修复走 CharacterSkills 事务视图，改的是**已经存在**的记录；这类写入
	//    同样必须让外层缓存失效，否则本批次后面的配对会按旧技能页发包。
	if err := inner.Skill.Save(ctx, dnfrepo.SkillRecord{
		CharacterID: "8",
		Skills:      map[int64]dnfrepo.SkillState{7: {Level: 1, Enabled: true}},
	}); err != nil {
		t.Fatalf("seed skill: %v", err)
	}
	if before, found, err := repos.Skill.Load(ctx, "8"); err != nil || !found || before.Skills[7].Level != 1 {
		t.Fatalf("skill first load = %+v found=%t err=%v", before, found, err)
	}
	skills := inner.CharacterSkills
	if skills == nil {
		t.Fatal("memory group has no character skill unit of work")
	}
	repair := projectionMemoCharacterSkillsUnitOfWork{CharacterSkillUnitOfWork: skills, cache: cache}
	if err := repair.WithinCharacterSkill(ctx, "8", func(store dnfrepo.SkillRepository) error {
		current, found, loadErr := store.Load(ctx, "8")
		if loadErr != nil || !found {
			return loadErr
		}
		current = dnfrepo.CloneSkill(current)
		current.Skills[7] = dnfrepo.SkillState{Level: 9, Enabled: true}
		return dnfrepo.SaveSkillFields(ctx, store, current, dnfrepo.SkillFieldSkills)
	}); err != nil {
		t.Fatalf("WithinCharacterSkill: %v", err)
	}
	if after, _, err := repos.Skill.Load(ctx, "8"); err != nil || after.Skills[7].Level != 9 {
		t.Fatalf("skill repair did not invalidate the cache: level=%d err=%v want 9", after.Skills[7].Level, err)
	}
}

// TestProjectionReadCacheKeepsUnwrappedFieldsCurrent 守住包装复用的边界：缓存只在
// 底层仓储仍是同一批时才复用已经装箱的包装，而且没被包装的字段每一次都取自
// 当前拿到的组。否则一次批量里换过仓储就会读到别的对象。
func TestProjectionReadCacheKeepsUnwrappedFieldsCurrent(t *testing.T) {
	ctx := context.Background()
	cache := newProjectionReadCache()

	first := dnfrepomemory.NewMemoryGroup()
	first.Quest = dnfrepomemory.NewMemoryGroup().Quest
	decoratedFirst := cache.decoratedRepositories(first)
	if decoratedFirst.Quest != first.Quest {
		t.Fatalf("unwrapped field was not taken from the current group")
	}
	if _, _, err := decoratedFirst.Equipment.Load(ctx, "1"); err != nil {
		t.Fatalf("load through decorated equipment: %v", err)
	}

	// 同一批仓储 + 不同的未包装字段：包装可以复用，未包装字段必须是新的。
	second := first
	second.Quest = dnfrepomemory.NewMemoryGroup().Quest
	if second.Quest == first.Quest {
		t.Fatalf("test fixture failed to swap the quest repository")
	}
	decoratedSecond := cache.decoratedRepositories(second)
	if decoratedSecond.Quest != second.Quest {
		t.Fatalf("reused wrappers carried a stale unwrapped field")
	}

	// 换掉被包装的仓储：必须重新装箱，读要落到新仓储上。
	replacement := dnfrepomemory.NewMemoryGroup()
	if err := replacement.Equipment.Save(ctx, dnfrepo.EquipmentRecord{
		CharacterID: "1",
		Entries:     map[string]dnfrepo.EquipmentEntry{"1": {SlotIndex: 1, ItemID: 333}},
	}); err != nil {
		t.Fatalf("seed replacement equipment: %v", err)
	}
	third := second
	third.Equipment = replacement.Equipment
	decoratedThird := cache.decoratedRepositories(third)
	// 缓存里 "1" 的记录来自旧仓储；换了底层仓储之后不能再命中它。
	record, found, err := decoratedThird.Equipment.Load(ctx, "1")
	if err != nil || !found || record.Entries["1"].ItemID != 333 {
		t.Fatalf("swapped equipment repository was not re-read: record=%+v found=%t err=%v", record, found, err)
	}
}
