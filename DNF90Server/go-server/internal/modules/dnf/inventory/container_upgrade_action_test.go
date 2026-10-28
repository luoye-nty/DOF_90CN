package inventory

import (
	"bytes"
	"context"
	"encoding/binary"
	"reflect"
	"strconv"
	"testing"
	"time"

	"longheng.io/server/internal/modules/dnf/alignedcmd"
	"longheng.io/server/internal/modules/dnf/dnfenum"
	dnfrepo "longheng.io/server/internal/modules/dnf/repository"
	dnfrepomemory "longheng.io/server/internal/modules/dnf/repository/memory"
)

// 现场 2026-10-03（服主报，角色 测试x = char 11）：三张扩容券"使用后没效果"。
// 取证见 container_upgrade_action.go 文件头——客户端发了 op430/op408/op515-146，
// 服务端一条都没接。本文件的用例锁住三条通道各自的落地结果与拒绝路径。
const (
	testInventoryVoucherItemID = 490701690 // [upgrade inventory]
	testCargoVoucherItemID     = 490701691 // [upgrade cargo]
	testAccountVoucherItemID   = 490701692 // [upgrade account cargo]
)

type containerUpgradeFixture struct {
	repos dnfrepo.Group
	now   time.Time
}

// newContainerUpgradeVoucherResolver 按 item id 给出 PVF 解析结果，形状与
// dnfbridge 的 alignedStackableUseResolverForCommand 一致。
func newContainerUpgradeVoucherResolver(t *testing.T, kinds map[int64]alignedcmd.ContainerUpgradeKind) alignedcmd.StackableUseResolver {
	t.Helper()
	return func(itemID int64) (alignedcmd.StackableUseResolution, error) {
		kind, ok := kinds[itemID]
		if !ok {
			// 普通消耗品：没有 [upgrade *] action type。
			return alignedcmd.StackableUseResolution{ItemID: itemID, StackableType: "[waste]"}, nil
		}
		return alignedcmd.StackableUseResolution{
			ItemID:        itemID,
			PVFPath:       "stackable/etc/chn_" + strconv.FormatInt(itemID, 10) + ".stk",
			StackableType: "[etc]",
			ContainerUpgrade: &alignedcmd.ContainerUpgradeResolution{
				Kind:   kind,
				ItemID: itemID,
			},
		}, nil
	}
}

func allThreeContainerUpgradeKinds() map[int64]alignedcmd.ContainerUpgradeKind {
	return map[int64]alignedcmd.ContainerUpgradeKind{
		testInventoryVoucherItemID: alignedcmd.ContainerUpgradeInventory,
		testCargoVoucherItemID:     alignedcmd.ContainerUpgradeCargo,
		testAccountVoucherItemID:   alignedcmd.ContainerUpgradeAccountCargo,
	}
}

func newContainerUpgradeFixture(
	t *testing.T,
	ctx context.Context,
	containerValues map[string]string,
	accountMetadata map[string]string,
	slots map[string]dnfrepo.ItemStack,
) containerUpgradeFixture {
	t.Helper()
	now := time.Date(2026, time.October, 3, 12, 0, 0, 0, time.UTC)
	repos := dnfrepomemory.NewMemoryGroup()
	if accountMetadata == nil {
		accountMetadata = map[string]string{}
	}
	if err := repos.Account.Save(ctx, dnfrepo.AccountRecord{
		AccountID: "acc",
		Metadata:  accountMetadata,
		UpdatedAt: now,
	}); err != nil {
		t.Fatal(err)
	}
	if err := repos.Character.Save(ctx, dnfrepo.CharacterRecord{
		CharacterID: "42",
		AccountID:   "acc",
		Stats:       map[string]int64{"gold": 123456},
	}); err != nil {
		t.Fatal(err)
	}
	if err := repos.Settings.Save(ctx, dnfrepo.SettingsRecord{
		Scope:     dnfrepo.CharacterContainerStateScope("42"),
		Values:    containerValues,
		UpdatedAt: now,
	}); err != nil {
		t.Fatal(err)
	}
	if err := repos.Inventory.Save(ctx, dnfrepo.InventoryRecord{
		CharacterID: "42",
		Slots:       slots,
		UpdatedAt:   now,
	}); err != nil {
		t.Fatal(err)
	}
	return containerUpgradeFixture{repos: repos, now: now}
}

