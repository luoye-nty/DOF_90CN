package dnfbridge

import (
	"context"
	"encoding/binary"
	"errors"
	"math"
	"sort"
	"strconv"
	"strings"
	"time"

	"longheng.io/server/internal/modules/dnf/adventuregroup"
	"longheng.io/server/internal/modules/dnf/dnfenum"
	dnfdungeon "longheng.io/server/internal/modules/dnf/dungeon"
	dnfhonor "longheng.io/server/internal/modules/dnf/honor"
	"longheng.io/server/internal/modules/dnf/progression"
	dnfrepo "longheng.io/server/internal/modules/dnf/repository"
)

const currentAdventureGenericFailureCode = 1

// 冒险团商店（op1414）的结果码取自客户端 0x00C79940 的跳转表（重启后只认这些）：
//
//	19  → dstr 44465 "超过购买限制"
//	22  → adv 100002122 "购买材料不足。"
//	23  → adv 100002156 "无法购买决斗场专用角色。"
//	204 → dstr 746  "物品栏空间不足"
//	235 → adv 100003018 "冒险团Lv较低。"
//
// 表里其余的码（**包括我们过去一律回的 1**）落到 default 分支 = 什么都不弹。
// "买超限了却没提示"就是这么来的。
const (
	currentAdventureShopPurchaseLimitCode = 19
	currentAdventureShopInsufficientCode  = 22
	currentAdventureShopLevelTooLowCode   = 235
	currentAdventureShopInventoryFullCode = 204
)

// currentAdventureShopFailureCode 把拒绝原因翻成客户端认识的 19/22/204，
// 认不出来的原因仍回笼统码（静默）——宁可不说，也不能把"背包满"说成"材料不足"。
func currentAdventureShopFailureCode(err error) byte {
	switch {
	case errors.Is(err, adventuregroup.ErrPurchaseLimitReached):
		return currentAdventureShopPurchaseLimitCode
	case errors.Is(err, adventuregroup.ErrInsufficientShopPoints):
		return currentAdventureShopInsufficientCode
	case errors.Is(err, errDungeonPickupInventoryFull),
		errors.Is(err, dnfdungeon.ErrInventoryFull):
		return currentAdventureShopInventoryFullCode
	default:
		return currentAdventureGenericFailureCode
	}
}

// The current NoPack executable and its DNF90 content profile cap characters
// at level 90. The progression PVF contains later table rows for compatibility,
// so its row count is not a playable-level cap.
const currentAdventureCharacterLevelCap = progression.MaximumCharacterLevel

var (
	errCurrentAdventureRuntimeUnavailable = errors.New("current adventure-group runtime is unavailable")
	errCurrentAdventureRequestInvalid     = errors.New("current adventure-group request is invalid")
	errCurrentAdventureStateInvalid       = errors.New("current adventure-group state is invalid")
)

type currentAdventureSelectedState struct {
	Repositories dnfrepo.Group
	AccountID    string
	CharacterID  string
	Character    dnfrepo.CharacterRecord
	Account      dnfrepo.AccountRecord
	Characters   []dnfrepo.CharacterRecord
	Summary      adventuregroup.Summary
	Config       adventuregroup.RuntimeConfig
	Runtime      adventuregroup.RuntimeState
}

func (s *Service) currentAdventureSelectedState(
	ctx context.Context,
	session *gameSession,
) (currentAdventureSelectedState, error) {
	if s == nil || session == nil || session.selectedCharacterID == 0 {
		return currentAdventureSelectedState{}, errCurrentAdventureRuntimeUnavailable
	}
	repositories, ok := s.repositoryGroup()
	if !ok || repositories.Account == nil || repositories.Character == nil {
		return currentAdventureSelectedState{}, errCurrentAdventureRuntimeUnavailable
	}
	accountID := strings.TrimSpace(s.accountIDForSession(session))
	characterID := strconv.Itoa(int(session.selectedCharacterID))
	character, found, err := repositories.Character.Load(ctx, characterID)
	if err != nil {
		return currentAdventureSelectedState{}, err
	}
	if !found || strings.TrimSpace(character.AccountID) != accountID {
		return currentAdventureSelectedState{}, errCurrentAdventureStateInvalid
	}
	account, found, err := repositories.Account.Load(ctx, accountID)
	if err != nil {
		return currentAdventureSelectedState{}, err
	}
	if !found || strings.TrimSpace(account.AccountID) != accountID {
		return currentAdventureSelectedState{}, errCurrentAdventureStateInvalid
	}
	state, err := s.currentAccountAdventureGroupState(ctx, character, true, session)
	if err != nil {
		return currentAdventureSelectedState{}, err
	}
	config := s.adventureGroupTable.Runtime()
	runtime, err := adventuregroup.ParseRuntimeState(account, config, s.gameplayNow())
	if err != nil {
		return currentAdventureSelectedState{}, err
	}
	return currentAdventureSelectedState{
		Repositories: repositories,
		AccountID:    accountID,
		CharacterID:  characterID,
		Character:    character,
		Account:      account,
		Characters:   state.Characters,
		Summary:      state.Summary,
		Config:       config,
		Runtime:      runtime,
	}, nil
}

