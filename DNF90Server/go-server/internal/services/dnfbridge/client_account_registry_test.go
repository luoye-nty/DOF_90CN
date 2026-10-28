package dnfbridge

import (
	"context"
	"net/netip"
	"testing"
	"time"
)

func TestRemoteClientAccountRegistryBindsDistinctWireGuardSources(t *testing.T) {
	service := &Service{clientAccounts: newClientAccountRegistry()}
	if err := service.RegisterRemoteClientAccount(0, "10.66.0.2", "account-a"); err != nil {
		t.Fatal(err)
	}
	if err := service.RegisterRemoteClientAccount(0, "10.66.0.3", "account-b"); err != nil {
		t.Fatal(err)
	}
	first := netip.MustParseAddr("10.66.0.2")
	second := netip.MustParseAddr("10.66.0.3")
	if got, pid, ok := service.registeredRemoteClientAccount(first); !ok || got != "account-a" || pid != 0 {
		t.Fatalf("first remote account=%q pid=%d found=%t", got, pid, ok)
	}
	if got, pid, ok := service.registeredRemoteClientAccount(second); !ok || got != "account-b" || pid != 0 {
		t.Fatalf("second remote account=%q pid=%d found=%t", got, pid, ok)
	}
	if got, ok := service.uniqueRegisteredClientAccount(); ok || got != "" {
		t.Fatalf("remote registrations leaked into local Wine fallback: account=%q found=%t", got, ok)
	}
}

func TestRemoteClientAccountRegistryReplacesOneSourceAndKeepsLifecycleIsolated(t *testing.T) {
	service := &Service{clientAccounts: newClientAccountRegistry()}
	if err := service.RegisterRemoteClientAccount(321, "10.66.0.2", "old-account"); err != nil {
		t.Fatal(err)
	}
	if err := service.RegisterRemoteClientAccount(654, "10.66.0.2", "new-account"); err != nil {
		t.Fatal(err)
	}
	if err := service.RegisterRemoteClientAccount(654, "10.66.0.3", "other-account"); err != nil {
		t.Fatal(err)
	}
	first := &gameSession{accountSourceIP: netip.MustParseAddr("10.66.0.2")}
	second := &gameSession{accountSourceIP: netip.MustParseAddr("10.66.0.3")}
	if got, pid, ok := service.registeredRemoteClientAccount(first.accountSourceIP); !ok || got != "new-account" || pid != 654 {
		t.Fatalf("replacement account=%q pid=%d found=%t", got, pid, ok)
	}
	if send, ready := service.beginCurrentSpendTimeEventInfo(first); !send || !ready {
		t.Fatalf("first remote descriptor send=%t ready=%t", send, ready)
	}
	if send, ready := service.beginCurrentSpendTimeEventInfo(second); !send || !ready {
		t.Fatalf("second remote descriptor was not isolated: send=%t ready=%t", send, ready)
	}
}

func TestRemoteClientAccountRegistryRejectsNonPrivateSources(t *testing.T) {
	service := &Service{clientAccounts: newClientAccountRegistry()}
	for _, address := range []string{"", "127.0.0.1", "8.8.8.8", "2001:db8::1"} {
		if err := service.RegisterRemoteClientAccount(0, address, "account-a"); err == nil {
			t.Fatalf("RegisterRemoteClientAccount(%q) succeeded", address)
		}
	}
}

func TestGameSessionAccountOverridesInstanceFallback(t *testing.T) {
	service := &Service{}
	service.options.accountID = "fallback-account"
	session := &gameSession{accountID: "session-account"}
	if got := service.accountIDForSession(session); got != "session-account" {
		t.Fatalf("session account=%q, want session-account", got)
	}
	if got := service.accountIDForSession(nil); got != "fallback-account" {
		t.Fatalf("fallback account=%q, want fallback-account", got)
	}
}

func TestClientAccountRegistryBindsDistinctPIDs(t *testing.T) {
	service := &Service{clientAccounts: newClientAccountRegistry()}
	if err := service.RegisterClientAccount(101, "account-a"); err != nil {
		t.Fatal(err)
	}
	if err := service.RegisterClientAccount(202, "account-b"); err != nil {
		t.Fatal(err)
	}
	if got, ok := service.registeredClientAccount(101); !ok || got != "account-a" {
		t.Fatalf("PID 101 account=%q found=%t", got, ok)
	}
	if got, ok := service.registeredClientAccount(202); !ok || got != "account-b" {
		t.Fatalf("PID 202 account=%q found=%t", got, ok)
	}
}

func TestClientAccountRegistryResolvesRegisteredLauncherAncestor(t *testing.T) {
	service := &Service{clientAccounts: newClientAccountRegistry()}
	if err := service.RegisterClientAccount(101, "account-a"); err != nil {
		t.Fatal(err)
	}
	if got, found, direct := service.registeredClientAccountForCandidates([]uint32{303, 202, 101}); !found || direct || got != "account-a" {
		t.Fatalf("ancestor account=%q found=%t direct=%t", got, found, direct)
	}
}

