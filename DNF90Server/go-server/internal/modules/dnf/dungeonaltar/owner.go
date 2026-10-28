package dungeonaltar

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"math"
	"strconv"
	"strings"
	"time"

	"longheng.io/server/internal/modules/dnf/premium"
	dnfrepo "longheng.io/server/internal/modules/dnf/repository"
)

const settingsSuffix = ":dungeon_altar_v1"

const (
	receiptActive   = "active"
	receiptRestored = "restored"

	endlessDailyCountKey  = "daily_count"
	ultimateDailyCountKey = "ultimate_daily_count"
)

type Owner struct {
	repositories dnfrepo.Group
}

func NewOwner(repositories dnfrepo.Group) (*Owner, error) {
	if repositories.Character == nil || repositories.Settings == nil || repositories.CharacterSettlement == nil {
		return nil, ErrOwnerUnavailable
	}
	return &Owner{repositories: repositories}, nil
}

func (o *Owner) ReserveEndless(
	ctx context.Context,
	characterID string,
	rule AdmissionRule,
	receipt string,
	now time.Time,
) (AdmissionSnapshot, error) {
	if o == nil || o.repositories.CharacterSettlement == nil {
		return AdmissionSnapshot{}, ErrOwnerUnavailable
	}
	characterID = strings.TrimSpace(characterID)
	receipt = strings.TrimSpace(receipt)
	if characterID == "" || receipt == "" {
		return AdmissionSnapshot{}, fmt.Errorf("%w: character and receipt are required", ErrCommandInvalid)
	}
	if err := validateAdmissionRule(rule); err != nil {
		return AdmissionSnapshot{}, err
	}
	if now.IsZero() {
		now = time.Now()
	}
	now = now.UTC()
	ctx = contextOrBackground(ctx)
	var result AdmissionSnapshot
	err := o.repositories.CharacterSettlement.WithinCharacterSettlement(ctx, characterID, func(tx dnfrepo.Group) error {
		if tx.Character == nil || tx.Settings == nil {
			return ErrOwnerUnavailable
		}
		character, found, err := tx.Character.Load(ctx, characterID)
		if err != nil {
			return err
		}
		if !found {
			return ErrCharacterMissing
		}
		character = dnfrepo.CloneCharacter(character)
		if character.Stats == nil {
			character.Stats = make(map[string]int64)
		}
		settings, found, err := tx.Settings.Load(ctx, SettingsScope(characterID))
		if err != nil {
			return err
		}
		settings = dnfrepo.CloneSettings(settings)
		if !found {
			settings.Scope = SettingsScope(characterID)
		}
		if settings.Values == nil {
			settings.Values = make(map[string]string)
		}
		resetAdmissionWindows(settings.Values, now)
		receiptKey := admissionReceiptKey(receipt)
		if stored := strings.TrimSpace(settings.Values[receiptKey]); stored != "" {
			replayed, state, parseErr := parseAdmissionReceipt(stored)
			if parseErr != nil {
				return parseErr
			}
			if state == receiptRestored {
				return ErrReceiptRestored
			}
			replayed.CharacterID = characterID
			replayed.Replayed = true
			result = replayed
			return nil
		}
		daily, err := storedUint32(settings.Values, endlessDailyCountKey)
		if err != nil {
			return err
		}
		weekly, err := storedUint32(settings.Values, "weekly_count")
		if err != nil {
			return err
		}
		if rule.DailyLimit != 0 && daily >= rule.DailyLimit {
			return fmt.Errorf("%w: used=%d limit=%d", ErrDailyLimit, daily, rule.DailyLimit)
		}
		level := character.Level
		if level < 0 {
			level = 0
		}
		if level > math.MaxUint16 {
			level = math.MaxUint16
		}
		fee, err := admissionFee(rule, uint16(level), daily)
		if err != nil {
			return err
		}
		basePrice, err := admissionBasePrice(rule, uint16(level))
		if err != nil {
			return err
		}
		lottoPoint := admissionLottoPoint(rule, daily)
		lottoGold := fee - basePrice
		totalLottoPoints, err := storedUint32(settings.Values, "lotto_points")
		if err != nil || totalLottoPoints > math.MaxUint32-lottoPoint {
			return ErrReceiptMismatch
		}
		totalLottoGold, err := storedInt64(settings.Values, "lotto_use_gold")
		if err != nil || lottoGold < 0 || totalLottoGold > math.MaxInt64-lottoGold {
			return ErrReceiptMismatch
		}
		gold := character.Stats["gold"]
		if gold < fee {
			return fmt.Errorf("%w: gold=%d fee=%d", ErrGoldInsufficient, gold, fee)
		}
		daily++
		weekly++
		totalLottoPoints += lottoPoint
		totalLottoGold += lottoGold
		gold -= fee
		character.Stats["gold"] = gold
		settings.Values[endlessDailyCountKey] = strconv.FormatUint(uint64(daily), 10)
		settings.Values["weekly_count"] = strconv.FormatUint(uint64(weekly), 10)
		settings.Values["lotto_points"] = strconv.FormatUint(uint64(totalLottoPoints), 10)
		settings.Values["lotto_use_gold"] = strconv.FormatInt(totalLottoGold, 10)
		result = AdmissionSnapshot{
			CharacterID: characterID, Fee: fee, GoldAfter: gold,
			DailyEnterCount: daily, WeeklyEnterCount: weekly,
			LottoPointEarned: lottoPoint, LottoGoldSpent: lottoGold,
			TotalLottoPoints: totalLottoPoints, TotalLottoGold: totalLottoGold,
		}
		settings.Values[receiptKey] = formatAdmissionReceipt(result, receiptActive)
		settings.UpdatedAt = now
		if err := dnfrepo.SaveCharacterFields(ctx, tx.Character, character, dnfrepo.CharacterFieldStats); err != nil {
			return err
		}
		return dnfrepo.SaveSettingsFields(ctx, tx.Settings, settings, dnfrepo.SettingsFieldValues)
	})
	return result, err
}

