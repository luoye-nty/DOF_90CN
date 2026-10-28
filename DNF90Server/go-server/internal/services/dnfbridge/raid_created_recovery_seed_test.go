package dnfbridge

import (
	"encoding/base64"
	"testing"
)

func TestRuntimeRaidCreatedRecoverySeedRestoresExactUnstartedObject(t *testing.T) {
	service := &Service{
		localDebugMode: true,
		options: options{raidCreatedRecoverySeed: base64.StdEncoding.EncodeToString([]byte(
			`{"raid_key":1507329,"leader_id":1,"route_or_raid_type":0,"name_bytes":"ZmZm","tail_flag":1,"single_raid_type":0}`,
		))},
		raids: make(map[uint32]*runtimeRaidState),
	}
	if err := service.restoreRuntimeCreatedRaidSeed(); err != nil {
		t.Fatalf("restore created raid seed: %v", err)
	}
	state, ok := service.runtimeRaidForMember(1)
	if !ok || state.RaidKey != 0x00170001 || state.LeaderID != 1 || string(state.NameBytes) != "fff" ||
		state.TailFlag != 1 || state.SingleRaidType != 0 || state.Started || state.StartPending || state.EntryCostPaid ||
		state.Lifecycle.Status != runtimeRaidLifecycleCreated || len(state.Members) != 1 || state.Members[0].CharID != 1 {
		t.Fatalf("restored wrong created raid state: ok=%t state=%+v", ok, state)
	}
}

func TestRuntimeRaidCreatedRecoverySeedRequiresLocalDebugAndExactCreatedShape(t *testing.T) {
	valid := base64.StdEncoding.EncodeToString([]byte(
		`{"raid_key":1507329,"leader_id":1,"route_or_raid_type":0,"name_bytes":"ZmZm","tail_flag":1,"single_raid_type":0}`,
	))
	service := &Service{options: options{raidCreatedRecoverySeed: valid}, raids: make(map[uint32]*runtimeRaidState)}
	if err := service.restoreRuntimeCreatedRaidSeed(); err == nil {
		t.Fatal("non-local created raid recovery seed was accepted")
	}
	service.localDebugMode = true
	service.options.raidCreatedRecoverySeed = base64.StdEncoding.EncodeToString([]byte(
		`{"raid_key":1507329,"leader_id":1,"route_or_raid_type":0,"name_bytes":"ZmZm","tail_flag":1,"single_raid_type":1}`,
	))
	if err := service.restoreRuntimeCreatedRaidSeed(); err == nil {
		t.Fatal("started/specialized recovery shape was accepted")
	}
}
