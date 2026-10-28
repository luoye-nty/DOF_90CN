// 本文件适配原生伴侣资料、戒指升级和心意点物品协议。
package dnfbridge

import (
	"context"
	"encoding/binary"
	"errors"
	"math"
	"strconv"

	dnfrepo "longheng.io/server/internal/modules/dnf/repository"
	"longheng.io/server/internal/modules/dnf/wedding"
)

func (s *Service) handleWeddingRing(session *gameSession, body []byte) error {
	if len(body) != 18 {
		return nil
	}
	return s.weddingUseItem(session, 1036, int32(binary.LittleEndian.Uint32(body[13:17])), body[17])
}

func (s *Service) handleWeddingLove(session *gameSession, body []byte) error {
	if len(body) != 17 {
		return nil
	}
	return s.weddingUseItem(session, 1037, int32(binary.LittleEndian.Uint32(body[13:17])), 0)
}

func (s *Service) weddingUseItem(session *gameSession, opcode uint16, slot int32, ring byte) error {
	identity, current := s.boundGameSessionCharacterSnapshot(session)
	if !current {
		return nil
	}
	ctx, cancel := context.WithTimeout(context.Background(), createWriteTimeout)
	defer cancel()
	s.weddingMu.Lock()
	defer s.weddingMu.Unlock()
	c, err := s.weddingCharacter(ctx, identity)
	state, stateErr := wedding.Read(c)
	if err == nil {
		err = stateErr
	}
	if err == nil && (slot < 3 || slot > 353) {
		err = wedding.Invalid
	}
	cat, catErr := s.currentWeddingCatalog()
	if err == nil {
		err = catErr
	}
	var result wedding.Result
	operation := byte(8)
	if opcode == 1037 {
		operation = 9
	}
	if err == nil {
		repos, _ := s.repositoryGroup()
		result, err = wedding.Commit(ctx, repos, cat, wedding.Command{ActorID: c.CharacterID, PartnerID: strconv.Itoa(int(state.PartnerID)), AccountID: s.accountIDForSession(session), Operation: operation, Slot: slot, Ring: ring, Now: s.gameplayNow()})
	}
	var code wedding.Error
	if err != nil && !errors.As(err, &code) {
		code = wedding.Invalid
	}
	var w packetWriter
	w.writeInt32(int(code))
	if opcode == 1036 {
		w.writeByte(ring)
	}
	ackErr := s.sendGameUpperSuccess(session, opcode, w.bytes())
	if err != nil {
		s.logGameEvent(session, "game-wedding-item-rejected", "opcode", opcode, "slot", slot, "error", err)
		return ackErr
	}
	bag := result.Inventories[0]
	key := inventorySlotKey(0, int16(slot))
	stack, occupied := bag.Slots[key]
	update := currentWeddingConsumedSlotUpdate(slot, stack, occupied)
	ackErr = errors.Join(ackErr, s.sendGameUpperRawClass(session, 14, buildCurrentItemUpdateBody(0, []currentItemListEntry{update}), 0))
	return errors.Join(ackErr, s.publishWeddingCommit(result, operation))
}

// currentWeddingConsumedSlotUpdate 构造"用掉一个之后该槽位的增量行"。
//
// 整叠被吃光时 consume() 已经把槽位从 bag 里删掉了（occupied=false）。这时必须发
// **清空行**（itemID=0xffffffff、count=0，与 dungeon_drop_pickup.go 的
// buildCurrentEpicBookTemporarySlotClearBody 同款）。
//
// 不能拿零值 ItemStack 直接构造行 —— 那会发出 itemID=0 的行，客户端不认，
// 于是**继续显示那个已经不存在的道具**：现场就是"最后一格心意通用掉后图标还在，
// 再点就报错"（2026-10-04 真机）。
func currentWeddingConsumedSlotUpdate(slot int32, stack dnfrepo.ItemStack, occupied bool) currentItemListEntry {
	if !occupied || stack.ItemID <= 0 || stack.Count <= 0 {
		var cleared currentItemListEntry
		cleared.patchCore(int16(slot), math.MaxUint32, 0)
		return cleared
	}
	return currentItemListEntryFromStack(dnfrepo.MainInventoryListType, int16(slot), stack)
}

