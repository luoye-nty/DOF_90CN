package dnfbridge

import (
	"bytes"
	"context"
	"encoding/binary"
	"errors"
	"fmt"
	"math"
	"math/rand/v2"
	"strconv"
	"strings"

	"longheng.io/server/internal/modules/dnf/alignedcmd"
	"longheng.io/server/internal/modules/dnf/dnfenum"
	dnfexpertjob "longheng.io/server/internal/modules/dnf/expertjob"
	dnfproto "longheng.io/server/internal/modules/dnf/protocol"
	dnfrepo "longheng.io/server/internal/modules/dnf/repository"
)

const (
	currentExpertJobDisjointStoreKind byte = 0
	currentExpertJobEnchantStoreKind  byte = 3

	currentExpertJobStoreCreateNotification  uint16 = 538
	currentExpertJobStoreCloseNotification   uint16 = 539
	currentExpertJobStoreUpdateNotification  uint16 = 544
	currentExpertJobEnchantOwnerNotification uint16 = 533
	// currentExpertJobStoreVisibleAttachMode is the CREATE NOTI's trailing mode
	// byte. The 86 reference (ExpertJobStorePacketBuilder.ReplaceOwnerAvatarMode,
	// 副职业架构业务接入规范 五.1) writes 0 and documents the value as:
	// mode=0 turns the owner's existing town clone into the store and creates no
	// further entity, while mode=1 creates an additional store entity and leaves
	// that clone behind as a second body ("残留分身").
	//
	// This server wrote 2, which is neither of the reference's two values: a live
	// client builds the extra stand-in entity for it, so every peer ends up with a
	// default-appearance body covering the store owner. 2 was inherited from the
	// upstream tree rather than measured here, and the client value that seemed to
	// require it (store entity +0x3C4 == 2) is only a copy of this very byte: the
	// draw gate at 0x01A80D50 admits either mode == 2 or a non-zero entity+0x358,
	// and the 0 path satisfies the second condition (live read with 0 on the wire:
	// mode=0, gate=1, same draw routine, no stand-in object).
	//
	// Verified live 2026-09-22 with no client patch injected: no phantom on the
	// peer, store visible / clickable / usable on both sides, correct label, owner
	// appearance restored after close, and correct across area change, private
	// room entry and relogin. Backup of the mode=2 source:
	// _backup_expert_job_store_mode0_20260922-001841.
	currentExpertJobStoreVisibleAttachMode byte = 0
)

// 【个人商店分解机批量分解 2026-09-23】
//
// 客户端实测（脚本与日志见 client-patch\probe_store_disjoint\README.md）：
// 个人商店分解机的"全部分解"在**一趟循环里、1 毫秒内**就把 N 件全发出去
// （8 件实测：SEND235 slot=9..16，sendSpanMs=0，[win+0x1B8] 全程为 1，
// [win+0x1EC]=0），与 op26（NPC 分解机）的循环行为完全同构。
//
// 所以慢的不是客户端发包节奏，而是"N 个包 = N 个事务"，而每个事务还要
// **重建 5 个容器列表**（currentSelectedItemListTypes = {0,1,2,宠物,公会勋章}）
// + 给店主发一次金币 + 发一次 op205。
//
// 这里给 op235 加一个与 op26 同形的批量信封，让客户端钩子把 N 个逐件命令合成一个：
//
//	u16 ownerID | u16 0xFFFF 哨兵 | u8 listType | u32 "B35\x01" | u8 count | i16 slots[count]
//
// 长度 10 + 2*count。哨兵是负槽位，单件解析器本来就拒绝，因此与现有 5 字节单件
// 请求**不冲突**；服务端继续接受单件形态，未注入钩子的客户端完全不受影响。
const (
	currentExpertJobStoreDisjointBatchSentinel int16 = -1
	currentExpertJobStoreDisjointBatchMaxItems       = 127
	// ownerID(2) + sentinel(2) + listType(1) + marker(4) + count(1)
	currentExpertJobStoreDisjointBatchHeaderSize = 10
)

var currentExpertJobStoreDisjointBatchMarker = [4]byte{'B', '3', '5', 1}

// currentExpertJobStoreDisjointBatchRequest 是客户端钩子合成的一个批量命令。
type currentExpertJobStoreDisjointBatchRequest struct {
	OwnerID    uint16
	TargetList byte
	Slots      []int16
}

type currentExpertJobStore struct {
	OwnerCharacterID uint16
	OwnerSession     *gameSession
	Kind             byte
	Name             []byte
	Cost             int64
	TownID           byte
	AreaID           byte
	InstanceKey      uint32
	PositionX        int16
	PositionY        int16
	OpaqueObjectLink uint16
	MachineGrade     int
	Endurance        int64
	Qualifications   []byte
}

type currentExpertJobStoreCreateRequest struct {
	Kind             byte
	Name             []byte
	Cost             int64
	PositionX        int16
	PositionY        int16
	OpaqueObjectLink uint16
}

type currentExpertJobStoreDisjointRequest struct {
	OwnerID    uint16
	TargetSlot int16
	TargetList byte
}

type currentExpertJobStoreEnchantRequest struct {
	OwnerID    uint16
	RecipeID   int64
	TargetList byte
	TargetSlot int16
	CardList   byte
	CardSlot   int16
}

type currentExpertJobStoreUseResult struct {
	RequesterGold  int64
	OwnerGold      int64
	Endurance      int64
	Experience     int64
	LevelChanged   bool
	Success        bool
	TargetList     byte
	TargetSlot     int16
	Materials      []currentExpertJobExtractionMaterial
	Qualifications []byte
}

func parseCurrentExpertJobStoreCreateRequest(body []byte) (currentExpertJobStoreCreateRequest, error) {
	if len(body) < 15 {
		return currentExpertJobStoreCreateRequest{}, fmt.Errorf("expert store create body=%d", len(body))
	}
	nameLength := int(int32(binary.LittleEndian.Uint32(body[1:5])))
	if nameLength < 0 || nameLength > math.MaxUint8 || len(body) != 15+nameLength {
		return currentExpertJobStoreCreateRequest{}, errors.New("expert store name boundary is invalid")
	}
	offset := 5 + nameLength
	request := currentExpertJobStoreCreateRequest{
		Kind: body[0], Name: append([]byte(nil), body[5:offset]...),
		Cost:             int64(binary.LittleEndian.Uint32(body[offset : offset+4])),
		PositionX:        int16(binary.LittleEndian.Uint16(body[offset+4 : offset+6])),
		PositionY:        int16(binary.LittleEndian.Uint16(body[offset+6 : offset+8])),
		OpaqueObjectLink: binary.LittleEndian.Uint16(body[offset+8 : offset+10]),
	}
	if nameLength == 0 || (request.Kind != currentExpertJobDisjointStoreKind && request.Kind != currentExpertJobEnchantStoreKind) {
		return currentExpertJobStoreCreateRequest{}, errors.New("expert store create values are invalid")
	}
	return request, nil
}

func parseCurrentExpertJobStoreDisjointRequest(body []byte) (currentExpertJobStoreDisjointRequest, error) {
	if len(body) != 5 {
		return currentExpertJobStoreDisjointRequest{}, fmt.Errorf("store disjoint body=%d", len(body))
	}
	request := currentExpertJobStoreDisjointRequest{OwnerID: binary.LittleEndian.Uint16(body[0:2]), TargetSlot: int16(binary.LittleEndian.Uint16(body[2:4])), TargetList: body[4]}
	if request.OwnerID == 0 || request.TargetSlot < 0 || request.TargetList != dnfrepo.MainInventoryListType {
		return currentExpertJobStoreDisjointRequest{}, errors.New("store disjoint values are invalid")
	}
	return request, nil
}

// parseCurrentExpertJobStoreDisjointBatchRequest 识别客户端钩子合成的批量命令。
// 第二个返回值表示"这是一个批量信封"（即使内容非法），以便调用方区分"不是批量"
// 与"批量但坏了"——与 op26 的 parseCurrentDisjointBatchRequest 同一约定。
func parseCurrentExpertJobStoreDisjointBatchRequest(body []byte) (currentExpertJobStoreDisjointBatchRequest, bool, error) {
	if len(body) < currentExpertJobStoreDisjointBatchHeaderSize ||
		int16(binary.LittleEndian.Uint16(body[2:4])) != currentExpertJobStoreDisjointBatchSentinel ||
		body[5] != currentExpertJobStoreDisjointBatchMarker[0] ||
		body[6] != currentExpertJobStoreDisjointBatchMarker[1] ||
		body[7] != currentExpertJobStoreDisjointBatchMarker[2] ||
		body[8] != currentExpertJobStoreDisjointBatchMarker[3] {
		return currentExpertJobStoreDisjointBatchRequest{}, false, nil
	}
	count := int(body[9])
	if count < 1 || count > currentExpertJobStoreDisjointBatchMaxItems ||
		len(body) != currentExpertJobStoreDisjointBatchHeaderSize+count*2 {
		return currentExpertJobStoreDisjointBatchRequest{}, true,
			fmt.Errorf("store disjoint batch count=%d body=%d", count, len(body))
	}
	request := currentExpertJobStoreDisjointBatchRequest{
		OwnerID:    binary.LittleEndian.Uint16(body[0:2]),
		TargetList: body[4],
		Slots:      make([]int16, 0, count),
	}
	if request.OwnerID == 0 || request.TargetList != dnfrepo.MainInventoryListType {
		return currentExpertJobStoreDisjointBatchRequest{}, true,
			fmt.Errorf("store disjoint batch owner=%d list=%d", request.OwnerID, request.TargetList)
	}
	seen := make(map[int16]struct{}, count)
	for index := 0; index < count; index++ {
		offset := currentExpertJobStoreDisjointBatchHeaderSize + index*2
		slot := int16(binary.LittleEndian.Uint16(body[offset : offset+2]))
		if slot < 0 || dnfrepo.IsAccountSharedInventorySlot(request.TargetList, slot) {
			return currentExpertJobStoreDisjointBatchRequest{}, true,
				fmt.Errorf("store disjoint batch index=%d slot=%d", index, slot)
		}
		if _, duplicate := seen[slot]; duplicate {
			return currentExpertJobStoreDisjointBatchRequest{}, true,
				fmt.Errorf("store disjoint batch duplicate slot=%d", slot)
		}
		seen[slot] = struct{}{}
		request.Slots = append(request.Slots, slot)
	}
	return request, true, nil
}

