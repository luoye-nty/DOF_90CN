package dnfbridge

import (
	"bytes"
	"context"
	"encoding/binary"
	"testing"

	dnfrepo "longheng.io/server/internal/modules/dnf/repository"
	dnfrepomemory "longheng.io/server/internal/modules/dnf/repository/memory"
)

// requireInitialTownCeraShopWalletProjection 从进城包序列里吃掉`商城钱包`通知
// （类 0 / op53），返回剩下的字节。
//
// 选角进城现在会多这一包：客户端没有"请求钱包"的上行包，不推的话商城那三个数字
// 永远是 0。这个助手与被吃的 860 限购、公会投影同款——把新增包显式计入序列，
// 而不是放宽 `len(rest)==0` 断言（那样会让后面真正的多发包失去保护）。
func requireInitialTownCeraShopWalletProjection(t *testing.T, stream []byte) []byte {
	t.Helper()
	if len(stream) == 0 {
		t.Fatal("选角进城序列里缺少商城钱包通知(class0/op53)")
	}
	packet, rest := splitCurrentGameServerUpperPacketAuto(t, stream)
	if packet.Header.Classification != 0 || packet.Header.MsgID != currentCeraShopWalletMsgID {
		t.Fatalf("商城钱包通知 header=%+v want=class0/op%d", packet.Header, currentCeraShopWalletMsgID)
	}
	body := packet.Body
	if len(body) == 16 && body[0] == 0 && body[1] == 0 && body[2] == 0 {
		body = body[3:] // server16 头模式在包体前带 3 个固定 0 字节
	}
	if len(body) != 13 || body[0] != 1 {
		t.Fatalf("商城钱包通知体=%x want 13 字节且 flag=1", packet.Body)
	}
	return rest
}

// 现场症状：登录后商城界面点券 / 代币券 / 欢乐代币券三个数字都是 0，玩家以为
// 代币券没到账（甚至拿它当材料用）。
//
// 根因是客户端**没有"请求钱包"的上行包**：class0/0x0035 是纯推送，一到就把包体
// 三个 i32 写进客户端那三个字段。服务端只在购买成功后推过一次（cera_shop_purchase.go
// 第 623 行），所以"这次会话里没买过东西"就一直是 0。
//
// 这组测试守住：进城/回城时按真实余额重放，且读不到账号时**不发**（零值覆盖比不发更糟）。

// 布局必须与客户端反编译一致：u8 flag + i32 + i32 + i32 = 13 字节，小端。
// （handler FUN_01d83c20 整包只有 4 次读：0x034E3970 一次 + 0x034E3A50 三次。）
func TestCurrentCeraShopWalletBodyIsThirteenBytesFlagThenThreeInt32(t *testing.T) {
	body := buildCurrentCeraShopWalletBody(700, 100, 100004)
	if len(body) != 13 {
		t.Fatalf("钱包包体长度=%d want=13 (%x)", len(body), body)
	}
	if body[0] != 1 {
		t.Fatalf("钱包包体首字节 flag=%d want=1", body[0])
	}
	if got := int32(binary.LittleEndian.Uint32(body[1:5])); got != 700 {
		t.Fatalf("点券=%d want=700", got)
	}
	if got := int32(binary.LittleEndian.Uint32(body[5:9])); got != 100 {
		t.Fatalf("代币券=%d want=100", got)
	}
	if got := int32(binary.LittleEndian.Uint32(body[9:13])); got != 100004 {
		t.Fatalf("欢乐代币券=%d want=100004", got)
	}
}

