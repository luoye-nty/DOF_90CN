package dnfbridge

import (
	"bytes"
	"context"
	"encoding/binary"
	"encoding/hex"
	"strconv"
	"strings"
	"testing"
	"time"

	dnfcerashop "longheng.io/server/internal/modules/dnf/cerashop"
	dnfenum "longheng.io/server/internal/modules/dnf/dnfenum"
	dnfproto "longheng.io/server/internal/modules/dnf/protocol"
	dnfrepo "longheng.io/server/internal/modules/dnf/repository"
	dnfrepomemory "longheng.io/server/internal/modules/dnf/repository/memory"
)

// currentCeraShopGiftQueryLiveCaptureBody 是 2026-09-27 15:32 真机抓到的那两包
// （`018800160000002a2d15a8db00 04000000 39393939 01`）去掉 13 字节 legacy 头之后的
// body：DSTR("9999") + u8(1)。两个用例共用它，避免"测试自己造一个刚好能过的包"。
func currentCeraShopGiftQueryLiveCaptureBody() []byte {
	return []byte{0x04, 0x00, 0x00, 0x00, '9', '9', '9', '9', 0x01}
}

// currentCeraShopGiftQueryLiveCaptureOp64Body 是同期正常购买（非赠送）抓到的一包
// op64 body。收件人查询的解析器必须**拒绝**它——两条命令的 body 前缀太像，
// 交叉误吞会把"购买"当成"查询"。
func currentCeraShopGiftQueryLiveCaptureOp64Body() []byte {
	return []byte{
		0x00, 0x00, 0x01, 0x00,
		0x00, 0x00, 0xff, 0x00, 0x12, 0x92, 0x01, 0x00,
		0x00, 0x00, 0x00, 0x00, 0x00, 0x00, 0x00,
	}
}

// clientGiftQueryReplyReader 逐字节复刻客户端 op136 回包读包器（RVA 0x1903E90）的
// 读取顺序：0x3057D20 读 DSTR、0x30E39E0 读 u16、0x30E3970 读 u8。
// 这个读包器才是回包形状的**唯一权威**——测试用它而不是用服务端自己的构造器来校验。
type clientGiftQueryReplyReader struct {
	t    *testing.T
	body []byte
	off  int
}

func (r *clientGiftQueryReplyReader) u8() byte {
	r.t.Helper()
	if r.off+1 > len(r.body) {
		r.t.Fatalf("client reader underflow at %d (body=%x)", r.off, r.body)
	}
	value := r.body[r.off]
	r.off++
	return value
}

func (r *clientGiftQueryReplyReader) u16() uint16 {
	r.t.Helper()
	if r.off+2 > len(r.body) {
		r.t.Fatalf("client reader underflow at %d (body=%x)", r.off, r.body)
	}
	value := binary.LittleEndian.Uint16(r.body[r.off : r.off+2])
	r.off += 2
	return value
}

// dstr 复刻 0x3057C80：先读 u32 长度，长度必须在 1..maxBytes-1 之间，
// 否则客户端**不消费名字字节**（游标错位，后续字段全读错）。
func (r *clientGiftQueryReplyReader) dstr(maxBytes int) []byte {
	r.t.Helper()
	length := int(binary.LittleEndian.Uint32(r.body[r.off : r.off+4]))
	r.off += 4
	if length <= 0 || length >= maxBytes {
		r.t.Fatalf("client reader would reject dstr length %d (max %d): cursor desync", length, maxBytes)
	}
	if r.off+length > len(r.body) {
		r.t.Fatalf("client reader dstr overrun at %d len=%d body=%x", r.off, length, r.body)
	}
	value := r.body[r.off : r.off+length]
	r.off += length
	return value
}

func TestCurrentCeraShopGiftQueryParsesLiveCapture(t *testing.T) {
	request, err := parseCurrentCeraShopGiftQuery(currentCeraShopGiftQueryLiveCaptureBody())
	if err != nil {
		t.Fatalf("live capture rejected: %v", err)
	}
	if request.Name != "9999" {
		t.Fatalf("name=%q want=9999", request.Name)
	}
	if !bytes.Equal(request.NameRaw, []byte("9999")) {
		t.Fatalf("name raw=%x want=39393939", request.NameRaw)
	}
	if request.Kind != 1 {
		t.Fatalf("kind=%d want=1", request.Kind)
	}
	if request.RawLen != 9 {
		t.Fatalf("raw len=%d want=9", request.RawLen)
	}
}

// 收件人查询的解析器必须对自己的一包接受、对 op64 的一包拒绝：两条 body 的前 4 字节
// 都是 `u32`，只有"多出来的那一个类型字节"能把它们区分开。
func TestCurrentCeraShopGiftQueryRejectsForeignAndMalformedBodies(t *testing.T) {
	cases := []struct {
		name string
		body []byte
	}{
		{"empty", nil},
		{"op64 normal purchase body", currentCeraShopGiftQueryLiveCaptureOp64Body()},
		{"missing kind byte", []byte{0x04, 0x00, 0x00, 0x00, '9', '9', '9', '9'}},
		{"trailing byte", []byte{0x04, 0x00, 0x00, 0x00, '9', '9', '9', '9', 0x01, 0x00}},
		{"declared length past end", []byte{0x09, 0x00, 0x00, 0x00, '9', '9', '9', '9', 0x01}},
		{"declared length short", []byte{0x02, 0x00, 0x00, 0x00, '9', '9', '9', '9', 0x01}},
		{"zero length", []byte{0x00, 0x00, 0x00, 0x00, 0x01}},
		{"embedded nul", []byte{0x03, 0x00, 0x00, 0x00, 'a', 0x00, 'b', 0x01}},
		{"all zero name", []byte{0x02, 0x00, 0x00, 0x00, 0x00, 0x00, 0x01}},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if _, err := parseCurrentCeraShopGiftQuery(tc.body); err == nil {
				t.Fatalf("body %x was accepted", tc.body)
			}
		})
	}
}

// 用客户端自己的读包顺序把回包解一遍：任何字段错位/宽度错都会被这里拦住。
func TestCurrentCeraShopGiftQueryAckMatchesClientReaderOrder(t *testing.T) {
	ack := buildCurrentCeraShopGiftQueryAck([]byte("9999"), 87, 11, 3, 0)
	reader := &clientGiftQueryReplyReader{t: t, body: ack}

	// class-1 分发器先吃掉的成功字节，同时作为处理器的第二个参数（0 = 失败分支）。
	if flag := reader.u8(); flag != 1 {
		t.Fatalf("success flag=%d want=1 (0 would select the client's failure branch)", flag)
	}
	if name := reader.dstr(currentCeraShopGiftQueryMaxNameBytes); !bytes.Equal(name, []byte("9999")) {
		t.Fatalf("echoed name=%x want=39393939", name)
	}
	if level := reader.u16(); level != 87 {
		t.Fatalf("level(u16)=%d want=87", level)
	}
	// 第 4 个字段进窗口 +0x200，是结果对话框「职业」栏唯一查表用的值。
	// 2026-09-27 真机：这一格放过 grow(35)，「职业」栏空白。
	if job := reader.u8(); job != 11 {
		t.Fatalf("job=%d want=11 (this is the byte the dialog looks the job name up with)", job)
	}
	if grow := reader.u8(); grow != 3 {
		t.Fatalf("grow=%d want=3", grow)
	}
	// 对方已收礼物数：客户端判 `cartQuantity + 该值 <= CeraShop.etc 上限(20)`。
	// 服务端还没记账，必须回 0，否则会把「赠送」按钮直接灰掉。
	if received := reader.u8(); received != 0 {
		t.Fatalf("received gifts=%d want=0 (non-zero can grey the gift button out)", received)
	}
	if boolean := reader.u8(); boolean != 1 {
		t.Fatalf("bool field=%d want=1 (the client's own default)", boolean)
	}
	if trailing := reader.u8(); trailing != 0 {
		t.Fatalf("window +0x20C=%d want=0", trailing)
	}
	if selector := reader.u8(); selector != 0 {
		t.Fatalf("dialog selector=%d want=0 (the client's own default)", selector)
	}
	if reader.off != len(ack) {
		t.Fatalf("client reader consumed %d of %d bytes", reader.off, len(ack))
	}
}

// 失败回包 = `u8 0 + u8 结果码`（两个字节），与全服其它 class-1 失败包同形。
//
// 现场 2026-09-27 17:17 的教训：只回一个字节时，客户端把结果码当**标志位**读，
// `a5` 非 0 ⇒ 走成功分支 ⇒ 去读没发过去的 name/level/职业 ⇒ 对话框里是垃圾值。
func TestCurrentCeraShopGiftQueryFailureBodyIsFlagThenResultCode(t *testing.T) {
	for _, test := range []struct {
		name string
		code byte
		want byte
	}{
		{name: "self", code: currentCeraShopGiftQueryCodeSelf, want: 0x07},
		{name: "not found", code: currentCeraShopGiftQueryCodeNotFound, want: 0x15},
		{name: "daily cap", code: currentCeraShopGiftQueryCodeDailyCap, want: 0xa3},
		{name: "friend too new", code: currentCeraShopGiftQueryCodeFriendTooNew, want: 0xa4},
		{name: "not friend", code: currentCeraShopGiftQueryCodeNotFriend, want: 0xa5},
		{name: "retry", code: currentCeraShopGiftQueryCodeRetry, want: 0x02},
		// 0 / 1 不在失败码表里：回 0 会让客户端把"标志位=失败 + 码=0"当成未知提示，
		// 回 1 更糟（会被当成成功包）。两者都退化成"请稍后再使用"。
		{name: "zero is not a failure code", code: 0, want: currentCeraShopGiftQueryCodeRetry},
		{name: "success is not a failure code", code: currentCeraShopGiftQueryCodeSuccess, want: currentCeraShopGiftQueryCodeRetry},
		{name: "unknown code", code: 0x7f, want: currentCeraShopGiftQueryCodeRetry},
	} {
		t.Run(test.name, func(t *testing.T) {
			body := buildCurrentCeraShopGiftQueryFailure(test.code)
			if len(body) != 2 {
				t.Fatalf("failure body=%x want flag+code (2 bytes)", body)
			}
			if body[0] != 0 {
				t.Fatalf("failure flag=%#x want=0 (non-zero makes the client take the SUCCESS branch)", body[0])
			}
			if body[1] != test.want {
				t.Fatalf("failure code=%#x want=%#x", body[1], test.want)
			}
		})
	}
}

