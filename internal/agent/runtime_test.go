package agent

import (
	"context"
	"errors"
	"reflect"
	"sync"
	"sync/atomic"
	"testing"
	"time"
)

func TestRuntimeStartStopAreIdempotentAndJoinCleanup(t *testing.T) {
	var starts atomic.Int32
	var order []string
	started := make(chan struct{})
	r := New(func(ctx context.Context) error {
		starts.Add(1)
		close(started)
		<-ctx.Done()
		order = append(order, "worker")
		return ctx.Err()
	}, func(ctx context.Context) error {
		if ctx.Err() != nil {
			t.Error("cleanup inherited the canceled root context")
		}
		order = append(order, "first hook")
		return nil
	}, func(context.Context) error {
		order = append(order, "second hook")
		return nil
	})
	ctx, cancel := context.WithTimeout(context.Background(), time.Second)
	defer cancel()

	if err := r.Stop(ctx); err != nil {
		t.Fatal(err)
	}
	if err := r.Start(context.Background()); err != nil {
		t.Fatal(err)
	}
	if err := r.Start(context.Background()); err != nil {
		t.Fatal(err)
	}
	select {
	case <-started:
	case <-ctx.Done():
		t.Fatal("worker did not start")
	}

	var callers sync.WaitGroup
	for range 4 {
		callers.Add(1)
		go func() {
			defer callers.Done()
			if err := r.Stop(ctx); err != nil {
				t.Error(err)
			}
		}()
	}
	callers.Wait()

	if starts.Load() != 1 || !reflect.DeepEqual(order, []string{"worker", "second hook", "first hook"}) {
		t.Fatalf("starts=%d, cleanup order=%v", starts.Load(), order)
	}
	if err := r.Start(context.Background()); !errors.Is(err, ErrStopped) {
		t.Fatalf("completed runtime restarted: %v", err)
	}
}

func TestRuntimePreservesWorkerAndHookErrors(t *testing.T) {
	workerErr := errors.New("worker failed")
	hookErr := errors.New("cleanup failed")
	ctx, cancel := context.WithTimeout(context.Background(), time.Second)
	defer cancel()

	err := Run(ctx, func(context.Context) error { return workerErr }, func(context.Context) error { return hookErr })
	if !errors.Is(err, workerErr) || !errors.Is(err, hookErr) {
		t.Fatalf("failure lost: %v", err)
	}
}

func TestRuntimeStopHasCallerDeadline(t *testing.T) {
	release := make(chan struct{})
	r := New(func(context.Context) error { <-release; return nil })
	if err := r.Start(context.Background()); err != nil {
		t.Fatal(err)
	}

	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Millisecond)
	defer cancel()

	err := r.Stop(ctx)
	close(release)

	if !errors.Is(err, context.DeadlineExceeded) {
		t.Fatalf("uncooperative worker blocked stop: %v", err)
	}

	joined, stop := context.WithTimeout(context.Background(), time.Second)
	defer stop()
	if err := r.Stop(joined); err != nil {
		t.Fatal(err)
	}
}

func TestRuntimeParentCancellation(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	started := make(chan struct{})
	done := make(chan error, 1)
	go func() {
		done <- Run(ctx, func(ctx context.Context) error {
			close(started)
			<-ctx.Done()
			return ctx.Err()
		})
	}()

	select {
	case <-started:
	case <-time.After(time.Second):
		t.Fatal("runtime did not start")
	}

	cancel()

	select {
	case err := <-done:
		if err != nil {
			t.Fatal(err)
		}
	case <-time.After(time.Second):
		t.Fatal("root cancellation did not stop the runtime")
	}
}

func TestCancellationDoesNotHideWorkerFailure(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	failure := errors.New("worker cleanup failed")

	err := Run(ctx, func(ctx context.Context) error {
		cancel()
		return errors.Join(ctx.Err(), failure)
	})

	if !errors.Is(err, failure) {
		t.Fatalf("cancellation hid a real failure: %v", err)
	}
}
