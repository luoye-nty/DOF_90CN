// questautocompleteinspect replays the op1429 bulk main-quest auto-complete
// planner against a dumped character quest record. It is a read-only
// diagnostic: it never touches the archive, the database or the repository.
//
// The quest record is read from a TSV dump of dnf_quest_states:
//
//	quest_id<TAB>status<TAB>state_group
package main

import (
	"bufio"
	"context"
	"flag"
	"fmt"
	"os"
	"sort"
	"strconv"
	"strings"
	"time"

	"longheng.io/server/internal/modules/dnf/jobmap"
	dnfpvf "longheng.io/server/internal/modules/dnf/pvf"
	"longheng.io/server/internal/modules/dnf/quest"
	dnfrepo "longheng.io/server/internal/modules/dnf/repository"
	platformpvf "longheng.io/server/internal/platform/pvf"
)

func main() {
	pvfPath := flag.String("pvf", "", "Script.pvf path (required)")
	statesPath := flag.String("states", "", "TSV dump of dnf_quest_states (required)")
	level := flag.Int("level", 90, "character level")
	job := flag.Int("job", 0, "character job")
	growType := flag.Int("grow-type", 0, "character grow_type")
	target := flag.Int64("target", 0, "op1429 request selector (0 = bulk)")
	flag.Parse()
	if strings.TrimSpace(*pvfPath) == "" || strings.TrimSpace(*statesPath) == "" {
		fatalf("-pvf and -states are required")
	}

	record := dnfrepo.QuestRecord{
		CharacterID: "inspect",
		States:      make(map[int64]dnfrepo.QuestState),
		Progress:    make(map[int64]dnfrepo.QuestState),
	}
	file, err := os.Open(*statesPath)
	if err != nil {
		fatalf("open states: %v", err)
	}
	scanner := bufio.NewScanner(file)
	for scanner.Scan() {
		fields := strings.Split(strings.TrimRight(scanner.Text(), "\r\n"), "\t")
		if len(fields) < 3 {
			continue
		}
		questID, convErr := strconv.ParseInt(strings.TrimSpace(fields[0]), 10, 64)
		if convErr != nil {
			continue
		}
		state := dnfrepo.QuestState{Status: strings.TrimSpace(fields[1])}
		if strings.TrimSpace(fields[2]) == "progress" {
			record.Progress[questID] = state
		} else {
			record.States[questID] = state
		}
	}
	file.Close()
	if err := scanner.Err(); err != nil {
		fatalf("read states: %v", err)
	}
	fmt.Printf("states=%d progress=%d\n", len(record.States), len(record.Progress))

	archive, err := platformpvf.LoadArchive(platformpvf.Options{Path: *pvfPath})
	if err != nil {
		fatalf("open PVF: %v", err)
	}
	index, err := dnfpvf.Build(context.Background(), archive, dnfpvf.BuildOptions{Lists: []string{quest.DefaultList}})
	if err != nil {
		fatalf("build PVF index: %v", err)
	}
	catalog, err := quest.Load(context.Background(), index)
	if err != nil {
		fatalf("load quest catalog: %v", err)
	}

	character := quest.CharacterEligibility{Level: *level, Job: *job, GrowType: *growType}
	cutoff := *level - 1
	if cutoff > 89 {
		cutoff = 89
	}
	plan, planErr := catalog.PlanAutoCompleteMainQuests(record, character, cutoff, *target, time.Now().UTC())
	if planErr != nil {
		fmt.Printf("plan error: %v\n", planErr)
	} else {
		sort.Slice(plan.EligibleQuestIDs, func(i, j int) bool {
			return plan.EligibleQuestIDs[i] < plan.EligibleQuestIDs[j]
		})
		fmt.Printf("eligible=%d changed=%d linked_sub=%d active_completed=%d highest=%d\n",
			len(plan.EligibleQuestIDs), len(plan.ChangedQuestIDs), len(plan.ChangedLinkedSubIDs),
			plan.ActiveCompletedCount, plan.HighestCompletedQuest)
		fmt.Printf("eligible ids: %v\n", plan.EligibleQuestIDs)
	}

	// Break the "no candidate" verdict down so the blocking predicate is visible.
	completed := make(map[int64]struct{}, len(record.States))
	for id, state := range record.States {
		if strings.EqualFold(strings.TrimSpace(state.Status), "completed") {
			completed[id] = struct{}{}
		}
	}
	var epic, inLevel, exposed, notEvent, jobOK, uncompleted, prereqOK int
	for _, definition := range catalog.Definitions() {
		if !strings.EqualFold(strings.Trim(definition.Grade, " []\t\r\n"), "epic") {
			continue
		}
		epic++
		if definition.LevelMin <= 0 || definition.LevelMin > cutoff {
			continue
		}
		inLevel++
		if definition.ExposedByNPC == 0 {
			continue
		}
		exposed++
		if definition.IsEvent || strings.EqualFold(strings.Trim(definition.Type, " []\t\r\n"), "question") {
			continue
		}
		notEvent++
		if !matchesCharacter(definition, character) {
			continue
		}
		jobOK++
		if _, done := completed[definition.ID]; done {
			continue
		}
		uncompleted++
		blocked := prerequisiteBlocker(definition, completed)
		if blocked == "" {
			prereqOK++
		}
		if uncompleted <= 30 {
			fmt.Printf("  open id=%d lvl=%d type=%q job=%q main=%d jobChange=%d hasGrow=%v grow=%d blocked=%q\n",
				definition.ID, definition.LevelMin, definition.Type, definition.Job,
				definition.MainQuestID, definition.JobChangeQuest,
				definition.HasGrowType, definition.GrowType, blocked)
		}
	}
	fmt.Printf("epic=%d in_level=%d exposed=%d not_event_or_question=%d job_match=%d open=%d prereq_ok=%d\n",
		epic, inLevel, exposed, notEvent, jobOK, uncompleted, prereqOK)
}

