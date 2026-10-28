package dnfbridge

import (
	"context"
	"errors"
	"fmt"
	"time"

	dnfaltar "longheng.io/server/internal/modules/dnf/dungeonaltar"
	"longheng.io/server/internal/modules/dnf/worldmap"
)

type currentDungeonAltarAdmissionReservation struct {
	owner      *dnfaltar.Owner
	session    *gameSession
	runtime    *runtimeDungeonState
	kind       worldmap.BloodAltarDungeonKind
	receipt    string
	reservedAt time.Time
	snapshot   dnfaltar.AdmissionSnapshot
	restored   bool
}

func (s *Service) reserveCurrentDungeonAltarAdmission(
	ctx context.Context,
	session *gameSession,
	runtime *runtimeDungeonState,
) (*currentDungeonAltarAdmissionReservation, error) {
	if runtime == nil || runtime.altar == nil {
		return nil, nil
	}
	kind := runtime.altar.Kind
	if kind != worldmap.BloodAltarDungeonKindEndless && kind != worldmap.BloodAltarDungeonKindUltimate {
		return nil, nil
	}
	if session == nil || session.selectedCharacterID == 0 {
		return nil, dnfaltar.ErrCommandInvalid
	}
	var dailyLimit uint32
	limitFound := false
	if limit, found := runtime.Dungeon.Metadata.IntegerValues["limit inout count"]; found {
		if limit <= 0 || limit > int64(^uint32(0)) {
			return nil, fmt.Errorf("invalid %s altar limit inout count=%d", string(kind), limit)
		}
		dailyLimit = uint32(limit)
		limitFound = true
	}
	// Preserve the legacy unlimited Ultimate Altar behavior when its DGN does
	// not declare an explicit admission cap.
	if kind == worldmap.BloodAltarDungeonKindUltimate && !limitFound {
		return nil, nil
	}
	repositories, ok := s.repositoryGroup()
	if !ok {
		return nil, dnfaltar.ErrOwnerUnavailable
	}
	owner, err := dnfaltar.NewOwner(repositories)
	if err != nil {
		return nil, err
	}
	nonce, err := s.currentDungeonAdmissionReceiptNonce()
	if err != nil {
		return nil, err
	}
	attempt := s.allocateDungeonLifecycleToken()
	if attempt == 0 {
		return nil, errors.New("altar admission attempt token is unavailable")
	}
	receipt := fmt.Sprintf("altar-admission-v1:boot:%s:attempt:%d:character:%d:dungeon:%d", nonce, attempt, session.selectedCharacterID, runtime.Dungeon.ID)
	now := s.gameplayNow().UTC()
	var snapshot dnfaltar.AdmissionSnapshot
	if kind == worldmap.BloodAltarDungeonKindEndless {
		catalog, catalogErr := s.dungeonMonsterCatalog()
		if catalogErr != nil {
			return nil, catalogErr
		}
		rule, ruleErr := loadCurrentDungeonAltarAdmissionRule(catalog.source)
		if ruleErr != nil {
			return nil, ruleErr
		}
		if limitFound {
			rule.DailyLimit = dailyLimit
		}
		snapshot, err = owner.ReserveEndless(ctx, fmt.Sprint(session.selectedCharacterID), rule, receipt, now)
	} else {
		snapshot, err = owner.ReserveUltimate(ctx, fmt.Sprint(session.selectedCharacterID), dailyLimit, receipt, now)
	}
	if err != nil {
		return nil, err
	}
	runtime.altar.entryDailyCount = snapshot.DailyEnterCount
	reservation := &currentDungeonAltarAdmissionReservation{
		owner: owner, session: session, runtime: runtime, kind: kind, receipt: receipt,
		reservedAt: now, snapshot: snapshot,
	}
	s.logGameEvent(session, "game-dungeon-altar-admission-reserved",
		"dungeon_id", runtime.Dungeon.ID,
		"kind", kind,
		"daily_limit", dailyLimit,
		"fee", snapshot.Fee,
		"gold_after", snapshot.GoldAfter,
		"daily_enter_count", snapshot.DailyEnterCount,
		"weekly_enter_count", snapshot.WeeklyEnterCount,
		"lotto_point_earned", snapshot.LottoPointEarned,
		"lotto_gold_spent", snapshot.LottoGoldSpent,
		"total_lotto_points", snapshot.TotalLottoPoints,
		"total_lotto_gold", snapshot.TotalLottoGold,
		"replayed", snapshot.Replayed)
	return reservation, nil
}

func (s *Service) restoreCurrentDungeonAltarAdmission(reservation *currentDungeonAltarAdmissionReservation) error {
	if reservation == nil || reservation.owner == nil || reservation.restored {
		return nil
	}
	ctx, cancel := context.WithTimeout(context.Background(), createWriteTimeout)
	defer cancel()
	var restored dnfaltar.AdmissionSnapshot
	var err error
	if reservation.kind == worldmap.BloodAltarDungeonKindUltimate {
		restored, err = reservation.owner.RestoreUltimate(
			ctx,
			fmt.Sprint(reservation.session.selectedCharacterID),
			reservation.receipt,
			reservation.reservedAt,
		)
	} else {
		restored, err = reservation.owner.RestoreEndless(
			ctx,
			fmt.Sprint(reservation.session.selectedCharacterID),
			reservation.receipt,
			reservation.reservedAt,
		)
	}
	if err != nil {
		return err
	}
	reservation.restored = true
	if reservation.runtime != nil && reservation.runtime.altar != nil {
		reservation.runtime.altar.entryDailyCount = restored.DailyEnterCount
	}
	s.logGameEvent(reservation.session, "game-dungeon-altar-admission-restored",
		"dungeon_id", reservation.runtime.Dungeon.ID,
		"kind", reservation.kind,
		"fee", reservation.snapshot.Fee,
		"gold_after", restored.GoldAfter,
		"daily_enter_count", restored.DailyEnterCount,
		"total_lotto_points", restored.TotalLottoPoints,
		"total_lotto_gold", restored.TotalLottoGold)
	return nil
}