// ★★ 赠送的正向验收：付款方扣点券、**商品邮寄给收件人**、付款方背包一个格子都不动。
//
// 这一条同时钉住三件事：
//  1. 赠送走的是**同一套结账**（价钱/限购/点券瀑布全不变），只是目标容器换成邮箱；
//  2. 商品不会被落进付款方背包（"送给别人 = 给自己买"这条红线）；
//  3. 邮件带上了赠送方署名与商品实例（收件人领取到的就是他该拿到的那件东西）。
func TestHandleCurrentCeraShopGiftPurchaseMailsTheGoodsToTheRecipient(t *testing.T) {
	catalog := mustCurrentCeraShopTestCatalog(t)
	repositories := dnfrepomemory.NewMemoryGroup()
	ctx := context.Background()
	now := time.Now().UTC()

	// 赠送方：有名字、有职业、账号满 30 天、背包是空的。
	if err := repositories.Account.Save(ctx, dnfrepo.AccountRecord{
		AccountID: "account-1",
		Metadata:  map[string]string{"account_cera": "200"},
		CreatedAt: now.Add(-30 * 24 * time.Hour),
	}); err != nil {
		t.Fatal(err)
	}
	if err := repositories.Character.Save(ctx, dnfrepo.CharacterRecord{
		CharacterID: "19",
		AccountID:   "account-1",
		Name:        "1111",
		Job:         "0",
		Level:       90,
		Stats:       map[string]int64{"grow_type": 1},
	}); err != nil {
		t.Fatal(err)
	}
	if err := repositories.Inventory.Save(ctx, dnfrepo.InventoryRecord{CharacterID: "19", Slots: map[string]dnfrepo.ItemStack{}}); err != nil {
		t.Fatal(err)
	}
	// 收件人：同职业、同大区，与赠送方互为好友 30 天。
	if err := repositories.Character.Save(ctx, dnfrepo.CharacterRecord{
		CharacterID: "22",
		AccountID:   "account-2",
		Name:        "9999",
		Job:         "0",
		Level:       90,
		Stats:       map[string]int64{"grow_type": 1},
	}); err != nil {
		t.Fatal(err)
	}
	seedCeraShopGiftQueryFriendship(t, repositories, "1111", "9999", now.Add(-30*24*time.Hour))

	conn := &bufferConn{}
	service := newCeraShopPremiumTestService(repositories, catalog)
	session := &gameSession{conn: conn, connID: "cera-gift-purchase-ok", selectedCharacterID: 19}

	// 100050 = 道具 37（可叠加，能邮寄），单价 80，余额 200 够买。
	if err := service.handleCurrentCeraShopPurchase(session, currentCeraShopGiftPurchaseBody("9999", 100050)); err != nil {
		t.Fatal(err)
	}
	// 赠送成功后付款方会收到**多个**包：点券余额更新（class1/op53 那一路）和
	// 每件商品的 op64 成功回包（和普通购买同形）。这里把所有包都拆开找 op64，
	// 而不是只拆第一个 —— 只拆第一个会把"余额包"当成购买回包。
	raw := conn.write.Bytes()
	if len(raw) == 0 {
		t.Fatal("no reply: the gift purchase produced no packets")
	}
	var purchaseAcks []dnfproto.ChannelPacket
	for len(raw) > 0 {
		packet, rest := splitGameServerUpperPacket(t, raw)
		if packet.Header.MsgID == uint16(dnfenum.CmdPacketBuyCerashopItem) {
			purchaseAcks = append(purchaseAcks, packet)
		}
		raw = rest
	}
	if len(purchaseAcks) != 1 {
		t.Fatalf("op64 acks=%d want=1", len(purchaseAcks))
	}
	if purchaseAcks[0].Header.Classification != dnfproto.DefaultChannelClassification {
		t.Fatalf("op64 ack classification=%d want=%d",
			purchaseAcks[0].Header.Classification, dnfproto.DefaultChannelClassification)
	}
	if body := purchaseAcks[0].Body; len(body) == 0 || body[0] != 1 {
		t.Fatalf("op64 ack body=%x want a success ack (01 …)", body)
	}

	// 付款方：钱被扣了，背包没变。
	if balance := loadCeraShopAccountCera(t, ctx, repositories, "account-1"); balance != 120 {
		t.Fatalf("sender cera=%d want=120 (200 - 80)", balance)
	}
	inventory, found, err := repositories.Inventory.Load(ctx, "19")
	if err != nil || !found {
		t.Fatalf("load sender inventory found=%t err=%v", found, err)
	}
	if len(inventory.Slots) != 0 {
		t.Fatalf("sender inventory changed: %+v (a gift must never land in the payer's bag)", inventory.Slots)
	}

	// 收件人：收到一封带商品的邮件，寄件人是赠送方。
	mailbox, found, err := repositories.Mailbox.Load(ctx, "22")
	if err != nil || !found {
		t.Fatalf("load recipient mailbox found=%t err=%v", found, err)
	}
	if len(mailbox.Mails) != 1 {
		t.Fatalf("recipient mails=%d want=1: %+v", len(mailbox.Mails), mailbox.Mails)
	}
	for mailID, mail := range mailbox.Mails {
		if mail.SenderName != "1111" {
			t.Fatalf("mail %s sender=%q want=1111 (the gift must name its sender)", mailID, mail.SenderName)
		}
		if mail.RecipientCharacterID != "22" {
			t.Fatalf("mail %s recipient=%q want=22", mailID, mail.RecipientCharacterID)
		}
		if len(mail.Attachments) != 1 {
			t.Fatalf("mail %s attachments=%d want=1: %+v", mailID, len(mail.Attachments), mail.Attachments)
		}
		attachment := mail.Attachments[0]
		if attachment.ItemID != 37 || attachment.Count != 3 {
			t.Fatalf("mail %s attachment=%+v want item 37 x3", mailID, attachment)
		}
		if len(attachment.RawEntry) == 0 {
			t.Fatalf("mail %s attachment has no native row: %+v", mailID, attachment)
		}
		if attachment.Extra["source"] != currentCeraShopGiftMailSource {
			t.Fatalf("mail %s attachment source=%q want=%q", mailID, attachment.Extra["source"], currentCeraShopGiftMailSource)
		}
	}
}

// 「结账即消费」的商品（契约/扩容/改名卡…）不能赠送：它们只作用在付款方自己身上，
// 放行就等于"送给别人 = 给自己买"。必须在**扣钱之前**拒绝（回 10 该商品无法赠送）。
func TestHandleCurrentCeraShopGiftPurchaseRejectsConsumedAtCheckoutGoods(t *testing.T) {
	for _, test := range []struct {
		name      string
		commodity uint32
	}{
		{name: "premium contract", commodity: 100090},
		{name: "personal cargo upgrade", commodity: 100080},
		{name: "character slot expansion", commodity: 100084},
		{name: "main inventory upgrade", commodity: 101098},
		// 账号金库扩容走的是另一条结账函数（commitCurrentCeraShopAccountCargoUpgrade），
		// 是最容易漏掉的一条 —— 它必须同样在扣钱之前被赠送守卫拦下。
		{name: "account cargo upgrade", commodity: 100083},
		{name: "rename card", commodity: 100902},
		{name: "name tag", commodity: 100058},
	} {
		t.Run(test.name, func(t *testing.T) {
			catalog := mustCurrentCeraShopTestCatalog(t)
			repositories := dnfrepomemory.NewMemoryGroup()
			ctx := context.Background()
			now := time.Now().UTC()
			if err := repositories.Account.Save(ctx, dnfrepo.AccountRecord{
				AccountID: "account-1",
				Metadata:  map[string]string{"account_cera": "100000"},
				CreatedAt: now.Add(-30 * 24 * time.Hour),
			}); err != nil {
				t.Fatal(err)
			}
			if err := repositories.Character.Save(ctx, dnfrepo.CharacterRecord{
				CharacterID: "19", AccountID: "account-1", Name: "1111", Job: "0", Level: 90,
				Stats: map[string]int64{"grow_type": 1},
			}); err != nil {
				t.Fatal(err)
			}
			if err := repositories.Character.Save(ctx, dnfrepo.CharacterRecord{
				CharacterID: "22", AccountID: "account-2", Name: "9999", Job: "0", Level: 90,
				Stats: map[string]int64{"grow_type": 1},
			}); err != nil {
				t.Fatal(err)
			}
			if err := repositories.Inventory.Save(ctx, dnfrepo.InventoryRecord{CharacterID: "19", Slots: map[string]dnfrepo.ItemStack{}}); err != nil {
				t.Fatal(err)
			}
			seedCeraShopGiftQueryFriendship(t, repositories, "1111", "9999", now.Add(-30*24*time.Hour))

			conn := &bufferConn{}
			service := newCeraShopPremiumTestService(repositories, catalog)
			session := &gameSession{conn: conn, connID: "cera-gift-not-giftable", selectedCharacterID: 19}
			if err := service.handleCurrentCeraShopPurchase(session, currentCeraShopGiftPurchaseBody("9999", test.commodity)); err != nil {
				t.Fatal(err)
			}
			reply, _ := splitGameServerUpperPacket(t, conn.write.Bytes())
			if len(reply.Body) != 22 || reply.Body[0] != 0 || reply.Body[1] != currentCeraShopPurchaseFailureCodeGiftNotGiftable {
				t.Fatalf("failure body=%x want failure with code 0x%02x",
					reply.Body, currentCeraShopPurchaseFailureCodeGiftNotGiftable)
			}
			if balance := loadCeraShopAccountCera(t, ctx, repositories, "account-1"); balance != 100000 {
				t.Fatalf("sender cera=%d want=100000 (nothing may be charged for a rejected gift)", balance)
			}
			mailbox, found, err := repositories.Mailbox.Load(ctx, "22")
			if err != nil {
				t.Fatal(err)
			}
			if found && len(mailbox.Mails) != 0 {
				t.Fatalf("recipient got %d mails for a rejected gift", len(mailbox.Mails))
			}
		})
	}
}

// 探针：赠送用的"道具 → 邮件附件"转换对测试目录里那件可叠加道具必须成立。
// 不成立的话，正向下单用例只会看到一个笼统的 0x0a，查起来要重新逆向一遍。
func TestCurrentCeraShopGiftAttachmentConversionForTestPotion(t *testing.T) {
	catalog := mustCurrentCeraShopTestCatalog(t)
	definition, err := catalog.items.ResolveItem(37)
	if err != nil {
		t.Fatal(err)
	}
	t.Logf("item=37 kind=%v slot_start=%d stack_limit=%d", definition.Kind, definition.SlotStart, definition.StackLimit)
	attachments, err := currentCeraPackageOverflowAttachments(definition, 3, 100050)
	if err != nil {
		t.Fatalf("convert item 37: %v", err)
	}
	if len(attachments) != 1 || attachments[0].ItemID != 37 || attachments[0].Count != 3 {
		t.Fatalf("attachments=%+v", attachments)
	}
}

