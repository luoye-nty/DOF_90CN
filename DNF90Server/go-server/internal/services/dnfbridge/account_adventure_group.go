package dnfbridge

import (
	"context"
	"errors"
	"fmt"
	"strings"

	"longheng.io/server/internal/modules/dnf/adventuregroup"
	dnfdungeon "longheng.io/server/internal/modules/dnf/dungeon"
	dnfrepo "longheng.io/server/internal/modules/dnf/repository"
)

var (
	errAdventureGroupTableUnavailable  = errors.New("dnf adventure-group table is unavailable")
	errAdventureGroupRosterUnavailable = errors.New("dnf adventure-group account roster is unavailable")
	errAdventureGroupOwnerMismatch     = errors.New("dnf adventure-group selected character owner mismatch")
)

type accountAdventureGroupState struct {
	Characters []dnfrepo.CharacterRecord
	Summary    adventuregroup.Summary
	Projection adventuregroup.Projection
}

// preloadAdventureGroupTable makes the current runtime PVF authoritative for
// adventure-group calculation. Production cannot start with a guessed table.
func (s *Service) preloadAdventureGroupTable(ctx context.Context) error {
	if _, err := s.loadAdventureGroupTable(ctx); err != nil {
		return fmt.Errorf("preload dnf adventure-group table: %w", err)
	}
	return nil
}

func (s *Service) loadAdventureGroupTable(ctx context.Context) (*adventuregroup.Tables, error) {
	if s == nil {
		return nil, errAdventureGroupTableUnavailable
	}
	s.adventureGroupMu.Lock()
	defer s.adventureGroupMu.Unlock()
	if s.adventureGroupTable != nil {
		return s.adventureGroupTable, nil
	}
	if s.adventureGroupLoadErr != nil {
		return nil, s.adventureGroupLoadErr
	}

	s.initialEquipmentMu.Lock()
	archive, err := s.initialEquipmentArchiveLocked()
	s.initialEquipmentMu.Unlock()
	if err != nil {
		s.adventureGroupLoadErr = err
		return nil, err
	}
	table, err := adventuregroup.LoadComplete(ctx, archive)
	if err != nil {
		s.adventureGroupLoadErr = err
		return nil, err
	}
	s.adventureGroupTable = table
	snapshot := table.Snapshot()
	s.logPacketEvent("dnf-adventure-group-table-loaded",
		"point_ranges", snapshot.PointRanges,
		"manage_thresholds", snapshot.ManageThresholds,
		"manage_level_max", snapshot.ManageLevelMax,
		"exp_bonus_levels", snapshot.ExpBonusLevels,
		"gold_bonus_levels", snapshot.GoldBonusLevels,
		"manage_option_levels", snapshot.ManageOptionLevels)
	return table, nil
}

func (s *Service) currentAccountAdventureGroupSummary(ctx context.Context, selected dnfrepo.CharacterRecord, hasSelected bool, sessions ...*gameSession) (adventuregroup.Summary, error) {
	state, err := s.currentAccountAdventureGroupState(ctx, selected, hasSelected, sessions...)
	return state.Summary, err
}

func (s *Service) currentAccountAdventureGroupState(ctx context.Context, selected dnfrepo.CharacterRecord, hasSelected bool, sessions ...*gameSession) (accountAdventureGroupState, error) {
	if s == nil {
		return accountAdventureGroupState{}, errAdventureGroupTableUnavailable
	}
	s.adventureGroupMu.Lock()
	table := s.adventureGroupTable
	loadErr := s.adventureGroupLoadErr
	s.adventureGroupMu.Unlock()
	if table == nil {
		if loadErr != nil {
			return accountAdventureGroupState{}, loadErr
		}
		return accountAdventureGroupState{}, errAdventureGroupTableUnavailable
	}
	repositories, ok := s.repositoryGroup()
	if !ok || repositories.Character == nil {
		return accountAdventureGroupState{}, errAdventureGroupRosterUnavailable
	}
	accountID := s.accountIDForSession(sessions...)
	if hasSelected {
		selectedAccountID := strings.TrimSpace(selected.AccountID)
		if selectedAccountID != "" && selectedAccountID != accountID {
			return accountAdventureGroupState{}, fmt.Errorf("%w: selected=%q current=%q", errAdventureGroupOwnerMismatch, selectedAccountID, accountID)
		}
	}
	characters, err := repositories.Character.ListByAccount(ctx, accountID, maxCharacterSlots)
	if err != nil {
		return accountAdventureGroupState{}, fmt.Errorf("list dnf adventure-group account characters: %w", err)
	}
	rows := make([]adventuregroup.Character, 0, len(characters))
	for _, character := range characters {
		rows = append(rows, adventuregroup.Character{Level: character.Level})
	}
	summary, err := table.Calculate(rows)
	if err != nil {
		return accountAdventureGroupState{}, err
	}
	// 冒险团等级的所有权在账号成长账本：账本一旦存在，名册推导值只能当它的初始
	// 种子。这条覆写必须留在 State 这一层，因为它服务的是**所有**报文与判定路径
	// （mode1/mode3 角色面板、场景、结算、塔次数、商店门槛）。过去只把它写在
	// currentAccountAdventureGroupInfoState（冒险团窗口）里，于是窗口显示账本等级
	// （≤30），其余报文退回名册刻度；而名册刻度被 charactermanage.etc
	// `[manage level point]` 的 13 条阈值钳死（见 tables.go ManageLevelForPoint：
	// 表不够长时最高只能到最后一个阈值），现场表现就是"冒险团 30 级，角色面板还是
	// 13 级的四围"。参考实现（DNF_NEW 的 account_adventure_group.go）同样把覆写
	// 放在这个函数里。
	// 账本读不出来时保留名册值并记一条 deferred：一个坏账本不该打断玩家会话。
	if table.Runtime().Growth.MaximumLevel > 0 && repositories.Account != nil && accountID != "" {
		account, found, loadErr := repositories.Account.Load(ctx, accountID)
		switch {
		case loadErr != nil:
			loadErr = fmt.Errorf("load dnf adventure-group growth account: %w", loadErr)
		case !found:
			loadErr = adventuregroup.ErrAccountNotFound
		default:
			if ledgerSummary, ledgerErr := table.CurrentSummary(account, rows); ledgerErr == nil {
				summary = ledgerSummary
				loadErr = nil
			} else {
				loadErr = ledgerErr
			}
		}
		if loadErr != nil {
			var logSession *gameSession
			if len(sessions) > 0 {
				logSession = sessions[0]
			}
			s.logGameEvent(logSession, "game-adventure-growth-summary-deferred",
				"account_id", accountID, "reason", loadErr)
		}
	}
	return accountAdventureGroupState{
		Characters: characters,
		Summary:    summary,
	}, nil
}

