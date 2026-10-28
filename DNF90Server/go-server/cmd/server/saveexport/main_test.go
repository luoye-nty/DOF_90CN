package main

import (
	"strings"
	"testing"
)

func TestReadPasswordStripsWindowsPipelineBOM(t *testing.T) {
	password, err := readPassword(strings.NewReader("\ufefftest123456\r\n"))
	if err != nil {
		t.Fatal(err)
	}
	if password != "test123456" {
		t.Fatalf("password = %q", password)
	}
}

func TestBuildGuideIncludesPublishedTestCredentials(t *testing.T) {
	guide := buildGuide(bundleInfo{
		Username:       "test",
		CharacterName:  "Test",
		CharacterLevel: 90,
	}, "test123456")
	for _, want := range []string{"账号：test", "密码：test123456", "角色：Test（90 级）"} {
		if !strings.Contains(guide, want) {
			t.Fatalf("guide is missing %q: %s", want, guide)
		}
	}
}
