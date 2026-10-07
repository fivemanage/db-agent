package service

import (
	"testing"

	manager "github.com/kardianos/service"
	"golang.org/x/sys/windows/svc"
)

func TestWindowsStoppingIsNotReportedAsStopped(t *testing.T) {
	for _, test := range []struct {
		state  svc.State
		status manager.Status
		err    bool
	}{
		{svc.Running, manager.StatusRunning, false},
		{svc.StartPending, manager.StatusRunning, false},
		{svc.Stopped, manager.StatusStopped, false},
		{svc.StopPending, manager.StatusUnknown, true},
		{svc.Paused, manager.StatusUnknown, true},
	} {
		status, err := windowsStatus(test.state)
		if status != test.status || (err != nil) != test.err {
			t.Fatalf("state=%v: status=%v err=%v", test.state, status, err)
		}
	}
}
