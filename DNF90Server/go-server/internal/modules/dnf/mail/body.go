package mail

import (
	"encoding/binary"
	"encoding/hex"
	"fmt"
	"math"
	"strconv"
	"strings"
	"time"

	"golang.org/x/text/encoding/simplifiedchinese"

	"longheng.io/server/internal/modules/dnf/alignedcmd"
	dnfcerashop "longheng.io/server/internal/modules/dnf/cerashop"
	dnfproto "longheng.io/server/internal/modules/dnf/protocol"
	dnfrepo "longheng.io/server/internal/modules/dnf/repository"
)

const (
	MailboxListNotificationMessageID   uint16 = 0x0061
	mailboxListNotificationMessageID          = MailboxListNotificationMessageID
	mailboxRemoveNotificationMessageID uint16 = 0x0062
	MailboxAlarmNotificationMessageID  uint16 = 0x0063
	// Current NoPack.exe registers class0/op718 beside mailbox op97/op98/op99
	// notifications as the response that fills IDC_TREE_MYCHARACTER after
	// class1/op789 requests a server node.
	mailboxRecipientListMessageID    uint16 = 718
	mailboxRecipientListNameMaxBytes        = 29
	mailboxItemRawSize                      = 0x77
	// The two avatar extension blocks the current client reads after the fixed
	// item row target a 30-byte jewel socket and a single colour DWORD, the same
	// pair the list-1 item-list rows already carry.
	mailboxAvatarSocketBytes = 30
	mailboxAvatarColorBytes  = 4
)

// BuildListNotification creates the complete clear-first class0/op97 mailbox
// snapshot used by an open mailbox and by trusted live GM deliveries.
func BuildListNotification(result OpenResult) ([]byte, error) {
	return buildMailboxListNotification(result.Mails, result.NotLoaded, result.ObservedAt)
}

func class1Response(opcode uint16, body []byte) alignedcmd.UpperResponse {
	return alignedcmd.UpperResponse{
		MsgID:          opcode,
		Body:           append([]byte(nil), body...),
		Classification: dnfproto.DefaultChannelClassification,
		AllowCodec:     true,
	}
}

func class0Response(messageID uint16, body []byte) alignedcmd.UpperResponse {
	return alignedcmd.UpperResponse{
		MsgID:          messageID,
		Body:           append([]byte(nil), body...),
		Classification: 0,
		AllowCodec:     true,
	}
}

func buildMailboxOpenAck(total int) []byte {
	if total < 0 {
		total = 0
	}
	if total > 0xffff {
		total = 0xffff
	}
	body := []byte{0x01, 0x00, 0x00}
	binary.LittleEndian.PutUint16(body[1:], uint16(total))
	return body
}

func buildSuccessAck() []byte {
	return []byte{0x01}
}

// buildExtractAck writes the op95 acknowledgement: success flag, u32 result
// count, then three u32 per result.
//
// sub_1D225F0 collects those triples and hands them to sub_27156D0, which reads
// them as (letter id, claim object, mode):
//
//   - the first dword is the container key, so it must be the letter id.
//   - the second dword is passed to sub_2708220(entry, claimObject), which scans
//     the entry's claim-object vector at +0x70 and returns the paired item object
//     at +0x84. It has to be the same claim object the summary row gave that
//     slot; with the letter id in both places a multi-attachment letter only ever
//     retires its first slot, and a zero here never matches at all.
//   - the third dword's low word must stay zero; a non-zero value diverts to
//     sub_27154E0 instead of the normal extraction path.
//
// A key or claim object that does not resolve is harmless - the applier just
// advances to the next triple - so a stale row can never fault the client.
func buildExtractAck(claimed []ClaimedAttachment) []byte {
	body := make([]byte, 1+4+len(claimed)*12)
	body[0] = 0x01
	binary.LittleEndian.PutUint32(body[1:5], uint32(len(claimed)))
	offset := 5
	for _, row := range claimed {
		binary.LittleEndian.PutUint32(body[offset:offset+4], row.LetterID)
		binary.LittleEndian.PutUint32(body[offset+4:offset+8], row.ClaimObject)
		offset += 12
	}
	return body
}

