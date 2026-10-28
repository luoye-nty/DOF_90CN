package main

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"os"

	_ "modernc.org/sqlite"

	dnfdungeon "longheng.io/server/internal/modules/dnf/dungeon"
	"longheng.io/server/internal/modules/dnf/progression"
	"longheng.io/server/internal/modules/dnf/repository"
	platformpvf "longheng.io/server/internal/platform/pvf"
)

func main() {
	databasePath := flag.String("db", "", "SQLite save database")
	characterID := flag.String("character-id", "", "optional character ID to inspect")
	listCharacters := flag.Bool("list-characters", false, "list compact character identity and profession state")
	pvfPath := flag.String("pvf", "", "optional Script.pvf used to project a settlement")
	experienceGain := flag.Uint("experience-gain", 0, "optional settlement experience gain")
	flag.Parse()
	if *databasePath == "" {
		fmt.Fprintln(os.Stderr, "FAILED: -db is required")
		os.Exit(2)
	}
	database, err := sql.Open("sqlite", *databasePath)
	if err != nil {
		panic(err)
	}
	defer database.Close()
	if *listCharacters {
		rows, err := database.Query(`SELECT record_key, payload FROM dnf_repository_records WHERE record_kind = 'character' ORDER BY CAST(record_key AS INTEGER), record_key`)
		if err != nil {
			panic(err)
		}
		defer rows.Close()
		characters := make([]map[string]any, 0)
		for rows.Next() {
			var recordKey string
			var payload []byte
			if err := rows.Scan(&recordKey, &payload); err != nil {
				panic(err)
			}
			var character repository.CharacterRecord
			if err := json.Unmarshal(payload, &character); err != nil {
				panic(err)
			}
			characters = append(characters, map[string]any{
				"record_key": recordKey,
				"id":         character.CharacterID,
				"name":       character.Name,
				"job":        character.Job,
				"grow_type":  character.Stats["grow_type"],
				"level":      character.Level,
			})
		}
		if err := rows.Err(); err != nil {
			panic(err)
		}
		encoder := json.NewEncoder(os.Stdout)
		encoder.SetIndent("", "  ")
		if err := encoder.Encode(characters); err != nil {
			panic(err)
		}
		return
	}
	var username, accountID string
	if err := database.QueryRow(`SELECT username, account_id FROM dnf_login_accounts LIMIT 1`).Scan(&username, &accountID); err != nil {
		panic(err)
	}
	characterQuery := `SELECT payload FROM dnf_repository_records WHERE record_kind = 'character' ORDER BY record_key LIMIT 1`
	characterArgs := []any(nil)
	if *characterID != "" {
		characterQuery = `SELECT payload FROM dnf_repository_records WHERE record_kind = 'character' AND record_key = ?`
		characterArgs = append(characterArgs, *characterID)
	}
	var characterJSON []byte
	if err := database.QueryRow(characterQuery, characterArgs...).Scan(&characterJSON); err != nil {
		panic(err)
	}
	var character repository.CharacterRecord
	if err := json.Unmarshal(characterJSON, &character); err != nil {
		panic(err)
	}
	var skill repository.SkillRecord
	skillFound := loadRepositoryRecord(database, "skill", character.CharacterID, &skill)
	var inventory repository.InventoryRecord
	inventoryFound := loadRepositoryRecord(database, "inventory", character.CharacterID, &inventory)
	experience, experienceFound := character.Stats["exp"]
	var records, accounts, characters int
	for _, query := range []struct {
		sql    string
		target *int
	}{
		{`SELECT COUNT(*) FROM dnf_repository_records`, &records},
		{`SELECT COUNT(*) FROM dnf_repository_records WHERE record_kind = 'account'`, &accounts},
		{`SELECT COUNT(*) FROM dnf_repository_records WHERE record_kind = 'character'`, &characters},
	} {
		if err := database.QueryRow(query.sql).Scan(query.target); err != nil {
			panic(err)
		}
	}
	result := map[string]any{
		"username":            username,
		"account_id":          accountID,
		"character_id":        character.CharacterID,
		"character_name":      character.Name,
		"character_job":       character.Job,
		"character_level":     character.Level,
		"character_grow_type": character.Stats["grow_type"],
		"experience":          experience,
		"experience_found":    experienceFound,
		"skill_found":         skillFound,
		"skill_character":     skill.CharacterID,
		"skill_points":        skill.Points,
		"skill_synced_level":  skill.Points.SyncedLevel,
		"inventory_found":     inventoryFound,
		"inventory_character": inventory.CharacterID,
		"inventory_slots":     inventory.Slots,
		"record_count":        records,
		"account_count":       accounts,
		"character_count":     characters,
	}
	if *pvfPath != "" && *experienceGain > 0 {
		archive, err := platformpvf.Open(*pvfPath)
		if err != nil {
			panic(err)
		}
		tables, err := progression.Load(context.Background(), archive)
		if err != nil {
			panic(err)
		}
		plan, err := dnfdungeon.PlanSettlementProgressionAtCap(
			tables,
			character.Level,
			uint32(experience),
			uint32(*experienceGain),
			skill.Points,
			90,
		)
		result["settlement_plan"] = plan
		if err != nil {
			result["settlement_error"] = err.Error()
		}
	}
	encoder := json.NewEncoder(os.Stdout)
	encoder.SetIndent("", "  ")
	if err := encoder.Encode(result); err != nil {
		panic(err)
	}
}

func loadRepositoryRecord(database *sql.DB, kind, key string, target any) bool {
	var payload []byte
	err := database.QueryRow(
		`SELECT payload FROM dnf_repository_records WHERE record_kind = ? AND record_key = ?`,
		kind,
		key,
	).Scan(&payload)
	if errors.Is(err, sql.ErrNoRows) {
		return false
	}
	if err != nil {
		panic(err)
	}
	if err := json.Unmarshal(payload, target); err != nil {
		panic(err)
	}
	return true
}
