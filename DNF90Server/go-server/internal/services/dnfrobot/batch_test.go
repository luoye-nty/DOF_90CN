package dnfrobot

import (
	"context"
	"testing"

	dnfrobotdomain "longheng.io/server/internal/modules/dnf/robot"
)

func TestManagerBatchProfileIDsAreStableAndDeduplicated(t *testing.T) {
	manager := &Manager{config: dnfrobotdomain.Config{Profiles: []dnfrobotdomain.Profile{{ID: "Alpha"}, {ID: "beta"}}}}
	all := manager.batchProfileIDs(nil)
	if len(all) != 2 || all[0] != "Alpha" || all[1] != "beta" {
		t.Fatalf("all ids = %v", all)
	}
	selected := manager.batchProfileIDs([]string{" alpha ", "ALPHA", "beta", ""})
	if len(selected) != 2 || selected[0] != "alpha" || selected[1] != "beta" {
		t.Fatalf("selected ids = %v", selected)
	}
}

func TestManagerBatchRejectsUnknownActionBeforeMutation(t *testing.T) {
	manager := &Manager{}
	if _, err := manager.Batch(context.Background(), BatchRequest{Action: "invented"}); err == nil {
		t.Fatal("Batch accepted an unknown action")
	}
}

func TestManagerStatusCountsCurrentProtocolStates(t *testing.T) {
	manager := &Manager{
		config: dnfrobotdomain.Config{Profiles: []dnfrobotdomain.Profile{{ID: "ready"}, {ID: "store"}, {ID: "offline"}}},
		running: map[string]*profileRuntime{
			"ready": {profile: dnfrobotdomain.Profile{ID: "ready"}, client: clientWithState(StateTownReady), reconnects: 2},
			"store": {profile: dnfrobotdomain.Profile{ID: "store"}, client: clientWithState(StateStoreReady)},
		},
	}
	status := manager.Status()
	if status.Configured != 3 || status.Running != 2 || status.TownReady != 1 || status.StoreReady != 1 || status.Reconnects != 2 {
		t.Fatalf("status = %+v", status)
	}
}

func clientWithState(state State) *Client {
	client := NewClient(dnfrobotdomain.Profile{}, "test", []byte("secret"))
	client.mu.Lock()
	client.state = state
	client.mu.Unlock()
	return client
}
