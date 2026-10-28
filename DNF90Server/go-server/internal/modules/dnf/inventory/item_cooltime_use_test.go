package inventory

import (
	"context"
	"encoding/binary"
	"fmt"
	"strings"
	"testing"
	"time"

	"longheng.io/server/internal/modules/dnf/alignedcmd"
	"longheng.io/server/internal/modules/dnf/dnfenum"
	dnfitemcooldown "longheng.io/server/internal/modules/dnf/itemcooldown"
	dnfrepo "longheng.io/server/internal/modules/dnf/repository"
	dnfrepomemory "longheng.io/server/internal/modules/dnf/repository/memory"
)

// 线上报告 2026-09-17：首日度假礼盒（490702504，PVF `[cool time]` 86400000、
// `[cooltime group]` 66、`[cooltime maintenance]` 1 1 1）用掉之后小退重上，冷却
// 就没了 —— 客户端的冷却门只是进程内内存，重上必然清零。服务端因此必须在 op44
// 里自己落一份到期时间并拦住再次使用。这一组用例覆盖 op44 这条真实入口。
func TestHandlerUseStackableRefusesMaintainedCooltimeUntilItExpires(t *testing.T) {
	const itemID = int64(490702504)
	h := newItemCooltimeHarness(t, map[int16]int64{5: itemID})
	h.setContract(itemID, itemCooltimeContract{maintains: true, seconds: 86400, group: 66})
	now := time.Now().UTC().Unix()

	first := h.use(5, itemID)
	firstID, firstExpire := startedCooltime(t, first)
	if firstID != itemID || firstExpire <= now {
		t.Fatalf("首次使用没有开启冷却窗口: id=%d expire=%d now=%d", firstID, firstExpire, now)
	}
	if got := h.stackCount(5); got != 4 {
		t.Fatalf("首次使用后剩余=%d want=4", got)
	}
	if expire, found := dnfitemcooldown.ExpireAt(h.character().Stats, itemID); !found || expire != firstExpire {
		t.Fatalf("冷却没有落库: expire=%d found=%t stats=%+v", expire, found, h.character().Stats)
	}

	// 小退重上：客户端内存里的冷却没了，服务端必须继续拦，而且不能扣道具。
	second := h.use(5, itemID)
	if !strings.Contains(second.Reason, "cooltime is still active") {
		t.Fatalf("窗口内的第二次使用没有被拒: %q", second.Reason)
	}
	if len(second.ItemCooltimeStates) != 0 {
		t.Fatalf("被拒的使用不应上报冷却: %+v", second.ItemCooltimeStates)
	}
	if got := h.stackCount(5); got != 4 {
		t.Fatalf("被拒的使用扣了道具: 剩余=%d want=4", got)
	}
	if expire, found := dnfitemcooldown.ExpireAt(h.character().Stats, itemID); !found || expire != firstExpire {
		t.Fatalf("被拒的使用改动了窗口: expire=%d found=%t want=%d", expire, found, firstExpire)
	}

	// 到期之后同一件道具必须重新可用，并且窗口被刷新。
	h.setCooltime(itemID, now-1)
	third := h.use(5, itemID)
	thirdID, thirdExpire := startedCooltime(t, third)
	if thirdID != itemID || thirdExpire <= now {
		t.Fatalf("到期后仍被拒绝: %q id=%d expire=%d", third.Reason, thirdID, thirdExpire)
	}
	// 同一秒内重算，到期时间可能与第一次相同，但绝不能比旧窗口更早。
	if thirdExpire < firstExpire {
		t.Fatalf("到期后的使用把窗口改短了: expire=%d want>=%d", thirdExpire, firstExpire)
	}
	if got := h.stackCount(5); got != 3 {
		t.Fatalf("到期后使用没有扣道具: 剩余=%d want=3", got)
	}
}

// `[cooltime group]` 的语义是同组道具共用一个窗口（86 参考实现
// InventoryItemLifecycleService.ResolveCooltimeGroup）：礼盒开出的另一件同组
// 道具在窗口内也必须被拦，而不同组、或没有组的道具不受影响。
func TestHandlerUseStackableSharesCooltimeWindowAcrossPVFGroup(t *testing.T) {
	const firstBox = int64(490702504)
	const secondBox = int64(490702505)
	const soloBox = int64(490702506)
	h := newItemCooltimeHarness(t, map[int16]int64{5: firstBox, 6: secondBox, 7: soloBox})
	h.setContract(firstBox, itemCooltimeContract{maintains: true, seconds: 86400, group: 66})
	h.setContract(secondBox, itemCooltimeContract{maintains: true, seconds: 86400, group: 66})
	h.setContract(soloBox, itemCooltimeContract{maintains: true, seconds: 86400, group: 0})

	if id, _ := startedCooltime(t, h.use(5, firstBox)); id != firstBox {
		t.Fatalf("同组的第一件道具没有开启冷却: id=%d", id)
	}
	if got := h.use(6, secondBox); !strings.Contains(got.Reason, "cooltime is still active") {
		t.Fatalf("同组道具没有共用窗口: %q", got.Reason)
	}
	if got := h.stackCount(6); got != 5 {
		t.Fatalf("被同组窗口拦下的道具被消耗: 剩余=%d want=5", got)
	}
	if id, _ := startedCooltime(t, h.use(7, soloBox)); id != soloBox {
		t.Fatalf("无组道具被同组窗口误拦: id=%d", id)
	}
}

