package dnfrobot

import (
	"errors"
	"fmt"
	"testing"
)

func TestManagerOperationHistoryIsBoundedAndFrozen(t *testing.T) {
	manager := &Manager{}
	for index := 0; index < maximumOperationHistory+3; index++ {
		operation := manager.BeginOperation("batch", fmt.Sprintf("scope-%d", index))
		if index == 2 {
			manager.CompleteOperation(operation.ID, "failed row", errors.New("boom"))
		} else {
			manager.CompleteOperation(operation.ID, "done", nil)
		}
	}
	history := manager.Operations()
	if len(history) != maximumOperationHistory || history[0].Scope != "scope-22" || history[len(history)-1].Scope != "scope-3" {
		t.Fatalf("operation history=%+v", history)
	}
	history[0].Scope = "mutated"
	if manager.Operations()[0].Scope == "mutated" {
		t.Fatal("operation history leaked mutable storage")
	}
}
