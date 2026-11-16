package dungeon

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"time"

	dnfrepo "longheng.io/server/internal/modules/dnf/repository"
)

var (
	ErrDirectContainerDropInvalid   = errors.New("direct dungeon container drop is invalid")
	ErrDirectContainerDropAmbiguous = errors.New("direct dungeon container drop has a partial prior delivery")
)

// DirectContainerDropItem is a fully built inventory stack that bypasses the
// scene-pickup protocol. The bridge owns PVF/raw-row projection; this owner
// owns the atomic inventory-or-mail delivery.
type DirectContainerDropItem struct {
	Stack    dnfrepo.ItemStack
	GrantKey string
}

type DirectContainerDropCommand struct {
	CharacterID   string
	ListType      byte
	SlotStart     int16
	SlotEnd       int16
	CompletionKey string
	MailTitle     string
	MailBody      string
	Source        string
	UpdatedAt     time.Time
	Items         []DirectContainerDropItem
	Project       func(slot int16, stack dnfrepo.ItemStack) (dnfrepo.ItemStack, error)
}

type DirectContainerDropResultItem struct {
	Slot  int16
	Stack dnfrepo.ItemStack
}

type DirectContainerDropResult struct {
	Items           []DirectContainerDropResultItem
	MailID          string
	MailAttachments int
	Replayed        bool
}

// GrantDirectContainerDrops commits a complete batch either into the supplied
// inventory page or into one idempotent system mail when that page cannot hold
// all rows. GrantKey is persisted in each stack/attachment so an indeterminate
// commit can be replayed without duplicating the reward.
func (o *Owner) GrantDirectContainerDrops(
	ctx context.Context,
	cmd DirectContainerDropCommand,
) (DirectContainerDropResult, error) {
	if o == nil || o.mailboxAssets == nil || strings.TrimSpace(cmd.CharacterID) == "" ||
		cmd.ListType == 0 || cmd.SlotEnd < cmd.SlotStart || strings.TrimSpace(cmd.CompletionKey) == "" ||
		strings.TrimSpace(cmd.Source) == "" || len(cmd.Items) == 0 || cmd.Project == nil {
		return DirectContainerDropResult{}, ErrDirectContainerDropInvalid
	}
	seen := make(map[string]struct{}, len(cmd.Items))
	for _, item := range cmd.Items {
		key := strings.TrimSpace(item.GrantKey)
		if item.Stack.ItemID <= 0 || item.Stack.Count <= 0 || key == "" ||
			item.Stack.Extra == nil || item.Stack.Extra[directContainerDropGrantKeyExtra] != key {
			return DirectContainerDropResult{}, ErrDirectContainerDropInvalid
		}
		if _, duplicate := seen[key]; duplicate {
			return DirectContainerDropResult{}, ErrDirectContainerDropInvalid
		}
		seen[key] = struct{}{}
	}
	if ctx == nil {
		ctx = context.Background()
	}
	now := cmd.UpdatedAt.UTC()
	if now.IsZero() {
		now = time.Now().UTC()
	}

	var result DirectContainerDropResult
	err := o.mailboxAssets.WithinMailboxAssets(ctx, cmd.CharacterID, cmd.CharacterID, func(
		_ dnfrepo.AccountRepository,
		_ dnfrepo.AccountInventoryRepository,
		_ dnfrepo.CharacterRepository,
		inventories dnfrepo.InventoryRepository,
		mailboxes dnfrepo.MailboxRepository,
	) error {
		inventory, found, err := inventories.Load(ctx, cmd.CharacterID)
		if err != nil {
			return err
		}
		if !found {
			inventory = dnfrepo.InventoryRecord{CharacterID: cmd.CharacterID}
		}
		inventory = dnfrepo.CloneInventory(inventory)
		if inventory.Slots == nil {
			inventory.Slots = make(map[string]dnfrepo.ItemStack)
		}
		mailbox, _, err := mailboxes.Load(ctx, cmd.CharacterID)
		if err != nil {
			return err
		}

		existing := make([]DirectContainerDropResultItem, len(cmd.Items))
		existingDirect := make([]bool, len(cmd.Items))
		existingMail := make([]bool, len(cmd.Items))
		directCount := 0
		mailCount := 0
		for index, item := range cmd.Items {
			slot, stack, ok, findErr := directContainerDropInventoryGrant(inventory, cmd.ListType, item.GrantKey)
			if findErr != nil {
				return findErr
			}
			if ok {
				if stack.Count != item.Stack.Count {
					return fmt.Errorf(
						"%w: grant=%q count=%d want=%d",
						ErrDirectContainerDropInvalid,
						item.GrantKey,
						stack.Count,
						item.Stack.Count,
					)
				}
				existing[index] = DirectContainerDropResultItem{Slot: slot, Stack: stack}
				existingDirect[index] = true
				directCount++
			}
			if directContainerDropMailContains(mailbox, item.GrantKey) {
				existingMail[index] = true
				mailCount++
			}
		}
		if directCount == len(cmd.Items) {
			result.Items = existing
			result.Replayed = true
			return nil
		}
		if mailCount == len(cmd.Items) {
			result.MailAttachments = len(cmd.Items)
			result.Replayed = true
			return nil
		}
		if directCount != 0 || mailCount != 0 {
			return fmt.Errorf(
				"%w: direct=%d mail=%d total=%d",
				ErrDirectContainerDropAmbiguous,
				directCount,
				mailCount,
				len(cmd.Items),
			)
		}

		freeSlots := directContainerDropFreeSlots(inventory, cmd.ListType, cmd.SlotStart, cmd.SlotEnd)
		if len(freeSlots) < len(cmd.Items) {
			attachments := make([]dnfrepo.MailAttachment, 0, len(cmd.Items))
			for _, item := range cmd.Items {
				stack := cloneDirectContainerDropStack(item.Stack)
				if stack.Extra == nil {
					stack.Extra = make(map[string]string, 1)
				}
				stack.Extra["overflow_mail"] = "true"
				attachments = append(attachments, dnfrepo.MailAttachment{
					ItemID:   stack.ItemID,
					Count:    stack.Count,
					Bind:     stack.Bind,
					ExpireAt: stack.ExpireAt,
					RawEntry: append([]byte(nil), stack.RawEntry...),
					Extra:    cloneDirectContainerDropExtra(stack.Extra),
				})
			}
			mailID, mailErr := dnfrepo.AppendSystemMail(ctx, mailboxes, dnfrepo.SystemMailDelivery{
				RecipientCharacterID: cmd.CharacterID,
				Title:                cmd.MailTitle,
				Body:                 cmd.MailBody,
				Source:               cmd.Source,
				CompletionKey:        cmd.CompletionKey,
				Attachments:          attachments,
				CreatedAt:            now,
			})
			if mailErr != nil {
				return mailErr
			}
			result.MailID = mailID
			result.MailAttachments = len(attachments)
			return nil
		}

		result.Items = make([]DirectContainerDropResultItem, 0, len(cmd.Items))
		for index, item := range cmd.Items {
			slot := freeSlots[index]
			stack, projectErr := cmd.Project(slot, cloneDirectContainerDropStack(item.Stack))
			if projectErr != nil {
				return projectErr
			}
			inventory.Slots[directContainerDropSlotKey(cmd.ListType, slot)] = stack
			result.Items = append(result.Items, DirectContainerDropResultItem{Slot: slot, Stack: stack})
		}
		inventory.UpdatedAt = now
		if err := dnfrepo.SaveInventoryFields(
			ctx,
			inventories,
			inventory,
			dnfrepo.InventoryFieldSlots,
		); err != nil {
			return err
		}
		return nil
	})
	if err != nil {
		return DirectContainerDropResult{}, err
	}
	return result, nil
}

