package chat

import (
	"context"
	"errors"
	"fmt"
	"strconv"
	"strings"
	"time"

	dnfrepo "longheng.io/server/internal/modules/dnf/repository"
)

// The current EXE decides which megaphone a chat mode spends before it sends
// anything: the mode byte indexes a jump table that yields one stackable item
// id, and the surrounding helper refuses the mode when the backpack holds none
// of it. Disassembled from the shipped client (`client/DNF.exe`, no packer):
// `mode - 11` indexes the byte table at 0x027296D0 into the switch at
// 0x027295DE, and the same mapping is repeated in the guard at 0x027175C7.
//
//	mode 11 -> 0x24 = 36        服务器喇叭
//	mode 12 -> 0x382 = 898      解放的服务器喇叭
//	mode 13 -> 0x25 = 37        频道喇叭
//	mode 14 -> 0x460 = 1120     服务器喇叭 (GM)
//	mode 15 -> 0x28992F         占领战支援喇叭
//	mode 53 -> 0x98A146         同心喇叭
//
// Only the two modes the release answers are named here.
const (
	ServerMegaphoneItemID  uint32 = 36
	ChannelMegaphoneItemID uint32 = 37
)

var (
	ErrOwnerUnavailable  = errors.New("chat megaphone owner is unavailable")
	ErrAccountRequired   = errors.New("chat megaphone account id is required")
	ErrCharacterRequired = errors.New("chat megaphone character id is required")
	ErrSlotInvalid       = errors.New("chat megaphone inventory slot is invalid")
	ErrCharacterNotFound = errors.New("chat megaphone character is not found")
	ErrAccountMismatch   = errors.New("chat megaphone character account mismatch")
	ErrInventoryNotFound = errors.New("chat megaphone inventory is not found")
	ErrItemMissing       = errors.New("chat megaphone item is missing")
)

type Command struct {
	AccountID   string
	CharacterID string
	Slot        int16
	UpdatedAt   time.Time
}

type Result struct {
	Slot      int16
	Remaining uint32
	Removed   bool
}

type Owner struct {
	assets dnfrepo.CharacterAssetUnitOfWork
}

func NewOwner(repositories dnfrepo.Group) (*Owner, error) {
	if repositories.CharacterAssets == nil {
		return nil, ErrOwnerUnavailable
	}
	return &Owner{assets: repositories.CharacterAssets}, nil
}

// ConsumeServerMegaphone debits one exact item-36 stack from the main
// inventory slot carried by the current client's mode-11 chat request. The
// character identity check and debit commit in one scoped transaction; packet
// broadcast remains a bridge responsibility after this call succeeds.
func (o *Owner) ConsumeServerMegaphone(ctx context.Context, command Command) (Result, error) {
	return o.consumeMegaphone(ctx, command, ServerMegaphoneItemID)
}

// ConsumeChannelMegaphone is the mode-13 twin of ConsumeServerMegaphone and
// spends item 37 instead. Both modes carry the same request shape, so the debit
// rule is identical apart from the item id the client itself picked.
func (o *Owner) ConsumeChannelMegaphone(ctx context.Context, command Command) (Result, error) {
	return o.consumeMegaphone(ctx, command, ChannelMegaphoneItemID)
}

func (o *Owner) consumeMegaphone(ctx context.Context, command Command, itemID uint32) (Result, error) {
	if o == nil || o.assets == nil {
		return Result{}, ErrOwnerUnavailable
	}
	accountID := strings.TrimSpace(command.AccountID)
	characterID := strings.TrimSpace(command.CharacterID)
	if accountID == "" {
		return Result{}, ErrAccountRequired
	}
	if characterID == "" {
		return Result{}, ErrCharacterRequired
	}
	if command.Slot < 0 || dnfrepo.IsAccountSharedInventorySlot(dnfrepo.MainInventoryListType, command.Slot) {
		return Result{}, ErrSlotInvalid
	}
	if ctx == nil {
		ctx = context.Background()
	}
	if err := ctx.Err(); err != nil {
		return Result{}, err
	}
	now := command.UpdatedAt.UTC()
	if now.IsZero() {
		now = time.Now().UTC()
	}

	var result Result
	err := dnfrepo.WithinCharacterAssetsScoped(ctx, o.assets, characterID, func(
		characters dnfrepo.CharacterRepository,
		inventories dnfrepo.InventoryRepository,
		_ dnfrepo.EquipmentRepository,
	) error {
		if characters == nil || inventories == nil {
			return ErrOwnerUnavailable
		}
		character, found, err := characters.Load(ctx, characterID)
		if err != nil {
			return err
		}
		if !found || strings.TrimSpace(character.CharacterID) != characterID {
			return fmt.Errorf("%w: character=%s", ErrCharacterNotFound, characterID)
		}
		if strings.TrimSpace(character.AccountID) != accountID {
			return fmt.Errorf("%w: account=%s character=%s", ErrAccountMismatch, accountID, characterID)
		}
		inventory, found, err := inventories.Load(ctx, characterID)
		if err != nil {
			return err
		}
		if !found || strings.TrimSpace(inventory.CharacterID) != characterID || inventory.Slots == nil {
			return fmt.Errorf("%w: character=%s", ErrInventoryNotFound, characterID)
		}
		inventory = dnfrepo.CloneInventory(inventory)
		key := strconv.Itoa(int(dnfrepo.MainInventoryListType)) + ":" + strconv.Itoa(int(command.Slot))
		stack, found := inventory.Slots[key]
		if !found || stack.ItemID != int64(itemID) || stack.Count < 1 {
			return fmt.Errorf("%w: slot=%d item=%d count=%d", ErrItemMissing, command.Slot, stack.ItemID, stack.Count)
		}
		stack.Count--
		result = Result{Slot: command.Slot, Remaining: uint32(stack.Count), Removed: stack.Count == 0}
		if stack.Count == 0 {
			delete(inventory.Slots, key)
		} else {
			if stack.Extra == nil {
				stack.Extra = make(map[string]string, 2)
			}
			count := strconv.FormatInt(stack.Count, 10)
			stack.Extra["count"] = count
			stack.Extra["amount_or_count"] = count
			inventory.Slots[key] = stack
		}
		inventory.UpdatedAt = now
		return dnfrepo.SaveInventoryFields(ctx, inventories, inventory, dnfrepo.InventoryFieldSlots)
	})
	if err != nil {
		return Result{}, err
	}
	return result, nil
}