// 1696 件消耗品都声明了 `[cooltime maintenance]`，但只有声明了
// `[cooltime maintenance]` 的才需要跨会话保留：没有这个字段时哪怕 `[cool time]`
// 为正，也不该产生任何服务端状态（客户端的本地冷却照旧）。
func TestHandlerUseStackableIgnoresCooltimeWithoutMaintenance(t *testing.T) {
	const itemID = int64(490702504)
	h := newItemCooltimeHarness(t, map[int16]int64{5: itemID})
	h.setContract(itemID, itemCooltimeContract{maintains: false, seconds: 86400, group: 66})

	for attempt := 1; attempt <= 3; attempt++ {
		got := h.use(5, itemID)
		if len(got.ItemCooltimeStates) != 0 {
			t.Fatalf("第 %d 次使用上报了冷却: %+v", attempt, got.ItemCooltimeStates)
		}
	}
	if got := h.stackCount(5); got != 2 {
		t.Fatalf("三次使用后剩余=%d want=2", got)
	}
	if entries := dnfitemcooldown.Entries(h.character().Stats); len(entries) != 0 {
		t.Fatalf("无 [cooltime maintenance] 的道具不应落冷却: %+v", entries)
	}
}

// 消耗失败必须把预占的冷却窗口还回去，否则一次失败的空操作会让这件道具白白
// 冷却一整天（PVF 里最长的是 86400000 毫秒）。
func TestHandlerUseStackableReleasesCooltimeWhenTheUseFails(t *testing.T) {
	const itemID = int64(490702504)
	h := newItemCooltimeHarness(t, map[int16]int64{5: itemID})
	h.setContract(itemID, itemCooltimeContract{maintains: true, seconds: 86400, group: 66})

	// slot 7 是空槽：冷却已经被预占，但道具消耗一定失败。
	failed := h.use(7, itemID)
	if len(failed.ItemCooltimeStates) != 0 {
		t.Fatalf("失败的消耗不应上报冷却: %+v", failed.ItemCooltimeStates)
	}
	if entries := dnfitemcooldown.Entries(h.character().Stats); len(entries) != 0 {
		t.Fatalf("失败的消耗留下了冷却窗口: %+v", entries)
	}
	if got := h.stackCount(5); got != 5 {
		t.Fatalf("失败的消耗改动了背包: 剩余=%d want=5", got)
	}
	// 撤回之后这件道具必须还能正常用一次。
	if id, _ := startedCooltime(t, h.use(5, itemID)); id != itemID {
		t.Fatalf("撤回冷却后仍然不能使用: id=%d", id)
	}
	if got := h.stackCount(5); got != 4 {
		t.Fatalf("撤回冷却后的使用没有扣道具: 剩余=%d want=4", got)
	}
}

// itemCooltimeContract 是一件道具在 PVF 里的冷却契约。
type itemCooltimeContract struct {
	maintains bool
	seconds   int64
	group     int64
}

// itemCooltimeHarness 是 op44 的共用夹具：角色 78 的主背包里放若干组道具，
// 解析器按 setContract 登记的契约回答。
type itemCooltimeHarness struct {
	t         *testing.T
	ctx       context.Context
	repos     dnfrepo.Group
	contracts map[int64]itemCooltimeContract
}

func newItemCooltimeHarness(t *testing.T, slotItems map[int16]int64) *itemCooltimeHarness {
	t.Helper()
	ctx := context.Background()
	repos := dnfrepomemory.NewMemoryGroup()
	if err := repos.Character.Save(ctx, dnfrepo.CharacterRecord{
		CharacterID: "78", AccountID: "acc", Level: 60,
		Stats: map[string]int64{},
	}); err != nil {
		t.Fatal(err)
	}
	slots := make(map[string]dnfrepo.ItemStack, len(slotItems))
	for slot, itemID := range slotItems {
		slots[slotKey(listTypeMain, slot)] = dnfrepo.ItemStack{
			ItemID: itemID,
			Count:  5,
			Extra: map[string]string{
				"item_kind":      "stackable",
				"pvf_path":       itemCooltimeTestPVFPath(itemID),
				"stackable_type": "[waste]",
			},
		}
	}
	saveTestInventory(t, ctx, repos, dnfrepo.InventoryRecord{CharacterID: "78", Slots: slots})
	return &itemCooltimeHarness{
		t: t, ctx: ctx, repos: repos,
		contracts: map[int64]itemCooltimeContract{},
	}
}