func parseCurrentExpertJobStoreEnchantRequest(body []byte) (currentExpertJobStoreEnchantRequest, error) {
	if len(body) != 13 {
		return currentExpertJobStoreEnchantRequest{}, fmt.Errorf("store enchant body=%d", len(body))
	}
	request := currentExpertJobStoreEnchantRequest{
		OwnerID: binary.LittleEndian.Uint16(body[0:2]), RecipeID: int64(int32(binary.LittleEndian.Uint32(body[2:6]))),
		TargetList: body[7], TargetSlot: int16(binary.LittleEndian.Uint16(body[8:10])), CardList: body[10], CardSlot: int16(binary.LittleEndian.Uint16(body[11:13])),
	}
	if request.OwnerID == 0 || request.RecipeID <= 0 || body[6] != 2 || request.TargetList != dnfrepo.MainInventoryListType || request.CardList != dnfrepo.MainInventoryListType || request.TargetSlot < 0 || request.CardSlot < 0 || request.TargetSlot == request.CardSlot {
		return currentExpertJobStoreEnchantRequest{}, errors.New("store enchant values are invalid")
	}
	return request, nil
}

func (s *Service) handleCurrentExpertJobStoreCreate(session *gameSession, opcode uint16, body []byte) error {
	request, err := parseCurrentExpertJobStoreCreateRequest(body)
	if err != nil {
		return s.sendGameUpperFailure(session, opcode, 19)
	}
	s.expertJobStoreOpMu.Lock()
	defer s.expertJobStoreOpMu.Unlock()
	store, err := s.createCurrentExpertJobStore(session, request)
	if err != nil {
		return s.sendGameUpperFailure(session, opcode, currentExpertJobStoreError(err))
	}
	createBody := buildCurrentExpertJobStoreCreateNotification(store)
	if err := s.sendGameUpperRawClass(session, opcode, []byte{1}, dnfproto.DefaultChannelClassification); err != nil {
		s.removeCurrentExpertJobStore(store.OwnerCharacterID, store.OwnerSession)
		return err
	}
	// op598 creates the owner's local object. NOTI544 attaches enchanter
	// metadata to that object, while NOTI538 is strictly the peer projection.
	if store.Kind == currentExpertJobEnchantStoreKind {
		if err := s.sendGameUpperRawClass(session, currentExpertJobStoreUpdateNotification, buildCurrentExpertJobStoreUpdateNotification(store), 0); err != nil {
			s.removeCurrentExpertJobStore(store.OwnerCharacterID, store.OwnerSession)
			return err
		}
	}
	for _, peer := range s.onlinePlayers.PeersInSameArea(store.OwnerCharacterID) {
		if peer.Session != nil {
			_ = s.sendGameUpperRawClass(peer.Session, currentExpertJobStoreCreateNotification, createBody, 0)
		}
	}
	return nil
}

func (s *Service) createCurrentExpertJobStore(session *gameSession, request currentExpertJobStoreCreateRequest) (*currentExpertJobStore, error) {
	if s == nil || session == nil || session.selectedCharacterID == 0 || s.onlinePlayers == nil || currentExpertJobSessionInDungeon(session) || runtimePartyStateSnapshot(session).PartyID != 0 {
		return nil, dnfexpertjob.ErrMachineInvalid
	}
	player, ok := s.onlinePlayers.PlayerForCharacter(session.selectedCharacterID)
	if !ok || player.Session != session || player.TownID == 0 {
		return nil, dnfexpertjob.ErrMachineInvalid
	}
	repositories, ok := s.repositoryGroup()
	if !ok || repositories.Character == nil {
		return nil, dnfexpertjob.ErrOwnerUnavailable
	}
	catalog, err := s.currentExpertJobCatalog()
	if err != nil {
		return nil, err
	}
	ctx, cancel := context.WithTimeout(context.Background(), currentRepositorySnapshotTimeout)
	defer cancel()
	characterID := strconv.FormatUint(uint64(session.selectedCharacterID), 10)
	character, found, err := repositories.Character.Load(ctx, characterID)
	if err != nil || !found || character.CharacterID != characterID {
		return nil, dnfexpertjob.ErrCharacterNotFound
	}
	jobType, experience, err := currentExpertJobCharacterState(&character)
	if err != nil {
		return nil, err
	}
	store := &currentExpertJobStore{OwnerCharacterID: session.selectedCharacterID, OwnerSession: session, Kind: request.Kind, Name: request.Name, Cost: request.Cost, TownID: player.TownID, AreaID: player.AreaID, InstanceKey: player.InstanceKey, PositionX: request.PositionX, PositionY: request.PositionY, OpaqueObjectLink: request.OpaqueObjectLink}
	switch request.Kind {
	case currentExpertJobDisjointStoreKind:
		config, ok := catalog.Disjointer()
		if !ok || jobType != dnfexpertjob.DisjointerType || request.Cost > config.MaximumStoreCharge {
			return nil, dnfexpertjob.ErrMachineInvalid
		}
		store.MachineGrade = int(currentExpertJobStatDefault(character.Stats, currentExpertJobMachineGradeStat, 1))
		store.Endurance = currentExpertJobStatDefault(character.Stats, currentExpertJobMachineEnduranceStat, config.InitialEndurance)
		if _, ok := config.RepairRule(store.MachineGrade); !ok || store.Endurance <= 0 {
			return nil, dnfexpertjob.ErrMachineEndurance
		}
	case currentExpertJobEnchantStoreKind:
		config, ok := catalog.Enchanter()
		if !ok || jobType != dnfexpertjob.EnchanterType || request.Cost > config.MaximumStoreCharge {
			return nil, dnfexpertjob.ErrMachineInvalid
		}
		store.Endurance = currentExpertJobStatDefault(character.Stats, currentExpertJobMachineEnduranceStat, config.InitialEndurance)
		store.Qualifications = config.Qualifications(experience)
		if store.Endurance <= 0 {
			return nil, dnfexpertjob.ErrMachineEndurance
		}
	default:
		return nil, dnfexpertjob.ErrMachineInvalid
	}
	s.expertJobStoreMu.Lock()
	defer s.expertJobStoreMu.Unlock()
	if s.expertJobStores == nil {
		s.expertJobStores = make(map[uint16]*currentExpertJobStore)
	}
	if _, exists := s.expertJobStores[store.OwnerCharacterID]; exists {
		return nil, dnfexpertjob.ErrMachineInvalid
	}
	s.expertJobStores[store.OwnerCharacterID] = store
	return cloneCurrentExpertJobStore(store), nil
}

func (s *Service) handleCurrentExpertJobStoreEnter(session *gameSession, opcode uint16, body []byte) error {
	if len(body) != 2 || session == nil || session.selectedCharacterID == 0 || currentExpertJobSessionInDungeon(session) {
		return s.sendGameUpperFailure(session, opcode, 19)
	}
	ownerID := binary.LittleEndian.Uint16(body)
	store, ok := s.enterCurrentExpertJobStore(session, ownerID)
	if !ok {
		return s.sendGameUpperFailure(session, opcode, 19)
	}
	return s.sendGameUpperRawClass(session, opcode, buildCurrentExpertJobStoreEnterSuccess(store), dnfproto.DefaultChannelClassification)
}

func (s *Service) enterCurrentExpertJobStore(session *gameSession, ownerID uint16) (*currentExpertJobStore, bool) {
	if s == nil || session == nil || ownerID == 0 || s.onlinePlayers == nil {
		return nil, false
	}
	visitor, visitorOK := s.onlinePlayers.PlayerForCharacter(session.selectedCharacterID)
	s.expertJobStoreMu.Lock()
	defer s.expertJobStoreMu.Unlock()
	store := s.expertJobStores[ownerID]
	if !visitorOK || visitor.Session != session || store == nil || store.OwnerSession == nil || store.TownID != visitor.TownID || store.AreaID != visitor.AreaID || store.InstanceKey != visitor.InstanceKey {
		return nil, false
	}
	if s.expertJobVisitors == nil {
		s.expertJobVisitors = make(map[*gameSession]uint16)
	}
	s.expertJobVisitors[session] = ownerID
	return cloneCurrentExpertJobStore(store), true
}

func (s *Service) handleCurrentExpertJobStoreClose(session *gameSession, opcode uint16, body []byte) error {
	if len(body) != 0 || session == nil || session.selectedCharacterID == 0 {
		return s.sendGameUpperFailure(session, opcode, 19)
	}
	s.expertJobStoreOpMu.Lock()
	defer s.expertJobStoreOpMu.Unlock()
	store := s.removeCurrentExpertJobStore(session.selectedCharacterID, session)
	if store != nil {
		s.broadcastCurrentExpertJobStoreClose(store, true)
		return nil
	}
	s.expertJobStoreMu.Lock()
	_, visitor := s.expertJobVisitors[session]
	delete(s.expertJobVisitors, session)
	s.expertJobStoreMu.Unlock()
	if visitor {
		return nil
	}
	return s.sendGameUpperFailure(session, opcode, 19)
}

