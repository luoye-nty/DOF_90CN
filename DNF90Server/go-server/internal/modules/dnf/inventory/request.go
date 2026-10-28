// 本文件集中解析背包类 C2S 请求体。
// 字节顺序按最新 EXE/C# 对齐证据整理；解析器只产生请求计划，不直接修改资产或生成成功响应。
package inventory

import (
	"encoding/binary"
	"fmt"
	"math"

	"google.golang.org/protobuf/encoding/protowire"

	dnfrepo "longheng.io/server/internal/modules/dnf/repository"
)

const (
	listTypeMain          byte = 0
	listTypeAvatar        byte = 1
	listTypePersonalCargo byte = 2
	listTypeEquipment     byte = 3
	listTypePet           byte = 7
	listTypeActorWornAlt  byte = 17
	listTypeAccountCargo  byte = 12
	listTypeGuildMedal    byte = 38
	maxDeleteEntries           = 100
)

// DeleteRequest 描述删除物品请求；Extended=true 时 body 带批量删除 entry。
type DeleteRequest struct {
	Extended     bool
	StateTrailer bool
	ListType     byte
	SlotIndex    int16
	Count        int16
	Entries      []DeleteEntry
}

// DeleteEntry 是扩展删除格式里的 12 字节 entry。
type DeleteEntry struct {
	OpType      int16
	SlotIndex   int16
	ItemID      int32
	DeleteCount int32
}

// DeleteOrSellRequest 描述删除/出售共用的短格式请求。
type DeleteOrSellRequest struct {
	HasListType bool
	ListType    byte
	SlotIndex   int16
	Count       int16
}

// MoveItemspaceRequest 描述物品移动/拆分请求。
type MoveItemspaceRequest struct {
	SourceListType           byte
	SourceSlotIndex          int16
	SourceInstanceValue      int32
	MoveCount                int32
	DestinationListType      byte
	DestinationSlotIndex     int16
	DestinationInstanceValue int32
	DestinationStack         int32
	ActorIndex               int32
	TrailingState0           byte
	TrailingState1           byte
}

// SortItemRequest 描述整理背包请求。
type SortItemRequest struct {
	ListType  byte
	Category  byte
	Condition byte
}

// BuyItemRequest 描述 NPC 购买请求。
type BuyItemRequest struct {
	ItemTemplateID int32
	Count          int32
}

// RepairEquipmentRequest 描述装备维修请求。
type RepairEquipmentRequest struct {
	InvenType      byte
	SlotIndex      int16
	RepairItemSlot int16
	AutoRepair     bool
	QuickRepair    bool
}

// DisjointItemRequest 描述物品分解请求。
type DisjointItemRequest struct {
	TargetSlotIndex       int16
	ItemSpace             byte
	DisjointItemSlotIndex int16
	ContextValue          int32
}

// UseStackableRequest 描述使用消耗品请求。
type UseStackableRequest struct {
	SlotIndex     int16
	ListType      byte
	InstanceValue int32
	ItemCode      int32
	Reserved      uint32
}

// EventServerLevelUpRequest is the current NoPack op418 request. The client
// sends only the durable inventory location; item identity is resolved from
// the repository and revalidated inside the atomic level-up settlement.
type EventServerLevelUpRequest struct {
	SlotIndex int16
	ListType  byte
}

// IncreaseStatusRequest is the current NoPack op30
// (ENUM_CMDPACKET_INCREASE_STATUS) request the client sends when an SP/TP
// skill book or a character experience book is double-clicked. Its body is a
// bare main-inventory slot: no list owner, no instance value and no item
// template, so identity is resolved from the durable row and revalidated
// inside the atomic settlement, exactly like op418.
type IncreaseStatusRequest struct {
	SlotIndex int16
}

// UseSharedEffectItemRequest describes the current class-1/type251 request.
// Current-client evidence shows a compact six-byte body: item template u32,
// source slot byte, then the inventory list byte.
type UseSharedEffectItemRequest struct {
	SlotIndex      int16
	ItemTemplateID int32
	ListType       byte
}

type UseStackableActionRequest struct {
	SourceSlotIndex int16
	ListType        byte
	Reserved0       uint32
	ActionIndex     uint32
	Reserved1       uint32
	Reserved2       uint32
}

type SelectDamageFontRequest struct {
	FontIndex uint16
}

// ContainerUpgradeRequest is the current-EXE op430/op408 body: one i16 slot.
//
// 现场 2026-10-03（conn game-000124，测试x = char 11）抓到的就是 2 字节：
//
//	op430 ENUM_CMDPACKET_UPGRADE_INVENTORY body=`4200`（券在背包槽 66）
//	op408 ENUM_CMDPACKET_UPGRADE_CARGO     body=`4400`（券在背包槽 68）
//
// 官方服务端（df_game_r(1) 带符号）同名分发器读的也是 `PacketBuf::get_short`，之后
// 用 INVEN_TYPE=1 去取槽位物品，本字段口径与它一致。客户端只发背包（list 0）里的券。
type ContainerUpgradeRequest struct {
	SlotIndex int16
	ListType  byte
}

