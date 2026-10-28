package repository

import (
	"context"
	"errors"
	"math"
	"strconv"
	"strings"
	"time"
)

var ErrSystemMailInvalid = errors.New("system mailbox delivery is invalid")

// SystemMailDelivery is a server-owned, non-expiring mail created by a
// committed gameplay transaction. It deliberately has no player postage;
// callers must supply transaction-scoped MailboxRepository instances so reward
// consumption and the durable mail commit together.
type SystemMailDelivery struct {
	RecipientCharacterID string
	RecipientName        string
	Title                string
	Body                 string
	Source               string
	CompletionKey        string
	Gold                 int64
	Attachments          []MailAttachment
	CreatedAt            time.Time
	// SenderCharacterID / SenderName 是**可选**的来源署名。
	//
	// 留空 = 系统邮件（寄件人显示"系统"），既有调用方一行都不用改。
	// 商城赠送要填：收件人得看得出来这封礼物是谁送的。
	// SenderName 只用于展示，绝不参与任何权限判定。
	SenderCharacterID string
	SenderName        string
}

// systemMailSenderName 是系统邮件的默认署名（寄件人显示"系统"）。
// 与 AppendSystemMail 改动前的字面量完全一致：既有系统邮件（礼包溢出、GM 发信）
// 的寄件人显示不能因为这次改动发生变化。
const systemMailSenderName = "系统"

// AppendSystemMail appends one non-expiring system mail to a character's
// mailbox. It is safe to call only through the transaction supplied by the
// relevant asset owner; this helper owns no transaction itself.
func AppendSystemMail(ctx context.Context, repo MailboxRepository, delivery SystemMailDelivery) (string, error) {
	recipientID := strings.TrimSpace(delivery.RecipientCharacterID)
	if repo == nil || recipientID == "" || strings.TrimSpace(delivery.Source) == "" || delivery.Gold < 0 ||
		(delivery.Gold == 0 && len(delivery.Attachments) == 0 && strings.TrimSpace(delivery.Title) == "" && strings.TrimSpace(delivery.Body) == "") {
		return "", ErrSystemMailInvalid
	}
	attachments := cloneMailAttachments(delivery.Attachments)
	for _, attachment := range attachments {
		if attachment.ItemID <= 0 || attachment.Count <= 0 {
			return "", ErrSystemMailInvalid
		}
	}

	record, found, err := repo.Load(ctx, recipientID)
	if err != nil {
		return "", err
	}
	if !found {
		record = MailboxRecord{CharacterID: recipientID}
	}
	if record.Mails == nil {
		record.Mails = make(map[string]MailRecord)
	}
	completionKey := strings.TrimSpace(delivery.CompletionKey)
	if completionKey != "" {
		for mailID, existing := range record.Mails {
			if strings.TrimSpace(existing.Metadata["completion_key"]) != completionKey {
				continue
			}
			if existing.RecipientCharacterID != recipientID || existing.Gold != delivery.Gold ||
				strings.TrimSpace(existing.Metadata["source"]) != strings.TrimSpace(delivery.Source) {
				return "", ErrSystemMailInvalid
			}
			return mailID, nil
		}
	}
	mailID, err := nextSystemMailID(record.Mails)
	if err != nil {
		return "", err
	}
	now := delivery.CreatedAt.UTC()
	if now.IsZero() {
		now = time.Now().UTC()
	}
	// 署名：留空 → 系统（与改动前逐字节一致）；商城赠送填赠送方，收件人看得出是谁送的。
	senderCharacterID := strings.TrimSpace(delivery.SenderCharacterID)
	if senderCharacterID == "" {
		senderCharacterID = "0"
	}
	senderName := strings.TrimSpace(delivery.SenderName)
	if senderName == "" {
		senderName = systemMailSenderName
	}
	record.Mails[mailID] = MailRecord{
		MailID:               mailID,
		SenderCharacterID:    senderCharacterID,
		SenderName:           senderName,
		RecipientCharacterID: recipientID,
		RecipientName:        strings.TrimSpace(delivery.RecipientName),
		Title:                strings.TrimSpace(delivery.Title),
		Body:                 strings.TrimSpace(delivery.Body),
		Gold:                 delivery.Gold,
		Attachments:          attachments,
		CreatedAt:            now,
		// A system overflow delivery must remain claimable until the player
		// makes room. A zero expiry is the repository's durable non-expiring
		// representation and is projected as such by the mailbox owner.
		Metadata: map[string]string{
			"source":         strings.TrimSpace(delivery.Source),
			"system_mail":    "true",
			"unlimited":      "true",
			"completion_key": completionKey,
		},
	}
	record.UpdatedAt = now
	if err := SaveMailboxFields(ctx, repo, record, MailboxFieldMails); err != nil {
		return "", err
	}
	return mailID, nil
}

func nextSystemMailID(mails map[string]MailRecord) (string, error) {
	var max uint64
	for key := range mails {
		value, err := strconv.ParseUint(strings.TrimSpace(key), 10, 32)
		if err != nil {
			continue
		}
		if value > max {
			max = value
		}
	}
	if max >= math.MaxUint32 {
		return "", ErrSystemMailInvalid
	}
	return strconv.FormatUint(max+1, 10), nil
}
