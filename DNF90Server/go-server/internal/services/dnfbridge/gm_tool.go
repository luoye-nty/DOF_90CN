package dnfbridge

import (
	"context"
	"errors"
	"fmt"
	"math"
	"path"
	"sort"
	"strconv"
	"strings"
	"time"

	dnfjobmap "longheng.io/server/internal/modules/dnf/jobmap"
	dnfprofession "longheng.io/server/internal/modules/dnf/profession"
	dnfprogression "longheng.io/server/internal/modules/dnf/progression"
	dnfpvf "longheng.io/server/internal/modules/dnf/pvf"
	dnfquest "longheng.io/server/internal/modules/dnf/quest"
	dnfrepo "longheng.io/server/internal/modules/dnf/repository"
	dnfskill "longheng.io/server/internal/modules/dnf/skill"
)

var (
	ErrGMUnavailable     = errors.New("dnf90 GM service is unavailable")
	ErrGMAccountNotFound = errors.New("dnf90 GM account was not found")
	ErrGMCharacterOnline = errors.New("dnf90 GM refuses to modify an online character")
	ErrGMInvalidRequest  = errors.New("dnf90 GM request is invalid")
)

type GMStatus struct {
	RepositoryReady  bool   `json:"repository_ready"`
	ItemCatalogReady bool   `json:"item_catalog_ready"`
	OnlineCharacters int    `json:"online_characters"`
	LevelCap         int    `json:"level_cap"`
	Error            string `json:"error,omitempty"`
}

type GMChannel struct {
	ID           int    `json:"id"`
	Type         uint8  `json:"type"`
	Group        string `json:"group"`
	Name         string `json:"name"`
	Port         int    `json:"port"`
	Ordinary     bool   `json:"ordinary"`
	Required     bool   `json:"required"`
	GroupMinimum int    `json:"group_minimum"`
	Enabled      bool   `json:"enabled"`
}

type GMChannelOverview struct {
	TotalChannels          int         `json:"total_channels"`
	EnabledChannels        int         `json:"enabled_channels"`
	GamePorts              int         `json:"game_ports"`
	Channels               []GMChannel `json:"channels"`
	SinglePlayerIDs        []int       `json:"single_player_ids"`
	MinimalSinglePlayerIDs []int       `json:"minimal_single_player_ids"`
}

type GMAccountSummary struct {
	AccountID            string    `json:"account_id"`
	State                string    `json:"state,omitempty"`
	RepresentAccountName string    `json:"represent_account_name,omitempty"`
	UpdatedAt            time.Time `json:"updated_at,omitempty"`
}

type GMCharacterSummary struct {
	CharacterID    string `json:"character_id"`
	AccountID      string `json:"account_id"`
	Slot           int    `json:"slot"`
	Name           string `json:"name"`
	Job            string `json:"job"`
	ProfessionName string `json:"profession_name"`
	GrowType       int64  `json:"grow_type"`
	FirstGrow      int64  `json:"first_grow"`
	Awakening      int64  `json:"awakening"`
	Level          int    `json:"level"`
	Gold           int64  `json:"gold"`
	Online         bool   `json:"online"`
}

type GMCareerState struct {
	Character      GMCharacterSummary     `json:"character"`
	Job            byte                   `json:"job"`
	GrowType       byte                   `json:"grow_type"`
	FirstGrow      byte                   `json:"first_grow"`
	Awakening      byte                   `json:"awakening"`
	ProfessionName string                 `json:"profession_name"`
	Options        []dnfprofession.Option `json:"options"`
	SkillsReset    bool                   `json:"skills_reset,omitempty"`
	QuestResetIDs  []int64                `json:"quest_reset_ids,omitempty"`
}

type GMQuestSummary struct {
	QuestID         int64     `json:"quest_id"`
	Name            string    `json:"name"`
	Path            string    `json:"path"`
	Grade           string    `json:"grade"`
	Type            string    `json:"type"`
	LevelMin        int       `json:"level_min"`
	LevelMax        int       `json:"level_max"`
	Status          string    `json:"status"`
	ProgressValue   int64     `json:"progress_value,omitempty"`
	Active          bool      `json:"active"`
	Completed       bool      `json:"completed"`
	Acceptable      bool      `json:"acceptable"`
	JobChangeQuest  int       `json:"job_change_quest,omitempty"`
	RewardType      string    `json:"reward_type,omitempty"`
	ProfessionQuest bool      `json:"profession_quest"`
	MainQuestID     int64     `json:"main_quest_id,omitempty"`
	Prerequisites   [][]int64 `json:"prerequisites,omitempty"`
	Collisions      []int64   `json:"collisions,omitempty"`
	CantGiveUp      bool      `json:"cant_give_up,omitempty"`
	TitleItemID     int32     `json:"title_item_id,omitempty"`
	TitleCategory   int32     `json:"title_category,omitempty"`
	TitleBookIndex  int32     `json:"title_book_index,omitempty"`
}

type GMQuestOverview struct {
	Character GMCharacterSummary `json:"character"`
	Active    []GMQuestSummary   `json:"active"`
	Completed []GMQuestSummary   `json:"completed"`
}

type GMQuestRewardItem struct {
	ItemID           int64  `json:"item_id"`
	Name             string `json:"name,omitempty"`
	Count            int64  `json:"count"`
	Selectable       bool   `json:"selectable,omitempty"`
	HasJobFilter     bool   `json:"has_job_filter,omitempty"`
	RequiredJob      int    `json:"required_job,omitempty"`
	RequiredGrowType int    `json:"required_grow_type,omitempty"`
}

type GMQuestDetail struct {
	Quest                GMQuestSummary      `json:"quest"`
	Introduction         string              `json:"introduction,omitempty"`
	Objective            string              `json:"objective,omitempty"`
	Completion           string              `json:"completion,omitempty"`
	Difficulty           string              `json:"difficulty,omitempty"`
	Job                  string              `json:"job,omitempty"`
	NoExperience         bool                `json:"no_experience,omitempty"`
	GoldMultiple         int                 `json:"gold_multiple,omitempty"`
	RewardItems          []GMQuestRewardItem `json:"reward_items,omitempty"`
	RewardSelectionItems []GMQuestRewardItem `json:"reward_selection_items,omitempty"`
}

type GMQuestMutationResult struct {
	Character         GMCharacterSummary `json:"character"`
	Quest             GMQuestSummary     `json:"quest"`
	RewardGranted     bool               `json:"reward_granted"`
	ProfessionChanged bool               `json:"profession_changed"`
}

type GMItemSummary struct {
	ItemID           uint32             `json:"item_id"`
	Name             string             `json:"name"`
	Kind             string             `json:"kind"`
	PVFPath          string             `json:"pvf_path"`
	IconPath         string             `json:"icon_path,omitempty"`
	IconIndex        int64              `json:"icon_index,omitempty"`
	MinimumLevel     int64              `json:"minimum_level,omitempty"`
	Rarity           int64              `json:"rarity,omitempty"`
	StackLimit       int64              `json:"stack_limit,omitempty"`
	AttachType       string             `json:"attach_type,omitempty"`
	EquipmentType    string             `json:"equipment_type,omitempty"`
	UsableJobs       []string           `json:"usable_jobs,omitempty"`
	Grade            int64              `json:"grade,omitempty"`
	StackableType    string             `json:"stackable_type,omitempty"`
	ExpirationDate   time.Time          `json:"expiration_date,omitempty"`
	UsablePeriodDays int64              `json:"usable_period_days,omitempty"`
	Price            int64              `json:"price,omitempty"`
	MailEffects      []GMItemMailEffect `json:"mail_effects,omitempty"`
}

type GMItemAttribute struct {
	Name  string `json:"name"`
	Value int64  `json:"value"`
}

type GMItemSkillLevelUp struct {
	Job       string `json:"job"`
	SkillID   int64  `json:"skill_id"`
	SkillName string `json:"skill_name,omitempty"`
	Level     int64  `json:"level"`
}

type GMItemRequiredSkill struct {
	Job       string `json:"job"`
	SkillID   int64  `json:"skill_id"`
	SkillName string `json:"skill_name,omitempty"`
}

type GMItemSkillModifier struct {
	SkillType       string `json:"skill_type"`
	MPCostPercent   int64  `json:"mp_cost_percent,omitempty"`
	CooldownPercent int64  `json:"cooldown_percent,omitempty"`
}

type GMItemMailEffect struct {
	Name                string               `json:"name,omitempty"`
	Description         string               `json:"description,omitempty"`
	Attributes          []GMItemAttribute    `json:"attributes,omitempty"`
	ElementalProperties []string             `json:"elemental_properties,omitempty"`
	SkillLevelUps       []GMItemSkillLevelUp `json:"skill_level_ups,omitempty"`
}

type GMEquipmentSetBonus struct {
	RequiredPieces int               `json:"required_pieces"`
	BasicExplain   string            `json:"basic_explain,omitempty"`
	DetailExplain  string            `json:"detail_explain,omitempty"`
	Attributes     []GMItemAttribute `json:"attributes,omitempty"`
}

type GMEquipmentSetDetail struct {
	SetID   int64                 `json:"set_id"`
	Name    string                `json:"name"`
	PVFPath string                `json:"pvf_path"`
	Bonuses []GMEquipmentSetBonus `json:"bonuses,omitempty"`
}

type GMAvatarLayer struct {
	Order int64  `json:"order"`
	Name  string `json:"name"`
}

type GMAvatarVariation struct {
	AnimationJob string          `json:"animation_job"`
	Variation    int64           `json:"variation"`
	Palette      int64           `json:"palette"`
	Layers       []GMAvatarLayer `json:"layers,omitempty"`
}

type GMAvatarAuraFrame struct {
	ImagePath     string `json:"image_path"`
	ImageIndex    int64  `json:"image_index"`
	X             int64  `json:"x"`
	Y             int64  `json:"y"`
	Delay         int64  `json:"delay"`
	GraphicEffect string `json:"graphic_effect,omitempty"`
}

type GMAvatarAuraAnimation struct {
	Layer   string              `json:"layer"`
	ANIPath string              `json:"ani_path"`
	Frames  []GMAvatarAuraFrame `json:"frames"`
}

type GMItemDetail struct {
	Item                 GMItemSummary           `json:"item"`
	Explain              string                  `json:"explain,omitempty"`
	BasicExplain         string                  `json:"basic_explain,omitempty"`
	DetailExplain        string                  `json:"detail_explain,omitempty"`
	FlavorText           string                  `json:"flavor_text,omitempty"`
	Durability           int64                   `json:"durability,omitempty"`
	Weight               int64                   `json:"weight,omitempty"`
	RepairPrice          int64                   `json:"repair_price,omitempty"`
	Value                int64                   `json:"value,omitempty"`
	QualityPercent       int64                   `json:"quality_percent,omitempty"`
	WeaponAttackSpeed    string                  `json:"weapon_attack_speed,omitempty"`
	ItemGroupName        string                  `json:"item_group_name,omitempty"`
	AvatarWeaponGroup    string                  `json:"avatar_weapon_group,omitempty"`
	PartSetIndex         int64                   `json:"part_set_index,omitempty"`
	EquipmentSet         *GMEquipmentSetDetail   `json:"equipment_set,omitempty"`
	ElementalProperties  []string                `json:"elemental_properties,omitempty"`
	Attributes           []GMItemAttribute       `json:"attributes,omitempty"`
	RequiredSkills       []GMItemRequiredSkill   `json:"required_skills,omitempty"`
	SkillLevelUps        []GMItemSkillLevelUp    `json:"skill_level_ups,omitempty"`
	SkillModifiers       []GMItemSkillModifier   `json:"skill_modifiers,omitempty"`
	AvatarVariations     []GMAvatarVariation     `json:"avatar_variations,omitempty"`
	AvatarAuraAnimations []GMAvatarAuraAnimation `json:"avatar_aura_animations,omitempty"`
}

type GMMailDeliveryResult struct {
	MailID    string             `json:"mail_id"`
	Character GMCharacterSummary `json:"character"`
	Item      GMItemSummary      `json:"item"`
	Count     int64              `json:"count"`
}

type GMMailAttachmentSummary struct {
	ItemID   int64     `json:"item_id"`
	Count    int64     `json:"count"`
	Bind     bool      `json:"bind"`
	ExpireAt time.Time `json:"expire_at,omitempty"`
}

type GMMailSummary struct {
	MailID      string                    `json:"mail_id"`
	SenderName  string                    `json:"sender_name"`
	Title       string                    `json:"title"`
	Body        string                    `json:"body"`
	Gold        int64                     `json:"gold"`
	Attachments []GMMailAttachmentSummary `json:"attachments"`
	Read        bool                      `json:"read"`
	Claimed     bool                      `json:"claimed"`
	Claimable   bool                      `json:"claimable"`
	Saved       bool                      `json:"saved"`
	CreatedAt   time.Time                 `json:"created_at,omitempty"`
	ExpireAt    time.Time                 `json:"expire_at,omitempty"`
}

type GMMailboxSnapshot struct {
	Character GMCharacterSummary `json:"character"`
	Mails     []GMMailSummary    `json:"mails"`
}

type GMMailDeleteResult struct {
	Deleted int               `json:"deleted"`
	Mailbox GMMailboxSnapshot `json:"mailbox"`
}

type GMExtraEquipmentSlotState struct {
	Character          GMCharacterSummary `json:"character"`
	Mask               byte               `json:"mask"`
	SupportUnlocked    bool               `json:"support_unlocked"`
	MagicStoneUnlocked bool               `json:"magic_stone_unlocked"`
	EarringUnlocked    bool               `json:"earring_unlocked"`
}

// GMMailOptions contains only item attributes that are proved by the current
// client inventory projection. Zero values preserve the PVF/default behavior.
type GMMailOptions struct {
	ExpireDays        int      `json:"expire_days,omitempty"`
	Reinforce         int      `json:"reinforce,omitempty"`
	AmplifyType       int      `json:"amplify_type,omitempty"`
	EquipmentQuality  string   `json:"equipment_quality,omitempty"`
	Forging           int      `json:"forging,omitempty"`
	EmblemItemIDs     []uint32 `json:"emblem_item_ids,omitempty"`
	EnchantBeadItemID uint32   `json:"enchant_bead_item_id,omitempty"`
}

