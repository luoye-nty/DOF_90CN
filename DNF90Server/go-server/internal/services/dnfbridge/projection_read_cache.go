// 本文件为「一次批量投影」提供角色维度只读缓存。
//
// 背景：进入场景边界（syncRuntimeRaidMateActorsAllAtSceneBoundary）和城镇同屏广播
// （broadcastTownPlayerEnter）都是 (接收者, 演员) 配对的扇出。发给每个接收者的
// actor 投影只由**演员自己的角色 ID** 决定，与接收者无关，因此同一个演员在一次
// 扇出里被完整解析 N-1 次，每次都把同一批仓储读重跑一遍。实测 21 × N(N-1)：
// N=40 时 32760 次读，其中只有 21 × N = 840 次是不同内容。
//
// 这里的机制：一次扇出在 ctx 上挂一个 projectionReadCache，投影路径取仓储时
// 走 Service.projectionRepositories(ctx) 拿到「带只读缓存的仓储组」。缓存只在
// 这次扇出内存活，函数返回即丢弃，过期窗口是微秒级，也不可能把上一次请求的
// 数据带给下一次。
//
// 三条不改变行为的约束：
//
//  1. 命中时一定返回 Clone：底层仓储（mysql / memory / RedisCachedCharStore）
//     每次 Load 都给调用方一份新的记录，缓存命中必须等价，否则调用方一旦就地
//     修改 map/slice 就会泄漏给下一个接收者。仓储包已经为每种记录提供了深拷贝
//     （CloneAccount / CloneCharacter / CloneEquipment / CloneInventory /
//     ClonePet / CloneSkill / CloneSettings），这里直接复用。
//  2. 读失败不入缓存，但「记录不存在」入缓存。失败可能是瞬时的，下一次读应当
//     重试；不存在是一个确定结果，与命中一样要能复用。为了让它安全，第 3 条
//     必须覆盖投影路径上所有可能的写入。
//  3. 写入必须让缓存失效。写入通过包装层原样转发，但在转发前把对应键从缓存里
//     删掉并标记为「本批次已写过」，此后该键在本批次内不再缓存。这样「先读后写
//     再读」一定拿到新值。投影路径上会写库的只有两处，两处都被接上了：
//     characterInitializationPresenceForRecord → characterdata.Initializer（经
//     CharacterCreate 工作单元）只补「刚刚查过且不存在」的记录，dnfskill.SceneOwner
//     的 Backfill / SyncPoints / EnsureTree / EnsureBaseline（经 CharacterSkills
//     工作单元）会改已经存在的技能记录。
//
// 没有包进缓存的仓储字段一律保持原对象：不包装既不会改变行为，也不会因为把
// 可选接口藏起来而改变类型断言的走向（例如 SaveFields 的 FieldStore 分支）。
package dnfbridge

import (
	"context"
	"strings"
	"sync"
	"time"

	dnfrepo "longheng.io/server/internal/modules/dnf/repository"
)

// 缓存键里的仓储标识。同一个键字符串只会和同一个记录类型配对，类型不符时
// 读取路径会退回真实 Load，不会 panic。
const (
	projectionCacheRepoAccount   = "account"
	projectionCacheRepoCharacter = "character"
	projectionCacheRepoEquipment = "equipment"
	projectionCacheRepoInventory = "inventory"
	projectionCacheRepoPet       = "pet"
	projectionCacheRepoSettings  = "settings"
	projectionCacheRepoSkill     = "skill"
)

type projectionReadCacheKey struct {
	repository string
	recordKey  string
}

// projectionCacheMiss 是一条「本次批量里确认不存在」的缓存值。它必须与
// 「没有缓存」区分开，否则不存在的记录每个配对都会重新查库。
type projectionCacheMiss struct{}

// projectionReadCache 一次扇出内的角色维度读缓存。
//
// 键是 (仓储, 记录主键)：记录主键就是角色 ID（Settings 是 scope，Account 是
// 账号 ID）。扇出串行执行，加锁只是为了在将来被并发驱动时仍然正确。
type projectionReadCache struct {
	mu      sync.Mutex
	entries map[projectionReadCacheKey]any
	written map[projectionReadCacheKey]struct{}

	// wraps 是一次批量里已经装箱好的包装仓储。把一个接口值装箱要在堆上分配，
	// 而一次扇出会取仓储上万次，所以只在底层仓储真的换了的时候重建，并且换的
	// 时候连同缓存内容一起丢掉（缓存值来自旧仓储，键里分不出来源）。
	wrapsReady bool
	wrapsInner dnfrepo.Group
	wraps      projectionReadWrappers
}

