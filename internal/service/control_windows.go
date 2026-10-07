//go:build windows

package service

import (
	"errors"
	"fmt"

	"golang.org/x/sys/windows"
)

func requireAdministrator() error {
	token, err := windows.OpenCurrentProcessToken()
	if err != nil {
		return fmt.Errorf("check service permissions: %w", err)
	}
	defer token.Close()
	if !token.IsElevated() {
		return errors.New("service control requires Administrator permissions; open a terminal with Run as administrator")
	}
	return nil
}
