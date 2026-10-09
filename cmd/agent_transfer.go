package cmd

import (
	"context"
	"errors"
	"fmt"
	"io"
	"sync"

	"github.com/fivemanage/db-agent/internal/controlplane"
	"github.com/fivemanage/db-agent/internal/transfer"
)

// Transfers can take longer than a heartbeat interval. Keep their bounded
// sessions in a joined worker so replay never blocks the next liveness poll.
func runHeartbeatTransfers(
	parent context.Context,
	heartbeats func(context.Context, func(controlplane.Result)) error,
	resume func(context.Context) error,
	report func(controlplane.Result),
	output io.Writer,
) error {
	ctx, cancel := context.WithCancel(parent)
	sessions := make(chan struct{}, 1)
	done := make(chan error, 2)
	var workers sync.WaitGroup
	defer func() {
		cancel()
		workers.Wait()
	}()

	workers.Add(2)
	go func() {
		defer workers.Done()

		done <- heartbeats(ctx, func(result controlplane.Result) {
			if report != nil {
				report(result)
			}

			if result.Connection == controlplane.Connected {
				select {
				case sessions <- struct{}{}:
				default:
				}
			}
		})
	}()

	go func() {
		defer workers.Done()

		for {
			select {
			case <-ctx.Done():
				done <- nil
				return
			case <-sessions:
				if ctx.Err() != nil {
					done <- nil
					return
				}

				err := resume(ctx)
				if ctx.Err() != nil {
					done <- nil
					return
				}
				if errors.Is(err, controlplane.ErrCredentialRejected) || errors.Is(err, transfer.ErrLocal) {
					done <- err
					return
				}

				if err != nil && !errors.Is(err, transfer.ErrBusy) {
					fmt.Fprintln(output, "Backup transfer remains queued or failed; check local backup status.")
				}
			}
		}
	}()

	select {
	case <-parent.Done():
		return nil
	case err := <-done:
		return err
	}
}