// UpgradeItemRequest 描述强化/增幅请求。
type UpgradeItemRequest struct {
	Mode    string
	RawMode uint16
	// TargetListType is the high byte of the op50 mode word: 0 for a backpack
	// row, 3 for a worn equipment slot.
	TargetListType          byte
	TargetSlotIndex         int16
	TargetItemTemplateID    int32
	MaterialSlotIndex       int16
	OptionalTicketSlotIndex int16
	TargetItemName          string
	// HighUpgrade is op50 sub mode 2: the 高级装备强化机, fed with 高级炉岩炭
	// instead of the ordinary 无色小晶块. The mode is otherwise a reinforce.
	HighUpgrade bool
}

// ChronicleGrowthRequest is the fixed current-EXE class1/op271 body.
// TargetTaggedValue is independent from authoritative raw+0x58 and is kept
// only for protocol completeness.
type ChronicleGrowthRequest struct {
	TicketSlotIndex   int16
	TicketItemCode    int32
	TargetSlotIndex   int16
	TargetItemCode    int32
	TargetTaggedValue byte
	MaterialSlotIndex int16
	MaterialItemCode  int32
}

// SeparateUpgradeRequest is the current NoPack class1/op438 weapon-forging
// request. The item name is a u32 byte-length DSTR, followed by one independent
// trailing byte.
type SeparateUpgradeRequest struct {
	TargetListType       byte
	TargetSlotIndex      int16
	TargetItemTemplateID int32
	MaterialSlotIndex    int16
	TargetItemName       string
	TrailingFlag         byte
}

// ChronicleRefineRequest is the exact current NoPack class1/op370 body.
type ChronicleRefineRequest struct {
	MaterialSlotIndex      int16
	MaterialItemTemplateID int32
	TargetListType         byte
	TargetSlotIndex        int16
	TargetItemTemplateID   int32
	OptionNo               byte
}

// EnchantByBeadRequest 描述宝珠附魔请求。
type EnchantByBeadRequest struct {
	BeadListType    byte
	BeadSlotIndex   int16
	TargetListType  byte
	TargetSlotIndex int16
}

// MonsterCardUpgradeRequest is the exact current NoPack op603 body observed
// from the Abello card-upgrade window.
type MonsterCardUpgradeRequest struct {
	ListType      byte
	TargetSlot    int16
	MaterialCount int16
	MaterialSlot  int16
}

// DecodeDeleteRequest 解析删除物品请求，兼容 C# 里 simple 与 extended 两种格式。
func DecodeDeleteRequest(body []byte) (DeleteRequest, error) {
	// Skill-material consumption appends a one-byte state marker (0x01)
	// after the declared delete protobuf. Keep this exception local to op18.
	if payload, ok := unwrapCurrentDeleteStateTrailer(body); ok {
		parsed, err := decodeCurrentDeleteRequest(payload)
		if err != nil {
			return DeleteRequest{}, err
		}
		parsed.StateTrailer = true
		return parsed, nil
	}
	if payload, current, err := unwrapCurrentProtobufBody(body); current {
		if err != nil {
			return DeleteRequest{}, err
		}
		return decodeCurrentDeleteRequest(payload)
	}
	if len(body) < 4 {
		return DeleteRequest{}, shortBodyError(len(body), 4)
	}
	if len(body) >= 15 && body[1] >= 1 && body[1] <= maxDeleteEntries {
		count := int(body[1])
		want := 2 + count*12
		if len(body) < want {
			return DeleteRequest{}, shortBodyError(len(body), want)
		}
		entries := make([]DeleteEntry, 0, count)
		for i := 0; i < count; i++ {
			offset := 2 + i*12
			entries = append(entries, DeleteEntry{
				OpType:      readI16(body, offset),
				SlotIndex:   readI16(body, offset+2),
				ItemID:      readI32(body, offset+4),
				DeleteCount: readI32(body, offset+8),
			})
		}
		return DeleteRequest{Extended: true, ListType: body[0], Entries: entries}, nil
	}

	simple, err := DecodeDeleteOrSellRequest(body)
	if err != nil {
		return DeleteRequest{}, err
	}
	return DeleteRequest{
		ListType:  simple.ListType,
		SlotIndex: simple.SlotIndex,
		Count:     simple.Count,
	}, nil
}

// DecodeDeleteOrSellRequest 解析删除/出售短格式；首字节是已知 list type 时使用带 list 格式。
func unwrapCurrentDeleteStateTrailer(body []byte) ([]byte, bool) {
	if len(body) < 5 {
		return nil, false
	}
	declared := uint64(binary.LittleEndian.Uint32(body[:4]))
	available := uint64(len(body) - 4)
	if declared+1 != available || body[len(body)-1] != 1 || declared > uint64(len(body)-5) {
		return nil, false
	}
	return body[4 : 4+int(declared)], true
}

func DecodeDeleteOrSellRequest(body []byte) (DeleteOrSellRequest, error) {
	if len(body) < 4 {
		return DeleteOrSellRequest{}, shortBodyError(len(body), 4)
	}
	if len(body) >= 5 && isKnownListType(body[0]) {
		return DeleteOrSellRequest{
			HasListType: true,
			ListType:    body[0],
			SlotIndex:   readI16(body, 1),
			Count:       readI16(body, 3),
		}, nil
	}
	return DeleteOrSellRequest{
		ListType:  listTypeMain,
		SlotIndex: readI16(body, 0),
		Count:     readI16(body, 2),
	}, nil
}

