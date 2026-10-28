package dnfrobot

import (
	"context"
	"fmt"
	"hash/fnv"
	"strings"
	"time"

	dnfrobotdomain "longheng.io/server/internal/modules/dnf/robot"
)

const (
	// AutomaticFleetTarget is deliberately below robot.MaxProfiles so operators
	// can still keep manual profiles beside the zero-touch fleet.
	AutomaticFleetTarget = 500
	// AutomaticFleetOnlineTarget keeps all 500 persistent identities available
	// while bounding native game sessions.  The recovered robot supervisor used
	// a finite actor/pending pool; treating every profile as an independently
	// reconnecting actor can starve real character-selection transactions.
	AutomaticFleetOnlineTarget = 64
	// automaticFleetPendingLimit mirrors the recovered supervisor's bounded
	// actor backlog: at most eight batches may be waiting to log in or
	// reconnect. With the current two-session native handshake limit this keeps
	// repository pressure bounded at sixteen pending automatic runtimes.
	automaticFleetPendingLimit       = 16
	automaticFleetRetry              = 30 * time.Second
	automaticFleetGatewayRetry       = 2 * time.Second
	automaticFleetGatewaySamples     = 3
	automaticFleetGatewayGap         = 250 * time.Millisecond
	automaticFleetPreferredChannelID = 19
	automaticFleetConfigVersion      = 7
	automaticFleetProfilePrefix      = "auto-generated-"
)

const (
	automationActionMove      = "move"
	automationActionChat      = "chat"
	automationActionStore     = "store"
	automationActionMegaphone = "megaphone"
)

// AutomaticFleetProvider creates or reuses one native, repository-backed
// character. Implementations must be idempotent for the same index.
type AutomaticFleetProvider interface {
	EnsureAutomaticRobotProfile(ctx context.Context, index int) (dnfrobotdomain.Profile, error)
}

// DefaultAutomaticFleetPolicy is the zero-operation policy installed only
// when no prior automation policy exists. The intervals are intentionally
// broad; per-profile personalities further stagger every action.
func DefaultAutomaticFleetPolicy() dnfrobotdomain.AutomationPolicy {
	return dnfrobotdomain.AutomationPolicy{
		Enabled: true, TargetOnline: AutomaticFleetOnlineTarget,
		ScaleIntervalSeconds: 5, ScaleUpBatch: 2, ScaleDownBatch: 2,
		MoveIntervalMinSeconds: 12, MoveIntervalMaxSeconds: 45, MoveRadius: 120,
		ChatIntervalMinSeconds: 180, ChatIntervalMaxSeconds: 600, ChatMode: 3,
		ChatMessages: []string{
			"今天也要努力冒险", "有人一起刷图吗", "整理一下背包", "城镇里真热闹",
			"看看我的小店吧", "休息一会再出发", "祝大家都出好装备", "准备下一次冒险",
		},
		MegaphoneIntervalMinSeconds: 900, MegaphoneIntervalMaxSeconds: 1800,
		MegaphoneProfileModulo:  50,
		StoreIntervalMinSeconds: 90, StoreIntervalMaxSeconds: 300,
		StoreDurationSeconds: 120, StoreConcurrent: 2, StoreMaximumRows: 8,
		StoreUnitPrice: 10000, StoreTitle: "冒险家商店",
	}
}

