package dnfbridge

import (
	"context"
	"encoding/binary"
	"net/http"
	"net/http/httptest"
	"strconv"
	"strings"
	"testing"
	"time"

	"longheng.io/server/internal/modules/dnf/dnfenum"
	"longheng.io/server/internal/modules/dnf/premium"
	dnfproto "longheng.io/server/internal/modules/dnf/protocol"
	dnfrepo "longheng.io/server/internal/modules/dnf/repository"
)

func TestBuildCurrentCeraTicketSuccessExactBody(t *testing.T) {
	webURL := "http://127.0.0.1:18111" + PaymentWebOrdinaryPath
	body, err := buildCurrentCeraTicketSuccess(webURL, 0)
	if err != nil {
		t.Fatal(err)
	}
	if body[0] != 1 || binary.LittleEndian.Uint32(body[1:5]) != uint32(len(webURL)) {
		t.Fatalf("cera ticket prefix=% X", body[:5])
	}
	end := 5 + len(webURL)
	if string(body[5:end]) != webURL || binary.LittleEndian.Uint32(body[end:]) != 0 {
		t.Fatalf("cera ticket body=% X", body)
	}
}

func TestBuildCurrentCeraTicketRejectsUnsupportedURLLength(t *testing.T) {
	for _, webURL := range []string{"", strings.Repeat("x", currentCeraTicketMaxURL+1)} {
		if _, err := buildCurrentCeraTicketSuccess(webURL, 0); err == nil {
			t.Fatalf("URL length %d was accepted", len(webURL))
		}
	}
}

func TestCurrentPremiumWebOrdinaryPageOwnsBlackDiamondNavigation(t *testing.T) {
	service := &Service{}
	request := httptest.NewRequest(http.MethodGet, PaymentWebOrdinaryPath, nil)
	response := httptest.NewRecorder()
	service.PaymentWebHandler().ServeHTTP(response, request)
	if response.Code != http.StatusOK ||
		!strings.Contains(response.Body.String(), "黑钻会员") ||
		!strings.Contains(response.Body.String(), PaymentWebBlackDiamondPath) {
		t.Fatalf("ordinary premium page status=%d body=%q", response.Code, response.Body.String())
	}
}

func TestCurrentCeraTicketRequiresClassOne(t *testing.T) {
	service := &Service{}
	session := &gameSession{}
	handled, err := currentGameplayModules.DispatchUpper(
		service,
		session,
		uint16(dnfenum.CmdPacketGenCeraticket),
		0,
		nil,
	)
	if err != nil || !handled {
		t.Fatalf("class0 op65 handled=%t err=%v", handled, err)
	}
	if session.conn != nil {
		t.Fatal("class0 op65 must not create a payment session")
	}
	if dnfproto.DefaultChannelClassification != 1 {
		t.Fatalf("unexpected default channel classification=%d", dnfproto.DefaultChannelClassification)
	}
}

func TestCurrentBlackDiamondActivationPersistsEntitlementAndPreservesFatigueUse(t *testing.T) {
	service, _, session, _, _ := prepareCompletedSettlementRuntime(t)
	now := time.Unix(1_700_000_000, 0).UTC()
	service.gameplayTimers = &processGameplayTimeQueue{now: func() time.Time { return now }}
	repositories, ok := service.repositoryGroup()
	if !ok {
		t.Fatal("repository group unavailable")
	}
	if err := repositories.Account.Save(context.Background(), dnfrepo.AccountRecord{AccountID: "account-1"}); err != nil {
		t.Fatal(err)
	}
	character, found, err := repositories.Character.Load(context.Background(), "99")
	if err != nil || !found {
		t.Fatalf("load character found=%t err=%v", found, err)
	}
	character.Stats["fatigue"] = 120
	character.Stats["fatigue_limit"] = 156
	if err := repositories.Character.Save(context.Background(), character); err != nil {
		t.Fatal(err)
	}
	ticket := &currentPremiumWebTicket{
		accountID: "account-1", characterID: 99,
		characterGeneration: session.characterGeneration, session: session,
	}
	result, err := service.activateCurrentBlackDiamondEntitlement(context.Background(), ticket)
	if err != nil {
		t.Fatal(err)
	}
	wantExpiry := now.Add(currentBlackDiamondTerm).Unix()
	if !result.Active || result.ExpiresAt.Unix() != wantExpiry ||
		result.Character.Stats["fatigue"] != 152 || result.Character.Stats["fatigue_limit"] != 188 {
		t.Fatalf("activation result=%+v stats=%+v", result, result.Character.Stats)
	}
	account, found, err := repositories.Account.Load(context.Background(), "account-1")
	if err != nil || !found {
		t.Fatalf("load account found=%t err=%v", found, err)
	}
	if got := account.Metadata[premium.MetadataKey(premium.TypeBlackDiamondEntitlement)]; got != strconv.FormatInt(wantExpiry, 10) {
		t.Fatalf("black diamond expiry metadata=%q, want %d", got, wantExpiry)
	}
	for _, ordinaryType := range []int64{1, 17, 29, premium.TypeBlackDiamondWire} {
		if _, exists := account.Metadata[premium.MetadataKey(ordinaryType)]; exists {
			t.Fatalf("activation wrote non-entitlement premium type %d", ordinaryType)
		}
	}
}