func (o *Owner) RestoreEndless(
	ctx context.Context,
	characterID string,
	receipt string,
	now time.Time,
) (AdmissionSnapshot, error) {
	if o == nil || o.repositories.CharacterSettlement == nil {
		return AdmissionSnapshot{}, ErrOwnerUnavailable
	}
	characterID = strings.TrimSpace(characterID)
	receipt = strings.TrimSpace(receipt)
	if characterID == "" || receipt == "" {
		return AdmissionSnapshot{}, fmt.Errorf("%w: character and receipt are required", ErrCommandInvalid)
	}
	if now.IsZero() {
		now = time.Now()
	}
	now = now.UTC()
	ctx = contextOrBackground(ctx)
	var result AdmissionSnapshot
	err := o.repositories.CharacterSettlement.WithinCharacterSettlement(ctx, characterID, func(tx dnfrepo.Group) error {
		character, found, err := tx.Character.Load(ctx, characterID)
		if err != nil {
			return err
		}
		if !found {
			return ErrCharacterMissing
		}
		settings, found, err := tx.Settings.Load(ctx, SettingsScope(characterID))
		if err != nil {
			return err
		}
		if !found || settings.Values == nil {
			return ErrReceiptMismatch
		}
		settings = dnfrepo.CloneSettings(settings)
		key := admissionReceiptKey(receipt)
		stored, state, err := parseAdmissionReceipt(settings.Values[key])
		if err != nil {
			return err
		}
		if state == receiptRestored {
			stored.CharacterID = characterID
			stored.Replayed = true
			result = stored
			return nil
		}
		daily, err := storedUint32(settings.Values, endlessDailyCountKey)
		if err != nil {
			return err
		}
		weekly, err := storedUint32(settings.Values, "weekly_count")
		if err != nil {
			return err
		}
		if premium.ServiceDayID(now) != parseInt64(settings.Values["daily_day"]) ||
			serviceWeekID(now) != parseInt64(settings.Values["weekly_week"]) || daily == 0 || weekly == 0 {
			return ErrReceiptMismatch
		}
		totalLottoPoints, err := storedUint32(settings.Values, "lotto_points")
		if err != nil || totalLottoPoints < stored.LottoPointEarned {
			return ErrReceiptMismatch
		}
		totalLottoGold, err := storedInt64(settings.Values, "lotto_use_gold")
		if err != nil || totalLottoGold < stored.LottoGoldSpent {
			return ErrReceiptMismatch
		}
		character = dnfrepo.CloneCharacter(character)
		if character.Stats == nil || stored.Fee < 0 || character.Stats["gold"] > math.MaxInt64-stored.Fee {
			return ErrReceiptMismatch
		}
		daily--
		weekly--
		totalLottoPoints -= stored.LottoPointEarned
		totalLottoGold -= stored.LottoGoldSpent
		character.Stats["gold"] += stored.Fee
		settings.Values[endlessDailyCountKey] = strconv.FormatUint(uint64(daily), 10)
		settings.Values["weekly_count"] = strconv.FormatUint(uint64(weekly), 10)
		settings.Values["lotto_points"] = strconv.FormatUint(uint64(totalLottoPoints), 10)
		settings.Values["lotto_use_gold"] = strconv.FormatInt(totalLottoGold, 10)
		stored.GoldAfter = character.Stats["gold"]
		stored.DailyEnterCount = daily
		stored.WeeklyEnterCount = weekly
		stored.TotalLottoPoints = totalLottoPoints
		stored.TotalLottoGold = totalLottoGold
		settings.Values[key] = formatAdmissionReceipt(stored, receiptRestored)
		settings.UpdatedAt = now
		if err := dnfrepo.SaveCharacterFields(ctx, tx.Character, character, dnfrepo.CharacterFieldStats); err != nil {
			return err
		}
		if err := dnfrepo.SaveSettingsFields(ctx, tx.Settings, settings, dnfrepo.SettingsFieldValues); err != nil {
			return err
		}
		stored.CharacterID = characterID
		result = stored
		return nil
	})
	return result, err
}

