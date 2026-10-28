package dnfbridge

import (
	"context"
	"encoding/binary"
	"strconv"
	"testing"
	"time"

	"longheng.io/server/internal/modules/dnf/dnfenum"
	"longheng.io/server/internal/modules/dnf/premium"
	dnfproto "longheng.io/server/internal/modules/dnf/protocol"
	dnfrepo "longheng.io/server/internal/modules/dnf/repository"
)

// 修炼场 is a free practice room: leaving it must never weaken, however the
// exit happens. Every other forced exit still does.
func TestPracticeRoomExitDoesNotWeaken(t *testing.T) {
	for _, testCase := range []struct {
		name      string
		practice  bool
		wantLevel int64
	}{
		{name: "ordinary dungeon give-up weakens", practice: false, wantLevel: currentWeaknessForcedReturnLevel},
		{name: "practice room give-up does not", practice: true, wantLevel: 0},
	} {
		t.Run(testCase.name, func(t *testing.T) {
			service, session, runtime := newBackToVillageRuntime(t)
			if testCase.practice {
				runtime.ProtocolMode = currentDungeonProtocolModePractice
			}
			if got := currentDungeonRuntimeIsPractice(runtime); got != testCase.practice {
				t.Fatalf("runtime practice=%t want %t", got, testCase.practice)
			}
			request, err := dnfproto.BuildChannelPacket(
				uint16(dnfenum.CmdPacketBack2Village), nil, 0, dnfproto.DefaultChannelClassification,
			)
			if err != nil {
				t.Fatal(err)
			}
			if err := service.handleGameUpper(session, request); err != nil {
				t.Fatalf("handle op132: %v", err)
			}
			repositories, ok := service.repositoryGroup()
			if !ok {
				t.Fatal("no repositories")
			}
			character, _, err := repositories.Character.Load(
				context.Background(), strconv.Itoa(int(session.selectedCharacterID)))
			if err != nil {
				t.Fatal(err)
			}
			if got := currentWeaknessLevel(character); got != testCase.wantLevel {
				t.Fatalf("weakness after exit=%d want %d", got, testCase.wantLevel)
			}
		})
	}
}

// The 解除虚弱 NPC sends op9 on the legacy transport (cmd=1 type=9). It was
// registered only under UpperHandlers, so the packet arrived, was logged, and
// was then dropped without ever reaching a handler -- the button did nothing
// and no recovery event was ever written. Every opcode in this module has to
// be reachable on both transports.
func TestPremiumModuleRegistersEveryOpcodeOnBothTransports(t *testing.T) {
	module := premiumGameplayModule()
	for _, opcode := range []uint16{
		uint16(dnfenum.CmdPacketPremiumService),
		uint16(dnfenum.CmdPacketRecoverStamina),
		uint16(dnfenum.CmdPacketUpdateContractOfCubeInfo),
		uint16(dnfenum.CmdPacketUseLimitCube),
	} {
		if module.LegacyHandlers[opcode] == nil || module.UpperHandlers[opcode] == nil {
			t.Fatalf("opcode %d is not registered for both current transports", opcode)
		}
	}
}

// The 解除虚弱 NPC talks to the server on the legacy transport. Registering the
// opcode is not enough on its own: the request has to reach the handler and
// actually clear the penalty, or the button stays dead exactly as the operator
// reported it.
func TestLegacyWeaknessRecoveryRequestReachesTheHandler(t *testing.T) {
	devilType := premium.DevilSlotType(premium.DevilSlotFreeWeakness)
	service, session, repositories, connection := prepareCurrentWeaknessTest(
		t, 40, 4, 0, map[int64]int64{devilType: 86400},
	)
	registry, err := newGameplayModuleRegistry(premiumGameplayModule())
	if err != nil {
		t.Fatal(err)
	}
	body := make([]byte, 2)
	binary.LittleEndian.PutUint16(body, 1)
	handled, err := registry.DispatchLegacy(
		service, session, uint16(dnfenum.CmdPacketRecoverStamina), body,
	)
	if err != nil {
		t.Fatal(err)
	}
	if !handled {
		t.Fatal("a legacy op9 recovery request was dropped with no handler")
	}
	packet, _ := splitGameServerUpperPacket(t, connection.write.Bytes())
	if packet.Header.MsgID != uint16(dnfenum.CmdPacketRecoverStamina) ||
		len(packet.Body) != 1 || packet.Body[0] != 1 {
		t.Fatalf("legacy recovery ACK=%+v body=%x", packet.Header, packet.Body)
	}
	if got := currentWeaknessStoredLevel(t, repositories); got != 0 {
		t.Fatalf("weakness after a legacy recovery=%d want 0", got)
	}
}