// DecodeMoveItemspaceRequest decodes the 28-byte layout emitted by the current
// NoPack.exe senders sub_2326C90, sub_232EA00, and sub_1B33B40.
func DecodeMoveItemspaceRequest(body []byte) (MoveItemspaceRequest, error) {
	const currentMoveItemspaceBodySize = 28
	if len(body) != currentMoveItemspaceBodySize {
		return MoveItemspaceRequest{}, fmt.Errorf("move itemspace body length %d, want %d", len(body), currentMoveItemspaceBodySize)
	}
	return MoveItemspaceRequest{
		SourceListType:           body[0],
		SourceSlotIndex:          readI16(body, 1),
		SourceInstanceValue:      readI32(body, 3),
		MoveCount:                readI32(body, 7),
		DestinationListType:      body[11],
		DestinationSlotIndex:     readI16(body, 12),
		DestinationInstanceValue: readI32(body, 14),
		DestinationStack:         readI32(body, 18),
		ActorIndex:               readI32(body, 22),
		TrailingState0:           body[26],
		TrailingState1:           body[27],
	}, nil
}

// DecodeSortItemRequest 解析整理背包请求。
func DecodeSortItemRequest(body []byte) (SortItemRequest, error) {
	if payload, current, err := unwrapCurrentProtobufBody(body); current {
		if err != nil {
			return SortItemRequest{}, err
		}
		return decodeCurrentSortItemRequest(payload)
	}
	if len(body) < 2 {
		return SortItemRequest{}, shortBodyError(len(body), 2)
	}
	req := SortItemRequest{ListType: body[0], Category: body[1]}
	if len(body) > 2 {
		req.Condition = body[2]
	}
	return req, nil
}

// unwrapCurrentProtobufBody recognizes the current u32-length-prefixed body.
// One zero byte after the protobuf is accepted because current delete senders
// append that terminator. Legacy S4A12 bodies are left to the old decoders.
func unwrapCurrentProtobufBody(body []byte) ([]byte, bool, error) {
	if len(body) < 4 {
		return nil, false, nil
	}
	declared := uint64(binary.LittleEndian.Uint32(body[:4]))
	available := uint64(len(body) - 4)
	if declared == available {
		return body[4:], true, nil
	}
	if declared+1 == available {
		if body[len(body)-1] != 0 {
			return nil, true, fmt.Errorf("protobuf body has non-zero terminator 0x%02X", body[len(body)-1])
		}
		return body[4 : len(body)-1], true, nil
	}
	// Current inventory protobufs are small and therefore have a zero upper
	// length prefix. Treat a mismatch as truncation instead of unsafe fallback.
	if body[1] == 0 && body[2] == 0 && body[3] == 0 {
		return nil, true, fmt.Errorf("protobuf body length %d, want %d or %d with zero terminator", available, declared, declared+1)
	}
	return nil, false, nil
}

func decodeCurrentSortItemRequest(payload []byte) (SortItemRequest, error) {
	var req SortItemRequest
	var seen uint8
	for len(payload) > 0 {
		number, wireType, n := protowire.ConsumeTag(payload)
		if n < 0 {
			return SortItemRequest{}, fmt.Errorf("sort protobuf tag: %w", protowire.ParseError(n))
		}
		payload = payload[n:]
		if wireType != protowire.VarintType || number < 2 || number > 4 {
			return SortItemRequest{}, fmt.Errorf("sort protobuf field %d has unsupported wire type %d", number, wireType)
		}
		value, n := protowire.ConsumeVarint(payload)
		if n < 0 {
			return SortItemRequest{}, fmt.Errorf("sort protobuf field %d: %w", number, protowire.ParseError(n))
		}
		payload = payload[n:]
		if value > math.MaxUint8 {
			return SortItemRequest{}, fmt.Errorf("sort protobuf field %d value %d overflows byte", number, value)
		}
		bit := uint8(1 << (number - 2))
		if seen&bit != 0 {
			return SortItemRequest{}, fmt.Errorf("sort protobuf field %d is duplicated", number)
		}
		seen |= bit
		switch number {
		case 2:
			req.ListType = byte(value)
		case 3:
			req.Category = byte(value)
		case 4:
			req.Condition = byte(value)
		}
	}
	if seen != 0x07 {
		return SortItemRequest{}, fmt.Errorf("sort protobuf required fields missing: mask=0x%02X", seen)
	}
	return req, nil
}