// 探针：直接调结账，把赠送收集阶段的真实错误打出来（处理器只回一个码，看不到原因）。
func TestCurrentCeraShopGiftCommitCollectsAttachmentsForTestPotion(t *testing.T) {
	catalog := mustCurrentCeraShopTestCatalog(t)
	repositories := dnfrepomemory.NewMemoryGroup()
	ctx := context.Background()
	now := time.Now().UTC()
	if err := repositories.Account.Save(ctx, dnfrepo.AccountRecord{
		AccountID: "account-1",
		Metadata:  map[string]string{"account_cera": "200"},
		CreatedAt: now.Add(-30 * 24 * time.Hour),
	}); err != nil {
		t.Fatal(err)
	}
	if err := repositories.Character.Save(ctx, dnfrepo.CharacterRecord{
		CharacterID: "19", AccountID: "account-1", Name: "1111", Job: "0", Level: 90,
		Stats: map[string]int64{"grow_type": 1},
	}); err != nil {
		t.Fatal(err)
	}
	if err := repositories.Inventory.Save(ctx, dnfrepo.InventoryRecord{CharacterID: "19", Slots: map[string]dnfrepo.ItemStack{}}); err != nil {
		t.Fatal(err)
	}
	request, err := parseCurrentCeraShopPurchaseRequest(currentCeraShopGiftPurchaseBody("9999", 100050))
	if err != nil {
		t.Fatal(err)
	}
	service := newCeraShopPremiumTestService(repositories, catalog)
	session := &gameSession{connID: "gift-commit-probe", selectedCharacterID: 19}
	result, commitErr := service.commitCurrentCeraShopPurchase(ctx, session, catalog, request)
	t.Logf("commit err=%v attachments=%d notes=%v paid=%s", commitErr, len(result.GiftAttachments), result.GiftGrantNotes, currentCeraShopGiftPaidSummary(result))
	if commitErr != nil {
		t.Fatalf("gift commit failed: %v", commitErr)
	}
	if len(result.GiftAttachments) != 1 {
		t.Fatalf("attachments=%+v want one stack of item 37", result.GiftAttachments)
	}
	if balance := loadCeraShopAccountCera(t, ctx, repositories, "account-1"); balance != 120 {
		t.Fatalf("sender cera=%d want=120", balance)
	}
	inventory, found, err := repositories.Inventory.Load(ctx, "19")
	if err != nil || !found {
		t.Fatalf("load sender inventory found=%t err=%v", found, err)
	}
	if len(inventory.Slots) != 0 {
		t.Fatalf("sender inventory changed: %+v", inventory.Slots)
	}
}

// ★ 投递失败必须**退款**：结账已经提交（点券已扣），邮件投递不成功就不能让玩家白花钱。
// 这条覆盖的是整条赠送路径里最危险的补偿分支。
func TestHandleCurrentCeraShopGiftPurchaseRefundsWhenDeliveryFails(t *testing.T) {
	catalog := mustCurrentCeraShopTestCatalog(t)
	repositories := dnfrepomemory.NewMemoryGroup()
	// 邮件资产单元不可用 = 投递一定失败，但钱包/角色那半条链完全正常。
	repositories.MailboxAssets = nil
	ctx := context.Background()
	now := time.Now().UTC()
	if err := repositories.Account.Save(ctx, dnfrepo.AccountRecord{
		AccountID: "account-1",
		Metadata:  map[string]string{"account_cera": "200"},
		CreatedAt: now.Add(-30 * 24 * time.Hour),
	}); err != nil {
		t.Fatal(err)
	}
	if err := repositories.Character.Save(ctx, dnfrepo.CharacterRecord{
		CharacterID: "19", AccountID: "account-1", Name: "1111", Job: "0", Level: 90,
		Stats: map[string]int64{"grow_type": 1},
	}); err != nil {
		t.Fatal(err)
	}
	if err := repositories.Character.Save(ctx, dnfrepo.CharacterRecord{
		CharacterID: "22", AccountID: "account-2", Name: "9999", Job: "0", Level: 90,
		Stats: map[string]int64{"grow_type": 1},
	}); err != nil {
		t.Fatal(err)
	}
	if err := repositories.Inventory.Save(ctx, dnfrepo.InventoryRecord{CharacterID: "19", Slots: map[string]dnfrepo.ItemStack{}}); err != nil {
		t.Fatal(err)
	}
	seedCeraShopGiftQueryFriendship(t, repositories, "1111", "9999", now.Add(-30*24*time.Hour))

	conn := &bufferConn{}
	service := newCeraShopPremiumTestService(repositories, catalog)
	session := &gameSession{conn: conn, connID: "cera-gift-refund", selectedCharacterID: 19}
	if err := service.handleCurrentCeraShopPurchase(session, currentCeraShopGiftPurchaseBody("9999", 100050)); err != nil {
		t.Fatal(err)
	}
	reply, _ := splitGameServerUpperPacket(t, conn.write.Bytes())
	// 邮件写不进去 = 瞬时故障、钱已退回 ⇒ 回 20「与服务器连接不稳定，请稍候再试」。
	if len(reply.Body) != 22 || reply.Body[0] != 0 || reply.Body[1] != currentCeraShopPurchaseFailureCodeUnknown {
		t.Fatalf("failure body=%x want failure with code 0x%02x",
			reply.Body, currentCeraShopPurchaseFailureCodeUnknown)
	}
	if balance := loadCeraShopAccountCera(t, ctx, repositories, "account-1"); balance != 200 {
		t.Fatalf("sender cera=%d want=200 (a failed delivery must be refunded)", balance)
	}
	inventory, found, err := repositories.Inventory.Load(ctx, "19")
	if err != nil || !found {
		t.Fatalf("load sender inventory found=%t err=%v", found, err)
	}
	if len(inventory.Slots) != 0 {
		t.Fatalf("sender inventory changed on a failed gift: %+v", inventory.Slots)
	}
}

// 赠送方名字超过邮件摘要行的 30 字节预算时必须**拒绝投递并退款**：
// 放它进邮件会让收件人打开邮箱时整包构建失败（等于弄坏对方的邮箱）。
// GB18030 里一个汉字 2 字节，所以 16 个汉字 = 32 字节 > 30。
func TestHandleCurrentCeraShopGiftPurchaseRejectsOverlongSenderName(t *testing.T) {
	catalog := mustCurrentCeraShopTestCatalog(t)
	repositories := dnfrepomemory.NewMemoryGroup()
	ctx := context.Background()
	now := time.Now().UTC()
	longName := strings.Repeat("测", 16)
	if err := repositories.Account.Save(ctx, dnfrepo.AccountRecord{
		AccountID: "account-1",
		Metadata:  map[string]string{"account_cera": "200"},
		CreatedAt: now.Add(-30 * 24 * time.Hour),
	}); err != nil {
		t.Fatal(err)
	}
	if err := repositories.Character.Save(ctx, dnfrepo.CharacterRecord{
		CharacterID: "19", AccountID: "account-1", Name: longName, Job: "0", Level: 90,
		Stats: map[string]int64{"grow_type": 1},
	}); err != nil {
		t.Fatal(err)
	}
	if err := repositories.Character.Save(ctx, dnfrepo.CharacterRecord{
		CharacterID: "22", AccountID: "account-2", Name: "9999", Job: "0", Level: 90,
		Stats: map[string]int64{"grow_type": 1},
	}); err != nil {
		t.Fatal(err)
	}
	if err := repositories.Inventory.Save(ctx, dnfrepo.InventoryRecord{CharacterID: "19", Slots: map[string]dnfrepo.ItemStack{}}); err != nil {
		t.Fatal(err)
	}
	seedCeraShopGiftQueryFriendship(t, repositories, longName, "9999", now.Add(-30*24*time.Hour))

	conn := &bufferConn{}
	service := newCeraShopPremiumTestService(repositories, catalog)
	session := &gameSession{conn: conn, connID: "cera-gift-long-sender", selectedCharacterID: 19}
	if err := service.handleCurrentCeraShopPurchase(session, currentCeraShopGiftPurchaseBody("9999", 100050)); err != nil {
		t.Fatal(err)
	}
	reply, _ := splitGameServerUpperPacket(t, conn.write.Bytes())
	// 同上：投递不可用 ⇒ 20，且必须退款。
	if len(reply.Body) != 22 || reply.Body[0] != 0 || reply.Body[1] != currentCeraShopPurchaseFailureCodeUnknown {
		t.Fatalf("failure body=%x want failure with code 0x%02x",
			reply.Body, currentCeraShopPurchaseFailureCodeUnknown)
	}
	if balance := loadCeraShopAccountCera(t, ctx, repositories, "account-1"); balance != 200 {
		t.Fatalf("sender cera=%d want=200 (an overlong sender name must refund)", balance)
	}
	mailbox, found, err := repositories.Mailbox.Load(ctx, "22")
	if err != nil {
		t.Fatal(err)
	}
	if found && len(mailbox.Mails) != 0 {
		t.Fatalf("recipient got %d mails with an unencodable sender name", len(mailbox.Mails))
	}
}