// The penalty only ever went up before 2026-09-11: the paid NPC recovery was
// the single way down, so six abandoned dungeon runs pinned a test character at
// level 9 (19% health) with no way back short of paying. Recovery is lazy and
// **在线秒**驱动：角色不在线时计时锁定，上线才继续走（运营要求 2026-09-15）。
// 所以这里的时钟是 `weakness_online_seconds`（已结清窗口）+「窗口起点到现在」。
func TestSettleCurrentWeaknessRecoveryStepsOneLevelPerInterval(t *testing.T) {
	base := time.Date(2026, 9, 11, 23, 0, 0, 0, time.UTC)
	interval := int64(currentWeaknessRecoveryInterval / time.Second)

	// windowStart = 本次在线窗口的墙上起点；倒计时锚点用"在线秒"。
	// 锚点为 -1 = "惩罚刚下、还没锚点"（0 是合法的在线秒位置，不能当哨兵）。
	newCharacter := func(level, startedAtOnlineSeconds int64) dnfrepo.CharacterRecord {
		stats := map[string]int64{
			currentWeaknessStatKey:              level,
			currentWeaknessOnlineWindowStatKey:  base.Unix(),
			currentWeaknessOnlineSecondsStatKey: 0,
		}
		// 等级 0 的存档不带倒计时（真实数据就是这样，见 settle 里的清理分支）。
		if level > 0 {
			stats[currentWeaknessRecoveryAtStatKey] = startedAtOnlineSeconds
		}
		return dnfrepo.CharacterRecord{CharacterID: "1", Stats: stats}
	}

	for _, testCase := range []struct {
		name        string
		level       int64
		startedAt   int64
		elapsed     time.Duration
		wantLevel   int64
		wantChanged bool
	}{
		{name: "before one interval nothing moves", level: 9, startedAt: 0, elapsed: currentWeaknessRecoveryInterval - time.Second, wantLevel: 9},
		{name: "one interval recovers one level", level: 9, startedAt: 0, elapsed: currentWeaknessRecoveryInterval, wantLevel: 8, wantChanged: true},
		// 一次结算最多掉一级（口径 = 每在线一分钟一级），所以跨越多个 interval 的一次调用
		// 也只掉一级；余量由后续结算按同一公式补上。
		{name: "one settle never drops more than one level", level: 9, startedAt: 0, elapsed: 3 * currentWeaknessRecoveryInterval, wantLevel: 8, wantChanged: true},
		{name: "a healthy character stays healthy", level: 0, startedAt: 0, elapsed: 5 * currentWeaknessRecoveryInterval, wantLevel: 0},
	} {
		t.Run(testCase.name, func(t *testing.T) {
			character := newCharacter(testCase.level, testCase.startedAt)
			level, changed := settleCurrentWeaknessRecovery(&character, base.Add(testCase.elapsed))
			if level != testCase.wantLevel || changed != testCase.wantChanged {
				t.Fatalf("level=%d changed=%t want level=%d changed=%t", level, changed, testCase.wantLevel, testCase.wantChanged)
			}
			if got := currentWeaknessLevel(character); got != testCase.wantLevel {
				t.Fatalf("stored level=%d want %d", got, testCase.wantLevel)
			}
			// A cleared penalty must drop its countdown so the next one starts
			// a fresh clock instead of instantly expiring.
			if testCase.wantLevel == 0 {
				if _, found := character.Stats[currentWeaknessRecoveryAtStatKey]; found {
					t.Fatalf("healthy character kept a recovery timestamp: %+v", character.Stats)
				}
			}
		})
	}

	// The remainder is carried. Settling every minute must still recover on the
	// interval; restarting the clock on each settle would stall recovery
	// forever for a character whose record is read often.
	t.Run("frequent settles still recover", func(t *testing.T) {
		character := newCharacter(3, 0)
		for minute := 1; minute <= int(currentWeaknessRecoveryInterval/time.Minute); minute++ {
			settleCurrentWeaknessRecovery(&character, base.Add(time.Duration(minute)*time.Minute))
		}
		if got := currentWeaknessLevel(character); got != 2 {
			t.Fatalf("level=%d want 2 after one interval of per-minute settles", got)
		}
		if got := character.Stats[currentWeaknessRecoveryAtStatKey]; got != interval {
			t.Fatalf("countdown=%d want %d (remainder must carry, in online seconds)", got, interval)
		}
	})

	// 长时间在线（例如服务端重启后才发现）也必须**逐次**恢复：每次结算掉一级，
	// 直到等级归零；不会一口气清空。
	t.Run("a long online window clears one level per settle", func(t *testing.T) {
		character := newCharacter(3, 0)
		levels := make([]int64, 0, 4)
		for round := 1; round <= 4; round++ {
			settleCurrentWeaknessRecovery(&character, base.Add(time.Duration(round)*currentWeaknessRecoveryInterval))
			levels = append(levels, currentWeaknessLevel(character))
		}
		want := []int64{2, 1, 0, 0}
		for index := range want {
			if levels[index] != want[index] {
				t.Fatalf("levels=%v want %v", levels, want)
			}
		}
		if _, found := character.Stats[currentWeaknessRecoveryAtStatKey]; found {
			t.Fatalf("cleared penalty kept a countdown: %+v", character.Stats)
		}
	})

	// 惩罚刚下、还没有锚点（-1）：从当前在线秒起算，而不是直接免罚。
	t.Run("a penalty without an anchor starts the clock at the current online second", func(t *testing.T) {
		character := newCharacter(9, -1)
		level, changed := settleCurrentWeaknessRecovery(&character, base)
		if level != 9 || !changed {
			t.Fatalf("level=%d changed=%t want 9/true", level, changed)
		}
		if got := character.Stats[currentWeaknessRecoveryAtStatKey]; got != 0 {
			t.Fatalf("countdown=%d want 0 (online seconds)", got)
		}
	})

	// 老存档里锚点可能是 unix 秒（远大于在线秒）。那不能当成"已经过了很久"，
	// 否则一次上线就把惩罚全清掉 —— 必须重锚到当前在线秒。
	t.Run("a countdown from the old wall clock does not clear the penalty", func(t *testing.T) {
		character := newCharacter(4, base.Add(time.Hour).Unix())
		level, changed := settleCurrentWeaknessRecovery(&character, base)
		if level != 4 || !changed {
			t.Fatalf("level=%d changed=%t want 4/true", level, changed)
		}
		if got := character.Stats[currentWeaknessRecoveryAtStatKey]; got != 0 {
			t.Fatalf("countdown=%d want 0 (re-anchored to online seconds)", got)
		}
	})

	// Condition is what the client renders as the health cap, and it is the
	// number the operator reported (level 9 -> 19%, level 6 -> 46%).
	t.Run("condition mapping matches the reported health caps", func(t *testing.T) {
		if got := currentWeaknessCondition(9); got != 19 {
			t.Fatalf("level 9 condition=%d want 19", got)
		}
		if got := currentWeaknessCondition(6); got != 46 {
			t.Fatalf("level 6 condition=%d want 46", got)
		}
		if got := currentWeaknessCondition(0); got != 100 {
			t.Fatalf("healthy condition=%d want 100", got)
		}
	})
}

