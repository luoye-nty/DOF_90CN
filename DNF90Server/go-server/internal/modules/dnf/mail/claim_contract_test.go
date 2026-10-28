package mail

import (
	"context"
	"fmt"
	"testing"
	"time"

	"longheng.io/server/internal/modules/dnf/alignedcmd"
	"longheng.io/server/internal/modules/dnf/dnfenum"
	"longheng.io/server/internal/modules/dnf/premium"
	dnfrepo "longheng.io/server/internal/modules/dnf/repository"
	dnfrepomemory "longheng.io/server/internal/modules/dnf/repository/memory"
	dnfrevivecoin "longheng.io/server/internal/modules/dnf/revivecoin"
)

const (
	claimContractItemID  int64 = 10000391
	claimContractType    int64 = 3
	claimContractSeconds int64 = 86400
)

func claimContractResolver() alignedcmd.PremiumContractResolver {
	return func(itemID int64) (alignedcmd.PremiumContractResolution, error) {
		if itemID != claimContractItemID {
			return alignedcmd.PremiumContractResolution{}, nil
		}
		return alignedcmd.PremiumContractResolution{
			ItemID:          itemID,
			PremiumType:     claimContractType,
			DurationSeconds: claimContractSeconds,
		}, nil
	}
}

// claimContractRepos builds a character whose consumable tab is completely full,
// which is the state every one of these letters is claimed in.
func claimContractRepos(t *testing.T, ctx context.Context, attachments []dnfrepo.MailAttachment) dnfrepo.Group {
	t.Helper()
	repos := dnfrepomemory.NewMemoryGroup()
	mustSaveCharacter(t, repos, dnfrepo.CharacterRecord{CharacterID: "77", AccountID: "acc-a", Name: "receiver"})
	if err := repos.Account.Save(ctx, dnfrepo.AccountRecord{AccountID: "acc-a"}); err != nil {
		t.Fatalf("save account: %v", err)
	}
	slots := map[string]dnfrepo.ItemStack{
		dnfrevivecoin.WalletKey(): {
			ItemID: dnfrevivecoin.WalletItemID,
			Count:  4,
			Extra:  map[string]string{"item_kind": "stackable", "revive_coin_wallet": "1"},
		},
	}
	for slot := int16(3); slot <= 8; slot++ {
		slots[fmt.Sprintf("%d:%d", dnfrepo.MainInventoryListType, slot)] = dnfrepo.ItemStack{ItemID: 800000 + int64(slot), Count: 1}
	}
	for slot := int16(65); slot <= 120; slot++ {
		slots[fmt.Sprintf("%d:%d", dnfrepo.MainInventoryListType, slot)] = dnfrepo.ItemStack{ItemID: 900000 + int64(slot), Count: 1}
	}
	if err := repos.Inventory.Save(ctx, dnfrepo.InventoryRecord{CharacterID: "77", Slots: slots}); err != nil {
		t.Fatalf("save inventory: %v", err)
	}
	if err := repos.Mailbox.Save(ctx, dnfrepo.MailboxRecord{
		CharacterID: "77",
		Mails:       map[string]dnfrepo.MailRecord{"1": {MailID: "1", Attachments: attachments}},
	}); err != nil {
		t.Fatalf("save mailbox: %v", err)
	}
	return repos
}

func claimContractRequest(repos dnfrepo.Group, claimObject uint32) alignedcmd.Request {
	return alignedcmd.Request{
		Opcode:                  uint16(dnfenum.CmdPacketMailboxExtractItem),
		SelectedCharacterID:     77,
		AccountID:               "acc-a",
		Repositories:            repos,
		MailboxItemResolver:     mailboxClaimTestResolver(65, 120, 0),
		PremiumContractResolver: claimContractResolver(),
		Body:                    currentMailIDList(claimObject),
	}
}

// TestMailboxClaimActivatesContractWithoutASlot proves a mailed 契约 is delivered
// by activating it: the account premium term grows, no backpack slot is used and
// the claim succeeds against a completely full consumable tab.
func TestMailboxClaimActivatesContractWithoutASlot(t *testing.T) {
	ctx := context.Background()
	repos := claimContractRepos(t, ctx, []dnfrepo.MailAttachment{
		{ItemID: claimContractItemID, Count: 2},
	})
	claimObject, err := MailboxAttachmentClaimObject(1, 0)
	if err != nil {
		t.Fatal(err)
	}
	got, err := NewHandler().Handle(ctx, claimContractRequest(repos, claimObject))
	if err != nil {
		t.Fatalf("Handle error = %v", err)
	}
	body := got.UpperResponses[0].Body
	if len(body) == 2 && body[0] == 0x00 {
		t.Fatalf("contract claim was rejected with code %d; a contract needs no slot", body[1])
	}

	account, found, err := repos.Account.Load(ctx, "acc-a")
	if err != nil || !found {
		t.Fatalf("load account found=%t err=%v", found, err)
	}
	if expireAt := premium.ExpireAt(account, claimContractType); expireAt < time.Now().Unix()+claimContractSeconds {
		t.Fatalf("premium expiry = %d, want both contract days", expireAt)
	}
	inventory, _, _ := repos.Inventory.Load(ctx, "77")
	for key, stack := range inventory.Slots {
		if stack.ItemID == claimContractItemID {
			t.Fatalf("the contract must not occupy a slot: %s %+v", key, stack)
		}
	}
	box, _, _ := repos.Mailbox.Load(ctx, "77")
	if mail := box.Mails["1"]; !mail.Claimed {
		t.Fatalf("the letter's only slot was claimed, so it must close: %+v", mail)
	}

	// Granting the term is only half the delivery: without the activation
	// notification the client keeps drawing the old remaining time, which reads in
	// game as a contract that added no days at all.
	notice, found := mailboxPremiumActivation(got)
	if !found {
		t.Fatalf("a claimed contract must send the activation notification: %+v", got.UpperResponses)
	}
	if notice.Body[2] != byte(claimContractType) {
		t.Fatalf("notification premium type = %d, want %d", notice.Body[2], claimContractType)
	}
	remaining := int64(0)
	for shift := 0; shift < 8; shift++ {
		remaining |= int64(notice.Body[3+shift]) << (8 * shift)
	}
	if remaining < 2*claimContractSeconds {
		t.Fatalf("notified remaining = %d, want both contract days", remaining)
	}
}

