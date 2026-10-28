package dnfbridge

import (
	"fmt"
	"time"

	dnfguild "longheng.io/server/internal/modules/dnf/guild"
	dnfrepo "longheng.io/server/internal/modules/dnf/repository"
	"longheng.io/server/internal/modules/dnf/worldmap"
)

// The durable battle key owns the timer. Refresh/reconnect reconstructs its
// remaining delay, and a callback from an older battle never changes a new one.
func (s *Service) scheduleCurrentGuildWar(g *dnfguild.Guild) error {
	if g == nil || g.War.Battle == nil {
		return nil
	}
	b := *g.War.Battle
	id := g.ID
	queue := s.ensureGameplayTimeQueue()
	return queue.ScheduleAfter(fmt.Sprintf("guild-war:%d:%s", id, b.Key), max(time.Millisecond, time.Unix(b.EndsAt, 0).Sub(queue.Now())), func(now time.Time) {
		ctx, cancel := s.currentGuildContext()
		defer cancel()
		var ended *dnfguild.Guild
		err := s.mutateCurrentGuild(ctx, func(_ dnfrepo.Group, r *dnfguild.Registry) error {
			g := r.Guilds[id]
			if g == nil || g.War.Battle == nil || g.War.Battle.Key != b.Key || now.Unix() < g.War.Battle.EndsAt {
				return nil
			}
			r.AdvanceWars(now)
			ended = g
			return nil
		})
		if err != nil {
			s.logPacketEvent("game-guild-war-expiry-failed", "guild_id", id, "error", err)
			// Preserve the same durable generation while retrying a failed save.
			retry := *g
			copyBattle := b
			copyBattle.EndsAt = now.Add(time.Second).Unix()
			retry.War.Battle = &copyBattle
			if e := s.scheduleCurrentGuildWar(&retry); e != nil {
				s.logPacketEvent("game-guild-war-expiry-retry-failed", "error", e)
			}
			return
		}
		if ended != nil {
			if err = s.publishCurrentGuildWarEnd(ended, b.Key); err != nil {
				s.logPacketEvent("game-guild-war-expiry-publish-failed", "guild_id", id, "error", err)
			}
		}
	})
}

func (s *Service) publishCurrentGuildWarEnd(g *dnfguild.Guild, key string) error {
	for id := range g.Members {
		identity, online := s.onlineGameSessionCharacterSnapshot(uint16(currentGuildNumericID(id)))
		if !online || identity.session == nil {
			continue
		}
		ss := identity.session
		if err := s.postGameSessionCharacterEvent(ss, "guild-war-ended", identity.character, identity.generation, func() error {
			ss.dungeon.mu.Lock()
			runtime := ss.dungeon.runtime
			belongs := runtime != nil && runtime.guildWarID == g.ID && runtime.guildWarKey == key && runtime.Session != nil
			active := belongs && runtime.Session.Snapshot().Run.Status == worldmap.DungeonRunActive
			ss.dungeon.mu.Unlock()
			if active {
				ctx, cancel := s.currentGuildContext()
				defer cancel()
				transition, err := s.prepareCurrentDungeonTownTransitionForSession(ctx, ss, identity.character)
				if err != nil {
					return err
				}
				if err = s.sendCurrentActiveDungeonReturnToTown(ss, runtime, transition, 132, "guild_war_ended"); err != nil {
					return err
				}
			}
			if ready, _ := s.currentSceneTransitionReadyForState(ss); !ready {
				return nil
			}
			return s.writeCurrentGuildWarOverview(ss, g)
		}); err != nil {
			return err
		}
	}
	return nil
}
