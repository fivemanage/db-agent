package securefile

import (
	"testing"

	"golang.org/x/sys/windows"
)

func TestWorkspaceWindowsDACLIsProtected(t *testing.T) {
	dir, err := MkdirTemp(t.TempDir(), ".restore-work-")
	if err != nil {
		t.Fatal(err)
	}
	sd, err := windows.GetNamedSecurityInfo(dir, windows.SE_FILE_OBJECT, windows.DACL_SECURITY_INFORMATION)
	if err != nil || checkDACL(sd) != nil {
		t.Fatal("workspace did not receive a protected System/Administrators DACL")
	}
}