func (s *Service) GMStatus(ctx context.Context) GMStatus {
	status := GMStatus{LevelCap: currentAdventureCharacterLevelCap}
	repositories, ok := s.repositoryGroup()
	if !ok {
		status.Error = ErrGMUnavailable.Error()
		return status
	}
	if ctx == nil {
		ctx = context.Background()
	}
	if err := repositories.Check(ctx); err != nil {
		status.Error = err.Error()
	} else {
		status.RepositoryReady = true
	}
	_, loaded, catalogErr := s.currentPVFItemCatalogIfLoaded()
	status.ItemCatalogReady = loaded && catalogErr == nil
	if catalogErr != nil && status.Error == "" {
		status.Error = catalogErr.Error()
	}
	s.mu.Lock()
	status.OnlineCharacters = len(s.gameSessions)
	s.mu.Unlock()
	return status
}

func (s *Service) GMChannels() (GMChannelOverview, error) {
	active := s.currentCatalog()
	if active == nil {
		return GMChannelOverview{}, ErrGMUnavailable
	}
	enabled := make(map[int]bool)
	for _, channel := range active.Channels() {
		enabled[channel.ID] = true
	}
	channels := active.Channels()
	overview := GMChannelOverview{
		TotalChannels:   len(channels),
		EnabledChannels: len(enabled),
		GamePorts:       len(active.GamePorts()),
		Channels:        make([]GMChannel, 0, len(channels)),
	}
	for _, channel := range channels {
		overview.Channels = append(overview.Channels, GMChannel{
			ID:           channel.ID,
			Type:         channel.Type,
			Group:        channel.Group,
			Name:         channel.Name,
			Port:         channel.Port,
			Ordinary:     true,
			Required:     true,
			GroupMinimum: 1,
			Enabled:      enabled[channel.ID],
		})
	}
	return overview, nil
}

func (s *Service) GMListAccounts(ctx context.Context, query string, limit int) ([]GMAccountSummary, error) {
	repositories, ok := s.repositoryGroup()
	if !ok || repositories.Account == nil {
		return nil, ErrGMUnavailable
	}
	records, err := dnfrepo.ListAccounts(ctx, repositories.Account, query, limit)
	if err != nil {
		return nil, err
	}
	out := make([]GMAccountSummary, 0, len(records))
	for _, record := range records {
		out = append(out, GMAccountSummary{
			AccountID:            record.AccountID,
			State:                record.State,
			RepresentAccountName: record.RepresentAccountName,
			UpdatedAt:            record.UpdatedAt,
		})
	}
	return out, nil
}

func (s *Service) GMListCharacters(ctx context.Context, accountID string) ([]GMCharacterSummary, error) {
	repositories, ok := s.repositoryGroup()
	accountID = strings.TrimSpace(accountID)
	if !ok || repositories.Account == nil || repositories.Character == nil || accountID == "" {
		return nil, ErrGMInvalidRequest
	}
	if _, found, err := repositories.Account.Load(ctx, accountID); err != nil {
		return nil, err
	} else if !found {
		return []GMCharacterSummary{}, nil
	}
	records, err := repositories.Character.ListByAccount(ctx, accountID, dnfrepo.MaxCharacterSlotLimit)
	if err != nil {
		return nil, err
	}
	out := make([]GMCharacterSummary, 0, len(records))
	for _, record := range records {
		out = append(out, s.gmCharacterSummary(record))
	}
	sort.SliceStable(out, func(i, j int) bool {
		if out[i].Slot != out[j].Slot {
			return out[i].Slot < out[j].Slot
		}
		return out[i].CharacterID < out[j].CharacterID
	})
	return out, nil
}

func (s *Service) GMCharacter(ctx context.Context, characterID string) (GMCharacterSummary, error) {
	repositories, ok := s.repositoryGroup()
	characterID = strings.TrimSpace(characterID)
	if !ok || repositories.Character == nil || characterID == "" {
		return GMCharacterSummary{}, ErrGMInvalidRequest
	}
	record, found, err := repositories.Character.Load(ctx, characterID)
	if err != nil {
		return GMCharacterSummary{}, err
	}
	if !found {
		return GMCharacterSummary{}, ErrGMCharacterNotFound
	}
	return s.gmCharacterSummary(record), nil
}

func (s *Service) GMMailbox(ctx context.Context, characterID string) (GMMailboxSnapshot, error) {
	characterID, _, err := gmCharacterID(characterID)
	if err != nil {
		return GMMailboxSnapshot{}, err
	}
	repositories, ok := s.repositoryGroup()
	if !ok || repositories.Character == nil || repositories.Mailbox == nil {
		return GMMailboxSnapshot{}, ErrGMUnavailable
	}
	character, found, err := repositories.Character.Load(ctx, characterID)
	if err != nil {
		return GMMailboxSnapshot{}, err
	}
	if !found {
		return GMMailboxSnapshot{}, ErrGMCharacterNotFound
	}
	mailbox, found, err := repositories.Mailbox.Load(ctx, characterID)
	if err != nil {
		return GMMailboxSnapshot{}, err
	}
	if !found {
		mailbox = dnfrepo.MailboxRecord{CharacterID: characterID}
	}
	return s.gmMailboxSnapshot(character, mailbox), nil
}

func (s *Service) GMExtraEquipmentSlots(ctx context.Context, characterID string) (GMExtraEquipmentSlotState, error) {
	characterID, _, err := gmCharacterID(characterID)
	if err != nil {
		return GMExtraEquipmentSlotState{}, err
	}
	repositories, ok := s.repositoryGroup()
	if !ok || repositories.Character == nil {
		return GMExtraEquipmentSlotState{}, ErrGMUnavailable
	}
	character, found, err := repositories.Character.Load(ctx, characterID)
	if err != nil {
		return GMExtraEquipmentSlotState{}, err
	}
	if !found {
		return GMExtraEquipmentSlotState{}, ErrGMCharacterNotFound
	}
	return s.gmExtraEquipmentSlotState(character), nil
}

func (s *Service) GMUnlockExtraEquipmentSlots(ctx context.Context, characterID string, slots []string) (GMExtraEquipmentSlotState, error) {
	characterID, numericID, err := gmCharacterID(characterID)
	if err != nil {
		return GMExtraEquipmentSlotState{}, err
	}
	if _, online := s.onlineGameSession(numericID); online {
		return GMExtraEquipmentSlotState{}, ErrGMCharacterOnline
	}
	var unlockMask byte
	for _, raw := range slots {
		switch strings.ToLower(strings.TrimSpace(raw)) {
		case "support":
			unlockMask |= dnfquest.ExEquipSlotSupport
		case "magic_stone", "magic stone", "magicstone":
			unlockMask |= dnfquest.ExEquipSlotMagicStone
		case "earring":
			unlockMask |= dnfquest.ExEquipSlotEarring
		default:
			return GMExtraEquipmentSlotState{}, fmt.Errorf("%w: unsupported extra equipment slot %q", ErrGMInvalidRequest, raw)
		}
	}
	if unlockMask == 0 {
		return GMExtraEquipmentSlotState{}, fmt.Errorf("%w: select at least one extra equipment slot", ErrGMInvalidRequest)
	}
	repositories, ok := s.repositoryGroup()
	if !ok || repositories.CharacterAssets == nil {
		return GMExtraEquipmentSlotState{}, ErrGMUnavailable
	}
	var updated dnfrepo.CharacterRecord
	err = repositories.CharacterAssets.WithinCharacterAssets(ctx, characterID, func(
		characters dnfrepo.CharacterRepository,
		_ dnfrepo.InventoryRepository,
		_ dnfrepo.EquipmentRepository,
	) error {
		character, found, loadErr := characters.Load(ctx, characterID)
		if loadErr != nil {
			return loadErr
		}
		if !found {
			return ErrGMCharacterNotFound
		}
		character = dnfrepo.CloneCharacter(character)
		if character.Stats == nil {
			character.Stats = make(map[string]int64)
		}
		current := byte(character.Stats["ex_equip_slot_stat"]) & dnfquest.ExEquipSlotAll
		character.Stats["ex_equip_slot_stat"] = int64(current | unlockMask)
		character.UpdatedAt = time.Now().UTC()
		if saveErr := dnfrepo.SaveCharacterFields(ctx, characters, character, dnfrepo.CharacterFieldStats); saveErr != nil {
			return saveErr
		}
		updated = character
		return nil
	})
	if err != nil {
		return GMExtraEquipmentSlotState{}, err
	}
	return s.gmExtraEquipmentSlotState(updated), nil
}

func (s *Service) gmExtraEquipmentSlotState(character dnfrepo.CharacterRecord) GMExtraEquipmentSlotState {
	mask := byte(character.Stats["ex_equip_slot_stat"]) & dnfquest.ExEquipSlotAll
	return GMExtraEquipmentSlotState{
		Character:          s.gmCharacterSummary(character),
		Mask:               mask,
		SupportUnlocked:    mask&dnfquest.ExEquipSlotSupport != 0,
		MagicStoneUnlocked: mask&dnfquest.ExEquipSlotMagicStone != 0,
		EarringUnlocked:    mask&dnfquest.ExEquipSlotEarring != 0,
	}
}

func (s *Service) GMDeleteMailboxMails(ctx context.Context, characterID string, mailIDs []string, force bool) (GMMailDeleteResult, error) {
	characterID, numericID, err := gmCharacterID(characterID)
	if err != nil {
		return GMMailDeleteResult{}, err
	}
	if len(mailIDs) == 0 || len(mailIDs) > 100 {
		return GMMailDeleteResult{}, fmt.Errorf("%w: mail_ids must contain 1..100 entries", ErrGMInvalidRequest)
	}
	if _, online := s.onlineGameSession(numericID); online {
		return GMMailDeleteResult{}, ErrGMCharacterOnline
	}
	unique := make([]string, 0, len(mailIDs))
	seen := make(map[string]struct{}, len(mailIDs))
	for _, raw := range mailIDs {
		mailID := strings.TrimSpace(raw)
		if mailID == "" {
			return GMMailDeleteResult{}, fmt.Errorf("%w: mail id is empty", ErrGMInvalidRequest)
		}
		if _, duplicate := seen[mailID]; duplicate {
			continue
		}
		seen[mailID] = struct{}{}
		unique = append(unique, mailID)
	}
	repositories, ok := s.repositoryGroup()
	if !ok || repositories.MailboxAssets == nil {
		return GMMailDeleteResult{}, ErrGMUnavailable
	}
	var result GMMailDeleteResult
	err = repositories.MailboxAssets.WithinMailboxAssets(ctx, characterID, characterID, func(
		_ dnfrepo.AccountRepository,
		_ dnfrepo.AccountInventoryRepository,
		characters dnfrepo.CharacterRepository,
		_ dnfrepo.InventoryRepository,
		mailboxes dnfrepo.MailboxRepository,
	) error {
		character, found, loadErr := characters.Load(ctx, characterID)
		if loadErr != nil {
			return loadErr
		}
		if !found {
			return ErrGMCharacterNotFound
		}
		mailbox, found, loadErr := mailboxes.Load(ctx, characterID)
		if loadErr != nil {
			return loadErr
		}
		if !found {
			return fmt.Errorf("%w: mailbox is empty", ErrGMInvalidRequest)
		}
		discardedAssets := 0
		for _, mailID := range unique {
			mail, exists := mailbox.Mails[mailID]
			if !exists || mail.Deleted {
				return fmt.Errorf("%w: mail %s was not found", ErrGMInvalidRequest, mailID)
			}
			// 2026-10-17：网页后台（dnf90admin，无源码）删除邮件时不带 force，
			// 于是"带未领取附件的邮件"永远删不掉（报 still has unclaimed assets）。
			// 管理员主动删这封邮件本身就意味着丢弃它，这里直接按丢弃处理，
			// 只把被丢弃的资产数写进日志留痕；force 参数保留兼容老调用方。
			if !mail.Claimed && (mail.Gold > 0 || len(mail.Attachments) > 0) {
				discardedAssets++
			}
		}
		mailbox = dnfrepo.CloneMailbox(mailbox)
		for _, mailID := range unique {
			mail := mailbox.Mails[mailID]
			mail.Deleted = true
			mail.Read = true
			mail.Gold = 0
			mail.Attachments = nil
			mailbox.Mails[mailID] = mail
		}
		mailbox.UpdatedAt = time.Now().UTC()
		if saveErr := dnfrepo.SaveMailboxFields(ctx, mailboxes, mailbox, dnfrepo.MailboxFieldMails); saveErr != nil {
			return saveErr
		}
		if discardedAssets > 0 {
			s.logInfo("dnfbridge gm mail delete discarded unclaimed assets",
				"char_id", characterID, "mails", len(unique), "discarded_assets", discardedAssets)
		}
		result.Deleted = len(unique)
		result.Mailbox = s.gmMailboxSnapshot(character, mailbox)
		return nil
	})
	if err != nil {
		return GMMailDeleteResult{}, err
	}
	return result, nil
}

func (s *Service) gmMailboxSnapshot(character dnfrepo.CharacterRecord, mailbox dnfrepo.MailboxRecord) GMMailboxSnapshot {
	mails := make([]GMMailSummary, 0, len(mailbox.Mails))
	for key, mail := range mailbox.Mails {
		if mail.Deleted {
			continue
		}
		mailID := strings.TrimSpace(mail.MailID)
		if mailID == "" {
			mailID = key
		}
		attachments := make([]GMMailAttachmentSummary, 0, len(mail.Attachments))
		for _, attachment := range mail.Attachments {
			attachments = append(attachments, GMMailAttachmentSummary{
				ItemID: attachment.ItemID, Count: attachment.Count, Bind: attachment.Bind, ExpireAt: attachment.ExpireAt,
			})
		}
		mails = append(mails, GMMailSummary{
			MailID:      mailID,
			SenderName:  mail.SenderName,
			Title:       mail.Title,
			Body:        mail.Body,
			Gold:        mail.Gold,
			Attachments: attachments,
			Read:        mail.Read,
			Claimed:     mail.Claimed,
			Claimable:   !mail.Claimed && (mail.Gold > 0 || len(mail.Attachments) > 0),
			Saved:       strings.EqualFold(strings.TrimSpace(mail.Metadata["mailbox_saved"]), "true"),
			CreatedAt:   mail.CreatedAt,
			ExpireAt:    mail.ExpireAt,
		})
	}
	sort.SliceStable(mails, func(i, j int) bool {
		if !mails[i].CreatedAt.Equal(mails[j].CreatedAt) {
			return mails[i].CreatedAt.After(mails[j].CreatedAt)
		}
		left, leftErr := strconv.ParseUint(mails[i].MailID, 10, 64)
		right, rightErr := strconv.ParseUint(mails[j].MailID, 10, 64)
		if leftErr == nil && rightErr == nil {
			return left > right
		}
		return mails[i].MailID > mails[j].MailID
	})
	return GMMailboxSnapshot{Character: s.gmCharacterSummary(character), Mails: mails}
}

