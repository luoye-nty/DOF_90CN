package dnfbridge

import (
	"context"
	"encoding/binary"
	"errors"
	"fmt"
	"math"
	"strconv"
	"strings"
	"time"

	"longheng.io/server/internal/modules/dnf/dnfenum"
	dnfdungeon "longheng.io/server/internal/modules/dnf/dungeon"
	dnfrepo "longheng.io/server/internal/modules/dnf/repository"
)

const (
	currentDungeonDiscardOpcode          uint16 = uint16(dnfenum.CmdPacketDropItem)
	currentDungeonDiscardSceneOpcode     uint16 = 40
	currentDungeonDiscardSceneBodySize          = 2 + 2 + 2 + 4 + currentItemListEntryWireSize + 1 + 2
	currentDungeonDiscardRequestSize            = 2 + 2 + 1 + 2 + 4 + 1
	currentDungeonDiscardAckSize                = 1 + 2 + 4
	currentDungeonDiscardSceneMode              = byte(0)
	currentDungeonDiscardUnscopedContext        = uint16(0)
)

var (
	errCurrentDungeonDiscardMalformed        = errors.New("current dungeon discard request is malformed")
	errCurrentDungeonDiscardRuntimeMissing   = errors.New("current dungeon discard runtime is missing")
	errCurrentDungeonDiscardTransaction      = errors.New("current dungeon discard transaction is unavailable")
	errCurrentDungeonDiscardInventoryMissing = errors.New("current dungeon discard inventory is missing")
	errCurrentDungeonDiscardItemMismatch     = errors.New("current dungeon discard item does not match the authoritative inventory")
	errCurrentDungeonDiscardCountInvalid     = errors.New("current dungeon discard count is invalid")
	errCurrentDungeonDiscardNotTradeable     = errors.New("current dungeon discard item is not tradeable")
	errCurrentDungeonDiscardItemLocked       = errors.New("current dungeon discard item is locked")
	errCurrentDungeonDiscardItemExpired      = errors.New("current dungeon discard item is expired")
	errCurrentDungeonDiscardRestoreConflict  = errors.New("current dungeon discarded item cannot be restored to its source slot")
)

type currentDungeonDiscardRequest struct {
	PositionX  uint16
	PositionY  uint16
	ListType   byte
	SourceSlot int16
	Count      uint32
	SceneMode  byte
	AckPayload []byte
}

type runtimeDungeonDiscardOrigin struct {
	ListType     byte
	SourceSlot   int16
	AccountOwned bool
	Stack        dnfrepo.ItemStack
}

// parseCurrentDungeonDiscardRequest 解析当前客户端的 op47 丢弃请求。
//
// body[11] 曾经被当成「必须为 0 的 mode」硬校验（常量
// currentDungeonDiscardRequestMode）。线上取证证明这一位不是常量：负重上限被
// 顶破时客户端仍允许拾取，随后立刻把刚越线的那一件装备丢回地面，这一版 op47
// 的 body[11] 就是 1（packet_log 里三次 discard-blocked 全是
// `list=0 slot=… count=1 mode=1`，而唯一一次 `mode=0` 的成功丢弃发生在未超重
// 状态）。硬校验等于把整包判 malformed，而 malformed 分支既不落库也不回包，
// 客户端于是表现为「超重后丢不掉、且没有任何提示」。
//
// 这一位在服务端没有消费者：request.SceneMode 解析出来之后没有下游读者，op40
// 场景包的模式字节由 buildCurrentDungeonDiscardSceneBody 的调用方传常量
// currentDungeonDiscardSceneMode(0)（见 handleCurrentDungeonDiscard）。所以放行
// 这一位不改变任何出站字节，也不新增代码路径——被放行的请求仍要过完全相同的
// 权威校验（容器/格位存在、PVF 可交易、未锁定、未过期、数量匹配）。
// 这里只把它原样记进 SceneMode 供日志取证，不参与任何判定。
func parseCurrentDungeonDiscardRequest(body []byte) (currentDungeonDiscardRequest, error) {
	if len(body) != currentDungeonDiscardRequestSize {
		return currentDungeonDiscardRequest{}, fmt.Errorf("%w: body_len=%d", errCurrentDungeonDiscardMalformed, len(body))
	}
	listType := body[4]
	sourceSlot := binary.LittleEndian.Uint16(body[5:7])
	count := binary.LittleEndian.Uint32(body[7:11])
	sceneMode := body[11]
	if listType != dnfrepo.MainInventoryListType ||
		sourceSlot > math.MaxInt16 ||
		count == 0 {
		return currentDungeonDiscardRequest{}, fmt.Errorf(
			"%w: list=%d slot=%d count=%d mode=%d",
			errCurrentDungeonDiscardMalformed,
			listType,
			sourceSlot,
			count,
			sceneMode,
		)
	}
	ackPayload := make([]byte, currentDungeonDiscardAckSize)
	ackPayload[0] = listType
	binary.LittleEndian.PutUint16(ackPayload[1:3], sourceSlot)
	binary.LittleEndian.PutUint32(ackPayload[3:7], count)
	return currentDungeonDiscardRequest{
		PositionX:  binary.LittleEndian.Uint16(body[0:2]),
		PositionY:  binary.LittleEndian.Uint16(body[2:4]),
		ListType:   listType,
		SourceSlot: int16(sourceSlot),
		Count:      count,
		SceneMode:  sceneMode,
		AckPayload: ackPayload,
	}, nil
}