func (s *Service) removeCurrentExpertJobStore(ownerID uint16, ownerSession *gameSession) *currentExpertJobStore {
	if s == nil || ownerID == 0 {
		return nil
	}
	s.expertJobStoreMu.Lock()
	defer s.expertJobStoreMu.Unlock()
	store := s.expertJobStores[ownerID]
	if store == nil || (ownerSession != nil && store.OwnerSession != ownerSession) {
		return nil
	}
	delete(s.expertJobStores, ownerID)
	for visitor, enteredOwner := range s.expertJobVisitors {
		if enteredOwner == ownerID {
			delete(s.expertJobVisitors, visitor)
		}
	}
	return cloneCurrentExpertJobStore(store)
}

func (s *Service) closeCurrentExpertJobStoreSession(session *gameSession, includeOwner bool) {
	if s == nil || session == nil {
		return
	}
	s.expertJobStoreOpMu.Lock()
	defer s.expertJobStoreOpMu.Unlock()
	store := s.removeCurrentExpertJobStore(session.selectedCharacterID, session)
	s.expertJobStoreMu.Lock()
	delete(s.expertJobVisitors, session)
	s.expertJobStoreMu.Unlock()
	if store != nil {
		s.broadcastCurrentExpertJobStoreClose(store, includeOwner)
	}
}

func (s *Service) broadcastCurrentExpertJobStoreClose(store *currentExpertJobStore, includeOwner bool) {
	if s == nil || store == nil || s.onlinePlayers == nil {
		return
	}
	body := buildCurrentExpertJobStoreCloseNotification(store.OwnerCharacterID)
	for _, player := range s.onlinePlayers.GetAreaInstancePlayers(
		townPresenceChannelID(store.OwnerSession),
		store.TownID, store.AreaID, store.InstanceKey) {
		if player.Session == nil || (!includeOwner && player.CharacterID == store.OwnerCharacterID) {
			continue
		}
		_ = s.sendGameUpperRawClass(player.Session, currentExpertJobStoreCloseNotification, body, 0)
	}
}

func (s *Service) replayCurrentExpertJobStores(session *gameSession, townID, areaID byte) {
	if s == nil || session == nil {
		return
	}
	instanceKey := uint32(0)
	if s.onlinePlayers != nil {
		if player, ok := s.onlinePlayers.PlayerForCharacter(session.selectedCharacterID); ok && player.Session == session {
			instanceKey = player.InstanceKey
		}
	}
	s.expertJobStoreMu.Lock()
	stores := make([]*currentExpertJobStore, 0)
	for _, store := range s.expertJobStores {
		if store.TownID == townID && store.AreaID == areaID && store.InstanceKey == instanceKey && store.OwnerSession != session {
			stores = append(stores, cloneCurrentExpertJobStore(store))
		}
	}
	s.expertJobStoreMu.Unlock()
	for _, store := range stores {
		_ = s.sendGameUpperRawClass(session, currentExpertJobStoreCreateNotification, buildCurrentExpertJobStoreCreateNotification(store), 0)
	}
}

func cloneCurrentExpertJobStore(store *currentExpertJobStore) *currentExpertJobStore {
	if store == nil {
		return nil
	}
	clone := *store
	clone.Name = append([]byte(nil), store.Name...)
	clone.Qualifications = append([]byte(nil), store.Qualifications...)
	return &clone
}

func currentExpertJobSessionInDungeon(session *gameSession) bool {
	if session == nil {
		return true
	}
	session.dungeon.mu.Lock()
	active := session.dungeon.runtime != nil
	session.dungeon.mu.Unlock()
	return active
}

func buildCurrentExpertJobStoreCreateNotification(store *currentExpertJobStore) []byte {
	w := packetWriter{}
	w.writeByte(store.Kind)
	w.writeUint16(store.OwnerCharacterID)
	w.writeRawDstr(store.Name)
	w.writeByte(store.TownID)
	w.writeUint32(uint32(store.AreaID))
	w.writeUint16(uint16(store.PositionX))
	w.writeUint16(uint16(store.PositionY))
	w.writeUint32(uint32(store.Cost))
	w.writeByte(currentExpertJobStoreVisibleAttachMode)
	if store.Kind == currentExpertJobEnchantStoreKind {
		w.writeByte(byte(len(store.Qualifications)))
		w.writeBytes(store.Qualifications)
	}
	return w.bytes()
}

func buildCurrentExpertJobStoreUpdateNotification(store *currentExpertJobStore) []byte {
	w := packetWriter{}
	w.writeByte(store.Kind)
	w.writeUint16(store.OwnerCharacterID)
	w.writeRawDstr(store.Name)
	if store.Kind == currentExpertJobEnchantStoreKind {
		w.writeByte(byte(len(store.Qualifications)))
		w.writeBytes(store.Qualifications)
	}
	return w.bytes()
}

func (s *Service) broadcastCurrentExpertJobStoreUpdate(store *currentExpertJobStore) {
	if s == nil || store == nil || s.onlinePlayers == nil {
		return
	}
	body := buildCurrentExpertJobStoreUpdateNotification(store)
	for _, player := range s.onlinePlayers.GetAreaInstancePlayers(
		townPresenceChannelID(store.OwnerSession),
		store.TownID, store.AreaID, store.InstanceKey) {
		if player.Session != nil {
			_ = s.sendGameUpperRawClass(player.Session, currentExpertJobStoreUpdateNotification, body, 0)
		}
	}
}

func buildCurrentExpertJobStoreCloseNotification(ownerID uint16) []byte {
	w := packetWriter{}
	w.writeUint16(ownerID)
	return w.bytes()
}

func buildCurrentExpertJobStoreEnterSuccess(store *currentExpertJobStore) []byte {
	w := packetWriter{}
	w.writeByte(1)
	w.writeByte(store.Kind)
	if store.Kind == currentExpertJobEnchantStoreKind {
		w.writeUint16(store.OwnerCharacterID)
		w.writeInt32(int(store.Endurance))
	} else {
		w.writeByte(byte(store.MachineGrade))
		w.writeInt32(int(store.Cost))
		w.writeInt32(int(store.Endurance))
	}
	return w.bytes()
}

func currentExpertJobStoreError(err error) byte {
	switch {
	case errors.Is(err, dnfexpertjob.ErrInsufficientGold):
		return 21
	case errors.Is(err, dnfexpertjob.ErrLevelTooLow):
		return 14
	case errors.Is(err, dnfexpertjob.ErrMachineEndurance):
		// 【2026-09-23 修正 189 → 190】op235 的失败码表（客户端读取器 sub_192B820
		// 尾部）实测只认这些码：3/4/10/17/19/21/22/23/126/175/190/212，其余落到
		// 通用映射器 sub_2EDDB80，而它的显式码集合是
		// {1,4,12,54,77,105,114,115,122,123,124,134,136,137,138,167,208,210,214,
		//  228,229,230,232,234,237,239}，默认分支是 `xor al,al; ret`（什么都不画）。
		// 189 两边都不在 → 玩家点分解机时**没有任何提示**。190 才是
		// dstr 44069「分解机的当前耐久度为0，无法进行分解」。
		return 190
	case errors.Is(err, dnfexpertjob.ErrMachineGradeTooLow):
		return 0xD4
	case errors.Is(err, errDungeonPickupInventoryFull), errors.Is(err, errCurrentDisjointRewardInvalid):
		return 4
	default:
		return 19
	}
}

func (s *Service) handleCurrentExpertJobStoreRepair(session *gameSession, opcode uint16, body []byte) error {
	if len(body) != 0 {
		return s.sendGameUpperFailure(session, opcode, 60)
	}
	s.expertJobStoreOpMu.Lock()
	ctx, cancel := context.WithTimeout(context.Background(), createWriteTimeout)
	result, jobType, err := s.commitCurrentExpertJobMachineRepair(ctx, session)
	cancel()
	s.expertJobStoreOpMu.Unlock()
	if err != nil {
		return s.sendGameUpperFailure(session, opcode, currentExpertJobRepairError(err))
	}
	w := packetWriter{}
	w.writeByte(1)
	w.writeInt32(int(result.Gold))
	w.writeInt32(int(result.Endurance))
	if err := s.sendGameUpperRawClass(session, opcode, w.bytes(), dnfproto.DefaultChannelClassification); err != nil {
		return err
	}
	// The current EXE only applies the gold field of this ACK on the op236
	// route (sub_1D0B860 reads two u32 and calls the wallet setter). The op611
	// route (sub_1D0E670) reads nothing at all - it just toasts string 0x57BC -
	// so the deducted gold stayed on screen until the next character select
	// replayed the full list. Push the authoritative wallet row either way.
	if err := s.sendCurrentExpertJobGoldRefresh(session, opcode, result.Gold); err != nil {
		return err
	}
	return s.sendCurrentExpertJobInfoFromRepository(session, jobType, true)
}

// sendCurrentExpertJobGoldRefresh pushes one narrow class-0 op14 carrying only
// the list-0 gold wallet row, so an expert-job store operation that spent gold
// updates the inventory display immediately.
func (s *Service) sendCurrentExpertJobGoldRefresh(session *gameSession, opcode uint16, gold int64) error {
	if session == nil || gold < 0 {
		return nil
	}
	entries := []currentItemListEntry{currentGoldWalletItemListEntry(gold)}
	body := buildCurrentItemUpdateBody(dnfrepo.MainInventoryListType, entries)
	s.logPacketEvent("game-upper-current-expert-job-gold-refresh-send",
		"conn_id", session.connID,
		"char_id", session.selectedCharacterID,
		"source_msg_id", opcode,
		"msg_id", uint16(dnfenum.CmdPacketWalkoutPartyMember),
		"classification", 0,
		"list_type", dnfrepo.MainInventoryListType,
		"gold_after", gold,
		"body_len", len(body),
		"sequence", "expert_job_ack_then_narrow_gold_op14")
	return s.sendGameUpperRawClass(session, uint16(dnfenum.CmdPacketWalkoutPartyMember), body, 0)
}

