package dnf90

import "testing"

func TestDNFRobotsEnabledDefaultsOff(t *testing.T) {
	t.Setenv("DNFBRIDGE_ROBOT_ENABLED", "")
	if dnfRobotsEnabled() {
		t.Fatal("robots should remain disabled when the switch is absent")
	}
}

func TestDNFRobotsEnabledHonorsExplicitSwitch(t *testing.T) {
	t.Setenv("DNFBRIDGE_ROBOT_ENABLED", "false")
	if dnfRobotsEnabled() {
		t.Fatal("robots should be disabled by an explicit false switch")
	}

	t.Setenv("DNFBRIDGE_ROBOT_ENABLED", " TRUE ")
	if !dnfRobotsEnabled() {
		t.Fatal("robots should be enabled by an explicit true switch")
	}
}
