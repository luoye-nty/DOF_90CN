package repository

import "testing"

func TestAccountCharacterSlotLimitDefaultsAndPersistsExpansion(t *testing.T) {
	tests := []struct {
		name     string
		metadata map[string]string
		want     int
	}{
		{name: "missing", want: DefaultCharacterSlotLimit},
		{name: "malformed", metadata: map[string]string{CharacterSlotLimitMetadataKey: "bad"}, want: DefaultCharacterSlotLimit},
		{name: "below default", metadata: map[string]string{CharacterSlotLimitMetadataKey: "31"}, want: DefaultCharacterSlotLimit},
		{name: "above maximum", metadata: map[string]string{CharacterSlotLimitMetadataKey: "61"}, want: DefaultCharacterSlotLimit},
		{name: "expanded", metadata: map[string]string{CharacterSlotLimitMetadataKey: "33"}, want: 33},
		{name: "maximum", metadata: map[string]string{CharacterSlotLimitMetadataKey: "60"}, want: MaxCharacterSlotLimit},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			if got := AccountCharacterSlotLimit(AccountRecord{Metadata: test.metadata}); got != test.want {
				t.Fatalf("slot limit=%d, want %d", got, test.want)
			}
		})
	}
	account := AccountRecord{}
	if err := SetAccountCharacterSlotLimit(&account, 33); err != nil {
		t.Fatal(err)
	}
	if account.Metadata[CharacterSlotLimitMetadataKey] != "33" || AccountCharacterSlotLimit(account) != 33 {
		t.Fatalf("expanded account=%+v", account)
	}
	if err := SetAccountCharacterSlotLimit(&account, MaxCharacterSlotLimit+1); err == nil {
		t.Fatal("above-maximum character slot limit was accepted")
	}
}