func defaultContainerValues() map[string]string {
	return map[string]string{
		"source":                      "current_exe_86jp_op13_container_state",
		"main_list_param16":           "0",
		"avatar_list_param16":         "0",
		"personal_cargo_list_param16": "8",
		"account_cargo_selection_key": "0",
		"account_cargo_value32":       "0",
	}
}

func defaultAccountCargoMetadata() map[string]string {
	return map[string]string{
		"account_cargo_created": "true",
		"account_cargo_level":   "72",
		"account_cargo_gold":    "6283103",
		"account_cera":          "665094",
	}
}

// newContainerUpgradeOpcodeRequest 构造 op430/op408 的 2 字节请求（i16 槽位）。
func newContainerUpgradeOpcodeRequest(
	t *testing.T,
	opcode dnfenum.CmdPacket,
	fixture containerUpgradeFixture,
	slot uint16,
	kinds map[int64]alignedcmd.ContainerUpgradeKind,
) alignedcmd.Request {
	t.Helper()
	body := make([]byte, 2)
	binary.LittleEndian.PutUint16(body, slot)
	return alignedcmd.Request{
		Opcode:               uint16(opcode),
		Body:                 body,
		AccountID:            "acc",
		SelectedCharacterID:  42,
		Repositories:         fixture.repos,
		StackableUseResolver: newContainerUpgradeVoucherResolver(t, kinds),
	}
}

// newContainerUpgradeActionRequest 构造 op515 的 19 字节请求（槽位 + action index）。
func newContainerUpgradeActionRequest(
	t *testing.T,
	fixture containerUpgradeFixture,
	slot uint16,
	actionIndex uint32,
	kinds map[int64]alignedcmd.ContainerUpgradeKind,
) alignedcmd.Request {
	t.Helper()
	body := make([]byte, 19)
	binary.LittleEndian.PutUint16(body[0:2], slot)
	binary.LittleEndian.PutUint32(body[7:11], actionIndex)
	return alignedcmd.Request{
		Opcode:               uint16(dnfenum.CmdPacketUseStackableAction),
		Body:                 body,
		AccountID:            "acc",
		SelectedCharacterID:  42,
		Repositories:         fixture.repos,
		StackableUseResolver: newContainerUpgradeVoucherResolver(t, kinds),
	}
}

func loadContainerState(t *testing.T, ctx context.Context, fixture containerUpgradeFixture) dnfrepo.CharacterContainerState {
	t.Helper()
	record, found, err := fixture.repos.Settings.Load(ctx, dnfrepo.CharacterContainerStateScope("42"))
	if err != nil || !found {
		t.Fatalf("container state load found=%t err=%v", found, err)
	}
	state, err := dnfrepo.CharacterContainerStateFromSettings(record, "42")
	if err != nil {
		t.Fatal(err)
	}
	return state
}

func loadAccountMetadata(t *testing.T, ctx context.Context, fixture containerUpgradeFixture) map[string]string {
	t.Helper()
	account, found, err := fixture.repos.Account.Load(ctx, "acc")
	if err != nil || !found {
		t.Fatalf("account load found=%t err=%v", found, err)
	}
	return account.Metadata
}

func loadSlot(t *testing.T, ctx context.Context, fixture containerUpgradeFixture, key string) (dnfrepo.ItemStack, bool) {
	t.Helper()
	inventory, found, err := fixture.repos.Inventory.Load(ctx, "42")
	if err != nil || !found {
		t.Fatalf("inventory load found=%t err=%v", found, err)
	}
	stack, ok := inventory.Slots[key]
	return stack, ok
}

