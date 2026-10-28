package dnfbridge

import (
	"context"
	"encoding/binary"
	"errors"
	"path/filepath"
	"testing"
	"time"

	"longheng.io/server/internal/modules/dnf/alignedcmd"
	"longheng.io/server/internal/modules/dnf/dnfenum"
	dnfrepo "longheng.io/server/internal/modules/dnf/repository"
	"longheng.io/server/internal/modules/dnf/repository/memory"
	platformpvf "longheng.io/server/internal/platform/pvf"
)

func TestRealPVFCustomAbilityGroupConverterVariantsResolveAsOpenOnlyDevices(t *testing.T) {
	pvfPath := filepath.Clean(filepath.Join("..", "..", "..", "..", "runtime", "data", "dnf", "Script.pvf"))
	archive, err := platformpvf.Open(pvfPath)
	if err != nil {
		t.Skipf("runtime Script.pvf unavailable: %v", err)
	}
	catalog, err := newPVFDungeonDropCatalog(archive)
	if err != nil {
		t.Fatal(err)
	}
	service := &Service{pvfItemCatalog: catalog}
	resolver, err := service.alignedStackableUseResolverForCommand(dnfenum.CmdPacketUseStackable)
	if err != nil {
		t.Fatal(err)
	}
	for _, itemID := range []int64{10163994, 490007494, 490008029, 490008598} {
		resolution, err := resolver(itemID)
		if err != nil {
			t.Fatalf("item %d: %v", itemID, err)
		}
		if resolution.CustomAbilityGroupConverter == nil || resolution.StackableType != "[etc]" ||
			resolution.CustomAbilityGroupConverter.MinimumLevel != 1 {
			t.Fatalf("item %d resolution=%+v", itemID, resolution)
		}
		if resolution.Projectile != nil || resolution.Transformation != nil || resolution.TPBook != nil {
			t.Fatalf("item %d was misclassified as an immediate-use consumable: %+v", itemID, resolution)
		}
	}
}

func TestCustomAbilityGroupConverterSessionStartsOnlyOnceAndResets(t *testing.T) {
	now := time.Unix(100, 0).UTC()
	state := currentCustomAbilityGroupConverterSessionState{}
	want := alignedcmd.CustomAbilityGroupConverterProjection{
		CharacterID: 77, SourceListType: 0, SourceSlotIndex: 70, SourceInstance: 123, SourceItemID: 10163994,
	}
	state.arm(want, now)
	got, active, busy := state.begin(77, now.Add(time.Second))
	if !active || busy || got != want {
		t.Fatalf("begin got=%+v active=%t busy=%t", got, active, busy)
	}
	if _, active, busy := state.begin(77, now.Add(2*time.Second)); !active || !busy {
		t.Fatalf("second begin active=%t busy=%t", active, busy)
	}
	state.finish(false)
	if _, active, busy := state.begin(77, now.Add(3*time.Second)); !active || busy {
		t.Fatalf("retry begin active=%t busy=%t", active, busy)
	}
	state.finish(true)
	if _, active, busy := state.begin(77, now.Add(4*time.Second)); active || busy {
		t.Fatalf("committed continuation survived active=%t busy=%t", active, busy)
	}

	state.arm(want, now)
	if _, active, _ := state.begin(77, now.Add(currentCustomAbilityGroupConverterWindow)); active {
		t.Fatal("expired continuation remained active")
	}
	state.arm(want, now)
	state.reset()
	if _, active, _ := state.begin(77, now.Add(time.Second)); active {
		t.Fatal("reset continuation remained active")
	}
}