// 点券不足是玩家最常撞到的那条路（本服所有账号点券都是 0）：赠送必须回
// 90CN 表里的 3「点券不足，请充值后再次尝试」，而不是兜底的 4「无法购买物品」，
// 也不能是 13「您无法使用赠送功能」—— 玩家缺钱时要看得出是缺钱。
func TestHandleCurrentCeraShopGiftPurchaseAnswersInsufficientCeraCode(t *testing.T) {
	catalog := mustCurrentCeraShopTestCatalog(t)
	repositories := dnfrepomemory.NewMemoryGroup()
	ctx := context.Background()
	now := time.Now().UTC()
	// 账号点券 0：结账一定在支付瀑布那一步失败。
	if err := repositories.Account.Save(ctx, dnfrepo.AccountRecord{
		AccountID: "account-1",
		CreatedAt: now.Add(-30 * 24 * time.Hour),
	}); err != nil {
		t.Fatal(err)
	}
	if err := repositories.Character.Save(ctx, dnfrepo.CharacterRecord{
		CharacterID: "19", AccountID: "account-1", Name: "1111", Job: "0", Level: 90,
		Stats: map[string]int64{"grow_type": 1},
	}); err != nil {
		t.Fatal(err)
	}
	if err := repositories.Character.Save(ctx, dnfrepo.CharacterRecord{
		CharacterID: "22", AccountID: "account-2", Name: "9999", Job: "0", Level: 90,
		Stats: map[string]int64{"grow_type": 1},
	}); err != nil {
		t.Fatal(err)
	}
	if err := repositories.Inventory.Save(ctx, dnfrepo.InventoryRecord{CharacterID: "19", Slots: map[string]dnfrepo.ItemStack{}}); err != nil {
		t.Fatal(err)
	}
	seedCeraShopGiftQueryFriendship(t, repositories, "1111", "9999", now.Add(-30*24*time.Hour))

	conn := &bufferConn{}
	service := newCeraShopPremiumTestService(repositories, catalog)
	session := &gameSession{conn: conn, connID: "cera-gift-no-cera", selectedCharacterID: 19}
	if err := service.handleCurrentCeraShopPurchase(session, currentCeraShopGiftPurchaseBody("9999", 100050)); err != nil {
		t.Fatal(err)
	}
	reply, _ := splitGameServerUpperPacket(t, conn.write.Bytes())
	if len(reply.Body) != 22 || reply.Body[0] != 0 ||
		reply.Body[1] != currentCeraShopPurchaseFailureCodeInsufficientCera {
		t.Fatalf("failure body=%x want failure with the insufficient-cera code 0x%02x",
			reply.Body, currentCeraShopPurchaseFailureCodeInsufficientCera)
	}
	if balance := loadCeraShopAccountCera(t, ctx, repositories, "account-1"); balance != 0 {
		t.Fatalf("sender cera=%d want=0", balance)
	}
	mailbox, found, err := repositories.Mailbox.Load(ctx, "22")
	if err != nil {
		t.Fatal(err)
	}
	if found && len(mailbox.Mails) != 0 {
		t.Fatalf("recipient got %d mails without paying", len(mailbox.Mails))
	}
}

// 90CN 商城回执码表：这些数字直接从本客户端字符串表 21969..21995 抄下来
// （见 cera_shop_protocol.go 的注释）。改动它们等于改玩家看到的文案，必须是有意的。
func TestCurrentCeraShopFailureCodeTableMatchesThe90CNClientStrings(t *testing.T) {
	for _, test := range []struct {
		name string
		code byte
		want byte
	}{
		// 实测规律：字符串id = 269 - code（见 cera_shop_protocol.go 顶部）。
		{"cannot buy (无法购买的物品)", currentCeraShopPurchaseFailureCodeCannotBuy, 16},
		{"wrong commodity", currentCeraShopPurchaseFailureCodeWrongCommodity, 15},
		{"inventory full", currentCeraShopPurchaseFailureCodeInventoryFull, 14},
		{"insufficient gold", currentCeraShopPurchaseFailureCodeInsufficientGold, 13},
		{"insufficient cera (实测确认)", currentCeraShopPurchaseFailureCodeInsufficientCera, 11},
		{"level too low", currentCeraShopPurchaseFailureCodeLevelTooLow, 10},
		{"unknown", currentCeraShopPurchaseFailureCodeUnknown, 4},
		{"gift not giftable", currentCeraShopPurchaseFailureCodeGiftNotGiftable, 16},
		{"gift amount cap", currentCeraShopPurchaseFailureCodeGiftAmountCap, 16},
		{"gift account age", currentCeraShopPurchaseFailureCodeGiftAccountTooNew, 16},
		{"gift forbidden", currentCeraShopPurchaseFailureCodeGiftForbidden, 16},
		{"limit fallback", currentCeraShopPurchaseLimitFailureCode, 16},
		{"gift unsupported alias", currentCeraShopPurchaseFailureCodeGiftUnsupported, 16},
	} {
		if test.code != test.want {
			t.Fatalf("%s code=%d want=%d (269-%d)", test.name, test.code, test.want, test.want)
		}
	}
	// 失败包的形状：u8 0 + u8 码 + 5×u32，长度固定 22。
	body := buildCurrentCeraShopPurchaseFailureBodyWithCode(currentCeraShopPurchaseFailureCodeInsufficientCera)
	if len(body) != 22 || body[0] != 0 || body[1] != currentCeraShopPurchaseFailureCodeInsufficientCera {
		t.Fatalf("failure body=%x want 00 %02x + 5x u32", body, currentCeraShopPurchaseFailureCodeInsufficientCera)
	}
	// 默认失败包 = 兜底码（无法购买的物品）。
	if body := buildCurrentCeraShopPurchaseFailureBody(); body[1] != currentCeraShopPurchaseFailureCodeCannotBuy {
		t.Fatalf("default failure code=%d want=%d", body[1], currentCeraShopPurchaseFailureCodeCannotBuy)
	}
}

// ★ 现场回归（2026-09-27 17:17）：玩家把收件人填成 `test`（角色 3，与赠送方不是好友），
// 服务端判定是对的，但回包只发了**一个字节** `a5`：客户端把结果码当标志位读，
// 非 0 ⇒ 走成功分支 ⇒ 去读根本没发过去的 名字/等级/职业 ⇒ 对话框画出 "Lv 12336"、
// 职业空白，玩家完全看不懂。
//
// 这条用例用**现场那一包的原始 body**（DSTR "test" + u8 1）钉住修复后的形状：
// 失败必须是 `00 a5`（标志位 0 + 结果码 0xA5「您与该用户还不是好友关系」）。
func TestCurrentCeraShopGiftQueryNotFriendReplyMatchesTheLiveIncident(t *testing.T) {
	repositories := testRepositoryGroup()
	seedCeraShopGiftQuerySender(t, repositories, 21, "dnf:1", "礼白666", "0", 30*24*time.Hour)
	seedCeraShopGiftQueryRecipient(t, repositories, dnfrepo.CharacterRecord{
		CharacterID: "3", AccountID: "dnf:2", Name: "test", Job: "11", Level: 90,
		Stats: map[string]int64{"grow_type": 17},
	})
	service := newCeraShopGiftQueryTestService(repositories)
	conn := &bufferConn{}
	session := &gameSession{conn: conn, connID: "gift-live-not-friend", selectedCharacterID: 21}

	if err := service.handleCurrentCeraShopGiftQuery(session, giftQueryRequestBody([]byte("test"), 1)); err != nil {
		t.Fatalf("handle gift recipient query: %v", err)
	}
	packet, rest := splitGameServerUpperPacket(t, conn.write.Bytes())
	if len(rest) != 0 {
		t.Fatalf("trailing bytes after the ack: %d", len(rest))
	}
	if got := hex.EncodeToString(packet.Body); got != "00a5" {
		t.Fatalf("failure body=%s want=00a5 (flag 0 + code 0xA5): a single byte makes the client read a success payload", got)
	}
}

// ★ 每个赠送错误都必须有一个**明确的客户端码**，否则会掉进 default 变成
// 「您无法使用赠送功能」，玩家看到的提示和真实原因对不上。
// 职业不符当初就丢过一次（判定返回 10，回给客户端却成了 13）。
func TestCurrentCeraShopGiftErrorCodesAreExplicit(t *testing.T) {
	for _, test := range []struct {
		name string
		err  error
		want byte
	}{
		{"no error", nil, 0},
		// op64 只能查 253..265 那张通用购买错误表（`字符串id = 269 - code`），
		// 里面没有赠送专用文案 ⇒ 除"每日额度用完"外一律兜底 16（无法购买的物品）。
		{"not giftable", errCurrentCeraShopGiftNotGiftable, 16},
		{"job mismatch", errCurrentCeraShopGiftJobMismatch, 16},
		{"account too new", errCurrentCeraShopGiftAccountTooNew, 16},
		{"daily gift allowance", errCurrentCeraShopGiftDailyLimit, 16},
		{"insufficient cera", dnfcerashop.ErrWalletInsufficient, 11},
		{"insufficient cera (cera only)", dnfcerashop.ErrCeraInsufficient, 11},
		{"self", errCurrentCeraShopGiftSelf, 16},
		{"not friend", errCurrentCeraShopGiftNotFriend, 16},
		{"friend too new", errCurrentCeraShopGiftFriendTooNew, 16},
		{"mail unavailable", errCurrentCeraShopGiftMailUnavailable, 4},
		{"recipient gone", errCurrentCeraShopGiftRecipientGone, 16},
		{"sender missing", errCurrentCeraShopGiftSenderMissing, 16},
	} {
		if got := currentCeraShopGiftFailureCode(test.err); got != test.want {
			t.Fatalf("%s code=%d want=%d", test.name, got, test.want)
		}
	}
}

// ★ D6 回归：**一个方向是老数据，不能把另一个方向的年轻边一起放行**。
//
// A→B 是迁移前留下的 NULL 老行、B 昨天才把 A 加回来 —— "互为好友"其实只有一天。
// 早先的实现是"只要有一条边老，整对就按已满足处理"，于是这种情况能立刻赠送；
// 老数据只应该豁免**它自己那条边**。
func TestCurrentCeraShopGiftQueryRulesDoNotLetALegacyEdgeGrandfatherANewOne(t *testing.T) {
	repositories := testRepositoryGroup()
	seedCeraShopGiftQuerySender(t, repositories, 21, "dnf:1", "1111", "0", 30*24*time.Hour)
	seedCeraShopGiftQueryRecipient(t, repositories, dnfrepo.CharacterRecord{
		CharacterID: "22", AccountID: "dnf:2", Name: "9999", Job: "11", Level: 87,
		Stats: map[string]int64{"grow_type": 3},
	})
	ctx := context.Background()
	now := time.Now().UTC()
	// 1111→9999 是老数据（零值 = MySQL 里的 NULL），9999→1111 是昨天新建的。
	if err := repositories.UnitedFriend.Save(ctx, dnfrepo.UnitedFriendRecord{
		OwnerName: "1111", FriendNames: []string{"9999"}, UpdatedAt: now,
		FriendSince: map[string]time.Time{"9999": {}},
	}); err != nil {
		t.Fatal(err)
	}
	if err := repositories.UnitedFriend.Save(ctx, dnfrepo.UnitedFriendRecord{
		OwnerName: "9999", FriendNames: []string{"1111"}, UpdatedAt: now,
		FriendSince: map[string]time.Time{"1111": now.Add(-24 * time.Hour)},
	}); err != nil {
		t.Fatal(err)
	}
	service := newCeraShopGiftQueryTestService(repositories)
	session := &gameSession{connID: "gift-rule-legacy-one-way", selectedCharacterID: 21}
	if code := giftQueryReplyCode(t, service, session, currentCeraShopGiftQueryLiveCaptureBody()); code != currentCeraShopGiftQueryCodeFriendTooNew {
		t.Fatalf("legacy+new friendship code=%#x want=%#x (the young edge must still gate)",
			code, currentCeraShopGiftQueryCodeFriendTooNew)
	}
}

