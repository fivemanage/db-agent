package service

import (
	"context"
	"errors"
	"time"
)

var ErrNotRunning = errors.New("the service started but did not keep running; check its logs with journalctl -u db-agent on Linux, or Event Viewer > Windows Logs > Application on Windows, then run db-agent start")

// WaitStarted confirms that a just-started service stays running. A start
// request alone succeeds even when the process exits at once, and a service
// manager can restart a crashing process in a loop.
func WaitStarted(ctx context.Context, svc Controller) error {
	return waitStarted(ctx, func() (bool, error) { return running(svc) }, 5*time.Second, 30*time.Second, 500*time.Millisecond)
}

func waitStarted(ctx context.Context, check func() (bool, error), stable, limit, interval time.Duration) error {
	deadline := time.Now().Add(limit)
	var since time.Time
	for {
		ok, err := check()
		if err != nil {
			return err
		}
		now := time.Now()
		if !ok {
			since = time.Time{}
		} else if since.IsZero() {
			since = now
		}
		if ok && now.Sub(since) >= stable {
			return nil
		}
		if now.After(deadline) {
			return ErrNotRunning
		}

		timer := time.NewTimer(interval)
		select {
		case <-ctx.Done():
			timer.Stop()
			return ctx.Err()
		case <-timer.C:
		}
	}
}
