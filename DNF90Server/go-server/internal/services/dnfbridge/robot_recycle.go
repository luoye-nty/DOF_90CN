package dnfbridge

import (
	"context"
	"crypto/rand"
	"errors"
	"fmt"
	"math"
	"math/big"
	"runtime"
	"sort"
	"strconv"
	"strings"
	"sync"
	"time"

	"longheng.io/server/internal/modules/dnf/itemtrade"
	"longheng.io/server/internal/modules/dnf/party"
	dnfpvf "longheng.io/server/internal/modules/dnf/pvf"
	dnfrepo "longheng.io/server/internal/modules/dnf/repository"
	dnfrobotdomain "longheng.io/server/internal/modules/dnf/robot"
)

const (
	robotRecycleMessageFavorite       = "这个是我最喜欢的"
	robotRecycleMessageCheap          = "有些道具不值钱，你愿意给我就收走了"
	robotRecycleMessageMaterialTaken  = "给你换个装备，材料我拿走了"
	robotRecycleMessageRejected       = "我不收这个"
	robotRecycleMessageGoblin         = "你去格兰之森，问问那的哥布林要不要你这垃圾"
	robotRecycleAssetMarker           = "robot_recycle_trade"
	robotRecycleMaximumTradeOfferRows = 9
)

// RobotRecycleHooks connect the game bridge to the robot manager without
// making the manager own any trade/session packet state.
type RobotRecycleHooks struct {
	Begin func(characterID uint16) (dnfrobotdomain.Profile, bool)
	End   func(profileID string)
}

type robotRecycleGeneratedAsset struct {
	Slot   int16
	ItemID uint32
}

type robotRecycleEquipmentCandidateScan struct {
	ItemID uint32
	Key    dungeonDropGradeRarityKey
	OK     bool
}

type robotRecycleTrade struct {
	PlayerID     uint16
	Robot        dnfrobotdomain.Profile
	Token        string
	CompletionID string
	Gold         int64
	TopUpGold    int64
	Generated    []robotRecycleGeneratedAsset
}

func (s *Service) tryAcceptRobotRecycleTrade(player *gameSession, targetID uint16) (bool, error) {
	if s == nil || player == nil || targetID == 0 || s.robotRecycle.Begin == nil {
		return false, nil
	}
	profile, ok := s.robotRecycle.Begin(targetID)
	if !ok {
		return false, nil
	}
	target, online := s.onlineGameSession(targetID)
	if !online || target == nil {
		s.endRobotRecycle(profile.ID)
		return false, nil
	}
	// This is the same class-0/op8 response the native invitee emits after
	// class-1/op11. The bridge owns the robot's answer, so the external robot
	// client does not need a second trade state machine.
	notice := party.BuildResponsePeerNotice(targetID, 1, 0)
	if err := s.sendGameUpperRawClassCodec(player, 0x0008, notice, 0, true); err != nil {
		s.endRobotRecycle(profile.ID)
		return true, err
	}
	s.beginOnlineItemTrade(player, target)
	s.tradeMu.Lock()
	trade := s.itemTrades[player.selectedCharacterID]
	if trade != nil && trade == s.itemTrades[targetID] {
		trade.recycle = &robotRecycleTrade{
			PlayerID: player.selectedCharacterID,
			Robot:    profile,
			Token:    newRobotRecycleToken(),
		}
	}
	s.tradeMu.Unlock()
	if trade == nil || trade.recycle == nil {
		s.endRobotRecycle(profile.ID)
		return false, nil
	}
	s.logGameEvent(player, "game-robot-recycle-trade-accepted",
		"player_char_id", player.selectedCharacterID,
		"robot_char_id", targetID,
		"robot_profile", profile.ID)
	return true, nil
}

func (s *Service) endRobotRecycle(profileID string) {
	if s != nil && s.robotRecycle.End != nil && strings.TrimSpace(profileID) != "" {
		s.robotRecycle.End(strings.TrimSpace(profileID))
	}
}

type robotRecyclePlan struct {
	AcceptedOffers []onlineItemTradeOffer
	RejectedOffers []onlineItemTradeOffer
	Equipments     []dungeonDropItemDefinition
	Gold           int64
	GoldHit        bool
	Accepted       bool
	Messages       []string
}