func (s *Service) commitCurrentExpertJobMachineRepair(ctx context.Context, session *gameSession) (dnfexpertjob.MachineRepairPlan, byte, error) {
	jobCatalog, _, owner, accountID, characterID, err := s.currentExpertJobMutationContext(session)
	if err != nil || currentExpertJobSessionInDungeon(session) {
		return dnfexpertjob.MachineRepairPlan{}, 0, dnfexpertjob.ErrMachineInvalid
	}
	s.expertJobStoreMu.Lock()
	_, busy := s.expertJobStores[session.selectedCharacterID]
	s.expertJobStoreMu.Unlock()
	if busy {
		return dnfexpertjob.MachineRepairPlan{}, 0, dnfexpertjob.ErrMachineInvalid
	}
	var result dnfexpertjob.MachineRepairPlan
	var resultJobType byte
	err = owner.Machine(ctx, dnfexpertjob.Command{AccountID: accountID, CharacterID: characterID, UpdatedAt: s.gameplayNow(), Project: func(assets *dnfexpertjob.Assets) (dnfexpertjob.Changes, error) {
		jobType, experience, stateErr := currentExpertJobCharacterState(assets.Character)
		if stateErr != nil || (jobType != dnfexpertjob.EnchanterType && jobType != dnfexpertjob.DisjointerType) {
			return dnfexpertjob.Changes{}, dnfexpertjob.ErrJobUnsupported
		}
		grade := 1
		var rule dnfexpertjob.RepairRule
		var configured bool
		initial := int64(0)
		if jobType == dnfexpertjob.DisjointerType {
			config, ok := jobCatalog.Disjointer()
			if !ok {
				return dnfexpertjob.Changes{}, dnfexpertjob.ErrJobUnsupported
			}
			grade = int(currentExpertJobStatDefault(assets.Character.Stats, currentExpertJobMachineGradeStat, 1))
			rule, configured = config.RepairRule(grade)
			initial = config.InitialEndurance
		} else {
			config, ok := jobCatalog.Enchanter()
			if !ok {
				return dnfexpertjob.Changes{}, dnfexpertjob.ErrJobUnsupported
			}
			level := config.Recipes.Level(experience)
			if level > len(config.RepairRules) {
				level = len(config.RepairRules)
			}
			if level > 0 {
				rule, configured = config.RepairRules[level-1], true
			}
			initial = config.InitialEndurance
		}
		if !configured {
			return dnfexpertjob.Changes{}, dnfexpertjob.ErrMachineInvalid
		}
		gold, goldErr := currentExpertJobWalletGold(assets.Character)
		if goldErr != nil {
			return dnfexpertjob.Changes{}, goldErr
		}
		current := currentExpertJobStatDefault(assets.Character.Stats, currentExpertJobMachineEnduranceStat, initial)
		plan, planErr := dnfexpertjob.PlanMachineRepair(gold, current, rule)
		if planErr != nil {
			return dnfexpertjob.Changes{}, planErr
		}
		currentExpertJobSetWalletGold(assets.Character, plan.Gold)
		if assets.Character.Stats == nil {
			assets.Character.Stats = make(map[string]int64)
		}
		assets.Character.Stats[currentExpertJobMachineEnduranceStat] = plan.Endurance
		result, resultJobType = plan, jobType
		return dnfexpertjob.Changes{Character: true, Inventory: true}, nil
	}})
	return result, resultJobType, err
}

func (s *Service) handleCurrentExpertJobStoreUpgrade(session *gameSession, opcode uint16, body []byte) error {
	if len(body) != 0 {
		return s.sendGameUpperFailure(session, opcode, 19)
	}
	s.expertJobStoreOpMu.Lock()
	ctx, cancel := context.WithTimeout(context.Background(), createWriteTimeout)
	result, err := s.commitCurrentDisjointerMachineUpgrade(ctx, session)
	cancel()
	s.expertJobStoreOpMu.Unlock()
	if err != nil {
		return s.sendGameUpperFailure(session, opcode, currentExpertJobUpgradeError(err))
	}
	w := packetWriter{}
	w.writeByte(1)
	w.writeInt32(int(result.Gold))
	w.writeInt32(result.Grade)
	w.writeInt32(int(result.Endurance))
	if err := s.sendGameUpperRawClass(session, opcode, w.bytes(), dnfproto.DefaultChannelClassification); err != nil {
		return err
	}
	return s.sendCurrentExpertJobInfoFromRepository(session, dnfexpertjob.DisjointerType, true)
}

func (s *Service) commitCurrentDisjointerMachineUpgrade(ctx context.Context, session *gameSession) (dnfexpertjob.DisjointerUpgradePlan, error) {
	jobCatalog, _, owner, accountID, characterID, err := s.currentExpertJobMutationContext(session)
	if err != nil || currentExpertJobSessionInDungeon(session) {
		return dnfexpertjob.DisjointerUpgradePlan{}, dnfexpertjob.ErrMachineInvalid
	}
	s.expertJobStoreMu.Lock()
	_, busy := s.expertJobStores[session.selectedCharacterID]
	s.expertJobStoreMu.Unlock()
	if busy {
		return dnfexpertjob.DisjointerUpgradePlan{}, dnfexpertjob.ErrMachineInvalid
	}
	var result dnfexpertjob.DisjointerUpgradePlan
	err = owner.Machine(ctx, dnfexpertjob.Command{AccountID: accountID, CharacterID: characterID, UpdatedAt: s.gameplayNow(), Project: func(assets *dnfexpertjob.Assets) (dnfexpertjob.Changes, error) {
		jobType, experience, stateErr := currentExpertJobCharacterState(assets.Character)
		if stateErr != nil || jobType != dnfexpertjob.DisjointerType {
			return dnfexpertjob.Changes{}, dnfexpertjob.ErrJobUnsupported
		}
		config, ok := jobCatalog.Disjointer()
		if !ok {
			return dnfexpertjob.Changes{}, dnfexpertjob.ErrJobUnsupported
		}
		gold, goldErr := currentExpertJobWalletGold(assets.Character)
		if goldErr != nil {
			return dnfexpertjob.Changes{}, goldErr
		}
		grade := int(currentExpertJobStatDefault(assets.Character.Stats, currentExpertJobMachineGradeStat, 1))
		endurance := currentExpertJobStatDefault(assets.Character.Stats, currentExpertJobMachineEnduranceStat, config.InitialEndurance)
		plan, planErr := config.PlanUpgrade(gold, experience, endurance, grade, assets.Character.Level)
		if planErr != nil {
			return dnfexpertjob.Changes{}, planErr
		}
		currentExpertJobSetWalletGold(assets.Character, plan.Gold)
		if assets.Character.Stats == nil {
			assets.Character.Stats = make(map[string]int64)
		}
		assets.Character.Stats[currentExpertJobMachineGradeStat], assets.Character.Stats[currentExpertJobMachineEnduranceStat] = int64(plan.Grade), plan.Endurance
		result = plan
		return dnfexpertjob.Changes{Character: true, Inventory: true}, nil
	}})
	return result, err
}

func currentExpertJobWalletGold(character *dnfrepo.CharacterRecord) (int64, error) {
	if character == nil {
		return 0, dnfexpertjob.ErrCharacterNotFound
	}
	gold := character.Stats["gold"]
	if gold < 0 {
		return 0, dnfexpertjob.ErrInsufficientGold
	}
	return gold, nil
}

func currentExpertJobSetWalletGold(character *dnfrepo.CharacterRecord, gold int64) {
	if character.Stats == nil {
		character.Stats = make(map[string]int64)
	}
	character.Stats["gold"] = gold
}

func currentExpertJobRepairError(err error) byte {
	if errors.Is(err, dnfexpertjob.ErrMachineInvalid) {
		return 60
	}
	return 22
}

func currentExpertJobUpgradeError(err error) byte {
	if errors.Is(err, dnfexpertjob.ErrLevelTooLow) {
		return 14
	}
	if errors.Is(err, dnfexpertjob.ErrInsufficientGold) {
		return 22
	}
	return 19
}

func currentExpertJobStoreStringContainsFold(values []string, needle string) bool {
	for _, value := range values {
		if strings.EqualFold(strings.TrimSpace(value), strings.TrimSpace(needle)) {
			return true
		}
	}
	return false
}

func currentExpertJobStoreIntContains(values []int64, needle int64) bool {
	for _, value := range values {
		if value == needle {
			return true
		}
	}
	return false
}

