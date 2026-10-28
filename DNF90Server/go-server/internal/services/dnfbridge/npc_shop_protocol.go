package dnfbridge

import (
	"encoding/binary"
	"errors"
	"fmt"
	"math"

	dnfrepo "longheng.io/server/internal/modules/dnf/repository"
	"longheng.io/server/internal/modules/dnf/wedding"
)

const (
	currentNPCShopBuyRequestWireSize           = 16
	currentNPCShopSellRequestWireSize          = 11
	currentNPCShopPurchaseCountRequestWireSize = 4
	currentNPCShopPurchaseCountListCount       = 4
	currentNPCShopBuyErrorCode                 = byte(0x04)
	currentNPCShopSellErrorCode                = byte(0x11)
	// currentNPCShopSellErrorLocked is the op22 failure code the client maps to
	// dstr 42232 「在物品已锁定状态下， 您无法进行当前操作。」 - the same 214 the
	// upgrade, private-store (op87) and cargo paths already answer. Without it a
	// locked item looked like any other sell failure and the client stayed silent
	// (live 2026-09-15 07:35: the rejection itself was correct).
	currentNPCShopSellErrorLocked = byte(214)
	currentNPCShopMaxBuyCount     = uint32(math.MaxUint16)
)

var errCurrentNPCShopRequestInvalid = errors.New("dnf current NPC shop request is invalid")

type currentNPCShopBuyRequest struct {
	ItemID uint32
	// Count is the raw wire value until PVF item-kind validation. The current
	// client sends 0 for one equipment item and positive counts for stackables.
	Count        uint32
	ShopContext  uint32
	ActorContext uint32
}

type currentNPCShopSellRequest struct {
	ListType     byte
	Slot         int16
	Count        uint32
	ActorContext uint32
}

type currentNPCShopBuyResult struct {
	WeddingPurchase *wedding.Result
	WeddingRoom     *wedding.RoomResult
	GoldAfter       int64
	SPAfter         int64
	CoinAfter       int64
	Item            currentItemListEntry
	Updates         []currentItemListEntry
	// ItemListType is the list the purchased rows belong to. Ordinary goods
	// stay in list 0 and their rows ride inside Updates; guild medals and
	// guardian gems land in list 38 and their rows are carried separately in
	// ItemUpdates so the handler can send them under the right op14 list byte.
	ItemListType     byte
	ItemUpdates      []currentItemListEntry
	Slot             int16
	ItemID           uint32
	Count            uint32
	CostItemID       uint32
	CostItemNewCount int64
}

type currentNPCShopSellResult struct {
	GoldAfter int64
	ListType  byte
	Slot      int16
	Applied   uint32
	ItemID    uint32
	Updates   []currentItemListEntry
}

func parseCurrentNPCShopPurchaseCountRequest(body []byte) (uint32, error) {
	if len(body) == 0 {
		return 0, nil
	}
	if len(body) != currentNPCShopPurchaseCountRequestWireSize {
		return 0, fmt.Errorf("%w: purchase-count body length=%d want=0 or %d", errCurrentNPCShopRequestInvalid, len(body), currentNPCShopPurchaseCountRequestWireSize)
	}
	return binary.LittleEndian.Uint32(body), nil
}

func parseCurrentNPCShopBuyRequest(body []byte) (currentNPCShopBuyRequest, error) {
	if len(body) != currentNPCShopBuyRequestWireSize {
		return currentNPCShopBuyRequest{}, fmt.Errorf("%w: buy body length=%d want=%d", errCurrentNPCShopRequestInvalid, len(body), currentNPCShopBuyRequestWireSize)
	}
	request := currentNPCShopBuyRequest{
		ItemID:       binary.LittleEndian.Uint32(body[0:4]),
		Count:        binary.LittleEndian.Uint32(body[4:8]),
		ShopContext:  binary.LittleEndian.Uint32(body[8:12]),
		ActorContext: binary.LittleEndian.Uint32(body[12:16]),
	}
	if request.ItemID == 0 || request.Count > currentNPCShopMaxBuyCount {
		return currentNPCShopBuyRequest{}, fmt.Errorf("%w: buy item=%d count=%d", errCurrentNPCShopRequestInvalid, request.ItemID, request.Count)
	}
	return request, nil
}

func normalizeCurrentNPCShopBuyCount(definition dungeonDropItemDefinition, wireCount uint32) (uint32, error) {
	switch definition.Kind {
	case dungeonDropItemEquipment:
		if wireCount > 1 {
			return 0, fmt.Errorf("%w: equipment item=%d wire_count=%d", errCurrentNPCShopProductUnavailable, definition.ItemID, wireCount)
		}
		return 1, nil
	case dungeonDropItemStackable:
		if wireCount == 0 {
			return 0, fmt.Errorf("%w: stackable item=%d wire_count=0", errCurrentNPCShopRequestInvalid, definition.ItemID)
		}
		return wireCount, nil
	default:
		return 0, fmt.Errorf("%w: item=%d kind=%s", errCurrentNPCShopProductUnavailable, definition.ItemID, definition.Kind)
	}
}