func saveCurrentAdventureRuntime(
	ctx context.Context,
	state currentAdventureSelectedState,
	now time.Time,
) error {
	value, err := state.Runtime.Marshal()
	if err != nil {
		return err
	}
	if state.Repositories.CharacterSettlement == nil {
		return errCurrentAdventureRuntimeUnavailable
	}
	return state.Repositories.CharacterSettlement.WithinCharacterSettlement(ctx, state.CharacterID, func(tx dnfrepo.Group) error {
		account, found, err := tx.Account.Load(ctx, state.AccountID)
		if err != nil {
			return err
		}
		if !found || account.AccountID != state.AccountID ||
			account.Metadata[adventuregroup.RuntimeStateMetadataKey] != state.Account.Metadata[adventuregroup.RuntimeStateMetadataKey] {
			return errCurrentAdventureStateInvalid
		}
		// Reject a stale expedition write if settlement or another session accrued currency.
		return dnfrepo.SaveAccountMetadataEntry(ctx, tx.Account, account, adventuregroup.RuntimeStateMetadataKey, value, now)
	})
}

func (s *Service) handleCurrentAdventureExpeditionInfo(session *gameSession, request []byte) error {
	if len(request) < 1 || int(request[0])+1 != len(request) {
		return s.sendGameUpperFailure(session, uint16(dnfenum.CmdPacketMercenaryInfo), currentAdventureGenericFailureCode)
	}
	ctx, cancel := context.WithTimeout(context.Background(), createWriteTimeout)
	defer cancel()
	s.adventureGroupRuntimeMu.Lock()
	state, err := s.currentAdventureSelectedState(ctx, session)
	s.adventureGroupRuntimeMu.Unlock()
	if err != nil {
		s.logGameEvent(session, "game-adventure-expedition-info-rejected", "error", err)
		return s.sendGameUpperFailure(session, uint16(dnfenum.CmdPacketMercenaryInfo), currentAdventureGenericFailureCode)
	}
	body := buildCurrentAdventureExpeditionStateBody(currentAdventureExpeditionProjection(state, s.gameplayNow()), s.gameplayNow())
	return s.sendGameUpperSuccess(session, uint16(dnfenum.CmdPacketMercenaryInfo), body)
}

type currentAdventureExpeditionStartRequest struct {
	Area            byte
	DurationSeconds uint32
	Members         []currentAdventureExpeditionMemberRequest
}

type currentAdventureExpeditionMemberRequest struct {
	Selector byte
	Job      byte
	GrowType byte
}

func parseCurrentAdventureExpeditionStartRequest(body []byte) (currentAdventureExpeditionStartRequest, error) {
	if len(body) < 7 {
		return currentAdventureExpeditionStartRequest{}, errCurrentAdventureRequestInvalid
	}
	count := int(binary.LittleEndian.Uint16(body[5:7]))
	if count <= 0 || count > 4 || len(body) != 7+count*3 {
		return currentAdventureExpeditionStartRequest{}, errCurrentAdventureRequestInvalid
	}
	request := currentAdventureExpeditionStartRequest{
		Area:            body[0],
		DurationSeconds: binary.LittleEndian.Uint32(body[1:5]),
		Members:         make([]currentAdventureExpeditionMemberRequest, count),
	}
	for index := range request.Members {
		offset := 7 + index*3
		request.Members[index] = currentAdventureExpeditionMemberRequest{
			Selector: body[offset],
			Job:      body[offset+1],
			GrowType: body[offset+2],
		}
	}
	return request, nil
}

func (s *Service) handleCurrentAdventureExpeditionStart(session *gameSession, body []byte) error {
	request, err := parseCurrentAdventureExpeditionStartRequest(body)
	if err != nil {
		return s.sendGameUpperFailure(session, uint16(dnfenum.CmdPacketMercenaryCompetition), currentAdventureGenericFailureCode)
	}
	ctx, cancel := context.WithTimeout(context.Background(), createWriteTimeout)
	defer cancel()
	now := s.gameplayNow()
	s.adventureGroupRuntimeMu.Lock()
	defer s.adventureGroupRuntimeMu.Unlock()
	state, err := s.currentAdventureSelectedState(ctx, session)
	if err != nil {
		return s.sendGameUpperFailure(session, uint16(dnfenum.CmdPacketMercenaryCompetition), currentAdventureGenericFailureCode)
	}
	area, found := state.Config.Area(request.Area)
	if !found || state.Summary.ManageLevel < area.RequiredManageLevel {
		return s.sendGameUpperFailure(session, uint16(dnfenum.CmdPacketMercenaryCompetition), 7)
	}
	if _, exists := state.Runtime.Expeditions[adventuregroup.ExpeditionKey(request.Area)]; exists {
		return s.sendGameUpperFailure(session, uint16(dnfenum.CmdPacketMercenaryCompetition), 14)
	}
	if _, found := area.RewardRates[request.DurationSeconds]; !found {
		return s.sendGameUpperFailure(session, uint16(dnfenum.CmdPacketMercenaryCompetition), 18)
	}
	members, rewardInputs, ok := resolveCurrentAdventureExpeditionMembers(
		request.Members,
		state.Characters,
		state.Runtime,
	)
	if !ok {
		return s.sendGameUpperFailure(session, uint16(dnfenum.CmdPacketMercenaryCompetition), 22)
	}
	// The designated attribute set comes from the same rotated snapshot the
	// client was shown, so the paid reward matches the preview.
	reward, ok := state.Config.ExpeditionReward(
		request.Area,
		request.DurationSeconds,
		rewardInputs,
		state.Config.AreaAttributes(request.Area, now),
	)
	if !ok {
		return s.sendGameUpperFailure(session, uint16(dnfenum.CmdPacketMercenaryCompetition), 22)
	}
	startedAt := now.Unix()
	state.Runtime.Expeditions[adventuregroup.ExpeditionKey(request.Area)] = adventuregroup.Expedition{
		Area:       request.Area,
		State:      1,
		StartedAt:  startedAt,
		EndsAt:     startedAt + int64(request.DurationSeconds),
		Attributes: state.Config.AreaAttributes(request.Area, now),
		Members:    members,
		Reward:     reward,
	}
	if err := saveCurrentAdventureRuntime(ctx, state, now); err != nil {
		return s.sendGameUpperFailure(session, uint16(dnfenum.CmdPacketMercenaryCompetition), currentAdventureGenericFailureCode)
	}
	if err := s.sendGameUpperSuccess(session, uint16(dnfenum.CmdPacketMercenaryCompetition), nil); err != nil {
		return err
	}
	return s.sendCurrentAdventureExpeditionStatePush(session, state, now)
}

