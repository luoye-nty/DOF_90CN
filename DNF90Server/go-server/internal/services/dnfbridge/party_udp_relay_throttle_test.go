package dnfbridge

import (
	"testing"
	"time"
)

// TestCurrentPartyUDPRelayForwardBudgetLeavesNormalTrafficAlone pins the
// "defensive only" half of the forwarding budget: traffic a real party produces
// (here 60 datagrams per second for a minute, and a 2,000-datagram combat burst)
// is never refused, so the guard cannot change normal play.
func TestCurrentPartyUDPRelayForwardBudgetLeavesNormalTrafficAlone(t *testing.T) {
	leg := &currentPartyUDPRelayLeg{}
	now := time.Unix(1758300000, 0).UTC()
	for tick := 0; tick < 60; tick++ {
		now = now.Add(time.Second)
		for packet := 0; packet < 60; packet++ {
			admitted, report, _ := leg.admitForward(now)
			if !admitted || report {
				t.Fatalf("normal traffic refused at second=%d packet=%d", tick, packet)
			}
		}
	}
	burst := &currentPartyUDPRelayLeg{}
	for packet := 0; packet < 2000; packet++ {
		admitted, _, _ := burst.admitForward(now)
		if !admitted {
			t.Fatalf("2000-packet combat burst refused at packet=%d", packet)
		}
	}
}

// TestCurrentPartyUDPRelayForwardBudgetCapsARetransmitStorm is the measured
// incident: a client that stops dispatching retransmits the same 9-byte payload
// at ~63,400 datagrams per second. The leg must forward at its budget and refuse
// the rest, and the refusal must be reported once per interval rather than per
// datagram.
func TestCurrentPartyUDPRelayForwardBudgetCapsARetransmitStorm(t *testing.T) {
	leg := &currentPartyUDPRelayLeg{}
	start := time.Unix(1758300000, 0).UTC()
	admitted := 0
	reports := 0
	simulated := 0
	for second := 0; second < 5; second++ {
		now := start.Add(time.Duration(second) * time.Second)
		for packet := 0; packet < 63400; packet++ {
			simulated++
			ok, report, _ := leg.admitForward(now)
			if ok {
				admitted++
			}
			if report {
				reports++
			}
		}
	}
	if simulated != 5*63400 {
		t.Fatalf("simulated=%d", simulated)
	}
	// One full burst plus five seconds of refill, with a little slack for the
	// bucket's per-datagram accounting.
	upper := currentPartyUDPRelayForwardBurstPackets +
		int(5*currentPartyUDPRelayForwardRatePerSecond) + 100
	if admitted > upper {
		t.Fatalf("admitted=%d exceeds budget ceiling=%d", admitted, upper)
	}
	if admitted > simulated/8 {
		t.Fatalf("admitted=%d of %d: a 63k/s storm was not cut down", admitted, simulated)
	}
	if reports == 0 || reports > 6 {
		t.Fatalf("throttle reports=%d, want at least one and never one per datagram", reports)
	}
	leg.mu.Lock()
	refused := leg.throttledPackets
	leg.mu.Unlock()
	if refused != uint64(simulated-admitted) {
		t.Fatalf("refused=%d admitted=%d simulated=%d", refused, admitted, simulated)
	}
}

// TestCurrentPartyUDPRelayForwardBudgetRefillsAfterTheStorm pins recovery: once
// the client behaves again the same leg forwards normally, so a single storm does
// not permanently degrade the party transport.
func TestCurrentPartyUDPRelayForwardBudgetRefillsAfterTheStorm(t *testing.T) {
	leg := &currentPartyUDPRelayLeg{}
	now := time.Unix(1758300000, 0).UTC()
	for packet := 0; packet < 10000; packet++ {
		leg.admitForward(now)
	}
	// One second of quiet, then a normal second of play.
	now = now.Add(2 * time.Second)
	for packet := 0; packet < 60; packet++ {
		if admitted, _, _ := leg.admitForward(now); !admitted {
			t.Fatalf("leg did not recover after the storm at packet=%d", packet)
		}
	}
}
