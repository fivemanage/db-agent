package dump

import (
	"context"
	"errors"
	"io"
	"time"
)

func copyOutput(ctx context.Context, dst io.Writer, src io.Reader, rate int64) error {
	var buffer [32 << 10]byte
	defer clear(buffer[:])
	for {
		if err := ctx.Err(); err != nil {
			return contextFailure(err)
		}
		n, readErr := src.Read(buffer[:])
		if n > 0 {
			if err := waitForRate(ctx, n, rate); err != nil {
				return contextFailure(err)
			}
			written, err := dst.Write(buffer[:n])
			if err == nil && written != n {
				err = io.ErrShortWrite
			}
			if err != nil {
				return outputFailure(err)
			}
		}
		if readErr != nil {
			if errors.Is(readErr, io.EOF) {
				return nil
			}
			return ErrFailed
		}
	}
}

func waitForRate(ctx context.Context, n int, rate int64) error {
	if rate <= 0 {
		return ctx.Err()
	}
	// Sleep per bounded chunk, not by multiplying the cumulative byte count;
	// large dumps cannot overflow a duration or accumulate a large burst budget.
	delay := time.Duration(float64(n) / float64(rate) * float64(time.Second))
	if delay <= 0 {
		return ctx.Err()
	}
	timer := time.NewTimer(delay)
	defer timer.Stop()
	select {
	case <-ctx.Done():
		return ctx.Err()
	case <-timer.C:
		return nil
	}
}