func newProjectionReadCache() *projectionReadCache {
	return &projectionReadCache{
		entries: make(map[projectionReadCacheKey]any),
		written: make(map[projectionReadCacheKey]struct{}),
	}
}

// lookup 取一条缓存记录。本批次写过的主键永远不命中。
func (cache *projectionReadCache) lookup(repository string, recordKey string) (any, bool) {
	if cache == nil || strings.TrimSpace(recordKey) == "" {
		return nil, false
	}
	key := projectionReadCacheKey{repository: repository, recordKey: recordKey}
	cache.mu.Lock()
	defer cache.mu.Unlock()
	if _, dirty := cache.written[key]; dirty {
		return nil, false
	}
	value, ok := cache.entries[key]
	return value, ok
}

// store 记一条缓存记录。主键为空、或本批次已经写过该主键时不缓存。
func (cache *projectionReadCache) store(repository string, recordKey string, value any) {
	if cache == nil || strings.TrimSpace(recordKey) == "" || value == nil {
		return
	}
	key := projectionReadCacheKey{repository: repository, recordKey: recordKey}
	cache.mu.Lock()
	defer cache.mu.Unlock()
	if _, dirty := cache.written[key]; dirty {
		return
	}
	cache.entries[key] = value
}

// forget 在写入前让一条记录失效，并把主键标记为本批次已写过。
func (cache *projectionReadCache) forget(repository string, recordKey string) {
	if cache == nil {
		return
	}
	key := projectionReadCacheKey{repository: repository, recordKey: recordKey}
	cache.mu.Lock()
	defer cache.mu.Unlock()
	if strings.TrimSpace(recordKey) == "" {
		cache.forgetRepositoryLocked(repository)
		return
	}
	delete(cache.entries, key)
	cache.written[key] = struct{}{}
}

// forgetRepository 让整个仓储失效：不知道写入落在哪个主键时使用。
func (cache *projectionReadCache) forgetRepository(repository string) {
	if cache == nil {
		return
	}
	cache.mu.Lock()
	defer cache.mu.Unlock()
	cache.forgetRepositoryLocked(repository)
}

func (cache *projectionReadCache) forgetRepositoryLocked(repository string) {
	for key := range cache.entries {
		if key.repository == repository {
			delete(cache.entries, key)
			cache.written[key] = struct{}{}
		}
	}
}

type projectionReadCacheContextKey struct{}

// withProjectionReadCache 为一次扇出挂上缓存，并返回带缓存的 ctx。
func withProjectionReadCache(ctx context.Context) (context.Context, *projectionReadCache) {
	if ctx == nil {
		ctx = context.Background()
	}
	cache := newProjectionReadCache()
	return context.WithValue(ctx, projectionReadCacheContextKey{}, cache), cache
}

func projectionReadCacheFromContext(ctx context.Context) *projectionReadCache {
	if ctx == nil {
		return nil
	}
	cache, _ := ctx.Value(projectionReadCacheContextKey{}).(*projectionReadCache)
	return cache
}

// projectionRepositories 是投影路径取仓储的入口。没有缓存时与
// Service.repositoryGroup 完全一致；有缓存时返回同一批仓储的只读缓存包装。
func (s *Service) projectionRepositories(ctx context.Context) (dnfrepo.Group, bool) {
	repositories, ok := s.repositoryGroup()
	if !ok {
		return repositories, false
	}
	cache := projectionReadCacheFromContext(ctx)
	if cache == nil {
		return repositories, true
	}
	return cache.decoratedRepositories(repositories), true
}

// decoratedRepositories 返回同一次批量里可复用的包装仓储组。
//
// 未包装的字段每一次都取自当前 provider 返回的组，所以永远是最新值；只有被包装
// 的那几个字段会复用，而且复用前必须确认底层仓储仍然是同一批（见
// projectionSameRepositories）。判断不出来时一律重建，宁可慢也不会把别的仓储
// 的包装留在手上。
func (cache *projectionReadCache) decoratedRepositories(repositories dnfrepo.Group) dnfrepo.Group {
	cache.mu.Lock()
	if !cache.wrapsReady || !projectionSameRepositories(cache.wrapsInner, repositories) {
		cache.wrapsInner = repositories
		cache.wraps = newProjectionReadWrappers(repositories, cache)
		cache.wrapsReady = true
		cache.entries = make(map[projectionReadCacheKey]any)
		cache.written = make(map[projectionReadCacheKey]struct{})
	}
	wraps := cache.wraps
	cache.mu.Unlock()
	return wraps.apply(repositories)
}

