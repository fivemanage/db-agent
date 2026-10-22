package cmd

import "golang.org/x/sys/windows"

func enableSetupColor(fd uintptr) bool {
	handle := windows.Handle(fd)
	var mode uint32
	if err := windows.GetConsoleMode(handle, &mode); err != nil {
		return false
	}

	return windows.SetConsoleMode(handle, mode|windows.ENABLE_VIRTUAL_TERMINAL_PROCESSING) == nil
}