func newCeraShopGiftQueryTestService(repositories dnfrepo.Group) *Service {
	return &Service{
		options:            options{gameUpperHeader: gameUpperHeaderChannel13, gameUpperBodyCodec: gameUpperBodyCodecPlain},
		repositoryProvider: func() (dnfrepo.Group, bool) { return repositories, true },
	}
}

func seedCeraShopGiftQueryRecipient(t *testing.T, repositories dnfrepo.Group, record dnfrepo.CharacterRecord) {
	t.Helper()
	if err := repositories.Character.Save(context.Background(), record); err != nil {
		t.Fatal(err)
	}
}

// seedCeraShopGiftQuerySender 造出"当前登录角色"（赠送方 = 付款方）以及它的账号。
// accountAge 为 0 时不写账号记录（= 账号年龄不可知，按"不拦"处理）。
func seedCeraShopGiftQuerySender(
	t *testing.T,
	repositories dnfrepo.Group,
	characterID uint16,
	accountID string,
	name string,
	job string,
	accountAge time.Duration,
) {
	t.Helper()
	ctx := context.Background()
	if err := repositories.Character.Save(ctx, dnfrepo.CharacterRecord{
		CharacterID: strconv.FormatUint(uint64(characterID), 10),
		AccountID:   accountID,
		Name:        name,
		Job:         job,
		Level:       90,
		Stats:       map[string]int64{"grow_type": 1},
	}); err != nil {
		t.Fatal(err)
	}
	if accountAge == 0 {
		return
	}
	if err := repositories.Account.Save(ctx, dnfrepo.AccountRecord{
		AccountID: accountID,
		CreatedAt: time.Now().UTC().Add(-accountAge),
	}); err != nil {
		t.Fatal(err)
	}
}

// seedCeraShopGiftQueryFriendship 写**两个方向**的好友边（本服的好友边是单向存储的，
// "互为好友"必须两条都在）。since 为零值 = 老数据（friend_since 字段上线之前的关系）。
func seedCeraShopGiftQueryFriendship(t *testing.T, repositories dnfrepo.Group, left, right string, since time.Time) {
	t.Helper()
	ctx := context.Background()
	for _, pair := range [][2]string{{left, right}, {right, left}} {
		record := dnfrepo.UnitedFriendRecord{
			OwnerName:   pair[0],
			FriendNames: []string{pair[1]},
			UpdatedAt:   time.Now().UTC(),
		}
		if !since.IsZero() {
			record.FriendSince = map[string]time.Time{pair[1]: since}
		}
		if err := repositories.UnitedFriend.Save(ctx, record); err != nil {
			t.Fatal(err)
		}
	}
}

func giftQueryRequestBody(name []byte, kind byte) []byte {
	body := make([]byte, 0, 5+len(name))
	var scratch [4]byte
	binary.LittleEndian.PutUint32(scratch[:], uint32(len(name)))
	body = append(body, scratch[:]...)
	body = append(body, name...)
	body = append(body, kind)
	return body
}

func TestHandleCurrentCeraShopGiftQueryAnswersKnownRecipient(t *testing.T) {
	repositories := testRepositoryGroup()
	// 赠送方 = 当前登录角色；收件人 9999 与它是**互为好友满 16 天**的关系。
	seedCeraShopGiftQuerySender(t, repositories, 21, "dnf:1", "1111", "0", 30*24*time.Hour)
	seedCeraShopGiftQueryRecipient(t, repositories, dnfrepo.CharacterRecord{
		CharacterID: "22",
		AccountID:   "dnf:2",
		Name:        "9999",
		Job:         "11",
		Level:       87,
		Stats:       map[string]int64{"grow_type": 3},
	})
	seedCeraShopGiftQueryFriendship(t, repositories, "1111", "9999", time.Now().UTC().Add(-16*24*time.Hour))
	service := newCeraShopGiftQueryTestService(repositories)
	conn := &bufferConn{}
	session := &gameSession{conn: conn, connID: "gift-query-ack", selectedCharacterID: 21}

	if err := service.handleCurrentCeraShopGiftQuery(session, currentCeraShopGiftQueryLiveCaptureBody()); err != nil {
		t.Fatalf("handle gift recipient query: %v", err)
	}
	packet, rest := splitGameServerUpperPacket(t, conn.write.Bytes())
	if len(rest) != 0 {
		t.Fatalf("trailing bytes after the ack: %d", len(rest))
	}
	if packet.Header.Classification != dnfproto.DefaultChannelClassification {
		t.Fatalf("classification=%d want=%d (class-1 upper)", packet.Header.Classification, dnfproto.DefaultChannelClassification)
	}
	if packet.Header.MsgID != uint16(dnfenum.CmdPacketQueryCharacInfo) {
		t.Fatalf("msg id=%d want=%d", packet.Header.MsgID, dnfenum.CmdPacketQueryCharacInfo)
	}
	reader := &clientGiftQueryReplyReader{t: t, body: packet.Body}
	if flag := reader.u8(); flag != 1 {
		t.Fatalf("success flag=%d want=1", flag)
	}
	if name := reader.dstr(currentCeraShopGiftQueryMaxNameBytes); !bytes.Equal(name, []byte("9999")) {
		t.Fatalf("echoed name=%x want=39393939 (must be the bytes the client sent)", name)
	}
	if level := reader.u16(); level != 87 {
		t.Fatalf("level=%d want=87", level)
	}
	if job := reader.u8(); job != 11 {
		t.Fatalf("job=%d want=11", job)
	}
	if grow := reader.u8(); grow != 3 {
		t.Fatalf("grow=%d want=3", grow)
	}
}

// 查无此人必须回到"客户端会弹提示"的那条路，而不是静默 —— 这正是本次线上现象的成因。
// 回包里的 0x15 就是客户端「该ID不存在」那句文案的码。
func TestHandleCurrentCeraShopGiftQueryAnswersNotFoundWithResultCode(t *testing.T) {
	repositories := testRepositoryGroup()
	seedCeraShopGiftQuerySender(t, repositories, 21, "dnf:1", "1111", "0", 30*24*time.Hour)
	service := newCeraShopGiftQueryTestService(repositories)
	conn := &bufferConn{}
	session := &gameSession{conn: conn, connID: "gift-query-missing", selectedCharacterID: 21}

	if err := service.handleCurrentCeraShopGiftQuery(session, currentCeraShopGiftQueryLiveCaptureBody()); err != nil {
		t.Fatalf("handle gift recipient query: %v", err)
	}
	packet, rest := splitGameServerUpperPacket(t, conn.write.Bytes())
	if len(rest) != 0 {
		t.Fatalf("trailing bytes after the failure ack: %d", len(rest))
	}
	if packet.Header.MsgID != uint16(dnfenum.CmdPacketQueryCharacInfo) ||
		packet.Header.Classification != dnfproto.DefaultChannelClassification {
		t.Fatalf("failure header=%+v want class1/op136", packet.Header)
	}
	if !bytes.Equal(packet.Body, []byte{0, currentCeraShopGiftQueryCodeNotFound}) {
		t.Fatalf("failure body=%x want=00%02x", packet.Body, currentCeraShopGiftQueryCodeNotFound)
	}
}

// 名字超过客户端读包器的 0x14 字节预算时必须回失败：回成功会让客户端游标停在名字
// 中间，后面 7 个字段全部错位（比"没反应"更糟——会往对话框灌垃圾值）。
func TestHandleCurrentCeraShopGiftQueryRefusesNameBeyondReaderBudget(t *testing.T) {
	repositories := testRepositoryGroup()
	seedCeraShopGiftQuerySender(t, repositories, 21, "dnf:1", "1111", "0", 30*24*time.Hour)
	long := bytes.Repeat([]byte{'a'}, currentCeraShopGiftQueryMaxNameBytes)
	seedCeraShopGiftQueryRecipient(t, repositories, dnfrepo.CharacterRecord{
		CharacterID: "23",
		AccountID:   "dnf:2",
		Name:        string(long),
		Job:         "11",
		Level:       87,
		Stats:       map[string]int64{"grow_type": 3},
	})
	service := newCeraShopGiftQueryTestService(repositories)
	conn := &bufferConn{}
	session := &gameSession{conn: conn, connID: "gift-query-long-name", selectedCharacterID: 21}

	if err := service.handleCurrentCeraShopGiftQuery(session, giftQueryRequestBody(long, 1)); err != nil {
		t.Fatalf("handle long-name query: %v", err)
	}
	packet, _ := splitGameServerUpperPacket(t, conn.write.Bytes())
	if !bytes.Equal(packet.Body, []byte{0, currentCeraShopGiftQueryCodeNotFound}) {
		t.Fatalf("long-name body=%x want=00%02x (a success echo would desync the client reader)",
			packet.Body, currentCeraShopGiftQueryCodeNotFound)
	}
}

// buildLegacyGiftQueryFrame 复刻现场那一包的 13 字节 legacy 头：
// cmd | u16 type | u32 总长 | u32 checksum | u16 seq | u8 0，后面跟 body。
// （dnfproto.ParseLegacyGamePacket 不校验 checksum，只校验总长，所以这里填 0。）
func buildLegacyGiftQueryFrame(body []byte) []byte {
	frame := make([]byte, dnfproto.LegacyGameHeaderSize+len(body))
	frame[0] = byte(dnfenum.GameCmdCommand)
	binary.LittleEndian.PutUint16(frame[1:3], uint16(dnfenum.CmdPacketQueryCharacInfo))
	binary.LittleEndian.PutUint32(frame[3:7], uint32(len(frame)))
	binary.LittleEndian.PutUint16(frame[11:13], 1)
	copy(frame[dnfproto.LegacyGameHeaderSize:], body)
	return frame
}

