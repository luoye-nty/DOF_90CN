package dnfbridge

// currentEpicWishHit* scale the Epic wish preference branch.  The branch runs
// only AFTER the Hell table has already selected rarity 4, so it never changes
// the Epic rarity roll itself -- it only decides which Epic is handed out.
//
// The numerator is a product dial, not a PVF value.  Measured against the live
// runtime Script.pvf on 2026-09-23: etc/epicpieceinfo.etc holds 966 entries
// across six level buckets (65/70/75/80/85/90), so the level-90 band [85,90]
// carries roughly 264 candidates with a total creation-rate weight of roughly
// 30,900; one average Epic is therefore only ~0.38% of the band and the
// heaviest observed weight (250) is ~0.81%.
//
// Because the miss branch REMOVES every wished entry from the pool, a valid
// wish does not add to its item's natural share -- it REPLACES it with
// numerator/denominator/N (N = filled wish slots, 1..5).  The former 25/100
// therefore pinned a single wish at 25% of all Epic drops, about 66x its
// natural share.  2/100 keeps a single wish a 2.5x..20x gain while a fully
// filled five-slot list lands at 0.4% per slot, i.e. near -- and for the
// heavier entries slightly below -- the natural share.
const (
	currentEpicWishHitNumerator   = 2
	currentEpicWishHitDenominator = 100
)

func selectCurrentTimeRiftEpicCandidate(
	rng *currentDungeonDropLCG,
	pools map[int][]currentDungeonHellPartyFragmentCandidate,
	monsterLevel int,
	wishItems []uint32,
) (currentDungeonHellPartyFragmentCandidate, bool, bool) {
	if rng == nil {
		return currentDungeonHellPartyFragmentCandidate{}, false, false
	}
	candidates := make([]currentDungeonHellPartyFragmentCandidate, 0)
	byEquipment := make(map[uint32]currentDungeonHellPartyFragmentCandidate)
	for _, level := range currentHellPartyEpicLevels(monsterLevel) {
		for _, candidate := range pools[level] {
			candidates = append(candidates, candidate)
			if candidate.EquipmentID != 0 {
				byEquipment[candidate.EquipmentID] = candidate
			}
		}
	}
	if len(candidates) == 0 {
		return currentDungeonHellPartyFragmentCandidate{}, false, false
	}

	validWish := make([]currentDungeonHellPartyFragmentCandidate, 0, len(wishItems))
	wished := make(map[uint32]struct{}, len(wishItems))
	for _, itemID := range wishItems {
		candidate, found := byEquipment[itemID]
		if itemID == 0 || !found {
			continue
		}
		if _, duplicate := wished[itemID]; duplicate {
			continue
		}
		wished[itemID] = struct{}{}
		validWish = append(validWish, candidate)
	}
	if len(validWish) != 0 &&
		rng.Next(currentEpicWishHitDenominator) < currentEpicWishHitNumerator {
		return validWish[rng.Next(len(validWish))], true, true
	}

	normal := make([]currentDungeonHellPartyFragmentCandidate, 0, len(candidates))
	for _, candidate := range candidates {
		if _, excluded := wished[candidate.EquipmentID]; !excluded {
			normal = append(normal, candidate)
		}
	}
	if candidate, found := selectCurrentHellPartyFragmentCandidateFromPool(rng, normal); found {
		return candidate, true, false
	}

	// If every eligible Epic is wished, the archived miss branch had an empty
	// pool and silently erased an already-successful rarity-4 result. Preserve
	// that Epic roll by falling back to the complete weighted pool.
	candidate, found := selectCurrentHellPartyFragmentCandidateFromPool(rng, candidates)
	if !found {
		return currentDungeonHellPartyFragmentCandidate{}, false, false
	}
	return candidate, true, false
}
