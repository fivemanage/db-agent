package agent

import (
	"context"
	"errors"
	"sync"
	"time"
)

const ShutdownTimeout = 15 * time.Second

var ErrStopped = errors.New("agent runtime has already stopped")

type Hook func(context.Context) error

// Runtime owns one run and its cleanup. Work must join its child workers before
// returning; hooks release resources afterwards, in reverse registration order.
type Runtime struct {
	mu     sync.Mutex
	run    func(context.Context) error
	hooks  []Hook
	cancel context.CancelFunc
	done   chan struct{}
	err    error
}

func New(run func(context.Context) error, hooks ...Hook) *Runtime {
	return &Runtime{run: run, hooks: append([]Hook(nil), hooks...), done: make(chan struct{})}
}

func Run(ctx context.Context, work func(context.Context) error, hooks ...Hook) error {
	r := New(work, hooks...)
	if err := r.Start(ctx); err != nil {
		return err
	}

	select {
	case <-ctx.Done():
	case <-r.Done():
	}

	shutdown, cancel := context.WithTimeout(context.Background(), ShutdownTimeout)
	defer cancel()

	return r.Stop(shutdown)
}

func (r *Runtime) Start(parent context.Context) error {
	r.mu.Lock()
	defer r.mu.Unlock()

	if r.cancel != nil {
		select {
		case <-r.done:
			return ErrStopped
		default:
			return nil
		}
	}

	if r.run == nil {
		return errors.New("agent runtime has no work configured")
	}

	ctx, cancel := context.WithCancel(parent)
	r.cancel = cancel

	go func() {
		err := r.run(ctx)
		if err != nil && ctx.Err() != nil && cancellationOnly(err, ctx.Err()) {
			err = nil
		}

		cancel()

		// Cleanup needs a fresh budget: the work context is already canceled.
		cleanup, stop := context.WithTimeout(context.Background(), ShutdownTimeout)
		defer stop()

		for i := len(r.hooks) - 1; i >= 0; i-- {
			err = errors.Join(err, r.hooks[i](cleanup))
		}

		r.mu.Lock()
		r.err = err
		close(r.done)
		r.mu.Unlock()
	}()

	return nil
}

func cancellationOnly(err, canceled error) bool {
	if joined, ok := err.(interface{ Unwrap() []error }); ok {
		for _, child := range joined.Unwrap() {
			if !cancellationOnly(child, canceled) {
				return false
			}
		}

		return true
	}

	if wrapped, ok := err.(interface{ Unwrap() error }); ok {
		return cancellationOnly(wrapped.Unwrap(), canceled)
	}

	return errors.Is(err, canceled)
}

func (r *Runtime) Done() <-chan struct{} { return r.done }

func (r *Runtime) Wait(ctx context.Context) error {
	select {
	case <-r.done:
		r.mu.Lock()
		defer r.mu.Unlock()

		return r.err
	case <-ctx.Done():
		return ctx.Err()
	}
}

func (r *Runtime) Stop(ctx context.Context) error {
	r.mu.Lock()
	cancel := r.cancel
	r.mu.Unlock()

	if cancel == nil {
		return nil
	}

	cancel()

	return r.Wait(ctx)
}