// projectionReadWrappers 是一次批量里装箱好的包装仓储。
type projectionReadWrappers struct {
	account         dnfrepo.AccountRepository
	character       dnfrepo.CharacterRepository
	equipment       dnfrepo.EquipmentRepository
	inventory       dnfrepo.InventoryRepository
	pet             dnfrepo.PetRepository
	settings        dnfrepo.SettingsRepository
	skill           dnfrepo.SkillRepository
	characterCreate dnfrepo.CharacterCreationUnitOfWork
	characterSkills dnfrepo.CharacterSkillUnitOfWork
}

// newProjectionReadWrappers 是纯构建：projectionWrapStores 是唯一的生产/失效包装
// 构建路径，这里只把它产出的 7 个仓储字段取出来，再加上两个工作单元包装。
func newProjectionReadWrappers(repositories dnfrepo.Group, cache *projectionReadCache) projectionReadWrappers {
	wrapped := projectionWrapStores(repositories, cache, true)
	wraps := projectionReadWrappers{
		account:   wrapped.Account,
		character: wrapped.Character,
		equipment: wrapped.Equipment,
		inventory: wrapped.Inventory,
		pet:       wrapped.Pet,
		settings:  wrapped.Settings,
		skill:     wrapped.Skill,
	}
	if repositories.CharacterCreate != nil {
		wraps.characterCreate = projectionMemoCharacterCreateUnitOfWork{
			CharacterCreationUnitOfWork: repositories.CharacterCreate,
			cache:                       cache,
		}
	}
	if repositories.CharacterSkills != nil {
		wraps.characterSkills = projectionMemoCharacterSkillsUnitOfWork{
			CharacterSkillUnitOfWork: repositories.CharacterSkills,
			cache:                    cache,
		}
	}
	return wraps
}

func (wraps projectionReadWrappers) apply(repositories dnfrepo.Group) dnfrepo.Group {
	if wraps.account != nil {
		repositories.Account = wraps.account
	}
	if wraps.character != nil {
		repositories.Character = wraps.character
	}
	if wraps.equipment != nil {
		repositories.Equipment = wraps.equipment
	}
	if wraps.inventory != nil {
		repositories.Inventory = wraps.inventory
	}
	if wraps.pet != nil {
		repositories.Pet = wraps.pet
	}
	if wraps.settings != nil {
		repositories.Settings = wraps.settings
	}
	if wraps.skill != nil {
		repositories.Skill = wraps.skill
	}
	if wraps.characterCreate != nil {
		repositories.CharacterCreate = wraps.characterCreate
	}
	if wraps.characterSkills != nil {
		repositories.CharacterSkills = wraps.characterSkills
	}
	return repositories
}

// projectionSameRepositories 判断两个仓储组里被包装的那几个字段是不是同一批
// 底层仓储。相等就复用已经装箱的包装，不等就重建。
//
// 直接比较接口值在本仓库是可行的：这里出现的实现要么是指针，要么是只含可比较
// 字段的小结构体（例如压测夹具里的计数装饰器）。真正不可比较的动态类型（比如
// 内含 map/slice 的值类型）会让接口比较在运行期 panic，所以整组比较包了一层
// recover：一旦比较不了就按「不同」处理，代价只是重新装箱一次，绝不会读到别的
// 仓储。
func projectionSameRepositories(left dnfrepo.Group, right dnfrepo.Group) bool {
	same, comparable := projectionCompareRepositories(func() bool {
		return left.Account == right.Account &&
			left.Character == right.Character &&
			left.Equipment == right.Equipment &&
			left.Inventory == right.Inventory &&
			left.Pet == right.Pet &&
			left.Settings == right.Settings &&
			left.Skill == right.Skill &&
			left.CharacterCreate == right.CharacterCreate &&
			left.CharacterSkills == right.CharacterSkills
	})
	return comparable && same
}

func projectionCompareRepositories(compare func() bool) (same bool, comparable bool) {
	defer func() {
		if recover() != nil {
			same, comparable = false, false
		}
	}()
	return compare(), true
}

