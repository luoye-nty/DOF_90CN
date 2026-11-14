package dnf90

import (
	"strings"
	"testing"
	"unicode"
)

func TestNormalizeGMRobotProvisionBatchRequest(t *testing.T) {
	request, err := normalizeGMRobotProvisionBatchRequest(gmRobotProvisionBatchRequest{
		Count: 3, AccountPrefix: " robot-account ", NamePrefix: "假人", ProfilePrefix: " town ", ChannelID: 19,
	})
	if err != nil {
		t.Fatalf("normalize provision batch: %v", err)
	}
	if request.StartIndex != 1 || request.AccountPrefix != "robot-account" || request.ProfilePrefix != "town" {
		t.Fatalf("normalized request=%+v", request)
	}
	if _, err := normalizeGMRobotProvisionBatchRequest(gmRobotProvisionBatchRequest{
		Count: gmRobotMaximumProvisionBatch + 1, AccountPrefix: "a", NamePrefix: "n", ProfilePrefix: "p", ChannelID: 19,
	}); err == nil {
		t.Fatal("oversized provision batch unexpectedly accepted")
	}
	if _, err := normalizeGMRobotProvisionBatchRequest(gmRobotProvisionBatchRequest{
		RobotType: "professional", Count: 1, AccountPrefix: "a", NamePrefix: "n",
		ProfilePrefix: "p", ChannelID: 19,
	}); err == nil {
		t.Fatal("professional robot without expert kind unexpectedly accepted")
	}
	personal, err := normalizeGMRobotProvisionBatchRequest(gmRobotProvisionBatchRequest{
		RobotType: "personal_store", ExpertStoreKind: "enchant", ExpertStoreCost: 500,
		Count: 1, AccountPrefix: "a", NamePrefix: "n", ProfilePrefix: "p", ChannelID: 19,
	})
	if err != nil {
		t.Fatal(err)
	}
	if personal.ExpertStoreKind != "" || personal.ExpertStoreCost != 0 {
		t.Fatalf("personal store retained expert fields: %+v", personal)
	}
}

func TestRandomRobotCharacterNameUsesPrefixAndThreeHanCharacters(t *testing.T) {
	const prefix = "巡游者"
	for attempt := 0; attempt < 64; attempt++ {
		name, err := randomRobotCharacterName(prefix)
		if err != nil {
			t.Fatalf("random robot character name: %v", err)
		}
		runes := []rune(name)
		if len(runes) != 6 || string(runes[:3]) != prefix {
			t.Fatalf("robot character name %q does not use prefix + three characters", name)
		}
		for _, value := range runes[3:] {
			if !unicode.Is(unicode.Han, value) {
				t.Fatalf("robot character name %q contains non-Han suffix %q", name, value)
			}
		}
		if strings.ContainsAny(name, "0123456789") {
			t.Fatalf("robot character name %q contains a digit", name)
		}
	}
}
