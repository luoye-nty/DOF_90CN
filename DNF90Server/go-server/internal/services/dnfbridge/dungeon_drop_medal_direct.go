package dnfbridge

import (
	"context"
	"errors"
	"fmt"
	"math"
	"strconv"
	"strings"

	dnfdungeon "longheng.io/server/internal/modules/dnf/dungeon"
	dnfrepo "longheng.io/server/internal/modules/dnf/repository"
)

const (
	currentDungeonDirectMedalDropSource = "dungeon_direct_medal_drop"
	currentDungeonDirectMedalMailTitle  = "公会副本勋章奖励"
	currentDungeonDirectMedalMailBody   = "勋章栏空间不足，本次公会副本掉落的勋章已随本邮件发送。请清理勋章栏后领取。"
)

var (
	errCurrentDungeonDirectMedalDropInvalid  = errors.New("dungeon direct medal drop is invalid")
	errCurrentDungeonDirectMedalDropDelivery = errors.New("dungeon direct medal drop delivery is unavailable")
	currentDungeonDirectMedalGrantKeyExtra   = dnfdungeon.DirectContainerDropGrantKeyExtra()
)

// currentDungeonDirectMedalDrop is a [flag] guild medal selected by the real
// PVF drop roll. It deliberately has no scene object: the current client's
// flag-pickup branch never submits op43, so the medal is delivered directly to
// list 38 instead.
type currentDungeonDirectMedalDrop struct {
	Definition  dungeonDropItemDefinition
	Amount      uint32
	QualitySeed uint32
}

type currentDungeonDirectMedalDelivery struct {
	Entries         []currentItemListEntry
	MailID          string
	MailAttachments int
	Reused          bool
}

func currentDungeonDirectGuildMedalDrop(definition dungeonDropItemDefinition) bool {
	return definition.Kind == dungeonDropItemEquipment &&
		normalizeEquipmentPlacementPVFType(definition.EquipmentType) == "[flag]"
}

func currentDungeonDirectMedalDropEventKey(
	runtime *runtimeDungeonState,
	roomLabel string,
	monsterObjectKey uint32,
) (string, error) {
	if runtime == nil || runtime.lifecycleToken == 0 || monsterObjectKey == 0 {
		return "", errCurrentDungeonDirectMedalDropInvalid
	}
	roomLabel = strings.TrimSpace(roomLabel)
	if roomLabel == "" {
		return "", errCurrentDungeonDirectMedalDropInvalid
	}
	return fmt.Sprintf(
		"%s:run:%d:dungeon:%d:maze:%d:room:%s:monster:%d",
		currentDungeonDirectMedalDropSource,
		runtime.lifecycleToken,
		runtime.Dungeon.ID,
		runtime.MazeIndex,
		roomLabel,
		monsterObjectKey,
	), nil
}

// grantCurrentDungeonDirectMedalDrops commits the selected medals either to the
// character's list-38 medal page or, when that page cannot hold the complete
// reward batch, to one idempotent system mail. Inventory and mailbox live in
// the same transaction so a medal can never be duplicated or lost at the
// hand-off boundary.
func (s *Service) grantCurrentDungeonDirectMedalDrops(
	ctx context.Context,
	session *gameSession,
	runtime *runtimeDungeonState,
	roomLabel string,
	monsterObjectKey uint32,
	drops []currentDungeonDirectMedalDrop,
) (currentDungeonDirectMedalDelivery, error) {
	var result currentDungeonDirectMedalDelivery
	if s == nil || session == nil || runtime == nil || session.selectedCharacterID == 0 || len(drops) == 0 {
		return result, errCurrentDungeonDirectMedalDropInvalid
	}
	repositories, ok := s.repositoryGroup()
	if !ok || repositories.Inventory == nil || repositories.Mailbox == nil || repositories.MailboxAssets == nil {
		return result, errCurrentDungeonDirectMedalDropDelivery
	}
	eventKey, err := currentDungeonDirectMedalDropEventKey(runtime, roomLabel, monsterObjectKey)
	if err != nil {
		return result, err
	}
	grantKeys := make([]string, len(drops))
	for index, drop := range drops {
		if !currentDungeonDirectGuildMedalDrop(drop.Definition) || drop.Amount == 0 {
			return result, errCurrentDungeonDirectMedalDropInvalid
		}
		grantKeys[index] = eventKey + ":item:" + strconv.Itoa(index)
	}
	if ctx == nil {
		ctx = context.Background()
	}

	characterID := strconv.Itoa(int(session.selectedCharacterID))
	owner, err := dnfdungeon.NewOwner(repositories)
	if err != nil {
		return result, err
	}
	items := make([]dnfdungeon.DirectContainerDropItem, 0, len(drops))
	for index, drop := range drops {
		start, end, isMedal := currentGuildMedalListPlacement(drop.Definition)
		if !isMedal || start != currentGuildMedalPageSlotStart || end != currentGuildMedalPageSlotEnd {
			return result, errCurrentDungeonDirectMedalDropInvalid
		}
		stack, buildErr := currentDungeonDirectMedalStack(drop, grantKeys[index])
		if buildErr != nil {
			return result, buildErr
		}
		items = append(items, dnfdungeon.DirectContainerDropItem{
			Stack:    stack,
			GrantKey: grantKeys[index],
		})
	}
	delivery, err := owner.GrantDirectContainerDrops(ctx, dnfdungeon.DirectContainerDropCommand{
		CharacterID:   characterID,
		ListType:      currentGuildMedalInventoryListType,
		SlotStart:     currentGuildMedalPageSlotStart,
		SlotEnd:       currentGuildMedalPageSlotEnd,
		CompletionKey: eventKey,
		MailTitle:     currentDungeonDirectMedalMailTitle,
		MailBody:      currentDungeonDirectMedalMailBody,
		Source:        currentDungeonDirectMedalDropSource,
		Items:         items,
		Project: func(slot int16, stack dnfrepo.ItemStack) (dnfrepo.ItemStack, error) {
			entry := currentItemListEntryFromStack(currentGuildMedalInventoryListType, slot, stack)
			stack.RawEntry = append([]byte(nil), entry.data[:]...)
			return stack, nil
		},
	})
	if err != nil {
		return result, err
	}
	result.MailID = delivery.MailID
	result.MailAttachments = delivery.MailAttachments
	result.Reused = delivery.Replayed
	for _, item := range delivery.Items {
		result.Entries = append(result.Entries, currentItemListEntryFromStack(
			currentGuildMedalInventoryListType,
			item.Slot,
			item.Stack,
		))
	}
	return result, nil
}

