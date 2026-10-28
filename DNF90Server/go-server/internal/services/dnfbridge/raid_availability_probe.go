package dnfbridge

import (
	"os"
	"path/filepath"
)

// TEST AID -- local testing only; delete the marker file to go back to normal.
// Nothing has to be rebuilt or restarted either way (the marker is checked per
// send), and it rides the same runtimeRaidDebugStateDir() mechanism as
// raid_debug_phase_skip.go -- a file rather than an environment variable,
// because the control tool strips DNFBRIDGE_* out of the inherited environment.
//
//   - runtime/state/raid-availability-probe: the class0/op1303 three-row table
//     carries three sets of unmistakable, deliberately absurd numbers instead of
//     live counters.
//
// WHY: the five bytes of each op1303 row had only ever been *inferred*. The first
// live attempt (2026-09-30) proved the inference wrong in a way no amount of
// disassembly had settled: byte 4 was sent as "weekly used = 1" next to
// "weekly limit = 20" and the client rendered 帐号周入场场数 21/21 -- it ADDED the
// two, while the daily pair on the same row subtracts. Reading the client's own
// formula off a screenshot needs numbers whose every possible reading is
// distinct, which is exactly what this probe emits.
//
// SAFETY: display only. Raid-start enforcement reads the catalog policies
// (raid_admission_counter.go), never these five bytes, so a probe build cannot
// let anyone in or keep anyone out. The numbers are all >= the real limits, so
// nothing in the UI can read as "you are out of entries".
//
// READING THE RESULT -- the probe has been RUN (2026-09-30, server owner's
// screenshot) and it answered everything it was built for. Keeping it here so the
// next person does not have to re-derive the byte map. Per mode it sent
// (b1=Used, b2=Limit, b3=WeeklyBonus, b4=<inert, since removed>):
//
//	模式            b1   b2   b3
//	安徒恩 (mode 1)  11   110  13
//	卢克   (mode 2)  22   220  26
//	超时空 (mode 3)  33   230  39
//
// and the client rendered:
//
//	帐号日入场数  99/110 | 198/220 | 197/230   = (b2-b1)/b2   -> b1 used, b2 limit
//	帐号周入场场数 43/43  | 46/46   | 59/59     = (b3 + Lcfg) twice
//	角色周入场次数  3/3   |  2/2    |  2/2      -> UNCHANGED by the probe
//
// 43 = 13 + 30, 46 = 26 + 20, 59 = 39 + 20: b3 is ADDED to the client's own
// per-mode weekly limit (Lcfg = 30/20/20 from its own Script.pvf), and the same
// total is printed twice. So b3 is an extra-allowance/bonus byte, not a used
// count -- which is why the first live attempt, sending weekly used=1 next to
// limit=20, rendered 21/21 and looked like the counter was going UP.
//
// Two consequences that are now settled and must not be re-guessed:
//
//   - The account weekly USED count is NOT carried by op1303 at all. Nothing sent
//     in any of the five bytes changed the weekly readout's relationship to a
//     "remaining" number.
//   - Window B (角色周入场次数 and its 一..日 calendar) is NOT fed by op1303
//     either: its 3/2/2 stayed put while every other line moved. Those come from
//     the client's own Script.pvf. The packet that actually feeds the weekly
//     counters -- both the account one and window B's character one -- is still
//     unfound and is the remaining work for this feature.
const runtimeRaidAvailabilityProbeMarker = "raid-availability-probe"

// currentRaidAvailabilityProbeRows returns the probe table when the marker file
// is present, and ok=false otherwise. A missing marker (the normal case) is
// completely inert: the caller keeps its live rows.
func currentRaidAvailabilityProbeRows() ([]currentRaidDailyAvailability, bool) {
	dir := runtimeRaidDebugStateDir()
	if dir == "" {
		return nil, false
	}
	if _, err := os.Stat(filepath.Join(dir, runtimeRaidAvailabilityProbeMarker)); err != nil {
		return nil, false
	}
	return []currentRaidDailyAvailability{
		{Mode: currentRaidAntonMode, Used: 11, Limit: 110, WeeklyBonus: 13},
		{Mode: currentRaidLukeMode, Used: 22, Limit: 220, WeeklyBonus: 26},
		{Mode: currentRaidFiendWarMode, Used: 33, Limit: 230, WeeklyBonus: 39},
	}, true
}