// mailboxPremiumActivation finds the class0 premium activation notification in a
// claim response.
func mailboxPremiumActivation(got alignedcmd.Result) (alignedcmd.UpperResponse, bool) {
	for _, response := range got.UpperResponses {
		if response.MsgID == premium.ActivatedNotificationMessageID && len(response.Body) == 11 {
			return response, true
		}
	}
	return alignedcmd.UpperResponse{}, false
}

// TestMailboxClaimCreditsReviveCoinWallet proves a mailed 复活币 goes to the fixed
// slot-1 wallet instead of demanding a consumable slot.
func TestMailboxClaimCreditsReviveCoinWallet(t *testing.T) {
	ctx := context.Background()
	repos := claimContractRepos(t, ctx, []dnfrepo.MailAttachment{
		{ItemID: dnfrevivecoin.ConsumableItemID, Count: 7},
	})
	claimObject, err := MailboxAttachmentClaimObject(1, 0)
	if err != nil {
		t.Fatal(err)
	}
	got, err := NewHandler().Handle(ctx, claimContractRequest(repos, claimObject))
	if err != nil {
		t.Fatalf("Handle error = %v", err)
	}
	body := got.UpperResponses[0].Body
	if len(body) == 2 && body[0] == 0x00 {
		t.Fatalf("revive-coin claim was rejected with code %d; the wallet has room", body[1])
	}
	inventory, _, _ := repos.Inventory.Load(ctx, "77")
	wallet := inventory.Slots[dnfrevivecoin.WalletKey()]
	if wallet.ItemID != dnfrevivecoin.WalletItemID || wallet.Count != 11 {
		t.Fatalf("wallet = %+v, want item %d count 11", wallet, dnfrevivecoin.WalletItemID)
	}
	for key, stack := range inventory.Slots {
		if stack.ItemID == dnfrevivecoin.ConsumableItemID {
			t.Fatalf("the coin consumable must not be stored: %s %+v", key, stack)
		}
	}
}

// TestMailboxClaimKeepsContractWhenAnotherSlotBlocksTheLetter proves the staging
// is per letter: a whole-letter claim that cannot place its ordinary item must
// not grant the contract either.
func TestMailboxClaimKeepsContractWhenAnotherSlotBlocksTheLetter(t *testing.T) {
	ctx := context.Background()
	repos := claimContractRepos(t, ctx, []dnfrepo.MailAttachment{
		{ItemID: claimContractItemID, Count: 1},
		{ItemID: 2001, Count: 1},
	})
	got, err := NewHandler().Handle(ctx, claimContractRequest(repos, 1))
	if err != nil {
		t.Fatalf("Handle error = %v", err)
	}
	assertOneMailboxResponse(t, got, dnfenum.CmdPacketMailboxExtractItem, []byte{0x00, 0x04})
	account, _, _ := repos.Account.Load(ctx, "acc-a")
	if expireAt := premium.ExpireAt(account, claimContractType); expireAt != 0 {
		t.Fatalf("premium expiry = %d, want none for a refused letter", expireAt)
	}
	box, _, _ := repos.Mailbox.Load(ctx, "77")
	mail := box.Mails["1"]
	if mail.Claimed || len(mail.Attachments) != 2 {
		t.Fatalf("a refused letter must keep both attachments: %+v", mail)
	}
}

// TestMailboxClaimContractSlotLeavesTheBlockedSlotBehind is the practical case:
// the contract slot is claimed on its own while the item that has nowhere to go
// stays in the letter at its original position.
func TestMailboxClaimContractSlotLeavesTheBlockedSlotBehind(t *testing.T) {
	ctx := context.Background()
	repos := claimContractRepos(t, ctx, []dnfrepo.MailAttachment{
		{ItemID: 2001, Count: 1},
		{ItemID: claimContractItemID, Count: 1},
	})
	claimObject, err := MailboxAttachmentClaimObject(1, 1)
	if err != nil {
		t.Fatal(err)
	}
	got, err := NewHandler().Handle(ctx, claimContractRequest(repos, claimObject))
	if err != nil {
		t.Fatalf("Handle error = %v", err)
	}
	body := got.UpperResponses[0].Body
	if len(body) == 2 && body[0] == 0x00 {
		t.Fatalf("the contract slot was rejected with code %d", body[1])
	}
	account, _, _ := repos.Account.Load(ctx, "acc-a")
	if expireAt := premium.ExpireAt(account, claimContractType); expireAt < time.Now().Unix() {
		t.Fatalf("premium expiry = %d, want the contract activated", expireAt)
	}
	box, _, _ := repos.Mailbox.Load(ctx, "77")
	mail := box.Mails["1"]
	if mail.Claimed {
		t.Fatalf("the blocked slot keeps the letter claimable: %+v", mail)
	}
	if len(mail.Attachments) != 2 || mail.Attachments[0].ItemID != 2001 || mail.Attachments[1].ItemID != 0 {
		t.Fatalf("attachments = %+v, want the contract slot blanked in place", mail.Attachments)
	}
}