// projectionInvalidatingRepositories 是同一个包装的「只失效、不缓存」形态：
// 事务内的读必须走事务自己的视图（未提交的写入要看得见，回滚也不能把值留在
// 缓存里），但事务里的写入同样要让外层缓存失效。
func projectionInvalidatingRepositories(inner dnfrepo.Group, cache *projectionReadCache) dnfrepo.Group {
	if cache == nil {
		return inner
	}
	return projectionWrapStores(inner, cache, false)
}

// projectionWrapStores 包住投影路径读到的 7 个仓储字段。memoizeReads 为 false 时
// 读直通底层（事务视图），写仍然先失效再转发。
func projectionWrapStores(inner dnfrepo.Group, cache *projectionReadCache, memoizeReads bool) dnfrepo.Group {
	out := inner
	if inner.Account != nil {
		out.Account = projectionMemoAccountStore{AccountRepository: inner.Account, cache: cache, memoizeReads: memoizeReads}
	}
	if inner.Character != nil {
		out.Character = projectionMemoCharacterStore{CharacterRepository: inner.Character, cache: cache, memoizeReads: memoizeReads}
	}
	if inner.Equipment != nil {
		out.Equipment = newProjectionMemoStore[dnfrepo.EquipmentRecord, dnfrepo.EquipmentField](
			inner.Equipment, cache, projectionCacheRepoEquipment, dnfrepo.CloneEquipment, dnfrepo.EquipmentKey, memoizeReads)
	}
	if inner.Inventory != nil {
		out.Inventory = newProjectionMemoStore[dnfrepo.InventoryRecord, dnfrepo.InventoryField](
			inner.Inventory, cache, projectionCacheRepoInventory, dnfrepo.CloneInventory, dnfrepo.InventoryKey, memoizeReads)
	}
	if inner.Pet != nil {
		out.Pet = newProjectionMemoStore[dnfrepo.PetRecord, dnfrepo.PetField](
			inner.Pet, cache, projectionCacheRepoPet, dnfrepo.ClonePet, dnfrepo.PetKey, memoizeReads)
	}
	if inner.Settings != nil {
		out.Settings = newProjectionMemoStore[dnfrepo.SettingsRecord, dnfrepo.SettingsField](
			inner.Settings, cache, projectionCacheRepoSettings, dnfrepo.CloneSettings, dnfrepo.SettingsKey, memoizeReads)
	}
	if inner.Skill != nil {
		out.Skill = newProjectionMemoStore[dnfrepo.SkillRecord, dnfrepo.SkillField](
			inner.Skill, cache, projectionCacheRepoSkill, dnfrepo.CloneSkill, dnfrepo.SkillKey, memoizeReads)
	}
	return out
}

// ---------------------------------------------------------------------------
// 通用仓储包装
// ---------------------------------------------------------------------------

// projectionMemoStore 用只读缓存包住一个仓储 Store。
//
// Load 命中时返回深拷贝；命中「不存在」时返回零值 + false，与一次真实未命中
// 完全一致。Save / SaveFields / SaveFieldBatch 先让缓存失效再原样转发，因此写
// 路径的行为与未包装时一致（含 FieldStore / BatchFieldStore 的能力探测与退化
// 分支，与 platform/db 的实现一一对应）。
//
// memoizeReads 为 false 时读完全直通底层，只保留写入失效：事务内的仓储必须看到
// 自己未提交的写入，回滚时也不能把未提交的值留在缓存里。
type projectionMemoStore[T any, F comparable] struct {
	inner        dnfrepo.Store[T]
	fieldStore   dnfrepo.FieldStore[T, F]
	batchStore   dnfrepo.BatchFieldStore[T, F]
	cache        *projectionReadCache
	repository   string
	clone        func(T) T
	key          func(T) string
	memoizeReads bool
}

func newProjectionMemoStore[T any, F comparable](
	inner dnfrepo.Store[T],
	cache *projectionReadCache,
	repository string,
	clone func(T) T,
	key func(T) string,
	memoizeReads bool,
) projectionMemoStore[T, F] {
	store := projectionMemoStore[T, F]{
		inner:        inner,
		cache:        cache,
		repository:   repository,
		clone:        clone,
		key:          key,
		memoizeReads: memoizeReads,
	}
	if fieldStore, ok := inner.(dnfrepo.FieldStore[T, F]); ok {
		store.fieldStore = fieldStore
	}
	if batchStore, ok := inner.(dnfrepo.BatchFieldStore[T, F]); ok {
		store.batchStore = batchStore
	}
	return store
}