func (s *Service) prepareRobotRecycleQuote(trade *onlineItemTrade, playerID uint16) error {
	if s == nil || trade == nil {
		return nil
	}
	s.tradeMu.Lock()
	recycle := trade.recycle
	playerState := trade.states[playerID]
	robotState := trade.states[recycleRobotCharacterID(trade)]
	playerOffers := snapshotOnlineItemTradeOffers(trade, playerID)
	s.tradeMu.Unlock()
	if recycle == nil || playerState != currentItemTradeAddingDone || robotState != 0 {
		return nil
	}

	policy := recycle.Robot.Recycle
	plan, err := s.planRobotRecycle(playerOffers, policy)
	if err != nil {
		s.logInfo("dnfbridge robot recycle quote failed", "player_char_id", playerID, "robot_char_id", recycle.Robot.CharacterID, "error", err)
		s.sendRobotRecycleMessages(recycle, playerID, []string{robotRecycleMessageRejected})
		return nil
	}
	if !plan.Accepted {
		messages := plan.Messages
		if len(messages) == 0 {
			messages = []string{robotRecycleMessageRejected}
		}
		s.sendRobotRecycleMessages(recycle, playerID, messages)
		return nil
	}

	ctx, cancel := context.WithTimeout(context.Background(), currentRepositorySnapshotTimeout)
	generated, topUp, err := s.prepareRobotRecycleAssets(ctx, recycle, plan.Gold, plan.Equipments)
	cancel()
	if err != nil {
		s.logInfo("dnfbridge robot recycle asset preparation failed",
			"player_char_id", playerID, "robot_char_id", recycle.Robot.CharacterID, "error", err)
		s.sendRobotRecycleMessages(recycle, playerID, []string{robotRecycleMessageRejected})
		return nil
	}

	s.tradeMu.Lock()
	current := s.itemTrades[playerID]
	if current != trade {
		s.tradeMu.Unlock()
		_ = s.cleanupRobotRecycleAssets(context.Background(), recycle, topUp, nil, true)
		s.endRobotRecycle(recycle.Robot.ID)
		return nil
	}
	if trade.committing || trade.states[playerID] != currentItemTradeAddingDone ||
		trade.states[recycle.Robot.CharacterID] != 0 {
		s.tradeMu.Unlock()
		_ = s.cleanupRobotRecycleAssets(context.Background(), recycle, topUp, nil, true)
		s.sendRobotRecycleMessages(recycle, playerID, []string{robotRecycleMessageRejected})
		return nil
	}
	for _, rejected := range plan.RejectedOffers {
		delete(trade.offers[playerID], rejected.tradeSlot)
	}
	trade.gold[recycle.Robot.CharacterID] = plan.Gold
	robotTradeSlot := currentItemTradeOfferFirst
	for _, asset := range generated {
		stack, found := s.robotRecycleGeneratedStack(context.Background(), recycle.Robot, asset)
		if !found || robotTradeSlot > currentItemTradeOfferLast {
			s.tradeMu.Unlock()
			_ = s.cleanupRobotRecycleAssets(context.Background(), recycle, topUp, nil, true)
			s.sendRobotRecycleMessages(recycle, playerID, []string{robotRecycleMessageRejected})
			return nil
		}
		trade.offers[recycle.Robot.CharacterID][robotTradeSlot] = onlineItemTradeOffer{
			tradeSlot:  robotTradeSlot,
			sourceList: dnfrepo.MainInventoryListType,
			sourceSlot: asset.Slot,
			count:      1,
			itemID:     int64(asset.ItemID),
			stack:      stack,
		}
		robotTradeSlot++
	}
	trade.states[recycle.Robot.CharacterID] = currentItemTradeAddingDone
	recycle.Gold = plan.Gold
	recycle.TopUpGold = topUp
	recycle.Generated = append([]robotRecycleGeneratedAsset(nil), generated...)
	s.tradeMu.Unlock()

	s.sendRobotRecycleMessages(recycle, playerID, plan.Messages)
	for _, rejected := range plan.RejectedOffers {
		if player, online := s.onlineGameSession(playerID); online {
			_ = s.sendGameUpperRawClassCodec(player, currentItemTradeChangeMsg, buildOnlineItemTradeRemovedEntry(rejected.tradeSlot), 0, true)
		}
		if robot, online := s.onlineGameSession(recycle.Robot.CharacterID); online {
			_ = s.sendGameUpperRawClassCodec(robot, currentItemTradeChangeMsg, buildOnlineItemTradeRemovedEntry(rejected.tradeSlot), 0, true)
		}
	}
	if player, online := s.onlineGameSession(playerID); online {
		if err := s.replayOnlineItemTradeCounterpartOffers(trade, player); err != nil {
			return err
		}
	}
	if robot, online := s.onlineGameSession(recycle.Robot.CharacterID); online {
		if err := s.replayOnlineItemTradeCounterpartOffers(trade, robot); err != nil {
			return err
		}
	}
	for _, characterID := range []uint16{playerID, recycle.Robot.CharacterID} {
		stateBody := buildOnlineItemTradeStateBody(characterID, currentItemTradeAddingDone)
		if player, online := s.onlineGameSession(playerID); online {
			if err := s.sendGameUpperRawClassCodec(player, currentItemTradeStateMsg, stateBody, 0, true); err != nil {
				return err
			}
		}
		if robot, online := s.onlineGameSession(recycle.Robot.CharacterID); online {
			if err := s.sendGameUpperRawClassCodec(robot, currentItemTradeStateMsg, stateBody, 0, true); err != nil {
				return err
			}
		}
	}
	s.logGameEvent(nil, "game-robot-recycle-quote-ready",
		"player_char_id", playerID,
		"robot_char_id", recycle.Robot.CharacterID,
		"gold", plan.Gold,
		"exchange_equipment", len(generated),
		"rejected_rows", len(plan.RejectedOffers),
		"messages", len(plan.Messages))
	return nil
}