func decodeCurrentDeleteRequest(payload []byte) (DeleteRequest, error) {
	var req DeleteRequest
	req.Extended = true
	var seenListType, seenCondition bool
	for len(payload) > 0 {
		number, wireType, n := protowire.ConsumeTag(payload)
		if n < 0 {
			return DeleteRequest{}, fmt.Errorf("delete protobuf tag: %w", protowire.ParseError(n))
		}
		payload = payload[n:]
		switch number {
		case 2, 4:
			if wireType != protowire.VarintType {
				return DeleteRequest{}, fmt.Errorf("delete protobuf field %d has wire type %d, want varint", number, wireType)
			}
			value, consumed := protowire.ConsumeVarint(payload)
			if consumed < 0 {
				return DeleteRequest{}, fmt.Errorf("delete protobuf field %d: %w", number, protowire.ParseError(consumed))
			}
			payload = payload[consumed:]
			if number == 2 {
				if seenListType || value > math.MaxUint8 {
					return DeleteRequest{}, fmt.Errorf("delete protobuf list type is duplicated or invalid: %d", value)
				}
				req.ListType = byte(value)
				seenListType = true
			} else {
				if seenCondition || value != 0 {
					return DeleteRequest{}, fmt.Errorf("delete protobuf condition is duplicated or non-zero: %d", value)
				}
				seenCondition = true
			}
		case 3:
			if wireType != protowire.BytesType {
				return DeleteRequest{}, fmt.Errorf("delete protobuf field 3 has wire type %d, want bytes", wireType)
			}
			if len(req.Entries) >= maxDeleteEntries {
				return DeleteRequest{}, fmt.Errorf("delete protobuf has more than %d entries", maxDeleteEntries)
			}
			nested, consumed := protowire.ConsumeBytes(payload)
			if consumed < 0 {
				return DeleteRequest{}, fmt.Errorf("delete protobuf entry: %w", protowire.ParseError(consumed))
			}
			payload = payload[consumed:]
			entry, err := decodeCurrentDeleteEntry(nested)
			if err != nil {
				return DeleteRequest{}, err
			}
			req.Entries = append(req.Entries, entry)
		default:
			return DeleteRequest{}, fmt.Errorf("delete protobuf field %d is unsupported", number)
		}
	}
	if !seenListType || !seenCondition || len(req.Entries) == 0 {
		return DeleteRequest{}, fmt.Errorf("delete protobuf required fields missing: list=%t entries=%d condition=%t", seenListType, len(req.Entries), seenCondition)
	}
	return req, nil
}

func decodeCurrentDeleteEntry(payload []byte) (DeleteEntry, error) {
	var entry DeleteEntry
	var seen uint8
	for len(payload) > 0 {
		number, wireType, n := protowire.ConsumeTag(payload)
		if n < 0 {
			return DeleteEntry{}, fmt.Errorf("delete entry protobuf tag: %w", protowire.ParseError(n))
		}
		payload = payload[n:]
		if wireType != protowire.VarintType || number < 1 || number > 4 {
			return DeleteEntry{}, fmt.Errorf("delete entry protobuf field %d has unsupported wire type %d", number, wireType)
		}
		value, consumed := protowire.ConsumeVarint(payload)
		if consumed < 0 {
			return DeleteEntry{}, fmt.Errorf("delete entry protobuf field %d: %w", number, protowire.ParseError(consumed))
		}
		payload = payload[consumed:]
		bit := uint8(1 << (number - 1))
		if seen&bit != 0 {
			return DeleteEntry{}, fmt.Errorf("delete entry protobuf field %d is duplicated", number)
		}
		seen |= bit
		switch number {
		case 1:
			if value > math.MaxInt16 {
				return DeleteEntry{}, fmt.Errorf("delete entry op type %d overflows int16", value)
			}
			entry.OpType = int16(value)
		case 2:
			if value > math.MaxInt16 {
				return DeleteEntry{}, fmt.Errorf("delete entry slot %d overflows int16", value)
			}
			entry.SlotIndex = int16(value)
		case 3:
			if value == 0 || value > math.MaxInt32 {
				return DeleteEntry{}, fmt.Errorf("delete entry item id %d is invalid", value)
			}
			entry.ItemID = int32(value)
		case 4:
			if value == 0 || value > math.MaxInt32 {
				return DeleteEntry{}, fmt.Errorf("delete entry count %d is invalid", value)
			}
			entry.DeleteCount = int32(value)
		}
	}
	if seen != 0x0F {
		return DeleteEntry{}, fmt.Errorf("delete entry protobuf required fields missing: mask=0x%02X", seen)
	}
	return entry, nil
}

// DecodeBuyItemRequest 解析 NPC 购买请求。
func DecodeBuyItemRequest(body []byte) (BuyItemRequest, error) {
	if len(body) < 4 {
		return BuyItemRequest{}, shortBodyError(len(body), 4)
	}
	req := BuyItemRequest{ItemTemplateID: readI32(body, 0), Count: 1}
	if len(body) >= 8 {
		req.Count = readI32(body, 4)
		if req.Count <= 0 {
			req.Count = 1
		}
	}
	return req, nil
}

// DecodeRepairEquipmentRequest 解析装备维修请求。
func DecodeRepairEquipmentRequest(body []byte) (RepairEquipmentRequest, error) {
	if len(body) < 5 {
		return RepairEquipmentRequest{}, shortBodyError(len(body), 5)
	}
	return RepairEquipmentRequest{
		InvenType:      body[0],
		SlotIndex:      readI16(body, 1),
		RepairItemSlot: readI16(body, 3),
		AutoRepair:     len(body) >= 6 && body[5] == 1,
		QuickRepair:    len(body) >= 8 && body[7] == 1,
	}, nil
}

// DecodeDisjointItemRequest 解析物品分解请求。
func DecodeDisjointItemRequest(body []byte) (DisjointItemRequest, error) {
	if len(body) < 5 {
		return DisjointItemRequest{}, shortBodyError(len(body), 5)
	}
	req := DisjointItemRequest{
		TargetSlotIndex:       readI16(body, 0),
		ItemSpace:             body[2],
		DisjointItemSlotIndex: readI16(body, 3),
	}
	if len(body) >= 9 {
		req.ContextValue = readI32(body, 5)
	}
	if req.TargetSlotIndex < 0 {
		return DisjointItemRequest{}, fmt.Errorf("target slot %d invalid", req.TargetSlotIndex)
	}
	return req, nil
}

