package cmd

import (
	"context"
	"errors"
	"fmt"
	"io"
	"net/http"
	"time"

	"github.com/fivemanage/db-agent/internal/controlplane"
	"github.com/fivemanage/db-agent/internal/setup"
)

const setupPollInterval = 5 * time.Second

type setupPlane interface {
	PollSetup(context.Context, controlplane.SetupStatus) (controlplane.SetupPollResponse, error)
	ReportSetup(context.Context, string, controlplane.SetupReport) error
}

type pendingSetupReport struct {
	id     string
	report controlplane.SetupReport
}

// runDashboardSetup polls while local setup is incomplete. It reports setup
// progress, runs dashboard finish requests, and calls finished once when setup
// becomes complete during this run, so readiness never stays at the startup check.
func runDashboardSetup(ctx context.Context, path string, plane setupPlane, interval time.Duration, finished func(context.Context), errorOutput io.Writer) error {
	ticker := time.NewTicker(interval)
	defer ticker.Stop()

	var pending *pendingSetupReport
	var lastStatusError string
	incomplete := false
	for {
		if ctx.Err() != nil {
			return nil
		}

		if pending != nil {
			err := plane.ReportSetup(ctx, pending.id, pending.report)
			if err == nil || !transientSetupError(err) {
				pending = nil
			}
			if err != nil && !transientSetupError(err) {
				if errors.Is(err, controlplane.ErrCredentialRejected) {
					return err
				}
				fmt.Fprintf(errorOutput, "Dashboard setup result was not accepted: %v\n", err)
			}
		}

		status, err := setup.Status(path)
		if message := errorText(err); message != lastStatusError {
			if err != nil {
				fmt.Fprintf(errorOutput, "Local setup status is incomplete: %v\n", err)
			}
			lastStatusError = message
		}
		if !status.Complete {
			incomplete = true
		}

		response, err := plane.PollSetup(ctx, status)
		switch {
		case ctx.Err() != nil:
			return nil
		case err != nil:
			var httpErr *controlplane.ResponseError
			if errors.As(err, &httpErr) && (httpErr.StatusCode == http.StatusNotFound || httpErr.StatusCode >= 300 && httpErr.StatusCode < 400) {
				return nil
			}
			if !transientSetupError(err) {
				return err
			}
		case status.Complete:
			if pending == nil {
				if incomplete {
					finished(ctx)
				}
				return nil
			}
		case response.Request != nil && pending == nil:
			report := setup.Finish(ctx, path)
			if ctx.Err() != nil {
				return nil
			}
			pending = &pendingSetupReport{id: response.Request.ID, report: report}
			continue
		}

		select {
		case <-ctx.Done():
			return nil
		case <-ticker.C:
		}
	}
}

func errorText(err error) string {
	if err == nil {
		return ""
	}
	return err.Error()
}

func transientSetupError(err error) bool {
	var response *controlplane.ResponseError
	var transport *controlplane.TransportError
	return errors.As(err, &transport) || errors.As(err, &response) && (response.StatusCode == 429 || response.StatusCode >= 500)
}