// TestUpgradeInventoryVoucherAddsEightSlotsAndRebuildsListZero 对应 op430：
// 券被消耗一张、main_list_param16 +8、最后整表重建 list 0（背包没有仓库底板，
// 与商城买扩容券那条已验证路径同口径）。
func TestUpgradeInventoryVoucherAddsEightSlotsAndRebuildsListZero(t *testing.T) {
	ctx := context.Background()
	slots := map[string]dnfrepo.ItemStack{
		"0:66": {ItemID: testInventoryVoucherItemID, Count: 1000},
	}
	fixture := newContainerUpgradeFixture(t, ctx, defaultContainerValues(), defaultAccountCargoMetadata(), slots)
	result, err := NewHandler().Handle(ctx, newContainerUpgradeOpcodeRequest(
		t, dnfenum.CmdPacketUpgradeInventory, fixture, 66, allThreeContainerUpgradeKinds()))
	if err != nil {
		t.Fatal(err)
	}
	if !result.Handled || !result.ResponseAllowed || len(result.UpperResponses) != 1 {
		t.Fatalf("result=%+v", result)
	}
	wantAck := []byte{1, 66, 0, 0, 0, 0xE7, 0x03, 0, 0, 1}
	if got := result.UpperResponses[0].Body; !bytes.Equal(got, wantAck) {
		t.Fatalf("op430 ACK=%x want=%x", got, wantAck)
	}
	if len(result.ItemSlotRefreshes) != 1 || result.ItemSlotRefreshes[0].ListType != listTypeMain ||
		result.ItemSlotRefreshes[0].SlotIndex != 66 {
		t.Fatalf("item slot refreshes=%+v", result.ItemSlotRefreshes)
	}
	if !bytes.Equal(result.ItemListRebuilds, []byte{listTypeMain}) {
		t.Fatalf("item list rebuilds=%v, want [0]", result.ItemListRebuilds)
	}
	if len(result.PostActions) != 0 {
		t.Fatalf("post actions=%+v, 背包扩容不该重建仓库底板", result.PostActions)
	}
	state := loadContainerState(t, ctx, fixture)
	if state.MainSlotCount != 8 || state.PersonalCargoSlotCount != 8 {
		t.Fatalf("container state=%+v, want main 8 / cargo 8", state)
	}
	stack, found := loadSlot(t, ctx, fixture, "0:66")
	if !found || stack.Count != 999 {
		t.Fatalf("voucher slot=%+v found=%t, want 999 left", stack, found)
	}
}

// TestUpgradeCargoVoucherAddsSixteenSlotsAndResetsCargoPad 对应 op408：
// 金库 +16 格，而且必须先发 op391 重建仓库底板再发 list 2 —— 当前 EXE 的面板格子
// 是建面板时按容量铺的，只更新行数据不会解锁新格子（现场 2026-09-18 的既有结论）。
func TestUpgradeCargoVoucherAddsSixteenSlotsAndResetsCargoPad(t *testing.T) {
	ctx := context.Background()
	slots := map[string]dnfrepo.ItemStack{
		"0:68": {ItemID: testCargoVoucherItemID, Count: 1},
	}
	fixture := newContainerUpgradeFixture(t, ctx, defaultContainerValues(), defaultAccountCargoMetadata(), slots)
	result, err := NewHandler().Handle(ctx, newContainerUpgradeOpcodeRequest(
		t, dnfenum.CmdPacketUpgradeCargo, fixture, 68, allThreeContainerUpgradeKinds()))
	if err != nil {
		t.Fatal(err)
	}
	if !result.ResponseAllowed || len(result.UpperResponses) != 1 {
		t.Fatalf("result=%+v", result)
	}
	wantActions := []alignedcmd.PostAction{
		alignedcmd.PostActionResetCargoPadBeforeItemLists,
		alignedcmd.PostActionRefreshSelectedPersonalCargo,
	}
	if !reflect.DeepEqual(result.PostActions, wantActions) {
		t.Fatalf("post actions=%+v want=%+v", result.PostActions, wantActions)
	}
	if len(result.ItemListRebuilds) != 0 {
		t.Fatalf("item list rebuilds=%v, 金库要走底板+list2 而不是整表重建", result.ItemListRebuilds)
	}
	state := loadContainerState(t, ctx, fixture)
	if state.PersonalCargoSlotCount != 24 || state.MainSlotCount != 0 {
		t.Fatalf("container state=%+v, want cargo 24 / main 0", state)
	}
	if _, found := loadSlot(t, ctx, fixture, "0:68"); found {
		t.Fatalf("voucher slot 68 must be gone after the last one was consumed")
	}
}