// DecodeUseStackableRequest 解析使用消耗品请求。
func DecodeUseStackableRequest(body []byte) (UseStackableRequest, error) {
	const currentUseStackableBodySize = 15
	if len(body) != currentUseStackableBodySize {
		return UseStackableRequest{}, fmt.Errorf("use stackable body length %d, want %d", len(body), currentUseStackableBodySize)
	}
	req := UseStackableRequest{
		SlotIndex:     readI16(body, 0),
		ListType:      body[2],
		InstanceValue: readI32(body, 3),
		ItemCode:      readI32(body, 7),
		Reserved:      readU32(body, 11),
	}
	if req.SlotIndex < 0 {
		return UseStackableRequest{}, fmt.Errorf("use stackable slot %d invalid", req.SlotIndex)
	}
	if req.InstanceValue == 0 || req.ItemCode <= 0 {
		return UseStackableRequest{}, fmt.Errorf("use stackable identity invalid: instance=0x%08X item=%d", uint32(req.InstanceValue), req.ItemCode)
	}
	if req.Reserved != 0 {
		return UseStackableRequest{}, fmt.Errorf("use stackable reserved value 0x%08X, want 0", req.Reserved)
	}
	return req, nil
}

// DecodeEventServerLevelUpRequest parses the exact current NoPack op418 body:
// u16 slot followed by u8 inventory list.
func DecodeEventServerLevelUpRequest(body []byte) (EventServerLevelUpRequest, error) {
	const bodySize = 3
	if len(body) != bodySize {
		return EventServerLevelUpRequest{}, fmt.Errorf("event server level-up body length %d, want %d", len(body), bodySize)
	}
	req := EventServerLevelUpRequest{
		SlotIndex: readI16(body, 0),
		ListType:  body[2],
	}
	if req.SlotIndex < 0 {
		return EventServerLevelUpRequest{}, fmt.Errorf("event server level-up slot %d invalid", req.SlotIndex)
	}
	if req.ListType != listTypeMain {
		return EventServerLevelUpRequest{}, fmt.Errorf("event server level-up list %d, want main inventory", req.ListType)
	}
	return req, nil
}

// DecodeIncreaseStatusRequest parses the exact current NoPack op30 body: one
// u16 main-inventory slot. Two live captures carried the double-clicked books'
// own slots (0x42 and 0x43) and nothing else.
func DecodeIncreaseStatusRequest(body []byte) (IncreaseStatusRequest, error) {
	const bodySize = 2
	if len(body) != bodySize {
		return IncreaseStatusRequest{}, fmt.Errorf("increase status body length %d, want %d", len(body), bodySize)
	}
	req := IncreaseStatusRequest{SlotIndex: readI16(body, 0)}
	if req.SlotIndex < 0 {
		return IncreaseStatusRequest{}, fmt.Errorf("increase status slot %d invalid", req.SlotIndex)
	}
	return req, nil
}

// DecodeUseSharedEffectItemRequest parses the exact current type251 body.
func DecodeUseSharedEffectItemRequest(body []byte) (UseSharedEffectItemRequest, error) {
	const bodySize = 6
	if len(body) != bodySize {
		return UseSharedEffectItemRequest{}, fmt.Errorf("use shared effect item body length %d, want %d", len(body), bodySize)
	}
	req := UseSharedEffectItemRequest{
		ItemTemplateID: readI32(body, 0),
		SlotIndex:      int16(body[4]),
		ListType:       body[5],
	}
	if req.ItemTemplateID <= 0 {
		return UseSharedEffectItemRequest{}, fmt.Errorf("use shared effect item template invalid: 0x%08X", uint32(req.ItemTemplateID))
	}
	if err := checkUseStackableListType(req.ListType); err != nil {
		return UseSharedEffectItemRequest{}, err
	}
	return req, nil
}

