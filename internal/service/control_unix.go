//go:build !windows

package service

import (
	"errors"
	"os"
)

func requireAdministrator() error {
	if os.Geteuid() != 0 {
		return errors.New("service control requires root permissions; rerun with sudo")
	}
	return nil
}