func recycleRobotCharacterID(trade *onlineItemTrade) uint16 {
	if trade == nil || trade.recycle == nil {
		return 0
	}
	return trade.recycle.Robot.CharacterID
}

func (s *Service) planRobotRecycle(offers []onlineItemTradeOffer, policy dnfrobotdomain.RecyclePolicy) (robotRecyclePlan, error) {
	if !policy.Enabled {
		return robotRecyclePlan{Messages: []string{robotRecycleMessageRejected}}, nil
	}
	catalog, err := s.currentPVFItemCatalog()
	if err != nil {
		return robotRecyclePlan{}, err
	}
	prices := make(map[uint32]uint64, len(policy.BuyItems))
	for _, item := range policy.BuyItems {
		prices[item.ItemID] = item.Price
	}
	type classifiedOffer struct {
		offer      onlineItemTradeOffer
		definition dungeonDropItemDefinition
	}
	classified := make([]classifiedOffer, 0, len(offers))
	plan := robotRecyclePlan{}
	hasUnlistedItems := false
	for _, offer := range offers {
		if offer.itemID <= 0 || offer.itemID > math.MaxUint32 || offer.count <= 0 {
			plan.RejectedOffers = append(plan.RejectedOffers, offer)
			continue
		}
		definition, resolveErr := catalog.ResolveItem(uint32(offer.itemID))
		if resolveErr != nil || !robotRecycleOfferProtocolValid(offer) {
			plan.RejectedOffers = append(plan.RejectedOffers, offer)
			continue
		}
		classified = append(classified, classifiedOffer{offer: offer, definition: definition})
		if price := prices[definition.ItemID]; price > 0 {
			subtotal, ok := robotRecycleMultiplyPrice(price, int64(offer.count))
			if !ok {
				return robotRecyclePlan{}, errors.New("robot recycle gold amount overflow")
			}
			if plan.Gold > math.MaxInt64-subtotal {
				return robotRecyclePlan{}, errors.New("robot recycle total gold amount overflow")
			}
			plan.Gold += subtotal
			plan.GoldHit = true
		} else {
			hasUnlistedItems = true
		}
	}
	if len(classified) == 0 {
		plan.Messages = append(plan.Messages, robotRecycleMessageRejected)
		return plan, nil
	}

	if plan.GoldHit {
		for _, row := range classified {
			plan.AcceptedOffers = append(plan.AcceptedOffers, row.offer)
		}
		plan.Messages = append(plan.Messages, robotRecycleMessageFavorite)
		if hasUnlistedItems || len(plan.RejectedOffers) > 0 || len(classified) < len(offers) {
			plan.Messages = append(plan.Messages, robotRecycleMessageCheap)
		}
		plan.Accepted = true
		return plan, nil
	}

	hasMaterial := false
	equipmentSeen := 0
	for _, row := range classified {
		if row.definition.Kind != dungeonDropItemEquipment {
			hasMaterial = true
			continue
		}
		equipmentSeen++
		if row.definition.MinimumLevel < 50 && (row.definition.Rarity == 0 || row.definition.Rarity == 1) {
			reject, rollErr := s.robotRecycleRoll(100)
			if rollErr != nil {
				return robotRecyclePlan{}, rollErr
			}
			if reject < 70 {
				plan.RejectedOffers = append(plan.RejectedOffers, row.offer)
				plan.Messages = append(plan.Messages, robotRecycleMessageGoblin)
				continue
			}
		}
		candidate, found, candidateErr := s.selectRobotRecycleEquipment(catalog, row.definition, uint32(row.offer.itemID))
		if candidateErr != nil {
			return robotRecyclePlan{}, candidateErr
		}
		if !found {
			plan.RejectedOffers = append(plan.RejectedOffers, row.offer)
			plan.Messages = append(plan.Messages, robotRecycleMessageRejected)
			continue
		}
		plan.AcceptedOffers = append(plan.AcceptedOffers, row.offer)
		plan.Equipments = append(plan.Equipments, candidate)
	}
	if equipmentSeen == 0 || len(plan.AcceptedOffers) == 0 {
		plan.AcceptedOffers = nil
		plan.Equipments = nil
		if len(plan.Messages) == 0 {
			plan.Messages = append(plan.Messages, robotRecycleMessageRejected)
		}
		return plan, nil
	}
	for _, row := range classified {
		if row.definition.Kind != dungeonDropItemEquipment {
			plan.AcceptedOffers = append(plan.AcceptedOffers, row.offer)
		}
	}
	if hasMaterial {
		plan.Messages = append(plan.Messages, robotRecycleMessageMaterialTaken)
	}
	plan.Accepted = true
	return plan, nil
}

