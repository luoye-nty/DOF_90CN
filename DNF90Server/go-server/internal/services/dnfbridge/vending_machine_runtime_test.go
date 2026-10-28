package dnfbridge

import (
	"bytes"
	"context"
	"encoding/binary"
	"errors"
	"fmt"
	"math"
	"sync/atomic"
	"testing"
	"time"

	"longheng.io/server/internal/modules/dnf/dnfenum"
	dnfproto "longheng.io/server/internal/modules/dnf/protocol"
	dnfrepo "longheng.io/server/internal/modules/dnf/repository"
	dnfrepomemory "longheng.io/server/internal/modules/dnf/repository/memory"
)

func TestCurrentVendingMachineLiveNormalAndAdvancedRequestsCommitAndAnimate(t *testing.T) {
	tests := []struct {
		name         string
		body         []byte
		materialID   int64
		materialSlot int16
		rewardID     uint32
		rewardCount  uint32
	}{
		{
			name:         "normal consumes diamond coin",
			body:         []byte{1, 0, 0, 0, 1, 0, 0, 0, 0x8c, 0},
			materialID:   7454,
			materialSlot: 140,
			rewardID:     31,
			rewardCount:  2,
		},
		{
			name:         "advanced consumes advanced diamond coin",
			body:         []byte{3, 0, 0, 0, 1, 0, 0, 0, 0x8e, 0},
			materialID:   2749933,
			materialSlot: 142,
			rewardID:     44,
			rewardCount:  3,
		},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			service, repositories := currentVendingMachineTestService(t, map[string]dnfrepo.ItemStack{
				currentCeraShopInventorySlotKey(0, test.materialSlot): {ItemID: test.materialID, Count: 2},
			})
			conn := &bufferConn{}
			session := &gameSession{conn: conn, connID: "vending-machine-test", selectedCharacterID: 19}

			if err := service.handleGameCommand(
				session,
				byte(dnfenum.GameCmdCommand),
				uint16(dnfenum.CmdPacketUseVendingMachine),
				test.body,
			); err != nil {
				t.Fatal(err)
			}

			inventory := currentVendingMachineTestInventory(t, repositories)
			if material := inventory.Slots[currentCeraShopInventorySlotKey(0, test.materialSlot)]; material.ItemID != test.materialID || material.Count != 1 {
				t.Fatalf("material=%+v", material)
			}
			var rewardSlot int16 = -1
			for key, stack := range inventory.Slots {
				listType, slot, ok := parseSceneInventorySlotKey(key)
				if ok && listType == 0 && stack.ItemID == int64(test.rewardID) && stack.Count == int64(test.rewardCount) {
					rewardSlot = slot
					break
				}
			}
			if rewardSlot < 0 {
				t.Fatalf("reward item=%d count=%d missing: %+v", test.rewardID, test.rewardCount, inventory.Slots)
			}

			ack, rest := splitGameServerUpperPacket(t, conn.write.Bytes())
			if ack.Header.Classification != dnfproto.DefaultChannelClassification ||
				ack.Header.MsgID != uint16(dnfenum.CmdPacketUseVendingMachine) {
				t.Fatalf("ack header=%+v", ack.Header)
			}
			if len(ack.Body) != 25+1+currentItemListEntryWireSize || ack.Body[0] != 1 ||
				binary.LittleEndian.Uint16(ack.Body[1:3]) != uint16(test.materialSlot) ||
				binary.LittleEndian.Uint32(ack.Body[3:7]) != 1 ||
				binary.LittleEndian.Uint32(ack.Body[7:11]) != test.rewardID ||
				binary.LittleEndian.Uint32(ack.Body[11:15]) != test.rewardCount ||
				binary.LittleEndian.Uint16(ack.Body[23:25]) != 1 || ack.Body[25] != 0 ||
				binary.LittleEndian.Uint16(ack.Body[26:28]) != uint16(rewardSlot) ||
				binary.LittleEndian.Uint32(ack.Body[28:32]) != test.rewardID {
				t.Fatalf("ack body len=%d body=%x", len(ack.Body), ack.Body)
			}

			if len(rest) != 0 {
				t.Fatalf("unexpected packet after native op215 result: %d bytes", len(rest))
			}
			if session.vendingMachine.pending != nil {
				t.Fatal("successful response retained pending vending operation")
			}
		})
	}
}

