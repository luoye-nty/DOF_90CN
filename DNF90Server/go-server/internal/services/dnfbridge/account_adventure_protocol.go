package dnfbridge

import (
	"encoding/binary"
	"sort"
	"strconv"
	"strings"

	"golang.org/x/text/encoding/simplifiedchinese"
	"longheng.io/server/internal/modules/dnf/adventuregroup"
	dnfrepo "longheng.io/server/internal/modules/dnf/repository"
)

// 冒险团经验值在报文里的单位是 1/10 点原始经验。
//
// 现场取证（2026-09-29，冒险团 5 级）：客户端把 `[adventure exp table]` 第 6 项
// 631,238,480 显示成 **63,123,848**（正好是表值 ÷10），而"当前值"是把报文里的
// u64 **原样打印**的（截图 227,362,848 既不是 10 的倍数，又正好等于服务端账本
// 里那个 5 级账号的进度）。也就是说客户端拿"表值÷10"当分母、拿报文值当分子，
// 我们过去直接发原始经验 → 分子是客户端的 10 倍，条一到 631,238,480/10 就冲破
// 100%（实测 360%、10 格全亮），而服务端要涨满 631,238,480 才升级。
// 这里统一除以刻度之后，客户端的条 = TotalPoint / RequiredExperience[Level+1]，
// **条满 100% 的那一刻正好就是服务端升级的那一刻**，两边口径彻底对齐。
const currentAdventureWirePointScale = 10

// currentAdventureWirePoint 把账本里的原始冒险团经验换算成报文的客户端单位。
func currentAdventureWirePoint(totalPoint uint64) uint64 {
	return totalPoint / currentAdventureWirePointScale
}

// buildCurrentAdventureActorRefreshBody follows current NoPack sub_C7A4C0.
func buildCurrentAdventureActorRefreshBody(
	currentObjectKey uint16,
	summary adventuregroup.Summary,
	projection adventuregroup.Projection,
) []byte {
	raw := make([]byte, currentAdventureActorRefreshRawLength)
	binary.LittleEndian.PutUint32(raw[0:4], uint32(summary.ManageLevel))
	binary.LittleEndian.PutUint64(raw[8:16], currentAdventureWirePoint(summary.TotalPoint))

	var writer packetWriter
	writer.writeUint16(currentObjectKey)
	writer.writeUint32(uint32(len(raw)))
	writer.writeBytes(raw)
	// sub_C7A4C0 reads these u8 counters after its 16-byte raw state.
	writeCurrentAdventureTowerCounters(&writer, projection)
	return writer.bytes()
}

// writeCurrentAdventureTowerCounters writes the four native tower counters
// (remaining and limit, despair then grief) as u8 values.
func writeCurrentAdventureTowerCounters(writer *packetWriter, projection adventuregroup.Projection) {
	for _, count := range [...]uint32{
		projection.TowerRemaining[adventuregroup.TowerDespairType],
		projection.TowerLimits[adventuregroup.TowerDespairType],
		projection.TowerRemaining[adventuregroup.TowerGriefType],
		projection.TowerLimits[adventuregroup.TowerGriefType],
	} {
		writer.writeByte(byte(min(count, 255)))
	}
}

func buildCurrentAdventureInfoBody(
	characterID uint16,
	summary adventuregroup.Summary,
	characters []dnfrepo.CharacterRecord,
) []byte {
	return buildCurrentAdventureInfoBodyWithIdentity(characterID, summary, characters, "", 0)
}

func buildCurrentAdventureInfoBodyWithName(
	characterID uint16,
	summary adventuregroup.Summary,
	characters []dnfrepo.CharacterRecord,
	representAccountName string,
) []byte {
	return buildCurrentAdventureInfoBodyWithIdentity(
		characterID,
		summary,
		characters,
		representAccountName,
		0,
	)
}