func robotRecycleOfferProtocolValid(offer onlineItemTradeOffer) bool {
	stack := offer.stack
	if stack.ItemID <= 0 || stack.Count <= 0 || stack.Bind ||
		(!stack.ExpireAt.IsZero() && !stack.ExpireAt.After(time.Now())) ||
		currentDungeonDiscardStackLocked(stack) {
		return false
	}
	return true
}

func robotRecycleMultiplyPrice(price uint64, count int64) (int64, bool) {
	if price == 0 || count <= 0 || price > math.MaxInt64 || uint64(count) > uint64(math.MaxInt64)/price {
		return 0, false
	}
	return int64(price) * count, true
}

func (s *Service) selectRobotRecycleEquipment(
	catalog *pvfDungeonDropCatalog,
	input dungeonDropItemDefinition,
	inputItemID uint32,
) (dungeonDropItemDefinition, bool, error) {
	levelOffset, err := s.robotRecycleRoll(100)
	if err != nil {
		return dungeonDropItemDefinition{}, false, err
	}
	targetLevel := input.MinimumLevel
	switch {
	case levelOffset < 40:
		targetLevel -= 5
	case levelOffset >= 70:
		targetLevel += 5
	}
	rarityOffset, err := s.robotRecycleRoll(100)
	if err != nil {
		return dungeonDropItemDefinition{}, false, err
	}
	targetRarity := input.Rarity
	switch {
	case rarityOffset < 10:
		targetRarity++
	case rarityOffset >= 50:
		targetRarity--
	}
	type target struct{ level, rarity int64 }
	targets := []target{
		{targetLevel, targetRarity},
		{input.MinimumLevel, targetRarity},
		{input.MinimumLevel, input.Rarity},
	}
	seen := make(map[target]struct{}, len(targets))
	for _, candidateTarget := range targets {
		if _, duplicate := seen[candidateTarget]; duplicate {
			continue
		}
		seen[candidateTarget] = struct{}{}
		candidates, candidateErr := catalog.robotRecycleEquipmentCandidates(candidateTarget.level, candidateTarget.rarity, inputItemID)
		if candidateErr != nil {
			return dungeonDropItemDefinition{}, false, candidateErr
		}
		if len(candidates) == 0 {
			continue
		}
		index, rollErr := s.robotRecycleRoll(len(candidates))
		if rollErr != nil {
			return dungeonDropItemDefinition{}, false, rollErr
		}
		return candidates[index], true, nil
	}
	return dungeonDropItemDefinition{}, false, nil
}

func (c *pvfDungeonDropCatalog) robotRecycleEquipmentCandidates(
	level int64,
	rarity int64,
	excludeItemID uint32,
) ([]dungeonDropItemDefinition, error) {
	if c == nil || c.source == nil || level < 1 || level > 200 || rarity < 0 || rarity > 6 {
		return nil, nil
	}
	c.mu.Lock()
	loaded := c.genericLoaded
	genericIDs := make([]uint32, 0)
	if loaded {
		for _, candidate := range c.genericEquipment[dungeonDropGradeRarityKey{Grade: level, Rarity: rarity}] {
			if candidate.ItemID != excludeItemID {
				genericIDs = append(genericIDs, candidate.ItemID)
			}
		}
	}
	c.mu.Unlock()
	if !loaded {
		if err := c.ensureGenericPools(); err != nil {
			return nil, err
		}
		return c.robotRecycleEquipmentCandidates(level, rarity, excludeItemID)
	}
	if preferred := robotRecyclePreferredAttachDefinitions(
		c.robotRecycleResolvedCandidates(genericIDs, level, rarity, excludeItemID),
	); len(preferred) > 0 {
		return preferred, nil
	}

	if err := c.ensureRobotRecycleEquipmentPool(); err != nil {
		return nil, err
	}
	c.mu.Lock()
	fullIDs := append([]uint32(nil), c.robotRecycleEquipment[dungeonDropGradeRarityKey{Grade: level, Rarity: rarity}]...)
	c.mu.Unlock()
	return robotRecyclePreferredAttachDefinitions(
		c.robotRecycleResolvedCandidates(fullIDs, level, rarity, excludeItemID),
	), nil
}