func resolveCurrentAdventureExpeditionMembers(
	requests []currentAdventureExpeditionMemberRequest,
	characters []dnfrepo.CharacterRecord,
	runtime adventuregroup.RuntimeState,
) ([]adventuregroup.ExpeditionMember, []adventuregroup.ExpeditionMemberInput, bool) {
	used := make(map[byte]struct{}, len(requests))
	members := make([]adventuregroup.ExpeditionMember, 0, len(requests))
	inputs := make([]adventuregroup.ExpeditionMemberInput, 0, len(requests))
	for _, request := range requests {
		if _, duplicate := used[request.Selector]; duplicate {
			return nil, nil, false
		}
		used[request.Selector] = struct{}{}
		var character dnfrepo.CharacterRecord
		found := false
		for _, candidate := range characters {
			// The selector is a roster slot. A character whose numeric ID
			// happens to equal it is a different character, and a stale
			// selector left over from a roster reorder must not resolve.
			if candidate.Slot == int(request.Selector) {
				character = candidate
				found = true
				break
			}
		}
		job := byte(numericCharacterStat(character.Job))
		grow := byte(numericCharacterStatValue(character, "grow_type")) & 0x0f
		if !found || job != request.Job || grow != request.GrowType || character.Level <= 0 {
			return nil, nil, false
		}
		// Busy is decided by durable identity, so reordering the roster cannot
		// let the same character be sent out twice.
		if currentAdventureExpeditionCharacterBusy(runtime, character) {
			return nil, nil, false
		}
		members = append(members, adventuregroup.ExpeditionMember{
			CharacterID:           uint16(request.Selector),
			PersistentCharacterID: character.CharacterID,
			Name:                  character.Name,
			Level:                 uint32(character.Level),
			Job:                   job,
			GrowType:              grow,
		})
		inputs = append(inputs, adventuregroup.ExpeditionMemberInput{
			Level: character.Level, Job: job, GrowType: grow,
		})
	}
	return members, inputs, true
}

// currentAdventureExpeditionCharacterBusy reports whether this character is
// already sent out. It matches on the durable character ID first, and falls
// back to the profession row because the native candidate table holds one row
// per profession and old saves only recorded that pair.
func currentAdventureExpeditionCharacterBusy(runtime adventuregroup.RuntimeState, character dnfrepo.CharacterRecord) bool {
	for _, expedition := range runtime.Expeditions {
		for _, member := range expedition.Members {
			if member.PersistentCharacterID == character.CharacterID ||
				(member.Job == byte(numericCharacterStat(character.Job)) &&
					member.GrowType&0x0f == byte(numericCharacterStatValue(character, "grow_type"))&0x0f) {
				return true
			}
		}
	}
	return false
}

// currentAdventureExpeditionProjection builds the state the client reads. The
// native sub_D44860 accepts area 0 as the idle-candidate table and rebuilds the
// whole list from it, so every eligible character is published there; without
// it the expedition window has no characters to offer.
func currentAdventureExpeditionProjection(state currentAdventureSelectedState, now time.Time) adventuregroup.RuntimeState {
	projection := state.Runtime
	projection.Expeditions = make(map[string]adventuregroup.Expedition, len(state.Config.ExpeditionAreas)+1)
	idle := adventuregroup.Expedition{}
	for _, character := range state.Characters {
		if character.Slot < 0 || character.Slot > math.MaxUint8 ||
			currentAdventureExpeditionCharacterBusy(state.Runtime, character) {
			continue
		}
		idle.Members = append(idle.Members, adventuregroup.ExpeditionMember{
			CharacterID:           uint16(character.Slot),
			PersistentCharacterID: character.CharacterID,
			Name:                  character.Name,
			Level:                 uint32(character.Level),
			Job:                   byte(numericCharacterStat(character.Job)),
			GrowType:              byte(numericCharacterStatValue(character, "grow_type")) & 0x0f,
		})
	}
	projection.Expeditions["0"] = idle
	for _, area := range state.Config.ExpeditionAreas {
		key := adventuregroup.ExpeditionKey(area.Index)
		expedition, exists := state.Runtime.Expeditions[key]
		if !exists {
			expedition = adventuregroup.Expedition{Area: area.Index, Attributes: state.Config.AreaAttributes(area.Index, now)}
		}
		projection.Expeditions[key] = expedition
	}
	return projection
}