// buildCurrentAdventureInfoBody follows sub_C7F960 in the authoritative EXE.
func buildCurrentAdventureInfoBodyWithIdentity(
	characterID uint16,
	summary adventuregroup.Summary,
	characters []dnfrepo.CharacterRecord,
	representAccountName string,
	createdDate uint32,
) []byte {
	return buildCurrentAdventureInfoBodyWithState(
		characterID,
		summary,
		characters,
		representAccountName,
		createdDate,
		adventuregroup.Projection{},
	)
}

func buildCurrentAdventureInfoBodyWithState(
	characterID uint16,
	summary adventuregroup.Summary,
	characters []dnfrepo.CharacterRecord,
	representAccountName string,
	createdDate uint32,
	projection adventuregroup.Projection,
) []byte {
	raw := make([]byte, currentAdventureInfoRawLength)
	binary.LittleEndian.PutUint32(
		raw[currentAdventureInfoManageLevelOffset:currentAdventureInfoManageLevelOffset+4],
		uint32(summary.ManageLevel),
	)
	binary.LittleEndian.PutUint64(
		raw[currentAdventureInfoCurrentPointOffset:currentAdventureInfoCurrentPointOffset+8],
		currentAdventureWirePoint(summary.TotalPoint),
	)
	binary.LittleEndian.PutUint32(
		raw[currentAdventureInfoLoginDaysOffset:currentAdventureInfoLoginDaysOffset+4],
		projection.ConsecutiveLoginDays,
	)
	writeCurrentAdventureHighestHonorSummary(raw, characters)
	characterCount := len(characters)
	if characterCount > currentAdventureInfoRosterCount {
		characterCount = currentAdventureInfoRosterCount
	}
	binary.LittleEndian.PutUint16(
		raw[currentAdventureInfoCharacterCountOffset:currentAdventureInfoCharacterCountOffset+2],
		uint16(characterCount),
	)
	representatives := adventuregroup.RepresentativeState{}
	if projection.RepresentativesEnabled {
		representatives = projection.Representatives
	}
	selected := adventuregroup.ProjectRepresentativeCharacters(
		characters,
		representatives,
		currentAdventureCharacterLevelCap,
	)
	writeCurrentAdventureInfoRoster(raw, selected)
	// 原生 sub_A10AA0 通过 sub_A103A0 清掉被移除的代表，但**保留 job/baseGrow
	// 那一格**。少写这一条会让 sub_A100B0 把这张卡锁死，玩家之后再也选不回该槽位。
	index := len(selected)
	for _, character := range adventuregroup.ProjectRepresentativeCharacters(
		characters,
		adventuregroup.RepresentativeState{},
		currentAdventureCharacterLevelCap,
	) {
		slot, ok := adventuregroup.RepresentativeSlotForCharacter(character, currentAdventureCharacterLevelCap)
		if !ok || index >= currentAdventureInfoRosterCount || !representatives.Selections[slot].Removed {
			continue
		}
		offset := currentAdventureInfoRosterOffset + index*currentAdventureInfoRosterEntrySize
		entry := raw[offset : offset+currentAdventureInfoRosterEntrySize]
		clear(entry)
		entry[0], entry[1] = slot.Job, slot.BaseGrow
		binary.LittleEndian.PutUint32(entry[currentAdventureInfoRosterCardLevelOffset:], uint32(rosterLevel(character)))
		index++
	}
	// 8 字节一条：**数量在前、当前窗口进度在后**。
	// 现场取证（2026-09-23）：把分类号写在第 0 个 dword 时，客户端把分类号当成
	// 数量显示 —— 荣耀结晶显示 "1"、骁勇结晶显示 "0/250"。改成数量在前之后数量
	// 显示正常；随后往第 0 条的第 2 个 dword 写 1,500,000，那条"获得1个结晶"
	// 进度条正好显示 10%，从而确认第 2 个 dword 是进度（分母是 PVF 的 1500 万）。
	for index, points := range projection.Runtime.ShopPoints {
		if index >= currentAdventureInfoShopPointCount {
			break
		}
		offset := currentAdventureInfoShopPointOffset + index*currentAdventureInfoShopPointEntrySize
		binary.LittleEndian.PutUint32(raw[offset:offset+4], points)
		binary.LittleEndian.PutUint32(
			raw[offset+4:offset+8],
			currentAdventureInfoShopPointProgress(index, projection),
		)
	}
	purchaseIndex := 0
	for _, category := range []byte{
		adventuregroup.ShopPointBrave,
		adventuregroup.ShopPointGlory,
		adventuregroup.ShopPointPure,
	} {
		for _, product := range adventureGroupProductsForProjection(projection, category) {
			if purchaseIndex >= currentAdventureInfoPurchaseCount {
				break
			}
			offset := currentAdventureInfoPurchaseOffset + purchaseIndex*currentAdventureInfoPurchaseEntrySize
			binary.LittleEndian.PutUint32(raw[offset:offset+4], product.ItemID)
			binary.LittleEndian.PutUint32(raw[offset+4:offset+8], product.Count)
			purchaseIndex++
		}
	}
	for index := 0; index < adventuregroup.ContentTypeCount; index++ {
		offset := currentAdventureInfoContentCountOffsets[index]
		binary.LittleEndian.PutUint32(raw[offset:offset+4], projection.ContentCounts[index])
	}
	for index := 0; index < currentAdventureInfoTripleCount; index++ {
		offset := currentAdventureInfoTripleOffset + index*currentAdventureInfoTripleSize
		// Native row order is type:u8 then count:u16. The classification byte
		// comes first, and the count is the per-profession character count.
		raw[offset+currentAdventureInfoTripleTypeOffset] = byte(index)
		binary.LittleEndian.PutUint16(
			raw[offset+currentAdventureInfoTripleCountOffset:offset+currentAdventureInfoTripleCountOffset+2],
			projection.ProfessionCounts[index],
		)
	}
	binary.LittleEndian.PutUint64(
		raw[currentAdventureInfoGrowthCapsuleOffset:currentAdventureInfoGrowthCapsuleOffset+8],
		projection.Runtime.GrowthExperience,
	)
	binary.LittleEndian.PutUint32(
		raw[currentAdventureInfoCapsuleClaimCountOffset:currentAdventureInfoCapsuleClaimCountOffset+4],
		currentAdventureInfoCapsuleClaimCount(projection),
	)

	var writer packetWriter
	writer.writeUint16(characterID)
	writer.writeUint32(createdDate)
	// sub_C7F960 reads four u8 counters at body+6..9. Its +96..108
	// offsets belong to the destination object, not the raw roster block.
	writeCurrentAdventureTowerCounters(&writer, projection)
	writer.writeUint32(0)
	writer.writeUint32(uint32(len(raw)))
	writer.writeBytes(raw)
	name, err := encodeRepresentAccountName(representAccountName)
	if err != nil {
		name = nil
	}
	writer.writeRawDstr(name)
	return writer.bytes()
}