func (store projectionMemoStore[T, F]) Load(ctx context.Context, key string) (T, bool, error) {
	var zero T
	if store.memoizeReads {
		if value, ok := store.cache.lookup(store.repository, key); ok {
			if _, miss := value.(projectionCacheMiss); miss {
				return zero, false, nil
			}
			if record, typed := value.(T); typed {
				return store.clone(record), true, nil
			}
		}
	}
	record, found, err := store.inner.Load(ctx, key)
	if err != nil {
		// 读失败不入缓存：失败可能是瞬时的，下一次读应当重试。
		return record, found, err
	}
	if store.memoizeReads {
		if found {
			store.cache.store(store.repository, key, store.clone(record))
		} else {
			store.cache.store(store.repository, key, projectionCacheMiss{})
		}
	}
	return record, found, nil
}

func (store projectionMemoStore[T, F]) Save(ctx context.Context, record T) error {
	store.cache.forget(store.repository, store.recordKey(record))
	return store.inner.Save(ctx, record)
}

func (store projectionMemoStore[T, F]) SaveFields(ctx context.Context, record T, fields ...F) error {
	store.cache.forget(store.repository, store.recordKey(record))
	if store.fieldStore != nil {
		return store.fieldStore.SaveFields(ctx, record, fields...)
	}
	return store.inner.Save(ctx, record)
}

func (store projectionMemoStore[T, F]) SaveFieldBatch(ctx context.Context, saves []dnfrepo.FieldSave[T, F]) error {
	for _, save := range saves {
		store.cache.forget(store.repository, store.recordKey(save.Record))
	}
	if store.batchStore != nil {
		return store.batchStore.SaveFieldBatch(ctx, saves)
	}
	for _, save := range saves {
		if err := dnfrepo.SaveFields(ctx, store.inner, save.Record, nil, save.Fields...); err != nil {
			return err
		}
	}
	return nil
}

func (store projectionMemoStore[T, F]) Check(ctx context.Context) error {
	if checker, ok := store.inner.(dnfrepo.Checker); ok {
		return checker.Check(ctx)
	}
	return nil
}

func (store projectionMemoStore[T, F]) Expire(ctx context.Context, key string, ttl time.Duration) error {
	store.cache.forget(store.repository, key)
	if expirer, ok := store.inner.(dnfrepo.Expirer); ok {
		return expirer.Expire(ctx, key, ttl)
	}
	return nil
}

func (store projectionMemoStore[T, F]) Close(ctx context.Context) error {
	if closer, ok := store.inner.(interface {
		Close(context.Context) error
	}); ok {
		return closer.Close(ctx)
	}
	if flusher, ok := store.inner.(interface {
		Flush(context.Context) error
	}); ok {
		return flusher.Flush(ctx)
	}
	return nil
}

func (store projectionMemoStore[T, F]) recordKey(record T) string {
	if store.key == nil {
		return ""
	}
	return store.key(record)
}

// ---------------------------------------------------------------------------
// 角色仓储包装
// ---------------------------------------------------------------------------

// projectionMemoCharacterStore 包住 CharacterRepository。Load 走缓存；接口自带的
// ListByAccount / FindIDByName / NextNumericID 由内嵌接口原样转发（它们不是按
// 角色主键的单条读，本批次不做缓存，行为保持不变）。可选接口按底层能力转发。
type projectionMemoCharacterStore struct {
	dnfrepo.CharacterRepository
	cache        *projectionReadCache
	memoizeReads bool
}

func (store projectionMemoCharacterStore) Load(ctx context.Context, key string) (dnfrepo.CharacterRecord, bool, error) {
	if store.memoizeReads {
		if value, ok := store.cache.lookup(projectionCacheRepoCharacter, key); ok {
			if _, miss := value.(projectionCacheMiss); miss {
				return dnfrepo.CharacterRecord{}, false, nil
			}
			if record, typed := value.(dnfrepo.CharacterRecord); typed {
				return dnfrepo.CloneCharacter(record), true, nil
			}
		}
	}
	record, found, err := store.CharacterRepository.Load(ctx, key)
	if err != nil {
		return record, found, err
	}
	if store.memoizeReads {
		if found {
			store.cache.store(projectionCacheRepoCharacter, key, dnfrepo.CloneCharacter(record))
		} else {
			store.cache.store(projectionCacheRepoCharacter, key, projectionCacheMiss{})
		}
	}
	return record, found, nil
}

