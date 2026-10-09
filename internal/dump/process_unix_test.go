//go:build !windows

package dump

import (
	"os"
	"runtime"
	"strconv"
	"strings"

	"golang.org/x/sys/unix"
)

func processAlive(pid int) bool {
	if unix.Kill(pid, 0) != nil {
		return false
	}
	if runtime.GOOS == "linux" {
		data, err := os.ReadFile("/proc/" + strconv.Itoa(pid) + "/stat")
		if err != nil {
			return false
		}
		// An orphan can briefly be a zombie until its OS reaper collects it.
		_, state, _ := strings.Cut(string(data), ") ")
		return !strings.HasPrefix(state, "Z")
	}
	return true
}