func buildCurrentDungeonDiscardSceneBody(
	actorObjectKey uint16,
	positionX uint16,
	positionY uint16,
	objectKey uint32,
	sceneSlot uint16,
	stack dnfrepo.ItemStack,
	definition dungeonDropItemDefinition,
	amount uint32,
	sceneMode byte,
) ([]byte, currentItemListEntry, error) {
	if actorObjectKey == 0 || objectKey == 0 || sceneSlot == 0 ||
		stack.ItemID <= 0 || uint64(stack.ItemID) > math.MaxUint32 ||
		uint32(stack.ItemID) != definition.ItemID || amount == 0 ||
		sceneMode != currentDungeonDiscardSceneMode {
		return nil, currentItemListEntry{}, errCurrentDungeonDiscardMalformed
	}
	entry := currentItemListEntryFromStack(dnfrepo.MainInventoryListType, int16(sceneSlot), stack)
	instanceValue := amount
	if definition.Kind == dungeonDropItemEquipment {
		if amount != 1 {
			return nil, currentItemListEntry{}, errCurrentDungeonDiscardCountInvalid
		}
		instanceValue = binary.LittleEndian.Uint32(entry.data[0x06:0x0A])
		if instanceValue == 0 {
			return nil, currentItemListEntry{}, errDungeonDeathDropItemInvalid
		}
	}
	entry.patchCore(int16(sceneSlot), definition.ItemID, instanceValue)

	var writer packetWriter
	writer.writeUint16(actorObjectKey)
	writer.writeUint16(positionX)
	writer.writeUint16(positionY)
	writer.writeUint32(objectKey)
	writer.writeBytes(entry.data[:])
	writer.writeByte(sceneMode)
	// Current EXE sub_1D74B60 treats a nonzero trailing u16 as a protected
	// runtime-context guard and returns before creating the ground object when
	// it does not match. Public dungeon discards are deliberately unscoped.
	writer.writeUint16(currentDungeonDiscardUnscopedContext)
	body := writer.bytes()
	if len(body) != currentDungeonDiscardSceneBodySize {
		return nil, currentItemListEntry{}, fmt.Errorf(
			"%w: scene_body_len=%d",
			errCurrentDungeonDiscardMalformed,
			len(body),
		)
	}
	return body, entry, nil
}

// currentDungeonDiscardTradeable 是「这件物品能不能交易」的唯一判定，也是上架
// （拍卖行 op183 / 摆摊 op87）和副本内丢弃共用的那一个。它只看 PVF 的
// [attach type]：只有 `[free]` 与 `[sealing]` 放行，其余取值
// （`[trade]`、`[account]`、`[trade delete]`、空值…）一律拒绝。
// 品级 [rarity] 完全不参与判断。
//
// `[sealing]` 还有一层封装态要求，见 currentDungeonDiscardSealRequired 与
// currentDungeonDiscardSealed；本函数只回答「这个 attach type 允不允许交易」。
func currentDungeonDiscardTradeable(definition dungeonDropItemDefinition) bool {
	switch normalizeMagicBoxPVFType(definition.AttachType) {
	case "free", "sealing":
		return true
	default:
		return false
	}
}

// currentDungeonDiscardSealRequired 报告该定义是否必须处于封装态才可交易。
// 只有 `[sealing]` 需要封装；`[free]` 不看封装状态。
func currentDungeonDiscardSealRequired(definition dungeonDropItemDefinition) bool {
	return normalizeMagicBoxPVFType(definition.AttachType) == "sealing"
}