func (store projectionMemoCharacterStore) Save(ctx context.Context, record dnfrepo.CharacterRecord) error {
	store.cache.forget(projectionCacheRepoCharacter, dnfrepo.CharacterKey(record))
	return store.CharacterRepository.Save(ctx, record)
}

func (store projectionMemoCharacterStore) SaveFields(ctx context.Context, record dnfrepo.CharacterRecord, fields ...dnfrepo.CharacterField) error {
	store.cache.forget(projectionCacheRepoCharacter, dnfrepo.CharacterKey(record))
	if fieldStore, ok := store.CharacterRepository.(dnfrepo.FieldStore[dnfrepo.CharacterRecord, dnfrepo.CharacterField]); ok {
		return fieldStore.SaveFields(ctx, record, fields...)
	}
	return store.CharacterRepository.Save(ctx, record)
}

func (store projectionMemoCharacterStore) SaveFieldBatch(ctx context.Context, saves []dnfrepo.FieldSave[dnfrepo.CharacterRecord, dnfrepo.CharacterField]) error {
	for _, save := range saves {
		store.cache.forget(projectionCacheRepoCharacter, dnfrepo.CharacterKey(save.Record))
	}
	if batchStore, ok := store.CharacterRepository.(dnfrepo.BatchFieldStore[dnfrepo.CharacterRecord, dnfrepo.CharacterField]); ok {
		return batchStore.SaveFieldBatch(ctx, saves)
	}
	for _, save := range saves {
		if err := dnfrepo.SaveFields(ctx, store.CharacterRepository, save.Record, nil, save.Fields...); err != nil {
			return err
		}
	}
	return nil
}

func (store projectionMemoCharacterStore) CreateCharacter(ctx context.Context, record dnfrepo.CharacterRecord) error {
	store.cache.forget(projectionCacheRepoCharacter, dnfrepo.CharacterKey(record))
	creator, ok := store.CharacterRepository.(dnfrepo.CharacterCreator)
	if !ok {
		return dnfrepo.ErrCharacterCreateMissing
	}
	return creator.CreateCharacter(ctx, record)
}

func (store projectionMemoCharacterStore) SwapCharacterSlots(ctx context.Context, accountID string, slotA int, slotB int) error {
	swapper, ok := store.CharacterRepository.(dnfrepo.CharacterSlotSwapper)
	if !ok {
		return dnfrepo.ErrCharacterSlotSwapUnavailable
	}
	// 槽位交换会改动同账号的其它角色行；只知道账号时整仓失效。
	store.cache.forgetRepository(projectionCacheRepoCharacter)
	return swapper.SwapCharacterSlots(ctx, accountID, slotA, slotB)
}

func (store projectionMemoCharacterStore) DeleteCharacterAndCompact(ctx context.Context, accountID string, slot int, name string) error {
	deleter, ok := store.CharacterRepository.(dnfrepo.CharacterRosterDeleter)
	if !ok {
		return dnfrepo.ErrCharacterDeleteUnavailable
	}
	store.cache.forgetRepository(projectionCacheRepoCharacter)
	return deleter.DeleteCharacterAndCompact(ctx, accountID, slot, name)
}

func (store projectionMemoCharacterStore) AdvanceStoryDigest(ctx context.Context, characterID string, level uint32, migrationVersion uint32) error {
	advancer, ok := store.CharacterRepository.(dnfrepo.CharacterStoryDigestAdvancer)
	if !ok {
		return dnfrepo.ErrCharacterStoryDigestAdvanceUnavailable
	}
	store.cache.forget(projectionCacheRepoCharacter, characterID)
	return advancer.AdvanceStoryDigest(ctx, characterID, level, migrationVersion)
}

func (store projectionMemoCharacterStore) Check(ctx context.Context) error {
	if checker, ok := store.CharacterRepository.(dnfrepo.Checker); ok {
		return checker.Check(ctx)
	}
	return nil
}

func (store projectionMemoCharacterStore) Expire(ctx context.Context, key string, ttl time.Duration) error {
	store.cache.forget(projectionCacheRepoCharacter, key)
	if expirer, ok := store.CharacterRepository.(dnfrepo.Expirer); ok {
		return expirer.Expire(ctx, key, ttl)
	}
	return nil
}