func buildChangeStateAck(mailIDs []uint32, status uint16) []byte {
	body := make([]byte, 1+4+len(mailIDs)*6)
	body[0] = 0x01
	binary.LittleEndian.PutUint32(body[1:5], uint32(len(mailIDs)))
	offset := 5
	for _, mailID := range mailIDs {
		binary.LittleEndian.PutUint32(body[offset:offset+4], mailID)
		binary.LittleEndian.PutUint16(body[offset+4:offset+6], status)
		offset += 6
	}
	return body
}

func buildQueryCharacterAck(nameRaw []byte, grow byte, level uint16, job byte) []byte {
	body := make([]byte, 0, 1+4+len(nameRaw)+8)
	body = append(body, 0x01)
	var scratch [4]byte
	binary.LittleEndian.PutUint32(scratch[:], uint32(len(nameRaw)))
	body = append(body, scratch[:]...)
	body = append(body, nameRaw...)
	body = append(body, grow)
	var levelBytes [2]byte
	binary.LittleEndian.PutUint16(levelBytes[:], level)
	body = append(body, levelBytes[:]...)
	// sub_1D0BAD0 reads job plus four trailing state bytes after level. The
	// 1 in the third state field matches the handler's initialized normal state.
	body = append(body, job, 0x00, 0x01, 0x00, 0x00)
	return body
}

func buildRecipientCharacterListBody(serverID byte, characters []dnfrepo.CharacterRecord, selectedCharacterID uint16) ([]byte, error) {
	writer := mailboxBodyWriter{}
	writer.u8(serverID)
	writer.u8(0)
	count := 0
	for _, character := range characters {
		if count == math.MaxUint8 {
			break
		}
		characterID, err := strconv.ParseUint(strings.TrimSpace(character.CharacterID), 10, 32)
		if err != nil || characterID == 0 {
			return nil, fmt.Errorf("%w: invalid mailbox account-role character id %q", ErrMailboxInvalidRequest, character.CharacterID)
		}
		if uint32(characterID) == uint32(selectedCharacterID) {
			continue
		}
		name := strings.TrimSpace(character.Name)
		if name == "" {
			return nil, fmt.Errorf("%w: mailbox account-role character %d has no name", ErrMailboxInvalidRequest, characterID)
		}
		nameRaw, err := encodeMailboxText(name, mailboxRecipientListNameMaxBytes)
		if err != nil {
			return nil, fmt.Errorf("%w: mailbox account-role name %q: %v", ErrMailboxInvalidRequest, name, err)
		}
		writer.u32(uint32(characterID))
		writer.u32(uint32(len(nameRaw)))
		writer.raw(nameRaw)
		writer.u8(0)
		count++
	}
	writer.data[1] = byte(count)
	return writer.bytes(), nil
}

func buildErrorAck(code byte) []byte {
	if code == 0 {
		code = 1
	}
	return []byte{0x00, code}
}

