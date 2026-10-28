package dnfbridge

import (
	"context"
	"testing"
	"time"

	dnfdungeonadmission "longheng.io/server/internal/modules/dnf/dungeonadmission"
)

func TestGMRaidAdmissionCountersListsAllThreeRaidsInOrder(t *testing.T) {
	service, _, _, _ := newRuntimeRaidAdmissionCounterTest(t, 0, 1001)
	state, err := service.GMRaidAdmissionCounters(context.Background(), "1001")
	if err != nil {
		t.Fatal(err)
	}
	if len(state.Groups) != 3 {
		t.Fatalf("raid groups=%+v", state.Groups)
	}
	wantRaids := []string{gmRaidAdmissionRaidAnton, gmRaidAdmissionRaidLuke, gmRaidAdmissionRaidFiendWar}
	wantCounters := []string{
		gmRaidAdmissionAccountDaily,
		gmRaidAdmissionAccountWeekly,
		gmRaidAdmissionCharacterWeekly,
	}
	for index, group := range state.Groups {
		if group.Raid != wantRaids[index] {
			t.Fatalf("group[%d]=%s want=%s", index, group.Raid, wantRaids[index])
		}
		if len(group.Counters) != 3 {
			t.Fatalf("group=%s counters=%+v", group.Raid, group.Counters)
		}
		for counterIndex, counter := range group.Counters {
			if counter.ID != wantCounters[counterIndex] {
				t.Fatalf("group=%s counter[%d]=%s want=%s", group.Raid, counterIndex, counter.ID, wantCounters[counterIndex])
			}
		}
	}
}

func TestGMResetRaidAdmissionCounterResetsOnlyTheSelectedCount(t *testing.T) {
	service, repositories, catalog, _ := newRuntimeRaidAdmissionCounterTest(t, 0, 1001)
	now := time.Now().UTC()
	policyKeys := []string{
		currentRaidLukeDailyPolicyKey,
		dnfdungeonadmission.LukeWeeklyAccountPolicyKey,
		dnfdungeonadmission.LukeWeeklyCharacterPolicyKey,
	}
	policies := make([]dnfdungeonadmission.Policy, 0, len(policyKeys))
	for _, key := range policyKeys {
		policy, found := catalog.Policy(key)
		if !found {
			t.Fatalf("policy %s missing", key)
		}
		policies = append(policies, policy)
	}
	owner, err := dnfdungeonadmission.NewOwner(repositories)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := owner.Commit(context.Background(), "1001", policies, "gm-reset-test", now); err != nil {
		t.Fatal(err)
	}
	state, err := service.GMRaidAdmissionCounters(context.Background(), "1001")
	if err != nil {
		t.Fatal(err)
	}
	if used := gmRaidCounterUsed(t, state, gmRaidAdmissionRaidLuke, gmRaidAdmissionAccountDaily); used != 1 {
		t.Fatalf("luke account daily used=%d want=1", used)
	}

	result, err := service.GMResetRaidAdmissionCounter(
		context.Background(),
		"1001",
		gmRaidAdmissionRaidLuke,
		gmRaidAdmissionAccountDaily,
	)
	if err != nil {
		t.Fatal(err)
	}
	if result.ResetCounter != "luke/account_daily" ||
		gmRaidCounterUsed(t, result.State, gmRaidAdmissionRaidLuke, gmRaidAdmissionAccountDaily) != 0 ||
		gmRaidCounterUsed(t, result.State, gmRaidAdmissionRaidLuke, gmRaidAdmissionAccountWeekly) != 1 ||
		gmRaidCounterUsed(t, result.State, gmRaidAdmissionRaidLuke, gmRaidAdmissionCharacterWeekly) != 1 {
		t.Fatalf("reset result=%+v", result)
	}
}

func TestGMResetRaidAdmissionCounterRejectsUnknownSelection(t *testing.T) {
	service, _, _, _ := newRuntimeRaidAdmissionCounterTest(t, 0, 1001)
	if _, err := service.GMResetRaidAdmissionCounter(
		context.Background(), "1001", "unknown", gmRaidAdmissionAccountDaily,
	); err == nil {
		t.Fatal("unknown raid was accepted")
	}
}

func gmRaidCounterUsed(t *testing.T, state GMRaidAdmissionState, raid, counter string) uint32 {
	t.Helper()
	for _, group := range state.Groups {
		if group.Raid != raid {
			continue
		}
		for _, value := range group.Counters {
			if value.ID == counter {
				return value.Used
			}
		}
	}
	t.Fatalf("counter raid=%s id=%s missing in %+v", raid, counter, state.Groups)
	return 0
}