func (store projectionMemoCharacterStore) Close(ctx context.Context) error {
	if closer, ok := store.CharacterRepository.(interface {
		Close(context.Context) error
	}); ok {
		return closer.Close(ctx)
	}
	if flusher, ok := store.CharacterRepository.(interface {
		Flush(context.Context) error
	}); ok {
		return flusher.Flush(ctx)
	}
	return nil
}

// ---------------------------------------------------------------------------
// 账号仓储包装
// ---------------------------------------------------------------------------

// projectionMemoAccountStore 包住 AccountRepository。AccountRecord 没有
// FieldStore 语义（仓储包未定义 AccountField），因此这里不实现 SaveFields，
// 与未包装时一致；仅在键级元数据写入可用时保留该可选接口。
type projectionMemoAccountStore struct {
	dnfrepo.AccountRepository
	cache        *projectionReadCache
	memoizeReads bool
}

func (store projectionMemoAccountStore) Load(ctx context.Context, key string) (dnfrepo.AccountRecord, bool, error) {
	if store.memoizeReads {
		if value, ok := store.cache.lookup(projectionCacheRepoAccount, key); ok {
			if _, miss := value.(projectionCacheMiss); miss {
				return dnfrepo.AccountRecord{}, false, nil
			}
			if record, typed := value.(dnfrepo.AccountRecord); typed {
				return dnfrepo.CloneAccount(record), true, nil
			}
		}
	}
	record, found, err := store.AccountRepository.Load(ctx, key)
	if err != nil {
		return record, found, err
	}
	if store.memoizeReads {
		if found {
			store.cache.store(projectionCacheRepoAccount, key, dnfrepo.CloneAccount(record))
		} else {
			store.cache.store(projectionCacheRepoAccount, key, projectionCacheMiss{})
		}
	}
	return record, found, nil
}

func (store projectionMemoAccountStore) Save(ctx context.Context, record dnfrepo.AccountRecord) error {
	store.cache.forget(projectionCacheRepoAccount, dnfrepo.AccountKey(record))
	return store.AccountRepository.Save(ctx, record)
}

func (store projectionMemoAccountStore) SaveMetadataEntry(ctx context.Context, accountID string, key string, value string, updatedAt time.Time) error {
	store.cache.forget(projectionCacheRepoAccount, accountID)
	writer, ok := store.AccountRepository.(dnfrepo.AccountMetadataEntryWriter)
	if !ok {
		return dnfrepo.ErrAccountMetadataUnavailable
	}
	return writer.SaveMetadataEntry(ctx, accountID, key, value, updatedAt)
}

func (store projectionMemoAccountStore) ListAccounts(ctx context.Context, query string, limit int) ([]dnfrepo.AccountRecord, error) {
	lister, ok := store.AccountRepository.(dnfrepo.AccountLister)
	if !ok {
		return nil, dnfrepo.ErrAccountListUnavailable
	}
	return lister.ListAccounts(ctx, query, limit)
}

func (store projectionMemoAccountStore) Check(ctx context.Context) error {
	if checker, ok := store.AccountRepository.(dnfrepo.Checker); ok {
		return checker.Check(ctx)
	}
	return nil
}

func (store projectionMemoAccountStore) Expire(ctx context.Context, key string, ttl time.Duration) error {
	store.cache.forget(projectionCacheRepoAccount, key)
	if expirer, ok := store.AccountRepository.(dnfrepo.Expirer); ok {
		return expirer.Expire(ctx, key, ttl)
	}
	return nil
}

func (store projectionMemoAccountStore) Close(ctx context.Context) error {
	if closer, ok := store.AccountRepository.(interface {
		Close(context.Context) error
	}); ok {
		return closer.Close(ctx)
	}
	if flusher, ok := store.AccountRepository.(interface {
		Flush(context.Context) error
	}); ok {
		return flusher.Flush(ctx)
	}
	return nil
}

// ---------------------------------------------------------------------------
// 角色创建工作单元包装
// ---------------------------------------------------------------------------