func (s *Service) refreshCurrentAdventureExpeditionState(ctx context.Context, session *gameSession) error {
	// Representative ACKs update the collection only; op1342 rebuilds the cache.
	state, err := s.currentAdventureSelectedState(ctx, session)
	if err != nil {
		return err
	}
	return s.sendCurrentAdventureExpeditionStatePush(session, state, s.gameplayNow())
}

func (s *Service) handleCurrentAdventureExpeditionCancel(session *gameSession, body []byte) error {
	if len(body) != 1 {
		return s.sendGameUpperFailure(session, uint16(dnfenum.CmdPacketMercenaryCompetitionCancle), currentAdventureGenericFailureCode)
	}
	ctx, cancel := context.WithTimeout(context.Background(), createWriteTimeout)
	defer cancel()
	now := s.gameplayNow()
	s.adventureGroupRuntimeMu.Lock()
	defer s.adventureGroupRuntimeMu.Unlock()
	state, err := s.currentAdventureSelectedState(ctx, session)
	if err != nil {
		return s.sendGameUpperFailure(session, uint16(dnfenum.CmdPacketMercenaryCompetitionCancle), currentAdventureGenericFailureCode)
	}
	key := adventuregroup.ExpeditionKey(body[0])
	if _, found := state.Runtime.Expeditions[key]; !found {
		return s.sendGameUpperFailure(session, uint16(dnfenum.CmdPacketMercenaryCompetitionCancle), currentAdventureGenericFailureCode)
	}
	delete(state.Runtime.Expeditions, key)
	if err := saveCurrentAdventureRuntime(ctx, state, now); err != nil {
		return s.sendGameUpperFailure(session, uint16(dnfenum.CmdPacketMercenaryCompetitionCancle), currentAdventureGenericFailureCode)
	}
	if err := s.sendGameUpperSuccess(session, uint16(dnfenum.CmdPacketMercenaryCompetitionCancle), nil); err != nil {
		return err
	}
	return s.sendCurrentAdventureExpeditionStatePush(session, state, now)
}

func (s *Service) handleCurrentAdventureExpeditionReward(session *gameSession, body []byte) error {
	if len(body) != 1 {
		return s.sendGameUpperFailure(session, uint16(dnfenum.CmdPacketMercenaryCompetitionRewardRequest), currentAdventureGenericFailureCode)
	}
	ctx, cancel := context.WithTimeout(context.Background(), createWriteTimeout)
	defer cancel()
	now := s.gameplayNow()
	s.adventureGroupRuntimeMu.Lock()
	defer s.adventureGroupRuntimeMu.Unlock()
	state, err := s.currentAdventureSelectedState(ctx, session)
	if err != nil {
		return s.sendGameUpperFailure(session, uint16(dnfenum.CmdPacketMercenaryCompetitionRewardRequest), currentAdventureGenericFailureCode)
	}
	key := adventuregroup.ExpeditionKey(body[0])
	expedition, found := state.Runtime.Expeditions[key]
	if !found || expedition.State == 0 || expedition.EndsAt > now.Unix() || expedition.EndsAt <= expedition.StartedAt || expedition.EndsAt-expedition.StartedAt > math.MaxUint32 {
		return s.sendGameUpperFailure(session, uint16(dnfenum.CmdPacketMercenaryCompetitionRewardRequest), currentAdventureGenericFailureCode)
	}
	// Recompute old saved estimates with the native formula and the departure
	// attribute snapshot, so pending expeditions also stop using level * rate.
	rewardInputs := make([]adventuregroup.ExpeditionMemberInput, 0, len(expedition.Members))
	for _, member := range expedition.Members {
		rewardInputs = append(rewardInputs, adventuregroup.ExpeditionMemberInput{Level: int(member.Level), Job: member.Job, GrowType: member.GrowType})
	}
	reward, ok := state.Config.ExpeditionReward(expedition.Area, uint32(expedition.EndsAt-expedition.StartedAt), rewardInputs, expedition.Attributes)
	if !ok {
		return s.sendGameUpperFailure(session, uint16(dnfenum.CmdPacketMercenaryCompetitionRewardRequest), currentAdventureGenericFailureCode)
	}
	delete(state.Runtime.Expeditions, key)
	previousGlory := state.Runtime.ShopPoints[adventuregroup.ShopPointGlory]
	state.Runtime.AddGlory(state.Config, reward)
	if err := saveCurrentAdventureRuntime(ctx, state, now); err != nil {
		return s.sendGameUpperFailure(session, uint16(dnfenum.CmdPacketMercenaryCompetitionRewardRequest), currentAdventureGenericFailureCode)
	}
	s.logGameEvent(session, "game-adventure-expedition-reward-committed",
		"area", expedition.Area, "duration_seconds", expedition.EndsAt-expedition.StartedAt,
		"members", len(expedition.Members), "previous_estimate", expedition.Reward,
		"native_reward", reward, "granted", state.Runtime.ShopPoints[adventuregroup.ShopPointGlory]-previousGlory)
	var response packetWriter
	response.writeUint32(state.Runtime.ShopPoints[adventuregroup.ShopPointGlory] - previousGlory)
	if err := s.sendGameUpperSuccess(session, uint16(dnfenum.CmdPacketMercenaryCompetitionRewardRequest), response.bytes()); err != nil {
		return err
	}
	if err := s.sendCurrentAdventureExpeditionStatePush(session, state, now); err != nil {
		return err
	}
	return s.sendCurrentAdventureInfoPushFromAccount(session, session.selectedCharacterID, "expedition_reward")
}