func DecodeUseStackableActionRequest(body []byte) (UseStackableActionRequest, error) {
	const bodySize = 19
	if len(body) != bodySize {
		return UseStackableActionRequest{}, fmt.Errorf("use stackable action body length %d, want %d", len(body), bodySize)
	}
	req := UseStackableActionRequest{
		SourceSlotIndex: readI16(body, 0),
		ListType:        body[2],
		Reserved0:       readU32(body, 3),
		ActionIndex:     readU32(body, 7),
		Reserved1:       readU32(body, 11),
		Reserved2:       readU32(body, 15),
	}
	if req.SourceSlotIndex < 0 {
		return UseStackableActionRequest{}, fmt.Errorf("use stackable action slot %d invalid", req.SourceSlotIndex)
	}
	if req.ListType != listTypeMain {
		return UseStackableActionRequest{}, fmt.Errorf("use stackable action list %d, want %d", req.ListType, listTypeMain)
	}
	if req.ActionIndex != dungeonAdmissionTicketActionIndex && req.ActionIndex != damageFontActionIndex &&
		req.ActionIndex != questCompletionTicketActionIndex && req.ActionIndex != fatiguePotionActionIndex &&
		req.ActionIndex != rentalPointActionIndex && req.ActionIndex != accountCargoUpgradeActionIndex &&
		req.ActionIndex != favorLetterActionIndex {
		return UseStackableActionRequest{}, fmt.Errorf("use stackable action index %d unsupported", req.ActionIndex)
	}
	// 2026-10-16：action 157（`[action type] [use favor point]`，充满爱慕的信等）的客户端
	// 会在第一个"保留"位置带一个非零值（线上抓包实测 0x11=17，同一包里槽位 0x44=68、
	// 动作号 0x9d=157），其余已支持的动作都还是全零。该字段语义尚未反出来，先按动作号
	// 放行并原样保留，别让它把整条动作挡成 error 17。
	if req.ActionIndex != favorLetterActionIndex && (req.Reserved0 != 0 || req.Reserved1 != 0 || req.Reserved2 != 0) {
		return UseStackableActionRequest{}, fmt.Errorf("use stackable action reserved values (%d,%d,%d), want all zero", req.Reserved0, req.Reserved1, req.Reserved2)
	}
	return req, nil
}

func DecodeSelectDamageFontRequest(body []byte) (SelectDamageFontRequest, error) {
	if len(body) != 2 {
		return SelectDamageFontRequest{}, fmt.Errorf("select damage font body length %d, want 2", len(body))
	}
	return SelectDamageFontRequest{FontIndex: readU16(body, 0)}, nil
}

// DecodeContainerUpgradeRequest 解析 op430/op408 的扩容券请求：只有一个 i16 槽位。
//
// 严格 2 字节：现场 11 次 op430 + 1 次 op408 全是这个形状，多一个字节就说明这不是
// 我们证过的那条路，宁可拒绝也不猜。槽位必须落在背包主表（list 0）且不是账号共享
// 固定槽（354..365），因为券只可能躺在普通背包格里。
func DecodeContainerUpgradeRequest(body []byte) (ContainerUpgradeRequest, error) {
	const bodySize = 2
	if len(body) != bodySize {
		return ContainerUpgradeRequest{}, fmt.Errorf("container upgrade body length %d, want %d", len(body), bodySize)
	}
	req := ContainerUpgradeRequest{
		SlotIndex: readI16(body, 0),
		ListType:  listTypeMain,
	}
	if req.SlotIndex < 0 {
		return ContainerUpgradeRequest{}, fmt.Errorf("container upgrade slot %d invalid", req.SlotIndex)
	}
	if dnfrepo.IsAccountSharedInventorySlot(req.ListType, req.SlotIndex) {
		return ContainerUpgradeRequest{}, fmt.Errorf("container upgrade slot %d is an account-shared fixed slot", req.SlotIndex)
	}
	return req, nil
}

// DecodeUpgradeItemRequest 解析强化/增幅请求。
func DecodeUpgradeItemRequest(body []byte) (UpgradeItemRequest, error) {
	if len(body) < 16 {
		return UpgradeItemRequest{}, shortBodyError(len(body), 16)
	}
	// The current client packs the two-byte mode word as target list (high
	// byte) + sub mode (low byte): 0x0000 list-0 reinforce, 0x0001 list-0
	// amplify, 0x0300 worn list-3 reinforce, 0x0301 worn list-3 amplify.
	// Reading it as one flat u16 and refusing anything above 1 rejected every
	// upgrade on a worn item at decode with "raw mode 768 invalid".
	rawMode := readU16(body, 0)
	listType := byte(rawMode >> 8)
	subMode := byte(rawMode & 0xFF)
	// 公会徽章 (list 38) is a reinforce-only path. Upstream refuses list 38 with
	// an amplify sub mode and confines it to the medal page slots 0..48: the
	// native flag_upgrade_table is keyed by flag level, so there is no row for
	// an amplify and nothing outside the page to address.
	targetSlot := readI16(body, 2)
	guildMedal := listType == listTypeGuildMedal && subMode == 0 && targetSlot >= 0 && targetSlot <= 48
	if listType != listTypeMain && listType != listTypeEquipment && !guildMedal {
		return UpgradeItemRequest{}, fmt.Errorf("raw mode 0x%04X invalid: target list %d unsupported", rawMode, listType)
	}
	// Sub mode 2 is the 高级装备强化机: the client names the 高级炉岩炭 stack in
	// the material slot instead of the 无色小晶块 the ordinary machine uses.
	// Live capture 2026-09-11 13:35:37, mode 0x0002, material slot 126 holding
	// item 2681644 (高级炉岩炭). It was rejected outright until then, so the
	// machine had never worked on this server.
	if subMode > 2 {
		return UpgradeItemRequest{}, fmt.Errorf("raw mode 0x%04X invalid: sub mode %d unsupported", rawMode, subMode)
	}
	nameLength := readI32(body, 12)
	if nameLength < 0 {
		return UpgradeItemRequest{}, fmt.Errorf("name length %d invalid", nameLength)
	}
	end := 16 + int(nameLength)
	if len(body) < end {
		return UpgradeItemRequest{}, shortBodyError(len(body), end)
	}
	mode := "reinforce"
	if subMode == 1 {
		mode = "amplify"
	}
	return UpgradeItemRequest{
		Mode:                    mode,
		HighUpgrade:             subMode == 2,
		RawMode:                 rawMode,
		TargetListType:          listType,
		TargetSlotIndex:         targetSlot,
		TargetItemTemplateID:    readI32(body, 4),
		MaterialSlotIndex:       readI16(body, 8),
		OptionalTicketSlotIndex: readI16(body, 10),
		TargetItemName:          string(body[16:end]),
	}, nil
}

