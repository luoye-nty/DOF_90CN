package dnfbridge

import (
	"encoding/binary"
	"math"
	"testing"

	dnfrepo "longheng.io/server/internal/modules/dnf/repository"
)

// 心意通/戒指券在用掉"最后一个"之后，槽位已经从背包里删掉了。
// 这时必须发**清空行**（itemID=0xffffffff、count=0），否则客户端会继续显示那个
// 已经不存在的道具，再点就报错（2026-10-04 真机现场）。
func TestCurrentWeddingConsumedSlotUpdateClearsExhaustedSlot(t *testing.T) {
	const slot int32 = 42

	cleared := currentWeddingConsumedSlotUpdate(slot, dnfrepo.ItemStack{}, false)
	if got := binary.LittleEndian.Uint32(cleared.data[2:6]); got != math.MaxUint32 {
		t.Fatalf("清空行的 itemID=%#x，应为 0xffffffff", got)
	}
	if got := binary.LittleEndian.Uint32(cleared.data[6:10]); got != 0 {
		t.Fatalf("清空行的 count=%d，应为 0", got)
	}

	// 还有剩余时照常回真实的那一行。
	remaining := currentWeddingConsumedSlotUpdate(slot, dnfrepo.ItemStack{ItemID: 10002901, Count: 3}, true)
	if got := binary.LittleEndian.Uint32(remaining.data[2:6]); got != 10002901 {
		t.Fatalf("剩余行的 itemID=%d，应为 10002901", got)
	}
	if got := binary.LittleEndian.Uint32(remaining.data[6:10]); got != 3 {
		t.Fatalf("剩余行的 count=%d，应为 3", got)
	}

	// 计数归零但键还留着（不该发生）同样按清空处理，避免残留幻影行。
	zeroed := currentWeddingConsumedSlotUpdate(slot, dnfrepo.ItemStack{ItemID: 10002901, Count: 0}, true)
	if got := binary.LittleEndian.Uint32(zeroed.data[2:6]); got != math.MaxUint32 {
		t.Fatalf("计数归零的行 itemID=%#x，应为 0xffffffff", got)
	}
}