func TestCurrentVendingMachineRejectsWrongCoinWithoutDebitOrReward(t *testing.T) {
	service, repositories := currentVendingMachineTestService(t, map[string]dnfrepo.ItemStack{
		"0:140": {ItemID: 2749933, Count: 8},
	})
	conn := &bufferConn{}
	session := &gameSession{conn: conn, connID: "vending-wrong-coin", selectedCharacterID: 19}
	body := []byte{1, 0, 0, 0, 1, 0, 0, 0, 0x8c, 0}
	if err := service.handleCurrentVendingMachine(session, body); err != nil {
		t.Fatal(err)
	}
	inventory := currentVendingMachineTestInventory(t, repositories)
	if stack := inventory.Slots["0:140"]; stack.ItemID != 2749933 || stack.Count != 8 || len(inventory.Slots) != 1 {
		t.Fatalf("failed draw mutated inventory: %+v", inventory.Slots)
	}
	packet, trailing := splitGameServerUpperPacket(t, conn.write.Bytes())
	if len(trailing) != 0 || packet.Header.MsgID != uint16(dnfenum.CmdPacketUseVendingMachine) ||
		!bytes.Equal(packet.Body, []byte{0, currentVendingMachineError}) {
		t.Fatalf("failure header=%+v body=%x trailing=%d", packet.Header, packet.Body, len(trailing))
	}
}

func TestCurrentVendingMachineInventoryFullRollsBackCoin(t *testing.T) {
	slots := make(map[string]dnfrepo.ItemStack)
	for slot := int16(121); slot <= 176; slot++ {
		slots[currentCeraShopInventorySlotKey(0, slot)] = dnfrepo.ItemStack{ItemID: 999999, Count: 1}
	}
	slots["0:140"] = dnfrepo.ItemStack{ItemID: 7454, Count: 2}
	service, repositories := currentVendingMachineTestService(t, slots)
	conn := &bufferConn{}
	session := &gameSession{conn: conn, connID: "vending-full", selectedCharacterID: 19}
	body := []byte{1, 0, 0, 0, 1, 0, 0, 0, 0x8c, 0}
	if err := service.handleCurrentVendingMachine(session, body); err != nil {
		t.Fatal(err)
	}
	inventory := currentVendingMachineTestInventory(t, repositories)
	if inventory.Slots["0:140"].Count != 2 || currentVendingMachineTestItemCount(inventory, 31) != 0 {
		t.Fatalf("full inventory leaked debit/reward: %+v", inventory.Slots)
	}
	packet, trailing := splitGameServerUpperPacket(t, conn.write.Bytes())
	if len(trailing) != 0 || !bytes.Equal(packet.Body, []byte{0, currentVendingMachineError}) {
		t.Fatalf("full inventory failure=%x trailing=%d", packet.Body, len(trailing))
	}
}

func TestCurrentVendingMachineLastCoinUsesZeroRemainingAndDeletesMaterial(t *testing.T) {
	service, repositories := currentVendingMachineTestService(t, map[string]dnfrepo.ItemStack{
		"0:140": {ItemID: 7454, Count: 1},
	})
	conn := &bufferConn{}
	session := &gameSession{conn: conn, connID: "vending-last-coin", selectedCharacterID: 19}
	body := []byte{1, 0, 0, 0, 1, 0, 0, 0, 0x8c, 0}
	if err := service.handleCurrentVendingMachine(session, body); err != nil {
		t.Fatal(err)
	}
	inventory := currentVendingMachineTestInventory(t, repositories)
	if _, found := inventory.Slots["0:140"]; found {
		t.Fatalf("last coin was not deleted: %+v", inventory.Slots["0:140"])
	}
	ack, trailing := splitGameServerUpperPacket(t, conn.write.Bytes())
	if len(trailing) != 0 || binary.LittleEndian.Uint16(ack.Body[1:3]) != 140 ||
		binary.LittleEndian.Uint32(ack.Body[3:7]) != 0 {
		t.Fatalf("last-coin ack=%x trailing=%d", ack.Body, len(trailing))
	}
}

