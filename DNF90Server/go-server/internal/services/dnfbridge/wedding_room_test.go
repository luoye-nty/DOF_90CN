package dnfbridge

import (
	"context"
	"encoding/binary"
	"os"
	"testing"
	"time"

	dnfproto "longheng.io/server/internal/modules/dnf/protocol"
	dnfrepo "longheng.io/server/internal/modules/dnf/repository"
	"longheng.io/server/internal/modules/dnf/wedding"
	platformpvf "longheng.io/server/internal/platform/pvf"
)

func TestWeddingPartnerAndIncrementalStateWire(t *testing.T) {
	s, repos, a, b := weddingTestService(t)
	ctx := context.Background()
	var result wedding.Result
	for i, id := range []string{"1", "2"} {
		c, _, _ := repos.Character.Load(ctx, id)
		c.Stats = map[string]int64{"wedding_state": 4, "wedding_id": 65538, "wedding_partner_id": int64(2 - i), "wedding_love": 7, "wedding_ring": 1}
		if err := repos.Character.Save(ctx, c); err != nil {
			t.Fatal(err)
		}
		if err := repos.Equipment.Save(ctx, dnfrepo.EquipmentRecord{CharacterID: id, Entries: map[string]dnfrepo.EquipmentEntry{"12": {ItemID: 100, SlotIndex: 12}}}); err != nil {
			t.Fatal(err)
		}
		result.Characters[i] = c
	}
	// The dungeon menu resolves the spouse by the cached preview name. Every
	// full state notification deletes that preview, including town re-entry.
	for repeat := 0; repeat < 2; repeat++ {
		for i, peer := range []*gameSession{a, b} {
			if err := s.sendWeddingCharacterState(peer, result.Characters[i]); err != nil {
				t.Fatal(err)
			}
			state, rest := splitLongHengGameServerUpperPacket(t, peer.conn.(*bufferConn).write.Bytes())
			if state.Header.MsgID != 1000 || state.Body[4] != 4 || len(rest) == 0 {
				t.Fatalf("full state must restore partner cache: state=%+v rest=%x", state, rest)
			}
			partner, rest := splitLongHengGameServerUpperPacket(t, rest)
			other := result.Characters[1-i]
			if partner.Header.MsgID != 1035 || len(partner.Body) != 98 || partner.Body[0] != 1 || len(rest) != 0 ||
				binary.LittleEndian.Uint32(partner.Body[5:9]) != 65538 ||
				binary.LittleEndian.Uint32(partner.Body[9:13]) != uint32(2-i) ||
				string(partner.Body[13:13+len(other.Name)]) != other.Name {
				t.Fatalf("partner cache after full state: %+v %x", partner.Header, partner.Body)
			}
			peer.conn.(*bufferConn).write.Reset()
		}
	}
	if err := s.handleWeddingPartner(a, make([]byte, 17)); err != nil {
		t.Fatal(err)
	}
	packet, trailing := splitLongHengGameServerUpperPacket(t, a.conn.(*bufferConn).write.Bytes())
	if packet.Header.MsgID != 1035 || len(packet.Body) != 98 || len(trailing) != 0 || packet.Body[0] != 1 {
		t.Fatalf("partner packet: %+v %x", packet.Header, packet.Body)
	}
	body := packet.Body[1:]
	if binary.LittleEndian.Uint32(body[4:8]) != 65538 || binary.LittleEndian.Uint32(body[8:12]) != 2 || string(body[12:18]) != "second" || body[42] != 30 || body[43] != 1 || binary.LittleEndian.Uint32(body[93:97]) != 100 {
		t.Fatalf("partner layout: %x", body)
	}
	for _, peer := range []*gameSession{a, b} {
		peer.conn.(*bufferConn).write.Reset()
	}
	if err := s.publishWeddingCommit(result, 9); err != nil {
		t.Fatal(err)
	}
	for i, peer := range []*gameSession{a, b} {
		packet, trailing := splitLongHengGameServerUpperPacket(t, peer.conn.(*bufferConn).write.Bytes())
		if packet.Header.MsgID != 1003 || len(packet.Body) != 8 || len(trailing) != 0 || binary.LittleEndian.Uint32(packet.Body[:4]) != uint32(i+1) || binary.LittleEndian.Uint32(packet.Body[4:]) != 7 {
			t.Fatalf("love update: %+v %x", packet.Header, packet.Body)
		}
	}
	for ring := int64(0); ring <= 5; ring++ {
		for i, peer := range []*gameSession{a, b} {
			peer.conn.(*bufferConn).write.Reset()
			result.Characters[i].Stats["wedding_ring"] = ring
		}
		if err := s.publishWeddingCommit(result, 8); err != nil {
			t.Fatal(err)
		}
		for _, peer := range []*gameSession{a, b} {
			packet, trailing := splitLongHengGameServerUpperPacket(t, peer.conn.(*bufferConn).write.Bytes())
			if packet.Header.MsgID != 1001 || len(packet.Body) != 1 || packet.Body[0] != byte(ring) || len(trailing) != 0 {
				t.Fatalf("ring %d update: %+v %x", ring, packet.Header, packet.Body)
			}
		}
	}
}

