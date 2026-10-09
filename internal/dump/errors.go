package dump

import (
	"errors"
	"os"
	"regexp"
	"strconv"
	"syscall"
)

var (
	ErrConfiguration  = errors.New("dump requires a local connection, explicitly selected application databases without option-shaped or duplicate names, and nonnegative execution options")
	ErrTool           = errors.New("use a compatible logical dump client from database discovery; physical backup execution is not supported")
	ErrCredentials    = errors.New("cannot prepare private dump credentials; check temporary-directory permissions and free disk space")
	ErrStart          = errors.New("cannot start the dump client safely; check its executable, service-account permissions, and process isolation support")
	ErrOutput         = errors.New("cannot write the dump stream; check the backup destination and downstream pipeline")
	ErrDiskFull       = errors.New("dump stopped because the database, destination, or temporary filesystem is full; free disk space before retrying")
	ErrPermission     = errors.New("dump access denied; check temporary-directory, executable, and output permissions for the service account")
	ErrAuthentication = errors.New("dump authentication rejected; check the local backup account password and allowed account host")
	ErrPrivileges     = errors.New("dump privileges are insufficient; run db-agent test and ask a local administrator to verify the scoped backup grants")
	ErrConnection     = errors.New("dump could not reach the local database; check the host, port, and database service")
	ErrDatabase       = errors.New("a selected database is unavailable; run db-agent configure to verify the backup scope")
	ErrFailed         = errors.New("dump client failed; run db-agent test to check compatibility, connectivity, and grants; partial output must be discarded")
	ErrCleanup        = errors.New("cannot remove private dump credentials; ask a local administrator to inspect the temporary directory before retrying")
	ErrProcessCleanup = errors.New("cannot terminate the dump process tree; check local process isolation and service-account permissions")
)

var dumpErrorCode = regexp.MustCompile(`(?i)(?:got error:|error[: ]+)\s*([0-9]{4})(?:\D|$)`)

func explainStderr(data []byte) error {
	match := dumpErrorCode.FindSubmatch(data)
	if len(match) == 0 {
		return ErrFailed
	}
	code, _ := strconv.Atoi(string(match[1]))
	switch code {
	case 1045, 1698:
		return ErrAuthentication
	case 1044, 1142, 1143, 1227, 1370:
		return ErrPrivileges
	case 1049:
		return ErrDatabase
	case 1021:
		return ErrDiskFull
	case 2002, 2003, 2005, 2006, 2013:
		return ErrConnection
	default:
		return ErrFailed
	}
}

func fileFailure(err error) error {
	if diskFull(err) {
		return ErrDiskFull
	}
	if errors.Is(err, os.ErrPermission) {
		return ErrPermission
	}
	return ErrCredentials
}

func outputFailure(err error) error {
	if diskFull(err) {
		return ErrDiskFull
	}
	if errors.Is(err, os.ErrPermission) {
		return ErrPermission
	}
	return ErrOutput
}

func diskFull(err error) bool {
	return errors.Is(err, syscall.ENOSPC) || platformDiskFull(err)
}