func robotRecyclePreferredAttachDefinitions(all []dungeonDropItemDefinition) []dungeonDropItemDefinition {
	groups := make([][]dungeonDropItemDefinition, 3)
	for _, definition := range all {
		switch normalizeMagicBoxPVFType(definition.AttachType) {
		case "free":
			groups[0] = append(groups[0], definition)
		case "sealing":
			groups[1] = append(groups[1], definition)
		case "trade":
			groups[2] = append(groups[2], definition)
		}
	}
	for _, group := range groups {
		if len(group) > 0 {
			return group
		}
	}
	return nil
}

func (c *pvfDungeonDropCatalog) robotRecycleResolvedCandidates(
	ids []uint32,
	level int64,
	rarity int64,
	excludeItemID uint32,
) []dungeonDropItemDefinition {
	ids = append([]uint32(nil), ids...)
	sort.Slice(ids, func(i, j int) bool { return ids[i] < ids[j] })
	result := make([]dungeonDropItemDefinition, 0, len(ids))
	for _, itemID := range ids {
		if itemID == excludeItemID {
			continue
		}
		definition, err := c.ResolveItem(itemID)
		if err != nil || definition.Kind != dungeonDropItemEquipment || !robotRecycleOutputAttachAllowed(definition.AttachType) ||
			definition.MinimumLevel != level || definition.Rarity != rarity {
			continue
		}
		result = append(result, definition)
	}
	return result
}

func robotRecycleOutputAttachAllowed(attach string) bool {
	switch normalizeMagicBoxPVFType(attach) {
	case "free", "sealing", "trade":
		return true
	default:
		return false
	}
}

// ensureRobotRecycleEquipmentPool scans equipment.lst independently from the
// monster drop pool. The latter intentionally only contains monster-droppable
// templates; high-rarity client equipment can still exist as a normal free
// trade template outside that set. The scan is lazy and cached because the
// first full scan reads the complete equipment list.
func (c *pvfDungeonDropCatalog) ensureRobotRecycleEquipmentPool() error {
	if c == nil || c.source == nil {
		return errDungeonDropSourceRequired
	}
	c.mu.Lock()
	if c.robotRecycleLoaded {
		err := c.robotRecycleErr
		c.mu.Unlock()
		return err
	}
	c.robotRecycleLoaded = true
	c.robotRecycleEquipment = make(map[dungeonDropGradeRarityKey][]uint32)
	ids := append([]uint32(nil), c.equipmentIDs...)
	c.mu.Unlock()

	workers := runtime.GOMAXPROCS(0)
	if workers > dungeonDropEquipmentPoolMaxWorkers {
		workers = dungeonDropEquipmentPoolMaxWorkers
	}
	if workers > len(ids) {
		workers = len(ids)
	}
	results := make([]robotRecycleEquipmentCandidateScan, len(ids))
	jobs := make(chan int)
	var wait sync.WaitGroup
	for worker := 0; worker < workers; worker++ {
		wait.Add(1)
		go func() {
			defer wait.Done()
			for index := range jobs {
				results[index] = c.scanRobotRecycleEquipmentCandidate(ids[index])
			}
		}()
	}
	for index := range ids {
		jobs <- index
	}
	close(jobs)
	wait.Wait()

	c.mu.Lock()
	for _, result := range results {
		if result.OK {
			c.robotRecycleEquipment[result.Key] = append(c.robotRecycleEquipment[result.Key], result.ItemID)
		}
	}
	c.mu.Unlock()
	return nil
}