// currentDungeonDiscardSealed 是封装状态的权威读法，与 currentItemListBindFlag /
// itemgrade.SealFlag 同源：raw_entry[0x0D] → extra seal_flag/seal → Bind。
//
// 不能用 stack.Bind 代替：Bind 是 DB 的 bind_flag（装备绑定），封装标记是物品行
// 的 +0x0D 那一位。GM 邮件发放的 `[sealing]` 装备恰好是「已封装但 bind_flag=0」
// （gmApplyMailEquipmentSeal 只写 extra seal_flag/seal，不像 itemgrade 的
// applyResealState 那样同时写 Bind），用 Bind 判断会把封装件误判成解封件，
// 于是「封装好的装备在拍卖行和摆摊都上不了架」。
func currentDungeonDiscardSealed(stack dnfrepo.ItemStack) bool {
	return currentItemSealFlag(stack) != 0
}

func currentDungeonDiscardStackLocked(stack dnfrepo.ItemStack) bool {
	if stack.Extra == nil {
		return false
	}
	switch strings.ToLower(strings.TrimSpace(stack.Extra["equipment_lock_state"])) {
	case "1", "2", "active", "locked", "unlocking", "pending_unlock":
		return true
	default:
		return false
	}
}

func currentDungeonDiscardStackExpired(stack dnfrepo.ItemStack, now time.Time) bool {
	if now.IsZero() {
		now = time.Now().UTC()
	}
	if !stack.ExpireAt.IsZero() && !stack.ExpireAt.After(now) {
		return true
	}
	expire := currentItemListStackExpire(stack)
	return expire != 0 && int64(expire) <= now.Unix()
}

func cloneRuntimeDungeonDiscardStack(stack dnfrepo.ItemStack) dnfrepo.ItemStack {
	stack.RawEntry = append([]byte(nil), stack.RawEntry...)
	if stack.Extra != nil {
		extra := make(map[string]string, len(stack.Extra))
		for key, value := range stack.Extra {
			extra[key] = value
		}
		stack.Extra = extra
	}
	return stack
}

