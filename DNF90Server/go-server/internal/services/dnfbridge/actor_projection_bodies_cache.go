package dnfbridge

import (
	"context"
	"sync"
	"time"
)

// 本文件为 syncRuntimeRaidMateActorsAllAtSceneBoundary 的 M×(M−1) 双循环提供
// 「按演员复用投影 body」的缓存。
//
// 背景（2026-09-25 测试服实测，8 人攻坚）：
//
//	一次 loaded 边界产生 56 个有序对，每个对都要建两份 body
//	（mode0 场景对象表 + mode1 角色绑定），每份都带一次 PVF 属性重算。
//	一次边界 6.0 秒，全部串行。
//
// 但两份 body **只由演员决定，除一个字节外**：
//
//	character_scene_object.go:85-90  writeByte(mode0) writeUint16(1)
//	                                 writeByte(currentSceneObjectRoute) writeByte(ownerChannel)
//	userinfo_current_modes.go:32-35  writeByte(mode1) writeUint16(1)
//	                                 writeByte(currentSceneObjectRoute) writeByte(ownerChannel)
//
// 即两份 body 的**字节 4** 就是 ownerChannel，其余全部来自演员。
// 而 ownerChannel = currentTownRemoteActorOwnerContext(target, _) 显式忽略
// charID（online_player_manager.go:978-983，第二个参数就是 `_ uint16`），
// 只取决于**接收者**。
//
// ⇒ 缓存键里**不能**含 ownerChannel：那会让每个接收者各建一遍、命中率为零。
//   第一版就是这么写的，实测一次边界只从 6.0s 降到 4.5s（那点收益全部来自
//   mode1 那处 includeEquipment 修复）。正确做法是按演员缓存一份「字节 4 归零」
//   的 body，发给每个接收者前把字节 4 补成该接收者的 owner。
//
// 这样 56 次构建退化成约 M 次，且**最终字节与改动前逐字相同**。
//
// 缓存生命周期 = 一次扇出（挂在 ctx 上），与 projectionReadCache 同一约定：
// 函数返回即失效，绝不跨请求留存演员状态。

// actorProjectionOwnerChannelOffset 是两份 body 里 ownerChannel 的字节偏移，
// 由上面两个 writer 的逐行核实得出。
const actorProjectionOwnerChannelOffset = 4

// actorProjectionBodies 是一个键对应的两份 body（字节 4 已归零）。
type actorProjectionBodies struct {
	mode0 []byte
	mode1 []byte
}

// actorProjectionBodiesKey 是缓存键：只含**演员维度**的输入。
//
// charID  —— 演员角色 ID（body 的决定性输入）
// binding —— withBinding（决定 mode1 是否带 ~31 行装备行，见
//
//	sendTownRemoteActorProjectionWithContext）
type actorProjectionBodiesKey struct {
	charID      uint16
	withBinding bool
}

type actorProjectionBodiesCache struct {
	entries map[actorProjectionBodiesKey]actorProjectionBodies

	// 诊断计数：回答「键为什么没复用」。
	//
	// 按 withBinding 分开统计，因为 withBinding 由**接收者**场景决定
	// （runtimeRaidPeerActorInstallModeFor(state, target)）。如果它随接收者变化，
	// 同一个演员就会被拆成多个键、命中率暴跌 —— 这正是要证伪的假设。
	mu            sync.Mutex
	hitNoBinding  int
	hitBinding    int
	missNoBinding int
	missBinding   int
	buildNanos    int64
	sendNanos     int64
}

func (cache *actorProjectionBodiesCache) noteHit(withBinding bool) {
	cache.mu.Lock()
	if withBinding {
		cache.hitBinding++
	} else {
		cache.hitNoBinding++
	}
	cache.mu.Unlock()
}

func (cache *actorProjectionBodiesCache) noteMiss(withBinding bool) {
	cache.mu.Lock()
	if withBinding {
		cache.missBinding++
	} else {
		cache.missNoBinding++
	}
	cache.mu.Unlock()
}

// logSummary 在一次扇出结束时打一行计数，用于判定缓存是否真的复用上了。
func (cache *actorProjectionBodiesCache) logSummary(s *Service, source string) {
	if cache == nil || s == nil {
		return
	}
	cache.mu.Lock()
	hitNo, hitYes := cache.hitNoBinding, cache.hitBinding
	missNo, missYes := cache.missNoBinding, cache.missBinding
	entries := len(cache.entries)
	buildNanos, sendNanos := cache.buildNanos, cache.sendNanos
	cache.mu.Unlock()
	s.logPacketEvent("game-actor-body-cache-summary",
		"source", source,
		"hit_nobind", hitNo,
		"hit_bind", hitYes,
		"miss_nobind", missNo,
		"miss_bind", missYes,
		"distinct_keys", entries,
		"build_ms", buildNanos/1e6,
		"send_ms", sendNanos/1e6)
}

