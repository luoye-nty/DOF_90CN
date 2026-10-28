package premium

import (
	"testing"
	"time"

	dnfrepo "longheng.io/server/internal/modules/dnf/repository"
)

// TestActivateContractUnitsExtendsEntitlementWithoutInventoryRows pins the
// reward-path contract: a premium contract that a box rolls is turned into
// account time directly, so it never needs a bag cell (and therefore cannot
// push an ordinary reward into overflow mail). The result must still look like
// an activation so the bridge can notify the client.
func TestActivateContractUnitsExtendsEntitlementWithoutInventoryRows(t *testing.T) {
	now := time.Unix(1_700_000_000, 0).UTC()
	account := dnfrepo.AccountRecord{AccountID: "acct", Metadata: map[string]string{}}
	contracts := map[int64]InventoryContract{
		66001: {ItemID: 66001, PremiumType: 22, DurationSeconds: 86400},
	}
	result, err := ActivateContractUnits(&account, contracts, map[int64]int64{66001: 3}, now)
	if err != nil {
		t.Fatal(err)
	}
	if !result.Changed() || len(result.RemovedSlots) != 0 {
		t.Fatalf("result=%+v want activations without removed slots", result)
	}
	if len(result.Activations) != 1 {
		t.Fatalf("activations=%+v", result.Activations)
	}
	activation := result.Activations[0]
	if activation.PremiumType != 22 || activation.Units != 3 || activation.DurationSeconds != 3*86400 {
		t.Fatalf("activation=%+v", activation)
	}
	if expire := ExpireAt(account, 22); expire != now.Unix()+3*86400 {
		t.Fatalf("expire=%d want=%d", expire, now.Unix()+3*86400)
	}

	// Unknown ids and zero counts change nothing.
	empty, err := ActivateContractUnits(&account, contracts, map[int64]int64{999999: 5, 66001: 0}, now)
	if err != nil {
		t.Fatal(err)
	}
	if empty.Changed() {
		t.Fatalf("unexpected activation=%+v", empty)
	}
}