func (s *Service) handleCurrentExpertJobStoreDisjoint(session *gameSession, opcode uint16, body []byte) error {
	// 【批量分解 2026-09-23】客户端钩子（独立 DLL，验证通过后才并入 90CN）会把
	// "全部分解"选中的 N 件合成一个带标记的信封。不是信封就走下面原来的单件路径，
	// 未注入钩子的客户端行为完全不变。
	if batch, isBatch, batchErr := parseCurrentExpertJobStoreDisjointBatchRequest(body); isBatch {
		if batchErr != nil {
			s.logGameEvent(session, "game-expert-job-store-disjoint-batch-rejected",
				"body_len", len(body), "reason", batchErr)
			return s.sendGameUpperFailure(session, opcode, 19)
		}
		return s.handleCurrentExpertJobStoreDisjointBatch(session, opcode, batch)
	}
	request, err := parseCurrentExpertJobStoreDisjointRequest(body)
	if err != nil {
		return s.sendGameUpperFailure(session, opcode, 19)
	}
	s.expertJobStoreOpMu.Lock()
	store, ok := s.currentExpertJobEnteredStore(session, request.OwnerID, currentExpertJobDisjointStoreKind)
	if !ok {
		s.expertJobStoreOpMu.Unlock()
		return s.sendGameUpperFailure(session, opcode, 19)
	}
	ctx, cancel := context.WithTimeout(context.Background(), createWriteTimeout)
	result, err := s.commitCurrentExpertJobStoreDisjoint(ctx, session, store, request)
	cancel()
	var closedStore *currentExpertJobStore
	if err == nil {
		closedStore = s.applyCurrentExpertJobStoreMachineCommit(store, result.Endurance, store.MachineGrade)
	}
	s.expertJobStoreOpMu.Unlock()
	if err != nil {
		return s.sendGameUpperFailure(session, opcode, currentExpertJobStoreError(err))
	}
	if err := s.sendGameUpperRawClass(session, opcode, buildCurrentExpertJobStoreDisjointSuccessBody(request, result), dnfproto.DefaultChannelClassification); err != nil {
		return err
	}
	if err := s.sendSelectedCurrentContainerListsRefresh(session, "expert_job_store_disjoint_after_ack"); err != nil {
		return err
	}
	if store.OwnerSession != nil {
		if store.OwnerSession != session {
			_ = s.sendGameUpperRawClass(
				store.OwnerSession,
				uint16(dnfenum.CmdPacketWalkoutPartyMember),
				buildCurrentGoldStateBody(result.OwnerGold),
				0,
			)
		}
		_ = s.sendCurrentExpertJobInfoFromRepository(store.OwnerSession, dnfexpertjob.DisjointerType, true)
	}
	if closedStore != nil {
		s.broadcastCurrentExpertJobStoreClose(closedStore, true)
	}
	return nil
}

func (s *Service) commitCurrentExpertJobStoreDisjoint(ctx context.Context, session *gameSession, store *currentExpertJobStore, request currentExpertJobStoreDisjointRequest) (currentExpertJobStoreUseResult, error) {
	jobCatalog, err := s.currentExpertJobCatalog()
	if err != nil {
		return currentExpertJobStoreUseResult{}, err
	}
	items, err := s.currentPVFItemCatalog()
	if err != nil {
		return currentExpertJobStoreUseResult{}, err
	}
	disjointConfig, err := s.currentDisjointPVFConfigCached(items.source)
	if err != nil {
		return currentExpertJobStoreUseResult{}, err
	}
	var result currentExpertJobStoreUseResult
	err = s.mutateCurrentExpertJobStorePair(ctx, session, store, func(requesterCharacter *dnfrepo.CharacterRecord, requesterInventory *dnfrepo.InventoryRecord, requesterAccount *dnfrepo.AccountInventoryRecord, ownerCharacter *dnfrepo.CharacterRecord, ownerInventory *dnfrepo.InventoryRecord) error {
		jobType, experience, stateErr := currentExpertJobCharacterState(ownerCharacter)
		if stateErr != nil || jobType != dnfexpertjob.DisjointerType {
			return dnfexpertjob.ErrJobUnsupported
		}
		config, configured := jobCatalog.Disjointer()
		if !configured {
			return dnfexpertjob.ErrJobUnsupported
		}
		grade := int(currentExpertJobStatDefault(ownerCharacter.Stats, currentExpertJobMachineGradeStat, 1))
		endurance := currentExpertJobStatDefault(ownerCharacter.Stats, currentExpertJobMachineEnduranceStat, config.InitialEndurance)
		if grade != store.MachineGrade || endurance != store.Endurance {
			return dnfexpertjob.ErrMachineInvalid
		}
		targetKey := currentCeraShopInventorySlotKey(request.TargetList, request.TargetSlot)
		target, found := requesterInventory.Slots[targetKey]
		if !found || target.ItemID <= 0 || target.ItemID > math.MaxUint32 || target.Count != 1 || currentNPCShopItemLocked(target) {
			return dnfexpertjob.ErrExtractionInvalid
		}
		metadata, metadataErr := jobCatalog.Equipment(target.ItemID)
		if metadataErr != nil || metadata.DisjointForbidden || metadata.AttachType == "trade delete" {
			return dnfexpertjob.ErrExtractionInvalid
		}
		metadata.State = currentExpertJobEquipmentState(target)
		plan, planErr := jobCatalog.PlanDisjointer(experience, grade, metadata, requesterCharacter.CharacterID == ownerCharacter.CharacterID, rand.IntN)
		if planErr != nil {
			return planErr
		}
		// The profession machine owns its base/additional/big-win table in
		// disjointer.exj, while account-bound souls remain the global
		// rarity expansion declared by etc/disjoint.etc.
		if soul, granted, soulErr := currentDisjointExpandReward(disjointConfig, metadata.Rarity, metadata.Level); soulErr != nil {
			return soulErr
		} else if granted {
			merged := false
			for index := range plan.Materials {
				if plan.Materials[index].ItemID == int64(soul.ItemID) {
					plan.Materials[index].Count += int64(soul.Count)
					merged = true
					break
				}
			}
			if !merged {
				plan.Materials = append(plan.Materials, dnfexpertjob.RecipeEntry{ItemID: int64(soul.ItemID), Count: int64(soul.Count)})
			}
		}
		if endurance < plan.EnduranceReduction {
			return dnfexpertjob.ErrMachineEndurance
		}
		requesterGold, goldErr := currentExpertJobWalletGold(requesterCharacter)
		if goldErr != nil {
			return goldErr
		}
		ownerGold, ownerGoldErr := currentExpertJobWalletGold(ownerCharacter)
		if ownerGoldErr != nil {
			return ownerGoldErr
		}
		self := requesterCharacter.CharacterID == ownerCharacter.CharacterID
		if !self {
			if requesterGold < store.Cost {
				return dnfexpertjob.ErrInsufficientGold
			}
			if ownerGold > math.MaxInt32-store.Cost {
				return dnfexpertjob.ErrInsufficientGold
			}
			requesterGold -= store.Cost
			ownerGold += store.Cost
			currentExpertJobSetWalletGold(requesterCharacter, requesterGold)
			currentExpertJobSetWalletGold(ownerCharacter, ownerGold)
		}
		delete(requesterInventory.Slots, targetKey)
		rewards := make([]currentDisjointReward, 0, len(plan.Materials))
		for _, reward := range plan.Materials {
			if reward.ItemID <= 0 || reward.ItemID > math.MaxUint32 || reward.Count <= 0 || reward.Count > math.MaxUint32 {
				return dnfexpertjob.ErrExtractionInvalid
			}
			rewards = append(rewards, currentDisjointReward{ItemID: uint32(reward.ItemID), Count: uint32(reward.Count)})
		}
		rewardSlots, _, grantErr := currentGrantDisjointRewards(requesterInventory, requesterAccount, items, rewards, nil, s.currentInventoryPlacementMainSlotCount(ctx, requesterInventory, ""))
		if grantErr != nil {
			return grantErr
		}
		for _, reward := range rewardSlots {
			result.Materials = append(result.Materials, currentExpertJobExtractionMaterial{Slot: reward.Slot, ItemID: reward.ItemID, Count: reward.Granted})
		}
		if ownerCharacter.Stats == nil {
			ownerCharacter.Stats = make(map[string]int64)
		}
		ownerCharacter.Stats["expert_job_exp"] = plan.FinalExperience
		ownerCharacter.Stats[currentExpertJobMachineEnduranceStat] = endurance - plan.EnduranceReduction
		result.RequesterGold, result.OwnerGold, result.Endurance, result.Experience, result.LevelChanged = requesterGold, ownerGold, endurance-plan.EnduranceReduction, plan.ExperienceGain, plan.LevelChanged
		return nil
	})
	return result, err
}

// currentExpertJobStoreDisjointBatchMaxRows 是合并回包能携带的产物行数上限。
// 回包里的 rowCount 是 u8（客户端 sub_192B820 按它循环），所以必须 < 256；
// 单个槽位一行、不跨件合并（与单件路径逐行语义一致），留出余量取 200。
const currentExpertJobStoreDisjointBatchMaxRows = 200

