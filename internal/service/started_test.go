package service

import (
	"context"
	"errors"
	"testing"
	"time"
)

func TestWaitStartedRequiresServiceToStayRunning(t *testing.T) {
	states := []bool{false, true, false, true, true, true, true, true, true, true, true}
	calls := 0
	check := func() (bool, error) {
		state := states[min(calls, len(states)-1)]
		calls++
		return state, nil
	}

	err := waitStarted(context.Background(), check, 20*time.Millisecond, time.Second, 5*time.Millisecond)

	if err != nil || calls < 6 {
		t.Fatalf("service was accepted before it stayed running: calls=%d err=%v", calls, err)
	}
}

func TestWaitStartedRejectsCrashLoop(t *testing.T) {
	calls := 0
	check := func() (bool, error) {
		calls++
		return calls%3 == 0, nil
	}

	err := waitStarted(context.Background(), check, 20*time.Millisecond, 100*time.Millisecond, 5*time.Millisecond)

	if !errors.Is(err, ErrNotRunning) {
		t.Fatalf("crash-looping service was accepted: %v", err)
	}
}