func (s *Service) handleCurrentAdventurePointRecalculate(session *gameSession, body []byte) error {
	if len(body) != 0 {
		return s.sendGameUpperFailure(session, uint16(dnfenum.CmdPacketMercenaryPointRecalculate), currentAdventureGenericFailureCode)
	}
	ctx, cancel := context.WithTimeout(context.Background(), createWriteTimeout)
	defer cancel()
	s.adventureGroupRuntimeMu.Lock()
	state, err := s.currentAdventureSelectedState(ctx, session)
	s.adventureGroupRuntimeMu.Unlock()
	if err != nil {
		return s.sendGameUpperFailure(session, uint16(dnfenum.CmdPacketMercenaryPointRecalculate), currentAdventureGenericFailureCode)
	}
	var response packetWriter
	response.writeUint32(state.Runtime.ShopPoints[adventuregroup.ShopPointGlory])
	response.writeUint32(state.Runtime.ShopPoints[adventuregroup.ShopPointBrave])
	response.writeByte(0)
	return s.sendGameUpperSuccess(session, uint16(dnfenum.CmdPacketMercenaryPointRecalculate), response.bytes())
}

func buildCurrentAdventureExpeditionStateBody(state adventuregroup.RuntimeState, now time.Time) []byte {
	keys := make([]int, 0, len(state.Expeditions))
	for key := range state.Expeditions {
		value, err := strconv.Atoi(key)
		// Area 0 is the native idle-candidate table the client reads to list
		// the characters it may send out; filtering it out leaves that list
		// empty and the expedition window shows no characters at all.
		if err == nil && value >= 0 && value <= math.MaxUint8 {
			keys = append(keys, value)
		}
	}
	sort.Ints(keys)
	if len(keys) > math.MaxUint8 {
		keys = keys[:math.MaxUint8]
	}
	var writer packetWriter
	writer.writeByte(byte(len(keys)))
	for _, areaValue := range keys {
		expedition := state.Expeditions[strconv.Itoa(areaValue)]
		expeditionState := expedition.State
		// The idle table carries state 0 with no timestamps; only a real
		// expedition can expire.
		if expedition.State != 0 && expedition.EndsAt <= now.Unix() {
			expeditionState = 2
		}
		writer.writeByte(byte(areaValue))
		writer.writeByte(expeditionState)
		writer.writeUint32(clampAdventureUnix(expedition.StartedAt))
		writer.writeUint32(clampAdventureUnix(expedition.EndsAt))
		attributeCount := len(expedition.Attributes)
		if attributeCount > math.MaxUint8 {
			attributeCount = math.MaxUint8
		}
		writer.writeByte(byte(attributeCount))
		for _, attribute := range expedition.Attributes[:attributeCount] {
			writer.writeByte(attribute)
		}
		memberCount := len(expedition.Members)
		if memberCount > math.MaxUint16 {
			memberCount = math.MaxUint16
		}
		writer.writeUint16(uint16(memberCount))
		for _, member := range expedition.Members[:memberCount] {
			writer.writeUint16(member.CharacterID)
			writer.writeRawDstr(rosterNameBytes(member.Name))
			writer.writeUint32(member.Level)
			writer.writeByte(member.Job)
			writer.writeByte(member.GrowType)
			writer.writeByte(member.Status)
		}
	}
	return writer.bytes()
}

func clampAdventureUnix(value int64) uint32 {
	if value <= 0 {
		return 0
	}
	if value > math.MaxUint32 {
		return math.MaxUint32
	}
	return uint32(value)
}

func (s *Service) sendCurrentAdventureExpeditionStatePush(
	session *gameSession,
	selected currentAdventureSelectedState,
	now time.Time,
) error {
	var body packetWriter
	body.writeByte(1)
	body.writeBytes(buildCurrentAdventureExpeditionStateBody(currentAdventureExpeditionProjection(selected, now), now))
	return s.sendGameUpperRawClass(session, 1342, body.bytes(), 0)
}

type currentAdventureShopCommit struct {
	Inventory dnfrepo.InventoryRecord
	// AccountInventory / AccountSlots 是账号共享仓那一侧（无色小晶块、灵魂落 354..365）。
	AccountInventory dnfrepo.AccountInventoryRecord
	AccountSlots     []uint16
	Slots            []uint16
}