// ReserveUltimate consumes one independent daily Entry for the Ultimate Altar.
// Unlike the Endless Altar, Ultimate Altar admission has no gold fee, fee
// escalation or lotto-point side effects.
func (o *Owner) ReserveUltimate(
	ctx context.Context,
	characterID string,
	dailyLimit uint32,
	receipt string,
	now time.Time,
) (AdmissionSnapshot, error) {
	if o == nil || o.repositories.CharacterSettlement == nil {
		return AdmissionSnapshot{}, ErrOwnerUnavailable
	}
	characterID = strings.TrimSpace(characterID)
	receipt = strings.TrimSpace(receipt)
	if characterID == "" || receipt == "" || dailyLimit == 0 {
		return AdmissionSnapshot{}, ErrCommandInvalid
	}
	if now.IsZero() {
		now = time.Now()
	}
	now = now.UTC()
	ctx = contextOrBackground(ctx)
	var result AdmissionSnapshot
	err := o.repositories.CharacterSettlement.WithinCharacterSettlement(ctx, characterID, func(tx dnfrepo.Group) error {
		character, found, err := tx.Character.Load(ctx, characterID)
		if err != nil {
			return err
		}
		if !found {
			return ErrCharacterMissing
		}
		settings, found, err := tx.Settings.Load(ctx, SettingsScope(characterID))
		if err != nil {
			return err
		}
		settings = dnfrepo.CloneSettings(settings)
		if !found {
			settings.Scope = SettingsScope(characterID)
		}
		if settings.Values == nil {
			settings.Values = make(map[string]string)
		}
		resetAdmissionWindows(settings.Values, now)
		receiptKey := admissionReceiptKey(receipt)
		if stored := strings.TrimSpace(settings.Values[receiptKey]); stored != "" {
			replayed, state, parseErr := parseAdmissionReceipt(stored)
			if parseErr != nil {
				return parseErr
			}
			if state == receiptRestored {
				return ErrReceiptRestored
			}
			replayed.CharacterID = characterID
			replayed.Replayed = true
			result = replayed
			return nil
		}
		daily, err := storedUint32(settings.Values, ultimateDailyCountKey)
		if err != nil {
			return err
		}
		if daily >= dailyLimit {
			return fmt.Errorf("%w: kind=ultimate used=%d limit=%d", ErrDailyLimit, daily, dailyLimit)
		}
		daily++
		settings.Values[ultimateDailyCountKey] = strconv.FormatUint(uint64(daily), 10)
		result = AdmissionSnapshot{
			CharacterID:     characterID,
			GoldAfter:       character.Stats["gold"],
			DailyEnterCount: daily,
		}
		settings.Values[receiptKey] = formatAdmissionReceipt(result, receiptActive)
		settings.UpdatedAt = now
		return dnfrepo.SaveSettingsFields(ctx, tx.Settings, settings, dnfrepo.SettingsFieldValues)
	})
	return result, err
}