func (s *Service) GMCareer(ctx context.Context, characterID string) (GMCareerState, error) {
	characterID, _, err := gmCharacterID(characterID)
	if err != nil {
		return GMCareerState{}, err
	}
	repositories, ok := s.repositoryGroup()
	if !ok || repositories.Character == nil {
		return GMCareerState{}, ErrGMUnavailable
	}
	record, found, err := repositories.Character.Load(ctx, characterID)
	if err != nil {
		return GMCareerState{}, err
	}
	if !found {
		return GMCareerState{}, ErrGMCharacterNotFound
	}
	profiles, _, err := s.currentProfessionResources(ctx)
	if err != nil {
		return GMCareerState{}, err
	}
	return s.gmCareerState(record, profiles, false)
}

// GMSetCareer applies the requested final profession state as one administrative
// class rebuild. Unlike an ordinary awakening quest, a direct GM edit always
// clears incompatible learned skills and rebuilds the complete PVF free-skill
// baseline for the selected job/branch/stage.
func (s *Service) GMSetCareer(ctx context.Context, characterID string, job int, first int, awakening int) (GMCareerState, error) {
	if job < 0 || job > math.MaxUint8 || first < 0 || first > 0x0f || awakening < 0 || awakening > 0x0f {
		return GMCareerState{}, fmt.Errorf("%w: profession selection is out of range", ErrGMInvalidRequest)
	}
	characterID, numericID, err := gmCharacterID(characterID)
	if err != nil {
		return GMCareerState{}, err
	}
	if _, online := s.onlineGameSession(numericID); online {
		return GMCareerState{}, ErrGMCharacterOnline
	}
	profiles, skillCatalog, err := s.currentProfessionResources(ctx)
	if err != nil {
		return GMCareerState{}, err
	}
	jobByte, firstByte, awakeningByte := byte(job), byte(first), byte(awakening)
	if !profiles.SupportsSelection(jobByte, firstByte, awakeningByte) {
		return GMCareerState{}, fmt.Errorf("%w: selection is not present in the active Script.pvf", ErrGMInvalidRequest)
	}
	packed, err := dnfprofession.Encode(firstByte, awakeningByte)
	if err != nil {
		return GMCareerState{}, fmt.Errorf("%w: %v", ErrGMInvalidRequest, err)
	}
	var questCatalog *dnfquest.Catalog
	if firstByte == 0 && awakeningByte == 0 {
		questCatalog, err = s.loadQuestCatalog(ctx)
		if err != nil {
			return GMCareerState{}, err
		}
	}
	repositories, ok := s.repositoryGroup()
	if !ok || repositories.CharacterSettlement == nil {
		return GMCareerState{}, ErrGMUnavailable
	}
	var updated dnfrepo.CharacterRecord
	var questResetIDs []int64
	err = repositories.CharacterSettlement.WithinCharacterSettlement(ctx, characterID, func(tx dnfrepo.Group) error {
		if tx.Character == nil || tx.Skill == nil || (questCatalog != nil && tx.Quest == nil) {
			return ErrGMUnavailable
		}
		character, found, loadErr := tx.Character.Load(ctx, characterID)
		if loadErr != nil {
			return loadErr
		}
		if !found {
			return ErrGMCharacterNotFound
		}
		skill, found, loadErr := tx.Skill.Load(ctx, characterID)
		if loadErr != nil {
			return loadErr
		}
		if !found {
			return fmt.Errorf("%w: skill ledger is missing", ErrGMInvalidRequest)
		}
		character = dnfrepo.CloneCharacter(character)
		skill = dnfrepo.CloneSkill(skill)
		previousGrowType := byte(numericCharacterStatValue(character, "grow_type"))
		character.Job = strconv.Itoa(job)
		if character.Stats == nil {
			character.Stats = make(map[string]int64)
		}
		character.Stats["grow_type"] = int64(packed)
		transition := dnfprofession.Transition{
			Kind:             dnfprofession.KindClassChange,
			PreviousGrowType: previousGrowType,
			NewGrowType:      packed,
			FirstGrowType:    firstByte,
			AwakeningStage:   awakeningByte,
		}
		skill, loadErr = profiles.ApplySkillTransition(skillCatalog, jobByte, character.Level, transition, skill, skill.Points)
		if loadErr != nil {
			return loadErr
		}
		stats, loadErr := s.characterPVFStatValuesForLevel(character, character.Level)
		if loadErr != nil {
			return loadErr
		}
		for key, value := range stats {
			character.Stats[key] = value
		}
		now := time.Now().UTC()
		character.UpdatedAt = now
		skill.UpdatedAt = now
		if questCatalog != nil {
			questRecord, questFound, questErr := tx.Quest.Load(ctx, characterID)
			if questErr != nil {
				return questErr
			}
			if questFound {
				resetPlan, planErr := questCatalog.PlanBaseProfessionQuestReset(questRecord, now)
				if planErr != nil {
					return planErr
				}
				if len(resetPlan.ChangedFields) != 0 {
					if saveErr := dnfrepo.SaveQuestFields(ctx, tx.Quest, resetPlan.Record, resetPlan.ChangedFields...); saveErr != nil {
						return saveErr
					}
				}
				questResetIDs = append(questResetIDs[:0], resetPlan.ResetQuestIDs...)
			}
		}
		if saveErr := dnfrepo.SaveCharacterFields(ctx, tx.Character, character, dnfrepo.CharacterFieldBase, dnfrepo.CharacterFieldStats); saveErr != nil {
			return saveErr
		}
		if saveErr := dnfrepo.SaveSkillFields(ctx, tx.Skill, skill,
			dnfrepo.SkillFieldSkills,
			dnfrepo.SkillFieldPoints,
			dnfrepo.SkillFieldLayouts,
			dnfrepo.SkillFieldCooldowns,
		); saveErr != nil {
			return saveErr
		}
		updated = character
		return nil
	})
	if err != nil {
		return GMCareerState{}, err
	}
	state, err := s.gmCareerState(updated, profiles, true)
	if err != nil {
		return GMCareerState{}, err
	}
	state.QuestResetIDs = questResetIDs
	return state, nil
}

func (s *Service) GMSetCharacterGold(ctx context.Context, characterID string, value int64) (GMCharacterSummary, error) {
	if value < 0 || value > math.MaxUint32 {
		return GMCharacterSummary{}, fmt.Errorf("%w: gold must be between 0 and %d", ErrGMInvalidRequest, uint64(math.MaxUint32))
	}
	characterID, numericID, err := gmCharacterID(characterID)
	if err != nil {
		return GMCharacterSummary{}, err
	}
	if _, online := s.onlineGameSession(numericID); online {
		return GMCharacterSummary{}, ErrGMCharacterOnline
	}
	repositories, ok := s.repositoryGroup()
	if !ok || repositories.CharacterAssets == nil {
		return GMCharacterSummary{}, ErrGMUnavailable
	}
	var updated dnfrepo.CharacterRecord
	err = repositories.CharacterAssets.WithinCharacterAssets(ctx, characterID, func(
		characters dnfrepo.CharacterRepository,
		_ dnfrepo.InventoryRepository,
		_ dnfrepo.EquipmentRepository,
	) error {
		record, found, loadErr := characters.Load(ctx, characterID)
		if loadErr != nil {
			return loadErr
		}
		if !found {
			return ErrGMCharacterNotFound
		}
		record = dnfrepo.CloneCharacter(record)
		if record.Stats == nil {
			record.Stats = make(map[string]int64)
		}
		record.Stats["gold"] = value
		record.UpdatedAt = time.Now().UTC()
		if saveErr := dnfrepo.SaveCharacterFields(ctx, characters, record, dnfrepo.CharacterFieldStats); saveErr != nil {
			return saveErr
		}
		updated = record
		return nil
	})
	if err != nil {
		return GMCharacterSummary{}, err
	}
	return s.gmCharacterSummary(updated), nil
}

func (s *Service) GMSetCharacterLevelForTool(ctx context.Context, characterID string, level int) (GMCharacterSummary, error) {
	if level < 1 || level > currentAdventureCharacterLevelCap {
		return GMCharacterSummary{}, fmt.Errorf("%w: level must be between 1 and %d", ErrGMInvalidRequest, currentAdventureCharacterLevelCap)
	}
	characterID, numericID, err := gmCharacterID(characterID)
	if err != nil {
		return GMCharacterSummary{}, err
	}
	if _, online := s.onlineGameSession(numericID); online {
		return GMCharacterSummary{}, ErrGMCharacterOnline
	}
	repositories, ok := s.repositoryGroup()
	if !ok || repositories.Character == nil || repositories.Skill == nil || repositories.CharacterProgression == nil {
		return GMCharacterSummary{}, ErrGMUnavailable
	}
	character, found, err := repositories.Character.Load(ctx, characterID)
	if err != nil {
		return GMCharacterSummary{}, err
	}
	if !found {
		return GMCharacterSummary{}, ErrGMCharacterNotFound
	}
	if level < character.Level {
		return GMCharacterSummary{}, fmt.Errorf("%w: level reduction is not supported; current=%d target=%d", ErrGMInvalidRequest, character.Level, level)
	}
	if level == character.Level {
		return s.gmCharacterSummary(character), nil
	}
	skill, found, err := repositories.Skill.Load(ctx, characterID)
	if err != nil {
		return GMCharacterSummary{}, err
	}
	if !found {
		return GMCharacterSummary{}, fmt.Errorf("%w: skill ledger is missing", ErrGMInvalidRequest)
	}
	experience64 := character.Stats["exp"]
	if experience64 < 0 || experience64 > math.MaxUint32 {
		return GMCharacterSummary{}, fmt.Errorf("%w: stored experience is invalid", ErrGMInvalidRequest)
	}
	s.initialEquipmentMu.Lock()
	archive, err := s.initialEquipmentArchiveLocked()
	s.initialEquipmentMu.Unlock()
	if err != nil {
		return GMCharacterSummary{}, err
	}
	tables, err := dnfprogression.Load(ctx, archive)
	if err != nil {
		return GMCharacterSummary{}, err
	}
	targetExperience := uint32(experience64)
	if level > 1 {
		targetExperience, err = tables.ThresholdToNext(level - 1)
		if err != nil {
			return GMCharacterSummary{}, err
		}
	}
	if targetExperience < uint32(experience64) {
		targetExperience = uint32(experience64)
	}
	advance, err := tables.AdvanceSkillPoints(skill.Points, level)
	if err != nil {
		return GMCharacterSummary{}, err
	}
	levelStats, err := s.characterPVFStatValuesForLevel(character, level)
	if err != nil {
		return GMCharacterSummary{}, err
	}
	_, err = dnfprogression.Commit(ctx, repositories.CharacterProgression, dnfprogression.CommitPlan{
		CharacterID: characterID,
		AccountID:   character.AccountID,
		Expected: dnfprogression.CommitState{
			Level:       character.Level,
			Experience:  uint32(experience64),
			SkillPoints: skill.Points,
		},
		Next: dnfprogression.CommitState{
			Level:       level,
			Experience:  targetExperience,
			SkillPoints: advance.New,
		},
		NextCharacterStats: levelStats,
	})
	if err != nil {
		return GMCharacterSummary{}, err
	}
	return s.GMCharacter(ctx, characterID)
}

func (s *Service) GMQuestOverview(ctx context.Context, characterID string) (GMQuestOverview, error) {
	character, record, catalog, err := s.gmQuestContext(ctx, characterID)
	if err != nil {
		return GMQuestOverview{}, err
	}
	seen := make(map[int64]struct{}, len(record.States)+len(record.Progress))
	active := make([]GMQuestSummary, 0)
	completed := make([]GMQuestSummary, 0)
	titleMapping := s.currentTitleBookMapping()
	appendState := func(questID int64, state dnfrepo.QuestState) {
		if _, exists := seen[questID]; exists {
			return
		}
		seen[questID] = struct{}{}
		definition, known := catalog.Find(questID)
		if !known {
			definition = dnfquest.Definition{ID: questID}
		}
		summary := gmQuestSummaryWithTitle(definition, state, false, titleMapping)
		if summary.Active {
			active = append(active, summary)
		}
		if summary.Completed {
			completed = append(completed, summary)
		}
	}
	for questID, state := range record.States {
		appendState(questID, state)
	}
	for questID, state := range record.Progress {
		appendState(questID, state)
	}
	sort.Slice(active, func(i, j int) bool { return active[i].QuestID < active[j].QuestID })
	sort.Slice(completed, func(i, j int) bool { return completed[i].QuestID < completed[j].QuestID })
	return GMQuestOverview{Character: s.gmCharacterSummary(character), Active: active, Completed: completed}, nil
}

func (s *Service) GMSearchQuests(ctx context.Context, characterID string, query string, limit int) ([]GMQuestSummary, error) {
	character, record, catalog, err := s.gmQuestContext(ctx, characterID)
	if err != nil {
		return nil, err
	}
	job, err := strconv.Atoi(strings.TrimSpace(character.Job))
	if err != nil || job < 0 || job > math.MaxUint8 {
		return nil, fmt.Errorf("%w: stored character job is invalid", ErrGMInvalidRequest)
	}
	available := catalog.Acceptable(dnfquest.CharacterEligibility{
		Level: character.Level, Job: job, GrowType: int(numericCharacterStatValue(character, "grow_type")),
	}, record)
	acceptable := make(map[int64]struct{}, len(available.IDs))
	for _, questID := range available.IDs {
		acceptable[int64(questID)] = struct{}{}
	}
	definitions := catalog.Search(query, limit)
	titleMapping := s.currentTitleBookMapping()
	out := make([]GMQuestSummary, 0, len(definitions))
	for _, definition := range definitions {
		state, _ := gmQuestState(record, definition.ID)
		_, canAccept := acceptable[definition.ID]
		out = append(out, gmQuestSummaryWithTitle(definition, state, canAccept, titleMapping))
	}
	return out, nil
}

