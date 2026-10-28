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

func (s *Service) currentWeddingRoomCatalog() (*wedding.RoomCatalog, error) {
	s.initialEquipmentMu.Lock()
	archive, err := s.initialEquipmentArchiveLocked()
	s.initialEquipmentMu.Unlock()
	if err != nil {
		return nil, err
	}
	return wedding.LoadRoomCatalog(archive)
}

func weddingRoomTreeBody(r wedding.RoomState) []byte {
	var w packetWriter
	w.writeUint32(uint32(r.Tree))
	w.writeUint32(uint32(r.Expires))
	w.writeUint32(uint32(r.Growth))
	w.writeByte(byte(r.WaterCount))
	return w.bytes()
}

func weddingRoomCargoBody(r wedding.RoomState) []byte {
	var w packetWriter
	n := 0
	for _, item := range r.Cargo {
		if item.ID != 0 {
			n++
		}
	}
	w.writeUint32(uint32(n))
	for slot, item := range r.Cargo {
		if item.ID != 0 {
			w.writeUint16(uint16(slot))
			w.writeUint32(uint32(item.ID))
			w.writeUint32(uint32(item.Count))
		}
	}
	w.writeUint32(0)
	return w.bytes()
}

func weddingRoomUpdatesBody(r wedding.RoomState) []byte {
	var w packetWriter
	w.writeUint32(uint32(len(r.Installed) + len(r.Cargo)))
	for slot, id := range r.Installed {
		w.writeByte(0)
		w.writeUint32(uint32(id))
		w.writeUint16(uint16(slot))
		if id == 0 {
			w.writeUint32(0)
		} else {
			w.writeUint32(1)
		}
	}
	for slot, item := range r.Cargo {
		w.writeByte(1)
		w.writeUint32(uint32(item.ID))
		w.writeUint16(uint16(slot))
		w.writeUint32(uint32(item.Count))
	}
	return w.bytes()
}

func (s *Service) sendWeddingRoomState(session *gameSession, c dnfrepo.CharacterRecord) error {
	state, err := wedding.Read(c)
	if err != nil || state.Status != 4 {
		return err
	}
	ctx, cancel := context.WithTimeout(context.Background(), createWriteTimeout)
	defer cancel()
	repos, ok := s.repositoryGroup()
	if !ok || repos.Character == nil {
		return wedding.Invalid
	}
	peer, found, err := repos.Character.Load(ctx, strconv.Itoa(int(state.PartnerID)))
	if err != nil {
		return err
	}
	if !found {
		return wedding.Missing
	}
	other, err := wedding.Read(peer)
	if err != nil || other.PairID != state.PairID || other.Status != 4 || strconv.Itoa(int(other.PartnerID)) != c.CharacterID {
		return wedding.Invalid
	}
	canonical := c
	if peer.CharacterID < c.CharacterID {
		canonical = peer
	}
	r, err := wedding.ReadRoom(canonical, state.PairID, s.gameplayNow())
	if err != nil {
		return err
	}
	return errors.Join(
		s.sendGameUpperRawClass(session, 1011, weddingRoomUpdatesBody(r), 0),
		s.sendGameUpperRawClass(session, 1008, weddingRoomStateBody(r), 0),
	)
}

func weddingRoomStateBody(r wedding.RoomState) []byte {
	var w packetWriter
	n := 0
	for _, id := range r.Installed {
		if id != 0 {
			n++
		}
	}
	w.writeUint32(uint32(n))
	for slot, id := range r.Installed {
		if id != 0 {
			w.writeUint16(uint16(slot))
			w.writeUint32(uint32(id))
		}
	}
	w.writeBytes(weddingRoomTreeBody(r))
	return w.bytes()
}

