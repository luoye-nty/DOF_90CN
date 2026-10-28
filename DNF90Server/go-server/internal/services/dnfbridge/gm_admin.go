package dnfbridge

import (
	"context"
	"errors"
	"fmt"
	"math"
	"sort"
	"strconv"
	"strings"
	"time"

	"longheng.io/server/internal/modules/dnf/mail"
	dnfrepo "longheng.io/server/internal/modules/dnf/repository"
)

var (
	ErrGMRepositoryUnavailable = errors.New("dnf gm repository is unavailable")
	ErrGMRequestInvalid        = errors.New("dnf gm request is invalid")
	ErrGMCharacterNotFound     = errors.New("dnf gm character is not found")
)

const maxGMSystemMailAttachments = 10

type GMCharacterInfo struct {
	CharacterID string `json:"character_id"`
	AccountID   string `json:"account_id"`
	Name        string `json:"name"`
	Level       int    `json:"level"`
	Gold        int64  `json:"gold"`
	Cera        int64  `json:"cera"`
	Online      bool   `json:"online"`
}

type GMSystemMailAttachment struct {
	ItemID         uint32 `json:"item_id"`
	Count          uint32 `json:"count"`
	Bind           bool   `json:"bind"`
	ReinforceLevel int    `json:"reinforce_level,omitempty"`
	AmplifyLevel   int    `json:"amplify_level,omitempty"`
	AmplifyType    int    `json:"amplify_type,omitempty"`
}

type GMSystemMailRequest struct {
	CharacterID   string                   `json:"character_id,omitempty"`
	CharacterName string                   `json:"character_name,omitempty"`
	Title         string                   `json:"title,omitempty"`
	Body          string                   `json:"body,omitempty"`
	Gold          int64                    `json:"gold,omitempty"`
	Attachments   []GMSystemMailAttachment `json:"attachments,omitempty"`
}

type GMSystemMailResult struct {
	MailID               string `json:"mail_id"`
	RecipientCharacterID string `json:"recipient_character_id"`
	RecipientName        string `json:"recipient_name"`
	Online               bool   `json:"online"`
	Refreshed            bool   `json:"refreshed"`
	RefreshError         string `json:"refresh_error,omitempty"`
}

type GMMailboxClearRequest struct {
	CharacterID   string `json:"character_id,omitempty"`
	CharacterName string `json:"character_name,omitempty"`
	Confirm       bool   `json:"confirm"`
}

type GMMailboxClearResult struct {
	CharacterID        string `json:"character_id"`
	CharacterName      string `json:"character_name"`
	RemovedMails       int    `json:"removed_mails"`
	RemovedUnread      int    `json:"removed_unread"`
	RemovedClaimable   int    `json:"removed_claimable"`
	RemovedGold        int64  `json:"removed_gold"`
	RemovedAttachments int    `json:"removed_attachments"`
	Online             bool   `json:"online"`
	Refreshed          bool   `json:"refreshed"`
	RefreshError       string `json:"refresh_error,omitempty"`
}

// GMCharacters returns the local account's authoritative character rows from
// the configured repository. Both repository backends implement this same
// interface, so callers never need to inspect MySQL tables or SQLite records.
func (s *Service) GMCharacters(ctx context.Context) ([]GMCharacterInfo, error) {
	return s.GMFindCharacters(ctx, s.accountID(), "")
}