// buildMailboxListNotification follows the current client's
// sub_1D913C0 reader exactly: attachment/gold summaries, the deferred-page
// count, then the detailed text/status rows. The common 0x77 item row is
// deliberately retained as a bounded opaque client item row; its front fields
// are projected separately because the client reads both representations.
func buildMailboxListNotification(mails []dnfrepo.MailRecord, notLoaded int, now time.Time) ([]byte, error) {
	if len(mails) > math.MaxUint16 {
		return nil, fmt.Errorf("%w: mailbox page has %d mails", ErrMailboxInvalidRequest, len(mails))
	}

	summaries := make([]mailboxSummary, 0, len(mails))
	for _, durableMail := range mails {
		mail := mailboxPresentationMail(durableMail)
		mailID, err := mailboxWireID(mail.MailID)
		if err != nil {
			return nil, err
		}
		wrote := false
		for index, attachment := range mail.Attachments {
			// An empty slot is one that has already been handed over. Skip it
			// instead of rejecting the page: one unusable row must never cost the
			// player the whole mailbox, which is exactly what happened when such
			// rows reached this builder and every op96 answered
			// "mailbox snapshot unavailable".
			if attachment.ItemID <= 0 || attachment.Count <= 0 {
				continue
			}
			if len(summaries) >= math.MaxUint8 {
				return nil, fmt.Errorf("%w: mailbox page has more than %d attachment summaries", ErrMailboxInvalidRequest, math.MaxUint8)
			}
			summaries = append(summaries, mailboxSummary{
				mail:        mail,
				mailID:      mailID,
				claimObject: mailboxSlotClaimObject(mailID, index),
				attachment:  &attachment,
				includeGold: !wrote,
				// A wallet-currency voucher is money the client has to print:
				// its cell's amount comes from this slot, not from the item row's
				// count (see mailboxAttachmentWireAmount).
				amount: mailboxAttachmentWireAmount(attachment),
			})
			wrote = true
		}
		if !wrote && mail.Gold > 0 {
			if len(summaries) >= math.MaxUint8 {
				return nil, fmt.Errorf("%w: mailbox page has more than %d summaries", ErrMailboxInvalidRequest, math.MaxUint8)
			}
			summaries = append(summaries, mailboxSummary{mail: mail, mailID: mailID, claimObject: mailID, includeGold: true})
			wrote = true
		}
		// The client keeps the current message identity from the summary pass.
		// A text-only mail therefore needs one intentionally empty seed record.
		if !wrote {
			if len(summaries) >= math.MaxUint8 {
				return nil, fmt.Errorf("%w: mailbox page has more than %d summaries", ErrMailboxInvalidRequest, math.MaxUint8)
			}
			summaries = append(summaries, mailboxSummary{mail: mail, mailID: mailID, claimObject: mailID, seedOnly: true})
		}
	}

	writer := mailboxBodyWriter{}
	writer.u8(byte(len(summaries)))
	// Zero makes sub_1D913C0 clear both current mail containers before this
	// complete snapshot; subsequent page batches would use one here.
	writer.u8(0)
	for _, summary := range summaries {
		if err := writer.writeSummary(summary, now); err != nil {
			return nil, err
		}
	}
	writer.u16(clampMailboxU16(notLoaded))
	writer.u16(uint16(len(mails)))
	for _, durableMail := range mails {
		mail := mailboxPresentationMail(durableMail)
		if err := writer.writeDetail(mail); err != nil {
			return nil, err
		}
	}
	return writer.bytes(), nil
}

// mailboxPresentationMail projects old full-inventory system mail with the
// current Chinese copy. Claimed mail is always projected as an empty, read
// receipt; this also protects the UI while an older durable row is being
// repaired by Owner.Open.
func mailboxPresentationMail(mail dnfrepo.MailRecord) dnfrepo.MailRecord {
	switch strings.TrimSpace(mail.Metadata["source"]) {
	case "magic_box_reward_inventory_full":
		mail.SenderName = "系统"
		mail.Title = "背包已满：礼盒奖励"
		mail.Body = "背包空间不足，礼盒奖励已通过邮件发送。请清理对应道具分页后领取。"
	case "dungeon_card_reward_inventory_full":
		mail.SenderName = "系统"
		mail.Title = "背包已满：通关奖励"
		mail.Body = "背包空间不足，通关奖励已通过邮件发送。请清理对应道具分页后领取。"
	}
	if mail.Claimed {
		mail.Gold = 0
		mail.Attachments = nil
	}
	return mail
}

