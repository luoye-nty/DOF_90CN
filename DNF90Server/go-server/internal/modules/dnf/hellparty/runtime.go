package hellparty

import (
	"errors"
	"fmt"
)

var (
	ErrRuntimeInvalid = errors.New("dnf hell party runtime is invalid")
	ErrPhaseInvalid   = errors.New("dnf hell party phase transition is invalid")
	ErrActorUnknown   = errors.New("dnf hell party actor is unknown")
)

type Phase uint8

const (
	PhaseWaitingStart Phase = iota
	PhaseStarted
	PhaseComplete
)

type DeathResult struct {
	Duplicate        bool
	GroupCompleted   bool
	AllComplete      bool
	RewardRollCount  uint32
	RewardSuppressed bool
}

type Runtime struct {
	plan           Plan
	phase          Phase
	actors         map[ActorIdentity]PlannedActor
	killed         map[ActorIdentity]struct{}
	remaining      map[waveIdentity]int
	rewardReleased map[waveIdentity]struct{}
}

type waveIdentity struct {
	Order uint32
	Group uint32
}

func NewRuntime(plan Plan) (*Runtime, error) {
	if len(plan.Waves) == 0 {
		return nil, fmt.Errorf("%w: no waves", ErrRuntimeInvalid)
	}
	runtime := &Runtime{
		plan:           clonePlan(plan),
		phase:          PhaseWaitingStart,
		actors:         make(map[ActorIdentity]PlannedActor),
		killed:         make(map[ActorIdentity]struct{}),
		remaining:      make(map[waveIdentity]int),
		rewardReleased: make(map[waveIdentity]struct{}),
	}
	for _, wave := range plan.Waves {
		identity := waveIdentity{Order: wave.Order, Group: wave.GroupID}
		if _, exists := runtime.remaining[identity]; exists || len(wave.Actors) == 0 {
			return nil, fmt.Errorf("%w: duplicate or empty wave order=%d group=%d", ErrRuntimeInvalid, wave.Order, wave.GroupID)
		}
		runtime.remaining[identity] = len(wave.Actors)
		for _, actor := range wave.Actors {
			if _, exists := runtime.actors[actor.Identity]; exists {
				return nil, fmt.Errorf("%w: duplicate actor=%+v", ErrRuntimeInvalid, actor.Identity)
			}
			runtime.actors[actor.Identity] = actor
		}
	}
	return runtime, nil
}

func (r *Runtime) Start() error {
	if r == nil || r.phase != PhaseWaitingStart {
		return ErrPhaseInvalid
	}
	r.phase = PhaseStarted
	return nil
}

func (r *Runtime) RecordDeath(identity ActorIdentity) (DeathResult, error) {
	if r == nil {
		return DeathResult{}, ErrPhaseInvalid
	}
	actor, exists := r.actors[identity]
	if !exists {
		return DeathResult{}, ErrActorUnknown
	}
	if _, duplicate := r.killed[identity]; duplicate {
		return DeathResult{Duplicate: true, AllComplete: r.phase == PhaseComplete}, nil
	}
	if r.phase != PhaseStarted {
		return DeathResult{}, ErrPhaseInvalid
	}
	r.killed[identity] = struct{}{}
	waveID := waveIdentity{Order: identity.Order, Group: identity.Group}
	r.remaining[waveID]--
	result := DeathResult{}
	if r.remaining[waveID] == 0 {
		result.GroupCompleted = true
		if _, released := r.rewardReleased[waveID]; !released {
			r.rewardReleased[waveID] = struct{}{}
			result.RewardSuppressed = actor.SuppressGroupReward
			if !result.RewardSuppressed {
				result.RewardRollCount = r.waveRewardRollCount(waveID)
			}
		}
	}
	if len(r.killed) == len(r.actors) {
		r.phase = PhaseComplete
		result.AllComplete = true
	}
	return result, nil
}

func (r *Runtime) Phase() Phase {
	if r == nil {
		return PhaseWaitingStart
	}
	return r.phase
}

func (r *Runtime) Plan() Plan {
	if r == nil {
		return Plan{}
	}
	return clonePlan(r.plan)
}

func (r *Runtime) waveRewardRollCount(identity waveIdentity) uint32 {
	for _, wave := range r.plan.Waves {
		if wave.Order == identity.Order && wave.GroupID == identity.Group {
			return wave.RewardRollCount
		}
	}
	return 0
}

func clonePlan(plan Plan) Plan {
	plan.Waves = append([]Wave(nil), plan.Waves...)
	for index := range plan.Waves {
		plan.Waves[index].Actors = append([]PlannedActor(nil), plan.Waves[index].Actors...)
	}
	return plan
}
