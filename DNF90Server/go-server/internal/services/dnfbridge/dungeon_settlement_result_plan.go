package dnfbridge

import (
	"errors"
	"fmt"

	dnfrepo "longheng.io/server/internal/modules/dnf/repository"
)

var (
	errCurrentDungeonSettlementPlanShape = errors.New("current dungeon settlement packet plan shape is invalid")
	errCurrentDungeonSettlementPlanOwner = errors.New("current dungeon settlement packet plan owner mismatch")
)

// currentDungeonSettlementPacketPlan freezes the three current-EXE result
// packets only after the reward transaction has committed. ClientRankPoint
// binds a replay to its C2S op46 request and must be carried unchanged in the
// frozen op34 body; local debug rewards never alter the client result state.
type currentDungeonSettlementPacketPlan struct {
	CharacterID           uint16
	CompletionKey         string
	Source                string
	ClientRankPoint       byte
	ClearTimeMS           uint32
	PlayResultBody        []byte
	CharacterBody         []byte
	ClearRewardBody       []byte
	DungeonPermissionBody []byte
	// WeddingCapsuleBody 是"已婚角色通关推荐副本"时下发的 1016 号婚姻成长胶囊
	// 进度行（u32 progress）。为空表示本次结算与婚姻无关。
	WeddingCapsuleBody []byte
	// TowerRewardMsgID/TowerRewardBody carry the tower clear window: op348 for
	// the Tower of Despair, op1263 for the Tower of Grief. A tower never uses
	// the ordinary card reward path.
	TowerRewardMsgID uint16
	TowerRewardBody  []byte
	// clearRewardSnapshot is the committed reward snapshot this plan was built
	// from, kept so a later reader can inspect what the clear presented.
	clearRewardSnapshot *currentDungeonClearRewardSnapshot
}

func buildCurrentDungeonSettlementPacketPlan(
	clientRankPoint byte,
	notice currentDungeonPlayResultNotice,
	reward currentDungeonClearRewardSnapshot,
	character dnfrepo.CharacterRecord,
	points dnfrepo.SkillPointState,
) (currentDungeonSettlementPacketPlan, error) {
	return buildCurrentDungeonSettlementPacketPlanWithWelcome(
		clientRankPoint, notice, reward, character, points, nil,
	)
}

// buildCurrentDungeonSettlementPacketPlanWithWelcome 与上面同义，额外接收本次通关
// 已提交的"回归冒险家欢迎经验"分类值。
//
// 为什么需要它：op37 的 `finish_loading_exp_category_9` 按设计是从**角色 stats**
// 读的（`buildCurrentFinishLoadingCharacterStateBody` 在没有 presentation 时就走这条
// 路），所以通关链路必须把这个分类写到用于出包的角色副本上，否则结算面板里那一行
// 恒为 0——欢迎经验明明已经发给了玩家，结果窗却不显示。
// welcome == nil 表示本次通关没有欢迎加成（非回归玩家），此时写 0 覆盖掉任何陈旧值。
func buildCurrentDungeonSettlementPacketPlanWithWelcome(
	clientRankPoint byte,
	notice currentDungeonPlayResultNotice,
	reward currentDungeonClearRewardSnapshot,
	character dnfrepo.CharacterRecord,
	points dnfrepo.SkillPointState,
	welcome *uint32,
) (currentDungeonSettlementPacketPlan, error) {
	characterID := numericCharacterID(character)
	if characterID <= 0 || characterID > int(^uint16(0)) || uint16(characterID) != reward.CharacterID {
		return currentDungeonSettlementPacketPlan{}, fmt.Errorf(
			"%w: character=%d reward_character=%d",
			errCurrentDungeonSettlementPlanOwner,
			characterID,
			reward.CharacterID,
		)
	}
	if notice.RankPoint != clientRankPoint {
		return currentDungeonSettlementPacketPlan{}, fmt.Errorf(
			"%w: notice_client_rank=%d request_client_rank=%d",
			errCurrentDungeonSettlementPlanShape,
			notice.RankPoint,
			clientRankPoint,
		)
	}
	ownedParticipants := 0
	for _, participant := range notice.Participants {
		if participant.ObjectKey == reward.CharacterID {
			ownedParticipants++
		}
	}
	if ownedParticipants != 1 {
		return currentDungeonSettlementPacketPlan{}, fmt.Errorf(
			"%w: character=%d owned_participants=%d",
			errCurrentDungeonSettlementPlanOwner,
			reward.CharacterID,
			ownedParticipants,
		)
	}

	playResultBody, err := buildCurrentDungeonPlayResultNoticeBody(notice)
	if err != nil {
		return currentDungeonSettlementPacketPlan{}, err
	}
	clearRewardBody, err := buildCurrentDungeonClearRewardBody(reward)
	if err != nil {
		return currentDungeonSettlementPacketPlan{}, err
	}
	// 把本次通关已提交的欢迎经验分类写进出包用的角色副本。必须在拷贝之后再写，
	// 不能改调用方持有的记录（那份记录要落库，op37 的分类是**表现**不是持久状态）。
	planCharacter := dnfrepo.CloneCharacter(character)
	if planCharacter.Stats == nil {
		planCharacter.Stats = make(map[string]int64, 1)
	}
	if welcome != nil {
		planCharacter.Stats["finish_loading_exp_category_9"] = int64(*welcome)
	} else {
		planCharacter.Stats["finish_loading_exp_category_9"] = 0
	}
	characterBody := buildCurrentFinishLoadingCharacterStateBody(planCharacter, points)
	if len(characterBody) != currentFinishLoadingCharacterStateBodySize {
		return currentDungeonSettlementPacketPlan{}, fmt.Errorf(
			"%w: character_body=%d want=%d",
			errCurrentDungeonSettlementPlanShape,
			len(characterBody),
			currentFinishLoadingCharacterStateBodySize,
		)
	}

	return currentDungeonSettlementPacketPlan{
		CharacterID:     reward.CharacterID,
		CompletionKey:   reward.CompletionKey,
		Source:          reward.Source,
		ClientRankPoint: clientRankPoint,
		ClearTimeMS:     notice.ClearTimeMS,
		PlayResultBody:  append([]byte(nil), playResultBody...),
		CharacterBody:   append([]byte(nil), characterBody...),
		ClearRewardBody: append([]byte(nil), clearRewardBody...),
		// Keep the committed reward snapshot with the frozen plan so a later
		// reader can tell whether this clear actually presented the ordinary
		// result UI (a tower must not).
		clearRewardSnapshot: &reward,
	}, nil
}