func (s *Service) handleCurrentAdventureShopPurchase(session *gameSession, body []byte) error {
	if len(body) != 5 {
		return s.sendGameUpperFailure(session, uint16(dnfenum.CmdPacketAdventurerShopPurchase), currentAdventureGenericFailureCode)
	}
	categoryIndex := body[0]
	selector := binary.LittleEndian.Uint32(body[1:5])
	ctx, cancel := context.WithTimeout(context.Background(), createWriteTimeout)
	defer cancel()
	now := s.gameplayNow()

	s.adventureGroupRuntimeMu.Lock()
	defer s.adventureGroupRuntimeMu.Unlock()
	state, err := s.currentAdventureSelectedState(ctx, session)
	if err != nil || state.Repositories.CeraShopAssets == nil {
		return s.sendGameUpperFailure(session, uint16(dnfenum.CmdPacketAdventurerShopPurchase), currentAdventureGenericFailureCode)
	}
	category, found := state.Config.Shop(categoryIndex)
	if !found {
		return s.sendGameUpperFailure(session, uint16(dnfenum.CmdPacketAdventurerShopPurchase), currentAdventureGenericFailureCode)
	}
	product, found := resolveCurrentAdventureShopProduct(category, selector)
	if !found {
		return s.sendGameUpperFailure(session, uint16(dnfenum.CmdPacketAdventurerShopPurchase), currentAdventureGenericFailureCode)
	}
	if state.Summary.ManageLevel < product.RequiredManageLevel {
		// 客户端 235 = "冒险团Lv较低。"（0x00C79A2B）。回笼统码会让它什么都不弹。
		return s.sendGameUpperFailure(session, uint16(dnfenum.CmdPacketAdventurerShopPurchase), currentAdventureShopLevelTooLowCode)
	}
	catalog, err := s.currentPVFItemCatalog()
	if err != nil {
		return s.sendGameUpperFailure(session, uint16(dnfenum.CmdPacketAdventurerShopPurchase), currentAdventureGenericFailureCode)
	}
	definition, err := catalog.ResolveItem(product.ItemID)
	if err != nil {
		return s.sendGameUpperFailure(session, uint16(dnfenum.CmdPacketAdventurerShopPurchase), currentAdventureGenericFailureCode)
	}
	var result currentAdventureShopCommit
	err = state.Repositories.CeraShopAssets.WithinCeraShopAssets(
		ctx,
		state.AccountID,
		state.CharacterID,
		"adventure_group_shop:"+state.CharacterID,
		func(
			accounts dnfrepo.AccountRepository,
			accountInventories dnfrepo.AccountInventoryRepository,
			characters dnfrepo.CharacterRepository,
			inventories dnfrepo.InventoryRepository,
			_ dnfrepo.EquipmentRepository,
			_ dnfrepo.SettingsRepository,
		) error {
			account, found, err := accounts.Load(ctx, state.AccountID)
			if err != nil || !found {
				return errCurrentAdventureStateInvalid
			}
			character, found, err := characters.Load(ctx, state.CharacterID)
			if err != nil || !found || strings.TrimSpace(character.AccountID) != state.AccountID {
				return errCurrentAdventureStateInvalid
			}
			inventory, found, err := inventories.Load(ctx, state.CharacterID)
			if err != nil || !found {
				return errCurrentAdventureStateInvalid
			}
			runtime, err := adventuregroup.ParseRuntimeState(account, state.Config, now)
			if err != nil {
				return err
			}
			if err := runtime.Spend(category, product); err != nil {
				return err
			}
			// 账号共享优先：冒险团商店直接卖传说灵魂（PVF [adventurer shop purchase info]
			// 里的 `10099775 15 70 3 3`），它的家是账号共享仓的 365 格。
			// 账号记录与本事务同生共死（扣点、发货一起提交）。
			accountInventory, accountFound, err := accountInventories.Load(ctx, state.AccountID)
			if err != nil {
				return errCurrentAdventureStateInvalid
			}
			if !accountFound {
				accountInventory = dnfrepo.AccountInventoryRecord{AccountID: state.AccountID}
			}
			accountInventory = dnfrepo.CloneAccountInventory(accountInventory)
			accountInventory.AccountID = state.AccountID
			if accountInventory.Slots == nil {
				accountInventory.Slots = make(map[string]dnfrepo.ItemStack)
			}
			var accountSlot int16
			shared := grantCurrentAccountSharedFirst(&accountInventory, definition, 1, "adventure_shop")
			if shared.Changed {
				accountSlot = shared.Slot
			}
			var slots []uint16
			if shared.Remainder > 0 {
				bagSlots, err := grantCurrentCeraShopProduct(&inventory, definition, shared.Remainder, s.currentInventoryPlacementMainSlotCount(ctx, &inventory, ""))
				if err != nil {
					return err
				}
				slots = append(slots, bagSlots...)
			}
			if accountSlot == 0 && len(slots) == 0 {
				return errCurrentAdventureStateInvalid
			}
			encoded, err := runtime.Marshal()
			if err != nil {
				return err
			}
			account = dnfrepo.CloneAccount(account)
			if account.Metadata == nil {
				account.Metadata = make(map[string]string)
			}
			account.Metadata[adventuregroup.RuntimeStateMetadataKey] = encoded
			account.UpdatedAt = now.UTC()
			inventory.UpdatedAt = now.UTC()
			if err := accounts.Save(ctx, account); err != nil {
				return err
			}
			if accountSlot != 0 {
				accountInventory.UpdatedAt = now.UTC()
				if err := accountInventories.Save(ctx, accountInventory); err != nil {
					return err
				}
			}
			if err := inventories.Save(ctx, inventory); err != nil {
				return err
			}
			result = currentAdventureShopCommit{
				Inventory:        dnfrepo.CloneInventory(inventory),
				AccountInventory: dnfrepo.CloneAccountInventory(accountInventory),
				Slots:            append([]uint16(nil), slots...),
			}
			if accountSlot != 0 {
				result.AccountSlots = []uint16{uint16(accountSlot)}
			}
			return nil
		},
	)
	if err != nil {
		failureCode := currentAdventureShopFailureCode(err)
		s.logGameEvent(session, "game-adventure-shop-purchase-rejected",
			"category", categoryIndex, "selector", selector, "item_id", product.ItemID,
			"failure_code", failureCode, "error", err)
		return s.sendGameUpperFailure(session, uint16(dnfenum.CmdPacketAdventurerShopPurchase), failureCode)
	}
	if err := s.sendGameUpperSuccess(session, uint16(dnfenum.CmdPacketAdventurerShopPurchase), nil); err != nil {
		return err
	}
	if err := sendCurrentAdventureInventoryUpdates(s, session, result); err != nil {
		return err
	}
	return s.sendCurrentAdventureInfoPushFromAccount(session, session.selectedCharacterID, "adventure_shop_purchase")
}

