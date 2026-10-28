package dnfbridge

import (
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
)

var errRuntimeRaidCreatedRecoverySeed = errors.New("runtime created raid recovery seed is invalid")

// runtimeRaidCreatedRecoverySeed is a local-only deployment migration for an
// exact unstarted object that is still owned by the process being replaced.
// It is deliberately unable to describe a started raid or lifecycle progress;
// normal reconnects restore those from the live server-owned runtime state.
type runtimeRaidCreatedRecoverySeed struct {
	RaidKey         uint32 `json:"raid_key"`
	LeaderID        uint16 `json:"leader_id"`
	RouteOrRaidType byte   `json:"route_or_raid_type"`
	NameBytes       []byte `json:"name_bytes"`
	TailFlag        byte   `json:"tail_flag"`
	SingleRaidType  byte   `json:"single_raid_type"`
}

func (s *Service) restoreRuntimeCreatedRaidSeed() error {
	if s == nil || s.options.raidCreatedRecoverySeed == "" {
		return nil
	}
	if !s.localDebugMode {
		return fmt.Errorf("%w: local debug mode is required", errRuntimeRaidCreatedRecoverySeed)
	}
	raw, err := base64.StdEncoding.DecodeString(s.options.raidCreatedRecoverySeed)
	if err != nil {
		return fmt.Errorf("%w: decode base64: %v", errRuntimeRaidCreatedRecoverySeed, err)
	}
	var seed runtimeRaidCreatedRecoverySeed
	if err := json.Unmarshal(raw, &seed); err != nil {
		return fmt.Errorf("%w: decode json: %v", errRuntimeRaidCreatedRecoverySeed, err)
	}
	if seed.RaidKey == 0 || seed.LeaderID == 0 || uint16(seed.RaidKey) != seed.LeaderID ||
		seed.RaidKey>>16 == 0 || len(seed.NameBytes) == 0 || len(seed.NameBytes) > 0xff ||
		seed.TailFlag > 1 || seed.SingleRaidType != 0 {
		return fmt.Errorf("%w: unsupported created object", errRuntimeRaidCreatedRecoverySeed)
	}

	s.raidMu.Lock()
	defer s.raidMu.Unlock()
	s.ensureRuntimeRaidMapsLocked()
	if len(s.raids) != 0 || s.runtimeRaidByMemberLocked(seed.LeaderID) != nil {
		return fmt.Errorf("%w: runtime raid state is not empty", errRuntimeRaidCreatedRecoverySeed)
	}
	member := s.newRuntimeRaidMemberLocked(seed.LeaderID, runtimeRaidDefaultGroup, 0)
	state := &runtimeRaidState{
		RaidKey:         seed.RaidKey,
		LeaderID:        seed.LeaderID,
		RouteOrRaidType: seed.RouteOrRaidType,
		NameBytes:       append([]byte(nil), seed.NameBytes...),
		TailFlag:        seed.TailFlag,
		SingleRaidType:  seed.SingleRaidType,
		Members:         []runtimeRaidMemberState{member},
		Lifecycle: runtimeRaidLifecycleState{
			Status: runtimeRaidLifecycleCreated,
		},
	}
	s.raids[state.RaidKey] = state
	s.logInfo("dnfbridge local created raid recovery seed restored",
		"raid_key", state.RaidKey,
		"leader_id", state.LeaderID,
		"member_count", len(state.Members),
		"route_or_raid_type", state.RouteOrRaidType,
		"tail_flag", state.TailFlag,
		"single_raid_type", state.SingleRaidType,
		"source", "operator_supplied_current_process_packet_evidence")
	return nil
}