// 用**真 legacy 帧**（13 字节头 + 现场 body）走 handleLegacyGamePacket，
// 而不是直接调处理器：这才证明
//  1. gameplay module 那条路由对真实 legacy 流量确实被命中；
//  2. normalizeLegacyGameBody 把 body 逐字节原样交下来（136 没登记 normalizer）；
//  3. 回包真的以 class1/op136 发出去，且字节与规格一致。
//
// 直接调处理器是证明不了前两条的。
func TestHandleLegacyGiftQueryFrameReachesRegisteredRoute(t *testing.T) {
	frame := buildLegacyGiftQueryFrame(currentCeraShopGiftQueryLiveCaptureBody())
	if len(frame) != dnfproto.LegacyGameHeaderSize+len(currentCeraShopGiftQueryLiveCaptureBody()) {
		t.Fatalf("frame length=%d", len(frame))
	}
	// 与现场抓到的那一包同形（body 逐字节一致）。
	if got := hex.EncodeToString(frame[dnfproto.LegacyGameHeaderSize:]); got != "040000003939393901" {
		t.Fatalf("frame body=%s want=040000003939393901", got)
	}

	repositories := testRepositoryGroup()
	seedCeraShopGiftQuerySender(t, repositories, 21, "dnf:1", "1111", "0", 30*24*time.Hour)
	seedCeraShopGiftQueryRecipient(t, repositories, dnfrepo.CharacterRecord{
		CharacterID: "22",
		AccountID:   "dnf:2",
		Name:        "9999",
		Job:         "11",
		Level:       87,
		Stats:       map[string]int64{"grow_type": 3},
	})
	seedCeraShopGiftQueryFriendship(t, repositories, "1111", "9999", time.Now().UTC().Add(-16*24*time.Hour))
	service := newCeraShopGiftQueryTestService(repositories)
	conn := &bufferConn{}
	session := &gameSession{conn: conn, connID: "gift-query-legacy-frame", selectedCharacterID: 21}

	if err := service.handleLegacyGamePacket(session, frame); err != nil {
		t.Fatalf("handle legacy op136 frame: %v", err)
	}
	if conn.write.Len() == 0 {
		t.Fatal("no reply: the legacy route was not reached (this is exactly the live freeze)")
	}
	packet, rest := splitGameServerUpperPacket(t, conn.write.Bytes())
	if len(rest) != 0 {
		t.Fatalf("trailing bytes after the ack: %d", len(rest))
	}
	if packet.Header.Classification != dnfproto.DefaultChannelClassification ||
		packet.Header.MsgID != uint16(dnfenum.CmdPacketQueryCharacInfo) {
		t.Fatalf("reply header=%+v want class1/op136", packet.Header)
	}
	// 金标准：改这里必须同时改文档里的规格表。
	// 01 | DSTR("9999") | u16 57=87级 | u8 0b=job11 | u8 03=grow | u8 00 | u8 01 | u8 00 | u8 00
	const wantHex = "01040000003939393957000b0300010000"
	if got := hex.EncodeToString(packet.Body); got != wantHex {
		t.Fatalf("reply body=%s want=%s", got, wantHex)
	}
}

// ---------------------------------------------------------------------------
// 赠送**结账**形态（op64）：2026-09-27 16:14:02 现场抓包（body_len=28）
//
//	00 01 | 04000000 39393939 | 00 | 01 00 00 ff 00 c6a801 00 ...
//	^^ ^^   ^^^^^^^^ ^^^^^^^^   ^^   ^^^^^^^^^^^^^^^^^^^^^^^^^^^^
//	|  |    u32 名字长  "9999"   |    普通 body 从第 3 字节起（商品 108742）
//	|  赠送位 = 1（下标 1！）     赠送类型 u8
//	普通 body 的头两个字节
//
// 关键：**形态位在下标 1**，不是下标 0。客户端 CashShopBuyWindow::sendBuyPacket
// （RVA 0x02E7471A）先写 [edi+0x282C]、再写 [edi+0x2828]，只有后者 == 1 才追加
// WriteDstr(收件人) + WriteU8(类型)。普通购买那一包 body[1] = 0（现场 3 包全是 0）。
//
// 我第一次是照着人工抄的 hex 写成"形态位在下标 0"的，测试直接把它顶了回来：
// 那样这一包会被当普通购买，count 读成 4、commodity 读成 3750201，最后回一个
// 跟真实原因无关的失败码 —— 正是现场看到的现象。
// ---------------------------------------------------------------------------

// currentCeraShopGiftPurchaseLiveCaptureBody 是从 packet_log.txt 里逐字节抄下来的
// 现场 body（28 字节）。
func currentCeraShopGiftPurchaseLiveCaptureBody() []byte {
	return []byte{
		0x00, 0x01,
		0x04, 0x00, 0x00, 0x00, '9', '9', '9', '9',
		0x00,
		0x01, 0x00, 0x00, 0xff, 0x00, 0xc6, 0xa8, 0x01, 0x00,
		0x00, 0x00, 0x00, 0x00, 0x00, 0x00, 0x00, 0x00,
	}
}

// currentCeraShopGiftPurchaseBody 按同一套布局拼一包赠送 body：
// 把普通 cart body 的第 2 个字节置 1，并在其后插入 DSTR(收件人) + u8 类型。
func currentCeraShopGiftPurchaseBody(recipient string, commodityID uint32) []byte {
	cart := currentCeraShopTestRequestBody(commodityID)
	raw := []byte(recipient)
	body := []byte{cart[0], 0x01}
	var scratch [4]byte
	binary.LittleEndian.PutUint32(scratch[:], uint32(len(raw)))
	body = append(body, scratch[:]...)
	body = append(body, raw...)
	body = append(body, 0x00) // 赠送类型
	return append(body, cart[2:]...)
}

func TestParseCurrentCeraShopGiftPurchaseMatchesLiveCapture(t *testing.T) {
	body := currentCeraShopGiftPurchaseLiveCaptureBody()
	if len(body) != 28 {
		t.Fatalf("live gift body len=%d want=28", len(body))
	}
	request, err := parseCurrentCeraShopPurchaseRequest(body)
	if err != nil {
		t.Fatalf("live gift capture rejected: %v", err)
	}
	if request.GiftRecipientName != "9999" {
		t.Fatalf("recipient=%q want=9999", request.GiftRecipientName)
	}
	if !bytes.Equal(request.GiftRecipientRaw, []byte("9999")) {
		t.Fatalf("recipient raw=%x", request.GiftRecipientRaw)
	}
	if request.GiftKind != 0 {
		t.Fatalf("gift kind=%d want=0 (live capture)", request.GiftKind)
	}
	// 还原后的普通 body 必须与"普通购买"逐字节同形：行数 1、点券付款、商品号 108742。
	if len(request.Items) != 1 || request.Items[0].CommodityID != 108742 {
		t.Fatalf("items=%+v want one row with commodity 108742", request.Items)
	}
	if request.PaymentMode != currentCeraShopPaymentModeCera {
		t.Fatalf("payment mode=%d want cera", request.PaymentMode)
	}
}

// 普通购买（现场 3 包）必须继续解析成"不是赠送"：形态位只能是 0，前缀绝不能被误剥。
func TestParseCurrentCeraShopNormalPurchaseStaysUngifted(t *testing.T) {
	for _, body := range [][]byte{
		{0x00, 0x00, 0x01, 0x00, 0x00, 0x00, 0xff, 0x00, 0xa4, 0x86, 0x01, 0x00, 0x00, 0x00, 0x00, 0x00, 0x00, 0x00, 0x00},
		{0x00, 0x00, 0x01, 0x00, 0x00, 0x00, 0xff, 0x00, 0xa0, 0x86, 0x01, 0x00, 0x00, 0x00, 0x00, 0x00, 0x00, 0x00, 0x00},
		{0x00, 0x00, 0x01, 0x00, 0x00, 0xff, 0x00, 0x12, 0x92, 0x01, 0x00, 0x00, 0x00, 0x00, 0x00, 0x00, 0x00, 0x00, 0x00},
	} {
		request, err := parseCurrentCeraShopPurchaseRequest(body)
		if err != nil {
			t.Fatalf("normal body %x rejected: %v", body, err)
		}
		if request.GiftRecipientName != "" || len(request.GiftRecipientRaw) != 0 {
			t.Fatalf("normal body %x was read as a gift: %+v", body, request)
		}
	}
}

// 畸形赠送前缀必须被**显式**拒绝：落进普通购买解析器就会变成一次看不懂的购买失败
// （现场 16:14 那次就是这样），甚至可能被当成真购买。
func TestParseCurrentCeraShopGiftPrefixRejectsMalformed(t *testing.T) {
	cart := currentCeraShopTestRequestBody(100050)
	// 构造 [cart0][1][middle...][cart 第 3 字节起]，middle = u32 长度 + 名字 + 类型。
	prefixed := func(middle ...byte) []byte {
		body := []byte{cart[0], 0x01}
		body = append(body, middle...)
		return append(body, cart[2:]...)
	}
	cases := []struct {
		name string
		body []byte
	}{
		{"flag set but no length field", []byte{0x00, 0x01, 0x04, 0x00}},
		{"zero name length", prefixed(0x00, 0x00, 0x00, 0x00, 0x00)},
		{"name length past end", prefixed(0x40, 0x00, 0x00, 0x00)},
		{"name length eats the cart", prefixed(0x30, 0x00, 0x00, 0x00)},
		{"embedded nul in name", prefixed(0x03, 0x00, 0x00, 0x00, 'a', 0x00, 'b', 0x00)},
		{"name is whitespace only", prefixed(0x01, 0x00, 0x00, 0x00, 0x20, 0x00)},
		// 注意：**没有**"名字不是合法 GB18030"这条用例 —— x/text 的 GB18030 解码器很宽松
		// （0x80/0xFF 单字节都能解出来），所以那个分支实际上到不了。乱名字会走
		// "查不到这个角色" 的正常路径，那条路径有独立用例覆盖。
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if _, err := parseCurrentCeraShopPurchaseRequest(tc.body); err == nil {
				t.Fatalf("body %x was accepted", tc.body)
			}
		})
	}
}

