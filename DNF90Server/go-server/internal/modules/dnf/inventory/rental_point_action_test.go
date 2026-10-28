package inventory

import (
	"bytes"
	"context"
	"encoding/binary"
	"errors"
	"testing"
	"time"

	"longheng.io/server/internal/modules/dnf/alignedcmd"
	"longheng.io/server/internal/modules/dnf/dnfenum"
	dnfrental "longheng.io/server/internal/modules/dnf/rental"
	dnfrepo "longheng.io/server/internal/modules/dnf/repository"
	dnfrepomemory "longheng.io/server/internal/modules/dnf/repository/memory"
)

// 50幸运星礼盒 (490001384) carries [action type] `[rent point]` 50, which the
// current client sends as op515 action index 105. Before this action existed
// the request fell through to "unsupported" and the client only ever saw
// op515 failure 17, so the box could not be opened at all.
func newRentalPointRequest(t *testing.T, repos dnfrepo.Group, slot uint16, grant, limit uint32, now time.Time) alignedcmd.Request {
	t.Helper()
	body := make([]byte, 19)
	binary.LittleEndian.PutUint16(body[0:2], slot)
	binary.LittleEndian.PutUint32(body[7:11], rentalPointActionIndex)
	return alignedcmd.Request{
		Opcode:              uint16(dnfenum.CmdPacketUseStackableAction),
		Body:                body,
		AccountID:           "acc",
		SelectedCharacterID: 42,
		Repositories:        repos,
		RentalPointNow:      now,
		StackableUseResolver: func(itemID int64) (alignedcmd.StackableUseResolution, error) {
			return alignedcmd.StackableUseResolution{
				ItemID:        itemID,
				PVFPath:       "stackable/490001001/chn_490001384.stk",
				StackableType: "[waste]",
				RentalPoint:   &alignedcmd.RentalPointGrantResolution{Grant: grant, Limit: limit},
			}, nil
		},
	}
}

func newRentalPointFixture(t *testing.T, ctx context.Context, points string, count int64) dnfrepo.Group {
	t.Helper()
	repos := dnfrepomemory.NewMemoryGroup()
	account := dnfrepo.AccountRecord{AccountID: "acc"}
	if points != "" {
		account.Metadata = map[string]string{dnfrental.PointMetadataKey: points}
	}
	if err := repos.Account.Save(ctx, account); err != nil {
		t.Fatal(err)
	}
	if err := repos.Character.Save(ctx, dnfrepo.CharacterRecord{CharacterID: "42", AccountID: "acc"}); err != nil {
		t.Fatal(err)
	}
	if err := repos.Inventory.Save(ctx, dnfrepo.InventoryRecord{CharacterID: "42", Slots: map[string]dnfrepo.ItemStack{
		"0:3": {ItemID: 490001384, Count: count, Extra: map[string]string{"pvf_path": "stackable/490001001/chn_490001384.stk"}},
	}}); err != nil {
		t.Fatal(err)
	}
	return repos
}

func TestUseStackableAction105ChargesRentalWalletAndConsumesOneBox(t *testing.T) {
	ctx := context.Background()
	now := time.Date(2026, time.September, 7, 12, 0, 0, 0, time.UTC)
	repos := newRentalPointFixture(t, ctx, "10", 2)
	result, err := NewHandler().Handle(ctx, newRentalPointRequest(t, repos, 3, 50, 400, now))
	if err != nil {
		t.Fatal(err)
	}
	if !result.ResponseAllowed || len(result.UpperResponses) != 1 {
		t.Fatalf("result=%+v", result)
	}
	wantAck := []byte{1, 3, 0, 0, 0, 1, 0, 0, 0, 1}
	if got := result.UpperResponses[0].Body; !bytes.Equal(got, wantAck) {
		t.Fatalf("op515 ACK=%x want=%x", got, wantAck)
	}
	if len(result.ItemSlotRefreshes) != 1 || result.ItemSlotRefreshes[0].SlotIndex != 3 ||
		len(result.PostActions) != 1 || result.PostActions[0] != alignedcmd.PostActionRefreshSelectedRentalWallet {
		t.Fatalf("refresh result=%+v", result)
	}
	bag, found, err := repos.Inventory.Load(ctx, "42")
	if err != nil || !found || bag.Slots["0:3"].Count != 1 {
		t.Fatalf("inventory=%+v found=%t err=%v", bag, found, err)
	}
	account, found, err := repos.Account.Load(ctx, "acc")
	if err != nil || !found {
		t.Fatalf("account found=%t err=%v", found, err)
	}
	points, err := dnfrental.Points(account)
	if err != nil || points != 60 {
		t.Fatalf("wallet=%d err=%v want 60", points, err)
	}
}

// The client's own explain text caps the wallet ("持有上限为400个"), so the
// grant is clamped instead of overflowing the cap.
func TestUseStackableAction105ClampsGrantToWalletLimit(t *testing.T) {
	ctx := context.Background()
	now := time.Date(2026, time.September, 7, 12, 0, 0, 0, time.UTC)
	repos := newRentalPointFixture(t, ctx, "380", 1)
	if _, err := NewHandler().Handle(ctx, newRentalPointRequest(t, repos, 3, 50, 400, now)); err != nil {
		t.Fatal(err)
	}
	account, _, err := repos.Account.Load(ctx, "acc")
	if err != nil {
		t.Fatal(err)
	}
	points, err := dnfrental.Points(account)
	if err != nil || points != 400 {
		t.Fatalf("wallet=%d err=%v want 400", points, err)
	}
	bag, _, err := repos.Inventory.Load(ctx, "42")
	if err != nil {
		t.Fatal(err)
	}
	if _, present := bag.Slots["0:3"]; present {
		t.Fatalf("last box must be consumed: %+v", bag.Slots)
	}
}

// A full wallet must refuse the use so the box is not eaten for nothing.
func TestUseStackableAction105RefusesWhenWalletIsFull(t *testing.T) {
	ctx := context.Background()
	now := time.Date(2026, time.September, 7, 12, 0, 0, 0, time.UTC)
	repos := newRentalPointFixture(t, ctx, "400", 1)
	owner, err := NewOwner(repos)
	if err != nil {
		t.Fatal(err)
	}
	request := newRentalPointRequest(t, repos, 3, 50, 400, now)
	parsed, err := DecodeUseStackableActionRequest(request.Body)
	if err != nil {
		t.Fatal(err)
	}
	_, err = owner.UseRentalPointAction(ctx, NewUseStackableActionCommand(request, parsed), request.StackableUseResolver, now)
	if !errors.Is(err, ErrRentalPointLimit) {
		t.Fatalf("full wallet error=%v want ErrRentalPointLimit", err)
	}
	bag, _, err := repos.Inventory.Load(ctx, "42")
	if err != nil || bag.Slots["0:3"].Count != 1 {
		t.Fatalf("box must survive a refused use: %+v err=%v", bag.Slots, err)
	}
}
