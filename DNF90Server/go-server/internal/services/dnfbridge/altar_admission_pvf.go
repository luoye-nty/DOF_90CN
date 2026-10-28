package dnfbridge

import (
	"fmt"

	dnfaltar "longheng.io/server/internal/modules/dnf/dungeonaltar"
	dnfpvf "longheng.io/server/internal/modules/dnf/pvf"
)

const currentDungeonAltarAdmissionDefinitionPath = "etc/serverparameter.etc"

func loadCurrentDungeonAltarAdmissionRule(source dnfpvf.Source) (dnfaltar.AdmissionRule, error) {
	document, err := parseDungeonCardPVFDocument(source, currentDungeonAltarAdmissionDefinitionPath)
	if err != nil {
		return dnfaltar.AdmissionRule{}, err
	}
	prices, err := currentDungeonAltarPVFInts(document, "price average")
	if err != nil || len(prices) == 0 || len(prices)%3 != 0 {
		return dnfaltar.AdmissionRule{}, currentDungeonAltarAdmissionShapeError("price average", len(prices), 3, err)
	}
	increases, err := currentDungeonAltarPVFInts(document, "admission increase")
	if err != nil || len(increases) == 0 || len(increases)%2 != 0 {
		return dnfaltar.AdmissionRule{}, currentDungeonAltarAdmissionShapeError("admission increase", len(increases), 2, err)
	}
	lottoPoints, err := currentDungeonAltarPVFInts(document, "lotto point")
	if err != nil || len(lottoPoints) == 0 || len(lottoPoints)%2 != 0 {
		return dnfaltar.AdmissionRule{}, currentDungeonAltarAdmissionShapeError("lotto point", len(lottoPoints), 2, err)
	}
	lottoCost, err := currentDungeonAltarPVFInts(document, "lotto cost")
	if err != nil || len(lottoCost) != 1 {
		return dnfaltar.AdmissionRule{}, currentDungeonAltarAdmissionShapeError("lotto cost", len(lottoCost), 1, err)
	}
	rule := dnfaltar.AdmissionRule{
		PriceBands:        make([]dnfaltar.PriceBand, 0, len(prices)/3),
		AdmissionIncrease: make([]dnfaltar.AdmissionIncrease, 0, len(increases)/2),
		LottoPoints:       make([]dnfaltar.LottoPoint, 0, len(lottoPoints)/2),
		LottoCost:         lottoCost[0],
	}
	for offset := 0; offset < len(prices); offset += 3 {
		minimum, maximum, price := prices[offset], prices[offset+1], prices[offset+2]
		if minimum <= 0 || maximum < minimum || maximum > int64(^uint16(0)) || price < 0 {
			return dnfaltar.AdmissionRule{}, fmt.Errorf("invalid altar price band: %d %d %d", minimum, maximum, price)
		}
		rule.PriceBands = append(rule.PriceBands, dnfaltar.PriceBand{
			MinimumLevel: uint16(minimum), MaximumLevel: uint16(maximum), Price: price,
		})
	}
	for offset := 0; offset < len(increases); offset += 2 {
		threshold, percent := increases[offset], increases[offset+1]
		if threshold <= 0 || threshold > int64(^uint32(0)) || percent < 0 {
			return dnfaltar.AdmissionRule{}, fmt.Errorf("invalid altar admission increase: %d %d", threshold, percent)
		}
		rule.AdmissionIncrease = append(rule.AdmissionIncrease, dnfaltar.AdmissionIncrease{
			UpperExclusiveEnterCount: uint32(threshold), Percent: percent,
		})
	}
	for offset := 0; offset < len(lottoPoints); offset += 2 {
		threshold, points := lottoPoints[offset], lottoPoints[offset+1]
		if threshold <= 0 || threshold > int64(^uint32(0)) || points < 0 || points > int64(^uint32(0)) {
			return dnfaltar.AdmissionRule{}, fmt.Errorf("invalid altar lotto point: %d %d", threshold, points)
		}
		rule.LottoPoints = append(rule.LottoPoints, dnfaltar.LottoPoint{
			UpperExclusiveEnterCount: uint32(threshold), Points: uint32(points),
		})
	}
	return rule, nil
}

func currentDungeonAltarAdmissionShapeError(section string, count, width int, cause error) error {
	if cause != nil {
		return fmt.Errorf("invalid altar %s table: %w", section, cause)
	}
	return fmt.Errorf("invalid altar %s table: values=%d width=%d", section, count, width)
}