// GMFindCharacters performs an exact repository-native lookup by account ID
// or character name. It deliberately avoids a database-wide scan: the local
// GM page can query the intended owner directly on both MySQL and SQLite.
func (s *Service) GMFindCharacters(ctx context.Context, accountID, characterName string) ([]GMCharacterInfo, error) {
	if ctx == nil {
		ctx = context.Background()
	}
	accountID = strings.TrimSpace(accountID)
	characterName = strings.TrimSpace(characterName)
	if (accountID == "") == (characterName == "") || len(accountID) > 128 || len(characterName) > 64 {
		return nil, ErrGMRequestInvalid
	}
	repositories, available := s.repositoryGroup()
	if !available || repositories.Character == nil {
		return nil, ErrGMRepositoryUnavailable
	}
	characters := make([]dnfrepo.CharacterRecord, 0, dnfrepo.MaxCharacterSlotLimit)
	if accountID != "" {
		var err error
		characters, err = repositories.Character.ListByAccount(ctx, accountID, dnfrepo.MaxCharacterSlotLimit)
		if err != nil {
			return nil, err
		}
	} else {
		characterID, found, err := repositories.Character.FindIDByName(ctx, characterName)
		if err != nil {
			return nil, err
		}
		if found {
			character, loaded, loadErr := repositories.Character.Load(ctx, characterID)
			if loadErr != nil {
				return nil, loadErr
			}
			if loaded && character.Stats["delete_flag"] == 0 {
				characters = append(characters, character)
			}
		}
	}
	result := make([]GMCharacterInfo, 0, len(characters))
	for _, character := range characters {
		info, err := s.gmCharacterInfo(ctx, repositories, character)
		if err != nil {
			return nil, err
		}
		result = append(result, info)
	}
	sort.SliceStable(result, func(left, right int) bool {
		leftID, _ := strconv.ParseUint(result[left].CharacterID, 10, 64)
		rightID, _ := strconv.ParseUint(result[right].CharacterID, 10, 64)
		return leftID < rightID
	})
	return result, nil
}

// GMFindCharactersByUsername resolves the launcher-visible login name before
// querying the DNF character owner. Password hashes never leave the repository.
func (s *Service) GMFindCharactersByUsername(ctx context.Context, username string) ([]GMCharacterInfo, error) {
	if ctx == nil {
		ctx = context.Background()
	}
	username = strings.ToLower(strings.TrimSpace(username))
	if username == "" || len(username) > 64 {
		return nil, ErrGMRequestInvalid
	}
	repositories, available := s.repositoryGroup()
	if !available || repositories.Account == nil {
		return nil, ErrGMRepositoryUnavailable
	}
	finder, ok := repositories.Account.(dnfrepo.LoginAccountUsernameFinder)
	if !ok {
		return nil, ErrGMRepositoryUnavailable
	}
	accountID, found, err := finder.FindAccountIDByLoginUsername(ctx, username)
	if err != nil {
		return nil, err
	}
	if !found {
		return []GMCharacterInfo{}, nil
	}
	return s.GMFindCharacters(ctx, accountID, "")
}