// 离线锁定：窗口起点为 0（角色不在线）时，无论墙上时间过去多久，在线秒都不前进，
// 所以虚弱计时原地不动。这是运营要求 2026-09-15 的核心行为。
func TestWeaknessCountdownIsLockedWhileOffline(t *testing.T) {
	base := time.Date(2026, 9, 12, 10, 0, 0, 0, time.UTC)

	t.Run("window settled at the countdown anchor", func(t *testing.T) {
		character := dnfrepo.CharacterRecord{CharacterID: "1", Stats: map[string]int64{
			currentWeaknessStatKey:              9,
			currentWeaknessRecoveryAtStatKey:    120,
			currentWeaknessOnlineSecondsStatKey: 120,
			currentWeaknessOnlineWindowStatKey:  0, // 不在线
		}}
		level, changed := settleCurrentWeaknessRecovery(&character, base.Add(24*time.Hour))
		if level != 9 || changed {
			t.Fatalf("离线期间计时必须锁定: level=%d changed=%t want 9/false", level, changed)
		}
		if got := character.Stats[currentWeaknessOnlineSecondsStatKey]; got != 120 {
			t.Fatalf("离线期间在线秒不能前进: %d want 120", got)
		}
	})

	t.Run("the countdown resumes once the character is online again", func(t *testing.T) {
		character := dnfrepo.CharacterRecord{CharacterID: "1", Stats: map[string]int64{
			currentWeaknessStatKey:              9,
			currentWeaknessRecoveryAtStatKey:    120,
			currentWeaknessOnlineSecondsStatKey: 120,
			currentWeaknessOnlineWindowStatKey:  base.Unix(), // 本次上线
		}}
		level, changed := settleCurrentWeaknessRecovery(&character, base.Add(currentWeaknessRecoveryInterval))
		if !changed || level != 8 {
			t.Fatalf("上线一个间隔后应掉一级: level=%d changed=%t want 8/true", level, changed)
		}
	})
}