// currentAccountAdventureGroupInfoState adds durable account activity needed
// only by the full 7,420-byte adventure-group payload. The process lock
// serializes the metadata read/modify/write for this local account profile.
func (s *Service) currentAccountAdventureGroupInfoState(
	ctx context.Context,
	selected dnfrepo.CharacterRecord,
	hasSelected bool,
	sessions ...*gameSession,
) (accountAdventureGroupState, error) {
	state, err := s.currentAccountAdventureGroupState(ctx, selected, hasSelected, sessions...)
	if err != nil {
		return accountAdventureGroupState{}, err
	}
	repositories, ok := s.repositoryGroup()
	if !ok || repositories.Account == nil {
		state.Projection = adventuregroup.ProjectActivity(0, state.Characters)
		s.projectAdventureTowerCounts(&state.Projection, dnfrepo.AccountRecord{}, false, state.Summary.ManageLevel)
		s.projectAdventureProfessionCounts(&state.Projection, state.Characters)
		return state, nil
	}
	owner, err := adventuregroup.NewOwner(repositories)
	if err != nil {
		state.Projection = adventuregroup.ProjectActivity(0, state.Characters)
		s.projectAdventureTowerCounts(&state.Projection, dnfrepo.AccountRecord{}, false, state.Summary.ManageLevel)
		s.projectAdventureProfessionCounts(&state.Projection, state.Characters)
		return state, nil
	}
	accountID := s.accountIDForSession(sessions...)
	s.adventureGroupLoginMu.Lock()
	login, err := owner.ObserveDailyLogin(ctx, adventuregroup.ObserveDailyLoginCommand{
		AccountID:  accountID,
		ObservedAt: s.gameplayNow(),
	})
	s.adventureGroupLoginMu.Unlock()
	if err != nil {
		if errors.Is(err, adventuregroup.ErrAccountNotFound) ||
			errors.Is(err, adventuregroup.ErrAccountRequired) {
			state.Projection = adventuregroup.ProjectActivity(0, state.Characters)
			return state, nil
		}
		return accountAdventureGroupState{}, err
	}
	state.Projection = adventuregroup.ProjectActivity(login.ConsecutiveDays, state.Characters)
	if repositories.Settings != nil {
		state.Projection.RepresentativesEnabled = true
		if representativeOwner, ownerErr := adventuregroup.NewRepresentativeOwner(repositories); ownerErr == nil {
			s.adventureRepresentativeMu.Lock()
			representatives, loadErr := representativeOwner.Load(ctx, accountID)
			s.adventureRepresentativeMu.Unlock()
			if loadErr != nil {
				return accountAdventureGroupState{}, loadErr
			}
			state.Projection.Representatives = representatives
		}
	}
	account, found, loadErr := repositories.Account.Load(ctx, accountID)
	if loadErr != nil {
		return accountAdventureGroupState{}, fmt.Errorf("load adventure-group runtime account: %w", loadErr)
	}
	if found {
		now := s.gameplayNow()
		growth := s.adventureGroupTable.Runtime().Growth
		// sessions is variadic; never index it blindly.
		var logSession *gameSession
		if len(sessions) > 0 {
			logSession = sessions[0]
		}
		// The adventure-group level is owned by the account growth ledger once
		// that ledger exists. Seed it from the roster the first time both the
		// account and its roster are in hand, so a later experience grant can
		// never start the ledger below an established level.
		seeded, seedErr := adventuregroup.SeedGrowthState(&account, growth, state.Summary, now)
		if seedErr != nil {
			s.logGameEvent(logSession, "game-adventure-growth-seed-deferred",
				"account_id", accountID, "reason", seedErr)
		} else if seeded {
			if saveErr := repositories.Account.Save(ctx, account); saveErr != nil {
				return accountAdventureGroupState{}, fmt.Errorf("save seeded adventure-group growth: %w", saveErr)
			}
		}
		// 这一次是**种子落库之后**的重读：State 那层读的是种子之前的账号记录，
		// 而新种下的账本对 Level>1 会把进度清零（见 SeedGrowthState 的注释），
		// 窗口要以种子后的值为准。账本已存在时这次重读与 State 的结果一致。
		rows := make([]adventuregroup.Character, 0, len(state.Characters))
		for _, character := range state.Characters {
			rows = append(rows, adventuregroup.Character{Level: character.Level})
		}
		if summary, summaryErr := s.adventureGroupTable.CurrentSummary(account, rows); summaryErr == nil {
			state.Summary = summary
		} else {
			s.logGameEvent(logSession, "game-adventure-growth-summary-deferred",
				"account_id", accountID, "reason", summaryErr)
		}
	}
	s.projectAdventureTowerCounts(&state.Projection, account, found, state.Summary.ManageLevel)
	s.projectAdventureProfessionCounts(&state.Projection, state.Characters)
	if found {
		config := s.adventureGroupTable.Runtime()
		runtime, parseErr := adventuregroup.ParseRuntimeState(account, config, s.gameplayNow())
		if parseErr != nil {
			return accountAdventureGroupState{}, parseErr
		}
		state.Projection.Runtime = runtime
		// 商店里成长胶囊那一格的进度分母 = `[capsule max exp]`（一格 = 100%）。
		state.Projection.CapsuleGauge = adventuregroup.CapsuleGaugeCapacity(config.Capsule)
		// "可获得的经验值"那一栏的领取次数（一格 10% = 1 次）。
		state.Projection.CapsuleClaimCount = adventuregroup.CapsuleClaimCount(
			config.Capsule,
			runtime.GrowthExperience,
		)
	}
	return state, nil
}