func resolveCurrentAdventureShopProduct(category adventuregroup.ShopCategory, selector uint32) (adventuregroup.ShopProduct, bool) {
	for _, product := range category.Products {
		if product.ItemID == selector {
			return product, true
		}
	}
	if uint64(selector) < uint64(len(category.Products)) {
		return category.Products[selector], true
	}
	return adventuregroup.ShopProduct{}, false
}

func sendCurrentAdventureInventoryUpdates(
	service *Service,
	session *gameSession,
	result currentAdventureShopCommit,
) error {
	entries := make([]currentItemListEntry, 0, len(result.Slots)+len(result.AccountSlots))
	for _, slot := range result.Slots {
		signedSlot := int16(slot)
		stack, found := result.Inventory.Slots[currentDungeonPickupMainSlotKey(signedSlot)]
		if !found {
			continue
		}
		entries = append(entries, currentItemListEntryFromStack(dnfrepo.MainInventoryListType, signedSlot, stack))
	}
	// 账号共享槽也是 list-0 线地址（354..365）：不加这一行，客户端要等下一次
	// 列表刷新才看得到买到的灵魂。
	for _, slot := range result.AccountSlots {
		signedSlot := int16(slot)
		stack, found := result.AccountInventory.Slots[dnfrepo.AccountSharedInventorySlotKey(signedSlot)]
		if !found {
			continue
		}
		entries = append(entries, currentItemListEntryFromStack(dnfrepo.MainInventoryListType, signedSlot, stack))
	}
	if len(entries) == 0 {
		return nil
	}
	sortCurrentItemListEntries(entries)
	body := buildCurrentItemUpdateBody(dnfrepo.MainInventoryListType, entries)
	return service.sendGameUpperRawClass(session, uint16(dnfenum.CmdPacketWalkoutPartyMember), body, 0)
}

type currentAdventureGrowthResult struct {
	PreviousLevel int
	Character     dnfrepo.CharacterRecord
	Skill         dnfrepo.SkillRecord
}