func (s *Service) handleCurrentDungeonDiscard(session *gameSession, body []byte) (bool, error) {
	if session == nil {
		return false, nil
	}
	session.dungeon.mu.Lock()
	defer session.dungeon.mu.Unlock()

	runtime := session.dungeon.runtime
	if runtime == nil {
		return false, nil
	}
	if runtime.Session == nil || runtime.Room == nil || !dungeonRuntimeOwnsCharacter(runtime, session.selectedCharacterID) {
		s.logGameEvent(session, "game-dungeon-discard-blocked",
			"body_len", len(body),
			"reason", "active_dungeon_runtime_owner_invalid",
			"error", errCurrentDungeonDiscardRuntimeMissing)
		return true, nil
	}
	request, err := parseCurrentDungeonDiscardRequest(body)
	if err != nil {
		s.logGameEvent(session, "game-dungeon-discard-blocked",
			"body_len", len(body),
			"reason", "current_exe_op47_request_malformed",
			"error", err)
		return true, nil
	}
	scene, ok := runtime.Session.Scene()
	if !ok {
		s.logGameEvent(session, "game-dungeon-discard-blocked",
			"source_slot", request.SourceSlot,
			"reason", "current_dungeon_scene_missing",
			"error", errCurrentDungeonDiscardRuntimeMissing)
		return true, nil
	}

	monsterCatalog, err := s.dungeonMonsterCatalog()
	if err != nil {
		s.logGameEvent(session, "game-dungeon-discard-blocked",
			"source_slot", request.SourceSlot,
			"reason", "runtime_pvf_catalog_unavailable",
			"error", err)
		return true, nil
	}
	dropCatalog, err := monsterCatalog.DropCatalog()
	if err != nil {
		s.logGameEvent(session, "game-dungeon-discard-blocked",
			"source_slot", request.SourceSlot,
			"reason", "runtime_pvf_drop_catalog_unavailable",
			"error", err)
		return true, nil
	}

	owner := runtime.DropOwner
	if owner == nil {
		owner = newRuntimeDungeonDropOwner()
	}
	objectKey := owner.nextObjectKeyAtOrAbove(runtime.NextObjectKey)
	if objectKey == 0 || objectKey > math.MaxUint16 {
		s.logGameEvent(session, "game-dungeon-discard-blocked",
			"source_slot", request.SourceSlot,
			"reason", "dungeon_drop_object_key_exhausted",
			"error", errDungeonDropObjectKeyRange)
		return true, nil
	}
	if owner.nextSceneSlot == 0 || owner.nextSceneSlot == math.MaxUint16 {
		s.logGameEvent(session, "game-dungeon-discard-blocked",
			"source_slot", request.SourceSlot,
			"reason", "dungeon_drop_scene_slot_exhausted",
			"error", errDungeonDropSceneSlotRange)
		return true, nil
	}
	sceneSlot := owner.nextSceneSlot

	repositories, ok := s.repositoryGroup()
	if !ok || repositories.AccountAssets == nil {
		s.logGameEvent(session, "game-dungeon-discard-blocked",
			"source_slot", request.SourceSlot,
			"reason", "account_character_assets_transaction_missing",
			"error", errCurrentDungeonDiscardTransaction)
		return true, nil
	}
	assetOwner, err := dnfdungeon.NewOwner(repositories)
	if err != nil {
		s.logGameEvent(session, "game-dungeon-discard-blocked",
			"source_slot", request.SourceSlot,
			"reason", "dungeon_asset_owner_missing",
			"error", errCurrentDungeonDiscardTransaction)
		return true, nil
	}
	accountID := strings.TrimSpace(s.accountIDForSession(session))
	characterID := strconv.Itoa(int(session.selectedCharacterID))
	now := time.Now().UTC()
	accountOwned := dnfrepo.IsAccountSharedInventorySlot(request.ListType, request.SourceSlot)
	var (
		itemID       uint32
		definition   dungeonDropItemDefinition
		removedStack dnfrepo.ItemStack
		remaining    int64
		sceneBody    []byte
		sceneEntry   currentItemListEntry
		itemUpdate   currentItemListEntry
	)
	key := currentDungeonPickupMainSlotKey(request.SourceSlot)
	if accountOwned {
		key = dnfrepo.AccountSharedInventorySlotKey(request.SourceSlot)
	}
	err = assetOwner.MutateOwnedInventory(context.Background(), dnfdungeon.OwnedInventoryMutationCommand{
		AccountID:    accountID,
		CharacterID:  characterID,
		AccountOwned: accountOwned,
		UpdatedAt:    now,
		Apply: func(slots map[string]dnfrepo.ItemStack) (bool, error) {
			stack, foundStack := slots[key]
			if !foundStack || stack.ItemID <= 0 || uint64(stack.ItemID) > math.MaxUint32 || stack.Count <= 0 {
				return false, errCurrentDungeonDiscardItemMismatch
			}
			itemID = uint32(stack.ItemID)
			definition, err = dropCatalog.ResolveItem(itemID)
			if err != nil || !currentDungeonDiscardTradeable(definition) {
				return false, errors.Join(err, errCurrentDungeonDiscardNotTradeable)
			}
			if stack.Bind {
				return false, errCurrentDungeonDiscardNotTradeable
			}
			if currentDungeonDiscardStackLocked(stack) {
				return false, errCurrentDungeonDiscardItemLocked
			}
			if currentDungeonDiscardStackExpired(stack, now) {
				return false, errCurrentDungeonDiscardItemExpired
			}
			if int64(request.Count) > stack.Count ||
				(definition.Kind == dungeonDropItemEquipment && (request.Count != 1 || stack.Count != 1)) {
				return false, errCurrentDungeonDiscardCountInvalid
			}

			removedStack = cloneRuntimeDungeonDiscardStack(stack)
			removedStack.Count = int64(request.Count)
			sceneBody, sceneEntry, err = buildCurrentDungeonDiscardSceneBody(
				currentSceneActorObjectKey(session.selectedCharacterID),
				request.PositionX,
				request.PositionY,
				objectKey,
				sceneSlot,
				removedStack,
				definition,
				request.Count,
				currentDungeonDiscardSceneMode,
			)
			if err != nil {
				return false, err
			}
			remaining = stack.Count - int64(request.Count)
			if remaining > 0 {
				stack.Count = remaining
				entry := currentItemListEntryFromStack(request.ListType, request.SourceSlot, stack)
				stack.RawEntry = append([]byte(nil), entry.data[:]...)
				itemUpdate = entry
				slots[key] = stack
				return true, nil
			}
			itemUpdate.patchCore(request.SourceSlot, math.MaxUint32, 0)
			delete(slots, key)
			return true, nil
		},
	})
	if errors.Is(err, dnfdungeon.ErrCharacterNotFound) {
		err = errCurrentDungeonDiscardRuntimeMissing
	} else if errors.Is(err, dnfdungeon.ErrInventoryNotFound) {
		err = errCurrentDungeonDiscardInventoryMissing
	}
	if err != nil {
		s.logGameEvent(session, "game-dungeon-discard-blocked",
			"dungeon_id", runtime.Dungeon.ID,
			"room", scene.Coordinate.String(),
			"map_id", scene.Map.Map.ID,
			"item_id", itemID,
			"source_slot", request.SourceSlot,
			"count", request.Count,
			"account_owned", accountOwned,
			"reason", "authoritative_inventory_transaction_rejected",
			"error", err)
		return true, nil
	}

	qualitySeed := uint32(0)
	if definition.Kind == dungeonDropItemEquipment {
		qualitySeed = binary.LittleEndian.Uint32(sceneEntry.data[0x06:0x0A])
	}
	drop := &runtimeDungeonDrop{
		ObjectKey:   objectKey,
		SceneSlot:   sceneSlot,
		Item:        definition,
		Amount:      request.Count,
		QualitySeed: qualitySeed,
		DiscardOrigin: &runtimeDungeonDiscardOrigin{
			ListType:     request.ListType,
			SourceSlot:   request.SourceSlot,
			AccountOwned: accountOwned,
			Stack:        cloneRuntimeDungeonDiscardStack(removedStack),
		},
		OwnerActorObjectKey: currentSceneActorObjectKey(session.selectedCharacterID),
		Room:                runtimeDungeonRoomKeyFromScene(scene),
		Status:              runtimeDungeonDropAvailable,
	}
	if err := owner.registerBatch([]*runtimeDungeonDrop{drop}, sceneSlot+1); err != nil {
		s.logGameEvent(session, "game-dungeon-discard-runtime-register-failed-after-commit",
			"item_id", itemID,
			"source_slot", request.SourceSlot,
			"count", request.Count,
			"object_key", objectKey,
			"scene_slot", sceneSlot,
			"error", err)
		return true, err
	}
	runtime.DropOwner = owner
	runtime.NextObjectKey = objectKey + 1

	s.logGameEvent(session, "game-dungeon-discard-committed",
		"dungeon_id", runtime.Dungeon.ID,
		"room", scene.Coordinate.String(),
		"map_id", scene.Map.Map.ID,
		"item_id", itemID,
		"source_slot", request.SourceSlot,
		"count", request.Count,
		"remaining", remaining,
		"account_owned", accountOwned,
		"attach_type", definition.AttachType,
		"object_key", objectKey,
		"scene_slot", sceneSlot,
		"position_x", request.PositionX,
		"position_y", request.PositionY,
		"ack_opcode", currentDungeonDiscardOpcode,
		"scene_opcode", currentDungeonDiscardSceneOpcode)
	if err := s.sendGameUpperSuccess(session, currentDungeonDiscardOpcode, request.AckPayload); err != nil {
		return true, err
	}
	// The current client keeps the source slot and dungeon item manager in a
	// pending-discard state until class-1 op47 succeeds. Creating the class-0
	// op40 ground object before that ACK leaves the new object outside the
	// native pickup candidates and poisons later drops in the same room.
	if err := s.sendGameUpperRawClass(
		session,
		currentDungeonDiscardSceneOpcode,
		sceneBody,
		0,
	); err != nil {
		return true, err
	}
	itemUpdateBody := buildCurrentItemUpdateBody(request.ListType, []currentItemListEntry{itemUpdate})
	s.logGameEvent(session, "game-dungeon-discard-item-update-send",
		"item_id", itemID,
		"source_slot", request.SourceSlot,
		"remaining", remaining,
		"msg_id", uint16(dnfenum.CmdPacketWalkoutPartyMember),
		"classification", 0,
		"entry_count", 1,
		"body_len", len(itemUpdateBody),
		"body_source", "authoritative_post_commit_inventory_op14_raw77")
	if err := s.sendGameUpperRawClass(
		session,
		uint16(dnfenum.CmdPacketWalkoutPartyMember),
		itemUpdateBody,
		0,
	); err != nil {
		return true, err
	}
	return true, nil
}