// ★ 安全红线：赠送形态**绝不能**被当成普通购买执行。
// 如果这条挂了，玩家点"赠送"就会把商品买给自己 —— 那是真正的经济事故。
//
// 这里的赠送方是个"什么都判定不出来"的角色（没有名字、没有好友、没有账号年龄），
// 所以判定必须在**扣钱之前**失败：点券一分不能动，背包一个格子不能变。
func TestHandleCurrentCeraShopGiftPurchaseIsRejectedWithoutTouchingTheBuyer(t *testing.T) {
	catalog := mustCurrentCeraShopTestCatalog(t)
	repositories := dnfrepomemory.NewMemoryGroup()
	ctx := context.Background()
	seedCeraShopAccountCera(t, ctx, repositories, "account-1", 200)
	if err := repositories.Character.Save(ctx, dnfrepo.CharacterRecord{
		CharacterID: "19",
		AccountID:   "account-1",
		Stats:       map[string]int64{"cera": 200},
	}); err != nil {
		t.Fatal(err)
	}
	if err := repositories.Inventory.Save(ctx, dnfrepo.InventoryRecord{CharacterID: "19", Slots: map[string]dnfrepo.ItemStack{}}); err != nil {
		t.Fatal(err)
	}
	conn := &bufferConn{}
	service := newCeraShopPremiumTestService(repositories, catalog)
	session := &gameSession{conn: conn, connID: "cera-gift-purchase", selectedCharacterID: 19}

	// 100050 是夹具里最便宜的商品（80 点券），余额 200 足够真买下来 ——
	// 所以"余额没动"这件事才有意义。
	if err := service.handleCurrentCeraShopPurchase(session, currentCeraShopGiftPurchaseBody("9999", 100050)); err != nil {
		t.Fatal(err)
	}
	reply, rest := splitGameServerUpperPacket(t, conn.write.Bytes())
	if len(rest) != 0 {
		t.Fatalf("trailing bytes after the failure ack: %d", len(rest))
	}
	if reply.Header.Classification != dnfproto.DefaultChannelClassification ||
		reply.Header.MsgID != uint16(dnfenum.CmdPacketBuyCerashopItem) {
		t.Fatalf("reply header=%+v want class1/op64", reply.Header)
	}
	// u8 0 = 失败；u8 0x0d = 「您无法使用赠送功能」（90CN 文案表 21969+13）。
	// 赠送方连名字都读不出来 ⇒ 判定在扣钱之前失败，回的是"你不能用赠送功能"。
	if len(reply.Body) != 22 || reply.Body[0] != 0 || reply.Body[1] != currentCeraShopPurchaseFailureCodeGiftForbidden {
		t.Fatalf("failure body=%x want failure with code 0x%02x",
			reply.Body, currentCeraShopPurchaseFailureCodeGiftForbidden)
	}
	if balance := loadCeraShopAccountCera(t, ctx, repositories, "account-1"); balance != 200 {
		t.Fatalf("buyer cera changed to %d: the gift was charged (%d)", balance, balance)
	}
	inventory, found, err := repositories.Inventory.Load(ctx, "19")
	if err != nil || !found {
		t.Fatalf("load inventory found=%t err=%v", found, err)
	}
	if len(inventory.Slots) != 0 {
		t.Fatalf("buyer inventory changed: %+v (the gift was delivered to the buyer)", inventory.Slots)
	}
	// 判定失败时**不能**发出邮件：给"没付钱的人"发礼物是更严重的事故。
	mailbox, found, err := repositories.Mailbox.Load(ctx, "22")
	if err != nil {
		t.Fatal(err)
	}
	if found && len(mailbox.Mails) != 0 {
		t.Fatalf("recipient mailbox changed on a rejected gift: %+v", mailbox.Mails)
	}
}

// 路由必须落在 cera-shop 模块的 **legacy** 表上：现场只抓到 legacy 一条传输，
// upper 没有证据就不登记（没登记时行为与改动前逐字节一致）。
func TestCeraShopGiftQueryRouteIsLegacyOnly(t *testing.T) {
	route, ok := currentGameplayModules.legacyHandlers[uint16(dnfenum.CmdPacketQueryCharacInfo)]
	if !ok {
		t.Fatal("op136 has no legacy gameplay route")
	}
	if route.module != "cera-shop" {
		t.Fatalf("op136 legacy owner=%q want=cera-shop", route.module)
	}
	if _, upperRegistered := currentGameplayModules.upperHandlers[uint16(dnfenum.CmdPacketQueryCharacInfo)]; upperRegistered {
		t.Fatal("op136 upper route registered without a capture proving class-1 arrival")
	}
	// 邮件那条同族命令（op324）必须仍然归 mail 模块，不能被这次改动抢走。
	mailboxRoute, ok := currentGameplayModules.legacyHandlers[uint16(dnfenum.CmdPacketQueryCharacInfoMailbox)]
	if ok {
		t.Fatalf("op324 unexpectedly routed by the gameplay registry to %q", mailboxRoute.module)
	}
}

// ---------------------------------------------------------------------------
// 赠送业务规则（2026-09-27 用户确认的口径）：
//   - 双方必须互为好友满 7 天；
//   - 赠送方账号注册满 7 天；
//   - 双方必须在同一个大区；
//   - 时装/礼包：双方基础职业必须相同。
//
// 每个用例都用**真包**走 handleCurrentCeraShopGiftQuery，断言的是回包里那个结果码
// 字节 —— 也就是客户端拿去查文案表的值。只测内部函数是证明不了"客户端会弹出对应
// 提示"的。
// ---------------------------------------------------------------------------

// giftQueryReplyCode 跑一次收件人查询，返回**失败结果码**（成功时返回 0）。
//
// 回包形状：`u8 标志位 | 失败时 u8 结果码 | 成功时 8 个字段`。这里顺手把形状也断言掉 ——
// 只读 body[0] 会把"标志位 0"当成成功，正是现场事故的读法。
func giftQueryReplyCode(t *testing.T, service *Service, session *gameSession, body []byte) byte {
	t.Helper()
	conn := &bufferConn{}
	session.conn = conn
	if err := service.handleCurrentCeraShopGiftQuery(session, body); err != nil {
		t.Fatalf("handle gift recipient query: %v", err)
	}
	packet, rest := splitGameServerUpperPacket(t, conn.write.Bytes())
	if len(rest) != 0 {
		t.Fatalf("trailing bytes after the ack: %d", len(rest))
	}
	if packet.Header.MsgID != uint16(dnfenum.CmdPacketQueryCharacInfo) {
		t.Fatalf("reply msg id=%d want=%d", packet.Header.MsgID, dnfenum.CmdPacketQueryCharacInfo)
	}
	if len(packet.Body) == 0 {
		t.Fatal("empty reply body")
	}
	switch packet.Body[0] {
	case 1:
		return 0
	case 0:
		if len(packet.Body) < 2 {
			t.Fatalf("failure body=%x is missing the result code byte", packet.Body)
		}
		return packet.Body[1]
	default:
		t.Fatalf("reply flag=%#x want 0 (failure) or 1 (success)", packet.Body[0])
		return 0
	}
}

func TestCurrentCeraShopGiftQueryRulesRejectSelfRecipient(t *testing.T) {
	repositories := testRepositoryGroup()
	seedCeraShopGiftQuerySender(t, repositories, 21, "dnf:1", "1111", "0", 30*24*time.Hour)
	service := newCeraShopGiftQueryTestService(repositories)
	session := &gameSession{connID: "gift-rule-self", selectedCharacterID: 21}
	if code := giftQueryReplyCode(t, service, session, giftQueryRequestBody([]byte("1111"), 1)); code != currentCeraShopGiftQueryCodeSelf {
		t.Fatalf("self gift code=%#x want=%#x", code, currentCeraShopGiftQueryCodeSelf)
	}
}

func TestCurrentCeraShopGiftQueryRulesRejectNonFriend(t *testing.T) {
	repositories := testRepositoryGroup()
	seedCeraShopGiftQuerySender(t, repositories, 21, "dnf:1", "1111", "0", 30*24*time.Hour)
	seedCeraShopGiftQueryRecipient(t, repositories, dnfrepo.CharacterRecord{
		CharacterID: "22", AccountID: "dnf:2", Name: "9999", Job: "11", Level: 87,
		Stats: map[string]int64{"grow_type": 3},
	})
	service := newCeraShopGiftQueryTestService(repositories)
	session := &gameSession{connID: "gift-rule-not-friend", selectedCharacterID: 21}
	if code := giftQueryReplyCode(t, service, session, currentCeraShopGiftQueryLiveCaptureBody()); code != currentCeraShopGiftQueryCodeNotFriend {
		t.Fatalf("non-friend code=%#x want=%#x", code, currentCeraShopGiftQueryCodeNotFriend)
	}
}

// 单向边不算好友：仓库里存的是单向关系，"我加了他"不等于"我们是好友"。
func TestCurrentCeraShopGiftQueryRulesRequireBothFriendEdges(t *testing.T) {
	repositories := testRepositoryGroup()
	seedCeraShopGiftQuerySender(t, repositories, 21, "dnf:1", "1111", "0", 30*24*time.Hour)
	seedCeraShopGiftQueryRecipient(t, repositories, dnfrepo.CharacterRecord{
		CharacterID: "22", AccountID: "dnf:2", Name: "9999", Job: "11", Level: 87,
		Stats: map[string]int64{"grow_type": 3},
	})
	ctx := context.Background()
	if err := repositories.UnitedFriend.Save(ctx, dnfrepo.UnitedFriendRecord{
		OwnerName:   "1111",
		FriendNames: []string{"9999"},
		UpdatedAt:   time.Now().UTC(),
		FriendSince: map[string]time.Time{"9999": time.Now().UTC().Add(-30 * 24 * time.Hour)},
	}); err != nil {
		t.Fatal(err)
	}
	service := newCeraShopGiftQueryTestService(repositories)
	session := &gameSession{connID: "gift-rule-one-way", selectedCharacterID: 21}
	if code := giftQueryReplyCode(t, service, session, currentCeraShopGiftQueryLiveCaptureBody()); code != currentCeraShopGiftQueryCodeNotFriend {
		t.Fatalf("one-way friend code=%#x want=%#x", code, currentCeraShopGiftQueryCodeNotFriend)
	}
}

func TestCurrentCeraShopGiftQueryRulesRejectFriendYoungerThanSevenDays(t *testing.T) {
	repositories := testRepositoryGroup()
	seedCeraShopGiftQuerySender(t, repositories, 21, "dnf:1", "1111", "0", 30*24*time.Hour)
	seedCeraShopGiftQueryRecipient(t, repositories, dnfrepo.CharacterRecord{
		CharacterID: "22", AccountID: "dnf:2", Name: "9999", Job: "11", Level: 87,
		Stats: map[string]int64{"grow_type": 3},
	})
	seedCeraShopGiftQueryFriendship(t, repositories, "1111", "9999", time.Now().UTC().Add(-6*24*time.Hour))
	service := newCeraShopGiftQueryTestService(repositories)
	session := &gameSession{connID: "gift-rule-friend-young", selectedCharacterID: 21}
	if code := giftQueryReplyCode(t, service, session, currentCeraShopGiftQueryLiveCaptureBody()); code != currentCeraShopGiftQueryCodeFriendTooNew {
		t.Fatalf("young friendship code=%#x want=%#x", code, currentCeraShopGiftQueryCodeFriendTooNew)
	}
}