// projectAdventureTowerCounts projects the four native tower counters sent at
// body offsets 6..9. Limits come from the active PVF; stale account usage is
// naturally zero for a new service day.
func (s *Service) projectAdventureTowerCounts(projection *adventuregroup.Projection, account dnfrepo.AccountRecord, found bool, adventureLevel int) {
	if s == nil || projection == nil {
		return
	}
	limits := [adventuregroup.TowerTypeCount]uint32{3, 1}
	s.initialEquipmentMu.Lock()
	archive, _ := s.initialEquipmentArchiveLocked()
	s.initialEquipmentMu.Unlock()
	if archive != nil {
		if value, err := dnfdungeon.TowerDailyEnterableLimit(archive, dnfdungeon.TowerDespair, adventureLevel); err == nil {
			limits[adventuregroup.TowerDespairType] = value
		}
		if value, err := dnfdungeon.TowerDailyEnterableLimit(archive, dnfdungeon.TowerGrief, adventureLevel); err == nil {
			limits[adventuregroup.TowerGriefType] = value
		}
	}
	projection.TowerLimits = limits
	if !found {
		projection.TowerRemaining = limits
		return
	}
	for index, kind := range [...]dnfdungeon.TowerKind{dnfdungeon.TowerDespair, dnfdungeon.TowerGrief} {
		used, err := dnfdungeon.TowerDailyUsed(account, kind, s.gameplayNow())
		if err != nil || used >= limits[index] {
			projection.TowerRemaining[index] = 0
			continue
		}
		projection.TowerRemaining[index] = limits[index] - used
	}
}

// projectAdventureProfessionCounts fills the native 20-row profession
// descriptor block: row N is how many account characters classify into
// profession N, per the current runtime PVF's [profession classification].
func (s *Service) projectAdventureProfessionCounts(projection *adventuregroup.Projection, characters []dnfrepo.CharacterRecord) {
	if s == nil || projection == nil {
		return
	}
	s.adventureGroupMu.Lock()
	table := s.adventureGroupTable
	s.adventureGroupMu.Unlock()
	if table == nil {
		return
	}
	config := table.Runtime()
	for index := range projection.ProfessionCounts {
		projection.ProfessionCounts[index] = currentAdventureProfessionCharacterCount(characters, config, byte(index))
	}
}

func (s *Service) currentAccountAdventureGroupSummaryForPacket(ctx context.Context, session *gameSession, selected dnfrepo.CharacterRecord, hasSelected bool) adventuregroup.Summary {
	summary, err := s.currentAccountAdventureGroupSummary(ctx, selected, hasSelected, session)
	if err == nil {
		return summary
	}
	if session != nil {
		s.logGameEvent(session, "game-adventure-group-summary-unavailable",
			"character_id", selected.CharacterID,
			"error", err)
	}
	// Production Start preloads the strict PVF table. This zero result is only
	// a fail-closed wire value for a transient repository error or direct unit
	// construction that bypasses Start; it is always accompanied by an error.
	return adventuregroup.Summary{}
}