func parseCurrentNPCShopSellRequest(body []byte) (currentNPCShopSellRequest, error) {
	if len(body) != currentNPCShopSellRequestWireSize {
		return currentNPCShopSellRequest{}, fmt.Errorf("%w: sell body length=%d want=%d", errCurrentNPCShopRequestInvalid, len(body), currentNPCShopSellRequestWireSize)
	}
	slot := int16(binary.LittleEndian.Uint16(body[1:3]))
	request := currentNPCShopSellRequest{
		ListType:     body[0],
		Slot:         slot,
		Count:        binary.LittleEndian.Uint32(body[3:7]),
		ActorContext: binary.LittleEndian.Uint32(body[7:11]),
	}
	if request.ListType != dnfrepo.MainInventoryListType || request.Slot <= 0 || request.Count == 0 {
		return currentNPCShopSellRequest{}, fmt.Errorf("%w: sell list=%d slot=%d count=%d", errCurrentNPCShopRequestInvalid, request.ListType, request.Slot, request.Count)
	}
	// 晶体/灵魂仓库（list 0, 354..365）里的道具可以原地卖给 NPC：客户端把它们当普通
	// list-0 格子寻址（现场 2026-09-17 16:19:09 就是 list=0 slot=358 count=10 被这里
	// 拒掉，玩家看到的是"拖上去没反应"）。归属由 npcshop owner 的 Assets.AccountInventory
	// 决定，见 commitCurrentNPCShopSell。
	return request, nil
}

// currentNPCShopBuyAckAmountIsPurchasedCount 决定购买回包里那一行道具的数量字段写什么。
//
// 现场：客户端把回包里这一行的数量**累加**进它自己的“已购买”计数，于是显示 1/3/6/10/15
// （买第 n 个时入库堆叠数正好是 n，累加成三角数）。官方协议里这行是“本次买到的道具”，
// 数量应当是本次购买数量；入库后的真实堆叠数由紧随其后的 op14 增量行负责（我们本来就发，
// 且客户端的背包数量一直是对的）。所以这里写 result.Count。
//
// 若真机实测发现背包数量出现异常（例如停在购买数量而不是总堆叠数），把常量改成 false
// 即回到“写入库后堆叠数”的旧行为。
const currentNPCShopBuyAckAmountIsPurchasedCount = true

func buildCurrentNPCShopBuySuccessBody(result currentNPCShopBuyResult) []byte {
	var body packetWriter
	body.writeByte(1)
	body.writeUint32(currentNPCShopWalletUint32(result.GoldAfter))
	body.writeUint32(currentNPCShopWalletUint32(result.SPAfter))
	body.writeUint32(0)
	body.writeUint32(currentNPCShopWalletUint32(result.CoinAfter))
	ackItem := result.Item
	if currentNPCShopBuyAckAmountIsPurchasedCount {
		purchased := result.Count
		if purchased == 0 {
			purchased = 1
		}
		binary.LittleEndian.PutUint32(ackItem.data[0x06:0x0A], purchased)
	}
	body.writeBytes(ackItem.data[:])
	// 86JP BuyItemAckBuilder: trailing cost-item rows (u8 count, then
	// u32 item id + u32 new stack count each). Gold buys carry none; material
	// exchanges report the consumed material's remaining count.
	if result.CostItemID == 0 {
		body.writeByte(0)
	} else {
		body.writeByte(1)
		body.writeUint32(result.CostItemID)
		body.writeUint32(currentNPCShopWalletUint32(result.CostItemNewCount))
	}
	return body.bytes()
}

func buildCurrentNPCShopPurchaseCountBody() []byte {
	var body packetWriter
	body.writeByte(1)
	for range currentNPCShopPurchaseCountListCount {
		body.writeUint32(0)
	}
	return body.bytes()
}

func buildCurrentNPCShopSellSuccessBody(result currentNPCShopSellResult) []byte {
	var body packetWriter
	body.writeByte(1)
	body.writeUint32(currentNPCShopWalletUint32(result.GoldAfter))
	body.writeByte(result.ListType)
	body.writeUint16(uint16(result.Slot))
	body.writeUint32(result.Applied)
	return body.bytes()
}

func buildCurrentNPCShopFailureBody(errorCode byte) []byte {
	return []byte{0, errorCode}
}

func currentNPCShopWalletUint32(value int64) uint32 {
	if value <= 0 {
		return 0
	}
	if value > math.MaxUint32 {
		return math.MaxUint32
	}
	return uint32(value)
}
