package restorejob

import (
	"bufio"
	"context"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"testing"
	"time"

	"github.com/fivemanage/db-agent/internal/operation"
)

func TestJournalHelperProcess(t *testing.T) {
	phase := Phase(os.Getenv("DB_AGENT_RESTORE_JOB_HELPER"))
	if phase == "" {
		return
	}
	j, err := New(os.Getenv("DB_AGENT_RESTORE_JOB_DIR"), os.Getenv("DB_AGENT_RESTORE_OPERATION_DIR"), testJob().Binding)
	if err != nil {
		t.Fatal(err)
	}
	s := openJob(t, j, testJob())
	reach(t, s, phase)
	fmt.Fprintln(os.Stdout, "saved")
	// No Close: the parent kills this process to exercise OS lock release and
	// recovery from the actual persisted write-intent boundary.
	for {
		time.Sleep(time.Hour)
	}
}

func TestProcessCrashPreservesWriteIntentAndReleasesLock(t *testing.T) {
	for _, phase := range []Phase{Downloading, Importing, Validating, Succeeded} {
		t.Run(string(phase), func(t *testing.T) {
			j := newJournal(t)
			ctx, cancel := context.WithTimeout(context.Background(), 15*time.Second)
			defer cancel()
			child := exec.CommandContext(ctx, os.Args[0], "-test.run=^TestJournalHelperProcess$")
			child.Env = append(os.Environ(),
				"DB_AGENT_RESTORE_JOB_HELPER="+string(phase),
				"DB_AGENT_RESTORE_JOB_DIR="+j.dir,
				"DB_AGENT_RESTORE_OPERATION_DIR="+j.operationDir,
			)
			stdout, err := child.StdoutPipe()
			if err != nil {
				t.Fatal(err)
			}
			if err := child.Start(); err != nil {
				t.Fatal(err)
			}
			defer func() {
				if child.ProcessState == nil {
					child.Process.Kill()
					child.Wait()
				}
			}()
			line, err := bufio.NewReader(stdout).ReadString('\n')
			if err != nil || line != "saved\n" {
				t.Fatalf("child did not persist intent: %q, %v", line, err)
			}
			if s, err := j.Open(ctx, testJob()); s != nil || !errors.Is(err, operation.ErrBusy) {
				if s != nil {
					s.Close()
				}
				t.Fatalf("parent overlapped live child: %v", err)
			}
			if err := child.Process.Kill(); err != nil {
				t.Fatal(err)
			}
			child.Wait()

			recovered := openJob(t, j, testJob())
			state := assertSaved(t, recovered)
			switch phase {
			case Downloading:
				if state.Phase != Accepted || state.WritesStarted {
					t.Fatal("download crash authorized import")
				}
			case Importing, Validating:
				if state.Phase != Failed || state.Problem != ImportInterrupted || !state.WritesStarted {
					t.Fatal("import crash lost write intent or reported success")
				}
			case Succeeded:
				if state.Phase != Succeeded || !state.Pending() {
					t.Fatal("crash lost a saved success awaiting delivery")
				}
			}
		})
	}
}