func buildMailboxRemoveNotification(mailIDs []uint32) []byte {
	writer := mailboxBodyWriter{}
	writer.u32(uint32(len(mailIDs)))
	for _, mailID := range mailIDs {
		writer.u32(mailID)
	}
	return writer.bytes()
}

// buildMailboxAlarmNotification is the exact one-WORD class0/0x63 body. It
// is kept here for the session directory path, which may notify an online
// recipient independently from the sender's command response.
func buildMailboxAlarmNotification(count int) []byte {
	writer := mailboxBodyWriter{}
	writer.u16(clampMailboxU16(count))
	return writer.bytes()
}

// BuildAlarmNotification produces the current class0/0x63 one-WORD payload
// used when a recipient already has an active game session.
func BuildAlarmNotification(count int) []byte {
	return buildMailboxAlarmNotification(count)
}

type mailboxSummary struct {
	mail        dnfrepo.MailRecord
	mailID      uint32
	claimObject uint32
	attachment  *dnfrepo.MailAttachment
	includeGold bool
	// amount is the money this one attachment cell is worth, for the attachment
	// kinds the client renders as a number. The letter's own gold wins when this
	// row carries it (includeGold).
	amount   uint32
	seedOnly bool
}

// mailboxAttachmentWireAmount is the amount the client prints on one attachment
// cell. The current op0x61 reader takes it from the summary's gold slot, NOT
// from the item row's count:
//
//	FUN_034e3a50(&gold);     ; the summary's amount slot
//	FUN_034e3a50(&itemID);
//	if (itemID - 0x28eb7dU < 9)  gold = (itemID - 0x28eb7c) * 1000000;   ; gold_1m..gold_9m
//	else if (itemID - 0x28eb86U < 3) gold = (itemID - 0x28eb85) * 10000000; ; gold_10m..30m
//	...
//	FUN_0270bfb0(0, claim, .., gold, ..);    ; that value becomes the row's amount
//
// so a gold certificate is priced by the client itself from its item id, while
// every other cell shows whatever this slot carried. A 代币券 attachment is money
// too - it is credited to the account wallet on claim - and with a zero here the
// seller's sale letter showed an icon and "0" next to it.
func mailboxAttachmentWireAmount(attachment dnfrepo.MailAttachment) uint32 {
	// VoucherKindOf is the same predicate the claim path uses to route these
	// attachments to the account wallet, so presentation and payment can never
	// disagree about which items are money.
	if dnfcerashop.VoucherKindOf(attachment.ItemID) == dnfcerashop.VoucherNone {
		return 0
	}
	if attachment.Count <= 0 || attachment.Count > math.MaxUint32 {
		return 0
	}
	return uint32(attachment.Count)
}

// mailboxMaxAttachmentsPerMail is 86JP's per-letter attachment cap, so four bits
// address every slot of a letter.
const mailboxMaxAttachmentsPerMail = 16

// MailboxAttachmentSlotFlag marks a claim object that addresses one attachment
// slot instead of the whole letter.
//
// 86JP uses 0x40000000 (MailboxRepository.AttachmentClaimFlag) and documents the
// contract this depends on: the client merges summary rows by the letter id near
// the end of the row and gives every distinct leading dword its own attachment
// slot. Writing the letter id in both fields is what collapses a letter into one
// visible item. The flag itself is moved below the reference's value because the
// client faulted in its op95 handler while ids above 0x40000000 were on the wire;
// real mail ids stay far below this range, so an encoded slot can never be
// mistaken for a letter.
const MailboxAttachmentSlotFlag uint32 = 0x10000000