func (s *Service) handleWeddingPartner(session *gameSession, body []byte) error {
	if len(body) != 17 {
		return nil
	}
	identity, current := s.boundGameSessionCharacterSnapshot(session)
	if !current {
		return nil
	}
	target := binary.LittleEndian.Uint32(body[13:17])
	if target == 0 {
		target = uint32(identity.character)
	}
	if target > 65535 {
		return s.sendGameUpperFailure(session, 1035, 1)
	}
	ctx, cancel := context.WithTimeout(context.Background(), createWriteTimeout)
	defer cancel()
	repos, ok := s.repositoryGroup()
	if !ok || repos.Character == nil || repos.Equipment == nil {
		return s.sendGameUpperFailure(session, 1035, 1)
	}
	c, found, err := repos.Character.Load(ctx, strconv.Itoa(int(target)))
	if err != nil || !found || c.Stats["delete_flag"] != 0 {
		return s.sendGameUpperFailure(session, 1035, 1)
	}
	return s.sendWeddingPartnerState(ctx, session, c)
}

func (s *Service) sendWeddingPartnerState(ctx context.Context, session *gameSession, c dnfrepo.CharacterRecord) error {
	repos, ok := s.repositoryGroup()
	if !ok || repos.Character == nil || repos.Equipment == nil {
		return s.sendGameUpperFailure(session, 1035, 1)
	}
	target, err := strconv.ParseUint(c.CharacterID, 10, 16)
	if err != nil || target == 0 || c.Stats["delete_flag"] != 0 {
		return s.sendGameUpperFailure(session, 1035, 1)
	}
	state, err := wedding.Read(c)
	if err != nil || state.Status == 0 {
		return s.sendGameUpperFailure(session, 1035, 1)
	}
	partner, found, err := repos.Character.Load(ctx, strconv.Itoa(int(state.PartnerID)))
	if err != nil || !found || partner.Stats["delete_flag"] != 0 {
		return s.sendGameUpperFailure(session, 1035, 1)
	}
	other, err := wedding.Read(partner)
	if err != nil || other.PartnerID != uint16(target) || other.PairID != state.PairID || other.Status != state.Status {
		return s.sendGameUpperFailure(session, 1035, 1)
	}
	equipment, found, err := repos.Equipment.Load(ctx, partner.CharacterID)
	if err != nil || !found {
		return s.sendGameUpperFailure(session, 1035, 1)
	}
	rows, err := buildCurrentActorMode0AppearanceSummaryFromEquipment(equipment)
	if err != nil {
		return s.sendGameUpperFailure(session, 1035, 1)
	}
	// sub_1832F30 consumes a packed 97-byte record including thirteen appearance IDs.
	var w packetWriter
	w.writeInt32(0)
	w.writeUint32(state.PairID)
	w.writeUint32(uint32(state.PartnerID))
	name := rosterNameBytes(partner.Name)
	if len(name) > 29 {
		return s.sendGameUpperFailure(session, 1035, 1)
	}
	w.writeBytes(name)
	w.writeZero(30 - len(name))
	// sub_1832F30 maps offset 42 to protected level (+20), 43 to job (+4).
	w.writeByte(rosterLevel(partner))
	w.writeByte(byte(numericCharacterStat(partner.Job)))
	w.writeByte(byte(numericCharacterStatValue(partner, "grow_type")))
	for i := 0; i < 13; i++ {
		w.writeUint32(uint32(rows[i].ItemIDOrIcon))
	}
	return s.sendGameUpperSuccess(session, 1035, w.bytes())
}