func (h *itemCooltimeHarness) setContract(itemID int64, contract itemCooltimeContract) {
	h.contracts[itemID] = contract
}

// use 发一次真实的 op44（CmdPacketUseStackable）并返回 bridge 能看到的对齐结果。
func (h *itemCooltimeHarness) use(slot int16, itemID int64) alignedcmd.Result {
	h.t.Helper()
	got, err := NewHandler().Handle(h.ctx, alignedcmd.Request{
		Opcode:                    uint16(dnfenum.CmdPacketUseStackable),
		Body:                      itemCooltimeUseBody(slot, itemID),
		AccountID:                 "acc",
		SelectedCharacterID:       78,
		Repositories:              h.repos,
		StackableUseResolver:      h.resolve,
		ItemCooltimeGroupResolver: h.groupOf,
	})
	if err != nil {
		h.t.Fatalf("Handle error = %v", err)
	}
	return got
}

// startedCooltime 从一次 op44 的结果里取服务端刚开启的冷却窗口。
func startedCooltime(t *testing.T, result alignedcmd.Result) (int64, int64) {
	t.Helper()
	if len(result.ItemCooltimeStates) == 0 {
		return 0, 0
	}
	if len(result.ItemCooltimeStates) > 1 {
		t.Fatalf("一次使用上报了多个冷却窗口: %+v", result.ItemCooltimeStates)
	}
	return result.ItemCooltimeStates[0].ItemID, result.ItemCooltimeStates[0].ExpireAt
}

func (h *itemCooltimeHarness) resolve(itemID int64) (alignedcmd.StackableUseResolution, error) {
	contract, found := h.contracts[itemID]
	if !found {
		return alignedcmd.StackableUseResolution{}, fmt.Errorf("unexpected item %d", itemID)
	}
	return alignedcmd.StackableUseResolution{
		ItemID:            itemID,
		PVFPath:           itemCooltimeTestPVFPath(itemID),
		StackableType:     "[waste]",
		MaintainsCooltime: contract.maintains,
		CooltimeSeconds:   contract.seconds,
		CooltimeGroup:     contract.group,
	}, nil
}

func (h *itemCooltimeHarness) groupOf(itemID int64) int64 {
	return h.contracts[itemID].group
}

func (h *itemCooltimeHarness) character() dnfrepo.CharacterRecord {
	h.t.Helper()
	character, found, err := h.repos.Character.Load(h.ctx, "78")
	if err != nil || !found {
		h.t.Fatalf("character reload err=%v found=%t", err, found)
	}
	return character
}

func (h *itemCooltimeHarness) setCooltime(itemID, expireAt int64) {
	h.t.Helper()
	character := h.character()
	dnfitemcooldown.Set(character.Stats, itemID, expireAt)
	if err := dnfrepo.SaveCharacterFields(h.ctx, h.repos.Character, character, dnfrepo.CharacterFieldStats); err != nil {
		h.t.Fatal(err)
	}
}

func (h *itemCooltimeHarness) stackCount(slot int16) int64 {
	h.t.Helper()
	return loadTestInventory(h.t, h.ctx, h.repos, "78").Slots[slotKey(listTypeMain, slot)].Count
}

// itemCooltimeUseBody 是当前 EXE 的 15 字节 op44 包体：u16 槽位、u8 背包、
// i32 实例值、i32 道具编号、u32 保留位（必须为 0）。
func itemCooltimeUseBody(slot int16, itemID int64) []byte {
	body := make([]byte, 15)
	binary.LittleEndian.PutUint16(body[0:2], uint16(slot))
	body[2] = listTypeMain
	binary.LittleEndian.PutUint32(body[3:7], 0x11223344)
	binary.LittleEndian.PutUint32(body[7:11], uint32(itemID))
	return body
}

// itemCooltimeTestPVFPath 让夹具里的背包记录和解析器指向同一个 PVF 路径，
// 这是 owner 授权 `[waste]` 消耗的必要条件之一。
func itemCooltimeTestPVFPath(itemID int64) string {
	return fmt.Sprintf("stackable/%d/item.stk", itemID)
}