// writeCurrentAdventureHighestHonorSummary fills the basic-page profession,
// grow-type and HonorExpert level fields proved by current NoPack.exe
// sub_C79D40 (+44/+45/+48) and sub_9F44D0. Invalid persisted progress is
// omitted instead of being converted into a different character state.
func writeCurrentAdventureHighestHonorSummary(raw []byte, characters []dnfrepo.CharacterRecord) {
	if len(raw) < currentAdventureInfoHighestHonorLevelOffset+4 {
		return
	}
	var (
		bestJob        byte
		bestGrow       byte
		bestLevel      uint32
		bestExperience uint64
	)
	for _, character := range characters {
		progress, err := currentHonorExpertProgress(character)
		if err != nil || progress.Level == 0 ||
			(progress.Level < bestLevel ||
				(progress.Level == bestLevel && progress.CurrentLevelExperience <= bestExperience)) {
			continue
		}
		job, err := strconv.ParseUint(strings.TrimSpace(character.Job), 10, 8)
		if err != nil {
			continue
		}
		grow := character.Stats["grow_type"]
		if grow < 0 || grow > 0xff {
			continue
		}
		bestJob = byte(job)
		bestGrow = byte(grow) & currentAdventureRepresentGrowMask
		bestLevel = progress.Level
		bestExperience = progress.CurrentLevelExperience
	}
	if bestLevel == 0 {
		return
	}
	raw[currentAdventureInfoHighestHonorJobOffset] = bestJob
	raw[currentAdventureInfoHighestHonorGrowOffset] = bestGrow
	binary.LittleEndian.PutUint32(
		raw[currentAdventureInfoHighestHonorLevelOffset:currentAdventureInfoHighestHonorLevelOffset+4],
		bestLevel,
	)
}