// GMSendSystemMail resolves every attachment against the active runtime PVF,
// commits one non-expiring system mail through the repository unit of work,
// then refreshes the online client's unread badge and complete mailbox page.
func (s *Service) GMSendSystemMail(ctx context.Context, request GMSystemMailRequest) (GMSystemMailResult, error) {
	if ctx == nil {
		ctx = context.Background()
	}
	request.CharacterID = strings.TrimSpace(request.CharacterID)
	request.CharacterName = strings.TrimSpace(request.CharacterName)
	request.Title = strings.TrimSpace(request.Title)
	request.Body = strings.TrimSpace(request.Body)
	if request.CharacterID == "" && request.CharacterName == "" {
		return GMSystemMailResult{}, fmt.Errorf("%w: character selector is required", ErrGMRequestInvalid)
	}
	if request.Gold < 0 || request.Gold > math.MaxUint32 || len(request.Attachments) > maxGMSystemMailAttachments ||
		(request.Gold == 0 && len(request.Attachments) == 0 && request.Title == "" && request.Body == "") {
		return GMSystemMailResult{}, ErrGMRequestInvalid
	}
	repositories, available := s.repositoryGroup()
	if !available || repositories.Character == nil || repositories.Mailbox == nil || repositories.MailboxAssets == nil {
		return GMSystemMailResult{}, ErrGMRepositoryUnavailable
	}

	attachments := make([]dnfrepo.MailAttachment, 0, len(request.Attachments))
	if len(request.Attachments) > 0 {
		catalog, err := s.currentPVFItemCatalog()
		if err != nil {
			return GMSystemMailResult{}, fmt.Errorf("resolve runtime PVF: %w", err)
		}
		var amplifyConfig *currentAmplifyItemConfig
		for index, requested := range request.Attachments {
			if requested.ItemID == 0 || requested.Count == 0 {
				return GMSystemMailResult{}, fmt.Errorf("%w: attachment=%d", ErrGMRequestInvalid, index)
			}
			if requested.ReinforceLevel < 0 || requested.ReinforceLevel > 31 || requested.AmplifyLevel < 0 || requested.AmplifyLevel > 31 ||
				requested.AmplifyType < 0 || requested.AmplifyType > 4 ||
				(requested.ReinforceLevel > 0 && (requested.AmplifyLevel > 0 || requested.AmplifyType > 0)) ||
				(requested.AmplifyLevel > 0 && requested.AmplifyType == 0) {
				return GMSystemMailResult{}, fmt.Errorf("%w: attachment=%d upgrade fields", ErrGMRequestInvalid, index)
			}
			definition, err := catalog.ResolveItem(requested.ItemID)
			if err != nil {
				return GMSystemMailResult{}, fmt.Errorf("%w: attachment=%d item=%d: %v", ErrGMRequestInvalid, index, requested.ItemID, err)
			}
			if definition.Kind == dungeonDropItemEquipment && requested.Count != 1 {
				return GMSystemMailResult{}, fmt.Errorf("%w: equipment attachment=%d count must be 1", ErrGMRequestInvalid, index)
			}
			if definition.Kind != dungeonDropItemEquipment && (requested.ReinforceLevel != 0 || requested.AmplifyLevel != 0 || requested.AmplifyType != 0) {
				return GMSystemMailResult{}, fmt.Errorf("%w: stackable attachment=%d cannot be upgraded", ErrGMRequestInvalid, index)
			}
			now := time.Now().UTC()
			definition, err = currentPVFItemDefinitionForGrantAt(definition, now)
			if err != nil {
				return GMSystemMailResult{}, fmt.Errorf("%w: attachment=%d expiration: %v", ErrGMRequestInvalid, index, err)
			}
			listType, slotStart, _, placementErr := currentMailboxPVFPlacement(definition)
			if placementErr != nil {
				return GMSystemMailResult{}, fmt.Errorf("resolve attachment=%d item=%d mailbox placement: %w", index, requested.ItemID, placementErr)
			}
			// The generic grant constructor requires the definition's source-page
			// slot to seed its fixed row. Reproject that durable stack below into
			// the mailbox item's native list and slot.
			stack, err := buildCurrentDungeonPickupStack(definition.SlotStart, definition, requested.Count)
			if err != nil {
				return GMSystemMailResult{}, fmt.Errorf("build attachment=%d item=%d: %w", index, requested.ItemID, err)
			}
			stack.Bind = requested.Bind
			if stack.Extra == nil {
				stack.Extra = make(map[string]string)
			}
			stack.Extra["source"] = "gm_admin_system_mail"
			stack.Extra["last_grant_source"] = "gm_admin_system_mail"
			stack.Extra["mailbox_pvf_kind"] = string(definition.Kind)
			stack.Extra["mailbox_equipment_type"] = definition.EquipmentType
			stack.Extra["mailbox_pvf_path"] = definition.PVFPath
			if definition.Kind == dungeonDropItemEquipment {
				level := requested.ReinforceLevel
				if requested.AmplifyType > 0 {
					level = requested.AmplifyLevel
				}
				levelText := strconv.Itoa(level)
				stack.Extra["ext_data0"] = levelText
				stack.Extra["packed_flag_byte"] = levelText
				stack.Extra["reinforce"] = levelText
				stack.Extra["upgrade_level"] = levelText
				if requested.AmplifyType > 0 {
					if amplifyConfig == nil {
						amplifyConfig, err = currentAmplifyItemConfigFromSource(catalog.source)
						if err != nil {
							return GMSystemMailResult{}, fmt.Errorf("resolve attachment=%d amplification config: %w", index, err)
						}
					}
					initialValues, initialErr := currentAmplifyInitialValues(amplifyConfig, definition.Rarity)
					if initialErr != nil {
						return GMSystemMailResult{}, fmt.Errorf("resolve attachment=%d item=%d amplification value: %w", index, requested.ItemID, initialErr)
					}
					amplifyValue := initialValues[byte(requested.AmplifyType)]
					if amplifyValue == 0 {
						return GMSystemMailResult{}, fmt.Errorf("%w: attachment=%d item=%d amplification type=%d has no PVF value", ErrGMRequestInvalid, index, requested.ItemID, requested.AmplifyType)
					}
					amplifyType := strconv.Itoa(requested.AmplifyType)
					amplifyValueText := strconv.FormatUint(uint64(amplifyValue), 10)
					for _, key := range []string{"amplify_type", "amplification_type", "byte_13", "value_13", "value_c"} {
						stack.Extra[key] = amplifyType
					}
					for _, key := range []string{"amplify_value", "amplification_value", "marker_16", "marker16", "value_d"} {
						stack.Extra[key] = amplifyValueText
					}
				}
			}
			// Preserve the item's native fixed 0x77 row in the envelope. Mailbox
			// summaries normalize their own core identity fields, while a later
			// claim retains this row and projects it through the resolved native
			// owner. Avatars therefore never persist a list-0 equipment row that
			// would be replayed into list 1 after extraction.
			entry := currentItemListEntryFromStack(listType, slotStart, stack)
			stack.RawEntry = append([]byte(nil), entry.data[:]...)
			attachments = append(attachments, dnfrepo.MailAttachment{
				ItemID:   stack.ItemID,
				Count:    stack.Count,
				Bind:     stack.Bind,
				ExpireAt: stack.ExpireAt,
				RawEntry: append([]byte(nil), stack.RawEntry...),
				Extra:    cloneDungeonCardStringMap(stack.Extra),
			})
		}
	}

	delivery, err := mail.NewOwner(repositories).SendSystem(ctx, mail.SystemSendRequest{
		RecipientCharacterID: request.CharacterID,
		RecipientName:        request.CharacterName,
		Title:                request.Title,
		Body:                 request.Body,
		Gold:                 request.Gold,
		Attachments:          attachments,
		Source:               "gm_admin_system_mail",
	})
	if err != nil {
		if errors.Is(err, mail.ErrMailboxInvalidRequest) {
			return GMSystemMailResult{}, fmt.Errorf("%w: %v", ErrGMRequestInvalid, err)
		}
		if errors.Is(err, mail.ErrMailboxRecipientMissing) || errors.Is(err, mail.ErrMailboxCharacterMissing) {
			return GMSystemMailResult{}, fmt.Errorf("%w: %v", ErrGMCharacterNotFound, err)
		}
		return GMSystemMailResult{}, err
	}
	result := GMSystemMailResult{
		MailID:               delivery.MailID,
		RecipientCharacterID: delivery.RecipientCharacterID,
		RecipientName:        delivery.RecipientName,
	}
	characterID, parseErr := strconv.ParseUint(delivery.RecipientCharacterID, 10, 16)
	if parseErr != nil || characterID == 0 {
		return result, nil
	}
	session, online := s.onlineGameSession(uint16(characterID))
	result.Online = online
	if !online {
		return result, nil
	}
	// 邮件此刻已经落库：推送用独立 context，客户端断开不该让提醒被事件循环丢掉。
	// 闭包内部的仓储读也必须跟着用 pushCtx —— 否则请求 ctx 一取消，Open 就先失败了。
	// 详见 gmPostCommitPushContext。
	pushCtx, pushCancel := gmPostCommitPushContext()
	defer pushCancel()
	if err := s.callGameSession(pushCtx, session, "gm-admin-system-mail-refresh", func() error {
		if session.selectedCharacterID != uint16(characterID) {
			return fmt.Errorf("selected character changed before mailbox refresh")
		}
		opened, err := mail.NewOwner(repositories).Open(pushCtx, delivery.RecipientCharacterID)
		if err != nil {
			return err
		}
		if err := s.sendGameUpperRawClass(session, mail.MailboxAlarmNotificationMessageID, mail.BuildAlarmNotification(opened.Unread), 0); err != nil {
			return err
		}
		// The current EXE creates every attachment object while consuming op97.
		// A GM delivery can arrive while no mailbox page owns those objects (or
		// while the previous page is still inside a callback), so an unsolicited
		// full-page replacement is unsafe for non-main inventory families such as
		// rare avatars. Native online delivery only needs the op99 unread alarm;
		// the client's next op96 mailbox open owns and requests the complete op97.
		s.logGameEvent(session, "game-gm-system-mail-refreshed",
			"char_id", characterID,
			"mail_id", delivery.MailID,
			"unread", opened.Unread,
			"sequence", "op99_alarm_only_then_client_op96_owned_op97")
		return nil
	}); err != nil {
		result.RefreshError = err.Error()
		s.logWarn("dnfbridge GM system mail live refresh deferred",
			"character_id", delivery.RecipientCharacterID,
			"mail_id", delivery.MailID,
			"error", err)
		return result, nil
	}
	result.Refreshed = true
	return result, nil
}