func currentDungeonDirectMedalStack(
	drop currentDungeonDirectMedalDrop,
	grantKey string,
) (dnfrepo.ItemStack, error) {
	if !currentDungeonDirectGuildMedalDrop(drop.Definition) || drop.Amount == 0 || grantKey == "" {
		return dnfrepo.ItemStack{}, errCurrentDungeonDirectMedalDropInvalid
	}
	stack, err := buildCurrentDungeonPickupStack(
		drop.Definition.SlotStart,
		drop.Definition,
		drop.Amount,
		drop.QualitySeed,
	)
	if err != nil {
		return dnfrepo.ItemStack{}, err
	}
	if stack.Extra == nil {
		stack.Extra = make(map[string]string, 5)
	}
	stack.Extra["source"] = currentDungeonDirectMedalDropSource
	stack.Extra["last_grant_source"] = currentDungeonDirectMedalDropSource
	stack.Extra[currentDungeonDirectMedalGrantKeyExtra] = grantKey
	return stack, nil
}

// appendCurrentDungeonDirectMedalGroundFallback is only used when the direct
// delivery transaction cannot start or commit. The normal server path never
// reaches this helper, but keeping the medal as a scene drop is safer than
// silently losing a PVF reward.
func (s *Service) appendCurrentDungeonDirectMedalGroundFallback(
	runtime *runtimeDungeonState,
	ownerActorObjectKey uint16,
	drops []currentDungeonDirectMedalDrop,
) ([]currentDungeonDeathDropWire, error) {
	if runtime == nil || runtime.Session == nil || runtime.Room == nil || ownerActorObjectKey == 0 ||
		len(drops) == 0 {
		return nil, errCurrentDungeonDirectMedalDropInvalid
	}
	scene, ok := runtime.Session.Scene()
	if !ok {
		return nil, errDungeonWorldMapUnavailable
	}
	owner := runtime.DropOwner
	if owner == nil {
		owner = newRuntimeDungeonDropOwner()
	}
	nextObjectKey := owner.nextObjectKeyAtOrAbove(runtime.NextObjectKey)
	count := uint32(len(drops))
	if nextObjectKey == 0 || nextObjectKey > math.MaxUint16 ||
		count > math.MaxUint16-nextObjectKey {
		return nil, errDungeonDropObjectKeyRange
	}
	if owner.nextSceneSlot == 0 || uint32(owner.nextSceneSlot)+count > math.MaxUint16 {
		return nil, errDungeonDropSceneSlotRange
	}

	roomKey := runtimeDungeonRoomKeyFromScene(scene)
	nextSceneSlot := owner.nextSceneSlot
	runtimeDrops := make([]*runtimeDungeonDrop, 0, count)
	wires := make([]currentDungeonDeathDropWire, 0, count)
	for _, reward := range drops {
		itemState, err := currentDungeonDeathDropItemState(
			nextSceneSlot,
			reward.Definition,
			reward.Amount,
			reward.QualitySeed,
		)
		if err != nil {
			return nil, err
		}
		drop := &runtimeDungeonDrop{
			ObjectKey:           nextObjectKey,
			SceneSlot:           nextSceneSlot,
			Item:                reward.Definition,
			Amount:              reward.Amount,
			QualitySeed:         reward.QualitySeed,
			OwnerActorObjectKey: ownerActorObjectKey,
			Room:                roomKey,
			Status:              runtimeDungeonDropAvailable,
		}
		runtimeDrops = append(runtimeDrops, drop)
		wires = append(wires, currentDungeonDeathDropWire{
			SceneObjectKey:      drop.ObjectKey,
			Item:                itemState,
			UnknownTailSentinel: math.MaxUint16,
			OwnerActorObjectKey: ownerActorObjectKey,
		})
		nextObjectKey++
		nextSceneSlot++
	}
	if err := owner.registerBatch(runtimeDrops, nextSceneSlot); err != nil {
		return nil, err
	}
	runtime.DropOwner = owner
	runtime.NextObjectKey = nextObjectKey
	return wires, nil
}