func TestCommitCurrentCustomAbilityGroupConvertRandomizesSelectedGroupAndConsumesOneDevice(t *testing.T) {
	repositories := memory.NewMemoryGroup()
	catalog := &currentCustomAbilityCatalog{
		optionIDs: map[byte]struct{}{1: {}, 2: {}, 3: {}, 11: {}, 12: {}},
		groups:    [][]byte{{1, 2, 3}, {11, 12}},
	}
	raw := make([]byte, currentItemListEntryWireSize)
	binary.LittleEndian.PutUint16(raw[0:2], 9)
	binary.LittleEndian.PutUint32(raw[2:6], 100130573)
	raw[currentCustomAbilityOption0Offset] = 1
	raw[currentCustomAbilityOption1Offset] = 11
	raw[currentCustomAbilitySetRecordOffset] = 7
	raw[currentCustomAbilityGradeOffset] = 3
	device := dnfrepo.ItemStack{ItemID: 10163994, Count: 2}
	deviceEntry := currentItemListEntryFromStack(dnfrepo.MainInventoryListType, 70, device)
	device.RawEntry = append([]byte(nil), deviceEntry.data[:]...)
	if err := repositories.Inventory.Save(context.Background(), dnfrepo.InventoryRecord{
		CharacterID: "4",
		Slots: map[string]dnfrepo.ItemStack{
			"0:9":  {ItemID: 100130573, Count: 1, RawEntry: raw},
			"0:70": device,
		},
	}); err != nil {
		t.Fatal(err)
	}

	draw := func(limit int) (int, error) {
		if limit != 3 {
			t.Fatalf("draw limit=%d", limit)
		}
		return 2, nil
	}
	result, err := commitCurrentCustomAbilityGroupConvert(
		context.Background(),
		repositories,
		"4",
		currentCustomAbilityModifyRequest{targetSlot: 9, optionPosition: 0, newOptionID: 2},
		100130573,
		alignedcmd.CustomAbilityGroupConverterProjection{
			CharacterID: 4, SourceListType: 0, SourceSlotIndex: 70, SourceItemID: 10163994,
		},
		catalog,
		draw,
	)
	if err != nil {
		t.Fatal(err)
	}
	if !result.groupConverter || result.oldOptionID != 1 || result.newOptionID != 3 ||
		result.converterItemID != 10163994 || result.converterRemaining != 1 || len(result.refreshes) != 2 {
		t.Fatalf("result=%+v", result)
	}
	stored, _, err := repositories.Inventory.Load(context.Background(), "4")
	if err != nil {
		t.Fatal(err)
	}
	if stored.Slots["0:9"].RawEntry[currentCustomAbilityOption0Offset] != 3 ||
		stored.Slots["0:9"].RawEntry[currentCustomAbilityOption1Offset] != 11 ||
		stored.Slots["0:70"].Count != 1 {
		t.Fatalf("stored slots=%+v", stored.Slots)
	}
}

func TestCommitCurrentCustomAbilityGroupConvertMissingDeviceRollsBackTarget(t *testing.T) {
	repositories := memory.NewMemoryGroup()
	catalog := &currentCustomAbilityCatalog{
		optionIDs: map[byte]struct{}{1: {}, 2: {}, 11: {}},
		groups:    [][]byte{{1, 2}, {11}},
	}
	raw := make([]byte, currentItemListEntryWireSize)
	raw[currentCustomAbilityOption0Offset] = 1
	raw[currentCustomAbilityOption1Offset] = 11
	raw[currentCustomAbilityGradeOffset] = 2
	if err := repositories.Inventory.Save(context.Background(), dnfrepo.InventoryRecord{
		CharacterID: "4",
		Slots:       map[string]dnfrepo.ItemStack{"0:9": {ItemID: 100130573, Count: 1, RawEntry: raw}},
	}); err != nil {
		t.Fatal(err)
	}
	_, err := commitCurrentCustomAbilityGroupConvert(
		context.Background(), repositories, "4",
		currentCustomAbilityModifyRequest{targetSlot: 9, optionPosition: 0, newOptionID: 2},
		100130573,
		alignedcmd.CustomAbilityGroupConverterProjection{CharacterID: 4, SourceListType: 0, SourceSlotIndex: 70, SourceItemID: 10163994},
		catalog,
		func(int) (int, error) { return 1, nil },
	)
	if !errors.Is(err, errCurrentCustomAbilityModifyMaterial) {
		t.Fatalf("error=%v", err)
	}
	stored, _, _ := repositories.Inventory.Load(context.Background(), "4")
	if stored.Slots["0:9"].RawEntry[currentCustomAbilityOption0Offset] != 1 {
		t.Fatalf("failed conversion mutated target=%+v", stored.Slots["0:9"])
	}
}

func TestAlignedCustomAbilityGroupConverterArmsOnlyAfterSuccessfulOp44Write(t *testing.T) {
	projection := &alignedcmd.CustomAbilityGroupConverterProjection{
		CharacterID: 77, SourceListType: 0, SourceSlotIndex: 70, SourceItemID: 10163994,
	}
	result := alignedcmd.Result{
		Operation: "use_stackable",
		UpperResponses: []alignedcmd.UpperResponse{{
			MsgID: 44, Classification: 1, Body: []byte{1}, AllowCodec: true,
		}},
		CustomAbilityGroupConverter: projection,
	}
	connection := &bufferConn{}
	service := &Service{}
	session := &gameSession{conn: connection, selectedCharacterID: 77}
	if err := service.sendAlignedUpperResponses(session, result); err != nil {
		t.Fatal(err)
	}
	if got, active, busy := session.customAbilityConverter.begin(77, service.gameplayNow()); !active || busy || got.SourceItemID != 10163994 {
		t.Fatalf("continuation got=%+v active=%t busy=%t", got, active, busy)
	}
	session.customAbilityConverter.finish(false)

	wantErr := errors.New("write failed")
	failing := &runtimeRaidPhaseTestConn{remainingFailures: 1, writeErr: wantErr}
	failedSession := &gameSession{conn: failing, selectedCharacterID: 77}
	if err := service.sendAlignedUpperResponses(failedSession, result); !errors.Is(err, wantErr) {
		t.Fatalf("write error=%v", err)
	}
	if _, active, _ := failedSession.customAbilityConverter.begin(77, service.gameplayNow()); active {
		t.Fatal("failed op44 write armed converter continuation")
	}
}