func (s *Service) GMQuestDetail(ctx context.Context, characterID string, questID int64) (GMQuestDetail, error) {
	if questID <= 0 || questID > math.MaxUint16 {
		return GMQuestDetail{}, fmt.Errorf("%w: quest id must be between 1 and %d", ErrGMInvalidRequest, uint64(math.MaxUint16))
	}
	character, record, catalog, err := s.gmQuestContext(ctx, characterID)
	if err != nil {
		return GMQuestDetail{}, err
	}
	definition, found := catalog.Find(questID)
	if !found {
		return GMQuestDetail{}, fmt.Errorf("%w: quest %d is not present in the active Script.pvf", ErrGMInvalidRequest, questID)
	}
	job, err := strconv.Atoi(strings.TrimSpace(character.Job))
	if err != nil || job < 0 || job > math.MaxUint8 {
		return GMQuestDetail{}, fmt.Errorf("%w: stored character job is invalid", ErrGMInvalidRequest)
	}
	state, _ := gmQuestState(record, questID)
	acceptable := false
	if !gmQuestStatusActive(state.Status) && !gmQuestStatusCompleted(state.Status) {
		available := catalog.Acceptable(dnfquest.CharacterEligibility{
			Level: character.Level, Job: job, GrowType: int(numericCharacterStatValue(character, "grow_type")),
		}, record)
		for _, candidate := range available.IDs {
			if int64(candidate) == questID {
				acceptable = true
				break
			}
		}
	}
	detail := GMQuestDetail{
		Quest:                gmQuestSummaryWithTitle(definition, state, acceptable, s.currentTitleBookMapping()),
		Introduction:         strings.TrimSpace(definition.DependMessage),
		Objective:            strings.TrimSpace(definition.ConditionMessage),
		Completion:           strings.TrimSpace(definition.SolveMessage),
		Difficulty:           strings.TrimSpace(definition.Difficulty),
		Job:                  strings.TrimSpace(definition.Job),
		NoExperience:         definition.NoExperience,
		GoldMultiple:         definition.GoldMultiple,
		RewardItems:          s.gmQuestRewardItems(definition.RewardItems, false),
		RewardSelectionItems: s.gmQuestRewardItems(definition.RewardSelectionItems, true),
	}
	if len(detail.RewardItems) == 0 && normalizeMagicBoxPVFType(definition.RewardType) == "title" && len(definition.RewardIntData) >= 2 {
		detail.RewardItems = s.gmQuestRewardItemIDs(definition.RewardIntData[0], definition.RewardIntData[1], false)
	}
	return detail, nil
}

func (s *Service) gmQuestRewardItems(rules []dnfquest.RewardItemRule, selectable bool) []GMQuestRewardItem {
	if len(rules) == 0 {
		return nil
	}
	out := make([]GMQuestRewardItem, 0, len(rules))
	for _, rule := range rules {
		if rule.ItemID <= 0 || rule.Count <= 0 {
			continue
		}
		items := s.gmQuestRewardItemIDs(rule.ItemID, rule.Count, selectable)
		if len(items) == 0 {
			continue
		}
		items[0].HasJobFilter = rule.HasJobFilter
		items[0].RequiredJob = rule.Job
		items[0].RequiredGrowType = rule.GrowType
		out = append(out, items[0])
	}
	return out
}

func (s *Service) gmQuestRewardItemIDs(itemID, count int64, selectable bool) []GMQuestRewardItem {
	if itemID <= 0 || itemID > math.MaxUint32 || count <= 0 {
		return nil
	}
	item := GMQuestRewardItem{ItemID: itemID, Count: count, Selectable: selectable}
	if catalog, err := s.currentPVFItemCatalog(); err == nil {
		if definition, resolveErr := catalog.ResolveItem(uint32(itemID)); resolveErr == nil {
			item.Name = strings.TrimSpace(definition.Name)
		}
	}
	return []GMQuestRewardItem{item}
}

func (s *Service) GMAcceptQuest(ctx context.Context, characterID string, questID int64) (GMQuestMutationResult, error) {
	characterID, numericID, err := gmQuestMutationIDs(characterID, questID)
	if err != nil {
		return GMQuestMutationResult{}, err
	}
	if _, online := s.onlineGameSession(numericID); online {
		return GMQuestMutationResult{}, ErrGMCharacterOnline
	}
	character, _, catalog, err := s.gmQuestContext(ctx, characterID)
	if err != nil {
		return GMQuestMutationResult{}, err
	}
	job, err := strconv.Atoi(strings.TrimSpace(character.Job))
	if err != nil || job < 0 || job > math.MaxUint8 {
		return GMQuestMutationResult{}, fmt.Errorf("%w: stored character job is invalid", ErrGMInvalidRequest)
	}
	repositories, ok := s.repositoryGroup()
	if !ok {
		return GMQuestMutationResult{}, ErrGMUnavailable
	}
	owner, err := dnfquest.NewOwner(repositories)
	if err != nil {
		return GMQuestMutationResult{}, ErrGMUnavailable
	}
	_, err = owner.ApplyAccept(ctx, catalog, dnfquest.CharacterEligibility{
		Level: character.Level, Job: job, GrowType: int(numericCharacterStatValue(character, "grow_type")),
	}, dnfquest.Command{Operation: "accept_quest", AccountID: character.AccountID, SelectedCharacterID: numericID, QuestID: uint16(questID)})
	if err != nil {
		return GMQuestMutationResult{}, fmt.Errorf("%w: accept quest %d: %v", ErrGMInvalidRequest, questID, err)
	}
	return s.gmQuestMutationResult(ctx, characterID, questID, false, false)
}

func (s *Service) GMGiveUpQuest(ctx context.Context, characterID string, questID int64) (GMQuestMutationResult, error) {
	characterID, numericID, err := gmQuestMutationIDs(characterID, questID)
	if err != nil {
		return GMQuestMutationResult{}, err
	}
	if _, online := s.onlineGameSession(numericID); online {
		return GMQuestMutationResult{}, ErrGMCharacterOnline
	}
	_, _, catalog, err := s.gmQuestContext(ctx, characterID)
	if err != nil {
		return GMQuestMutationResult{}, err
	}
	repositories, ok := s.repositoryGroup()
	if !ok {
		return GMQuestMutationResult{}, ErrGMUnavailable
	}
	owner, err := dnfquest.NewOwner(repositories)
	if err != nil {
		return GMQuestMutationResult{}, ErrGMUnavailable
	}
	// 与客户端放弃走同一套语义（允许放弃 + 只回收任务专属道具），
	// 否则 GM 处理完的世界状态和玩家自己点放弃的结果会不一致。
	_, err = owner.ApplyGiveUpSettlement(ctx, catalog, dnfquest.GiveUpInput{
		SelectedCharacterID: numericID,
		QuestID:             uint16(questID),
		IsQuestItem:         s.currentGiveUpQuestItemClassifier(nil),
	})
	if err != nil {
		return GMQuestMutationResult{}, fmt.Errorf("%w: give up quest %d: %v", ErrGMInvalidRequest, questID, err)
	}
	return s.gmQuestMutationResult(ctx, characterID, questID, false, false)
}

func (s *Service) GMMarkQuestReady(ctx context.Context, characterID string, questID int64) (GMQuestMutationResult, error) {
	characterID, numericID, err := gmQuestMutationIDs(characterID, questID)
	if err != nil {
		return GMQuestMutationResult{}, err
	}
	if _, online := s.onlineGameSession(numericID); online {
		return GMQuestMutationResult{}, ErrGMCharacterOnline
	}
	repositories, ok := s.repositoryGroup()
	if !ok || repositories.CharacterSettlement == nil {
		return GMQuestMutationResult{}, ErrGMUnavailable
	}
	err = repositories.CharacterSettlement.WithinCharacterSettlement(ctx, characterID, func(tx dnfrepo.Group) error {
		if tx.Character == nil || tx.Quest == nil {
			return ErrGMUnavailable
		}
		if _, found, loadErr := tx.Character.Load(ctx, characterID); loadErr != nil {
			return loadErr
		} else if !found {
			return ErrGMCharacterNotFound
		}
		record, found, loadErr := tx.Quest.Load(ctx, characterID)
		if loadErr != nil {
			return loadErr
		}
		if !found {
			return fmt.Errorf("%w: quest is not active", ErrGMInvalidRequest)
		}
		record = dnfrepo.CloneQuest(record)
		state, field, found := gmMutableQuestState(record, questID)
		if !found || !gmQuestStatusActive(state.Status) {
			return fmt.Errorf("%w: quest is not active", ErrGMInvalidRequest)
		}
		state.ProgressValue = 0
		state.UpdatedAt = time.Now().UTC()
		if state.Extra == nil {
			state.Extra = make(map[string]string)
		}
		for _, key := range []string{"completion_key", "completion_kind", "reward_state", "settlement_receipt_v1"} {
			delete(state.Extra, key)
		}
		state.Extra["gm_marked_ready"] = "true"
		gmPutQuestState(&record, questID, state, field)
		record.UpdatedAt = state.UpdatedAt
		return dnfrepo.SaveQuestFields(ctx, tx.Quest, record, field)
	})
	if err != nil {
		return GMQuestMutationResult{}, err
	}
	return s.gmQuestMutationResult(ctx, characterID, questID, false, false)
}

func (s *Service) GMForceCompleteQuest(ctx context.Context, characterID string, questID int64) (GMQuestMutationResult, error) {
	characterID, numericID, err := gmQuestMutationIDs(characterID, questID)
	if err != nil {
		return GMQuestMutationResult{}, err
	}
	if _, online := s.onlineGameSession(numericID); online {
		return GMQuestMutationResult{}, ErrGMCharacterOnline
	}
	catalog, err := s.loadQuestCatalog(ctx)
	if err != nil {
		return GMQuestMutationResult{}, err
	}
	definition, found := catalog.Find(questID)
	if !found {
		return GMQuestMutationResult{}, fmt.Errorf("%w: quest %d is not present in the active Script.pvf", ErrGMInvalidRequest, questID)
	}
	professionQuest := gmProfessionRewardType(definition.RewardType)
	var profiles *dnfprofession.Profiles
	var skillCatalog *dnfskill.Table
	if professionQuest {
		profiles, skillCatalog, err = s.currentProfessionResources(ctx)
		if err != nil {
			return GMQuestMutationResult{}, err
		}
	}
	repositories, ok := s.repositoryGroup()
	if !ok || repositories.CharacterSettlement == nil {
		return GMQuestMutationResult{}, ErrGMUnavailable
	}
	professionChanged := false
	err = repositories.CharacterSettlement.WithinCharacterSettlement(ctx, characterID, func(tx dnfrepo.Group) error {
		if tx.Character == nil || tx.Quest == nil {
			return ErrGMUnavailable
		}
		character, found, loadErr := tx.Character.Load(ctx, characterID)
		if loadErr != nil {
			return loadErr
		}
		if !found {
			return ErrGMCharacterNotFound
		}
		record, found, loadErr := tx.Quest.Load(ctx, characterID)
		if loadErr != nil {
			return loadErr
		}
		if !found {
			record = dnfrepo.QuestRecord{CharacterID: characterID}
		} else {
			record = dnfrepo.CloneQuest(record)
		}
		if state, known := gmQuestState(record, questID); known && gmQuestStatusCompleted(state.Status) {
			return nil
		}
		character = dnfrepo.CloneCharacter(character)
		if professionQuest {
			if tx.Skill == nil {
				return ErrGMUnavailable
			}
			job, parseErr := strconv.ParseUint(strings.TrimSpace(character.Job), 10, 8)
			grow := numericCharacterStatValue(character, "grow_type")
			if parseErr != nil || grow < 0 || grow > math.MaxUint8 {
				return fmt.Errorf("%w: stored profession state is invalid", ErrGMInvalidRequest)
			}
			request, transitionErr := dnfprofession.ParseReward(definition.JobChangeQuest, definition.RewardType, definition.RewardIntData)
			if transitionErr != nil {
				return fmt.Errorf("%w: %v", ErrGMInvalidRequest, transitionErr)
			}
			transition, transitionErr := profiles.PlanTransition(byte(job), byte(grow), request)
			if transitionErr != nil {
				return fmt.Errorf("%w: %v", ErrGMInvalidRequest, transitionErr)
			}
			skill, skillFound, skillErr := tx.Skill.Load(ctx, characterID)
			if skillErr != nil {
				return skillErr
			}
			if !skillFound {
				return fmt.Errorf("%w: skill ledger is missing", ErrGMInvalidRequest)
			}
			skill, skillErr = profiles.ApplySkillTransition(skillCatalog, byte(job), character.Level, transition, skill, skill.Points)
			if skillErr != nil {
				return skillErr
			}
			if character.Stats == nil {
				character.Stats = make(map[string]int64)
			}
			character.Stats["grow_type"] = int64(transition.NewGrowType)
			stats, statErr := s.characterPVFStatValuesForLevel(character, character.Level)
			if statErr != nil {
				return statErr
			}
			for key, value := range stats {
				character.Stats[key] = value
			}
			now := time.Now().UTC()
			character.UpdatedAt = now
			skill.UpdatedAt = now
			if saveErr := dnfrepo.SaveCharacterFields(ctx, tx.Character, character, dnfrepo.CharacterFieldStats); saveErr != nil {
				return saveErr
			}
			if saveErr := dnfrepo.SaveSkillFields(ctx, tx.Skill, skill,
				dnfrepo.SkillFieldSkills, dnfrepo.SkillFieldPoints, dnfrepo.SkillFieldLayouts, dnfrepo.SkillFieldCooldowns,
			); saveErr != nil {
				return saveErr
			}
			professionChanged = true
		}
		now := time.Now().UTC()
		if record.States == nil {
			record.States = make(map[int64]dnfrepo.QuestState)
		}
		state, _ := gmQuestState(record, questID)
		if state.Extra == nil {
			state.Extra = make(map[string]string)
		}
		state.Status = "completed"
		state.ProgressValue = 0
		state.UpdatedAt = now
		state.Extra["pvf_path"] = definition.Path
		state.Extra["gm_force_completed"] = "true"
		state.Extra["gm_rewards_granted"] = "false"
		for _, key := range []string{"completion_key", "completion_kind", "reward_state", "settlement_receipt_v1"} {
			delete(state.Extra, key)
		}
		record.States[questID] = state
		delete(record.Progress, questID)
		record.UpdatedAt = now
		return dnfrepo.SaveQuestFields(ctx, tx.Quest, record, dnfrepo.QuestFieldStates, dnfrepo.QuestFieldProgress)
	})
	if err != nil {
		return GMQuestMutationResult{}, err
	}
	return s.gmQuestMutationResult(ctx, characterID, questID, false, professionChanged)
}