type adventureGroupProjectedPurchase struct {
	ItemID uint32
	Count  uint32
}

func adventureGroupProductsForProjection(
	projection adventuregroup.Projection,
	category byte,
) []adventureGroupProjectedPurchase {
	prefix := strconv.Itoa(int(category)) + ":"
	products := make([]adventureGroupProjectedPurchase, 0)
	for key, count := range projection.Runtime.Purchases {
		if count == 0 || !strings.HasPrefix(key, prefix) {
			continue
		}
		itemID, err := strconv.ParseUint(strings.TrimPrefix(key, prefix), 10, 32)
		if err != nil || itemID == 0 {
			continue
		}
		products = append(products, adventureGroupProjectedPurchase{ItemID: uint32(itemID), Count: count})
	}
	sort.Slice(products, func(left, right int) bool { return products[left].ItemID < products[right].ItemID })
	return products
}

func writeCurrentAdventureInfoRoster(raw []byte, characters []dnfrepo.CharacterRecord) {
	count := len(characters)
	if count > currentAdventureInfoRosterCount {
		count = currentAdventureInfoRosterCount
	}
	for index := 0; index < count; index++ {
		entryOffset := currentAdventureInfoRosterOffset + index*currentAdventureInfoRosterEntrySize
		entry := raw[entryOffset : entryOffset+currentAdventureInfoRosterEntrySize]
		writeCurrentAdventureInfoRosterEntry(entry, characters[index])
	}
}

func writeCurrentAdventureInfoRosterEntry(entry []byte, character dnfrepo.CharacterRecord) {
	if len(entry) < currentAdventureInfoRosterEntrySize {
		return
	}
	entry = entry[:currentAdventureInfoRosterEntrySize]
	clear(entry)
	entry[0] = byte(numericCharacterStat(character.Job))
	entry[1] = byte(numericCharacterStatValue(character, "grow_type")) & currentAdventureRepresentGrowMask
	if honor, err := currentHonorExpertProgress(character); err == nil {
		binary.LittleEndian.PutUint32(
			entry[currentAdventureInfoRosterHonorOffset:currentAdventureInfoRosterHonorOffset+4],
			honor.Level,
		)
	}
	name := currentAdventureInfoRosterNameBytes(character.Name)
	copy(
		entry[currentAdventureInfoRosterNameOffset:currentAdventureInfoRosterNameOffset+currentAdventureInfoRosterNameSize],
		name,
	)
	level := rosterLevel(character)
	entry[currentAdventureInfoRosterRegionOffset] = currentAdventureCharacterServerGroup(character)
	// The collection row and op1467 candidate row are distinct layouts. The
	// former carries base grow, HonorExpert level, region and character level;
	// it does not carry a candidate character ID.
	binary.LittleEndian.PutUint32(
		entry[currentAdventureInfoRosterCardLevelOffset:currentAdventureInfoRosterCardLevelOffset+4],
		uint32(level),
	)
}

func currentAdventureInfoRosterNameBytes(name string) []byte {
	encoded := make([]byte, 0, currentAdventureInfoRosterNameSize-1)
	for _, value := range name {
		part, err := simplifiedchinese.GB18030.NewEncoder().Bytes([]byte(string(value)))
		if err != nil || len(encoded)+len(part) >= currentAdventureInfoRosterNameSize {
			break
		}
		encoded = append(encoded, part...)
	}
	return encoded
}
