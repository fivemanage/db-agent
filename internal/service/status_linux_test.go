package service

import (
	"errors"
	"testing"

	manager "github.com/kardianos/service"
)

func TestSystemdStatesDoNotConfuseFailedWithMissing(t *testing.T) {
	for _, test := range []struct {
		output string
		status manager.Status
		err    bool
	}{
		{"LoadState=loaded\nActiveState=active\n", manager.StatusRunning, false},
		{"LoadState=loaded\nActiveState=activating\n", manager.StatusRunning, false},
		{"LoadState=loaded\nActiveState=failed\n", manager.StatusStopped, false},
		{"LoadState=loaded\nActiveState=inactive\n", manager.StatusStopped, false},
		{"LoadState=not-found\nActiveState=inactive\n", manager.StatusUnknown, true},
		{"LoadState=masked\nActiveState=inactive\n", manager.StatusUnknown, true},
		{"unexpected output", manager.StatusUnknown, true},
	} {
		status, err := systemdStatus(test.output)
		if status != test.status || (err != nil) != test.err {
			t.Fatalf("%q: status=%v err=%v", test.output, status, err)
		}
		if errors.Is(err, manager.ErrNotInstalled) != (test.output == "LoadState=not-found\nActiveState=inactive\n") {
			t.Fatalf("misclassified missing service: %q: %v", test.output, err)
		}
	}
}
