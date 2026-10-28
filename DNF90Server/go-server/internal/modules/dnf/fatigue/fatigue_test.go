package fatigue

import "testing"

func TestConsumeClampsAtZero(t *testing.T) {
	tests := []struct {
		name         string
		remaining    int64
		amount       int64
		wantNext     int64
		wantConsumed int64
	}{
		{name: "room", remaining: 100, amount: 1, wantNext: 99, wantConsumed: 1},
		{name: "last point", remaining: 1, amount: 1, wantNext: 0, wantConsumed: 1},
		{name: "exhausted", remaining: 0, amount: 1, wantNext: 0, wantConsumed: 0},
		{name: "negative persisted value", remaining: -1, amount: 1, wantNext: 0, wantConsumed: 0},
		{name: "non-positive request", remaining: 10, amount: 0, wantNext: 10, wantConsumed: 0},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			next, consumed := Consume(test.remaining, test.amount)
			if next != test.wantNext || consumed != test.wantConsumed {
				t.Fatalf("Consume(%d, %d) = %d, %d; want %d, %d", test.remaining, test.amount, next, consumed, test.wantNext, test.wantConsumed)
			}
		})
	}
}

func TestReconcileBlackDiamondLimitPreservesConsumedFatigue(t *testing.T) {
	remaining, limit, changed := ReconcileLimit(120, DefaultLimit, BlackDiamondLimit)
	if !changed || remaining != 152 || limit != BlackDiamondLimit {
		t.Fatalf("activation = %d/%d changed=%t, want 152/188", remaining, limit, changed)
	}
	remaining, limit, changed = ReconcileLimit(remaining, limit, DefaultLimit)
	if !changed || remaining != 120 || limit != DefaultLimit {
		t.Fatalf("expiry = %d/%d changed=%t, want 120/156", remaining, limit, changed)
	}
}

func TestDesiredBlackDiamondLimitPreservesCustomLimit(t *testing.T) {
	if got := DesiredBlackDiamondLimit(200, true); got != 200 {
		t.Fatalf("custom active limit=%d, want 200", got)
	}
	if got := DesiredBlackDiamondLimit(200, false); got != 200 {
		t.Fatalf("custom inactive limit=%d, want 200", got)
	}
}

func TestReconcileLimitPersistsNormalizedNativeState(t *testing.T) {
	remaining, limit, changed := ReconcileLimit(999, 0, DefaultLimit)
	if !changed || remaining != DefaultLimit || limit != DefaultLimit {
		t.Fatalf("normalized state=%d/%d changed=%t, want 156/156 changed", remaining, limit, changed)
	}
}