// TestUseStackableAction146UpgradesAccountCargoWithTheVoucherAsPayment 对应
// op515 action 146（现场日志里被拒的那条）：帐号金库推进一档，而且**不再收**金币/
// 无色小晶块/点券 —— 券本身就是代价。
func TestUseStackableAction146UpgradesAccountCargoWithTheVoucherAsPayment(t *testing.T) {
	ctx := context.Background()
	slots := map[string]dnfrepo.ItemStack{
		"0:67": {ItemID: testAccountVoucherItemID, Count: 1},
	}
	fixture := newContainerUpgradeFixture(t, ctx, defaultContainerValues(), defaultAccountCargoMetadata(), slots)
	// action index 写死 146：这是现场日志里实测出来的线上字节（body[7:11]=0x92000000），
	// 用常量会让"常量被改错"和"用例跟着一起改"互相掩盖。
	result, err := NewHandler().Handle(ctx, newContainerUpgradeActionRequest(
		t, fixture, 67, 146, allThreeContainerUpgradeKinds()))
	if err != nil {
		t.Fatal(err)
	}
	if !result.ResponseAllowed || len(result.UpperResponses) != 1 {
		t.Fatalf("result=%+v", result)
	}
	wantAck := []byte{1, 67, 0, 0, 0, 0, 0, 0, 0, 1}
	if got := result.UpperResponses[0].Body; !bytes.Equal(got, wantAck) {
		t.Fatalf("op515 ACK=%x want=%x", got, wantAck)
	}
	wantActions := []alignedcmd.PostAction{
		alignedcmd.PostActionResetCargoPadBeforeItemLists,
		alignedcmd.PostActionRefreshSelectedAccountCargo,
	}
	if !reflect.DeepEqual(result.PostActions, wantActions) {
		t.Fatalf("post actions=%+v want=%+v", result.PostActions, wantActions)
	}
	metadata := loadAccountMetadata(t, ctx, fixture)
	if metadata["account_cargo_level"] != "80" {
		t.Fatalf("account_cargo_level=%q want 80", metadata["account_cargo_level"])
	}
	if metadata["account_cera"] != "665094" {
		t.Fatalf("account_cera=%q, 券付账不该再扣点券", metadata["account_cera"])
	}
	if metadata["account_cargo_gold"] != "6283103" {
		t.Fatalf("account_cargo_gold=%q, 券付账不该动金库金币", metadata["account_cargo_gold"])
	}
	character, found, err := fixture.repos.Character.Load(ctx, "42")
	if err != nil || !found {
		t.Fatalf("character load found=%t err=%v", found, err)
	}
	if character.Stats["gold"] != 123456 {
		t.Fatalf("character gold=%d, 券付账不该扣角色金币", character.Stats["gold"])
	}
	if _, found := loadSlot(t, ctx, fixture, "0:67"); found {
		t.Fatalf("voucher slot 67 must be gone")
	}
}

// TestUpgradeVoucherRejectsMaxStageWithoutConsuming：到顶就是拒绝，且**不能**吃券。
func TestUpgradeVoucherRejectsMaxStageWithoutConsuming(t *testing.T) {
	ctx := context.Background()
	values := defaultContainerValues()
	values["main_list_param16"] = "24"
	slots := map[string]dnfrepo.ItemStack{
		"0:66": {ItemID: testInventoryVoucherItemID, Count: 3},
	}
	fixture := newContainerUpgradeFixture(t, ctx, values, defaultAccountCargoMetadata(), slots)
	result, err := NewHandler().Handle(ctx, newContainerUpgradeOpcodeRequest(
		t, dnfenum.CmdPacketUpgradeInventory, fixture, 66, allThreeContainerUpgradeKinds()))
	if err != nil {
		t.Fatal(err)
	}
	if !result.Handled || !result.ResponseAllowed || len(result.UpperResponses) != 1 {
		t.Fatalf("result=%+v", result)
	}
	wantAck := []byte{0, useStackableActionFailureCode}
	if got := result.UpperResponses[0].Body; !bytes.Equal(got, wantAck) {
		t.Fatalf("failure ACK=%x want=%x", got, wantAck)
	}
	state := loadContainerState(t, ctx, fixture)
	if state.MainSlotCount != 24 {
		t.Fatalf("container state=%+v, 拒绝时必须原样不动", state)
	}
	stack, found := loadSlot(t, ctx, fixture, "0:66")
	if !found || stack.Count != 3 {
		t.Fatalf("voucher slot=%+v found=%t, 拒绝时不能扣券", stack, found)
	}
}