func (c *pvfDungeonDropCatalog) scanRobotRecycleEquipmentCandidate(itemID uint32) robotRecycleEquipmentCandidateScan {
	reference, found := c.itemRefs[itemID]
	if !found || reference.kind != dungeonDropItemEquipment {
		return robotRecycleEquipmentCandidateScan{ItemID: itemID}
	}
	docPath, text, err := readDungeonDropText(c.source, string(dungeonDropItemEquipment), reference.path)
	if err != nil {
		return robotRecycleEquipmentCandidateScan{ItemID: itemID}
	}
	document, err := dnfpvf.Parse(docPath, text)
	if err != nil {
		return robotRecycleEquipmentCandidateScan{ItemID: itemID}
	}
	rarity, found := document.Int("rarity")
	if !found || rarity < 0 || rarity > 6 {
		return robotRecycleEquipmentCandidateScan{ItemID: itemID}
	}
	level, found := document.Int("minimum level")
	if !found || level < 1 || level > 200 {
		return robotRecycleEquipmentCandidateScan{ItemID: itemID}
	}
	attach, _ := document.Text("attach type")
	if !robotRecycleOutputAttachAllowed(attach) {
		return robotRecycleEquipmentCandidateScan{ItemID: itemID}
	}
	equipmentType, _ := document.Text("equipment type")
	if dungeonDropEquipmentPoolExcludedType(equipmentType) {
		return robotRecycleEquipmentCandidateScan{ItemID: itemID}
	}
	return robotRecycleEquipmentCandidateScan{
		ItemID: itemID,
		Key:    dungeonDropGradeRarityKey{Grade: level, Rarity: rarity},
		OK:     true,
	}
}

func (s *Service) prepareRobotRecycleAssets(
	ctx context.Context,
	recycle *robotRecycleTrade,
	gold int64,
	equipments []dungeonDropItemDefinition,
) ([]robotRecycleGeneratedAsset, int64, error) {
	if recycle == nil || len(equipments) > robotRecycleMaximumTradeOfferRows {
		return nil, 0, errors.New("robot recycle equipment count is invalid")
	}
	repositories, ok := s.repositoryGroup()
	if !ok || repositories.CharacterAssets == nil {
		return nil, 0, errors.New("robot recycle asset repository is unavailable")
	}
	characterID := strconv.Itoa(int(recycle.Robot.CharacterID))
	generated := make([]robotRecycleGeneratedAsset, 0, len(equipments))
	topUp := int64(0)
	err := repositories.CharacterAssets.WithinCharacterAssets(ctx, characterID, func(
		characters dnfrepo.CharacterRepository,
		inventories dnfrepo.InventoryRepository,
		_ dnfrepo.EquipmentRepository,
	) error {
		character, found, err := characters.Load(ctx, characterID)
		if err != nil {
			return err
		}
		if !found || strings.TrimSpace(character.AccountID) != recycle.Robot.AccountID {
			return errors.New("robot recycle character/account mismatch")
		}
		character = dnfrepo.CloneCharacter(character)
		if character.Stats == nil {
			character.Stats = make(map[string]int64)
		}
		if gold > character.Stats["gold"] {
			topUp = gold - character.Stats["gold"]
			character.Stats["gold"] = gold
		}
		inventory, found, err := inventories.Load(ctx, characterID)
		if err != nil {
			return err
		}
		if !found {
			return errors.New("robot recycle inventory is unavailable")
		}
		inventory = dnfrepo.CloneInventory(inventory)
		if inventory.Slots == nil {
			inventory.Slots = make(map[string]dnfrepo.ItemStack)
		}
		for _, definition := range equipments {
			slot, err := addCurrentDungeonPickupToInventory(&inventory, definition, 1)
			if err != nil {
				return fmt.Errorf("place robot recycle equipment %d: %w", definition.ItemID, err)
			}
			if slot < 0 || int(slot) > math.MaxInt16 {
				return errors.New("robot recycle equipment slot is invalid")
			}
			key := fmt.Sprintf("%d:%d", dnfrepo.MainInventoryListType, slot)
			stack, found := inventory.Slots[key]
			if !found || stack.ItemID != int64(definition.ItemID) {
				return errors.New("robot recycle equipment placement disappeared")
			}
			if stack.Extra == nil {
				stack.Extra = make(map[string]string)
			}
			stack.Extra[robotRecycleAssetMarker] = recycle.Token
			currentRefreshStackRawEntry(&stack, dnfrepo.MainInventoryListType, int16(slot))
			inventory.Slots[key] = stack
			generated = append(generated, robotRecycleGeneratedAsset{Slot: int16(slot), ItemID: definition.ItemID})
		}
		if topUp > 0 {
			if err := dnfrepo.SaveCharacterFields(ctx, characters, character, dnfrepo.CharacterFieldStats); err != nil {
				return err
			}
		}
		if len(generated) > 0 {
			if err := dnfrepo.SaveInventoryFields(ctx, inventories, inventory, dnfrepo.InventoryFieldSlots); err != nil {
				return err
			}
		}
		return nil
	})
	if err != nil {
		return nil, 0, err
	}
	return generated, topUp, nil
}

