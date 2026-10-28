package dnfbridge

import (
	"errors"
	"fmt"
	"testing"

	dnfcerashop "longheng.io/server/internal/modules/dnf/cerashop"
)

func TestCurrentCeraShopPurchaseFailureCodeForCommitError(t *testing.T) {
	tests := []struct {
		name string
		err  error
		want byte
	}{
		{
			name: "inventory full",
			err:  fmt.Errorf("grant package: %w", errDungeonPickupInventoryFull),
			want: currentCeraShopPurchaseFailureCodeInventoryFull,
		},
		{
			name: "insufficient cera",
			err:  fmt.Errorf("checkout: %w", dnfcerashop.ErrCeraInsufficient),
			want: currentCeraShopPurchaseFailureCodeInsufficientCera,
		},
		{
			name: "insufficient wallet",
			err:  fmt.Errorf("checkout: %w", dnfcerashop.ErrWalletInsufficient),
			want: currentCeraShopPurchaseFailureCodeInsufficientCera,
		},
		{
			name: "insufficient gold",
			err:  fmt.Errorf("checkout: %w", errCurrentCeraShopGoldInsufficient),
			want: currentCeraShopPurchaseFailureCodeInsufficientGold,
		},
		{
			name: "generic rejection",
			err:  errors.New("unclassified checkout failure"),
			want: currentCeraShopPurchaseFailureCodeCannotBuy,
		},
	}

	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			if got := currentCeraShopPurchaseFailureCodeForCommitError(test.err); got != test.want {
				t.Fatalf("code=%d want=%d", got, test.want)
			}
		})
	}
}