func TestCurrentVendingMachineSplitRewardIncludesEveryTouchedFinalRow(t *testing.T) {
	service, repositories := currentVendingMachineTestService(t, map[string]dnfrepo.ItemStack{
		"0:140": {ItemID: 7454, Count: 2},
	})
	definition, err := service.pvfItemCatalog.ResolveItem(31)
	if err != nil {
		t.Fatal(err)
	}
	definition.StackLimit = 2
	service.pvfItemCatalog.itemCache[31] = definition
	group := service.vendingMachineCatalog.groups[1][1]
	group.Outputs[0].Count = 5
	service.vendingMachineCatalog.groups[1][1] = group
	conn := &bufferConn{}
	session := &gameSession{conn: conn, connID: "vending-split-reward", selectedCharacterID: 19}
	body := []byte{1, 0, 0, 0, 1, 0, 0, 0, 0x8c, 0}
	if err := service.handleCurrentVendingMachine(session, body); err != nil {
		t.Fatal(err)
	}
	inventory := currentVendingMachineTestInventory(t, repositories)
	if got := currentVendingMachineTestItemCount(inventory, 31); got != 5 {
		t.Fatalf("split reward count=%d want=5", got)
	}
	ack, trailing := splitGameServerUpperPacket(t, conn.write.Bytes())
	if len(trailing) != 0 || binary.LittleEndian.Uint32(ack.Body[11:15]) != 5 ||
		binary.LittleEndian.Uint16(ack.Body[23:25]) != 3 ||
		len(ack.Body) != 25+3*(1+currentItemListEntryWireSize) {
		t.Fatalf("split reward ack len=%d body=%x trailing=%d", len(ack.Body), ack.Body, len(trailing))
	}
	for index := 0; index < 3; index++ {
		row := ack.Body[25+index*(1+currentItemListEntryWireSize):]
		if row[0] != dnfrepo.MainInventoryListType || binary.LittleEndian.Uint32(row[3:7]) != 31 {
			t.Fatalf("row %d=%x", index, row[:10])
		}
	}
}

func TestCurrentVendingMachineTwoSuccessfulClicksAreTwoDistinctUses(t *testing.T) {
	service, repositories := currentVendingMachineTestService(t, map[string]dnfrepo.ItemStack{
		"0:140": {ItemID: 7454, Count: 3},
	})
	conn := &bufferConn{}
	session := &gameSession{conn: conn, connID: "vending-two-clicks", selectedCharacterID: 19}
	body := []byte{1, 0, 0, 0, 1, 0, 0, 0, 0x8c, 0}
	for index := 0; index < 2; index++ {
		if err := service.handleCurrentVendingMachine(session, body); err != nil {
			t.Fatalf("click %d: %v", index+1, err)
		}
	}
	inventory := currentVendingMachineTestInventory(t, repositories)
	if inventory.Slots["0:140"].Count != 1 || currentVendingMachineTestItemCount(inventory, 31) != 4 {
		t.Fatalf("two uses inventory=%+v", inventory.Slots)
	}
	stream := conn.write.Bytes()
	for index := 0; index < 2; index++ {
		_, stream = splitGameServerUpperPacket(t, stream)
	}
	if len(stream) != 0 {
		t.Fatalf("two uses trailing bytes=%d", len(stream))
	}
}