func (s *Service) robotRecycleGeneratedStack(ctx context.Context, profile dnfrobotdomain.Profile, asset robotRecycleGeneratedAsset) (dnfrepo.ItemStack, bool) {
	repositories, ok := s.repositoryGroup()
	if !ok || repositories.Inventory == nil {
		return dnfrepo.ItemStack{}, false
	}
	inventory, found, err := repositories.Inventory.Load(ctx, strconv.Itoa(int(profile.CharacterID)))
	if err != nil || !found {
		return dnfrepo.ItemStack{}, false
	}
	stack, found := inventory.Slots[fmt.Sprintf("%d:%d", dnfrepo.MainInventoryListType, asset.Slot)]
	return stack, found && stack.ItemID == int64(asset.ItemID)
}

func (s *Service) completeRobotRecycleTrade(
	ctx context.Context,
	trade *onlineItemTrade,
	result itemtrade.Result,
) error {
	if trade == nil || trade.recycle == nil {
		return nil
	}
	received := result.Received[strconv.Itoa(int(trade.recycle.Robot.CharacterID))]
	return s.cleanupRobotRecycleAssets(ctx, trade.recycle, 0, received, true)
}

func (s *Service) rollbackRobotRecycleTrade(trade *onlineItemTrade) {
	if trade == nil || trade.recycle == nil {
		return
	}
	ctx, cancel := context.WithTimeout(context.Background(), currentRepositorySnapshotTimeout)
	defer cancel()
	if err := s.cleanupRobotRecycleAssets(ctx, trade.recycle, trade.recycle.TopUpGold, nil, true); err != nil {
		s.logWarn("dnfbridge robot recycle rollback failed",
			"robot_char_id", trade.recycle.Robot.CharacterID, "error", err)
	}
	s.endRobotRecycle(trade.recycle.Robot.ID)
}

func (s *Service) cleanupRobotRecycleAssets(
	ctx context.Context,
	recycle *robotRecycleTrade,
	rollbackGold int64,
	received []itemtrade.Transfer,
	removeGenerated bool,
) error {
	if recycle == nil {
		return nil
	}
	repositories, ok := s.repositoryGroup()
	if !ok || repositories.CharacterAssets == nil {
		return errors.New("robot recycle cleanup repository is unavailable")
	}
	characterID := strconv.Itoa(int(recycle.Robot.CharacterID))
	err := repositories.CharacterAssets.WithinCharacterAssets(ctx, characterID, func(
		characters dnfrepo.CharacterRepository,
		inventories dnfrepo.InventoryRepository,
		_ dnfrepo.EquipmentRepository,
	) error {
		inventory, found, err := inventories.Load(ctx, characterID)
		if err != nil {
			return err
		}
		if !found {
			return nil
		}
		inventory = dnfrepo.CloneInventory(inventory)
		changed := false
		if removeGenerated {
			for _, asset := range recycle.Generated {
				key := fmt.Sprintf("%d:%d", dnfrepo.MainInventoryListType, asset.Slot)
				stack, found := inventory.Slots[key]
				if found && stack.ItemID == int64(asset.ItemID) &&
					strings.TrimSpace(stack.Extra[robotRecycleAssetMarker]) == recycle.Token {
					delete(inventory.Slots, key)
					changed = true
				}
			}
		}
		for _, transfer := range received {
			if dnfrepo.IsAccountSharedInventorySlot(transfer.DestinationList, transfer.DestinationSlot) {
				continue
			}
			key := fmt.Sprintf("%d:%d", transfer.DestinationList, transfer.DestinationSlot)
			stack, found := inventory.Slots[key]
			if found && stack.ItemID == transfer.Stack.ItemID && stack.Count >= transfer.Stack.Count {
				if stack.Count == transfer.Stack.Count {
					delete(inventory.Slots, key)
				} else {
					stack.Count -= transfer.Stack.Count
					inventory.Slots[key] = stack
				}
				changed = true
			}
		}
		if changed {
			if err := dnfrepo.SaveInventoryFields(ctx, inventories, inventory, dnfrepo.InventoryFieldSlots); err != nil {
				return err
			}
		}
		if rollbackGold > 0 {
			character, found, err := characters.Load(ctx, characterID)
			if err != nil {
				return err
			}
			if found {
				character = dnfrepo.CloneCharacter(character)
				if character.Stats == nil {
					character.Stats = make(map[string]int64)
				}
				balance := character.Stats["gold"]
				if rollbackGold > balance {
					rollbackGold = balance
				}
				if rollbackGold > 0 {
					character.Stats["gold"] = balance - rollbackGold
					if err := dnfrepo.SaveCharacterFields(ctx, characters, character, dnfrepo.CharacterFieldStats); err != nil {
						return err
					}
				}
			}
		}
		return nil
	})
	if err != nil {
		return err
	}
	return s.cleanupRobotRecycleAccountReceived(ctx, recycle, received)
}