func TestCurrentCeraShopWalletProjectionReplaysRealAccountBalances(t *testing.T) {
	ctx := context.Background()
	repositories := dnfrepomemory.NewMemoryGroup()
	if err := repositories.Account.Save(ctx, dnfrepo.AccountRecord{
		AccountID: "account-1",
		Metadata: map[string]string{
			"account_cera":             "700",
			"account_token_cera":       "100",
			"account_happy_token_cera": "100004",
		},
	}); err != nil {
		t.Fatal(err)
	}
	connection := &bufferConn{}
	service := &Service{
		options: options{
			accountID:          "account-1",
			gameUpperHeader:    gameUpperHeaderServer16,
			gameUpperBodyCodec: gameUpperBodyCodecPlain,
		},
		repositoryProvider: func() (dnfrepo.Group, bool) { return repositories, true },
	}
	session := &gameSession{conn: connection, connID: "cera-shop-wallet", selectedCharacterID: 19}

	if err := service.sendCurrentCeraShopWalletProjection(ctx, session, "unit_test"); err != nil {
		t.Fatal(err)
	}
	packet, trailing := splitGameServerUpperPacket(t, connection.write.Bytes())
	if len(trailing) != 0 {
		t.Fatalf("钱包通知后有多余字节: %x", trailing)
	}
	// class 必须是 0：同一个 opcode 1035 在 class-1 表里是婚礼命令回包，发错表客户端会走错 handler。
	if packet.Header.Classification != 0 || packet.Header.MsgID != currentCeraShopWalletMsgID {
		t.Fatalf("钱包通知 header=%+v want=class0/op%#x", packet.Header, currentCeraShopWalletMsgID)
	}
	body := packet.Body
	if len(body) > 3 && body[0] == 0 && body[1] == 0 && body[2] == 0 {
		body = body[3:] // server16 头模式在包体前带 3 个固定 0 字节
	}
	if !bytes.Equal(body, buildCurrentCeraShopWalletBody(700, 100, 100004)) {
		t.Fatalf("钱包通知体=%x", packet.Body)
	}
}

// 账号读不到时绝不能发：零值会把客户端已经正确的余额覆盖成 0。发与不发之间，
// 不发永远更安全（客户端保留现值）。
func TestCurrentCeraShopWalletProjectionStaysSilentWhenAccountIsUnknown(t *testing.T) {
	ctx := context.Background()
	cases := map[string]*Service{}
	repositories := dnfrepomemory.NewMemoryGroup()
	cases["account_missing"] = &Service{
		options:            options{accountID: "account-never-saved"},
		repositoryProvider: func() (dnfrepo.Group, bool) { return repositories, true },
	}
	cases["repository_unavailable"] = &Service{
		options: options{accountID: "account-1"},
	}
	cases["account_repository_nil"] = &Service{
		options:            options{accountID: "account-1"},
		repositoryProvider: func() (dnfrepo.Group, bool) { return dnfrepo.Group{}, true },
	}
	for name, service := range cases {
		t.Run(name, func(t *testing.T) {
			connection := &bufferConn{}
			session := &gameSession{conn: connection, connID: name, selectedCharacterID: 19}
			if err := service.sendCurrentCeraShopWalletProjection(ctx, session, "unit_test"); err != nil {
				t.Fatalf("不应报错: %v", err)
			}
			if connection.write.Len() != 0 {
				t.Fatalf("账号未知时不应发包: %x", connection.write.Bytes())
			}
		})
	}
}

// 没选角色 / session 为空：同样不发包、不 panic。这两个是回城流程里的边界。
func TestCurrentCeraShopWalletProjectionSkipsSessionWithoutCharacter(t *testing.T) {
	ctx := context.Background()
	repositories := dnfrepomemory.NewMemoryGroup()
	if err := repositories.Account.Save(ctx, dnfrepo.AccountRecord{
		AccountID: "account-1",
		Metadata:  map[string]string{"account_cera": "700"},
	}); err != nil {
		t.Fatal(err)
	}
	service := &Service{
		options:            options{accountID: "account-1"},
		repositoryProvider: func() (dnfrepo.Group, bool) { return repositories, true },
	}
	connection := &bufferConn{}
	if err := service.sendCurrentCeraShopWalletProjection(ctx, &gameSession{conn: connection, connID: "no-char"}, "unit_test"); err != nil {
		t.Fatal(err)
	}
	if connection.write.Len() != 0 {
		t.Fatalf("没选角色时不应发包: %x", connection.write.Bytes())
	}
	if err := service.sendCurrentCeraShopWalletProjection(ctx, nil, "unit_test"); err != nil {
		t.Fatalf("nil session 不应报错: %v", err)
	}
	if err := (*Service)(nil).sendCurrentCeraShopWalletProjection(ctx, &gameSession{selectedCharacterID: 19}, "unit_test"); err != nil {
		t.Fatalf("nil service 不应报错: %v", err)
	}
}
