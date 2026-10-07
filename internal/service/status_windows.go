package service

import (
	"context"
	"errors"
	"fmt"
	"time"

	"github.com/fivemanage/db-agent/internal/agent"
	manager "github.com/kardianos/service"
	"golang.org/x/sys/windows"
	"golang.org/x/sys/windows/svc"
	"golang.org/x/sys/windows/svc/mgr"
)

type windowsController struct{ manager.Service }

func platformController(service manager.Service) Controller { return windowsController{service} }

func openWindowsService() (*mgr.Mgr, *mgr.Service, error) {
	m, err := mgr.Connect()
	if err != nil {
		return nil, nil, err
	}
	s, err := m.OpenService(Name)
	if err != nil {
		m.Disconnect()
		if errors.Is(err, windows.ERROR_SERVICE_DOES_NOT_EXIST) {
			err = manager.ErrNotInstalled
		}
		return nil, nil, err
	}
	return m, s, nil
}

func (w windowsController) Status() (manager.Status, error) {
	m, s, err := openWindowsService()
	if err != nil {
		return manager.StatusUnknown, err
	}
	defer m.Disconnect()
	defer s.Close()
	status, err := s.Query()
	if err != nil {
		return manager.StatusUnknown, err
	}
	return windowsStatus(status.State)
}

func windowsStatus(state svc.State) (manager.Status, error) {
	switch state {
	case svc.Running, svc.StartPending:
		return manager.StatusRunning, nil
	case svc.Stopped:
		return manager.StatusStopped, nil
	case svc.StopPending:
		return manager.StatusUnknown, errors.New("service is still stopping; wait for shutdown to finish before retrying")
	default:
		return manager.StatusUnknown, errors.New("service is paused or in an unexpected state; inspect Windows Services before retrying")
	}
}

func (w windowsController) Stop() error {
	m, s, err := openWindowsService()
	if err != nil {
		return err
	}
	defer m.Disconnect()
	defer s.Close()
	status, err := s.Query()
	if err != nil || status.State == svc.Stopped {
		return err
	}
	if status.State != svc.StopPending {
		if _, err := s.Control(svc.Stop); err != nil {
			return err
		}
	}
	// The library's Windows stop wait does not exit its loop on timeout. Keep
	// CLI control bounded and never delete a service that is still stopping.
	ctx, cancel := context.WithTimeout(context.Background(), agent.ShutdownTimeout+5*time.Second)
	defer cancel()
	ticker := time.NewTicker(100 * time.Millisecond)
	defer ticker.Stop()
	for {
		status, err = s.Query()
		if err != nil || status.State == svc.Stopped {
			return err
		}
		select {
		case <-ctx.Done():
			return fmt.Errorf("service did not finish stopping; inspect Windows Services before retrying: %w", ctx.Err())
		case <-ticker.C:
		}
	}
}