func (s *Service) handleCurrentAdventureGrowthCapsule(session *gameSession, body []byte) error {
	if len(body) != 0 {
		return s.sendGameUpperFailure(session, uint16(dnfenum.CmdPacketAdventureGrowthcapsuleExp), currentAdventureGenericFailureCode)
	}
	ctx, cancel := context.WithTimeout(context.Background(), createWriteTimeout)
	defer cancel()
	now := s.gameplayNow()
	s.adventureGroupRuntimeMu.Lock()
	defer s.adventureGroupRuntimeMu.Unlock()
	state, err := s.currentAdventureSelectedState(ctx, session)
	if err != nil || state.Repositories.CharacterSettlement == nil {
		return s.sendGameUpperFailure(session, uint16(dnfenum.CmdPacketAdventureGrowthcapsuleExp), currentAdventureGenericFailureCode)
	}
	s.initialEquipmentMu.Lock()
	archive, err := s.initialEquipmentArchiveLocked()
	s.initialEquipmentMu.Unlock()
	if err != nil {
		return s.sendGameUpperFailure(session, uint16(dnfenum.CmdPacketAdventureGrowthcapsuleExp), currentAdventureGenericFailureCode)
	}
	tables, err := progression.Load(ctx, archive)
	if err != nil {
		return s.sendGameUpperFailure(session, uint16(dnfenum.CmdPacketAdventureGrowthcapsuleExp), currentAdventureGenericFailureCode)
	}
	preAwardExperience, present := state.Character.Stats["exp"]
	if !present || preAwardExperience < 0 || uint64(preAwardExperience) > math.MaxUint32 {
		return s.sendGameUpperFailure(session, uint16(dnfenum.CmdPacketAdventureGrowthcapsuleExp), currentAdventureGenericFailureCode)
	}
	preAwardHonorExpertGain, err := currentHonorExpertExperienceGain(
		tables,
		state.Character.Level,
		uint32(preAwardExperience),
		state.Config.Capsule.GrantedExperience,
	)
	if err != nil {
		return s.sendGameUpperFailure(session, uint16(dnfenum.CmdPacketAdventureGrowthcapsuleExp), currentAdventureGenericFailureCode)
	}
	var honorTables *dnfhonor.Tables
	if preAwardHonorExpertGain > 0 {
		honorTables, err = s.loadHonorTable(ctx)
		if err != nil {
			return s.sendGameUpperFailure(session, uint16(dnfenum.CmdPacketAdventureGrowthcapsuleExp), currentAdventureGenericFailureCode)
		}
	}
	var result currentAdventureGrowthResult
	err = state.Repositories.CharacterSettlement.WithinCharacterSettlement(ctx, state.CharacterID, func(tx dnfrepo.Group) error {
		if tx.Account == nil {
			return errCurrentAdventureRuntimeUnavailable
		}
		account, found, err := tx.Account.Load(ctx, state.AccountID)
		if err != nil || !found {
			return errCurrentAdventureStateInvalid
		}
		character, found, err := tx.Character.Load(ctx, state.CharacterID)
		if err != nil || !found || strings.TrimSpace(character.AccountID) != state.AccountID {
			return errCurrentAdventureStateInvalid
		}
		skill, found, err := tx.Skill.Load(ctx, state.CharacterID)
		if err != nil || !found || skill.Points.SyncedLevel != character.Level {
			return errCurrentAdventureStateInvalid
		}
		capsule := state.Config.Capsule
		if character.Level < capsule.MinimumLevel || character.Level > capsule.MaximumLevel {
			return errCurrentAdventureRequestInvalid
		}
		runtime, err := adventuregroup.ParseRuntimeState(account, state.Config, now)
		if err != nil || !runtime.ConsumeCapsule(state.Config) {
			return errCurrentAdventureRequestInvalid
		}
		currentExperience := character.Stats["exp"]
		if currentExperience < 0 || uint64(currentExperience) > math.MaxUint32 {
			return errCurrentAdventureStateInvalid
		}
		plan, err := dnfdungeon.PlanSettlementProgressionAtCap(
			tables,
			character.Level,
			uint32(currentExperience),
			capsule.GrantedExperience,
			skill.Points,
			currentAdventureCharacterLevelCap,
		)
		if err != nil {
			return err
		}
		honorExpertGain, err := currentHonorExpertExperienceGain(
			tables,
			character.Level,
			uint32(currentExperience),
			capsule.GrantedExperience,
		)
		if err != nil {
			return err
		}
		previousLevel := character.Level
		character = dnfrepo.CloneCharacter(character)
		skill = dnfrepo.CloneSkill(skill)
		account = dnfrepo.CloneAccount(account)
		character.Level = plan.Experience.NewLevel
		character.Stats["exp"] = int64(plan.Experience.NewExperience)
		if honorExpertGain > 0 {
			nextHonor, err := planCurrentHonorExpertProgress(honorTables, character, honorExpertGain)
			if err != nil {
				return err
			}
			for key, value := range currentHonorExpertStats(nextHonor) {
				character.Stats[key] = value
			}
		}
		character.UpdatedAt = now.UTC()
		skill.Points = plan.SkillPoints.New
		skill.UpdatedAt = now.UTC()
		encoded, err := runtime.Marshal()
		if err != nil {
			return err
		}
		if account.Metadata == nil {
			account.Metadata = make(map[string]string)
		}
		account.Metadata[adventuregroup.RuntimeStateMetadataKey] = encoded
		account.UpdatedAt = now.UTC()
		if err := dnfrepo.SaveCharacterFields(ctx, tx.Character, character, dnfrepo.CharacterFieldBase, dnfrepo.CharacterFieldStats); err != nil {
			return err
		}
		if err := dnfrepo.SaveSkillFields(ctx, tx.Skill, skill, dnfrepo.SkillFieldPoints); err != nil {
			return err
		}
		if err := tx.Account.Save(ctx, account); err != nil {
			return err
		}
		result = currentAdventureGrowthResult{
			PreviousLevel: previousLevel,
			Character:     character,
			Skill:         skill,
		}
		return nil
	})
	if err != nil {
		s.logGameEvent(session, "game-adventure-growth-capsule-rejected", "error", err)
		return s.sendGameUpperFailure(session, uint16(dnfenum.CmdPacketAdventureGrowthcapsuleExp), currentAdventureGenericFailureCode)
	}
	if err := s.sendGameUpperSuccess(session, uint16(dnfenum.CmdPacketAdventureGrowthcapsuleExp), nil); err != nil {
		return err
	}
	characterBody := buildCurrentFinishLoadingCharacterStateBody(result.Character, result.Skill.Points)
	if err := s.sendGameUpperRawClass(session, currentDungeonCharacterStateMsgID, characterBody, 0); err != nil {
		return err
	}
	if result.Character.Level != result.PreviousLevel {
		if err := s.sendCurrentAcceptableQuestListForSession(
			session,
			"adventure_growth_capsule_after_atomic_level_change",
		); err != nil {
			return err
		}
	}
	return s.sendCurrentAdventureInfoPushFromAccount(session, session.selectedCharacterID, "adventure_growth_capsule")
}
