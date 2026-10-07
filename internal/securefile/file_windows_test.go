package securefile

import (
	"errors"
	"testing"

	"golang.org/x/sys/windows"
)

func TestWindowsDACLPermitsOnlyProtectedSystemAndAdministrators(t *testing.T) {
	for _, test := range []struct {
		sddl string
		ok   bool
	}{
		{PrivateDACL, true},
		{"D:(A;;FA;;;SY)(A;;FA;;;BA)", false},
		{"D:P(A;;FA;;;WD)", false},
		{"D:P(A;;FA;;;SY)(A;;FR;;;BU)", false},
		{"D:P", false},
	} {
		sd, err := windows.SecurityDescriptorFromString(test.sddl)
		if err != nil {
			t.Fatal(err)
		}
		err = checkDACL(sd)
		if (err == nil) != test.ok || err != nil && !errors.Is(err, ErrUnsafe) {
			t.Fatalf("DACL safety classification failed: %v", err)
		}
	}
}