func matchesCharacter(definition quest.Definition, character quest.CharacterEligibility) bool {
	if job := strings.TrimSpace(definition.Job); job != "" && !strings.EqualFold(strings.Trim(job, " []\t\r\n"), "all") &&
		!jobmap.MatchesQuestTag(job, character.Job) {
		return false
	}
	if len(definition.TargetCharacterRules) != 0 && definition.JobChangeQuest >= 1 && definition.JobChangeQuest <= 3 {
		matched := false
		firstGrowType := character.GrowType & 0x0f
		awakeningStage := (character.GrowType >> 4) & 0x0f
		for _, rule := range definition.TargetCharacterRules {
			if jobmap.MatchesQuestTag(rule.JobTag, character.Job) &&
				rule.FirstGrowType == firstGrowType && rule.AwakeningStage == awakeningStage {
				matched = true
				break
			}
		}
		if !matched {
			return false
		}
	} else if target := strings.TrimSpace(definition.TargetCharacter); target != "" &&
		!jobmap.MatchesQuestTag(target, character.Job) {
		return false
	}
	if definition.HasGrowType && definition.GrowType >= 0 {
		switch definition.JobChangeQuest {
		case 1, 10, 20:
		case 2, 3:
			if definition.GrowType != character.GrowType&0x0f {
				return false
			}
		default:
			if definition.GrowType != character.GrowType {
				return false
			}
		}
	}
	return true
}

func prerequisiteBlocker(definition quest.Definition, completed map[int64]struct{}) string {
	if definition.MalformedPreAnswers {
		return "malformed_pre_answers"
	}
	if definition.MainQuestID > 0 {
		if _, done := completed[definition.MainQuestID]; !done {
			return fmt.Sprintf("parent_%d", definition.MainQuestID)
		}
	}
	if len(definition.PreRequiredGroups) > 0 {
		for _, group := range definition.PreRequiredGroups {
			all := true
			for _, id := range group {
				if _, done := completed[id]; !done {
					all = false
					break
				}
			}
			if all {
				goto groupsOK
			}
		}
		return fmt.Sprintf("pre_required_%v", definition.PreRequiredGroups)
	}
groupsOK:
	if len(definition.PreRequiredAnswers) > 0 {
		return "pre_required_answer"
	}
	for _, collision := range definition.CollisionQuests {
		if collision <= 0 {
			continue
		}
		if _, done := completed[collision]; done {
			return fmt.Sprintf("collision_%d", collision)
		}
	}
	return ""
}

func fatalf(format string, args ...any) {
	fmt.Fprintf(os.Stderr, format+"\n", args...)
	os.Exit(1)
}
