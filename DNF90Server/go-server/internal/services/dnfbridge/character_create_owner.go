package dnfbridge

import (
	"context"
	"fmt"
	"time"

	dnfcharacterdata "longheng.io/server/internal/modules/dnf/characterdata"
	darkcombo "longheng.io/server/internal/modules/dnf/darkknightcombo"
	dnfpvp "longheng.io/server/internal/modules/dnf/pvp"
	dnfrepo "longheng.io/server/internal/modules/dnf/repository"
	dnfskill "longheng.io/server/internal/modules/dnf/skill"
)

func (s *Service) saveNewCharacter(ctx context.Context, repos dnfrepo.Group, record dnfrepo.CharacterRecord) error {
	now := record.UpdatedAt
	if now.IsZero() {
		now = time.Now().UTC()
	}
	init, initErr := s.newCharacterPVFInitialization(ctx, record)
	if initErr != nil {
		s.logPacketEvent("dnf-character-pvf-initialization-blocked",
			"character_id", record.CharacterID,
			"job", record.Job,
			"level", record.Level,
			"error", initErr)
		return fmt.Errorf("initialize new character from pvf: %w", initErr)
	}
	if init.HasStats {
		applyCharacterPVFStats(&record, init.Stats)
	}
	var equipment *dnfrepo.EquipmentRecord
	if built := initialEquipmentRecord(record.CharacterID, init.Equipment, now); len(built.Entries) > 0 {
		equipment = &built
	}
	skill := initialSkillRecord(record, init, now)
	settings := s.newCharacterCSharpDefaultSettings(ctx, record, now)
	if init.Job == darkcombo.Job {
		// Dark Knight owns two independent skill/combo pages from the moment the
		// character reaches the selector. Build the alternate tree before the
		// aggregate commit so op809 never observes a half-created ordinary-only
		// character.
		primary, found := dnfrepo.SkillTree(skill, 0)
		if !found {
			return fmt.Errorf("initialize new dark knight combo: primary skill tree is unavailable")
		}
		dnfrepo.SetSkillTree(&skill, 1, primary)
		comboSettings, comboErr := s.buildNewDarkKnightComboSettings(ctx, record, skill, now)
		if comboErr != nil {
			return fmt.Errorf("initialize new dark knight combo: %w", comboErr)
		}
		settings = append(settings, comboSettings)
	}
	creation := dnfcharacterdata.Creation{
		Account: &dnfrepo.AccountRecord{
			AccountID: record.AccountID,
			State:     "active",
			Metadata:  map[string]string{"source": "dnfbridge"},
			CreatedAt: record.CreatedAt,
			UpdatedAt: now,
		},
		Character: record,
		Inventory: ptrInventoryRecord(newCharacterInventoryRecord(record.CharacterID, now)),
		Equipment: equipment,
		Skill:     &skill,
		Settings:  settings,
	}
	// 决斗场专用角色：按职业档案的 [pvp private create equipment list] 重建技能树
	// 并发一套预设装备（穿戴的进装备栏，其余进主背包），同时解锁对应的辅助装备槽。
	if dnfpvp.IsPrivateCharacter(record) {
		if err := s.initializePVPPrivateCreation(ctx, &creation); err != nil {
			return err
		}
	}
	if err := dnfcharacterdata.NewCreator(repos).Create(ctx, creation); err != nil {
		return fmt.Errorf("persist new character initialization: %w", err)
	}
	if creation.Equipment != nil && len(creation.Equipment.Entries) > 0 {
		source := "pvf_create_equipment_list"
		if creation.PVPPrivate != nil {
			source = "pvf_pvp_private_create_equipment_list"
		}
		s.logPacketEvent("dnf-character-initial-equipment-seeded",
			"character_id", record.CharacterID,
			"job", record.Job,
			"count", len(creation.Equipment.Entries),
			"source", source)
	}
	return nil
}

func (s *Service) buildNewDarkKnightComboSettings(
	ctx context.Context,
	record dnfrepo.CharacterRecord,
	skill dnfrepo.SkillRecord,
	now time.Time,
) (dnfrepo.SettingsRecord, error) {
	s.initialEquipmentMu.Lock()
	archive, err := s.initialEquipmentArchiveLocked()
	s.initialEquipmentMu.Unlock()
	if err != nil {
		return dnfrepo.SettingsRecord{}, fmt.Errorf("load active PVF: %w", err)
	}
	catalog, err := s.loadSkillCatalog(ctx, archive)
	if err != nil {
		return dnfrepo.SettingsRecord{}, fmt.Errorf("load skill catalog: %w", err)
	}
	return buildNewDarkKnightComboSettingsFromPVF(archive, catalog, record.CharacterID, skill, now)
}

func buildNewDarkKnightComboSettingsFromPVF(
	source darkcombo.TextSource,
	catalog *dnfskill.Table,
	characterID string,
	skill dnfrepo.SkillRecord,
	now time.Time,
) (dnfrepo.SettingsRecord, error) {
	pages, err := darkcombo.BuildDefaultPages(source, catalog, skill)
	if err != nil {
		return dnfrepo.SettingsRecord{}, fmt.Errorf("build PVF combo pages: %w", err)
	}
	if len(pages) != darkcombo.MaximumPageCount {
		return dnfrepo.SettingsRecord{}, fmt.Errorf(
			"PVF combo pages=%d want=%d",
			len(pages),
			darkcombo.MaximumPageCount,
		)
	}
	return darkcombo.BuildSettingsRecord(characterID, pages, now)
}

func ptrInventoryRecord(record dnfrepo.InventoryRecord) *dnfrepo.InventoryRecord {
	return &record
}