func (o *Owner) RestoreUltimate(
	ctx context.Context,
	characterID string,
	receipt string,
	now time.Time,
) (AdmissionSnapshot, error) {
	if o == nil || o.repositories.CharacterSettlement == nil {
		return AdmissionSnapshot{}, ErrOwnerUnavailable
	}
	characterID = strings.TrimSpace(characterID)
	receipt = strings.TrimSpace(receipt)
	if characterID == "" || receipt == "" {
		return AdmissionSnapshot{}, ErrCommandInvalid
	}
	if now.IsZero() {
		now = time.Now()
	}
	now = now.UTC()
	ctx = contextOrBackground(ctx)
	var result AdmissionSnapshot
	err := o.repositories.CharacterSettlement.WithinCharacterSettlement(ctx, characterID, func(tx dnfrepo.Group) error {
		character, found, err := tx.Character.Load(ctx, characterID)
		if err != nil {
			return err
		}
		if !found {
			return ErrCharacterMissing
		}
		settings, found, err := tx.Settings.Load(ctx, SettingsScope(characterID))
		if err != nil {
			return err
		}
		if !found || settings.Values == nil {
			return ErrReceiptMismatch
		}
		settings = dnfrepo.CloneSettings(settings)
		key := admissionReceiptKey(receipt)
		stored, state, err := parseAdmissionReceipt(settings.Values[key])
		if err != nil {
			return err
		}
		if state == receiptRestored {
			stored.CharacterID = characterID
			stored.Replayed = true
			result = stored
			return nil
		}
		daily, err := storedUint32(settings.Values, ultimateDailyCountKey)
		if err != nil {
			return err
		}
		if premium.ServiceDayID(now) != parseInt64(settings.Values["daily_day"]) || daily == 0 {
			return ErrReceiptMismatch
		}
		daily--
		settings.Values[ultimateDailyCountKey] = strconv.FormatUint(uint64(daily), 10)
		stored.GoldAfter = character.Stats["gold"]
		stored.DailyEnterCount = daily
		settings.Values[key] = formatAdmissionReceipt(stored, receiptRestored)
		settings.UpdatedAt = now
		if err := dnfrepo.SaveSettingsFields(ctx, tx.Settings, settings, dnfrepo.SettingsFieldValues); err != nil {
			return err
		}
		stored.CharacterID = characterID
		result = stored
		return nil
	})
	return result, err
}

func (o *Owner) UpdateBestRecord(
	ctx context.Context,
	characterID string,
	dungeonID uint32,
	round uint32,
	playTimeMillis uint32,
	receipt string,
	now time.Time,
) (RecordSnapshot, error) {
	if o == nil || o.repositories.CharacterSettlement == nil {
		return RecordSnapshot{}, ErrOwnerUnavailable
	}
	characterID = strings.TrimSpace(characterID)
	receipt = strings.TrimSpace(receipt)
	if characterID == "" || dungeonID == 0 || round == 0 || round > 15 || receipt == "" {
		return RecordSnapshot{}, fmt.Errorf("%w: invalid record input", ErrCommandInvalid)
	}
	if now.IsZero() {
		now = time.Now()
	}
	ctx = contextOrBackground(ctx)
	var result RecordSnapshot
	err := o.repositories.CharacterSettlement.WithinCharacterSettlement(ctx, characterID, func(tx dnfrepo.Group) error {
		if _, found, err := tx.Character.Load(ctx, characterID); err != nil {
			return err
		} else if !found {
			return ErrCharacterMissing
		}
		settings, found, err := tx.Settings.Load(ctx, SettingsScope(characterID))
		if err != nil {
			return err
		}
		settings = dnfrepo.CloneSettings(settings)
		if !found {
			settings.Scope = SettingsScope(characterID)
		}
		if settings.Values == nil {
			settings.Values = make(map[string]string)
		}
		receiptKey := recordReceiptKey(dungeonID, receipt)
		if stored := strings.TrimSpace(settings.Values[receiptKey]); stored != "" {
			parsed, err := parseRecord(stored, dungeonID)
			if err != nil {
				return err
			}
			parsed.Replayed = true
			result = parsed
			return nil
		}
		bestRound, err := storedUint32(settings.Values, recordRoundKey(dungeonID))
		if err != nil {
			return err
		}
		bestTime, err := storedUint32(settings.Values, recordTimeKey(dungeonID))
		if err != nil {
			return err
		}
		if bestRound > 15 || (bestRound == 0 && bestTime != 0) {
			return ErrReceiptMismatch
		}
		changed := round > bestRound || (round == bestRound && (bestTime == 0 || playTimeMillis < bestTime))
		if changed {
			bestRound, bestTime = round, playTimeMillis
			settings.Values[recordRoundKey(dungeonID)] = strconv.FormatUint(uint64(bestRound), 10)
			settings.Values[recordTimeKey(dungeonID)] = strconv.FormatUint(uint64(bestTime), 10)
		}
		result = RecordSnapshot{DungeonID: dungeonID, BestRound: bestRound, BestTimeMillis: bestTime, Changed: changed}
		settings.Values[receiptKey] = formatRecord(result)
		settings.UpdatedAt = now.UTC()
		return dnfrepo.SaveSettingsFields(ctx, tx.Settings, settings, dnfrepo.SettingsFieldValues)
	})
	return result, err
}

