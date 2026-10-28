package mail

import (
	"context"
	"fmt"
	"math"
	"strings"

	dnfrepo "longheng.io/server/internal/modules/dnf/repository"
)

// SystemSendRequest is a trusted server-side mailbox delivery. Unlike the
// player op94/op315 flow it has no sender postage or source inventory, but it
// still commits through the mailbox asset unit of work used by both MySQL and
// SQLite repositories.
type SystemSendRequest struct {
	RecipientCharacterID string
	RecipientName        string
	Title                string
	Body                 string
	Gold                 int64
	Attachments          []dnfrepo.MailAttachment
	Source               string
	CompletionKey        string
}

type SystemSendResult struct {
	MailID               string
	RecipientCharacterID string
	RecipientName        string
}

// SendSystem validates a trusted delivery and appends it atomically to the
// authoritative mailbox aggregate. Item definitions and wire rows must already
// have been resolved by the bridge from the active runtime PVF.
func (o Owner) SendSystem(ctx context.Context, req SystemSendRequest) (SystemSendResult, error) {
	if o.repos.Character == nil {
		return SystemSendResult{}, ErrMailboxCharacterMissing
	}
	if o.repos.Mailbox == nil {
		return SystemSendResult{}, ErrMailboxRepoMissing
	}
	if o.repos.MailboxAssets == nil {
		return SystemSendResult{}, ErrMailboxAssetsUOW
	}
	req.RecipientCharacterID = strings.TrimSpace(req.RecipientCharacterID)
	req.RecipientName = strings.TrimSpace(req.RecipientName)
	req.Title = strings.TrimSpace(req.Title)
	req.Body = strings.TrimSpace(req.Body)
	req.Source = strings.TrimSpace(req.Source)
	if req.RecipientCharacterID == "" && req.RecipientName == "" {
		return SystemSendResult{}, ErrMailboxRecipientMissing
	}
	if req.Source == "" || req.Gold < 0 || req.Gold > math.MaxUint32 || len(req.Attachments) > maxMailboxAttachments ||
		(req.Gold == 0 && len(req.Attachments) == 0 && req.Title == "" && req.Body == "") {
		return SystemSendResult{}, ErrMailboxInvalidRequest
	}
	if req.Title != "" {
		if _, err := encodeMailboxText(req.Title, maxMailboxMessageBytes); err != nil {
			return SystemSendResult{}, fmt.Errorf("%w: title: %v", ErrMailboxInvalidRequest, err)
		}
	}
	if req.Body != "" {
		if _, err := encodeMailboxText(req.Body, maxMailboxMessageBytes); err != nil {
			return SystemSendResult{}, fmt.Errorf("%w: body: %v", ErrMailboxInvalidRequest, err)
		}
	}
	for index, attachment := range req.Attachments {
		if attachment.ItemID <= 0 || attachment.ItemID > math.MaxUint32 ||
			attachment.Count <= 0 || attachment.Count > math.MaxUint32 {
			return SystemSendResult{}, fmt.Errorf("%w: attachment=%d", ErrMailboxInvalidRequest, index)
		}
	}

	recipientID := req.RecipientCharacterID
	if recipientID == "" {
		var found bool
		var err error
		recipientID, found, err = o.repos.Character.FindIDByName(ctx, req.RecipientName)
		if err != nil {
			return SystemSendResult{}, err
		}
		if !found {
			return SystemSendResult{}, ErrMailboxRecipientMissing
		}
	}

	var result SystemSendResult
	err := o.repos.MailboxAssets.WithinMailboxAssets(ctx, recipientID, recipientID, func(
		_ dnfrepo.AccountRepository,
		_ dnfrepo.AccountInventoryRepository,
		characters dnfrepo.CharacterRepository,
		_ dnfrepo.InventoryRepository,
		mailboxes dnfrepo.MailboxRepository,
	) error {
		recipient, found, err := characters.Load(ctx, recipientID)
		if err != nil {
			return err
		}
		if !found || recipient.Stats["delete_flag"] != 0 {
			return ErrMailboxRecipientMissing
		}
		recipientName := strings.TrimSpace(recipient.Name)
		if recipientName == "" || (req.RecipientName != "" && recipientName != req.RecipientName) {
			return ErrMailboxRecipientMissing
		}
		mailID, err := dnfrepo.AppendSystemMail(ctx, mailboxes, dnfrepo.SystemMailDelivery{
			RecipientCharacterID: recipientID,
			RecipientName:        recipientName,
			Title:                req.Title,
			Body:                 req.Body,
			Source:               req.Source,
			CompletionKey:        strings.TrimSpace(req.CompletionKey),
			Gold:                 req.Gold,
			Attachments:          req.Attachments,
		})
		if err != nil {
			return err
		}
		result = SystemSendResult{
			MailID:               mailID,
			RecipientCharacterID: recipientID,
			RecipientName:        recipientName,
		}
		return nil
	})
	if err != nil {
		return SystemSendResult{}, err
	}
	return result, nil
}