func TestCurrentVendingMachineImmediateReceiptReplaySurvivesUnknownCommitOutcome(t *testing.T) {
	service, repositories := currentVendingMachineTestService(t, map[string]dnfrepo.ItemStack{
		"0:140": {ItemID: 7454, Count: 2},
	})
	wantErr := errors.New("committed but connection outcome was lost")
	indeterminate := &currentVendingCommitThenErrorAssets{
		base:     repositories.CharacterAssets,
		failOnce: true,
		err:      wantErr,
	}
	repositories.CharacterAssets = indeterminate
	service.repositoryProvider = func() (dnfrepo.Group, bool) { return repositories, true }
	conn := &bufferConn{}
	session := &gameSession{conn: conn, connID: "vending-unknown-commit", selectedCharacterID: 19}
	body := []byte{1, 0, 0, 0, 1, 0, 0, 0, 0x8c, 0}

	if err := service.handleCurrentVendingMachine(session, body); err != nil {
		t.Fatal(err)
	}
	inventory := currentVendingMachineTestInventory(t, repositories)
	if inventory.Slots["0:140"].Count != 1 || currentVendingMachineTestItemCount(inventory, 31) != 2 || indeterminate.calls != 2 {
		t.Fatalf("unknown commit replay duplicated or lost result: slots=%+v calls=%d", inventory.Slots, indeterminate.calls)
	}
	ack, _ := splitGameServerUpperPacket(t, conn.write.Bytes())
	if ack.Body[0] != 1 {
		t.Fatalf("unknown commit did not return success: %x", ack.Body)
	}
}

func TestCurrentVendingMachineCharacterLockSerializesReconnectReceiptReplay(t *testing.T) {
	service, repositories := currentVendingMachineTestService(t, map[string]dnfrepo.ItemStack{
		"0:140": {ItemID: 7454, Count: 3},
	})
	indeterminate := &currentVendingBlockingCommitThenErrorAssets{
		base:           repositories.CharacterAssets,
		firstCommitted: make(chan struct{}),
		releaseFirst:   make(chan struct{}),
		err:            errors.New("first reconnect transaction committed with an unknown outcome"),
	}
	repositories.CharacterAssets = indeterminate
	service.repositoryProvider = func() (dnfrepo.Group, bool) { return repositories, true }
	body := []byte{1, 0, 0, 0, 1, 0, 0, 0, 0x8c, 0}
	oldConn := &bufferConn{}
	newConn := &bufferConn{}
	oldSession := &gameSession{conn: oldConn, connID: "vending-old-session", selectedCharacterID: 19}
	newSession := &gameSession{conn: newConn, connID: "vending-reconnect-session", selectedCharacterID: 19}

	oldDone := make(chan error, 1)
	go func() { oldDone <- service.handleCurrentVendingMachine(oldSession, body) }()
	select {
	case <-indeterminate.firstCommitted:
	case <-time.After(5 * time.Second):
		t.Fatal("old session did not reach the indeterminate post-commit boundary")
	}

	newStarted := make(chan struct{})
	newDone := make(chan error, 1)
	go func() {
		close(newStarted)
		newDone <- service.handleCurrentVendingMachine(newSession, body)
	}()
	<-newStarted
	// The replacement session must not enter a second character-assets
	// transaction while the old session still owns the receipt-replay window.
	time.Sleep(100 * time.Millisecond)
	if calls := indeterminate.calls.Load(); calls != 1 {
		close(indeterminate.releaseFirst)
		t.Fatalf("replacement session crossed character lock: transaction calls=%d", calls)
	}
	close(indeterminate.releaseFirst)

	for name, done := range map[string]<-chan error{"old": oldDone, "replacement": newDone} {
		select {
		case err := <-done:
			if err != nil {
				t.Fatalf("%s session: %v", name, err)
			}
		case <-time.After(5 * time.Second):
			t.Fatalf("%s session did not finish", name)
		}
	}

	inventory := currentVendingMachineTestInventory(t, repositories)
	if inventory.Slots["0:140"].Count != 1 || currentVendingMachineTestItemCount(inventory, 31) != 4 {
		t.Fatalf("reconnect overlap duplicated a use: %+v", inventory.Slots)
	}
	if calls := indeterminate.calls.Load(); calls != 3 {
		t.Fatalf("transaction calls=%d want old commit+replay and one replacement use", calls)
	}
	for name, conn := range map[string]*bufferConn{"old": oldConn, "replacement": newConn} {
		packet, trailing := splitGameServerUpperPacket(t, conn.write.Bytes())
		if len(trailing) != 0 || packet.Body[0] != 1 {
			t.Fatalf("%s response body=%x trailing=%d", name, packet.Body, len(trailing))
		}
	}
}