func SettingsScope(characterID string) string {
	return "character:" + strings.TrimSpace(characterID) + settingsSuffix
}

func admissionFee(rule AdmissionRule, level uint16, enterCount uint32) (int64, error) {
	price, err := admissionBasePrice(rule, level)
	if err != nil {
		return 0, err
	}
	percent := rule.AdmissionIncrease[len(rule.AdmissionIncrease)-1].Percent
	for _, row := range rule.AdmissionIncrease {
		if enterCount < row.UpperExclusiveEnterCount {
			percent = row.Percent
			break
		}
	}
	if price > math.MaxInt64/(100+percent) {
		return 0, ErrCommandInvalid
	}
	return price + price*percent/100 + rule.LottoCost, nil
}

func admissionBasePrice(rule AdmissionRule, level uint16) (int64, error) {
	for _, band := range rule.PriceBands {
		if level >= band.MinimumLevel && level <= band.MaximumLevel {
			return band.Price, nil
		}
	}
	return 0, fmt.Errorf("%w: level=%d has no price band", ErrCommandInvalid, level)
}

func admissionLottoPoint(rule AdmissionRule, enterCount uint32) uint32 {
	points := rule.LottoPoints[len(rule.LottoPoints)-1].Points
	for _, row := range rule.LottoPoints {
		if enterCount < row.UpperExclusiveEnterCount {
			return row.Points
		}
	}
	return points
}

func validateAdmissionRule(rule AdmissionRule) error {
	if len(rule.PriceBands) == 0 || len(rule.AdmissionIncrease) == 0 || len(rule.LottoPoints) == 0 || rule.LottoCost < 0 {
		return ErrCommandInvalid
	}
	var previousMaximum uint16
	for index, band := range rule.PriceBands {
		if band.MinimumLevel == 0 || band.MaximumLevel < band.MinimumLevel || band.Price < 0 ||
			(index > 0 && band.MinimumLevel != previousMaximum+1) {
			return ErrCommandInvalid
		}
		previousMaximum = band.MaximumLevel
	}
	var previousThreshold uint32
	for _, row := range rule.AdmissionIncrease {
		if row.UpperExclusiveEnterCount == 0 || row.UpperExclusiveEnterCount <= previousThreshold || row.Percent < 0 {
			return ErrCommandInvalid
		}
		previousThreshold = row.UpperExclusiveEnterCount
	}
	previousThreshold = 0
	for _, row := range rule.LottoPoints {
		if row.UpperExclusiveEnterCount == 0 || row.UpperExclusiveEnterCount <= previousThreshold {
			return ErrCommandInvalid
		}
		previousThreshold = row.UpperExclusiveEnterCount
	}
	return nil
}

func resetAdmissionWindows(values map[string]string, now time.Time) {
	day := premium.ServiceDayID(now)
	if parseInt64(values["daily_day"]) != day {
		values["daily_day"] = strconv.FormatInt(day, 10)
		values[endlessDailyCountKey] = "0"
		values["ultimate_daily_count"] = "0"
	}
	week := serviceWeekID(now)
	if parseInt64(values["weekly_week"]) != week {
		values["weekly_week"] = strconv.FormatInt(week, 10)
		values["weekly_count"] = "0"
		values["lotto_points"] = "0"
		values["lotto_use_gold"] = "0"
	}
}

func serviceWeekID(now time.Time) int64 {
	day := premium.ServiceDayID(now)
	// Unix service day 0 is Thursday. Shift by three so Monday 06:00 begins a week.
	return (day + 3) / 7
}

func admissionReceiptKey(receipt string) string { return "admission_receipt:" + digest(receipt) }
func recordReceiptKey(dungeonID uint32, receipt string) string {
	return "record_receipt:" + strconv.FormatUint(uint64(dungeonID), 10) + ":" + digest(receipt)
}
func recordRoundKey(dungeonID uint32) string {
	return "record:" + strconv.FormatUint(uint64(dungeonID), 10) + ":round"
}
func recordTimeKey(dungeonID uint32) string {
	return "record:" + strconv.FormatUint(uint64(dungeonID), 10) + ":time"
}