func (s *Service) GMUncompleteQuest(ctx context.Context, characterID string, questID int64) (GMQuestMutationResult, error) {
	characterID, numericID, err := gmQuestMutationIDs(characterID, questID)
	if err != nil {
		return GMQuestMutationResult{}, err
	}
	if _, online := s.onlineGameSession(numericID); online {
		return GMQuestMutationResult{}, ErrGMCharacterOnline
	}
	catalog, err := s.loadQuestCatalog(ctx)
	if err != nil {
		return GMQuestMutationResult{}, err
	}
	definition, definitionKnown := catalog.Find(questID)
	if !definitionKnown {
		return GMQuestMutationResult{}, fmt.Errorf("%w: quest definition is missing", ErrGMInvalidRequest)
	}
	repositories, ok := s.repositoryGroup()
	if !ok || repositories.CharacterSettlement == nil {
		return GMQuestMutationResult{}, ErrGMUnavailable
	}
	err = repositories.CharacterSettlement.WithinCharacterSettlement(ctx, characterID, func(tx dnfrepo.Group) error {
		if tx.Character == nil || tx.Quest == nil {
			return ErrGMUnavailable
		}
		character, found, loadErr := tx.Character.Load(ctx, characterID)
		if loadErr != nil {
			return loadErr
		}
		if !found {
			return ErrGMCharacterNotFound
		}
		record, found, loadErr := tx.Quest.Load(ctx, characterID)
		if loadErr != nil {
			return loadErr
		}
		if !found {
			return nil
		}
		record = dnfrepo.CloneQuest(record)
		if definition.MainQuestID > 0 {
			job, parseErr := strconv.Atoi(strings.TrimSpace(character.Job))
			if parseErr != nil || job < 0 || job > math.MaxUint8 {
				return fmt.Errorf("%w: stored character job is invalid", ErrGMInvalidRequest)
			}
			plan, planErr := catalog.PlanLinkedQuestChainReset(dnfquest.CharacterEligibility{
				Level:    character.Level,
				Job:      job,
				GrowType: int(numericCharacterStatValue(character, "grow_type")),
			}, record, questID, time.Now().UTC())
			if planErr != nil {
				return fmt.Errorf("%w: reset linked quest chain for %d: %v", ErrGMInvalidRequest, questID, planErr)
			}
			return dnfrepo.SaveQuestFields(ctx, tx.Quest, plan.Record, plan.ChangedFields...)
		}
		if state, known := gmQuestState(record, questID); known && !gmQuestStatusCompleted(state.Status) {
			return fmt.Errorf("%w: quest is not completed", ErrGMInvalidRequest)
		}
		delete(record.States, questID)
		delete(record.Progress, questID)
		record.UpdatedAt = time.Now().UTC()
		return dnfrepo.SaveQuestFields(ctx, tx.Quest, record, dnfrepo.QuestFieldStates, dnfrepo.QuestFieldProgress)
	})
	if err != nil {
		return GMQuestMutationResult{}, err
	}
	return s.gmQuestMutationResult(ctx, characterID, questID, false, false)
}

func (s *Service) GMSearchItems(ctx context.Context, query string, limit int) ([]GMItemSummary, error) {
	if ctx == nil {
		ctx = context.Background()
	}
	if limit <= 0 {
		limit = 30
	}
	if limit > 100 {
		limit = 100
	}
	catalog, err := s.currentPVFItemCatalog()
	if err != nil {
		return nil, err
	}
	query = strings.ToLower(strings.TrimSpace(query))
	if itemID, parseErr := strconv.ParseUint(query, 10, 32); parseErr == nil && itemID > 0 {
		definition, resolveErr := catalog.ResolveItem(uint32(itemID))
		if resolveErr != nil {
			if errors.Is(resolveErr, errDungeonDropItemUnresolved) {
				return []GMItemSummary{}, nil
			}
			return nil, resolveErr
		}
		return []GMItemSummary{gmItemSummary(definition)}, nil
	}
	catalog.mu.Lock()
	ids := make([]uint32, 0, len(catalog.itemRefs))
	for itemID := range catalog.itemRefs {
		ids = append(ids, itemID)
	}
	catalog.mu.Unlock()
	sort.Slice(ids, func(i, j int) bool { return ids[i] < ids[j] })
	out := make([]GMItemSummary, 0, limit)
	for _, itemID := range ids {
		if err := ctx.Err(); err != nil {
			return nil, err
		}
		definition, resolveErr := catalog.ResolveItem(itemID)
		if resolveErr != nil {
			continue
		}
		item := gmItemSummary(definition)
		if query != "" &&
			!strings.Contains(strings.ToLower(item.Name), query) &&
			!strings.Contains(strings.ToLower(item.PVFPath), query) {
			continue
		}
		out = append(out, item)
		if len(out) >= limit {
			break
		}
	}
	return out, nil
}

func (s *Service) GMItemDetail(ctx context.Context, itemID uint32) (GMItemDetail, error) {
	if ctx == nil {
		ctx = context.Background()
	}
	if itemID == 0 {
		return GMItemDetail{}, ErrGMInvalidRequest
	}
	catalog, err := s.currentPVFItemCatalog()
	if err != nil {
		return GMItemDetail{}, err
	}
	definition, err := catalog.ResolveItem(itemID)
	if err != nil {
		return GMItemDetail{}, err
	}
	if err := ctx.Err(); err != nil {
		return GMItemDetail{}, err
	}
	text, err := catalog.source.ReadText(definition.PVFPath)
	if err != nil {
		return GMItemDetail{}, fmt.Errorf("read GM item detail item=%d path=%s: %w", itemID, definition.PVFPath, err)
	}
	document, err := dnfpvf.Parse(definition.PVFPath, text)
	if err != nil {
		return GMItemDetail{}, fmt.Errorf("parse GM item detail item=%d path=%s: %w", itemID, definition.PVFPath, err)
	}
	detail := GMItemDetail{
		Item:          gmItemSummary(definition),
		Explain:       gmItemDetailText(document, "explain"),
		BasicExplain:  gmItemDetailText(document, "basic explain"),
		DetailExplain: gmItemDetailText(document, "detail explain"),
		FlavorText:    gmItemDetailText(document, "flavor text"),
		Durability:    int64(definition.Durability),
	}
	detail.Weight, _ = document.Int("weight")
	detail.RepairPrice, _ = document.Int("repair price")
	detail.Value, _ = document.Int("value")
	if definition.Kind == dungeonDropItemEquipment {
		detail.QualityPercent = 100
		detail.WeaponAttackSpeed = gmEquipmentWeaponAttackSpeed(document, definition.EquipmentType)
		detail.ItemGroupName = gmItemDetailText(document, "item group name")
		detail.PartSetIndex, _ = document.Int("part set index")
		if detail.PartSetIndex > 0 {
			detail.EquipmentSet, err = gmEquipmentSetDetail(catalog.source, detail.PartSetIndex)
			if err != nil {
				return GMItemDetail{}, fmt.Errorf("load GM item set detail item=%d set=%d: %w", itemID, detail.PartSetIndex, err)
			}
		}
		detail.ElementalProperties = gmEquipmentElementalProperties(document)
		detail.Attributes = gmEquipmentDetailAttributes(document)
		detail.RequiredSkills = gmItemRequiredSkills(document)
		detail.SkillLevelUps = gmMailOptionSkillLevelUps(document)
		detail.SkillModifiers = gmWeaponSkillModifiers(detail.ItemGroupName)
		if skillCatalog, skillErr := s.loadSkillCatalog(ctx, catalog.source); skillErr == nil {
			gmResolveItemSkillNames(skillCatalog, detail.RequiredSkills, detail.SkillLevelUps)
		}
	}
	detail.AvatarVariations = gmAvatarVariations(document)
	switch gmItemTypeTag(definition.EquipmentType) {
	case "weapon avatar":
		detail.AvatarWeaponGroup = gmAvatarWeaponGroup(document)
		if len(detail.AvatarVariations) == 0 {
			detail.AvatarVariations = gmWeaponAvatarVariations(document, definition.UsableJobs)
		}
	case "aurora avatar":
		detail.AvatarAuraAnimations = gmAvatarAuraAnimations(catalog.source, document)
	}
	return detail, nil
}

