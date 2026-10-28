package dnfbridge

import (
	"sync/atomic"
	"testing"
	"time"
)

func TestScheduleProcessMemoryTrimRunsOnce(t *testing.T) {
	service := &Service{}
	var calls atomic.Int32
	called := make(chan struct{}, 2)
	trim := func() {
		calls.Add(1)
		called <- struct{}{}
	}
	service.scheduleProcessMemoryTrim(0, trim)
	service.scheduleProcessMemoryTrim(0, trim)
	select {
	case <-called:
	case <-time.After(time.Second):
		t.Fatal("scheduled memory trim did not run")
	}
	select {
	case <-called:
		t.Fatal("scheduled memory trim ran more than once")
	case <-time.After(20 * time.Millisecond):
	}
	if got := calls.Load(); got != 1 {
		t.Fatalf("trim calls=%d want=1", got)
	}
}