func TestCurrentVendingMachineUnknownCommitPublishesFrozenTransactionRow(t *testing.T) {
	service, repositories := currentVendingMachineTestService(t, map[string]dnfrepo.ItemStack{
		"0:140": {ItemID: 7454, Count: 2},
	})
	indeterminate := &currentVendingCommitMutateThenErrorAssets{
		base:      repositories.CharacterAssets,
		inventory: repositories.Inventory,
		err:       errors.New("commit response lost after a later owner merged the reward stack"),
	}
	repositories.CharacterAssets = indeterminate
	service.repositoryProvider = func() (dnfrepo.Group, bool) { return repositories, true }
	conn := &bufferConn{}
	session := &gameSession{conn: conn, connID: "vending-frozen-row", selectedCharacterID: 19}
	body := []byte{1, 0, 0, 0, 1, 0, 0, 0, 0x8c, 0}

	if err := service.handleCurrentVendingMachine(session, body); err != nil {
		t.Fatal(err)
	}
	inventory := currentVendingMachineTestInventory(t, repositories)
	if inventory.Slots["0:140"].Count != 1 || currentVendingMachineTestItemCount(inventory, 31) != 102 {
		t.Fatalf("unexpected durable inventory after cross mutation: %+v", inventory.Slots)
	}
	if calls := indeterminate.calls.Load(); calls != 2 {
		t.Fatalf("transaction calls=%d want commit plus receipt replay", calls)
	}
	ack, trailing := splitGameServerUpperPacket(t, conn.write.Bytes())
	if len(trailing) != 0 || ack.Body[0] != 1 ||
		binary.LittleEndian.Uint32(ack.Body[7:11]) != 31 ||
		binary.LittleEndian.Uint32(ack.Body[11:15]) != 2 ||
		binary.LittleEndian.Uint32(ack.Body[32:36]) != 2 {
		t.Fatalf("op215 did not retain first transaction row: body=%x trailing=%d", ack.Body, len(trailing))
	}
}

func TestCurrentVendingMachineFailedACKReplaysPersistedFrozenRow(t *testing.T) {
	service, repositories := currentVendingMachineTestService(t, map[string]dnfrepo.ItemStack{
		"0:140": {ItemID: 7454, Count: 2},
	})
	wantWriteErr := errors.New("native op215 ACK write failed")
	failing := &failNthDungeonWriteConn{failAt: 1, err: wantWriteErr}
	session := &gameSession{conn: failing, connID: "vending-failed-ack", selectedCharacterID: 19}
	body := []byte{1, 0, 0, 0, 1, 0, 0, 0, 0x8c, 0}

	if err := service.handleCurrentVendingMachine(session, body); !errors.Is(err, wantWriteErr) {
		t.Fatalf("first ACK error=%v want=%v", err, wantWriteErr)
	}
	if session.vendingMachine.pending == nil {
		t.Fatal("failed ACK discarded the durable completion key")
	}
	inventory := currentVendingMachineTestInventory(t, repositories)
	if inventory.Slots["0:140"].Count != 1 || currentVendingMachineTestItemCount(inventory, 31) != 2 {
		t.Fatalf("first committed draw=%+v", inventory.Slots)
	}
	for key, stack := range inventory.Slots {
		if stack.ItemID != 31 {
			continue
		}
		stack.Count += 100
		entry := currentItemListEntryFromStack(dnfrepo.MainInventoryListType, 121, stack)
		if _, slot, ok := parseSceneInventorySlotKey(key); ok {
			entry = currentItemListEntryFromStack(dnfrepo.MainInventoryListType, slot, stack)
		}
		stack.RawEntry = append([]byte(nil), entry.data[:]...)
		inventory.Slots[key] = stack
		break
	}
	if err := repositories.Inventory.Save(context.Background(), inventory); err != nil {
		t.Fatal(err)
	}

	retryConn := &bufferConn{}
	session.conn = retryConn
	if err := service.handleCurrentVendingMachine(session, body); err != nil {
		t.Fatal(err)
	}
	if session.vendingMachine.pending != nil {
		t.Fatal("successful receipt replay retained pending operation")
	}
	inventory = currentVendingMachineTestInventory(t, repositories)
	if inventory.Slots["0:140"].Count != 1 || currentVendingMachineTestItemCount(inventory, 31) != 102 {
		t.Fatalf("ACK retry consumed or granted twice: %+v", inventory.Slots)
	}
	ack, trailing := splitGameServerUpperPacket(t, retryConn.write.Bytes())
	if len(trailing) != 0 || ack.Body[0] != 1 ||
		binary.LittleEndian.Uint32(ack.Body[11:15]) != 2 ||
		binary.LittleEndian.Uint32(ack.Body[32:36]) != 2 {
		t.Fatalf("receipt replay used later slot state: body=%x trailing=%d", ack.Body, len(trailing))
	}
}