// handleCurrentExpertJobStoreDisjointBatch 处理客户端的"全部分解"批量命令：
// **一个**事务结算 N 件 → **一个**合并回包 → 一次增量槽位刷新。
//
// 行移除分工（与 op26 批量 commitCurrentDisjointItemBatch 完全同一口径）：
// 合并回包只上报一个 targetSlot，客户端读取器只会清那一个槽；其余被消耗的
// 源槽必须靠 op14 权威刷新，所以下面把每个源槽和每个产物槽都发一遍。
func (s *Service) handleCurrentExpertJobStoreDisjointBatch(session *gameSession, opcode uint16, batch currentExpertJobStoreDisjointBatchRequest) error {
	s.expertJobStoreOpMu.Lock()
	store, ok := s.currentExpertJobEnteredStore(session, batch.OwnerID, currentExpertJobDisjointStoreKind)
	if !ok {
		s.expertJobStoreOpMu.Unlock()
		return s.sendGameUpperFailure(session, opcode, 19)
	}
	ctx, cancel := context.WithTimeout(context.Background(), createWriteTimeout)
	result, err := s.commitCurrentExpertJobStoreDisjointBatch(ctx, session, store, batch)
	cancel()
	var closedStore *currentExpertJobStore
	if err == nil {
		closedStore = s.applyCurrentExpertJobStoreMachineCommit(store, result.Endurance, store.MachineGrade)
	}
	s.expertJobStoreOpMu.Unlock()
	if err != nil {
		s.logGameEvent(session, "game-expert-job-store-disjoint-batch-failed",
			"slot_count", len(batch.Slots), "owner", batch.OwnerID, "reason", err)
		return s.sendGameUpperFailure(session, opcode, currentExpertJobStoreError(err))
	}
	ackRequest := currentExpertJobStoreDisjointRequest{
		OwnerID:    batch.OwnerID,
		TargetSlot: batch.Slots[0],
		TargetList: batch.TargetList,
	}
	if err := s.sendGameUpperRawClass(session, opcode, buildCurrentExpertJobStoreDisjointSuccessBody(ackRequest, result), dnfproto.DefaultChannelClassification); err != nil {
		return err
	}
	refreshes := make([]alignedcmd.ItemSlotRefresh, 0, len(batch.Slots)+len(result.Materials))
	for _, slot := range batch.Slots {
		refreshes = append(refreshes, alignedcmd.ItemSlotRefresh{
			ListType:  dnfrepo.MainInventoryListType,
			SlotIndex: slot,
		})
	}
	for _, material := range result.Materials {
		refreshes = append(refreshes, alignedcmd.ItemSlotRefresh{
			ListType:  dnfrepo.MainInventoryListType,
			SlotIndex: material.Slot,
		})
	}
	if err := s.sendSelectedIncrementalItemSlotRefreshes(session, "expert_job_store_disjoint_batch", refreshes); err != nil {
		return err
	}
	if store.OwnerSession != nil {
		if store.OwnerSession != session {
			_ = s.sendGameUpperRawClass(
				store.OwnerSession,
				uint16(dnfenum.CmdPacketWalkoutPartyMember),
				buildCurrentGoldStateBody(result.OwnerGold),
				0,
			)
		}
		_ = s.sendCurrentExpertJobInfoFromRepository(store.OwnerSession, dnfexpertjob.DisjointerType, true)
	}
	if closedStore != nil {
		s.broadcastCurrentExpertJobStoreClose(closedStore, true)
	}
	s.logGameEvent(session, "game-expert-job-store-disjoint-batch-committed",
		"slot_count", len(batch.Slots), "row_count", len(result.Materials),
		"owner", batch.OwnerID, "single_merged_op235_ack", "inventory_refresh",
		"repository_backed_op14_source_and_reward_slots")
	return nil
}

// commitCurrentExpertJobStoreDisjointBatch 在**一个**双人事务里逐件结算。
//
// 与 N 次单件请求等价的语义，逐条保持：
//   - 每件都按**当前**经验与**当前**耐久去掷（PlanDisjointer 的等级/大成功表依赖经验，
//     所以经验要逐件往下传递，而不是全程用初始值）；
//   - 耐久逐件累扣，中间任意一件不够就返回 ErrMachineEndurance → 整个事务回滚
//     （整批原子，与 op26 的 commitCurrentDisjointItemBatch 一致）；
//   - 手续费逐件收（自用免费），中途余额不足同样整批回滚。
//
// 之所以"整批原子"：客户端 UI 的 [win+0x434] 写的是选中件数 N，如果服务端只做了一部分，
// 界面计数与实际入包行数会对不上。代价是选中集合里混进一个锁定/不可分解的道具会导致
// 整批失败——这是相对"只失败那一件"的行为变化，已记入报告。
func (s *Service) commitCurrentExpertJobStoreDisjointBatch(ctx context.Context, session *gameSession, store *currentExpertJobStore, batch currentExpertJobStoreDisjointBatchRequest) (currentExpertJobStoreUseResult, error) {
	jobCatalog, err := s.currentExpertJobCatalog()
	if err != nil {
		return currentExpertJobStoreUseResult{}, err
	}
	items, err := s.currentPVFItemCatalog()
	if err != nil {
		return currentExpertJobStoreUseResult{}, err
	}
	disjointConfig, err := s.currentDisjointPVFConfigCached(items.source)
	if err != nil {
		return currentExpertJobStoreUseResult{}, err
	}
	var result currentExpertJobStoreUseResult
	err = s.mutateCurrentExpertJobStorePair(ctx, session, store, func(requesterCharacter *dnfrepo.CharacterRecord, requesterInventory *dnfrepo.InventoryRecord, requesterAccount *dnfrepo.AccountInventoryRecord, ownerCharacter *dnfrepo.CharacterRecord, ownerInventory *dnfrepo.InventoryRecord) error {
		jobType, experience, stateErr := currentExpertJobCharacterState(ownerCharacter)
		if stateErr != nil || jobType != dnfexpertjob.DisjointerType {
			return dnfexpertjob.ErrJobUnsupported
		}
		config, configured := jobCatalog.Disjointer()
		if !configured {
			return dnfexpertjob.ErrJobUnsupported
		}
		grade := int(currentExpertJobStatDefault(ownerCharacter.Stats, currentExpertJobMachineGradeStat, 1))
		endurance := currentExpertJobStatDefault(ownerCharacter.Stats, currentExpertJobMachineEnduranceStat, config.InitialEndurance)
		if grade != store.MachineGrade || endurance != store.Endurance {
			return dnfexpertjob.ErrMachineInvalid
		}
		startExperience := experience
		levelChanged := false
		self := requesterCharacter.CharacterID == ownerCharacter.CharacterID
		requesterGold, goldErr := currentExpertJobWalletGold(requesterCharacter)
		if goldErr != nil {
			return goldErr
		}
		ownerGold, ownerGoldErr := currentExpertJobWalletGold(ownerCharacter)
		if ownerGoldErr != nil {
			return ownerGoldErr
		}
		// 与单件路径一样，容量只在进入事务时取一次。
		mainSlotCount := s.currentInventoryPlacementMainSlotCount(ctx, requesterInventory, "")
		for _, slot := range batch.Slots {
			targetKey := currentCeraShopInventorySlotKey(batch.TargetList, slot)
			target, found := requesterInventory.Slots[targetKey]
			if !found || target.ItemID <= 0 || target.ItemID > math.MaxUint32 || target.Count != 1 || currentNPCShopItemLocked(target) {
				return dnfexpertjob.ErrExtractionInvalid
			}
			metadata, metadataErr := jobCatalog.Equipment(target.ItemID)
			if metadataErr != nil || metadata.DisjointForbidden || metadata.AttachType == "trade delete" {
				return dnfexpertjob.ErrExtractionInvalid
			}
			metadata.State = currentExpertJobEquipmentState(target)
			plan, planErr := jobCatalog.PlanDisjointer(experience, grade, metadata, self, rand.IntN)
			if planErr != nil {
				return planErr
			}
			// The profession machine owns its base/additional/big-win table in
			// disjointer.exj, while account-bound souls remain the global
			// rarity expansion declared by etc/disjoint.etc.
			if soul, granted, soulErr := currentDisjointExpandReward(disjointConfig, metadata.Rarity, metadata.Level); soulErr != nil {
				return soulErr
			} else if granted {
				merged := false
				for index := range plan.Materials {
					if plan.Materials[index].ItemID == int64(soul.ItemID) {
						plan.Materials[index].Count += int64(soul.Count)
						merged = true
						break
					}
				}
				if !merged {
					plan.Materials = append(plan.Materials, dnfexpertjob.RecipeEntry{ItemID: int64(soul.ItemID), Count: int64(soul.Count)})
				}
			}
			if endurance < plan.EnduranceReduction {
				return dnfexpertjob.ErrMachineEndurance
			}
			if !self {
				if requesterGold < store.Cost {
					return dnfexpertjob.ErrInsufficientGold
				}
				if ownerGold > math.MaxInt32-store.Cost {
					return dnfexpertjob.ErrInsufficientGold
				}
				requesterGold -= store.Cost
				ownerGold += store.Cost
			}
			delete(requesterInventory.Slots, targetKey)
			rewards := make([]currentDisjointReward, 0, len(plan.Materials))
			for _, reward := range plan.Materials {
				if reward.ItemID <= 0 || reward.ItemID > math.MaxUint32 || reward.Count <= 0 || reward.Count > math.MaxUint32 {
					return dnfexpertjob.ErrExtractionInvalid
				}
				rewards = append(rewards, currentDisjointReward{ItemID: uint32(reward.ItemID), Count: uint32(reward.Count)})
			}
			rewardSlots, _, grantErr := currentGrantDisjointRewards(requesterInventory, requesterAccount, items, rewards, nil, mainSlotCount)
			if grantErr != nil {
				return grantErr
			}
			if len(result.Materials)+len(rewardSlots) > currentExpertJobStoreDisjointBatchMaxRows {
				return errCurrentDisjointRewardInvalid
			}
			for _, reward := range rewardSlots {
				result.Materials = append(result.Materials, currentExpertJobExtractionMaterial{Slot: reward.Slot, ItemID: reward.ItemID, Count: reward.Granted})
			}
			endurance -= plan.EnduranceReduction
			experience = plan.FinalExperience
			levelChanged = levelChanged || plan.LevelChanged
		}
		if !self {
			currentExpertJobSetWalletGold(requesterCharacter, requesterGold)
			currentExpertJobSetWalletGold(ownerCharacter, ownerGold)
		}
		if ownerCharacter.Stats == nil {
			ownerCharacter.Stats = make(map[string]int64)
		}
		ownerCharacter.Stats["expert_job_exp"] = experience
		ownerCharacter.Stats[currentExpertJobMachineEnduranceStat] = endurance
		result.RequesterGold, result.OwnerGold, result.Endurance = requesterGold, ownerGold, endurance
		result.Experience = experience - startExperience
		result.LevelChanged = levelChanged
		result.TargetList = batch.TargetList
		result.TargetSlot = batch.Slots[0]
		return nil
	})
	return result, err
}

