package dnfbridge

import (
	"longheng.io/server/internal/modules/dnf/adventuregroup"
	"longheng.io/server/internal/modules/dnf/dnfenum"
)

const (
	currentAdventureInfoPushMsgID               = uint16(dnfenum.CmdPacketCustomAbilityEquipOption)
	currentAdventureActorRefreshMsgID           = uint16(dnfenum.CmdPacketAgitWarMissionReward)
	currentAdventureInfoRequestWireLength       = 16
	currentAdventureInfoSceneRequestLength      = 2
	currentAdventureInfoRawLength               = 7420
	currentAdventureActorRefreshRawLength       = 16
	currentAdventureInfoManageLevelOffset       = 0
	currentAdventureInfoCurrentPointOffset      = 8
	currentAdventureInfoLoginDaysOffset         = 20
	currentAdventureInfoHighestHonorJobOffset   = 44
	currentAdventureInfoHighestHonorGrowOffset  = 45
	currentAdventureInfoHighestHonorLevelOffset = 48
	currentAdventureInfoRosterOffset            = 60
	currentAdventureInfoRosterCount             = 100
	currentAdventureInfoRosterEntrySize         = 52
	currentAdventureInfoRosterHonorOffset       = 4
	currentAdventureInfoRosterNameOffset        = 12
	currentAdventureInfoRosterNameSize          = 30
	currentAdventureInfoRosterRegionOffset      = 42
	currentAdventureInfoRosterCardLevelOffset   = 48
	currentAdventureInfoCharacterCountOffset    = 56
	currentAdventureInfoShopPointOffset         = 5260
	currentAdventureInfoShopPointCount          = 5
	currentAdventureInfoShopPointEntrySize      = 8
	currentAdventureInfoPurchaseOffset          = 5300
	currentAdventureInfoPurchaseCount           = 256
	currentAdventureInfoPurchaseEntrySize       = 8
	currentAdventureInfoTripleOffset            = 7348
	currentAdventureInfoTripleCount             = 20
	currentAdventureInfoTripleSize              = 3
	// Each native row is type:u8 then count:u16. The order matters: the client
	// reads the classification byte first (see the native regression test).
	currentAdventureInfoTripleTypeOffset    = 0
	currentAdventureInfoTripleCountOffset   = 1
	currentAdventureInfoGrowthCapsuleOffset = 7408
	// currentAdventureInfoCapsuleClaimCountOffset 是成长胶囊页签"可获得的经验值"
	// 那一栏的值，也就是**现在可以领取几次**（每次 = 10% 档 = 1,000,000 点）。
	// 客户端解析 sub_C7DAF0 时把这个 dword 存进 manager+0x310，而
	// 0x1DE1F30 = `mov eax,[ecx+0x310]; ret` —— 反汇编直接确认。
	// 它同时被 0x13F0FA0 当成循环上限逐个建列表项，所以**只能发次数**，
	// 不能发经验值总量（发 1,100 万会让客户端建上千万个字符串而卡死）。
	currentAdventureInfoCapsuleClaimCountOffset = 7416
	currentSelectorAdventureInfoSlotMetadata    = adventuregroup.SelectorSlotMetadataKey
)

// currentAdventureInfoContentCountOffsets are the six u32 content counters.
// The client reads them at exactly these offsets; the order is the native
// index order (ContentCounts[0] -> 16, [1] -> 32, ...), not sorted ascending.
var currentAdventureInfoContentCountOffsets = [adventuregroup.ContentTypeCount]int{16, 32, 24, 28, 36, 40}