func TestWeddingRoomProtocolRealPVF(t *testing.T) {
	path := os.Getenv("DNFBRIDGE_REAL_PVF_SMOKE")
	if path == "" {
		t.Skip("set DNFBRIDGE_REAL_PVF_SMOKE for room wire validation")
	}
	archive, err := platformpvf.Open(path)
	if err != nil {
		t.Fatal(err)
	}
	for _, upper := range []bool{false, true} {
		s, repos, a, b := weddingTestService(t)
		s.initialEquipmentArchive = archive
		for i, id := range []string{"1", "2"} {
			c, _, _ := repos.Character.Load(context.Background(), id)
			c.Stats = map[string]int64{"wedding_state": 4, "wedding_id": 65538, "wedding_partner_id": int64(2 - i)}
			if id == "1" {
				c.Stats["wedding_house_id"] = 65538
				c.Stats["wedding_house_cargo_0"] = 10002694
				c.Stats["wedding_house_count_0"] = 1
			}
			if err := repos.Character.Save(context.Background(), c); err != nil {
				t.Fatal(err)
			}
			if err := repos.Inventory.Save(context.Background(), dnfrepo.InventoryRecord{CharacterID: id, Slots: map[string]dnfrepo.ItemStack{"0:80": {ItemID: 10002766, Count: 1}}}); err != nil {
				t.Fatal(err)
			}
		}
		a.townPositionSnapshot.TownID = 101
		b.townPositionSnapshot.TownID = 101
		send := func(op uint16, body []byte) {
			t.Helper()
			m := weddingGameplayModule()
			req := gameplayRequest{Body: body, Classification: dnfproto.DefaultChannelClassification}
			var err error
			if upper {
				err = m.UpperHandlers[op](s, a, req)
			} else {
				req.Body = m.LegacyNormalizers[op](body)
				err = m.LegacyHandlers[op](s, a, req)
			}
			if err != nil {
				t.Fatal(err)
			}
		}
		send(1045, make([]byte, 14))
		ack, rest := splitLongHengGameServerUpperPacket(t, a.conn.(*bufferConn).write.Bytes())
		cargo, rest := splitLongHengGameServerUpperPacket(t, rest)
		if ack.Header.MsgID != 1045 || len(ack.Body) != 6 || ack.Body[0] != 1 || binary.LittleEndian.Uint32(ack.Body[1:5]) != 0 || cargo.Header.MsgID != 1009 || len(cargo.Body) != 18 || binary.LittleEndian.Uint32(cargo.Body[:4]) != 1 || binary.LittleEndian.Uint32(cargo.Body[6:10]) != 10002694 || len(rest) != 0 {
			t.Fatalf("cargo open: ack=%x cargo=%x", ack.Body, cargo.Body)
		}
		a.conn.(*bufferConn).write.Reset()
		body := make([]byte, 19)
		binary.LittleEndian.PutUint16(body[13:15], 80)
		binary.LittleEndian.PutUint32(body[15:19], 10002766)
		send(1048, body)
		ack, rest = splitLongHengGameServerUpperPacket(t, a.conn.(*bufferConn).write.Bytes())
		tree, _ := splitLongHengGameServerUpperPacket(t, rest)
		if ack.Header.MsgID != 1048 || len(ack.Body) != 5 || binary.LittleEndian.Uint32(ack.Body[1:]) != 0 || tree.Header.MsgID != 1012 || len(tree.Body) != 13 || binary.LittleEndian.Uint32(tree.Body[:4]) != 1 || int64(binary.LittleEndian.Uint32(tree.Body[4:8])) < time.Now().Add(29*24*time.Hour).Unix() {
			t.Fatalf("plant ack=%x tree=%x", ack.Body, tree.Body)
		}
		peerTree, _ := splitLongHengGameServerUpperPacket(t, b.conn.(*bufferConn).write.Bytes())
		if peerTree.Header.MsgID != 1012 || binary.LittleEndian.Uint32(peerTree.Body[:4]) != 1 {
			t.Fatalf("partner tree=%x", peerTree.Body)
		}
		c, _, _ := repos.Character.Load(context.Background(), "1")
		room, err := wedding.ReadRoom(c, 65538, time.Now())
		if err != nil || room.Tree != 1 {
			t.Fatalf("restore=%+v %v", room, err)
		}
		updates := weddingRoomUpdatesBody(room)
		if len(updates) != 4+68*11 || binary.LittleEndian.Uint32(updates[:4]) != 68 {
			t.Fatalf("updates length=%d", len(updates))
		}
		state := weddingRoomStateBody(room)
		if len(state) != 17 || binary.LittleEndian.Uint32(state[4:8]) != 1 {
			t.Fatalf("state=%x", state)
		}
	}
}