// DecodeSeparateUpgradeRequest decodes the exact current-client op438 body:
// u8 list, i16 target slot, i32 item id, i16 material slot, DSTR name, u8 tail.
func DecodeChronicleGrowthRequest(body []byte) (ChronicleGrowthRequest, error) {
	if len(body) != 19 {
		return ChronicleGrowthRequest{}, fmt.Errorf("chronicle growth body length %d, want 19", len(body))
	}
	req := ChronicleGrowthRequest{
		TicketSlotIndex:   readI16(body, 0),
		TicketItemCode:    readI32(body, 2),
		TargetSlotIndex:   readI16(body, 6),
		TargetItemCode:    readI32(body, 8),
		TargetTaggedValue: body[12],
		MaterialSlotIndex: readI16(body, 13),
		MaterialItemCode:  readI32(body, 15),
	}
	if req.TicketSlotIndex < 0 || req.TargetSlotIndex < 0 ||
		req.TicketItemCode <= 0 || req.TargetItemCode <= 0 {
		return ChronicleGrowthRequest{}, fmt.Errorf("chronicle growth request contains invalid slot/id")
	}
	// Equipment-emancipation books own a multi-material recipe, so the current
	// client cannot name one material stack in this fixed body.  It sends the
	// native (-1, 0xffff) auto-material sentinel instead.  Ordinary Chronicle
	// growth still names one concrete material stack and item code.
	autoMaterials := req.MaterialSlotIndex == -1 && req.MaterialItemCode == 0xffff
	explicitMaterial := req.MaterialSlotIndex >= 0 && req.MaterialItemCode > 0
	if !autoMaterials && !explicitMaterial {
		return ChronicleGrowthRequest{}, fmt.Errorf("chronicle growth request contains invalid material slot/id")
	}
	if req.TicketSlotIndex == req.TargetSlotIndex ||
		(req.MaterialSlotIndex >= 0 && (req.MaterialSlotIndex == req.TargetSlotIndex || req.MaterialSlotIndex == req.TicketSlotIndex)) {
		return ChronicleGrowthRequest{}, fmt.Errorf("chronicle growth slots overlap")
	}
	return req, nil
}

func DecodeSeparateUpgradeRequest(body []byte) (SeparateUpgradeRequest, error) {
	const fixedSizeBeforeName = 13
	const minimumBodySize = fixedSizeBeforeName + 1
	if len(body) < minimumBodySize {
		return SeparateUpgradeRequest{}, shortBodyError(len(body), minimumBodySize)
	}
	nameLength := binary.LittleEndian.Uint32(body[9:13])
	if uint64(nameLength) > uint64(len(body)-minimumBodySize) {
		return SeparateUpgradeRequest{}, fmt.Errorf("separate upgrade name length %d exceeds body %d", nameLength, len(body))
	}
	nameEnd := fixedSizeBeforeName + int(nameLength)
	if nameEnd+1 != len(body) {
		return SeparateUpgradeRequest{}, fmt.Errorf("separate upgrade body length %d, want %d from DSTR", len(body), nameEnd+1)
	}
	return SeparateUpgradeRequest{
		TargetListType:       body[0],
		TargetSlotIndex:      readI16(body, 1),
		TargetItemTemplateID: readI32(body, 3),
		MaterialSlotIndex:    readI16(body, 7),
		TargetItemName:       string(body[fixedSizeBeforeName:nameEnd]),
		TrailingFlag:         body[nameEnd],
	}, nil
}

// DecodeChronicleRefineRequest decodes the fixed 14-byte body emitted by
// SendChronicleRefineRequest_Op370 in the current client.
func DecodeChronicleRefineRequest(body []byte) (ChronicleRefineRequest, error) {
	const bodySize = 14
	if len(body) != bodySize {
		return ChronicleRefineRequest{}, fmt.Errorf("chronicle refine body length %d, want %d", len(body), bodySize)
	}
	return ChronicleRefineRequest{
		MaterialSlotIndex:      readI16(body, 0),
		MaterialItemTemplateID: readI32(body, 2),
		TargetListType:         body[6],
		TargetSlotIndex:        readI16(body, 7),
		TargetItemTemplateID:   readI32(body, 9),
		OptionNo:               body[13],
	}, nil
}

// DecodeEnchantByBeadRequest 解析宝珠附魔请求。
func DecodeEnchantByBeadRequest(body []byte) (EnchantByBeadRequest, error) {
	if len(body) < 6 {
		return EnchantByBeadRequest{}, shortBodyError(len(body), 6)
	}
	return EnchantByBeadRequest{
		BeadListType:    body[0],
		BeadSlotIndex:   readI16(body, 1),
		TargetListType:  body[3],
		TargetSlotIndex: readI16(body, 4),
	}, nil
}