// TestUpgradeVoucherRejectsAnotherContainersVoucher：拿金库券去点物品栏（或反过来）
// 必须被拒——PVF 解析出的 kind 与 opcode 要求的 kind 不一致。
func TestUpgradeVoucherRejectsAnotherContainersVoucher(t *testing.T) {
	ctx := context.Background()
	slots := map[string]dnfrepo.ItemStack{
		"0:66": {ItemID: testCargoVoucherItemID, Count: 1},
	}
	fixture := newContainerUpgradeFixture(t, ctx, defaultContainerValues(), defaultAccountCargoMetadata(), slots)
	result, err := NewHandler().Handle(ctx, newContainerUpgradeOpcodeRequest(
		t, dnfenum.CmdPacketUpgradeInventory, fixture, 66, allThreeContainerUpgradeKinds()))
	if err != nil {
		t.Fatal(err)
	}
	wantAck := []byte{0, useStackableActionFailureCode}
	if len(result.UpperResponses) != 1 || !bytes.Equal(result.UpperResponses[0].Body, wantAck) {
		t.Fatalf("result=%+v want failure ACK %x", result.UpperResponses, wantAck)
	}
	state := loadContainerState(t, ctx, fixture)
	if state.MainSlotCount != 0 || state.PersonalCargoSlotCount != 8 {
		t.Fatalf("container state=%+v, 错位组合必须原样不动", state)
	}
	if stack, found := loadSlot(t, ctx, fixture, "0:66"); !found || stack.Count != 1 {
		t.Fatalf("voucher slot=%+v found=%t, 拒绝时不能扣券", stack, found)
	}
}

// TestUpgradeVoucherRejectsOrdinaryConsumable：槽上放的不是扩容券（PVF 没有
// [upgrade *] action type）时必须拒绝，且不消耗任何东西。
func TestUpgradeVoucherRejectsOrdinaryConsumable(t *testing.T) {
	ctx := context.Background()
	slots := map[string]dnfrepo.ItemStack{
		"0:66": {ItemID: 490001384, Count: 5},
	}
	fixture := newContainerUpgradeFixture(t, ctx, defaultContainerValues(), defaultAccountCargoMetadata(), slots)
	result, err := NewHandler().Handle(ctx, newContainerUpgradeOpcodeRequest(
		t, dnfenum.CmdPacketUpgradeInventory, fixture, 66, allThreeContainerUpgradeKinds()))
	if err != nil {
		t.Fatal(err)
	}
	wantAck := []byte{0, useStackableActionFailureCode}
	if len(result.UpperResponses) != 1 || !bytes.Equal(result.UpperResponses[0].Body, wantAck) {
		t.Fatalf("result=%+v want failure ACK %x", result.UpperResponses, wantAck)
	}
	if stack, found := loadSlot(t, ctx, fixture, "0:66"); !found || stack.Count != 5 {
		t.Fatalf("slot=%+v found=%t, 拒绝时不能扣道具", stack, found)
	}
	state := loadContainerState(t, ctx, fixture)
	if state.MainSlotCount != 0 {
		t.Fatalf("container state=%+v, 拒绝时必须原样不动", state)
	}
}