func gmAvatarAuraAnimations(source dnfpvf.Source, document *dnfpvf.Document) []GMAvatarAuraAnimation {
	if source == nil || document == nil {
		return nil
	}
	tokens, ok := document.Section("aurora graphic effects")
	if !ok {
		return nil
	}
	candidates := map[string][]string{"back": {}, "front": {}}
	pendingLayer := ""
	for _, token := range tokens {
		if token.Kind == dnfpvf.TokenInt {
			switch token.Int {
			case 0:
				pendingLayer = "back"
			case 1:
				pendingLayer = "front"
			default:
				pendingLayer = ""
			}
			continue
		}
		if token.Kind != dnfpvf.TokenString && token.Kind != dnfpvf.TokenIdent {
			continue
		}
		aniPath := strings.TrimSpace(strings.ReplaceAll(token.Value, `\`, "/"))
		lowerPath := strings.ToLower(aniPath)
		if !strings.HasSuffix(lowerPath, ".ani") || strings.Contains(lowerPath, "..") || strings.Contains(lowerPath, ":") {
			continue
		}
		layer := pendingLayer
		pendingLayer = ""
		if layer == "" {
			switch {
			case strings.Contains(lowerPath, "front") || strings.Contains(lowerPath, "/up_"):
				layer = "front"
			case strings.Contains(lowerPath, "back") || strings.Contains(lowerPath, "/down_") || strings.Contains(lowerPath, "/donw_"):
				layer = "back"
			}
		}
		if layer != "" {
			candidates[layer] = append(candidates[layer], aniPath)
		}
	}
	paths := []struct {
		layer string
		path  string
	}{
		{layer: "back", path: gmPreferredAuraAnimation(candidates["back"])},
		{layer: "front", path: gmPreferredAuraAnimation(candidates["front"])},
	}
	result := make([]GMAvatarAuraAnimation, 0, 2)
	for _, candidate := range paths {
		if candidate.path == "" {
			continue
		}
		resolvedPath, text, err := gmReadAuraAnimation(source, candidate.path)
		if err != nil {
			continue
		}
		animationDocument, err := dnfpvf.Parse(resolvedPath, text)
		if err != nil {
			continue
		}
		frames := gmAvatarAuraFrames(animationDocument)
		if len(frames) == 0 {
			continue
		}
		result = append(result, GMAvatarAuraAnimation{Layer: candidate.layer, ANIPath: resolvedPath, Frames: frames})
	}
	return result
}

func gmPreferredAuraAnimation(paths []string) string {
	for _, aniPath := range paths {
		if strings.Contains(strings.ToLower(aniPath), "normal") {
			return aniPath
		}
	}
	if len(paths) > 0 {
		return paths[0]
	}
	return ""
}

func gmReadAuraAnimation(source dnfpvf.Source, aniPath string) (string, string, error) {
	reported := strings.TrimLeft(strings.ReplaceAll(strings.TrimSpace(aniPath), `\`, "/"), "/")
	normalized := strings.ToLower(reported)
	type pathCandidate struct {
		readPath   string
		reportPath string
	}
	paths := []pathCandidate{{readPath: normalized, reportPath: reported}}
	if strings.Contains(normalized, "/donw_") {
		paths = append(paths, pathCandidate{
			readPath:   strings.Replace(normalized, "/donw_", "/down_", 1),
			reportPath: strings.Replace(normalized, "/donw_", "/down_", 1),
		})
	}
	var lastErr error
	for _, candidate := range paths {
		text, err := source.ReadText(candidate.readPath)
		if err == nil {
			return candidate.reportPath, text, nil
		}
		lastErr = err
	}
	return "", "", lastErr
}

func gmAvatarAuraFrames(document *dnfpvf.Document) []GMAvatarAuraFrame {
	if document == nil {
		return nil
	}
	frames := make([]GMAvatarAuraFrame, 0, 24)
	var current *GMAvatarAuraFrame
	appendCurrent := func() {
		if current == nil || current.ImagePath == "" || len(frames) >= 120 {
			return
		}
		if current.Delay < 16 || current.Delay > 5000 {
			current.Delay = 100
		}
		frames = append(frames, *current)
	}
	for _, section := range document.Sections {
		if section.Start < 0 || section.End > len(document.Tokens) || section.Start > section.End {
			continue
		}
		name := strings.ToLower(strings.Join(strings.Fields(strings.TrimSpace(section.Name)), " "))
		tokens := document.Tokens[section.Start:section.End]
		if strings.HasPrefix(name, "frame") && name != "frame max" {
			appendCurrent()
			current = &GMAvatarAuraFrame{Delay: 100}
			continue
		}
		if current == nil {
			continue
		}
		switch name {
		case "image":
			for _, token := range tokens {
				if current.ImagePath == "" && (token.Kind == dnfpvf.TokenString || token.Kind == dnfpvf.TokenIdent) {
					imagePath := strings.TrimSpace(strings.ReplaceAll(token.Value, `\`, "/"))
					lowerPath := strings.ToLower(imagePath)
					if strings.HasSuffix(lowerPath, ".img") && !strings.Contains(lowerPath, "..") && !strings.Contains(lowerPath, ":") {
						current.ImagePath = imagePath
					}
					continue
				}
				if token.Kind == dnfpvf.TokenInt {
					current.ImageIndex = token.Int
					break
				}
			}
		case "image pos":
			values := gmAvatarSectionInts(tokens)
			if len(values) >= 2 && values[0] >= -2048 && values[0] <= 2048 && values[1] >= -2048 && values[1] <= 2048 {
				current.X, current.Y = values[0], values[1]
			}
		case "delay":
			values := gmAvatarSectionInts(tokens)
			if len(values) > 0 {
				current.Delay = values[0]
			}
		case "graphic effect":
			for _, token := range tokens {
				if token.Kind != dnfpvf.TokenString && token.Kind != dnfpvf.TokenIdent {
					continue
				}
				effect := strings.ToLower(strings.Join(strings.Fields(strings.TrimSpace(token.Value)), ""))
				if len(effect) <= 32 {
					current.GraphicEffect = effect
				}
				break
			}
		}
	}
	appendCurrent()
	return frames
}

func gmAvatarWeaponGroup(document *dnfpvf.Document) string {
	if document == nil {
		return ""
	}
	raw, ok := document.Text("expand path")
	if !ok {
		return ""
	}
	normalized := strings.TrimSuffix(strings.ReplaceAll(strings.TrimSpace(raw), `\`, "/"), "/")
	group := strings.ToLower(path.Base(normalized))
	if group == "." || group == "" || len(group) > 24 {
		return ""
	}
	for _, character := range group {
		if (character < 'a' || character > 'z') && (character < '0' || character > '9') && character != '_' {
			return ""
		}
	}
	return group
}

func gmWeaponAvatarVariations(document *dnfpvf.Document, usableJobs []string) []GMAvatarVariation {
	if document == nil {
		return nil
	}
	jobs := gmSpecificJobTags(usableJobs)
	if len(jobs) == 0 {
		return nil
	}
	insideExpandAnimation := false
	variation := int64(0)
	palette := int64(0)
	hasVariation := false
	layers := make([]GMAvatarLayer, 0, 3)
	for _, section := range document.Sections {
		if section.Start < 0 || section.End > len(document.Tokens) || section.Start > section.End {
			continue
		}
		name := strings.ToLower(strings.Join(strings.Fields(strings.TrimSpace(section.Name)), " "))
		tokens := document.Tokens[section.Start:section.End]
		switch name {
		case "expand ani":
			insideExpandAnimation = true
			variation, palette, hasVariation = 0, 0, false
			layers = layers[:0]
		case "/expand ani":
			insideExpandAnimation = false
		case "variation":
			if !insideExpandAnimation {
				continue
			}
			values := gmAvatarSectionInts(tokens)
			if len(values) > 0 {
				variation = values[0]
				hasVariation = true
			}
			if len(values) > 1 {
				palette = values[1]
			}
		case "layer variation":
			if !insideExpandAnimation {
				continue
			}
			for index := 0; index < len(tokens); index++ {
				if tokens[index].Kind != dnfpvf.TokenInt {
					continue
				}
				nameIndex := index + 1
				for nameIndex < len(tokens) && tokens[nameIndex].Kind != dnfpvf.TokenString && tokens[nameIndex].Kind != dnfpvf.TokenIdent {
					nameIndex++
				}
				if nameIndex >= len(tokens) {
					break
				}
				layerName := strings.TrimSpace(tokens[nameIndex].Value)
				if layerName != "" {
					layers = append(layers, GMAvatarLayer{Order: tokens[index].Int, Name: layerName})
				}
				index = nameIndex
			}
		}
	}
	if !hasVariation || len(layers) == 0 {
		return nil
	}
	result := make([]GMAvatarVariation, 0, len(jobs))
	for _, job := range jobs {
		result = append(result, GMAvatarVariation{
			AnimationJob: "[" + job + "]",
			Variation:    variation,
			Palette:      palette,
			Layers:       append([]GMAvatarLayer(nil), layers...),
		})
	}
	return result
}

func gmAvatarVariations(document *dnfpvf.Document) []GMAvatarVariation {
	if document == nil {
		return nil
	}
	result := make([]GMAvatarVariation, 0, 4)
	var current *GMAvatarVariation
	flush := func() {
		if current != nil && strings.TrimSpace(current.AnimationJob) != "" {
			result = append(result, *current)
		}
		current = nil
	}
	for _, section := range document.Sections {
		if section.Start < 0 || section.End > len(document.Tokens) || section.Start > section.End {
			continue
		}
		name := strings.ToLower(strings.Join(strings.Fields(strings.TrimSpace(section.Name)), " "))
		tokens := document.Tokens[section.Start:section.End]
		switch name {
		case "animation job":
			flush()
			if job, ok := gmAvatarSectionText(tokens); ok {
				current = &GMAvatarVariation{AnimationJob: job}
			}
		case "variation":
			if current == nil {
				continue
			}
			values := gmAvatarSectionInts(tokens)
			if len(values) > 0 {
				current.Variation = values[0]
			}
			if len(values) > 1 {
				current.Palette = values[1]
			}
		case "layer variation":
			if current == nil {
				continue
			}
			for index := 0; index < len(tokens); index++ {
				if tokens[index].Kind != dnfpvf.TokenInt {
					continue
				}
				nameIndex := index + 1
				for nameIndex < len(tokens) && tokens[nameIndex].Kind != dnfpvf.TokenString && tokens[nameIndex].Kind != dnfpvf.TokenIdent {
					nameIndex++
				}
				if nameIndex >= len(tokens) {
					break
				}
				layerName := strings.TrimSpace(tokens[nameIndex].Value)
				if layerName != "" {
					current.Layers = append(current.Layers, GMAvatarLayer{Order: tokens[index].Int, Name: layerName})
				}
				index = nameIndex
			}
		}
	}
	flush()
	return result
}

func gmAvatarSectionText(tokens []dnfpvf.Token) (string, bool) {
	for _, token := range tokens {
		if token.Kind != dnfpvf.TokenString && token.Kind != dnfpvf.TokenIdent {
			continue
		}
		value := strings.TrimSpace(token.Value)
		if value != "" {
			return value, true
		}
	}
	return "", false
}

func gmAvatarSectionInts(tokens []dnfpvf.Token) []int64 {
	result := make([]int64, 0, 2)
	for _, token := range tokens {
		if token.Kind == dnfpvf.TokenInt {
			result = append(result, token.Int)
		}
	}
	return result
}

func gmItemRequiredSkills(document *dnfpvf.Document) []GMItemRequiredSkill {
	if document == nil {
		return nil
	}
	tokens, ok := document.Section("required job skill")
	if !ok {
		return nil
	}
	result := make([]GMItemRequiredSkill, 0, 2)
	for index := 0; index < len(tokens); {
		token := tokens[index]
		if token.Kind != dnfpvf.TokenString && token.Kind != dnfpvf.TokenIdent {
			index++
			continue
		}
		job := strings.TrimSpace(token.Value)
		index++
		if index >= len(tokens) || tokens[index].Kind != dnfpvf.TokenInt {
			continue
		}
		result = append(result, GMItemRequiredSkill{Job: job, SkillID: tokens[index].Int})
		index++
	}
	return result
}

func gmResolveItemSkillNames(catalog *dnfskill.Table, required []GMItemRequiredSkill, levelUps []GMItemSkillLevelUp) {
	if catalog == nil {
		return
	}
	for index := range required {
		required[index].SkillName = gmItemSkillName(catalog, required[index].Job, required[index].SkillID)
	}
	for index := range levelUps {
		levelUps[index].SkillName = gmItemSkillName(catalog, levelUps[index].Job, levelUps[index].SkillID)
	}
}

func gmItemSkillName(catalog *dnfskill.Table, jobTag string, skillID int64) string {
	if catalog == nil || skillID < 0 || skillID > math.MaxUint16 {
		return ""
	}
	for job := 0; job <= dnfjobmap.MaxID; job++ {
		if !dnfjobmap.MatchesQuestTag(jobTag, job) {
			continue
		}
		if skill, ok := catalog.Find(byte(job), uint16(skillID)); ok {
			return strings.TrimSpace(skill.Name)
		}
		return ""
	}
	return ""
}

// Weapon skill cost/cooldown lines are fixed client presentation rules keyed
// by item group; they are not repeated in every individual equipment script.
func gmWeaponSkillModifiers(itemGroup string) []GMItemSkillModifier {
	switch normalizeEquipmentPlacementItemGroup(itemGroup) {
	case "beamswd":
		return []GMItemSkillModifier{{SkillType: "physical", MPCostPercent: -10, CooldownPercent: -10}}
	default:
		return nil
	}
}

func gmItemDetailText(document *dnfpvf.Document, section string) string {
	if document == nil {
		return ""
	}
	parts := make([]string, 0, 1)
	for _, value := range document.Texts(section) {
		if value = strings.TrimSpace(value); value != "" {
			parts = append(parts, strings.ReplaceAll(value, "%%", "%"))
		}
	}
	return strings.Join(parts, "\n")
}

func gmEquipmentDetailAttributes(document *dnfpvf.Document) []GMItemAttribute {
	candidates := []GMItemAttribute{
		{Name: "physical_attack", Value: gmEquipmentTopQualityStat(document, "equipment physical attack", "add equipment physical attack")},
		{Name: "magical_attack", Value: gmEquipmentTopQualityStat(document, "equipment magical attack", "add equipment magical attack")},
		{Name: "independent_attack", Value: gmEquipmentTopQualityStat(document, "separate attack", "add separate attack")},
		{Name: "physical_defense", Value: gmEquipmentTopQualityStat(document, "equipment physical defense", "add equipment physical defense")},
		{Name: "magical_defense", Value: gmEquipmentTopQualityStat(document, "equipment magical defense", "add equipment magical defense")},
		{Name: "strength", Value: gmEquipmentTopQualityStat(document, "physical attack", "add physical attack") + gmEquipmentDetailSum(document, "strength", "add strength")},
		{Name: "intelligence", Value: gmEquipmentTopQualityStat(document, "magical attack", "add magical attack") + gmEquipmentDetailSum(document, "intelligence", "add intelligence")},
		{Name: "vitality", Value: gmEquipmentTopQualityStat(document, "physical defense", "add physical defense") + gmEquipmentDetailSum(document, "vitality", "add vitality")},
		{Name: "spirit", Value: gmEquipmentTopQualityStat(document, "magical defense", "add magical defense") + gmEquipmentDetailSum(document, "spirit", "add spirit")},
		{Name: "hp_max", Value: gmEquipmentDetailSum(document, "HP MAX", "hp max", "add HP MAX", "add hp max")},
		{Name: "mp_max", Value: gmEquipmentDetailSum(document, "MP MAX", "mp max", "add MP MAX", "add mp max")},
		{Name: "attack_speed", Value: gmEquipmentDetailSum(document, "add attack speed")},
		{Name: "move_speed", Value: gmEquipmentDetailSum(document, "move speed", "add move speed")},
		{Name: "cast_speed", Value: gmEquipmentDetailSum(document, "cast speed", "add cast speed")},
		{Name: "hit_recovery", Value: gmEquipmentDetailSum(document, "hit recovery", "add hit recovery")},
		{Name: "jump_power", Value: gmEquipmentDetailSum(document, "jump power", "add jump power")},
		{Name: "hit_rate", Value: -gmEquipmentDetailSum(document, "stuck", "add stuck")},
		{Name: "physical_critical", Value: gmEquipmentDetailSum(document, "physical critical hit", "add physical critical hit")},
		{Name: "magical_critical", Value: gmEquipmentDetailSum(document, "magical critical hit", "add magical critical hit")},
		{Name: "all_element_enhancement", Value: gmEquipmentDetailSum(document, "all elemental attack", "add all elemental attack")},
		{Name: "fire_element_enhancement", Value: gmEquipmentDetailSum(document, "fire attack", "fire elemental attack", "add fire attack", "add fire elemental attack")},
		{Name: "water_element_enhancement", Value: gmEquipmentDetailSum(document, "water attack", "water elemental attack", "add water attack", "add water elemental attack")},
		{Name: "light_element_enhancement", Value: gmEquipmentDetailSum(document, "light attack", "light elemental attack", "add light attack", "add light elemental attack")},
		{Name: "dark_element_enhancement", Value: gmEquipmentDetailSum(document, "dark attack", "dark elemental attack", "add dark attack", "add dark elemental attack")},
		{Name: "fire_resistance", Value: gmEquipmentDetailSum(document, "fire resistance", "add fire resistance")},
		{Name: "water_resistance", Value: gmEquipmentDetailSum(document, "water resistance", "add water resistance")},
		{Name: "light_resistance", Value: gmEquipmentDetailSum(document, "light resistance", "add light resistance")},
		{Name: "dark_resistance", Value: gmEquipmentDetailSum(document, "dark resistance", "add dark resistance")},
		{Name: "anti_magic", Value: gmEquipmentDetailSum(document, "anti evil")},
	}
	attributes := make([]GMItemAttribute, 0, len(candidates))
	for _, attribute := range candidates {
		if attribute.Value != 0 {
			attributes = append(attributes, attribute)
		}
	}
	return attributes
}

const gmEquipmentTopQualityMultiplier = 1.1

func gmEquipmentTopQualityStat(document *dnfpvf.Document, baseSection, addSection string) int64 {
	base := equipmentStatNumber(document, baseSection)
	add := equipmentStatNumber(document, addSection)
	if base == 0 {
		return add
	}
	return int64(math.Round(float64(base)*gmEquipmentTopQualityMultiplier)) + add
}

func gmEquipmentDetailSum(document *dnfpvf.Document, sections ...string) int64 {
	var total int64
	seen := make(map[string]struct{}, len(sections))
	for _, section := range sections {
		key := strings.ToLower(strings.Join(strings.Fields(strings.TrimSpace(section)), " "))
		if _, duplicate := seen[key]; duplicate {
			continue
		}
		seen[key] = struct{}{}
		total += equipmentStatNumber(document, section)
	}
	return total
}

func gmEquipmentWeaponAttackSpeed(document *dnfpvf.Document, equipmentType string) string {
	if document == nil || !strings.EqualFold(normalizeMagicBoxPVFType(equipmentType), "weapon") {
		return ""
	}
	values := document.Numbers("attack speed")
	if len(values) == 0 {
		return ""
	}
	switch speed := values[0]; {
	case speed >= 100:
		return "very_fast"
	case speed >= 50:
		return "fast"
	case speed > -50:
		return "normal"
	case speed > -100:
		return "slow"
	default:
		return "very_slow"
	}
}

func gmEquipmentElementalProperties(document *dnfpvf.Document) []string {
	if document == nil {
		return nil
	}
	values := document.Texts("elemental property")
	out := make([]string, 0, len(values))
	seen := make(map[string]struct{}, len(values))
	for _, value := range values {
		value = normalizeMagicBoxPVFType(value)
		if value == "" {
			continue
		}
		if _, duplicate := seen[value]; duplicate {
			continue
		}
		seen[value] = struct{}{}
		out = append(out, value)
	}
	return out
}

const gmEquipmentPartSetListPath = "etc/equipmentpartset.etc"

func gmEquipmentSetDetail(source dnfpvf.Source, setID int64) (*GMEquipmentSetDetail, error) {
	if source == nil || setID <= 0 {
		return nil, nil
	}
	listText, err := source.ReadText(gmEquipmentPartSetListPath)
	if err != nil {
		return nil, fmt.Errorf("read %s: %w", gmEquipmentPartSetListPath, err)
	}
	listDocument, err := dnfpvf.Parse(gmEquipmentPartSetListPath, listText)
	if err != nil {
		return nil, fmt.Errorf("parse %s: %w", gmEquipmentPartSetListPath, err)
	}
	var reference, name string
	for index := 0; index+1 < len(listDocument.Tokens); index++ {
		idToken := listDocument.Tokens[index]
		pathToken := listDocument.Tokens[index+1]
		if idToken.Kind != dnfpvf.TokenInt || idToken.Int != setID ||
			(pathToken.Kind != dnfpvf.TokenString && pathToken.Kind != dnfpvf.TokenIdent) {
			continue
		}
		reference = strings.TrimSpace(strings.Trim(pathToken.Value, "`\"'"))
		if index+2 < len(listDocument.Tokens) {
			nameToken := listDocument.Tokens[index+2]
			if nameToken.Kind == dnfpvf.TokenString || nameToken.Kind == dnfpvf.TokenIdent {
				name = strings.Join(strings.Fields(nameToken.Value), " ")
			}
		}
		break
	}
	if reference == "" {
		return nil, nil
	}
	reference = strings.ReplaceAll(reference, "\\", "/")
	setPath := path.Clean(reference)
	if !strings.HasPrefix(strings.ToLower(setPath), "equipment/") {
		setPath = path.Clean(path.Join("equipment", setPath))
	}
	setText, err := source.ReadText(setPath)
	if err != nil {
		return nil, fmt.Errorf("read %s: %w", setPath, err)
	}
	setDocument, err := dnfpvf.Parse(setPath, setText)
	if err != nil {
		return nil, fmt.Errorf("parse %s: %w", setPath, err)
	}
	return &GMEquipmentSetDetail{
		SetID: setID, Name: name, PVFPath: setPath, Bonuses: gmEquipmentSetBonuses(setDocument),
	}, nil
}

