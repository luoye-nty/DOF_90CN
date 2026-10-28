package dnfbridge

import (
	"context"
	"errors"
	"fmt"
	"strconv"
	"time"

	dnfrepo "longheng.io/server/internal/modules/dnf/repository"
)

const currentJoustSettlementMailSource = "joust_settlement_multiplier_return"

// reconcileCurrentMailboxAttachmentExpirations repairs every still-claimable
// mailbox attachment against the active PVF. System and overflow producers can
// create an attachment without a 0x77 row; for a live absolute deadline or a
// relative [usable period], that zero row makes the current client render the
// item as already expired. Persist the resolved instance deadline and the same
// raw/Extra state used by direct inventory grants. An explicitly supplied
// future instance deadline remains authoritative over a historical PVF date.
func (s *Service) reconcileCurrentMailboxAttachmentExpirations(
	ctx context.Context,
	characterID uint16,
) (int, error) {
	if s == nil || characterID == 0 {
		return 0, nil
	}
	repositories, ok := s.repositoryGroup()
	if !ok || repositories.MailboxAssets == nil {
		return 0, dnfrepo.ErrRepoMissing
	}
	catalog, err := s.currentPVFItemCatalog()
	if err != nil {
		return 0, err
	}
	characterKey := strconv.FormatUint(uint64(characterID), 10)
	now := s.gameplayNow().UTC()
	if now.IsZero() {
		now = time.Now().UTC()
	}
	changed := 0
	var reconcileErrs []error
	err = repositories.MailboxAssets.WithinMailboxAssets(ctx, characterKey, characterKey, func(
		_ dnfrepo.AccountRepository,
		_ dnfrepo.AccountInventoryRepository,
		_ dnfrepo.CharacterRepository,
		_ dnfrepo.InventoryRepository,
		mailboxes dnfrepo.MailboxRepository,
	) error {
		mailbox, found, loadErr := mailboxes.Load(ctx, characterKey)
		if loadErr != nil || !found {
			return loadErr
		}
		mailbox = dnfrepo.CloneMailbox(mailbox)
		for mailID, mail := range mailbox.Mails {
			if mail.Claimed || mail.Deleted {
				continue
			}
			mailChanged := false
			for index := range mail.Attachments {
				attachment := &mail.Attachments[index]
				if attachment.ItemID <= 0 || attachment.Count <= 0 || attachment.ItemID > int64(^uint32(0)) {
					reconcileErrs = append(reconcileErrs, fmt.Errorf("mail=%s attachment=%d is invalid", mailID, index))
					continue
				}
				definition, resolveErr := catalog.ResolveItem(uint32(attachment.ItemID))
				if resolveErr != nil {
					reconcileErrs = append(reconcileErrs, fmt.Errorf("resolve mail=%s attachment=%d item=%d: %w", mailID, index, attachment.ItemID, resolveErr))
					continue
				}
				if definition.Kind != dungeonDropItemStackable && definition.Kind != dungeonDropItemEquipment {
					continue
				}
				grantDefinition := definition
				expirationDate := attachment.ExpireAt
				if expirationDate.IsZero() {
					grantDefinition, resolveErr = currentPVFItemDefinitionForGrantAt(definition, now)
					if resolveErr != nil {
						reconcileErrs = append(reconcileErrs, fmt.Errorf("grant mail=%s attachment=%d item=%d: %w", mailID, index, attachment.ItemID, resolveErr))
						continue
					}
					expirationDate = grantDefinition.ExpirationDate
				}
				if expirationDate.IsZero() || !expirationDate.After(now) {
					continue
				}
				grantDefinition.ExpirationDate = expirationDate
				stack, stackChanged := applyCurrentPVFItemExpirationAt(dnfrepo.ItemStack{
					ItemID:   attachment.ItemID,
					Count:    attachment.Count,
					Bind:     attachment.Bind,
					ExpireAt: attachment.ExpireAt,
					RawEntry: attachment.RawEntry,
					Extra:    attachment.Extra,
				}, grantDefinition, now)
				if !stackChanged {
					continue
				}
				attachment.Bind = stack.Bind
				attachment.ExpireAt = stack.ExpireAt
				attachment.RawEntry = stack.RawEntry
				attachment.Extra = stack.Extra
				mailChanged = true
				changed++
			}
			if mailChanged {
				mailbox.Mails[mailID] = mail
			}
		}
		if changed == 0 {
			return nil
		}
		mailbox.UpdatedAt = now
		return dnfrepo.SaveMailboxFields(ctx, mailboxes, mailbox, dnfrepo.MailboxFieldMails)
	})
	return changed, errors.Join(err, errors.Join(reconcileErrs...))
}

// Keep the old entry point for focused compatibility tests and older callers.
func (s *Service) reconcileCurrentJoustSettlementMailAttachments(ctx context.Context, characterID uint16) (int, error) {
	return s.reconcileCurrentMailboxAttachmentExpirations(ctx, characterID)
}

func (s *Service) reconcileCurrentMailboxAttachmentExpirationsBeforeOpen(session *gameSession) {
	if s == nil || session == nil || session.selectedCharacterID == 0 {
		return
	}
	ctx, cancel := context.WithTimeout(context.Background(), createWriteTimeout)
	defer cancel()
	changed, err := s.reconcileCurrentMailboxAttachmentExpirations(ctx, session.selectedCharacterID)
	if err != nil {
		s.logGameEvent(session, "game-mailbox-attachment-expiration-repair-partial", "error", err)
		return
	}
	if changed > 0 {
		s.logGameEvent(session, "game-mailbox-attachment-expiration-repaired", "attachments", changed)
	}
}
