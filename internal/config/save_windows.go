//go:build windows

package config

import (
	"errors"
	"os"
	"time"

	"github.com/fivemanage/db-agent/internal/securefile"
	"golang.org/x/sys/windows"
)

func createPrivateFile(path string) (*os.File, error) {
	return securefile.OpenNew(path)
}

// replacePrivateFile retries briefly on Windows sharing/access violations caused
// by antivirus scanners or concurrent config readers.
func replacePrivateFile(from, to string) error {
	delay := 25 * time.Millisecond
	var err error
	for attempt := 0; attempt < 5; attempt++ {
		err = securefile.Replace(from, to)
		if err == nil || (!errors.Is(err, windows.ERROR_SHARING_VIOLATION) && !errors.Is(err, windows.ERROR_ACCESS_DENIED)) {
			return err
		}
		time.Sleep(delay)
		delay *= 2
	}
	return err
}