func (m *Manager) runAutomaticFleet(ctx context.Context) {
	defer m.wg.Done()
	provider, ok := m.endpoint.(AutomaticFleetProvider)
	if !ok {
		return
	}
	if stability, available := m.endpoint.(AutomaticFleetGatewayStability); available {
		for {
			if err := stability.WaitRobotGameEndpointStable(ctx, automaticFleetPreferredChannelID, automaticFleetGatewaySamples, automaticFleetGatewayGap); err == nil {
				break
			} else if ctx.Err() != nil {
				return
			} else {
				m.logger.Warn("dnf automatic robot gateway is not stable", "channel", automaticFleetPreferredChannelID, "error", err)
			}
			timer := time.NewTimer(automaticFleetGatewayRetry)
			select {
			case <-ctx.Done():
				timer.Stop()
				return
			case <-timer.C:
			}
		}
	}
	ensured := make(map[int]struct{}, AutomaticFleetTarget)
	timer := time.NewTimer(0)
	defer timer.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-timer.C:
		}
		operation := m.BeginOperation("automatic_fleet", fmt.Sprintf("target=%d", AutomaticFleetTarget))
		succeeded, failed := 0, 0
		for index := 1; index <= AutomaticFleetTarget; index++ {
			if _, ok := ensured[index]; ok {
				succeeded++
				continue
			}
			if ctx.Err() != nil {
				m.CompleteOperation(operation.ID, fmt.Sprintf("ensured=%d pending=%d", succeeded, AutomaticFleetTarget-succeeded), ctx.Err())
				return
			}
			profile, err := provider.EnsureAutomaticRobotProfile(ctx, index)
			if err == nil {
				// Fleet profiles are scheduler-owned. Direct autostart would fan out
				// hundreds of character-selection transactions at process startup.
				profile.AutoStart = false
				err = m.installAutomaticProfile(ctx, profile)
			}
			if err != nil {
				failed++
				m.logger.Warn("dnf automatic robot ensure deferred", "index", index, "error", err)
				// Repository pressure is shared by every following profile.  Stop at
				// the first failure instead of immediately starting hundreds of
				// transactions that are expected to fail for the same reason.
				break
			}
			ensured[index] = struct{}{}
			succeeded++
			provisionDelay := m.markAutomaticFleetProfileEnsured()
			if provisionDelay > 0 && index < AutomaticFleetTarget {
				pace := time.NewTimer(provisionDelay)
				select {
				case <-ctx.Done():
					pace.Stop()
					m.CompleteOperation(operation.ID, fmt.Sprintf("ensured=%d pending=%d", succeeded, AutomaticFleetTarget-succeeded), ctx.Err())
					return
				case <-pace.C:
				}
			}
		}
		summary := fmt.Sprintf("ensured=%d pending=%d", succeeded, AutomaticFleetTarget-succeeded)
		if failed == 0 {
			m.CompleteOperation(operation.ID, summary, nil)
			return
		}
		m.CompleteOperation(operation.ID, summary, fmt.Errorf("%d automatic robot profiles are pending", failed))
		timer.Reset(automaticFleetRetry)
	}
}

func (m *Manager) markAutomaticFleetProfileEnsured() time.Duration {
	if m == nil {
		return 0
	}
	m.mu.Lock()
	defer m.mu.Unlock()
	if !m.automaticFleetReady {
		m.automaticFleetReady = true
		m.nextScale = time.Time{}
	}
	return m.fleetProvisionDelay
}

func (m *Manager) installAutomaticProfile(ctx context.Context, profile dnfrobotdomain.Profile) error {
	m.mu.RLock()
	current, found := findProfile(m.config, profile.ID)
	if found && current == profile {
		m.mu.RUnlock()
		return nil
	}
	for _, candidate := range m.config.Profiles {
		if candidate.CharacterID == profile.CharacterID && profileKey(candidate.ID) != profileKey(profile.ID) {
			m.mu.RUnlock()
			return fmt.Errorf("automatic character %d is already assigned to profile %q", profile.CharacterID, candidate.ID)
		}
	}
	m.mu.RUnlock()
	_, err := m.Upsert(ctx, profile)
	return err
}

func automationBehaviorSeed(profile dnfrobotdomain.Profile) uint64 {
	hash := fnv.New64a()
	_, _ = hash.Write([]byte(strings.ToLower(strings.TrimSpace(profile.ID))))
	_, _ = hash.Write([]byte{0})
	_, _ = hash.Write([]byte(profile.AccountID))
	_, _ = hash.Write([]byte{byte(profile.CharacterID), byte(profile.CharacterID >> 8)})
	return hash.Sum64()
}

func automationPersonality(seed uint64) string {
	switch seed % 4 {
	case 0:
		return "merchant"
	case 1:
		return "wanderer"
	case 2:
		return "social"
	default:
		return "balanced"
	}
}