func (s *Service) weddingRoomCommand(session *gameSession, opcode uint16, body []byte) error {
	identity, current := s.boundGameSessionCharacterSnapshot(session)
	if !current {
		return nil
	}
	cmd := wedding.RoomCommand{Opcode: opcode, Now: s.gameplayNow()}
	switch opcode {
	case 1045:
		if len(body) != 14 || body[13] > 1 {
			return nil
		}
	case 1046:
		if len(body) != 27 {
			return nil
		}
		cmd.From, cmd.To = body[13], body[20]
		cmd.FromSlot = int(binary.LittleEndian.Uint16(body[14:16]))
		cmd.FromItem = int64(binary.LittleEndian.Uint32(body[16:20]))
		cmd.ToSlot = int(binary.LittleEndian.Uint16(body[21:23]))
		cmd.ToItem = int64(binary.LittleEndian.Uint32(body[23:27]))
	case 1047:
		if len(body) != 23 {
			return nil
		}
		cmd.Slot = int32(binary.LittleEndian.Uint16(body[13:15]))
		cmd.ItemID = int64(binary.LittleEndian.Uint32(body[15:19]))
		cmd.Count = int64(binary.LittleEndian.Uint32(body[19:23]))
	case 1048, 1049:
		if len(body) != 19 {
			return nil
		}
		cmd.Slot = int32(binary.LittleEndian.Uint16(body[13:15]))
		cmd.ItemID = int64(binary.LittleEndian.Uint32(body[15:19]))
		if cmd.Slot < 3 || cmd.Slot > 353 {
			return nil
		}
	case 1050, 1051:
		if len(body) != 0 {
			return nil
		}
	default:
		return nil
	}
	ctx, cancel := context.WithTimeout(context.Background(), createWriteTimeout)
	defer cancel()
	s.weddingMu.Lock()
	defer s.weddingMu.Unlock()
	c, err := s.weddingCharacter(ctx, identity)
	// TryLock preserves the town->presence lock order during scene changes.
	if err == nil {
		if !session.townMu.TryLock() {
			err = wedding.Busy
		} else {
			pos := session.townPositionSnapshot
			ready := session.townSceneReadyCharacterID == identity.character
			session.townMu.Unlock()
			if !ready || pos.CharacterID != identity.character || pos.TownID != 101 {
				err = wedding.Invalid
			}
		}
	}
	var result wedding.RoomResult
	if err == nil {
		result, err = s.commitWeddingRoom(ctx, session, c, cmd)
	}
	var code wedding.Error
	if err != nil && !errors.As(err, &code) {
		code = wedding.Invalid
	}
	var ack packetWriter
	ack.writeInt32(int(code))
	if opcode == 1045 {
		ack.writeByte(body[13])
	}
	ackErr := s.sendGameUpperSuccess(session, opcode, ack.bytes())
	if err != nil {
		s.logGameEvent(session, "game-wedding-room-rejected", "opcode", opcode, "error", err)
		return ackErr
	}
	if opcode == 1045 {
		return errors.Join(ackErr, s.sendGameUpperRawClass(session, 1009, weddingRoomCargoBody(result.Room), 0))
	}
	if opcode == 1046 {
		var w packetWriter
		w.writeByte(0)
		for _, endpoint := range []struct {
			kind byte
			slot int
			id   int64
		}{{cmd.From, cmd.FromSlot, cmd.FromItem}, {cmd.To, cmd.ToSlot, cmd.ToItem}} {
			id, count := endpoint.id, int64(0)
			if endpoint.kind == 0 {
				id = result.Room.Installed[endpoint.slot]
				if id != 0 {
					count = 1
				}
			} else {
				item := result.Room.Cargo[endpoint.slot]
				id, count = item.ID, item.Count
			}
			w.writeByte(endpoint.kind)
			w.writeUint32(uint32(id))
			w.writeUint16(uint16(endpoint.slot))
			w.writeUint32(uint32(count))
		}
		ackErr = errors.Join(ackErr, s.sendGameUpperRawClass(session, 1010, w.bytes(), 0))
	}
	return errors.Join(ackErr, s.publishWeddingRoom(result, opcode))
}

func (s *Service) commitWeddingRoom(ctx context.Context, session *gameSession, c dnfrepo.CharacterRecord, cmd wedding.RoomCommand) (wedding.RoomResult, error) {
	state, err := wedding.Read(c)
	if err != nil {
		return wedding.RoomResult{}, err
	}
	cat, err := s.currentWeddingCatalog()
	if err != nil {
		return wedding.RoomResult{}, err
	}
	rules, err := s.currentWeddingRoomCatalog()
	if err != nil {
		return wedding.RoomResult{}, err
	}
	cmd.ActorID, cmd.PartnerID, cmd.AccountID = c.CharacterID, strconv.Itoa(int(state.PartnerID)), s.accountIDForSession(session)
	if cmd.Opcode == 1050 {
		items, err := s.currentPVFItemCatalog()
		if err != nil {
			return wedding.RoomResult{}, err
		}
		cmd.Grant = func(bag *dnfrepo.InventoryRecord, item wedding.Item) error {
			def, err := items.ResolveItem(uint32(item.ID))
			if err != nil {
				return err
			}
			_, err = s.grantCurrentWeddingMainProduct(bag, def, uint32(item.Count))
			return err
		}
	}
	repos, _ := s.repositoryGroup()
	return wedding.CommitRoom(ctx, repos, cat, rules, cmd)
}