func (s *Service) handleCurrentExpertJobStoreEnchant(session *gameSession, opcode uint16, body []byte) error {
	request, err := parseCurrentExpertJobStoreEnchantRequest(body)
	if err != nil {
		return s.sendGameUpperFailure(session, opcode, 19)
	}
	s.expertJobStoreOpMu.Lock()
	store, ok := s.currentExpertJobEnteredStore(session, request.OwnerID, currentExpertJobEnchantStoreKind)
	if !ok {
		s.expertJobStoreOpMu.Unlock()
		return s.sendGameUpperFailure(session, opcode, 19)
	}
	ctx, cancel := context.WithTimeout(context.Background(), createWriteTimeout)
	result, err := s.commitCurrentExpertJobStoreEnchant(ctx, session, store, request)
	cancel()
	var updatedStore, closedStore *currentExpertJobStore
	qualificationsChanged := false
	if err == nil {
		s.expertJobStoreMu.Lock()
		live := s.expertJobStores[store.OwnerCharacterID]
		if live != nil && live.OwnerSession == store.OwnerSession {
			qualificationsChanged = !bytes.Equal(live.Qualifications, result.Qualifications)
			live.Endurance = result.Endurance
			live.Qualifications = append([]byte(nil), result.Qualifications...)
			updatedStore = cloneCurrentExpertJobStore(live)
		}
		s.expertJobStoreMu.Unlock()
		if result.Endurance == 0 {
			closedStore = s.removeCurrentExpertJobStore(store.OwnerCharacterID, store.OwnerSession)
		}
	}
	s.expertJobStoreOpMu.Unlock()
	if err != nil {
		return s.sendGameUpperFailure(session, opcode, currentExpertJobStoreError(err))
	}
	// The refreshed containers must go out BEFORE the reply, not after it.
	//
	// sub_1D0E470 handles the op601 reply synchronously: it resolves the target
	// through its own key global (0x518B798) against the item manager
	// (0x51B2768) and hands the resolved object straight to kSceneUiOpen for
	// result window 0x3D9, all inside the reply handler and before any later
	// packet is processed. Whatever the client's item manager holds at reply
	// time is therefore exactly what the window renders.
	//
	// Sent the other way round, that was still the pre-enchant row: the window
	// named the right item but showed none of the enchant it had just received.
	// Which item the window names is a separate, client-side defect: the stall
	// path never wrote 0x518B798 and the DLL now does. This ordering is what
	// makes the row it names already carry the enchant.
	//
	// Same rule as the 宝珠附魔 path, which has always refreshed before its ack.
	if err := s.sendSelectedCurrentContainerListsRefresh(session, "expert_job_store_enchant_before_ack"); err != nil {
		return err
	}
	if err := s.sendGameUpperRawClass(session, opcode, buildCurrentExpertJobStoreEnchantSuccessBody(result), dnfproto.DefaultChannelClassification); err != nil {
		return err
	}
	if store.OwnerSession != nil && store.OwnerSession != session {
		owner := packetWriter{}
		owner.writeInt32(int(result.OwnerGold))
		owner.writeInt32(int(result.Endurance))
		_ = s.sendGameUpperRawClass(store.OwnerSession, currentExpertJobEnchantOwnerNotification, owner.bytes(), 0)
		_ = s.sendGameUpperRawClass(session, uint16(dnfenum.CmdPacketWalkoutPartyMember), buildCurrentGoldStateBody(result.RequesterGold), 0)
		_ = s.sendGameUpperRawClass(store.OwnerSession, uint16(dnfenum.CmdPacketWalkoutPartyMember), buildCurrentGoldStateBody(result.OwnerGold), 0)
	}
	if store.OwnerSession != nil {
		_ = s.sendCurrentExpertJobInfoFromRepository(store.OwnerSession, dnfexpertjob.EnchanterType, true)
	}
	if qualificationsChanged && closedStore == nil && updatedStore != nil {
		s.broadcastCurrentExpertJobStoreUpdate(updatedStore)
	}
	if closedStore != nil {
		s.broadcastCurrentExpertJobStoreClose(closedStore, true)
	}
	return nil
}

func (s *Service) commitCurrentExpertJobStoreEnchant(ctx context.Context, session *gameSession, store *currentExpertJobStore, request currentExpertJobStoreEnchantRequest) (currentExpertJobStoreUseResult, error) {
	jobCatalog, err := s.currentExpertJobCatalog()
	if err != nil {
		return currentExpertJobStoreUseResult{}, err
	}
	items, err := s.currentPVFItemCatalog()
	if err != nil {
		return currentExpertJobStoreUseResult{}, err
	}
	s.initialEquipmentMu.Lock()
	source, sourceErr := s.initialEquipmentArchiveLocked()
	s.initialEquipmentMu.Unlock()
	if sourceErr != nil {
		return currentExpertJobStoreUseResult{}, sourceErr
	}
	var result currentExpertJobStoreUseResult
	err = s.mutateCurrentExpertJobStorePair(ctx, session, store, func(requesterCharacter *dnfrepo.CharacterRecord, requesterInventory *dnfrepo.InventoryRecord, _ *dnfrepo.AccountInventoryRecord, ownerCharacter *dnfrepo.CharacterRecord, ownerInventory *dnfrepo.InventoryRecord) error {
		jobType, experience, stateErr := currentExpertJobCharacterState(ownerCharacter)
		if stateErr != nil || jobType != dnfexpertjob.EnchanterType {
			return dnfexpertjob.ErrJobUnsupported
		}
		config, configured := jobCatalog.Enchanter()
		if !configured {
			return dnfexpertjob.ErrJobUnsupported
		}
		endurance := currentExpertJobStatDefault(ownerCharacter.Stats, currentExpertJobMachineEnduranceStat, config.InitialEndurance)
		if endurance != store.Endurance {
			return dnfexpertjob.ErrMachineInvalid
		}
		targetKey := currentCeraShopInventorySlotKey(request.TargetList, request.TargetSlot)
		cardKey := currentCeraShopInventorySlotKey(request.CardList, request.CardSlot)
		target, targetFound := requesterInventory.Slots[targetKey]
		card, cardFound := requesterInventory.Slots[cardKey]
		if !targetFound || target.ItemID <= 0 || target.ItemID > math.MaxUint32 || target.Count != 1 || currentNPCShopItemLocked(target) || !cardFound || card.ItemID <= 0 || card.ItemID > math.MaxUint32 || card.Count <= 0 || currentNPCShopItemLocked(card) {
			return dnfexpertjob.ErrExtractionInvalid
		}
		plan, planErr := jobCatalog.PlanEnchanterStore(experience, request.RecipeID, card.ItemID, endurance, rand.IntN)
		if planErr != nil {
			return planErr
		}
		resolution, resolutionErr := resolveCurrentEnchantCardMetadata(items, source, card.ItemID, target.ItemID)
		if resolutionErr != nil {
			return resolutionErr
		}
		upgradeCount := currentExpertJobEnchantUpgradeCount(card)
		if resolution.CardItemID != card.ItemID || resolution.TargetKind != string(dungeonDropItemEquipment) || !currentExpertJobStoreStringContainsFold(resolution.AllowedEquipmentTypes, resolution.TargetEquipmentType) || (len(resolution.UpgradeCounts) > 0 && !currentExpertJobStoreIntContains(resolution.UpgradeCounts, int64(upgradeCount))) || (len(resolution.UpgradeCounts) == 0 && upgradeCount != 0) {
			return dnfexpertjob.ErrExtractionInvalid
		}
		requesterGold, goldErr := currentExpertJobWalletGold(requesterCharacter)
		if goldErr != nil {
			return goldErr
		}
		ownerGold, ownerGoldErr := currentExpertJobWalletGold(ownerCharacter)
		if ownerGoldErr != nil {
			return ownerGoldErr
		}
		self := requesterCharacter.CharacterID == ownerCharacter.CharacterID
		if !self {
			if requesterGold < store.Cost || ownerGold > math.MaxInt32-store.Cost {
				return dnfexpertjob.ErrInsufficientGold
			}
			requesterGold -= store.Cost
			ownerGold += store.Cost
			currentExpertJobSetWalletGold(requesterCharacter, requesterGold)
			currentExpertJobSetWalletGold(ownerCharacter, ownerGold)
		}
		if plan.Success {
			currentExpertJobSetEnchantFields(&target, card.ItemID, upgradeCount)
			requesterInventory.Slots[targetKey] = target
		}
		card.Count--
		if card.Count == 0 {
			delete(requesterInventory.Slots, cardKey)
		} else {
			entry := currentItemListEntryFromStack(request.CardList, request.CardSlot, card)
			card.RawEntry = append([]byte(nil), entry.data[:]...)
			requesterInventory.Slots[cardKey] = card
		}
		if ownerCharacter.Stats == nil {
			ownerCharacter.Stats = make(map[string]int64)
		}
		ownerCharacter.Stats["expert_job_exp"] = plan.FinalExperience
		ownerCharacter.Stats[currentExpertJobMachineEnduranceStat] = endurance - plan.EnduranceReduction
		result = currentExpertJobStoreUseResult{RequesterGold: requesterGold, OwnerGold: ownerGold, Endurance: endurance - plan.EnduranceReduction, Experience: plan.FinalExperience, LevelChanged: plan.LevelChanged, Success: plan.Success, TargetList: request.TargetList, TargetSlot: request.TargetSlot, Qualifications: config.Qualifications(plan.FinalExperience)}
		return nil
	})
	return result, err
}