// MailboxAttachmentClaimObject encodes one attachment slot: the flag, the letter
// id, then the slot index in the low four bits.
func MailboxAttachmentClaimObject(mailID uint32, index int) (uint32, error) {
	if index < 0 || index >= mailboxMaxAttachmentsPerMail {
		return 0, fmt.Errorf("%w: attachment index %d is outside 0..%d", ErrMailboxInvalidRequest, index, mailboxMaxAttachmentsPerMail-1)
	}
	if mailID == 0 || mailID > (MailboxAttachmentSlotFlag-1)/mailboxMaxAttachmentsPerMail {
		return 0, fmt.Errorf("%w: mail id %d cannot carry an attachment claim object", ErrMailboxInvalidRequest, mailID)
	}
	return MailboxAttachmentSlotFlag | (mailID * mailboxMaxAttachmentsPerMail) | uint32(index), nil
}

// mailboxSlotClaimObject is the addressing used on the wire. A slot that cannot
// be encoded - past the sixteenth attachment, or a letter id too large to carry
// an index - falls back to the letter id instead of failing. Such a row then
// merges into the letter's first slot exactly as it did before slots existed,
// which costs that one row its own icon; failing here would cost the player the
// whole mailbox page.
func mailboxSlotClaimObject(mailID uint32, index int) uint32 {
	claimObject, err := MailboxAttachmentClaimObject(mailID, index)
	if err != nil {
		return mailID
	}
	return claimObject
}

// DecodeMailboxClaimObject splits one op95 entry. A value below the flag still
// addresses the whole letter, which is what the client sends for a gold-only or
// text-only row and what every older client build sends.
func DecodeMailboxClaimObject(claimObject uint32) (mailID uint32, attachmentIndex int, singleSlot bool) {
	if claimObject < MailboxAttachmentSlotFlag {
		return claimObject, 0, false
	}
	value := claimObject &^ MailboxAttachmentSlotFlag
	return value / mailboxMaxAttachmentsPerMail, int(value % mailboxMaxAttachmentsPerMail), true
}

type mailboxBodyWriter struct{ data []byte }

func (w *mailboxBodyWriter) u8(value byte) { w.data = append(w.data, value) }

func (w *mailboxBodyWriter) u16(value uint16) {
	var encoded [2]byte
	binary.LittleEndian.PutUint16(encoded[:], value)
	w.data = append(w.data, encoded[:]...)
}

func (w *mailboxBodyWriter) u32(value uint32) {
	var encoded [4]byte
	binary.LittleEndian.PutUint32(encoded[:], value)
	w.data = append(w.data, encoded[:]...)
}

func (w *mailboxBodyWriter) raw(value []byte) { w.data = append(w.data, value...) }

// optionalBlob writes the current client's `u32 length + payload` extension
// block (sub_3457C50: one sub_34E3A50 length followed by a sub_34E3AC0 read of
// exactly that many bytes). An absent payload is written as a zero length, which
// leaves the client's freshly constructed target object at its defaults.
func (w *mailboxBodyWriter) optionalBlob(data []byte) {
	if len(data) == 0 || len(data) > math.MaxUint32 {
		w.u32(0)
		return
	}
	w.u32(uint32(len(data)))
	w.raw(data)
}

func (w *mailboxBodyWriter) dstring(value string, maxBytes int) error {
	raw, err := encodeMailboxText(value, maxBytes)
	if err != nil {
		return err
	}
	w.u32(uint32(len(raw)))
	w.raw(raw)
	return nil
}

