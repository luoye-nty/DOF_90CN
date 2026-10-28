//go:build windows

package main

import (
	"context"
	"testing"
	"time"
)

func TestWineControllerMutexCanBeReleasedAndReacquired(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), time.Second)
	defer cancel()
	root := t.TempDir()
	firstRelease, err := acquireWineControllerLock(ctx, root)
	if err != nil {
		t.Fatal(err)
	}
	firstRelease()
	secondRelease, err := acquireWineControllerLock(ctx, root)
	if err != nil {
		t.Fatal(err)
	}
	secondRelease()
}