// 方向取**较晚**的那条：A→B 三十天、B→A 一天，"互为好友"只有一天。
func TestCurrentCeraShopGiftQueryRulesUseTheLaterFriendEdge(t *testing.T) {
	repositories := testRepositoryGroup()
	seedCeraShopGiftQuerySender(t, repositories, 21, "dnf:1", "1111", "0", 30*24*time.Hour)
	seedCeraShopGiftQueryRecipient(t, repositories, dnfrepo.CharacterRecord{
		CharacterID: "22", AccountID: "dnf:2", Name: "9999", Job: "11", Level: 87,
		Stats: map[string]int64{"grow_type": 3},
	})
	ctx := context.Background()
	now := time.Now().UTC()
	for _, edge := range []dnfrepo.UnitedFriendRecord{
		{OwnerName: "1111", FriendNames: []string{"9999"}, UpdatedAt: now,
			FriendSince: map[string]time.Time{"9999": now.Add(-30 * 24 * time.Hour)}},
		{OwnerName: "9999", FriendNames: []string{"1111"}, UpdatedAt: now,
			FriendSince: map[string]time.Time{"1111": now.Add(-24 * time.Hour)}},
	} {
		if err := repositories.UnitedFriend.Save(ctx, edge); err != nil {
			t.Fatal(err)
		}
	}
	service := newCeraShopGiftQueryTestService(repositories)
	session := &gameSession{connID: "gift-rule-later-edge", selectedCharacterID: 21}
	if code := giftQueryReplyCode(t, service, session, currentCeraShopGiftQueryLiveCaptureBody()); code != currentCeraShopGiftQueryCodeFriendTooNew {
		t.Fatalf("asymmetric friendship code=%#x want=%#x", code, currentCeraShopGiftQueryCodeFriendTooNew)
	}
}

// 老数据（friend_since 字段上线之前建立的好友，落库为 NULL → 读回来是零值）必须
// **放行**：新规则不能把线上已有的老好友追溯封锁 7 天。
func TestCurrentCeraShopGiftQueryRulesGrandfatherLegacyFriendships(t *testing.T) {
	repositories := testRepositoryGroup()
	seedCeraShopGiftQuerySender(t, repositories, 21, "dnf:1", "1111", "0", 30*24*time.Hour)
	seedCeraShopGiftQueryRecipient(t, repositories, dnfrepo.CharacterRecord{
		CharacterID: "22", AccountID: "dnf:2", Name: "9999", Job: "11", Level: 87,
		Stats: map[string]int64{"grow_type": 3},
	})
	// 零值时间 = 老数据（MySQL 里就是 NULL）。
	seedCeraShopGiftQueryFriendship(t, repositories, "1111", "9999", time.Time{})
	service := newCeraShopGiftQueryTestService(repositories)
	session := &gameSession{connID: "gift-rule-legacy", selectedCharacterID: 21}
	// 老数据放行 ⇒ 走成功分支（回包里标志位 = 1，所以这里的结果码是 0 = 通过）。
	if code := giftQueryReplyCode(t, service, session, currentCeraShopGiftQueryLiveCaptureBody()); code != 0 {
		t.Fatalf("legacy friendship code=%#x want success (grandfathered)", code)
	}
}

// 跨大区：当前部署所有人 server_group_id 都是 0，所以这里显式造出两个不同分组。
func TestCurrentCeraShopGiftQueryRulesRejectCrossServerGroup(t *testing.T) {
	repositories := testRepositoryGroup()
	ctx := context.Background()
	if err := repositories.Character.Save(ctx, dnfrepo.CharacterRecord{
		CharacterID: "21", AccountID: "dnf:1", Name: "1111", Job: "0", Level: 90,
		Stats: map[string]int64{"grow_type": 1, "server_group_id": 2},
	}); err != nil {
		t.Fatal(err)
	}
	if err := repositories.Account.Save(ctx, dnfrepo.AccountRecord{
		AccountID: "dnf:1", CreatedAt: time.Now().UTC().Add(-30 * 24 * time.Hour),
	}); err != nil {
		t.Fatal(err)
	}
	seedCeraShopGiftQueryRecipient(t, repositories, dnfrepo.CharacterRecord{
		CharacterID: "22", AccountID: "dnf:2", Name: "9999", Job: "11", Level: 87,
		Stats: map[string]int64{"grow_type": 3, "server_group_id": 1},
	})
	seedCeraShopGiftQueryFriendship(t, repositories, "1111", "9999", time.Now().UTC().Add(-30*24*time.Hour))
	service := newCeraShopGiftQueryTestService(repositories)
	session := &gameSession{connID: "gift-rule-cross-group", selectedCharacterID: 21}
	if code := giftQueryReplyCode(t, service, session, currentCeraShopGiftQueryLiveCaptureBody()); code != currentCeraShopGiftQueryCodeNotFound {
		t.Fatalf("cross server group code=%#x want=%#x", code, currentCeraShopGiftQueryCodeNotFound)
	}
}

// 收件人不可解析（没有 grow_type）时必须回"请稍后再使用"，不能猜一个成功包出去。
func TestCurrentCeraShopGiftQueryRulesRejectUnreadableRecipient(t *testing.T) {
	repositories := testRepositoryGroup()
	seedCeraShopGiftQuerySender(t, repositories, 21, "dnf:1", "1111", "0", 30*24*time.Hour)
	seedCeraShopGiftQueryRecipient(t, repositories, dnfrepo.CharacterRecord{
		CharacterID: "22", AccountID: "dnf:2", Name: "9999", Job: "11", Level: 87,
	})
	service := newCeraShopGiftQueryTestService(repositories)
	session := &gameSession{connID: "gift-rule-unreadable", selectedCharacterID: 21}
	if code := giftQueryReplyCode(t, service, session, currentCeraShopGiftQueryLiveCaptureBody()); code != currentCeraShopGiftQueryCodeRetry {
		t.Fatalf("unreadable recipient code=%#x want=%#x", code, currentCeraShopGiftQueryCodeRetry)
	}
}

// 登录角色解析不出来（会话里没有选中角色）时绝不能放行 —— 否则规则 1 会被绕过。
func TestCurrentCeraShopGiftQueryRulesRejectUnknownSender(t *testing.T) {
	repositories := testRepositoryGroup()
	seedCeraShopGiftQueryRecipient(t, repositories, dnfrepo.CharacterRecord{
		CharacterID: "22", AccountID: "dnf:2", Name: "9999", Job: "11", Level: 87,
		Stats: map[string]int64{"grow_type": 3},
	})
	service := newCeraShopGiftQueryTestService(repositories)
	session := &gameSession{connID: "gift-rule-no-sender"}
	if code := giftQueryReplyCode(t, service, session, currentCeraShopGiftQueryLiveCaptureBody()); code != currentCeraShopGiftQueryCodeRetry {
		t.Fatalf("unknown sender code=%#x want=%#x", code, currentCeraShopGiftQueryCodeRetry)
	}
}

// op64 阶段：赠送方账号注册不满 7 天 → 回 12（90CN 文案表 21969+12 = 21981
// 「年龄不符合赠送条件」）。
func TestCurrentCeraShopGiftPurchaseRulesRejectYoungAccount(t *testing.T) {
	repositories := testRepositoryGroup()
	seedCeraShopGiftQuerySender(t, repositories, 21, "dnf:1", "1111", "3", 3*24*time.Hour)
	service := newCeraShopGiftQueryTestService(repositories)
	session := &gameSession{connID: "gift-purchase-young-account", selectedCharacterID: 21}
	code, err := service.evaluateCurrentCeraShopGiftPurchaseRules(context.Background(), session, "9999", 3, true)
	if err != nil {
		t.Fatalf("evaluate purchase rules: %v", err)
	}
	if code != currentCeraShopPurchaseFailureCodeGiftAccountTooNew {
		t.Fatalf("young account code=%#x want=%#x", code, currentCeraShopPurchaseFailureCodeGiftAccountTooNew)
	}
}

// op64 阶段：双方基础职业不同 → 回 10（「该商品无法赠送」）。
func TestCurrentCeraShopGiftPurchaseRulesRejectJobMismatch(t *testing.T) {
	repositories := testRepositoryGroup()
	seedCeraShopGiftQuerySender(t, repositories, 21, "dnf:1", "1111", "3", 30*24*time.Hour)
	service := newCeraShopGiftQueryTestService(repositories)
	session := &gameSession{connID: "gift-purchase-job-mismatch", selectedCharacterID: 21}
	code, err := service.evaluateCurrentCeraShopGiftPurchaseRules(context.Background(), session, "9999", 11, true)
	if err != nil {
		t.Fatalf("evaluate purchase rules: %v", err)
	}
	if code != currentCeraShopPurchaseFailureCodeGiftNotGiftable {
		t.Fatalf("job mismatch code=%#x want=%#x", code, currentCeraShopPurchaseFailureCodeGiftNotGiftable)
	}
}

// 规则通过时必须回 0：否则"合法赠送"会被这些新规则拦住。
func TestCurrentCeraShopGiftPurchaseRulesAcceptEligibleGift(t *testing.T) {
	repositories := testRepositoryGroup()
	seedCeraShopGiftQuerySender(t, repositories, 21, "dnf:1", "1111", "3", 30*24*time.Hour)
	service := newCeraShopGiftQueryTestService(repositories)
	session := &gameSession{connID: "gift-purchase-ok", selectedCharacterID: 21}
	code, err := service.evaluateCurrentCeraShopGiftPurchaseRules(context.Background(), session, "9999", 3, true)
	if err != nil {
		t.Fatalf("evaluate purchase rules: %v", err)
	}
	if code != 0 {
		t.Fatalf("eligible gift code=%#x want 0", code)
	}
}

// 职业解析不出来时不能拦：数据缺失 ≠ 职业不同。
func TestCurrentCeraShopGiftPurchaseRulesSkipJobRuleWhenUnreadable(t *testing.T) {
	repositories := testRepositoryGroup()
	seedCeraShopGiftQuerySender(t, repositories, 21, "dnf:1", "1111", "3", 30*24*time.Hour)
	service := newCeraShopGiftQueryTestService(repositories)
	session := &gameSession{connID: "gift-purchase-job-unknown", selectedCharacterID: 21}
	code, err := service.evaluateCurrentCeraShopGiftPurchaseRules(context.Background(), session, "9999", 11, false)
	if err != nil {
		t.Fatalf("evaluate purchase rules: %v", err)
	}
	if code != 0 {
		t.Fatalf("unreadable job code=%#x want 0", code)
	}
}

// 账号年龄不可知（没有账号记录）时不拦：宁可放行，也不能用缺失数据封锁赠送。
func TestCurrentCeraShopGiftPurchaseRulesSkipAccountRuleWhenUnknown(t *testing.T) {
	repositories := testRepositoryGroup()
	seedCeraShopGiftQuerySender(t, repositories, 21, "dnf:1", "1111", "3", 0)
	service := newCeraShopGiftQueryTestService(repositories)
	session := &gameSession{connID: "gift-purchase-no-account", selectedCharacterID: 21}
	code, err := service.evaluateCurrentCeraShopGiftPurchaseRules(context.Background(), session, "9999", 3, true)
	if err != nil {
		t.Fatalf("evaluate purchase rules: %v", err)
	}
	if code != 0 {
		t.Fatalf("unknown account code=%#x want 0", code)
	}
}