func (w *mailboxBodyWriter) writeSummary(summary mailboxSummary, now time.Time) error {
	senderID, err := mailboxSenderWireID(summary.mail.SenderCharacterID)
	if err != nil {
		return err
	}
	// First dword: the claim object, which must be distinct per attachment.
	//
	// The client keys mailbox entries by the letter id at the end of this row and
	// merges same-letter rows through sub_2709990. That merge keeps two parallel
	// vectors on the entry - claim objects at +0x70 and their item objects at
	// +0x84 - and appends the pair only after scanning +0x70 for the incoming
	// claim object: on a hit it aborts the whole merge (`xor al,al; ret`). Writing
	// the letter id into every row of a letter therefore collapses it to a single
	// visible attachment, which is exactly the "one item per mail" defect.
	w.u32(summary.claimObject)
	w.u32(senderID)
	if err := w.dstring(summary.mail.SenderName, maxMailboxRecipientBytes); err != nil {
		return fmt.Errorf("sender name: %w", err)
	}
	// This slot is the cell's amount as far as the client is concerned - it
	// prints it beside the cell's icon and never reads the item row's count for
	// it (see mailboxAttachmentWireAmount). A row that owns the letter's gold
	// shows that gold; every other row shows its own amount, and only the
	// wallet-currency vouchers have one.
	amount := summary.amount
	if summary.includeGold {
		if summary.mail.Gold < 0 || summary.mail.Gold > math.MaxUint32 {
			return fmt.Errorf("%w: mail=%d gold=%d", ErrMailboxInvalidRequest, summary.mailID, summary.mail.Gold)
		}
		if summary.mail.Gold > 0 {
			amount = uint32(summary.mail.Gold)
		}
	}
	w.u32(amount)

	if summary.attachment == nil {
		w.u32(0)
		w.u8(0)
		w.u32(0)
		w.u16(0)
		w.u8(0)
		w.u32(0)
		w.u8(0)
		w.u8(0)
		w.u16(0)
		w.raw(make([]byte, mailboxItemRawSize))
	} else {
		attachment := *summary.attachment
		if attachment.ItemID > math.MaxUint32 || attachment.Count > math.MaxUint32 {
			return fmt.Errorf("%w: attachment item/count outside wire range", ErrMailboxInvalidRequest)
		}
		raw := mailboxAttachmentRaw(attachment, now)
		w.u32(uint32(attachment.ItemID))
		w.u8(1)
		w.u32(binary.LittleEndian.Uint32(raw[6:10]))
		w.u16(binary.LittleEndian.Uint16(raw[0x0b:0x0d]))
		w.u8(raw[0x0d])
		w.u32(binary.LittleEndian.Uint32(raw[0x0e:0x12]))
		w.u8(raw[0x12])
		w.u8(raw[0x13])
		w.u16(binary.LittleEndian.Uint16(raw[0x14:0x16]))
		if mailboxAttachmentRequiresClass26(attachment) {
			// The current type-26 branch consumes this DWORD and its detail flag
			// before the fixed item row. We have no durable creature-detail UOW,
			// so the explicit zero flag requests only the proven generic item
			// projection instead of fabricating the three detail dwords.
			w.u32(0)
			w.u8(0)
		}
		w.raw(raw)
		if mailboxAttachmentRequiresAvatarBlobs(attachment) {
			// Current sub_1D913C0 keeps reading after the fixed 0x77 row when
			// the attachment's item-table type is 0..11: sub_229C570 gates the
			// avatar branch on that range and calls sub_3457C50 for the jewel
			// socket, then sub_225C9B0(.., itemID, 3) re-tests the same range
			// through sub_229ACD0 and reads the colour block. Both are the
			// `u32 length + payload` extension already written for list-1 item
			// rows, and a zero length keeps the client's constructed defaults.
			w.optionalBlob(mailboxAttachmentFixedHexBlob(
				attachment,
				mailboxAvatarSocketBytes,
				"avatar_socket_data", "reserved2", "jewel_socket", "jewelSocket",
			))
			w.optionalBlob(mailboxAttachmentFixedHexBlob(
				attachment,
				mailboxAvatarColorBytes,
				"avatar_color_data", "color_data", "tail_data", "tailData",
			))
		}
	}
	w.u32(mailboxRemainingSeconds(summary.mail, now))
	if summary.seedOnly {
		w.u32(0)
	} else {
		w.u32(summary.mailID)
	}
	w.u8(mailboxMailType(summary.mail))
	return nil
}

