//go:build windows

package update

import (
	"fmt"
	"syscall"
	"unsafe"
)

var replaceFileW = syscall.NewLazyDLL("kernel32.dll").NewProc("ReplaceFileW")

func replaceWithBackup(target, staged, backup string) error {
	return replaceWindowsFile(target, staged, backup)
}

func restoreBackup(target, backup string) error {
	return replaceWindowsFile(target, backup, "")
}

func replaceWindowsFile(target, replacement, backup string) error {
	oldPtr, err := syscall.UTF16PtrFromString(target)
	if err != nil {
		return err
	}
	newPtr, err := syscall.UTF16PtrFromString(replacement)
	if err != nil {
		return err
	}
	var backupPtr *uint16
	if backup != "" {
		backupPtr, err = syscall.UTF16PtrFromString(backup)
		if err != nil {
			return err
		}
	}
	result, _, callErr := replaceFileW.Call(uintptr(unsafe.Pointer(oldPtr)), uintptr(unsafe.Pointer(newPtr)), uintptr(unsafe.Pointer(backupPtr)), 0, 0, 0)
	if result == 0 {
		return fmt.Errorf("ReplaceFileW: %w", callErr)
	}
	return nil
}