// GMClearMailbox permanently clears the selected character's complete durable
// inbox through the repository transaction shared by MySQL and SQLite. An
// online client receives the same unread and list notifications as opening an
// empty mailbox in game.
func (s *Service) GMClearMailbox(ctx context.Context, request GMMailboxClearRequest) (GMMailboxClearResult, error) {
	if ctx == nil {
		ctx = context.Background()
	}
	request.CharacterID = strings.TrimSpace(request.CharacterID)
	request.CharacterName = strings.TrimSpace(request.CharacterName)
	if !request.Confirm || (request.CharacterID == "" && request.CharacterName == "") ||
		len(request.CharacterID) > 32 || len(request.CharacterName) > 64 {
		return GMMailboxClearResult{}, ErrGMRequestInvalid
	}
	repositories, available := s.repositoryGroup()
	if !available || repositories.Character == nil || repositories.Mailbox == nil || repositories.MailboxAssets == nil {
		return GMMailboxClearResult{}, ErrGMRepositoryUnavailable
	}
	character, err := s.gmCharacter(ctx, repositories, request.CharacterID, request.CharacterName)
	if err != nil {
		return GMMailboxClearResult{}, err
	}
	cleared, err := mail.NewOwner(repositories).ClearAll(ctx, character.CharacterID)
	if err != nil {
		return GMMailboxClearResult{}, err
	}
	result := GMMailboxClearResult{
		CharacterID:        character.CharacterID,
		CharacterName:      character.Name,
		RemovedMails:       cleared.Removed,
		RemovedUnread:      cleared.Unread,
		RemovedClaimable:   cleared.Claimable,
		RemovedGold:        cleared.Gold,
		RemovedAttachments: cleared.Attachments,
	}
	characterID, parseErr := strconv.ParseUint(character.CharacterID, 10, 16)
	if parseErr != nil || characterID == 0 {
		return result, nil
	}
	session, online := s.onlineGameSession(uint16(characterID))
	result.Online = online
	if !online {
		return result, nil
	}
	// 同"系统邮件"：清空已经落库，推送与闭包内的仓储读都改用独立 context。
	pushCtx, pushCancel := gmPostCommitPushContext()
	defer pushCancel()
	if err := s.callGameSession(pushCtx, session, "gm-admin-mailbox-clear-refresh", func() error {
		if session.selectedCharacterID != uint16(characterID) {
			return fmt.Errorf("selected character changed before mailbox clear refresh")
		}
		opened, err := mail.NewOwner(repositories).Open(pushCtx, character.CharacterID)
		if err != nil {
			return err
		}
		snapshot, err := mail.BuildListNotification(opened)
		if err != nil {
			return err
		}
		if err := s.sendGameUpperRawClass(session, mail.MailboxAlarmNotificationMessageID, mail.BuildAlarmNotification(0), 0); err != nil {
			return err
		}
		if err := s.sendGameUpperRawClass(session, mail.MailboxListNotificationMessageID, snapshot, 0); err != nil {
			return err
		}
		s.logGameEvent(session, "game-gm-mailbox-cleared",
			"char_id", characterID,
			"removed_mails", cleared.Removed,
			"removed_unread", cleared.Unread,
			"removed_claimable", cleared.Claimable)
		return nil
	}); err != nil {
		result.RefreshError = err.Error()
		s.logWarn("dnfbridge GM mailbox clear live refresh deferred",
			"character_id", character.CharacterID,
			"removed_mails", cleared.Removed,
			"error", err)
		return result, nil
	}
	result.Refreshed = true
	return result, nil
}