func gmEquipmentSetBonuses(document *dnfpvf.Document) []GMEquipmentSetBonus {
	if document == nil {
		return nil
	}
	bonuses := make([]GMEquipmentSetBonus, 0, 3)
	var current *dnfpvf.Document
	var requiredPieces int
	finish := func() {
		if current == nil || requiredPieces <= 0 {
			return
		}
		bonuses = append(bonuses, GMEquipmentSetBonus{
			RequiredPieces: requiredPieces,
			BasicExplain:   gmItemDetailText(current, "parameter basic explain"),
			DetailExplain:  gmItemDetailText(current, "parameter detail explain"),
			Attributes:     gmEquipmentSetAttributes(current),
		})
	}
	for _, section := range document.Sections {
		name := strings.ToLower(strings.Join(strings.Fields(strings.TrimSpace(section.Name)), " "))
		if name == "piece set ability" {
			finish()
			current = &dnfpvf.Document{Path: document.Path}
			requiredPieces = 0
			for _, token := range gmDocumentSectionTokens(document, section) {
				if token.Kind == dnfpvf.TokenInt && token.Int > 0 && token.Int <= math.MaxInt32 {
					requiredPieces = int(token.Int)
					break
				}
			}
			continue
		}
		if name == "/piece set ability" {
			finish()
			current = nil
			requiredPieces = 0
			continue
		}
		if current == nil {
			continue
		}
		tokens := gmDocumentSectionTokens(document, section)
		start := len(current.Tokens)
		current.Tokens = append(current.Tokens, tokens...)
		current.Sections = append(current.Sections, dnfpvf.Section{Name: section.Name, Start: start, End: len(current.Tokens)})
	}
	finish()
	return bonuses
}

func gmDocumentSectionTokens(document *dnfpvf.Document, section dnfpvf.Section) []dnfpvf.Token {
	if document == nil || section.Start < 0 || section.End < section.Start || section.End > len(document.Tokens) {
		return nil
	}
	return document.Tokens[section.Start:section.End]
}

func gmEquipmentSetAttributes(document *dnfpvf.Document) []GMItemAttribute {
	candidates := []GMItemAttribute{
		{Name: "physical_attack", Value: gmEquipmentDetailSum(document, "equipment physical attack", "add equipment physical attack")},
		{Name: "magical_attack", Value: gmEquipmentDetailSum(document, "equipment magical attack", "add equipment magical attack")},
		{Name: "independent_attack", Value: gmEquipmentDetailSum(document, "separate attack", "add separate attack")},
		{Name: "physical_defense", Value: gmEquipmentDetailSum(document, "equipment physical defense", "add equipment physical defense")},
		{Name: "magical_defense", Value: gmEquipmentDetailSum(document, "equipment magical defense", "add equipment magical defense")},
		{Name: "strength", Value: gmEquipmentDetailSum(document, "physical attack", "add physical attack", "strength", "add strength")},
		{Name: "intelligence", Value: gmEquipmentDetailSum(document, "magical attack", "add magical attack", "intelligence", "add intelligence")},
		{Name: "vitality", Value: gmEquipmentDetailSum(document, "physical defense", "add physical defense", "vitality", "add vitality")},
		{Name: "spirit", Value: gmEquipmentDetailSum(document, "magical defense", "add magical defense", "spirit", "add spirit")},
		{Name: "hp_max", Value: gmEquipmentDetailSum(document, "HP MAX", "hp max", "add HP MAX", "add hp max")},
		{Name: "mp_max", Value: gmEquipmentDetailSum(document, "MP MAX", "mp max", "add MP MAX", "add mp max")},
		{Name: "attack_speed", Value: gmEquipmentDetailSum(document, "attack speed", "add attack speed")},
		{Name: "move_speed", Value: gmEquipmentDetailSum(document, "move speed", "add move speed")},
		{Name: "cast_speed", Value: gmEquipmentDetailSum(document, "cast speed", "add cast speed")},
		{Name: "hit_recovery", Value: gmEquipmentDetailSum(document, "hit recovery", "add hit recovery")},
		{Name: "jump_power", Value: gmEquipmentDetailSum(document, "jump power", "add jump power")},
		{Name: "hit_rate", Value: -gmEquipmentDetailSum(document, "stuck", "add stuck")},
		{Name: "physical_critical", Value: gmEquipmentDetailSum(document, "physical critical hit", "add physical critical hit")},
		{Name: "magical_critical", Value: gmEquipmentDetailSum(document, "magical critical hit", "add magical critical hit")},
		{Name: "all_element_enhancement", Value: gmEquipmentDetailSum(document, "all elemental attack", "add all elemental attack")},
		{Name: "fire_element_enhancement", Value: gmEquipmentDetailSum(document, "fire attack", "fire elemental attack", "add fire attack", "add fire elemental attack")},
		{Name: "water_element_enhancement", Value: gmEquipmentDetailSum(document, "water attack", "water elemental attack", "add water attack", "add water elemental attack")},
		{Name: "light_element_enhancement", Value: gmEquipmentDetailSum(document, "light attack", "light elemental attack", "add light attack", "add light elemental attack")},
		{Name: "dark_element_enhancement", Value: gmEquipmentDetailSum(document, "dark attack", "dark elemental attack", "add dark attack", "add dark elemental attack")},
		{Name: "fire_resistance", Value: gmEquipmentDetailSum(document, "fire resistance", "add fire resistance")},
		{Name: "water_resistance", Value: gmEquipmentDetailSum(document, "water resistance", "add water resistance")},
		{Name: "light_resistance", Value: gmEquipmentDetailSum(document, "light resistance", "add light resistance")},
		{Name: "dark_resistance", Value: gmEquipmentDetailSum(document, "dark resistance", "add dark resistance")},
		{Name: "anti_magic", Value: gmEquipmentDetailSum(document, "anti evil")},
	}
	attributes := make([]GMItemAttribute, 0, len(candidates))
	for _, attribute := range candidates {
		if attribute.Value != 0 {
			attributes = append(attributes, attribute)
		}
	}
	return attributes
}

func (s *Service) GMSendItemMail(
	ctx context.Context,
	characterID string,
	itemID uint32,
	count int64,
	title string,
	body string,
) (GMMailDeliveryResult, error) {
	return s.GMSendItemMailWithOptions(ctx, characterID, itemID, count, title, body, GMMailOptions{})
}

func (s *Service) GMSendItemMailWithOptions(
	ctx context.Context,
	characterID string,
	itemID uint32,
	count int64,
	title string,
	body string,
	options GMMailOptions,
) (GMMailDeliveryResult, error) {
	characterID, numericID, err := gmCharacterID(characterID)
	if err != nil || itemID == 0 || count <= 0 || uint64(count) > math.MaxUint32 {
		return GMMailDeliveryResult{}, fmt.Errorf("%w: character, item and positive count are required", ErrGMInvalidRequest)
	}
	repositories, ok := s.repositoryGroup()
	if !ok || repositories.MailboxAssets == nil {
		return GMMailDeliveryResult{}, ErrGMUnavailable
	}
	catalog, err := s.currentPVFItemCatalog()
	if err != nil {
		return GMMailDeliveryResult{}, err
	}
	definition, err := catalog.ResolveItem(itemID)
	if err != nil {
		return GMMailDeliveryResult{}, err
	}
	if definition.Kind == dungeonDropItemEquipment && count != 1 {
		return GMMailDeliveryResult{}, fmt.Errorf("%w: equipment mail count must be 1", ErrGMInvalidRequest)
	}
	quality := strings.ToLower(strings.TrimSpace(options.EquipmentQuality))
	if options.ExpireDays < 0 || options.ExpireDays > 3650 || options.Reinforce < 0 || options.Reinforce > 31 || options.AmplifyType < 0 || options.AmplifyType > 4 || options.Forging < 0 || options.Forging > 8 {
		return GMMailDeliveryResult{}, fmt.Errorf("%w: expire_days, reinforce, amplify_type or forging is out of range", ErrGMInvalidRequest)
	}
	if quality != "" && quality != "default" && quality != "random" && quality != "top" {
		return GMMailDeliveryResult{}, fmt.Errorf("%w: equipment_quality must be default, random or top", ErrGMInvalidRequest)
	}
	if definition.Kind != dungeonDropItemEquipment && (options.Reinforce != 0 || options.AmplifyType != 0 || options.Forging != 0 || len(options.EmblemItemIDs) != 0 || options.EnchantBeadItemID != 0 || (quality != "" && quality != "default")) {
		return GMMailDeliveryResult{}, fmt.Errorf("%w: equipment attributes require an equipment item", ErrGMInvalidRequest)
	}
	if definition.Kind == dungeonDropItemEquipment && !gmItemSupportsEquipmentAttributes(definition.EquipmentType) &&
		(options.Reinforce != 0 || options.AmplifyType != 0 || (quality != "" && quality != "default")) {
		return GMMailDeliveryResult{}, fmt.Errorf("%w: equipment attributes are not supported by this equipment type", ErrGMInvalidRequest)
	}
	title = strings.TrimSpace(title)
	if title == "" {
		title = "GM 发放"
	}
	body = strings.TrimSpace(body)
	if body == "" {
		body = "请在邮箱中领取物品。"
	}
	if len([]rune(title)) > 40 || len([]rune(body)) > 200 {
		return GMMailDeliveryResult{}, fmt.Errorf("%w: mail title or body is too long", ErrGMInvalidRequest)
	}
	now := time.Now().UTC()
	stack, _ := applyCurrentPVFItemExpirationAt(dnfrepo.ItemStack{
		ItemID: int64(itemID),
		Count:  count,
		Extra: map[string]string{
			"item_kind":      string(definition.Kind),
			"pvf_path":       definition.PVFPath,
			"stackable_type": definition.StackableType,
		},
	}, definition, now)
	if options.ExpireDays > 0 {
		stack, _ = applyCurrentGMExpirationOverrideAt(
			stack,
			now.Add(time.Duration(options.ExpireDays)*24*time.Hour),
			options.ExpireDays,
			now,
		)
	}
	if definition.Kind == dungeonDropItemEquipment {
		itemListType := dnfrepo.MainInventoryListType
		if definition.EquipmentType != "" {
			stack.Extra["equipment_type"] = definition.EquipmentType
		}
		gmApplyMailEquipmentSeal(&stack, definition)
		if rule, supported := currentEquipmentPlacementRuleForPVFType(definition.EquipmentType); supported && rule.class == currentEquipmentPlacementClassAvatar {
			itemListType = currentAvatarInventoryListType
			stack.Extra["item_kind"] = "avatar"
			stack.Extra["amount_or_count"] = "0"
		}
		if definition.Durability > 0 {
			stack.Extra["durability"] = strconv.FormatUint(uint64(definition.Durability), 10)
			stack.Extra["max_durability"] = strconv.FormatUint(uint64(definition.Durability), 10)
		}
		if options.Reinforce > 0 {
			value := strconv.Itoa(options.Reinforce)
			stack.Extra["reinforce"] = value
			stack.Extra["ext_data0"] = value
		}
		if options.AmplifyType > 0 {
			// An amplification option is only real to the game once the row
			// carries both raw+0x13 (option) and raw+0x14..0x15 (its PVF base
			// value). Writing the option alone produced a state the client can
			// never create: op80 amplification then failed the
			// "option present but no value" gate and answered error 174.
			// GMSystemMail already resolves the same value, so mirror it here.
			amplifyConfig, configErr := currentAmplifyItemConfigFromSource(catalog.source)
			if configErr != nil {
				return GMMailDeliveryResult{}, fmt.Errorf("resolve item=%d amplification config: %w", itemID, configErr)
			}
			initialValues, initialErr := currentAmplifyInitialValues(amplifyConfig, definition.Rarity)
			if initialErr != nil {
				return GMMailDeliveryResult{}, fmt.Errorf("resolve item=%d amplification value: %w", itemID, initialErr)
			}
			amplifyValue := initialValues[byte(options.AmplifyType)]
			if amplifyValue == 0 {
				return GMMailDeliveryResult{}, fmt.Errorf("%w: item=%d amplification type=%d has no PVF value", ErrGMInvalidRequest, itemID, options.AmplifyType)
			}
			value := strconv.Itoa(options.AmplifyType)
			amplifyValueText := strconv.FormatUint(uint64(amplifyValue), 10)
			for _, key := range []string{"amplify_type", "amplification_type", "byte_13", "value_13", "value_c"} {
				stack.Extra[key] = value
			}
			for _, key := range []string{"amplify_value", "amplification_value", "marker_16", "marker16", "value_d"} {
				stack.Extra[key] = amplifyValueText
			}
		}
		if err := gmApplyMailEquipmentCustomization(&stack, catalog, definition, options); err != nil {
			return GMMailDeliveryResult{}, err
		}
		var qualitySeed uint32
		switch quality {
		case "random":
			qualitySeed, err = newCurrentEquipmentQualitySeed()
		case "top":
			qualitySeed = currentEquipmentTopQualitySeed
		}
		if err != nil {
			return GMMailDeliveryResult{}, err
		}
		if qualitySeed != 0 {
			if err := applyCurrentEquipmentQualitySeed(&stack, qualitySeed); err != nil {
				return GMMailDeliveryResult{}, err
			}
		}
		entry := currentItemListEntryFromStack(itemListType, 0, stack)
		stack.RawEntry = append([]byte(nil), entry.data[:]...)
	}
	attachment := dnfrepo.MailAttachment{
		ItemID:   int64(itemID),
		Count:    count,
		ExpireAt: stack.ExpireAt,
		RawEntry: stack.RawEntry,
		Extra:    stack.Extra,
	}
	if attachment.Extra == nil {
		attachment.Extra = make(map[string]string)
	}
	attachment.Extra["mailbox_pvf_kind"] = string(definition.Kind)
	attachment.Extra["mailbox_equipment_type"] = definition.EquipmentType
	attachment.Extra["mailbox_pvf_path"] = definition.PVFPath
	if definition.StackLimit > 0 {
		attachment.Extra["stack_limit"] = strconv.FormatInt(definition.StackLimit, 10)
	}
	var mailID string
	var character dnfrepo.CharacterRecord
	err = repositories.MailboxAssets.WithinMailboxAssets(ctx, characterID, characterID, func(
		_ dnfrepo.AccountRepository,
		_ dnfrepo.AccountInventoryRepository,
		characters dnfrepo.CharacterRepository,
		_ dnfrepo.InventoryRepository,
		mailboxes dnfrepo.MailboxRepository,
	) error {
		record, found, loadErr := characters.Load(ctx, characterID)
		if loadErr != nil {
			return loadErr
		}
		if !found {
			return ErrGMCharacterNotFound
		}
		character = record
		mailID, loadErr = dnfrepo.AppendSystemMail(ctx, mailboxes, dnfrepo.SystemMailDelivery{
			RecipientCharacterID: characterID,
			Title:                title,
			Body:                 body,
			Source:               "dnf90-gm",
			Attachments:          []dnfrepo.MailAttachment{attachment},
			CreatedAt:            now,
		})
		return loadErr
	})
	if err != nil {
		return GMMailDeliveryResult{}, err
	}
	if err := s.sendMailboxAlarmToOnlineRecipient(numericID); err != nil {
		s.logWarn("dnf90 GM mailbox alarm failed", "character_id", characterID, "mail_id", mailID, "error", err)
	}
	return GMMailDeliveryResult{
		MailID:    mailID,
		Character: s.gmCharacterSummary(character),
		Item:      gmItemSummary(definition),
		Count:     count,
	}, nil
}

