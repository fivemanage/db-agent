package controlplane

import (
	"context"
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"net/http/httptest"
	"sync/atomic"
	"testing"
	"time"
)

func TestHeartbeatWorkerSendsImmediatelyAndReadsCurrentActivityOnTicks(t *testing.T) {
	var calls atomic.Int32
	observed := make(chan Status, 3)
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		calls.Add(1)
		var request heartbeatRequest
		if err := json.NewDecoder(r.Body).Decode(&request); err != nil {
			t.Error(err)
		}
		observed <- request.Status
		io.WriteString(w, `{}`)
	}))
	defer server.Close()
	c := testClient(t, server.URL)
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	ticks := make(chan time.Time)
	results := make(chan Result, 3)
	done := make(chan error, 1)
	state := State{Status: Idle}
	go func() {
		done <- c.runHeartbeat(ctx, func() State { return state }, func(result Result) { results <- result }, ticks)
	}()
	for _, next := range []Status{Idle, BackingUp, Uploading} {
		if next != Idle {
			state = State{Status: next}
			ticks <- time.Now()
		}
		select {
		case result := <-results:
			if result.Connection != Connected || result.Err != nil {
				t.Error("heartbeat did not connect")
			}
		case <-time.After(time.Second):
			t.Fatal("heartbeat did not run immediately/on tick")
		}
		if actual := <-observed; actual != next {
			t.Errorf("activity = %s, want %s", actual, next)
		}
	}
	cancel()
	select {
	case err := <-done:
		if err != nil {
			t.Fatal(err)
		}
	case <-time.After(time.Second):
		t.Fatal("worker did not stop")
	}
	if calls.Load() != 3 {
		t.Fatal("unexpected heartbeat overlap or extra polls")
	}
}

func TestHeartbeatWorkerRecoversAndStopsOnCredentialRejection(t *testing.T) {
	var calls atomic.Int32
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch calls.Add(1) {
		case 1:
			w.WriteHeader(503)
		case 2:
			io.WriteString(w, `{}`)
		default:
			w.WriteHeader(401)
		}
	}))
	defer server.Close()
	c := testClient(t, server.URL)
	c.maxAttempts = 1
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	ticks := make(chan time.Time)
	results := make(chan Result, 3)
	done := make(chan error, 1)
	go func() {
		done <- c.runHeartbeat(ctx, func() State { return State{Status: Idle} }, func(result Result) { results <- result }, ticks)
	}()
	for i, expected := range []Connection{Unreachable, Connected, Rejected} {
		if i > 0 {
			select {
			case ticks <- time.Now():
			case <-time.After(time.Second):
				t.Fatal("worker stopped before recovery/rejection")
			}
		}
		select {
		case result := <-results:
			if result.Connection != expected {
				t.Errorf("connection = %s, want %s", result.Connection, expected)
			}
		case <-time.After(time.Second):
			t.Fatal("missing connection state")
		}
	}
	select {
	case err := <-done:
		if !errors.Is(err, ErrCredentialRejected) {
			t.Fatal("rejection did not stop worker")
		}
	case <-time.After(time.Second):
		t.Fatal("worker continued after rejection")
	}
	if calls.Load() != 3 {
		t.Fatal("rejected credential was retried")
	}
}

func TestHeartbeatProblemsCannotContainRawSecrets(t *testing.T) {
	for _, state := range []State{
		{Status: "invalid"},
		{Status: Idle, Problem: SetupIncomplete},
		{Status: Error, Problem: "private-db-password"},
	} {
		_, err := state.payload("dev")
		if err == nil || err.Error() == "private-db-password" {
			t.Fatal("unsafe state accepted or echoed")
		}
	}
	for _, problem := range []Problem{NoProblem, SetupIncomplete, DatabaseUnavailable, BackupFailed, UploadFailed} {
		payload, err := (State{Status: Error, Problem: problem}).payload("dev")
		if err != nil || payload.LastError == "" {
			t.Fatal("classified error did not generate a diagnostic message")
		}
	}
}
