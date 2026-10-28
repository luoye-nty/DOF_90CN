package dnfbridge

import (
	"bytes"
	"context"
	"testing"
	"time"

	"golang.org/x/text/encoding/simplifiedchinese"

	dnfenum "longheng.io/server/internal/modules/dnf/dnfenum"
	dnfproto "longheng.io/server/internal/modules/dnf/protocol"
	dnfrepo "longheng.io/server/internal/modules/dnf/repository"
	dnfrepomemory "longheng.io/server/internal/modules/dnf/repository/memory"
)

// 本包的用例默认在"赠送功能开启"的状态下跑（生产默认是关闭的），
// 下面两个开关用例自己临时关掉、跑完再恢复。
func init() { currentCeraShopGiftEnabled = true }

func disableCeraShopGiftForTest(t *testing.T) {
	t.Helper()
	previous := currentCeraShopGiftEnabled
	currentCeraShopGiftEnabled = false
	t.Cleanup(func() { currentCeraShopGiftEnabled = previous })
}

// ★ 运营开关：赠送功能暂时关闭时
//  1. op136（查收件人）**不查库**就拒绝，并且**必须回包**（不回包客户端会停在等待状态）；
//  2. 同时下发服务端公告「赠送功能暂时关闭」——客户端那张结果码表里没有这句话，
//     只能走公告通道；
//  3. op64（真下单）也必须被拦：不信任客户端走过 op136，不能让它直接下单扣钱。
func TestCurrentCeraShopGiftFeatureSwitchBlocksQueryWithNoticeAndAck(t *testing.T) {
	disableCeraShopGiftForTest(t)
	repositories := testRepositoryGroup()
	// 连好友仓库都不给：关闭状态下不该有任何查库行为，能跑通就说明没查。
	repositories.UnitedFriend = nil
	service := newCeraShopGiftQueryTestService(repositories)
	conn := &bufferConn{}
	session := &gameSession{conn: conn, connID: "gift-disabled", selectedCharacterID: 21}

	if err := service.handleCurrentCeraShopGiftQuery(session, currentCeraShopGiftQueryLiveCaptureBody()); err != nil {
		t.Fatalf("disabled gift query: %v", err)
	}
	raw := conn.write.Bytes()
	if len(raw) == 0 {
		t.Fatal("disabled gift query produced no packets: the client would hang")
	}
	// 公告文本按 GBK 编码后直接在整串回包里找 —— 不去解 class-0 场景包的信封细节，
	// 只关心"这句话确实发出去了"。
	wantText, err := simplifiedchinese.GBK.NewEncoder().Bytes([]byte("赠送功能暂时关闭"))
	if err != nil {
		t.Fatal(err)
	}
	if !bytes.Contains(raw, wantText) {
		t.Fatalf("reply does not carry the closed-gift notice text (hex=%x)", raw)
	}
	sawNotice := false
	sawAck := false
	for len(raw) > 0 {
		packet, rest := splitGameServerUpperPacket(t, raw)
		switch {
		case packet.Header.MsgID == currentNativeNoticeMsgID:
			sawNotice = true
		case packet.Header.MsgID == uint16(dnfenum.CmdPacketQueryCharacInfo):
			sawAck = true
			if packet.Header.Classification != dnfproto.DefaultChannelClassification {
				t.Fatalf("ack classification=%d", packet.Header.Classification)
			}
			if len(packet.Body) != 2 || packet.Body[0] != 0 ||
				packet.Body[1] != currentCeraShopGiftDisabledFailureCode {
				t.Fatalf("op136 ack body=%x want 00%02x", packet.Body, currentCeraShopGiftDisabledFailureCode)
			}
		}
		raw = rest
	}
	if !sawNotice {
		t.Fatal("no native notice packet was sent: the player would see no reason")
	}
	if !sawAck {
		t.Fatal("no op136 failure ack was sent: the client would stay in its waiting state")
	}
}

// 关闭期间 op64 赠送必须整笔拒绝：不扣点券、不发邮件。
func TestCurrentCeraShopGiftFeatureSwitchBlocksGiftPurchase(t *testing.T) {
	disableCeraShopGiftForTest(t)
	ctx := context.Background()
	catalog := mustCurrentCeraShopTestCatalog(t)
	repositories := dnfrepomemory.NewMemoryGroup()
	now := time.Now().UTC()
	if err := repositories.Account.Save(ctx, dnfrepo.AccountRecord{
		AccountID: "account-1",
		Metadata:  map[string]string{"account_cera": "200"},
		CreatedAt: now.Add(-30 * 24 * time.Hour),
	}); err != nil {
		t.Fatal(err)
	}
	for _, character := range []dnfrepo.CharacterRecord{
		{CharacterID: "19", AccountID: "account-1", Name: "1111", Job: "0", Level: 90,
			Stats: map[string]int64{"grow_type": 1}},
		{CharacterID: "22", AccountID: "account-2", Name: "9999", Job: "0", Level: 90,
			Stats: map[string]int64{"grow_type": 1}},
	} {
		if err := repositories.Character.Save(ctx, character); err != nil {
			t.Fatal(err)
		}
		if err := repositories.Inventory.Save(ctx, dnfrepo.InventoryRecord{
			CharacterID: character.CharacterID, Slots: map[string]dnfrepo.ItemStack{},
		}); err != nil {
			t.Fatal(err)
		}
	}
	// 故意不造好友关系：关闭状态应该在规则判定之前就拒绝。
	service := newCeraShopPremiumTestService(repositories, catalog)
	conn := &bufferConn{}
	session := &gameSession{conn: conn, connID: "gift-disabled-purchase", selectedCharacterID: 19}
	if err := service.handleCurrentCeraShopPurchase(session, currentCeraShopGiftPurchaseBody("9999", 100050)); err != nil {
		t.Fatal(err)
	}
	raw := conn.write.Bytes()
	if len(raw) == 0 {
		t.Fatal("disabled gift purchase produced no packets")
	}
	sawNotice := false
	sawAck := false
	for len(raw) > 0 {
		packet, rest := splitGameServerUpperPacket(t, raw)
		if packet.Header.MsgID == currentNativeNoticeMsgID {
			sawNotice = true
		}
		if packet.Header.MsgID == uint16(dnfenum.CmdPacketBuyCerashopItem) {
			sawAck = true
			if len(packet.Body) != 22 || packet.Body[0] != 0 {
				t.Fatalf("op64 body=%x want a 22-byte failure ack", packet.Body)
			}
		}
		raw = rest
	}
	if !sawNotice || !sawAck {
		t.Fatalf("notice=%t ack=%t want both", sawNotice, sawAck)
	}
	if balance := loadCeraShopAccountCera(t, ctx, repositories, "account-1"); balance != 200 {
		t.Fatalf("sender cera=%d want=200 (a disabled gift must not charge)", balance)
	}
	mailbox, found, err := repositories.Mailbox.Load(ctx, "22")
	if err != nil {
		t.Fatal(err)
	}
	if found && len(mailbox.Mails) != 0 {
		t.Fatalf("recipient got %d mails while gifting is disabled", len(mailbox.Mails))
	}
}