func TestWaitRegisteredClientAccountClosesLauncherStartRace(t *testing.T) {
	service := &Service{clientAccounts: newClientAccountRegistry()}
	registered := make(chan struct{})
	go func() {
		time.Sleep(25 * time.Millisecond)
		if err := service.RegisterClientAccount(303, "account-c"); err != nil {
			return
		}
		close(registered)
	}()
	started := time.Now()
	accountID, found := service.waitRegisteredClientAccount(context.Background(), 303, time.Second)
	if !found || accountID != "account-c" {
		t.Fatalf("late registration account=%q found=%t", accountID, found)
	}
	if elapsed := time.Since(started); elapsed < 20*time.Millisecond || elapsed > 500*time.Millisecond {
		t.Fatalf("late registration wait=%s, want bounded launcher grace", elapsed)
	}
	select {
	case <-registered:
	default:
		t.Fatal("registration goroutine did not complete")
	}
}

func TestWaitRegisteredClientAccountHonorsCancellation(t *testing.T) {
	service := &Service{clientAccounts: newClientAccountRegistry()}
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	started := time.Now()
	if accountID, found := service.waitRegisteredClientAccount(ctx, 404, time.Second); found || accountID != "" {
		t.Fatalf("cancelled wait account=%q found=%t", accountID, found)
	}
	if elapsed := time.Since(started); elapsed > 100*time.Millisecond {
		t.Fatalf("cancelled wait took %s", elapsed)
	}
}

func TestUniqueRegisteredClientAccountRequiresOneAccountIdentity(t *testing.T) {
	service := &Service{clientAccounts: newClientAccountRegistry()}
	if accountID, found := service.uniqueRegisteredClientAccount(); found || accountID != "" {
		t.Fatalf("empty registry account=%q found=%t", accountID, found)
	}
	if err := service.RegisterClientAccount(101, "account-a"); err != nil {
		t.Fatal(err)
	}
	if err := service.RegisterClientAccount(202, "account-a"); err != nil {
		t.Fatal(err)
	}
	if accountID, found := service.uniqueRegisteredClientAccount(); !found || accountID != "account-a" {
		t.Fatalf("single identity account=%q found=%t", accountID, found)
	}
	if err := service.RegisterClientAccount(303, "account-b"); err != nil {
		t.Fatal(err)
	}
	if accountID, found := service.uniqueRegisteredClientAccount(); found || accountID != "" {
		t.Fatalf("ambiguous registry account=%q found=%t", accountID, found)
	}
}

func TestWaitUniqueRegisteredClientAccountClosesWineLauncherRace(t *testing.T) {
	service := &Service{clientAccounts: newClientAccountRegistry()}
	go func() {
		time.Sleep(25 * time.Millisecond)
		_ = service.RegisterClientAccount(404, "wine-account")
	}()
	accountID, found := service.waitUniqueRegisteredClientAccount(
		context.Background(),
		time.Second,
	)
	if !found || accountID != "wine-account" {
		t.Fatalf("Wine fallback account=%q found=%t", accountID, found)
	}
}

func TestClientAccountRegistryOwnsSpendTimeDescriptorPerPIDAndRegistrationResetsIt(t *testing.T) {
	service := &Service{clientAccounts: newClientAccountRegistry()}
	if err := service.RegisterClientAccount(101, "account-a"); err != nil {
		t.Fatal(err)
	}
	if err := service.RegisterClientAccount(101, "account-a"); err != nil {
		t.Fatal(err)
	}
	first := &gameSession{clientPID: 101}
	second := &gameSession{clientPID: 101}
	if send, ready := service.beginCurrentSpendTimeEventInfo(first); !send || !ready {
		t.Fatalf("first reservation send=%t ready=%t", send, ready)
	}
	if send, ready := service.beginCurrentSpendTimeEventInfo(second); send || ready {
		t.Fatalf("concurrent reservation send=%t ready=%t", send, ready)
	}
	service.finishCurrentSpendTimeEventInfo(first, true)
	if send, ready := service.beginCurrentSpendTimeEventInfo(second); send || !ready || !second.spendTime.eventInfoSent {
		t.Fatalf("same PID after send=%t ready=%t session_bit=%t", send, ready, second.spendTime.eventInfoSent)
	}
	if err := service.RegisterClientAccount(101, "account-a"); err != nil {
		t.Fatal(err)
	}
	third := &gameSession{clientPID: 101}
	if send, ready := service.beginCurrentSpendTimeEventInfo(third); !send || !ready {
		t.Fatalf("new process lifecycle did not reset descriptor send=%t ready=%t", send, ready)
	}
	service.finishCurrentSpendTimeEventInfo(third, false)
	if send, ready := service.beginCurrentSpendTimeEventInfo(third); !send || !ready {
		t.Fatalf("failed write did not release descriptor reservation send=%t ready=%t", send, ready)
	}
}

func TestClientAccountRegistryPIDZeroUsesProgressOnly(t *testing.T) {
	service := &Service{clientAccounts: newClientAccountRegistry()}
	session := &gameSession{}
	if send, ready := service.beginCurrentSpendTimeEventInfo(session); send || !ready {
		t.Fatalf("PID zero descriptor send=%t ready=%t", send, ready)
	}
	if !session.spendTime.eventInfoSent {
		t.Fatal("PID zero progress-only gate was not closed")
	}
}

func TestClientAccountRegistryRecreatedForRunningPIDUsesProgressOnly(t *testing.T) {
	service := &Service{clientAccounts: newClientAccountRegistry()}
	session := &gameSession{clientPID: 101}
	if send, ready := service.beginCurrentSpendTimeEventInfo(session); send || !ready {
		t.Fatalf("unknown running PID descriptor send=%t ready=%t", send, ready)
	}
	if !session.spendTime.eventInfoSent {
		t.Fatal("progress-only recovery did not close the session descriptor gate")
	}
}