func (s *Service) cleanupRobotRecycleAccountReceived(ctx context.Context, recycle *robotRecycleTrade, received []itemtrade.Transfer) error {
	repositories, ok := s.repositoryGroup()
	if !ok || repositories.AccountInventory == nil {
		return nil
	}
	hasAccountRows := false
	for _, transfer := range received {
		if dnfrepo.IsAccountSharedInventorySlot(transfer.DestinationList, transfer.DestinationSlot) {
			hasAccountRows = true
			break
		}
	}
	if !hasAccountRows {
		return nil
	}
	record, found, err := repositories.AccountInventory.Load(ctx, recycle.Robot.AccountID)
	if err != nil || !found {
		return err
	}
	record = dnfrepo.CloneAccountInventory(record)
	changed := false
	for _, transfer := range received {
		if !dnfrepo.IsAccountSharedInventorySlot(transfer.DestinationList, transfer.DestinationSlot) {
			continue
		}
		key := fmt.Sprintf("%d:%d", transfer.DestinationList, transfer.DestinationSlot)
		stack, found := record.Slots[key]
		if !found || stack.ItemID != transfer.Stack.ItemID || stack.Count < transfer.Stack.Count {
			continue
		}
		if stack.Count == transfer.Stack.Count {
			delete(record.Slots, key)
		} else {
			stack.Count -= transfer.Stack.Count
			record.Slots[key] = stack
		}
		changed = true
	}
	if changed {
		return repositories.AccountInventory.Save(ctx, record)
	}
	return nil
}

func (s *Service) sendRobotRecycleMessages(recycle *robotRecycleTrade, playerID uint16, messages []string) {
	if recycle == nil || playerID == 0 || len(messages) == 0 {
		return
	}
	player, online := s.onlineGameSession(playerID)
	if !online || player == nil {
		return
	}
	repositories, ok := s.repositoryGroup()
	if !ok || repositories.Character == nil {
		return
	}
	ctx, cancel := context.WithTimeout(context.Background(), currentRepositorySnapshotTimeout)
	character, found, err := repositories.Character.Load(ctx, strconv.Itoa(int(recycle.Robot.CharacterID)))
	cancel()
	if err != nil || !found || strings.TrimSpace(character.Name) == "" {
		character.Name = recycle.Robot.ID
	}
	state := buildCurrentPeerChatStateBody(recycle.Robot.CharacterID, currentTownChatWhisperMode)
	_ = s.sendGameUpperRawClassCodec(player, currentPeerChatStateOpcode, state, 0, true)
	for _, message := range messages {
		text := encodeCurrentLocationHotkeyText(message)
		notice := buildCurrentGuildChatNoticeBody(
			currentTownChatWhisperMode,
			currentChatServerGroup,
			byte(townPresenceChannelID(player)),
			0,
			rosterNameBytes(character.Name),
			text,
		)
		if sendErr := s.sendGameUpperRawClassCodec(player, currentPeerChatNotifyOpcode, notice, 0, true); sendErr != nil {
			s.logInfo("dnfbridge robot recycle private message failed", "player_char_id", playerID, "error", sendErr)
			return
		}
	}
}

func (s *Service) robotRecycleRoll(limit int) (int, error) {
	if limit <= 0 {
		return 0, errors.New("robot recycle roll limit is invalid")
	}
	if s != nil && s.robotRecycleRollFn != nil {
		return s.robotRecycleRollFn(limit)
	}
	value, err := rand.Int(rand.Reader, big.NewInt(int64(limit)))
	if err != nil {
		return 0, err
	}
	return int(value.Int64()), nil
}

func newRobotRecycleToken() string {
	var raw [16]byte
	if _, err := rand.Read(raw[:]); err != nil {
		return fmt.Sprintf("robot-recycle-%d", time.Now().UnixNano())
	}
	return fmt.Sprintf("robot-recycle-%x", raw[:])
}

func robotRecycleMessagesContain(messages []string, want string) bool {
	for _, message := range messages {
		if message == want {
			return true
		}
	}
	return false
}
