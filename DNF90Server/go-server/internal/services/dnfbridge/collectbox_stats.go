package dnfbridge

import (
	"context"
	"time"

	dnfcharstat "longheng.io/server/internal/modules/dnf/charstat"
	dnfcollectbox "longheng.io/server/internal/modules/dnf/collectbox"
)

func (s *Service) currentCollectboxBonus(ctx context.Context, session *gameSession) (dnfcollectbox.Bonus, error) {
	if s == nil || session == nil || session.selectedCharacterID == 0 {
		return dnfcollectbox.Bonus{}, nil
	}
	catalog, err := s.currentCollectboxCatalog()
	if err != nil {
		return dnfcollectbox.Bonus{}, err
	}
	// 走 projectionRepositories(ctx) 而不是 repositoryGroup()：前者会在一次扇出
	// 内把同一个角色的仓储读只做一遍（projection_read_cache.go:176-186），后者
	// 每次都是真读。本函数由 characterPVFStatsForUserInfo 调用，而后者在
	// syncRuntimeRaidMateActorsAllAtSceneBoundary 的 M×(M−1) 双循环里**每个有序对
	// 都会跑一遍** —— 实测一次边界 112 次属性重算，也就是 112 次未缓存的
	// loadCollectboxState。ctx 上没有缓存时它退化成与 repositoryGroup() 完全一致，
	// 所以这是一处纯读缓存，不改变任何字节与顺序。
	repositories, ok := s.projectionRepositories(ctx)
	if !ok {
		return dnfcollectbox.Bonus{}, nil
	}
	state, err := loadCollectboxState(ctx, repositories, s.accountIDForSession(session), uint64(session.selectedCharacterID))
	if err != nil {
		return dnfcollectbox.Bonus{}, err
	}
	var result dnfcollectbox.Bonus
	for _, index := range catalog.indexes {
		definition, found := collectboxDefinition(catalog, index)
		if found {
			result.Add(dnfcollectbox.SnapshotBonus(definition, state, time.Now().UTC()))
		}
	}
	return result, nil
}

func applyCurrentCollectboxBonus(stats dnfcharstat.Vector, bonus dnfcollectbox.Bonus) dnfcharstat.Vector {
	stats.PhysicalAttack += bonus.PhysicalAttack
	stats.PhysicalDefense += bonus.PhysicalDefense
	stats.MagicalAttack += bonus.MagicalAttack
	stats.MagicalDefense += bonus.MagicalDefense
	stats.IndependentAttack += bonus.IndependentAttack
	return stats
}