func (w *mailboxBodyWriter) writeDetail(mail dnfrepo.MailRecord) error {
	mailID, err := mailboxWireID(mail.MailID)
	if err != nil {
		return err
	}
	senderID, err := mailboxSenderWireID(mail.SenderCharacterID)
	if err != nil {
		return err
	}
	w.u32(mailID)
	w.u32(senderID)
	if err := w.dstring(mail.SenderName, maxMailboxRecipientBytes); err != nil {
		return fmt.Errorf("sender name: %w", err)
	}
	if err := w.dstring(mail.Body, maxMailboxMessageBytes); err != nil {
		return fmt.Errorf("body: %w", err)
	}
	w.u32(mailboxUnix(mail.CreatedAt))
	w.u16(mailboxLetterStat(mail))
	w.u8(mailboxMailType(mail))
	return nil
}

func (w mailboxBodyWriter) bytes() []byte { return append([]byte(nil), w.data...) }

func mailboxAttachmentRaw(attachment dnfrepo.MailAttachment, now time.Time) []byte {
	raw := make([]byte, mailboxItemRawSize)
	if len(attachment.RawEntry) == mailboxItemRawSize {
		copy(raw, attachment.RawEntry)
	}
	binary.LittleEndian.PutUint16(raw[0:2], 0)
	binary.LittleEndian.PutUint32(raw[2:6], uint32(attachment.ItemID))
	binary.LittleEndian.PutUint32(raw[6:10], uint32(attachment.Count))
	if attachment.Bind && raw[0x0d] == 0 {
		raw[0x0d] = 1
	}
	if !attachment.ExpireAt.IsZero() && attachment.ExpireAt.Unix() > 0 {
		binary.LittleEndian.PutUint32(raw[0x38:0x3c], mailboxUnix(attachment.ExpireAt))
		if mailboxAttachmentIsStackable(attachment) {
			binary.LittleEndian.PutUint16(raw[0x0b:0x0d], mailboxAttachmentRemainingUseSeconds(attachment.ExpireAt, now))
		}
	}
	return raw
}

func mailboxAttachmentIsStackable(attachment dnfrepo.MailAttachment) bool {
	return strings.EqualFold(strings.TrimSpace(attachment.Extra["item_kind"]), "stackable") ||
		strings.TrimSpace(attachment.Extra["stackable_type"]) != ""
}

func mailboxAttachmentRemainingUseSeconds(expiresAt time.Time, now time.Time) uint16 {
	if expiresAt.IsZero() || now.IsZero() || !expiresAt.After(now) {
		return 0
	}
	remaining := expiresAt.Sub(now)
	seconds := int64(remaining / time.Second)
	if remaining%time.Second != 0 {
		seconds++
	}
	if seconds > math.MaxUint16 {
		return math.MaxUint16
	}
	return uint16(seconds)
}

func mailboxAttachmentRequiresClass26(attachment dnfrepo.MailAttachment) bool {
	return mailboxAttachmentEquipmentType(attachment) == "creature"
}

// mailboxAttachmentRequiresAvatarBlobs reports whether the current client will
// consume the two trailing extension blocks for this attachment. Current
// sub_229C570/sub_229ACD0 both gate them on item-table type <= 11, which is the
// avatar family the PVF names `[hat avatar]` .. `[weapon avatar]`; `[creature]`
// is 26 and ordinary equipment starts at 12, so neither reads them.
func mailboxAttachmentRequiresAvatarBlobs(attachment dnfrepo.MailAttachment) bool {
	return strings.HasSuffix(mailboxAttachmentEquipmentType(attachment), " avatar")
}

// mailboxAttachmentEquipmentType normalizes the durable PVF `[equipment type]`
// text kept on the attachment. The runtime PVF stores it as a backtick-quoted
// bracket tag such as `[weapon avatar]`, while older rows and tests carry the
// bare token, so both forms must resolve to the same category.
func mailboxAttachmentEquipmentType(attachment dnfrepo.MailAttachment) string {
	tag := strings.ToLower(strings.TrimSpace(attachment.Extra["mailbox_equipment_type"]))
	tag = strings.TrimSpace(strings.Trim(tag, "`"))
	tag = strings.TrimPrefix(tag, "[")
	tag = strings.TrimSuffix(tag, "]")
	return strings.TrimSpace(tag)
}