func gmItemSupportsEquipmentAttributes(equipmentType string) bool {
	switch gmItemTypeTag(equipmentType) {
	case "weapon", "coat", "shoulder", "pants", "shoes", "waist", "amulet", "wrist", "ring", "support", "magic stone", "earring", "support weapon":
		return true
	default:
		return false
	}
}

func (s *Service) gmCareerState(record dnfrepo.CharacterRecord, profiles *dnfprofession.Profiles, skillsReset bool) (GMCareerState, error) {
	job, err := strconv.ParseUint(strings.TrimSpace(record.Job), 10, 8)
	grow := numericCharacterStatValue(record, "grow_type")
	if err != nil || grow < 0 || grow > math.MaxUint8 {
		return GMCareerState{}, fmt.Errorf("%w: stored profession state is invalid", ErrGMInvalidRequest)
	}
	first, awakening := dnfprofession.Decode(byte(grow))
	name, _ := profiles.DisplayName(byte(job), byte(grow))
	if strings.TrimSpace(name) == "" {
		name = fmt.Sprintf("职业 %d / 转职 %d / 觉醒 %d", job, first, awakening)
	}
	return GMCareerState{
		Character:      s.gmCharacterSummary(record),
		Job:            byte(job),
		GrowType:       byte(grow),
		FirstGrow:      first,
		Awakening:      awakening,
		ProfessionName: name,
		Options:        profiles.Options(),
		SkillsReset:    skillsReset,
	}, nil
}

func (s *Service) gmQuestContext(ctx context.Context, characterID string) (dnfrepo.CharacterRecord, dnfrepo.QuestRecord, *dnfquest.Catalog, error) {
	characterID, _, err := gmCharacterID(characterID)
	if err != nil {
		return dnfrepo.CharacterRecord{}, dnfrepo.QuestRecord{}, nil, err
	}
	repositories, ok := s.repositoryGroup()
	if !ok || repositories.Character == nil || repositories.Quest == nil {
		return dnfrepo.CharacterRecord{}, dnfrepo.QuestRecord{}, nil, ErrGMUnavailable
	}
	character, found, err := repositories.Character.Load(ctx, characterID)
	if err != nil {
		return dnfrepo.CharacterRecord{}, dnfrepo.QuestRecord{}, nil, err
	}
	if !found {
		return dnfrepo.CharacterRecord{}, dnfrepo.QuestRecord{}, nil, ErrGMCharacterNotFound
	}
	record, found, err := repositories.Quest.Load(ctx, characterID)
	if err != nil {
		return dnfrepo.CharacterRecord{}, dnfrepo.QuestRecord{}, nil, err
	}
	if !found {
		record = dnfrepo.QuestRecord{CharacterID: characterID}
	}
	catalog, err := s.loadQuestCatalog(ctx)
	if err != nil {
		return dnfrepo.CharacterRecord{}, dnfrepo.QuestRecord{}, nil, err
	}
	return character, record, catalog, nil
}

func (s *Service) gmQuestMutationResult(ctx context.Context, characterID string, questID int64, rewardGranted bool, professionChanged bool) (GMQuestMutationResult, error) {
	character, record, catalog, err := s.gmQuestContext(ctx, characterID)
	if err != nil {
		return GMQuestMutationResult{}, err
	}
	definition, found := catalog.Find(questID)
	if !found {
		definition = dnfquest.Definition{ID: questID}
	}
	state, _ := gmQuestState(record, questID)
	return GMQuestMutationResult{
		Character:         s.gmCharacterSummary(character),
		Quest:             gmQuestSummaryWithTitle(definition, state, false, s.currentTitleBookMapping()),
		RewardGranted:     rewardGranted,
		ProfessionChanged: professionChanged,
	}, nil
}

func gmQuestMutationIDs(characterID string, questID int64) (string, uint16, error) {
	characterID, numericID, err := gmCharacterID(characterID)
	if err != nil {
		return "", 0, err
	}
	if questID <= 0 || questID > math.MaxUint16 {
		return "", 0, fmt.Errorf("%w: quest id must be between 1 and %d", ErrGMInvalidRequest, uint64(math.MaxUint16))
	}
	return characterID, numericID, nil
}

func gmQuestState(record dnfrepo.QuestRecord, questID int64) (dnfrepo.QuestState, bool) {
	if state, ok := record.States[questID]; ok {
		return state, true
	}
	if state, ok := record.Progress[questID]; ok {
		return state, true
	}
	return dnfrepo.QuestState{}, false
}

func gmMutableQuestState(record dnfrepo.QuestRecord, questID int64) (dnfrepo.QuestState, dnfrepo.QuestField, bool) {
	if state, ok := record.States[questID]; ok {
		return state, dnfrepo.QuestFieldStates, true
	}
	if state, ok := record.Progress[questID]; ok {
		return state, dnfrepo.QuestFieldProgress, true
	}
	return dnfrepo.QuestState{}, "", false
}

func gmPutQuestState(record *dnfrepo.QuestRecord, questID int64, state dnfrepo.QuestState, field dnfrepo.QuestField) {
	if record == nil {
		return
	}
	switch field {
	case dnfrepo.QuestFieldProgress:
		if record.Progress == nil {
			record.Progress = make(map[int64]dnfrepo.QuestState)
		}
		record.Progress[questID] = state
	default:
		if record.States == nil {
			record.States = make(map[int64]dnfrepo.QuestState)
		}
		record.States[questID] = state
	}
}

func gmQuestStatusActive(status string) bool {
	switch strings.ToLower(strings.TrimSpace(status)) {
	case "active", "accepted", "inprogress", "progress":
		return true
	default:
		return false
	}
}

func gmQuestStatusCompleted(status string) bool {
	switch strings.ToLower(strings.TrimSpace(status)) {
	case "completed", "complete", "cleared", "finished", "rewarded":
		return true
	default:
		return false
	}
}

func gmProfessionRewardType(value string) bool {
	value = strings.ToLower(strings.TrimSpace(value))
	value = strings.TrimSpace(strings.TrimSuffix(strings.TrimPrefix(value, "["), "]"))
	return value == "grow type" || value == "awakening type"
}

func gmQuestSummary(definition dnfquest.Definition, state dnfrepo.QuestState, acceptable bool) GMQuestSummary {
	name := strings.TrimSpace(definition.Name)
	if name == "" {
		name = strings.TrimSpace(definition.Path)
	}
	if name == "" {
		name = fmt.Sprintf("任务 %d", definition.ID)
	}
	active := gmQuestStatusActive(state.Status)
	completed := gmQuestStatusCompleted(state.Status)
	status := strings.TrimSpace(state.Status)
	if status == "" {
		if acceptable {
			status = "available"
		} else {
			status = "not_started"
		}
	}
	return GMQuestSummary{
		QuestID:         definition.ID,
		Name:            name,
		Path:            definition.Path,
		Grade:           definition.Grade,
		Type:            definition.Type,
		LevelMin:        definition.LevelMin,
		LevelMax:        definition.LevelMax,
		Status:          status,
		ProgressValue:   state.ProgressValue,
		Active:          active,
		Completed:       completed,
		Acceptable:      acceptable && !active && !completed,
		JobChangeQuest:  definition.JobChangeQuest,
		RewardType:      definition.RewardType,
		ProfessionQuest: gmProfessionRewardType(definition.RewardType),
		MainQuestID:     definition.MainQuestID,
		Prerequisites:   definition.PreRequiredGroups,
		Collisions:      definition.CollisionQuests,
		CantGiveUp:      definition.CantGiveUp,
	}
}

func gmQuestSummaryWithTitle(definition dnfquest.Definition, state dnfrepo.QuestState, acceptable bool, mapping map[int32]titleBookMappingEntry) GMQuestSummary {
	summary := gmQuestSummary(definition, state, acceptable)
	if definition.ID <= 0 || definition.ID > math.MaxInt32 {
		return summary
	}
	if entry, ok := mapping[int32(definition.ID)]; ok {
		summary.TitleItemID = entry.titleItemID
		summary.TitleCategory = entry.category
		summary.TitleBookIndex = entry.bookIndex
	}
	return summary
}

func (s *Service) gmCharacterSummary(record dnfrepo.CharacterRecord) GMCharacterSummary {
	_, numericID, _ := gmCharacterID(record.CharacterID)
	_, online := s.onlineGameSession(numericID)
	growType := record.Stats["grow_type"]
	professionName := ""
	job, jobErr := strconv.ParseUint(strings.TrimSpace(record.Job), 10, 8)
	if jobErr == nil {
		s.professionMu.Lock()
		profiles := s.professionProfiles
		s.professionMu.Unlock()
		if profiles != nil {
			professionName, _ = profiles.DisplayName(byte(job), byte(growType))
		}
		if strings.TrimSpace(professionName) == "" {
			professionName = gmBaseProfessionName(byte(job))
		}
	}
	if strings.TrimSpace(professionName) == "" {
		professionName = "未知职业"
	}
	return GMCharacterSummary{
		CharacterID:    record.CharacterID,
		AccountID:      record.AccountID,
		Slot:           record.Slot,
		Name:           record.Name,
		Job:            record.Job,
		ProfessionName: professionName,
		GrowType:       growType,
		FirstGrow:      growType & 0x0f,
		Awakening:      (growType >> 4) & 0x0f,
		Level:          record.Level,
		Gold:           record.Stats["gold"],
		Online:         online,
	}
}

func gmBaseProfessionName(job byte) string {
	names := [...]string{
		"男鬼剑士", "女格斗家", "男神枪手", "女魔法师", "男圣职者", "女神枪手", "暗夜使者", "男格斗家",
		"男魔法师", "黑暗武士", "缔造者", "女鬼剑士", "守护者", "魔枪士", "女圣职者", "枪剑士",
	}
	if int(job) >= len(names) {
		return ""
	}
	return names[job]
}

func gmCharacterID(value string) (string, uint16, error) {
	value = strings.TrimSpace(value)
	parsed, err := strconv.ParseUint(value, 10, 16)
	if err != nil || parsed == 0 {
		return "", 0, fmt.Errorf("%w: invalid character id %q", ErrGMInvalidRequest, value)
	}
	return value, uint16(parsed), nil
}

func gmItemSummary(definition dungeonDropItemDefinition) GMItemSummary {
	name := strings.TrimSpace(definition.Name)
	if name == "" {
		name = definition.PVFPath
	}
	return GMItemSummary{
		ItemID:           definition.ItemID,
		Name:             name,
		Kind:             string(definition.Kind),
		PVFPath:          definition.PVFPath,
		IconPath:         definition.IconPath,
		IconIndex:        definition.IconIndex,
		MinimumLevel:     definition.MinimumLevel,
		Rarity:           definition.Rarity,
		StackLimit:       definition.StackLimit,
		AttachType:       definition.AttachType,
		EquipmentType:    definition.EquipmentType,
		UsableJobs:       append([]string(nil), definition.UsableJobs...),
		Grade:            definition.Grade,
		StackableType:    definition.StackableType,
		ExpirationDate:   definition.ExpirationDate,
		UsablePeriodDays: definition.UsablePeriodDays,
		Price:            definition.Price,
	}
}