func currentVendingMachineTestService(
	t *testing.T,
	slots map[string]dnfrepo.ItemStack,
) (*Service, dnfrepo.Group) {
	t.Helper()
	ctx := context.Background()
	repositories := dnfrepomemory.NewMemoryGroup()
	if err := repositories.Character.Save(ctx, dnfrepo.CharacterRecord{
		CharacterID: "19",
		AccountID:   "account-1",
		Stats:       map[string]int64{},
	}); err != nil {
		t.Fatal(err)
	}
	if err := repositories.Inventory.Save(ctx, dnfrepo.InventoryRecord{
		CharacterID: "19",
		Slots:       slots,
	}); err != nil {
		t.Fatal(err)
	}
	source := vendingMachinePVFTestSource{
		"monster/monster.lst":            "",
		"stackable/stackable.lst":        "7454 `normal_coin.stk`\n2749933 `advanced_coin.stk`\n31 `normal_reward.stk`\n44 `advanced_reward.stk`\n",
		"equipment/equipment.lst":        "",
		"stackable/normal_coin.stk":      "[stackable type] `[material]`\n[stack limit] 1000\n",
		"stackable/advanced_coin.stk":    "[stackable type] `[material]`\n[stack limit] 1000\n",
		"stackable/normal_reward.stk":    "[stackable type] `[material]`\n[stack limit] 1000\n",
		"stackable/advanced_reward.stk":  "[stackable type] `[material]`\n[stack limit] 1000\n",
		currentVendingMachineListPVFPath: "1 `normal.vm` 3 `advanced.vm`",
		"etc/normal.vm":                  vendingMachineDefinitionForTest("1", "7454 1", "31 1 2 0"),
		"etc/advanced.vm":                vendingMachineDefinitionForTest("1", "2749933 1", "44 1 3 0"),
	}
	itemCatalog, err := newPVFDungeonDropCatalog(source)
	if err != nil {
		t.Fatal(err)
	}
	vendingCatalog, err := newCurrentVendingMachineCatalog(source)
	if err != nil {
		t.Fatal(err)
	}
	completionSequence := 0
	service := &Service{
		options: options{
			accountID:          "account-1",
			gameUpperHeader:    gameUpperHeaderChannel13,
			gameUpperBodyCodec: gameUpperBodyCodecPlain,
		},
		pvfItemCatalog:        itemCatalog,
		vendingMachineCatalog: vendingCatalog,
		vendingMachineRoll: func(limit uint64) (uint64, error) {
			if limit == 0 {
				return 0, errCurrentVendingMachineRollInvalid
			}
			return 0, nil
		},
		vendingMachineCompletionKey: func() (string, error) {
			completionSequence++
			return fmt.Sprintf("vending-machine-test-completion-%d", completionSequence), nil
		},
		repositoryProvider: func() (dnfrepo.Group, bool) { return repositories, true },
	}
	return service, repositories
}

