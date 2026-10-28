package dnf90

import "testing"

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
}