// projectionMemoCharacterCreateUnitOfWork 让角色初始化回填事务里的写入也接到
// 缓存失效上。
//
// characterdata.Initializer.Ensure 通过 CharacterCreationUnitOfWork 提交缺记录的
// 初始化（背包/装备/技能/三份设置）。事务自己按 base 建一份事务视图，写入落在
// 那个视图上，因此必须把交给回调的 tx 组再包一层失效包装，否则本批次前面缓存
// 的「不存在」会在回填之后继续被后面的配对读到。
type projectionMemoCharacterCreateUnitOfWork struct {
	dnfrepo.CharacterCreationUnitOfWork
	cache *projectionReadCache
}

func (unit projectionMemoCharacterCreateUnitOfWork) WithinCharacterCreation(
	ctx context.Context,
	characterID string,
	base dnfrepo.Group,
	apply func(dnfrepo.Group) error,
) error {
	return unit.CharacterCreationUnitOfWork.WithinCharacterCreation(ctx, characterID, base, func(tx dnfrepo.Group) error {
		// 读直通事务视图：事务要看得见自己未提交的写入，回滚也不能把未提交的
		// 值留在缓存里。
		return apply(projectionInvalidatingRepositories(tx, unit.cache))
	})
}

// ---------------------------------------------------------------------------
// 技能工作单元包装
// ---------------------------------------------------------------------------

// projectionMemoCharacterSkillsUnitOfWork 让技能修复事务里的写入也接到缓存失效上。
//
// dnfskill.SceneOwner 的 Backfill / SyncPoints / EnsureTree / EnsureBaseline /
// EnsureLayout 会修改**已经存在**的技能记录，而且只通过 CharacterSkills 工作单元
// 拿到事务内的 SkillRepository（不经过投影路径手里的仓储组）。如果不管它，本批次
// 前面缓存的技能记录会在修复之后继续被后面的配对读到。这里把事务内的 SkillRepository
// 再包一层：读原样转发（事务自己有视图，不参与缓存），写先失效再转发。
type projectionMemoCharacterSkillsUnitOfWork struct {
	dnfrepo.CharacterSkillUnitOfWork
	cache *projectionReadCache
}

func (unit projectionMemoCharacterSkillsUnitOfWork) WithinCharacterSkill(
	ctx context.Context,
	characterID string,
	apply func(dnfrepo.SkillRepository) error,
) error {
	return unit.CharacterSkillUnitOfWork.WithinCharacterSkill(ctx, characterID, func(skills dnfrepo.SkillRepository) error {
		if skills == nil {
			return apply(skills)
		}
		return apply(projectionMemoSkillTransactionStore{SkillRepository: skills, cache: unit.cache})
	})
}

// projectionMemoSkillTransactionStore 只做写入失效；Load 直接走事务仓储，
// 因此事务一定能看到自己未提交的写入，回滚也不会把未提交数据留在缓存里。
type projectionMemoSkillTransactionStore struct {
	dnfrepo.SkillRepository
	cache *projectionReadCache
}

func (store projectionMemoSkillTransactionStore) Save(ctx context.Context, record dnfrepo.SkillRecord) error {
	store.cache.forget(projectionCacheRepoSkill, dnfrepo.SkillKey(record))
	return store.SkillRepository.Save(ctx, record)
}

func (store projectionMemoSkillTransactionStore) SaveFields(ctx context.Context, record dnfrepo.SkillRecord, fields ...dnfrepo.SkillField) error {
	store.cache.forget(projectionCacheRepoSkill, dnfrepo.SkillKey(record))
	if fieldStore, ok := store.SkillRepository.(dnfrepo.FieldStore[dnfrepo.SkillRecord, dnfrepo.SkillField]); ok {
		return fieldStore.SaveFields(ctx, record, fields...)
	}
	return store.SkillRepository.Save(ctx, record)
}

func (store projectionMemoSkillTransactionStore) SaveFieldBatch(ctx context.Context, saves []dnfrepo.FieldSave[dnfrepo.SkillRecord, dnfrepo.SkillField]) error {
	for _, save := range saves {
		store.cache.forget(projectionCacheRepoSkill, dnfrepo.SkillKey(save.Record))
	}
	if batchStore, ok := store.SkillRepository.(dnfrepo.BatchFieldStore[dnfrepo.SkillRecord, dnfrepo.SkillField]); ok {
		return batchStore.SaveFieldBatch(ctx, saves)
	}
	for _, save := range saves {
		if err := dnfrepo.SaveFields(ctx, store.SkillRepository, save.Record, nil, save.Fields...); err != nil {
			return err
		}
	}
	return nil
}
