package dump

import (
	"os"
	"strconv"
	"strings"
	"testing"
)

func TestDumpChildRunsAtLowCPUPriority(t *testing.T) {
	helperMode(t, "sleep")
	executable, err := os.Executable()
	if err != nil {
		t.Fatal(err)
	}
	output, err := os.OpenFile(os.DevNull, os.O_WRONLY, 0)
	if err != nil {
		t.Fatal(err)
	}
	defer output.Close()
	child, err := startProcess(command{path: executable, env: os.Environ()}, output, output)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = child.kill(); child.wait() }()

	status, err := os.ReadFile("/proc/" + strconv.Itoa(child.cmd.Process.Pid) + "/stat")
	if err != nil {
		t.Fatal(err)
	}
	// The command name may contain spaces or parentheses; nice is field 19,
	// at zero-based index 16 after the final ')' that terminates that name.
	fields := strings.Fields(string(status[strings.LastIndexByte(string(status), ')')+1:]))
	if len(fields) < 17 || fields[16] != "19" {
		t.Fatalf("dump child did not receive nice=19: %s", status)
	}
}
