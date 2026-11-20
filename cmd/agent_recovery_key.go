package cmd

import (
	"context"
	"errors"
	"fmt"
	"io"
	"net/http"
	"time"

	"github.com/fivemanage/db-agent/internal/controlplane"
	"github.com/fivemanage/db-agent/internal/key"
	"github.com/fivemanage/db-agent/internal/setup"
)

const recoveryKeySyncInterval = 30 * time.Second

type recoveryKeyPlane interface {
	RecoveryKeyStatus(context.Context) (controlplane.RecoveryKeyStatus, error)
	UploadRecoveryKey(context.Context, key.Key) error
}

func syncRecoveryKey(ctx context.Context, path string, plane recoveryKeyPlane) (bool, error) {
	status, err := plane.RecoveryKeyStatus(ctx)
	if err != nil {
		return false, err
	}
	if status != controlplane.RecoveryKeyPending {
		return status == controlplane.RecoveryKeySaved, nil
	}

	recoveryKey, ready, err := setup.BoundRecoveryKey(path)
	if err != nil || !ready {
		return false, err
	}
	if err := plane.UploadRecoveryKey(ctx, recoveryKey); err != nil {
		return false, err
	}
	return true, nil
}

// The worker never changes backup readiness. Existing installations wait for
// explicit opt-in; a dashboard without this protocol needs no further polls.
func runRecoveryKeySync(ctx context.Context, path string, plane recoveryKeyPlane, output io.Writer) {
	ticker := time.NewTicker(recoveryKeySyncInterval)
	defer ticker.Stop()
	var lastError string
	for ctx.Err() == nil {
		saved, err := syncRecoveryKey(ctx, path, plane)
		if saved || ctx.Err() != nil || errors.Is(err, controlplane.ErrCredentialRejected) {
			return
		}
		var response *controlplane.ResponseError
		if errors.As(err, &response) && (response.StatusCode == http.StatusNotFound ||
			response.StatusCode == http.StatusMethodNotAllowed ||
			response.StatusCode >= 300 && response.StatusCode < 400) {
			return
		}
		if message := errorText(err); message != lastError {
			if err != nil {
				fmt.Fprintf(output, "Managed recovery copy is not ready: %v\n", err)
			}
			lastError = message
		}
		select {
		case <-ctx.Done():
			return
		case <-ticker.C:
		}
	}
}