func (s *Service) publishWeddingRoom(result wedding.RoomResult, opcode uint16) error {
	var err error
	for i, c := range result.Characters {
		id, _ := strconv.ParseUint(c.CharacterID, 10, 16)
		identity, online := s.onlineGameSessionCharacterSnapshot(uint16(id))
		if !online {
			continue
		}
		session := identity.session
		if opcode == 21 || opcode == 1046 || opcode == 1047 {
			err = errors.Join(err, s.sendGameUpperRawClass(session, 1011, weddingRoomUpdatesBody(result.Room), 0))
			if opcode == 1046 {
				// The snapshot recomputes native decoration bonuses after all
				// changed slots (including a displaced third item) are applied.
				err = errors.Join(err, s.sendGameUpperRawClass(session, 1008, weddingRoomStateBody(result.Room), 0))
			}
		}
		if opcode >= 1048 && opcode <= 1051 {
			err = errors.Join(err, s.sendGameUpperRawClass(session, 1012, weddingRoomTreeBody(result.Room), 0))
		}
		if result.AssetsChanged {
			// Only the actor paid; harvest rewards go to both inventories.
			if i == 0 || opcode == 1050 {
				err = errors.Join(err, s.sendWeddingInventorySnapshot(session, result.Inventories[i]))
			}
		}
		if opcode == 1050 && result.Reward.ID > 0 {
			var w packetWriter
			w.writeUint16(1)
			w.writeUint32(uint32(result.Reward.ID))
			w.writeUint32(uint32(result.Reward.Count))
			err = errors.Join(err, s.sendGameUpperRawClass(session, 1014, w.bytes(), 0))
		}
	}
	if opcode == 21 {
		err = errors.Join(err, s.publishWeddingCommit(result.Result, 9))
	}
	return err
}

func (s *Service) commitWeddingDecorationBuy(ctx context.Context, session *gameSession, pricing currentNPCShopPricing, request currentNPCShopBuyRequest) (currentNPCShopBuyResult, error) {
	identity, current := s.boundGameSessionCharacterSnapshot(session)
	if !current {
		return currentNPCShopBuyResult{}, wedding.Invalid
	}
	s.weddingMu.Lock()
	defer s.weddingMu.Unlock()
	c, err := s.weddingCharacter(ctx, identity)
	if err != nil {
		return currentNPCShopBuyResult{}, err
	}
	cmd := wedding.RoomCommand{Opcode: 21, ItemID: int64(request.ItemID), Count: int64(request.Count), Now: s.gameplayNow()}
	if pricing.MaterialExchange {
		if pricing.NeedMaterialCount > math.MaxInt32/cmd.Count {
			return currentNPCShopBuyResult{}, wedding.Invalid
		}
		cmd.Materials = []wedding.Item{{ID: int64(pricing.NeedMaterialItem), Count: pricing.NeedMaterialCount * cmd.Count}}
	} else {
		if pricing.BuyGold > math.MaxInt32/cmd.Count {
			return currentNPCShopBuyResult{}, wedding.Invalid
		}
		cmd.Gold = pricing.BuyGold * cmd.Count
	}
	result, err := s.commitWeddingRoom(ctx, session, c, cmd)
	if err != nil {
		return currentNPCShopBuyResult{}, err
	}
	item := result.Room.Cargo[result.Slot]
	entry := currentItemListEntryFromStack(19, int16(result.Slot), dnfrepo.ItemStack{ItemID: item.ID, Count: item.Count})
	// 上游用一个 ListType 字段同时表达"ACK 行"和"增量行属于哪个容器"。工作区把它
	// 拆成了两件事：Updates 固定走 list 0（主背包），ItemListType/ItemUpdates 承载
	// 别的容器。婚房装饰落在 19 号容器，所以装饰行必须走 ItemUpdates，金币钱包行
	// 留在 Updates，否则装饰会被客户端归进 list 0 的页面里而无法使用。
	out := currentNPCShopBuyResult{
		GoldAfter: result.Characters[0].Stats["gold"], Item: entry,
		Slot: int16(result.Slot), ItemID: request.ItemID, Count: request.Count,
		ItemListType: 19, ItemUpdates: []currentItemListEntry{entry},
	}
	out.WeddingRoom = &result
	out.Updates = []currentItemListEntry{currentGoldWalletItemListEntry(out.GoldAfter)}
	repos, _ := s.repositoryGroup()
	out.CoinAfter = s.loadCurrentAccountCera(ctx, repos, session)
	// The purchase ACK is emitted by the shared shop handler; room publication
	// follows it using the persisted result, so failed notifications never repay.
	return out, nil
}