const directContainerDropGrantKeyExtra = "direct_container_drop_grant_key"

func DirectContainerDropGrantKeyExtra() string {
	return directContainerDropGrantKeyExtra
}

func directContainerDropInventoryGrant(
	inventory dnfrepo.InventoryRecord,
	listType byte,
	grantKey string,
) (int16, dnfrepo.ItemStack, bool, error) {
	var foundSlot int16
	var foundStack dnfrepo.ItemStack
	found := false
	for key, stack := range inventory.Slots {
		parsedList, slot, ok := directContainerDropParseSlotKey(key)
		if !ok || parsedList != listType || stack.Count <= 0 ||
			stack.Extra[directContainerDropGrantKeyExtra] != grantKey {
			continue
		}
		if found {
			return 0, dnfrepo.ItemStack{}, false, fmt.Errorf(
				"%w: grant=%q appears more than once",
				ErrDirectContainerDropInvalid,
				grantKey,
			)
		}
		found = true
		foundSlot = slot
		foundStack = stack
	}
	return foundSlot, foundStack, found, nil
}

func directContainerDropMailContains(mailbox dnfrepo.MailboxRecord, grantKey string) bool {
	for _, mail := range mailbox.Mails {
		for _, attachment := range mail.Attachments {
			if attachment.Extra[directContainerDropGrantKeyExtra] == grantKey {
				return true
			}
		}
	}
	return false
}

func directContainerDropFreeSlots(
	inventory dnfrepo.InventoryRecord,
	listType byte,
	start int16,
	end int16,
) []int16 {
	slots := make([]int16, 0, int(end-start)+1)
	for slot := start; slot <= end; slot++ {
		stack, occupied := inventory.Slots[directContainerDropSlotKey(listType, slot)]
		if occupied && stack.ItemID > 0 && stack.Count > 0 {
			continue
		}
		slots = append(slots, slot)
	}
	return slots
}

func directContainerDropSlotKey(listType byte, slot int16) string {
	return fmt.Sprintf("%d:%d", listType, slot)
}

func directContainerDropParseSlotKey(key string) (byte, int16, bool) {
	var listType int
	var slot int
	if _, err := fmt.Sscanf(key, "%d:%d", &listType, &slot); err != nil ||
		listType < 0 || listType > 255 || slot < 0 || slot > 32767 {
		return 0, 0, false
	}
	return byte(listType), int16(slot), true
}

func cloneDirectContainerDropStack(stack dnfrepo.ItemStack) dnfrepo.ItemStack {
	stack.RawEntry = append([]byte(nil), stack.RawEntry...)
	stack.Extra = cloneDirectContainerDropExtra(stack.Extra)
	return stack
}

func cloneDirectContainerDropExtra(in map[string]string) map[string]string {
	if len(in) == 0 {
		return nil
	}
	out := make(map[string]string, len(in))
	for key, value := range in {
		out[key] = value
	}
	return out
}