func digest(value string) string {
	sum := sha256.Sum256([]byte(strings.TrimSpace(value)))
	return hex.EncodeToString(sum[:16])
}

func formatAdmissionReceipt(value AdmissionSnapshot, state string) string {
	return fmt.Sprintf("%s:%d:%d:%d:%d:%d:%d:%d:%d", state, value.Fee, value.GoldAfter,
		value.DailyEnterCount, value.WeeklyEnterCount, value.LottoPointEarned, value.LottoGoldSpent,
		value.TotalLottoPoints, value.TotalLottoGold)
}

func parseAdmissionReceipt(value string) (AdmissionSnapshot, string, error) {
	parts := strings.Split(strings.TrimSpace(value), ":")
	if len(parts) != 9 || (parts[0] != receiptActive && parts[0] != receiptRestored) {
		return AdmissionSnapshot{}, "", ErrReceiptMismatch
	}
	fee, err1 := strconv.ParseInt(parts[1], 10, 64)
	gold, err2 := strconv.ParseInt(parts[2], 10, 64)
	daily, err3 := strconv.ParseUint(parts[3], 10, 32)
	weekly, err4 := strconv.ParseUint(parts[4], 10, 32)
	earned, err5 := strconv.ParseUint(parts[5], 10, 32)
	lottoGold, err6 := strconv.ParseInt(parts[6], 10, 64)
	totalPoints, err7 := strconv.ParseUint(parts[7], 10, 32)
	totalGold, err8 := strconv.ParseInt(parts[8], 10, 64)
	if err1 != nil || err2 != nil || err3 != nil || err4 != nil || err5 != nil || err6 != nil ||
		err7 != nil || err8 != nil || fee < 0 || gold < 0 || lottoGold < 0 || totalGold < 0 ||
		(parts[0] == receiptActive && (uint32(earned) > uint32(totalPoints) || lottoGold > totalGold)) {
		return AdmissionSnapshot{}, "", ErrReceiptMismatch
	}
	return AdmissionSnapshot{
		Fee: fee, GoldAfter: gold, DailyEnterCount: uint32(daily), WeeklyEnterCount: uint32(weekly),
		LottoPointEarned: uint32(earned), LottoGoldSpent: lottoGold,
		TotalLottoPoints: uint32(totalPoints), TotalLottoGold: totalGold,
	}, parts[0], nil
}

func formatRecord(value RecordSnapshot) string {
	changed := 0
	if value.Changed {
		changed = 1
	}
	return fmt.Sprintf("%d:%d:%d:%d", value.DungeonID, value.BestRound, value.BestTimeMillis, changed)
}

func parseRecord(value string, dungeonID uint32) (RecordSnapshot, error) {
	parts := strings.Split(strings.TrimSpace(value), ":")
	if len(parts) != 4 {
		return RecordSnapshot{}, ErrReceiptMismatch
	}
	id, e1 := strconv.ParseUint(parts[0], 10, 32)
	round, e2 := strconv.ParseUint(parts[1], 10, 32)
	timeValue, e3 := strconv.ParseUint(parts[2], 10, 32)
	changed, e4 := strconv.ParseUint(parts[3], 10, 1)
	if e1 != nil || e2 != nil || e3 != nil || e4 != nil || uint32(id) != dungeonID || round == 0 || round > 15 {
		return RecordSnapshot{}, ErrReceiptMismatch
	}
	return RecordSnapshot{DungeonID: dungeonID, BestRound: uint32(round), BestTimeMillis: uint32(timeValue), Changed: changed == 1}, nil
}

func storedUint32(values map[string]string, key string) (uint32, error) {
	value := strings.TrimSpace(values[key])
	if value == "" {
		return 0, nil
	}
	parsed, err := strconv.ParseUint(value, 10, 32)
	if err != nil {
		return 0, ErrReceiptMismatch
	}
	return uint32(parsed), nil
}

func storedInt64(values map[string]string, key string) (int64, error) {
	value := strings.TrimSpace(values[key])
	if value == "" {
		return 0, nil
	}
	parsed, err := strconv.ParseInt(value, 10, 64)
	if err != nil || parsed < 0 {
		return 0, ErrReceiptMismatch
	}
	return parsed, nil
}

func parseInt64(value string) int64 {
	parsed, err := strconv.ParseInt(strings.TrimSpace(value), 10, 64)
	if err != nil {
		return 0
	}
	return parsed
}

func contextOrBackground(ctx context.Context) context.Context {
	if ctx == nil {
		return context.Background()
	}
	return ctx
}