func automationBehaviorDelay(minSeconds, maxSeconds, cursor int, seed uint64, personality, action string) time.Duration {
	if minSeconds <= 0 {
		return 0
	}
	numerator := 100
	switch personality {
	case "merchant":
		switch action {
		case automationActionStore:
			numerator = 70
		case automationActionMove:
			numerator = 140
		case automationActionChat:
			numerator = 120
		}
	case "wanderer":
		if action == automationActionMove {
			numerator = 65
		} else if action == automationActionStore {
			numerator = 150
		}
	case "social":
		if action == automationActionChat {
			numerator = 60
		} else if action == automationActionStore {
			numerator = 130
		}
	}
	minSeconds = maxInt(1, minSeconds*numerator/100)
	maxSeconds = maxInt(minSeconds, maxSeconds*numerator/100)
	if seed == 0 {
		return automationDelay(minSeconds, maxSeconds, cursor)
	}
	width := maxSeconds - minSeconds + 1
	offset := int(automationMix(seed, uint64(cursor), stringSeed(action)) % uint64(width))
	return time.Duration(minSeconds+offset) * time.Second
}

func automationBehaviorPosition(originX, originY, radius uint16, cursor int, seed uint64) (uint16, uint16) {
	if seed == 0 {
		return automationPosition(originX, originY, radius, cursor)
	}
	if radius == 0 {
		return originX, originY
	}
	mixed := automationMix(seed, uint64(cursor), 0x6d6f7665)
	distance := int(radius)/2 + int(mixed%uint64(maxInt(1, int(radius)/2+1)))
	directions := [8][2]int{{1, 0}, {1, 1}, {0, 1}, {-1, 1}, {-1, 0}, {-1, -1}, {0, -1}, {1, -1}}
	direction := directions[(mixed>>8)&7]
	x := clampAutomationCoordinate(int(originX) + direction[0]*distance)
	y := clampAutomationCoordinate(int(originY) + direction[1]*distance)
	return uint16(x), uint16(y)
}

func automationBehaviorPositionForProfile(profile dnfrobotdomain.Profile, radius uint16, cursor int, seed uint64) (uint16, uint16) {
	x, y := automationBehaviorPosition(profile.X, profile.Y, radius, cursor, seed)
	if !profile.MoveBoundsSet {
		return x, y
	}
	return clampAutomationRange(x, profile.MoveMinX, profile.MoveMaxX), clampAutomationRange(y, profile.MoveMinY, profile.MoveMaxY)
}

func clampAutomationRange(value, minimum, maximum uint16) uint16 {
	if value < minimum {
		return minimum
	}
	if value > maximum {
		return maximum
	}
	return value
}

func automationMessageIndex(cursor int, seed uint64, count int) int {
	if count <= 0 {
		return 0
	}
	if seed == 0 {
		return cursor % count
	}
	return int(automationMix(seed, uint64(cursor), 0x63686174) % uint64(count))
}

func automationStorePrice(base uint32, seed uint64, cursor int) uint32 {
	if base == 0 || seed == 0 {
		return base
	}
	percent := uint64(85) + automationMix(seed, uint64(cursor), 0x7072696365)%31
	price := uint64(base) * percent / 100
	if price == 0 {
		return 1
	}
	if price > uint64(^uint32(0)) {
		return ^uint32(0)
	}
	return uint32(price)
}

func automationMix(seed, cursor, salt uint64) uint64 {
	value := seed ^ salt ^ (cursor+1)*0x9e3779b97f4a7c15
	value ^= value >> 30
	value *= 0xbf58476d1ce4e5b9
	value ^= value >> 27
	value *= 0x94d049bb133111eb
	return value ^ (value >> 31)
}

func stringSeed(value string) uint64 {
	hash := fnv.New64a()
	_, _ = hash.Write([]byte(value))
	return hash.Sum64()
}

func clampAutomationCoordinate(value int) int {
	if value < 0 {
		return 0
	}
	if value > 65535 {
		return 65535
	}
	return value
}

func maxInt(a, b int) int {
	if a > b {
		return a
	}
	return b
}
