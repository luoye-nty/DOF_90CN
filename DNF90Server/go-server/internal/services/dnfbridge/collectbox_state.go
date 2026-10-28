package dnfbridge

import (
	"context"
	"fmt"
	"strconv"

	dnfcollectbox "longheng.io/server/internal/modules/dnf/collectbox"
	dnfrepo "longheng.io/server/internal/modules/dnf/repository"
)

// loadCollectboxState keeps account metadata authoritative while preserving
// the character-settings state written by older deployments as a fallback.
func loadCollectboxState(
	ctx context.Context,
	repositories dnfrepo.Group,
	accountID string,
	characterID uint64,
) (dnfcollectbox.State, error) {
	if repositories.Account != nil {
		state, found, err := dnfcollectbox.LoadAccountState(ctx, repositories.Account, accountID)
		if err != nil {
			return dnfcollectbox.State{}, err
		}
		if found {
			return state, nil
		}
	}
	if repositories.Settings != nil && characterID != 0 {
		return dnfcollectbox.LoadState(ctx, repositories.Settings, strconv.FormatUint(characterID, 10))
	}
	return dnfcollectbox.State{}, fmt.Errorf("collect box state repositories unavailable")
}