func restoreCurrentDungeonDiscardedItem(
	ctx context.Context,
	uow dnfrepo.AccountCharacterAssetUnitOfWork,
	accountID string,
	characterID string,
	origin runtimeDungeonDiscardOrigin,
	definition dungeonDropItemDefinition,
	now time.Time,
) (uint16, currentItemListEntry, error) {
	if uow == nil || strings.TrimSpace(accountID) == "" || strings.TrimSpace(characterID) == "" ||
		origin.ListType != dnfrepo.MainInventoryListType || origin.SourceSlot < 0 ||
		origin.Stack.ItemID <= 0 || origin.Stack.Count <= 0 ||
		uint64(origin.Stack.ItemID) > math.MaxUint32 ||
		uint32(origin.Stack.ItemID) != definition.ItemID {
		return 0, currentItemListEntry{}, errCurrentDungeonDiscardTransaction
	}
	if ctx == nil {
		ctx = context.Background()
	}
	if now.IsZero() {
		now = time.Now().UTC()
	}
	var (
		destination uint16
		itemUpdate  currentItemListEntry
	)
	owner, err := dnfdungeon.NewOwner(dnfrepo.Group{AccountAssets: uow})
	if err != nil {
		return 0, currentItemListEntry{}, errCurrentDungeonDiscardTransaction
	}
	err = owner.MutateOwnedInventory(ctx, dnfdungeon.OwnedInventoryMutationCommand{
		AccountID:    accountID,
		CharacterID:  characterID,
		AccountOwned: origin.AccountOwned,
		UpdatedAt:    now,
		Apply: func(slots map[string]dnfrepo.ItemStack) (bool, error) {
			stack := cloneRuntimeDungeonDiscardStack(origin.Stack)
			key := currentDungeonPickupMainSlotKey(origin.SourceSlot)
			if origin.AccountOwned {
				if !dnfrepo.IsAccountSharedInventorySlot(origin.ListType, origin.SourceSlot) {
					return false, errCurrentDungeonDiscardRestoreConflict
				}
				key = dnfrepo.AccountSharedInventorySlotKey(origin.SourceSlot)
			}
			if existing, occupied := slots[key]; occupied {
				stack, err = mergeCurrentDungeonDiscardOrigin(slots, existing, stack, definition)
				if err != nil {
					return false, err
				}
			}
			entry := currentItemListEntryFromStack(origin.ListType, origin.SourceSlot, stack)
			stack.RawEntry = append([]byte(nil), entry.data[:]...)
			itemUpdate = entry
			slots[key] = stack
			destination = uint16(origin.SourceSlot)
			return true, nil
		},
	})
	if errors.Is(err, dnfdungeon.ErrCharacterNotFound) {
		err = errCurrentDungeonDiscardRuntimeMissing
	} else if errors.Is(err, dnfdungeon.ErrInventoryNotFound) {
		err = errCurrentDungeonDiscardInventoryMissing
	}
	if err != nil {
		return 0, currentItemListEntry{}, err
	}
	return destination, itemUpdate, nil
}

