package experiencebonus

import (
	"errors"
	"testing"
	"time"

	dnfrepo "longheng.io/server/internal/modules/dnf/repository"
)

func TestActivateAndExpireExperienceConsumable(t *testing.T) {
	now := time.Unix(1_700_000_000, 0).UTC()
	account := dnfrepo.AccountRecord{AccountID: "acc"}
	effect, err := Activate(&account, 10005931, 200, 30*time.Minute, now)
	if err != nil {
		t.Fatal(err)
	}
	if effect.BonusPercent != 200 || !effect.ExpireAt.Equal(now.Add(30*time.Minute)) {
		t.Fatalf("effect=%+v", effect)
	}
	active, ok := Active(account, now.Add(29*time.Minute))
	if !ok || active.ItemID != 10005931 || active.BonusPercent != 200 {
		t.Fatalf("active=%+v ok=%t", active, ok)
	}
	if _, err := Activate(&account, 10006569, 100, 30*time.Minute, now.Add(time.Minute)); !errors.Is(err, ErrEffectActive) {
		t.Fatalf("overlapping activation error=%v", err)
	}
	if _, ok := Active(account, now.Add(30*time.Minute)); ok {
		t.Fatal("effect remained active at expiry")
	}
}