// mailboxAttachmentFixedHexBlob decodes the first present hex-encoded extra and
// returns exactly size bytes. The current client reads these blocks into fixed
// targets - a 30-byte jewel socket built by sub_3775940 and the single colour
// DWORD built by sub_3775970 and read back as one dword - so an over-long
// durable value must never reach the wire.
func mailboxAttachmentFixedHexBlob(attachment dnfrepo.MailAttachment, size int, keys ...string) []byte {
	for _, key := range keys {
		raw := strings.TrimSpace(attachment.Extra[key])
		if raw == "" {
			continue
		}
		raw = strings.NewReplacer(" ", "", "\t", "", "\r", "", "\n", "", "0x", "", "0X", "").Replace(raw)
		decoded, err := hex.DecodeString(raw)
		if err != nil || len(decoded) == 0 {
			continue
		}
		fixed := make([]byte, size)
		copy(fixed, decoded)
		return fixed
	}
	return nil
}

func mailboxLetterStat(mail dnfrepo.MailRecord) uint16 {
	if mailboxMailSaved(mail) {
		return 3
	}
	if mail.Read {
		return 2
	}
	return 1
}

func mailboxMailType(mail dnfrepo.MailRecord) byte {
	value, err := strconv.ParseUint(strings.TrimSpace(mail.Metadata["mail_type"]), 10, 8)
	if err != nil {
		return 0
	}
	return byte(value)
}

func mailboxRemainingSeconds(mail dnfrepo.MailRecord, now time.Time) uint32 {
	if mailboxMailSaved(mail) || mailboxMailUnlimited(mail) || now.IsZero() {
		return 0
	}
	expiresAt := mail.ExpireAt
	if expiresAt.IsZero() && !mail.CreatedAt.IsZero() {
		expiresAt = mail.CreatedAt.Add(mailboxNormalLifetime)
	}
	if expiresAt.IsZero() || !expiresAt.After(now) {
		return 0
	}
	remaining := int64(expiresAt.Sub(now) / time.Second)
	if remaining > math.MaxUint32 {
		return math.MaxUint32
	}
	return uint32(remaining)
}

func mailboxWireID(value string) (uint32, error) {
	parsed, err := strconv.ParseUint(strings.TrimSpace(value), 10, 32)
	if err != nil || parsed == 0 {
		return 0, fmt.Errorf("%w: invalid wire mail id %q", ErrMailboxInvalidRequest, value)
	}
	return uint32(parsed), nil
}

func mailboxSenderWireID(value string) (uint32, error) {
	if strings.TrimSpace(value) == "" {
		return 0, nil
	}
	parsed, err := strconv.ParseUint(strings.TrimSpace(value), 10, 32)
	if err != nil {
		return 0, fmt.Errorf("%w: invalid sender id %q", ErrMailboxInvalidRequest, value)
	}
	return uint32(parsed), nil
}

func mailboxUnix(value time.Time) uint32 {
	if value.IsZero() || value.Unix() <= 0 {
		return 0
	}
	if value.Unix() > math.MaxUint32 {
		return math.MaxUint32
	}
	return uint32(value.Unix())
}

func clampMailboxU16(value int) uint16 {
	if value <= 0 {
		return 0
	}
	if value >= math.MaxUint16 {
		return math.MaxUint16
	}
	return uint16(value)
}

func encodeMailboxText(value string, maxBytes int) ([]byte, error) {
	encoded, err := simplifiedchinese.GB18030.NewEncoder().Bytes([]byte(value))
	if err != nil {
		return nil, err
	}
	if len(encoded) > maxBytes {
		return nil, fmt.Errorf("length %d exceeds %d", len(encoded), maxBytes)
	}
	return encoded, nil
}