func (s *Service) currentExpertJobEnteredStore(session *gameSession, ownerID uint16, kind byte) (*currentExpertJobStore, bool) {
	if s == nil || session == nil || session.selectedCharacterID == 0 || currentExpertJobSessionInDungeon(session) || s.onlinePlayers == nil {
		return nil, false
	}
	visitor, visitorOK := s.onlinePlayers.PlayerForCharacter(session.selectedCharacterID)
	s.expertJobStoreMu.Lock()
	defer s.expertJobStoreMu.Unlock()
	store := s.expertJobStores[ownerID]
	if !visitorOK || visitor.Session != session || s.expertJobVisitors[session] != ownerID || store == nil || store.Kind != kind || store.OwnerSession == nil || store.TownID != visitor.TownID || store.AreaID != visitor.AreaID || store.InstanceKey != visitor.InstanceKey {
		return nil, false
	}
	return cloneCurrentExpertJobStore(store), true
}

func (s *Service) updateCurrentExpertJobStoreMachine(snapshot *currentExpertJobStore, endurance int64, grade int) {
	if s == nil || snapshot == nil {
		return
	}
	s.expertJobStoreMu.Lock()
	defer s.expertJobStoreMu.Unlock()
	store := s.expertJobStores[snapshot.OwnerCharacterID]
	if store != nil && store.OwnerSession == snapshot.OwnerSession {
		store.Endurance, store.MachineGrade = endurance, grade
	}
}

// applyCurrentExpertJobStoreMachineCommit updates the live machine only after
// its durable disjoint transaction has committed. The caller holds
// expertJobStoreOpMu, so a zero-endurance machine follows the same removal
// path as an explicit close and can produce at most one close notification.
func (s *Service) applyCurrentExpertJobStoreMachineCommit(snapshot *currentExpertJobStore, endurance int64, grade int) *currentExpertJobStore {
	s.updateCurrentExpertJobStoreMachine(snapshot, endurance, grade)
	if endurance != 0 || snapshot == nil {
		return nil
	}
	return s.removeCurrentExpertJobStore(snapshot.OwnerCharacterID, snapshot.OwnerSession)
}

func buildCurrentExpertJobStoreDisjointSuccessBody(request currentExpertJobStoreDisjointRequest, result currentExpertJobStoreUseResult) []byte {
	w := packetWriter{}
	w.writeByte(1)
	w.writeUint16(uint16(request.TargetSlot))
	w.writeByte(request.TargetList)
	w.writeUint16(request.OwnerID)
	w.writeByte(byte(len(result.Materials)))
	for _, material := range result.Materials {
		w.writeUint16(uint16(material.Slot))
		w.writeUint32(material.ItemID)
		w.writeUint32(material.Count)
	}
	w.writeUint32(uint32(result.RequesterGold))
	w.writeUint32(uint32(result.Endurance))
	return w.bytes()
}

// buildCurrentExpertJobStoreEnchantSuccessBody builds the op601 reply: the
// leading container byte the class-1 pump consumes as `a2`, then the ten bytes
// sub_1D0E470 reads - u8 success, u32 experience, u8 reserved, u32 endurance.
//
// The ten-byte form was tried and discharged by the live client on 2026-09-14.
// The class-1 pump reads body[0] as `a2` (0x02262186 reads one byte through the
// same cursor reader 0x34E3970 the handler uses, and 0x022622EA pushes that byte
// as a2 with no branch anywhere in .text bypassing the read), so the handler's
// own ten bytes start at body[1]. With the reply shortened to ten bytes the
// endurance read ran one byte past the body: a live reply of success=1,
// exp=59, reserved=0, endurance=261 was rendered as 805306369 (0x30000001) -
// bytes 01 00 00 from the real endurance plus one stray byte. That value is the
// machine's 耐久 in the stall window, so the seller sees the enchant machine
// break. Eleven bytes is the aligned form.
func buildCurrentExpertJobStoreEnchantSuccessBody(result currentExpertJobStoreUseResult) []byte {
	w := packetWriter{}
	w.writeByte(1)
	if result.Success {
		w.writeByte(1)
	} else {
		w.writeByte(0)
	}
	w.writeUint32(uint32(result.Experience))
	w.writeByte(0)
	w.writeUint32(uint32(result.Endurance))
	return w.bytes()
}

func (s *Service) mutateCurrentExpertJobStorePair(ctx context.Context, requesterSession *gameSession, store *currentExpertJobStore, apply func(*dnfrepo.CharacterRecord, *dnfrepo.InventoryRecord, *dnfrepo.AccountInventoryRecord, *dnfrepo.CharacterRecord, *dnfrepo.InventoryRecord) error) error {
	if s == nil || requesterSession == nil || store == nil || apply == nil || requesterSession.selectedCharacterID == 0 {
		return dnfexpertjob.ErrOwnerUnavailable
	}
	if ctx == nil {
		ctx = context.Background()
	}
	repositories, ok := s.repositoryGroup()
	if !ok {
		return dnfexpertjob.ErrOwnerUnavailable
	}
	requesterID := strconv.FormatUint(uint64(requesterSession.selectedCharacterID), 10)
	ownerID := strconv.FormatUint(uint64(store.OwnerCharacterID), 10)
	if requesterID == ownerID {
		owner, ownerErr := dnfexpertjob.NewOwner(repositories)
		if ownerErr != nil {
			return ownerErr
		}
		accountID := strings.TrimSpace(s.accountIDForSession(requesterSession))
		return owner.Machine(ctx, dnfexpertjob.Command{AccountID: accountID, CharacterID: requesterID, UpdatedAt: s.gameplayNow(), Project: func(assets *dnfexpertjob.Assets) (dnfexpertjob.Changes, error) {
			if err := apply(assets.Character, assets.Inventory, assets.AccountInventory, assets.Character, assets.Inventory); err != nil {
				return dnfexpertjob.Changes{}, err
			}
			return dnfexpertjob.Changes{Character: true, AccountInventory: true, Inventory: true}, nil
		}})
	}
	if repositories.AccountCharacterTrade == nil {
		return dnfrepo.ErrAccountCharacterTradeTransactionUnavailable
	}
	accountID := strings.TrimSpace(s.accountIDForSession(requesterSession))
	if accountID == "" {
		return dnfexpertjob.ErrAccountMismatch
	}
	return repositories.AccountCharacterTrade.WithinAccountCharacterTrade(ctx, accountID, requesterID, ownerID, func(accounts dnfrepo.AccountInventoryRepository, characters dnfrepo.CharacterRepository, inventories dnfrepo.InventoryRepository, _ dnfrepo.EpicBookRepository, _ dnfrepo.PetRepository) error {
		ids := []string{requesterID, ownerID}
		if ids[1] < ids[0] {
			ids[0], ids[1] = ids[1], ids[0]
		}
		characterRows := make(map[string]dnfrepo.CharacterRecord, 2)
		inventoryRows := make(map[string]dnfrepo.InventoryRecord, 2)
		for _, id := range ids {
			character, found, err := characters.Load(ctx, id)
			if err != nil {
				return err
			}
			if !found || character.CharacterID != id {
				return dnfexpertjob.ErrCharacterNotFound
			}
			inventory, found, err := inventories.Load(ctx, id)
			if err != nil {
				return err
			}
			if !found || inventory.CharacterID != id || inventory.Slots == nil {
				return dnfexpertjob.ErrInventoryNotFound
			}
			characterRows[id], inventoryRows[id] = dnfrepo.CloneCharacter(character), dnfrepo.CloneInventory(inventory)
		}
		requesterCharacter, requesterInventory := characterRows[requesterID], inventoryRows[requesterID]
		ownerCharacter, ownerInventory := characterRows[ownerID], inventoryRows[ownerID]
		if strings.TrimSpace(requesterCharacter.AccountID) != accountID {
			return dnfexpertjob.ErrAccountMismatch
		}
		requesterAccount, _, err := accounts.Load(ctx, accountID)
		if err != nil {
			return err
		}
		requesterAccount = dnfrepo.CloneAccountInventory(requesterAccount)
		requesterAccount.AccountID = accountID
		if requesterAccount.Slots == nil {
			requesterAccount.Slots = make(map[string]dnfrepo.ItemStack)
		}
		if err := apply(&requesterCharacter, &requesterInventory, &requesterAccount, &ownerCharacter, &ownerInventory); err != nil {
			return err
		}
		characterRows[requesterID], inventoryRows[requesterID], characterRows[ownerID], inventoryRows[ownerID] = requesterCharacter, requesterInventory, ownerCharacter, ownerInventory
		now := s.gameplayNow()
		for _, id := range ids {
			character := characterRows[id]
			character.UpdatedAt = now
			if err := dnfrepo.SaveCharacterFields(ctx, characters, character, dnfrepo.CharacterFieldStats); err != nil {
				return err
			}
			inventory := inventoryRows[id]
			inventory.UpdatedAt = now
			if err := dnfrepo.SaveInventoryFields(ctx, inventories, inventory, dnfrepo.InventoryFieldSlots); err != nil {
				return err
			}
		}
		requesterAccount.UpdatedAt = now
		if err := accounts.Save(ctx, requesterAccount); err != nil {
			return err
		}
		return nil
	})
}

func currentExpertJobSetEnchantFields(stack *dnfrepo.ItemStack, cardItemID int64, upgradeCount byte) {
	if stack == nil {
		return
	}
	if stack.Extra == nil {
		stack.Extra = make(map[string]string, 4)
	}
	stack.Extra["value_a"], stack.Extra["enchant_card_id"] = strconv.FormatInt(cardItemID, 10), strconv.FormatInt(cardItemID, 10)
	stack.Extra["byte_12"], stack.Extra["enchant_upgrade_count"] = strconv.Itoa(int(upgradeCount)), strconv.Itoa(int(upgradeCount))
	if len(stack.RawEntry) == currentItemListEntryWireSize {
		stack.RawEntry = append([]byte(nil), stack.RawEntry...)
		value := uint32(cardItemID)
		binary.LittleEndian.PutUint32(stack.RawEntry[0x0E:0x12], value)
		stack.RawEntry[0x12] = upgradeCount
	}
}