func currentVendingMachineTestInventory(t *testing.T, repositories dnfrepo.Group) dnfrepo.InventoryRecord {
	t.Helper()
	inventory, found, err := repositories.Inventory.Load(context.Background(), "19")
	if err != nil || !found {
		t.Fatalf("load inventory found=%t err=%v", found, err)
	}
	return inventory
}

func currentVendingMachineTestItemCount(inventory dnfrepo.InventoryRecord, itemID int64) int64 {
	var total int64
	for _, stack := range inventory.Slots {
		if stack.ItemID == itemID && stack.Count > 0 && total <= math.MaxInt64-stack.Count {
			total += stack.Count
		}
	}
	return total
}

type currentVendingCommitThenErrorAssets struct {
	base     dnfrepo.CharacterAssetUnitOfWork
	failOnce bool
	err      error
	calls    int
}

func (unit *currentVendingCommitThenErrorAssets) WithinCharacterAssets(
	ctx context.Context,
	characterID string,
	apply func(dnfrepo.CharacterRepository, dnfrepo.InventoryRepository, dnfrepo.EquipmentRepository) error,
) error {
	unit.calls++
	err := unit.base.WithinCharacterAssets(ctx, characterID, apply)
	if err == nil && unit.failOnce {
		unit.failOnce = false
		return unit.err
	}
	return err
}

var _ dnfrepo.CharacterAssetUnitOfWork = (*currentVendingCommitThenErrorAssets)(nil)

type currentVendingBlockingCommitThenErrorAssets struct {
	base           dnfrepo.CharacterAssetUnitOfWork
	firstCommitted chan struct{}
	releaseFirst   chan struct{}
	err            error
	calls          atomic.Int32
}

func (unit *currentVendingBlockingCommitThenErrorAssets) WithinCharacterAssets(
	ctx context.Context,
	characterID string,
	apply func(dnfrepo.CharacterRepository, dnfrepo.InventoryRepository, dnfrepo.EquipmentRepository) error,
) error {
	call := unit.calls.Add(1)
	err := unit.base.WithinCharacterAssets(ctx, characterID, apply)
	if err == nil && call == 1 {
		close(unit.firstCommitted)
		select {
		case <-unit.releaseFirst:
		case <-ctx.Done():
			return ctx.Err()
		}
		return unit.err
	}
	return err
}

var _ dnfrepo.CharacterAssetUnitOfWork = (*currentVendingBlockingCommitThenErrorAssets)(nil)

type currentVendingCommitMutateThenErrorAssets struct {
	base      dnfrepo.CharacterAssetUnitOfWork
	inventory dnfrepo.InventoryRepository
	err       error
	calls     atomic.Int32
}

func (unit *currentVendingCommitMutateThenErrorAssets) WithinCharacterAssets(
	ctx context.Context,
	characterID string,
	apply func(dnfrepo.CharacterRepository, dnfrepo.InventoryRepository, dnfrepo.EquipmentRepository) error,
) error {
	call := unit.calls.Add(1)
	err := unit.base.WithinCharacterAssets(ctx, characterID, apply)
	if err != nil || call != 1 {
		return err
	}
	inventory, found, loadErr := unit.inventory.Load(ctx, characterID)
	if loadErr != nil || !found {
		return errors.Join(loadErr, unit.err)
	}
	mutated := false
	for key, stack := range inventory.Slots {
		if stack.ItemID != 31 {
			continue
		}
		stack.Count += 100
		inventory.Slots[key] = stack
		mutated = true
		break
	}
	if !mutated {
		return errors.New("committed reward slot was not found for cross mutation")
	}
	if saveErr := unit.inventory.Save(ctx, inventory); saveErr != nil {
		return errors.Join(saveErr, unit.err)
	}
	return unit.err
}

var _ dnfrepo.CharacterAssetUnitOfWork = (*currentVendingCommitMutateThenErrorAssets)(nil)