func mergeCurrentDungeonDiscardOrigin(
	slots map[string]dnfrepo.ItemStack,
	existing dnfrepo.ItemStack,
	returned dnfrepo.ItemStack,
	definition dungeonDropItemDefinition,
) (dnfrepo.ItemStack, error) {
	if definition.Kind != dungeonDropItemStackable ||
		existing.ItemID != returned.ItemID ||
		existing.Bind != returned.Bind ||
		existing.Bind ||
		currentDungeonDiscardStackLocked(existing) ||
		currentItemListStackExpire(existing) != currentItemListStackExpire(returned) ||
		existing.Count <= 0 || returned.Count <= 0 ||
		existing.Count > math.MaxInt64-returned.Count {
		return dnfrepo.ItemStack{}, errCurrentDungeonDiscardRestoreConflict
	}
	// 归还目标是这件道具原来那一行，但封顶仍然按"这个容器里该道具总量"算：
	// PVF [stack limit] 同时是单行上限和容器总量上限。
	if definition.StackLimit > 0 && (existing.Count > definition.StackLimit-returned.Count ||
		returned.Count > dnfrepo.ContainerItemSpace(
			slots,
			dnfrepo.MainInventoryListType,
			returned.ItemID,
			definition.StackLimit,
		)) {
		return dnfrepo.ItemStack{}, errCurrentDungeonDiscardRestoreConflict
	}
	existing = cloneRuntimeDungeonDiscardStack(existing)
	existing.Count += returned.Count
	return existing, nil
}
