package dnfbridge

import (
	"testing"
	"time"

	dnfrepo "longheng.io/server/internal/modules/dnf/repository"
)

func TestDropExpiredStacksUsesClientWireExpiry(t *testing.T) {
	now := time.Unix(1000, 0)
	slots := map[string]dnfrepo.ItemStack{
		"future": {
			ExpireAt: time.Unix(2000, 0),
		},
		"extra-expired": {
			ExpireAt: time.Unix(2000, 0),
			Extra:    map[string]string{"expire_time": "900"},
		},
		"extra-future": {
			Extra: map[string]string{"expire_unix": "2000"},
		},
		"expireat-expired": {
			ExpireAt: time.Unix(900, 0),
		},
		"no-expiry": {},
	}

	if removed := dropExpiredStacks(slots, now); removed != 2 {
		t.Fatalf("removed = %d, want 2", removed)
	}
	for _, key := range []string{"future", "extra-future", "no-expiry"} {
		if _, found := slots[key]; !found {
			t.Fatalf("future/no-expiry row %q was removed", key)
		}
	}
	for _, key := range []string{"extra-expired", "expireat-expired"} {
		if _, found := slots[key]; found {
			t.Fatalf("expired row %q survived", key)
		}
	}
}