// TestUpgradeVoucherRejectsEmptySlot：空格子（客户端乱发）必须拒绝而不是 panic。
func TestUpgradeVoucherRejectsEmptySlot(t *testing.T) {
	ctx := context.Background()
	fixture := newContainerUpgradeFixture(t, ctx, defaultContainerValues(), defaultAccountCargoMetadata(),
		map[string]dnfrepo.ItemStack{})
	result, err := NewHandler().Handle(ctx, newContainerUpgradeOpcodeRequest(
		t, dnfenum.CmdPacketUpgradeInventory, fixture, 66, allThreeContainerUpgradeKinds()))
	if err != nil {
		t.Fatal(err)
	}
	wantAck := []byte{0, useStackableActionFailureCode}
	if len(result.UpperResponses) != 1 || !bytes.Equal(result.UpperResponses[0].Body, wantAck) {
		t.Fatalf("result=%+v want failure ACK %x", result.UpperResponses, wantAck)
	}
}

// TestUpgradeAccountCargoVoucherRequiresAnExistingVault：没有帐号金库就升级不了
// （官方/既有 op305 语义是 ErrCargoNotCreated），券不能被吃掉。
func TestUpgradeAccountCargoVoucherRequiresAnExistingVault(t *testing.T) {
	ctx := context.Background()
	metadata := defaultAccountCargoMetadata()
	metadata["account_cargo_level"] = "0"
	delete(metadata, "account_cargo_created")
	slots := map[string]dnfrepo.ItemStack{
		"0:67": {ItemID: testAccountVoucherItemID, Count: 1},
	}
	fixture := newContainerUpgradeFixture(t, ctx, defaultContainerValues(), metadata, slots)
	result, err := NewHandler().Handle(ctx, newContainerUpgradeActionRequest(
		t, fixture, 67, 146, allThreeContainerUpgradeKinds()))
	if err != nil {
		t.Fatal(err)
	}
	wantAck := []byte{0, useStackableActionFailureCode}
	if len(result.UpperResponses) != 1 || !bytes.Equal(result.UpperResponses[0].Body, wantAck) {
		t.Fatalf("result=%+v want failure ACK %x", result.UpperResponses, wantAck)
	}
	if stack, found := loadSlot(t, ctx, fixture, "0:67"); !found || stack.Count != 1 {
		t.Fatalf("voucher slot=%+v found=%t, 拒绝时不能扣券", stack, found)
	}
}

// TestUseStackableActionDecoderAccepts146AndStillRejectsUnknownIndexes：
// 白名单只放宽到证据支持的那一个 action index。
func TestUseStackableActionDecoderAccepts146AndStillRejectsUnknownIndexes(t *testing.T) {
	body := make([]byte, 19)
	binary.LittleEndian.PutUint16(body[0:2], 67)
	// 线上字节：现场 op515 请求 body[7:11]=0x92000000 ⇒ 146。
	binary.LittleEndian.PutUint32(body[7:11], 146)
	parsed, err := DecodeUseStackableActionRequest(body)
	if err != nil || parsed.ActionIndex != 146 || parsed.SourceSlotIndex != 67 {
		t.Fatalf("decode=%+v err=%v", parsed, err)
	}
	binary.LittleEndian.PutUint32(body[7:11], 147)
	if _, err := DecodeUseStackableActionRequest(body); err == nil {
		t.Fatalf("action index 147 must stay unsupported")
	}
}

// TestContainerUpgradeRequestDecoderIsStrictAboutTheBody：
// op430/op408 的 body 现场只有 2 字节；别的长度一律拒绝，不猜。
func TestContainerUpgradeRequestDecoderIsStrictAboutTheBody(t *testing.T) {
	body := []byte{0x42, 0x00}
	parsed, err := DecodeContainerUpgradeRequest(body)
	if err != nil || parsed.SlotIndex != 66 || parsed.ListType != listTypeMain {
		t.Fatalf("decode=%+v err=%v", parsed, err)
	}
	for _, bad := range [][]byte{{}, {0x42}, {0x42, 0x00, 0x00}} {
		if _, err := DecodeContainerUpgradeRequest(bad); err == nil {
			t.Fatalf("body %v must be rejected", bad)
		}
	}
	// 账号共享固定槽（354..365）不是扩容券能待的地方。
	shared := make([]byte, 2)
	binary.LittleEndian.PutUint16(shared, 354)
	if _, err := DecodeContainerUpgradeRequest(shared); err == nil {
		t.Fatalf("account-shared slot 354 must be rejected")
	}
}
