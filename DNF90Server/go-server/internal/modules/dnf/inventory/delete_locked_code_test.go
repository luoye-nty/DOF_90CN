package inventory

import (
	"context"
	"testing"

	"longheng.io/server/internal/modules/dnf/alignedcmd"
	"longheng.io/server/internal/modules/dnf/dnfenum"
	dnfrepo "longheng.io/server/internal/modules/dnf/repository"
	dnfrepomemory "longheng.io/server/internal/modules/dnf/repository/memory"
)

// 这组用例钉住 op18 删除物品被拒时回给客户端的**失败原因码**。
//
// 现场证据（2026-09-15，线上包 packet log，玩家把锁定物品拖去删除那一刻）：
//
//	SEND kind=game-upper msg_id=18 body=000400000008001800
//
// 第 1 字节 result=0（失败），第 2 字节才是原因码。客户端 op18 失败分支
// 0x01D21E8B 的 cmp 链只有 17(0x11)/19(0x13)/23(0x17)/235(0xEB) 有专属文案，
// 其余落到 0x01D21F3B 再 `call 0x32DDC70`（通用映射器）—— 通用映射器里
// `cmp eax,0xd6` → `push 0xa4f8` 就是 dstr 42232
// 「在物品已锁定状态下，您无法进行当前操作。」所以**锁定必须回 214**，
// 回 4 只会退化成一句和锁定无关的通用文案。
func TestDeleteItemLockedRejectionUsesLockedErrorCode(t *testing.T) {
	ctx := context.Background()
	repos := dnfrepomemory.NewMemoryGroup()
	saveTestCharacter(t, ctx, repos, dnfrepo.CharacterRecord{CharacterID: "77", AccountID: "acc"})
	saveTestInventory(t, ctx, repos, dnfrepo.InventoryRecord{
		CharacterID: "77",
		Slots: map[string]dnfrepo.ItemStack{
			slotKey(listTypeMain, 2): {
				ItemID: 100, Count: 1,
				Extra: map[string]string{"equipment_lock_state": "1"},
			},
		},
	})

	got, err := NewHandler().Handle(ctx, alignedcmd.Request{
		Opcode:              uint16(dnfenum.CmdPacketDeleteItem),
		Body:                []byte{listTypeMain, 0x02, 0x00, 0x01, 0x00},
		AccountID:           "acc",
		SelectedCharacterID: 77,
		Repositories:        repos,
	})
	if err != nil {
		t.Fatalf("Handle error = %v", err)
	}
	if !got.Handled || !got.ResponseAllowed || len(got.UpperResponses) != 1 {
		t.Fatalf("result = %+v", got)
	}
	body := got.UpperResponses[0].Body
	// 帧形状 [result=0][code][u32 len][RETURN]：result 必须是 0，code 必须是锁定码。
	if len(body) != 10 || body[0] != 0 || body[1] != deleteItemLockedErrorCode {
		t.Fatalf("锁定删除失败帧=%x，期望 result=0 code=%d", body, deleteItemLockedErrorCode)
	}
	if deleteItemLockedErrorCode != 214 {
		t.Fatalf("锁定码=%d，客户端只有 214 会渲染 dstr 42232", deleteItemLockedErrorCode)
	}
	// 物品必须原地不动。
	inventory := loadTestInventory(t, ctx, repos, "77")
	if stack := inventory.Slots[slotKey(listTypeMain, 2)]; stack.ItemID != 100 || stack.Count != 1 {
		t.Fatalf("锁定物品被删除/改动: %+v", stack)
	}
}

// 非锁定原因仍然回原来那个通用失败码，不能被锁定码顶掉。
func TestDeleteItemGenericRejectionKeepsGenericErrorCode(t *testing.T) {
	cases := []struct {
		name string
		err  error
		want byte
	}{
		{"locked", ErrItemLocked, deleteItemLockedErrorCode},
		{"other", ErrInventoryNotFound, deleteItemFailureErrorCode},
		{"nil", nil, deleteItemFailureErrorCode},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if got := deleteItemRejectionErrorCode(tc.err); got != tc.want {
				t.Fatalf("code=%d want=%d", got, tc.want)
			}
		})
	}
	// 帧长必须稳定：客户端按固定 10 字节读 RETURN，多一字节少一字节都会错位。
	frame := buildDungeonSkillMaterialDeleteAckWithCode(deleteItemLockedErrorCode)
	if len(frame) != 10 || frame[0] != 0 || frame[1] != 214 || frame[2] != 8 {
		t.Fatalf("锁定删除帧=%x 形状不对", frame)
	}
	// 成功帧保持原样，不能被这次改动碰到。
	if want := []byte{1, 4, 0, 0, 0, 0x08, 0, 0x18, 0}; string(buildDungeonSkillMaterialDeleteAck(true)) != string(want) {
		t.Fatalf("成功帧被改动了: %x", buildDungeonSkillMaterialDeleteAck(true))
	}
	if want := []byte{0, 4, 0, 0, 0, 0x08, 0, 0x18, 0}; string(buildDungeonSkillMaterialDeleteAck(false)) != string(want) {
		t.Fatalf("技能材料失败帧被改动了: %x", buildDungeonSkillMaterialDeleteAck(false))
	}
}