func newActorProjectionBodiesCache() *actorProjectionBodiesCache {
	return &actorProjectionBodiesCache{
		entries: make(map[actorProjectionBodiesKey]actorProjectionBodies),
	}
}

type actorProjectionBodiesCtxKey struct{}

// withActorProjectionBodiesCache 为一次扇出挂上 body 缓存。
func withActorProjectionBodiesCache(ctx context.Context) context.Context {
	if ctx == nil {
		ctx = context.Background()
	}
	return context.WithValue(ctx, actorProjectionBodiesCtxKey{}, newActorProjectionBodiesCache())
}

// actorProjectionBodiesCacheFromContext 取当前扇出的缓存；没有则返回 nil，
// 此时 lookup 一律未命中、store 是空操作 —— 也就是**改动前的原始行为**。
func actorProjectionBodiesCacheFromContext(ctx context.Context) *actorProjectionBodiesCache {
	if ctx == nil {
		return nil
	}
	cache, _ := ctx.Value(actorProjectionBodiesCtxKey{}).(*actorProjectionBodiesCache)
	return cache
}

// lookup 命中时返回**缓存内容的副本**。
//
// 副本是刻意的：调用方会把切片交给发包路径，缓存自己必须留一份不受影响的
// 权威拷贝，否则任何下游对切片的写入都会污染后续所有接收者。一次约 500 字节
// 的 memcpy 相比重新构建（含 PVF 重算与仓储读）可以忽略。
func (cache *actorProjectionBodiesCache) lookup(
	key actorProjectionBodiesKey,
) ([]byte, []byte, bool) {
	if cache == nil {
		return nil, nil, false
	}
	cache.mu.Lock()
	entry, ok := cache.entries[key]
	cache.mu.Unlock()
	if !ok {
		cache.noteMiss(key.withBinding)
		return nil, nil, false
	}
	cache.noteHit(key.withBinding)
	return append([]byte(nil), entry.mode0...), append([]byte(nil), entry.mode1...), true
}

// store 保存两份 body 的副本，并把 ownerChannel 字节归零。
//
// 归零是刻意的：缓存代表**演员维度**的内容，接收者维度由
// applyActorProjectionOwnerChannel 在取出后补上。
func (cache *actorProjectionBodiesCache) store(
	key actorProjectionBodiesKey, mode0, mode1 []byte,
) {
	if cache == nil {
		return
	}
	cache.mu.Lock()
	cache.entries[key] = actorProjectionBodies{
		mode0: zeroActorProjectionOwnerChannel(mode0),
		mode1: zeroActorProjectionOwnerChannel(mode1),
	}
	cache.mu.Unlock()
}

// noteBuild / noteSend 累加「建 body」与「发包」各自花掉的时间。
//
// 这是为了回答「4.3 秒到底在构建还是在投递」。缓存已经把构建从 56 次降到 8 次
// （hit 48 / miss 8），如果 noteSend 仍占绝大部分，那剩下的就是同步写进每个
// 接收者 writer 的等待（gameWriterSyncBudget=250ms），与构建无关。
func (cache *actorProjectionBodiesCache) noteBuild(d time.Duration) {
	if cache == nil || d <= 0 {
		return
	}
	cache.mu.Lock()
	cache.buildNanos += d.Nanoseconds()
	cache.mu.Unlock()
}

func (cache *actorProjectionBodiesCache) noteSend(d time.Duration) {
	if cache == nil || d <= 0 {
		return
	}
	cache.mu.Lock()
	cache.sendNanos += d.Nanoseconds()
	cache.mu.Unlock()
}

func zeroActorProjectionOwnerChannel(body []byte) []byte {
	stored := append([]byte(nil), body...)
	if len(stored) > actorProjectionOwnerChannelOffset {
		stored[actorProjectionOwnerChannelOffset] = 0
	}
	return stored
}

// applyActorProjectionOwnerChannel 把 body 的 ownerChannel 字节补成接收者的值。
//
// 该字节是两份 body 里**唯一**与接收者相关的值（已逐行核实两个 writer）。
// body 短于该偏移时不改，与改动前保持一致。
func applyActorProjectionOwnerChannel(body []byte, ownerChannel byte) {
	if len(body) > actorProjectionOwnerChannelOffset {
		body[actorProjectionOwnerChannelOffset] = ownerChannel
	}
}