// DecodeMonsterCardUpgradeRequest decodes the fixed seven-byte op603 body:
// u8 inventory list, i16 target slot, i16 material count, i16 material slot.
func DecodeMonsterCardUpgradeRequest(body []byte) (MonsterCardUpgradeRequest, error) {
	if len(body) != 7 {
		return MonsterCardUpgradeRequest{}, fmt.Errorf("monster card upgrade body=%d want=7", len(body))
	}
	return MonsterCardUpgradeRequest{
		ListType:      body[0],
		TargetSlot:    readI16(body, 1),
		MaterialCount: readI16(body, 3),
		MaterialSlot:  readI16(body, 5),
	}, nil
}

func (r DeleteRequest) String() string {
	if r.Extended {
		return fmt.Sprintf("extended list=%d entries=%d", r.ListType, len(r.Entries))
	}
	return fmt.Sprintf("list=%d slot=%d count=%d", r.ListType, r.SlotIndex, r.Count)
}

func (r DeleteOrSellRequest) String() string {
	return fmt.Sprintf("hasList=%t list=%d slot=%d count=%d", r.HasListType, r.ListType, r.SlotIndex, r.Count)
}

func (r MoveItemspaceRequest) String() string {
	return fmt.Sprintf("src=(%d,%d,0x%08X) count=%d dst=(%d,%d,0x%08X) dstStack=%d actor=%d tail=(%d,%d)", r.SourceListType, r.SourceSlotIndex, uint32(r.SourceInstanceValue), r.MoveCount, r.DestinationListType, r.DestinationSlotIndex, uint32(r.DestinationInstanceValue), r.DestinationStack, r.ActorIndex, r.TrailingState0, r.TrailingState1)
}

func (r SortItemRequest) String() string {
	return fmt.Sprintf("list=%d category=%d condition=%d", r.ListType, r.Category, r.Condition)
}

func (r BuyItemRequest) String() string {
	return fmt.Sprintf("item=%d count=%d", r.ItemTemplateID, r.Count)
}

func (r RepairEquipmentRequest) String() string {
	return fmt.Sprintf("inven=%d slot=%d repairSlot=%d auto=%t quick=%t", r.InvenType, r.SlotIndex, r.RepairItemSlot, r.AutoRepair, r.QuickRepair)
}

func (r DisjointItemRequest) String() string {
	return fmt.Sprintf("targetSlot=%d itemSpace=%d disjointSlot=%d ctx=0x%08X", r.TargetSlotIndex, r.ItemSpace, r.DisjointItemSlotIndex, uint32(r.ContextValue))
}

func (r UseStackableRequest) String() string {
	return fmt.Sprintf("slot=%d list=%d instance=0x%08X item=0x%08X reserved=0x%08X", r.SlotIndex, r.ListType, uint32(r.InstanceValue), uint32(r.ItemCode), r.Reserved)
}

func (r EventServerLevelUpRequest) String() string {
	return fmt.Sprintf("slot=%d list=%d", r.SlotIndex, r.ListType)
}

func (r UseSharedEffectItemRequest) String() string {
	return fmt.Sprintf("slot=%d list=%d item=0x%08X", r.SlotIndex, r.ListType, uint32(r.ItemTemplateID))
}

func (r UseStackableActionRequest) String() string {
	return fmt.Sprintf("slot=%d list=%d action=%d reserved=(%d,%d,%d)", r.SourceSlotIndex, r.ListType, r.ActionIndex, r.Reserved0, r.Reserved1, r.Reserved2)
}

func (r SelectDamageFontRequest) String() string {
	return fmt.Sprintf("font=%d", r.FontIndex)
}

func (r UpgradeItemRequest) String() string {
	return fmt.Sprintf("mode=%s target=(%d,%d) material=%d ticket=%d name=%q", r.Mode, r.TargetSlotIndex, r.TargetItemTemplateID, r.MaterialSlotIndex, r.OptionalTicketSlotIndex, r.TargetItemName)
}

func (r SeparateUpgradeRequest) String() string {
	return fmt.Sprintf("target=(%d,%d,%d) material=%d name=%q tail=%d", r.TargetListType, r.TargetSlotIndex, r.TargetItemTemplateID, r.MaterialSlotIndex, r.TargetItemName, r.TrailingFlag)
}

func (r EnchantByBeadRequest) String() string {
	return fmt.Sprintf("bead=(%d,%d) target=(%d,%d)", r.BeadListType, r.BeadSlotIndex, r.TargetListType, r.TargetSlotIndex)
}

func isKnownListType(v byte) bool {
	switch v {
	case listTypeMain, listTypeAvatar, listTypePersonalCargo, listTypeEquipment, listTypePet, listTypeAccountCargo, listTypeGuildMedal:
		return true
	default:
		return false
	}
}

func shortBodyError(got int, want int) error {
	return fmt.Errorf("body too short: got %d want >= %d", got, want)
}

func readI16(body []byte, offset int) int16 {
	return int16(binary.LittleEndian.Uint16(body[offset:]))
}

func readU16(body []byte, offset int) uint16 {
	return binary.LittleEndian.Uint16(body[offset:])
}

func readI32(body []byte, offset int) int32 {
	return int32(binary.LittleEndian.Uint32(body[offset:]))
}

func readU32(body []byte, offset int) uint32 {
	return binary.LittleEndian.Uint32(body[offset:])
}