// A forced exit applies the whole penalty and restarts the countdown, so a
// player who gives up twice in a row owes a fresh interval for the second one
// instead of inheriting the first one's remainder.
func TestForcedReturnRestartsTheRecoveryCountdown(t *testing.T) {
	service, session, repositories, _ := prepareCurrentWeaknessTest(t, 40, 0, 0, nil)
	// 角色在线：给出窗口起点，否则在线秒不前进（离线锁定语义）。
	character, found, err := repositories.Character.Load(context.Background(), "5")
	if err != nil || !found {
		t.Fatalf("load character found=%t err=%v", found, err)
	}
	if character.Stats == nil {
		character.Stats = map[string]int64{}
	}
	character.Stats[currentWeaknessOnlineWindowStatKey] = service.gameplayNow().Unix()
	if err := repositories.Character.Save(context.Background(), character); err != nil {
		t.Fatal(err)
	}
	service.applyCurrentForcedReturnWeakness(session, "test_forced_exit")
	character, found, err = repositories.Character.Load(context.Background(), "5")
	if err != nil || !found {
		t.Fatalf("load character found=%t err=%v", found, err)
	}
	// 新惩罚把倒计时锚到"当前在线秒"（刚上线时为 0）。
	if got := character.Stats[currentWeaknessRecoveryAtStatKey]; got != 0 {
		t.Fatalf("countdown=%d want 0 (online seconds at login)", got)
	}
	// One interval later exactly one level has worn off, not the whole penalty.
	later := service.gameplayNow().Add(currentWeaknessRecoveryInterval)
	level, changed := settleCurrentWeaknessRecovery(&character, later)
	if !changed || level != currentWeaknessForcedReturnLevel-1 {
		t.Fatalf("level=%d changed=%t want %d/true", level, changed, currentWeaknessForcedReturnLevel-1)
	}
}
