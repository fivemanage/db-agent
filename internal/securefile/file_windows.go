package securefile

import (
	"os"
	"unsafe"

	"golang.org/x/sys/windows"
)

const PrivateDACL = "D:P(A;;FA;;;SY)(A;;FA;;;BA)"

func OpenNew(path string) (*os.File, error) {
	sd, err := windows.SecurityDescriptorFromString(PrivateDACL)
	if err != nil {
		return nil, err
	}
	sa := &windows.SecurityAttributes{Length: uint32(unsafe.Sizeof(windows.SecurityAttributes{})), SecurityDescriptor: sd}
	name, err := windows.UTF16PtrFromString(path)
	if err != nil {
		return nil, err
	}
	handle, err := windows.CreateFile(name, windows.GENERIC_WRITE, 0, sa, windows.CREATE_NEW, windows.FILE_ATTRIBUTE_NORMAL, 0)
	if err != nil {
		return nil, err
	}
	return os.NewFile(uintptr(handle), path), nil
}

func openRead(path string) (*os.File, error) {
	name, err := windows.UTF16PtrFromString(path)
	if err != nil {
		return nil, err
	}
	// State/config readers must not prevent an atomic replacement by a worker.
	handle, err := windows.CreateFile(name, windows.GENERIC_READ|windows.READ_CONTROL, windows.FILE_SHARE_READ|windows.FILE_SHARE_DELETE, nil, windows.OPEN_EXISTING, windows.FILE_FLAG_OPEN_REPARSE_POINT, 0)
	if err != nil {
		return nil, err
	}
	return os.NewFile(uintptr(handle), path), nil
}

func checkPermissions(file *os.File) error {
	var info windows.ByHandleFileInformation
	if err := windows.GetFileInformationByHandle(windows.Handle(file.Fd()), &info); err != nil {
		return err
	}
	if info.FileAttributes&windows.FILE_ATTRIBUTE_REPARSE_POINT != 0 {
		return ErrUnsafe
	}
	sd, err := windows.GetSecurityInfo(windows.Handle(file.Fd()), windows.SE_FILE_OBJECT, windows.DACL_SECURITY_INFORMATION)
	if err != nil {
		return err
	}
	return checkDACL(sd)
}

func checkDACL(sd *windows.SECURITY_DESCRIPTOR) error {
	control, _, err := sd.Control()
	if err != nil || control&windows.SE_DACL_PROTECTED == 0 {
		return ErrUnsafe
	}
	dacl, _, err := sd.DACL()
	if err != nil || dacl == nil || dacl.AceCount == 0 {
		return ErrUnsafe
	}
	system, err := windows.CreateWellKnownSid(windows.WinLocalSystemSid)
	if err != nil {
		return err
	}
	admins, err := windows.CreateWellKnownSid(windows.WinBuiltinAdministratorsSid)
	if err != nil {
		return err
	}
	for i := uint32(0); i < uint32(dacl.AceCount); i++ {
		var ace *windows.ACCESS_ALLOWED_ACE
		if err := windows.GetAce(dacl, i, &ace); err != nil {
			return err
		}
		if ace.Header.AceType != windows.ACCESS_ALLOWED_ACE_TYPE || ace.Header.AceFlags != 0 || ace.Header.AceSize < uint16(unsafe.Sizeof(*ace)) {
			return ErrUnsafe
		}
		// GetAce supplies an OS-validated ACE; SidStart is the beginning of its
		// variable-length SID, not a separate pointer.
		sid := (*windows.SID)(unsafe.Pointer(&ace.SidStart))
		if !sid.Equals(system) && !sid.Equals(admins) {
			return ErrUnsafe
		}
	}
	return nil
}

func move(from, to string, flags uint32) error {
	source, err := windows.UTF16PtrFromString(from)
	if err != nil {
		return err
	}
	target, err := windows.UTF16PtrFromString(to)
	if err != nil {
		return err
	}
	return windows.MoveFileEx(source, target, flags|windows.MOVEFILE_WRITE_THROUGH)
}

func publish(from, to string) error { return move(from, to, 0) }
func Replace(from, to string) error { return move(from, to, windows.MOVEFILE_REPLACE_EXISTING) }

func ReplaceDurable(from, to string) error {
	return Replace(from, to)
}
