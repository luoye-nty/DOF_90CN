package dnfbridge

import (
	"errors"
	"math"

	dnfrepo "longheng.io/server/internal/modules/dnf/repository"
)

var (
	errCurrentOverflowMailRowInvalid = errors.New("current overflow refund row invalid")
	// currentOverflowMailMaxRows caps one row's expansion so a corrupt stack
	// limit cannot build an unbounded letter.
	currentOverflowMailMaxRows = 32
)

// currentOverflowMailAttachments turns one reward row the bag refused into
// mail attachments, splitting a count beyond the template's stack limit across
// rows. The stack goes through buildCurrentDungeonPickupStack so the
// attachment carries the exact item-instance payload and PVF expiration rather
// than a bare template id -- a zero instance timestamp reads as a retired
// event deadline when the letter is opened.
//
// source names the grant for the item's extra map, which is how these rows are
// told apart later from ordinary drops.
func currentOverflowMailAttachments(
	catalog *pvfDungeonDropCatalog,
	itemID uint32,
	count int64,
	source string,
) ([]dnfrepo.MailAttachment, error) {
	if catalog == nil || itemID == 0 || count <= 0 || source == "" {
		return nil, errCurrentOverflowMailRowInvalid
	}
	definition, err := catalog.ResolveItem(itemID)
	if err != nil {
		return nil, err
	}
	attachments := make([]dnfrepo.MailAttachment, 0, 1)
	remaining := count
	for remaining > 0 {
		chunk := remaining
		switch definition.Kind {
		case dungeonDropItemEquipment:
			chunk = 1
		case dungeonDropItemStackable:
			if definition.StackLimit > 0 && chunk > definition.StackLimit {
				chunk = definition.StackLimit
			}
		default:
			return nil, errCurrentOverflowMailRowInvalid
		}
		if chunk <= 0 || chunk > math.MaxUint32 {
			return nil, errCurrentOverflowMailRowInvalid
		}
		stack, buildErr := buildCurrentDungeonPickupStack(definition.SlotStart, definition, uint32(chunk))
		if buildErr != nil {
			return nil, buildErr
		}
		if stack.Extra == nil {
			stack.Extra = make(map[string]string, 3)
		}
		stack.Extra["source"] = source
		stack.Extra["last_grant_source"] = source
		stack.Extra["overflow_mail"] = "true"
		attachments = append(attachments, dnfrepo.MailAttachment{
			ItemID:   stack.ItemID,
			Count:    stack.Count,
			Bind:     stack.Bind,
			ExpireAt: stack.ExpireAt,
			RawEntry: stack.RawEntry,
			Extra:    stack.Extra,
		})
		remaining -= chunk
		if len(attachments) > currentOverflowMailMaxRows {
			return nil, errCurrentOverflowMailRowInvalid
		}
	}
	return attachments, nil
}
